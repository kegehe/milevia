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

const [page, types, styles, pageStyles, dashboard] = await Promise.all([
  readFile(new URL("./pages/McpManagerPage.tsx", import.meta.url), "utf8"),
  readFile(new URL("./lib/types.ts", import.meta.url), "utf8"),
  readFile(new URL("./style.css", import.meta.url), "utf8"),
  readFile(new URL("./pages/mcp-manager.css", import.meta.url), "utf8"),
  readFile(new URL("./pages/DashboardPage.tsx", import.meta.url), "utf8"),
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
const topbar = sliceBetween(page, 'className="mcp-bar"', "{localError &&");
const advancedBlock = sliceBetween(page, "{showAdvanced && <section", "<h3>项目视图</h3>");
const wizardJsx = sliceBetween(page, "{wizard && <div", "{showForm && <div");
const connectedBlock = sliceBetween(page, "<h2>我的服务", "可以连接的服务</h2>");

test("独立页面壳：不再是「Dashboard + 弹窗背板」，页面名与首页入口写同一个字符串", () => {
  // 旧形态的三件套不许回来：以 Dashboard 为底、全屏背板、dialog 语义的页面根。
  assert.doesNotMatch(page, /<DashboardPage\s*\/>/);
  assert.doesNotMatch(page, /ssh-manager-backdrop/);
  assert.doesNotMatch(page, /aria-modal="true"[^>]*aria-labelledby="mcp-manager-title"/);
  // 新壳：独立页面 + 顶栏返回。
  assert.match(page, /className="mcp-shell"/);
  assert.match(topbar, /onClick=\{\(\) => navigate\("\/"\)\}>返回</);
  // 项目列表自己拉：独立页不再渲染 DashboardPage（它是原先 refreshProjects 的唯一调用方），
  // 直接刷新 / 托盘跳转进来时，表单与导入的项目下拉才有内容。
  assert.match(page, /void refreshProjects\(\);/);
  // 页面名只有一个来源：顶栏 h1 与首页入口的 title / <span> 写同一个字符串。
  // （改名字时三处一起动 —— 与 Cli 管理页同一条纪律。）
  const h1 = page.match(/<h1 className="mcp-title">([^<]+)<\/h1>/)?.[1];
  assert.ok(h1, "页面顶栏应有 h1 标题");
  assert.match(dashboard, new RegExp(`title="${h1}"[\\s\\S]{0,120}navigate\\("/mcp-manager"\\)`));
  assert.match(dashboard, new RegExp(`<span>${h1}</span>`));
});

test("主路径只有一条：点目录卡片进向导，其余能力收进「高级设置」", () => {
  assert.match(page, /const \[showAdvanced, setShowAdvanced\] = useState\(false\);/);
  assert.match(page, /onClick=\{\(\) => openWizard\(preset\)\}/);
  assert.match(page, /\{showAdvanced && <section className="mcp-section mcp-advanced">/);

  // 顶栏只留「高级设置」这一个开关：手动配置 / 导入 / 审计全部收进高级区。
  assert.match(topbar, /高级设置/);
  assert.doesNotMatch(topbar, /手动配置|从现有配置导入|调用审计/);
  assert.match(advancedBlock, /手动配置/);
  assert.match(advancedBlock, /从现有配置导入/);
  assert.match(advancedBlock, /调用审计/);
  // 「要准备什么」徽标行已随 UI 删除（presetBadges 一并移除），不许悄悄长回来。
  assert.doesNotMatch(page, /presetBadges|mcp-preset-meta|点一次授权即可|可授权，也可填密钥/);
  // 目录卡片的按钮：统一只叫「连接」（cardActionLabel 已删，不许再出现第二份文案口径），
  // 且按钮与左侧标题/描述左右分布（mcp-preset-main 左、mcp-preset-action 右），不单独占一行。
  assert.doesNotMatch(page, /cardActionLabel/);
  const presetCard = sliceBetween(page, 'className="mcp-preset-card"', "</article>");
  assert.match(presetCard, /<div className="mcp-preset-main">/);
  assert.match(presetCard, /<div className="mcp-preset-action"><button className="primary" type="button" onClick=\{\(\) => openWizard\(preset\)\}>连接<\/button><\/div>/);
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
  // 先落库再取消，不能假装什么都没发生 —— 取消时清掉半成品（见「取消向导」用例，
  // 那里也用切片断言守着 DELETE 路径）。
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

test("状态口径：enabled 只能说「已启用」，不许说成「已连接」", () => {
  // 「已连接」是一个**连接事实**，而列表里的每一条只是配置存在且启用 ——
  // 点了连接又中途放弃的人也会在这里看到一条记录（OAuth 路径会先落库）。
  assert.match(connectedBlock, /\{server\.enabled \? "已启用" : "已停用"\}/);
  assert.doesNotMatch(connectedBlock, /已连接/);
  assert.match(connectedBlock, /<h2>我的服务/);
});

test("三态：读失败与「真的没有」分开渲染，目录读失败不许静默消失", () => {
  // 服务列表：读失败是红调失败块 + 重试；只有读成功且为空才出「还没有配置」空态卡。
  //
  // ⚠️ 判据必须是**只由 loadServers 写**的那个状态（serversError），不能复用 localError：
  // localError 还承担"取消向导时清理半成品失败"这类与列表无关的提示，用它当判据时
  // 一次清理失败就会把真实空态改口成"读不到服务列表"、计数也从 0 变成「—」
  // （2026-09-29 复查修的就是这个，与目录那一路的 presetsError 同构）。
  assert.match(connectedBlock, /servers\.length === 0 && serversError \? <div className="mcp-empty mcp-read-fail"/);
  assert.match(connectedBlock, /<b>读不到服务列表<\/b>/);
  assert.match(connectedBlock, /onClick=\{\(\) => void loadServers\(\)\}>重试</);
  // 计数在读失败时显示「—」，不许把「读不到」显示成 0。
  assert.match(connectedBlock, /serversError \? "—" : servers\.length/);
  // 反面对照：这两个判据都不许再读 localError —— 读回来的就是原来那个混用的状态。
  assert.doesNotMatch(connectedBlock, /localError \? "—"/);
  assert.doesNotMatch(connectedBlock, /servers\.length === 0 && localError/);
  // 目录读失败单独成块（带重试），不许静默消失成「平台只有这几个服务」。
  assert.match(page, /presetsError \? <section className="mcp-section">/);
  assert.match(page, /<b>读不到服务目录<\/b>/);
  assert.match(page, /onClick=\{\(\) => void loadPresets\(\)\}>重试</);
});

test("取消向导：没有验证通过的半成品一律删掉，不留「从没连上」的记录", () => {
  // OAuth 路径先落库是技术必然（回调要按 id 存令牌），但用户点了连接又放弃时，
  // 那条记录必须清掉 —— 否则列表里躺着一条从没连上的「已启用」。
  assert.match(closeWizard, /wizardResult\?\.ok !== true/);
  assert.match(closeWizard, /\/api\/mcp\/servers\/\$\{leftover\.id\}`, \{ method: "DELETE" \}/);
  assert.match(closeWizard, /没有保存任何东西/);
  // 验证通过的（wizardResult.ok）才保留，并如实告诉用户它已存在。
  assert.match(closeWizard, /else if \(leftover\) \{[\s\S]*?已保存，可在列表里管理/);
});

test("服务牌位走官方图标：白名单内用 ServiceLogo，白名单外回落示意图标", async () => {
  // 判据只有一份（serviceLogoKey），三个渲染点都经它走：已连接卡 / 目录卡 / 向导弹窗头。
  const mark = "service={server.name} fallback={<PresetIcon name={presetIconKey(server.name, presets)} />}";
  assert.match(connectedBlock, new RegExp(`<ServiceLogo ${mark.replace(/[{}()]/g, "\\$&")} />`));
  assert.match(page, /<ServiceLogo service=\{preset\.name\} fallback=\{<PresetIcon name=\{preset\.icon\} \/>\} \/>/);
  assert.match(page, /<ServiceLogo service=\{wizard\.name\} fallback=\{<PresetIcon name=\{wizard\.icon\} \/>\} \/>/);
  // 资产在位：白名单里的每个键都要有一份官方 SVG —— 少一份就是一次运行时 404。
  for (const key of ["github", "notion", "linear", "sentry", "slack", "jira", "stripe", "playwright", "context7"]) {
    const asset = await readFile(new URL(`./assets/mcp-${key}.svg`, import.meta.url), "utf8");
    assert.match(asset, /<svg[\s\S]+<\/svg>/, `assets/mcp-${key}.svg 不是一份 SVG`);
  }
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
  for (const className of ["mcp-preset-grid", "mcp-preset-card", "mcp-preset-block", "mcp-preset-credential", "mcp-catalog-group", "mcp-service-mark", "mcp-wizard-steps", "mcp-wizard-trust", "mcp-advanced"]) {
    assert.ok(styles.includes(`.${className}`), `style.css 缺少 .${className}`);
  }
  // 内联 SVG 不给宽高会按 300×150 撑破布局；`.ssh-dialog-mark` 那条规则认的是 `.ssh-icon`，管不到它。
  assert.match(styles, /\.mcp-service-icon \{ display: block; width: 20px; height: 20px; flex: none; \}/);
});

test("独立页壳的样式在 mcp-manager.css：弹窗里限定的按钮规则在页面有等价版本", () => {
  // 页面壳三件套 + 分节 + 空态卡片（有下一步动作的空态禁止「一行灰字」）。
  for (const className of ["mcp-shell", "mcp-bar", "mcp-body", "mcp-section", "mcp-empty"]) {
    assert.ok(pageStyles.includes(`.${className}`), `mcp-manager.css 缺少 .${className}`);
  }
  // 目录卡片的左右分布：mcp-preset-main 左（标题/描述/徽标）、mcp-preset-action 右（按钮）。
  for (const className of ["mcp-preset-main", "mcp-preset-action"]) {
    assert.ok(pageStyles.includes(`.${className}`), `mcp-manager.css 缺少 .${className}`);
  }
  assert.match(pageStyles, /\.mcp-shell \.mcp-preset-card \{ display: flex; align-items: center; gap: 12px; padding: 15px 16px; \}/);
  // 原先作用域限定在 .ssh-manager-dialog 的按钮规则，页面上要有 .mcp-shell 等价版本
  //（少一半就是「弹窗里能点、页面上没样式」的半截迁移）。
  // ⚠️ 断言的是**完整选择器组**：只匹配单个选择器会被同组其它选择器的子串
  // 骗过（本轮复查实测：.connect 错位成 base，子串检查照样绿）。
  for (const group of [
    ".mcp-shell .ssh-action-button, .ssh-manager-dialog .ssh-action-button {",
    ".mcp-shell .ssh-action-button:hover:not(:disabled), .ssh-manager-dialog .ssh-action-button:hover:not(:disabled) {",
    ".mcp-shell .ssh-action-button.connect, .ssh-manager-dialog .ssh-action-button.connect {",
    ".mcp-shell .ssh-action-button.connect:hover:not(:disabled), .ssh-manager-dialog .ssh-action-button.connect:hover:not(:disabled) {",
    ".mcp-shell .ssh-action-button.danger:hover:not(:disabled), .ssh-manager-dialog .ssh-action-button.danger:hover:not(:disabled) {",
  ]) {
    assert.ok(styles.includes(group), `style.css 缺少选择器组：${group}`);
  }
  // .mcp-test-button 的宽度规则只剩弹窗半边 —— 页面上的版本在 mcp-manager.css
  //（留在 style.css 会形成半截迁移：宽度是死值，只有 font-size 生效）。
  assert.ok(styles.includes(".ssh-manager-dialog .ssh-action-button.mcp-test-button {"), "style.css 缺少弹窗侧 .mcp-test-button 规则");
  assert.doesNotMatch(styles, /\.mcp-shell \.ssh-action-button\.mcp-test-button/);
  assert.match(pageStyles, /\.mcp-shell \.ssh-connection-actions \.ssh-action-button \{[^}]*font-size: 11px/);
  // 变体迁移错位的兜底：mcp-shell 侧的 base 规则里不许混进 connect 的绿色
  //（错位的形状是「.mcp-shell .ssh-action-button, … .connect」——所有按钮常态变绿）。
  assert.doesNotMatch(styles, /\.mcp-shell \.ssh-action-button, \.ssh-manager-dialog \.ssh-action-button\.connect \{/);
  // 栅格：连接卡两列 / 目录卡三列，窄屏回落。
  assert.match(pageStyles, /\.mcp-shell \.mcp-conn-list \{ grid-template-columns: repeat\(2, minmax\(0, 1fr\)\); \}/);
  assert.match(pageStyles, /\.mcp-shell \.mcp-preset-grid \{ grid-template-columns: repeat\(3, minmax\(0, 1fr\)\); gap: 13px; \}/);
  // 官方图标用 <img> 渲染，牌位里要块状排布（inline 基线空隙会把卡片撑歪）。
  assert.match(pageStyles, /\.mcp-service-mark img \{ display: block; \}/);
  // 动作按钮：弹窗时代是 30px 图标方块，页面上带文字的按钮必须放开宽度，否则「授权/停用」竖排折行。
  assert.match(pageStyles, /\.mcp-shell \.ssh-connection-actions \.ssh-action-button \{ width: auto; min-width: 30px; height: 30px; padding: 0 10px; white-space: nowrap; font-size: 11px; font-weight: 700; \}/);
});

test("审计保留上限这条事实在界面上有出口，且不靠弹窗副标题", () => {
  // 2026-09-26 按用户要求删掉弹窗副标题「最近 2000 次 MCP 工具调用的裁决与结果」时，
  // 上限这个数字挪进了列表计数那一行 —— 服务端 mcpAuditRetention=2000 在读接口裁剪，
  // 界面上没有第二处说它，删干净等于把事实一起删了。
  assert.match(page, /最多保留最近 2000 次/);
  assert.doesNotMatch(page, /最近 2000 次 MCP 工具调用的裁决与结果/);
});
