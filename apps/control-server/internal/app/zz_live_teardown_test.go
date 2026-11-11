package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLiveClaudeOneShotTeardown 用**真实的 claude CLI**（以及本机真实的
// ~/.claude/settings.json）跑一次只读一次性运行，验证 2026-11-04 回归的两个点：
//
//  1. result 事件之后 CLI 还有约 0.55s 收尾，收尾看护不能在这个窗口里杀进程；
//  2. 成功 result 已经完整到达时，进程退出码不决定成败。
//
// 默认跳过（会真实调用模型、消耗额度）：MILEVIA_LIVE_CLAUDE=1 时运行。
func TestLiveClaudeOneShotTeardown(t *testing.T) {
	if os.Getenv("MILEVIA_LIVE_CLAUDE") == "" {
		t.Skip("live test: set MILEVIA_LIVE_CLAUDE=1 to run against the real Claude CLI")
	}
	const magic = "MAGIC-TOKEN-42"
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(magic), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	sink := &claudeOutputTestSink{}
	runner := &claudeCLIRunner{config: Config{ClaudePath: "claude"}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	started := time.Now()
	err := runner.Run(ctx, AgentRunRequest{
		SessionID:      "00000000-0000-4000-8000-000000000000",
		ProjectPath:    dir,
		Prompt:         `读取当前目录的 a.txt，然后只调用 StructuredOutput 工具返回 {"answer":"文件内容"}。不要做其它事，不要询问。`,
		PermissionMode: "default",
		ReadOnlyTools:  []string{"Read", "Glob", "Grep"},
		PromptViaStdin: true,
		SkipSessionID:  true,
		OutputSchema: json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},` +
			`"required":["answer"],"additionalProperties":false}`),
	}, sink)
	if err != nil {
		t.Fatalf("real claude one-shot failed after %s: %v", time.Since(started).Round(time.Millisecond), err)
	}
	if ctx.Err() != nil {
		t.Fatalf("run did not finish within the test budget: %v", ctx.Err())
	}
	joined := strings.Join(sink.texts, "")
	if !strings.Contains(joined, magic) {
		t.Fatalf("structured output missing the file content; got %q", joined)
	}
	t.Logf("real one-shot succeeded in %s; structured output: %s", time.Since(started).Round(time.Millisecond), joined)
}
