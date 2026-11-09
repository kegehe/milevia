//go:build windows

package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"
)

func setSupervisionPhaseForTest(t *testing.T, s *Server, id, state string) {
	t.Helper()
	if err := s.withSupervisionTx(context.Background(), id, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
		ss.State = state
		ss.StateVersion++
		return appendSupervisionEventTx(context.Background(), tx, ss, "session."+state, nil, time.Now().UTC())
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSupervisionResumePreservesExecutionPhase(t *testing.T) {
	for _, phase := range []string{"running", "finalizing"} {
		for _, mode := range []string{"pause", "restart", "pause_restart", "needs_input", "accept_unchanged"} {
			t.Run(phase+"/"+mode, func(t *testing.T) {
				s, _, _, ss, _ := supervisionPlanFixture(t)
				ctx := context.Background()
				setSupervisionPhaseForTest(t, s, ss.ID, phase)
				if mode == "pause" || mode == "pause_restart" {
					ss = supervisionSessionForTest(t, s, ss.ID)
					if err := s.requestSupervisionControl(ctx, ss.ID, ss.StateVersion, "pause", time.Now().UTC()); err != nil {
						t.Fatal(err)
					}
					if err := s.settleSupervisionControl(ctx, ss.ID, time.Now().UTC()); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "needs_input" {
					setSupervisionPhaseForTest(t, s, ss.ID, "needs_input")
				}
				if mode == "restart" || mode == "pause_restart" {
					if err := s.recoverWorkspaceAdmissions(ctx); err != nil {
						t.Fatal(err)
					}
					if err := s.recoverSupervisionSessions(ctx, time.Now().UTC()); err != nil {
						t.Fatal(err)
					}
					if _, err := supervisionWorkerForTest(s, nil).maintain(ctx, ss.ID); err != nil {
						t.Fatal(err)
					}
				}
				ss = supervisionSessionForTest(t, s, ss.ID)
				if mode != "restart" {
					response := supervisionRequestForTest(s, "POST", "/api/supervision/"+ss.ID+"/control", "resume-phase", fmt.Sprintf(`{"stateVersion":%d,"action":"resume","acceptDrift":%v}`, ss.StateVersion, mode == "accept_unchanged"))
					if response.Code != 200 && response.Code != 202 {
						t.Fatal(response.Code, response.Body.String())
					}
				}
				ss = supervisionSessionForTest(t, s, ss.ID)
				if ss.State != phase || ss.Control != "" || ss.RecoveryState != "ready" {
					t.Fatalf("resume changed the execution phase: want=%s session=%+v", phase, ss)
				}
			})
		}
	}
}

func TestSupervisionFinalizingInstructionsReopenWorkWithoutOverridingUserPause(t *testing.T) {
	for _, immediate := range []bool{false, true} {
		for _, paused := range []bool{false, true} {
			t.Run(fmt.Sprintf("immediate=%v/paused=%v", immediate, paused), func(t *testing.T) {
				s, _, taskID, ss, pc := supervisionPlanFixture(t)
				ctx := context.Background()
				setSupervisionPhaseForTest(t, s, ss.ID, "finalizing")
				ss = supervisionSessionForTest(t, s, ss.ID)
				if paused {
					if err := s.requestSupervisionControl(ctx, ss.ID, ss.StateVersion, "pause", time.Now().UTC()); err != nil {
						t.Fatal(err)
					}
					if err := s.settleSupervisionControl(ctx, ss.ID, time.Now().UTC()); err != nil {
						t.Fatal(err)
					}
					ss = supervisionSessionForTest(t, s, ss.ID)
				}
				response := supervisionRequestForTest(s, "POST", "/api/supervision/"+ss.ID+"/instructions", "phase-instruction", fmt.Sprintf(`{"stateVersion":%d,"taskId":%q,"content":"新增一项行为要求，请修订实施并重新验收","immediate":%v}`, ss.StateVersion, taskID, immediate))
				if response.Code != 202 {
					t.Fatal(response.Code, response.Body.String())
				}
				if _, err := supervisionWorkerForTest(s, nil).maintain(ctx, ss.ID); err != nil {
					t.Fatal(err)
				}
				ss = supervisionSessionForTest(t, s, ss.ID)
				if paused {
					if ss.State != "paused" || ss.Control != "pause" || ss.Reason != "user_pause" {
						t.Fatal("amendment overrode an explicit pause", ss)
					}
					if err := s.resumeSupervisionSession(ctx, ss.ID, ss.StateVersion, false, false); err != nil {
						t.Fatal(err)
					}
					ss = supervisionSessionForTest(t, s, ss.ID)
				}
				var revision int64
				var taskPhase string
				if err := s.db.QueryRow(`select requirements_revision,phase from supervision_tasks where session_id=? and task_id=?`, ss.ID, taskID).Scan(&revision, &taskPhase); err != nil {
					t.Fatal(err)
				}
				if ss.State != "running" || ss.Control != "" || revision != 2 || taskPhase != "pending" {
					t.Fatalf("new requirements cannot be implemented: session=%+v revision=%d taskPhase=%s", ss, revision, taskPhase)
				}
				plan := validSupervisionPlanActionForTest(ss, taskID, pc)
				plan.RequirementsRevision = revision
				applyWorkflowActionForTest(t, s, ss.ID, plan)
				applyWorkflowActionForTest(t, s, ss.ID, supervisionWorkflowAction{Type: "dispatch_implementation", SessionID: ss.ID, TaskID: taskID, RequirementsRevision: revision, PlanRevision: 1, ContextID: pc.ID, Instruction: "按新增要求实施并提交可验证成果"})
				var queued bool
				if err := s.db.QueryRow(`select exists(select 1 from supervision_role_jobs where session_id=? and task_id=? and kind='implementation' and state='queued')`, ss.ID, taskID).Scan(&queued); err != nil || !queued {
					t.Fatal("new implementation was not dispatched", queued, err)
				}
			})
		}
	}
}

func TestSupervisionPhaseMigrationIsConservativeAndIdempotent(t *testing.T) {
	for _, state := range []string{"running", "finalizing", "paused", "needs_input"} {
		t.Run(state, func(t *testing.T) {
			s, _, _, ss, _ := supervisionPlanFixture(t)
			ctx := context.Background()
			setSupervisionPhaseForTest(t, s, ss.ID, "finalizing")
			setSupervisionPhaseForTest(t, s, ss.ID, state)
			// Recreate the old schema, not only an empty value in the new column.
			if _, err := s.db.Exec(`alter table supervision_sessions drop column run_phase`); err != nil {
				t.Fatal(err)
			}
			if err := s.migrateSupervision(ctx); err != nil {
				t.Fatal(err)
			}
			want := "finalizing"
			if state == "running" {
				want = "running"
			}
			if got := supervisionSessionForTest(t, s, ss.ID); got.RunPhase != want {
				t.Fatal("migration reopened an unknown old phase", got)
			}
			// An explicitly reopened phase (for example by a new requirement)
			// must survive later startups despite historical finalizing events.
			if _, err := s.db.Exec(`update supervision_sessions set state='paused',run_phase='running' where id=?`, ss.ID); err != nil {
				t.Fatal(err)
			}
			if err := s.migrateSupervision(ctx); err != nil {
				t.Fatal(err)
			}
			if got := supervisionSessionForTest(t, s, ss.ID); got.RunPhase != "running" {
				t.Fatal("migration overwrote persisted phase", got)
			}
		})
	}
}

func TestSupervisionPausedFinalReserveBudgetReopensDeferredWork(t *testing.T) {
	s, ids, ss := finalReserveForBudgetReviewTest(t)
	taskID := ids[1]
	ctx := context.Background()
	if err := s.requestSupervisionControl(ctx, ss.ID, ss.StateVersion, "pause", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.settleSupervisionControl(ctx, ss.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	ss = supervisionSessionForTest(t, s, ss.ID)
	budget := ss.Budget
	budget.MaxAgentTurns = 20
	if err := s.updateSupervisionBudget(ctx, ss.ID, ss.StateVersion, budget, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	ss = supervisionSessionForTest(t, s, ss.ID)
	if ss.State != "paused" || ss.Control != "pause" {
		t.Fatal("budget change overrode user pause", ss)
	}
	if err := s.resumeSupervisionSession(ctx, ss.ID, ss.StateVersion, false, false); err != nil {
		t.Fatal(err)
	}
	if got := supervisionSessionForTest(t, s, ss.ID); got.State != "running" {
		t.Fatal("reopened budget-deferred task remains forbidden to implement", got)
	}
	var phase string
	if err := s.db.QueryRow(`select phase from supervision_tasks where session_id=? and task_id=?`, ss.ID, taskID).Scan(&phase); err != nil || phase != "pending" {
		t.Fatal("budget-deferred task was not reopened", phase, err)
	}
}

func TestSupervisionFinalizingResumeReopensWorkOnlyAfterAcceptingActualDrift(t *testing.T) {
	s, project, _, ss, _ := supervisionPlanFixture(t)
	ctx := context.Background()
	setSupervisionPhaseForTest(t, s, ss.ID, "finalizing")
	ss = supervisionSessionForTest(t, s, ss.ID)
	if err := s.requestSupervisionControl(ctx, ss.ID, ss.StateVersion, "pause", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.settleSupervisionControl(ctx, ss.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	writeSupervisionSourceForTest(t, project.Path, "src/main.go", "package externally_changed\n")
	ss = supervisionSessionForTest(t, s, ss.ID)
	if err := s.resumeSupervisionSession(ctx, ss.ID, ss.StateVersion, false, false); !errors.Is(err, errSupervisionSnapshotChanged) {
		t.Fatal("silently accepted external changes", err)
	}
	if got := supervisionSessionForTest(t, s, ss.ID); got.State != "paused" || got.RunPhase != "finalizing" {
		t.Fatal("failed resume changed the original phase", got)
	}
	if err := s.resumeSupervisionSession(ctx, ss.ID, ss.StateVersion, true, false); err != nil {
		t.Fatal(err)
	}
	if got := supervisionSessionForTest(t, s, ss.ID); got.State != "running" || got.RunPhase != "running" {
		t.Fatal("explicitly accepted drift cannot be implemented and reverified", got)
	}
}
