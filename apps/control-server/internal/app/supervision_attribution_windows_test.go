//go:build windows

package app

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func TestSupervisionReviewReceivesActualPerTaskImplementationChanges(t *testing.T) {
	s, _, ids, ss, _ := multiWorkflowFixtureForTest(t, defaultSupervisionBudget(), false)
	ctx := context.Background()
	for i, id := range ids {
		pc, err := loadSupervisionProjectContext(ctx, s.db, ss.ID, "current")
		if err != nil {
			t.Fatal(err)
		}
		name := []string{"a/main.cjs", "b/main.cjs"}[i]
		plan := validSupervisionPlanActionForTest(ss, id, pc)
		plan.Plan.Scope = []string{name}
		applyWorkflowActionForTest(t, s, ss.ID, plan)
		applyWorkflowActionForTest(t, s, ss.ID, supervisionWorkflowAction{Type: "dispatch_implementation", SessionID: ss.ID, TaskID: id, RequirementsRevision: 1, PlanRevision: 1, ContextID: pc.ID, Instruction: "只修复本任务文件"})
		permit, _ := workflowPermitForTest(t, s, ss.ID)
		if err := s.finishSupervisionExecution(ctx, permit, supervisionSchedulerCLIReceipt{ContainerQuiet: true}, nil, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		result := supervisionImplementationResult{Summary: "修改指定实现", Edits: []supervisionEdit{{Path: name, BaseSHA256: supervisionSHA256([]byte("module.exports=(a,b)=>a-b;\n")), Content: "module.exports=(a,b)=>a+b;\n"}}}
		if err := s.completeSupervisionRole(ctx, permit, string(mustJSON(result)), nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.withSupervisionTx(ctx, ss.ID, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, current *supervisionSession) error {
		pc, err := loadSupervisionProjectContext(ctx, tx, ss.ID, "current")
		if err != nil {
			return err
		}
		job := supervisionRoleJob{TaskID: ids[1], PlanRevision: 1, Spec: mustJSON(supervisionWorkflowAction{Type: "request_review", TaskID: ids[1]})}
		input, err := buildSupervisionRoleInputTx(ctx, tx, current, supervisionDecision{}, pc, job)
		if err != nil {
			return err
		}
		if len(input.WorkspaceChanges) != 2 || len(input.ImplementationChanges) != 2 || input.ImplementationChangesIncomplete {
			t.Fatalf("missing batch attribution: %+v", input.ImplementationChanges)
		}
		for _, change := range input.ImplementationChanges {
			want := "a/main.cjs"
			if change.TaskID == ids[1] {
				want = "b/main.cjs"
			}
			if change.Incomplete || change.State != "completed" || len(change.Changes) != 1 || change.Changes[0].Path != want || len(change.Scope) != 1 || change.Scope[0] != want || change.InputRevision == change.OutputRevision {
				t.Fatalf("batch delta incorrectly attributed to one task: %+v", change)
			}
		}
		before := supervisionReviewFingerprint(current, input)
		input.ImplementationChanges[0].Incomplete = true
		if before == supervisionReviewFingerprint(current, input) {
			t.Fatal("review fingerprint omitted attribution facts")
		}
		if _, err := tx.ExecContext(ctx, `update supervision_role_jobs set state='failed',output_revision='' where session_id=? and kind='implementation' and task_id=?`, ss.ID, ids[0]); err != nil {
			return err
		}
		changes, incomplete, err := supervisionImplementationChangesTx(ctx, tx, ss.ID)
		if err != nil {
			return err
		}
		if !incomplete {
			t.Fatal("unverified implementation was silently treated as attributed")
		}
		for _, change := range changes {
			if change.TaskID == ids[0] && (!change.Incomplete || len(change.Changes) != 0) {
				t.Fatal("failed result acquired trusted attribution", change)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
