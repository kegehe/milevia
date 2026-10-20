import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

import { anchorForSlot, canOfferDispatch, canRedispatch, compareTaskOrder, filterQueueTasks, isSameSlot, isTaskAwaitingMainMerge, isTaskOrchestrating, positionForMove, sortByPosition, sortQueueTasks, taskDisplayStatus, taskDisplayStatusClass, taskQueueNote, taskRunStatusLabel, taskRunStatusTone, type Task } from "./task-model.ts";

// 全仓 CSS 扫描：用来钉住"同一件事只能写一处"这类跨文件约定（同一份样式写两遍时，
// 后写的那份会静默压掉前面的）。
function collectCssFiles(dir: string): string[] {
  const out: string[] = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    if (entry.name.startsWith(".") || entry.name === "node_modules") continue;
    const full = join(dir, entry.name);
    if (entry.isDirectory()) out.push(...collectCssFiles(full));
    else if (entry.name.endsWith(".css")) out.push(full);
  }
  return out;
}

const makeTask = (id: string, overrides: Partial<Task> = {}): Task => ({
  id,
  title: id,
  description: "Task description",
  priority: "normal",
  position: 1,
  status: "todo",
  dependsOn: [],
  blockedBy: [],
  blocks: [],
  createdAt: "2026-07-20T00:00:00Z",
  updatedAt: "2026-07-20T00:00:00Z",
  ...overrides,
});

test("orders the queue by manual position before status and priority", () => {
  // position 是服务端保存的手工顺序（新建任务 = 当前最大值 +1），队列必须照它排，
  // 否则新建任务会被状态分组或优先级插到队列中间。
  const ready = makeTask("ready", { position: 1 });
  const running = makeTask("running", { status: "running", position: 2 });
  const review = makeTask("review", { status: "awaiting_review", position: 3 });
  const repair = makeTask("repair", { status: "action_required", position: 4 });
  const done = makeTask("done", { status: "done", position: 5 });
  const cancelled = makeTask("cancelled", { status: "cancelled", position: 6 });

  const visible = filterQueueTasks([ready, running, review, repair, done, cancelled], "active");

  // 位置顺序优先于状态分组：旧实现会把 running 提到最前、把 repair/review 排到末尾。
  assert.deepEqual(sortQueueTasks(visible).map((task) => task.id), ["ready", "running", "review", "repair"]);
});

test("falls back to status rank only when positions tie", () => {
  const ready = makeTask("ready", { position: 2 });
  const running = makeTask("running", { status: "running", position: 2 });
  const review = makeTask("review", { status: "awaiting_review", position: 2 });
  const repair = makeTask("repair", { status: "action_required", position: 2 });

  assert.deepEqual(sortQueueTasks([ready, running, review, repair]).map((task) => task.id), ["running", "ready", "repair", "review"]);
});

test("appends a newly created task to the end of the queue", () => {
  // 回归：新建任务（含「优化建议添加到任务」）的 position 取当前最大值 +1，
  // 必须落在最后，不能被状态分组（待验收/执行中）或优先级顶到前面。
  const queue = [
    makeTask("review", { status: "awaiting_review", priority: "urgent", position: 1 }),
    makeTask("running", { status: "running", priority: "high", position: 2 }),
    makeTask("pinned", { priority: "high", position: 3, pinned: true }),
  ];
  const created = makeTask("created", { priority: "low", position: 4 });

  assert.deepEqual(sortQueueTasks([...queue, created]).map((task) => task.id), ["pinned", "review", "running", "created"]);
});

test("keeps the manually dragged order even when priorities differ", () => {
  // 用户把手上的高优先级任务拖到末尾后，顺序必须保持，不能被优先级重新打散。
  const top = makeTask("top", { priority: "low", position: 1 });
  const bottom = makeTask("bottom", { priority: "urgent", position: 2 });

  assert.deepEqual(sortQueueTasks([bottom, top]).map((task) => task.id), ["top", "bottom"]);
});

test("keeps the older task ahead when positions collide", () => {
  // 同一 position 时旧的在前：新建任务不能被顶到旧任务前面去（历史数据可能已有重复值）。
  const older = makeTask("older", { position: 1, createdAt: "2026-07-01T00:00:00Z" });
  const newer = makeTask("newer", { position: 1, createdAt: "2026-07-21T00:00:00Z" });

  assert.deepEqual(sortQueueTasks([newer, older]).map((task) => task.id), ["older", "newer"]);
});

test("computes a free position between the drop neighbours", () => {
  const tasks = [makeTask("a", { position: 1 }), makeTask("b", { position: 2 }), makeTask("c", { position: 3 })];

  // 把 c 拖到 b 之前 → 落在 a 与 b 之间；把 a 拖到 b 之后 → 落在 b 与 c 之间
  assert.equal(positionForMove(tasks, "c", "b", "before"), 1.5);
  assert.equal(positionForMove(tasks, "a", "b", "after"), 2.5);
});

test("places head and tail drops strictly outside every used position", () => {
  // 位置密集（间距 0.1）：±1 这类定值步长在这里必然撞位，按相邻间距让出一格才不会。
  const tasks = [makeTask("a", { position: 5.4 }), makeTask("b", { position: 5.5 }), makeTask("c", { position: 5.6 })];

  const head = positionForMove(tasks, "c", "a", "before");
  const tail = positionForMove(tasks, "a", "c", "after");

  assert.equal(head, 5.3);
  assert.equal(tail, 5.7);
  for (const value of [head, tail]) {
    assert.equal(tasks.some((task) => task.position === value), false);
  }
});

test("sorts internally so a raw API order cannot break the drop target", () => {
  // 接口返回的任务顺序不等于位置顺序：传乱序数组也必须算对（曾经把"移到最前"算成了
  // 别人占用的位置，导致顺序错乱）。
  const tasks = [makeTask("c", { position: 3 }), makeTask("a", { position: 1 }), makeTask("b", { position: 2 })];

  assert.equal(positionForMove(tasks, "a", "c", "after"), 4);
  assert.equal(positionForMove(tasks, "c", "a", "before"), 0);
  assert.equal(positionForMove(tasks, "b", "a", "before"), -1);
});

test("refuses to guess a position when the task or the anchor is gone", () => {
  const tasks = [makeTask("a", { position: 1 }), makeTask("b", { position: 2 })];

  assert.equal(positionForMove(tasks, "ghost", "a", "before"), null);
  assert.equal(positionForMove(tasks, "a", "ghost", "before"), null);
});

test("refuses to write a position another task already occupies", () => {
  // 历史数据里存在重复 position 时，宁可不改也不要再写一个冲突值（顺序会退到 createdAt 兜底）。
  const tasks = [makeTask("a", { position: 1 }), makeTask("b", { position: 1 }), makeTask("c", { position: 2 })];

  assert.equal(positionForMove(tasks, "c", "b", "after"), null);
});

test("keeps every position unique and the order exact across a sequence of drags", () => {
  // 回归（旧实现的核心缺陷）：±0.5 / ±1 的定值步长在密集列表里拖几次就会算出重复的
  // position，顺序随即退化到 createdAt 兜底——用户看到"拖了但顺序没变"。
  // 这里逐步断言：每一步之后 position 两两不同，且按 position 排出来的顺序与预期完全一致。
  const scripted: { moved: string; anchor: string | null; placement: "before" | "after" }[] = [
    { moved: "e", anchor: "a", placement: "before" },
    { moved: "e", anchor: "c", placement: "after" },
    { moved: "a", anchor: null, placement: "after" },
    { moved: "c", anchor: "a", placement: "before" },
    { moved: "b", anchor: "d", placement: "after" },
    { moved: "d", anchor: "a", placement: "before" },
    { moved: "b", anchor: "e", placement: "before" },
  ];
  let tasks = ["a", "b", "c", "d", "e"].map((id, index) => makeTask(id, { position: index + 1 }));
  let model = ["a", "b", "c", "d", "e"];

  for (const step of scripted) {
    const position = positionForMove(tasks, step.moved, step.anchor, step.placement);
    assert.notEqual(position, null);
    tasks = tasks.map((task) => (task.id === step.moved ? { ...task, position: position as number } : task));
    model = model.filter((id) => id !== step.moved);
    if (step.anchor) {
      const anchorIndex = model.indexOf(step.anchor);
      assert.notEqual(anchorIndex, -1);
      model.splice(step.placement === "before" ? anchorIndex : anchorIndex + 1, 0, step.moved);
    } else {
      model.push(step.moved);
    }

    const positions = tasks.map((task) => task.position);
    assert.equal(new Set(positions).size, positions.length);
    assert.deepEqual(sortByPosition(tasks).map((task) => task.id), model);
    assert.deepEqual([...tasks].sort(compareTaskOrder).map((task) => task.id), model);
  }
});

test("detects a drop that would not change the order", () => {
  const tasks = [makeTask("a", { position: 1 }), makeTask("b", { position: 2 }), makeTask("c", { position: 3 })];

  // 已经在目标位置：a 就在 b 前面、c 就在 b 后面
  assert.equal(isSameSlot(tasks, "a", "b", "before"), true);
  assert.equal(isSameSlot(tasks, "c", "b", "after"), true);
  // 真正需要动的
  assert.equal(isSameSlot(tasks, "c", "a", "before"), false);
  assert.equal(isSameSlot(tasks, "a", "b", "after"), false);
  // 任务或锚点不存在时一律当作"需要动"，交给 positionForMove 去拒绝
  assert.equal(isSameSlot(tasks, "a", "ghost", "before"), false);
  assert.equal(isSameSlot(tasks, "ghost", "a", "before"), false);
});

test("routes every manual reorder through positionForMove", () => {
  const queue = readFileSync(new URL("./TaskQueue.tsx", import.meta.url), "utf8");
  const board = readFileSync(new URL("./TaskBoard.tsx", import.meta.url), "utf8");

  for (const source of [queue, board]) {
    // 必须调用统一的插入位置计算，且第一个参数是**服务端最新列表**（不是本组件可能过期的快照，
    // 也不是筛选后的可见子集）——用过期快照算出的位置会与真实顺序错位
    assert.match(source, /positionForMove\(latest,/);
    // 反例守卫：position 的加减算术只允许留在 task-model（定值步长 / 可见子集中点都会撞位）
    assert.doesNotMatch(source, /\.position\s*[+-]/);
  }
});

test("keeps pinned and stale-snapshot writes honest", () => {
  const queue = readFileSync(new URL("./TaskQueue.tsx", import.meta.url), "utf8");
  const board = readFileSync(new URL("./TaskBoard.tsx", import.meta.url), "utf8");

  // 拖动置顶任务必须同时解除置顶，否则置顶优先渲染会让"拖到别处"看不出变化；
  // 且"位置本来就对"时也必须写（否则只跳过位置写入会连解除置顶一起跳过）
  assert.match(queue, /const unpin = Boolean\(task\.pinned\);/);
  assert.match(queue, /if \(sameSlot && !unpin\) return;/);
  assert.match(queue, /if \(unpin\) body\.pinned = false;/);
  // 置顶只切 pinned：不能回传可能过期 10s 的 position 快照
  assert.doesNotMatch(queue, /pinned: p, position: t\.position/);
  // 编辑既有任务时不带 position（旧快照会覆盖这期间的重排）；新建才用 0 号追加哨兵
  assert.match(board, /task \? \{ description, priority \} : \{ description, priority, position: 0 \}/);
});

test("derives the board drop slot from the pointer, never from drag state", () => {
  const board = readFileSync(new URL("./TaskBoard.tsx", import.meta.url), "utf8");
  const dropSource = board.slice(board.indexOf("const handleColumnDrop"), board.indexOf("const handleTaskDragStart"));

  // 落点必须由本次 drop 的坐标算出（跨列/在列内空白处松手时，dragOverIndex 可能残留旧值）
  assert.match(dropSource, /e\.clientY < rect\.top \+ rect\.height \/ 2/);
  assert.match(dropSource, /columnRefs\.current\[columnID\]\?\.querySelectorAll/);
  // 只允许重置拖拽态（setDragOverIndex(null)），不允许读取它来定落点
  assert.doesNotMatch(dropSource, /dragOverIndex\s*(===|!==|<=|>=|<|>)|Math\.min\(dragOverIndex/);
  // 槽位 → 锚点必须走共享规则，不许在组件里再手写一遍（看板与侧栏口径会漂）
  assert.match(dropSource, /anchorForSlot\(columnTasks, targetIndex\)/);
  assert.doesNotMatch(dropSource, /targetIndex <= 0 \? columnTasks\[0\]/);
});

test("translates a drop slot into an anchor and a side", () => {
  const tasks = [makeTask("a", { position: 1 }), makeTask("b", { position: 2 }), makeTask("c", { position: 3 })];

  // 贴头：锚点取首行、放它前面
  assert.deepEqual(anchorForSlot(tasks, 0), { anchorID: "a", placement: "before" });
  assert.deepEqual(anchorForSlot(tasks, -3), { anchorID: "a", placement: "before" });
  // 其余槽位：锚点取"槽位上一行"、放它后面（不是"下一行 + before"）
  assert.deepEqual(anchorForSlot(tasks, 1), { anchorID: "a", placement: "after" });
  assert.deepEqual(anchorForSlot(tasks, 2), { anchorID: "b", placement: "after" });
  assert.deepEqual(anchorForSlot(tasks, 3), { anchorID: "c", placement: "after" });
  assert.deepEqual(anchorForSlot(tasks, 99), { anchorID: "c", placement: "after" });
  // 空列表没有锚点 → 调用方保持原 position
  assert.equal(anchorForSlot([], 0), null);
  assert.equal(anchorForSlot([], 4), null);
});

test("lands a card right below the row it was dropped on, not at the gap's far end", () => {
  // 真实浏览器里复现过的场景：待办列可见 [a, n1]，中间夹着待验收的 b 和已完成的 d。
  const all = [
    makeTask("c", { position: 0.5 }),
    makeTask("a", { position: 1 }),
    makeTask("b", { position: 2, status: "awaiting_review" }),
    makeTask("d", { position: 3, status: "done" }),
    makeTask("n1", { position: 4 }),
  ];
  const visibleTodo = [all[1], all[4]];

  // 把 c 拖到 a 的下半 → 槽位 1 → 锚点 a / after → 1.5（紧跟 a）
  const target = anchorForSlot(visibleTodo, 1);
  assert.notEqual(target, null);
  assert.equal(positionForMove(all, "c", (target as { anchorID: string }).anchorID, (target as { placement: "before" | "after" }).placement), 1.5);
  // 对照：取"槽位下一行 n1 的 before"会落到 3.5（b 之后），全局顺序差两位
  assert.equal(positionForMove(all, "c", "n1", "before"), 3.5);
});

test("keeps positions unique and ordered over a long randomized sequence of moves", () => {
  // 伪随机但确定（Lehmer RNG，固定种子）：200 步随机"挑任务 + 挑锚点 + 选侧别"，
  // 每步都断言 position 两两不同、且按 position 排出来的顺序与预期完全一致。
  let seed = 20260912;
  const random = (bound: number) => {
    seed = (seed * 48271) % 2147483647;
    return seed % bound;
  };
  let tasks = Array.from({ length: 12 }, (_, index) => makeTask(`t${index}`, { position: index + 1 }));
  let model = tasks.map((task) => task.id);
  let moves = 0;

  for (let step = 0; step < 200; step += 1) {
    const movedID = model[random(model.length)];
    const anchorID = model[random(model.length)];
    if (movedID === anchorID) continue;
    const placement = random(2) === 0 ? "before" : "after";
    const position = positionForMove(tasks, movedID, anchorID, placement);

    assert.notEqual(position, null, `第 ${step} 步：${movedID} 在 ${anchorID} ${placement} 算不出位置`);
    assert.ok(Number.isFinite(position as number), `第 ${step} 步：position 不是有限数`);
    moves += 1;

    tasks = tasks.map((task) => (task.id === movedID ? { ...task, position: position as number } : task));
    model = model.filter((id) => id !== movedID);
    const anchorIndex = model.indexOf(anchorID);
    model.splice(placement === "before" ? anchorIndex : anchorIndex + 1, 0, movedID);

    const sorted = sortByPosition(tasks).map((task) => task.id);
    assert.equal(new Set(sorted).size, sorted.length, `第 ${step} 步：出现重复 position`);
    assert.deepEqual(sorted, model, `第 ${step} 步：顺序与预期不一致`);
  }

  assert.ok(moves > 150, `实际只执行了 ${moves} 次移动`);
  assert.ok(tasks.every((task) => Number.isFinite(task.position) && Number.isSafeInteger(Math.round(task.position * 1e6))));
});

test("shows failure reason in queue note for action_required tasks", () => {
  const failed = makeTask("failed", {
    status: "action_required",
    lastRun: { id: "r1", runId: "run1", sequence: 2, status: "failed", createdAt: "2026-07-20T00:00:00Z", failureReason: "Claude exited: exit status 1" },
  });
  const stopped = makeTask("stopped", {
    status: "action_required",
    lastRun: { id: "r2", runId: "run2", sequence: 1, status: "stopped", createdAt: "2026-07-20T00:00:00Z", failureReason: "" },
  });
  const succeeded = makeTask("succeeded", {
    status: "action_required",
    lastRun: { id: "r3", runId: "run3", sequence: 3, status: "succeeded", createdAt: "2026-07-20T00:00:00Z", failureReason: "" },
  });
  const noLastRun = makeTask("noRun", { status: "action_required" });

  assert.match(taskQueueNote(failed), /第 2 次执行失败：Claude exited: exit status 1/);
  assert.match(taskQueueNote(stopped), /第 1 次执行已停止/);
  assert.match(taskQueueNote(succeeded), /第 3 次执行完成，要求修改/);
  assert.match(taskQueueNote(noLastRun), /要求修改，等待重新下发/);
});

test("shows awaiting review note", () => {
  const review = makeTask("review", { status: "awaiting_review" });
  assert.match(taskQueueNote(review), /执行完成，等待人工确认/);
  assert.equal(canRedispatch(review), true);
});

test("blocks redispatch while awaiting_review task is under orchestration", () => {
  const checking = makeTask("checking", { status: "awaiting_review", orchestrationStatus: "checking" });
  assert.equal(isTaskOrchestrating(checking), true);
  assert.equal(canRedispatch(checking), false);

  const awaitingMain = makeTask("merge", { status: "awaiting_review", orchestrationStatus: "awaiting_main", orchestrationTargetBranch: "release/2026.08" });
  assert.equal(isTaskAwaitingMainMerge(awaitingMain), true);
  assert.equal(canRedispatch(awaitingMain), false);

  // 暂停/待人工决策/清理中……任何编排状态都禁止手动重新下发，
  // 防止与编排收尾互相覆盖任务状态（后端同规则）。
  for (const orchestrationStatus of ["paused", "needs_human", "stopped", "removing"]) {
    const task = makeTask("orch", { status: "awaiting_review", orchestrationStatus });
    assert.equal(canRedispatch(task), false);
  }
});

test("blocks redispatch when awaiting_review task has unfinished predecessors", () => {
  const blocked = makeTask("blocked-review", {
    status: "awaiting_review",
    blockedBy: [{ taskId: "predecessor", title: "完成接口设计", status: "action_required" }],
  });
  assert.equal(canRedispatch(blocked), false);
});

test("marks verified orchestration branches as awaiting main merge", () => {
  for (const orchestrationStatus of ["awaiting_main", "integrated_to_dev"]) {
    const task = makeTask(orchestrationStatus, { status: "awaiting_review", orchestrationStatus });
    assert.equal(isTaskAwaitingMainMerge(task), true);
    assert.equal(taskDisplayStatus(task), "待合并 目标分支");
    assert.match(taskQueueNote(task), /可合并至 目标分支/);
  }
});

test("shows the orchestration snapshot target branch", () => {
  const task = makeTask("release", { orchestrationStatus: "awaiting_main", orchestrationTargetBranch: "release/2026.08" });
  assert.equal(taskDisplayStatus(task), "待合并 release/2026.08");
  assert.match(taskQueueNote(task), /可合并至 release\/2026\.08/);
});

test("makes an active orchestration checkout visible ahead of manual review", () => {
  const review = makeTask("review", { status: "awaiting_review", orchestrationStatus: "checking", orchestrationUpdatedAt: "2026-07-20T00:01:00Z" });

  assert.equal(taskDisplayStatus(review), "编排收尾中");
  assert.equal(taskDisplayStatusClass(review), "orchestration-checking");
  assert.equal(isTaskOrchestrating(review), true);
  assert.match(taskQueueNote(review), /正在提交实现，等待人工验证/);
});

test("shows orchestration cleanup as an active state", () => {
  const task = makeTask("cleanup", { status: "action_required", orchestrationStatus: "removing" });

  assert.equal(taskDisplayStatus(task), "自动编排清理中");
  assert.equal(taskDisplayStatusClass(task), "orchestration-removing");
  assert.equal(isTaskOrchestrating(task), true);
  assert.match(taskQueueNote(task), /正在停止执行并清理工作区/);
});

test("orders equal-priority tasks by position before creation time", () => {
  const first = makeTask("first", { position: 1, createdAt: "2026-07-01T00:00:00Z" });
  const second = makeTask("second", { position: 2, createdAt: "2026-07-21T00:00:00Z" });

  assert.deepEqual(sortQueueTasks([second, first]).map((task) => task.id), ["first", "second"]);
});

test("places pinned tasks ahead of status and priority ordering", () => {
  const running = makeTask("running", { status: "running", priority: "urgent", position: 1 });
  const pinned = makeTask("pinned", { status: "todo", priority: "low", position: 99, pinned: true });
  assert.deepEqual(sortQueueTasks([running, pinned]).map((task) => task.id), ["pinned", "running"]);
});

test("keeps blocked tasks out of dispatch actions and explains why", () => {
  const blocked = makeTask("blocked", {
    blockedBy: [{ taskId: "predecessor", title: "完成接口设计", status: "todo" }],
  });

  assert.equal(canOfferDispatch(blocked), false);
  assert.equal(canRedispatch({ ...blocked, status: "action_required" }), false);
  assert.equal(taskQueueNote(blocked), "等待：完成接口设计");
});

test("requires server eligibility in the queue confirmation flow", () => {
  const source = readFileSync(new URL("./TaskQueue.tsx", import.meta.url), "utf8");

  assert.match(source, /\/api\/tasks\/\$\{taskID\}/);
  assert.match(source, /detail\.canDispatch/);
  assert.match(source, /\/dispatch/);
});

test("offers re-dispatch and icon-only review on awaiting_review cards", () => {
  const source = readFileSync(new URL("./TaskQueue.tsx", import.meta.url), "utf8");

  // 待验收卡片四按钮 2×2：验收(1,1)、置顶(1,2)、重新下发(2,1)、删除(2,2)。
  // 重新下发按钮在待验收态落到下排左位（awaiting-review 修饰类由 CSS 定位）。
  assert.match(source, /task\.status === "awaiting_review" \? " awaiting-review" : ""/);
  // 验收按钮改为图标：不再渲染文字，保留 title/aria-label 无障碍提示。
  assert.match(source, /className="task-queue-review" draggable=\{false\} title="验收"/);
  assert.doesNotMatch(source, /task-queue-review secondary/);
});

test("exposes historical cancelled tasks as read-only items in the board", () => {
  const source = readFileSync(new URL("./TaskBoard.tsx", import.meta.url), "utf8");

  assert.match(source, /显示历史已取消/);
  assert.match(source, /label: "历史已取消"/);
  assert.match(source, /task\.status === "cancelled"\) return;/);
  assert.match(source, /task\.status === "running" \|\| task\.status === "done" \|\| task\.status === "cancelled"/);
  assert.match(source, /const acceptsDrops = definition\.id !== "cancelled"/);
  assert.match(source, /\{acceptsDrops && <div className=\{`task-drop-indicator/);
});

test("uses insertion gaps for downward board drags and suppresses drag clicks", () => {
  const source = readFileSync(new URL("./TaskBoard.tsx", import.meta.url), "utf8");
  const handleDropSource = source.slice(source.indexOf("const handleDrop"), source.indexOf("\n\n  return <section"));

  assert.match(source, /const targetIndex = e\.clientY < rect\.top \+ rect\.height \/ 2 \? index : index \+ 1;/);
  // 无操作拖拽的判据换成 isSameSlot（按服务端最新列表判断，不再用可见快照比下标）
  assert.match(source, /isSameSlot\(latest, task\.id, anchor, where\)/);
  assert.match(source, /const dragGestureActive = useRef\(false\);/);
  assert.match(source, /const dragClickSuppressedUntil = useRef\(0\);/);
  assert.match(source, /onClickCapture=\{handleBoardClickCapture\}/);
  assert.match(source, /dragGestureActive\.current = true;/);
  assert.match(source, /dragGestureActive\.current = false;/);
  assert.match(source, /dragAttempted\.current = true;/);
  assert.match(source, /if \(dragged\.current \|\| dragAttempted\.current \|\| Date\.now\(\) < suppressClickUntil\.current\)/);
  assert.match(source, /if \(dragged\.current \|\| dragAttempted\.current \|\| Date\.now\(\) < suppressClickUntil\.current\)[\s\S]*if \(e\.detail === 0\)/);
  assert.doesNotMatch(handleDropSource, /refresh\(task\.id\)/);
  assert.match(handleDropSource, /await refresh\(\);/);
});

test("uses WebSocket history once and caps client-side run logs", () => {
	const source = readFileSync(new URL("../run/ProjectRunPanel.tsx", import.meta.url), "utf8");

	assert.doesNotMatch(source, /setLogs\(s\.recentLogs\)/);
	assert.match(source, /mergeIncomingLogs\(s\.recentLogs\)/);
	assert.match(source, /ws\.onclose/);
	assert.match(source, /RECONNECT_MAX_DELAY/);
	assert.match(source, /LOG_BOTTOM_THRESHOLD/);
	assert.doesNotMatch(source, /scrollIntoView/);
	assert.match(source, /\.slice\(-MAX_LOG_ENTRIES\)/);
	assert.match(source, /request<RunStatusResponse>\(withWorkspace\(`\$\{basePath\}\/logs\/clear`\), \{ method: "POST" \}\)/);
	assert.match(source, /setStatus\(next\)/);
});

test("resets runner state only when changing workspaces", () => {
	const source = readFileSync(new URL("../run/ProjectRunPanel.tsx", import.meta.url), "utf8");

	assert.match(source, /const configDirtyRef = useRef\(false\);/);
	assert.match(source, /const configRevisionRef = useRef\(0\);/);
	assert.match(source, /workspaceGeneration === workspaceGenerationRef\.current && revision === configRevisionRef\.current && !configDirtyRef\.current/);
	assert.match(source, /if \(workspaceGeneration === workspaceGenerationRef\.current && revision === configRevisionRef\.current\)/);
	assert.match(source, /const updateConfig = \(next: RunConfig\) => \{\s*configDirtyRef\.current = true;\s*configRevisionRef\.current \+= 1;/s);
	assert.match(source, /const workspaceKey = `\$\{projectID\}:\$\{conversationId \|\| ""\}`;/);
	assert.match(source, /if \(renderedWorkspaceKeyRef\.current === workspaceKey\) return;/);
	assert.match(source, /renderedWorkspaceKeyRef\.current = workspaceKey;[\s\S]*clearedThroughLogIDRef\.current = 0;/);
});

test("keeps file tabs in sync with renamed and removed paths", () => {
  const source = readFileSync(new URL("../files/FilesPanel.tsx", import.meta.url), "utf8");

  assert.match(source, /function remapPath\(path: string, oldPath: string, newPath: string\)/);
  assert.match(source, /path\.startsWith\(`\$\{oldPath\}\/`\)/);
  assert.match(source, /stat: \{ \.\.\.file\.stat, path: nextPath, name \}/);
  assert.match(source, /function isPathAtOrBelow\(path: string, directory: string\)/);
  assert.match(source, /prev\.filter\(\(file\) => !isPathAtOrBelow\(file\.path, removedPath\)\)/);
});

test("loads orchestration configuration for single-task enqueue", () => {
  const source = readFileSync(new URL("./TaskBoard.tsx", import.meta.url), "utf8");

  assert.match(source, /request<OrchestrationConfig>\(`\/api\/projects\/\$\{projectID\}\/orchestration\/config`\)/);
  assert.match(source, /await Promise\.all\(\[refresh\(detail\.id\), loadOrchestration\(\)\]\)/);
  assert.match(source, /taskDisplayStatus\(task\)/);
});

test("keeps automatic orchestration as a dedicated workspace", () => {
  const source = readFileSync(new URL("./TaskBoard.tsx", import.meta.url), "utf8");

  assert.match(source, /navigate\(`\/projects\/\$\{projectID\}\/orchestration`\)/);
  assert.doesNotMatch(source, /编排设置/);
});

test("prevents duplicate task review submissions", () => {
  const source = readFileSync(new URL("./TaskBoard.tsx", import.meta.url), "utf8");

  assert.match(source, /const \[reviewSubmitting, setReviewSubmitting\] = useState\(false\);/);
  assert.match(source, /if \(reviewSubmitting\) return;/);
	assert.match(source, /disabled=\{reviewSubmitting\} onClick=\{\(\) => void submitReview\("request_changes"\)\}/);
	assert.doesNotMatch(source, /task-detail-close" title="关闭" aria-label="关闭" disabled=\{reviewBusy\}/);
	assert.match(source, /<h3>验证记录<\/h3>/);
	assert.match(source, /verificationPhaseLabel\(run\.phase\)/);
});

test("执行记录圆点的色调由纯函数判定，未建模状态不许画成空白", () => {
  assert.equal(taskRunStatusTone("succeeded"), "ok");
  assert.equal(taskRunStatusTone("failed"), "warn");
  assert.equal(taskRunStatusTone("stopped"), "warn");
  assert.equal(taskRunStatusTone("interrupted"), "warn");
  assert.equal(taskRunStatusTone("running"), "live");
  // queued 是"还没结果"，不是"出错了"，也不是"没问题"：给它中性档，而不是套绿点。
  assert.equal(taskRunStatusTone("queued"), "idle");
  // 未建模状态（后端 run.status 无 CHECK 约束）必须落在一个有底色的档上：
  // 圆点没颜色＝把"不知道"画成"没问题"，文案也必须回落到原值而不是空串。
  assert.equal(taskRunStatusTone("paused"), "idle");
  assert.equal(taskRunStatusLabel("paused"), "paused");
});

test("详情面板：一排操作条 + 图标工具按钮 + 状态单一来源", () => {
  const source = readFileSync(new URL("./TaskQueue.tsx", import.meta.url), "utf8");
  const styles = readFileSync(new URL("../../tasks.css", import.meta.url), "utf8");
  // 锚点必须是组件本身（`function InlineTaskDetail`）：早先按 `<div className="task-inline-detail">`
  // 切片会命中 loading 分支那一行（904 行），切片起点比预想的早、断言语义悄悄跑偏。
  const start = source.indexOf("function InlineTaskDetail");
  const detail = source.slice(start, source.indexOf("function formatDate", start));
  assert.match(detail, /className="task-inline-detail" data-status=\{detail\.status\}/, "切片必须落在 InlineTaskDetail 组件内");
  const actions = detail.slice(detail.indexOf("task-inline-detail-actions"));

  // 一排优先：`.task-inline-action-row` 是横向 flex，间距走 gap（不用 margin 拼），
  // 实测 340 栏宽 → 操作行内宽 234px，而「确认完成 + 要求修改 + 详情 + 删除」需要 209px。
  assert.match(styles, /\.task-inline-action-row \{ display: flex;[^}]*flex-wrap: wrap;[^}]*row-gap: 8px; \}/);
  // 放不下时必须**按组换行**：宁可换行也不许溢出（overflow 会把工具按钮画到卡片边框外）
  // 或横滚（把操作藏起来）。换行靠 .task-inline-action-tools 的 margin-left:auto 落成第二行右对齐，
  // 所以不再需要单独的撑开元素——两套机制会互相抢空间。
  assert.match(styles, /\.task-inline-action-tools \{ display: inline-flex; flex: none; gap: 5px; margin-left: auto; \}/);
  assert.doesNotMatch(detail, /task-inline-action-gap/);
  // 读数不进操作条：创建/执行时间只属于详情信息，混进操作行会让人以为"这行都在做操作"。
  assert.doesNotMatch(actions, /formatDate\(|创建|上次执行/);
  // 工具组恒有两个动作（完整详情 + 删除，不带条件渲染）：任何状态都不许出现空操作条。
  const toolsBlock = detail.slice(detail.indexOf('<span className="task-inline-action-tools">'), detail.indexOf("</div>", detail.indexOf('<span className="task-inline-action-tools">')));
  assert.match(toolsBlock, /title="完整详情" aria-label="完整详情"/);
  assert.match(toolsBlock, /title=\{deleteLabel\} aria-label=\{deleteLabel\}/);
  // 三个通用工具是图标按钮，但必须各自带可访问名（只有图标、没有名字＝读屏与悬浮都拿不到含义）。
  assert.match(detail, /className="task-inline-tool" title="编辑" aria-label="编辑"/);
  // 可访问名与视觉提示必须同一口径：删除中时 title/aria 一起改口，不许一个静态一个动态。
  assert.match(detail, /const deleteLabel = busy === "delete" \? "删除中" : "删除任务";/);
  // 三个工具共用同一个忙碌口径，避免"验收提交中还能点删除"这种在途请求与危险操作抢跑。
  assert.equal((detail.match(/disabled=\{toolBusy\}/g) || []).length, 3);
  // 按钮规格唯一：图标按钮与文字按钮同高同圆角，否则一行里会一大一小。
  // ⚠️ 必须同时钉住 min-height：`style.css:293` 给 `.primary / .secondary` 定了 min-height: 38px，
  // 而 min-height 永远压过 height —— 只写 height: 28px 时实测仍是 38px（第一版就是这么漏的，
  // 截图里只是"按钮略高"，肉眼看不出来；靠探针量出 38 才发现）。
  assert.match(styles, /\.task-inline-action \{ display: inline-flex; height: 28px; min-height: 28px;/);
  assert.match(styles, /\.task-inline-edit-actions \.primary, \.task-inline-edit-actions \.secondary \{ display: inline-flex; height: 28px; min-height: 28px;/);
  assert.match(styles, /\.task-inline-tool \{ display: grid; width: 28px; height: 28px;[^}]*border-radius: 6px;/);
  assert.match(styles, /\.task-inline-tool-icon \{ width: 14px; height: 14px;/);
  // 禁用态必须逐形态重申底色（同一份样式里 `.task-inline-action.primary` 排在全局
  // `.primary:disabled` 之后，只写 opacity 会让禁用的实底按钮仍然"看着能点"）。
  assert.match(styles, /\.task-inline-action\.primary:disabled \{ border-color: #c9d6ce; color: #809188; background: #dce7df; \}/);
  assert.match(styles, /\.task-inline-action\.secondary:disabled \{/);
  assert.match(styles, /\.task-inline-action\.warn:disabled \{/);
  assert.match(styles, /\.task-inline-tool:disabled \{ border-color: #dce7df;/);
  // 同一个面板里不许出现两种实底色（查看态 #2c7567 vs 全站柠檬绿）。
  assert.match(styles, /\.task-inline-edit-actions \.primary \{ border: 1px solid #2c7567; color: #f8fdf9; background: #2c7567; \}/);
  assert.match(styles, /\.task-inline-edit-actions \.primary:disabled \{ border-color: #c9d6ce;/);
  // 状态只有一个来源：根节点 data-status 直接取 detail.status，降灰态选择器再按它命中。
  assert.match(detail, /className="task-inline-detail" data-status=\{detail\.status\}/);
  assert.match(styles, /\.task-inline-detail\[data-status="cancelled"\] \{/);
  assert.match(styles, /\.task-inline-detail\[data-status="cancelled"\] \.task-inline-detail-title b \{/);
  // 状态胶囊与优先级同一行（原来是各占一行的上下堆叠，窄栏里白吃一行高度）。
  assert.match(detail, /className="task-inline-meta">/);
  assert.match(styles, /\.task-inline-meta \{ display: flex;[^}]*gap: 8px;/);
  // 面板里的状态文案/配色必须和队列行同源（taskDisplayStatus / taskDisplayStatusClass）：
  // 直接用 statusLabels[detail.status] 会让同一个任务在行里显示「自动执行中」、在详情里显示「执行中」；
  // `queued ? "队列中" : …` 也是多余的——taskDisplayStatus 自己就返回「队列中」（task-model.ts:48）。
  assert.match(detail, /taskDisplayStatusClass\(detail\)/);
  assert.match(detail, /taskDisplayStatus\(detail\)/);
  assert.doesNotMatch(detail, /statusLabels\[detail\.status\]/);
  assert.doesNotMatch(detail, /\{queued \? "队列中" : taskDisplayStatus\(detail\)\}/);
  // 「能不能就地验收」只有一处判据，且必须和队列行的验收入口同口径（含 isTaskOrchestrating）：
  // 否则会出现"行里不给验收、点进详情却能确认完成"，与编排收尾互相覆盖。
  assert.match(detail, /const canReviewInline = detail\.status === "awaiting_review" && !isTaskAwaitingMainMerge\(detail\) && !isTaskOrchestrating\(detail\);/);
  assert.equal((detail.match(/canReviewInline &&/g) || []).length, 2, "输入框与两个验收按钮共用同一个判据");
  assert.equal((detail.match(/detail\.status === "awaiting_review"/g) || []).length, 2, "只有「验收判据」与「暂不可验收的原因」两处按这个状态分支");
  // 暂不可验收时要说出原因，不能静默把按钮拿掉。
  assert.match(detail, /reviewUnavailable/);
  assert.match(styles, /\.task-inline-review-blocked \{/);
  // 「内容块要不要渲染」的判据只有一处，且在 JSX 里（交给 CSS :empty 会被 React 的空文本节点废掉）。
  assert.match(detail, /const hasBody = hasDescription \|\| detail\.runs\.length > 0 \|\| Boolean\(detail\.blockReason\);/);
  assert.match(detail, /\{hasBody && <div className="task-inline-detail-body">/);
  assert.doesNotMatch(styles, /\.task-inline-detail-body:empty/);
  // 头部分隔线必须是「头部之后」才出现：加载态/失败态复用同一个 .task-inline-detail-body 容器，
  // 写成无条件上边框会让那两态凭空多一条线（实测 2026-10-15 复查时抓到的）。
  assert.match(styles, /\.task-inline-detail-head \+ \.task-inline-detail-body \{ border-top: 1px solid #edf4ef; padding-top: 11px; \}/);
  assert.doesNotMatch(styles, /\.task-inline-detail-body \{ display: grid; gap: 10px; border-top/, "内容块的边距不许无条件写在基础规则里");
  // 面板内的竖直间距只有一个来源（根节点 grid gap）：表单再叠 margin-top 会变成双倍间距。
  assert.match(styles, /\.task-inline-edit-form \{ display: grid; gap: 9px; \}/);
  assert.doesNotMatch(styles, /\.task-inline-edit-form \{[^}]*margin-top/);
  // 禁用态一律降到项目已有的禁用灰：不许为"危险描边"另造暖褐色（越出项目色板）。
  assert.match(styles, /\.task-inline-action\.warn:disabled \{ border-color: #dce7df; color: #809188; background: #f2f5f3; \}/);
  // 执行记录的状态文案与色调都走纯函数：服务端枚举（succeeded / interrupted）不许直接上屏。
  assert.match(detail, /taskRunStatusLabel\(run\.status\)/);
  assert.match(detail, /taskRunStatusTone\(run\.status\)/);
  assert.doesNotMatch(detail, /<em>\{run\.status\}<\/em>/);
  // 同一份样式只允许写一遍：改动前有一段 `.task-inline-detail` 的后置覆盖，
  // 会让新卡片的底色/圆角/阴影被静默压回去（后写的赢）。这里扫**全部** css，
  // 并额外排除"和别的选择器写在同一份规则里"（旧覆盖正是 `.task-queue-inline-review, .task-inline-detail {`）。
  const cssFiles = collectCssFiles(fileURLToPath(new URL("../../", import.meta.url)));
  let cardRules = 0;
  for (const file of cssFiles) {
    const text = readFileSync(file, "utf8");
    cardRules += (text.match(/^\.task-inline-detail\s*\{/gm) || []).length;
    assert.doesNotMatch(text, /,\s*\.task-inline-detail\s*[,{]/, `${file} 里 .task-inline-detail 不许和别的选择器合并成一条规则`);
  }
  assert.equal(cardRules, 1, "详情面板的卡片样式只能有一处定义（全仓 css 范围内）");
});

test("defines a responsive task work rail", () => {
  const conversationStyles = readFileSync(new URL("../../conversation.css", import.meta.url), "utf8");
  const taskStyles = readFileSync(new URL("../../tasks.css", import.meta.url), "utf8");

  assert.match(conversationStyles, /grid-template-columns:\s*minmax\(260px, 300px\) minmax\(440px, 1fr\) minmax\(280px, 340px\)/);
  assert.match(conversationStyles, /\.quick-actions-row/);
  assert.match(taskStyles, /\.task-queue-mobile-toggle/);
});

test("未建模的状态值不能渲染成空白徽标", () => {
  // 后端 tasks.status 没有 CHECK 约束（task.go）：旧数据或新版本可能带来前端没建模的状态值。
  // statusLabels 查不到时若返回 undefined，徽标就是一个空胶囊 —— 等于把"不知道"画成空白。
  const legacy = makeTask("legacy", { status: "paused" as Task["status"] });
  assert.equal(taskDisplayStatus(legacy), "未知状态 paused");
  assert.equal(taskDisplayStatusClass(legacy), "paused");
});

test("batch management selects a single status group instead of only everything", () => {
  const source = readFileSync(new URL("./TaskBoard.tsx", import.meta.url), "utf8");
  const styles = readFileSync(new URL("../../tasks.css", import.meta.url), "utf8");

  // 「分类」= 一个状态组：看板列头与列表分组头都要挂上同一个 toggleGroup，
  // 只留一个"全选所有任务"的话，想清掉「已完成」就只能一列一列手工勾。
  assert.match(source, /const toggleGroup = \(ids: string\[\]\) => \{/);
  assert.match(source, /className="task-board-column-tools"/);
  assert.match(source, /onChange=\{\(\) => toggleGroup\(items\.map\(\(task\) => task\.id\)\)\}/);
  assert.match(source, /className="task-list-group-head"/);
  assert.match(source, /onChange=\{\(\) => toggleGroup\(ids\)\}/);
  // 已全选时按下去是"取消"，可访问名必须跟着改口，否则读屏用户听到的动作与结果相反。
  // 看板列头与列表分组头各一处（只数 aria-label，免得以后多挂个属性就要改这个数字）。
  assert.equal((source.match(/aria-label=\{`\$\{items\.length > 0 && picked === items\.length \? "取消选择" : "全选"\}/g) || []).length, 2, "看板列头与列表分组头的可访问名都要随状态改口");
  // 分组必须是"完备"的：后端 tasks.status 没有 CHECK 约束，前端 union 只是类型，
  // 真出现没建模的状态值时不许把这条任务从批量列表里吞掉（非批量能看到多少行，批量就得看到多少行）。
  assert.match(source, /if \(unknownItems\.length > 0\) groups\.push\(/);
  assert.match(source, /label: "其他状态"/);
  // 看板的列写死六个状态，未建模的状态值会让那条任务"哪一列都不属于" → 界面必须说出来。
  assert.match(source, /uncoveredStatusCount > 0 && <p className="task-board-note"/);
  // 顶部「全选」的勾选态一律按 every/some 算：选中项可能来自上一次搜索，
  // 用 `selectedIDs.size === visibleTasks.length` 判定会在两边数量碰巧相等时显示成全选。
  assert.match(source, /const visibleSelection = useMemo\(\(\) => \{/);
  assert.doesNotMatch(source, /selectedIDs\.size === visibleTasks\.length/);
  // 复选框用原生 input（自带 indeterminate/键盘语义），卡片里的视觉件与它共用同一套外观。
  assert.match(source, /className="task-check" type="checkbox"/);
  assert.match(styles, /\.task-check, \.task-batch-check \{/);
  assert.match(styles, /\.task-check:indeterminate, \.task-batch-check\.partial \{/);
  // 位置：复选框必须和状态徽标同一行。原来它独占一个网格行，批量模式下每张卡凭空高出约 26px。
  assert.match(source, /className="task-item-top">\{batchMode && <span/);
  // 选中态必须写在最后一条 `.task-item:hover` / `.task-list-row:hover` 之后：两者同权重，
  // 排在前面的话悬停底色会盖掉选中底色（浅绿列底上看起来就像"没选中"）。用 lastIndexOf 比对，
  // 因为悬停态在文件里出现两次，只要有一条排在选中态后面就会吃掉它。
  assert.ok(styles.indexOf(".task-item.batch-selected {") > styles.lastIndexOf(".task-item:hover {"), "卡片选中态要排在卡片悬停态之后");
  assert.ok(styles.indexOf(".task-list-row.batch-selected {") > styles.lastIndexOf(".task-list-row:hover {"), "列表选中态要排在列表悬停态之后");
  // 行的标题容器必须靠自己的类命中：批量模式多了一列勾选框，
  // 原来写 `> span:first-child` 会落到勾选列上，标题与描述被挤成一行。
  assert.match(source, /<span className="task-list-main">/);
  assert.doesNotMatch(styles, /\.task-list-row > span:first-child \{/);
  // 搜索框在批量模式下被整块换掉，筛选条件会在界面上"消失" —— 范围提示必须写明
  // "只作用于筛出的 N 项"，否则用户点列头会以为选的是整列（静默改小选择范围）。
  // 窄屏也只许藏那条发现性提示（data-scope="all"），范围提示必须留着。
  assert.match(source, /data-scope=\{query\.trim\(\) \? "filtered" : "all"\}/);
  assert.match(source, /筛选生效：只作用于搜索出的 \$\{visibleTasks\.length\} 项/);
  assert.match(styles, /\.task-batch-hint\[data-scope="filtered"\] \{ color: #8b6524; \}/);
  assert.match(styles, /\.task-batch-hint\[data-scope="all"\] \{ display: none; \}/);
  // 悬停规则必须**显式排除选中态**：`.task-check:hover:not(:disabled)` 的特异性是 (0,3,0)，
  // 天然盖过 (0,2,0) 的 `:checked`/`:indeterminate`（与书写顺序无关）—— 不排除的话，
  // 鼠标一停在已勾选的复选框上它就掉回浅底、白勾在近白底上看不见（实测 bg 掉回 #f2faf5）。
  assert.match(styles, /\.task-check:hover:not\(:disabled\):not\(:checked\):not\(:indeterminate\)/);
  assert.match(styles, /\.task-batch-check:hover:not\(\.checked\):not\(\.partial\)/);
  // 窄屏的行布局必须排在**无断点那条列定义之后**：同断点同权重、后来者赢，写在前面就是死规则
  // （实测后果：768px 下每行仍按 5 列算、行最小宽度 804px、整页横向溢出）。
  const wideRowColumns = styles.indexOf(".task-list-head, .task-list-row { grid-template-columns: minmax(280px, 2fr)");
  const narrowRowColumns = styles.indexOf(".task-list-head, .task-list-row { grid-template-columns: minmax(0, 1fr) auto; gap: 9px; }");
  assert.ok(wideRowColumns > 0 && narrowRowColumns > wideRowColumns, "窄屏行布局要写在无断点的五列定义之后，否则等于没生效");
});
