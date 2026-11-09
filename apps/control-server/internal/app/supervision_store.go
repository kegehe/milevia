package app

// 托管领域状态与普通 Task/Run 分离，通过独立任务托管路由提供服务。
// 所有 launch/control 写入共用 withWorkspaceAdmissionTx，锁内不运行 CLI 或等待进程。
import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	errSupervisionState  = errors.New("托管状态已变化，请刷新后重试")
	errSupervisionLease  = errors.New("托管调度代际已失效")
	errSupervisionBudget = errors.New("托管可用预算不足")
)

type supervisionBudget struct {
	MaxStorageBytes     int64 `json:"maxStorageBytes,omitempty"`
	MaxActiveMillis     int64 `json:"maxActiveMillis"`
	MaxAgentTurns       int   `json:"maxAgentTurns"`
	MaxTurnMillis       int64 `json:"maxTurnMillis"`
	MaxFixRounds        int   `json:"maxFixRounds"`
	MaxTransientRetries int   `json:"maxTransientRetries"`
	FinalReservePercent int   `json:"finalReservePercent"`
}

func defaultSupervisionBudget() supervisionBudget {
	return supervisionBudget{MaxStorageBytes: supervisionStorageLimit, MaxActiveMillis: int64(4 * time.Hour / time.Millisecond), MaxAgentTurns: 200,
		MaxTurnMillis: int64(30 * time.Minute / time.Millisecond), MaxFixRounds: 3, MaxTransientRetries: 2, FinalReservePercent: 20}
}

func (b supervisionBudget) validate() error {
	if b.MaxStorageBytes != 0 && (b.MaxStorageBytes < 16<<20 || b.MaxStorageBytes > 64<<30) {
		return errors.New("托管存储预算应为16MiB至64GiB")
	}
	if b.MaxActiveMillis < 1 || b.MaxActiveMillis > int64(365*24*time.Hour/time.Millisecond) || b.MaxAgentTurns < 1 || b.MaxAgentTurns > 100000 ||
		b.MaxTurnMillis < 1 || b.MaxTurnMillis > int64(30*time.Minute/time.Millisecond) || b.MaxFixRounds < 0 || b.MaxFixRounds > 100 ||
		b.MaxTransientRetries < 0 || b.MaxTransientRetries > 100 || b.FinalReservePercent < 1 || b.FinalReservePercent > 90 {
		return errors.New("托管预算超出有效范围")
	}
	return nil
}

type supervisionSession struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectId"`
	State     string `json:"state"`
	// 生命周期暂停、等待输入及重启核对不能丢失实施/最终验收阶段。
	RunPhase         string                 `json:"-"`
	StateVersion     int64                  `json:"stateVersion"`
	EventSeq         int64                  `json:"eventSeq"`
	SchedulerAgentID string                 `json:"schedulerAgentId"`
	Roles            supervisionRoleConfigs `json:"roles"`
	RouteSnapshot    json.RawMessage        `json:"routeSnapshot"`
	AdmissionID      string                 `json:"admissionId,omitempty"`
	AdmissionEpoch   int64                  `json:"admissionEpoch"`
	RecoveryState    string                 `json:"recoveryState"`
	Reason           string                 `json:"reason,omitempty"`
	Control          string                 `json:"control,omitempty"`
	Budget           supervisionBudget      `json:"budget"`
	ActiveMillis     int64                  `json:"activeMillis"`
	UsedTurns        int                    `json:"usedTurns"`
	ReservedTurns    int                    `json:"reservedTurns"`
	ClockAt          *time.Time             `json:"-"`
	LeaseOwner       string                 `json:"-"`
	LeaseEpoch       int64                  `json:"leaseEpoch"`
	LeaseExpiresAt   *time.Time             `json:"-"`
	CreatedAt        time.Time              `json:"createdAt"`
	UpdatedAt        time.Time              `json:"updatedAt"`
	ArchivedAt       *time.Time             `json:"archivedAt,omitempty"`
}

type supervisionTask struct {
	TaskID                string          `json:"taskId"`
	Position              int             `json:"position"`
	Phase                 string          `json:"phase"`
	RequirementsRevision  int64           `json:"requirementsRevision"`
	TaskSnapshot          json.RawMessage `json:"taskSnapshot"`
	DependencySnapshot    json.RawMessage `json:"dependencySnapshot"`
	PlanRevision          int64           `json:"planRevision"`
	EvidenceFreshness     string          `json:"evidenceFreshness"`
	ImplementationStarted bool            `json:"implementationStarted"`
	FixRounds             int             `json:"fixRounds"`
}

type supervisionEvent struct {
	SessionID string          `json:"sessionId"`
	Seq       int64           `json:"seq"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"createdAt"`
}

func (s *Server) migrateSupervision(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `create table if not exists supervision_sessions (
		id text primary key,
		project_id text not null references projects(id) on delete cascade,
		state text not null check(state in ('draft','running','pausing','paused','needs_input','finalizing','stopping','stopped','completed','completed_with_issues','failed')),
		state_version integer not null check(state_version>0), event_seq integer not null default 0,
		scheduler_agent_id text not null, route_snapshot text not null default '[]',
		admission_id text not null default '', admission_epoch integer not null default 0,
		recovery_state text not null default 'ready', reason text not null default '', control text not null default '' check(control in ('','pause','stop')),
		budget text not null, active_millis integer not null default 0 check(active_millis>=0),
		used_turns integer not null default 0 check(used_turns>=0), reserved_turns integer not null default 0 check(reserved_turns>=0), clock_at datetime,
		lease_owner text not null default '', lease_epoch integer not null default 0, lease_expires_at datetime,
		created_at datetime not null, updated_at datetime not null
	);
	create unique index if not exists supervision_sessions_active_project on supervision_sessions(project_id)
		where state not in ('draft','stopped','completed','completed_with_issues','failed');
	create table if not exists supervision_tasks (
		session_id text not null references supervision_sessions(id) on delete cascade,
		task_id text not null,
		position integer not null, phase text not null default 'pending', requirements_revision integer not null default 1,
		task_snapshot text not null default '{}', dependency_snapshot text not null default '[]',
		plan_revision integer not null default 0, evidence_freshness text not null default 'missing',
		implementation_started integer not null default 0, fix_rounds integer not null default 0,
		primary key(session_id,task_id), unique(session_id,position)
	);
	create table if not exists supervision_events (
		session_id text not null references supervision_sessions(id) on delete cascade,
		seq integer not null, type text not null, payload text not null, created_at datetime not null,
		primary key(session_id,seq)
	);
	create table if not exists supervision_decisions (
		id text primary key, session_id text not null references supervision_sessions(id) on delete cascade,
		expected_state_version integer not null, basis_seq integer not null, lease_epoch integer not null,
		state text not null check(state in ('pending','running','proposed','applied','stale','invalid','cancelled')),
		output text not null default '', validation_error text not null default '',
		created_at datetime not null, completed_at datetime,
		unique(session_id,id)
	);
	create unique index if not exists supervision_decisions_active on supervision_decisions(session_id) where state in ('pending','running','proposed');
	create table if not exists supervision_work_items (
		id text primary key, session_id text not null references supervision_sessions(id) on delete cascade,
		decision_id text not null references supervision_decisions(id) on delete cascade,
		task_id text not null default '', role text not null check(role in ('scheduler','implementation','test','review')),
		attempt integer not null default 1 check(attempt>0), execution_id text not null unique,
		state text not null check(state in ('queued','suspended','launching','running','quiescent','cancelled','unknown')),
		lease_epoch integer not null, admission_epoch integer not null, budget_class text not null check(budget_class in ('work','final')),
		turn_state text not null check(turn_state in ('reserved','consumed','released')),
		result_text text not null default '', result_state text not null default '', result_error text not null default '', result_received_at datetime,
		created_at datetime not null, started_at datetime, finished_at datetime,
		unique(decision_id,attempt)
	);
	create index if not exists supervision_work_items_session on supervision_work_items(session_id,state);
	create table if not exists supervision_outbox (
		execution_id text primary key references supervision_work_items(execution_id) on delete cascade,
		session_id text not null references supervision_sessions(id) on delete cascade,
		state text not null check(state in ('pending','suspended','launching','delivered','completed','cancelled','unknown')),
		lease_epoch integer not null, created_at datetime not null, updated_at datetime not null
	);
	create table if not exists supervision_scheduler_inputs (
		execution_id text primary key references supervision_work_items(execution_id) on delete cascade,
		input_json text not null, input_sha256 text not null, created_at datetime not null
	);
	create table if not exists supervision_executions (
		execution_id text primary key references supervision_work_items(execution_id) on delete cascade,
		session_id text not null references supervision_sessions(id) on delete cascade,
		run_id text not null unique references runs(id) on delete cascade,
		token_hash text not null, container text not null unique,
		pid integer not null default 0, birth text not null default '',
		executable text not null default '', directory text not null default '',
		state text not null check(state in ('prepared','starting','created','running','quiescent','unknown')),
		receipt text not null default '{}', error text not null default '',
		created_at datetime not null, updated_at datetime not null
	);`)
	if err != nil {
		return err
	}
	if err := ensureColumn(ctx, s.db, "supervision_sessions", "role_config", "text not null default '{}'"); err != nil {
		return err
	}
	if err := ensureColumn(ctx, s.db, "supervision_sessions", "archived_at", "datetime"); err != nil {
		return err
	}
	if err := ensureColumn(ctx, s.db, "supervision_sessions", "run_phase", "text not null default '' check(run_phase in ('','running','finalizing'))"); err != nil {
		return err
	}
	// 旧版没有保存暂停前阶段。活动状态可直接恢复；其余有过最终验收的
	// 记录保守保留该限制，不能因升级重新授予实施权限。只回填未初始化行。
	if _, err := s.db.ExecContext(ctx, `update supervision_sessions set run_phase=case
		when state in ('running','finalizing') then state
		when exists(select 1 from supervision_events e where e.session_id=supervision_sessions.id and e.type='session.finalizing') then 'finalizing'
		else 'running' end where run_phase=''`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `create table if not exists supervision_storage_reservations(execution_id text primary key references supervision_work_items(execution_id) on delete cascade,session_id text not null references supervision_sessions(id) on delete cascade,bytes integer not null check(bytes>0))`); err != nil {
		return err
	}
	if err := s.migrateSupervisionRequests(ctx); err != nil {
		return err
	}
	// 旧执行保持原记录及恢复路径，不补造新版规格或重新授权。
	if _, err := s.db.ExecContext(ctx, `create table if not exists supervision_execution_specs (
		execution_id text primary key references supervision_executions(execution_id) on delete cascade,
		spec text not null, created_at datetime not null
	)`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `create table if not exists supervision_target_bindings (
		runner_id text primary key, target_id text not null, protocol integer not null,
		helper_sha256 text not null, target_os text not null, target_arch text not null,
		verified_at datetime not null
	)`); err != nil {
		return err
	}
	if err := s.migrateSupervisionTargetEvents(ctx); err != nil {
		return err
	}
	if err := s.migrateSupervisionTargetRoutes(ctx); err != nil {
		return err
	}
	if err := s.migrateSupervisionTargetControls(ctx); err != nil {
		return err
	}
	return s.migrateSupervisionContext(ctx)
}

const supervisionSessionColumns = `id,project_id,state,state_version,event_seq,scheduler_agent_id,route_snapshot,admission_id,admission_epoch,recovery_state,reason,control,budget,active_millis,used_turns,reserved_turns,clock_at,lease_owner,lease_epoch,lease_expires_at,created_at,updated_at,role_config,archived_at,run_phase`

func loadSupervisionSession(ctx context.Context, q sqlQueryer, id string) (supervisionSession, error) {
	var ss supervisionSession
	var route, budget, roles string
	var clock, lease, archived sql.NullTime
	err := q.QueryRowContext(ctx, `select `+supervisionSessionColumns+` from supervision_sessions where id=?`, id).Scan(
		&ss.ID, &ss.ProjectID, &ss.State, &ss.StateVersion, &ss.EventSeq, &ss.SchedulerAgentID, &route,
		&ss.AdmissionID, &ss.AdmissionEpoch, &ss.RecoveryState, &ss.Reason, &ss.Control, &budget, &ss.ActiveMillis,
		&ss.UsedTurns, &ss.ReservedTurns, &clock, &ss.LeaseOwner, &ss.LeaseEpoch, &lease, &ss.CreatedAt, &ss.UpdatedAt, &roles, &archived, &ss.RunPhase)
	if err != nil {
		return ss, err
	}
	if ss.State == "running" || ss.State == "finalizing" {
		ss.RunPhase = ss.State
	}
	ss.RouteSnapshot = json.RawMessage(route)
	if err := json.Unmarshal([]byte(roles), &ss.Roles); err != nil {
		return ss, err
	}
	if err := json.Unmarshal([]byte(budget), &ss.Budget); err != nil {
		return ss, err
	}
	if err := ss.Budget.validate(); err != nil {
		return ss, err
	}
	if clock.Valid {
		ss.ClockAt = &clock.Time
	}
	if lease.Valid {
		ss.LeaseExpiresAt = &lease.Time
	}
	if archived.Valid {
		ss.ArchivedAt = &archived.Time
	}
	return ss, nil
}

func saveSupervisionSessionTx(ctx context.Context, tx *sql.Tx, ss *supervisionSession) error {
	if err := syncSupervisionTargetControlTx(ctx, tx, ss); err != nil {
		return err
	}
	if ss.State == "running" || ss.State == "finalizing" {
		ss.RunPhase = ss.State
	} else if ss.RunPhase == "" {
		ss.RunPhase = "running"
	}
	budget, err := json.Marshal(ss.Budget)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `update supervision_sessions set state=?,state_version=?,event_seq=?,route_snapshot=?,admission_id=?,admission_epoch=?,recovery_state=?,reason=?,control=?,budget=?,active_millis=?,used_turns=?,reserved_turns=?,clock_at=?,lease_owner=?,lease_epoch=?,lease_expires_at=?,updated_at=?,scheduler_agent_id=?,role_config=?,archived_at=?,run_phase=? where id=?`,
		ss.State, ss.StateVersion, ss.EventSeq, string(ss.RouteSnapshot), ss.AdmissionID, ss.AdmissionEpoch, ss.RecoveryState, ss.Reason, ss.Control, string(budget), ss.ActiveMillis, ss.UsedTurns, ss.ReservedTurns, ss.ClockAt, ss.LeaseOwner, ss.LeaseEpoch, ss.LeaseExpiresAt, ss.UpdatedAt, ss.SchedulerAgentID, string(mustJSON(ss.Roles)), ss.ArchivedAt, ss.RunPhase, ss.ID)
	return err
}

func appendSupervisionEventTx(ctx context.Context, tx *sql.Tx, ss *supervisionSession, typ string, payload any, now time.Time) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	ss.EventSeq++
	ss.UpdatedAt = now
	_, err = tx.ExecContext(ctx, `insert into supervision_events(session_id,seq,type,payload,created_at) values (?,?,?,?,?)`, ss.ID, ss.EventSeq, typ, string(raw), now)
	return err
}

func supervisionTerminal(state string) bool {
	return state == "stopped" || state == "completed" || state == "completed_with_issues" || state == "failed"
}

func accountSupervisionTime(ss *supervisionSession, now time.Time) {
	if ss.ClockAt == nil || now.Before(*ss.ClockAt) {
		return
	}
	ss.ActiveMillis += now.Sub(*ss.ClockAt).Milliseconds()
	ss.ClockAt = &now
}

// 用注入时间做领域计算，生产调用方传 time.Now；测试不必靠 sleep 耗尽预算。
func (s *Server) withSupervisionTx(ctx context.Context, id string, now time.Time, fn func(*sql.Tx, map[string]WorkspaceAdmission, *supervisionSession) error) error {
	changed := false
	err := s.withWorkspaceAdmissionTx(ctx, func(tx *sql.Tx, next map[string]WorkspaceAdmission) error {
		ss, err := loadSupervisionSession(ctx, tx, id)
		if err != nil {
			return err
		}
		beforeBytes, err := supervisionAdmittedBytes(ctx, tx, id)
		if err != nil {
			return err
		}
		version, seq := ss.StateVersion, ss.EventSeq
		accountSupervisionTime(&ss, now)
		if err := fn(tx, next, &ss); err != nil {
			return err
		}
		changed = ss.StateVersion != version || ss.EventSeq != seq
		if err := saveSupervisionSessionTx(ctx, tx, &ss); err != nil {
			return err
		}
		afterBytes, err := supervisionAdmittedBytes(ctx, tx, id)
		if err != nil {
			return err
		}
		if afterBytes > beforeBytes {
			limit := ss.Budget.storageLimit()
			control := ss.Control != "" || ss.State == "needs_input" || supervisionTerminal(ss.State) || ss.RecoveryState != "ready"
			if control {
				limit += supervisionStorageControlReserve
			}
			if afterBytes > limit {
				return errSupervisionStorage
			}
			if !control {
				if err := s.supervisionDiskAvailable(3 * (afterBytes - beforeBytes)); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if errors.Is(err, errSupervisionStorage) || errors.Is(err, errSupervisionDisk) {
		reason := "storage_limit"
		if errors.Is(err, errSupervisionDisk) {
			reason = "disk_space_low"
		}
		pauseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		pauseErr := s.pauseSupervisionForStorage(pauseCtx, id, reason)
		cancel()
		err = errors.Join(err, pauseErr)
		s.wakeSupervisionWorker(id)
	}
	if err == nil && changed {
		s.wakeSupervisionWorker(id)
	}
	return err
}

func supervisionTaskIDsTx(ctx context.Context, tx *sql.Tx, id string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `select task_id from supervision_tasks where session_id=? order by position`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var taskID string
		if err := rows.Scan(&taskID); err != nil {
			return nil, err
		}
		ids = append(ids, taskID)
	}
	return ids, rows.Err()
}

func snapshotSupervisionTaskTx(ctx context.Context, tx *sql.Tx, projectID, taskID string) (string, string, error) {
	var task Task
	if err := tx.QueryRowContext(ctx, `select id,project_id,title,description,priority,status,updated_at from tasks where id=? and project_id=?`, taskID, projectID).
		Scan(&task.ID, &task.ProjectID, &task.Title, &task.Description, &task.Priority, &task.Status, &task.UpdatedAt); err != nil {
		return "", "", err
	}
	if task.Status != taskTodo && task.Status != taskActionRequired {
		return "", "", errors.New("只允许托管待办或需要处理的任务")
	}
	rows, err := tx.QueryContext(ctx, `select t.id,t.title,t.status from task_dependencies d join tasks t on t.id=d.predecessor_task_id where d.task_id=? order by t.id`, taskID)
	if err != nil {
		return "", "", err
	}
	defer rows.Close()
	dependencies := []TaskDependency{}
	for rows.Next() {
		var d TaskDependency
		if err := rows.Scan(&d.TaskID, &d.Title, &d.Status); err != nil {
			return "", "", err
		}
		dependencies = append(dependencies, d)
	}
	if err := rows.Err(); err != nil {
		return "", "", err
	}
	taskJSON, err := json.Marshal(task)
	if err != nil {
		return "", "", err
	}
	dependencyJSON, err := json.Marshal(dependencies)
	return string(taskJSON), string(dependencyJSON), err
}

func (s *Server) createSupervisionDraft(ctx context.Context, projectID string, taskIDs []string, agentID string, budget supervisionBudget, roleOptions ...supervisionRoleConfigs) (supervisionSession, error) {
	if err := budget.validate(); err != nil {
		return supervisionSession{}, err
	}
	if len(taskIDs) == 0 || len(taskIDs) > 500 {
		return supervisionSession{}, errors.New("请选择 1 至 500 个任务")
	}
	if !validProfileAgent(agentID) {
		return supervisionSession{}, errors.New("托管 CLI 必须来自现有 CLI 目录")
	}
	roles := supervisionRoleConfigs{}
	if len(roleOptions) > 0 {
		roles = roleOptions[0]
	}
	if err := validateSupervisionRoles(roles, agentID); err != nil {
		return supervisionSession{}, err
	}
	if value := roles["scheduler"]; value.AgentID != "" {
		agentID = value.AgentID
	}
	now := time.Now().UTC()
	ss := supervisionSession{ID: uuid.NewString(), ProjectID: projectID, State: "draft", StateVersion: 1, SchedulerAgentID: agentID, Roles: roles, RouteSnapshot: json.RawMessage(`[]`), RecoveryState: "ready", Budget: budget, CreatedAt: now, UpdatedAt: now}
	err := s.withWorkspaceAdmissionTx(ctx, func(tx *sql.Tx, _ map[string]WorkspaceAdmission) error {
		previous, replay, err := supervisionRequestReceipt(ctx, tx)
		if err != nil {
			return err
		}
		if replay {
			ss, err = loadSupervisionSession(ctx, tx, previous)
			return err
		}
		budgetJSON, _ := json.Marshal(budget)
		if _, err := tx.ExecContext(ctx, `insert into supervision_sessions(id,project_id,state,state_version,scheduler_agent_id,budget,created_at,updated_at) values (?,?,'draft',1,?,?,?,?)`, ss.ID, projectID, agentID, string(budgetJSON), now, now); err != nil {
			return err
		}
		seen := map[string]bool{}
		for pos, taskID := range taskIDs {
			if strings.TrimSpace(taskID) == "" || seen[taskID] {
				return errors.New("选中任务包含空值或重复项")
			}
			seen[taskID] = true
			taskSnapshot, dependencySnapshot, err := snapshotSupervisionTaskTx(ctx, tx, projectID, taskID)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `insert into supervision_tasks(session_id,task_id,position,task_snapshot,dependency_snapshot) values (?,?,?,?,?)`, ss.ID, taskID, pos, taskSnapshot, dependencySnapshot); err != nil {
				return err
			}
		}
		if err := appendSupervisionEventTx(ctx, tx, &ss, "session.created", map[string]any{"taskIds": taskIDs, "budget": budget}, now); err != nil {
			return err
		}
		if err := saveSupervisionSessionTx(ctx, tx, &ss); err != nil {
			return err
		}
		if err := recordSupervisionRequestTx(ctx, tx, ss.ID); err != nil {
			return err
		}
		size, err := supervisionStoredBytes(ctx, tx, ss.ID)
		if err != nil {
			return err
		}
		if size > ss.Budget.storageLimit() {
			return errSupervisionStorage
		}
		return s.supervisionDiskAvailable(3 * size)

	})
	if err != nil {
		return supervisionSession{}, err
	}
	return ss, nil
}

// 供受能力检查的启动服务调用。此函数只认领并进入等待目录状态，由 worker 启动 Agent。
func (s *Server) startSupervisionSession(ctx context.Context, id string, version int64, now time.Time) error {
	s.projectLifecycleMu.Lock()
	defer s.projectLifecycleMu.Unlock()
	return s.withSupervisionRequestTx(ctx, id, now, func(tx *sql.Tx, next map[string]WorkspaceAdmission, ss *supervisionSession) error {
		if ss.State != "draft" || ss.StateVersion != version {
			return errSupervisionState
		}
		var p Project
		if err := tx.QueryRowContext(ctx, `select id,path,coalesce(nullif(runner_id,''),runner) from projects where id=?`, ss.ProjectID).Scan(&p.ID, &p.Path, &p.Runner); err != nil {
			return err
		}
		ids, err := supervisionTaskIDsTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := claimSupervisionTasksTx(ctx, tx, p.ID, id, ids, now); err != nil {
			return err
		}
		for _, taskID := range ids {
			taskSnapshot, dependencySnapshot, err := snapshotSupervisionTaskTx(ctx, tx, p.ID, taskID)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `update supervision_tasks set task_snapshot=?,dependency_snapshot=? where session_id=? and task_id=?`, taskSnapshot, dependencySnapshot, id, taskID); err != nil {
				return err
			}
		}
		admission, err := s.reserveWorkspaceAdmissionTx(ctx, tx, next, p, id)
		if err != nil {
			return err
		}
		// 路由仅保存非敏感版本引用。池路由保留池版本，不能伪装成已经绑定某个账号。
		routes := []map[string]string{}
		rows, err := tx.QueryContext(ctx, `select r.agent_id,r.current_revision_id,v.mode,v.profile_revision_id,v.pool_revision_id from project_agent_routes r join project_agent_route_revisions v on v.id=r.current_revision_id where r.project_id=? and r.enabled=1 and v.state='active' order by r.agent_id`, p.ID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var agent, revision, mode, profile, pool string
			if err := rows.Scan(&agent, &revision, &mode, &profile, &pool); err != nil {
				rows.Close()
				return err
			}
			routes = append(routes, map[string]string{"agentId": agent, "routeRevisionId": revision, "mode": mode, "profileRevisionId": profile, "poolRevisionId": pool})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		// 调度角色即使使用 CLI 登录或旧 default_profile_id，也冻结本次选择。
		foundScheduler := false
		for _, route := range routes {
			if route["agentId"] == ss.SchedulerAgentID {
				foundScheduler = true
			}
		}
		if !foundScheduler {
			selection, err := s.profileRouteForNewConversationTx(supervisionProfileContext(ctx), tx, nil, p.Runner, ss.SchedulerAgentID, p.ID)
			if err != nil {
				return err
			}
			mode := "cli_managed"
			if selection.ProfileRevisionID != "" {
				mode = "pinned"
			}
			routes = append(routes, map[string]string{"agentId": ss.SchedulerAgentID, "mode": mode, "routeRevisionId": selection.RouteRevisionID, "profileRevisionId": selection.ProfileRevisionID})
		}
		ss.RouteSnapshot, _ = json.Marshal(routes)
		if err := s.freezeSupervisionRoleRoutesTx(ctx, tx, ss, p.Runner); err != nil {
			return err
		}
		ss.AdmissionID, ss.AdmissionEpoch = admission.ID, admission.Epoch
		ss.State, ss.Reason, ss.ClockAt = "running", "waiting_workspace", &now
		ss.StateVersion++
		return appendSupervisionEventTx(ctx, tx, ss, "session.started", map[string]any{"taskIds": ids, "admissionId": admission.ID}, now)
	})
}

func (s *Server) supervisionEvents(ctx context.Context, id string, after int64, limit int) ([]supervisionEvent, error) {
	if after < 0 || limit < 1 || limit > 500 {
		return nil, errors.New("无效的托管事件游标或数量")
	}
	// 不把“会话不存在”与“暂时没有新事件”混为一谈。
	if _, err := loadSupervisionSession(ctx, s.db, id); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `select seq,type,payload,created_at from supervision_events where session_id=? and seq>? order by seq limit ?`, id, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []supervisionEvent{}
	for rows.Next() {
		event := supervisionEvent{SessionID: id}
		var raw string
		if err := rows.Scan(&event.Seq, &event.Type, &raw, &event.CreatedAt); err != nil {
			return nil, err
		}
		event.Payload = json.RawMessage(raw)
		events = append(events, event)
	}
	return events, rows.Err()
}

func requireSupervisionVersion(ss *supervisionSession, version int64) error {
	if ss.StateVersion != version {
		return fmt.Errorf("%w: expected %d, current %d", errSupervisionState, version, ss.StateVersion)
	}
	return nil
}
