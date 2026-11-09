package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

func (s *Server) applySupervisionAction(ctx context.Context, decisionID string, lease supervisionLease) (bool, error) {
	handled, err := s.applySupervisionPlanDecision(ctx, decisionID, lease)
	if handled || err != nil {
		return handled, err
	}
	d, err := loadSupervisionDecision(ctx, s.db, decisionID)
	if err != nil {
		return false, err
	}
	var output string
	if err := s.db.QueryRowContext(ctx, `select output from supervision_decisions where id=?`, d.ID).Scan(&output); err != nil {
		return false, err
	}
	p, err := parseSupervisionProposal(output, d)
	if err != nil {
		return false, err
	}
	var done bool
	if err := s.db.QueryRowContext(ctx, `select exists(select 1 from supervision_action_results where decision_id=? and attempt=?)`, d.ID, d.Attempt).Scan(&done); err != nil {
		return true, err
	}
	if done {
		return true, nil
	}
	var a supervisionWorkflowAction
	invalid := decodeSupervisionAction(p.Action, &a)
	if invalid == nil {
		_, err = s.captureSupervisionProjectContext(ctx, d.SessionID, d.ExpectedStateVersion)
		if err != nil {
			return true, err
		}
		if err := s.refreshSupervisionEvidenceEnvironments(ctx, d.SessionID); err != nil {
			return true, err
		}
	}
	err = s.withSupervisionTx(ctx, d.SessionID, time.Now().UTC(), func(tx *sql.Tx, next map[string]WorkspaceAdmission, ss *supervisionSession) error {
		if err := requireSupervisionLease(ss, lease, time.Now().UTC()); err != nil {
			return err
		}
		if ss.StateVersion != d.ExpectedStateVersion || ss.LeaseEpoch != d.LeaseEpoch || ss.Control != "" || ss.RecoveryState != "ready" {
			return errSupervisionState
		}
		latest, err := loadSupervisionDecision(ctx, tx, d.ID)
		if err != nil {
			return err
		}
		if latest.State != "proposed" || latest.ExecutionID != d.ExecutionID {
			return errSupervisionState
		}
		if err := supervisionRunsQuiescentTx(ctx, tx, ss.ID); err != nil {
			return err
		}
		if err := requireSupervisionRoleObservationsTx(ctx, tx, ss.ID); err != nil {
			return err
		}
		if err := verifySupervisionDependenciesTx(ctx, tx, ss.ID); err != nil {
			return err
		}
		admission, ok := next[ss.AdmissionID]
		if !ok || admission.State != "owned" || admission.Epoch != ss.AdmissionEpoch || s.admissionHasOperationsLocked(admission) {
			return errWorkspaceNotQuiescent
		}
		pc, err := loadSupervisionProjectContext(ctx, tx, ss.ID, "current")
		if err != nil {
			return err
		}
		if invalid == nil && a.SessionID != ss.ID {
			invalid = invalidSupervisionWorkflow("动作会话不匹配")
		}
		if invalid == nil {
			invalid = s.validateSupervisionWorkflowTx(ctx, tx, ss, a, pc)
		}
		if invalid != nil && !errors.Is(invalid, errSupervisionPlanInvalid) { // 字段解码错误是领域错误；存储错误须回滚。
			if err := decodeSupervisionAction(p.Action, &supervisionWorkflowAction{}); err == nil {
				return invalid
			}
		}
		state, message := "applied", ""
		if invalid != nil {
			state, message = "rejected", invalid.Error()
			if _, err := tx.ExecContext(ctx, `update supervision_decisions set state='invalid',validation_error=? where id=?;update supervision_work_items set result_state='invalid',result_error=? where execution_id=?`, message, d.ID, message, d.ExecutionID); err != nil {
				return err
			}
			if d.Attempt >= 3 {
				ss.State, ss.Reason, ss.ClockAt = "needs_input", "invalid_scheduler_output", nil
				if errors.Is(invalid, errSupervisionNoProgress) {
					ss.Reason = "no_progress"
				}
				ss.StateVersion++
			}
		} else {
			if _, err := tx.ExecContext(ctx, `update supervision_decisions set state='applied' where id=?`, d.ID); err != nil {
				return err
			}
			if err := s.recordSupervisionWorkflowTx(ctx, tx, ss, d, a, pc, p.Reason); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `insert into supervision_action_results(decision_id,attempt,state,error,created_at) values (?,?,?,?,?)`, d.ID, d.Attempt, state, message, time.Now().UTC()); err != nil {
			return err
		}
		return appendSupervisionEventTx(ctx, tx, ss, "decision.action_"+state, map[string]any{"decisionId": d.ID, "attempt": d.Attempt, "error": message, "action": a.Type}, time.Now().UTC())
	})
	return true, err
}

func (s *Server) validateSupervisionWorkflowTx(ctx context.Context, tx *sql.Tx, ss *supervisionSession, a supervisionWorkflowAction, pc supervisionProjectContext) error {
	if a.Type == "request_review" || a.Type == "dispatch_implementation" {
		if len(a.Paths) > 50 {
			return invalidSupervisionWorkflow("角色附带源码路径超过50项")
		}
		allowed := map[string]bool{}
		for _, entry := range pc.Manifest.Entries {
			if entry.Kind == "file" && entry.Gap == "" && entry.Size <= supervisionSourceBudget {
				allowed[entry.Path] = true
			}
		}
		for _, name := range a.Paths {
			if !allowed[name] {
				return invalidSupervisionWorkflow("角色附带源码路径缺失、敏感或超限: %s", name)
			}
		}
	}
	if a.Type == "read_context" {
		if (len(a.Paths) == 0 && a.TaskID == "" && a.ManifestOffset == 0) || len(a.Paths) > 50 || a.ManifestOffset < 0 || a.ManifestOffset > len(pc.Manifest.Entries) {
			return invalidSupervisionWorkflow("读取需要明确源码路径、任务ID或有效清单分页偏移")
		}
		if a.TaskID != "" {
			var exists bool
			if err := tx.QueryRowContext(ctx, `select exists(select 1 from supervision_tasks where session_id=? and task_id=?)`, ss.ID, a.TaskID).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return invalidSupervisionWorkflow("读取任务不在选中范围")
			}
		}
		allowed := map[string]bool{}
		for _, e := range pc.Manifest.Entries {
			if e.Kind == "file" && e.Gap == "" && e.Size <= supervisionSourceBudget {
				allowed[e.Path] = true
			}
		}
		for _, p := range a.Paths {
			if !allowed[p] {
				return invalidSupervisionWorkflow("源码路径缺失、敏感或超出读取预算: %s", p)
			}
		}
		var repeated bool
		raw, _ := json.Marshal(a.Paths)
		if err := tx.QueryRowContext(ctx, `select exists(select 1 from supervision_source_requests r join supervision_context_window w on w.session_id=r.session_id where r.session_id=? and r.paths=? and w.task_id=? and w.manifest_offset=?)`, ss.ID, string(raw), a.TaskID, a.ManifestOffset).Scan(&repeated); err != nil {
			return err
		}
		if repeated {
			return supervisionNoProgress("本次请求的源码和任务资料已在当前观察中，请使用现有资料推进、读取尚缺的资料或明确交还")
		}
		return nil
	}
	if a.Type == "wait" {
		if !supervisionTextValid(a.Condition, 2000) {
			return invalidSupervisionWorkflow("等待需要明确条件")
		}
		return nil
	}
	if a.Type == "ask_user" && a.TaskID == "" {
		if !supervisionTextValid(a.Question, 8000) {
			return invalidSupervisionWorkflow("问题为空或超限")
		}
		return nil
	}
	var req, plan int64
	var phase string
	if err := tx.QueryRowContext(ctx, `select requirements_revision,plan_revision,phase from supervision_tasks where session_id=? and task_id=?`, ss.ID, a.TaskID).Scan(&req, &plan, &phase); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return invalidSupervisionWorkflow("任务不在选中范围")
		}
		return err
	}
	claim, exists, err := taskExecutionClaim(ctx, tx, a.TaskID)
	if err != nil {
		return err
	}
	if !exists || claim.OwnerKind != "supervision" || claim.OwnerID != ss.ID {
		return invalidSupervisionWorkflow("任务归属不匹配")
	}
	if a.Type == "ask_user" {
		if !supervisionTextValid(a.Question, 8000) {
			return invalidSupervisionWorkflow("问题为空或超限")
		}
		return nil
	}
	if a.Type == "record_task_disposition" {
		if (a.Disposition != "unresolved" && a.Disposition != "deferred") || !supervisionTextValid(a.Instruction, 8000) {
			return invalidSupervisionWorkflow("未完成任务需要明确处理状态和原因")
		}
		return nil
	}
	if req != a.RequirementsRevision || plan != a.PlanRevision || plan < 1 || a.ContextID != pc.ID {
		return invalidSupervisionWorkflow("任务、计划或当前上下文版本不匹配")
	}
	p, err := supervisionTaskPlanTx(ctx, tx, ss.ID, a.TaskID)
	if err != nil {
		return err
	}
	if p.RequirementsRevision != req {
		return invalidSupervisionWorkflow("要求已更新，请先按新要求修订计划")
	}
	var pending bool
	if err := tx.QueryRowContext(ctx, `select exists(select 1 from supervision_amendments where session_id=? and state='pending' and task_id in ('',?)) or exists(select 1 from supervision_questions where session_id=? and task_id in ('',?) and answered_at is null and expired=0)`, ss.ID, a.TaskID, ss.ID, a.TaskID).Scan(&pending); err != nil {
		return err
	}
	if pending {
		return invalidSupervisionWorkflow("任务有待处理的用户要求或问题")
	}
	switch a.Type {
	case "dispatch_implementation":
		if ss.State == "finalizing" {
			return invalidSupervisionWorkflow("最终验收阶段不再扩大实施")
		}
		if p.Status == "review_required" {
			return invalidSupervisionWorkflow("验收修订尚未独立审查")
		}
		if !supervisionTextValid(a.Instruction, 16000) {
			return invalidSupervisionWorkflow("实施需要具体下发内容")
		}
		if p.Plan.RequiresBaseline {
			var captured bool
			if err := tx.QueryRowContext(ctx, `select exists(select 1 from supervision_evidence where session_id=? and task_id=? and kind in ('command','browser') and requirements_revision=? and plan_revision=? and rowid>coalesce((select evidence_rowid from supervision_dependency_cutoff where session_id=?),0) and json_extract(details,'$.check.baseline')=1 and coalesce(json_extract(details,'$.interrupted'),0)=0 and coalesce(json_extract(details,'$.notExecuted'),0)=0 and coalesce(json_extract(details,'$.exitCode'),-1)>=0 and coalesce(json_extract(details,'$.truncated'),0)=0)`, ss.ID, a.TaskID, req, plan, ss.ID).Scan(&captured); err != nil {
				return err
			}
			if !captured {
				return invalidSupervisionWorkflow("本计划需要先取得可比较的实施前基线验证记录")
			}
		}
		if err := supervisionDependenciesReadyTx(ctx, tx, ss.ID, p.Plan.DependsOn, pc.RevisionID); err != nil {
			return err
		}
		var fixes int
		var started bool
		if err := tx.QueryRowContext(ctx, `select fix_rounds,implementation_started from supervision_tasks where session_id=? and task_id=?`, ss.ID, a.TaskID).Scan(&fixes, &started); err != nil {
			return err
		}
		if started && fixes >= ss.Budget.MaxFixRounds {
			return invalidSupervisionWorkflow("该任务修复次数已达预算上限，请保留未完成原因")
		}
	case "request_verification":
		if len(a.Checks) != 1 {
			return invalidSupervisionWorkflow("每个工作项只运行一个检查规格，多个检查应分别派发")
		}
		for _, check := range a.Checks {
			if check.Baseline {
				var started bool
				if err := tx.QueryRowContext(ctx, `select implementation_started from supervision_tasks where session_id=? and task_id=?`, ss.ID, a.TaskID).Scan(&started); err != nil {
					return err
				}
				if started {
					return invalidSupervisionWorkflow("实施开始后不能补造实施前基线验证")
				}
			}
			if err := validateSupervisionCheck(check, p.Plan, pc.Manifest); err != nil {
				return invalidSupervisionWorkflow("%s", err)
			}
		}
		if duplicate, err := supervisionDuplicateCheckTx(ctx, tx, ss, a, pc.RevisionID); err != nil {
			return err
		} else if duplicate {
			return invalidSupervisionWorkflow("同一快照、要求和检查规格已有真实结果，请使用现有证据，不要仅修改检查ID重跑")
		}
	case "request_review":
		if !supervisionTextValid(a.Instruction, 16000) {
			return invalidSupervisionWorkflow("审查需要具体检查内容")
		}
		if duplicate, err := supervisionDuplicateReviewTx(ctx, tx, ss, a, pc); err != nil {
			return err
		} else if duplicate {
			return invalidSupervisionWorkflow("相同任务、源码、验证证据与审查输入已实际审查3次；请补充新资料、修复、处理其他任务或明确交还，不要只改审查措辞重复派发")
		}
	case "record_acceptance":
		if p.Status == "review_required" {
			return invalidSupervisionWorkflow("验收计划尚未通过独立审查")
		}
		if err := supervisionAcceptanceReadyTx(ctx, tx, ss.ID, a.TaskID, pc.RevisionID, req, plan, p.Plan); err != nil {
			return err
		}
	default:
		return invalidSupervisionWorkflow("动作尚不支持或不允许在当前阶段执行")
	}
	if a.Type == "dispatch_implementation" || a.Type == "request_review" {
		class := "work"
		if ss.State == "finalizing" {
			class = "final"
		}
		available, err := supervisionTurnsAvailableTx(ctx, tx, ss, class, true)
		if err != nil {
			return err
		}
		if !available {
			return invalidSupervisionWorkflow("角色调用预算不足")
		}
	}
	return nil
}

func supervisionDependenciesReadyTx(ctx context.Context, tx *sql.Tx, sessionID string, deps []string, revision string) error {
	for _, id := range deps {
		var status string
		if err := tx.QueryRowContext(ctx, `select status from tasks where id=?`, id).Scan(&status); err != nil {
			return err
		}
		if status == taskDone {
			continue
		}
		var ready bool
		if err := tx.QueryRowContext(ctx, `select exists(select 1 from supervision_tasks st join supervision_evidence e on e.session_id=st.session_id and e.task_id=st.task_id where st.session_id=? and st.task_id=? and st.phase='accepted' and st.evidence_freshness='fresh' and e.kind='acceptance' and e.revision_id=? and e.requirements_revision=st.requirements_revision and e.plan_revision=st.plan_revision and e.passed=1)`, sessionID, id, revision).Scan(&ready); err != nil {
			return err
		}
		if !ready {
			return invalidSupervisionWorkflow("前置任务 %s 尚无当前快照的有效候选", id)
		}
	}
	return nil
}

func (s *Server) recordSupervisionWorkflowTx(ctx context.Context, tx *sql.Tx, ss *supervisionSession, d supervisionDecision, a supervisionWorkflowAction, pc supervisionProjectContext, reason string) error {
	now := time.Now().UTC()
	switch a.Type {
	case "read_context":
		raw, _ := json.Marshal(a.Paths)
		if _, err := tx.ExecContext(ctx, `insert into supervision_source_requests(session_id,paths) values (?,?) on conflict(session_id) do update set paths=excluded.paths`, ss.ID, string(raw)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `insert into supervision_context_window(session_id,task_id,manifest_offset) values (?,?,?) on conflict(session_id) do update set task_id=excluded.task_id,manifest_offset=excluded.manifest_offset`, ss.ID, a.TaskID, a.ManifestOffset); err != nil {
			return err
		}
		ss.StateVersion++
		if err := recordSupervisionObservationProgressTx(ctx, tx, ss, pc, a); err != nil {
			return err
		}
	case "wait":
		return recordSupervisionWaitTx(ctx, tx, ss, pc, a.Condition, reason, now)
	case "ask_user":
		var exists bool
		if err := tx.QueryRowContext(ctx, `select exists(select 1 from supervision_questions where session_id=? and task_id=? and question=? and answered_at is null and expired=0)`, ss.ID, a.TaskID, a.Question).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return nil
		}
		requirements, err := supervisionQuestionRequirementsTx(ctx, tx, ss.ID, a.TaskID)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `insert into supervision_questions(id,session_id,task_id,question,created_at,requirements_snapshot) values (?,?,?,?,?,?)`, uuid.NewString(), ss.ID, a.TaskID, a.Question, now, requirements); err != nil {
			return err
		}
		if a.TaskID == "" {
			ss.State, ss.Reason, ss.ClockAt = "needs_input", "user_question", nil
		} else {
			if _, err := tx.ExecContext(ctx, `update supervision_tasks set phase='blocked' where session_id=? and task_id=?`, ss.ID, a.TaskID); err != nil {
				return err
			}
		}
		ss.StateVersion++
	case "record_task_disposition":
		// An explicit diagnosis supersedes the automatic budget deferral. A
		// later budget increase cannot silently undo this scheduling decision.
		if _, err := tx.ExecContext(ctx, `delete from supervision_budget_deferred where session_id=? and task_id=?`, ss.ID, a.TaskID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `update supervision_tasks set phase=?,evidence_freshness='missing' where session_id=? and task_id=?`, a.Disposition, ss.ID, a.TaskID); err != nil {
			return err
		}
		ss.StateVersion++
		if err := appendSupervisionEventTx(ctx, tx, ss, "task.disposition", map[string]string{"taskId": a.TaskID, "disposition": a.Disposition, "reason": a.Instruction}, now); err != nil {
			return err
		}
		return s.maybeFinalizeSupervisionTx(ctx, tx, ss, pc)
	case "record_acceptance":
		if _, err := tx.ExecContext(ctx, `update supervision_tasks set phase='accepted',evidence_freshness='fresh' where session_id=? and task_id=?;update tasks set status='awaiting_review',updated_at=? where id=?`, ss.ID, a.TaskID, now, a.TaskID); err != nil {
			return err
		}
		// 验收证据引用独立审查工作项，不伪造新的执行。
		var executionID string
		if err := tx.QueryRowContext(ctx, `select execution_id from supervision_evidence where session_id=? and task_id=? and kind='review' and revision_id=? and requirements_revision=? and plan_revision=? and passed=1 order by created_at desc limit 1`, ss.ID, a.TaskID, pc.RevisionID, a.RequirementsRevision, a.PlanRevision).Scan(&executionID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `insert into supervision_evidence(id,session_id,task_id,execution_id,kind,revision_id,requirements_revision,plan_revision,passed,details,created_at) values (?,?,?,?,'acceptance',?,?,?,1,?,?)`, uuid.NewString(), ss.ID, a.TaskID, executionID, pc.RevisionID, a.RequirementsRevision, a.PlanRevision, string(mustJSON(map[string]string{"reason": reason})), now); err != nil {
			return err
		}
		ss.StateVersion++
		if err := appendSupervisionEventTx(ctx, tx, ss, "task.candidate_accepted", map[string]string{"taskId": a.TaskID, "revisionId": pc.RevisionID}, now); err != nil {
			return err
		}
		return s.maybeFinalizeSupervisionTx(ctx, tx, ss, pc)
	default:
		return s.queueSupervisionRoleTx(ctx, tx, ss, d, a, pc)
	}
	return appendSupervisionEventTx(ctx, tx, ss, "scheduler."+a.Type, map[string]any{"taskId": a.TaskID, "reason": reason}, now)
}

func (s *Server) queueSupervisionRoleTx(ctx context.Context, tx *sql.Tx, ss *supervisionSession, parent supervisionDecision, a supervisionWorkflowAction, pc supervisionProjectContext) error {
	kind, role := "implementation", "implementation"
	if a.Type == "request_review" {
		kind, role = "review", "review"
	}
	if a.Type == "request_verification" {
		kind, role = "verification", "test"
	}
	class := "work"
	if ss.State == "finalizing" {
		class = "final"
	}
	ss.StateVersion++
	d := supervisionDecision{ID: uuid.NewString(), SessionID: ss.ID, ExpectedStateVersion: ss.StateVersion, BasisSeq: ss.EventSeq, LeaseEpoch: ss.LeaseEpoch, WorkItemID: uuid.NewString(), ExecutionID: uuid.NewString(), Attempt: 1, State: "pending"}
	now := time.Now().UTC()
	turn := "reserved"
	if kind == "verification" {
		turn = "released"
	} else {
		ss.ReservedTurns++
	}
	if _, err := tx.ExecContext(ctx, `insert into supervision_decisions(id,session_id,expected_state_version,basis_seq,lease_epoch,state,created_at) values (?,?,?,?,?,'pending',?)`, d.ID, ss.ID, d.ExpectedStateVersion, d.BasisSeq, d.LeaseEpoch, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `insert into supervision_work_items(id,session_id,decision_id,task_id,role,attempt,execution_id,state,lease_epoch,admission_epoch,budget_class,turn_state,created_at) values (?,?,?,?,?,1,?,'queued',?,?,?,?,?)`, d.WorkItemID, ss.ID, d.ID, a.TaskID, role, d.ExecutionID, ss.LeaseEpoch, ss.AdmissionEpoch, class, turn, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `insert into supervision_outbox(execution_id,session_id,state,lease_epoch,created_at,updated_at) values (?,?,'pending',?,?,?)`, d.ExecutionID, ss.ID, ss.LeaseEpoch, now, now); err != nil {
		return err
	}
	spec, _ := json.Marshal(a)
	if _, err := tx.ExecContext(ctx, `insert into supervision_role_jobs(execution_id,session_id,task_id,kind,requirements_revision,plan_revision,input_revision,spec,state,created_at) values (?,?,?,?,?,?,?,?,'queued',?)`, d.ExecutionID, ss.ID, a.TaskID, kind, a.RequirementsRevision, a.PlanRevision, pc.RevisionID, string(spec), now); err != nil {
		return err
	}
	if err := saveSupervisionRoleInputTx(ctx, tx, ss, d, pc); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `update supervision_tasks set phase=? where session_id=? and task_id=?`, map[string]string{"implementation": "implementing", "verification": "verifying", "review": "reviewing"}[kind], ss.ID, a.TaskID); err != nil {
		return err
	}
	return appendSupervisionEventTx(ctx, tx, ss, "work_item.dispatched", map[string]any{"parentDecisionId": parent.ID, "workItemId": d.WorkItemID, "executionId": d.ExecutionID, "taskId": a.TaskID, "role": role, "instruction": a.Instruction, "checks": a.Checks}, now)
}

func (s *Server) maybeFinalizeSupervisionTx(ctx context.Context, tx *sql.Tx, ss *supervisionSession, pc supervisionProjectContext) error {
	// Deferred instructions let the current turn finish, but must be applied
	// before the session can finalize. A disposition based on older facts may
	// otherwise abandon acknowledged requirements (including other tasks).
	var pending bool
	if err := tx.QueryRowContext(ctx, `select exists(select 1 from supervision_amendments where session_id=? and state='pending')`, ss.ID).Scan(&pending); err != nil {
		return err
	}
	if pending {
		return nil
	}
	var remaining, issues, accepted int
	if err := tx.QueryRowContext(ctx, `select sum(case when phase not in ('accepted','unresolved','deferred') then 1 else 0 end),sum(case when phase in ('unresolved','deferred') then 1 else 0 end),sum(case when phase='accepted' then 1 else 0 end) from supervision_tasks where session_id=?`, ss.ID).Scan(&remaining, &issues, &accepted); err != nil {
		return err
	}
	if remaining > 0 {
		return nil
	}
	plans, err := loadSupervisionPlansTx(ctx, tx, ss.ID)
	if err != nil {
		return err
	}
	for _, p := range plans {
		var phase string
		if err := tx.QueryRowContext(ctx, `select phase from supervision_tasks where session_id=? and task_id=?`, ss.ID, p.TaskID).Scan(&phase); err != nil {
			return err
		}
		if phase != "accepted" {
			continue
		}
		if err := supervisionAcceptanceReadyTx(ctx, tx, ss.ID, p.TaskID, pc.RevisionID, p.RequirementsRevision, p.Revision, p.Plan); err != nil {
			if !errors.Is(err, errSupervisionPlanInvalid) {
				return err
			}
			ss.State = "finalizing"
			ss.StateVersion++
			return appendSupervisionEventTx(ctx, tx, ss, "session.finalizing", map[string]string{"reason": err.Error()}, time.Now().UTC())
		}
		if err := supervisionDependenciesReadyTx(ctx, tx, ss.ID, p.Plan.DependsOn, pc.RevisionID); err != nil {
			if !errors.Is(err, errSupervisionPlanInvalid) {
				return err
			}
			ss.State = "finalizing"
			ss.StateVersion++
			return nil
		}
	}
	// 清理/目录释放在终态后由维护循环完成，正式 done 与报告同事务提交。
	if _, err := loadSupervisionTargetControl(ctx, tx, ss.ID); err == nil {
		// The worker performs the network handoff outside this transaction.
		// Only its receipt-consumption transaction may publish target Task.done.
		if ss.State != "finalizing" || ss.Reason != "target_finalization" {
			ss.State, ss.Reason = "finalizing", "target_finalization"
			ss.StateVersion++
			return appendSupervisionEventTx(ctx, tx, ss, "session.finalizing", map[string]string{"reason": ss.Reason}, time.Now().UTC())
		}
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return s.commitSupervisionTasksTx(ctx, tx, ss, pc, accepted, issues)
}

// The caller has validated acceptance and, for a target, consumed its sealed
// finalization receipt in this same transaction. This function performs no I/O.
func (s *Server) commitSupervisionTasksTx(ctx context.Context, tx *sql.Tx, ss *supervisionSession, pc supervisionProjectContext, accepted, issues int) error {
	now := time.Now().UTC()
	ids, err := supervisionTaskIDsTx(ctx, tx, ss.ID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		claim, exists, err := taskExecutionClaim(ctx, tx, id)
		if err != nil {
			return err
		}
		if !exists || claim.OwnerKind != "supervision" || claim.OwnerID != ss.ID {
			return errTaskExecutionClaimed
		}
		if err := s.recordTaskEventTx(ctx, tx, id, "", "supervision.finalized", map[string]string{"sessionId": ss.ID, "revisionId": pc.RevisionID}, now); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `update tasks set status=case when id in (select task_id from supervision_tasks where session_id=? and phase='accepted') then 'done' when id in (select task_id from supervision_tasks where session_id=? and implementation_started=1) then 'action_required' else status end,completed_at=case when id in (select task_id from supervision_tasks where session_id=? and phase='accepted') then ? else completed_at end,updated_at=? where id in (select task_id from supervision_tasks where session_id=?)`, ss.ID, ss.ID, ss.ID, now, now, ss.ID); err != nil {
		return err
	}
	ss.State = "completed"
	if issues > 0 {
		ss.State = "completed_with_issues"
	}
	if accepted == 0 {
		ss.State = "failed"
	}
	ss.Reason = ""
	ss.ClockAt = nil
	ss.StateVersion++
	return appendSupervisionEventTx(ctx, tx, ss, "session."+ss.State, map[string]any{"revisionId": pc.RevisionID, "accepted": accepted, "issues": issues}, now)
}
