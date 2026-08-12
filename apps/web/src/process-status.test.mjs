import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const provider = await readFile(new URL("./components/ProcessStatusProvider.tsx", import.meta.url), "utf8");
const dashboard = await readFile(new URL("./pages/DashboardPage.tsx", import.meta.url), "utf8");
const types = await readFile(new URL("./lib/types.ts", import.meta.url), "utf8");
const app = await readFile(new URL("./App.tsx", import.meta.url), "utf8");

// ProcessStatusProvider 暴露独立 hook 并独占进程状态（单一 owner 约束）
test("ProcessStatusProvider: 暴露 useProcessStatusMap hook 并持有独立 Context", () => {
  assert.match(provider, /export function useProcessStatusMap\(\)/);
  assert.match(provider, /ProcessStatusContext\.Provider/);
});

// REST 兜底端点与会话状态接口分离，注释明确单一 owner 无冲突
test("ProcessStatusProvider: REST 兜底走独立进程状态端点（与会话状态解耦）", () => {
  assert.match(provider, /\/api\/projects\/processes\/statuses/);
  assert.match(provider, /PROCESS_POLL_INTERVAL = 10_000/);
});

// WS 实时 + 自动重连（指数退避）
test("ProcessStatusProvider: WS /ws/processes 实时 + 指数退避重连", () => {
  assert.match(provider, /createWebSocket\("\/ws\/processes"\)/);
  assert.match(provider, /500 \* Math\.pow\(2, reconnectAttempts - 1\)/);
});

// 跨 Tab BroadcastChannel 同步
test("ProcessStatusProvider: BroadcastChannel 跨 Tab 转发", () => {
  assert.match(provider, /new BroadcastChannel\("app-process-status"\)/);
});

// 卡片渲染：非 stopped 状态才展示进程徽标，复用 statusLabels/statusColors
test("DashboardPage: 项目卡片渲染开发进程状态点徽标（stopped 隐藏）", () => {
  assert.match(dashboard, /runStatus !== "stopped"/);
  assert.match(dashboard, /statusLabels\[runStatus\]/);
  assert.match(dashboard, /statusColors\[runStatus\]/);
  assert.match(dashboard, /project-run-state run-state-\$\{runStatus\}/);
  assert.match(dashboard, /useProcessStatusMap\(\)/);
});

// 类型扩展：新增进程状态类型；会话状态 ProjectStatus 追加优化建议分析状态字段
test("types: 新增 RunStatus/ProjectProcessStatus/RunStatusEvent，ProjectStatus 含 insights 状态", () => {
  assert.match(types, /export type RunStatus = "stopped" \| "starting" \| "running" \| "stopping" \| "failed"/);
  assert.match(types, /export type ProjectProcessStatusMap = Record<string, ProjectProcessStatus>/);
  assert.match(types, /export type RunStatusEvent = \{ projectId: string; status: RunStatus/);
  assert.match(types, /export type ProjectStatus = \{ running: boolean; conversationCount: number; activeTitle: string; insightsRunning: boolean; insightsMessage: string \};/);
});

// App 根部挂载：Provider 树位于 NotificationProvider 与 ProjectProvider 之间
test("App: 根部挂载 ProcessStatusProvider（Notification → ProcessStatus → Project）", () => {
  assert.match(app, /<NotificationProvider>\s*\n\s*<ProcessStatusProvider>/);
  assert.match(app, /<ProcessStatusProvider>\s*\n\s*<ProjectProvider>/);
});

// 实时状态过期回退 REST（修复"运行中卡死残留态"）：
// 没有 runUpdatedAt 或缺省即视为过期，REST 兜底可覆盖。
test("ProcessStatusProvider: 实时状态过期后回退采纳 REST 兜底值", () => {
  assert.match(provider, /REST_STALE_AFTER_MS/);
  assert.match(provider, /live\.runUpdatedAt === undefined \|\| now - live\.runUpdatedAt > REST_STALE_AFTER_MS/);
});

// REST 轮询无实质变化时不触发重渲染（sameStatus 去重）。
test("ProcessStatusProvider: mergeEvent/REST 对无变化状态去重（不重渲染）", () => {
  assert.match(provider, /function mergeEvent\(/);
  assert.match(provider, /sameStatus\(next, existing\)\) return prev;/);
  assert.match(provider, /sameStatus\(live, next\[id\]\)/);
});

// stopped 状态显式清空 pid/startedAt（避免残留旧进程细节）。
test("ProcessStatusProvider: stopped 状态清空 pid/startedAt", () => {
  assert.match(provider, /if \(event\.status === "stopped"\)/);
  assert.match(provider, /delete next\.runPid;/);
  assert.match(provider, /delete next\.runStartedAt;/);
});
