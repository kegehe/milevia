// Timeline 构建逻辑 — 从 App.tsx 提取

import type { Event, Message, ToolAction, ToolOutput, AgentExecution, AgentNode, AgentLog, TimelineItem, Approval, ApprovalEvent, SystemItem, SystemVariant } from "./types";
import { asRecord } from "./api";
import { contentToText, agentSummary, toolSummary, formatTokens, formatDuration } from "./utils";

const ignoredCodexStderr = new Set(["Reading additional input from stdin..."]);
const allowedTechnicalTerms = /\b(?:AI|API|CLI|Codex|Claude|Git|HTTP|ID|JSON|SSH|URL|WSL)\b/g;

function isIgnoredCLIStderr(message: string): boolean {
  return ignoredCodexStderr.has(message.trim());
}

// Codex wraps shell commands as `/usr/bin/zsh -lc 'actual command'` (or sh).
// Strip the wrapper so the tool card shows the real command the model ran.
function unwrapCodexShell(command: string): string {
  const match = command.match(/^(?:\/[^\s'"]+\/)?(?:zsh|sh|bash)\s+-lc\s+['"](.*)['"]$/s);
  if (match) return match[1];
  return command;
}

// shortenPath reduces an absolute path to its last two segments for compact
// display in tool card descriptions.
function shortenPath(path: string): string {
  const segments = path.split("/").filter(Boolean);
  if (segments.length <= 2) return segments.join("/") || path;
  return segments.slice(-2).join("/");
}

function firstText(...values: unknown[]): string {
  for (const value of values) {
    if (typeof value === "string" && value.trim()) return value.trim();
  }
  return "";
}

// isInternalError detects Go runtime errors that would leak implementation
// details to the UI \u2014 stack traces, panics, and file/line references.
function isInternalError(message: string): boolean {
  return message.includes(".go:") || message.includes("panic:") || message.includes("goroutine");
}

function localizedErrorDetail(value: unknown, fallback: string): string {
  const detail = typeof value === "string" ? value.trim() : "";
  if (!detail) return fallback;
  // Pure Chinese without untranslated English \u2014 display directly.
  if (/[\u4e00-\u9fff]/.test(detail)) {
    const withoutTechnicalTerms = detail.replace(allowedTechnicalTerms, "");
    if (!/[A-Za-z]{3,}/.test(withoutTechnicalTerms)) return detail;
  }
  // Internal Go errors (stack traces, panics) \u2014 keep the fallback only.
  if (isInternalError(detail)) return fallback;
  // English or mixed-language errors \u2014 keep the fallback as a prefix, then
  // append the original error so users can see the real cause.
  return fallback + "\uff1a" + detail;
}

// Codex emits top-level failures as JSONL events. Depending on the failure
// phase, its diagnostic is a string or a nested error object.
function eventErrorDetail(payload: Record<string, any>): string {
  const nestedError = asRecord(payload.error);
  return localizedErrorDetail(firstText(
    payload.message,
    payload.detail,
    payload.reason,
    typeof payload.error === "string" ? payload.error : "",
    nestedError.message,
    nestedError.detail,
    nestedError.reason,
    nestedError.code,
  ), "任务执行失败，请查看任务日志后重试。");
}

function isGenericCodexExit(detail: string): boolean {
  return /^Codex exited: exit status \d+\.?$/.test(detail.trim());
}

export function getApproval(event: Event): Approval | null {
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

export function buildTimeline(messages: Message[], events: Event[]): TimelineItem[] {
  const results = new Map<string, ToolOutput>();
  const runStatuses = new Map<string, string>();
  const approvalStates = new Map<string, ApprovalEvent>();
  for (const event of events) {
    const payload = asRecord(event.payload);
    if (event.type === "user") {
      for (const part of asRecord(payload.message).content || []) {
        if (part?.type === "tool_result" && part.tool_use_id) {
          const isError = Boolean(part.is_error);
          const content = contentToText(part.content);
          results.set(String(part.tool_use_id), { content: isError ? localizedErrorDetail(content, "工具执行失败，请查看任务日志后重试。") : content, isError });
        }
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
  const codexTools = new Map<string, ToolAction>();
  for (const event of events) {
    if (event.type !== "item.started" && event.type !== "item.completed") continue;
    const item = asRecord(asRecord(event.payload).item);
    const itemType = String(item.type || "");
    if (itemType !== "command_execution" && itemType !== "file_change") continue;
    const id = String(item.id || event.id);
    const key = `codex:${id}`;
    const changes = Array.isArray(item.changes) ? item.changes.map(asRecord) : [];
    const changeLabel = (kind: string): string => {
      switch (String(kind)) {
        case "add": return "新增";
        case "modify": return "修改";
        case "delete": return "删除";
        default: return "修改";
      }
    };
    const changeSummary = changes.map((change) => `${changeLabel(String(change.kind || ""))} ${shortenPath(String(change.path || "文件"))}`).join("\n");
    const changeDescription = changes.length > 0 ? changeSummary.replace(/\n/g, "、") : "修改项目文件";
    const input = itemType === "command_execution"
      ? { command: unwrapCodexShell(String(item.command || "")), description: "执行命令" }
      : { description: changeDescription, changes };
    const existing = codexTools.get(key);
    const changeDiffs = changes.map((change) => {
      const diff = typeof change.diff === "string" ? change.diff.trim() : "";
      const header = `${changeLabel(String(change.kind || ""))} ${shortenPath(String(change.path || "文件"))}`;
      return diff ? `${header}\n${diff}` : header;
    }).join("\n\n");
    const outputText = itemType === "command_execution" ? String(item.aggregated_output || item.output || "") : changeDiffs || changeSummary;
    const failed = Number(item.exit_code ?? item.exitCode ?? 0) !== 0 || item.status === "failed";
    codexTools.set(key, {
      id: key,
      runId: event.runId,
      name: itemType === "command_execution" ? "终端命令" : "文件修改",
      input,
      createdAt: existing?.createdAt || event.createdAt,
      output: event.type === "item.completed" ? { content: failed ? localizedErrorDetail(outputText, "命令执行失败，请查看任务日志后重试。") : outputText || "已完成", isError: failed } : existing?.output,
      runStatus: runStatuses.get(event.runId),
    });
  }
  tools.push(...codexTools.values());

  // ---- system 事件 → system timeline items ----
  // Claude CLI 的斜杠命令（如 /compact）和后台任务会产生 system 类型事件，
  // 这些事件携带重要的会话状态信息（压缩进度、token 统计、API 重试、后台任务等），
  // 需要解析并展示在时间线上。
  const systemItems: SystemItem[] = [];
  for (const event of events) {
    // tool_progress 是长时间运行工具的心跳事件（每 ~31s 一次），
    // 与 ToolCard 的"执行中"状态指示器冗余，且多次心跳会产生噪音卡片，故不展示。
    if (event.type === "tool_progress") continue;
    if (event.type !== "system") continue;
    const payload = asRecord(event.payload);
    const subtype = String(payload.subtype || "");
    if (subtype === "status") {
      const status = String(payload.status || "");
      const compactResult = String(payload.compact_result || "");
      if (status === "compacting") {
        systemItems.push({
          id: event.id, createdAt: event.createdAt, runId: event.runId,
          variant: "compact", title: "正在压缩上下文",
          detail: "Claude 正在压缩对话上下文以释放 token 空间…",
        });
      } else if (compactResult === "success") {
        systemItems.push({
          id: event.id, createdAt: event.createdAt, runId: event.runId,
          variant: "compact_result", title: "上下文压缩完成",
        });
      } else if (compactResult && compactResult !== "success") {
        systemItems.push({
          id: event.id, createdAt: event.createdAt, runId: event.runId,
          variant: "compact_result", title: "上下文压缩失败",
          detail: `压缩结果：${compactResult}`,
        });
      }
      continue;
    }
    if (subtype === "compact_boundary") {
      const meta = asRecord(payload.compact_metadata);
      const preTokens = Number(meta.pre_tokens) || 0;
      const postTokens = Number(meta.post_tokens) || 0;
      const dropped = Number(meta.cumulative_dropped_tokens) || 0;
      const durationMs = Number(meta.duration_ms) || 0;
      const parts: string[] = [];
      if (preTokens && postTokens) parts.push(`${formatTokens(preTokens)} → ${formatTokens(postTokens)}`);
      if (dropped) parts.push(`减少 ${formatTokens(dropped)}`);
      if (durationMs) parts.push(`耗时 ${formatDuration(durationMs)}`);
      systemItems.push({
        id: event.id, createdAt: event.createdAt, runId: event.runId,
        variant: "compact_boundary", title: "上下文压缩摘要",
        detail: parts.join(" · ") || undefined,
        metadata: meta as Record<string, unknown>,
      });
      continue;
    }
    if (subtype === "api_retry") {
      const attempt = Number(payload.attempt) || 0;
      const maxRetries = Number(payload.max_retries) || 0;
      const errorStatus = Number(payload.error_status) || 0;
      // 常见错误码/标识本地化，其余原样展示。
      const errorLabel = (() => {
        const raw = String(payload.error || "").trim();
        if (!raw) return "";
        const known: Record<string, string> = { rate_limit: "速率限制", overloaded: "服务过载", server_error: "服务器错误", timeout: "请求超时" };
        return known[raw] || raw;
      })();
      const errorPart = errorLabel ? `：${errorLabel}${errorStatus ? ` (${errorStatus})` : ""}` : errorStatus ? ` (${errorStatus})` : "";
      systemItems.push({
        id: event.id, createdAt: event.createdAt, runId: event.runId,
        variant: "api_retry", title: "API 重试中",
        detail: attempt && maxRetries ? `第 ${attempt}/${maxRetries} 次重试${errorPart}` : undefined,
      });
      continue;
    }
    if (subtype === "task_started" || subtype === "task_notification" || subtype === "task_updated" || subtype === "background_tasks_changed") {
      let title = "后台任务";
      let detail = "";
      if (subtype === "task_started") {
        title = "后台任务启动";
        detail = String(payload.description || payload.summary || "").trim();
      } else if (subtype === "task_notification") {
        const taskStatus = String(payload.status || "");
        // CLI 有时用英文模板生成 summary（如 Background command "..." completed (exit code 0)），
        // 尝试提取引号内的描述并本地化；若不匹配则原样展示。
        const rawSummary = String(payload.summary || "").trim();
        const bgMatch = rawSummary.match(/^Background command "(.+)" (completed|failed) \(exit code (\d+)\)$/);
        if (taskStatus === "failed") { title = "后台任务失败"; }
        else if (taskStatus === "completed") { title = "后台任务完成"; }
        else { title = "后台任务通知"; }
        detail = bgMatch
          ? `${bgMatch[2] === "failed" ? "失败" : "完成"}（退出码 ${bgMatch[3]}）${bgMatch[1]}`
          : rawSummary;
      } else if (subtype === "task_updated") {
        const patch = asRecord(payload.patch);
        if (patch.is_backgrounded) { title = "任务转入后台"; }
        else continue; // 无意义的状态更新，跳过
      } else {
        // background_tasks_changed
        const tasks = Array.isArray(payload.tasks) ? payload.tasks.map(asRecord) : [];
        if (tasks.length === 0) { title = "后台任务已全部完成"; }
        else {
          title = "后台任务列表更新";
          detail = tasks.map((t) => String(t.description || t.task_type || "")).filter(Boolean).join("、");
        }
      }
      systemItems.push({
        id: event.id, createdAt: event.createdAt, runId: event.runId,
        variant: "task", title, detail: detail || undefined,
      });
      continue;
    }
    // 其它 system 子类型（init 等）不展示在时间线上
  }

  const errorItems: TimelineItem[] = [];
  const detailedRuns = new Set<string>();
  const seenDiagnostics = new Set<string>();
  const fallbackTerminalFailures: Array<{ event: Event; detail: string }> = [];
  const addDiagnostic = (event: Event, title: string, detail: string, detailed = true) => {
    const normalized = detail.trim();
    if (!normalized) return;
    const key = `${event.runId}:${normalized}`;
    if (seenDiagnostics.has(key)) return;
    seenDiagnostics.add(key);
    if (detailed) detailedRuns.add(event.runId);
    const payload = asRecord(event.payload);
    const taskId = typeof payload.taskId === "string" && payload.taskId ? payload.taskId : undefined;
    errorItems.push({ kind: "error", id: event.id, createdAt: event.createdAt, runId: event.runId, title, detail: normalized, taskId });
  };
  for (const event of events) {
    const payload = asRecord(event.payload);
    const rawDetail = firstText(
      payload.message,
      payload.detail,
      payload.reason,
      typeof payload.error === "string" ? payload.error : "",
      asRecord(payload.error).message,
      asRecord(payload.error).detail,
      asRecord(payload.error).reason,
      asRecord(payload.error).code,
    );
    const detail = localizedErrorDetail(rawDetail, "任务执行失败，请查看任务日志后重试。");
    if (event.type === "run.failed" || event.type === "run.interrupted") {
      if (detail && !isGenericCodexExit(rawDetail)) {
        addDiagnostic(event, event.type === "run.interrupted" ? "执行中断" : "执行失败", detail);
      } else if (detail) {
        fallbackTerminalFailures.push({ event, detail });
      }
    } else if (event.type === "error" || event.type === "turn.failed") {
      addDiagnostic(event, "执行错误", detail);
    } else if (event.type === "stream.error") {
      addDiagnostic(event, "流错误", detail);
    }
    if (event.type === "stderr" && typeof payload.message === "string" && payload.message.trim() && !isIgnoredCLIStderr(payload.message)) {
      addDiagnostic(event, "CLI 输出", localizedErrorDetail(payload.message, "工具输出异常，请查看任务日志后重试。"));
    }
  }
  // The process exit code is useful only when the CLI provided no actionable
  // diagnostic in JSONL or stderr for the same run.
  for (const { event, detail } of fallbackTerminalFailures) {
    if (!detailedRuns.has(event.runId)) addDiagnostic(event, "执行失败", detail, false);
  }
  const timeline: TimelineItem[] = [
    ...messages.map((message) => ({ kind: "message" as const, id: message.id, createdAt: message.createdAt, message })),
    ...tools.map((action) => ({ kind: "tool" as const, id: action.id, createdAt: action.createdAt, action })),
    ...systemItems.map((system) => ({ kind: "system" as const, id: system.id, createdAt: system.createdAt, system })),
    ...errorItems,
  ];
  return timeline.sort((left, right) => new Date(left.createdAt).getTime() - new Date(right.createdAt).getTime());
}

export function eventMessageParts(event: Event): Record<string, any>[] {
  const content = asRecord(asRecord(event.payload).message).content;
  return Array.isArray(content) ? content.map(asRecord) : [];
}

export function buildAgentExecutions(events: Event[]): AgentExecution[] {
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
        const isError = Boolean(part.is_error);
        const output = contentToText(part.content);
        appendLog(ownerID, { id: event.id + part.tool_use_id, createdAt: event.createdAt, kind: "result", title: isError ? "工具失败" : "工具结果", detail: isError ? localizedErrorDetail(output, "工具执行失败，请查看任务日志后重试。") : output, isError });
        const agent = nodes.get(part.tool_use_id);
        if (agent) agent.status = part.is_error ? "failed" : "completed";
      }
    }
    if (event.type === "stream.error") appendLog(parentID, { id: event.id, createdAt: event.createdAt, kind: "error", title: "流错误", detail: localizedErrorDetail(payload.error, "无法读取工具输出，请稍后重试。"), isError: true });
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

export function subagentTextIndex(events: Event[]): Map<string, number[]> {
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

export function isSubagentMessage(message: Message, indexedTexts: Map<string, number[]>): boolean {
  if (message.parentToolUseId) return true;
  if (message.runId) return false;
  if (message.role !== "assistant") return false;
  const messageTime = new Date(message.createdAt).getTime();
  return (indexedTexts.get(message.content) || []).some((eventTime) => Math.abs(eventTime - messageTime) <= 5_000);
}

export function flattenAgents(agents: AgentNode[]): AgentNode[] {
  return agents.flatMap((agent) => [agent, ...flattenAgents(agent.children)]);
}

export function timelineContentVersion(timeline: TimelineItem[], executions: AgentExecution[]): string {
  const timelineVersion = timeline.map((item) => {
    if (item.kind === "message") return `message:${item.id}:${item.message.content.length}`;
    if (item.kind === "tool") {
      return `tool:${item.id}:${item.action.output?.content.length || 0}:${item.action.approval?.status || ""}:${item.action.runStatus || ""}`;
    }
    if (item.kind === "system") return `system:${item.id}:${item.system.title}:${item.system.detail?.length || 0}`;
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
