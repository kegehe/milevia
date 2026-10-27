// 真实浏览器：常驻连接跨路由、失败重试、冷启动补发、设备隔离。
// 原生插件用可控桩替代；不能替代 HarmonyOS 真机的系统展示与后台限制验证。
import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import { chromium } from "playwright";
import { createServer } from "vite";

const probeID = "\0mobile-notifications-probe";
const nativeID = "\0mobile-notifications-native";
const cloudID = "\0mobile-notifications-cloud";
const streams = new Set();
const requests = [];
const events = [{ eventId: "baseline", instanceId: "pc1", agentSequence: 10, type: "run.completed", createdAt: new Date(Date.now() - 60_000).toISOString(), payload: { status: "completed" } }];
let sequence = 10;
const nativeCode = `
  const state = window.__mobileNative = { active: true, attempts: 0, fail: 0, sent: [], listeners: new Map(), stateWaiters: [] };
  function listen(name, handler) {
    if (!state.listeners.has(name)) state.listeners.set(name, new Set());
    state.listeners.get(name).add(handler);
    return Promise.resolve({ remove: async () => state.listeners.get(name).delete(handler) });
  }
  state.fire = (name, value) => { for (const handler of state.listeners.get(name) || []) handler(value); };
  export const Capacitor = { isNativePlatform: () => true, getPlatform: () => "android" };
  export const BarcodeScanner = {};
  export const BarcodeFormat = { QrCode: "QR_CODE" };
  export const LensFacing = { Back: "BACK" };
  export const App = { getState: () => new URLSearchParams(location.search).has("slow-state")
    ? new Promise(resolve => state.stateWaiters.push(() => resolve({ isActive: true }))) : Promise.resolve({ isActive: state.active }),
    getInfo: async () => ({ version: "0.0.0", build: "1" }), addListener: listen };
  export const LocalNotifications = {
    checkPermissions: async () => ({ display: "granted" }), requestPermissions: async () => ({ display: "granted" }),
    areEnabled: async () => ({ value: true }), createChannel: async () => {},
    listChannels: async () => ({ channels: [{ id: "milevia-tasks", importance: 4 }] }), addListener: listen,
    schedule: async (options) => { state.attempts++; if (state.fail > 0) { state.fail--; throw new Error("模拟通知失败"); } state.sent.push(options.notifications[0]); },
  };
`;

const server = await createServer({
  configFile: fileURLToPath(new URL("../vite.config.ts", import.meta.url)),
  cacheDir: fileURLToPath(new URL("../node_modules/.vite-mobile-notifications-test", import.meta.url)),
  server: { host: "127.0.0.1", port: 0, hmr: false, ws: false },
  optimizeDeps: { exclude: ["@capacitor/core", "@capacitor/app", "@capacitor/local-notifications"] },
  resolve: { alias: [
    { find: "@capacitor/core", replacement: nativeID },
    { find: "@capacitor/app", replacement: nativeID },
    { find: "@capacitor/local-notifications", replacement: nativeID },
    { find: "@capacitor-mlkit/barcode-scanning", replacement: nativeID },
    { find: /^.*\/cloud-url(?:\.ts)?$/, replacement: cloudID },
  ] },
  plugins: [{
    name: "mobile-notifications-probe",
    resolveId(id) { if ([probeID, nativeID, cloudID, "/mobile-notifications-probe.js"].includes(id)) return id === "/mobile-notifications-probe.js" ? probeID : id; },
    load(id) {
      if (id === nativeID) return nativeCode;
      if (id === cloudID) return 'export const cloudURL = "";';
      if (id !== probeID) return;
      return `
        import "/src/style.css";
        import "/src/notification.css";
        import { createElement as h, StrictMode, useState } from "react";
        import { createRoot } from "react-dom/client";
        import { MemoryRouter, Routes, Route, useNavigate } from "react-router-dom";
        import { UIPreferencesProvider } from "/src/stores/useUIPreferences.tsx";
        import { MobileRemoteProvider, useMobileNotifications } from "/src/components/MobileRemoteProvider.tsx";
        import { useMobileRemoteEventStream } from "/src/features/remote/use-mobile-remote-event-stream.ts";
        import { setActiveToken } from "/src/lib/mobile-devices.ts";
        import SettingsPage from "/src/pages/SettingsPage.tsx";
        import MobileRemotePage from "/src/pages/MobileRemotePage.tsx";
        import { ProjectProvider } from "/src/stores/useProjectStore.tsx";
        window.switchDevice = setActiveToken;
        function Projects() {
          const [count, setCount] = useState(0);
          const navigate = useNavigate();
          useMobileRemoteEventStream({ instanceID: "pc1", activeToken: "test-token", deviceEpoch: 0, visible: true,
            streamURL: "/v1/stream?instanceId=pc1", isCurrentToken: () => true,
            onEvent: () => { setCount(n => n + 1); return true; }, refreshSnapshot: () => {}, refreshGit: () => {} });
          return h("div", null, h("output", { id: "events" }, count), h("button", { id: "settings", onClick: () => navigate("/settings") }, "设置"));
        }
        function Settings() {
          const service = useMobileNotifications();
          const navigate = useNavigate();
          return h("div", { id: "settings-page" }, h("output", { id: "diagnostic" }, service.diagnostic?.message),
            h("button", { id: "test", onClick: () => service.testNotification() }, "测试通知"),
            h("button", { id: "projects", onClick: () => navigate("/mobile") }, "项目"));
        }
        createRoot(document.getElementById("root")).render(h(StrictMode, null,
          h(MemoryRouter, { initialEntries: [new URLSearchParams(location.search).has("actual") ? "/settings-actual" : new URLSearchParams(location.search).has("settings") ? "/settings" : "/mobile"] },
            h(UIPreferencesProvider, null, h(MobileRemoteProvider, null,
              h(Routes, null, h(Route, { path: "/settings", element: h(Settings) }),
                h(Route, { path: "/settings-actual", element: h(ProjectProvider, null, h(SettingsPage)) }),
                h(Route, { path: "*", element: new URLSearchParams(location.search).has("projects-actual") ? h(MobileRemotePage) : h(Projects) })))))));
      `;
    },
    configureServer(vite) {
      vite.middlewares.use((req, res, next) => {
        const url = new URL(req.url, "http://localhost");
        if (url.pathname === "/v1/instances") {
          res.setHeader("Content-Type", "application/json");
          const instanceId = req.headers.authorization === "Bearer other-token" ? "pc2" : "pc1";
          res.end(JSON.stringify([{ instanceId, name: instanceId === "pc1" ? "第一台电脑" : "第二台电脑", status: "online", lastAgentSequence: sequence }]));
          return;
        }
        if (/^\/v1\/instances\/[^/]+\/snapshot$/.test(url.pathname)) {
          const instanceId = url.pathname.split("/")[3];
          res.setHeader("Content-Type", "application/json");
          res.end(JSON.stringify({ snapshotRevision: sequence, observedAt: new Date().toISOString(), projects: [{
            id: "project1", name: `通知定位项目-${instanceId}`, runner: "codex", gitBranch: "main", tasks: [],
            conversations: [{ id: "conversation1", title: "当前会话", status: "idle", agentId: "codex", isCurrent: true, lastActivityAt: new Date().toISOString(), messages: [] }],
          }] }));
          return;
        }
        if (url.pathname === "/v1/events") {
          res.setHeader("Content-Type", "application/json");
          // 首次启动以前的历史不应弹通知。
          res.end(JSON.stringify(events.filter(item => item.instanceId === url.searchParams.get("instanceId")).slice(-500).reverse()));
          return;
        }
        if (url.pathname !== "/v1/stream") return next();
        const entry = { instanceId: url.searchParams.get("instanceId"), res };
        requests.push({ instanceId: entry.instanceId, cursor: req.headers["last-event-id"] });
        streams.add(entry);
        res.on("close", () => streams.delete(entry));
        res.setHeader("Content-Type", "text/event-stream");
        res.setHeader("Cache-Control", "no-cache");
        res.write(": connected\n\n");
        let cursor = Number(req.headers["last-event-id"] || 0);
        const sent = new Set();
        // 对齐生产服务：先追赶，再回扫断点前100条，兜底乱序上传。
        for (const event of events) if (event.instanceId === entry.instanceId && event.agentSequence > cursor) {
          write(res, event);
          cursor = event.agentSequence;
          sent.add(event.agentSequence);
        }
        for (const event of events) if (event.instanceId === entry.instanceId && event.agentSequence <= cursor && event.agentSequence > cursor - 100 && !sent.has(event.agentSequence)) write(res, event);
      });
    },
  }],
});

function write(res, event) { res.write(`id: ${event.agentSequence}\ndata: ${JSON.stringify(event)}\n\n`); }
function send(id, instanceId = "pc1", live = true, type = "run.completed") {
  const event = { eventId: id, instanceId, agentSequence: ++sequence, type, createdAt: new Date().toISOString(), payload: { status: "completed", projectId: "project1", conversationId: "conversation1" } };
  events.push(event);
  if (live) for (const stream of streams) if (stream.instanceId === instanceId) write(stream.res, event);
  return event;
}
async function until(predicate) {
  const deadline = Date.now() + 10_000;
  while (!predicate()) {
    assert.ok(Date.now() < deadline, "等待事件连接超时");
    await new Promise(resolve => setTimeout(resolve, 20));
  }
}

let browser;
await server.listen();
const base = `http://127.0.0.1:${server.httpServer.address().port}`;
try {
  browser = await chromium.launch({ executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH || undefined });
  const page = await browser.newPage({ viewport: { width: 412, height: 915 } });
  const errors = [];
  page.on("pageerror", error => { errors.push(error.message); console.error("PAGE ERROR:", error.message); });
  page.on("console", message => { if (message.type() === "error") console.error(message.text()); });
  await page.route("**/api/**", route => route.fulfill({ contentType: "application/json", body: "[]" }));
  await page.route("**/mobile-notifications-probe*", route => {
    if (new URL(route.request().url()).pathname.endsWith(".js")) return route.continue();
    return route.fulfill({ contentType: "text/html", body: '<div id="root"></div><script type="module" src="/mobile-notifications-probe.js"></script>' });
  });
  await page.addInitScript(() => {
    if (location.protocol !== "http:") return;
    if (localStorage.getItem("milevia.cloud.devices")) return;
    localStorage.setItem("milevia.cloud.devices", JSON.stringify([
      { token: "test-token", instanceId: "pc1", name: "第一台电脑" },
      { token: "other-token", instanceId: "pc2", name: "第二台电脑" },
    ]));
    localStorage.setItem("milevia.cloud.active", "test-token");
    localStorage.setItem("milevia.cloud.token", "test-token");
    localStorage.setItem("milevia:settings:v1", JSON.stringify({ systemNotificationsEnabled: true }));
  });
  await page.goto(`${base}/mobile-notifications-probe`, { waitUntil: "domcontentloaded", timeout: 60_000 });
  await page.locator("#events").waitFor();
  await until(() => streams.size === 1);
  assert.equal(requests.at(-1).cursor, "10");
  await page.waitForFunction(() => Number(document.getElementById("events")?.textContent) > 0);
  await page.waitForFunction(() => Object.keys(localStorage).some(key => key.startsWith("milevia.stream-cursor.v1:")));
  // 等一帧，确保初始回扫经过通知处理。
  await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  assert.equal(await page.evaluate(() => window.__mobileNative.attempts), 0, "首次基线前的近期历史不应通知");
  console.log("PASS: 首次连接回扫近期历史时不弹出旧通知。");
  const opened = requests.length;
  await page.locator("#settings").click();
  await page.locator("#settings-page").waitFor();
  assert.equal(streams.size, 1);
  assert.equal(requests.length, opened);
  await page.evaluate(() => window.__mobileNative.fire("appStateChange", { isActive: false }));
  send("settings-background");
  await page.waitForFunction(() => window.__mobileNative.sent.length === 1);
  assert.equal(requests.length, opened);
  console.log("PASS: StrictMode 下共用一条连接，进入设置页并切后台仍发送通知。");

  await page.evaluate(() => { window.__mobileNative.fail = 1; });
  send("retry-after-failure");
  await page.waitForFunction(() => document.getElementById("diagnostic")?.textContent.includes("模拟通知失败"));
  await page.waitForFunction(() => window.__mobileNative.sent.length === 2);
  assert.equal(await page.evaluate(() => window.__mobileNative.attempts), 3);
  await page.locator("#test").click();
  await page.waitForFunction(() => window.__mobileNative.sent.length === 3);
  assert.equal(await page.evaluate(() => window.__mobileNative.sent.at(-1).isExactNotification), false);
  console.log("PASS: 失败有诊断且自动重试；测试通知使用即时通知。");

  send("queued-while-locked", "pc1", false);
  await page.evaluate(() => window.__mobileNative.fire("appStateChange", { isActive: true }));
  for (const stream of streams) if (stream.instanceId === "pc1") write(stream.res, events.at(-1));
  await page.waitForFunction(() => window.__mobileNative.sent.length === 4);
  console.log("PASS: 锁屏期间完成、回前台后才处理的结果仍会补发。");

  const viewed = send("already-viewed-in-foreground");
  await page.waitForFunction(sequence => Object.keys(localStorage).some(key => key.startsWith("milevia.stream-cursor.v1:") && localStorage.getItem(key) === String(sequence)), viewed.agentSequence);
  await page.evaluate(() => window.__mobileNative.fire("appStateChange", { isActive: false }));
  for (const stream of streams) if (stream.instanceId === "pc1") write(stream.res, viewed);
  await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  assert.equal(await page.evaluate(() => window.__mobileNative.sent.length), 4, "前台已看过的结果不能因后台回扫再次弹出");
  console.log("PASS: 前台已处理的完成事件在后台回扫时不重复通知。");

  const cursor = String(sequence);
  await page.goto("about:blank");
  await until(() => streams.size === 0);
  send("offline-completion", "pc1", false);
  await page.goto(`${base}/mobile-notifications-probe?settings=1`, { waitUntil: "domcontentloaded", timeout: 60_000 });
  await page.locator("#settings-page").waitFor();
  await page.waitForFunction(() => window.__mobileNative.sent.length === 1);
  assert.equal(requests.at(-1).cursor, cursor);
  assert.equal(await page.evaluate(() => window.__mobileNative.attempts), 1);
  const beforeReconnect = requests.length;
  for (const stream of streams) stream.res.end();
  await until(() => requests.length > beforeReconnect);
  assert.equal(requests.at(-1).cursor, String(sequence), "回扫旧事件后再次重连仍应携带最新断点");
  console.log("PASS: 从设置页冷启动恢复断点，只补发关闭期间的新结果。");

  await page.evaluate(() => window.switchDevice("other-token"));
  await until(() => streams.size === 1 && [...streams][0].instanceId === "pc2");
  await page.evaluate(() => window.__mobileNative.fire("appStateChange", { isActive: false }));
  send("old-device", "pc1");
  send("new-device", "pc2");
  await page.waitForFunction(() => window.__mobileNative.sent.length === 2);
  assert.equal(await page.evaluate(() => window.__mobileNative.sent.at(-1).extra.instanceId), "pc2");
  await page.evaluate(() => window.__mobileNative.fire("localNotificationActionPerformed", { notification: window.__mobileNative.sent[0] }));
  await page.locator("#events").waitFor();
  await until(() => streams.size === 1 && [...streams][0].instanceId === "pc1");
  assert.deepEqual(errors, []);
  console.log("PASS: 切换设备关闭旧连接，通知目标可从设置页返回项目。");

  await page.goto(`${base}/mobile-notifications-probe?actual=1#notifications`, { waitUntil: "domcontentloaded", timeout: 60_000 });
  const testButton = page.getByRole("button", { name: "发送测试通知" });
  await testButton.waitFor();
  await testButton.click();
  await page.waitForFunction(() => window.__mobileNative.sent.some(item => item.title === "Milevia 测试通知"));
  await page.getByRole("status").filter({ hasText: "测试通知已交给系统" }).first().waitFor();
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, "通知设置不应超出手机视口");
  assert.deepEqual(errors, []);
  console.log("PASS: 实际设置页可发送测试通知并显示最近结果。");

  await page.goto(`${base}/mobile-notifications-probe?settings=1&slow-state=1`, { waitUntil: "domcontentloaded", timeout: 60_000 });
  await page.locator("#settings-page").waitFor();
  await until(() => streams.size === 1);
  await page.evaluate(() => {
    window.__mobileNative.fire("appStateChange", { isActive: false });
    for (const resolve of window.__mobileNative.stateWaiters) resolve();
  });
  send("late-initial-state");
  await page.waitForFunction(() => window.__mobileNative.sent.some(item => item.body.includes("任务已完成")), null, { timeout: 5000 });
  console.log("PASS: 晚到的初始前台状态不会覆盖较新的后台事件。");

  await page.evaluate(() => {
    localStorage.setItem("milevia.cloud.devices", JSON.stringify([{ token: "test-token", instanceId: "", name: "" }]));
    localStorage.setItem("milevia.cloud.active", "test-token");
    localStorage.setItem("milevia.cloud.token", "test-token");
  });
  await page.goto(`${base}/mobile-notifications-probe?settings=1`, { waitUntil: "domcontentloaded", timeout: 60_000 });
  await page.locator("#settings-page").waitFor();
  await until(() => streams.size === 1 && [...streams][0].instanceId === "pc1");
  console.log("PASS: 旧配对记录缺少电脑标识时，从设置页启动也能恢复通知连接。");

  await page.goto(`${base}/mobile-notifications-probe?actual=1#notifications`, { waitUntil: "domcontentloaded", timeout: 60_000 });
  await page.getByRole("button", { name: "发送测试通知" }).waitFor();
  await until(() => streams.size === 1);
  const taskToggle = page.locator(".settings-card").filter({ hasText: "任务完成与失败" }).locator('input[type="checkbox"]');
  await taskToggle.uncheck();
  await page.evaluate(() => window.__mobileNative.fire("appStateChange", { isActive: false }));
  const beforeMuted = await page.evaluate(() => window.__mobileNative.attempts);
  const muted = send("task-notifications-disabled");
  await page.waitForFunction(sequence => Object.keys(localStorage).some(key => key.startsWith("milevia.stream-cursor.v1:") && localStorage.getItem(key) === String(sequence)), muted.agentSequence);
  await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  assert.equal(await page.evaluate(() => window.__mobileNative.attempts), beforeMuted, "关闭任务完成与失败开关后不应继续发送普通通知");
  console.log("PASS: 实际设置页的任务通知开关控制后台投递。");

  await taskToggle.check();
  const quietTimes = await page.evaluate(() => {
    const now = new Date();
    const minutes = now.getHours() * 60 + now.getMinutes();
    const clock = value => `${String(Math.floor(value / 60)).padStart(2, "0")}:${String(value % 60).padStart(2, "0")}`;
    return { start: clock((minutes + 1439) % 1440), end: clock((minutes + 2) % 1440) };
  });
  await page.locator(".settings-card").filter({ hasText: "免打扰时段" }).locator('input[type="checkbox"]').check();
  await page.getByLabel("免打扰开始时间").fill(quietTimes.start);
  await page.getByLabel("免打扰结束时间").fill(quietTimes.end);
  const quiet = send("quiet-hours-completion");
  await page.waitForFunction(sequence => Object.keys(localStorage).some(key => key.startsWith("milevia.stream-cursor.v1:") && localStorage.getItem(key) === String(sequence)), quiet.agentSequence);
  await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  assert.equal(await page.evaluate(() => window.__mobileNative.attempts), beforeMuted, "免打扰时段不发送普通完成通知");
  send("approval-during-quiet-hours", "pc1", true, "approval.pending");
  await page.waitForFunction(count => window.__mobileNative.attempts === count + 1, beforeMuted);
  console.log("PASS: 实际免打扰设置抑制普通通知，审批提醒仍按高优先级处理。");

  await page.evaluate(() => {
    localStorage.setItem("milevia:settings:v1", JSON.stringify({ systemNotificationsEnabled: true }));
    localStorage.setItem("milevia.cloud.devices", JSON.stringify([
      { token: "test-token", instanceId: "pc1", name: "第一台电脑" },
      { token: "other-token", instanceId: "pc2", name: "第二台电脑" },
    ]));
    localStorage.setItem("milevia.cloud.active", "other-token");
    localStorage.setItem("milevia.cloud.token", "other-token");
  });
  await page.goto(`${base}/mobile-notifications-probe?settings=1&projects-actual=1`, { waitUntil: "domcontentloaded", timeout: 60_000 });
  await page.locator("#settings-page").waitFor();
  await until(() => streams.size === 1 && [...streams][0].instanceId === "pc2");
  await page.evaluate(() => window.__mobileNative.fire("localNotificationActionPerformed", { notification: { extra: { instanceId: "pc1", projectId: "project1", conversationId: "conversation1", taskId: "" } } }));
  await page.locator(".mobile-conversation-header, .mobile-remote-header").filter({ hasText: "通知定位项目-pc1" }).waitFor();
  await until(() => streams.size === 1 && [...streams][0].instanceId === "pc1");
  assert.equal(await page.evaluate(() => localStorage.getItem("milevia.cloud.active")), "test-token");
  assert.deepEqual(errors, []);
  console.log("PASS: 通知点击从设置页切换电脑，真实项目页等待快照后定位所属项目。");

  await page.evaluate(() => window.__mobileNative.fire("localNotificationActionPerformed", { notification: { extra: { instanceId: "pc2", projectId: "project1", conversationId: "conversation1", taskId: "" } } }));
  await page.locator(".mobile-remote-header").filter({ hasText: "通知定位项目-pc2" }).waitFor();
  await until(() => streams.size === 1 && [...streams][0].instanceId === "pc2");
  assert.equal(await page.evaluate(() => localStorage.getItem("milevia.cloud.active")), "other-token");
  await page.evaluate(() => window.__mobileNative.fire("appStateChange", { isActive: false }));
  const actualSends = await page.evaluate(() => window.__mobileNative.sent.length);
  send("actual-page-background-result", "pc2");
  await page.waitForFunction(count => window.__mobileNative.sent.length === count + 1, actualSends);
  assert.deepEqual(errors, []);
  console.log("PASS: 真实项目页挂载期间跨电脑通知可切换目标，共享流仍能投递后台完成通知。");
} finally {
  await browser?.close();
  for (const stream of streams) stream.res.end();
  await server.close();
}
