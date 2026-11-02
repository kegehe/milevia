// 真实浏览器验证「提交并推送」：分裂按钮、下拉菜单、两段串联的写操作与失败语义。
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
  const [workspace] = React.useState({ projectID: "p", conversationId: "first" });
  return <GitWorkbench {...workspace} active
    mobile={new URLSearchParams(location.search).has("mobile")} request={request}
    fail={(message) => { document.getElementById("errors").textContent = message; }} />;
}
createRoot(document.getElementById("root")).render(<Harness />);
`;

const server = await createServer({
  configFile: fileURLToPath(new URL("../vite.config.ts", import.meta.url)),
  plugins: [{
    name: "git-commit-push-browser-test",
    resolveId(id) { if (id === "/__git-commit-push-test.tsx") return "\0git-commit-push-test.tsx"; },
    async load(id) {
      if (id === "\0git-commit-push-test.tsx") return (await transformWithEsbuild(entry, "test.tsx", { loader: "tsx", jsx: "automatic" })).code;
    },
    configureServer(vite) {
      vite.middlewares.use((req, res, next) => {
        if (!req.url.startsWith("/__git-commit-push-test?")) return next();
        res.setHeader("Content-Type", "text/html; charset=utf-8");
        res.end('<html><head><style>html,body,#root{height:100%;margin:0}.git-workbench{height:100%}</style></head><body><div id="errors"></div><div id="root"></div><script type="module" src="/__git-commit-push-test.tsx"></script></body></html>');
      });
    },
  }],
  server: { host: "127.0.0.1", port: 0 },
});
await server.listen();
const base = `http://127.0.0.1:${server.httpServer.address().port}`;
let browser;

async function verify({ mobile = false, pushFailure = false, commitFailure = false, detached = false, noUpstream = false, needsAttention = false, transportFailure = false } = {}) {
  const context = await browser.newContext({ viewport: mobile ? { width: 390, height: 844 } : { width: 1280, height: 900 } });
  const page = await context.newPage();
  const pageErrors = [];
  page.on("pageerror", (error) => pageErrors.push(error.message));
  const writes = [];
  let committed = false;
  let pushed = false;
  let tokenVersion = 1;
  await page.route("**/api/**", async (route) => {
    const req = route.request();
    const suffix = new URL(req.url()).pathname.split("/git/")[1];
    let data = [];
    let status = 200;
    if (req.method() === "POST") {
      const payload = req.postDataJSON();
      writes.push({ suffix, payload });
      if (suffix === "commits") {
        assert.equal(payload.message, "改了 README");
        if (transportFailure) {
          // 传输层失败：连接直接断掉（不是服务端回绝）—— fetch 会抛 TypeError
          await route.abort();
          return;
        }
        if (commitFailure) {
          await route.fulfill({ status: 202, contentType: "application/json", body: JSON.stringify({ operationId: "op-1", status: "failed", errorMessage: "没有可提交的已暂存改动" }) });
          return;
        }
        if (needsAttention) {
          await route.fulfill({ status: 202, contentType: "application/json", body: JSON.stringify({ operationId: "op-1", status: "needs_attention", errorMessage: "Git 操作已执行，但无法读取最新仓库状态" }) });
          return;
        }
        committed = true;
        tokenVersion = 2;
      } else if (suffix === "push") {
        assert.equal(payload.branch, "main");
        assert.equal(payload.remote, "origin");
        if (pushFailure) {
          await route.fulfill({ status: 202, contentType: "application/json", body: JSON.stringify({ operationId: "op-2", status: "failed", errorMessage: "推送被拒绝，远端有本地没有的提交" }) });
          return;
        }
        pushed = true;
        tokenVersion = 3;
      } else {
        throw new Error(`unexpected write: ${suffix}`);
      }
      data = { operationId: "op", status: "succeeded" };
      status = 202;
    } else if (suffix === "summary") {
      data = {
        repositoryState: "ready",
        stateToken: `token-${tokenVersion}`,
        head: { oid: committed ? "c".repeat(40) : "b".repeat(40), branch: "main", detached, upstream: noUpstream ? "" : "origin/main", ahead: pushed ? 0 : (committed ? 1 : 0), behind: 0 },
        worktree: { staged: committed ? 0 : 1, modified: 0, untracked: 0, deleted: 0, renamed: 0, conflicted: 0 },
        changes: committed ? [] : [{ path: "README.md", staged: true, modified: false, untracked: false }],
      };
    } else if (suffix === "conflicts") {
      data = { context: { operationType: "none" }, files: [] };
    } else if (suffix === "branches") {
      data = [{ name: "main", current: true, remote: false }];
    } else if (suffix === "operations") {
      data = [];
    }
    await route.fulfill({ status, contentType: "application/json", body: JSON.stringify(data) });
  });

  try {
    await page.goto(`${base}/__git-commit-push-test?${mobile ? "mobile=1" : "desktop=1"}`);
    const submit = page.getByRole("button", { name: /^提交( \d+ 个已暂存文件)?$/ });
    await submit.waitFor();

    // ⚠️ 手机端**没有**这个下拉箭头（移动端 DOM 是定稿契约，docs/41 §6.2）。
    const caret = page.getByRole("button", { name: "提交与推送方式", exact: true });
    if (mobile) {
      // 手机端：等页面稳定后再数 —— Vite 首次预构建依赖时会重载一次页面，
      // 那一次量到的是空 DOM（偶发失败就是这么来的）。
      await submit.waitFor();
      await page.waitForTimeout(300);
      assert.equal(await caret.count(), 0, "the dropdown trigger must not exist on mobile");
      console.log("PASS: mobile keeps the original commit button, no dropdown trigger");
      return;
    }
    await caret.waitFor();

    await page.getByRole("textbox", { name: "提交信息" }).fill("改了 README");
    // 等 React 把重渲染跑完 —— 否则量到的是「信息还空着」那一份（提交键是灰的）。
    await page.waitForFunction(() => document.querySelector(".git-commit-submit")?.disabled === false);
    // ⚠️ 还要等背景色**过渡完**：.primary 有 160ms 的 background 过渡，
    // 刚变成可用的那一帧算出来还是禁用态的灰绿色。
    await page.waitForFunction(() => {
      const node = document.querySelector(".git-commit-submit");
      return node !== null && getComputedStyle(node).backgroundColor === "rgb(200, 232, 90)";
    });
    // ── 视觉与几何：分裂按钮必须真的拼成一颗胶囊 ──
    // 提交键与箭头：同高、紧贴、左右圆角互补。
    const submitBox = await submit.boundingBox();
    const caretBox = await caret.boundingBox();
    assert.ok(Math.abs(submitBox.y - caretBox.y) < 1, "the caret must align with the submit button vertically");
    assert.ok(Math.abs(submitBox.height - caretBox.height) < 1, "the caret must have the same height as the submit button");
    assert.ok(Math.abs(submitBox.x + submitBox.width - caretBox.x) < 1, "the caret must sit right against the submit button");
    // border-radius 的顺序是 上左 上右 下右 下左：
    // 提交键左端圆右端方（方的那边交给箭头），箭头左端方右端圆。
    const submitRadius = await submit.evaluate((node) => getComputedStyle(node).borderRadius);
    const caretRadius = await caret.evaluate((node) => getComputedStyle(node).borderRadius);
    assert.match(submitRadius, /999px 0px 0px 999px/, `submit right corners must be square, got ${submitRadius}`);
    assert.match(caretRadius, /0px 999px 999px 0px/, `caret right corners must be round, got ${caretRadius}`);
    // 配色一致（同一颗黄绿色胶囊，不是两个颜色）。
    const submitBg = await submit.evaluate((node) => getComputedStyle(node).backgroundColor);
    const caretBg = await caret.evaluate((node) => getComputedStyle(node).backgroundColor);
    assert.equal(caretBg, submitBg, `the caret must match the submit button color (submit ${submitBg} vs caret ${caretBg})`);

    // 打开下拉菜单
    await caret.click();
    const menu = page.getByRole("menu", { name: "提交与推送方式", exact: true });
    await menu.waitFor();
    const item = page.getByRole("menuitem", { name: /提交并推送/ });
    await item.waitFor();
    assert.match(await item.textContent(), /提交并推送到 origin\/main/);
    // 弹层挂在 body 上（提交面板在 overflow:auto 的侧栏里，绝对定位会被裁掉）
    assert.equal(await page.evaluate(() => document.querySelector(".git-commit-menu")?.parentElement?.tagName), "BODY");
    // 菜单：整个落在视口内、不被侧栏裁掉。
    const menuBox = await menu.boundingBox();
    assert.ok(menuBox.width >= 200 && menuBox.width <= 320, `menu width ${menuBox.width} is off`);
    assert.ok(menuBox.x >= 0 && menuBox.y >= 0, "the menu must be fully inside the viewport");
    assert.ok(menuBox.y + menuBox.height <= 900, "the menu must not overflow the bottom of the viewport");

    if (detached) {
      // 游离指针推不了 —— 菜单项灰掉并说明原因
      assert.equal(await item.isDisabled(), true);
      assert.match(await item.textContent(), /游离指针状态无法推送/);
      console.log("PASS: detached HEAD disables the push entry with a reason");
      return;
    }

    await item.click();
    const dialog = page.getByRole("dialog", { name: "提交并推送", exact: true });
    await dialog.waitFor();
    await dialog.getByText("先创建提交，再推送，两步合成一次操作", { exact: true }).waitFor();
    await dialog.getByRole("button", { name: "确认提交并推送", exact: true }).click();

    if (commitFailure) {
      // 提交段失败：确认框**留着**（用户改好信息就能重试），且说清"什么都没发生"
      await dialog.getByText(/本地没有产生任何提交，可以直接重试/).waitFor();
      assert.equal(await dialog.getByRole("button", { name: "确认提交并推送", exact: true }).isDisabled(), false);
      assert.deepEqual(writes.map((write) => write.suffix), ["commits"]);
      console.log("PASS: a failed commit keeps the dialog open and says nothing was created");
      return;
    }
    if (needsAttention) {
      // 结果不确定：不能说"什么都没发生"，也不能让人就地重试（可能已经提交了）
      await page.getByText(/无法确认/).first().waitFor();
      assert.equal(await page.getByRole("dialog").count(), 0, "an unconfirmed commit must not leave the dialog open for a blind retry");
      console.log("PASS: an unconfirmed commit says so and does not invite a blind retry");
      return;
    }
    if (transportFailure) {
      // 传输层失败：服务端**可能**已经把提交建出来了 —— 不能说"什么都没发生"，
      // 确认框也必须收掉（留着它等于把"再提交一次"的按钮摆在用户面前）。
      await page.getByText(/提交是否落地无法确认/).waitFor();
      assert.equal(await page.getByRole("dialog").count(), 0, "a transport failure must close the dialog so the commit is not made twice");
      assert.deepEqual(writes.map((write) => write.suffix), ["commits"]);
      console.log("PASS: a transport failure says the outcome is unknown and closes the dialog");
      return;
    }
    if (pushFailure) {
      // 推送段失败：提交已在本地 —— 确认框必须收掉（否则再点一次就多一个提交）
      await page.getByText(/提交已在本地创建，不要重复提交/).waitFor();
      assert.equal(await page.getByRole("dialog").count(), 0, "a failed push must close the dialog so the commit is not made twice");
      assert.deepEqual(writes.map((write) => write.suffix), ["commits", "push"]);
      console.log("PASS: a failed push says the commit exists and closes the dialog");
      return;
    }

    // 成功：两段都跑过，第二个用**对账后重新签发**的令牌
    await page.getByText(/已提交，已推送/).waitFor();
    assert.deepEqual(writes.map((write) => write.suffix), ["commits", "push"]);
    assert.equal(writes[0].payload.stateToken, "token-1");
    assert.equal(writes[1].payload.stateToken, "token-2", "the push must use the token issued after the commit");
    assert.deepEqual(pageErrors, []);
    console.log("PASS: commit and push run in sequence with a relayed token");
  } finally {
    await context.close();
  }
}

try {
  browser = await chromium.launch({ executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH || undefined });
  await verify();
  await verify({ mobile: true });
  await verify({ commitFailure: true });
  await verify({ needsAttention: true });
  await verify({ transportFailure: true });
  await verify({ pushFailure: true });
  await verify({ detached: true });
  await verify({ noUpstream: true });
  console.log("PASS: first push establishes the upstream");
} finally {
  await browser?.close();
  await server.close();
}

