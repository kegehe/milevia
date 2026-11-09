package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
)

type supervisionLease struct {
	Owner string
	Epoch int64
}

type supervisionDecision struct {
	ID                   string `json:"id"`
	SessionID            string `json:"sessionId"`
	ExpectedStateVersion int64  `json:"expectedStateVersion"`
	BasisSeq             int64  `json:"basisSeq"`
	LeaseEpoch           int64  `json:"leaseEpoch"`
	State                string `json:"state"`
	WorkItemID           string `json:"workItemId"`
	ExecutionID          string `json:"executionId"`
	Attempt              int    `json:"attempt"`
}

func requireSupervisionLease(ss *supervisionSession, lease supervisionLease, now time.Time) error {
	if ss.LeaseOwner != lease.Owner || lease.Owner == "" || ss.LeaseEpoch != lease.Epoch || ss.LeaseExpiresAt == nil || !now.Before(*ss.LeaseExpiresAt) {
		return errSupervisionLease
	}
	return nil
}

func (s *Server) acquireSupervisionLease(ctx context.Context, id, owner string, now time.Time) (supervisionLease, error) {
	if strings.TrimSpace(owner) == "" {
		return supervisionLease{}, errSupervisionLease
	}
	var lease supervisionLease
	err := s.withSupervisionTx(ctx, id, now, func(tx *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
		if (ss.State != "running" && ss.State != "finalizing") || ss.Control != "" || ss.RecoveryState != "ready" {
			return errSupervisionState
		}
		if err := requireSupervisionNoPendingDrift(ctx, tx, id); err != nil {
			return err
		}
		alive := ss.LeaseExpiresAt != nil && now.Before(*ss.LeaseExpiresAt)
		if alive && ss.LeaseOwner != owner {
			return errSupervisionLease
		}
		if !alive && ss.LeaseOwner != owner {
			active, err := supervisionHasExecutionTx(ctx, tx, id)
			if err != nil {
				return err
			}
			if active {
				return errWorkspaceNotQuiescent
			}
			if err := suspendSupervisionQueuedTx(ctx, tx, ss, false, now); err != nil {
				return err
			}
			ss.LeaseEpoch++
		}
		ss.LeaseOwner = owner
		expires := now.Add(30 * time.Second)
		ss.LeaseExpiresAt = &expires
		lease = supervisionLease{Owner: owner, Epoch: ss.LeaseEpoch}
		return appendSupervisionEventTx(ctx, tx, ss, "scheduler.lease", map[string]int64{"epoch": ss.LeaseEpoch}, now)
	})
	return lease, err
}

func loadSupervisionDecision(ctx context.Context, q sqlQueryer, id string) (supervisionDecision, error) {
	var d supervisionDecision
	err := q.QueryRowContext(ctx, `select d.id,d.session_id,d.expected_state_version,d.basis_seq,d.lease_epoch,d.state,w.id,w.execution_id,w.attempt from supervision_decisions d join supervision_work_items w on w.decision_id=d.id where d.id=? order by w.attempt desc limit 1`, id).
		Scan(&d.ID, &d.SessionID, &d.ExpectedStateVersion, &d.BasisSeq, &d.LeaseEpoch, &d.State, &d.WorkItemID, &d.ExecutionID, &d.Attempt)
	return d, err
}

// 同版本重复唤醒返回原决策。retryID 只用于同一决策最多两次格式纠正，不是无限重调度。
func (s *Server) prepareSupervisionDecision(ctx context.Context, id string, version int64, lease supervisionLease, retryID string, now time.Time) (supervisionDecision, error) {
	var decision supervisionDecision
	var denied error
	err := s.withSupervisionTx(ctx, id, now, func(tx *sql.Tx, next map[string]WorkspaceAdmission, ss *supervisionSession) error {
		if err := requireSupervisionVersion(ss, version); err != nil {
			return err
		}
		if err := requireSupervisionLease(ss, lease, now); err != nil {
			return err
		}
		if (ss.State != "running" && ss.State != "finalizing") || ss.Control != "" || ss.RecoveryState != "ready" {
			return errSupervisionState
		}
		if err := requireSupervisionRoleObservationsTx(ctx, tx, id); err != nil {
			return err
		}
		a, exists := next[ss.AdmissionID]
		if !exists || a.OwnerID != id || a.State != "owned" || a.Epoch != ss.AdmissionEpoch || a.RecoveryState != "ready" {
			return errWorkspaceAdmissionStale
		}
		var activeID string
		err := tx.QueryRowContext(ctx, `select id from supervision_decisions where session_id=? and state in ('pending','running','proposed')`, id).Scan(&activeID)
		if err == nil {
			decision, err = loadSupervisionDecision(ctx, tx, activeID)
			if err != nil {
				return err
			}
			if retryID != "" || decision.ExpectedStateVersion != ss.StateVersion || decision.LeaseEpoch != lease.Epoch {
				return errSupervisionState
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if retryID == "" {
			var existingID string
			err := tx.QueryRowContext(ctx, `select id from supervision_decisions where session_id=? and expected_state_version=? and lease_epoch=? order by created_at desc,id desc limit 1`, id, ss.StateVersion, lease.Epoch).Scan(&existingID)
			if err == nil {
				decision, err = loadSupervisionDecision(ctx, tx, existingID)
				return err
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		if err := supervisionRunsQuiescentTx(ctx, tx, id); err != nil {
			return err
		}
		attempt := 1
		decisionID := uuid.NewString()
		if retryID != "" {
			previous, err := loadSupervisionDecision(ctx, tx, retryID)
			if err != nil {
				return err
			}
			if previous.SessionID != id || previous.State != "invalid" || previous.ExpectedStateVersion != ss.StateVersion || previous.LeaseEpoch != lease.Epoch {
				return errSupervisionState
			}
			if previous.Attempt >= 3 {
				return errors.New("调度结果已纠正两次，需要人工处理")
			}
			decisionID, attempt = retryID, previous.Attempt+1
		}
		class := "work"
		if ss.State == "finalizing" {
			class = "final"
		}
		available, err := supervisionTurnsAvailableTx(ctx, tx, ss, class, true)
		if err != nil {
			return err
		}
		if !available {
			denied = errSupervisionBudget
			return pauseSupervisionForBudgetTx(ctx, tx, ss, now)
		}
		decision = supervisionDecision{ID: decisionID, SessionID: id, ExpectedStateVersion: ss.StateVersion, BasisSeq: ss.EventSeq, LeaseEpoch: lease.Epoch, State: "pending", WorkItemID: uuid.NewString(), ExecutionID: uuid.NewString(), Attempt: attempt}
		if retryID == "" {
			if _, err := tx.ExecContext(ctx, `insert into supervision_decisions(id,session_id,expected_state_version,basis_seq,lease_epoch,state,created_at) values (?,?,?,?,?,'pending',?)`, decision.ID, id, decision.ExpectedStateVersion, decision.BasisSeq, lease.Epoch, now); err != nil {
				return err
			}
		} else {
			if _, err := tx.ExecContext(ctx, `update supervision_decisions set state='pending',basis_seq=?,output='',validation_error='',completed_at=null where id=?`, decision.BasisSeq, decision.ID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `insert into supervision_work_items(id,session_id,decision_id,role,attempt,execution_id,state,lease_epoch,admission_epoch,budget_class,turn_state,created_at) values (?,?,?,'scheduler',?,?,'queued',?,?,?,'reserved',?)`, decision.WorkItemID, id, decision.ID, attempt, decision.ExecutionID, lease.Epoch, a.Epoch, class, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `insert into supervision_outbox(execution_id,session_id,state,lease_epoch,created_at,updated_at) values (?,?,'pending',?,?,?)`, decision.ExecutionID, id, lease.Epoch, now, now); err != nil {
			return err
		}
		ss.ReservedTurns++
		if err := saveSupervisionSchedulerInputTx(ctx, tx, ss, decision, now); err != nil {
			return err
		}
		return appendSupervisionEventTx(ctx, tx, ss, "decision.requested", decision, now)
	})
	if err != nil {
		return supervisionDecision{}, err
	}
	if denied != nil {
		return supervisionDecision{}, denied
	}
	return decision, nil
}

// 在提交 launching 后才可调用执行适配器。这里只返回握手事实，不直接运行 CLI。
// 返回的 deadline 是总预算、实施份额及单轮上限的最小值，执行适配器必须沿用。
func (s *Server) claimSupervisionDecisionLaunch(ctx context.Context, decisionID string, lease supervisionLease, now time.Time) (time.Time, error) {
	decision, err := loadSupervisionDecision(ctx, s.db, decisionID)
	if err != nil {
		return time.Time{}, err
	}
	var deadline time.Time
	var denied error
	err = s.withSupervisionTx(ctx, decision.SessionID, now, func(tx *sql.Tx, next map[string]WorkspaceAdmission, ss *supervisionSession) error {
		if err := requireSupervisionLease(ss, lease, now); err != nil {
			return err
		}
		d, err := loadSupervisionDecision(ctx, tx, decisionID)
		if err != nil {
			return err
		}
		if d.LeaseEpoch != lease.Epoch || d.ExpectedStateVersion != ss.StateVersion || d.State != "pending" || ss.Control != "" || ss.RecoveryState != "ready" || (ss.State != "running" && ss.State != "finalizing") {
			return errSupervisionState
		}
		if err := requireSupervisionNoPendingDrift(ctx, tx, ss.ID); err != nil {
			return err
		}
		a, exists := next[ss.AdmissionID]
		if !exists || a.OwnerID != ss.ID || a.State != "owned" || a.Epoch != ss.AdmissionEpoch || a.RecoveryState != "ready" {
			return errWorkspaceAdmissionStale
		}
		if s.admissionHasOperationsLocked(a) {
			return errWorkspaceNotQuiescent
		}
		if _, err := s.supervisionWorkspaceSourceTx(ctx, tx, ss, a); err != nil {
			return err
		}
		if err := supervisionRunsQuiescentTx(ctx, tx, ss.ID); err != nil {
			return err
		}
		var class, state, turnState string
		var epoch int64
		if err := tx.QueryRowContext(ctx, `select budget_class,state,turn_state,admission_epoch from supervision_work_items where id=?`, d.WorkItemID).Scan(&class, &state, &turnState, &epoch); err != nil {
			return err
		}
		var verification bool
		if err := tx.QueryRowContext(ctx, `select exists(select 1 from supervision_role_jobs where execution_id=? and kind='verification')`, d.ExecutionID).Scan(&verification); err != nil {
			return err
		}
		if state != "queued" || (!verification && (turnState != "reserved" || ss.ReservedTurns < 1)) || epoch != a.Epoch {
			return errSupervisionState
		}
		available, err := supervisionTurnsAvailableTx(ctx, tx, ss, class, false)
		if err != nil {
			return err
		}
		if !available {
			denied = errSupervisionBudget
			return pauseSupervisionForBudgetTx(ctx, tx, ss, now)
		}
		nextTurn := "consumed"
		if verification {
			nextTurn = "released"
		}
		if _, err := tx.ExecContext(ctx, `update supervision_work_items set state='launching',turn_state=?,started_at=? where id=?`, nextTurn, now, d.WorkItemID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `update supervision_outbox set state='launching',updated_at=? where execution_id=?`, now, d.ExecutionID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `update supervision_decisions set state='running' where id=?`, d.ID); err != nil {
			return err
		}
		if !verification {
			ss.ReservedTurns--
			ss.UsedTurns++
		}
		if _, err := tx.ExecContext(ctx, `update supervision_tasks set fix_rounds=fix_rounds+implementation_started,implementation_started=1 where session_id=? and task_id=(select task_id from supervision_work_items where id=? and role='implementation');update tasks set status='running',updated_at=? where id=(select task_id from supervision_work_items where id=? and role='implementation')`, ss.ID, d.WorkItemID, now, d.WorkItemID); err != nil {
			return err
		}
		remaining := supervisionTimeAvailable(ss, class)
		if remaining > ss.Budget.MaxTurnMillis {
			remaining = ss.Budget.MaxTurnMillis
		}
		deadline = now.Add(time.Duration(remaining) * time.Millisecond)
		return appendSupervisionEventTx(ctx, tx, ss, "work_item.launching", map[string]any{"workItemId": d.WorkItemID, "executionId": d.ExecutionID, "attempt": d.Attempt, "deadline": deadline}, now)
	})
	if err != nil {
		return time.Time{}, err
	}
	if denied != nil {
		return time.Time{}, denied
	}
	return deadline, nil
}

// 启动适配器保存 Run 后回到同一个 gate 登记。stop 已提交时仍保存已启动事实，
// 并返回 cancelRequired；不能因停止而丢掉一个可能正在运行的进程关联。
func (s *Server) bindSupervisionDecisionRun(ctx context.Context, decisionID, executionID, conversationID, runID string, lease supervisionLease, now time.Time) (bool, error) {
	d, err := loadSupervisionDecision(ctx, s.db, decisionID)
	if err != nil {
		return false, err
	}
	cancelRequired := false
	err = s.withSupervisionTx(ctx, d.SessionID, now, func(tx *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
		// 回调不要求 lease 仍在时间窗内，但代际与所有者必须仍匹配。
		// 时间窗过期后不能新派发；已启动事实仍须保存供停止及恢复核对。
		if lease.Owner == "" || lease.Owner != ss.LeaseOwner || lease.Epoch != ss.LeaseEpoch || ss.RecoveryState != "ready" {
			return errSupervisionLease
		}
		var workItemID, state string
		var attempt int
		var itemEpoch int64
		if err := tx.QueryRowContext(ctx, `select id,state,attempt,lease_epoch from supervision_work_items where execution_id=? and decision_id=? and session_id=?`, executionID, decisionID, ss.ID).Scan(&workItemID, &state, &attempt, &itemEpoch); err != nil {
			return err
		}
		if (state != "launching" && state != "running") || itemEpoch != lease.Epoch {
			return errSupervisionState
		}
		if err := linkSupervisionRunTx(ctx, tx, supervisionRunOwnership{SessionID: ss.ID, WorkItemID: workItemID, Role: "scheduler", Attempt: attempt, AdmissionID: ss.AdmissionID, Epoch: ss.AdmissionEpoch, RunID: runID, ConversationID: conversationID}); err != nil {
			return err
		}
		cancelRequired = ss.Control == "stop" || ss.Reason == "budget_exhausted"
		if state == "running" {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `update supervision_work_items set state='running' where id=?`, workItemID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `update supervision_run_links set process_state='running' where run_id=?`, runID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `update supervision_outbox set state='delivered',updated_at=? where execution_id=?`, now, executionID); err != nil {
			return err
		}
		return appendSupervisionEventTx(ctx, tx, ss, "work_item.run_bound", map[string]any{"workItemId": workItemID, "executionId": executionID, "runId": runID, "conversationId": conversationID, "cancelRequired": cancelRequired}, now)
	})
	return cancelRequired, err
}

type supervisionProposal struct {
	SchemaVersion        int             `json:"schemaVersion"`
	DecisionID           string          `json:"decisionId"`
	ExpectedStateVersion int64           `json:"expectedStateVersion"`
	BasisSeq             int64           `json:"basisSeq"`
	Reason               string          `json:"reason"`
	Action               json.RawMessage `json:"action"`
}

// 这里只校验建议封套；B2 领域服务须再次核对任务、计划、依赖与证据后才能应用动作。
func parseSupervisionProposal(raw string, d supervisionDecision) (supervisionProposal, error) {
	var proposal supervisionProposal
	if len(raw) > 256*1024 {
		return proposal, errors.New("调度输出超出 256 KiB")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&proposal); err != nil {
		return proposal, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return proposal, errors.New("调度输出只能包含一个 JSON 对象")
	}
	if proposal.SchemaVersion != 1 || proposal.DecisionID != d.ID || proposal.ExpectedStateVersion != d.ExpectedStateVersion || proposal.BasisSeq != d.BasisSeq || strings.TrimSpace(proposal.Reason) == "" {
		return proposal, errors.New("调度输出的版本、身份、事件依据或理由不匹配")
	}
	var action map[string]json.RawMessage
	if err := json.Unmarshal(proposal.Action, &action); err != nil || len(action) == 0 {
		return proposal, errors.New("调度动作必须是一个对象")
	}
	var typ, sessionID string
	if err := json.Unmarshal(action["type"], &typ); err != nil {
		return proposal, err
	}
	if err := json.Unmarshal(action["sessionId"], &sessionID); err != nil || sessionID != d.SessionID {
		return proposal, errors.New("调度动作不属于当前托管会话")
	}
	switch typ {
	case "read_context", "record_plan", "dispatch_implementation", "request_verification", "request_review", "record_acceptance", "record_task_disposition", "cancel_work_item", "wait", "ask_user":
	default:
		return proposal, errors.New("调度输出包含未知动作")
	}
	// JSON 重复键不能用“最后一个值覆盖”解释权限或动作身份。
	if err := rejectDuplicateSupervisionJSONKeys(json.NewDecoder(bytes.NewBufferString(raw)), 0); err != nil {
		return proposal, err
	}
	return proposal, nil
}

func rejectDuplicateSupervisionJSONKeys(decoder *json.Decoder, depth int) error {
	if depth > 64 {
		return errors.New("调度 JSON 嵌套过深")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	if delim != '{' && delim != '[' {
		return errors.New("无效的调度 JSON")
	}
	seen := map[string]bool{}
	for decoder.More() {
		if delim == '{' {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return errors.New("调度 JSON 包含重复键")
			}
			if depth == 0 {
				switch name {
				case "schemaVersion", "decisionId", "expectedStateVersion", "basisSeq", "reason", "action":
				default:
					return errors.New("调度输出包含非规范字段名")
				}
			}
			seen[name] = true
		}
		if err := rejectDuplicateSupervisionJSONKeys(decoder, depth+1); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}

// 保存建议不意味着进程已退出，也不意味着业务动作已获准。迟到结果仅作为历史。
func (s *Server) recordSupervisionDecisionOutput(ctx context.Context, decisionID, executionID, raw string, now time.Time) (string, error) {
	d, err := loadSupervisionDecision(ctx, s.db, decisionID)
	if err != nil {
		return "", err
	}
	disposition := ""
	err = s.withSupervisionTx(ctx, d.SessionID, now, func(tx *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
		var state, existing string
		var workItemID string
		var attempt int
		if err := tx.QueryRowContext(ctx, `select id,state,result_text,attempt from supervision_work_items where execution_id=? and decision_id=? and session_id=?`, executionID, decisionID, ss.ID).Scan(&workItemID, &state, &existing, &attempt); err != nil {
			return err
		}
		var received bool
		if err := tx.QueryRowContext(ctx, `select result_received_at is not null from supervision_work_items where id=?`, workItemID).Scan(&received); err != nil {
			return err
		}
		truncated := len(raw) > 256*1024
		if truncated {
			raw = raw[:256*1024]
		}
		if received {
			if existing != raw {
				return errors.New("同一次调度执行不能覆盖已有输出")
			}
			if err := tx.QueryRowContext(ctx, `select result_state from supervision_work_items where id=?`, workItemID).Scan(&disposition); err != nil {
				return err
			}
			return nil
		}
		if state != "launching" && state != "running" && state != "unknown" && state != "quiescent" {
			return errSupervisionState
		}
		latest, err := loadSupervisionDecision(ctx, tx, decisionID)
		if err != nil {
			return err
		}
		_, parseErr := parseSupervisionProposal(raw, latest)
		if truncated {
			parseErr = errors.New("调度输出超出 256 KiB，已截断，不能执行")
		}
		disposition = "proposed"
		validationError := ""
		if parseErr != nil {
			disposition = "invalid"
			validationError = parseErr.Error()
		}
		if ss.StateVersion != latest.ExpectedStateVersion || ss.LeaseEpoch != latest.LeaseEpoch || ss.LeaseExpiresAt == nil || !now.Before(*ss.LeaseExpiresAt) || ss.Control != "" || ss.RecoveryState != "ready" || latest.State == "stale" || latest.State == "cancelled" || attempt != latest.Attempt || supervisionTerminal(ss.State) {
			disposition = "stale"
		}
		if _, err := tx.ExecContext(ctx, `update supervision_work_items set result_text=?,result_state=?,result_error=?,result_received_at=? where id=?`, raw, disposition, validationError, now, workItemID); err != nil {
			return err
		}
		if attempt == latest.Attempt {
			if _, err := tx.ExecContext(ctx, `update supervision_decisions set state=?,output=?,validation_error=?,completed_at=? where id=?`, disposition, raw, validationError, now, decisionID); err != nil {
				return err
			}
		}
		if state == "quiescent" && disposition == "invalid" && attempt >= 3 && ss.Control == "" {
			ss.State, ss.Reason, ss.ClockAt = "needs_input", "invalid_scheduler_output", nil
			ss.StateVersion++
			if err := appendSupervisionEventTx(ctx, tx, ss, "scheduler.failed", map[string]string{"reason": ss.Reason}, now); err != nil {
				return err
			}
		}
		return appendSupervisionEventTx(ctx, tx, ss, "decision.result", map[string]any{"decisionId": decisionID, "executionId": executionID, "attempt": attempt, "state": disposition, "validationError": validationError, "truncated": truncated}, now)
	})
	return disposition, err
}

// 只能由进程等待/启动失败核对回调调用，不能由模型文本或普通 Run.completed 调用。
func (s *Server) confirmSupervisionDecisionQuiescent(ctx context.Context, decisionID, executionID string, epoch int64, now time.Time) error {
	d, err := loadSupervisionDecision(ctx, s.db, decisionID)
	if err != nil {
		return err
	}
	return s.withSupervisionTx(ctx, d.SessionID, now, func(tx *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
		var state, workItemID string
		var itemEpoch int64
		if err := tx.QueryRowContext(ctx, `select id,state,lease_epoch from supervision_work_items where execution_id=? and decision_id=?`, executionID, decisionID).Scan(&workItemID, &state, &itemEpoch); err != nil {
			return err
		}
		if epoch != itemEpoch || epoch != ss.LeaseEpoch || ss.RecoveryState != "ready" {
			return errSupervisionLease
		}
		if state == "quiescent" {
			return nil
		}
		if state != "launching" && state != "running" {
			return errWorkspaceNotQuiescent
		}
		var executionActive bool
		if err := tx.QueryRowContext(ctx, `select exists(select 1 from supervision_executions where execution_id=? and state<>'quiescent')`, executionID).Scan(&executionActive); err != nil {
			return err
		}
		if executionActive {
			return errWorkspaceNotQuiescent
		}
		var linkedActive bool
		if err := tx.QueryRowContext(ctx, `select exists(select 1 from supervision_run_links l join runs r on r.id=l.run_id where l.work_item_id=? and (l.process_state<>'quiescent' or r.status in ('queued','running')))`, workItemID).Scan(&linkedActive); err != nil {
			return err
		}
		if linkedActive {
			return errWorkspaceNotQuiescent
		}
		if _, err := tx.ExecContext(ctx, `update supervision_work_items set state='quiescent',finished_at=? where id=?`, now, workItemID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `update supervision_outbox set state='completed',updated_at=? where execution_id=?`, now, executionID); err != nil {
			return err
		}
		var resultState string
		var attempt int
		if err := tx.QueryRowContext(ctx, `select result_state,attempt from supervision_work_items where id=?`, workItemID).Scan(&resultState, &attempt); err != nil {
			return err
		}
		if resultState == "invalid" && attempt >= 3 && ss.Control == "" {
			ss.State, ss.Reason, ss.ClockAt = "needs_input", "invalid_scheduler_output", nil
			ss.StateVersion++
			if err := appendSupervisionEventTx(ctx, tx, ss, "scheduler.failed", map[string]string{"reason": ss.Reason}, now); err != nil {
				return err
			}
		}
		return appendSupervisionEventTx(ctx, tx, ss, "work_item.quiescent", map[string]string{"workItemId": workItemID, "executionId": executionID}, now)
	})
}
