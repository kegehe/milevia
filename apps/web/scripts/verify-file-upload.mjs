// 浏览器验证文件选择、系统拖放、目标目录、同名保护和工作区切换。
import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import { chromium } from "playwright";
import { createServer, transformWithEsbuild } from "vite";

const entry = `
import React from "react";
import { createRoot } from "react-dom/client";
import { FilesPanel } from "/src/features/files/FilesPanel.tsx";
import "/src/files.css";
const request = async (path, init) => {
  const response = await fetch(path, init);
  const data = await response.json();
  if (!response.ok) throw Object.assign(new Error(data.error), { status: response.status });
  return data;
};
const registerNavigationGuard = () => {};
function Harness() {
  const [occupied, setOccupied] = React.useState(false);
  const [conversationId, setConversationId] = React.useState("first");
  const [projectId, setProjectId] = React.useState("p");
  const [channel, setChannel] = React.useState(0);
  const scopedRequest = React.useMemo(() => (path, init) => request(path, init), [channel]);
  window.setOccupied = setOccupied;
  window.setConversation = setConversationId;
  window.setProject = setProjectId;
  window.switchTransport = () => setChannel((value) => value + 1);
  return <FilesPanel projectId={projectId} conversationId={conversationId} runner="local" request={scopedRequest}
    mobile={new URLSearchParams(location.search).has("mobile")}
    isWorkspaceOccupied={occupied} registerNavigationGuard={registerNavigationGuard} />;
}
createRoot(document.getElementById("root")).render(<Harness />);
`;
const server = await createServer({
  configFile: fileURLToPath(new URL("../vite.config.ts", import.meta.url)),
  plugins: [{
    name: "file-upload-browser-test",
    resolveId(id) { if (id === "/__file-upload-test.tsx") return "\0file-upload-test.tsx"; },
    async load(id) {
      if (id === "\0file-upload-test.tsx") return (await transformWithEsbuild(entry, "test.tsx", { loader: "tsx", jsx: "automatic" })).code;
    },
    configureServer(vite) {
      vite.middlewares.use((req, res, next) => {
        if (!req.url.startsWith("/__file-upload-test?")) return next();
        res.setHeader("Content-Type", "text/html; charset=utf-8");
        res.end('<html><head><style>html,body,#root{height:100%;margin:0}#root{display:flex}.files-panel{height:100%}</style></head><body><div id="root"></div><script type="module" src="/__file-upload-test.tsx"></script></body></html>');
      });
    },
  }],
  server: { host: "127.0.0.1", port: 0 },
});
await server.listen();
const base = `http://127.0.0.1:${server.httpServer.address().port}`;
let browser;

async function verify(mobile) {
  const context = await browser.newContext({ viewport: mobile ? { width: 390, height: 844 } : { width: 1280, height: 800 } });
  const page = await context.newPage();
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  const files = new Map();
  const writes = [];
  const trees = [];
  await page.route("**/api/**", async (route) => {
    const req = route.request();
    const url = new URL(req.url());
    let status = 200;
    let data;
    if (req.method() === "PUT") {
      const body = req.postDataJSON();
      writes.push({ ...body, conversationId: url.searchParams.get("conversationId") });
      assert.equal(body.encoding, "base64");
      assert.equal(body.createOnly, true);
      if (files.has(body.path)) { status = 409; data = { error: "文件已存在" }; }
      else { files.set(body.path, Buffer.from(body.content, "base64")); data = { status: "ok", version: "v1" }; }
    } else if (url.pathname.endsWith("/tree")) {
      const dir = url.searchParams.get("path") || "";
      trees.push(dir);
      data = { entries: [
        ...(dir === "" ? [{ name: "assets", path: "assets", isDir: true }] : []),
        ...[...files].filter(([path]) => (path.includes("/") ? path.slice(0, path.lastIndexOf("/")) : "") === dir)
          .map(([path, bytes]) => ({ name: path.split("/").pop(), path, size: bytes.length, isDir: false })),
      ] };
    } else throw new Error(`Unexpected API: ${url}`);
    await route.fulfill({ status, contentType: "application/json", body: JSON.stringify(data) });
  });
  const payload = (name, buffer) => ({ name, buffer, mimeType: "application/octet-stream" });
  const expectSummary = (text) => page.locator(".files-upload-summary").getByText(text, { exact: true }).waitFor();
  const selectFiles = async (files) => {
    const chooserPromise = page.waitForEvent("filechooser");
    await page.getByRole("button", { name: "上传文件", exact: true }).click();
    await (await chooserPromise).setFiles(files);
  };
  try {
    await page.goto(`${base}/__file-upload-test?${mobile ? "mobile=1" : "desktop=1"}`);
    await page.getByRole("button", { name: "上传文件", exact: true }).waitFor();
    const image = Buffer.from([137, 80, 78, 71, 0, 255, 254]);
    const pdf = Buffer.from("%PDF-1.7\n\x00\xff", "binary");
    const chooserPromise = page.waitForEvent("filechooser");
    await page.getByRole("button", { name: "上传文件", exact: true }).click();
    await (await chooserPromise).setFiles([payload("图片.png", image), payload("report.pdf", pdf)]);
    await expectSummary("已上传 2 个文件");
    assert.deepEqual(files.get("图片.png"), image);
    assert.deepEqual(files.get("report.pdf"), pdf);
    await page.locator(".file-tree-item-name").getByText("report.pdf", { exact: true }).waitFor();
    assert.ok(writes.every((write) => write.conversationId === "first"));

    await selectFiles([payload("图片.png", Buffer.from("overwrite")), payload("empty.bin", Buffer.alloc(0))]);
    await expectSummary("已上传 1 个文件，1 个失败");
    await page.getByText("图片.png：文件已存在", { exact: true }).waitFor();
    assert.deepEqual(files.get("图片.png"), image);
    assert.equal(files.get("empty.bin").length, 0);

    const assets = page.locator(".file-tree-item", { hasText: "assets" });
    await assets.click({ button: "right" });
    const dirChooserPromise = page.waitForEvent("filechooser");
    await page.getByRole("button", { name: "上传文件到此目录", exact: true }).click();
    await (await dirChooserPromise).setFiles(payload("nested.pdf", pdf));
    await expectSummary("已上传 1 个文件");
    assert.deepEqual(files.get("assets/nested.pdf"), pdf);
    assert.equal(trees.at(-1), "assets");

    const transfer = await page.evaluateHandle(() => {
      const data = new DataTransfer();
      data.items.add(new File([new Uint8Array([0, 255, 1, 128])], "drop.bin"));
      return data;
    });
    await assets.dispatchEvent("dragover", { dataTransfer: transfer });
    await page.getByText("松开以上传到 assets", { exact: true }).waitFor();
    await assets.dispatchEvent("drop", { dataTransfer: transfer });
    await expectSummary("已上传 1 个文件");
    assert.deepEqual(files.get("assets/drop.bin"), Buffer.from([0, 255, 1, 128]));
    assert.equal(await page.locator(".files-drop-hint").count(), 0);

    const rootTransfer = await page.evaluateHandle(() => {
      const data = new DataTransfer();
      data.items.add(new File(["root"], "root.txt"));
      return data;
    });
    await page.locator(".files-panel").dispatchEvent("drop", { dataTransfer: rootTransfer });
    await expectSummary("已上传 1 个文件");
    assert.equal(files.get("root.txt").toString(), "root");

    const arrowTransfer = await page.evaluateHandle(() => {
      const data = new DataTransfer();
      data.items.add(new File(["arrow"], "arrow.bin"));
      return data;
    });
    const folderArrow = assets.locator("xpath=ancestor::li[1]").locator(".file-tree-arrow").first();
    await folderArrow.dispatchEvent("dragover", { dataTransfer: arrowTransfer });
    await page.getByText("松开以上传到 assets", { exact: true }).waitFor();
    const arrowResponse = page.waitForResponse((response) => response.request().method() === "PUT");
    await folderArrow.dispatchEvent("drop", { dataTransfer: arrowTransfer });
    await arrowResponse;
    assert.equal(writes.at(-1).path, "assets/arrow.bin", "目录行的展开箭头也应使用该目录作为上传目标");

    const paddingTransfer = await page.evaluateHandle(() => {
      const data = new DataTransfer();
      data.items.add(new File(["padding"], "padding.bin"));
      return data;
    });
    const folderRow = assets.locator("xpath=ancestor::*[@data-rct-item-container][1]");
    const paddingResponse = page.waitForResponse((response) => response.request().method() === "PUT");
    await folderRow.dispatchEvent("drop", { dataTransfer: paddingTransfer });
    await paddingResponse;
    assert.equal(writes.at(-1).path, "assets/padding.bin", "目录行留白也应使用该目录作为上传目标");

    // 菜单中的目录也来自打开菜单时的工作区，不能跨切换继续使用。
    for (const change of ["conversation", "project", "transport"]) {
      await assets.click({ button: "right" });
      await page.getByRole("button", { name: "上传文件到此目录", exact: true }).waitFor();
      await page.evaluate((change) => {
        if (change === "conversation") window.setConversation("menu-switched");
        if (change === "project") window.setProject("menu-project");
        if (change === "transport") window.switchTransport();
      }, change);
      await page.getByRole("button", { name: "上传文件到此目录", exact: true }).waitFor({ state: "hidden", timeout: 3000 });
      assert.equal(await page.getByRole("button", { name: "上传文件到此目录", exact: true }).count(), 0,
        `跨 ${change} 切换后不能保留旧目录的上传菜单`);
    }
    await page.evaluate(() => window.setProject("p"));
    await page.evaluate(() => window.setConversation("first"));

    for (const change of ["conversation", "project", "transport"]) {
      const pendingChooser = page.waitForEvent("filechooser");
      await page.getByRole("button", { name: "上传文件", exact: true }).click();
      const oldChooser = await pendingChooser;
      const beforePickerSwitch = writes.length;
      await page.evaluate((change) => {
        if (change === "conversation") window.setConversation("picker-switched");
        if (change === "project") window.setProject("q");
        if (change === "transport") window.switchTransport();
      }, change);
      await page.locator(".files-upload-status").waitFor({ state: "hidden" });
      const unexpectedWrite = page.waitForRequest((req) => req.method() === "PUT", { timeout: 750 }).catch(() => null);
      await oldChooser.setFiles(payload(`old-picker-${change}.bin`, image));
      assert.equal((await unexpectedWrite)?.method(), undefined, `旧选择器跨 ${change} 切换后不得写入`);
      assert.equal(writes.length, beforePickerSwitch);
    }
    await page.evaluate(() => window.setProject("p"));
    await page.evaluate(() => window.setConversation("first"));

    await page.evaluate(() => window.setOccupied(true));
    await page.getByRole("button", { name: "上传文件", exact: true }).waitFor({ state: "hidden" });
    const beforeLocked = writes.length;
    await page.locator(".files-panel").dispatchEvent("drop", { dataTransfer: rootTransfer });
    await page.getByText("工作区正被占用，暂时无法上传文件", { exact: true }).waitFor();
    assert.equal(writes.length, beforeLocked);
    await page.evaluate(() => window.setOccupied(false));
    await page.getByRole("button", { name: "上传文件", exact: true }).waitFor();

    if (mobile) {
      const beforeLarge = writes.length;
      await selectFiles(payload("large.pdf", Buffer.alloc(256 * 1024 + 1)));
      await expectSummary("已上传 0 个文件，1 个失败");
      await page.getByText("large.pdf：手机端单个文件最大支持 256 KiB，请在电脑端上传较大文件", { exact: true }).waitFor();
      assert.equal(writes.length, beforeLarge);
    }

    await page.evaluate(() => {
      const original = File.prototype.arrayBuffer;
      File.prototype.arrayBuffer = async function () {
        await new Promise((resolve) => { window.releaseFileRead = resolve; });
        return original.call(this);
      };
      window.restoreFileRead = () => { File.prototype.arrayBuffer = original; };
    });
    await selectFiles([payload("stale.bin", image), payload("never.bin", pdf)]);
    await page.getByText("正在上传文件（0/2）…", { exact: true }).waitFor();
    assert.equal(await page.getByRole("button", { name: "上传文件", exact: true }).isDisabled(), true);
    const beforeSwitch = writes.length;
    await page.evaluate(() => window.setConversation("second"));
    await page.locator(".files-upload-status").waitFor({ state: "hidden" });
    await page.evaluate(() => { window.restoreFileRead(); window.releaseFileRead(); });
    await selectFiles(payload("fresh.bin", image));
    await expectSummary("已上传 1 个文件");
    assert.equal(writes.length, beforeSwitch + 1);
    assert.equal(writes.at(-1).conversationId, "second");
    assert.equal(files.has("stale.bin"), false);
    assert.deepEqual(errors, []);
    console.log(`PASS: ${mobile ? "mobile" : "desktop"} picker, binary bytes, conflicts, directory, row/arrow drops, lock, workspace switch, stale pickers`);
  } finally {
    await context.close();
  }
}

try {
  browser = await chromium.launch({ executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH || undefined });
  await verify(false);
  await verify(true);
} finally {
  await browser?.close();
  await server.close();
}
