package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"testing"
	"time"
)

func TestSupervisionReviewRetriesBoundActualEquivalentInputsAndAllowNewSource(t *testing.T) {
	s, p, taskID, ss, now := supervisionFixture(t, defaultSupervisionBudget())
	ctx := context.Background()
	writeSupervisionSourceForTest(t, p.Path, "src/main.go", "package main\n")
	writeSupervisionSourceForTest(t, p.Path, "fixtures/expected.json", "{\"answer\":42}\n")
	ss = startSupervisionForTest(t, s, ss.ID, now)
	pc, err := s.captureSupervisionProjectContext(ctx, ss.ID, ss.StateVersion)
	if err != nil {
		t.Fatal(err)
	}
	ss = supervisionSessionForTest(t, s, ss.ID)
	plan := validSupervisionPlanActionForTest(ss, taskID, pc)
	lease, d := proposeSupervisionPlanForTest(t, s, ss.ID, plan)
	if _, err := s.applySupervisionAction(ctx, d.ID, lease); err != nil {
		t.Fatal(err)
	}
	a := supervisionWorkflowAction{Type: "request_review", SessionID: ss.ID, TaskID: taskID, RequirementsRevision: 1, PlanRevision: 1, ContextID: pc.ID, Instruction: "核对源码和验收"}
	for attempt := 0; attempt < 3; attempt++ {
		if err := s.withSupervisionTx(ctx, ss.ID, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, current *supervisionSession) error {
			duplicate, err := supervisionDuplicateReviewTx(ctx, tx, current, a, pc)
			if err != nil {
				return err
			}
			if duplicate {
				t.Fatal("review blocked before actual retry budget")
			}
			if err := s.queueSupervisionRoleTx(ctx, tx, current, d, a, pc); err != nil {
				return err
			}
			_, err = tx.Exec(`update supervision_work_items set state='quiescent',turn_state='consumed' where session_id=? and role='review' and state='queued';update supervision_role_jobs set state='failed' where session_id=? and kind='review';update supervision_outbox set state='completed' where session_id=?;update supervision_decisions set state='applied' where session_id=?`, ss.ID, ss.ID, ss.ID, ss.ID)
			current.ReservedTurns--
			current.UsedTurns++
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.withSupervisionTx(ctx, ss.ID, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, current *supervisionSession) error {
		a.Instruction = "换个说法再次审查"
		duplicate, err := supervisionDuplicateReviewTx(ctx, tx, current, a, pc)
		if err != nil {
			return err
		}
		if !duplicate {
			t.Fatal("changing review prose bypassed actual retry budget")
		}
		a.Paths = []string{"fixtures/expected.json"}
		duplicate, err = supervisionDuplicateReviewTx(ctx, tx, current, a, pc)
		if err != nil {
			return err
		}
		if duplicate {
			t.Fatal("new source did not reopen review budget")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSupervisionRoleContextIncludesTaskAndVerificationFilesOutsideWriteScope(t *testing.T) {
	entries := []supervisionSnapshotEntry{{Path: "src/math.cjs", Kind: "file"}, {Path: "scripts/check.cjs", Kind: "file"}, {Path: "fixtures/expected.json", Kind: "file"}}
	evidence := []supervisionEvidence{{Kind: "command", Details: json.RawMessage(`{"check":{"program":"node","args":["../scripts/check.cjs"],"directory":"tests"}}`)}}
	paths := supervisionRoleContextPaths(entries, []string{"src/math.cjs"}, `验收 fixtures/expected.json`, evidence)
	for _, name := range []string{"src/math.cjs", "scripts/check.cjs", "fixtures/expected.json"} {
		if !slices.Contains(paths, name) {
			t.Fatalf("missing context %s: %v", name, paths)
		}
	}
}

func TestSupervisionRoleChangesPreserveAddedDeletedAndOutsideScopeFiles(t *testing.T) {
	before := []supervisionSnapshotEntry{{Path: "same", Kind: "file", SHA256: "same"}, {Path: "deleted", Kind: "file", SHA256: "old"}, {Path: "outside", Kind: "file", SHA256: "old"}}
	after := []supervisionSnapshotEntry{{Path: "same", Kind: "file", SHA256: "same"}, {Path: "added", Kind: "file", SHA256: "new"}, {Path: "outside", Kind: "file", SHA256: "new"}}
	changes, truncated := supervisionRoleChanges(before, after)
	if truncated || len(changes) != 3 || changes[0].Path != "added" || changes[0].Before != nil || changes[1].Path != "deleted" || changes[1].After != nil || changes[2].Path != "outside" || changes[2].Before.SHA256 != "old" || changes[2].After.SHA256 != "new" {
		t.Fatalf("incorrect review change summary: %+v truncated=%v", changes, truncated)
	}
}
