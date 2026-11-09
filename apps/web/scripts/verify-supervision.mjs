// 独立托管页的真实浏览器交互验证；API 使用可控夹具，执行器闭环另有 Go 集成测试。
import assert from "node:assert/strict";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { chromium } from "playwright";
import { createServer, transformWithEsbuild } from "vite";

const entry = `
import React from 'react'; import {createRoot} from 'react-dom/client';
import {BrowserRouter,Routes,Route,useNavigate} from 'react-router-dom';
import SupervisionPage from '/src/pages/SupervisionPage.tsx';
function Harness(){const navigate=useNavigate();window.switchSupervisionProject=()=>navigate('/projects/q/supervision');return <Routes><Route path='/projects/:projectId/supervision' element={<SupervisionPage/>}/></Routes>}
createRoot(document.getElementById('root')).render(<BrowserRouter><Harness/></BrowserRouter>);
`;
const server = await createServer({
  configFile: fileURLToPath(new URL("../vite.config.ts", import.meta.url)),
  plugins: [{
    name: "supervision-browser-verification",
    resolveId(id) { if (id === "/__supervision-test.tsx") return "\0supervision-test.tsx"; },
    async load(id) { if (id === "\0supervision-test.tsx") return (await transformWithEsbuild(entry, "supervision-test.tsx", { loader: "tsx", jsx: "automatic" })).code; },
    configureServer(vite) {
      vite.middlewares.use((req, res, next) => {
        if (!req.url.startsWith("/projects/")) return next();
        res.setHeader("Content-Type", "text/html; charset=utf-8");
        res.end('<html><head><meta name="viewport" content="width=device-width,initial-scale=1"><style>html,body,#root{height:100%;margin:0;font-family:system-ui}*{box-sizing:border-box}</style></head><body><div id="root"></div><script type="module" src="/__supervision-test.tsx"></script></body></html>');
      });
    },
  }],
  server: { host: "127.0.0.1", port: 0 },
});
await server.listen();
const base = `http://127.0.0.1:${server.httpServer.address().port}`;
const budget = { maxActiveMillis: 14400000, maxAgentTurns: 200, maxTurnMillis: 1800000, maxFixRounds: 3, maxTransientRetries: 2, finalReservePercent: 20 };
let browser;
async function verify(mobile) {
  const context = await browser.newContext({ viewport: mobile ? { width: 390, height: 844 } : { width: 1440, height: 1050 } });
  if (mobile) await context.addInitScript(() => Object.defineProperty(globalThis.crypto, "randomUUID", { value: undefined }));
  const page = await context.newPage(); const errors = []; page.on("pageerror", error => errors.push(error.message));
  let session = { id: "s1", projectId: "p", state: "draft", stateVersion: 1, budget, usedTurns: 0, reservedTurns: 0, activeMillis: 0, recoveryState: "ready", createdAt: "2026-11-03T01:00:00Z" };
  let created = false, answered = false, cleaned = false, protectionReleased = false; const writes = [];
  let managedProfiles = [{ id: "foreign-profile", currentRevisionId: "foreign-revision", agentId: "codex", name: "另一个目标的档案", model: "foreign-model", authMode: "cli_managed", enabled: true, state: "active", executionMode: "target_isolated", targetId: "foreign-target" }];
  const profileWrites = [];
  let workspaceOperations = [
    { id: "unknown-writer", path: "C:/project/unknown", state: "unconfirmed", updatedAt: "2026-11-05T01:00:00Z" },
    { id: "finished-writer", path: "C:/project/finished", state: "release_pending", updatedAt: "2026-11-05T01:00:00Z" },
  ];
  let loseInstructionResponse = true; let appliedInstructions = 0; const instructionReceipts = new Map();
  let delayDetail = false, releaseDetail, detailEntered;
  const delayed = new Promise(resolve => { releaseDetail = resolve; });
  const entered = new Promise(resolve => { detailEntered = resolve; });
  const detail = () => ({ protection: { state: session.state === "draft" ? "not_acquired" : ["stopped", "completed", "completed_with_issues", "failed"].includes(session.state) ? protectionReleased ? "released" : "releasing" : "owned", target: false, finalization: "" }, wait: session.state === "running" ? { condition: "等待测试环境准备", nextCheckAt: "2026-11-03T02:00:00Z", rounds: 1 } : null, storage: { bytes: 3145728, limitBytes: 2147483648, diskFreeBytes: 10737418240, diskReserveBytes: 268435456, accounting: "logical_payload" }, session, tasks: [{ taskId: "t1", phase: session.state === "draft" ? "pending" : "implemented", requirementsRevision: 1, planRevision: 1, evidenceFreshness: "stale", fixRounds: 0, taskSnapshot: { title: "修复加法与边界条件", description: "完成选中的任务并独立验收" } }], plans: [{ taskId: "t1", revision: 1, status: "ready", plan: { goal: "加法行为符合需求", scope: ["src/math.cjs"], dependsOn: [], environment: ["Node"], limitations: [], acceptance: [{ id: "behavior", requirement: "正负数加法正确", kind: "command", verification: "真实断言", critical: true }] } }], jobs: [], evidence: session.state === "draft" ? [] : [{ id: "e1", taskId: "t1", executionId: "x1", kind: "browser", passed: true, revisionId: "current", requirementsRevision: 1, planRevision: 1, details: { exitCode: 0, stdout: "behavior checked" }, createdAt: "2026-11-03T01:03:00Z" }], questions: session.state !== "draft" && !answered ? [{ id: "q1", taskId: "t1", question: "失败边界是否包括负数？", answered: false }] : [], events: [{ seq: 1, type: session.state === "failed" ? "session.failed" : "work_item.dispatched", payload: { instruction: "修复选中的加法任务" }, createdAt: "2026-11-03T01:01:00Z" }], eventsTruncated: false, projectContext: { id: "pc1", revisionId: "current", rulesHash: "rules", rules: [], entries: 2 }, artifacts: [{ executionId: "x1", asset: "screenshot.png", state: cleaned ? "cleaned" : "present", sha256: "hash" }] });
  await page.route("**/api/**", async route => {
    const request = route.request(); const url = new URL(request.url()); const pathname = url.pathname;
    const respond = data => route.fulfill({ status: 200, json: data });
    if (pathname.includes("/workspace-operations")) {
      if (request.method() === "GET") return respond(pathname.startsWith("/api/projects/p/") ? workspaceOperations : []);
      if (pathname.includes("/unknown-writer/")) return route.fulfill({ status: 409, json: { error: "目标退出状态仍未确认" } });
      workspaceOperations = workspaceOperations.filter(operation => operation.id !== "finished-writer");
      return respond({ state: "released" });
    }
    if (pathname.includes("/supervision-profiles") || pathname.includes("/supervision-profile-revisions")) {
      if (request.method() === "GET") return respond(managedProfiles);
      const body = request.postDataJSON(); profileWrites.push({ path: pathname, body, projectId: url.searchParams.get("projectId") });
      if (request.method() === "POST" && pathname.endsWith("/supervision-profiles")) managedProfiles.push({ ...body, apiKey: undefined, id: "managed-profile", currentRevisionId: "managed-v1", enabled: true, state: "active", executionMode: "target_isolated", targetId: "native:windows-local" });
      if (request.method() === "PATCH") managedProfiles = managedProfiles.map(profile => profile.id === "managed-profile" ? { ...profile, ...body, apiKey: undefined, currentRevisionId: "managed-v2" } : profile);
      if (pathname.endsWith("/disable")) managedProfiles = managedProfiles.map(profile => profile.id === "managed-profile" ? { ...profile, enabled: false } : profile);
      if (pathname.endsWith("/revoke")) managedProfiles = managedProfiles.map(profile => profile.id === "managed-profile" ? { ...profile, state: "revoked" } : profile);
      return respond({ accepted: true });
    }
    if (request.method() !== "GET") {
      const body = request.postDataJSON(); const key = request.headers()["idempotency-key"];
      if (request.method() === "POST" || request.method() === "PUT") assert(key, "supervision write must carry a stable request key");
      writes.push({ path: pathname, method: request.method(), body, key });
      if (pathname.endsWith("/instructions")) {
        if (instructionReceipts.has(key)) { assert.equal(instructionReceipts.get(key), request.postData()); return route.fulfill({ status: 202, json: { accepted: true } }); }
        instructionReceipts.set(key, request.postData()); appliedInstructions++; session = { ...session, stateVersion: session.stateVersion + 1 };
        if (loseInstructionResponse) { loseInstructionResponse = false; return route.abort("failed"); }
      }
      if (pathname === "/api/projects/p/supervision") { created = true; session = { ...session, schedulerAgentId: body.schedulerAgentId, roles: body.roles }; return route.fulfill({ status: 201, json: session }); }
      if (pathname.endsWith("/start")) session = { ...session, state: "running", stateVersion: session.stateVersion + 1 };
      if (pathname.endsWith("/control")) session = { ...session, state: { pause: "paused", resume: "running", stop: "stopped" }[body.action], stateVersion: session.stateVersion + 1 };
      if (pathname.endsWith("/answer")) answered = true;
      if (pathname.endsWith("/roles")) { session = { ...session, roles: body.roles, stateVersion: session.stateVersion + 1 }; return route.fulfill({ status: 204 }); }
      if (pathname.endsWith("/archive")) { session = { ...session, archivedAt: body.archived ? new Date().toISOString() : undefined, stateVersion: session.stateVersion + 1 }; return route.fulfill({ status: 204 }); }
      if (pathname.endsWith("/budget")) session = { ...session, budget: body.budget, stateVersion: session.stateVersion + 1 };
      if (pathname.endsWith("/artifacts")) cleaned = true;
      if (pathname.endsWith("/instructions") || pathname.endsWith("/answer") || pathname.endsWith("/manual-evidence")) return route.fulfill({ status: 202, json: { accepted: true } });
      return respond(session);
    }
    if (pathname.endsWith("/capabilities")) return respond({ profiles: [{ id: "review-profile", name: "独立审查认证", agentId: "claude-code", model: "sonnet" }], target: { targetId: "native:windows-local", runnerId: "windows-local", environment: "windows", local: true, state: "ready" }, agents: [
      { agentId: "claude-code", name: "Claude Code", available: true, state: "probe_required", supportsProfiles: true, reasoningEfforts: ["low", "medium", "high"] },
      { agentId: "codex", name: "Codex", available: false, state: "adapter_pending", reason: "无隔离适配", supportsProfiles: true, reasoningEfforts: ["low", "medium", "high", "xhigh"] },
      // 只用于验证页面遵守后端元数据；不代表该工具的生产执行已完成适配。
      { agentId: "codebuddy", name: "CodeBuddy", available: true, state: "probe_required", supportsProfiles: false, reasoningEfforts: [] },
      ...["qoder", "opencode", "cursor"].map(agentId => ({ agentId, available: false, state: "adapter_pending", reason: "受控适配待实现", supportsProfiles: false, reasoningEfforts: [] })),
    ], defaultAgentId: pathname.startsWith("/api/projects/q/") ? "codex" : "claude-code", defaultBudget: budget, maxTasks: 500 });
    if (pathname.endsWith("/tasks")) return respond([{ id: pathname.includes("/q/") ? "q-task" : "t1", title: pathname.includes("/q/") ? "另一个项目的任务" : "修复加法与边界条件", status: "todo" }, { id: "t2", title: "无关任务", status: "todo" }, { id: "t-review", title: "待验收任务不可托管", status: "awaiting_review" }]);
    if (pathname === "/api/projects/p/supervision") return respond(created && (!session.archivedAt || url.searchParams.get("includeArchived") === "1") ? [session] : []);
    if (pathname === "/api/projects/q/supervision") return respond([]);
    if (pathname === "/api/supervision/s1") {
      const value = detail();
      if (delayDetail) { detailEntered(); await delayed; }
      return respond(value);
    }
    if (pathname.endsWith("/process")) {
      const after = Number(url.searchParams.get("afterMessage"));
      if (session.state === "draft") return respond({ messages: [], events: [], afterMessage: 0, afterEvent: 0, hasMore: false });
      if (after === 0) return respond({ messages: [{ cursor: 1, id: "m1", role: "assistant", content: JSON.stringify({ reason: "先处理可独立推进的加法任务", action: { type: "dispatch_implementation", instruction: "修复加法并保留已有断言" } }), agentRole: "scheduler", createdAt: "2026-11-03T01:01:00Z" }], events: [], afterMessage: 1, afterEvent: 0, hasMore: true });
      if (after === 1) return respond({ messages: [
        { cursor: 2, id: "m2", role: "assistant", content: JSON.stringify({ summary: "已完成加法修复", edits: [{ path: "src/math.cjs" }] }), taskId: "t1", agentRole: "implementation", createdAt: "2026-11-03T01:02:00Z" },
        ...[
          { summary: "错误类型的编辑保留待诊断", edits: "invalid-array" },
          { summary: "空编辑项保留待诊断", edits: [null] },
          { summary: { invalid: "object summary" }, action: { instruction: {} }, edits: [null] },
        ].map((content, index) => ({ cursor: index + 3, id: `invalid-${index}`, role: "assistant", content: JSON.stringify(content), taskId: "t1", agentRole: "implementation", createdAt: "2026-11-03T01:02:01Z" })),
      ], events: [{ cursor: 1, id: "v1", type: "verification.output", payload: { text: "behavior checked", stream: "stdout" }, taskId: "t1", agentRole: "test", createdAt: "2026-11-03T01:03:00Z" }, { cursor: 2, id: "runtime1", type: "supervision.runtime", payload: { protocolVersion: 1, targetId: "windows-local", cli: { agentId: "claude-code", path: "C:/tools/claude.exe", version: "test-cli-version", contract: "claude-controlled-input-v1" }, role: "scheduler", authentication: { source: "cli_managed" }, inputSha256: "synthetic-input-hash" }, taskId: "", agentRole: "scheduler", createdAt: "2026-11-03T01:00:30Z" }], afterMessage: 5, afterEvent: 2, hasMore: false });
      return respond({ messages: [], events: [], afterMessage: 5, afterEvent: 2, hasMore: false });
    }
    if (pathname.endsWith("/changes")) return respond({ baselineRevisionId: "before", currentRevisionId: "current", changes: [{ path: "src/math.cjs", beforeSha256: "old", afterSha256: "new", readable: true, beforeSize: 20, afterSize: 20 }] });
    if (pathname.endsWith("/source")) return route.fulfill({ body: url.searchParams.get("revisionId") === "before" ? "module.exports=(a,b)=>a-b;" : "module.exports=(a,b)=>a+b;", contentType: "text/plain" });
    if (pathname.endsWith("/screenshot.png")) return route.fulfill({ body: Buffer.from("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aRz8AAAAASUVORK5CYII=", "base64"), contentType: "image/png" });
    return respond({});
  });
  await page.goto(`${base}/projects/p/supervision?task=t-review&task=missing`, { waitUntil: "domcontentloaded", timeout: 60000 });
  await page.getByRole("heading", { name: "选择要完成的任务" }).waitFor();
  assert.equal(await page.locator(".supervision-task-picker").getByText("待验收任务不可托管", { exact: true }).count(), 0, "picker offered a task state rejected by the backend");
  assert(await page.getByRole("button", { name: "创建托管草稿（0 项）" }).isDisabled(), "ineligible URL selections enabled creation");
  const workspacePanel = page.locator(".supervision-workspace-operations");
  await workspacePanel.locator("summary").click();
  await workspacePanel.getByText(/退出状态待核对 ·/).waitFor();
  await workspacePanel.getByRole("button", { name: "核对目标交还状态", exact: true }).click();
  await workspacePanel.getByRole("alert").waitFor();
  assert.equal(workspaceOperations.length, 2, "unknown execution was force released by the UI");
  await workspacePanel.getByRole("button", { name: "重试交还", exact: true }).click();
  await workspacePanel.getByText(/已结束，等待交还 ·/).waitFor({ state: "detached" });
  await workspacePanel.getByText(/退出状态待核对 ·/).waitFor();
  assert.equal(workspaceOperations.length, 1, "confirmed release did not recover");
  await page.goto(`${base}/projects/p/supervision?task=t1&task=t-review&task=missing&task=t1`, { waitUntil: "domcontentloaded", timeout: 60000 });
  await page.getByText("已排除 2 项当前不可托管或已不存在的预选任务。", { exact: true }).waitFor();
  await page.getByRole("button", { name: "管理当前目标的托管认证档案" }).click();
  const profileDialog = page.getByRole("dialog", { name: "托管认证档案" });
  await profileDialog.waitFor();
  assert.equal(await profileDialog.getByText("另一个目标的档案", { exact: true }).count(), 0, "another target's profile appeared in the editor");
  await profileDialog.getByLabel("CLI", { exact: true }).selectOption("codex");
  await profileDialog.getByLabel("名称", { exact: true }).fill("当前目标的认证");
  await profileDialog.getByLabel("认证方式", { exact: true }).selectOption("api_key");
  await profileDialog.getByLabel("API Key", { exact: true }).fill("browser-test-private-value");
  await profileDialog.getByRole("button", { name: "保存档案", exact: true }).click();
  await profileDialog.getByText("当前目标的认证", { exact: true }).waitFor();
  assert.equal(profileWrites[0].projectId, "p", "profile creation lost its actual project destination");
  assert(!await profileDialog.textContent().then(text => text.includes("browser-test-private-value")), "saved credential echoed into the dialog");
  assert.equal(await profileDialog.locator('input[type="password"]').count(), 0, "saved credential remained in the form");
  await profileDialog.getByRole("button", { name: "编辑新版本", exact: true }).click();
  assert.equal(await profileDialog.getByLabel("API Key", { exact: true }).inputValue(), "", "edit fetched or retained the secret");
  await profileDialog.getByLabel("模型", { exact: true }).fill("updated-model");
  await profileDialog.getByRole("button", { name: "保存档案", exact: true }).click();
  await profileDialog.getByText("codex · updated-model · 可选择", { exact: true }).waitFor();
  assert(!Object.hasOwn(profileWrites[1].body, "apiKey"), "blank edit replaced the stored credential");
  await profileDialog.getByRole("button", { name: "停用新选择", exact: true }).click();
  await profileDialog.getByText("codex · updated-model · 已停用", { exact: true }).waitFor();
  await profileDialog.getByRole("button", { name: "吊销此版本并停止相关运行", exact: true }).click();
  await profileDialog.getByText("codex · updated-model · 已吊销", { exact: true }).waitFor();
  assert.equal(profileWrites.at(-1).path, "/api/supervision-profile-revisions/managed-v2/revoke", "revocation targeted a historical revision");
  await profileDialog.getByRole("button", { name: "关闭", exact: true }).click();
  assert.match(await page.locator(".supervision-create").textContent(), /项目执行环境：windows · windows-local/);
  assert.equal(await page.getByLabel("实施 Agent CLI", { exact: true }).locator("option").count(), 7, "CLI catalog was shortened");
  assert.equal(await page.getByLabel("实施 Agent CLI", { exact: true }).locator('option[value="codex"]').getAttribute("disabled"), "", "unavailable CLI option must be disabled");
  await page.getByLabel("实施 Agent CLI", { exact: true }).selectOption("codebuddy");
  assert.equal(await page.getByLabel("实施 Agent认证档案", { exact: true }).count(), 0, "tool without profiles offered a selector");
  assert.equal(await page.getByLabel("实施 Agent思考强度", { exact: true }).count(), 0, "tool inherited another CLI's efforts");
  await page.getByLabel("实施 Agent CLI", { exact: true }).selectOption("");
  await page.getByLabel("实施 Agent模型", { exact: true }).fill("opus");
  await page.getByLabel("独立审查 Agent认证档案", { exact: true }).selectOption("review-profile");
  await page.getByLabel("独立审查 Agent思考强度", { exact: true }).selectOption("medium");
  await page.getByRole("button", { name: "创建托管草稿（1 项）" }).click();
  assert.equal(writes[0].body.roles.implementation.model, "opus");
  assert.equal(writes[0].body.roles.review.profileId, "review-profile");
  assert.deepEqual(writes[0].body.taskIds, ["t1"]);
  await page.getByRole("button", { name: "启动托管" }).click();
  await page.locator(".supervision-wait").waitFor();
  assert.match(await page.locator(".supervision-wait").textContent(), /正在等待：等待测试环境准备.*下次复查：/);
  await page.getByText("先处理可独立推进的加法任务", { exact: true }).waitFor();
  await page.getByText("执行配置：claude-code · test-cli-version · windows-local", { exact: true }).waitFor();
  await page.getByText("认证来源：CLI 现有配置", { exact: true }).waitFor();
  await page.getByText("已完成加法修复", { exact: true }).waitFor();
  await page.getByText("错误类型的编辑保留待诊断", { exact: true }).waitFor();
  await page.getByText("空编辑项保留待诊断", { exact: true }).waitFor();
  await page.getByRole("tab", { name: "测试", exact: true }).click();
  await page.locator(".supervision-transcript pre").getByText("behavior checked", { exact: true }).waitFor();
  await page.getByRole("tab", { name: "全部过程" }).click();
  await page.getByLabel("回答 Agent").fill("包括负数，按现有断言验收");
  await page.getByRole("button", { name: "提交回答" }).click();
  await page.getByRole("button", { name: "提交回答" }).waitFor({ state: "detached" });
  await page.getByPlaceholder("补充目标、限制或发现的问题…").fill("保留用户已有修改");
  await page.getByRole("button", { name: "发送要求" }).click();
  await page.getByRole("alert").waitFor();
  assert.equal(await page.getByPlaceholder("补充目标、限制或发现的问题…").inputValue(), "保留用户已有修改");
  await page.getByRole("button", { name: "发送要求" }).click();
  await page.waitForFunction(() => document.querySelector(".supervision-instruction textarea")?.value === "");
  const instructionWrites = writes.filter(write => write.path.endsWith("/instructions"));
  assert.equal(instructionWrites.length, 2); assert.equal(instructionWrites[0].key, instructionWrites[1].key);
  assert.deepEqual(instructionWrites[0].body, instructionWrites[1].body); assert.equal(appliedInstructions, 1);
  assert(writes.some(write => write.path.endsWith("/instructions") && write.body.content === "保留用户已有修改" && write.body.immediate === false));
  await page.getByRole("button", { name: "暂停", exact: true }).click();
  await page.locator(".supervision-wait").waitFor({ state: "detached" });
  await page.getByRole("button", { name: "角色配置", exact: true }).click();
  await page.getByLabel("独立审查 Agent模型", { exact: true }).fill("sonnet");
  await page.locator(".supervision-role-fields").scrollIntoViewIfNeeded();
  assert(await page.locator(".supervision-role-fields fieldset").evaluateAll(fields => fields.every(field => field.scrollWidth <= field.clientWidth)), "role form has horizontal overflow");
  await page.screenshot({ path: path.join(os.tmpdir(), `milevia-supervision-roles-${mobile ? "mobile" : "desktop"}.png`) });
  await page.getByRole("button", { name: "保存角色配置" }).click();
  await page.getByRole("button", { name: "保存角色配置" }).waitFor({ state: "detached" });
  assert(writes.some(write => write.path.endsWith("/roles") && write.body.roles.review.model === "sonnet" && write.body.roles.implementation.model === "opus"));
  await page.getByRole("button", { name: "核对并继续" }).click();
  await page.getByRole("button", { name: "调整预算" }).click();
  await page.getByLabel("Agent 调用次数").fill("220");
  await page.getByRole("button", { name: "保存预算" }).click();
  await page.getByRole("button", { name: "保存预算" }).waitFor({ state: "detached" });
  await page.getByRole("button", { name: "比较启动基线与当前快照" }).click();
  await page.getByRole("button", { name: "src/math.cjs", exact: true }).click();
  await page.getByText("module.exports=(a,b)=>a+b;", { exact: true }).waitFor();
  await page.getByRole("button", { name: "截图", exact: true }).click();
  await page.getByRole("img", { name: "浏览器验收截图" }).waitFor();
  await page.getByRole("dialog").getByRole("button", { name: "关闭" }).click();
  await page.screenshot({ path: path.join(os.tmpdir(), `milevia-supervision-ui-${mobile ? "mobile" : "desktop"}.png`), fullPage: true });
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth + 1), false, "page overflows horizontally");
  assert.equal(await page.locator(".supervision-message").count(), 7, "process pagination lost or duplicated runtime, valid or invalid output entries");
  await page.getByRole("button", { name: "停止并交还任务" }).click();
  await page.getByRole("heading", { name: "托管结果：已停止" }).waitFor();
  await page.getByText("正在核对执行资源并交还工作区；交还完成前，目录仍受托管保护，暂不能归档或清理产物。", { exact: true }).waitFor();
  assert(await page.getByRole("button", { name: "归档会话", exact: true }).isDisabled(), "archive was enabled before release proof");
  assert(await page.getByRole("button", { name: "清理原始截图与报告文件（保留结构化结果）" }).isDisabled(), "cleanup was enabled before release proof");
  protectionReleased = true;
  await page.getByText("工作区已交还，可以继续其他操作。", { exact: true }).waitFor();
  await page.getByRole("button", { name: "清理原始截图与报告文件（保留结构化结果）" }).click();
  await page.getByText("原始产物已清理，结构化报告与哈希保留。", { exact: true }).waitFor();
  await page.getByRole("button", { name: "归档会话", exact: true }).click();
  await page.getByText("已归档，过程与证据保留。", { exact: true }).waitFor();
  await page.getByRole("checkbox", { name: "显示已归档" }).check();
  await page.getByRole("combobox", { name: "托管会话" }).locator("option").filter({ hasText: "已归档" }).waitFor({ state: "attached" });
  await page.locator("#workspace-panel-supervision").evaluate(element => { element.scrollTop = 0; });
  await page.screenshot({ path: path.join(os.tmpdir(), `milevia-supervision-archive-${mobile ? "mobile" : "desktop"}.png`), fullPage: true });
  await page.getByRole("button", { name: "取消归档", exact: true }).click();
  await page.getByRole("button", { name: "归档会话", exact: true }).waitFor();
  assert.equal(session.state, "stopped", "unarchive resumed stopped execution");
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth + 1), false, "archive actions overflow horizontally");
  session = { ...session, state: "failed", stateVersion: session.stateVersion + 1 };
  await page.getByRole("heading", { name: "托管结果：失败" }).waitFor();
  assert((await page.locator(".supervision-report details pre").textContent()).includes("session.failed"), "failure report lost its terminal event");
  delayDetail = true; await entered;
  await page.evaluate(() => window.switchSupervisionProject()); releaseDetail();
  await page.getByText("另一个项目的任务", { exact: true }).waitFor();
  assert.equal(await page.locator(".supervision-create select").first().inputValue(), "codex", "unavailable configured CLI silently fell back to another tool");
  assert.equal(await page.locator(".supervision-status").count(), 0, "old session response overwrote new project");
  assert.deepEqual(errors, []); await context.close();
}
try {
  browser = await chromium.launch({ headless: true });
  await verify(false); console.log("PASS: desktop create/start/process/pagination/control/instructions/budget/evidence/artifacts/diff/project-switch");
  await verify(true); console.log("PASS: mobile interactions and no horizontal overflow");
} finally { await browser?.close(); await server.close(); }
