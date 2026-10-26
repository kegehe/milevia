import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

// 粘贴超长文本 → 内联卡片（WorkBuddy 形态）。
// 显示层是卡片，发送由 serializeComposer 还原为完整原文 —— 下面每条断言都在钉
// 「显示什么」与「发什么」的这条分界线，以及用户点名要的两个细节：
// 悬浮弹窗看原文、卡片与前后文字之间留有空隙。
//
// 换行先归一化成 LF（仓库 core.autocrlf=true，CRLF 锚点会静默失配）。
const normalize = (text) => text.replace(/\r\n/g, "\n");
const page = normalize(await readFile(new URL("./pages/ConversationPage.tsx", import.meta.url), "utf8"));
const lib = normalize(await readFile(new URL("./lib/composer-paste.ts", import.meta.url), "utf8"));
const stylesheet = normalize(await readFile(new URL("./conversation.css", import.meta.url), "utf8"));

test("paste handling lives in one judge and the page consumes it", () => {
  // 阈值判据只有一份（lib），页面里不许出现第二份字面量。
  assert.match(lib, /export const PASTE_FOLD_THRESHOLD = \d+;/);
  assert.match(lib, /export function shouldFoldPaste\(text: string\): boolean \{\s*return text\.length > PASTE_FOLD_THRESHOLD;/s);
  assert.doesNotMatch(page, /> ?800|800 ?字/);
  // 页面走 lib 的函数分流，不自己量长度。
  const pasteBody = page.match(/const handleComposerPaste = \(event: React\.ClipboardEvent<HTMLDivElement>\) => \{[\s\S]*?\n  \};/)?.[0] ?? "";
  assert.ok(pasteBody, "找不到 handleComposerPaste");
  assert.match(pasteBody, /shouldFoldPaste\(pasted\)/);
  assert.match(pasteBody, /event\.preventDefault\(\)/);
  assert.match(pasteBody, /insertPasteCardAtSelection\(el, pasted\)/);
  assert.match(pasteBody, /serializeComposer\(el\)/);
  // 富文本粘贴必须拦成 text/plain，否则样式标签会进序列化口径。
  assert.match(pasteBody, /getData\("text\/plain"\)/);
});

test("serialization is the only boundary between the card and the sent content", () => {
  assert.match(lib, /export function serializeComposer\(root: ComposerLikeNode\): string \{/);
  // 卡片还原为 data.full；读不到时是空串而不是把 DOM 内部串出去。
  assert.match(lib, /const full = node\.dataset\?\.full \?\? "";/);
  // 输入路径每次按键都把 DOM 序列化回草稿（发送、历史、技能拼接全部吃这一份）。
  assert.match(page, /const handleComposerInput = \(event: React\.FormEvent<HTMLDivElement>\) => \{[\s\S]*?handleTextChange\(serializeComposer\(el\)\);/s);
  // 卡片 ✕ 的移除不产生 React input 事件，必须另有重算入口；处理器经 ref 转发
  //（直接闭包进 [] effect 会把首帧带旧 projectId 的 setComposerText 钉死，跨项目写串草稿）。
  assert.match(page, /addEventListener\("composer-paste-changed", onChanged\)/);
  assert.match(page, /pasteChangedRef\.current = \(\) => \{[\s\S]*?handleTextChange\(serializeComposer\(el\)\);/s);
  // text → DOM 的同步 effect：同一会话内一致时绝不动 DOM（否则光标跳动）。
  // 比对用 textRef.current：passive effect 异步调度，比闭包 state 会把快速打字误判成外部覆盖。
  // 会话 id 变化是无条件重写的例外（两份草稿可能恰好同串，旧卡片不能残留到新会话）。
  assert.match(page, /if \(composerSyncedConversationRef\.current === conversation\?\.id && serializeComposer\(el\) === textRef\.current\) return;/);
  assert.match(page, /el\.textContent = textRef\.current;/);
  assert.match(page, /\}, \[text, conversation\?\.id\]\);/);
});

test("the hover popup shows the original text and scrolls for long pastes", () => {
  // 浮层挂在 body（fixed）：编辑区是 overflow:auto 滚动容器，卡片内的 absolute 浮层会被裁掉。
  assert.match(lib, /document\.body\.appendChild\(activePop\)/);
  assert.match(lib, /activePop\.style\.top = rect\.top > height \+ 24 \? `\$\{rect\.top - height - 10\}px` : `\$\{rect\.bottom \+ 10\}px`;/);
  // 离开卡片不立刻收（延迟收），鼠标移进浮层滚动时不算离开。
  assert.match(lib, /popHideTimer = window\.setTimeout\(hidePastePop, 120\)/);
  assert.match(lib, /pop\.addEventListener\("mouseenter", \(\) => window\.clearTimeout\(popHideTimer\)\)/);
  // fixed 定位不跟卡片走：滚动/改窗必须收掉，否则是指着空处的孤儿浮层。
  // 但浮层自己 body 里的滚动必须放行（capture 会抓到它）—— 那是「长文可滚动查看」本体。
  assert.match(lib, /window\.addEventListener\("scroll", onWindowScrollHide, true\)/);
  assert.match(lib, /window\.addEventListener\("resize", onWindowScrollHide, true\)/);
  assert.match(lib, /if \(activePop && event\.target instanceof Node && activePop\.contains\(event\.target\)\) return;/);
  // 键盘出口：Esc 关浮层，随 hidePastePop 摘除监听（不泄漏）。
  assert.match(lib, /window\.addEventListener\("keydown", onPopEscapeHide\)/);
  assert.match(lib, /window\.removeEventListener\("keydown", onPopEscapeHide\);/);
  // 锚卡片被键盘/拖拽删除（非 ✕ 路径）时由输入路径兜底收掉。
  assert.match(lib, /export function hidePastePopIfDetached\(\)/);
  assert.match(page, /hidePastePopIfDetached\(\)/);
  // 卸载清理：浮层是 body 上的单例，跨页面不能残留。
  assert.match(page, /el\.removeEventListener\("composer-paste-changed", onChanged\);\s*hidePastePop\(\);/s);
  assert.match(stylesheet, /\.paste-pop-body \{[\s\S]*max-height: 320px;[\s\S]*overflow-y: auto;/s);
  // 浮层让位模态背板（style.css 的 .backdrop 是 z-index: 30）。
  assert.match(stylesheet, /\.paste-pop \{[^}]*z-index: 25;/s);
});

test("the inline card keeps breathing room and is removable", () => {
  // 卡片与前后文字的空隙（用户点名）：左右各 5px。
  assert.match(stylesheet, /\.composer \.paste-card \{[^}]*margin: 0 5px;/s);
  // 长文件名/长标题截断而不是撑破输入框。
  assert.match(stylesheet, /\.composer \.paste-card-text \{[^}]*text-overflow: ellipsis;/s);
  // ✕ 是 type="button"（卡片在 form 里，漏了会触发提交）。
  assert.match(lib, /'<button type="button" class="paste-card-x" title="移除粘贴的文本" aria-label="移除粘贴的文本">×<\/button>'/);
  // 移除键红色沿用 composer-icon-btn.danger 的 #ad4835 系，不另造暖色。
  assert.match(stylesheet, /\.composer \.paste-card-x \{[^}]*color: #ad4835;/s);
  // 删除通知必须 dispatch 到还挂在 DOM 里的祖先：对已摘除的卡片 dispatch（哪怕 bubbles）
  // 传播路径只有它自己，编辑器上的监听器永远收不到 —— 草稿就漏掉这次删除。
  assert.match(lib, /const parent = card\.parentNode;\s*card\.remove\(\);\s*\(parent \?\? card\)\.dispatchEvent\(/s);
  // 只读/禁用态不许删卡片（编辑器 data-disabled 守卫 + 指针事件兜底）。
  assert.match(lib, /card\.closest\("\[data-disabled\]"\)/);
  assert.match(stylesheet, /\.composer \.composer-edit\[data-disabled\] \.paste-card-x \{[^}]*pointer-events: none;/s);
});

test("the editor is a contenteditable div with a CSS-driven placeholder", () => {
  assert.match(page, /ref=\{composerEditRef\}\s*\n\s*className="composer-edit"\s*\n\s*contentEditable=/s);
  // 占位符走 :empty::before；空态归一化只针对「子节点全是 BR」（孤立 <br> 残留），
  // 不许吞掉用户敲的空白字符；IME 组合期间不做 DOM 归一化（打断选字）。
  assert.match(stylesheet, /\.composer \.composer-edit:empty::before \{ content: attr\(data-placeholder\);/);
  // 空态归一化判据在 lib（isComposerBlankResidue）：BR 与只含 BR 的空 div 都是残留，
  // 文本节点一律算内容（用户敲的空格不许吞）；IME 组合期间不做归一化。
  assert.match(page, /Array\.from\(el\.childNodes\)\.every\(isComposerBlankResidue\)/);
  assert.match(page, /!\(event\.nativeEvent as InputEvent\)\.isComposing/);
  // 粘贴一律 preventDefault（富文本/图片不许直接进 DOM），text/plain 为空就什么也不插。
  const pasteBody = page.match(/const handleComposerPaste = \(event: React\.ClipboardEvent<HTMLDivElement>\) => \{[\s\S]*?\n  \};/)?.[0] ?? "";
  assert.ok(pasteBody.length > 0, "找不到 handleComposerPaste");
  assert.ok(pasteBody.indexOf("event.preventDefault()") < pasteBody.indexOf('if (!pasted) return;'), "preventDefault 必须先于空串短路");
  // Enter 发送带 keyCode 229 兜底（个别 WebKit 确认选词时 isComposing 已为 false）。
  assert.match(page, /event\.nativeEvent\.keyCode !== 229/);
  // 卡片与浮层规则在 conversation.css 只允许存在一份（复发坑：后置覆盖静默压回）。
  // .composer-edit 基础块用特征属性锚定：≤820px 媒体查询里的 min-height 覆写是合法的第二处。
  assert.equal((stylesheet.match(/\.composer \.paste-card \{/g) ?? []).length, 1);
  assert.equal((stylesheet.match(/\.paste-pop \{/g) ?? []).length, 1);
  assert.equal((stylesheet.match(/\.composer \.composer-edit \{[^}]*white-space: pre-wrap/g) ?? []).length, 1);
});
