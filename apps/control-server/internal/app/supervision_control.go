package app

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

func (s *Server) admitSupervisionWorkspace(ctx context.Context, id string, version int64, now time.Time) error {
	prepared, err := s.prepareSharedSupervisionAdmission(ctx, id, version)
	if err != nil {
		return err
	}
	return s.withSupervisionTx(ctx, id, now, func(tx *sql.Tx, next map[string]WorkspaceAdmission, ss *supervisionSession) error {
		if err := requireSupervisionVersion(ss, version); err != nil {
			return err
		}
		if ss.State != "running" || ss.Control != "" || ss.RecoveryState != "ready" {
			return errSupervisionState
		}
		a, exists := next[ss.AdmissionID]
		if !exists || a.OwnerID != id || a.Epoch != ss.AdmissionEpoch {
			return errWorkspaceAdmissionStale
		}
		if a.RecoveryState != "ready" {
			return errWorkspaceNotQuiescent
		}
		if a.State == "owned" {
			return nil
		}
		if a.State != "pending" {
			return errWorkspaceAdmissionStale
		}
		if s.admissionHasOperationsLocked(a) {
			return errWorkspaceNotQuiescent
		}
		if prepared.ID != a.ID || prepared.OwnerID != a.OwnerID || prepared.Epoch != a.Epoch || prepared.Domain != a.Domain || prepared.OriginalPath != a.OriginalPath {
			return errWorkspaceAdmissionStale
		}
		if a.Domain == "" || a.Domain == "local" {
			if err := verifyWorkspaceAdmissionPath(a); err != nil {
				return err
			}
		} else {
			if prepared.DirectoryIdentity == "" || prepared.CanonicalPath == "" {
				return errWorkspaceAdmissionStale
			}
			a.DirectoryIdentity, a.CanonicalPath = prepared.DirectoryIdentity, prepared.CanonicalPath
		}
		if err := supervisionRunsQuiescentTx(ctx, tx, id); err != nil {
			return err
		}
		a.State, a.UpdatedAt = "owned", now
		if _, err := tx.ExecContext(ctx, `update workspace_admissions set state='owned',canonical_path=?,directory_identity=?,updated_at=? where id=?`, a.CanonicalPath, a.DirectoryIdentity, now, a.ID); err != nil {
			return err
		}
		next[a.ID] = a
		ss.Reason = ""
		ss.StateVersion++
		return appendSupervisionEventTx(ctx, tx, ss, "workspace.admitted", map[string]any{"admissionId": a.ID, "epoch": a.Epoch}, now)
	})
}

// 未启动的预留可释放；launching 已算实际尝试，必须核对进程，不能返还预算。
func suspendSupervisionQueuedTx(ctx context.Context, tx *sql.Tx, ss *supervisionSession, stop bool, now time.Time) error {
	var reserved int
	if err := tx.QueryRowContext(ctx, `select count(*) from supervision_work_items where session_id=? and state in ('queued','suspended') and turn_state='reserved'`, ss.ID).Scan(&reserved); err != nil {
		return err
	}
	if reserved > ss.ReservedTurns {
		return errors.New("托管预算预留计数不一致")
	}
	ss.ReservedTurns -= reserved
	state, outboxState, decisionState := "suspended", "suspended", "stale"
	if stop {
		state, outboxState, decisionState = "cancelled", "cancelled", "cancelled"
	}
	if _, err := tx.ExecContext(ctx, `update supervision_outbox set state=?,updated_at=? where session_id=? and state in ('pending','suspended')`, outboxState, now, ss.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `update supervision_work_items set state=?,turn_state='released',finished_at=? where session_id=? and state in ('queued','suspended')`, state, now, ss.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `update supervision_role_jobs set state='interrupted',error='执行前被控制或代际变化取消' where session_id=? and state='queued' and execution_id in (select execution_id from supervision_work_items where state in ('suspended','cancelled'))`, ss.ID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `update supervision_decisions set state=? where session_id=? and state in ('pending','running','proposed')`, decisionState, ss.ID)
	return err
}

func supervisionHasExecutionTx(ctx context.Context, tx *sql.Tx, id string) (bool, error) {
	var active bool
	err := tx.QueryRowContext(ctx, `select exists(select 1 from supervision_work_items where session_id=? and state in ('launching','running','unknown')) or exists(select 1 from supervision_run_links where session_id=? and process_state<>'quiescent') or exists(select 1 from supervision_executions where session_id=? and state<>'quiescent') or exists(select 1 from supervision_context_captures where session_id=? and state='collecting') or exists(select 1 from supervision_mutations where session_id=? and state in ('applying','unknown'))`, id, id, id, id, id).Scan(&active)
	return active, err
}

func (s *Server) requestSupervisionControl(ctx context.Context, id string, version int64, control string, now time.Time) error {
	if control != "pause" && control != "stop" {
		return errors.New("无效的托管控制动作")
	}
	return s.withSupervisionRequestTx(ctx, id, now, func(tx *sql.Tx, next map[string]WorkspaceAdmission, ss *supervisionSession) error {
		// Automatic pauses may resume after amendments or budget maintenance.
		// They do not already satisfy an explicit user pause. Only an established
		// user intent can bypass version checks as an idempotent control retry.
		sameIntent := ss.Control == control && (control == "stop" || ss.Reason == "user_pause")
		if (control == "stop" && ss.State == "stopped") || (sameIntent && (ss.State == "pausing" || ss.State == "paused" || ss.State == "stopping")) {
			return nil
		}
		if err := requireSupervisionVersion(ss, version); err != nil {
			return err
		}
		if supervisionTerminal(ss.State) || (ss.Control == "stop" && control != "stop") {
			return errSupervisionState
		}
		if ss.State == "draft" {
			if control != "stop" {
				return errSupervisionState
			}
			ss.State, ss.Control, ss.ClockAt = "stopped", "stop", nil
		} else {
			ss.Control = control
			if control == "stop" {
				ss.State = "stopping"
			} else {
				ss.State = "pausing"
			}
			ss.Reason = "user_" + control
			if ss.ClockAt == nil {
				ss.ClockAt = &now
			}
			if err := suspendSupervisionQueuedTx(ctx, tx, ss, control == "stop", now); err != nil {
				return err
			}
		}
		ss.StateVersion++
		return appendSupervisionEventTx(ctx, tx, ss, "control.requested", map[string]string{"control": control}, now)
	})
}

// 只做数据库收敛；取消进程由执行适配器负责。无法确认时保持 pausing/stopping。
func (s *Server) settleSupervisionControl(ctx context.Context, id string, now time.Time) error {
	current, err := loadSupervisionSession(ctx, s.db, id)
	if err != nil {
		return err
	}
	if supervisionTerminal(current.State) {
		return s.releaseTerminalSupervision(ctx, current)
	}
	if current.State == "paused" {
		return nil
	}
	if current.State != "pausing" && current.State != "stopping" {
		return errSupervisionState
	}
	proof, err := s.supervisionTargetQuietForControl(ctx, id)
	if err != nil {
		return err
	}
	err = s.withSupervisionTx(ctx, id, now, func(tx *sql.Tx, next map[string]WorkspaceAdmission, ss *supervisionSession) error {
		if supervisionTerminal(ss.State) || ss.State == "paused" {
			return nil
		}
		if ss.State != "pausing" && ss.State != "stopping" {
			return errSupervisionState
		}
		a, exists := next[ss.AdmissionID]
		if !exists || a.OwnerID != id || a.Epoch != ss.AdmissionEpoch {
			return errWorkspaceAdmissionStale
		}
		if ss.RecoveryState != "ready" || a.RecoveryState != "ready" || s.admissionHasOperationsLocked(a) {
			return errWorkspaceNotQuiescent
		}
		if ss.Control == "stop" {
			// unknown is written only after the local writer returns (or restart
			// recovery). Stop closes that lifecycle without accepting its output.
			// Applying writes remain fenced; any active resource below rolls back
			// this metadata change together with the rest of the transaction.
			result, err := tx.ExecContext(ctx, `update supervision_mutations set state='completed' where session_id=? and state='unknown'`, id)
			if err != nil {
				return err
			}
			if n, err := result.RowsAffected(); err != nil {
				return err
			} else if n > 0 {
				if err := appendSupervisionEventTx(ctx, tx, ss, "files.stopped_unverified", map[string]any{"count": n, "reason": "写入已结束，停止交还；中断成果仍未验收，保留文件与变更记录"}, now); err != nil {
					return err
				}
			}
		}
		if err := supervisionRunsQuiescentTx(ctx, tx, id); err != nil {
			return err
		}
		// 停止只收口已确认静默的资源，不在目录中继续执行或接受新基线。
		// 目录被移动/替换后仍须允许交还；暂停和后续派发继续校验目录身份。
		pathErr := proof.verifyTx(ctx, tx, ss, a)
		if proof != nil && pathErr != nil {
			return pathErr // Stop can ignore path drift, never changed authority.
		}
		if pathErr != nil && ss.Control != "stop" {
			return pathErr
		}
		ss.ClockAt = nil
		ss.LeaseOwner, ss.LeaseExpiresAt = "", nil
		ss.LeaseEpoch++
		if ss.Control == "pause" {
			ss.State = "paused"
		} else if ss.Control == "stop" {
			// 没有实施的任务保留原领域状态；停止不能代办验收。
			ids, err := supervisionTaskIDsTx(ctx, tx, id)
			if err != nil {
				return err
			}
			for _, taskID := range ids {
				var started bool
				if err := tx.QueryRowContext(ctx, `select implementation_started from supervision_tasks where session_id=? and task_id=?`, id, taskID).Scan(&started); err != nil {
					return err
				}
				if started {
					claim, exists, err := taskExecutionClaim(ctx, tx, taskID)
					if err != nil {
						return err
					}
					if !exists || claim.OwnerKind != "supervision" || claim.OwnerID != id {
						return errTaskExecutionClaimed
					}
					if _, err := tx.ExecContext(ctx, `update tasks set status='action_required',updated_at=? where id=?`, now, taskID); err != nil {
						return err
					}
					if err := s.recordTaskEventTx(ctx, tx, taskID, "", "supervision.stopped", map[string]string{"sessionId": id}, now); err != nil {
						return err
					}
				}
			}
			ss.State = "stopped"
			a.State, a.UpdatedAt = "releasing", now
			if _, err := tx.ExecContext(ctx, `update workspace_admissions set state='releasing',updated_at=? where id=?`, now, a.ID); err != nil {
				return err
			}
			next[a.ID] = a
		} else {
			return errSupervisionState
		}
		ss.StateVersion++
		if pathErr != nil {
			if err := appendSupervisionEventTx(ctx, tx, ss, "workspace.stopped_after_path_change", map[string]string{"admissionId": a.ID, "reason": "所有受管执行已确认收敛，停止交还；未接受变化后的目录或成果"}, now); err != nil {
				return err
			}
		}
		return appendSupervisionEventTx(ctx, tx, ss, "session."+ss.State, map[string]string{"reason": ss.Reason}, now)
	})
	if err != nil {
		return err
	}
	ss, err := loadSupervisionSession(ctx, s.db, id)
	if err != nil {
		return err
	}
	if ss.State == "stopped" && ss.AdmissionID != "" {
		return s.releaseTerminalSupervision(ctx, ss)
	}
	return nil
}

func supervisionTimeAvailable(ss *supervisionSession, class string) int64 {
	limit := ss.Budget.MaxActiveMillis
	if class != "final" {
		limit -= ss.Budget.MaxActiveMillis * int64(ss.Budget.FinalReservePercent) / 100
	}
	return limit - ss.ActiveMillis
}

func supervisionTurnsAvailableTx(ctx context.Context, tx *sql.Tx, ss *supervisionSession, class string, reserving bool) (bool, error) {
	count := ss.UsedTurns + ss.ReservedTurns
	if reserving {
		count++
	}
	if count > ss.Budget.MaxAgentTurns || supervisionTimeAvailable(ss, class) <= 0 {
		return false, nil
	}
	if class == "final" {
		return true, nil
	}
	reserve := (ss.Budget.MaxAgentTurns*ss.Budget.FinalReservePercent + 99) / 100
	var workTurns int
	if err := tx.QueryRowContext(ctx, `select count(*) from supervision_work_items where session_id=? and budget_class='work' and turn_state in ('reserved','consumed')`, ss.ID).Scan(&workTurns); err != nil {
		return false, err
	}
	if reserving {
		workTurns++
	}
	return workTurns <= ss.Budget.MaxAgentTurns-reserve, nil
}

func pauseSupervisionForBudgetTx(ctx context.Context, tx *sql.Tx, ss *supervisionSession, now time.Time) error {
	// Budget maintenance must not replace an existing user pause with an
	// automatically resumable budget pause (nor override stop).
	if ss.Control != "" {
		return nil
	}
	ss.Control, ss.State, ss.Reason = "pause", "pausing", "budget_exhausted"
	if err := suspendSupervisionQueuedTx(ctx, tx, ss, false, now); err != nil {
		return err
	}
	ss.StateVersion++
	return appendSupervisionEventTx(ctx, tx, ss, "budget.exhausted", map[string]any{"activeMillis": ss.ActiveMillis, "usedTurns": ss.UsedTurns}, now)
}

func (s *Server) updateSupervisionBudget(ctx context.Context, id string, version int64, budget supervisionBudget, now time.Time) error {
	if err := budget.validate(); err != nil {
		return err
	}
	return s.withSupervisionRequestTx(ctx, id, now, func(tx *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
		if err := requireSupervisionVersion(ss, version); err != nil {
			return err
		}
		if supervisionTerminal(ss.State) || ss.Control == "stop" {
			return errSupervisionState
		}
		if ss.Budget == budget {
			return nil
		}
		ss.Budget = budget
		ss.StateVersion++
		if err := suspendSupervisionQueuedTx(ctx, tx, ss, false, now); err != nil {
			return err
		}
		// The persisted provenance survives an explicit pause or restart. Only
		// tasks deferred by the budget may reopen when work becomes affordable.
		workAvailable, err := supervisionTurnsAvailableTx(ctx, tx, ss, "work", true)
		if err != nil {
			return err
		}
		if workAvailable {
			reopened, err := tx.ExecContext(ctx, `update supervision_tasks set phase='pending' where session_id=? and phase='deferred' and task_id in (select task_id from supervision_budget_deferred where session_id=?)`, id, id)
			if err != nil {
				return err
			}
			count, err := reopened.RowsAffected()
			if err != nil {
				return err
			}
			if count > 0 {
				ss.RunPhase = "running"
			}
			if _, err := tx.ExecContext(ctx, `delete from supervision_budget_deferred where session_id=?`, id); err != nil {
				return err
			}
			if ss.State == "finalizing" && ss.Reason == "work_budget_exhausted" && ss.Control == "" {
				ss.State, ss.Reason = "running", ""
			}
		}
		class := "work"
		if ss.State == "finalizing" {
			class = "final"
		}
		available, err := supervisionTurnsAvailableTx(ctx, tx, ss, class, true)
		if err != nil {
			return err
		}
		if !available && ss.State != "draft" {
			if err := pauseSupervisionForBudgetTx(ctx, tx, ss, now); err != nil {
				return err
			}
		}
		return appendSupervisionEventTx(ctx, tx, ss, "budget.changed", budget, now)
	})
}

// 周期核对可在没有模型输出时触发预算收敛。实际取消由执行适配器消费 control。
func (s *Server) checkSupervisionBudget(ctx context.Context, id string, now time.Time) error {
	return s.withSupervisionTx(ctx, id, now, func(tx *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
		if ss.State != "running" && ss.State != "finalizing" {
			return nil
		}
		if ss.Control != "" {
			return nil
		}
		class := "work"
		if ss.State == "finalizing" {
			class = "final"
		}
		// 用完次数只禁止下一次调用；正在执行的最后一轮仍可正常交回结果。
		if supervisionTimeAvailable(ss, class) <= 0 {
			return pauseSupervisionForBudgetTx(ctx, tx, ss, now)
		}
		active, err := supervisionHasExecutionTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if active {
			return nil
		}
		var reserved bool
		if err := tx.QueryRowContext(ctx, `select exists(select 1 from supervision_work_items where session_id=? and state='queued' and turn_state='reserved')`, id).Scan(&reserved); err != nil {
			return err
		}
		// 已预留的最后一轮仍可启动；补查不能再为它虚加一轮而提前暂停。
		available, err := supervisionTurnsAvailableTx(ctx, tx, ss, class, !reserved)
		if err != nil {
			return err
		}
		if !available {
			if entered, err := enterSupervisionFinalReserveTx(ctx, tx, ss, now); err != nil || entered {
				return err
			}
			return pauseSupervisionForBudgetTx(ctx, tx, ss, now)
		}
		return nil
	})
}

// 必须在 worker 和普通 Run 恢复前执行。这里只保留事实，不宣称进程已结束。
func (s *Server) recoverSupervisionSessions(ctx context.Context, now time.Time) error {
	rows, err := s.db.QueryContext(ctx, `select id from supervision_sessions where state not in ('draft','stopped','completed','completed_with_issues','failed')`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, id := range ids {
		err := s.withWorkspaceAdmissionTx(ctx, func(tx *sql.Tx, next map[string]WorkspaceAdmission) error {
			ss, err := loadSupervisionSession(ctx, tx, id)
			if err != nil {
				return err
			}
			a, exists := next[ss.AdmissionID]
			if !exists || a.OwnerID != id {
				return errors.New("活动托管缺少目录准入，不能恢复派发")
			}
			// 采集只执行本进程的文件读取；重启不重放，旧内存结果由 epoch 拒绝。
			if _, err := tx.ExecContext(ctx, `update supervision_context_captures set state='failed',completed_at=? where session_id=? and state='collecting'`, now, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `update supervision_mutations set state='unknown' where session_id=? and state='applying'; update supervision_role_jobs set state='unknown' where session_id=? and state='running'`, id, id); err != nil {
				return err
			}
			active, err := supervisionHasExecutionTx(ctx, tx, id)
			if err != nil {
				return err
			}
			if active {
				accountSupervisionTime(&ss, now)
			} // 无法确认停机期间的执行，保守计时。
			ss.ClockAt = nil
			ss.AdmissionEpoch = a.Epoch
			ss.LeaseEpoch++
			ss.LeaseOwner, ss.LeaseExpiresAt = "", nil
			ss.RecoveryState = "reconciling"
			if active {
				ss.RecoveryState = "outcome_unknown"
				ss.ClockAt = &now
			}
			// 预留与消费以持久工作项账本为准，不因服务重启或汇总计数缺失重置。
			if err := tx.QueryRowContext(ctx, `select coalesce(sum(case when turn_state='consumed' then 1 else 0 end),0),coalesce(sum(case when turn_state='reserved' then 1 else 0 end),0) from supervision_work_items where session_id=?`, id).Scan(&ss.UsedTurns, &ss.ReservedTurns); err != nil {
				return err
			}
			if err := suspendSupervisionQueuedTx(ctx, tx, &ss, ss.Control == "stop", now); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `update supervision_work_items set state='unknown' where session_id=? and state in ('launching','running')`, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `update supervision_executions set state='unknown',updated_at=? where session_id=? and state<>'quiescent'`, now, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `update supervision_outbox set state='unknown',updated_at=? where session_id=? and state in ('launching','delivered')`, now, id); err != nil {
				return err
			}
			ss.StateVersion++
			if err := appendSupervisionEventTx(ctx, tx, &ss, "session.recovery_required", map[string]any{"recoveryState": ss.RecoveryState, "epoch": ss.LeaseEpoch}, now); err != nil {
				return err
			}
			return saveSupervisionSessionTx(ctx, tx, &ss)
		})
		if err != nil {
			return err
		}
	}
	return nil
}
