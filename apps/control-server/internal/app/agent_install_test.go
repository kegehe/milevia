package app

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// newInstallTestServer 建一个只带数据库与解析器的 Server，并把托管工具链
// 指向临时目录（AUTO_TOOLCHAIN_ROOT）—— 绝不碰用户的真实目录。
func newInstallTestServer(t *testing.T) *Server {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "install.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	server := &Server{
		db: db, paths: newAgentPathResolver(Config{}),
		runnerUpdating: map[runnerAgentKey]bool{},
	}
	if err := server.migrateAgentInstallations(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := server.migrateRunnerInstallGrants(context.Background()); err != nil {
		t.Fatal(err)
	}
	return server
}

// plantManagedToolchain 在临时工具链目录里放一套"看起来像托管 Node"的东西。
func plantManagedToolchain(t *testing.T, root string) {
	t.Helper()
	binary := managedNodeBinary(root)
	if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, binary, "24.21.0")
	npm := managedNpmCommand(root)
	if err := os.MkdirAll(filepath.Dir(npm), 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, npm, "11.0.0")
}

func TestResolveAgentInstallPlanPrefersManagedWhenNotYetInstalled(t *testing.T) {
	server := newInstallTestServer(t)
	root := t.TempDir()
	t.Setenv(toolchainRootEnv, root)
	plantManagedToolchain(t, root)

	plan, err := server.resolveAgentInstallPlan(context.Background(), server.localRunnerID(), "claude-code")
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	// 托管优先：系统 npm 的全局 prefix 在 Linux 上常不可写，装到那里会以 EACCES
	// 失败，而用户看不出该改什么。
	if plan.Kind != installKindNpmManaged {
		t.Fatalf("kind = %q，期望 %q", plan.Kind, installKindNpmManaged)
	}
	if plan.Prefix != managedNpmGlobalPrefix(root) {
		t.Fatalf("prefix = %q，期望 %q", plan.Prefix, managedNpmGlobalPrefix(root))
	}
	// 运行时版本要读得出来 —— 这依赖"夹具里的 node 真的能执行"。
	// Windows 上做不到（一个 .exe 必须是真正的 PE 文件，写不了假），所以按平台分流；
	// 版本闸门本身由 TestCheckRuntimeGateBlocksOldRuntime 在所有平台上守着。
	if runtime.GOOS != "windows" && plan.RuntimeVersion != "24.21.0" {
		t.Fatalf("运行时版本 = %q", plan.RuntimeVersion)
	}
}

// TestResolveAgentInstallPlanKeepsExistingKind 是本组最要紧的一条。
//
// 已经装在系统 npm 全局里的工具，升级时**必须继续用系统 npm**。若改用托管
// prefix，用户机器上就会出现两份 CLI，而"哪份在生效"取决于解析顺序 —— 那是
// 一个没人能看出原因的坑。
func TestResolveAgentInstallPlanKeepsExistingKind(t *testing.T) {
	server := newInstallTestServer(t)
	root := t.TempDir()
	t.Setenv(toolchainRootEnv, root)
	plantManagedToolchain(t, root)
	fakeSystemNpm(t, "24.21.0")

	runnerID := server.localRunnerID()
	if err := server.recordAgentInstallation(context.Background(), agentInstallation{
		RunnerID: runnerID, AgentID: "claude-code", BinaryPath: "/usr/local/bin/claude",
		InstallKind: installKindNpmSystem, Version: "2.0.0", Source: "discovered",
	}); err != nil {
		t.Fatal(err)
	}

	plan, err := server.resolveAgentInstallPlan(context.Background(), runnerID, "claude-code")
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if plan.Kind != installKindNpmSystem {
		t.Fatalf("kind = %q，期望继续沿用 %q（不能另造一份）", plan.Kind, installKindNpmSystem)
	}
	if plan.Prefix != "" {
		t.Fatalf("系统全局安装不该带 --prefix，却给了 %q", plan.Prefix)
	}
}

// fakeSystemNpm 造一个能应答 `node --version` 的"系统 npm 旁路"（PATH 上放 npm 与 node）。
func fakeSystemNpm(t *testing.T, nodeVersion string) {
	t.Helper()
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "npm"+exeSuffixForTest()), "11.0.0")
	writeExecutable(t, filepath.Join(dir, "node"+exeSuffixForTest()), nodeVersion)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func exeSuffixForTest() string {
	if runtime.GOOS == "windows" {
		return ".cmd"
	}
	return ""
}

func TestResolveAgentInstallPlanReportsMissingRuntime(t *testing.T) {
	server := newInstallTestServer(t)
	t.Setenv(toolchainRootEnv, t.TempDir()) // 空工具链
	// 把 PATH 清成空目录：既没有托管 npm，也没有系统 npm。
	t.Setenv("PATH", t.TempDir())

	_, err := server.resolveAgentInstallPlan(context.Background(), server.localRunnerID(), "claude-code")
	if err == nil {
		t.Fatal("两种运行时都没有时应当报错")
	}
	// 报错必须指向"去装 Node"，而不是笼统的失败。
	if !strings.Contains(err.Error(), "Node") {
		t.Fatalf("报错没有指向运行时：%v", err)
	}
}

func TestResolveAgentInstallPlanRejectsNativeInstall(t *testing.T) {
	server := newInstallTestServer(t)
	root := t.TempDir()
	t.Setenv(toolchainRootEnv, root)
	plantManagedToolchain(t, root)

	runnerID := server.localRunnerID()
	if err := server.recordAgentInstallation(context.Background(), agentInstallation{
		RunnerID: runnerID, AgentID: "claude-code", BinaryPath: "/home/u/.local/bin/claude",
		InstallKind: installKindNative, Version: "2.1.216", Source: "discovered",
	}); err != nil {
		t.Fatal(err)
	}
	_, err := server.resolveAgentInstallPlan(context.Background(), runnerID, "claude-code")
	if err == nil {
		t.Fatal("官方安装器装的工具不该被平台接管")
	}
	if !strings.Contains(err.Error(), "官方安装器") {
		t.Fatalf("报错没有说明原因：%v", err)
	}
}

// TestResolveAgentInstallPlanRejectsRemoteRunner 钉住"跨端不按本机方式安装"。
//
// 跨端有它自己的实现（installAgentCLICross）：npm 在哪、装到哪个 prefix 由**目标
// 环境**报出来，而本机这套默认（托管工具链目录 / 系统 PATH）在那边根本不成立。
// 所以把跨端 runner 传进本机计划必须报错 —— 静默按本机装会把工具装到控制服务
// 所在的这台机器上，而用户以为装到了远端。
func TestResolveAgentInstallPlanRejectsRemoteRunner(t *testing.T) {
	server := newInstallTestServer(t)
	_, err := server.resolveAgentInstallPlan(context.Background(), "ssh-prod", "claude-code")
	if err == nil {
		t.Fatal("跨端 runner 不该被按本机方式处理")
	}
	if !strings.Contains(err.Error(), "跨端") {
		t.Fatalf("报错没有说明这是跨端：%v", err)
	}
}

func TestCheckRuntimeGateBlocksOldRuntime(t *testing.T) {
	entry, ok := agentByID("claude-code")
	if !ok {
		t.Fatal("目录里没有 claude-code")
	}
	if err := checkRuntimeGate("16.20.2", entry); err == nil {
		t.Fatal("Node 16 应当被闸门拦住")
	} else if !strings.Contains(err.Error(), "过低") {
		t.Fatalf("报错没有说明是版本过低：%v", err)
	}
	if err := checkRuntimeGate("18.0.0", entry); err != nil {
		t.Fatalf("刚好满足最低版本时不该拦：%v", err)
	}
	// 读不到版本时放行（那是"探测失败"，不是"版本过低"——不能把读不到说成太旧）。
	if err := checkRuntimeGate("", entry); err != nil {
		t.Fatalf("探测不到运行时版本时不该直接拦下：%v", err)
	}
}

// TestInstallAgentCLIEndToEnd 用一个假 npm 走完整条安装链路。
//
// 假 npm 的行为：把 `install -g <pkg>@<ver>` 变成"在 prefix 下写出一个可执行的
// 假 CLI"。于是"装后自检真的执行了产物"这件事也被验到了 —— 只写文件而不执行，
// 是发现不了坏包的。
func TestInstallAgentCLIEndToEnd(t *testing.T) {
	server := newInstallTestServer(t)
	root := t.TempDir()
	t.Setenv(toolchainRootEnv, root)
	plantManagedToolchain(t, root)

	// 覆盖托管 npm：它要真的往托管 prefix 里写一个假 claude。
	//
	// 落点必须用 npmCLIInstall.commandPath 算 —— Windows 的 npm 全局 shim 直接
	// 放在 prefix 下（`<prefix>\claude.cmd`），不是 Unix 那样的 `bin/` 子目录。
	// 手写路径猜错的表现正是本用例第一版抓到的：自检找不到产物，回落到 PATH，
	// 于是把机器上真实存在的那份 claude 认成了我们的安装。
	npmPath := managedNpmCommand(root)
	prefix := managedNpmGlobalPrefix(root)
	install := npmCLIInstall{scope: "@anthropic-ai", packageName: "claude-code", commandName: "claude", binFile: "claude.exe"}
	target := install.commandPath(prefix)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	var script string
	if runtime.GOOS == "windows" {
		script = "@echo off\r\necho 2.1.217 (Claude Code)\r\n"
	} else {
		script = "#!/bin/sh\necho '2.1.217 (Claude Code)'\n"
	}
	if err := os.WriteFile(target, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// 让假 npm 无论收到什么参数都返回成功。
	writeExitZeroCommand(t, npmPath)

	installation, err := server.installAgentCLI(context.Background(), server.localRunnerID(), "claude-code", "")
	if err != nil {
		t.Fatalf("安装失败：%v", err)
	}
	if installation.Version != "2.1.217" {
		t.Fatalf("版本 = %q，期望自检读到的 2.1.217", installation.Version)
	}
	if installation.InstallKind != installKindNpmManaged {
		t.Fatalf("install_kind = %q", installation.InstallKind)
	}
	if installation.Prefix != prefix {
		t.Fatalf("prefix = %q，期望 %q（缺了它升级会去找错位置）", installation.Prefix, prefix)
	}
	// 登记之后解析器立刻能用（不必重启）。
	if got := server.paths.Path("claude-code"); got != target {
		t.Fatalf("解析器给出 %q，期望 %q", got, target)
	}
}

func writeExitZeroCommand(t *testing.T, path string) {
	t.Helper()
	var body string
	if runtime.GOOS == "windows" {
		body = "@echo off\r\nexit /b 0\r\n"
	} else {
		body = "#!/bin/sh\nexit 0\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestInstallAgentCLIRejectsMalformedVersion(t *testing.T) {
	server := newInstallTestServer(t)
	root := t.TempDir()
	t.Setenv(toolchainRootEnv, root)
	plantManagedToolchain(t, root)

	// 版本号会拼进命令参数，所以必须过白名单。`; rm -rf /` 这类要在这里被挡下。
	for _, bad := range []string{"1.2", "latest; rm -rf /", "$(whoami)", "1.2.3 && echo pwned"} {
		if _, err := server.installAgentCLI(context.Background(), server.localRunnerID(), "claude-code", bad); err == nil {
			t.Fatalf("非法版本号 %q 被接受了", bad)
		}
	}
}

func TestRecordInstallAuditRoundTrip(t *testing.T) {
	server := newInstallTestServer(t)
	ctx := context.Background()
	if err := server.recordInstallAudit(ctx, installAuditEntry{
		RunnerID: "ssh-prod", AgentID: "claude-code", Action: "install",
		FromVersion: "2.0.0", ToVersion: "2.1.217", Result: "succeeded",
	}); err != nil {
		t.Fatal(err)
	}
	items, err := server.listInstallAudit(ctx, "ssh-prod", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ToVersion != "2.1.217" || items[0].Result != "succeeded" {
		t.Fatalf("审计记录不完整：%#v", items)
	}
}

func TestRemoteInstallGrantIsPerHostAndRevocable(t *testing.T) {
	server := newInstallTestServer(t)
	ctx := context.Background()

	// 默认关。
	if server.remoteInstallAllowed(ctx, "ssh-prod") {
		t.Fatal("未授权的主机不该被当成已授权")
	}
	// 本机永远允许（那是用户自己的机器，且不涉及提权与远程执行）。
	if !server.remoteInstallAllowed(ctx, server.localRunnerID()) {
		t.Fatal("本机不该需要授权")
	}

	if _, err := server.db.ExecContext(ctx, `insert into runner_install_grants (runner_id,granted_at) values (?,?)`, "ssh-prod", "2026-09-21 00:00:00"); err != nil {
		t.Fatal(err)
	}
	if !server.remoteInstallAllowed(ctx, "ssh-prod") {
		t.Fatal("授权后应当放行")
	}
	// 逐主机：给 prod 授权不该顺带efault 把 staging 也放开。
	if server.remoteInstallAllowed(ctx, "ssh-staging") {
		t.Fatal("授权泄漏到了别的机器")
	}
	// 可撤销。
	if _, err := server.db.ExecContext(ctx, `delete from runner_install_grants where runner_id=?`, "ssh-prod"); err != nil {
		t.Fatal(err)
	}
	if server.remoteInstallAllowed(ctx, "ssh-prod") {
		t.Fatal("撤销后应当恢复为拒绝")
	}
}
