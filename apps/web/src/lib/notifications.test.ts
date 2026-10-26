import { test } from "node:test";
import assert from "node:assert/strict";
import { formatDurationMs, isWithinQuietHours, isWindowsNotifyType, notificationConversationURL, notificationTargetURL, priorityForType, unreadIdsMissingFromServer, webNotificationsSupported, windowsToastLines, type NotificationEvent } from "./notifications";

test("failed and error notifications use normal priority", () => {
  assert.equal(priorityForType("task.done"), "normal");
  assert.equal(priorityForType("run.failed"), "normal");
  assert.equal(priorityForType("agent.error"), "normal");
});

function makeEvent(partial: Partial<NotificationEvent>): NotificationEvent {
  return {
    id: "n1",
    type: "task.done",
    projectId: "proj-1",
    projectName: "项目一",
    title: "t",
    body: "b",
    priority: "normal",
    conversationId: "conv-1",
    actionUrl: "/projects/proj-1/conversations/conv-1",
    createdAt: "2026-08-18T00:00:00Z",
    ...partial,
  };
}

test("notificationTargetURL 优先使用后端 actionUrl（run 页）", () => {
  const event = makeEvent({ type: "run.failed", actionUrl: "/projects/proj-1/run" });
  assert.equal(notificationTargetURL(event), "/projects/proj-1/run");
});

test("notificationTargetURL 无 actionUrl 时回落对话页", () => {
  const event = makeEvent({ actionUrl: "" });
  assert.equal(notificationTargetURL(event), "/projects/proj-1/conversations/conv-1");
});

test("notificationConversationURL 无 conversationId 时进入项目对话入口", () => {
  assert.equal(notificationConversationURL({ projectId: "proj-1" }), "/projects/proj-1/conversations");
});

test("免打扰时段支持日间与跨午夜区间", () => {
  assert.equal(isWithinQuietHours(new Date(2026, 7, 19, 9, 0), "09:00", "17:00"), true);
  assert.equal(isWithinQuietHours(new Date(2026, 7, 19, 17, 0), "09:00", "17:00"), false);
  assert.equal(isWithinQuietHours(new Date(2026, 7, 19, 22, 0), "22:00", "08:00"), true);
  assert.equal(isWithinQuietHours(new Date(2026, 7, 20, 7, 59), "22:00", "08:00"), true);
  assert.equal(isWithinQuietHours(new Date(2026, 7, 20, 8, 0), "22:00", "08:00"), false);
});

test("Windows 弹窗通知仅限任务完成类状态", () => {
  // 完成语义 → 应触发
  assert.equal(isWindowsNotifyType("task.done"), true);
  assert.equal(isWindowsNotifyType("task.awaiting_review"), true);
  // 需要处理/审批等 → 不应触发（弹窗标题取事件 title，是完成语义；含义不符的不弹）
  assert.equal(isWindowsNotifyType("task.action_required"), false);
  assert.equal(isWindowsNotifyType("orchestration.needs_human"), false);
  assert.equal(isWindowsNotifyType("approval.pending"), false);
  assert.equal(isWindowsNotifyType("run.completed"), false);
});

test("formatDurationMs 按秒/分/小时分层", () => {
  assert.equal(formatDurationMs(42_000), "42 秒");
  assert.equal(formatDurationMs(59_000), "59 秒");
  assert.equal(formatDurationMs(61_000), "1 分 1 秒");
  assert.equal(formatDurationMs(222_000), "3 分 42 秒");
  assert.equal(formatDurationMs(300_000), "5 分钟");
  assert.equal(formatDurationMs(4_980_000), "1 小时 23 分钟");
  assert.equal(formatDurationMs(7_200_000), "2 小时");
});

test("windowsToastLines 组装方案A三行：结果 / 项目+任务 / 耗时", () => {
  const lines = windowsToastLines(makeEvent({ title: "任务已完成", taskTitle: "修复移动端导航错位", durationMs: 222_000 }));
  assert.deepEqual(lines, { title: "任务已完成", line1: "项目「项目一」· 修复移动端导航错位", line2: "3 分 42 秒" });
});

test("windowsToastLines 字段缺失时省略对应行，不编造", () => {
  // 无耗时（durationMs 缺失或 0）→ 不显示耗时行
  assert.equal(windowsToastLines(makeEvent({ taskTitle: "X", durationMs: 0 })).line2, "");
  assert.equal(windowsToastLines(makeEvent({ taskTitle: "X" })).line2, "");
  // 无项目名 → 主体退化为任务名
  assert.deepEqual(windowsToastLines(makeEvent({ projectName: "", taskTitle: "修复移动端导航错位", durationMs: 1_000 })), { title: "t", line1: "修复移动端导航错位", line2: "1 秒" });
  // 旧通知两字段都没有 → 只剩标题（等价旧行为的最短弹窗）
  assert.deepEqual(windowsToastLines(makeEvent({ projectName: "" })), { title: "t", line1: "", line2: "" });
});

test("无效或相同的免打扰时段不会静默所有通知", () => {
  const now = new Date(2026, 7, 19, 12, 0);
  assert.equal(isWithinQuietHours(now, "09:00", "09:00"), false);
  assert.equal(isWithinQuietHours(now, "25:00", "08:00"), false);
});

test("Web Notification 只在浏览器可用：桌面端与原生包一律判不支持", () => {
  // 桌面端：WebView2 的 PermissionRequested / NotificationReceived 都无人处理，
  // 权限拿不到 granted 且 denied 会被写进应用自己的 profile —— 必须判"不支持"而不是"被拒绝"。
  assert.equal(webNotificationsSupported({ hasNotificationAPI: true, isDesktop: true, isNativePlatform: false }), false);
  // 原生包：Android WebView 不接系统通知栏。它自己的系统通知走另一条通道
  //（@capacitor/local-notifications 的本地通知，见 lib/mobile-notify.ts），
  // 那条通道不影响这里的结论 —— 本函数问的就是"Web Notification 这条 API 能不能用"。
  assert.equal(webNotificationsSupported({ hasNotificationAPI: true, isDesktop: false, isNativePlatform: true }), false);
  // 浏览器：非安全上下文里这条 API 压根不存在。
  assert.equal(webNotificationsSupported({ hasNotificationAPI: false, isDesktop: false, isNativePlatform: false }), false);
  assert.equal(webNotificationsSupported({ hasNotificationAPI: true, isDesktop: false, isNativePlatform: false }), true);
});

test("未读角标只清理服务端已删除的通知；拉取期间新到的不在快照里", () => {
  const beforeFetch = new Set<string>(["kept", "deleted"]);
  const serverIds = new Set<string>(["kept"]);
  assert.deepEqual(unreadIdsMissingFromServer(beforeFetch, serverIds), ["deleted"]);
});
