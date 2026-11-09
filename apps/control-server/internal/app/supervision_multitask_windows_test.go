//go:build windows

package app

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func multiWorkflowFixtureForTest(t *testing.T, budget supervisionBudget, dependent bool) (*Server, Project, []string, supervisionSession, supervisionProjectContext) {
	t.Helper()
	s, p, first, _, now := supervisionFixture(t, budget)
	second := createTaskForTest(t, s.routes(), p.ID, "second selected task")
	ids := []string{first, second}
	if dependent {
		if _, err := s.db.Exec(`insert into task_dependencies(task_id,predecessor_task_id,created_at) values (?,?,?)`, second, first, now); err != nil {
			t.Fatal(err)
		}
	}
	for _, folder := range []string{"a", "b"} {
		writeSupervisionSourceForTest(t, p.Path, folder+"/main.cjs", "module.exports=(a,b)=>a-b;\n")
		writeSupervisionSourceForTest(t, p.Path, folder+"/check.cjs", "require('node:assert/strict').equal(require('./main.cjs')(2,3),5);console.log('checked');\n")
	}
	ss, err := s.createSupervisionDraft(context.Background(), p.ID, ids, "claude-code", budget)
	if err != nil {
		t.Fatal(err)
	}
	startSupervisionForTest(t, s, ss.ID, now)
	ss = supervisionSessionForTest(t, s, ss.ID)
	pc, err := s.captureSupervisionProjectContext(context.Background(), ss.ID, ss.StateVersion)
	if err != nil {
		t.Fatal(err)
	}
	return s, p, ids, supervisionSessionForTest(t, s, ss.ID), pc
}

func TestSupervisionWorkflowIndependentFailureAndDependentFinalSnapshot(t *testing.T) {
	for _, dependent := range []bool{false, true} {
		t.Run(fmt.Sprint(dependent), func(t *testing.T) {
			s, _, ids, ss, _ := multiWorkflowFixtureForTest(t, defaultSupervisionBudget(), dependent)
			firstFailed := !dependent
			failedOnce := false
			w := supervisionWorkerForTest(s, func(ctx context.Context, d supervisionDecision, lease supervisionLease) error {
				var role, taskID string
				if err := s.db.QueryRow(`select role,task_id from supervision_work_items where execution_id=?`, d.ExecutionID).Scan(&role, &taskID); err != nil {
					return err
				}
				folder := "a"
				if taskID == ids[1] {
					folder = "b"
				}
				if role != "scheduler" {
					deadline, err := s.claimSupervisionDecisionLaunch(ctx, d.ID, lease, time.Now().UTC())
					if err != nil {
						return err
					}
					permit, err := s.prepareSupervisionExecution(ctx, d, lease, time.Now().UTC())
					if err != nil {
						return err
					}
					if role == "test" {
						return s.executeSupervisionVerification(ctx, permit, deadline)
					}
					var raw string
					var roleErr error
					if role == "implementation" {
						if firstFailed && taskID == ids[0] {
							failedOnce = true
							roleErr = errors.New("该任务实施输出无法解析，保留诊断并推进无关任务")
						} else {
							raw = string(mustJSON(supervisionImplementationResult{Summary: "修复指定函数", Edits: []supervisionEdit{{Path: folder + "/main.cjs", BaseSHA256: supervisionSHA256([]byte("module.exports=(a,b)=>a-b;\n")), Content: "module.exports=(a,b)=>a+b;\n"}}}))
						}
					} else {
						raw = string(mustJSON(supervisionReviewResult{Verdict: "pass", Summary: "当前实现与真实测试符合验收点", Points: []supervisionReviewPoint{{ID: "behavior", Passed: true, Reason: "当前快照真实断言通过"}}, Blockers: []string{}}))
					}
					if err := s.finishSupervisionExecution(ctx, permit, supervisionSchedulerCLIReceipt{ContainerQuiet: true}, roleErr, time.Now().UTC()); err != nil {
						return err
					}
					return s.completeSupervisionRole(ctx, permit, raw, roleErr)
				}
				pc, err := loadSupervisionProjectContext(ctx, s.db, ss.ID, "current")
				if err != nil {
					return err
				}
				type state struct{ id, phase, freshness string }
				states := []state{}
				for _, id := range ids {
					value := state{id: id}
					if err := s.db.QueryRow(`select phase,evidence_freshness from supervision_tasks where session_id=? and task_id=?`, ss.ID, id).Scan(&value.phase, &value.freshness); err != nil {
						return err
					}
					states = append(states, value)
				}
				chosen := state{}
				for _, value := range states {
					if value.phase != "accepted" && value.phase != "unresolved" && value.phase != "deferred" {
						chosen = value
						break
					}
				}
				if chosen.id == "" {
					for _, value := range states {
						if value.phase == "accepted" && value.freshness == "stale" {
							chosen = value
							break
						}
					}
				}
				if chosen.id == "" {
					return errors.New("no actionable task")
				}
				taskID = chosen.id
				folder = "a"
				if taskID == ids[1] {
					folder = "b"
				}
				a := supervisionWorkflowAction{SessionID: ss.ID, TaskID: taskID, RequirementsRevision: 1, PlanRevision: 1, ContextID: pc.ID, Instruction: "按本任务内容与实际证据推进"}
				var action any
				switch chosen.phase {
				case "pending":
					plan := validSupervisionPlanActionForTest(ss, taskID, pc)
					plan.Plan.Scope = []string{folder}
					if dependent && taskID == ids[1] {
						plan.Plan.DependsOn = []string{ids[0]}
					}
					action = plan
				case "ready", "blocked":
					a.Type = "dispatch_implementation"
					action = a
				case "needs_repair":
					a.Type = "record_task_disposition"
					a.Disposition = "unresolved"
					a.Instruction = "实施协议失败，已保留错误；继续其他独立任务"
					action = a
				case "implemented", "accepted":
					a.Type = "request_verification"
					a.Checks = []supervisionCheck{{ID: "behavior", AcceptanceIDs: []string{"behavior"}, Program: "node", Args: []string{folder + "/check.cjs"}, Directory: ".", TimeoutMillis: 30000}}
					action = a
				case "verified":
					a.Type = "request_review"
					action = a
				case "reviewed":
					a.Type = "record_acceptance"
					action = a
				default:
					return fmt.Errorf("unexpected phase %s", chosen.phase)
				}
				return finishWorkerDecisionForTest(s, d, lease, workflowProposalForTest(d, action))
			})
			ctx := context.Background()
			for i := 0; i < 32; i++ {
				if err := w.process(ctx, ss.ID); err != nil {
					t.Fatalf("round %d: %v", i, err)
				}
				if supervisionTerminal(supervisionSessionForTest(t, s, ss.ID).State) {
					break
				}
			}
			current := supervisionSessionForTest(t, s, ss.ID)
			want := "completed_with_issues"
			if dependent {
				want = "completed"
			}
			if current.State != want {
				t.Fatalf("state=%+v", current)
			}
			var terminalEvent string
			if err := s.db.QueryRow(`select type from supervision_events where session_id=? and type in ('session.completed','session.completed_with_issues','session.failed') order by seq desc limit 1`, ss.ID).Scan(&terminalEvent); err != nil || terminalEvent != "session."+want {
				t.Fatal("terminal event disagrees with outcome", terminalEvent, err)
			}
			var firstStatus, secondStatus string
			if err := s.db.QueryRow(`select status from tasks where id=?`, ids[0]).Scan(&firstStatus); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRow(`select status from tasks where id=?`, ids[1]).Scan(&secondStatus); err != nil {
				t.Fatal(err)
			}
			if secondStatus != "done" || (!dependent && (!failedOnce || firstStatus != "action_required")) || (dependent && firstStatus != "done") {
				t.Fatalf("task outcomes=%s/%s failure=%v", firstStatus, secondStatus, failedOnce)
			}
			if dependent {
				pc, err := loadSupervisionProjectContext(ctx, s.db, ss.ID, "current")
				if err != nil {
					t.Fatal(err)
				}
				var finalCandidates, firstChecks int
				if err := s.db.QueryRow(`select count(distinct task_id) from supervision_evidence where session_id=? and revision_id=? and kind='acceptance' and passed=1`, ss.ID, pc.RevisionID).Scan(&finalCandidates); err != nil || finalCandidates != 2 {
					t.Fatalf("not same final snapshot: %d %v", finalCandidates, err)
				}
				if err := s.db.QueryRow(`select count(*) from supervision_evidence where session_id=? and task_id=? and kind='command' and passed=1`, ss.ID, ids[0]).Scan(&firstChecks); err != nil || firstChecks != 2 {
					t.Fatalf("first task was not reverified after later write: %d %v", firstChecks, err)
				}
				var implementations int
				if err := s.db.QueryRow(`select count(*) from supervision_role_jobs where session_id=? and task_id=? and kind='implementation'`, ss.ID, ids[0]).Scan(&implementations); err != nil || implementations != 1 {
					t.Fatalf("stale evidence repeated implementation: %d %v", implementations, err)
				}
			}
			for _, id := range ids {
				assertExecutionClaimForTest(t, s, id, "", "")
			}
		})
	}
}

func TestSupervisionWorkflowWorkBudgetEntersFinalReserveWithoutStartingOtherTasks(t *testing.T) {
	budget := defaultSupervisionBudget()
	budget.MaxAgentTurns = 5
	budget.FinalReservePercent = 40
	s, _, ids, ss, pc := multiWorkflowFixtureForTest(t, budget, false)
	plan := validSupervisionPlanActionForTest(ss, ids[0], pc)
	plan.Plan.Scope = []string{"a"}
	applyWorkflowActionForTest(t, s, ss.ID, plan)
	applyWorkflowActionForTest(t, s, ss.ID, supervisionWorkflowAction{Type: "dispatch_implementation", SessionID: ss.ID, TaskID: ids[0], RequirementsRevision: 1, PlanRevision: 1, ContextID: pc.ID, Instruction: "核查实现并保留已有源码"})
	permit, _ := workflowPermitForTest(t, s, ss.ID)
	if err := s.finishSupervisionExecution(context.Background(), permit, supervisionSchedulerCLIReceipt{ContainerQuiet: true}, nil, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.completeSupervisionRole(context.Background(), permit, `{"summary":"核查完成，无文件变更","edits":[]}`, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.checkSupervisionBudget(context.Background(), ss.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	current := supervisionSessionForTest(t, s, ss.ID)
	if current.State != "finalizing" || current.Control != "" || current.UsedTurns != 3 {
		t.Fatalf("reserve state=%+v", current)
	}
	var phase string
	var started bool
	if err := s.db.QueryRow(`select phase,implementation_started from supervision_tasks where session_id=? and task_id=?`, ss.ID, ids[1]).Scan(&phase, &started); err != nil || phase != "deferred" || started {
		t.Fatalf("unstarted task=%s/%v %v", phase, started, err)
	}
	budget.MaxAgentTurns = 20
	if err := s.updateSupervisionBudget(context.Background(), ss.ID, current.StateVersion, budget, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	current = supervisionSessionForTest(t, s, ss.ID)
	if current.State != "running" {
		t.Fatalf("increased budget did not restore work: %+v", current)
	}
	if err := s.db.QueryRow(`select phase from supervision_tasks where session_id=? and task_id=?`, ss.ID, ids[1]).Scan(&phase); err != nil || phase != "pending" {
		t.Fatalf("budget deferred task not restored: %s %v", phase, err)
	}
}
