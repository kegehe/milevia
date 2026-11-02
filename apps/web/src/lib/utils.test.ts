import assert from "node:assert/strict";
import test from "node:test";
import { formatCost, workspacePathForDisplay } from "./utils";

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

// WSL 项目卡片上已有企鹅图标，路径只保留发行版内部的部分，避免 \\wsl$\Ubuntu 挤占位置。
test("workspacePathForDisplay 剥掉 WSL UNC 前缀", () => {
  assert.equal(workspacePathForDisplay("\\\\wsl$\\Ubuntu\\home\\root\\projects"), "/home/root/projects");
  assert.equal(workspacePathForDisplay("\\\\wsl.localhost\\Ubuntu\\home\\dev"), "/home/dev");
  assert.equal(workspacePathForDisplay("\\\\WSL$\\Ubuntu\\srv\\app"), "/srv/app");
});

// 非 WSL 路径原样返回：Windows 盘符、Linux 绝对路径都不能被误改。
test("workspacePathForDisplay 保留非 WSL 路径", () => {
  for (const path of ["D:\\projects\\milevia", "/home/root/projects", "\\\\server\\share\\dir"]) {
    assert.equal(workspacePathForDisplay(path), path);
  }
});
