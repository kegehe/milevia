import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

// 方案 A「图标化降噪」：对话页 .head-actions 与总览页 .dashboard-actions 的头部
// 按钮排统一降到 34px 档，次要操作在桌面端只露图标。文字保留在 DOM（.action-label）
// 里 —— 对话页供 ≤820px 的「操作」菜单显示，总览页供 ≤820px 窄屏显示；
// display:none 的文字不进无障碍树，所以图标钮必须自带 aria-label。
const conversationPage = await readFile(new URL("./pages/ConversationPage.tsx", import.meta.url), "utf8");
const conversationStyles = await readFile(new URL("./conversation.css", import.meta.url), "utf8");
const dashboardPage = await readFile(new URL("./pages/DashboardPage.tsx", import.meta.url), "utf8");
const globalStyles = await readFile(new URL("./style.css", import.meta.url), "utf8");
const notificationStyles = await readFile(new URL("./notification.css", import.meta.url), "utf8");

test("对话页次要操作收成图标方钮，文字留给窄屏菜单与读屏", () => {
  // 历史 / 新建工作区 / 移除工作区 / AI 配置 四个次要操作都挂 .icon-action。
  assert.match(conversationPage, /className="conversation-head-action secondary icon-action"[^>]*onClick=\{openConversationHistory\}[^>]*title="历史" aria-label="会话历史"/);
  assert.match(conversationPage, /className="conversation-head-action secondary workspace-create-action icon-action"/);
  assert.match(conversationPage, /className="conversation-head-action secondary workspace-remove-action icon-action"/);
  assert.match(conversationPage, /className="conversation-head-action secondary icon-action"[^>]*onClick=\{openAiConfig\}[^>]*title="AI 配置" aria-label="AI 配置"/);
  // 文字都在 .action-label 里（display:none 后不进无障碍树，读屏靠 aria-label）。
  assert.match(conversationPage, /<HistoryIcon \/><span className="action-label">历史<\/span>/);
  assert.match(conversationPage, /<ProjectConfigIcon \/><span className="action-label">AI 配置<\/span>/);
  assert.match(conversationPage, /<WorkspaceCreateIcon \/><span className="action-label">\{workspaceBusy \? "处理中" : "新建工作区"\}<\/span>/);
  assert.match(conversationPage, /<WorkspaceRemoveIcon \/><span className="action-label">移除工作区<\/span>/);
  // 新会话是「新建对话」不是「新建目录」：主按钮用加号，文件夹加号让给新建工作区。
  assert.match(conversationPage, /<PlusIcon \/><span>新会话<\/span>/);
  assert.doesNotMatch(conversationPage, /<NewConversationIcon \/>/);
  // 工作区紧凑芯片带文件夹图标。
  assert.match(conversationPage, /<label className="conversation-workspace-picker"><WorkspaceFolderIcon \/><span>工作区<\/span><select/);
});

test("conversation.css 承载 34px 档与图标方钮判据", () => {
  assert.match(conversationStyles, /\.permission-trigger,\s*\.conversation-head-action \{ display: inline-flex; min-height: 34px;[^}]*padding: 0 12px;/s);
  assert.match(conversationStyles, /\.conversation-head-action\.icon-action \{ width: 34px; min-width: 34px; padding: 0; \}/);
  assert.match(conversationStyles, /\.conversation-head-action \.action-label \{ display: none; \}/);
  // ≤820px「操作」菜单：文字恢复显示，34px 方钮约束显式解除（min-width 归零）。
  assert.match(conversationStyles, /\.head-actions-menu \.head-actions \.action-label \{ display: inline; \}/);
  assert.match(conversationStyles, /\.head-actions-menu \.conversation-head-action\.icon-action \{ width: auto; min-width: 0; justify-content: flex-start; \}/);
  // 工作区芯片降到 34px 档，select 高度随行收窄。
  assert.match(conversationStyles, /\.project-head \.conversation-workspace-picker \{ position: relative; min-height: 34px;[^}]*padding: 3px 6px 3px 9px;/s);
  assert.match(conversationStyles, /\.project-head \.conversation-workspace-picker select \{ min-width: 118px; max-width: 170px; height: 26px;/);
});

test("总览页与对话页共用同一套 34px 图标化规范", () => {
  // 四个连接管理入口挂 .action-label + aria-label（设置本就是图标钮）。
  assert.match(dashboardPage, /<RemotePairingIcon \/><span className="action-label">远程控制<\/span>/);
  assert.match(dashboardPage, /<SshConnectionIcon \/><span className="action-label">SSH连接<\/span>/);
  assert.match(dashboardPage, /<McpIcon \/><span className="action-label">MCP连接<\/span>/);
  assert.match(dashboardPage, /<CliToolsIcon \/><span className="action-label">Cli管理<\/span>/);
  assert.match(dashboardPage, /title="SSH连接" aria-label="SSH连接"/);
  assert.match(dashboardPage, /title="MCP连接" aria-label="MCP连接"/);
  assert.match(dashboardPage, /title="Cli管理" aria-label="Cli管理"/);
  // 主按钮「加载项目」保留文字，不挂 action-label。
  assert.match(dashboardPage, /<ImportProjectIcon \/><span>加载项目<\/span>/);
  // style.css：34px 档 + 桌面端（>820px）收起标签；≤820px 触屏显示（无悬停可依赖）。
  assert.match(globalStyles, /\.dashboard-actions \.dashboard-action \{ display: inline-flex; min-height: 34px;[^}]*padding: 0 12px;/s);
  assert.match(globalStyles, /\.dashboard-actions \.dashboard-action-settings \{ width: 34px; min-width: 34px; padding: 0; \}/);
  assert.match(globalStyles, /@media \(min-width: 821px\) \{ \.dashboard-actions \.dashboard-action \.action-label \{ display: none; \} \}/);
});

test("通知铃铛与按钮排同档（34px），角标悬出位置不变", () => {
  assert.match(notificationStyles, /\.notification-bell \{[^}]*width: 34px;[^}]*height: 34px;/s);
  assert.match(notificationStyles, /\.notification-bell-icon \{[^}]*width: 16px;[^}]*height: 16px;/s);
  assert.match(notificationStyles, /\.notification-badge \{\s*position: absolute;\s*top: -4px;\s*right: -4px;/s);
});
