import assert from "node:assert/strict";
import test from "node:test";
import { mergeSnapshotStreamingMessages } from "./snapshot-streaming-merge";

type Message = { id: string; content: string; role: string; createdAt: string };
type Snapshot = { projects: Array<{ conversations: Array<{ id: string; messages: Message[] }> }> };

const placeholder = (id: string, chunk: { content: string; createdAt: string }): Message => ({
  id: "stream-" + id,
  content: chunk.content,
  role: "assistant",
  createdAt: chunk.createdAt,
});

test("流式消息在快照尚未持久化时补入对应会话", () => {
  const snapshot: Snapshot = { projects: [{ conversations: [{ id: "c1", messages: [] }] }] };
  const streaming = new Map([["m1", { conversationId: "c1", content: "正在输出", createdAt: "2026-01-01" }]]);
  const merged = mergeSnapshotStreamingMessages(snapshot, streaming, placeholder);
  assert.deepEqual(merged.projects[0].conversations[0].messages, [placeholder("m1", streaming.get("m1")!)]);
});

test("流式缓冲用最新文本覆盖快照中的占位消息", () => {
  const snapshot: Snapshot = { projects: [{ conversations: [{ id: "c1", messages: [placeholder("m1", { content: "旧文本", createdAt: "2026-01-01" })] }] }] };
  const streaming = new Map([["m1", { conversationId: "c1", content: "新文本", createdAt: "2026-01-01" }]]);
  const merged = mergeSnapshotStreamingMessages(snapshot, streaming, placeholder);
  assert.equal(merged.projects[0].conversations[0].messages[0].content, "新文本");
});
