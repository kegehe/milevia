// 真实浏览器回归：删除当前会话后不能重新激活旧 ID，旧消息不能进入下一条会话。
// 默认使用 Playwright Chromium；可通过 PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH 指定本机浏览器。
import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import { chromium } from "playwright";
import { createServer } from "vite";

const server = await createServer({
  configFile: fileURLToPath(new URL("../vite.config.ts", import.meta.url)),
  server: { host: "127.0.0.1", port: 0 },
});
await server.listen();
const base = `http://127.0.0.1:${server.httpServer.address().port}`;
let browser;

function deferred() {
  let resolve;
  const promise = new Promise((done) => { resolve = done; });
  return { promise, resolve };
}

async function waitForDeferred(signal, name) {
  let timer;
  try {
    await Promise.race([signal.promise, new Promise((_, reject) => {
      timer = setTimeout(() => reject(new Error(`等待${name}超时`)), 10_000);
    })]);
  } finally {
    clearTimeout(timer);
  }
}

async function verify({ name, single = false, lateHistory = false, navigateDuringDelete = false, leaveDuringDelete = false, failFirstDelete = 0 }) {
  const context = await browser.newContext();
  const page = await context.newPage();
  const requestsAfterDeletion = [];
  const pageErrors = [];
  let deleted = false;
  let created = false;
  let holdHistory = false;
  let deleteAttempts = 0;
  const historyReleases = [];
  const historyHeld = deferred();
  const deleteStarted = deferred();
  const deleteRelease = deferred();
  const keepStarted = deferred();
  const keepRelease = deferred();
  const old = {
    id: "old", projectId: "p", title: "旧会话", preview: "旧消息",
    status: "idle", agentId: "claude-code", permissionMode: "approval_required",
    isCurrent: true, isOrchestration: false,
    lastActivityAt: "2026-10-19T00:00:00Z", createdAt: "2026-10-19T00:00:00Z",
  };
  const next = { ...old, id: "next", title: "新会话", preview: "" };
  const keep = { ...next, id: "keep", title: "用户选择的会话" };
  if (single) {
    await page.addInitScript(() => sessionStorage.setItem("milevia.conversation-tabs.v1:p", JSON.stringify({
      openConversationIds: ["old", "next"], activeConversationId: "old",
      readPositions: {}, latestPositions: {}, unreadConversationIds: [],
    })));
  }
  page.on("pageerror", (error) => pageErrors.push(error.message));
  await page.routeWebSocket(/\/ws\//, (socket) => socket.close());
  await page.route("**/api/**", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const path = url.pathname;
    const method = request.method();
    if (deleted) requestsAfterDeletion.push(`${method} ${path}`);
    let status = 200;
    let data;
    if (path === "/api/projects/p") {
      data = { id: "p", name: "删除回归", pathDisplay: "probe", fullPath: "probe", runner: "local",
        environment: "windows", localRunner: true, gitBranch: "main", claudeReady: true, codexReady: true, agentReady: true };
    } else if (path === "/api/projects/statuses") {
      data = [];
    } else if (path === "/api/projects/p/conversations" && method === "GET") {
      // 保存发起时的快照，故意让它在删除成功之后才返回。
      const items = deleted ? (single ? [{ ...next, isCurrent: false }] : created ? [next] : [])
        : [old, ...(single ? [{ ...next, isCurrent: false }] : [])];
      if (holdHistory && !deleted) {
        historyHeld.resolve();
        await new Promise((resolve) => { historyReleases.push(resolve); });
      } else if (deleted) {
        await new Promise((resolve) => setTimeout(resolve, 200));
      }
      data = { items, nextCursor: "" };
    } else if ((path === "/api/projects/p/conversations" || path === "/api/conversations/old") && method === "DELETE") {
      deleteAttempts++;
      if (failFirstDelete && deleteAttempts === 1) {
        await route.fulfill({ status: failFirstDelete, contentType: "application/json",
          body: JSON.stringify({ error: "回归测试：删除暂时被拒绝" }) });
        return;
      }
      deleted = true;
      data = { deleted: 1, skipped: 0, deletedIds: ["old"] };
      deleteStarted.resolve();
      if (navigateDuringDelete || leaveDuringDelete) await deleteRelease.promise;
      setTimeout(() => historyReleases.forEach((release) => release()), 50);
    } else if (path === "/api/projects/p/conversations" && method === "POST") {
      created = true;
      data = next;
    } else if (path === "/api/conversations/old/activate") {
      status = deleted ? 404 : 200;
      data = deleted ? { error: "会话不存在或已被删除。" } : old;
    } else if (path === "/api/conversations/next/activate") {
      data = next;
    } else if (path === "/api/conversations/keep") {
      keepStarted.resolve();
      await keepRelease.promise;
      data = { conversation: keep, messages: [{ id: "m-keep", role: "user", content: "用户选择的会话消息", createdAt: old.createdAt }],
        events: [], activeRunId: null, hasMore: false, hasMoreMessages: false, nextCursor: "" };
    } else if (path === "/api/conversations/old" || path === "/api/conversations/next") {
      const isOld = path.endsWith("/old");
      status = isOld && deleted ? 404 : 200;
      data = status === 404 ? { error: "会话不存在或已被删除。" } : {
        conversation: isOld ? old : next,
        messages: isOld || single ? [{ id: isOld ? "m-old" : "m-next", role: "user",
          content: isOld ? "旧消息" : "相邻会话消息", createdAt: old.createdAt }] : [],
        events: [], activeRunId: null, hasMore: false, hasMoreMessages: false, nextCursor: "",
      };
    } else if (path.endsWith("/usage")) {
      data = { context: { model: "" }, session: { taskCount: 0 } };
    } else if (path.endsWith("/checkpoints")) {
      data = { checkpoints: [] };
    } else if (path.endsWith("/activity")) {
      data = { conversations: [], missingConversationIds: [] };
    } else if (/\/(shortcuts|skills|workspaces|input-history|tasks)(\/|$)/.test(path)) {
      data = [];
    } else if (path.endsWith("/commands")) {
      data = { commands: [], authoritative: false, customAllowed: true };
    } else {
      status = 404;
      data = { error: "未模拟的辅助接口" };
    }
    await route.fulfill({ status, contentType: "application/json", body: JSON.stringify(data) });
  });
  try {
    await page.goto(`${base}/projects/p/conversations/old`);
    await page.locator(".message.user").filter({ hasText: "旧消息" }).waitFor();
    if (failFirstDelete) await page.locator(".composer-input-area textarea").fill("删除失败时保留草稿");
    holdHistory = lateHistory;
    await page.getByRole("button", { name: "历史", exact: true }).click();
    if (lateHistory) await waitForDeferred(historyHeld, "旧历史请求");
    const confirmDeletion = async () => {
      if (single) {
        await page.getByRole("button", { name: "删除会话 旧会话", exact: true }).click();
        await page.locator(".conversation-delete-dialog").getByRole("button", { name: "删除", exact: true }).click();
      } else {
        await page.locator(".history-delete-all").click();
        await page.locator(".conversation-delete-all-dialog").getByRole("button", { name: "清除全部", exact: true }).click();
      }
    };
    await confirmDeletion();
    if (failFirstDelete) {
      await page.getByText("回归测试：删除暂时被拒绝", { exact: true }).waitFor();
      await page.locator(".conversation-delete-dialog, .conversation-delete-all-dialog").waitFor({ state: "hidden" });
      assert.match(page.url(), /\/conversations\/old(?:\?|$)/, "删除失败不能改变当前会话路由");
      assert.equal(await page.locator(".message.user").filter({ hasText: "旧消息" }).count(), 1);
      assert.equal(await page.locator(".composer-input-area textarea").inputValue(), "删除失败时保留草稿");
      const tabs = await page.evaluate(() => JSON.parse(sessionStorage.getItem("milevia.conversation-tabs.v1:p")));
      assert.ok(tabs.openConversationIds.includes("old"), "删除失败不能移除原有 Tab");
      assert.equal(created, false, "删除失败不能自动新建会话");
      await confirmDeletion();
    }
    if (leaveDuringDelete) {
      await waitForDeferred(deleteStarted, "删除请求");
      await page.evaluate(() => window.__mileviaNavigate("/projects/p/tasks/board"));
      await page.waitForURL(/\/tasks\/board$/);
      const deletedResponse = page.waitForResponse((response) => response.request().method() === "DELETE");
      deleteRelease.resolve();
      await deletedResponse;
      await page.waitForTimeout(500);
      assert.match(page.url(), /\/tasks\/board$/, "删除回调不能把用户带回已离开的对话页");
      assert.deepEqual(pageErrors, []);
      console.log(`通过：${name}`);
      return;
    }
    if (navigateDuringDelete) {
      await waitForDeferred(deleteStarted, "删除请求");
      await page.evaluate(() => window.__mileviaNavigate("/projects/p/conversations/keep"));
      await waitForDeferred(keepStarted, "用户选择的会话请求");
      const deletedResponse = page.waitForResponse((response) => response.request().method() === "DELETE");
      deleteRelease.resolve();
      await deletedResponse;
      await page.waitForTimeout(300);
      assert.match(page.url(), /\/conversations\/keep(?:\?|$)/, "删除回调不能覆盖用户的新导航");
      keepRelease.resolve();
    } else {
      await page.waitForURL(/\/conversations\/next(?:\?|$)/);
    }
    await page.waitForFunction(() => !Array.from(document.querySelectorAll(".message")).some((node) => node.textContent.includes("旧消息")));
    if (navigateDuringDelete) await page.locator(".message.user").filter({ hasText: "用户选择的会话消息" }).waitFor();
    else if (single) await page.locator(".message.user").filter({ hasText: "相邻会话消息" }).waitFor();
    else await page.locator(".conversation-starter").waitFor();
    // 等待晚到的目录响应也落地，不能只检查跳转瞬间。
    await page.waitForTimeout(500);
    assert.equal(await page.locator(".message").filter({ hasText: "旧消息" }).count(), 0);
    assert.equal((await page.locator("body").innerText()).includes("会话不存在或已被删除"), false, JSON.stringify(requestsAfterDeletion));
    assert.deepEqual(requestsAfterDeletion.filter((request) => request.includes("/conversations/old")), []);
    assert.equal(requestsAfterDeletion.filter((request) => request === "POST /api/projects/p/conversations").length, single ? 0 : 1);
    assert.deepEqual(pageErrors, []);
    assert.equal(deleteAttempts, failFirstDelete ? 2 : 1);
    console.log(`通过：${name}`);
  } finally {
    historyReleases.forEach((release) => release());
    deleteRelease.resolve();
    keepRelease.resolve();
    await context.close();
  }
}

try {
  browser = await chromium.launch({ executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH || undefined });
  await verify({ name: "清除全部后进入空白新会话" });
  await verify({ name: "删除前的历史响应晚到也不会恢复旧会话", lateHistory: true });
  await verify({ name: "删除当前会话后正确展示相邻会话", single: true });
  await verify({ name: "删除返回时保留用户新选择的会话", single: true, navigateDuringDelete: true });
  await verify({ name: "离开对话页后忽略旧删除回调", single: true, leaveDuringDelete: true });
  await verify({ name: "批量删除被拒绝后保留消息和草稿，并可重试", failFirstDelete: 409 });
  await verify({ name: "单条删除服务端失败后保留消息和草稿，并可重试", single: true, failFirstDelete: 500 });
} finally {
  await browser?.close();
  await server.close();
}
