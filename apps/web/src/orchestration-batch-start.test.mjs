import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const pageSource = await readFile(new URL("./pages/OrchestrationPage.tsx", import.meta.url), "utf8");
const taskModelSource = await readFile(new URL("./features/tasks/task-model.ts", import.meta.url), "utf8");
const cssSource = await readFile(new URL("./orchestration.css", import.meta.url), "utf8");

// 用户要的闸门：新建编排任务后把子任务加进队列**不会自己跑**，必须点「开始执行」。
// 后端靠计划的 started_at 拦住调度器，前端负责把这件事说清楚并给出入口。
test("未开始的计划在右栏给出「开始执行」入口并调用 start 接口", () => {
  // 只在未开始时渲染：已开始的计划再加子任务本来就会跟着跑，不该再出现这个按钮。
  assert.match(pageSource, /\{!batch\.started && <button type="button" className="primary"/);
  assert.match(pageSource, /aria-label="开始执行"/);
  assert.match(pageSource, /`\/api\/projects\/\$\{projectId\}\/orchestration\/batches\/\$\{batch\.id\}\/start`, \{ method: "POST", body: "\{\}" \}/);
  assert.match(pageSource, /const startBatch = async \(batch: OrchestrationBatch\) => \{/);
  // 接线：面板的 onStart 与调用处都要接上，否则按钮点了没反应。
  assert.match(pageSource, /onClick=\{onStart\}/);
  assert.match(pageSource, /onStart=\{\(\) => void startBatch\(panelBatch\)\}/);
});

// 未开始的计划里，子任务状态全是 queued；如果「未开始」分支排在这些派生分支后面，
// 界面会把它说成「子任务推进中」——正是这道闸门要消除的误解。
test("计划文案里「未开始」优先于其它派生状态", () => {
  const fn = pageSource.indexOf("function orchestrationPlanStatusLabel(batch: OrchestrationBatch) {");
  assert.ok(fn > -1, "找不到 orchestrationPlanStatusLabel");
  const body = pageSource.slice(fn, pageSource.indexOf("\n}", fn));
  const notStarted = body.indexOf('if (!batch.started) return "未开始";');
  const emptyCount = body.indexOf('if (batch.taskCount === 0) return "暂无子任务";');
  const running = body.indexOf('return "子任务推进中";');
  assert.ok(notStarted > -1, "缺少「未开始」分支");
  assert.ok(notStarted < emptyCount && notStarted < running, "「未开始」必须排在前面");
});

// 未开始的计划会让子任务永久停在 queued，若照旧按 queued 触发 5 秒轮询，一个放着不动的
// 草稿会让页面一直空转刷新。
test("未开始计划的排队子任务不触发轮询刷新", () => {
  assert.match(pageSource, /const needsRefresh = \(job: OrchestrationJob\) => refreshingStatuses\.has\(job\.status\) && \(job\.status !== "queued" \|\| !job\.batchId \|\| startedBatchIDs\.has\(job\.batchId\)\);/);
  assert.match(pageSource, /const startedBatchIDs = new Set\(batches\.filter\(\(batch\) => batch\.started\)\.map\(\(batch\) => batch\.id\)\);/);
  // 轮询条件必须用 needsRefresh，不能退回裸的 refreshingStatuses.has。
  assert.doesNotMatch(pageSource, /if \(!jobs\.some\(\(job\) => refreshingStatuses\.has\(job\.status\)\)\) return;/);
});

// 开始成功后立刻本地落定，否则刷新失败时按钮会一直停在「开始执行」上，用户会反复点。
test("开始成功后本地立刻标记为已开始", () => {
  assert.match(pageSource, /previous\.map\(\(item\) => \(item\.id === batch\.id \? \{ \.\.\.item, started: true, status: "active" \} : item\)\)/);
});

// 任务看板（移动端复用同一份文案）不能再把「等计划开始」说成「已进入自动编排队列」。
test("任务看板区分「等计划开始」", () => {
  assert.match(taskModelSource, /if \(task\.orchestrationPending\) return "等待计划开始";/);
  assert.match(taskModelSource, /if \(task\.orchestrationPending\) return "已加入编排任务，等计划开始后才执行";/);
  assert.match(taskModelSource, /orchestrationPending\?: boolean;/);
});

// ①栏的状态点：未知 status 会落到默认灰，显式配色才能和 paused/stopped 的灰区分开。
test("「未开始」状态点有独立配色", () => {
  assert.match(cssSource, /\.orchestration-status-dot\.not_started \{ background: #a9b6b0; \}/);
});
