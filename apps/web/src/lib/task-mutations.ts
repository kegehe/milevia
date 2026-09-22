// 手机端任务增删改的乐观更新规则。
//
// 抽成独立模块是为了能直接跑逻辑用例（src/lib/task-mutations.test.ts）：这段规则的
// 每一条分支错了，表现都是"用户刚做的改动在界面上凭空消失一次"或者"任务看起来建好了
// 其实没建"，靠读代码看不出来。
//
// 背景：老实现是"发命令 → 轮询等最多 30 秒 → 再整份重拉快照"，期间整屏 busy。真机
// 实测（2026-09-16）命令往返从下午的 3 秒劣化到晚上的 19~75 秒，全部越过 30 秒上限：
// 用户看到的是"点了没反应、过一会儿还报失败"，而桌面端其实晚几十秒才执行完。

// ⚠️ `title` / `description` 与快照里的 `Task` 是**同一份线协议数据**，所以类型也必须一样地
// 允许 null（Go 侧 `json:"title"` 没有 omitempty，键一定在、值仍可能是 null）。
// 这里写 `string` 的后果不是"类型不准"而是**真会炸**：`mutationReflectsInSnapshot` 里要
// `task.title.trim()` 去比内容，一条 title 为 null 的旧任务就能让"新建任务"的确认逻辑抛错，
// 而它发生在乐观更新那条链上（表现是卡片永远停在「同步中」或整页崩）。
// 2026-09-18 复查时把 `MobileRemotePage` 的 `Task` 收紧成 `string | null`，tsc 当场点出这里。
export type PendingTaskTask = {
  id: string;
  title: string | null;
  description?: string | null;
  priority: string;
  status: string;
  updatedAt: string;
};

export type PendingTaskMutation = {
  commandId: string;
  kind: "create" | "update" | "delete";
  projectId: string;
  taskId: string;
  // create 用：本地先摆出来的那张卡片（id 是临时 id，桌面端会分配真正的 id）。
  task?: PendingTaskTask;
  // update 用：要叠加到快照上的字段。
  patch?: Record<string, unknown>;
};

// 本地临时 id 的前缀。命令回执会把桌面端分配的真 id 带回来，届时就地替换（见
// withResolvedCreateID），此后判据变精确。
//
// 生成也放在这里（newPendingTaskID）：前缀是个约定，"是不是临时 id"全靠它判断，
// 生成端与判断端分在两个文件里迟早会漂开 —— 一旦漂开，新建的卡片就永远等不到
// 「已反映」，永远停在「同步中」。
export const PENDING_TASK_ID_PREFIX = "pending-task-";

export function isPendingTaskID(id: string): boolean {
  return id.startsWith(PENDING_TASK_ID_PREFIX);
}

export function newPendingTaskID(): string {
  const suffix = globalThis.crypto?.randomUUID?.() || `${Date.now()}-${Math.random().toString(36).slice(2)}`;
  return `${PENDING_TASK_ID_PREFIX}${suffix}`;
}

// withResolvedCreateID 把一次新建的临时 id 换成电脑端回执里的真 id。
//
// 真 id 到手之后，"这次新建落地了吗"就不用再靠标题+描述去猜：判据变成"快照里有没有
// 这个 id"。靠内容猜在两种情况下会说谎 —— 用户先后建了两条同名同描述的任务，或者
// 快照里本来就有一条一样的 —— 猜错的代价是那张卡片当着用户的面消失一次。
export function withResolvedCreateID(mutation: PendingTaskMutation, taskID: string): PendingTaskMutation {
  if (mutation.kind !== "create" || !taskID || !mutation.task || !isPendingTaskID(mutation.task.id)) return mutation;
  return {
    ...mutation,
    taskId: taskID,
    // 卡片本身不改内容，只换 id：界面上的那张卡不会闪。
    task: { ...mutation.task, id: taskID },
  };
}

// applyPendingTaskMutations 把还没被电脑端确认的本地改动叠加到快照上。
//
// 叠加顺序固定为 先删、再改、最后加：一次"新建又立刻删除"会同时留下两条记录，
// 顺序不定就会出现"删掉的又冒出来"。
//
// 没有待确认改动时**原样返回入参**（同一个数组引用），让 React 的依赖比较能短路。
export function applyPendingTaskMutations<P extends { id: string; tasks: PendingTaskTask[] }>(
  projects: P[],
  pending: Iterable<PendingTaskMutation>,
): P[] {
  const items = Array.from(pending);
  if (items.length === 0) return projects;
  const deletes = new Set<string>();
  const patches = new Map<string, Record<string, unknown>>();
  const creates = new Map<string, PendingTaskTask[]>();
  for (const mutation of items) {
    if (mutation.kind === "delete") deletes.add(mutation.taskId);
    else if (mutation.kind === "update" && mutation.patch) patches.set(mutation.taskId, mutation.patch);
    else if (mutation.kind === "create" && mutation.task) {
      const list = creates.get(mutation.projectId) || [];
      list.push(mutation.task);
      creates.set(mutation.projectId, list);
    }
  }
  return projects.map((entry) => {
    const removed = entry.tasks.filter((task) => !deletes.has(task.id));
    const next = removed.map((task) => {
      const patch = patches.get(task.id);
      // 任务已经被删掉了就不再叠加改动：改动只对还存在的任务有意义。
      return patch ? { ...task, ...patch } : task;
    });
    // 新建的排在最前：跟桌面端"新任务默认排在队首"的观感一致，用户一眼就能看到
    // 自己刚加的东西，而不是要滚到列表底部去找。
    //
    // 两处过滤都不能省：同一条已删掉的不许复活（deletes），**已经出现在快照里**的
    // 也不许再画一遍 —— 真 id 换回来之后，快照里那张卡的 id 和本地这张是同一个，
    // 不判重就会渲染出两张同样的卡（React 还会因为 key 重复报警）。
    const snapshotIDs = new Set(entry.tasks.map((task) => task.id));
    const added = (creates.get(entry.id) || []).filter((task) => !deletes.has(task.id) && !snapshotIDs.has(task.id));
    // 这个项目没有任何待确认改动时**原样返回同一个对象**：调用方拿它去喂 useMemo，
    // 每次都给新对象会让整条依赖链失效、白白重渲染（改一个项目的任务不该惊动所有项目）。
    if (added.length === 0 && next.length === entry.tasks.length && next.every((task, index) => task === entry.tasks[index])) {
      return entry;
    }
    const tasks = added.length > 0 ? [...added, ...next] : next;
    return { ...entry, tasks };
  });
}

// mutationReflectsInSnapshot 判断"这次改动的结果，快照里已经有了吗"。
//
// 判据刻意保守：宁可在界面上多留一会儿「同步中」，也不能在快照还没带回来之前就把
// 本地改动摘掉 —— 那会让任务凭空消失一次。快照有上传节流，命令完成后拿到的很可能
// 是命令之前那一版，所以这个判断必须真的比对内容，而不能只看"命令成功了"。
export function mutationReflectsInSnapshot(
  mutation: PendingTaskMutation,
  projects: { id: string; tasks: PendingTaskTask[] }[] | null | undefined,
  pending: Iterable<PendingTaskMutation> = [],
): boolean {
  const project = (projects || []).find((entry) => entry.id === mutation.projectId);
  if (!project) return false;
  if (mutation.kind === "delete") return !project.tasks.some((task) => task.id === mutation.taskId);
  if (mutation.kind === "update") {
    const task = project.tasks.find((entry) => entry.id === mutation.taskId);
    if (!task || !mutation.patch) return false;
    const record = task as unknown as Record<string, unknown>;
    return Object.entries(mutation.patch).every(([key, value]) => record[key] === value);
  }
  const created = mutation.task;
  // 没有卡片可比的创建（理论上不会发生）按"已反映"处理，免得永远摘不掉。
  if (!created) return true;
  // 电脑端分配的真 id 已经拿到：判据是精确的。
  if (!isPendingTaskID(created.id)) return project.tasks.some((task) => task.id === created.id);
  // 还没有真 id（命令回执里没带回来）：只能按内容比。此时要求快照里同内容的条数
  // **不少于**还没落地的同内容创建数 —— 否则用户连续建两条一模一样的任务时，
  // 第二条会被第一条误判成"已反映"，卡片当着用户的面消失一次。
  // 两个字段都兜 null：`created` 是本地刚建的（一定有值），但 `task` 来自**快照**（线协议）。
  // 少了兜底，快照里任意一条 title 为 null 的任务都会让这次比对抛错。
  const title = (created.title || "").trim();
  const description = created.description?.trim() || "";
  const matches = (task: PendingTaskTask) => (task.title || "").trim() === title && (task.description?.trim() || "") === description;
  const inSnapshot = project.tasks.filter(matches).length;
  const stillPending = Array.from(pending).filter((item) => item.kind === "create" && item.projectId === mutation.projectId && item.task && isPendingTaskID(item.task.id) && matches(item.task)).length;
  return inSnapshot >= stillPending;
}
