import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

import { canOfferDispatch, canRedispatch, filterQueueTasks, isTaskOrchestrating, sortQueueTasks, taskDisplayStatus, taskDisplayStatusClass, taskQueueNote, type Task } from "./task-model.ts";

const makeTask = (id: string, overrides: Partial<Task> = {}): Task => ({
  id,
  title: id,
  description: "Task description",
  acceptanceCriteria: "Task acceptance criteria",
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

test("orders tasks by status priority in queue", () => {
  const ready = makeTask("ready", { position: 2 });
  const running = makeTask("running", { status: "running" });
  const review = makeTask("review", { status: "awaiting_review" });
  const repair = makeTask("repair", { status: "action_required" });
  const done = makeTask("done", { status: "done" });
  const cancelled = makeTask("cancelled", { status: "cancelled" });

  const visible = filterQueueTasks([ready, running, review, repair, done, cancelled], "active");

  assert.deepEqual(sortQueueTasks(visible).map((task) => task.id), ["running", "ready", "repair", "review"]);
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
  assert.equal(canRedispatch(review), false);
});

test("makes an active independent review visible ahead of manual review", () => {
  const review = makeTask("review", { status: "awaiting_review", orchestrationStatus: "checking", orchestrationUpdatedAt: "2026-07-20T00:01:00Z" });

  assert.equal(taskDisplayStatus(review), "独立审查中");
  assert.equal(taskDisplayStatusClass(review), "orchestration-checking");
  assert.equal(isTaskOrchestrating(review), true);
  assert.match(taskQueueNote(review), /独立审查代理正在检查/);
});

test("orders equal-priority tasks by position before creation time", () => {
  const first = makeTask("first", { position: 1, createdAt: "2026-07-01T00:00:00Z" });
  const second = makeTask("second", { position: 2, createdAt: "2026-07-21T00:00:00Z" });

  assert.deepEqual(sortQueueTasks([second, first]).map((task) => task.id), ["first", "second"]);
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

test("exposes historical cancelled tasks as read-only items in the board", () => {
  const source = readFileSync(new URL("./TaskBoard.tsx", import.meta.url), "utf8");

  assert.match(source, /显示历史已取消/);
  assert.match(source, /label: "历史已取消"/);
  assert.match(source, /task\.status === "cancelled"\) return;/);
  assert.match(source, /task\.status === "running" \|\| task\.status === "done" \|\| task\.status === "cancelled"/);
  assert.match(source, /const acceptsDrops = definition\.id !== "cancelled"/);
  assert.match(source, /\{acceptsDrops && <div className=\{`task-drop-indicator/);
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
});

test("refreshes active orchestration jobs and renders their review status", () => {
  const source = readFileSync(new URL("./TaskBoard.tsx", import.meta.url), "utf8");

  assert.match(source, /\["queued", "preparing", "implementing", "checking"\]/);
  assert.match(source, /loadOrchestration\(\)\.catch/);
  assert.match(source, /taskDisplayStatus\(task\)/);
});

test("prevents duplicate task review submissions", () => {
  const source = readFileSync(new URL("./TaskBoard.tsx", import.meta.url), "utf8");

  assert.match(source, /const \[reviewSubmitting, setReviewSubmitting\] = useState\(false\);/);
  assert.match(source, /if \(reviewSubmitting\) return;/);
  assert.match(source, /disabled=\{reviewSubmitting\} onClick=\{\(\) => void submitReview\("request_changes"\)\}/);
});

test("defines a responsive task work rail", () => {
  const conversationStyles = readFileSync(new URL("../../conversation.css", import.meta.url), "utf8");
  const taskStyles = readFileSync(new URL("../../tasks.css", import.meta.url), "utf8");

  assert.match(conversationStyles, /grid-template-columns:\s*300px minmax\(0, 1fr\)/);
  assert.match(conversationStyles, /\.quick-actions-row/);
  assert.match(taskStyles, /\.task-queue-mobile-toggle/);
});
