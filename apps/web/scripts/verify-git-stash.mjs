// 真实浏览器验证 Stash 的确认、写入、刷新及桌面/手机入口。
import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import { chromium } from "playwright";
import { createServer, transformWithEsbuild } from "vite";

const entry = `
import React from "react";
import { createRoot } from "react-dom/client";
import { GitWorkbench } from "/src/features/git/GitWorkbench.tsx";
import "/src/style.css";
import "/src/git.css";
const request = async (path, init) => {
  const response = await fetch(path, init);
  const data = await response.json();
  if (!response.ok) throw Object.assign(new Error(data.error), { status: response.status, code: data.code });
  return data;
};
function Harness() {
  const [workspace, setWorkspace] = React.useState({ projectID: "p", conversationId: "first" });
  const [channel, setChannel] = React.useState(0);
  const scopedRequest = React.useMemo(() => (path, init) => request(path, init), [channel]);
  window.switchWorkspace = setWorkspace;
  window.switchTransport = () => setChannel((value) => value + 1);
  return <GitWorkbench {...workspace} active
    mobile={new URLSearchParams(location.search).has("mobile")} request={scopedRequest}
    fail={(message) => { document.getElementById("errors").textContent = message; }} />;
}
createRoot(document.getElementById("root")).render(<Harness />);
`;
const server = await createServer({
  configFile: fileURLToPath(new URL("../vite.config.ts", import.meta.url)),
  plugins: [{
    name: "git-stash-browser-test",
    resolveId(id) { if (id === "/__git-stash-test.tsx") return "\0git-stash-test.tsx"; },
    async load(id) {
      if (id === "\0git-stash-test.tsx") return (await transformWithEsbuild(entry, "test.tsx", { loader: "tsx", jsx: "automatic" })).code;
    },
    configureServer(vite) {
      vite.middlewares.use((req, res, next) => {
        if (!req.url.startsWith("/__git-stash-test?")) return next();
        res.setHeader("Content-Type", "text/html; charset=utf-8");
        res.end('<html><head><style>html,body,#root{height:100%;margin:0}.git-workbench{height:100%}</style></head><body><div id="errors"></div><div id="root"></div><script type="module" src="/__git-stash-test.tsx"></script></body></html>');
      });
    },
  }],
  server: { host: "127.0.0.1", port: 0 },
});
await server.listen();
const base = `http://127.0.0.1:${server.httpServer.address().port}`;
let browser;

async function verify({ mobile = false, listFailure = false, restoreConflict = false, restoreWithoutIndex = false, staleState = false, unborn = false, merging = false, switchIdentity = "" } = {}) {
  const context = await browser.newContext({ viewport: mobile ? { width: 390, height: 844 } : { width: 1280, height: 900 } });
  const page = await context.newPage();
  const pageErrors = [];
  page.on("pageerror", (error) => pageErrors.push(error.message));
  let dirty = true;
  let stashes = [];
  const writes = [];
  await page.route("**/api/**", async (route) => {
    const req = route.request();
    const suffix = new URL(req.url()).pathname.split("/git/")[1];
    let data = [];
    let status = 200;
    if (req.method() === "POST") {
      const payload = req.postDataJSON();
      writes.push({ suffix, payload });
      assert.equal(payload.stateToken, dirty ? "dirty-token" : "clean-token");
      if (staleState) {
        await route.fulfill({ status: 409, contentType: "application/json", body: JSON.stringify({ error: "仓库状态已经变化，请重新确认", code: "stale_state" }) });
        return;
      }
      if (suffix === "stashes") {
        assert.equal(payload.message, "切换前保存");
        dirty = false;
        stashes = [{ oid: "a".repeat(40), ref: "stash@{0}", subject: "On main: 切换前保存", createdAt: "2026-10-26T08:00:00Z" }];
      } else if (suffix === "stashes/apply") {
        assert.equal(payload.oid, stashes[0].oid);
        assert.equal(payload.restoreIndex, !restoreWithoutIndex);
        dirty = true;
      } else {
        throw new Error(`unexpected write: ${suffix}`);
      }
      data = restoreConflict && suffix === "stashes/apply"
        ? { operationId: "test", status: "needs_attention", errorMessage: "Stash 恢复未完成，备份仍然保留" }
        : { operationId: "test", status: "succeeded" };
      status = 202;
    } else if (suffix === "summary") {
      const query = new URL(req.url());
      const branch = query.pathname.includes("/projects/q/") || query.searchParams.get("conversationId") === "second" ? "new-workspace" : "main";
      data = { repositoryState: "ready", stateToken: dirty ? "dirty-token" : "clean-token",
        head: { oid: unborn ? "(initial)" : "b".repeat(40), branch, detached: false, upstream: "", ahead: 0, behind: 0 },
        worktree: { staged: dirty ? 1 : 0, modified: 0, untracked: dirty ? 1 : 0, deleted: 0, renamed: 0, conflicted: 0 },
        changes: dirty ? [
          { path: "tracked.txt", staged: true, modified: false, untracked: false },
          { path: "new.txt", staged: false, modified: false, untracked: true },
        ] : [] };
    } else if (suffix === "stashes") {
      status = listFailure ? 503 : 200;
      data = listFailure ? { error: "list unavailable" } : stashes;
    } else if (suffix === "conflicts") {
      data = { context: { operationType: merging ? "merge" : "none", oursLabel: "main", canAbort: merging, canFinish: merging }, files: [] };
    } else if (suffix === "branches") {
      data = [{ name: "main", current: true, remote: false }, { name: "other", current: false, remote: false }];
    }
    await route.fulfill({ status, contentType: "application/json", body: JSON.stringify(data) });
  });
  try {
    await page.goto(`${base}/__git-stash-test?${mobile ? "mobile=1" : "desktop=1"}`);
    const panel = page.getByRole("region", { name: "收起的改动（Stash）", exact: true });
    await panel.getByRole("button", { name: "收起改动", exact: true }).waitFor();
    if (merging) {
      await page.getByRole("button", { name: "完成合并", exact: true }).waitFor();
      assert.equal(await panel.getByRole("button", { name: "收起改动", exact: true }).isDisabled(), true);
      await page.getByRole("tab", { name: "分支", exact: true }).click();
      await page.getByRole("button", { name: "切换", exact: true }).click();
      assert.equal(await page.getByRole("button", { name: "先收起改动", exact: true }).isDisabled(), true);
      assert.equal(writes.length, 0);
      return;
    }
    if (unborn) {
      const create = panel.getByRole("button", { name: "收起改动", exact: true });
      assert.equal(await create.isDisabled(), true);
      assert.equal(await create.getAttribute("title"), "创建首次提交后才能使用 Stash");
      return;
    }
    if (listFailure) {
      await panel.getByText("无法读取 Stash 列表", { exact: false }).waitFor();
      assert.equal(await panel.getByText("没有收起的改动", { exact: true }).count(), 0);
      return;
    }
    // 从脏工作区的切分支确认中，也能直接进入收起操作。
    await page.getByRole("tab", { name: "分支", exact: true }).click();
    await page.getByRole("button", { name: "切换", exact: true }).click();
    await page.getByRole("button", { name: "先收起改动", exact: true }).click();
    const dialog = page.getByRole("dialog", { name: "收起改动（Stash）", exact: true });
    await dialog.getByText("1 个受跟踪文件、1 个未跟踪文件", { exact: true }).waitFor();
    if (switchIdentity) {
      const nextSummary = page.waitForResponse((response) => response.url().includes("/git/summary"));
      await page.evaluate((kind) => kind === "transport" ? window.switchTransport() : window.switchWorkspace({ projectID: kind === "project" ? "q" : "p", conversationId: kind === "conversation" ? "second" : "first" }), switchIdentity);
      await nextSummary;
      if (switchIdentity !== "transport") await page.getByText("new-workspace", { exact: true }).first().waitFor();
      assert.equal(await page.getByRole("dialog").count(), 0, "a previous workspace confirmation must close");
      assert.equal(writes.length, 0);
      return;
    }
    await dialog.getByRole("textbox", { name: "说明（可选）" }).fill("切换前保存");
    await dialog.getByRole("button", { name: "确认收起", exact: true }).click();
    if (staleState) {
      await page.locator("#errors").getByText("仓库状态已经变化，请重新确认", { exact: true }).waitFor();
      await page.waitForFunction(() => document.querySelector(".git-confirmation-dialog .primary")?.disabled === false);
      assert.equal(writes.length, 1, "a stale write must not be replayed automatically");
      assert.equal(stashes.length, 0);
      assert.deepEqual(pageErrors, []);
      console.log("PASS: stale stash refreshes without replacing the original notice or replaying");
      return;
    }
    await dialog.waitFor({ state: "hidden" });
    await page.getByRole("tab", { name: "变更", exact: true }).click();
    const restore = panel.getByRole("button", { name: "恢复", exact: true });
    await restore.click();
    const restoreDialog = page.getByRole("dialog", { name: "恢复收起的改动", exact: true });
    await restoreDialog.getByText("恢复后保留 Stash 备份", { exact: true }).waitFor();
    const restoreIndex = restoreDialog.getByRole("checkbox", { name: "恢复原有暂存状态", exact: true });
    assert.equal(await restoreIndex.isChecked(), true);
    if (restoreWithoutIndex) await restoreIndex.uncheck();
    await restoreDialog.getByRole("button", { name: "确认恢复", exact: true }).click();
    await restoreDialog.waitFor({ state: "hidden" });
    await page.waitForFunction(() => document.querySelector(".git-stash-list button")?.disabled === true);
    assert.equal(stashes.length, 1);
    assert.deepEqual(writes.map((write) => write.suffix), ["stashes", "stashes/apply"]);
    if (restoreConflict) await page.locator("#errors").getByText("Stash 恢复未完成，备份仍然保留", { exact: true }).waitFor();
    assert.deepEqual(pageErrors, []);
    console.log(`PASS: ${mobile ? "mobile" : "desktop"}${restoreConflict ? " conflict" : " round trip"}${restoreWithoutIndex ? " without index" : ""}`);
  } finally {
    await context.close();
  }
}

try {
  browser = await chromium.launch({ executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH || undefined });
  await verify();
  await verify({ mobile: true });
  await verify({ restoreWithoutIndex: true });
  await verify({ mobile: true, restoreWithoutIndex: true });
  await verify({ staleState: true });
  await verify({ restoreConflict: true });
  await verify({ listFailure: true });
  console.log("PASS: unavailable stash list is explicit");
  await verify({ unborn: true });
  console.log("PASS: unborn HEAD cannot create a stash");
  await verify({ merging: true });
  console.log("PASS: resolved merge blocks stash until finished");
  await verify({ switchIdentity: "project" });
  await verify({ switchIdentity: "conversation" });
  await verify({ switchIdentity: "transport" });
  console.log("PASS: switching workspace closes old confirmations");
} finally {
  await browser?.close();
  await server.close();
}
