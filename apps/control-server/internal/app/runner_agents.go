package app

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// 某个 Runner 上「运行时 + 目录里每个工具」的状态汇总。
//
// 这是管理页唯一的数据来源。三档状态必须分开表达（docs/42 §5.2）：
//
//	probeOk=false  → 通道坏了（WSL 未安装 / SSH 未连接）。此时 items 一律不可信，
//	                 界面要说"无法检测"，**不能**渲染成"未安装"；
//	probeOk=true 且 installed=false → 真的未安装；
//	probeOk=true 且 installed=true  → 已安装（ready 说明还能用）。
//
// 这三档合并任何两档，都会让用户去做一件没有用的事（去装一个装不上、或本来就有的东西）。

// runnerAgentView 是单个工具在该 Runner 上的状态。
type runnerAgentView struct {
	ID        string `json:"id"`
	Installed bool   `json:"installed"`
	Version   string `json:"version,omitempty"`
	// BinaryPath 是实测可用的路径，界面据此显示"安装位置"（可直接复制）。
	BinaryPath string `json:"binaryPath,omitempty"`
	// InstallKindUsed：这次是怎么装上的（决定"升级"走哪条路，也让用户能自己判断）。
	InstallKindUsed string `json:"installKindUsed,omitempty"`
	// Ready 表示现在就能用（已安装且通过该工具的就绪判据）。
	Ready  bool   `json:"ready"`
	Reason string `json:"reason,omitempty"`

	InstallSupported     bool   `json:"installSupported"`
	InstallBlockedReason string `json:"installBlockedReason,omitempty"`
	UpdateSupported      bool   `json:"updateSupported"`
	// UpgradeNeedsGrant 表示"升级这一档也要先授权"（平台装的走 npm 重装）。
	// 与 NeedsGrant 分开：那一档是"装不了"，这一档是"装完了但升不了"。
	UpgradeNeedsGrant bool `json:"upgradeNeedsGrant,omitempty"`
	// AutoUpdatable 沿用既有三态：false = 有新版本但只能到目标环境手动升级。
	AutoUpdatable bool `json:"autoUpdatable"`
	// Operation 非空表示该工具有进行中的安装/升级。
	Operation string `json:"operation,omitempty"`
}

// runnerAgentsView 是 GET /api/runners/{runnerID}/agents 的响应。
type runnerAgentsView struct {
	RunnerID    string            `json:"runnerId"`
	Environment string            `json:"environment"`
	ProbeOK     bool              `json:"probeOk"`
	ProbeError  string            `json:"probeError,omitempty"`
	Runtime     any               `json:"runtime"`
	Items       []runnerAgentView `json:"items"`
	// RemoteInstallAllowed：跨端安装是否已被逐主机授权（docs/42 §9.1）。
	RemoteInstallAllowed bool `json:"remoteInstallAllowed"`
}

// resolveProbeTarget 解析 {runnerID}，并把**两件不同的事**分开：
//
//	① 这台机器不在清单里（跨端未注册）→ 返回 err，调用方回 404；
//	② 它在清单里，但这会儿探测不了（典型：WSL 没装 / SSH 没连上）→
//	   返回 probeOk=false + 原因，调用方回 200 但如实说"无法检测"。
//
// **两处端点共用（listRunnerAgents / listRunnerDiagnostics）**：判据只有一份，
// 于是两处对同一台机器必然给出同一个结论。分开写会怎样：列表页说"无法检测"、
// 诊断页却列出一排工具症状，而用户不知道该信哪个 —— 那正是 docs/42 §19.2 E
// 收敛掉的那类判据分裂。（诊断那一侧原先硬写 `probeOk: true`，于是这个字段
// 永远为真、前端也没法消费它：一条假装存在的边界。）
func (s *Server) resolveProbeTarget(runnerID string) (RunnerMeta, bool, string, error) {
	if runnerID == "wsl-local" {
		// 与 runnerStatus 一致：WSL 可能因冷启动超时未注册，按需补一次探测。
		s.ensureWSLRunner()
	}
	meta, ok := s.runnerRegistry.getMeta(runnerID)
	if ok {
		return meta, true, "", nil
	}
	if !isLocalRunnerID(runnerID) {
		return RunnerMeta{}, false, "", errors.New("runner not found")
	}
	// 通道级失败：**不返回 404**，而是如实说"检测不了"。
	// 返回 404 会让界面把它当成"这个运行器不存在"，而真相常常是
	// "WSL 没装 / SSH 没连上"——那是用户可以去解决的事。
	reason := fmt.Sprintf("无法检测：%s 未就绪", runnerID)
	if runnerID == "wsl-local" {
		reason = "无法检测：未检测到可用的 WSL 发行版（wsl-local 未注册）。请确认 WSL 已安装并设置了默认发行版。"
	}
	return RunnerMeta{}, false, reason, nil
}

// listRunnerAgents 汇总某个 Runner 上的运行时与工具状态。
func (s *Server) listRunnerAgents(w http.ResponseWriter, r *http.Request) {
	runnerID := chi.URLParam(r, "runnerID")
	meta, probeOK, probeError, err := s.resolveProbeTarget(runnerID)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if !probeOK {
		writeJSON(w, http.StatusOK, runnerAgentsView{
			RunnerID: runnerID, ProbeOK: false, ProbeError: probeError,
			Items: []runnerAgentView{}, Runtime: nil,
		})
		return
	}

	statuses := s.probeAgents(r.Context(), meta)
	view := runnerAgentsView{
		RunnerID: meta.ID, Environment: meta.Environment, ProbeOK: true,
		Items: make([]runnerAgentView, 0, len(statuses)),
	}
	recorded := map[string]string{}
	recordedKind := map[string]string{}
	// 登记表**两端都读**（原先只读本机）：跨端也有自己的登记项，而"这份工具是怎么装上的"
	// 决定升级走哪条路、要不要过授权闸门 —— 界面必须知道这件事，否则它会亮出一个
	// 点了必失败的升级按钮。
	items, err := s.listAgentInstallations(r.Context(), meta.ID)
	if err != nil {
		// 读不到登记时**不能让页面继续**：下面的每一行（安装位置、升级走哪条路、
		// 要不要授权）都靠它，静默当成"没装过"会让界面显示"未安装"并给出错误的
		// 升级入口 —— 那是把"读不到"说成"没有"。
		writeError(w, http.StatusInternalServerError, fmt.Errorf("读取安装登记失败：%w", err))
		return
	}
	for _, item := range items {
		recorded[item.AgentID] = item.BinaryPath
		recordedKind[item.AgentID] = item.InstallKind
	}
	if isLocalRunnerID(meta.ID) {
		view.Runtime = s.probeRuntime(r.Context())
		view.RemoteInstallAllowed = true // 本机不需要逐主机授权
	} else {
		// 跨端也要有运行时状态：那是"能不能装工具"的前置，没有它界面就只能
		// 显示一个点了必失败的安装按钮。探测放在这个按需端点里（不进热路径）。
		view.Runtime = s.runtimeStatusFor(r.Context(), meta.ID)
		view.RemoteInstallAllowed = s.remoteInstallAllowed(r.Context(), meta.ID)
	}

	for _, status := range statuses {
		entry, known := agentByID(status.ID)
		var backend AgentRunner
		if known {
			backend, _ = s.agentBackend(meta, entry)
		}
		item := runnerAgentView{
			ID:              status.ID,
			Installed:       status.Status != agentStatusUnavailable && status.Status != agentStatusUnsupported,
			Version:         status.Version,
			BinaryPath:      recorded[status.ID],
			InstallKindUsed: recordedKind[status.ID],
			Ready:           status.Status == agentStatusReady,
			Reason:          status.Reason,
			UpdateSupported: true,
			// 与 /api/runners 的 checkAgentUpdate 同一判据（agentAutoUpdatable）。
			AutoUpdatable: agentAutoUpdatable(backend),
		}
		if known {
			item.InstallSupported = entry.SupportsInstall
		}
		if status.Status == agentStatusUpdating {
			item.Operation = "running"
			item.Installed = true
		}
		// 平台装的（npm 类）升级时走 npm 重装 —— 那要过授权闸门。用户自己用官方
		// 安装器装的走 CLI 自带的 update，不受这道闸门约束（见 crossUpdateUsesNpmInstall）。
		if kind := recordedKind[status.ID]; kind == installKindNpmManaged || kind == installKindNpmSystem {
			item.UpgradeNeedsGrant = !view.RemoteInstallAllowed
		}
		if status.Status == agentStatusUnsupported {
			// "这个环境不提供该工具"：既不支持安装，也不该给出更新按钮。
			item.InstallSupported = false
			item.InstallBlockedReason = status.Reason
			item.UpdateSupported = false
		} else if !isLocalRunnerID(meta.ID) && !view.RemoteInstallAllowed {
			// 未授权：这一档由 installSupported=false + 可操作的理由表达，
			// 授权入口在运行时卡片里给**一处**（每个工具再各来一个同义按钮是噪音）。
			item.InstallSupported = false
			item.InstallBlockedReason = fmt.Sprintf("尚未授权在 %s 上安装；需先在该主机上显式确认一次", meta.Name)
		}
		view.Items = append(view.Items, item)
	}
	writeJSON(w, http.StatusOK, view)
}
