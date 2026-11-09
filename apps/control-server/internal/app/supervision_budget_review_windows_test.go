//go:build windows

package app

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func finalReserveForBudgetReviewTest(t *testing.T) (*Server, []string, supervisionSession) {
	t.Helper()
	budget := defaultSupervisionBudget()
	budget.MaxAgentTurns, budget.FinalReservePercent = 5, 40
	s, _, ids, ss, pc := multiWorkflowFixtureForTest(t, budget, false)
	plan := validSupervisionPlanActionForTest(ss, ids[0], pc)
	plan.Plan.Scope = []string{"a"}
	applyWorkflowActionForTest(t, s, ss.ID, plan)
	applyWorkflowActionForTest(t, s, ss.ID, supervisionWorkflowAction{Type: "dispatch_implementation", SessionID: ss.ID, TaskID: ids[0], RequirementsRevision: 1, PlanRevision: 1, ContextID: pc.ID, Instruction: "核查已有成果"})
	permit, _ := workflowPermitForTest(t, s, ss.ID)
	ctx := context.Background()
	if err := s.finishSupervisionExecution(ctx, permit, supervisionSchedulerCLIReceipt{ContainerQuiet: true}, nil, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.completeSupervisionRole(ctx, permit, `{"summary":"已有成果保留，无文件变更","edits":[]}`, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.checkSupervisionBudget(ctx, ss.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	ss = supervisionSessionForTest(t, s, ss.ID)
	if ss.State != "finalizing" || ss.UsedTurns != 3 {
		t.Fatal("fixture did not enter its final budget reserve", ss)
	}
	return s, ids, ss
}

func TestSupervisionBudgetIncreaseRestoresOnlyBudgetDeferredTasksIncludingWhilePaused(t *testing.T) {
	for _, paused := range []bool{false, true} {
		for _, explicit := range []bool{false, true} {
			t.Run(fmt.Sprintf("paused=%v/explicit_disposition=%v", paused, explicit), func(t *testing.T) {
				s, ids, ss := finalReserveForBudgetReviewTest(t)
				ctx := context.Background()
				if explicit {
					applyWorkflowActionForTest(t, s, ss.ID, supervisionWorkflowAction{Type: "record_task_disposition", SessionID: ss.ID, TaskID: ids[1], Disposition: "deferred", Instruction: "外部条件不满足，增加预算也不能继续此任务"})
					ss = supervisionSessionForTest(t, s, ss.ID)
				}
				if paused {
					if err := s.requestSupervisionControl(ctx, ss.ID, ss.StateVersion, "pause", time.Now().UTC()); err != nil {
						t.Fatal(err)
					}
					if err := s.settleSupervisionControl(ctx, ss.ID, time.Now().UTC()); err != nil {
						t.Fatal(err)
					}
					ss = supervisionSessionForTest(t, s, ss.ID)
				}
				budget := ss.Budget
				budget.MaxAgentTurns = 20
				if err := s.updateSupervisionBudget(ctx, ss.ID, ss.StateVersion, budget, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
				ss = supervisionSessionForTest(t, s, ss.ID)
				if paused && (ss.State != "paused" || ss.Control != "pause") {
					t.Fatal("increasing budget cleared an explicit pause", ss)
				}
				if paused {
					if err := s.resumeSupervisionSession(ctx, ss.ID, ss.StateVersion, false, false); err != nil {
						t.Fatal(err)
					}
				}
				var phase string
				if err := s.db.QueryRow(`select phase from supervision_tasks where session_id=? and task_id=?`, ss.ID, ids[1]).Scan(&phase); err != nil {
					t.Fatal(err)
				}
				want := "pending"
				if explicit {
					want = "deferred"
				}
				if phase != want {
					t.Fatal("budget change lost task disposition semantics", phase, want)
				}
				assertExecutionClaimForTest(t, s, ids[1], "supervision", ss.ID)
			})
		}
	}
}

func TestSupervisionBudgetChangeCannotAutomaticallyResumeUserPause(t *testing.T) {
	for _, settled := range []bool{false, true} {
		t.Run(fmt.Sprint(settled), func(t *testing.T) {
			s, _, ss := finalReserveForBudgetReviewTest(t)
			ctx := context.Background()
			if err := s.requestSupervisionControl(ctx, ss.ID, ss.StateVersion, "pause", time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			if settled {
				if err := s.settleSupervisionControl(ctx, ss.ID, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
			}
			ss = supervisionSessionForTest(t, s, ss.ID)
			budget := ss.Budget
			budget.MaxFixRounds++
			if err := s.updateSupervisionBudget(ctx, ss.ID, ss.StateVersion, budget, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			current, err := supervisionWorkerForTest(s, nil).maintain(ctx, ss.ID)
			if err != nil || current.State != "paused" || current.Control != "pause" {
				t.Fatal("budget maintenance resumed an explicit user pause", current, err)
			}
		})
	}
}

func TestSupervisionBudgetReductionDuringUserPauseStillCancelsActiveExecution(t *testing.T) {
	for _, limit := range []string{"total_time", "turn_time"} {
		t.Run(limit, func(t *testing.T) {
			s, _, permit, now, _ := supervisionExecutionForTest(t)
			registerAndResumeExecutionForTest(t, s, permit, now)
			ctx := context.Background()
			ss := supervisionSessionForTest(t, s, permit.Decision.SessionID)
			if err := s.requestSupervisionControl(ctx, ss.ID, ss.StateVersion, "pause", now); err != nil {
				t.Fatal(err)
			}
			ss = supervisionSessionForTest(t, s, ss.ID)
			budget := ss.Budget
			if limit == "total_time" {
				budget.MaxActiveMillis = 1
			} else {
				budget.MaxTurnMillis = 1
			}
			if err := s.updateSupervisionBudget(ctx, ss.ID, ss.StateVersion, budget, now.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			runCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			done, stopped := make(chan struct{}), make(chan struct{})
			go func() {
				defer close(stopped)
				s.monitorSupervisionExecution(runCtx, permit, cancel, done)
			}()
			defer func() { close(done); <-stopped }()
			select {
			case <-runCtx.Done():
			case <-time.After(4 * time.Second):
				t.Fatal("lowered budget failed to cancel execution while preserving user pause")
			}
			if err := s.finishSupervisionExecution(ctx, permit, supervisionSchedulerCLIReceipt{ContainerQuiet: true}, runCtx.Err(), time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			if err := s.settleSupervisionControl(ctx, ss.ID, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			current := supervisionSessionForTest(t, s, ss.ID)
			if current.State != "paused" || current.Control != "pause" || current.Reason != "user_pause" {
				t.Fatal("budget cancellation lost explicit pause intent", current)
			}
		})
	}
}
