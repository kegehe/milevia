import assert from "node:assert/strict";
import test from "node:test";
import { MobileNotificationDelivery, MOBILE_NOTIFICATION_MAX_AGE_MS, isRecoverableMobileNotification, sendNativeMobileNotification } from "./mobile-notification-delivery";
import { advanceMobileStreamCursor, initializeMobileStreamCursor, mobileStreamScope, readMobileNotificationBaseline, readMobileStreamCursor, saveMobileStreamCursor } from "./mobile-stream-state";
import { MOBILE_NOTIFICATION_CHANNEL, shouldPresentMobileNotification } from "../../lib/mobile-notify";

const content = { title: "任务已完成", body: "测试任务", projectId: "p1", conversationId: "c1", taskId: "t1" };
function memoryStore() {
  const values = new Map<string, string>();
  return { values, getItem: (key: string) => values.get(key) ?? null, setItem: (key: string, value: string) => { values.set(key, value); }, removeItem: (key: string) => { values.delete(key); } };
}

test("同一事件并发投递只发送一次，成功记录重启后仍去重", async (t) => {
  const store = memoryStore();
  let sends = 0;
  let complete!: () => void;
  const delivery = new MobileNotificationDelivery({ scope: "a", store, enabled: () => true, send: async () => {
    sends++;
    await new Promise<void>((resolve) => { complete = resolve; });
  } });
  t.after(() => delivery.stop());
  delivery.enqueue("e1", content);
  const first = delivery.flush();
  delivery.enqueue("e1", content);
  await delivery.flush();
  assert.equal(sends, 1);
  complete();
  await first;
  assert.equal(delivery.pendingCount, 0);
  assert.equal(delivery.diagnostic?.status, "scheduled");
  const restored = new MobileNotificationDelivery({ scope: "a", store, enabled: () => true, send: async () => { sends++; } });
  t.after(() => restored.stop());
  restored.enqueue("e1", content);
  await restored.flush();
  assert.equal(sends, 1);
});

test("失败保留待投递记录，重启后能补发，成功后不再重发", async (t) => {
  const store = memoryStore();
  const failing = new MobileNotificationDelivery({ scope: "a", store, enabled: () => true, send: async () => { throw new Error("渠道创建失败"); } });
  t.after(() => failing.stop());
  failing.enqueue("e1", content);
  await failing.flush();
  assert.equal(failing.pendingCount, 1);
  assert.equal(failing.diagnostic?.message, "渠道创建失败");
  failing.stop();
  let sends = 0;
  const restored = new MobileNotificationDelivery({ scope: "a", store, enabled: () => true, send: async () => { sends++; } });
  t.after(() => restored.stop());
  await restored.resume();
  assert.equal(sends, 1);
  assert.equal(restored.pendingCount, 0);
  restored.enqueue("e1", content);
  await restored.flush();
  assert.equal(sends, 1);
});

test("自动重试有上限，恢复权限或回前台后可以重新尝试", async (t) => {
  let clock = 100_000;
  let sends = 0;
  let failing = true;
  const delivery = new MobileNotificationDelivery({ scope: "a", store: memoryStore(), enabled: () => true, now: () => clock, send: async () => {
    sends++;
    if (failing) throw new Error("通知被禁用");
  } });
  t.after(() => delivery.stop());
  delivery.enqueue("e1", content);
  for (let i = 0; i < 5; i++) { await delivery.flush(); clock += 10_000; }
  assert.equal(sends, 3);
  assert.equal(delivery.pendingCount, 1);
  failing = false;
  await delivery.resume();
  assert.equal(sends, 4);
  assert.equal(delivery.pendingCount, 0);
});

test("开关关闭或设备失效时暂停发送，不丢待发送记录", async (t) => {
  let enabled = false;
  let sends = 0;
  const delivery = new MobileNotificationDelivery({ scope: "a", store: memoryStore(), enabled: () => enabled, send: async () => { sends++; } });
  t.after(() => delivery.stop());
  delivery.enqueue("e1", content);
  await delivery.flush();
  assert.equal(sends, 0);
  enabled = true;
  await delivery.resume();
  assert.equal(sends, 1);
});

test("设备独立存储，过期通知不补发，待投递集合最多保留100条", async (t) => {
  const store = memoryStore();
  const clock = MOBILE_NOTIFICATION_MAX_AGE_MS + 1000;
  const first = new MobileNotificationDelivery({ scope: "first", store, enabled: () => false, now: () => clock, send: async () => {} });
  const other = new MobileNotificationDelivery({ scope: "other", store, enabled: () => false, now: () => clock, send: async () => {} });
  t.after(() => { first.stop(); other.stop(); });
  first.enqueue("expired", content, 0);
  assert.equal(first.pendingCount, 0);
  for (let i = 0; i < 150; i++) first.enqueue(`e${i}`, content);
  assert.equal(first.pendingCount, 100);
  assert.equal(other.pendingCount, 0);
});

test("存储失败会向上游报错，恢复存储后同一事件能重新安全入队", () => {
  const store = memoryStore();
  let failing = true;
  const save = store.setItem;
  store.setItem = (key, value) => { if (failing) throw new Error("quota"); save(key, value); };
  const delivery = new MobileNotificationDelivery({ scope: "a", store, enabled: () => false, send: async () => {} });
  assert.throws(() => delivery.enqueue("e1", content), /quota/);
  assert.equal(delivery.pendingCount, 0);
  failing = false;
  delivery.enqueue("e1", content);
  const restored = new MobileNotificationDelivery({ scope: "a", store, enabled: () => false, send: async () => {} });
  assert.equal(restored.pendingCount, 1);
});

test("恢复结果通知，不补发过时的运行中或审批状态", () => {
  for (const type of ["project.batch_completed", "run.completed", "run.failed", "task.run_succeeded"]) assert.equal(isRecoverableMobileNotification(type), true);
  for (const type of ["task.created", "run.started", "approval.pending", "assistant.delta", undefined]) assert.equal(isRecoverableMobileNotification(type), false);
});

function nativeAPI() {
  const payloads: unknown[] = [];
  return {
    payloads,
    checkPermissions: async () => ({ display: "granted" }), areEnabled: async () => ({ value: true }),
    createChannel: async () => {}, listChannels: async () => ({ channels: [{ ...MOBILE_NOTIFICATION_CHANNEL, importance: 4 }] }),
    schedule: async (options: unknown) => { payloads.push(options); },
  };
}

test("原生投递等待渠道创建，携带实例目标并明确禁用精确闹钟", async () => {
  const api = nativeAPI();
  let created = false;
  api.createChannel = async () => { created = true; };
  api.schedule = async (options) => { assert.equal(created, true); api.payloads.push(options); };
  await sendNativeMobileNotification(api, 123, content, "pc1");
  assert.deepEqual(api.payloads, [{ notifications: [{ id: 123, title: content.title, body: content.body, channelId: MOBILE_NOTIFICATION_CHANNEL.id,
    extra: { ...content, instanceId: "pc1" }, autoCancel: true, isExactNotification: false }] }]);
});

test("系统总开关、渠道关闭或渠道创建失败均报错，不调用schedule", async () => {
  const disabled = nativeAPI();
  disabled.areEnabled = async () => ({ value: false });
  await assert.rejects(sendNativeMobileNotification(disabled, 1, content, "pc1"), /系统尚未允许/);
  assert.equal(disabled.payloads.length, 0);
  const channelOff = nativeAPI();
  channelOff.listChannels = async () => ({ channels: [{ ...MOBILE_NOTIFICATION_CHANNEL, importance: 0 }] });
  await assert.rejects(sendNativeMobileNotification(channelOff, 1, content, "pc1"), /类别未开启/);
  assert.equal(channelOff.payloads.length, 0);
  const failure = nativeAPI();
  failure.createChannel = async () => { throw new Error("native failure"); };
  await assert.rejects(sendNativeMobileNotification(failure, 1, content, "pc1"), /native failure/);
  assert.equal(failure.payloads.length, 0);
});

test("权限检查期间切换设备或卸载服务，不向旧设备发送", async () => {
  const api = nativeAPI();
  await assert.rejects(sendNativeMobileNotification(api, 1, content, "pc1", () => false), /设备已切换/);
  assert.equal(api.payloads.length, 0);
});

test("Android 7 不支持通知渠道 API 时仍能发送即时通知", async () => {
  const api = nativeAPI();
  api.createChannel = async () => { throw Object.assign(new Error("not available"), { code: "UNAVAILABLE" }); };
  await sendNativeMobileNotification(api, 1, content, "pc1");
  assert.equal(api.payloads.length, 1);
});

test("队列满时新记录存储失败，原有100条记录都应保留", () => {
  const store = memoryStore();
  const delivery = new MobileNotificationDelivery({ scope: "a", store, enabled: () => false, send: async () => {} });
  for (let i = 0; i < 100; i++) delivery.enqueue(`e${i}`, content);
  const save = store.setItem;
  store.setItem = () => { throw new Error("quota"); };
  assert.throws(() => delivery.enqueue("overflow", content), /quota/);
  assert.equal(delivery.pendingCount, 100);
  store.setItem = save;
});

test("前台已处理的事件重启后也不再投递，待发送事件不会被前台回扫删除", async (t) => {
  const store = memoryStore();
  let sends = 0;
  const first = new MobileNotificationDelivery({ scope: "a", store, enabled: () => false, send: async () => {} });
  t.after(() => first.stop());
  first.rememberHandled("viewed");
  first.enqueue("pending", content);
  first.rememberHandled("pending");
  assert.equal(first.pendingCount, 1);
  assert.equal(first.diagnostic, null);
  const restored = new MobileNotificationDelivery({ scope: "a", store, enabled: () => true, send: async (item) => {
    assert.equal(item.id, "pending");
    sends++;
  } });
  t.after(() => restored.stop());
  restored.enqueue("viewed", content);
  await restored.resume();
  assert.equal(sends, 1);
});

test("前台去重记录持久化失败时回滚，事件仍可重新处理", async (t) => {
  const store = memoryStore();
  let sends = 0;
  const delivery = new MobileNotificationDelivery({ scope: "a", store, enabled: () => true, send: async () => { sends++; } });
  t.after(() => delivery.stop());
  const save = store.setItem;
  store.setItem = () => { throw new Error("quota"); };
  assert.throws(() => delivery.rememberHandled("viewed"), /quota/);
  store.setItem = save;
  delivery.enqueue("viewed", content);
  await delivery.flush();
  assert.equal(sends, 1);
});

test("通知分类开关和跨午夜免打扰生效，高优先级仍服从总开关", () => {
  const preferences = { systemNotificationsEnabled: true, taskNotificationsEnabled: true, lowPriorityNotificationsEnabled: false,
    quietHoursEnabled: false, quietHoursStart: "22:00", quietHoursEnd: "08:00" };
  const night = new Date(2026, 9, 27, 23, 0);
  const day = new Date(2026, 9, 27, 12, 0);
  assert.equal(shouldPresentMobileNotification("run.completed", preferences, night), true);
  assert.equal(shouldPresentMobileNotification("run.started", preferences, night), false);
  assert.equal(shouldPresentMobileNotification("run.completed", { ...preferences, taskNotificationsEnabled: false }, day), false);
  assert.equal(shouldPresentMobileNotification("run.failed", { ...preferences, quietHoursEnabled: true }, night), false);
  assert.equal(shouldPresentMobileNotification("run.failed", { ...preferences, quietHoursEnabled: true }, day), true);
  assert.equal(shouldPresentMobileNotification("approval.pending", { ...preferences, quietHoursEnabled: true, taskNotificationsEnabled: false }, night), true);
  assert.equal(shouldPresentMobileNotification("approval.pending", { ...preferences, systemNotificationsEnabled: false }, night), false);
});

test("排队重试前重新检查偏好，静默处理不会误记为已发送或在重启后补发", async (t) => {
  const store = memoryStore();
  let allowed = true;
  let sends = 0;
  const delivery = new MobileNotificationDelivery({ scope: "a", store, enabled: () => true, shouldSend: () => allowed, send: async () => {
    sends++;
    throw new Error("原生发送失败");
  } });
  t.after(() => delivery.stop());
  delivery.enqueue("e1", content, Date.now(), "run.completed");
  await delivery.flush();
  assert.equal(sends, 1);
  allowed = false;
  await delivery.resume();
  assert.equal(delivery.pendingCount, 0);
  assert.equal(sends, 1);
  assert.equal(delivery.diagnostic?.status, "failed", "静默处理不能写发送成功诊断");
  const restored = new MobileNotificationDelivery({ scope: "a", store, enabled: () => true, send: async () => { sends++; } });
  t.after(() => restored.stop());
  restored.enqueue("e1", content);
  await restored.resume();
  assert.equal(sends, 1);
});

test("服务端先追赶再回扫时，内存断点也不能回退", () => {
  assert.equal(advanceMobileStreamCursor("105", "104"), "105");
  assert.equal(advanceMobileStreamCursor("105", "106"), "106");
  assert.equal(advanceMobileStreamCursor("", "0"), "0");
  for (const value of ["", "-1", "NaN", "9007199254740992"]) assert.equal(advanceMobileStreamCursor("105", value), "105");
});

test("接收断点保留零、单调推进，并按服务器与实例隔离", () => {
  const store = memoryStore();
  const a = mobileStreamScope("https://a.example/v1/stream?instanceId=pc1");
  const b = mobileStreamScope("https://a.example/v1/stream?instanceId=pc2");
  const otherServer = mobileStreamScope("https://b.example/v1/stream?instanceId=pc1");
  assert.equal(readMobileStreamCursor(store, a), null);
  saveMobileStreamCursor(store, a, "0");
  assert.equal(readMobileStreamCursor(store, a), "0");
  saveMobileStreamCursor(store, a, "10");
  saveMobileStreamCursor(store, a, "8");
  assert.equal(readMobileStreamCursor(store, a), "10");
  assert.equal(readMobileStreamCursor(store, b), null);
  assert.equal(readMobileStreamCursor(store, otherServer), null);
  for (const value of ["-1", "NaN", "", "9007199254740992"]) saveMobileStreamCursor(store, a, value);
  assert.equal(readMobileStreamCursor(store, a), "10");
});

test("第一次建立最新基线，重新启动保留已有断点，不重新跳到最新", async (t) => {
  const store = memoryStore();
  let calls = 0;
  t.mock.method(globalThis, "fetch", async (_url: string, init: RequestInit) => {
    calls++;
    assert.equal(new Headers(init.headers).get("Authorization"), "Bearer test-token");
    return new Response(JSON.stringify([{ agentSequence: 8 }, { agentSequence: 10 }]));
  });
  const first = await initializeMobileStreamCursor(store, "pc1", "/v1/events", "test-token", new AbortController().signal);
  assert.equal(first, "10");
  assert.equal(readMobileNotificationBaseline(store, "pc1"), "10");
  assert.equal(calls, 1);
  saveMobileStreamCursor(store, "pc1", "15");
  assert.equal(await initializeMobileStreamCursor(store, "pc1", "/v1/events", "test-token", new AbortController().signal), "15");
  assert.equal(readMobileNotificationBaseline(store, "pc1"), "10", "首次基线不随断点推进");
  assert.equal(calls, 1);
});

test("已有断点迁移时以该断点建立通知基线，不请求最新历史", async (t) => {
  const store = memoryStore();
  saveMobileStreamCursor(store, "pc1", "15");
  t.mock.method(globalThis, "fetch", async () => { throw new Error("不应请求历史"); });
  assert.equal(await initializeMobileStreamCursor(store, "pc1", "/v1/events", "test-token", new AbortController().signal), "15");
  assert.equal(readMobileNotificationBaseline(store, "pc1"), "15");
});

test("基线写入后断点保存失败，重试仍从原基线恢复而不跳过新事件", async (t) => {
  const store = memoryStore();
  let calls = 0;
  t.mock.method(globalThis, "fetch", async () => {
    calls++;
    return new Response(JSON.stringify([{ agentSequence: calls === 1 ? 10 : 200 }]));
  });
  const save = store.setItem;
  store.setItem = (key, value) => { if (key.startsWith("milevia.stream-cursor")) throw new Error("quota"); save(key, value); };
  await assert.rejects(initializeMobileStreamCursor(store, "pc1", "/v1/events", "test-token", new AbortController().signal), /quota/);
  assert.equal(readMobileNotificationBaseline(store, "pc1"), "10");
  assert.equal(readMobileStreamCursor(store, "pc1"), null);
  store.setItem = save;
  assert.equal(await initializeMobileStreamCursor(store, "pc1", "/v1/events", "test-token", new AbortController().signal), "10");
  assert.equal(calls, 1);
});

test("空实例保存零断点，基线请求失败或返回无效格式时不写断点", async (t) => {
  const store = memoryStore();
  let mode = "empty";
  t.mock.method(globalThis, "fetch", async () => mode === "error" ? new Response("error", { status: 503 }) : new Response(JSON.stringify(mode === "empty" ? [] : {})));
  assert.equal(await initializeMobileStreamCursor(store, "empty", "/v1/events", "test-token", new AbortController().signal), "0");
  mode = "error";
  await assert.rejects(initializeMobileStreamCursor(store, "failure", "/v1/events", "test-token", new AbortController().signal), /503/);
  assert.equal(readMobileStreamCursor(store, "failure"), null);
  mode = "invalid";
  await assert.rejects(initializeMobileStreamCursor(store, "invalid", "/v1/events", "test-token", new AbortController().signal), /格式无效/);
  assert.equal(readMobileStreamCursor(store, "invalid"), null);
});
