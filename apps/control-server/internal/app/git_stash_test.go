package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func postGitStashForTest(t *testing.T, server *Server, projectID, action string, payload map[string]any, want int) gitOperationResult {
	t.Helper()
	if _, supplied := payload["stateToken"]; !supplied {
		payload["stateToken"] = gitSummaryForTest(t, server, projectID).StateToken
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/git/"+action, bytes.NewReader(body)))
	if response.Code != want {
		t.Fatalf("%s: status=%d body=%s", action, response.Code, response.Body.String())
	}
	var result gitOperationResult
	if want == http.StatusAccepted {
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
	}
	return result
}

func getGitStashesForTest(t *testing.T, server *Server, projectID string) []GitStash {
	t.Helper()
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/git/stashes", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("list stashes: %d %s", response.Code, response.Body.String())
	}
	var stashes []GitStash
	if err := json.Unmarshal(response.Body.Bytes(), &stashes); err != nil {
		t.Fatal(err)
	}
	return stashes
}

func TestGitStashRoundTrip(t *testing.T) {
	server := newTestServer(t)
	repo := newTempGitRepository(t)
	writeGitTestFile(t, repo, "readme.txt", "initial\n")
	writeGitTestFile(t, repo, ".gitignore", "ignored.txt\n")
	runGitForTest(t, repo, "add", ".")
	runGitForTest(t, repo, "commit", "-m", "initial")
	runGitForTest(t, repo, "branch", "other")
	seedGitProjectForTest(t, server, "git-project", repo)
	if stashes := getGitStashesForTest(t, server, "git-project"); len(stashes) != 0 {
		t.Fatalf("unexpected initial stashes: %#v", stashes)
	}
	writeGitTestFile(t, repo, "readme.txt", "staged\n")
	runGitForTest(t, repo, "add", "readme.txt")
	writeGitTestFile(t, repo, "readme.txt", "unstaged\n")
	writeGitTestFile(t, repo, "new file.txt", "untracked\n")
	writeGitTestFile(t, repo, "ignored.txt", "ignored\n")
	result := postGitStashForTest(t, server, "git-project", "stashes", map[string]any{"message": "切换前的改动"}, http.StatusAccepted)
	if result.Status != gitOperationSucceeded {
		t.Fatalf("create: %#v", result)
	}
	if status := gitOutputForTest(t, repo, "status", "--porcelain"); status != "" {
		t.Fatalf("stash did not clean workspace: %q", status)
	}
	if _, err := os.Stat(filepath.Join(repo, "ignored.txt")); err != nil {
		t.Fatalf("ignored file was removed: %v", err)
	}
	stashes := getGitStashesForTest(t, server, "git-project")
	if len(stashes) != 1 || !strings.Contains(stashes[0].Subject, "切换前的改动") || stashes[0].CreatedAt.IsZero() {
		t.Fatalf("stash metadata: %#v", stashes)
	}
	// 收起后能通过原有切分支接口，再创建一条记录使原来的 stash@{0} 移位。
	result = postGitStashForTest(t, server, "git-project", "switch", map[string]any{"branch": "other"}, http.StatusAccepted)
	if result.Status != gitOperationSucceeded {
		t.Fatalf("switch: %#v", result)
	}
	writeGitTestFile(t, repo, "second.txt", "second stash\n")
	result = postGitStashForTest(t, server, "git-project", "stashes", map[string]any{}, http.StatusAccepted)
	if result.Status != gitOperationSucceeded {
		t.Fatalf("second stash: %#v", result)
	}
	result = postGitStashForTest(t, server, "git-project", "stashes/apply", map[string]any{"oid": stashes[0].OID}, http.StatusAccepted)
	if result.Status != gitOperationSucceeded {
		t.Fatalf("restore: %#v", result)
	}
	if index := gitOutputForTest(t, repo, "show", ":readme.txt"); index != "staged\n" {
		t.Fatalf("index not restored: %q", index)
	}
	for name, want := range map[string]string{"readme.txt": "unstaged\n", "new file.txt": "untracked\n"} {
		got, err := os.ReadFile(filepath.Join(repo, name))
		if err != nil || string(got) != want {
			t.Fatalf("restored %s = %q, %v", name, got, err)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, "second.txt")); !os.IsNotExist(err) {
		t.Fatalf("restored the wrong stash: %v", err)
	}
	if retained := getGitStashesForTest(t, server, "git-project"); len(retained) != 2 {
		t.Fatalf("restore deleted backups: %#v", retained)
	}
	var creates, applies int
	if err := server.db.QueryRow(`select count(*) from git_operations where type='stash_create' and status='succeeded'`).Scan(&creates); err != nil {
		t.Fatal(err)
	}
	if err := server.db.QueryRow(`select count(*) from git_operations where type='stash_apply' and status='succeeded'`).Scan(&applies); err != nil || creates != 2 || applies != 1 {
		t.Fatalf("audit: creates=%d applies=%d err=%v", creates, applies, err)
	}
	for typ, count := range map[string]int{"stash_create": 2, "stash_apply": 1} {
		response := httptest.NewRecorder()
		server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/projects/git-project/git/operations?type="+typ, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("filter %s: %d %s", typ, response.Code, response.Body.String())
		}
		var operations []GitOperation
		if err := json.Unmarshal(response.Body.Bytes(), &operations); err != nil || len(operations) != count {
			t.Fatalf("filter %s returned %d operations, want %d: %v", typ, len(operations), count, err)
		}
		for _, operation := range operations {
			if operation.Type != typ {
				t.Fatalf("filter %s included %s", typ, operation.Type)
			}
		}
	}
}

func TestGitStashRejectsUnsafeRequests(t *testing.T) {
	server := newTestServer(t)
	repo := newTempGitRepository(t)
	seedGitProjectForTest(t, server, "git-project", repo)
	writeGitTestFile(t, repo, "readme.txt", "initial\n")
	postGitStashForTest(t, server, "git-project", "stashes", map[string]any{}, http.StatusConflict) // unborn HEAD
	runGitForTest(t, repo, "add", ".")
	runGitForTest(t, repo, "commit", "-m", "initial")
	postGitStashForTest(t, server, "git-project", "stashes", map[string]any{}, http.StatusConflict) // clean
	writeGitTestFile(t, repo, "readme.txt", "first\n")
	stale := gitSummaryForTest(t, server, "git-project").StateToken
	writeGitTestFile(t, repo, "readme.txt", "second version\n")
	postGitStashForTest(t, server, "git-project", "stashes", map[string]any{"stateToken": stale}, http.StatusConflict)
	release, ok := server.acquireProjectWorkspace("git-project", "conversation:active")
	if !ok {
		t.Fatal("acquire AI lease")
	}
	postGitStashForTest(t, server, "git-project", "stashes", map[string]any{}, http.StatusConflict)
	release()
	result := postGitStashForTest(t, server, "git-project", "stashes", map[string]any{}, http.StatusAccepted)
	if result.Status != gitOperationSucceeded {
		t.Fatalf("create: %#v", result)
	}
	stash := getGitStashesForTest(t, server, "git-project")[0]
	postGitStashForTest(t, server, "git-project", "stashes/apply", map[string]any{"oid": "--all"}, http.StatusBadRequest)
	postGitStashForTest(t, server, "git-project", "stashes/apply", map[string]any{"oid": strings.TrimSpace(gitOutputForTest(t, repo, "rev-parse", "HEAD"))}, http.StatusConflict)
	writeGitTestFile(t, repo, "new.txt", "must survive\n")
	postGitStashForTest(t, server, "git-project", "stashes/apply", map[string]any{"oid": stash.OID}, http.StatusConflict)
	if data, err := os.ReadFile(filepath.Join(repo, "new.txt")); err != nil || string(data) != "must survive\n" {
		t.Fatalf("dirty restore changed files: %q %v", data, err)
	}
}

func TestGitStashConflictRetainsBackup(t *testing.T) {
	server := newTestServer(t)
	repo := newTempGitRepository(t)
	writeGitTestFile(t, repo, "readme.txt", "initial\n")
	runGitForTest(t, repo, "add", ".")
	runGitForTest(t, repo, "commit", "-m", "initial")
	seedGitProjectForTest(t, server, "git-project", repo)
	writeGitTestFile(t, repo, "readme.txt", "stashed\n")
	result := postGitStashForTest(t, server, "git-project", "stashes", map[string]any{}, http.StatusAccepted)
	if result.Status != gitOperationSucceeded {
		t.Fatalf("create: %#v", result)
	}
	stash := getGitStashesForTest(t, server, "git-project")[0]
	writeGitTestFile(t, repo, "readme.txt", "diverged\n")
	runGitForTest(t, repo, "add", ".")
	runGitForTest(t, repo, "commit", "-m", "diverged")
	result = postGitStashForTest(t, server, "git-project", "stashes/apply", map[string]any{"oid": stash.OID}, http.StatusAccepted)
	if result.Status != gitOperationNeedsAttention || !strings.Contains(result.ErrorMessage, "备份仍然保留") {
		t.Fatalf("restore conflict: %#v", result)
	}
	if stashes := getGitStashesForTest(t, server, "git-project"); len(stashes) != 1 || stashes[0].OID != stash.OID {
		t.Fatalf("lost stash after conflict: %#v", stashes)
	}
	postGitStashForTest(t, server, "git-project", "stashes", map[string]any{}, http.StatusConflict)
}

func TestGitStashSurvivesWorktreeRemoval(t *testing.T) {
	server := newTestServer(t)
	repo := newTempGitRepository(t)
	writeGitTestFile(t, repo, "readme.txt", "initial\n")
	runGitForTest(t, repo, "add", ".")
	runGitForTest(t, repo, "commit", "-m", "initial")
	worktree := filepath.Join(t.TempDir(), "worktree")
	runGitForTest(t, repo, "worktree", "add", "-b", "feature", worktree)
	seedGitProjectForTest(t, server, "source", worktree)
	seedGitProjectForTest(t, server, "target", repo)
	writeGitTestFile(t, worktree, "new.txt", "saved from removed worktree\n")
	result := postGitStashForTest(t, server, "source", "stashes", map[string]any{}, http.StatusAccepted)
	if result.Status != gitOperationSucceeded {
		t.Fatalf("stash linked worktree: %#v", result)
	}
	runGitForTest(t, repo, "worktree", "remove", worktree)
	stashes := getGitStashesForTest(t, server, "target")
	if len(stashes) != 1 {
		t.Fatalf("shared stash missing: %#v", stashes)
	}
	result = postGitStashForTest(t, server, "target", "stashes/apply", map[string]any{"oid": stashes[0].OID}, http.StatusAccepted)
	if result.Status != gitOperationSucceeded {
		t.Fatalf("restore from removed worktree: %#v", result)
	}
	if data, err := os.ReadFile(filepath.Join(repo, "new.txt")); err != nil || string(data) != "saved from removed worktree\n" {
		t.Fatalf("restore from removed worktree: %q %v", data, err)
	}
}

func TestGitStashDoesNotReportUnstashedSubmoduleChangesAsSuccess(t *testing.T) {
	server := newTestServer(t)
	repo := newTempGitRepository(t)
	submodule := newTempGitRepository(t)
	writeGitTestFile(t, submodule, "readme.txt", "initial\n")
	runGitForTest(t, submodule, "add", ".")
	runGitForTest(t, submodule, "commit", "-m", "initial")
	runGitForTest(t, repo, "-c", "protocol.file.allow=always", "submodule", "add", submodule, "nested")
	runGitForTest(t, repo, "commit", "-m", "add submodule")
	seedGitProjectForTest(t, server, "git-project", repo)
	writeGitTestFile(t, repo, "nested/readme.txt", "must remain visible\n")
	result := postGitStashForTest(t, server, "git-project", "stashes", map[string]any{}, http.StatusAccepted)
	if result.Status != gitOperationNeedsAttention || !strings.Contains(result.ErrorMessage, "收起未完成") {
		t.Fatalf("unstashed submodule reported as success: %#v", result)
	}
	if data, err := os.ReadFile(filepath.Join(repo, "nested", "readme.txt")); err != nil || string(data) != "must remain visible\n" {
		t.Fatalf("submodule content changed: %q %v", data, err)
	}
	// 隐藏子模块改动的配置也不能让“收起后已干净”的检查误报成功。
	runGitForTest(t, repo, "config", "submodule.nested.ignore", "all")
	writeGitTestFile(t, repo, "outside.txt", "can be stashed\n")
	result = postGitStashForTest(t, server, "git-project", "stashes", map[string]any{}, http.StatusAccepted)
	if result.Status != gitOperationNeedsAttention {
		t.Fatalf("hidden submodule changes reported as success: %#v", result)
	}
}

func TestGitStashRejectsResolvedButUnfinishedMerge(t *testing.T) {
	server := newTestServer(t)
	repo := newTempGitRepository(t)
	writeGitTestFile(t, repo, "readme.txt", "initial\n")
	runGitForTest(t, repo, "add", ".")
	runGitForTest(t, repo, "commit", "-m", "initial")
	runGitForTest(t, repo, "checkout", "-b", "feature")
	writeGitTestFile(t, repo, "feature.txt", "feature\n")
	runGitForTest(t, repo, "add", ".")
	runGitForTest(t, repo, "commit", "-m", "feature")
	runGitForTest(t, repo, "checkout", "main")
	runGitForTest(t, repo, "merge", "--no-commit", "--no-ff", "feature")
	seedGitProjectForTest(t, server, "git-project", repo)
	postGitStashForTest(t, server, "git-project", "stashes", map[string]any{}, http.StatusConflict)
	if stashes := getGitStashesForTest(t, server, "git-project"); len(stashes) != 0 {
		t.Fatalf("created stash during merge: %#v", stashes)
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", "MERGE_HEAD")); err != nil {
		t.Fatalf("merge state was lost: %v", err)
	}
}

func TestGitStashCommandFailurePreservesFilesAndExistingLock(t *testing.T) {
	server := newTestServer(t)
	repo := newTempGitRepository(t)
	writeGitTestFile(t, repo, "readme.txt", "initial\n")
	runGitForTest(t, repo, "add", ".")
	runGitForTest(t, repo, "commit", "-m", "initial")
	seedGitProjectForTest(t, server, "git-project", repo)
	writeGitTestFile(t, repo, "readme.txt", "must survive\n")
	token := gitSummaryForTest(t, server, "git-project").StateToken
	writeGitTestFile(t, repo, ".git/index.lock", "external lock\n")
	result := postGitStashForTest(t, server, "git-project", "stashes", map[string]any{"stateToken": token}, http.StatusAccepted)
	if result.Status != gitOperationNeedsAttention || !strings.Contains(result.ErrorMessage, "收起未完成") {
		t.Fatalf("command failure: %#v", result)
	}
	for path, want := range map[string]string{"readme.txt": "must survive\n", ".git/index.lock": "external lock\n"} {
		if data, err := os.ReadFile(filepath.Join(repo, path)); err != nil || string(data) != want {
			t.Fatalf("command failure changed %s: %q %v", path, data, err)
		}
	}
}

func TestGitStashRejectsPausedRebaseWithoutRebaseHead(t *testing.T) {
	for _, linked := range []bool{false, true} {
		name := "main worktree"
		if linked {
			name = "linked worktree"
		}
		t.Run(name, func(t *testing.T) {
			server := newTestServer(t)
			repo := newTempGitRepository(t)
			writeGitTestFile(t, repo, "readme.txt", "initial\n")
			runGitForTest(t, repo, "add", ".")
			runGitForTest(t, repo, "commit", "-m", "initial")
			writeGitTestFile(t, repo, "second.txt", "second\n")
			runGitForTest(t, repo, "add", ".")
			runGitForTest(t, repo, "commit", "-m", "second")
			if linked {
				worktree := filepath.Join(t.TempDir(), "linked worktree")
				runGitForTest(t, repo, "worktree", "add", "-b", "paused-rebase", worktree)
				repo = worktree
			}
			runGitForTest(t, repo, "-c", "sequence.editor=sed -i '1i break'", "rebase", "-i", "HEAD~1")
			gitDir := strings.TrimSpace(gitOutputForTest(t, repo, "rev-parse", "--absolute-git-dir"))
			if _, err := os.Stat(filepath.Join(gitDir, "rebase-merge")); err != nil {
				t.Fatalf("rebase was not paused: %v", err)
			}
			if _, err := os.Stat(filepath.Join(gitDir, "REBASE_HEAD")); !os.IsNotExist(err) {
				t.Fatalf("fixture unexpectedly has REBASE_HEAD: %v", err)
			}
			overview, err := newGitRunner().ConflictOverview(context.Background(), repo)
			if err != nil || overview.Context.OperationType != GitConflictOperationRebase {
				t.Fatalf("paused rebase overview: %#v, %v", overview, err)
			}
			seedGitProjectForTest(t, server, "git-project", repo)
			writeGitTestFile(t, repo, "readme.txt", "rebase work in progress\n")
			postGitStashForTest(t, server, "git-project", "stashes", map[string]any{}, http.StatusConflict)
			if data, err := os.ReadFile(filepath.Join(repo, "readme.txt")); err != nil || string(data) != "rebase work in progress\n" {
				t.Fatalf("stash changed the paused rebase: %q %v", data, err)
			}
		})
	}
}

func TestGitStashRestoreWithoutIndexExposesResolvableConflicts(t *testing.T) {
	server := newTestServer(t)
	repo := newTempGitRepository(t)
	writeGitTestFile(t, repo, "readme.txt", "initial\n")
	runGitForTest(t, repo, "add", ".")
	runGitForTest(t, repo, "commit", "-m", "initial")
	seedGitProjectForTest(t, server, "git-project", repo)
	writeGitTestFile(t, repo, "readme.txt", "staged stash\n")
	runGitForTest(t, repo, "add", ".")
	result := postGitStashForTest(t, server, "git-project", "stashes", map[string]any{}, http.StatusAccepted)
	if result.Status != gitOperationSucceeded {
		t.Fatalf("create: %#v", result)
	}
	stash := getGitStashesForTest(t, server, "git-project")[0]
	writeGitTestFile(t, repo, "readme.txt", "new branch content\n")
	runGitForTest(t, repo, "add", ".")
	runGitForTest(t, repo, "commit", "-m", "diverged")
	result = postGitStashForTest(t, server, "git-project", "stashes/apply", map[string]any{"oid": stash.OID}, http.StatusAccepted)
	if result.Status != gitOperationNeedsAttention {
		t.Fatalf("expected index restore failure: %#v", result)
	}
	if changes := gitOutputForTest(t, repo, "status", "--porcelain"); changes != "" {
		t.Fatalf("index restore unexpectedly left changes: %q", changes)
	}
	// 不恢复 index 时，Git 才能执行三方合并，产生可在页面解决的冲突。
	result = postGitStashForTest(t, server, "git-project", "stashes/apply", map[string]any{"oid": stash.OID, "restoreIndex": false}, http.StatusAccepted)
	if result.Status != gitOperationNeedsAttention {
		t.Fatalf("expected working tree conflict: %#v", result)
	}
	if summary := gitSummaryForTest(t, server, "git-project"); summary.Worktree.Conflicted != 1 {
		t.Fatalf("restore did not expose the conflict: %#v", summary.Worktree)
	}
	if stashes := getGitStashesForTest(t, server, "git-project"); len(stashes) != 1 || stashes[0].OID != stash.OID {
		t.Fatalf("restore lost backup: %#v", stashes)
	}
}

func TestGitStashChecksUntrackedFilesInsideDirectories(t *testing.T) {
	for _, statusSetting := range []string{"normal", "no"} {
		t.Run(statusSetting, func(t *testing.T) {
			server := newTestServer(t)
			repo := newTempGitRepository(t)
			writeGitTestFile(t, repo, "readme.txt", "initial\n")
			runGitForTest(t, repo, "add", ".")
			runGitForTest(t, repo, "commit", "-m", "initial")
			runGitForTest(t, repo, "config", "status.showUntrackedFiles", statusSetting)
			seedGitProjectForTest(t, server, "git-project", repo)
			if err := os.Mkdir(filepath.Join(repo, "nested"), 0o700); err != nil {
				t.Fatal(err)
			}
			writeGitTestFile(t, repo, "nested/new.txt", "confirmed\n")
			writeGitTestFile(t, repo, "readme.txt", "tracked change\n")
			summary := gitSummaryForTest(t, server, "git-project")
			if summary.Worktree.Untracked != 1 {
				t.Fatalf("untracked file is hidden from confirmation: %#v", summary.Worktree)
			}
			// 改文件内容不改变父目录时间；目录级指纹无法发现这种变化。
			writeGitTestFile(t, repo, "nested/new.txt", "changed later\n")
			later := time.Now().Add(time.Minute)
			if err := os.Chtimes(filepath.Join(repo, "nested/new.txt"), later, later); err != nil {
				t.Fatal(err)
			}
			postGitStashForTest(t, server, "git-project", "stashes", map[string]any{"stateToken": summary.StateToken}, http.StatusConflict)
			if stashes := getGitStashesForTest(t, server, "git-project"); len(stashes) != 0 {
				t.Fatalf("created stash after an unconfirmed nested file change: %#v", stashes)
			}
			if data, err := os.ReadFile(filepath.Join(repo, "nested/new.txt")); err != nil || string(data) != "changed later\n" {
				t.Fatalf("changed unconfirmed file: %q %v", data, err)
			}
			result := postGitStashForTest(t, server, "git-project", "stashes", map[string]any{}, http.StatusAccepted)
			if result.Status != gitOperationSucceeded {
				t.Fatalf("create with refreshed confirmation: %#v", result)
			}
			stash := getGitStashesForTest(t, server, "git-project")[0]
			writeGitTestFile(t, repo, "nested/new.txt", "must not be overwritten\n")
			postGitStashForTest(t, server, "git-project", "stashes/apply", map[string]any{"oid": stash.OID}, http.StatusConflict)
		})
	}
}

func TestGitStashRejectsIndexContentChangedWithoutWorktreeChange(t *testing.T) {
	server := newTestServer(t)
	repo := newTempGitRepository(t)
	writeGitTestFile(t, repo, "readme.txt", "initial\n")
	runGitForTest(t, repo, "add", ".")
	runGitForTest(t, repo, "commit", "-m", "initial")
	seedGitProjectForTest(t, server, "git-project", repo)
	writeGitTestFile(t, repo, "readme.txt", "first staged version\n")
	runGitForTest(t, repo, "add", ".")
	firstOID := strings.TrimSpace(gitOutputForTest(t, repo, "rev-parse", ":readme.txt"))
	writeGitTestFile(t, repo, "readme.txt", "second staged version\n")
	runGitForTest(t, repo, "add", ".")
	secondOID := strings.TrimSpace(gitOutputForTest(t, repo, "rev-parse", ":readme.txt"))
	writeGitTestFile(t, repo, "readme.txt", "unchanged working content\n")
	runGitForTest(t, repo, "update-index", "--cacheinfo", "100644,"+firstOID+",readme.txt")
	confirmed := gitSummaryForTest(t, server, "git-project")
	// 只替换暂存区内容，文件路径、MM 标记、计数与工作区文件指纹都不变。
	runGitForTest(t, repo, "update-index", "--cacheinfo", "100644,"+secondOID+",readme.txt")
	postGitStashForTest(t, server, "git-project", "stashes", map[string]any{"stateToken": confirmed.StateToken}, http.StatusConflict)
	if stashes := getGitStashesForTest(t, server, "git-project"); len(stashes) != 0 {
		t.Fatalf("stashed an unconfirmed index version: %#v", stashes)
	}
	if content := gitOutputForTest(t, repo, "show", ":readme.txt"); content != "second staged version\n" {
		t.Fatalf("changed index content: %q", content)
	}
	if content, err := os.ReadFile(filepath.Join(repo, "readme.txt")); err != nil || string(content) != "unchanged working content\n" {
		t.Fatalf("changed worktree content: %q %v", content, err)
	}
	refreshed := gitSummaryForTest(t, server, "git-project")
	if confirmed.ChangesRevision == refreshed.ChangesRevision {
		t.Fatal("the background probe cannot detect the changed index content")
	}
	result := postGitStashForTest(t, server, "git-project", "stashes", map[string]any{"stateToken": refreshed.StateToken}, http.StatusAccepted)
	if result.Status != gitOperationSucceeded {
		t.Fatalf("create after confirming the new index: %#v", result)
	}
	stash := getGitStashesForTest(t, server, "git-project")[0]
	if content := gitOutputForTest(t, repo, "show", stash.OID+"^2:readme.txt"); content != "second staged version\n" {
		t.Fatalf("saved the wrong index version: %q", content)
	}
}
