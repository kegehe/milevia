import { FormEvent, useCallback, useEffect, useMemo, useRef, useState } from "react";
import QRCode from "qrcode";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { api, asRecord } from "../lib/api";
import { isDesktop } from "../lib/runtime";
import { systemItemFromEvent, eventDiagnostic, cliOutputDiagnostic, getApproval } from "../lib/timeline";
import type { SystemVariant } from "../lib/types";
import { markdownCodeComponents } from "../components/MarkdownCodeBlock";
import { priorityLabels, statusLabels, type Priority } from "../features/tasks/task-model";
import { useNavigate } from "react-router-dom";
import { Capacitor } from "@capacitor/core";
import { App as CapacitorApp } from "@capacitor/app";
import type { PluginListenerHandle } from "@capacitor/core";
import { BarcodeFormat, BarcodeScanner, LensFacing } from "@capacitor-mlkit/barcode-scanning";
import { invoke } from "@tauri-apps/api/core";
import { useDocumentVisible } from "../lib/useDocumentVisible";
import { checkMobileUpdate, type MobileUpdateState } from "../features/updater/mobile-update";
// markdown 渲染的样式依赖必须由本页面自己声明：真机上 main.tsx 全局引入过它，
// 所以"少这一行"在真机看不出来 —— 但任何绕过 main.tsx 的入口（预览夹具、单页测试）
// 都会渲染出裸的 pre / blockquote / 复制按钮。页面自包含更稳。
import "../markdown.css";
import "./mobile-remote.css";

type Instance = { instanceId: string; name: string; status: string; lastAgentSequence: number; lastSeenAt?: string };
type Task = { id: string; title: string; description?: string; priority: string; status: string; updatedAt: string };
type Message = { id: string; runId?: string; role: "user" | "assistant"; content: string; createdAt: string };
// Notice 是"运行过程"而非"对话内容"：API 重试、上下文压缩、后台任务、执行失败。
// 手机端原先只转发 user/assistant 消息，这类事件在到达时被直接丢弃，于是电脑上
// 已经显示"重试中 / 正在压缩上下文 / 执行失败"，手机端却像卡住了一样什么都没有。
type RemoteNotice = {
  id: string;
  runId: string;
  createdAt: string;
  variant: SystemVariant | "error";
  title: string;
  detail?: string;
  // task 卡片按成功/失败/中性着色，与桌面端 system-card 的 state 后缀同源。
  state?: string;
  // 同 id 再次到达时怎么处理：
  //   "append"  —— 逐行到达的内容（CLI 输出），新行并进同一条；
  //   "replace" —— 状态推进（工具确认 pending → allow/deny），用最新状态覆盖。
  // 缺省 = 同 id 视为重复，保留先到的那条。
  merge?: "append" | "replace";
};
type RemoteConversation = { id: string; title: string; status: string; agentId: string; lastActivityAt: string; isCurrent: boolean; messages: Message[]; notices?: RemoteNotice[] };
// 电脑端数据库里的「常用提示词 / 常用命令」条目。字段与 control-server 的 Shortcut 一一对应
// （只少了手机端用不到的 createdAt/updatedAt）。
type RemoteShortcut = { id: string; name: string; description: string; kind: string; template: string; scope: string; defaultAction: string; groupName?: string; pinned: boolean; enabled: boolean; sortOrder: number; projectIds: string[] };
// 电脑端（或 SSH 远端）文件系统上扫出来的技能。source 决定分组标题（用户 / 项目 / 系统插件）。
type RemoteSkill = { name: string; description: string; agent: string; env: string; source: string };
type ProjectEnvironment = "windows" | "wsl" | "remote-linux";
type Project = { id: string; name: string; runner: string; environment?: ProjectEnvironment; running?: boolean; gitBranch: string; tasks: Task[]; conversations: RemoteConversation[]; skills?: RemoteSkill[] };
type Snapshot = { snapshotRevision: number; observedAt: string; projects: Project[]; shortcuts?: RemoteShortcut[] };
type CommandState = { commandId: string; status: string; result?: unknown };
type AcceptedCommand = { commandId: string; status: string };
// 一次快照拉取的真实结果。"updated/unchanged" 都算成功，区别只在版本号有没有前进；
// "skipped" 表示这次请求没有真正发出去（没有选中的实例 / 已被更新的请求取代）。
// 手动刷新必须拿到这个结果才能给出"已刷新 / 已是最新 / 刷新失败"的可信反馈——只靠
// setSnapshot 是看不出有没有生效的。
type SnapshotFetchOutcome = "updated" | "unchanged" | "failed" | "skipped";
type SnapshotFetchResult = { outcome: SnapshotFetchOutcome; snapshot: Snapshot | null; reason?: string };
// 实例列表请求的结果。superseded 必须与 failed 分开：列表每 5 秒被轮询拉一次，
// 手动刷新那一次被轮询抢答是常态而不是故障（见 loadInstances 注释）。
type InstanceFetchResult = { instances: Instance[]; outcome: "ok" | "failed" | "superseded"; reason?: string };
// 刷新按钮的结果条：refreshing 与其它状态互斥，at 为完成时刻（refreshing 时为空）。
type RefreshStatus = { state: "refreshing" | "success" | "unchanged" | "failed"; message: string; at: Date | null };
type PendingMessage = { requestId: string; content: string; createdAt: string };
type ProcessingConversation = { count: number; startedAt: number; snapshotRevision: number };
type ProcessingConversations = Record<string, ProcessingConversation>;

const terminalCommandStatuses = ["completed", "failed", "expired", "cancelled", "indeterminate"];

function commandStatusRank(status: string): number {
  if (status === "queued" || status === "pending") return 0;
  if (status === "received" || status === "executing") return 1;
  return terminalCommandStatuses.includes(status) ? 2 : 1;
}

function mergeCommandState(current: CommandState | null, next: CommandState): CommandState {
  if (!current || current.commandId !== next.commandId) return next;
  // Poll responses can arrive out of order. A terminal state must never be
  // replaced by an older queued/executing response.
  if (commandStatusRank(next.status) < commandStatusRank(current.status)) return current;
  if (terminalCommandStatuses.includes(current.status) && !terminalCommandStatuses.includes(next.status)) return current;
  return next;
}

// 见下方 taskPriorityLabel：规范状态一律取桌面看板那份 statusLabels，这里只补服务端可能出现的
// 非规范状态。曾经手机端把 action_required 写成"需要操作"，而同一屏的分类胶囊、桌面看板与
// insights 都叫"需处理"——同一个状态在一屏里两个名字，就是没共用一份枚举的代价。
function taskStatusLabel(status: string): string {
  const labels: Record<string, string> = {
    ...statusLabels,
    queued: "排队中",
    completed: "已完成",
    failed: "失败",
    blocked: "已阻塞",
  };
  return labels[status] || status || "未知状态";
}

function taskStatusClass(status: string): string {
  return `status-${status.replace(/[^a-z0-9_-]/gi, "-") || "unknown"}`;
}

// 优先级文案复用桌面看板那份（task-model.ts 的 priorityLabels），不在手机端另写一份枚举。
function taskPriorityLabel(priority: string): string {
  return priorityLabels[priority as Priority] || priority || "普通";
}

// 分类胶囊的文案同样取 statusLabels：以前这两份是各写一遍的，谁改一处另一处就悄悄对不上。
const taskFilters = [
  { id: "all", label: "全部" },
  { id: "todo", label: statusLabels.todo },
  { id: "running", label: statusLabels.running },
  { id: "awaiting_review", label: statusLabels.awaiting_review },
  { id: "action_required", label: statusLabels.action_required },
  { id: "done", label: statusLabels.done },
  { id: "cancelled", label: statusLabels.cancelled },
] as const;

type TaskFilter = typeof taskFilters[number]["id"];

function taskSummary(task: Task): string {
  const title = task.title.trim();
  if (title) return title;
  const description = task.description?.trim() || "";
  return description || "暂无任务内容";
}

function conversationStatusLabel(status: string): string {
  const labels: Record<string, string> = { idle: "就绪", queued: "排队中", running: "执行中", completed: "已完成", failed: "失败", stopped: "已停止" };
  return labels[status] || status || "未知状态";
}

function conversationAgentLabel(agentId: string): string {
  return agentId === "codex" ? "Codex" : "Claude Code";
}

// 状态卡图标沿用桌面时间线那一套字形，两端看到的是同一个符号。
const noticeIcons: Record<RemoteNotice["variant"], string> = { compact: "◐", compact_result: "✓", compact_boundary: "≡", api_retry: "↻", task: "▸", error: "!" };

// 工具确认的只读文案。桌面端是工具卡上的 allow/deny 按钮，手机端不做交互，
// 只用一张卡说明"卡在哪了"；键是 approval 事件的类型后缀。
const approvalLabels: Record<string, { title: string; state: string }> = {
  pending: { title: "等待工具确认", state: "info" },
  allow: { title: "工具已确认", state: "success" },
  deny: { title: "工具已拒绝", state: "failed" },
  aborted: { title: "工具确认已中止", state: "info" },
  timeout: { title: "工具确认超时", state: "failed" },
};

// 一次会话只保留最近这一段运行记录：时间久了状态卡会越积越多，而用户翻历史时
// 关心的是刚刚发生了什么，不是三天前的每一次重试。
const maxConversationNotices = 24;

// 「＋」面板里的技能分组。来源键与桌面端 ConversationPage 的 skillSourceMeta 完全一致，
// 顺序也照抄（项目 > 用户 > 系统/官方），两端看到的技能分组不会长得不一样。
const skillSourceOrder = ["project", "user", "plugin"];
const skillSourceLabels: Record<string, string> = { project: "项目", user: "用户", plugin: "系统/官方" };

// 技能片段：与桌面端 mergeSkillPrompt 的文案**逐字一致**（见 docs/22）。技能本身只是文件系统上
// 的一个 SKILL.md，手机端不可能去读它，只能把"名称 + 描述"拼成一句可引用的提示词让 CLI 自己去
// 加载——所以这段文案必须和桌面端一字不差，否则同一个技能在两端会让 Agent 收到不同的指令。
function skillPrompt(skill: RemoteSkill): string {
  return skill.description && skill.description !== skill.name
    ? `请使用技能 <${skill.name}>：${skill.description}`
    : `请使用技能 <${skill.name}>`;
}

// 「用户写的正文」与「已引用的技能」拼成真正发出去的文本：引用在前、正文在后，中间空一行。
// 这里是"输入框里显示什么"与"实际发什么"之间唯一的分界线 —— 手机端只显示一颗胶囊，
// 电脑端 CLI 收到的仍是那句完整的自然语言引用（headless 通道不认 `/skill-name`，见 docs/22）。
// 与桌面端 ConversationPage 的 composeSkillMessage 必须**逐字一致**，否则同一个技能两端发出的指令不同。
function composeSkillMessage(text: string, refs: RemoteSkill[]): string {
  if (refs.length === 0) return text.trim();
  const body = refs.map(skillPrompt).join("\n");
  return text.trim() ? `${body}\n\n${text.trim()}` : body;
}

// 快捷方式在手机端该走哪条路。fill 只渲染、内容回填到手机输入框；run / confirm 直接在电脑端
// 执行（与桌面端点击同一条服务端路径）。
// 片段（snippet）永远是 fill：服务端的 run 接口明确拒绝片段（"snippets cannot run"）。
function shortcutAction(shortcut: RemoteShortcut): "fill" | "run" | "confirm" {
  if (shortcut.kind === "snippet") return "fill";
  return shortcut.defaultAction === "run" || shortcut.defaultAction === "confirm" ? shortcut.defaultAction : "fill";
}

// 命令结果里取渲染后的正文。形状与电脑端 /preview 的响应一致：{shortcut, content, requiresConfirmation}。
function shortcutContentFromCommandResult(result: unknown): string {
  if (!result || typeof result !== "object") return "";
  const content = (result as { content?: unknown }).content;
  return typeof content === "string" ? content : "";
}

function noticeTime(createdAt: string): string {
  const parsed = new Date(createdAt);
  if (Number.isNaN(parsed.getTime())) return "";
  // 只到分钟：卡片右侧的时间是辅助信息，秒在窄屏上只会把标题挤窄。
  return parsed.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}

// 消息时间：当天只给时刻（19:31），跨天才补日期（9/12 19:31）。
// 原来的 toLocaleString() 输出"2026/9/13 19:31:15"——年份和秒对读对话毫无帮助，
// 却占掉气泡顶部最显眼的一行。
function messageTime(createdAt: string): string {
  const parsed = new Date(createdAt);
  if (Number.isNaN(parsed.getTime())) return "";
  const now = new Date();
  const clock = parsed.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  if (parsed.toDateString() === now.toDateString()) return clock;
  const day = `${parsed.getMonth() + 1}/${parsed.getDate()}`;
  // 跨年还要带年份，否则"12/31 19:41"会被读成今年的。
  return `${parsed.getFullYear() === now.getFullYear() ? day : `${parsed.getFullYear()}/${day}`} ${clock}`;
}

// noticeFromEventFields 解析一条状态/诊断事件本身（不含会话归属），解析规则全部
// 来自桌面时间线（systemItemFromEvent / eventDiagnostic），手机端不维护第二套文案
// 枚举。解析不出内容的（CLI init 等）返回 null，由调用方丢弃。
function noticeFromEventFields(type: string, payload: unknown, id: string, runId: string, createdAt: string): RemoteNotice | null {
  const source = { id, type, payload: asRecord(payload), runId, createdAt };
  // CLI 输出：逐行到达，聚合成同一条卡（与桌面 buildTimeline 共用 cliOutputDiagnostic）。
  // 不聚合的话，一次编译失败能刷出几十张卡片，把重试/压缩那几张真要紧的挤下去。
  if (type === "stderr") {
    const message = typeof source.payload.message === "string" ? String(source.payload.message) : "";
    const output = cliOutputDiagnostic([message]);
    if (!output) return null; // 心跳行等噪声（过滤规则见 isIgnoredCLIStderr）
    return { id: `stderr:${runId || createdAt}`, runId, createdAt, variant: "error", title: output.title, detail: output.detail, merge: "append" };
  }
  // 工具确认：桌面把它做成工具卡上的按钮；手机端只读，但必须显示——运行卡在等确认时，
  // 用户至少要知道"为什么没动静"，而不是以为进程死了。
  const approval = getApproval(source);
  if (approval) {
    const command = typeof approval.toolInput.command === "string" ? approval.toolInput.command.replace(/\s+/g, " ").trim() : "";
    const detail = command ? `${approval.toolName}：${command.slice(0, 120)}${command.length > 120 ? "…" : ""}` : approval.toolName;
    // 状态取事件类型后缀而不是 getApproval 的 status：aborted/timeout 在那边被归成
    // "pending"，沿用会让已经中止的确认一直显示成"等待确认"（5 分钟无人操作就会超时，
    // 这条路径并不罕见）。
    const resolution = type.slice("approval.".length);
    const label = approvalLabels[resolution] || approvalLabels.pending;
    return {
      id: `approval:${approval.approvalId}`,
      runId,
      createdAt,
      variant: "task",
      title: label.title,
      detail: resolution === "pending" ? `${detail} · 请在电脑上确认` : detail,
      state: label.state,
      merge: "replace",
    };
  }
  const system = systemItemFromEvent(source);
  if (system) {
    const state = typeof system.metadata?.state === "string" ? system.metadata.state : "";
    return { id: system.id, runId: system.runId, createdAt: system.createdAt, variant: system.variant, title: system.title, detail: system.detail, state };
  }
  const diagnostic = eventDiagnostic(source);
  // deferFallback 指的是"只有退出码、没有原因"的那条（Codex exited: exit status N）。
  // 桌面端会等同一次运行有没有更详细的诊断再决定展示它，手机端没有那套归并，
  // 直接跳过，免得用一句没有信息量的话盖住真正的错误。
  if (diagnostic && !diagnostic.deferFallback) {
    return { id: source.id || `${type}:${createdAt}`, runId, createdAt, variant: "error", title: diagnostic.title, detail: diagnostic.detail };
  }
  return null;
}

// noticeFromRealtimeEvent 处理实时通道的事件：除了事件本身，还必须先拿到它属于
// 哪个会话。返回 null 的事件交给调用方按原逻辑处理（会话创建、流式增量、普通消息）。
function noticeFromRealtimeEvent(event: { eventId?: string; type?: string; taskRunId?: string; payload?: unknown; createdAt?: string }): { conversationId: string; notice: RemoteNotice } | null {
  const record = asRecord(event.payload);
  // 事件必须自带会话归属：状态事件由 CLI 产生，payload 里本来没有会话字段，
  // 是控制端在中继副本上补写的（见 remoteEventPayloadWithConversation）。拿不到
  // 就返回 null 让快照回放补上——宁可晚一步，也不把 A 会话的重试贴到 B 会话里。
  const conversationId = typeof record.conversationId === "string" ? record.conversationId : "";
  if (!conversationId) return null;
  const notice = noticeFromEventFields(
    typeof event.type === "string" ? event.type : "",
    record,
    typeof event.eventId === "string" ? event.eventId : "",
    typeof event.taskRunId === "string" ? event.taskRunId : "",
    typeof event.createdAt === "string" && event.createdAt ? event.createdAt : new Date().toISOString(),
  );
  return notice ? { conversationId, notice } : null;
}

// noticesFromSnapshot 解析快照回放的记录。快照里给的是事件原文（type + payload），
// 与实时通道同源但少一层信封，所以这里必须走同一个解析函数——否则会出现"实时
// 到达时是一张正常的状态卡，刷新之后变成一张没有类型、配色全丢的空卡"。
function noticesFromSnapshot(raw: unknown): RemoteNotice[] {
  if (!Array.isArray(raw)) return [];
  let items: RemoteNotice[] = [];
  for (const entry of raw) {
    const record = asRecord(entry);
    const notice = noticeFromEventFields(
      String(record.type || ""),
      record.payload,
      String(record.id || ""),
      String(record.runId || ""),
      String(record.createdAt || ""),
    );
    // 逐条并进：同一个 approvalId 的 pending → allow/deny、同一个 run 的多行输出，
    // 都要按各自的 merge 语义收成一条。
    if (notice) items = appendNotice(items, notice);
  }
  return items;
}

function appendNotice(notices: RemoteNotice[] | undefined, notice: RemoteNotice): RemoteNotice[] {
  const current = notices || [];
  const index = current.findIndex((item) => item.id === notice.id);
  if (index < 0) {
    const next = [...current, notice];
    return next.length > maxConversationNotices ? next.slice(next.length - maxConversationNotices) : next;
  }
  if (notice.merge === "replace") {
    const existing = current[index];
    // 快照有上传节流，可能带着比本地更旧的状态回来（工具确认已经在电脑上被允许，
    // 但快照里还只有 pending）。只有不更旧的才允许覆盖，否则界面会从"已确认"
    // 倒退回"等待确认"，用户以为又被卡住了。
    if ((Date.parse(notice.createdAt) || 0) < (Date.parse(existing.createdAt) || 0)) return current;
    return current.map((item, at) => (at === index ? notice : item));
  }
  if (notice.merge === "append" && notice.detail) {
    const existing = current[index];
    // 同一行可能因 SSE 重连被重复投递；按行去重，免得"CLI 输出"里同一句出现两遍。
    const lines = (existing.detail || "").split("\n");
    if (lines.includes(notice.detail)) return current;
    const detail = existing.detail ? `${existing.detail}\n${notice.detail}` : notice.detail;
    return current.map((item, at) => (at === index ? { ...existing, detail } : item));
  }
  return current;
}

// mergeNotices 取两份记录的并集。逐条并进而不是按 id 覆盖：CLI 输出要累积行、
// 工具确认要保留最新状态，两种语义只有 appendNotice 这层知道，覆盖式合并会把
// 已经收到的 stderr 行丢掉。
function mergeNotices(existing: RemoteNotice[], incoming: RemoteNotice[]): RemoteNotice[] {
  if (existing.length === 0) return incoming;
  if (incoming.length === 0) return existing;
  let merged = existing;
  for (const notice of incoming) merged = appendNotice(merged, notice);
  return merged.slice().sort((left, right) => (Date.parse(left.createdAt) || 0) - (Date.parse(right.createdAt) || 0));
}

function projectEnvironment(project: Project): ProjectEnvironment {
  if (project.environment === "windows" || project.environment === "wsl" || project.environment === "remote-linux") return project.environment;
  return project.runner.startsWith("ssh-") ? "remote-linux" : "wsl";
}

function projectEnvironmentLabel(environment: ProjectEnvironment): string {
  return environment === "windows" ? "Windows" : environment === "wsl" ? "WSL" : "SSH";
}

function ProjectEnvironmentIcon({ environment }: { environment: ProjectEnvironment }) {
  if (environment === "windows") return <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M3 5.5 10.5 4v7H3v-5.5ZM13 3.5 21 2v9h-8v-7.5ZM3 13h7.5v7L3 18.5V13ZM13 13h8v9l-8-1.5V13Z" /></svg>;
  if (environment === "wsl") return <svg viewBox="0 0 24 24" aria-hidden="true"><g fill="currentColor"><path fillRule="evenodd" d="M9 7.8h6c3.2 1.2 4.2 3.4 4 6.2-.2 2.8-1 5.2-3.4 6.6-1.2.8-6 .8-7.2 0C6 19.2 5.2 16.8 5 14c-.2-2.8.8-5 4-6.2Zm-.4 7.8a3.4 3.8 0 1 0 6.8 0 3.4 3.8 0 1 0-6.8 0ZM12 1.2a4.2 4.2 0 1 0 0 8.4 4.2 4.2 0 1 0 0-8.4Zm-1.8 3a1 1 0 1 0 0 2 1 1 0 1 0 0-2Zm3.6 0a1 1 0 1 0 0 2 1 1 0 1 0 0-2Zm-2.9 2.7h2.2L12 8.8Z" /><path d="M16.5 10.8c2.3 1 3.7 3.2 3.7 5.4 0 2.2-1.6 3.4-4 3.4-1 0-1.6-.6-1.8-1.2.4-2.2.8-5.4 2.1-7.6ZM7.5 10.8c-2.3 1-3.7 3.2-3.7 5.4 0 2.2 1.6 3.4 4 3.4 1 0 1.6-.6 1.8-1.2-.4-2.2-.8-5.4-2.1-7.6Z" /></g></svg>;
  return <svg viewBox="0 0 24 24" aria-hidden="true"><rect x="4" y="4" width="16" height="6" rx="1.2" /><rect x="4" y="14" width="16" height="6" rx="1.2" /><path d="M8 7h.01M8 17h.01M12 7h5M12 17h5" /></svg>;
}

function conversationIDFromCommandResult(result: unknown): string {
  if (!result || typeof result !== "object") return "";
  const value = result as { id?: unknown; conversation?: { id?: unknown } };
  if (typeof value.id === "string") return value.id;
  return typeof value.conversation?.id === "string" ? value.conversation.id : "";
}

function commandFailureDetail(result: unknown): string {
  if (!result || typeof result !== "object") return "";
  const error = (result as { error?: unknown }).error;
  return typeof error === "string" ? error.trim() : "";
}

function normalizeCommandState(value: unknown, fallbackCommandID = ""): CommandState {
  if (!value || typeof value !== "object") return { commandId: fallbackCommandID, status: "indeterminate" };
  const raw = value as { commandId?: unknown; status?: unknown; result?: unknown; command?: { commandId?: unknown } };
  return {
    commandId: typeof raw.commandId === "string" ? raw.commandId
      : typeof raw.command?.commandId === "string" ? raw.command.commandId
        : fallbackCommandID,
    status: typeof raw.status === "string" ? raw.status : "indeterminate",
    result: raw.result,
  };
}

function conversationFromCommandResult(result: unknown): RemoteConversation | null {
  if (!result || typeof result !== "object") return null;
  const raw = (result as { conversation?: unknown }).conversation;
  const value = raw && typeof raw === "object" ? raw : result;
  const item = value as Partial<RemoteConversation> & { id?: unknown };
  if (typeof item.id !== "string" || item.id.trim() === "") return null;
  return {
    id: item.id,
    title: typeof item.title === "string" ? item.title : "新会话",
    status: typeof item.status === "string" ? item.status : "idle",
    agentId: typeof item.agentId === "string" ? item.agentId : "",
    lastActivityAt: typeof item.lastActivityAt === "string" ? item.lastActivityAt : new Date().toISOString(),
    isCurrent: item.isCurrent !== false,
    messages: Array.isArray(item.messages) ? item.messages as Message[] : [],
  };
}

// Native builds do not have a proxy origin. Keep a production fallback so an
// APK built without a local .env can still reach the single public endpoint.
const configuredCloudURL = (import.meta.env.VITE_CLOUD_URL as string | undefined)?.replace(/\/$/, "") || "";
// In Vite development use the local /v1 proxy to avoid browser CORS. Native
// and production builds keep an absolute public URL.
const cloudURL = Capacitor.isNativePlatform()
  ? configuredCloudURL || "https://keyanjia.info:8443"
  : import.meta.env.DEV ? "" : configuredCloudURL;
const cloudRequestTimeoutMs = 15_000;

type BarcodeDetectorResult = { rawValue?: string };
type BarcodeDetectorLike = new (options?: { formats?: string[] }) => { detect(source: HTMLVideoElement): Promise<BarcodeDetectorResult[]> };

// 原生分支（`BarcodeScanner.startScan()`）把摄像头预览画在 **WebView 之下**，
// 所以"能不能看见画面"完全由 WebView 自身 + 它上层每一层 DOM 的不透明度决定。
// 这里必须连 <html> 一起清底：`style.css` 的 `:root { background: #f3f8f4 }` 给根元素
// 铺了一层不透明画布底色，只清 body / #root 压不住它 —— 症状就是"点了扫码，摄像头
// 其实已经启动，但整块画面被根元素底色挡住，看起来像没调用起来"。
// 两处挂载点必须同时改（<html> 与 <body>），少挂一处会让 `html.barcode-scanner-active`
// 那一组清底规则整组失效。
const barCodeScannerActiveClass = "barcode-scanner-active";
function setScannerActive(active: boolean) {
  document.documentElement.classList.toggle(barCodeScannerActiveClass, active);
  document.body.classList.toggle(barCodeScannerActiveClass, active);
}

function pairingFromScan(value: string): { pairingID: string; code: string } {
  try {
    const parsed = new URL(value);
    return {
      pairingID: parsed.searchParams.get("pairingId") || parsed.searchParams.get("pairing_id") || "",
      code: parsed.searchParams.get("code") || "",
    };
  } catch {
    return { pairingID: "", code: "" };
  }
}

// The QR code has to point at an absolute http(s) address that the phone can
// reach. The desktop WebView origin is a tauri:// URL, and a cloud deployment
// without MILEVIA_CLOUD_APP_URL returns a relative path — falling back to the
// local origin in either case would produce a code that no phone can open, so
// return an empty string and let the caller explain the problem instead.
function pairingURLWithCode(value: string | undefined, pairingID: string): string {
  const candidates = [(value || "").trim(), cloudURL ? `${cloudURL}/mobile` : ""];
  for (const candidate of candidates) {
    if (!candidate) continue;
    let parsed: URL;
    try {
      parsed = new URL(candidate);
    } catch {
      continue;
    }
    if (parsed.protocol !== "http:" && parsed.protocol !== "https:") continue;
    parsed.searchParams.set("pairingId", pairingID);
    // Keep the short-lived pairing code out of URLs, where it can leak through
    // browser history, proxy logs, or referrer data.
    parsed.searchParams.delete("code");
    return parsed.toString();
  }
  return "";
}

function idempotencyKey() {
  return globalThis.crypto?.randomUUID?.() || `${Date.now()}-${Math.random().toString(36).slice(2)}`;
}

// 云端的错误原因是这里最准确的信号，但它以英文短语返回，而且不能靠状态码推断
// 场景——409 既可能是配对码冲突，也可能是"电脑当前离线"。因此按原因做定向翻译，
// 认不出的原因原样透出，避免给出与实际场景不符的提示。
function localizeCloudError(raw: string, status: number): string {
  const text = raw.toLowerCase();
  if (text.includes("instance_offline")) return "电脑当前离线，请等电脑上线后再试";
  if (text.includes("pairing is not ready for confirmation")) return "手机还没有提交校验码，请先扫码并输入校验码";
  if (text.includes("too many pairing attempts")) return "配对尝试次数过多，请让电脑重新生成二维码";
  if (text.includes("pairing code is ambiguous")) return "校验码发生冲突，请让电脑重新生成二维码";
  if (text.includes("pairing code has expired or was already used")) return "配对码已过期或已被使用，请让电脑重新生成";
  if (text.includes("pairing code is invalid")) return "配对码无效或已过期，请让电脑重新生成";
  if (text.includes("pairing session not found")) return "配对会话不存在，请让电脑重新生成二维码";
  if (text.includes("invalid user token")) return "云端令牌已失效，请重新配对";
  if (text.includes("instance access denied")) return "当前令牌没有访问权限";
  if (text.includes("instance not found")) return "找不到已配对的电脑";
  if (text.includes("too many requests")) return "请求过于频繁，请稍后再试";
  if (text.includes("idempotency key conflicts")) return "该操作与之前的请求冲突，请稍后重试";
  if (text.includes("unsupported command type")) return "当前版本不支持该操作";
  if (text.includes("payload must be valid json")) return "操作内容格式不正确或超过大小限制";
  return raw || `请求失败 (${status})`;
}

async function cloud<T>(path: string, init?: RequestInit): Promise<T> {
  const headers = new Headers(init?.headers);
  headers.set("Content-Type", "application/json");
  const token = localStorage.getItem("milevia.cloud.token");
  if (token) headers.set("Authorization", `Bearer ${token}`);
  const controller = new AbortController();
  const timeout = globalThis.setTimeout(() => controller.abort(), cloudRequestTimeoutMs);
  const abort = () => controller.abort();
  if (init?.signal?.aborted) controller.abort();
  else init?.signal?.addEventListener("abort", abort, { once: true });
  let response: Response;
  try {
    response = await fetch(`${cloudURL}${path}`, { ...init, headers, signal: controller.signal });
    const body = await response.json().catch(() => null);
    if (!response.ok) {
      if (response.status === 401) {
        // The stored token is unusable: expired, revoked from the desktop, or
        // never activated because the pairing was not confirmed. Keeping it
        // would make every later request fail the same way, so drop it and let
        // the page fall back to the pairing flow.
        if (localStorage.getItem("milevia.cloud.token")) {
          localStorage.removeItem("milevia.cloud.token");
          globalThis.dispatchEvent(new Event("milevia:token-cleared"));
        }
        throw new Error("云端令牌已失效或被撤销，请重新配对");
      }
      const rawError = body && typeof body.error === "string" ? body.error : "";
      throw new Error(localizeCloudError(rawError, response.status));
    }
    if (body === null || body === undefined) throw new Error("云端返回格式无效，请稍后重试");
    return body as T;
  } catch (cause) {
    if (controller.signal.aborted && !init?.signal?.aborted) {
      throw new Error("云端请求超时，请检查手机网络后重试");
    }
    throw cause;
  } finally {
    globalThis.clearTimeout(timeout);
    init?.signal?.removeEventListener("abort", abort);
  }
}

async function consumeMobileEventStream(url: string, token: string, lastEventID: string, onMessage: (event: MessageEvent) => void, signal: AbortSignal) {
  const headers = new Headers({ Accept: "text/event-stream", Authorization: `Bearer ${token}` });
  if (lastEventID) headers.set("Last-Event-ID", lastEventID);
  const response = await fetch(url, { headers, signal });
  if (!response.ok) throw new Error(`stream request failed (${response.status})`);
  if (!response.body) throw new Error("stream response has no body");
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  let eventID = "";
  let data: string[] = [];
  const dispatch = () => {
    if (data.length > 0) onMessage(new MessageEvent("message", { data: data.join("\n"), lastEventId: eventID }));
    data = [];
    eventID = "";
  };
  const processLine = (line: string) => {
    if (line === "") { dispatch(); return; }
    if (line.startsWith(":")) return;
    const separator = line.indexOf(":");
    const field = separator < 0 ? line : line.slice(0, separator);
    const value = separator < 0 ? "" : line.slice(separator + 1).replace(/^ /, "");
    if (field === "id") eventID = value;
    if (field === "data") data.push(value);
  };
  while (!signal.aborted) {
    const next = await reader.read();
    if (next.done) {
      buffer += decoder.decode();
      if (buffer) processLine(buffer);
      break;
    }
    buffer += decoder.decode(next.value, { stream: true });
    const lines = buffer.split(/\r?\n/);
    buffer = lines.pop() || "";
    for (const line of lines) processLine(line);
  }
  dispatch();
}

export default function MobileRemotePage() {
  const navigate = useNavigate();
  // Capacitor 原生 WebView 可能同时注入桌面运行时对象；原生包始终
  // 使用移动端项目选择/对话布局，避免被桌面分支误判。
  const mobileApp = Capacitor.isNativePlatform() || !isDesktop();
  const [token, setToken] = useState(() => localStorage.getItem("milevia.cloud.token") || "");
  const [mobileUpdate, setMobileUpdate] = useState<MobileUpdateState | null>(null);
  const [updateDismissed, setUpdateDismissed] = useState(false);
  const [instances, setInstances] = useState<Instance[]>([]);
  const [instanceID, setInstanceID] = useState("");
  const [snapshot, setSnapshot] = useState<Snapshot | null>(null);
  const [selectedProject, setSelectedProject] = useState("");
  const [selectedConversation, setSelectedConversation] = useState("");
  // 会话历史改成按钮 + 弹窗（原来是下拉框：长标题会把选项撑得非常高）。
  const [conversationHistoryOpen, setConversationHistoryOpen] = useState(false);
  const [newConversationProject, setNewConversationProject] = useState<Project | null>(null);
  const [newConversationAgent, setNewConversationAgent] = useState<"claude-code" | "codex">("claude-code");
  const [mobileView, setMobileView] = useState<"projects" | "conversation">("projects");
  // 手机上的"侧滑返回"（Chrome/WebView 的返回手势、iOS 边缘手势）本质是**历史后退**。
  // 只在 React state 里切视图时历史里没有这一层，手势就什么也不做——所以进项目要真的压一层历史。
  // 这个 ref 记录"我们压过一层、还没退掉"：保证只压一次（HTTP 响应与 SSE 事件可能都触发进入），
  // 并且退出时把它退干净，否则历史里留一个空层，用户下一次返回会被白吞。
  const conversationHistoryRef = useRef(false);
  const [tasksOpen, setTasksOpen] = useState(false);
  // 会话视图的顶栏只有「返回 + 会话名 + ⋯」这一行：刷新、历史会话、新会话、任务队列都收进 ⋯ 菜单。
  // 原来它们各占一行（标题条 + 一行三个按钮），实测吃掉 199px —— 接近 390×800 屏幕的四分之一。
  const [headerMenuOpen, setHeaderMenuOpen] = useState(false);
  // 任务队列是个整页模态层：打开时焦点要搬进面板、关闭时还给触发它的那颗按钮（见下面的焦点 effect）。
  const taskPanelRef = useRef<HTMLElement | null>(null);
  // ⋯ 菜单按钮：既是弹出层的锚点，也是任务面板关闭后焦点的落点（原来是「任务」按钮，现在它就是
  // 菜单里的「任务队列」那一项，关掉面板时它已经不在 DOM 里了）。
  const headerMenuButtonRef = useRef<HTMLButtonElement | null>(null);
  const [taskFilter, setTaskFilter] = useState<TaskFilter>("all");
  const [messageDraft, setMessageDraft] = useState("");
  // 已引用、尚未发送的技能。点技能**不再**把「技能名 + 描述」整段塞进输入框：技能描述动辄上百字，
  // 手机上会直接铺满整屏，而 setMessageDraft 是整体覆盖 —— 用户写到一半的草稿会被无声吃掉。
  // 现在记成一颗可删除的胶囊，发送那一刻才由 composeSkillMessage 展开成完整引用指令。
  const [skillRefs, setSkillRefs] = useState<RemoteSkill[]>([]);
  // 输入条左侧「＋」弹出的工具面板。**内容与电脑端左侧快捷栏同源**：常用提示词 / 常用命令 /
  // 技能三组来自快照（电脑端数据库 + 文件系统扫描），会话入口与输入两组是手机端独有的本地动作。
  const [composerToolsOpen, setComposerToolsOpen] = useState(false);
  // 正在执行的那条快捷方式的 id：点过之后按钮显示"发送中"并挡住重复点击（与桌面端 shortcutBusy 同义）。
  // 走的是异步命令通道，一次往返要等电脑端接单 + 回执，没有这个状态用户会以为没点上而连点。
  const [shortcutBusy, setShortcutBusy] = useState("");
  // defaultAction=confirm 的快捷方式要先弹确认框：它在电脑端执行，手机上误触的代价不在这一屏。
  const [confirmShortcut, setConfirmShortcut] = useState<RemoteShortcut | null>(null);
  const composerShellRef = useRef<HTMLDivElement | null>(null);
  const [title, setTitle] = useState("");
  const [description, setDescription] = useState("");
  const [editingTask, setEditingTask] = useState<Task | null>(null);
  const [editTitle, setEditTitle] = useState("");
  const [editDescription, setEditDescription] = useState("");
  const [editPriority, setEditPriority] = useState("normal");
  const [deletingTask, setDeletingTask] = useState<Task | null>(null);
  const [busy, setBusy] = useState(false);
  // 手动刷新的可见反馈。用户点了刷新之后必须马上看到"在转"、结束后立刻看到结果与时刻，
  // 否则界面完全没有变化，只能靠猜（刷新按钮原先就是这个问题）。
  const [refreshing, setRefreshing] = useState(false);
  const [refreshStatus, setRefreshStatus] = useState<RefreshStatus | null>(null);
  const refreshingRef = useRef(false);
  const [processingConversations, setProcessingConversations] = useState<ProcessingConversations>({});
  const [error, setError] = useState("");
  const [notificationPermission, setNotificationPermission] = useState<NotificationPermission | "unsupported">(() => typeof Notification === "undefined" ? "unsupported" : Notification.permission);
  const [pairingCode, setPairingCode] = useState(() => new URLSearchParams(location.search).get("code") || "");
  const [manualPairingCode, setManualPairingCode] = useState("");
  const [pairingID, setPairingID] = useState(() => new URLSearchParams(location.search).get("pairingId") || new URLSearchParams(location.search).get("pairing_id") || "");
  const [pairingStatus, setPairingStatus] = useState("");
  const [pairingURL, setPairingURL] = useState("");
  const [pairingQR, setPairingQR] = useState("");
  const [pairingExpanded, setPairingExpanded] = useState(false);
  const [pairingReadyForConfirm, setPairingReadyForConfirm] = useState(false);
  const [pairingConfirmed, setPairingConfirmed] = useState(false);
  // 桌面端远程服务状态：Agent 未注册时云端根本没有这台电脑，二维码无从生成。
  const [agentStatus, setAgentStatus] = useState<{ ready: boolean; instanceId: string } | null>(null);
  const [agentEnrollToken, setAgentEnrollToken] = useState("");
  const [agentEnrollBusy, setAgentEnrollBusy] = useState(false);
  const [agentEnrollMessage, setAgentEnrollMessage] = useState("");
  const [agentEnrollWaiting, setAgentEnrollWaiting] = useState(false);
  const [scanning, setScanning] = useState(false);
  const [scanError, setScanError] = useState("");
  // 权限被"永久拒绝"（用户选了不再询问）时系统不会再弹窗，只能去系统设置里放开；
  // 这时给一条可点的入口，否则用户只能对着"请在系统设置中允许摄像头"猜路径。
  const [scanNeedsSettings, setScanNeedsSettings] = useState(false);
  // 手电筒是可选能力：设备没有闪光灯 / 拿不到可用性时报 false，那颗按钮就永远不渲染，
  // 不让"没有闪光灯"变成一条错误提示。
  const [scanTorchAvailable, setScanTorchAvailable] = useState(false);
  const [scanTorchOn, setScanTorchOn] = useState(false);
  const [scanVideo, setScanVideo] = useState<HTMLVideoElement | null>(null);
  const nativeScanListener = useRef<PluginListenerHandle | null>(null);
  const messageInputRef = useRef<HTMLTextAreaElement | null>(null);
  const manualCodeRef = useRef<HTMLInputElement | null>(null);
  const selectedConversationRef = useRef("");
  const pendingMessageRef = useRef(new Map<string, Map<string, PendingMessage>>());
  // revision 记录这条实时消息到达时的快照版本。快照一旦前进到更新的版本，
  // 就说明云端已经反映了那个时刻的状态（消息仍在，或者已被删除），此时不能再
  // 把实时消息当作"快照还没包含"补回去，否则电脑端删掉的消息会在手机上复活。
  const realtimeMessagesRef = useRef(new Map<string, { conversationId: string; message: Message; revision: number }>());
  // 流式增量：按 CLI 的 messageId 累积尚未落地为正式消息的助手文本。正式
  // assistant.message 到达时用同一事件里的 agentMessageId 认领并替换占位内容，
  // 因此这里不参与快照对账，下次快照整体覆盖时会自然清掉残留。
  const streamingMessagesRef = useRef(new Map<string, { conversationId: string; content: string; createdAt: string }>());
  // 实时收到、但服务端快照还没回放的运行记录（重试/压缩/失败）。快照是权威历史，
  // 但它有上传节流：不在这里留一份，刚冒出来的"API 重试中"会被下一次快照抹掉。
  // 每条记录一旦出现在快照里就从这里移除，所以它不会无限增长。
  const pendingNoticesRef = useRef(new Map<string, RemoteNotice[]>());
  const creatingProjectRef = useRef("");
  const instancesRequestGenerationRef = useRef(0);
  const snapshotRevisionRef = useRef(-1);
  const snapshotLoadGenerationRef = useRef(0);
  const scanAccepted = useRef(false);
  const initialPairing = useRef({
    pairingID: new URLSearchParams(location.search).get("pairingId") || new URLSearchParams(location.search).get("pairing_id") || "",
    code: new URLSearchParams(location.search).get("code") || "",
    attempted: false,
  });
  const notifiedEventIDs = useRef(new Set<string>());
  const [commandState, setCommandState] = useState<CommandState | null>(null);
  const [pendingAccessToken, setPendingAccessToken] = useState("");

  // 页面可见性：切到后台时暂停轮询与快照兜底，回到前台立即补一次，省电又省流量。
  const documentVisible = useDocumentVisible();
  const documentVisibleRef = useRef(documentVisible);
  // 上一次的可见性，用来识别「由隐藏转为可见」这个瞬间。
  const wasDocumentVisibleRef = useRef(documentVisible);
  // 消息列表是否「贴着底部」。用户主动向上翻阅时不能夺走滚动位置。
  const stickToBottomRef = useRef(true);
  // 安卓物理返回键的当前处理逻辑。放在 ref 里，监听只注册一次也能拿到最新状态。
  const backHandlerRef = useRef<() => boolean>(() => false);

  // 桌面端探测电脑端 Agent 是否已注册到云端。未注册时配对无法进行，页面应引导
  // 用户做一次注册，而不是让用户反复点击注定返回 503 的"生成二维码"。
  const loadAgentStatus = useCallback(async () => {
    if (!isDesktop()) return false;
    try {
      const value = await api<{ ready?: boolean; instanceId?: string }>("/api/remote/agent-status");
      setAgentStatus({ ready: Boolean(value?.ready), instanceId: value?.instanceId || "" });
      return Boolean(value?.ready);
    } catch {
      setAgentStatus(null);
      return false;
    }
  }, []);

  const markConversationProcessing = useCallback((conversationID: string, snapshotRevision: number) => {
    setProcessingConversations((current) => {
      const existing = current[conversationID];
      return {
        ...current,
        [conversationID]: {
          count: (existing?.count || 0) + 1,
          startedAt: existing?.startedAt || Date.now(),
          snapshotRevision: existing?.snapshotRevision ?? snapshotRevision,
        },
      };
    });
  }, []);

  const clearConversationProcessing = useCallback((conversationID: string, all = false) => {
    setProcessingConversations((current) => {
      const existing = current[conversationID];
      if (!existing) return current;
      if (!all && existing.count > 1) return { ...current, [conversationID]: { ...existing, count: existing.count - 1 } };
      const next = { ...current };
      delete next[conversationID];
      return next;
    });
  }, []);

  // 扫码结果的唯一接收点：原生（`barcodesScanned` 事件）与 Web（BarcodeDetector 轮询）
  // 两条路都汇到这里，所以幂等守卫只写在这一处。ML Kit 是**逐帧持续上报**的 —— 同一张码
  // 会被反复识别，没有 `scanAccepted` 这道闸，下层就会对着同一张码反复走 claim 流程。
  // 因此 `if (scanAccepted.current) return true` 必须在最前面（连"不是配对码"的报错都
  // 不能被反复刷出来 —— 那一支返回 false 且不置位，只在真识别到码时才走到）。
  //
  // 依赖数组写 `[]` 是有意的：这里引用的 `closeScanOverlay` / `claimPairing` 都是组件内
  // 普通函数，会被捕获成**首帧那一版**。之所以安全，是因为它们只读"显式传入的参数 +
  // setState 函数"——`closeScanOverlay` 只调三个 setState；`claimPairing` 那两个 state
  // 默认参数（pairingID/pairingCode）在扫码路径上总被显式传参覆盖，永远不生效。
  // 换成带依赖的 useCallback 只会让 identity 频繁变化，进而把下面两个平台 effect 反复重启。
  const acceptScannedPairing = useCallback((value: string) => {
    const scanned = pairingFromScan(value);
    if (!scanned.pairingID) {
      setScanError("这不是 Milevia 配对二维码，请对准电脑端刚生成的二维码");
      return false;
    }
    if (scanAccepted.current) return true;
    scanAccepted.current = true;
    setPairingID(scanned.pairingID);
    setPairingCode(scanned.code || "");
    // 扫到之后立刻关层：走同一个出口（而不是就地写 setScanning(false)），
    // 这样"同一拍撤掉清底类名"这条快路径在成功/取消两条路上都生效。
    closeScanOverlay();
    if (/^\d{6}$/.test(scanned.code)) {
      void claimPairing(scanned.pairingID, scanned.code);
    } else {
      setPairingStatus("已识别配对会话，请输入电脑端显示的 6 位校验码");
      // 二维码有意不含校验码（避免经浏览器历史或代理日志泄漏），扫码后必须
      // 人工补输，因此直接聚焦输入框，省掉一次寻找动作。
      window.setTimeout(() => manualCodeRef.current?.focus(), 0);
    }
    return true;
  }, []);

  // 取景层"在屏幕上"期间，整链必须同时保持两件事：① 清底（让 WebView 之下的摄像头透上来）；
  // ② 其余同级区块不绘制（`visibility: hidden`，顺带移出焦点与无障碍树）。
  //
  // 这两件事由**这一个** effect 拥有，不要散到两个平台分支里去 —— 散开必然踩两个坑：
  //   ① 失败态依然留在屏幕上（此时 scanning 已回 false），平台分支的清理会把类名撤掉 →
  //      浮层背后那屏恢复可见、**键盘还能 Tab 到被盖住的按钮**（浮层就不再是模态了）；
  //   ② 类名的增删若与浮层的挂载分处两个时机，关闭时会闪一帧"层已摘掉、页面还被隐藏着"
  //      的空屏（原生分支上那一帧还会再闪一下摄像头画面）。
  // 声明位置必须在两个平台 effect **之前**：同一次提交里 passive effect 按声明顺序执行，
  // 原生分支要保证 startScan() 之前底已经清好。
  const scanOverlayVisible = scanning || Boolean(scanError);
  useEffect(() => {
    setScannerActive(scanOverlayVisible);
    return () => setScannerActive(false);
  }, [scanOverlayVisible]);

  useEffect(() => {
    if (!scanning || !Capacitor.isNativePlatform()) return;
    let cancelled = false;
    let started = false;
    let timeout: number | undefined;
    scanAccepted.current = false;

    const stop = async () => {
      if (timeout !== undefined) window.clearTimeout(timeout);
      const listener = nativeScanListener.current;
      nativeScanListener.current = null;
      await listener?.remove().catch(() => undefined);
      if (started) await BarcodeScanner.stopScan().catch(() => undefined);
      setScanTorchAvailable(false);
      setScanTorchOn(false);
    };

    void (async () => {
      try {
        const { supported } = await BarcodeScanner.isSupported();
        if (!supported) throw new Error("此设备没有可用摄像头");
        let permission = await BarcodeScanner.checkPermissions();
        if (permission.camera === "prompt" || permission.camera === "prompt-with-rationale") {
          permission = await BarcodeScanner.requestPermissions();
        }
        if (permission.camera !== "granted" && permission.camera !== "limited") {
          // 走到这里可能是"用户刚拒绝"，也可能是"早就拒绝了、系统不再弹窗"。无论哪种，
          // 都只有系统设置这一条路能修好，所以直接把设置入口放出来。
          if (!cancelled) setScanNeedsSettings(true);
          throw new Error("未获得摄像头权限，请在系统设置中允许 Milevia 使用摄像头");
        }
        if (cancelled) return;

        nativeScanListener.current = await BarcodeScanner.addListener("barcodesScanned", ({ barcodes }) => {
          for (const barcode of barcodes) {
            if (acceptScannedPairing(barcode.rawValue || barcode.displayValue)) return;
          }
        });
        if (cancelled) return;
        await BarcodeScanner.startScan({ formats: [BarcodeFormat.QrCode], lensFacing: LensFacing.Back });
        started = true;
        // 这一段**不是**冗余的，删掉会让相机永久开着：清理函数（见下方 return）会在
        // `cancelled = true` 之后立刻调 `stop()`，而那时 `started` 还是 false（`startScan`
        // 尚未 resolve）→ `stop()` 里 `if (started) stopScan()` 整支跳过，相机没被停。
        // 随后 `startScan` 才 resolve、`started` 才变 true —— 若不在这里补一次，就再也没人
        // 会去停它了：listener 已被摘掉、超时也没设，用户看到的是"浮层关了但摄像头一直开着"。
        // 走 `stop()` 而不是就地写 stopScan()，是为了让"摘 listener / 关灯 / 复位状态"只在一个地方。
        if (cancelled) {
          await stop();
          return;
        }
        // 手电筒可用性单独问一次：问不到就当"没有"，不把异常冒泡成扫码失败。
        void BarcodeScanner.isTorchAvailable()
          .then((value) => { if (!cancelled) setScanTorchAvailable(Boolean((value as { available?: boolean })?.available)); })
          .catch(() => undefined);
        timeout = window.setTimeout(() => {
          if (!cancelled) {
            setScanError("30 秒内未识别到二维码，请靠近电脑屏幕、调亮亮度或打开手电筒后重试");
            setScanning(false);
          }
        }, 30_000);
      } catch (cause) {
        if (!cancelled) {
          setScanError(cause instanceof Error ? cause.message : "无法启动二维码扫描，请稍后重试");
          setScanning(false);
        }
      }
    })();

    return () => {
      cancelled = true;
      void stop();
    };
  }, [scanning, acceptScannedPairing]);

  // Web 分支（手机浏览器 / 桌面预览）：画面是自己画的，用 getUserMedia + BarcodeDetector。
  // 清底类名由上面那个 effect 统一管，这里不再碰（`.mobile-scan[data-backdrop="solid"]`
  // 会自己铺底，所以两条分支共用同一套取景层样式）。
  useEffect(() => {
    if (!scanning || Capacitor.isNativePlatform() || !scanVideo) return;
    let cancelled = false;
    let stream: MediaStream | undefined;
    const timeout = window.setTimeout(() => {
      if (!cancelled) {
        setScanError("30 秒内未识别到二维码，请靠近电脑屏幕、调亮亮度后重试");
        setScanning(false);
      }
    }, 30_000);
    const detectorCtor = (globalThis as typeof globalThis & { BarcodeDetector?: BarcodeDetectorLike }).BarcodeDetector;
    if (!detectorCtor) {
      window.clearTimeout(timeout);
      setScanError("当前浏览器不支持二维码识别，请用手机系统相机扫码，或改用「使用校验码」配对");
      setScanning(false);
      return;
    }
    const mediaDevices = navigator.mediaDevices;
    if (!mediaDevices?.getUserMedia) {
      window.clearTimeout(timeout);
      setScanError("当前设备无法访问摄像头，请检查浏览器摄像头权限");
      setScanning(false);
      return;
    }
    void mediaDevices.getUserMedia({ video: { facingMode: { ideal: "environment" } }, audio: false })
      .then(async (nextStream) => {
        if (cancelled) { nextStream.getTracks().forEach((track) => track.stop()); return; }
        stream = nextStream;
        scanVideo.srcObject = nextStream;
        await scanVideo.play();
        let detector: { detect(source: HTMLVideoElement): Promise<BarcodeDetectorResult[]> };
        try {
          detector = new detectorCtor({ formats: ["qr_code"] });
        } catch {
          throw new Error("当前 Android WebView 不支持二维码识别，请改用「使用校验码」配对");
        }
        while (!cancelled) {
          const results = await detector.detect(scanVideo).catch(() => []);
          if (results.some((item) => acceptScannedPairing(item.rawValue || ""))) break;
          if (results.some((item) => item.rawValue)) {
            setScanError("扫描到的二维码不是 Milevia 配对二维码，请对准电脑端刚生成的那一张");
          }
          await new Promise((resolve) => window.setTimeout(resolve, 250));
        }
      })
      .catch((cause: unknown) => {
        if (!cancelled) {
          setScanError(cause instanceof Error && cause.message.includes("不支持") ? cause.message : "无法打开摄像头，请在浏览器设置里允许本站使用摄像头后重试");
          setScanning(false);
        }
      });
    return () => {
      cancelled = true;
      window.clearTimeout(timeout);
      stream?.getTracks().forEach((track) => track.stop());
      if (scanVideo) scanVideo.srcObject = null;
    };
  }, [scanning, scanVideo, acceptScannedPairing]);

  // 返回本次请求的真实结果，供"手动刷新"判断该给什么反馈。
  // 三个结果必须分开：列表请求每 5 秒被轮询触发一次，手动刷新那一次**很容易被轮询抢答**
  // （手机上一次列表请求要几秒，而轮询周期就是 5 秒）。把"被更新的请求取代"当成失败，
  // 就会在一切正常时弹出一句"刷新失败：没有连上云端"——这正是用户看不懂的那种假失败。
  //   ok         = 拿到了列表（可能是空数组：云端确实一台电脑都没有）
  //   failed     = 请求真的失败了
  //   superseded = 这次请求被更新的请求取代，结果由那一次落地
  const loadInstances = useCallback(async (): Promise<InstanceFetchResult> => {
    const requestGeneration = ++instancesRequestGenerationRef.current;
    const superseded: InstanceFetchResult = { instances: [], outcome: "superseded" };
    if (!localStorage.getItem("milevia.cloud.token")) {
      if (requestGeneration !== instancesRequestGenerationRef.current) return superseded;
      setInstances([]);
      setInstanceID("");
      // 没有令牌是"还没配对"，属于拿到了真实状态（空的），不是失败。
      return { instances: [], outcome: "ok" };
    }
    setError("");
    try {
      const result = await cloud<Instance[]>("/v1/instances");
      if (requestGeneration !== instancesRequestGenerationRef.current) return superseded;
      const nextInstances = Array.isArray(result) ? result : [];
      setInstances(nextInstances);
      setInstanceID((current) => nextInstances.some((item) => item.instanceId === current)
        ? current
        : nextInstances[0]?.instanceId || "");
      if (!Array.isArray(result)) {
        setError("云端返回了无效的电脑实例列表");
      }
      return { instances: nextInstances, outcome: "ok" };
    } catch (cause) {
      if (requestGeneration !== instancesRequestGenerationRef.current) return superseded;
      const reason = cause instanceof TypeError ? "无法连接云端，请确认 keyanjia.info:8443 已放行并可访问" : cause instanceof Error ? cause.message : "无法加载电脑实例";
      setError(reason);
      return { instances: [], outcome: "failed", reason };
    }
  }, []);

  useEffect(() => {
    snapshotRevisionRef.current = -1;
    // These maps are scoped to the selected desktop instance. Retaining them
    // across a logout or instance switch can make a coincidentally reused
    // conversation ID appear to be processing in the new instance.
    pendingMessageRef.current.clear();
    realtimeMessagesRef.current.clear();
    streamingMessagesRef.current.clear();
    pendingNoticesRef.current.clear();
    setProcessingConversations({});
  }, [instanceID]);
  const loadSnapshot = useCallback(async (options?: { force?: boolean; instanceId?: string }): Promise<SnapshotFetchResult> => {
    // 允许显式指定实例：手动刷新刚刚才拉过实例列表，选中的电脑可能已经失效并被换掉，
    // 这时要请求"列表里实际选中的那台"，而不是这一轮闭包里那台已经不存在的。
    const target = options?.instanceId || instanceID;
    if (!target) return { outcome: "skipped", snapshot: null };
    const requestGeneration = ++snapshotLoadGenerationRef.current;
    const force = options?.force === true;
    // 版本基线。请求的实例与"当前选中的实例"不是同一台时（刚配对完、或选中的电脑已从列表
    // 里消失）没有可比基线，必须按全新实例处理：拿 B 的版本号去比 A 的，既可能让守卫不通过、
    // 把 B 的快照整个丢掉，又会把"什么都没应用"报成"已是最新"。
    const baseline = options?.instanceId && options.instanceId !== instanceID ? -1 : snapshotRevisionRef.current;
    setError("");
    try {
      // 带上已知版本号：云端在内容未变化时只回一个小标记，省掉一次全量传输。
      // 手动刷新（force）必须跳过这个短路——被短路掉的长轮询看不出来，但用户点了刷新
      // 却什么都没发生，就等于"刷新按钮坏了"（本轮修复的原始投诉）。而且本地快照可能
      // 落后于 snapshotRevisionRef（例如上一次失败后回落到 localStorage 缓存），
      // 这种情况下短路会把"显示着旧数据"判成"已是最新"。
      const query = force ? "" : `?revision=${baseline}`;
      const value = await cloud<Snapshot & { unchanged?: boolean }>(`/v1/instances/${encodeURIComponent(target)}/snapshot${query}`);
      if (value?.unchanged) return { outcome: "unchanged", snapshot: null };
      if (!value || !Array.isArray(value.projects)) {
        throw new Error("云端返回了无效的项目快照");
      }
      // Snapshots from older mobile builds may not carry a revision. Treat
      // those as the initial revision instead of dropping a valid project
      // list because `undefined >= -1` is false.
      value.snapshotRevision = typeof value.snapshotRevision === "number" && Number.isFinite(value.snapshotRevision)
        ? value.snapshotRevision
        : 0;
      if (requestGeneration === snapshotLoadGenerationRef.current && value.snapshotRevision >= baseline) {
        snapshotRevisionRef.current = value.snapshotRevision;
        const pending = pendingMessageRef.current;
        if (pending.size > 0) {
          value.projects = value.projects.map((item) => ({ ...item, conversations: item.conversations.map((entry) => {
            const messages = pending.get(entry.id);
            if (!messages || messages.size === 0) return entry;
            // Consume persisted messages as a multiset so two identical
            // prompts do not accidentally clear both optimistic entries.
            const persistedCounts = new Map<string, number>();
            for (const message of entry.messages) {
              if (message.role === "user") persistedCounts.set(message.content, (persistedCounts.get(message.content) || 0) + 1);
            }
            const additions: Message[] = [];
            const matchedRequestIDs = new Set<string>();
            for (const pendingMessage of messages.values()) {
              const count = persistedCounts.get(pendingMessage.content) || 0;
              if (count > 0) {
                persistedCounts.set(pendingMessage.content, count - 1);
                matchedRequestIDs.add(pendingMessage.requestId);
              }
              else additions.push({ id: `pending-${pendingMessage.requestId}`, role: "user", content: pendingMessage.content, createdAt: pendingMessage.createdAt });
            }
            for (const requestID of matchedRequestIDs) messages.delete(requestID);
            if (messages.size === 0) pending.delete(entry.id);
            return additions.length > 0 ? { ...entry, messages: [...entry.messages, ...additions] } : entry;
          }) }));
        }
        // A message event can arrive before the Agent has uploaded its next
        // snapshot. Preserve those messages while accepting the older snapshot
        // so streamed output cannot briefly disappear from the mobile view.
        const realtimeMessages = realtimeMessagesRef.current;
        if (realtimeMessages.size > 0) {
          value.projects = value.projects.map((item) => ({ ...item, conversations: item.conversations.map((entry) => {
            let changed = false;
            const messages = entry.messages.map((message) => {
              const live = realtimeMessages.get(message.id);
              if (!live || live.conversationId !== entry.id) return message;
              realtimeMessages.delete(message.id);
              // The recovery snapshot intentionally bounds older content. If
              // the realtime event has the full message, prefer it so a fast
              // snapshot cannot overwrite a complete assistant response with
              // its 2000-character prefix.
              if (live.message.content.length > message.content.length) {
                changed = true;
                return live.message;
              }
              return message;
            });
            const extras = [...realtimeMessages.values()]
              .filter((item) => item.conversationId === entry.id
                && item.revision >= value.snapshotRevision
                && !entry.messages.some((message) => message.id === item.message.id))
              .map((item) => {
                realtimeMessages.delete(item.message.id);
                return item.message;
              });
            return changed || extras.length > 0 ? { ...entry, messages: [...messages, ...extras] } : entry;
          }) }));
        }
        // 增量不落库，因此不在快照里：整体覆盖会把正在输出的回复抹掉。这里按
        // 累积内容把它补回，正式消息到达时替换、之后随快照自然消失。
        const streaming = streamingMessagesRef.current;
        if (streaming.size > 0) {
          value.projects = value.projects.map((item) => {
            const conversations = item.conversations.map((entry) => {
              let touched = false;
              const messages = entry.messages.map((message) => {
                if (!message.id.startsWith("stream-")) return message;
                const chunk = streaming.get(message.id.slice("stream-".length));
                if (!chunk || chunk.conversationId !== entry.id) return message;
                touched = true;
                return { ...message, content: chunk.content };
              });
              const pending: Message[] = [];
              for (const [messageID, chunk] of streaming) {
                if (chunk.conversationId !== entry.id) continue;
                const placeholderID = `stream-${messageID}`;
                if (messages.some((message) => message.id === placeholderID)) continue;
                pending.push({ id: placeholderID, role: "assistant", content: chunk.content, createdAt: chunk.createdAt });
                touched = true;
              }
              return touched ? { ...entry, messages: [...messages, ...pending] } : entry;
            });
            const changed = conversations.some((entry, index) => entry !== item.conversations[index]);
            return changed ? { ...item, conversations } : item;
          });
        }
        // 快照里的 notices 是事件原文，先按与实时通道同一套规则解析；再把本次刚收到、
        // 还没进快照的那几条并回来——少了后者，刚冒出来的"API 重试中"会被下一次快照覆盖掉。
        const pendingNotices = pendingNoticesRef.current;
        value.projects = value.projects.map((item) => ({ ...item, conversations: item.conversations.map((entry) => {
          const serverNotices = noticesFromSnapshot(entry.notices);
          const local = pendingNotices.get(entry.id);
          if (!local || local.length === 0) return { ...entry, notices: serverNotices };
          const serverIDs = new Set(serverNotices.map((notice) => notice.id));
          // "服务端已有同名记录"不等于"本地这条可以扔"。工具确认（replace 型）必须
          // 一直留着：快照有上传节流，下一次可能只带回更旧的 pending，此时若本地那条
          // 已被清掉，合并结果就会是 pending —— 界面从"已确认"倒退回"等待确认"。
          const remaining = local.filter((notice) => notice.merge === "replace" || !serverIDs.has(notice.id));
          if (remaining.length > 0) pendingNotices.set(entry.id, remaining);
          else pendingNotices.delete(entry.id);
          return { ...entry, notices: mergeNotices(serverNotices, remaining) };
        }) }));
        setSnapshot(value);
        localStorage.setItem(`milevia.snapshot.${target}`, JSON.stringify(value));
      }
      // 版本号有没有前进，就是"这次刷新到底有没有带回来新东西"的判据。
      return { outcome: value.snapshotRevision > baseline ? "updated" : "unchanged", snapshot: value };
    } catch (cause) {
      if (requestGeneration !== snapshotLoadGenerationRef.current) return { outcome: "skipped", snapshot: null };
      const reason = cause instanceof Error ? cause.message : "无法加载项目快照";
      const cached = localStorage.getItem(`milevia.snapshot.${target}`);
      if (cached) {
        try {
          const cachedValue = JSON.parse(cached) as Snapshot;
          setSnapshot(cachedValue);
          setError("当前显示的是最近一次同步快照");
          // 有缓存也要如实报失败：界面确实停在旧数据上，不能让刷新按钮显示"已刷新"。
          // 原因写短一点——"当前显示的是最近一次同步快照"那句由页面上的错误条负责说明，
          // 两边各说一句，避免同一条红框提示连说两遍。
          return { outcome: "failed", snapshot: cachedValue, reason: "无法连接云端" };
        } catch { /* ignore invalid cache */ }
      }
      setError(reason);
      return { outcome: "failed", snapshot: null, reason };
    }
  }, [instanceID]);

  const applyRealtimeEvent = useCallback((raw: MessageEvent): boolean => {
    try {
      const event = JSON.parse(String(raw.data)) as { eventId?: string; type?: string; taskRunId?: string; payload?: unknown; createdAt?: string };
      if (!event || !event.type || !event.payload) return false;
      const payload = event.payload as Partial<Message> & {
        conversationId?: string;
        projectId?: string;
        status?: string;
        // 流式增量事件的字段：messageId 是 CLI 自己的消息 id，与最终
        // assistant.message 上的 agentMessageId 一致，据此认领占位内容。
        messageId?: string;
        delta?: string;
        agentMessageId?: string;
      };
      if (event.type === "conversation.created" && typeof payload.conversationId === "string" && typeof payload.projectId === "string") {
        setSnapshot((current) => {
          if (!current) return current;
          return {
            ...current,
            projects: current.projects.map((item) => item.id !== payload.projectId || item.conversations.some((entry) => entry.id === payload.conversationId)
              ? item
              : {
                ...item,
                conversations: [{
                  id: payload.conversationId!,
                  title: "新会话",
                  status: "idle",
                  agentId: "",
                  lastActivityAt: event.createdAt || new Date().toISOString(),
                  isCurrent: true,
                  messages: [],
                }, ...item.conversations.map((entry) => ({ ...entry, isCurrent: false }))],
              }),
          };
        });
        if (creatingProjectRef.current === payload.projectId) {
          creatingProjectRef.current = "";
          setSelectedProject(payload.projectId);
          setSelectedConversation(payload.conversationId);
          setTasksOpen(false);
          setPairingExpanded(false);
          enterConversationView();
          setBusy(false);
        }
        return true;
      }
      if (event.type === "assistant.delta") {
        const delta = typeof payload.delta === "string" ? payload.delta : "";
        const conversationId = typeof payload.conversationId === "string" ? payload.conversationId : "";
        if (typeof payload.messageId !== "string" || payload.messageId === "" || delta === "" || conversationId === "") return false;
        const placeholderID = `stream-${payload.messageId}`;
        const streaming = streamingMessagesRef.current;
        const previous = streaming.get(payload.messageId);
        const accumulated = {
          conversationId,
          content: (previous?.content || "") + delta,
          createdAt: previous?.createdAt || event.createdAt || new Date().toISOString(),
        };
        streaming.set(payload.messageId, accumulated);
        setSnapshot((current) => {
          if (!current) return current;
          return { ...current, projects: current.projects.map((item) => ({ ...item, conversations: item.conversations.map((entry) => {
            if (entry.id !== conversationId) return entry;
            const placeholder: Message = { id: placeholderID, role: "assistant", content: accumulated.content, createdAt: accumulated.createdAt };
            const index = entry.messages.findIndex((message) => message.id === placeholderID);
            return index < 0
              ? { ...entry, messages: [...entry.messages, placeholder] }
              : { ...entry, messages: entry.messages.map((message, at) => (at === index ? placeholder : message)) };
          }) })) };
        });
        return true;
      }
      if ((event.type === "user.message" || event.type === "assistant.message") && typeof payload.id === "string" && typeof payload.conversationId === "string" && typeof payload.content === "string") {
        const role: Message["role"] = event.type === "assistant.message" ? "assistant" : "user";
        const realtimeMessage: Message = { id: payload.id, runId: payload.runId, role, content: payload.content, createdAt: payload.createdAt || event.createdAt || new Date().toISOString() };
        realtimeMessagesRef.current.set(realtimeMessage.id, { conversationId: payload.conversationId, message: realtimeMessage, revision: snapshotRevisionRef.current });
        if (realtimeMessagesRef.current.size > 500) {
          const oldest = realtimeMessagesRef.current.keys().next().value;
          if (oldest) realtimeMessagesRef.current.delete(oldest);
        }
        if (role === "user") {
          const pending = pendingMessageRef.current.get(payload.conversationId);
          if (pending) {
            const matching = [...pending.values()].find((item) => item.content === payload.content);
            if (matching) pending.delete(matching.requestId);
            if (pending.size === 0) pendingMessageRef.current.delete(payload.conversationId);
          }
        }
        // 正式消息到达后由它取代流式占位，两者在同一次状态更新里交接，避免
        // 中间出现「占位 + 正式」并存的重复帧。
        const agentMessageID = typeof payload.agentMessageId === "string" ? payload.agentMessageId : "";
        const placeholderID = agentMessageID ? `stream-${agentMessageID}` : "";
        if (agentMessageID) streamingMessagesRef.current.delete(agentMessageID);
        setSnapshot((current) => {
          if (!current) return current;
          return { ...current, projects: current.projects.map((item) => ({ ...item, conversations: item.conversations.map((entry) => {
            if (entry.id !== payload.conversationId) return entry;
            const base = placeholderID === "" ? entry.messages : entry.messages.filter((message) => message.id !== placeholderID);
            const exists = base.some((message) => message.id === payload.id);
            const optimisticIndex = role === "user" ? base.findIndex((message) => message.id.startsWith("pending-") && message.role === "user" && message.content === payload.content) : -1;
            if (exists) return base.length === entry.messages.length ? entry : { ...entry, messages: base };
            const messages = optimisticIndex >= 0
              ? base.map((message, index) => index === optimisticIndex ? realtimeMessage : message)
              : [...base, realtimeMessage];
            return { ...entry, messages };
          }) })) };
        });
        return true;
      }
      // 运行状态类事件（API 重试、上下文压缩、后台任务、执行失败）：以前落到这里
      // 就返回 false 被丢掉，手机端只看得到对话内容，看不到"正在重试""压缩失败"。
      const parsed = noticeFromRealtimeEvent({ eventId: event.eventId, type: event.type, taskRunId: event.taskRunId, payload, createdAt: event.createdAt });
      if (parsed) {
        const pendingNotices = pendingNoticesRef.current;
        pendingNotices.set(parsed.conversationId, appendNotice(pendingNotices.get(parsed.conversationId), parsed.notice));
        setSnapshot((current) => {
          if (!current) return current;
          return {
            ...current,
            projects: current.projects.map((item) => ({ ...item, conversations: item.conversations.map((entry) => entry.id === parsed.conversationId ? { ...entry, notices: appendNotice(entry.notices, parsed.notice) } : entry) })),
          };
        });
        return true;
      }
    } catch { /* malformed realtime payloads are recovered by the snapshot */ }
    return false;
  }, []);

  const notifyHiddenMobileEvent = useCallback((raw: MessageEvent) => {
    if (!document.hidden || typeof Notification === "undefined" || Notification.permission !== "granted") return;
    try {
      const event = JSON.parse(String(raw.data)) as { eventId?: string; type?: string; payload?: unknown };
      if (!event.eventId || notifiedEventIDs.current.has(event.eventId)) return;
      // Conversation messages are already visible when the user returns. Task
      // and run state changes are the events that need an interruption notice.
      if (!event.type || !/^(task\.|run\.|approval\.)/.test(event.type)) return;
      notifiedEventIDs.current.add(event.eventId);
      const payload = event.payload && typeof event.payload === "object" ? event.payload as { summary?: unknown; status?: unknown } : {};
      const detail = typeof payload.summary === "string" ? payload.summary : typeof payload.status === "string" ? `状态：${payload.status}` : event.type;
      new Notification("Milevia", { body: detail, tag: event.eventId });
    } catch {
      // Malformed events are recovered by the normal snapshot refresh.
    }
  }, []);

  // 云端判定令牌不可用时（过期、被电脑端撤销、或配对尚未确认）云请求层会清理
  // 存储并广播该事件，这里把页面状态一起复位，回到配对流程，避免用户卡在一
  // 串注定失败的请求里。
  useEffect(() => {
    const onTokenCleared = () => {
      setToken("");
      setInstances([]);
      setInstanceID("");
      setSnapshot(null);
      setSelectedProject("");
      setSelectedConversation("");
      setPairingExpanded(true);
      setPairingStatus("云端令牌已失效，请重新配对");
    };
    globalThis.addEventListener("milevia:token-cleared", onTokenCleared);
    return () => globalThis.removeEventListener("milevia:token-cleared", onTokenCleared);
  }, []);
  useEffect(() => {
    documentVisibleRef.current = documentVisible;
  }, [documentVisible]);

  // 启动后在后台查一次手机端有没有新版本。Android 包没法自己下载安装，所以这里
  // 只把结果做成一条横幅，点「立即更新」交给系统浏览器下载（见 mobile-update.ts）。
  // 失败一律静默：更新提醒不值得在启动路径上打扰用户，也不该让页面出现红字。
  useEffect(() => {
    if (!Capacitor.isNativePlatform()) return;
    const controller = new AbortController();
    void checkMobileUpdate(controller.signal)
      .then((next) => {
        if (next) setMobileUpdate(next);
      })
      .catch(() => undefined);
    return () => controller.abort();
  }, []);
  useEffect(() => {
    if (!token.trim()) return;
    void loadInstances();
    // 后台不轮询。visibilitychange 会重跑本效果，因此回到前台时上面这句会立刻补一次。
    if (!documentVisible) return;
    const timer = window.setInterval(() => { void loadInstances(); }, 5000);
    return () => window.clearInterval(timer);
  }, [token, loadInstances, documentVisible]);
  useEffect(() => { void loadSnapshot(); }, [loadSnapshot]);
  useEffect(() => {
    if (!instanceID) return;
    const tokenValue = localStorage.getItem("milevia.cloud.token") || "";
    const controller = new AbortController();
    let stopped = false;
    let lastEventID = "";
    // SSE is the fast path; periodic snapshots remain active because
    // conversation output is produced independently of task events.
    // SSE is the normal fast path. Keep a slower safety poll for proxies or
    // networks that silently drop events, without competing with every event
    // refresh and increasing full-snapshot traffic threefold.
    // SSE 是快速通道，仍保留一个较慢的快照兜底，兼容会静默丢事件的代理与网络。
    // 后台不跑兜底轮询（SSE 连接本身保留），回到前台由可见性效果补刷一次快照。
    let fallbackTimer: number | null = null;
    const startFallbackTimer = () => {
      if (fallbackTimer !== null) return;
      fallbackTimer = window.setInterval(() => {
        if (!documentVisibleRef.current) return;
        void loadSnapshot();
      }, 15_000);
    };
    startFallbackTimer();
    let refreshTimer: number | null = null;
    const scheduleSnapshotRefresh = (delay: number) => {
      // Trailing throttle: a busy assistant stream cannot keep postponing
      // durable task metadata and history reconciliation indefinitely.
      if (refreshTimer !== null) return;
      refreshTimer = window.setTimeout(() => {
        refreshTimer = null;
        // 后台不拉快照：SSE 事件在后台仍会到达，若不拦住，前面「隐藏时暂停
        // 轮询」就会被这条路径完全抵消。回到前台时可见性效果会整体补刷一次。
        if (!documentVisibleRef.current) return;
        void loadSnapshot();
      }, delay);
    };
    const wait = (delay: number) => new Promise<void>((resolve) => {
      const timer = window.setTimeout(resolve, delay);
      controller.signal.addEventListener("abort", () => { window.clearTimeout(timer); resolve(); }, { once: true });
    });
    const runStream = async () => {
      let retryDelay = 1000;
      while (!stopped && !controller.signal.aborted) {
        try {
          const query = new URLSearchParams({ instanceId: instanceID });
          await consumeMobileEventStream(`${cloudURL}/v1/stream?${query.toString()}`, tokenValue, lastEventID, (event) => {
            if (event.lastEventId) lastEventID = event.lastEventId;
            notifyHiddenMobileEvent(event);
            const isMessage = applyRealtimeEvent(event);
            // 消息内容已经随事件到达，不需要为它再拉整份快照：那会让手机在每条
            // 助手消息后重新下载全部项目与会话，是移动端延迟最大的单一来源。结构性
            // 事件（任务、会话生命周期）仍需对账，漏掉的事件由慢速兜底轮询收敛。
            if (!isMessage) scheduleSnapshotRefresh(500);
          }, controller.signal);
          if (stopped || controller.signal.aborted) break;
          await wait(retryDelay);
          retryDelay = 1000;
        } catch {
          if (stopped || controller.signal.aborted) break;
          startFallbackTimer();
          await wait(retryDelay);
          retryDelay = Math.min(retryDelay * 2, 30_000);
        }
      }
    };
    void runStream();
    return () => {
      stopped = true;
      controller.abort();
      if (fallbackTimer !== null) window.clearInterval(fallbackTimer);
      if (refreshTimer !== null) window.clearTimeout(refreshTimer);
    };
  }, [instanceID, loadSnapshot, applyRealtimeEvent, notifyHiddenMobileEvent]);
  // 回到前台补刷一次快照，避免用户先看到后台期间的旧状态。
  // 只在「隐藏 → 可见」的瞬间触发：实例切换等其它原因引起的重跑不需要额外拉快照。
  // （实例列表由上面的轮询效果在可见性变化时自动补拉，这里不重复。）
  useEffect(() => {
    const wasVisible = wasDocumentVisibleRef.current;
    wasDocumentVisibleRef.current = documentVisible;
    if (!documentVisible || wasVisible) return;
    if (!instanceID) return;
    void loadSnapshot();
  }, [documentVisible, instanceID, loadSnapshot]);
  useEffect(() => {
    let cancelled = false;
    if (!pairingURL) {
      setPairingQR("");
      return () => { cancelled = true; };
    }
    void QRCode.toDataURL(pairingURL, { width: 240, margin: 1, errorCorrectionLevel: "M" })
      .then((dataURL) => { if (!cancelled) setPairingQR(dataURL); })
      .catch(() => { if (!cancelled) setPairingQR(""); });
    return () => { cancelled = true; };
  }, [pairingURL]);
  useEffect(() => {
    if (!commandState || terminalCommandStatuses.includes(commandState.status)) return;
    const timer = window.setInterval(() => {
      void cloud<CommandState & { command?: { commandId?: string } }>(`/v1/commands/${encodeURIComponent(commandState.commandId)}`)
        .then((value) => setCommandState((current) => mergeCommandState(current, normalizeCommandState(value, commandState.commandId))))
        .catch(() => undefined);
    }, 5000);
    return () => window.clearInterval(timer);
  }, [commandState?.commandId, commandState?.status]);
  useEffect(() => {
    if (!pendingAccessToken || !pairingID) return;
    const timer = window.setInterval(() => {
      void cloud<{ status: string }>(`/v1/pairings/${encodeURIComponent(pairingID)}/status`)
        .then((state) => {
          if (state.status === "confirmed") {
            localStorage.setItem("milevia.cloud.token", pendingAccessToken);
            setToken(pendingAccessToken);
            setPendingAccessToken("");
            setPairingExpanded(false);
            setPairingStatus("电脑已确认，绑定完成");
            void loadInstances();
          } else if (["expired", "cancelled"].includes(state.status)) {
            setPendingAccessToken("");
            setPairingStatus("配对已失效，请让电脑重新生成二维码");
          }
        })
        .catch(() => undefined);
    }, 1500);
    return () => window.clearInterval(timer);
  }, [pendingAccessToken, pairingID, loadInstances]);
  // 桌面端跟踪配对会话：云端只允许在手机提交校验码之后确认，提前点击必然被
  // 拒绝。让"确认绑定"跟着会话状态启用，用户就不必盲点并对着报错猜原因。
  useEffect(() => {
    if (mobileApp || !pairingID.trim() || pairingConfirmed) return;
    let cancelled = false;
    const poll = () => {
      void api<{ status?: string }>(`/api/remote/pairing/status?pairingId=${encodeURIComponent(pairingID.trim())}`)
        .then((state) => {
          if (cancelled) return;
          const status = String(state?.status || "");
          if (status === "confirmed") {
            setPairingConfirmed(true);
            setPairingReadyForConfirm(false);
            setPairingStatus("已确认绑定，手机可以开始使用");
          } else if (status === "expired" || status === "cancelled") {
            setPairingReadyForConfirm(false);
            setPairingStatus("配对已失效，请重新生成二维码");
          } else if (status === "claimed") {
            setPairingReadyForConfirm(true);
            setPairingStatus("手机已提交校验码，请点击确认绑定");
          } else {
            setPairingReadyForConfirm(false);
          }
        })
        .catch(() => undefined);
    };
    poll();
    const timer = window.setInterval(poll, 2000);
    return () => { cancelled = true; window.clearInterval(timer); };
  }, [mobileApp, pairingID, pairingConfirmed]);
  // 桌面端进入页面时探测一次远程服务状态。
  useEffect(() => {
    if (mobileApp) return;
    void loadAgentStatus();
  }, [mobileApp, loadAgentStatus]);
  // 注册是异步的：Agent 子进程要连上云端并回报凭据后才可用，因此注册后轮询到
  // 就绪或超时为止，让用户看到结果而不是自行猜测。
  useEffect(() => {
    if (!agentEnrollWaiting) return;
    let attempts = 0;
    const timer = window.setInterval(() => {
      attempts += 1;
      void loadAgentStatus().then((ready) => {
        if (ready) {
          setAgentEnrollWaiting(false);
          setAgentEnrollMessage("远程服务已就绪，现在可以生成二维码了。");
          return;
        }
        if (attempts >= 15) {
          setAgentEnrollWaiting(false);
          setAgentEnrollMessage("仍未检测到注册结果，请查看应用数据目录下的 milevia-agent.log 后重试。");
        }
      });
    }, 2000);
    return () => window.clearInterval(timer);
  }, [agentEnrollWaiting, loadAgentStatus]);

  const instance = instances.find((item) => item.instanceId === instanceID);
  const projects = snapshot?.projects || [];
  const project = projects.find((item) => item.id === selectedProject);
  // Keep the agent visible wherever the mobile conversation title is shown.
  // The API title remains untouched; this is presentation-only decoration.
  const conversations = useMemo(() => (project?.conversations || []).map((item) => ({
    ...item,
    title: `${item.title || "未命名会话"} · ${conversationAgentLabel(item.agentId)}`,
    // rawTitle 是**不带**「· Agent」后缀的原名。会话页顶栏只用这个名字：它本来就是页面里最大
    // 的一行字，再缀上「· Claude Code」在 320px 上会被省略号吃掉一截，而"我正跟哪个 Agent 说话"
    // 更适合放在 ⋯ 菜单里（那是会话信息该在的地方），历史列表那边继续用带后缀的 title。
    rawTitle: item.title || "未命名会话",
  })), [project?.conversations]);
  const conversation = conversations.find((item) => item.id === selectedConversation) || conversations.find((item) => item.isCurrent) || conversations[0];
  // 消息与运行记录按时间混排：用户要看的是"什么时候发生了什么"，而不是
  // "对话"和"状态"两个割裂的列表。sort 是稳定的，同一时刻消息在前、状态卡在后。
  const conversationTimeline = useMemo(() => {
    const entries: Array<
      | { kind: "message"; key: string; at: number; message: Message }
      | { kind: "notice"; key: string; at: number; notice: RemoteNotice }
    > = [];
    for (const message of conversation?.messages || []) {
      entries.push({ kind: "message", key: message.id, at: Date.parse(message.createdAt) || 0, message });
    }
    for (const notice of conversation?.notices || []) {
      entries.push({ kind: "notice", key: notice.id, at: Date.parse(notice.createdAt) || 0, notice });
    }
    return entries.sort((left, right) => left.at - right.at);
  }, [conversation]);
  const conversationProcessing = Boolean(conversation && (conversation.status === "running" || processingConversations[conversation.id]));
  const conversationMessageCount = conversation?.messages?.length ?? 0;
  // 流式回复只更新最后一条消息的 content、不改变条数，所以还要盯着它的长度，
  // 否则「贴底跟随」在 AI 正在输出时不会生效。
  const lastMessageLength = conversationMessageCount > 0
    ? (conversation?.messages?.[conversationMessageCount - 1]?.content?.length ?? 0)
    : 0;

  // 消息列表不是独立滚动容器（整页滚动），因此监听 window。
  // 「贴近底部」留 100px 容差：足以吸收地址栏收放带来的视口变化，又不会在用户
  // 轻微上滑阅读时把视口拽回底部。
  useEffect(() => {
    const onScroll = () => {
      const root = document.documentElement;
      stickToBottomRef.current = root.scrollHeight - window.scrollY - window.innerHeight < 100;
    };
    onScroll();
    window.addEventListener("scroll", onScroll, { passive: true });
    window.addEventListener("resize", onScroll);
    return () => {
      window.removeEventListener("scroll", onScroll);
      window.removeEventListener("resize", onScroll);
    };
  }, []);
  // 新消息到达、AI 正在流式输出、或切换会话时，只要用户本就贴着底部就继续跟随；
  // 用户正在向上翻阅历史时不打扰。
  useEffect(() => {
    if (!mobileApp || mobileView !== "conversation") return;
    if (!stickToBottomRef.current) return;
    window.scrollTo({ top: document.documentElement.scrollHeight });
  }, [mobileApp, mobileView, selectedConversation, conversationMessageCount, lastMessageLength, conversationProcessing]);

  // 系统侧滑 / 浏览器返回按钮 / 网页版手势返回：历史已经在后退了，这里只把视图同步回项目列表，
  // **不再动历史**（否则会再多退一层，把用户直接带出应用）。安卓返回键是同一件事的另一条链路，
  // 见下面注册的 backButton 监听。
  // 已知限制（不做对称处理）：用浏览器"前进"回到会话层时不会再自动进入会话视图（手机端没有前进
  // 按钮，桌面端/长按历史列表最多是"少一次反应"）；要对称就得再记一份 selectedProject，收益不抵复杂度。
  useEffect(() => {
    const onPopState = () => {
      if (!conversationHistoryRef.current) return;
      conversationHistoryRef.current = false;
      leaveConversationView();
    };
    window.addEventListener("popstate", onPopState);
    return () => window.removeEventListener("popstate", onPopState);
  }, []);
  // 进程被系统回收后又恢复时，历史里可能留着上一次的会话层标记，而视图已经回到项目列表。
  // 如实把它记成"还没退掉的一层"，这样用户下一次返回就能把它用掉，而不是被空吞一次。
  useEffect(() => {
    conversationHistoryRef.current = window.history.state?.mileviaMobileConversation === true;
  }, []);

  // 安卓物理返回键：与页内「←」保持同一顺序——先关弹层，再退出会话视图，最后最小化应用。
  // 不注册监听时 Capacitor 的默认返回回调会吞掉事件，返回键等于完全失效。
  useEffect(() => {
    backHandlerRef.current = () => {
      // 弹层正在提交时，与弹层内「关闭/取消」按钮的 disabled 规则保持一致：
      // 吞掉返回事件，避免中途丢弃已经在进行的请求。
      if (busy && (editingTask || deletingTask || newConversationProject || confirmShortcut)) return true;
      // ⋯ 菜单是页面里最上层的一小块浮层，返回键先收它，别一步退到项目列表。
      if (headerMenuOpen) { setHeaderMenuOpen(false); return true; }
      // 快捷方式确认框与上面两个任务弹层同样处理。漏掉它的症状实测过：返回键一路走到
      // exitConversationView()，视图退回项目列表，而确认框（backdrop 是 position: fixed）
      // 还浮在项目列表上、「执行」仍然可点 —— 用户会看到一条"在项目列表上执行电脑端命令"
      // 的弹窗，而且那一下会真的发出去。
      if (confirmShortcut) { setConfirmShortcut(null); return true; }
      if (editingTask) { setEditingTask(null); return true; }
      if (deletingTask) { setDeletingTask(null); return true; }
      if (newConversationProject) { cancelNewConversation(); return true; }
      // 取景层有两个渲染条件（scanning / scanError），按扫描态判断会在失败态漏掉它。
      if (scanning || scanError) { closeScanOverlay(); return true; }
      if (tasksOpen) { setTasksOpen(false); return true; }
      if (conversationHistoryOpen) { setConversationHistoryOpen(false); return true; }
      if (pairingExpanded) { setPairingExpanded(false); return true; }
      if (mobileApp && mobileView === "conversation") {
        exitConversationView();
        return true;
      }
      return false;
    };
  });
  useEffect(() => {
    if (!Capacitor.isNativePlatform()) return;
    let listener: PluginListenerHandle | null = null;
    let cancelled = false;
    void CapacitorApp.addListener("backButton", () => {
      // 没有可回退的层级时最小化应用（与页内返回按钮一致，符合 Android 根页面返回习惯）。
      if (!backHandlerRef.current()) void CapacitorApp.minimizeApp().catch(() => undefined);
    }).then((handle) => {
      if (cancelled) { void handle.remove(); return; }
      listener = handle;
    }).catch(() => undefined);
    return () => {
      cancelled = true;
      if (listener) void listener.remove();
    };
  }, []);

  // Normalize an older cached snapshot as well as network responses. This
  // keeps revision comparisons numeric during offline recovery.
  useEffect(() => {
    if (!snapshot || Number.isFinite(snapshot.snapshotRevision)) return;
    setSnapshot((current) => current && !Number.isFinite(current.snapshotRevision)
      ? { ...current, snapshotRevision: 0 }
      : current);
  }, [snapshot]);

  // SSE can be unavailable while snapshots continue to refresh. Clear the
  // local optimistic indicator once a completed assistant reply is present.
  useEffect(() => {
    if (!snapshot || Object.keys(processingConversations).length === 0) return;
    const completedIDs = new Set<string>();
    for (const [conversationID, state] of Object.entries(processingConversations)) {
      const remoteConversation = snapshot.projects.flatMap((item) => item.conversations).find((item) => item.id === conversationID);
      if (!remoteConversation) {
        completedIDs.add(conversationID);
        continue;
      }
      if (["failed", "stopped", "cancelled"].includes(remoteConversation.status)) {
        if (snapshot.snapshotRevision > state.snapshotRevision) completedIDs.add(conversationID);
        continue;
      }
      // A run can finish without an assistant message (for example, a
      // startup failure, cancellation, or service restart). Once a newer
      // durable snapshot says the conversation is no longer running, the
      // indicator must not remain stuck. Keep it while the optimistic user
      // message is still waiting to be persisted so a queued command cannot
      // be cleared by an unrelated snapshot update.
      const pending = pendingMessageRef.current.get(conversationID);
      const hasPendingMessage = Boolean(pending && pending.size > 0);
      if (snapshot.snapshotRevision > state.snapshotRevision && remoteConversation.status !== "running" && !hasPendingMessage) completedIDs.add(conversationID);
    }
    if (completedIDs.size > 0) {
      setProcessingConversations((current) => {
        const next = { ...current };
        completedIDs.forEach((id) => delete next[id]);
        return next;
      });
    }
  }, [snapshot, processingConversations]);
  const taskCount = useMemo(() => projects.reduce((sum, item) => sum + item.tasks.length, 0), [projects]);
  const visibleTasks = useMemo(() => {
    const tasks = project?.tasks || [];
    return taskFilter === "all" ? tasks : tasks.filter((task) => task.status === taskFilter);
  }, [project?.tasks, taskFilter]);
  // 面板副标题：选"全部"时给总数，选了分类时给"该分类 命中 / 总数"——分类胶囊上的
  // 数字与列表实际条数是两处来源，副标题把它们对上，用户一眼能看出筛选到底生效没有。
  const taskFilterSummary = useMemo(() => {
    const total = project?.tasks.length ?? 0;
    if (taskFilter === "all") return `共 ${total} 个任务`;
    const label = taskFilters.find((item) => item.id === taskFilter)?.label || "";
    return `${label} ${visibleTasks.length} / ${total}`;
  }, [project?.tasks, taskFilter, visibleTasks]);

  // Keep the extra task controls synchronized with React state. The task row
  // markup predates these controls, so reconcile the small imperative portion
  // whenever the filtered snapshot or command state changes.
  useEffect(() => {
    if (!tasksOpen) return;
    const timer = window.setTimeout(() => {
      document.querySelectorAll<HTMLElement>(".mobile-task-panel .mobile-task").forEach((row, index) => {
        const task = visibleTasks[index];
        const actions = row.querySelector<HTMLElement>(".mobile-task-actions");
        if (!task || !actions) return;
        const canEdit = task.status === "todo" || task.status === "action_required";
        let edit = actions.querySelector<HTMLButtonElement>("[data-mobile-task-edit]");
        if (canEdit && !edit) {
          edit = document.createElement("button");
          edit.type = "button"; edit.textContent = "编辑"; edit.dataset.mobileTaskEdit = "true";
          actions.append(edit);
        }
        if (edit) {
          edit.hidden = !canEdit;
          edit.disabled = busy || !canEdit;
          edit.onclick = (event) => { event.stopPropagation(); openTaskEditor(task); };
        }
        let remove = actions.querySelector<HTMLButtonElement>("[data-mobile-task-delete]");
        if (!remove) {
          remove = document.createElement("button");
          remove.type = "button"; remove.textContent = "删除"; remove.dataset.mobileTaskDelete = "true"; remove.className = "mobile-task-delete";
          actions.append(remove);
        }
        remove.hidden = false;
        remove.disabled = busy || task.status === "running";
        remove.onclick = (event) => { event.stopPropagation(); setDeletingTask(task); };
      });
    }, 0);
    return () => window.clearTimeout(timer);
  }, [tasksOpen, visibleTasks, taskFilter, busy]);

  // 整页模态层的焦点管理：打开时把焦点搬进面板、把背后的内容标成 inert（不进 Tab 顺序、读屏忽略），
  // 关闭时还给触发它的按钮。少了这一步，键盘 / 读屏用户打开面板后仍在背后那屏里打转，而视觉上
  // 明明已经"进到面板里了"。inert 在不支持它的旧 WebView 上会被忽略，不影响其余行为。
  // 依赖里必须带上"面板会不会被渲染"的那几个条件（project / mobileView / mobileApp），不能只写
  // tasksOpen：面板的渲染条件是 `mobileApp && mobileView === "conversation" && project && tasksOpen`，
  // 一旦 project 消失或视图切回列表，面板会先于 tasksOpen 离场——若 effect 不重跑，cleanup 就不执行，
  // 背景会永久留在 inert 上（整页看得见、点不动）。
  useEffect(() => {
    if (!tasksOpen) return;
    const panel = taskPanelRef.current;
    if (!panel) return;
    // 触发按钮是菜单里的「任务队列」，点它的同一次渲染里菜单就收起了 —— 到这一步 activeElement 已经
    // 变成 document.body。body 不是一个有意义的落点（focus 它等于没聚焦），必须换成 ⋯ 按钮。
    const active = document.activeElement;
    const restoreTo = active instanceof HTMLElement && active !== document.body ? active : headerMenuButtonRef.current;
    const blocked: HTMLElement[] = [];
    const block = (element: Element | null) => {
      // 面板本身、以及包含面板的祖先都不能 inert —— inert 会让整棵子树失效，包括面板自己。
      if (!(element instanceof HTMLElement) || element === panel || element.contains(panel)) return;
      element.inert = true;
      blocked.push(element);
    };
    for (const sibling of Array.from(panel.closest("main")?.children || [])) block(sibling);
    for (const sibling of Array.from(panel.parentElement?.children || [])) block(sibling);
    panel.focus({ preventScroll: true });
    return () => {
      for (const element of blocked) element.inert = false;
      if (restoreTo && document.contains(restoreTo)) restoreTo.focus({ preventScroll: true });
    };
  }, [tasksOpen, mobileApp, mobileView, project?.id]);

  const showMobilePairing = mobileApp && mobileView === "projects" && (!token.trim() || instances.length === 0 || pairingExpanded);
  // 取景层的画面来源由平台决定：原生是"WebView 之下的摄像头"（页面里画不了，只能靠清底透出来），
  // Web 是页面内的 <video>。前者决定 <video> 是否渲染，后者与 scanLiveNative 一起决定 data-backdrop。
  const nativeScanPlatform = Capacitor.isNativePlatform();
  // 只有"原生 + 正在扫描"这一种组合背后真的有摄像头（data-backdrop="camera"）。其余情况
  // （Web 分支、以及相机起不来之后的报错态）背后没有画面，取景层必须自己铺一层不透明底
  // —— 否则清掉根元素底色之后会直接露出浏览器画布。
  const scanLiveNative = scanning && nativeScanPlatform;

  useEffect(() => {
    if (!selectedProject || projects.some((item) => item.id === selectedProject)) return;
    setSelectedProject("");
    setSelectedConversation("");
    exitConversationView();
  }, [projects, selectedProject]);
  useEffect(() => {
    setMessageDraft("");
    // 技能引用与草稿同一条规则：换会话 / 换项目就清掉。引用属于原来那个会话，
    // 留着会变成"新会话的输入框上挂着上一个会话的技能"，而且它比草稿更容易被忽略
    // （草稿至少还看得出内容不对）。
    setSkillRefs([]);
    setConversationHistoryOpen(false);
    setComposerToolsOpen(false);
    // 换会话（含从历史会话里点另一条）时，顶栏 ⋯ 菜单里的「历史会话 N / 任务队列 N」都是旧项目的
    // 计数，留着会显示成另一条会话的数字 —— 一并收起，让用户重新看一眼当前值。
    setHeaderMenuOpen(false);
  }, [selectedProject, selectedConversation]);
  useEffect(() => {
    selectedConversationRef.current = selectedConversation;
  }, [selectedConversation]);
  // 自增高上限必须与 CSS 的 max-height 一致（两者分叉会让输入框在某一侧被裁）。
  // ＋2 是取整余量：scrollHeight 取整后可能比真实内容矮不到 1px，直接当高度用会让最后一行贴边。
  useEffect(() => {
    const input = messageInputRef.current;
    if (!input) return;
    input.style.height = "auto";
    input.style.height = `${Math.min(input.scrollHeight + 2, 140)}px`;
  }, [messageDraft]);
  // 面板外的任意按下都收起面板（输入条与面板同属一个外壳，点它们不算"外面"）。
  useEffect(() => {
    if (!composerToolsOpen) return;
    function onPointerDown(event: PointerEvent) {
      const target = event.target;
      if (target instanceof Element && target.closest(".mobile-composer-shell")) return;
      setComposerToolsOpen(false);
    }
    document.addEventListener("pointerdown", onPointerDown);
    return () => document.removeEventListener("pointerdown", onPointerDown);
  }, [composerToolsOpen]);
  // 顶栏 ⋯ 菜单：面板外的任意按下、或按 Esc 都收起（与输入条工具面板同一套规则）。
  useEffect(() => {
    if (!headerMenuOpen) return;
    function onPointerDown(event: PointerEvent) {
      const target = event.target;
      if (target instanceof Element && target.closest(".mobile-header-menu")) return;
      setHeaderMenuOpen(false);
    }
    function onKeyDown(event: KeyboardEvent) {
      if (event.key !== "Escape") return;
      setHeaderMenuOpen(false);
      // 收起后焦点必须回到 ⋯：不还焦点，键盘用户会掉在 document.body 上，得从头 Tab 一遍。
      headerMenuButtonRef.current?.focus();
    }
    document.addEventListener("pointerdown", onPointerDown);
    document.addEventListener("keydown", onKeyDown);
    return () => {
      document.removeEventListener("pointerdown", onPointerDown);
      document.removeEventListener("keydown", onKeyDown);
    };
  }, [headerMenuOpen]);
  // 会话内容的底部预留要跟着输入条实际高度走：写死一个数值在多行输入（输入条能长到 158px）
  // 或打开工具面板时会失效，最后几条消息被压住。这里把实测高度写进 CSS 变量供样式消费。
  useEffect(() => {
    const shell = composerShellRef.current;
    if (!shell) return;
    let lastHeight = 0;
    const apply = () => {
      const height = Math.ceil(shell.getBoundingClientRect().height);
      document.documentElement.style.setProperty("--mobile-composer-height", `${height}px`);
      // 输入条长高（多行输入、展开工具面板）时，文档底部预留虽然跟着变大，但浏览器会保持
      // 原来的 scrollY —— 于是最后几条消息被推到输入条底下压住。只在"本来就贴着底"时
      // 补一次贴底，正在往上翻的用户不打扰。
      if (height > lastHeight && stickToBottomRef.current) {
        window.scrollTo({ top: document.documentElement.scrollHeight });
      }
      lastHeight = height;
    };
    apply();
    const observer = new ResizeObserver(apply);
    observer.observe(shell);
    return () => {
      observer.disconnect();
      document.documentElement.style.removeProperty("--mobile-composer-height");
    };
  }, [mobileView, project?.id]);

  // 「刷新」按钮。三步都必须做，少一步用户就看不出有没有生效：
  //   ① 立刻把按钮置为"转圈 + 禁用"，让点击当场有反应；
  //   ② 真的回云端取一份（loadSnapshot 的 force 分支绕过 revision 短路）；
  //   ③ 把结果写成一条带时刻的状态条——成功/无变化/失败各有各的说法，不再是一点动静都没有。
  async function refreshNow() {
    // 双击只当一次：重复请求不但白费流量，两条重叠的反馈还会互相覆盖。
    if (refreshingRef.current) return;
    refreshingRef.current = true;
    setRefreshing(true);
    setRefreshStatus({ state: "refreshing", message: "正在刷新…", at: null });
    try {
      const list = await loadInstances();
      if (list.outcome === "failed") {
        // 带上真实原因（网络不通 / 令牌失效 / 云端返回异常），别再写死一句"没有连上云端"。
        setRefreshStatus({ state: "failed", message: `刷新失败：${list.reason || "请稍后重试"}`, at: new Date() });
        return;
      }
      // 与 loadInstances 选实例的规则保持一致（没变就留着，失效了就退回第一台）：
      // 两处规则一旦分叉，刷新就会去请求一台已经不是当前选中的电脑。
      // superseded 时拿不到这一趟的列表（结果由抢答的那次请求落地），退回当前选中的实例继续。
      const known = list.outcome === "ok" ? list.instances : null;
      const target = known
        ? (known.some((item) => item.instanceId === instanceID) ? instanceID : known[0]?.instanceId || "")
        : instanceID;
      if (!target) {
        if (list.outcome === "superseded") {
          // 列表这一趟没落地、当前也没有选中的实例：接下来的自动同步会把列表与快照补齐，
          // 这里既不能报成功也不能报失败。
          setRefreshStatus({ state: "unchanged", message: "电脑列表已更新，正在同步数据…", at: new Date() });
          return;
        }
        setRefreshStatus({
          state: "failed",
          message: token.trim() ? "没有找到在线的电脑，请确认电脑端已启动并联网" : "尚未配对电脑，请先扫码配对",
          at: new Date(),
        });
        return;
      }
      const result = await loadSnapshot({ force: true, instanceId: target });
      if (result.outcome === "updated") {
        setRefreshStatus({ state: "success", message: `已刷新 · ${result.snapshot?.projects.length ?? 0} 个项目`, at: new Date() });
      } else if (result.outcome === "unchanged") {
        setRefreshStatus({ state: "unchanged", message: "已是最新，电脑端没有新的变化", at: new Date() });
      } else if (result.outcome === "failed") {
        setRefreshStatus({ state: "failed", message: `刷新失败：${result.reason || "请稍后重试"}`, at: new Date() });
      } else {
        // skipped：这次请求被更新的请求取代了，界面会由那一次的结果收尾。
        setRefreshStatus({ state: "unchanged", message: "已更新电脑列表，正在同步数据…", at: new Date() });
      }
    } catch {
      // loadInstances / loadSnapshot 内部都各自兜住了异常，这里是最后一道保险：
      // 万一有意外抛出也必须把状态条收尾，否则按钮会永远停在"正在刷新…"的转圈状态——
      // 那比原来的"点了没反应"更糟。
      setRefreshStatus({ state: "failed", message: "刷新失败：请稍后重试", at: new Date() });
    } finally {
      refreshingRef.current = false;
      setRefreshing(false);
    }
  }

  function saveToken(event: FormEvent) {
    event.preventDefault();
    const nextToken = token.trim();
    localStorage.setItem("milevia.cloud.token", nextToken);
    setInstances([]);
    setInstanceID("");
    setSnapshot(null);
    setSelectedProject("");
    setSelectedConversation("");
    exitConversationView();
    setPairingExpanded(false);
    void loadInstances();
  }

  async function enableMobileNotifications() {
    if (typeof Notification === "undefined") return;
    try {
      const permission = await Notification.requestPermission();
      setNotificationPermission(permission);
    } catch {
      setError("无法请求通知权限，请在浏览器或系统设置中允许通知");
    }
  }

  async function claimPairing(pairingIDValue = pairingID, pairingCodeValue = pairingCode) {
    if (!pairingIDValue.trim() || !/^\d{6}$/.test(pairingCodeValue.trim())) {
      setError("二维码缺少有效的配对信息，请让电脑重新生成二维码");
      return;
    }
    setBusy(true); setError("");
    try {
      const result = await cloud<{ instanceId: string; status: string }>(`/v1/pairings/${encodeURIComponent(pairingIDValue.trim())}/claim`, { method: "POST", body: JSON.stringify({ code: pairingCodeValue.trim() }) });
      const accessToken = (result as { accessToken?: string }).accessToken;
      if (accessToken) setPendingAccessToken(accessToken);
      setPairingStatus("已扫描，等待电脑确认");
    } catch (cause) { setError(cause instanceof Error ? cause.message : "配对失败"); }
    finally { setBusy(false); }
  }

  // 「扫描二维码」的唯一入口。五次复位必须写在同一个地方：少复位一项，复用上一次的
  // 结果就会串到这一次上（最典型的是 scanAccepted 残留 —— 相机起来后会对任何一张
  // 二维码都判"已经接受过"，看起来像扫码没反应）。
  function beginPairingScan() {
    scanAccepted.current = false;
    setScanError("");
    setScanNeedsSettings(false);
    setScanTorchAvailable(false);
    setScanTorchOn(false);
    setScanning(true);
  }

  // 关掉取景层。scanError 必须一起清掉：它是取景层的第二个渲染条件（失败态要留在层里），
  // 不清就会关不掉。浮层清退的两处（返回键 / leaveConversationView）也走这一个出口。
  //
  // 这里**同步**撤一次清底类名，不等 effect 清理：类名归上面那个 effect 拥有，而它的清理是
  // passive 的（可能落在下一次绘制之后），中间那一帧会出现"取景层已摘掉、页面还被
  // visibility: hidden 盖着"的空屏 —— 原生分支上还会再多闪一下摄像头画面。
  // effect 里那句 setScannerActive 仍是权威（覆盖超时/卸载等路径），这里只是把它提前一拍。
  function closeScanOverlay() {
    setScannerActive(false);
    setScanning(false);
    setScanError("");
  }

  // 「打开系统设置」按钮只在原生分支出现（scanNeedsSettings 只由原生分支置位）——
  // 浏览器里"系统设置"没有对应入口，提示语本身已经写了该去浏览器站点设置里改。
  // 这里的 catch 因此是**防御性**的：真要在 Web 上调到 openSettings()，插件会抛
  // "This method is not implemented on web."，不该把这句英文丢给用户。
  async function openScannerSettings() {
    try {
      await BarcodeScanner.openSettings();
    } catch {
      setScanError("无法自动打开系统设置，请到「设置 → 应用 → Milevia → 权限」中允许使用摄像头");
    }
  }

  // 手电筒开关。判据只能用"回读到的真实状态"，**不能**用"调用有没有抛异常"：
  // 插件实现里 `enableTorch()` / `disableTorch()` 在相机尚未就绪时（`camera == null`，
  // 见 BarcodeScanner.java）是**静默 return** 的 —— 不抛异常、正常 resolve，但灯没亮。
  // 按"没抛就算成功"来置状态，界面就会显示"手电筒已打开"而实际是黑的。
  // 所以用 `toggleTorch()` 发声、再 `isTorchEnabled()` 回读；回读与切换前相同即说明这次
  // 切换没生效（相机还没就绪），此时把按钮收起来 —— 与"设备没有闪光灯"一样处理，
  // 不留一个点了没反应的开关。
  async function toggleScanTorch() {
    try {
      await BarcodeScanner.toggleTorch();
      const { enabled } = await BarcodeScanner.isTorchEnabled();
      if (Boolean(enabled) === scanTorchOn) {
        setScanTorchAvailable(false);
        setScanTorchOn(false);
        return;
      }
      setScanTorchOn(Boolean(enabled));
    } catch {
      // 真抛异常（相机已停等）走这里，处理同上。
      setScanTorchAvailable(false);
      setScanTorchOn(false);
    }
  }

  useEffect(() => {
    const { pairingID: initialPairingID, code: initialPairingCode } = initialPairing.current;
    if (!mobileApp || initialPairing.current.attempted || !initialPairingID || !/^\d{6}$/.test(initialPairingCode)) return;
    initialPairing.current.attempted = true;
    void claimPairing(initialPairingID, initialPairingCode);
  }, [mobileApp]);

  async function claimPairingByCode(event: FormEvent) {
    event.preventDefault();
    const code = manualPairingCode.trim();
    if (!/^\d{6}$/.test(code)) {
      setError("请输入 6 位校验码");
      return;
    }
    setBusy(true); setError("");
    try {
      const result = await cloud<{ pairingId: string; instanceId: string; status: string; accessToken?: string }>("/v1/pairings/claim", { method: "POST", body: JSON.stringify({ code }) });
      setPairingID(result.pairingId || "");
      setPairingCode(code);
      if (result.accessToken) setPendingAccessToken(result.accessToken);
      setPairingStatus("校验码已提交，等待电脑确认");
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "校验码配对失败");
    } finally {
      setBusy(false);
    }
  }

  async function confirmDesktopPairing() {
    if (!pairingID.trim()) return;
    setBusy(true); setError("");
    try {
      await api(`/api/remote/pairing/confirm`, { method: "POST", body: JSON.stringify({ pairingId: pairingID.trim() }) });
      setPairingConfirmed(true);
      setPairingReadyForConfirm(false);
      setPairingStatus("已确认绑定，手机可以开始使用");
    } catch (cause) { setError(cause instanceof Error ? cause.message : "确认绑定失败"); }
    finally { setBusy(false); }
  }

  // 注册是电脑端的一次性部署动作：令牌只注入本次 Agent 子进程，注册成功后凭据
  // 由 DPAPI 保存，令牌既不落盘也不会进入后续启动环境。
  async function enrollRemoteAgent(event: FormEvent) {
    event.preventDefault();
    const token = agentEnrollToken.trim();
    if (!token) return;
    if (!isDesktop()) {
      setAgentEnrollMessage("请在 Milevia 桌面应用中完成注册。");
      return;
    }
    setAgentEnrollBusy(true);
    setAgentEnrollMessage("");
    try {
      await invoke("enroll_remote_agent", { enrollmentToken: token });
      setAgentEnrollToken("");
      setAgentEnrollMessage("已提交注册，正在等待电脑端连接云端……");
      setAgentEnrollWaiting(true);
    } catch (cause) {
      setAgentEnrollMessage(cause instanceof Error ? cause.message : String(cause));
    } finally {
      setAgentEnrollBusy(false);
    }
  }

  async function createDesktopPairing() {
    setBusy(true); setError("");
    try {
      const value = await api<{ pairingId: string; code: string; pairingURL?: string }>("/api/remote/pairing", { method: "POST" });
      const qrURL = pairingURLWithCode(value.pairingURL, value.pairingId || "");
      setPairingID(value.pairingId || "");
      setPairingCode(value.code || "");
      setPairingURL(qrURL);
      setPairingReadyForConfirm(false);
      setPairingConfirmed(false);
      if (qrURL) {
        setPairingStatus("二维码已生成，有效期 5 分钟");
      } else {
        // 云端未配置公网地址时只能使用校验码。明确说出来，而不是留一块
        // 空白让用户反复点击。
        setPairingStatus("已生成校验码，请在手机上输入下方 6 位数字");
        setError("云端未配置公网地址（MILEVIA_CLOUD_APP_URL），二维码不可用；请改用校验码配对。");
      }
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "无法生成配对二维码");
      // 最常见的原因是 Agent 还没注册，顺手刷新一次状态以便页面给出正确引导。
      void loadAgentStatus();
    }
    finally { setBusy(false); }
  }

  // 解除当前手机的绑定：先请云端吊销这台设备在该实例上的令牌，再清理本地
  // 状态。云端不可达时仍然完成本地清理——用户的意图是让这台手机停止访问，
  // 而不是修好一次网络请求，所以本地解绑不能依赖请求成功。
  async function unbindDevice() {
    const target = instanceID.trim();
    setBusy(true); setError("");
    try {
      if (target) {
        await cloud(`/v1/instances/${encodeURIComponent(target)}/revoke`, {
          method: "POST",
          body: JSON.stringify({ scope: "mobile" }),
        });
      }
    } catch {
      // 忽略云端错误：本地已经解除绑定，用户随时可以重新配对。
    } finally {
      localStorage.removeItem("milevia.cloud.token");
      setToken(""); setInstances([]); setInstanceID(""); setSnapshot(null);
      setSelectedProject(""); setSelectedConversation("");
      setPairingURL(""); setPairingCode(""); setPairingID("");
      setPairingStatus("已解除绑定，请重新配对");
      setPairingExpanded(true);
      setBusy(false);
    }
  }

  async function createTask(event: FormEvent) {
    event.preventDefault();
    if (!project || !title.trim()) return;
    setBusy(true); setError("");
    try {
	  const accepted = await cloud<AcceptedCommand>(`/v1/instances/${encodeURIComponent(instanceID)}/commands`, {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey() },
        body: JSON.stringify({ type: "task.create", projectId: project.id, payload: { title: title.trim(), description: description.trim(), priority: "normal" } }),
      });
	  setCommandState(accepted);
	  const finalState = await waitForCommand(accepted.commandId);
	  if (!finalState || finalState.status !== "completed") {
	    setError(finalState ? "任务创建失败，请检查电脑端 Agent 状态" : "任务仍在处理中，请稍后刷新查看结果");
	    return;
	  }
      setTitle(""); setDescription("");
      await loadSnapshot();
    } catch (cause) { setError(cause instanceof Error ? cause.message : "创建任务失败"); }
    finally { setBusy(false); }
  }

  async function sendTaskCommand(taskID: string, type: string, payload: Record<string, unknown> = {}): Promise<boolean> {
    setBusy(true); setError("");
    try {
      const commandPayload = type === "task.review" ? { action: "accept", ...payload } : payload;
      const accepted = await cloud<AcceptedCommand>(`/v1/instances/${encodeURIComponent(instanceID)}/commands`, { method: "POST", headers: { "Idempotency-Key": idempotencyKey() }, body: JSON.stringify({ type, taskId: taskID, payload: commandPayload }) });
      setCommandState(accepted);
      const finalState = await waitForCommand(accepted.commandId);
      if (!finalState || finalState.status !== "completed") {
        setError(finalState ? "任务操作失败，请检查任务状态" : "任务操作仍在处理中，请稍后刷新");
        return false;
      }
      await loadSnapshot();
      return true;
    } catch (cause) { setError(cause instanceof Error ? cause.message : "命令发送失败"); }
    finally { setBusy(false); }
    return false;
  }

  function openTaskEditor(task: Task) {
    setEditingTask(task);
    setEditTitle(task.title || "");
    setEditDescription(task.description || "");
    setEditPriority(task.priority || "normal");
  }

  async function saveTaskEdit(event: FormEvent) {
    event.preventDefault();
    if (!editingTask || !editTitle.trim()) return;
    if (!editDescription.trim()) {
      setError("任务描述不能为空");
      return;
    }
    if (await sendTaskCommand(editingTask.id, "task.update", { title: editTitle.trim(), description: editDescription.trim(), priority: editPriority })) setEditingTask(null);
  }

  async function confirmTaskDelete() {
    if (!deletingTask) return;
    if (await sendTaskCommand(deletingTask.id, "task.delete")) setDeletingTask(null);
  }

  // 把一段文本放进草稿、收起面板、把焦点还给输入框 —— 手机上没有"先关面板、再点输入框"这个动作。
  // 等新草稿渲染进 textarea 之后再聚焦，否则光标会落在旧内容的末尾。
  function fillComposer(text: string) {
    setMessageDraft(text);
    setComposerToolsOpen(false);
    requestAnimationFrame(() => messageInputRef.current?.focus());
  }

  // 技能：与桌面端 useSkill 一样只挂一颗引用胶囊，**不碰用户已经写好的草稿**
  // （旧实现是直接调 fillComposer 把整段引用覆盖进输入框）。
  // 技能本身只是文件系统上的 SKILL.md，手机端不可能去读它，展开成"名称 + 描述"那句指令
  // 推迟到发送那一刻，见 composeSkillMessage。
  function fillSkill(skill: RemoteSkill) {
    setSkillRefs((current) => current.some((item) => item.name === skill.name && item.source === skill.source) ? current : [...current, skill]);
    setComposerToolsOpen(false);
    requestAnimationFrame(() => messageInputRef.current?.focus());
  }

  // 移除一颗引用胶囊。同名技能只可能在同一个来源出现一次，所以用 (名字, 来源) 定位。
  function removeSkillRef(skill: RemoteSkill) {
    setSkillRefs((current) => current.filter((item) => !(item.name === skill.name && item.source === skill.source)));
  }

  // 快捷方式入口。fill 只让电脑端渲染（模板里的 ${project.path} 这类变量手机端没有），
  // 把渲染结果填进手机输入框；run / confirm 直接在电脑端执行。
  // confirm 先在手机上弹确认框：这条命令会在电脑上跑，误触的代价不在这一屏。
  async function applyShortcut(shortcut: RemoteShortcut) {
    if (!conversation || busy || shortcutBusy) return;
    const action = shortcutAction(shortcut);
    if (action === "confirm") { setComposerToolsOpen(false); setConfirmShortcut(shortcut); return; }
    await sendShortcutCommand(shortcut.id, action);
  }

  // 走云端命令通道触发电脑端同一条捷径路径：
  //   fill           → 电脑端 /preview 渲染后把正文回传，手机端填入自己的输入框
  //   run / confirm  → 电脑端 /run 真的执行（渲染规则、命令包装、运行审计全在电脑端，
  //                    两端不可能跑出不同结果）
  async function sendShortcutCommand(shortcutID: string, action: "fill" | "run" | "confirm") {
    if (!conversation || shortcutBusy) return;
    setShortcutBusy(shortcutID);
    setError("");
    try {
      const accepted = await cloud<AcceptedCommand>(`/v1/instances/${encodeURIComponent(instanceID)}/commands`, {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey() },
        body: JSON.stringify({ type: "conversation.shortcut", payload: { conversationId: conversation.id, shortcutId: shortcutID, action } }),
      });
      setCommandState(accepted);
      const finalState = await waitForCommand(accepted.commandId);
      if (!finalState || finalState.status !== "completed") {
        setError(finalState ? `快捷方式执行失败：${commandFailureDetail(finalState.result) || "请检查电脑端 Agent 状态"}` : "快捷方式仍在处理中，请稍后重试");
        return;
      }
      if (action === "fill") {
        const content = shortcutContentFromCommandResult(finalState.result);
        if (!content) {
          setError("快捷方式没有可填入的内容");
          return;
        }
        fillComposer(content);
        return;
      }
      setComposerToolsOpen(false);
      // 执行类的捷径会在电脑端产生新消息/状态，拉一次快照让手机端跟上。
      void loadSnapshot();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "快捷方式执行失败");
    } finally {
      setShortcutBusy("");
    }
  }

  async function confirmShortcutRun() {
    const shortcut = confirmShortcut;
    if (!shortcut) return;
    setConfirmShortcut(null);
    await sendShortcutCommand(shortcut.id, "confirm");
  }

  // 软键盘上没有顺手的 Shift+Enter（Enter 已经在 onKeyDown 里被当成发送），
  // 所以"换行"必须留一个显式入口，否则手机上根本写不出多行消息。
  function insertComposerLineBreak() {
    const input = messageInputRef.current;
    const start = input?.selectionStart ?? messageDraft.length;
    const end = input?.selectionEnd ?? start;
    setMessageDraft(`${messageDraft.slice(0, start)}\n${messageDraft.slice(end)}`);
    setComposerToolsOpen(false);
    const caret = start + 1;
    requestAnimationFrame(() => {
      const element = messageInputRef.current;
      if (!element) return;
      element.focus();
      element.setSelectionRange(caret, caret);
    });
  }

  async function sendConversationMessage(event: FormEvent) {
    event.preventDefault();
    // draft = 用户自己在输入框里写的正文；content = 拼上技能引用后真正上云的那条消息。
    // 所有"写回输入框"的路径都只能用 draft —— 否则一次发送失败就会把整段技能描述重新灌回输入框。
    const draft = messageDraft.trim();
    if (!conversation || (!draft && skillRefs.length === 0)) return;
    const content = composeSkillMessage(draft, skillRefs);
    const conversationID = conversation.id;
    const clientRequestId = idempotencyKey();
    const createdAt = new Date().toISOString();
    const optimisticID = `pending-${clientRequestId}`;
    // 发送时无条件恢复贴底跟随：键盘弹出会因 adjustResize 缩小视口、把「贴底」
    // 判定顶掉，若不在这里重置，用户发完消息不会自动跟随自己的新消息与回复。
    stickToBottomRef.current = true;
    const pending = pendingMessageRef.current.get(conversationID) || new Map<string, PendingMessage>();
    pending.set(clientRequestId, { requestId: clientRequestId, content, createdAt });
    pendingMessageRef.current.set(conversationID, pending);
    markConversationProcessing(conversationID, snapshot?.snapshotRevision ?? -1);
    setSnapshot((current) => current ? { ...current, projects: current.projects.map((item) => ({ ...item, conversations: item.conversations.map((entry) => entry.id === conversationID && !entry.messages.some((message) => message.id === optimisticID) ? { ...entry, messages: [...entry.messages, { id: optimisticID, role: "user", content, createdAt }] } : entry) })) } : current);
    setMessageDraft("");
    // 引用与草稿一起乐观清空；下面每条失败回填分支都会把胶囊一并放回去。
    setSkillRefs([]);
    setBusy(true); setError("");
    try {
      const accepted = await cloud<{ commandId: string; status: string }>(`/v1/instances/${encodeURIComponent(instanceID)}/commands`, {
        method: "POST",
        headers: { "Idempotency-Key": clientRequestId },
        body: JSON.stringify({ type: "conversation.message", payload: { conversationId: conversationID, content, clientRequestId } }),
      });
      setCommandState(accepted);
      while (pending.size > 50) {
        const oldest = pending.keys().next().value;
        if (!oldest) break;
        pending.delete(oldest);
      }
      void loadSnapshot();
      void waitForCommand(accepted.commandId).then((finalState) => {
        if (finalState?.status === "completed") {
          // The optimistic entry and SSE event are already visible. One
          // background snapshot reconciles status/history without a polling
          // storm when the Agent uploads its next snapshot.
          void loadSnapshot();
          return;
        }
        const stillOnConversation = selectedConversationRef.current === conversationID;
        // 回填只放"用户自己写的东西"：正文回输入框、技能回胶囊。
        // 灌 content 会把整段技能描述重新铺满输入框，等于把这次修复原地撤销。
        const restoreDraft = () => {
          setMessageDraft((current) => current.trim() ? current : draft);
          setSkillRefs((current) => current.length > 0 ? current : skillRefs);
        };
        if (finalState?.status !== "completed") {
          clearConversationProcessing(conversationID);
          const pendingForConversation = pendingMessageRef.current.get(conversationID);
          pendingForConversation?.delete(clientRequestId);
          if (pendingForConversation?.size === 0) pendingMessageRef.current.delete(conversationID);
          setSnapshot((current) => current ? { ...current, projects: current.projects.map((item) => ({ ...item, conversations: item.conversations.map((entry) => entry.id === conversationID ? { ...entry, messages: entry.messages.filter((message) => message.id !== optimisticID) } : entry) })) } : current);
        }
        const anotherMessagePending = (pendingMessageRef.current.get(conversationID)?.size || 0) > 0;
        if (finalState && stillOnConversation && !anotherMessagePending) {
          restoreDraft();
          const detail = commandFailureDetail(finalState.result);
          setError(`消息执行失败：${detail || "输入内容仍保留在输入框"}`);
        } else if (!finalState && !anotherMessagePending) {
          if (stillOnConversation) restoreDraft();
          setError("消息状态暂时无法确认，输入内容仍保留在输入框");
        }
      });
    } catch (cause) {
      clearConversationProcessing(conversationID);
      pending.delete(clientRequestId);
      if (pending.size === 0) pendingMessageRef.current.delete(conversationID);
      setSnapshot((current) => current ? { ...current, projects: current.projects.map((item) => ({ ...item, conversations: item.conversations.map((entry) => entry.id === conversationID ? { ...entry, messages: entry.messages.filter((message) => message.id !== optimisticID) } : entry) })) } : current);
      setMessageDraft(draft);
      setSkillRefs(skillRefs);
      setError(cause instanceof Error ? cause.message : "消息发送失败");
    }
    finally { setBusy(false); }
  }

  async function createConversationForProject(projectValue: Project, agentId?: "claude-code" | "codex") {
    if (!agentId) {
      openNewConversation(projectValue);
      return;
    }
    const previousIDs = new Set((projectValue.conversations || []).map((item) => item.id));
    creatingProjectRef.current = projectValue.id;
    setBusy(true);
    setError("");
    try {
      const accepted = await cloud<{ commandId: string; status: string }>(`/v1/instances/${encodeURIComponent(instanceID)}/commands`, {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey() },
        body: JSON.stringify({ type: "conversation.create", projectId: projectValue.id, payload: { agentId } }),
      });
      setCommandState(accepted);
      const finalState = await waitForCommand(accepted.commandId);
      if (finalState?.status !== "completed") {
        const detail = commandFailureDetail(finalState?.result);
        setError(finalState ? `无法创建会话：${detail || "请检查电脑端 Agent 状态"}` : "创建会话仍在处理中，请稍后刷新");
        return;
      }
      const preferredConversationID = conversationIDFromCommandResult(finalState.result);
      const returnedConversation = conversationFromCommandResult(finalState.result);
      const created = returnedConversation || projectValue.conversations?.find((item) => item.id === preferredConversationID)
        || projectValue.conversations?.find((item) => !previousIDs.has(item.id));
      if (!created || (preferredConversationID ? created.id !== preferredConversationID : previousIDs.has(created.id))) {
        setError("会话已创建，但同步尚未完成，请稍后刷新");
        return;
      }
      if (returnedConversation) {
        setSnapshot((current) => current ? {
          ...current,
          projects: current.projects.map((item) => item.id !== projectValue.id ? item : {
            ...item,
            conversations: [returnedConversation, ...item.conversations.filter((entry) => entry.id !== returnedConversation.id).map((entry) => ({ ...entry, isCurrent: false }))],
          }),
        } : current);
      }
      setSelectedProject(projectValue.id);
      setSelectedConversation(created?.id || "");
      setTasksOpen(false);
      setPairingExpanded(false);
      enterConversationView();
      setNewConversationProject(null);
      // Reconcile project/task metadata in the background. The returned
      // conversation is already sufficient to render and send immediately.
      void loadSnapshot();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "无法创建会话");
    } finally {
      if (creatingProjectRef.current === projectValue.id) creatingProjectRef.current = "";
      setBusy(false);
    }
  }

  async function openMobileProject(projectValue: Project) {
    setSelectedProject(projectValue.id);
    // 进入项目时强制跟到底部，避免带着上一个会话的滚动位置。
    stickToBottomRef.current = true;
    setTasksOpen(false);
    setPairingExpanded(false);
    const existingConversation = projectValue.conversations?.find((entry) => entry.isCurrent) || projectValue.conversations?.[0];
    if (existingConversation) {
      setSelectedConversation(existingConversation.id);
      enterConversationView();
      return;
    }
    setNewConversationAgent("claude-code");
    setNewConversationProject(projectValue);
  }

  function openNewConversation(projectValue: Project) {
    setNewConversationAgent("claude-code");
    setNewConversationProject(projectValue);
  }

  function cancelNewConversation() {
    if (!busy) setNewConversationProject(null);
  }

  async function waitForCommand(commandID: string) {
    const deadline = Date.now() + 30_000;
    while (Date.now() < deadline) {
      const remaining = deadline - Date.now();
      if (remaining <= 0) break;
      const requestController = new AbortController();
      const requestTimeout = window.setTimeout(() => requestController.abort(), Math.min(remaining, cloudRequestTimeoutMs));
      try {
        const response = await cloud<CommandState & { command?: { commandId?: string } }>(`/v1/commands/${encodeURIComponent(commandID)}`, { signal: requestController.signal });
        const state = normalizeCommandState(response, commandID);
        setCommandState((current) => mergeCommandState(current, state));
        if (terminalCommandStatuses.includes(state.status)) return state;
      } catch {
        // A transient mobile network failure must not be treated as command
        // failure. Keep polling until the bounded wait expires.
      } finally {
        window.clearTimeout(requestTimeout);
      }
      await new Promise((resolve) => window.setTimeout(resolve, Math.min(500, Math.max(0, deadline - Date.now()))));
    }
    return null;
  }

  // 只切视图、不动历史：给"历史已经退过了"的那条链路用。
  function leaveConversationView() {
    setMobileView("projects");
    setTasksOpen(false);
    // 侧滑返回会走 popstate → 这里：视图退到项目列表时，压在上面的弹窗必须一起关掉，
    // 否则回到项目列表后还挂着一个"选会话"弹窗（任务抽屉 / 顶栏 ⋯ 菜单 / 快捷方式确认框同理）。
    setConversationHistoryOpen(false);
    setHeaderMenuOpen(false);
    // 确认框尤其不能漏：它的 backdrop 是 position: fixed，会直接盖在项目列表上，
    // 而里面的「执行」是真会往电脑端发命令的（实测：漏掉这一行时，退出视图后仍能点执行）。
    setConfirmShortcut(null);
    // 工具面板也要收：它渲染在会话块内部，退出视图后看不见，但状态还在 —— 重新进入
    // **同一个**项目时 selectedProject/selectedConversation 都没变，下面那个清理 effect
    // 不会重跑，面板就会自己冒出来。
    setComposerToolsOpen(false);
    // 取景层同样是 position: fixed 的整层浮层，侧滑返回时必须在**这里**收掉：popstate
    // 链路不走 backHandlerRef，漏掉它就会带着一个"正在扫码"的整层浮层退回项目列表，
    // 而且摄像头一直开着。
    closeScanOverlay();
  }

  // 进入项目会话视图：压一层历史，让系统侧滑/浏览器返回键有东西可退。
  function enterConversationView() {
    if (!conversationHistoryRef.current) {
      const current = window.history.state;
      const base = current && typeof current === "object" ? current : {};
      // react-router 把 { usr, key, idx } 存在 history.state 里，压历史时必须保留它并把 idx 递增，
      // 否则路由按 idx 差值计算"退了几步"会算错。标记位只用于识别"这一层是我们压的"。
      window.history.pushState({
        ...base,
        ...(typeof base.idx === "number" ? { idx: base.idx + 1 } : {}),
        mileviaMobileConversation: true,
      }, "");
      conversationHistoryRef.current = true;
    }
    setMobileView("conversation");
  }

  // 离开项目会话视图（页内「←」、安卓返回键、项目消失、重新配对）：视图立刻切回项目列表，
  // 同时把我们压的那层历史退掉。注意"退历史"只在这里做——系统侧滑触发的那次 popstate 里
  // 历史已经退过了，再退一次会把用户直接带出应用。
  function exitConversationView() {
    leaveConversationView();
    if (conversationHistoryRef.current) {
      conversationHistoryRef.current = false;
      window.history.back();
    }
  }

  function goBack() {
    if (mobileApp && mobileView === "conversation") {
      exitConversationView();
      setPairingExpanded(false);
      return;
    }
    if (window.history.length > 1 && !Capacitor.isNativePlatform()) {
      navigate(-1);
    } else if (Capacitor.isNativePlatform()) {
      void CapacitorApp.minimizeApp().catch(() => undefined);
    } else if (!Capacitor.isNativePlatform()) {
      navigate("/");
    }
  }

  // 会话视图专属的顶栏内容：标题换成**会话名**（用户此刻关心的是哪条对话，不是哪个项目），
  // 三个会话入口与刷新收进 ⋯ 菜单。未完成任务数做成 ⋯ 上的小角标 —— 它是"一眼可见的状态"，
  // 收进菜单就等于看不见了。
  //
  // 两个条件分开写：title 只看视图（project 短暂消失时也要显示会话名，别闪回品牌名），
  // active 要求 project 存在（菜单每一项都要读 project.tasks / project.id，缺了会整块崩）。
  const showConversationTitle = Boolean(mobileApp && mobileView === "conversation");
  const conversationHeaderActive = Boolean(showConversationTitle && project);
  // 「还没跑完」＝ done / cancelled 之外的一切，与共享层 filterQueueTasks(…, "active") 同一口径。
  // 手机端的 Task 是从快照裁剪出来的精简类型（status 只是 string），接不上那份函数的 Task 类型，
  // 所以这里就地写一遍；将来改口径时两处必须一起改。
  const activeTaskCount = conversationHeaderActive ? (project?.tasks || []).filter((task) => task.status !== "done" && task.status !== "cancelled").length : 0;
  // 「就绪」是默认态、没有任何信息量，不占位；其余状态（排队中 / 执行中 / 已完成 / 失败 / 已停止）
  // 都值得一眼看到，尤其"失败"和"已停止" —— 用户需要知道上次为什么没跑完。
  const conversationState = conversation?.status && conversation.status !== "idle" ? conversationStatusLabel(conversation.status) : "";

  // 「＋」面板里与电脑端同源的三组内容。
  //
  // 可见性过滤放在手机端，是因为快照只发一份快捷方式库（用 projectIds 表达"绑定到哪些项目"），
  // 复制到每个项目里会让同一份数据在快照中出现多个版本。判据与电脑端 listShortcuts 的 SQL
  // 逐字对应：`scope='local' or 绑定到本项目`。
  //
  // **唯一的有意分叉**：已停用（enabled=false）的条目这里直接不显示，而电脑端是"灰显 + 可点进
  // 编辑器改"（它拉的是 ?includeDisabled=true）。手机端没有快捷方式编辑器，露出一条永远点不动
  // 的胶囊只是噪音 —— 停用状态要到电脑上改，手机端只负责"能用的那些"。（两侧的**排序**必须是
  // 同一份：见 control-server remoteSnapshotShortcuts 的 order by。）
  // 分组边界照抄桌面端：prompt + snippet 归「常用提示词」，command_request 归「常用命令」。
  const projectShortcuts = (snapshot?.shortcuts || []).filter((item) => item.enabled && (item.scope === "local" || item.projectIds.includes(project?.id || "")));
  const promptShortcuts = projectShortcuts.filter((item) => item.kind === "prompt" || item.kind === "snippet");
  const commandShortcuts = projectShortcuts.filter((item) => item.kind === "command_request");
  const projectSkills = project?.skills || [];
  // 技能按来源分组，只保留有内容的组；顺序固定为 项目 > 用户 > 系统/官方。
  const skillGroups = skillSourceOrder.map((source) => ({ source, label: skillSourceLabels[source], items: projectSkills.filter((skill) => skill.source === source) })).filter((group) => group.items.length > 0);

  return <main className={`mobile-remote ${mobileApp && mobileView === "conversation" ? "mobile-conversation-mode" : ""}`}>
    {editingTask && <div className="mobile-task-modal-backdrop" role="dialog" aria-modal="true" aria-labelledby="mobile-task-edit-title"><form className="mobile-task-modal" onSubmit={(event) => void saveTaskEdit(event)}><header><h2 id="mobile-task-edit-title">编辑任务</h2><button type="button" onClick={() => setEditingTask(null)} disabled={busy} aria-label="关闭">×</button></header><label>标题<input value={editTitle} onChange={(event) => setEditTitle(event.target.value)} required /></label><label>描述<textarea value={editDescription} onChange={(event) => setEditDescription(event.target.value)} rows={4} /></label><label>优先级<select value={editPriority} onChange={(event) => setEditPriority(event.target.value)}><option value="urgent">紧急</option><option value="high">高</option><option value="normal">普通</option><option value="low">低</option></select></label><footer><button type="button" onClick={() => setEditingTask(null)} disabled={busy}>取消</button><button type="submit" disabled={busy || !editTitle.trim()}>保存</button></footer></form></div>}
    {deletingTask && <div className="mobile-task-modal-backdrop" role="dialog" aria-modal="true" aria-labelledby="mobile-task-delete-title"><section className="mobile-task-modal mobile-task-delete-modal"><header><h2 id="mobile-task-delete-title">删除任务</h2><button type="button" onClick={() => setDeletingTask(null)} disabled={busy} aria-label="关闭">×</button></header><p>确定删除“{taskSummary(deletingTask)}”吗？删除后无法恢复。</p><footer><button type="button" onClick={() => setDeletingTask(null)} disabled={busy}>取消</button><button type="button" className="mobile-task-delete-confirm" onClick={() => void confirmTaskDelete()} disabled={busy}>确认删除</button></footer></section></div>}
    {confirmShortcut && <div className="mobile-task-modal-backdrop" role="dialog" aria-modal="true" aria-labelledby="mobile-shortcut-confirm-title"><section className="mobile-task-modal mobile-task-delete-modal"><header><h2 id="mobile-shortcut-confirm-title">执行「{confirmShortcut.name}」</h2><button type="button" onClick={() => setConfirmShortcut(null)} disabled={busy || Boolean(shortcutBusy)} aria-label="关闭">×</button></header><p>这条命令会在电脑端执行。模板内容：</p><pre className="mobile-shortcut-confirm-template">{confirmShortcut.template}</pre><footer><button type="button" onClick={() => setConfirmShortcut(null)} disabled={busy || Boolean(shortcutBusy)}>取消</button><button type="button" className="mobile-task-delete-confirm" onClick={() => void confirmShortcutRun()} disabled={busy || Boolean(shortcutBusy)}>执行</button></footer></section></div>}
    {/* 扫码取景层。必须是 <main> 的**直接子元素**：清底时 `html.barcode-scanner-active
        .mobile-remote > *:not(.mobile-scan)` 会把其余同级块整体不绘制，而取景窗要靠
        "挖空 + 100vmax 影子"透出画面 —— 一旦被嵌进配对面板里，就会连取景窗一起被隐藏
        （旧实现正是嵌在 .mobile-pairing 内部，靠两条补丁规则才勉强透出来）。

        渲染条件带上 scanError：相机起不来时 `scanning` 会立刻回 false，如果只按 scanning
        渲染，用户只会看到取景层闪一下消失、连一句"为什么"都没有（旧实现把提示挂在配对面板
        里，扫码期间那整块是 visibility: hidden，等于从来没提示过）。留在这一层里才能既说明
        原因、又给"重新扫描 / 打开系统设置"。

        `data-phase` 是给"矮视口 + 失败态"用的：失败态多出一张报错卡，会把取景窗挤小到
        222px（844×390）/ 76px（720×200），而这时窗口里本来就没有画面、只写着"相机未开启"。
        极矮视口下由 CSS 按这个属性把空框收掉（见 mobile-remote.css 的第二级媒体查询）。 */}
    {(scanOverlayVisible) && <div className="mobile-scan" data-backdrop={scanLiveNative ? "camera" : "solid"} data-phase={scanning ? "live" : "error"} role="dialog" aria-modal="true" aria-label="扫描配对二维码">
      {/* 原生分支不渲染 <video>：画面由原生插件画在 WebView 之下，页面上只需要留一个"洞"。 */}
      {!nativeScanPlatform && <video className="mobile-scan-video" ref={setScanVideo} muted playsInline autoPlay />}
      {/* DOM 顺序＝列方向上的视觉顺序，必须是「文案 → 遮罩 → 底部按钮」：
          遮罩是这一段里**会伸缩**的部分（flex: 1），它要待在文案与按钮之间；
          放在文案前面就会把文案挤到取景窗下面去（改布局时踩过一次）。 */}
      <div className="mobile-scan-text">
        <strong>{scanning ? "将电脑上的二维码放入框内" : "扫码没有成功"}</strong>
        <small>{scanning ? "二维码在电脑端「扫码配对」里生成；扫到之后还要输入电脑上显示的 6 位校验码。" : "可以重新扫描，也可以在下面改用 6 位校验码完成配对。"}</small>
      </div>
      <div className="mobile-scan-mask" aria-hidden="true">
        <div className="mobile-scan-window">
          <i className="mobile-scan-corner top-left" /><i className="mobile-scan-corner top-right" />
          <i className="mobile-scan-corner bottom-left" /><i className="mobile-scan-corner bottom-right" />
          {/* 失败态的取景窗里没有画面（相机根本没起来）。空着会被读成"卡住了"，
              所以补一句状态说明；扫描中这里只放扫描线。 */}
          {scanning ? <i className="mobile-scan-laser" /> : <span className="mobile-scan-window-note">相机未开启</span>}
        </div>
      </div>
      <div className="mobile-scan-footer">
        {/* 报错必须画在取景层内部：旧实现的提示挂在配对面板里，而扫码期间配对面板整体
            visibility: hidden —— "扫到的不是配对码"这类提示因此从来没被看见过。 */}
        {scanError && <p className="mobile-scan-error" role="alert"><span>{scanError}</span>{scanNeedsSettings && <button className="mobile-scan-settings" type="button" onClick={() => void openScannerSettings()}>打开系统设置</button>}</p>}
        <div className="mobile-scan-actions">
          {scanning && scanTorchAvailable && <button className="mobile-scan-torch" type="button" aria-pressed={scanTorchOn} aria-label={scanTorchOn ? "关闭手电筒" : "打开手电筒"} title={scanTorchOn ? "关闭手电筒" : "打开手电筒"} onClick={() => void toggleScanTorch()}><svg viewBox="0 0 24 24" aria-hidden="true" focusable="false" fill="none" stroke="currentColor" strokeWidth="1.9" strokeLinecap="round" strokeLinejoin="round"><path d="M13 2 4.5 13H11l-1 9 8.5-11H12l1-9Z" /></svg></button>}
          {scanning
            ? <button className="mobile-scan-cancel" type="button" onClick={closeScanOverlay}>取消扫描</button>
            : <><button className="mobile-scan-cancel" type="button" onClick={closeScanOverlay}>关闭</button><button className="mobile-scan-retry" type="button" onClick={beginPairingScan}>重新扫描</button></>}
        </div>
      </div>
    </div>}
    <header className="mobile-remote-header"><div className="mobile-remote-title">{(!mobileApp || mobileView === "conversation") && <button className="mobile-back" type="button" onClick={goBack} title={mobileView === "conversation" ? "返回项目" : "返回"} aria-label={mobileView === "conversation" ? "返回项目" : "返回"}><svg viewBox="0 0 24 24" width="18" height="18" aria-hidden="true" focusable="false" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round"><path d="M19 12H5" /><path d="M11 18l-6-6 6-6" /></svg></button>}<div className="mobile-brand"><img className="mobile-brand-mark" src="/milevia-mark.svg" width="36" height="36" alt="" /><h1>{showConversationTitle ? (conversation?.rawTitle || project?.name || "项目对话") : "Milevia"}</h1></div>{conversationHeaderActive && conversationState && <span className={`mobile-conversation-state mobile-conversation-state-${conversation?.status}`}>{conversationState}</span>}</div><div className="mobile-header-actions">{!conversationHeaderActive && notificationPermission === "default" && <button className="mobile-notification-button" type="button" onClick={() => void enableMobileNotifications()} title="开启后台通知">开启通知</button>}{!conversationHeaderActive && <button className="mobile-refresh" type="button" onClick={() => void refreshNow()} disabled={refreshing} aria-busy={refreshing} title="从电脑端重新同步"><svg className="mobile-refresh-icon" viewBox="0 0 24 24" width="13" height="13" aria-hidden="true" focusable="false" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><polyline points="23 4 23 10 17 10" /><polyline points="1 20 1 14 7 14" /><path d="M3.51 9a9 9 0 0 1 14.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0 0 20.49 15" /></svg><span>刷新</span></button>}{conversationHeaderActive && project && <div className="mobile-header-menu"><button className="mobile-header-menu-button" ref={headerMenuButtonRef} type="button" aria-haspopup="menu" aria-expanded={headerMenuOpen} aria-controls="mobile-header-menu-sheet" onClick={() => setHeaderMenuOpen((open) => !open)} aria-label="更多操作" title="更多操作">{activeTaskCount > 0 && <span className="mobile-header-menu-badge" aria-hidden="true">{activeTaskCount}</span>}<span aria-hidden="true">⋯</span></button>{headerMenuOpen && <div className="mobile-header-menu-sheet" id="mobile-header-menu-sheet" role="menu" aria-label="更多操作">{conversation && <div className="mobile-header-menu-info"><span>执行 Agent</span><small>{conversationAgentLabel(conversation.agentId)}</small></div>}<button type="button" role="menuitem" onClick={() => { setHeaderMenuOpen(false); void refreshNow(); }} disabled={refreshing} aria-busy={refreshing}><span>刷新</span><small>{refreshing ? "同步中…" : "重新同步电脑端"}</small></button>{notificationPermission === "default" && <button type="button" role="menuitem" onClick={() => { setHeaderMenuOpen(false); void enableMobileNotifications(); }}><span>开启通知</span><small>后台提醒</small></button>}<div className="mobile-header-menu-separator" role="none" /><button type="button" role="menuitem" onClick={() => { setHeaderMenuOpen(false); setConversationHistoryOpen(true); }} disabled={conversations.length === 0}><span>历史会话</span><small>{conversations.length}</small></button><button type="button" role="menuitem" onClick={() => { setHeaderMenuOpen(false); void createConversationForProject(project); }} disabled={busy}><span>新会话</span><small>新建</small></button><button type="button" role="menuitem" onClick={() => { setHeaderMenuOpen(false); setTasksOpen(true); }}><span>任务队列</span><small>{project.tasks.length}</small></button></div>}</div>}</div></header>
    {refreshStatus && <div className={`mobile-refresh-status mobile-refresh-status-${refreshStatus.state}`} role="status">{refreshStatus.state === "refreshing" && <span className="mobile-refresh-spinner" aria-hidden="true" />}<span className="mobile-refresh-status-text">{refreshStatus.message}</span>{refreshStatus.at && <time dateTime={refreshStatus.at.toISOString()}>{refreshStatus.at.toLocaleTimeString()}</time>}</div>}
    {mobileUpdate && !updateDismissed && <section className="mobile-update" role="status"><div className="mobile-update-text"><strong>发现新版本 v{mobileUpdate.release.version}</strong><small>当前 v{mobileUpdate.currentVersion}{mobileUpdate.release.size ? ` · ${(mobileUpdate.release.size / 1024 / 1024).toFixed(1)} MB` : ""}{mobileUpdate.release.notes ? ` · ${mobileUpdate.release.notes.slice(0, 40)}` : ""}</small></div><a className="mobile-update-action" href={mobileUpdate.release.url} target="_blank" rel="noreferrer">立即更新</a><button className="mobile-update-dismiss" type="button" onClick={() => setUpdateDismissed(true)} aria-label="稍后提醒" title="稍后提醒">✕</button></section>}
    {!mobileApp && agentStatus && !agentStatus.ready && <section className="mobile-agent-enroll"><div><h2>远程服务未就绪</h2><p>电脑端 Agent 尚未连接到云端，手机此时无法配对。请粘贴管理员提供的部署注册令牌完成一次注册；令牌只在本次注册使用，不会保存到磁盘，也不会进入安装包。</p></div><form onSubmit={(event) => void enrollRemoteAgent(event)}><input type="password" value={agentEnrollToken} onChange={(event) => setAgentEnrollToken(event.target.value)} placeholder="部署注册令牌" aria-label="部署注册令牌" autoComplete="off" /><button type="submit" disabled={agentEnrollBusy || !agentEnrollToken.trim()}>{agentEnrollBusy ? "提交中" : "注册远程服务"}</button></form>{agentEnrollMessage && <small>{agentEnrollMessage}</small>}</section>}
    {(!mobileApp || showMobilePairing) && <section className="mobile-pairing"><div><h2>扫码配对</h2><p>{mobileApp ? "扫描电脑上的二维码，再输入电脑显示的 6 位校验码，等待电脑确认。" : "点击生成二维码，手机扫码后输入校验码，再点击确认绑定。"}</p></div>{!mobileApp && <button className="mobile-pairing-generate" onClick={() => void createDesktopPairing()} disabled={busy}>生成二维码</button>}{mobileApp && <button className="mobile-pairing-generate" onClick={beginPairingScan} disabled={busy || scanning}>扫描二维码</button>}{pairingQR && <img className="mobile-pairing-qr" src={pairingQR} alt="Milevia 配对二维码" />}{!mobileApp && pairingID && <button className="mobile-pairing-confirm" onClick={() => void confirmDesktopPairing()} disabled={busy || !pairingReadyForConfirm}>确认绑定</button>}{pairingStatus && <small>{pairingStatus}</small>}</section>}
    {mobileApp && showMobilePairing && <section className="mobile-pairing-manual"><h2>使用校验码</h2><p>在电脑端生成校验码后，在此输入 6 位数字。</p><form onSubmit={(event) => void claimPairingByCode(event)}><input ref={manualCodeRef} inputMode="numeric" pattern="[0-9]{6}" maxLength={6} value={manualPairingCode} onChange={(event) => setManualPairingCode(event.target.value.replace(/\D/g, "").slice(0, 6))} placeholder="6 位校验码" aria-label="6 位校验码" /><button type="submit" disabled={busy || manualPairingCode.length !== 6}>验证并配对</button></form>{token.trim() && <button className="mobile-pairing-collapse" type="button" onClick={() => setPairingExpanded(false)}>返回项目</button>}</section>}
    {!mobileApp && pairingCode && <div className="mobile-pairing-code">校验码：<strong>{pairingCode}</strong></div>}
    {error && <div className="mobile-error" role="alert">{error}</div>}
    {commandState && <div className={`mobile-command-status ${commandState.status}`}>命令 {commandState.commandId}：{commandState.status}</div>}
    {(!mobileApp || mobileView === "projects") && instance && <section className="mobile-instance-status"><div><strong>{instance.name || instance.instanceId}</strong><span className={`mobile-status ${instance.status}`}>{instance.status}</span></div><small>事件序号 {instance.lastAgentSequence} · {instance.lastSeenAt ? new Date(instance.lastSeenAt).toLocaleString() : "尚未连接"}</small></section>}
    {(!mobileApp || mobileView === "projects") && <section className="mobile-summary"><span><b>{projects.length}</b><small>项目</small></span><span><b>{taskCount}</b><small>任务</small></span></section>}
    {mobileApp && mobileView === "projects" && <section className="mobile-project-picker"><div className="mobile-section-heading"><h2>选择项目</h2><span>{snapshot ? new Date(snapshot.observedAt).toLocaleTimeString() : "加载中"}</span></div>{projects.length === 0 ? <p className="mobile-empty">暂无项目或电脑尚未同步。</p> : projects.map((item) => { const environment = projectEnvironment(item); const running = item.running === true; return <button type="button" className="mobile-project-choice" key={item.id} onClick={() => void openMobileProject(item)} disabled={busy}><span className={`mobile-project-mark ${environment}`} aria-hidden="true"><ProjectEnvironmentIcon environment={environment} /></span><span className="mobile-project-choice-content"><span className="mobile-project-choice-head"><strong title={item.name}>{item.name}</strong><span className="mobile-project-running" data-state={running ? "running" : "idle"}><i></i>{running ? "运行中" : "未运行"}</span></span><small className="mobile-project-choice-meta" title={`${projectEnvironmentLabel(environment)} · ${item.gitBranch || "默认分支"}`}>{projectEnvironmentLabel(environment)} · {item.gitBranch || "默认分支"} · {item.tasks.length} 个任务 · {item.conversations?.length || 0} 个会话</small></span><span className="mobile-project-choice-chevron" aria-hidden="true">›</span></button>; })}</section>}
    {mobileApp && mobileView === "projects" && token.trim() && instances.length > 0 && !pairingExpanded && <div className="mobile-pairing-actions"><button className="mobile-repair" type="button" onClick={() => setPairingExpanded(true)}>重新配对此设备</button><button className="mobile-unbind" type="button" onClick={() => void unbindDevice()} disabled={busy}>解除绑定</button></div>}
    {mobileApp && mobileView === "conversation" && project && <section className="mobile-conversation"><div className="mobile-message-list">{conversationTimeline.length ? conversationTimeline.map((entry) => entry.kind === "message" ? <article className={`mobile-message ${entry.message.role}`} key={entry.key}><small>{entry.message.role === "user" ? "我" : "Agent"} · {messageTime(entry.message.createdAt)}</small><div className="mobile-message-markdown markdown"><ReactMarkdown remarkPlugins={[remarkGfm]} skipHtml components={{ ...markdownCodeComponents, a: ({ href, children }) => <a href={href} target="_blank" rel="noreferrer">{children}</a> }}>{entry.message.content}</ReactMarkdown></div></article> : <article className={`mobile-notice mobile-notice-${entry.notice.variant}${entry.notice.state ? ` mobile-notice-${entry.notice.state}` : ""}`} key={entry.key}><span className="mobile-notice-icon" aria-hidden="true">{noticeIcons[entry.notice.variant]}</span><div className="mobile-notice-text"><strong>{entry.notice.title}</strong>{entry.notice.detail && <small>{entry.notice.detail}</small>}</div>{noticeTime(entry.notice.createdAt) && <time className="mobile-notice-time">{noticeTime(entry.notice.createdAt)}</time>}</article>) : <p className="mobile-empty">{conversation ? "该会话暂无对话内容。" : "请先新建会话。"}</p>}{conversationProcessing && <div className="mobile-agent-processing" role="status" aria-live="polite"><span className="mobile-agent-processing-dots" aria-hidden="true"><i></i><i></i><i></i></span><span>{conversation ? conversationAgentLabel(conversation.agentId) : "Agent"} 正在处理...</span></div>}</div>{tasksOpen && <section className="mobile-task-panel" ref={taskPanelRef} tabIndex={-1} role="dialog" aria-modal="true" aria-labelledby="mobile-task-panel-title">
      <header className="mobile-task-panel-header">
        <div>
          <h3 id="mobile-task-panel-title">任务队列</h3>
          <small>{taskFilterSummary}</small>
        </div>
        <button type="button" className="mobile-task-panel-close" onClick={() => setTasksOpen(false)} aria-label="关闭任务队列" title="关闭">×</button>
      </header>
      <nav className="mobile-task-filters" aria-label="任务状态分类" role="tablist">
        {taskFilters.map((filter) => {
          const count = filter.id === "all" ? project.tasks.length : project.tasks.filter((task) => task.status === filter.id).length;
          return <button type="button" role="tab" aria-selected={taskFilter === filter.id} className={taskFilter === filter.id ? "active" : ""} key={filter.id} onClick={() => setTaskFilter(filter.id)}>{filter.label}<span>{count}</span></button>;
        })}
      </nav>
      <div className="mobile-task-panel-body">
        <div className="mobile-task-list">
          {visibleTasks.length === 0 ? <p className="mobile-empty">当前分类没有任务。</p> : visibleTasks.map((task) => <div className="mobile-task" key={task.id}>
            <div>
              <strong>{taskSummary(task)}</strong>
              <span className={`mobile-task-status ${taskStatusClass(task.status)}`}>{taskStatusLabel(task.status)}</span>
              <small>{taskPriorityLabel(task.priority)}</small>
            </div>
            <div className="mobile-task-actions">
              {task.status === "todo" || task.status === "action_required" ? <button type="button" disabled={busy} onClick={() => void sendTaskCommand(task.id, "task.dispatch")}>下发</button> : null}
              {task.status === "awaiting_review" ? <button type="button" disabled={busy} onClick={() => void sendTaskCommand(task.id, "task.review")}>验收</button> : null}
              {task.status === "running" ? <button type="button" disabled={busy} onClick={() => void sendTaskCommand(task.id, "task.stop")}>停止</button> : null}
            </div>
            <details className="mobile-task-disclosure">
              <summary>查看详情</summary>
              <p>{task.description?.trim() || "暂无任务描述"}</p>
              <time dateTime={task.updatedAt}>更新于 {new Date(task.updatedAt).toLocaleString()}</time>
            </details>
          </div>)}
        </div>
        <div className="mobile-create">
          <h3>创建任务</h3>
          <form onSubmit={createTask}>
            <label>标题<input value={title} onChange={(event) => setTitle(event.target.value)} required placeholder="要处理的事情" /></label>
            <label>描述<textarea value={description} onChange={(event) => setDescription(event.target.value)} placeholder="补充上下文（可选）" rows={3} /></label>
            <button type="submit" disabled={busy || !title.trim()}>创建并排队</button>
          </form>
        </div>
      </div>
    </section>}<div className="mobile-composer-shell" ref={composerShellRef}>{composerToolsOpen && <section className="mobile-composer-tools" id="mobile-composer-tools" aria-label="工具">
<div className="mobile-tool-group"><h3>常用提示词<span>{promptShortcuts.length}</span></h3>{promptShortcuts.length === 0 ? <p className="mobile-tool-empty">电脑端还没有添加常用提示词。</p> : <div className="mobile-tool-chips">{promptShortcuts.map((item) => <button type="button" key={item.id} disabled={busy || !conversation || Boolean(shortcutBusy)} onClick={() => void applyShortcut(item)} title={item.template}>{shortcutBusy === item.id ? "处理中" : item.name}</button>)}</div>}</div>
<div className="mobile-tool-group"><h3>常用命令<span>{commandShortcuts.length}</span></h3>{commandShortcuts.length === 0 ? <p className="mobile-tool-empty">电脑端还没有添加常用命令。</p> : <div className="mobile-tool-chips">{commandShortcuts.map((item) => <button type="button" key={item.id} disabled={busy || !conversation || Boolean(shortcutBusy)} onClick={() => void applyShortcut(item)} title={item.template}>{shortcutBusy === item.id ? "处理中" : item.name}</button>)}</div>}</div>
<div className="mobile-tool-group"><h3>技能<span>{projectSkills.length}</span></h3>{projectSkills.length === 0 ? <p className="mobile-tool-empty">电脑端未发现可用技能。</p> : <div className="mobile-tool-subgroups">{skillGroups.map((group) => <div className="mobile-tool-subgroup" key={group.source}><h4>{group.label}<span>{group.items.length}</span></h4><div className="mobile-tool-chips">{group.items.map((skill) => <button type="button" key={`${group.source}-${skill.name}`} disabled={busy || !conversation} onClick={() => fillSkill(skill)} title={skill.description}>{skill.name}</button>)}</div></div>)}</div>}</div>
<div className="mobile-tool-group"><h3>输入</h3><div className="mobile-tool-chips"><button type="button" disabled={busy || !conversation} onClick={insertComposerLineBreak}>换行</button><button type="button" disabled={!messageDraft && skillRefs.length === 0} onClick={() => { setMessageDraft(""); setSkillRefs([]); setComposerToolsOpen(false); }}>清空草稿</button></div></div>
</section>}<form className="mobile-composer" onSubmit={sendConversationMessage}>{skillRefs.length > 0 && <div className="mobile-skill-refs" role="group" aria-label="已引用的技能">{skillRefs.map((skill) => <span className="mobile-skill-ref" key={`${skill.source}-${skill.name}`}><span className="mobile-skill-ref-name" title={skill.description || skill.name}>{skill.name}</span><button type="button" className="mobile-skill-ref-remove" title={`移除技能 ${skill.name}`} aria-label={`移除技能 ${skill.name}`} disabled={busy} onClick={() => removeSkillRef(skill)}>×</button></span>)}<span className="mobile-skill-ref-hint">发送时展开为完整引用</span></div>}<div className="mobile-composer-box"><button type="button" className="mobile-composer-tool" aria-label="工具" title="工具" aria-expanded={composerToolsOpen} aria-controls="mobile-composer-tools" onClick={() => setComposerToolsOpen((open) => !open)} disabled={busy}><svg viewBox="0 0 24 24" width="18" height="18" aria-hidden="true" focusable="false" fill="none" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round"><path d="M12 5v14" /><path d="M5 12h14" /></svg></button><textarea ref={messageInputRef} value={messageDraft} onChange={(event) => setMessageDraft(event.target.value)} onKeyDown={(event) => { if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing) { event.preventDefault(); event.currentTarget.form?.requestSubmit(); } }} placeholder={conversation ? "输入消息..." : "请先新建会话"} aria-label="输入消息" rows={1} disabled={busy || !conversation} /><button type="submit" className="mobile-composer-send" disabled={busy || !conversation || (!messageDraft.trim() && skillRefs.length === 0)} aria-label="发送消息" title="发送消息"><svg viewBox="0 0 24 24" width="18" height="18" aria-hidden="true" focusable="false" fill="none" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round" strokeLinejoin="round"><path d="M12 19V5" /><path d="M5 12l7-7 7 7" /></svg></button></div></form></div></section>}
    {!mobileApp && <section className="mobile-projects"><div className="mobile-section-heading"><h2>项目与任务</h2><span>{snapshot ? new Date(snapshot.observedAt).toLocaleTimeString() : "加载中"}</span></div>{projects.length === 0 ? <p className="mobile-empty">暂无项目或电脑尚未同步。</p> : projects.map((item) => <article className={`mobile-project ${project?.id === item.id ? "selected" : ""}`} key={item.id} role="button" tabIndex={0} aria-expanded={project?.id === item.id} onClick={() => setSelectedProject(item.id)} onKeyDown={(event) => { if (event.key === "Enter" || event.key === " ") { event.preventDefault(); setSelectedProject(item.id); } }}><header><div><h3>{item.name}</h3><small>{item.gitBranch || "默认分支"}</small></div><span>{item.tasks.length} 个任务</span></header>{project?.id === item.id && <div className="mobile-task-list">{item.tasks.length === 0 ? <p className="mobile-empty">还没有任务。</p> : item.tasks.map((task) => <div className="mobile-task" key={task.id}><div><strong>{task.title}</strong><small>{task.priority} · {task.status}</small></div><div className="mobile-task-actions"><button type="button" disabled={busy} onClick={(event) => { event.stopPropagation(); void sendTaskCommand(task.id, task.status === "running" ? "task.stop" : task.status === "awaiting_review" ? "task.review" : "task.dispatch"); }}>操作</button></div></div>)}</div>}</article>)}</section>}
    {!mobileApp && project && <section className="mobile-create"><h2>创建任务 · {project.name}</h2><form onSubmit={createTask}><label>标题<input value={title} onChange={(event) => setTitle(event.target.value)} required placeholder="要处理的事情" /></label><label>描述<textarea value={description} onChange={(event) => setDescription(event.target.value)} placeholder="补充上下文（可选）" rows={3} /></label><button type="submit" disabled={busy || !title.trim()}>创建并排队</button></form></section>}
    {newConversationProject && <div className="mobile-new-conversation-backdrop" role="dialog" aria-modal="true" aria-labelledby="mobile-new-conversation-title"><section className="mobile-new-conversation-dialog"><header><div><h2 id="mobile-new-conversation-title">新会话</h2><p>选择执行 Agent</p></div><button type="button" onClick={cancelNewConversation} disabled={busy} aria-label="关闭">×</button></header><div className="mobile-agent-options" role="radiogroup" aria-label="选择执行 Agent"><button type="button" role="radio" aria-checked={newConversationAgent === "claude-code"} className={newConversationAgent === "claude-code" ? "active" : ""} onClick={() => setNewConversationAgent("claude-code")}><strong>Claude Code</strong><small>使用 Claude Code 执行</small></button><button type="button" role="radio" aria-checked={newConversationAgent === "codex"} className={newConversationAgent === "codex" ? "active" : ""} onClick={() => setNewConversationAgent("codex")}><strong>Codex</strong><small>使用 Codex 执行</small></button></div><footer><button type="button" className="mobile-new-conversation-cancel" onClick={cancelNewConversation} disabled={busy}>取消</button><button type="button" className="mobile-new-conversation-confirm" onClick={() => void createConversationForProject(newConversationProject, newConversationAgent)} disabled={busy}>创建会话</button></footer></section></div>}
    {conversationHistoryOpen && <div className="mobile-conversation-history-backdrop" role="dialog" aria-modal="true" aria-labelledby="mobile-conversation-history-title" onClick={(event) => { if (event.target === event.currentTarget) setConversationHistoryOpen(false); }}><section className="mobile-conversation-history-dialog"><header><div><h2 id="mobile-conversation-history-title">历史会话</h2><small>{conversations.length} 个会话</small></div><button type="button" onClick={() => setConversationHistoryOpen(false)} aria-label="关闭历史会话">×</button></header>{conversations.length === 0 ? <p className="mobile-empty">该项目还没有会话。</p> : <ul className="mobile-conversation-history-list">{conversations.map((item) => <li key={item.id}><button type="button" className={`mobile-conversation-history-item${item.id === conversation?.id ? " active" : ""}`} aria-current={item.id === conversation?.id ? "true" : undefined} onClick={() => { setSelectedConversation(item.id); setConversationHistoryOpen(false); }}><strong>{item.title || "未命名会话"}</strong><small>{conversationStatusLabel(item.status)}{item.lastActivityAt ? ` · ${new Date(item.lastActivityAt).toLocaleString()}` : ""}</small></button></li>)}</ul>}</section></div>}
  </main>;
}
