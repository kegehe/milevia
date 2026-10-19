import assert from "node:assert/strict";
import test from "node:test";
import { mergeSnapshotPendingMessages } from "./snapshot-pending-message-merge";

type Message = { id: string; role: string; content: string; createdAt: string };
type Snapshot = { projects: Array<{ conversations: Array<{ id: string; messages: Message[] }> }> };
const optimistic = (message: { requestId: string; content: string; createdAt: string }): Message => ({ id: "pending-" + message.requestId, role: "user", content: message.content, createdAt: message.createdAt });

test("快照只确认已持久化数量内的相同待发消息", () => {
  const pending = new Map([["c1", new Map([["a", { requestId: "a", content: "同一句", createdAt: "1" }], ["b", { requestId: "b", content: "同一句", createdAt: "2" }]])]]);
  const snapshot: Snapshot = { projects: [{ conversations: [{ id: "c1", messages: [{ id: "server", role: "user", content: "同一句", createdAt: "0" }] }] }] };
  const merged = mergeSnapshotPendingMessages(snapshot, pending, optimistic);
  assert.equal(pending.get("c1")?.size, 1);
  assert.deepEqual(merged.projects[0].conversations[0].messages.map((message) => message.id), ["server", "pending-b"]);
});
