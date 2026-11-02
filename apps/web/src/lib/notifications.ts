// 通知事件类型定义

export interface NotificationEvent {
  id: string;
  type: string; // "task.done", "task.action_required", "approval.pending", "run.completed", "orchestration.needs_human"
  projectId: string;
  projectName: string;
  /** 任务的通知用标题；服务端算好下发（title 为空时回落描述前 80 字），前端不许从 body 反解析。 */
  taskTitle?: string;
  /** 最近一次任务执行耗时（毫秒，服务端从 startedAt→finishedAt 算好）；缺失或为 0 表示没有可靠耗时。 */
  durationMs?: number;
  conversationId?: string;
  taskId?: string;
  /** 命令审批的 id。实时推送会带上；历史重放没有这一列时，通知 id 就是它。 */
  approvalId?: string;
  title: string;
  body: string;
  priority: "high" | "normal" | "low";
  actionUrl: string;
  createdAt: string;
}

/** 与服务端 notificationMaxRows 对齐，实时推送期间也不无限累积未读集合。 */
export const MAX_UNREAD_NOTIFICATIONS = 1000;

/** 保留一份 REST 历史和一份实时窗口；逐个淘汰，不能超限后整批清空。 */
export function rememberNotificationID(ids: Set<string>, id: string): void {
  ids.delete(id);
  ids.add(id);
  while (ids.size > 2 * MAX_UNREAD_NOTIFICATIONS) ids.delete(ids.values().next().value!);
}

/** Date 只保留毫秒；服务端 RFC3339Nano 的余下精度用于排序和全部已读边界。 */
export function notificationTimeNs(value: string): bigint | null {
  const milliseconds = Date.parse(value);
  if (!Number.isFinite(milliseconds)) return null;
  const fraction = /\.(\d+)(?:Z|[+-]\d{2}:\d{2})$/.exec(value)?.[1] ?? "";
  const remainingNs = fraction.padEnd(9, "0").slice(3, 9);
  return BigInt(milliseconds) * 1_000_000n + BigInt(remainingNs || "0");
}

export function trimUnreadNotifications(unread: Map<string, bigint>): void {
  if (unread.size <= MAX_UNREAD_NOTIFICATIONS) return;
  const latest = [...unread].sort(([leftID, leftTime], [rightID, rightTime]) =>
    (leftTime < rightTime ? 1 : leftTime > rightTime ? -1 : 0) || (leftID < rightID ? 1 : leftID > rightID ? -1 : 0));
  for (const [id] of latest.slice(MAX_UNREAD_NOTIFICATIONS)) unread.delete(id);
}

/** 整批对账后再裁剪，避免已删除的新记录先挤掉服务端补回的有效旧记录。 */
export function reconcileUnreadNotifications(
  unread: Map<string, bigint>,
  beforeFetch: ReadonlySet<string>,
  serverUnread: ReadonlyMap<string, bigint>,
): void {
  for (const id of unreadIdsMissingFromServer(beforeFetch, new Set(serverUnread.keys()))) unread.delete(id);
  for (const [id, createdAt] of serverUnread) unread.set(id, createdAt);
  trimUnreadNotifications(unread);
}

export function isClockTime(value: unknown): value is string {
  if (typeof value !== "string" || !/^\d{2}:\d{2}$/.test(value)) return false;
  const [hours, minutes] = value.split(":").map(Number);
  return hours >= 0 && hours < 24 && minutes >= 0 && minutes < 60;
}

/** 通知统一进入所属项目的对话页面，不再跳转到任务详情。 */
export function notificationConversationURL(event: Pick<NotificationEvent, "projectId" | "conversationId">): string {
  const baseURL = `/projects/${event.projectId}/conversations`;
  return event.conversationId ? `${baseURL}/${event.conversationId}` : baseURL;
}

/** 后端提供明确目标时优先跳转该页面，兼容旧通知时回退到所属会话。 */
export function notificationTargetURL(event: Pick<NotificationEvent, "actionUrl" | "projectId" | "conversationId">): string {
  return event.actionUrl || notificationConversationURL(event);
}

/** 判断当前本地时间是否落在有效的免打扰时段内，支持跨午夜区间。 */
export function isWithinQuietHours(now: Date, start: string, end: string): boolean {
  if (!isClockTime(start) || !isClockTime(end)) return false;
  const toMinutes = (value: string) => {
    const [hours, minutes] = value.split(":").map(Number);
    return hours * 60 + minutes;
  };
  const startMinutes = toMinutes(start);
  const endMinutes = toMinutes(end);
  if (startMinutes === endMinutes) return false;
  const currentMinutes = now.getHours() * 60 + now.getMinutes();
  return startMinutes < endMinutes
    ? currentMinutes >= startMinutes && currentMinutes < endMinutes
    : currentMinutes >= startMinutes || currentMinutes < endMinutes;
}

/** 窗口不在前台时要弹 Windows 通知的种类。
 * result：任务完成 / 等待审查，短时弹窗，点正文打开会话。
 * interrupt：需要人处理（提问、任务需要处理、编排卡住），留在屏幕上直到点掉。
 * approval：命令审批。除了留在屏幕上，还带允许 / 拒绝。 */
export function windowsToastKind(type: string): "result" | "interrupt" | "approval" | null {
  if (type === "task.done" || type === "task.awaiting_review") return "result";
  if (type === "approval.pending") return "approval";
  if (type === "task.action_required" || type === "orchestration.needs_human" || type === "ask.pending") return "interrupt";
  return null;
}

export function isWindowsNotifyType(type: string): boolean {
  return windowsToastKind(type) !== null;
}

/** 命令审批的 id。实时事件带 approvalId；数据库重放的历史通知用通知 id（两者被写成同一个）。 */
export function approvalIDFromNotification(event: { type: string; id?: string; approvalId?: string }): string {
  if (event.type !== "approval.pending") return "";
  return (event.approvalId || event.id || "").trim();
}

/** 弹窗上的按钮。只有命令审批能在通知上直接裁决，顺序与会话里的横幅一致：拒绝在前。 */
export function windowsToastActions(event: { type: string; id?: string; approvalId?: string }): { label: string; action: "allow" | "deny" }[] {
  if (!approvalIDFromNotification(event)) return [];
  return [
    { label: "拒绝", action: "deny" },
    { label: "允许", action: "allow" },
  ];
}

/** 需要人马上处理的弹窗不自己消失。完成类仍是短时提示。 */
export function windowsToastSticky(type: string): boolean {
  const kind = windowsToastKind(type);
  return kind === "approval" || kind === "interrupt";
}

const WINDOWS_TOAST_DETAIL_TYPES = new Set(["approval.pending", "orchestration.needs_human", "ask.pending"]);

function toastDetail(body: string): string {
  const text = body.replace(/\s+/g, " ").trim();
  const runes = [...text];
  if (runes.length <= 140) return text;
  return `${runes.slice(0, 139).join("")}…`;
}

/** 毫秒 → 人类可读耗时（Windows 弹窗第三行）。
 * 秒级「42 秒」；分钟级「3 分 42 秒 / 5 分钟」；小时级「1 小时 23 分钟」。
 * 入参必须 > 0；调用方（windowsToastLines）负责在缺失时省略整行。 */
export function formatDurationMs(durationMs: number): string {
  const totalSeconds = Math.floor(durationMs / 1000);
  if (totalSeconds < 60) return `${totalSeconds} 秒`;
  const minutes = Math.floor(totalSeconds / 60);
  const seconds = totalSeconds % 60;
  if (minutes < 60) return seconds > 0 ? `${minutes} 分 ${seconds} 秒` : `${minutes} 分钟`;
  const hours = Math.floor(minutes / 60);
  const restMinutes = minutes % 60;
  return restMinutes > 0 ? `${hours} 小时 ${restMinutes} 分钟` : `${hours} 小时`;
}

/** Windows 系统弹窗的三行内容。
 * 标题=事件 title；第二行「项目「X」· 任务Y」消费服务端的 projectName/taskTitle；
 * 第三行优先是耗时。审批、提问、编排卡住没有耗时，第三行改用正文（命令、问题或原因）。
 * 各行都可能为空串，Rust 侧按空串跳过对应行。 */
export function windowsToastLines(event: Pick<NotificationEvent, "title" | "projectName" | "taskTitle" | "durationMs" | "type" | "body">): { title: string; line1: string; line2: string } {
  const taskTitle = (event.taskTitle ?? "").trim();
  const projectName = (event.projectName ?? "").trim();
  const line1 = projectName && taskTitle ? `项目「${projectName}」· ${taskTitle}` : taskTitle || (projectName ? `项目「${projectName}」` : "");
  const duration = event.durationMs && event.durationMs > 0 ? formatDurationMs(event.durationMs) : "";
  // 完成类没有耗时时留空，不拿正文去填——正文常常是标题的另一种说法。
  // 审批、提问、编排卡住的正文才是人要看的那一句（命令、问题、原因）。
  const line2 = duration || (WINDOWS_TOAST_DETAIL_TYPES.has(event.type) ? toastDetail(event.body ?? "") : "");
  return { title: event.title, line1, line2 };
}

/** 需要发通知的任务/编排状态 */
export const NOTIFIABLE_STATUSES = new Set([
  "action_required",
  "needs_human",
  "done",
  "awaiting_review",
]);

/** 服务端列表里已经不存在、应当从本地未读集合移除的通知 ID。
 *
 * 传进来的 `unreadIds` 必须是**本次拉取开始前**的本地快照：请求期间刚到的通知
 * 不在快照里，因此不会因为"这次响应里还没有它"而被误删（也避免依赖两端时钟）。*/
export function unreadIdsMissingFromServer(
  unreadIds: ReadonlySet<string>,
  serverIds: ReadonlySet<string>,
): string[] {
  const missing: string[] = [];
  for (const id of unreadIds) {
    if (!serverIds.has(id)) missing.push(id);
  }
  return missing;
}

/** 通知类型到优先级映射 */
export function priorityForType(type: string): "high" | "normal" | "low" {
  if (type.includes("action_required") || type.includes("needs_human") || type.includes("approval")) {
    return "high";
  }
  if (type.includes("done") || type.includes("completed") || type.includes("succeeded") || type.includes("failed") || type.includes("error")) {
    return "normal";
  }
  return "low";
}

/** 通知类型到 toast 变体映射 */
export function toastVariantForType(type: string): "error" | "warning" | "success" | "info" {
  if (type.includes("failed") || type.includes("error")) return "error";
  if (type.includes("action_required") || type.includes("needs_human") || type.includes("approval")) return "warning";
  if (type.includes("done") || type.includes("succeeded")) return "success";
  return "info";
}

/**
 * Web Notification API 在当前环境能否真正把通知送到系统。只有浏览器可以。
 *
 * 桌面端（Tauri/WebView2）不行：WebView2 把通知授权交给宿主 `PermissionRequested`、
 * 把渲染交给宿主 `NotificationReceived`，而 wry 两者都没实现 —— 结果是权限恒为 denied，
 * 且这个 denied 会被持久化进应用自己的 EBWebView profile（用户在桌面端没有任何入口去改它）。
 * Tauri 官方的通知插件正是因为这条 API 在 webview 里不可用，注入脚本直接覆盖了
 * `window.Notification`。
 *
 * 原生包（Capacitor）同理不行：Android WebView 不把 Web Notification 接到系统通知栏。
 * 注意这不代表"手机端没有系统通知"——原生包走的是另一条通道（`@capacitor/local-notifications`
 * 的本地通知，判定与接线见 lib/mobile-notify.ts 与 MobileRemotePage）。本函数问的是
 * "Web Notification 这条 API 能不能用"，在原生包里答案仍然是不能用。
 */
export function webNotificationsSupported(env: {
  hasNotificationAPI: boolean;
  isDesktop: boolean;
  isNativePlatform: boolean;
}): boolean {
  return env.hasNotificationAPI && !env.isDesktop && !env.isNativePlatform;
}
