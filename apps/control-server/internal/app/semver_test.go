package app

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestUpdateAvailableFromRejectsOlderRegistryVersion 是这次修掉的真实缺陷所对应的用例。
//
// 修之前 Claude 侧的判据是 `latest != local`（字符串不等），所以只要 registry 上的
// 版本和本地不一样就报"有更新可用"—— 用户装了比 registry 更新的预发布版时，管理页
// 会把**降级**当成升级推给他。这里锁死"远端更旧 → 没有更新"。
func TestUpdateAvailableFromRejectsOlderRegistryVersion(t *testing.T) {
	available, err := updateAvailableFrom("2.1.216", "1.0.0")
	if err != nil {
		t.Fatalf("updateAvailableFrom returned error: %v", err)
	}
	if available {
		t.Fatal("registry 上的 1.0.0 比本地 2.1.216 旧，却被报成有更新可用 —— 会把用户的降级当成升级")
	}

	// 反向：远端更高必须报有更新（否则上面那条就只是"永远 false"）。
	available, err = updateAvailableFrom("2.1.216", "2.1.217")
	if err != nil {
		t.Fatalf("updateAvailableFrom returned error: %v", err)
	}
	if !available {
		t.Fatal("registry 上的 2.1.217 比本地 2.1.216 新，却没有报有更新")
	}

	// 相同版本不算更新。
	if available, err = updateAvailableFrom("2.1.216", "2.1.216"); err != nil || available {
		t.Fatalf("同版本 should be no-update; got available=%t err=%v", available, err)
	}
}

// TestUpdateAvailableFromReportsUnparsableVersions 保证"问不到"与"确实没有新版"
// 是两条路：调用方要据此区分显示，这正是本项目"空列表三态"的同一条要求。
func TestUpdateAvailableFromReportsUnparsableVersions(t *testing.T) {
	if _, err := updateAvailableFrom("2.1.216", "not-a-version"); err == nil {
		t.Fatal("无法解析的远端版本必须返回错误，而不是静默当成'没有新版'")
	}
	if _, err := updateAvailableFrom("also-bad", "2.1.216"); err == nil {
		t.Fatal("无法解析的本地版本必须返回错误")
	}
}

func TestParseSemverRejectsLooseVersions(t *testing.T) {
	for _, raw := range []string{"1.2", "1", "1.2.3.4", "", "1.02.3", "v", "1.2.x"} {
		if _, err := parseSemver(raw); err == nil {
			t.Fatalf("parseSemver(%q) 应当被拒：宽松解析会让'段数'相关的判据静默偏移", raw)
		}
	}
	for _, raw := range []string{"v1.2.3", "1.2.3", "1.2.3-rc.1", "1.2.3+build.5", " 1.2.3  "} {
		if _, err := parseSemver(raw); err != nil {
			t.Fatalf("parseSemver(%q) 应当被接受，却报 %v", raw, err)
		}
	}
}

func TestRuntimeMeetsMinimumGatesOldRuntime(t *testing.T) {
	tests := []struct {
		name            string
		actual, minimum string
		want            bool
		wantErr         bool
	}{
		{name: "too old", actual: "16.20.2", minimum: "18.0.0", want: false},
		{name: "exactly minimum", actual: "18.0.0", minimum: "18.0.0", want: true},
		{name: "newer major", actual: "24.5.0", minimum: "18.0.0", want: true},
		{name: "newer minor", actual: "18.2.0", minimum: "18.1.0", want: true},
		{name: "no requirement", actual: "16.0.0", minimum: "", want: true},
		{name: "unparsable actual", actual: "v-not", minimum: "18.0.0", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := runtimeMeetsMinimum(test.actual, test.minimum)
			if test.wantErr {
				if err == nil {
					t.Fatal("期望报错却没有")
				}
				return
			}
			if err != nil {
				t.Fatalf("runtimeMeetsMinimum(%q, %q) 报错：%v", test.actual, test.minimum, err)
			}
			if got != test.want {
				t.Fatalf("runtimeMeetsMinimum(%q, %q) = %t，期望 %t", test.actual, test.minimum, got, test.want)
			}
		})
	}
}

// TestClaudeCheckUpdateUsesSemverComparison 是行为级回归：真的调一次 CheckUpdate，
// 让 registry 报出比本地更旧的版本，断言结论是"没有更新"。
//
// 为什么不用纯函数测：缺陷不在比较函数里（它一直是对的），而在 Claude 的调用点
// 用没用它。只测比较函数的话，把调用点改回字符串不等，测试照样全绿。
func TestClaudeCheckUpdateUsesSemverComparison(t *testing.T) {
	binDir := t.TempDir()
	writeFakeCommand(t, binDir, "claude", "2.1.216")
	writeFakeCommand(t, binDir, "npm", "1.0.0")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// paths 传 nil：本用例要验的是"调用点用没用 semver"，路径来源与它无关。
	runner := newClaudeCLIRunner(Config{
		ClaudePath:         filepath.Join(binDir, executableName("claude")),
		AgentUpdateTimeout: time.Minute,
	}, nil)
	if got := runner.Version(context.Background()); got != "2.1.216" {
		t.Fatalf("夹具没被用上：Version() = %q，期望 2.1.216", got)
	}
	available, latest, err := runner.CheckUpdate(context.Background())
	if err != nil {
		t.Fatalf("CheckUpdate 报错：%v", err)
	}
	if latest != "1.0.0" {
		t.Fatalf("latest = %q，期望夹具的 1.0.0（说明 npm 查询没走我们的替身）", latest)
	}
	if available {
		t.Fatal("registry 报 1.0.0、本地 2.1.216，CheckUpdate 却说有更新可用 —— 调用点没走 semver 比较")
	}
}

// TestClaudeCheckUpdateSourceDoesNotCompareVersionsAsStrings 是接线断言。
//
// 行为用例覆盖的是"当下这条路"，这条覆盖"写法本身"：只要有人把 `latest != local`
// 之类的字符串比较写回来就红。**必须剥掉注释** —— 注释里原样出现的写法会让这条
// 断言永远不可能红（本项目在别处踩过两次）。
func TestClaudeCheckUpdateSourceDoesNotCompareVersionsAsStrings(t *testing.T) {
	code := stripGoComments(t, readGoSource(t, "claude_runner.go"))
	if strings.Contains(code, "latest != local") {
		t.Fatal("claude_runner.go 里又出现了版本字符串不等比较，应改用 updateAvailableFrom")
	}
	if !strings.Contains(code, "updateAvailableFrom(local, latest)") {
		t.Fatal("claude_runner.go 的 CheckUpdate 没有调用 updateAvailableFrom")
	}
}

// TestAgentCheckUpdateQueriesPackageFromCatalog 断言两个工具的包名都来自目录：
// 直接改目录里的包名，行为要跟着变（证明没有第二份硬编码）。
func TestAgentCheckUpdateQueriesPackageFromCatalog(t *testing.T) {
	entry, ok := agentByID("claude-code")
	if !ok {
		t.Fatal("catalog is missing claude-code")
	}
	if entry.NpmPackage != "@anthropic-ai/claude-code" {
		t.Fatalf("claude-code 的 npm 包名被改成了 %q；若是有意变更请一并更新本条用例", entry.NpmPackage)
	}
	code := stripGoComments(t, readGoSource(t, "claude_runner.go"))
	if strings.Contains(code, `"@anthropic-ai/claude-code"`) {
		t.Fatal("claude_runner.go 里又出现了硬编码的 npm 包名，应走目录")
	}
	codexCode := stripGoComments(t, readGoSource(t, "codex_runner.go"))
	if strings.Contains(codexCode, `"@openai/codex"`) {
		t.Fatal("codex_runner.go 里又出现了硬编码的 npm 包名，应走目录")
	}
}

// ── 测试辅助 ────────────────────────────────────────────────────────────────

// readGoSource 读同包下的 Go 源码，供接线断言使用。
func readGoSource(t *testing.T, name string) string {
	t.Helper()
	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("无法定位测试文件位置")
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(current), name))
	if err != nil {
		t.Fatalf("读 %s 失败：%v", name, err)
	}
	return string(raw)
}

// stripGoComments 去掉行注释与块注释。
//
// 负向断言（"不该出现某个写法"）必须先剥注释：注释里往往正写着"为什么不要这么写"，
// 于是断言永远不可能红 —— 那样一条防线是空转的。
func stripGoComments(t *testing.T, source string) string {
	t.Helper()
	var out strings.Builder
	lines := strings.Split(source, "\n")
	inBlock := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if inBlock {
			if index := strings.Index(trimmed, "*/"); index >= 0 {
				inBlock = false
				trimmed = trimmed[index+2:]
			} else {
				continue
			}
		}
		if strings.HasPrefix(trimmed, "/*") {
			if index := strings.Index(trimmed, "*/"); index >= 0 {
				trimmed = trimmed[index+2:]
			} else {
				inBlock = true
				continue
			}
		}
		if index := strings.Index(trimmed, "//"); index >= 0 {
			trimmed = trimmed[:index]
		}
		out.WriteString(trimmed)
		out.WriteString("\n")
	}
	return out.String()
}

// executableName 给出某个裸命令在当前平台上的实际文件名。
func executableName(command string) string {
	if runtime.GOOS == "windows" {
		return command + ".cmd"
	}
	return command
}

// writeFakeCommand 在目录里放一个"固定输出"的假命令，用于隔离真实 CLI。
// 输出不带换行以外的任何东西，且不含括号等 batch 敏感字符。
func writeFakeCommand(t *testing.T, dir, command, output string) {
	t.Helper()
	path := filepath.Join(dir, executableName(command))
	var body string
	if runtime.GOOS == "windows" {
		body = "@echo off\r\necho " + output + "\r\n"
	} else {
		body = "#!/bin/sh\necho " + output + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("写假命令 %s 失败：%v", path, err)
	}
}

// TestAgentVersionExtractionHasExactlyOneImplementation 钉住"版本号提取只有一处"。
//
// 为什么值得一条**结构**断言：这件事曾经在 7 个地方各写了一遍 —— 本机 Claude/Codex、
// SSH 的 Claude/Codex、WSL 的 Claude/Codex，以及 agent_install.go 里那份。前六份各按
// 自己那侧的真实输出写对了，最后一份用目录里的单个字段 + TrimSuffix，于是对 Codex
// （产品名在**前**）静默失效：界面显示 0.155.1，登记表与审计里存 "codex-cli 0.155.1"。
//
// 单测抓不到它 —— 那些用例喂的都是编造的输出；只有真装一次才暴露。所以这里补一条
// 不依赖运行时的断言：产品名字面量不许出现在任何**生产代码**里（注释不算），
// 各 runner 必须统一调 agentVersionFromOutput。
func TestAgentVersionExtractionHasExactlyOneImplementation(t *testing.T) {
	// 产品名在两种输出里的形态。它们只允许作为**测试输入**存在。
	productNameLiterals := []string{" (Claude Code)", "codex-cli "}
	productionFiles := []string{
		"claude_runner.go", "codex_runner.go", "ssh_runner.go", "wsl_agent_runner.go",
		"agent_install.go", "agent_install_cross.go", "agent_target_env.go", "agent_catalog.go",
	}
	for _, name := range productionFiles {
		source := stripGoComments(t, readGoSource(t, name))
		for _, literal := range productNameLiterals {
			if strings.Contains(source, literal) {
				t.Errorf("%s 里出现了产品名字面量 %q —— 版本号提取必须走 agentVersionFromOutput；"+
					"按前后缀各剥一遍正是 Codex 那次静默失效的成因", name, literal)
			}
		}
	}

	// 反向断言：真正消费版本输出的文件必须调到那个唯一实现。
	// 否则"只有一处实现"很容易变成"一处都没有"。
	for _, name := range []string{
		"claude_runner.go", "codex_runner.go", "ssh_runner.go", "wsl_agent_runner.go",
		"agent_install.go", "agent_install_cross.go", "agent_target_env.go",
	} {
		if !strings.Contains(readGoSource(t, name), "agentVersionFromOutput(") {
			t.Errorf("%s 没有调用 agentVersionFromOutput —— 它应当归一化，而不是自己剥字符串", name)
		}
	}
}
