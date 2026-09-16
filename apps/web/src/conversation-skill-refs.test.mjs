import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

// 点技能 = 挂一颗可删除的引用胶囊，发送那一刻才展开成完整引用指令。
//
// 修的是什么：旧实现把「请使用技能 <name>：描述」整段 setComposerText 进输入框。
// 技能描述动辄上百字，输入框被铺满三行；更要命的是它是**整体覆盖**，
// 用户写到一半的草稿会被无声吃掉。改成胶囊之后，"输入框里显示什么"与"实际发什么"分开了，
// 所以下面每一处"写回输入框"的路径都必须只回 draft（用户正文），不回 content（含引用的上线文本）——
// 回错一处，整段技能描述就会重新铺满输入框，这次修复等于没做。
//
// 换行先归一化成 LF（仓库 core.autocrlf=true，工作区是 CRLF，带 \r 的锚点会静默失配）。
const normalize = (text) => text.replace(/\r\n/g, "\n");
const conversationPage = normalize(await readFile(new URL("./pages/ConversationPage.tsx", import.meta.url), "utf8"));
const mobilePage = normalize(await readFile(new URL("./pages/MobileRemotePage.tsx", import.meta.url), "utf8"));
const stylesheet = normalize(await readFile(new URL("./conversation.css", import.meta.url), "utf8"));
const mobileStyles = normalize(await readFile(new URL("./pages/mobile-remote.css", import.meta.url), "utf8"));

test("clicking a skill attaches a reference chip instead of overwriting the draft", () => {
  // 断言锁在 useSkill 的函数体内：文件里还有别的 addSkillRef 调用点，
  // 用 `[\s\S]*?` 跨过去的话"点技能没挂胶囊"也能被别处满足。
  const useSkillBody = conversationPage.match(/const useSkill = \(skill: Skill\) => \{[\s\S]*?\n  \};/)?.[0] ?? "";
  assert.ok(useSkillBody, "找不到 useSkill");
  assert.match(useSkillBody, /addSkillRef\(skill\)/);
  assert.doesNotMatch(useSkillBody, /setComposerText|mergeSkillPrompt/);
  assert.match(conversationPage, /const addSkillRef = \(skill: Skill\) => \{[\s\S]*?setSkillRefs\(/);
  assert.match(conversationPage, /const removeSkillRef = \(skill: Skill\) => \{/);
  // 同名同来源只挂一次：连点同一颗技能不该冒出两颗一模一样的胶囊。
  assert.match(conversationPage, /current\.some\(\(item\) => item\.name === skill\.name && item\.source === skill\.source\) \? current : \[\.\.\.current, skill\]/);
  // 手机端同一条规则（用它自己的 RemoteSkill 形状）。
  const fillSkillBody = mobilePage.match(/function fillSkill\(skill: RemoteSkill\) \{[\s\S]*?\n  \}/)?.[0] ?? "";
  assert.ok(fillSkillBody, "找不到 fillSkill");
  assert.match(fillSkillBody, /setSkillRefs/);
  assert.doesNotMatch(fillSkillBody, /fillComposer|skillPrompt/);
});

test("the reference is expanded only at send time, and both ends agree on the text", () => {
  // 拼接规则：引用在前、正文在后、中间空一行；只引用技能、一个字没写也允许发出去。
  assert.match(conversationPage, /const composeSkillMessage = \(text: string, refs: Skill\[\]\) => \{/);
  assert.match(conversationPage, /const body = refs\.map\(mergeSkillPrompt\)\.join\("\\n"\);/);
  assert.match(conversationPage, /return text\.trim\(\) \? `\$\{body\}\\n\\n\$\{text\.trim\(\)\}` : body;/);
  assert.match(conversationPage, /const content = composeSkillMessage\(draft, refs\);/);
  assert.match(conversationPage, /if \(!draft && refs\.length === 0\) return false;/);
  // 点发送这条路径必须真的把引用带上（漏了它就是"输入框里有胶囊、发出去却没有引用"）。
  assert.match(conversationPage, /const send = \(event: FormEvent\) => \{\s*event\.preventDefault\(\);\s*setShowSendMenu\(false\);\s*void sendContent\(text, true, \{ skillRefs \}\);\s*\};/s);
  // 两端必须逐字一致：同一个技能不能因为"在手机上点的"就让 Agent 收到另一句指令。
  // headless 通道（-p --output-type stream-json）不解析交互式斜杠命令，所以发出去的
  // 只能是这句自然语言引用 —— 能改的只有显示层，这条断言就是那道闸门。
  const desktopPrompt = conversationPage.match(/const mergeSkillPrompt = \(skill: Skill\) =>[\s\S]*?;\n/)?.[0] ?? "";
  const mobilePrompt = mobilePage.match(/function skillPrompt\(skill: RemoteSkill\): string \{[\s\S]*?\n\}/)?.[0] ?? "";
  assert.ok(desktopPrompt && mobilePrompt, "找不到两端的技能文案");
  for (const line of [/请使用技能 <\$\{skill\.name\}>：\$\{skill\.description\}/, /请使用技能 <\$\{skill\.name\}>/]) {
    assert.match(desktopPrompt, line);
    assert.match(mobilePrompt, line);
  }
  // 手机端也走同一条拼接规则。
  assert.match(mobilePage, /function composeSkillMessage\(text: string, refs: RemoteSkill\[\]\): string \{/);
  assert.match(mobilePage, /const content = composeSkillMessage\(draft, skillRefs\);/);
});

test("every write-back path keeps the draft and the reference separate", () => {
  // 失败回填只许回 draft。
  assert.match(conversationPage, /const draft = rawContent\.trim\(\);/);
  assert.match(conversationPage, /setComposerText\(draft, conversationID\);\s*setSkillRefs\(refs\);/s);
  assert.match(mobilePage, /setMessageDraft\(draft\);\s*setSkillRefs\(skillRefs\);/s);
  assert.match(mobilePage, /setMessageDraft\(\(current\) => current\.trim\(\) \? current : draft\);/);
  // 撤回 / 上箭头重发恢复的是"用户自己写的东西"，不是拼好的上线文本。
  assert.match(conversationPage, /pendingUserDrafts\.current\.set\(data\.runId, draft\)/);
  assert.match(conversationPage, /if \(draft\) appendInputHistory\(draft\);/);
  // /resume 不会发出去（只是打开历史弹窗），但它同样清空输入框 —— 引用要跟着清，
  // 否则用户关掉弹窗后剩一颗不知道从哪来的孤儿胶囊。
  assert.match(conversationPage, /if \(draft === "\/resume"\) \{[\s\S]{0,420}setSkillRefs\(\[\]\);[\s\S]{0,80}openConversationHistory\(\);/);
  const desktopWritesBackContent = /setComposerText\(content, conversationID\)/.test(conversationPage);
  const mobileWritesBackContent = /setMessageDraft\(content\)/.test(mobilePage);
  assert.equal(desktopWritesBackContent, false, "桌面端有一处把含引用的上线文本写回了输入框");
  assert.equal(mobileWritesBackContent, false, "手机端有一处把含引用的上线文本写回了输入框");
});

test("scheduled send carries the references through the queue", () => {
  assert.match(conversationPage, /const pendingSendSkillRefsRef = useRef<Skill\[\]>\(\[\]\);/);
  assert.match(conversationPage, /pendingSendSkillRefsRef\.current = skillRefs;/);
  assert.match(conversationPage, /if \(\(!content && skillRefs\.length === 0\) \|\| !conversation\) return;/);
  assert.match(conversationPage, /const refs = pendingSendSkillRefsRef\.current;/);
  assert.match(conversationPage, /skillRefs: refs \}\)/);
  assert.match(conversationPage, /if \(!conversationID \|\| \(!draft && refs\.length === 0\) \|\| !clientRequestId\) return false;/);
  // 取消预约：正文回输入框，引用回胶囊（不展开成长文本）。
  assert.match(conversationPage, /setComposerText\(existing \? `\$\{existing\}\\n\$\{pending\}` : pending, conversationID\);\s*setSkillRefs\(refs\);/s);
  // 所有"清掉预约"的出口都要连引用一起清，否则下一轮预约会带上失效引用。
  assert.ok((conversationPage.match(/pendingSendSkillRefsRef\.current = \[\];/g) ?? []).length >= 4, "预约链路里有出口没有清掉技能引用");
  // 只引用技能时 content 是空串，预约条仍要显示（真值判断会让它整条消失、取消不掉）。
  assert.match(conversationPage, /pendingSendContent !== null && <div className="composer-pending"/);
});

test("skill references never leak across conversations", () => {
  // 桌面端两道：resetConversationView 里显式清一次（主路径），外加 conversation.id 变化兜底 ——
  // 本组件还有"URL 没带 id → 直接 setConversation(next)"那条不走 reset 的路，
  // 以及将来新增的切换入口，逐个补一次迟早在某一处漏掉。
  assert.match(conversationPage, /setComposerText\(nextDraft, next\.id\);[\s\S]{0,300}setSkillRefs\(\[\]\);/);
  assert.match(conversationPage, /useEffect\(\(\) => \{ setSkillRefs\(\[\]\); \}, \[conversation\?\.id\]\);/);
  // 手机端：清引用与清草稿同处一个 effect（换项目 / 换会话），别让它孤零零挂在别处 ——
  // 分成两个 effect 的话，以后删掉任何一个都能让"引用跨会话漂移"重新出现。
  const mobileResetBody = mobilePage.match(/useEffect\(\(\) => \{\s*setMessageDraft\(""\);[\s\S]*?\n  \}, \[selectedProject, selectedConversation\]\);/)?.[0] ?? "";
  assert.ok(mobileResetBody, "找不到手机端换会话时清草稿的 effect");
  assert.match(mobileResetBody, /setSkillRefs\(\[\]\);/);
});

test("the chip renders on both ends and is actually removable", () => {
  // 容器用 role="group" 而不是 role="list"：这一行里除了胶囊还有一句提示文字，
  // list 的直接子元素必须是 listitem，多一个 span 就是 a11y 违规。
  assert.match(conversationPage, /className="composer-skill-refs" role="group"/);
  assert.match(conversationPage, /className="composer-skill-ref-remove"[^>]*onClick=\{\(\) => removeSkillRef\(skill\)\}/);
  assert.match(mobilePage, /className="mobile-skill-refs" role="group"/);
  assert.match(mobilePage, /className="mobile-skill-ref-remove"[^>]*onClick=\{\(\) => removeSkillRef\(skill\)\}/);
  // 手机端的 ×：视觉 28px + ::after 外扩 8px 补到 44 的触控热区。
  // 两条都必须带 `.mobile-composer` 前缀 —— `.mobile-composer button` 是"类 + 元素"，
  // 特异性比单个类高，不带前缀就会被 38px 的方块尺寸盖掉。
  assert.match(mobileStyles, /\.mobile-composer \.mobile-skill-ref-remove\s*\{[^}]*width:\s*28px;[^}]*height:\s*28px;/s);
  assert.match(mobileStyles, /\.mobile-composer \.mobile-skill-ref-remove::after\s*\{[^}]*inset:\s*-8px;/s);
  // 引用行要能占满一整行（flex 容器因此需要 wrap），否则会跟输入框盒子挤在同一行。
  assert.match(mobileStyles, /\.mobile-composer\s*\{[^}]*flex-wrap:\s*wrap;/s);
  assert.match(mobileStyles, /\.mobile-skill-refs\s*\{[^}]*flex:\s*0 0 100%;/s);
  // 桌面端：长技能名截断而不是把胶囊撑破。
  assert.match(stylesheet, /\.composer-skill-refs\s*\{[^}]*flex-wrap:\s*wrap;/s);
  assert.match(stylesheet, /\.composer-skill-ref-remove:hover:not\(:disabled\)/);
  assert.match(stylesheet, /\.composer-skill-ref-name > span\s*\{[^}]*text-overflow:\s*ellipsis;/s);
});
