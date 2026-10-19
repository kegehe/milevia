import assert from "node:assert/strict";
import test from "node:test";
import { mergeSnapshotRealtimeMessages } from "./snapshot-realtime-merge";

type Message = { id: string; content: string };
type Snapshot = { projects: Array<{ conversations: Array<{ id: string; messages: Message[] }> }> };

test("快照合并保留尚未持久化的实时消息", () => {
  const buffer = new Map([["live", { conversationId: "conversation", message: { id: "live", content: "实时消息" }, revision: 4 }]]);
  const snapshot: Snapshot = { projects: [{ conversations: [{ id: "conversation", messages: [] }] }] };
  const merged = mergeSnapshotRealtimeMessages(snapshot, buffer, 4);
  assert.deepEqual(merged.projects[0].conversations[0].messages, [{ id: "live", content: "实时消息" }]);
  assert.equal(buffer.has("live"), true);
});

test("快照包含同等或更完整内容后才消费实时缓冲", () => {
  const buffer = new Map([["live", { conversationId: "conversation", message: { id: "live", content: "完整消息" }, revision: 4 }]]);
  const snapshot: Snapshot = { projects: [{ conversations: [{ id: "conversation", messages: [{ id: "live", content: "完整消息" }] }] }] };
  mergeSnapshotRealtimeMessages(snapshot, buffer, 4);
  assert.equal(buffer.has("live"), false);
});
