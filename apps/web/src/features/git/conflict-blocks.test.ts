import assert from "node:assert/strict";
import test from "node:test";

import { countConflictMarkers, parseConflictBlocks, resolveConflictBlock } from "./conflict-blocks.ts";

const diff3Text = [
  "line0",
  "<<<<<<< ours",
  "line2 main-side",
  "||||||| base",
  "line2 base",
  "=======",
  "line2 feature-side",
  ">>>>>>> theirs",
  "line3",
].join("\n");

test("解析带 base 的 diff3 标记", () => {
  const blocks = parseConflictBlocks(diff3Text);
  assert.equal(blocks.length, 1);
  assert.equal(blocks[0].ours, "line2 main-side");
  assert.equal(blocks[0].theirs, "line2 feature-side");
  assert.equal(blocks[0].base, "line2 base");
  assert.equal(blocks[0].startLine, 1);
  assert.equal(blocks[0].endLine, 7);
});

test("解析无 base 的双向标记", () => {
  const text = ["<<<<<<< HEAD", "a", "=======", "b", ">>>>>>> feature"].join("\n");
  const blocks = parseConflictBlocks(text);
  assert.equal(blocks.length, 1);
  assert.equal(blocks[0].base, null);
  assert.equal(blocks[0].ours, "a");
  assert.equal(blocks[0].theirs, "b");
});

test("解析多个冲突块并保留上下文", () => {
  const text = ["x", "<<<<<<< HEAD", "1", "=======", "2", ">>>>>>> f", "y", "<<<<<<< HEAD", "3", "=======", "4", ">>>>>>> f", "z"].join("\n");
  const blocks = parseConflictBlocks(text);
  assert.equal(blocks.length, 2);
  assert.equal(blocks[1].ours, "3");
});

test("采用 ours 只替换目标块", () => {
  const text = ["ctx", "<<<<<<< ours", "a", "=======", "b", ">>>>>>> theirs", "tail"].join("\n");
  const resolved = resolveConflictBlock(text, 0, "ours");
  assert.equal(resolved, ["ctx", "a", "tail"].join("\n"));
});

test("采用 theirs 替换为传入内容", () => {
  const resolved = resolveConflictBlock(diff3Text, 0, "theirs");
  assert.equal(resolved, ["line0", "line2 feature-side", "line3"].join("\n"));
});

test("both 顺序保留 ours 后接 theirs", () => {
  const text = ["<<<<<<< ours", "a1", "a2", "=======", "b1", ">>>>>>> theirs"].join("\n");
  const resolved = resolveConflictBlock(text, 0, "both");
  assert.equal(resolved, ["a1", "a2", "b1"].join("\n"));
});

test("空侧内容时采用该侧会删除整个块", () => {
  const text = ["before", "<<<<<<< ours", "=======", "b", ">>>>>>> theirs", "after"].join("\n");
  assert.equal(resolveConflictBlock(text, 0, "ours"), ["before", "after"].join("\n"));
});

test("越界索引返回原文本", () => {
  assert.equal(resolveConflictBlock(diff3Text, 5, "ours"), diff3Text);
});

test("连续解决多个块后无残留标记", () => {
  let text = ["a", "<<<<<<< ours", "1", "=======", "2", ">>>>>>> theirs", "b", "<<<<<<< ours", "3", "=======", "4", ">>>>>>> theirs", "c"].join("\n");
  assert.equal(countConflictMarkers(text), 2);
  text = resolveConflictBlock(text, 0, "theirs");
  assert.equal(countConflictMarkers(text), 1);
  text = resolveConflictBlock(text, 0, "ours");
  assert.equal(countConflictMarkers(text), 0);
});

test("统计未解决块", () => {
  assert.equal(countConflictMarkers(diff3Text), 1);
  assert.equal(countConflictMarkers("no markers here"), 0);
});
