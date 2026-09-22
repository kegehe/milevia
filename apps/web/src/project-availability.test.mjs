import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const [projectStore, dashboardPage] = await Promise.all([
  readFile(new URL("./stores/useProjectStore.tsx", import.meta.url), "utf8"),
  readFile(new URL("./pages/DashboardPage.tsx", import.meta.url), "utf8"),
]);
const importProjectPage = await readFile(new URL("./pages/ImportProjectPage.tsx", import.meta.url), "utf8");

test("项目列表刷新不等远端连通性探测", () => {
  // /api/projects 已不再同步探活，codex/agent 就绪度改由独立接口提供。
  assert.match(projectStore, /apiFn<ProjectAvailability\[\]>\("\/api\/projects\/availability", undefined, 0\)/);
  // 探活不阻塞列表：先落地列表数据，再以 void 调用异步合并探测结果。
  assert.match(projectStore, /void refreshAvailability\(\);/);
  assert.doesNotMatch(projectStore, /await refreshAvailability\(\)/);
  // 列表接口返回的就绪度只是落库值，必须保留上一次已探明的结果，避免卡片在
  // "已就绪/不可用"之间闪动。
  assert.match(projectStore, /codexReady: previous\.codexReady, agentReady: project\.claudeReady \|\| previous\.codexReady/);
  // 探测结果按项目 id 合并，删除掉的项目不会被探测响应重新带回。
  assert.match(projectStore, /const byId = new Map\(items\.map\(\(item\) => \[item\.id, item\]\)\);/);
  assert.match(projectStore, /const next = current\.map\(\(project\) => \{[\s\S]*?byId\.get\(project\.id\)/);
});

test("加载项目不再被环境就绪度阻塞（空目录/无环境也能加载）", () => {
  // 「确认加载」按钮不再因为 agentReady=false 禁用，只由正常加载状态控制。
  assert.doesNotMatch(importProjectPage, /disabled=\{!result\?\.agentReady/);
  assert.match(importProjectPage, /disabled=\{busy \|\| loadingDirectory \|\| !directoryReady \|\| !result\}/);
  // 环境不可用时不阻断，改为提示「仍可加载」。
  assert.match(importProjectPage, /环境不可用，仍可加载；新建会话时可安装或升级 CLI 工具/);
  assert.match(importProjectPage, /仍可加载/);
});

test("项目总览页保持事件驱动刷新 + 30 秒兜底轮询", () => {
  assert.match(dashboardPage, /useLiveStateEventsFor\("projects", undefined, onRealtime\)/);
  assert.match(dashboardPage, /useLiveStateEventsFor\("all", undefined, onRealtime\)/);
  assert.match(dashboardPage, /30_000/);
});

test("项目刷新走并发合并，WS 事件风暴不会占满浏览器连接池", () => {
  // 三个刷新入口都必须经 useCoalescedRefresh：同一条状态事件密集到达时只保留一个在飞请求
  // + 一次尾部补刷，否则点击项目的那一发请求会排在队尾直到超时。
  assert.match(projectStore, /import \{ coalesceRefresh \} from "\.\.\/lib\/refresh-coalesce";/);
  assert.match(projectStore, /const refreshStatuses = useCoalescedRefresh\(async \(\) => \{/);
  assert.match(projectStore, /const refreshAvailability = useCoalescedRefresh\(async \(\) => \{/);
  assert.match(projectStore, /const refreshProjects = useCoalescedRefresh\(async \(\) => \{/);
  assert.doesNotMatch(projectStore, /const refreshProjects = useCallback\(/);
});
