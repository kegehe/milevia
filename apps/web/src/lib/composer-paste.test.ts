import assert from "node:assert/strict";
import test from "node:test";

import { PASTE_FOLD_THRESHOLD, isComposerBlankResidue, serializeComposer, shouldFoldPaste, type ComposerLikeNode } from "./composer-paste.ts";

// 内联卡片的序列化是「显示层 ↔ 发送内容」的唯一分界线：卡片还原为完整原文，
// 差一处就是"界面看着是卡片、发出去却是 [object HTMLElement]"级别的事故。
// 节点用普通对象按真实 DOM 的形状构造（tagName 大写、classList.contains、dataset.full）。

const text = (value: string): ComposerLikeNode => ({ nodeType: 3, textContent: value });
const card = (full: string): ComposerLikeNode => ({
  nodeType: 1,
  tagName: "SPAN",
  nodeName: "SPAN",
  classList: { contains: (name: string) => name === "paste-card" },
  dataset: { full },
});
const br = (): ComposerLikeNode => ({ nodeType: 1, tagName: "BR", nodeName: "BR" });
const el = (tagName: string, ...children: ComposerLikeNode[]): ComposerLikeNode => ({
  nodeType: 1,
  tagName,
  nodeName: tagName,
  childNodes: children,
});
const root = (...children: ComposerLikeNode[]): ComposerLikeNode => ({ nodeType: 1, tagName: "DIV", childNodes: children });

test("the fold threshold is the single judge of a long paste", () => {
  assert.equal(PASTE_FOLD_THRESHOLD, 800);
  assert.equal(shouldFoldPaste("a".repeat(800)), false, "恰好 800 字不折叠（大于才折叠）");
  assert.equal(shouldFoldPaste("a".repeat(801)), true);
  assert.equal(shouldFoldPaste(""), false);
});

test("plain text, line breaks and nested blocks survive serialization verbatim", () => {
  assert.equal(serializeComposer(root(text("你好，世界"))), "你好，世界");
  assert.equal(serializeComposer(root(text("第一行"), br(), text("第二行"))), "第一行\n第二行");
  // Shift+Enter 连续换行（连续 BR）不粘连。
  assert.equal(serializeComposer(root(text("a"), br(), br(), text("b"))), "a\n\nb");
  // Chromium 的补全产物是 <div> 包块：块与块之间是换行，不是拼接成一坨。
  assert.equal(
    serializeComposer(root(text("开头"), el("DIV", text("块内")), el("DIV", text("块二")))),
    "开头\n块内\n块二",
  );
  // 块级容器内还有 BR（用户在 div 里按过 Shift+Enter 的真实产物）。
  assert.equal(serializeComposer(root(el("DIV", text("x"), br(), text("y")))), "x\ny");
  // 块之后紧跟游离文本（Chromium 删除/undo 的真实产物）：显示两行，发送也是两行。
  assert.equal(serializeComposer(root(el("DIV", text("a")), text("b"))), "a\nb");
  // 块之后紧跟卡片同样补换行，卡片全文从新行开始。
  assert.equal(serializeComposer(root(el("DIV", text("a")), card("BBB"))), "a\nBBB");
});

test("a paste card serializes back to its FULL original text", () => {
  const original = ["Error: fetch data source failed", "    at render.js:12", "WinError 10061 拒绝连接"].join("\n");
  assert.equal(serializeComposer(root(text("帮我看看："), card(original), text("，谢谢"))),
    `帮我看看：${original}，谢谢`);
  // 连续多张卡片各自还原，顺序不乱。
  assert.equal(serializeComposer(root(card("AAA"), card("BBB"))), "AAABBB");
  // 卡片夹在块级容器里同样还原。
  assert.equal(serializeComposer(root(el("DIV", text("x"), card("CC")))), "xCC");
});

test("a card without data-full degrades to empty instead of leaking DOM internals", () => {
  // 「读不到 ≠ 编一个」：dataset.full 缺失时必须是空串，
  // 否则 [object HTMLSpanElement] 会被原样发出去。
  const broken: ComposerLikeNode = {
    nodeType: 1,
    tagName: "SPAN",
    classList: { contains: (name: string) => name === "paste-card" },
  };
  assert.equal(serializeComposer(root(text("前"), broken, text("后"))), "前后");
});

test("empty editor serializes to an empty string", () => {
  assert.equal(serializeComposer(root()), "");
  // 清空后 Chromium 可能残留一个孤立 <br>：序列化得到一个换行，
  // 页面负责在「trim 后为空」时把 DOM 归零（见 ConversationPage 的输入归一化），占位符才能回来。
  assert.equal(serializeComposer(root(br())), "\n");
});

test("blank residue covers both Chromium leftovers, but typed whitespace is content", () => {
  // 孤立 <br>、只含 <br> 的空 div：两种清空残留都要归零（否则 :empty 占位符回不来）。
  assert.equal(isComposerBlankResidue(br()), true);
  assert.equal(isComposerBlankResidue(el("DIV", br())), true);
  assert.equal(isComposerBlankResidue(el("DIV", br(), el("DIV"))), true);
  // 文本节点一律是内容：用户敲的空格不是残留，不许吞（textarea 允许以空格开头）。
  assert.equal(isComposerBlankResidue(text(" ")), false);
  assert.equal(isComposerBlankResidue(el("DIV", text(" "))), false);
  // 卡片不是残留（它带着要发送的原文），带卡片的 div 也不是。
  assert.equal(isComposerBlankResidue(card("AAA")), false);
  assert.equal(isComposerBlankResidue(el("DIV", card("AAA"))), false);
  // 其他元素（如富文本残留的 <b>）不算空白残留，不归零。
  assert.equal(isComposerBlankResidue(el("B", text("x"))), false);
});
