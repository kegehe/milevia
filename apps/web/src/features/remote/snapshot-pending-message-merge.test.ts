import assert from "node:assert/strict";
import test from "node:test";
import { hasOtherPendingMessages, mergeSnapshotPendingMessages, pendingMessageMatches, type PendingMessage } from "./snapshot-pending-message-merge";

type Message = { id: string; role: string; content: string; createdAt: string; clientRequestId?: string; attachments?: { path: string }[] };
type Snapshot = { projects: Array<{ conversations: Array<{ id: string; messages: Message[] }> }> };
const optimistic = (message: { requestId: string; content: string; createdAt: string }): Message => ({ id: "pending-" + message.requestId, role: "user", content: message.content, createdAt: message.createdAt });

test("快照只确认已持久化数量内的相同待发消息", () => {
  const pending = new Map([["c1", new Map([["a", { requestId: "a", content: "同一句", createdAt: "1" }], ["b", { requestId: "b", content: "同一句", createdAt: "2" }]])]]);
  const snapshot: Snapshot = { projects: [{ conversations: [{ id: "c1", messages: [{ id: "server", role: "user", content: "同一句", createdAt: "0" }] }] }] };
  const merged = mergeSnapshotPendingMessages(snapshot, pending, optimistic);
  assert.equal(pending.get("c1")?.size, 1);
  assert.deepEqual(merged.projects[0].conversations[0].messages.map((message) => message.id), ["server", "pending-b"]);
});

test("相同正文附带不同文件时，快照只确认路径一致的消息", () => {
  const first: PendingMessage = { requestId: "a", content: "", createdAt: "1", attachments: [{ path: "a.png" }] };
  const second: PendingMessage = { ...first, requestId: "b", attachments: [{ path: "b.png" }] };
  const pending = new Map([["c1", new Map([["a", first], ["b", second]])]]);
  const snapshot: Snapshot = { projects: [{ conversations: [{ id: "c1", messages: [{ id: "server", role: "user", content: "", createdAt: "0", attachments: first.attachments }] }] }] };
  const merged = mergeSnapshotPendingMessages<Snapshot, Message>(snapshot, pending, (message) => ({ ...optimistic(message), attachments: message.attachments }));
  assert.equal(pending.get("c1")?.size, 1);
  assert.ok(pending.get("c1")?.has("b"));
  assert.deepEqual(merged.projects[0].conversations[0].messages[1].attachments, [{ path: "b.png" }]);
});

test("重复发送相同附件时按请求 ID 确认，不把旧历史当成本次回执", () => {
  const first: PendingMessage = { requestId: "new-a", content: "", createdAt: "1", attachments: [{ path: "a.png" }] };
  const second = { ...first, requestId: "new-b" };
  const pending = new Map([["c1", new Map([[first.requestId, first], [second.requestId, second]])]]);
  const snapshot: Snapshot = { projects: [{ conversations: [{ id: "c1", messages: [
    { id: "old", clientRequestId: "old-request", role: "user", content: "", createdAt: "0", attachments: first.attachments },
    { id: "server-b", clientRequestId: "new-b", role: "user", content: "", createdAt: "2", attachments: first.attachments },
  ] }] }] };
  const merged = mergeSnapshotPendingMessages(snapshot, pending, optimistic);
  assert.deepEqual([...pending.get("c1")!.keys()], ["new-a"]);
  assert.deepEqual(merged.projects[0].conversations[0].messages.map(message => message.id), ["old", "server-b", "pending-new-a"]);
});

test("快照截断正文仍可按请求 ID 确认；明确没有请求 ID 的其他来源消息不确认待发消息", () => {
  const message: PendingMessage = { requestId: "request", content: "long original", createdAt: "1" };
  assert.equal(pendingMessageMatches(message, { content: "truncated", clientRequestId: "request" }), true);
  assert.equal(pendingMessageMatches(message, { content: message.content, clientRequestId: "" }), false);
  assert.equal(pendingMessageMatches(message, { content: message.content, clientRequestId: "other" }), false);
  // 缺少字段的旧服务端仍沿用内容匹配。
  assert.equal(pendingMessageMatches(message, { content: message.content }), true);
  const pending = new Map([["c1", new Map([[message.requestId, message]])]]);
  const snapshot: Snapshot = { projects: [{ conversations: [{ id: "c1", messages: [{ id: "server", clientRequestId: message.requestId, role: "user", content: "truncated", createdAt: "2" }] }] }] };
  mergeSnapshotPendingMessages(snapshot, pending, optimistic);
  assert.equal(pending.size, 0);
});

test("失败消息自身不算其他待发请求，最后一条失败消息能够回填正文和提示", () => {
  const first: PendingMessage = { requestId: "a", content: "draft", createdAt: "1" };
  const pending = new Map([[first.requestId, first]]);
  assert.equal(hasOtherPendingMessages(pending, first.requestId), false);
  pending.set("b", { ...first, requestId: "b" });
  assert.equal(hasOtherPendingMessages(pending, first.requestId), true);
  assert.equal(hasOtherPendingMessages(undefined, first.requestId), false);
});
