import assert from "node:assert/strict";
import test from "node:test";

import { dropAfterCutoff, hasCutoff } from "./conversation-cutoff";

type Item = { id: string; createdAt: string };

function item(id: string, at: string): Item {
  return { id, createdAt: at };
}

const T0 = "2026-10-20T00:00:00Z";
const cutoff = "2026-10-20T00:10:00.000000123Z"; // 服务端给的是 RFC3339Nano
const rewoundAt = "2026-10-20T00:30:00Z";

// 没有节点（没回退过）时什么都不藏：本地缓存里的一切都还在服务端的可见范围内。
test("没有 cutoff 时原样返回", () => {
  const items = [item("a", T0), item("b", "2026-10-20T00:20:00Z")];
  assert.deepEqual(dropAfterCutoff(items, undefined).map((entry) => entry.id), ["a", "b"]);
  assert.deepEqual(dropAfterCutoff(items, "").map((entry) => entry.id), ["a", "b"]);
  assert.deepEqual(dropAfterCutoff(items, "not-a-date").map((entry) => entry.id), ["a", "b"]);
});

// 核心判据：节点之前留着；节点之后、回退之前的丢掉；回退之后新发生的留着。
test("只丢「节点之后、回退之前」的内容", () => {
  const items = [
    item("kept", "2026-10-20T00:05:00Z"),
    item("at-node", cutoff),
    item("discarded", "2026-10-20T00:20:00Z"),
    item("retry-after-rewind", "2026-10-20T00:31:00Z"),
  ];
  assert.deepEqual(dropAfterCutoff(items, cutoff, rewoundAt).map((entry) => entry.id), ["kept", "at-node", "retry-after-rewind"]);
});

// 没有回退时刻（老服务端）时退回"比节点新就丢"：宁可多藏，也不能把丢掉的历史留在屏上。
test("没有回退时刻时按节点时间硬过滤", () => {
  const items = [item("kept", "2026-10-20T00:05:00Z"), item("newer", "2026-10-20T00:20:00Z")];
  assert.deepEqual(dropAfterCutoff(items, cutoff).map((entry) => entry.id), ["kept"]);
});

// 时间戳坏掉的条目一律保留：拿坏数据去猜，会把用户的东西藏没。
test("时间戳无效的条目保留", () => {
  const items = [item("broken", "nope"), item("newer", "2026-10-20T00:20:00Z")];
  assert.deepEqual(dropAfterCutoff(items, cutoff, rewoundAt).map((entry) => entry.id), ["broken"]);
});

test("hasCutoff 认得出有效时间", () => {
  assert.equal(hasCutoff(cutoff), true);
  assert.equal(hasCutoff(undefined), false);
  assert.equal(hasCutoff(""), false);
  assert.equal(hasCutoff("nope"), false);
});
