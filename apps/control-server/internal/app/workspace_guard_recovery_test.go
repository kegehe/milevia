package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
	"golang.org/x/crypto/ssh"
)

func TestWorkspaceGuardScopedSSHFailureKeepsIndependentProjectUsable(t *testing.T) {
	client := workspaceSSHClientForTest(t, func(channel ssh.Channel, req *ssh.Request) {
		_ = req.Reply(true, nil) // Deliberately omit the target exit receipt.
	})
	bad := client.workspaceFailures.watch("/projects/first")
	alias := client.workspaceFailures.watch("/projects/first/src")
	good := client.workspaceFailures.watch("/projects/second")
	if _, err := client.execCommand(workspaceCommandContext(context.Background(), "/projects/first"), "fixture"); err == nil {
		t.Fatal("missing exit receipt was accepted")
	}
	if bad() || alias() {
		t.Fatal("failed project or child scope lost its protection")
	}
	if !good() || client.workspaceUncertainty.Load() != 0 {
		t.Fatal("a scoped channel failure poisoned the independent project")
	}
}

func TestWorkspaceGuardScopedSFTPAndWSLFailures(t *testing.T) {
	client := &sshClient{}
	first, second := client.workspaceFailures.watch("/first"), client.workspaceFailures.watch("/second")
	client.workspaceSFTPError(errors.New("lost write response"), "/first/file")
	if first() || !second() {
		t.Fatal("SFTP failure crossed project boundaries")
	}
	tracker := workspaceWSLFailureTracker(t.Name())
	linux := tracker.watch("/ProjectCase")
	unrelated := tracker.watch("/projectcase")
	noteWorkspaceWSLExit(t.Name(), context.Background(), errors.New("launcher lost"), true, "/ProjectCase/src")
	if linux() || !unrelated() {
		t.Fatal("Linux directory case or WSL scope was lost")
	}
	if workspaceWSLTrackingPath("Ubuntu", `C:\Work\Project`) != workspaceWSLTrackingPath("Ubuntu", "/mnt/c/work/project") {
		t.Fatal("Windows mounted aliases did not share tracking scope")
	}
}

func TestWorkspaceGuardScopedAdmissionReleasesIndependentWriter(t *testing.T) {
	s := newTestServer(t)
	remote := &sshClient{}
	s.runnerRegistry.register("ssh-scoped", &sshRunner{client: remote}, RunnerMeta{ID: "ssh-scoped"})
	left, right := net.Pipe()
	transport := &targetexec.Transport{EndpointIdentity: "scoped-journal", Extension: func(_ context.Context, request targetexec.Request) (json.RawMessage, error) {
		if request.Method == "health" {
			return mustJSON(map[string]any{"protocol": targetexec.ProtocolVersion, "workspaceGuard": true}), nil
		}
		var record workspaceGuardRecord
		if json.Unmarshal(request.Parameters, &record) != nil {
			return nil, targetexec.ErrProtocol
		}
		switch request.Method {
		case "guard.begin":
			record.State = "active"
			if record.Path == "/first-alias" {
				record.Path = "/actual-first"
			}
		case "guard.release":
			record.State = "released"
		default:
			return nil, targetexec.ErrProtocol
		}
		return mustJSON(record), nil
	}}
	done := make(chan error, 1)
	go func() { done <- transport.Serve(context.Background(), right) }()
	client := targetexec.NewClient(left)
	if _, err := client.Call(context.Background(), targetexec.Request{ID: "hello", Method: "hello"}); err != nil {
		t.Fatal(err)
	}
	link := &supervisionTargetLink{Client: client, TargetID: "scoped-journal"}
	s.workspaceGuardTargets = newSupervisionTargetConnections(s.runtimeCtx, func(context.Context, string) (*supervisionTargetLink, error) { return link, nil })
	t.Cleanup(func() { s.workspaceGuardTargets.Close(); client.Close(); <-done })
	first, err := s.beginWorkspaceOperationAtPath(Project{Runner: "ssh-scoped"}, "/first-alias", "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.beginWorkspaceOperationAtPath(Project{Runner: "ssh-scoped"}, "/second", "second")
	if err != nil {
		t.Fatal(err)
	}
	remote.workspaceSFTPError(io.EOF, "/actual-first/input.txt")
	first()
	second()
	operations, err := s.workspaceGuardOperations(context.Background(), "", false, 100)
	if err != nil || len(operations) != 1 || operations[0].Path != "/first-alias" || operations[0].State != "unconfirmed" {
		t.Fatal("independent admission was retained or uncertain writer was released", operations, err)
	}
}

func TestWorkspaceGuardRecoveryRunsWithoutAnySupervisionSession(t *testing.T) {
	s, p, _ := admissionProjectForTest(t)
	release, err := s.beginWorkspaceOperationAtPath(p, p.Path, "writer")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	// No process runs in this fixture: record an already confirmed exit whose
	// first release delivery was interrupted before the maintenance loop starts.
	if _, err := s.db.Exec(`update workspace_guard_operations set state='release_pending'`); err != nil {
		t.Fatal(err)
	}
	if !s.startSupervisionWorker(func(context.Context, supervisionDecision, supervisionLease) error {
		return errors.New("unexpected model dispatch")
	}, 10*time.Millisecond) {
		t.Fatal("worker did not start")
	}
	waitSupervisionWorkerForTest(t, func() bool {
		var remaining int
		return s.db.QueryRow(`select count(*) from workspace_guard_operations`).Scan(&remaining) == nil && remaining == 0
	})
}

func TestWorkspaceGuardRecoveryRetriesOnlyConfirmedReleaseAndPinsJournal(t *testing.T) {
	s, p, _ := admissionProjectForTest(t)
	release, err := s.beginWorkspaceOperationAtPath(p, p.Path, "writer")
	if err != nil {
		t.Fatal(err)
	}
	original := s.config.WorkspaceGuardDir
	// Simulate a release outage by pointing the caller at another journal.
	// A matching local path must not be enough to accept that other identity.
	s.config.WorkspaceGuardDir = t.TempDir()
	release()
	operations, err := s.workspaceGuardOperations(context.Background(), "", true, 100)
	if err != nil || len(operations) != 1 || operations[0].TargetID == "" {
		t.Fatal(operations, err)
	}
	if err := s.reconcileWorkspaceGuardOperation(context.Background(), operations[0]); !errors.Is(err, targetexec.ErrAuthority) {
		t.Fatal("replacement journal accepted release", err)
	}
	s.config.WorkspaceGuardDir = original
	if _, err := changeWorkspaceGuard(context.Background(), s.config, "query", workspaceGuardRecord{ID: operations[0].ID, Kind: "operation"}); err != nil {
		t.Fatal("failed release destroyed original target token", err)
	}
	if err := s.reconcilePendingWorkspaceGuardOperations(context.Background()); err != nil {
		t.Fatal(err)
	}
	remaining, err := s.workspaceGuardOperations(context.Background(), "", false, 100)
	if err != nil || len(remaining) != 0 {
		t.Fatal("confirmed release was not recovered", remaining, err)
	}
	actual, err := changeWorkspaceGuard(context.Background(), s.config, "query", workspaceGuardRecord{ID: operations[0].ID, Kind: "operation"})
	if err != nil || actual.State != "released" {
		t.Fatal("recovery did not obtain actual release receipt", actual, err)
	}
}

func TestWorkspaceGuardRecoveryDoesNotReleaseUnknownWriter(t *testing.T) {
	s, p, _ := admissionProjectForTest(t)
	release, err := s.beginWorkspaceOperationAtPath(p, p.Path, "writer")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := s.recoverWorkspaceAdmissions(context.Background()); err != nil {
		t.Fatal(err)
	}
	operations, err := s.workspaceGuardOperations(context.Background(), "", false, 100)
	if err != nil || len(operations) != 1 || operations[0].State != "unconfirmed" {
		t.Fatal(operations, err)
	}
	if err := s.reconcilePendingWorkspaceGuardOperations(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcileWorkspaceGuardOperation(context.Background(), operations[0]); !errors.Is(err, errWorkspaceNotQuiescent) {
		t.Fatal("unknown writer was force released", err)
	}
	response := httptest.NewRecorder()
	s.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects/"+p.ID+"/workspace-operations/"+operations[0].ID+"/reconcile", nil))
	if response.Code != http.StatusConflict {
		t.Fatal(response.Code, response.Body.String())
	}
	if _, err := changeWorkspaceGuard(context.Background(), s.config, "release", workspaceGuardRecord{ID: operations[0].ID, Kind: "operation"}); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcileWorkspaceGuardOperation(context.Background(), operations[0]); err != nil {
		t.Fatal("already released target receipt could not reconcile", err)
	}
}

func TestWorkspaceGuardRecoveryPersistsExitBeforeDeleteFailure(t *testing.T) {
	s, p, _ := admissionProjectForTest(t)
	release, err := s.beginWorkspaceOperationAtPath(p, p.Path, "writer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`create trigger fail_guard_delete before delete on workspace_guard_operations begin select raise(abort,'lost acknowledgement'); end`); err != nil {
		t.Fatal(err)
	}
	release()
	operations, err := s.workspaceGuardOperations(context.Background(), "", true, 100)
	if err != nil || len(operations) != 1 {
		t.Fatal("release intent was lost after target success", operations, err)
	}
	if _, err := s.db.Exec(`drop trigger fail_guard_delete`); err != nil {
		t.Fatal(err)
	}
	if err := s.recoverWorkspaceAdmissions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcilePendingWorkspaceGuardOperations(context.Background()); err != nil {
		t.Fatal(err)
	}
	operations, err = s.workspaceGuardOperations(context.Background(), "", false, 100)
	if err != nil || len(operations) != 0 {
		t.Fatal("restart did not finish durable confirmed release", operations, err)
	}
}

func TestWorkspaceGuardExpiredBeginIsNotAnExpiredWriter(t *testing.T) {
	s, p, _ := admissionProjectForTest(t)
	release, err := s.beginWorkspaceOperationAtPath(p, p.Path, "writer")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	operations, err := s.workspaceGuardOperations(context.Background(), "", false, 100)
	if err != nil || len(operations) != 1 {
		t.Fatal(operations, err)
	}
	token := workspaceGuardRecord{ID: operations[0].ID, Kind: "operation", JournalID: operations[0].TargetID, ValidUntil: time.Now().Add(-time.Hour)}
	actual, err := changeWorkspaceGuard(context.Background(), s.config, "query", token)
	if err != nil || actual.State != "active" {
		t.Fatal("expired begin window released a live writer", actual, err)
	}
	token.ID = "never-started"
	token.ValidUntil = time.Now().Add(time.Hour)
	if _, err := changeWorkspaceGuard(context.Background(), s.config, "query", token); !errors.Is(err, targetexec.ErrConflict) {
		t.Fatal("unexpired missing begin was treated as released", err)
	}
	token.ValidUntil = time.Now().Add(-time.Hour)
	actual, err = changeWorkspaceGuard(context.Background(), s.config, "query", token)
	if err != nil || actual.State != "released" {
		t.Fatal("expired absent token could not be reconciled", actual, err)
	}
	token.JournalID = "another-journal"
	if _, err := changeWorkspaceGuard(context.Background(), s.config, "query", token); !errors.Is(err, targetexec.ErrAuthority) {
		t.Fatal("missing token accepted a different target journal", err)
	}
}

func TestWorkspaceGuardRecoveryDoesNotStarveAnotherTarget(t *testing.T) {
	s, p, _ := admissionProjectForTest(t)
	first, err := s.beginWorkspaceOperationAtPath(p, p.Path, "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.beginWorkspaceOperationAtPath(p, p.Path, "second")
	if err != nil {
		t.Fatal(err)
	}
	defer first()
	defer second()
	operations, err := s.workspaceGuardOperations(context.Background(), "", false, 100)
	if err != nil || len(operations) != 2 {
		t.Fatal(operations, err)
	}
	if _, err := s.db.Exec(`update workspace_guard_operations set state='release_pending'; update workspace_guard_operations set target_id='unavailable-target' where id=?`, operations[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcilePendingWorkspaceGuardOperations(context.Background()); err != nil {
		t.Fatal(err)
	}
	remaining, err := s.workspaceGuardOperations(context.Background(), "", false, 100)
	if err != nil || len(remaining) != 1 || remaining[0].ID != operations[0].ID {
		t.Fatal("one failed target blocked another confirmed release", remaining, err)
	}
}

func TestWorkspaceGuardRecoveryHTTPKeepsProjectScopeAndActiveProtection(t *testing.T) {
	s, p, _ := admissionProjectForTest(t)
	first, err := s.beginWorkspaceOperationAtPath(p, p.Path, "first")
	if err != nil {
		t.Fatal(err)
	}
	defer first()
	second, err := s.beginWorkspaceOperationAtPath(p, t.TempDir(), "different-project")
	if err != nil {
		t.Fatal(err)
	}
	defer second()
	operations, err := s.projectWorkspaceGuardOperations(context.Background(), p.ID)
	if err != nil || len(operations) != 1 {
		t.Fatal("project list included another directory or lost its own operation", operations, err)
	}
	response := httptest.NewRecorder()
	s.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects/"+p.ID+"/workspace-operations/"+operations[0].ID+"/reconcile", nil))
	if response.Code != http.StatusConflict {
		t.Fatal("active operation was released through HTTP", response.Code, response.Body.String())
	}
	// Legacy rows must query their existing target token before retrying release.
	if _, err := s.db.Exec(`update workspace_guard_operations set state='release_pending',target_id='',valid_until=null where id=?`, operations[0].ID); err != nil {
		t.Fatal(err)
	}
	operations, err = s.projectWorkspaceGuardOperations(context.Background(), p.ID)
	if err != nil || len(operations) != 1 {
		t.Fatal(operations, err)
	}
	if err := s.reconcileWorkspaceGuardOperation(context.Background(), operations[0]); err != nil {
		t.Fatal("legacy confirmed release could not reconcile its existing token", err)
	}
}
