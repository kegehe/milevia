import assert from "node:assert/strict";
import test from "node:test";
import { mergeSnapshotNotices } from "./snapshot-notice-merge";

type Notice = { id: string; merge?: "append" | "replace" };
type Snapshot = { projects: Array<{ conversations: Array<{ id: string; notices?: Notice[] }> }> };

const parse = (value: unknown): Notice[] => Array.isArray(value) ? value as Notice[] : [];
const combine = (server: Notice[], local: Notice[]) => [...server, ...local.filter((notice) => !server.some((item) => item.id === notice.id))];

test("快照确认普通通知后消费本地缓冲", () => {
  const pending = new Map([["c1", [{ id: "n1" }]]]);
  const snapshot: Snapshot = { projects: [{ conversations: [{ id: "c1", notices: [{ id: "n1" }] }] }] };
  const merged = mergeSnapshotNotices(snapshot, pending, parse, combine);
  assert.equal(pending.has("c1"), false);
  assert.deepEqual(merged.projects[0].conversations[0].notices, [{ id: "n1" }]);
});

test("replace 通知在快照带回旧记录时继续保留本地终态", () => {
  const pending = new Map([["c1", [{ id: "n1", merge: "replace" }]]]);
  const snapshot: Snapshot = { projects: [{ conversations: [{ id: "c1", notices: [{ id: "n1" }] }] }] };
  const merged = mergeSnapshotNotices(snapshot, pending, parse, combine);
  assert.deepEqual(pending.get("c1"), [{ id: "n1", merge: "replace" }]);
  assert.deepEqual(merged.projects[0].conversations[0].notices, [{ id: "n1" }]);
});
