//go:build windows

package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestSupervisionObservedDriftSurvivesPauseAndRecoveryUntilExplicitAcceptance(t *testing.T) {
	for _, change := range []string{"source", "rules"} {
		for _, control := range []string{"none", "pause", "pause_restart"} {
			t.Run(change+"/"+control, func(t *testing.T) {
				s, p, taskID, ss, baseline := supervisionPlanFixture(t)
				ctx := context.Background()
				setSupervisionPhaseForTest(t, s, ss.ID, "finalizing")
				if change == "source" {
					writeSupervisionSourceForTest(t, p.Path, "src/main.go", "package external_change\n")
				} else {
					writeSupervisionSourceForTest(t, p.Path, "AGENTS.md", "New rule: verify all error paths.\n")
				}
				ss = supervisionSessionForTest(t, s, ss.ID)
				observed, err := s.captureSupervisionProjectContext(ctx, ss.ID, ss.StateVersion)
				if !errors.Is(err, errSupervisionSnapshotChanged) {
					t.Fatal("workspace drift was not detected", err)
				}
				ss = supervisionSessionForTest(t, s, ss.ID)
				if control != "none" {
					if err := s.requestSupervisionControl(ctx, ss.ID, ss.StateVersion, "pause", time.Now().UTC()); err != nil {
						t.Fatal(err)
					}
					if err := s.settleSupervisionControl(ctx, ss.ID, time.Now().UTC()); err != nil {
						t.Fatal(err)
					}
				}
				if control == "pause_restart" {
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
				response := supervisionRequestForTest(s, "POST", "/api/supervision/"+ss.ID+"/control", "do-not-accept-drift", fmt.Sprintf(`{"stateVersion":%d,"action":"resume","acceptDrift":false}`, ss.StateVersion))
				if response.Code != 409 {
					t.Fatal("observed changes were silently accepted after the reason was replaced", response.Code, response.Body.String())
				}
				response = supervisionRequestForTest(s, "POST", "/api/supervision/"+ss.ID+"/control", "explicitly-accept-drift", fmt.Sprintf(`{"stateVersion":%d,"action":"resume","acceptDrift":true}`, ss.StateVersion))
				if response.Code != 202 {
					t.Fatal(response.Code, response.Body.String())
				}
				ss = supervisionSessionForTest(t, s, ss.ID)
				if ss.State != "running" || ss.Control != "" {
					t.Fatal("accepted observed drift did not reopen implementation/reverification", ss)
				}
				var revision int64
				var phase string
				if err := s.db.QueryRow(`select requirements_revision,phase from supervision_tasks where session_id=? and task_id=?`, ss.ID, taskID).Scan(&revision, &phase); err != nil {
					t.Fatal(err)
				}
				if change == "rules" && (revision != 2 || phase != "pending") {
					t.Fatal("accepted new rules reused old requirements/plan", revision, phase)
				}
				if change == "source" && revision != 1 {
					t.Fatal("source-only drift changed requirements", revision)
				}
				current, err := loadSupervisionProjectContext(ctx, s.db, ss.ID, "current")
				if err != nil || current.RevisionID != observed.RevisionID || current.RulesHash != observed.RulesHash {
					t.Fatal("accepted observation not retained", current, err)
				}
				original, err := loadSupervisionProjectContext(ctx, s.db, ss.ID, "baseline")
				if err != nil || original.ID != baseline.ID {
					t.Fatal("acceptance replaced the startup baseline", original, err)
				}
			})
		}
	}
}

func TestSupervisionObservedInheritedRuleChangeInvalidatesOldAcceptance(t *testing.T) {
	s, p, taskID, ss, pc, execution := implementedWorkflowForTest(t)
	ctx := context.Background()
	check := supervisionCheck{ID: "behavior", AcceptanceIDs: []string{"behavior"}, Program: "node", Args: []string{"src/check.cjs"}, Directory: ".", TimeoutMillis: 30000}
	// Isolate the rules/version gate from CLI and assertion adapters.
	addWorkflowEvidenceForTest(t, s, ss.ID, taskID, execution, "command", pc.RevisionID, true, supervisionCheckResult{Check: check, ExitCode: 0})
	addWorkflowEvidenceForTest(t, s, ss.ID, taskID, execution, "review", pc.RevisionID, true, map[string]string{"verdict": "pass"})
	writeSupervisionSourceForTest(t, filepath.Dir(p.Path), "AGENTS.md", "New inherited requirement: verify edge cases.\n")
	observed, err := s.captureSupervisionProjectContext(ctx, ss.ID, ss.StateVersion)
	if !errors.Is(err, errSupervisionSnapshotChanged) || observed.RevisionID != pc.RevisionID || observed.RulesHash == pc.RulesHash {
		t.Fatal("fixture must change inherited rules without changing the project source revision", observed, err)
	}
	ss = supervisionSessionForTest(t, s, ss.ID)
	if err := s.resumeSupervisionSession(ctx, ss.ID, ss.StateVersion, true, false); err != nil {
		t.Fatal(err)
	}
	current, err := loadSupervisionProjectContext(ctx, s.db, ss.ID, "current")
	if err != nil {
		t.Fatal(err)
	}
	action := supervisionWorkflowAction{Type: "record_acceptance", SessionID: ss.ID, TaskID: taskID, RequirementsRevision: 1, PlanRevision: 1, ContextID: current.ID}
	var validation error
	if err := s.withSupervisionTx(ctx, ss.ID, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, latest *supervisionSession) error {
		validation = s.validateSupervisionWorkflowTx(ctx, tx, latest, action, current)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(validation, errSupervisionPlanInvalid) {
		t.Fatal("old tests and review were accepted under newly inherited project rules", validation)
	}
}

func TestSupervisionPendingDriftCannotBeClearedByAmendmentsOrLegacyMigration(t *testing.T) {
	for _, mode := range []string{"current", "legacy", "legacy_implicit_resume"} {
		t.Run(mode, func(t *testing.T) {
			s, p, _, ss, _ := supervisionPlanFixture(t)
			ctx := context.Background()
			writeSupervisionSourceForTest(t, p.Path, "AGENTS.md", "Require additional compatibility checks.\n")
			if _, err := s.captureSupervisionProjectContext(ctx, ss.ID, ss.StateVersion); !errors.Is(err, errSupervisionSnapshotChanged) {
				t.Fatal(err)
			}
			if mode == "legacy_implicit_resume" {
				// Old resume could incorrectly confirm success without an explicit
				// drift decision after user pause replaced the original reason.
				if err := s.withSupervisionTx(ctx, ss.ID, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, latest *supervisionSession) error {
					latest.State, latest.Reason = "running", ""
					latest.StateVersion++
					return appendSupervisionEventTx(ctx, tx, latest, "session.resumed", map[string]bool{"acceptedDrift": false}, time.Now().UTC())
				}); err != nil {
					t.Fatal(err)
				}
			}
			if mode != "current" {
				if _, err := s.db.Exec(`alter table supervision_context_state drop column pending_base_id`); err != nil {
					t.Fatal(err)
				}
				if err := s.migrateSupervisionContext(ctx); err != nil {
					t.Fatal(err)
				}
			}
			ss = supervisionSessionForTest(t, s, ss.ID)
			response := supervisionRequestForTest(s, "POST", "/api/supervision/"+ss.ID+"/instructions", "instruction-before-drift-review", fmt.Sprintf(`{"stateVersion":%d,"content":"继续推进任务并考虑新增要求","immediate":false}`, ss.StateVersion))
			if response.Code != 202 {
				t.Fatal(response.Code, response.Body.String())
			}
			if _, err := supervisionWorkerForTest(s, nil).maintain(ctx, ss.ID); err != nil {
				t.Fatal(err)
			}
			ss = supervisionSessionForTest(t, s, ss.ID)
			if err := s.resumeSupervisionSession(ctx, ss.ID, ss.StateVersion, false, false); !errors.Is(err, errSupervisionSnapshotChanged) {
				t.Fatal("new instructions erased the required workspace confirmation", ss, err)
			}
			if _, err := s.acquireSupervisionLease(ctx, ss.ID, "must-not-run", time.Now().UTC()); err == nil {
				t.Fatal("pending drift allowed scheduling")
			}
			if err := s.resumeSupervisionSession(ctx, ss.ID, ss.StateVersion, true, false); err != nil {
				t.Fatal(err)
			}
			if err := s.migrateSupervisionContext(ctx); err != nil {
				t.Fatal(err)
			}
			if err := requireSupervisionNoPendingDrift(ctx, s.db, ss.ID); err != nil {
				t.Fatal("repeated migration reintroduced an accepted observation", err)
			}
		})
	}
}
