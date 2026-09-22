package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// 本轮复查抓到的 bug 的回归测试。
//
// 放在单独一个文件里，是因为这几条都是"实现看起来对、但实际会走到错误分支"的形态 ——
// 它们不测功能，测的是**判据落在哪一侧**。

// writeStubCommand 在 dir 下造一个"打印固定输出"的假命令（跨平台）。
func writeStubCommand(t *testing.T, dir, name, output string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	body := "#!/bin/sh\nprintf '%s\\n' '" + output + "'\n"
	if runtime.GOOS == "windows" {
		// Windows 上 `exec.Command("npm")` 靠 PATHEXT 找到 .cmd（默认 PATHEXT 含 .CMD）。
		path = filepath.Join(dir, name+".cmd")
		body = "@echo off\r\necho " + output + "\r\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestWSLClaudeCheckUpdateUsesSemverNotStringInequality 钉住"版本判据只有一条"。
//
// 字符串不等（`latest != local`）在本地装了**比 registry 更新的预发布版**时会把
// **降级**报成"有更新可用"，用户点下去就把自己降级了。Claude 侧原先只有本机修过，
// SSH 与 WSL 两处漏了 —— 同一台机器上 Claude 与 Codex 因此有两套判据。
func TestWSLClaudeCheckUpdateUsesSemverNotStringInequality(t *testing.T) {
	binDir := t.TempDir()
	writeStubCommand(t, binDir, "npm", "2.1.217")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	runner := newWSLAgentRunner(Config{ClaudePath: "claude"}, "Ubuntu", nil)
	// 注入假探测：WSL 内的本地版本是预发布的 2.1.218-beta.1，比 registry 的 2.1.217 新。
	runner.probeFn = func(context.Context, string) (string, error) { return "2.1.218-beta.1", nil }

	available, latest, err := runner.CheckUpdate(context.Background())
	if err != nil {
		t.Fatalf("检查更新失败：%v", err)
	}
	if latest != "2.1.217" {
		t.Fatalf("registry 最新版本 = %q", latest)
	}
	if available {
		t.Fatal("本地 2.1.218-beta.1 比 registry 的 2.1.217 新，不该报“有更新可用” —— 那是降级")
	}
}

// TestCrossRuntimeArchiveNeverLandsInStaging 钉住"上传的压缩包不能被自己的清理删掉"。
//
// 运行时安装脚本开头会 `rm -rf "$staging"` 清上次残留。若压缩包就落在 staging 里
// （SSH 那条"上传过去"的路按 targetDir 拼路径：`path.Join(staging, name)`），
// tar 拿到的是一分钟前被删掉的文件 —— 界面上却是一个"可以点"的按钮。
//
// 行为侧的断言在 TestInstallManagedRuntimeCrossUploadsWhenNotShared（那里会真的读
// 生成的脚本）；这条是源码级的，防止有人"顺手"把落点改回去。
func TestCrossRuntimeArchiveNeverLandsInStaging(t *testing.T) {
	source, err := os.ReadFile("runtime_install_cross.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(source), "stageArchive(ctx, env, localArchive, root)") {
		t.Fatal("压缩包必须落在 root：落进 staging 会被安装脚本开头的 rm -rf 删掉，SSH 上必然失败")
	}
}

// TestRecordedInstallationSeparatesFailureFromAbsent 钉住"读不到 ≠ 没有"。
//
// 原来的写法一律 `if err == nil`，于是 SQL 出错与 `sql.ErrNoRows` 落到同一个分支：
// 一份装在系统 npm 全局里的工具会被当成"没装过"，升级时改走托管 npm 另装一份 ——
// 用户机器上出现两份 CLI，而没人知道哪份在生效。
func TestRecordedInstallationSeparatesFailureFromAbsent(t *testing.T) {
	server := newInstallTestServer(t)
	ctx := context.Background()

	if _, has, err := server.recordedInstallation(ctx, "ssh-prod", "claude-code"); err != nil || has {
		t.Fatalf("没有记录时应当 has=false 且无错误：has=%v err=%v", has, err)
	}
	if err := server.recordAgentInstallation(ctx, agentInstallation{
		RunnerID: "ssh-prod", AgentID: "claude-code",
		BinaryPath: "/usr/local/bin/claude", InstallKind: installKindNpmSystem, Version: "2.1.217",
	}); err != nil {
		t.Fatal(err)
	}
	recorded, has, err := server.recordedInstallation(ctx, "ssh-prod", "claude-code")
	if err != nil || !has {
		t.Fatalf("有记录时应当 has=true：has=%v err=%v", has, err)
	}
	if recorded.InstallKind != installKindNpmSystem {
		t.Fatalf("installKind = %q", recorded.InstallKind)
	}

	// 数据库不可用：必须**报错**，不能退化成"没装过"。
	if err := server.db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := server.recordedInstallation(ctx, "ssh-prod", "claude-code"); err == nil {
		t.Fatal("读登记失败时必须如实报错 —— 退化成“没装过”会让升级另装一份")
	}
}

// TestRuntimeInstallSharesTheSameMaintenanceGate 钉住"同一台机器上只有一个安装任务"。
//
// 运行时安装原先自己占一个 runnerUpdating[{runnerID,"node"}] 槽，而 CLI 的安装/升级
// 走 beginAgentMaintenance（它的另一半是 runnerUpdateExecuting[runnerID]）—— 两把锁
// 互不相见，于是"装 Node"与"装 CLI"能同时跑：两个 npm 写同一个 prefix，而运行时的
// 到位动作（mv node）会把 CLI 正在用的 node 换掉。
func TestRuntimeInstallSharesTheSameMaintenanceGate(t *testing.T) {
	source, err := os.ReadFile("runtime_install.go")
	if err != nil {
		t.Fatal(err)
	}
	code := string(source)
	if !strings.Contains(code, "s.beginAgentMaintenance(w, r, runnerID, runtimeAgentID") {
		t.Fatal("运行时的安装必须与 CLI 的安装走同一把闸门")
	}
	if strings.Contains(code, "s.runnerUpdating[") {
		t.Fatal("不该再自己占 runnerUpdating 槽位 —— 那把闸门的另一半只有 beginAgentMaintenance 维护")
	}
}

// TestCrossAgentInstallScriptDefinesPrefixBeforeUsingIt 钉住脚本里的**求值顺序**。
//
// 脚本开头是 `set -eu`，而 install 那一行用 `--prefix "$prefix"`：先引用后赋值会让
// 脚本立刻以 "unbound variable" 退出 —— 症状只是"安装失败：exit status 1"，
// 完全指不到原因，而跨端安装**每一次**都会失败。
//
// 断言必须落在行序上：只断言"某两行都存在"对这个 bug 是空转的。
func TestCrossAgentInstallScriptDefinesPrefixBeforeUsingIt(t *testing.T) {
	for _, prefix := range []string{"/opt/npm-global", ""} {
		script := crossAgentInstallScript("/opt/npm/bin/npm", prefix, "@x/y@latest", "y")
		assign, use := -1, -1
		for index, line := range strings.Split(script, "\n") {
			if assign < 0 && strings.HasPrefix(line, "prefix=") {
				assign = index
			}
			if use < 0 && strings.Contains(line, "install -g") {
				use = index
			}
		}
		if assign < 0 || use < 0 {
			t.Fatalf("prefix 赋值或 install 行缺失（prefix=%q）：%s", prefix, script)
		}
		if assign > use {
			t.Fatalf("prefix 在第 %d 行赋值、第 %d 行就被引用；`set -eu` 下脚本会立刻退出（prefix=%q）",
				assign+1, use+1, prefix)
		}
	}
}

// TestRuntimeMeetsMinimumForFollowsInstallKind 钉住"够不够"按**安装方式**选套。
//
// 一台机器上可以同时有托管与系统两套运行时，而某个工具登记为系统 npm 装的 ——
// 它升级时用的是系统那套。界面若按"当前生效的那套"（托管优先）算，就会亮出一个
// 点了必失败的升级按钮。
func TestRuntimeMeetsMinimumForFollowsInstallKind(t *testing.T) {
	server := newCrossTestServer(t)
	ctx := context.Background()
	root := "/home/dev/.local/share/milevia/toolchain"
	// 托管 Node 24、系统 Node 14：两种登记方式下结论应当相反。
	status := runtimeStatus{
		ID: runtimeAgentID, Installed: true, Origin: "managed", Version: "24.21.0",
		NpmVersion: "11.0.0", SystemNodeVersion: "14.21.3", SystemNpmVersion: "6.14.18",
		SystemNpmPath: "/usr/bin/npm",
	}
	entry, ok := agentByID("claude-code")
	if !ok {
		t.Fatal("目录里没有 claude-code")
	}

	recorded, hasRecord, err := server.recordedInstallation(ctx, "wsl-local", entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !meetsMinimumByInstallKind(status, hasRecord, recorded, entry, root) {
		t.Fatal("没装过时按托管那套算，Node 24 应当够")
	}

	// 登记为系统 npm 装的 → 升级用的是系统 Node 14 → 不够。
	if err := server.recordAgentInstallation(ctx, agentInstallation{
		RunnerID: "wsl-local", AgentID: entry.ID,
		BinaryPath: "/usr/local/bin/claude", InstallKind: installKindNpmSystem, Version: "2.1.216",
	}); err != nil {
		t.Fatal(err)
	}
	recorded, hasRecord, err = server.recordedInstallation(ctx, "wsl-local", entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if meetsMinimumByInstallKind(status, hasRecord, recorded, entry, root) {
		t.Fatal("登记为系统 npm 装的工具升级用的是系统那套；Node 14 不够，界面不该给出升级按钮")
	}

	// 认不出的登记值不许猜（猜错会装到第二个位置）。
	if _, reason := resolveCrossInstallTarget(status, true, agentInstallation{InstallKind: "wat"}, root); reason == "" {
		t.Fatal("认不出的安装方式应当如实报错，而不是猜一个")
	}
}

// TestRuntimeInstallWritesAuditEvenOnFailure 钉住"装运行时也留痕"。
//
// 往目标环境落一整套运行时是这条链路里最重的动作。原先它**一条审计都不写**——
// 而"逐主机授权"那一套设计（docs/42 §9.1）要求的三件事里就有审计。
func TestRuntimeInstallWritesAuditEvenOnFailure(t *testing.T) {
	server := newTestServer(t)
	ctx := context.Background()
	if _, err := server.db.ExecContext(ctx,
		`insert into runner_install_grants (runner_id,granted_at) values (?,?)`, "ssh-prod", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	// 这个 runner 没有注册（跨端通道取不到）→ 安装会失败，但审计必须留下。
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/runners/ssh-prod/runtime/install", strings.NewReader("{}")))
	if rec.Code == http.StatusOK {
		t.Fatalf("没有跨端通道时不该成功：body=%s", rec.Body.String())
	}

	items, err := server.listInstallAudit(ctx, "ssh-prod", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Action != "install-runtime" {
			continue
		}
		if item.Result != "failed" {
			t.Fatalf("这次安装失败了，审计应当记 failed：%+v", item)
		}
		return
	}
	t.Fatal("装运行时没有留下任何审计记录")
}
