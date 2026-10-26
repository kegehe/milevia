// 粘贴超长文本 → 输入框里的内联卡片。
//
// 形态对齐 WorkBuddy：超长粘贴不铺满输入框，而是变成文字流里的一枚
// `contenteditable=false` 小卡片，光标可以放在卡片前后继续输入；悬浮卡片
// 弹出原文浮层（长文可滚动）；发送时由 serializeComposer 把卡片还原成
// 完整原文 —— 「输入框里显示什么」与「实际发什么」再次分离，与技能胶囊
// （conversation-skill-refs）是同一条纪律。
//
// 判据单一来源：阈值、序列化规则都只在本文件；页面里不许再写第二份。

/** 超过这个字数的粘贴文本收成内联卡片；以内的走浏览器默认粘贴。 */
export const PASTE_FOLD_THRESHOLD = 800;

export function shouldFoldPaste(text: string): boolean {
  return text.length > PASTE_FOLD_THRESHOLD;
}

/**
 * 清空残留判据：孤立 <br>，或只含 BR/空块的空 div/p（Chromium 清空输入后的两种残留形态）。
 * 文本节点一律算内容 —— 用户敲的空格不是残留，不许吞。
 */
export function isComposerBlankResidue(node: ComposerLikeNode & { nodeName?: string }): boolean {
  if (node.nodeName === "BR") return true;
  if (node.nodeType !== 1) return false;
  if (node.tagName !== "DIV" && node.tagName !== "P") return false;
  const children = node.childNodes ?? [];
  return Array.from(children).every((child) => isComposerBlankResidue(child));
}

/**
 * 序列化用的最小节点形状：真实 DOM 节点天然满足（tagName 为大写、
 * classList.contains、dataset.full），测试里可以用普通对象构造同样的形状。
 */
export interface ComposerLikeNode {
  nodeType: number;
  textContent?: string | null;
  childNodes?: ArrayLike<ComposerLikeNode>;
  tagName?: string;
  classList?: { contains(name: string): boolean };
  dataset?: { full?: string | undefined };
}

const TEXT_NODE = 3;

/**
 * 把编辑器 DOM 还原成要发送的纯文本：
 * - 文本节点原样保留；
 * - paste-card 还原为 data-full 里的完整原文；
 * - <br> 还原为 \n（Shift+Enter 的换行）；
 * - 块级容器（div/p）按换行拼接，且**块之后紧跟的游离内容也要补换行** ——
 *   Chromium 删除/undo 后会出现 `<div>a</div>b` 这种结构，显示两行、
 *   不补就是 "ab"（显示与发送再次分家）。
 * 这个函数必须是无副作用纯函数 —— 输入路径每次按键都会跑一遍。
 */
export function serializeComposer(root: ComposerLikeNode): string {
  let out = "";
  let afterBlock = false;
  const children = root.childNodes ?? [];
  for (let i = 0; i < children.length; i++) {
    const node = children[i];
    if (node.nodeType === TEXT_NODE) {
      const value = node.textContent ?? "";
      if (afterBlock && value && !out.endsWith("\n")) out += "\n";
      out += value;
      afterBlock = false;
    } else if (node.classList?.contains("paste-card")) {
      const full = node.dataset?.full ?? "";
      if (afterBlock && !out.endsWith("\n")) out += "\n";
      out += full;
      afterBlock = false;
    } else if (node.tagName === "BR") {
      out += "\n";
      afterBlock = false;
    } else if (node.tagName === "DIV" || node.tagName === "P") {
      const inner = serializeComposer(node);
      if (inner) {
        if (out && !out.endsWith("\n")) out += "\n";
        out += inner;
        afterBlock = true;
      }
    } else {
      const inner = serializeComposer(node);
      if (afterBlock && inner && !out.endsWith("\n")) out += "\n";
      out += inner;
      afterBlock = false;
    }
  }
  return out;
}

/* ── 以下为浏览器侧 DOM 行为（node 测试跑不到，仅供页面调用） ── */

export function buildPasteCard(full: string): HTMLSpanElement {
  const card = document.createElement("span");
  card.className = "paste-card";
  card.contentEditable = "false";
  card.dataset.full = full;
  card.setAttribute("aria-label", `粘贴的文本，共 ${full.length} 字，悬浮或点按查看原文`);
  card.innerHTML =
    '<span class="paste-card-ico" aria-hidden="true">📄</span>' +
    '<span class="paste-card-text">粘贴的文本</span>' +
    `<span class="paste-card-meta">共 ${full.length} 字</span>` +
    '<button type="button" class="paste-card-x" title="移除粘贴的文本" aria-label="移除粘贴的文本">×</button>';
  card.querySelector(".paste-card-x")?.addEventListener("click", (event) => {
    event.preventDefault();
    event.stopPropagation();
    // 只读/禁用态不许删卡片（readOnly 会话里也不能改草稿）。
    if (card.closest("[data-disabled]")) return;
    hidePastePop();
    // 先摘父节点引用再 remove：挂在 detached 节点上 dispatch（哪怕 bubbles）
    // 的传播路径只有它自己，永远到不了编辑器上的监听器 —— 草稿就会漏掉这次删除。
    const parent = card.parentNode;
    card.remove();
    (parent ?? card).dispatchEvent(new CustomEvent("composer-paste-changed", { bubbles: true }));
  });
  attachPasteHover(card, full);
  return card;
}

/** 在当前光标处插入卡片；光标不在编辑器内时退化为追加到末尾。 */
export function insertPasteCardAtSelection(root: HTMLElement, full: string): HTMLSpanElement {
  const card = buildPasteCard(full);
  const sel = window.getSelection();
  const range = sel && sel.rangeCount > 0 ? sel.getRangeAt(0) : null;
  if (!range || !root.contains(range.commonAncestorContainer)) {
    root.appendChild(card);
    return card;
  }
  range.deleteContents();
  range.insertNode(card);
  range.setStartAfter(card);
  range.collapse(true);
  sel?.removeAllRanges();
  sel?.addRange(range);
  return card;
}

/* ── 悬浮原文浮层 ──
   必须挂在 body 上用 fixed 定位：编辑区是 overflow:auto 的滚动容器，
   absolute 子元素会被裁掉（这是这个浮层第一版就被裁没的原因）。
   mouseleave 后延迟 120ms 再收，鼠标从卡片移进浮层的间隙不算离开。 */

let activePop: HTMLDivElement | null = null;
let activePopCard: HTMLSpanElement | null = null;
let popHideTimer: number | undefined;

export function hidePastePop(): void {
  window.clearTimeout(popHideTimer);
  window.removeEventListener("scroll", onWindowScrollHide, true);
  window.removeEventListener("resize", onWindowScrollHide, true);
  window.removeEventListener("keydown", onPopEscapeHide);
  activePop?.remove();
  activePop = null;
  activePopCard = null;
}

/** 锚卡片已从 DOM 失联（键盘 Backspace/拖拽删除）时收掉浮层；正常悬浮不动。 */
export function hidePastePopIfDetached(): void {
  if (activePop && activePopCard && !activePopCard.isConnected) hidePastePop();
}

// 浮层打开期间任何滚动（含编辑器自身滚动容器的非冒泡 scroll，capture 兜住）/改窗
// 都直接收掉：fixed 定位不会跟着卡片走，留着就是一张指着空处的孤儿浮层。
// 唯一例外是浮层自己 body 里的滚动 —— 「长文可在此滚动查看」就是它本体，
// capture 会把它一并抓到，必须放行，否则滚一下长文浮层就被关了。
function onWindowScrollHide(event: Event): void {
  if (activePop && event.target instanceof Node && activePop.contains(event.target)) return;
  hidePastePop();
}

// 键盘出口：悬浮浮层没有关闭按钮，Esc 是键盘/读屏用户唯一的明确退出路径。
function onPopEscapeHide(event: KeyboardEvent): void {
  if (event.key === "Escape") hidePastePop();
}

function buildPastePop(full: string): HTMLDivElement {
  const pop = document.createElement("div");
  pop.className = "paste-pop";
  pop.setAttribute("role", "tooltip");
  const head = document.createElement("div");
  head.className = "paste-pop-head";
  head.innerHTML =
    '<span class="paste-pop-title">📄 粘贴的文本</span>' +
    '<span class="paste-pop-sep">·</span>' +
    `<span>共 ${full.length} 字</span>` +
    '<span class="paste-pop-sep">·</span>' +
    `<span>${full.split("\n").length} 行</span>` +
    '<span class="paste-pop-hint">长文可在此滚动查看 ↓</span>';
  const body = document.createElement("div");
  body.className = "paste-pop-body";
  body.textContent = full;
  pop.append(head, body);
  pop.addEventListener("mouseenter", () => window.clearTimeout(popHideTimer));
  pop.addEventListener("mouseleave", () => hidePastePop());
  return pop;
}

function showPastePop(card: HTMLSpanElement, full: string): void {
  window.clearTimeout(popHideTimer);
  if (activePop && activePopCard === card) return;
  hidePastePop();
  activePopCard = card;
  activePop = buildPastePop(full);
  document.body.appendChild(activePop);
  const rect = card.getBoundingClientRect();
  activePop.style.display = "flex";
  const width = activePop.offsetWidth;
  const left = Math.max(12, Math.min(rect.left, window.innerWidth - width - 16));
  activePop.style.left = `${left}px`;
  const height = activePop.offsetHeight;
  // 上方放得下就朝上弹（输入框贴着窗口底），放不下才朝下。
  activePop.style.top = rect.top > height + 24 ? `${rect.top - height - 10}px` : `${rect.bottom + 10}px`;
  window.addEventListener("scroll", onWindowScrollHide, true);
  window.addEventListener("resize", onWindowScrollHide, true);
  window.addEventListener("keydown", onPopEscapeHide);
}

export function attachPasteHover(card: HTMLSpanElement, full: string): void {
  card.addEventListener("mouseenter", () => showPastePop(card, full));
  card.addEventListener("mouseleave", () => { popHideTimer = window.setTimeout(hidePastePop, 120); });
  // 触屏没有 hover：点按卡片切换浮层（✕ 已在 buildPasteCard 里自行处理并止泡）。
  card.addEventListener("click", (event) => {
    if ((event.target as HTMLElement | null)?.closest(".paste-card-x")) return;
    if (activePopCard === card) hidePastePop();
    else showPastePop(card, full);
  });
}

/* ── 光标位置辅助（输入历史翻阅用） ── */

/** 光标之前的文本（等价于 textarea 的 value.slice(0, selectionStart)）。 */
export function caretTextBefore(root: HTMLElement): string {
  const sel = window.getSelection();
  if (!sel || sel.rangeCount === 0) return "";
  const range = sel.getRangeAt(0);
  const pre = document.createRange();
  pre.selectNodeContents(root);
  try {
    pre.setEnd(range.startContainer, range.startOffset);
  } catch {
    return "";
  }
  // 不能用 Range.toString()：它不把 <br> 计为换行（Shift+Enter 的产物），
  // 会把视觉第二行误判成第一行、↑ 误入历史翻阅 —— 走 serializeComposer
  // 这一份口径（br/div 与发送文本同规则）。注意它包含卡片还原出的原文，
  // 多行卡之后的 ↑ 不翻历史，与旧 textarea「值里有换行就不翻」的行为一致。
  return serializeComposer(pre.cloneContents() as unknown as ComposerLikeNode);
}
