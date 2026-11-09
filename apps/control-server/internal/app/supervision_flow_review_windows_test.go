//go:build windows

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestSupervisionFlowRepeatedReadsMustNotSilentlyStopScheduler(t *testing.T) {
	s, _, _, ss, _ := supervisionPlanFixture(t)
	calls := 0
	w := supervisionWorkerForTest(s, func(ctx context.Context, d supervisionDecision, lease supervisionLease) error {
		calls++
		action := supervisionWorkflowAction{Type: "read_context", SessionID: ss.ID, Paths: []string{"src/main.go"}}
		return finishWorkerDecisionForTest(s, d, lease, workflowProposalForTest(d, action))
	})
	for i := 0; i < 8; i++ {
		if err := w.process(context.Background(), ss.ID); err != nil {
			t.Fatal(err)
		}
	}
	current := supervisionSessionForTest(t, s, ss.ID)
	if current.State != "needs_input" || current.Reason != "no_progress" {
		t.Fatalf("duplicate reads silently stalled: calls=%d state=%s reason=%q usedTurns=%d; expected bounded no_progress handoff", calls, current.State, current.Reason, current.UsedTurns)
	}
}

func TestSupervisionFlowWaitRechecksArePersistentBoundedAndRespectControls(t *testing.T) {
	for _, mode := range []string{"bounded", "pause", "stop", "finalizing", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			s, _, taskID, ss, _ := supervisionPlanFixture(t)
			ctx := context.Background()
			if mode == "finalizing" {
				setSupervisionPhaseForTest(t, s, ss.ID, "finalizing")
			}
			calls := 0
			w := supervisionWorkerForTest(s, func(_ context.Context, d supervisionDecision, lease supervisionLease) error {
				calls++
				return finishWorkerDecisionForTest(s, d, lease, decisionProposalForTest(d))
			})
			if err := w.process(ctx, ss.ID); err != nil {
				t.Fatal(err)
			}
			current := supervisionSessionForTest(t, s, ss.ID)
			wait, err := loadSupervisionWait(ctx, s.db, current)
			if err != nil || wait == nil || wait.Condition == "" || wait.Rounds != 1 {
				t.Fatal("wait did not persist", wait, err)
			}
			response := supervisionRequestForTest(s, http.MethodGet, "/api/supervision/"+ss.ID, "", "")
			var detail struct {
				Wait *supervisionWait `json:"wait"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &detail); err != nil || response.Code != http.StatusOK || detail.Wait == nil || !detail.Wait.NextCheckAt.Equal(wait.NextCheckAt) {
				t.Fatal("wait reason and schedule missing from HTTP detail", response.Code, response.Body.String(), err)
			}
			if mode == "legacy" {
				if _, err := s.db.Exec(`drop table supervision_wait_state`); err != nil {
					t.Fatal(err)
				}
				for i := 0; i < 2; i++ {
					if err := s.migrateSupervision(ctx); err != nil {
						t.Fatal("legacy wait migration", err)
					}
				}
				if err := w.process(ctx, ss.ID); err != nil {
					t.Fatal(err)
				}
				wait, err = loadSupervisionWait(ctx, s.db, supervisionSessionForTest(t, s, ss.ID))
				if err != nil || wait == nil || wait.Rounds != 1 || calls != 1 {
					t.Fatal("legacy wait did not recover without a model replay", wait, calls, err)
				}
			}
			for i := 0; i < 3; i++ {
				if err := w.process(ctx, ss.ID); err != nil {
					t.Fatal(err)
				}
			}
			if calls != 1 {
				t.Fatal("early or duplicate wakes repeated a waiting model call", calls)
			}
			if mode == "pause" || mode == "stop" {
				current = supervisionSessionForTest(t, s, ss.ID)
				if err := s.requestSupervisionControl(ctx, ss.ID, current.StateVersion, mode, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
				if err := s.refreshSupervisionWait(ctx, ss.ID, wait.NextCheckAt.Add(time.Second)); err != nil {
					t.Fatal(err)
				}
				if err := w.process(ctx, ss.ID); err != nil {
					t.Fatal(err)
				}
				current = supervisionSessionForTest(t, s, ss.ID)
				if calls != 1 || current.Reason != "user_"+mode || current.Control != mode {
					t.Fatal("due wait overrode user control", calls, current)
				}
				return
			}
			for round := 2; round <= 3; round++ {
				// Simulate a persisted due timestamp, without sleeping or changing
				// source, requirements, checks or model output between rounds.
				if _, err := s.db.Exec(`update supervision_wait_state set next_check_at=? where session_id=?`, time.Now().UTC().Add(-time.Second), ss.ID); err != nil {
					t.Fatal(err)
				}
				if err := w.process(ctx, ss.ID); err != nil {
					t.Fatal(err)
				}
				current = supervisionSessionForTest(t, s, ss.ID)
				if calls != round {
					t.Fatal("due wait did not schedule exactly one reassessment", calls, round)
				}
				if mode == "finalizing" && round == 2 && current.State != "finalizing" {
					t.Fatal("wait reassessment reopened implementation", current)
				}
			}
			if current.State != "needs_input" || current.Reason != "no_progress" || current.UsedTurns != 3 {
				t.Fatal("unchanged wait did not hand off within its limit", current)
			}
			if wait, err := loadSupervisionWait(ctx, s.db, current); err != nil || wait != nil {
				t.Fatal("a non-running session still advertised a pending automatic recheck", wait, err)
			}
			assertExecutionClaimForTest(t, s, taskID, "supervision", ss.ID)
		})
	}
}

func TestSupervisionFlowDuplicateReadCanCorrectAndAdvanceAnotherTask(t *testing.T) {
	s, p, first, ss, _ := supervisionPlanFixture(t)
	second := createTaskForTest(t, s.routes(), p.ID, "another independent task")
	if _, err := s.db.Exec(`insert into supervision_tasks(session_id,task_id,position,task_snapshot,dependency_snapshot) values (?,?,1,'{}','[]');insert into task_execution_claims(task_id,owner_kind,owner_id,created_at) values (?,'supervision',?,?)`, ss.ID, second, second, ss.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	calls := 0
	w := supervisionWorkerForTest(s, func(ctx context.Context, d supervisionDecision, lease supervisionLease) error {
		calls++
		action := supervisionWorkflowAction{Type: "read_context", SessionID: ss.ID, TaskID: first}
		if calls == 3 {
			raw, _, err := s.supervisionSchedulerInput(ctx, d.ExecutionID)
			if err != nil {
				return err
			}
			var in supervisionSchedulerInput
			if err := json.Unmarshal(raw, &in); err != nil {
				return err
			}
			if in.Correction == nil || in.Correction.ValidationError == "" || d.Attempt != 2 {
				return fmt.Errorf("duplicate read lacks bounded correction feedback")
			}
			action.TaskID = second
		}
		return finishWorkerDecisionForTest(s, d, lease, workflowProposalForTest(d, action))
	})
	for i := 0; i < 2; i++ {
		if err := w.process(context.Background(), ss.ID); err != nil {
			t.Fatal(err)
		}
	}
	var focused string
	if err := s.db.QueryRow(`select task_id from supervision_context_window where session_id=?`, ss.ID).Scan(&focused); err != nil || focused != second || calls != 3 {
		t.Fatal("a corrected duplicate prevented another independent task from advancing", focused, calls, err)
	}
	if current := supervisionSessionForTest(t, s, ss.ID); current.State != "running" || current.UsedTurns != 3 || current.ReservedTurns != 0 {
		t.Fatal("correction lost budget or running state", current)
	}
}

func TestSupervisionFlowWaitMustObserveExternalSourceAndRuleChanges(t *testing.T) {
	s, p, _, ss, pc := supervisionPlanFixture(t)
	calls := 0
	w := supervisionWorkerForTest(s, func(ctx context.Context, d supervisionDecision, lease supervisionLease) error {
		calls++
		return finishWorkerDecisionForTest(s, d, lease, decisionProposalForTest(d))
	})
	if err := w.process(context.Background(), ss.ID); err != nil {
		t.Fatal(err)
	}
	writeSupervisionSourceForTest(t, p.Path, "src/main.go", "package changed\n")
	writeSupervisionSourceForTest(t, p.Path, "AGENTS.md", "# New rules\nCheck the new external requirements.\n")
	for i := 0; i < 5; i++ {
		if err := w.process(context.Background(), ss.ID); err != nil {
			t.Fatal(err)
		}
	}
	current := supervisionSessionForTest(t, s, ss.ID)
	observed, err := loadSupervisionProjectContext(context.Background(), s.db, ss.ID, "current")
	if err != nil {
		t.Fatal(err)
	}
	if current.State != "needs_input" || current.Reason != "workspace_or_rules_drift" {
		t.Fatalf("wait never observed source/rule changes: calls=%d state=%s reason=%q contextUnchanged=%v; expected explicit drift review", calls, current.State, current.Reason, observed.ID == pc.ID)
	}
}

func TestSupervisionFlowRepeatedPlansMustNotSilentlyStopScheduler(t *testing.T) {
	s, _, taskID, ss, pc := supervisionPlanFixture(t)
	calls := 0
	w := supervisionWorkerForTest(s, func(ctx context.Context, d supervisionDecision, lease supervisionLease) error {
		calls++
		action := validSupervisionPlanActionForTest(supervisionSessionForTest(t, s, ss.ID), taskID, pc)
		if err := s.db.QueryRow(`select plan_revision from supervision_tasks where session_id=? and task_id=?`, ss.ID, taskID).Scan(&action.ExpectedPlanRevision); err != nil {
			return err
		}
		return finishWorkerDecisionForTest(s, d, lease, supervisionPlanProposalForTest(d, action))
	})
	for i := 0; i < 8; i++ {
		if err := w.process(context.Background(), ss.ID); err != nil {
			t.Fatal(err)
		}
	}
	current := supervisionSessionForTest(t, s, ss.ID)
	if current.State != "needs_input" || current.Reason != "no_progress" {
		t.Fatalf("semantic replay silently stalled: calls=%d state=%s reason=%q usedTurns=%d; expected bounded no_progress handoff", calls, current.State, current.Reason, current.UsedTurns)
	}
}

func TestSupervisionFlowLegacyNoOpDecisionIsCorrectedWithoutUserResume(t *testing.T) {
	for _, kind := range []string{"read_context", "record_plan"} {
		t.Run(kind, func(t *testing.T) {
			s, _, taskID, ss, pc := supervisionPlanFixture(t)
			var action any = supervisionWorkflowAction{Type: kind, SessionID: ss.ID, TaskID: taskID}
			if kind == "record_plan" {
				action = validSupervisionPlanActionForTest(ss, taskID, pc)
			}
			applyWorkflowActionForTest(t, s, ss.ID, action)
			if kind == "record_plan" {
				plan := action.(supervisionRecordPlanAction)
				plan.ExpectedPlanRevision = 1
				action = plan
			}
			lease, d := decisionForTest(t, s, ss.ID, time.Now().UTC())
			if err := finishWorkerDecisionForTest(s, d, lease, workflowProposalForTest(d, action)); err != nil {
				t.Fatal(err)
			}
			// Recreate the old applied no-op state, retaining its action receipt.
			if _, err := s.db.Exec(`update supervision_decisions set state='applied' where id=?;insert into supervision_action_results(decision_id,attempt,state,error,created_at) values (?,?,'applied','',?)`, d.ID, d.ID, d.Attempt, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			calls := 0
			w := supervisionWorkerForTest(s, func(ctx context.Context, next supervisionDecision, nextLease supervisionLease) error {
				calls++
				if next.ID != d.ID || next.Attempt != 2 || next.ExecutionID == d.ExecutionID {
					return fmt.Errorf("legacy correction replayed an old execution or lost its attempt limit")
				}
				return finishWorkerDecisionForTest(s, next, nextLease, decisionProposalForTest(next))
			})
			w.owner = lease.Owner
			if err := w.process(context.Background(), ss.ID); err != nil {
				t.Fatal(err)
			}
			current := supervisionSessionForTest(t, s, ss.ID)
			wait, err := loadSupervisionWait(context.Background(), s.db, current)
			if err != nil || calls != 1 || wait == nil || current.State != "running" {
				t.Fatal("legacy no-op was not corrected to an explicitly monitored wait", calls, wait, current, err)
			}
		})
	}
}
