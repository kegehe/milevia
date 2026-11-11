// 真实手机面板 + 实际 REST→RPC 适配器；目标电脑回包由可控夹具提供。
import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import { chromium } from "playwright";
import { createServer } from "vite";

const probeID = "\0mobile-insights-probe";
const entry = `
import { createElement as h } from "react";
import { createRoot } from "react-dom/client";
import { MobileOptimizationPanel } from "/src/features/optimization/MobileOptimizationPanel.tsx";
import { createMobileInsightsRequest } from "/src/features/insights/mobile-insights-request.ts";
const mode = new URLSearchParams(location.search).get("mode");
const now = new Date().toISOString();
const finding = { id: "00000000-0000-0000-0000-000000000001", projectId: "p", scanId: "s", type: "bug", severity: "high", title: "回归建议", summary: "检查真实交互", status: "open", createdAt: now };
const state = window.__insights = { calls: [], errors: [], scan: null };
if (mode === "linked") {
  state.scan = { id: "s", projectId: "p", status: "completed", findingsCount: 1, suppressedCount: 0, createdAt: now };
  finding.linkedTaskStatus = "running";
  finding.linkedTaskTitle = "关联任务";
  state.finishLinkedTask = () => { finding.linkedTaskStatus = "done"; };
}
const entries = [
  { id: "claude-code", name: "Claude Code", supportsProjectInsights: true },
  { id: "codex", name: "Codex", supportsProjectInsights: true },
  { id: "qoder", name: "Qoder CLI", supportsProjectInsights: true, projectInsightsLocalOnly: true },
  { id: "codebuddy", name: "CodeBuddy", supportsProjectInsights: false },
];
const adapter = createMobileInsightsRequest({ transport: async (op, params, timeoutMs) => {
  state.calls.push({ op, params, timeoutMs });
  let data = {};
  if (op === "insights.engines") {
    await new Promise(resolve => { state.releaseEngines = resolve; });
    if (mode === "error") return { ok: false, status: 503, error: "目标电脑状态读取失败" };
    data = { entries, runner: { id: "target", local: mode !== "ssh", agents: entries.map(entry => ({ id: entry.id, status: mode === "unavailable" || entry.id === "codex" ? "unavailable" : "ready" })) } };
  } else if (op === "insights.list") {
    data = { defaultAgent: "codex", scan: state.scan, findings: [finding], events: [], hasScan: !!state.scan, suppressedCount: 0, openCount: 1 };
  } else if (op === "insights.scan") {
    state.scan = { id: "s", projectId: "p", status: "running", findingsCount: 0, suppressedCount: 0, createdAt: now };
  } else if (op === "insights.cancel") {
    state.scan.status = "cancelled";
  } else if (op === "insights.verify") {
    finding.verificationResult = "valid";
  } else throw new Error("unexpected operation: " + op);
  return { ok: true, status: 200, data: structuredClone(data) };
} });
createRoot(document.getElementById("root")).render(h(MobileOptimizationPanel, {
  project: { id: "p", name: "目标项目", runner: "target" }, defaultAgentID: "codex",
  request: adapter.request, fail: message => state.errors.push(message), onClose() {}, onAgentChange() {},
}));
`;

const server = await createServer({
  configFile: fileURLToPath(new URL("../vite.config.ts", import.meta.url)),
  cacheDir: fileURLToPath(new URL("../node_modules/.vite-mobile-insights-test", import.meta.url)),
  server: { host: "127.0.0.1", port: 0, hmr: false, ws: false },
  plugins: [{
    name: "mobile-insights-probe",
    resolveId(id) { if (id === "/mobile-insights-probe.js" || id === probeID) return probeID; },
    load(id) { if (id === probeID) return entry; },
    configureServer(vite) {
      vite.middlewares.use((req, res, next) => {
        if (!req.url?.startsWith("/__mobile_insights")) return next();
        res.setHeader("Content-Type", "text/html; charset=utf-8");
        res.end('<div id="root"></div><script type="module" src="/mobile-insights-probe.js"></script>');
      });
    },
  }],
});

let browser;
try {
  await server.listen();
  browser = await chromium.launch({ headless: true });
  const forbidden = [];
  const pageErrors = [];
  for (const mode of ["local", "ssh", "linked", "unavailable", "error"]) {
    const page = await browser.newPage({ viewport: { width: 412, height: 915 } });
    page.on("pageerror", error => pageErrors.push(error.message));
    await page.route("**/api/**", route => {
      forbidden.push(route.request().url());
      return route.abort();
    });
    await page.goto(`http://127.0.0.1:${server.httpServer.address().port}/__mobile_insights?mode=${mode}`);
    await page.waitForFunction(() => typeof window.__insights?.releaseEngines === "function");
    const start = page.getByRole("button", { name: "开始分析", exact: true });
    const verifyAll = page.getByRole("button", { name: "验证全部建议", exact: true });
    assert.equal(await start.isDisabled(), true, "探测完成前不能发起分析");
    await page.evaluate(() => window.__insights.releaseEngines());
    if (mode === "error" || mode === "unavailable") {
      await page.getByText(mode === "error" ? "目标电脑状态读取失败" : "当前环境没有可用的分析引擎", { exact: true }).waitFor();
      assert.equal(await start.isDisabled(), true);
      assert.equal(await verifyAll.isDisabled(), true);
      assert.equal(await page.getByRole("radio").count(), 0);
    } else {
      await page.locator('input[name="insight-agent"][value="claude-code"]').waitFor({ state: "attached" });
      assert.equal(await page.getByRole("radio").count(), mode === "ssh" ? 1 : 2);
      assert.equal(await page.locator('input[value="codex"]').count(), 0, "不可用工具不出现");
      assert.equal(await page.locator('input[value="codebuddy"]').count(), 0, "不支持分析的工具不出现");
      if (mode === "linked") {
        const convert = page.getByRole("button", { name: "添加到任务", exact: true });
        assert.equal(await convert.isDisabled(), true);
        await page.evaluate(() => window.__insights.finishLinkedTask());
        await page.waitForFunction(() => [...document.querySelectorAll("button")].some(button => button.textContent.trim() === "添加到任务" && !button.disabled), undefined, { timeout: 7000 });
      }
      await start.click();
      await page.getByRole("button", { name: "停止分析", exact: true }).click();
      await verifyAll.click();
      await page.getByRole("button", { name: "重新验证", exact: true }).click();
      await page.waitForFunction(() => window.__insights.calls.filter(call => call.op === "insights.verify").length === 2);
      const { calls, errors } = await page.evaluate(() => window.__insights);
      assert.deepEqual(errors, []);
      for (const op of ["insights.engines", "insights.scan", "insights.verify"]) {
        assert.ok(calls.filter(call => call.op === op).every(call => call.timeoutMs === 60_000));
      }
      assert.equal(calls.find(call => call.op === "insights.scan").params.agent, "claude-code");
      assert.deepEqual(calls.filter(call => call.op === "insights.verify").map(call => call.params.findingIds), [[], ["00000000-0000-0000-0000-000000000001"]]);
    }
    console.log(`PASS mobile insights: ${mode}`);
    await page.close();
  }
  assert.deepEqual(forbidden, [], "手机面板不得依赖同源桌面 API");
  assert.deepEqual(pageErrors, []);
} finally {
  await browser?.close();
  await server.close();
}
