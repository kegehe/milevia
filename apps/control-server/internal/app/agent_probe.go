package app

import (
	"context"
	"fmt"
	"sync"
)

// 单个 Runner 上「每个工具的状态」的探测与汇总。
//
// 收口前的状况：同一个事实在 listRunners 与 runnerStatus 里各写了一遍，
// 两段近乎逐行重复（claude 一段、codex 一段），加第三个工具就得加第三段。
// 更实际的问题是两段的文案已经漂了：同一条件在 listRunners 里写「本机 Codex CLI
// 未安装或未登录」、在 runnerStatus 里写「本地 Codex CLI 未安装或未登录」。
// 现在工具维度由目录决定，两处都调 probeAgents。

// 状态取值。ready / unavailable / updating 是界面原有认识的三档，
// unsupported 用于"这个 Runner 根本不提供该工具"（与"提供了但没装"是两件事，
// 界面文案必须不同：前者要用户换环境，后者要用户去装）。
const (
	agentStatusReady       = "ready"
	agentStatusUnavailable = "unavailable"
	agentStatusUpdating    = "updating"
	agentStatusUnsupported = "unsupported"
)

// AgentStatus 是某个 Runner 上某个工具的状态快照。
type AgentStatus struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	// Version 在 status=ready 时有值。
	Version string `json:"version,omitempty"`
	// Reason 只在 status 不是 ready/updating 时有值，且说的是**确切原因**。
	Reason string `json:"reason,omitempty"`
}

// probeAgents 汇总某个 Runner 上目录里全部工具的状态（**走读数缓存**，见 agent_readings.go）。
//
// ⚠️ 签名与两个调用点（listRunners / runnerStatus）**刻意一个字都不动**：
// TestRunnerEndpointsShareOneProbePath 用源码断言钉着它们必须调用这个方法。
// 要绕过缓存请用 probeAgentsFor —— 缓存做在本方法**内部**，而不是在调用点上。
func (s *Server) probeAgents(ctx context.Context, meta RunnerMeta) []AgentStatus {
	statuses, _ := s.probeAgentsFor(ctx, meta, false)
	return statuses
}

// probeAgentsUncached 是真正的探测（每次都跑，不看缓存）。
//
// 工具之间互相独立、且每个探测都可能拉起一个远端进程，因此并发跑 —— 原先只有
// runnerStatus 这样做，listRunners 是串行的；合并后两边都是并发，逐个探测的超时
// 不再相加。
func (s *Server) probeAgentsUncached(ctx context.Context, meta RunnerMeta) []AgentStatus {
	entries := agentCatalog()
	out := make([]AgentStatus, len(entries))
	var probes sync.WaitGroup
	for index, entry := range entries {
		probes.Add(1)
		go func(index int, entry AgentCatalogEntry) {
			defer probes.Done()
			out[index] = s.probeAgent(ctx, meta, entry)
		}(index, entry)
	}
	probes.Wait()
	return out
}

// probeAgent 探测单个工具。
func (s *Server) probeAgent(ctx context.Context, meta RunnerMeta, entry AgentCatalogEntry) AgentStatus {
	backend, unsupportedReason := s.agentBackend(meta, entry)
	if unsupportedReason != "" {
		// "这个环境不提供该工具"与"提供了但没装"是两件事：前者要用户换环境，
		// 后者要用户去安装。这一档也不能被更新状态盖掉 —— 没人会在不支持它的
		// 环境上安装它。
		return AgentStatus{ID: entry.ID, Status: agentStatusUnsupported, Reason: unsupportedReason}
	}
	// 进行中的安装/升级优先于探测结果：那是我们自己发起、确切知道的状态。
	// 顺序不能反 —— 更新期间二进制可能正处于被替换的中间态，此时探测会得到
	// "版本为空"，界面就会把"正在更新"说成"未安装"。跳过探测也顺带避免了在
	// 文件正被替换时去执行它。
	if s.agentMaintenanceActive(meta.ID, entry.ID) {
		return AgentStatus{ID: entry.ID, Status: agentStatusUpdating}
	}
	if backend == nil {
		// 防御分支：register 会同时写入 metas 与 runners，正常不会出现。
		return AgentStatus{ID: entry.ID, Status: agentStatusUnavailable}
	}

	status := agentStatusReady
	version := ""
	reason := ""
	if entry.Readiness == readinessBinary {
		// 只问"二进制在不在"，不问登录态（受管 api_key 档案自带凭据）。
		if !backend.Ready(ctx) {
			status = agentStatusUnavailable
		} else {
			version = backend.Version(ctx)
		}
	} else {
		// 能报出版本即就绪。这条不查认证，所以探测足够轻，可以放进列表接口。
		version = backend.Version(ctx)
		if version == "" {
			status = agentStatusUnavailable
		}
	}
	if status == agentStatusUnavailable {
		reason = s.agentUnavailableReason(ctx, meta, entry)
	}
	return AgentStatus{ID: entry.ID, Status: status, Version: version, Reason: reason}
}

// agentBackend 选出"在这个 Runner 上执行该工具"的后端。
//
// 返回 nil 表示探测不了，第二项是给用户的原因（空串 = 运行器自身未注册，
// 属于防御分支：register 会同时写入 metas 与 runners，因此正常不会出现）。
//
// 分派的顺序与理由（这一处是"某工具在某条路径上莫名不可用"的高发地，所以写清楚）：
//
//	① 本机的 Codex / CodeBuddy 由各自独立的管理 runner 提供 —— 它们与注册表里那个
//	   Claude runner **不是同一个进程面**（Codex 走一次性 exec、CodeBuddy 走
//	   stream-json 会话）。这是结构差异，不是"哪个工具特殊"。
//	② Codex / Claude 这两个**真有额外语义**的走特化：
//	   - Codex 的"就绪"要看登录态（`codex login status`），仅测能不能报版本会把
//	     "装了但没登录"报成就绪；
//	   - Claude 的跨端探测键（claude-version / `claude auth status`）与读数缓存已被
//	     WSL 的保活唤醒（refreshFailedProbes）依赖着，改走目录驱动会另起一套探测键
//	     （同一件事探两遍），并把就绪判据从"登录态"放宽成"能报版本"。
//	③ **其余一律走目录驱动**（catalogAgentBackend）。这是本函数原先那句"从两份变一份，
//	   而不是把它继续扩散"的落点：目录里新增一个工具，只要目标端的 runner 能跑 shell，
//	   就自动可用 —— 不必再在 wsl/ssh 两个 runner 上各加一组方法。
//
// ③ 生效前后最直接的一个差别：CodeBuddy 在跨端不再是"跨端管理尚未接通"（那一档
// 判成 unsupported，于是管理页连「安装」按钮都不给），而是如实报"未安装"并给出安装入口。
func (s *Server) agentBackend(meta RunnerMeta, entry AgentCatalogEntry) (AgentRunner, string) {
	if isLocalRunnerID(meta.ID) {
		switch entry.ID {
		case "codex":
			// 本机 Codex 不经过 runnerRegistry：它由独立的 codexRunner 提供。
			return s.codexRunner, ""
		case "codebuddy":
			// 本机 CodeBuddy 由独立的管理面 runner 提供。
			return s.codebuddyRunner, ""
		}
	}
	runner, registered := s.runnerRegistry.get(meta.ID)
	if !registered {
		return nil, ""
	}
	switch entry.ID {
	case "codex":
		codexR, ok := runner.(CodexCapableRunner)
		if !ok {
			return nil, fmt.Sprintf("此 Runner 不支持 %s", entry.Name)
		}
		return codexRunnerAdapter{codexR}, ""
	case "claude-code":
		// 注册表里这个 runner 本身就是 Claude 的执行面（跨端亦然）。
		return runner, ""
	}
	if shell, ok := runner.(crossShellRunner); ok {
		return &catalogAgentBackend{entry: entry, shell: shell}, ""
	}
	// 这个 runner 连"在目标环境里跑一条命令"都提供不了（如 WSL→Windows 那条方向，
	// 见 docs/42 §17.7）。如实说"尚未接通"，不用另一个工具的读数冒充它。
	return nil, fmt.Sprintf("%s 在 %s 上的管理尚未接通", entry.Name, meta.Name)
}

// agentUnavailableReason 给出"装了但用不了 / 没装"的确切说法。
//
// 按目录里的 Readiness 分流，而不是一律写"未安装或未登录"：只查二进制存在的
// （Codex）说不上是不是登录问题，只查能否报版本的（Claude Code）也一样。
// 原先把两种判据的原因写成同一句话，是把两种真相混成了一句。
//
// **再分一层（docs/43 §7 选 B）**：如果这台机器上**登记过**这个工具，那就不是
// "未安装"，而是"已安装但不可用"。两者的下一步完全不同（去装一个 vs 去修一个），
// 而原先这一档会被界面当成"未安装"处理（`Installed=false` ⇒ 只给「安装」、
// 不给「检查更新」）—— 那正是用户说的"无法使用也无法更新"。
//
// ⚠️ 这里**只改说法，不动 `installed` 的语义**。那个字段还要喂给对话页与手机端
// 快照，为了管理页的展示去改线协议不划算（see docs/43 §7 选项 C 为什么不做）。
func (s *Server) agentUnavailableReason(ctx context.Context, meta RunnerMeta, entry AgentCatalogEntry) string {
	hasRecord := false
	// 读不到登记表时按老说法走 —— **"读不到"不能当成"有记录"，也不能把整条列表
	// 接口拖坏**。这一句只是展示文案，而 probeAgents 跑在 /api/runners 的热路径上、
	// 每个工具一次；让它因为一次库读失败（或一个没建库的测试夹具）而 panic，
	// 会把整个控制服务带走。
	if s.db != nil {
		if _, recorded, err := s.recordedInstallation(ctx, meta.ID, entry.ID); err == nil {
			hasRecord = recorded
		}
	}
	return agentUnavailableReasonText(meta, entry, hasRecord)
}

// agentUnavailableReasonText 是那句文案本身（**纯函数**：不碰库、不碰磁盘）。
//
// 拆出来是为了让"各档文案互不相同"这条断言能在没有 Server 的情况下钉住 ——
// 文案这类东西一旦要建个 Server 才能测，就没人测了。
func agentUnavailableReasonText(meta RunnerMeta, entry AgentCatalogEntry, hasRecord bool) string {
	where := agentRunnerWhere(meta)
	if hasRecord {
		return fmt.Sprintf("%s %s 已安装但不可用（可在 CLI 工具管理页检测并修复）", where, entry.Name)
	}
	if entry.Readiness == readinessBinary {
		return fmt.Sprintf("%s %s 未安装", where, entry.Name)
	}
	return fmt.Sprintf("%s %s 未安装或不可执行", where, entry.Name)
}

// agentRunnerWhere 是"在哪台机器上"的说法，按 Runner 类型分流。
//
// WSL 是本机的子系统，不是"远程服务器" —— 对着一台本机 WSL 说"远程服务器上未安装"，
// 用户会跑去检查网络。
func agentRunnerWhere(meta RunnerMeta) string {
	switch {
	case isLocalRunnerID(meta.ID):
		return "本机"
	case meta.Environment == "wsl":
		return "WSL 内"
	default:
		return "远程服务器上"
	}
}

// agentMaintenanceActive 报告该 (runner, agent) 是否有安装/升级正在进行。
func (s *Server) agentMaintenanceActive(runnerID, agentID string) bool {
	s.runnerMaintenanceMu.Lock()
	defer s.runnerMaintenanceMu.Unlock()
	return s.runnerUpdating[runnerAgentKey{runnerID: runnerID, agentID: agentID}]
}

// anyMaintenanceActive 报告该机器上是否有**任何**工具正在安装/升级。
//
// 读数缓存用它决定"要不要绕过缓存"（见 agent_readings.go 的 probeAgentsFor）：维护位是
// 动态状态，回放一份旧读数会让别的客户端在工具正被替换时看到"可用"。
func (s *Server) anyMaintenanceActive(runnerID string) bool {
	s.runnerMaintenanceMu.Lock()
	defer s.runnerMaintenanceMu.Unlock()
	for key := range s.runnerUpdating {
		if key.runnerID == runnerID {
			return true
		}
	}
	return false
}

// legacyAgentFields 由 agents[] 派生 claude / codex 两个旧字段。
//
// 存在意义只有一条：前端与手机端快照仍在读这两个键，一次性切换会让两条链路同时
// 变动。**派生**而不是各自再探测一遍，保证旧字段与新字段永远同源。
// 形状逐键对齐改动前：claude 只有 status/version，codex 三个键齐全。
func legacyAgentFields(agents []AgentStatus, runnerRegistered bool) map[string]any {
	out := map[string]any{}
	for _, agent := range agents {
		switch agent.ID {
		case "claude-code":
			// 原行为：runner 未注册时不出现该键（而不是出现一个 unavailable）。
			if !runnerRegistered {
				continue
			}
			out["claude"] = map[string]any{
				"status":  agent.Status,
				"version": agent.Version,
			}
		case "codex":
			out["codex"] = map[string]string{
				"status":  agent.Status,
				"version": agent.Version,
				"reason":  agent.Reason,
			}
		}
	}
	return out
}

// agentAutoUpdatable 回答"这个工具在这台机器上能不能应用内升级"。
//
// 判据必须有**唯一来源**：管理页（GET /api/runners/{id}/agents）与对话页
// （POST …/check-update）问的是同一件事。原先一处硬编码 true、一处读 runner 的能力
// 标记，于是同一个 WSL 上的同一个工具，一处给"升级"按钮、一处说"需手动更新"。
func agentAutoUpdatable(backend AgentRunner) bool {
	if backend == nil {
		return false
	}
	if ar, ok := backend.(autoUpdateSupportedRunner); ok {
		return ar.AutoUpdateSupported()
	}
	// 未实现该可选接口的 runner 视为支持（既有语义：本地 / SSH）。
	return true
}
