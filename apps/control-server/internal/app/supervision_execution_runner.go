package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// 这是单次决策的可信执行服务，不是自动调度 worker，也不应用模型建议。
func (s *Server) executeSupervisionScheduler(ctx context.Context, permit supervisionExecutionPermit, deadline time.Time, runner supervisionCLIAdapter) error {
	if err := s.requireSupervisionLocalExecution(ctx, permit.Decision.SessionID); err != nil {
		return err
	}
	if runner == nil {
		return errors.New("缺少受控托管 CLI 适配器")
	}
	err := s.withSupervisionTx(ctx, permit.Decision.SessionID, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
		state, err := checkSupervisionExecutionTx(ctx, tx, ss, permit)
		if err != nil {
			return err
		}
		if state != "prepared" {
			return errSupervisionState
		}
		var started time.Time
		var class string
		if err := tx.QueryRowContext(ctx, `select started_at,budget_class from supervision_work_items where execution_id=?`, permit.Decision.ExecutionID).Scan(&started, &class); err != nil {
			return err
		}
		turnDeadline := started.Add(time.Duration(ss.Budget.MaxTurnMillis) * time.Millisecond)
		totalDeadline := time.Now().Add(time.Duration(supervisionTimeAvailable(ss, class)) * time.Millisecond)
		if deadline.IsZero() || turnDeadline.Before(deadline) {
			deadline = turnDeadline
		}
		if totalDeadline.Before(deadline) {
			deadline = totalDeadline
		}
		// 同一个单次令牌只能有一个执行器认领，重复唤醒不能竞争同一命名容器并误停原执行。
		_, err = tx.ExecContext(ctx, `update supervision_executions set state='starting',updated_at=? where execution_id=?`, time.Now().UTC(), permit.Decision.ExecutionID)
		return err
	})
	if err != nil {
		return err
	}
	input, _, err := s.supervisionSchedulerInput(ctx, permit.Decision.ExecutionID)
	if err != nil {
		settleCtx, settleCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer settleCancel()
		return errors.Join(err, s.finishSupervisionExecution(settleCtx, permit, supervisionSchedulerCLIReceipt{ContainerQuiet: true}, err, time.Now().UTC()))
	}
	runCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	// 与档案吊销使用相同的 gate：先校验版本并登记取消句柄，再释放准入锁。
	if permit.Profile != nil {
		err = s.admitSupervisionTargetProfile(runCtx, permit, cancel)
		if err != nil {
			_ = s.finishSupervisionExecution(context.Background(), permit, supervisionSchedulerCLIReceipt{ContainerQuiet: true}, err, time.Now().UTC())
			return err
		}
		defer s.unregisterProfileRunCancel(permit.RunID)
	}
	sink := &supervisionRoleSink{server: s, permit: permit, cancel: cancel}
	s.beginRunUsage(permit.RunID, permit.ConversationID)
	s.seedRunUsageModel(permit.RunID, permit.AgentID, runModel(permit.Model, permit.Profile), runner)
	monitorDone, monitorStopped := make(chan struct{}), make(chan struct{})
	var monitorErr error
	go func() {
		defer close(monitorStopped)
		monitorErr = s.monitorSupervisionExecution(runCtx, permit, cancel, monitorDone)
	}()
	var specRecorded atomic.Bool
	receipt, runErr := runner.runSupervisionScheduler(runCtx, supervisionSchedulerCLIRequest{
		Input: input, Profile: permit.Profile, Model: runModel(permit.Model, permit.Profile), ReasoningEffort: permit.ReasoningEffort, Container: permit.Container,
		SystemPrompt: supervisionRolePrompt(permit.Role),
		Resolved: func(info supervisionCLIInfo) error {
			err := s.recordSupervisionExecutionSpec(runCtx, permit, input, info)
			if err == nil {
				specRecorded.Store(true)
			}
			return err
		},
		Handshake: func(id supervisionProcessIdentity, resume func() error) error {
			if !specRecorded.Load() {
				return errors.New("CLI 尚未持久化实际执行规格，禁止启动")
			}
			if err := s.registerSupervisionProcess(runCtx, permit, id, time.Now().UTC()); err != nil {
				return err
			}
			return s.resumeSupervisionProcess(runCtx, permit, resume, time.Now().UTC())
		},
	}, sink)
	if runErr == nil && !specRecorded.Load() {
		runErr = errors.New("托管 CLI 缺少实际执行规格")
	}
	if receipt.ContainerQuiet {
		s.stopQuotaLeaseHeartbeat(permit.RunID)
	}
	close(monitorDone)
	<-monitorStopped
	runErr = errors.Join(runErr, monitorErr, sink.failure())
	usageStatus := "running"
	if receipt.ContainerQuiet {
		usageStatus = "completed"
		if runErr != nil {
			usageStatus = "failed"
		}
	}
	runErr = errors.Join(runErr, s.persistRunUsage(permit.RunID, usageStatus))
	settleCtx, settleCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer settleCancel()
	if err := s.finishSupervisionExecution(settleCtx, permit, receipt, runErr, time.Now().UTC()); err != nil {
		return errors.Join(runErr, err)
	}
	if receipt.ContainerQuiet {
		s.discardRunUsage(permit.RunID)
	}
	if permit.Role == "implementation" || permit.Role == "review" {
		return s.completeSupervisionRole(settleCtx, permit, receipt.Output, runErr)
	}
	if runErr == nil || receipt.Output != "" {
		if _, err := s.recordSupervisionDecisionOutput(settleCtx, permit.Decision.ID, permit.Decision.ExecutionID, receipt.Output, time.Now().UTC()); err != nil {
			return errors.Join(runErr, err)
		}
	}
	return runErr
}

func supervisionRolePrompt(role string) string {
	switch role {
	case "implementation":
		return supervisionImplementationPrompt
	case "review":
		return supervisionReviewPrompt
	default:
		return supervisionSchedulerSystemPrompt
	}
}

func (s *Server) monitorSupervisionExecution(ctx context.Context, permit supervisionExecutionPermit, cancel context.CancelFunc, done <-chan struct{}) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	nextLease := time.Now().Add(10 * time.Second)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-done:
			return nil
		case <-ticker.C:
		}
		probeCtx, release := context.WithTimeout(ctx, 3*time.Second)
		stage := "读取会话"
		ss, err := loadSupervisionSession(probeCtx, s.db, permit.Decision.SessionID)
		if err == nil && (ss.LeaseEpoch != permit.Lease.Epoch || ss.LeaseOwner != permit.Lease.Owner || ss.RecoveryState != "ready" || ss.Control == "stop" || ss.Reason == "budget_exhausted" || ss.Reason == "amendment_interrupt" || ss.Reason == "storage_limit" || ss.Reason == "disk_space_low") {
			stage = "核对授权和控制状态"
			err = errSupervisionLease
		}
		if err == nil {
			stage = "核对执行时间预算"
			// An explicit pause lets a running turn finish, but must not let
			// it exceed a subsequently reduced time budget. Its deadline was
			// computed before that change; check current limits independently
			// of the pause reason and preserve the user's control intent.
			now := time.Now().UTC()
			accountSupervisionTime(&ss, now)
			var started time.Time
			var class string
			err = s.db.QueryRowContext(probeCtx, `select started_at,budget_class from supervision_work_items where execution_id=?`, permit.Decision.ExecutionID).Scan(&started, &class)
			if err == nil && (supervisionTimeAvailable(&ss, class) <= 0 || !now.Before(started.Add(time.Duration(ss.Budget.MaxTurnMillis)*time.Millisecond))) {
				err = errSupervisionBudget
			}
		}
		if err == nil && ss.Control == "" && time.Now().After(nextLease) {
			stage = "续期调度租约"
			err = s.renewSupervisionExecutionLease(probeCtx, permit, time.Now().UTC())
			nextLease = time.Now().Add(10 * time.Second)
		}
		release()
		// 活动回合已按其 budget_class 核对最新时间限制；次数只限制下一轮。
		// 不再每秒调用面向派发的完整预算事务，也不让存储统计消耗租约续期的期限。
		if err == nil {
			stage = "核对存储预算"
			storageCtx, storageCancel := context.WithTimeout(ctx, 3*time.Second)
			err = s.checkSupervisionStorage(storageCtx, ss.ID)
			storageCancel()
		}
		if err != nil {
			cancel()
			return fmt.Errorf("托管执行监测（%s）失败：%w", stage, err)
		}
	}
}

// 心跳只更新已授权活动执行的时间事实，不产生不断增长的模型历史或存储统计。
// 仍在共享控制事务内核对当前所有者和代际，停止及接管不会被旧心跳覆盖。
func (s *Server) renewSupervisionExecutionLease(ctx context.Context, permit supervisionExecutionPermit, now time.Time) error {
	return s.withWorkspaceAdmissionTx(ctx, func(tx *sql.Tx, _ map[string]WorkspaceAdmission) error {
		ss, err := loadSupervisionSession(ctx, tx, permit.Decision.SessionID)
		if err != nil {
			return err
		}
		state, err := checkSupervisionExecutionTx(ctx, tx, &ss, permit)
		if err != nil {
			return err
		}
		if ss.Control == "pause" {
			return nil // 显式暂停允许本回合结束，不再授予后续执行。
		}
		if ss.Control != "" || (ss.State != "running" && ss.State != "finalizing") || (state != "starting" && state != "created" && state != "running") {
			return errSupervisionState
		}
		if err := requireSupervisionLease(&ss, permit.Lease, now); err != nil {
			return err
		}
		if err := requireSupervisionNoPendingDrift(ctx, tx, ss.ID); err != nil {
			return err
		}
		accountSupervisionTime(&ss, now)
		_, err = tx.ExecContext(ctx, `update supervision_sessions set lease_expires_at=?,active_millis=?,clock_at=?,updated_at=? where id=?`, now.Add(30*time.Second), ss.ActiveMillis, ss.ClockAt, now, ss.ID)
		return err
	})
}

// 专用 sink 持久化失败时取消执行；不能像普通展示事件那样仅记日志继续推进验收。
type supervisionRoleSink struct {
	server *Server
	permit supervisionExecutionPermit
	cancel context.CancelFunc
	mu     sync.Mutex
	err    error
}

func (sink *supervisionRoleSink) failure() error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return sink.err
}

func (sink *supervisionRoleSink) persist(fn func(context.Context, *sql.Tx, *supervisionSession) error) {
	if sink.failure() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := sink.server.withSupervisionTx(ctx, sink.permit.Decision.SessionID, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
		state, err := checkSupervisionExecutionTx(ctx, tx, ss, sink.permit)
		if err != nil {
			return err
		}
		if state != "created" && state != "running" {
			return errSupervisionState
		}
		return fn(ctx, tx, ss)
	})
	if err != nil {
		sink.mu.Lock()
		if sink.err == nil {
			sink.err = fmt.Errorf("托管过程持久化失败：%w", err)
		}
		sink.mu.Unlock()
		sink.cancel()
	}
}

func (sink *supervisionRoleSink) Event(typ string, payload json.RawMessage) {
	// 与普通会话的 appendEvent 保持一致：逐 token 的估算计数只用于实时提示，
	// 不属于过程正文或计费用量。逐条落库会让每次准入反复扫描不断增长的历史，
	// 最终阻塞同一数据库连接上的控制与续租；真实正文和终结用量仍须持久化。
	if isThinkingTokensEvent(typ, payload) {
		if sink.failure() == nil {
			event := Event{ID: uuid.NewString(), ConversationID: sink.permit.ConversationID, RunID: sink.permit.RunID, Type: typ, Payload: payload, CreatedAt: time.Now().UTC()}
			raw, _ := json.Marshal(event)
			sink.server.enqueueConversationEvent(event.ConversationID, raw)
			sink.server.collectUsageEvent(event.RunID, event.ConversationID, typ, payload)
		}
		return
	}
	var event Event
	sink.persist(func(ctx context.Context, tx *sql.Tx, ss *supervisionSession) error {
		event = Event{ID: uuid.NewString(), ConversationID: sink.permit.ConversationID, RunID: sink.permit.RunID, Type: typ, Payload: payload, CreatedAt: time.Now().UTC()}
		if _, err := tx.ExecContext(ctx, `insert into events(id,conversation_id,run_id,type,payload,created_at) values (?,?,?,?,?,?)`, event.ID, event.ConversationID, event.RunID, event.Type, string(event.Payload), event.CreatedAt); err != nil {
			return err
		}
		if sink.server.remoteRelayConfigured() {
			if err := sink.server.enqueueRemoteEventTxWithConversation(ctx, tx, event.ID, "", event.RunID, event.ConversationID, event.Type, event.Payload, event.CreatedAt); err != nil {
				return err
			}
		}
		return nil
	})
	if sink.failure() == nil {
		raw, _ := json.Marshal(event)
		sink.server.enqueueConversationEvent(event.ConversationID, raw)
		sink.server.collectUsageEvent(event.RunID, event.ConversationID, typ, payload)
		sink.server.wakeRemoteOutbox()
	}
}

func (sink *supervisionRoleSink) AssistantText(content, parent string) {
	var message Message
	sink.persist(func(ctx context.Context, tx *sql.Tx, ss *supervisionSession) error {
		message = Message{ID: uuid.NewString(), ConversationID: sink.permit.ConversationID, RunID: sink.permit.RunID, Role: "assistant", Content: content, ParentToolUseID: parent, CreatedAt: time.Now().UTC()}
		_, err := tx.ExecContext(ctx, `insert into messages(id,conversation_id,run_id,role,content,parent_tool_use_id,created_at) values (?,?,?,'assistant',?,?,?)`, message.ID, message.ConversationID, message.RunID, content, parent, message.CreatedAt)
		return err
	})
	if sink.failure() == nil {
		sink.Event("assistant.message", mustJSON(message))
	}
}

func (sink *supervisionRoleSink) SessionIdentified(id string) {
	sink.persist(func(ctx context.Context, tx *sql.Tx, _ *supervisionSession) error {
		if _, err := tx.ExecContext(ctx, `update runs set agent_run_id=? where id=?`, id, sink.permit.RunID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `update conversations set agent_session_id=?,claude_session_id=? where id=?`, id, id, sink.permit.ConversationID)
		return err
	})
}

func (sink *supervisionRoleSink) SessionInitialized() {
	sink.persist(func(ctx context.Context, tx *sql.Tx, _ *supervisionSession) error {
		_, err := tx.ExecContext(ctx, `update conversations set agent_initialized=1,claude_initialized=1 where id=?`, sink.permit.ConversationID)
		return err
	})
}
