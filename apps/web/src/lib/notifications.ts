// 通知事件类型定义

export interface NotificationEvent {
  id: string;
  type: string; // "task.done", "task.action_required", "approval.pending", "run.completed", "orchestration.needs_human"
  projectId: string;
  projectName: string;
  conversationId?: string;
  taskId?: string;
  title: string;
  body: string;
  priority: "high" | "normal" | "low";
  actionUrl: string;
  createdAt: string;
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

/** 需要弹 Windows 系统通知的类型：仅“任务完成 / 等待审查”这类已完成语义。
 * 弹窗文字固定为“有任务完成”，因此只匹配完成类事件；需要处理的（action_required、
 * needs_human 等）仍走应用内提醒，避免文字与事件含义不符。 */
export function isWindowsNotifyType(type: string): boolean {
  return type === "task.done" || type === "task.awaiting_review";
}

/** 需要发通知的任务/编排状态 */
export const NOTIFIABLE_STATUSES = new Set([
  "action_required",
  "needs_human",
  "done",
  "awaiting_review",
]);

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
 * `window.Notification`。原生包（Capacitor）同理：Android WebView 不把 Web Notification
 * 接到系统通知栏，且清单里没声明 `POST_NOTIFICATIONS`。
 */
export function webNotificationsSupported(env: {
  hasNotificationAPI: boolean;
  isDesktop: boolean;
  isNativePlatform: boolean;
}): boolean {
  return env.hasNotificationAPI && !env.isDesktop && !env.isNativePlatform;
}
