package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runGitForTestExpectConflict 执行会产生冲突（非零退出但带 CONFLICT 提示）的 git 命令。
func runGitForTestExpectConflict(t *testing.T, repo string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repo}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil && !strings.Contains(string(output), "CONFLICT (") {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func TestGitConflictsContentMergeOverviewAndResolveOurs(t *testing.T) {
	repo := newTempGitRepository(t)
	makeGitConflictFixture(t, repo)
	runner := newGitRunner()
	ctx := context.Background()

	overview, err := runner.ConflictOverview(ctx, repo)
	if err != nil {
		t.Fatalf("read conflict overview: %v", err)
	}
	if overview.Context.OperationType != GitConflictOperationMerge {
		t.Fatalf("operation type: got=%q want=merge", overview.Context.OperationType)
	}
	if overview.Context.OursLabel != "main" || overview.Context.TheirsLabel != "feature" {
		t.Fatalf("unexpected side labels: ours=%q theirs=%q", overview.Context.OursLabel, overview.Context.TheirsLabel)
	}
	if len(overview.Files) != 1 || overview.Files[0].Path != "app.txt" || overview.Files[0].Kind != "content" {
		t.Fatalf("unexpected conflict files: %#v", overview.Files)
	}

	content, err := runner.ConflictContent(ctx, repo, "app.txt")
	if err != nil {
		t.Fatalf("read conflict content: %v", err)
	}
	if content.OursDeleted || content.TheirsDeleted || content.Binary {
		t.Fatalf("unexpected conflict flags: %#v", content)
	}
	if !strings.Contains(content.Working, "<<<<<<<") || !strings.Contains(content.Working, ">>>>>>>") {
		t.Fatalf("working file should still carry conflict markers: %q", content.Working)
	}
	if !strings.Contains(content.Ours, "line2 main-side") || !strings.Contains(content.Theirs, "line2 feature-side") || !strings.Contains(content.Base, "line2 base") {
		t.Fatalf("three-way content mismatch: ours=%q theirs=%q base=%q", content.Ours, content.Theirs, content.Base)
	}

	// 采用 ours 后仓库应不再有未合并路径，且工作区回到 main 侧内容。
	if err := runner.ResolveConflict(ctx, repo, "app.txt", "ours", nil); err != nil {
		t.Fatalf("resolve conflict as ours: %v", err)
	}
	changes, err := runner.Changes(ctx, repo)
	if err != nil {
		t.Fatalf("read changes after resolve: %v", err)
	}
	for _, change := range changes {
		if change.Conflicted {
			t.Fatalf("conflict remains after resolve: %#v", change)
		}
	}
	if raw, err := os.ReadFile(filepath.Join(repo, "app.txt")); err != nil || strings.Contains(string(raw), "feature-side") {
		t.Fatalf("ours content not applied: %q err=%v", string(raw), err)
	}
}

func TestGitConflictsContentMergeResolveTheirs(t *testing.T) {
	repo := newTempGitRepository(t)
	makeGitConflictFixture(t, repo)
	runner := newGitRunner()
	ctx := context.Background()
	if err := runner.ResolveConflict(ctx, repo, "app.txt", "theirs", nil); err != nil {
		t.Fatalf("resolve conflict as theirs: %v", err)
	}
	if raw, err := os.ReadFile(filepath.Join(repo, "app.txt")); err != nil || !strings.Contains(string(raw), "feature-side") || strings.Contains(string(raw), "main-side") {
		t.Fatalf("theirs content not applied: %q err=%v", string(raw), err)
	}
}

func TestGitConflictsResolveWorkingWritesResolvedContent(t *testing.T) {
	repo := newTempGitRepository(t)
	makeGitConflictFixture(t, repo)
	runner := newGitRunner()
	ctx := context.Background()

	resolved := "line1\nline2 hand-merged\nline3\nline4 shared\nline5\n"
	if err := runner.ResolveConflict(ctx, repo, "app.txt", "working", []byte(resolved)); err != nil {
		t.Fatalf("resolve conflict with working content: %v", err)
	}
	changes, err := runner.Changes(ctx, repo)
	if err != nil {
		t.Fatalf("read changes after working resolve: %v", err)
	}
	for _, change := range changes {
		if change.Conflicted {
			t.Fatalf("conflict remains after working resolve: %#v", change)
		}
	}
	if raw, err := os.ReadFile(filepath.Join(repo, "app.txt")); err != nil || string(raw) != resolved {
		t.Fatalf("working content not persisted: %q err=%v", string(raw), err)
	}
}

func TestGitConflictsModifyDeleteResolveToDeleteAndKeep(t *testing.T) {
	repo := newTempGitRepository(t)
	// main 修改、feature 删除 → UD：theirs 为删除。
	writeGitTestFile(t, repo, "del.txt", "keep me\n")
	runGitForTest(t, repo, "add", "del.txt")
	runGitForTest(t, repo, "commit", "-m", "base")
	runGitForTest(t, repo, "checkout", "-b", "feature")
	runGitForTest(t, repo, "rm", "del.txt")
	runGitForTest(t, repo, "commit", "-m", "delete del.txt on feature")
	runGitForTest(t, repo, "checkout", "main")
	writeGitTestFile(t, repo, "del.txt", "keep me\nchanged on main\n")
	runGitForTest(t, repo, "add", "del.txt")
	runGitForTest(t, repo, "commit", "-m", "modify del.txt on main")
	runGitForTestExpectConflict(t, repo, "merge", "feature")

	runner := newGitRunner()
	ctx := context.Background()
	overview, err := runner.ConflictOverview(ctx, repo)
	if err != nil {
		t.Fatalf("read conflict overview: %v", err)
	}
	files := overview.Files
	if len(files) != 1 || files[0].Path != "del.txt" || files[0].Kind != "modify-delete" || !files[0].TheirsDeleted {
		t.Fatalf("unexpected modify/delete file meta: %#v", files)
	}

	// theirs=删除：接受 theirs 应把文件以删除收场。
	if err := runner.ResolveConflict(ctx, repo, "del.txt", "theirs", nil); err != nil {
		t.Fatalf("resolve modify/delete as theirs: %v", err)
	}
	changes, err := runner.Changes(ctx, repo)
	if err != nil {
		t.Fatalf("read changes: %v", err)
	}
	for _, change := range changes {
		if change.Conflicted {
			t.Fatalf("conflict remains: %#v", change)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, "del.txt")); !os.IsNotExist(err) {
		t.Fatal("theirs-deleted resolution should remove the file")
	}

	// ours=保留：先中止 theirs 的删除解决，再重新制造冲突后接受 ours，文件应保留。
	runGitForTest(t, repo, "merge", "--abort")
	runGitForTestExpectConflict(t, repo, "merge", "feature")
	if err := runner.ResolveConflict(ctx, repo, "del.txt", "ours", nil); err != nil {
		t.Fatalf("resolve modify/delete as ours: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "del.txt")); err != nil {
		t.Fatalf("ours-keep resolution should keep the file: %v", err)
	}
}

func TestGitConflictsAbortMergeRestoresHead(t *testing.T) {
	repo := newTempGitRepository(t)
	makeGitConflictFixture(t, repo)
	runner := newGitRunner()
	ctx := context.Background()
	before, err := runner.Snapshot(ctx, repo)
	if err != nil {
		t.Fatalf("read snapshot before abort: %v", err)
	}
	if err := runner.AbortConflict(ctx, repo); err != nil {
		t.Fatalf("abort merge: %v", err)
	}
	after, err := runner.Snapshot(ctx, repo)
	if err != nil {
		t.Fatalf("read snapshot after abort: %v", err)
	}
	if after.Head.OID != before.Head.OID {
		t.Fatalf("abort did not restore HEAD: before=%s after=%s", before.Head.OID, after.Head.OID)
	}
	if after.Worktree.Conflicted != 0 {
		t.Fatalf("conflicts remain after abort: %#v", after.Worktree)
	}
}

func TestGitConflictsFinishMergeCreatesMergeCommit(t *testing.T) {
	repo := newTempGitRepository(t)
	makeGitConflictFixture(t, repo)
	runner := newGitRunner()
	ctx := context.Background()
	if err := runner.ResolveConflict(ctx, repo, "app.txt", "ours", nil); err != nil {
		t.Fatalf("resolve conflict: %v", err)
	}
	if err := runner.FinishConflict(ctx, repo); err != nil {
		t.Fatalf("finish merge: %v", err)
	}
	parents := strings.Fields(gitOutputForTest(t, repo, "log", "-1", "--format=%P"))
	if len(parents) != 2 {
		t.Fatalf("expected a merge commit with two parents, got parents=%v", parents)
	}
	changes, err := runner.Changes(ctx, repo)
	if err != nil {
		t.Fatalf("read changes after finish: %v", err)
	}
	for _, change := range changes {
		if change.Conflicted {
			t.Fatalf("conflict remains after finish: %#v", change)
		}
	}
}

func TestGitConflictsRebaseLabelsAreNotInverted(t *testing.T) {
	repo := newTempGitRepository(t)
	makeGitConflictFixture(t, repo)
	runGitForTest(t, repo, "merge", "--abort")
	// feature 落后于 main，把 feature 变基到 main 会再次冲突。
	runGitForTest(t, repo, "checkout", "feature")
	runGitForTestExpectConflict(t, repo, "rebase", "main")
	runner := newGitRunner()
	ctx := context.Background()
	overview, err := runner.ConflictOverview(ctx, repo)
	if err != nil {
		t.Fatalf("read rebase conflict overview: %v", err)
	}
	if overview.Context.OperationType != GitConflictOperationRebase {
		t.Fatalf("operation type: got=%q want=rebase", overview.Context.OperationType)
	}
	if !strings.Contains(overview.Context.OursLabel, "main") || !strings.Contains(overview.Context.TheirsLabel, "feature") {
		t.Fatalf("rebase labels should read ours=main theirs=feature, got ours=%q theirs=%q", overview.Context.OursLabel, overview.Context.TheirsLabel)
	}
	// rebase 冲突中 ours=目标基底（main），theirs=正在重放的提交（feature）。
	// 采用 theirs 才能在重放后保留 feature 的改动与提交。
	if err := runner.ResolveConflict(ctx, repo, "app.txt", "theirs", nil); err != nil {
		t.Fatalf("resolve rebase conflict as theirs: %v", err)
	}
	if err := runner.FinishConflict(ctx, repo); err != nil {
		t.Fatalf("finish rebase: %v", err)
	}
	out := strings.TrimSpace(gitOutputForTest(t, repo, "log", "-1", "--format=%s"))
	if !strings.Contains(out, "feature") {
		t.Fatalf("rebase should preserve feature commit, got %q", out)
	}
}

// makeGitConflictFixture 构造 main 与 feature 对 app.txt 的冲突，并留在冲突中的 main 上。
func makeGitConflictFixture(t *testing.T, repo string) {
	t.Helper()
	writeGitTestFile(t, repo, "app.txt", "line1\nline2 base\nline3\nline4 shared\nline5\n")
	runGitForTest(t, repo, "add", "app.txt")
	runGitForTest(t, repo, "commit", "-m", "base")
	runGitForTest(t, repo, "checkout", "-b", "feature")
	writeGitTestFile(t, repo, "app.txt", "line0\nline1\nline2 feature-side\nline3\nline4 shared\nline5\n")
	runGitForTest(t, repo, "add", "app.txt")
	runGitForTest(t, repo, "commit", "-m", "feature change")
	runGitForTest(t, repo, "checkout", "main")
	writeGitTestFile(t, repo, "app.txt", "line1\nline2 main-side\nline3\nline4 shared\nline5\nline6 main\n")
	runGitForTest(t, repo, "add", "app.txt")
	runGitForTest(t, repo, "commit", "-m", "main change")
	runGitForTestExpectConflict(t, repo, "merge", "feature")
}
