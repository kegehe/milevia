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

/** 通知统一进入所属项目的对话页面，不再跳转到任务详情。 */
export function notificationConversationURL(event: Pick<NotificationEvent, "projectId" | "conversationId">): string {
  const baseURL = `/projects/${event.projectId}/conversations`;
  return event.conversationId ? `${baseURL}/${event.conversationId}` : baseURL;
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
  if (type.includes("done") || type.includes("completed") || type.includes("succeeded")) {
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
