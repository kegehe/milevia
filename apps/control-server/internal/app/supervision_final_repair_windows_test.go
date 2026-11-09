//go:build windows

package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestSupervisionFinalFailureReopensOnlyAffordableUncontrolledRepair(t *testing.T) {
	for _, role := range []string{"test", "review"} {
		for _, condition := range []string{"affordable", "work_exhausted", "fix_exhausted", "pause", "stop"} {
			t.Run(role+"/"+condition, func(t *testing.T) {
				budget := defaultSupervisionBudget()
				if condition == "work_exhausted" {
					budget.MaxAgentTurns, budget.FinalReservePercent = 10, 70
				}
				if condition == "fix_exhausted" {
					budget.MaxFixRounds = 0
				}
				s, _, ids, ss, pc := multiWorkflowFixtureForTest(t, budget, false)
				ctx := context.Background()
				plan := validSupervisionPlanActionForTest(ss, ids[0], pc)
				plan.Plan.Scope = []string{"a"}
				applyWorkflowActionForTest(t, s, ss.ID, plan)
				applyWorkflowActionForTest(t, s, ss.ID, supervisionWorkflowAction{Type: "dispatch_implementation", SessionID: ss.ID, TaskID: ids[0], RequirementsRevision: 1, PlanRevision: 1, ContextID: pc.ID, Instruction: "核查已有代码，不修改原始断言"})
				permit, _ := workflowPermitForTest(t, s, ss.ID)
				if err := s.finishSupervisionExecution(ctx, permit, supervisionSchedulerCLIReceipt{ContainerQuiet: true}, nil, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
				if err := s.completeSupervisionRole(ctx, permit, `{"summary":"已核查当前实现","edits":[]}`, nil); err != nil {
					t.Fatal(err)
				}
				setSupervisionPhaseForTest(t, s, ss.ID, "finalizing")
				action := supervisionWorkflowAction{Type: "request_review", SessionID: ss.ID, TaskID: ids[0], RequirementsRevision: 1, PlanRevision: 1, ContextID: pc.ID, Instruction: "根据当前代码和测试证据逐点独立审查"}
				if role == "test" {
					action.Type = "request_verification"
					action.Checks = []supervisionCheck{{ID: "behavior", AcceptanceIDs: []string{"behavior"}, Program: "node", Args: []string{"a/check.cjs"}, Directory: ".", TimeoutMillis: 30000}}
				}
				applyWorkflowActionForTest(t, s, ss.ID, action)
				permit, deadline := workflowPermitForTest(t, s, ss.ID)
				if condition == "pause" || condition == "stop" {
					ss = supervisionSessionForTest(t, s, ss.ID)
					if err := s.requestSupervisionControl(ctx, ss.ID, ss.StateVersion, condition, time.Now().UTC()); err != nil {
						t.Fatal(err)
					}
				}
				if role == "test" {
					// Real Node assertion fails: the unchanged implementation subtracts
					// while the original check requires addition. No model substitute
					// creates a passing result or edits the protected check.
					if err := s.executeSupervisionVerification(ctx, permit, deadline); err != nil {
						t.Fatal(err)
					}
					var passed bool
					var raw string
					if err := s.db.QueryRow(`select passed,details from supervision_evidence where session_id=? and execution_id=? and kind='command'`, ss.ID, permit.Decision.ExecutionID).Scan(&passed, &raw); err != nil {
						t.Fatal(err)
					}
					var result supervisionCheckResult
					if err := json.Unmarshal([]byte(raw), &result); err != nil {
						t.Fatal(err)
					}
					if passed || (condition != "pause" && condition != "stop" && (result.ExitCode != 1 || result.NotExecuted || result.Interrupted)) {
						t.Fatalf("expected a real failing Node assertion, not a missing tool or invented result: %+v", result)
					}
				} else {
					// Isolate review result consumption from nondeterministic CLI output.
					if err := s.finishSupervisionExecution(ctx, permit, supervisionSchedulerCLIReceipt{ContainerQuiet: true}, nil, time.Now().UTC()); err != nil {
						t.Fatal(err)
					}
					if err := s.completeSupervisionRole(ctx, permit, `{"verdict":"fail","summary":"当前减法实现不满足加法要求","points":[{"id":"behavior","passed":false,"reason":"2和3需要返回5"}],"blockers":["必须修复计算逻辑"]}`, nil); err != nil {
						t.Fatal(err)
					}
				}
				ss = supervisionSessionForTest(t, s, ss.ID)
				want := "finalizing"
				if condition == "affordable" {
					want = "running"
				} else if condition == "pause" {
					want = "pausing"
				} else if condition == "stop" {
					want = "stopping"
				}
				if ss.State != want {
					t.Fatalf("final failure cannot be diagnosed/repaired or bypassed its limits: want=%s session=%+v", want, ss)
				}
				if condition == "affordable" {
					applyWorkflowActionForTest(t, s, ss.ID, supervisionWorkflowAction{Type: "dispatch_implementation", SessionID: ss.ID, TaskID: ids[0], RequirementsRevision: 1, PlanRevision: 1, ContextID: pc.ID, Instruction: "根据失败证据修复加法逻辑，保留原始断言"})
					var queued bool
					if err := s.db.QueryRow(`select exists(select 1 from supervision_role_jobs where session_id=? and task_id=? and kind='implementation' and state='queued')`, ss.ID, ids[0]).Scan(&queued); err != nil || !queued {
						t.Fatal("repair was not dispatched", queued, err)
					}
				}
				if (condition == "pause" || condition == "stop") && ss.Control != condition {
					t.Fatal("repair overrode user control", ss)
				}
				assertExecutionClaimForTest(t, s, ids[0], "supervision", ss.ID)
			})
		}
	}
}
