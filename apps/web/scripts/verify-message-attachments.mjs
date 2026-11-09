// Browser behavior of the shared desktop/mobile attachment composer. Uses an
// in-memory filesystem fixture; never invokes a model or touches user projects.
import assert from "node:assert/strict";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { chromium } from "playwright";
import { createServer, transformWithEsbuild } from "vite";

const entry = `
import React,{useState} from 'react';import{createRoot}from'react-dom/client';
import{MessageAttachmentPicker}from'/src/features/files/MessageAttachments.tsx';
import{MobileModelSheet}from'/src/features/remote/MobileSessionDock.tsx';
import '/src/features/files/message-attachments.css';
function ModelHarness(){const[effort,setEffort]=useState('Deep'),[busy,setBusy]=useState(false);return <MobileModelSheet models={{models:[],reasoningEffort:effort,reasoningOptions:['low','high'],reasoningCustomAllowed:true}} busy={busy} onClose={()=>{}} onSelect={()=>{}} onReasoning={value=>{setBusy(true);setTimeout(()=>{setEffort(value);setBusy(false)},200)}}/>}
function Harness(){const[files,setFiles]=useState([]),[busy,setBusy]=useState(false),[error,setError]=useState(''),[id,setID]=useState('first');
const request=async(url,init)=>{if(init?.method==='PUT'){const body=JSON.parse(init.body);if(body.path.endsWith('/bad.txt'))throw Error('fixture failed');await new Promise(r=>setTimeout(r,300));}return {isDir:false,size:3,mimeType:'text/plain'}};
return <main style={{padding:20,position:'fixed',bottom:20,left:0,right:0}}><button onClick={()=>{setID('second');setFiles([])}}>Switch</button><output>{error}</output><div data-testid="files">{files.map(f=><p key={f.path}>{f.path}</p>)}</div><MessageAttachmentPicker key={id} projectID="p" mobile={location.search.includes('mobile')} disabled={busy} attachments={files} request={request} onBusy={setBusy} onFail={setError} onAdd={added=>setFiles(old=>[...old,...added])}/><button disabled={busy}>Send</button></main>}
createRoot(document.getElementById('root')).render(location.search.includes('model')?<ModelHarness/>:<Harness/>);`;
const server = await createServer({
  configFile: fileURLToPath(new URL("../vite.config.ts", import.meta.url)),
  cacheDir: fileURLToPath(new URL("../node_modules/.vite-message-attachments-test", import.meta.url)),
  server: { host: "127.0.0.1", port: 0, hmr: false, ws: false },
  resolve: { alias: [{ find: /^.*\/cloud-url(?:\.ts)?$/, replacement: "\0attachment-cloud" }] },
  plugins: [{
    name: "attachment-browser-fixture",
    resolveId(id) { if (id === "/__attachments.tsx") return "\0attachments.tsx"; if (id === "\0attachment-cloud") return id; },
    async load(id) { if (id === "\0attachment-cloud") return 'export const cloudURL = "";'; if (id === "\0attachments.tsx") return (await transformWithEsbuild(entry, "attachments.tsx", { loader: "tsx", jsx: "automatic" })).code; },
    configureServer(vite) { vite.middlewares.use((req,res,next)=>{
      if (!req.url.startsWith('/attachment-fixture')) return next();
      res.setHeader('Content-Type','text/html; charset=utf-8');
      res.end('<html><head><meta name="viewport" content="width=device-width,initial-scale=1"></head><body><div id="root"></div><script type="module" src="/__attachments.tsx"></script></body></html>');
    }); },
  }],
});
let browser;
async function verifyMobileConversation(browser, base) {
  const context = await browser.newContext({ viewport: { width: 390, height: 844 }, permissions: ["clipboard-read", "clipboard-write"] });
  const page = await context.newPage();
  const errors = [], sent = [];
  let snapshotRequests = 0, releaseFailure;
  const failureGate = new Promise(resolve => { releaseFailure = resolve; });
  const files = [{ path: "docs/existing.txt", mediaType: "text/plain", size: 7 }];
  const createdAt = new Date().toISOString();
  const messages = [
    { id: "old-file", clientRequestId: "old-file-request", role: "user", content: "", attachments: files, createdAt },
    { id: "old-text", clientRequestId: "old-text-request", role: "user", content: "再试试", attachments: files, createdAt },
  ];
  const conversation = { id: "c1", title: "当前会话", status: "idle", agentId: "codex", isCurrent: true, lastActivityAt: createdAt, messages };
  const project = { id: "p1", name: "远程附件回归", runner: "local", environment: "windows", gitBranch: "main", tasks: [], conversations: [conversation], skills: [] };
  page.on("pageerror", error => errors.push(error.message));
  await page.addInitScript(() => {
    localStorage.setItem("milevia.cloud.devices", JSON.stringify([{ token: "fixture-token", instanceId: "pc1", name: "回归电脑" }]));
    localStorage.setItem("milevia.cloud.active", "fixture-token");
    localStorage.setItem("milevia.cloud.token", "fixture-token");
  });
  await page.route("**/api/**", route => route.fulfill({ contentType: "application/json", body: "[]" }));
  await page.route("**/v1/**", async route => {
    const request = route.request(), p = new URL(request.url()).pathname;
    let data = [];
    if (p === "/v1/instances") data = [{ instanceId: "pc1", name: "回归电脑", status: "online", lastAgentSequence: 1 }];
    else if (p.endsWith("/snapshot")) {
      snapshotRequests++;
      data = { snapshotRevision: 1, observedAt: createdAt, projects: [project] };
    } else if (p.endsWith("/commands")) {
      sent.push(request.postDataJSON()); data = { commandId: "failed-message", status: "queued" };
    } else if (p === "/v1/commands/failed-message") {
      await failureGate; data = { commandId: "failed-message", status: "failed", result: { error: "回归：文件不可用" } };
    } else if (p.endsWith("/rpc")) {
      const body = request.postDataJSON();
      let value = [];
      if (body.op === "conversation.history") value = { conversation, messages, events: [], activeRunId: null, hasMore: false, hasMoreMessages: false, nextCursor: "" };
      else if (body.op === "conversation.list") value = { items: [conversation], nextCursor: "" };
      else if (body.op === "conversation.usage") value = { context: { model: "" }, session: { taskCount: 0 } };
      else if (body.op === "conversation.checkpoints") value = { checkpoints: [] };
      data = { ok: true, status: 200, data: value };
    } else if (p.endsWith("/stream")) {
      await route.fulfill({ contentType: "text/event-stream", body: ": fixture\n\n" }); return;
    }
    await route.fulfill({ contentType: "application/json", body: JSON.stringify(data) });
  });
  try {
    await page.goto(`${base}/mobile`);
    await page.locator(".mobile-project-choice").filter({ hasText: project.name }).click();
    await page.locator(".mobile-message.user").first().waitFor();
    const first = page.locator(".mobile-message.user").first();
    await first.locator(".mobile-message-more").click();
    await first.getByRole("button", { name: "复制这条消息", exact: true }).click();
    assert.equal(await page.evaluate(() => navigator.clipboard.readText()), "@docs/existing.txt", "手机纯附件复制不能是空串");
    await first.getByRole("button", { name: "引用我这条消息到输入框", exact: true }).click();
    assert.equal(await page.locator(".mobile-quote-ref-text").innerText(), "@docs/existing.txt");
    await page.locator(".mobile-quote-ref-remove").click();
    await first.locator(".mobile-message-more").click();
    await first.getByRole("button", { name: "把这条消息的原文填回输入框", exact: true }).click();
    await page.getByRole("button", { name: "移除附件 docs/existing.txt", exact: true }).waitFor();
    await page.getByRole("button", { name: "工具", exact: true }).click();
    await page.getByRole("button", { name: "清空草稿", exact: true }).click();
    await page.getByRole("button", { name: "移除附件 docs/existing.txt", exact: true }).waitFor({ state: "hidden" });
    assert.equal(sent.length, 0, "填回附件或清空草稿不能自动发送");
    await first.locator(".mobile-message-more").click();
    await first.getByRole("button", { name: "把这条消息的原文填回输入框", exact: true }).click();
    await page.getByRole("button", { name: "移除附件 docs/existing.txt", exact: true }).waitFor();
    await page.getByRole("textbox", { name: "输入消息", exact: true }).fill("再试试");
    const before = snapshotRequests;
    const nextSnapshot = page.waitForResponse(response => response.url().includes("/snapshot"));
    await page.getByRole("button", { name: "发送消息", exact: true }).click();
    await page.waitForFunction(() => document.querySelectorAll(".mobile-message.user[data-transient=true]").length === 1);
    await nextSnapshot;
    assert.ok(snapshotRequests > before);
    assert.equal(await page.locator(".mobile-message.user").count(), 3, "旧的同内容同附件历史不能确认新的待发请求");
    assert.equal(sent.length, 1);
    assert.deepEqual(sent[0].payload.attachments, files);
    releaseFailure();
    await page.waitForFunction(() => document.querySelector("textarea[aria-label='输入消息']")?.value === "再试试");
    await page.getByRole("button", { name: "移除附件 docs/existing.txt", exact: true }).waitFor();
    await page.getByText(/消息执行失败/).waitFor();
    assert.equal(await page.locator(".mobile-message.user[data-transient=true]").count(), 0);
    assert.deepEqual(errors, []);
    console.log("PASS MobileRemotePage: attachment-only copy/quote/resend/clear, old history does not confirm repeated send, explicit failure restores text and attachments");
  } finally { releaseFailure(); await context.close(); }
}
async function verifyConversationPage(browser, base) {
  const page = await browser.newPage({ viewport: { width: 1280, height: 900 } });
  const errors = [], fileRequests = [], discoveryRequests = [], sent = [];
  let activeWorkspace = "workspace-1";
  let activeRunId = null;
  let reasoningEffort = "Deep";
  const reasoningRequests = [];
  const conversation = { id: "attachment-conversation", projectId: "p", title: "附件回归", status: "idle", agentId: "claude-code", permissionMode: "approval_required", isCurrent: true, createdAt: "2026-11-06T00:00:00Z" };
  const messages = [{ id: "initial", role: "user", content: "附件测试已就绪", createdAt: conversation.createdAt }];
  page.on("pageerror", error => errors.push(error.message));
  await page.routeWebSocket(/\/ws\//, socket => socket.close());
  await page.route("**/api/**", async route => {
    const request = route.request(), url = new URL(request.url()), p = url.pathname;
    let data = [], status = 200;
    if (p === "/api/projects/p") data = { id: "p", name: "附件回归", fullPath: "fixture", pathDisplay: "fixture", runner: "local", environment: "windows", localRunner: true, claudeReady: true, agentReady: true };
    else if (p === "/api/projects/p/conversations") data = request.method() === "GET" ? { items: [conversation], nextCursor: "" } : conversation;
    else if (p === "/api/conversations/attachment-conversation/activate") data = conversation;
    else if (p === "/api/conversations/attachment-conversation") data = { conversation, messages, events: [], activeRunId, hasMore: false, hasMoreMessages: false, nextCursor: "" };
    else if (p === "/api/conversations/attachment-conversation/messages") {
      const body = request.postDataJSON(); sent.push(body);
      activeRunId = `fixture-run-${sent.length}`;
      const message = { id: `sent-${sent.length}`, runId: activeRunId, conversationId: conversation.id, role: "user", content: body.content, attachments: body.attachments, createdAt: conversation.createdAt };
      messages.push(message); data = { message, runId: activeRunId }; status = 202;
    } else if (/\/api\/runs\/fixture-run-\d+\/stop/.test(p)) {
      activeRunId = null; data = { status: "stopped" };
    } else if (p.includes("/fs/")) {
      fileRequests.push({ path: p, conversationID: url.searchParams.get("conversationId") });
      data = p.endsWith("/stat") ? { isDir: false, size: 7, mimeType: "text/plain" } : {};
    } else if (p.endsWith("/skills") || p.endsWith("/plugins")) {
      discoveryRequests.push(url.searchParams.get("conversationId"));
      data = p.endsWith("/plugins") ? { plugins: [], note: "fixture" } : [];
    } else if (p.includes("/workspaces/") && p.endsWith("/activate")) { activeWorkspace = p.split("/").at(-2); data = {}; }
    else if (p.endsWith("/workspaces")) data = ["workspace-1", "workspace-2"].map((id, i) => ({ id, conversationId: conversation.id, mode: i ? "isolated_worktree" : "shared_project", state: "ready", active: activeWorkspace === id, generation: i + 1 }));
    else if (p.endsWith("/usage")) data = { context: { model: "" }, session: { taskCount: 0 } };
    else if (p.endsWith("/checkpoints")) data = { checkpoints: [] };
    else if (p.endsWith("/activity")) data = { conversations: [], missingConversationIds: [] };
    else if (p.endsWith("/commands")) data = { commands: [], authoritative: false, customAllowed: true };
    else if (p.endsWith("/models")) data = { models: [], reasoningOptions: ["low", "high"], reasoningCustomAllowed: true, reasoningEffort };
    else if (p.endsWith("/reasoning-effort")) {
      const body = request.postDataJSON(); reasoningRequests.push(body.reasoningEffort);
      await new Promise(resolve => setTimeout(resolve, 200));
      reasoningEffort = body.reasoningEffort; data = { reasoningEffort };
    }
    else if (/\/(shortcuts|input-history|tasks|statuses)(\/|$)/.test(p)) data = [];
    else { status = 404; data = { error: "未模拟的辅助接口" }; }
    await route.fulfill({ status, contentType: "application/json", body: JSON.stringify(data) });
  });
  try {
    await page.goto(`${base}/projects/p/conversations/${conversation.id}`);
    await page.locator(".message.user").filter({ hasText: "附件测试已就绪" }).waitFor();
    await page.getByRole("button", { name: "添加附件", exact: true }).click();
    await page.locator(".message-attachment-picker input[type=file]").setInputFiles({ name: "uploaded.txt", mimeType: "text/plain", buffer: Buffer.from("fixture") });
    await page.getByRole("button", { name: /移除附件 .*uploaded.txt/ }).waitFor();
    await page.getByRole("combobox", { name: "会话工作区", exact: true }).selectOption("workspace-2");
    await page.getByRole("button", { name: /移除附件 .*uploaded.txt/ }).waitFor({ state: "hidden" });
    assert.equal(sent.length, 0, "切换工作区不能自动发送旧附件");
    await page.getByRole("button", { name: "添加附件", exact: true }).click();
    await page.locator(".message-attachment-picker input[type=file]").setInputFiles({ name: "uploaded.txt", mimeType: "text/plain", buffer: Buffer.from("fixture") });
    await page.getByRole("button", { name: /移除附件 .*uploaded.txt/ }).waitFor();
    await page.getByRole("button", { name: "添加附件", exact: true }).click();
    await page.getByRole("textbox", { name: "附件路径" }).fill("docs/existing.txt");
    await page.getByRole("button", { name: "引用文件", exact: true }).click();
    await page.getByRole("button", { name: "移除附件 docs/existing.txt", exact: true }).waitFor();
    // Freeze browser timers so a new draft can be composed before the 400 ms
    // scheduled-send delay, independently of the speed of this machine.
    await page.clock.install({ time: new Date("2026-11-06T00:00:00Z") });
    await page.clock.pauseAt(new Date("2026-11-06T00:00:01Z"));
    await page.getByRole("button", { name: "发送方式", exact: true }).click();
    await page.getByRole("menuitem", { name: /预约发送/ }).click();
    await page.locator(".composer-pending").waitFor();
    await page.getByRole("button", { name: "添加附件", exact: true }).click();
    await page.getByRole("textbox", { name: "附件路径" }).fill("docs/new-draft.txt");
    await page.getByRole("button", { name: "引用文件", exact: true }).click();
    await page.getByRole("button", { name: "移除附件 docs/new-draft.txt", exact: true }).waitFor();
    await page.locator(".composer-edit").fill("later draft");
    await page.clock.runFor(500);
    await page.waitForFunction(() => document.querySelectorAll(".message.user").length === 2);
    assert.equal(sent.length, 1);
    assert.equal(sent[0].content, "");
    assert.equal(sent[0].attachments.length, 2);
    assert.equal(await page.locator(".composer-edit").innerText(), "later draft", "预约发送不能清掉后来写的正文");
    await page.getByRole("button", { name: "移除附件 docs/new-draft.txt", exact: true }).waitFor();
    await page.locator(".composer-stop").click();
    await page.getByRole("button", { name: "移除附件 docs/existing.txt", exact: true }).waitFor();
    await page.getByRole("button", { name: /移除附件 .*uploaded.txt/ }).waitFor();
    await page.getByRole("button", { name: "移除附件 docs/new-draft.txt", exact: true }).waitFor();
    assert.equal(await page.locator(".composer-edit").innerText(), "later draft", "停止纯附件消息应合并恢复附件并保留新正文");
    await page.getByRole("button", { name: "发送方式", exact: true }).click();
    await page.getByRole("menuitem", { name: /预约发送/ }).click();
    await page.locator(".composer-edit").fill("newer draft");
    await page.locator(".composer-pending-cancel").click();
    assert.equal(await page.locator(".composer-edit").innerText(), "newer draft\nlater draft");
    await page.getByRole("button", { name: "移除附件 docs/existing.txt", exact: true }).waitFor();
    await page.getByRole("button", { name: "移除附件 docs/new-draft.txt", exact: true }).click();
    await page.locator(".composer-model-trigger").click();
    const effortSelect = page.locator(".model-menu-effort select");
    await effortSelect.waitFor();
    assert.equal(await effortSelect.inputValue(), "Deep", "自定义推理强度不能显示成默认值");
    await page.locator("#composer-custom-variant").fill("Deeper");
    await page.locator("#composer-custom-variant").locator("..").getByRole("button", { name: "使用", exact: true }).click();
    assert.equal(await effortSelect.isDisabled(), true, "推理强度切换请求期间必须禁用重复操作");
    await page.waitForFunction(() => document.querySelector(".model-menu-effort select")?.value === "Deeper");
    assert.deepEqual(reasoningRequests, ["Deeper"]);
    await page.getByRole("button", { name: "关闭", exact: true }).click();
    await page.locator(".composer-edit").fill("");
    await page.locator(".composer-send").click();
    await page.waitForFunction(() => document.querySelectorAll(".message.user").length === 2);
    assert.equal(sent.length, 2);
    assert.equal(sent[1].content, "");
    assert.equal(sent[1].attachments.length, 2);
    assert.ok(sent[1].attachments.some(file => file.path === "docs/existing.txt"));
    assert.ok(fileRequests.some(item => item.path.endsWith("/write")));
    assert.ok(fileRequests.every(item => item.conversationID === conversation.id), JSON.stringify(fileRequests));
    assert.ok(discoveryRequests.includes(conversation.id));
    assert.deepEqual(errors, []);
    await page.screenshot({ path: path.join(os.tmpdir(), "milevia-conversation-attachments.png") });
    console.log("PASS ConversationPage: workspace scoping, attachment-only POST/history, scheduled send preserves newer draft, cancellation merges draft, stop restores attachment-only draft, custom reasoning display and busy state");
  } finally { await page.close(); }
}
try {
  await server.listen();
  browser = await chromium.launch({ executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH || undefined });
  for (const mobile of [false,true]) {
    const page = await browser.newPage({ viewport: mobile ? {width:390,height:844} : {width:1280,height:900} });
    const errors=[];page.on('pageerror',error=>errors.push(error.message));
    if (mobile) await page.addInitScript(() => {
      // Reproduce the crypto capability exposed by a LAN HTTP page.
      Object.defineProperty(globalThis.crypto, "randomUUID", { configurable: true, value: undefined });
    });
    await page.goto(`http://127.0.0.1:${server.httpServer.address().port}/attachment-fixture${mobile?'?mobile':''}`);
    await page.getByRole('button',{name:'添加附件',exact:true}).click();
    const menu=page.getByRole('group',{name:'添加附件',exact:true});
    await menu.waitFor();
    const box=await menu.boundingBox();assert.ok(box.x>=0&&box.x+box.width<=(mobile?390:1280));
    await page.locator('input[type=file]').setInputFiles([
      {name:'bad.txt',mimeType:'text/plain',buffer:Buffer.from('bad')},
      {name:'good.png',mimeType:'image/png',buffer:Buffer.from('good')},
    ]);
    await page.waitForFunction(()=>document.querySelector('button:last-child')?.disabled===true);
    await page.getByTestId('files').getByText(/good.png/).waitFor();
    if (mobile) assert.match(await page.getByTestId('files').textContent(), /\.milevia-attachments\/upload-[a-z0-9-]+\/good\.png/);
    assert.match(await page.locator('output').textContent(),/bad.txt/);
    assert.equal(await page.getByTestId('files').getByText(/bad.txt/).count(),0);
    await page.getByRole('button',{name:'添加附件',exact:true}).click();
    await page.getByRole('textbox',{name:'附件路径'}).fill('docs/existing.txt');
    await page.getByRole('button',{name:'引用文件',exact:true}).click();
    await page.getByTestId('files').getByText('docs/existing.txt').waitFor();
    const screenshot=path.join(os.tmpdir(),`milevia-attachments-${mobile?'mobile':'desktop'}.png`);
    await page.screenshot({path:screenshot});
    await page.locator('input[type=file]').setInputFiles({name:'late.txt',mimeType:'text/plain',buffer:Buffer.from('late')});
    await page.getByRole('button',{name:'Switch',exact:true}).click();
    await page.waitForTimeout(500);
    assert.equal(await page.getByTestId('files').textContent(),'');
    assert.deepEqual(errors,[]);
    console.log(`PASS ${mobile?'mobile':'desktop'}: partial upload, busy send, path reference, conversation switch; ${screenshot}`);
    await page.close();
  }
  const modelPage = await browser.newPage({ viewport: { width: 390, height: 844 } });
  try {
    await modelPage.goto(`http://127.0.0.1:${server.httpServer.address().port}/attachment-fixture?model`);
    const select = modelPage.getByRole("combobox");
    await select.waitFor();
    assert.equal(await select.inputValue(), "Deep");
    const variantInput = modelPage.getByRole("textbox", { name: /自定义 variant/ });
    await variantInput.fill("Deeper");
    await variantInput.locator("../..").getByRole("button", { name: "使用", exact: true }).click();
    await modelPage.waitForFunction(() => document.querySelector("select")?.value === "Deeper");
    console.log("PASS mobile model sheet: custom reasoning remains selected after applying a variant");
  } finally { await modelPage.close(); }
  await verifyConversationPage(browser, `http://127.0.0.1:${server.httpServer.address().port}`);
  await verifyMobileConversation(browser, `http://127.0.0.1:${server.httpServer.address().port}`);
} finally { await browser?.close();await server.close(); }
