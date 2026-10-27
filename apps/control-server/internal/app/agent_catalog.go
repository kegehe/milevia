package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
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
	// CommandAliases 是**同一个可执行文件**在 PATH 上的其它命令名。
	//
	// 原生安装器有时会同时放两个入口（Cursor 的官方安装器就有 cursor-agent 与 agent
	// 两个名字，用户 PATH 里可能只出现其中一个）。探测与升级必须把别名也当成这个工具，
	// 否则"装了却报未安装"；但它们不是两个工具，所以不单独成条目。
	CommandAliases []string `json:"-"`
	// BinPath 是该命令在 npm 包内的**相对路径**，逐字等于 `npm view <pkg> bin` 里
	// 那一条目标：
	// claude-code → `bin/claude.exe`、codex → `bin/codex.js`、codebuddy →
	// `bin/codebuddy`、qoder → `bundle/qoder-npm-dispatcher.cjs`、
	// opencode → `bin/opencode.exe`。
	//
	// ⚠️ 早先这个字段叫 BinFile，只装**文件名**，由消费方再拼 `bin/`。那样一来
	// 目标不在 bin/ 下的包（qoder 的 bin 目标在 bundle/）根本表达不出来，而且
	// 照抄完整目标会得到 `<包根>/bin/bin/<X>` 这个不存在的路径。改成"包内相对
	// 路径"之后，消费方只做一次 Join，没有第二个假设可错。升级 CLI 后需复核。
	BinPath string `json:"-"`

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

	// LatestVersionURL / LatestVersionPattern：没有 npm registry 的**原生分发**工具，
	// 其最新版本从官方安装脚本里提取（脚本里带下载目录的版本号）。
	// 抓不到时由 latestNativeAgentVersion 如实报错 —— 绝不回落成"已是最新"。
	LatestVersionURL     string `json:"-"`
	LatestVersionPattern string `json:"-"`

	// ── 安装与升级 ──────────────────────────────────────────────────
	SupportsInstall bool     `json:"supportsInstall"`
	UpdateArgs      []string `json:"-"`

	// ── 能力声明（界面据此渲染，不在前端另写一遍）────────────────────
	PermissionModes       []string           `json:"permissionModes"`
	DefaultPermissionMode string             `json:"defaultPermissionMode"`
	Requires              []AgentRequirement `json:"requires"`
	// SlashCommands 表示该 CLI 自报斜杠命令目录（前端据此决定是否显示命令选择器）。
	SlashCommands bool `json:"slashCommands"`
	// MCPInjection 描述注入方式：
	// "config-file" | "cli-args" | "inline-config" | "unsupported-remote" | "user-config"。
	// "user-config" 表示 CLI 没有会话级注入参数，只读用户自己的配置文件 ——
	// 平台必须显式记录"本次未注入"，不能假装注入成功。
	// 仅作能力声明；真正的注入分支在各 runner / mcp_config.go。
	MCPInjection string `json:"mcpInjection"`

	// ── 能力开关（界面据此渲染，不在前端另写一遍）────────────────────
	// SupportsLogin 表示平台可以在管理页发起该工具自己的登录流程（浏览器授权/D 码）。
	// 仅录入目录、接收登录动作；真正的登录执行器在 agent_login.go（阶段 2）。
	SupportsLogin bool `json:"supportsLogin"`
	// LoginArgs 是兜底登录指引里的子命令参数（如 `auth login`）。
	// 只有无头环境无法驱动 TUI 的工具才需要它；能自行发起登录的 runner 不读这里。
	LoginArgs []string `json:"-"`
	// AuthStatusArgs 是查询登录态的目录命令（如 `auth list`）。跨端 backend 用它
	// 在目标环境执行；为空表示该工具没有可用的非交互登录态查询。
	AuthStatusArgs []string `json:"-"`
	// AuthStatusEmptyMarkers 是 AuthStatusArgs 输出里表示“没有凭据”的片段。
	// 命中任意一条即判为未登录；不命中且命令成功才判为已登录。
	AuthStatusEmptyMarkers []string `json:"-"`
	// RunnableInProject 表示平台已实现该工具的 AgentRunner，可在项目对话中真正运行。
	// 未接通前为 false：管理页照常展示与安装，但新建会话不会把它列为可用 agent。
	RunnableInProject bool `json:"runnableInProject"`
	// SupportsProjectInsights 表示平台已接通该工具的只读分析协议，可用于优化建议。
	// 这比能否参与普通项目对话更严格：分析需要可靠的只读权限、结构化输出与复核流程。
	SupportsProjectInsights bool `json:"supportsProjectInsights"`
	// SupportsConflictSuggestions 表示已接通单文件冲突的只读合并建议协议。
	// 不等同于优化建议扫描：会话工具也可以只运行一轮并返回合并结果。
	SupportsConflictSuggestions bool `json:"supportsConflictSuggestions"`
	// SupportsProfiles 表示档案/路由表能存这个工具。当前仍受 agent_profiles 表的
	// CHECK(agent_id in ('claude-code','codex')) 约束；新增工具在迁移表之前必须为 false，
	// 否则界面会提供一个保存时必然撞 SQLite 约束的选项。
	SupportsProfiles bool `json:"supportsProfiles"`
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
		BinPath:           "bin/claude.exe",
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
		// Claude 长驻会话（StartSession）已实现并经活 harness 验证，可参与项目对话。
		RunnableInProject:           true,
		SupportsProjectInsights:     true,
		SupportsConflictSuggestions: true,
		SupportsProfiles:            true,
	},
	{
		ID:                "codex",
		Name:              "Codex",
		Vendor:            "OpenAI",
		Homepage:          "https://developers.openai.com/codex/",
		InstallKind:       InstallKindNpmGlobal,
		NpmPackage:        "@openai/codex",
		CommandName:       "codex",
		BinPath:           "bin/codex.js",
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
		// Codex 走一次性回合（非流式 Run），已在项目对话中接通，可参与对话。
		RunnableInProject:           true,
		SupportsProjectInsights:     true,
		SupportsConflictSuggestions: true,
		SupportsProfiles:            true,
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
		BinPath:           "bin/codebuddy",
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
		RunnableInProject:           true,
		SupportsConflictSuggestions: true,
	},
	{
		ID:          "qoder",
		Name:        "Qoder CLI",
		Vendor:      "Alibaba",
		Homepage:    "https://qoder.com/",
		DocsURL:     "https://docs.qoder.com/cli/quickstart",
		InstallKind: InstallKindNpmGlobal,
		NpmPackage:  "@qoder-ai/qodercli",
		// 两个 bin 目标（qoder / qodercli）指向同一份实现；取文档里一直在用的
		// `qoder`，用户照着官方文档敲命令时名字对得上。
		CommandName:       "qoder",
		BinPath:           "bundle/qoder-npm-dispatcher.cjs",
		MinRuntimeVersion: "20.0.0",
		Readiness:         readinessVersion,
		VersionArgs:       []string{"--version"},
		SupportsInstall:   true,
		UpdateArgs:        []string{"update"},
		// 与 codex / codebuddy 同一档：headless `-p` 下没有可回复的审批通道，
		// 所以不列 approval_required。三档到参数的映射见 qoderSessionArgs：
		// read_only ↔ default（无头下需要确认的一律拒绝）、
		// workspace_write ↔ accept_edits、full_control ↔ bypass_permissions。
		PermissionModes:       []string{"read_only", "workspace_write", "full_control"},
		DefaultPermissionMode: "workspace_write",
		Requires: []AgentRequirement{{
			Command: "npm", Label: "Node.js（含 npm）", Kind: "managed-runtime",
			InstallHint: "https://nodejs.org/",
		}},
		// 实测（1.1.75）`-p -o stream-json` 的 system/init 帧自带 slash_commands 列表，
		// 但平台的"命令目录"接口（project_commands.go）还没有接 Qoder 的探测 —— 那边
		// 一旦按 Claude 探测，就会把 Claude 的命令当成 Qoder 的。目录接通前如实为 false：
		// 界面不给一个空的选择器。
		SlashCommands: false,
		// 走 `--mcp-config <file> [--strict-mcp-config]`，与 Claude Code 同形；
		// 密钥仍以 ${VAR} 占位符留在文件里、真值随进程环境走（mcp_config.go）。
		MCPInjection: "cli-args",
		// 平台会**在后台拉起** `qoder login`：CLI 自己在桌面环境打开浏览器（官方文档：
		// CI / SSH / 无图形界面时才跳过并打印链接）。登录是长流程，进程由 runner 持有、
		// 上限 10 分钟，重复点击复用同一次；登录态用 `qoder status` 的 Account 行读真实
		// 结果，见 qoder_runner.go 的 Login / LoginStatus。
		SupportsLogin: true,
		// 会话走 `-p --input-format stream-json --output-format stream-json`，
		// 事件形状与 Claude Code 同构（init → assistant → result）。
		RunnableInProject:           true,
		SupportsConflictSuggestions: true,
	},
	{
		ID:          "opencode",
		Name:        "OpenCode",
		Vendor:      "Anomaly",
		Homepage:    "https://opencode.ai/",
		DocsURL:     "https://opencode.ai/docs/",
		InstallKind: InstallKindNpmGlobal,
		NpmPackage:  "opencode-ai",
		CommandName: "opencode",
		// postinstall 在所有平台都把对应架构的二进制复制到 bin/opencode.exe
		// （见 opencode-ai 的 postinstall.mjs），因此这个相对路径可以跨平台通用。
		BinPath:           "bin/opencode.exe",
		MinRuntimeVersion: "18.0.0",
		Readiness:         readinessVersion,
		VersionArgs:       []string{"--version"},
		SupportsInstall:   true,
		UpdateArgs:        []string{"upgrade"},
		// 与 codex/codebuddy/qoder 同一档：run 是非交互的，没有可回复的逐命令审批通道。
		// 只读靠 `--agent plan` + 显式 deny 权限；写与完全控制靠 `--auto` + 权限配置。
		PermissionModes:       []string{"read_only", "workspace_write", "full_control"},
		DefaultPermissionMode: "workspace_write",
		Requires: []AgentRequirement{{
			Command: "npm", Label: "Node.js（含 npm）", Kind: "managed-runtime",
			InstallHint: "https://nodejs.org/",
		}},
		SlashCommands: false,
		MCPInjection:  "inline-config",
		// OpenCode 的登录是按 provider 的交互式流程（opencode auth login），平台不驱动 TUI，
		// 只提供入口与 `opencode auth list` 读数。
		SupportsLogin:          true,
		LoginArgs:              []string{"auth", "login"},
		AuthStatusArgs:         []string{"auth", "list"},
		AuthStatusEmptyMarkers: []string{"0 credentials", "No credentials", "not logged in", "No providers"},
		// 会话走一次性 `opencode run --format json`，由 opencode_runner.go 提供。
		RunnableInProject:           true,
		SupportsProjectInsights:     true,
		SupportsConflictSuggestions: true,
	},
	{
		ID:       "cursor",
		Name:     "Cursor CLI",
		Vendor:   "Anysphere",
		Homepage: "https://cursor.com/",
		DocsURL:  "https://cursor.com/docs/cli/overview",
		// 官方只提供 curl / PowerShell 安装器，npm 上的 cursor-agent 是第三方包，
		// 绝不能接（平台装错东西比装不上更糟）。
		InstallKind: installKindNative,
		CommandName: "cursor-agent",
		// Windows 安装器同时放 cursor-agent.exe 与 agent.cmd；PATH 里可能只有一个。
		// 两个名字都指向同一份实现，探测/升级都要认。
		CommandAliases: []string{"agent"},
		Readiness:      readinessVersion,
		VersionArgs:    []string{"--version"},
		// 没有公开的版本 JSON API；官方安装脚本里带下载目录版本（如 2026.10.19-<hash>）。
		// 读不到就如实报错，界面显示"读不到最新版本"，不冒充"已是最新"。
		LatestVersionURL: "https://cursor.com/install",
		// 月份/日可能不带前导零（2026.11.2-<hash>），构建号也可能有多段
		// （YYYY.MM.DD-HH-MM-SS-commit）。比较逻辑见 cursorUpdateAvailableFrom。
		LatestVersionPattern: `lab/(\d{4}\.\d{1,2}\.\d{1,2}(?:-[0-9A-Za-z]+)*)`,
		// 平台不代装原生分发包；升级交给 CLI 自己的 update。
		SupportsInstall: false,
		UpdateArgs:      []string{"update"},
		// headless `-p` 下没有可回复的审批通道，因此不列 approval_required。
		// 三档到参数的映射见 cursorExecArgs：
		// read_only ↔ --mode ask；workspace_write ↔ 默认 Agent 模式 + --force；
		// full_control ↔ 默认 Agent 模式 + --force + --sandbox disabled。
		// --mode 只接受 plan / ask，**不能**显式传 agent（那是默认值）。
		// 这是按官方参数文档做的映射，**尚未在装有 Cursor CLI 的机器上校准**
		// （见 docs 里的待校准清单）。
		PermissionModes:       []string{"read_only", "workspace_write", "full_control"},
		DefaultPermissionMode: "workspace_write",
		SlashCommands:         false,
		// Cursor CLI 没有会话级 MCP 参数，只读 ~/.cursor/mcp.json 与项目 .cursor/mcp.json；
		// 平台无法把选中的 server 注入这一轮，必须显式记录"未注入"（mcp_config.go）。
		MCPInjection: "user-config",
		// `cursor-agent login` 是交互式浏览器授权，平台只给入口与 `cursor-agent status` 读数。
		SupportsLogin:          true,
		LoginArgs:              []string{"login"},
		AuthStatusArgs:         []string{"status"},
		AuthStatusEmptyMarkers: []string{"not logged in", "not authenticated", "logged out", "not signed in"},
		// 一次性 print 回合：`cursor-agent -p --output-format stream-json`，
		// 下一轮用 `--resume <chatId>` 续聊（见 cursor_runner.go）。
		// 会话面只在本机（可执行文件由控制服务的 PATH 解析）。
		RunnableInProject:           true,
		SupportsProjectInsights:     false,
		SupportsConflictSuggestions: true,
		// agent_profiles 的 CHECK 约束仍只允许 claude-code / codex。
		SupportsProfiles: false,
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

// latestAgentVersion 查某工具的最新版本。
//
// 包名/来源地址从目录取，不再在各自的 CheckUpdate 里各写一遍字面量。
// 查询本身留在 control-server 所在侧执行：版本号跨平台唯一，与"目标环境用哪一端"无关
// （论证见 claude_runner.go 的 latestNpmPackageVersion）。
func latestAgentVersion(ctx context.Context, agentID string) (string, error) {
	entry, ok := agentByID(agentID)
	if !ok {
		return "", fmt.Errorf("unknown agent %q", agentID)
	}
	if entry.LatestVersionURL != "" {
		return latestNativeAgentVersion(ctx, entry)
	}
	if entry.NpmPackage == "" {
		return "", fmt.Errorf("%s 没有可查询的版本来源", entry.Name)
	}
	return latestNpmPackageVersion(ctx, entry.NpmPackage)
}

// latestNativeAgentVersion 从官方安装脚本里提取原生工具的最新版本。
//
// 为什么是脚本而不是 JSON API：这类工具（如 Cursor）只提供 curl / PowerShell 安装器，
// 没有公开的版本接口；安装脚本里的下载目录带版本号，是官方给出的唯一稳定读数。
//
// 任何一步失败都**如实报错**：非 2xx、读不出、正则不命中。绝不回落到"已是最新"——
// 那会把一次读取失败写成"没有更新"，用户于是永远不知道需要升级。
func latestNativeAgentVersion(ctx context.Context, entry AgentCatalogEntry) (string, error) {
	pattern, err := regexp.Compile(entry.LatestVersionPattern)
	if err != nil {
		return "", fmt.Errorf("读取 %s 最新版本的规则无效：%w", entry.Name, err)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, entry.LatestVersionURL, nil)
	if err != nil {
		return "", fmt.Errorf("构造 %s 版本查询失败：%w", entry.Name, err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("查询 %s 最新版本失败：%w", entry.Name, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("查询 %s 最新版本失败：HTTP %d", entry.Name, response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024))
	if err != nil {
		return "", fmt.Errorf("读取 %s 版本来源失败：%w", entry.Name, err)
	}
	match := pattern.FindSubmatch(body)
	if len(match) < 2 {
		return "", fmt.Errorf("%s 的版本来源里没有找到版本号", entry.Name)
	}
	version := strings.TrimSpace(string(match[1]))
	if version == "" {
		return "", fmt.Errorf("%s 的版本来源给出了空版本号", entry.Name)
	}
	return version, nil
}
