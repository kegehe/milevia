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
// 全局样式表。扫码取景层的清底规则必须与它对齐：`style.css` 给根元素上了不透明底色，
// 那正是"只清 body 不够用"的前提（见扫码那条用例）。
const rootStyles = normalize(await readFile(new URL("./style.css", import.meta.url), "utf8"));

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
  const listOpen = page.indexOf('<div className="mobile-message-list">');
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
  // "正文或已引用技能"有其一，发送键就该可用（只点了一颗技能、一个字没写也能发出去）。
  assert.match(page, /<button type="submit" className="mobile-composer-send" disabled=\{busy \|\| !conversation \|\| \(!messageDraft\.trim\(\) && skillRefs\.length === 0\)\}/);
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
  assert.match(page, /const content = composeSkillMessage\(draft, skillRefs\);/);
  assert.match(page, /setMessageDraft\(draft\);/);
  assert.match(page, /setMessageDraft\(\(current\) => current\.trim\(\) \? current : draft\);/);
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
  assert.match(page, /if \(height > lastHeight && stickToBottomRef\.current\) \{\s*window\.scrollTo\(\{ top: document\.documentElement\.scrollHeight \}\);\s*\}/);
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
  assert.match(page, /data-mobile-task-delete/);
  assert.match(page, /remove\.disabled = busy \|\| task\.status === "running"/);
  assert.match(page, /edit\.hidden = !canEdit/);
  assert.match(page, /任务描述不能为空/);
});

test("mobile pairing QR URLs contain only the pairing identifier", () => {
  assert.match(page, /function pairingURLWithCode\(value: string \| undefined, pairingID: string\): string/);
  assert.match(page, /parsed\.searchParams\.set\("pairingId", pairingID\);/);
  assert.match(page, /parsed\.searchParams\.delete\("code"\);/);
  assert.match(page, /pairingURLWithCode\(value\.pairingURL, value\.pairingId \|\| ""\)/);
  assert.doesNotMatch(page, /pairingURLWithCode\([^\n]*value\.code/);
});

test("mobile QR scans without an embedded code fall back to manual verification", () => {
  assert.match(page, /setPairingID\(scanned\.pairingID\);/);
  assert.match(page, /setPairingCode\(scanned\.code \|\| ""\);/);
  assert.match(page, /if \(\/\^\\d\{6\}\$\/\.test\(scanned\.code\)\)[\s\S]*?claimPairing\(scanned\.pairingID, scanned\.code\)/);
  assert.match(page, /setPairingStatus\("[^"]*6[^"]*"\);/);
  assert.match(page, /mobile-pairing-manual/);
  assert.match(page, /\/v1\/pairings\/claim/);
});

test("mobile conversations render markdown and project cards expose keyboard activation", () => {
  assert.match(page, /<ReactMarkdown remarkPlugins=\{\[remarkGfm\]\}/);
  assert.match(page, /className="mobile-message-markdown markdown"/);
  assert.match(page, /className=\{`mobile-project \$\{project\?\.id === item\.id \? "selected" : ""\}`\}[^>]*role="button"[^>]*tabIndex=\{0\}/);
  assert.match(page, /if \(event\.key === "Enter" \|\| event\.key === " "\)/);
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
  //   min(390px, 100vw-22px) 宽；② 头部的"全部/待处理/执行中/待验收/需处理/已完成/已取消"
  //   7 个分类横排一行需要 577px（实测每个胶囊 67~79px、间距 6px），只能换行成三行，
  //   或者横向滑动才能看全。
  // 修法：整页弹层 + 分类栏 7 等分一行（原来还有"卡片折叠状态下大片空白"、"删除"孤零零
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
  assert.match(styles, /\.mobile-task-panel:focus \{ outline: none; \}/);
  // 面板的 padding 必须分四条写：不支持 env() 的环境（老 WebView / 部分桌面浏览器）会把含 env()
  // 的**整条声明**丢掉——写成一条就等于完全没有 padding、内容贴死屏幕四边（docs/33 记过这个坑，
  // 页面其它 7 处 env() 也都是这个写法）。
  assert.match(styles, /\.mobile-task-panel \{[^}]*padding: 12px 14px; padding-top: max\(12px, env\(safe-area-inset-top\)\);/s);
  assert.match(styles, /\.mobile-task-panel-body \{[^}]*flex: 1;[^}]*min-height: 0;[^}]*overflow-y: auto;/s);
  // 7 个分类等分一行：等宽 grid（宽度瓶颈是 3 个中文字，不许换行、也不许横向滚动找）。
  assert.match(styles, /\.mobile-task-filters \{[^}]*grid-template-columns: repeat\(7, minmax\(0, 1fr\)\);/s);
  assert.doesNotMatch(styles, /\.mobile-task-filters \{ display: flex;/);
  assert.doesNotMatch(styles, /\.mobile-task-filters \{[^}]*flex-wrap: wrap;/s);
  assert.doesNotMatch(styles, /\.mobile-task-filters \{[^}]*overflow-x: auto/s);
  // 标签与计数上下两行堆叠，宽度才只由标签决定（横排时"标签 + 计数"要 50px，320px 屏每格只有 37px）。
  assert.match(styles, /\.mobile-task-filters button \{ display: grid; min-width: 0; min-height: 48px; place-content: center;/);
  // 320px 屏每格 37px，12px 的三字标签是 36px 会顶到格边，必须降一档字号。
  assert.match(styles, /@media \(max-width: 359px\) \{\s*\.mobile-task-filters button \{ font-size: 11px; \}/);
  // 卡片：面板正文在宽屏下也被 padding-inline 限宽到 560px，所以永远纵向排列——不能按视口宽度切
  //   （>620px 视口下走横向布局，标题会被右侧按钮挤成窄条）。
  assert.match(styles, /\.mobile-task-panel \.mobile-task \{ flex-direction: column;/);
  // 面板有浅绿底，卡片改成白底圆角块（原来靠一条底边框分隔，整页宽度下看不清每张卡片的起止）。
  assert.match(styles, /\.mobile-task-panel \.mobile-task \{[^}]*border-radius: 12px;[^}]*background: #fff;/s);
  // 状态胶囊与优先级并到同一行（原来三个元素各占一行，折叠状态下白白多出两行高度）。
  assert.match(styles, /\.mobile-task-panel \.mobile-task > div:first-child \{[^}]*grid-template-columns: auto minmax\(0, 1fr\);/s);
  // 摘要行改 flex（文字垂直居中、箭头靠右）并把 52px 收到 44px——原来文字贴在 52px 高的顶边，
  // 下面整块是空白，就是用户说的"折叠状态下还有很多空白"。
  assert.match(styles, /\.mobile-task-disclosure summary \{[^}]*display: flex;[^}]*min-height: 44px;/s);
  // 操作行与"查看详情"分两行：details 必须排在 actions 之后，"删除"才不会看起来挂在详情开关旁。
  const actionsAt = page.indexOf('className="mobile-task-actions"');
  const detailsAt = page.indexOf('<details className="mobile-task-disclosure">');
  assert.ok(actionsAt > 0 && detailsAt > actionsAt, "任务卡片的「查看详情」应排在操作行之后");
  // 编辑/删除按钮是 imperative 代码按行序插进 .mobile-task-actions 的：选择器一旦与面板类名
  // 漂移（改名漏改一处），这两个按钮会整批消失，而界面看起来只是"这些任务刚好不能编辑"。
  assert.match(page, /document\.querySelectorAll<HTMLElement>\("\.mobile-task-panel \.mobile-task"\)/);
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

  // 标题栏放的是**会话名**。rawTitle 不带「· Agent」后缀：320px 上多这一截就会被省略号吃掉，
  // 而"我正跟哪个 Agent 说话"挪进 ⋯ 菜单的信息行更合适。
  assert.match(page, /rawTitle: item\.title \|\| "未命名会话",/);
  assert.match(page, /conversation\?\.rawTitle \|\| project\?\.name \|\| "项目对话"/);
  // 历史列表继续用带后缀的 title —— 那里一行只有标题，多这一截反而有用，别一起改掉。
  assert.match(page, /title: `\$\{item\.title \|\| "未命名会话"\} · \$\{conversationAgentLabel\(item\.agentId\)\}`/);
  // 会话里不画品牌方块：把横向空间让给更长的会话名（项目列表视图不受影响）。
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
  assert.match(page, /\{conversationHeaderActive && project && <div className="mobile-header-menu">/);
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
  // 视图切换只能由这三个 helper 发起：三个进入点（点项目卡、新建会话成功、SSE 事件）都要走
  // enterConversationView，否则会出现"手势要退两层"或"压了不认"的不一致。
  // 用 >= 而不是 ==：将来**正确地**新增进入点（也调 helper）不该把测试判红，否则只会教人改数字；
  // 真正的不变式是下面那两条——setMobileView 只允许出现在两个 helper 里各一次，绕过 helper 必红。
  assert.ok((page.match(/enterConversationView\(\);/g) ?? []).length >= 3, "三个进入点都要走 enterConversationView");
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
  const listOpen = page.indexOf('<div className="mobile-message-list">');
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
  // 用户重进页面就只看到"运行中"却不知道卡在等确认。这条断言锁住后端的类型过滤。
  const remote = await readFile(new URL("../../../apps/control-server/internal/app/remote_control.go", import.meta.url), "utf8");
  assert.match(remote, /const remoteNoticeTypesSQL = `type in \('system','run\.failed','run\.interrupted','error','turn\.failed','stream\.error'\) or type like 'approval\.%'`/);
  assert.match(remote, /where conversation_id=\? and \(%s\) order by created_at desc,id desc limit \?`, remoteNoticeTypesSQL\)/);
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
  //      刷多少次都不会变，等于没有证据。
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

  const refreshBody = page.match(/async function refreshNow\(\) \{[\s\S]*?\n  \}\n\n  function saveToken/)?.[0] ?? "";
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
  assert.match(styles, /\.mobile-create button:disabled, \.mobile-task-actions button:disabled \{ opacity: \.5; \}/);
  assert.doesNotMatch(styles, /\.mobile-refresh:disabled,/);
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
  assert.match(page, /<button className="mobile-pairing-generate" onClick=\{beginPairingScan\} disabled=\{busy \|\| scanning\}>扫描二维码<\/button>/);
  assert.doesNotMatch(page, /onClick=\{\(\) => \{ scanAccepted\.current = false;/);
  // 浮层清退两处都要收：① 安卓返回键的 if 链；② leaveConversationView（popstate / 侧滑不走 ①）。
  // 漏 ② 的症状：带着一个 position: fixed 的取景层退回项目列表，摄像头还一直开着。
  assert.match(page, /if \(scanning \|\| scanError\) \{ closeScanOverlay\(\); return true; \}/);
  // 失败态必须留在取景层里（scanning 已经是 false，只按 scanning 渲染就会"闪一下消失"）。
  assert.match(page, /<strong>\{scanning \? "将电脑上的二维码放入框内" : "扫码没有成功"\}<\/strong>/);
  assert.match(page, /<button className="mobile-scan-retry" type="button" onClick=\{beginPairingScan\}>重新扫描<\/button>/);
  assert.match(styles, /\.mobile-scan-retry \{[^}]*background: #8ce7b0;/);
  assert.match(page, /function leaveConversationView\(\) \{[\s\S]*?closeScanOverlay\(\);\s*\}/);
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
