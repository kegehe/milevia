package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

// Real filesystem/protocol/CAS fixture, without CLI or model calls. Controller
// and target deliberately have different contents and different root paths.
func supervisionWorkspaceTargetFixture(t *testing.T, afterCapture func(*Server, string)) (*Server, string, *supervisionTargetLink) {
	t.Helper()
	s, project, _, ss, now := supervisionFixture(t, defaultSupervisionBudget())
	writeSupervisionSourceForTest(t, project.Path, "source.txt", "controller decoy")
	startSupervisionForTest(t, s, ss.ID, now)
	if _, err := s.acquireSupervisionLease(context.Background(), ss.ID, "workspace-test", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	targetRoot := t.TempDir()
	writeSupervisionSourceForTest(t, targetRoot, "source.txt", "actual target source")
	writeSupervisionSourceForTest(t, targetRoot, "AGENTS.md", "Do not publish.")
	targetAdmission := snapshotAdmissionForTest(t, targetRoot)
	current := supervisionSessionForTest(t, s, ss.ID)
	err := s.withWorkspaceAdmissionTx(context.Background(), func(tx *sql.Tx, admissions map[string]WorkspaceAdmission) error {
		a := admissions[current.AdmissionID]
		a.CanonicalPath, a.DirectoryIdentity = targetAdmission.CanonicalPath, targetAdmission.DirectoryIdentity
		// The target path cannot be verified by looking at this controller path.
		a.OriginalPath = filepath.Join(t.TempDir(), "unavailable-on-controller")
		admissions[a.ID] = a
		_, err := tx.Exec(`update workspace_admissions set original_path=?,canonical_path=?,directory_identity=? where id=?`, a.OriginalPath, a.CanonicalPath, a.DirectoryIdentity, a.ID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := targetexec.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.RegisterWorkspace(targetexec.Workspace{ID: targetAdmission.DirectoryIdentity, Path: targetAdmission.CanonicalPath}); err != nil {
		t.Fatal(err)
	}
	targetID, err := store.Identity()
	if err != nil {
		t.Fatal(err)
	}
	runnerID := s.localRunnerID()
	artifact := targetexec.HelperArtifact{OS: runtime.GOOS, Arch: runtime.GOARCH, SHA256: strings.Repeat("a", 64)}
	if err := s.recordSupervisionTargetBinding(context.Background(), runnerID, &supervisionTargetLink{TargetID: targetID, Artifact: artifact}); err != nil {
		t.Fatal(err)
	}
	if err := s.stageSupervisionTargetControl(context.Background(), ss.ID, runnerID, targetID, targetAdmission.DirectoryIdentity); err != nil {
		t.Fatal(err)
	}
	target := &supervisionTargetRuntime{store: store, storage: t.TempDir(), config: Config{WorkspaceGuardDir: t.TempDir()}}
	supervisor, err := targetexec.NewSupervisor(context.Background(), store, target)
	if err != nil {
		t.Fatal(err)
	}
	left, right := net.Pipe()
	done := make(chan error, 1)
	var captures atomic.Int32
	transport := &targetexec.Transport{Store: store, Supervisor: supervisor, ValidateControl: target.validateControl, Extension: func(ctx context.Context, request targetexec.Request) (json.RawMessage, error) {
		data, err := target.handle(ctx, request)
		if (request.Method == "snapshot" || request.Method == "snapshot.reconcile") && err == nil && captures.Add(1) == 2 && afterCapture != nil {
			afterCapture(s, ss.ID)
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
	if err := s.deliverSupervisionTargetControl(context.Background(), link, ss.ID); err != nil {
		t.Fatal(err)
	}
	return s, ss.ID, link
}

func pauseSupervisionWorkspaceTargetForTest(t *testing.T, s *Server, id string) {
	t.Helper()
	ss := supervisionSessionForTest(t, s, id)
	if err := s.requestSupervisionControl(context.Background(), id, ss.StateVersion, "pause", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.settleSupervisionControl(context.Background(), id, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}

func TestSupervisionWorkspaceTargetStopConfirmsReleaseBeforeControllerAdmission(t *testing.T) {
	s, id, link := supervisionWorkspaceTargetFixture(t, nil)
	ss := supervisionSessionForTest(t, s, id)
	if err := s.requestSupervisionControl(context.Background(), id, ss.StateVersion, "stop", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.settleSupervisionControl(context.Background(), id, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	response, err := link.Call(context.Background(), targetexec.Request{ID: "stopped-query", Method: "query", SessionID: id})
	if err != nil || response.Session == nil || !response.Session.Released || response.Session.Control.State != "stopped" {
		t.Fatal("controller released without target tombstone", response.Session, err)
	}
	var count int
	if err := s.db.QueryRow(`select count(*) from workspace_admissions where owner_id=?`, id).Scan(&count); err != nil || count != 0 {
		t.Fatal("confirmed target stop did not release controller", count, err)
	}
	if err := s.db.QueryRow(`select count(*) from supervision_target_releases where session_id=?`, id).Scan(&count); err != nil || count != 1 {
		t.Fatal("target release proof not persisted", count, err)
	}
}

func TestSupervisionWorkspaceTargetDisconnectedControlKeepsProtection(t *testing.T) {
	for _, control := range []string{"pause", "stop"} {
		t.Run(control, func(t *testing.T) {
			s, id, link := supervisionWorkspaceTargetFixture(t, nil)
			ss := supervisionSessionForTest(t, s, id)
			if err := s.requestSupervisionControl(context.Background(), id, ss.StateVersion, control, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			link.Client.Close()
			if err := s.settleSupervisionControl(context.Background(), id, time.Now().UTC()); err == nil {
				t.Fatal("disconnected target control was considered settled")
			}
			var held bool
			if err := s.db.QueryRow(`select exists(select 1 from workspace_admissions where owner_id=?)`, id).Scan(&held); err != nil || !held {
				t.Fatal("disconnected control released protection", held, err)
			}
		})
	}
}

func TestSupervisionWorkspaceResumeReadsPausedTargetWithoutAuthorizingExecution(t *testing.T) {
	s, id, link := supervisionWorkspaceTargetFixture(t, nil)
	ctx := context.Background()
	ss := supervisionSessionForTest(t, s, id)
	if _, err := s.captureSupervisionProjectContext(ctx, id, ss.StateVersion); err != nil {
		t.Fatal(err)
	}
	pauseSupervisionWorkspaceTargetForTest(t, s, id)
	ss = supervisionSessionForTest(t, s, id)
	if err := s.resumeSupervisionSession(ctx, id, ss.StateVersion, false, false); err != nil {
		t.Fatal("target resume used controller filesystem", err)
	}
	ss = supervisionSessionForTest(t, s, id)
	if ss.State != "running" || ss.Control != "" || ss.RecoveryState != "ready" || ss.LeaseOwner != "" {
		t.Fatal("resume did not open a fresh unowned scheduling generation", ss)
	}
	response, err := link.Call(ctx, targetexec.Request{ID: "resume-query", Method: "query", SessionID: id})
	if err != nil || response.Session == nil || response.Session.Control.State != "paused" || len(response.Session.Executions) != 0 || response.Session.Released {
		t.Fatal("read-only recovery authorized execution or released protection", response.Session, err)
	}
}

func TestSupervisionWorkspaceResumeDetectsActualTargetDrift(t *testing.T) {
	s, id, _ := supervisionWorkspaceTargetFixture(t, nil)
	ctx := context.Background()
	ss := supervisionSessionForTest(t, s, id)
	if _, err := s.captureSupervisionProjectContext(ctx, id, ss.StateVersion); err != nil {
		t.Fatal(err)
	}
	pauseSupervisionWorkspaceTargetForTest(t, s, id)
	ss = supervisionSessionForTest(t, s, id)
	var root string
	if err := s.db.QueryRow(`select canonical_path from workspace_admissions where id=?`, ss.AdmissionID).Scan(&root); err != nil {
		t.Fatal(err)
	}
	writeSupervisionSourceForTest(t, root, "source.txt", "actual target was edited")
	if err := s.resumeSupervisionSession(ctx, id, ss.StateVersion, false, false); !errors.Is(err, errSupervisionSnapshotChanged) {
		t.Fatal("target drift ignored", err)
	}
	if err := s.resumeSupervisionSession(ctx, id, ss.StateVersion, true, false); err != nil {
		t.Fatal("accepted target drift could not resume", err)
	}
	pc, err := loadSupervisionProjectContext(ctx, s.db, id, "current")
	if err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err := s.db.QueryRow(`select exists(select 1 from supervision_revision_blobs where sha256=?)`, supervisionSHA256([]byte("actual target was edited"))).Scan(&exists); err != nil || !exists || pc.RevisionID == "" {
		t.Fatal("accepted target source missing", exists, err)
	}
}

func TestSupervisionWorkspaceCapturePersistsTargetFilesInsteadOfControllerDecoy(t *testing.T) {
	s, id, _ := supervisionWorkspaceTargetFixture(t, nil)
	ss := supervisionSessionForTest(t, s, id)
	pc, err := s.captureSupervisionProjectContext(context.Background(), id, ss.StateVersion)
	if err != nil {
		t.Fatal(err)
	}
	var actual []byte
	if err := s.db.QueryRow(`select content from supervision_revision_blobs where sha256=?`, supervisionSHA256([]byte("actual target source"))).Scan(&actual); err != nil || string(actual) != "actual target source" {
		t.Fatal("target source not retained in CAS", string(actual), err)
	}
	var decoy bool
	if err := s.db.QueryRow(`select exists(select 1 from supervision_revision_blobs where sha256=?)`, supervisionSHA256([]byte("controller decoy"))).Scan(&decoy); err != nil || decoy {
		t.Fatal("controller source substituted for target", decoy, err)
	}
	if pc.Manifest.DirectoryIdentity == "" || len(pc.Rules) == 0 {
		t.Fatal("target identity/rules lost", pc)
	}
}

func TestSupervisionWorkspaceCaptureRejectsRevocationBeforeCommit(t *testing.T) {
	s, id, _ := supervisionWorkspaceTargetFixture(t, func(s *Server, id string) {
		ss := supervisionSessionForTest(t, s, id)
		if err := s.requestSupervisionControl(context.Background(), id, ss.StateVersion, "stop", time.Now().UTC()); err != nil {
			t.Error(err)
		}
	})
	ss := supervisionSessionForTest(t, s, id)
	if _, err := s.captureSupervisionProjectContext(context.Background(), id, ss.StateVersion); err == nil {
		t.Fatal("revoked snapshot became current")
	}
	if _, err := loadSupervisionProjectContext(context.Background(), s.db, id, "current"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("revoked capture changed context", err)
	}
	ss = supervisionSessionForTest(t, s, id)
	if ss.Control != "stop" {
		t.Fatal("capture cleanup changed user's control", ss.Control)
	}
}

func TestSupervisionWorkspaceCaptureCannotFallBackAfterTargetDisconnect(t *testing.T) {
	s, id, link := supervisionWorkspaceTargetFixture(t, nil)
	link.Client.Close()
	ss := supervisionSessionForTest(t, s, id)
	if _, err := s.captureSupervisionProjectContext(context.Background(), id, ss.StateVersion); err == nil {
		t.Fatal("disconnected target read controller files")
	}
	if _, err := loadSupervisionProjectContext(context.Background(), s.db, id, "current"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("failed target capture changed current context", err)
	}
	ss = supervisionSessionForTest(t, s, id)
	if ss.Reason != "workspace_capture_failed" || ss.AdmissionID == "" {
		t.Fatal("failed capture lost failure state or protection", ss)
	}
}

func TestSupervisionWorkspaceEnvironmentUsesActualTargetDependencies(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node unavailable")
	}
	s, id, _ := supervisionWorkspaceTargetFixture(t, nil)
	ctx := context.Background()
	var source supervisionWorkspaceSource
	if err := s.withSupervisionTx(ctx, id, time.Now().UTC(), func(tx *sql.Tx, admissions map[string]WorkspaceAdmission, ss *supervisionSession) error {
		var err error
		source, err = s.supervisionWorkspaceSourceTx(ctx, tx, ss, admissions[ss.AdmissionID])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	plan := supervisionTaskPlan{Goal: "Check target dependency environment", Scope: []string{"source.txt"}, Acceptance: []supervisionAcceptancePoint{{ID: "actual", Requirement: "Resolve target runtime", Kind: "command", Verification: "Run node", Critical: true}}, Environment: []string{"node"}, Review: []string{"Inspect the target environment"}}
	check := supervisionCheck{ID: "actual", AcceptanceIDs: []string{"actual"}, Program: "node", Args: []string{"--version"}, Directory: ".", TimeoutMillis: 10000}
	actual, err := source.environment(ctx, s, plan, check)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := supervisionCheckEnvironment(ctx, source.Admission.CanonicalPath, check)
	if err != nil || actual != expected {
		t.Fatal("target environment mismatch", actual, expected, err)
	}
	writeSupervisionSourceForTest(t, source.Admission.CanonicalPath, "node_modules/pkg/index.js", "module.exports=2;")
	after, err := source.environment(ctx, s, plan, check)
	if err != nil || after == actual {
		t.Fatal("target dependency change was missed", after, err)
	}
}
