// 通用工具函数 — 从 App.tsx 提取

export function contentToText(value: unknown): string {
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

export function formatTime(value: string): string {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? "" : date.toLocaleTimeString("zh-CN", { hour: "2-digit", minute: "2-digit" });
}

export function formatHistoryTime(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "";
  return date.toLocaleString("zh-CN", { month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit" });
}

export function formatTokens(value: number): string {
  if (!Number.isFinite(value) || value <= 0) return "0";
  if (value >= 1_000_000) return `${(value / 1_000_000).toFixed(value >= 10_000_000 ? 0 : 1)}m`;
  if (value >= 1_000) return `${(value / 1_000).toFixed(value >= 10_000 ? 0 : 1)}k`;
  return String(value);
}

export function formatDuration(value: number): string {
  if (!Number.isFinite(value) || value < 0) return "--";
  const seconds = Math.floor(value / 1000);
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  return minutes < 60 ? `${minutes}m ${seconds % 60}s` : `${Math.floor(minutes / 60)}h ${minutes % 60}m`;
}

export function formatCost(value: number): string {
  return Number.isFinite(value) && value > 0 ? `$${value.toFixed(4)}` : "$0.0000";
}

export function runDuration(run: import("./types").RunUsage | undefined, now: number): number {
  if (!run) return 0;
  if (run.durationMs > 0) return run.durationMs;
  const startedAt = run.startedAt ? new Date(run.startedAt).getTime() : Number.NaN;
  return Number.isNaN(startedAt) ? 0 : Math.max(0, now - startedAt);
}

export function contextLabel(context: import("./types").RunUsage | undefined): string {
  if (context?.available === false) return "上下文暂不可用";
  const tokens = context?.contextInputTokens;
  if (!tokens) return context?.hasResult ? "上下文暂不可用" : "上下文计算中";
  if (!context?.contextWindow) return "上下文暂不可用";
  const percent = Math.round(tokens / context.contextWindow * 100);
  const tokensDisplay = formatTokens(tokens);
  const windowDisplay = formatTokens(context.contextWindow);
  return `上下文 ${tokensDisplay} / ${windowDisplay} (${percent}%)`;
}

export function contextLevel(context: import("./types").RunUsage | undefined): string {
  const tokens = context?.contextInputTokens;
  if (!tokens || !context?.contextWindow) return "pending";
  const percent = tokens / context.contextWindow * 100;
  return percent >= 85 ? "danger" : percent >= 70 ? "warning" : "normal";
}

export function requiredShortcutVariables(template: string): ("selection" | "error")[] {
  return (["selection", "error"] as const).filter((key) => template.includes(`\${${key}}`));
}

export function isNarrowConversationLayout(): boolean {
  return window.matchMedia("(max-width: 820px)").matches;
}

export function websocketAssistantMessageID(eventID: string, index: number): string {
  return `${eventID}:assistant-text:${index}`;
}

export function isTemporaryMessage(item: import("./types").Message): boolean {
  return item.id.includes(":assistant-text:");
}

export function mergeConversationItems<T extends { id: string; createdAt: string }>(history: T[], live: T[]): T[] {
  const items = new Map<string, T>();
  for (const item of history) items.set(item.id, item);
  for (const item of live) items.set(item.id, item);
  return [...items.values()].sort((left, right) => new Date(left.createdAt).getTime() - new Date(right.createdAt).getTime());
}

export function mergeReloadMessages(dbMessages: import("./types").Message[], currentMessages: import("./types").Message[]): import("./types").Message[] {
  const canonicalIndex = new Map<string, import("./types").Message>();
  for (const msg of dbMessages) {
    if (msg.role !== "assistant") continue;
    const key = msg.runId + "::" + (msg.parentToolUseId || "") + "::" + msg.content;
    canonicalIndex.set(key, msg);
  }

  const filtered = currentMessages.filter((item) => {
    if (!isTemporaryMessage(item)) return true;
    const key = (item.runId || "") + "::" + (item.parentToolUseId || "") + "::" + item.content;
    return !canonicalIndex.has(key);
  });

  return mergeConversationItems([...filtered, ...dbMessages], []);
}

export function agentStatusLabel(status: import("./types").AgentStatus): string {
  return ({ pending: "等待开始", running: "执行中", completed: "已完成", failed: "执行失败", stopped: "已停止", unresolved: "结果未收齐" })[status];
}

export function agentSummary(input: Record<string, any>): string {
  const value = input.description || input.prompt || input.task || input.agent_type || input.subagent_type;
  return typeof value === "string" && value.trim() ? value.trim().replace(/\s+/g, " ") : "执行子任务";
}

export function toolSummary(part: Record<string, any>): string {
  const input = typeof part.input === "object" && part.input ? part.input as Record<string, any> : {};
  const value = input.command || input.description || input.path || input.prompt || input.query;
  return typeof value === "string" && value.trim() ? value.trim() : "";
}