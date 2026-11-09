package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

var errSupervisionNoProgress = errors.New("调度未产生进展")

func supervisionNoProgress(message string) error {
	return fmt.Errorf("%w: %w: %s", errSupervisionPlanInvalid, errSupervisionNoProgress, message)
}

// Older versions could commit a no-op read/plan as applied without advancing
// state_version. Reclassify only that exact stalled case, never a productive
// action, an explicit wait, or an already-controlled session.
func (s *Server) correctSupervisionStalledDecision(ctx context.Context, id, decisionID string) (bool, error) {
	corrected := false
	err := s.withSupervisionTx(ctx, id, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
		if ss.Control != "" || ss.RecoveryState != "ready" || (ss.State != "running" && ss.State != "finalizing") {
			return nil
		}
		d, err := loadSupervisionDecision(ctx, tx, decisionID)
		if err != nil {
			return err
		}
		if d.SessionID != id || d.State != "applied" || d.ExpectedStateVersion != ss.StateVersion {
			return nil
		}
		var raw string
		if err := tx.QueryRowContext(ctx, `select output from supervision_decisions where id=?`, d.ID).Scan(&raw); err != nil {
			return err
		}
		var proposal struct {
			Action struct {
				Type string `json:"type"`
			} `json:"action"`
		}
		if json.Unmarshal([]byte(raw), &proposal) != nil || (proposal.Action.Type != "read_context" && proposal.Action.Type != "record_plan") {
			return nil
		}
		if err := supervisionRunsQuiescentTx(ctx, tx, id); err != nil {
			return err
		}
		message := supervisionNoProgress("上一轮重复读取或重复计划没有新增事实，请推进或明确交还").Error()
		if _, err := tx.ExecContext(ctx, `update supervision_decisions set state='invalid',validation_error=? where id=?;update supervision_work_items set result_state='invalid',result_error=? where execution_id=?`, message, d.ID, message, d.ExecutionID); err != nil {
			return err
		}
		if d.Attempt >= 3 {
			ss.State, ss.Reason, ss.ClockAt = "needs_input", "no_progress", nil
			ss.StateVersion++
		}
		corrected = true
		return appendSupervisionEventTx(ctx, tx, ss, "scheduler.stalled_action_corrected", map[string]string{"decisionId": d.ID}, time.Now().UTC())
	})
	return corrected, err
}

// 外部前置的实时状态独立于启动快照。事实改变会唤醒等待中的调度并使旧证据过期。
func (s *Server) refreshSupervisionDependencies(ctx context.Context, id string) error {
	return s.withSupervisionTx(ctx, id, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
		if ss.Control != "" || ss.RecoveryState != "ready" || (ss.State != "running" && ss.State != "finalizing") {
			return nil
		}
		active, err := supervisionHasExecutionTx(ctx, tx, id)
		if err != nil || active {
			return err
		}
		facts, err := supervisionExternalDependencyFactsTx(ctx, tx, id)
		if err != nil {
			return err
		}
		raw := mustJSON(facts)
		hash := supervisionSHA256(raw)
		var previous string
		err = tx.QueryRowContext(ctx, `select fingerprint from supervision_dependency_state where session_id=?`, id).Scan(&previous)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if previous == hash {
			return nil
		}
		now := time.Now().UTC()
		if _, err := tx.ExecContext(ctx, `insert into supervision_dependency_state(session_id,fingerprint,facts,changed_at) values (?,?,?,?) on conflict(session_id) do update set fingerprint=excluded.fingerprint,facts=excluded.facts,changed_at=excluded.changed_at`, id, hash, string(raw), now); err != nil {
			return err
		}
		if previous == "" {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `insert into supervision_dependency_cutoff(session_id,evidence_rowid) select ?,coalesce(max(rowid),0) from supervision_evidence where session_id=? on conflict(session_id) do update set evidence_rowid=excluded.evidence_rowid`, id, id); err != nil {
			return err
		}
		if err := suspendSupervisionQueuedTx(ctx, tx, ss, false, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `update supervision_tasks set evidence_freshness='stale' where session_id=? and evidence_freshness='fresh'`, id); err != nil {
			return err
		}
		ss.StateVersion++
		return appendSupervisionEventTx(ctx, tx, ss, "dependencies.changed", facts, now)
	})
}

func supervisionExternalDependencyFactsTx(ctx context.Context, tx *sql.Tx, id string) ([]map[string]any, error) {
	rows, err := tx.QueryContext(ctx, `select distinct t.id,t.status,t.updated_at from task_dependencies d join tasks t on t.id=d.predecessor_task_id where d.task_id in (select task_id from supervision_tasks where session_id=?) and t.id not in (select task_id from supervision_tasks where session_id=?) order by t.id`, id, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	facts := []map[string]any{}
	for rows.Next() {
		var taskID, status string
		var updated time.Time
		if err := rows.Scan(&taskID, &status, &updated); err != nil {
			return nil, err
		}
		facts = append(facts, map[string]any{"taskId": taskID, "status": status, "updatedAt": updated})
	}
	return facts, rows.Err()
}

func verifySupervisionDependenciesTx(ctx context.Context, tx *sql.Tx, id string) error {
	var expected string
	if err := tx.QueryRowContext(ctx, `select fingerprint from supervision_dependency_state where session_id=?`, id).Scan(&expected); errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	facts, err := supervisionExternalDependencyFactsTx(ctx, tx, id)
	if err != nil {
		return err
	}
	if expected != supervisionSHA256(mustJSON(facts)) {
		return errSupervisionState
	}
	return nil
}

// 工作份额耗尽后保留实施成果，用总预算中的预留份额完成候选子集验收。
func enterSupervisionFinalReserveTx(ctx context.Context, tx *sql.Tx, ss *supervisionSession, now time.Time) (bool, error) {
	if ss.State == "finalizing" || ss.Control == "stop" || supervisionTimeAvailable(ss, "final") <= 0 || ss.UsedTurns+ss.ReservedTurns >= ss.Budget.MaxAgentTurns {
		return false, nil
	}
	var implemented int
	if err := tx.QueryRowContext(ctx, `select count(*) from supervision_tasks where session_id=? and implementation_started=1 and phase not in ('unresolved','deferred')`, ss.ID).Scan(&implemented); err != nil {
		return false, err
	}
	if implemented == 0 {
		return false, nil
	}
	active, err := supervisionHasExecutionTx(ctx, tx, ss.ID)
	if err != nil || active {
		return false, err
	}
	if err := suspendSupervisionQueuedTx(ctx, tx, ss, false, now); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `insert into supervision_budget_deferred(session_id,task_id) select session_id,task_id from supervision_tasks where session_id=? and implementation_started=0 and phase not in ('unresolved','deferred') on conflict do nothing`, ss.ID); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `update supervision_tasks set phase='deferred',evidence_freshness='missing' where session_id=? and implementation_started=0 and phase not in ('unresolved','deferred')`, ss.ID); err != nil {
		return false, err
	}
	ss.State, ss.Control, ss.Reason = "finalizing", "", "work_budget_exhausted"
	ss.StateVersion++
	ss.ClockAt = &now
	return true, appendSupervisionEventTx(ctx, tx, ss, "session.finalizing", map[string]string{"reason": "实施份额耗尽，未开始任务暂缓，预留份额仅用于核对已有成果"}, now)
}

// 最终复核发现已有成果失败时，先明确离开最终验收再让调度器诊断、
// 决定是否修复。只使用剩余实施份额，不借用最终预留或解除用户控制。
func reopenSupervisionFinalRepairTx(ctx context.Context, tx *sql.Tx, ss *supervisionSession, job supervisionRoleJob, now time.Time) error {
	if ss.State != "finalizing" || ss.Control != "" || ss.RecoveryState != "ready" {
		return nil
	}
	var started bool
	var fixes int
	if err := tx.QueryRowContext(ctx, `select implementation_started,fix_rounds from supervision_tasks where session_id=? and task_id=?`, ss.ID, job.TaskID).Scan(&started, &fixes); err != nil {
		return err
	}
	if !started || fixes >= ss.Budget.MaxFixRounds {
		return nil
	}
	available, err := supervisionTurnsAvailableTx(ctx, tx, ss, "work", true)
	if err != nil || !available {
		return err
	}
	ss.State, ss.Reason, ss.ClockAt = "running", "", &now
	return appendSupervisionEventTx(ctx, tx, ss, "session.repair_reopened", map[string]string{"taskId": job.TaskID, "executionId": job.ExecutionID, "reason": "最终复核失败，仍有实施和修复额度，重新诊断并按需派发修复"}, now)
}

// 相同代码、要求及验证事实下反复切换读取内容属于无进展，不靠修改路径绕过。
func recordSupervisionObservationProgressTx(ctx context.Context, tx *sql.Tx, ss *supervisionSession, pc supervisionProjectContext, a supervisionWorkflowAction) error {
	rows, err := tx.QueryContext(ctx, `select task_id,requirements_revision from supervision_tasks where session_id=? order by task_id`, ss.ID)
	if err != nil {
		return err
	}
	facts := []any{pc.RevisionID, pc.RulesHash}
	for rows.Next() {
		var id string
		var revision int64
		if err := rows.Scan(&id, &revision); err != nil {
			rows.Close()
			return err
		}
		facts = append(facts, []any{id, revision})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var evidence int
	if err := tx.QueryRowContext(ctx, `select count(*) from supervision_evidence where session_id=?`, ss.ID).Scan(&evidence); err != nil {
		return err
	}
	facts = append(facts, evidence)
	hash := supervisionSHA256(mustJSON(facts))
	var old string
	var count int
	err = tx.QueryRowContext(ctx, `select fingerprint,count from supervision_observation_progress where session_id=?`, ss.ID).Scan(&old, &count)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if old != hash {
		count = 0
	}
	resources := []string{fmt.Sprintf("manifest:%d", a.ManifestOffset)}
	for _, name := range a.Paths {
		resources = append(resources, "source:"+name)
	}
	if a.TaskID != "" {
		resources = append(resources, "task:"+a.TaskID)
	}
	newFacts := int64(0)
	for _, resource := range resources {
		result, err := tx.ExecContext(ctx, `insert into supervision_context_reads(session_id,fingerprint,resource) values (?,?,?) on conflict do nothing`, ss.ID, hash, resource)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		newFacts += n
	}
	if newFacts > 0 {
		count = 0
	} else {
		count++
	}
	if _, err := tx.ExecContext(ctx, `insert into supervision_observation_progress(session_id,fingerprint,count) values (?,?,?) on conflict(session_id) do update set fingerprint=excluded.fingerprint,count=excluded.count`, ss.ID, hash, count); err != nil {
		return err
	}
	if count >= 3 {
		ss.State, ss.Reason, ss.ClockAt = "needs_input", "no_progress", nil
		return appendSupervisionEventTx(ctx, tx, ss, "scheduler.no_progress", map[string]int{"consecutiveReads": count}, time.Now().UTC())
	}
	return nil
}

func supervisionCheckSemanticHash(c supervisionCheck) string {
	c.ID = ""
	sort.Strings(c.AcceptanceIDs)
	raw, _ := json.Marshal(c)
	return supervisionSHA256(raw)
}

func supervisionDuplicateCheckTx(ctx context.Context, tx *sql.Tx, ss *supervisionSession, a supervisionWorkflowAction, revision string) (bool, error) {
	if len(a.Checks) != 1 {
		return false, nil
	}
	rows, err := tx.QueryContext(ctx, `select details from supervision_evidence where session_id=? and task_id=? and revision_id=? and requirements_revision=? and plan_revision=? and kind in ('command','browser') and rowid>coalesce((select evidence_rowid from supervision_dependency_cutoff where session_id=?),0)`, ss.ID, a.TaskID, revision, a.RequirementsRevision, a.PlanRevision, ss.ID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	want := supervisionCheckSemanticHash(a.Checks[0])
	attempts := 0
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return false, err
		}
		var result supervisionCheckResult
		if err := json.Unmarshal([]byte(raw), &result); err != nil {
			return false, err
		}
		if result.Interrupted || result.NotExecuted || result.ExitCode < 0 {
			continue
		}
		if supervisionCheckSemanticHash(result.Check) == want {
			attempts++
			if !supervisionTransientError(result.Error) || attempts > ss.Budget.MaxTransientRetries {
				return true, nil
			}
		}
	}
	return false, rows.Err()
}
