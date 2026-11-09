package app

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
)

// 执行经过领域校验的调度动作。各角色复用持久工作项、预算和进程收口协议。
type supervisionDecisionExecutor func(context.Context, supervisionDecision, supervisionLease) error

type supervisionWorker struct {
	server              *Server
	owner               string
	execute             supervisionDecisionExecutor
	wake                chan string
	interval            time.Duration
	mu                  sync.Mutex
	active              map[string]bool
	recoveringWorkspace bool
}

func (s *Server) startSupervisionWorkerLoop() bool {
	return s.startSupervisionWorker(s.executeSupervisionWorkerDecision, 30*time.Second)
}

func (s *Server) startSupervisionWorker(execute supervisionDecisionExecutor, interval time.Duration) bool {
	if execute == nil || interval <= 0 || s.runtimeCtx == nil {
		return false
	}
	s.supervisionMu.Lock()
	defer s.supervisionMu.Unlock()
	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()
	if closing || s.runtimeCtx.Err() != nil || s.supervisionWorker != nil {
		return false
	}
	w := &supervisionWorker{server: s, owner: uuid.NewString(), execute: execute, wake: make(chan string, 64), interval: interval, active: map[string]bool{}}
	s.supervisionWorker = w
	s.supervisionWG.Add(1)
	go func() {
		defer s.supervisionWG.Done()
		w.loop(s.runtimeCtx)
	}()
	return true
}

// 唤醒只是提示；事实在数据库中。队列满或活动回合中的重复通知由周期补查兜底。
func (s *Server) wakeSupervisionWorker(id string) {
	s.supervisionMu.Lock()
	w := s.supervisionWorker
	s.supervisionMu.Unlock()
	if w == nil {
		return
	}
	select {
	case w.wake <- id:
	default:
	}
}

func (w *supervisionWorker) loop(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	w.sweep(ctx)
	gcTicker := time.NewTicker(time.Hour)
	defer gcTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-w.wake:
			w.start(ctx, id)
		case <-ticker.C:
			w.sweep(ctx)
		case <-gcTicker.C:
			w.report(ctx, w.server.pruneSupervisionCAS(ctx))
		}
	}
}

func (w *supervisionWorker) sweep(ctx context.Context) {
	w.startWorkspaceRecovery(ctx)
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := w.server.db.QueryContext(probeCtx, `select id from supervision_sessions where state not in ('draft','stopped','completed','completed_with_issues','failed') or (state in ('stopped','completed','completed_with_issues','failed') and exists(select 1 from workspace_admissions a where a.id=supervision_sessions.admission_id)) order by created_at,id`)
	if err != nil {
		w.report(ctx, err)
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close() // 唯一 SQLite 连接必须先释放，再启动领域事务。
	if err != nil {
		w.report(ctx, err)
		return
	}
	for _, id := range ids {
		w.start(ctx, id)
	}
}

func (w *supervisionWorker) startWorkspaceRecovery(ctx context.Context) {
	w.server.supervisionMu.Lock()
	defer w.server.supervisionMu.Unlock()
	w.server.mu.Lock()
	closing := w.server.closing
	w.server.mu.Unlock()
	if closing || ctx.Err() != nil {
		return
	}
	w.mu.Lock()
	if w.recoveringWorkspace {
		w.mu.Unlock()
		return
	}
	w.recoveringWorkspace = true
	w.mu.Unlock()
	w.server.supervisionWG.Add(1)
	go func() {
		defer w.server.supervisionWG.Done()
		defer func() { w.mu.Lock(); w.recoveringWorkspace = false; w.mu.Unlock() }()
		probe, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		w.report(ctx, w.server.reconcilePendingWorkspaceGuardOperations(probe))
	}()
}

func (w *supervisionWorker) report(ctx context.Context, err error) {
	if err != nil && ctx.Err() == nil && !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, errSupervisionState) && !errors.Is(err, errSupervisionLease) && !errors.Is(err, errWorkspaceNotQuiescent) && !errors.Is(err, errSupervisionBudget) {
		log.Printf("[supervision] worker: %s", redactAgentText(err.Error()))
	}
}

func (w *supervisionWorker) start(ctx context.Context, id string) {
	w.server.supervisionMu.Lock()
	defer w.server.supervisionMu.Unlock()
	w.server.mu.Lock()
	closing := w.server.closing
	w.server.mu.Unlock()
	if closing || ctx.Err() != nil {
		return
	}
	w.mu.Lock()
	if w.active[id] {
		w.mu.Unlock()
		return
	}
	w.active[id] = true
	w.mu.Unlock()
	w.server.supervisionWG.Add(1)
	go func() {
		defer w.server.supervisionWG.Done()
		defer func() { w.mu.Lock(); delete(w.active, id); w.mu.Unlock(); w.reschedule(ctx, id) }()
		w.report(ctx, w.process(ctx, id))
	}()
}

func (w *supervisionWorker) maintain(ctx context.Context, id string) (supervisionSession, error) {
	s := w.server
	ss, err := loadSupervisionSession(ctx, s.db, id)
	if err != nil {
		return ss, err
	}
	if supervisionTerminal(ss.State) && ss.AdmissionID != "" {
		err := s.releaseTerminalSupervision(ctx, ss)
		return ss, err
	}
	if ss.RecoveryState != "ready" {
		// 仅查询受保护容器，不解除恢复保护或重放旧 outbox。
		rows, err := s.db.QueryContext(ctx, `select execution_id from supervision_executions where session_id=? and state='unknown'`, id)
		if err != nil {
			return ss, err
		}
		var ids []string
		for rows.Next() {
			var executionID string
			if err = rows.Scan(&executionID); err != nil {
				break
			}
			ids = append(ids, executionID)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return ss, err
		}
		for _, executionID := range ids {
			if ss.Control != "" {
				if err := s.stopSupervisionExecutionForRecovery(ctx, id, executionID); err != nil {
					return ss, err
				}
			}
			if err := s.reconcileSupervisionExecution(ctx, executionID, time.Now().UTC()); err != nil {
				return ss, err
			}
		}
		ss, err = loadSupervisionSession(ctx, s.db, id)
		if err != nil {
			return ss, err
		}
		if ss.Control == "" && (ss.State == "running" || ss.State == "finalizing") {
			if err := s.resumeSupervisionSession(ctx, id, ss.StateVersion, false, true); err == nil {
				return loadSupervisionSession(ctx, s.db, id)
			} else if errors.Is(err, errSupervisionSnapshotChanged) {
				if err := s.markSupervisionRecoveryReview(ctx, id); err != nil {
					return ss, err
				}
				return loadSupervisionSession(ctx, s.db, id)
			} else if !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, errWorkspaceNotQuiescent) {
				return ss, err
			}
		}
		if ss.Control != "" {
			if err := s.prepareRecoveredSupervisionControl(ctx, id); err != nil {
				return ss, err
			}
			if err := s.settleSupervisionControl(ctx, id, time.Now().UTC()); err != nil {
				return ss, err
			}
			return loadSupervisionSession(ctx, s.db, id)
		}
		return ss, nil
	}
	if err := s.advanceSupervisionRoleObservations(ctx, id, w.owner); err != nil {
		return ss, err
	}
	if handled, err := s.advanceSupervisionTargetHandoff(ctx, id, w.owner); handled || err != nil {
		if err != nil {
			return ss, err
		}
		return loadSupervisionSession(ctx, s.db, id)
	}
	if err := s.scheduleSupervisionRetry(ctx, id); err != nil {
		return ss, err
	}
	if err := s.applySupervisionAmendments(ctx, id); err != nil {
		return ss, err
	}
	if err := s.refreshSupervisionDependencies(ctx, id); err != nil {
		return ss, err
	}
	if err := s.checkSupervisionBudget(ctx, id, time.Now().UTC()); err != nil {
		return ss, err
	}
	if err := s.refreshSupervisionEvidenceEnvironments(ctx, id); err != nil {
		return ss, err
	}
	if err := s.checkSupervisionStorage(ctx, id); err != nil {
		return ss, err
	}
	if err := s.refreshSupervisionWait(ctx, id, time.Now().UTC()); err != nil {
		return ss, err
	}
	ss, err = loadSupervisionSession(ctx, s.db, id)
	if err == nil && (ss.State == "pausing" || ss.State == "stopping") {
		err = s.settleSupervisionControl(ctx, id, time.Now().UTC())
		if err == nil {
			ss, err = loadSupervisionSession(ctx, s.db, id)
		}
	}
	if err == nil && ss.State == "paused" && ss.Reason == "budget_exhausted" {
		err = s.withSupervisionTx(ctx, id, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, current *supervisionSession) error {
			_, e := enterSupervisionFinalReserveTx(ctx, tx, current, time.Now().UTC())
			return e
		})
		if err == nil {
			ss, err = loadSupervisionSession(ctx, s.db, id)
		}
	}
	if err == nil {
		err = s.fenceSupervisionPendingDrift(ctx, id)
		if err == nil {
			ss, err = loadSupervisionSession(ctx, s.db, id)
		}
	}
	return ss, err
}

func (w *supervisionWorker) process(ctx context.Context, id string) error {
	s := w.server
	// 一次唤醒最多三轮：初次输出及两次格式纠正，不做任意业务重试。
	for round := 0; round < 3; round++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		ss, err := w.maintain(ctx, id)
		if err != nil {
			return err
		}
		if ss.RecoveryState != "ready" || ss.Control != "" || (ss.State != "running" && ss.State != "finalizing") {
			return nil
		}
		var handoffPending bool
		if err := s.db.QueryRowContext(ctx, `select exists(select 1 from supervision_target_finalizations where session_id=? and state='pending')`, id).Scan(&handoffPending); err != nil {
			return err
		}
		if handoffPending || ss.State == "finalizing" && ss.Reason == "target_finalization" {
			return nil // A sealed or superseded handoff never opens a model turn.
		}
		if due, err := s.supervisionRetryDue(ctx, id); err != nil {
			return err
		} else if !due {
			return nil
		}
		var admissionState string
		if err := s.db.QueryRowContext(ctx, `select state from workspace_admissions where id=?`, ss.AdmissionID).Scan(&admissionState); err != nil {
			return err
		}
		if admissionState == "pending" {
			if err := s.admitSupervisionWorkspace(ctx, id, ss.StateVersion, time.Now().UTC()); err != nil {
				return err
			}
			ss, err = loadSupervisionSession(ctx, s.db, id)
			if err != nil {
				return err
			}
		}
		if err := s.ensureSupervisionPublicTarget(ctx, id, w.owner); err != nil {
			return err
		}
		ss, err = loadSupervisionSession(ctx, s.db, id)
		if err != nil {
			return err
		}
		if _, err := loadSupervisionProjectContext(ctx, s.db, id, "baseline"); errors.Is(err, sql.ErrNoRows) {
			if _, err := s.captureSupervisionProjectContext(ctx, id, ss.StateVersion); err != nil {
				return err
			}
			ss, err = loadSupervisionSession(ctx, s.db, id)
			if err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		// 相同业务事实已有建议或失败结果时不再次调用模型，lease 过期也不能绕过。
		var previousID, previousState string
		err = s.db.QueryRowContext(ctx, `select id,state from supervision_decisions where session_id=? and expected_state_version=? order by created_at desc,id desc limit 1`, id, ss.StateVersion).Scan(&previousID, &previousState)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if previousState == "proposed" && ss.LeaseOwner == w.owner {
			lease, err := s.acquireSupervisionLease(ctx, id, w.owner, time.Now().UTC())
			if err != nil {
				return err
			}
			_, err = s.applySupervisionAction(ctx, previousID, lease)
			return err
		}
		if previousState == "applied" {
			corrected, err := s.correctSupervisionStalledDecision(ctx, id, previousID)
			if err != nil {
				return err
			}
			if corrected {
				previousState = "invalid"
				ss, err = loadSupervisionSession(ctx, s.db, id)
				if err != nil || ss.State == "needs_input" {
					return err
				}
			}
		}
		if previousState == "proposed" || previousState == "applied" || previousState == "stale" || previousState == "cancelled" {
			return nil
		}
		lease, err := s.acquireSupervisionLease(ctx, id, w.owner, time.Now().UTC())
		if err != nil {
			return err
		}
		if ss.Reason == "transient_retry_wait" {
			if err := s.activateSupervisionRetry(ctx, id); err != nil {
				return err
			}
		}
		retryID := ""
		if previousState == "invalid" {
			retryID = previousID
		}
		d, err := s.prepareSupervisionDecision(ctx, id, ss.StateVersion, lease, retryID, time.Now().UTC())
		if err != nil {
			return err
		}
		if d.State != "pending" {
			return nil
		}
		if err := w.execute(ctx, d, lease); err != nil {
			if ctx.Err() == nil {
				_, settleErr := w.maintain(ctx, id)
				return errors.Join(err, settleErr)
			}
			return err
		}
		latest, err := loadSupervisionDecision(ctx, s.db, d.ID)
		if err != nil {
			return err
		}
		if latest.State == "proposed" {
			if _, err := s.applySupervisionAction(ctx, latest.ID, lease); err != nil {
				return err
			}
			latest, err = loadSupervisionDecision(ctx, s.db, d.ID)
			if err != nil {
				return err
			}
		}
		if latest.State != "invalid" {
			// 包括完成回合期间提交的控制；不等下一次 30 秒补查才收口。
			_, err = w.maintain(ctx, id)
			return err
		}
	}
	return nil
}

func (s *Server) executeSupervisionWorkerDecision(ctx context.Context, d supervisionDecision, lease supervisionLease) error {
	deadline, err := s.claimSupervisionDecisionLaunch(ctx, d.ID, lease, time.Now().UTC())
	if err != nil {
		return err
	}
	permit, err := s.prepareSupervisionExecution(ctx, d, lease, time.Now().UTC())
	if err != nil {
		settleCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return errors.Join(err, s.failSupervisionUnstartedDecision(settleCtx, d, lease, err, time.Now().UTC()))
	}
	return s.executeSupervisionPreparedWork(ctx, permit, deadline)
}

func (s *Server) executeSupervisionPreparedWork(ctx context.Context, permit supervisionExecutionPermit, deadline time.Time) error {
	// A durable target binding selects the target executor even when its paths
	// happen to exist on this controller. Never fall back after a target error.
	intent, err := loadSupervisionTargetControl(ctx, s.db, permit.Decision.SessionID)
	if err == nil {
		return s.executeSupervisionTargetDecision(ctx, permit, deadline, intent)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if permit.Role == "test" {
		return s.executeSupervisionVerification(ctx, permit, deadline)
	}
	runner, err := s.supervisionAdapter(permit.AgentID)
	if err != nil {
		settleCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return errors.Join(err, s.finishSupervisionExecution(settleCtx, permit, supervisionSchedulerCLIReceipt{ContainerQuiet: true}, err, time.Now().UTC()))
	}
	return s.executeSupervisionScheduler(ctx, permit, deadline, runner)
}

// launching 后创建 Run 失败的可信补偿：只有不存在任何执行身份/关联才能确认未启动。
// 已消费的尝试不退款；pause/stop 意图优先，不能将已有 Run 当作启动失败抹掉。
func (s *Server) failSupervisionUnstartedDecision(ctx context.Context, d supervisionDecision, lease supervisionLease, cause error, now time.Time) error {
	return s.withSupervisionTx(ctx, d.SessionID, now, func(tx *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
		if ss.LeaseOwner != lease.Owner || lease.Owner == "" || ss.LeaseEpoch != lease.Epoch || ss.RecoveryState != "ready" {
			return errSupervisionLease
		}
		var state, workItemID string
		var active bool
		if err := tx.QueryRowContext(ctx, `select id,state from supervision_work_items where execution_id=? and decision_id=? and session_id=? and lease_epoch=?`, d.ExecutionID, d.ID, ss.ID, lease.Epoch).Scan(&workItemID, &state); err != nil {
			return err
		}
		if workItemID != d.WorkItemID {
			return errSupervisionState
		}
		if state == "quiescent" {
			return nil
		}
		if state != "launching" {
			return errSupervisionState
		}
		if err := tx.QueryRowContext(ctx, `select exists(select 1 from supervision_executions where execution_id=?) or exists(select 1 from supervision_run_links where work_item_id=?)`, d.ExecutionID, workItemID).Scan(&active); err != nil {
			return err
		}
		if active {
			return errWorkspaceNotQuiescent
		}
		message := "调度启动失败"
		if cause != nil {
			message = redactAgentText(cause.Error())
			if len(message) > 4096 {
				message = message[:4096]
			}
		}
		if _, err := tx.ExecContext(ctx, `update supervision_work_items set state='quiescent',finished_at=? where execution_id=?`, now, d.ExecutionID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `update supervision_outbox set state='completed',updated_at=? where execution_id=?`, now, d.ExecutionID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `update supervision_decisions set state='stale',validation_error=?,completed_at=? where id=? and state not in ('cancelled','applied')`, message, now, d.ID); err != nil {
			return err
		}
		var role, taskID string
		if err := tx.QueryRowContext(ctx, `select role,task_id from supervision_work_items where execution_id=?`, d.ExecutionID).Scan(&role, &taskID); err != nil {
			return err
		}
		if role != "scheduler" {
			if _, err := tx.ExecContext(ctx, `update supervision_role_jobs set state='failed',error=? where execution_id=?;update supervision_tasks set phase='needs_repair' where session_id=? and task_id=?;update supervision_decisions set state='applied' where id=?`, message, d.ExecutionID, ss.ID, taskID, d.ID); err != nil {
				return err
			}
			ss.StateVersion++
			return appendSupervisionEventTx(ctx, tx, ss, "work_item.failed", map[string]string{"taskId": taskID, "executionId": d.ExecutionID, "role": role, "error": message}, now)
		}
		if ss.Control == "" {
			ss.State, ss.Reason, ss.ClockAt = "needs_input", "scheduler_start_failed", nil
			ss.StateVersion++
		}
		return appendSupervisionEventTx(ctx, tx, ss, "scheduler.start_failed", map[string]string{"decisionId": d.ID, "executionId": d.ExecutionID, "error": message}, now)
	})
}
