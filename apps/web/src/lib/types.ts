// 类型定义 — 从 App.tsx 提取，供全项目使用

// 后端对非 Git 目录项目填的 gitBranch 标记值（app.go 三处一致）。前端用它在标签栏隐藏
// Git 工作台入口并拦截直接访问 /git 的重定向；若后端更换标记，只需改这一处。
export const NON_GIT_BRANCH = "非 Git 目录";

export type Project = { id: string; name: string; pathDisplay: string; fullPath: string; runner: string; environment: string; gitBranch: string; claudeReady: boolean; codexReady: boolean; agentReady: boolean };
// /api/projects/availability 的单项：项目连通性探测结果（远端 / 跨端 codex 就绪）。
// 列表接口不再同步探活，这份结果由前端单独拉取后按项目 id 合并。
export type ProjectAvailability = { id: string; claudeReady: boolean; codexReady: boolean; agentReady: boolean };
export type ProjectStatus = { running: boolean; conversationCount: number; activeTitle: string; insightsRunning: boolean; insightsMessage: string };
// 开发进程运行状态（/api/projects/processes/statuses + /ws/processes）。
// 与会话状态 ProjectStatus 语义独立，由 ProcessStatusProvider 单独拥有。
export type RunStatus = "stopped" | "starting" | "running" | "stopping" | "failed";
export type ProjectProcessStatus = { runStatus: RunStatus; runPid?: number; runStartedAt?: string | null; /** 服务端状态事件序号。 */ runSequence?: number; /** 进程状态上次由 WS 实时更新的时间戳(ms),用于判断 REST 兜底是否已过期。 */ runUpdatedAt?: number };
export type ProjectProcessStatusMap = Record<string, ProjectProcessStatus>;
// /ws/processes 单帧负载（与批量端点字段一一对应）。
export type RunStatusEvent = { projectId: string; status: RunStatus; sequence?: number; startedAt?: string | null; pid?: number | null };
export type ProjectFilter = "all" | "running" | "ready" | "offline";
export type PermissionMode = "approval_required" | "full_control" | "read_only" | "workspace_write";
export type AgentID = "claude-code" | "codex";
export type Conversation = { id: string; status: string; agentId: AgentID; agentSessionId: string; agentRuntimeId: string; agentProfileRevisionId?: string; executionPolicy: PermissionMode; permissionMode: PermissionMode; modelOverride?: string; title: string; preview?: string; lastActivityAt: string; isCurrent: boolean; isOrchestration?: boolean };
// 底部模型选择器的候选项与当前生效信息（GET /api/conversations/{id}/models）。
export type AgentModelOption = { id: string; label?: string; description?: string; alias?: boolean };
export type ConversationModels = { conversationId: string; agentId: AgentID; selected: string; effective: string; source: "override" | "profile" | "cli_default"; models: AgentModelOption[]; customAllowed: boolean; note?: string };
export type ConversationWorkspace = { id: string; conversationId: string; generation: number; mode: "project_shared" | "isolated_worktree" | string; path: string; branch?: string; baseRevision?: string; state: "provisioning" | "ready" | "active" | "failed" | "archived" | string; active?: boolean; createdAt: string; archivedAt?: string | null };
export type Message = { id: string; runId?: string; role: "user" | "assistant"; content: string; parentToolUseId?: string; createdAt: string };
export type ShortcutKind = "prompt" | "snippet" | "command_request";
export type Shortcut = { id: string; name: string; description: string; kind: ShortcutKind; template: string; scope: "local" | "project"; defaultAction: "fill" | "confirm" | "run"; groupName: string; pinned: boolean; enabled: boolean; sortOrder: number; projectIds: string[] };
export type ShortcutEditorState = { kind: ShortcutKind; shortcut?: Shortcut };
// 命令选择器的一项（GET /api/projects/{id}/commands，见 docs/37）。命令名不含前导 `/`。
export type AgentCommandGroup = "builtin" | "skill" | "project" | "user" | "plugin" | "other";
export type AgentCommandOption = { name: string; label?: string; description?: string; argumentHint?: string; group: AgentCommandGroup; recommended?: boolean; terminalOnly?: boolean };
// 命令目录。authoritative 为 true 时目录来自 CLI 本身，才能用它判断某条命令是否已失效；
// static/scan 下的"没找到"不能当作失效（会误报）。
export type ProjectCommands = { projectId: string; agentId: AgentID; env: string; source: "run" | "probe" | "scan" | "static"; authoritative: boolean; commands: AgentCommandOption[]; claudeCodeVersion?: string; refreshedAt?: string; customAllowed: boolean; note?: string };
export type SkillAgent = "claude-code" | "codex";
export type SkillSource = "user" | "project" | "plugin";
export type Skill = { name: string; description: string; agent: SkillAgent; env: "windows" | "wsl" | "remote-linux"; source: SkillSource };
export type ScheduledTaskScheduleType = "once" | "daily" | "weekly";
export type ScheduledTaskRunStatus = "queued" | "running" | "succeeded" | "failed" | "stopped" | "interrupted";
export type ScheduledTaskRun = { id: string; scheduledTaskId: string; scheduledFor: string; status: ScheduledTaskRunStatus; titleSnapshot?: string; agentIdSnapshot?: AgentID; permissionModeSnapshot?: PermissionMode; profileRevisionSnapshot?: string; promptSnapshot: string; skillsSnapshot: string[]; conversationId?: string; runId?: string; failureReason?: string; createdAt: string; startedAt?: string; finishedAt?: string };
export type ScheduledTask = { id: string; projectId: string; title: string; prompt: string; skills: string[]; agentId: AgentID; permissionMode: PermissionMode; scheduleType: ScheduledTaskScheduleType; timezone: string; runAt?: string; timeOfDay?: string; weekdays: number[]; enabled: boolean; nextRunAt?: string; lastRunAt?: string; createdAt: string; updatedAt: string; lastRun?: ScheduledTaskRun; runs?: ScheduledTaskRun[] };
export type Event = { id: string; type: string; payload: unknown; runId: string; createdAt: string };
export type Directory = { name: string; path: string };
export type Approval = { approvalId: string; status: "pending" | "allow" | "deny"; toolName: string; toolInput: Record<string, unknown>; toolUseId?: string };
export type ApprovalEvent = { approval: Approval; runId: string; createdAt: string };
export type ToolOutput = { content: string; isError: boolean };
export type ToolAction = { id: string; runId: string; name: string; input: Record<string, unknown>; createdAt: string; output?: ToolOutput; approval?: Approval; runStatus?: string };
export type AgentStatus = "pending" | "running" | "completed" | "failed" | "stopped" | "unresolved";
export type SSHProfile = { host: string; port: number; user: string; privateKeyPath: string };
export type SSHConnection = { id: string; name: string; host: string; port: number; user: string; authMethod: "key" | "password"; privateKeyPath?: string; rootPath: string; status: string; lastSeen?: string | null; errorMsg?: string; createdAt?: string };
export type SSHPreflightResult = { ok: boolean; claudeReady?: boolean; hostKey?: string; fingerprint?: string; checks?: Record<string, boolean>; error?: string; resolved?: SSHProfile };
export type MCPTransport = "stdio" | "http" | "sse";
export type MCPScope = "global" | "project";
export type MCPServer = {
  id: string;
  name: string;
  displayName: string;
  description: string;
  transport: MCPTransport;
  command?: string;
  args?: string[];
  env?: Record<string, string>;
  cwd?: string;
  url?: string;
  headers?: Record<string, string>;
  scope: MCPScope;
  projectId?: string;
  environments: string[];
  agents: string[];
  enabled: boolean;
  autoApproveTools?: string[];
  startupTimeoutSec: number;
  toolTimeoutSec: number;
  source: string;
  createdAt?: string;
  updatedAt?: string;
  // 读接口返回：属于加密引用的键名（明文永不回显）。
  envSecretKeys?: string[];
  headerSecretKeys?: string[];
};
export type MCPProjectView = {
  projectId: string;
  environment: string;
  strictMode: boolean;
  // 放行项目根目录的 .mcp.json（关闭 strict 的等价开关）。开启后项目自带的 MCP
  // 配置会一并生效，代价是绕过了 Milevia 的白名单与审计，故需二次确认。
  allowMcpJson: boolean;
  // 项目 .mcp.json 中发现的 server 名（仅提示用；未放行时它们不会生效）。
  mcpJsonServers: string[];
  effective: { id: string; name: string; displayName: string; transport: MCPTransport; scope: MCPScope; enabled: boolean; origin: MCPScope }[];
  bindings: MCPProjectBinding[];
  warnings: string[];
};
export type MCPProjectBinding = {
  serverId: string;
  serverName: string;
  displayName: string;
  scope: MCPScope;
  enabled: boolean;
  // overridden 表示该项目显式关掉了这条全局 server（默认无覆盖即启用）。
  overridden: boolean;
};
export type MCPToolFlag = { code: string; label: string; detail?: string; severity: "info" | "warn" | "danger"; note?: string };
export type MCPToolInfo = {
  name: string;
  title?: string;
  description?: string;
  inputSchema?: unknown;
  // 该工具声明需要用户交互：-p 模式下会被直接拒绝，免审批白名单也覆盖不了。
  requiresInteraction?: boolean;
  flags?: MCPToolFlag[];
};
// resources / prompts 是 MCP 的可选原语。Claude Code 会用到它们；Codex 仅消费 tools。
export type MCPResourceInfo = { uri: string; name?: string; title?: string; description?: string; mimeType?: string };
export type MCPPromptInfo = { name: string; title?: string; description?: string; arguments?: unknown };
export type MCPTestResult = {
  ok: boolean;
  serverId: string;
  name: string;
  displayName: string;
  environment: string;
  transport: MCPTransport;
  protocolVersion?: string;
  serverInfo?: unknown;
  tools: MCPToolInfo[];
  resources: MCPResourceInfo[];
  prompts: MCPPromptInfo[];
  toolCount: number;
  flaggedCount: number;
  durationMs: number;
  error?: string;
  hint?: string;
};
export type MCPImportSource = { kind: string; path: string; label: string; present: boolean; note?: string };
export type MCPImportCandidate = {
  source: string;
  sourceLabel: string;
  sourcePath: string;
  name: string;
  originalName: string;
  displayName: string;
  transport: MCPTransport;
  command: string;
  args: string[];
  url: string;
  envKeys: string[];
  headerKeys: string[];
  secretKeys: string[];
  agents: string[];
  scope: MCPScope;
  projectId?: string;
  conflict: boolean;
  conflictWith?: string;
  skipReason?: string;
  // 解析到但未导入的键（如 Codex 的 bearer_token_env_var）——提示用户手工补配。
  warnings?: string[];
};
export type MCPImportResult = { candidates: MCPImportCandidate[]; sources: MCPImportSource[]; imported: number; errors?: string[] };

// —— 按环境预览（POST /api/mcp/preview，保存前调用） ——
export type MCPPreviewInput = {
  transport: MCPTransport;
  command: string;
  args: string[];
  env: Record<string, string>;
  url: string;
  headers: Record<string, string>;
  environment: string;
  projectId: string;
};
export type MCPPreviewResult = {
  environment: string;
  projectPath: string;
  command: string;
  args: string[];
  env: Record<string, string>;
  url: string;
  headers: Record<string, string>;
  notes: string[];
};

// —— 内置模板（GET /api/mcp/presets） ——
// 模板除了「启动命令是什么」，还要说清「需要装什么、要填哪个 key」——
// 否则用户仍要去别处查，模板就只剩省几次敲键盘。
export type MCPPresetCredential = {
  key: string;
  // 凭据落在表单的哪个字段：env=环境变量，header=请求头。
  target: "env" | "header";
  label: string;
  description: string;
  // 填值时要替用户补上的前缀（如 "Bearer "）。有它，用户在向导里只需粘 token 本身。
  valuePrefix?: string;
  docsUrl?: string;
};
export type MCPPresetRequirement = { command: string; label: string; hint?: string };
export type MCPPreset = {
  id: string;
  name: string;
  displayName: string;
  description: string;
  // 给不懂 MCP 的用户看的一句话：连上之后能干什么。
  summary: string;
  // 目录分组；顺序由服务端给，前端不本地重排。
  category: string;
  // 图标键；前端只实现固定几个键，未知键退化为通用图标。
  icon: string;
  transport: MCPTransport;
  command?: string;
  args?: string[];
  url?: string;
  environments: string[];
  requires?: MCPPresetRequirement[];
  credentials?: MCPPresetCredential[];
  // 支持浏览器授权（用户点一次即可，不用自己去申请 Token）。
  oauth?: boolean;
  docsUrl?: string;
  note?: string;
};

// —— 运行时依赖检查（POST /api/mcp/runtime-check，保存前调用） ——
// 与「连接测试」的分工：测试会真的拉起 server 且必须已落库，本接口只回答
// 「这条配置在当前环境跑不跑得起来」，因此能在保存之前用。
export type MCPRuntimeCheckItem = { command: string; found: boolean; path?: string; error?: string };
export type MCPRuntimeCheckResult = {
  environment: string;
  items: MCPRuntimeCheckItem[];
  // 通道级失败（SSH 未连接、WSL 不可用）。非空时 items 里的「未找到」不成立。
  error?: string;
};

// —— MCP 远程授权（OAuth 2.1 + PKCE，仅 http / sse） ——
export type MCPOAuthStatus = {
  serverId: string;
  authorized: boolean;
  scope?: string;
  tokenType?: string;
  expiresAt?: string;
  expired: boolean;
  hasRefreshToken: boolean;
  hasClientSecret: boolean;
  clientId?: string;
  authorizationEndpoint?: string;
  tokenEndpoint?: string;
};
export type MCPOAuthStart = {
  flowId: string;
  authorizationUrl: string;
  redirectUri: string;
  scope?: string;
  clientId?: string;
};
export type MCPOAuthFlowStatus = {
  flowId: string;
  serverId: string;
  status: "pending" | "done" | "error";
  error?: string;
};

// —— MCP 调用审计 ——
export type MCPAuditEntry = {
  id: string;
  serverName: string;
  toolName: string;
  conversationId: string;
  runId: string;
  projectId?: string;
  argsPreview?: string;
  decision?: string;
  status?: string;
  error?: string;
  durationMs: number;
  createdAt: string;
};
export type MCPAuditResponse = { entries: MCPAuditEntry[]; total: number };

// —— 最近一次注入快照（GET /api/projects/{id}/mcp/status） ——
export type MCPInjectionServer = { name: string; displayName: string; transport: string; origin: string };
export type MCPInjectionStatus = {
  projectId: string;
  environment: string;
  agentId: string;
  strictMode: boolean;
  serverCount: number;
  servers: MCPInjectionServer[];
  note?: string;
  runKey?: string;
  updatedAt: string;
};
export type AgentLog = { id: string; createdAt: string; kind: "text" | "tool" | "result" | "error"; title: string; detail: string; isError?: boolean };
export type AgentNode = { id: string; runId: string; parentId?: string; name: string; summary: string; createdAt: string; status: AgentStatus; logs: AgentLog[]; children: AgentNode[] };
export type AgentExecution = { runId: string; status: string; incomplete: boolean; agents: AgentNode[]; createdAt: string };
export type ModelUsage = { model: string; inputTokens: number; outputTokens: number; cacheReadTokens: number; cacheCreationTokens: number; estimatedCostUsd: number; contextWindow: number };
export type RunUsage = { runId: string; conversationId: string; available: boolean; reason?: string; status: string; model: string; contextWindow: number; contextInputTokens: number; inputTokens: number; outputTokens: number; cacheReadTokens: number; cacheCreationTokens: number; estimatedCostUsd: number; agentTurns: number; modelSteps: number; toolCalls: number; subagentCount: number; durationMs: number; ttftMs: number; terminalReason: string; hasResult: boolean; startedAt?: string; completedAt?: string; models: ModelUsage[] };
export type ConversationUsage = { taskCount: number; agentTurns: number; modelSteps: number; toolCalls: number; subagentCount: number; inputTokens: number; outputTokens: number; cacheReadTokens: number; cacheCreationTokens: number; estimatedCostUsd: number };
export type ConversationUsageResponse = { conversationId: string; available: boolean; reason?: string; context: RunUsage; currentRun?: RunUsage; latestRun?: RunUsage; session: ConversationUsage; models: ModelUsage[] };
export type SystemVariant = "compact" | "compact_result" | "compact_boundary" | "api_retry" | "task";
export type SystemItem = { id: string; createdAt: string; runId: string; variant: SystemVariant; title: string; detail?: string; metadata?: Record<string, unknown> };
export type TimelineItem =
  | { kind: "message"; id: string; createdAt: string; message: Message }
  | { kind: "tool"; id: string; createdAt: string; action: ToolAction }
  | { kind: "system"; id: string; createdAt: string; system: SystemItem }
  | { kind: "error"; id: string; createdAt: string; runId: string; title: string; detail: string; taskId?: string };
export type WorkspaceTab = "conversation" | "tasks" | "orchestration" | "files" | "git" | "run" | "terminal" | "insights";
export type TerminalSessionInfo = { id: string; projectId: string; workspaceId?: string; environment: string; shell?: string; elevated?: boolean; cwdDisplay?: string; status: "starting" | "running" | "exited" | "failed" | "closed"; createdAt: string };
// GET .../terminal/sessions 的响应：会话清单 + 服务端下发的并发上限（界面禁用
// “新建”和计数都以此为准，不再前端硬编码固定值）。
export type TerminalSessionList = { sessions: TerminalSessionInfo[]; maxPerProject: number; maxProjects: number };

export type ToolStatus = { status: "ready" | "unavailable" | "needs_auth" | "updating"; version: string; reason?: string };
export type RunnerInfo = {
  id: string;
  name: string;
  environment: string;
  root: string;
  profileManagement?: boolean;
  claude: ToolStatus;
  codex?: ToolStatus;
};

export type AgentProfile = {
  id: string;
  runnerId: string;
  agentId: AgentID;
  name: string;
  currentRevisionId: string;
  enabled: boolean;
  revision: number;
  baseUrl?: string;
  model?: string;
  // Legacy records can still be listed so their owner can migrate them. New
  // records and every executable record are cli_managed.
  authMode: string;
  state: "active" | "deprecated" | "revoked";
};

export type QuotaGroup = {
  id: string;
  runnerId: string;
  name: string;
  scope: "credential" | "organization" | "workspace" | "model" | "ip";
  scopeKey: string;
  rpmLimit: number;
  tpmLimit: number;
  maxConcurrency: number;
  enabled: boolean;
};

export type CredentialPool = {
  id: string;
  runnerId: string;
  name: string;
  enabled: boolean;
  currentRevisionId: string;
  strategy: "fair_queue" | "least_loaded" | "round_robin";
  projectMaxConcurrency: number;
  members: { profileId: string; profileRevisionId: string; name: string; agentId: AgentID; model: string; enabled: boolean }[];
};

export type CheckUpdateResult = {
  updateAvailable: boolean;
  // 该 runner 是否支持"应用内自动更新"。缺省（旧服务端未返回）视为 true；
  // 跨端 runner（wsl-local 等）为 false，表示有新版本但需到目标环境手动执行 claude/codex update。
  autoUpdatable?: boolean;
  currentVersion: string;
  latestVersion?: string;
  error?: string;
};

export type UpdateResult = {
  success: boolean;
  previousVersion?: string;
  currentVersion?: string;
  error?: string;
};
