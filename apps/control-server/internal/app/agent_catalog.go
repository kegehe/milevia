package app

import (
	"context"
	"fmt"
	"net/http"
	"sort"
)

// 本文件是「平台支持哪些 AI CLI 工具」的唯一事实来源。
//
// 为什么单独成文件：在这次整理之前，这个事实被硬编码在至少八处
// （agent_profiles.go 的 validProfileAgent 与一个 for 循环、mcp_config.go、
// mcp_servers.go、git_conflict_suggest.go、insights.go、app.go 里两段近乎逐行
// 重复的 claude/codex 探测块，以及路由表）。任何一处漏改都会表现为"某个工具在
// 某条路径上莫名不可用"。新增或改名工具时，这里改一处，其余全部读它。

// InstallKindNpmGlobal 表示该 CLI 通过 npm 全局包分发。
// 写成字段而不是常量判断，是为了让"换一种分发方式"只改目录、不改执行代码。
const InstallKindNpmGlobal = "npm-global"

// Readiness 的两种取值，语义见 AgentCatalogEntry.Readiness。
const (
	readinessVersion = "version"
	readinessBinary  = "binary"
)

// AgentRequirement 声明某个工具在目标环境需要什么运行时。
//
// 这里只声明「需要什么」——真伪由目标环境实测（见 docs/42 §6.1），
// 与 MCP 模板目录（mcp_servers.go）的口径一致：声明与实测分开，不互相冒充。
type AgentRequirement struct {
	// Command 是在目标环境探测的命令名（`command -v <command>`）。
	Command string `json:"command"`
	// Label 面向用户，只说"要准备什么"，不带协议名词。
	Label string `json:"label"`
	// Kind 目前只有 managed-runtime：平台可以自己下载并托管它。
	Kind string `json:"kind"`
	// InstallHint 是缺该运行时给出的可操作入口。
	InstallHint string `json:"installHint,omitempty"`
}

// AgentCatalogEntry 描述一个平台支持的 CLI 工具。
type AgentCatalogEntry struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Vendor   string `json:"vendor"`
	Homepage string `json:"homepage,omitempty"`
	DocsURL  string `json:"docsUrl,omitempty"`

	// ── 分发与命令解析 ────────────────────────────────────────────────
	InstallKind string `json:"installKind"`
	NpmPackage  string `json:"npmPackage"`
	CommandName string `json:"commandName"`
	// BinFile 是 npm 包内 bin/ 目录下的文件名（与 npmCLIInstall.binFile 同义）。
	// 注意：这是既有实现的沿用值，Unix 侧的确切文件名未经验证 —— 它只被
	// npmCLIInstall.binaryPath 用于符号链接比对（npm_cli_install.go:75），
	// 属于既有行为，本次不改动它。
	BinFile string `json:"-"`

	// MinRuntimeVersion 是该 CLI 要求的 Node 最低版本。
	// 值取保守下限：过低只是放行一次注定失败的尝试，过高则会错误拦住可用环境。
	// 确切下限以各包声明的 engines 字段为准，升级 CLI 后需复核（docs/42 §7.3）。
	MinRuntimeVersion string `json:"minRuntimeVersion"`

	// ── 版本探测 ────────────────────────────────────────────────────
	//
	// Readiness 说明"就绪"怎么判，两个取值：
	//   readinessVersion —— 能报出版本即就绪。探测很轻（不查登录态），
	//                       因此可以放在列表接口里逐 runner 跑。
	//   readinessBinary  —— 二进制存在即就绪，**不查登录态**：受管 api_key
	//                       档案会自带凭据，查登录态会把可用环境误判为不可用
	//                       （见 codex_runner.go 的 BinaryReady 注释与 docs/13 §6）。
	// 这个差异原本写在两段重复的 if 块里，收进目录后新增工具只能二选一，不会漏。
	Readiness   string   `json:"readiness"`
	VersionArgs []string `json:"-"`

	// ── 安装与升级 ──────────────────────────────────────────────────
	SupportsInstall bool     `json:"supportsInstall"`
	UpdateArgs      []string `json:"-"`

	// ── 能力声明（界面据此渲染，不在前端另写一遍）────────────────────
	PermissionModes       []string           `json:"permissionModes"`
	DefaultPermissionMode string             `json:"defaultPermissionMode"`
	Requires              []AgentRequirement `json:"requires"`
	// SlashCommands 表示该 CLI 自报斜杠命令目录（前端据此决定是否显示命令选择器）。
	SlashCommands bool `json:"slashCommands"`
	// MCPInjection 描述注入方式："config-file" | "cli-args" | "unsupported-remote"。
	MCPInjection string `json:"mcpInjection"`

	// ── 能力开关（界面据此渲染，不在前端另写一遍）────────────────────
	// SupportsLogin 表示平台可以在管理页发起该工具自己的登录流程（浏览器授权/D 码）。
	// 仅录入目录、接收登录动作；真正的登录执行器在 agent_login.go（阶段 2）。
	SupportsLogin bool `json:"supportsLogin"`
	// RunnableInProject 表示平台已实现该工具的 AgentRunner，可在项目对话中真正运行。
	// 未接通前为 false：管理页照常展示与安装，但新建会话不会把它列为可用 agent。
	RunnableInProject bool `json:"runnableInProject"`
}

// agentCatalogEntries 是唯一清单。顺序即界面顺序。
var agentCatalogEntries = []AgentCatalogEntry{
	{
		ID:                "claude-code",
		Name:              "Claude Code",
		Vendor:            "Anthropic",
		Homepage:          "https://claude.com/product/claude-code",
		InstallKind:       InstallKindNpmGlobal,
		NpmPackage:        "@anthropic-ai/claude-code",
		CommandName:       "claude",
		BinFile:           "claude.exe",
		MinRuntimeVersion: "18.0.0",
		Readiness:         readinessVersion,
		VersionArgs:       []string{"--version"},
		SupportsInstall:   true,
		UpdateArgs:        []string{"update"},
		// 逐命令网页审批依赖平台的 Bash Hook 回调，只有 Claude 这条链路有。
		PermissionModes:       []string{"approval_required", "full_control"},
		DefaultPermissionMode: "approval_required",
		Requires: []AgentRequirement{{
			Command: "npm", Label: "Node.js（含 npm）", Kind: "managed-runtime",
			InstallHint: "https://nodejs.org/",
		}},
		SlashCommands: true,
		MCPInjection:  "config-file",
	},
	{
		ID:                "codex",
		Name:              "Codex",
		Vendor:            "OpenAI",
		Homepage:          "https://developers.openai.com/codex/",
		InstallKind:       InstallKindNpmGlobal,
		NpmPackage:        "@openai/codex",
		CommandName:       "codex",
		BinFile:           "codex.js",
		MinRuntimeVersion: "18.0.0",
		Readiness:         readinessBinary,
		VersionArgs:       []string{"--version"},
		SupportsInstall:   true,
		UpdateArgs:        []string{"update"},
		// approval_required 不可选：codex exec 的非交互命令面没有可回复的审批通道
		// （见 docs/13 §7.2）。页面必须如实告知改用 Claude，而不是把
		// workspace_write 说成"命令需确认"。
		PermissionModes:       []string{"read_only", "workspace_write", "full_control"},
		DefaultPermissionMode: "workspace_write",
		Requires: []AgentRequirement{{
			Command: "npm", Label: "Node.js（含 npm）", Kind: "managed-runtime",
			InstallHint: "https://nodejs.org/",
		}},
		// Codex 没有斜杠命令目录：远端命令面不提供该自报清单。
		SlashCommands: false,
		MCPInjection:  "cli-args",
	},
	{
		ID:                "codebuddy",
		Name:              "CodeBuddy Code",
		Vendor:            "Tencent Cloud",
		Homepage:          "https://www.codebuddy.ai/",
		DocsURL:           "https://www.codebuddy.ai/docs/cli/quickstart",
		InstallKind:       InstallKindNpmGlobal,
		NpmPackage:        "@tencent-ai/codebuddy-code",
		CommandName:       "codebuddy",
		BinFile:           "bin/codebuddy", // npm bin 目标（实测，见 npm view @tencent-ai/codebuddy-code bin）
		MinRuntimeVersion: "18.20.0",
		Readiness:         readinessVersion,
		VersionArgs:       []string{"--version"},
		SupportsInstall:   true,
		UpdateArgs:        []string{"update"},
		// 与 codex 一致走无头 exec 的权限面；`--permission-mode`/`--dangerously-skip-permissions`
		// 到本平台三档模式的真实映射留待阶段 3 真机校准（见 docs/CodeBuddy 方案 §未知点）。
		PermissionModes:       []string{"read_only", "workspace_write", "full_control"},
		DefaultPermissionMode: "workspace_write",
		Requires: []AgentRequirement{{
			Command: "npm", Label: "Node.js（含 npm）", Kind: "managed-runtime",
			InstallHint: "https://nodejs.org/",
		}},
		SlashCommands: true,
		MCPInjection:  "config-file",
		// CodeBuddy 首次使用需登录：支持的登录流程（浏览器授权）在阶段 2 接通。
		SupportsLogin: true,
		// AgentRunner（StartSession 流式会话）已实现并经活 harness 验证，可参与项目对话。
		RunnableInProject: true,
	},
}

// agentCatalog 返回目录副本，调用方可自由改动。
func agentCatalog() []AgentCatalogEntry {
	out := make([]AgentCatalogEntry, len(agentCatalogEntries))
	copy(out, agentCatalogEntries)
	return out
}

// agentByID 按 ID 查目录。第二个返回值为 false 表示平台不支持该工具。
func agentByID(agentID string) (AgentCatalogEntry, bool) {
	for _, entry := range agentCatalogEntries {
		if entry.ID == agentID {
			return entry, true
		}
	}
	return AgentCatalogEntry{}, false
}

// supportedAgentIDs 返回目录里全部工具 ID。
func supportedAgentIDs() []string {
	out := make([]string, 0, len(agentCatalogEntries))
	for _, entry := range agentCatalogEntries {
		out = append(out, entry.ID)
	}
	return out
}

// agentDisplayName 给出面向用户的工具名。
//
// 未知 ID 原样返回而不是回落到某个默认工具名 —— 把 Codex 显示成 "Claude Code"
// 正是本次要消灭的错（见 docs/42 §14.A）。
func agentDisplayName(agentID string) string {
	if entry, ok := agentByID(agentID); ok {
		return entry.Name
	}
	return agentID
}

// validProfileAgent 报告某个 ID 是否是平台支持的工具。
//
// 原先写死成 `agentID == "claude-code" || agentID == "codex"`——目录化之后
// 新增工具不再需要改这里（测试会给一个"目录里存在但旧硬编码里没有"的 ID）。
func validProfileAgent(agentID string) bool {
	_, ok := agentByID(agentID)
	return ok
}

// validAgentPolicy 报告某个工具是否支持某个权限模式。
//
// 原先按 agentID 分支写死两套，改为查目录的 PermissionModes。
func validAgentPolicy(agentID, policy string) bool {
	entry, ok := agentByID(agentID)
	if !ok {
		return false
	}
	for _, mode := range entry.PermissionModes {
		if mode == policy {
			return true
		}
	}
	return false
}

// listAgents 是 GET /api/agents：把目录交给前端，使"支持哪些工具""每个工具叫什么"
// "支持哪些权限模式"只有服务端一份。前端据此渲染工具选择器与状态标签，不再自己
// 维护名单（那些二元三元式是本次收敛的重点）。
func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	entries := agentCatalog()
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	writeJSON(w, http.StatusOK, entries)
}

// latestAgentVersion 查 registry 上某工具的最新版本。
//
// 包名从目录取，不再在 Claude / Codex 各自的 CheckUpdate 里各写一遍字面量。
// 查询本身留在 control-server 所在侧执行：npm registry 的版本号跨平台唯一，
// 与"目标环境用哪一端 npm"无关（论证见 claude_runner.go 的 latestNpmPackageVersion）。
func latestAgentVersion(ctx context.Context, agentID string) (string, error) {
	entry, ok := agentByID(agentID)
	if !ok {
		return "", fmt.Errorf("unknown agent %q", agentID)
	}
	if entry.NpmPackage == "" {
		return "", fmt.Errorf("%s 没有可查询的 npm 包", entry.Name)
	}
	return latestNpmPackageVersion(ctx, entry.NpmPackage)
}
