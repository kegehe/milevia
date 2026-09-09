package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestProjectGitConflictResolveApi 覆盖冲突解决 REST 链路的端到端行为：
// 冲突总览 → 单文件三方内容 → 采用 theirs 解决 → 完成 merge。
func TestProjectGitConflictResolveApi(t *testing.T) {
	server := newTestServer(t)
	repo := newTempGitRepository(t)
	writeGitTestFile(t, repo, "readme.txt", "one\ntwo\n")
	runGitForTest(t, repo, "add", "readme.txt")
	runGitForTest(t, repo, "commit", "-m", "initial")
	runGitForTest(t, repo, "checkout", "-b", "feature")
	writeGitTestFile(t, repo, "readme.txt", "one\nfeature\n")
	runGitForTest(t, repo, "add", "readme.txt")
	runGitForTest(t, repo, "commit", "-m", "feature change")
	runGitForTest(t, repo, "checkout", "main")
	writeGitTestFile(t, repo, "readme.txt", "one\nmain\n")
	runGitForTest(t, repo, "add", "readme.txt")
	runGitForTest(t, repo, "commit", "-m", "main change")
	runGitForTestExpectConflict(t, repo, "merge", "feature")
	seedGitProjectForTest(t, server, "git-project", repo)

	// 冲突总览：merge 上下文 + 一个冲突文件。
	overview := httptest.NewRecorder()
	server.routes().ServeHTTP(overview, httptest.NewRequest(http.MethodGet, "/api/projects/git-project/git/conflicts", nil))
	if overview.Code != http.StatusOK {
		t.Fatalf("conflicts status=%d body=%s", overview.Code, overview.Body.String())
	}
	var meta GitConflictOverview
	if err := json.Unmarshal(overview.Body.Bytes(), &meta); err != nil {
		t.Fatalf("decode conflicts: %v", err)
	}
	if meta.Context.OperationType != GitConflictOperationMerge || len(meta.Files) != 1 || meta.Files[0].Path != "readme.txt" {
		t.Fatalf("unexpected conflict overview: %#v", meta)
	}

	// 单文件三方内容。
	content := httptest.NewRecorder()
	server.routes().ServeHTTP(content, httptest.NewRequest(http.MethodGet, "/api/projects/git-project/git/conflicts/content?path=readme.txt", nil))
	if content.Code != http.StatusOK {
		t.Fatalf("conflict content status=%d body=%s", content.Code, content.Body.String())
	}
	var detail GitConflictContent
	if err := json.Unmarshal(content.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode conflict content: %v", err)
	}
	if !bytes.Contains([]byte(detail.Working), []byte("<<<<<<<")) || !bytes.Contains([]byte(detail.Working), []byte(">>>>>>>")) {
		t.Fatalf("working content should carry markers: %q", detail.Working)
	}
	if detail.Ours != "one\nmain\n" || detail.Theirs != "one\nfeature\n" {
		t.Fatalf("three-way content mismatch: ours=%q theirs=%q", detail.Ours, detail.Theirs)
	}

	// 采用 theirs 解决。
	summary := gitSummaryForTest(t, server, "git-project")
	resolve := httptest.NewRecorder()
	server.routes().ServeHTTP(resolve, httptest.NewRequest(http.MethodPost, "/api/projects/git-project/git/conflicts/resolve", bytes.NewBufferString(`{"path":"readme.txt","action":"theirs","stateToken":"`+summary.StateToken+`"}`)))
	if resolve.Code != http.StatusAccepted {
		t.Fatalf("resolve status=%d body=%s", resolve.Code, resolve.Body.String())
	}
	afterResolve := gitSummaryForTest(t, server, "git-project")
	if afterResolve.Worktree.Conflicted != 0 || afterResolve.Worktree.Staged != 1 {
		t.Fatalf("unexpected worktree after resolve: %#v", afterResolve.Worktree)
	}

	// 完成 merge（生成合并提交）。
	summary = gitSummaryForTest(t, server, "git-project")
	finish := httptest.NewRecorder()
	server.routes().ServeHTTP(finish, httptest.NewRequest(http.MethodPost, "/api/projects/git-project/git/conflicts/continue", bytes.NewBufferString(`{"stateToken":"`+summary.StateToken+`"}`)))
	if finish.Code != http.StatusAccepted {
		t.Fatalf("finish status=%d body=%s", finish.Code, finish.Body.String())
	}
	parents := gitOutputForTest(t, repo, "log", "-1", "--format=%P")
	if len(bytes.Fields([]byte(parents))) != 2 {
		t.Fatalf("expected a merge commit with two parents, got %q", parents)
	}
}

// TestProjectGitConflictAbortApi 覆盖中止 merge 的完整 HTTP 链路（含 stateToken），
// 中止后应回到 merge 前的主分支提交且不再有冲突。
func TestProjectGitConflictAbortApi(t *testing.T) {
	server := newTestServer(t)
	repo := newTempGitRepository(t)
	writeGitTestFile(t, repo, "readme.txt", "one\n")
	runGitForTest(t, repo, "add", "readme.txt")
	runGitForTest(t, repo, "commit", "-m", "initial")
	runGitForTest(t, repo, "checkout", "-b", "feature")
	writeGitTestFile(t, repo, "readme.txt", "feature\n")
	runGitForTest(t, repo, "add", "readme.txt")
	runGitForTest(t, repo, "commit", "-m", "feature change")
	runGitForTest(t, repo, "checkout", "main")
	writeGitTestFile(t, repo, "readme.txt", "main\n")
	runGitForTest(t, repo, "add", "readme.txt")
	runGitForTest(t, repo, "commit", "-m", "main change")
	before := gitOutputForTest(t, repo, "rev-parse", "HEAD")
	runGitForTestExpectConflict(t, repo, "merge", "feature")
	seedGitProjectForTest(t, server, "git-project", repo)

	summary := gitSummaryForTest(t, server, "git-project")
	if summary.Worktree.Conflicted != 1 {
		t.Fatalf("expected one conflicted file, got %d", summary.Worktree.Conflicted)
	}
	abort := httptest.NewRecorder()
	server.routes().ServeHTTP(abort, httptest.NewRequest(http.MethodPost, "/api/projects/git-project/git/conflicts/abort", bytes.NewBufferString(`{"stateToken":"`+summary.StateToken+`"}`)))
	if abort.Code != http.StatusAccepted {
		t.Fatalf("abort status=%d body=%s", abort.Code, abort.Body.String())
	}
	if after := gitOutputForTest(t, repo, "rev-parse", "HEAD"); strings.TrimSpace(after) != strings.TrimSpace(before) {
		t.Fatalf("abort did not restore HEAD: before=%q after=%q", strings.TrimSpace(before), strings.TrimSpace(after))
	}
	if restored := gitSummaryForTest(t, server, "git-project"); restored.Worktree.Conflicted != 0 {
		t.Fatalf("conflicts remain after abort: %#v", restored.Worktree)
	}
}

// TestProjectGitConflictResolveWorkingApi 覆盖“手工编辑后标记为已解决”的 REST 行为。
func TestProjectGitConflictResolveWorkingApi(t *testing.T) {
	server := newTestServer(t)
	repo := newTempGitRepository(t)
	writeGitTestFile(t, repo, "readme.txt", "one\n")
	runGitForTest(t, repo, "add", "readme.txt")
	runGitForTest(t, repo, "commit", "-m", "initial")
	runGitForTest(t, repo, "checkout", "-b", "feature")
	writeGitTestFile(t, repo, "readme.txt", "feature\n")
	runGitForTest(t, repo, "add", "readme.txt")
	runGitForTest(t, repo, "commit", "-m", "feature change")
	runGitForTest(t, repo, "checkout", "main")
	writeGitTestFile(t, repo, "readme.txt", "main\n")
	runGitForTest(t, repo, "add", "readme.txt")
	runGitForTest(t, repo, "commit", "-m", "main change")
	runGitForTestExpectConflict(t, repo, "merge", "feature")
	seedGitProjectForTest(t, server, "git-project", repo)

	summary := gitSummaryForTest(t, server, "git-project")
	body := `{"path":"readme.txt","action":"working","content":"hand merged\n","stateToken":"` + summary.StateToken + `"}`
	resolve := httptest.NewRecorder()
	server.routes().ServeHTTP(resolve, httptest.NewRequest(http.MethodPost, "/api/projects/git-project/git/conflicts/resolve", bytes.NewBufferString(body)))
	if resolve.Code != http.StatusAccepted {
		t.Fatalf("resolve working status=%d body=%s", resolve.Code, resolve.Body.String())
	}
	after := gitSummaryForTest(t, server, "git-project")
	if after.Worktree.Conflicted != 0 || after.Worktree.Staged != 1 {
		t.Fatalf("unexpected worktree after working resolve: %#v", after.Worktree)
	}
	if raw := gitOutputForTest(t, repo, "show", ":readme.txt"); raw != "hand merged\n" {
		t.Fatalf("staged content mismatch: %q", raw)
	}
}
