import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

// 全站确认框统一用自绘的 ConfirmDialog（components/ConfirmDialog），不再用原生 window.confirm。
// 同一个应用里两种确认样式并存很突兀，原生弹窗也没有标题/图标/忙碌态，桌面外壳里尤其明显。
// 这一份守住"切换分支、移除工作区、回退对话、定时任务完全控制"这四个曾经的漏网处。
const conversationPage = await readFile(new URL("./pages/ConversationPage.tsx", import.meta.url), "utf8");
const scheduledTasksPage = await readFile(new URL("./pages/ScheduledTasksPage.tsx", import.meta.url), "utf8");

test("桌面端不再调用原生 window.confirm", () => {
  assert.doesNotMatch(conversationPage, /window\.confirm\(/);
  assert.doesNotMatch(scheduledTasksPage, /window\.confirm\(/);
});

test("切换分支、移除工作区、回退对话都走 ConfirmDialog（不再是原生弹窗）", () => {
  assert.match(conversationPage, /<ConfirmDialog title="切换分支"/);
  assert.match(conversationPage, /title: "移除工作区",/);
  assert.match(conversationPage, /title: rewindConfirmTitle\(request\),/);
  // 点入口只打开确认层，真正执行留到 onConfirm。
  assert.match(conversationPage, /setPendingSwitch\(branch\);/);
  assert.match(conversationPage, /void archiveWorkspaceConfirmed\(targetConversationID, workspace\)\.finally/);
  assert.match(conversationPage, /void rewindToCheckpointConfirmed\(targetConversationID, checkpoint, request\)\.finally/);
});

test("定时任务「完全控制」的确认也走 ConfirmDialog", () => {
  assert.match(scheduledTasksPage, /<ConfirmDialog title="使用完全控制"/);
  assert.match(scheduledTasksPage, /setPendingFullControl\(true\);/);
  assert.match(scheduledTasksPage, /onConfirm=\{\(\) => \{ setPendingFullControl\(false\); save\(true\); \}\}/);
});

test("确认层共用一格但清理有归属：旧的慢操作 finally 不能清掉新会话的确认框", () => {
  // 每个打开者领令牌，finally 只清自己那一格；否则 A 会话的回退/移除在 B 会话上收尾时
  // 会把 B 刚打开的确认框（以及它的忙碌态）一起清掉。
  assert.match(conversationPage, /const pendingConfirmTokenRef = useRef\(0\);/);
  assert.match(conversationPage, /const closePendingConfirm = \(token: number\) => \{\s*if \(pendingConfirmTokenRef\.current !== token\) return;/);
  assert.match(conversationPage, /\.finally\(\(\) => closePendingConfirm\(token\)\)/);
  // 所有打开点都必须走 openPendingConfirm（旧的"直接 setPendingConfirm"写法会绕过令牌）。
  assert.doesNotMatch(conversationPage, /pendingConfirmBusyRef\.current = false;\s*setPendingConfirmBusy\(false\);\s*setPendingConfirm\(\{/);
  // 切会话收确认层时必须走一遍 onCancel：强制停止那条靠它复位 stopping。
  assert.match(conversationPage, /if \(orphaned\) orphaned\.onCancel\(\);/);
});
