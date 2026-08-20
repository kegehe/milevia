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
