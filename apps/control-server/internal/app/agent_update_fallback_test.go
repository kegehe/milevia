package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// 这一组钉住的是"CLI 自带的 update 失败之后，平台自己接手原地重装"。
//
// 真机背景（2026-09，Claude Code 2.1.266 + Node 22.15.0，Windows）：npm 全局装的
// CLI 走 `claude update` 是**必然**失败的 —— CLI 用 child_process.spawn 直接拉起
// `npm.cmd` 而没有 shell，Node ≥ 22 拒绝执行（EINVAL），于是它把一次注定失败的
// 进程启动报成 "npm registry is unreachable"。详见 agent_update_fallback.go。

// plantNpmGlobalCLI 在 prefix 下摆出"npm 全局安装该有的样子"，返回那个命令的路径。
//
// 两处平台差异都是**跟着生产代码走**的，不是随手写的：
//   - 命令落点用 npmCLIInstall.commandPath：Windows 的 shim 直接在 prefix 下
//     （`<prefix>\claude.cmd`），Unix 在 `bin/` 里；
//   - Unix 的 shim 是指向包内产物的**符号链接**（verifyNpmCLICommand 会把两边
//     EvalSymlinks 之后再比），Windows 的 shim 本身就是被比对的 .cmd。
func plantNpmGlobalCLI(t *testing.T, entry AgentCatalogEntry, prefix, version string) string {
	t.Helper()
	install := agentNpmCLIInstall(entry)
	binary := install.binaryPath(prefix)
	if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, binary, version)

	command := install.commandPath(prefix)
	if err := os.MkdirAll(filepath.Dir(command), 0o755); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		writeExecutable(t, command, version)
	} else if err := os.Symlink(binary, command); err != nil {
		t.Fatal(err)
	}
	return command
}

// fakeNpmAnsweringPrefix 在 PATH 上放一个假 npm：把收到的参数追加到返回的日志文件，
// 并回出 prefix。
//
// 为什么"任何调用"都回 prefix：`npm prefix -g` 靠它拿 prefix；`npm install -g …` 的
// 输出没人读 —— 装没装上由装后自检真的执行一次产物来判（见 verifyAgentInstall）。
// 参数落日志是为了钉住"升的是哪个包、装到哪个 prefix"：只看结果的话，一个装错包
// 或装错位置的实现照样能通过。
func fakeNpmAnsweringPrefix(t *testing.T, prefix string) string {
	t.Helper()
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "npm-args.log")
	npm := filepath.Join(dir, "npm"+exeSuffixForTest())
	var body string
	if runtime.GOOS == "windows" {
		body = "@echo off\r\necho %* >> \"" + argsLog + "\"\r\necho " + prefix + "\r\n"
	} else {
		body = "#!/bin/sh\necho \"$@\" >> '" + argsLog + "'\necho '" + prefix + "'\n"
	}
	if err := os.WriteFile(npm, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	// 运行时闸门要读 node 的版本；nodeVersionNearNpm 先看 npm 同目录再看 PATH。
	writeExecutable(t, filepath.Join(dir, "node"+exeSuffixForTest()), "24.21.0")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argsLog
}

// TestPerformAgentUpdateRepairsNpmGlobalInstallWhenCLIUpdateFails 是本组的主用例。
//
// 它同时钉住三件事：① CLI 的 update 失败不再等于升级失败；② 重装落在**原来那个**
// prefix 上（不另造一份）；③ 装完登记下来 —— 这正是"下一次不必再撞 CLI 的坑"的凭据。
func TestPerformAgentUpdateRepairsNpmGlobalInstallWhenCLIUpdateFails(t *testing.T) {
	server := newInstallTestServer(t)
	ctx := context.Background()
	// 空工具链：托管工具链存在时"没登记过"的默认计划会优先装到托管 prefix，
	// 而原地修复**必须**装到用户机器上那一份。这条断言就是用来抓那个错的。
	t.Setenv(toolchainRootEnv, t.TempDir())

	entry, ok := agentByID("claude-code")
	if !ok {
		t.Fatal("目录里没有 claude-code")
	}
	prefix := t.TempDir()
	command := plantNpmGlobalCLI(t, entry, prefix, "2.1.217")
	argsLog := fakeNpmAnsweringPrefix(t, prefix)
	// 只告诉路径解析器"这份 claude 在哪"，**不写登记表** —— 用户自己装的正是这一档。
	server.paths.Remember("claude-code", command)

	runner := &updateTestRunner{updateErr: errors.New(
		"update Claude Code 失败：exit status 1（  • npm registry is unreachable   • Corporate proxy/firewall blocking npm）")}
	previous, current, err := server.performAgentUpdate(ctx, server.localRunnerID(), "claude-code", runner)
	if err != nil {
		t.Fatalf("CLI 的 update 失败后应当由平台接手重装，却报错：%v", err)
	}
	if runner.updateCalls != 1 {
		t.Fatalf("CLI 的 update 被调了 %d 次，期望 1 次（先试 CLI，失败才接手）", runner.updateCalls)
	}
	if previous != "2.1.216" || current != "2.1.217" {
		t.Fatalf("版本 %q → %q，期望 2.1.216 → 2.1.217", previous, current)
	}

	// 光看"装成功了"不够：装错包、或者装到别的 prefix 上去，上面每一句都照样成立。
	rawArgs, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatalf("假 npm 没被调用过：%v", err)
	}
	for _, want := range []string{"install", "-g", entry.NpmPackage + "@latest", "--prefix", prefix} {
		if !strings.Contains(string(rawArgs), want) {
			t.Fatalf("npm 收到的参数里没有 %q：\n%s", want, rawArgs)
		}
	}

	recorded, hasRecord, err := server.recordedInstallation(ctx, server.localRunnerID(), "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if !hasRecord {
		t.Fatal("修复之后没有登记 —— 下一次升级会再撞一遍 CLI 的坑")
	}
	if recorded.InstallKind != installKindNpmSystem {
		t.Fatalf("install_kind = %q，期望 %q", recorded.InstallKind, installKindNpmSystem)
	}
	if recorded.Source != "npm-repair" {
		t.Fatalf("source = %q，期望 %q（审计里要能看出这份是修复出来的）", recorded.Source, "npm-repair")
	}
	if recorded.Version != "2.1.217" {
		t.Fatalf("登记版本 = %q，期望 2.1.217", recorded.Version)
	}
	if !sameCleanPath(recorded.Prefix, prefix) {
		t.Fatalf("登记 prefix = %q，期望 %q", recorded.Prefix, prefix)
	}
}

// TestPerformAgentUpdateRepairsManagedInstallInPlace 钉住"托管 prefix 里的那份也原地升"。
//
// 与上一条的差别只在 prefix 落在哪：托管工具链里的那份，prefix 必须原样用回它自己，
// 而不是被 existingNpmInstallPlan 认成系统 npm 全局（那会去动另一个位置）。
func TestPerformAgentUpdateRepairsManagedInstallInPlace(t *testing.T) {
	server := newInstallTestServer(t)
	ctx := context.Background()
	root := t.TempDir()
	t.Setenv(toolchainRootEnv, root)
	plantManagedToolchain(t, root)

	entry, ok := agentByID("claude-code")
	if !ok {
		t.Fatal("目录里没有 claude-code")
	}
	prefix := managedNpmGlobalPrefix(root)
	command := plantNpmGlobalCLI(t, entry, prefix, "2.1.218")
	// 托管 npm 既当"回 prefix 的假 npm"，也是计划里要用的那个 npm。
	writeExecutable(t, managedNpmCommand(root), prefix)
	// PATH 上再放一个假 npm：**不能让机器上真的那份 npm 可达**。否则一旦
	// existingNpmInstallPlan 把这份认成系统 npm 全局，用例会去网上真装一遍，
	// 而且因为产物是预先摆好的，它照样会通过 —— 一个既慢又验不出东西的用例。
	argsLog := fakeNpmAnsweringPrefix(t, t.TempDir())
	server.paths.Remember("claude-code", command)

	runner := &updateTestRunner{updateErr: errors.New("update Claude Code 失败：exit status 1")}
	_, current, err := server.performAgentUpdate(ctx, server.localRunnerID(), "claude-code", runner)
	if err != nil {
		t.Fatalf("托管安装的修复失败：%v", err)
	}
	if current != "2.1.218" {
		t.Fatalf("版本 = %q，期望 2.1.218", current)
	}
	// 装的那一步必须落在**托管 npm** 上：PATH 上那份只该被问过 `prefix -g`。
	if raw, err := os.ReadFile(argsLog); err == nil && strings.Contains(string(raw), "install") {
		t.Fatalf("用了 PATH 上的 npm 而不是托管 npm：\n%s", raw)
	}
	recorded, hasRecord, err := server.recordedInstallation(ctx, server.localRunnerID(), "claude-code")
	if err != nil || !hasRecord {
		t.Fatalf("修复后没有登记：%v", err)
	}
	if recorded.InstallKind != installKindNpmManaged {
		t.Fatalf("install_kind = %q，期望 %q", recorded.InstallKind, installKindNpmManaged)
	}
	if !sameCleanPath(recorded.Prefix, prefix) {
		t.Fatalf("登记 prefix = %q，期望 %q", recorded.Prefix, prefix)
	}
}

// TestPerformAgentUpdateRefusesRepairForNonNpmInstall 是这一组的**反向**用例。
//
// 用户用 pnpm / bun / 官方安装器装的那一份，平台既不知道它怎么升，也不该升：
// 硬来会在用户机器上留下第二份 CLI，而"哪份在生效"取决于解析顺序 —— 没人看得出
// 原因。所以这一档必须原样报出 CLI 自己的错，且**一个字节都不许动**。
func TestPerformAgentUpdateRefusesRepairForNonNpmInstall(t *testing.T) {
	server := newInstallTestServer(t)
	ctx := context.Background()
	t.Setenv(toolchainRootEnv, t.TempDir())

	// 一个不在任何 npm 全局 prefix 里的 claude（模拟官方安装器那份）。
	dir := t.TempDir()
	command := filepath.Join(dir, "claude"+exeSuffixForTest())
	writeExecutable(t, command, "2.1.216")
	fakeNpmAnsweringPrefix(t, t.TempDir()) // 有一个 npm，但它并不提供这个命令
	server.paths.Remember("claude-code", command)

	cliErr := errors.New("update Claude Code 失败：exit status 1（安装位置不是 npm 全局包）")
	runner := &updateTestRunner{updateErr: cliErr}
	previous, current, err := server.performAgentUpdate(ctx, server.localRunnerID(), "claude-code", runner)
	if err == nil {
		t.Fatal("来源确认不了时不该接管升级")
	}
	if previous != "2.1.216" || current != "" {
		t.Fatalf("失败时应报 %q/空，得到 %q/%q", "2.1.216", previous, current)
	}
	// CLI 自己的错必须原样带出来 —— 那是用户唯一能拿去查的证据。
	if !strings.Contains(err.Error(), cliErr.Error()) {
		t.Fatalf("报错丢掉了 CLI 的原文：%v", err)
	}
	if !strings.Contains(err.Error(), "不在任何 npm 全局 prefix 下") {
		t.Fatalf("报错没有说明为什么没有接手：%v", err)
	}
	if _, hasRecord, _ := server.recordedInstallation(ctx, server.localRunnerID(), "claude-code"); hasRecord {
		t.Fatal("拒绝接管却留下了登记")
	}
}

// TestRepairAgentViaNpmRejectsRemoteRunner 钉住"跨端不走本机这套"。
//
// 跨端的 npm 在哪、装到哪个 prefix 由目标环境自己报（installAgentCLICross）；
// 拿本机的 PATH / 托管工具链去猜，会把工具装到控制服务所在的这台机器上，
// 而用户以为升的是远端那份。
func TestRepairAgentViaNpmRejectsRemoteRunner(t *testing.T) {
	server := newInstallTestServer(t)
	_, err := server.repairAgentViaNpm(context.Background(), "ssh-prod", "claude-code")
	if err == nil {
		t.Fatal("跨端 runner 不该被按本机方式修复")
	}
	if !strings.Contains(err.Error(), "跨端") {
		t.Fatalf("报错没有说明这是跨端：%v", err)
	}
}
