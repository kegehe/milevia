import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

// 「一键连接」这条主路径在桌面端的回归测试（2026-09-15）。
//
// **分工**：凡是「结果取决于优先级 / 顺序」的纯逻辑（解析、凭据提示、向导该走哪一屏、
// 默认值怎么给）都在 `features/mcp/mcp-model.test.ts` 里做行为断言；本文件只守**页面接线**
// 与**文案**这类扫源码才验得到的东西（谁 import 谁、先调哪个接口、界面上还留没留协议名词）。
//
// 这个分工是被变异检验逼出来的：最初把「模板依赖优先于表单命令」也写成源码正则，
// 结果把优先级改反照样绿 —— 文本里 `requires` 与 `return fromPreset` 都还在。

const [page, types, styles] = await Promise.all([
  readFile(new URL("./pages/McpManagerPage.tsx", import.meta.url), "utf8"),
  readFile(new URL("./lib/types.ts", import.meta.url), "utf8"),
  readFile(new URL("./style.css", import.meta.url), "utf8"),
]);

// sliceBetween 取出一段函数体 / 一段 JSX。
//
// 断言必须限定在被测片段内部：直接用 /starter[\s\S]*?expect/ 会一路匹配到后面的其它函数，
// 把「这里没有」误判成「有」—— 那样测试在变异检验里挡不住任何东西。
function sliceBetween(source, startMarker, endMarker) {
  const start = source.indexOf(startMarker);
  assert.ok(start >= 0, `未找到起点 ${startMarker}`);
  const end = source.indexOf(endMarker, start + startMarker.length);
  assert.ok(end > start, `未找到终点 ${endMarker}`);
  return source.slice(start, end);
}

const openWizard = sliceBetween(page, "const openWizard = ", "const closeWizard = ");
const closeWizard = sliceBetween(page, "const closeWizard = ", "const wizardDraftPayload = ");
const wizardDraftPayload = sliceBetween(page, "const wizardDraftPayload = ", "const runWizardCheck = ");
const runWizardCheck = sliceBetween(page, "const runWizardCheck = ", "const startWizardOAuth = ");
const startWizardOAuth = sliceBetween(page, "const startWizardOAuth = ", "const finishWizard = ");
const finishWizard = sliceBetween(page, "const finishWizard = ", "const performSave = ");
const startEdit = sliceBetween(page, "const startEdit = ", "const closeForm = ");
const closeForm = sliceBetween(page, "const closeForm = ", "const submit = ");
const toolbar = sliceBetween(page, 'className="ssh-manager-toolbar"', "{localError &&");
const advancedBlock = sliceBetween(page, "{showAdvanced && <div", "<h3>项目视图</h3>");
const wizardJsx = sliceBetween(page, "{wizard && <div", "{showForm && <div");
const connectedBlock = sliceBetween(page, "<h3>已连接</h3>", "<h3>可以连接的服务</h3>");

test("主路径只有一条：点目录卡片进向导，其余能力收进「高级设置」", () => {
  assert.match(page, /const \[showAdvanced, setShowAdvanced\] = useState\(false\);/);
  assert.match(page, /onClick=\{\(\) => openWizard\(preset\)\}/);
  assert.match(page, /\{showAdvanced && <div className="ssh-form-section mcp-advanced">/);

  // 工具栏只留「高级设置」这一个开关：手动配置 / 导入 / 审计全部收进高级区。
  assert.match(toolbar, /高级设置/);
  assert.doesNotMatch(toolbar, /手动配置|从现有配置导入|调用审计/);
  assert.match(advancedBlock, /手动配置/);
  assert.match(advancedBlock, /从现有配置导入/);
  assert.match(advancedBlock, /调用审计/);
  // 目录卡片必须带「要准备什么」这一行 —— 那是用户决定点不点它的唯一依据。
  assert.match(page, /\{presetBadges\(preset\)\.map/);
});

test("向导三屏：起点由模型层决定，界面上不留协议名词", () => {
  assert.match(page, /const \[wizardStep, setWizardStep\] = useState<"credential" \| "check" \| "done">\("credential"\);/);
  assert.match(openWizard, /setWizardStep\(wizardStartsAt\(preset\)\);/);
  assert.match(page, /<ol className="mcp-wizard-steps">/);
  // 用户不懂 MCP：向导里不该出现这些词。
  for (const word of ["传输类型", "作用域", "适用环境", "占位符", "stdio", "http", "npx"]) {
    assert.ok(!wizardJsx.includes(word), `向导里出现了协议名词：${word}`);
  }
});

test("创建体走模型层：默认值全给上，页面自己拼不出第二个版本", () => {
  assert.match(wizardDraftPayload, /buildDraftServerPayload\(/);
  assert.match(wizardDraftPayload, /ENVIRONMENT_OPTIONS\.map\(\(option\) => option\.id\)/);
  assert.match(wizardDraftPayload, /\["claude-code"\]/);
  // OAuth 路径与最终保存必须共用同一个创建体，否则两条路的默认值会漂移。
  assert.match(startWizardOAuth, /JSON\.stringify\(wizardDraftPayload\(\)\)/);
  assert.match(finishWizard, /JSON\.stringify\(wizardDraftPayload\(\)\)/);
});

test("向导先查依赖、再试连，且试连发生在保存之前", () => {
  // 依赖清单必须真的来自条目的 requires（把它过滤成空数组就等于悄悄跳过了这一步）。
  assert.match(runWizardCheck, /const commands = \(wizard\.requires \|\| \[\]\)\.map\(\(item\) => item\.command\);/);
  const runtimeAt = runWizardCheck.indexOf("/api/mcp/runtime-check");
  const draftAt = runWizardCheck.indexOf("/api/mcp/test-draft");
  assert.ok(runtimeAt > 0, "向导应先做运行时依赖检查");
  assert.ok(draftAt > 0, "向导应做草稿态试连");
  // 顺序不能反：缺运行环境时直接试连，用户只会拿到一条难懂的错误。
  // 这是**位置**断言（流程里带网络调用，抽不成纯函数），够挡住「把两段调换」这类回归。
  assert.ok(runtimeAt < draftAt, "必须先查依赖再试连");
  // 缺依赖时提前返回，避免把「装东西」和「凭据不对」两件事混成一条错误。
  assert.match(runWizardCheck, /setWizardError\("这台电脑还缺运行环境[\s\S]{0,40}?return;/);
});

test("草稿试连必须带上凭据，且与创建请求共用同一份整理逻辑", () => {
  // 试连不落库，凭据只能靠 env / headers 带过去 —— 少了它，正确的 token 也会报「连不上」。
  assert.match(runWizardCheck, /\.\.\.draftProbeValues\(wizard, wizardSecrets\), environment: ""/);
  // 前缀 / 落点只许有一份实现：两处都从模型层取，避免某处忘了补 Bearer。
  assert.match(page, /import \{[^}]*draftProbeValues[^}]*\} from "\.\.\/features\/mcp\/mcp-model";/);
});

test("OAuth 路径先落库再授权，并用已落库的测试接口验证", () => {
  // OAuth 回调要按 serverID 存令牌，所以这条路径必须先把 server 建出来。
  assert.match(startWizardOAuth, /await api<MCPServer>\("\/api\/mcp\/servers", \{ method: "POST"/);
  assert.match(startWizardOAuth, /\/oauth\/start/);
  // 验证只能走已落库的接口：草稿态试连拿不到服务端保存的令牌。
  assert.match(startWizardOAuth, /`\/api\/mcp\/servers\/\$\{created\.id\}\/test`/);
  assert.doesNotMatch(startWizardOAuth, /test-draft/);
  // 先落库再取消，不能假装什么都没发生。
  // **必须切片断言**：整文件匹配 `if (wizardServer) {` 会被 finishWizard 里那处满足，
  // 删掉这里的代码也照样绿（P12 变异实测漏网过一次）。
  assert.match(closeWizard, /if \(wizardServer\) \{[\s\S]*?toast\.message\(`「\$\{wizardServer\.displayName/);
});

test("「完成」才写库，且「以后不用再问」只落到白名单", () => {
  assert.match(finishWizard, /await api\("\/api\/mcp\/servers", \{ method: "POST"/);
  assert.match(finishWizard, /\/auto-approve`/);
  assert.match(finishWizard, /patterns: \[`mcp__\$\{wizardServer\.name\}__\*`\]/);
  // 已落库的路径不该重复创建。
  assert.match(finishWizard, /if \(wizardServer\) \{[\s\S]*?\} else \{/);
});

test("已连接的卡片按「服务」呈现，不再暴露传输类型与环境矩阵", () => {
  assert.match(connectedBlock, /PresetIcon name=\{presetIconKey\(server\.name, presets\)\}/);
  assert.doesNotMatch(connectedBlock, /环境：\{server\.environments\.join/);
  assert.doesNotMatch(connectedBlock, /\{server\.transport\} · /);
});

test("解析与凭据提示共用模型层，页面里不留第二份实现", () => {
  assert.match(page, /import \{[^}]*presetGuidanceLines[^}]*\} from "\.\.\/features\/mcp\/mcp-model";/);
  assert.match(page, /import \{[^}]*parseKeyValueLines[^}]*\} from "\.\.\/features\/mcp\/mcp-model";/);
  assert.doesNotMatch(page, /function parseKeyValueLines/);
  assert.doesNotMatch(page, /function presetGuidanceLines/);
  assert.match(page, /const runtimeCommands = \(\): string\[\] => runtimeCommandsFor\(presetMeta, form\.transport, form\.command\);/);
});

test("模板要求只在来自模板时展示，编辑既有 server 时清掉", () => {
  assert.match(page, /const \[presetMeta, setPresetMeta\] = useState<MCPPreset \| null>\(null\);/);
  const startCreate = sliceBetween(page, "const startCreate = ", "const startEdit = ");
  assert.match(startCreate, /setPresetMeta\(preset\);/);
  assert.match(startCreate, /setPresetMeta\(null\);/);
  // 编辑既有 server：模板要求反映的是当初的模板，不是当前配置的事实，必须清掉。
  assert.match(startEdit, /setPresetMeta\(null\);/);
  assert.match(closeForm, /setPresetMeta\(null\);/);
});

test("过期文案已修正：Codex 不再标「P1 起支持」", () => {
  assert.doesNotMatch(page, /P1 起支持/);
  assert.match(page, /const AGENT_OPTIONS: \{ id: string; label: string \}\[\] = \[\s*\{ id: "claude-code", label: "Claude Code" \},\s*\{ id: "codex", label: "Codex" \},\s*\];/);
});

test("类型声明了目录字段、依赖与凭据落点", () => {
  assert.match(types, /export type MCPPresetCredential = \{[\s\S]*?target: "env" \| "header";[\s\S]*?valuePrefix\?: string;[\s\S]*?docsUrl\?: string;[\s\S]*?\};/);
  assert.match(types, /export type MCPPresetRequirement = \{ command: string; label: string; hint\?: string \};/);
  assert.match(types, /export type MCPPreset = \{[\s\S]*?summary: string;[\s\S]*?category: string;[\s\S]*?icon: string;[\s\S]*?oauth\?: boolean;[\s\S]*?\};/);
  assert.match(types, /export type MCPRuntimeCheckResult = \{[\s\S]*?error\?: string;[\s\S]*?\};/);
});

test("新增样式都带组件前缀，且图标自带尺寸", () => {
  for (const className of ["mcp-preset-grid", "mcp-preset-card", "mcp-preset-meta", "mcp-preset-block", "mcp-preset-credential", "mcp-catalog-group", "mcp-service-mark", "mcp-wizard-steps", "mcp-wizard-trust", "mcp-advanced"]) {
    assert.ok(styles.includes(`.${className}`), `style.css 缺少 .${className}`);
  }
  // 内联 SVG 不给宽高会按 300×150 撑破布局；`.ssh-dialog-mark` 那条规则认的是 `.ssh-icon`，管不到它。
  assert.match(styles, /\.mcp-service-icon \{ display: block; width: 20px; height: 20px; flex: none; \}/);
});
