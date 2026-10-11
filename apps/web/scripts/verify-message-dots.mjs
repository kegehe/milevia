// 方案A「用户消息圆点轨」实施后的真实渲染验证：
// mock 会话接口 → 桌面 1440 截图（默认 / 悬浮预览 / 点击跳转）→ 窄屏隐藏断言。
import { chromium } from "playwright";

const OUT = "D:/projects/Programs/milevia/outputs/对话页小圆点导航";
import { mkdirSync } from "node:fs";
mkdirSync(OUT, { recursive: true });

const BASE = "http://localhost:5199";
const now = Date.now();
const iso = (min) => new Date(now - min * 60_000).toISOString();

const project = {
  id: "p1", name: "milevia", pathDisplay: "milevia", fullPath: "D:\\projects\\milevia",
  runner: "local", environment: "windows", localRunner: true, gitBranch: "main",
  claudeReady: true, codexReady: true, agentReady: true,
};
const conversation = {
  id: "c1", status: "idle", agentId: "claude-code", agentSessionId: "s1", agentRuntimeId: "rt1",
  executionPolicy: "full_control", permissionMode: "full_control",
  title: "编辑器工具改造", preview: "", lastActivityAt: iso(1), isCurrent: true,
};
const messages = [
  { id: "m1", runId: "r1", role: "user", content: "把行高统一改成 1.6，正文颜色换成 #31594f", createdAt: iso(50) },
  { id: "m2", runId: "r1", role: "assistant", content: "已统一调整：正文 1.6 行高、#31594f。标题保持 1.4，代码块不受影响。", createdAt: iso(49) },
  { id: "m3", runId: "r2", role: "user", content: "导出 API 文档，先给我看目录结构", createdAt: iso(40) },
  { id: "m4", runId: "r2", role: "assistant", content: "目录结构如下：auth / conversation / runner / git 四个模块，详细接口清单已生成到 docs/api.md。", createdAt: iso(39) },
  { id: "m5", runId: "r3", role: "user", content: "这个卡片在窄屏下被裁掉了，帮我查一下原因", createdAt: iso(30) },
  { id: "m6", runId: "r3", role: "assistant", content: "查到了：容器写死了 320px 高度，820px 断点下没有解锁，改成 min-height 后恢复正常。", createdAt: iso(29) },
  { id: "m7", runId: "r4", role: "user", content: "跑一遍测试，把失败的用例整理成清单", createdAt: iso(20) },
  { id: "m8", runId: "r5", role: "user", content: "把方案 A 的间距从 16 调成 12，其他保持不变", createdAt: iso(10) },
];

async function json(route, body) {
  await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
}
const notFound = (route) => route.fulfill({ status: 404, contentType: "application/json", body: "{}" });

const browser = await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1440, height: 960 }, deviceScaleFactor: 2 });
// 注意：Playwright 路由按注册的逆序匹配 —— 兜底 404 必须最先注册，具体 mock 才能盖住它。
await page.route("**/api/**", notFound);
await page.route("**/ws/**", (route) => route.abort());
await page.route("**/api/projects", (route) => json(route, [project]));
await page.route("**/api/projects/p1", (route) => json(route, project));
await page.route("**/api/projects/p1/**", (route) => json(route, []));
await page.route("**/api/projects/p1/conversations/activity*", (route) => json(route, { conversations: [], missingConversationIds: [] }));
await page.route("**/api/projects/p1/conversations*", (route) => {
  if (route.request().method() === "POST") return json(route, conversation);
  return json(route, { items: [conversation], nextCursor: "" });
});
await page.route("**/api/conversations/c1/activate", (route) => json(route, conversation));
await page.route("**/api/conversations/c1/models", (route) => json(route, { conversationId: "c1", agentId: "claude-code", selected: "", effective: "", source: "cli_default", models: [], customAllowed: true }));
await page.route("**/api/conversations/c1/workspaces", (route) => json(route, []));
await page.route("**/api/conversations/c1/usage", (route) => json(route, { conversationId: "c1", available: false, context: {}, models: null }));
await page.route(/\/api\/conversations\/c1\?limit=1$/, (route) => json(route, { conversation, activeRunId: null }));
await page.route(/\/api\/conversations\/c1\?limit=400[^]*$/, (route) => json(route, { messages, events: [], activeRunId: null, hasMore: false, hasMoreMessages: false, nextCursor: "" }));
await page.route("**/api/projects/p1/shortcuts*", (route) => json(route, []));
await page.route("**/api/projects/p1/skills*", (route) => json(route, []));
await page.route("**/api/projects/p1/commands*", (route) => json(route, { projectId: "p1", agentId: "claude-code", env: "windows", source: "static", authoritative: false, commands: [], customAllowed: true }));
await page.route("**/api/projects/p1/input-history*", (route) => json(route, []));
await page.route("**/api/runners/*/agent-profiles", (route) => json(route, []));
await page.route("**/api/notifications", (route) => json(route, []));

await page.goto(`${BASE}/projects/p1/conversations/c1`);
if (process.env.DEBUG_API) {
  page.on("request", (req) => { if (req.url().includes("/api/")) console.log("REQ", req.method(), req.url()); });
  page.on("response", (res) => { if (res.url().includes("/api/")) console.log("RES", res.status(), res.url()); });
}
await page.waitForSelector(".message.user", { timeout: 20000 });
await page.waitForTimeout(800);

const failures = [];
const check = (name, ok, detail = "") => {
  if (!ok) failures.push(`${name} ${detail}`);
  console.log(`${ok ? "PASS" : "FAIL"} ${name}${detail ? " :: " + detail : ""}`);
};

// 1. 圆点数量 = 用户消息数量，轨道可见
const dotCount = await page.locator(".message-dot-btn").count();
check("圆点数量 = 用户消息数(5)", dotCount === 5, `实际 ${dotCount}`);
const railVisible = await page.locator(".message-dots").isVisible();
check("圆点轨可见", railVisible);

// 2. 初始加载（自动滚底）：当前点判据沿用「视口顶缘上方最近的一条」，恰有一枚高亮即可
const initialActive = await page.locator(".message-dot-btn.active").count();
check("初始恰有一枚当前点", initialActive === 1, `实际 ${initialActive}`);

// 3. 悬浮第 3 枚：预览卡出现，内容 = 序号 + 时间 + 消息文本
await page.locator(".message-dot-btn").nth(2).hover();
await page.waitForSelector(".message-dots-preview", { timeout: 5000 });
const previewMeta = await page.locator(".message-dots-preview-meta").innerText();
const previewBody = await page.locator(".message-dots-preview-body").innerText();
check("预览卡序号行", previewMeta.includes("第 3 条"), previewMeta);
check("预览卡正文 = 第 3 条用户消息", previewBody.includes("窄屏下被裁掉"), previewBody.slice(0, 30));
await page.screenshot({ path: `${OUT}/实施-悬浮预览.png`, clip: { x: 640, y: 60, width: 800, height: 900 } });

// 3b. 鼠标移到预览卡上：卡片必须保持打开（pointer-events 已恢复），点击预览卡也能跳转
await page.locator(".message-dots-preview").hover();
await page.waitForTimeout(300);
const previewStillOpen = await page.locator(".message-dots-preview").isVisible();
check("鼠标移入预览卡保持打开", previewStillOpen);
await page.locator(".message-dots-preview").click();
await page.waitForTimeout(900);
const thirdIsActiveAfterPreviewClick = await page.locator(".message-dot-btn").nth(2).evaluate((el) => el.classList.contains("active"));
check("点击预览卡跳到第 3 条", thirdIsActiveAfterPreviewClick);

// 4. 点击第 1 枚：跳转 + 当前点迁移
await page.locator(".message-dot-btn").nth(0).click();
await page.waitForTimeout(900);
const firstIsActive = await page.locator(".message-dot-btn").nth(0).evaluate((el) => el.classList.contains("active"));
const nearTop = await page.evaluate(() => {
  const timeline = document.querySelector(".timeline");
  return timeline.scrollTop < 200;
});
check("点击第 1 枚后当前点迁移", firstIsActive);
check("时间线滚到顶部附近", nearTop);
await page.screenshot({ path: `${OUT}/实施-点击跳转后.png`, clip: { x: 640, y: 60, width: 800, height: 900 } });

// 5. 默认状态整页截图（悬浮态收起后再来一张）
await page.locator(".message-dot-btn").nth(4).click();
await page.mouse.move(400, 300);
await page.waitForTimeout(600);
await page.screenshot({ path: `${OUT}/实施-默认状态.png`, fullPage: false });

// 6. 窄屏隐藏
await page.setViewportSize({ width: 480, height: 900 });
await page.waitForTimeout(500);
const narrowHidden = await page.locator(".message-dots-wrap").isHidden();
check("窄屏(480)圆点轨隐藏", narrowHidden);

await browser.close();
if (failures.length) {
  console.error("FAILURES:\n" + failures.join("\n"));
  process.exit(1);
}
console.log("all checks passed");
