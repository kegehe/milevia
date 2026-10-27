// 真实浏览器验证动态冲突分析工具列表、手机布局及建议接受流程；模型 API 使用桩响应。
import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import { chromium } from "playwright";
import { createServer, transformWithEsbuild } from "vite";

const agents = [
  { id: "claude-code", name: "Claude Code" },
  { id: "codex", name: "Codex" },
  { id: "codebuddy", name: "CodeBuddy Code" },
  { id: "qoder", name: "Qoder CLI" },
  { id: "opencode", name: "OpenCode" },
  { id: "cursor", name: "Cursor CLI" },
  { id: "future-tool", name: "Future Tool" },
];
const entry = `
import React from "react";
import { createRoot } from "react-dom/client";
import { ConflictSolveView } from "/src/features/git/ConflictSolveView.tsx";
import "/src/git.css";
const mobile = new URLSearchParams(location.search).has("mobile");
const request = async (path, init) => {
  const response = await fetch(path, init);
  const data = await response.json();
  if (!response.ok) throw new Error(data.error || "请求失败");
  return data;
};
createRoot(document.getElementById("root")).render(<div className="git-workbench" data-mobile={String(mobile)}>
  <ConflictSolveView projectID="p" conversationId="conversation/1" path="test.txt" conflictPaths={["test.txt"]}
    request={request} fail={message => { window.failure = message; }} oursLabel="main" theirsLabel="feature"
    busy={false} mobile={mobile} onResolve={(...args) => { window.resolved = args; }} onOpenFile={() => {}} onClose={() => {}} />
</div>);
`;
const server = await createServer({
  configFile: fileURLToPath(new URL("../vite.config.ts", import.meta.url)),
  cacheDir: "node_modules/.vite-conflict-verification",
  plugins: [{
    name: "conflict-suggestion-verification",
    resolveId(id) { if (id === "/__conflict-test.tsx") return "\0conflict-test.tsx"; },
    async load(id) {
      if (id === "\0conflict-test.tsx") return (await transformWithEsbuild(entry, "test.tsx", { loader: "tsx", jsx: "automatic" })).code;
    },
    configureServer(vite) {
      vite.middlewares.use((req, res, next) => {
        if (!req.url.startsWith("/__conflict-test?")) return next();
        res.setHeader("Content-Type", "text/html; charset=utf-8");
        res.end('<html><head><style>body{margin:0}</style></head><body><div id="root"></div><script type="module" src="/__conflict-test.tsx"></script></body></html>');
      });
    },
  }],
  server: { host: "127.0.0.1", port: 5198 },
});
let browser;

async function verify(mobile, agent) {
  const context = await browser.newContext({ viewport: mobile ? { width: 390, height: 844 } : { width: 1280, height: 900 } });
  try {
    const page = await context.newPage();
    const errors = [], posts = [];
    page.on("pageerror", error => errors.push(error.message));
    await page.route("**/api/**", async route => {
      const req = route.request(), url = new URL(req.url());
      assert.equal(url.searchParams.get("conversationId"), "conversation/1");
      const suffix = url.pathname.split("/git/")[1];
      let data;
      if (suffix === "conflicts/content") {
        data = { path: "test.txt", kind: "content", binary: false, oversized: false, oursDeleted: false, theirsDeleted: false,
          base: "base\n", ours: "ours\n", theirs: "theirs\n", working: "<<<<<<< HEAD\nours\n=======\ntheirs\n>>>>>>> feature\n",
          // 缺少新字段的旧服务端应禁用启动，不猜测工具能力。
          ...(agent ? { suggestionAgents: agents } : {}) };
      } else if (suffix === "conflicts/suggest") {
        assert.equal(req.method(), "POST");
        posts.push(req.postDataJSON());
        data = { id: "s1", projectId: "p", path: "test.txt", agent: agent.id, status: "running" };
      } else if (suffix === "conflicts/suggestions/s1") {
        data = { id: "s1", projectId: "p", path: "test.txt", agent: agent.id, status: "completed", merged: "merged\n", explanation: "合并两侧内容" };
      } else throw new Error(`Unexpected API ${suffix}`);
      await route.fulfill({ status: suffix === "conflicts/suggest" ? 202 : 200, contentType: "application/json", body: JSON.stringify(data) });
    });
    const base = `http://127.0.0.1:${server.httpServer.address().port}`;
    await page.goto(`${base}/__conflict-test?${mobile ? "mobile=1" : "desktop=1"}`);
    const launch = page.getByRole("button", { name: "AI 生成建议", exact: true });
    await launch.waitFor();
    if (!agent) {
      assert.equal(await launch.isDisabled(), true);
      await page.getByText("暂无可用工具", { exact: true }).waitFor({ state: "attached" });
      assert.deepEqual(posts, []);
      assert.deepEqual(errors, []);
      return;
    }
    const picker = mobile ? page.getByRole("button", { name: agent.name, exact: true }) : page.getByRole("combobox", { name: "AI 模型" });
    if (mobile) {
      assert.equal(await page.getByRole("combobox").count(), 0);
      assert.equal(await page.locator(".git-conflict-ai-agents button").count(), agents.length);
      assert.equal(await page.locator(".git-conflict-ai-agents").evaluate(el => el.scrollWidth <= el.clientWidth + 1), true, "mobile tool buttons must wrap");
      await picker.click();
      assert.equal(await picker.getAttribute("aria-pressed"), "true");
    } else {
      assert.deepEqual(await picker.locator("option").allTextContents(), agents.map(item => item.name));
      await picker.selectOption(agent.id);
    }
    await launch.click();
    const result = page.getByRole("region", { name: "AI 建议", exact: true });
    await result.getByText(`AI 建议（${agent.name}）`, { exact: true }).waitFor();
    assert.deepEqual(posts, [{ path: "test.txt", agent: agent.id }]);
    assert.equal(await picker.isDisabled(), true);
    const accept = result.getByRole("button", { name: "接受并标记为已解决", exact: true });
    await accept.waitFor();
    assert.equal(await page.evaluate(() => window.resolved), undefined);
    if (agent.id === "future-tool") {
      await result.getByRole("button", { name: "拒绝", exact: true }).click();
      assert.equal(await page.evaluate(() => window.resolved), undefined);
      await result.waitFor({ state: "hidden" });
    } else {
      await accept.click();
      assert.deepEqual(await page.evaluate(() => window.resolved), ["test.txt", "working", "merged\n"]);
    }
    assert.equal(await page.evaluate(() => window.failure), undefined);
    assert.deepEqual(errors, []);
    console.log(`PASS: ${mobile ? "mobile" : "desktop"} ${agent.id}: selection, request, title, polling and review`);
  } finally {
    await context.close();
  }
}

async function verifyLatePoll(mobile, failLate, replace = true) {
  const context = await browser.newContext({ viewport: mobile ? { width: 390, height: 844 } : { width: 1280, height: 900 } });
  try {
    const page = await context.newPage();
    let firstPoll;
    let polls = 0;
    let starts = 0;
    const record = (id, agent, status) => ({ id, projectId: "p", path: "test.txt", agent, status, merged: "merged\n" });
    await page.route("**/api/**", async route => {
      const req = route.request();
      const suffix = new URL(req.url()).pathname.split("/git/")[1];
      let data;
      if (suffix === "conflicts/content") {
        data = { path: "test.txt", kind: "content", working: "<<<<<<< HEAD\nours\n=======\ntheirs\n>>>>>>> feature\n", suggestionAgents: agents };
      } else if (suffix === "conflicts/suggest") {
        data = record(`s${++starts}`, req.postDataJSON().agent, "running");
      } else if (suffix === "conflicts/suggestions/s1") {
        if (++polls === 1) {
          firstPoll = route; // 第一枪滞留，第二枪先拿到完成结果。
          return;
        }
        data = record("s1", "opencode", "completed");
      } else if (suffix === "conflicts/suggestions/s2") {
        data = record("s2", "cursor", "running");
      } else throw new Error(`Unexpected API ${suffix}`);
      await route.fulfill({ contentType: "application/json", body: JSON.stringify(data) });
    });
    await page.goto(`http://127.0.0.1:${server.httpServer.address().port}/__conflict-test?${mobile ? "mobile=1" : "desktop=1"}`);
    const choose = async (id, name) => {
      if (mobile) await page.getByRole("button", { name, exact: true }).click();
      else await page.getByRole("combobox", { name: "AI 模型" }).selectOption(id);
    };
    const launch = page.getByRole("button", { name: "AI 生成建议", exact: true });
    await launch.waitFor();
    await choose("opencode", "OpenCode");
    await launch.click();
    const result = page.getByRole("region", { name: "AI 建议", exact: true });
    await result.getByRole("button", { name: "拒绝", exact: true }).waitFor();
    assert.ok(firstPoll);
    if (replace) {
      await result.getByRole("button", { name: "拒绝", exact: true }).click();
      await choose("cursor", "Cursor CLI");
      await launch.click();
      await result.getByText("AI 建议（Cursor CLI）", { exact: true }).waitFor();
    }
    const settled = page.waitForEvent(failLate ? "requestfailed" : "requestfinished", req => req.url().endsWith("/suggestions/s1?conversationId=conversation%2F1"));
    if (failLate) await firstPoll.abort("failed");
    else await firstPoll.fulfill({ contentType: "application/json", body: JSON.stringify(record("s1", "opencode", "running")) });
    await settled;
    // 等响应的状态更新提交到 DOM，避免在回调尚未执行时空跑通过。
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    assert.equal(await result.locator("header > span").first().textContent(), replace ? "AI 建议（Cursor CLI）" : "AI 建议（OpenCode）", "late poll must not replace the current suggestion");
    assert.equal(await result.locator(".git-conflict-ai-status").textContent(), replace ? "生成中…" : "完成", "late polls must not regress the current suggestion status");
    console.log(`PASS: ${mobile ? "mobile" : "desktop"} late ${failLate ? "failure" : "response"} cannot overwrite a ${replace ? "new" : "completed"} suggestion`);
  } finally {
    await context.close();
  }
}

async function verifyPollError(mobile, cancel) {
  const context = await browser.newContext({ viewport: mobile ? { width: 390, height: 844 } : { width: 1280, height: 900 } });
  try {
    const page = await context.newPage();
    let polls = 0;
    let starts = 0;
    let cancelled = false;
    const record = status => ({ id: "s1", projectId: "p", path: "test.txt", agent: "opencode", status, merged: "merged\n" });
    await page.route("**/api/**", async route => {
      const req = route.request();
      const suffix = new URL(req.url()).pathname.split("/git/")[1];
      let data;
      if (suffix === "conflicts/content") {
        data = { path: "test.txt", kind: "content", working: "<<<<<<< HEAD\nours\n=======\ntheirs\n>>>>>>> feature\n", suggestionAgents: agents };
      } else if (suffix === "conflicts/suggest") {
        starts++;
        data = record("running");
      } else if (suffix === "conflicts/suggestions/s1/cancel") {
        assert.equal(req.method(), "POST");
        cancelled = true;
        data = { cancelled: true };
      } else if (suffix === "conflicts/suggestions/s1") {
        if (++polls === 1) {
          await route.fulfill({ status: 503, contentType: "application/json", body: JSON.stringify({ error: "状态服务暂不可用" }) });
          return;
        }
        data = record(cancelled ? "cancelled" : cancel ? "running" : "completed");
      } else throw new Error(`Unexpected API ${suffix}`);
      await route.fulfill({ contentType: "application/json", body: JSON.stringify(data) });
    });
    await page.goto(`http://127.0.0.1:${server.httpServer.address().port}/__conflict-test?${mobile ? "mobile=1" : "desktop=1"}`);
    const launch = page.getByRole("button", { name: "AI 生成建议", exact: true });
    await launch.waitFor();
    if (mobile) await page.getByRole("button", { name: "OpenCode", exact: true }).click();
    else await page.getByRole("combobox", { name: "AI 模型" }).selectOption("opencode");
    await launch.click();
    const result = page.getByRole("region", { name: "AI 建议", exact: true });
    await result.getByText(/状态服务暂不可用/).waitFor();
    assert.equal(await result.locator(".git-conflict-ai-status").textContent(), "生成中…", "poll failure must not report a running task as failed");
    assert.equal(await page.getByRole("button", { name: "AI 生成中…", exact: true }).isDisabled(), true);
    assert.equal(await result.getByRole("button", { name: "取消生成", exact: true }).isVisible(), true);
    if (cancel) {
      await result.getByRole("button", { name: "取消生成", exact: true }).click();
      await result.getByText("已取消", { exact: true }).waitFor();
      assert.equal(cancelled, true);
    } else {
      await result.getByRole("button", { name: "接受并标记为已解决", exact: true }).waitFor({ timeout: 8000 });
      assert.equal(await result.getByText(/状态服务暂不可用/).count(), 0, "successful poll must clear the previous transport error");
    }
    assert.equal(starts, 1, "recovering or cancelling must not start a second task");
    console.log(`PASS: ${mobile ? "mobile" : "desktop"} poll failure preserves ${cancel ? "cancellation" : "automatic recovery"}`);
  } finally {
    await context.close();
  }
}

try {
  await server.listen();
  const executablePath = process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH;
  browser = await chromium.launch(executablePath ? { executablePath } : process.platform === "win32" ? { channel: "msedge" } : {});
  for (const mobile of [false, true]) {
    if (process.argv.includes("--poll-errors-only")) {
      for (const cancel of [false, true]) await verifyPollError(mobile, cancel);
      continue;
    }
    if (!process.argv.includes("--poll-races-only")) {
      for (const agent of agents) await verify(mobile, agent);
      await verify(mobile, null);
      console.log(`PASS: ${mobile ? "mobile" : "desktop"} unavailable tools cannot start analysis`);
    }
    for (const failLate of [false, true]) await verifyLatePoll(mobile, failLate);
    if (!mobile) for (const failLate of [false, true]) await verifyLatePoll(mobile, failLate, false);
    if (!process.argv.includes("--poll-races-only")) for (const cancel of [false, true]) await verifyPollError(mobile, cancel);
  }
} finally {
  await browser?.close();
  await server.close();
}
