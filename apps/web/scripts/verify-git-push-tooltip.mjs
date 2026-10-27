// 在真实浏览器中验证：提交后按钮解禁、旧提示清理、悬停期间提示更新。
import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import { chromium } from "playwright";
import { createServer, transformWithEsbuild } from "vite";

const entry = `
import React from "react";
import { createRoot } from "react-dom/client";
import { TooltipProvider } from "/src/components/TooltipProvider.tsx";
import { GitWorkbench } from "/src/features/git/GitWorkbench.tsx";
import "/src/style.css";
import "/src/git.css";
const request = async (path, init) => {
  const response = await fetch(path, init);
  return response.json();
};
function Harness() {
  const [title, setTitle] = React.useState("旧提示");
  const [description, setDescription] = React.useState("旧说明");
  window.setTestTitle = setTitle;
  window.setTestDescription = setDescription;
  return <TooltipProvider>
    <button id="dynamic-title" title={title}>动态提示</button>
    <button id="structured-tooltip" data-tooltip-title="结构化提示" data-tooltip-desc={description}>结构化提示</button>
    <p id="errors" />
    <GitWorkbench projectID="p" active request={request}
      mobile={new URLSearchParams(location.search).has("mobile")}
      fail={(message) => { document.getElementById("errors").textContent = message; }} />
  </TooltipProvider>;
}
createRoot(document.getElementById("root")).render(<React.StrictMode><Harness /></React.StrictMode>);
`;
const server = await createServer({
  configFile: fileURLToPath(new URL("../vite.config.ts", import.meta.url)),
  plugins: [{
    name: "git-push-tooltip-test",
    resolveId(id) { if (id === "/__git-push-tooltip.tsx") return "\0git-push-tooltip.tsx"; },
    async load(id) {
      if (id === "\0git-push-tooltip.tsx") return (await transformWithEsbuild(entry, "test.tsx", { loader: "tsx", jsx: "automatic" })).code;
    },
    configureServer(vite) {
      vite.middlewares.use((req, res, next) => {
        if (!req.url.startsWith("/__git-push-tooltip?")) return next();
        res.setHeader("Content-Type", "text/html; charset=utf-8");
        res.end('<html><body><div id="root"></div><script type="module" src="/__git-push-tooltip.tsx"></script></body></html>');
      });
    },
  }],
  server: { host: "127.0.0.1", port: 0 },
});
await server.listen();
const base = `http://127.0.0.1:${server.httpServer.address().port}`;
let browser;

async function verify({ mobile = false, upstream = "origin/main", unborn = false } = {}) {
  const page = await browser.newPage({ viewport: mobile ? { width: 390, height: 844 } : { width: 1280, height: 900 } });
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  let committed = false;
  const writes = [];
  await page.route("**/api/**", async (route) => {
    const req = route.request();
    const suffix = new URL(req.url()).pathname.split("/git/")[1];
    let data = [];
    if (req.method() === "POST") {
      writes.push(suffix);
      assert.equal(suffix, "commits", "测试不应发送推送请求");
      committed = true;
      data = { operationId: "commit-test", status: "succeeded" };
    } else if (suffix === "summary") {
      data = {
        repositoryState: "ready", stateToken: committed ? "new-token" : "old-token",
        head: { oid: unborn ? "(initial)" : (committed ? "b" : "a").repeat(40), branch: "main", detached: false, upstream, ahead: committed && upstream ? 1 : 0, behind: 0 },
        worktree: { staged: committed ? 0 : 1, modified: 0, untracked: 0, deleted: 0, renamed: 0, conflicted: 0 },
        changes: committed ? [] : [{ path: "example.txt", staged: true, modified: false, untracked: false }],
      };
    } else if (suffix === "conflicts") {
      data = { context: { operationType: "none", canAbort: false, canFinish: false }, files: [] };
    }
    await route.fulfill({ contentType: "application/json", body: JSON.stringify(data) });
  });
  try {
    await page.goto(`${base}/__git-push-tooltip?${mobile ? "mobile=1" : "desktop=1"}`);
    const push = page.locator(".git-push");
    await page.locator(".git-bar-name").waitFor();
    if (unborn || !upstream) {
      assert.equal(await push.isDisabled(), unborn);
      if (unborn) {
        await push.hover();
        await page.getByRole("tooltip").getByText("创建首次提交后才能推送", { exact: true }).waitFor();
      } else {
        assert.equal(await push.getAttribute("data-tooltip"), null);
        await push.click();
        const dialog = page.getByRole("dialog", { name: "推送到远端", exact: true });
        await dialog.getByText("将同时设置上游跟踪", { exact: true }).waitFor();
      }
      assert.deepEqual(writes, []);
      assert.deepEqual(errors, []);
      assert.equal(await page.locator("#errors").innerText(), "");
      return;
    }
    assert.equal(await push.isDisabled(), true);
    await push.hover();
    await page.getByRole("tooltip").getByText("没有需要推送的提交", { exact: true }).waitFor();
    await page.locator(".git-commit-panel textarea").fill("测试提交后提示");
    await page.locator(".git-commit-panel .primary").click();
    await page.getByRole("button", { name: "确认提交", exact: true }).click();
    await page.waitForFunction(() => document.querySelector(".git-push")?.disabled === false);
    assert.match(await page.locator(".git-sync-badge").innerText(), /领先 1/);
    await push.hover();
    assert.equal(await push.getAttribute("data-tooltip"), null);
    assert.equal(await page.getByRole("tooltip").count(), 0);
    assert.deepEqual(writes, ["commits"]);

    const dynamic = page.locator("#dynamic-title");
    await dynamic.hover();
    await page.getByRole("tooltip").getByText("旧提示", { exact: true }).waitFor();
    await page.evaluate(() => window.setTestTitle("新提示"));
    await page.getByRole("tooltip").getByText("新提示", { exact: true }).waitFor();
    await page.evaluate(() => window.setTestTitle(undefined));
    await page.getByRole("tooltip").waitFor({ state: "hidden" });
    assert.equal(await dynamic.getAttribute("data-tooltip"), null);
    assert.equal(await dynamic.getAttribute("aria-describedby"), null);
    await page.mouse.move(page.viewportSize().width - 5, page.viewportSize().height - 5);
    await page.evaluate(() => window.setTestTitle("再次出现"));
    await page.waitForFunction(() => document.querySelector("#dynamic-title")?.dataset.tooltip === "再次出现");
    await dynamic.hover();
    await page.getByRole("tooltip").getByText("再次出现", { exact: true }).waitFor();
    await page.evaluate(() => window.setTestTitle(""));
    await page.getByRole("tooltip").waitFor({ state: "hidden" });
    assert.equal(await dynamic.getAttribute("data-tooltip"), null);
    await page.locator("#structured-tooltip").hover();
    await page.getByRole("tooltip").getByText("旧说明", { exact: true }).waitFor();
    await page.evaluate(() => window.setTestDescription("新说明"));
    await page.getByRole("tooltip").getByText("新说明", { exact: true }).waitFor();
    // title 更新与 pointerover 发生在同一轮事件中，不能把转换器自身的置空当作调用方清空。
    await page.evaluate(() => {
      const target = document.querySelector("#dynamic-title");
      target.setAttribute("title", "即时提示");
      target.dispatchEvent(new PointerEvent("pointerover", { bubbles: true, pointerType: "mouse" }));
    });
    await page.getByRole("tooltip").getByText("即时提示", { exact: true }).waitFor();
    await page.evaluate(() => {
      const target = document.querySelector("#dynamic-title");
      target.setAttribute("title", "临时提示");
      target.setAttribute("title", "");
    });
    await page.getByRole("tooltip").waitFor({ state: "hidden" });
    assert.equal(await dynamic.getAttribute("data-tooltip"), null);
    // 同一批属性变化只采用最终文字，不能还原或显示中间状态。
    await page.evaluate(() => {
      const target = document.querySelector("#dynamic-title");
      target.setAttribute("title", "中间提示");
      target.setAttribute("title", "最终提示");
      target.dispatchEvent(new PointerEvent("pointerover", { bubbles: true, pointerType: "mouse" }));
    });
    await page.getByRole("tooltip").getByText("最终提示", { exact: true }).waitFor();
    await page.evaluate(() => document.querySelector("#dynamic-title").setAttribute("title", " \n\t "));
    await page.getByRole("tooltip").waitFor({ state: "hidden" });
    assert.equal(await dynamic.getAttribute("data-tooltip"), null);
    // 调用方独立管理 data-tooltip 时，清理 title 的副本不能误删调用方的新内容。
    await page.evaluate(() => document.querySelector("#dynamic-title").setAttribute("title", "转换产生的提示"));
    await page.waitForFunction(() => document.querySelector("#dynamic-title")?.dataset.tooltip === "转换产生的提示");
    await page.evaluate(() => {
      const target = document.querySelector("#dynamic-title");
      target.dataset.tooltip = "调用方的新提示";
      target.removeAttribute("title");
      target.dispatchEvent(new PointerEvent("pointerover", { bubbles: true, pointerType: "mouse" }));
    });
    await page.getByRole("tooltip").getByText("调用方的新提示", { exact: true }).waitFor();
    assert.equal(await dynamic.getAttribute("data-tooltip"), "调用方的新提示");
    await dynamic.evaluate((target) => target.removeAttribute("data-tooltip"));
    await page.getByRole("tooltip").waitFor({ state: "hidden" });
    await page.mouse.move(page.viewportSize().width - 5, page.viewportSize().height - 5);
    await page.locator("#structured-tooltip").hover();
    await page.getByRole("tooltip").getByText("新说明", { exact: true }).waitFor();
    await page.locator("#structured-tooltip").evaluate((target) => target.removeAttribute("data-tooltip-title"));
    await page.getByRole("tooltip").waitFor({ state: "hidden" });
    await page.evaluate(() => {
      const target = document.createElement("button");
      target.id = "inserted-tooltip";
      target.title = "新增提示";
      target.textContent = "新增按钮";
      target.setAttribute("aria-describedby", "existing-description");
      document.body.prepend(target);
    });
    const inserted = page.locator("#inserted-tooltip");
    await inserted.hover();
    await page.getByRole("tooltip").getByText("新增提示", { exact: true }).waitFor();
    for (const pointerType of ["touch", "pen"]) {
      await inserted.dispatchEvent("pointerdown", { pointerType });
      await inserted.focus();
      assert.equal(await page.getByRole("tooltip").count(), 0);
      assert.equal(await inserted.getAttribute("aria-describedby"), "existing-description");
      await inserted.evaluate((target) => target.blur());
    }
    await page.keyboard.press("Tab");
    await inserted.focus();
    await page.getByRole("tooltip").getByText("新增提示", { exact: true }).waitFor();
    await inserted.evaluate((target) => target.remove());
    await page.getByRole("tooltip").waitFor({ state: "hidden" });
    assert.deepEqual(errors, []);
    assert.equal(await page.locator("#errors").innerText(), "");
  } finally {
    await page.close();
  }
}

try {
  browser = await chromium.launch({ executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH || undefined });
  await verify();
  await verify({ mobile: true });
  console.log("PASS: desktop/mobile commit clears stale push tooltip; hovered tooltips update and clear");
  await verify({ upstream: "" });
  await verify({ upstream: "", unborn: true });
  console.log("PASS: a committed branch without upstream can push; unborn branch cannot push");
} finally {
  await browser?.close();
  await server.close();
}
