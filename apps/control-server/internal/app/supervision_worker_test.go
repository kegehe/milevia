package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func supervisionWorkerForTest(s *Server, execute supervisionDecisionExecutor) *supervisionWorker {
	return &supervisionWorker{server: s, owner: "worker-test", execute: execute, active: map[string]bool{}}
}

// 领域调度替身，不模拟真实 CLI 隔离；进程及单次握手另有 Windows/显式 CLI 实验。
func finishWorkerDecisionForTest(s *Server, d supervisionDecision, lease supervisionLease, output string) error {
	ctx := context.Background()
	if _, err := s.claimSupervisionDecisionLaunch(ctx, d.ID, lease, time.Now().UTC()); err != nil {
		return err
	}
	return completeWorkerDecisionForTest(s, d, lease, output)
}

func completeWorkerDecisionForTest(s *Server, d supervisionDecision, lease supervisionLease, output string) error {
	ctx := context.Background()
	if _, err := s.recordSupervisionDecisionOutput(ctx, d.ID, d.ExecutionID, output, time.Now().UTC()); err != nil {
		return err
	}
	return s.confirmSupervisionDecisionQuiescent(ctx, d.ID, d.ExecutionID, lease.Epoch, time.Now().UTC())
}

func waitSupervisionWorkerForTest(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for !ready() {
		select {
		case <-deadline.C:
			t.Fatal("worker did not reach expected state")
		case <-ticker.C:
		}
	}
}

func TestSupervisionWorkerRetriesFormatWithFrozenCorrectionAndStopsAtLimit(t *testing.T) {
	for _, validThird := range []bool{true, false} {
		t.Run(map[bool]string{true: "third_valid", false: "third_invalid"}[validThird], func(t *testing.T) {
			s, _, taskID, ss, now := supervisionFixture(t, defaultSupervisionBudget())
			startSupervisionForTest(t, s, ss.ID, now)
			var calls int
			var firstInput []byte
			var firstExecution string
			w := supervisionWorkerForTest(s, func(ctx context.Context, d supervisionDecision, lease supervisionLease) error {
				calls++
				raw, _, err := s.supervisionSchedulerInput(ctx, d.ExecutionID)
				if err != nil {
					return err
				}
				var input supervisionSchedulerInput
				if err := json.Unmarshal(raw, &input); err != nil {
					return err
				}
				if calls == 1 {
					firstInput, firstExecution = raw, d.ExecutionID
					if input.Correction != nil {
						return errors.New("first attempt has a correction")
					}
				} else if input.Correction == nil || input.Correction.Attempt != calls-1 || input.Correction.Output != "invalid output" || input.Correction.ValidationError == "" || input.Correction.ExecutionID == d.ExecutionID {
					return errors.New("retry lacks previous invalid output and validation error")
				}
				output := "invalid output"
				if calls == 3 && validThird {
					output = decisionProposalForTest(d)
				}
				return finishWorkerDecisionForTest(s, d, lease, output)
			})
			if err := w.process(context.Background(), ss.ID); err != nil {
				t.Fatal(err)
			}
			if calls != 3 {
				t.Fatalf("calls=%d", calls)
			}
			current := supervisionSessionForTest(t, s, ss.ID)
			if current.UsedTurns != 3 || current.ReservedTurns != 0 || (!validThird && (current.State != "needs_input" || current.Reason != "invalid_scheduler_output")) {
				t.Fatalf("retry state=%+v", current)
			}
			later, _, err := s.supervisionSchedulerInput(context.Background(), firstExecution)
			if err != nil || !bytes.Equal(firstInput, later) {
				t.Fatal("retry mutated original observation")
			}
			if _, err := s.db.Exec(`update supervision_sessions set lease_expires_at=? where id=?`, now.Add(-time.Hour), ss.ID); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 5; i++ {
				if err := w.process(context.Background(), ss.ID); err != nil {
					t.Fatal(err)
				}
			}
			if calls != 3 {
				t.Fatal("duplicate wake or lease expiry bypassed attempt limit")
			}
			var decisions, quiescent int
			if err := s.db.QueryRow(`select count(*) from supervision_decisions where session_id=?`, ss.ID).Scan(&decisions); err != nil || decisions != 1 {
				t.Fatalf("decisions=%d err=%v", decisions, err)
			}
			if err := s.db.QueryRow(`select count(*) from supervision_work_items where session_id=? and state='quiescent' and turn_state='consumed'`, ss.ID).Scan(&quiescent); err != nil || quiescent != 3 {
				t.Fatalf("attempt ledger=%d err=%v", quiescent, err)
			}
			assertExecutionClaimForTest(t, s, taskID, "supervision", ss.ID)
		})
	}
}

func TestSupervisionWorkerBudgetCanStopFormatCorrection(t *testing.T) {
	budget := defaultSupervisionBudget()
	budget.MaxAgentTurns = 2 // 一轮 work，一轮 final。
	s, _, _, ss, now := supervisionFixture(t, budget)
	startSupervisionForTest(t, s, ss.ID, now)
	var calls int
	w := supervisionWorkerForTest(s, func(_ context.Context, d supervisionDecision, lease supervisionLease) error {
		calls++
		return finishWorkerDecisionForTest(s, d, lease, "")
	})
	if err := w.process(context.Background(), ss.ID); err != nil {
		t.Fatal(err)
	}
	current := supervisionSessionForTest(t, s, ss.ID)
	if calls != 1 || current.State != "paused" || current.Reason != "budget_exhausted" || current.UsedTurns != 1 || current.ReservedTurns != 0 {
		t.Fatalf("budget calls=%d state=%+v", calls, current)
	}
}

func TestSupervisionWorkerDoesNotChargeQueuedReservationTwice(t *testing.T) {
	budget := defaultSupervisionBudget()
	budget.MaxAgentTurns = 2
	s, _, _, ss, now := supervisionFixture(t, budget)
	startSupervisionForTest(t, s, ss.ID, now)
	var calls int
	w := supervisionWorkerForTest(s, func(_ context.Context, d supervisionDecision, lease supervisionLease) error {
		calls++
		return finishWorkerDecisionForTest(s, d, lease, decisionProposalForTest(d))
	})
	lease, err := s.acquireSupervisionLease(context.Background(), ss.ID, w.owner, now)
	if err != nil {
		t.Fatal(err)
	}
	current := supervisionSessionForTest(t, s, ss.ID)
	if _, err := s.prepareSupervisionDecision(context.Background(), ss.ID, current.StateVersion, lease, "", now); err != nil {
		t.Fatal(err)
	}
	if err := w.process(context.Background(), ss.ID); err != nil {
		t.Fatal(err)
	}
	current = supervisionSessionForTest(t, s, ss.ID)
	if calls != 1 || current.UsedTurns != 1 || current.ReservedTurns != 0 {
		t.Fatalf("last reserved turn did not execute: calls=%d state=%+v", calls, current)
	}
}

func TestSupervisionWorkerCoalescesConcurrentWakeAndAppliesWaitWithoutInventingAcceptance(t *testing.T) {
	s, _, taskID, ss, now := supervisionFixture(t, defaultSupervisionBudget())
	startSupervisionForTest(t, s, ss.ID, now)
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	w := supervisionWorkerForTest(s, func(_ context.Context, d supervisionDecision, lease supervisionLease) error {
		calls.Add(1)
		close(started)
		<-release
		return finishWorkerDecisionForTest(s, d, lease, decisionProposalForTest(d))
	})
	w.start(s.runtimeCtx, ss.ID)
	<-started
	var wakes sync.WaitGroup
	for i := 0; i < 20; i++ {
		wakes.Add(1)
		go func() { defer wakes.Done(); w.start(s.runtimeCtx, ss.ID) }()
	}
	wakes.Wait()
	unblock()
	waitSupervisionWorkerForTest(t, func() bool { w.mu.Lock(); defer w.mu.Unlock(); return !w.active[ss.ID] })
	if err := w.process(context.Background(), ss.ID); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("duplicate wakes launched another round")
	}
	var state, taskStatus string
	if err := s.db.QueryRow(`select state from supervision_decisions where session_id=?`, ss.ID).Scan(&state); err != nil || state != "applied" {
		t.Fatalf("proposal=%s err=%v", state, err)
	}
	if err := s.db.QueryRow(`select status from tasks where id=?`, taskID).Scan(&taskStatus); err != nil || taskStatus == "done" {
		t.Fatalf("worker invented acceptance: %s %v", taskStatus, err)
	}
	assertExecutionClaimForTest(t, s, taskID, "supervision", ss.ID)
}

func TestSupervisionWorkerControlDuringRoundSettlesWithoutNextDispatch(t *testing.T) {
	for _, control := range []string{"pause", "stop"} {
		t.Run(control, func(t *testing.T) {
			s, _, taskID, ss, now := supervisionFixture(t, defaultSupervisionBudget())
			startSupervisionForTest(t, s, ss.ID, now)
			w := supervisionWorkerForTest(s, func(ctx context.Context, d supervisionDecision, lease supervisionLease) error {
				if _, err := s.claimSupervisionDecisionLaunch(ctx, d.ID, lease, time.Now().UTC()); err != nil {
					return err
				}
				current, err := loadSupervisionSession(ctx, s.db, ss.ID)
				if err != nil {
					return err
				}
				if err := s.requestSupervisionControl(ctx, ss.ID, current.StateVersion, control, time.Now().UTC()); err != nil {
					return err
				}
				return completeWorkerDecisionForTest(s, d, lease, decisionProposalForTest(d))
			})
			if err := w.process(context.Background(), ss.ID); err != nil {
				t.Fatal(err)
			}
			current := supervisionSessionForTest(t, s, ss.ID)
			wantState := map[string]string{"pause": "paused", "stop": "stopped"}[control]
			if current.State != wantState || current.UsedTurns != 1 || current.ClockAt != nil {
				t.Fatalf("control=%+v", current)
			}
			if control == "stop" {
				assertExecutionClaimForTest(t, s, taskID, "", "")
			} else {
				assertExecutionClaimForTest(t, s, taskID, "supervision", ss.ID)
			}
		})
	}
}

func TestSupervisionWorkerLaunchFailureClosesOnlyUnstartedFacts(t *testing.T) {
	s, _, _, ss, now := supervisionFixture(t, defaultSupervisionBudget())
	s.codexRunner = unavailableRunner{} // 普通 Runner 不具备托管协议，必须显式拒绝。
	startSupervisionForTest(t, s, ss.ID, now)
	w := supervisionWorkerForTest(s, s.executeSupervisionWorkerDecision)
	if err := w.process(context.Background(), ss.ID); err == nil {
		t.Fatal("unavailable scheduler was silently accepted")
	}
	current := supervisionSessionForTest(t, s, ss.ID)
	if current.State != "needs_input" || current.Reason != "scheduler_start_failed" || current.UsedTurns != 1 {
		t.Fatalf("failed launch=%+v", current)
	}
	var state string
	if err := s.db.QueryRow(`select state from supervision_work_items where session_id=?`, ss.ID).Scan(&state); err != nil || state != "quiescent" {
		t.Fatalf("launch window remained active: %s %v", state, err)
	}
	if err := w.process(context.Background(), ss.ID); err != nil {
		t.Fatal(err)
	}
	if current := supervisionSessionForTest(t, s, ss.ID); current.UsedTurns != 1 {
		t.Fatal("unavailable scheduler retried indefinitely")
	}
}

func TestSupervisionWorkerFailureCompensationRejectsForeignOrBoundExecution(t *testing.T) {
	s, p, _, ss, now := supervisionFixture(t, defaultSupervisionBudget())
	startSupervisionForTest(t, s, ss.ID, now)
	lease, d := decisionForTest(t, s, ss.ID, now)
	if _, err := s.claimSupervisionDecisionLaunch(context.Background(), d.ID, lease, now); err != nil {
		t.Fatal(err)
	}
	forged := d
	forged.WorkItemID = "foreign-work-item"
	if err := s.failSupervisionUnstartedDecision(context.Background(), forged, lease, errors.New("failed"), now); !errors.Is(err, errSupervisionState) {
		t.Fatalf("foreign work item accepted: %v", err)
	}
	if _, err := s.db.Exec(`insert into runs(id,conversation_id,status,workspace_path,created_at) values ('bound-run','conversation','running',?,?)`, p.Path, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.bindSupervisionDecisionRun(context.Background(), d.ID, d.ExecutionID, "conversation", "bound-run", lease, now); err != nil {
		t.Fatal(err)
	}
	if err := s.failSupervisionUnstartedDecision(context.Background(), d, lease, errors.New("failed"), now); !errors.Is(err, errSupervisionState) && !errors.Is(err, errWorkspaceNotQuiescent) {
		t.Fatalf("bound execution erased: %v", err)
	}
	if current := supervisionSessionForTest(t, s, ss.ID); current.State != "running" || current.UsedTurns != 1 {
		t.Fatalf("compensation changed live facts: %+v", current)
	}
}

func TestSupervisionWorkerSweepFindsMissedWakeAndDoesNotBlockOtherProject(t *testing.T) {
	s, _, _, first, now := supervisionFixture(t, defaultSupervisionBudget())
	startSupervisionForTest(t, s, first.ID, now)
	if _, err := s.db.Exec(`insert into projects(id,name,path,runner,git_branch,claude_ready,created_at) values ('independent-project','independent',?,?,'main',1,?)`, t.TempDir(), s.localRunnerID(), now); err != nil {
		t.Fatal(err)
	}
	secondTask := createTaskForTest(t, s.routes(), "independent-project", "independent task")
	second, err := s.createSupervisionDraft(context.Background(), "independent-project", []string{secondTask}, "codex", defaultSupervisionBudget())
	if err != nil {
		t.Fatal(err)
	}
	startSupervisionForTest(t, s, second.ID, now)
	entered, release := make(chan string, 2), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	w := supervisionWorkerForTest(s, func(_ context.Context, d supervisionDecision, lease supervisionLease) error {
		entered <- d.SessionID
		<-release
		return finishWorkerDecisionForTest(s, d, lease, decisionProposalForTest(d))
	})
	// 不注册 w 到通知入口，模拟没有任何唤醒提示，仅由持久状态补查发现。
	w.sweep(s.runtimeCtx)
	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case id := <-entered:
			seen[id] = true
		case <-time.After(5 * time.Second):
			t.Fatal("one project blocked another project's scheduler")
		}
	}
	unblock()
	waitSupervisionWorkerForTest(t, func() bool { w.mu.Lock(); defer w.mu.Unlock(); return len(w.active) == 0 })
	for _, id := range []string{first.ID, second.ID} {
		var state string
		if err := s.db.QueryRow(`select state from supervision_decisions where session_id=?`, id).Scan(&state); err != nil || state != "applied" {
			t.Fatalf("session=%s decision=%s err=%v", id, state, err)
		}
	}
}

func TestSupervisionWorkerPendingOperationsBlockAndQuiescentInitialRecoveryContinues(t *testing.T) {
	s, p, taskID, ss, now := supervisionFixture(t, defaultSupervisionBudget())
	release, ok := s.acquireWorkspace(p.ID, "existing-operation")
	if !ok {
		t.Fatal("failed to acquire initial operation")
	}
	defer release()
	if err := s.startSupervisionSession(context.Background(), ss.ID, ss.StateVersion, now); err != nil {
		t.Fatal(err)
	}
	calls := 0
	w := supervisionWorkerForTest(s, func(_ context.Context, d supervisionDecision, lease supervisionLease) error {
		calls++
		return finishWorkerDecisionForTest(s, d, lease, decisionProposalForTest(d))
	})
	if err := w.process(context.Background(), ss.ID); !errors.Is(err, errWorkspaceNotQuiescent) {
		t.Fatalf("pending directory=%v", err)
	}
	release()
	if err := s.recoverWorkspaceAdmissions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.recoverSupervisionSessions(context.Background(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := w.process(context.Background(), ss.ID); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || supervisionSessionForTest(t, s, ss.ID).RecoveryState != "ready" {
		t.Fatal("quiescent initial recovery did not continue safely")
	}
	assertExecutionClaimForTest(t, s, taskID, "supervision", ss.ID)
}

func TestSupervisionWorkerLoopFindsCommittedStartWithoutManualWake(t *testing.T) {
	s, _, _, ss, now := supervisionFixture(t, defaultSupervisionBudget())
	var calls atomic.Int32
	if !s.startSupervisionWorker(func(_ context.Context, d supervisionDecision, lease supervisionLease) error {
		calls.Add(1)
		return finishWorkerDecisionForTest(s, d, lease, decisionProposalForTest(d))
	}, 20*time.Millisecond) {
		t.Fatal("worker did not start")
	}
	if s.startSupervisionWorker(func(context.Context, supervisionDecision, supervisionLease) error { return nil }, time.Second) {
		t.Fatal("duplicate worker loop started")
	}
	if err := s.startSupervisionSession(context.Background(), ss.ID, ss.StateVersion, now); err != nil {
		t.Fatal(err)
	}
	waitSupervisionWorkerForTest(t, func() bool {
		var state string
		err := s.db.QueryRow(`select state from supervision_decisions where session_id=?`, ss.ID).Scan(&state)
		return err == nil && state == "applied"
	})
	if calls.Load() != 1 {
		t.Fatalf("calls=%d", calls.Load())
	}
	// 直接调用补查，验证健康且无变化时不再发起模型调用。
	s.supervisionWorker.sweep(s.runtimeCtx)
	s.Close()
	if calls.Load() != 1 || s.startSupervisionWorkerLoop() {
		t.Fatal("worker repeated a proposal or restarted after Close")
	}
}

func TestSupervisionWorkerCloseWaitsForExecutionFactSettlement(t *testing.T) {
	s, _, _, ss, now := supervisionFixture(t, defaultSupervisionBudget())
	startSupervisionForTest(t, s, ss.ID, now)
	started, cancelled, finish := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(finish) }) }
	defer unblock()
	settled := make(chan error, 1)
	if !s.startSupervisionWorker(func(ctx context.Context, d supervisionDecision, lease supervisionLease) error {
		if _, err := s.claimSupervisionDecisionLaunch(ctx, d.ID, lease, time.Now().UTC()); err != nil {
			settled <- err
			return err
		}
		close(started)
		<-ctx.Done()
		close(cancelled)
		<-finish
		err := s.failSupervisionUnstartedDecision(context.Background(), d, lease, ctx.Err(), time.Now().UTC())
		settled <- err
		return err
	}, time.Hour) {
		t.Fatal("worker did not start")
	}
	select {
	case <-started:
	case err := <-settled:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not execute")
	}
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not cancel worker")
	}
	select {
	case <-closed:
		t.Fatal("database closed before execution facts settled")
	default:
	}
	if err := s.db.Ping(); err != nil {
		t.Fatal(err)
	}
	unblock()
	if err := <-settled; err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not finish")
	}
}
