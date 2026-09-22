package app

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// executableSuffix 给出当前平台的普通可执行文件后缀（夹具用）。
func executableSuffix() string {
	if runtime.GOOS == "windows" {
		return ".cmd"
	}
	return ""
}

// writeExecutable 写一个"固定输出"的可执行夹具。
func writeExecutable(t *testing.T, path, output string) {
	t.Helper()
	var body string
	if runtime.GOOS == "windows" {
		body = "@echo off\r\necho " + output + "\r\n"
	} else {
		body = "#!/bin/sh\necho " + output + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("写夹具 %s 失败：%v", path, err)
	}
}

// TestAgentPathResolverHonoursOrder 钉住解析顺序：环境覆盖 > 登记表 > PATH > 平台兜底。
//
// 顺序不许换。登记表那档是"平台自己装出来的实测结果"，如果被 PATH 或一次探测压过去，
// 就会表现成"装好了但界面还说没装"。
func TestAgentPathResolverHonoursOrder(t *testing.T) {
	dir := t.TempDir()
	recorded := filepath.Join(dir, "recorded"+executableSuffix())
	override := filepath.Join(dir, "override"+executableSuffix())
	writeExecutable(t, recorded, "recorded")
	writeExecutable(t, override, "override")

	// 一档：环境覆盖赢过登记表。
	resolver := &agentPathResolver{
		override: map[string]string{"claude-code": override},
		recorded: map[string]string{"claude-code": recorded},
	}
	if got := resolver.Path("claude-code"); got != override {
		t.Fatalf("环境覆盖应当优先，得到 %q", got)
	}

	// 二档：没有覆盖时登记表赢。
	resolver = &agentPathResolver{override: map[string]string{}, recorded: map[string]string{"claude-code": recorded}}
	if got := resolver.Path("claude-code"); got != recorded {
		t.Fatalf("登记表应当优先于 PATH 查找，得到 %q", got)
	}

	// 未知工具返回空串（不能回落到某个已知工具）。
	if got := resolver.Path("no-such-agent"); got != "" {
		t.Fatalf("未知工具应返回空串，得到 %q", got)
	}
}

// TestAgentPathResolverSkipsStaleRecordedPath 是"记录还在、文件没了"的那一档。
//
// 装完之后被用户删掉、或换了一台机器（同一个数据库被搬过去），登记表里那条就会指空。
// 这时必须继续往下找，而不是抱着死路径不放 —— 否则界面会一直说"已安装"却永远跑不起来。
func TestAgentPathResolverSkipsStaleRecordedPath(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "gone"+executableSuffix())
	resolver := &agentPathResolver{override: map[string]string{}, recorded: map[string]string{"claude-code": missing}}

	got := resolver.Path("claude-code")
	if got == missing {
		t.Fatal("登记表里的路径已不存在，仍被采用")
	}
	// 全都找不到时回落成目录里的命令名（与改动前 Config 的缺省值一致）。
	if got == "" {
		t.Fatal("解析结果不该是空串：回落值应当是命令名，让上层报错里出现的是工具名")
	}
}

// TestRunnerUsesResolvedBinaryNotConfigPath 是这一轮最关键的行为用例。
//
// 它模拟"平台把 CLI 装到了自己的目录里"：可执行文件既不在 PATH 上，也不在
// Config.ClaudePath 指向的位置。修复前的情况是 —— runner 只认 Config.ClaudePath，
// 于是装完仍然 `Version()` 为空、界面显示"未安装"。
//
// 判据刻意用 `Version()`：它真的会去执行那个文件。如果把路径来源改回 Config（值是一段
// 不存在的路径），Version() 会返回空串 —— 用例立刻变红。
func TestRunnerUsesResolvedBinaryNotConfigPath(t *testing.T) {
	dir := t.TempDir()
	managed := filepath.Join(dir, "managed", "claude"+executableSuffix())
	if err := os.MkdirAll(filepath.Dir(managed), 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, managed, "2.9.9")

	// Config 指向一个**不存在**的位置，模拟"启动时那会儿还没装"。
	config := Config{ClaudePath: filepath.Join(dir, "missing", "claude")}
	resolver := &agentPathResolver{override: map[string]string{}, recorded: map[string]string{"claude-code": managed}}

	runner := newClaudeCLIRunner(config, resolver)
	if got := runner.Version(context.Background()); got != "2.9.9" {
		t.Fatalf("Version() = %q，期望夹具的 2.9.9 —— runner 没有走解析器，仍在直接用 Config 里的旧路径", got)
	}

	// 反向：把记录清掉之后，同一个 runner 必须**读不到**版本。
	// 没有这一半，上面那条断言就只是"永远返回夹具值"。
	resolver.Forget("claude-code")
	if got := runner.Version(context.Background()); got == "2.9.9" {
		t.Fatal("记录已清除，Version() 仍回到了夹具 —— 说明它读的不是解析器")
	}
}

// TestCodexRunnerUsesResolvedBinary 是上一条的 Codex 侧对称用例（两个工具都经过解析器）。
func TestCodexRunnerUsesResolvedBinary(t *testing.T) {
	dir := t.TempDir()
	managed := filepath.Join(dir, "managed", "codex"+executableSuffix())
	if err := os.MkdirAll(filepath.Dir(managed), 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, managed, "codex-cli 0.146.0")

	config := Config{CodexPath: filepath.Join(dir, "missing", "codex")}
	resolver := &agentPathResolver{override: map[string]string{}, recorded: map[string]string{"codex": managed}}

	runner := newCodexCLIRunner(config, resolver)
	if got := runner.Version(context.Background()); got != "0.146.0" {
		t.Fatalf("Version() = %q，期望 0.146.0 —— Codex runner 没有走解析器", got)
	}
}

// TestAgentInstallationRoundTrip 覆盖登记表的写入、载入与解析器同步。
func TestAgentInstallationRoundTrip(t *testing.T) {
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "installations.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	server := &Server{db: db, paths: &agentPathResolver{override: map[string]string{}, recorded: map[string]string{}}}
	ctx := context.Background()
	if err := server.migrateAgentInstallations(ctx); err != nil {
		t.Fatalf("建表失败：%v", err)
	}

	runnerID := server.localRunnerID()
	binary := filepath.Join(t.TempDir(), "claude"+executableSuffix())
	writeExecutable(t, binary, "2.9.9")

	// 路径与 install_kind / prefix **成对**记录：只有路径而没有 prefix 时，
	// 后续"升级"会去找系统 npm 的全局位置，而不是这个工具实际所在的托管位置。
	if err := server.recordAgentInstallation(ctx, agentInstallation{
		RunnerID: runnerID, AgentID: "claude-code", BinaryPath: binary,
		InstallKind: "npm-global-managed", Prefix: "/tmp/managed-prefix", Version: "2.9.9", Source: "managed-install",
	}); err != nil {
		t.Fatalf("记录失败：%v", err)
	}

	// 记录之后解析器立刻能用（不必重启）。
	if got := server.paths.Path("claude-code"); got != binary {
		t.Fatalf("记录后解析器给出 %q，期望 %q", got, binary)
	}

	loaded, err := server.loadAgentInstallations(ctx, runnerID)
	if err != nil {
		t.Fatalf("载入失败：%v", err)
	}
	if loaded["claude-code"] != binary {
		t.Fatalf("载入结果 %#v 里没有刚写的路径", loaded)
	}

	items, err := server.listAgentInstallations(ctx, runnerID)
	if err != nil {
		t.Fatalf("列表失败：%v", err)
	}
	if len(items) != 1 || items[0].Prefix != "/tmp/managed-prefix" || items[0].InstallKind != "npm-global-managed" {
		t.Fatalf("登记项元数据不完整：%#v", items)
	}

	// 忘掉之后解析器也要跟着忘（否则"卸载"之后界面还能跑起来，是假的）。
	// 直接删登记行。原先走的是 forgetAgentInstallation —— 那个函数只被这条测试用，
	// 已删掉：本期不做卸载，留一个没有生产调用者的函数是负担。
	if _, err := server.db.ExecContext(ctx,
		`delete from agent_installations where runner_id=? and agent_id=?`, runnerID, "claude-code"); err != nil {
		t.Fatalf("删除失败：%v", err)
	}
	if items, err := server.listAgentInstallations(ctx, runnerID); err != nil || len(items) != 0 {
		t.Fatalf("删除后仍有登记项：%#v err=%v", items, err)
	}
}

// TestConfigEnvOnlyCarriesExplicitOverride 钉住"平台兜底不再伪装成用户覆盖"这件事。
//
// 改动前 ConfigFromEnv 会把探测到的 npm shim 绝对路径写进 CodexPath，从值上看起来与
// 用户显式指定无法区分，于是会盖掉登记表里的实测路径。现在它只承载真正的环境变量。
func TestConfigEnvOnlyCarriesExplicitOverride(t *testing.T) {
	t.Setenv("AUTO_CODEX_PATH", "")
	t.Setenv("AUTO_CLAUDE_PATH", "")
	config := ConfigFromEnv()
	if config.CodexPath != "codex" {
		t.Fatalf("没有环境变量时 CodexPath 应当是裸命令名，得到 %q", config.CodexPath)
	}
	if config.ClaudePath != "claude" {
		t.Fatalf("没有环境变量时 ClaudePath 应当是裸命令名，得到 %q", config.ClaudePath)
	}

	// 解析器据此判断"没有覆盖"，于是登记表那一档能生效。
	resolver := newAgentPathResolver(config)
	if len(resolver.override) != 0 {
		t.Fatalf("裸命令名不该被当成覆盖：%#v", resolver.override)
	}

	t.Setenv("AUTO_CLAUDE_PATH", "/custom/claude")
	config = ConfigFromEnv()
	resolver = newAgentPathResolver(config)
	if resolver.override["claude-code"] != "/custom/claude" {
		t.Fatalf("显式设置的环境变量应当成为覆盖：%#v", resolver.override)
	}
}
