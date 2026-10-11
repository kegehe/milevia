import assert from "node:assert/strict";
import test from "node:test";
import { applyMobileTerminalEvent, terminalConversationStatus } from "./mobile-realtime";

function snapshot() {
  return {
    projects: [{
      id: "p1",
      tasks: [{ id: "t1", status: "running", updatedAt: "old" }],
      conversations: [{ id: "c1", status: "running", lastActivityAt: "old", messages: [
        { id: "user-1", role: "user", content: "开始", createdAt: "old" },
        { id: "stream-a", role: "assistant", content: "未完成", createdAt: "old" },
      ] }],
    }],
  };
}

test("maps run terminal events to conversation status", () => {
  assert.equal(terminalConversationStatus("run.stopped"), "stopped");
  assert.equal(terminalConversationStatus("run.failed"), "failed");
  assert.equal(terminalConversationStatus("run.completed"), "completed");
});

test("stopped event updates the conversation and removes stream placeholders", () => {
  const result = applyMobileTerminalEvent(snapshot(), {
    type: "run.stopped",
    createdAt: "2026-10-10T00:00:02Z",
    payload: { conversationId: "c1", taskId: "t1" },
  });
  const conversation = result.snapshot.projects[0].conversations[0];
  assert.equal(result.changed, true);
  assert.equal(conversation.status, "stopped");
  assert.equal(conversation.lastActivityAt, "2026-10-10T00:00:02Z");
  assert.deepEqual(conversation.messages.map((message) => message.id), ["user-1"]);
  assert.equal(result.snapshot.projects[0].tasks[0].status, "action_required");
});

test("terminal event application is idempotent", () => {
  const event = { type: "run.stopped", createdAt: "2026-10-10T00:00:02Z", payload: { conversationId: "c1" } };
  const first = applyMobileTerminalEvent(snapshot(), event);
  const second = applyMobileTerminalEvent(first.snapshot, event);
  assert.equal(second.changed, false);
  assert.strictEqual(second.snapshot, first.snapshot);
});

test("task status events update the matching task only", () => {
  const result = applyMobileTerminalEvent(snapshot(), {
    type: "task.updated",
    taskId: "t1",
    createdAt: "2026-10-10T00:00:03Z",
    payload: { status: "stopped" },
  });
  assert.equal(result.snapshot.projects[0].tasks[0].status, "stopped");
  assert.equal(result.snapshot.projects[0].tasks[0].updatedAt, "2026-10-10T00:00:03Z");
  assert.equal(result.snapshot.projects[0].conversations[0].status, "running");
});

test("run status is not copied into the task status field", () => {
  const result = applyMobileTerminalEvent(snapshot(), {
    type: "run.failed",
    taskId: "t1",
    createdAt: "2026-10-10T00:00:04Z",
    payload: { conversationId: "c1", status: "failed" },
  });
  assert.equal(result.snapshot.projects[0].tasks[0].status, "action_required");
});

test("missing event timestamps remain idempotent", () => {
  const first = applyMobileTerminalEvent(snapshot(), { type: "run.stopped", payload: { conversationId: "c1" } });
  const second = applyMobileTerminalEvent(first.snapshot, { type: "run.stopped", payload: { conversationId: "c1" } });
  assert.equal(first.snapshot.projects[0].conversations[0].lastActivityAt, "old");
  assert.equal(second.changed, false);
  assert.strictEqual(second.snapshot, first.snapshot);
});

test("a late run terminal event does not regress a completed task", () => {
  const current = snapshot();
  current.projects[0].tasks[0].status = "done";
  const result = applyMobileTerminalEvent(current, {
    type: "run.failed",
    taskId: "t1",
    payload: { conversationId: "c1", status: "failed" },
  });
  assert.equal(result.snapshot.projects[0].tasks[0].status, "done");
  assert.equal(result.snapshot.projects[0].conversations[0].status, "failed");
});
