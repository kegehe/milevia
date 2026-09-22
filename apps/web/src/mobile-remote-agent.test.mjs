import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

// 读源码做文本断言。**换行先归一化成 LF**：仓库 core.autocrlf=true 且没有 .gitattributes，
// git 重新签出后工作区是 CRLF，而下面大量锚点写的是 `\n  }\n\n  function …` 这种形式——
// 带上 \r 就会静默失配（2026-09-12 实测：refreshNow 那条用例报"找不到 refreshNow"，
// 根因是文件被换行改写，不是代码改了）。断言关心的是代码结构，不该被行尾格式左右。
const normalize = (text) => text.replace(/\r\n/g, "\n");
const page = normalize(await readFile(new URL("./pages/MobileRemotePage.tsx", import.meta.url), "utf8"));
const styles = normalize(await readFile(new URL("./pages/mobile-remote.css", import.meta.url), "utf8"));
// 设备表（多设备 + 备注名）那一层。这里只做**结构/接线**断言：`alias` 是不是走同一处归一化、
// 重新配对有没有继承。真正的行为判据在 `lib/mobile-devices.test.ts`（那份能直接跑函数）。
const lib = normalize(await readFile(new URL("./lib/mobile-devices.ts", import.meta.url), "utf8"));
// 剥掉 CSS 注释后的样式正文。**negative 断言必须拿它来比**：解释"为什么把这一组规则删掉"的
// 注释里会原样写出那些选择器，直接对 `styles` 做 `doesNotMatch` 会被自己的注释满足
// （同一个坑在 JSX 那边叫 `page.replace(/^\s*\/\/.*$/gm, "")`，见 TOOLING「断言前先剥注释」）。
const styleRules = styles.replace(/\/\*[\s\S]*?\*\//g, "");
// 全局样式表。扫码取景层的清底规则必须与它对齐：`style.css` 给根元素上了不透明底色，
// 那正是"只清 body 不够用"的前提（见扫码那条用例）。
const rootStyles = normalize(await readFile(new URL("./style.css", import.meta.url), "utf8"));
// 电脑端那一屏（`!mobileApp`）自己的样式表。它与手机页共用组件，所以必须单独读进来断言 ——
// 混在 mobile-remote.css 里的话，"这一页到底有没有桌面页头"就又变成靠肉眼看了。
const desktopStyles = normalize(await readFile(new URL("./pages/desktop-remote.css", import.meta.url), "utf8"));

// 消息列表**开标签**的锚点。不要写死成 `'<div className="mobile-message-list">'`：
// 2026-09-17 给它加了 `data-empty` 之后，两处"夹逼"用例的锚点一起失配，
// `indexOf` 全返回 -1，下面的比较就退化成"比 -1 大"的永真断言（只有锚点自证那条报了出来）。
// 这里只认「标签名 + 类名 + 后面必须是属性或标签结束」，多了属性不会失配、也不会误配
// `mobile-message-list-xxx` 这种前缀相同的类名。
function messageListOpen() {
  const at = page.search(/<div className="mobile-message-list"[ >]/);
  assert.ok(at > 0, '找不到消息列表开标签锚点（<div className="mobile-message-list"）');
  return at;
}

test("mobile new conversations choose and submit the selected agent", () => {
  assert.match(page, /newConversationAgent.*useState<"claude-code" \| "codex">/s);
  assert.match(page, /payload: \{ agentId \}/);
  // 必须写成"同一行、紧挨着"：原来的 /role="radio"[\s\S]*newConversationAgent === "claude-code"/
  // 会被同一按钮后面 onClick 里的同名表达式满足 —— 把 aria-checked 断掉也照样绿（已实测）。
  assert.match(page, /role="radio" aria-checked=\{newConversationAgent === "claude-code"\}/);
  assert.match(page, /role="radio" aria-checked=\{newConversationAgent === "codex"\}/);
  assert.match(page, /createConversationForProject\(newConversationProject, newConversationAgent\)/);
});

test("mobile processing indicator waits for durable completion", () => {
  assert.match(page, /mobile-agent-processing/);
  assert.match(page, /snapshotRevision: number/);
  assert.match(page, /snapshot\.snapshotRevision > state\.snapshotRevision/);
  assert.match(page, /hasPendingMessage[\s\S]*remoteConversation\.status !== "running"/);
  assert.match(page, /startup failure, cancellation, or service restart/);
  assert.doesNotMatch(page, /clearConversationProcessing\(payload\.conversationId, true\)/);
  assert.match(styles, /@keyframes mobile-agent-processing-dot/);
});

test("mobile processing state is scoped to the selected instance", () => {
  assert.match(page, /pendingMessageRef\.current\.clear\(\)/);
  assert.match(page, /realtimeMessagesRef\.current\.clear\(\)/);
  assert.match(page, /setProcessingConversations\(\{\}\)/);
});

test("mobile processing indicator sits at the end of the message list", () => {
  // 症状（用户报的）：对话运行中时，"Claude Code / Codex 正在处理..."显示在对话内容的**上方**
  //   （工具栏与消息列表之间），看历史时它一直钉在顶部；期望像豆包 / DeepSeek 那样跟在
  //   对话历史末尾——历史到哪，它就显示在哪。
  // 根因：状态条原先渲染成 .mobile-message-list 的**兄弟**节点，且排在该列表之前。
  // 修法：挪进 .mobile-message-list 内部、作为最后一个网格项；与上一条消息的间距交给
  //   列表的 gap（状态条自身不再留 margin-top）。
  //
  // 断言用"夹逼"而不是抠死 JSX 形状：状态条必须落在
  // [消息列表的开始标签, 列表的结束标签) 区间内，且在消息内容之后。
  // 中间怎么改消息渲染都不该判红，但挪回列表上方、挪到列表外面、挪到消息前面都必红
  // （三种情况都做过变异检验）。
  const listOpen = messageListOpen();
  // `</div>{tasksOpen` = 消息列表的结束标签必须紧跟任务抽屉区块。这是本文件里
  // 唯一能定位"列表边界"的锚点；它若因重构消失，anchors 那条会显式报错（不会静默通过）。
  const listClose = page.indexOf("</div>{tasksOpen");
  const messages = page.indexOf('className="mobile-message-markdown markdown"');
  const indicator = page.indexOf('className="mobile-agent-processing"');
  // 锚点先自证存在：少一个，下面的比较就会退化成"比 -1 大"的永真断言
  //（本用例第一版写的 `className="mobile-message-markdown"` 就少了 ` markdown` 后缀，
  //  indexOf 恒为 -1，那条断言对改坏完全无感——变异检验抓出来的）。
  assert.ok(listOpen > 0 && listClose > listOpen && messages > 0 && indicator > 0, "找不到消息列表 / 消息内容 / 状态条的锚点");
  assert.ok(indicator > listOpen, "处理状态条必须渲染在消息列表内部，不能挂在列表上方");
  assert.ok(indicator < listClose, "处理状态条必须在消息列表结束之前（必须是列表的最后一项）");
  assert.ok(indicator > messages, "处理状态条必须排在消息内容之后");
  // 间距交给列表的 gap：状态条自身不能再留 margin-top。真实浏览器实测——留 0 时它与上一条
  // 消息的间距是列表 gap 的 10px；把它注入回 10px 后变成 20px（间距翻倍）。
  assert.match(styles, /\.mobile-agent-processing \{[^}]*margin-top: 0;/s);
});

test("mobile processing bar is three sourced cells, not one sentence", () => {
  // 旧版是「三颗跳点 + 一句『Claude Code 正在处理...』」：Agent 名与状态同一字号同一颜色，
  // 身份和状态挤在一句话里；不说在干什么、不说多久了。现在拆成三格，每格一个来源。
  assert.match(page, /className="mobile-agent-processing-badge"/);
  assert.match(page, /className="mobile-agent-processing-stage"/);
  assert.match(page, /className="mobile-agent-processing-elapsed"/);
  // 三格读的三个值必须来自 lib/processing-indicator.ts —— 页面上**不许再出现第二套判断**。
  // 这是本项目"同一件事在多处各写一遍就是缺陷"那条的直接落地：钉住页面上只有调用、没有内联枚举。
  assert.match(page, /import \{ formatElapsed, latestNotice, processingBadge, processingStageText \} from "\.\.\/lib\/processing-indicator";/);
  assert.match(page, /const processingAgent = processingBadge\(conversation\?\.agentId \?\? ""\);/);
  assert.match(page, /const processingStage = processingStageText\(\{ notice: latestNotice\(conversation\?\.notices\) \}\);/);
  assert.match(page, /const processingElapsed = processingSince === null \? "" : formatElapsed\(processingClock - processingSince\);/);
  // 旧的那句文案（连同 ASCII 三点）必须真的不再渲染。**比对剥过注释的源码**：
  // 那句旧文案作为"为什么要改"写在状态条那段 JSX 注释里，直接对 `page` 做 doesNotMatch
  // 会被自己的注释满足（同一个坑在样式那边叫 styleRules，见文件头「断言前先剥注释」）。
  assert.doesNotMatch(page.replace(/\/\*[\s\S]*?\*\//g, "").replace(/^\s*\/\/.*$/gm, ""), /正在处理\.\.\./);
  // 徽标配色只由 data-agent 一处分档（不在 JSX 里写颜色）。
  assert.match(page, /data-agent=\{processingAgent\.tone\}/);
  assert.match(styles, /\.mobile-agent-processing\[data-agent="codex"\] \.mobile-agent-processing-badge \{ background: #1f2933; \}/);
});

test("mobile processing elapsed clock ticks on the running flag, not on message count", () => {
  // ⚠️ 挂点必须只看 conversationProcessing。若挂到消息条数上，AI 想出三十秒而中间不吐字
  //（最常见的一段）读数就整段冻住 —— 而那时正是最该显示秒数的时刻。
  const effect = page.match(/const \[processingClock, setProcessingClock\] = useState\(0\);[\s\S]*?\}, \[conversationProcessing\]\);/)?.[0] ?? "";
  assert.ok(effect, "找不到 processingClock 的定时 effect");
  assert.match(effect, /if \(!conversationProcessing\) return;/);
  assert.match(effect, /window\.setInterval\(\(\) => setProcessingClock\(Date\.now\(\)\), 1000\)/);
  assert.match(effect, /return \(\) => window\.clearInterval\(timer\);/);
  assert.doesNotMatch(effect, /conversationMessageCount|lastMessageLength/, "计时不该挂在消息条数上");
});

test("mobile processing elapsed time consumes startedAt instead of ignoring it", () => {
  // `processingConversations[id].startedAt` 原先**只写不读**（典型的"算了但没人读"，
  // 一条假装存在的边界）。本次改动把它真正消费掉，这条用例就是那条边界的守门人：
  // 删掉 startedAt 这个读点、或改成从 0 开始计时，这里必红。
  assert.match(page, /processingConversations\[conversation\.id\]\?\.startedAt/);
  // 退路必须存在且**不能**是 0：中途被杀掉再重开时没有 startedAt（内存态不落盘），
  // 从 0 开始会显示「3 秒」而它其实已经跑了一小时 —— 那是一句编出来的读数。
  assert.match(page, /Date\.parse\(conversation\.lastActivityAt \|\| ""\) \|\| null/);
  assert.match(page, /: null;\n  \/\/ 每秒走一格/);
});

test("mobile processing motion is shimmer plus glow, each degrading on its own", () => {
  // M2 流光 / M1 光晕。两条对 overflow 的要求**正好相反**（一个要裁切、一个要溢出），
  // 所以一条挂 ::after、一条挂 ::before，互不抢位。
  assert.match(styles, /@keyframes mobile-agent-processing-shimmer \{ 0% \{ transform: translateX\(-100%\); \} 55%, 100% \{ transform: translateX\(100%\); \} \}/);
  assert.match(styles, /@keyframes mobile-agent-processing-glow \{ 0%, 100% \{ opacity: \.35; transform: scale\(1\); \} 50% \{ opacity: \.85; transform: scale\(1\.035\); \} \}/);
  // ⚠️ 流光必须走 transform，**不能**用 left —— left 是布局属性，每帧会改 scrollWidth，
  // 与这一页的「贴底跟随」判据（scrollHeight - scrollY - innerHeight < 100）直接打架。
  // 这条用 styleRules（剥过注释）比：解释"为什么不用 left"的注释里就写着 left。
  assert.doesNotMatch(styleRules, /\.mobile-agent-processing::after[^}]*\bleft:/);
  // ⚠️ 辉光必须被压到条体底色**之下**：少了 z-index: -1，呼吸的是盖在文字上的一层绿雾，
  // 读数会被压得发糊（这条是"看着还行、读不清"的那种坏，静态截图里最容易放过）。
  assert.match(styles, /\.mobile-agent-processing::before \{ content: ""; position: absolute; inset: -1px; z-index: -1;/);
  // ⚠️ **裁切只能挂在 ::after 自己身上**。一旦 overflow: hidden 落在 .mobile-agent-processing
  // 本身上，::before 那圈外扩的辉光会被整块裁掉 —— M1 直接消失，而且不报错、只是不动。
  assert.match(styles, /\.mobile-agent-processing::after \{[^}]*overflow: hidden;/s);
  assert.doesNotMatch(styleRules, /\.mobile-agent-processing \{[^}]*overflow: hidden;/s);
  // 主题色一个像素都不动：底 / 边 / 字与改动前逐字一致，品牌色只出现在徽标那一格。
  assert.match(styles, /\.mobile-agent-processing \{[^}]*border: 1px solid #b8dcca;[^}]*color: #216b58;[^}]*background: #eef9f2;/s);
  assert.match(styles, /\.mobile-agent-processing-badge \{[^}]*background: #d97757;/s);
});

test("mobile processing degradation keeps the reading and drops only the decoration", () => {
  // 判据是"降级后信息不丢"，**不是"什么都不动"**：流光与光晕是装饰 → 撤掉；
  // 已耗时是读数 → 保留（它由 JS 每秒写进去，本来就不受 CSS 影响）。
  assert.match(styles, /@media \(prefers-reduced-motion: reduce\) \{\s*\.mobile-agent-processing::before, \.mobile-agent-processing::after \{ animation: none; \}/);
  assert.match(styles, /\.mobile-agent-processing::after \{ background: none; \}/);
  // ⚠️ 反向断言：**不能**顺手把 -elapsed 一起藏掉（那是把"多久了"这条信息弄丢，
  // 属于降级过度）。hide 的写法有一堆（display:none / visibility:hidden / opacity:0），三种都要挡。
  assert.doesNotMatch(styleRules, /\.mobile-agent-processing-elapsed[^{]*\{[^}]*(display: none|visibility: hidden|opacity: 0)/s);
  // 那条共用的关键帧**不能**随三颗点的 DOM 一起删掉：气泡里的「正在写」还在用它。
  assert.match(styles, /@keyframes mobile-agent-processing-dot \{ 0%, 60%, 100% \{ opacity: \.3; transform: translateY\(0\); \} 30% \{ opacity: 1; transform: translateY\(-3px\); \} \}/);
});

test("mobile snapshots normalize missing revisions for offline recovery", () => {
  assert.match(page, /Number\.isFinite\(value\.snapshotRevision\)/);
  assert.match(page, /Number\.isFinite\(snapshot\.snapshotRevision\)/);
});

test("mobile projects without a conversation open agent selection before creating", () => {
  // 进入会话视图要压一层浏览器历史（见"entering a project pushes a real history entry"），
  // 所以这里的断言跟着改成 enterConversationView：有会话才切视图，没会话先弹 Agent 选择。
  //
  // 断言必须锁在 openMobileProject 的函数体内：`setNewConversationProject(projectValue)` 在文件里
  // 还有一处（openNewConversation），用 `[\s\S]*?` 跨过去会被那一处满足 —— 把这里的分支整段删掉
  // 也照样绿（已实测）。所以先把函数体抠出来，再在函数体上断言。
  const openProjectBody = page.match(/async function openMobileProject\(projectValue: Project\) \{[\s\S]*?\n  \}/)?.[0] ?? "";
  assert.ok(openProjectBody, "找不到 openMobileProject");
  assert.match(openProjectBody, /if \(existingConversation\) \{\s*setSelectedConversation\(existingConversation\.id\);\s*enterConversationView\(\);\s*return;\s*\}/);
  assert.match(openProjectBody, /setNewConversationProject\(projectValue\);/);
  assert.match(page, /if \(!agentId\) \{[\s\S]*?openNewConversation\(projectValue\);[\s\S]*?return;/);
});

test("mobile conversation titles include the active agent", () => {
  assert.match(page, /function conversationAgentLabel\(agentId: string\)/);
  assert.match(page, /title: `\$\{item\.title \|\| "未命名会话"\} · \$\{conversationAgentLabel\(item\.agentId\)\}`/);
  assert.match(styles, /\.mobile-new-conversation-backdrop\s*\{/);
});

test("mobile composer is one rounded box with both buttons mounted inside it", () => {
  // "正文 / 已引用技能 / 已引用某条消息"三者有其一，发送键就该可用（只点了一颗技能、
  // 或只引用了某条消息、一个字没写，都能发出去）。漏掉 quoteRef 这一项的症状是：
  // 输入框上方明明挂着一颗引用胶囊，发送键却是灰的 —— 用户以为引用没生效。
  assert.match(page, /<button type="submit" className="mobile-composer-send" disabled=\{busy \|\| !conversation \|\| \(!messageDraft\.trim\(\) && skillRefs\.length === 0 && !quoteRef\)\}/);
  // 整盒 + 单行内嵌（DeepSeek / ChatGPT 那种）：盒子里**一行**依次是 [＋] [文字] [发送]。
  assert.match(page, /<div className="mobile-composer-box"><button type="button" className="mobile-composer-tool"[\s\S]*?<\/button><textarea[\s\S]*?\/><button type="submit" className="mobile-composer-send"/);
  assert.doesNotMatch(page, /mobile-composer-bar/);
  assert.doesNotMatch(styles, /mobile-composer-bar/);
  assert.match(styles, /\.mobile-composer-box\s*\{[^}]*display:\s*flex;[^}]*align-items:\s*flex-end;/s);
  assert.match(styles, /\.mobile-composer-box\s*\{[^}]*border-radius:\s*22px;/s);
  assert.match(styles, /\.mobile-composer textarea\s*\{[^}]*flex:\s*1;[^}]*min-height:\s*38px;/s);
  assert.match(styles, /\.mobile-composer\s*\{[^}]*display:\s*flex;/s);
  // 两颗按钮常驻。旧实现里发送键在草稿为空时 display: none，一打字就凭空出现，
  // 把输入框宽度挤掉一颗按钮、光标跟着跳；禁用态改由专门的一条配色规则表达。
  assert.match(styles, /\.mobile-composer button\s*\{[^}]*display:\s*grid;/s);
  assert.doesNotMatch(styles, /\.mobile-composer button:not\(:disabled\)/);
  assert.match(styles, /\.mobile-composer \.mobile-composer-send:disabled\s*\{[^}]*background:\s*#e9f0ec;/s);
  // 圆形按钮视觉 38px，热区靠 ::after 外扩 3px 补回 44（与头部三颗同一套做法）。
  assert.match(styles, /\.mobile-composer button\s*\{[^}]*width:\s*38px;[^}]*height:\s*38px;[^}]*border-radius:\s*999px;/s);
  assert.match(styles, /\.mobile-composer button::after\s*\{[^}]*inset:\s*-3px;/s);
  // 中文 / 日文选词时按 Enter 是"确认候选词"，不能当成发送（桌面端一直有这个判断，移动端原先漏了）。
  assert.match(page, /if \(event\.key === "Enter" && !event\.shiftKey && !event\.nativeEvent\.isComposing\)/);
  // 外壳只做"贴底固定"：它自己**不能**再画白底 / 上边框，否则盒子底下会多出一条白横带。
  assert.match(styles, /\.mobile-composer-shell\s*\{[^}]*position:\s*fixed;/s);
  assert.doesNotMatch(styles, /\.mobile-composer-shell\s*\{[^}]*background:/s);
  assert.doesNotMatch(styles, /\.mobile-composer-shell\s*\{[^}]*border-top:/s);
  // 透明外壳必须配 pointer-events: none，否则整条会挡住底下内容的点击。
  assert.match(styles, /\.mobile-composer-shell\s*\{[^}]*pointer-events:\s*none;/s);
  assert.match(styles, /\.mobile-composer-shell > \*\s*\{\s*pointer-events:\s*auto;/);
  assert.match(styles, /\.mobile-composer\s*\{[^}]*padding-bottom:\s*calc\(8px \+ env\(safe-area-inset-bottom\)\);/s);
});

test("mobile composer exposes a + tool panel with entries and a line break", () => {
  assert.match(page, /className="mobile-composer-tool"[^>]*aria-expanded=\{composerToolsOpen\}/);
  assert.match(page, /aria-controls="mobile-composer-tools"/);
  assert.match(page, /composerToolsOpen && <section className="mobile-composer-tools"/);
  assert.match(page, /function insertComposerLineBreak\(\)/);
  // 外壳透明之后，面板得自带白底与描边，做成浮在输入框上方的一张独立卡片。
  assert.match(styles, /\.mobile-composer-tools\s*\{[^}]*background:\s*#fff;/s);
  assert.match(styles, /\.mobile-composer-tools\s*\{[^}]*border:\s*1px solid #dbe9e0;/s);
  assert.match(styles, /\.mobile-tool-chips\s*\{[^}]*flex-wrap:\s*wrap;/s);
  // 面板里的胶囊也要 ≥44 触控高度，不然它是全页唯一"矮一截"的按钮。
  assert.match(styles, /\.mobile-tool-chips button\s*\{[^}]*min-height:\s*44px;/s);
  // 会话内容底部预留跟着输入条实测高度走，面板展开 / 多行输入时才不会被压住。
  assert.match(page, /--mobile-composer-height/);
  assert.match(styles, /padding-bottom:\s*calc\(var\(--mobile-composer-height,\s*68px\)\s*\+\s*16px\)/);
  // textarea 在 Chromium 里"一聚焦就命中 :focus-visible"，画出来的是盒子内部的一圈描边，
  // 会和盒子边框叠成双层 —— 焦点指示交给盒子承担（整盒变绿 + 3px 外发光），所以这里显式关掉。
  assert.match(styles, /\.mobile-remote \.mobile-composer textarea:focus-visible\s*\{[^}]*outline:\s*none;/s);
  assert.match(styles, /\.mobile-composer-box:focus-within\s*\{[^}]*border-color:\s*#2c7567;[^}]*box-shadow:\s*0 0 0 3px/s);
});

test("mobile + panel mirrors the desktop shortcut library instead of hardcoded phrases", () => {
  // 这里曾经是一组写死的 ["继续","总结当前改动","运行测试","解释这段代码"]，理由是"桌面端那套
  // 存在本机数据库、手机端拿不到"。现在快照把 shortcuts / skills 一并下发，用户要求的"同源"成立，
  // 所以断言的第一条就是**常量必须消失** —— 否则将来有人"顺手加一条常用指令"就又分叉了。
  assert.doesNotMatch(page, /composerToolPhrases/);
  assert.doesNotMatch(page, /insertComposerPhrase/);
  // 类型：快捷方式在快照顶层（一份可被多个项目共用），技能挂在项目上（它是"项目 + CLI"两个维度的产物）。
  assert.match(page, /type RemoteShortcut = \{ id: string; name: string;/);
  assert.match(page, /type Project = \{[^}]*skills\?: RemoteSkill\[\] \};/);
  assert.match(page, /type Snapshot = \{ snapshotRevision: number; observedAt: string; projects: Project\[\]; shortcuts\?: RemoteShortcut\[\] \}/);
  // 可见性判据必须与电脑端 listShortcuts 的 SQL 逐字对应（`scope='local' or 绑定到本项目`）；
  // **顺序**也必须是同一份 —— 由 control-server remoteSnapshotShortcuts 的 `order by sort_order,name`
  // 保证（曾多写一个 pinned desc，与桌面端不一致，且配了一句引错函数名的注释）。
  // 分组边界与桌面端一致（prompt + snippet → 常用提示词，command_request → 常用命令）。
  assert.match(page, /item\.enabled && \(item\.scope === "local" \|\| item\.projectIds\.includes\(project\?\.id \|\| ""\)\)/);
  assert.match(page, /item\.kind === "prompt" \|\| item\.kind === "snippet"/);
  assert.match(page, /item\.kind === "command_request"/);
  // 技能文案必须与桌面端 mergeSkillPrompt 一字不差：同一个技能在两端要让 Agent 收到同一句指令。
  assert.match(page, /function skillPrompt\(skill: RemoteSkill\): string \{/);
  assert.match(page, /`请使用技能 <\$\{skill\.name\}>：\$\{skill\.description\}`/);
  assert.match(page, /`请使用技能 <\$\{skill\.name\}>`/);
  // 点技能只挂一颗可删除的胶囊，**不许再整段覆盖输入框**：技能描述动辄上百字，手机上会铺满整屏，
  // 而 setMessageDraft 是整体覆盖 —— 用户写到一半的草稿会被无声吃掉。
  // 断言锁在 fillSkill 的函数体内：文件里还有别的 setSkillRefs（发送、切会话），
  // 用 [\s\S]*? 跨过去的话"点技能不挂胶囊"也能被别处满足。
  const fillSkillBody = page.match(/function fillSkill\(skill: RemoteSkill\) \{[\s\S]*?\n  \}/)?.[0] ?? "";
  assert.ok(fillSkillBody, "找不到 fillSkill");
  assert.match(fillSkillBody, /setSkillRefs/);
  assert.doesNotMatch(fillSkillBody, /fillComposer|skillPrompt/);
  assert.match(page, /function removeSkillRef\(skill: RemoteSkill\) \{/);
  // 展开推迟到发送那一刻，且与桌面端 composeSkillMessage 同构：引用在前、正文在后、中间空一行。
  assert.match(page, /function composeSkillMessage\(text: string, refs: RemoteSkill\[\]\): string \{/);
  assert.match(page, /const body = refs\.map\(skillPrompt\)\.join\("\\n"\);/);
  assert.match(page, /return text\.trim\(\) \? `\$\{body\}\\n\\n\$\{text\.trim\(\)\}` : body;/);
  // 正文（draft）与上线文本（content）必须分开：所有"写回输入框"的路径只许用 draft，
  // 灌 content 会把整段技能描述重新铺满输入框，等于把这次修复原地撤销。
  assert.match(page, /const draft = messageDraft\.trim\(\);/);
  // 2026-09-20 起 content 先过 withQuoteBlock（「引用某条消息」的引用块），再交给
  // composeSkillMessage 加技能指令 —— 所以顺序严格是 技能 → 引用 → 正文。
  assert.match(page, /const content = composeSkillMessage\(withQuoteBlock\(draft, quoteRef\), skillRefs\);/);
  // 引用与技能同一条生命周期：发送时一起清、失败时一起还回来、切会话一起清掉。
  assert.match(page, /setSkillRefs\(\[\]\);\s*setQuoteRef\(null\);/);
  assert.match(page, /setQuoteRef\(\(current\) => current \?\? quoteRef\);/);
  // 引用块的行前缀：逐行加 "> "，**空行也要带 ">"**（markdown 把空行当引用结束，
  // 不补的话一段带空行的长引用会被拆成「引用 + 普通段落」两截）。
  // 这条与超长引用的截断声明一起守"发出去的到底是什么"。
  assert.match(page, /return body\.split\("\\n"\)\.map\(\(line\) => \(line \? `> \$\{line\}` : ">"\)\)\.join\("\\n"\);/);
  assert.match(page, /const QUOTE_MAX_CHARS = \d+;/);
  // 收起是**唯一出口**：两处状态必须一起收。漏掉反馈的重置，下一条消息展开时会直接带着
  // 上一条的勾（探针里有"复制→收起→再展开另一条"那条用例）。
  assert.match(page, /function closeMessageActions\(\) \{[\s\S]*?setMessageCopyState\("idle"\);\s*setMessageActionId\(""\);\s*\}/);
  // 正文与引用**一起**回填，且都走"用户已经打了字就不覆盖"的守卫形式 —— 命令没发出去
  // 时把正文还回去、把技能胶囊还回去，两件事必须成对出现。
  assert.match(page, /setMessageDraft\(\(current\) => current\.trim\(\) \? current : draft\);\s*setSkillRefs\(\(current\) => current\.length > 0 \? current : skillRefs\);/s);
  assert.doesNotMatch(page, /setMessageDraft\(content\)/);
  // 胶囊本体：可删除的 × 必须在，且视觉 28px + ::after 外扩到 44 的触控热区。
  // 容器用 role="group" 而不是 role="list"：这一行里除了胶囊还有一句提示文字，
  // list 的直接子元素必须是 listitem，多一个 span 就是 a11y 违规。
  assert.match(page, /className="mobile-skill-refs" role="group"/);
  assert.match(page, /className="mobile-skill-ref-remove"/);
  assert.match(styles, /\.mobile-skill-refs\s*\{[^}]*flex:\s*0 0 100%;/s);
  assert.match(styles, /\.mobile-composer\s+\.mobile-skill-ref-remove\s*\{[^}]*width:\s*28px;[^}]*height:\s*28px;/s);
  assert.match(styles, /\.mobile-composer\s+\.mobile-skill-ref-remove::after\s*\{[^}]*inset:\s*-8px;/s);
  // 渲染一律走电脑端：模板里的 ${project.path} 这类变量手机端根本拿不到，本地拼一遍必然分叉。
  assert.match(page, /type: "conversation\.shortcut"/);
  assert.match(page, /function shortcutContentFromCommandResult\(result: unknown\): string/);
  // 片段只能"填入"：服务端的 run 接口明确拒绝片段（"snippets cannot run"）。
  assert.match(page, /if \(shortcut\.kind === "snippet"\) return "fill";/);
  // confirm 类先在手机上弹确认框 —— 这条命令会在电脑端执行，误触的代价不在这一屏。
  assert.match(page, /if \(action === "confirm"\) \{ setComposerToolsOpen\(false\); setConfirmShortcut\(shortcut\); return; \}/);
  assert.match(page, /mobile-shortcut-confirm-template/);
  // 确认框是三个模态里最"动手"的一个（点了就真的在电脑上跑一条命令），返回键必须能关掉它。
  assert.match(page, /if \(confirmShortcut\) \{ setConfirmShortcut\(null\); return true; \}/);
  // 会话入口不再出现在面板里：顶栏 ⋯ 菜单已经有一份，两处都给等于同一个动作重复两遍。
  assert.doesNotMatch(page, /<h3>会话<\/h3>/);
  // 独立空态文案：数据来自电脑端，没有条目时要说明来源，而不是让用户以为面板坏了。
  assert.match(page, /电脑端还没有添加常用提示词。/);
  assert.match(page, /电脑端未发现可用技能。/);
});

test("mobile message bubbles size to their content and point at the sender", () => {
  // grid 项默认 stretch：两种角色都必须**显式**写 justify-self，否则 Agent 的短句也撑满整条 86%。
  assert.match(styles, /\.mobile-message\s*\{[^}]*justify-self:\s*start;/s);
  // 宽度只能有一个来源：曾出现主体 86% + `@media (max-width: 620px)` 94% 的双值，
  // 手机上永远走后者 —— 前者是死值，"改了但没生效"。
  assert.match(styles, /\.mobile-message\s*\{[^}]*max-width:\s*94%;/s);
  assert.doesNotMatch(styles, /@media \(max-width: 620px\) \{[^@]*?\.mobile-message \{/s);
  assert.match(styles, /\.mobile-message\.user\s*\{[^}]*justify-self:\s*end;/s);
  // 圆角 16px，尖角那一侧收到 5px（Agent 左下、用户右下）。
  assert.match(styles, /\.mobile-message\s*\{[^}]*border-radius:\s*16px 16px 16px 5px;/s);
  assert.match(styles, /\.mobile-message\.user\s*\{[^}]*border-radius:\s*16px 16px 5px 16px;/s);
  // 不再靠 1px 全框分层：描边压淡 + 一层极淡投影。
  assert.match(styles, /\.mobile-message\s*\{[^}]*box-shadow:\s*0 1px 3px/s);
  // 元信息：11px + 显式行高（<small> 继承全局行高会让同一组件量出两个高度）。
  assert.match(styles, /\.mobile-message small\s*\{[^}]*font-size:\s*11px;[^}]*line-height:\s*1\.45;/s);
  // 「我」的消息是实心深绿 + 白字（2026-09-13 从 4 个候选里选定）。
  assert.match(styles, /\.mobile-message\.user\s*\{[^}]*background:\s*#2c7567;/s);
  assert.match(styles, /\.mobile-message\.user\s*\{[^}]*color:\s*#fff;/s);
  // 深底上每一处前景色都要显式覆盖：markdown 的标题 / 行内代码 / 链接都是深绿系，
  // 压在 #2c7567 上直接看不见；代码块里的 code 还要再恢复继承。
  assert.match(styles, /\.mobile-message\.user small\s*\{[^}]*color:\s*#c2e7d8;/s);
  assert.match(styles, /\.mobile-message\.user \.mobile-message-markdown :is\(h1, h2, h3, h4, h5, h6\)\s*\{[^}]*color:\s*#fff;/s);
  assert.match(styles, /\.mobile-message\.user \.mobile-message-markdown a\s*\{[^}]*color:\s*#e6f6ee;/s);
  assert.match(styles, /\.mobile-message\.user \.mobile-message-markdown code\s*\{[^}]*background:\s*rgba\(255, 255, 255, \.16\);/s);
  assert.match(styles, /\.mobile-message\.user \.mobile-message-markdown pre code\s*\{[^}]*background:\s*transparent;/s);
  // 表格 / 分隔线在浅色气泡里是浅底深字，压到深绿上会变突兀的浅块 —— 也要换成半透明白。
  assert.match(styles, /\.mobile-message\.user \.mobile-message-markdown table\s*\{[^}]*background:\s*rgba\(255, 255, 255, \.1\);/s);
  assert.match(styles, /\.mobile-message\.user \.mobile-message-markdown td\s*\{[^}]*color:\s*#fff;/s);
  assert.match(styles, /\.mobile-message\.user \.mobile-message-markdown hr\s*\{[^}]*background:\s*rgba\(255, 255, 255, \.26\);/s);
  // 段落间距必须真的留出来：曾经有一条把 `.mobile-message` 段落 `margin-bottom` 清零的历史覆盖
  // 写在 markdown 规则**之后**、同特异性，把气泡里所有段落间距压成了 0（多段回复挤成一坨）。
  // 断言前先剥掉注释 —— 解释这条历史覆盖时，注释里会把那段选择器原样写一遍。
  assert.doesNotMatch(styles.replace(/\/\*[\s\S]*?\*\//g, ""), /\.mobile-message p\s*\{[^}]*margin-bottom:\s*0/s);
  // 只要求"有下边距"：具体数值是排版方案的自由度（见下面 structured briefing 那条）。
  assert.match(styles, /\.mobile-message-markdown p\s*\{[^}]*margin:\s*0 0 \d+px;/s);
  // 时间戳收窄：原 toLocaleString 输出 "2026/9/13 19:31:15"，年份和秒对读对话毫无帮助。
  assert.match(page, /function messageTime\(createdAt: string\): string/);
  assert.match(page, /messageTime\(entry\.message\.createdAt\)/);
  assert.doesNotMatch(page, /new Date\(entry\.message\.createdAt\)\.toLocaleString\(\)/);
  // 跨年要带年份，否则"12/31 19:41"会被读成今年的。
  assert.match(page, /parsed\.getFullYear\(\) === now\.getFullYear\(\)/);
});

test("mobile message bubbles reveal an icon action row instead of opening a sheet", () => {
  // 症状（用户要求）：对话卡片上没有任何操作 —— 想复制一条 Agent 的回复只能长按选词，
  // 想把某条引用一下更是无从下手。
  // 演进：2026-09-20 先在三个候选里选了 B（角落「⋯」→ 底部白底面板），落地后用户否掉了它 ——
  // "不应该升起白底面板来做操作，而是几个图标按钮，不需要那么多文字"。于是那一整层（背板、
  // 焦点搬运、背景 inert、目标预览卡）全部删掉，改成点「⋯」在该气泡的元信息行里**原地**
  // 冒出一排图标（复制 / 引用 / 重发）。
  // 为什么不是"每条气泡底部铺一行按钮"（候选 A）：常驻一行按 44px 触控算，实测每条 +59px，
  // 一屏完整可见的消息从 5 条掉到 3 条；现在这套只占**已有的** 30px 元信息行，展开前后都是 30px。
  // 为什么不用长按（候选 C）：本页没有抑制 contextmenu（只有桌面端 Tauri 抑制），
  // 用户现在长按能选词复制；要弹自己的面板就得给气泡上 user-select: none，那是净损失。
  //
  // 这一条守的是**结构**（真实行为在浏览器探针 probe-mobile-message-actions.mjs 里）。
  assert.match(page, /const \[messageActionId, setMessageActionId\] = useState\(""\);/);
  assert.match(page, /const \[messageCopyState, setMessageCopyState\] = useState<"idle" \| "copied" \| "failed">\("idle"\);/);
  // 面板那一层的痕迹一个都不能剩：留着半截代码就说明还有一条能走进去的死路。
  assert.doesNotMatch(page, /message-sheet/);
  assert.doesNotMatch(styles, /mobile-message-sheet/);
  // 展开的是**哪一条**：记 id 不记 Message 对象，流式消息的正文才不会被冻在展开那一刻。
  assert.doesNotMatch(page, /messageActionContent/);

  // ① 触发它的那颗「⋯」仍在元信息行右端，且是**同一条消息**的切换开关（收起也走它）。
  // 元信息行里现在还有"流式那条"的「正在写」三点（2026-09-20 动效，见下面的动效用例），
  // 所以这里连它一起钉住 —— 三点必须在小字**内部**（不是 head-actions 里那颗「⋯」旁边）。
  assert.match(page, /<div className="mobile-message-head"><small>\{entry\.message\.role === "user" \? "我" : "Agent"\} · \{messageTime\(entry\.message\.createdAt\)\}\{isStreamingMessage\(entry\.message\) && <span className="mobile-message-writing" role="img" aria-label="正在写入">/);
  assert.match(page, /<\/span>\}<\/small><div className="mobile-message-head-actions">/);
  assert.match(page, /data-actions=\{messageActionId === entry\.message\.id \? "open" : undefined\}/);
  assert.match(page, /aria-expanded=\{messageActionId === entry\.message\.id\}/);
  assert.match(page, /aria-controls=\{messageActionId === entry\.message\.id \? `mobile-message-actions-\$\{index\}` : undefined\}/);
  assert.match(page, /onClick=\{\(event\) => \{ if \(messageActionId === entry\.message\.id\) \{ closeMessageActions\(\); return; \} openMessageActions\(event\.currentTarget, entry\.message\.id\); \}\}/);
  // 渲染点恰好一处：多一处就意味着状态卡 / 别的地方也长了这颗按钮（图标行操作的是消息，不是状态卡）。
  assert.equal((page.match(/className="mobile-message-more"/g) || []).length, 1, "「⋯」应当只在消息气泡上渲染一次");
  // 视觉 30px，热区靠 ::after 外扩 7px 补到 44×44（与顶栏那三颗同一套做法）。
  assert.match(styles, /\.mobile-message-more \{[^}]*width: 30px;[^}]*height: 30px;/s);
  assert.match(styles, /\.mobile-message-more::after \{ position: absolute; inset: -7px;/);
  // 深绿气泡（「我」）上必须显式覆盖前景色：默认那颗 #52766a 压在 #2c7567 上等于看不见。
  assert.match(styles, /\.mobile-message\.user \.mobile-message-more \{/);
  // 展开时把元信息文字整行隐去，靠的是气泡上的 data-actions（变体一律走 data-*，不另造类名）。
  // ⚠️ 属性在 **article** 上，选择器必须从 `.mobile-message` 起 —— 写成 `.mobile-message-head[…]`
  // 时属性不在那层、规则永远不生效（探针量到 labelHidden:false 才发现的）。
  assert.match(styles, /\.mobile-message\[data-actions="open"\] \.mobile-message-head > small \{ display: none; \}/);

  // ② 图标行本体：复制 / 引用 / 重发三条，**只有图标、没有可见文字**。
  // 分组名字里带不带那句"此刻只能复制"，取决于这条消息落没落定 —— 禁用图标**不可聚焦**，
  // 它的 title/aria-label 在焦点模式下根本读不到，所以原因必须挂到分组这个能读到的容器上。
  assert.match(page, /\{messageActionId === entry\.message\.id && <div className="mobile-message-actions" id=\{`mobile-message-actions-\$\{index\}`\} ref=\{messageActionsRef\} role="group" aria-label=\{`\$\{entry\.message\.role === "user" \? "我" : "Agent"\}这条消息的操作\$\{isTransientMessage\(entry\.message\) \? `（\$\{transientReason\(entry\.message, "row"\)\}）` : ""\}`\}>/);
  assert.equal((page.match(/className="mobile-message-action-icon"/g) || []).length, 3, "复制 / 引用 / 重发三颗");
  {
    // ⚠️ 反例断言必须**切到图标行这一块**再扫，不能扫全文件：`重新生成` 在电脑端那张配对卡片上
    // 是另一件事（「重新生成二维码」），扫全文件会让这条断言永远红。剥注释同理 ——
    // 图标行上方的注释为了解释"为什么不做"会把这两个词原样写一遍。
    const rowAt = page.indexOf('className="mobile-message-actions"');
    const moreAt = page.indexOf('className="mobile-message-more"', rowAt);
    const rowSlice = rowAt > 0 && moreAt > rowAt ? page.slice(rowAt, moreAt) : "";
    assert.ok(rowSlice.length > 0, "没切到图标行");
    const code = rowSlice.replace(/\/\*[\s\S]*?\*\//g, "");
    assert.doesNotMatch(code, /删除|重新生成/);
    // "没有可见文字"这条由它守：面板那一版每个动作都带 <b> 标题 + <small> 副说明，现在一个都不许有。
    assert.doesNotMatch(code, /<b>|<small>/, "图标行里不许再出现可见文案");
    // 图标行在「⋯」**之前**：⋯ 始终是最右那颗，位置不随展开而跳。
    assert.ok(moreAt > rowAt);
  }

  // ③ 尺寸链是自洽的：30px 画 + ::after 外扩 7px = 44px 热区 ⇒ 相邻两颗必须隔 ≥14px，
  //    否则两颗的热区互相压住（点左边那颗实际命中右边）。这个 14px 只有一个来源，别调小。
  assert.match(styles, /\.mobile-message-actions \{ position: relative; display: flex; align-items: center; gap: 14px; \}/);
  assert.match(styles, /\.mobile-message-head-actions \{ display: flex; flex: none; align-items: center; gap: 14px; margin-left: auto; \}/);
  assert.match(styles, /\.mobile-message-action-icon \{[^}]*width: 30px;[^}]*height: 30px;/s);
  assert.match(styles, /\.mobile-message-action-icon::after \{ position: absolute; inset: -7px;/);
  // 禁用态（流式 / 还没发出去）只允许"把字色压暗一档"，**不许用 opacity 把整颗淡掉**：
  // 透明度会把字色和底色一起推向背景，两种底色上的结果完全不同；而字色是量出来选的下限
  // （`#6f887c` 压 `#eef3f1` = 3.46:1，刚好过图形对象那道 3:1）。白气泡与深绿气泡各一档。
  assert.match(styles, /\.mobile-message-action-icon:disabled \{[^}]*color: #6f887c;[^}]*background: #eef3f1;/s);
  assert.doesNotMatch(styles, /\.mobile-message-action-icon:disabled \{[^}]*opacity:/s, "禁用态不许用 opacity 淡掉（两种底色上结果不同）");
  // 深绿底上**不许留半透明白底**：留着会把合成底色提亮到 #4A887C，同一颗亮字只剩 2.6:1
  // （探针量的；手算拿纯气泡色会得 3.46，那是漏了"半透明底会提亮背景"这一步）。
  assert.match(styles, /\.mobile-message\.user \.mobile-message-action-icon:disabled \{ color: #a9d8c6; background: transparent; \}/);
  assert.match(styles, /\.mobile-message\.user \.mobile-message-action-icon \{/);
  // 热区外扩 7px 的下沿在"距气泡顶 47px"，正文首行的行盒必须从 48px 之后开始 ——
  // 这个 8px 是算出来的（10 + 30 + 8），不是审美取值；调小它就会让热区压住正文首行。
  assert.match(styles, /\.mobile-message-head \+ \.mobile-message-markdown \{ margin-top: 8px; \}/);

  // ④ 复制：走两端共用的 lib/clipboard，反馈**就地**（那颗图标自己换成勾），不弹页面级提示。
  //    图标行不是浮层，没有第二处可以写"已复制"三个字 —— 所以 sr-only 的播报必须有，
  //    而且**不能**用 display: none（那会把节点移出无障碍树，这条就成了摆设）。
  assert.match(page, /async function copyMessageBody\(content: string\) \{\s*const copied = await copyToClipboard\(content\);/);
  assert.match(page, /messageCopyTimerRef\.current = window\.setTimeout\(\(\) => setMessageCopyState\("idle"\), 1_600\);/);
  assert.match(page, /data-copy-state=\{messageCopyState\}/);
  assert.match(page, /<MessageActionIcon kind=\{messageCopyState === "idle" \? "copy" : messageCopyState\} \/>/);
  assert.match(page, /if \(kind === "copied"\) return <svg/);
  assert.match(page, /if \(kind === "failed"\) return <svg/);
  assert.match(page, /<span className="mobile-message-action-status" role="status" aria-live="polite">/);
  // ⚠️ 反馈色的选择器**必须带气泡前缀**：`.mobile-message.user .mobile-message-action-icon` 是 (0,3,0)，
  // 裸的 `[data-copy-state]` 只有 (0,2,0) —— 后者压不过前者，于是**在「我」的深绿气泡上复制成功时
  // 底色根本不会换**（只剩图标形状变）。这一条是 2026-09-20 复查源码时按特异性算出来的，
  // 探针里另有一条"深绿气泡上合成底色必须是 #d9f2e4"的实测（两套一起才算数）。
  assert.match(styles, /\.mobile-message \.mobile-message-action-icon\[data-copy-state="copied"\],\s*\.mobile-message\.user \.mobile-message-action-icon\[data-copy-state="copied"\] \{ color: #1f7a52; background: #d9f2e4; \}/);
  assert.match(styles, /\.mobile-message \.mobile-message-action-icon\[data-copy-state="failed"\],\s*\.mobile-message\.user \.mobile-message-action-icon\[data-copy-state="failed"\] \{ color: #9b3e33; background: #f7e3df; \}/);
  // 不许退回"裸类选择器"那种写法：单类版本遇上气泡覆盖规则就是静默失效。
  assert.doesNotMatch(styles, /^\.mobile-message-action-icon\[data-copy-state/m, "反馈色必须带气泡前缀（否则被气泡覆盖规则吃掉）");
  assert.match(styles, /\.mobile-message-action-status \{[^}]*clip-path: inset\(50%\);/s);
  assert.doesNotMatch(styles, /\.mobile-message-action-status \{[^}]*display: none/s, "sr-only 用 display:none 会把节点移出无障碍树");
  // 复制的是**现在这一刻**的正文（直接读渲染中的 entry.message），不再是某次快照。
  assert.match(page, /onClick=\{\(\) => void copyMessageBody\(entry\.message\.content\)\}/);

  // ⑤ 流式（stream-*）与乐观（pending-*）消息把「引用」禁用掉（前者内容还在变、后者还没真发出去），
  //    「重发」另外只对用户消息出现。禁用而不是隐藏 —— 图标位置不跳，也让"有过这颗、此刻不可用"
  //    这件事看得出来。原因写在 title / aria-label 里，且两条路的原因必须**分开**说。
  // 「还没落定」的判据只有一份来源：占位 id 的前缀。`isTransientMessage` 复用它 ——
  // 顶替判据（换 id 不换消息）用的也是同一个前缀判断，两处各写一遍迟早会漂移。
  assert.match(page, /function isPlaceholderKey\(key: string\): boolean \{\s*return key\.startsWith\("pending-"\) \|\| key\.startsWith\("stream-"\);\s*\}/);
  assert.match(page, /function isTransientMessage\(message: Message\): boolean \{\s*return isPlaceholderKey\(message\.id\);\s*\}/);
  // 文案来源必须是**一个函数**、且三种用法（分组名 / 引用 / 重发）各自拿到对的句子：
  // 重发只可能出现在用户消息上，所以它不需要"还在写入中"那一路；反过来把这两句合成一句
  // 就是在替一条还没发出去的消息编造"正在写入"的进度（探针与变异各有一条守它）。
  assert.match(page, /function transientReason\(message: Message, action: "row" \| "quote" \| "resend"\): string \{/);
  assert.match(page, /if \(action === "resend"\) return "这条消息还没发出去，不用重发";/);
  assert.match(page, /if \(action === "row"\) return streaming \? "回复还在写入中，暂时只能复制" : "这条消息还没发出去，暂时只能复制";/);
  assert.match(page, /return streaming \? "回复还在写入中，暂时不能引用" : "这条消息还没发出去，暂时不能引用";/);
  assert.match(page, /disabled=\{isTransientMessage\(entry\.message\)\} aria-label=\{`引用\$\{entry\.message\.role === "user" \? "我" : "Agent"\}这条消息到输入框`\}/);
  // 原因文案只有 `transientReason()` 一个来源（分组名 / 引用 / 重发三处共用）——
  // 分开写三份的结果是"改一处、另两处说错原因"，而这两句话被明确要求**必须分开**。
  assert.match(page, /title=\{isTransientMessage\(entry\.message\) \? transientReason\(entry\.message, "quote"\) : "引用到输入框"\}/);
  assert.match(page, /\{entry\.message\.role === "user" && <button type="button" className="mobile-message-action-icon" disabled=\{isTransientMessage\(entry\.message\)\} aria-label="把这条消息的原文填回输入框"/);
  assert.match(page, /title=\{isTransientMessage\(entry\.message\) \? transientReason\(entry\.message, "resend"\) : "填回输入框，改完再发"\} onClick=\{\(\) => resendMessage\(entry\.message\.content\)\}/);

  // ⑥ 引用：**只留一条**，挂在输入条上方（与技能引用同一形态），发送时才展开成引用块。
  assert.match(page, /const \[quoteRef, setQuoteRef\] = useState<MessageQuote \| null>\(null\);/);
  assert.match(page, /\{quoteRef && <div className="mobile-quote-refs" role="group" aria-label="已引用的消息">/);
  assert.match(page, /onClick=\{\(\) => setQuoteRef\(null\)\}>×<\/button>/);
  // 回显的是"别人写的内容"，长度不可控 ⇒ 必须夹列宽 + 单行省略。min-width: 0 要给到**文字本身**：
  // flex 项的自动最小尺寸是 min-content（整段原文的宽度），会顶掉 max-width: 100%。
  assert.match(styles, /\.mobile-quote-ref \{[^}]*min-width: 0;[^}]*max-width: 100%;/s);
  assert.match(styles, /\.mobile-quote-ref-text \{[^}]*min-width: 0;[^}]*text-overflow: ellipsis;[^}]*white-space: nowrap;/s);
  // × 的规则必须带 .mobile-composer 前缀：`.mobile-composer button`(0,1,1) 的 38px 方块会盖掉单类选择器。
  assert.match(styles, /\.mobile-composer \.mobile-quote-ref-remove \{[^}]*width: 28px;[^}]*height: 28px;/s);
  assert.match(styles, /\.mobile-composer \.mobile-quote-ref-remove::after \{ position: absolute; inset: -8px;/);

  // ⑦ 重新发送＝把原文写回输入框，**绝不自动发出**；有草稿时追加，不覆盖
  //    （项目踩过"整体覆盖把用户写到一半的草稿无声吃掉"）。
  const resendBody = page.match(/function resendMessage\(content: string\) \{[\s\S]*?\n  \}/)?.[0] ?? "";
  assert.ok(resendBody, "找不到 resendMessage");
  assert.match(resendBody, /setMessageDraft\(\(current\) => \(current\.trim\(\) \? `\$\{current\.replace\(\/\\s\+\$\/, ""\)\}\\n\$\{content\}` : content\)\)/);
  assert.doesNotMatch(resendBody, /dispatchConversationMessage|sendConversationMessage|cloud\(/, "「重新发送」只许填回输入框，不许直接发出去");

  // ⑧ 清退两处都要有它（返回键链 + 侧滑那条链）：只写一处就会"退回项目列表、图标行还留着"，
  //    而它上面的三颗图标都还是可点的。
  //    ⚠️ 返回键那条链的判据必须是 `messageActionOpen`（界面上真的有这一排），不能用 id：
  //    id 可能指向一条已经不在快照里的消息 —— 那样按一次返回键会被一个谁也看不见的状态白吃掉
  //    （与 leaveConversationView 里"不让谁也收不掉的状态吃掉一次返回键"同因）。
  assert.match(page, /const messageActionOpen = conversationTimeline\.some\(\(entry\) => entry\.kind === "message" && entry\.message\.id === messageActionId\);/);
  assert.match(page, /if \(messageActionOpen\) \{ closeMessageActions\(\); return true; \}/);
  assert.doesNotMatch(page, /if \(messageActionId\) \{ closeMessageActions\(\); return true; \}/, "返回键不能按 id 判（会白吃一次）");
  {
    const leaveAt = page.indexOf("function leaveConversationView() {");
    const leaveBlock = leaveAt > 0 ? page.slice(leaveAt, page.indexOf("\n  }", leaveAt)) : "";
    assert.ok(leaveBlock.includes("closeMessageActions();"), "侧滑返回那条链也要收消息动作图标行");
  }
  // 会话创建失败那条回填路径：队列里每条消息都带着 `quote`，**必须一并还回来**。
  // 只还 draft 的话，用户看到自己写的字回来了、以为引用还挂着 —— 实际那条引用已经随失败没了。
  // ⚠️ 诚实标注：这条路上 `quote` 今天**基本恒为 null**（待建会话的线程里只有 `pending-*`
  // 乐观气泡，引用对它们禁用），所以这两条断言守的是"漏掉就静默丢"的结构不变式，
  // **不是**一条能在界面上跑出来的路径 —— 没有探针覆盖它，别把它当成行为已验证。
  {
    const failAt = page.indexOf("if (deferred.length > 0 && wasOnConversation) {");
    const failBlock = failAt > 0 ? page.slice(failAt, failAt + 900) : "";
    assert.ok(failBlock, "找不到会话创建失败的回填分支");
    assert.match(failBlock, /const lastQuote = \[\.\.\.deferred\]\.reverse\(\)\.find\(\(message\) => message\.quote\)\?\.quote \?\? null;/);
    assert.match(failBlock, /if \(lastQuote\) setQuoteRef\(\(current\) => current \?\? lastQuote\);/);
  }
  // 入队那一刻就把 quote 存进队列（它是"失败时要还回去的东西"的清单，不能只存 draft）。
  assert.match(page, /queue\.push\(\{ requestId: clientRequestId, content, createdAt, draft, quote: quoteRef \}\);/);

  // ⑨ 收起与焦点：点图标行之外的任何地方、按 Esc 都收（顶栏 ⋯ 菜单同一套：document 上的
  //    pointerdown + closest，**不铺透明遮罩** —— 遮罩虽然也能"点外部收起"，但它是 inset:0
  //    的一层，会把消息列的滑动一起吃掉）。触发它的「⋯」每条气泡各一颗，落点靠打开时记下的
  //    event.currentTarget，而不是一个固定的 ref。
  assert.match(page, /const messageActionsRef = useRef<HTMLDivElement \| null>\(null\);/);
  assert.match(page, /const messageActionTriggerRef = useRef<HTMLButtonElement \| null>\(null\);/);
  assert.match(page, /if \(target instanceof Element && target\.closest\("\.mobile-message-head-actions"\)\) return;/);
  assert.match(page, /document\.addEventListener\("pointerdown", onPointerDown\);/);
  assert.match(page, /if \(event\.key !== "Escape"\) return;/);
  assert.match(page, /if \(trigger && document\.contains\(trigger\)\) trigger\.focus\(\{ preventScroll: true \}\);/);
  // 展开后焦点交给第一颗**可用**的图标：不搬焦点的话读屏用户不知道旁边多了什么；
  // 禁用那颗不接收焦点，否则焦点会落在"按不动的那颗"上。
  assert.match(page, /messageActionsRef\.current\?\.querySelector<HTMLButtonElement>\("button:not\(:disabled\)"\)\?\.focus\(\{ preventScroll: true \}\);/);
  assert.equal((page.match(/\}, \[messageActionId\]\);/g) || []).length, 2, "焦点与「点外部收起」两个 effect 都以 messageActionId 为依赖");
  // 收起后不再有"抢焦点"的 cleanup（面板那版的 keepFocus 牌子随面板一起删了）——
  // 留着它就是一段永远走不到的逻辑。
  assert.doesNotMatch(page, /messageActionKeepFocusRef|closeMessageActionsForComposer/);
});

test("mobile conversation card motion only animates genuinely new entries, and every rule degrades", () => {
  // 2026-09-20：气泡本身原来一条动效都没有（那 15 处 animation 全是"状态指示"）。
  // 这一条守的是那批新动效里**最容易写错、且静态截图看不出来**的地方 —— 真实行为在浏览器探针
  // probe-mobile-message-actions.mjs 里。
  // 切片从**这一段的第一个关键帧**开始（不是从 `[data-arrive]`：那条规则排在
  // `@keyframes mobile-arrive-in` 之后，用它当起点会把第一条关键帧切掉）。
  const motionAt = styleRules.indexOf("@keyframes mobile-arrive-in");
  assert.ok(motionAt >= 0, "找不到动效那一段");
  const motion = styleRules.slice(motionAt);

  // ── ① 入场必须**逐条**挂，不能在列表容器上挂"就绪"开关 ─────────────────────
  // CSS 入场动画的触发条件是"元素**开始匹配**一条带 animation 的规则"。容器开关从"不匹配"
  // 切到"匹配"时，已经在 DOM 里的那批条目会一起重放（切换会话 = 整屏闪一次）；
  // 逐条挂则只有新挂载的那条会匹配。两种写法在静态截图里长得一样，只有真点一次才分得出来。
  assert.equal((page.match(/data-arrive=\{arrivingKeys\.includes\(entry\.key\)/g) || []).length, 2, "消息与状态卡两条渲染路径都要挂");
  assert.match(styles, /\.mobile-message\[data-arrive="true"\] \{ animation: mobile-arrive-in 260ms cubic-bezier\(\.25, \.8, \.25, 1\) backwards; \}/);
  assert.match(styles, /\.mobile-notice\[data-arrive="true"\] \{ animation: mobile-notice-in 180ms/);
  // 认"第一批"的那块账：换会话 / 首次进入时先记下来、不发动画。
  assert.match(page, /const seenTimelineKeysRef = useRef<Set<string> \| null>\(null\);/);
  assert.match(page, /const arriveScopeRef = useRef\(""\);/);
  // 上一批的 key 要**有序**保存：光有集合认不出"同一个位置被顶替"。
  assert.match(page, /const previousTimelineKeysRef = useRef<string\[\]>\(\[\]\);/);
  assert.match(page, /if \(arriveScopeRef\.current !== scope\) \{\s*arriveScopeRef\.current = scope;\s*seenTimelineKeysRef\.current = new Set\(keys\);\s*previousTimelineKeysRef\.current = keys;\s*setArrivingKeys\(\[\]\);\s*return;\s*\}/);
  // 判据只比 key、不比内容：流式回复每帧都在改 content，按内容比会每帧都算出一批"新的"。
  assert.match(page, /const fresh = keys\.filter\(\(key\) => !seen\.has\(key\)\);/);
  assert.doesNotMatch(page, /const fresh[\s\S]{0,120}message\.content/);
  // ③ "占位被正式消息顶替"不是新消息 —— 没有这条，`pending-*`/`stream-*` 换成真 id 时
  //    会重放一次入场（同位置同气泡又浮一下）。探针 12.8 用 animationstart 数真实播放次数。
  assert.match(page, /function isPlaceholderKey\(key: string\): boolean \{\s*return key\.startsWith\("pending-"\) \|\| key\.startsWith\("stream-"\);\s*\}/);
  assert.match(page, /const gone = previous\.filter\(\(key\) => !currentSet\.has\(key\)\);/);
  assert.match(page, /const replacedPlaceholder = gone\.length > 0\s*&& gone\.every\(\(key\) => isPlaceholderKey\(key\)\)\s*&& fresh\.length > 0\s*&& fresh\.length <= gone\.length;/);
  assert.match(page, /if \(fresh\.length > 0 && !replacedPlaceholder\) \{\s*setArrivingKeys\(\(current\) => Array\.from\(new Set\(\[\.\.\.current, \.\.\.fresh\]\)\)\);\s*\}/);
  // ④ 标记是**追加**不是覆盖：两条消息在 400ms 窗口内先后到达时，覆盖会把前一条的标记摘掉、
  //    等于在动画途中撤掉 animation 声明（元素瞬间跳到终态）。
  assert.doesNotMatch(page, /setArrivingKeys\(fresh\)/);
  // 标记的清理是**按动画结束摘**，不是按时间整体清空 —— 后者会误伤"在 400ms 窗口边缘刚加上"的
  // 新标记（那条的动画要么根本没开始、要么中途被撤掉；第三轮复查实测抓出来的）。
  // 兜底：没跑动画的（reduced-motion 下 animation: none）标记留到切会话时清。
  assert.match(page, /const clearArriving = useCallback\(\(key: string\) => \{\s*setArrivingKeys\(\(current\) => \(current\.includes\(key\) \? current\.filter\(\(item\) => item !== key\) : current\)\);\s*\}, \[\]\);/);
  assert.equal((page.match(/onAnimationEnd=\{\(event\) => \{ if \(event\.target === event\.currentTarget\) clearArriving\(entry\.key\); \}\}/g) || []).length, 2, "消息与状态卡两条渲染路径都要按动画结束摘标记");
  assert.doesNotMatch(page, /setTimeout\(\(\) => setArrivingKeys\(\[\]\), 400\)/);

  // ── ② 「正在写」三点：只对 stream-* 成立 ──────────────────────────────────
  // 与 isTransientMessage 分开是有意的：那个含 pending-*（乐观气泡，语义是"还没发出去"），
  // 把"还没发出去"说成"正在写"是**说错话**（与 transientReason 的三档同一条纪律）。
  assert.match(page, /function isStreamingMessage\(message: Message\): boolean \{\s*return message\.id\.startsWith\("stream-"\);\s*\}/);
  assert.match(page, /\{isStreamingMessage\(entry\.message\) && <span className="mobile-message-writing" role="img" aria-label="正在写入"><i aria-hidden="true" \/><i aria-hidden="true" \/><i aria-hidden="true" \/><\/span>\}/);
  // 复用页面里已有那条状态条的关键帧（同一种运动语言），且**降级后仍然看得见**。
  assert.match(styles, /\.mobile-message-writing i \{[^}]*animation: mobile-agent-processing-dot 1\.15s ease-in-out infinite;/s);
  assert.match(styles, /\.mobile-message-writing i:nth-child\(2\) \{ animation-delay: \.16s; \}/);
  assert.match(styles, /\.mobile-message-writing i:nth-child\(3\) \{ animation-delay: \.32s; \}/);
  assert.match(styles, /@media \(prefers-reduced-motion: reduce\) \{ \.mobile-message-writing i \{ animation: none; opacity: \.85; \} \}/);

  // ── ③ 图标行错开：三颗各 60ms，「⋯」只挂**展开态** ───────────────────────
  assert.match(motion, /\.mobile-message-actions \.mobile-message-action-icon \{ animation: mobile-action-pop 190ms cubic-bezier\(\.34, 1\.42, \.5, 1\) backwards; \}/);
  assert.match(motion, /\.mobile-message-actions \.mobile-message-action-icon:nth-of-type\(1\) \{ animation-delay: 0ms; \}/);
  assert.match(motion, /\.mobile-message-actions \.mobile-message-action-icon:nth-of-type\(2\) \{ animation-delay: 60ms; \}/);
  assert.match(motion, /\.mobile-message-actions \.mobile-message-action-icon:nth-of-type\(3\) \{ animation-delay: 120ms; \}/);
  // 「⋯」是每条气泡常驻的一颗：无条件加动画会让整页的「⋯」在挂载时一起弹（列表加载像在抖）。
  assert.match(motion, /\.mobile-message\[data-actions="open"\] \.mobile-message-head-actions > \.mobile-message-more \{\s*animation: mobile-action-pop 190ms cubic-bezier\(\.34, 1\.42, \.5, 1\) backwards; animation-delay: 180ms; \}/);

  // ── ④ 复制成功的勾：描边 + 回弹，且回弹**挂在 svg 上** ────────────────────
  // 挂在按钮上会与 ③ 那条 animation 互相覆盖 —— 后果不是"少一条"，而是每次
  // data-copy-state 变化都重建动画列表、把 pop 重放一遍（复位时图标自己再弹一下）。
  assert.match(page, /<path className="mobile-action-check" d="M4\.5 12\.5 9\.5 17\.5 19\.5 7" \/>/);
  assert.match(motion, /@keyframes mobile-action-check-draw \{ 0% \{ stroke-dashoffset: 22; \} 25% \{ stroke-dashoffset: 8; \} 100% \{ stroke-dashoffset: 0; \} \}/);
  assert.match(motion, /\.mobile-action-check \{ stroke-dasharray: 22; animation: mobile-action-check-draw 240ms/);
  assert.match(motion, /\.mobile-message \.mobile-message-action-icon\[data-copy-state="copied"\] svg \{\s*animation: mobile-action-check-nudge 380ms/);

  // ── ⑤ 引用胶囊入场 / ⑦ 按下反馈只给按钮 ──────────────────────────────────
  assert.match(motion, /\.mobile-quote-refs \.mobile-quote-ref \{ animation: mobile-quote-ref-in 200ms/);
  assert.match(motion, /\.mobile-message-action-icon:not\(:disabled\):active \{ transform: scale\(\.92\); \}/);
  // 气泡本身**不许**有 :active 缩放：它要能长按选词（本页刻意不抑制 contextmenu），
  // 整个气泡跟着缩会让选词时画面在抖。
  assert.doesNotMatch(motion, /\.mobile-message(?::active|\.user:active|\[data-actions="open"\]:active)/);

  // ── 全局约束 ①：只动 transform / opacity ────────────────────────────────
  // 这一页是整页滚动 + 有「贴底跟随」判据（scrollHeight 每帧参与运算）——
  // 任何改布局的动画都会每帧改动 scrollHeight，与它直接打架。
  for (const name of ["mobile-arrive-in", "mobile-notice-in", "mobile-action-pop", "mobile-action-check-nudge", "mobile-quote-ref-in"]) {
    const block = motion.match(new RegExp(`@keyframes ${name} \\{[^}]*\\}`))?.[0] || "";
    assert.ok(block, `缺关键帧 ${name}`);
    assert.doesNotMatch(block, /(?:^|[^\w-])(?:height|width|margin|padding|top|left|right|bottom)\s*:/, `${name} 不许动布局属性`);
  }

  // ── 全局约束 ②：入场类用 backwards，**不用 both** ────────────────────────
  // both 会 fill 到终态、一直压着 transform，于是 ⑦ 那点按反馈永远不生效
  // （动画播完后 transform 仍被 animation 覆盖）。这几条的终态本就等于正常样式。
  assert.doesNotMatch(motion, /animation:[^;]*\bboth\b/, "入场类动画不许用 both（会压住 :active）");
  assert.match(motion, /animation: mobile-arrive-in 260ms cubic-bezier\(\.25, \.8, \.25, 1\) backwards;/);

  // ── 全局约束 ③：每条动效都有降级分支，判据是"降级后信息不丢" ─────────────
  for (const rule of [
    '\\.mobile-message\\[data-arrive="true"\\], \\.mobile-notice\\[data-arrive="true"\\] \\{ animation: none; \\}',
    "\\.mobile-message-actions \\.mobile-message-action-icon,",
    "\\.mobile-action-check \\{ stroke-dasharray: none; animation: none; \\}",
    "\\.mobile-quote-refs \\.mobile-quote-ref \\{ animation: none; \\}",
  ]) {
    assert.match(motion, new RegExp(rule), `缺降级分支：${rule}`);
  }
});

test("mobile bubble content uses the structured briefing layout", () => {
  // 2026-09-13 从 4 个候选里选定 D：正文 15px / 1.66 行高 / 12px 段距。
  assert.match(styles, /\.mobile-message-markdown\s*\{[^}]*font-size:\s*15px;/s);
  // 行高必须写在 p 上：容器上的会被 `.mobile-message p { line-height: 1.55 }` 这条直接声明压掉。
  assert.match(styles, /\.mobile-message-markdown p\s*\{[^}]*margin:\s*0 0 12px;[^}]*line-height:\s*1\.66;/s);
  // 标题带左侧主题色竖条、列表符号用主题色。
  assert.match(styles, /\.mobile-message-markdown :is\(h1, h2, h3, h4, h5, h6\)\s*\{[^}]*border-left:\s*3px solid #4c9c83;/s);
  assert.match(styles, /\.mobile-message-markdown ul li::marker\s*\{[^}]*color:\s*#4c9c83;/s);
  assert.match(styles, /\.mobile-message-markdown ol li::marker\s*\{[^}]*color:\s*#4c9c83;/s);
  assert.match(styles, /\.mobile-message-markdown blockquote\s*\{[^}]*border-left-color:\s*#4c9c83;[^}]*font-style:\s*italic;/s);
  // 加粗用主题色，但只限 Agent 气泡：深绿底上深绿加粗看不见。
  assert.match(styles, /\.mobile-message:not\(\.user\) \.mobile-message-markdown strong\s*\{[^}]*color:\s*#1f6b57;/s);
  // 「我」的深绿气泡要用更亮的一档竖条 / 符号色。
  assert.match(styles, /\.mobile-message\.user \.mobile-message-markdown :is\(h1, h2, h3, h4, h5, h6\)\s*\{[^}]*border-left-color:\s*#7fd3b4;/s);
  assert.match(styles, /\.mobile-message\.user \.mobile-message-markdown :is\(ul, ol\) li::marker\s*\{[^}]*color:\s*#9fdcc4;/s);
});

test("mobile conversation page declares its own markdown stylesheet", () => {
  // 真机上 markdown.css 由 main.tsx 全局引入，所以"页面自己没 import"看不出来 ——
  // 但任何绕过 main.tsx 的入口（预览夹具、单页测试）都会渲染出裸的 pre / blockquote / 复制按钮。
  assert.match(page, /import "\.\.\/markdown\.css";/);
  assert.match(page, /import "\.\/mobile-remote\.css";/);
});

test("mobile keeps the last message above the composer when it grows", () => {
  // 输入条长高（多行 / 面板）时文档底部预留跟着变大，但浏览器会保持原 scrollY —— 最后几条会被压住。
  assert.match(page, /if \(height > lastHeight && stickToBottomRef\.current && !emptyThreadRef\.current\) \{\s*window\.scrollTo\(\{ top: document\.documentElement\.scrollHeight \}\);\s*\}/);
  // 空态那一半是**有意的例外**：空会话里根本没有"底部"可粘，而空态卡是"在顶栏与输入条之间
  // 居中"的，补一次贴底就把整页往上推几十像素、卡片顶被顶栏切掉（真机探针实测展开工具面板时
  // 33px）。没有这条断言，`!emptyThreadRef.current` 被谁顺手删掉是静默的。
  assert.match(page, /const emptyThreadRef = useRef\(false\);/);
  assert.match(page, /useEffect\(\(\) => \{ emptyThreadRef\.current = showConversationEmpty; \}, \[showConversationEmpty\]\);/);
});

test("mobile keyboard opening keeps a bottom-pinned conversation pinned", () => {
  // 真机现象（用户报的）：已经滚到最底部，点一下输入框 —— 输入条被键盘顶上去的同时，
  // 最后几条消息也被输入条 + 键盘压住，还得手动再划一下才看得见。
  //
  // 根因链：Manifest 是 adjustResize，键盘弹起时 WebView 整体变矮、页面 scrollY 却不动，
  // 于是"距底距离"凭空多出大半个键盘的高度；原来挂的 resize 监听直接拿新视口重算
  // 「贴底」，100px 容差当场被顶穿 —— stickToBottomRef 翻成 false，之后再也补不回来。
  // 修法：视口**在变矮的整段过程里**只补底、不重算；判据是"累计矮了多少"而不是"这一帧矮了
  // 多少"，否则键盘分几帧下发的机型会在中间那几帧被误判成"不是键盘"，重算照样把贴底顶掉。
  const anchors = ['const onResize = () => {', 'window.addEventListener("resize", onResize);'];
  for (const anchor of anchors) assert.ok(page.includes(anchor), `找不到锚点：${anchor}`);
  // 回归红线：resize 不能再直接把重算函数挂上去。
  assert.doesNotMatch(page, /window\.addEventListener\("resize", measure\)/);
  const handler = page.slice(page.indexOf(anchors[0]), page.indexOf(anchors[1]));
  const shrinkAt = handler.indexOf("if (height < settledViewportHeight) {");
  const branchEnd = handler.indexOf("settledViewportHeight = height;");
  const measureAt = handler.indexOf("measure();");
  assert.ok(shrinkAt >= 0 && branchEnd > shrinkAt && measureAt > branchEnd, "找不到「视口变矮」的分支或它后面的重算");
  const shrinkBranch = handler.slice(shrinkAt, branchEnd);
  // 变矮的过程中不能重算「贴底」（这一条就是本次修复本身）。
  assert.doesNotMatch(shrinkBranch, /measure\(\)/);
  // 补底的两个条件缺一不可：确实是键盘（累计矮了 80px 以上；地址栏收放只有五六十像素，
  // 整段都碰不到这条线）＋ 弹起前就贴着底（正在往上翻历史时不许把用户拽回来）。
  assert.match(shrinkBranch, /if \(settledViewportHeight - height > 80 && stickToBottomRef\.current\) \{\s*window\.scrollTo\(\{ top: root\.scrollHeight \}\);\s*\}/);
  assert.match(shrinkBranch, /return;/);
  // 基准线只允许在分支**之外**推进：写进分支里就退化成"逐帧比较"，分帧下发时照样漏判。
  // 两行之间留出余量：注释怎么写、中间插不插空行都不该让这条断言误报。
  assert.doesNotMatch(shrinkBranch, /settledViewportHeight\s*=/);
  assert.match(handler.slice(branchEnd), /^settledViewportHeight = height;[\s\S]{0,160}?measure\(\);/);
});

test("mobile task controls support dispatch, edit, and delete commands", () => {
  assert.match(page, /task\.dispatch/);
  assert.match(page, /task\.update/);
  assert.match(page, /task\.delete/);
  assert.match(page, /编辑任务/);
  assert.match(page, /确认删除/);
  assert.match(styles, /\.mobile-task-modal-backdrop/);
  assert.match(styles, /\.mobile-task-delete/);
});

test("mobile task controls reconcile busy and status changes without duplicates", () => {
  // 「编辑 / 删除」的可点规则原来写在 imperative 补丁里（两行赋值 + 靠 visibleTasks[index] 与行下标
  // 对齐，重复执行还会重复 append）。2026-09-16 改成 JSX 里的真按钮之后，"重复渲染产生重复按钮"
  // 这件事在结构上不可能发生；这里守的是同一套**权限规则**没有在搬家过程中走样：
  // 忙碌时两颗都禁用、「删除」在任务执行中禁用、「编辑」只在待处理 / 需处理时露出。
  //
  // 2026-09-17：这里判的 busy 从**全局** busy 换成了**这张卡自己的** taskSyncing。
  // 全局那一份会让用户在等一条命令时连别的任务都动不了，而命令往返真机上要几十秒
  // —— 一次下发就把整个面板冻住几十秒，那是比慢更糟的体验。
  assert.match(page, /const canEditTask = task\.status === "todo" \|\| task\.status === "action_required";/);
  assert.match(page, /const taskSyncing = pendingTaskBusy\.has\(task\.id\)/);
  assert.match(page, /data-mobile-task-edit="true" hidden=\{!canEditTask\} disabled=\{taskSyncing \|\| !canEditTask\}/);
  assert.match(page, /data-mobile-task-delete="true" disabled=\{taskSyncing \|\| task\.status === "running"\}/);
  assert.match(page, /任务描述不能为空/);
});

// 「标题可选、任务说明必填」是电脑端与 control-server 已经定下的字段语义：TaskBoard 把
// 「任务名称」标成"可选"、给「任务说明」挂 `required`；control-server `validateTaskInput`
// 也只在 description 为空时报 "task description is required"。
// 手机端原来把这条判在 `title` 上，正好两头都判反 —— 按电脑端习惯只填说明的用户会撞上一颗
// **静默禁用**的按钮（禁用态当时没有配色，外观与可点状态逐项相同，触屏上连 cursor 都看不到），
// 只填标题的用户则会被服务端打回来。2026-09-20 实测复现并修掉；这里把"哪一端必填"同时钉在
// DOM 与提交守卫两处，防止再次写反。
test("mobile task forms treat the title as optional and the description as required", () => {
  // ① 两处表单的字段声明：标题不再 required，描述 required。
  assert.match(page, /<label>标题（可选）<input value=\{title\} onChange=\{\(event\) => setTitle\(event\.target\.value\)\} placeholder="留空时用描述代替" \/>/);
  assert.match(page, /<label>描述<textarea value=\{description\} onChange=\{\(event\) => setDescription\(event\.target\.value\)\} required placeholder=/);
  assert.match(page, /<label>标题（可选）<input value=\{editTitle\} onChange=\{\(event\) => setEditTitle\(event\.target\.value\)\} placeholder="留空时用描述代替" \/>/);
  assert.match(page, /<label>描述<textarea value=\{editDescription\} onChange=\{\(event\) => setEditDescription\(event\.target\.value\)\} required rows=\{4\} \/>/);

  // ② 提交按钮只跟全局 busy 走：交给 `required` 出原生提示，不再拿"标题为空"把按钮按住。
  //    这里刻意用**正向**断言钉死整条 `disabled={busy}`，而不是补一条 `doesNotMatch(/\|\|/ )`
  //    之类的反向断言：反向断言扫的是整个源文件（含注释），任何一句说明里出现同样的表达式
  //    都会误报 —— 写这条用例时就真的被自己的注释绊过一次。整条属性钉死之后，
  //    `disabled={busy || 别的理由}` 会直接让下面的 match 失败，覆盖力反而更强。
  assert.match(page, /<button type="submit" disabled=\{busy\}>创建<\/button>/);
  assert.match(page, /<button type="submit" disabled=\{busy\}>保存<\/button>/);

  // ③ 两条提交守卫判的必须是 description，且留空时给**弹层内**的提示
  //    （页面级 `.mobile-error` 在遮罩之下，弹层不关就看不到 —— 那正是"点了保存、什么都没发生"）。
  assert.match(page, /const trimmedDescription = description\.trim\(\);[\s\S]{0,700}?if \(!trimmedDescription\) \{\s*setCreateError\("任务描述不能为空"\);/);
  assert.match(page, /const trimmedDescription = editDescription\.trim\(\);[\s\S]{0,400}?if \(!trimmedDescription\) \{\s*setEditError\("任务描述不能为空"\);/);
  assert.match(page, /\{editError && <p className="mobile-task-modal-error" role="alert">\{editError\}<\/p>\}/);

  // ④ 禁用的按钮必须看得出禁用（本页约定：走配色、不走 opacity）。
  //    两条选择器都要在：`:last-child` 那条特异性与第一条相同，只写一条就得靠书写顺序取胜。
  assert.match(styles, /\.mobile-task-modal footer button:disabled,\s*\.mobile-task-modal footer button:last-child:disabled \{ border-color: #d3e3da; color: #64857a; background: #e9f0ec;/);
  assert.match(styles, /\.mobile-task-modal header button:disabled \{/);
});

// 任务增删改的体验要求（用户 2026-09-17 明确提出）：在手机上**立刻生效、立刻有反馈**，
// 同步给电脑端可以异步。真机上命令往返 19~75 秒、手机端等待上限 30 秒，同步等待必然
// 表现为"点了没反应、最后还报失败"。
test("mobile task mutations apply locally before the command round trip", () => {
  // 项目列表必须是"快照 + 待确认改动"叠出来的，否则本地改完立刻被下一次快照顶掉。
  assert.match(page, /applyPendingTaskMutations\(snapshot\?\.projects \|\| \[\], pendingTaskRef\.current\.values\(\)\)/);
  assert.match(page, /import \{ applyPendingTaskMutations, mutationReflectsInSnapshot, newPendingTaskID, withResolvedCreateID, type PendingTaskMutation \} from "\.\.\/lib\/task-mutations";/);

  // 三个入口都必须在发命令**之前**先落本地状态；顺序反了就等于没做乐观更新。
  const createTask = page.slice(page.indexOf("async function createTask(event: FormEvent)"), page.indexOf("async function sendTaskCommand("));
  assert.match(createTask, /trackPendingTask\(\{[\s\S]{0,200}?kind: "create"/);
  assert.match(createTask, /setCreatingTask\(false\)[\s\S]{0,400}?await runTaskCommand\(/);
  // 弹层要在请求落地之前就收起来：用户已经看到任务出现在队列里了，没有理由再等。
  assert.doesNotMatch(createTask, /await waitForCommand/);

  const saveTaskEdit = page.slice(page.indexOf("async function saveTaskEdit(event: FormEvent)"), page.indexOf("async function confirmTaskDelete()"));
  assert.match(saveTaskEdit, /kind: "update"/);
  assert.match(saveTaskEdit, /setEditingTask\(null\)[\s\S]{0,200}?await sendTaskCommand\(/);

  const confirmDelete = page.slice(page.indexOf("async function confirmTaskDelete()"), page.indexOf("// 把一段文本放进草稿"));
  assert.match(confirmDelete, /kind: "delete"/);
  assert.match(confirmDelete, /setDeletingTask\(null\)[\s\S]{0,200}?await sendTaskCommand\(/);

  // 任务命令不再设全局 busy：一次下发不该把整个面板按住几十秒。
  assert.doesNotMatch(createTask, /setBusy\(/);
  assert.doesNotMatch(saveTaskEdit, /setBusy\(/);
  assert.doesNotMatch(confirmDelete, /setBusy\(/);
});

// 「同步中」是乐观更新必须配的那一半反馈：没有它，用户没法区分"已经同步好了"和
// "还在路上"，界面就成了在替云端撒谎。
test("mobile task cards show a per-card syncing state and roll back on failure", () => {
  assert.match(page, /data-syncing=\{taskSyncing \? "true" : undefined\}/);
  assert.match(page, /taskSyncing && <span className="mobile-task-syncing" role="status">同步中<\/span>/);
  assert.match(styles, /\.mobile-task-syncing \{/);
  assert.match(styles, /\.mobile-task\[data-syncing="true"\]/);
  // 动效要能被系统设置关掉。
  assert.match(styles, /@media \(prefers-reduced-motion: reduce\) \{ \.mobile-task-syncing::before \{ animation: none;/);

  // 命令明确失败必须回滚本地改动，并且把原因说出来。
  assert.match(page, /if \(outcome\.error\) \{\s*dropPendingTask\(taskID\);\s*setTaskBusy\(taskID, false\);\s*setError\(outcome\.error\);/);
  // 只是"还没拿到回执"则**什么都不撤**：30 秒没等到终态不等于电脑端没做，撤掉卡片
  // 就是把一张可能已经建好的任务从用户眼前删掉。
  assert.match(page, /if \(!outcome\.settled\) \{\s*setError\("电脑端还没有回执，操作可能仍在进行；同步完成后会自动更新"\);\s*return;\s*\}/);
  // 成功之后也要等快照真的反映了结果才摘掉「同步中」：快照有上传节流，
  // 命令成功那一刻拿到的往往还是命令之前那一版。
  assert.match(page, /if \(mutationReflectsInSnapshot\(mutation, \(result\.snapshot \|\| snapshot\)\?\.projects, pendingTaskRef\.current\.values\(\)\)\) dropPendingTask\(taskID\);/);
  // 新建拿到真 id 后就地替换：判据从"标题+描述"变成精确的 id，用户连建两条同名任务
  // 时第二条不会被第一条误判成已落地。
  // 换 id 时键、值、以及卡片上的「同步中」必须一起换，收尾也要用换完的那个 id ——
  // 漏掉任何一个，卡片都会在同步途中丢掉「同步中」，成功对账也找不回它。
  assert.match(page, /const settleID = created \? resolvePendingCreate\(optimisticID, created\.id, outcome\.commandId\) : optimisticID;/);
  assert.match(page, /await settleTaskMutation\(settleID, outcome, \(reason\) => \{/);
  assert.match(page, /setTaskBusy\(pendingID, false\);\s*setTaskBusy\(resolved\.taskId, true\);/);
  assert.match(page, /const resolved = \{ \.\.\.withResolvedCreateID\(mutation, realID\), commandId \};\s*if \(resolved\.taskId === pendingID\) return pendingID;/);
  // 快照没拉回来时保留本地改动，别让用户刚建的任务凭空消失一次。
  assert.match(page, /if \(result\.outcome === "failed"\) \{\s*setTaskBusy\(taskID, false\);\s*setError\("已同步到电脑端，但云端快照还没更新，稍后刷新即可"\);\s*return;\s*\}/);
  // 创建失败要把用户刚打的字放回输入框：任务已经被撤掉了，至少不让他重打一遍。
  // （2026-09-17 起中间多了一句 setCreatePriority：优先级也要一起放回去，见下面的优先级用例。）
  assert.match(page, /setTitle\(trimmedTitle\);\s*setDescription\(trimmedDescription\);\s*setCreatePriority\(chosenPriority\);\s*setCreateError\(reason\);\s*setCreatingTask\(true\);/);
  // 待确认改动按实例隔离，换电脑后不能叠到新电脑的快照上。
  assert.match(page, /if \(pendingTaskRef\.current\.size > 0\) \{\s*pendingTaskRef\.current\.clear\(\);/);
  // 每一次快照落地都要对一次账：命令没在 30 秒窗口内拿到回执时我们故意不回滚，
  // 本地那张卡只能靠快照自己收敛 —— 少了这个 effect，「同步中」会一直挂着。
  assert.match(page, /if \(!mutationReflectsInSnapshot\(mutation, snapshot\.projects, current\.values\(\)\)\) continue;/);
  assert.match(page, /current\.delete\(mutation\.taskId\);/);
});

test("mobile pairing QR carries the pairing handle plus the one-time code", () => {
  // 症状（2026-09-17 用户报的）：手机扫了电脑上刚生成的二维码，"手机端没有任何反应"，
  // 最后还是必须手输 6 位校验码 —— 扫码这条链路等于白给。
  // 根因：二维码 URL 里被删掉了 code，而手机端 acceptScannedPairing() 只有读到 6 位数字
  // 才会真的调 claimPairing()；只给 pairingId 就只是"预填了一个会话句柄"。
  // 修法：把校验码写进二维码（只有 6 位才写；伺服端没给就退回"手输校验码"那条老路）。
  assert.match(page, /function pairingURLWithCode\(value: string \| undefined, pairingID: string, pairingCode: string\): string/);
  assert.match(page, /parsed\.searchParams\.set\("pairingId", pairingID\);/);
  // 两行必须成对且保持这个顺序：只写不删（脏 URL 复用）或只删不写（扫码失效）都要挡。
  assert.match(page, /if \(\/\^\\d\{6\}\$\/\.test\(pairingCode\)\) parsed\.searchParams\.set\("code", pairingCode\);\s*else parsed\.searchParams\.delete\("code"\);/);
  // 生成二维码那一处必须把 code 传进来：少传一个实参，扫码就只能退回手输。
  assert.match(page, /pairingURLWithCode\(value\.pairingURL, value\.pairingId \|\| "", value\.code \|\| ""\)/);
  // 扫码只解析、不加载这个 URL（配对句柄不能当页面地址用）。
  assert.match(page, /if \(\/\^\\d\{6\}\$\/\.test\(scanned\.code\)\)[\s\S]*?claimPairing\(scanned\.pairingID, scanned\.code\)/);
});

test("mobile QR scans without an embedded code fall back to manual verification", () => {
  assert.match(page, /setPairingID\(scanned\.pairingID\);/);
  assert.match(page, /setPairingCode\(scanned\.code \|\| ""\);/);
  // 兜底提示也是一条"状态行"，必须带 state —— 见下面那组配对状态分级的用例。
  assert.match(page, /setPairingNotice\(\{ text: "[^"]*6 位数字", state: "idle" \}\);/);
  // 兜底那条路（手输 6 位校验码）现在长在配对页里，与扫码同为这一页的入口之一。
  assert.match(page, /className="mobile-pairing-code"/);
  assert.match(page, /\/v1\/pairings\/claim/);
});

test("mobile pairing states are graded and the notice outlives the pairing panel", () => {
  // 症状（2026-09-17 用户报）：扫码之后"手机端没有任何反应"。当时的实现其实写了提示，
  // 但那是一行 12px 绿字、且**挂在配对面板内部**：绑定成功后 showMobilePairing 立刻变 false，
  // 面板连同"电脑已确认，绑定完成"一起卸载，成功的确认根本没人看见过。
  assert.match(page, /type PairingNotice = \{ text: string; state: PairingNoticeState \};/);
  assert.match(page, /type PairingNoticeState = "idle" \| "waiting" \| "success" \| "error";/);
  // 三态各自的出现点：等待（提交后等电脑）/ 成功（电脑已确认）/ 失败（失效）。
  assert.match(page, /state: "waiting" \}\);/);
  assert.match(page, /state: "success" \}\);/);
  assert.match(page, /state: "error" \}\);/);
  // 状态行在 2026-09-20 之后是**一个元素、两处挂载点**：配对页里（用户正看着这一页）
  // 与页面级（页面关掉之后它才是唯一回执）。旧版的病是"只有一处、而且在面板里" ——
  // 绑定成功后面板与那句"电脑已确认，绑定完成"一起卸载，用户什么都看不到。
  // 现在靠两条结构性约束守住它，两条都要断言（光断言"这个类名在不在"挡不住回归）：
  //  ① 元素是页面级的一个 `const`，不在配对页的 JSX 子树里（所以不可能被页面的卸载带走）；
  //  ② 页内那一处**只服务 idle / error**（waiting 由等待卡原话说过了、success 必须留在页外），
  //     页面级那一处要求页面已关闭。
  assert.match(page, /const pairingNoticeBox = pairingNotice \? <p className={`mobile-pairing-notice/);
  assert.match(page, /const pairingPageNotice = pairingNotice && \(pairingNotice\.state === "idle" \|\| pairingNotice\.state === "error"\) \? pairingNoticeBox : null;/);
  assert.match(page, /\{pairingPageNotice\}/);
  assert.match(page, /\{!showMobilePairing && pairingNotice && \(!mobileApp \|\| mobileView === "projects"\) && pairingNoticeBox\}/);
  // 绑定成功与"收起配对页"必须是同一次更新里的两个动作，否则成功提示会先渲染在页里再被卸载。
  assert.match(page, /setPairingExpanded\(false\);\s*setPairingNotice\(\{ text: "电脑已确认，绑定完成", state: "success" \}\);/);
  // 三态要用 data-state 落到 DOM 上（CSS 的配色/图标全挂在它上面）。
  assert.match(page, /data-state=\{pairingNotice\.state\}/);
  // ⚠️ 挂进配对页时必须显式给宽度：页面正文是 flex 列，而这一条带 `margin: 0 auto` ——
  // **交叉轴上的 auto 外边距会取消 stretch**，宽度退回 fit-content，一条本该通栏的结果条
  // 会缩成一颗居中的小胶囊（与它在项目页 / 电脑页的样子不是同一个东西）。
  assert.match(styles, /\.mobile-pairing-page \.mobile-pairing-notice \{ width: 100%;/);
  // 等待态要有 spinner（12px 的圆点），成功态定时自己走。
  assert.match(page, /pairingNotice\.state === "waiting" && <span className="mobile-pairing-notice-spinner"/);
  assert.match(page, /if \(pairingNotice\?\.state !== "success"\) return;\s*const timer = window\.setTimeout\(\(\) => setPairingNotice\(null\), 8000\);/);
  // 重开配对页要清掉上一轮的两样残留：状态行（否则上次的"配对已失效"会被读成这次的结果）
  // 与六格里的校验码（一次性凭据；留着它格子是满的、提交键是亮的，点下去只会拿到"配对已失效"）。
  // 清状态行还有一条结构性作用：这一页**必须总是从第 1 步打开**，否则上一轮那个还没确认的
  // waiting 会把用户困在第 2 步 —— 那一屏没有任何回到"扫码 / 输码"的出口。
  // 顺带记住"是谁打开的"：关闭时焦点要还回去（见配对页那个焦点管理 effect）。
  // 按**函数切片**断言，不写整段正则：这个函数已经被改过三轮，整段锚点每回都会因为
  // "多了一行无关的话"假红（见 TOOLING「锚点越长越脆」）。
  {
    const openAt = page.indexOf("function openPairingPanel() {");
    const openSlice = page.slice(openAt, page.indexOf("\n  }", openAt));
    assert.ok(openAt > 0 && openSlice.length > 0, "没切到 openPairingPanel");
    assert.match(openSlice, /pairingTriggerRef\.current = active instanceof HTMLElement/);
    assert.match(openSlice, /setPairingNotice\(null\);/);
    assert.match(openSlice, /setManualPairingCode\(""\);/);
    // ⚠️ 设备面板必须在这里收掉：两者同为 `position: fixed; z-index: 20`，而面板在 DOM 里
    // 更靠后 —— 同时开着就是"面板盖在配对页上"（用户看到的是"点了没反应"）。
    // 写在这个共用出口里，三条调用路径就都盖住了。
    assert.match(openSlice, /setDevicesOpen\(false\);/);
    assert.match(openSlice, /setPairingExpanded\(true\);/);
  }
  // 收页时**只有"等待电脑确认"那条要留**：页面一关，它就是用户在项目列表上唯一的进度指示
  // （配对还在进行，电脑点确认依然生效 —— 收掉它等于把"还在等"变成"什么都没发生"）。
  // 其余三态跟着页面一起收，否则下次开页会看到上一轮的结果。
  assert.match(page, /function closePairingPanel\(\) \{\s*setPairingExpanded\(false\);\s*setPairingNotice\(\(current\) => \(current\?\.state === "waiting" \? current : null\)\);\s*\}/);
  // 「重新配对此设备」并进了设备面板底部的「＋ 添加电脑或重新配对」——
  // 它们本来就是同一个扫码流程，分成两颗按钮等于同一动作给两个入口。
  assert.match(page, /onClick=\{\(\) => \{ setDevicesOpen\(false\); openPairingPanel\(\); \}\}>＋ 添加电脑或重新配对</);
  // 首启空态卡上的第二处入口（还没有任何电脑时页面上只有它）。
  assert.match(page, /<button type="button" onClick=\{openPairingPanel\}>＋ 添加电脑<\/button>/);
  // 配对页的两个出口：头部左边的「返回项目」与右边的「关闭」，都走同一个 closePairingPanel。
  // （旧版那颗贴底的「返回项目」已被删除，见下方 `doesNotMatch`。）
  assert.match(page, /className="mobile-pairing-back" type="button" onClick=\{closePairingPanel\}/);
  assert.match(page, /className="mobile-pairing-close" type="button" onClick=\{closePairingPanel\}/);
  assert.doesNotMatch(page, /className="mobile-pairing-collapse"/);
  // 负向断言一律对 `styleRules`（已剥注释）比 —— 那段"这一组已删"的注释里写着选择器全名。
  assert.doesNotMatch(styleRules, /\.mobile-pairing-collapse/);
});

test("mobile pairing is a full page with three visible steps instead of two inline blocks", () => {
  // 症状（2026-09-20 用户原话）："现在是在上面显示扫码配对等东西，我需要的是一个弹窗或者页面显示"。
  // 旧实现是两块**内联**在项目列表之上：`mobile-pairing`（扫码）+ `mobile-pairing-manual`（校验码），
  // 各带一个 h2 —— 读起来像页面有两件事要做，实际是同一件事的两种做法；而且它把项目列表整体推下去。
  // ⚠️ 样式侧一律对 `styleRules`（已剥注释）比：用来解释"这一组为什么被删"的注释里
  // 原样写着这几个选择器，对 `styles` 比会被自己的注释满足（TOOLING「断言前先剥注释」）。
  assert.doesNotMatch(page, /className="mobile-pairing"/);
  assert.doesNotMatch(page, /className="mobile-pairing-manual"/);
  assert.doesNotMatch(styleRules, /^\.mobile-pairing \{/m);
  assert.doesNotMatch(styleRules, /\.mobile-pairing-manual/);
  assert.doesNotMatch(styleRules, /\.mobile-pairing-generate/);
  // 旧面板那套按钮视觉（实心深绿 + 5px 圆角）一条都不该留：这一页的主次是靠形态区分的。
  assert.doesNotMatch(styleRules, /\.mobile-pairing (button|small|form|h2|p) \{/);

  // 它现在是**一整页**（与任务面板同档），不是浮在列表上的一张卡。
  assert.match(styles, /\.mobile-pairing-page \{ position: fixed; z-index: 20; inset: 0; display: flex; flex-direction: column;/);
  assert.match(styles, /\.mobile-pairing-page-body \{[^}]*flex: 1;[^}]*min-height: 0;[^}]*overflow-y: auto;/s);
  // 程序化聚焦的容器不画聚焦环（与任务面板 / 设备面板同一套）。
  assert.match(styles, /\.mobile-pairing-page:focus \{ outline: none; \}/);
  assert.match(page, /<section className="mobile-pairing-page" ref=\{pairingPageRef\} tabIndex=\{-1\} role="dialog" aria-modal="true"/);

  // ⚠️ 容器**不能**叫 `.mobile-pairing`：文件上半部分那条旧规则 `.mobile-pairing button`(0,1,1)
  // 会把这一页所有按钮涂成实心深绿，主次关系（实心＝扫码 / 描边＝校验码）当场消失。
  assert.match(styles, /\.mobile-pairing-scan-button \{[^}]*color: #fff;[^}]*background: #2c7567;/s);
  assert.match(styles, /\.mobile-pairing-code-submit \{[^}]*color: #2c7567; background: #fff;/s);

  // 三步指示器：状态由 `pairingStep` 派生，不许写成三个布尔。
  assert.match(page, /const pairingStep = pairingNotice\?\.state === "success" \? 3 : pairingNotice\?\.state === "waiting" \? 2 : 1;/);
  assert.match(page, /<li data-state=\{pairingStep > 1 \? "done" : "current"\}><i>1<\/i>扫码 \/ 输码<\/li>/);
  assert.match(page, /<li data-state=\{pairingStep > 2 \? "done" : pairingStep === 2 \? "current" : "todo"\}><i>2<\/i>电脑上确认<\/li>/);
  // 小字必须过 4.5:1 那道闸：未到的那步 #587568（在 #f4faf6 上 4.76），当前/已完成 #2f6a5a（5.90）。
  // "到没到"靠数字圈的填充表达，不靠把字调淡。
  assert.match(styles, /\.mobile-pairing-steps li \{[^}]*color: #587568;[^}]*\}/);
  // ⚠️ done 与 current **必须长得不一样**：第一版两档都是实心绿圈，走到第 2 步时屏上出现
  // 两个一模一样的圈，"哪一步正在做"就没了（探针截图抓到）。现在 done 是浅绿底 + 主色数字、
  // current 是实心主色 + 白数字。
  assert.match(styles, /\.mobile-pairing-steps li\[data-state="done"\] i \{[^}]*color: #2c7567; background: #e8f7ee; \}/);
  assert.match(styles, /\.mobile-pairing-steps li\[data-state="current"\] i \{[^}]*color: #fff; background: #2c7567; \}/);

  // 等待电脑确认那一屏 —— 整页流程存在的全部理由（旧实现扫完就退回去，"现在轮到电脑"没人说）。
  assert.match(page, /pairingStep === 2\s*\? <div className="mobile-pairing-wait" role="status">/);
  assert.match(page, /<strong>已提交，等待电脑确认<\/strong>/);
  // 出口的措辞必须是「先返回项目」而不是「取消」：手机上根本没有"取消配对"这个接口，
  // 会话还在云端挂着，电脑点确认依然生效 —— 写成"取消"是骗人。
  assert.match(page, /<button type="button" onClick=\{closePairingPanel\}>先返回项目<\/button>/);
  assert.match(styles, /\.mobile-pairing-wait-spin \{[^}]*animation: mobile-refresh-spin/s);
  // ⚠️ 这两条 reduced-motion 覆盖写成**自带祖先**的两段式（(0,2,0)），而不是裸类名（(0,1,0)）：
  // 裸类名与基础规则同特异性，生效完全依赖"写在基础规则之后" —— 属于"靠位置生效"的规则，
  // 本文件在这类规则上已经栽过三次（见 TOOLING），代价为零的解耦就不赌。
  // ⚠️ 注意别把这条纪律写成"压缩产物里顺序一定会乱"：2026-09-20 我用文本位置扫 dist，
  // 得出过"覆盖被挪到基础规则之前"的结论，实际那个 @media 块很大、基础规则在块内部（在后），
  // 拿浏览器量是生效的 —— **产物里的顺序只能实测，不能推理**。真正判定"有没有生效"的是
  // `probe-dist-header.mjs` 里那对实测断言（默认环境必须在转 / reduce 下必须停）。
  assert.match(styles, /@media \(prefers-reduced-motion: reduce\) \{ \.mobile-pairing-wait \.mobile-pairing-wait-spin \{ animation: none; \} \}/);
  assert.match(styles, /@media \(prefers-reduced-motion: reduce\) \{ \.mobile-pairing-notice \.mobile-pairing-notice-spinner \{ animation: none; \} \}/);
  // 反例：退回裸类名就重新变成"靠顺序"，这条断言把形状钉住。
  assert.doesNotMatch(styleRules, /@media \(prefers-reduced-motion: reduce\) \{ \.mobile-pairing-wait-spin \{/);
  assert.doesNotMatch(styleRules, /@media \(prefers-reduced-motion: reduce\) \{ \.mobile-pairing-notice-spinner \{/);

  // **不画假取景框**：原生分支的摄像头由插件画在 WebView 之下，页面里画一个"框"只会是一块
  // 假的取景区（真画面只在 `.mobile-scan` 那层透出来）。所以这一格回答的是"二维码在哪一页"。
  assert.match(page, /<figure className="mobile-pairing-figure">/);
  assert.match(page, /电脑端：「远程控制」→「生成二维码」/);
  assert.doesNotMatch(page, /mobile-pairing-viewfinder/);

  // 六格校验码：一个透明的真 input 盖在 6 个格子上。那个 input 必须
  // ① 留着 `aria-label="6 位校验码"`（扫到不带校验码的二维码时要把焦点搬过去，探针按它找）；
  // ② 用更高特异性压掉 `.mobile-remote input`(0,1,1) 的 100% 宽 + 白底，否则六格布局会散掉。
  assert.match(page, /<input id="mobile-pairing-code" ref=\{manualCodeRef\} inputMode="numeric" pattern="\[0-9\]\{6\}" maxLength=\{6\}/);
  assert.match(page, /aria-label="6 位校验码"/);
  assert.match(page, /\{\[0, 1, 2, 3, 4, 5\]\.map\(\(index\) => <i key=\{index\} aria-hidden="true" data-filled=\{manualPairingCode\.length > index \? "true" : "false"\}/);
  assert.match(styles, /\.mobile-pairing-page \.mobile-pairing-code-field input \{[^}]*position: absolute;[^}]*color: transparent;/s);
  // 焦点指示由整排格子承担，所以真 input 自己不画环；这条必须写成 (0,3,1) 才压得过
  // `.mobile-remote input:focus-visible`(0,2,1) 的琥珀 outline。
  assert.match(styles, /\.mobile-pairing-page \.mobile-pairing-code-field input:focus-visible \{ outline: none; \}/);

  // 首启（一台电脑都没有）＝空态卡，且此时「选择项目」整段不渲染 ——
  // 摆一个空标题加一行"暂无项目"就是"有下一步动作的空态禁止一行灰字"那条规则点名不许的东西。
  assert.match(page, /\{showPairingStart && <section className="mobile-pairing-start">/);
  assert.match(page, /<h2>还没有连接电脑<\/h2>/);
  assert.match(page, /mobileView === "projects" && !showPairingStart && <><section className="mobile-project-picker"/);
  assert.match(styles, /\.mobile-pairing-start-icon \{[^}]*border-radius: 50%;/);

  // 判据：不再自己冒出来（那会把"读不到实例列表"也当成"还没配对"），只认用户主动打开；
  // 首启空态卡只认**令牌**，不认 instances.length。
  assert.match(page, /const showMobilePairing = mobileApp && mobileView === "projects" && pairingExpanded;/);
  assert.match(page, /const showPairingStart = mobileApp && mobileView === "projects" && !pairingExpanded && !token\.trim\(\);/);

  // 浮层清退两处都要收：① 返回键 if 链；② leaveConversationView（popstate / 侧滑不走 ①）。
  assert.match(page, /if \(pairingExpanded\) \{ closePairingPanel\(\); return true; \}/);
  {
    const leaveAt = page.indexOf("function leaveConversationView() {");
    const leaveBlock = leaveAt > 0 ? page.slice(leaveAt, page.indexOf("\n  }", leaveAt)) : "";
    assert.ok(leaveBlock.includes("setPairingExpanded(false);"), "侧滑返回那条链也要收配对页");
    // 状态行**不跟着收**：等待电脑确认那条是用户退回来之后唯一的进度指示。
    assert.ok(!leaveBlock.includes("setPairingNotice(null);"), "leaveConversationView 不该把等待回执一起清掉");
  }

  // 焦点管理：与另外两个整页浮层同一套（搬焦点 + 背景 inert + 关闭还焦点）。
  assert.match(page, /const pairingPageRef = useRef<HTMLElement \| null>\(null\);/);
  assert.match(page, /const pairingTriggerRef = useRef<HTMLElement \| null>\(null\);/);
  assert.match(page, /if \(!showMobilePairing\) return;\s*const panel = pairingPageRef\.current;/);
});

test("desktop pairing QR expires visibly instead of staying scannable", () => {
  // 症状：二维码 5 分钟到期后只换了一句话，那张**已经扫不动的码还挂在屏幕上**，
  // 用户拿着它反复扫，手机端只会回"配对已失效"。
  assert.match(page, /const \[pairingExpiresAt, setPairingExpiresAt\] = useState\(""\);/);
  // 有效期与二维码同一次落地：分开写会让倒计时先渲染出 0:00、二维码闪一帧"已失效"。
  assert.match(page, /setPairingExpiresAt\(value\.expiresAt \|\| ""\);\s*setPairingSecondsLeft\(secondsUntil\(value\.expiresAt\)\);/);
  assert.match(page, /const pairingExpired = Boolean\(pairingID\) && pairingExpiresAt !== "" && pairingSecondsLeft <= 0 && !pairingConfirmed;/);
  assert.match(page, /data-expired=\{pairingExpired \? "true" : "false"\}/);
  // 有效期这片界面在 2026-09-17 从手机页搬到了电脑端自己的卡片里（`desktop-remote-*`）：
  // 手机端那一支根本走不到生成二维码（`setPairingURL` 只在电脑端被调用），
  // 所以手机页里那套 `mobile-pairing-qr*` 也一并删了。
  assert.match(page, /className="desktop-remote-qr-dead"/);
  assert.match(desktopStyles, /\.desktop-remote-qr-wrap\[data-expired="true"\] \.desktop-remote-qr/);
  assert.match(desktopStyles, /\.desktop-remote-qr-wrap\[data-spent="true"\] \.desktop-remote-qr/);
  // 失效后确认按钮不可点，避免"点了确认却绑定不上"。
  assert.match(page, /disabled=\{busy \|\| !pairingReadyForConfirm \|\| pairingExpired \|\| pairingConfirmed\}/);
  // 倒计时按云端 expiresAt 走秒，解析不出来就不显示（别自己编一个 5 分钟）。
  assert.match(page, /function secondsUntil\(value: string \| undefined\): number \{/);
  assert.match(page, /return `\$\{Math\.floor\(seconds \/ 60\)\}:\$\{String\(seconds % 60\)\.padStart\(2, "0"\)\}`;/);
});

test("mobile unbind asks for confirmation and names the device it detaches", () => {
  // 解绑会云端吊销令牌，不可逆。旧版是一颗裸文本按钮，误触直接解绑。
  // 多电脑之后它还必须是**指名道姓**的：存的是设备令牌而不是一个 boolean，
  // 否则"确认解除"会解到当前那台，而不是用户点的那台。
  assert.match(page, /const \[confirmUnbind, setConfirmUnbind\] = useState\(""\);/);
  assert.match(page, /function requestUnbind\(deviceToken: string\) \{\s*setConfirmUnbind\(deviceToken\);\s*\}/);
  // 解绑入口只剩一处：设备面板里每行右侧那颗（页面底部那颗已并入面板）。
  // 名字走 `deviceDisplayName`（备注优先）—— 与列表里显示的那个名字必须一致，
  // 否则确认框问的是"要断这台吗"，用户却在列表里找不到那个名字。
  assert.match(page, /onClick=\{\(\) => requestUnbind\(item\.token\)\} disabled=\{busy\} aria-label=\{`解绑「\$\{deviceDisplayName\(item\)\}」`\}>解绑</);
  assert.doesNotMatch(page, /className="mobile-pairing-actions"/);
  // 确认框按对象分岔：当前那台走 unbindDevice（用它自己的令牌吊销），其余走 unbindDeviceByToken。
  assert.match(page, /const target = confirmUnbind;\s*setConfirmUnbind\(""\);\s*if \(!target\) return;\s*if \(target === readActiveToken\(\)\) \{\s*await unbindDevice\(\);\s*return;\s*\}\s*await unbindDeviceByToken\(target\);/);
  assert.match(page, /async function unbindDeviceByToken\(deviceToken: string\) \{/);
  assert.match(page, /id="mobile-unbind-title">解除绑定/);
  assert.match(page, /onClick=\{\(\) => void confirmUnbindDevice\(\)\}/);
  // 浮层清退的两处都要有它：① 返回键链 ② 侧滑返回那条链（popstate 不走 ①）。
  assert.match(page, /if \(confirmUnbind\) \{ setConfirmUnbind\(""\); return true; \}/);
  // 按函数切片断言，不写"两个标记之间不超过 N 字符"的字符预算 —— 中间加一行注释就会假红。
  const leaveStart = page.indexOf("function leaveConversationView() {");
  const leaveSlice = page.slice(leaveStart, page.indexOf("\n  }", leaveStart));
  assert.ok(leaveStart > 0 && leaveSlice.length > 0, "没切到 leaveConversationView");
  assert.match(leaveSlice, /setConfirmUnbind\(""\);/);
  assert.match(page, /confirmShortcut \|\| confirmUnbind\)\) return true;/);
});

test("mobile unbind keeps the component in step with the device store", () => {
  // ⚠️ 2026-09-20 抓到的既有缺陷（被"首启空态卡"放大了）：`removeDevice` 在摘掉当前设备时
  // 会**顺手把"当前设备"顺移到下一台可用的电脑**（`mobile-devices.ts` 里那句
  // `setActiveToken(next.find((item) => !item.revoked)?.token || "")`），而 `unbindDevice`
  // 却写死 `setToken("")` —— 组件状态与 storage 从此刻开始打架：store 里当前设备是 B，
  // 页面手上的 `token` 是空串，于是按"还没配过对"渲染出首启空态卡（"还没有连接电脑 / ＋ 添加电脑"），
  // 用户会以为所有配对一起没了（以前只是让内联配对块多显示一次，看不出是错的）。
  assert.doesNotMatch(page, /setToken\(""\); setInstances\(\[\]\);/);
  // 按**函数切片**断言，不写跨行的整段正则：注释插在语句之间就会假红（同文件里已经栽过几次）。
  {
    const unbindAt = page.indexOf("async function unbindDevice() {");
    const unbindSlice = page.slice(unbindAt, page.indexOf("\n  }", unbindAt));
    assert.ok(unbindAt > 0 && unbindSlice.length > 0, "没切到 unbindDevice");
    assert.match(unbindSlice, /const nextToken = readActiveToken\(\);/);
    assert.match(unbindSlice, /setToken\(nextToken\);/);
    // 还有别的电脑可用时不该弹配对页（那不是"必须重新配对"，只是这台没了）；
    // 一台都不剩时才弹 —— 否则用户面对的是"没有电脑、也没有入口"。
    assert.match(unbindSlice, /nextToken \? "已解除绑定，已切到另一台电脑" : "已解除绑定，请重新配对"/);
    assert.match(unbindSlice, /setPairingExpanded\(!nextToken\);/);
    // ⚠️ 要弹配对页的那一支必须先收掉设备面板：两者同为 `position: fixed; z-index: 20`，
    // 而面板在 DOM 里更靠后 —— 同时开着就是"面板盖住配对页"，用户以为点了没反应。
    assert.match(unbindSlice, /if \(!nextToken\) setDevicesOpen\(false\);/);
    // 顺移之后必须重新拉实例：设备卡的渲染条件是 `instance` 存在，而设备面板只能从那张卡进，
    // 不拉的话用户既看不到卡、也没有入口去切别的电脑。
    assert.match(unbindSlice, /if \(nextToken\) void loadInstances\(\);/);
  }
  // 令牌失效那条路**不走** `openPairingPanel`（它要先把"为什么"写进状态行，不能走那个会清空
  // 状态行的出口），所以设备面板要在那里自己收 —— 漏了同样是"面板盖在配对页上、像没反应"。
  //
  // 它同时与 `unbindDevice` 共用同一条"顺移"语义（2026-09-20 第二轮自查补齐，两条都要断言）：
  //  ① 被顶掉的正是当前这台时，切到下一台**可用**的电脑，而不是把令牌清空 —— 否则文案说
  //     "可切换到其它电脑"，而「切换」入口就在配对页背后那一屏（打开时它是 inert 的，够不着）；
  //  ② 顺移必须**写 storage**：`cloud()` 是从 storage 取令牌的（`deviceToken ?? readActiveToken()`），
  //     只改组件 state 的话界面显示已切、请求仍带着被吊销那台的令牌（继续 401），
  //     而且下次冷启动会再撞一次、永远好不了（`switchDevice` 里那两句成对出现就是这个原因）。
  {
    const at = page.indexOf("const onTokenCleared = () => {");
    const slice = page.slice(at, page.indexOf('globalThis.addEventListener("milevia:token-cleared"', at));
    assert.ok(at > 0 && slice.length > 0, "没切到 onTokenCleared");
    assert.match(slice, /setDevicesOpen\(false\);/);
    assert.match(slice, /remaining\.find\(\(item\) => !item\.revoked\)\?\.token \|\| ""/);
    assert.match(slice, /setActiveToken\(keep\);/);
    assert.match(slice, /setToken\(keep\);/);
    assert.match(slice, /setPairingExpanded\(!keep\);/);
    assert.match(slice, /if \(keep\) void loadInstances\(\);/);
    // 反例：写死 `setPairingExpanded(true)` 就是"不管还有没有别的电脑，一律把人按在配对页上"。
    assert.doesNotMatch(slice, /setPairingExpanded\(true\);/);
  }

  // 深链（系统相机扫二维码 → 打开这个 URL）进来的人已经在流程中间：提交成功后要把配对页一起打开，
  // 否则他看到的是项目列表上一条孤零零的回执。放在**提交成功之后**（不是进函数就开）——
  // 提交在路上时露出第 1 步那颗可点的「扫描二维码」会让人以为还能重来一次。
  assert.match(page, /setPairingExpanded\(true\);\s*setPairingNotice\(\{ text: "已扫码并提交，请在电脑上点击「确认绑定」", state: "waiting" \}\);/);
  // 应用内扫码那条路页面本来就开着，所以上面那一句对它必须是 no-op —— 两个入口共用同一条链，
  // 也就不用再各自记着"开页"这件事（`acceptScannedPairing` 里那句 `claimPairing` 是唯一入口，
  // 由 506 号用例盯着；这里不写"两个锚点之间不超过 N 字符"的字符预算，
  // 中间加一行注释就会假红 —— 见 TOOLING）。
});

test("mobile never loses its device entry points when the instance read fails", () => {
  // 来源：2026-09-20 的手机端可用性排查（探针实测复现过）。
  // 症状：有令牌、但冷启动时 `/v1/instances` 读不到（断网/云端故障，或刚 switchDevice / 解绑之后
  // 那次重拉失败 —— 那几处都会 `setInstances([])`）⇒ `instance` 为 undefined ⇒ 设备卡整块不渲染。
  // 而「切换 / 管理 / 添加电脑或重新配对 / 解绑」**全挂在设备卡上**（`openDevicesSheet` 的唯一
  // 调用点就是卡上那颗按钮），配对页又只认"完全没有令牌" ⇒ 整页只剩一个「刷新」（实测
  // `clickable: ["刷新"]`）。读数读不到，不该把入口也一起收走。
  assert.match(page, /mobileView === "projects" && \(instance \|\| token\.trim\(\)\) && <section className="mobile-device-card"/);
  assert.match(page, /data-unresolved=\{instance \? undefined : "true"\}/);
  // 三种真相分开说（同"空列表有三种真相"那条规则）：读到并在线 / 读到但离线 / 这次根本没读回来。
  // 第三种**不能**写成"离线" —— 那是把"不知道"栽赃成"知道"。
  assert.match(page, /这台电脑的信息没读回来/);
  assert.match(page, /点右侧可以换一台、重新配对，或先刷新/);
  // 入口按钮与读数解耦：instance 缺失时写「管理」，而不是让整颗按钮跟着读数一起消失。
  assert.match(page, /\{instance \? devicePanelLabel : "管理"\}/);
  // 设备面板本身只吃本地设备表（`readDevices()`），不需要云端读数 —— 这是"入口能兜底"的前提。
  assert.match(page, /function openDevicesSheet\(\) \{\s*setDevices\(readDevices\(\)\);/);

  // 原生包里没有通知通道：`capacitor.plugins.json` 只有扫码与 App 两个插件、`AndroidManifest.xml`
  // 也没有 `POST_NOTIFICATIONS`，而 Android WebView 不会把 Web Notification 接到系统通知栏。
  // 所以那颗「开启通知」在原生平台必须收起 —— 留着它就是一颗"点了永远拿不到权限"的哑按钮。
  assert.match(page, /\(\) => Capacitor\.isNativePlatform\(\) \|\| typeof Notification === "undefined" \? "unsupported" : Notification\.permission\)/);
  // 两个入口（顶栏那颗 + ⋯ 菜单里那项）都只认 "default"，所以判 unsupported 就能一起收起来。
  assert.ok((page.match(/notificationPermission === "default"/g) || []).length >= 2,
    "顶栏与 ⋯ 菜单里的通知入口都只认 default（判 unsupported 才能把两处一起收起）");
});

test("mobile one-phone-many-desktops: device bar, switcher sheet and per-device state", () => {
  // 方案 A：项目页顶部一条「当前电脑」（与实例状态合并成一条）+ 点「切换」开底部弹层。
  // ① 只有 ≥2 台可用设备时才有那颗「切换」按钮；卡片本身单台时也在（它就是原来的实例状态卡）。
  // 入口按钮**始终在**（单台也要能进面板去添加/重新配对/解绑），文案跟着面板的主功能变：
  // ≥2 台是「切换」，只有一台是「管理」。
  assert.match(page, /const devicePanelLabel = usableDevices\.length >= 2 \? "切换" : "管理";/);
  assert.match(page, /<button className="mobile-device-switch" type="button" ref=\{deviceSwitchButtonRef\} onClick=\{openDevicesSheet\} aria-haspopup="dialog">\{instance \? devicePanelLabel : "管理"\}<svg/);
  // ② 卡片结构（2026-09-17 定稿）：[图标块 + 右下角状态灯] [电脑名 +「在线」] [「切换」]。
  //    图标块**复用项目卡那一套**（.mobile-project-mark）—— 两处各写一遍 CSS 迟早走偏。
  assert.match(page, /\{mobileApp && mobileView === "projects" && \(instance \|\| token\.trim\(\)\) && <section className="mobile-device-card"/);
  assert.match(page, /<span className="mobile-project-mark mobile-device-mark" aria-hidden="true">/);
  // 状态灯的 class 必须**跟状态走**：离线时还亮着绿灯等于骗人。
  assert.match(page, /<i className=\{`mobile-device-led\$\{instance && instance\.status === "online" \? "" : " is-away"\}`\} \/>/);
  // ③「在线」胶囊跟在电脑名**后面**（同一个 <b> 内、名字之后），不再占右列。
  //    名字用 `activeDeviceName`（＝备注 > 云端真名，在本文件上半段合成一次）——
  //    ⚠️ 这里**不能**退回 `instance.name`：卡片读的是云端读数、备注在本地设备表里，
  //    分头拼必然漏一处，症状是"面板里改了备注、卡片上还是老名字"。
  assert.match(page, /<b>\{activeDeviceName\}<span className=\{`mobile-device-pill \$\{instance\.status\}`\}>\{instanceStatusLabel\(instance\.status\)\}<\/span><\/b>/);
  assert.doesNotMatch(page, /mobile-device-actions/);
  // ④ 副标题＝"什么时候同步的 + 几个项目"；事件序号是排障用的内部量，从这张卡上撤掉。
  assert.match(page, /<small>\{deviceSyncText\(instance\.lastSeenAt\)\}<\/small>/);
  assert.match(page, /function deviceSyncText\(lastSeenAt: string \| undefined\): string \{/);
  // 项目数**只出现一次**：在「选择项目」标题右侧。
  // ⚠️ 电脑端那一屏（`!mobileApp`）在 2026-09-17 改成方案 B 的两栏工作台之后**不再有**
  // 「N 项目 / N 任务」统计卡：它取的是云端快照，而电脑端没有云端令牌 ⇒ 数字恒为 0/0，
  // 下面还会跟着一句永远转不完的「加载中」。这是本轮审计的 A1/A2。
  assert.match(page, /<h2>选择项目<\/h2><span>\{snapshot \? `\$\{projects\.length\} 个项目` : "加载中"\}<\/span>/);
  assert.doesNotMatch(page, /className="mobile-summary"/);
  // 还没拿到快照时必须写"加载中" —— 不能把"还没回来"写成"0 个项目"。
  assert.match(page, /snapshot \? `\$\{projects\.length\} 个项目` : "加载中"/);
  // 按区块切片断言"手机端卡片里没有事件序号"，而不是全文 doesNotMatch ——
  // 桌面端那张卡**仍然**保留它（信息密度需求不同）。
  const cardAt = page.indexOf('className="mobile-device-card"');
  const cardSlice = page.slice(cardAt, page.indexOf("</section>", cardAt));
  assert.ok(cardAt > 0 && cardSlice.length > 0, "没切到「当前电脑」卡片");
  assert.doesNotMatch(cardSlice, /事件序号/);
  // 副标题上的时间：**今天只写时:分**（同一天的日期只是白占宽度，昨天以前才写月/日）。
  assert.match(page, /function syncClockText\(value: string \| undefined\): string \{/);
  assert.match(page, /return sameDay \? clock : `\$\{at\.getMonth\(\) \+ 1\}\/\$\{at\.getDate\(\)\} \$\{clock\}`;/);
  // 电脑端**不再有**独立的实例状态卡（`.mobile-instance-status`）。它当年的渲染条件是
  // `!mobileApp && instance`，而 `instance` 只从需要云端令牌的 /v1/instances 来 ——
  // 电脑端没有令牌 ⇒ 那张卡在真机上**从未出现过**，留着只是一段读不到的死代码。
  // 现在"这台电脑在云端的身份/活跃度"改由 desktop-remote 那一套直接读本机
  // /api/remote/agent-status（见本文件末尾"desktop remote"那组用例）。
  assert.doesNotMatch(page, /className="mobile-instance-status"/);
  assert.match(page, /<h2 id="desktop-remote-service-title">远程服务<\/h2>/);
  // ② 顶栏不动：当前电脑只是⋯菜单里的一行非交互信息（和"执行 Agent"同一套样式）。
  assert.match(page, /className="mobile-header-menu-info mobile-header-menu-device"><span>当前电脑<\/span>/);
  assert.doesNotMatch(page, /<h1>\{[^}]*activeDeviceRecord/);
  // ③ 切换必须复位上一台的现场：instanceID 置空是有意的（拉失败也不能挂旧数据）。
  assert.match(page, /function switchDevice\(nextToken: string\) \{/);
  assert.match(page, /setActiveToken\(nextToken\);\s*setDevices\(readDevices\(\)\);\s*setToken\(nextToken\);\s*setInstances\(\[\]\);\s*setInstanceID\(""\);\s*setSnapshot\(null\);/);
  // 切换面板：当前那台带勾、失效那台不可点且标签写"需重新配对"。
  // 行是**容器**（里面要嵌「解绑」这第二颗按钮，嵌套按钮非法）：
  // 行主体＝切到这台，右侧＝解绑。
  assert.match(page, /className="mobile-device-row" key=\{item\.token\} data-current=\{item\.token === token \? "true" : "false"\} data-revoked=\{item\.revoked \? "true" : "false"\}/);
  assert.match(page, /<button className="mobile-device-row-main" type="button" onClick=\{\(\) => switchDevice\(item\.token\)\} disabled=\{item\.revoked\}/);
  // 当前给 ✓、其余未失效的给 ›；失效那行两个都不给（它点不动）。
  assert.match(page, /item\.token === token\s*\? <span className="mobile-device-tick" aria-hidden="true">✓<\/span>\s*: !item\.revoked && <span className="mobile-device-chevron" aria-hidden="true">›<\/span>/);
  assert.match(page, /item\.revoked \? "需重新配对" : item\.status === "online" \? "在线" : "离线"/);
  assert.match(page, /onClick=\{\(\) => \{ setDevicesOpen\(false\); openPairingPanel\(\); \}\}>＋ 添加电脑或重新配对</);
  // 面板标题与那句说明（把"换电脑 / 改名字 / 重新配对"三重含义说清楚）。
  assert.match(page, /id="mobile-device-sheet-title">我的电脑</);
  assert.match(page, /className="mobile-device-sheet-note">一台手机可以连多台电脑；每台电脑同一时间只服务一台手机。换电脑、改名字或重新配对都从这里开始。</);
  // 浮层清退两处（返回键 / 侧滑返回）都要收掉切换面板。
  assert.match(page, /if \(devicesOpen\) \{ setDevicesOpen\(false\); return true; \}/);
  const leaveAt = page.indexOf("function leaveConversationView() {");
  assert.match(page.slice(leaveAt, page.indexOf("\n  }", leaveAt)), /setDevicesOpen\(false\);/);
  // 逐台探测：别的电脑要用**它自己的**令牌去问，不能借用当前那台。
  assert.match(page, /await cloud<Instance\[\]>\("\/v1\/instances", undefined, item\.token\)/);
  // 非当前设备 30s 一次、当前设备 5s 一次；依赖写 devices.length 而不是 devices（否则定时器自我重启）。
  assert.match(page, /\}, 30_000\);/);
  assert.match(page, /\}, \[devices\.length, documentVisible, probeOtherDevices\]\);/);
  // 「我的电脑」总账：每台一行、各自解绑。
  // 「我的电脑」总账已从配对面板里删除：它和底部面板是同一份数据、各带一半能力
  // （总账能解绑不能切、面板能切不能解绑），现在合并成"点设备进面板"这一层。
  assert.doesNotMatch(page, /className="mobile-pairing-devices"/);
  assert.doesNotMatch(page, /className="mobile-device-manage-row"/);
  // 失效标记：三种真相（离线 / 绑定已失效 / 状态未知）不能混成一句"没有数据"。
  assert.match(page, /if \(device\.revoked\) return "绑定已失效，需要重新扫码配对";/);
});

test("mobile device alias: 每台电脑一个只在这台手机上生效的备注名", () => {
  // 需求（2026-09-21）：手机能连多台电脑之后，两台都叫 "DESKTOP-8F2K" / "LAPTOP-3C91" 分不清，
  // 要给每台起一个自己的名字。设计把三件事钉死：**纯本地**、**只覆盖显示**、**可清除**。
  //
  // ① 显示名的唯一判据是 `deviceDisplayName`（备注 > 真名 > "未命名电脑"），
  //    不许在各渲染点各写一遍 `alias || name || ...`：那样必然漏一处，症状是
  //    "面板里改了、卡片上还是老名字"。这四条断言就是数它有没有被绕过。
  assert.match(page, /import \{[^}]*deviceDisplayName[^}]*\} from "\.\.\/lib\/mobile-devices";/);
  assert.ok((page.match(/deviceDisplayName\(/g) || []).length >= 4,
    `四处显示点（卡片 / 面板行 / 面板行的 aria-label / 解绑确认框 / ⋯ 菜单）都要走它，实际 ${(page.match(/deviceDisplayName\(/g) || []).length} 处`);
  assert.match(page, /const activeDeviceName = instance \? deviceDisplayName\(\{ alias: activeAlias, name: instance\.name \|\| instance\.instanceId \}\) : activeAlias;/);
  assert.match(page, /<b>\{deviceDisplayName\(item\)\}<\/b>/);
  // ⋯ 菜单里那行「当前电脑」也要跟着走 —— 它是会话视图里唯一能确认"现在连的是哪台"的地方。
  assert.match(page, /<small>\{deviceDisplayName\(activeDeviceRecord\)\} · \{activeDeviceRecord\.revoked \? "绑定已失效"/);

  // ② 卡片上**必须显示备注名**，并且在云端读数没回来时也显示：备注是本地读数，
  //    不依赖 `/v1/instances`。但副标题仍要说"没读回来"—— 名字有了不等于这台电脑是好的。
  assert.match(page, /<b>\{activeDeviceName \|\| "这台电脑的信息没读回来"\}<span className="mobile-device-pill offline">未同步<\/span><\/b>/);

  // ③ 编辑入口：每行一颗「备注」，开着备注时写「改备注」；名字取自设备表（本地，不发请求）。
  assert.match(page, /const \[aliasTarget, setAliasTarget\] = useState\(""\);/);
  assert.match(page, /<button type="button" className="mobile-device-row-alias" onClick=\{\(\) => openAliasEditor\(item\.token\)\} disabled=\{busy\}/);
  assert.match(page, /aria-label=\{`给「\$\{deviceDisplayName\(item\)\}」改备注`\}>\{item\.alias \? "改备注" : "备注"\}</);
  // 草稿只在**打开弹层那一刻**灌入，不在 devices 刷新时回灌 —— 其它设备的在线状态每 30 秒
  // 探测一次并整表写回，回灌会把用户打到一半的字冲掉。
  {
    const openAt = page.indexOf("function openAliasEditor(deviceToken: string) {");
    const openSlice = page.slice(openAt, page.indexOf("\n  }", openAt));
    assert.ok(openAt > 0 && openSlice.length > 0, "没切到 openAliasEditor");
    assert.match(openSlice, /setAliasDraft\(normalizeAlias\(record\?\.alias \|\| ""\)\);/);
    assert.match(openSlice, /setAliasTarget\(deviceToken\);/);
  }
  // 保存与清除走同一条路：`setDeviceAlias` 里把"什么算空"收成一处（全空格 = 清除 = 回落真名）。
  {
    const saveAt = page.indexOf("function saveAlias(event: FormEvent) {");
    const saveSlice = page.slice(saveAt, page.indexOf("\n  }", saveAt));
    assert.ok(saveAt > 0 && saveSlice.length > 0, "没切到 saveAlias");
    assert.match(saveSlice, /setDeviceAlias\(aliasTarget, aliasDraft\);/);
    assert.match(saveSlice, /setDevices\(readDevices\(\)\);/);
    assert.match(page, /const \[aliasDraft, setAliasDraft\] = useState\(""\);/);
  }
  assert.match(page, /maxLength=\{DEVICE_ALIAS_MAX_LENGTH\}/);
  // 「清除备注」只在**已经起过备注**时渲染（否则一进来就摆着一颗看起来必须点的按钮）。
  assert.match(page, /<footer>\{aliasTargetRecord\?\.alias \? <button type="button" className="mobile-device-alias-clear"/);
  // 设备存储里的 `alias` 是后加字段：读的时候必须走 `normalizeAlias`（老记录没有这个键 ⇒ 空串）。
  assert.match(lib, /alias: typeof value\.alias === "string" \? normalizeAlias\(value\.alias\) : "",/);
  // ⑥ 重新配对**不能丢备注**：同一台电脑换新令牌时旧记录被替换，备注必须被继承过来。
  //    这是本功能最容易漏的一条路径（"重新配对"恰恰是用户恢复连接时最常做的操作）。
  //    下面两条守接线，行为由 `lib/mobile-devices.test.ts` 里的对应用例守着。
  assert.match(lib, /const inherited = previous\.find\(\(item\) => \(input\.instanceId && item\.instanceId === input\.instanceId\) \|\| item\.token === input\.token\);/);
  assert.match(lib, /alias: inherited \? normalizeAlias\(inherited\.alias\) : "",/);
  // 归一化只此一处：折叠空白 → trim → **按码点**截断。顺序换了就会出现"回车把面板那一行撑高"、
  // 或者"全空格被存成一个看不见的名字"；截断改成 `slice` 则会切坏 emoji（单测里钉了孤立代理）。
  assert.match(lib, /export function normalizeAlias\(raw: string\): string \{\s*const collapsed = raw\.replace\(\/\\s\+\/g, " "\)\.trim\(\);\s*return Array\.from\(collapsed\)\.slice\(0, DEVICE_ALIAS_MAX_LENGTH\)\.join\(""\);\s*\}/);
  assert.doesNotMatch(lib, /trim\(\)\.slice\(0, DEVICE_ALIAS_MAX_LENGTH\)/);

  // ④ 浮层清退两处（返回键链 / 侧滑返回）。弹层是**面板之上**的一层，必须排在 `devicesOpen` 之前 ——
  //    排在后面时返回键会先收掉整个面板，弹层还浮在项目列表上（与 confirmUnbind 同一个形状）。
  const backAt = page.indexOf("if (aliasTarget) { closeAliasEditor(); return true; }");
  const openAt = page.indexOf("if (devicesOpen) { setDevicesOpen(false); return true; }");
  assert.ok(backAt > 0 && openAt > 0 && backAt < openAt,
    "备注弹层的返回键分支必须排在设备面板之前（否则面板先被收掉、弹层还浮着）");
  const leaveAt2 = page.indexOf("function leaveConversationView() {");
  assert.match(page.slice(leaveAt2, page.indexOf("\n  }", leaveAt2)), /closeAliasEditor\(\);/);
  // ⚠️ 渲染条件必须**同时**带上 `devicesOpen`：备注弹层是设备面板的子层，不该比面板活得久。
  //    今天没有可达路径能让面板先关掉（弹层背板盖满整屏、面板上的按钮点不到），但"靠今天
  //    恰好没有那条路"不值得赌 —— 少这一个条件，将来任何新增的 `setDevicesOpen(false)` 都会
  //    留下"面板没了、弹层还浮在项目列表上"。写成条件就不必每个关面板的地方都记得它。
  assert.match(page, /\{devicesOpen && aliasTarget && <div className="mobile-task-modal-backdrop" role="dialog" aria-modal="true" aria-labelledby="mobile-device-alias-title">/);

  // ⑤ 面板行的动作列是**竖排**：320px 屏上并排会把名字挤到约 5 个字一行，
  //    而名字正是这一行存在的理由（探针里量了实际宽度）。这条 CSS 靠视觉效果守住，
  //    代码里只留一句"为什么不能改回并排"。
  assert.match(styles, /\.mobile-device-row-actions \{ flex: none; display: flex; flex-direction: column;/);
  assert.match(styles, /\.mobile-device-row-alias \{ min-height: 36px; margin-right: 8px; border: 1px solid #b9dfcb;/);
  // 备注是主色描边（可点、无损），解绑仍是危险色 —— 两档必须分得开，
  // 否则扫一眼会把"改个名字"当成"解绑"。
  // ⚠️ `align-self` 必须是 `stretch`：这两颗现在住在**竖排**动作列里，而 `align-self` 作用在
  // **交叉轴**（列方向＝水平）—— 写 `center` 等于把较窄的「解绑」水平居中，两颗右边缘就错开
  // 几个像素，而这恰是"起了备注"时最常见的状态（「改备注」3 字 vs「解绑」2 字）。
  // 探针里量了两颗按钮的矩形（右边缘必须相等）；这里守的是别把它改回去。
  assert.match(styles, /\.mobile-device-row-unbind \{ flex: none; align-self: stretch; margin-right: 8px; min-height: 36px; border: 1px solid #e3b3a9;/);
  assert.doesNotMatch(styles, /\.mobile-device-row-unbind \{ flex: none; align-self: center;/);
  // 「原名」那行要压过 `.mobile-device-row-who small`(0,1,1)，所以选择器必须写到 (0,3,0)；
  // 只写类名会变成"靠书写顺序生效"（本文件已吃过一次这种亏）。
  assert.match(styles, /\.mobile-device-row \.mobile-device-row-who \.mobile-device-row-origin \{ overflow: hidden; color: #9aafa6;/);
});

test("mobile device storage keeps the legacy key alive for older builds", () => {
  // 旧包只认 milevia.cloud.token：新包必须继续镜像它，否则用户回退版本就会发现"配对没了"。
  assert.match(page, /const \[devices, setDevices\] = useState<MobileDevice\[\]>\(\(\) => reconcileDevices\(\)\);/);
  assert.match(page, /const \[token, setToken\] = useState\(\(\) => readActiveToken\(\)\);/);
  assert.match(page, /addOrReplaceDevice\(\{ token: pendingAccessToken, instanceId: instanceID, name: instance\?\.name \|\| "" \}\);/);
  // 手动粘贴令牌的入口（`saveToken`）在 2026-09-20 的可用性排查里确认是死代码（全仓无调用点），
  // 已删除 —— 所以这里不再断言它的函数体，改为断言"确实删了 + 留下了恢复线索"。
  assert.doesNotMatch(page, /function saveToken\(/);
  assert.match(page, /这里原来有一个 `saveToken\(event\)`/);
  // 401 只标记那一台（记录留着当现场），并且只有"当前这台"被拒才把整页拉回配对流程。
  assert.match(page, /markDeviceRevoked\(token\);/);
  assert.match(page, /if \(token === readActiveToken\(\)\) globalThis\.dispatchEvent\(new Event\("milevia:token-cleared"\)\);/);
  // 上报设备名：电脑端"当前绑定的手机"那一行就是它。
  assert.match(page, /deviceName: deviceLabel\(\)/);
});

test("instance status renders Chinese text through one shared label map", () => {
  // 云端给的是 online / offline，直接渲染就是一颗写着 "online" 的绿胶囊。
  assert.match(page, /const instanceStatusLabels: Record<string, string> = \{ online: "在线", offline: "离线", machine_offline: "电脑未响应" \};/);
  // 唯一的渲染点＝手机端「当前电脑」卡上的那颗胶囊。文案与类名都来自同一份映射，
  // 所以 computer 端 2026-09-17 撤掉自己那张卡之后，这里只剩一处 —— 别再补第二处。
  assert.match(page, /<span className={`mobile-device-pill \$\{instance\.status\}`}>\{instanceStatusLabel\(instance\.status\)\}<\/span>/);
  assert.doesNotMatch(page, /className="mobile-status /);
});

test("mobile conversations render markdown and project cards expose keyboard activation", () => {
  assert.match(page, /<ReactMarkdown remarkPlugins=\{\[remarkGfm\]\}/);
  assert.match(page, /className="mobile-message-markdown markdown"/);
  // 手机端的项目卡本身就是 <button>，键盘激活由浏览器给 —— 不写 onKeyDown 是**有意的**。
  // 电脑端那张 `<article role="button" tabIndex={0}>` 项目卡在 2026-09-17 随方案 B 撤掉了
  // （它列的是云端快照，电脑端拿不到）。这条断言一起删：留着一个永远匹配不上的锚点，
  // 下次有人把锚点改回去也看不出它已经失效。
  assert.doesNotMatch(page, /mobile-project \$\{project\?\.id/);
});

test("mobile message columns cannot be widened by wide message content", () => {
  // 症状：手机上"消息只能看到左边一半"，而且没法横向滑动看到右边。
  // 根因：.mobile-message-list 是 display: grid 且列宽为默认的 auto，auto 轨道的最小尺寸
  // 取的是最宽那条消息的 min-content——长代码行（pre 不换行）、宽表格（width: max-content）、
  // 1600px 截图（img 没有 max-width）都能把这一列顶到 1500px 以上；.mobile-remote 又是
  // overflow-x: hidden，超出的部分被直接裁掉，于是整屏消息一起被截断。
  // 修法：轨道用 minmax(0, 1fr) 钉死，气泡再补 min-width: 0，宽内容改为在气泡内部横滚。
  assert.match(styles, /\.mobile-message-list \{[^}]*grid-template-columns: minmax\(0, 1fr\)/s);
  assert.doesNotMatch(styles, /\.mobile-message-list \{[^}]*grid-template-columns: auto/s);
  assert.match(styles, /\.mobile-message \{[^}]*min-width: 0;/s);
  // 宽内容改为在气泡内部横向滚动（图片的宽度约束是跨入口的通用规则，在 markdown.css
  // 的 `.markdown img` 里，由 markdown-rendering.test.mjs 守着）。
  assert.match(styles, /\.mobile-message-markdown pre \{[^}]*overflow-x: auto;/s);
  assert.match(styles, /\.mobile-message-markdown table \{[^}]*overscroll-behavior-x: contain;/s);
});

test("mobile task panel is a full-page dialog whose status filters fit on one line", () => {
  // 症状（用户报的）：① 这一层叫"任务与操作"，点开却是贴右侧的抽屉——手机上只有
  //   min(390px, 100vw-22px) 宽；② 分类栏横排一行放不下，只能换行成三行、或者横向滑动才看全。
  // 修法：整页弹层 + 分类栏 5 等分一行（2026-09-16 又去掉了「全部」与「已取消」两颗，
  // 见下面那组"分类栏本身"的断言；原来还有"卡片折叠状态下大片空白"、"删除"孤零零
  // 挂在"查看详情"旁边两个问题，下面各自的断言继续守着）。
  assert.match(page, /<h3 id="mobile-task-panel-title">任务队列<\/h3>/);
  assert.doesNotMatch(page, /任务与操作/);
  // 抽屉那一套必须彻底消失（类名、右侧定位、点击遮罩），别留半套。
  assert.doesNotMatch(page, /mobile-task-drawer/);
  assert.doesNotMatch(styles, /\.mobile-task-drawer/);
  // 面板占满视口，头栏与分类栏是固定项，只有正文自己滚——"打开就能看全分类"靠的就是这个。
  assert.match(styles, /\.mobile-task-panel \{ position: fixed; z-index: 20; inset: 0; display: flex; flex-direction: column;/);
  // 整页模态层的焦点语义：打开时焦点搬进面板、背后那屏标成 inert，关闭时还给触发按钮。
  // 缺了这步，键盘 / 读屏用户打开面板后仍在背后那屏里打转，而视觉上已经"进到面板里了"。
  assert.match(page, /<section className="mobile-task-panel" ref=\{taskPanelRef\} tabIndex=\{-1\} role="dialog" aria-modal="true"/);
  // 触发它的按钮是顶栏 ⋯ 菜单里的「任务队列」，点它的同一次渲染里菜单就收起了 ——
  // 此时 activeElement 已经变成 document.body，而 body 不是有意义的落点，必须换成 ⋯ 按钮。
  assert.match(page, /const active = document\.activeElement;\n    const restoreTo = active instanceof HTMLElement && active !== document\.body \? active : headerMenuButtonRef\.current;/);
  assert.match(page, /element\.inert = true;/);
  assert.match(page, /for \(const element of blocked\) element\.inert = false;/);
  assert.match(page, /panel\.focus\(\{ preventScroll: true \}\)/);
  // 这个 effect 的依赖必须带上"面板会不会被渲染"的条件（project / mobileView / mobileApp）：
  // 面板的渲染条件是 `mobileApp && mobileView === "conversation" && project && tasksOpen` 的乘积，
  // 只挂 tasksOpen 的话，将来一旦有路径让面板先离场而 tasksOpen 没变，cleanup 就不执行、
  // 背景的 inert 会残留（整页看得见、点不动）——本轮的实测脚本验证过依赖齐全时不会残留。
  assert.match(page, /\}, \[tasksOpen, mobileApp, mobileView, project\?\.id\]\);/);
  // 「我的电脑」面板也要搬焦点 / 背景 inert / 关闭还焦点。依赖必须带 mobileApp / mobileView：
  // 只挂 devicesOpen 时，切视图会让面板先离场而 cleanup 不跑，背景永久 inert（整页点不动）。
  assert.match(page, /const devicesSheetRef = useRef<HTMLElement \| null>\(null\);/);
  assert.match(page, /const deviceSwitchButtonRef = useRef<HTMLButtonElement \| null>\(null\);/);
  assert.match(page, /ref=\{devicesSheetRef\} tabIndex=\{-1\}/);
  assert.match(page, /ref=\{deviceSwitchButtonRef\} onClick=\{openDevicesSheet\}/);
  assert.match(page, /\}, \[devicesOpen, mobileApp, mobileView\]\);/);
  assert.match(styles, /\.mobile-device-sheet:focus \{ outline: none; \}/);
  assert.match(styles, /\.mobile-task-panel:focus \{ outline: none; \}/);
  // 面板的 padding 必须分四条写：不支持 env() 的环境（老 WebView / 部分桌面浏览器）会把含 env()
  // 的**整条声明**丢掉——写成一条就等于完全没有 padding、内容贴死屏幕四边（docs/33 记过这个坑，
  // 页面其它 7 处 env() 也都是这个写法）。
  assert.match(styles, /\.mobile-task-panel \{[^}]*padding: 12px 14px; padding-top: max\(12px, env\(safe-area-inset-top\)\);/s);
  assert.match(styles, /\.mobile-task-panel-body \{[^}]*flex: 1;[^}]*min-height: 0;[^}]*overflow-y: auto;/s);
  // 5 个分类等分一行：等宽 grid（宽度瓶颈是 3 个中文字，不许换行、也不许横向滚动找）。
  assert.match(styles, /\.mobile-task-filters \{[^}]*grid-template-columns: repeat\(5, minmax\(0, 1fr\)\);/s);
  assert.doesNotMatch(styles, /\.mobile-task-filters \{ display: flex;/);
  assert.doesNotMatch(styles, /\.mobile-task-filters \{[^}]*flex-wrap: wrap;/s);
  assert.doesNotMatch(styles, /\.mobile-task-filters \{[^}]*overflow-x: auto/s);
  // 标签与计数上下两行堆叠，宽度才只由标签决定（横排时"标签 + 计数"更宽，窄屏每格放不下）。
  // 54px 是触控尺寸（整格可点），不是随手写的高度。
  assert.match(styles, /\.mobile-task-filters button \{ display: grid; min-width: 0; min-height: 54px; place-content: center;/);
  // 320px 屏每格 54px，13px 的三字标签（39px）会顶到格边，必须降一档字号。
  assert.match(styles, /@media \(max-width: 359px\) \{\s*\.mobile-task-filters button \{ font-size: 12px; \}/);
  // 任务条目＝浮起卡片（2026-09-16 定案，替换掉"描边卡片 + 查看详情折叠行"那一版）：
  // 去掉 1px 描边改两层柔投影，标题 16px 允许 2 行，描述给 1 行预览，底部一行放操作。
  assert.match(styles, /\.mobile-task-panel \.mobile-task \{[^}]*border: 0;[^}]*border-radius: 16px;[^}]*background: #fff;[^}]*box-shadow:/s);
  assert.match(styles, /\.mobile-task-panel \.mobile-task-title \{[^}]*font-size: 16px;[^}]*line-clamp: 2;/s);
  assert.match(styles, /\.mobile-task-panel \.mobile-task-desc \{[^}]*line-clamp: 1;/s);
  // 时间靠右 + 等宽数字：一列卡片的时间因此竖直成列，扫一眼就知道哪条多久没动了。
  assert.match(styles, /\.mobile-task-panel \.mobile-task-time \{ margin-left: auto;[^}]*tabular-nums;/s);
  // 行内按钮视觉 36px，热区靠 ::after 外扩 4px 补到 44px（与顶栏按钮同一套做法；间距 16px
  // 大于相邻两颗各外扩的 4px，热区不会互相压住）。**按钮自带 display 会压掉 UA 的
  // [hidden] { display: none }**，所以必须显式兜一条，否则"不能编辑"的任务上会留一颗点不动的「编辑」。
  assert.match(styles, /\.mobile-task-panel \.mobile-task-foot button \{[^}]*min-height: 36px;/s);
  assert.match(styles, /\.mobile-task-panel \.mobile-task-foot button::after \{ content: ""; position: absolute; inset: -4px; \}/);
  assert.match(styles, /\.mobile-task-panel \.mobile-task-foot button\[hidden\] \{ display: none; \}/);
  // ⚠️ 主操作按钮的规则必须带足前缀：通用规则是 `.mobile-task-panel .mobile-task-foot button`
  // (0,2,1)，写成单个类 `.mobile-task-primary` (0,1,0) 会被它整条压掉 —— 实测症状是按钮
  // **渲染了但看不见**（background 被重置成 none、border 透明，而字色是 #fff → 白字白底）。
  assert.match(styles, /\.mobile-task-panel \.mobile-task-foot \.mobile-task-primary \{[^}]*background: #2c7567;[^}]*color: #fff;/s);
  // 「需处理」过去**没有**配色规则，落到默认灰绿底、与「待处理」同色（分类栏数得出 1 条、
  // 列表里认不出是哪条）。补的是桌面端 tasks.css 的同一格琥珀，不新造色相。
  assert.match(styles, /\.mobile-task-status\.status-action_required \{ color: #926125; background: #fff8df; \}/);
  // 空列表分**三种**够得到的真相（搜索没命中 / 分类筛空 / 队列本来就空），**只给一行标题**：
  // 用户明确要求去掉下面那行说明。**不写"正在同步/同步失败"两态**：面板能渲染就
  // 说明 project 已从 snapshot 取到，那两态在面板里永远到不了（写出来是死代码）。
  assert.match(page, /const taskPanelEmpty = useMemo\(/);
  assert.match(page, /data-state=\{taskPanelEmpty\.state\}/);
  assert.match(page, /title: `「\$\{label\}」下没有任务`/);
  // ⚠️ 判据必须是**队列总条数**，不能是"当前分类是不是全部"：`"全部"` 这一档 2026-09-16 没了，
  // 那个判据跟着一起失效（而且队列整体为空时按分类点名会被读成"分类的问题"）。
  assert.match(page, /\}, \[listedMatchCount, project\?\.tasks\.length, taskFilter, taskQuery, visibleTasks\.length\]\);/);
  assert.match(page, /if \(\(project\?\.tasks\.length \?\? 0\) === 0\) return \{ state: "empty" as const, title: "队列里还没有任务" \};/);
  assert.doesNotMatch(page, /taskFilter !== "all"/);
  assert.doesNotMatch(page, /taskPanelEmpty\.hint/);
  assert.doesNotMatch(styles, /\.mobile-task-empty small/);
  assert.match(styles, /\.mobile-task-panel \.mobile-task-empty \{/);
  // ⚠️ 这两条是"搜索词原样回显"的承重闸门（2026-09-18 复查实测出来的）：
  // `nomatch` 空态把搜索词写进标题，而一段**没有空格的长串**（78 字符英文 / 粘贴进来的 URL）
  // 在隐式 `auto` 轨道里的 min-content 就是整串 —— 实测卡片自身 362px、内容撑到 736px（320 屏 444px），
  // 被 `.mobile-remote` 的 `overflow-x: hidden` **静默裁掉**：页面不横向滚，用户只是读到半句话。
  // 与 `.mobile-message-list` / `.mobile-task-list` 是同一个隐式 `auto` 轨道的坑，两条缺一不可：
  // 只加 `overflow-wrap` 时列宽约束没有来源，轨道的 min-content 早已把卡片顶宽。
  assert.match(styles, /\.mobile-task-panel \.mobile-task-empty \{ display: grid; grid-template-columns: minmax\(0, 1fr\); justify-items: center;/);
  assert.match(styles, /\.mobile-task-panel \.mobile-task-empty strong \{ overflow-wrap: anywhere;/);
  assert.doesNotMatch(page, /mobile-task-empty-retry/);
  // ── 面板内的搜索框（2026-09-18）────────────────────────────────────────────
  // 语义是"**在当前分类里**再收一次"，所以三件事必须同时成立：
  //   ① 搜索框渲染在分类栏**上面**（源码顺序：`mobile-task-search` 出现在 `mobile-task-filters` 之前）；
  //   ② 匹配范围与电脑端 `TaskQueue` 一致（标题 + 描述、忽略大小写）；
  //   ③ 搜索先收窄、再过分类 —— 胶囊计数与列表都从 `matchedTasks` 出发。⚠️ 计数若仍按全量算，
  //      就会出现"胶囊写着 6、点进去是空的"，那正是当初去掉「全部」要避免的"列表与分类对不上"。
  //   ⚠️ 有搜索词时**不能说「「待处理」下没有任务」**（队列里可能就有，只是不匹配这个词），
  //      也不能一律说「没有匹配的任务」（别的分类下命中时那句话是假的）—— 两句分开，判据是命中总数。
  assert.match(page, /const \[taskQuery, setTaskQuery\] = useState\(""\);/);
  assert.match(page, /const matchedTasks = useMemo\(\(\) => \{/);
  // ⚠️ `title` 必须兜 null：它是线协议字段（`type Task` 里的 `title: string` 只是假设），
  // 而这一行在**每次按键**上跑遍队列里的每一条任务 —— 一个 null 就在 `useMemo` 里抛错、整页白屏。
  // 2026-09-18 复查用 `title: null` 的夹具实测复现（`Cannot read properties of null (reading 'toLowerCase')`），
  // 所以断言按"带兜底"的写法钉死；配对的是 `taskSummary` 那条（同一天实测它先炸，且**不碰搜索框**也会炸）。
  assert.match(page, /return tasks\.filter\(\(task\) => \(task\.title \|\| ""\)\.toLowerCase\(\)\.includes\(term\) \|\| \(task\.description \|\| ""\)\.toLowerCase\(\)\.includes\(term\)\);/);
  assert.match(page, /const title = \(task\.title \|\| ""\)\.trim\(\);/);
  // 全文兜底：这个字段**不许**在别处被直取（改标题渲染时最容易顺手写回去）。
  // ⚠️ 必须**先剥行注释再断言**：本页有大量解释性注释会原样引用危险写法，
  // 直接对 `page` 做 doesNotMatch 会被注释满足 —— 2026-09-18 我连着踩了两次
  // （第一次是注释里写了 `task.title.trim()`，第二次是"别写它"的那句说明里又写了一遍）。
  assert.doesNotMatch(page.replace(/^\s*\/\/.*$/gm, ""), /task\.title\.trim\(\)/);
  assert.match(page, /return matchedTasks\.filter\(\(task\) => task\.status === taskFilter\);/);
  assert.match(page, /const taskCounts = useMemo\(\(\) => Object\.fromEntries\(taskFilters\.map\(\(filter\) => \[filter\.id, matchedTasks\.filter\(\(task\) => task\.status === filter\.id\)\.length\]\)\)/);
  assert.match(page, /const count = taskCounts\[filter\.id\];/);
  assert.doesNotMatch(page, /const count = project\.tasks\.filter\(/);
  assert.match(page, /title: `「\$\{label\}」下没有匹配「\$\{term\}」的任务`/);
  assert.match(page, /title: `没有匹配「\$\{term\}」的任务`/);
  assert.match(page, /const term = taskQuery\.trim\(\);/);
  // ⚠️ 判据必须是 `listedMatchCount`（**列得出来**的命中数），不能是 `matchedTasks.length`：
  // 后者的口径更宽，把 `cancelled / queued` 这类没有任何分类覆盖的任务也算成"别处有命中"，
  // 而那些任务恰恰是点遍 5 档也找不到的 —— 那时说"别的分类里有"就是骗人。
  assert.match(page, /const listedMatchCount = useMemo\(\(\) => taskFilters\.reduce\(\(sum, filter\) => sum \+ taskCounts\[filter\.id\], 0\), \[taskCounts\]\);/);
  assert.match(page, /return listedMatchCount > 0\n\s*\? \{ state: "nomatch" as const, title: `「\$\{label\}」下没有匹配「\$\{term\}」的任务` \}/);
  assert.doesNotMatch(page, /matchedTasks\.length > 0\n/);
  const searchAt = page.indexOf('className="mobile-task-search"');
  const filtersAt = page.indexOf('className="mobile-task-filters"');
  assert.ok(searchAt > 0 && filtersAt > 0, "没找到搜索框或分类栏");
  assert.ok(searchAt < filtersAt, "搜索框必须渲染在分类栏**上面**");
  // 关闭面板要把搜索词一起清掉（否则下次打开时上一轮的词会**无声地**继续过滤列表）——
  // 与 `expandedTask` 同一条 effect，断言写在下面「详情展开」那一节，这里不重复第二遍。
  // 原生 `type=search` 的放大镜与清除键由系统绘制，本页样式管不到（与原生 select 同一类问题）→
  // 两个装饰自绘，并显式关掉原生伪元素；清除键只在有词时渲染，省掉 `[hidden]` 与 display 的纠缠。
  assert.match(page, /input type="search" value=\{taskQuery\}/);
  assert.match(page, /className="mobile-task-search-clear"/);
  assert.match(page, /\{taskQuery !== "" && <button type="button" className="mobile-task-search-clear"/);
  assert.match(page, /onMouseDown=\{\(event\) => event\.preventDefault\(\)\}/);
  assert.match(styles, /\.mobile-task-panel \.mobile-task-search \{ display: flex; flex: none; align-items: center; gap: 8px; box-sizing: border-box; min-height: 44px;/);
  assert.match(styles, /\.mobile-task-panel \.mobile-task-search input \{ width: 100%; min-width: 0; flex: 1; border: 0;/);
  assert.match(styles, /\.mobile-task-panel \.mobile-task-search input::placeholder \{ color: #587568; opacity: 1; \}/);
  // 焦点指示由盒子承担（与 `.mobile-composer-box` 同一套）：这条必须写成 (0,2,1)，
  // 才压得过 `.mobile-remote input:focus-visible` 那条 (0,2,0) 的琥珀色 outline。
  assert.match(styles, /\.mobile-task-panel \.mobile-task-search input:focus-visible \{ outline: none; \}/);
  assert.match(styles, /\.mobile-task-panel \.mobile-task-search:focus-within \{ border-color: #2c7567; \}/);
  assert.match(styles, /\.mobile-task-panel \.mobile-task-search input::-webkit-search-decoration,\n\.mobile-task-panel \.mobile-task-search input::-webkit-search-cancel-button \{ display: none; -webkit-appearance: none; \}/);
  assert.match(styles, /\.mobile-task-panel \.mobile-task-search-clear \{ position: relative; display: grid; width: 32px; height: 32px; flex: none; place-items: center; border: 0; border-radius: 50%;/);
  assert.match(styles, /\.mobile-task-panel \.mobile-task-search-clear::after \{ content: ""; position: absolute; inset: -6px; \}/);
  // 分类栏本身（2026-09-16 按用户要求改版）：
  //   · 去掉「全部」与「已取消」两颗 → 5 档，且**不能用 "all" 兜底**（那样列表和分类会对不上）；
  //   · 没有「全部」之后默认必须落在某一档上（否则"没有一颗高亮"与"列表是全量"互相矛盾）；
  //   · 计数为 0 的那档退到淡底（退让只许改底与描边，**不许降文字对比度**——4.5 那道闸过不去）；
  //   · 「已取消」不再有入口，必须由栏下那行如实报数，否则任务静默消失（完备性规则）。
  const filtersSlice = page.slice(page.indexOf("const taskFilters = ["), page.indexOf("type TaskFilter ="));
  assert.ok(filtersSlice.length > 0, "没切到 taskFilters");
  assert.equal((filtersSlice.match(/\{ id: "/g) || []).length, 5, "分类栏应恰好 5 档");
  for (const status of ["todo", "running", "awaiting_review", "action_required", "done"]) {
    assert.ok(filtersSlice.includes(`{ id: "${status}"`), `分类栏少了 ${status} 这一档`);
  }
  assert.doesNotMatch(filtersSlice, /id: "all"/);
  assert.doesNotMatch(filtersSlice, /id: "cancelled"/);
  assert.match(page, /const \[taskFilter, setTaskFilter\] = useState<TaskFilter>\("todo"\);/);
  assert.doesNotMatch(page, /useState<TaskFilter>\("all"\)/);
  assert.match(page, /data-count=\{count === 0 \? "0" : "n"\}/);
  // 栏下那行"未列出"提示：只在真的存在时才渲染，且判据必须是**没有任何分类覆盖的状态**，
  // 不能只认 `cancelled` 一个字面值 —— `taskStatusLabel` 与 `.mobile-task-status` 里
  // 专门为 `queued / completed / failed / blocked` 这些"服务端可能出现的非规范状态"留了位置，
  // 它们同样不在这 5 档里；只认 cancelled 的话这些任务会既没有胶囊、也不进计数 → 彻底无声无息。
  assert.match(page, /const hiddenTaskNote = useMemo\(\(\) => \{/);
  assert.match(page, /const hidden = tasks\.filter\(\(task\) => !taskFilters\.some\(\(filter\) => filter\.id === task\.status\)\);/);
  assert.match(page, /const onlyCancelled = hidden\.every\(\(task\) => task\.status === "cancelled"\);/);
  assert.match(page, /`另有 \$\{hidden\.length\} 条\$\{onlyCancelled \? "已取消的" : "不在以上分类的"\}任务未列出`/);
  assert.match(page, /\{hiddenTaskNote && <p className="mobile-task-hidden">\{hiddenTaskNote\}<\/p>\}/);
  assert.doesNotMatch(page, /cancelledTaskCount/);
  assert.match(styles, /\.mobile-task-hidden \{ margin: 0; padding: 9px 2px 0; color: #587568; font-size: 12px; \}/);
  // 零计数退让的**顺序**：必须排在 `.active` 之后（否则"正好 0 条又被选中"的那档会被盖掉），
  // 再由 `[data-count="0"].active` 压回主色。两条都靠位置生效，所以断言按位置写。
  const zeroRuleAt = styles.indexOf('.mobile-task-filters button[data-count="0"] {');
  const activeRuleAt = styles.indexOf(".mobile-task-filters button.active {");
  const zeroActiveAt = styles.indexOf('.mobile-task-filters button[data-count="0"].active {');
  assert.ok(activeRuleAt > 0 && zeroRuleAt > activeRuleAt, "零计数退让规则必须排在 .active 之后");
  assert.ok(zeroActiveAt > zeroRuleAt, "选中的零计数分类必须有一条规则把它压回主色");
  assert.match(styles, /\.mobile-task-filters button\[data-count="0"\] \{ border-color: transparent; background: #f1f7f3; \}/);
  // ⚠️ 状态徽标的**整份调色板都要留住**，包括面板列不出来的那几格。2026-09-16 我一度把
  // `status-cancelled` 当"死样式"删掉（理由是"没有胶囊了，永远匹配不到"）—— 那是错的：
  // 这四格守的是**协议上的状态空间**（`taskStatusLabel` 里那串"非规范状态"就是证据），
  // 而 `taskStatusClass` 是通用的 `status-${status}`。删掉只会让 failed/blocked 这类**安全信号**
  // 退回默认灰绿、跟「待处理」同色。"列不出来"要靠 `.mobile-task-hidden` 报数兜，不靠删色。
  for (const [status, color, background] of [
    ["cancelled", "#708078", "#edf1ee"],
    ["completed", "#316c51", "#e3f2e7"],
    ["done", "#316c51", "#e3f2e7"],
    ["failed", "#9b3e33", "#fff0ec"],
    ["blocked", "#9b3e33", "#fff0ec"],
  ]) {
    const hit = status === "failed" || status === "blocked"
      ? styles.includes(`.mobile-task-status.status-failed, .mobile-task-status.status-blocked { color: ${color}; background: ${background}; }`)
      : status === "completed" || status === "done"
        ? styles.includes(`.mobile-task-status.status-completed, .mobile-task-status.status-done { color: ${color}; background: ${background}; }`)
        : styles.includes(`.mobile-task-status.status-${status} { color: ${color}; background: ${background}; }`);
    assert.ok(hit, `状态徽标缺了 ${status} 的配色（那是协议上的状态空间，不许当死样式删）`);
  }
  // 面板头栏也不再挂副标题（"共 9 个任务" / "待验收 0 / 6"）—— 那是分类胶囊上同一份数字的
  // 第二遍展示，而胶囊就在下一行。头栏只剩「任务队列」+ 关闭键。
  assert.doesNotMatch(page, /taskFilterSummary/);
  assert.doesNotMatch(styles, /\.mobile-task-panel-header small/);
  // 光断言"那个 memo 和那条 CSS 没了"不够：**硬编码一行文案能照样溜过去**（变异检验抓出来的口子）。
  // 按结构切出这两块，直接禁止里面出现 <small>。
  const headerSlice = page.slice(page.indexOf('className="mobile-task-panel-header"'), page.indexOf('className="mobile-task-filters"'));
  assert.ok(headerSlice.length > 0 && headerSlice.includes("mobile-task-panel-close"), "没切到面板头栏");
  assert.doesNotMatch(headerSlice, /<small/, "面板头栏不该再有副标题");
  const emptySlice = page.slice(page.indexOf('className="mobile-task-empty"'), page.indexOf("visibleTasks.map("));
  assert.ok(emptySlice.length > 0 && emptySlice.includes("taskPanelEmpty.title"), "没切到空态块");
  assert.doesNotMatch(emptySlice, /<small/, "空态只给一行标题，不该多一行说明");
  // 创建任务的**唯一**入口是面板右下角那颗加号：分类页上不再挂常驻的创建表单
  // （以前每个分类页底部都吊着两百多像素，且它是列表最后一项 —— 既要滚到底又占可见高度）。
  // 表单收进弹层，结构复用编辑 / 删除那两张（`.mobile-task-modal*`），不另造视觉。
  assert.match(page, /className="mobile-task-fab"/);
  assert.match(page, /aria-label="新建任务"/);
  assert.match(page, /id="mobile-task-create-title"/);
  assert.match(styles, /\.mobile-task-panel \.mobile-task-fab \{[^}]*position: absolute;[^}]*width: 52px;[^}]*height: 52px;[^}]*border-radius: 50%;/s);
  // 浮钮是浮层：必须给列表留落脚处，否则最后一张卡片永远被它压住右下角。
  assert.match(styles, /\.mobile-task-panel-body \{ padding-bottom: 78px; \}/);
  // 常驻创建表单在本页**一处都不剩**：面板里靠加号，电脑端那一屏（`!mobileApp`）那份
  // 也撤了 —— 它同样要云端令牌才发得出 task.create，电脑端点了必然失败（审计 A1 同源问题）。
  assert.equal(page.split('className="mobile-create"').length - 1, 0, "面板与电脑端都不该再有常驻创建表单");
  assert.equal(page.split('name="mobile-web-create-priority"').length - 1, 0, "电脑端那份创建表单已撤掉");
  // 提交按钮落在创建弹层内部（把标签绑到弹层的标题上，别写成全文件计数 —— 非 App 视图里也有一份）。
  // 窗口是"同一张弹层里的字符预算"：2026-09-17 加进优先级选择器之后从 700 涨到 759，
  // 所以放宽到 1000。往这张弹层里再加字段时记得跟着放（否则报的是"按钮丢了"这种假红）。
  // 锚点用整条按钮标签而不是裸文案：按钮文案 2026-09-20 从「创建并排队」收成「创建」，
  // 两个字太短 —— 弹层里将来只要出现任何一处中文「创建」（"创建时间"之类）就会假绿。
  assert.match(page, /id="mobile-task-create-title"[\s\S]{0,1000}<button type="submit" disabled=\{busy\}>创建<\/button>/);
  // 「新建任务」是面板**之上**的一层：返回键必须先收它（排在 tasksOpen 之前），
  // 侧滑返回那条链（leaveConversationView）也要一起收，否则弹层会浮在项目列表上。
  assert.match(page, /if \(creatingTask\) \{ setCreatingTask\(false\); return true; \}/);
  assert.match(page, /busy && \(editingTask \|\| deletingTask \|\| creatingTask \|\| newConversationProject \|\| confirmShortcut \|\| confirmUnbind\)/);
  assert.match(page, /setEditingTask\(null\);\n\s*setDeletingTask\(null\);\n\s*setCreatingTask\(false\);/);
  // ⚠️ 顺序必须单独断言：光断言"这一行在文件里存在"，把它整体挪到 tasksOpen 之后照样绿
  // —— 变异检验实测就是这个漏网（症状：返回键先收掉整页面板，弹层却还浮在会话页上）。
  // 返回键链的语义是"先收盖在面板之上的那几层，再收面板本身，最后才退视图"。
  const backStart = page.indexOf("backHandlerRef.current = () => {");
  const backSlice = page.slice(backStart, page.indexOf("return false;", backStart));
  const tasksOpenAt = backSlice.indexOf("if (tasksOpen) { setTasksOpen(false); return true; }");
  assert.ok(backStart > 0 && tasksOpenAt > 0, "没切到返回键的 if 链");
  for (const guard of ["if (headerMenuOpen)", "if (confirmShortcut)", "if (editingTask)", "if (deletingTask)", "if (creatingTask)", "if (confirmUnbind)"]) {
    const at = backSlice.indexOf(guard);
    assert.ok(at > 0 && at < tasksOpenAt, `${guard} 必须排在 tasksOpen 之前（先收面板之上的层，再收面板）`);
  }
  // 面板打开时背景会 inert，但**对话框那一层不能 inert**：它是盖在面板之上的活动层，
  // 一起冻住就是"弹窗看得见、点不动"（在新的创建弹层之前，这只是靠 effect 不重跑侥幸成立的）。
  // ⚠️ 这条防线在源码里有**三处**（任务面板 / 「我的电脑」面板 / 配对页各一份 `block()`），
  // 所以必须按**出现次数**断言：只写"这一行在不在"时，删掉其中一处照样绿 —— 2026-09-18 变异检验
  // 实测漏网（根因是变异脚本的 `replace` 只换第一处，见 TOOLING「锚点不唯一 ⇒ 假变异」）。
  // 2026-09-20：消息卡片那套从"底部浮层"改成"气泡内的图标行"之后，它不再是浮层、也不再 inert
  // 任何东西，于是这份名单从 4 回到 3 —— 计数从 4 改回 3 是同一次改动的一部分，不是"断言放松"。
  assert.equal((page.match(/if \(element\.matches\('\[role="dialog"\], \[role="alertdialog"\]'\)\) return;/g) || []).length, 3, "「对话框不能 inert」这条防线应在任务面板、「我的电脑」面板、配对页各有一处");
  // 只断言"旧那句兜底文案不再作为标记渲染出来"——按标签写，别按裸文案写：
  // 注释里也会原样引用这句话，裸文案会被注释满足而永远绿（见 TOOLING"断言前先剥注释"）。
  assert.doesNotMatch(page, /<p className="mobile-empty">当前分类没有任务/);
  // 「编辑 / 删除」现在是操作行里的真按钮（原来那段是 setTimeout + DOM append 的 imperative
  // 补丁，还靠 visibleTasks[index] 与行下标对齐）。data-* 留给探针选择器。
  assert.doesNotMatch(page, /document\.querySelectorAll<HTMLElement>\("\.mobile-task-panel \.mobile-task"\)/);
  assert.match(page, /data-mobile-task-edit="true" hidden=\{!canEditTask\}/);
  assert.match(page, /data-mobile-task-delete="true"/);
  // 操作行顺序：主操作 → 详情 → 编辑 → 删除（"删除"不再挂在详情开关旁边）。
  const footAt = page.indexOf('className="mobile-task-foot"');
  const detailAt = page.indexOf('className="mobile-task-detail"');
  const editAt = page.indexOf('data-mobile-task-edit="true"');
  const deleteAt = page.indexOf('data-mobile-task-delete="true"');
  assert.ok(footAt > 0 && detailAt > footAt && editAt > detailAt && deleteAt > editAt, "操作行顺序应为 主操作 → 详情 → 编辑 → 删除");
  // 描述在卡片上只给一行预览，但完整描述与完整更新时间必须还找得到（「详情」展开）。
  assert.match(page, /className="mobile-task-expanded"/);
  assert.match(page, /const \[expandedTask, setExpandedTask\] = useState\(""\);/);
  // 关掉面板就收起「详情」、并清掉搜索词，否则下次打开时上一轮展开的那条还开着（像自己弹开的）、
  // 而且上一轮的搜索词会**无声地**继续过滤列表（用户看到的是"任务莫名其妙少了"）。
  // 两者共用同一条 effect（依赖 tasksOpen 能覆盖所有关闭路径），所以断言按整块写。
  assert.match(page, /if \(!tasksOpen\) \{\n\s*setExpandedTask\(""\);\n\s*setTaskQuery\(""\);\n\s*\}/);
  // 「详情」是展开器，读屏要知道三件事：现在展没展开、展开的是哪一块。`aria-controls` 只在
  // 展开时给 —— 折叠状态下指一个不存在的 id 属于悬空引用，无障碍审查会判失败。
  assert.match(page, /className="mobile-task-detail" aria-expanded=\{expandedTask === task\.id\} aria-controls=\{expandedTask === task\.id \? `mobile-task-detail-\$\{task\.id\}` : undefined\}/);
  assert.match(page, /className="mobile-task-expanded" id=\{`mobile-task-detail-\$\{task\.id\}`\}/);
  // ⚠️ 列宽必须夹住（与 `.mobile-message-list` 同一个坑）：`.mobile-task-list` 是 display: grid，
  // 隐式列 `auto` 的 growth limit 取**内容 max-content**，一条 30 字长标题就能把列顶到 510px
  // （实测 390 屏：列表 362 / 卡片 510 / 「删除」被推到屏幕外，108~218px 横向溢出）。
  // 两道闸各承重一次，缺一即复发；效果由 browser 探针 `probe-task-panel` 的 track 断言守。
  assert.match(styles, /\.mobile-task-list \{ grid-template-columns: minmax\(0, 1fr\); \}/);
  assert.match(styles, /\.mobile-task-panel \.mobile-task \{ display: block; min-width: 0;/);
  // 320 屏卡片内宽只有 262px，而四颗按钮是 `flex: none` 不会自己缩：4×56 + 3×16 = 272 会顶出 10px。
  const narrowStyles = styles.slice(styles.indexOf("@media (max-width: 359px)"), styles.indexOf("@media (max-width: 380px)"));
  assert.ok(narrowStyles.length > 0, "没切到 ≤359px 那一档");
  assert.match(narrowStyles, /\.mobile-task-panel \.mobile-task-foot \{ gap: 12px; \}/);
  assert.match(narrowStyles, /\.mobile-task-panel \.mobile-task-foot button \{ padding: 0 12px; \}/);
  // 对比度按项目闸门算过的值钉住（小字 ≥4.5、禁用 ≥3），这些值都是算出来而不是手感调的：
  //   #6f8a7f 在白底只有 3.74 → 已完成/已取消标题改 #5f7a6e(4.67)、展开区时间改 #587568(5.05)；
  //   禁用浮钮原为浅字压深底 2.41 → 改 #64857a on #e3efe9(3.44)；
  //   实心主操作叠 opacity .5 后是"白字压白底" ≈2.11 → 改配色式禁用 #64857a on #f4f8f6(3.79)。
  assert.doesNotMatch(styles, /color: #6f8a7f/);
  assert.doesNotMatch(styles, /\.mobile-task-panel \.mobile-task-foot button:disabled \{ opacity/);
  assert.match(styles, /\.mobile-task-panel \.mobile-task\[data-status="done"\] \.mobile-task-title, \.mobile-task-panel \.mobile-task\[data-status="cancelled"\] \.mobile-task-title \{ color: #5f7a6e; \}/);
  assert.match(styles, /\.mobile-task-panel \.mobile-task-expanded time \{ color: #587568;/);
  assert.match(styles, /\.mobile-task-panel \.mobile-task-fab:disabled \{ color: #64857a; background: #e3efe9;/);
  assert.match(styles, /\.mobile-task-panel \.mobile-task-foot button:disabled \{ color: #64857a; \}/);
  assert.match(styles, /\.mobile-task-panel \.mobile-task-foot \.mobile-task-primary:disabled \{ border-color: #d3e3da; background: #f4f8f6; color: #64857a; \}/);
  // ⚠️ `white-space: normal` 是**必须显式写**的一条，不是可有可无的装饰：
  //    第 71 行那条 legacy 规则 `.mobile-task strong`（无作用域，本来是给非 App 的 Web 视图那张
  //    任务列表用的）也命中卡片标题（标题就是 <strong>），它带 `white-space: nowrap` + `text-overflow:
  //    ellipsis`，而这两条标题规则里没有对应声明 → 整条泄漏进来，实测 42 字标题被压成 1 行
  //    （scrollWidth 672 / clientWidth 332），`-webkit-line-clamp: 2` 的第 2 行永远不出现。
  //    **属性级的泄漏只能靠属性级声明压住**，改特异性没用。效果由 browser 探针
  //    `probe-task-card-text`（按"盒子高度 ÷ 行高"数行）守。
  assert.match(styles, /\.mobile-task-panel \.mobile-task-title \{[^}]*overflow: hidden; white-space: normal;[^}]*line-clamp: 2;/s);
  assert.match(styles, /\.mobile-task-filters button > span \{ color: #587568;/);
  assert.match(styles, /\.mobile-task-filters button\.active > span \{ color: #dff1e9; \}/);
  // 「删除」的禁用态不能用 opacity：白卡上叠 50% 是 2.25:1，连 3:1 的禁用下限都过不了，
  // 而且同一排的「下发」已经是配色式禁用（3.79:1）。三个属性都要 `!important` ——
  // 上面那条 `.mobile-task-delete` 基础规则就是 `!important` 的，非重要声明永远压不过它。
  assert.doesNotMatch(styles, /\.mobile-task-delete:disabled \{ opacity/);
  assert.match(styles, /\.mobile-task-panel \.mobile-task-foot \.mobile-task-delete:disabled \{ border-color: #e6d5d0 !important; color: #8a6259 !important; background: #f7efed !important; opacity: 1; \}/);
  // 创建失败的理由必须画在弹层**里面**：页面级那条 `.mobile-error` 在遮罩之下，
  // 弹层不关就永远看不到"为什么创建失败"，而弹层又不会自己关（保住用户输入）。
  assert.match(page, /const \[createError, setCreateError\] = useState\(""\);/);
  assert.match(page, /className="mobile-task-modal-error" role="alert"/);
  assert.match(styles, /\.mobile-task-modal \.mobile-task-modal-error \{/);
  // 优先级文案复用桌面看板的 priorityLabels，不在手机端另写一份枚举。
  assert.match(page, /import \{ priorityLabels, statusLabels, type Priority \} from "\.\.\/features\/tasks\/task-model";/);
  assert.match(page, /return priorityLabels\[priority as Priority\] \|\| priority \|\| "普通";/);
  assert.doesNotMatch(page, /<small>\{task\.priority \|\| "normal"\}<\/small>/);
  // 状态文案也必须共用 statusLabels：卡片徽标曾经把 action_required 写成"需要操作"，
  // 而同一屏的分类胶囊、桌面看板（TaskBoard）和 insights 都叫"需处理"——一个状态两个名字。
  assert.doesNotMatch(page, /action_required: "需要操作"/);
  assert.match(page, /const labels: Record<string, string> = \{\s*\.\.\.statusLabels,/);
  assert.match(page, /\{ id: "action_required", label: statusLabels\.action_required \}/);
  // 分类胶囊与状态徽标必须对同一个状态给出同一个词（从 statusLabels 取，天然一致）。
  assert.doesNotMatch(page, /\{ id: "action_required", label: "需处理" \}/);
});

// 症状（用户报的）：在手机上编辑任务时点「优先级」，跳出来的是系统自己那个很难看的
// 单选页面（Android 是整屏列表、iOS 是底部滚轮），而不是在表单里就地选一下。
// 根因：那一格用的是**原生 select** —— 它弹出的那一层由系统绘制，本页 CSS 一行都管不到。
// 现在改成一行四档分段按钮：档位同屏可见、点一下就选中，与分类栏 / 选 Agent 同一种交互语言。
// 三个渲染点（编辑弹层 / 新建弹层 / 非 App 的 Web 表单）共用同一个组件，不各写一套。
test("mobile task forms pick a priority in place instead of opening a native picker", () => {
  // 必须先剥掉整行注释再断言：本用例第一版就栽在"注释满足了 negative 断言"上 ——
  // 上面那段注释里写了"不能写成 <label> 包住这排按钮"，于是 `doesNotMatch(/<label…优先级/)`
  // 被自己的注释命中，报的是一条指向应用的假红（TOOLING「断言前先剥注释」）。
  const code = page.replace(/^\s*\/\/.*$/gm, "");
  // ① 整个移动端页面里不许再出现原生 select，连它的样式规则都不留（别只挪走 JSX）。
  assert.doesNotMatch(code, /<select/);
  assert.doesNotMatch(styles, /mobile-task-modal select/);
  assert.doesNotMatch(styles, /mobile-task-modal label select/);
  // ② 档位清单必须**派生**自桌面看板那份 priorityLabels，不能在手机端另手写一份
  //    （旧实现的 <option> 把四个 value 与四句中文一字一句抄了一遍，两处各改各的）。
  assert.match(code, /const priorityOptions = Object\.keys\(priorityLabels\) as Priority\[\];/);
  assert.doesNotMatch(code, /<option value="urgent">紧急<\/option>/);
  // ③ 两个渲染点：编辑弹层、新建弹层。（原来是三个 —— 第三个在电脑端那张常驻创建表单里，
  //    2026-09-17 随方案 B 一起撤了；见 `.tmp/patch-desktop-remote.py`。）
  //    两个 name **必须各用各的**：命名重复就是两个 `#…-priority-label`，
  //    `aria-labelledby` 指到谁全看文档顺序（这两张弹层不会同时挂载，但改名成本为零）。
  assert.equal((code.match(/<MobilePriorityPicker /g) || []).length, 2, "优先级选择器的渲染点应恰好 2 处");
  assert.match(code, /<MobilePriorityPicker name="mobile-task-edit-priority" value=\{editPriority\} onChange=\{setEditPriority\} \/>/);
  assert.match(code, /<MobilePriorityPicker name="mobile-task-create-priority" value=\{createPriority\} onChange=\{setCreatePriority\} \/>/);
  // ④ 无障碍语义：一组 radiogroup，每档是 role=radio 的按钮，选中态写在 aria-checked 上。
  //    样式只认 aria-checked（见 CSS）—— 视觉与语义同一个来源，不会出现"看着选中、读屏没选中"。
  assert.match(code, /className="mobile-priority-picker" role="radiogroup" aria-labelledby=\{`\$\{name\}-label`\}/);
  assert.match(code, /<span className="mobile-priority-field-label" id=\{`\$\{name\}-label`\}>优先级<\/span>/);
  assert.match(code, /key=\{option\} type="button" role="radio" aria-checked=\{value === option\} className="mobile-priority-option"/);
  // ⑤ ⚠️ 标题不许写成原生 label 元素包住这排按钮：button 是 labelable 元素，包进去之后
  //    "点标题文字"会变成"点第一颗按钮"（无声地把优先级选成「紧急」）。
  assert.doesNotMatch(code, /<label[^>]*>[^<]*优先级/);
  // ⑥ 新建任务必须把用户点的那一档带进 payload 与乐观卡片，并在提交后把这一格复位。
  //    旧实现两处都写死 "normal"：用户在手机上建不出「紧急」的任务。
  assert.match(code, /const \[createPriority, setCreatePriority\] = useState\("normal"\);/);
  assert.match(code, /const chosenPriority = createPriority \|\| "normal";/);
  assert.match(code, /task: \{ id: optimisticID, title: trimmedTitle, description: trimmedDescription, priority: chosenPriority,/);
  assert.match(code, /payload: \{ title: trimmedTitle, description: trimmedDescription, priority: chosenPriority \}/);
  assert.match(code, /setTitle\(""\); setDescription\(""\); setCreatePriority\("normal"\); setCreatingTask\(false\);/);
  // 失败回滚要把优先级一起放回去，否则弹层重开时那一格显示的是「普通」，而用户明明选过「紧急」。
  assert.match(code, /setDescription\(trimmedDescription\);\s*setCreatePriority\(chosenPriority\);/);
  // ⑦ 编辑弹层打开时用这条任务自己的档位（不是默认普通，也不是上一轮改过的值）。
  assert.match(code, /setEditPriority\(task\.priority \|\| "normal"\);/);
  // ⑧ 文案只有 task-model.ts 一个来源：本页**任何一处**都不许直接打印原始枚举
  //    （`normal · todo` / `high`）。原来那条举证挂在电脑端的任务列表上，那张列表已经撤了，
  //    所以改成全文断言 —— 判据没变，覆盖面反而更宽。
  assert.doesNotMatch(code, /\{task\.priority\} · \{task\.status\}/);
  assert.doesNotMatch(code, />\{task\.priority\}</);
  assert.match(code, /taskPriorityLabel\(task\.priority\)/);
  assert.match(code, /taskStatusLabel\(task\.status\)/);
  // ⑨ 样式：四档等分一行（320 屏每格仍有 57px，不许退回 flex / 换行 / 横向滚动）；
  //    44px 是触控尺寸；选中态由 [aria-checked="true"] 决定，主色实心 + 白字（5.46:1）。
  assert.match(styles, /\.mobile-priority-picker \{ display: grid; grid-template-columns: repeat\(4, minmax\(0, 1fr\)\); gap: 8px; \}/);
  // ⑩ ⚠️ 两条选项规则的**选择器必须是两个类**（0,2,0）：本组件渲染在 `.mobile-task-modal` 里，
  //    而"给弹层里的 button 加一条样式"是随时会发生的事（同文件已有
  //    `.mobile-task-modal footer button` = 0,2,1）。任何 (0,1,1) 的声明都赢不了 (0,1,0)
  //    两类的写法 —— 特异性先于顺序。
  //    历史教训（2026-09-17 已消除）：本组件一度也放进 `!mobileApp`（电脑端）那一支，那里有
  //    一批无作用域的 legacy 规则（`.mobile-create button` 等，(0,1,1)），整条命中之后
  //    四档**全部**被涂成实心深绿、选中态消失，还各自缩成内容宽度（54/41/54/41，格子宽 84）。
  //    那批规则随电脑端改用 desktop-remote-* 一起删了，这三条也不再是空话。
  assert.match(styles, /\.mobile-priority-picker \.mobile-priority-option \{ width: 100%; min-width: 0; min-height: 44px;[^}]*color: #416b5e; background: #f7fbf8;/s);
  assert.match(styles, /\.mobile-priority-picker \.mobile-priority-option\[aria-checked="true"\] \{ border-color: #2c7567; color: #fff; background: #2c7567; box-shadow: 0 0 0 2px #bfe2cf; \}/);
  assert.doesNotMatch(styles, /\.mobile-priority-option\.active/);
  assert.doesNotMatch(styles, /^\.mobile-priority-option \{/m);
  // ⑪ 弹层必须能滚：横屏手机 844×390 下它高 461px，视口只有 390px —— 没有 max-height + overflow-y 时
  //    底部那颗「保存」直接落在屏幕外（实测 top=16 / bottom=477、overflow-y 还是 visible，点不到）。
  assert.match(styles, /\.mobile-task-modal \{[^}]*max-height: calc\(100dvh - 32px\);[^}]*overflow-y: auto;/s);
});

// 2026-09-17 复查顺带抓到的一条**既有缺陷**（不是本轮改优先级引入的）：
// 创建失败时弹层会停在原地、输入也回填了，但"为什么失败"一个字都看不到 ——
// 因为清空失败原因写成了 `useEffect(() => { if (creatingTask) setCreateError("") }, [creatingTask])`，
// 而失败回滚那条路径**也会**把 creatingTask 置为 true：同一个批次里刚写进去的
// setCreateError(reason) 在提交之后被这个 effect 立刻抹掉。
// browser 探针 probe-task-create 的失败态断言（"失败理由画在弹层里面且是 alert 语义"）
// 一直红着就是这个原因；下面是同一件事的源码级闸门。
test("mobile create failures keep their reason on screen", () => {
  // 注释里会写出被禁止的写法，先剥整行注释再断言（否则否定断言被自己的注释满足）。
  const code = page.replace(/^\s*\/\/.*$/gm, "");
  // 清"上一次的失败原因"必须发生在**用户点加号**那一刻：只有那条路径才该清。
  assert.match(code, /onClick=\{\(\) => \{ setCreateError\(""\); setCreatingTask\(true\); \}\}/);
  // 绝不能再退回"依赖 creatingTask 的 effect"：它分不清是谁开的弹层。
  assert.doesNotMatch(code, /if \(creatingTask\) setCreateError\(""\);/);
  // 失败回滚仍要把理由写进弹层、并且不关弹层。
  assert.match(code, /setCreatePriority\(chosenPriority\);\s*setCreateError\(reason\);\s*setCreatingTask\(true\);/);
});

test("mobile conversation history is a button that opens a dialog with one row per conversation", () => {
  // 原先是下拉框：选项里直接渲染整条标题，标题一长选项就撑到十几行、把面板拉得很高。
  // 现在是 ⋯ 菜单里的一项，点开是弹窗，弹窗里一行一个会话。
  assert.match(page, /<button type="button" role="menuitem" onClick=\{\(\) => \{ setHeaderMenuOpen\(false\); setConversationHistoryOpen\(true\); \}\} disabled=\{conversations\.length === 0\}><span>历史会话<\/span>/);
  // 下拉框那一整套必须彻底消失（触发器里的标题 span、选项列表、listbox 角色、对应 CSS），别留半套。
  assert.doesNotMatch(page, /mobile-conversation-picker/);
  assert.doesNotMatch(page, /mobile-conversation-options/);
  assert.doesNotMatch(page, /aria-haspopup="listbox"/);
  assert.doesNotMatch(styles, /\.mobile-conversation-picker/);
  assert.doesNotMatch(styles, /\.mobile-conversation-options/);
  // 弹窗沿用本页其它弹窗的约定：backdrop 上 role=dialog + aria-modal + labelledby，头部一个关闭按钮。
  assert.match(page, /className="mobile-conversation-history-backdrop" role="dialog" aria-modal="true" aria-labelledby="mobile-conversation-history-title"/);
  assert.match(page, /<h2 id="mobile-conversation-history-title">历史会话<\/h2>/);
  assert.match(page, /aria-label="关闭历史会话"/);
  // 一行一个会话：标题单行截断 + 元信息一行（长标题不再把条目撑高），当前会话标出来。
  assert.match(page, /className=\{`mobile-conversation-history-item\$\{/);
  assert.match(page, /item\.id === conversation\?\.id \? " active" : ""/);
  assert.match(page, /aria-current=\{item\.id === conversation\?\.id \? "true" : undefined\}/);
  assert.match(styles, /\.mobile-conversation-history-item strong \{[^}]*overflow: hidden;[^}]*text-overflow: ellipsis;[^}]*white-space: nowrap;/s);
  // 列表自己滚动：min-height: 0 才能在 flex 列里真正收缩，会话很多时不会把弹窗顶出屏幕。
  assert.match(styles, /\.mobile-conversation-history-list \{[^}]*min-height: 0;[^}]*overflow-y: auto;/s);
  assert.match(styles, /\.mobile-conversation-history-dialog \{[^}]*max-height: min\(78dvh, 640px\);/s);
  // 点背景关闭：用 target === currentTarget 判定，不依赖 stopPropagation。
  assert.match(page, /if \(event\.target === event\.currentTarget\) setConversationHistoryOpen\(false\);/);
  // 安卓返回键先关弹窗再退视图；侧滑返回（popstate → 退视图）也要把弹窗一起关掉，
  // 否则退回项目列表后还挂着一个"选会话"弹窗。
  assert.match(page, /if \(conversationHistoryOpen\) \{ setConversationHistoryOpen\(false\); return true; \}/);
  // 注意别用 `[\s\S]*?` 直接跨到文件后面——那样即使删掉这里的调用也照样绿（变异检验抓到过一次）。
  // 先把函数体单独抠出来再断言，范围就锁死在闭括号之前。
  const leaveBody = page.match(/function leaveConversationView\(\) \{[\s\S]*?\n  \}/);
  assert.ok(leaveBody, "找不到 leaveConversationView");
  assert.match(leaveBody[0], /setTasksOpen\(false\);/);
  assert.match(leaveBody[0], /setConversationHistoryOpen\(false\);/);
  // 顶栏 ⋯ 菜单也是压在会话视图上的一层浮层，退视图时必须一起收掉。
  assert.match(leaveBody[0], /setHeaderMenuOpen\(false\);/);
  // 侧滑返回走的是 popstate → 这条路**不经过** backHandlerRef，所以压在会话上的浮层必须在
  // leaveConversationView 里各自收一次。快捷方式确认框就是漏在这里：它的 backdrop 是
  // position: fixed，会直接盖在项目列表上，而里面的「执行」是真会往电脑端发命令的
  // （浏览器实测：漏掉这一行时，退出视图后确认框仍在 DOM、按钮仍可点、命令仍发得出去）。
  assert.match(leaveBody[0], /setConfirmShortcut\(null\);/);
  // 工具面板同理：它渲染在会话块内部，退视图后看不见但状态还在；重进**同一个**项目时
  // selectedProject/selectedConversation 都没变，上面那个清理 effect 不会重跑，面板会自己冒出来。
  assert.match(leaveBody[0], /setComposerToolsOpen\(false\);/);
});

test("mobile conversation header collapses to one line with an overflow menu", () => {
  // 【本轮定案】顶部从"两段三行"归并成"一行标题 + ⋯"。
  // 旧结构（会话标题条 + 状态行 + 「历史/新会话/任务」按钮行）实测在 390×800 上吃掉 199px，
  // 接近四分之一屏；而那三个入口与输入条「＋」面板的「会话」组**完全重复**，等于拿最贵的
  // 顶部空间换了一次重复。新结构实测 70px。
  assert.doesNotMatch(page, /mobile-conversation-toolbar/);
  assert.doesNotMatch(styles, /\.mobile-conversation-toolbar/);
  assert.doesNotMatch(page, /mobile-task-toggle/);
  assert.doesNotMatch(styles, /\.mobile-task-toggle/);
  // 触发任务面板的按钮改名成 headerMenuButtonRef —— 它现在是 ⋯，也兼作面板关闭后的焦点落点，
  // 留一个只为"还焦点"存在的旧 ref 名会让人以为按钮还在。
  assert.doesNotMatch(page, /taskToggleRef/);

  // 标题栏放的是**项目名**，不是会话名：会话名是首条用户消息截出来的前 80 字（编排会话更是
  // 直接拼的任务标题），长度不受控、还会随对话内容变，挂在顶栏那行大字上是句半截的话。
  // 顶栏因此不再读会话名 —— 也不再需要那份"不带 · Agent 后缀"的 rawTitle 副本。
  assert.doesNotMatch(page, /rawTitle/);
  // 文件面板打开时标题换成「项目文件」——它是会话视图的子态，所以三个分支写在同一个表达式里，
  // 而不是另起一个 h1（两个 h1 会在切换时闪两次）。
  assert.match(page, /\{filesHeaderActive \? "项目文件" : gitHeaderActive \? "Git 工作台" : showConversationTitle \? \(project\?\.name \|\| "项目对话"\) : "Milevia"\}/);
  // 返回键的去处在两个视图里不同，写成一处分档的文案（mobileBackLabel），
  // 别在 title 与 aria-label 上各写一遍三元表达式 —— 那样改一处必漏一处。
  assert.match(page, /const mobileBackLabel = subHeaderActive \? "返回会话" : mobileView === "conversation" \? "返回项目" : "返回";/);
  // 两套 ⋯ 菜单**互斥渲染**：文件面板上那四项（历史会话/新会话/任务队列/重新同步）点开是
  // 另一件事，硬塞进同一张表里会让"刷新"在文件视图里变成重新同步快照。
  assert.match(page, /\{filesHeaderActive && <div className="mobile-header-menu">/);
  assert.match(page, /\{conversationHeaderActive && project && !subHeaderActive && <div className="mobile-header-menu">/);
  assert.match(page, /<div className="mobile-header-menu-sheet" id="mobile-files-menu-sheet" role="menu" aria-label="文件操作">/);
  // 历史列表继续用带后缀的 title —— 那里一行只有标题，多这一截反而有用，别一起改掉。
  assert.match(page, /title: `\$\{item\.title \|\| "未命名会话"\} · \$\{conversationAgentLabel\(item\.agentId\)\}`/);
  // 会话里不画品牌方块：把横向空间让给更长的项目名（项目列表视图不受影响）。
  assert.match(styles, /\.mobile-conversation-mode \.mobile-brand-mark \{ display: none; \}/);

  // ⋯ 按钮：38×38、aria 齐全（haspopup/expanded/controls 三者缺一，读屏用户就不知道它是弹层触发器）。
  assert.match(page, /className="mobile-header-menu-button" ref=\{headerMenuButtonRef\} type="button" aria-haspopup="menu" aria-expanded=\{headerMenuOpen\} aria-controls="mobile-header-menu-sheet"/);
  assert.match(styles, /\.mobile-header-menu-button \{[^}]*width: 38px;[^}]*height: 38px;/s);
  // 热区靠 ::after 外扩，外扩量**按有没有 1px 边框**分档：inset 的参照系是 padding box，
  // 带边框的按钮写 -3px 只能扩到 42。实测过：⋯ 右侧 border-box 外 2px 处已落到 <main> 上
  // （正好压在 ::after 边界上，半开区间判为界外），左侧却因亚像素取整侥幸命中——
  // 同一份 CSS 两侧表现不一致，就是"热区偏小 2px"的典型症状。刷新胶囊 / 开启通知同理。
  assert.match(styles, /\.mobile-back::after \{ position: absolute; inset: -3px; content: ""; \}/);
  assert.match(styles, /\.mobile-refresh::after, \.mobile-notification-button::after, \.mobile-header-menu-button::after \{ position: absolute; inset: -4px; content: ""; \}/);
  // 展开态**不**整颗填实心深绿：⋯ 字形与右上角标同为浅色，填实会糊成一团深绿块，看不出是按钮。
  // 与刷新胶囊忙碌态同一套做法——状态靠底色变化表达。
  assert.match(styles, /\.mobile-header-menu-button\[aria-expanded="true"\] \{ border-color: #2c7567; color: #1d5b4e; background: #dcefe4; \}/);

  // 菜单本体：role=menu、Agent 信息行 + 四项操作、每项 ≥44、禁用态用配色不用 opacity。
  assert.match(page, /<div className="mobile-header-menu-sheet" id="mobile-header-menu-sheet" role="menu" aria-label="更多操作">/);
  assert.match(page, /className="mobile-header-menu-info"><span>执行 Agent<\/span>/);
  assert.match(styles, /\.mobile-header-menu-sheet button \{[^}]*min-height: 44px;/s);
  assert.match(styles, /\.mobile-header-menu-sheet button:disabled \{ color: #64857a; \}/);
  // 面板要能塞进最窄的屏：min-width 用 min() 夹住，否则 320px 上会顶破视口。
  assert.match(styles, /\.mobile-header-menu-sheet \{[^}]*min-width: min\(216px, calc\(100vw - 32px\)\);/s);
  // 角标：只在"还有任务没跑完"时渲染。它是唯一一眼可见的状态信号，收进菜单就等于看不见了；
  // 口径 = done / cancelled 之外（与共享层 filterQueueTasks(…, "active") 一致）。
  assert.match(page, /activeTaskCount > 0 && <span className="mobile-header-menu-badge" aria-hidden="true">\{activeTaskCount\}<\/span>/);
  assert.match(page, /task\.status !== "done" && task\.status !== "cancelled"/);

  // 关闭路径：点面板外 + Esc。Esc 必须把焦点还给 ⋯，否则键盘用户掉在 document.body 上、
  // 只能从头 Tab 一遍（本页其它浮层早就有这个约定）。
  assert.match(page, /if \(target instanceof Element && target\.closest\("\.mobile-header-menu"\)\) return;/);
  assert.match(page, /if \(event\.key !== "Escape"\) return;/);
  assert.match(page, /headerMenuButtonRef\.current\?\.focus\(\);/);
  // 安卓返回键的第一优先级是收菜单：菜单是页面上最上层的一小块浮层，别一步退到项目列表。
  assert.match(page, /if \(headerMenuOpen\) \{ setHeaderMenuOpen\(false\); return true; \}/);
  // 换会话时收起菜单：菜单里的「历史会话 N / 任务队列 N」是当前项目的计数，留着会显示成旧数字。
  const switchEffect = page.match(/setConversationHistoryOpen\(false\);\n    setComposerToolsOpen\(false\);[\s\S]*?\}, \[selectedProject, selectedConversation\]\);/);
  assert.ok(switchEffect, "找不到换会话时的清理 effect");
  assert.match(switchEffect[0], /setHeaderMenuOpen\(false\);/);

  // 状态胶囊：只在非 idle 时渲染。「就绪」是默认态、没有信息量，不该占位；而
  // 排队中 / 执行中 / 已完成 / 失败 / 已停止 都值得一眼看到，尤其"失败"和"已停止"。
  assert.match(page, /conversation\?\.status && conversation\.status !== "idle" \? conversationStatusLabel\(conversation\.status\) : ""/);
  assert.match(page, /\{conversationHeaderActive && conversationState && <span className=\{`mobile-conversation-state mobile-conversation-state-\$\{conversation\?\.status\}`\}>\{conversationState\}<\/span>\}/);
  assert.match(styles, /\.mobile-conversation-state-failed, \.mobile-conversation-state-stopped \{ color: #9b3e33; background: #fdeeea; \}/);

  // 两个视图各留各的入口，互不干扰：刷新胶囊只在非会话视图渲染，⋯ 只在会话视图渲染。
  assert.match(page, /\{!conversationHeaderActive && <button className="mobile-refresh"/);
  // 文件面板打开时这一套让位给文件那套（两套互斥，见文件视图那条用例）。
  assert.match(page, /\{conversationHeaderActive && project && !subHeaderActive && <div className="mobile-header-menu">/);
});

test("mobile web exposes notification permission when the browser supports it", () => {
  assert.match(page, /notificationPermission === "default"/);
  assert.match(page, /setNotificationPermission\(permission\)/);
  assert.match(page, /onClick=\{\(\) => void enableMobileNotifications\(\)\}/);
  assert.doesNotMatch(page, /Notification\.permission !== "granted" && <button className="mobile-notification-button"/);
});

test("legacy QR URLs automatically submit their embedded pairing code", () => {
  assert.match(page, /const initialPairing = useRef\(\{/);
  assert.match(page, /get\("pairingId"\) \|\| new URLSearchParams\(location\.search\)\.get\("pairing_id"\)/);
  assert.match(page, /const \{ pairingID: initialPairingID, code: initialPairingCode \} = initialPairing\.current/);
  assert.match(page, /initialPairing\.current\.attempted/);
  assert.match(page, /initialPairing\.current\.attempted = true/);
  assert.match(page, /if \(!mobileApp \|\| initialPairing\.current\.attempted \|\| !initialPairingID \|\| !\/\^\\d\{6\}\$\//);
  assert.match(page, /void claimPairing\(initialPairingID, initialPairingCode\)/);
});

test("mobile cloud request timeout covers response body parsing", () => {
  assert.match(page, /response = await fetch[\s\S]*?const body = await response\.json\(\)/);
  assert.match(page, /const body = await response\.json\(\)\.catch\(\(\) => null\);[\s\S]*?finally \{/);
});

test("mobile event stream uses an Authorization header instead of a URL token", () => {
  assert.match(page, /Accept: "text\/event-stream", Authorization: `Bearer \$\{token\}`/);
  assert.match(page, /headers\.set\("Last-Event-ID", lastEventID\)/);
  assert.doesNotMatch(page, /access_token/);
  assert.doesNotMatch(page, /new EventSource\(/);
});

test("entering a project pushes a real history entry so the system back gesture returns", () => {
  // 症状：进入某个项目后，从屏幕最左边向右滑（Chrome/WebView 的返回手势、iOS 边缘手势）没有任何反应。
  // 根因：进项目只是 setMobileView("conversation")，视图切换没有进浏览器历史，而侧滑手势本质是
  // **历史后退**——历史里没有这一层，手势就没有可退的东西。页内「←」和安卓返回键都在 React 里兜着，
  // 所以只有系统手势/浏览器返回键失效。
  assert.match(page, /const conversationHistoryRef = useRef\(false\)/);
  // 进入时压一层带标记的历史，并且必须保留 react-router 存在 history.state 里的 { usr, key, idx }
  // （idx 递增），否则路由按 idx 差值算"退了几步"会算错。
  assert.match(page, /window\.history\.pushState\(\{[\s\S]*?mileviaMobileConversation: true,[\s\S]*?\}, ""\);/);
  assert.match(page, /typeof base\.idx === "number" \? \{ idx: base\.idx \+ 1 \} : \{\}/);
  // 只压一次：同一次进入可能被 HTTP 响应和 SSE conversation.created 两条路径都触发。
  assert.match(page, /if \(!conversationHistoryRef\.current\) \{[\s\S]*?conversationHistoryRef\.current = true;[\s\S]*?\}/);
  // 手势/浏览器返回键：popstate 只同步视图，**不再退一次历史**（否则会把用户直接带出应用）。
  assert.match(page, /window\.addEventListener\("popstate", onPopState\)/);
  assert.match(page, /const onPopState = \(\) => \{\s*if \(!conversationHistoryRef\.current\) return;\s*conversationHistoryRef\.current = false;\s*leaveConversationView\(\);\s*\};/);
  // 进程被回收后恢复时，历史里的残留标记要如实认领，否则用户下一次返回会被空吞一次。
  assert.match(page, /conversationHistoryRef\.current = window\.history\.state\?\.mileviaMobileConversation === true;/);
  // 视图切换只能由这三个 helper 发起：进入点（点项目卡、新建会话**那一刻**）都要走
  // enterConversationView，否则会出现"手势要退两层"或"压了不认"的不一致。
  //
  // 新建会话的进入点从"电脑端回了成功"提前到了"本地建好"（见 conversation-mutations）——
  // 因此 SSE 的 conversation.created 不再是进入点：它到达时用户早就在这条会话里了，
  // 那时要做的是把本地那条换成真身（认领 effect），不是再切一次视图。
  //
  // 用 >= 而不是 ==：将来**正确地**新增进入点（也调 helper）不该把测试判红，否则只会教人改数字；
  // 真正的不变式是下面那两条——setMobileView 只允许出现在两个 helper 里各一次，绕过 helper 必红。
  assert.ok((page.match(/enterConversationView\(\);/g) ?? []).length >= 2, "两个进入点都要走 enterConversationView");
  assert.ok((page.match(/exitConversationView\(\);/g) ?? []).length >= 4, "四个退出点都要走 exitConversationView");
  assert.equal((page.match(/setMobileView\("conversation"\)/g) ?? []).length, 1);
  assert.equal((page.match(/setMobileView\("projects"\)/g) ?? []).length, 1);
  // 退出路径（页内返回、安卓返回键、项目消失、重新配对）统一走 exitConversationView：
  // 视图切回项目列表 + 把我们压的那层历史退掉。
  assert.match(page, /function exitConversationView\(\) \{\s*leaveConversationView\(\);\s*if \(conversationHistoryRef\.current\) \{\s*conversationHistoryRef\.current = false;\s*window\.history\.back\(\);\s*\}\s*\}/);
  assert.match(page, /if \(mobileApp && mobileView === "conversation"\) \{\s*exitConversationView\(\);\s*return true;\s*\}/);
});

test("mobile timeline carries run status notices alongside messages", () => {
  // 症状（用户报的）：电脑端已经在显示「API 重试中 / 正在压缩上下文 / 执行失败」，
  //   手机端只收到对话内容，看起来像卡住了。
  // 根因：applyRealtimeEvent 只认 conversation.created、assistant.delta、
  //   user.message、assistant.message，其余事件落到函数末尾 return false 被丢掉
  //   —— 而 system（重试/压缩/后台任务）与失败诊断走的正是这条路径。
  // 修法：这类事件解析成状态卡，与消息按时间混排进同一个列表。
  //
  // 解析必须来自桌面时间线同一份实现：两边各写一套枚举，就会出现电脑上说
  // 「第 2/5 次重试：服务过载」、手机上是另一套说法，改了一处另一处悄悄对不上。
  assert.match(page, /import \{ systemItemFromEvent, eventDiagnostic, cliOutputDiagnostic, getApproval \} from "\.\.\/lib\/timeline";/);
  // 断言锁在 applyRealtimeEvent 函数体内：只在模块里定义解析函数、实时路径不调用，
  // 手机上依旧什么都看不到（这正是改坏时的表现）。
  const realtimeBody = page.match(/const applyRealtimeEvent = useCallback\(\(raw: MessageEvent\): boolean => \{[\s\S]*?\n  \}, \[\]\);/)?.[0] ?? "";
  assert.ok(realtimeBody, "找不到 applyRealtimeEvent");
  assert.match(realtimeBody, /const parsed = noticeFromRealtimeEvent\(\{ eventId: event\.eventId, type: event\.type, taskRunId: event\.taskRunId, payload, createdAt: event\.createdAt \}\);/);
  assert.match(realtimeBody, /notices: appendNotice\(entry\.notices, parsed\.notice\)/);
  // 归属只能靠事件自带的会话 id：宁可晚一步（由快照补上），也不能把 A 会话的
  // 重试/失败贴到用户正在看的 B 会话里。
  assert.match(page, /const conversationId = typeof record\.conversationId === "string" \? record\.conversationId : "";/);
  assert.match(page, /if \(!conversationId\) return null;/);
  // 只有退出码、没有原因的那条（deferFallback）不展示：桌面端会等更详细的诊断，
  // 手机端没有那套归并，直接跳过，免得盖住真正的错误原因。
  assert.match(page, /if \(diagnostic && !diagnostic\.deferFallback\)/);
});

test("desktop and mobile parse run status events with the same implementation", async () => {
  const timeline = await readFile(new URL("./lib/timeline.ts", import.meta.url), "utf8");
  // 桌面 buildTimeline 必须复用共享解析（否则"共用"只是单向的，两边仍会漂移）。
  assert.match(timeline, /const item = systemItemFromEvent\(event\);/);
  assert.match(timeline, /const diagnostic = eventDiagnostic\(event\);/);
  assert.match(timeline, /export function systemItemFromEvent\(event: Event\): SystemItem \| null \{/);
  assert.match(timeline, /export function eventDiagnostic\(event: Event\): EventDiagnostic \| null \{/);
  // 压缩失败要带失败状态：手机端据此上故障色，不能跟着成功一起变绿。
  assert.match(timeline, /title: "上下文压缩失败", detail: `压缩结果：\$\{compactResult\}`, metadata: \{ state: "failed" \}/);
  assert.match(page, /const state = typeof system\.metadata\?\.state === "string" \? system\.metadata\.state : "";/);
});

test("mobile keeps run status notices across snapshot refreshes", () => {
  // 状态事件走实时通道，快照有上传节流；不在本地留一份，刚冒出来的「重试中」会被
  // 下一次快照抹掉。反过来，快照带回的记录必须留下来，手机重进页面才看得到历史。
  assert.match(page, /const pendingNoticesRef = useRef\(new Map<string, RemoteNotice\[\]>\(\)\)/);
  assert.match(page, /pendingNotices\.set\(parsed\.conversationId, appendNotice\(pendingNotices\.get\(parsed\.conversationId\), parsed\.notice\)\)/);
  // 快照里给的是事件原文（type + payload），必须走同一个解析函数——否则会出现
  // "实时到达时是正常状态卡，刷新之后变成一张没有类型、配色全丢的空卡"
  //（真机探针实测：variant 变成 undefined，六张卡一起失去配色）。
  assert.match(page, /const serverNotices = noticesFromSnapshot\(entry\.notices\);/);
  assert.match(page, /return \{ \.\.\.entry, notices: mergeNotices\(serverNotices, remaining\) \};/);
  assert.match(page, /function noticesFromSnapshot\(raw: unknown\): RemoteNotice\[\] \{/);
  // 已进入快照的记录从本地表移除，这张表才不会无限增长；但 replace 型（工具确认）
  // 必须留着——快照可能比它旧，清掉就会让状态倒退。
  assert.match(page, /const remaining = local\.filter\(\(notice\) => notice\.merge === "replace" \|\| !serverIDs\.has\(notice\.id\)\);/);
  assert.match(page, /if \(\(Date\.parse\(notice\.createdAt\) \|\| 0\) < \(Date\.parse\(existing\.createdAt\) \|\| 0\)\) return current;/);
  // 实例切换要清空：另一个实例里恰好同 id 的会话不该继承上一台的记录。
  assert.match(page, /pendingNoticesRef\.current\.clear\(\);/);
});

test("mobile notice details keep their line breaks", () => {
  // CLI 输出是多行文本；CSS 默认的 white-space: normal 会把换行折叠成空格，
  // 一段几十行的构建日志会挤成一坨。桌面端 .error-card > pre 用的就是 pre-wrap，
  // 手机端保持同一口径（同样加 word-break，与桌面一致）。
  assert.match(styles, /\.mobile-notice-text small \{[^}]*overflow-wrap: anywhere;[^}]*white-space: pre-wrap;[^}]*word-break: break-word;/s);
});

test("mobile renders notices inside the message list and before the processing indicator", () => {
  const listOpen = messageListOpen();
  const listClose = page.indexOf("</div>{tasksOpen");
  const notice = page.indexOf('className={`mobile-notice mobile-notice-${entry.notice.variant}');
  const indicator = page.indexOf('className="mobile-agent-processing"');
  // 锚点先自证存在，否则下面的比较会退化成"比 -1 大"的永真断言。
  assert.ok(listOpen > 0 && listClose > listOpen && notice > 0 && indicator > 0, "找不到消息列表 / 状态卡 / 状态条的锚点");
  assert.ok(notice > listOpen, "状态卡必须渲染在消息列表内部");
  assert.ok(notice < listClose, "状态卡必须在消息列表结束之前");
  // 「正在处理」仍是列表的最后一项：它要跟在刚发生的状态之后，而不是被状态卡挤到前面。
  assert.ok(indicator > notice, "处理状态条必须排在状态卡之后");
  // 消息与状态卡按时间混排，而不是"先列全部消息、再列全部状态"。
  assert.match(page, /return entries\.sort\(\(left, right\) => left\.at - right\.at\);/);
  // 图标沿用桌面时间线的字形，两端看到的是同一个符号。
  assert.match(page, /const noticeIcons: Record<RemoteNotice\["variant"\], string> = \{ compact: "◐", compact_result: "✓", compact_boundary: "≡", api_retry: "↻", task: "▸", error: "!" \};/);
});

test("mobile notice cards stay inside the narrow message column", () => {
  // 与消息气泡同一个坑：状态卡里的长 token 数字、长错误文本同样能把列表列顶宽，
  // 而 .mobile-remote 是 overflow-x: hidden —— 一旦被顶宽，整屏内容会被静默裁掉。
  assert.match(styles, /\.mobile-notice \{[^}]*grid-template-columns: auto minmax\(0, 1fr\) auto;[^}]*min-width: 0;/s);
  assert.match(styles, /\.mobile-notice-text \{ min-width: 0; \}/);
  assert.match(styles, /\.mobile-notice-text strong \{[^}]*overflow-wrap: anywhere;/s);
  assert.match(styles, /\.mobile-notice-text small \{[^}]*overflow-wrap: anywhere;/s);
  // 时间列只放时分并且不换行：三列 grid 里唯一允许"占固定宽度"的就是它。
  assert.match(styles, /\.mobile-notice-time \{[^}]*white-space: nowrap;/s);
  assert.match(page, /return parsed\.toLocaleTimeString\(\[\], \{ hour: "2-digit", minute: "2-digit" \}\);/);
  // 变体类名必须带 mobile-notice- 前缀。裸变体里最致命的是 `error`：style.css 用它
  // 定义了一个右上角固定的全局提示条（position: fixed / top: 16px / right: 16px /
  // max-width: min(480px, calc(100vw - 32px))），于是错误卡会被钉在屏幕右上角、
  // 宽度缩到 480px（真机探针实测 320px 视口下卡宽 288px，且脱离消息列表定位）。
  assert.match(page, /className=\{`mobile-notice mobile-notice-\$\{entry\.notice\.variant\}/);
  assert.doesNotMatch(page, /className=\{`mobile-notice \$\{entry\.notice\.variant\}/);
  assert.doesNotMatch(styles, /\.mobile-notice\.error/);
  assert.match(styles, /\.mobile-notice-error \{ border-left-color: #c65343;/);
  // 变体配色照抄桌面 .system-card：重试琥珀、失败/错误红、压缩蓝灰，不能反着来。
  assert.match(styles, /\.mobile-notice-api_retry \{ border-left-color: #b8923a;/);
  assert.match(styles, /\.mobile-notice-compact \{ border-left-color: #6b7fa3;/);
  assert.match(styles, /\.mobile-notice-task\.mobile-notice-failed \{ border-left-color: #c65343;/);
});

test("mobile aggregates CLI stderr into one notice and keeps the newest tool confirmation", () => {
  // 桌面端运行时的两类信息此前在手机上完全空白：
  // ① CLI 输出（逐行 stderr）——不聚合的话一次编译失败能刷出几十张卡，把重试/压缩
  //    那几张真要紧的挤下去，所以同一个 run 收成一条、后续行并进去（merge: "append"）。
  // ② 工具确认（approval.*）——桌面是工具卡上的按钮；手机端只读，但运行卡在等确认时
  //    必须能看到原因。同一 approvalId 会推进 pending → allow/deny，要保留最新状态
  //    （merge: "replace"），否则永远显示"等待确认"。
  assert.match(page, /if \(type === "stderr"\) \{/);
  assert.match(page, /const output = cliOutputDiagnostic\(\[message\]\);/);
  assert.match(page, /id: `stderr:\$\{runId \|\| createdAt\}`/);
  assert.match(page, /variant: "error", title: output\.title, detail: output\.detail, merge: "append"/);
  assert.match(page, /id: `approval:\$\{approval\.approvalId\}`/);
  assert.match(page, /merge: "replace",/);
  // 两种语义必须在 appendNotice 里真的落地（只写在类型注释里等于没实现）。
  const appendBody = page.match(/function appendNotice\(notices: RemoteNotice\[\] \| undefined, notice: RemoteNotice\): RemoteNotice\[\] \{[\s\S]*?\n\}/)?.[0] ?? "";
  assert.ok(appendBody, "找不到 appendNotice");
  assert.match(appendBody, /if \(notice\.merge === "replace"\) \{/);
  assert.match(appendBody, /if \(notice\.merge === "append" && notice\.detail\) \{/);
  assert.match(appendBody, /lines\.includes\(notice\.detail\)/);
  // 合并两份记录时不能退回"按 id 覆盖"，否则已经收到的 stderr 行会被丢掉。
  assert.match(page, /for \(const notice of incoming\) merged = appendNotice\(merged, notice\);/);
  const snapshotBody = page.match(/function noticesFromSnapshot\(raw: unknown\): RemoteNotice\[\] \{[\s\S]*?\n\}/)?.[0] ?? "";
  assert.ok(snapshotBody, "找不到 noticesFromSnapshot");
  assert.match(snapshotBody, /if \(notice\) items = appendNotice\(items, notice\);/);
  // 超时/中止不能被显示成"还在等确认"：状态取事件类型后缀，而不是 getApproval 归一化后的 status。
  assert.match(page, /const resolution = type\.slice\("approval\."\.length\);/);
  assert.match(page, /timeout: \{ title: "工具确认超时", state: "failed" \}/);
  assert.match(page, /deny: \{ title: "工具已拒绝", state: "failed" \}/);
  assert.match(page, /title: label\.title,/);
  assert.match(page, /resolution === "pending" \? `\$\{detail\} · 请在电脑上确认` : detail/);
  // 桌面端的 CLI 输出聚合规则与手机端共用同一份实现。
  assert.match(page, /import \{ systemItemFromEvent, eventDiagnostic, cliOutputDiagnostic, getApproval \}/);
});

test("the snapshot replays tool confirmations so a stuck run is visible after reopening", async () => {
  // 契约跨层：手机端把 approval 卡片解析出来了，但快照若不回放 approval 事件，
  // 用户重进页面就只看到"运行中"却不知道卡在等确认。这条断言锁住后端的取数条件。
  const remote = await readFile(new URL("../../../apps/control-server/internal/app/remote_control.go", import.meta.url), "utf8");
  assert.match(remote, /const remoteNoticeTypesSQL = `type in \('system','run\.failed','run\.interrupted','error','turn\.failed','stream\.error'\) or type like 'approval\.%'`/);
  // 取数条件从"只有类型表"变成了类型表 + 排除空转子类型：快照的窗口是定长的，而 CLI 每几秒
  // 一条 {"status":"requesting","subtype":"status"} 会把 24 格占满（实测只剩 4~10 格是真卡片），
  // 一张"后台任务启动"卡片几分钟就被挤出去。所以这里必须锁到**新**的谓词上。
  assert.match(remote, /where conversation_id=\? and %s order by created_at desc,id desc limit \?`, remoteNoticeReplayPredicate\("events"\)\)/);
  const replayPredicate = remote.match(/func remoteNoticeReplayPredicate\(alias string\) string \{[\s\S]*?\n\}/)?.[0] ?? "";
  // 锚点先自证存在：抠不到函数体时下面的断言会退化成对空串匹配。
  assert.ok(replayPredicate, "找不到 remoteNoticeReplayPredicate（签名变了就更新本用例的锚点）");
  assert.match(replayPredicate, /remoteNoticeTypesSQL/);
  assert.match(replayPredicate, /unrenderedSystemSubtypePredicate/);
  // 排除项本身长在 event_replay.go（会话历史分页与手机端快照共用同一条，见那边的注释），
  // 所以第二个文件也要读。
  const replay = await readFile(new URL("../../../apps/control-server/internal/app/event_replay.go", import.meta.url), "utf8");
  assert.match(replay, /func unrenderedSystemSubtypePredicate\(alias string\) string \{/);
  // 排除项必须**只认 type='system'**：写成无条件的子类型匹配，会把 approval.* 这类
  // 根本不属于 system 的事件一起排掉，等于把上面那张"卡在等确认"的卡片又抹掉一次。
  assert.match(replay, /return "\(" \+ alias \+ "\.type='system' and \(" \+ strings\.Join\(conditions, " or "\) \+ "\)\)"/);
});

test("mobile refresh actually re-fetches and reports the outcome", () => {
  // 症状（用户报的）：点「刷新」没有任何动静——按钮不变、数据不变、时间戳也不变，
  // 完全看不出有没有生效。
  // 根因三层，缺一层都不算修好：
  //   ① 按钮只调 loadInstances + loadSnapshot，没有 loading、没有结果状态；
  //   ② loadSnapshot 带 revision 短路：内容没变时云端只回 {unchanged:true}，一次全量都不落。
  //      更糟的是本地快照可能落后于 snapshotRevisionRef（上一次请求失败后回落到 localStorage
  //      缓存），此时短路会把"界面正停在旧数据上"判成"已是最新"；
  //   ③ 头部那行时间戳取的是电脑端的 observedAt（云端 updated_at），不是本次拉取时刻，
  //      刷多少次都不会变，等于没有证据。（那一行属于电脑端的「项目与任务」列表，
  //      2026-09-17 已随方案 B 撤掉；这条历史依据保留，因为它解释的是状态条为什么必须带时刻。）
  // 修法：手动刷新强制取全量并回传结构化结果（updated / unchanged / failed / skipped），
  //      按钮忙碌时禁用 + 转圈，结果写成一条带时刻的状态条。
  const snapBody = page.match(/const loadSnapshot = useCallback\(async \(options\?: \{ force\?: boolean; instanceId\?: string \}\)[\s\S]*?\n  \}, \[instanceID\]\);/)?.[0] ?? "";
  // 锚点先自证存在：抠不到函数体时下面的断言会退化成对空串匹配，全部恒假/恒真。
  assert.ok(snapBody, "找不到 loadSnapshot（签名变了就更新本用例的锚点）");
  // ① force 必须真的绕过 revision：照旧拼 ?revision= 的话，用户点刷新还是会拿到
  //    {unchanged:true}，界面照样一动不动。
  assert.match(snapBody, /const query = force \? "" : `\?revision=\$\{baseline\}`;/);
  assert.match(snapBody, /snapshot\$\{query\}/);
  assert.doesNotMatch(snapBody, /\/snapshot\?revision=\$\{snapshotRevisionRef\.current\}/);
  // ② 结果要如实区分"有新内容"和"没有新内容"，判据是版本号有没有前进。
  assert.match(snapBody, /outcome: value\.snapshotRevision > baseline \? "updated" : "unchanged"/);
  // ③ 换实例时（请求的实例 ≠ 当前选中的实例）必须丢掉旧实例的版本基线：拿 B 的版本号比 A 的，
  //    守卫会不通过把 B 的快照整个丢掉，结果还把"什么都没应用"报成"已是最新"。
  assert.match(snapBody, /const baseline = options\?\.instanceId && options\.instanceId !== instanceID \? -1 : snapshotRevisionRef\.current;/);
  assert.match(snapBody, /value\.snapshotRevision >= baseline/);
  // ④ 失败不能因为"有缓存兜底"就报成功：界面停在旧快照上，必须说失败并给出原因。
  //    原因只写"无法连接云端"，"当前显示的是最近一次同步快照"交给页面错误条说一次，
  //    免得同一条提示连着出现两遍。
  assert.match(snapBody, /return \{ outcome: "failed", snapshot: cachedValue, reason: "无法连接云端" \};/);
  assert.match(snapBody, /setError\(reason\);/);
  // ⑤ 刷新可以指定实例：实例列表刚被重拉过，选中的电脑可能已经失效并被换掉。
  assert.match(snapBody, /const target = options\?\.instanceId \|\| instanceID;/);
  assert.match(snapBody, /encodeURIComponent\(target\)/);

  // 实例列表请求的结果必须三态分离（ok / failed / superseded）。列表每 5 秒被轮询拉一次，
  // 手动刷新那一次被轮询抢答是常态：把它当失败，就会在一切正常时弹"刷新失败：没有连上云端"。
  const instancesBody = page.match(/const loadInstances = useCallback\(async \(\): Promise<InstanceFetchResult> => \{[\s\S]*?\n  \}, \[\]\);/)?.[0] ?? "";
  assert.ok(instancesBody, "找不到 loadInstances（签名变了就更新本用例的锚点）");
  assert.match(page, /type InstanceFetchResult = \{ instances: Instance\[\]; outcome: "ok" \| "failed" \| "superseded"; reason\?: string \};/);
  assert.match(instancesBody, /const superseded: InstanceFetchResult = \{ instances: \[\], outcome: "superseded" \};/);
  assert.match(instancesBody, /return \{ instances: nextInstances, outcome: "ok" \};/);
  assert.match(instancesBody, /return \{ instances: \[\], outcome: "failed", reason \};/);
  // 失败原因要报给调用方（刷新按钮据此说清是网络不通还是令牌失效），不能再写死一句
  // "没有连上云端"，写死的原因在"云端返回异常"这类失败上是错的。
  assert.match(instancesBody, /const reason = cause instanceof TypeError/);
  assert.match(instancesBody, /setError\(reason\);/);
  assert.doesNotMatch(instancesBody, /return null/);
  // 三处"被更新的请求取代"都必须返回 superseded，不能退化成 null/失败。
  assert.ok((instancesBody.match(/return superseded;/g) || []).length >= 3, "三处 generation 过期都必须返回 superseded");

  // `saveToken` 删除后，`refreshNow` 后面紧跟的是那段"这里原来有一个 saveToken"的说明注释
  // （再往后才是 `enableMobileNotifications`），所以锚点收在函数结尾 + "后面跟着注释或函数定义"。
  const refreshBody = page.match(/async function refreshNow\(\) \{[\s\S]*?\n  \}\n\n  (?:\/\/|async function)/)?.[0] ?? "";
  assert.ok(refreshBody, "找不到 refreshNow");
  // 连点两次只发一次请求，也保证不会有两条反馈互相覆盖。
  assert.match(refreshBody, /if \(refreshingRef\.current\) return;/);
  assert.match(refreshBody, /setRefreshing\(true\);/);
  // 点击后立刻给出"正在刷新"的可见状态，而不是等结果回来才动。
  assert.match(refreshBody, /setRefreshStatus\(\{ state: "refreshing", message: "正在刷新…", at: null \}\);/);
  assert.match(refreshBody, /finally \{[\s\S]*setRefreshing\(false\);[\s\S]*\n  \}/);
  // 实例列表与快照都要重拉，且快照这一趟必须带 force + 刚算出来的目标实例。
  assert.match(refreshBody, /const list = await loadInstances\(\);/);
  assert.match(refreshBody, /const known = list\.outcome === "ok" \? list\.instances : null;/);
  assert.match(refreshBody, /known\.some\(\(item\) => item\.instanceId === instanceID\) \? instanceID : known\[0\]\?\.instanceId \|\| ""/);
  assert.match(refreshBody, /const result = await loadSnapshot\(\{ force: true, instanceId: target \}\);/);
  // 四种结果各有各的说法：成功报项目数、无变化说"已是最新"、失败带原因。
  assert.match(refreshBody, /state: "success", message: `已刷新 · \$\{result\.snapshot\?\.projects\.length \?\? 0\} 个项目`/);
  assert.match(refreshBody, /state: "unchanged", message: "已是最新，电脑端没有新的变化"/);
  assert.match(refreshBody, /state: "failed", message: `刷新失败：\$\{result\.reason \|\| "请稍后重试"\}`/);
  // 没有实例时必须说清楚是"没配对"还是"电脑不在线"，而不是照旧一声不响。
  assert.match(refreshBody, /message: token\.trim\(\) \? "没有找到在线的电脑，请确认电脑端已启动并联网" : "尚未配对电脑，请先扫码配对"/);
  // 只有"请求真的失败"才报连接失败；被轮询抢答（superseded）时既不能报成功也不能报失败。
  assert.match(refreshBody, /if \(list\.outcome === "failed"\) \{[\s\S]*?刷新失败：\$\{list\.reason \|\| "请稍后重试"\}[\s\S]*?\n      \}/);
  assert.match(refreshBody, /if \(list\.outcome === "superseded"\) \{[\s\S]*?state: "unchanged", message: "电脑列表已更新，正在同步数据…"[\s\S]*?\n        \}/);
  // 最后一道保险：意外抛出也要收尾，否则按钮会永远停在"正在刷新…"的转圈状态。
  assert.match(refreshBody, /\} catch \{[\s\S]*?setRefreshStatus\(\{ state: "failed", message: "刷新失败：请稍后重试", at: new Date\(\) \}\);/);

  // 按钮本身：连到 refreshNow、忙碌时禁用并标记 aria-busy（CSS 靠它转圈），
  // 旧的两行调用必须删干净——留着就等于保留了"点了没反应"的旧行为。
  assert.match(page, /<button className="mobile-refresh" type="button" onClick=\{\(\) => void refreshNow\(\)\} disabled=\{refreshing\} aria-busy=\{refreshing\} title="从电脑端重新同步">/);
  assert.doesNotMatch(page, /void loadInstances\(\); void loadSnapshot\(\);/);
  // 结果条：assistive 技术可读（role=status），并渲染本次拉取的时刻作为"生效"的证据。
  assert.match(page, /className=\{`mobile-refresh-status mobile-refresh-status-\$\{refreshStatus\.state\}`\} role="status"/);
  assert.match(page, /refreshStatus\.state === "refreshing" && <span className="mobile-refresh-spinner" aria-hidden="true" \/>/);
  assert.match(page, /<time dateTime=\{refreshStatus\.at\.toISOString\(\)\}>\{refreshStatus\.at\.toLocaleTimeString\(\)\}<\/time>/);

  // 样式：忙碌时图标旋转、结果条按状态配色（变体类名带 mobile-refresh- 前缀，
  // 裸词会命中 style.css 的全局组件样式）。
  assert.match(styles, /\.mobile-refresh \{ position: relative; display: inline-flex; min-height: 38px; align-items: center; justify-content: center; gap: 5px; border: 1px solid #cfe4da; border-radius: 999px; padding: 0 11px; color: #256a5b; background: #eaf4ef; font: inherit; font-size: 12px; font-weight: 700; \}/);
  assert.match(styles, /\.mobile-refresh\[aria-busy="true"\] \.mobile-refresh-icon \{ animation: mobile-refresh-spin \.9s linear infinite; \}/);
  assert.match(styles, /@keyframes mobile-refresh-spin \{ to \{ transform: rotate\(360deg\); \} \}/);
  assert.match(styles, /\.mobile-refresh-status \{ display: flex; align-items: center; gap: 8px; max-width: 860px;/);
  assert.match(styles, /\.mobile-refresh-status-success \{ border-color: #8fb9a5;/);
  assert.match(styles, /\.mobile-refresh-status-unchanged \{ border-color: #cfded6;/);
  assert.match(styles, /\.mobile-refresh-status-failed \{ border-color: #e5b8aa;/);
  // 头部三颗按钮收成 38px 的软胶囊（原来是 44px 描边方块，刷新还是实心 #2c7567）：
  //   · 尺寸：38px + ::after 外扩 = 44px 热区，看着小了、按着没小；
  //   · min-width: 76px 必须删掉——它是"实心块不随图标变宽"的产物，留着会把胶囊又撑宽；
  //   · .mobile-refresh 也不能再回到 44px 那一组（那组是给表单按钮的）。
  assert.match(styles, /\.mobile-back \{ position: relative; display: grid; place-items: center; width: 38px; height: 38px; flex: none; border: 0; border-radius: 999px; padding: 0; color: #256a5b; background: #eaf4ef; cursor: pointer; \}/);
  // 外扩量**按有没有 1px 边框**分两条写，不能合成一条：inset 的参照系是 padding box
  // （带边框时比 border box 小 1px），-3px 只能扩到 42。详见顶栏那条用例里的实测记录。
  assert.match(styles, /\.mobile-back::after \{ position: absolute; inset: -3px; content: ""; \}/);
  assert.match(styles, /\.mobile-refresh::after, \.mobile-notification-button::after, \.mobile-header-menu-button::after \{ position: absolute; inset: -4px; content: ""; \}/);
  assert.doesNotMatch(styles, /\.mobile-refresh \{ min-width: 76px; \}/);
  assert.doesNotMatch(styles, /\.mobile-refresh, \.mobile-pairing button/);
  assert.doesNotMatch(styles, /\.mobile-back \{ width: 44px; height: 44px; \}/);
  assert.match(styles, /\.mobile-refresh-icon \{ flex: none; \}/);
  // span 必须 nowrap：按钮是 inline-flex，默认可收缩，而会话视图里标题会来抢宽度
  // （实测 320px 时去掉 nowrap，胶囊被压到 54px，"刷新"溢出自己的盒子）。让位的是标题。
  // 不加 flex: none —— 实测它单独去掉/加上胶囊都是 66px，属于不承重的"保险"。
  assert.match(styles, /\.mobile-refresh span \{ white-space: nowrap; \}/);
  assert.doesNotMatch(styles, /\.mobile-refresh span \{[^}]*flex: none/);
  // 「开启通知」是纯文字 flex 项，min-content 默认只有一个汉字宽：没有 nowrap 时会被标题挤成
  // 一列竖排（实测 390px：38×66px、4 行，头部高一倍）。同样不加 flex: none。
  assert.match(styles, /\.mobile-notification-button \{[^}]*white-space: nowrap; \}/);
  assert.doesNotMatch(styles, /\.mobile-notification-button \{[^}]*flex: none;/);
  assert.match(page, /<svg className="mobile-refresh-icon" viewBox="0 0 24 24" width="13" height="13"/);
  // 返回键的箭头必须是 SVG，不能再是文本字形 "←"：字重/基线由各平台字体决定，
  // 和旁边同为 SVG 的刷新图标对不齐（Windows / Android / iOS 三处渲染都不一样）。
  assert.match(page, /className="mobile-back" type="button" onClick=\{goBack\}[^>]*><svg viewBox="0 0 24 24" width="18" height="18" aria-hidden="true" focusable="false" fill="none" stroke="currentColor" strokeWidth="2.2"/);
  assert.doesNotMatch(page, />←<\/button>/);
  // 忙碌态不能靠 opacity：`.mobile-refresh` 唯一的 disabled 场景就是"正在刷新"，而半透明的
  // 浅色胶囊在浅色页面上几乎消失——实测文字对比度掉到 2.06:1（原来实心块时代也是 2.1:1，
  // 只是看不出"胶囊不见了"）。忙碌态改由 aria-busy 换底色表达，保持全不透明（5.32:1）。
  // 那组半透明的 legacy 禁用态（`.mobile-create button:disabled` 等，0,1,1）在 2026-09-17
  // 随电脑端改版一起删了：它的三个选择器都已无宿主。**不要再加回来** —— 单类选择器一进
  // 优先级选择器所在的弹层，就会把 (0,2,0) 的那几条规则拖进"谁能赢"的不确定里。
  // ⚠️ 锚点必须是**整条规则**（含声明），不能只写 `\.mobile-create button` ——
  // 样式表里那段解释"为什么删掉它"的注释原样引用了这个选择器，
  // 按裸选择器断言会被自己的注释满足（TOOLING「断言前先剥注释」）。
  assert.doesNotMatch(styles, /\.mobile-create button:disabled, \.mobile-task-actions button:disabled \{/);
  assert.doesNotMatch(styles, /\.mobile-create button \{ width: fit-content/);
  assert.doesNotMatch(styles, /\.mobile-refresh:disabled,/);
  // 旧电脑端那张注册卡（`{!mobileApp && agentStatus && !agentStatus.ready && <section className="mobile-agent-enroll">`）
  // 2026-09-17 被 `desktop-remote-enroll` 取代，但**样式漏删了 8 条**（h2/p/form/input/button/small）。
  // 复查时把"CSS 选择器 ↔ JSX 类名"两个方向都比一遍才逮到 —— 这类孤儿不报错、没人会点到，
  // 只会一直躺在样式表里，让下一个人以为"旧注册界面还在"。
  // 锚点写**整条规则**而不是裸类名：将来若有人补一句"为什么删掉它"的注释，裸锚点会被注释满足。
  assert.doesNotMatch(styles, /\.mobile-agent-enroll \{/);
  assert.doesNotMatch(styles, /\.mobile-agent-enroll (?:h2|p|form|input|button|small)/);
  assert.match(styles, /\.mobile-refresh\[aria-busy="true"\] \{ background: #dcefe4; border-color: #bcdccb; \}/);
});

test("mobile header truncates a long project title instead of spilling over its actions", () => {
  // 刷新按钮就在头部这一行里。实测（320px 会话视图 + 16 字项目名，注入还原法）：
  //   · 承重的是 h1 上的 overflow: hidden——还原成 visible 后，nowrap 标题按整串文字撑开
  //     （可用宽 142px → 内容宽 360px），文字溢出行边界、被 .mobile-remote 的 overflow-x:
  //     hidden 静默裁掉，并压到右侧的通知/刷新按钮上。这就是"标题该截断"的那道闸。
  //   · 按钮本身不会被顶出视口：头部是 space-between，胶囊宽度由内容算（`flex: none` 的
  //     文字 + 固定内边距），实测右缘恒为 310px（320px 视口）——所以"给 h1 补 min-width: 0"
  //     与"给按钮组 flex: none"两条实测都不承重（逐像素无差异 / 只差 3px 且视觉盒仍有 38px），
  //     它们因此不在样式表里。不要再加回来当"保险"：无用规则配一句"此处必须如此"的
  //     注释比没有更误导人。
  //     （这两条结论在 2026-09-13 头部按钮收小后复测过：按钮尺寸从 76×44 变成 66×38，
  //       h1 可用宽 114 → 142px，但"谁在承重"完全没变。）
  assert.match(styles, /\.mobile-remote h1 \{[^}]*overflow: hidden;/);
  assert.match(styles, /\.mobile-remote h1 \{[^}]*text-overflow: ellipsis;/);
  assert.match(styles, /\.mobile-remote h1 \{[^}]*white-space: nowrap;/);
  assert.doesNotMatch(styles, /\.mobile-remote h1 \{ min-width: 0; \}/);
  assert.doesNotMatch(styles, /\.mobile-header-actions \{ flex: none; \}/);
  // 同一件事的第一道闸（标题组、品牌组）仍然保留。
  assert.match(styles, /\.mobile-remote-title \{ display: flex; align-items: center; gap: 10px; min-width: 0; \}/);
  assert.match(styles, /\.mobile-brand \{ display: flex; align-items: center; gap: 10px; min-width: 0; \}/);
});

test("mobile conversation header stays frozen while only the message list scrolls", () => {
  // 症状（用户报的）：会话页往下滑，返回 / 标题 / ⋯ 一起被推出屏幕。
  // 根因不在"没写 sticky"，而在 `.mobile-remote { overflow-x: hidden }`：
  //   css-overflow-3 规定一轴 hidden + 另一轴 visible ⇒ visible 计算成 auto，
  //   `.mobile-remote` 因此成了**滚动容器**；而它靠内容撑高、永远不会自己滚，
  //   sticky 的参照系于是落在了一个"不动的盒子"上 —— 顶栏等于没粘。
  //   实测（.tmp/probe-frozen-header.mjs，390×800，scrollY=600）：
  //     overflow-x: hidden → 顶栏 y = -586；clip → y = 0。
  // 断言两件事，缺一条就复发：
  //   ① 会话视图必须把 `.mobile-remote` 换成 `overflow-x: clip`（照旧裁横向溢出，
  //      但不再是滚动容器）；**选择器必须带 `.mobile-remote` 前缀** —— 单写
  //      `.mobile-conversation-mode` 与 `.mobile-remote` 同特异性，而后者在样式表里
  //      更靠后，会把它整条盖掉（这就是"改了却没生效"的写法）。
  //   ② 顶栏 sticky + 自己带安全区上边距：贴到 top: 0 之后，<main> 那条
  //      `14px + safe-area` 会留在顶栏**上方**，滚动内容从缝里露出来，标题还会钻进状态栏。
  assert.match(styles, /\.mobile-remote\.mobile-conversation-mode \{[^}]*overflow-x: clip;/);
  assert.match(styles, /\.mobile-remote\.mobile-conversation-mode \{[^}]*padding-top: 0;/);
  assert.match(styles, /\.mobile-conversation-mode \.mobile-remote-header \{[^}]*position: sticky;/);
  assert.match(styles, /\.mobile-conversation-mode \.mobile-remote-header \{[^}]*top: 0;/);
  assert.match(styles, /\.mobile-conversation-mode \.mobile-remote-header \{[^}]*padding-top: calc\(10px \+ env\(safe-area-inset-top\)\);/);
  // 顶栏要盖住从底下穿过去的消息（不透明底色），但不能压过任何一次性浮层：
  // 任务面板 20 / 弹层 30 / 取景层 60 都在它之上，输入条外壳 4 也应保持在上。
  assert.match(styles, /\.mobile-conversation-mode \.mobile-remote-header \{[^}]*z-index: 3;/);
  // 底色必须是**不透明**的页面底色：出图比过 .95/.88/.82 三档，.88 起标题底下就透出灰字；
  // 而在不透出残影的那一档上 blur 只剩 5% 的贡献 = 白付一次每帧背景采样，所以刻意不用
  // backdrop-filter。两条都钉死，别让它退回半透明 + 毛玻璃。
  assert.match(styles, /\.mobile-conversation-mode \.mobile-remote-header \{[^}]*background: #f3f8f4;/);
  assert.doesNotMatch(styles, /\.mobile-conversation-mode \.mobile-remote-header \{[^}]*backdrop-filter/);
  assert.doesNotMatch(styles, /\.mobile-conversation-mode \.mobile-remote-header \{[^}]*background: #f3f8f4[0-9a-f]{2};/);
  // 其余视图（项目列表）不改：`hidden` 仍然是那条"列宽三闸"的兜底。
  assert.match(styles, /\.mobile-remote \{ width: 100%; overflow-x: hidden;/);
  // 反例钉死：不要用裸选择器写这条覆盖（会被后面的 .mobile-remote 规则压掉）。
  assert.doesNotMatch(styles, /^\.mobile-conversation-mode \{[^}]*overflow-x:/m);
});

test("mobile refresh feedback stays visible now that the header never scrolls away", () => {
  // 顶栏冻结的**副作用**（复查时抓到的）：⋯ 菜单在会话的任何滚动位置都点得到了，
  // 而刷新状态条还留在文档流顶部 —— 实测（.tmp/probe-frozen-header-feedback.mjs，
  // 390×800、scrollY=2052）状态条落在 y = -1990，用户点完「刷新」在屏幕上什么都看不到。
  // 改法两条，缺一条都不成立：
  //   ① 状态条钉在顶栏正下方。高度不能写死 58px（含安全区，刘海机一变就对不上），
  //      由页面按顶栏实测高度写进 `--mobile-header-height`，与 `--mobile-composer-height`
  //      同一套做法。
  //   ② 它因此会一直占着顶栏折成一行才省下来的那 ~40px，所以**只在会话视图**让终态
  //      6 秒后自己收掉；"正在刷新…"（at 为 null）不收，那时还没有结果可看。
  //      项目列表页保持原样：那一页的刷新按钮本来就在文档顶部。
  assert.match(styles, /\.mobile-conversation-mode \.mobile-refresh-status \{[^}]*position: sticky;/);
  assert.match(styles, /\.mobile-conversation-mode \.mobile-refresh-status \{[^}]*top: var\(--mobile-header-height/);
  assert.match(page, /ref=\{headerRef\}/);
  assert.match(page, /document\.documentElement\.style\.setProperty\("--mobile-header-height"/);
  assert.match(page, /if \(mobileView !== "conversation"\) return;[\s\S]{0,120}?if \(!refreshStatus\?\.at\) return;/);
  assert.match(page, /window\.setTimeout\(\(\) => setRefreshStatus\(null\), REMOTE_REFRESH_STATUS_TTL_MS\)/);
  assert.match(page, /const REMOTE_REFRESH_STATUS_TTL_MS = \d+;/);
});

test("mobile scan clears the root element background so the native camera preview is visible", () => {
  // 症状（用户报的）：点「扫描二维码」后摄像头起不来，取景层上只有一片深色。
  // 根因：原生分支走 `BarcodeScanner.startScan()`，插件把 CameraX 的 PreviewView 插到
  //   WebView **下面**（BarcodeScanner.java: addView(previewView, 0)），所以画面能不能看见
  //   取决于 WebView 之上每一层 DOM 的不透明度 —— 而 style.css 里
  //   `:root { background: #f3f8f4 }` 给根元素铺了一层不透明画布底色，
  //   旧实现只清 body / #root / .mobile-remote（`.mobile-remote` 自己那条 background 也被清了），
  //   唯独没清 <html>，于是相机其实已经在跑、画面却被整块盖住。
  // 修法两件：① JS 把类名同时挂到 <html> 与 <body>；② 清底规则逐层点名 html / body /
  //   #root / .mobile-remote。注意：`body.barcode-scanner-active` 清的是 body 自己的底色，
  //   它跟 html 上的 `:root { background: #f3f8f4 }` 管不到同一个元素 —— 只挂 body 的话，
  //   根元素那层不透明画布底色永远清不掉（这跟特异性无关）。
  assert.match(page, /const barCodeScannerActiveClass = "barcode-scanner-active";/);
  assert.match(page, /function setScannerActive\(active: boolean\) \{\s*document\.documentElement\.classList\.toggle\(barCodeScannerActiveClass, active\);\s*document\.body\.classList\.toggle\(barCodeScannerActiveClass, active\);\s*\}/);
  assert.doesNotMatch(page, /document\.body\.classList\.add\("barcode-scanner-active"\)/);
  assert.doesNotMatch(page, /document\.body\.classList\.remove\("barcode-scanner-active"\)/);
  // 【归属】类名的增删只允许有 3 处调用：owner effect 的开/关 + closeScanOverlay 的提前一拍。
  // 写回平台分支的后果实测过（2026-09-14 复核）：扫码失败后 scanning 回 false、平台分支的清理
  // 把类名撤掉，而取景层仍留在屏幕上（scanError）—— 背后那屏于是恢复可见，键盘还能 Tab 到
  // 被盖住的按钮，模态就漏了。这条断言把"散回平台分支"挡在门外。
  const scannerActiveCalls = [...page.matchAll(/(?<!function )setScannerActive\(/g)].length;
  assert.equal(scannerActiveCalls, 3, `setScannerActive 只该有 3 处调用（owner effect 开/关 + closeScanOverlay 快路径），实际 ${scannerActiveCalls} 处`);
  // owner effect 以「取景层是否在屏幕上」为准（scanning || scanError），不是只看 scanning。
  assert.match(page, /const scanOverlayVisible = scanning \|\| Boolean\(scanError\);/);
  assert.match(page, /useEffect\(\(\) => \{\s*setScannerActive\(scanOverlayVisible\);\s*return \(\) => setScannerActive\(false\);\s*\}, \[scanOverlayVisible\]\);/);
  assert.match(page, /function closeScanOverlay\(\) \{\s*setScannerActive\(false\);\s*setScanning\(false\);\s*setScanError\(""\);\s*\}/);
  // 取景层与 effect 必须读同一个派生值，别一处 scanning||scanError、一处只写 scanning。
  assert.match(page, /\{\(scanOverlayVisible\) && <div className="mobile-scan"/);
  // 扫到码的成功路径也要走同一个出口（否则"提前一拍撤类名"只在取消那条路上生效）。
  assert.match(page, /setPairingCode\(scanned\.code \|\| ""\);[\s\S]{0,320}?closeScanOverlay\(\);/);
  // 清底要**逐层点名**，漏一层等于没改：`:root` / `body`（style.css 都给了底色）、
  // `#root`、以及 `.mobile-remote` 自己那条 #f3f8f4。`.mobile-remote` 就漏过一次，
  // 靠像素探针的仿真抽样当场抓到（取景窗里仍是页面底色）。
  assert.match(styles, /html\.barcode-scanner-active,\s*html\.barcode-scanner-active body,\s*html\.barcode-scanner-active #root,\s*html\.barcode-scanner-active \.mobile-remote \{ background: transparent; \}/);
  // 反面锚点：清底段落里不能再出现"只针对 body"的写法 —— 那一版就是被 :root 压住才失效的。
  assert.doesNotMatch(styles, /body\.barcode-scanner-active, body\.barcode-scanner-active #root/);
  // 顺便钉住"为什么非清 html 不可"这条前提：style.css 确实给根元素上了不透明底色。
  assert.match(rootStyles, /:root \{[^}]*background: #f3f8f4;/);
});

test("mobile scan overlay is a direct child of the page and keeps its backdrop see-through", () => {
  // 取景层不能再嵌在配对面板里：清底规则是 `.mobile-remote > *:not(.mobile-scan)`，
  // 嵌进去就会连取景窗一起被 visibility: hidden —— 旧实现正是嵌在 .mobile-pairing 内部，
  // 靠两条补丁规则（`.mobile-pairing > :not(.mobile-pairing-scanner-shell)`）才勉强透出来。
  assert.doesNotMatch(page, /mobile-pairing-scanner/);
  assert.doesNotMatch(styles, /mobile-pairing-scanner/);
    assert.match(page, /\{!nativeScanPlatform && <video className="mobile-scan-video" ref=\{setScanVideo\} muted playsInline autoPlay \/>\}/);
  assert.match(styles, /html\.barcode-scanner-active \.mobile-remote > \*:not\(\.mobile-scan\) \{ visibility: hidden; \}/);
  // 被隐藏的区块仍占布局，不锁根元素的话扫码时还能滚到下面那片空页。
  assert.match(styles, /html\.barcode-scanner-active, html\.barcode-scanner-active body \{ overflow: hidden; overscroll-behavior: none; \}/);
  // 取景窗必须是"挖空 + 超大阴影"，不是铺满屏幕的深色块：铺满就会把摄像头一起盖掉。
  assert.match(styles, /\.mobile-scan-window \{[^}]*box-shadow: 0 0 0 100vmax rgba\(6, 17, 14, \.72\); \}/);
  assert.doesNotMatch(styles, /\.mobile-scan \{[^}]*background:/);
  // Web 分支没有"WebView 下面的摄像头"，由这一层自己铺底；原生分支必须保持透明。
  assert.match(styles, /\.mobile-scan\[data-backdrop="solid"\] \{ background: #0a1714; \}/);
  // 遮罩不能挡交互，底部按钮自己抬上来。
  assert.match(styles, /\.mobile-scan-mask \{[^}]*pointer-events: none; \}/);
  // 吸底由「会伸缩的遮罩」负责：取景窗在文案与按钮之间的空当里居中，
  // 而不是按整屏居中 —— 矮视口（横屏 844×390）下整屏居中会同时压住上下两段（实测过）。
  assert.match(styles, /\.mobile-scan-mask \{[^}]*flex: 1; min-height: 0;[^}]*\}/);
  // DOM 顺序＝视觉顺序：遮罩是那个会伸缩的段，必须待在文案与按钮之间，
  // 放在文案前面会把标题挤到取景窗下面（改布局时真踩过）。
  assert.ok(page.indexOf('className="mobile-scan-text"') < page.indexOf('className="mobile-scan-mask"')
    && page.indexOf('className="mobile-scan-mask"') < page.indexOf('className="mobile-scan-footer"'),
    "取景层 DOM 顺序必须是 文案 → 遮罩 → 底部按钮");
  // 三者都是定位元素，光靠 DOM 顺序会让后画的遮罩把文案压暗 → 文案与按钮抬到 z-index: 1。
  assert.match(styles, /\.mobile-scan-text \{[^}]*z-index: 1;/);
  assert.match(styles, /\.mobile-scan-footer \{[^}]*z-index: 1;/);

  assert.match(styles, /\.mobile-scan-window \{[^}]*height: min\(68vw, 260px\); max-height: 100%; aspect-ratio: 1;/);
  assert.doesNotMatch(styles, /\.mobile-scan-window \{[^}]*width: min\(68vw, 260px\)/);
  assert.doesNotMatch(styles, /\.mobile-scan-footer \{[^}]*margin-top: auto/);
  // 画面要有负层：遮罩改成正常流之后，绝对定位的 <video> 会反过来压住遮罩。
  assert.match(styles, /\.mobile-scan-video \{[^}]*z-index: -1;/);
});

test("mobile scan failures surface inside the overlay instead of behind it", () => {
  // 旧实现的报错挂在配对面板里，而扫码期间配对面板整体 visibility: hidden，
  // 于是"扫到的不是 Milevia 配对二维码"这类提示从来没被用户看见过。
  assert.match(page, /<p className="mobile-scan-error" role="alert">/);
  assert.match(styles, /\.mobile-scan-error \{[^}]*background: rgba\(58, 18, 14, \.8\);/);
  // 权限被永久拒绝（系统不再弹窗）时唯一的出路是系统设置，必须给按钮；
  // openSettings() 在 Web 上是"未实现"异常，所以要 catch 出可读的中文指引。
  assert.match(page, /scanNeedsSettings && <button className="mobile-scan-settings"/);
  assert.match(page, /await BarcodeScanner\.openSettings\(\);/);
  assert.match(page, /if \(!cancelled\) setScanNeedsSettings\(true\);/);
  // 手电筒是可选能力：拿不到可用性就永远不渲染那颗按钮，让它不出现在"点了没反应"的状态里。
  assert.match(page, /\{scanning && scanTorchAvailable && <button className="mobile-scan-torch"/);
  assert.match(page, /aria-pressed=\{scanTorchOn\}/);
  assert.match(page, /void BarcodeScanner\.isTorchAvailable\(\)/);
  assert.match(styles, /\.mobile-scan-torch\[aria-pressed="true"\]/);
  // 失败态的取景窗里没有画面，空着会被读成「卡住了」——补一句说明，扫描中则只有扫描线。
  assert.match(page, /\{scanning \? <i className="mobile-scan-laser" \/> : <span className="mobile-scan-window-note">相机未开启<\/span>\}/);
  assert.match(styles, /\.mobile-scan-window-note \{[^}]*place-items: center;/);
});

test("mobile scan always resets its per-run state and is dismissed by both teardown paths", () => {
  // 五次复位集中在 beginPairingScan 一处：漏复位 scanAccepted 的后果最隐蔽 ——
  // 相机起来后会对任何一张二维码都判"已经接受过"，看起来像扫码没反应。
  assert.match(page, /function beginPairingScan\(\) \{\s*scanAccepted\.current = false;\s*setScanError\(""\);\s*setScanNeedsSettings\(false\);\s*setScanTorchAvailable\(false\);\s*setScanTorchOn\(false\);\s*setScanning\(true\);\s*\}/);
  // 「扫描二维码」把 `beginPairingScan` 作为唯一入口。它现在是配对页上的主按钮
  // （整页流程的第 1 步），不再是内联面板里那颗 `.mobile-pairing-generate`。
  assert.match(page, /<button className="mobile-pairing-scan-button" type="button" onClick=\{beginPairingScan\} disabled=\{busy \|\| scanning\}>扫描二维码<\/button>/);
  assert.doesNotMatch(page, /onClick=\{\(\) => \{ scanAccepted\.current = false;/);
  // 浮层清退两处都要收：① 安卓返回键的 if 链；② leaveConversationView（popstate / 侧滑不走 ①）。
  // 漏 ② 的症状：带着一个 position: fixed 的取景层退回项目列表，摄像头还一直开着。
  assert.match(page, /if \(scanning \|\| scanError\) \{ closeScanOverlay\(\); return true; \}/);
  // 失败态必须留在取景层里（scanning 已经是 false，只按 scanning 渲染就会"闪一下消失"）。
  assert.match(page, /<strong>\{scanning \? "将电脑上的二维码放入框内" : "扫码没有成功"\}<\/strong>/);
  assert.match(page, /<button className="mobile-scan-retry" type="button" onClick=\{beginPairingScan\}>重新扫描<\/button>/);
  assert.match(styles, /\.mobile-scan-retry \{[^}]*background: #8ce7b0;/);
  // ⚠️ 锚点只要求"这一句在 leaveConversationView 里"，**不要**写成 `closeScanOverlay\(\);\s*\}`：
  // 那个 `}` 会把这条断言钉死在"它必须是最后一条语句"上，之后往同一个函数里加一句
  // （2026-09-20 加的 `setPairingExpanded(false)`）就会报一条跟取景层毫无关系的假红。
  assert.match(page, /function leaveConversationView\(\) \{[\s\S]*?closeScanOverlay\(\);/);
  // 同理**不要**写成 `setPairingExpanded\(false\);\s*\}` —— `\s*\}` 等于断言"它必须是最后一条
  // 语句"，之后每往同一个函数里加一句都会报一条与配对页毫无关系的假红。
  // 这条锚点自己就踩过一次（2026-09-20 加 setPairingExpanded 时），2026-09-21 加文件面板时又踩了一次。
  // 改成"函数体里必须出现这一句"；真要守顺序，就单独写一条顺序断言（见下面那条）。
  assert.match(page, /function leaveConversationView\(\) \{[\s\S]*?setPairingExpanded\(false\);/);
  // 文件面板也是"退出会话视图时必须一起收掉"的一层：它是会话视图的子态，
  // 漏掉的话回到项目列表后状态还留着，而清理 effect 只按 selectedProject/selectedConversation
  // 变化触发 —— 下次进**同一个**项目它会自己冒出来。
  assert.match(page, /function leaveConversationView\(\) \{[\s\S]*?setSessionSub\(null\);/);
});


test("mobile scan torch reads back the real state instead of trusting the call", () => {
  // 症状：界面显示"手电筒已打开"，实际灯是黑的。
  // 根因：插件的 `enableTorch()` / `disableTorch()` 在相机尚未就绪时（`camera == null`，
  //   见 BarcodeScanner.java）是**静默 return** 的 —— 不抛异常、正常 resolve，但什么也没做。
  //   原来按"调用没抛就算成功"去 `setScanTorchOn(next)`，状态就与硬件脱钩了。
  // 修法：`toggleTorch()` 发声，再用 `isTorchEnabled()` **回读**真实状态（回读值由插件在
  //   每次真正生效的操作后维护，静默失败时不会变），以回读值为准。
  assert.match(page, /await BarcodeScanner\.toggleTorch\(\);/);
  assert.match(page, /const \{ enabled \} = await BarcodeScanner\.isTorchEnabled\(\);/);
  assert.match(page, /if \(Boolean\(enabled\) === scanTorchOn\) \{/);
  // 反面锚点：不许再用"调用成功即等于目标状态"的写法（回读与切换前相同 ⇒ 这次切换没生效，
  // 此时收起按钮，跟"设备没有闪光灯"一样处理）。
  assert.doesNotMatch(page, /await BarcodeScanner\.enableTorch\(\)/);
  assert.doesNotMatch(page, /await BarcodeScanner\.disableTorch\(\)/);
  assert.doesNotMatch(page, /setScanTorchOn\(next\)/);
});

test("mobile scan keeps one teardown path and tags which phase the overlay is in", () => {
  // 【为什么那条兜底调用不能删】清理函数会在 `cancelled = true` 之后立刻调 `stop()`，
  // 而那时 `startScan` 可能还没 resolve（`started` 仍是 false）→ `stop()` 里
  // `if (started) stopScan()` 整支跳过，相机没被停；随后 `startScan` 才 resolve。
  // 若不在那里补一次，listener 已被摘掉、超时也没设，用户看到的是"浮层关了但摄像头一直开着"。
  // 这条兜底必须走同一个 `stop()`（摘 listener / 关灯 / 复位状态只在一个地方）。
  assert.match(page, /if \(cancelled\) \{\s*await stop\(\);\s*return;\s*\}/);
  assert.doesNotMatch(page, /if \(cancelled\) \{\s*await BarcodeScanner\.stopScan\(\)/);
  // stop 里连手电筒可用性一起复位，否则下次进来会拿着上一次的可用性渲染按钮。
  assert.match(page, /if \(started\) await BarcodeScanner\.stopScan\(\)\.catch\(\(\) => undefined\);\s*setScanTorchAvailable\(false\);\s*setScanTorchOn\(false\);\s*\};/);
  // 插件的 stopScan() 内部第一件事就是 disableTorch()，所以这里不需要（也不该）手写关灯 —— 钉住这个前提。
  assert.doesNotMatch(page, /stopScan[\s\S]{0,200}disableTorch/);

  // data-phase 是给"矮视口 + 失败态"用的钩子：失败态多一张报错卡，会把取景窗挤小
  // （实测 844×390 → 222px、720×200 → 76px），而那时窗口里本来就没有画面。
  assert.match(page, /data-phase=\{scanning \? "live" : "error"\}/);
  assert.match(styles, /@media \(max-height: 340px\) \{\s*\.mobile-scan\[data-phase="error"\] \{ justify-content: center; \}\s*\.mobile-scan\[data-phase="error"\] \.mobile-scan-mask \{ display: none; \}\s*\}/);
  // 两级矮视口降级：先收说明小字（标题留着），再连标题一起收。
  // 阈值 340 有意压在常见横屏尺寸（812×375 / 844×390）之下 —— 那两档窗口有 240~255px、
  // 本来就够用，不该为多 5px 丢掉引导标题。
  assert.match(styles, /@media \(max-height: 560px\) \{ \.mobile-scan-text small \{ display: none; \} \}/);
  assert.match(styles, /@media \(max-height: 340px\) \{\s*\.mobile-scan-text \{ display: none; \}\s*\.mobile-scan \{ padding: 10px 16px;/);
});

test("mobile conversation empty state is a card with a next step, not a bare gray line", () => {
  // 2026-09-17（用户报的）：空会话页原来只有消息列里一行 12px 灰字（`.mobile-empty`）——
  // 不说是哪个 Agent、不说下一步能干什么，而「新建会话」的唯一入口还藏在顶栏 ⋯ 里，
  // 新项目点进来就是个死胡同。现在做成白卡：圆形图标 + 标题 + 说明 + 可点起手。
  // 几何与点下去填进输入框这类行为由 .tmp/probe-mobile-empty-state.mjs 在真浏览器里验，
  // 这里只钉住结构还在不在——探针不在 CI 里跑，源码锚点是自动化那一侧的兜底。
  assert.doesNotMatch(page, /: <p className="mobile-empty">/);
  assert.match(page, /\) : conversationEmpty\}\{conversationProcessing/);
  // 那句旧文案（「该会话暂无对话内容。」）**不做全文 grep**：它作为"为什么要改"写在空态那段
  // 注释里，源码里必然出现，grep 它只会永远失败。要钉的是"它不再被渲染"——旧版的标记就是
  // 消息列里那个 `<p className="mobile-empty">`，上面那条已经覆盖了。
  assert.match(page, /data-kind="no-message"/);
  assert.match(page, /data-kind="no-conversation"/);
  assert.match(page, /className="mobile-empty-start-primary"/);
  // Agent 正在跑的时候**一个空态都不渲染**：这时说"还没有消息"是自我打脸，
  // 下面那条 .mobile-agent-processing 状态条才是唯一该出现的东西。
  assert.match(page, /\) : conversationProcessing \? null : \(/);
  // 起手胶囊只收「点下去只把内容填进输入框」的提示词：run / confirm 那一类会在电脑端
  // **真执行命令**，摆在空态上误触代价太高（要看全部就走卡底的「全部工具」）。
  assert.match(page, /promptShortcuts\.filter\(\(item\) => shortcutAction\(item\) === "fill"\)\.slice\(0, 3\)/);
  // data-empty 只由"真的上屏"决定：只要这个节点存在就加，会让**有消息的会话**也被套上
  // "撑满一屏并居中"的 min-height（真浏览器探针抓到过）。
  // 而且必须**同时**要求会话视图真的开着 —— 类名加在整页共用的 `<main>` 上，
  // 而"会话为空"这件事在项目列表页同样成立，那里根本没有消息列（探针有一条专门守它）。
  assert.match(page, /const conversationViewActive = Boolean\(mobileApp && mobileView === "conversation" && project\);/);
  assert.match(page, /const showConversationEmpty = conversationViewActive && conversationTimeline\.length === 0 && conversationEmpty !== null;/);
  assert.match(page, /data-empty=\{showConversationEmpty \? "true" : undefined\}/);
  // 样式侧：卡片语言与任务队列空态同源（16 圆角 + 白底 + 52px 圆形图标 #2c7567 on #e5f4ea）。
  assert.match(styles, /\.mobile-empty-start \{[^}]*border-radius: 16px;[^}]*background: #fff;/s);
  assert.match(styles, /\.mobile-empty-start-mark \{[^}]*width: 52px;[^}]*height: 52px;[^}]*background: #e5f4ea;/s);
  // ⚠️ **列宽必须夹住**（与 `.mobile-message-list` / `.mobile-task-list` 同一个坑）：
  // 卡片是 grid，不写 grid-template-columns 就得到 `auto` 隐式轨道，而 `auto` 的最小尺寸是
  // min-content —— 一颗 `white-space: nowrap` 的长胶囊把它顶到 695px（卡片本身只有 300px），
  // 于是所有百分比约束都解析在一个 695 的坐标系里、全部失效（真浏览器实测顶穿 415px）。
  assert.match(styles, /\.mobile-empty-start \{ display: grid; grid-template-columns: minmax\(0, 1fr\);/);
  assert.doesNotMatch(styles, /\.mobile-empty-start \{ display: grid; justify-items/);
  // 第二道闸：子元素撑满轨道，百分比宽度才有确定的参照系。
  assert.match(styles, /\.mobile-empty-start > strong \{ justify-self: stretch;/);
  assert.match(styles, /\.mobile-empty-start > p \{ justify-self: stretch;/);
  assert.match(styles, /\.mobile-empty-starters \{ display: flex; flex-wrap: wrap; justify-content: center; gap: 8px; width: 100%; margin-top: 16px; \}/);
  // 触控高度与「＋」面板里的同类胶囊同一档（≥44）：面板里那三组胶囊当初就是为这条被抬到 44 的，
  // 空态里再出现一排 40px 的"矮一截"胶囊，正是那种说不出原因、一眼不对的错。
  assert.match(styles, /\.mobile-empty-starters button \{[^}]*min-width: 0;[^}]*max-width: 100%;[^}]*min-height: 44px;/s);
  // 省略号**必须挂在按钮里的 span 上**：Chromium 的 `<button>` 把内容包成匿名 flex 项，
  // 写在按钮自己的 `text-overflow: ellipsis` 对匿名项不生效（长名字会被硬切）。
  assert.match(page, /<span>\{shortcutBusy === item\.id \? "填入中…" : item\.name\}<\/span>/);
  assert.match(styles, /\.mobile-empty-starters button > span \{ overflow: hidden; min-width: 0; text-overflow: ellipsis; white-space: nowrap; \}/);
  // 对比度是**算出来的**：`.mobile-empty-start-note` 最初抄了 `.mobile-section-heading span` 的
  // #82968d（白底 3.14:1），12px 正文档过不了 4.5 的闸门。真浏览器里也有一条算对比度的断言。
  assert.match(styles, /\.mobile-empty-start-note \{ justify-self: stretch;[^}]*color: #5f7a6e;/s);
  assert.doesNotMatch(styles, /\.mobile-empty-start-note \{[^}]*#82968d/s);
  // 居中靠"空态下把会话视图变成一根撑满视口的弹性列"，**不是**"减 N px"的算术 ——
  // 后者要把顶栏、输入条、页面 padding、会话块 margin 逐个扣掉，少减一个就凭空多出一条
  // 页面滚动（第一版栽了两回：会话块的 14px 下边距、刷新状态条出现后多出来的那一条）。
  // 弹性列对这几种"上下文高度变化"天然免疫：顶栏 / 状态条按内容占位，消息列吃掉剩下的。
  assert.match(page, /mobile-empty-mode/);
  assert.match(styles, /\.mobile-remote\.mobile-conversation-mode\.mobile-empty-mode \{ display: flex; flex-direction: column; padding-bottom: var\(--mobile-composer-height, 68px\); \}/);
  assert.match(styles, /\.mobile-remote\.mobile-conversation-mode\.mobile-empty-mode \.mobile-conversation \{ display: flex; flex: 1 0 auto; flex-direction: column; margin-bottom: 0; \}/);
  // ⚠️ 顶栏（以及刷新状态条 / 错误条 / 更新条 / 会话块）都是 `max-width: 860px; margin: 0 auto`，
  // 而**flex 项在交叉轴上的 auto 外边距会取消 `align-self: stretch`** —— 宽度退回 fit-content。
  // 长项目名把顶栏撑到 492px（⋯ 被裁出视口、点不到）；短项目名又把它缩到 124.7px、⋯ 飘到屏幕中间。
  // 杠杆必须是 `width: 100%`：`max-width: 100%` 只封顶、**不给出宽度来源**（短内容时照样 fit-content），
  // `min-width: 0` 实测无效，`margin-inline: 0` 会毁掉 >860px 宽屏下与内容列的对齐。
  assert.match(styles, /\.mobile-remote\.mobile-conversation-mode\.mobile-empty-mode > \* \{ width: 100%; \}/);
  assert.doesNotMatch(styles, /mobile-empty-mode > \* \{ max-width: 100%/);
  assert.doesNotMatch(styles, /mobile-empty-mode > \* \{ min-width: 0/);
  // `[data-empty]` 的 (0,2,0) 压过后面那条 .mobile-message-list { min-height: 120px }。
  assert.match(styles, /\.mobile-message-list\[data-empty="true"\] \{ flex: 1 1 auto; align-content: center; \}/);
  // 工具面板展开时卡片要收成「标题 + 一句说明」：面板自己就把起手胶囊与「全部工具」整张列全了，
  // 而 400px 的面板 + 输入条占掉 476px，配 323px 的整卡在 844px 的屏上放不下 ——
  // 卡片会被面板盖住一截、还得手动往下滚（探针实测重叠 49.3px、可滚 69px）。
  assert.match(page, /data-tools=\{composerToolsOpen \? "open" : "closed"\}/);
  assert.match(styles, /\.mobile-empty-start\[data-tools="open"\] \.mobile-empty-start-mark,\s*\.mobile-empty-start\[data-tools="open"\] \.mobile-empty-starters,\s*\.mobile-empty-start\[data-tools="open"\] \.mobile-empty-tools \{ display: none; \}/);
  // `flex-shrink: 0` 是承重的一条：空间不够时（展开工具面板：400px 面板 + 295px 空态卡
  // 放不进 844px 的屏）宁可让文档长出去、页面滚一下，也不能把会话块压到比内容还矮 ——
  // 那会让卡片从盒子里溢出来、被面板直接盖住。变异检验里"改成 flex: 1"当场被抓住。
  assert.doesNotMatch(styles, /mobile-empty-mode \.mobile-conversation \{ display: flex; flex: 1; flex-direction:/);
  // 特异性必须写到 (0,3,0)：`.mobile-remote.mobile-conversation-mode` 那条 padding-bottom
  // 在文件里排得更靠后，同级会被它盖掉（空态卡就会整体偏上 8px）。
  assert.doesNotMatch(styles, /\n\.mobile-empty-mode \{[^}]*padding-bottom/s);

  // 兜底：输入框的 placeholder 是另一处独立的「请先新建会话」，它不归这条用例管，
  // 但顺手钉住——真要一起改的时候得先看见它。
  assert.match(page, /placeholder=\{conversation \? "输入消息\.\.\." : "请先新建会话"\}/);
});

// ── 电脑端「远程控制」页（2026-09-17 方案 B：两栏工作台）───────────────────────
// 这一屏的教训是"同一个组件服务两端，要按两端各自的数据源审"：它没有云端令牌，拿不到快照、
// 也发不出命令，却照旧渲染了消费快照的区块 —— 统计卡恒 0/0、旁边还挂着一句永远转不完的
// 「加载中」；实例状态卡的条件里那个 `instance` 只从 /v1/instances 来，**真机上从未渲染过**；
// 点「刷新」报的是手机那一侧的动作（"请先扫码配对"）。下面的断言分四类：
//   ① 该删的删了（不许有第二个数据源）；② 该显示的显示了（服务状态 / 绑的是谁）；
//   ③ "还没回来"与"没有"分开渲染；④ 骨架与同应用其它管理页同源，且窄窗口有降级。
test("desktop remote control page only shows what this machine can actually know", () => {
  // ── ① 拿不到数据的区块不许回来 ────────────────────────────────────────
  assert.doesNotMatch(page, /className="mobile-summary"/);
  assert.doesNotMatch(page, /className="mobile-projects"/);
  assert.doesNotMatch(page, /className="mobile-create"/);
  assert.doesNotMatch(page, /className="mobile-instance-status"/);
  // 桌面端也不再复用手机页顶栏（30px 品牌名 + 圆形返回胶囊 + 刷新胶囊）；手机端那一支原样保留。
  assert.match(page, /\{mobileApp \? <header className="mobile-remote-header"/);
  assert.match(page, /<header className="desktop-remote-header">/);
  assert.match(page, /\{!mobileApp && <div className="desktop-remote-body">/);

  // ── ② 电脑端的「刷新」必须走自己那条链路 ──────────────────────────────
  // refreshNow 会去拉 /v1/instances 与云端快照，而电脑端没有令牌 ⇒ 必然走到
  // `token ? … : "尚未配对电脑，请先扫码配对"`。电脑端自己就是被配对的那台机器，
  // 照着这句话做只会更糊涂（审计 A3 的实测文案）。
  const desktopRefresh = page.match(/async function refreshDesktopStatus\(\) \{[\s\S]*?\n  \}\n/)?.[0] ?? "";
  assert.ok(desktopRefresh, "找不到 refreshDesktopStatus（签名变了就更新本用例的锚点）");
  assert.match(desktopRefresh, /await loadAgentStatus\(\)/);
  assert.match(desktopRefresh, /await loadBindings\(\)/);
  assert.doesNotMatch(desktopRefresh, /loadInstances|loadSnapshot/);
  // 两颗刷新按钮各连各的：桌面页头那颗连 refreshDesktopStatus，手机那颗仍连 refreshNow。
  assert.match(page, /onClick=\{\(\) => void refreshDesktopStatus\(\)\}/);
  assert.match(page, /onClick=\{\(\) => void refreshNow\(\)\}/);

  // ── ③ 服务状态：`ready` 只说明凭据在不在，必须再有一路"还活着吗" ────────
  // 心跳时刻由 control-server 在 /api/remote/overview 里记（Agent 每 750ms 打一次），
  // 心跳停了它就不再前进 —— 这一档（"收不到心跳"）旧版完全没有。
  // ⚠️ 心跳**只在 Agent 连着云端时才跳**（那条 ticker 长在 agent.runConnection 里），
  // 所以它同时是"没在跑"和"连不上云端"两种原因的表现，文案不许只挑一种说。见 ⑫。
  assert.match(page, /lastAgentHeartbeatAt/);
  // ⚠️ 五档的**判定与文案**不在这里断言：它们被抽到 `features/remote/desktop-service.ts`，
  // 由 `desktop-service.test.ts` 用**行为断言**守着。理由（2026-09-17 变异检验实测）：
  // 把 `isAgentAlive(...)` 换成 `true` 会让"无心跳"那一档永远不可达，
  // 而五句文案全都还在源码里 —— 扫源码的断言照样绿。页面这一侧只守**接线**：
  // 拿到心跳年龄、按模块给的结论渲染、并且有一个让它自己随时间重算的定时器。
  assert.match(page, /const desktopService = desktopServiceView\(agentStatusState, agentStatus, heartbeatAgeMs\);/);
  assert.match(page, /const timer = window\.setInterval\(measure, 5000\);/);
  assert.match(page, /data-state=\{desktopService\.state\}/);
  // 云端地址与实例 ID 服务端一直在返回（`cloudUrl` / `instanceId`），旧前端只读了 `ready`
  // ⇒ 就绪态零信息。三项都要落在界面上。
  assert.match(page, /cloudUrl: value\?\.cloudUrl \|\| ""/);
  assert.match(page, /<dt>云端<\/dt><dd>\{agentStatus\?\.cloudUrl \? <code>\{agentStatus\.cloudUrl\}<\/code> : "—"\}<\/dd>/);
  assert.match(page, /<dt>实例 ID<\/dt>/);
  assert.match(page, /<dt>最近心跳<\/dt>/);
  // 实例 ID 很长，界面上只给缩写（全文放 title），别把卡片撑破。
  assert.match(page, /<code title=\{desktopInstanceID\}>/);

  // ── ④ "没有内容"的四种真相必须分开渲染 ────────────────────────────────
  // 旧版把它们压成一个 bindingsReady，失败时整块**静默消失** —— 用户分不清
  // "没有手机绑定"和"没读到"（项目规则：「还没回来」绝不能写成「没有数据」）。
  assert.match(page, /useState<"loading" \| "loaded" \| "unregistered" \| "failed">\("loading"\)/);
  for (const branch of ['bindingsState === "loading"', 'bindingsState === "failed"', 'bindingsState === "unregistered"', "boundPhone ?"]) {
    assert.ok(page.includes(branch), `绑定卡缺一条分支：${branch}`);
  }
  assert.match(page, /setBindingsState\(value\?\.ready === false \? "unregistered" : "loaded"\)/);
  // 服务状态同理：读不到 ≠ 未注册。
  assert.match(page, /useState<"loading" \| "loaded" \| "failed">\("loading"\)/);
  assert.match(page, /setAgentStatusState\("failed"\)/);

  // ── ⑤ 未就绪时主栏换成注册引导（**但只在"读到了、确实未注册"时**）────────
  // 旧版把注册卡与配对面板同时摆出来，"生成二维码"照旧可点、点了必然 503（审计 A6）。
  //
  // ⚠️ 这里是 2026-09-17 复查抓到的第二个"两个真相同屏"：`!agentStatus?.ready` 在
  // `agentStatus === null`（还没读到 / 读失败）时**同样是 true**，于是冷启动那几秒
  // 主栏会摆出"请粘贴部署注册令牌"的表单，而页头与侧栏同一时刻写着"读不到本机服务状态"。
  // 判据必须写成"读到了、而且确实未注册"，第三种态另有一条分支（下一条断言）。
  assert.match(page, /const desktopNeedsEnroll = agentStatusState === "loaded" && \(!agentStatus\?\.ready \|\| reenrollOpen\);/);
  assert.match(page, /\{desktopNeedsEnroll \? <section className="desktop-remote-card amber"/);
  // 第三种态：还没读取到时**不许摆注册表单**，读失败时给一颗重试（重连的唯一动作）。
  assert.match(page, /: agentStatusState === "loaded" \? <section className="desktop-remote-card" aria-labelledby="desktop-remote-pairing-title">/);
  assert.match(page, /: <section className="desktop-remote-card" aria-labelledby="desktop-remote-pending-title">/);
  assert.match(page, /\{agentStatusState === "loading" \? "正在读取服务状态" : "读不到本机服务状态"\}/);
  assert.match(page, /onClick=\{\(\) => void refreshDesktopStatus\(\)\} disabled=\{refreshing\}>重试</);
  // 「重新注册」只把主栏切回注册态：表单**只有一处**，侧栏不许再抄一份。
  assert.equal(page.split('className="desktop-remote-enroll"').length - 1, 1, "注册表单应当只有一处");

  // ── ⑥ 配对流程保留下来的两条产品面硬约束 ──────────────────────────────
  // 换绑警告必须点名会断开谁（顶替语义要在界面上说出来），确认按钮要自己说清在等什么。
  assert.match(page, /确认绑定新的手机后，<b>\{boundPhone\.deviceName/);
  assert.match(page, /pairingConfirmed \? "已绑定" : pairingReadyForConfirm \? "确认绑定" : "等待手机扫码…"/);

  // ── ⑦ 侧栏只剩两卡：服务状态 + 已绑定手机 ──────────────────────────────
  // 曾经还有一张「手机端可以做什么」授权范围卡（列出"能下发任务 / 不能改项目配置"）。
  // 2026-09-17 用户明确要求去掉 —— 那是**产品取舍**，不是遗漏：这一页要回答的是
  // "服务在不在跑、现在绑的是谁"，授权说明另有入口，摆在这里只会把侧栏撑成三卡。
  // 断言改成反向守卫：别让它（或它的专属样式）以"顺手加回来"的方式复活。
  assert.doesNotMatch(page, /手机端可以做什么/);
  assert.doesNotMatch(page, /不能改项目配置、装 MCP、开终端/);
  assert.doesNotMatch(desktopStyles, /\.desktop-remote-list/);

  // ── ⑧ 骨架与设置页 / Agent 档案页同源 ─────────────────────────────────
  // 旧版沿用手机页那套 30px 大标题 + 860px 卡片列，1440 屏上左右各空 ~290px，
  // 而且与同应用其它管理页不是一套语言（审计 C1/C2）。
  assert.match(desktopStyles, /width: min\(1120px, calc\(100% - 48px\)\)/);
  assert.match(desktopStyles, /min-height: 96px/);
  assert.match(desktopStyles, /border-bottom: 1px solid #cfe0d6/);
  assert.match(desktopStyles, /\.desktop-remote-grid \{ display: grid; grid-template-columns: minmax\(0, 1fr\) 320px; gap: 22px; align-items: start; \}/);
  // 侧栏跟手：主栏的二维码会随"生成/过期"变高变矮，状态不该跟着滚出视口。
  assert.match(desktopStyles, /\.desktop-remote-aside \{ position: sticky; top: 16px;/);
  // 窄窗口降级必须写在无断点规则**之后** —— 同特异性的宽度声明只认源码顺序
  // （tasks.css 的窄屏覆盖被后面那条无断点规则吃掉过一次）。
  assert.ok(
    desktopStyles.indexOf("@media (max-width: 1020px)") > desktopStyles.indexOf(".desktop-remote-grid { display: grid;"),
    "断点覆盖必须写在网格定义之后",
  );
  // 共用的状态条（刷新结果 / 配对状态行 / 错误）必须被拉进**本页那条内容列**。
  // 只清 `max-width` 是不够的：清掉之后元素会撑满 `<main>` 的内边距盒，而这一页的
  // `<main>` 没有左右内边距 ⇒ **满屏出血**（实测 1440 视口下宽 1440、x=0）。
  assert.match(
    desktopStyles,
    /\.mobile-remote\.desktop-remote \.mobile-refresh-status,[\s\S]{0,220}width: min\(1120px, calc\(100% - 48px\)\);/,
  );

  // ── ⑨ 类名一律带前缀 ─────────────────────────────────────────────────
  // 这一页与手机页共用组件，裸词会串味（本文件上半部分就有 `.mobile-status` 这种
  // 只差一个前缀的类名）。详见 mobile-remote.css 里「别用裸词做类名」那条。
  for (const bare of [".card", ".chip", ".list", ".btn", ".note", ".hint", ".empty", ".warn", ".code", ".kv", ".aside", ".grid", ".body", ".main"]) {
    assert.ok(!desktopStyles.includes("\n" + bare + " ") && !desktopStyles.includes("\n" + bare + ","), `样式表里出现裸词类名 ${bare}`);
  }

  // ── ⑩ 注册流程的**终态回执**与收尾（2026-09-17 复查新增）─────────────────
  // 三件事缺任何一件，真机上的表现都是"点了注册没反应 / 说了已就绪却还让你注册"：
  //   ① 成功那一刻 `ready` 变 true ⇒ 注册卡随 `desktopNeedsEnroll` 一起卸载 ——
  //      回执写在卡片里就等于没写（和配对状态行是同一个坑）；
  //   ② 「重新注册」那条路上成功之后必须把入口收掉，否则主栏停在
  //      "已就绪，但还摆着让你注册的表单"这种自相矛盾的状态；
  //   ③ 超时回执同理不可靠：用户可能在等待期间点了「取消」，卡片已经切回配对卡。
  const enrollPoll = page.match(/useEffect\(\(\) => \{\n    if \(!agentEnrollWaiting\) return;[\s\S]*?\n  \}, \[agentEnrollWaiting, loadAgentStatus\]\);/)?.[0] ?? "";
  assert.ok(enrollPoll, "找不到注册轮询 effect（签名变了就更新本用例的锚点）");
  assert.match(enrollPoll, /setReenrollOpen\(false\)/);
  assert.match(enrollPoll, /setPairingNotice\(\{ text: "远程服务已就绪，现在可以生成二维码了。", state: "success" \}\)/);
  assert.match(enrollPoll, /setPairingNotice\(\{ text: "仍未检测到注册结果[^"]*", state: "error" \}\)/);
  // 终态**不许**再写回卡片里的那句（写了就会随卡片一起消失）
  assert.doesNotMatch(enrollPoll, /setAgentEnrollMessage\("远程服务已就绪/);
  assert.doesNotMatch(enrollPoll, /setAgentEnrollMessage\("仍未检测到/);
  // 提交时要收掉上一轮的旧回执，否则它和本次进度同屏，看起来像刚出的结果。
  assert.match(page, /setAgentEnrollMessage\(""\);\n    \/\/ 收掉上一次留下的终态回执[\s\S]{0,140}setPairingNotice\(null\);/);
  // 回执的宿主必须在注册卡**之外**（`mobile-pairing-notice` 渲染在两栏之上）。
  const noticeAt = page.indexOf("className={`mobile-pairing-notice");
  const bodyAt = page.indexOf('className="desktop-remote-body"');
  assert.ok(noticeAt > 0 && bodyAt > noticeAt, "注册回执必须渲染在注册卡之外（卡片一卸载就跟着没了）");

  // ── ⑪ 侧栏那张绑定卡的重读时机 ───────────────────────────────────────
  // 依赖挂 `agentStatus`（对象）会让注册期间 2 秒一次的轮询把它一起带走 ——
  // 每次读回状态都是新对象 ⇒ 卡片反复重读、界面上闪成"正在读取…"。
  // 挂 `agentStatus?.ready`（布尔）则只在"就绪与否"真的变了才重读。
  assert.match(page, /\}, \[mobileApp, agentStatus\?\.ready, loadBindings\]\);/);
  // 未注册时**也要读**：服务端对未注册实例回 200 + `{ready:false}`（不是错误），
  // 卡片据此说"远程服务未注册，暂时读不到绑定信息" —— 比停在"正在读取…"诚实。
  // 以前那条"未就绪就早退"能成立，只因为首次运行时 agentStatus 还是 null（巧合），别再写回去。
  assert.doesNotMatch(page, /if \(agentStatus && !agentStatus\.ready\) return;/);

  // ── ⑫ 这一页碰得到的远端端点＝会话令牌白名单，且**不含心跳端点** ──────
  // control-server 的 `desktopPairingPaths` 是"页面能用会话令牌打的端点"那张显式小表
  // （其余 `/api/remote/**` 只认 Agent 令牌）。两张表必须一一对应：
  //   · 页面多打一个 → 401，用户看到的是"刷新失败"这种没头没脑的话；
  //   · 页面打 `/api/remote/overview` → **那是 Agent 的心跳端点**，control-server 在
  //     那个处理器里记"最近听到 Agent 说话"的时刻 —— 页面自己去打就等于自己给自己
  //     发心跳，"收不到心跳"那一档永远不可达（假绿，且是最难查的那种）。
  const apiPaths = [...page.matchAll(/\bapi(?:<[^>]*>)?\(\s*[`"]([^`"]+)[`"]/g)].map((match) => match[1].split("?")[0]);
  const relayPaths = [...new Set(apiPaths.filter((item) => item.startsWith("/api/remote/")))].sort();
  assert.deepEqual(
    relayPaths,
    [
      "/api/remote/agent-status",
      "/api/remote/bindings",
      "/api/remote/bindings/revoke",
      "/api/remote/pairing",
      "/api/remote/pairing/confirm",
      "/api/remote/pairing/status",
    ],
    "这一页能打的远端端点变了：新端点要同时进 control-server 的 desktopPairingPaths；心跳端点不许进",
  );

  // ── ⑬ 五档的"变体"走 data-* 属性，不拼类名 ─────────────────────────────
  // `desktop-remote-chip-${state}` 这种模板串类名**没有任何样式消费**（样式表里
  // 全是 `[data-state="…"]` 选择器），只会养出"看着像在用、改不动也删不掉"的死类名。
  assert.match(page, /className="desktop-remote-service" data-state=\{desktopService\.state\}/);
  assert.match(page, /className="desktop-remote-chip" data-state=\{desktopService\.state\}/);
  assert.doesNotMatch(page, /desktop-remote-(?:chip|service)-\$\{/);
  for (const state of ["loading", "unregistered", "stale", "failed"]) {
    assert.ok(desktopStyles.includes(`.desktop-remote-service[data-state="${state}"]`), `页头胶囊缺一档配色：${state}`);
  }
  // 侧栏那颗胶囊：stale 必须与 failed 一样走告警色，**不能退成"运行中"的绿色**。
  assert.ok(desktopStyles.includes('.desktop-remote-chip[data-state="stale"]'), "侧栏胶囊缺「无心跳」那一档配色（会退成绿色，等于把故障画成正常）");

  // ── ⑭ 「手机现在还在不在用」（2026-09-18 新增）────────────────────────
  // 在这之前桌面端只能答"绑过谁"，答不了"手机现在能不能用"：用户在手机上看到
  // "已连接 / 在线"，走到电脑前看到一片沉默 —— 而"被手机连着"正是进这一页要确认的事。
  //
  // ① 判据抽在纯模块里（四档 unsupported / never / online / idle），页面只负责递年龄。
  //    四档的**行为**断言在 features/remote/desktop-phone.test.ts；这里只守"接线还在"。
  assert.match(page, /const phoneSync = phoneSyncView\(boundPhone, phoneSyncAgeMs\);/);
  assert.match(page, /const boundPhonePlatform = platformLabel\(boundPhone\?\.platform\);/);
  // ② 年龄必须**自己随时间重算**。只读一次 Date.now() 的话，手机被系统杀掉之后
  //    这张卡会永远停在"在线"上 —— 和"远程服务没心跳"当初那屏静止的假绿是同一个坑。
  //    取年龄必须和 `loadBindings` 落地那一刻**共用同一个函数**：两处各写一遍
  //    `Date.parse` 会分叉出"刚读到是在线、5 秒后跳未同步"的抖动。
  const phoneTicker = page.match(/useEffect\(\(\) => \{\n    if \(mobileApp\) return;\n    const measure = \(\) => setPhoneSyncAgeMs\(syncAgeFrom\(bindings\[0\]\?\.lastUsedAt\)\);[\s\S]*?\n  \}, \[mobileApp, bindings\]\);/)?.[0] ?? "";
  assert.ok(phoneTicker, "找不到手机「最近同步」年龄的定时器（签名变了就更新本用例的锚点）");
  assert.match(phoneTicker, /window\.setInterval\(measure, 5000\)/);
  assert.match(page, /setPhoneSyncAgeMs\(syncAgeFrom\(items\[0\]\?\.lastUsedAt\)\);/, "数据落地那一刻必须就地量一次年龄，否则会先闪一帧「未同步」");
  assert.match(page, /import \{ normalizeBindings, phoneSyncView, platformLabel, syncAgeFrom, type DesktopBinding \} from "\.\.\/features\/remote\/desktop-phone";/);
  // ②b 挂载时那个 effect 会跑**两次**（`agentStatus` 先 null、再 `ready` 变 true），
  //     也就是开一次页面发两次请求。第二次必须静默：否则会闪一次「正在读取…」，
  //     且它偶发失败会把第一次刚读到的内容清成"读取失败"（探针 ④e 守真实行为）。
  //     但首次**不能**静默，否则"还没回来"会被渲染成"没有绑定"。
  assert.match(page, /const bindingsLoadedOnceRef = useRef\(false\);/);
  assert.match(page, /void loadBindings\(\{ silent: bindingsLoadedOnceRef\.current \}\);/);
  assert.match(page, /bindingsLoadedOnceRef\.current = true;/);
  // ③ 轮询四件套：静默重读 / 页面不可见停表 / **防叠加** / 就绪与否变了立刻重读。
  //    缺①会每 10 秒闪一次「正在读取…」；缺③会在本地服务变慢时把在飞的请求堆起来 ——
  //    `api()` 对 GET 带两级重试（15s→30s 超时），一次卡住的轮询能占 45 秒、发 3 个请求，
  //    而本地控制服务只有一个 SQLite 连接（越等越慢，同"outbox 热循环"那个形状）。
  const bindingsPoll = page.match(/\n    if \(!documentVisible\) return;\n    const timer = window\.setInterval\(\(\) => \{\n      if \(bindingsPollInFlightRef\.current\) return;\n[\s\S]*?\n  \}, \[mobileApp, documentVisible, agentStatus\?\.ready, loadBindings\]\);/)?.[0] ?? "";
  assert.ok(bindingsPoll, "找不到绑定信息的轮询 effect（少了它，手机连上/掉线都不会自己反映出来）");
  assert.match(bindingsPoll, /void loadBindings\(\{ silent: true \}\)\.finally\(\(\) => \{ bindingsPollInFlightRef\.current = false; \}\);/);
  assert.match(bindingsPoll, /\}, 10_000\);/);
  assert.match(page, /const bindingsPollInFlightRef = useRef\(false\);/);
  assert.match(page, /const loadBindings = useCallback\(async \(options\?: \{ silent\?: boolean \}\) => \{/);
  assert.match(page, /if \(!options\?\.silent\) setBindingsState\("loading"\);/);
  // ③b 请求代际：晚发的赢。少了它，一个"先失败"的旧请求会盖掉一个"后成功"的新结果，
  //     卡片会自己挂上"重读失败"，而其实刚刚才读成功过。同文件里 loadInstances 就是这么写的。
  //     ⚠️ 这道闸**只有这一层防线**：探针里构造不出"旧请求晚回来"——
  //     实测同源上有一个未回的响应时，后续请求会被浏览器排到它后面（串行），
  //     详见 `.tmp/probe-desktop-remote-page.mjs` 顶部那段说明（两次失败的尝试都记在里面了）。
  //     生产里 API 在另一个源、又多条连接，乱序是真实的，所以闸必须留。
  assert.match(page, /const bindingsRequestGenerationRef = useRef\(0\);/);
  assert.match(page, /const generation = \+\+bindingsRequestGenerationRef\.current;/);
  assert.equal(page.split("if (generation !== bindingsRequestGenerationRef.current) return;").length - 1, 2, "成功与失败两条路径都要丢掉过期响应");
  // ④ 静默重读失败：**不许**动 `bindingsState`，只置 `bindingsStale`。
  //    打成 failed 会让绑定卡整块换成"读取失败"、页头胶囊一起消失，10 秒后再长回来 ——
  //    那既是闪烁，又丢掉了"这台电脑绑了谁"这条此刻最需要的信息。
  //    ⚠️ 这条与下面那条必须成对存在：只守"silent 时不清空"会漏掉"silent 时也不许改状态"，
  //    而那正是最初写错的那一版（读数还在 state 里，渲染却已经被 failed 分支顶掉了）。
  assert.match(page, /if \(options\?\.silent\) \{\n        setBindingsStale\(true\);\n        return;\n      \}/);
  assert.match(page, /setBindings\(\[\]\);\n      setBindingsState\("failed"\);/);
  assert.match(page, /const \[bindingsStale, setBindingsStale\] = useState\(false\);/);
  // ⑤ 读数过期时页头**必须换一种说法**：继续说「手机在线」是拿旧读数冒充新的，
  //    改说「手机未同步」是把我们读不到栽赃给手机。只能是"读不到手机状态"。
  assert.match(page, /data-state=\{bindingsStale \? "stale" : phoneSync\.state\}/);
  assert.match(page, /\{bindingsStale \? "读不到手机状态" : phoneSync\.headerChip\}/);
  assert.ok(desktopStyles.includes('.desktop-remote-phone-chip[data-state="stale"]'), "页头手机胶囊缺「读不到手机状态」那一档配色");
  // ⑤b 卡片里那颗灯与胶囊也必须跟着换：过期时握着的是**上一次**读到的状态，
  //     继续亮绿灯／写「在线」就是拿旧读数冒充新的（"离线时亮绿灯等于骗人"）。
  //     但措辞只能是「读不到」，不许写「未同步」—— 那是把我们的读失败说成手机的事实。
  assert.equal(page.split('data-state={bindingsStale ? "stale" : phoneSync.state}').length - 1, 3, "页头胶囊 / 状态灯 / 卡片胶囊三处都要跟着过期态换挡");
  assert.match(page, /\{bindingsStale \? "读不到" : phoneSync\.chip\}/);
  assert.ok(desktopStyles.includes('.desktop-remote-phone-mark[data-state="stale"] .desktop-remote-phone-led'), "读数过期时状态灯必须换色（不许继续亮绿灯）");
  assert.ok(desktopStyles.includes('.desktop-remote-phone-state[data-state="stale"]'), "卡片里那颗胶囊缺「读不到」那一档配色");
  // 卡片里同理：这一档**只留**"重读失败"那句，不许再叠加一条归因给手机的 hint
  //（两段解释同屏，用户会以为是两件事）。
  assert.match(page, /\{bindingsStale\n                  \? <p className="desktop-remote-hint" data-tone="warn" role="status">最近一次重读绑定信息失败，上面显示的是最后一次读到的内容。<\/p>\n                  : phoneSync\.hint && <p/);
  // ⑥ 页头那颗胶囊：没绑定时**整颗不渲染**，不摆一句"未绑定"占位。
  //    状态灯与胶囊变体一律走 `data-state`，不拼类名（同 ⑬ 的理由）。
  assert.match(page, /\{bindingsState === "loaded" && boundPhone && <span className="desktop-remote-phone-chip"/);
  assert.match(page, /className="desktop-remote-phone-mark" data-state=/);
  assert.match(page, /className="desktop-remote-phone-state" data-state=/);
  assert.doesNotMatch(page, /desktop-remote-phone-(?:state|chip)-\$\{/);
  // 「未同步」两档必须换色：继续用"运行中"的绿，等于把"手机已经不在"画成正常。
  for (const state of ["idle", "never"]) {
    assert.ok(desktopStyles.includes(`.desktop-remote-phone-chip[data-state="${state}"]`), `页头手机胶囊缺一档配色：${state}`);
    assert.ok(desktopStyles.includes(`.desktop-remote-phone-state[data-state="${state}"]`), `手机名旁那颗胶囊缺一档配色：${state}`);
  }
  assert.ok(desktopStyles.includes('.desktop-remote-phone-mark[data-state="unsupported"] .desktop-remote-phone-led'), "读不到「最近同步」时状态灯必须换成灰的（不许继续亮绿灯）");
  // ⑦ 「最近同步」是本页新增的读数；读不到时给「—」，不许编一句"未同步"顶上
  //    （那是"还没回来"写成"没有数据"，这个项目踩过三次）。
  assert.match(page, /<dt>最近同步<\/dt><dd>\{phoneSync\.agoText \|\| "—"\}<\/dd>/);
  // ⑧ 平台由手机上报，两条 claim 路径都要带上；闭集收口在 lib/mobile-devices.ts
  //    （`devicePlatform`），别直接塞 `Capacitor.getPlatform()` 的原始值。
  assert.equal(page.split("platform: devicePlatform(Capacitor.getPlatform())").length - 1, 2, "两条 claim 路径（扫码 / 6 位校验码）都要上报平台");
});
test("mobile conversation creation and message send never wait for the desktop", () => {
  // 症状（用户报的）：手机上点「创建会话」，要等电脑端把命令跑完界面才切过去；SSE 一断就是
  // 对着一个按钮全灰的弹层干等 30 秒。发消息超时还会把刚发出的气泡撤掉、正文塞回输入框，
  // 诱导用户重发一次（而电脑端其实已经收到并在回答了）。
  //
  // 不变式：**手机端的操作在本机的这一刻就完成**，那条命令只是事后发给电脑端的一条通知。
  // 下面每条断言都对应一种会让它退回去的写法。
  const createBody = page.match(/async function createConversationForProject\(projectValue: Project, agentId\?: "claude-code" \| "codex"\) \{[\s\S]*?\n  \}\n/)?.[0] ?? "";
  assert.ok(createBody, "找不到 createConversationForProject");
  const enterAt = createBody.indexOf("enterConversationView();");
  const waitAt = createBody.indexOf("await waitForCommand(");
  // ① 进会话必须发生在等命令**之前**。反过来写（先 await 再 enter）就是把用户按在
  //    "什么都还没发生"的那一屏上，最长 30 秒。
  assert.ok(enterAt >= 0 && waitAt >= 0 && enterAt < waitAt, "必须先切视图、再去等命令");
  // ② 本地那条会话要先登记再进视图，否则进去看到的是一条不在项目列表里的会话。
  assert.ok(createBody.indexOf("pendingConversationsRef.current.set(pendingID") < enterAt, "本地会话要先登记");
  // ③ 新建会话不许占全局 busy：占了之后任务面板、别的会话、扫码会一起被按住 ——
  //    那正是"一个操作把整页锁住"的形状。
  assert.doesNotMatch(createBody, /setBusy\(/);
  // ④ 弹层立刻收起，不用等回执。
  assert.ok(createBody.indexOf("setNewConversationProject(null);") < waitAt, "弹层必须立刻收起");

  // ⑤ 发消息同样不占全局 busy，并且在会话还没有真 id 时把消息**排队**而不是丢掉。
  const sendBody = page.match(/async function sendConversationMessage\(event: FormEvent\) \{[\s\S]*?\n  \}\n/)?.[0] ?? "";
  assert.ok(sendBody, "找不到 sendConversationMessage");
  assert.doesNotMatch(sendBody, /setBusy\(/);
  assert.match(sendBody, /if \(isPendingConversationID\(conversationID\)\) \{/);
  assert.match(sendBody, /deferredMessagesRef\.current\.set\(conversationID, queue\);/);

  // ⑥ 「没拿到终态」（null）那一支**不许**撤气泡、不许回填输入框 —— 与任务那三条同一套策略。
  const dispatchBody = page.match(/async function dispatchConversationMessage\([\s\S]*?\n  \}\n/)?.[0] ?? "";
  assert.ok(dispatchBody, "找不到 dispatchConversationMessage");
  const timeoutBranch = dispatchBody.match(/if \(!finalState\) \{[\s\S]*?\n        \}/)?.[0] ?? "";
  assert.ok(timeoutBranch, "找不到「没拿到终态」那一支");
  assert.doesNotMatch(timeoutBranch, /dropOptimistic\(\)|onRejected\(/, "超时不等于失败：不许撤气泡、不许回填");

  // ⑦ 快捷方式只有 fill 需要等（正文是电脑端渲染的）；run / confirm 不许 await ——
  //    那等于让用户为一个他根本不看的返回值等最长 30 秒。
  const shortcutBody = page.match(/async function sendShortcutCommand\(shortcutID: string, action: "fill" \| "run" \| "confirm"\) \{[\s\S]*?\n  \}\n/)?.[0] ?? "";
  assert.ok(shortcutBody, "找不到 sendShortcutCommand");
  const nonFill = shortcutBody.match(/if \(action !== "fill"\) \{[\s\S]*?\n      \}/)?.[0] ?? "";
  assert.ok(nonFill, "找不到 run / confirm 那一支");
  assert.doesNotMatch(nonFill, /await waitForCommand/);
  assert.match(nonFill, /void waitForCommand\(/);

  // ⑧ 待确认会话的认领只由快照驱动（SSE / 命令回执 / 兜底轮询三条来源最终都落到快照上）。
  //    散成多份的话，SSE 一断就有一条链路永远接不上真身。
  assert.equal((page.match(/matchPendingConversation\(/g) ?? []).length, 1, "认领判据只允许出现一处");
  assert.match(page, /const realID = matchPendingConversation\(item, owning\.conversations, claimed\);/);
  // 同项目里同时新建两条时，谁也不许把对方已认下的那条会话再认一遍 —— 两条的判据长得
  // 一模一样（"第一条以前没见过的会话"），不排掉就会双双认到同一条上。
  assert.match(page, /const claimed = Array\.from\(registry\.values\(\)\)\n        \.filter\(\(other\) => other\.id !== item\.id && other\.resolvedId\)/);
  assert.match(page, /if \(!item\.resolvedId\) resolvePendingConversation\(item\.id, realID\);/);

  // ⑨ 交棒必须是**两步**：认下真 id 只让本地卡改用真 id 继续顶着，摘掉它还要再等
  //    「快照版本号真的前进过」。理由：SSE 的 conversation.created 会先把会话塞进当前
  //    那版快照，紧随其后的刷新拿到的往往还是"创建之前"的那一版 —— 只看"出现了就交棒"，
  //    那一刷就会把卡片连同用户刚打的消息整块盖掉（浏览器探针 ⑲ 实测复现过）。
  assert.match(page, /if \(snapshot\.snapshotRevision > item\.seenRevision\) adoptPendingConversation\(item\.id, realID\);/);
  // 登记时取的是**当前快照对象**的版本号，不是 ref：断网回落到 localStorage 缓存那条路
  // 只 setSnapshot、不动 ref，两者会分叉，而 SSE 塞进会话的正是当前这份快照。
  assert.match(page, /seenRevision: snapshot\?\.snapshotRevision \?\? snapshotRevisionRef\.current,/);
  const resolveBody = page.match(/function resolvePendingConversation\(pendingID: string, realID: string\) \{[\s\S]*?\n  \}\n/)?.[0] ?? "";
  assert.ok(resolveBody, "找不到 resolvePendingConversation");
  assert.doesNotMatch(resolveBody, /registry\.delete\(/, "认下真 id 的那一步不许摘掉本地卡");
  assert.match(resolveBody, /registry\.set\(pendingID, \{ \.\.\.item, resolvedId: realID \}\);/);
  // 补发必须用**真 id**：用临时 id 发出去就是一条注定"找不到会话"的命令。
  // 而且它挂在 resolve 上（回执丢了、只靠快照认回来那条路同样会经过这里），
  // 漏掉就等于用户排队的消息永远发不出去，且界面上什么都不会说。
  assert.match(resolveBody, /flushDeferredConversationMessages\(pendingID, realID\);/);
  const flushBody = page.match(/function flushDeferredConversationMessages\(pendingID: string, realID: string\) \{[\s\S]*?\n  \}\n/)?.[0] ?? "";
  assert.ok(flushBody, "找不到 flushDeferredConversationMessages");
  // 逐条**按顺序**发。电脑端执行远程命令是串行的，而并发 POST 的到达顺序没有保证 ——
  // 用户在会话还没建好时连发三条，并发补发会让电脑端乱序执行、回复也跟着乱序。
  assert.match(flushBody, /for \(const message of deferred\) \{\n        await dispatchConversationMessage\(realID, message\.content, message\.requestId, message\.createdAt/);
  assert.doesNotMatch(flushBody, /void dispatchConversationMessage\(/);
  const adoptBody = page.match(/function adoptPendingConversation\(pendingID: string, realID: string\) \{[\s\S]*?\n  \}\n/)?.[0] ?? "";
  assert.ok(adoptBody, "找不到 adoptPendingConversation");
  assert.match(adoptBody, /if \(!pendingConversationsRef\.current\.delete\(pendingID\)\) return;/);
  // 本地卡退场的那一刻，气泡的顶替来源就从叠加层换成了快照 —— 而快照里那条很可能还是
  // `conversation.created` 塞进去的空壳。不就地补上，用户的刚发出去的消息会消失到
  // 电脑端下一次上传快照为止。
  assert.match(adoptBody, /const messages = pendingMessageRef\.current\.get\(realID\);/);
  assert.match(adoptBody, /const additions = bubbles\.filter\(\(bubble\) => !seen\.has\(bubble\.id\)\);/);

  // ⑩ 本地卡退场后，气泡仍要有人顶：SSE 抢先塞进快照的那条是**空壳**，不把还没落地的
  //    乐观气泡补上去，用户刚发出去的消息就会凭空消失一帧。
  assert.match(page, /\(conversationId\) => Array\.from\(pendingMessageRef\.current\.get\(conversationId\)\?\.values\(\) \|\| \[\]\),/);

  // ⑪ 换 id 不是换会话。`selectedConversation` 从临时 id 换成真 id 会触发那个"换会话就
  //    清草稿 / 清技能胶囊 / 收面板"的复位 effect —— 用户还停在同一条会话上，正打在
  //    输入框里的字不该被一条后台回执清掉（这是"新建会话后立刻打字"最常见的动作）。
  //    守卫必须**幂等**：StrictMode 与 Fast Refresh 会把 effect 多跑一遍，写成"读一次
  //    就清空标记"的话第二遍就会误判成换会话。
  assert.match(page, /const conversationIDSwapRef = useRef<\{ projectId: string; from: string; to: string \} \| null>\(null\);/);
  assert.match(page, /conversationIDSwapRef\.current = \{ projectId, from: pendingID, to: realID \};/);
  const resetBody = page.match(/useEffect\(\(\) => \{\n    \/\/ ⚠️ 新建会话那条链路上[\s\S]*?\n  \}, \[selectedProject, selectedConversation\]\);/)?.[0] ?? "";
  assert.ok(resetBody, "找不到换会话复位的 effect");
  assert.match(resetBody, /const isIDSwap = Boolean\(swap && swap\.projectId === selectedProject && swap\.to === selectedConversation\);/);
  assert.match(resetBody, /if \(!isIDSwap\) conversationIDSwapRef\.current = null;\n    if \(isIDSwap\) return;\n    setMessageDraft\(""\);/);

  // ⑫ 已经认下真 id 之后不许再回滚：那条会话在电脑端确实存在，撤卡 + 把已发出的正文塞回
  //    输入框 + 把用户踢回列表，等于当着用户的面删掉一条活着的会话。
  const failBody = page.match(/function failPendingConversation\(pendingID: string, reason: string\) \{[\s\S]*?\n  \}\n/)?.[0] ?? "";
  assert.ok(failBody, "找不到 failPendingConversation");
  assert.match(failBody, /if \(item\.resolvedId\) \{ setError\(reason\); return; \}/);
  // 回填正文只在"用户此刻就停在这条会话上"时做。输入框只有一个，用户已经走开到别的会话
  // 时把他没读过的字灌进去，比丢掉这段字更糟（他会以为那是当前会话的草稿）。
  assert.match(failBody, /const wasOnConversation = selectedConversationRef\.current === pendingID;/);
  assert.match(failBody, /if \(deferred\.length > 0 && wasOnConversation\) \{/);
  assert.match(page, /\(\) => applyPendingConversations\(/);
  assert.match(page, /\[snapshot, pendingTaskRevision, pendingConversationRevision\],/);
});

test("mobile event stream detects a dead connection instead of hanging forever", () => {
  // 症状：手机换网 / NAT 表超时 / 系统把 socket 悄悄收走之后，这条 SSE 变成**半开**连接 ——
  // `read()` 一直挂着，既没有事件也没有错误。原来那个 1s→30s 的退避重连只挂在 read() 报错上，
  // 于是永远轮不到：实时通道静默，用户只能重开 App。这是文档里记过的"agent 卡死、重连循环
  // 再也没跑过"在客户端的镜像（那次修的是服务端）。
  //
  // 服务端每 15 秒发一条 `: keepalive`（cloud-control 的 mobileEventStream），所以客户端
  // 可以按"多久没有字节进来"判定。
  const body = page.match(/async function consumeMobileEventStream\([\s\S]*?\n  \}\n\}/)?.[0] ?? "";
  assert.ok(body, "找不到 consumeMobileEventStream");
  // ① 判据：45 秒（三次没收到 keepalive），且这个数必须与 15 秒的 keepalive 同处可查。
  assert.match(page, /const streamIdleTimeoutMs = 45_000;/);
  assert.match(page, /服务端每 15 秒往这条流里写一条 `: keepalive` 注释/);
  // ② 用**内部**的 controller 超时，不直接 abort 调用方那个 signal：后者表示"这次订阅该结束了"
  //    （卸载 / 换电脑），混在一起会让上层把一次超时当成卸载而不再重连。
  assert.match(body, /const controller = new AbortController\(\);/);
  assert.match(body, /const abortByCaller = \(\) => controller.abort\(\);/);
  assert.match(body, /signal\.addEventListener\("abort", abortByCaller, \{ once: true \}\);/);
  assert.match(body, /idleTimer = window\.setTimeout\(\(\) => controller\.abort\(\), streamIdleTimeoutMs\);/);
  // ③ 看门狗必须在**每次读到字节时**重新起表（注释行也算，它本来就只为"我还活着"而存在），
  //    并且读完要清掉，否则一次订阅结束之后还会留下一个定时器。
  assert.match(body, /armIdleWatchdog\(\);\n    while \(!controller\.signal\.aborted\) \{\n      const next = await reader\.read\(\);\n      \/\/ 每读到一段字节就重新起表[\s\S]{0,80}\n      armIdleWatchdog\(\);/);
  assert.match(body, /while \(!controller\.signal\.aborted\) \{/);
  assert.match(body, /\} finally \{\n    if \(idleTimer !== undefined\) window\.clearTimeout\(idleTimer\);\n    signal\.removeEventListener\("abort", abortByCaller\);/);
  // ④ 循环判据换成内部 signal：还用调用方那个的话，看门狗 abort 之后这一次循环不会结束。
  assert.doesNotMatch(body, /while \(!signal\.aborted\)/);
});

test("mobile file view is a sub-state of the conversation, wired to the relay adapter", async () => {
  const filesPanel = normalize(await readFile(new URL("./features/files/FilesPanel.tsx", import.meta.url), "utf8"));
  const projectFileTree = normalize(await readFile(new URL("./features/files/ProjectFileTree.tsx", import.meta.url), "utf8"));
  const fileModel = normalize(await readFile(new URL("./features/files/file-model.ts", import.meta.url), "utf8"));
  const fileViewer = normalize(await readFile(new URL("./features/files/FileViewer.tsx", import.meta.url), "utf8"));
  const fileEditor = normalize(await readFile(new URL("./features/files/FileEditor.tsx", import.meta.url), "utf8"));
  const codeFileView = normalize(await readFile(new URL("./features/files/CodeFileView.tsx", import.meta.url), "utf8"));
  // `.mobile-files` 的规则散在两处，是有意的：**布局**（容器高度、顶栏标题胶囊）在
  // mobile-remote.css，**状态相关的**（键盘避让的 max-height）跟文件面板自己那套在一起，
  // 留在 files.css。改前要看清断言的是哪一份，别把两边混着比。
  const fileStyles = normalize(await readFile(new URL("./files.css", import.meta.url), "utf8"));

  // ① 入口：会话 ⋯ 菜单里那一项，走 openFiles。写成 `() => openFiles()` 而不是
  //    直接传函数 —— 直接传会把 click 事件当成"要打开的文件路径"传进去。
  assert.match(page, /<button type="button" role="menuitem" onClick=\{\(\) => openFiles\(\)\}><span>项目文件<\/span><small>查看与编辑<\/small><\/button>/);
  // 而 openFiles 自己也挡一道：拿到的不是字符串就当成"没指定文件"。
  assert.match(page, /setFilesInitialPath\(typeof initialPath === "string" && initialPath\.trim\(\) \? initialPath\.trim\(\) : null\);/);

  // ② 它**不压历史层**。压了的话 `exitConversationView` 里那次 `history.back()` 会先退掉
  //    文件层、而会话层的标记已经被清掉 —— 用户停在会话视图里，"再按一次返回"却已无层可退。
  const openFilesBody = page.match(/function openFiles\(initialPath\?: string\) \{[\s\S]*?\n  \}/)?.[0] ?? "";
  assert.ok(openFilesBody, "openFiles 不见了");
  assert.doesNotMatch(openFilesBody, /pushState|history\./);

  // ③ 返回键顺序：先让面板退一层（查看器 → 文件列表），退不动了才关面板。
  //    反过来会让用户在查看器里按返回被直接弹回会话，正在看的文件就没了。
  assert.match(page, /if \(mobileApp && mobileView === "conversation" && filesOpen\) \{\n\s*if \(filesPanelRef\.current\?\.showTree\(\)\) return true;\n\s*closeFiles\(\);\n\s*return true;\n\s*\}/);
  // 页内「←」必须与返回键同一条顺序（两处各写一遍必然有一天分叉）。
  assert.match(page, /function goBack\(\) \{\n\s*if \(mobileApp && mobileView === "conversation" && filesOpen\) \{\n\s*\/\/ 与返回键同一条顺序[\s\S]{0,120}?if \(filesPanelRef\.current\?\.showTree\(\)\) return;\n\s*closeFiles\(\);\n\s*return;\n\s*\}/);
  // 「当前有没有上一层」只有面板自己知道，所以走 ref 暴露的方法，而不是外层猜一个布尔量回传。
  assert.match(page, /const filesPanelRef = useRef<FilesPanelHandle \| null>\(null\);/);
  assert.match(filesPanel, /useImperativeHandle\(ref, \(\) => \(\{[\s\S]*?showTree: \(\) => \{[\s\S]*?if \(mobileView === "tree"\) return false;[\s\S]*?setMobileView\("tree"\);[\s\S]*?return true;/);

  // ④ 取数走中继适配器，不是本机 REST。适配器**持有缓存**，所以必须记忆化 ——
  //    每次渲染换一个 = 缓存清零，用户在目录里点两下就会不停重新拉取。
  assert.match(page, /const filesAdapter = useMemo\(\(\) => createMobileFsRequest\(\{ transport: rpcTransport \}\), \[rpcTransport\]\);/);
  assert.match(page, /return \(op: string, params: unknown, timeoutMs\?: number\) =>\n\s*cloud<MobileRpcReply>\(`\/v1\/instances\/\$\{encodeURIComponent\(instanceID\)\}\/rpc`, \{/);
  assert.match(page, /request=\{filesAdapter\.request\}/);
  // 图片字节的入口同样是记忆化对象：面板里的 useMobileMedia 把它写进依赖，
  // 每次渲染换一个新对象会让每张图重新解析（自造的死循环）。
  assert.match(page, /const filesMedia = useMemo\(\(\) => \(\{ resolve: \(path: string\) => filesAdapter\.resolveMedia\(path\) \}\), \[filesAdapter\]\);/);
  // 会话还是"本地已建好、电脑端没分配真 id"时不能把它的 id 传下去：
  // 服务端要按 id 查工作区，查不到会让整个文件视图报错。
  assert.match(page, /const conversationID = conversation && !conversationPending \? conversation\.id : "";/);

  // ⑤ 手机端的能力降级：不给下载入口、编辑器走移动分支。**不预先把面板锁成只读** ——
  //    手机端没有"工作区此刻被谁占着"的读数，锁早了会让用户在 AI 没跑时也改不了文件。
  assert.match(page, /media=\{filesMedia\}\n\s*mobile\n\s*disableDownload/);
  assert.match(page, /isWorkspaceOccupied=\{false\}/);

  // ⑥ 图片不能再走 `<img src="/fs/raw">`：那是浏览器导航，带不了 Authorization 头，
  //    手机的 WebView 也到不了电脑的本机端口。手机上字节只能从中继取回来。
  assert.match(fileViewer, /const mobile = useMobileMedia\(media, stat\.path\);/);
  assert.match(fileViewer, /export interface FileViewerMedia \{/);
  assert.match(fileViewer, /const url = URL\.createObjectURL\(base64ToBlob\(resolution\.base64, resolution\.mimeType\)\);/);
  // object URL 必须撤销：不撤销就是每张图泄漏一块内存，而 Markdown 文档里图片可能很多。
  assert.match(fileViewer, /if \(createdUrl\) URL\.revokeObjectURL\(createdUrl\);/);
  // 拿不到字节时要说**原因**，不能只说"加载失败"（用户会以为文件坏了或网络有问题）。
  assert.match(fileViewer, /if \(mobile\.message\) return <FileMessage projectId=\{projectId\} conversationId=\{conversationId\} stat=\{stat\} disableDownload=\{disableDownload\} message=\{mobile\.message\} \/>;/);
  // 内容被服务端省掉时只渲染元信息卡，不看 previewKind —— 一个大 .ts 文件会算出 "source"，
  // 按它渲染就是一个空白编辑器，用户会以为文件是空的。
  // 服务端的「能不能改」优先级最高：它知道内容发不发得回去（要减掉 JSON 转义与
  // 中继信封的余量），而 isEditableFile 只看扩展名与 isText。少了这一条，那条
  // 256–320 KiB 的只读带就形同不存在 —— 界面亮出「编辑」，用户改完按保存才失败。
  assert.match(fileViewer, /const serverReadOnly = editable === false;/);
  assert.match(fileViewer, /const canEdit = !omittedMessage && !serverReadOnly && isEditableFile\(stat\)/);
  assert.match(filesPanel, /editable: res\?\.editable,/);
  assert.match(filesPanel, /readOnlyReason: res\?\.readOnlyReason,/);
  assert.match(filesPanel, /editable=\{activeFile\.editable\}/);
  assert.match(filesPanel, /readOnlyReason=\{activeFile\.readOnlyReason\}/);
  // 按钮不渲染 ≠ 说清了原因：只读那句解释必须**另起一支**，否则用户只看到
  // 一个没有「编辑」的工具条，看不出是文件太大还是别的什么。
  assert.match(fileViewer, /\{!canEdit && !omittedMessage && serverReadOnly && <span className="file-viewer-readonly-hint">\{readOnlyHint\}<\/span>\}/);
  assert.match(fileViewer, /const readOnlyHint = readOnlyReason === "binary_file" \? "只读 · 非文本文件" : "只读 · 文件较大";/);
  assert.match(filesPanel, /omittedMessage=\{activeFile\.omitted\?\.message\}/);
  assert.match(filesPanel, /const reason = contentOmittedFrom\(error\);\n\s*if \(!reason\) throw error;\n\s*omitted = reason;/);

  // ⑦ 编辑器：手机端必须软换行（窄屏上横向滚代码没法读，还会和边缘返回手势抢事件）。
  assert.match(codeFileView, /setExtensions\(wrap \? \[\.\.\.languageExtensions, module\.EditorView\.lineWrapping\] : languageExtensions\);/);
  assert.match(fileEditor, /<CodeFileView content=\{content\} filename=\{stat\.name\} fontSize=\{fontSize\} editable onChange=\{onChange\} wrap=\{mobile\} \/>/);
  // 软键盘避让：量的是"编辑器顶边到键盘顶边"，不是整个可视高度 ——
  // 编辑器上方还压着 sticky 顶栏，只按可视高度压，工具栏仍会落在键盘下面。
  assert.match(fileEditor, /const top = editorRef\.current\?\.getBoundingClientRect\(\)\.top \?\? 0;\n\s*const available = Math\.max\(200, Math\.round\(viewport\.height - top\)\);/);
  assert.match(fileEditor, /if \(!mobile\) return;\n\s*const viewport = window\.visualViewport;/);
  assert.match(fileStyles, /\.mobile-files \.file-editor \{\n\s*max-height: var\(--file-editor-visible-height, none\);\n\}/);
  // 手机上目录树是全屏的，没有"并排可拖宽"这回事：留着 resize 手柄会画出一个拖了没用的抓取点。
  assert.match(fileStyles, /\.mobile-files \.files-tree \{ width: 100%; min-width: 0; resize: none; \}/);

  // ⑧ 容器必须有界高度：`.files-panel` 内部全是 flex + overflow，容器高度不定时
  //    目录树与编辑器不会各自滚动（会变成整页一起滚，顶栏跟着走）。
  assert.match(styles, /\.mobile-files, \.mobile-git-view \{\n\s*--mobile-files-chrome: 76px;\n\s*display: flex;\n\s*height: calc\(100dvh - var\(--mobile-header-height, 58px\) - var\(--mobile-files-chrome\)\);/);
  // 窄屏那一档页面 padding 变小，chrome 跟着改；同一选择器靠**顺序**生效，必须排在无断点规则之后。
  assert.match(styles, /@media \(max-width: 620px\) \{ \.mobile-files, \.mobile-git-view \{ --mobile-files-chrome: 58px; \} \}/);
  assert.ok(
    styles.indexOf("@media (max-width: 620px) { .mobile-files, .mobile-git-view {") > styles.indexOf("--mobile-files-chrome: 76px;"),
    "窄屏覆盖必须写在无断点规则之后（同一选择器、同一优先级，靠顺序生效）",
  );

  // ⑪ 服务端批量取树时只把读不到的目录标出来、不让整棵树失败（readTree 的约定），
  //    所以界面必须说出来：不说它就和空目录长得一模一样。三处缺一不可 ——
  //    类型、从 FileEntry 搬到树条目、渲染。
  assert.match(fileModel, /unreadable\?: boolean;/);
  assert.match(projectFileTree, /unreadable: entry\.unreadable,/);
  assert.match(projectFileTree, /\{item\.data\.unreadable && <span className="file-tree-item-unreadable" title="这个目录读不到">读不到<\/span>\}/);
  // getItemTitle 的返回值同时被当成行内可见标签 —— 把解释塞进去会变成
  // 「locked（这个目录读不到）  读不到」。判据是它必须原样返回名字。
  assert.match(projectFileTree, /getItemTitle=\{\(item\) => item\.data\.name\}/);
  assert.match(fileStyles, /\.file-tree-item-unreadable \{ flex: none; margin-left: auto;/);

  // ⑨ 上下文一变（换会话 / 换项目 / 令牌失效清空项目），文件面板必须一起收掉。
  //    它是**会话视图的子态**，绑的是「哪条会话的哪个工作区」（适配器按 conversation.id
  //    建、缓存跟着那条会话）。留着会让用户看到另一条会话的文件树；而它一旦因为
  //    `project` 暂时为空而不渲染（令牌失效那条链会清空 selectedProject），状态仍在，
  //    下次进任何一个项目它又会自己冒出来 —— 与设置页那条「清理 effect 不重跑」同因。
  assert.match(page, /setSessionSub\(null\);\n\s*setFilesInitialPath\(null\);\n\s*filesGuardRef\.current = null;\n\s*\}, \[selectedProject, selectedConversation\]\);/);
  // ⑩ 退出会话视图那条链走的是 `setFilesOpen` 而不是 `closeFiles`，所以守卫要在这里
  //    再清一次（closeFiles 里那句管不到它）：留着会指向一个已卸载的面板，
  //    「放弃未保存的更改」那条链会拿着它去问一个不存在的编辑态。
  {
    const leaveAt = page.indexOf("function leaveConversationView() {");
    const leaveBlock = leaveAt > 0 ? page.slice(leaveAt, page.indexOf("\n  }", leaveAt)) : "";
    assert.ok(leaveBlock.includes("setSessionSub(null);"), "侧滑返回那条链也要收掉子态槽位");
    assert.ok(leaveBlock.includes("filesGuardRef.current = null;"), "侧滑返回那条链要清掉面板登记的守卫");
  }
});

test("会话里的文件路径变成可点的入口，且只接管行内反引号", () => {
  // 手机上看文件九成是为了看 AI 刚改了什么；AI 引用文件最常说 `src/main.ts`。
  // 没有这个入口，用户得从会话退出去、进文件视图、再一层层点进去。
  assert.match(page, /code: \(\{ className, children \}\) => <MobileInlineCode className=\{className\} onOpenFile=\{openFiles\}>\{children\}<\/MobileInlineCode>/);
  // 只接管**行内** code。块级代码走 pre（markdownCodeComponents 里那个带复制按钮的），
  // 它的内层 <code> 带 language-* 类名 —— 不排除掉，一整块代码会被当成一个路径按钮，
  // 点下去必然报"文件不存在"。
  assert.match(page, /const isBlock = typeof className === "string" && className\.includes\("language-"\);/);
  assert.match(page, /const reference = isBlock \? null : projectFileReference\(text\);/);
  assert.match(page, /if \(!reference\) return <code className=\{className\}>\{children\}<\/code>;/);
  // 行号只作为提示带出去：手机端查看器不做跳行，带行号的字符串当路径去查会直接找不到文件。
  assert.match(page, /title=\{`打开 \$\{reference\.path\}\$\{reference\.line \? `:\$\{reference\.line\}` : ""\}`\}/);
  assert.match(page, /onClick=\{\(\) => onOpenFile\(reference\.path\)\}/);
  // 进面板时带上要打开的文件；**消费掉必须清空**，否则下次再进文件视图又会自动打开它。
  assert.match(page, /initialPath=\{filesInitialPath\}/);
  assert.match(page, /onInitialPathConsumed=\{\(\) => setFilesInitialPath\(null\)\}/);
  assert.match(styles, /\.mobile-message-file-link \{ display: inline-flex;/);
});


test("mobile Git workbench is a sub-state of the conversation, wired to its own adapter", async () => {
  const workbench = normalize(await readFile(new URL("./features/git/GitWorkbench.tsx", import.meta.url), "utf8"));
  const panel = normalize(await readFile(new URL("./features/git/MobileGitPanel.tsx", import.meta.url), "utf8"));
  const adapter = normalize(await readFile(new URL("./features/git/mobile-git-request.ts", import.meta.url), "utf8"));
  const gitStyles = normalize(await readFile(new URL("./git.css", import.meta.url), "utf8"));

  // ① 入口：会话 ⋯ 菜单里「项目文件」之后那一项，走 openGit。
  assert.match(page, /<button type="button" role="menuitem" onClick=\{\(\) => openGit\(\)\}><span>Git 工作台<\/span>/);

  // ② 子态是**一个槽位**，不是第二个布尔。两个布尔意味着返回键链与 leaveConversationView
  //    要各记两条，而那也是"两处必须一致"的结构 —— 漏一处就会出现"点错也能执行"。
  assert.match(page, /const \[sessionSub, setSessionSub\] = useState<null \| "files" \| "git">\(null\);/);
  assert.match(page, /const filesOpen = sessionSub === "files";/);
  assert.match(page, /const gitOpen = sessionSub === "git";/);
  assert.match(page, /const subHeaderActive = filesHeaderActive \|\| gitHeaderActive;/);

  // ③ 返回键两处都要有 Git 这一档，且顺序是"先退面板里的那一层"。
  //    漏了它，用户在 diff 里按返回会被直接弹回会话，正在看的那份差异就没了。
  const backKeyGit = /if \(mobileApp && mobileView === "conversation" && gitOpen\) \{\n\s*if \(gitPanelRef\.current\?\.showTopLevel\(\)\) return true;\n\s*closeGit\(\);\n\s*return true;\n\s*\}/;
  assert.match(page, backKeyGit);
  const goBackGit = /if \(mobileApp && mobileView === "conversation" && gitOpen\) \{\n\s*if \(gitPanelRef\.current\?\.showTopLevel\(\)\) return;\n\s*closeGit\(\);\n\s*return;\n\s*\}/;
  assert.match(page, goBackGit);
  // 两处各写一遍必然有一天分叉，所以顺序也要守：Git 那一档必须在会话回退之前。
  assert.ok(
    page.indexOf("gitPanelRef.current?.showTopLevel()") < page.indexOf('if (mobileApp && mobileView === "conversation") {\n        exitConversationView();'),
    "Git 的详情层必须先于「退出会话视图」被消化",
  );

  // ④ 退出会话视图（popstate / 侧滑那条不走返回键的链）同样要收掉它，两个守卫都要清。
  assert.match(page, /function leaveConversationView\(\) \{[\s\S]*?setSessionSub\(null\);[\s\S]*?gitPanelRef\.current = null;/);

  // ⑤ 渲染：Git 面板挂在槽位上，用与文件面板**共用**的容器类，并把共用发信器传下去。
  assert.match(page, /\{mobileApp && mobileView === "conversation" && project && gitOpen && <section className="mobile-git-view" aria-label="Git 工作台"><MobileGitPanel/);
  assert.match(page, /\n\s*transport=\{rpcTransport\}\n/);
  // 会话列表与子态互斥：漏改这条会让 Git 视图下面又渲染一整段会话。
  assert.match(page, /\{mobileApp && mobileView === "conversation" && project && !subHeaderActive && <section className="mobile-conversation">/);
  // 顶栏 ⋯ 菜单里的「刷新仓库状态」走工作台自己的 reload，而不是重新同步云端快照
  // （快照里根本没有仓库状态，那两件事语义不同）。
  assert.match(page, /function reloadGit\(\) \{\n\s*gitPanelRef\.current\?\.reload\(\);\n\s*\}/);
  assert.match(page, /onClick=\{\(\) => \{ setHeaderMenuOpen\(false\); reloadGit\(\); \}\}><span>刷新仓库状态<\/span>/);

  // ⑥ 适配器必须记忆化：它持有"上一条可恢复提示还挂着吗"这份状态（以及两个回调）。
  //    每次渲染重建会把它清零，于是那条提示的重置时机变得不可预测。
  assert.match(panel, /const adapter = useMemo\(/);
  assert.match(panel, /\[transport\],/);
  assert.match(panel, /onStale: \(message\) => setNotice\(message\)/);
  assert.match(panel, /onRecovered: \(\) => setNotice\(""\)/);

  // ⑦ 错误与提示有**就地落点**，不走页面级那条 `.mobile-error`。
  //    理由不是审美：那个文件里已经有三处记录了"页面级错误条在弹层遮罩之下、用户看不到"。
  //
  //    `fail` **不能**直接接 setError：「仓库状态已变化」那条同时走绿色提示与抛出的错误
  //    （适配器为了让调用方仍能判失败，必须抛），直接接就会让用户同时看到
  //    "已为你刷新"和一条红色的"…：Git state changed…"。所以这里要认出那句话并放行。
  assert.match(panel, /fail=\{\(message\) => \{ if \(message !== STALE_STATE_NOTICE\) setError\(message\); \}\}/);
  assert.match(panel, /import \{ createMobileGitRequest, STALE_STATE_NOTICE \} from "\.\/mobile-git-request";/);
  assert.match(panel, /<div className="mobile-git-error" role="alert">/);
  assert.match(panel, /<div className="mobile-git-notice" role="status">/);
  assert.match(gitStyles, /\.mobile-git \{[^}]*flex: 1;/s);
  // 负向断言必须对**剥掉注释的正文**做判断：这个文件的注释里就写着"页面级 .mobile-error
  // 在遮罩之下"（那正是它自己那一段存在的理由），直接 includes 会被自己的注释满足。
  const panelCode = panel.replace(/^\s*\/\/.*$/gm, "").replace(/\/\*[\s\S]*?\*\//g, "");
  assert.ok(!panelCode.includes("mobile-error"), "Git 视图不许复用页面级的 .mobile-error");

  // ⑧ ⚠️ 最重要的一条：冲突总览**不许**再静默降级成"没有冲突"。
  //    第一版写的是 `.catch(() => null)`，而并发被拒时被吞掉的恰好是它 ——
  //    症状是"仓库真有冲突，手机端却显示成一个完全正常的仓库"，且时好时坏（竞态）。
  // 同样先剥注释：这个文件的注释里为了说明"为什么删掉它"，把那句原样写了一遍。
  const workbenchCode = workbench.replace(/^\s*\/\/.*$/gm, "").replace(/\/\*[\s\S]*?\*\//g, "");
  assert.doesNotMatch(workbenchCode, /\.catch\(\(\) => null\)/);
  assert.match(workbench, /const \[conflictsState, setConflictsState\] = useState<"loading" \| "ready" \| "unavailable">\("loading"\);/);
  assert.match(workbench, /setConflictsState\(conflictsResult\.ok \? "ready" : "unavailable"\);/);
  // 「读不到」必须显式渲染，且文案与"真的有冲突"那条横幅**不同**（两件事）。
  assert.match(workbench, /conflictsState === "unavailable" && <div className="git-conflict-unavailable" role="status">/);
  assert.match(gitStyles, /\.git-conflict-unavailable \{[^}]*background: #fdf8ec;/s);

  // ⑨ 手机端两级推进：详情开着时列表让位，而不是排在它上面 300px 处（用户会以为点击没生效）。
  assert.match(workbench, /data-mobile=\{mobile \? "true" : undefined\}/);
  assert.match(workbench, /data-detail=\{mobileDetailOpen \? "open" : undefined\}/);
  assert.match(workbench, /const mobileDetailOpen = selectedDiff !== null \|\| conflictPath !== null;/);
  assert.match(workbench, /className="git-mobile-back"/);
  assert.match(gitStyles, /\.git-workbench\[data-mobile="true"\]\[data-detail="open"\] \.git-changes-sidebar \{ display: none; \}/);
  // 那一层是**派生**的，不是独立状态：独立布尔必然有一天与 selectedDiff 不同步，
  // 症状是"返回键吃掉一次、界面上什么都不发生"。
  assert.doesNotMatch(workbenchCode, /const \[mobilePane/);

  // ⑩ diff 软换行。基准样式是 `white-space: pre` + `min-width: max-content`，
  //    手机上每一行都会拖出一屏；这与文件查看器是同一条约定。
  assert.match(gitStyles, /\.git-workbench\[data-mobile="true"\] \.git-diff-line code \{[^}]*white-space: pre-wrap;/s);
  assert.match(gitStyles, /\.git-workbench\[data-mobile="true"\] \.git-diff-line \{[^}]*min-width: 0;/s);

  // ⑪ 手机端不用原生 <select>：操作记录的筛选换成胶囊，且用 aria-pressed 当样式钩子
  //    （不新造裸的 .active 类名）。
  assert.match(workbench, /\{mobile \? <><FilterChips /);
  assert.match(workbench, /function FilterChips\(\{ label, value, options, onChange \}/);
  assert.match(gitStyles, /\.git-ops-chips button\[aria-pressed="true"\] \{/);

  // ⑫ 句柄：里面有没有上一层、以及刷新，都由工作台自己回答（外层猜一个布尔量必然分叉）。
  assert.match(workbench, /export interface GitWorkbenchHandle \{/);
  assert.match(workbench, /showTopLevel: \(\) => boolean;/);
  assert.match(workbench, /reload: \(\) => void;/);
  assert.match(workbench, /export const GitWorkbench = forwardRef<GitWorkbenchHandle, GitWorkbenchProps>/);
  assert.match(panel, /showTopLevel: \(\) => workbenchRef\.current\?\.showTopLevel\(\) \?\? false,/);

  // ⑬ 失败分类**优先用服务端给的稳定错误码**。那句 `error` 会被电脑端本地化
  //    （"project workspace is occupied…" → "项目工作区正被其他 AI 任务或 Git 操作占用…"），
  //    只按原文匹配的分支在真实链路上永远不命中 —— 而单测喂原文照样绿。
  //    服务端的 httpErrorCode 正是为"别把行为耦合到本地化文案上"而存在的。
  assert.match(adapter, /const FAILURE_CODES: Record<string, MobileGitFailureKind> = \{\n\s*workspace_occupied: "workspace_busy",\n\s*\};/);
  assert.match(adapter, /classifyGitFailure\(message, reply\?\.code\)/);
  // 没有码的那些只能匹配文案，所以本地化之后的中文写法也必须留着。
  assert.match(adapter, /\{ kind: "workspace_busy", needle: "项目工作区正被其他 AI 任务或 Git 操作占用" \},/);

  // 没有内容缓存 ⇒ 不该有一个"清缓存"的出口：留着它就是在暗示这里存了东西。
  // （注释里为了说明"为什么不需要它"提到了这个名字，所以对剥掉注释的正文判断。）
  const adapterCode = adapter.replace(/^\s*\/\/.*$/gm, "").replace(/\/\*[\s\S]*?\*\//g, "");
  assert.ok(!adapterCode.includes("invalidateAll"), "Git 适配器没有缓存，不该留 invalidateAll");

  // ⑭ 手机端的排版尺度是**设计参数**，把数值本身钉住（390×844 实测后定的）。
  //    没有这几条，任何一次"顺手统一字号"都能把它们悄悄改回去，而截图不会有人天天看。
  assert.match(gitStyles, /\.git-workbench\[data-mobile="true"\] \.git-diff-code \{ font-size: 13px; \}/);
  // ⚠️ 行号是最容易漏的一条：它继承 `.git-diff-code`，代码一涨它就跟着涨，
  //    而它的盒子是固定列宽 + border-box ⇒ 三位数就会顶出去。
  assert.match(gitStyles, /\.git-workbench\[data-mobile="true"\] \.git-diff-line \.git-diff-number \{ font-size: 11px; \}/);
  assert.match(gitStyles, /\.git-workbench\[data-mobile="true"\] \.git-diff-line \.git-diff-number \{ padding: 0 7px 0 3px; \}/);
  assert.match(gitStyles, /\.git-workbench\[data-mobile="true"\] \.git-change-list b \{ font-size: 13px; \}/);
  assert.match(gitStyles, /\.git-workbench\[data-mobile="true"\] \.git-icon-btn \{ width: 34px; height: 34px; border-radius: 8px; \}/);
  assert.match(gitStyles, /\.git-workbench\[data-mobile="true"\] \.git-ops-chips button \{ padding: 6px 13px; font-size: 13px; \}/);
  assert.match(gitStyles, /\.mobile-git-error, \.mobile-git-notice \{ font-size: 13px; \}/);
  // 提交面板在手机上要**排到最后**（DOM 里它在两组列表中间，桌面两栏并置时才对）。
  // 这条同时是行为了：它在单列布局里决定"改了什么"要不要多滑一屏才看得到。
  assert.match(gitStyles, /\.git-workbench\[data-mobile="true"\] \.git-changes-sidebar > \.git-commit-panel \{ order: 1; \}/);

  // ⑮ 冲突解决视图里 AI 模型那一项：**手机端必须是胶囊**。
  //    原生 `<select>` 的弹层由系统绘制、样式一行都管不到 —— 这条是本项目手机端的硬规则
  //    （操作记录的筛选栏当初就是为它换成胶囊的），这一处是上一轮漏网的。
  //    结构上钉住两点：只有**一个** `<select`，且它排在胶囊那一支**之后**（= 它是 else 支）。
  const conflictView = normalize(await readFile(new URL("./features/git/ConflictSolveView.tsx", import.meta.url), "utf8"));
  assert.match(conflictView, /mobile\?: boolean;/);
  assert.match(conflictView, /const AI_AGENT_OPTIONS = \[\{ value: "claude-code", label: "Claude" \}, \{ value: "codex", label: "Codex" \}\] as const;/);
  assert.match(conflictView, /<div className="git-conflict-ai-agents">/);
  assert.equal((conflictView.match(/<select /g) ?? []).length, 1, "AI 模型只该留一个原生 select（桌面端那一支）");
  assert.ok(
    conflictView.indexOf('className="git-conflict-ai-agents"') < conflictView.indexOf("<select "),
    "手机端的胶囊分支必须排在桌面端的 <select 之前（否则说明条件写反了）",
  );
  assert.match(gitStyles, /\.git-workbench\[data-mobile="true"\] \.git-conflict-ai-agents button\[aria-pressed="true"\] \{/);

  // ⑯ 提交历史必须有「读不到」这一档。
  //    原来只有「正在读取」与「没有记录」两态 ⇒ `git.log` 失败时列表区写的是
  //    "该分支没有可显示的提交记录" —— 一句关于**仓库事实**的断言，而真相是**我们没读到**。
  //    提交详情那侧一直有 error 档，历史列表漏了，是不对称。空态有下一步动作，所以走卡片。
  assert.match(workbench, /const \[historyError, setHistoryError\] = useState\(""\);/);
  assert.match(workbench, /if \(error !== "" && commits\.length === 0\) return <div className="git-history-error" role="status">/);
  assert.match(workbench, /<CommitHistory commits=\{branchCommits\} loading=\{historyLoading\} error=\{historyError\} onSelect=\{openCommit\} \/>/);
  assert.match(gitStyles, /\.git-history-error \{ display: grid;/);  // 「真的没有提交」那句要留着（只有真没有时才说），且**排在** error 档之后 —— 顺序反了就是两句话换了意思。
  assert.ok(
    workbench.indexOf('className="git-history-error"') < workbench.indexOf("该分支没有可显示的提交记录"),
    "「读不到」那一档必须排在「没有提交记录」之前",
  );

  // ⑰ 子态顶栏的分支胶囊只在**文件**子态出现。
  //    Git 子态自己的顶栏第一行就是分支名 + 同步状态，胶囊与它相隔 60px 再说一遍"main"
  //    （第一版两处都留了，读起来就是重复）。文件子态没有这个信息，所以那边留着。
  assert.match(page, /\{filesHeaderActive && project\?\.gitBranch && <span className="mobile-files-workspace"/);

  // ⑱ 左基线：手机端只允许两条 —— 外 24（卡片 / 顶栏内容 / 页签）与内 39（卡片里的标题与标签）。
  //    实测过一次出现三条（外壳 12 / 顶栏 24 / 卡片 28），"乱"主要就是它。
  assert.match(gitStyles, /\.git-workbench\[data-mobile="true"\] \.git-workbench-body \{ padding-right: 12px; padding-left: 12px; \}/);
  assert.match(gitStyles, /\.git-workbench\[data-mobile="true"\] \.git-change-list > div > button:first-child \{ padding: 12px 13px 12px 14px; \}/);
  assert.match(gitStyles, /\.git-workbench\[data-mobile="true"\] \.git-commit-panel \{ padding: 14px; \}/);
  // ⚠️ 外层容器在手机端被收掉之后，边框必须补回给里面的卡片：
  //    基准样式里 `.git-changes-content > .git-diff` 与 `.git-conflict-solve` 都是 `border: 0`
  //    （故意让外层提供那一圈）。只删外层不补里层，详情卡片会整片"没有边" ——
  //    两边都在源码里，断言查不出来，是看截图发现的。
  assert.match(gitStyles, /\.git-workbench\[data-mobile="true"\] \.git-changes-content \{ min-height: 0; border: 0;/);
  assert.match(gitStyles, /\.git-workbench\[data-mobile="true"\] \.git-changes-content > \.git-diff,\n\.git-workbench\[data-mobile="true"\] \.git-changes-content > \.git-conflict-solve \{ overflow: hidden; border: 1px solid #d5e4dc;/);

  // ⑲ 图标按钮：**固定宽高与内边距同时命中会被压扁**。
  //    基准里 `.git-refresh/.git-ops-trigger` 是 `padding: 0`，但 ① 段那条宽选择器
  //    `.git-bar-actions button { padding: 10px 14px }` 特异性更高 ⇒ 必须在 ④ 段显式压回。
  //    实测症状是 svg 被压成 10×16（**等比断言能抓到，溢出断言抓不到**）。
  //    圆角同理：8px 要自己声明，否则退回基准的 6px，与同屏的 `.git-icon-btn` 不一致。
  assert.match(gitStyles, /\.git-workbench\[data-mobile="true"\] \.git-bar-actions \.git-refresh,\n\.git-workbench\[data-mobile="true"\] \.git-bar-actions \.git-ops-trigger \{ width: 40px; height: 40px; flex: 0 0 40px; padding: 0; border-radius: 8px; \}/);
  // ⑳ `.git-mobile-back` 只能有**一条**定义：曾经三条互相覆盖，留下"padding 被盖掉、
  //    min-height 仍生效"的半死状态（读代码的人会以为 padding 还是 8px 14px）。
  const mobileBackRules = gitStyles.match(/\.git-mobile-back \{/g) ?? [];
  assert.equal(mobileBackRules.length, 1, `.git-mobile-back 应只有一条定义，实际 ${mobileBackRules.length} 条`);

  // ⑬ 适配器的三条硬性质：写操作换新令牌、超时分档、路由表按"字面先于占位符"匹配。
  assert.match(adapter, /if \(parsed\.route\.needsStateToken\) \{/);
  assert.match(adapter, /const token = await fetchStateToken\(\);/);
  assert.match(adapter, /const LOCAL_WRITE_TIMEOUT_MS = 45_000;/);
  assert.match(adapter, /const NETWORK_WRITE_TIMEOUT_MS = 60_000;/);
  assert.match(adapter, /\{ pattern: "\/commits\/amend",[\s\S]*?\n\s*\{ pattern: "\/commits",/);
  // 令牌换取失败必须原样失败，不能"换不到就拿旧的硬发"（那会拿到一个指不到原因的 409）。
  assert.match(adapter, /readonly code: "operation_failed" \| "wiring" \| MobileGitFailureKind;/);
});
