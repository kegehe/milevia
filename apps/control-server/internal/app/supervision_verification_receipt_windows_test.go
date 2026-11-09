//go:build windows

package app

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSupervisionVerificationReceiptIsIdempotentAndBoundToOriginalCheck(t *testing.T) {
	s, _, taskID, ss, pc, _ := implementedWorkflowForTest(t)
	ctx := context.Background()
	check := supervisionCheck{ID: "check", AcceptanceIDs: []string{"behavior"}, Program: "node", Args: []string{"--version"}, Directory: ".", TimeoutMillis: 30000}
	applyWorkflowActionForTest(t, s, ss.ID, supervisionWorkflowAction{Type: "request_verification", SessionID: ss.ID, TaskID: taskID, RequirementsRevision: 1, PlanRevision: 1, ContextID: pc.ID, Checks: []supervisionCheck{check}})
	permit, _ := workflowPermitForTest(t, s, ss.ID)
	j, err := loadSupervisionRoleJob(ctx, s.db, permit.Decision.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	result := supervisionCheckResult{Check: check, ExitCode: 0}
	if err := s.finishSupervisionVerification(ctx, permit, j, result, nil); !errors.Is(err, errWorkspaceNotQuiescent) {
		t.Fatal("running process generated accepted evidence", err)
	}
	if err := s.finishSupervisionExecution(ctx, permit, supervisionSchedulerCLIReceipt{ContainerQuiet: true}, nil, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	forged := result
	forged.Check = check
	forged.Check.Program = "fake-check"
	if err := s.finishSupervisionVerification(ctx, permit, j, forged, nil); !errors.Is(err, errSupervisionState) {
		t.Fatal("receipt for another check accepted", err)
	}
	if err := s.finishSupervisionVerification(ctx, permit, j, result, nil); err != nil {
		t.Fatal(err)
	}
	completed := supervisionSessionForTest(t, s, ss.ID)
	if err := s.finishSupervisionVerification(ctx, permit, j, result, nil); err != nil {
		t.Fatal("identical completion retry failed", err)
	}
	if got := supervisionSessionForTest(t, s, ss.ID); got.StateVersion != completed.StateVersion || got.EventSeq != completed.EventSeq {
		t.Fatal("retry advanced state or duplicated events")
	}
	// The same receipt stays historical after control changes. Its original
	// evidence must neither be relabelled as interrupted nor inserted again.
	if err := s.requestSupervisionControl(ctx, ss.ID, completed.StateVersion, "pause", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.settleSupervisionControl(ctx, ss.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.finishSupervisionVerification(ctx, permit, j, result, nil); err != nil {
		t.Fatal("history retry needed fresh execution authority", err)
	}
	forged = result
	forged.Stdout = "different actual output"
	if err := s.finishSupervisionVerification(ctx, permit, j, forged, nil); !errors.Is(err, errSupervisionState) {
		t.Fatal("conflicting completion replaced original", err)
	}
	var count, passed int
	if err := s.db.QueryRow(`select count(*),sum(passed) from supervision_evidence where execution_id=?`, j.ExecutionID).Scan(&count, &passed); err != nil || count != 1 || passed != 1 {
		t.Fatal("verification receipt duplicated or changed evidence", count, passed, err)
	}
}
