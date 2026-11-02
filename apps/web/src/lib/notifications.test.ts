import { test } from "node:test";
import assert from "node:assert/strict";
import { approvalIDFromNotification, formatDurationMs, isWithinQuietHours, isWindowsNotifyType, MAX_UNREAD_NOTIFICATIONS, notificationConversationURL, notificationTargetURL, notificationTimeNs, priorityForType, reconcileUnreadNotifications, rememberNotificationID, trimUnreadNotifications, unreadIdsMissingFromServer, webNotificationsSupported, windowsToastActions, windowsToastLines, windowsToastSticky, type NotificationEvent } from "./notifications";

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

test("窗口不在前台时，完成类和需要马上处理的通知都弹，状态流水不弹", () => {
  assert.equal(isWindowsNotifyType("task.done"), true);
  assert.equal(isWindowsNotifyType("task.awaiting_review"), true);
  assert.equal(isWindowsNotifyType("task.action_required"), true);
  assert.equal(isWindowsNotifyType("orchestration.needs_human"), true);
  assert.equal(isWindowsNotifyType("approval.pending"), true);
  assert.equal(isWindowsNotifyType("ask.pending"), true);
  assert.equal(isWindowsNotifyType("run.completed"), false);
  assert.equal(isWindowsNotifyType("orchestration.retry"), false);
  assert.equal(isWindowsNotifyType("orchestration.paused"), false);
});

test("只有命令审批能在弹窗上允许或拒绝，并且留在屏幕上", () => {
  assert.deepEqual(windowsToastActions(makeEvent({ type: "approval.pending", id: "ap-1" })), [
    { label: "拒绝", action: "deny" },
    { label: "允许", action: "allow" },
  ]);
  assert.equal(approvalIDFromNotification(makeEvent({ type: "approval.pending", id: "ap-1", approvalId: "ap-live" })), "ap-live");
  assert.equal(approvalIDFromNotification(makeEvent({ type: "approval.pending", id: "ap-1" })), "ap-1");
  assert.deepEqual(windowsToastActions(makeEvent({ type: "ask.pending", id: "ask-1" })), []);
  assert.deepEqual(windowsToastActions(makeEvent({ type: "task.action_required" })), []);
  assert.equal(windowsToastSticky("approval.pending"), true);
  assert.equal(windowsToastSticky("orchestration.needs_human"), true);
  assert.equal(windowsToastSticky("ask.pending"), true);
  assert.equal(windowsToastSticky("task.action_required"), true);
  assert.equal(windowsToastSticky("task.done"), false);
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

test("需要处理的弹窗把正文放进第三行，完成类没有耗时时不拿正文填", () => {
  assert.equal(windowsToastLines(makeEvent({ type: "approval.pending", title: "等待确认命令", body: "pnpm test", taskTitle: "", durationMs: 0 })).line2, "pnpm test");
  assert.equal(windowsToastLines(makeEvent({ type: "task.done", body: "项目「项目一」的任务已完成", taskTitle: "X", durationMs: 0 })).line2, "");
  const long = "命".repeat(200);
  assert.equal([...windowsToastLines(makeEvent({ type: "ask.pending", body: long, durationMs: 0 })).line2].length, 140);
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

test("实时通知集合只保留最新 1000 条，延迟的历史补发不会挤掉新通知", () => {
  const unread = new Map<string, bigint>();
  for (let index = 0; index < MAX_UNREAD_NOTIFICATIONS; index++) {
    unread.set(`new-${index}`, BigInt(index + 1));
  }
  unread.set("delayed-history", 0n);
  trimUnreadNotifications(unread);
  assert.equal(unread.size, MAX_UNREAD_NOTIFICATIONS);
  assert.equal(unread.has("delayed-history"), false);
  unread.set("latest", BigInt(MAX_UNREAD_NOTIFICATIONS + 1));
  trimUnreadNotifications(unread);
  assert.equal(unread.has("new-0"), false);
  assert.equal(unread.has("latest"), true);
});

test("未读集合在时间相同时按 ID 倒序裁剪，与服务端顺序一致", () => {
  const unread = new Map<string, bigint>();
  for (let index = 0; index <= MAX_UNREAD_NOTIFICATIONS; index++) {
    unread.set(String(index).padStart(4, "0"), 1n);
  }
  trimUnreadNotifications(unread);
  assert.equal(unread.size, MAX_UNREAD_NOTIFICATIONS);
  assert.equal(unread.has("0000"), false);
  assert.equal(unread.has("1000"), true);
});

test("满容量时先移除已删除的新通知，再保留服务端补回的有效旧通知", () => {
  const unread = new Map<string, bigint>();
  const serverUnread = new Map<string, bigint>();
  for (let index = 0; index < MAX_UNREAD_NOTIFICATIONS; index++) {
    unread.set(`deleted-${index}`, BigInt(2000 + index));
    serverUnread.set(`kept-${index}`, BigInt(index));
  }
  reconcileUnreadNotifications(unread, new Set(unread.keys()), serverUnread);
  assert.deepEqual(unread, serverUnread);
});

test("整批对账保留拉取期间实时到达的新通知，合并后只裁掉最旧的记录", () => {
  const unread = new Map<string, bigint>([["deleted", 0n]]);
  const beforeFetch = new Set(unread.keys());
  unread.set("live-during-fetch", 2000n);
  const serverUnread = new Map<string, bigint>();
  for (let index = 0; index < MAX_UNREAD_NOTIFICATIONS; index++) serverUnread.set(`kept-${index}`, BigInt(index));
  reconcileUnreadNotifications(unread, beforeFetch, serverUnread);
  assert.equal(unread.size, MAX_UNREAD_NOTIFICATIONS);
  assert.equal(unread.has("live-during-fetch"), true);
  assert.equal(unread.has("deleted"), false);
  assert.equal(unread.has("kept-0"), false);
});

test("通知时间比较保留服务端纳秒精度，并兼容无小数和时区偏移", () => {
  const base = notificationTimeNs("2026-10-26T12:00:00.000Z")!;
  assert.equal(notificationTimeNs("2026-10-26T12:00:00Z"), base);
  assert.equal(notificationTimeNs("2026-10-26T20:00:00+08:00"), base);
  assert.equal(notificationTimeNs("2026-10-26T12:00:00.123456789Z"), base + 123_456_789n);
  assert.equal(notificationTimeNs("2026-10-26T20:00:00.123456789+08:00"), base + 123_456_789n);
  assert.equal(notificationTimeNs("2026-10-26T12:00:00.1Z"), base + 100_000_000n);
  assert.equal(notificationTimeNs("invalid"), null);
});

test("同一毫秒内按纳秒排序，只有时间完全相同时才比较 ID", () => {
  const unread = new Map<string, bigint>();
  for (let index = 0; index < MAX_UNREAD_NOTIFICATIONS - 1; index++) unread.set(`recent-${index}`, 2_000_000n);
  unread.set("zzz-older", notificationTimeNs("1970-01-01T00:00:00.001000100Z")!);
  unread.set("aaa-newer", notificationTimeNs("1970-01-01T00:00:00.001000200Z")!);
  trimUnreadNotifications(unread);
  assert.equal(unread.has("zzz-older"), false);
  assert.equal(unread.has("aaa-newer"), true);
});

test("去重集合超限只逐个淘汰最旧记录，重见的通知继续保留", () => {
  const ids = new Set<string>();
  for (let index = 0; index < 2 * MAX_UNREAD_NOTIFICATIONS; index++) rememberNotificationID(ids, `id-${index}`);
  rememberNotificationID(ids, "id-0");
  rememberNotificationID(ids, "latest");
  assert.equal(ids.size, 2 * MAX_UNREAD_NOTIFICATIONS);
  assert.equal(ids.has("id-0"), true);
  assert.equal(ids.has("id-1"), false);
  assert.equal(ids.has("id-1999"), true);
  assert.equal(ids.has("latest"), true);
});
