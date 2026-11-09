//go:build windows

package app

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

func TestSupervisionSubdirectoryCheckRunsWithDependenciesAndInvalidatesChangedEnvironment(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node unavailable")
	}
	s, p, taskID, ss, now := supervisionFixture(t, defaultSupervisionBudget())
	ctx := context.Background()
	writeSupervisionSourceForTest(t, p.Path, "apps/web/src/check.cjs", "require('node:assert/strict').equal(require('pkg'),1);console.log('subdirectory dependency verified');\n")
	writeSupervisionSourceForTest(t, p.Path, "apps/web/node_modules/pkg/index.js", "module.exports=1;\n")
	writeSupervisionSourceForTest(t, p.Path, "apps/web/.env", "LOCAL_TEST_SETTING=excluded\n")
	if _, err := s.db.Exec(`update supervision_sessions set scheduler_agent_id='claude-code' where id=?`, ss.ID); err != nil {
		t.Fatal(err)
	}
	ss = startSupervisionForTest(t, s, ss.ID, now)
	pc, err := s.captureSupervisionProjectContext(ctx, ss.ID, ss.StateVersion)
	if err != nil {
		t.Fatal(err)
	}
	plan := validSupervisionPlanActionForTest(ss, taskID, pc)
	plan.Plan.Scope = []string{"apps/web/src"}
	applyWorkflowActionForTest(t, s, ss.ID, plan)
	check := supervisionCheck{ID: "subdirectory-dependency", AcceptanceIDs: []string{"behavior"}, Program: "node", Args: []string{"src/check.cjs"}, Directory: "apps/web", TimeoutMillis: 30000}
	action := supervisionWorkflowAction{Type: "request_verification", SessionID: ss.ID, TaskID: taskID, RequirementsRevision: 1, PlanRevision: 1, ContextID: pc.ID, Checks: []supervisionCheck{check}}
	run := func(wantPass bool, wantExit int) {
		t.Helper()
		applyWorkflowActionForTest(t, s, ss.ID, action)
		permit, deadline := workflowPermitForTest(t, s, ss.ID)
		if err := s.executeSupervisionVerification(ctx, permit, deadline); err != nil {
			t.Fatal(err)
		}
		var raw string
		var passed bool
		if err := s.db.QueryRow(`select passed,details from supervision_evidence where execution_id=?`, permit.Decision.ExecutionID).Scan(&passed, &raw); err != nil {
			t.Fatal(err)
		}
		var result supervisionCheckResult
		if err := json.Unmarshal([]byte(raw), &result); err != nil || passed != wantPass || result.ExitCode != wantExit || result.NotExecuted || result.Interrupted || result.Environment == "" {
			t.Fatal("unexpected actual subdirectory Node result", passed, result, err)
		}
		if wantPass && !strings.Contains(result.Stdout, "subdirectory dependency verified") {
			t.Fatal("Node did not execute the subdirectory assertion", result)
		}
	}
	run(true, 0)
	before := supervisionSessionForTest(t, s, ss.ID)
	if err := s.refreshSupervisionEvidenceEnvironments(ctx, ss.ID); err != nil {
		t.Fatal(err)
	}
	if after := supervisionSessionForTest(t, s, ss.ID); after.StateVersion != before.StateVersion {
		t.Fatal("unchanged environment invalidated actual evidence")
	}
	writeSupervisionSourceForTest(t, p.Path, "apps/web/node_modules/pkg/index.js", "module.exports=0;\n")
	ss = supervisionSessionForTest(t, s, ss.ID)
	current, err := s.captureSupervisionProjectContext(ctx, ss.ID, ss.StateVersion)
	if err != nil || current.ID != pc.ID {
		t.Fatal("excluded dependency unexpectedly changed source context", current, err)
	}
	if err := s.refreshSupervisionEvidenceEnvironments(ctx, ss.ID); err != nil {
		t.Fatal(err)
	}
	var freshness string
	if err := s.db.QueryRow(`select evidence_freshness from supervision_tasks where session_id=? and task_id=?`, ss.ID, taskID).Scan(&freshness); err != nil {
		t.Fatal(err)
	}
	if freshness != "stale" || supervisionSessionForTest(t, s, ss.ID).StateVersion <= before.StateVersion {
		t.Fatal("subdirectory dependency change kept old evidence", freshness)
	}
	run(false, 1)
}

func TestSupervisionExcludedRuntimeConfigurationCannotPreserveOldEvidence(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node unavailable")
	}
	s, p, taskID, ss, now := supervisionFixture(t, defaultSupervisionBudget())
	ctx := context.Background()
	writeSupervisionSourceForTest(t, p.Path, "apps/web/src/check.cjs", "require('node:assert/strict').equal(require('node:fs').readFileSync('.env','utf8'),'LOCAL_TEST_VALUE=1\\n');console.log('runtime configuration verified');\n")
	writeSupervisionSourceForTest(t, p.Path, "apps/web/.env", "LOCAL_TEST_VALUE=1\n")
	if _, err := s.db.Exec(`update supervision_sessions set scheduler_agent_id='claude-code' where id=?`, ss.ID); err != nil {
		t.Fatal(err)
	}
	ss = startSupervisionForTest(t, s, ss.ID, now)
	pc, err := s.captureSupervisionProjectContext(ctx, ss.ID, ss.StateVersion)
	if err != nil {
		t.Fatal(err)
	}
	plan := validSupervisionPlanActionForTest(ss, taskID, pc)
	plan.Plan.Scope = []string{"apps/web/src"}
	applyWorkflowActionForTest(t, s, ss.ID, plan)
	check := supervisionCheck{ID: "runtime-config", AcceptanceIDs: []string{"behavior"}, Program: "node", Args: []string{"src/check.cjs"}, Directory: "apps/web", TimeoutMillis: 30000}
	action := supervisionWorkflowAction{Type: "request_verification", SessionID: ss.ID, TaskID: taskID, RequirementsRevision: 1, PlanRevision: 1, ContextID: pc.ID, Checks: []supervisionCheck{check}}
	run := func(wantPass bool, wantExit int) {
		t.Helper()
		applyWorkflowActionForTest(t, s, ss.ID, action)
		permit, deadline := workflowPermitForTest(t, s, ss.ID)
		if err := s.executeSupervisionVerification(ctx, permit, deadline); err != nil {
			t.Fatal(err)
		}
		var passed bool
		var raw string
		if err := s.db.QueryRow(`select passed,details from supervision_evidence where execution_id=?`, permit.Decision.ExecutionID).Scan(&passed, &raw); err != nil {
			t.Fatal(err)
		}
		var result supervisionCheckResult
		if err := json.Unmarshal([]byte(raw), &result); err != nil || passed != wantPass || result.ExitCode != wantExit || result.NotExecuted || result.Interrupted || result.Environment == "" {
			t.Fatal("unexpected actual runtime configuration check", passed, result, err)
		}
	}
	run(true, 0)
	before := supervisionSessionForTest(t, s, ss.ID)
	writeSupervisionSourceForTest(t, p.Path, "apps/web/.env", "LOCAL_TEST_VALUE=0\n")
	current, err := s.captureSupervisionProjectContext(ctx, ss.ID, before.StateVersion)
	if err != nil || current.ID != pc.ID {
		t.Fatal("excluded runtime configuration changed source context", err)
	}
	if err := s.refreshSupervisionEvidenceEnvironments(ctx, ss.ID); err != nil {
		t.Fatal(err)
	}
	var freshness string
	if err := s.db.QueryRow(`select evidence_freshness from supervision_tasks where session_id=? and task_id=?`, ss.ID, taskID).Scan(&freshness); err != nil {
		t.Fatal(err)
	}
	if freshness != "stale" || supervisionSessionForTest(t, s, ss.ID).StateVersion <= before.StateVersion {
		t.Fatal("changed excluded runtime configuration preserved actual passing evidence", freshness)
	}
	run(false, 1)
}
