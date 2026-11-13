import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

// 插件管理页的硬要求（docs/65）：
//   1. 读不到 ≠ 没有：列表的 error 与 ready-but-empty 必须分开渲染；
//   2. 状态未知 ≠ 已禁用：磁盘兜底记录只显示"状态未知"，不给启停按钮；
//   3. marketplace 待执行命令必须由用户确认命令与 sha256 后才重试安装。
// 这些都属于"合并了也不会报错，但用户会做错事"的类型，所以用源码断言钉住。
const page = await readFile(new URL("./pages/PluginsPage.tsx", import.meta.url), "utf8");
const app = await readFile(new URL("./App.tsx", import.meta.url), "utf8");
const dashboard = await readFile(new URL("./pages/DashboardPage.tsx", import.meta.url), "utf8");
const conversation = await readFile(new URL("./pages/ConversationPage.tsx", import.meta.url), "utf8");
const api = await readFile(new URL("./lib/api.ts", import.meta.url), "utf8");

function stripComments(source) {
  return source.replace(/\/\*[\s\S]*?\*\//g, "").replace(/^\s*\/\/.*$/gm, "");
}

const code = stripComments(page);

test("插件页有独立路由与首页入口", () => {
  assert.match(app, /<Route path="\/plugins" element=\{<PluginsPage \/>\} \/>/);
  assert.match(dashboard, /title="插件" aria-label="插件" onClick=\{\(\) => navigate\("\/plugins"\)\}/);
  assert.match(dashboard, /<span className="action-label">插件<\/span>/);
});

test("工具清单来自服务端目录，页面不写死插件工具 ID", () => {
  assert.match(code, /\/api\/plugin-agents/);
  for (const forbidden of ['"claude-code"', '"codex"', '"qoder"']) {
    assert.ok(!code.includes(forbidden), `页面里出现了写死的工具 ID ${forbidden}`);
  }
});

test("读不到 ≠ 没有：列表错误与空态分开渲染", () => {
  assert.match(code, /viewState === "error"/);
  assert.match(code, /读不到插件列表/);
  assert.match(code, /view\.plugins\.length === 0/);
  assert.match(code, /还没有安装插件/);
  // 目录 / 执行环境也各有独立错误态。
  assert.match(code, /agentsState === "error"/);
  assert.match(code, /runnersState === "error"/);
});

test("状态未知 ≠ 已禁用：未知状态不给启停按钮", () => {
  assert.match(code, /record\.stateKnown \?/);
  assert.match(code, /data-tone="unknown">状态未知/);
  assert.match(code, /const toggleable = record\.stateKnown && record\.canDisable/);
  assert.match(code, /data-disabled=\{record\.stateKnown && !record\.enabled \? "true" : undefined\}/);
});

test("marketplace 命令必须显式确认后才带 sha256 重试", () => {
  assert.match(code, /result\.needsConfirmation/);
  assert.match(code, /installConfirm\.command/);
  assert.match(code, /installConfirm\.commandSha256/);
  assert.match(code, /acceptCommandSha256/);
  assert.match(code, /确认执行并安装/);
  // 反面：页面自己不许拼 -y / --yes。
  assert.ok(!code.includes("-y") && !code.includes("--yes"));
});

test("项目范围必须选项目，用户范围不需要", () => {
  assert.match(code, /installScope !== "user" && !installProject/);
  assert.match(code, /项目范围操作需要选择一个项目|项目范围需要先选择一个项目/);
  assert.match(code, /marketScope !== "user" && !marketProject/);
});

test("插件写操作按服务端动作路径调用，卸载走 DELETE", () => {
  assert.match(code, /next \? "enable" : "disable"/);
  assert.match(code, /mutationPath\(record, "update"\)/);
  assert.match(code, /method: "DELETE"/);
  assert.match(code, /plugin-marketplaces/);
  // CLI 省略 --scope 时会删除所有范围里的同名声明，确认框必须说清楚。
  assert.match(code, /移除会删除同名市场在所有范围/);
});

test("插件列表接口给足探测/写操作预算", () => {
  assert.ok(api.includes("/plugins"), "api.ts 应当为插件接口登记长任务预算");
  assert.ok(api.includes("installTimeoutMs"), "插件写操作应当走安装级预算");
});

test("对话页插件块接真实启用状态，并提供管理入口", () => {
  assert.match(conversation, /stateKnown\?:boolean/);
  assert.match(conversation, /plugin\.stateKnown \? "状态未知"/);
  assert.match(conversation, /navigate\("\/plugins"\)/);
  assert.match(conversation, /管理插件/);
});

test("同一插件多范围安装：列表 key 与操作定位都带 scope/project", () => {
  // 同一 ID 可以同时装在 user 与 project；只用 id 做 key 会让 React 复用错卡片。
  assert.match(code, /const recordKey = \(record: PluginRecord\) => record\.key \|\| record\.id/);
  assert.match(code, /key=\{recordKey\(record\)\}/);
  assert.match(code, /recordProjectPath/);
  assert.match(code, /params\.set\("scope", record\.scope\)/);
  assert.match(code, /params\.set\("recordProjectPath", record\.projectPath\)/);
  assert.match(code, /recordProjectPath: record\.projectPath \|\| ""/);
  assert.match(code, /params\.set\("recordProjectPath", uninstallTarget\.projectPath\)/);
});

test("范围选项以服务端能力位为准，不显示 CLI 不支持的范围", () => {
  assert.match(code, /scopes\?: string\[\]/);
  assert.match(code, /const supportedScopes =/);
  assert.match(code, /supportedScopes\.map\(\(scope\) =>/);
  assert.ok(!code.includes('<option value="project">'), "项目范围不能写死成永远可选");
});

test("升级遇到待执行命令时同样必须确认，不能静默结束", () => {
  assert.match(code, /updateConfirm/);
  assert.match(code, /if \(result\?\.needsConfirmation\) setUpdateConfirm/);
  assert.match(code, /acceptCommandSha256: result\.commandSha256/);
  assert.match(code, /确认执行并升级/);
  // 确认期间命令又变了，不能用旧 sha 强行结束。
  assert.match(code, /if \(retried\?\.needsConfirmation\) setUpdateConfirm/);
});

test("切换执行环境或工具时关闭旧环境的对话框并作废旧请求", () => {
  assert.match(code, /setInstallTarget\(null\);\n\s+setInstallConfirm\(null\);\n\s+setUninstallTarget\(null\);/);
  assert.match(code, /detailsVersion\.current\+\+/);
  assert.match(code, /availableVersion\.current\+\+/);
  assert.match(code, /if \(version !== detailsVersion\.current\) return;/);
  assert.match(code, /if \(version !== availableVersion\.current\) return;/);
});
