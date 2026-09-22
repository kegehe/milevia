package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// 托管 Node 运行时的探测与安装。
//
// 与工具目录的关系：**Node 不是目录里的工具**，它是工具的**前置运行时**
// （目录里每个工具的 Requires 都指向它）。所以它单独有一条探测与安装路径，
// 但仍然复用同一套存放位置（`agent_installations`，agent_id = "node"）与同一把
// 并发闸门（`runnerUpdating` 的 `agentID = "node"` 槽位），因此天然与 CLI 的
// 安装/升级互斥 —— 同一台机器上不会有两个 npm 同时写同一个 prefix。

// runtimeAgentID 是运行时在登记表与并发闸门里占用的槽位。
const runtimeAgentID = "node"

// runtimeStatus 是给界面的运行时状态。
type runtimeStatus struct {
	ID string `json:"id"`
	// Installed 表示**找得到可用的 node**（托管或系统）。
	Installed bool   `json:"installed"`
	Version   string `json:"version"`
	// NpmVersion 为空表示找到了 node 但没有可用的 npm（那种环境装不了 CLI）。
	NpmVersion string `json:"npmVersion"`
	NpmPath    string `json:"npmPath,omitempty"`
	// Origin：system（用户自己的）| managed（平台装的）| none。
	Origin string `json:"origin"`
	// ManagedPath 是托管工具链目录（origin=managed 时非空）。
	ManagedPath string `json:"managedPath,omitempty"`
	// MeetsMinimumFor 列出"当前运行时版本够用"的工具 ID；不够的工具不出现在这里。
	MeetsMinimumFor []string `json:"meetsMinimumFor"`
	// SystemNpmPath / SystemNpmVersion / SystemNodeVersion 是目标环境里**系统**那套的
	// 事实。Origin 只说"当前该用哪套"，而安装要按 installKind 选，所以两套都得带出来。
	SystemNpmPath     string `json:"systemNpmPath,omitempty"`
	SystemNpmVersion  string `json:"systemNpmVersion,omitempty"`
	SystemNodeVersion string `json:"systemNodeVersion,omitempty"`
	// InstallSupported / InstallBlockedReason 说明能不能装、不能装是为什么。
	// 三档不可安装（架构不支持 / 平台不支持 / 已被占用）文案各不相同。
	InstallSupported     bool   `json:"installSupported"`
	InstallBlockedReason string `json:"installBlockedReason,omitempty"`
	LatestVersion        string `json:"latestVersion,omitempty"`
	UpdateAvailable      bool   `json:"updateAvailable"`
}

// probeRuntime 探测当前环境的 Node 运行时。
//
// 顺序：托管工具链优先于系统 PATH。为什么反直觉地让托管优先 —— 托管是我们自己装的、
// 版本可控、与登记表里的 CLI 安装位置同源；而系统那个可能版本过旧（CLI 要求 >=18）。
// 若反过来，用户机器上一个 Node 12 会把平台自己装好的运行时顶掉。
func (s *Server) probeRuntime(ctx context.Context) runtimeStatus {
	status := runtimeStatus{ID: runtimeAgentID, Origin: "none", InstallSupported: true}

	root, rootErr := managedToolchainRoot()
	if rootErr != nil {
		status.InstallSupported = false
		status.InstallBlockedReason = rootErr.Error()
	}
	if rootErr == nil {
		if binary := managedNodeBinary(root); fileExists(binary) {
			status.Origin = "managed"
			status.ManagedPath = root
			status.Version = runVersionCommand(ctx, binary, "--version")
			npm := managedNpmCommand(root)
			if fileExists(npm) {
				status.NpmPath = npm
				status.NpmVersion = runVersionCommand(ctx, npm, "--version")
			}
		}
	}
	if status.Origin == "none" {
		if path, err := exec.LookPath("node"); err == nil {
			status.Origin = "system"
			status.Version = runVersionCommand(ctx, path, "--version")
			if npm, err := exec.LookPath("npm"); err == nil {
				status.NpmPath = npm
				status.NpmVersion = runVersionCommand(ctx, npm, "--version")
			}
		}
	}
	status.Installed = status.Version != ""

	// 最低版本闸门：逐工具判断"当前运行时够不够"。判据来自目录，不写死。
	if status.Installed {
		for _, entry := range agentCatalog() {
			ok, err := runtimeMeetsMinimum(status.Version, entry.MinRuntimeVersion)
			if err == nil && ok {
				status.MeetsMinimumFor = append(status.MeetsMinimumFor, entry.ID)
			}
		}
	}

	// 能不能装：平台得支持。
	if status.InstallSupported && rootErr == nil {
		if _, err := nodePlatformKey(runtime.GOOS, runtime.GOARCH, localNodeLibc()); err != nil {
			status.InstallSupported = false
			status.InstallBlockedReason = err.Error()
		}
	}
	// 最新 LTS 只用于提示"有新版"，取不到不算错误（网络不可达时不影响本机可用性）。
	if s.runtimes != nil {
		if entries, err := s.runtimes.fetchNodeVersionIndex(ctx); err == nil {
			if latest, err := pickNodeVersion(entries, "lts"); err == nil {
				status.LatestVersion = latest.nodeVersion()
				current, err := parseSemver(strings.TrimPrefix(strings.TrimSpace(status.Version), "v"))
				if err == nil {
					if newest, err := parseSemver(status.LatestVersion); err == nil {
						status.UpdateAvailable = compareSemver(newest, current) > 0
					}
				}
			}
		}
	}
	return status
}

// runVersionCommand 执行 `<bin> --version` 取版本，失败返回空串。
//
// 剥掉前导 v：node 的 `--version` 输出是 `v24.21.0`，npm 的是 `10.8.2`。
func runVersionCommand(ctx context.Context, binary string, args ...string) string {
	probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, binary, args...)
	configureProcessGroup(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	lined := strings.TrimSpace(string(out))
	if index := strings.IndexByte(lined, '\n'); index >= 0 {
		lined = strings.TrimSpace(lined[:index])
	}
	return strings.TrimPrefix(lined, "v")
}

// installRuntimeRequest 是运行时安装请求。
type installRuntimeRequest struct {
	// Version：空或 "lts" 取最新 LTS；也可以是具体版本号。
	Version string `json:"version"`
}

// installManagedRuntime 下载并落地托管 Node 运行时。
//
// 全程**不提权**：解压到用户目录下的托管工具链里。失败时清理现场并保留旧版本，
// 不会留下一个半装的运行时。
func (s *Server) installManagedRuntime(ctx context.Context, request installRuntimeRequest) (agentInstallation, error) {
	root, err := managedToolchainRoot()
	if err != nil {
		return agentInstallation{}, err
	}
	return s.installManagedRuntimeAt(ctx, request, root)
}

// installManagedRuntimeAt 是安装的实际实现，落点由调用方给出。
//
// 把落点做成参数（而不是在里面算）只有一个目的：让整条链路可以在临时目录里
// 端到端跑一遍测试 —— 否则这条链路的验证只能靠真的往用户目录里装。
func (s *Server) installManagedRuntimeAt(ctx context.Context, request installRuntimeRequest, root string) (agentInstallation, error) {
	if s.runtimes == nil {
		return agentInstallation{}, errors.New("运行时管理器不可用")
	}
	platform, err := nodePlatformKey(runtime.GOOS, runtime.GOARCH, localNodeLibc())
	if err != nil {
		return agentInstallation{}, err
	}
	dist, err := s.runtimes.resolveDistribution(ctx, request.Version, platform)
	if err != nil {
		return agentInstallation{}, err
	}

	if err := os.MkdirAll(root, 0o755); err != nil {
		return agentInstallation{}, err
	}
	staging := filepath.Join(root, fmt.Sprintf("node.staging-%d", time.Now().UTC().UnixNano()))
	defer os.RemoveAll(staging)

	archivePath := filepath.Join(staging, dist.FileName)
	if err := s.runtimes.downloadVerified(ctx, dist, archivePath, nil); err != nil {
		return agentInstallation{}, err
	}
	payload := filepath.Join(staging, "payload")
	if err := os.MkdirAll(payload, 0o755); err != nil {
		return agentInstallation{}, err
	}
	if err := extractArchive(archivePath, dist.Format, payload); err != nil {
		return agentInstallation{}, err
	}
	if err := os.Remove(archivePath); err != nil && !os.IsNotExist(err) {
		return agentInstallation{}, err
	}

	// 先确认解出来的东西真的能跑，再让它替换现有的运行时。
	// 顺序不能反：替换完才发现跑不起来，用户就损失了一个可用的运行时。
	stagedBinary := stagedNodeBinary(payload)
	if !fileExists(stagedBinary) {
		return agentInstallation{}, fmt.Errorf("解压后没有找到 node 可执行文件（分发包结构可能变了）")
	}
	version := runVersionCommand(ctx, stagedBinary, "--version")
	if version == "" {
		return agentInstallation{}, fmt.Errorf("解压出的 node 无法执行")
	}

	target := filepath.Join(root, "node")
	previous := ""
	if fileExists(target) {
		previous = filepath.Join(root, fmt.Sprintf("node.previous-%d", time.Now().UTC().UnixNano()))
		if err := os.Rename(target, previous); err != nil {
			return agentInstallation{}, fmt.Errorf("暂存现有运行时失败：%w", err)
		}
	}
	if err := os.Rename(payload, target); err != nil {
		if previous != "" {
			_ = os.Rename(previous, target)
		}
		return agentInstallation{}, fmt.Errorf("替换运行时失败：%w", err)
	}
	if previous != "" {
		_ = os.RemoveAll(previous)
	}

	binary := managedNodeBinary(root)
	npmVersion := ""
	if npm := managedNpmCommand(root); fileExists(npm) {
		npmVersion = runVersionCommand(ctx, npm, "--version")
	}
	if npmVersion == "" {
		// node 能跑但没有 npm：装不了 CLI。如实报错，并把刚装的东西留下
		// （Node 本身可用，用户/我们都可以据此排查），但**不登记成可用运行时**。
		return agentInstallation{}, fmt.Errorf("Node %s 已解压，但随包的 npm 不可用；请检查该分发包", version)
	}

	installation := agentInstallation{
		RunnerID:    s.localRunnerID(),
		AgentID:     runtimeAgentID,
		BinaryPath:  binary,
		InstallKind: "managed-toolchain",
		// prefix 必须一起记：CLI 的安装位置由它决定，只有路径没有 prefix 时
		// "升级"会去找系统 npm 的全局位置。
		Prefix:  managedNpmGlobalPrefix(root),
		Version: version,
		Source:  "managed-install",
	}
	if err := s.recordAgentInstallation(ctx, installation); err != nil {
		return installation, err
	}
	return installation, nil
}

// stagedNodeBinary 见 runtime_manager.go（布局在两处必须一致，所以只有一份实现）。

// ── HTTP ───────────────────────────────────────────────────────────────────

// listRuntimeCatalog 是 GET /api/runtimes/catalog。
//
// 它只回答"能装哪些版本"。报错时如实返回错误，**不是**返回空列表 ——
// 空列表会被界面渲染成"没有可安装的版本"，那是把"读不到"说成了"没有"。
func (s *Server) listRuntimeCatalog(w http.ResponseWriter, r *http.Request) {
	if s.runtimes == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("运行时管理器不可用"))
		return
	}
	entries, err := s.runtimes.fetchNodeVersionIndex(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"source":   nodeRuntimeBaseURL(),
		"versions": nodeVersionOptions(entries, 6),
	})
}

// installRuntime 是 POST /api/runners/{runnerID}/runtime/install。
//
// 本机与跨端（WSL / SSH）走各自的实现，由 installRuntimeFor 分派：下载与校验在
// 本机做、解压与落点在目标环境做（见 runtime_install_cross.go 的分工说明）。
func (s *Server) installRuntime(w http.ResponseWriter, r *http.Request) {
	runnerID := chi.URLParam(r, "runnerID")
	// 与 installAgentFor 同一道闸门：接口层必须自己拒绝，不能依赖界面不给按钮。
	if !s.remoteInstallAllowed(r.Context(), runnerID) {
		writeError(w, http.StatusForbidden, fmt.Errorf("尚未授权在 %s 上安装；请先在该主机上确认一次", runnerID))
		return
	}
	var request installRuntimeRequest
	// 请求体可选：不带版本时按 LTS。
	if !decodeOptional(w, r, &request) {
		return
	}
	// 与 CLI 的安装/升级共用**同一把**闸门（beginAgentMaintenance）。
	//
	// 原先这里自己占一个 runnerUpdating[{runnerID,"node"}] 槽，看着像互斥，其实不是：
	// 那把闸门的另一半是 runnerUpdateExecuting[runnerID]，只有 beginAgentMaintenance
	// 会维护它。于是"装 Node"与"装 CLI"能同时开跑 —— 两个 npm 同时写同一个 prefix，
	// 而运行时的到位动作（mv node）会把 CLI 安装正在用的 node 换掉。
	release, ok := s.beginAgentMaintenance(w, r, runnerID, runtimeAgentID, "Node.js 运行时")
	if !ok {
		return
	}
	defer release()

	// 超时与取消口径与 CLI 的安装一致：用服务端自己的生命周期上下文 + 统一超时。
	// 原先用 r.Context() 且不加超时 —— 关掉页面就会取消远端的安装（在那边留下半装的
	// 文件），而一直挂着则会把闸门占住十几分钟，期间这台机器上什么都装不了。
	installCtx, cancel := context.WithTimeout(s.runtimeCtx, s.config.agentUpdateTimeout())
	defer cancel()

	installation, err := s.installRuntimeFor(installCtx, runnerID, request)
	if err != nil {
		// 失败也留痕：往目标环境落一整套运行时是这条链路里最重的动作，
		// "装过但没记录"和"装成功但没记录"都不该发生。
		_ = s.recordInstallAudit(installCtx, installAuditEntry{
			RunnerID: runnerID, AgentID: runtimeAgentID, Action: "install-runtime",
			Result: "failed", Detail: errorText(err),
		})
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.recordInstallAudit(installCtx, installAuditEntry{
		RunnerID: runnerID, AgentID: runtimeAgentID, Action: "install-runtime",
		ToVersion: installation.Version, Result: "succeeded",
	}); err != nil {
		// 说清"已经装好了、只是没记上"：否则用户会以为失败而重装一遍。
		writeError(w, http.StatusInternalServerError,
			fmt.Errorf("运行时已安装（%s），但审计记录写入失败：%w", installation.Version, err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":    true,
		"version":    installation.Version,
		"binaryPath": installation.BinaryPath,
		"prefix":     installation.Prefix,
		"runtime":    s.runtimeStatusFor(installCtx, runnerID),
	})
}

// runtimeStatusFor 按 Runner 给出运行时状态（本机读本机，跨端读那边）。
//
// 跨端那条要跑一次环境探测（uname/命令探测），所以它只该出现在按需端点与
// 安装响应里，不能进 /api/runners 的热路径（docs/42 §14.G）。
func (s *Server) runtimeStatusFor(ctx context.Context, runnerID string) any {
	if isLocalRunnerID(runnerID) {
		return s.probeRuntime(ctx)
	}
	env := s.crossEnvironmentFor(runnerID)
	if env == nil {
		// **不能**返回 nil：那会变成 JSON 的 `null`，界面按 `runtime?.installed` 渲染成
		// "未安装" —— 把"读不到"写成"没有"。如实说清为什么读不到。
		return runtimeStatus{
			ID: runtimeAgentID, Origin: "none", InstallSupported: false,
			InstallBlockedReason: fmt.Sprintf("无法探测该执行环境上的运行时（%s 没有可用的跨端通道）", runnerID),
		}
	}
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	target, err := probeTargetEnvironment(probeCtx, env)
	if err != nil {
		return runtimeStatus{ID: runtimeAgentID, Origin: "none", InstallSupported: false, InstallBlockedReason: err.Error()}
	}
	if wsl, ok := env.(*wslCrossEnvironment); ok {
		wsl.recordVisibleMounts(target.Mounts)
	}
	status := s.probeRuntimeCross(probeCtx, env, target)
	// MeetsMinimumFor 由这里按"安装时会用的那套运行时"逐工具算 —— 与
	// installAgentCLICross 同一处判据（见 resolveCrossInstallTarget）。
	// 不放在 probeRuntimeCross 里：那里只知道"当前生效的那套"，而登记为系统 npm
	// 装的工具升级时用的是系统那套，两者在同一台机器上可以不同。
	if root, rootErr := crossToolchainRoot(target); rootErr == nil {
		status.MeetsMinimumFor = s.runtimeMeetsMinimumFor(ctx, runnerID, status, root)
	}
	return status
}
