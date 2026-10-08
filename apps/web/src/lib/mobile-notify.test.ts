import { test } from "node:test";
import assert from "node:assert/strict";
import {
  MOBILE_NOTIFICATION_CHANNEL,
  isInterruptingMobileEvent,
  mobileNotificationContent,
  notificationBackend,
  notificationIDFromEventID,
  pickNotificationProject,
  shouldNotifyWhileAway,
  toNotificationPermission,
} from "./mobile-notify";

test("原生包走本地通知通道，浏览器走 Web Notification", () => {
  assert.equal(notificationBackend({ isNativePlatform: true, hasNotificationAPI: false }), "native");
  // 原生包里 window.Notification 恒为 undefined（Android WebView 不接系统通知栏），
  // 但通道仍然是 native —— 这正是当初把原生判成 "unsupported" 的那个误会。
  assert.equal(notificationBackend({ isNativePlatform: true, hasNotificationAPI: true }), "native");
  assert.equal(notificationBackend({ isNativePlatform: false, hasNotificationAPI: true }), "web");
  assert.equal(notificationBackend({ isNativePlatform: false, hasNotificationAPI: false }), "none");
});

test("插件的 prompt 两态映射成 default，才能让既有的开启通知入口照常渲染", () => {
  assert.equal(toNotificationPermission("granted"), "granted");
  assert.equal(toNotificationPermission("denied"), "denied");
  assert.equal(toNotificationPermission("prompt"), "default");
  assert.equal(toNotificationPermission("prompt-with-rationale"), "default");
  // 已经是页面口径的值原样透传
  assert.equal(toNotificationPermission("default"), "default");
  // 读不出可识别取值时收起入口，而不是留一颗点了没反应的按钮
  assert.equal(toNotificationPermission(undefined), "unsupported");
  assert.equal(toNotificationPermission("something-else"), "unsupported");
});

test("只有任务/运行/审批类事件打断用户，会话消息不打扰", () => {
  assert.equal(isInterruptingMobileEvent("task.done"), true);
  assert.equal(isInterruptingMobileEvent("task.awaiting_review"), true);
  assert.equal(isInterruptingMobileEvent("run.completed"), true);
  assert.equal(isInterruptingMobileEvent("approval.pending"), true);
  // 会话消息回到 App 就能看到
  assert.equal(isInterruptingMobileEvent("assistant.delta"), false);
  assert.equal(isInterruptingMobileEvent("assistant.message"), false);
  assert.equal(isInterruptingMobileEvent("conversation.created"), false);
  assert.equal(isInterruptingMobileEvent("taskforce.done"), false);
  assert.equal(isInterruptingMobileEvent(undefined), false);
  assert.equal(isInterruptingMobileEvent(42), false);
});

test("通知文案沿用 summary → 状态 → 事件类型的三级回退", () => {
  assert.deepEqual(
    mobileNotificationContent({ type: "task.done", taskId: "t1", payload: { summary: "任务已完成", projectId: "p1", conversationId: "c1" } }),
    { title: "Milevia", body: "任务已完成", projectId: "p1", conversationId: "c1", taskId: "t1" },
  );
  assert.equal(
    mobileNotificationContent({ type: "run.failed", payload: { status: "failed" } })?.body,
    "状态：failed",
  );
  // 两个字段都没有时退回事件类型，不能是空字符串（空 body 的通知在通知栏里是一行空标题）
  assert.equal(mobileNotificationContent({ type: "task.action_required", payload: {} })?.body, "task.action_required");
  assert.equal(mobileNotificationContent({ type: "task.done" })?.body, "task.done");
  // 不值得打扰的事件直接不给内容
  assert.equal(mobileNotificationContent({ type: "assistant.delta", payload: { delta: "x" } }), null);
  // 跳转目标缺失时给空串、不给 undefined，调用方判空即可
  assert.deepEqual(
    mobileNotificationContent({ type: "task.done", payload: { summary: "s" } }),
    { title: "Milevia", body: "s", projectId: "", conversationId: "", taskId: "" },
  );
  // payload 不是对象时不能抛
  assert.equal(mobileNotificationContent({ type: "task.done", payload: "not-an-object" })?.body, "task.done");
  // taskId 有两个来源，缺一不可：task.* 事件只在**信封**上有（payload 里没有项目/会话字段），
  // run.* 事件反过来 —— 信封为空，是控制端主动塞进 payload 的。
  assert.equal(mobileNotificationContent({ type: "task.created", taskId: "t9", payload: { status: "todo" } })?.taskId, "t9");
  assert.equal(mobileNotificationContent({ type: "run.completed", payload: { status: "completed", taskId: "t7" } })?.taskId, "t7");
  // 两个都有时信封优先
  assert.equal(mobileNotificationContent({ type: "run.completed", taskId: "t1", payload: { taskId: "t2" } })?.taskId, "t1");
  // 信封是空串时同样要往 payload 退（不能把空串当成"有值"）
  assert.equal(mobileNotificationContent({ type: "run.completed", taskId: "", payload: { taskId: "t2" } })?.taskId, "t2");
  assert.equal(mobileNotificationContent({ type: "task.created", taskId: 42, payload: {} })?.taskId, "");
  assert.equal(mobileNotificationContent({ type: "task.created", payload: { taskId: 42 } })?.taskId, "");
});

test("跳转目标优先 projectId，缺失时按 taskId 反查项目", () => {
  const projects = [
    { id: "p1", tasks: [{ id: "t1" }] },
    { id: "p2", tasks: [{ id: "t2" }, { id: "t3" }] },
  ];
  assert.equal(pickNotificationProject(projects, { projectId: "p2", taskId: "" })?.id, "p2");
  // projectId 优先：即使 taskId 指向另一个项目也以 projectId 为准
  assert.equal(pickNotificationProject(projects, { projectId: "p1", taskId: "t3" })?.id, "p1");
  // task.* 事件只有 taskId —— 这正是必须反查的那条路
  assert.equal(pickNotificationProject(projects, { projectId: "", taskId: "t3" })?.id, "p2");
  // 找不到就返回 undefined，让调用方挂起，而不是随便跳一个项目
  assert.equal(pickNotificationProject(projects, { projectId: "missing", taskId: "" }), undefined);
  assert.equal(pickNotificationProject(projects, { projectId: "", taskId: "missing" }), undefined);
  assert.equal(pickNotificationProject(projects, { projectId: "", taskId: "" }), undefined);
  assert.equal(pickNotificationProject([], { projectId: "", taskId: "t1" }), undefined);
});

test("通知 id 稳定、非零、且落在 int32 非负区间内", () => {
  assert.equal(notificationIDFromEventID("evt-1"), notificationIDFromEventID("evt-1"));
  assert.notEqual(notificationIDFromEventID("evt-1"), notificationIDFromEventID("evt-2"));
  // 空串也要给非零 id：Android 上 0 是保留值
  const empty = notificationIDFromEventID("");
  assert.equal(empty, 1);
  for (const value of ["", "evt-1", "0".repeat(200), "\u0000\u0001", "task.done-12345"]) {
    const id = notificationIDFromEventID(value);
    assert.ok(Number.isInteger(id), `${value} → ${id} 不是整数`);
    assert.ok(id > 0 && id <= 0x7fffffff, `${value} → ${id} 超出 int32 非负区间`);
  }
});

test("前后台判定：两条信号取或，原生多一条 appStateChange", () => {
  // 浏览器只有 document.hidden 这一条
  assert.equal(shouldNotifyWhileAway({ isNative: false, nativeInBackground: true, documentHidden: false }), false);
  assert.equal(shouldNotifyWhileAway({ isNative: false, nativeInBackground: false, documentHidden: true }), true);
  // 原生：任一条说"用户不在看"就提醒
  assert.equal(shouldNotifyWhileAway({ isNative: true, nativeInBackground: true, documentHidden: false }), true);
  assert.equal(shouldNotifyWhileAway({ isNative: true, nativeInBackground: false, documentHidden: true }), true);
  assert.equal(shouldNotifyWhileAway({ isNative: true, nativeInBackground: false, documentHidden: false }), false);
});

test("通知渠道用高重要性，否则通知只会静默躺在通知栏里", () => {
  assert.equal(MOBILE_NOTIFICATION_CHANNEL.importance, 4);
  assert.equal(MOBILE_NOTIFICATION_CHANNEL.vibration, true);
  assert.ok(MOBILE_NOTIFICATION_CHANNEL.id.length > 0);
});
