//go:build windows

package app

import (
	"context"
	"encoding/json"
	"os/exec"
	"testing"
)

func TestSupervisionWindowsDependencyDirectoryCaseCannotPreserveOldEvidence(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node unavailable")
	}
	s, p, taskID, ss, now := supervisionFixture(t, defaultSupervisionBudget())
	ctx := context.Background()
	writeSupervisionSourceForTest(t, p.Path, "src/check.cjs", "require('node:assert/strict').equal(require('pkg'),1);console.log('dependency verified');\n")
	writeSupervisionSourceForTest(t, p.Path, "NODE_MODULES/pkg/index.js", "module.exports=1;\n")
	if _, err := s.db.Exec(`update supervision_sessions set scheduler_agent_id='claude-code' where id=?`, ss.ID); err != nil {
		t.Fatal(err)
	}
	ss = startSupervisionForTest(t, s, ss.ID, now)
	pc, err := s.captureSupervisionProjectContext(ctx, ss.ID, ss.StateVersion)
	if err != nil {
		t.Fatal(err)
	}
	applyWorkflowActionForTest(t, s, ss.ID, validSupervisionPlanActionForTest(ss, taskID, pc))
	check := supervisionCheck{ID: "dependency-case", AcceptanceIDs: []string{"behavior"}, Program: "node", Args: []string{"src/check.cjs"}, Directory: ".", TimeoutMillis: 30000}
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
			t.Fatal("unexpected actual Node result", passed, result, err)
		}
	}
	run(true, 0)
	before := supervisionSessionForTest(t, s, ss.ID)
	if err := s.refreshSupervisionEvidenceEnvironments(ctx, ss.ID); err != nil {
		t.Fatal(err)
	}
	if after := supervisionSessionForTest(t, s, ss.ID); after.StateVersion != before.StateVersion {
		t.Fatal("unchanged dependency invalidated actual evidence")
	}
	writeSupervisionSourceForTest(t, p.Path, "NODE_MODULES/pkg/index.js", "module.exports=0;\n")
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
		t.Fatal("Windows dependency case hid a real environment change and kept old evidence", freshness)
	}
	// The same source, check ID, requirements and plan must execute again;
	// Windows resolves NODE_MODULES for require('pkg') in both real processes.
	run(false, 1)
}

func TestSupervisionWindowsEnvironmentMatchesSnapshotPathCasePolicy(t *testing.T) {
	binary, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	for _, directory := range []string{"NODE_MODULES", ".VENV", "VeNv", "VENDOR"} {
		t.Run(directory, func(t *testing.T) {
			root := t.TempDir()
			dependency := "apps/web/" + directory + "/pkg/runtime.js"
			writeSupervisionSourceForTest(t, root, dependency, "version=1\n")
			fingerprint := func() string {
				t.Helper()
				value, err := supervisionEnvironmentFingerprint(context.Background(), root, binary)
				if err != nil {
					t.Fatal(err)
				}
				return value
			}
			before := fingerprint()
			writeSupervisionSourceForTest(t, root, dependency, "version=2\n")
			after := fingerprint()
			if before == after {
				t.Fatal("dependency case hid changed runtime content")
			}
			for _, cache := range []string{".CACHE/state", "__PYCACHE__/entry.PYC", ".PYTEST_CACHE/state", "module.PYC"} {
				writeSupervisionSourceForTest(t, root, "apps/web/"+directory+"/pkg/"+cache, "rebuildable\n")
				if fingerprint() != after {
					t.Fatal("Windows cache case incorrectly invalidated dependency evidence", cache)
				}
			}
		})
	}
}
