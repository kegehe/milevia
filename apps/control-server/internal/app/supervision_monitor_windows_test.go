//go:build windows

package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSupervisionRoleTelemetryDoesNotBlockControlOrPersist(t *testing.T) {
	s, _, permit, now, _ := supervisionExecutionForTest(t)
	registerAndResumeExecutionForTest(t, s, permit, now)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink := &supervisionRoleSink{server: s, permit: permit, cancel: cancel}
	// 模拟长回合的高频计数；独占连接时仍须立即返回，证明没有进入存储准入事务。
	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20000; i++ {
			if ctx.Err() != nil {
				return
			}
			sink.Event("system", json.RawMessage(`{"type":"system","subtype":"thinking_tokens","estimated_tokens":42}`))
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		cancel()
		conn.Close()
		<-done
		t.Fatal("token telemetry waited for the database/control connection")
	}
	conn.Close()
	if sink.failure() != nil || ctx.Err() != nil {
		t.Fatalf("telemetry interrupted execution: %v / %v", sink.failure(), ctx.Err())
	}
	var count int
	if err := s.db.QueryRow(`select count(*) from events where run_id=? and json_extract(payload,'$.subtype')='thinking_tokens'`, permit.RunID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("token telemetry persisted: %d / %v", count, err)
	}
	// 正文及最终用量不能因遥测过滤而丢失，且仍受执行授权核对。
	sink.AssistantText("实际审查正文", "")
	sink.Event("result", json.RawMessage(`{"type":"result","usage":{"input_tokens":10,"output_tokens":20}}`))
	if sink.failure() != nil {
		t.Fatal(sink.failure())
	}
	var content string
	if err := s.db.QueryRow(`select content from messages where run_id=? and role='assistant'`, permit.RunID).Scan(&content); err != nil || content != "实际审查正文" {
		t.Fatalf("assistant content was lost: %q / %v", content, err)
	}
	if err := s.db.QueryRow(`select count(*) from events where run_id=? and type='result'`, permit.RunID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("final usage was lost: %d / %v", count, err)
	}
}

type supervisionMonitorAdapterForTest struct {
	AgentRunner
	started chan struct{}
}

func (a *supervisionMonitorAdapterForTest) supervisionCLIInfo(context.Context, bool) (supervisionCLIInfo, error) {
	return supervisionCLIInfo{}, nil
}

func (a *supervisionMonitorAdapterForTest) runSupervisionScheduler(ctx context.Context, req supervisionSchedulerCLIRequest, _ AgentRunSink) (supervisionSchedulerCLIReceipt, error) {
	binary, err := os.Executable()
	if err == nil {
		err = req.Resolved(supervisionCLIInfo{AgentID: "claude-code", Path: binary, Version: "test", Contract: "monitor-test-v1"})
	}
	close(a.started)
	if err == nil {
		<-ctx.Done()
		err = ctx.Err()
	}
	return supervisionSchedulerCLIReceipt{ContainerQuiet: true}, err
}

func TestSupervisionMonitorPersistsCancellationCause(t *testing.T) {
	s, _, permit, _, _ := supervisionExecutionForTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	adapter := &supervisionMonitorAdapterForTest{AgentRunner: s.runner, started: make(chan struct{})}
	result := make(chan error, 1)
	go func() { result <- s.executeSupervisionScheduler(ctx, permit, time.Now().Add(time.Minute), adapter) }()
	select {
	case <-adapter.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// 持有唯一数据库连接，确定性触发监控探测超时，而非取消父 context。
	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	timer := time.NewTimer(4500 * time.Millisecond)
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
	conn.Close()
	var runErr error
	select {
	case runErr = <-result:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if !errors.Is(runErr, context.DeadlineExceeded) || !strings.Contains(runErr.Error(), "托管执行监测（读取会话）失败") {
		t.Fatalf("monitor cancellation lost its actual cause: %v", runErr)
	}
	var stored string
	if err := s.db.QueryRow(`select error from supervision_executions where execution_id=?`, permit.Decision.ExecutionID).Scan(&stored); err != nil || !strings.Contains(stored, "context deadline exceeded") {
		t.Fatalf("diagnosis absent from persisted receipt: %q %v", stored, err)
	}
	if !supervisionTransientError(stored) || supervisionTransientError(context.Canceled.Error()) {
		t.Fatal("only diagnosed transient failures may enter bounded retry")
	}
	if err := s.scheduleSupervisionRetry(ctx, permit.Decision.SessionID); err != nil {
		t.Fatal(err)
	}
	ss := supervisionSessionForTest(t, s, permit.Decision.SessionID)
	if ss.State != "running" || ss.Reason != "transient_retry_wait" {
		t.Fatalf("diagnosed probe timeout did not enter bounded retry: %+v", ss)
	}
}

func TestSupervisionMonitorControlCancellationIsNotTransient(t *testing.T) {
	s, _, permit, now, _ := supervisionExecutionForTest(t)
	registerAndResumeExecutionForTest(t, s, permit, now)
	ss := supervisionSessionForTest(t, s, permit.Decision.SessionID)
	if err := s.requestSupervisionControl(context.Background(), ss.ID, ss.StateVersion, "stop", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := s.monitorSupervisionExecution(ctx, permit, cancel, make(chan struct{}))
	if !errors.Is(err, errSupervisionLease) || !strings.Contains(err.Error(), "核对授权和控制状态") || supervisionTransientError(err.Error()) {
		t.Fatalf("explicit stop was hidden or treated as retryable: %v", err)
	}
}

func TestSupervisionExecutionHeartbeatPreservesControlAndDoesNotGrowHistory(t *testing.T) {
	for _, action := range []string{"renew", "pause", "stop", "expired", "old_epoch"} {
		t.Run(action, func(t *testing.T) {
			s, _, permit, now, _ := supervisionExecutionForTest(t)
			registerAndResumeExecutionForTest(t, s, permit, now)
			ctx := context.Background()
			ss := supervisionSessionForTest(t, s, permit.Decision.SessionID)
			if action == "pause" || action == "stop" {
				if err := s.requestSupervisionControl(ctx, ss.ID, ss.StateVersion, action, now); err != nil {
					t.Fatal(err)
				}
			} else if action == "old_epoch" {
				if _, err := s.db.Exec(`update supervision_sessions set lease_epoch=lease_epoch+1 where id=?`, ss.ID); err != nil {
					t.Fatal(err)
				}
			}
			before := supervisionSessionForTest(t, s, ss.ID)
			at := now.Add(5 * time.Second)
			if action == "expired" {
				at = before.LeaseExpiresAt.Add(time.Second)
			}
			err := s.renewSupervisionExecutionLease(ctx, permit, at)
			wantError := action == "stop" || action == "expired" || action == "old_epoch"
			if (err != nil) != wantError {
				t.Fatalf("%s heartbeat error=%v", action, err)
			}
			after := supervisionSessionForTest(t, s, ss.ID)
			if after.EventSeq != before.EventSeq || after.StateVersion != before.StateVersion || after.State != before.State || after.Control != before.Control || after.LeaseEpoch != before.LeaseEpoch || after.UsedTurns != before.UsedTurns {
				t.Fatal("heartbeat changed business facts, control intent or history")
			}
			wantExpiry := *before.LeaseExpiresAt
			if action == "renew" {
				wantExpiry = at.Add(30 * time.Second)
			}
			if after.LeaseExpiresAt == nil || !after.LeaseExpiresAt.Equal(wantExpiry) {
				t.Fatal("heartbeat extended a revoked/expired execution or failed to renew")
			}
		})
	}
}
