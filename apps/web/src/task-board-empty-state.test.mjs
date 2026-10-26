import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

// 「空看板」不能把筛选造成的空说成「还没有任务」回归。
//
// 搜索词没命中、或所有任务都是已取消且没开「显示历史已取消」时，看板原本一律渲染
// 「还没有任务」+「新建任务」。用户会以为任务被误删，而不是被当前筛选挡住了。
// 这里固化三种真相（真的空 / 搜索没命中 / 全被已取消折叠）的判据、文案与下一步动作。

const [taskBoard, tasksCss] = await Promise.all([
  readFile(new URL("./features/tasks/TaskBoard.tsx", import.meta.url), "utf8"),
  readFile(new URL("./tasks.css", import.meta.url), "utf8"),
]);

test("看板空态按原因分开渲染，不用『还没有任务』一概而论", () => {
  // 判据必须是队列总条数，不是当前可见条数：后者为 0 恰恰是这三种情况共同的表现。
  assert.match(taskBoard, /const hiddenCancelledCount = Math\.max\(0, tasks\.length - activeTasks\.length\);/);
  assert.match(taskBoard, /const emptyState = visibleTasks\.length > 0 \? null\s*: tasks\.length === 0 \? \{ state: "empty" as const, title: "还没有任务"/);

  // 搜索没命中：标题要带上搜索词，动作是「清除搜索」，不是「新建任务」。
  assert.match(taskBoard, /state: "filtered" as const,\s*title: `没有匹配「\$\{trimmedQuery\}」的任务`/);
  assert.match(taskBoard, /emptyState\?\.state === "filtered" \? <button className="primary" onClick=\{\(\) => setQuery\(""\)\}>清除搜索<\/button>/);

  // 全被「已取消」折叠：要说清条数，动作是打开开关。
  assert.match(taskBoard, /state: "hidden" as const, title: "任务都被隐藏了"/);
  assert.match(taskBoard, /全部 \$\{tasks\.length\} 个任务都是已取消状态/);
  assert.match(taskBoard, /emptyState\?\.state === "hidden" \? <button className="primary" onClick=\{\(\) => setShowHistoricalCancelled\(true\)\}>显示历史已取消<\/button>/);

  // 「新建任务」只能出现在真正空项目的那一支。
  const emptyBlock = taskBoard.slice(taskBoard.indexOf('className="task-empty"'), taskBoard.indexOf('emptyState?.state === "hidden"'));
  assert.doesNotMatch(emptyBlock, /新建任务/);
  assert.match(taskBoard, /: <button className="primary" onClick=\{\(\) => setEditor\(\{\}\)\}>新建任务<\/button>/);

  // 空态整体替换列表内容，两种视图共用；结果要能被读屏播报。
  assert.match(taskBoard, /<div className="task-empty" data-state=\{emptyState\?\.state \?\? "empty"\} role="status">/);
});

test("空态图标跟着原因切换，筛选态不再顶着加号", () => {
  assert.match(tasksCss, /\.task-empty\[data-state="filtered"\] h3::before \{ content: "\\2205"; \}/);
  assert.match(tasksCss, /\.task-empty\[data-state="hidden"\] h3::before \{ content: "\\2026"; \}/);
});
