// 先运行 pnpm --filter @milevia/web build；验证正式构建中的右键拦截及真实 xterm 行为。
import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import { chromium } from "playwright";
import { preview } from "vite";

const server = await preview({
  configFile: fileURLToPath(new URL("../vite.config.ts", import.meta.url)),
  preview: { host: "127.0.0.1", port: 0, open: false },
});
const base = `http://127.0.0.1:${server.httpServer.address().port}`;
let browser;
let testPage;
const debugRequests = [];
const errors = [];
try {
  browser = await chromium.launch({ channel: process.env.PLAYWRIGHT_CHANNEL || undefined });
  const context = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  await context.addInitScript(() => {
    if (!new URLSearchParams(location.search).has("web")) {
      window.__MILEVIA_DESKTOP_RUNTIME__ = { apiBase: location.origin, wsBase: location.origin.replace("http", "ws"), sessionToken: "test" };
    }
    window.testClipboard = "echo pasted\n";
    window.testInvocations = [];
    window.__TAURI_INTERNALS__ = {
      transformCallback: () => 1,
      invoke: async (command, args) => {
        window.testInvocations.push({ command, args });
        if (command === "get_updater_status") return { status: "idle" };
        if (command === "open_terminal_directory" && window.testDirectoryError) throw "目录不可访问";
        return 1;
      },
    };
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: {
      writeText: async (text) => {
        if (window.testWriteError) throw new Error("write denied");
        window.testClipboard = text;
      },
      readText: async () => {
        if (window.testClipboardError) throw new Error("denied");
        if (window.testDeferPaste) return new Promise((resolve, reject) => { window.testResolvePaste = resolve; window.testRejectPaste = reject; });
        return window.testClipboard;
      },
    } });
  });
  const page = await context.newPage();
  testPage = page;
  page.on("request", (req) => { if (req.url().includes("/api/")) debugRequests.push(new URL(req.url()).pathname + new URL(req.url()).search); });
  page.on("pageerror", (error) => errors.push(error.message));
  const project = { id: "p1", name: "菜单验证", pathDisplay: "project", fullPath: "D:\\project", runner: "windows-local", environment: "windows", localRunner: true, gitBranch: "main", claudeReady: true, codexReady: true, agentReady: true };
  const conversation = { id: "c1", projectId: "p1", status: "idle", agentId: "claude-code", title: "菜单验证", isCurrent: true, executionPolicy: "full_control", lastActivityAt: new Date().toISOString() };
  const content = "消息选区测试\n\n```js\nconsole.log('菜单');\n```";
  const readme = { name: "README.md", path: "README.md", isDir: false, isText: true, mimeType: "text/markdown", size: content.length, modTime: new Date().toISOString(), mode: "644" };
  let sessions = ["t1", "t2"].map((id) => ({ id, projectId: "p1", environment: "windows", cwdDisplay: "D:\\worktrees\\session & test", status: "running" }));
  const json = (route, data) => route.fulfill({ contentType: "application/json", body: JSON.stringify(data) });
  await page.route("**/api/**", (route) => {
    const url = new URL(route.request().url());
    const path = url.pathname;
    if (path === "/api/projects/p1/terminal/sessions") return json(route, { sessions, maxPerProject: 8, maxProjects: 8 });
    if (path === "/api/projects/p1") return json(route, project);
    if (path === "/api/projects") return json(route, [project]);
    if (path.endsWith("/fs/tree")) return json(route, { entries: [readme] });
    if (path.endsWith("/fs/stat")) return json(route, readme);
    if (path.endsWith("/fs/read")) return json(route, { content, version: "v1", stat: readme });
    if (path.endsWith("/mcp/status")) return json(route, { servers: [] });
    if (path.endsWith("/git/summary")) return json(route, { repositoryState: "ready", changes: [], worktree: {}, head: { oid: "abcdef1234567", branch: "main", detached: false, ahead: 0, behind: 0 } });
    if (path.endsWith("/conversations/activity")) return json(route, { conversations: [], missingConversationIds: [] });
    if (path === "/api/projects/p1/conversations") return json(route, { items: [conversation], nextCursor: "" });
    if (path === "/api/conversations/c1/activate") return json(route, conversation);
    if (path === "/api/conversations/c1") return json(route, url.searchParams.get("limit") === "1"
      ? { conversation, activeRunId: null }
      : { messages: [{ id: "m1", role: "assistant", content, createdAt: new Date().toISOString() }], events: [], activeRunId: null, hasMore: false, hasMoreMessages: false, nextCursor: "" });
    if (path.endsWith("/models")) return json(route, { models: [], customAllowed: true });
    if (path.endsWith("/usage")) return json(route, { available: false, context: {}, session: { taskCount: 0 }, models: null });
    if (path.endsWith("/commands")) return json(route, { commands: [], customAllowed: true });
    return json(route, []);
  });
  const input = [];
  const sockets = new Map();
  await page.routeWebSocket("**/ws/**", (socket) => {
    const id = new URL(socket.url()).pathname.split("/").at(-1);
    sockets.set(id, socket);
    socket.onMessage((data) => {
      if (typeof data !== "string") { input.push({ id, text: Buffer.from(data).subarray(8).toString() }); return; }
      if (JSON.parse(data).type !== "attach") return;
      socket.send(JSON.stringify({ type: "ready", status: "running" }));
      const frame = Buffer.alloc(8);
      frame.writeBigUInt64LE(1n);
      socket.send(Buffer.concat([frame, Buffer.from("terminal selection\r\nready> ")]));
    });
  });
  const menu = page.getByRole("menu", { name: "终端操作", exact: true });
  const action = (name) => menu.getByRole("menuitem", { name, exact: true });
  const rightClickTerminal = (options = {}) => page.locator(".xterm-screen").click({ button: "right", position: { x: 80, y: 20 }, ...options });
  // 裸右键是"粘贴"；Shift+右键才打开操作菜单，凡是访问菜单的用例都走这里。
  const openTerminalMenu = () => rightClickTerminal({ modifiers: ["Shift"] });
  const waitFor = async (predicate, message, timeout = 5000) => {
    const deadline = Date.now() + timeout;
    while (Date.now() < deadline) {
      if (await predicate()) return;
      await new Promise((resolve) => setTimeout(resolve, 50));
    }
    throw new Error(message);
  };
  await page.goto(`${base}/projects/p1/terminal`);
  await page.locator(".terminal-status.running").waitFor();
  // 拖拽选中即复制：松开鼠标后剪贴板里应出现选区文本。
  const screenBox = await page.locator(".xterm-screen").boundingBox();
  assert.ok(screenBox, "终端画布应已渲染");
  await page.mouse.move(screenBox.x + 2, screenBox.y + 6);
  await page.mouse.down();
  await page.mouse.move(screenBox.x + 150, screenBox.y + 6, { steps: 6 });
  await page.mouse.up();
  await waitFor(() => page.evaluate(() => window.testClipboard.includes("terminal")), "拖拽选中后应自动写入剪贴板");
  const copiedByDrag = await page.evaluate(() => window.testClipboard);
  assert.ok("terminal selection".startsWith(copiedByDrag) && copiedByDrag.length >= 8,
    `自动复制的内容应来自首行选区，实际为 ${JSON.stringify(copiedByDrag)}`);
  // 双击选词、三击选行同样属于选中，松手后应自动复制。
  await page.evaluate(() => { window.testClipboard = "before double click"; });
  await page.mouse.click(screenBox.x + 20, screenBox.y + 6, { clickCount: 2 });
  await waitFor(() => page.evaluate(() => window.testClipboard !== "before double click"), "双击选词后应自动复制");
  assert.equal(await page.evaluate(() => window.testClipboard), "terminal", "双击应复制光标下的单词");
  await page.evaluate(() => { window.testClipboard = "before triple click"; });
  await page.mouse.click(screenBox.x + 20, screenBox.y + 6, { clickCount: 3 });
  await waitFor(() => page.evaluate(() => window.testClipboard !== "before triple click"), "三击选行后应自动复制");
  assert.equal(await page.evaluate(() => window.testClipboard), "terminal selection", "三击应复制整行");
  // 拖出终端后再松手也要复制（mouseup 挂在 document 上的原因）。
  await page.evaluate(() => { window.testClipboard = "before outside release"; });
  await page.mouse.move(screenBox.x + 2, screenBox.y + 6);
  await page.mouse.down();
  await page.mouse.move(screenBox.x + 60, screenBox.y + 26, { steps: 4 });
  const metaBox = await page.locator(".terminal-meta").boundingBox();
  await page.mouse.move(metaBox.x + 40, metaBox.y + metaBox.height / 2, { steps: 6 });
  await page.mouse.up();
  await waitFor(() => page.evaluate(() => window.testClipboard !== "before outside release"), "拖出终端范围后松手也应复制");
  // 只点一下不产生选区时不能覆盖剪贴板。
  await page.evaluate(() => { window.testClipboard = "keep me"; });
  await page.locator(".xterm-screen").click({ position: { x: 200, y: 20 } });
  await page.waitForTimeout(120);
  assert.equal(await page.evaluate(() => window.testClipboard), "keep me", "单击没有选区时不应覆盖剪贴板");
  // 起点落在滚动条所在的 .xterm-viewport 上时不算选区操作，不应复制。
  await page.evaluate(() => { window.testClipboard = "before scrollbar"; });
  const viewportBox = await page.locator(".xterm-viewport").boundingBox();
  await page.mouse.click(viewportBox.x + viewportBox.width - 4, viewportBox.y + 20);
  await page.waitForTimeout(120);
  assert.equal(await page.evaluate(() => window.testClipboard), "before scrollbar", "点在滚动条上不应复制选区");
  // 裸右键把剪贴板内容直接粘贴进终端，且不弹菜单。
  const beforeRightClickPaste = input.length;
  await page.evaluate(() => { window.testClipboard = "echo right-click\r"; });
  await rightClickTerminal();
  await waitFor(() => input.length > beforeRightClickPaste, "右键应把剪贴板内容粘贴进终端");
  assert.ok(input.some((frame) => frame.id === "t1" && frame.text.includes("echo right-click")));
  assert.equal(await page.getByRole("menu", { name: "终端操作", exact: true }).count(), 0, "右键粘贴不应弹出操作菜单");
  // 键盘菜单键 / 触屏长按没有右键 mousedown：应当打开菜单，而不是把剪贴板内容送进会话。
  // 无头环境不会为 ContextMenu 键合成 contextmenu，这里直接派发等价事件。
  await page.evaluate(() => { window.testClipboard = "must not paste"; });
  const beforeKeyboardMenu = input.length;
  await page.evaluate(() => {
    document.querySelector(".xterm-screen").dispatchEvent(new MouseEvent("contextmenu", { bubbles: true, cancelable: true, clientX: 240, clientY: 80 }));
  });
  await waitFor(() => menu.isVisible(), "没有右键按下的 contextmenu 应打开终端操作菜单");
  assert.equal(input.length, beforeKeyboardMenu, "没有右键按下的 contextmenu 不能触发粘贴");
  await page.keyboard.press("Escape");
  await menu.waitFor({ state: "hidden" });
  await openTerminalMenu();
  const streamedFrame = Buffer.alloc(8);
  streamedFrame.writeBigUInt64LE(2n);
  sockets.get("t1").send(Buffer.concat([streamedFrame, Buffer.from(`\r\n${Array.from({ length: 100 }, (_, index) => `stream output ${index}\r\n`).join("")}`)]));
  await page.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent.includes("stream output 99"));
  await page.evaluate(async () => { await new Promise(requestAnimationFrame); await new Promise(requestAnimationFrame); });
  assert.equal(await menu.isVisible(), true, "终端持续输出引起的自动滚动不应关闭右键菜单");
  assert.equal(await action("复制").isDisabled(), true);
  const beforeMenuKeys = input.length;
  await page.keyboard.press("Home");
  assert.equal(await page.evaluate(() => document.activeElement.textContent), "粘贴", "键盘导航应跳过禁用项");
  await page.keyboard.press("ArrowUp");
  assert.equal(await page.evaluate(() => document.activeElement.textContent), "打开所在目录");
  await page.keyboard.press("ArrowDown");
  assert.equal(await page.evaluate(() => document.activeElement.textContent), "粘贴");
  assert.equal(input.length, beforeMenuKeys, "菜单键盘操作不能发送到终端进程");
  await action("全选").click();
  await openTerminalMenu();
  assert.equal(await action("复制").isEnabled(), true);
  await action("复制").click();
  await page.waitForFunction(() => window.testClipboard.includes("terminal selection"));
  await page.evaluate(() => {
    window.testWriteError = true;
    document.execCommand = (command) => {
      if (command !== "copy") return false;
      window.testClipboard = document.activeElement.value;
      window.testLegacyCopied = true;
      return true;
    };
  });
  await openTerminalMenu();
  await action("复制").click();
  await page.waitForFunction(() => window.testLegacyCopied);
  assert.equal(await page.evaluate(() => document.activeElement?.classList.contains("xterm-helper-textarea")), true, "兼容复制后应保留终端焦点");
  await page.evaluate(() => { window.testWriteError = false; });
  await openTerminalMenu();
  await action("粘贴").click();
  await page.waitForFunction(() => document.activeElement?.classList.contains("xterm-helper-textarea"));
  assert.ok(input.some((frame) => frame.id === "t1" && frame.text.includes("terminal selection")));

  await openTerminalMenu();
  await action("打开所在目录").click();
  assert.deepEqual(await page.evaluate(() => window.testInvocations.find((item) => item.command === "open_terminal_directory").args), { path: sessions[0].cwdDisplay });
  await page.evaluate(() => { window.testDirectoryError = true; });
  await openTerminalMenu();
  await action("打开所在目录").click();
  await page.getByRole("alert").filter({ hasText: "目录不可访问" }).waitFor();
  await page.evaluate(() => { window.testClipboardError = true; });
  await openTerminalMenu();
  await action("粘贴").click();
  await page.getByRole("alert").filter({ hasText: "无法读取剪贴板" }).waitFor();

  // 读取剪贴板期间切换终端，内容必须丢弃，不能落到旧会话或新会话。
  await page.evaluate(() => { window.testClipboardError = false; window.testDeferPaste = true; });
  await openTerminalMenu();
  await action("粘贴").click();
  await page.waitForFunction(() => !!window.testResolvePaste);
  const beforeSwitch = input.length;
  await page.locator('[data-terminal-tab="t2"]').click();
  await page.locator(".terminal-status.running").waitFor();
  await page.evaluate(() => window.testResolvePaste("must not send"));
  assert.equal(input.length, beforeSwitch);
  // 异步失败也属于发起读取的会话，不能把旧会话的报错写入新会话。
  await openTerminalMenu();
  await action("粘贴").click();
  await page.locator('[data-terminal-tab="t1"]').click();
  await page.locator(".terminal-status.running").waitFor();
  await page.evaluate(async () => {
    window.testRejectPaste(new Error("late clipboard denial"));
    await new Promise(requestAnimationFrame);
    await new Promise(requestAnimationFrame);
  });
  assert.equal(await page.getByRole("alert").count(), 0, "旧会话的剪贴板失败不应污染新会话");
  await openTerminalMenu();
  await action("粘贴").click();
  const beforeExit = input.length;
  sockets.get("t1").send(JSON.stringify({ type: "exit", code: 0 }));
  await page.locator(".terminal-status.exited").waitFor();
  await page.evaluate(async () => {
    window.testResolvePaste("must not send after exit");
    await new Promise(requestAnimationFrame); await new Promise(requestAnimationFrame);
  });
  assert.equal(input.length, beforeExit, "读取剪贴板期间终端退出后不能继续发送粘贴内容");
  // 会话已退出、无法写入时，裸右键退回操作菜单（而不是无声无息）。
  await rightClickTerminal();
  assert.equal(await action("粘贴").isDisabled(), true);
  await page.keyboard.press("Escape");
  await menu.waitFor({ state: "hidden" });
  assert.equal(await page.evaluate(() => document.activeElement?.classList.contains("xterm-helper-textarea")), true, "Escape 关闭菜单后应恢复终端焦点");

  sessions = sessions.map((session) => ({ ...session, environment: "remote-linux", cwdDisplay: "/srv/project" }));
  await page.reload();
  await page.locator(".terminal-status.running").waitFor();
  await openTerminalMenu();
  assert.equal(await action("打开所在目录").isDisabled(), true);
  sessions[0] = { ...sessions[0], environment: "wsl", cwdDisplay: "\\\\wsl.localhost\\Ubuntu\\home\\dev\\workspace" };
  await page.reload();
  await page.locator(".terminal-status.running").waitFor();
  await openTerminalMenu();
  await action("打开所在目录").click();
  assert.deepEqual(await page.evaluate(() => window.testInvocations.find((item) => item.command === "open_terminal_directory").args), { path: sessions[0].cwdDisplay });

  // 鼠标上报模式（vim / htop 等）：普通点击不改变选区，不能把旧选区反复写进剪贴板。
  await page.evaluate(() => { window.testClipboard = "sentinel"; });
  // 控制端下发的是「8 字节序号 + 负载」，客户端会剥掉序号再写进终端。
  const mouseModeFrame = Buffer.alloc(8);
  mouseModeFrame.writeBigUInt64LE(3n);
  sockets.get("t1").send(Buffer.concat([mouseModeFrame, Buffer.from("\x1b[?1003h")]));
  await page.waitForTimeout(150);
  // 该模式下 Shift+拖拽仍可强制选区，选中后照常自动复制。
  const trackBox = await page.locator(".xterm-screen").boundingBox();
  await page.keyboard.down("Shift");
  await page.mouse.move(trackBox.x + 2, trackBox.y + 6);
  await page.mouse.down();
  await page.mouse.move(trackBox.x + 150, trackBox.y + 6, { steps: 6 });
  await page.mouse.up();
  await page.keyboard.up("Shift");
  await waitFor(() => page.evaluate(() => window.testClipboard.includes("terminal")), "鼠标上报模式下 Shift 拖拽选中也应复制");
  // 随后的普通点击不应把同一段旧选区再写一次。
  await page.evaluate(() => { window.testClipboard = "sentinel"; });
  await page.locator(".xterm-screen").click({ position: { x: 120, y: 20 } });
  await page.waitForTimeout(200);
  assert.equal(await page.evaluate(() => window.testClipboard), "sentinel", "鼠标上报模式下的普通点击不应复制旧选区");
  const mouseModeOffFrame = Buffer.alloc(8);
  mouseModeOffFrame.writeBigUInt64LE(4n);
  sockets.get("t1").send(Buffer.concat([mouseModeOffFrame, Buffer.from("\x1b[?1003l")]));

  await page.goto(`${base}/projects/p1/conversations/c1`);
  const message = page.locator(".message.assistant");
  await message.waitFor();
  await message.locator(".markdown p").click({ button: "right" });
  const messageMenu = page.getByRole("menu", { name: "消息操作", exact: true });
  await messageMenu.getByRole("menuitem", { name: "复制消息", exact: true }).click();
  assert.equal(await page.evaluate(() => window.testClipboard), content);
  const selectText = (locator) => locator.evaluate((element) => {
    const range = document.createRange();
    range.selectNodeContents(element);
    const selection = window.getSelection();
    selection.removeAllRanges(); selection.addRange(range);
    element.dispatchEvent(new MouseEvent("contextmenu", { bubbles: true, cancelable: true, clientX: innerWidth - 2, clientY: innerHeight - 2 }));
  });
  await selectText(message.locator(".markdown p"));
  await messageMenu.getByRole("menuitem", { name: "复制选中内容", exact: true }).click();
  assert.equal(await page.evaluate(() => window.testClipboard), "消息选区测试");

  const codeMenu = page.getByRole("menu", { name: "代码块操作", exact: true });
  const mixedSelection = await message.evaluate((element) => {
    const paragraph = element.querySelector("p").firstChild;
    const code = element.querySelector("pre code");
    const range = document.createRange();
    range.setStart(paragraph, 0);
    range.setEnd(code.firstChild, code.textContent.length);
    const selection = window.getSelection();
    selection.removeAllRanges(); selection.addRange(range);
    const text = selection.toString();
    code.dispatchEvent(new MouseEvent("contextmenu", { bubbles: true, cancelable: true }));
    return text;
  });
  assert.equal(await codeMenu.getByRole("menuitem", { name: "复制选中内容", exact: true }).isEnabled(), true, "跨正文和代码块的选区仍应允许复制");
  await codeMenu.getByRole("menuitem", { name: "复制选中内容", exact: true }).click();
  assert.equal(await page.evaluate(() => window.testClipboard), mixedSelection);
  await message.evaluate((element) => {
    const range = document.createRange();
    range.selectNodeContents(element.querySelector("p"));
    const selection = window.getSelection();
    selection.removeAllRanges(); selection.addRange(range);
    element.querySelector("pre code").dispatchEvent(new MouseEvent("contextmenu", { bubbles: true, cancelable: true }));
  });
  assert.equal(await codeMenu.getByRole("menuitem", { name: "复制选中内容", exact: true }).isDisabled(), true, "不能把无关位置的残留选区当成代码选区");

  await selectText(message.locator("pre code"));
  await codeMenu.waitFor();
  assert.equal(await messageMenu.count(), 0, "代码块不应冒泡打开消息菜单");
  const bounds = await codeMenu.boundingBox();
  assert.ok(bounds.x >= 0 && bounds.y >= 0 && bounds.x + bounds.width <= 1280 && bounds.y + bounds.height <= 800);
  await page.keyboard.press("End");
  assert.equal(await page.evaluate(() => document.activeElement.textContent), "复制代码");
  await page.keyboard.press("Enter");
  assert.equal(await page.evaluate(() => window.testClipboard), "console.log('菜单');\n");

  // 菜单打开后流式内容仍可能更新；完整复制应读取点击时的内容。
  await message.locator(".markdown p").click({ button: "right" });
  const updatedContent = `${content}\n\n新增的流式内容`;
  const updateMessage = (id, nextContent) => sockets.get("c1").send(JSON.stringify({
    id, type: "assistant.message", runId: "r1", createdAt: new Date().toISOString(), payload: { id: "m1", content: nextContent },
  }));
  updateMessage("update-1", updatedContent);
  await message.getByText("新增的流式内容", { exact: true }).waitFor();
  await messageMenu.getByRole("menuitem", { name: "复制消息", exact: true }).click();
  assert.equal(await page.evaluate(() => window.testClipboard), updatedContent, "完整消息复制不应使用菜单打开时的旧内容");
  await message.locator("pre").click({ button: "right" });
  const updatedCode = "console.log('菜单新版');\n";
  updateMessage("update-2", updatedContent.replace("console.log('菜单');\n", updatedCode));
  await message.locator("pre code").getByText("console.log('菜单新版');", { exact: true }).waitFor();
  await codeMenu.getByRole("menuitem", { name: "复制代码", exact: true }).click();
  assert.equal(await page.evaluate(() => window.testClipboard), updatedCode, "完整代码复制不应使用菜单打开时的旧内容");
  await message.locator("pre").click({ button: "right" });
  await page.mouse.click(5, 5);
  await codeMenu.waitFor({ state: "hidden" });
  await message.locator("pre").click({ button: "right" });
  await page.evaluate(() => window.dispatchEvent(new Event("blur")));
  await codeMenu.waitFor({ state: "hidden" });

  // 共享代码菜单也用于文件预览；打开它时应收起原有文件菜单。
  await page.goto(`${base}/projects/p1/files`);
  // 右键要覆盖**整条**文件行：行的右端在文件名文字之外（按钮剩余宽度 / 行内边距），
  // 以前那里右键没反应，只有正好点在文字上才弹菜单。
  const treeRow = page.locator(".file-tree-body [data-rct-item-container]").first();
  await treeRow.waitFor();
  const rowBox = await treeRow.boundingBox();
  const nameBox = await treeRow.locator(".file-tree-item-name").boundingBox();
  const titleBox = await treeRow.locator(".file-tree-item").boundingBox();
  assert.ok(rowBox && nameBox && titleBox, "文件行、文件名与标题都应可测量");
  const probeX = nameBox.x + nameBox.width + 8;
  // 落点必须严格越过文字和标题 —— 正是用户说的"放到文字后面一点点"。
  // 旧实现把 onContextMenu 挂在标题上，这个点不在它的命中范围内，所以点了没反应。
  assert.ok(probeX > titleBox.x + titleBox.width, "落点须在标题之外，才算复现了行尾右键");
  assert.ok(probeX < rowBox.x + rowBox.width, "落点仍须落在该行内");
  await page.mouse.click(probeX, rowBox.y + rowBox.height / 2, { button: "right" });
  const treeMenu = page.locator(".file-tree-context-menu");
  await treeMenu.waitFor();
  assert.equal(await treeMenu.getByRole("button", { name: "打开", exact: true }).isVisible(), true, "行尾右键应弹出该文件的菜单");
  // 关菜单别再点界面上的东西：body 上的合成 click 会冒泡到 document，正好是关菜单那条路径。
  await page.evaluate(() => document.body.click());
  await treeMenu.waitFor({ state: "hidden" });
  await page.locator(".file-tree-item-name").getByText("README.md", { exact: true }).dblclick();
  const previewCode = page.locator(".file-viewer-markdown pre");
  await previewCode.waitFor();
  await page.locator(".file-tab").click({ button: "right" });
  await page.locator(".file-tree-context-menu").waitFor();
  await previewCode.click({ button: "right" });
  await codeMenu.waitFor();
  assert.equal(await page.locator(".file-tree-context-menu").count(), 0, "代码菜单打开时不应残留文件标签菜单");
  await page.locator(".file-tab").click({ button: "right" });
  await page.locator(".file-tree-context-menu").waitFor();
  await codeMenu.waitFor({ state: "hidden" });
  await previewCode.click({ button: "right" });
  await codeMenu.waitFor();
  await page.locator(".file-tree-item-name").getByText("README.md", { exact: true }).click({ button: "right" });
  await page.locator(".file-tree-context-menu").waitFor();
  await codeMenu.waitFor({ state: "hidden" });
  await previewCode.click({ button: "right" });
  await codeMenu.waitFor();
  assert.equal(await page.locator(".file-tree-context-menu").count(), 0, "代码菜单打开时不应残留文件树菜单");
  await page.keyboard.press("Escape");
  await codeMenu.waitFor({ state: "hidden" });

  // 网页和手机沿用浏览器菜单；桌面端的文本输入编辑菜单仍可用。
  assert.equal(await page.evaluate(() => {
    const textarea = document.createElement("textarea");
    document.body.append(textarea);
    const prevented = !textarea.dispatchEvent(new MouseEvent("contextmenu", { bubbles: true, cancelable: true }));
    textarea.remove();
    return prevented;
  }), false);
  await page.goto(`${base}/projects/p1/conversations/c1?web=1`);
  await message.waitFor();
  assert.equal(await message.locator("pre").evaluate((element) => !element.dispatchEvent(new MouseEvent("contextmenu", { bubbles: true, cancelable: true }))), false);
  assert.equal(await codeMenu.count(), 0);
  assert.deepEqual(errors, []);
  await context.close();
  console.log("PASS 正式构建：终端选区自动复制（拖拽/双击/三击/拖出终端、滚动条与单击不复制）、右键粘贴/Shift+右键菜单、复制粘贴与目录、剪贴板失败、会话切换保护、消息和代码选区、菜单定位/键盘/关闭、网页菜单兼容");
} catch (error) {
  console.error(error);
  console.error(errors, debugRequests);
  if (testPage) console.error(await testPage.locator("body").innerText());
  throw error;
} finally {
  await browser?.close();
  server.httpServer.closeAllConnections();
  await new Promise((resolve) => server.httpServer.close(resolve));
}
