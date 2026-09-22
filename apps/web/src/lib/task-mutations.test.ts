import assert from "node:assert/strict";
import test from "node:test";
import {
  applyPendingTaskMutations,
  isPendingTaskID,
  mutationReflectsInSnapshot,
  newPendingTaskID,
  withResolvedCreateID,
  type PendingTaskMutation,
  type PendingTaskTask,
} from "./task-mutations.ts";

// 这一组的每一条分支错了，表现都是"用户刚做的改动在界面上凭空消失一次"或者
// "任务看起来建好了其实没建" —— 靠读代码看不出来，必须跑。

function task(id: string, overrides: Partial<PendingTaskTask> = {}): PendingTaskTask {
  return { id, title: `任务 ${id}`, description: "", priority: "normal", status: "todo", updatedAt: "2026-09-17T00:00:00.000Z", ...overrides };
}

function project(id: string, tasks: PendingTaskTask[]) {
  return { id, name: `项目 ${id}`, tasks };
}

// 临时 id 一律用库里的生成函数：前缀是判断"是不是临时 id"的依据，测试自己手写一个
// 别的形状（比如 "pending-1"）就绕过了那条判断，测的就不是真实路径了。
// taskId 与卡片 id 必须**是同一个**（页面上就是这么造的）。分开写会让"删掉这条新建"
// 之类的用例打在不同 id 上，测出一条并不存在的通过路径。
function pendingCreate(options: { projectId?: string; taskId?: string; title?: string; description?: string } = {}): PendingTaskMutation {
  const id = options.taskId ?? newPendingTaskID();
  return {
    commandId: "",
    kind: "create",
    projectId: options.projectId ?? "p1",
    taskId: id,
    task: task(id, { title: options.title ?? "新任务", description: options.description ?? "" }),
  };
}

test("没有待确认改动时原样返回入参（同一个数组引用）", () => {
  const projects = [project("p1", [task("t1")])];
  assert.equal(applyPendingTaskMutations(projects, []), projects);
});

test("新建：卡片立刻出现在对应项目的最前面", () => {
  const mutation = pendingCreate();
  const projects = [project("p1", [task("t1"), task("t2")])];
  const result = applyPendingTaskMutations(projects, [mutation]);

  assert.equal(result[0].tasks.length, 3);
  assert.equal(result[0].tasks[0].id, mutation.taskId);
  // 原来的两张保持原顺序跟在后面
  assert.deepEqual(result[0].tasks.slice(1).map((entry) => entry.id), ["t1", "t2"]);
});

test("新建：只落在自己那个项目上，别的项目一动不动", () => {
  const mutation = pendingCreate({ projectId: "p2" });
  const projects = [project("p1", [task("t1")]), project("p2", [task("t2")])];
  const result = applyPendingTaskMutations(projects, [mutation]);

  assert.deepEqual(result[0].tasks.map((entry) => entry.id), ["t1"]);
  assert.deepEqual(result[1].tasks.map((entry) => entry.id), [mutation.taskId, "t2"]);
});

// 真 id 换回来之后，快照里那张卡的 id 和本地这张是同一个。不做判重就会同时画出两张
// 一模一样的卡（React 还会因为 key 重复报警），而且"同步中"那张永远摘不掉。
test("新建：快照里已经有这个 id 时不再重复画一张", () => {
  const realID = "task-real-1";
  const mutation = pendingCreate({ taskId: realID, title: "已落地" });
  const projects = [project("p1", [task(realID, { title: "已落地" }), task("t2")])];

  const result = applyPendingTaskMutations(projects, [mutation]);
  assert.deepEqual(result[0].tasks.map((entry) => entry.id), [realID, "t2"]);
});

test("编辑：补丁叠加上去，没提到的字段原样保留", () => {
  const projects = [project("p1", [task("t1", { title: "旧标题", priority: "low" })])];
  const result = applyPendingTaskMutations(projects, [{
    commandId: "",
    kind: "update",
    projectId: "p1",
    taskId: "t1",
    patch: { title: "新标题", description: "新描述" },
  }]);

  assert.equal(result[0].tasks[0].title, "新标题");
  assert.equal(result[0].tasks[0].description, "新描述");
  // 补丁没提的字段必须还在：整条替换会把优先级、状态一起抹掉。
  assert.equal(result[0].tasks[0].priority, "low");
  assert.equal(result[0].tasks[0].status, "todo");
});

test("删除：卡片立刻从列表里消失", () => {
  const projects = [project("p1", [task("t1"), task("t2")])];
  const result = applyPendingTaskMutations(projects, [{ commandId: "", kind: "delete", projectId: "p1", taskId: "t1" }]);

  assert.deepEqual(result[0].tasks.map((entry) => entry.id), ["t2"]);
});

test("同一条任务既被编辑又被删除时，删除优先且不会再冒出改动", () => {
  const projects = [project("p1", [task("t1")])];
  const result = applyPendingTaskMutations(projects, [
    { commandId: "", kind: "update", projectId: "p1", taskId: "t1", patch: { title: "改了也没用" } },
    { commandId: "", kind: "delete", projectId: "p1", taskId: "t1" },
  ]);

  assert.equal(result[0].tasks.length, 0);
});

test("新建又立刻删除：两条记录同时存在时，最终列表里不残留那张卡片", () => {
  const mutation = pendingCreate({ title: "手滑建的" });
  const projects = [project("p1", [task("t1")])];
  const result = applyPendingTaskMutations(projects, [
    mutation,
    { commandId: "", kind: "delete", projectId: "p1", taskId: mutation.taskId },
  ]);

  assert.deepEqual(result[0].tasks.map((entry) => entry.id), ["t1"]);
});

// 没被改到的项目必须保持同一个对象引用：它会被喂进 useMemo 的依赖链，每次都给新
// 对象会让整条链失效、白白重渲染（改一个项目的任务不该惊动所有项目）。
test("没有待确认改动的项目保持同一个对象引用", () => {
  const untouched = project("p2", [task("t2")]);
  const projects = [project("p1", [task("t1")]), untouched];
  const result = applyPendingTaskMutations(projects, [{ commandId: "", kind: "delete", projectId: "p1", taskId: "t1" }]);

  assert.equal(result[1], untouched);
  assert.notEqual(result[0], projects[0]);
});

test("待确认改动指向不存在的项目时安静跳过，不抛错", () => {
  const projects = [project("p1", [task("t1")])];
  const result = applyPendingTaskMutations(projects, [pendingCreate({ projectId: "没有这个项目" })]);

  assert.deepEqual(result[0].tasks.map((entry) => entry.id), ["t1"]);
});

// ── withResolvedCreateID：命令回执把真 id 带回来之后就地替换 ────────────────────

test("新建：真 id 到手后就地替换，卡片内容不变", () => {
  const mutation = pendingCreate({ title: "修复登录", description: "复现步骤" });
  const resolved = withResolvedCreateID(mutation, "task-real-9");

  assert.equal(resolved.taskId, "task-real-9");
  assert.equal(resolved.task?.id, "task-real-9");
  // 内容一个字都不能变：变了界面上那张卡就会闪一下。
  assert.equal(resolved.task?.title, "修复登录");
  assert.equal(resolved.task?.description, "复现步骤");
  assert.ok(!isPendingTaskID(resolved.task?.id || ""));
});

test("替换只对还没有真 id 的新建生效", () => {
  const already = pendingCreate({ taskId: "task-real-1" });
  assert.equal(withResolvedCreateID(already, "task-real-2").taskId, "task-real-1");

  const deletion: PendingTaskMutation = { commandId: "", kind: "delete", projectId: "p1", taskId: "t1" };
  assert.equal(withResolvedCreateID(deletion, "task-real-3").taskId, "t1");

  const mutation = pendingCreate();
  assert.equal(withResolvedCreateID(mutation, "").taskId, mutation.taskId);
});

// ── mutationReflectsInSnapshot：决定「同步中」什么时候可以摘掉 ──────────────────
// 判据必须保守：快照还没带回来就摘掉，用户会看到任务凭空消失一次。

test("新建（已拿到真 id）：判据是精确的 id 命中", () => {
  const mutation = pendingCreate({ taskId: "task-real-7", title: "修复登录" });

  assert.equal(mutationReflectsInSnapshot(mutation, [project("p1", [task("t1")])]), false);
  // 同名同描述的**别的**任务不能算数 —— 这正是真 id 存在的意义。
  assert.equal(mutationReflectsInSnapshot(mutation, [project("p1", [task("t9", { title: "修复登录" })])]), false);
  assert.equal(mutationReflectsInSnapshot(mutation, [project("p1", [task("task-real-7")])]), true);
});

test("新建（还没有真 id）：按内容比，命中即算已反映", () => {
  const mutation = pendingCreate({ title: "修复登录", description: "复现步骤" });

  assert.equal(mutationReflectsInSnapshot(mutation, [project("p1", [task("t1")])], [mutation]), false);
  assert.equal(mutationReflectsInSnapshot(mutation, [project("p1", [task("t1"), task("t9", { title: "修复登录", description: "复现步骤" })])], [mutation]), true);
});

test("新建：标题对了但描述不同，不算已反映", () => {
  const mutation = pendingCreate({ title: "修复登录", description: "甲" });
  assert.equal(mutationReflectsInSnapshot(mutation, [project("p1", [task("t9", { title: "修复登录", description: "乙" })])], [mutation]), false);
});

test("新建：首尾空格不该影响判定", () => {
  const mutation = pendingCreate({ title: "修复登录", description: "  复现  " });
  assert.equal(mutationReflectsInSnapshot(mutation, [project("p1", [task("t9", { title: " 修复登录 ", description: "复现" })])], [mutation]), true);
});

// ⚠️ 快照里的任务来自**线协议**：`title` 可能是 null（Go 侧 `json:"title"` 没有 omitempty，
// 键一定在、值仍可能是 null）。比对逻辑必须兜住 —— 一条脏数据就能让"新建任务"的确认逻辑抛错，
// 而它跑在乐观更新那条链上（症状是卡片永远摘不掉「同步中」，或整页崩）。
// 2026-09-18 复查抓到的第三处同类直取（前两处在 MobileRemotePage 里）。
test("新建：快照里有 title 为 null 的任务时，比对既不抛错也不算命中", () => {
  const mutation = pendingCreate({ title: "修复登录", description: "复现步骤" });
  const withDirty = [project("p1", [task("dirty", { title: null, description: null }), task("t1")])];
  assert.equal(mutationReflectsInSnapshot(mutation, withDirty, [mutation]), false);

  // 同一条脏数据在场时，"真的存在的那条"仍必须被认出来（否则「同步中」永远摘不掉）。
  const withMatch = [project("p1", [task("dirty", { title: null }), task("t9", { title: "修复登录", description: "复现步骤" })])];
  assert.equal(mutationReflectsInSnapshot(mutation, withMatch, [mutation]), true);
});

// 用户连着建两条同名同描述的任务时，第一条不能把第二条也"顶掉" —— 否则第二张卡会
// 当着用户的面消失，直到下一次快照才回来。
test("新建：两条同内容的任务要等快照里都出现才算都已反映", () => {
  const first = pendingCreate({ title: "重复", description: "一样" });
  const second = pendingCreate({ title: "重复", description: "一样" });
  const both = [first, second];
  const oneInSnapshot = [project("p1", [task("t9", { title: "重复", description: "一样" })])];
  const bothInSnapshot = [project("p1", [task("t9", { title: "重复", description: "一样" }), task("t10", { title: "重复", description: "一样" })])];

  assert.equal(mutationReflectsInSnapshot(first, oneInSnapshot, both), false);
  assert.equal(mutationReflectsInSnapshot(first, bothInSnapshot, both), true);
});

test("编辑：补丁里每个字段都对上才算已反映", () => {
  const mutation: PendingTaskMutation = { commandId: "", kind: "update", projectId: "p1", taskId: "t1", patch: { title: "新标题", priority: "high" } };

  assert.equal(mutationReflectsInSnapshot(mutation, [project("p1", [task("t1", { title: "旧标题", priority: "high" })])]), false);
  assert.equal(mutationReflectsInSnapshot(mutation, [project("p1", [task("t1", { title: "新标题", priority: "normal" })])]), false);
  assert.equal(mutationReflectsInSnapshot(mutation, [project("p1", [task("t1", { title: "新标题", priority: "high" })])]), true);
});

test("编辑：任务在快照里已经不存在，不算已反映", () => {
  const mutation: PendingTaskMutation = { commandId: "", kind: "update", projectId: "p1", taskId: "t1", patch: { title: "新标题" } };
  assert.equal(mutationReflectsInSnapshot(mutation, [project("p1", [task("t2")])]), false);
});

test("删除：快照里已经没有这条 id 才算已反映", () => {
  const mutation: PendingTaskMutation = { commandId: "", kind: "delete", projectId: "p1", taskId: "t1" };

  assert.equal(mutationReflectsInSnapshot(mutation, [project("p1", [task("t1")])]), false);
  assert.equal(mutationReflectsInSnapshot(mutation, [project("p1", [task("t2")])]), true);
});

test("项目不在快照里（快照还没同步到）时一律判为未反映", () => {
  const mutation = pendingCreate();
  assert.equal(mutationReflectsInSnapshot(mutation, []), false);
  assert.equal(mutationReflectsInSnapshot(mutation, null), false);
  assert.equal(mutationReflectsInSnapshot(mutation, undefined), false);
});
