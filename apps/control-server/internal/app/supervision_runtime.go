package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

func (w *supervisionWorker) reschedule(ctx context.Context, id string) {
	if ctx.Err() != nil {
		return
	}
	probe, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var needed bool
	err := w.server.db.QueryRowContext(probe, `select exists(select 1 from supervision_sessions ss where ss.id=? and ss.state in ('running','finalizing') and ss.control='' and ss.recovery_state='ready' and ss.reason<>'target_finalization' and not exists(select 1 from supervision_role_observations o where o.session_id=ss.id and o.state='pending') and not exists(select 1 from supervision_target_finalizations f where f.session_id=ss.id and f.state='pending') and not exists(select 1 from supervision_retry_state rt where rt.session_id=ss.id and rt.next_at>?) and (exists(select 1 from supervision_work_items where session_id=ss.id and state='queued') or not exists(select 1 from supervision_decisions where session_id=ss.id and expected_state_version=ss.state_version and state in ('applied','proposed','stale','cancelled'))))`, id, time.Now().UTC()).Scan(&needed)
	if err == nil && needed {
		w.server.wakeSupervisionWorker(id)
	}
}

func (s *Server) releaseTerminalSupervision(ctx context.Context, ss supervisionSession) error {
	if !supervisionTerminal(ss.State) || ss.AdmissionID == "" {
		return nil
	}
	err := s.withWorkspaceAdmissionTx(ctx, func(tx *sql.Tx, next map[string]WorkspaceAdmission) error {
		a, ok := next[ss.AdmissionID]
		if !ok {
			return nil
		}
		if a.OwnerID != ss.ID || a.Epoch != ss.AdmissionEpoch {
			return errWorkspaceAdmissionStale
		}
		if err := supervisionRunsQuiescentTx(ctx, tx, ss.ID); err != nil {
			return err
		}
		if a.State == "releasing" {
			return nil
		}
		a.State = "releasing"
		a.UpdatedAt = time.Now().UTC()
		if _, err := tx.ExecContext(ctx, `update workspace_admissions set state='releasing',updated_at=? where id=?`, a.UpdatedAt, a.ID); err != nil {
			return err
		}
		next[a.ID] = a
		return nil
	})
	if err != nil {
		return err
	}
	if err := s.releaseSupervisionTargetProtection(ctx, ss.ID); err != nil {
		return err
	}
	if err := s.releaseSharedSupervisionAdmission(ctx, ss); err != nil {
		return err
	}
	return s.releaseWorkspaceAdmission(ctx, ss.AdmissionID, ss.ID, ss.AdmissionEpoch)
}

func (s *Server) supervisionRetryDue(ctx context.Context, id string) (bool, error) {
	var at sql.NullTime
	err := s.db.QueryRowContext(ctx, `select next_at from supervision_retry_state where session_id=?`, id).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return !at.Valid || !time.Now().UTC().Before(at.Time), nil
}

func (s *Server) activateSupervisionRetry(ctx context.Context, id string) error {
	return s.withSupervisionTx(ctx, id, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
		if ss.Reason != "transient_retry_wait" || ss.Control != "" {
			return nil
		}
		now := time.Now().UTC()
		ss.Reason, ss.ClockAt = "", &now
		_, err := tx.ExecContext(ctx, `update supervision_retry_state set next_at=null where session_id=?`, id)
		return err
	})
}

func (s *Server) markSupervisionRecoveryReview(ctx context.Context, id string) error {
	return s.withSupervisionTx(ctx, id, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
		if ss.Control != "" || ss.RecoveryState == "ready" {
			return nil
		}
		ss.State, ss.Reason, ss.ClockAt = "needs_input", "recovery_review_required", nil
		ss.StateVersion++
		return appendSupervisionEventTx(ctx, tx, ss, "recovery.review_required", map[string]string{"reason": "目录、规则或中断写入需要核对；接受当前变更后可重新验收"}, time.Now().UTC())
	})
}

func (s *Server) prepareRecoveredSupervisionControl(ctx context.Context, id string) error {
	proof, err := s.supervisionTargetQuietForControl(ctx, id)
	if err != nil {
		return err
	}
	return s.withSupervisionTx(ctx, id, time.Now().UTC(), func(tx *sql.Tx, next map[string]WorkspaceAdmission, ss *supervisionSession) error {
		if ss.Control == "" || ss.RecoveryState == "ready" {
			return nil
		}
		a, exists := next[ss.AdmissionID]
		if !exists || a.OwnerID != id || a.Epoch != ss.AdmissionEpoch || s.admissionHasOperationsLocked(a) {
			return errWorkspaceNotQuiescent
		}
		if err := proof.verifyTx(ctx, tx, ss, a); err != nil && (proof != nil || ss.Control != "stop") {
			return err
		}
		var active bool
		if err := tx.QueryRowContext(ctx, `select exists(select 1 from supervision_executions where session_id=? and state<>'quiescent') or exists(select 1 from supervision_work_items where session_id=? and state in ('launching','running','unknown')) or exists(select 1 from supervision_run_links where session_id=? and process_state<>'quiescent')`, id, id, id).Scan(&active); err != nil {
			return err
		}
		if active {
			return errWorkspaceNotQuiescent
		}
		now := time.Now().UTC()
		if _, err := tx.ExecContext(ctx, `update supervision_mutations set state='completed' where session_id=? and state='unknown';update supervision_role_jobs set state='interrupted',error='恢复后遵循用户控制，未核验成果保留待处理' where session_id=? and state in ('running','queued','unknown');update task_runs set status='failed',failure_reason='托管恢复后停止或暂停，结果未验收',finished_at=? where status='running' and run_id in (select run_id from supervision_run_links where session_id=? and process_state='quiescent')`, id, id, now, id); err != nil {
			return err
		}
		a.RecoveryState, a.State, a.UpdatedAt = "ready", "owned", now
		if _, err := tx.ExecContext(ctx, `update workspace_admissions set recovery_state='ready',state='owned',updated_at=? where id=?`, now, a.ID); err != nil {
			return err
		}
		next[a.ID] = a
		ss.RecoveryState = "ready"
		ss.StateVersion++
		if ss.Control == "stop" {
			ss.State = "stopping"
		} else {
			ss.State = "pausing"
		}
		return appendSupervisionEventTx(ctx, tx, ss, "recovery.control_ready", map[string]string{"control": ss.Control, "outcome": "中断结果未被接受，只解除已确认静默资源的控制保护"}, now)
	})
}

func supervisionTransientError(message string) bool {
	lower := strings.ToLower(message)
	for _, denied := range []string{"unauthorized", "authentication", "forbidden", "invalid api key", "revoked", "401", "403", "版本", "协议", "无工具", "插件", "目录", "budget"} {
		if strings.Contains(lower, denied) {
			return false
		}
	}
	for _, word := range []string{"429", "rate limit", "overloaded", "503", "502", "connection reset", "connection refused", "temporary", "timeout", "timed out", "context deadline exceeded", "network", "eof"} {
		if strings.Contains(lower, word) {
			return true
		}
	}
	return false
}

func (s *Server) scheduleSupervisionRetry(ctx context.Context, id string) error {
	return s.withSupervisionTx(ctx, id, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
		if ss.State != "needs_input" || (ss.Reason != "scheduler_execution_failed" && ss.Reason != "scheduler_start_failed") || ss.Control != "" || ss.RecoveryState != "ready" {
			return nil
		}
		var message, budgetClass string
		if err := tx.QueryRowContext(ctx, `select d.validation_error,w.budget_class from supervision_decisions d join supervision_work_items w on w.decision_id=d.id where d.session_id=? and w.role='scheduler' order by d.created_at desc,d.id desc,w.attempt desc limit 1`, id).Scan(&message, &budgetClass); err != nil {
			return err
		}
		if !supervisionTransientError(message) {
			return nil
		}
		var count int
		if err := tx.QueryRowContext(ctx, `select coalesce((select count from supervision_retry_state where session_id=?),0)`, id).Scan(&count); err != nil {
			return err
		}
		if count >= ss.Budget.MaxTransientRetries {
			return nil
		}
		count++
		delay := time.Duration(1<<min(count, 6)) * time.Second
		at := time.Now().UTC().Add(delay)
		if _, err := tx.ExecContext(ctx, `insert into supervision_retry_state(session_id,count,next_at,error) values (?,?,?,?) on conflict(session_id) do update set count=excluded.count,next_at=excluded.next_at,error=excluded.error`, id, count, at, message); err != nil {
			return err
		}
		ss.State, ss.Reason = "running", "transient_retry_wait"
		if budgetClass == "final" {
			ss.State = "finalizing"
		}
		now := time.Now().UTC()
		ss.ClockAt = &now
		ss.StateVersion++
		return appendSupervisionEventTx(ctx, tx, ss, "scheduler.retry_scheduled", map[string]any{"attempt": count, "nextAt": at, "reason": message, "resumeState": ss.State}, time.Now().UTC())
	})
}

// resume 不重放旧 outbox。先在恢复保护下采集，再对照上次清单/已登记写入日志；
// 只有目录、规则、执行和所有权均可核对时，才开启一个新调度代际。
func (s *Server) resumeSupervisionSession(ctx context.Context, id string, version int64, acceptDrift, automatic bool) error {
	if previous, replay, err := supervisionRequestReceipt(ctx, s.db); err != nil {
		return err
	} else if replay {
		if previous != id {
			return errSupervisionState
		}
		return nil
	}
	ss, err := loadSupervisionSession(ctx, s.db, id)
	if err != nil {
		return err
	}
	if ss.StateVersion != version || supervisionTerminal(ss.State) || ss.State == "draft" || ss.Control == "stop" {
		return errSupervisionState
	}
	if automatic && (ss.Control != "" || (ss.State != "running" && ss.State != "finalizing")) {
		return errSupervisionState
	}
	if handled, err := s.resumeUnboundSupervisionAdmission(ctx, ss, automatic); handled || err != nil {
		return err
	}
	current, err := loadSupervisionProjectContext(ctx, s.db, id, "current")
	missingBaseline := errors.Is(err, sql.ErrNoRows)
	if err != nil && !missingBaseline {
		return err
	}
	approved := current
	pendingContext, pendingErr := loadSupervisionProjectContext(ctx, s.db, id, "pending")
	if pendingErr != nil && !errors.Is(pendingErr, sql.ErrNoRows) {
		return pendingErr
	}
	pendingDrift := pendingErr == nil
	if pendingDrift {
		approved = pendingContext
	}
	var admission WorkspaceAdmission
	var source supervisionWorkspaceSource
	err = s.withWorkspaceAdmissionTx(ctx, func(tx *sql.Tx, next map[string]WorkspaceAdmission) error {
		var ok bool
		admission, ok = next[ss.AdmissionID]
		if !ok || admission.OwnerID != id || admission.Epoch != ss.AdmissionEpoch || s.admissionHasOperationsLocked(admission) {
			return errWorkspaceNotQuiescent
		}
		// 容器核对可以确认退出；结果不明只能保留为失败事实，不能重新应用旧输出。
		if _, err := tx.ExecContext(ctx, `update task_runs set status='failed',failure_reason='托管重启后中断，保留成果等待重新核查',finished_at=? where status='running' and run_id in (select l.run_id from supervision_run_links l join runs r on r.id=l.run_id where l.session_id=? and l.process_state='quiescent' and r.status not in ('queued','running'))`, time.Now().UTC(), id); err != nil {
			return err
		}
		var active bool
		if err := tx.QueryRowContext(ctx, `select exists(select 1 from supervision_executions where session_id=? and state<>'quiescent') or exists(select 1 from supervision_work_items where session_id=? and state in ('launching','running','unknown') and execution_id not in (select execution_id from supervision_executions where state='quiescent')) or exists(select 1 from supervision_context_captures where session_id=? and state='collecting')`, id, id, id).Scan(&active); err != nil {
			return err
		}
		if active {
			return errWorkspaceNotQuiescent
		}
		var err error
		source, err = s.supervisionWorkspaceSourceModeTx(ctx, tx, &ss, admission, true)
		return err
	})
	if err != nil {
		return err
	}
	collected, err := source.collect(ctx, s)
	if err != nil {
		return err
	}
	drift := pendingDrift || !missingBaseline && (collected.RevisionID != current.RevisionID || collected.RulesHash != current.RulesHash)
	if !acceptDrift && (drift || ss.Reason == "workspace_or_rules_drift" || ss.Reason == "file_application_incomplete") {
		return errSupervisionSnapshotChanged
	}
	return s.withSupervisionRequestTx(ctx, id, time.Now().UTC(), func(tx *sql.Tx, next map[string]WorkspaceAdmission, latest *supervisionSession) error {
		if latest.StateVersion != version || latest.Control == "stop" {
			return errSupervisionState
		}
		a, ok := next[latest.AdmissionID]
		if !ok || a != admission {
			return errWorkspaceAdmissionStale
		}
		if err := source.verifyTx(ctx, s, tx, latest, a); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `update supervision_mutations set state='completed' where session_id=? and state='unknown';update supervision_role_jobs set state='interrupted',error='恢复后重新核查，不重放旧执行' where session_id=? and state in ('queued','running','unknown');update supervision_tasks set phase=case when phase='implementing' then 'needs_repair' when phase in ('verifying','reviewing') then 'implemented' else phase end,evidence_freshness=case when ? then 'stale' else evidence_freshness end where session_id=? and phase not in ('deferred','unresolved')`, id, id, drift, id); err != nil {
			return err
		}
		if err := supervisionRunsQuiescentTx(ctx, tx, id); err != nil {
			return err
		}
		if err := suspendSupervisionQueuedTx(ctx, tx, latest, false, time.Now().UTC()); err != nil {
			return err
		}
		if drift || missingBaseline {
			if err := persistSupervisionResumedContextTx(ctx, tx, latest, collected); err != nil {
				return err
			}
		}
		a.State, a.RecoveryState, a.UpdatedAt = "owned", "ready", time.Now().UTC()
		if _, err := tx.ExecContext(ctx, `update workspace_admissions set state='owned',recovery_state='ready',updated_at=? where id=?`, a.UpdatedAt, a.ID); err != nil {
			return err
		}
		next[a.ID] = a
		phase := latest.RunPhase
		if phase == "" || drift {
			phase = "running"
		}
		latest.State, latest.Control, latest.Reason, latest.RecoveryState = phase, "", "", "ready"
		latest.LeaseOwner = ""
		latest.LeaseExpiresAt = nil
		latest.LeaseEpoch++
		latest.StateVersion++
		now := time.Now().UTC()
		latest.ClockAt = &now
		if acceptDrift && !missingBaseline && collected.RulesHash != approved.RulesHash {
			if err := expireSupervisionQuestionsTx(ctx, tx, id, ""); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `update supervision_tasks set requirements_revision=requirements_revision+1,evidence_freshness='missing',phase='pending' where session_id=?`, id); err != nil {
				return err
			}
		}
		return appendSupervisionEventTx(ctx, tx, latest, "session.resumed", map[string]any{"automatic": automatic, "acceptedDrift": acceptDrift, "revisionId": collected.RevisionID, "resumeState": latest.State}, now)
	})
}

func persistSupervisionResumedContextTx(ctx context.Context, tx *sql.Tx, ss *supervisionSession, collected supervisionCollectedContext) error {
	if err := checkSupervisionCASBudgetTx(ctx, tx, ss.ID, collected.Blobs); err != nil {
		return err
	}
	for hash, data := range collected.Blobs {
		if _, err := tx.ExecContext(ctx, `insert into supervision_revision_blobs(sha256,content,size) values (?,?,?) on conflict(sha256) do nothing`, hash, data, len(data)); err != nil {
			return err
		}
	}
	manifest, _ := json.Marshal(collected.Manifest)
	rules, _ := json.Marshal(collected.Rules)
	if _, err := tx.ExecContext(ctx, `insert into supervision_revisions(id,manifest,created_at) values (?,?,?) on conflict(id) do nothing`, collected.RevisionID, string(manifest), time.Now().UTC()); err != nil {
		return err
	}
	contextID := uuid.NewString()
	if _, err := tx.ExecContext(ctx, `insert into supervision_project_contexts(id,session_id,revision_id,rules_hash,rules_json,admission_epoch,created_at) values (?,?,?,?,?,?,?)`, contextID, ss.ID, collected.RevisionID, collected.RulesHash, string(rules), ss.AdmissionEpoch, time.Now().UTC()); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `insert into supervision_context_state(session_id,baseline_id,current_id) values (?,?,?) on conflict(session_id) do update set current_id=excluded.current_id,pending_base_id=''`, ss.ID, contextID, contextID)
	return err
}

func (s *Server) applySupervisionAmendments(ctx context.Context, id string) error {
	return s.withSupervisionTx(ctx, id, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
		if supervisionTerminal(ss.State) || ss.Control == "stop" {
			return nil
		}
		active, err := supervisionHasExecutionTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if active {
			return nil
		}
		rows, err := tx.QueryContext(ctx, `select id,task_id,content from supervision_amendments where session_id=? and state='pending' order by created_at,id`, id)
		if err != nil {
			return err
		}
		var pending []supervisionAmendment
		for rows.Next() {
			var a supervisionAmendment
			if err := rows.Scan(&a.ID, &a.TaskID, &a.Content); err != nil {
				rows.Close()
				return err
			}
			pending = append(pending, a)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(pending) == 0 {
			return nil
		}
		for _, a := range pending {
			if err := expireSupervisionQuestionsTx(ctx, tx, id, a.TaskID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `update supervision_tasks set requirements_revision=requirements_revision+1,evidence_freshness='missing',phase='pending' where session_id=? and (?='' or task_id=?);update supervision_amendments set state='applied' where id=?`, id, a.TaskID, a.TaskID, a.ID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `delete from supervision_observation_progress where session_id=?;update supervision_plan_progress set revisions_without_progress=0 where session_id=?;delete from supervision_retry_state where session_id=?`, id, id, id); err != nil {
			return err
		}
		if err := suspendSupervisionQueuedTx(ctx, tx, ss, false, time.Now().UTC()); err != nil {
			return err
		}
		// 新要求使旧计划失效，需要重新实施。用户暂停仍保持暂停，但显式
		// 恢复时进入实施阶段；延后要求也不能困在只允许验收的阶段。
		ss.RunPhase = "running"
		if ss.State == "finalizing" && ss.Control == "" || ss.Reason == "amendment_interrupt" {
			ss.State, ss.Control, ss.Reason = "running", "", ""
			now := time.Now().UTC()
			ss.ClockAt = &now
		}
		if ss.State == "needs_input" && ss.Reason == "user_question" && ss.Control == "" && ss.RecoveryState == "ready" {
			var waiting bool
			if err := tx.QueryRowContext(ctx, `select exists(select 1 from supervision_questions where session_id=? and task_id='' and answered_at is null and expired=0)`, id).Scan(&waiting); err != nil {
				return err
			}
			if !waiting {
				ss.State, ss.Reason = "running", ""
				now := time.Now().UTC()
				ss.ClockAt = &now
			}
		}
		ss.StateVersion++
		return appendSupervisionEventTx(ctx, tx, ss, "instructions.applied", pending, time.Now().UTC())
	})
}

func (s *Server) pruneSupervisionCAS(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `delete from supervision_revisions where not exists(select 1 from supervision_project_contexts c where c.revision_id=supervision_revisions.id);
	delete from supervision_revision_blobs where not exists(select 1 from supervision_revisions r,json_each(r.manifest,'$.entries') e where json_extract(e.value,'$.sha256')=supervision_revision_blobs.sha256 and coalesce(json_extract(e.value,'$.gap'),'')='');`)
	return err
}
