import { FormEvent, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { createPortal } from "react-dom";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import "./markdown.css";
import "./permission.css";
import "./conversation.css";
import "./stop.css";
import "./tasks.css";
import "./git.css";
import "./run.css";
import { GitWorkbench } from "./features/git/GitWorkbench";
import { ProjectRunPanel } from "./features/run/ProjectRunPanel";
import { TaskBoard } from "./features/tasks/TaskBoard";
import { TaskQueue } from "./features/tasks/TaskQueue";
import { ConfirmDialog } from "./components/ConfirmDialog";

type Project = { id: string; name: string; pathDisplay: string; runner: string; environment: string; gitBranch: string; claudeReady: boolean; codexReady: boolean; agentReady: boolean };
type ProjectStatus = { running: boolean; conversationCount: number; activeTitle: string };
type ProjectFilter = "all" | "running" | "ready" | "offline";
type PermissionMode = "approval_required" | "full_control" | "read_only" | "workspace_write";
type AgentID = "claude-code" | "codex";
type Conversation = { id: string; status: string; agentId: AgentID; agentSessionId: string; agentRuntimeId: string; executionPolicy: PermissionMode; permissionMode: PermissionMode; title: string; preview?: string; lastActivityAt: string; isCurrent: boolean };
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
type RunUsage = { runId: string; conversationId: string; available: boolean; reason?: string; status: string; model: string; contextWindow: number; contextInputTokens: number; inputTokens: number; outputTokens: number; cacheReadTokens: number; cacheCreationTokens: number; estimatedCostUsd: number; agentTurns: number; modelSteps: number; toolCalls: number; subagentCount: number; durationMs: number; ttftMs: number; terminalReason: string; hasResult: boolean; startedAt?: string; completedAt?: string; models: ModelUsage[] };
type ConversationUsage = { taskCount: number; agentTurns: number; modelSteps: number; toolCalls: number; subagentCount: number; inputTokens: number; outputTokens: number; cacheReadTokens: number; cacheCreationTokens: number; estimatedCostUsd: number };
type ConversationUsageResponse = { conversationId: string; available: boolean; reason?: string; context: RunUsage; currentRun?: RunUsage; latestRun?: RunUsage; session: ConversationUsage; models: ModelUsage[] };
type TimelineItem =
  | { kind: "message"; id: string; createdAt: string; message: Message }
  | { kind: "tool"; id: string; createdAt: string; action: ToolAction }
  | { kind: "error"; id: string; createdAt: string; runId: string; title: string; detail: string };

function retryCountFor(init?: RequestInit): number {
  const method = (init?.method ?? "GET").toUpperCase();
  return method === "GET" || method === "HEAD" || method === "OPTIONS" ? 2 : 0;
}

async function api<T>(path: string, init?: RequestInit, retries = retryCountFor(init)): Promise<T> {
  let lastError: unknown;
  const signal = init?.signal;
  for (let attempt = 0; attempt <= retries; attempt++) {
    try {
      // Don't retry if the request was aborted by the caller.
      if (signal?.aborted) throw new DOMException("Aborted", "AbortError");
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
      if (cause instanceof DOMException && cause.name === "AbortError") throw cause;
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
  if (context?.available === false) return "上下文暂不可用";
  const tokens = context?.contextInputTokens;
  if (!tokens) return context?.hasResult ? "上下文暂不可用" : "上下文计算中";
  if (!context?.contextWindow) return "上下文暂不可用";
  const percent = Math.round(tokens / context.contextWindow * 100);
  const tokensDisplay = formatTokens(tokens);
  const windowDisplay = formatTokens(context.contextWindow);
  return `上下文 ${tokensDisplay} / ${windowDisplay} (${percent}%)`;
}

function contextLevel(context: RunUsage | undefined): string {
  const tokens = context?.contextInputTokens;
  if (!tokens || !context?.contextWindow) return "pending";
  const percent = tokens / context.contextWindow * 100;
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
  const runErrors = new Map<string, string>();
  const approvalStates = new Map<string, ApprovalEvent>();
  for (const event of events) {
    const payload = asRecord(event.payload);
    if (event.type === "user") {
      for (const part of asRecord(payload.message).content || []) {
        if (part?.type === "tool_result" && part.tool_use_id) results.set(String(part.tool_use_id), { content: contentToText(part.content), isError: Boolean(part.is_error) });
      }
    }
    if (event.type.startsWith("run.")) runStatuses.set(event.runId, event.type.slice(4));
    if ((event.type === "run.failed" || event.type === "run.interrupted") && typeof payload.error === "string" && payload.error) runErrors.set(event.runId, payload.error);
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
  const codexTools = new Map<string, ToolAction>();
  for (const event of events) {
    if (event.type !== "item.started" && event.type !== "item.completed") continue;
    const item = asRecord(asRecord(event.payload).item);
    const itemType = String(item.type || "");
    if (itemType !== "command_execution" && itemType !== "file_change") continue;
    const id = String(item.id || event.id);
    const key = `codex:${id}`;
    const changes = Array.isArray(item.changes) ? item.changes.map(asRecord) : [];
    const changeSummary = changes.map((change) => `${change.kind || "修改"}: ${change.path || "文件"}`).join("\n");
    const input = itemType === "command_execution"
      ? { command: String(item.command || ""), description: "执行命令" }
      : { description: changeSummary || "修改项目文件", changes };
    const existing = codexTools.get(key);
    const outputText = itemType === "command_execution" ? String(item.aggregated_output || item.output || "") : changeSummary;
    const failed = Number(item.exit_code ?? item.exitCode ?? 0) !== 0 || item.status === "failed";
    codexTools.set(key, {
      id: key,
      runId: event.runId,
      name: itemType === "command_execution" ? "命令" : "文件变更",
      input,
      createdAt: existing?.createdAt || event.createdAt,
      output: event.type === "item.completed" ? { content: outputText || (failed ? "命令执行失败" : "已完成"), isError: failed } : existing?.output,
      runStatus: runStatuses.get(event.runId),
    });
  }
  tools.push(...codexTools.values());
  const errorItems: TimelineItem[] = [];
  const seenStderr = new Set<string>();
  for (const event of events) {
    const payload = asRecord(event.payload);
    if ((event.type === "run.failed" || event.type === "run.interrupted") && typeof payload.error === "string" && payload.error) {
      errorItems.push({ kind: "error", id: event.id, createdAt: event.createdAt, runId: event.runId, title: event.type === "run.interrupted" ? "执行中断" : "执行失败", detail: payload.error });
    }
    if (event.type === "stream.error" && typeof payload.error === "string" && payload.error) {
      // Skip stream.error events that are already represented by a run.failed
      const alreadyFailed = runErrors.has(event.runId);
      if (!alreadyFailed) {
        errorItems.push({ kind: "error", id: event.id, createdAt: event.createdAt, runId: event.runId, title: "流错误", detail: payload.error });
      }
    }
    if (event.type === "stderr" && typeof payload.message === "string" && payload.message.trim()) {
      const dedupKey = `${event.runId}:${payload.message.trim()}`;
      if (!seenStderr.has(dedupKey)) {
        seenStderr.add(dedupKey);
        errorItems.push({ kind: "error", id: event.id, createdAt: event.createdAt, runId: event.runId, title: "CLI 输出", detail: payload.message.trim() });
      }
    }
  }
  const timeline: TimelineItem[] = [
    ...messages.map((message) => ({ kind: "message" as const, id: message.id, createdAt: message.createdAt, message })),
    ...tools.map((action) => ({ kind: "tool" as const, id: action.id, createdAt: action.createdAt, action })),
    ...errorItems,
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
    if (event.type === "stream.error") appendLog(parentID, { id: event.id, createdAt: event.createdAt, kind: "error", title: "流错误", detail: String(payload.error || "无法读取工具输出"), isError: true });
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

// isTemporaryMessage checks whether a message was created from a WebSocket
// "assistant" streaming event (versus a canonical database record from an
// "assistant.message" event or HTTP reload).
function isTemporaryMessage(item: Message): boolean {
  return item.id.includes(":assistant-text:");
}

// mergeReloadMessages safely merges HTTP-reloaded messages with the live
// message state.  Temporary WS-sourced messages that already have a
// corresponding canonical entry in data.messages are replaced; any
// temporary message whose content has no canonical match yet is kept
// (waiting for the next assistant.message event) to avoid content
// disappearing during reload races.
function mergeReloadMessages(dbMessages: Message[], currentMessages: Message[]): Message[] {
  // Index canonical messages by (runId, parentToolUseId || "", content)
  // so we can match temporary WS messages to their canonical equivalents.
  const canonicalIndex = new Map<string, Message>();
  for (const msg of dbMessages) {
    if (msg.role !== "assistant") continue;
    const key = msg.runId + "::" + (msg.parentToolUseId || "") + "::" + msg.content;
    canonicalIndex.set(key, msg);
  }

  const filtered = currentMessages.filter((item) => {
    if (!isTemporaryMessage(item)) return true;
    // If a canonical message with the same runId+parentToolUseId+content
    // exists in the HTTP response, drop the temporary so the canonical one
    // replaces it.  Otherwise keep the temporary — it arrived (or will
    // arrive) as an assistant.message and hasn't been matched yet.
    const key = (item.runId || "") + "::" + (item.parentToolUseId || "") + "::" + item.content;
    return !canonicalIndex.has(key);
  });

  return mergeConversationItems([...filtered, ...dbMessages], []);
}

function timelineContentVersion(timeline: TimelineItem[], executions: AgentExecution[]): string {
  const timelineVersion = timeline.map((item) => {
    if (item.kind === "message") return `message:${item.id}:${item.message.content.length}`;
    if (item.kind === "tool") {
      return `tool:${item.id}:${item.action.output?.content.length || 0}:${item.action.approval?.status || ""}:${item.action.runStatus || ""}`;
    }
    return `error:${item.id}:${item.detail.length}`;
  });
  const nodeVersion = (node: AgentNode): string => [
    node.id,
    node.status,
    ...node.logs.map((log) => `${log.id}:${log.detail.length}`),
    ...node.children.map(nodeVersion),
  ].join(":");
  const executionVersion = executions.map((execution) => `${execution.runId}:${execution.status}:${execution.agents.map(nodeVersion).join(",")}`);
  return [...timelineVersion, ...executionVersion].join("|");
}

function isNarrowConversationLayout(): boolean {
  return window.matchMedia("(max-width: 820px)").matches;
}

function requiredShortcutVariables(template: string): ("selection" | "error")[] {
  return (["selection", "error"] as const).filter((key) => template.includes(`\${${key}}`));
}

export function App() {
  const [projects, setProjects] = useState<Project[]>([]);
  const [projectStatuses, setProjectStatuses] = useState<Record<string, ProjectStatus>>({});
  const [project, setProject] = useState<Project | null>(null);
  const [showImport, setShowImport] = useState(false);
  const [showSSHManager, setShowSSHManager] = useState(false);
  const [error, setError] = useState("");
  const loadProjects = useCallback(async () => {
    try {
      const list = await api<Project[]>("/api/projects");
      setProjects(list);
      try {
        const statusList = await api<{ id: string; running: number; conversationCount: number; activeTitle: string }[]>("/api/projects/statuses");
        const entries = statusList.map((item) => [item.id, { running: item.running === 1, conversationCount: item.conversationCount, activeTitle: item.activeTitle }] as const);
        setProjectStatuses(Object.fromEntries(entries));
      } catch {
        // On subsequent polls, keep the last-known status rather than
        // blanking every card to idle on a transient error.
        setProjectStatuses((prev) => {
          if (Object.keys(prev).length > 0) return prev;
          const entries = list.map((item) => [item.id, { running: false, conversationCount: 0, activeTitle: "" }] as const);
          return Object.fromEntries(entries);
        });
      }
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Unable to load projects");
    }
  }, []);

  // Poll project status while the dashboard is visible so cards reflect the
  // real running/idle state without a manual reload.  This also handles the
  // initial load (project starts as null).
  useEffect(() => {
    if (project) return; // Chat is open — WebSocket keeps the conversation view up to date.
    void loadProjects().catch(() => undefined);
    const interval = window.setInterval(() => { void loadProjects().catch(() => undefined); }, 10_000);
    return () => window.clearInterval(interval);
  }, [loadProjects, project]);

  return <main className={project ? "app-shell project-open" : "app-shell"}>
    {error && <div className="error"><span>{error}</span><button title="关闭错误提示" onClick={() => setError("")}>x</button></div>}
    {project ? <Chat key={project.id} project={project} fail={setError} back={() => setProject(null)} /> : <ProjectDashboard projects={projects} statuses={projectStatuses} importProject={() => setShowImport(true)} openProject={setProject} onManageSSH={() => setShowSSHManager(true)} />}
    {showImport && <Importer close={() => setShowImport(false)} created={(item) => { setShowImport(false); setProject(item); void loadProjects(); }} fail={setError} />}
    {showSSHManager && <SSHConnectionManager close={() => setShowSSHManager(false)} fail={setError} />}
  </main>;
}

type ToolStatus = { status: "ready" | "unavailable" | "needs_auth" | "updating"; version: string; reason?: string };
type RunnerInfo = {
  id: string;
  name: string;
  environment: string;
  root: string;
  claude: ToolStatus;
  codex?: ToolStatus;
};

type CheckUpdateResult = {
  updateAvailable: boolean;
  currentVersion: string;
  latestVersion?: string;
  error?: string;
};

type UpdateResult = {
  success: boolean;
  previousVersion?: string;
  currentVersion?: string;
  error?: string;
};

function ComposerRunnerInfo({ runnerID, agentID, run, runLabel, permissionMode, usage, displayedModel, contextLabel, contextLevel, onShowUsage, stopping, onStop }: { runnerID: string; agentID: AgentID; run: string; runLabel: string; permissionMode?: string; usage: ConversationUsageResponse | null; displayedModel: string; contextLabel: string; contextLevel: string; onShowUsage: () => void; stopping: boolean; onStop: () => void }) {
  const [runner, setRunner] = useState<RunnerInfo | null>(null);
  const [checking, setChecking] = useState(false);
  const [updateInfo, setUpdateInfo] = useState<CheckUpdateResult | null>(null);
  const [updating, setUpdating] = useState(false);
  const [showConfirm, setShowConfirm] = useState(false);

  useEffect(() => {
    api<RunnerInfo[]>("/api/runners").then((list) => {
      const match = list.find((r) => r.id === runnerID);
      if (match) setRunner(match);
    }).catch(() => {});
  }, [runnerID]);

  const handleCheckUpdate = async () => {
    setChecking(true);
    setUpdateInfo(null);
    try {
      const result = await api<CheckUpdateResult>(`/api/runners/${runnerID}/claude/check-update`, { method: "POST" });
      setUpdateInfo(result);
    } catch (err) {
      setUpdateInfo({ updateAvailable: false, currentVersion: runner?.claude?.version || "", error: err instanceof Error ? err.message : "检查更新失败" });
    } finally {
      setChecking(false);
    }
  };

  const handleUpdate = async () => {
    setShowConfirm(false);
    setUpdating(true);
    try {
      const result = await api<UpdateResult>(`/api/runners/${runnerID}/claude/update`, { method: "POST" });
      if (result.success) {
        setRunner((prev) => prev ? { ...prev, claude: { ...prev.claude, version: result.currentVersion || prev.claude.version, status: "ready" } } : prev);
        setUpdateInfo(null);
      } else {
        setUpdateInfo({ updateAvailable: false, currentVersion: runner?.claude?.version || "", error: result.error || "更新失败" });
      }
    } catch (err) {
      setUpdateInfo({ updateAvailable: false, currentVersion: runner?.claude?.version || "", error: err instanceof Error ? err.message : "更新失败" });
    } finally {
      setUpdating(false);
    }
  };

  const tool = agentID === "codex" ? runner?.codex : runner?.claude;
  const toolName = agentID === "codex" ? "Codex" : "Claude Code";
  const runnerStatusClass = run ? "running" : tool?.status || "unavailable";

  const canShowUsage = Boolean(run) || tool?.status === "ready";
  return <><span className="composer-status-group"><span className={`runner-inline ${runnerStatusClass}`} title={tool?.reason}><i></i>{run ? runLabel : tool?.status === "ready" ? `${toolName} ${tool.version}` : tool?.status === "updating" ? "更新中..." : tool?.reason || `${toolName} 不可用`}</span>{agentID === "claude-code" && !run && tool?.status === "ready" && <button className="runner-inline-btn" disabled={checking || updating} onClick={() => void handleCheckUpdate()}>{checking ? "检查中..." : "检查更新"}</button>}{agentID === "claude-code" && !run && updateInfo?.updateAvailable && !updateInfo.error && <button className="runner-inline-btn update-available" onClick={() => setShowConfirm(true)}>更新至 {updateInfo.latestVersion}</button>}{agentID === "claude-code" && !run && updateInfo?.error && <span className="runner-inline-error" title={updateInfo.error}>{updateInfo.error}</span>}{canShowUsage ? <span className="composer-usage"><span title={displayedModel}>{displayedModel}</span><span className={contextLevel}>{contextLabel}</span><span>{usage ? `${usage.session.taskCount} 次对话` : "加载中"}</span><button className="usage-trigger" type="button" onClick={onShowUsage}>使用状态</button></span> : <span className="composer-usage pending">用量将在工具就绪后显示</span>}</span>{showConfirm && <div className="backdrop" role="dialog" aria-modal="true"><section className="modal"><header><div><label>UPDATE CLAUDE CODE</label><h2>确认更新 Claude Code</h2></div><button title="关闭" onClick={() => setShowConfirm(false)}>x</button></header><p className="permission-confirmation">当前版本：<b>{tool?.version}</b> → 最新版本：<b>{updateInfo?.latestVersion}</b>。更新期间将无法使用 AI 对话功能，更新预计需要数十秒。</p><footer><button className="secondary" onClick={() => setShowConfirm(false)}>取消</button><button className="primary" onClick={() => void handleUpdate()}>确认更新</button></footer></section></div>}</>;
}

function ProjectDashboard({ projects, statuses, importProject, openProject, onManageSSH }: { projects: Project[]; statuses: Record<string, ProjectStatus>; importProject: () => void; openProject: (project: Project) => void; onManageSSH: () => void }) {
  const [filter, setFilter] = useState<ProjectFilter>("all");
  const [deleteTarget, setDeleteTarget] = useState<Project | null>(null);
  const [deleting, setDeleting] = useState(false);
  const runningCount = Object.values(statuses).filter((status) => status.running).length;
  const readyCount = projects.filter((project) => project.agentReady && !statuses[project.id]?.running).length;
  const offlineCount = projects.filter((project) => !project.agentReady).length;
  const projectState = (project: Project): ProjectFilter => statuses[project.id]?.running ? "running" : project.agentReady ? "ready" : "offline";
  const filteredProjects = projects.filter((project) => filter === "all" || projectState(project) === filter).sort((left, right) => {
    const order: Record<ProjectFilter, number> = { running: 0, ready: 1, offline: 2, all: 3 };
    return order[projectState(left)] - order[projectState(right)] || left.name.localeCompare(right.name, "zh-CN");
  });
  const filters: { id: ProjectFilter; label: string; count: number }[] = [{ id: "all", label: "全部", count: projects.length }, { id: "running", label: "进行中", count: runningCount }, { id: "ready", label: "已就绪", count: readyCount }, { id: "offline", label: "不可用", count: offlineCount }];

  const confirmDelete = async () => {
    if (!deleteTarget || deleting) return;
    setDeleting(true);
    try {
      await api(`/api/projects/${deleteTarget.id}`, { method: "DELETE" });
      setDeleteTarget(null);
      // Reload projects after deletion — the parent App component polls
      // automatically, but we want immediate feedback.
      window.location.reload();
    } catch (cause) {
      alert(cause instanceof Error ? cause.message : "无法删除项目");
    } finally {
      setDeleting(false);
    }
  };
  return <><header className="dashboard-bar"><a className="brand" href="/"><b>A</b><span>Auto<br />Development</span></a><div className="dashboard-actions"><button className="secondary" onClick={onManageSSH}>SSH 连接</button><button className="primary" onClick={importProject}>加载项目 <i>+</i></button></div></header><section className="project-dashboard command-dashboard"><header className="dashboard-intro"><div><span className="eyebrow">COMMAND DESK</span><h1>项目任务 <em>总览</em></h1></div><div className="dashboard-summary" aria-label="按项目状态筛选">{filters.map((item) => <button key={item.id} className={`${filter === item.id ? "selected " : ""}${item.id === "running" ? "summary-running" : ""}`} aria-pressed={filter === item.id} onClick={() => setFilter(item.id)}><small>{item.label}</small><b>{item.count}</b></button>)}</div></header><div className="dashboard-divider"><span>任务队列</span><i></i><small>{filter === "all" ? "执行中的任务已置顶" : `显示${filters.find((item) => item.id === filter)?.label}项目`}</small></div>{projects.length === 0 ? <div className="empty"><h2>还没有项目</h2><p>加载一个 WSL 本地目录后，即可在这里开始 AI 工具对话。</p><button className="primary" onClick={importProject}>加载项目</button></div> : filteredProjects.length === 0 ? <div className="empty filtered-empty"><h2>没有匹配的项目</h2><p>当前筛选下没有项目，可切换筛选查看其他任务。</p></div> : <div className="project-grid">{filteredProjects.map((item) => <ProjectCard key={item.id} project={item} status={statuses[item.id]} open={() => openProject(item)} onDelete={() => setDeleteTarget(item)} />)}</div>}</section>{deleteTarget && <DeleteProjectDialog project={deleteTarget} busy={deleting} close={() => setDeleteTarget(null)} confirm={confirmDelete} />}</>;
}

function ProjectCard({ project, status, open, onDelete }: { project: Project; status?: ProjectStatus; open: () => void; onDelete: () => void }) {
  const state = status?.running ? "正在执行" : project.agentReady ? "已就绪" : "工具不可用";
  const stateClass = status?.running ? "running" : project.agentReady ? "ready" : "offline";
  const taskTitle = status?.running && status.activeTitle ? status.activeTitle : "等待新的任务";
  return <article className={`project-card task-card state-${stateClass}`} onClick={open}><button className="project-card-open" type="button" onClick={(event) => { event.stopPropagation(); open(); }} aria-label={`进入 ${project.name} 的对话`} /><div className="project-card-top"><span className="project-mark">{project.name.slice(0, 1).toUpperCase()}</span><span className="project-card-top-actions"><span className={`project-state ${stateClass}`}><i></i>{state}</span>{project.environment === "windows" && <span className="env-tag windows" title="Windows 项目">🪟</span>}{project.environment === "remote-linux" && <span className="env-tag remote" title="远程服务器">🖥️</span>}</span></div><div className="project-card-title"><span>项目</span><h2>{project.name}</h2><p>{taskTitle}</p></div><div className="project-card-meta"><span><small>会话</small><b>{status ? status.conversationCount : "--"}</b></span><span><small>工作目录</small><b title={project.pathDisplay}>{project.pathDisplay}</b></span></div><footer><span>{project.gitBranch || "非 Git 项目"}</span><b aria-hidden="true">打开对话 <i>→</i></b></footer><button className="project-card-delete" type="button" title="删除项目" onClick={(event) => { event.stopPropagation(); onDelete(); }} aria-label={`删除项目 ${project.name}`}>×</button></article>;
}

function DeleteProjectDialog({ project, busy, close, confirm }: { project: Project; busy: boolean; close: () => void; confirm: () => Promise<void> }) {
  return <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="delete-project-title"><section className="modal"><header><div><label>DELETE PROJECT</label><h2 id="delete-project-title">删除项目</h2></div><button title="关闭" disabled={busy} onClick={close}>x</button></header><p className="permission-confirmation">确定要从列表中删除项目 <b>{project.name}</b> 吗？此操作会同时删除该项目下的所有对话历史、任务、运行记录及相关数据。项目源文件不会被删除。</p><footer><button className="secondary" disabled={busy} onClick={close}>取消</button><button className="primary danger" disabled={busy} onClick={() => void confirm()}>{busy ? "删除中" : "确认删除"}</button></footer></section></div>;
}

function Importer({ close, created, fail }: { close: () => void; created: (project: Project) => void; fail: (message: string) => void }) {
  const [path, setPath] = useState("");
  const [parent, setParent] = useState("");
  const [dirs, setDirs] = useState<Directory[]>([]);
  const [result, setResult] = useState<any>(null);
  const [busy, setBusy] = useState(false);
  const [roots, setRoots] = useState<{ name: string; path: string; label?: string; runnerId?: string }[]>([]);
  const [activeRoot, setActiveRoot] = useState<string | null>(null);
  const [activeRunner, setActiveRunner] = useState<string>("");
  const [loadingRoots, setLoadingRoots] = useState(true);

  // Fetch available root paths from the runner API.
  useEffect(() => {
    let cancelled = false;
    api<{ roots?: { name: string; path: string; label?: string }[]; root?: string; id?: string }[]>("/api/runners")
      .then((runners) => {
        if (cancelled) return;
        const rootsFromRunner: { name: string; path: string; label?: string; runnerId?: string }[] = [];
        for (const runner of runners) {
          if (runner.roots) {
            for (const r of runner.roots) {
              rootsFromRunner.push({ ...r, runnerId: runner.id });
            }
          } else if (runner.root) {
            rootsFromRunner.push({ name: "WSL Home", path: runner.root, runnerId: runner.id });
          }
        }
        setRoots(rootsFromRunner);
      })
      .catch(() => { if (!cancelled) fail("无法获取可用的根路径"); })
      .finally(() => { if (!cancelled) setLoadingRoots(false); });
    return () => { cancelled = true; };
  }, [fail]);

  const browse = useCallback(async (target = "") => {
    try {
      const query = target ? `?path=${encodeURIComponent(target)}` : "";
      const runnerParam = activeRunner ? `${query ? "&" : "?"}runner=${encodeURIComponent(activeRunner)}` : "";
      const data = await api<{ path: string; parent: string; directories: Directory[] }>(`/api/directories${query}${runnerParam}`);
      setPath(data.path); setParent(data.parent); setDirs(data.directories); setResult(null);
    } catch (cause) { fail(cause instanceof Error ? cause.message : "Unable to browse directories"); }
  }, [fail, activeRunner]);

  const selectRoot = (rootPath: string, runnerId?: string) => {
    setActiveRoot(rootPath);
    setActiveRunner(runnerId || "");
    void browse(rootPath);
  };
  const validate = async () => {
    setBusy(true);
    try { setResult(await api("/api/projects/validate", { method: "POST", body: JSON.stringify({ path, runner: activeRunner }) })); }
    catch (cause) { fail(cause instanceof Error ? cause.message : "Unable to validate directory"); }
    finally { setBusy(false); }
  };
  const create = async () => {
    setBusy(true);
    try { created(await api<Project>("/api/projects", { method: "POST", body: JSON.stringify({ path, name: result?.name, runner: activeRunner }) })); }
    catch (cause) { fail(cause instanceof Error ? cause.message : "Unable to load project"); setBusy(false); }
  };

  // When no root is selected yet, show the root picker.
  if (activeRoot === null && roots.length > 0 && !loadingRoots) {
    return (
      <div className="backdrop" role="dialog" aria-modal="true">
        <section className="modal">
          <header>
            <div><label>LOAD PROJECT</label><h2>选择环境</h2></div>
            <button title="关闭" onClick={close}>x</button>
          </header>
          <p className="permission-confirmation" style={{ marginBottom: "1rem" }}>请选择项目所在的目录位置：</p>
          <div className="dirs">
            {roots.map((root) => (
              <button className="environment-option" key={root.path} onClick={() => selectRoot(root.path, root.runnerId)}>
                <b className="root-option"><i>{root.label === "windows" ? "🪟" : root.label === "remote-linux" ? "🖥️" : "🐧"}</i><span>{root.name}</span></b>
                <small>{root.path}</small>
              </button>
            ))}
          </div>
          <footer><button className="secondary" onClick={close}>取消</button></footer>
        </section>
      </div>
    );
  }

  // Show a brief loading indicator while roots are being fetched.
  if (activeRoot === null && loadingRoots) {
    return <div className="backdrop" role="dialog" aria-modal="true"><section className="modal"><header><div><label>LOAD PROJECT</label><h2>加载项目</h2></div><button title="关闭" onClick={close}>x</button></header><p className="permission-confirmation">正在检测可用的环境…</p><footer><button className="secondary" onClick={close}>取消</button></footer></section></div>;
  }

  // Fallback: no roots available (should be rare — no runner found).
  if (activeRoot === null && roots.length === 0) {
    return <div className="backdrop" role="dialog" aria-modal="true"><section className="modal"><header><div><label>LOAD PROJECT</label><h2>加载项目</h2></div><button title="关闭" onClick={close}>x</button></header><p className="permission-confirmation">未检测到可用的运行环境，请确认 WSL 配置正确。</p><footer><button className="secondary" onClick={close}>取消</button></footer></section></div>;
  }

  return <div className="backdrop" role="dialog" aria-modal="true"><section className="modal"><header><div><label>LOAD PROJECT</label><h2>加载项目</h2></div><button title="关闭" onClick={close}>x</button></header><div className="path"><button title="返回根选择" onClick={() => setActiveRoot(null)}>⟵ 环境</button><button title="上级目录" onClick={() => void browse(parent)}>Up</button><code>{path}</code><button className="secondary" disabled={busy} onClick={() => void validate()}>校验目录</button></div><div className="dirs">{dirs.map((dir) => <button key={dir.path} onClick={() => void browse(dir.path)}><b>{dir.name}</b><small>{dir.path}</small></button>)}</div>{result && <div className={result.agentReady ? "valid" : "invalid"}><b>{result.name}</b><span>Git: {result.gitReady ? result.gitBranch : "未检测到 Git 仓库（仍可加载）"}</span><span>Claude Code: {result.claudeReady ? "可用" : "不可用"}</span><span>Codex: {result.codexReady ? "可用" : "不可用"}</span>{result.performance === "cross-filesystem" && <span>⚠️ 跨文件系统 — 读写性能可能较慢</span>}</div>}<footer><button className="secondary" onClick={close}>取消</button><button className="primary" disabled={!result?.agentReady || busy} onClick={() => void create()}>{busy ? "处理中" : "确认加载"}</button></footer></section></div>;
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
  const [showMobileActions, setShowMobileActions] = useState(false);
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
	const [showRun, setShowRun] = useState(false);
  const [showAgentExecution, setShowAgentExecution] = useState<string | null>(null);
  const [hasMoreHistory, setHasMoreHistory] = useState(false);
  const [hasMoreMessageHistory, setHasMoreMessageHistory] = useState(false);
  const [historyCursor, setHistoryCursor] = useState("");
  const [pendingConfirm, setPendingConfirm] = useState<{ title: string; message: React.ReactNode; danger?: boolean; onConfirm: () => void; onCancel: () => void } | null>(null);
  const [loadingOlderHistory, setLoadingOlderHistory] = useState(false);
  const [currentUserMessageIndex, setCurrentUserMessageIndex] = useState(-1);
  // null means no pending navigation; an empty string means find the newest
  // user message from a newly loaded older page.
  const [pendingPreviousUserMessageID, setPendingPreviousUserMessageID] = useState<string | null>(null);
  const bottom = useRef<HTMLDivElement>(null);
  const top = useRef<HTMLDivElement>(null);
  const composerRef = useRef<HTMLFormElement>(null);
  const timelineRef = useRef<HTMLElement>(null);
  const userMessageElements = useRef(new Map<string, HTMLDivElement>());
  const userMessageIndexFrame = useRef<number | null>(null);
  const historyIndex = useRef<number | null>(null);
  const draftBeforeHistory = useRef("");
  const finishedRunIds = useRef(new Set<string>());
  const usageRequestVersion = useRef(0);
  const usageConversationID = useRef<string | null>(null);
  /** Mirror of conversation state used inside async callbacks so they can check
   *  whether the conversation has changed without reaching into setState updaters. */
  const conversationRef = useRef<Conversation | null>(null);
  conversationRef.current = conversation;
  /** Mirror of stopRun so the Ctrl+C keydown effect always calls the latest version. */
  const stopRunRef = useRef<() => Promise<void>>(async () => {});
  // stopRunRef.current is updated after stopRun is defined later in the component body.
  /** Tracks whether the user is near the bottom of the timeline — when false,
   *  auto-scroll is suppressed so the user can read earlier messages undisturbed. */
  const userNearBottom = useRef(true);
  /** When the user has scrolled away and new content arrives, this flag is set
   *  so the scroll-to-bottom button can indicate unseen content. */
  const [hasNewContent, setHasNewContent] = useState(false);
  /** Tracks the last run ID for which reload() was requested, to debounce
   *  rapid successive reloads for the same run.  run.* events for a
   *  different run (or the first event for any run) always trigger a reload. */
  const lastReloadRequestedAt = useRef(0);
  const lastReloadRunID = useRef<string | null>(null);
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
    let cancelled = false;
    void (async () => {
      try {
        await api("/api/shortcuts/defaults", { method: "POST" });
        if (!cancelled) await refreshShortcuts();
      } catch (cause) { if (!cancelled) fail(cause instanceof Error ? cause.message : "Unable to load shortcuts"); }
    })();
    return () => { cancelled = true; };
  }, [fail, refreshShortcuts]);

  useEffect(() => {
    if (!conversation) return undefined;
    let cancelled = false;
    let socket: WebSocket;
    let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
    let reconnectAttempts = 0;

    const reload = async (runID?: string) => {
      if (runID) lastReloadRunID.current = runID;
      lastReloadRequestedAt.current = Date.now();
      try {
        const data = await api<{ messages: Message[]; events: Event[]; activeRunId: string | null; hasMore: boolean; hasMoreMessages: boolean; nextCursor: string }>(`/api/conversations/${conversation.id}?limit=400`);
        if (cancelled) return;
        setMessages((current) => mergeReloadMessages(data.messages, current));
        setEvents((current) => mergeConversationItems(data.events, current));
        setRun(data.activeRunId || ""); setHasMoreHistory(data.hasMore); setHasMoreMessageHistory(data.hasMoreMessages); setHistoryCursor(data.nextCursor || "");
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
        // assistant.message events carry the server-persisted Message with the
        // canonical database ID. Replace any temporary WS-sourced message that
        // shares the same content+runId so later HTTP reloads do not duplicate.
        if (event.type === "assistant.message") {
          const m = asRecord(event.payload);
          if (typeof m.id === "string" && typeof m.content === "string") {
            const canonicalID = String(m.id);
            const canonicalContent = String(m.content);
            const canonicalRunID = event.runId;
            const canonicalParent = typeof m.parentToolUseId === "string" ? m.parentToolUseId : "";
            const canonicalCreatedAt = typeof m.createdAt === "string" ? m.createdAt : event.createdAt;
            setMessages((old) => {
              // Replace every temporary WS-sourced message that matches on
              // content, runId, and parentToolUseId with the canonical one.
              // We must filter *all* matching temporaries (not just the first)
              // because the same content may appear as an assistant slice more
              // than once (e.g. the model repeats a phrase).
              const filtered = old.filter((item) => {
                if (!isTemporaryMessage(item) || item.role !== "assistant") return true;
                if (item.runId !== canonicalRunID) return true;
                if ((item.parentToolUseId || "") !== canonicalParent) return true;
                if (item.content !== canonicalContent) return true;
                return false;
              });
              return mergeConversationItems(
                filtered,
                [{ id: canonicalID, runId: canonicalRunID, role: "assistant" as const, content: canonicalContent, parentToolUseId: canonicalParent, createdAt: canonicalCreatedAt }],
              );
            });
          }
        }
        if (event.type === "assistant") {
          const content = asRecord(asRecord(event.payload).message).content || [];
          const parentToolUseId = typeof asRecord(event.payload).parent_tool_use_id === "string" ? asRecord(event.payload).parent_tool_use_id : "";
          for (const [index, part] of content.entries()) {
            if (part?.type !== "text" || typeof part.text !== "string") continue;
            const text = part.text;
            setMessages((old) => {
              // Skip adding a temporary message if a canonical record (from
              // assistant.message or HTTP reload) already exists for this
              // runId, parentToolUseId, and content tuple.  Otherwise the
              // temporary and canonical records coexist and mergeReloadMessages
              // drops the temporary on the next reload, causing a visual "flash".
              const alreadyCanonical = old.some(
                (item) =>
                  item.role === "assistant" &&
                  item.runId === event.runId &&
                  (item.parentToolUseId || "") === parentToolUseId &&
                  item.content === text &&
                  !isTemporaryMessage(item),
              );
              if (alreadyCanonical) return old;
              return mergeConversationItems(old, [
                {
                  id: websocketAssistantMessageID(event.id, index),
                  runId: event.runId,
                  role: "assistant" as const,
                  content: text,
                  parentToolUseId,
                  createdAt: event.createdAt,
                },
              ]);
            });
          }
        }
        if (event.type === "usage.updated" || event.type.startsWith("run.")) {
          void refreshUsage().catch((cause) => fail(cause instanceof Error ? cause.message : "Unable to refresh usage"));
        }
        if (event.type.startsWith("run.")) {
          // When a run fails or is interrupted, the server may append an
          // interrupt marker message. Display it immediately without waiting
          // for the next HTTP reload.
          const runPayload = asRecord(event.payload);
          if (typeof runPayload.interruptedMarker === "string" && runPayload.interruptedMarker) {
            const markerMessage: Message = { id: `${event.id}:interrupted`, runId: event.runId, role: "assistant", content: runPayload.interruptedMarker, createdAt: event.createdAt };
            setMessages((old) => mergeConversationItems(old, [markerMessage]));
          }
          rememberFinishedRun(event.runId);
          setRun((active) => active === event.runId ? "" : active);
          setStopping(false);
          // Reload from HTTP to ensure the frontend has the complete event list.
          // WebSocket frames may be lost during brief disconnections, which can
          // cause subagent tool_result events to go missing.  Without those
          // events buildAgentExecutions marks finished subagents as "unresolved"
          // and the UI appears stuck until the user sends another message.
          //
          // Debounce: skip if a reload for the *same* run was requested less
          // than 500 ms ago.  run.* events can arrive back-to-back (e.g.
          // run.completed immediately followed by run.interrupted).
          const sameRun = lastReloadRunID.current === event.runId;
          const recentReload = Date.now() - lastReloadRequestedAt.current < 500;
          if (!sameRun || !recentReload) {
            void reload(event.runId);
          }
        }
      };
      socket.onerror = () => {
        // The onclose handler will fire after this and handle reconnection.
      };
      socket.onclose = (closeEvent) => {
        if (cancelled) return;
        // Don't reconnect on normal closure (code 1000) or when the component
        // is unmounting. Also stop after too many failed attempts.
        if (closeEvent.code === 1000) return;
        const maxAttempts = 12;
        if (reconnectAttempts >= maxAttempts) {
          fail("实时连接已断开，请刷新页面重新建立连接。");
          // The WebSocket is permanently dead. Clear the active run state and
          // reload via HTTP so the UI doesn't stay stuck showing "Claude is
          // processing" when the backend has already finished (or failed).
          setRun("");
          setStopping(false);
          void reload();
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
    let cancelled = false;
    void refreshUsage().catch((cause) => { if (!cancelled) fail(cause instanceof Error ? cause.message : "Unable to load usage"); });
    return () => { cancelled = true; };
  }, [conversation?.id, fail, refreshUsage]);

  useEffect(() => {
    if (!run) return undefined;
    setUsageNow(Date.now());
    const timer = window.setInterval(() => setUsageNow(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, [run]);

  // Poll the conversation state via HTTP while a run is active, as a safety net.
  // If the WebSocket dies permanently (e.g. after exhausting reconnection
  // attempts), this polling ensures the UI eventually learns that the run has
  // finished and stops showing "Claude 正在处理已发送内容" forever.
  useEffect(() => {
    if (!run || !conversation?.id) return undefined;
    let cancelled = false;
    const poll = async () => {
      try {
        const data = await api<{ activeRunId: string | null }>(`/api/conversations/${conversation.id}?limit=1`);
        if (cancelled) return;
        if (!data.activeRunId) {
          // Reload from HTTP to also refresh events, not just clear the run
          // state.  This covers the case where the run finished but the
          // WebSocket delivered the run.* event without the preceding subagent
          // tool_result events (or they were lost entirely).
          const full = await api<{ messages: Message[]; events: Event[]; activeRunId: string | null; hasMore: boolean; hasMoreMessages: boolean; nextCursor: string }>(`/api/conversations/${conversation.id}?limit=400`);
          if (cancelled) return;
          setMessages((current) => mergeReloadMessages(full.messages, current));
          setEvents((current) => mergeConversationItems(full.events, current));
          setRun(full.activeRunId || ""); setHasMoreHistory(full.hasMore); setHasMoreMessageHistory(full.hasMoreMessages); setHistoryCursor(full.nextCursor || "");
          setStopping(false);
        }
      } catch {
        // Silently ignore polling errors — the WebSocket is the primary channel.
      }
    };
    const interval = window.setInterval(() => { void poll(); }, 15_000);
    return () => {
      cancelled = true;
      window.clearInterval(interval);
    };
  }, [run, conversation?.id]);

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
  const userMessages = useMemo(() => primaryMessages.filter((message) => message.role === "user"), [primaryMessages]);
  const timeline = useMemo(() => buildTimeline(primaryMessages, events), [events, primaryMessages]);
  const agentExecutions = useMemo(() => buildAgentExecutions(events), [events]);
  const executionByRun = useMemo(() => new Map(agentExecutions.map((execution) => [execution.runId, execution])), [agentExecutions]);
  const anchoredExecutionRunIDs = useMemo(() => new Set(primaryMessages.flatMap((message) => message.role === "user" && message.runId && executionByRun.has(message.runId) ? [message.runId] : [])), [executionByRun, primaryMessages]);
  const visibleContentVersion = useMemo(() => timelineContentVersion(timeline, agentExecutions), [timeline, agentExecutions]);
  const loadOlderHistory = async (): Promise<boolean> => {
    if (!conversation || !historyCursor || loadingOlderHistory || sending) return false;
    const conversationID = conversation.id;  // capture before async — guard against mid-flight switch
    const narrowLayout = isNarrowConversationLayout();
    const container = timelineRef.current;
    const scrollTop = container?.scrollTop || 0;
    const scrollHeight = narrowLayout ? document.documentElement.scrollHeight : container?.scrollHeight || 0;
    // Loading older history is an intentional move away from the live edge.
    userNearBottom.current = false;
    setLoadingOlderHistory(true);
    try {
      const data = await api<{ messages: Message[]; events: Event[]; hasMore: boolean; hasMoreMessages: boolean; nextCursor: string }>(`/api/conversations/${conversationID}?limit=400&cursor=${encodeURIComponent(historyCursor)}`);
      if (conversationRef.current?.id !== conversationID) return false;  // conversation switched, discard
      setMessages((current) => mergeConversationItems(data.messages, current));
      setEvents((current) => mergeConversationItems(data.events, current));
      setHasMoreHistory(data.hasMore);
      setHasMoreMessageHistory(data.hasMoreMessages);
      setHistoryCursor(data.nextCursor || "");
      requestAnimationFrame(() => {
        if (conversationRef.current?.id !== conversationID) return;
        const heightDelta = (narrowLayout ? document.documentElement.scrollHeight : container?.scrollHeight || 0) - scrollHeight;
        if (heightDelta <= 0) return;
        if (narrowLayout) window.scrollBy({ top: heightDelta });
        else if (container) container.scrollTop = scrollTop + heightDelta;
      });
      return true;
    } catch (cause) {
      fail(cause instanceof Error ? cause.message : "Unable to load earlier conversation history");
      return false;
    } finally { setLoadingOlderHistory(false); }
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
  /** Scroll the conversation timeline (now an independently scrolling column)
   *  to its bottom so the latest message is visible above the composer. */
  const scrollToBottom = useCallback((behavior: ScrollBehavior = "smooth") => {
    if (isNarrowConversationLayout()) {
      bottom.current?.scrollIntoView({ behavior, block: "end" });
      setHasNewContent(false);
      userNearBottom.current = true;
      setCurrentUserMessageIndex(userMessages.length - 1);
      return;
    }
    const container = timelineRef.current;
    if (!container) return;
    container.scrollTo({ top: container.scrollHeight, behavior });
    setHasNewContent(false);
    userNearBottom.current = true;
    setCurrentUserMessageIndex(userMessages.length - 1);
  }, [userMessages.length]);

  const scrollToTop = useCallback(() => {
    userNearBottom.current = false;
    setCurrentUserMessageIndex(userMessages.length > 0 ? 0 : -1);
    if (isNarrowConversationLayout()) {
      top.current?.scrollIntoView({ behavior: "smooth", block: "start" });
      return;
    }
    const container = timelineRef.current;
    if (!container) { top.current?.scrollIntoView({ behavior: "smooth", block: "start" }); return; }
    container.scrollTo({ top: 0, behavior: "smooth" });
  }, [userMessages.length]);

  const currentUserMessageIndexAtViewport = useCallback(() => {
    if (userMessages.length === 0) return -1;
    const viewportTop = isNarrowConversationLayout() ? 0 : timelineRef.current?.getBoundingClientRect().top || 0;
    let low = 0;
    let high = userMessages.length - 1;
    let index = 0;
    while (low <= high) {
      const candidate = Math.floor((low + high) / 2);
      const element = userMessageElements.current.get(userMessages[candidate].id);
      if (!element || element.getBoundingClientRect().top > viewportTop + 16) {
        high = candidate - 1;
      } else {
        index = candidate;
        low = candidate + 1;
      }
    }
    return index;
  }, [userMessages]);

  const updateCurrentUserMessageIndex = useCallback(() => {
    const next = currentUserMessageIndexAtViewport();
    setCurrentUserMessageIndex((current) => current === next ? current : next);
  }, [currentUserMessageIndexAtViewport]);

  const scheduleCurrentUserMessageIndexUpdate = useCallback(() => {
    if (userMessageIndexFrame.current !== null) return;
    userMessageIndexFrame.current = requestAnimationFrame(() => {
      userMessageIndexFrame.current = null;
      updateCurrentUserMessageIndex();
    });
  }, [updateCurrentUserMessageIndex]);

  const scrollToUserMessage = useCallback((index: number) => {
    const message = userMessages[index];
    const element = message && userMessageElements.current.get(message.id);
    if (!element) return false;
    userNearBottom.current = false;
    element.scrollIntoView({ behavior: "smooth", block: "start" });
    setCurrentUserMessageIndex(index);
    return true;
  }, [userMessages]);

  const goToPreviousUserMessage = () => {
    if (loadingOlderHistory) return;
    const index = currentUserMessageIndexAtViewport();
    if (index > 0) {
      scrollToUserMessage(index - 1);
      return;
    }
    if (index === -1 && hasMoreMessageHistory) {
      setPendingPreviousUserMessageID("");
      void loadOlderHistory().then((loaded) => { if (!loaded) setPendingPreviousUserMessageID(null); });
      return;
    }
    if (index === 0 && hasMoreMessageHistory) {
      setPendingPreviousUserMessageID(userMessages[0].id);
      void loadOlderHistory().then((loaded) => { if (!loaded) setPendingPreviousUserMessageID(null); });
    }
  };

  const goToNextUserMessage = () => {
    const index = currentUserMessageIndexAtViewport();
    if (index >= 0 && index < userMessages.length - 1) scrollToUserMessage(index + 1);
  };

  const onTimelineScroll = () => {
    if (isNarrowConversationLayout()) return;
    const container = timelineRef.current;
    if (!container) return;
    const nearBottom = container.scrollHeight - container.scrollTop - container.clientHeight < 64;
    userNearBottom.current = nearBottom;
    if (nearBottom) setHasNewContent(false);
    scheduleCurrentUserMessageIndexUpdate();
  };

  // A newly selected conversation starts at its latest content. Do not carry
  // the reader's intentional scroll position from the previous conversation.
  useEffect(() => {
    userNearBottom.current = true;
    setHasNewContent(false);
    setCurrentUserMessageIndex(-1);
    setPendingPreviousUserMessageID(null);
  }, [conversation?.id]);

  // On narrow screens the page scrolls; desktop uses the independently
  // scrolling timeline above. Keep the near-bottom state tied to that owner.
  useEffect(() => {
    const media = window.matchMedia("(max-width: 820px)");
      const onScroll = () => {
        if (!media.matches) return;
      const distanceFromBottom = document.documentElement.scrollHeight - window.scrollY - window.innerHeight;
      const composerHeight = composerRef.current?.getBoundingClientRect().height ?? 80;
      const threshold = Math.max(composerHeight, 60); // at least 60 px to avoid jitter on tiny windows
      const nearBottom = distanceFromBottom <= threshold;
      userNearBottom.current = nearBottom;
      // Only call setHasNewContent when the flag actually changes to avoid
      // unnecessary React re-renders on every scroll tick.
        if (nearBottom) {
          setHasNewContent((prev) => prev ? false : prev);
        }
        scheduleCurrentUserMessageIndexUpdate();
      };
    const onLayoutChange = () => {
      if (media.matches) {
        onScroll();
        return;
      }
      const container = timelineRef.current;
      if (!container) return;
      const nearBottom = container.scrollHeight - container.scrollTop - container.clientHeight < 64;
      userNearBottom.current = nearBottom;
      if (nearBottom) setHasNewContent((prev) => prev ? false : prev);
      scheduleCurrentUserMessageIndexUpdate();
    };
    window.addEventListener("scroll", onScroll, { passive: true });
    media.addEventListener("change", onLayoutChange);
    onLayoutChange();
    return () => {
      window.removeEventListener("scroll", onScroll);
      media.removeEventListener("change", onLayoutChange);
    };
  }, [scheduleCurrentUserMessageIndexUpdate]);

  useEffect(() => () => {
    if (userMessageIndexFrame.current !== null) cancelAnimationFrame(userMessageIndexFrame.current);
  }, []);

  useEffect(() => {
    const frame = requestAnimationFrame(updateCurrentUserMessageIndex);
    return () => cancelAnimationFrame(frame);
  }, [updateCurrentUserMessageIndex, visibleContentVersion]);

  useEffect(() => {
    if (pendingPreviousUserMessageID === null || loadingOlderHistory) return;
    if (pendingPreviousUserMessageID === "" && userMessages.length > 0) {
      setPendingPreviousUserMessageID(null);
      requestAnimationFrame(() => { scrollToUserMessage(userMessages.length - 1); });
      return;
    }
    const currentIndex = userMessages.findIndex((message) => message.id === pendingPreviousUserMessageID);
    if (currentIndex > 0) {
      setPendingPreviousUserMessageID(null);
      // The page-load callback first restores the reader's previous offset.
      // Run the actual target jump on the next frame so it wins over that restore.
      requestAnimationFrame(() => { scrollToUserMessage(currentIndex - 1); });
      return;
    }
    if (!hasMoreMessageHistory) {
      setPendingPreviousUserMessageID(null);
      return;
    }
    void loadOlderHistory().then((loaded) => { if (!loaded) setPendingPreviousUserMessageID(null); });
  }, [hasMoreMessageHistory, loadOlderHistory, loadingOlderHistory, pendingPreviousUserMessageID, scrollToUserMessage, userMessages]);

  // Ctrl+C (or Cmd+C on macOS) to stop the active run — mimics Claude Code terminal behavior.
  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      // Only intercept when a run is active and not already stopping.
      if (!run || stopping) return;
      // Check for Ctrl+C (or Cmd+C on macOS).
      if ((event.ctrlKey || event.metaKey) && event.key === "c") {
        // Don't intercept when the user is copying text — if there is a selection,
        // let the browser handle the copy instead of triggering stop.
        const selection = window.getSelection();
        if (selection && selection.toString().trim()) return;
        // Don't intercept when a textarea or input is focused — the user might be
        // composing text and the browser's copy shortcut takes priority.
        const tag = document.activeElement?.tagName;
        if (tag === "TEXTAREA" || tag === "INPUT") return;
        event.preventDefault();
        void stopRunRef.current();
      }
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [run, stopping]);

  useEffect(() => {
    if (pendingApproval) return; // never scroll away while a command is waiting for approval
    if (userNearBottom.current) {
      // Streaming output can update an existing tool card without adding a
      // timeline item. Use an immediate scroll so rapid output stays caught up.
      scrollToBottom("auto");
    } else {
      setHasNewContent((prev) => prev ? prev : true);
    }
  }, [visibleContentVersion, run, pendingApproval, scrollToBottom]);

  const sendContent = async (rawContent: string, clearDraft = true) => {
    if (!conversation || sending || shortcutBusy) return;
    const content = rawContent.trim();
    if (!content) return;
    if (content === "/resume") { if (clearDraft) setText(""); openConversationHistory(); return; }
    const conversationID = conversation.id;  // capture before async — the conversation may change while we wait
    setSending(true);
    if (clearDraft) setText("");
    setShowPermissionMenu(false); historyIndex.current = null; draftBeforeHistory.current = "";
    try {
      const data = await api<{ message: Message; runId: string }>(`/api/conversations/${conversationID}/messages`, { method: "POST", body: JSON.stringify({ content }) });
      if (conversationRef.current?.id !== conversationID) return;  // conversation switched mid-flight, discard
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
      const conversationID = conversation.id;  // capture before async — guard against mid-flight conversation switch
      const data = await api<{ message: Message; runId: string }>(`/api/conversations/${conversationID}/shortcuts/${shortcut.id}/run`, { method: "POST", body: JSON.stringify({ variables, action }) });
      if (conversationRef.current?.id !== conversationID) return;  // conversation switched, discard
      setMessages((old) => [...old, data.message]);
      setInputHistory((old) => [...old.slice(-99), data.message.content]);
      setHistoryRefresh((version) => version + 1);
      setRun(finishedRunIds.current.has(data.runId) ? "" : data.runId);
      void refreshUsage().catch((cause) => fail(cause instanceof Error ? cause.message : "Unable to refresh usage"));
      void refreshConversationHistory().catch((cause) => fail(cause instanceof Error ? cause.message : "Unable to refresh conversation history"));
    } catch (cause) { fail(cause instanceof Error ? cause.message : "Unable to run shortcut"); }
    finally { setShortcutBusy(""); }
  };
  const newConversation = async (agentId: AgentID, permissionMode: PermissionMode) => {
    if (sending || shortcutBusy) { setShowNewConversation(false); return; }
    try {
      const next = await api<Conversation>(`/api/projects/${project.id}/conversations?new=true`, { method: "POST", body: JSON.stringify({ agentId, permissionMode }) });
      setMessages([]); setEvents([]); setRun(""); setUsage(null); setInputHistory([]); historyIndex.current = null; draftBeforeHistory.current = ""; finishedRunIds.current.clear(); setShowPermissionMenu(false); setShowAgentExecution(null); setHasMoreHistory(false); setHasMoreMessageHistory(false); setHistoryCursor(""); setConversation(next);
      void refreshConversationHistory().catch((cause) => fail(cause instanceof Error ? cause.message : "Unable to refresh conversation history"));
      setShowNewConversation(false);
    }
    catch (cause) { fail(cause instanceof Error ? cause.message : "Unable to start a new conversation"); }
  };
  const activateConversation = async (item: Conversation) => {
    if (!conversation || item.id === conversation.id || item.status === "running") { setShowHistory(false); return; }
    if (sending || shortcutBusy) { setShowHistory(false); return; }
    setActivatingConversation(item.id);
    try {
      const next = await api<Conversation>(`/api/conversations/${item.id}/activate`, { method: "POST" });
      setMessages([]); setEvents([]); setRun(""); setUsage(null); setInputHistory([]); historyIndex.current = null; draftBeforeHistory.current = ""; finishedRunIds.current.clear(); setShowPermissionMenu(false); setShowAgentExecution(null); setHasMoreHistory(false); setHasMoreMessageHistory(false); setHistoryCursor(""); setConversation(next); setShowHistory(false);
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
  const stopRunInternal = async (force: boolean) => {
    const runID = run;
    if (!runID) return;
    const url = force ? `/api/runs/${runID}/stop?force=true` : `/api/runs/${runID}/stop`;
    const result = await api<{ status: string }>(url, { method: "POST" });
    if (result.status !== "stopping") { rememberFinishedRun(runID); setRun((active) => active === runID ? "" : active); setStopping(false); }
  };
  const stopRun = async () => {
    if (!run || stopping) return;
    setStopping(true);
    try {
      await stopRunInternal(false);
    }
    catch (cause) {
      const message = cause instanceof Error ? cause.message : "";
      if (message.includes("queued") || message.includes("other queued") || message.includes("other queued or running")) {
        setPendingConfirm({
          title: "强制停止",
          message: "该对话还有其他排队中或执行中的请求，强制停止将一并取消它们。是否继续？",
          danger: true,
          onConfirm: () => {
            setPendingConfirm(null);
            void stopRunInternal(true).catch((cause) => {
              fail(cause instanceof Error ? cause.message : "Unable to stop run");
              setStopping(false);
            });
          },
          onCancel: () => { setPendingConfirm(null); setStopping(false); },
        });
        return;
      }
      fail(cause instanceof Error ? cause.message : "Unable to stop run");
      setStopping(false);
    }
  };
  stopRunRef.current = stopRun;
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

  const isCodex = conversation?.agentId === "codex";
	const isRemoteProject = project.runner.startsWith("ssh-");
  const permissionLabel = isCodex ? (conversation?.permissionMode === "read_only" ? "仅分析" : "项目内执行") : conversation?.permissionMode === "full_control" ? "完全控制" : "默认权限";
  const runLabel = isCodex ? "Codex 正在处理已发送内容" : conversation?.permissionMode === "full_control" ? "Claude 正在处理已发送内容" : "Claude 正在分析或等待命令确认";
  const currentUsage = usage?.currentRun ?? usage?.latestRun;
	const displayedModel = usage?.context.model || currentUsage?.model || (isCodex ? "Codex" : "Claude Code");
  const promptShortcuts = shortcuts.filter((shortcut) => shortcut.kind === "prompt" || shortcut.kind === "snippet");
  const commandShortcuts = shortcuts.filter((shortcut) => shortcut.kind === "command_request");
  const promptShortcutItems = promptShortcuts.length > 0 ? promptShortcuts : [undefined];
  const commandShortcutItems = commandShortcuts.length > 0 ? commandShortcuts : [undefined];
  const userMessageNavigationIndex = userMessages.length === 0 ? -1 : Math.max(0, Math.min(currentUserMessageIndex, userMessages.length - 1));
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
      <div className={`head-actions-menu${showMobileActions ? " mobile-open" : ""}`}>
        <button className="head-actions-mobile-toggle" type="button" aria-expanded={showMobileActions} onClick={() => setShowMobileActions((open) => !open)}>操作</button>
        <div className="head-actions">
        <div className="permission-menu">
          <button className={`permission-trigger ${conversation?.permissionMode === "full_control" ? "full" : ""}`} disabled={!!run || changingPermission} onClick={() => setShowPermissionMenu((open) => !open)}>{permissionLabel}</button>
          {showPermissionMenu && <div className="permission-popover" role="menu">{isCodex ? <><button className={conversation?.permissionMode === "read_only" ? "selected" : ""} onClick={() => void changePermissionMode("read_only")}><b>仅分析</b><span>只读检查，不修改项目。</span></button><button className={conversation?.permissionMode === "workspace_write" ? "selected" : ""} onClick={() => void changePermissionMode("workspace_write")}><b>项目内执行</b><span>可在当前项目范围内读写和执行。</span></button></> : <><button className={conversation?.permissionMode === "approval_required" ? "selected" : ""} onClick={() => void changePermissionMode("approval_required")}><b>默认权限</b><span>命令执行前需要确认</span></button><button className={conversation?.permissionMode === "full_control" ? "selected full" : ""} onClick={() => void changePermissionMode("full_control")}><b>完全控制</b><span>命令直接执行</span></button></>}</div>}
        </div>
        <button className="secondary" disabled={sending} onClick={openConversationHistory}>历史</button>
        <button className="secondary" disabled={!!run || sending} onClick={() => setShowNewConversation(true)}>新会话</button>
        <button className="secondary" onClick={() => setShowTasks({})}>任务</button>
        {project.gitBranch !== "非 Git 目录" && <button className="secondary git-trigger" disabled={isRemoteProject} title={isRemoteProject ? "远程 Git 工作台尚未支持" : undefined} onClick={() => setShowGit(true)}>Git: {project.gitBranch || "HEAD"}</button>}
        <button className="secondary" disabled={isRemoteProject} onClick={() => setShowRun(true)} title={isRemoteProject ? "远程项目启动尚未支持" : "项目启动"}>▶ 启动</button>
        </div>
      </div>
    </header>
    {showGit && <GitWorkbench projectID={project.id} request={api} fail={fail} close={() => setShowGit(false)} />}
	    {showRun && <ProjectRunPanel projectID={project.id} request={api} fail={fail} close={() => setShowRun(false)} />}
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
      <section className="chat-center">
      <section className="timeline" ref={timelineRef} onScroll={onTimelineScroll}>
        <div ref={top} />
        {hasMoreHistory && <button className="secondary load-earlier-history" type="button" disabled={loadingOlderHistory || sending} onClick={() => void loadOlderHistory()}>{loadingOlderHistory ? "加载中" : "加载更早记录"}</button>}
        {timeline.length === 0 && <div className="empty conversation-empty"><h2>开始与 {isCodex ? "Codex" : "Claude Code"} 对话</h2><p>选择常用操作，或直接描述希望工具完成的工作。</p><button className="secondary" type="button" onClick={() => composerRef.current?.querySelector("textarea")?.focus()}>开始输入</button></div>}
        {timeline.map((item) => <div key={item.id} data-user-message-id={item.kind === "message" && item.message.role === "user" ? item.message.id : undefined} ref={item.kind === "message" && item.message.role === "user" ? (element) => { if (element) userMessageElements.current.set(item.message.id, element); else userMessageElements.current.delete(item.message.id); } : undefined}><div className={`timeline-entry ${item.kind}`}>{item.kind === "message" ? <MessageCard message={item.message} agentID={conversation?.agentId || "claude-code"} /> : item.kind === "tool" ? <ToolCard action={item.action} resolving={resolving} decide={decide} /> : <ErrorCard item={item} />}</div>{item.kind === "message" && item.message.role === "user" && item.message.runId && executionByRun.get(item.message.runId) && <AgentExecutionCard execution={executionByRun.get(item.message.runId)!} open={() => setShowAgentExecution(item.message.runId!)} />}</div>)}
        {agentExecutions.filter((execution) => !anchoredExecutionRunIDs.has(execution.runId)).map((execution) => <AgentExecutionCard key={execution.runId} execution={execution} open={() => setShowAgentExecution(execution.runId)} />)}
        {run && <div className="run-indicator"><span></span>{runLabel}</div>}
        <div ref={bottom} />
      </section>
      <div className={`scroll-buttons${hasNewContent ? " has-new" : ""}`}>
        <button type="button" className="scroll-btn scroll-to-top" title="回到顶部" aria-label="回到顶部" onClick={scrollToTop}>⇞</button>
        <button type="button" className="scroll-btn scroll-to-previous-message" title="上一条我的消息" aria-label="上一条我的消息" disabled={loadingOlderHistory || (userMessageNavigationIndex <= 0 && !hasMoreMessageHistory)} onClick={goToPreviousUserMessage}>↑</button>
        <button type="button" className="scroll-btn scroll-to-next-message" title="下一条我的消息" aria-label="下一条我的消息" disabled={userMessageNavigationIndex < 0 || userMessageNavigationIndex >= userMessages.length - 1} onClick={goToNextUserMessage}>↓</button>
        <button type="button" className={`scroll-btn scroll-to-bottom${hasNewContent ? " pulse" : ""}`} title="回到底部" aria-label="回到底部" onClick={() => scrollToBottom()}>⇟</button>
      </div>
      <form ref={composerRef} className={`composer${pendingApproval ? " has-approval" : ""}`} onSubmit={(event) => void send(event)}>
        {pendingApproval && <ApprovalBanner action={pendingApproval} resolving={resolving} decide={decide} scrollToCard={() => { const el = timelineRef.current?.querySelector(".timeline-entry.tool .tool-card.waiting"); if (el) el.scrollIntoView({ behavior: "smooth", block: "center" }); }} />}
        <textarea value={text} onChange={(event) => handleTextChange(event.target.value)} onKeyDown={(event) => { if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing) { event.preventDefault(); event.currentTarget.form?.requestSubmit(); return; } navigateInputHistory(event); }} placeholder={`描述希望${isCodex ? " Codex" : " Claude"}在当前项目中完成的工作...`} disabled={sending || Boolean(shortcutBusy)} />
        <div><ComposerRunnerInfo runnerID={project.runner} agentID={conversation?.agentId || "claude-code"} run={run} runLabel={runLabel} permissionMode={conversation?.permissionMode} usage={usage} displayedModel={displayedModel} contextLabel={contextLabel(usage?.context).replace(/^上下文 /, "")} contextLevel={contextLevel(usage?.context)} onShowUsage={() => setShowUsage(true)} stopping={stopping} onStop={() => void stopRun()} /><span className="composer-actions"><button className="secondary" type="button" disabled={sending} onClick={() => void sendContent("继续", false)}>继续</button>{run && <button className="secondary" type="button" disabled={stopping} onClick={() => void stopRun()}>{stopping ? "停止中" : "停止"}</button>}<button className="primary" disabled={!text.trim() || sending || Boolean(shortcutBusy)}>{sending ? "发送中" : "发送"}</button></span></div>
      </form>
      </section>
    </section></>}
    {showNewConversation && <NewConversationDialog runnerID={project.runner} close={() => setShowNewConversation(false)} create={newConversation} />}
    {showHistory && <ConversationHistoryDialog conversations={conversationHistory} activeID={conversation?.id || ""} busyID={activatingConversation} close={() => setShowHistory(false)} activate={activateConversation} />}
    {showFullControlConfirmation && <FullControlConfirmationDialog close={() => setShowFullControlConfirmation(false)} confirm={confirmFullControl} changing={changingPermission} />}
    {showAgentExecution && agentExecutions.find((execution) => execution.runId === showAgentExecution) && <AgentExecutionDialog execution={agentExecutions.find((execution) => execution.runId === showAgentExecution)!} close={() => setShowAgentExecution(null)} />}
    {showUsage && <UsageDialog agentID={conversation?.agentId || "claude-code"} usage={usage} currentRun={currentUsage} now={usageNow} close={() => setShowUsage(false)} />}
    {shortcutEditor && <ShortcutEditor projectID={project.id} state={shortcutEditor} close={() => setShortcutEditor(null)} refresh={refreshShortcuts} fail={fail} />}
    {shortcutVariables && <ShortcutVariablesDialog state={shortcutVariables} close={() => setShortcutVariables(null)} run={(variables) => { setShortcutVariables(null); void runShortcut(shortcutVariables.shortcut, variables, true); }} />}
    {pendingConfirm && createPortal(<ConfirmDialog title={pendingConfirm.title} message={pendingConfirm.message} danger={pendingConfirm.danger} onConfirm={pendingConfirm.onConfirm} onCancel={pendingConfirm.onCancel} />, document.body)}
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
  const [pendingConfirm, setPendingConfirm] = useState<{ title: string; message: React.ReactNode; danger?: boolean; onConfirm: () => void; onCancel: () => void } | null>(null);
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
  const remove = () => {
    if (!shortcut) return;
    setPendingConfirm({
      title: "删除快捷方式",
      message: <>删除"<b>{shortcut.name}</b>"？</>,
      danger: true,
      onConfirm: () => void (async () => {
        setPendingConfirm(null);
        setBusy(true);
        try {
          await api(`/api/shortcuts/${shortcut.id}`, { method: "DELETE" });
          await refresh();
          close();
        } catch (cause) { fail(cause instanceof Error ? cause.message : "Unable to delete shortcut"); }
        finally { setBusy(false); }
      })(),
      onCancel: () => setPendingConfirm(null),
    });
  };

  return <><div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="shortcut-editor-title"><section className="modal shortcut-editor"><header><div><label>{isCommand ? "COMMON COMMAND" : "COMMON PROMPT"}</label><h2 id="shortcut-editor-title">{shortcut ? `编辑${isCommand ? "命令" : "提示词"}` : `新增${isCommand ? "命令" : "提示词"}`}</h2></div><button title="关闭" disabled={busy} onClick={close}>x</button></header><form onSubmit={(event) => void save(event)}><div className="shortcut-editor-body"><label>名称<input autoFocus required maxLength={64} value={name} onChange={(event) => setName(event.target.value)} placeholder={isCommand ? "例如：清空终端" : "例如：审查当前改动"} /></label><label>{isCommand ? "命令" : "提示词"}<textarea required maxLength={12000} value={template} onChange={(event) => setTemplate(event.target.value)} placeholder={isCommand ? "例如：clear" : "描述希望 Claude 在当前项目完成的工作"} /></label>{shortcut && <label className="shortcut-enabled"><input type="checkbox" checked={enabled} onChange={(event) => setEnabled(event.target.checked)} />启用此项</label>}{isCommand && <p>点击标签后会按当前会话权限请求执行该命令。</p>}</div><footer>{shortcut && <button type="button" className="danger-text" disabled={busy} onClick={() => void remove()}>删除</button>}<span></span><button type="button" className="secondary" disabled={busy} onClick={close}>取消</button><button className="primary" disabled={busy}>{busy ? "保存中" : "保存"}</button></footer></form></section></div>{pendingConfirm && createPortal(<ConfirmDialog title={pendingConfirm.title} message={pendingConfirm.message} danger={pendingConfirm.danger} onConfirm={pendingConfirm.onConfirm} onCancel={pendingConfirm.onCancel} />, document.body)}</>;
}

function UsageDialog({ agentID, usage, currentRun, now, close }: { agentID: AgentID; usage: ConversationUsageResponse | null; currentRun: RunUsage | undefined; now: number; close: () => void }) {
	if (usage && !usage.available) return <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="usage-title"><section className="modal usage-dialog"><header><div><label>{agentID === "codex" ? "CODEX" : "AI"} USAGE</label><h2 id="usage-title">使用状态</h2></div><button title="关闭" onClick={close}>x</button></header><div className="usage-body"><p className="usage-note">{usage.reason || "当前工具未提供可验证的使用统计。"}</p></div><footer><button className="secondary" onClick={close}>关闭</button></footer></section></div>;
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
  const agentName = agentID === "codex" ? "Codex" : "Claude Code";
  return <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="usage-title"><section className="modal usage-dialog"><header><div><label>{agentName.toUpperCase()} USAGE</label><h2 id="usage-title">使用状态</h2></div><button title="关闭" onClick={close}>x</button></header><div className="usage-body">
    <section className="usage-section"><div className="usage-section-head"><h3>当前任务</h3>{task?.model && <span>{task.model}</span>}</div>{task ? <><dl className="usage-grid">{metrics.map(([label, value]) => <div key={String(label)}><dt>{label}</dt><dd>{value}</dd></div>)}</dl>{!hasTaskUsage && !active && <p className="usage-note">{task.reason || `该任务未获得 ${agentName} 的最终统计数据。`}</p>}</> : <p className="usage-note">当前会话还没有可用的任务统计。</p>}</section>
    <section className="usage-section"><div className="usage-section-head"><h3>当前会话</h3><span className={`context-state ${contextLevel(usage?.context)}`}>{contextLabel(usage?.context)}</span></div>{(usage?.context.contextWindow ?? 0) > 0 && <p className="context-detail">{usage?.context.contextInputTokens ? `${formatTokens(usage.context.contextInputTokens)} / ${formatTokens(usage.context.contextWindow!)}` : usage?.context.available === false ? usage.context.reason || "当前工具未提供上下文快照" : usage?.context.hasResult ? "当前工具未提供上下文快照" : "等待上下文快照"}</p>}{session && <dl className="usage-grid session-grid"><div><dt>任务次数</dt><dd>{session.taskCount}</dd></div><div><dt>Agent 轮次</dt><dd>{session.agentTurns}</dd></div><div><dt>模型步骤</dt><dd>{session.modelSteps}</dd></div><div><dt>工具调用</dt><dd>{session.toolCalls}</dd></div><div><dt>输入 / 输出</dt><dd>{formatTokens(session.inputTokens)} / {formatTokens(session.outputTokens)}</dd></div><div><dt>缓存读取 / 创建</dt><dd>{formatTokens(session.cacheReadTokens)} / {formatTokens(session.cacheCreationTokens)}</dd></div><div><dt>费用估算</dt><dd>{formatCost(session.estimatedCostUsd)}</dd></div></dl>}</section>
    {usage?.models.length ? <section className="usage-section"><div className="usage-section-head"><h3>模型用量</h3><span>包含子代理</span></div><div className="model-usage-list">{usage.models.map((model) => <div key={model.model}><b>{model.model}</b><span>{formatTokens(model.inputTokens)} 输入 · {formatTokens(model.outputTokens)} 输出</span><em>{formatCost(model.estimatedCostUsd)}</em></div>)}</div></section> : null}
    <p className="usage-disclaimer">费用为客户端事件估算值，不代表账单金额。</p>
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
  return <div className="backdrop history-backdrop" role="dialog" aria-modal="true" aria-label="会话历史"><section className="modal conversation-history"><header><div><label>AI SESSION</label><h2>恢复会话</h2></div><button title="关闭" onClick={close}>x</button></header><input autoFocus value={query} onChange={(event) => setQuery(event.target.value)} onKeyDown={keyDown} placeholder="搜索历史会话" /><div className="history-list">{filtered.length === 0 ? <p className="history-empty">没有匹配的会话</p> : filtered.map((item, index) => {
    const runningElsewhere = item.status === "running" && item.id !== activeID;
    return <button key={item.id} className={`history-item ${busyID === item.id ? "activating" : ""} ${item.id === activeID ? "active" : ""} ${index === selected ? "selected" : ""}`} disabled={Boolean(busyID) || runningElsewhere} onMouseEnter={() => setSelected(index)} onClick={() => select(item)}><span><b>{item.title || "新会话"}</b><small>{item.agentId === "codex" ? "Codex" : "Claude Code"} · {item.preview || "尚未发送消息"}</small></span><em>{busyID === item.id ? "切换中…" : item.id === activeID ? "当前" : runningElsewhere ? "运行中" : formatHistoryTime(item.lastActivityAt)}</em></button>;
  })}</div><footer><small>输入 /resume 可再次打开</small><button className="secondary" onClick={close}>关闭</button></footer></section></div>;
}

function NewConversationDialog({ runnerID, close, create }: { runnerID: string; close: () => void; create: (agentId: AgentID, permissionMode: PermissionMode) => Promise<void> }) {
  const [agentId, setAgentId] = useState<AgentID>("claude-code");
  const [permissionMode, setPermissionMode] = useState<PermissionMode>("full_control");
  const [creating, setCreating] = useState(false);
  const [runner, setRunner] = useState<RunnerInfo | null>(null);
  useEffect(() => { api<RunnerInfo[]>("/api/runners").then((items) => setRunner(items.find((item) => item.id === runnerID) || null)).catch(() => setRunner(null)); }, [runnerID]);
  const selectAgent = (next: AgentID) => { setAgentId(next); setPermissionMode(next === "codex" ? "workspace_write" : "full_control"); };
  const codexStatus = runner?.codex;
  const codexReady = codexStatus?.status === "ready";
  const submit = async () => { if (agentId === "codex" && !codexReady) return; setCreating(true); try { await create(agentId, permissionMode); } finally { setCreating(false); } };
  const codex = agentId === "codex";
  return <div className="backdrop" role="dialog" aria-modal="true"><section className="modal permission-dialog"><header><div><label>AI SESSION</label><h2>新会话</h2></div><button title="关闭" onClick={close}>x</button></header><div className="permission-options"><button className={!codex ? "active" : ""} onClick={() => selectAgent("claude-code")}><b>Claude Code</b><span>保留命令确认与完全控制模式。</span></button><button className={codex ? "active" : ""} disabled={Boolean(runner) && !codexReady} title={codexStatus?.reason} onClick={() => selectAgent("codex")}><b>Codex</b><span>{codexReady ? "使用本地 Codex CLI 会话。" : codexStatus?.reason || "正在检查 Codex CLI 状态。"}</span></button></div><div className="permission-options">{codex ? <><button className={permissionMode === "read_only" ? "active" : ""} onClick={() => setPermissionMode("read_only")}><b>仅分析</b><span>只读检查，不修改项目。</span></button><button className={permissionMode === "workspace_write" ? "active" : ""} onClick={() => setPermissionMode("workspace_write")}><b>项目内执行</b><span>可在项目范围内读写和执行。</span></button></> : <><button className={permissionMode === "approval_required" ? "active" : ""} onClick={() => setPermissionMode("approval_required")}><b>默认权限</b><span>每条终端命令执行前等待确认。</span></button><button className={permissionMode === "full_control" ? "active" : ""} onClick={() => setPermissionMode("full_control")}><b>完全控制</b><span>Claude 可直接执行命令，不会等待确认。</span></button></>}</div><footer><button className="secondary" onClick={close}>取消</button><button className="primary" disabled={creating || (codex && !codexReady)} onClick={() => void submit()}>{creating ? "创建中" : "创建会话"}</button></footer></section></div>;
}

function FullControlConfirmationDialog({ close, confirm, changing }: { close: () => void; confirm: () => Promise<void>; changing: boolean }) {
  return <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="full-control-title"><section className="modal permission-dialog"><header><div><label>CLAUDE CODE PERMISSION</label><h2 id="full-control-title">切换为完全控制</h2></div><button title="关闭" disabled={changing} onClick={close}>x</button></header><p className="permission-confirmation">完全控制会允许 Claude 在当前项目中直接执行所有命令，不再等待确认。</p><footer><button className="secondary" disabled={changing} onClick={close}>取消</button><button className="primary danger" disabled={changing} onClick={() => void confirm()}>{changing ? "切换中" : "确认切换"}</button></footer></section></div>;
}

function MessageCard({ message, agentID }: { message: Message; agentID: AgentID }) {
  const isUser = message.role === "user";
  const agentName = agentID === "codex" ? "Codex" : "Claude";
  return <article className={`message ${message.role}`}><header><span className="message-avatar">{isUser ? "你" : agentID === "codex" ? "O" : "C"}</span><b>{isUser ? "你" : agentName}</b><time>{formatTime(message.createdAt)}</time></header><div className="markdown"><Markdown content={message.content} /></div></article>;
}

function ErrorCard({ item }: { item: TimelineItem & { kind: "error" } }) {
  return <article className="error-card"><header><span className="error-card-icon">!</span><div><b>{item.title}</b><time>{formatTime(item.createdAt)}</time></div></header><p>{item.detail}</p></article>;
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
  const cappedDepth = Math.min(depth, 6);
  return <div className="agent-tree-branch"><button className={`agent-tree-row ${agent.id === selectedID ? "selected" : ""}`} style={{ paddingLeft: `${12 + cappedDepth * 16}px` }} onClick={() => select(agent.id)}><span className={`agent-status ${agent.status}`}></span><span><b>{agent.summary}</b><small>{agentStatusLabel(agent.status)} · {agent.logs.length} 条记录</small></span></button>{agent.children.map((child) => <AgentTree key={child.id} agent={child} selectedID={selectedID} select={select} depth={depth + 1} />)}</div>;
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


function SSHConnectionManager({ close, fail }: { close: () => void; fail: (message: string) => void }) {
  const [connections, setConnections] = useState<any[]>([]);
	const [profiles, setProfiles] = useState<string[]>([]);
  const [loading, setLoading] = useState(true);
  const [showForm, setShowForm] = useState(false);
  const [testing, setTesting] = useState("");
  const [connecting, setConnecting] = useState("");
	const [form, setForm] = useState({ name: "", host: "", port: 22, user: "", privateKeyPath: "", rootPath: "", profileName: "" });
	const [preflight, setPreflight] = useState<{ ok: boolean; claudeReady?: boolean; hostKey?: string; fingerprint?: string; checks?: Record<string, boolean>; error?: string } | null>(null);
	const [preflighting, setPreflighting] = useState(false);
	const [hostKeyConfirmed, setHostKeyConfirmed] = useState(false);
  const [saving, setSaving] = useState(false);

  const loadConnections = useCallback(async () => {
    try { const list = await api<any[]>("/api/ssh-connections"); setConnections(list); } catch { /* ignore */ }
    setLoading(false);
  }, []);

  useEffect(() => { void loadConnections(); }, [loadConnections]);
	useEffect(() => { void api<string[]>("/api/ssh-profiles").then(setProfiles).catch(() => setProfiles([])); }, []);
	const updateForm = (next: typeof form) => { setForm(next); setPreflight(null); setHostKeyConfirmed(false); };
	const runPreflight = async () => {
		setPreflighting(true); setPreflight(null); setHostKeyConfirmed(false);
		try { setPreflight(await api("/api/ssh-connections/preflight", { method: "POST", body: JSON.stringify(form) })); }
		catch (cause) { fail(cause instanceof Error ? cause.message : "连接预检失败"); }
		finally { setPreflighting(false); }
	};

  const handleTest = async (id: string) => {
    setTesting(id);
    try {
      const result = await api<{ ok: boolean; error?: string }>(`/api/ssh-connections/${id}/test`, { method: "POST" });
      if (result.ok) alert("连接成功！");
      else alert("连接失败：" + (result.error || "未知错误"));
    } catch (cause) { fail(cause instanceof Error ? cause.message : "测试连接失败"); }
    finally { setTesting(""); void loadConnections(); }
  };

  const handleConnect = async (id: string) => {
    setConnecting(id);
    try { await api(`/api/ssh-connections/${id}/connect`, { method: "POST" }); }
    catch (cause) { fail(cause instanceof Error ? cause.message : "连接失败"); }
    finally { setConnecting(""); void loadConnections(); }
  };

  const handleDisconnect = async (id: string) => {
    setConnecting(id);
    try { await api(`/api/ssh-connections/${id}/disconnect`, { method: "POST" }); }
    catch (cause) { fail(cause instanceof Error ? cause.message : "断开失败"); }
    finally { setConnecting(""); void loadConnections(); }
  };

  const handleDelete = async (id: string) => {
    if (!confirm("确定要删除此 SSH 连接配置吗？如有关联项目则无法删除。")) return;
    try { await api(`/api/ssh-connections/${id}`, { method: "DELETE" }); void loadConnections(); }
    catch (cause) { fail(cause instanceof Error ? cause.message : "删除失败"); }
  };

  const handleSave = async () => {
		if (!preflight?.ok || !preflight.hostKey || !hostKeyConfirmed) { fail("请先完成全部预检并确认主机指纹"); return; }
    setSaving(true);
    try {
      await api("/api/ssh-connections", { method: "POST", body: JSON.stringify({ ...form, hostKey: preflight.hostKey, connect: true }) });
      setShowForm(false); updateForm({ name: "", host: "", port: 22, user: "", privateKeyPath: "", rootPath: "", profileName: "" }); void loadConnections();
    } catch (cause) { fail(cause instanceof Error ? cause.message : "保存失败"); }
    finally { setSaving(false); }
  };

  const statusIcon = (s: string) => s === "connected" ? "🟢" : s === "error" ? "🔴" : s === "disconnected" ? "⚫" : "⚪";

  return <div className="backdrop" role="dialog" aria-modal="true">
    <section className="modal">
      <header><div><label>SSH CONNECTIONS</label><h2>SSH 连接管理</h2></div><button title="关闭" onClick={close}>x</button></header>
      {showForm && <div className="ssh-form">
			<label>SSH Profile<select value={form.profileName} onChange={(e) => updateForm({ ...form, profileName: e.target.value })}><option value="">手工填写</option>{profiles.map((profile) => <option key={profile} value={profile}>{profile}</option>)}</select></label>
			<p className="permission-confirmation">选择 Profile 时，系统会读取本机 OpenSSH 配置并在预检中解析连接参数。</p>
        <div className="ssh-fields">
          <label>连接名称<input type="text" value={form.name} onChange={(e) => updateForm({ ...form, name: e.target.value })} placeholder="开发服务器" /></label>
          <label>主机地址<input disabled={Boolean(form.profileName)} type="text" value={form.host} onChange={(e) => updateForm({ ...form, host: e.target.value })} placeholder="192.168.1.100" /></label>
          <label>端口<input disabled={Boolean(form.profileName)} type="number" value={form.port} onChange={(e) => updateForm({ ...form, port: Number(e.target.value) || 22 })} /></label>
          <label>用户名<input disabled={Boolean(form.profileName)} type="text" value={form.user} onChange={(e) => updateForm({ ...form, user: e.target.value })} placeholder="root" /></label>
          <label className="ssh-field-wide">私钥路径<input disabled={Boolean(form.profileName)} type="text" value={form.privateKeyPath} onChange={(e) => updateForm({ ...form, privateKeyPath: e.target.value })} placeholder="例如 /home/user/.ssh/id_rsa" /><small>留空时使用 SSH Agent。</small></label>
          <label className="ssh-field-wide">根路径<input required type="text" value={form.rootPath} onChange={(e) => updateForm({ ...form, rootPath: e.target.value })} placeholder="/home/user/projects" /></label>
        </div>
			<div className="ssh-form-actions"><button className="secondary" disabled={preflighting} onClick={() => void runPreflight()}>{preflighting ? "预检中" : "预检连接"}</button><button className="secondary" onClick={() => setShowForm(false)}>取消</button><button className="primary" disabled={saving || !preflight?.ok || !hostKeyConfirmed} onClick={handleSave}>{saving ? "保存中" : "确认并保存"}</button></div>
			{preflight && <div className={`ssh-preflight ${preflight.ok ? "valid" : "invalid"}`}><b>{preflight.ok ? "连接预检通过" : "预检未通过"}</b><span>SSH {preflight.checks?.ssh ? "可用" : "不可用"} · SFTP {preflight.checks?.sftp ? "可用" : "不可用"} · Claude {preflight.checks?.claude ? "可用" : "待安装或登录"} · 审批隧道 {preflight.checks?.approvalTunnel ? "可用" : "不可用"}</span>{preflight.fingerprint && <label><input type="checkbox" checked={hostKeyConfirmed} onChange={(e) => setHostKeyConfirmed(e.target.checked)} />我已核对并确认此主机指纹：<code>{preflight.fingerprint}</code></label>}{preflight.error && <span>{preflight.error}</span>}</div>}
      </div>}
      {loading ? <p className="permission-confirmation">加载中...</p> : connections.length === 0 ? <p className="permission-confirmation">暂无 SSH 连接配置。<br /><button className="primary" onClick={() => setShowForm(true)} style={{ marginTop: "0.5rem" }}>添加 SSH 连接</button></p> : <>
        {connections.map((conn) => (
          <div key={conn.id} style={{ padding: "0.75rem", marginBottom: "0.5rem", background: "var(--surface)", borderRadius: "8px", display: "flex", alignItems: "center", justifyContent: "space-between" }}>
            <div><b>{statusIcon(conn.status)} {conn.name}</b><small style={{ display: "block", color: "var(--muted)" }}>{conn.user}@{conn.host}:{conn.port}</small>{conn.errorMsg && <small style={{ display: "block", color: "var(--danger)" }}>{conn.errorMsg}</small>}</div>
            <div style={{ display: "flex", gap: "0.25rem", flexShrink: 0 }}>
              <button className="secondary" disabled={testing === conn.id} onClick={() => handleTest(conn.id)}>{testing === conn.id ? "测试中" : "测试"}</button>
              {conn.status === "connected" ? <button className="secondary" disabled={connecting === conn.id} onClick={() => handleDisconnect(conn.id)}>断开</button> : <button className="secondary" disabled={connecting === conn.id} onClick={() => handleConnect(conn.id)}>{connecting === conn.id ? "连接中" : "连接"}</button>}
              <button className="secondary danger" onClick={() => handleDelete(conn.id)}>删除</button>
            </div>
          </div>))}
        <button className="primary" onClick={() => setShowForm(true)} style={{ marginTop: "0.5rem" }}>+ 添加 SSH 连接</button>
      </>}
      <footer><button className="secondary" onClick={close}>关闭</button></footer>
    </section>
  </div>;
}
