export type Request = <T>(path: string, init?: RequestInit) => Promise<T>;

export type TaskStatus = "todo" | "running" | "awaiting_review" | "action_required" | "done" | "cancelled";
export type Priority = "urgent" | "high" | "normal" | "low";
export type TaskFilter = "active" | "todo" | "running" | "awaiting_review";
export type Dependency = { taskId: string; title: string; status: TaskStatus };
export type Blocker = Dependency;
export type TaskRun = { id: string; runId: string; sequence: number; status: string; createdAt: string; finishedAt?: string; failureReason: string };
export type Task = {
  id: string;
  title: string;
  description: string;
  acceptanceCriteria: string;
  priority: Priority;
  pinned?: boolean;
  position: number;
  status: TaskStatus;
  dependsOn: Dependency[];
  blockedBy: Blocker[];
  blocks: Dependency[];
  lastRun?: TaskRun;
  orchestrationStatus?: string;
  orchestrationUpdatedAt?: string;
  createdAt: string;
  updatedAt: string;
};
export type TaskDetail = Task & { promptPreview: string; canDispatch: boolean; blockReason?: string; runs: TaskRun[] };

export const priorityLabels: Record<Priority, string> = { urgent: "紧急", high: "高", normal: "普通", low: "低" };
export const statusLabels: Record<TaskStatus, string> = { todo: "待处理", running: "执行中", awaiting_review: "待验收", action_required: "需处理", done: "已完成", cancelled: "已取消" };

export function taskDisplayStatus(task: Task): string {
  if (task.orchestrationStatus === "checking") return "独立审查中";
  if (task.orchestrationStatus === "preparing") return "准备执行";
  if (task.orchestrationStatus === "implementing") return "自动执行中";
  if (task.orchestrationStatus === "queued") return "自动队列中";
  if (task.orchestrationStatus === "paused") return "自动队列已暂停";
  if (task.orchestrationStatus === "needs_human") return "自动编排需处理";
  if (task.status === "running" && task.lastRun?.status === "queued") return "队列中";
  return statusLabels[task.status];
}

export function taskDisplayStatusClass(task: Task): string {
  return task.orchestrationStatus ? `orchestration-${task.orchestrationStatus}` : task.status === "running" && task.lastRun?.status === "queued" ? "action_required" : task.status;
}

export function isTaskOrchestrating(task: Task): boolean {
  return ["queued", "preparing", "implementing", "checking"].includes(task.orchestrationStatus || "");
}

export function isTaskBlocked(task: Task): boolean {
  return (task.status === "todo" || task.status === "action_required") && task.blockedBy.length > 0;
}

export function canOfferDispatch(task: Task): boolean {
  return task.status === "todo" && !isTaskBlocked(task);
}

export function canRedispatch(task: Task): boolean {
  return task.status === "action_required" && !isTaskBlocked(task) && task.lastRun?.status !== "queued" && task.lastRun?.status !== "running";
}

export function filterQueueTasks(tasks: Task[], filter: TaskFilter): Task[] {
  if (filter === "active") return tasks.filter((task) => task.status !== "done" && task.status !== "cancelled");
  if (filter === "todo") return tasks.filter((task) => task.status === "todo" || task.status === "action_required");
  return tasks.filter((task) => task.status === filter);
}

const priorityRank: Record<Priority, number> = { urgent: 0, high: 1, normal: 2, low: 3 };

export function queueRank(task: Task): number {
  if (task.status === "running") return 0;
  if (canOfferDispatch(task)) return 1;
  if (task.status === "action_required" && !isTaskBlocked(task)) return 2;
  if (task.status === "awaiting_review") return 3;
  if (isTaskBlocked(task)) return 4;
  return 4;
}

export function taskDisplayTitle(task: Task): string {
  return task.title || (task.description.length > 40 ? task.description.slice(0, 40) + "..." : task.description) || "未命名任务";
}

export function taskQueueNote(task: Task): string {
  if (task.orchestrationStatus === "checking") return "独立审查代理正在检查本次提交";
  if (task.orchestrationStatus === "preparing") return "自动编排正在准备工作区";
  if (task.orchestrationStatus === "implementing") return "自动编排正在执行任务";
  if (task.orchestrationStatus === "queued") return "已进入自动编排队列";
  if (task.orchestrationStatus === "paused") return "自动编排已暂停";
  if (task.orchestrationStatus === "needs_human") return "自动编排需要人工处理";
  if (isTaskBlocked(task)) return `等待：${task.blockedBy.map((item) => item.title).join("、")}`;
  if (task.status === "awaiting_review") return "执行完成，等待人工确认";
  if (task.status === "action_required" && task.lastRun) {
    const reason = task.lastRun.failureReason ? `：${task.lastRun.failureReason}` : "";
    return `第 ${task.lastRun.sequence} 次执行${task.lastRun.status === "failed" ? "失败" : task.lastRun.status === "stopped" ? "已停止" : task.lastRun.status === "interrupted" ? "已中断" : task.lastRun.status === "succeeded" ? "完成，要求修改" : task.lastRun.status}${reason}`;
  }
  if (task.status === "action_required") return "要求修改，等待重新下发";
  if (task.status === "running") {
    if (task.lastRun?.status === "queued") return "会话正在执行其他任务";
    return "执行中";
  }
  if (task.lastRun) return `第 ${task.lastRun.sequence} 次执行：${task.lastRun.status}`;
  return "可独立处理";
}

export function taskRunStatusLabel(status: string): string {
  switch (status) {
    case "queued": return "队列中";
    case "running": return "执行中";
    case "succeeded": return "已完成";
    case "failed": return "失败";
    case "stopped": return "已停止";
    case "interrupted": return "已中断";
    default: return status;
  }
}

export function sortQueueTasks(tasks: Task[]): Task[] {
  return [...tasks].sort((left, right) => Number(Boolean(right.pinned)) - Number(Boolean(left.pinned)) || queueRank(left) - queueRank(right) || priorityRank[left.priority] - priorityRank[right.priority] || left.position - right.position || new Date(right.createdAt).getTime() - new Date(left.createdAt).getTime() || left.title.localeCompare(right.title, "zh-CN"));
}
