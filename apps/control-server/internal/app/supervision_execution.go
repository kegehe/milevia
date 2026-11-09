package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

type supervisionExecutionPermit struct {
	Decision        supervisionDecision
	Lease           supervisionLease
	Token           string
	Container       string
	RunID           string
	ConversationID  string
	Profile         *AgentRuntimeProfile
	Role            string
	TaskID          string
	AgentID         string
	Model           string
	ReasoningEffort string
}

func supervisionTokenHash(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

func checkSupervisionExecutionTx(ctx context.Context, tx *sql.Tx, ss *supervisionSession, permit supervisionExecutionPermit) (string, error) {
	state, err := checkSupervisionExecutionIdentityTx(ctx, tx, ss.ID, permit)
	if err != nil {
		return "", err
	}
	if permit.Lease.Epoch != ss.LeaseEpoch || ss.LeaseOwner != permit.Lease.Owner || ss.RecoveryState != "ready" {
		return "", errSupervisionLease
	}
	return state, nil
}

// Identity remains verifiable after authority expires. Cleanup may record the
// original execution's uncertainty, but never gains permission for new work.
func checkSupervisionExecutionIdentityTx(ctx context.Context, tx *sql.Tx, sessionID string, permit supervisionExecutionPermit) (string, error) {
	var state, hash, container, runID, decisionID, workItemID, conversationID string
	var attempt int
	var epoch, version, basis int64
	err := tx.QueryRowContext(ctx, `select e.state,e.token_hash,e.container,e.run_id,w.lease_epoch,w.decision_id,w.id,w.attempt,r.conversation_id,d.expected_state_version,d.basis_seq from supervision_executions e join supervision_work_items w on w.execution_id=e.execution_id join runs r on r.id=e.run_id join supervision_decisions d on d.id=w.decision_id where e.execution_id=? and e.session_id=?`, permit.Decision.ExecutionID, sessionID).Scan(&state, &hash, &container, &runID, &epoch, &decisionID, &workItemID, &attempt, &conversationID, &version, &basis)
	if err != nil {
		return "", err
	}
	if sessionID != permit.Decision.SessionID || permit.Token == "" || subtle.ConstantTimeCompare([]byte(hash), []byte(supervisionTokenHash(permit.Token))) != 1 || container != permit.Container || runID != permit.RunID || decisionID != permit.Decision.ID || workItemID != permit.Decision.WorkItemID || attempt != permit.Decision.Attempt || conversationID != permit.ConversationID || version != permit.Decision.ExpectedStateVersion || basis != permit.Decision.BasisSeq || epoch != permit.Lease.Epoch {
		return "", errSupervisionLease
	}
	return state, nil
}

// 调用者已经持久化 launching；本事务创建专属角色会话/Run、单次子令牌和命名容器。
// 子令牌只返回可信执行器，库中仅存哈希；不发给模型、普通审批入口或前端。
func (s *Server) prepareSupervisionExecution(ctx context.Context, d supervisionDecision, lease supervisionLease, now time.Time) (supervisionExecutionPermit, error) {
	permit := supervisionExecutionPermit{Decision: d, Lease: lease, Container: supervisionJobPrefix + uuid.NewString(), RunID: uuid.NewString(), ConversationID: uuid.NewString()}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return permit, err
	}
	permit.Token = hex.EncodeToString(random[:])
	err := s.withSupervisionTx(ctx, d.SessionID, now, func(tx *sql.Tx, next map[string]WorkspaceAdmission, ss *supervisionSession) error {
		if err := requireSupervisionLease(ss, lease, now); err != nil {
			return err
		}
		if ss.Control != "" || ss.RecoveryState != "ready" || ss.StateVersion != d.ExpectedStateVersion {
			return errSupervisionState
		}
		if err := requireSupervisionRoleObservationsTx(ctx, tx, ss.ID); err != nil {
			return err
		}
		var state, decisionID string
		if err := tx.QueryRowContext(ctx, `select state,decision_id,role,task_id from supervision_work_items where execution_id=? and session_id=? and lease_epoch=?`, d.ExecutionID, ss.ID, lease.Epoch).Scan(&state, &decisionID, &permit.Role, &permit.TaskID); err != nil {
			return err
		}
		if state != "launching" || decisionID != d.ID {
			return errSupervisionState
		}
		config := ss.roleConfig(permit.Role)
		permit.AgentID, permit.Model, permit.ReasoningEffort = config.AgentID, config.Model, config.ReasoningEffort
		if !validProfileAgent(permit.AgentID) {
			return errors.New("未知的托管 CLI")
		}
		if permit.Role != "scheduler" {
			if _, err := tx.ExecContext(ctx, `update supervision_role_jobs set state='running' where execution_id=? and state='queued'`, d.ExecutionID); err != nil {
				return err
			}
		}
		a, exists := next[ss.AdmissionID]
		if !exists || a.State != "owned" || a.RecoveryState != "ready" || a.Epoch != ss.AdmissionEpoch || a.OwnerID != ss.ID {
			return errWorkspaceAdmissionStale
		}
		source, err := s.supervisionWorkspaceSourceTx(ctx, tx, ss, a)
		if err != nil {
			return err
		}
		runnerID := source.RunnerID
		if source.Control == nil {
			if err := supervisionLocalProcessCapability(); err != nil {
				return err
			}
			target := s.supervisionProjectTarget(Project{Runner: runnerID, Path: a.OriginalPath})
			if !target.Local || target.State != "ready" {
				return errors.New(target.Reason)
			}
			if permit.Role != "test" {
				if _, err := s.supervisionAdapter(permit.AgentID); err != nil {
					return err
				}
			}
		}
		var snapshots []map[string]string
		if err := json.Unmarshal(ss.RouteSnapshot, &snapshots); err != nil {
			return err
		}
		var profileRevision, routeRevision string
		foundRoute := false
		role := permit.Role
		if role == "test" {
			role = "scheduler"
		}
		ordered := []map[string]string{}
		for _, route := range snapshots {
			if route["role"] == role {
				ordered = append(ordered, route)
			}
		}
		for _, route := range snapshots {
			if route["role"] == "" {
				ordered = append(ordered, route)
			}
		}
		for _, route := range ordered {
			if route["agentId"] != permit.AgentID {
				continue
			}
			foundRoute = true
			profileRevision, routeRevision = route["profileRevisionId"], route["routeRevisionId"]
			if route["mode"] == "pool" {
				var err error
				profileRevision, err = s.selectPoolProfileRevisionTx(ctx, tx, routeRevision, runnerID, permit.AgentID)
				if err != nil {
					return err
				}
			}
			break
		}
		// 防止旧草稿/早期状态没有冻结路由时，静默使用后来更改的凭据配置。
		if !foundRoute {
			selection, err := s.profileRouteForNewConversationTx(ctx, tx, nil, runnerID, permit.AgentID, ss.ProjectID)
			if err != nil {
				return err
			}
			if selection.ProfileRevisionID != "" || selection.RouteRevisionID != "" {
				return errors.New("托管缺少冻结的调度角色路由，需要重新建立会话")
			}
		}
		permit.Profile, err = s.runtimeProfileTx(supervisionProfileContext(ctx), tx, profileRevision, runnerID, permit.AgentID)
		if err != nil {
			return err
		}
		if permit.Profile != nil && source.Control != nil && permit.Profile.ExecutionMode != "target_isolated" {
			return errors.New("跨端托管认证档案必须绑定实际执行目标")
		}
		if permit.Profile != nil && permit.Profile.TargetID != "" {
			expected := "native:" + runnerID
			if source.Control != nil {
				expected = source.Control.TargetID
			}
			if permit.Profile.TargetID != expected {
				return errors.New("认证档案与当前项目的实际目标不一致")
			}
		}
		nativeID := uuid.NewString()
		if _, err := tx.ExecContext(ctx, `insert into conversations(id,project_id,claude_session_id,agent_id,agent_session_id,agent_runtime_id,agent_profile_revision_id,project_agent_route_revision_id,execution_policy,status,permission_mode,title,last_activity_at,is_current,created_at) values (?,?,?,?,?,?,?,?, 'read_only','running','read_only','托管调度',?,0,?)`, permit.ConversationID, ss.ProjectID, nativeID, permit.AgentID, nativeID, runnerID, profileRevision, routeRevision, now, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `insert into runs(id,conversation_id,agent_id,agent_runtime_id,agent_profile_revision_id,project_agent_route_revision_id,execution_policy,workspace_path,status,created_at) values (?,?,?,?,?,?,'read_only',?,'running',?)`, permit.RunID, permit.ConversationID, permit.AgentID, runnerID, profileRevision, routeRevision, a.OriginalPath, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `update conversations set title=?,model_override=? where id=?`, supervisionRoleTitle(permit.Role), permit.Model, permit.ConversationID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `insert into conversation_model_settings(conversation_id,reasoning_effort) values (?,?)`, permit.ConversationID, permit.ReasoningEffort); err != nil {
			return err
		}
		if err := linkSupervisionRunTx(ctx, tx, supervisionRunOwnership{SessionID: ss.ID, WorkItemID: d.WorkItemID, TaskID: permit.TaskID, Role: permit.Role, Attempt: d.Attempt, AdmissionID: a.ID, Epoch: a.Epoch, RunID: permit.RunID, ConversationID: permit.ConversationID}); err != nil {
			return err
		}
		if err := func() error {
			if permit.Role == "test" {
				return nil
			}
			return s.reserveProfileQuotaTx(ctx, tx, permit.RunID, profileRevision)
		}(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `insert into supervision_executions(execution_id,session_id,run_id,token_hash,container,state,created_at,updated_at) values (?,?,?,?,?,'prepared',?,?)`, d.ExecutionID, ss.ID, permit.RunID, supervisionTokenHash(permit.Token), permit.Container, now, now); err != nil {
			return err
		}
		var input string
		if err := tx.QueryRowContext(ctx, `select input_json from supervision_scheduler_inputs where execution_id=?`, d.ExecutionID).Scan(&input); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `insert into messages(id,conversation_id,run_id,role,content,created_at) values (?,?,?,'user',?,?)`, uuid.NewString(), permit.ConversationID, permit.RunID, input, now); err != nil {
			return err
		}
		if permit.Role == "implementation" {
			var sequence int
			if err := tx.QueryRowContext(ctx, `select coalesce(max(sequence),0)+1 from task_runs where task_id=?`, permit.TaskID).Scan(&sequence); err != nil {
				return err
			}
			taskRunID := uuid.NewString()
			if _, err := tx.ExecContext(ctx, `insert into task_runs(id,task_id,conversation_id,run_id,sequence,status,prompt_snapshot,acceptance_snapshot,execution_source,execution_owner_id,created_at,started_at) values (?,?,?,?,?,'running',?,'','supervision',?,?,?)`, taskRunID, permit.TaskID, permit.ConversationID, permit.RunID, sequence, input, ss.ID, now, now); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `update tasks set last_task_run_id=? where id=?`, taskRunID, permit.TaskID); err != nil {
				return err
			}
		}
		return appendSupervisionEventTx(ctx, tx, ss, "execution.prepared", map[string]string{"executionId": d.ExecutionID, "runId": permit.RunID, "conversationId": permit.ConversationID}, now)
	})
	if err != nil {
		return supervisionExecutionPermit{}, err
	}
	return permit, nil
}

// 身份先提交，再单独进入 gate 放行主线程，避免事务失败时丢失已经执行的事实。
func (s *Server) registerSupervisionProcess(ctx context.Context, permit supervisionExecutionPermit, id supervisionProcessIdentity, now time.Time) error {
	return s.withSupervisionTx(ctx, permit.Decision.SessionID, now, func(tx *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
		state, err := checkSupervisionExecutionTx(ctx, tx, ss, permit)
		if err != nil {
			return err
		}
		if (state != "prepared" && state != "starting") || id.Container != permit.Container || id.PID < 1 || id.Birth == "" {
			return errSupervisionState
		}
		if err := verifySupervisionResolvedProcessTx(ctx, tx, permit, id); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `update supervision_executions set state='created',pid=?,birth=?,executable=?,directory=?,updated_at=? where execution_id=?`, id.PID, id.Birth, id.Executable, id.Directory, now, permit.Decision.ExecutionID)
		return err
	})
}

func (s *Server) resumeSupervisionProcess(ctx context.Context, permit supervisionExecutionPermit, resume func() error, now time.Time) error {
	return s.withSupervisionTx(ctx, permit.Decision.SessionID, now, func(tx *sql.Tx, next map[string]WorkspaceAdmission, ss *supervisionSession) error {
		state, err := checkSupervisionExecutionTx(ctx, tx, ss, permit)
		if err != nil {
			return err
		}
		if state != "created" || ss.Control != "" || ss.StateVersion != permit.Decision.ExpectedStateVersion {
			return errSupervisionState
		}
		if err := requireSupervisionLease(ss, permit.Lease, now); err != nil {
			return err
		}
		a, exists := next[ss.AdmissionID]
		if !exists || a.State != "owned" || a.Epoch != ss.AdmissionEpoch || a.RecoveryState != "ready" || s.admissionHasOperationsLocked(a) {
			return errWorkspaceNotQuiescent
		}
		if err := verifyWorkspaceAdmissionPath(a); err != nil {
			return err
		}
		if err := resume(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `update supervision_executions set state='running',updated_at=? where execution_id=?`, now, permit.Decision.ExecutionID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `update supervision_work_items set state='running' where execution_id=?`, permit.Decision.ExecutionID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `update supervision_run_links set process_state='running' where run_id=?`, permit.RunID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `update supervision_outbox set state='delivered',updated_at=? where execution_id=?`, now, permit.Decision.ExecutionID); err != nil {
			return err
		}
		return appendSupervisionEventTx(ctx, tx, ss, "execution.running", map[string]string{"executionId": permit.Decision.ExecutionID, "runId": permit.RunID}, now)
	})
}

func (s *Server) finishSupervisionExecution(ctx context.Context, permit supervisionExecutionPermit, receipt supervisionSchedulerCLIReceipt, runErr error, now time.Time) error {
	var remote bool
	if err := s.db.QueryRowContext(ctx, `select exists(select 1 from supervision_target_executions where execution_id=?)`, permit.Decision.ExecutionID).Scan(&remote); err != nil {
		return err
	}
	if remote {
		return errors.New("目标执行必须核对目标端静默事实，不能使用控制机容器收口")
	}
	quiet := receipt.ContainerQuiet && (!receipt.ProcessStarted || receipt.ProcessWaited)
	if quiet {
		var err error
		quiet, err = supervisionContainerQuiescent(permit.Container)
		if err != nil {
			return err
		}
	}
	return s.finishSupervisionExecutionFacts(ctx, permit, receipt, runErr, now, func(*sql.Tx) (bool, error) { return quiet, nil })
}

// The proof is checked inside the same short domain transaction that settles
// Run/work-item facts. It performs only local durable reads, never network I/O.
func (s *Server) finishSupervisionExecutionFacts(ctx context.Context, permit supervisionExecutionPermit, receipt supervisionSchedulerCLIReceipt, runErr error, now time.Time, proof func(*sql.Tx) (bool, error)) error {
	return s.withSupervisionTx(ctx, permit.Decision.SessionID, now, func(tx *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
		state, err := checkSupervisionExecutionTx(ctx, tx, ss, permit)
		if err != nil {
			return err
		}
		if state == "quiescent" {
			return nil
		}
		if state == "unknown" {
			return errWorkspaceNotQuiescent
		}
		quiet, err := proof(tx)
		if err != nil {
			return err
		}
		class := "work"
		if ss.State == "finalizing" {
			class = "final"
		}
		if ss.Control == "" && (ss.State == "running" || ss.State == "finalizing") && supervisionTimeAvailable(ss, class) <= 0 {
			if err := pauseSupervisionForBudgetTx(ctx, tx, ss, now); err != nil {
				return err
			}
		}
		// The full response already has a message/decision record. Keep lifecycle
		// receipts small so storage exhaustion cannot prevent process settlement.
		storedReceipt := receipt
		storedReceipt.Output = ""
		raw, err := json.Marshal(struct {
			supervisionSchedulerCLIReceipt
			OutputSHA256 string `json:"outputSha256"`
			OutputBytes  int    `json:"outputBytes"`
		}{storedReceipt, supervisionSHA256([]byte(receipt.Output)), len(receipt.Output)})
		if err != nil {
			return err
		}
		message := ""
		if runErr != nil {
			message = redactAgentText(runErr.Error())
			if len(message) > 4096 {
				message = message[:4096]
			}
		}
		if !quiet {
			if _, err := tx.ExecContext(ctx, `update supervision_executions set state='unknown',receipt=?,error=?,updated_at=? where execution_id=?`, string(raw), message, now, permit.Decision.ExecutionID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `update supervision_work_items set state='unknown' where execution_id=?`, permit.Decision.ExecutionID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `update supervision_run_links set process_state='unknown',outcome_state='unknown' where run_id=?`, permit.RunID); err != nil {
				return err
			}
			ss.RecoveryState = "outcome_unknown"
			ss.Reason = "process_not_quiescent"
			ss.StateVersion++
			return appendSupervisionEventTx(ctx, tx, ss, "execution.unknown", map[string]string{"executionId": permit.Decision.ExecutionID, "runId": permit.RunID, "error": message}, now)
		}
		status := "completed"
		if runErr != nil {
			status = "failed"
		}
		if ss.Control == "stop" || (runErr != nil && ss.Control != "") || errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
			status = "interrupted"
		}
		if _, err := tx.ExecContext(ctx, `update supervision_executions set state='quiescent',receipt=?,error=?,updated_at=? where execution_id=?`, string(raw), message, now, permit.Decision.ExecutionID); err != nil {
			return err
		}
		if err := s.recordQuotaFailure(ctx, tx, permit.RunID, runErr); err != nil {
			return err
		}
		if err := s.settleSupervisionRunFactsTx(ctx, tx, permit.Decision.ExecutionID, permit.RunID, permit.ConversationID, status, "known", now); err != nil {
			return err
		}
		if runErr != nil && (permit.Role == "" || permit.Role == "scheduler") {
			if _, err := tx.ExecContext(ctx, `update supervision_decisions set state='stale',validation_error=?,completed_at=? where id=?`, message, now, permit.Decision.ID); err != nil {
				return err
			}
			if ss.Control == "" {
				ss.State = "needs_input"
				ss.Reason = "scheduler_execution_failed"
				ss.ClockAt = nil
				ss.StateVersion++
			}
		}
		return appendSupervisionEventTx(ctx, tx, ss, "execution.quiescent", map[string]string{"executionId": permit.Decision.ExecutionID, "runId": permit.RunID, "status": status, "error": message}, now)
	})
}

func (s *Server) settleSupervisionRunFactsTx(ctx context.Context, tx *sql.Tx, executionID, runID, conversationID, status, outcome string, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `delete from supervision_storage_reservations where execution_id=?`, executionID); err != nil {
		return err
	}
	for _, update := range []struct {
		query string
		args  []any
	}{
		{`update runs set status=?,completed_at=? where id=?`, []any{status, now, runID}},
		{`update conversations set status='idle',last_activity_at=? where id=?`, []any{now, conversationID}},
		{`update supervision_run_links set process_state='quiescent',outcome_state=? where run_id=?`, []any{outcome, runID}},
		{`update supervision_work_items set state='quiescent',finished_at=? where execution_id=?`, []any{now, executionID}},
		{`update supervision_outbox set state='completed',updated_at=? where execution_id=?`, []any{now, executionID}},
		{`update run_usage set completed_at=? where run_id=?`, []any{now, runID}},
	} {
		if _, err := tx.ExecContext(ctx, update.query, update.args...); err != nil {
			return err
		}
	}
	if err := s.reconcileQuotaUsageTx(ctx, tx, runID); err != nil {
		return err
	}
	if err := s.releaseQuotaReservations(ctx, tx, runID); err != nil {
		return err
	}
	return s.completeRevocationJobsForRunTx(ctx, tx, runID)
}

// 重启后的核对只清理已确认退出的执行事实；不解除会话恢复保护，不自动续跑。
func (s *Server) reconcileSupervisionExecution(ctx context.Context, executionID string, now time.Time) error {
	var target bool
	if err := s.db.QueryRowContext(ctx, `select exists(select 1 from supervision_target_executions where execution_id=?)`, executionID).Scan(&target); err != nil {
		return err
	}
	if target {
		return s.reconcileSupervisionTargetExecution(ctx, executionID, now)
	}
	var sessionID, container string
	if err := s.db.QueryRowContext(ctx, `select session_id,container from supervision_executions where execution_id=?`, executionID).Scan(&sessionID, &container); err != nil {
		return err
	}
	quiet, err := supervisionContainerQuiescent(container)
	if err != nil {
		return err
	}
	if !quiet {
		return errWorkspaceNotQuiescent
	}
	return s.reconcileSupervisionExecutionFacts(ctx, executionID, sessionID, container, now, nil)
}

func (s *Server) reconcileSupervisionExecutionFacts(ctx context.Context, executionID, sessionID, container string, now time.Time, proof func(*sql.Tx) error) error {
	var reconciledRunID string
	err := s.withSupervisionTx(ctx, sessionID, now, func(tx *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
		if ss.RecoveryState == "ready" {
			return errSupervisionState
		}
		var state, runID, conversationID, actualContainer string
		if err := tx.QueryRowContext(ctx, `select e.state,e.container,e.run_id,r.conversation_id from supervision_executions e join runs r on r.id=e.run_id where e.execution_id=? and e.session_id=?`, executionID, sessionID).Scan(&state, &actualContainer, &runID, &conversationID); err != nil {
			return err
		}
		if state == "quiescent" {
			reconciledRunID = runID
			return nil
		}
		if state != "unknown" || actualContainer != container {
			return errSupervisionState
		}
		if proof != nil {
			if err := proof(tx); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `update supervision_executions set state='quiescent',updated_at=? where execution_id=?`, now, executionID); err != nil {
			return err
		}
		if err := s.settleSupervisionRunFactsTx(ctx, tx, executionID, runID, conversationID, "interrupted", "unknown", now); err != nil {
			return err
		}
		reconciledRunID = runID
		active, err := supervisionHasExecutionTx(ctx, tx, ss.ID)
		if err != nil {
			return err
		}
		if !active {
			ss.ClockAt = nil
		}
		ss.StateVersion++
		return appendSupervisionEventTx(ctx, tx, ss, "execution.reconciled", map[string]string{"executionId": executionID, "runId": runID, "outcome": "unknown"}, now)
	})
	if err == nil && reconciledRunID != "" {
		s.stopQuotaLeaseHeartbeat(reconciledRunID)
		s.discardRunUsage(reconciledRunID)
	}
	return err
}
