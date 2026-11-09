//go:build windows

package app

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"testing"
)

func TestSupervisionUnstartedBaselineCannotAuthorizeImplementation(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node unavailable")
	}
	s, _, taskID, ss, pc := supervisionPlanFixture(t)
	if _, err := s.db.Exec(`update supervision_sessions set scheduler_agent_id='claude-code' where id=?`, ss.ID); err != nil {
		t.Fatal(err)
	}
	plan := validSupervisionPlanActionForTest(ss, taskID, pc)
	plan.Plan.RequiresBaseline = true
	applyWorkflowActionForTest(t, s, ss.ID, plan)
	check := supervisionCheck{ID: "baseline", Baseline: true, AcceptanceIDs: []string{"behavior"}, Program: "node", Args: []string{"--version"}, Directory: ".", TimeoutMillis: 30000}
	verification := supervisionWorkflowAction{Type: "request_verification", SessionID: ss.ID, TaskID: taskID, RequirementsRevision: 1, PlanRevision: 1, ContextID: pc.ID, Checks: []supervisionCheck{check}}
	applyWorkflowActionForTest(t, s, ss.ID, verification)
	permit, deadline := workflowPermitForTest(t, s, ss.ID)
	// Exercise the actual preflight failure path, with no process ever created.
	originalPath := os.Getenv("PATH")
	t.Setenv("PATH", t.TempDir())
	ctx := context.Background()
	if err := s.executeSupervisionVerification(ctx, permit, deadline); err != nil {
		t.Fatal(err)
	}
	var raw string
	var passed bool
	if err := s.db.QueryRow(`select details,passed from supervision_evidence where execution_id=?`, permit.Decision.ExecutionID).Scan(&raw, &passed); err != nil {
		t.Fatal(err)
	}
	var result supervisionCheckResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil || passed || result.ExitCode != -1 || result.Interrupted || !result.NotExecuted || result.Error == "" {
		t.Fatal("missing tool failure was not retained accurately", result, passed, err)
	}
	ss = supervisionSessionForTest(t, s, ss.ID)
	action := supervisionWorkflowAction{Type: "dispatch_implementation", SessionID: ss.ID, TaskID: taskID, RequirementsRevision: 1, PlanRevision: 1, ContextID: pc.ID, Instruction: "先取得真实实施前基线"}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	validationErr := s.validateSupervisionWorkflowTx(ctx, tx, &ss, action, pc)
	duplicate, duplicateErr := supervisionDuplicateCheckTx(ctx, tx, &ss, verification, pc.RevisionID)
	tx.Rollback()
	if validationErr == nil {
		t.Fatal("a tool lookup failure was accepted as an executed baseline")
	}
	if duplicateErr != nil || duplicate {
		t.Fatal("a test that never executed prevented retry after repairing the toolchain", duplicate, duplicateErr)
	}
	// Restoring a tool changes no source snapshot. The same specification must
	// run again and supply a genuine baseline without changing its ID or plan.
	t.Setenv("PATH", originalPath)
	applyWorkflowActionForTest(t, s, ss.ID, verification)
	permit, deadline = workflowPermitForTest(t, s, ss.ID)
	if err := s.executeSupervisionVerification(ctx, permit, deadline); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`select details,passed from supervision_evidence where execution_id=?`, permit.Decision.ExecutionID).Scan(&raw, &passed); err != nil {
		t.Fatal(err)
	}
	result = supervisionCheckResult{}
	if err := json.Unmarshal([]byte(raw), &result); err != nil || !passed || result.NotExecuted || result.ExitCode != 0 {
		t.Fatal("repaired toolchain did not produce actual execution evidence", result, passed, err)
	}
	ss = supervisionSessionForTest(t, s, ss.ID)
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := s.validateSupervisionWorkflowTx(ctx, tx, &ss, action, pc); err != nil {
		t.Fatal("actual baseline did not authorize implementation", err)
	}
}

func TestSupervisionBaselineRequiresExecutedCompleteResult(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		result supervisionCheckResult
		allow  bool
	}{
		{name: "legacy_preflight_failure", result: supervisionCheckResult{ExitCode: -1}},
		{name: "suspended_process_terminated", result: supervisionCheckResult{ExitCode: 1, NotExecuted: true}},
		{name: "truncated_output", result: supervisionCheckResult{ExitCode: 0, Truncated: true}},
		{name: "real_assertion_failure", result: supervisionCheckResult{ExitCode: 1}, allow: true},
		{name: "real_success", result: supervisionCheckResult{ExitCode: 0}, allow: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			s, _, taskID, ss, pc, execution := implementedWorkflowForTest(t)
			if _, err := s.db.Exec(`update supervision_tasks set implementation_started=0,fix_rounds=0 where session_id=?`, ss.ID); err != nil {
				t.Fatal(err)
			}
			scenario.result.Check = supervisionCheck{ID: "baseline", Baseline: true, AcceptanceIDs: []string{"behavior"}, Program: "node", Args: []string{"--version"}, Directory: ".", TimeoutMillis: 30000}
			addWorkflowEvidenceForTest(t, s, ss.ID, taskID, execution, "command", pc.RevisionID, scenario.result.ExitCode == 0, scenario.result)
			ctx := context.Background()
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			// The plan content is protected by its semantic hash.
			plan := validSupervisionPlanActionForTest(ss, taskID, pc).Plan
			plan.RequiresBaseline = true
			raw := mustJSON(plan)
			if _, err := tx.ExecContext(ctx, `update supervision_plan_versions set plan_json=?,semantic_hash=? where session_id=?`, string(raw), supervisionSHA256(raw), ss.ID); err != nil {
				t.Fatal(err)
			}
			action := supervisionWorkflowAction{Type: "dispatch_implementation", SessionID: ss.ID, TaskID: taskID, RequirementsRevision: 1, PlanRevision: 1, ContextID: pc.ID, Instruction: "实施前先核对基线"}
			err = s.validateSupervisionWorkflowTx(ctx, tx, &ss, action, pc)
			if (err == nil) != scenario.allow {
				t.Fatal("baseline authorization did not match actual execution facts", scenario.allow, err)
			}
		})
	}
}
