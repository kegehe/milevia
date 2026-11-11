import assert from "node:assert/strict";
import test from "node:test";

import {
  CONVERSATION_LAYOUT,
  commitPaneDrag,
  defaultConversationLayout,
  displayConversationLayout,
  dragPane,
  normalizeConversationLayout,
  nudgeConversationPane,
  revealPane,
  settlePaneDrag,
} from "./conversation-layout";

const layout = defaultConversationLayout();

test("非法存档退回默认宽度", () => {
  assert.deepEqual(normalizeConversationLayout(null), layout);
  assert.deepEqual(normalizeConversationLayout([]), layout);
  assert.deepEqual(normalizeConversationLayout({ leftWidth: 40, rightWidth: 9000, leftCollapsed: true, rightCollapsed: "yes" }), {
    leftWidth: CONVERSATION_LAYOUT.minOpen,
    rightWidth: CONVERSATION_LAYOUT.maxWidth,
    leftCollapsed: true,
    rightCollapsed: false,
  });
});

test("展开时拖到最小宽度以上会跟着走，并被可用空间卡住", () => {
  const widened = dragPane({ proposed: 420, collapsed: false, openWidth: 300, available: 480, collapsedWidth: 40 });
  assert.equal(widened.collapsed, false);
  assert.equal(widened.px, 420);
  assert.equal(widened.openWidth, 420);

  const capped = dragPane({ proposed: 900, collapsed: false, openWidth: 300, available: 480, collapsedWidth: 40 });
  assert.equal(capped.px, 480);
  assert.equal(capped.openWidth, 480);
});

test("拖进最小宽度和收起阈值之间时贴住最小宽度，但不改掉原来的偏好", () => {
  const held = dragPane({ proposed: 180, collapsed: false, openWidth: 300, available: 800, collapsedWidth: 40 });
  assert.equal(held.collapsed, false);
  assert.equal(held.px, CONVERSATION_LAYOUT.minOpen);
  assert.equal(held.openWidth, 300);
  assert.equal(held.snapOpenToMin, true);
  assert.equal(commitPaneDrag(layout, "left", held).leftWidth, CONVERSATION_LAYOUT.minOpen);
});

test("拖过收起阈值才折叠，并记住折叠前的宽度", () => {
  const collapsed = dragPane({ proposed: 159, collapsed: false, openWidth: 360, available: 800, collapsedWidth: 40 });
  assert.equal(collapsed.collapsed, true);
  assert.equal(collapsed.visualCollapsed, true);
  assert.equal(collapsed.px, 40);
  assert.equal(collapsed.openWidth, 360);
  assert.equal(commitPaneDrag(layout, "left", collapsed).leftWidth, 360);
  assert.equal(commitPaneDrag(layout, "left", collapsed).leftCollapsed, true);
});

test("收起后要拖过展开阈值才重新打开", () => {
  const held = dragPane({ proposed: 199, collapsed: true, openWidth: 360, available: 800, collapsedWidth: 44 });
  assert.equal(held.collapsed, true);
  assert.equal(held.openWidth, 360);

  const opened = dragPane({ proposed: 200, collapsed: true, openWidth: 360, available: 800, collapsedWidth: 44 });
  assert.equal(opened.collapsed, false);
  assert.equal(opened.openWidth, CONVERSATION_LAYOUT.minOpen);
  assert.equal(opened.px, CONVERSATION_LAYOUT.minOpen);
});

test("窗口放得下时按偏好显示，放不下时先压缩再临时收起", () => {
  const comfortable = displayConversationLayout(layout, 1400, true);
  assert.deepEqual(comfortable, { leftPx: 300, rightPx: 320, leftCollapsed: false, rightCollapsed: false });

  const squeezed = displayConversationLayout(layout, 900, true);
  assert.equal(squeezed.leftCollapsed, false);
  assert.equal(squeezed.rightCollapsed, false);
  assert.ok(squeezed.leftPx < 300);
  assert.equal(900 - squeezed.leftPx - squeezed.rightPx - CONVERSATION_LAYOUT.splitter * 2, CONVERSATION_LAYOUT.centerMin);

  const cramped = displayConversationLayout({ ...layout, leftCollapsed: false, rightCollapsed: false }, 700, true);
  assert.equal(cramped.leftCollapsed, true);
  assert.equal(cramped.leftPx, CONVERSATION_LAYOUT.leftCollapsedWidth);
  assert.equal(cramped.rightCollapsed, false);

  const stored = displayConversationLayout({ ...layout, rightCollapsed: true, rightWidth: 400 }, 1400, true);
  assert.equal(stored.rightCollapsed, true);
  assert.equal(stored.rightPx, CONVERSATION_LAYOUT.rightCollapsedWidth);
  assert.equal(stored.leftPx, 300);
});

test("展开被挤没的一侧时，先给它腾出最小宽度", () => {
  const wideRight = { ...layout, leftCollapsed: true, leftWidth: 300, rightWidth: 900 };
  const revealed = revealPane(wideRight, "left", 1200, true);
  assert.equal(revealed.leftCollapsed, false);
  const shown = displayConversationLayout(revealed, 1200, true);
  assert.equal(shown.leftCollapsed, false);
  assert.ok(shown.leftPx >= CONVERSATION_LAYOUT.minOpen);
});

test("两边都偏宽时一起变窄，不先把左边收起", () => {
  const shown = displayConversationLayout({ ...layout, leftWidth: 700, rightWidth: 700 }, 1200, true);
  assert.equal(shown.leftCollapsed, false);
  assert.equal(shown.rightCollapsed, false);
  assert.ok(shown.leftPx >= CONVERSATION_LAYOUT.minOpen);
  assert.ok(shown.rightPx >= CONVERSATION_LAYOUT.minOpen);
  assert.equal(shown.leftPx + shown.rightPx + CONVERSATION_LAYOUT.splitter * 2 + CONVERSATION_LAYOUT.centerMin, 1200);
});

test("窗口挤出来的临时收起，轻拖一下不会记成用户折叠", () => {
  const drag = dragPane({ proposed: 50, collapsed: false, openWidth: 700, available: 800, collapsedWidth: 40 });
  assert.equal(drag.collapsed, true);
  assert.equal(settlePaneDrag({
    moved: true,
    storedCollapsed: false,
    visualCollapsed: true,
    drag,
    start: layout,
    side: "left",
  }), null);
});

test("从展开状态拖过收起阈值会记下来", () => {
  const drag = dragPane({ proposed: 100, collapsed: false, openWidth: 300, available: 800, collapsedWidth: 40 });
  const settled = settlePaneDrag({
    moved: true,
    storedCollapsed: false,
    visualCollapsed: false,
    drag,
    start: layout,
    side: "left",
  });
  assert.equal(settled?.leftCollapsed, true);
  assert.equal(settled?.leftWidth, 300);
});

test("窗口挤窄时方向键不把偏好收成当前可见宽度", () => {
  const wide = { ...layout, leftWidth: 700, rightWidth: 700 };
  const canvas = 1200;
  const shown = displayConversationLayout(wide, canvas, true);
  assert.ok(shown.leftPx < 700);
  assert.equal(nudgeConversationPane({ key: "ArrowRight", side: "left", layout: wide, canvasWidth: canvas, hasRight: true }), null);
  assert.equal(nudgeConversationPane({ key: "End", side: "left", layout: wide, canvasWidth: canvas, hasRight: true }), null);
  const shrunk = nudgeConversationPane({ key: "ArrowLeft", side: "left", layout: wide, canvasWidth: canvas, hasRight: true });
  assert.equal(shrunk?.leftWidth, 684);
  assert.equal(shrunk?.leftCollapsed, false);
});

test("临时收起的细条，缩小键不会记成用户折叠", () => {
  const stored = { ...layout, leftWidth: 700, rightWidth: 320 };
  const visual = displayConversationLayout({ ...stored, leftCollapsed: true }, 900, true);
  assert.equal(visual.leftCollapsed, true);
  const nudged = nudgeConversationPane({
    key: "ArrowLeft",
    side: "left",
    layout: { ...stored, leftCollapsed: false, leftWidth: 700 },
    canvasWidth: 700,
    hasRight: true,
  });
  assert.equal(nudged, null);
});

test("已经是最小宽度时，再缩小就折叠并记住这个宽度", () => {
  const compact = { ...layout, leftWidth: CONVERSATION_LAYOUT.minOpen };
  const collapsed = nudgeConversationPane({ key: "ArrowLeft", side: "left", layout: compact, canvasWidth: 1400, hasRight: true });
  assert.equal(collapsed?.leftCollapsed, true);
  assert.equal(collapsed?.leftWidth, CONVERSATION_LAYOUT.minOpen);
});

test("放得下时 End 把这一侧拉到剩余空间", () => {
  const grown = nudgeConversationPane({ key: "End", side: "left", layout, canvasWidth: 1400, hasRight: true });
  assert.equal(grown?.leftCollapsed, false);
  assert.ok((grown?.leftWidth ?? 0) > layout.leftWidth);
  const shown = displayConversationLayout(grown ?? layout, 1400, true);
  assert.equal(1400 - shown.leftPx - shown.rightPx - CONVERSATION_LAYOUT.splitter * 2, CONVERSATION_LAYOUT.centerMin);
});

test("没有右栏时只留左分隔条的占位", () => {
  const shown = displayConversationLayout(layout, 1000, false);
  assert.equal(shown.rightPx, 0);
  assert.equal(shown.rightCollapsed, false);
  assert.equal(shown.leftPx, 300);
});
