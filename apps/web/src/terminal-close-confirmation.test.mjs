import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

// 终端关闭确认回归。
//
// 问题：点标签上的 × 或中键点标签会直接 DELETE 会话，服务端随即结束终端进程。
// 误点一下就会中断正在跑的构建/部署。现在两个入口都必须走 requestCloseSession：
// 未结束的会话先弹确认，已 exited / failed 的会话才允许无确认直接关闭。

const source = await readFile(new URL("./pages/TerminalPage.tsx", import.meta.url), "utf8");

test("关闭按钮与中键都先经过确认入口", () => {
  assert.match(source, /onClick=\{\(\) => requestCloseSession\(item\.id\)\}/, "× 按钮应调用 requestCloseSession");
  assert.match(source, /event\.button === 1[\s\S]{0,120}requestCloseSession\(item\.id\)/, "中键点击应调用 requestCloseSession");
  // 两个入口都不能再绕过确认直接关闭。
  assert.doesNotMatch(source, /void closeSession\(item\.id\)/, "标签入口不应直接调用 closeSession");
});

test("只有已结束的会话才无确认直接关闭", () => {
  assert.match(source, /current === "exited" \|\| current === "failed"/);
  // 活动会话要读 ws 实时状态，否则后台列表里的陈旧状态会放过正在运行的会话。
  assert.match(source, /targetID === activeIDRef\.current \? statusRef\.current/);
});

test("确认弹窗执行关闭并带处理中状态", () => {
  assert.match(source, /<ConfirmDialog/);
  assert.match(source, /confirmLabel="关闭终端"/);
  assert.match(source, /onConfirm=\{\(\) => confirmCloseSession\(pendingCloseID\)\}/);
  assert.match(source, /busy=\{closing\}/);
});

test("确认弹窗的图标有显式尺寸", async () => {
  // ConfirmDialog 只给 .confirm-dialog-icon 一个裸 span，图标尺寸由每个弹窗的
  // 作用域样式决定；漏了类名或样式，SVG 会按 300×150 撑坏弹窗。
  const css = await readFile(new URL("./terminal.css", import.meta.url), "utf8");
  assert.match(source, /className="terminal-close-dialog"/);
  assert.match(css, /\.terminal-close-dialog \.confirm-dialog-icon svg\s*\{[^}]*width:\s*18px/);
});
