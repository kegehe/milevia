// 真实浏览器回归：有界通知对账、清理后的晚到响应、拉取期间的实时通知和已读操作。
import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import { chromium } from "playwright";
import { createServer } from "vite";

const probeID = "\0notification-retention-probe";
const server = await createServer({
  configFile: fileURLToPath(new URL("../vite.config.ts", import.meta.url)),
  cacheDir: fileURLToPath(new URL("../node_modules/.vite-notification-test", import.meta.url)),
  server: { host: "127.0.0.1", port: 0 },
  plugins: [{
    name: "notification-retention-probe",
    resolveId(id) { if (id === "/notification-retention-probe.js") return probeID; },
    load(id) {
      if (id !== probeID) return;
      return `
        import { createElement as h, Fragment, StrictMode } from "react";
        import { createRoot } from "react-dom/client";
        import { MemoryRouter } from "react-router-dom";
        import { UIPreferencesProvider } from "/src/stores/useUIPreferences.tsx";
        import { NotificationProvider, useUnreadCount, useNotificationRefresh } from "/src/components/NotificationProvider.tsx";
        import { LiveEventsProvider } from "/src/components/LiveEventsProvider.tsx";
        import { NotificationInvalidationBridge } from "/src/components/NotificationInvalidationBridge.tsx";
        import NotificationCenter from "/src/components/NotificationCenter.tsx";
        function Probe() {
          const count = useUnreadCount();
          const refresh = useNotificationRefresh();
          return h(Fragment, null, h("output", {id: "unread"}, count), h("button", {id: "refresh", onClick: refresh}, "refresh"), h(NotificationCenter));
        }
        const Wrapper = new URLSearchParams(window.location.search).has("strict") ? StrictMode : Fragment;
        createRoot(document.getElementById("root")).render(
          h(Wrapper, null, h(MemoryRouter, null, h(UIPreferencesProvider, null, h(NotificationProvider, null,
            h(LiveEventsProvider, null, h(NotificationInvalidationBridge), h(Probe))))))
        );
      `;
    },
  }],
});
await server.listen();
const base = `http://127.0.0.1:${server.httpServer.address().port}`;
let browser;

function notification(id, time = Date.now()) {
  return { id, type: "task.done", projectId: "", projectName: "", title: id, body: "", priority: "normal", actionUrl: "/", createdAt: new Date(time).toISOString() };
}

async function fixture(initial, holdInitial = false, strict = false) {
  const context = await browser.newContext();
  const page = await context.newPage();
  const errors = [];
  let current = initial;
  let holdNext = holdInitial;
  let socket;
  let stateSocket;
  const held = [];
  const writes = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await page.routeWebSocket(/\/ws\/notifications/, (ws) => { socket = ws; });
  await page.routeWebSocket(/\/ws\/events/, (ws) => { stateSocket = ws; });
  await page.route(/\/notification-retention-probe(?:\?.*)?$/, (route) => route.fulfill({
    contentType: "text/html", body: '<div id="root"></div><script type="module" src="/notification-retention-probe.js"></script>',
  }));
  await page.route("**/api/**", async (route) => {
    if (route.request().method() !== "GET") writes.push(route.request().url());
    let data = {};
    if (new URL(route.request().url()).pathname === "/api/notifications") {
      data = current;
      if (holdNext) {
        holdNext = false;
        await new Promise((resolve) => held.push(resolve));
      }
    }
    await route.fulfill({ contentType: "application/json", body: JSON.stringify(data) });
  });
  await page.goto(`${base}/notification-retention-probe${strict ? "?strict=1" : ""}`);
  await page.locator("#unread").waitFor();
  const waitCount = (expected) => page.waitForFunction((count) => document.getElementById("unread")?.textContent === String(count), expected);
  const waitHeld = async () => {
    const deadline = Date.now() + 10_000;
    while (!held.length) {
      assert.ok(Date.now() < deadline, "等待通知请求超时");
      await new Promise((resolve) => setTimeout(resolve, 10));
    }
  };
  return {
    page, waitCount, waitHeld, writes,
    setList(list) { current = list; },
    holdNext() { holdNext = true; },
    async refresh() {
      const response = holdNext ? null : page.waitForResponse((value) => new URL(value.url()).pathname === "/api/notifications");
      await page.locator("#refresh").click();
      if (response) await (await response).finished();
    },
    async release() {
      const response = page.waitForResponse((value) => new URL(value.url()).pathname === "/api/notifications");
      held.shift()();
      await response;
      // 等待晚到响应经过 api 解码并应用到 React，检查最终状态而非响应刚到的瞬间。
      await page.waitForTimeout(100);
    },
    async send(event) {
      assert.ok(socket, "通知 WebSocket 尚未连接");
      socket.send(JSON.stringify(event));
    },
    async sendState(event) {
      assert.ok(stateSocket, "状态事件 WebSocket 尚未连接");
      stateSocket.send(JSON.stringify(event));
    },
    async close() {
      held.splice(0).forEach((release) => release());
      await context.close();
      assert.deepEqual(errors, []);
    },
  };
}

async function verify(name, initial, run, holdInitial = false, strict = false) {
  const test = await fixture(initial, holdInitial, strict);
  try {
    await run(test);
    console.log(`通过：${name}`);
  } finally {
    await test.close();
  }
}

try {
  browser = await chromium.launch({ executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH || undefined });
  const now = Date.now();
  const deleted = Array.from({ length: 1000 }, (_, index) => notification(`deleted-${index}`, now + index));
  const retained = Array.from({ length: 1000 }, (_, index) => notification(`retained-${index}`, now - 2000 + index));
  await verify("满容量清理后补回旧通知仍保留正确角标", deleted, async (test) => {
    await test.waitCount(1000);
    test.setList(retained);
    await test.refresh();
    await test.waitCount(1000);
    // 后续已读事件必须能从保留集合里扣减，不能仅仅把显示数字钳制成 1000。
    await test.page.evaluate(() => window.dispatchEvent(new CustomEvent("notification:dismissed", { detail: { id: "retained-0" } })));
    await test.waitCount(999);
  });
  await verify("初始旧响应晚到不能恢复已经清理的通知", deleted, async (test) => {
    await test.waitHeld();
    test.setList([]);
    await test.refresh();
    await test.release();
    assert.equal(await test.page.locator("#unread").textContent(), "0");
  }, true);
  await verify("并发刷新只采用最新响应", deleted, async (test) => {
    await test.waitCount(1000);
    test.holdNext();
    await test.refresh();
    await test.waitHeld();
    test.setList([]);
    await test.refresh();
    await test.waitCount(0);
    await test.release();
    assert.equal(await test.page.locator("#unread").textContent(), "0");
  });
  await verify("首拉期间到达的实时通知不被空快照删除", [], async (test) => {
    await test.waitHeld();
    await test.send(notification("live-during-fetch"));
    await test.waitCount(1);
    await test.release();
    assert.equal(await test.page.locator("#unread").textContent(), "1");
  }, true);
  await verify("拉取期间忽略的通知不被晚到快照恢复", [notification("dismiss-during-fetch")], async (test) => {
    await test.waitCount(1);
    test.holdNext();
    await test.refresh();
    await test.waitHeld();
    await test.page.evaluate(() => window.dispatchEvent(new CustomEvent("notification:dismissed", { detail: { id: "dismiss-during-fetch" } })));
    await test.waitCount(0);
    await test.release();
    assert.equal(await test.page.locator("#unread").textContent(), "0");
  });
  const sameMillisecond = new Date(now).toISOString().slice(0, -1);
  const older = { ...notification("zzz-older"), createdAt: `${sameMillisecond}000100Z` };
  const newer = { ...notification("aaa-newer"), createdAt: `${sameMillisecond}000200Z` };
  const recent = Array.from({ length: 999 }, (_, index) => notification(`recent-${index}`, now + 1000 + index));
  await verify("满容量裁剪保留同一毫秒内真正较新的通知", [...recent, older], async (test) => {
    await test.waitCount(1000);
    await test.send(newer);
    // 先等实时事件完成处理，再验证忽略它能正确扣减集合。
    await test.page.locator('[data-sonner-toast]').filter({ hasText: "aaa-newer" }).waitFor();
    test.holdNext();
    await test.page.evaluate(() => window.dispatchEvent(new CustomEvent("notification:dismissed", { detail: { id: "aaa-newer" } })));
    await test.waitCount(999);
    await test.waitHeld();
  });
  await verify("全部已读后同一毫秒内的新通知仍可正常计数", [], async (test) => {
    await test.refresh();
    await test.page.evaluate((dismissedAt) => window.dispatchEvent(new CustomEvent("notification:dismissed-all", { detail: { dismissedAt } })), older.createdAt);
    await test.send(newer);
    await test.waitCount(1);
    await test.send(older);
    await test.page.waitForTimeout(100);
    assert.equal(await test.page.locator("#unread").textContent(), "1");
  });
  await verify("忽略后服务端补入更早通知时角标同步补位", deleted, async (test) => {
    await test.waitCount(1000);
    test.setList([...deleted.slice(1), notification("backfilled", now - 1)]);
    await test.page.evaluate(() => window.dispatchEvent(new CustomEvent("notification:dismissed", { detail: { id: "deleted-0" } })));
    await test.waitCount(1000);
    await test.page.evaluate(() => window.dispatchEvent(new CustomEvent("notification:dismissed", { detail: { id: "backfilled" } })));
    await test.waitCount(999);
  });
  await verify("超过去重容量后历史重放不重复弹窗", deleted, async (test) => {
    await test.waitCount(1000);
    await test.send(notification("one-more-live", now + 2000));
    await test.page.locator('[data-sonner-toast]').filter({ hasText: "one-more-live" }).waitFor();
    await test.send(deleted[999]);
    await test.page.waitForTimeout(150);
    assert.equal(await test.page.locator('[data-sonner-toast]').filter({ hasText: "deleted-999" }).count(), 0);
  });
  await verify("跨标签页全部已读保留纳秒边界并兼容旧毫秒字段", [older], async (test) => {
    await test.waitCount(1);
    await test.page.evaluate((dismissedAt) => {
      const milliseconds = Date.parse(dismissedAt);
      const channel = new BroadcastChannel("app-notifications");
      channel.postMessage({ type: "dismissed-all", dismissedAt: milliseconds, dismissedAtNs: BigInt(milliseconds) * 1_000_000n + 100n });
      channel.close();
    }, older.createdAt);
    await test.waitCount(0);
    await test.send(newer);
    await test.waitCount(1);
    await test.page.evaluate((dismissedAt) => {
      const channel = new BroadcastChannel("app-notifications");
      channel.postMessage({ type: "dismissed-all", dismissedAt });
      channel.close();
    }, now + 1);
    await test.waitCount(0);
  });
  await verify("旧首拉结束不能提前放行仍在补发的历史弹窗", [], async (test) => {
    await test.waitHeld();
    const history = notification("history-during-newer-refresh");
    test.setList([history]);
    test.holdNext();
    await test.refresh();
    await test.release(); // 旧首拉先结束，第二次刷新仍在等待。
    await test.send(history);
    await test.waitCount(1);
    await test.page.waitForTimeout(100);
    assert.equal(await test.page.locator('[data-sonner-toast]').filter({ hasText: history.title }).count(), 0);
    await test.release();
  }, true);
  await verify("状态通道重连的 all 信号补齐断线期间遗漏的通知清理", deleted, async (test) => {
    await test.waitCount(1000);
    test.setList([]);
    await test.sendState({ type: "all" });
    await test.waitCount(0);
  });
  await verify("清理通知时同时收起持久弹窗，不把清理误报为用户已读", [], async (test) => {
    await test.refresh();
    const event = { ...notification("persistent-before-retention"), priority: "high" };
    await test.send(event);
    await test.waitCount(1);
    const popup = test.page.locator('[data-sonner-toast]').filter({ hasText: event.title });
    await popup.waitFor();
    await test.sendState({ type: "notifications" });
    await test.waitCount(0);
    await popup.waitFor({ state: "hidden" });
    assert.deepEqual(test.writes, []);
  });
  await verify("另一标签页已读后同时收起本页持久弹窗", [], async (test) => {
    await test.refresh();
    const event = { ...notification("persistent-before-peer-dismiss"), priority: "high" };
    await test.send(event);
    await test.waitCount(1);
    const popup = test.page.locator('[data-sonner-toast]').filter({ hasText: event.title });
    await popup.waitFor();
    await test.page.evaluate((id) => {
      const channel = new BroadcastChannel("app-notifications");
      channel.postMessage({ type: "dismissed", id });
      channel.close();
    }, event.id);
    await test.waitCount(0);
    await popup.waitFor({ state: "hidden" });
    assert.deepEqual(test.writes, []);
  });
  await verify("用户主动关闭持久弹窗仍提交一次已读操作", [], async (test) => {
    await test.refresh();
    const event = { ...notification("persistent-user-dismiss"), priority: "high" };
    await test.send(event);
    await test.waitCount(1);
    const popup = test.page.locator('[data-sonner-toast]').filter({ hasText: event.title });
    await popup.waitFor();
    await popup.locator('[data-close-button]').click();
    await test.waitCount(0);
    await popup.waitFor({ state: "hidden" });
    assert.equal(test.writes.length, 1);
    assert.ok(test.writes[0].endsWith(`/api/notifications/${event.id}/dismiss`));
  });
  await verify("查看弹窗详情只提交一次已读操作", [], async (test) => {
    await test.refresh();
    const event = { ...notification("persistent-user-view"), priority: "high" };
    await test.send(event);
    await test.waitCount(1);
    const popup = test.page.locator('[data-sonner-toast]').filter({ hasText: event.title });
    await popup.waitFor();
    await popup.getByRole("button", { name: "查看详情" }).click();
    await test.waitCount(0);
    await popup.waitFor({ state: "hidden" });
    assert.equal(test.writes.length, 1);
  });
  await verify("StrictMode 重建订阅后仍正确恢复未读并接收实时通知", deleted, async (test) => {
    await test.waitCount(1000);
    await test.send(notification("strict-live", now + 3000));
    await test.page.locator('[data-sonner-toast]').filter({ hasText: "strict-live" }).waitFor();
    test.setList([]);
    await test.sendState({ type: "notifications" });
    await test.waitCount(0);
  }, false, true);
  await verify("通知中心打开时清理同步更新列表和角标", [notification("center-before-cleanup")], async (test) => {
    await test.waitCount(1);
    await test.page.locator(".notification-bell").click();
    await test.page.locator(".notification-item").filter({ hasText: "center-before-cleanup" }).waitFor();
    test.setList([]);
    await test.sendState({ type: "notifications" });
    await test.waitCount(0);
    await test.page.getByText("暂无未读通知", { exact: true }).waitFor();
    assert.equal(await test.page.locator(".notification-item").count(), 0);
  }, false, true);
} finally {
  await browser?.close();
  await server.close();
}
