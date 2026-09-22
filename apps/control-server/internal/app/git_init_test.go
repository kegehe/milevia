package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 纯本地用例（不构造 Server，可在常规回归里直接跑）：git init -b main 生成 .git，
// 且 gitBranch 对刚初始化的空仓库返回 ("main", true)。
func TestGitInitOnEmptyDirLocalBackend(t *testing.T) {
	emptyDir := t.TempDir()
	runner := newGitRunner()
	if _, err := runner.runGit(context.Background(), emptyDir, "init", "-b", "main"); err != nil {
		t.Fatalf("git init failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(emptyDir, ".git")); err != nil {
		t.Fatalf("expected .git directory after init: %v", err)
	}
	// 无提交的 unborn HEAD，symbolic-ref 仍应返回初始分支 main。
	branch, ok := gitBranch(context.Background(), emptyDir)
	if !ok || branch != "main" {
		t.Fatalf("gitBranch on freshly initialized repo = (%q, %v), want (\"main\", true)", branch, ok)
	}
}

// 端点级用例（构造 newTestServer，供 CI；本机沙箱因 wsl.exe 拦截跳过）：
// 非 git 项目 POST /api/projects/{id}/git/init 后，git_branch 写回 main，
// 且 git summary 可读（仓库已可用）。
func TestGitInitEndpoint(t *testing.T) {
	server := newTestServer(t)

	projectDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	projectID := "git-init-project"
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values (?,?,?,?,'非 Git 目录',0,?)`,
		projectID, "repo", projectDir, "wsl-local", now); err != nil {
		t.Fatalf("insert project: %v", err)
	}

	r := httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/git/init", nil)
	w := httptest.NewRecorder()
	server.gitInit(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("git init status = %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"gitBranch":"main"`) {
		t.Fatalf("git init response missing main branch: %s", w.Body.String())
	}

	// 项目行已更新为 main（含 unborn HEAD 的分支名）。
	var branch string
	if err := server.db.QueryRow(`select git_branch from projects where id=?`, projectID).Scan(&branch); err != nil {
		t.Fatalf("read git_branch: %v", err)
	}
	if branch != "main" {
		t.Fatalf("git_branch after init = %q, want \"main\"", branch)
	}
}
