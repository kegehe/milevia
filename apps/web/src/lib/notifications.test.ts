import { test } from "node:test";
import assert from "node:assert/strict";
import { isWithinQuietHours, notificationConversationURL, notificationTargetURL, priorityForType, type NotificationEvent } from "./notifications";

test("failed and error notifications use normal priority", () => {
  assert.equal(priorityForType("task.done"), "normal");
  assert.equal(priorityForType("run.failed"), "normal");
  assert.equal(priorityForType("agent.error"), "normal");
});

function makeEvent(partial: Partial<NotificationEvent>): NotificationEvent {
  return {
    id: "n1",
    type: "task.done",
    projectId: "proj-1",
    projectName: "项目一",
    title: "t",
    body: "b",
    priority: "normal",
    conversationId: "conv-1",
    actionUrl: "/projects/proj-1/conversations/conv-1",
    createdAt: "2026-08-18T00:00:00Z",
    ...partial,
  };
}

test("notificationTargetURL 优先使用后端 actionUrl（run 页）", () => {
  const event = makeEvent({ type: "run.failed", actionUrl: "/projects/proj-1/run" });
  assert.equal(notificationTargetURL(event), "/projects/proj-1/run");
});

test("notificationTargetURL 无 actionUrl 时回落对话页", () => {
  const event = makeEvent({ actionUrl: "" });
  assert.equal(notificationTargetURL(event), "/projects/proj-1/conversations/conv-1");
});

test("notificationConversationURL 无 conversationId 时进入项目对话入口", () => {
  assert.equal(notificationConversationURL({ projectId: "proj-1" }), "/projects/proj-1/conversations");
});

test("免打扰时段支持日间与跨午夜区间", () => {
  assert.equal(isWithinQuietHours(new Date(2026, 7, 19, 9, 0), "09:00", "17:00"), true);
  assert.equal(isWithinQuietHours(new Date(2026, 7, 19, 17, 0), "09:00", "17:00"), false);
  assert.equal(isWithinQuietHours(new Date(2026, 7, 19, 22, 0), "22:00", "08:00"), true);
  assert.equal(isWithinQuietHours(new Date(2026, 7, 20, 7, 59), "22:00", "08:00"), true);
  assert.equal(isWithinQuietHours(new Date(2026, 7, 20, 8, 0), "22:00", "08:00"), false);
});

test("无效或相同的免打扰时段不会静默所有通知", () => {
  const now = new Date(2026, 7, 19, 12, 0);
  assert.equal(isWithinQuietHours(now, "09:00", "09:00"), false);
  assert.equal(isWithinQuietHours(now, "25:00", "08:00"), false);
});
