// 对话页宽屏三栏的宽度与整栏折叠。全局一份，不跟项目走：
// 换项目时栏宽不该跳。提示词/命令/技能各自的折叠仍在 conversation-panels。

export const CONVERSATION_LAYOUT = {
  leftDefault: 300,
  rightDefault: 320,
  minOpen: 220,
  centerMin: 360,
  collapseAt: 160,
  expandAt: 200,
  leftCollapsedWidth: 40,
  rightCollapsedWidth: 44,
  splitter: 8,
  keyboardStep: 16,
  maxWidth: 1600,
} as const;

const STORAGE_KEY = "milevia:conversation-layout";

export type ConversationPaneSide = "left" | "right";

export type ConversationLayout = {
  leftWidth: number;
  rightWidth: number;
  leftCollapsed: boolean;
  rightCollapsed: boolean;
};

export type DisplayedConversationLayout = {
  leftPx: number;
  rightPx: number;
  leftCollapsed: boolean;
  rightCollapsed: boolean;
};

export type PaneDrag = {
  collapsed: boolean;
  openWidth: number;
  px: number;
  visualCollapsed: boolean;
  /** 指针停在收起阈值和最小展开宽度之间。松手时把偏好宽度收到最小展开宽度。 */
  snapOpenToMin: boolean;
};

export function defaultConversationLayout(): ConversationLayout {
  return {
    leftWidth: CONVERSATION_LAYOUT.leftDefault,
    rightWidth: CONVERSATION_LAYOUT.rightDefault,
    leftCollapsed: false,
    rightCollapsed: false,
  };
}

export function normalizeConversationLayout(value: unknown): ConversationLayout {
  const defaults = defaultConversationLayout();
  if (!value || typeof value !== "object" || Array.isArray(value)) return defaults;
  const raw = value as Partial<ConversationLayout>;
  return {
    leftWidth: normalizeWidth(raw.leftWidth, defaults.leftWidth),
    rightWidth: normalizeWidth(raw.rightWidth, defaults.rightWidth),
    leftCollapsed: raw.leftCollapsed === true,
    rightCollapsed: raw.rightCollapsed === true,
  };
}

export function readConversationLayout(): ConversationLayout {
  try {
    const raw = window.localStorage.getItem(STORAGE_KEY);
    if (!raw) return defaultConversationLayout();
    return normalizeConversationLayout(JSON.parse(raw));
  } catch {
    return defaultConversationLayout();
  }
}

export function writeConversationLayout(layout: ConversationLayout): void {
  try {
    window.localStorage.setItem(STORAGE_KEY, JSON.stringify(normalizeConversationLayout(layout)));
  } catch {
    // 隐私模式或存储满时忽略，宽度只在本次打开里生效。
  }
}

export function withPane(layout: ConversationLayout, side: ConversationPaneSide, pane: { collapsed: boolean; openWidth: number }): ConversationLayout {
  if (side === "left") return { ...layout, leftCollapsed: pane.collapsed, leftWidth: pane.openWidth };
  return { ...layout, rightCollapsed: pane.collapsed, rightWidth: pane.openWidth };
}

export function commitPaneDrag(layout: ConversationLayout, side: ConversationPaneSide, drag: PaneDrag): ConversationLayout {
  const openWidth = drag.snapOpenToMin && !drag.collapsed ? CONVERSATION_LAYOUT.minOpen : drag.openWidth;
  return withPane(layout, side, { collapsed: drag.collapsed, openWidth });
}

function sameLayout(current: ConversationLayout, next: ConversationLayout): ConversationLayout | null {
  if (
    current.leftWidth === next.leftWidth
    && current.rightWidth === next.rightWidth
    && current.leftCollapsed === next.leftCollapsed
    && current.rightCollapsed === next.rightCollapsed
  ) return null;
  return next;
}

// 键盘微调保存的偏好宽度，不跟窗口临时挤出来的像素走。
// 窗口把栏挤窄时，放大键不做任何事；缩小键每次只减一步偏好，不会一下收成当前可见宽度。
export function nudgeConversationPane(input: {
  key: string;
  side: ConversationPaneSide;
  layout: ConversationLayout;
  canvasWidth: number;
  hasRight: boolean;
}): ConversationLayout | null {
  const { key, side, layout, canvasWidth, hasRight } = input;
  if (canvasWidth <= 0) return null;
  const isLeft = side === "left";
  const grow = isLeft ? key === "ArrowRight" || key === "End" : key === "ArrowLeft" || key === "Home";
  const shrink = isLeft ? key === "ArrowLeft" || key === "Home" : key === "ArrowRight" || key === "End";
  if (!grow && !shrink) return null;

  const storedCollapsed = isLeft ? layout.leftCollapsed : layout.rightCollapsed;
  const openWidth = isLeft ? layout.leftWidth : layout.rightWidth;
  const displayed = displayConversationLayout(layout, canvasWidth, hasRight);
  const px = isLeft ? displayed.leftPx : displayed.rightPx;
  const visual = isLeft ? displayed.leftCollapsed : displayed.rightCollapsed;
  const arrow = key === "ArrowLeft" || key === "ArrowRight";
  const toMin = isLeft ? key === "Home" : key === "End";
  if (toMin) return sameLayout(layout, withPane(layout, side, { collapsed: true, openWidth }));

  if ((storedCollapsed || visual) && grow) {
    const revealed = revealPane(layout, side, canvasWidth, hasRight);
    if (arrow) return sameLayout(layout, revealed);
    const revealedWidth = side === "left" ? revealed.leftWidth : revealed.rightWidth;
    const shown = displayConversationLayout(revealed, canvasWidth, hasRight);
    const otherPx = side === "left" ? shown.rightPx : shown.leftPx;
    const available = paneAvailable(canvasWidth, otherPx, hasRight);
    const maxFit = Math.max(revealedWidth, Math.min(CONVERSATION_LAYOUT.maxWidth, Math.round(Math.max(0, available))));
    if (maxFit === revealedWidth) return sameLayout(layout, revealed);
    return withPane(revealed, side, { collapsed: false, openWidth: maxFit });
  }

  if (visual || (storedCollapsed && shrink)) return null;

  const squeezed = px < openWidth;
  if (squeezed && grow) return null;
  if (squeezed && shrink && arrow) {
    if (openWidth <= CONVERSATION_LAYOUT.minOpen) return withPane(layout, side, { collapsed: true, openWidth });
    const stepped = openWidth - CONVERSATION_LAYOUT.keyboardStep;
    if (stepped < CONVERSATION_LAYOUT.minOpen) return withPane(layout, side, { collapsed: false, openWidth: CONVERSATION_LAYOUT.minOpen });
    return withPane(layout, side, { collapsed: false, openWidth: stepped });
  }

  if (!storedCollapsed && shrink && arrow && openWidth <= CONVERSATION_LAYOUT.minOpen) {
    return withPane(layout, side, { collapsed: true, openWidth });
  }

  const proposed = arrow
    ? openWidth + (grow ? CONVERSATION_LAYOUT.keyboardStep : -CONVERSATION_LAYOUT.keyboardStep)
    : canvasWidth;
  const otherPx = isLeft ? displayed.rightPx : displayed.leftPx;
  const next = dragPane({
    proposed,
    collapsed: storedCollapsed,
    openWidth,
    available: paneAvailable(canvasWidth, otherPx, hasRight),
    collapsedWidth: isLeft ? CONVERSATION_LAYOUT.leftCollapsedWidth : CONVERSATION_LAYOUT.rightCollapsedWidth,
  });
  if (grow && !next.collapsed && next.openWidth < openWidth) return null;
  return sameLayout(layout, commitPaneDrag(layout, side, next));
}

// 松手时决定要不要把这次拖拽写进偏好。返回 null 表示保持拖拽开始前的布局：
// 没移动，或只是碰了一下「窗口太窄才临时收起」的细条。
export function settlePaneDrag(input: {
  moved: boolean;
  storedCollapsed: boolean;
  visualCollapsed: boolean;
  drag: PaneDrag;
  start: ConversationLayout;
  side: ConversationPaneSide;
}): ConversationLayout | null {
  if (!input.moved) return null;
  if (!input.storedCollapsed && input.visualCollapsed && input.drag.collapsed) return null;
  return commitPaneDrag(input.start, input.side, input.drag);
}

// 展开一侧。如果另一侧占满了中间所需的空间，就先把另一侧缩小或收起，避免点了展开却仍被挤没。
export function revealPane(layout: ConversationLayout, side: ConversationPaneSide, canvasWidth: number, hasRight: boolean): ConversationLayout {
  const openWidth = side === "left" ? layout.leftWidth : layout.rightWidth;
  const next = withPane(layout, side, { collapsed: false, openWidth });
  if (canvasWidth <= 0) return next;
  const shown = displayConversationLayout(next, canvasWidth, hasRight);
  const visible = side === "left" ? !shown.leftCollapsed : !shown.rightCollapsed;
  if (visible) return next;
  const other: ConversationPaneSide = side === "left" ? "right" : "left";
  if (other === "right" && !hasRight) return next;
  const otherWidth = other === "left" ? next.leftWidth : next.rightWidth;
  const room = paneAvailable(canvasWidth, CONVERSATION_LAYOUT.minOpen, hasRight);
  if (room < CONVERSATION_LAYOUT.minOpen) return withPane(next, other, { collapsed: true, openWidth: otherWidth });
  return withPane(next, other, { collapsed: false, openWidth: Math.min(otherWidth, room) });
}

export function paneAvailable(canvasWidth: number, otherPx: number, hasRight: boolean): number {
  const splitters = (hasRight ? 2 : 1) * CONVERSATION_LAYOUT.splitter;
  return canvasWidth - otherPx - splitters - CONVERSATION_LAYOUT.centerMin;
}

// 按当前偏好算出两侧实际占用。窗口不够时两边一起变窄，不改偏好；
// 窄到放不下最小展开宽度时才临时收起，窗口变宽后再按偏好展开。
export function displayConversationLayout(layout: ConversationLayout, canvasWidth: number, hasRight: boolean): DisplayedConversationLayout {
  const leftCollapsedWidth = CONVERSATION_LAYOUT.leftCollapsedWidth;
  const rightCollapsedWidth = CONVERSATION_LAYOUT.rightCollapsedWidth;
  if (canvasWidth <= 0) {
    return {
      leftPx: layout.leftCollapsed ? leftCollapsedWidth : layout.leftWidth,
      rightPx: hasRight ? (layout.rightCollapsed ? rightCollapsedWidth : layout.rightWidth) : 0,
      leftCollapsed: layout.leftCollapsed,
      rightCollapsed: hasRight && layout.rightCollapsed,
    };
  }

  const splitters = (hasRight ? 2 : 1) * CONVERSATION_LAYOUT.splitter;
  const budget = canvasWidth - splitters - CONVERSATION_LAYOUT.centerMin;
  let leftCollapsed = layout.leftCollapsed;
  let rightCollapsed = hasRight && layout.rightCollapsed;
  let leftPx = leftCollapsed ? leftCollapsedWidth : layout.leftWidth;
  let rightPx = !hasRight || rightCollapsed ? (hasRight && rightCollapsed ? rightCollapsedWidth : 0) : layout.rightWidth;

  const clampOpen = (width: number, available: number, collapsedWidth: number) => {
    if (available < CONVERSATION_LAYOUT.collapseAt) return { collapsed: true, px: collapsedWidth };
    return { collapsed: false, px: Math.round(Math.min(width, Math.max(0, available))) };
  };

  if (!hasRight) {
    const left = clampOpen(layout.leftWidth, budget, leftCollapsedWidth);
    if (layout.leftCollapsed) return { leftPx: leftCollapsedWidth, rightPx: 0, leftCollapsed: true, rightCollapsed: false };
    return { leftPx: left.px, rightPx: 0, leftCollapsed: left.collapsed, rightCollapsed: false };
  }

  if (leftCollapsed || rightCollapsed) {
    if (!leftCollapsed) {
      const left = clampOpen(layout.leftWidth, budget - rightPx, leftCollapsedWidth);
      leftCollapsed = left.collapsed;
      leftPx = left.px;
    }
    if (!rightCollapsed) {
      const right = clampOpen(layout.rightWidth, budget - leftPx, rightCollapsedWidth);
      rightCollapsed = right.collapsed;
      rightPx = right.px;
    }
    return { leftPx, rightPx, leftCollapsed, rightCollapsed };
  }

  if (leftPx + rightPx <= budget) return { leftPx, rightPx, leftCollapsed: false, rightCollapsed: false };

  const minBoth = CONVERSATION_LAYOUT.minOpen * 2;
  if (budget >= minBoth) {
    const extra = budget - minBoth;
    const leftWant = Math.max(0, layout.leftWidth - CONVERSATION_LAYOUT.minOpen);
    const rightWant = Math.max(0, layout.rightWidth - CONVERSATION_LAYOUT.minOpen);
    const want = leftWant + rightWant;
    leftPx = Math.round(CONVERSATION_LAYOUT.minOpen + (want === 0 ? extra / 2 : extra * leftWant / want));
    rightPx = budget - leftPx;
    leftPx = Math.min(leftPx, layout.leftWidth);
    rightPx = Math.min(Math.max(rightPx, CONVERSATION_LAYOUT.minOpen), layout.rightWidth);
    if (leftPx + rightPx > budget) leftPx = Math.max(CONVERSATION_LAYOUT.minOpen, leftPx - (leftPx + rightPx - budget));
    if (leftPx + rightPx > budget) rightPx = Math.max(CONVERSATION_LAYOUT.minOpen, rightPx - (leftPx + rightPx - budget));
    return { leftPx, rightPx, leftCollapsed: false, rightCollapsed: false };
  }

  // 两边都达不到最小展开宽度时先收起左边，把剩下的空间留给右边。
  const right = clampOpen(layout.rightWidth, budget - leftCollapsedWidth, rightCollapsedWidth);
  return { leftPx: leftCollapsedWidth, rightPx: right.px, leftCollapsed: true, rightCollapsed: right.collapsed };
}

// 一次拖拽里的宽度。proposed 是按下时的显示宽度加上指针位移，不是当前帧的栏宽。
// 展开中拖进 160px 以下才收起，并保留收起前的偏好宽度；收起后再拖过 200px 才展开。
// 160 到 220 之间不停留：拖的时候贴在 220，松手写成最小展开宽度。
export function dragPane(input: { proposed: number; collapsed: boolean; openWidth: number; available: number; collapsedWidth: number }): PaneDrag {
  const { proposed, collapsed, openWidth, available, collapsedWidth } = input;
  if (collapsed) {
    if (proposed < CONVERSATION_LAYOUT.expandAt) {
      return { collapsed: true, openWidth, px: collapsedWidth, visualCollapsed: true, snapOpenToMin: false };
    }
    const next = clampOpenWidth(proposed, available);
    return openDrag(next, available, collapsedWidth);
  }
  if (proposed < CONVERSATION_LAYOUT.collapseAt) {
    return { collapsed: true, openWidth, px: collapsedWidth, visualCollapsed: true, snapOpenToMin: false };
  }
  if (proposed < CONVERSATION_LAYOUT.minOpen) {
    const px = available < CONVERSATION_LAYOUT.collapseAt ? collapsedWidth : Math.min(CONVERSATION_LAYOUT.minOpen, available);
    return {
      collapsed: false,
      openWidth,
      px: Math.round(px),
      visualCollapsed: available < CONVERSATION_LAYOUT.collapseAt,
      snapOpenToMin: available >= CONVERSATION_LAYOUT.collapseAt,
    };
  }
  return openDrag(clampOpenWidth(proposed, available), available, collapsedWidth);
}

function openDrag(openWidth: number, available: number, collapsedWidth: number): PaneDrag {
  if (available < CONVERSATION_LAYOUT.collapseAt) {
    return { collapsed: false, openWidth, px: collapsedWidth, visualCollapsed: true, snapOpenToMin: false };
  }
  return {
    collapsed: false,
    openWidth,
    px: Math.round(Math.min(openWidth, Math.max(available, 0))),
    visualCollapsed: false,
    snapOpenToMin: false,
  };
}

function clampOpenWidth(width: number, available: number): number {
  const max = Math.max(CONVERSATION_LAYOUT.minOpen, Math.min(CONVERSATION_LAYOUT.maxWidth, available));
  return Math.round(Math.min(max, Math.max(CONVERSATION_LAYOUT.minOpen, width)));
}

function normalizeWidth(value: unknown, fallback: number): number {
  if (typeof value !== "number" || !Number.isFinite(value)) return fallback;
  return Math.round(Math.min(CONVERSATION_LAYOUT.maxWidth, Math.max(CONVERSATION_LAYOUT.minOpen, value)));
}
