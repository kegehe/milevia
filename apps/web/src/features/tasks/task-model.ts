export type Request = <T>(path: string, init?: RequestInit) => Promise<T>;

export type TaskStatus = "todo" | "running" | "awaiting_review" | "action_required" | "done" | "cancelled";
export type Priority = "urgent" | "high" | "normal" | "low";
export type TaskFilter = "active" | "todo" | "running" | "awaiting_review";
export type Dependency = { taskId: string; title: string; status: TaskStatus };
export type Blocker = Dependency;
export type TaskRun = { id: string; runId: string; sequence: number; status: string; createdAt: string; finishedAt?: string; failureReason: string };
export type VerificationRun = { id: string; phase: "task" | "review" | string; command: string; reviewedSha?: string; status: "passed" | "failed" | string; exitCode: number; output?: string; createdAt: string; completedAt?: string };
export type TaskEvent = { id: string; taskId: string; taskRunId?: string; type: string; payload: unknown; createdAt: string };
export type Task = {
  id: string;
  title: string;
  description: string;
  priority: Priority;
  pinned?: boolean;
  position: number;
  status: TaskStatus;
  dependsOn: Dependency[];
  blockedBy: Blocker[];
  blocks: Dependency[];
  lastRun?: TaskRun;
  orchestrationStatus?: string;
  orchestrationTargetBranch?: string;
  orchestrationUpdatedAt?: string;
  createdAt: string;
  updatedAt: string;
};
export type TaskDetail = Task & { canDispatch: boolean; blockReason?: string; runs: TaskRun[]; events: TaskEvent[]; verificationRuns: VerificationRun[] };

export const priorityLabels: Record<Priority, string> = { urgent: "紧急", high: "高", normal: "普通", low: "低" };
export const statusLabels: Record<TaskStatus, string> = { todo: "待处理", running: "执行中", awaiting_review: "待验收", action_required: "需处理", done: "已完成", cancelled: "已取消" };

export function taskDisplayStatus(task: Task): string {
  if (task.orchestrationStatus === "checking") return "编排收尾中";
  if (task.orchestrationStatus === "preparing") return "准备执行";
  if (task.orchestrationStatus === "implementing") return "自动执行中";
  if (task.orchestrationStatus === "queued") return "自动队列中";
  if (task.orchestrationStatus === "paused") return "自动队列已暂停";
  if (task.orchestrationStatus === "stopped") return "自动编排已停止";
  if (task.orchestrationStatus === "removing") return "自动编排清理中";
  if (task.orchestrationStatus === "needs_human") return "自动编排需处理";
  if (isTaskAwaitingMainMerge(task)) return `待合并 ${task.orchestrationTargetBranch || "目标分支"}`;
  if (task.status === "running" && task.lastRun?.status === "queued") return "队列中";
  // statusLabels 里查不到时**不能返回 undefined**：那会把徽标渲染成一个空胶囊，
  // 等于把"不知道"画成空白。后端 `tasks.status` 只是 `text not null default 'todo'`、
  // 没有 CHECK 约束（control-server/internal/app/task.go），旧数据或新版本都可能带来没建模的值。
  // 宁可把原始值原样报出来（「未知状态 paused」），也不要留白。
  return (statusLabels as Record<string, string | undefined>)[task.status] ?? `未知状态 ${task.status}`;
}

export function taskDisplayStatusClass(task: Task): string {
  // 队列中的任务（status=running、lastRun=queued）仅等待执行，属于执行管线的一环：
  // 沿用 running 的青色样式，而不是"需处理"（action_required）的琥珀色——后者语义是
  // 需要人工处理，两者字体可能一致但颜色必须区分，避免排队态被误读为待人工处理。
  return task.orchestrationStatus ? `orchestration-${task.orchestrationStatus}` : task.status;
}

export function isTaskOrchestrating(task: Task): boolean {
  return ["queued", "preparing", "implementing", "checking", "removing"].includes(task.orchestrationStatus || "");
}

export function isTaskAwaitingMainMerge(task: Task): boolean {
  return task.orchestrationStatus === "awaiting_main" || task.orchestrationStatus === "integrated_to_dev";
}

export function isTaskBlocked(task: Task): boolean {
  return (task.status === "todo" || task.status === "action_required") && task.blockedBy.length > 0;
}

export function canOfferDispatch(task: Task): boolean {
  return task.status === "todo" && !isTaskBlocked(task);
}

export function canRedispatch(task: Task): boolean {
  if (task.status === "action_required") {
    // 编排清理中不提供重新下发，避免与收尾流程竞争。
    return !isTaskOrchestrating(task) && !isTaskBlocked(task)
      && task.lastRun?.status !== "queued" && task.lastRun?.status !== "running";
  }
  if (task.status === "awaiting_review") {
    // 待验收任务可直接重新下发（跳过"要求修改"这一步），但仅限非编排管理的任务：
    // 只要挂了编排 job（paused/needs_human/checking/待合并等任何状态），就必须走
    // resume/验收/要求修改等编排感知路径，避免与编排收尾互相覆盖任务状态。
    // blockedBy 与后端一致：前置被重开会同样拦下发。
    return !task.orchestrationStatus
      && task.blockedBy.length === 0
      && task.lastRun?.status !== "queued" && task.lastRun?.status !== "running";
  }
  return false;
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
  if (task.orchestrationStatus === "checking") return "自动编排正在提交实现，等待人工验证";
  if (task.orchestrationStatus === "preparing") return "自动编排正在准备工作区";
  if (task.orchestrationStatus === "implementing") return "自动编排正在执行任务";
  if (task.orchestrationStatus === "queued") return "已进入自动编排队列";
  if (task.orchestrationStatus === "paused") return "自动编排已暂停";
  if (task.orchestrationStatus === "stopped") return "自动编排已停止，可重新开始";
  if (task.orchestrationStatus === "removing") return "自动编排正在停止执行并清理工作区";
  if (task.orchestrationStatus === "needs_human") return "自动编排需要人工处理";
  if (isTaskAwaitingMainMerge(task)) return `分支已验证，可合并至 ${task.orchestrationTargetBranch || "目标分支"}`;
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
  // 全新未执行、无阻塞、无编排状态的待办任务没有任何可补充的状态说明：
  // 是否可下发已由行内的下发按钮表达，这里不重复提示。
  return "";
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

// 队列顺序 = 用户的手工顺序：置顶任务恒在最前，其余一律按 position 升序。
//
// 【规则 · 勿改】position 必须排在状态分组与优先级之前。服务端新建任务（createTask
// 与「优化建议转任务」convertInsightToTask）都把 position 取为当前最大值 +1，即
// position 升序 === 追加到队列末尾。一旦让 queueRank/priorityRank 压过 position：
//   · 新建任务会被状态、优先级插到队列中间，而不是最后；
//   · 用户拖拽调整好的顺序会被下一次渲染按优先级重新打散（拖了等于没拖）。
// queueRank/priorityRank 仅作为 position 相同时的兜底，不再决定手工顺序。

// 看板与列表视图共用的稳定顺序：position 升序 → 创建时间升序 → 标题。
// position 是手工顺序的唯一依据（服务端契约：新建任务 position = 当前最大值 +1），
// 所以新任务永远排在最后；同 position 时旧的在前——正常路径下写入的 position 互不相同
// （见 positionForMove），这道兜底只防历史数据或将来新增写入路径引入重复值，
// 且方向必须朝"旧的前面、新的后面"，否则新建任务会被顶到旧任务之上。
export function compareTaskOrder(left: Task, right: Task): number {
  return left.position - right.position
    || new Date(left.createdAt).getTime() - new Date(right.createdAt).getTime()
    || left.title.localeCompare(right.title, "zh-CN");
}

// 仅按手工位置排序（不含置顶优先）：插入位置的计算必须基于这份"真实位置顺序"，
// 因为置顶只影响显示、不改变 position。
export function sortByPosition(tasks: Task[]): Task[] {
  return [...tasks].sort(compareTaskOrder);
}

// 位置精度：只用于排序，1e-6 足以区分任何真实拖拽，同时抹掉 2*x - y 留下的浮点尾巴
// （5.4 与 5.5 会算出 5.300000000000001）。
const POSITION_SCALE = 1e6;

// 把 moved 移到 anchor 之前/之后（anchor 为 null = 追加到末尾），返回应写入的 position。
// 返回 null = 本次重排无法安全落地（任务/锚点已不在列表里、或算不出不与既有 position
// 冲突的值），调用方直接放弃这次操作，不要拿旧值或 0 兜底写进去。
//
// 【规则 · 勿改】三条硬约束，都是踩过的坑：
//
// 1. allTasks 必须是**全量任务**（不要传筛选/分列后的可见子集）。筛掉的任务（已完成、
//    其它看板列、被搜索过滤掉的）位置照样参与排序比较，用可见子集算中点会与它们撞位。
// 2. 函数内部自己按 position 排序——接口返回的任务顺序不等于位置顺序，少这一步就会
//    把"移到末尾"算成别人的位置（真实发生过）。
// 3. 贴到最前/最后时步长必须取**相邻任务的间距**，不能用 ±0.5 / ±1 这类定值。定值步长
//    在密集列表里必然撞车：位置 1/2/3 上连续上移，第二次就会算出与另一任务相同的
//    position，顺序随即退化到 createdAt 兜底——用户看到的是"上移了但顺序没变"。
//
// 取中点 / 向外让出一个间距，结果都严格落在邻居之间（或两端之外）；返回前再校验一次
// 不与任何既有 position 相同，冲突就返回 null，宁可不改也不写乱。
export function positionForMove(allTasks: Task[], movedID: string, anchorID: string | null, placement: "before" | "after"): number | null {
  const moved = allTasks.find((task) => task.id === movedID);
  if (!moved) return null;
  const rest = sortByPosition(allTasks).filter((task) => task.id !== movedID);
  let index = rest.length;
  if (anchorID) {
    const anchorIndex = rest.findIndex((task) => task.id === anchorID);
    if (anchorIndex < 0) return null;
    index = placement === "before" ? anchorIndex : anchorIndex + 1;
  }
  const prev = rest[index - 1];
  const next = rest[index];
  if (!prev && !next) return moved.position;
  let raw: number;
  if (prev && next) raw = (prev.position + next.position) / 2;
  else if (prev) {
    // 贴尾：向右让出"prev 与它前面那位的间距"，结果严格大于 prev
    const before = rest[index - 2];
    raw = prev.position + (before ? prev.position - before.position : 1);
  } else {
    // 贴头：向左让出"next 与它后面那位的间距"，结果严格小于 next
    const after = rest[index + 1];
    raw = next.position - (after ? after.position - next.position : 1);
  }
  const candidate = Math.round(raw * POSITION_SCALE) / POSITION_SCALE;
  const lower = prev ? prev.position : Number.NEGATIVE_INFINITY;
  const upper = next ? next.position : Number.POSITIVE_INFINITY;
  const usable = candidate > lower && candidate < upper && rest.every((task) => task.position !== candidate);
  return usable ? candidate : null;
}

// 把拖拽「插入槽位」（0..n，插入线在两行之间的位置）翻译成「锚点 + 侧别」。
//
// 【规则 · 勿改】一律取**槽位上一行的 after**（贴头取首行的 before），不要取"槽位下一行的
// before"：两者在可见列表里看起来等价，但当槽位两侧的间隙中间还夹着别的列或被筛掉的任务时，
// 后者会把任务推到整个间隙的另一端（列表视图里像跳了好几位）。实测过：
// 待办列 [c,a,n1] 里把 c 拖到 a 下面，取"下一行 before"会落到 b/n1 之间（3.5），
// 取"上一行 after"才落在 a 紧跟之后（1.5）——列内看起来一样，但全局顺序差了两位。
// 返回 null = 该范围内一个任务都没有（空列），调用方应保持原 position。
export function anchorForSlot(ordered: Task[], slot: number): { anchorID: string; placement: "before" | "after" } | null {
  if (ordered.length === 0) return null;
  // 贴头（槽位 ≤ 0）：放到首行之前
  if (slot <= 0) return { anchorID: ordered[0].id, placement: "before" };
  // 其余槽位：锚点 = **槽位上面那一行**（越过末尾/越界 → 最后一行），放到它之后。
  // 注意夹取的是"上一行"的下标，不是槽位本身——夹错了会整体差一行。
  const index = Math.min(slot, ordered.length) - 1;
  return { anchorID: ordered[index].id, placement: "after" };
}

// 这次拖拽是否"本来就已经是那个位置"（是则不必写服务端）。
// 用途：避免每次无操作拖拽都把 position 挤向邻居中点、把间隙越挤越小，最后算不出安全值。
//
// 【规则 · 勿改】判断必须基于**服务端刚返回的列表**，不要用界面上的快照：界面最长可能落后
// 一个轮询周期（另一个视图/另一台设备刚重排过），用过期快照判断会把一次真实拖拽误判成
// "没动"而静默丢掉。
export function isSameSlot(ordered: Task[], movedID: string, anchorID: string, placement: "before" | "after"): boolean {
  const sorted = sortByPosition(ordered);
  const currentIndex = sorted.findIndex((task) => task.id === movedID);
  if (currentIndex < 0) return false;
  const rest = sorted.filter((task) => task.id !== movedID);
  const anchorIndex = rest.findIndex((task) => task.id === anchorID);
  if (anchorIndex < 0) return false;
  // "移除自己再插回同一个下标"得到的还是原列表，所以槽位等于当前下标即为无操作
  return (placement === "before" ? anchorIndex : anchorIndex + 1) === currentIndex;
}

export function sortQueueTasks(tasks: Task[]): Task[] {
  // 置顶恒在最前 → position（手工顺序）→ 状态分组 → 优先级 → 创建时间 → 标题。
  // position 必须排在状态与优先级之前，理由见上方【规则 · 勿改】。
  return [...tasks].sort((left, right) => Number(Boolean(right.pinned)) - Number(Boolean(left.pinned)) || left.position - right.position || queueRank(left) - queueRank(right) || priorityRank[left.priority] - priorityRank[right.priority] || new Date(left.createdAt).getTime() - new Date(right.createdAt).getTime() || left.title.localeCompare(right.title, "zh-CN"));
}
