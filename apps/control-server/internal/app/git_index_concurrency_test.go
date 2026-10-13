package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGitIndexRoutesWhileAIRuns(t *testing.T) {
	server := newTestServer(t)
	repo := newTempGitRepository(t)
	writeGitTestFile(t, repo, "readme.txt", "initial\n")
	runGitForTest(t, repo, "add", "readme.txt")
	runGitForTest(t, repo, "commit", "-m", "initial")
	seedGitProjectForTest(t, server, "git-project", repo)
	release, ok := server.acquireProjectWorkspace("git-project", "conversation:active")
	if !ok {
		t.Fatal("AI lease")
	}
	defer release()
	writeGitTestFile(t, repo, "readme.txt", "AI first version\n")
	stale := gitSummaryForTest(t, server, "git-project")
	writeGitTestFile(t, repo, "readme.txt", "AI first version updated\n")
	body, _ := json.Marshal(map[string]any{"stateToken": stale.StateToken, "paths": []string{"readme.txt"}})
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects/git-project/git/stage", bytes.NewReader(body)))
	if response.Code != http.StatusConflict {
		t.Fatalf("stale staging: %d %s", response.Code, response.Body.String())
	}
	post := func(action string, want int) {
		t.Helper()
		summary := gitSummaryForTest(t, server, "git-project")
		body, err := json.Marshal(map[string]any{"stateToken": summary.StateToken, "paths": []string{"readme.txt"}, "message": "test"})
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects/git-project/git/"+action, bytes.NewReader(body)))
		if response.Code != want {
			t.Fatalf("%s: %d %s", action, response.Code, response.Body.String())
		}
		if want == http.StatusAccepted {
			var result gitOperationResult
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Status != gitOperationSucceeded {
				t.Fatalf("%s: %#v", action, result)
			}
		}
	}
	post("stage", http.StatusAccepted)
	writeGitTestFile(t, repo, "readme.txt", "AI next version\n")
	summary := gitSummaryForTest(t, server, "git-project")
	if summary.Worktree.Staged != 1 || summary.Worktree.Modified != 1 {
		t.Fatalf("staged and later edits: %#v", summary.Worktree)
	}
	post("commits", http.StatusConflict)
	post("unstage", http.StatusAccepted)
	post("stage-all", http.StatusAccepted)
	post("unstage-all", http.StatusAccepted)
	summary = gitSummaryForTest(t, server, "git-project")
	if summary.Worktree.Staged != 0 || summary.Worktree.Modified != 1 {
		t.Fatalf("unstage preserved edits: %#v", summary.Worktree)
	}
}

func TestActiveIndexMutationRejectsWorkspaceMutations(t *testing.T) {
	server := &Server{projectWorkspaceLeases: map[string]*projectWorkspaceLease{}, gitIndexActive: map[string]bool{"project": true}}
	for _, owner := range []string{"git:commit", "fs:write", "workspace-archive:test", "orchestration-merge:test"} {
		if release, ok := server.acquireWorkspace("project", owner); ok {
			release()
			t.Fatalf("allowed %s during staging", owner)
		}
	}
	for _, owner := range []string{"conversation:test", "run:test"} {
		release, ok := server.acquireWorkspace("project", owner)
		if !ok {
			t.Fatalf("blocked AI %s", owner)
		}
		release()
	}
	releaseIndex, ok := server.acquireGitIndexLock("project")
	if !ok {
		t.Fatal("acquire index lock")
	}
	if server.gitIndexLocks["project"].TryLock() {
		t.Fatal("index lock admitted a second writer")
	}
	releaseIndex()
	if !server.gitIndexLocks["project"].TryLock() {
		t.Fatal("index lock did not release")
	}
	server.gitIndexLocks["project"].Unlock()
}

func TestAIWorkspaceLeasesCanOverlapWithoutReleasingEarly(t *testing.T) {
	server := &Server{projectWorkspaceLeases: map[string]*projectWorkspaceLease{}}
	firstRelease, ok := server.acquireWorkspace("project", "conversation:first")
	if !ok {
		t.Fatal("first AI lease")
	}
	secondRelease, ok := server.acquireWorkspace("project", "conversation:second")
	if !ok {
		t.Fatal("second AI lease should share the project workspace")
	}
	firstRelease()
	if _, ok := server.acquireWorkspace("project", "git:commit"); ok {
		t.Fatal("Git acquired the workspace while a second AI run was still active")
	}
	secondRelease()
	gitRelease, ok := server.acquireWorkspace("project", "git:commit")
	if !ok {
		t.Fatal("Git did not acquire the workspace after all AI runs finished")
	}
	gitRelease()
}
