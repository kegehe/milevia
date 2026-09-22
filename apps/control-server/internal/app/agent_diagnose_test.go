package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

// ── 夹具 ────────────────────────────────────────────────────────────────────
//
// 磁盘形状一律用**现有实现产出的真实命名**造，不照印象编：
//   - 生效包：`<prefix>/node_modules/<scope>/<name>/`（npmCLIInstall.packageRoot）
//   - 旧包备份：`<prefix>/node_modules/<scope>/.<name>-<版本>`（npm_cli_install.go:122 的查找前缀）
//   - 残骸：`<prefix>/node_modules/<scope>/.<name>-interrupted-<nano>`（同文件 :144 的产出）

// newDiagnoseTestServer 在安装夹具的基础上补上诊断/修复要用的东西。
//
// 刻意**不注册任何 runner**：诊断路径不依赖注册表（unsupportedReason 为空时直接继续），
// 而注册 runner 会拉起 WSL 补注册探测 —— 在沙箱里那会中止整条命令（见 TOOLING.md）。
func newDiagnoseTestServer(t *testing.T) *Server {
	t.Helper()
	server := newInstallTestServer(t)
	server.runnerRegistry = newRunnerRegistry()
	server.runtimeCtx = context.Background()
	// 修复/安装要过 beginAgentMaintenance：它占两个 map（nil 会 panic）并查 runs 表
	// 判断有没有活跃会话（表不存在会变成一句无关的 SQL 报错）。
	server.runnerUpdateExecuting = map[string]bool{}
	server.runnerMaintenanceMu = sync.Mutex{}
	if _, err := server.db.Exec(`create table if not exists runs (
		id integer primary key autoincrement,
		agent_runtime_id text not null default '',
		agent_id text not null default '',
		status text not null default '')`); err != nil {
		t.Fatal(err)
	}
	return server
}

func diagnoseLocalMeta(server *Server) RunnerMeta {
	return RunnerMeta{ID: server.localRunnerID(), Name: "本机", Environment: "local"}
}

type agentFixtureOptions struct {
	// SkipShim 不写命令入口（模拟入口被删/被清）。
	SkipShim bool
	// BrokenCommand 让命令入口"存在但执行失败"（半装最典型的样子）。
	BrokenCommand bool
	// SkipPackage 不写包目录（模拟登记了但产物整个不见了）。
	SkipPackage bool
	// BackupVersion 非空时额外造一份完整的旧包（可回滚）。
	BackupVersion string
	// Interrupted 额外造几份残骸目录。
	Interrupted int
}

// plantAgentFixture 在 prefix 下造出一份"看起来像平台的托管安装"的东西。
func plantAgentFixture(t *testing.T, prefix string, entry AgentCatalogEntry, version string, opts agentFixtureOptions) {
	t.Helper()
	install := agentNpmCLIInstall(entry)
	packageDir := filepath.Join(install.packageRoot(prefix), install.packageName)
	if !opts.SkipPackage {
		writePackageJSON(t, packageDir, version)
		// 包内的可执行文件在 `bin/` 下 —— 那个子目录要自己建
		// （writePackageJSON 只保证包目录本身存在）。
		binDir := filepath.Join(packageDir, "bin")
		if err := os.MkdirAll(binDir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeExecutable(t, filepath.Join(binDir, install.binFile), version)
	}
	if !opts.SkipShim {
		shim := install.commandPath(prefix)
		if err := os.MkdirAll(filepath.Dir(shim), 0o755); err != nil {
			t.Fatal(err)
		}
		if opts.BrokenCommand {
			writeFailingCommand(t, shim)
		} else {
			writeExecutable(t, shim, version)
		}
	}
	if opts.BackupVersion != "" {
		backup := filepath.Join(install.packageRoot(prefix), "."+install.packageName+"-"+opts.BackupVersion)
		writePackageJSON(t, backup, opts.BackupVersion)
		if err := os.MkdirAll(filepath.Join(backup, "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeExecutable(t, filepath.Join(backup, "bin", install.binFile), opts.BackupVersion)
	}
	for index := 0; index < opts.Interrupted; index++ {
		dir := filepath.Join(install.packageRoot(prefix),
			fmt.Sprintf(".%s-interrupted-%d", install.packageName, index+1))
		writePackageJSON(t, dir, "0.0.0")
	}
}

func writePackageJSON(t *testing.T, dir, version string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"name":"fixture","version":%q}`, version)
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeFailingCommand 写一个"执行必失败"的命令（模拟半装产物）。
func writeFailingCommand(t *testing.T, path string) {
	t.Helper()
	body := "#!/bin/sh\nexit 1\n"
	if runtime.GOOS == "windows" {
		body = "@echo off\r\nexit /b 1\r\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// writeSleepingCommand 写一个"不回答"的命令（用来逼出超时那条分支）。
// 睡 10 秒是**故意留着余量**：即使平台没能及时收掉进程树，一个用例也不会拖太久。
func writeSleepingCommand(t *testing.T, path string) {
	t.Helper()
	body := "#!/bin/sh\nsleep 10\n"
	if runtime.GOOS == "windows" {
		body = "@echo off\r\nping -n 11 127.0.0.1 >nul\r\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// isolateAgentLookups 把"这台机器上还有没有别的 claude / node"清干净。
//
// **必须的**：开发机上通常真的装着全局 claude（`%APPDATA%\npm\claude.cmd`），
// 而 platformFallbackCandidates 会如实把它找出来 —— 那会让"什么都没装"这类断言
// 变成一句环境依赖的谎话（实测过：不加这一句，not-installed 会被判成 ok）。
func isolateAgentLookups(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	t.Setenv("APPDATA", t.TempDir())
}

// treeSnapshot 是"目录里有些什么、各自多大、什么时候改的"。
//
// ⚠️ **目录只记名字与模式，不记 mtime**：Windows 对目录时间戳是**延迟写**的
// （NTFS 会把目录元数据缓一阵再落盘），于是"前后两次 stat 同一个没动过的目录"
// 也可能读到不同的值 —— 实测就是它让"诊断不改磁盘"这条断言假红。
// 文件那一档照记 size+mtime：那才是"有人写过东西"的证据。
type treeSnapshot map[string]string

func snapshotTree(t *testing.T, root string) treeSnapshot {
	t.Helper()
	out := treeSnapshot{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		if entry.IsDir() {
			out[relative+string(filepath.Separator)] = "dir|" + info.Mode().String()
			return nil
		}
		out[relative] = fmt.Sprintf("%d|%d|%s", info.Size(), info.ModTime().UnixNano(), info.Mode())
		return nil
	})
	if err != nil {
		t.Fatalf("快照 %s 失败：%v", root, err)
	}
	return out
}

// requireTreeUnchanged 比对两份快照，并把**具体差在哪**打出来 —— 只说"变了"
// 会让下一次失败又要从头查一遍。
func requireTreeUnchanged(t *testing.T, before, after treeSnapshot, what string) {
	t.Helper()
	problems := []string{}
	for path, fingerprint := range before {
		other, ok := after[path]
		switch {
		case !ok:
			problems = append(problems, "被删掉："+path)
		case other != fingerprint:
			problems = append(problems, fmt.Sprintf("被改动：%s（%s → %s）", path, fingerprint, other))
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			problems = append(problems, "被新建："+path)
		}
	}
	if len(problems) > 0 {
		t.Fatalf("%s 改动了磁盘，%d 处：\n%s", what, len(problems), strings.Join(problems, "\n"))
	}
}

// ── 工具 ────────────────────────────────────────────────────────────────────

func diagnoseIssueByCode(report agentDiagnosis, code string) (diagnoseIssue, bool) {
	for _, issue := range report.Issues {
		if issue.Code == code {
			return issue, true
		}
	}
	return diagnoseIssue{}, false
}

func requireIssue(t *testing.T, report agentDiagnosis, code string) diagnoseIssue {
	t.Helper()
	issue, ok := diagnoseIssueByCode(report, code)
	if !ok {
		codes := []string{}
		for _, item := range report.Issues {
			codes = append(codes, item.Code)
		}
		t.Fatalf("诊断里没有 %s。实际症状：%v（status=%s）", code, codes, report.Status)
	}
	if len(issue.Evidence) == 0 {
		t.Fatalf("%s 没有给出任何证据 —— 那样用户只能猜", code)
	}
	return issue
}

func requireNoIssue(t *testing.T, report agentDiagnosis, code string) {
	t.Helper()
	if _, ok := diagnoseIssueByCode(report, code); ok {
		t.Fatalf("不该出现 %s（status=%s，症状 %d 条）", code, report.Status, len(report.Issues))
	}
}

func requireRemedy(t *testing.T, issue diagnoseIssue, remedy string) {
	t.Helper()
	for _, candidate := range issue.Remedies {
		if candidate.ID == remedy {
			return
		}
	}
	ids := []string{}
	for _, item := range issue.Remedies {
		ids = append(ids, item.ID)
	}
	t.Fatalf("%s 没有给出修复动作 %s（实际：%v）", issue.Code, remedy, ids)
}

// ── 用例 ────────────────────────────────────────────────────────────────────

// TestDiagnoseLocalIsReadOnly 是本组最要紧的一条。
//
// 诊断必须**一个字节都不改**：它由列表页自动触发，发生在任何授权之前。
// docs/42 §18.1 就是为了这件事去掉过一次"mkdir 探可写性"。
func TestDiagnoseLocalIsReadOnly(t *testing.T) {
	server := newDiagnoseTestServer(t)
	root := t.TempDir()
	t.Setenv(toolchainRootEnv, root)
	plantManagedToolchain(t, root)

	entry := mustAgent(t, "claude-code")
	prefix := managedNpmGlobalPrefix(root)
	plantAgentFixture(t, prefix, entry, "2.1.216", agentFixtureOptions{
		BackupVersion: "2.1.215",
		Interrupted:   2,
	})
	if err := server.recordAgentInstallation(context.Background(), agentInstallation{
		RunnerID: server.localRunnerID(), AgentID: entry.ID,
		BinaryPath:  agentNpmCLIInstall(entry).commandPath(prefix),
		InstallKind: installKindNpmManaged, Prefix: prefix, Version: "2.1.216", Source: "managed-install",
	}); err != nil {
		t.Fatal(err)
	}

	before := snapshotTree(t, root)
	report := server.buildLocalAgentDiagnosis(context.Background(), diagnoseLocalMeta(server), entry)
	requireTreeUnchanged(t, before, snapshotTree(t, root), "诊断")
	// 顺带确认它真的看进去了（否则"没改动"可能是因为它什么都没做）。
	if report.Version != "2.1.216" {
		t.Fatalf("实测版本 = %q，期望夹具里那份的 2.1.216", report.Version)
	}
	for _, code := range []string{
		issueBinaryBroken, issueBinaryMissing, issueShimMissing,
		issueShimDangling, issueActivePackageBroken, issuePackageScanFailed,
	} {
		requireNoIssue(t, report, code)
	}
	// 注：夹具**故意**放了 2 份残骸与一份版本对不上的备份，所以
	// package-interrupted 会出现（正确），package-backup-available 不该出现（版本对不上）。
	requireNoIssue(t, report, issuePackageBackup)
	// ⚠️ 这里**不**断言 status == ok：夹具里的"托管 node"在 Windows 上只是一个批处理
	// 文件（写不出真的 PE），probeRuntime 因此读不出版本、会报 runtime-broken。
	// 那是夹具的限制，不是被测行为 —— 所以只钉"安装完整性"那几个码。
}

// TestDiagnoseClassifiesHalfInstalled 半装：文件在、执行不起来。
//
// 这一档原先与"真的没装"共用 `installed=false`，于是界面只说"未安装"，
// 而正确的下一步是"重装/回滚"，不是"去装一个"。
func TestDiagnoseClassifiesHalfInstalled(t *testing.T) {
	server := newDiagnoseTestServer(t)
	root := t.TempDir()
	t.Setenv(toolchainRootEnv, root)
	plantManagedToolchain(t, root)

	entry := mustAgent(t, "claude-code")
	prefix := managedNpmGlobalPrefix(root)
	plantAgentFixture(t, prefix, entry, "2.1.216", agentFixtureOptions{BrokenCommand: true})
	if err := server.recordAgentInstallation(context.Background(), agentInstallation{
		RunnerID: server.localRunnerID(), AgentID: entry.ID,
		BinaryPath:  agentNpmCLIInstall(entry).commandPath(prefix),
		InstallKind: installKindNpmManaged, Prefix: prefix, Version: "2.1.216",
	}); err != nil {
		t.Fatal(err)
	}

	report := server.buildLocalAgentDiagnosis(context.Background(), diagnoseLocalMeta(server), entry)
	if report.Status != diagnosisBroken {
		t.Fatalf("半装应当判 broken，得到 %s", report.Status)
	}
	issue := requireIssue(t, report, issueBinaryBroken)
	if issue.Severity != severityBlocker {
		t.Fatalf("半装是 blocker，得到 %s", issue.Severity)
	}
	requireRemedy(t, issue, remedyReinstall)
}

// TestDiagnoseSeparatesTimeoutFromBroken 钉住"超时"与"执行失败"是两个码。
//
// 两者的下一步动作不同（再试一次可能就好 vs 必须重装），而原先它们都被压成
// "未安装"。这里直接喂合成探针给 assessResolvedPath，避免真的等 8 秒。
func TestDiagnoseSeparatesTimeoutFromBroken(t *testing.T) {
	server := newDiagnoseTestServer(t)
	entry := mustAgent(t, "claude-code")
	live := filepath.Join(t.TempDir(), "claude"+executableSuffix())
	writeExecutable(t, live, "2.1.216")

	timeoutReport := agentDiagnosis{Issues: []diagnoseIssue{}, Paths: []diagnosePathFact{}}
	server.assessResolvedPath(&timeoutReport, entry, live, diagnosePathFact{Path: live, Checked: true, Exists: true, Probed: true},
		true, pathProbe{Path: live, Exists: true, TimedOut: true}, true, false, agentInstallation{})
	timeoutIssue := requireIssue(t, timeoutReport, issueProbeTimeout)

	brokenReport := agentDiagnosis{Issues: []diagnoseIssue{}, Paths: []diagnosePathFact{}}
	server.assessResolvedPath(&brokenReport, entry, live, diagnosePathFact{Path: live, Checked: true, Exists: true, Probed: true},
		true, pathProbe{Path: live, Exists: true, Detail: "（退出码 1）"}, true, false, agentInstallation{})
	brokenIssue := requireIssue(t, brokenReport, issueBinaryBroken)

	if timeoutIssue.Code == brokenIssue.Code {
		t.Fatal("超时与执行失败必须是两个码")
	}
	// 超时那一档**不能**给出"重装"：它可能只是启动慢，重装是白下载一整个包。
	if len(timeoutIssue.Remedies) != 0 {
		t.Fatalf("超时不该给出修复动作，得到 %v", timeoutIssue.Remedies)
	}
	requireRemedy(t, brokenIssue, remedyReinstall)
}

// TestProbeExecutableDetectsTimeout 确认"超时"这件事真的测得出来
// —— 上面那条测的是分类逻辑，这条测的是读数本身。
func TestProbeExecutableDetectsTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slow"+executableSuffix())
	writeSleepingCommand(t, path)

	probe := probeExecutableWithin(context.Background(), path, []string{"--version"}, 300*time.Millisecond)
	if !probe.TimedOut {
		t.Fatalf("应当判为超时：%#v", probe)
	}
	if probe.Works {
		t.Fatal("超时的命令不该被当成可用")
	}
}

// TestProbeExecutableDetectsCancellation 确认"被中断"这件事真的测得出来。
//
// 与上一条**成对**：那条测的是分类逻辑（喂一个合成探针给 assessResolvedPath），
// 这条测的是读数本身。缺了这条会怎样 —— 实测过：把 `probe.Canceled` 写死成 false，
// 上面那条照样绿（合成探针直接构造 Canceled=true，根本不经过读数）。
// "分类对"与"读数对"是两件事，必须各有一条。
func TestProbeExecutableDetectsCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slow"+executableSuffix())
	writeSleepingCommand(t, path)

	parent, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	defer cancel()

	probe := probeExecutableWithin(parent, path, []string{"--version"}, 8*time.Second)
	if !probe.Canceled {
		t.Fatalf("父上下文被取消时应当判为 Canceled：%#v", probe)
	}
	if probe.TimedOut {
		t.Fatal("「被中断」与「太慢」是两件事，不能混说")
	}
	if probe.Works {
		t.Fatal("中断的探测不该被当成可用")
	}
}

// TestDiagnoseFindsBackupOnlyWhenVersionMatches 只有版本与登记一致时才算"可回滚"。
//
// 与 rollbackInterruptedNpmInstall 的判据一致（它也只认 previous 那一个版本）。
// 把一份**别的**版本的备份恢复过去等于悄悄降级，那比"修不好"更坏。
func TestDiagnoseFindsBackupOnlyWhenVersionMatches(t *testing.T) {
	server := newDiagnoseTestServer(t)
	root := t.TempDir()
	t.Setenv(toolchainRootEnv, root)
	plantManagedToolchain(t, root)

	entry := mustAgent(t, "claude-code")
	prefix := managedNpmGlobalPrefix(root)
	plantAgentFixture(t, prefix, entry, "2.1.216", agentFixtureOptions{
		BackupVersion: "2.1.216",
		Interrupted:   1,
	})
	install := agentNpmCLIInstall(entry)
	// 再放一份**版本对不上**的备份：它不该被当成可回滚的目标。
	different := filepath.Join(install.packageRoot(prefix), "."+install.packageName+"-9.9.9")
	writePackageJSON(t, different, "9.9.9")
	if err := os.MkdirAll(filepath.Join(different, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(different, "bin", install.binFile), "9.9.9")

	if err := server.recordAgentInstallation(context.Background(), agentInstallation{
		RunnerID: server.localRunnerID(), AgentID: entry.ID,
		BinaryPath:  install.commandPath(prefix),
		InstallKind: installKindNpmManaged, Prefix: prefix, Version: "2.1.216",
	}); err != nil {
		t.Fatal(err)
	}

	report := server.buildLocalAgentDiagnosis(context.Background(), diagnoseLocalMeta(server), entry)
	backupIssue := requireIssue(t, report, issuePackageBackup)
	requireRemedy(t, backupIssue, remedyRestoreBackup)
	// 证据里必须出现 2.1.216 而不是 9.9.9。
	if !strings.Contains(strings.Join(backupIssue.Evidence, " "), "2.1.216") {
		t.Fatalf("证据指向了错误的备份：%v", backupIssue.Evidence)
	}
	interruptedIssue := requireIssue(t, report, issuePackageInterrupted)
	requireRemedy(t, interruptedIssue, remedyCleanupInterrupted)
}

// TestDiagnoseReportsMissingCommandShim 入口被清掉。
//
// 它与"包坏了"分开：入口可以靠一条幂等的重建命令修好，不必下载整个包。
func TestDiagnoseReportsMissingCommandShim(t *testing.T) {
	server := newDiagnoseTestServer(t)
	root := t.TempDir()
	t.Setenv(toolchainRootEnv, root)
	plantManagedToolchain(t, root)

	entry := mustAgent(t, "claude-code")
	prefix := managedNpmGlobalPrefix(root)
	plantAgentFixture(t, prefix, entry, "2.1.216", agentFixtureOptions{SkipShim: true})
	install := agentNpmCLIInstall(entry)
	if err := server.recordAgentInstallation(context.Background(), agentInstallation{
		RunnerID: server.localRunnerID(), AgentID: entry.ID,
		BinaryPath:  install.binaryPath(prefix),
		InstallKind: installKindNpmManaged, Prefix: prefix, Version: "2.1.216",
	}); err != nil {
		t.Fatal(err)
	}

	report := server.buildLocalAgentDiagnosis(context.Background(), diagnoseLocalMeta(server), entry)
	requireIssue(t, report, issueShimMissing)
	// 入口那一条只给"重建入口"，不给"重装整个包" —— 前者本地就能做，后者要联网。
	issue, _ := diagnoseIssueByCode(report, issueShimMissing)
	requireRemedy(t, issue, remedyRebuildShim)
}

// TestDiagnoseDistinguishesDeadRecordFromShadowedInstall 钉住两条不同的"登记不对"。
//
//	① 登记的位置死了、别处也没有  → 用不了（blocker），要重装；
//	② 登记的位置死了、别处有一份好的 → 现在能用（只是下次升级会装回去），只是 warning。
//
// 这两档并成一句就会出现"明明能用却被说成坏了"。
func TestDiagnoseDistinguishesDeadRecordFromShadowedInstall(t *testing.T) {
	server := newDiagnoseTestServer(t)
	root := t.TempDir()
	t.Setenv(toolchainRootEnv, root)
	plantManagedToolchain(t, root)

	entry := mustAgent(t, "claude-code")
	dead := filepath.Join(t.TempDir(), "gone", "claude"+executableSuffix())
	// ① 别处也没有。
	isolateAgentLookups(t)
	if err := server.recordAgentInstallation(context.Background(), agentInstallation{
		RunnerID: server.localRunnerID(), AgentID: entry.ID,
		BinaryPath: dead, InstallKind: installKindNpmManaged, Version: "2.1.216",
	}); err != nil {
		t.Fatal(err)
	}
	report := server.buildLocalAgentDiagnosis(context.Background(), diagnoseLocalMeta(server), entry)
	if report.Status != diagnosisBroken {
		t.Fatalf("哪儿都用不了时应当判 broken，得到 %s（症状 %v）", report.Status, report.Issues)
	}
	requireIssue(t, report, issueBinaryMissing)

	// ② 往 PATH 上放一份好的。
	pathDir := t.TempDir()
	writeExecutable(t, filepath.Join(pathDir, entry.CommandName+executableSuffix()), "2.1.217")
	t.Setenv("PATH", pathDir)

	report = server.buildLocalAgentDiagnosis(context.Background(), diagnoseLocalMeta(server), entry)
	requireIssue(t, report, issueRecordStale)
	requireNoIssue(t, report, issueBinaryMissing)
	stale, _ := diagnoseIssueByCode(report, issueRecordStale)
	if stale.Severity != severityWarning {
		t.Fatalf("还能用时应当是 warning，得到 %s", stale.Severity)
	}
	if report.Version != "2.1.217" {
		t.Fatalf("实测版本 = %q，期望 PATH 上那份的 2.1.217", report.Version)
	}
}

// TestDiagnoseReportsMissingRuntime 前置运行时没了。
//
// 这一档的关键是**指出该去装 Node**，而不是让用户在工具卡片上反复点"安装"。
func TestDiagnoseReportsMissingRuntime(t *testing.T) {
	server := newDiagnoseTestServer(t)
	t.Setenv(toolchainRootEnv, t.TempDir())
	isolateAgentLookups(t) // 既没有托管运行时，也没有系统 npm

	entry := mustAgent(t, "claude-code")
	if err := server.recordAgentInstallation(context.Background(), agentInstallation{
		RunnerID: server.localRunnerID(), AgentID: entry.ID,
		BinaryPath: filepath.Join(t.TempDir(), "claude"+executableSuffix()),
		InstallKind: installKindNpmManaged, Prefix: t.TempDir(), Version: "2.1.216",
	}); err != nil {
		t.Fatal(err)
	}

	report := server.buildLocalAgentDiagnosis(context.Background(), diagnoseLocalMeta(server), entry)
	if report.Status != diagnosisBroken {
		t.Fatalf("运行时没了就该判 broken，得到 %s", report.Status)
	}
	issue := requireIssue(t, report, issueRuntimeMissing)
	requireRemedy(t, issue, remedyInstallRuntime)
	if !strings.Contains(strings.Join(issue.Evidence, " "), "Node") {
		t.Fatalf("证据里没有指出需要 Node：%v", issue.Evidence)
	}
}

// TestDiagnoseReportsNotInstalledWithoutComplaint 真的没装**不该**报毛病。
//
// 这也是"不能把没有写成坏了"的反面：一个从没安装过的工具，诊断结论是
// not-installed + 一条安装指引，而不是一串 blocker。
func TestDiagnoseReportsNotInstalledWithoutComplaint(t *testing.T) {
	server := newDiagnoseTestServer(t)
	t.Setenv(toolchainRootEnv, t.TempDir())
	isolateAgentLookups(t)

	report := server.buildLocalAgentDiagnosis(context.Background(), diagnoseLocalMeta(server), mustAgent(t, "claude-code"))
	if report.Status != diagnosisNotInstalled {
		t.Fatalf("从没装过应判 not-installed，得到 %s", report.Status)
	}
	for _, issue := range report.Issues {
		if issue.Severity == severityBlocker {
			t.Fatalf("从没装过不该出现 blocker：%+v", issue)
		}
	}
	if report.Preflight == nil {
		t.Fatal("没装时最需要的就是预检结论（去哪装、装不装得上）")
	}
}

// TestDiagnoseReportsUnsupportedEnvironment 该环境不提供这个工具。
//
// 结论是"换环境"，不是"修工具" —— 所以详查都要跳过（needsDeepDiagnosis），
// 单工具诊断也要给出这一档而不是一串误导性的 blocker。
func TestDiagnoseReportsUnsupportedEnvironment(t *testing.T) {
	server := newDiagnoseTestServer(t)
	meta := RunnerMeta{ID: "ssh-prod", Name: "prod", Environment: "ssh"}

	report := server.buildAgentDiagnosis(context.Background(), meta, mustAgent(t, "codebuddy"))
	if report.Status != diagnosisUnsupported {
		t.Fatalf("状态 = %s，期望 %s", report.Status, diagnosisUnsupported)
	}
	requireIssue(t, report, issueEnvUnsupported)
	// 这一档的结论已经确定（换环境），详查要跳过 —— 否则会白跑一串子进程。
	if needsDeepDiagnosis(AgentStatus{ID: "codebuddy", Status: agentStatusUnsupported}) {
		t.Fatal("unsupported 不该被详查")
	}
}

// TestNeedsDeepDiagnosisIsCheapAndPrecise 批量端点的过滤判据。
//
// 注意它**只在通道可用时被调用**（通道坏了调用方根本不详查），所以签名里没有
// probeOK —— 那种恒真的参数只会长出一个永远走不到的分支。
func TestNeedsDeepDiagnosis(t *testing.T) {
	cases := []struct {
		status string
		want   bool
	}{
		{agentStatusReady, false},
		{agentStatusUnavailable, true},
		// 正在安装/升级也要详查：详查会给出"此刻结果不可信，等一下再测"这一档，
		// 比什么都不说有用。
		{agentStatusUpdating, true},
		{agentStatusUnsupported, false},
	}
	for _, item := range cases {
		if got := needsDeepDiagnosis(AgentStatus{Status: item.status}); got != item.want {
			t.Fatalf("status=%s → %v，期望 %v", item.status, got, item.want)
		}
	}
}

// TestDiagnoseReportsMaintenanceInsteadOfFakeBreakage 安装进行中**不许**报"坏了"。
//
// 一刻的中间态：npm 会先把旧的包改名、再解压新的，这期间探测拿到的是空版本。
// 若不拦，诊断会给出假的 binary-broken，用户照着它去点修复 —— 而修复会被
// beginAgentMaintenance 挡成 409。判据与 probeAgent 同源（agent_probe.go:70）。
func TestDiagnoseReportsMaintenanceInsteadOfFakeBreakage(t *testing.T) {
	server := newDiagnoseTestServer(t)
	root := t.TempDir()
	t.Setenv(toolchainRootEnv, root)
	plantManagedToolchain(t, root)

	entry := mustAgent(t, "claude-code")
	prefix := managedNpmGlobalPrefix(root)
	// 磁盘上摆一份**坏的**（如果诊断真的去探测，它一定会报 binary-broken）。
	plantAgentFixture(t, prefix, entry, "2.1.216", agentFixtureOptions{BrokenCommand: true})
	if err := server.recordAgentInstallation(context.Background(), agentInstallation{
		RunnerID: server.localRunnerID(), AgentID: entry.ID,
		BinaryPath:  agentNpmCLIInstall(entry).commandPath(prefix),
		InstallKind: installKindNpmManaged, Prefix: prefix, Version: "2.1.216",
	}); err != nil {
		t.Fatal(err)
	}

	// 先确认"没有 maintenance 时它确实会报坏"——否则下面那条断言可能因为别的原因绿。
	baseline := server.buildLocalAgentDiagnosis(context.Background(), diagnoseLocalMeta(server), entry)
	requireIssue(t, baseline, issueBinaryBroken)

	// 现在声称该工具正在安装/升级。
	server.runnerUpdating[runnerAgentKey{runnerID: server.localRunnerID(), agentID: entry.ID}] = true
	report := server.buildLocalAgentDiagnosis(context.Background(), diagnoseLocalMeta(server), entry)

	if report.Status != diagnosisUnknown {
		t.Fatalf("安装进行中时状态 = %s，期望 %s（不能给结论）", report.Status, diagnosisUnknown)
	}
	requireIssue(t, report, issueMaintenanceActive)
	// 关键：**不许**给出假的症状。
	requireNoIssue(t, report, issueBinaryBroken)
	requireNoIssue(t, report, issueShimMissing)
	requireNoIssue(t, report, issueActivePackageBroken)
	if report.Preflight != nil {
		t.Fatal("安装进行中时不该给预检结论（那一刻的状态随时会变）")
	}
}

// TestDiagnoseReadsLastFailureFromAudit 审计里那条"上次怎么坏的"必须被带出来。
//
// 数据本来就在库里（installAgentFor / updateAgent 都写了 detail），
// 在诊断之前没有任何地方读过它。
func TestDiagnoseReadsLastFailureFromAudit(t *testing.T) {
	server := newDiagnoseTestServer(t)
	root := t.TempDir()
	t.Setenv(toolchainRootEnv, root)
	plantManagedToolchain(t, root)

	runnerID := server.localRunnerID()
	ctx := context.Background()
	if err := server.recordInstallAudit(ctx, installAuditEntry{
		RunnerID: runnerID, AgentID: "claude-code", Action: "update",
		FromVersion: "2.1.216", Result: "failed",
		Detail: "安装 Claude Code 失败：exit status 1（npm ERR! network timeout）",
	}); err != nil {
		t.Fatal(err)
	}

	report := server.buildLocalAgentDiagnosis(ctx, diagnoseLocalMeta(server), mustAgent(t, "claude-code"))
	if report.LastFailure == nil {
		t.Fatal("没有把审计里的失败记录带出来 —— 那是用户唯一的现场证据")
	}
	if !strings.Contains(report.LastFailure.Detail, "npm ERR! network timeout") {
		t.Fatalf("失败原文被丢掉了：%q", report.LastFailure.Detail)
	}
	// 清洗过：不该带 ANSI 转义。
	if strings.ContainsAny(report.LastFailure.Detail, "\x1b\r") {
		t.Fatalf("失败原文没洗干净：%q", report.LastFailure.Detail)
	}
}

// TestDiagnosePreflightMatchesInstallFailure ⭐ 本方案最关键的一条。
//
// 诊断给出的"为什么点了会失败"，必须与真的点下去时服务端返回的**同一个字符串**。
// 一旦有人在诊断里另写一套判断，这条立刻红 —— 而那是 docs/42 §19.2 E 付过代价的错。
func TestDiagnosePreflightMatchesInstallFailure(t *testing.T) {
	server := newDiagnoseTestServer(t)
	t.Setenv(toolchainRootEnv, t.TempDir())
	isolateAgentLookups(t)

	entry := mustAgent(t, "claude-code")
	report := server.buildLocalAgentDiagnosis(context.Background(), diagnoseLocalMeta(server), entry)
	if report.Preflight == nil || report.Preflight.InstallOK {
		t.Fatalf("两种运行时都没有时，预检应当报「装不了」：%#v", report.Preflight)
	}

	// ① 判据本身：真的走一次安装计划解析，报错必须**逐字相同**。
	_, rawErr := server.installAgentCLIFor(context.Background(), server.localRunnerID(), entry.ID, "latest")
	if rawErr == nil {
		t.Fatal("预期安装失败，却成功了")
	}
	if got := errorText(rawErr); got != report.Preflight.InstallReason {
		t.Fatalf("预检与真实失败**不是同一句话**：\n预检：%q\n实际：%q", report.Preflight.InstallReason, got)
	}
	// ② 升级与安装共用同一份判据（performAgentUpdate 对 npm 类就是重装 latest）。
	if report.Preflight.UpgradeReason != report.Preflight.InstallReason {
		t.Fatalf("升级与安装的判据分叉了：%q vs %q",
			report.Preflight.UpgradeReason, report.Preflight.InstallReason)
	}

	// ③ 用户看到的那一份：HTTP 层会加一句通用前缀（localizedHTTPErrorText 的职责是
	// 不把内部实现细节泄露给界面），但**内核必须是同一串**。
	router := chi.NewRouter()
	router.Post("/api/runners/{runnerID}/agents/{agentID}/install", server.installAgentStatus)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost,
		"/api/runners/"+server.localRunnerID()+"/agents/claude-code/install", strings.NewReader(`{"version":"latest"}`)))
	if recorder.Code == http.StatusOK {
		t.Fatalf("预期安装失败，却成功了：%s", recorder.Body.String())
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析安装失败响应：%v body=%s", err, recorder.Body.String())
	}
	if !strings.HasSuffix(httpErrorMessageCore(body.Error), httpErrorMessageCore(report.Preflight.InstallReason)) {
		t.Fatalf("失败响应里的原因与预检对不上：\n响应：%q\n预检：%q", body.Error, report.Preflight.InstallReason)
	}
}

// httpErrorMessageCore 剥掉"通用前缀"那一层，取出真正给用户看的原因。
//
// 两处的通用前缀**本来就不同**，而且是有意的：`errorText` 用的是"任务执行失败，
// 请查看任务日志后重试。"，HTTP 层按状态码选（500 → "服务内部错误，请稍后重试。"），
// 后者是为了不把内部实现细节泄露给界面（localizedHTTPErrorText 的注释）。
// 所以这一条要钉的是**内核那一串逐字相同**，而不是整串相等。
func httpErrorMessageCore(message string) string {
	if index := strings.Index(message, "。："); index >= 0 {
		return message[index+len("。："):]
	}
	return message
}

// TestDiagnoseCrossReportsItsLimits 跨端只说能确定的事。
//
// 探测说不可用时**分不出**"没装"与"坏了"，所以状态是 unknown ——
// 猜成 broken 或 not-installed 都是在把"没查"写成结论。
func TestDiagnoseCrossReportsItsLimits(t *testing.T) {
	server := newDiagnoseTestServer(t)
	meta := RunnerMeta{ID: "ssh-prod", Name: "prod", Environment: "ssh"}

	report := server.buildAgentDiagnosis(context.Background(), meta, mustAgent(t, "claude-code"))
	if report.Status != diagnosisUnknown {
		t.Fatalf("跨端未接通深度检查时状态 = %s，期望 %s", report.Status, diagnosisUnknown)
	}
	if len(report.Limitations) == 0 {
		t.Fatal("受限诊断必须如实列出没跑的检查，否则界面会把「没查」渲染成「没问题」")
	}
	if len(report.Issues) != 0 {
		t.Fatalf("什么都没查到就不该给出症状：%+v", report.Issues)
	}
}

// TestDiagnoseReadFailureIsNotMissingRegistration 登记表读失败 ≠ 没登记过。
//
// 把读失败当成"没登记"，会让一份装着的工具被当成没装过 —— 那是本项目反复禁止的
// "把读不到写成没有"。这里把表删掉逼出读失败。
func TestDiagnoseReadFailureIsNotMissingRegistration(t *testing.T) {
	server := newDiagnoseTestServer(t)
	t.Setenv(toolchainRootEnv, t.TempDir())
	isolateAgentLookups(t)
	if _, err := server.db.ExecContext(context.Background(), `drop table agent_installations`); err != nil {
		t.Fatal(err)
	}

	report := server.buildLocalAgentDiagnosis(context.Background(), diagnoseLocalMeta(server), mustAgent(t, "claude-code"))
	if report.Status != diagnosisUnknown {
		t.Fatalf("登记表读不到时状态 = %s，期望 %s（不能猜成 not-installed）", report.Status, diagnosisUnknown)
	}
	issue := requireIssue(t, report, issueRecordUnreadable)
	if issue.Severity != severityBlocker {
		t.Fatalf("读不到登记是 blocker，得到 %s", issue.Severity)
	}
}

// ── 路径事实的三态（Checked / Exists / Probed） ─────────────────────────────

// TestDiagnoseReportsDeadOverridePath 环境变量覆盖指向一个死文件时必须报出来。
//
// 这是**回归用例**：修之前 `assessResolvedPath` 的 effective 分支取自 `probes` 这个
// map，而 map 只在"文件真的存在"时才写入 —— 于是"存在吗"与"实测过吗"被压成一个
// 布尔，`!fact.Exists` 那一支永远进不去。症状是：resolver 对 override **无条件返回**
// （它不检查存在性，也不往下找），命令行其实用不了；而诊断一条症状都不报，最终判成
// "没有问题" —— 也就是这一整轮要消灭的那类结论。
func TestDiagnoseReportsDeadOverridePath(t *testing.T) {
	server := newDiagnoseTestServer(t)
	root := t.TempDir()
	t.Setenv(toolchainRootEnv, root)
	plantManagedToolchain(t, root)
	isolateAgentLookups(t)

	entry := mustAgent(t, "claude-code")
	prefix := managedNpmGlobalPrefix(root)
	install := agentNpmCLIInstall(entry)
	// 装好的那一份（登记指向它、它自己也能跑）—— 于是"没有 override 时一切正常"。
	plantAgentFixture(t, prefix, entry, "2.1.216", agentFixtureOptions{})
	if err := server.recordAgentInstallation(context.Background(), agentInstallation{
		RunnerID: server.localRunnerID(), AgentID: entry.ID,
		BinaryPath:  install.commandPath(prefix),
		InstallKind: installKindNpmManaged, Prefix: prefix, Version: "2.1.216",
	}); err != nil {
		t.Fatal(err)
	}

	// 反证：没有 override 时它是**真的能用**（实测出版本号）—— 这样下面那条断言才
	// 说明"是 override 造成的"，而不是"这台机器本来就坏着"。
	healthy := server.buildLocalAgentDiagnosis(context.Background(), diagnoseLocalMeta(server), entry)
	requireNoIssue(t, healthy, issueOverrideDead)
	requireNoIssue(t, healthy, issueBinaryBroken)
	requireNoIssue(t, healthy, issueBinaryMissing)
	if healthy.Version != "2.1.216" {
		t.Fatalf("反证不成立：没有 override 时实测版本 = %q，期望 2.1.216", healthy.Version)
	}

	// 把覆盖指到一个不存在的文件上。
	dead := filepath.Join(t.TempDir(), "gone", "claude"+executableSuffix())
	server.paths.override[entry.ID] = dead

	report := server.buildLocalAgentDiagnosis(context.Background(), diagnoseLocalMeta(server), entry)
	issue := requireIssue(t, report, issueOverrideDead)
	if issue.Severity != severityBlocker {
		t.Fatalf("覆盖指向死文件是 blocker，得到 %s", issue.Severity)
	}
	if report.Status != diagnosisBroken {
		t.Fatalf("状态 = %s，期望 %s（它现在确实用不了）", report.Status, diagnosisBroken)
	}
	// **不给修复按钮**：平台清不掉部署方设的环境变量，重装也没有用（override 仍然
	// 优先于一切）。给一个点了修不好的按钮比不给更坏。
	if len(issue.Remedies) != 0 {
		t.Fatalf("死覆盖没有平台可自动修的动作，却给出了 %v", issue.Remedies)
	}
	// 证据里必须带上那条配置路径，否则用户无从下手。
	joined := strings.Join(issue.Evidence, " ")
	if !strings.Contains(joined, dead) {
		t.Fatalf("证据里没有那条配置路径：%v", issue.Evidence)
	}
	// 注：这里**不**断言"它是唯一的 blocker"。夹具里的托管 node 在 Windows 上只是一
	// 个批处理文件（写不出真的 PE），probeRuntime 因此读不出版本、会报 runtime-broken
	// ——那是夹具的限制（同 TestDiagnoseLocalIsReadOnly 的注），不是被测行为。
	// "是 override 造成的"这一点由上面的反证钉住（没有 override 时它实测可用）。
	// 路径事实表要把这条报成"核对过、不存在"（它是本机的一条真实读数）。
	fact, ok := diagnosePathFactByPath(report.Paths, dead)
	if !ok {
		t.Fatalf("路径事实里没有那条覆盖路径：%+v", report.Paths)
	}
	if !fact.Checked || fact.Exists {
		t.Fatalf("覆盖路径应当是「核对过但不存在」：%+v", fact)
	}
}

// TestDiagnoseDoesNotCallUnprobedPathBroken 存在但**没实测过**的路径不许被说成坏的。
//
// 实测预算是有限的（diagnoseProbeBudget），于是总会有"文件在、但我们没跑过它"的
// 候选。那一档既不能说"可执行"、也不能说"跑不起来" —— 后者是把"没查"写成"坏了"。
func TestDiagnoseDoesNotCallUnprobedPathBroken(t *testing.T) {
	server := newDiagnoseTestServer(t)
	entry := mustAgent(t, "claude-code")
	live := filepath.Join(t.TempDir(), "claude"+executableSuffix())
	writeExecutable(t, live, "2.1.216")

	report := agentDiagnosis{Issues: []diagnoseIssue{}, Paths: []diagnosePathFact{}}
	server.assessResolvedPath(&report, entry, live,
		diagnosePathFact{Path: live, Checked: true, Exists: true, Probed: false},
		true, pathProbe{Path: live, Exists: true}, false, false, agentInstallation{})

	requireNoIssue(t, report, issueBinaryBroken)
	requireNoIssue(t, report, issueProbeTimeout)
	requireNoIssue(t, report, issueOverrideDead)
	if len(report.Limitations) == 0 {
		t.Fatal("没实测就必须如实说「这一项没查」，否则界面会把它念成「存在但跑不起来」")
	}
}

// TestDiagnoseDoesNotCallCanceledProbeBroken 实测**被中断**也不许说成"坏了"。
//
// 父上下文取消（请求断了 / 操作结束了）时 `probeCtx.Err()` 是 Canceled 而不是
// DeadlineExceeded，原先两条都被压成"Works=false" ⇒ 报出假的 `binary-broken` blocker。
// 那是"我们没查完"，不是这个工具的问题 —— 一次断连不该在界面上留一条假结论。
func TestDiagnoseDoesNotCallCanceledProbeBroken(t *testing.T) {
	server := newDiagnoseTestServer(t)
	entry := mustAgent(t, "claude-code")
	live := filepath.Join(t.TempDir(), "claude"+executableSuffix())

	report := agentDiagnosis{Issues: []diagnoseIssue{}, Paths: []diagnosePathFact{}}
	server.assessResolvedPath(&report, entry, live,
		diagnosePathFact{Path: live, Checked: true, Exists: true, Probed: true},
		true, pathProbe{Path: live, Exists: true, Canceled: true}, true, false, agentInstallation{})

	requireNoIssue(t, report, issueBinaryBroken)
	requireNoIssue(t, report, issueProbeTimeout)
	if len(report.Limitations) == 0 {
		t.Fatal("被中断就必须如实说「这次没得出执行结论」，否则界面会把它念成「执行不起来」")
	}
}

// TestDiagnoseDoesNotCallUnreadablePathMissing 读不到 ≠ 不存在。
//
// `fileExists` 把 Stat 的权限/IO 失败与"真的不存在"压成同一个 false，而这一档在
// 生效路径上会直接变成一句"配置里指定的可执行文件路径不存在"（blocker）——
// 那是把"读不到"写成"没有"。
func TestDiagnoseDoesNotCallUnreadablePathMissing(t *testing.T) {
	server := newDiagnoseTestServer(t)
	entry := mustAgent(t, "claude-code")
	live := filepath.Join(t.TempDir(), "claude"+executableSuffix())

	report := agentDiagnosis{Issues: []diagnoseIssue{}, Paths: []diagnosePathFact{}}
	server.assessResolvedPath(&report, entry, live,
		// Checked=false、Exists=false 就是"读不到"那一档的形状。
		diagnosePathFact{Path: live, Checked: false, Exists: false},
		true, pathProbe{Path: live}, false, false, agentInstallation{})

	requireNoIssue(t, report, issueOverrideDead)
	requireNoIssue(t, report, issueBinaryBroken)
	if len(report.Limitations) == 0 {
		t.Fatal("读不到就必须如实说「读不到」，否则界面会把它念成「路径不存在」")
	}
}

// TestDiagnoseJudgesSystemNpmRuntimeFromSystemNode 装在系统 npm 全局里的工具，
// 运行时判据必须看**系统那一套**。
//
// 假 blocker 的构造：托管运行时"文件在、跑不起来"（一次失败的运行时安装留下的典型
// 残留），而系统那份 node/npm 好着。用 `probeRuntime`（托管优先）的读数去判这个工具，
// 会得到"找不到可用的 Node.js 运行时"—— 可它其实跑得好好的；而且给出的修复动作
//（装托管运行时）根本换不掉它实际用的那个 node。
func TestDiagnoseJudgesSystemNpmRuntimeFromSystemNode(t *testing.T) {
	entry := mustAgent(t, "claude-code")
	record := func(t *testing.T, server *Server) {
		t.Helper()
		if err := server.recordAgentInstallation(context.Background(), agentInstallation{
			RunnerID: server.localRunnerID(), AgentID: entry.ID,
			BinaryPath:  filepath.Join(t.TempDir(), "claude"+executableSuffix()),
			InstallKind: installKindNpmSystem, Version: "2.1.216",
		}); err != nil {
			t.Fatal(err)
		}
	}

	server := newDiagnoseTestServer(t)
	root := t.TempDir()
	t.Setenv(toolchainRootEnv, root)
	// 托管运行时"装着但跑不起来"：Windows 上它只是个批处理文件，写不出真的 PE。
	plantManagedToolchain(t, root)
	isolateAgentLookups(t)
	record(t, server)

	// ① 系统那边什么都没有 ⇒ 该报"找不到系统 npm"（这条是反证，证明上面那句假 blocker 的前提真实存在）。
	broken := server.buildLocalAgentDiagnosis(context.Background(), diagnoseLocalMeta(server), entry)
	requireIssue(t, broken, issueNpmUnavailable)

	// ② 系统那份 npm + node（24.0.0）可用 ⇒ 一条运行时毛病都不该报。
	systemDir := t.TempDir()
	writeExecutable(t, filepath.Join(systemDir, "npm"+exeSuffixForTest()), "11.0.0")
	writeExecutable(t, filepath.Join(systemDir, "node"+exeSuffixForTest()), "24.0.0")
	t.Setenv("PATH", systemDir)

	healthy := server.buildLocalAgentDiagnosis(context.Background(), diagnoseLocalMeta(server), entry)
	requireNoIssue(t, healthy, issueNpmUnavailable)
	requireNoIssue(t, healthy, issueRuntimeMissing)
	requireNoIssue(t, healthy, issueRuntimeTooOld)
	requireNoIssue(t, healthy, issueRuntimeBroken)
}

// TestDiagnoseCrossMarksPathFactsUnchecked 跨端不许把本机的读数写成目标机的事实。
//
// 登记里的路径在**目标环境的文件系统**上（WSL / SSH），而 fileExists 是本机的 Stat：
// 对一条 `/usr/local/bin/claude` 报"不存在"，与同一份报告里"没有在目标环境核对文件"
// 那句 Limitations 直接打架。
func TestDiagnoseCrossMarksPathFactsUnchecked(t *testing.T) {
	server := newDiagnoseTestServer(t)
	meta := RunnerMeta{ID: "ssh-prod", Name: "prod", Environment: "ssh"}
	entry := mustAgent(t, "claude-code")
	if err := server.recordAgentInstallation(context.Background(), agentInstallation{
		RunnerID: meta.ID, AgentID: entry.ID,
		BinaryPath: "/usr/local/bin/claude", InstallKind: installKindNpmManaged,
		Prefix: "/usr/local", Version: "2.1.216",
	}); err != nil {
		t.Fatal(err)
	}

	report := server.buildAgentDiagnosis(context.Background(), meta, entry)
	if len(report.Paths) != 1 {
		t.Fatalf("应当给出 1 条登记位置：%+v", report.Paths)
	}
	fact := report.Paths[0]
	if fact.Checked {
		t.Fatal("跨端不能把本机的 Stat 当成目标机的事实")
	}
	if fact.Exists || fact.Probed || fact.Works {
		t.Fatalf("未核对的路径不能带任何结论：%+v", fact)
	}
	if len(report.Limitations) == 0 {
		t.Fatal("必须如实说没在目标环境核对过")
	}
}

// TestDiagnosisAddDropsUnknownRemedyIDs 报告里只允许出现白名单表里的动作。
//
// 这是"界面照着诊断亮出一个点了没反应的动作"在结构上不可能的那条保证 ——
// 也是"请求体里的字符串只当表键用"的前半句。
func TestDiagnosisAddDropsUnknownRemedyIDs(t *testing.T) {
	report := agentDiagnosis{Issues: []diagnoseIssue{}}
	report.add(issueBinaryBroken, severityBlocker, "半装", nil,
		remedyReinstall, "reset-record", "重建入口=ok", "")

	if len(report.Issues) != 1 {
		t.Fatalf("症状条数 = %d", len(report.Issues))
	}
	offered := report.Issues[0].Remedies
	if len(offered) != 1 || offered[0].ID != remedyReinstall {
		t.Fatalf("只有白名单里的动作能进报告，实际：%+v", offered)
	}
	// 文案也必须来自那张表（否则界面上的按钮与真正执行的动作会分叉）。
	if offered[0].Label != agentRemedies[remedyReinstall].Label {
		t.Fatalf("label 不是来自白名单表：%q", offered[0].Label)
	}
}

// TestDiagnosePreflightIncludesRuntimeGate 预检必须覆盖"装得上"的**全部**判据。
//
// 安装真正走的是两步：先解析出计划（resolveAgentInstallPlan），再过运行时闸门
// （checkRuntimeGate）。预检原先只做第一步 —— Node 太旧时会说"可以装"、点下去才
// 报"运行时版本过低"，而把这句提前正是预检存在的唯一理由。
func TestDiagnosePreflightIncludesRuntimeGate(t *testing.T) {
	// 一台"只有系统 npm"的机器：PATH 上放假的 npm 与 node，托管工具链不存在。
	plant := func(t *testing.T, nodeVersion string) {
		t.Helper()
		t.Setenv(toolchainRootEnv, t.TempDir())
		dir := t.TempDir()
		writeExecutable(t, filepath.Join(dir, "npm"+exeSuffixForTest()), "11.0.0")
		writeExecutable(t, filepath.Join(dir, "node"+exeSuffixForTest()), nodeVersion)
		t.Setenv("PATH", dir)
		t.Setenv("APPDATA", t.TempDir())
	}
	entry := mustAgent(t, "claude-code")

	// ① 正对照：Node 满足要求时预检必须说"可以装"。
	// 没有这一档的话，下面的断言可能因为"配置本来就装不了"而假绿。
	server := newDiagnoseTestServer(t)
	plant(t, "24.0.0")
	ok := server.buildLocalAgentDiagnosis(context.Background(), diagnoseLocalMeta(server), entry)
	if ok.Preflight == nil || !ok.Preflight.InstallOK {
		t.Fatalf("Node 24 时预检应当说可以装：%#v", ok.Preflight)
	}

	// ② Node 太低：预检必须说"不能装"，而且理由就是闸门那一句。
	server = newDiagnoseTestServer(t)
	plant(t, "16.0.0")
	report := server.buildLocalAgentDiagnosis(context.Background(), diagnoseLocalMeta(server), entry)
	if report.Preflight == nil || report.Preflight.InstallOK {
		t.Fatalf("Node 16 时预检应当说不能装：%#v", report.Preflight)
	}
	if !strings.Contains(report.Preflight.InstallReason, "运行时版本过低") {
		t.Fatalf("理由应当指向运行时版本，得到 %q", report.Preflight.InstallReason)
	}
	// ③ 与真正点安装时报的是同一句话（判据只有一份）。
	_, rawErr := server.installAgentCLIFor(context.Background(), server.localRunnerID(), entry.ID, "latest")
	if rawErr == nil {
		t.Fatal("预期安装失败，却成功了")
	}
	if got := errorText(rawErr); got != report.Preflight.InstallReason {
		t.Fatalf("预检与真实失败不是同一句话：\n预检：%q\n实际：%q", report.Preflight.InstallReason, got)
	}
}
