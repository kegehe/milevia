package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// 真实端到端测试：**默认跳过**，用 `MILEVIA_E2E=1` 打开。
//
// 为什么必须存在这一类：其余测试用的是假 npm、假环境、假输出，而它们**答不了**
// "装得上吗、装完真的能用吗"。docs/42 §19 记的两条最严重的错
// （跨端脚本引用了尚未赋值的 `$prefix`、SSH 上传的压缩包被自己删掉）
// 全都逃过了当时的全部单测 —— 因为那两层都不真正执行脚本。
//
// 运行方式：
//
//	MILEVIA_E2E=1 go test ./internal/app/ -run TestRealE2E -v -timeout 40m
//
// 它会做什么（全部是真的）：
//   - 真的访问 nodejs.org 下载 Node，真的校验 SHA256，真的解压，真的执行解出来的 node；
//   - 真的访问 registry.npmjs.org，把 Claude Code 真的装进**临时**工具链前缀；
//   - 真的执行装出来的 `claude.cmd --version` 自检；
//   - 真的走一遍"检查更新 → 升级"，并核对版本真的变了。
//
// 它**不会**碰用户既有的安装：托管运行时装好之后，安装计划必然指向托管前缀；
// 用例里有一道显式断言守着这件事（见 §6），万一不成立就直接失败而不是继续装。
// 全程只写到 `.tmp/e2e-run/` 下，跑完可以整个删掉。

const e2eEnv = "MILEVIA_E2E"

// ── 响应形状（只声明这个用例真正读的字段）────────────────────────────────

type e2eToolStatus struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Version string `json:"version"`
	Reason  string `json:"reason"`
}

type e2eRunner struct {
	ID     string          `json:"id"`
	Name   string          `json:"name"`
	Agents []e2eToolStatus `json:"agents"`
	Claude *e2eToolStatus  `json:"claude"`
	Codex  *e2eToolStatus  `json:"codex"`
}

type e2eRuntime struct {
	ID                   string   `json:"id"`
	Installed            bool     `json:"installed"`
	Version              string   `json:"version"`
	NpmVersion           string   `json:"npmVersion"`
	NpmPath              string   `json:"npmPath"`
	Origin               string   `json:"origin"`
	ManagedPath          string   `json:"managedPath"`
	MeetsMinimumFor      []string `json:"meetsMinimumFor"`
	InstallSupported     bool     `json:"installSupported"`
	InstallBlockedReason string   `json:"installBlockedReason"`
	LatestVersion        string   `json:"latestVersion"`
	UpdateAvailable      bool     `json:"updateAvailable"`
}

type e2eAgentItem struct {
	ID                   string `json:"id"`
	Installed            bool   `json:"installed"`
	Version              string `json:"version"`
	BinaryPath           string `json:"binaryPath"`
	InstallKindUsed      string `json:"installKindUsed"`
	Ready                bool   `json:"ready"`
	Reason               string `json:"reason"`
	InstallSupported     bool   `json:"installSupported"`
	InstallBlockedReason string `json:"installBlockedReason"`
	UpgradeNeedsGrant    bool   `json:"upgradeNeedsGrant"`
	UpdateSupported      bool   `json:"updateSupported"`
	AutoUpdatable        bool   `json:"autoUpdatable"`
	Operation            string `json:"operation"`
}

type e2eRunnerAgents struct {
	RunnerID             string         `json:"runnerId"`
	Environment          string         `json:"environment"`
	ProbeOK              bool           `json:"probeOk"`
	ProbeError           string         `json:"probeError"`
	Runtime              *e2eRuntime    `json:"runtime"`
	Items                []e2eAgentItem `json:"items"`
	RemoteInstallAllowed bool           `json:"remoteInstallAllowed"`
}

type e2eInstallResult struct {
	Success     bool        `json:"success"`
	Version     string      `json:"version"`
	BinaryPath  string      `json:"binaryPath"`
	InstallKind string      `json:"installKind"`
	Prefix      string      `json:"prefix"`
	Runtime     *e2eRuntime `json:"runtime"`
}

type e2eUpdateCheck struct {
	UpdateAvailable bool   `json:"updateAvailable"`
	AutoUpdatable   bool   `json:"autoUpdatable"`
	CurrentVersion  string `json:"currentVersion"`
	LatestVersion   string `json:"latestVersion"`
	Error           string `json:"error"`
}

type e2eUpdateResult struct {
	Success         bool   `json:"success"`
	PreviousVersion string `json:"previousVersion"`
	CurrentVersion  string `json:"currentVersion"`
	Error           string `json:"error"`
}

type e2eAuditItem struct {
	AgentID     string `json:"agentId"`
	Action      string `json:"action"`
	FromVersion string `json:"fromVersion"`
	ToVersion   string `json:"toVersion"`
	Result      string `json:"result"`
	Detail      string `json:"detail"`
}

type e2eAuditList struct {
	RunnerID string         `json:"runnerId"`
	Items    []e2eAuditItem `json:"items"`
}

type e2eCatalogEntry struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Vendor  string `json:"vendor"`
	Command string `json:"commandName"`
	NpmPkg  string `json:"npmPackage"`
	MinNode string `json:"minRuntimeVersion"`
}

// ── 小工具 ────────────────────────────────────────────────────────────────

func e2eSay(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Logf("E2E │ "+format, args...)
}

func e2eCut(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}

func e2eRequest(t *testing.T, client *http.Client, method, url string, payload any, out any) int {
	t.Helper()
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("序列化请求体失败：%v", err)
		}
		body = strings.NewReader(string(raw))
	}
	request, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatalf("构造请求失败：%v", err)
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s %s 失败：%v", method, url, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("读取响应失败：%v", err)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("解析 %s 的响应失败：%v\n原文：%s", url, err, e2eCut(string(raw), 500))
		}
	}
	return response.StatusCode
}

func e2eFindItem(items []e2eAgentItem, id string) *e2eAgentItem {
	for index := range items {
		if items[index].ID == id {
			return &items[index]
		}
	}
	return nil
}

func e2eFindRunner(rows []e2eRunner, id string) *e2eRunner {
	for index := range rows {
		if rows[index].ID == id {
			return &rows[index]
		}
	}
	return nil
}

// e2eAgentVersion 从 /api/runners 的响应里取某个工具的版本。
//
// 首选新增的 agents[]，取不到再回落到旧的具名字段 —— 两条都读是为了让用例在
// "线格式迁移做了一半"时仍然给出有用信息，而不是报一句无关的失败。
func e2eAgentVersion(runner *e2eRunner, id string) (string, bool) {
	if runner == nil {
		return "", false
	}
	for _, status := range runner.Agents {
		if status.ID == id {
			return status.Version, true
		}
	}
	switch id {
	case "claude-code":
		if runner.Claude != nil {
			return runner.Claude.Version, true
		}
	case "codex":
		if runner.Codex != nil {
			return runner.Codex.Version, true
		}
	}
	return "", false
}

// e2eNpmVersions 取 npm 上的真实版本清单。
func e2eNpmVersions(t *testing.T, ctx context.Context, pkg string) (string, []string) {
	t.Helper()
	url := "https://registry.npmjs.org/" + strings.ReplaceAll(pkg, "/", "%2f")
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("构造 registry 请求失败：%v", err)
	}
	request.Header.Set("User-Agent", "milevia-e2e")
	response, err := (&http.Client{Timeout: 60 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("访问 npm registry 失败：%v", err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("读取 registry 响应失败：%v", err)
	}
	var payload struct {
		DistTags map[string]string `json:"dist-tags"`
		Versions map[string]any    `json:"versions"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("解析 registry 响应失败：%v", err)
	}
	latest := strings.TrimSpace(payload.DistTags["latest"])
	if latest == "" {
		t.Fatalf("%s 的 registry 响应里没有 dist-tags.latest", pkg)
	}
	stable := make([]string, 0, len(payload.Versions))
	for version := range payload.Versions {
		if _, err := parseSemver(version); err == nil && !strings.ContainsAny(version, "-+") {
			stable = append(stable, version)
		}
	}
	sort.SliceStable(stable, func(i, j int) bool {
		left, leftErr := parseSemver(stable[i])
		right, rightErr := parseSemver(stable[j])
		if leftErr != nil || rightErr != nil {
			return stable[i] < stable[j]
		}
		return compareSemver(left, right) < 0
	})
	return latest, stable
}

// e2eServer 建一个真实服务（真 DB、真路由、真 runner），落点全在 scratch 下。
func e2eServer(t *testing.T, scratch, database string) *Server {
	t.Helper()
	hookPath, err := filepath.Abs("../../../../scripts/claude-approval-hook.sh")
	if err != nil {
		t.Fatalf("解析 hook 路径失败：%v", err)
	}
	server, err := New(context.Background(), Config{
		DatabasePath: filepath.Join(scratch, database),
		AllowedRoot:  filepath.Join(scratch, "workspace"),
		ClaudePath:   "claude",
		ControlURL:   "http://127.0.0.1:8080",
		ApprovalHook: hookPath,
	})
	if err != nil {
		t.Fatalf("创建服务失败：%v", err)
	}
	return server
}

// e2eRequestSafe 与 e2eRequest 等价，但**不在失败时 Fatal**。
//
// 存在的理由只有一个：并发用例要在 goroutine 里发请求，而 t.Fatal 只能从测试
// 自己的 goroutine 调用。
func e2eRequestSafe(client *http.Client, method, url string, payload any) (int, []byte, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, err
		}
		body = strings.NewReader(string(raw))
	}
	request, err := http.NewRequest(method, url, body)
	if err != nil {
		return 0, nil, err
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return response.StatusCode, nil, err
	}
	return response.StatusCode, raw, nil
}

// ── 用例 ─────────────────────────────────────────────────────────────────

func TestRealE2EInstallAndUpdate(t *testing.T) {
	if os.Getenv(e2eEnv) != "1" {
		t.Skip("设置 MILEVIA_E2E=1 才运行真实端到端（会联网并真的安装几百 MB）")
	}
	ctx := context.Background()

	scratch, err := filepath.Abs(filepath.Join("..", "..", "..", "..", ".tmp", "e2e-run"))
	if err != nil {
		t.Fatalf("解析 scratch 目录失败：%v", err)
	}
	// 每次都从"干净的机器"开始：上一轮的托管工具链与登记表都清掉，
	// 否则测出来的可能是上次装剩下的东西。
	for _, path := range []string{filepath.Join(scratch, "toolchain"), filepath.Join(scratch, "e2e.db")} {
		if err := os.RemoveAll(path); err != nil {
			t.Fatalf("清理 %s 失败：%v", path, err)
		}
	}
	root := filepath.Join(scratch, "toolchain")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatalf("创建 scratch 失败：%v", err)
	}
	// 托管工具链落到 scratch，而不是 %LOCALAPPDATA%\Milevia\toolchain。
	t.Setenv(toolchainRootEnv, root)

	report := map[string]any{}
	defer func() {
		raw, _ := json.MarshalIndent(report, "", "  ")
		_ = os.WriteFile(filepath.Join(scratch, "report.json"), raw, 0o644)
	}()

	server := e2eServer(t, scratch, "e2e.db")
	defer server.Close()
	httpServer := httptest.NewServer(server.routes())
	defer httpServer.Close()
	// 单次请求上限给足：一次 CLI 安装要下载 ~220MB 的原生二进制。
	client := &http.Client{Timeout: 30 * time.Minute}
	api := func(path string) string { return httpServer.URL + path }
	localID := server.localRunnerID()
	e2eSay(t, "本机 runner = %s，托管工具链根目录 = %s", localID, root)

	// ── 0. 先记住"用户机器上原本那份"，收尾要核对它没被动过 ──────────────
	claudeEntry, ok := agentByID("claude-code")
	if !ok {
		t.Fatal("目录里没有 claude-code")
	}
	systemPath, err := exec.LookPath(claudeEntry.CommandName)
	if err != nil {
		t.Skipf("这台机器上没有装 %s（PATH 里找不到），无法验证\"不碰用户既有安装\"这条", claudeEntry.CommandName)
	}
	systemVersion := agentVersionFromOutput(runVersionCommand(ctx, systemPath, claudeEntry.VersionArgs...))
	if systemVersion == "" {
		t.Fatalf("系统那份 %s 读不出版本（%s）", claudeEntry.CommandName, systemPath)
	}
	e2eSay(t, "系统既有的 %s：%s @ %s", claudeEntry.CommandName, systemVersion, systemPath)
	report["systemBefore"] = map[string]any{"path": systemPath, "version": systemVersion}

	// ── 1. 工具目录 ────────────────────────────────────────────────────
	var catalog []e2eCatalogEntry
	if status := e2eRequest(t, client, http.MethodGet, api("/api/agents"), nil, &catalog); status != http.StatusOK {
		t.Fatalf("GET /api/agents 返回 %d", status)
	}
	if len(catalog) < 2 {
		t.Fatalf("目录只返回了 %d 个工具：%+v", len(catalog), catalog)
	}
	e2eSay(t, "目录：%d 个工具（%s）", len(catalog), e2eCatalogNames(catalog))
	report["catalog"] = catalog

	// ── 2. 真实探测：本机既有的两个工具都该被如实读出来 ────────────────
	var runners []e2eRunner
	if status := e2eRequest(t, client, http.MethodGet, api("/api/runners"), nil, &runners); status != http.StatusOK {
		t.Fatalf("GET /api/runners 返回 %d", status)
	}
	local := e2eFindRunner(runners, localID)
	if local == nil {
		t.Fatalf("列表里没有本机 runner %s：%+v", localID, runners)
	}
	claudeVersion, found := e2eAgentVersion(local, "claude-code")
	if !found {
		t.Fatalf("本机 runner 的状态里没有 claude-code：%+v", local)
	}
	if claudeVersion == "" {
		t.Fatalf("本机 claude 的版本是空的（探测没读到真实二进制）：%+v", local)
	}
	e2eSay(t, "探测到 claude-code = %q（系统那份是 %q）", claudeVersion, systemVersion)
	if codexVersion, found := e2eAgentVersion(local, "codex"); found {
		e2eSay(t, "探测到 codex = %q", codexVersion)
		report["codexVersion"] = codexVersion
	}
	report["claudeVersionBefore"] = claudeVersion

	// ── 3. 管理页数据端点（装之前）─────────────────────────────────────
	var before e2eRunnerAgents
	if status := e2eRequest(t, client, http.MethodGet, api("/api/runners/"+localID+"/agents"), nil, &before); status != http.StatusOK {
		t.Fatalf("GET /api/runners/%s/agents 返回 %d", localID, status)
	}
	if !before.ProbeOK {
		t.Fatalf("探测失败了：%s", before.ProbeError)
	}
	if before.Runtime == nil || !before.Runtime.Installed {
		t.Fatalf("运行时状态不对：%+v", before.Runtime)
	}
	e2eSay(t, "装之前：运行时 origin=%s version=%s npm=%s，工具 %d 个",
		before.Runtime.Origin, before.Runtime.Version, before.Runtime.NpmVersion, len(before.Items))
	if before.Runtime.Origin != "system" {
		t.Fatalf("还没装托管运行时，origin 应当是 system，实际 %q", before.Runtime.Origin)
	}
	if before.Runtime.NpmVersion == "" {
		t.Fatal("系统 npm 版本是空的（这会让后面的安装计划判错）")
	}
	claudeItem := e2eFindItem(before.Items, "claude-code")
	if claudeItem == nil {
		t.Fatalf("items 里没有 claude-code：%+v", before.Items)
	}
	if !claudeItem.Installed || !claudeItem.Ready {
		t.Fatalf("系统既有的 claude 应当报 installed+ready：%+v", claudeItem)
	}
	if !claudeItem.InstallSupported {
		t.Fatalf("claude-code 应当可安装：%+v", claudeItem)
	}
	report["runtimeBefore"] = before.Runtime
	report["claudeItemBefore"] = claudeItem

	// ── 4. 运行时版本清单（真联网）─────────────────────────────────────
	var runtimeCatalog struct {
		Source   string `json:"source"`
		Versions []struct {
			Version string `json:"version"`
			LTS     string `json:"lts"`
		} `json:"versions"`
	}
	if status := e2eRequest(t, client, http.MethodGet, api("/api/runtimes/catalog"), nil, &runtimeCatalog); status != http.StatusOK {
		t.Fatalf("GET /api/runtimes/catalog 返回 %d（网络或镜像不可达？）", status)
	}
	if len(runtimeCatalog.Versions) == 0 {
		t.Fatalf("运行时清单是空的（源：%s）", runtimeCatalog.Source)
	}
	e2eSay(t, "可安装的 Node 版本：%d 个，最新 LTS = %s", len(runtimeCatalog.Versions), runtimeCatalog.Versions[0].Version)
	report["runtimeCatalog"] = runtimeCatalog

	// ── 5. 真实安装托管 Node ──────────────────────────────────────────
	installStarted := time.Now()
	var runtimeInstall e2eInstallResult
	if status := e2eRequest(t, client, http.MethodPost, api("/api/runners/"+localID+"/runtime/install"),
		map[string]any{"version": "lts"}, &runtimeInstall); status != http.StatusOK {
		t.Fatalf("安装托管运行时返回 %d", status)
	}
	e2eSay(t, "运行时装好了：%s（耗时 %.1fs），binary=%s", runtimeInstall.Version, time.Since(installStarted).Seconds(), runtimeInstall.BinaryPath)
	if runtimeInstall.Version == "" {
		t.Fatal("运行时安装返回了空版本")
	}
	nodeBinary := managedNodeBinary(root)
	if !fileExists(nodeBinary) {
		t.Fatalf("托管 node 不在预期位置：%s", nodeBinary)
	}
	if got := runVersionCommand(ctx, nodeBinary, "--version"); got != runtimeInstall.Version {
		t.Fatalf("托管 node 报的版本 %q 与登记值 %q 不一致", got, runtimeInstall.Version)
	}
	npmCommand := managedNpmCommand(root)
	if !fileExists(npmCommand) {
		t.Fatalf("托管 npm 不在预期位置：%s", npmCommand)
	}
	npmVersion := runVersionCommand(ctx, npmCommand, "--version")
	if npmVersion == "" {
		t.Fatalf("托管 npm 跑不起来：%s", npmCommand)
	}
	e2eSay(t, "托管 node=%s npm=%s（%s）", runtimeInstall.Version, npmVersion, npmCommand)
	report["managedRuntime"] = map[string]any{
		"version": runtimeInstall.Version, "node": nodeBinary, "npm": npmCommand,
		"npmVersion": npmVersion, "prefix": runtimeInstall.Prefix,
		"elapsedSeconds": time.Since(installStarted).Seconds(),
	}

	// 装完之后 origin 必须变成 managed —— 托管优先于系统。
	var afterRuntime e2eRunnerAgents
	if status := e2eRequest(t, client, http.MethodGet, api("/api/runners/"+localID+"/agents"), nil, &afterRuntime); status != http.StatusOK {
		t.Fatalf("装完运行时后读状态返回 %d", status)
	}
	if afterRuntime.Runtime == nil || afterRuntime.Runtime.Origin != "managed" {
		t.Fatalf("装完托管运行时后 origin 应当是 managed：%+v", afterRuntime.Runtime)
	}
	if len(afterRuntime.Runtime.MeetsMinimumFor) == 0 {
		t.Fatalf("装了 %s 的运行时却没有任何工具满足最低版本：%+v", afterRuntime.Runtime.Version, afterRuntime.Runtime)
	}
	e2eSay(t, "运行时 origin=managed，满足最低版本的工具有：%v", afterRuntime.Runtime.MeetsMinimumFor)

	// ── 6. 安全前置：安装计划必须落在托管前缀 ──────────────────────────
	//
	// 这道断言是**保护用户的**：计划若回落到系统 npm，下一步的安装会覆盖这台机器上
	// 既有的那份 claude。宁可在这一步失败，也不去动用户的安装。
	plan, err := server.resolveAgentInstallPlan(ctx, localID, "claude-code")
	if err != nil {
		t.Fatalf("解析安装计划失败：%v", err)
	}
	if plan.Kind != installKindNpmManaged {
		t.Fatalf("安装计划应当是 %s，实际 %s（prefix=%q）—— 继续下去会动到系统那份安装，已中止",
			installKindNpmManaged, plan.Kind, plan.Prefix)
	}
	if !strings.HasPrefix(filepath.Clean(plan.Prefix), filepath.Clean(root)) {
		t.Fatalf("安装前缀 %q 不在托管工具链 %q 之内", plan.Prefix, root)
	}
	e2eSay(t, "安装计划：kind=%s prefix=%s runtime=%s", plan.Kind, plan.Prefix, plan.RuntimeVersion)
	report["plan"] = map[string]any{"kind": plan.Kind, "prefix": plan.Prefix, "runtime": plan.RuntimeVersion}

	// ── 7. 挑一个"确定与系统那版不同"的版本来装 ─────────────────────────
	//
	// 目的是让下一步能**证明**解析器真的换了生效路径：若装出来的版本与系统那版相同，
	// 探测报出同样的数字就无法区分"走了托管"还是"还在读系统那份"。
	latest, stable := e2eNpmVersions(t, ctx, claudeEntry.NpmPackage)
	pinned := ""
	for index := len(stable) - 1; index >= 0; index-- {
		candidate := stable[index]
		if candidate != latest && candidate != systemVersion && candidate != claudeVersion {
			pinned = candidate
			break
		}
	}
	if pinned == "" {
		t.Fatalf("找不到一个与系统版本（%s）不同的历史版本", systemVersion)
	}
	if _, err := updateAvailableFrom(pinned, latest); err != nil {
		t.Fatalf("挑出来的版本 %q 或最新版 %q 不是合法 semver：%v", pinned, latest, err)
	}
	e2eSay(t, "registry：latest=%s，将先装 %s，再升级到 %s", latest, pinned, latest)
	report["npmVersions"] = map[string]any{"latest": latest, "pinned": pinned, "stableCount": len(stable)}

	// ── 8. 真实安装 CLI（钉在 pinned）──────────────────────────────────
	installStarted = time.Now()
	var cliInstall e2eInstallResult
	if status := e2eRequest(t, client, http.MethodPost, api("/api/runners/"+localID+"/agents/claude-code/install"),
		map[string]any{"version": pinned}, &cliInstall); status != http.StatusOK {
		t.Fatalf("安装 claude-code@%s 返回 %d", pinned, status)
	}
	if !cliInstall.Success {
		t.Fatalf("安装返回了 success=false：%+v", cliInstall)
	}
	e2eSay(t, "CLI 装好了：version=%s kind=%s binary=%s（耗时 %.1fs）",
		cliInstall.Version, cliInstall.InstallKind, cliInstall.BinaryPath, time.Since(installStarted).Seconds())
	if cliInstall.Version != pinned {
		t.Fatalf("装的是 %s，登记/自检出来却是 %q", pinned, cliInstall.Version)
	}
	if cliInstall.InstallKind != installKindNpmManaged {
		t.Fatalf("安装方式应当是 %s，实际 %q", installKindNpmManaged, cliInstall.InstallKind)
	}
	if !strings.HasPrefix(filepath.Clean(cliInstall.BinaryPath), filepath.Clean(root)) {
		t.Fatalf("装出来的二进制 %q 不在托管工具链 %q 之内", cliInstall.BinaryPath, root)
	}
	if !fileExists(cliInstall.BinaryPath) {
		t.Fatalf("登记的可执行文件不存在：%s", cliInstall.BinaryPath)
	}
	if got := agentVersionFromOutput(runVersionCommand(ctx, cliInstall.BinaryPath, claudeEntry.VersionArgs...)); got != pinned {
		t.Fatalf("直接执行 %s 得到 %q，期望 %q", cliInstall.BinaryPath, got, pinned)
	}
	report["cliInstall"] = cliInstall

	// ── 9. 装完必须"真的生效"：解析器要改走托管那份 ────────────────────
	//
	// 这是 docs/42 §14.B 的核心：装到托管前缀之后，若 Ready()/Version()/Run() 还在读
	// 旧路径，那这次安装对用户等于没发生。判据用"版本必须变成 pinned"——
	// 因为 pinned 与系统那版不同，报出 pinned 只可能是走了托管那份。
	var afterInstall []e2eRunner
	if status := e2eRequest(t, client, http.MethodGet, api("/api/runners"), nil, &afterInstall); status != http.StatusOK {
		t.Fatalf("装完后读列表返回 %d", status)
	}
	local = e2eFindRunner(afterInstall, localID)
	managedVersion, _ := e2eAgentVersion(local, "claude-code")
	if managedVersion != pinned {
		t.Fatalf("装完之后探测到的版本是 %q，期望 %q —— 说明运行的仍是旧路径（装了个没用的）", managedVersion, pinned)
	}
	if got := agentVersionFromOutput(server.runner.Version(ctx)); got != pinned {
		t.Fatalf("runner.Version() 报 %q，期望 %q —— 路径解析器没被真正消费", got, pinned)
	}
	if resolved := server.agentBinary("claude-code"); !strings.HasPrefix(filepath.Clean(resolved), filepath.Clean(root)) {
		t.Fatalf("解析出来的路径 %q 不在托管工具链里（还在读系统那份？）", resolved)
	}
	e2eSay(t, "装完生效：探测/runner.Version()/解析器三处都指向托管的 %s", pinned)
	report["claudeVersionAfterInstall"] = managedVersion

	var afterInstallView e2eRunnerAgents
	if status := e2eRequest(t, client, http.MethodGet, api("/api/runners/"+localID+"/agents"), nil, &afterInstallView); status != http.StatusOK {
		t.Fatalf("装完后读管理页数据返回 %d", status)
	}
	installedItem := e2eFindItem(afterInstallView.Items, "claude-code")
	if installedItem == nil {
		t.Fatal("装完后 items 里没有 claude-code")
	}
	if installedItem.InstallKindUsed != installKindNpmManaged {
		t.Fatalf("管理页看到的安装方式是 %q，期望 %q", installedItem.InstallKindUsed, installKindNpmManaged)
	}
	if !strings.HasPrefix(filepath.Clean(installedItem.BinaryPath), filepath.Clean(root)) {
		t.Fatalf("管理页显示的安装位置 %q 不在托管工具链里", installedItem.BinaryPath)
	}
	if !installedItem.AutoUpdatable {
		t.Fatalf("本机平台装的工具应当可应用内升级：%+v", installedItem)
	}
	report["claudeItemAfterInstall"] = installedItem

	// ── 10. 检查更新（真联网查 registry）───────────────────────────────
	var check e2eUpdateCheck
	if status := e2eRequest(t, client, http.MethodPost, api("/api/runners/"+localID+"/agents/claude-code/check-update"), nil, &check); status != http.StatusOK {
		t.Fatalf("check-update 返回 %d", status)
	}
	if check.Error != "" {
		t.Fatalf("check-update 报错：%s", check.Error)
	}
	if !check.UpdateAvailable {
		t.Fatalf("装了 %s，registry 上最新是 %s，应当报有更新：%+v", pinned, latest, check)
	}
	if check.LatestVersion != latest {
		t.Fatalf("报的最新版是 %q，registry 上是 %q", check.LatestVersion, latest)
	}
	if check.CurrentVersion != pinned {
		t.Fatalf("报的当前版本是 %q，期望 %q", check.CurrentVersion, pinned)
	}
	if !check.AutoUpdatable {
		t.Fatal("本机平台装的工具应当报 autoUpdatable=true")
	}
	e2eSay(t, "检查更新：%s → %s（autoUpdatable=%v）", check.CurrentVersion, check.LatestVersion, check.AutoUpdatable)
	report["updateCheck"] = check

	// ── 11. 真实升级 ───────────────────────────────────────────────────
	updateStarted := time.Now()
	var updated e2eUpdateResult
	if status := e2eRequest(t, client, http.MethodPost, api("/api/runners/"+localID+"/agents/claude-code/update"), nil, &updated); status != http.StatusOK {
		t.Fatalf("升级返回 %d", status)
	}
	if !updated.Success {
		t.Fatalf("升级成功但 success=false：%+v", updated)
	}
	if updated.PreviousVersion != pinned {
		t.Fatalf("升级前的版本报成 %q，期望 %q", updated.PreviousVersion, pinned)
	}
	if updated.CurrentVersion != latest {
		t.Fatalf("升级后版本是 %q，期望 %q", updated.CurrentVersion, latest)
	}
	e2eSay(t, "升级：%s → %s（耗时 %.1fs）", updated.PreviousVersion, updated.CurrentVersion, time.Since(updateStarted).Seconds())
	report["update"] = updated

	// 升级后必须真的可用：直接问那个二进制，而不是只看登记表。
	if got := agentVersionFromOutput(runVersionCommand(ctx, cliInstall.BinaryPath, claudeEntry.VersionArgs...)); got != latest {
		t.Fatalf("升级后直接执行 %s 得到 %q，期望 %q", cliInstall.BinaryPath, got, latest)
	}
	var afterUpdate []e2eRunner
	if status := e2eRequest(t, client, http.MethodGet, api("/api/runners"), nil, &afterUpdate); status != http.StatusOK {
		t.Fatalf("升级后读列表返回 %d", status)
	}
	local = e2eFindRunner(afterUpdate, localID)
	if version, _ := e2eAgentVersion(local, "claude-code"); version != latest {
		t.Fatalf("升级后探测到的版本是 %q，期望 %q", version, latest)
	}
	var postCheck e2eUpdateCheck
	if status := e2eRequest(t, client, http.MethodPost, api("/api/runners/"+localID+"/agents/claude-code/check-update"), nil, &postCheck); status != http.StatusOK {
		t.Fatalf("升级后 check-update 返回 %d", status)
	}
	if postCheck.UpdateAvailable {
		t.Fatalf("已经升到最新（%s）却仍报有更新：%+v", latest, postCheck)
	}
	e2eSay(t, "升级后自检：二进制报 %s，探测报 %s，check-update 说已是最新", latest, latest)
	report["claudeVersionAfterUpdate"] = latest

	// ── 12. 审计：三种写操作都要留痕 ──────────────────────────────────
	var audit e2eAuditList
	if status := e2eRequest(t, client, http.MethodGet, api("/api/runners/"+localID+"/install-audit"), nil, &audit); status != http.StatusOK {
		t.Fatalf("读审计返回 %d", status)
	}
	seen := map[string]e2eAuditItem{}
	for _, item := range audit.Items {
		if item.Result == "succeeded" {
			if _, exists := seen[item.Action]; !exists {
				seen[item.Action] = item
			}
		}
	}
	for _, action := range []string{"install-runtime", "install", "update"} {
		if _, ok := seen[action]; !ok {
			t.Fatalf("审计里没有成功的 %s 记录：%+v", action, audit.Items)
		}
	}
	if item := seen["update"]; item.FromVersion != pinned || item.ToVersion != latest {
		t.Fatalf("升级审计的 from/to 不对：%+v", item)
	}
	if item := seen["install"]; item.ToVersion != pinned {
		t.Fatalf("安装审计的 to 应当是 %s：%+v", pinned, item)
	}
	e2eSay(t, "审计：install-runtime / install / update 三条都在（%d 条记录）", len(audit.Items))
	report["audit"] = audit.Items

	// ── 13. 未授权的主机必须被服务端拒绝（不是靠界面不给按钮）─────────
	if status := e2eRequest(t, client, http.MethodPost, api("/api/runners/ssh-does-not-exist/agents/claude-code/install"),
		map[string]any{}, nil); status != http.StatusForbidden {
		t.Fatalf("未授权的远端安装应当返回 403，实际 %d", status)
	}
	if status := e2eRequest(t, client, http.MethodPost, api("/api/runners/ssh-does-not-exist/runtime/install"),
		map[string]any{}, nil); status != http.StatusForbidden {
		t.Fatalf("未授权的远端运行时装应当返回 403，实际 %d", status)
	}
	e2eSay(t, "未授权主机：两个安装入口都返回 403")

	// ── 14. 收尾：用户原本那份安装没被动过 ────────────────────────────
	afterPath, err := exec.LookPath(claudeEntry.CommandName)
	if err != nil {
		t.Fatalf("系统那份 %s 不见了：%v", claudeEntry.CommandName, err)
	}
	afterSystemVersion := agentVersionFromOutput(runVersionCommand(ctx, afterPath, claudeEntry.VersionArgs...))
	if !sameCleanPath(afterPath, systemPath) || afterSystemVersion != systemVersion {
		t.Fatalf("系统那份安装被改动了：%s@%s → %s@%s", systemPath, systemVersion, afterPath, afterSystemVersion)
	}
	e2eSay(t, "系统既有的那份完好：%s @ %s", afterSystemVersion, afterPath)
	report["systemAfter"] = map[string]any{"path": afterPath, "version": afterSystemVersion}
	report["verdict"] = "安装、检测、升级三项均真实可用（本机）"
}

func e2eCatalogNames(entries []e2eCatalogEntry) string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, fmt.Sprintf("%s/%s", entry.ID, entry.Name))
	}
	return strings.Join(names, ", ")
}

// TestRealE2EInstallCodexAndRequestGuards 补上第一支用例没覆盖的两件事。
//
//  1. 第二个工具（Codex）走的是**另一条分发包路径** —— `@openai/codex` 本体几乎是空的，
//     真正的可执行文件在平台可选依赖里。目录里给它写的 BinFile 与版本号提取只有真的
//     装一次才能确认；写错了的表现是"装成功但自检失败"，而用户看到的是含糊的执行错误。
//  2. 请求边界在**真实 HTTP 上**是否真的守住：非法版本号、未知工具、未授权主机，
//     以及"并发时同一把闸门"。
//
// 它复用第一支用例装好的托管运行时（同一台机器上跑两次不必再下 30MB）；
// 若单独运行则会自己把运行时装上。
func TestRealE2EInstallCodexAndRequestGuards(t *testing.T) {
	if os.Getenv(e2eEnv) != "1" {
		t.Skip("设置 MILEVIA_E2E=1 才运行真实端到端（会联网并真的安装）")
	}
	ctx := context.Background()

	scratch, err := filepath.Abs(filepath.Join("..", "..", "..", "..", ".tmp", "e2e-run"))
	if err != nil {
		t.Fatalf("解析 scratch 目录失败：%v", err)
	}
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatalf("创建 scratch 失败：%v", err)
	}
	root := filepath.Join(scratch, "toolchain")
	t.Setenv(toolchainRootEnv, root)
	// 只清 DB，**不清工具链**：托管运行时是第一支用例留下的，这里直接复用。
	if err := os.RemoveAll(filepath.Join(scratch, "e2e2.db")); err != nil {
		t.Fatalf("清理 DB 失败：%v", err)
	}

	server := e2eServer(t, scratch, "e2e2.db")
	defer server.Close()
	httpServer := httptest.NewServer(server.routes())
	defer httpServer.Close()
	client := &http.Client{Timeout: 30 * time.Minute}
	api := func(path string) string { return httpServer.URL + path }
	localID := server.localRunnerID()

	// ── 1. 托管运行时必须在位（单独跑这支用例时会自己装）────────────────
	if !fileExists(managedNodeBinary(root)) {
		e2eSay(t, "托管运行时不在位，先装一次")
		var installed e2eInstallResult
		if status := e2eRequest(t, client, http.MethodPost, api("/api/runners/"+localID+"/runtime/install"),
			map[string]any{"version": "lts"}, &installed); status != http.StatusOK {
			t.Fatalf("安装托管运行时返回 %d", status)
		}
	}
	nodeVersion := runVersionCommand(ctx, managedNodeBinary(root), "--version")
	if nodeVersion == "" {
		t.Fatalf("托管 node 跑不起来：%s", managedNodeBinary(root))
	}
	e2eSay(t, "复用托管运行时 node=%s（%s）", nodeVersion, root)

	// ── 2. 安全前置：codex 的安装计划也必须落在托管前缀 ────────────────
	codexEntry, ok := agentByID("codex")
	if !ok {
		t.Fatal("目录里没有 codex")
	}
	plan, err := server.resolveAgentInstallPlan(ctx, localID, "codex")
	if err != nil {
		t.Fatalf("解析 codex 的安装计划失败：%v", err)
	}
	if plan.Kind != installKindNpmManaged || !strings.HasPrefix(filepath.Clean(plan.Prefix), filepath.Clean(root)) {
		t.Fatalf("codex 的安装计划不对（kind=%s prefix=%q）—— 继续会动到系统那份，已中止", plan.Kind, plan.Prefix)
	}

	// ── 3. 真实安装 codex ──────────────────────────────────────────────
	latest, err := latestAgentVersion(ctx, "codex")
	if err != nil {
		t.Fatalf("查 codex 最新版本失败：%v", err)
	}
	installStarted := time.Now()
	var codexInstall e2eInstallResult
	if status := e2eRequest(t, client, http.MethodPost, api("/api/runners/"+localID+"/agents/codex/install"),
		map[string]any{"version": "latest"}, &codexInstall); status != http.StatusOK {
		t.Fatalf("安装 codex 返回 %d", status)
	}
	e2eSay(t, "codex 装好了：version=%s kind=%s binary=%s（耗时 %.1fs，registry 最新 %s）",
		codexInstall.Version, codexInstall.InstallKind, codexInstall.BinaryPath,
		time.Since(installStarted).Seconds(), latest)
	if !codexInstall.Success {
		t.Fatalf("安装返回 success=false：%+v", codexInstall)
	}
	if codexInstall.Version != latest {
		t.Fatalf("装的是 latest（%s），自检出来是 %q —— 目录里的 BinFile 或版本号提取可能与实际不符",
			latest, codexInstall.Version)
	}
	if codexInstall.InstallKind != installKindNpmManaged {
		t.Fatalf("安装方式应当是 %s，实际 %q", installKindNpmManaged, codexInstall.InstallKind)
	}
	if !strings.HasPrefix(filepath.Clean(codexInstall.BinaryPath), filepath.Clean(root)) || !fileExists(codexInstall.BinaryPath) {
		t.Fatalf("登记的可执行文件 %q 不在托管工具链内或不存在", codexInstall.BinaryPath)
	}
	if got := agentVersionFromOutput(runVersionCommand(ctx, codexInstall.BinaryPath, codexEntry.VersionArgs...)); got != latest {
		t.Fatalf("直接执行 %s 得到 %q，期望 %q", codexInstall.BinaryPath, got, latest)
	}

	// ── 4. 装完必须真的生效（探测与解析器都要指向托管那份）────────────
	var runners []e2eRunner
	if status := e2eRequest(t, client, http.MethodGet, api("/api/runners"), nil, &runners); status != http.StatusOK {
		t.Fatalf("读列表返回 %d", status)
	}
	local := e2eFindRunner(runners, localID)
	codexVersion, _ := e2eAgentVersion(local, "codex")
	if codexVersion != latest {
		t.Fatalf("装完 codex 后探测到的是 %q，期望 %q", codexVersion, latest)
	}
	if resolved := server.agentBinary("codex"); !strings.HasPrefix(filepath.Clean(resolved), filepath.Clean(root)) {
		t.Fatalf("codex 的解析路径 %q 不在托管工具链里", resolved)
	}
	e2eSay(t, "codex 装完生效：探测与解析器都指向托管那份（%s）", latest)

	// 平台装的（npm 类）升级走 npm 重装，因此界面必须能从管理页数据里看出这一档。
	var view e2eRunnerAgents
	if status := e2eRequest(t, client, http.MethodGet, api("/api/runners/"+localID+"/agents"), nil, &view); status != http.StatusOK {
		t.Fatalf("读管理页数据返回 %d", status)
	}
	codexItem := e2eFindItem(view.Items, "codex")
	if codexItem == nil {
		t.Fatal("items 里没有 codex")
	}
	if codexItem.InstallKindUsed != installKindNpmManaged {
		t.Fatalf("管理页看到的 codex 安装方式是 %q", codexItem.InstallKindUsed)
	}
	if !codexItem.AutoUpdatable {
		t.Fatalf("本机平台装的 codex 应当可应用内升级：%+v", codexItem)
	}

	// ── 5. 用户自己那份（Prefix="" 那条分支）只读地验一遍 ───────────────
	//
	// 系统安装那条路的判据是"按 PATH 找 + 真的能执行"。这里不真的重装它，
	// 但要用真实环境验证这条分支判得对 —— 它是唯一一条会去动用户已有安装的路。
	systemNpm, err := exec.LookPath("npm")
	if err != nil {
		t.Fatalf("PATH 上没有 npm：%v", err)
	}
	systemNpmVersion := nodeVersionNearNpm(ctx, systemNpm)
	if systemNpmVersion == "" {
		t.Fatal("从系统 npm 推断不出它背后的 Node 版本（最低版本闸门会因此失效）")
	}
	claudeEntry, _ := agentByID("claude-code")
	systemPlan := agentInstallPlan{NpmPath: systemNpm, Prefix: "", Kind: installKindNpmSystem}
	systemBinary, err := server.verifyAgentInstall(ctx, systemPlan, claudeEntry)
	if err != nil {
		t.Fatalf("系统安装那条分支的自检失败：%v", err)
	}
	lookedUp, err := exec.LookPath(claudeEntry.CommandName)
	if err != nil {
		t.Fatalf("PATH 上找不到 %s：%v", claudeEntry.CommandName, err)
	}
	if !sameCleanPath(systemBinary, lookedUp) {
		t.Fatalf("系统分支自检出来的路径是 %q，PATH 上是 %q", systemBinary, lookedUp)
	}
	e2eSay(t, "系统分支（prefix 为空）判据正确：npm=%s node=%s，自检命中 %s", systemNpm, systemNpmVersion, systemBinary)

	// ── 6. 边界：非法版本号必须被拒，且不能损坏已有登记 ─────────────────
	for _, bogus := range []string{"1.2.3; rm -rf /", "../../etc/passwd", "latest; echo hi", "v1.2.3"} {
		status, _, err := e2eRequestSafe(client, http.MethodPost, api("/api/runners/"+localID+"/agents/codex/install"),
			map[string]any{"version": bogus})
		if err != nil {
			t.Fatalf("发请求失败：%v", err)
		}
		if status == http.StatusOK {
			t.Fatalf("非法版本号 %q 被接受了", bogus)
		}
		e2eSay(t, "非法版本号 %q 被拒（HTTP %d）", bogus, status)
	}
	recorded, has, err := server.recordedInstallation(ctx, localID, "codex")
	if err != nil || !has {
		t.Fatalf("拒绝非法请求之后登记项不见了：has=%v err=%v", has, err)
	}
	if recorded.Version != latest {
		t.Fatalf("非法请求把登记版本改成了 %q，期望仍是 %q", recorded.Version, latest)
	}

	// ── 7. 边界：未知工具 / 未知主机 ───────────────────────────────────
	if status := e2eRequest(t, client, http.MethodPost, api("/api/runners/"+localID+"/agents/not-a-tool/install"),
		map[string]any{}, nil); status != http.StatusNotFound {
		t.Fatalf("未知工具的安装入口应当 404，实际 %d", status)
	}
	if status := e2eRequest(t, client, http.MethodPost, api("/api/runners/"+localID+"/agents/not-a-tool/check-update"),
		nil, nil); status != http.StatusNotFound {
		t.Fatalf("未知工具的更新检查应当 404，实际 %d", status)
	}
	if status := e2eRequest(t, client, http.MethodGet, api("/api/runners/ssh-nope/agents"), nil, nil); status != http.StatusNotFound {
		t.Fatalf("未知主机的状态端点应当 404，实际 %d", status)
	}
	if status := e2eRequest(t, client, http.MethodPost, api("/api/runners/"+localID+"/remote-install/grant"),
		nil, nil); status != http.StatusBadRequest {
		t.Fatalf("对本机授权应当 400（本机不需要授权），实际 %d", status)
	}
	e2eSay(t, "边界：未知工具 404、未知主机 404、对本机授权 400")

	// 旧路径（前端尚未迁完的薄委托）必须仍然可用，否则对话页的更新按钮会静默失效。
	var legacyCheck e2eUpdateCheck
	if status := e2eRequest(t, client, http.MethodPost, api("/api/runners/"+localID+"/claude/check-update"), nil, &legacyCheck); status != http.StatusOK {
		t.Fatalf("旧的 /claude/check-update 返回 %d（薄委托坏了会让对话页的更新按钮静默失效）", status)
	}
	if legacyCheck.Error != "" {
		t.Fatalf("旧的 /claude/check-update 报错：%s", legacyCheck.Error)
	}
	if status := e2eRequest(t, client, http.MethodGet, api("/api/runners/"+localID+"/status"), nil, nil); status != http.StatusOK {
		t.Fatalf("旧的 /status 返回 %d", status)
	}
	e2eSay(t, "旧路径仍可用：/claude/check-update（当前 %s，最新 %s）与 /status",
		legacyCheck.CurrentVersion, legacyCheck.LatestVersion)

	// ── 8. 并发：CLI 安装进行中时，这台机器上别的写操作必须被挡住 ──────
	//
	// 这是"两把闸门其实是同一把"那条修复的真实版验证：装运行时与装 CLI 若各占
	// 一把锁，二者会同时开跑（两个 npm 写同一个 prefix，且 mv node 会换掉
	// CLI 正在用的那个 node）。
	firstDone := make(chan int, 1)
	go func() {
		status, _, _ := e2eRequestSafe(client, http.MethodPost, api("/api/runners/"+localID+"/agents/codex/update"), nil)
		firstDone <- status
	}()
	// 等第一个请求确实占住闸门（它要跑十几秒，而这一步只等 2s）。
	time.Sleep(2 * time.Second)
	runtimeStatus, _, err := e2eRequestSafe(client, http.MethodPost, api("/api/runners/"+localID+"/runtime/install"),
		map[string]any{"version": "lts"})
	if err != nil {
		t.Fatalf("发并发请求失败：%v", err)
	}
	if runtimeStatus != http.StatusConflict {
		t.Fatalf("CLI 升级进行中时装运行时应当 409，实际 %d —— 说明两个入口没共用同一把闸门", runtimeStatus)
	}
	agentStatus, _, err := e2eRequestSafe(client, http.MethodPost, api("/api/runners/"+localID+"/agents/claude-code/install"),
		map[string]any{})
	if err != nil {
		t.Fatalf("发并发请求失败：%v", err)
	}
	if agentStatus != http.StatusConflict {
		t.Fatalf("同一 Runner 上并发的第二个 CLI 安装应当 409，实际 %d", agentStatus)
	}
	first := <-firstDone
	if first != http.StatusOK {
		t.Fatalf("先发起的 codex 升级返回 %d", first)
	}
	e2eSay(t, "并发：升级进行中，装运行时=%d、装另一个 CLI=%d，先发起的那个 =%d", runtimeStatus, agentStatus, first)

	// 闸门必须已经释放（否则这台机器此后什么都装不了）。
	if status := e2eRequest(t, client, http.MethodPost, api("/api/runners/"+localID+"/agents/codex/check-update"), nil, nil); status != http.StatusOK {
		t.Fatalf("闸门没有释放：check-update 返回 %d", status)
	}
	e2eSay(t, "闸门已释放（随后的 check-update 正常）")
}
