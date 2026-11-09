package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// 在执行启动事务中保存归属与恢复事实，角色输出不能替代进程退出证明。
type supervisionRunOwnership struct {
	SessionID      string `json:"sessionId"`
	WorkItemID     string `json:"workItemId"`
	TaskID         string `json:"taskId,omitempty"`
	Role           string `json:"role"`
	Attempt        int    `json:"attempt"`
	AdmissionID    string `json:"admissionId"`
	Epoch          int64  `json:"epoch"`
	RunID          string `json:"runId"`
	ConversationID string `json:"conversationId"`
	ProcessState   string `json:"processState"`
	OutcomeState   string `json:"outcomeState"`
}

func (s *Server) migrateSupervisionRuns(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `create table if not exists supervision_run_links (
		run_id text primary key references runs(id) on delete cascade,
		conversation_id text not null references conversations(id) on delete cascade,
		session_id text not null,
		work_item_id text not null,
		task_id text not null default '',
		role text not null check(role in ('scheduler','implementation','test','review')),
		attempt integer not null check(attempt>0),
		admission_id text not null,
		epoch integer not null check(epoch>0),
		process_state text not null check(process_state in ('launching','running','quiescent','unknown')),
		outcome_state text not null check(outcome_state in ('pending','known','unknown')),
		created_at datetime not null,
		unique(session_id,work_item_id,attempt)
	);
	create index if not exists supervision_run_links_conversation on supervision_run_links(conversation_id);
	create index if not exists supervision_run_links_session on supervision_run_links(session_id,process_state);
	create table if not exists supervision_conversation_links (
		conversation_id text primary key references conversations(id) on delete cascade,
		session_id text not null,
		role text not null check(role in ('scheduler','implementation','test','review')),
		created_at datetime not null
	);
	create index if not exists supervision_conversation_links_session on supervision_conversation_links(session_id);
	insert or ignore into supervision_conversation_links(conversation_id,session_id,role,created_at)
		select conversation_id,session_id,role,min(created_at) from supervision_run_links group by conversation_id;`)
	return err
}

// 角色归属先于第一个 Run 保存；空闲、暂停和恢复期间也不能被普通入口接管。
func linkSupervisionConversationTx(ctx context.Context, tx *sql.Tx, conversationID, sessionID, role string) error {
	if role != "scheduler" && role != "implementation" && role != "test" && role != "review" {
		return errors.New("unsupported supervision role")
	}
	var admitted bool
	if err := tx.QueryRowContext(ctx, `select exists(select 1 from conversations c join workspace_admissions a on a.project_id=c.project_id where c.id=? and a.owner_id=?)`, conversationID, sessionID).Scan(&admitted); err != nil {
		return err
	}
	if !admitted {
		return errors.New("supervision conversation does not belong to the admitted project")
	}
	var previousOwner, previousRole string
	err := tx.QueryRowContext(ctx, `select session_id,role from supervision_conversation_links where conversation_id=?`, conversationID).Scan(&previousOwner, &previousRole)
	if err == nil {
		if previousOwner != sessionID || previousRole != role {
			return errors.New("supervision conversation already belongs to another owner or role")
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = tx.ExecContext(ctx, `insert into supervision_conversation_links(conversation_id,session_id,role,created_at) values (?,?,?,?)`, conversationID, sessionID, role, time.Now().UTC())
	return err
}

func linkSupervisionRunTx(ctx context.Context, tx *sql.Tx, ownership supervisionRunOwnership) error {
	if ownership.RunID == "" || ownership.SessionID == "" || ownership.WorkItemID == "" || ownership.ConversationID == "" || ownership.Attempt < 1 {
		return errors.New("supervision run ownership is incomplete")
	}
	if ownership.Role != "scheduler" && ownership.Role != "implementation" && ownership.Role != "test" && ownership.Role != "review" {
		return errors.New("unsupported supervision role")
	}
	var projectID, conversationID, workspacePath string
	var admission WorkspaceAdmission
	if err := tx.QueryRowContext(ctx, `select r.conversation_id,c.project_id,r.workspace_path from runs r join conversations c on c.id=r.conversation_id where r.id=?`, ownership.RunID).Scan(&conversationID, &projectID, &workspacePath); err != nil {
		return err
	}
	if conversationID != ownership.ConversationID {
		return errors.New("run does not belong to the supervision conversation")
	}
	if err := tx.QueryRowContext(ctx, `select project_id,canonical_path,original_path,directory_identity from workspace_admissions where id=? and owner_id=? and epoch=? and state='owned' and recovery_state='ready'`, ownership.AdmissionID, ownership.SessionID, ownership.Epoch).Scan(&admission.ProjectID, &admission.CanonicalPath, &admission.OriginalPath, &admission.DirectoryIdentity); err != nil {
		return fmt.Errorf("supervision workspace is not admitted: %w", err)
	}
	if projectID != admission.ProjectID {
		return errors.New("supervision run belongs to another project")
	}
	if err := verifySupervisionRunWorkspaceTx(ctx, tx, ownership, admission, workspacePath); err != nil {
		return err
	}
	// A2 的独立准入兼容保留；一旦存在正式托管会话，Run 必须对应已授权的实际工作项。
	var managedSession bool
	if err := tx.QueryRowContext(ctx, `select exists(select 1 from supervision_sessions where id=?)`, ownership.SessionID).Scan(&managedSession); err != nil {
		return err
	}
	if managedSession {
		var authorized bool
		if err := tx.QueryRowContext(ctx, `select exists(select 1 from supervision_work_items w join supervision_sessions ss on ss.id=w.session_id where w.id=? and w.session_id=? and w.task_id=? and w.role=? and w.attempt=? and w.admission_epoch=? and w.lease_epoch=ss.lease_epoch and w.state in ('launching','running') and ss.recovery_state='ready')`, ownership.WorkItemID, ownership.SessionID, ownership.TaskID, ownership.Role, ownership.Attempt, ownership.Epoch).Scan(&authorized); err != nil {
			return err
		}
		if !authorized {
			return errors.New("supervision run does not match an authorized work item")
		}
	}
	if err := linkSupervisionConversationTx(ctx, tx, ownership.ConversationID, ownership.SessionID, ownership.Role); err != nil {
		return err
	}
	if ownership.TaskID != "" {
		var ownerID, kind, taskProject string
		if err := tx.QueryRowContext(ctx, `select c.owner_id,c.owner_kind,t.project_id from task_execution_claims c join tasks t on t.id=c.task_id where c.task_id=?`, ownership.TaskID).Scan(&ownerID, &kind, &taskProject); err != nil {
			return err
		}
		if ownerID != ownership.SessionID || kind != "supervision" || taskProject != projectID {
			return errTaskExecutionClaimed
		}
	}
	// 相同 Run 重放幂等，但不能通过 ON CONFLICT 覆盖归属、角色或工作项。
	var existing string
	err := tx.QueryRowContext(ctx, `select run_id from supervision_run_links where run_id=? and conversation_id=? and session_id=? and work_item_id=? and task_id=? and role=? and attempt=? and admission_id=? and epoch=?`, ownership.RunID, ownership.ConversationID, ownership.SessionID, ownership.WorkItemID, ownership.TaskID, ownership.Role, ownership.Attempt, ownership.AdmissionID, ownership.Epoch).Scan(&existing)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = tx.ExecContext(ctx, `insert into supervision_run_links(run_id,conversation_id,session_id,work_item_id,task_id,role,attempt,admission_id,epoch,process_state,outcome_state,created_at) values (?,?,?,?,?,?,?,?,?,'launching','pending',?)`, ownership.RunID, ownership.ConversationID, ownership.SessionID, ownership.WorkItemID, ownership.TaskID, ownership.Role, ownership.Attempt, ownership.AdmissionID, ownership.Epoch, time.Now().UTC())
	return err
}

func verifySupervisionRunWorkspaceTx(ctx context.Context, tx *sql.Tx, ownership supervisionRunOwnership, admission WorkspaceAdmission, workspacePath string) error {
	project, err := supervisionProjectTx(ctx, tx, admission.ProjectID)
	if err != nil {
		return err
	}
	// Route calculation uses only runtime/project metadata, never a live runner.
	resolver := &Server{}
	projectRoute := resolver.supervisionProjectRoute(project)
	intent, err := loadSupervisionTargetControl(ctx, tx, ownership.SessionID)
	if errors.Is(err, sql.ErrNoRows) {
		var runnerID string
		if err := tx.QueryRowContext(ctx, `select agent_runtime_id from runs where id=?`, ownership.RunID).Scan(&runnerID); err != nil {
			return err
		}
		if runnerID == "" {
			runnerID = resolver.localRunnerID() // Historical local runs have no explicit runtime.
		}
		if runnerID != projectRoute.RunnerID || !projectRoute.local() {
			return errSupervisionProcessUnsupported
		}
		canonical, err := canonicalWorkspacePath(workspacePath)
		if err != nil || canonical != admission.CanonicalPath {
			return errors.New("supervision run workspace does not match its admitted directory")
		}
		return verifyWorkspaceAdmissionPath(admission)
	}
	if err != nil {
		return err
	}
	if _, err := resolver.checkSupervisionTargetProjectRoute(ctx, tx, supervisionTargetConnectionKey(intent), project); err != nil {
		return err
	}
	var authorized bool
	c := intent.Control
	err = tx.QueryRowContext(ctx, `select exists(select 1 from runs r join conversations cv on cv.id=r.conversation_id join supervision_target_bindings b on b.runner_id=? join supervision_sessions ss on ss.id=? where r.id=? and r.agent_runtime_id=? and b.target_id=? and b.protocol=? and ss.admission_id=? and ss.admission_epoch=? and ss.lease_epoch=? and ss.control='' and ss.recovery_state='ready')`, supervisionTargetConnectionKey(intent), ownership.SessionID, ownership.RunID, intent.RunnerID, c.TargetID, supervisionExecutionProtocol, ownership.AdmissionID, ownership.Epoch, c.OwnerEpoch).Scan(&authorized)
	if err != nil {
		return err
	}
	if !authorized || c.WorkspaceID != admission.DirectoryIdentity || c.State != "running" || workspacePath != admission.OriginalPath && workspacePath != admission.CanonicalPath {
		return errWorkspaceAdmissionStale
	}
	return nil
}

func supervisionRunsQuiescentTx(ctx context.Context, tx *sql.Tx, ownerID string) error {
	var active bool
	err := tx.QueryRowContext(ctx, `select exists(select 1 from supervision_run_links l join runs r on r.id=l.run_id where l.session_id=? and (l.process_state<>'quiescent' or r.status in ('queued','running'))) or exists(select 1 from task_runs where execution_source='supervision' and execution_owner_id=? and status in ('queued','running'))`, ownerID, ownerID).Scan(&active)
	if err != nil {
		return err
	}
	if active {
		return errWorkspaceNotQuiescent
	}
	err = tx.QueryRowContext(ctx, `select exists(select 1 from supervision_work_items where session_id=? and state in ('launching','running','unknown'))`, ownerID).Scan(&active)
	if err != nil {
		return err
	}
	if active {
		return errWorkspaceNotQuiescent
	}
	err = tx.QueryRowContext(ctx, `select exists(select 1 from supervision_executions where session_id=? and state<>'quiescent')`, ownerID).Scan(&active)
	if err != nil {
		return err
	}
	if active {
		return errWorkspaceNotQuiescent
	}
	err = tx.QueryRowContext(ctx, `select exists(select 1 from supervision_context_captures where session_id=? and state='collecting')`, ownerID).Scan(&active)
	if err != nil {
		return err
	}
	if active {
		return errWorkspaceNotQuiescent
	}
	if err := tx.QueryRowContext(ctx, `select exists(select 1 from supervision_mutations where session_id=? and state in ('applying','unknown'))`, ownerID).Scan(&active); err != nil {
		return err
	}
	if active {
		return errWorkspaceNotQuiescent
	}
	return nil
}

func rejectSupervisionConversationMessage(ctx context.Context, q sqlQueryer, conversationID string) error {
	var exists bool
	if err := q.QueryRowContext(ctx, `select exists(select 1 from supervision_conversation_links where conversation_id=?)`, conversationID).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return errors.New("托管角色会话不能通过普通消息入口执行，请在任务托管页操作")
	}
	return nil
}

func rejectActiveSupervisionConversation(ctx context.Context, q sqlQueryer, conversationID string) error {
	var active bool
	err := q.QueryRowContext(ctx, `select exists(select 1 from supervision_conversation_links l join workspace_admissions a on a.owner_id=l.session_id where l.conversation_id=?)`, conversationID).Scan(&active)
	if err != nil {
		return err
	}
	if active {
		return errors.New("托管角色会话仍参与活动托管，请先停止并完成资源收敛")
	}
	return nil
}
