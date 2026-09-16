import assert from "node:assert/strict";
import test from "node:test";
import { formatCost } from "./utils";

// 0 / 缺失 / NaN 都要走占位符：这些情况在数值上无法和「确实没花钱」区分，
// 打印 $0.0000 会被读成「统计完了，结果是 0 元」。
test("formatCost 对无费用数据返回占位符", () => {
  assert.equal(formatCost(0), "--");
  assert.equal(formatCost(Number.NaN), "--");
  assert.equal(formatCost(Number.POSITIVE_INFINITY), "--");
  assert.equal(formatCost(-1), "--");
});

test("formatCost 对正数保留四位小数", () => {
  assert.equal(formatCost(0.0001), "$0.0001");
  assert.equal(formatCost(1.23456), "$1.2346");
});
