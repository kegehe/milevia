//go:build windows

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

func TestSupervisionActualFailedCheckCanRetryAfterDependencyRepair(t *testing.T) {
	for _, baseline := range []bool{false, true} {
		t.Run(fmt.Sprintf("baseline=%v", baseline), func(t *testing.T) {
			s, p, taskID, ss, now := supervisionFixture(t, defaultSupervisionBudget())
			ctx := context.Background()
			writeSupervisionSourceForTest(t, p.Path, "src/main.cjs", "module.exports=1;\n")
			writeSupervisionSourceForTest(t, p.Path, "src/check.cjs", "require('node:assert/strict').equal(require('pkg'),1);console.log('dependency verified');\n")
			writeSupervisionSourceForTest(t, p.Path, "node_modules/pkg/index.js", "module.exports=0;\n")
			if _, err := s.db.Exec(`update supervision_sessions set scheduler_agent_id='claude-code' where id=?`, ss.ID); err != nil {
				t.Fatal(err)
			}
			ss = startSupervisionForTest(t, s, ss.ID, now)
			pc, err := s.captureSupervisionProjectContext(ctx, ss.ID, ss.StateVersion)
			if err != nil {
				t.Fatal(err)
			}
			plan := validSupervisionPlanActionForTest(ss, taskID, pc)
			plan.Plan.RequiresBaseline = baseline
			applyWorkflowActionForTest(t, s, ss.ID, plan)
			check := supervisionCheck{ID: "dependency-behavior", Baseline: baseline, AcceptanceIDs: []string{"behavior"}, Program: "node", Args: []string{"src/check.cjs"}, Directory: ".", TimeoutMillis: 30000}
			action := supervisionWorkflowAction{Type: "request_verification", SessionID: ss.ID, TaskID: taskID, RequirementsRevision: 1, PlanRevision: 1, ContextID: pc.ID, Checks: []supervisionCheck{check}}
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
			if err := json.Unmarshal([]byte(raw), &result); err != nil || passed || result.ExitCode != 1 || result.NotExecuted || result.Interrupted || result.Environment == "" {
				t.Fatal("expected an executed assertion failure with a known environment", result, err)
			}
			duplicate := func() bool {
				t.Helper()
				ss = supervisionSessionForTest(t, s, ss.ID)
				tx, err := s.db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				same, err := supervisionDuplicateCheckTx(ctx, tx, &ss, action, pc.RevisionID)
				if err != nil {
					t.Fatal(err)
				}
				return same
			}
			if err := s.refreshSupervisionEvidenceEnvironments(ctx, ss.ID); err != nil || !duplicate() {
				t.Fatal("an unchanged real failure must retain its retry guard", err)
			}
			// Repair only the excluded dependency. Source, test, check ID, plan
			// and requirement version remain unchanged.
			writeSupervisionSourceForTest(t, p.Path, "node_modules/pkg/index.js", "module.exports=1;\n")
			if err := s.refreshSupervisionEvidenceEnvironments(ctx, ss.ID); err != nil {
				t.Fatal(err)
			}
			if duplicate() {
				t.Fatal("a real failure permanently blocked the same check after repairing its dependency")
			}
			if baseline {
				ss = supervisionSessionForTest(t, s, ss.ID)
				tx, err := s.db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				validation := s.validateSupervisionWorkflowTx(ctx, tx, &ss, supervisionWorkflowAction{Type: "dispatch_implementation", SessionID: ss.ID, TaskID: taskID, RequirementsRevision: 1, PlanRevision: 1, ContextID: pc.ID, Instruction: "只在当前环境基线成立后实施"}, pc)
				tx.Rollback()
				if validation == nil {
					t.Fatal("baseline from an invalidated environment still authorized implementation")
				}
			}
			current, err := loadSupervisionProjectContext(ctx, s.db, ss.ID, "current")
			if err != nil || current.RevisionID != pc.RevisionID {
				t.Fatal("dependency repair unexpectedly changed source revision", current, err)
			}
			applyWorkflowActionForTest(t, s, ss.ID, action)
			permit, deadline = workflowPermitForTest(t, s, ss.ID)
			if err := s.executeSupervisionVerification(ctx, permit, deadline); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRow(`select passed,details from supervision_evidence where execution_id=?`, permit.Decision.ExecutionID).Scan(&passed, &raw); err != nil {
				t.Fatal(err)
			}
			result = supervisionCheckResult{}
			if err := json.Unmarshal([]byte(raw), &result); err != nil || !passed || result.ExitCode != 0 || result.NotExecuted || result.Interrupted {
				t.Fatal("repaired environment did not yield a real successful rerun", result, err)
			}
			before := supervisionSessionForTest(t, s, ss.ID)
			if err := s.refreshSupervisionEvidenceEnvironments(ctx, ss.ID); err != nil {
				t.Fatal(err)
			}
			if after := supervisionSessionForTest(t, s, ss.ID); after.StateVersion != before.StateVersion || !duplicate() {
				t.Fatal("old failed environment repeatedly invalidated the new passing result", before, after)
			}
		})
	}
}
