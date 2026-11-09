//go:build windows

package app

import (
	"context"
	"testing"
	"time"
)

func TestSupervisionSchedulerDeadlineRetriesWithinBudgetAndPreservesControl(t *testing.T) {
	for _, scenario := range []struct{ state, control string }{{"running", ""}, {"running", "pause"}, {"running", "stop"}, {"finalizing", ""}, {"finalizing", "pause"}, {"finalizing", "stop"}} {
		t.Run(scenario.state+"_control_"+scenario.control, func(t *testing.T) {
			s, taskID, permit, now, _ := supervisionExecutionForTest(t)
			ctx := context.Background()
			control := scenario.control
			if scenario.state == "finalizing" {
				if _, err := s.db.Exec(`update supervision_sessions set state='finalizing' where id=?; update supervision_work_items set budget_class='final' where execution_id=?`, permit.Decision.SessionID, permit.Decision.ExecutionID); err != nil {
					t.Fatal(err)
				}
			}
			if control != "" {
				ss := supervisionSessionForTest(t, s, permit.Decision.SessionID)
				if err := s.requestSupervisionControl(ctx, ss.ID, ss.StateVersion, control, now); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.finishSupervisionExecution(ctx, permit, supervisionSchedulerCLIReceipt{ContainerQuiet: true}, context.DeadlineExceeded, now); err != nil {
				t.Fatal(err)
			}
			if err := s.scheduleSupervisionRetry(ctx, permit.Decision.SessionID); err != nil {
				t.Fatal(err)
			}
			ss := supervisionSessionForTest(t, s, permit.Decision.SessionID)
			if control != "" {
				if ss.Control != control || ss.Reason != "user_"+control {
					t.Fatalf("timeout retry overwrote user control: %+v", ss)
				}
				return
			}
			if ss.State != scenario.state || ss.Reason != "transient_retry_wait" || ss.UsedTurns != 1 || ss.ReservedTurns != 0 {
				t.Fatalf("known ended timeout did not schedule bounded retry: %+v", ss)
			}
			if due, err := s.supervisionRetryDue(ctx, ss.ID); err != nil || due {
				t.Fatal("retry bypassed backoff", due, err)
			}
			// Make only the retry clock due; a new decision must execute, never
			// replay the timed out process or reset its consumed turn.
			if _, err := s.db.Exec(`update supervision_retry_state set next_at=? where session_id=?`, now.Add(-time.Minute), ss.ID); err != nil {
				t.Fatal(err)
			}
			calls := 0
			w := supervisionWorkerForTest(s, func(_ context.Context, d supervisionDecision, lease supervisionLease) error {
				calls++
				var class string
				if err := s.db.QueryRow(`select budget_class from supervision_work_items where execution_id=?`, d.ExecutionID).Scan(&class); err != nil {
					t.Fatal(err)
				}
				if (scenario.state == "finalizing") != (class == "final") {
					t.Fatal("retry changed the original execution budget class", class)
				}
				if d.ID == permit.Decision.ID || d.ExecutionID == permit.Decision.ExecutionID {
					t.Fatal("retry replayed old execution")
				}
				return finishWorkerDecisionForTest(s, d, lease, decisionProposalForTest(d))
			})
			// The fixture prepared the failed turn with another lease owner.
			if _, err := s.db.Exec(`update supervision_sessions set lease_expires_at=? where id=?`, now.Add(-time.Minute), ss.ID); err != nil {
				t.Fatal(err)
			}
			if err := w.process(ctx, ss.ID); err != nil {
				t.Fatal(err)
			}
			ss = supervisionSessionForTest(t, s, ss.ID)
			if calls != 1 || ss.UsedTurns != 2 {
				t.Fatalf("retry failed to progress without resetting usage: calls=%d session=%+v", calls, ss)
			}
			assertExecutionClaimForTest(t, s, taskID, "supervision", ss.ID)
			// Exhaustion is persisted and cannot be bypassed by another wake.
			if _, err := s.db.Exec(`update supervision_retry_state set count=? where session_id=?; update supervision_sessions set state='needs_input',reason='scheduler_execution_failed' where id=?; update supervision_decisions set validation_error=? where session_id=?`, ss.Budget.MaxTransientRetries, ss.ID, ss.ID, context.DeadlineExceeded.Error(), ss.ID); err != nil {
				t.Fatal(err)
			}
			if err := s.scheduleSupervisionRetry(ctx, ss.ID); err != nil {
				t.Fatal(err)
			}
			if got := supervisionSessionForTest(t, s, ss.ID); got.State != "needs_input" {
				t.Fatal("timeout bypassed exhausted retry budget", got)
			}
		})
	}
	for _, message := range []string{"context canceled", "401 context deadline exceeded", "budget timeout", "无工具协议 timeout"} {
		if supervisionTransientError(message) {
			t.Fatalf("non-retryable failure classified as transient: %s", message)
		}
	}
}
