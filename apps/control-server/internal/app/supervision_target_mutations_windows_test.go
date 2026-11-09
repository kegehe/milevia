//go:build windows

package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

// Planning/dispatch uses the existing domain path. Only model output is a
// fixture: the target snapshot, file edits, journal and readback are real.
func supervisionTargetMutationFixture(t *testing.T, mode string) (*Server, supervisionExecutionPermit, *targetexec.Store, string, *atomic.Int32) {
	t.Helper()
	s, project, taskID, ss, pc := supervisionPlanFixture(t)
	if _, err := s.db.Exec(`update supervision_sessions set scheduler_agent_id='claude-code' where id=?`, ss.ID); err != nil {
		t.Fatal(err)
	}
	applyWorkflowActionForTest(t, s, ss.ID, validSupervisionPlanActionForTest(ss, taskID, pc))
	applyWorkflowActionForTest(t, s, ss.ID, supervisionWorkflowAction{Type: "dispatch_implementation", SessionID: ss.ID, TaskID: taskID, RequirementsRevision: 1, PlanRevision: 1, ContextID: pc.ID, Instruction: "Update the selected source"})
	permit, _ := workflowPermitForTest(t, s, ss.ID)
	ss = supervisionSessionForTest(t, s, ss.ID)
	var workspace targetexec.Workspace
	if err := s.db.QueryRow(`select directory_identity,canonical_path from workspace_admissions where id=?`, ss.AdmissionID).Scan(&workspace.ID, &workspace.Path); err != nil {
		t.Fatal(err)
	}
	store, err := targetexec.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.RegisterWorkspace(workspace); err != nil {
		t.Fatal(err)
	}
	targetID, err := store.Identity()
	if err != nil {
		t.Fatal(err)
	}
	artifact := targetexec.HelperArtifact{OS: runtime.GOOS, Arch: runtime.GOARCH, SHA256: strings.Repeat("a", 64)}
	runnerID := s.localRunnerID()
	if err := s.recordSupervisionTargetBinding(context.Background(), runnerID, &supervisionTargetLink{TargetID: targetID, Artifact: artifact}); err != nil {
		t.Fatal(err)
	}
	if err := s.stageSupervisionTargetControl(context.Background(), ss.ID, runnerID, targetID, workspace.ID); err != nil {
		t.Fatal(err)
	}
	target := &supervisionTargetRuntime{store: store, storage: t.TempDir(), config: Config{WorkspaceGuardDir: t.TempDir()}}
	executor := &supervisionTargetTurnTestExecutor{store: store}
	supervisor, err := targetexec.NewSupervisor(context.Background(), store, executor)
	if err != nil {
		t.Fatal(err)
	}
	left, right := net.Pipe()
	done := make(chan error, 1)
	calls := new(atomic.Int32)
	transport := &targetexec.Transport{Store: store, Supervisor: supervisor, ValidateControl: target.validateControl, Extension: func(ctx context.Context, request targetexec.Request) (json.RawMessage, error) {
		data, err := target.handle(ctx, request)
		if request.Method != "mutate" {
			return data, err
		}
		calls.Add(1)
		if err == nil {
			switch mode {
			case "lost-reply":
				return nil, targetexec.ErrClosed
			case "corrupt-reply":
				var receipt targetexec.Mutation
				if json.Unmarshal(data, &receipt) != nil {
					return nil, targetexec.ErrProtocol
				}
				receipt.OutputRevision = strings.Repeat("b", 64)
				return mustJSON(receipt), nil
			case "stopped-before-commit":
				current := supervisionSessionForTest(t, s, ss.ID)
				if err := s.requestSupervisionControl(ctx, ss.ID, current.StateVersion, "stop", time.Now().UTC()); err != nil {
					return nil, err
				}
			}
		}
		return data, err
	}}
	go func() { done <- transport.Serve(context.Background(), right) }()
	client := targetexec.NewClient(left)
	if _, err := client.Call(context.Background(), targetexec.Request{ID: "hello", Method: "hello"}); err != nil {
		t.Fatal(err)
	}
	link := &supervisionTargetLink{Client: client, TargetID: targetID, Artifact: artifact}
	s.supervisionTargets = newSupervisionTargetConnections(s.runtimeCtx, func(context.Context, string) (*supervisionTargetLink, error) { return link, nil })
	t.Cleanup(func() { s.supervisionTargets.Close(); client.Close(); <-done })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.deliverSupervisionTargetControl(ctx, link, ss.ID); err != nil {
		t.Fatal(err)
	}
	intent, err := loadSupervisionTargetControl(ctx, s.db, ss.ID)
	if err != nil {
		t.Fatal(err)
	}
	input, err := s.supervisionTargetExecutionInput(ctx, permit)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.recordSupervisionTargetResolution(ctx, permit, intent.Control, input, supervisionCLIInfo{AgentID: permit.AgentID, Path: `C:\target\fixture.exe`, Version: "1.0", Contract: "fixture-v1"}); err != nil {
		t.Fatal(err)
	}
	if out, err := s.runSupervisionTargetTurn(ctx, link, permit); err != nil || out.RunError != nil {
		t.Fatal("fixture implementation did not settle", out.RunError, err)
	}
	// Poison only the controller's path. The target keeps the actual directory
	// identity and applies against that root, never a controller-side os.Root.
	if err := s.withWorkspaceAdmissionTx(ctx, func(tx *sql.Tx, admissions map[string]WorkspaceAdmission) error {
		a := admissions[ss.AdmissionID]
		a.OriginalPath = filepath.Join(t.TempDir(), "not-on-controller")
		admissions[a.ID] = a
		_, err := tx.Exec(`update workspace_admissions set original_path=? where id=?`, a.OriginalPath, a.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return s, permit, store, project.Path, calls
}

func TestSupervisionTargetMutationsApplyAndReconcileRealFilesWithoutControllerAccess(t *testing.T) {
	for _, mode := range []string{"complete", "lost-reply", "corrupt-reply", "stopped-before-commit", "outside-scope", "wrong-base"} {
		t.Run(mode, func(t *testing.T) {
			s, permit, target, root, calls := supervisionTargetMutationFixture(t, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			job, err := loadSupervisionRoleJob(ctx, s.db, permit.Decision.ExecutionID)
			if err != nil {
				t.Fatal(err)
			}
			edit := supervisionEdit{Path: "src/main.go", BaseSHA256: supervisionSHA256([]byte("package baseline\n")), Content: "package updated\n"}
			if mode == "outside-scope" {
				edit.Path, edit.BaseSHA256 = "outside.txt", ""
			}
			if mode == "wrong-base" {
				edit.BaseSHA256 = strings.Repeat("c", 64)
			}
			revision, err := s.applySupervisionEdits(ctx, permit, job, []supervisionEdit{edit})
			if mode == "outside-scope" || mode == "wrong-base" {
				if err == nil || calls.Load() != 0 {
					t.Fatal("invalid edit reached target mutation", calls.Load(), err)
				}
				return
			}
			if (err == nil) != (mode == "complete") {
				t.Fatal("unexpected mutation outcome", revision, err)
			}
			data, readErr := os.ReadFile(filepath.Join(root, "src", "main.go"))
			if readErr != nil || string(data) != "package updated\n" || calls.Load() != 1 {
				t.Fatal("actual target files not updated once", string(data), calls.Load(), readErr)
			}
			actual, queryErr := target.Mutation("edit-" + job.ExecutionID)
			if queryErr != nil || actual.State != "completed" || actual.OutputRevision == "" {
				t.Fatal("target durable completion missing", actual, queryErr)
			}
			var state, receipt string
			if err := s.db.QueryRow(`select m.state,t.receipt from supervision_mutations m join supervision_target_mutations t on t.execution_id=m.execution_id where m.execution_id=?`, job.ExecutionID).Scan(&state, &receipt); err != nil {
				t.Fatal(err)
			}
			if mode == "complete" {
				if state != "completed" || receipt == "" || revision != actual.OutputRevision {
					t.Fatal("confirmed target mutation not recorded", state, receipt, revision)
				}
			} else if state != "unknown" || receipt != "" {
				t.Fatal("unconfirmed target mutation accepted", state, receipt)
			}
			if _, err := s.applySupervisionEdits(ctx, permit, job, []supervisionEdit{edit}); err == nil || calls.Load() != 1 {
				t.Fatal("target edits replayed", calls.Load(), err)
			}
			ss := supervisionSessionForTest(t, s, permit.Decision.SessionID)
			if mode == "stopped-before-commit" && ss.Control != "stop" {
				t.Fatal("mutation cleanup lost stop request")
			}
			var ownership string
			if err := s.db.QueryRow(`select owner_id from workspace_admissions where id=?`, ss.AdmissionID).Scan(&ownership); err != nil || ownership != ss.ID {
				t.Fatal("mutation released workspace protection", ownership, err)
			}
		})
	}
}

func TestSupervisionTargetMutationReceiptRequiresCompleteMatchingFacts(t *testing.T) {
	spec := targetexec.MutationSpec{ID: "id", Files: []targetexec.FileChange{{Path: "source.txt"}}}
	receipt := targetexec.Mutation{Spec: spec, Digest: supervisionSHA256(mustJSON(spec)), State: "completed", Files: []string{"applied"}, OutputRevision: strings.Repeat("a", 64)}
	if !supervisionTargetMutationCompleted(spec, receipt) {
		t.Fatal("complete receipt rejected")
	}
	for _, change := range []func(*targetexec.Mutation){
		func(r *targetexec.Mutation) { r.State = "unknown" },
		func(r *targetexec.Mutation) { r.Digest = "wrong" },
		func(r *targetexec.Mutation) { r.Files = []string{"writing"} },
		func(r *targetexec.Mutation) { r.Files = nil },
		func(r *targetexec.Mutation) { r.Spec.ID = "foreign" },
		func(r *targetexec.Mutation) { r.OutputRevision = strings.Repeat("z", 64) },
	} {
		other := receipt
		change(&other)
		if supervisionTargetMutationCompleted(spec, other) {
			t.Fatal("unconfirmed receipt accepted")
		}
	}
}

func TestSupervisionTargetImplementationCompletionAdvancesDomainAndCapturesTargetRevision(t *testing.T) {
	s, permit, _, _, calls := supervisionTargetMutationFixture(t, "complete")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	raw := string(mustJSON(supervisionImplementationResult{Summary: "Update the selected source", Edits: []supervisionEdit{{Path: "src/main.go", BaseSHA256: supervisionSHA256([]byte("package baseline\n")), Content: "package updated\n"}}}))
	if err := s.completeSupervisionRole(ctx, permit, raw, nil); err != nil {
		t.Fatal("target-backed role completion failed", err)
	}
	job, err := loadSupervisionRoleJob(ctx, s.db, permit.Decision.ExecutionID)
	if err != nil || job.State != "completed" || job.OutputRevision == "" || job.OutputRevision == job.InputRevision || calls.Load() != 1 {
		t.Fatal("role did not record confirmed target output", job, calls.Load(), err)
	}
	pc, err := loadSupervisionProjectContext(ctx, s.db, permit.Decision.SessionID, "current")
	if err != nil || pc.RevisionID != job.OutputRevision {
		t.Fatal("post-implementation context missed target revision", pc.RevisionID, err)
	}
	var phase, status string
	if err := s.db.QueryRow(`select st.phase,t.status from supervision_tasks st join tasks t on t.id=st.task_id where st.session_id=? and st.task_id=?`, permit.Decision.SessionID, permit.TaskID).Scan(&phase, &status); err != nil || phase != "implemented" || status == "done" {
		t.Fatal("implementation skipped independent acceptance", phase, status, err)
	}
	if err := s.completeSupervisionRole(ctx, permit, raw, nil); err != nil || calls.Load() != 1 {
		t.Fatal("role completion replayed target mutation", calls.Load(), err)
	}
}
