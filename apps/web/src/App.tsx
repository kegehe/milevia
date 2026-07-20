import { FormEvent, useCallback, useEffect, useMemo, useRef, useState } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import "./markdown.css";
import "./permission.css";
import "./conversation.css";
import "./stop.css";
import "./design-preview.css";
import "./tasks.css";
import "./git.css";
import { GitWorkbench } from "./features/git/GitWorkbench";
import { TaskBoard } from "./features/tasks/TaskBoard";
import { TaskQueue } from "./features/tasks/TaskQueue";

type Project = { id: string; name: string; pathDisplay: string; runner: string; gitBranch: string; claudeReady: boolean };
type ProjectStatus = { running: boolean; conversationCount: number; activeTitle: string };
type ProjectFilter = "all" | "running" | "ready" | "offline";
type PermissionMode = "approval_required" | "full_control";
type Conversation = { id: string; status: string; permissionMode: PermissionMode; title: string; preview?: string; lastActivityAt: string; isCurrent: boolean };
type Message = { id: string; runId?: string; role: "user" | "assistant"; content: string; parentToolUseId?: string; createdAt: string };
type ShortcutKind = "prompt" | "snippet" | "command_request";
type Shortcut = { id: string; name: string; description: string; kind: ShortcutKind; template: string; scope: "local" | "project"; defaultAction: "fill" | "confirm" | "run"; groupName: string; pinned: boolean; enabled: boolean; sortOrder: number; projectIds: string[] };
type ShortcutEditorState = { kind: ShortcutKind; shortcut?: Shortcut };
type Event = { id: string; type: string; payload: unknown; runId: string; createdAt: string };
type Directory = { name: string; path: string };
type Approval = { approvalId: string; status: "pending" | "allow" | "deny"; toolName: string; toolInput: Record<string, unknown> };
type ApprovalEvent = { approval: Approval; runId: string; createdAt: string };
type ToolOutput = { content: string; isError: boolean };
type ToolAction = { id: string; runId: string; name: string; input: Record<string, unknown>; createdAt: string; output?: ToolOutput; approval?: Approval; runStatus?: string };
type AgentStatus = "pending" | "running" | "completed" | "failed" | "stopped" | "unresolved";
type AgentLog = { id: string; createdAt: string; kind: "text" | "tool" | "result" | "error"; title: string; detail: string; isError?: boolean };
type AgentNode = { id: string; runId: string; parentId?: string; name: string; summary: string; createdAt: string; status: AgentStatus; logs: AgentLog[]; children: AgentNode[] };
type AgentExecution = { runId: string; status: string; incomplete: boolean; agents: AgentNode[]; createdAt: string };
type ModelUsage = { model: string; inputTokens: number; outputTokens: number; cacheReadTokens: number; cacheCreationTokens: number; estimatedCostUsd: number; contextWindow: number };
type RunUsage = { runId: string; conversationId: string; status: string; model: string; contextWindow: number; contextInputTokens: number; inputTokens: number; outputTokens: number; cacheReadTokens: number; cacheCreationTokens: number; estimatedCostUsd: number; agentTurns: number; modelSteps: number; toolCalls: number; subagentCount: number; durationMs: number; ttftMs: number; terminalReason: string; hasResult: boolean; startedAt?: string; completedAt?: string; models: ModelUsage[] };
type ConversationUsage = { taskCount: number; agentTurns: number; modelSteps: number; toolCalls: number; subagentCount: number; inputTokens: number; outputTokens: number; cacheReadTokens: number; cacheCreationTokens: number; estimatedCostUsd: number };
type ConversationUsageResponse = { conversationId: string; context: RunUsage; currentRun?: RunUsage; latestRun?: RunUsage; session: ConversationUsage; models: ModelUsage[] };
type TimelineItem =
  | { kind: "message"; id: string; createdAt: string; message: Message }
  | { kind: "tool"; id: string; createdAt: string; action: ToolAction };

async function api<T>(path: string, init?: RequestInit, retries = 2): Promise<T> {
  let lastError: unknown;
  for (let attempt = 0; attempt <= retries; attempt++) {
    try {
      const response = await fetch(path, {
        headers: { "Content-Type": "application/json", ...init?.headers },
        ...init,
      });
      if (response.ok) return response.status === 204 ? undefined as T : response.json() as Promise<T>;
      const body = await response.json().catch(() => null);
      const message = body?.error || `Request failed (${response.status})`;
      // Don't retry client errors (4xx) — they won't succeed on retry.
      if (response.status >= 400 && response.status < 500) throw new Error(message);
      lastError = new Error(message);
      if (attempt < retries) await new Promise((resolve) => setTimeout(resolve, 500 * (attempt + 1)));
    } catch (cause: unknown) {
      if (cause instanceof TypeError) {
        // Network error (fetch failed to reach the server). Retry.
        lastError = new Error("无法连接到服务，请检查服务是否在运行。");
        if (attempt < retries) await new Promise((resolve) => setTimeout(resolve, 500 * (attempt + 1)));
        continue;
      }
      throw cause;
    }
  }
  throw lastError;
}

function asRecord(value: unknown): Record<string, any> {
  return value && typeof value === "object" ? value as Record<string, any> : {};
}

function contentToText(value: unknown): string {
  if (typeof value === "string") return value;
  if (Array.isArray(value)) return value.map(contentToText).filter(Boolean).join("\n");
  if (value && typeof value === "object") {
    const data = value as Record<string, unknown>;
    if (typeof data.text === "string") return data.text;
    if (typeof data.content === "string") return data.content;
    return JSON.stringify(value, null, 2);
  }
  return value == null ? "" : String(value);
}

function formatTime(value: string): string {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? "" : date.toLocaleTimeString("zh-CN", { hour: "2-digit", minute: "2-digit" });
}

function formatHistoryTime(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "";
  return date.toLocaleString("zh-CN", { month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit" });
}

function formatTokens(value: number): string {
  if (!Number.isFinite(value) || value <= 0) return "0";
  if (value >= 1_000_000) return `${(value / 1_000_000).toFixed(value >= 10_000_000 ? 0 : 1)}m`;
  if (value >= 1_000) return `${(value / 1_000).toFixed(value >= 10_000 ? 0 : 1)}k`;
  return String(value);
}

function formatDuration(value: number): string {
  if (!Number.isFinite(value) || value < 0) return "--";
  const seconds = Math.floor(value / 1000);
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  return minutes < 60 ? `${minutes}m ${seconds % 60}s` : `${Math.floor(minutes / 60)}h ${minutes % 60}m`;
}

function formatCost(value: number): string {
  return Number.isFinite(value) && value > 0 ? `$${value.toFixed(4)}` : "$0.0000";
}

function runDuration(run: RunUsage | undefined, now: number): number {
  if (!run) return 0;
  if (run.durationMs > 0) return run.durationMs;
  const startedAt = run.startedAt ? new Date(run.startedAt).getTime() : Number.NaN;
  return Number.isNaN(startedAt) ? 0 : Math.max(0, now - startedAt);
}

function contextLabel(context: RunUsage | undefined): string {
  if (!context?.contextInputTokens) return "上下文计算中";
  if (!context.contextWindow) return "上下文暂不可用";
  return `上下文约 ${Math.min(100, Math.round(context.contextInputTokens / context.contextWindow * 100))}%`;
}

function contextLevel(context: RunUsage | undefined): string {
  if (!context?.contextInputTokens || !context.contextWindow) return "pending";
  const percent = context.contextInputTokens / context.contextWindow * 100;
  return percent >= 85 ? "danger" : percent >= 70 ? "warning" : "normal";
}

function getApproval(event: Event): Approval | null {
  if (!event.type.startsWith("approval.")) return null;
  const payload = asRecord(event.payload);
  if (!payload.approvalId || !payload.toolInput) return null;
  return {
    approvalId: String(payload.approvalId),
    status: payload.status === "allow" || payload.status === "deny" ? payload.status : "pending",
    toolName: String(payload.toolName || "Bash"),
    toolInput: asRecord(payload.toolInput),
  };
}

function buildTimeline(messages: Message[], events: Event[]): TimelineItem[] {
  const results = new Map<string, ToolOutput>();
  const runStatuses = new Map<string, string>();
  const approvalStates = new Map<string, ApprovalEvent>();
  for (const event of events) {
    const payload = asRecord(event.payload);
    if (event.type === "user") {
      for (const part of asRecord(payload.message).content || []) {
        if (part?.type === "tool_result" && part.tool_use_id) results.set(String(part.tool_use_id), { content: contentToText(part.content), isError: Boolean(part.is_error) });
      }
    }
    if (event.type.startsWith("run.")) runStatuses.set(event.runId, event.type.slice(4));
    const approval = getApproval(event);
    if (approval) approvalStates.set(approval.approvalId, { approval, runId: event.runId, createdAt: event.createdAt });
  }
  const approvals = [...approvalStates.values()].sort((left, right) => new Date(left.createdAt).getTime() - new Date(right.createdAt).getTime());
  const usedApprovals = new Set<string>();

  const tools: ToolAction[] = [];
  for (const event of events) {
    if (event.type !== "assistant") continue;
    const payload = asRecord(event.payload);
    if (payload.parent_tool_use_id) continue;
    for (const part of asRecord(payload.message).content || []) {
      if (part?.type !== "tool_use" || !part.id) continue;
      if (part.name === "Agent") continue;
      const input = asRecord(part.input);
      const command = typeof input.command === "string" ? input.command : "";
      const matched = approvals.find((item) => !usedApprovals.has(item.approval.approvalId) && item.runId === event.runId && typeof item.approval.toolInput.command === "string" && item.approval.toolInput.command === command && new Date(item.createdAt).getTime() >= new Date(event.createdAt).getTime());
      if (matched) usedApprovals.add(matched.approval.approvalId);
      tools.push({
        id: String(part.id),
        runId: event.runId,
        name: String(part.name || "Tool"),
        input,
        createdAt: event.createdAt,
        output: results.get(String(part.id)),
        approval: matched?.approval,
        runStatus: runStatuses.get(event.runId),
      });
    }
  }
  const timeline: TimelineItem[] = [
    ...messages.map((message) => ({ kind: "message" as const, id: message.id, createdAt: message.createdAt, message })),
    ...tools.map((action) => ({ kind: "tool" as const, id: action.id, createdAt: action.createdAt, action })),
  ];
  return timeline.sort((left, right) => new Date(left.createdAt).getTime() - new Date(right.createdAt).getTime());
}

function eventMessageParts(event: Event): Record<string, any>[] {
  const content = asRecord(asRecord(event.payload).message).content;
  return Array.isArray(content) ? content.map(asRecord) : [];
}

function agentSummary(input: Record<string, any>): string {
  const value = input.description || input.prompt || input.task || input.agent_type || input.subagent_type;
  return typeof value === "string" && value.trim() ? value.trim().replace(/\s+/g, " ") : "执行子任务";
}

function toolSummary(part: Record<string, any>): string {
  const input = asRecord(part.input);
  const value = input.command || input.description || input.path || input.prompt || input.query;
  return typeof value === "string" && value.trim() ? value.trim() : "";
}

function agentStatusLabel(status: AgentStatus): string {
  return ({ pending: "等待开始", running: "执行中", completed: "已完成", failed: "执行失败", stopped: "已停止", unresolved: "结果未收齐" })[status];
}

function buildAgentExecutions(events: Event[]): AgentExecution[] {
  const nodes = new Map<string, AgentNode>();
  const toolOwners = new Map<string, string>();
  const runStatuses = new Map<string, string>();
  const runCreatedAt = new Map<string, string>();
  const ensureNode = (id: string, runId: string, parentId: string | undefined, name: string, summary: string, createdAt: string) => {
    const existing = nodes.get(id);
    if (existing) return existing;
    const node: AgentNode = { id, runId, parentId, name, summary, createdAt, status: "pending", logs: [], children: [] };
    nodes.set(id, node);
    return node;
  };
  const appendLog = (ownerID: string | undefined, log: AgentLog) => {
    if (!ownerID) return;
    const owner = nodes.get(ownerID);
    if (!owner) return;
    owner.logs.push(log);
    if (owner.status === "pending") owner.status = "running";
  };

  for (const event of events) {
    if (!runCreatedAt.has(event.runId)) runCreatedAt.set(event.runId, event.createdAt);
    if (event.type.startsWith("run.")) runStatuses.set(event.runId, event.type.slice(4));
    const payload = asRecord(event.payload);
    const parentID = typeof payload.parent_tool_use_id === "string" && payload.parent_tool_use_id ? payload.parent_tool_use_id : undefined;
    if (event.type === "assistant") {
      for (const part of eventMessageParts(event)) {
        if (part.type === "tool_use" && typeof part.id === "string") {
          const name = typeof part.name === "string" ? part.name : "Tool";
          const input = asRecord(part.input);
          if (name === "Agent") ensureNode(part.id, event.runId, parentID, name, agentSummary(input), event.createdAt);
          toolOwners.set(part.id, parentID || "");
          appendLog(parentID, { id: event.id + part.id, createdAt: event.createdAt, kind: "tool", title: name, detail: toolSummary(part) });
        }
        if (part.type === "text" && typeof part.text === "string" && part.text.trim()) appendLog(parentID, { id: event.id + part.text, createdAt: event.createdAt, kind: "text", title: "输出", detail: part.text.trim() });
      }
      continue;
    }
    if (event.type === "user") {
      for (const part of eventMessageParts(event)) {
        if (part.type !== "tool_result" || typeof part.tool_use_id !== "string") continue;
        const ownerID = toolOwners.get(part.tool_use_id);
        const output = contentToText(part.content);
        appendLog(ownerID, { id: event.id + part.tool_use_id, createdAt: event.createdAt, kind: "result", title: part.is_error ? "工具失败" : "工具结果", detail: output, isError: Boolean(part.is_error) });
        const agent = nodes.get(part.tool_use_id);
        if (agent) agent.status = part.is_error ? "failed" : "completed";
      }
    }
    if (event.type === "stream.error") appendLog(parentID, { id: event.id, createdAt: event.createdAt, kind: "error", title: "流错误", detail: String(payload.error || "无法读取 Claude 输出"), isError: true });
  }

  const executions = new Map<string, AgentExecution>();
  for (const node of nodes.values()) {
    const execution = executions.get(node.runId) || { runId: node.runId, status: runStatuses.get(node.runId) || "running", incomplete: false, agents: [], createdAt: runCreatedAt.get(node.runId) || node.createdAt };
    executions.set(node.runId, execution);
    if (node.parentId && nodes.has(node.parentId)) nodes.get(node.parentId)!.children.push(node);
    else execution.agents.push(node);
  }
  for (const execution of executions.values()) {
    execution.status = runStatuses.get(execution.runId) || execution.status;
    if (["completed", "failed", "stopped", "interrupted", "cancelled"].includes(execution.status)) {
      const markUnresolved = (node: AgentNode) => {
        if (node.status === "pending" || node.status === "running") {
          if (execution.status === "completed") {
            node.status = "unresolved";
            execution.incomplete = true;
          } else {
            node.status = execution.status === "failed" ? "failed" : "stopped";
          }
        }
        node.children.forEach(markUnresolved);
      };
      execution.agents.forEach(markUnresolved);
    }
  }
  return [...executions.values()].sort((left, right) => new Date(left.createdAt).getTime() - new Date(right.createdAt).getTime());
}

function subagentTextIndex(events: Event[]): Map<string, number[]> {
  const index = new Map<string, number[]>();
  for (const event of events) {
    const payload = asRecord(event.payload);
    if (event.type !== "assistant" || !payload.parent_tool_use_id) continue;
    const eventTime = new Date(event.createdAt).getTime();
    for (const part of eventMessageParts(event)) {
      if (part.type !== "text" || typeof part.text !== "string") continue;
      const times = index.get(part.text) || [];
      times.push(eventTime);
      index.set(part.text, times);
    }
  }
  return index;
}

function isSubagentMessage(message: Message, indexedTexts: Map<string, number[]>): boolean {
  if (message.parentToolUseId) return true;
  // New records have durable attribution. Keep the text/time fallback strictly
  // for records created before run and parent IDs were persisted.
  if (message.runId) return false;
  if (message.role !== "assistant") return false;
  const messageTime = new Date(message.createdAt).getTime();
  return (indexedTexts.get(message.content) || []).some((eventTime) => Math.abs(eventTime - messageTime) <= 5_000);
}

function mergeConversationItems<T extends { id: string; createdAt: string }>(history: T[], live: T[]): T[] {
  const items = new Map<string, T>();
  for (const item of history) items.set(item.id, item);
  for (const item of live) items.set(item.id, item);
  return [...items.values()].sort((left, right) => new Date(left.createdAt).getTime() - new Date(right.createdAt).getTime());
}

function websocketAssistantMessageID(eventID: string, index: number): string {
  return `${eventID}:assistant-text:${index}`;
}

function requiredShortcutVariables(template: string): ("selection" | "error")[] {
  return (["selection", "error"] as const).filter((key) => template.includes(`\${${key}}`));
}

export function App() {
  const [projects, setProjects] = useState<Project[]>([]);
  const [projectStatuses, setProjectStatuses] = useState<Record<string, ProjectStatus>>({});
  const [project, setProject] = useState<Project | null>(null);
  const [showImport, setShowImport] = useState(false);
  const [error, setError] = useState("");
  const loadProjects = useCallback(async () => {
    try {
      const list = await api<Project[]>("/api/projects");
      setProjects(list);
      const entries = await Promise.all(list.map(async (item) => {
        try {
          const conversations = await api<Conversation[]>(`/api/projects/${item.id}/conversations`);
          const active = conversations.find((conversation) => conversation.status === "running");
          return [item.id, { running: Boolean(active), conversationCount: conversations.length, activeTitle: active?.title || "" }] as const;
        } catch {
          return [item.id, { running: false, conversationCount: 0, activeTitle: "" }] as const;
        }
      }));
      setProjectStatuses(Object.fromEntries(entries));
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Unable to load projects");
    }
  }, []);

  useEffect(() => { void loadProjects(); }, [loadProjects]);

  if (window.location.pathname === "/design-preview") return <DesignPreview />;

  return <main className={project ? "app-shell project-open" : "app-shell"}>
    {error && <div className="error"><span>{error}</span><button title="关闭错误提示" onClick={() => setError("")}>x</button></div>}
    {project ? <Chat key={project.id} project={project} fail={setError} back={() => setProject(null)} /> : <ProjectDashboard projects={projects} statuses={projectStatuses} importProject={() => setShowImport(true)} openProject={setProject} />}
    {showImport && <Importer close={() => setShowImport(false)} created={(item) => { setShowImport(false); setProject(item); void loadProjects(); }} fail={setError} />}
  </main>;
}

function ProjectDashboard({ projects, statuses, importProject, openProject }: { projects: Project[]; statuses: Record<string, ProjectStatus>; importProject: () => void; openProject: (project: Project) => void }) {
  const [filter, setFilter] = useState<ProjectFilter>("all");
  const runningCount = Object.values(statuses).filter((status) => status.running).length;
  const readyCount = projects.filter((project) => project.claudeReady && !statuses[project.id]?.running).length;
  const offlineCount = projects.filter((project) => !project.claudeReady).length;
  const projectState = (project: Project): ProjectFilter => statuses[project.id]?.running ? "running" : project.claudeReady ? "ready" : "offline";
  const filteredProjects = projects.filter((project) => filter === "all" || projectState(project) === filter).sort((left, right) => {
    const order: Record<ProjectFilter, number> = { running: 0, ready: 1, offline: 2, all: 3 };
    return order[projectState(left)] - order[projectState(right)] || left.name.localeCompare(right.name, "zh-CN");
  });
  const filters: { id: ProjectFilter; label: string; count: number }[] = [{ id: "all", label: "全部", count: projects.length }, { id: "running", label: "进行中", count: runningCount }, { id: "ready", label: "可接手", count: readyCount }, { id: "offline", label: "不可用", count: offlineCount }];
  return <><header className="dashboard-bar"><a className="brand" href="/"><b>A</b><span>Auto<br />Development</span></a><div className="dashboard-actions"><a className="design-link" href="/design-preview">设计方向</a><span className="runner-label">WSL LOCAL RUNNER</span><button className="primary" onClick={importProject}>加载项目 <i>+</i></button></div></header><section className="project-dashboard command-dashboard"><header className="dashboard-intro"><div><span className="eyebrow">COMMAND DESK</span><h1>项目任务<br /><em>总览。</em></h1></div><div className="dashboard-summary"><div><small>全部项目</small><b>{projects.length}</b></div><div className="summary-running"><small>进行中</small><b>{runningCount}</b></div><div><small>可接手</small><b>{readyCount}</b></div></div></header>{projects.length > 0 && <nav className="task-filters" aria-label="按项目状态筛选">{filters.map((item) => <button key={item.id} className={filter === item.id ? "selected" : ""} aria-pressed={filter === item.id} onClick={() => setFilter(item.id)}>{item.label}<b>{item.count}</b></button>)}</nav>}<div className="dashboard-divider"><span>任务队列</span><i></i><small>{filter === "all" ? "执行中的任务已置顶" : `显示${filters.find((item) => item.id === filter)?.label}项目`}</small></div>{projects.length === 0 ? <div className="empty"><h2>还没有项目</h2><p>加载一个 WSL 本地目录后，即可在这里开始 Claude Code 对话。</p><button className="primary" onClick={importProject}>加载项目</button></div> : filteredProjects.length === 0 ? <div className="empty filtered-empty"><h2>没有匹配的项目</h2><p>当前筛选下没有项目，可切换筛选查看其他任务。</p></div> : <div className="project-grid">{filteredProjects.map((item) => <ProjectCard key={item.id} project={item} status={statuses[item.id]} open={() => openProject(item)} />)}</div>}</section></>;
}

function ProjectCard({ project, status, open }: { project: Project; status?: ProjectStatus; open: () => void }) {
  const state = status?.running ? "正在执行" : project.claudeReady ? "已就绪" : "Claude 不可用";
  const stateClass = status?.running ? "running" : project.claudeReady ? "ready" : "offline";
  const taskTitle = status?.running && status.activeTitle ? status.activeTitle : "等待新的任务";
  return <button className={`project-card task-card state-${stateClass}`} onClick={open} aria-label={`进入 ${project.name} 的对话`}><div className="project-card-top"><span className="project-mark">{project.name.slice(0, 1).toUpperCase()}</span><span className={`project-state ${stateClass}`}><i></i>{state}</span></div><div className="project-card-title"><span>当前任务</span><h2>{project.name}</h2><p>{taskTitle}</p></div><div className="project-card-meta"><span><small>会话</small><b>{status ? status.conversationCount : "--"}</b></span><span><small>工作目录</small><b title={project.pathDisplay}>{project.pathDisplay}</b></span></div><footer><span>{project.gitBranch || "非 Git 项目"}</span><b aria-hidden="true">打开对话 <i>→</i></b></footer></button>;
}

type DesignDirection = "workbench" | "console" | "ide" | "command";

const designDirections: { id: DesignDirection; name: string; description: string }[] = [
  { id: "workbench", name: "开发者工作台", description: "紧凑、沉稳，以对话和执行流为中心。" },
  { id: "console", name: "沉浸式 AI 控制台", description: "突出 AI 正在工作的过程，减少外围干扰。" },
  { id: "ide", name: "轻量 IDE", description: "为文件、分支与部署预留清晰的三栏空间。" },
  { id: "command", name: "任务指挥台", description: "用任务状态和交付信息组织自动开发工作。" },
];

function DesignPreview() {
  const [selected, setSelected] = useState<DesignDirection>("workbench");
  const direction = designDirections.find((item) => item.id === selected)!;
  return <main className="design-preview">
    <header className="preview-header"><a href="/">Auto Development</a><div><span>视觉方向预览</span><b>选择一个方向，再进入正式设计</b></div></header>
    <section className="preview-intro"><h1>对话式自动开发工作台</h1><p>四种方向均保留项目、对话、命令确认和使用状态，只调整信息层级与视觉表达。</p></section>
    <nav className="preview-selector" aria-label="设计方向">{designDirections.map((item) => <button key={item.id} className={selected === item.id ? "selected" : ""} onClick={() => setSelected(item.id)}><b>{item.name}</b><span>{item.description}</span></button>)}</nav>
    <section className={`preview-stage ${selected}`} aria-label={`${direction.name}预览`}><PreviewFrame direction={selected} /><div className="preview-caption"><span>当前方向</span><h2>{direction.name}</h2><p>{direction.description}</p></div></section>
    <section className="preview-grid" aria-label="全部方向缩略预览">{designDirections.map((item) => <button key={item.id} className={`preview-thumbnail ${item.id} ${selected === item.id ? "selected" : ""}`} onClick={() => setSelected(item.id)}><PreviewFrame direction={item.id} compact /><span>{item.name}</span></button>)}</section>
  </main>;
}

function PreviewFrame({ direction, compact = false }: { direction: DesignDirection; compact?: boolean }) {
  const title = direction === "command" ? "任务 #024 / 侧边栏 Tab" : "auto_test";
  return <div className={`preview-frame ${direction} ${compact ? "compact" : ""}`}>
    <aside className="preview-rail"><b>A</b><i></i><i></i><i></i><small>+</small></aside>
    {direction === "ide" && <aside className="preview-explorer"><span>EXPLORER</span><b>auto_test</b><em>src</em><em>components</em><em>Sidebar.tsx</em><em>package.json</em></aside>}
    <div className="preview-main"><header><div><span>{direction === "console" ? "CLAUDE SESSION" : direction === "command" ? "ACTIVE TASK" : "PROJECT"}</span><h3>{title}</h3></div><div className="preview-head-meta"><small>Sonnet</small><small>ctx 24%</small><b>运行中</b></div></header>
      <div className="preview-content">{direction === "command" && <div className="preview-task-line"><b>实现侧边栏 Tab</b><span>分析中</span><i></i><i></i><i></i></div>}
        {direction === "ide" && <div className="preview-tabs"><b>Sidebar.tsx</b><span>App.tsx</span></div>}
        <div className="preview-response"><span>Claude</span><p>我会先检查现有侧边栏结构，再添加一个可配置的 Tab，并确保移动端布局保持稳定。</p></div>
        <div className="preview-command"><div><b>$</b><span>检查当前项目结构</span><em>刚刚</em></div><code>rg --files src</code><small>等待确认</small></div>
        <div className="preview-response short"><span>Claude</span><p>已找到导航组件，准备修改。</p></div>
      </div>
      <footer><span>描述希望 Claude 完成的工作...</span><b>发送</b></footer>
    </div>
    {direction === "ide" && <aside className="preview-inspector"><span>INSPECTOR</span><b>本次任务</b><em>3 次工具调用</em><em>1.2k 输入 Token</em><em>$0.0042 估算</em></aside>}
  </div>;
}

function Importer({ close, created, fail }: { close: () => void; created: (project: Project) => void; fail: (message: string) => void }) {
  const [path, setPath] = useState("");
  const [parent, setParent] = useState("");
  const [dirs, setDirs] = useState<Directory[]>([]);
  const [result, setResult] = useState<any>(null);
  const [busy, setBusy] = useState(false);
  const browse = useCallback(async (target = "") => {
    try {
      const data = await api<{ path: string; parent: string; directories: Directory[] }>(`/api/directories${target ? `?path=${encodeURIComponent(target)}` : ""}`);
      setPath(data.path); setParent(data.parent); setDirs(data.directories); setResult(null);
    } catch (cause) { fail(cause instanceof Error ? cause.message : "Unable to browse directories"); }
  }, [fail]);
  useEffect(() => { void browse(); }, [browse]);
  const validate = async () => {
    setBusy(true);
    try { setResult(await api("/api/projects/validate", { method: "POST", body: JSON.stringify({ path }) })); }
    catch (cause) { fail(cause instanceof Error ? cause.message : "Unable to validate directory"); }
    finally { setBusy(false); }
  };
  const create = async () => {
    setBusy(true);
    try { created(await api<Project>("/api/projects", { method: "POST", body: JSON.stringify({ path, name: result?.name }) })); }
    catch (cause) { fail(cause instanceof Error ? cause.message : "Unable to load project"); setBusy(false); }
  };
  return <div className="backdrop" role="dialog" aria-modal="true"><section className="modal"><header><div><label>WSL LOCAL RUNNER</label><h2>加载项目</h2></div><button title="关闭" onClick={close}>x</button></header><div className="path"><button title="上级目录" onClick={() => void browse(parent)}>Up</button><code>{path}</code><button className="secondary" disabled={busy} onClick={() => void validate()}>校验目录</button></div><div className="dirs">{dirs.map((dir) => <button key={dir.path} onClick={() => void browse(dir.path)}><b>{dir.name}</b><small>{dir.path}</small></button>)}</div>{result && <div className={result.claudeReady ? "valid" : "invalid"}><b>{result.name}</b><span>Git: {result.gitReady ? result.gitBranch : "未检测到 Git 仓库（仍可加载）"}</span><span>Claude Code: {result.claudeReady ? "可用" : "不可用"}</span></div>}<footer><button className="secondary" onClick={close}>取消</button><button className="primary" disabled={!result?.claudeReady || busy} onClick={() => void create()}>{busy ? "处理中" : "确认加载"}</button></footer></section></div>;
}

function Chat({ project, fail, back }: { project: Project; fail: (message: string) => void; back: () => void }) {
  const [conversation, setConversation] = useState<Conversation | null>(null);
  const [conversationHistory, setConversationHistory] = useState<Conversation[]>([]);
  const [messages, setMessages] = useState<Message[]>([]);
  const [events, setEvents] = useState<Event[]>([]);
  const [text, setText] = useState("");
  const [inputHistory, setInputHistory] = useState<string[]>([]);
  const [historyRefresh, setHistoryRefresh] = useState(0);
  const [run, setRun] = useState("");
  const [sending, setSending] = useState(false);
  const [resolving, setResolving] = useState("");
  const [stopping, setStopping] = useState(false);
  const [changingPermission, setChangingPermission] = useState(false);
  const [showNewConversation, setShowNewConversation] = useState(false);
  const [showHistory, setShowHistory] = useState(false);
  const [showFullControlConfirmation, setShowFullControlConfirmation] = useState(false);
  const [activatingConversation, setActivatingConversation] = useState("");
  const [showPermissionMenu, setShowPermissionMenu] = useState(false);
  const [usage, setUsage] = useState<ConversationUsageResponse | null>(null);
  const [showUsage, setShowUsage] = useState(false);
  const [usageNow, setUsageNow] = useState(Date.now());
  const [shortcuts, setShortcuts] = useState<Shortcut[]>([]);
  const [shortcutEditor, setShortcutEditor] = useState<ShortcutEditorState | null>(null);
  const [shortcutVariables, setShortcutVariables] = useState<{ shortcut: Shortcut; variables: Record<string, string> } | null>(null);
  const [shortcutBusy, setShortcutBusy] = useState("");
  const [showTasks, setShowTasks] = useState<{ taskID?: string } | null>(null);
  const [showGit, setShowGit] = useState(false);
  const [showAgentExecution, setShowAgentExecution] = useState<string | null>(null);
  const [hasMoreHistory, setHasMoreHistory] = useState(false);
  const [historyCursor, setHistoryCursor] = useState("");
  const [loadingOlderHistory, setLoadingOlderHistory] = useState(false);
  const bottom = useRef<HTMLDivElement>(null);
  const top = useRef<HTMLDivElement>(null);
  const historyIndex = useRef<number | null>(null);
  const draftBeforeHistory = useRef("");
  const finishedRunIds = useRef(new Set<string>());
  const usageRequestVersion = useRef(0);
  const usageConversationID = useRef<string | null>(null);
  const rememberFinishedRun = (runID: string) => {
    const finished = finishedRunIds.current;
    finished.add(runID);
    if (finished.size > 128) {
      const oldest = finished.values().next().value;
      if (oldest !== undefined) finished.delete(oldest);
    }
  };

  const refreshConversationHistory = useCallback(async () => {
    const list = await api<Conversation[]>(`/api/projects/${project.id}/conversations`);
    setConversationHistory(list);
    return list;
  }, [project.id]);
  const refreshShortcuts = useCallback(async () => {
    setShortcuts(await api<Shortcut[]>(`/api/projects/${project.id}/shortcuts?includeDisabled=true`));
  }, [project.id]);
  const openConversationHistory = () => {
    setShowHistory(true);
    void refreshConversationHistory().catch((cause) => fail(cause instanceof Error ? cause.message : "Unable to refresh conversation history"));
  };
  const refreshUsage = useCallback(async () => {
    const conversationID = conversation?.id;
    if (!conversationID) return;
    const requestVersion = ++usageRequestVersion.current;
    let data: ConversationUsageResponse;
    try {
      data = await api<ConversationUsageResponse>(`/api/conversations/${conversationID}/usage`);
    } catch (cause) {
      if (requestVersion === usageRequestVersion.current && usageConversationID.current === conversationID) throw cause;
      return;
    }
    if (requestVersion === usageRequestVersion.current && usageConversationID.current === conversationID) setUsage(data);
  }, [conversation?.id]);

  useEffect(() => {
    usageConversationID.current = conversation?.id || null;
    usageRequestVersion.current += 1;
    setUsage(null);
  }, [conversation?.id]);

  useEffect(() => {
    let cancelled = false;
    async function loadConversation() {
      try {
        const list = await refreshConversationHistory();
        const next = list[0] ?? await api<Conversation>(`/api/projects/${project.id}/conversations`, { method: "POST" });
        if (!cancelled) {
          setConversation(next);
          if (list.length === 0) setConversationHistory([next]);
        }
      } catch (cause) { if (!cancelled) fail(cause instanceof Error ? cause.message : "Unable to create conversation"); }
    }
    void loadConversation();
    return () => { cancelled = true; };
  }, [project.id, fail, refreshConversationHistory]);

  useEffect(() => {
    void (async () => {
      try {
        await api("/api/shortcuts/defaults", { method: "POST" });
        await refreshShortcuts();
      } catch (cause) { fail(cause instanceof Error ? cause.message : "Unable to load shortcuts"); }
    })();
  }, [fail, refreshShortcuts]);

  useEffect(() => {
    if (!conversation) return undefined;
    let cancelled = false;
    let socket: WebSocket;
    let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
    let reconnectAttempts = 0;

    const reload = async () => {
      try {
        const data = await api<{ messages: Message[]; events: Event[]; activeRunId: string | null; hasMore: boolean; nextCursor: string }>(`/api/conversations/${conversation.id}?limit=400`);
        if (cancelled) return;
        // A WebSocket frame may arrive after the HTTP query has started. Merge
        // instead of replacing state so that frame cannot be lost on first load.
        setMessages((current) => mergeConversationItems(data.messages, current));
        setEvents((current) => mergeConversationItems(data.events, current));
        setRun(data.activeRunId || ""); setHasMoreHistory(data.hasMore); setHistoryCursor(data.nextCursor || "");
      } catch (cause) { if (!cancelled) fail(cause instanceof Error ? cause.message : "Unable to refresh conversation"); }
    };

    const connect = () => {
      if (cancelled) return;
      reconnectAttempts++;
      socket = new WebSocket(`${location.protocol === "https:" ? "wss" : "ws"}://${location.host}/ws/conversations/${conversation.id}`);
      socket.onopen = () => {
        reconnectAttempts = 0;
        void reload();
      };
      socket.onmessage = (raw) => {
        const event = JSON.parse(raw.data) as Event;
        setEvents((old) => old.some((item) => item.id === event.id) ? old : [...old, event]);
        if (event.type === "assistant") {
          const content = asRecord(asRecord(event.payload).message).content || [];
          const parentToolUseId = typeof asRecord(event.payload).parent_tool_use_id === "string" ? asRecord(event.payload).parent_tool_use_id : "";
          for (const [index, part] of content.entries()) {
            if (part?.type === "text" && typeof part.text === "string") setMessages((old) => mergeConversationItems(old, [{ id: websocketAssistantMessageID(event.id, index), runId: event.runId, role: "assistant", content: part.text, parentToolUseId, createdAt: event.createdAt }]));
          }
        }
        if (event.type === "usage.updated" || event.type.startsWith("run.")) {
          void refreshUsage().catch((cause) => fail(cause instanceof Error ? cause.message : "Unable to refresh usage"));
        }
        if (event.type.startsWith("run.")) { rememberFinishedRun(event.runId); setRun((active) => active === event.runId ? "" : active); setStopping(false); }
      };
      socket.onerror = () => {
        // The onclose handler will fire after this and handle reconnection.
      };
      socket.onclose = (event) => {
        if (cancelled) return;
        // Don't reconnect on normal closure (code 1000) or when the component
        // is unmounting. Also stop after too many failed attempts.
        const maxAttempts = 12;
        if (reconnectAttempts >= maxAttempts) {
          fail("实时连接已断开，请刷新页面重新建立连接。");
          return;
        }
        const delay = Math.min(500 * Math.pow(2, reconnectAttempts - 1), 15_000);
        reconnectTimer = setTimeout(connect, delay);
      };
    };

    connect();

    return () => {
      cancelled = true;
      if (reconnectTimer) clearTimeout(reconnectTimer);
      socket.close();
    };
  }, [conversation?.id, fail, refreshUsage]);

  useEffect(() => {
    if (!conversation) return undefined;
    void refreshUsage().catch((cause) => fail(cause instanceof Error ? cause.message : "Unable to load usage"));
    return undefined;
  }, [conversation?.id, fail, refreshUsage]);

  useEffect(() => {
    if (!run) return undefined;
    setUsageNow(Date.now());
    const timer = window.setInterval(() => setUsageNow(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, [run]);

  useEffect(() => {
    if (!conversation) return;
    historyIndex.current = null;
    draftBeforeHistory.current = "";
    setInputHistory([]);
  }, [conversation?.id]);

  useEffect(() => {
    if (!conversation) return undefined;
    let cancelled = false;
    const loadHistory = async () => {
      try {
        const history = await api<string[]>(`/api/conversations/${conversation.id}/input-history?limit=100`);
        if (!cancelled) setInputHistory(history);
      } catch (cause) { if (!cancelled) fail(cause instanceof Error ? cause.message : "Unable to load input history"); }
    };
    void loadHistory();
    return () => { cancelled = true; };
  }, [conversation?.id, fail, historyRefresh]);

  const knownSubagentTexts = useMemo(() => subagentTextIndex(events), [events]);
  const primaryMessages = useMemo(() => messages.filter((message) => !isSubagentMessage(message, knownSubagentTexts)), [knownSubagentTexts, messages]);
  const timeline = useMemo(() => buildTimeline(primaryMessages, events), [events, primaryMessages]);
  const agentExecutions = useMemo(() => buildAgentExecutions(events), [events]);
  const executionByRun = useMemo(() => new Map(agentExecutions.map((execution) => [execution.runId, execution])), [agentExecutions]);
  const anchoredExecutionRunIDs = useMemo(() => new Set(primaryMessages.flatMap((message) => message.role === "user" && message.runId && executionByRun.has(message.runId) ? [message.runId] : [])), [executionByRun, primaryMessages]);
  const loadOlderHistory = async () => {
    if (!conversation || !historyCursor || loadingOlderHistory) return;
    setLoadingOlderHistory(true);
    try {
      const data = await api<{ messages: Message[]; events: Event[]; hasMore: boolean; nextCursor: string }>(`/api/conversations/${conversation.id}?limit=400&cursor=${encodeURIComponent(historyCursor)}`);
      setMessages((current) => mergeConversationItems(data.messages, current));
      setEvents((current) => mergeConversationItems(data.events, current));
      setHasMoreHistory(data.hasMore);
      setHistoryCursor(data.nextCursor || "");
    } catch (cause) { fail(cause instanceof Error ? cause.message : "Unable to load earlier conversation history"); }
    finally { setLoadingOlderHistory(false); }
  };
  const pendingApproval = useMemo(() => {
    for (const item of timeline) {
      if (item.kind === "tool") {
        const approval = item.action.approval;
        if (approval?.status === "pending" && !item.action.output) return item.action;
      }
    }
    return null;
  }, [timeline]);
  useEffect(() => {
    if (pendingApproval) return; // never scroll away while a command is waiting for approval
    bottom.current?.scrollIntoView({ behavior: "smooth", block: "end" });
  }, [timeline.length, run, pendingApproval]);

  const scrollToTop = () => top.current?.scrollIntoView({ behavior: "smooth", block: "start" });
  const scrollToBottom = () => bottom.current?.scrollIntoView({ behavior: "smooth", block: "end" });

  const sendContent = async (rawContent: string, clearDraft = true) => {
    if (!conversation || sending || shortcutBusy) return;
    const content = rawContent.trim();
    if (!content) return;
    if (content === "/resume") { if (clearDraft) setText(""); openConversationHistory(); return; }
    setSending(true);
    if (clearDraft) setText("");
    setShowPermissionMenu(false); historyIndex.current = null; draftBeforeHistory.current = "";
    try {
      const data = await api<{ message: Message; runId: string }>(`/api/conversations/${conversation.id}/messages`, { method: "POST", body: JSON.stringify({ content }) });
      setMessages((old) => [...old, data.message]); setInputHistory((old) => [...old.slice(-99), data.message.content]); setHistoryRefresh((version) => version + 1); setRun(finishedRunIds.current.has(data.runId) ? "" : data.runId);
      void refreshUsage().catch((cause) => fail(cause instanceof Error ? cause.message : "Unable to refresh usage"));
      void refreshConversationHistory().catch((cause) => fail(cause instanceof Error ? cause.message : "Unable to refresh conversation history"));
    } catch (cause) { fail(cause instanceof Error ? cause.message : "Unable to send message"); if (clearDraft) setText(content); }
    finally { setSending(false); }
  };
  const send = (event: FormEvent) => {
    event.preventDefault();
    void sendContent(text);
  };
  const runShortcut = async (shortcut: Shortcut, variables: Record<string, string> = {}, variablesReady = false) => {
    if (!conversation || sending || shortcutBusy) return;
    const required = requiredShortcutVariables(shortcut.template);
    if (!variablesReady && required.length) { setShortcutVariables({ shortcut, variables }); return; }
    if (shortcut.defaultAction === "fill") {
      setShortcutBusy(shortcut.id);
      try {
        const preview = await api<{ content: string }>(`/api/conversations/${conversation.id}/shortcuts/${shortcut.id}/preview`, { method: "POST", body: JSON.stringify({ variables }) });
        setText(preview.content);
      } catch (cause) { fail(cause instanceof Error ? cause.message : "Unable to prepare shortcut"); }
      finally { setShortcutBusy(""); }
      return;
    }
    setShortcutBusy(shortcut.id);
    try {
      const action = shortcut.defaultAction === "confirm" ? "confirm" : "run";
      const data = await api<{ message: Message; runId: string }>(`/api/conversations/${conversation.id}/shortcuts/${shortcut.id}/run`, { method: "POST", body: JSON.stringify({ variables, action }) });
      setMessages((old) => [...old, data.message]);
      setInputHistory((old) => [...old.slice(-99), data.message.content]);
      setHistoryRefresh((version) => version + 1);
      setRun(finishedRunIds.current.has(data.runId) ? "" : data.runId);
      void refreshUsage().catch((cause) => fail(cause instanceof Error ? cause.message : "Unable to refresh usage"));
      void refreshConversationHistory().catch((cause) => fail(cause instanceof Error ? cause.message : "Unable to refresh conversation history"));
    } catch (cause) { fail(cause instanceof Error ? cause.message : "Unable to run shortcut"); }
    finally { setShortcutBusy(""); }
  };
  const newConversation = async (permissionMode: PermissionMode) => {
    try {
      const next = await api<Conversation>(`/api/projects/${project.id}/conversations?new=true`, { method: "POST", body: JSON.stringify({ permissionMode }) });
      setMessages([]); setEvents([]); setRun(""); setUsage(null); setInputHistory([]); historyIndex.current = null; draftBeforeHistory.current = ""; finishedRunIds.current.clear(); setShowPermissionMenu(false); setShowAgentExecution(null); setHasMoreHistory(false); setHistoryCursor(""); setConversation(next);
      void refreshConversationHistory().catch((cause) => fail(cause instanceof Error ? cause.message : "Unable to refresh conversation history"));
      setShowNewConversation(false);
    }
    catch (cause) { fail(cause instanceof Error ? cause.message : "Unable to start a new conversation"); }
  };
  const activateConversation = async (item: Conversation) => {
    if (!conversation || item.id === conversation.id || item.status === "running") { setShowHistory(false); return; }
    setActivatingConversation(item.id);
    try {
      const next = await api<Conversation>(`/api/conversations/${item.id}/activate`, { method: "POST" });
      setMessages([]); setEvents([]); setRun(""); setUsage(null); setInputHistory([]); historyIndex.current = null; draftBeforeHistory.current = ""; finishedRunIds.current.clear(); setShowPermissionMenu(false); setShowAgentExecution(null); setHasMoreHistory(false); setHistoryCursor(""); setConversation(next); setShowHistory(false);
      void refreshConversationHistory().catch((cause) => fail(cause instanceof Error ? cause.message : "Unable to refresh conversation history"));
    } catch (cause) { fail(cause instanceof Error ? cause.message : "Unable to switch conversation"); }
    finally { setActivatingConversation(""); }
  };
  const changePermissionMode = async (permissionMode: PermissionMode) => {
    if (!conversation || conversation.permissionMode === permissionMode || run) return;
    if (permissionMode === "full_control") { setShowPermissionMenu(false); setShowFullControlConfirmation(true); return; }
    setChangingPermission(true);
    try {
      setConversation(await api<Conversation>(`/api/conversations/${conversation.id}/permission-mode`, { method: "POST", body: JSON.stringify({ permissionMode }) }));
      setShowPermissionMenu(false);
    }
    catch (cause) { fail(cause instanceof Error ? cause.message : "Unable to change permission mode"); }
    finally { setChangingPermission(false); }
  };
  const confirmFullControl = async () => {
    if (!conversation || run) return;
    setChangingPermission(true);
    try {
      setConversation(await api<Conversation>(`/api/conversations/${conversation.id}/permission-mode`, { method: "POST", body: JSON.stringify({ permissionMode: "full_control" }) }));
      setShowFullControlConfirmation(false);
    } catch (cause) { fail(cause instanceof Error ? cause.message : "Unable to change permission mode"); }
    finally { setChangingPermission(false); }
  };
  const decide = async (approvalId: string, decision: "allow" | "deny") => {
    setResolving(approvalId);
    try { await api(`/api/approvals/${approvalId}`, { method: "POST", body: JSON.stringify({ decision }) }); }
    catch (cause) { fail(cause instanceof Error ? cause.message : "Unable to resolve command approval"); }
    finally { setResolving(""); }
  };
  const stopRun = async () => {
    if (!run || stopping) return;
    const runID = run;
    setStopping(true);
    try {
      const result = await api<{ status: string }>(`/api/runs/${runID}/stop`, { method: "POST" });
      if (result.status !== "stopping") { rememberFinishedRun(runID); setRun((active) => active === runID ? "" : active); setStopping(false); }
    }
    catch (cause) { fail(cause instanceof Error ? cause.message : "Unable to stop run"); setStopping(false); }
  };
  const handleTextChange = (value: string) => {
    historyIndex.current = null;
    draftBeforeHistory.current = "";
    setText(value);
  };
  const navigateInputHistory = (event: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if (event.nativeEvent.isComposing || event.shiftKey || (event.key !== "ArrowUp" && event.key !== "ArrowDown")) return;
    const textarea = event.currentTarget;
    const browsing = historyIndex.current !== null;
    if (!browsing && textarea.selectionStart !== textarea.selectionEnd) return;
    if (event.key === "ArrowUp") {
      if (inputHistory.length === 0 || (!browsing && textarea.value.slice(0, textarea.selectionStart).includes("\n"))) return;
      event.preventDefault();
      if (!browsing) {
        draftBeforeHistory.current = text;
        historyIndex.current = inputHistory.length - 1;
      } else {
        historyIndex.current = Math.max(0, historyIndex.current! - 1);
      }
      setText(inputHistory[historyIndex.current!]);
      return;
    }
    const currentIndex = historyIndex.current;
    if (currentIndex === null) return;
    event.preventDefault();
    if (currentIndex < inputHistory.length - 1) {
      const nextIndex = currentIndex + 1;
      historyIndex.current = nextIndex;
      setText(inputHistory[nextIndex]);
    } else {
      historyIndex.current = null;
      setText(draftBeforeHistory.current);
      draftBeforeHistory.current = "";
    }
  };

  const permissionLabel = conversation?.permissionMode === "full_control" ? "完全控制" : "默认权限";
  const runLabel = conversation?.permissionMode === "full_control" ? "Claude 正在处理已发送内容" : "Claude 正在分析或等待命令确认";
  const currentUsage = usage?.currentRun ?? usage?.latestRun;
  const displayedModel = usage?.context.model || currentUsage?.model || "Claude Code";
  const promptShortcuts = shortcuts.filter((shortcut) => shortcut.kind === "prompt" || shortcut.kind === "snippet");
  const commandShortcuts = shortcuts.filter((shortcut) => shortcut.kind === "command_request");
  const promptShortcutItems = promptShortcuts.length > 0 ? promptShortcuts : [undefined];
  const commandShortcutItems = commandShortcuts.length > 0 ? commandShortcuts : [undefined];
  const shortcutRowCount = Math.max(promptShortcutItems.length, commandShortcutItems.length);
  const renderShortcutCell = (shortcut: Shortcut | undefined, kind: "prompt" | "command_request", placeholder = false) => {
    if (!shortcut && placeholder) return <div className="quick-tag-slot" aria-hidden="true" />;
    if (!shortcut) return <button type="button" className={`quick-tag-empty${kind === "command_request" ? " command-tag" : ""}`} onClick={() => setShortcutEditor({ kind })}>{kind === "command_request" ? "添加命令" : "添加提示词"}</button>;
    return <div className={`quick-tag ${shortcut.enabled ? "" : "disabled"}${kind === "command_request" ? " command-tag" : ""}`} key={shortcut.id}><button type="button" disabled={!shortcut.enabled || !conversation || sending || Boolean(shortcutBusy)} onClick={() => void runShortcut(shortcut)} title={shortcut.enabled ? shortcut.template : `${shortcut.name}已停用`}>{shortcutBusy === shortcut.id ? "发送中" : shortcut.name}</button><button type="button" className="quick-tag-edit" title={`编辑 ${shortcut.name}`} disabled={Boolean(shortcutBusy)} onClick={() => setShortcutEditor({ kind: shortcut.kind, shortcut })}>...</button></div>;
  };
  const handleTaskDispatched = (message: Message, runID: string) => {
    setMessages((items) => [...items, message]);
    setInputHistory((items) => [...items.slice(-99), message.content]);
    setHistoryRefresh((version) => version + 1);
    setRun(finishedRunIds.current.has(runID) ? "" : runID);
    void refreshUsage().catch((cause) => fail(cause instanceof Error ? cause.message : "Unable to refresh usage"));
    void refreshConversationHistory().catch((cause) => fail(cause instanceof Error ? cause.message : "Unable to refresh conversation history"));
  };

  return <div className="chat">
    <header className="project-head">
      <div className="project-heading"><button className="back-projects" title="返回项目列表" onClick={back}>←</button><div><label>{project.runner}</label><h2>{project.name}</h2><code>{project.pathDisplay}</code></div></div>
      <div className="head-actions">
        <div className="permission-menu">
          <button className={`permission-trigger ${conversation?.permissionMode === "full_control" ? "full" : ""}`} disabled={!!run || changingPermission} onClick={() => setShowPermissionMenu((open) => !open)}>{permissionLabel}</button>
          {showPermissionMenu && <div className="permission-popover" role="menu"><button className={conversation?.permissionMode === "approval_required" ? "selected" : ""} onClick={() => void changePermissionMode("approval_required")}><b>默认权限</b><span>命令执行前需要确认</span></button><button className={conversation?.permissionMode === "full_control" ? "selected full" : ""} onClick={() => void changePermissionMode("full_control")}><b>完全控制</b><span>命令直接执行</span></button></div>}
        </div>
        <div className="usage-summary" aria-live="polite">
          <span title={displayedModel}>{displayedModel}</span>
          <span className={`context-state ${contextLevel(usage?.context)}`}>{contextLabel(usage?.context)}</span>
          <span>{usage ? `${usage.session.taskCount} 次任务` : "使用状态加载中"}</span>
          <button className="usage-trigger" onClick={() => setShowUsage(true)}>使用状态</button>
        </div>
        <button className="secondary" disabled={!!run} onClick={openConversationHistory}>历史</button>
        <button className="secondary" disabled={!!run} onClick={() => setShowNewConversation(true)}>新会话</button>
        <button className="secondary" onClick={() => setShowTasks({})}>任务</button>
        {project.gitBranch !== "非 Git 目录" && <button className="secondary git-trigger" onClick={() => setShowGit(true)}>Git: {project.gitBranch || "HEAD"}</button>}
        <span className={run ? "running" : "ready"}>{run ? "正在执行" : "已就绪"}</span>
        {run && <button className="stop-run" disabled={stopping} onClick={() => void stopRun()}>{stopping ? "停止中" : "停止任务"}</button>}
      </div>
    </header>
    {showGit && <GitWorkbench projectID={project.id} request={api} fail={fail} close={() => setShowGit(false)} />}
    {showTasks ? <TaskBoard projectID={project.id} initialTaskID={showTasks.taskID} permissionMode={conversation?.permissionMode} request={api} fail={fail} close={() => setShowTasks(null)} onDispatched={handleTaskDispatched} /> : <><section className="conversation-canvas">
      <aside className="quick-tag-rail" aria-label="常用操作">
        <div className="quick-actions-row">
          <div className="quick-actions-headings"><div className="quick-tag-heading"><span>常用提示词</span><button type="button" title="新增常用提示词" onClick={() => setShortcutEditor({ kind: "prompt" })}>+</button></div><div className="quick-tag-heading command-tag-heading"><span>常用命令</span><button type="button" title="新增常用命令" onClick={() => setShortcutEditor({ kind: "command_request" })}>+</button></div></div>
          {Array.from({ length: shortcutRowCount }, (_, index) => <div className="quick-tag-row" key={`shortcut-row-${index}`}>{renderShortcutCell(promptShortcutItems[index], "prompt", !promptShortcutItems[index])}{renderShortcutCell(commandShortcutItems[index], "command_request", !commandShortcutItems[index])}</div>)}
        </div>
        <div className="quick-actions-mobile">
          <div className="quick-tag-group"><div className="quick-tag-heading"><span>常用提示词</span><button type="button" title="新增常用提示词" onClick={() => setShortcutEditor({ kind: "prompt" })}>+</button></div><div className="quick-tag-list">{promptShortcutItems.map((shortcut, index) => <div key={shortcut?.id || `empty-prompt-${index}`}>{renderShortcutCell(shortcut, "prompt")}</div>)}</div></div>
          <div className="quick-tag-group command-tags"><div className="quick-tag-heading"><span>常用命令</span><button type="button" title="新增常用命令" onClick={() => setShortcutEditor({ kind: "command_request" })}>+</button></div><div className="quick-tag-list">{commandShortcutItems.map((shortcut, index) => <div key={shortcut?.id || `empty-command-${index}`}>{renderShortcutCell(shortcut, "command_request")}</div>)}</div></div>
        </div>
        <TaskQueue projectID={project.id} permissionMode={conversation?.permissionMode} request={api} fail={fail} onDispatched={handleTaskDispatched} openBoard={(taskID) => setShowTasks(taskID ? { taskID } : {})} />
      </aside>
      <section className="timeline">
        <div ref={top} />
        {hasMoreHistory && <button className="secondary load-earlier-history" type="button" disabled={loadingOlderHistory} onClick={() => void loadOlderHistory()}>{loadingOlderHistory ? "加载中" : "加载更早记录"}</button>}
        {timeline.length === 0 && <div className="empty"><h2>开始与 Claude Code 对话</h2><p>选择常用操作，或直接描述希望 Claude 完成的工作。</p></div>}
        {timeline.map((item) => <div key={item.id}><div className={`timeline-entry ${item.kind}`}>{item.kind === "message" ? <MessageCard message={item.message} /> : <ToolCard action={item.action} resolving={resolving} decide={decide} />}</div>{item.kind === "message" && item.message.role === "user" && item.message.runId && executionByRun.get(item.message.runId) && <AgentExecutionCard execution={executionByRun.get(item.message.runId)!} open={() => setShowAgentExecution(item.message.runId!)} />}</div>)}
        {agentExecutions.filter((execution) => !anchoredExecutionRunIDs.has(execution.runId)).map((execution) => <AgentExecutionCard key={execution.runId} execution={execution} open={() => setShowAgentExecution(execution.runId)} />)}
        {run && <div className="run-indicator"><span></span>{runLabel}</div>}
        <div ref={bottom} />
      </section>
    </section>
    <div className="scroll-buttons">
      <button type="button" className="scroll-btn scroll-to-top" title="回到顶部" onClick={scrollToTop}>↑</button>
      <button type="button" className="scroll-btn scroll-to-bottom" title="回到底部" onClick={scrollToBottom}>↓</button>
    </div>
    <form className={`composer${pendingApproval ? " has-approval" : ""}`} onSubmit={(event) => void send(event)}>
      {pendingApproval && <ApprovalBanner action={pendingApproval} resolving={resolving} decide={decide} scrollToCard={() => { const el = document.querySelector(".timeline-entry.tool .tool-card.waiting"); if (el) el.scrollIntoView({ behavior: "smooth", block: "center" }); }} />}
      <textarea value={text} onChange={(event) => handleTextChange(event.target.value)} onKeyDown={(event) => { if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing) { event.preventDefault(); event.currentTarget.form?.requestSubmit(); return; } navigateInputHistory(event); }} placeholder="描述希望 Claude 在当前项目中完成的工作..." disabled={sending || Boolean(shortcutBusy)} />
      <div><small>{run ? runLabel : conversation?.permissionMode === "full_control" ? "Claude Code · 完全控制" : "Claude Code · 命令需确认"}</small><span className="composer-actions">{run && <button className="secondary" type="button" disabled={stopping} onClick={() => void stopRun()}>{stopping ? "停止中" : "停止"}</button>}<button className="primary" disabled={!text.trim() || sending || Boolean(shortcutBusy)}>{sending ? "发送中" : "发送"}</button></span></div>
    </form></>}
    {showNewConversation && <NewConversationDialog close={() => setShowNewConversation(false)} create={newConversation} />}
    {showHistory && <ConversationHistoryDialog conversations={conversationHistory} activeID={conversation?.id || ""} busyID={activatingConversation} close={() => setShowHistory(false)} activate={activateConversation} />}
    {showFullControlConfirmation && <FullControlConfirmationDialog close={() => setShowFullControlConfirmation(false)} confirm={confirmFullControl} changing={changingPermission} />}
    {showAgentExecution && agentExecutions.find((execution) => execution.runId === showAgentExecution) && <AgentExecutionDialog execution={agentExecutions.find((execution) => execution.runId === showAgentExecution)!} close={() => setShowAgentExecution(null)} />}
    {showUsage && <UsageDialog usage={usage} currentRun={currentUsage} now={usageNow} close={() => setShowUsage(false)} />}
    {shortcutEditor && <ShortcutEditor projectID={project.id} state={shortcutEditor} close={() => setShortcutEditor(null)} refresh={refreshShortcuts} fail={fail} />}
    {shortcutVariables && <ShortcutVariablesDialog state={shortcutVariables} close={() => setShortcutVariables(null)} run={(variables) => { setShortcutVariables(null); void runShortcut(shortcutVariables.shortcut, variables, true); }} />}
  </div>;
}

function ShortcutVariablesDialog({ state, close, run }: { state: { shortcut: Shortcut; variables: Record<string, string> }; close: () => void; run: (variables: Record<string, string>) => void }) {
  const required = requiredShortcutVariables(state.shortcut.template);
  const [variables, setVariables] = useState(state.variables);
  useEffect(() => { setVariables(state.variables); }, [state]);
  const submit = (event: FormEvent) => { event.preventDefault(); run(variables); };
  return <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="shortcut-variables-title"><section className="modal shortcut-variables-dialog"><header><div><label>COMMON PROMPT</label><h2 id="shortcut-variables-title">{state.shortcut.name}</h2></div><button title="关闭" onClick={close}>x</button></header><form onSubmit={submit}><div className="shortcut-editor-body">{required.includes("selection") && <label>选中内容<textarea autoFocus required value={variables.selection || ""} onChange={(event) => setVariables((old) => ({ ...old, selection: event.target.value }))} placeholder="粘贴需要处理的内容" /></label>}{required.includes("error") && <label>错误信息<textarea autoFocus={!required.includes("selection")} required value={variables.error || ""} onChange={(event) => setVariables((old) => ({ ...old, error: event.target.value }))} placeholder="粘贴报错信息" /></label>}</div><footer><button type="button" className="secondary" onClick={close}>取消</button><button className="primary">发送</button></footer></form></section></div>;
}

function ShortcutEditor({ projectID, state, close, refresh, fail }: { projectID: string; state: ShortcutEditorState; close: () => void; refresh: () => Promise<void>; fail: (message: string) => void }) {
  const shortcut = state.shortcut;
  const [name, setName] = useState(shortcut?.name || "");
  const [template, setTemplate] = useState(shortcut?.template || "");
  const [enabled, setEnabled] = useState(shortcut?.enabled ?? true);
  const [busy, setBusy] = useState(false);
  const isCommand = state.kind === "command_request";
  const isSnippet = state.kind === "snippet";

  useEffect(() => {
    setName(state.shortcut?.name || "");
    setTemplate(state.shortcut?.template || "");
    setEnabled(state.shortcut?.enabled ?? true);
  }, [state]);

  const save = async (event: FormEvent) => {
    event.preventDefault();
    setBusy(true);
    const scope = shortcut?.scope || "local";
    try {
      await api(shortcut ? `/api/shortcuts/${shortcut.id}` : "/api/shortcuts", {
        method: shortcut ? "PATCH" : "POST",
        body: JSON.stringify({
          name,
          description: shortcut?.description || "",
          kind: state.kind,
          template,
          scope,
          defaultAction: isCommand ? "confirm" : isSnippet ? "fill" : "run",
          groupName: isCommand ? "常用命令" : "常用提示词",
          pinned: true,
          enabled,
          sortOrder: shortcut?.sortOrder || 0,
          projectIds: scope === "project" ? (shortcut?.projectIds.length ? shortcut.projectIds : [projectID]) : [],
        }),
      });
      await refresh();
      close();
    } catch (cause) { fail(cause instanceof Error ? cause.message : "Unable to save shortcut"); }
    finally { setBusy(false); }
  };
  const remove = async () => {
    if (!shortcut || !window.confirm(`删除“${shortcut.name}”？`)) return;
    setBusy(true);
    try {
      await api(`/api/shortcuts/${shortcut.id}`, { method: "DELETE" });
      await refresh();
      close();
    } catch (cause) { fail(cause instanceof Error ? cause.message : "Unable to delete shortcut"); }
    finally { setBusy(false); }
  };

  return <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="shortcut-editor-title"><section className="modal shortcut-editor"><header><div><label>{isCommand ? "COMMON COMMAND" : "COMMON PROMPT"}</label><h2 id="shortcut-editor-title">{shortcut ? `编辑${isCommand ? "命令" : "提示词"}` : `新增${isCommand ? "命令" : "提示词"}`}</h2></div><button title="关闭" disabled={busy} onClick={close}>x</button></header><form onSubmit={(event) => void save(event)}><div className="shortcut-editor-body"><label>名称<input autoFocus required maxLength={64} value={name} onChange={(event) => setName(event.target.value)} placeholder={isCommand ? "例如：清空终端" : "例如：审查当前改动"} /></label><label>{isCommand ? "命令" : "提示词"}<textarea required maxLength={12000} value={template} onChange={(event) => setTemplate(event.target.value)} placeholder={isCommand ? "例如：clear" : "描述希望 Claude 在当前项目完成的工作"} /></label>{shortcut && <label className="shortcut-enabled"><input type="checkbox" checked={enabled} onChange={(event) => setEnabled(event.target.checked)} />启用此项</label>}{isCommand && <p>点击标签后会按当前会话权限请求执行该命令。</p>}</div><footer>{shortcut && <button type="button" className="danger-text" disabled={busy} onClick={() => void remove()}>删除</button>}<span></span><button type="button" className="secondary" disabled={busy} onClick={close}>取消</button><button className="primary" disabled={busy}>{busy ? "保存中" : "保存"}</button></footer></form></section></div>;
}

function UsageDialog({ usage, currentRun, now, close }: { usage: ConversationUsageResponse | null; currentRun: RunUsage | undefined; now: number; close: () => void }) {
  const task = usage?.currentRun ?? usage?.latestRun;
  const active = Boolean(usage?.currentRun && usage.currentRun.status === "running");
  const hasTaskUsage = Boolean(task?.hasResult);
  const metrics = task ? [
    ["状态", active ? "运行中" : task.status === "completed" ? "已完成" : task.status === "stopped" ? "已停止" : task.status === "failed" ? "失败" : task.status],
    ["耗时", formatDuration(runDuration(task, now))],
    ["首个响应", task.ttftMs > 0 ? formatDuration(task.ttftMs) : "等待响应"],
    ["Agent 轮次", task.agentTurns || "--"],
    ["模型步骤", task.modelSteps || "--"],
    ["工具调用", task.toolCalls || "--"],
    ["子代理", task.subagentCount || "0"],
    ["输入 / 输出", hasTaskUsage ? `${formatTokens(task.inputTokens)} / ${formatTokens(task.outputTokens)}` : "等待最终统计"],
    ["缓存读取 / 创建", hasTaskUsage ? `${formatTokens(task.cacheReadTokens)} / ${formatTokens(task.cacheCreationTokens)}` : "等待最终统计"],
    ["费用估算", hasTaskUsage ? formatCost(task.estimatedCostUsd) : "等待最终统计"],
  ] : [];
  const session = usage?.session;
  return <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="usage-title"><section className="modal usage-dialog"><header><div><label>CLAUDE CODE USAGE</label><h2 id="usage-title">使用状态</h2></div><button title="关闭" onClick={close}>x</button></header><div className="usage-body">
    <section className="usage-section"><div className="usage-section-head"><h3>当前任务</h3>{task?.model && <span>{task.model}</span>}</div>{task ? <><dl className="usage-grid">{metrics.map(([label, value]) => <div key={String(label)}><dt>{label}</dt><dd>{value}</dd></div>)}</dl>{!hasTaskUsage && !active && <p className="usage-note">该任务未获得 Claude 的最终统计数据。</p>}</> : <p className="usage-note">当前会话还没有可用的任务统计。</p>}</section>
    <section className="usage-section"><div className="usage-section-head"><h3>当前会话</h3><span className={`context-state ${contextLevel(usage?.context)}`}>{contextLabel(usage?.context)}</span></div>{usage?.context.contextInputTokens && usage.context.contextWindow > 0 && <p className="context-detail">{formatTokens(usage.context.contextInputTokens)} / {formatTokens(usage.context.contextWindow)}</p>}{session && <dl className="usage-grid session-grid"><div><dt>任务次数</dt><dd>{session.taskCount}</dd></div><div><dt>Agent 轮次</dt><dd>{session.agentTurns}</dd></div><div><dt>模型步骤</dt><dd>{session.modelSteps}</dd></div><div><dt>工具调用</dt><dd>{session.toolCalls}</dd></div><div><dt>输入 / 输出</dt><dd>{formatTokens(session.inputTokens)} / {formatTokens(session.outputTokens)}</dd></div><div><dt>缓存读取 / 创建</dt><dd>{formatTokens(session.cacheReadTokens)} / {formatTokens(session.cacheCreationTokens)}</dd></div><div><dt>费用估算</dt><dd>{formatCost(session.estimatedCostUsd)}</dd></div></dl>}</section>
    {usage?.models.length ? <section className="usage-section"><div className="usage-section-head"><h3>模型用量</h3><span>包含子代理</span></div><div className="model-usage-list">{usage.models.map((model) => <div key={model.model}><b>{model.model}</b><span>{formatTokens(model.inputTokens)} 输入 · {formatTokens(model.outputTokens)} 输出</span><em>{formatCost(model.estimatedCostUsd)}</em></div>)}</div></section> : null}
    <p className="usage-disclaimer">费用为 Claude Code 客户端估算值，不代表账单金额。</p>
  </div><footer><button className="secondary" onClick={close}>关闭</button></footer></section></div>;
}

function ConversationHistoryDialog({ conversations, activeID, busyID, close, activate }: { conversations: Conversation[]; activeID: string; busyID: string; close: () => void; activate: (item: Conversation) => Promise<void> }) {
  const [query, setQuery] = useState("");
  const [selected, setSelected] = useState(0);
  const filtered = useMemo(() => {
    const keyword = query.trim().toLocaleLowerCase();
    return keyword ? conversations.filter((item) => `${item.title} ${item.preview || ""}`.toLocaleLowerCase().includes(keyword)) : conversations;
  }, [conversations, query]);
  useEffect(() => { setSelected(0); }, [query]);
  const select = (item: Conversation) => { if (item.id !== activeID && item.status !== "running" && !busyID) void activate(item); };
  const keyDown = (event: React.KeyboardEvent<HTMLInputElement>) => {
    if (event.key === "Escape") { event.preventDefault(); close(); return; }
    if (event.key === "ArrowDown") { event.preventDefault(); setSelected((index) => Math.min(filtered.length - 1, index + 1)); return; }
    if (event.key === "ArrowUp") { event.preventDefault(); setSelected((index) => Math.max(0, index - 1)); return; }
    if (event.key === "Enter" && filtered[selected]) { event.preventDefault(); select(filtered[selected]); }
  };
  return <div className="backdrop history-backdrop" role="dialog" aria-modal="true" aria-label="会话历史"><section className="modal conversation-history"><header><div><label>CLAUDE CODE SESSION</label><h2>恢复会话</h2></div><button title="关闭" onClick={close}>x</button></header><input autoFocus value={query} onChange={(event) => setQuery(event.target.value)} onKeyDown={keyDown} placeholder="搜索历史会话" /><div className="history-list">{filtered.length === 0 ? <p className="history-empty">没有匹配的会话</p> : filtered.map((item, index) => {
    const runningElsewhere = item.status === "running" && item.id !== activeID;
    return <button key={item.id} className={`history-item ${item.id === activeID ? "active" : ""} ${index === selected ? "selected" : ""}`} disabled={Boolean(busyID) || runningElsewhere} onMouseEnter={() => setSelected(index)} onClick={() => select(item)}><span><b>{item.title || "新会话"}</b><small>{item.preview || "尚未发送消息"}</small></span><em>{item.id === activeID ? "当前" : runningElsewhere ? "运行中" : formatHistoryTime(item.lastActivityAt)}</em></button>;
  })}</div><footer><small>输入 /resume 可再次打开</small><button className="secondary" onClick={close}>关闭</button></footer></section></div>;
}

function NewConversationDialog({ close, create }: { close: () => void; create: (permissionMode: PermissionMode) => Promise<void> }) {
  const [permissionMode, setPermissionMode] = useState<PermissionMode>("approval_required");
  const [creating, setCreating] = useState(false);
  const submit = async () => { setCreating(true); try { await create(permissionMode); } finally { setCreating(false); } };
  return <div className="backdrop" role="dialog" aria-modal="true"><section className="modal permission-dialog"><header><div><label>CLAUDE CODE SESSION</label><h2>新会话</h2></div><button title="关闭" onClick={close}>x</button></header><div className="permission-options"><button className={permissionMode === "approval_required" ? "active" : ""} onClick={() => setPermissionMode("approval_required")}><b>默认权限</b><span>每条终端命令执行前等待确认。</span></button><button className={permissionMode === "full_control" ? "active danger" : ""} onClick={() => setPermissionMode("full_control")}><b>完全控制</b><span>Claude 可直接执行命令，不会等待确认。</span></button></div><footer><button className="secondary" onClick={close}>取消</button><button className="primary" disabled={creating} onClick={() => void submit()}>{creating ? "创建中" : "创建会话"}</button></footer></section></div>;
}

function FullControlConfirmationDialog({ close, confirm, changing }: { close: () => void; confirm: () => Promise<void>; changing: boolean }) {
  return <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="full-control-title"><section className="modal permission-dialog"><header><div><label>CLAUDE CODE PERMISSION</label><h2 id="full-control-title">切换为完全控制</h2></div><button title="关闭" disabled={changing} onClick={close}>x</button></header><p className="permission-confirmation">完全控制会允许 Claude 在当前项目中直接执行所有命令，不再等待确认。</p><footer><button className="secondary" disabled={changing} onClick={close}>取消</button><button className="primary danger" disabled={changing} onClick={() => void confirm()}>{changing ? "切换中" : "确认切换"}</button></footer></section></div>;
}

function MessageCard({ message }: { message: Message }) {
  const isUser = message.role === "user";
  return <article className={`message ${message.role}`}><header><span className="message-avatar">{isUser ? "你" : "C"}</span><b>{isUser ? "你" : "Claude"}</b><time>{formatTime(message.createdAt)}</time></header><div className="markdown"><Markdown content={message.content} /></div></article>;
}

function MarkdownImage({ src, alt }: { src?: string; alt?: string }) {
  const label = alt?.trim() || "未命名图片";
  const externalImage = src && /^https:\/\//i.test(src) ? src : "";
  return <span className="markdown-image-reference" role="note">图片：{externalImage ? <a href={externalImage} target="_blank" rel="noreferrer">{label}</a> : label}</span>;
}

function Markdown({ content }: { content: string }) {
  return <ReactMarkdown remarkPlugins={[remarkGfm]} skipHtml components={{ a: ({ href, children }) => <a href={href} target="_blank" rel="noreferrer">{children}</a>, img: MarkdownImage }}>{content}</ReactMarkdown>;
}

function ToolCard({ action, resolving, decide }: { action: ToolAction; resolving: string; decide: (approvalId: string, decision: "allow" | "deny") => Promise<void> }) {
  const command = typeof action.input.command === "string" ? action.input.command : "";
  const description = typeof action.input.description === "string" ? action.input.description : action.name;
  const approval = action.approval;
  const waiting = approval?.status === "pending" && !action.output;
  const denied = approval?.status === "deny";
  const failed = !denied && (action.output?.isError || action.runStatus === "failed");
  const stopped = action.runStatus === "stopped";
  const status = denied ? "已拒绝" : failed ? "执行失败" : stopped ? "已停止" : action.output ? "已完成" : waiting ? "等待确认" : approval?.status === "allow" ? "已允许" : action.runStatus === "completed" ? "已结束" : "执行中";
  const output = action.output?.content || "(无输出)";
  const shouldCollapseOutput = output.length > 260 || output.split("\n").length > 5;
  const outputPreview = output.replace(/\s+/g, " ").trim().slice(0, 180);
  const statusClass = failed ? "failed" : stopped || denied ? "denied" : waiting ? "pending" : "";
  return <article className={`tool-card ${waiting ? "waiting" : ""}`}><header><div><span className="tool-icon">$</span><div><b>{action.name === "Bash" ? "终端命令" : action.name}</b><small>{description}</small></div></div><div className="tool-meta"><time>{formatTime(action.createdAt)}</time><span className={`tool-status ${statusClass}`}>{status}</span></div></header>{command && <pre className="command"><code>{command}</code></pre>}{waiting && approval && <div className="approval-actions"><span>此命令将会在当前项目目录执行。</span><div><button className="secondary" disabled={resolving === approval.approvalId} onClick={() => void decide(approval.approvalId, "deny")}>拒绝</button><button className="primary" disabled={resolving === approval.approvalId} onClick={() => void decide(approval.approvalId, "allow")}>{resolving === approval.approvalId ? "处理中" : "允许执行"}</button></div></div>}{action.output && (shouldCollapseOutput ? <details><summary>{failed ? "查看错误输出" : "查看命令输出"}<span>{outputPreview}</span></summary><pre className="output">{output}</pre></details> : <pre className={`output inline ${failed ? "error-output" : ""}`}>{output}</pre>)}</article>;
}

function flattenAgents(agents: AgentNode[]): AgentNode[] {
  return agents.flatMap((agent) => [agent, ...flattenAgents(agent.children)]);
}

function AgentExecutionCard({ execution, open }: { execution: AgentExecution; open: () => void }) {
  const agents = flattenAgents(execution.agents);
  const counts = agents.reduce<Record<AgentStatus, number>>((value, agent) => { value[agent.status]++; return value; }, { pending: 0, running: 0, completed: 0, failed: 0, stopped: 0, unresolved: 0 });
  const state = execution.incomplete ? "结果未收齐" : execution.status === "completed" ? "已完成" : execution.status === "failed" ? "执行失败" : ["stopped", "interrupted", "cancelled"].includes(execution.status) ? "已停止" : "执行中";
  const lastActivity = agents.flatMap((agent) => agent.logs).reduce<AgentLog | undefined>((latest, log) => !latest || new Date(log.createdAt).getTime() > new Date(latest.createdAt).getTime() ? log : latest, undefined);
  return <section className={`agent-execution-card ${execution.incomplete ? "incomplete" : execution.status}`} aria-label="子代理执行过程"><header><div><span className="agent-execution-icon">A</span><div><b>子代理执行过程</b><small>{state}{counts.running ? ` · ${counts.running} 个执行中` : ""}</small></div></div><button className="secondary" onClick={open}>查看过程</button></header><div className="agent-execution-summary"><span>{agents.length} 个子代理</span><span>{counts.completed} 已完成</span>{counts.stopped > 0 && <span>{counts.stopped} 已停止</span>}{counts.failed > 0 && <span className="failed">{counts.failed} 失败</span>}{counts.unresolved > 0 && <span className="incomplete">{counts.unresolved} 未收齐</span>}</div>{execution.incomplete && <p className="agent-execution-warning">主回合已结束，但没有收到全部子代理的最终结果。已收到的过程记录仍可查看。</p>}{lastActivity && <p className="agent-execution-last"><b>{lastActivity.title}</b><span>{lastActivity.detail.replace(/\s+/g, " ")}</span></p>}</section>;
}

function AgentExecutionDialog({ execution, close }: { execution: AgentExecution; close: () => void }) {
  const agents = flattenAgents(execution.agents);
  const [selectedID, setSelectedID] = useState(agents[0]?.id || "");
  const selected = agents.find((agent) => agent.id === selectedID) || agents[0];
  return <div className="backdrop agent-execution-backdrop" role="dialog" aria-modal="true" aria-labelledby="agent-execution-title"><section className="agent-execution-dialog"><header><div><label>CLAUDE CODE EXECUTION</label><h2 id="agent-execution-title">子代理过程</h2><p>{execution.incomplete ? "主回合已完成，但部分子代理的最终结果未送达。" : "查看每个子代理已收到的输出、工具调用和结果。"}</p></div><button title="关闭" onClick={close}>x</button></header><div className="agent-execution-body"><nav className="agent-tree" aria-label="子代理列表">{execution.agents.map((agent) => <AgentTree key={agent.id} agent={agent} selectedID={selectedID} select={setSelectedID} depth={0} />)}</nav><section className="agent-log-panel">{selected ? <><header><div><span className={`agent-status ${selected.status}`}>{agentStatusLabel(selected.status)}</span><h3>{selected.summary}</h3></div><small>{formatTime(selected.createdAt)}</small></header><div className="agent-log-list">{selected.logs.length === 0 ? <p className="agent-log-empty">尚未收到该子代理的过程输出。</p> : selected.logs.map((log) => <article className={`agent-log ${log.kind} ${log.isError ? "failed" : ""}`} key={log.id}><header><b>{log.title}</b><time>{formatTime(log.createdAt)}</time></header><details open={log.kind === "text"}><summary>{log.detail.replace(/\s+/g, " ").slice(0, 180) || "无输出"}</summary><pre>{log.detail || "(无输出)"}</pre></details></article>)}</div></> : <p className="agent-log-empty">没有可查看的子代理。</p>}</section></div></section></div>;
}

function AgentTree({ agent, selectedID, select, depth }: { agent: AgentNode; selectedID: string; select: (id: string) => void; depth: number }) {
  return <div className="agent-tree-branch"><button className={`agent-tree-row ${agent.id === selectedID ? "selected" : ""}`} style={{ paddingLeft: `${12 + depth * 16}px` }} onClick={() => select(agent.id)}><span className={`agent-status ${agent.status}`}></span><span><b>{agent.summary}</b><small>{agentStatusLabel(agent.status)} · {agent.logs.length} 条记录</small></span></button>{agent.children.map((child) => <AgentTree key={child.id} agent={child} selectedID={selectedID} select={select} depth={depth + 1} />)}</div>;
}

function ApprovalBanner({ action, resolving, decide, scrollToCard }: { action: ToolAction; resolving: string; decide: (approvalId: string, decision: "allow" | "deny") => Promise<void>; scrollToCard: () => void }) {
  const command = typeof action.input.command === "string" ? action.input.command : "";
  const description = typeof action.input.description === "string" ? action.input.description : "";
  const approval = action.approval;
  if (!approval) return null;
  return <div className="approval-banner">
    <div className="approval-banner-body">
      <span className="approval-banner-icon">⏳</span>
      <div className="approval-banner-info">
        <b>等待确认命令执行</b>
        <span>{description || command}</span>
      </div>
      <code className="approval-banner-command">{command}</code>
      <div className="approval-banner-actions">
        <button className="secondary" disabled={resolving === approval.approvalId} onClick={() => void decide(approval.approvalId, "deny")}>拒绝</button>
        <button className="primary" disabled={resolving === approval.approvalId} onClick={() => void decide(approval.approvalId, "allow")}>{resolving === approval.approvalId ? "处理中" : "允许执行"}</button>
        <button className="approval-banner-scroll" onClick={scrollToCard} title="滚动到命令卡片">↓</button>
      </div>
    </div>
  </div>;
}
