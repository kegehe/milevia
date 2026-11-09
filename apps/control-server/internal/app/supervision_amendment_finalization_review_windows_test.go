//go:build windows

package app

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestSupervisionDeferredInstructionsCannotBeLostDuringFinalDisposition(t *testing.T) {
	for _, scope := range []string{"global", "task"} {
		for _, disposition := range []string{"unresolved", "deferred"} {
			t.Run(scope+"/"+disposition, func(t *testing.T) {
				s, _, taskID, ss, _ := supervisionPlanFixture(t)
				ctx := context.Background()
				lease, decision := decisionForTest(t, s, ss.ID, time.Now().UTC())
				action := supervisionWorkflowAction{Type: "record_task_disposition", SessionID: ss.ID, TaskID: taskID, Disposition: disposition, Instruction: "根据当前已知资料，任务暂时无法继续"}
				if err := finishWorkerDecisionForTest(s, decision, lease, workflowProposalForTest(decision, action)); err != nil {
					t.Fatal(err)
				}
				// Arrive after the model's decision, before its domain action is
				// consumed. Deferred instructions intentionally let that turn finish.
				ss = supervisionSessionForTest(t, s, ss.ID)
				target := ""
				if scope == "task" {
					target = taskID
				}
				response := supervisionRequestForTest(s, "POST", "/api/supervision/"+ss.ID+"/instructions", "deferred-instruction", fmt.Sprintf(`{"stateVersion":%d,"taskId":%q,"content":"已补齐原来的前置条件，请按新要求继续处理","immediate":false}`, ss.StateVersion, target))
				if response.Code != 202 {
					t.Fatal(response.Code, response.Body.String())
				}
				if _, err := s.applySupervisionAction(ctx, decision.ID, lease); err != nil {
					t.Fatal(err)
				}
				current := supervisionSessionForTest(t, s, ss.ID)
				if supervisionTerminal(current.State) {
					t.Fatal("session finalized before consuming an acknowledged user instruction", current.State)
				}
				current, err := supervisionWorkerForTest(s, nil).maintain(ctx, ss.ID)
				if err != nil || current.State != "running" || current.Control != "" {
					t.Fatal("instruction did not remain actionable", current, err)
				}
				var phase, amendment string
				var revision int64
				if err := s.db.QueryRow(`select phase,requirements_revision from supervision_tasks where session_id=? and task_id=?`, ss.ID, taskID).Scan(&phase, &revision); err != nil {
					t.Fatal(err)
				}
				if err := s.db.QueryRow(`select state from supervision_amendments where session_id=?`, ss.ID).Scan(&amendment); err != nil {
					t.Fatal(err)
				}
				if phase != "pending" || revision != 2 || amendment != "applied" {
					t.Fatal("new requirements were silently abandoned", phase, revision, amendment)
				}
				assertExecutionClaimForTest(t, s, taskID, "supervision", ss.ID)
			})
		}
	}
}

func TestSupervisionPendingInstructionsPreventCompletingOtherAcceptedTask(t *testing.T) {
	s, _, ids, ss, pc := multiWorkflowFixtureForTest(t, defaultSupervisionBudget(), false)
	ctx := context.Background()
	plan := validSupervisionPlanActionForTest(ss, ids[0], pc)
	plan.Plan.Scope = []string{"a"}
	applyWorkflowActionForTest(t, s, ss.ID, plan)
	applyWorkflowActionForTest(t, s, ss.ID, supervisionWorkflowAction{Type: "dispatch_implementation", SessionID: ss.ID, TaskID: ids[0], RequirementsRevision: 1, PlanRevision: 1, ContextID: pc.ID, Instruction: "核对现有实现"})
	permit, _ := workflowPermitForTest(t, s, ss.ID)
	if err := s.finishSupervisionExecution(ctx, permit, supervisionSchedulerCLIReceipt{ContainerQuiet: true}, nil, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.completeSupervisionRole(ctx, permit, `{"summary":"已核对现有实现","edits":[]}`, nil); err != nil {
		t.Fatal(err)
	}
	// Domain evidence fixtures isolate finalization from the CLI and command
	// adapters, which have their own integration coverage.
	check := supervisionCheck{ID: "behavior", AcceptanceIDs: []string{"behavior"}, Program: "node", Args: []string{"a/check.cjs"}, Directory: ".", TimeoutMillis: 30000}
	addWorkflowEvidenceForTest(t, s, ss.ID, ids[0], permit.Decision.ExecutionID, "command", pc.RevisionID, true, supervisionCheckResult{Check: check, ExitCode: 0})
	addWorkflowEvidenceForTest(t, s, ss.ID, ids[0], permit.Decision.ExecutionID, "review", pc.RevisionID, true, map[string]string{"verdict": "pass"})
	applyWorkflowActionForTest(t, s, ss.ID, supervisionWorkflowAction{Type: "record_acceptance", SessionID: ss.ID, TaskID: ids[0], RequirementsRevision: 1, PlanRevision: 1, ContextID: pc.ID})
	lease, decision := decisionForTest(t, s, ss.ID, time.Now().UTC())
	action := supervisionWorkflowAction{Type: "record_task_disposition", SessionID: ss.ID, TaskID: ids[1], Disposition: "unresolved", Instruction: "第二项任务缺少必要前置条件"}
	if err := finishWorkerDecisionForTest(s, decision, lease, workflowProposalForTest(decision, action)); err != nil {
		t.Fatal(err)
	}
	ss = supervisionSessionForTest(t, s, ss.ID)
	response := supervisionRequestForTest(s, "POST", "/api/supervision/"+ss.ID+"/instructions", "revise-candidate", fmt.Sprintf(`{"stateVersion":%d,"taskId":%q,"content":"第一项新增要求，需要重新规划和验收","immediate":false}`, ss.StateVersion, ids[0]))
	if response.Code != 202 {
		t.Fatal(response.Code, response.Body.String())
	}
	if _, err := s.applySupervisionAction(ctx, decision.ID, lease); err != nil {
		t.Fatal(err)
	}
	current := supervisionSessionForTest(t, s, ss.ID)
	var status string
	if err := s.db.QueryRow(`select status from tasks where id=?`, ids[0]).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if supervisionTerminal(current.State) || status == taskDone {
		t.Fatal("pending instructions were bypassed by finalizing a different task", current.State, status)
	}
	if _, err := supervisionWorkerForTest(s, nil).maintain(ctx, ss.ID); err != nil {
		t.Fatal(err)
	}
	var phase string
	var revision int64
	if err := s.db.QueryRow(`select phase,requirements_revision from supervision_tasks where session_id=? and task_id=?`, ss.ID, ids[0]).Scan(&phase, &revision); err != nil {
		t.Fatal(err)
	}
	if phase != "pending" || revision != 2 {
		t.Fatal("accepted task was not reopened for the new requirements", phase, revision)
	}
	for _, id := range ids {
		assertExecutionClaimForTest(t, s, id, "supervision", ss.ID)
	}
}
