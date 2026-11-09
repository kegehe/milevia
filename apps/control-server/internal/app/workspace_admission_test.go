package app

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func admissionProjectForTest(t *testing.T) (*Server, Project, string) {
	t.Helper()
	s, id, conversation := seedTaskConversation(t)
	if _, err := s.db.Exec(`update projects set runner=?,runner_id='' where id=?`, s.localRunnerID(), id); err != nil {
		t.Fatal(err)
	}
	p, err := s.getProjectByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return s, p, conversation
}

func reserveAdmissionForTest(t *testing.T, s *Server, p Project, owner string) WorkspaceAdmission {
	t.Helper()
	var a WorkspaceAdmission
	err := s.withWorkspaceAdmissionTx(context.Background(), func(tx *sql.Tx, next map[string]WorkspaceAdmission) error {
		var err error
		a, err = s.reserveWorkspaceAdmissionTx(context.Background(), tx, next, p, owner)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func ownAdmissionForTest(t *testing.T, s *Server, a WorkspaceAdmission) {
	t.Helper()
	if err := s.transitionWorkspaceAdmission(context.Background(), a.ID, a.OwnerID, a.Epoch, "owned"); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceAdmissionPendingDrainsExistingAndBlocksNewOperations(t *testing.T) {
	s, p, _ := admissionProjectForTest(t)
	first, ok := s.acquireWorkspace(p.ID, "run:first")
	if !ok {
		t.Fatal("first AI was rejected")
	}
	defer first()
	second, ok := s.acquireWorkspace(p.ID, "run:second")
	if !ok {
		t.Fatal("ordinary AI concurrency changed")
	}
	defer second()
	a := reserveAdmissionForTest(t, s, p, "session")
	if release, ok := s.acquireWorkspace(p.ID, "run:third"); ok {
		release()
		t.Fatal("pending allowed a new AI")
	}
	if err := s.transitionWorkspaceAdmission(context.Background(), a.ID, a.OwnerID, a.Epoch, "owned"); !errors.Is(err, errWorkspaceNotQuiescent) {
		t.Fatalf("owned while two AI runs are active: %v", err)
	}
	first()
	if err := s.transitionWorkspaceAdmission(context.Background(), a.ID, a.OwnerID, a.Epoch, "owned"); !errors.Is(err, errWorkspaceNotQuiescent) {
		t.Fatalf("first release also released the second run: %v", err)
	}
	second()
	ownAdmissionForTest(t, s, a)
	if release, ok := s.acquireGitIndexLock(p.ID); ok {
		release()
		t.Fatal("Git index bypassed the owned directory")
	}
	if err := s.transitionWorkspaceAdmission(context.Background(), a.ID, "other", a.Epoch, "releasing"); !errors.Is(err, errWorkspaceAdmissionStale) {
		t.Fatalf("wrong owner accepted: %v", err)
	}
	if err := s.transitionWorkspaceAdmission(context.Background(), a.ID, a.OwnerID, a.Epoch+1, "releasing"); !errors.Is(err, errWorkspaceAdmissionStale) {
		t.Fatalf("wrong epoch accepted: %v", err)
	}
	if err := s.transitionWorkspaceAdmission(context.Background(), a.ID, a.OwnerID, a.Epoch, "releasing"); err != nil {
		t.Fatal(err)
	}
	if release, err := s.beginWorkspaceOperation(p.ID, "file"); err == nil {
		release()
		t.Fatal("releasing allowed a new write")
	}
	for range 2 {
		if err := s.releaseWorkspaceAdmission(context.Background(), a.ID, a.OwnerID, a.Epoch); err != nil {
			t.Fatal(err)
		}
	}
	release, err := s.beginWorkspaceOperation(p.ID, "file")
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestWorkspaceAdmissionScopesCoverAliasesParentsAndChildren(t *testing.T) {
	s, p, _ := admissionProjectForTest(t)
	a := reserveAdmissionForTest(t, s, p, "session")
	ownAdmissionForTest(t, s, a)
	for _, path := range []string{p.Path, filepath.Dir(p.Path), filepath.Join(p.Path, "new", "child")} {
		if release, err := s.beginWorkspaceOperationAtPath(Project{ID: "alias", Runner: p.Runner}, path, "file"); !errors.Is(err, errWorkspaceSupervised) {
			if release != nil {
				release()
			}
			t.Fatalf("overlapping path admitted: %s, %v", path, err)
		}
	}
	sibling := filepath.Join(filepath.Dir(p.Path), filepath.Base(p.Path)+"-sibling")
	release, err := s.beginWorkspaceOperationAtPath(p, sibling, "file")
	if err != nil {
		t.Fatalf("sibling blocked: %v", err)
	}
	release()
	remote := Project{Runner: "ssh-example"}
	key := "path:" + filepath.Clean(p.Path)
	s.rememberWorkspaceScope(key, p.Path, remote.Runner)
	release, err = s.beginWorkspaceOperation(key, "remote")
	if err == nil {
		release()
		t.Fatal("unconnected remote writer bypassed target protection")
	}
	if errors.Is(err, errWorkspaceSupervised) {
		t.Fatalf("remote scope was reinterpreted as local: %v", err)
	}
	if runtime.GOOS == "windows" {
		if release, err := s.beginWorkspaceOperationAtPath(p, strings.ToUpper(p.Path), "file"); err == nil {
			release()
			t.Fatal("case variant bypassed Windows admission")
		}
	}
}

func TestWorkspaceAdmissionResolvesSymlinkAliases(t *testing.T) {
	s, p, _ := admissionProjectForTest(t)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(p.Path, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	reserveAdmissionForTest(t, s, p, "session")
	if release, err := s.beginWorkspaceOperationAtPath(p, filepath.Join(alias, "new"), "file"); !errors.Is(err, errWorkspaceSupervised) {
		if release != nil {
			release()
		}
		t.Fatalf("symlink alias admitted: %v", err)
	}
}

func TestWorkspaceAdmissionRollbackDoesNotPublishOwnership(t *testing.T) {
	s, p, _ := admissionProjectForTest(t)
	cause := errors.New("force start transaction rollback")
	err := s.withWorkspaceAdmissionTx(context.Background(), func(tx *sql.Tx, next map[string]WorkspaceAdmission) error {
		if _, err := s.reserveWorkspaceAdmissionTx(context.Background(), tx, next, p, "session"); err != nil {
			return err
		}
		return cause
	})
	if !errors.Is(err, cause) {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`select count(*) from workspace_admissions`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rolled-back persisted reservation count=%d err=%v", count, err)
	}
	release, err := s.beginWorkspaceOperation(p.ID, "file")
	if err != nil {
		t.Fatalf("rolled-back in-memory reservation: %v", err)
	}
	release()
}

func TestWorkspaceAdmissionConcurrentClaimHasSingleWinner(t *testing.T) {
	s, p, _ := admissionProjectForTest(t)
	results := make(chan error, 12)
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results <- s.withWorkspaceAdmissionTx(context.Background(), func(tx *sql.Tx, next map[string]WorkspaceAdmission) error {
				_, err := s.reserveWorkspaceAdmissionTx(context.Background(), tx, next, p, string(rune('a'+i)))
				return err
			})
		}(i)
	}
	wg.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		} else if !errors.Is(err, errWorkspaceSupervised) {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatalf("winners=%d", winners)
	}
}

func TestWorkspaceAdmissionDirectoryReplacementCannotBeReleased(t *testing.T) {
	s, p, conversation := admissionProjectForTest(t)
	p.Path = filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(p.Path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`update projects set path=? where id=?`, p.Path, p.ID); err != nil {
		t.Fatal(err)
	}
	s.rememberWorkspaceScope(p.ID, p.Path, p.Runner)
	a := reserveAdmissionForTest(t, s, p, "session")
	ownAdmissionForTest(t, s, a)
	oldPath := p.Path + "-old"
	if err := os.Rename(p.Path, oldPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(p.Path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.transitionWorkspaceAdmission(context.Background(), a.ID, a.OwnerID, a.Epoch, "releasing"); err == nil {
		t.Fatal("replaced directory was released")
	}
	if _, err := s.db.Exec(`insert into runs(id,conversation_id,status,workspace_path,created_at) values ('changed-directory-run',?,'completed',?,?)`, conversation, p.Path, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	err = linkSupervisionRunTx(context.Background(), tx, supervisionRunOwnership{SessionID: a.OwnerID, WorkItemID: "item", Role: "scheduler", Attempt: 1, AdmissionID: a.ID, Epoch: a.Epoch, RunID: "changed-directory-run", ConversationID: conversation})
	_ = tx.Rollback()
	if err == nil {
		t.Fatal("same pathname with a different directory identity was accepted for launch")
	}
	if err := s.recoverWorkspaceAdmissions(context.Background()); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := s.db.QueryRow(`select recovery_state from workspace_admissions where id=?`, a.ID).Scan(&state); err != nil || state != "path_changed" {
		t.Fatalf("replacement recovery=%s err=%v", state, err)
	}
}

func linkAdmissionRunForTest(t *testing.T, s *Server, a WorkspaceAdmission, conversationID string) supervisionRunOwnership {
	t.Helper()
	runID := "supervised-run"
	if _, err := s.db.Exec(`insert into runs(id,conversation_id,status,workspace_path,created_at) values (?,?,'running',?,?)`, runID, conversationID, a.CanonicalPath, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	link := supervisionRunOwnership{SessionID: a.OwnerID, WorkItemID: "work-item", Role: "scheduler", Attempt: 1, AdmissionID: a.ID, Epoch: a.Epoch, RunID: runID, ConversationID: conversationID}
	for range 2 {
		tx, err := s.db.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		err = linkSupervisionRunTx(context.Background(), tx, link)
		if err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	return link
}

func TestWorkspaceAdmissionRecoveryPreservesSupervisionRunAndBlocksRelease(t *testing.T) {
	s, p, conversation := admissionProjectForTest(t)
	a := reserveAdmissionForTest(t, s, p, "session")
	ownAdmissionForTest(t, s, a)
	linkAdmissionRunForTest(t, s, a, conversation)
	if _, err := s.db.Exec(`update conversations set status='running' where id=?`, conversation); err != nil {
		t.Fatal(err)
	}
	if err := s.recoverWorkspaceAdmissions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.recoverInterruptedRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	var process, outcome, status, convStatus, recovery string
	var epoch int64
	err := s.db.QueryRow(`select l.process_state,l.outcome_state,r.status,c.status,a.recovery_state,a.epoch from supervision_run_links l join runs r on r.id=l.run_id join conversations c on c.id=l.conversation_id join workspace_admissions a on a.id=l.admission_id`).Scan(&process, &outcome, &status, &convStatus, &recovery, &epoch)
	if err != nil || process != "unknown" || outcome != "unknown" || status != "running" || convStatus != "running" || recovery != "reconciling" || epoch != 2 {
		t.Fatalf("recovery=%s/%s/%s/%s/%s/%d err=%v", process, outcome, status, convStatus, recovery, epoch, err)
	}
	if err := s.transitionWorkspaceAdmission(context.Background(), a.ID, a.OwnerID, epoch, "releasing"); !errors.Is(err, errWorkspaceNotQuiescent) {
		t.Fatalf("unknown released: %v", err)
	}
	if err := s.transitionWorkspaceAdmission(context.Background(), a.ID, a.OwnerID, a.Epoch, "releasing"); !errors.Is(err, errWorkspaceAdmissionStale) {
		t.Fatalf("old epoch accepted: %v", err)
	}
	if release, err := s.beginWorkspaceOperation(p.ID, "file"); err == nil {
		release()
		t.Fatal("recovery bypassed")
	}
}

func TestWorkspaceAdmissionRecoveryCleansReleasingResidue(t *testing.T) {
	s, p, _ := admissionProjectForTest(t)
	taskID := createTaskForTest(t, s.routes(), p.ID, "release residue")
	claimSupervisionTasksForTest(t, s, p.ID, "session", taskID)
	a := reserveAdmissionForTest(t, s, p, "session")
	if err := s.transitionWorkspaceAdmission(context.Background(), a.ID, a.OwnerID, a.Epoch, "releasing"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.recoverWorkspaceAdmissions(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	assertExecutionClaimForTest(t, s, taskID, "", "")
	release, err := s.beginWorkspaceOperation(p.ID, "file")
	if err != nil {
		t.Fatalf("releasing residue remained locked: %v", err)
	}
	release()
}

func TestWorkspaceAdmissionRecoveryDoesNotCleanUnconfirmedReleasingRun(t *testing.T) {
	s, p, conversation := admissionProjectForTest(t)
	a := reserveAdmissionForTest(t, s, p, "session")
	ownAdmissionForTest(t, s, a)
	linkAdmissionRunForTest(t, s, a, conversation)
	if _, err := s.db.Exec(`update workspace_admissions set state='releasing' where id=?;update runs set status='completed' where id='supervised-run'`, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.recoverWorkspaceAdmissions(context.Background()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`select count(*) from workspace_admissions where id=?`, a.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("unconfirmed releasing owner was erased: %d %v", count, err)
	}
	if release, err := s.beginWorkspaceOperation(p.ID, "file"); err == nil {
		release()
		t.Fatal("unknown process lost protection")
	}
}

func TestWorkspaceAdmissionSelectionRollbackIncludesTaskClaims(t *testing.T) {
	s, p, _ := admissionProjectForTest(t)
	taskID := createTaskForTest(t, s.routes(), p.ID, "atomic start")
	cause := errors.New("start failed after both reservations")
	err := s.withWorkspaceAdmissionTx(context.Background(), func(tx *sql.Tx, next map[string]WorkspaceAdmission) error {
		if err := claimSupervisionTasksTx(context.Background(), tx, p.ID, "session", []string{taskID}, time.Now().UTC()); err != nil {
			return err
		}
		if _, err := s.reserveWorkspaceAdmissionTx(context.Background(), tx, next, p, "session"); err != nil {
			return err
		}
		return cause
	})
	if !errors.Is(err, cause) {
		t.Fatal(err)
	}
	assertExecutionClaimForTest(t, s, taskID, "", "")
	release, err := s.beginWorkspaceOperation(p.ID, "file")
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestWorkspaceAdmissionRoleAndDirectSideEffectEntrypointsAreProtected(t *testing.T) {
	s, p, conversation := admissionProjectForTest(t)
	a := reserveAdmissionForTest(t, s, p, "session")
	ownAdmissionForTest(t, s, a)
	linkAdmissionRunForTest(t, s, a, conversation)
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodDelete, "/api/conversations/" + conversation, ""},
		{http.MethodPost, "/api/conversations/" + conversation + "/clear", "{}"},
		{http.MethodPost, "/api/conversations/" + conversation + "/stop", "{}"},
		{http.MethodPost, "/api/runs/supervised-run/stop", "{}"},
		{http.MethodPost, "/api/conversations/" + conversation + "/messages", `{"content":"change code"}`},
		{http.MethodPost, "/api/conversations/" + conversation + "/model", `{"model":""}`},
		{http.MethodPost, "/api/conversations/" + conversation + "/permission-mode", `{"permissionMode":"acceptEdits"}`},
		{http.MethodPost, "/api/conversations/" + conversation + "/reasoning-effort", `{"reasoningEffort":"high"}`},
		{http.MethodPost, "/api/conversations/" + conversation + "/workspaces", "{}"},
		{http.MethodPost, "/api/conversations/" + conversation + "/handoff", "{}"},
		{http.MethodPost, "/api/conversations/" + conversation + "/rewind", `{"checkpointId":"checkpoint","mode":"conversation"}`},
		{http.MethodPut, "/api/projects/" + p.ID + "/fs/write", `{"path":"protected.txt","content":"changed"}`},
		{http.MethodPost, "/api/projects/" + p.ID + "/git/init", "{}"},
	} {
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
		if w.Code != http.StatusConflict {
			t.Errorf("%s %s status=%d body=%s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
	if _, err := s.gitOutput(context.Background(), p.Path, "init"); !errors.Is(err, errWorkspaceSupervised) {
		t.Fatalf("internal Git bypassed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(p.Path, ".git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Git side effect occurred: %v", err)
	}
	if _, _, _, err := s.createWorkspaceSnapshot(context.Background(), p.ID, p.Path, p.Runner, conversation); !errors.Is(err, errWorkspaceSupervised) {
		t.Fatalf("checkpoint bypassed: %v", err)
	}
	snapshot := t.TempDir()
	if err := os.WriteFile(filepath.Join(snapshot, "protected.txt"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.Path, "protected.txt"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.restoreCheckpointCode(context.Background(), checkpointRestoreRequest{ProjectID: p.ID, ProjectPath: p.Path, Runner: p.Runner, SnapshotDir: snapshot}); !errors.Is(err, errWorkspaceSupervised) {
		t.Fatalf("restore bypassed: %v", err)
	}
	if content, err := os.ReadFile(filepath.Join(p.Path, "protected.txt")); err != nil || string(content) != "new" {
		t.Fatalf("restore changed protected code: %q %v", content, err)
	}
	manager := newTerminalManager(s)
	defer manager.closeAll()
	manager.factory = staticTerminalFactory{session: &testTerminalSession{id: "forbidden", ready: make(chan error, 1)}}
	if _, err := manager.create(context.Background(), p, 80, 24); !errors.Is(err, errWorkspaceSupervised) {
		t.Fatalf("terminal bypassed: %v", err)
	}
}

func TestWorkspaceAdmissionRoleIsProtectedBeforeFirstRun(t *testing.T) {
	s, p, conversation := admissionProjectForTest(t)
	reserveAdmissionForTest(t, s, p, "session")
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := linkSupervisionConversationTx(context.Background(), tx, conversation, "session", "scheduler"); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := rejectActiveSupervisionConversation(context.Background(), s.db, conversation); err == nil {
		t.Fatal("role without a Run is unprotected")
	}
	if _, _, _, code, err := s.startMessage(context.Background(), conversation, "change code", "", nil); code != http.StatusConflict || err == nil {
		t.Fatalf("empty role admitted an ordinary Run: %d %v", code, err)
	}
	if _, err := s.db.Exec(`update conversations set is_current=0 where id=?`, conversation); err != nil {
		t.Fatal(err)
	}
	for i := range maxConversationHistoryPerProject + 1 {
		if _, err := s.db.Exec(`insert into conversations(id,project_id,claude_session_id,status,is_current,created_at) values (?,?,?,'idle',0,?)`, "history-"+string(rune('a'+i)), p.ID, "session-"+string(rune('a'+i)), time.Now().UTC().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	tx, err = s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pruneConversationHistoryTx(context.Background(), tx, p.ID); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`select count(*) from conversations where id=?`, conversation).Scan(&count); err != nil || count != 1 {
		t.Fatalf("history pruning deleted the role: %d %v", count, err)
	}
}

func TestWorkspaceAdmissionRunTerminalStatusDoesNotProveProcessExit(t *testing.T) {
	s, p, conversation := admissionProjectForTest(t)
	a := reserveAdmissionForTest(t, s, p, "session")
	ownAdmissionForTest(t, s, a)
	link := linkAdmissionRunForTest(t, s, a, conversation)
	if _, err := s.db.Exec(`update runs set status='completed' where id=?`, link.RunID); err != nil {
		t.Fatal(err)
	}
	if err := s.transitionWorkspaceAdmission(context.Background(), a.ID, a.OwnerID, a.Epoch, "releasing"); !errors.Is(err, errWorkspaceNotQuiescent) {
		t.Fatalf("transport terminal status released unconfirmed process: %v", err)
	}
	if _, err := s.db.Exec(`update supervision_run_links set process_state='quiescent',outcome_state='known' where run_id=?`, link.RunID); err != nil {
		t.Fatal(err)
	}
	if err := s.transitionWorkspaceAdmission(context.Background(), a.ID, a.OwnerID, a.Epoch, "releasing"); err != nil {
		t.Fatal(err)
	}
	if err := s.releaseWorkspaceAdmission(context.Background(), a.ID, a.OwnerID, a.Epoch); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceAdmissionTerminalWaitsForReaderAndProcessEvenWhenClosed(t *testing.T) {
	s, p, _ := admissionProjectForTest(t)
	release, err := s.beginWorkspaceOperationAtPath(p, p.Path, "terminal")
	if err != nil {
		t.Fatal(err)
	}
	r := &terminalRecord{session: &testTerminalSession{id: "terminal"}, state: "closed", closed: true, releaseWorkspace: release}
	m := newTerminalManager(s)
	defer m.closeAll()
	a := reserveAdmissionForTest(t, s, p, "session")
	m.markReaderDone(r)
	if err := s.transitionWorkspaceAdmission(context.Background(), a.ID, a.OwnerID, a.Epoch, "owned"); !errors.Is(err, errWorkspaceNotQuiescent) {
		t.Fatalf("reader completion released live process: %v", err)
	}
	zero := 0
	m.markWaiterDone(r, &zero)
	ownAdmissionForTest(t, s, a)
	m.markReaderDone(r) // repeated exit delivery must remain harmless
}

func TestWorkspaceAdmissionProjectRunnerHoldsUntilActualExit(t *testing.T) {
	s, p, _ := admissionProjectForTest(t)
	runner := newProjectRunner(p.ID, "", longRunningProjectCommandForTest("echo supervision-admission-test"), nil, func(LogEntry) {})
	runner.workspaceAdmission = func(path string) (func(), error) { return s.beginWorkspaceOperationAtPath(p, path, "project-run") }
	t.Cleanup(func() { _ = runner.Stop() })
	if err := runner.Start(context.Background(), p.Path); err != nil {
		t.Fatal(err)
	}
	a := reserveAdmissionForTest(t, s, p, "session")
	if err := s.transitionWorkspaceAdmission(context.Background(), a.ID, a.OwnerID, a.Epoch, "owned"); !errors.Is(err, errWorkspaceNotQuiescent) {
		t.Fatalf("live service admitted: %v", err)
	}
	if err := runner.Stop(); err != nil {
		t.Fatal(err)
	}
	ownAdmissionForTest(t, s, a)
	if err := runner.Start(context.Background(), p.Path); !errors.Is(err, errWorkspaceSupervised) {
		t.Fatalf("restart bypassed: %v", err)
	}
}

func TestWorkspaceAdmissionProjectRunnerFailedLaunchReleasesOperation(t *testing.T) {
	s, p, _ := admissionProjectForTest(t)
	runner := newProjectRunner(p.ID, "", "echo unused", nil, func(LogEntry) {})
	runner.workspaceAdmission = func(path string) (func(), error) { return s.beginWorkspaceOperationAtPath(p, path, "project-run") }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runner.Start(ctx, p.Path); err == nil {
		t.Fatal("cancelled launch succeeded")
	}
	a := reserveAdmissionForTest(t, s, p, "session")
	ownAdmissionForTest(t, s, a)
}

type delayedAdmissionSession struct{ *conflictTestSession }

func (*delayedAdmissionSession) Stop() {} // 请求停止尚不代表进程退出。

func TestWorkspaceAdmissionTemporaryAgentHoldsUntilProcessExit(t *testing.T) {
	s, p, _ := admissionProjectForTest(t)
	session := &conflictTestSession{done: make(chan error, 1)}
	session.send = func(sink AgentRunSink) error {
		sink.AssistantText(`{"merged":"done"}`, "")
		sink.(AgentTurnSink).TurnFinished(nil)
		return nil
	}
	runner := &admissionStreamingRunner{session: &delayedAdmissionSession{session}}
	s.codebuddyRunner = runner
	var once sync.Once
	finish := func() { once.Do(func() { close(session.done) }) }
	defer finish()
	if _, err := s.runConflictSuggestionAgent(context.Background(), p, "codebuddy", "review"); err != nil {
		t.Fatal(err)
	}
	a := reserveAdmissionForTest(t, s, p, "session")
	if err := s.transitionWorkspaceAdmission(context.Background(), a.ID, a.OwnerID, a.Epoch, "owned"); !errors.Is(err, errWorkspaceNotQuiescent) {
		t.Fatalf("turn completion released a live CLI: %v", err)
	}
	finish()
	deadline := time.Now().Add(time.Second)
	for {
		err := s.transitionWorkspaceAdmission(context.Background(), a.ID, a.OwnerID, a.Epoch, "owned")
		if err == nil {
			break
		}
		if !errors.Is(err, errWorkspaceNotQuiescent) || time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
}

type admissionStreamingRunner struct {
	runnerFunc
	session AgentSession
}

func (r *admissionStreamingRunner) StartSession(context.Context, AgentSessionRequest) (AgentSession, error) {
	return r.session, nil
}

func TestWorkspaceAdmissionRunOwnershipRejectsChangedRoleOwnerAndWorkspace(t *testing.T) {
	s, p, conversation := admissionProjectForTest(t)
	a := reserveAdmissionForTest(t, s, p, "session")
	ownAdmissionForTest(t, s, a)
	link := linkAdmissionRunForTest(t, s, a, conversation)
	for _, mutate := range []func(*supervisionRunOwnership){
		func(l *supervisionRunOwnership) { l.Epoch++ },
		func(l *supervisionRunOwnership) { l.SessionID = "other" },
		func(l *supervisionRunOwnership) { l.Role = "implementation" },
		func(l *supervisionRunOwnership) { l.WorkItemID = "another-item" },
		func(l *supervisionRunOwnership) { l.ConversationID = "foreign" },
	} {
		changed := link
		mutate(&changed)
		tx, err := s.db.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		err = linkSupervisionRunTx(context.Background(), tx, changed)
		_ = tx.Rollback()
		if err == nil {
			t.Fatalf("changed ownership was accepted: %+v", changed)
		}
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`update runs set workspace_path=? where id=?`, t.TempDir(), link.RunID); err != nil {
		t.Fatal(err)
	}
	if err := linkSupervisionRunTx(context.Background(), tx, link); err == nil {
		t.Fatal("idempotent replay accepted a different workspace")
	}
}
