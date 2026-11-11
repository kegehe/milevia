import { useCallback, useLayoutEffect, useRef, useState, type KeyboardEvent as ReactKeyboardEvent, type PointerEvent as ReactPointerEvent } from "react";
import {
  CONVERSATION_LAYOUT,
  displayConversationLayout,
  dragPane,
  paneAvailable,
  normalizeConversationLayout,
  nudgeConversationPane,
  readConversationLayout,
  revealPane,
  settlePaneDrag,
  withPane,
  writeConversationLayout,
  type ConversationLayout,
  type ConversationPaneSide,
  type DisplayedConversationLayout,
  type PaneDrag,
} from "./conversation-layout";

const WIDE_QUERY = "(min-width: 1200px)";

type DragSession = {
  side: ConversationPaneSide;
  pointerId: number;
  startX: number;
  originPx: number;
  otherPx: number;
  otherCollapsed: boolean;
  openWidth: number;
  collapsed: boolean;
  moved: boolean;
  storedCollapsed: boolean;
  visualCollapsed: boolean;
  startLayout: ConversationLayout;
  last: PaneDrag;
  handle: HTMLElement;
  onMove: (event: PointerEvent) => void;
  onUp: (event: PointerEvent) => void;
  onBlur: () => void;
};

function setFlag(element: HTMLElement, name: string, on: boolean) {
  if (on) element.dataset[name] = "true";
  else delete element.dataset[name];
}

function collapsedWidth(side: ConversationPaneSide): number {
  return side === "left" ? CONVERSATION_LAYOUT.leftCollapsedWidth : CONVERSATION_LAYOUT.rightCollapsedWidth;
}

// 宽屏左右栏的拖拽、折叠和宽度记忆。拖动过程只改画布上的 CSS 变量，松手才提交状态。
export function useConversationLayout(hasRightPane: boolean) {
  const canvasRef = useRef<HTMLElement | null>(null);
  const dragRef = useRef<DragSession | null>(null);
  const [layout, setLayout] = useState<ConversationLayout>(() => readConversationLayout());
  const layoutRef = useRef(layout);
  // 拖动期间布局写在 ref 里，父组件重渲染不能把它盖回松手前的状态。
  if (!dragRef.current) layoutRef.current = layout;
  // 含「窗口太窄被临时挤扁」的显示状态。细条按钮要跟它走，不能只看用户主动折叠。
  const [squeezed, setSqueezed] = useState(() => ({ left: layout.leftCollapsed, right: layout.rightCollapsed }));
  const hasRightRef = useRef(hasRightPane);
  hasRightRef.current = hasRightPane;
  const [wide, setWide] = useState(() => typeof window !== "undefined" && window.matchMedia(WIDE_QUERY).matches);
  const wideRef = useRef(wide);
  wideRef.current = wide;

  const writeDom = useCallback((displayed: DisplayedConversationLayout) => {
    const canvas = canvasRef.current;
    if (!canvas) return;
    canvas.style.setProperty("--pane-left", `${displayed.leftPx}px`);
    canvas.style.setProperty("--pane-right", `${displayed.rightPx}px`);
    setFlag(canvas, "leftCollapsed", displayed.leftCollapsed);
    setFlag(canvas, "rightCollapsed", displayed.rightCollapsed);
    const leftRail = canvas.querySelector<HTMLElement>(":scope > .quick-tag-rail");
    const rightRail = canvas.querySelector<HTMLElement>(":scope > .task-queue-rail");
    if (leftRail) setFlag(leftRail, "collapsed", displayed.leftCollapsed);
    if (rightRail) setFlag(rightRail, "collapsed", displayed.rightCollapsed);
  }, []);

  const clearDom = useCallback(() => {
    const canvas = canvasRef.current;
    if (!canvas) return;
    canvas.style.removeProperty("--pane-left");
    canvas.style.removeProperty("--pane-right");
    delete canvas.dataset.leftCollapsed;
    delete canvas.dataset.rightCollapsed;
    canvas.querySelectorAll<HTMLElement>(":scope > .quick-tag-rail, :scope > .task-queue-rail").forEach((rail) => {
      delete rail.dataset.collapsed;
    });
  }, []);

  const publish = useCallback((next: ConversationLayout) => {
    const canvas = canvasRef.current;
    if (!canvas || !wideRef.current) return;
    const displayed = displayConversationLayout(next, canvas.clientWidth, hasRightRef.current);
    writeDom(displayed);
    setSqueezed((current) => current.left === displayed.leftCollapsed && current.right === displayed.rightCollapsed ? current : { left: displayed.leftCollapsed, right: displayed.rightCollapsed });
  }, [writeDom]);

  const commit = useCallback((next: ConversationLayout) => {
    const normalized = normalizeConversationLayout(next);
    layoutRef.current = normalized;
    setLayout(normalized);
    writeConversationLayout(normalized);
    publish(normalized);
  }, [publish]);

  useLayoutEffect(() => {
    const media = window.matchMedia(WIDE_QUERY);
    const sync = () => {
      wideRef.current = media.matches;
      setWide(media.matches);
    };
    sync();
    media.addEventListener("change", sync);
    return () => media.removeEventListener("change", sync);
  }, []);

  useLayoutEffect(() => {
    if (!wide) {
      clearDom();
      setSqueezed((current) => current.left || current.right ? { left: false, right: false } : current);
      return;
    }
    publish(layoutRef.current);
    const canvas = canvasRef.current;
    if (!canvas) return;
    const observer = new ResizeObserver(() => {
      if (dragRef.current) return;
      publish(layoutRef.current);
    });
    observer.observe(canvas);
    return () => observer.disconnect();
  }, [wide, hasRightPane, layout, clearDom, publish]);

  const endDragRef = useRef<() => void>(() => {});

  const endDrag = useCallback(() => {
    const drag = dragRef.current;
    if (!drag) return;
    dragRef.current = null;
    window.removeEventListener("pointermove", drag.onMove, true);
    window.removeEventListener("pointerup", drag.onUp, true);
    window.removeEventListener("pointercancel", drag.onUp, true);
    window.removeEventListener("blur", drag.onBlur);
    delete drag.handle.dataset.dragging;
    document.body.classList.remove("conversation-pane-dragging");
    const storedCollapsed = drag.side === "left" ? drag.startLayout.leftCollapsed : drag.startLayout.rightCollapsed;
    const storedWidth = drag.side === "left" ? drag.startLayout.leftWidth : drag.startLayout.rightWidth;
    const shownCollapsed = storedCollapsed || drag.visualCollapsed;
    const shown = shownCollapsed ? collapsedWidth(drag.side) : storedWidth;
    drag.handle.setAttribute("aria-valuenow", String(shown));
    drag.handle.setAttribute("aria-valuetext", shownCollapsed ? "已折叠" : `${shown} 像素`);
    const settled = settlePaneDrag({
      moved: drag.moved,
      storedCollapsed: drag.storedCollapsed,
      visualCollapsed: drag.visualCollapsed,
      drag: drag.last,
      start: drag.startLayout,
      side: drag.side,
    });
    if (!settled) {
      layoutRef.current = drag.startLayout;
      publish(drag.startLayout);
      return;
    }
    commit(settled);
  }, [commit, publish]);
  endDragRef.current = endDrag;

  const moveDrag = useCallback((event: PointerEvent) => {
    const drag = dragRef.current;
    const canvas = canvasRef.current;
    if (!drag || !canvas || event.pointerId !== drag.pointerId) return;
    const delta = drag.side === "left" ? event.clientX - drag.startX : drag.startX - event.clientX;
    if (delta !== 0) drag.moved = true;
    const available = paneAvailable(canvas.clientWidth, drag.otherPx, hasRightRef.current);
    const next = dragPane({
      proposed: drag.originPx + delta,
      collapsed: drag.collapsed,
      openWidth: drag.openWidth,
      available,
      collapsedWidth: collapsedWidth(drag.side),
    });
    drag.collapsed = next.collapsed;
    drag.openWidth = next.openWidth;
    drag.last = next;
    layoutRef.current = withPane(drag.startLayout, drag.side, { collapsed: next.collapsed, openWidth: next.openWidth });
    const preview: DisplayedConversationLayout = drag.side === "left"
      ? { leftPx: next.px, leftCollapsed: next.visualCollapsed, rightPx: drag.otherPx, rightCollapsed: drag.otherCollapsed }
      : { rightPx: next.px, rightCollapsed: next.visualCollapsed, leftPx: drag.otherPx, leftCollapsed: drag.otherCollapsed };
    writeDom(preview);
    drag.handle.setAttribute("aria-valuenow", String(Math.round(next.px)));
    drag.handle.setAttribute("aria-valuetext", next.visualCollapsed ? "已折叠" : `${Math.round(next.px)} 像素`);
  }, [writeDom]);

  useLayoutEffect(() => () => {
    document.body.classList.remove("conversation-pane-dragging");
    const drag = dragRef.current;
    if (!drag) return;
    dragRef.current = null;
    window.removeEventListener("pointermove", drag.onMove, true);
    window.removeEventListener("pointerup", drag.onUp, true);
    window.removeEventListener("pointercancel", drag.onUp, true);
    window.removeEventListener("blur", drag.onBlur);
  }, []);

  const startDrag = useCallback((side: ConversationPaneSide, event: ReactPointerEvent<HTMLElement>) => {
    if (event.button !== 0 || event.isPrimary === false || !wideRef.current) return;
    if (dragRef.current) endDragRef.current();
    const canvas = canvasRef.current;
    if (!canvas) return;
    event.preventDefault();
    const startLayout = layoutRef.current;
    const displayed = displayConversationLayout(startLayout, canvas.clientWidth, hasRightRef.current);
    const storedCollapsed = side === "left" ? startLayout.leftCollapsed : startLayout.rightCollapsed;
    const visualCollapsed = side === "left" ? displayed.leftCollapsed : displayed.rightCollapsed;
    const openWidth = side === "left" ? startLayout.leftWidth : startLayout.rightWidth;
    const originPx = side === "left" ? displayed.leftPx : displayed.rightPx;
    const handle = event.currentTarget;
    const onMove = (move: PointerEvent) => moveDrag(move);
    const onUp = (up: PointerEvent) => {
      if (up.pointerId !== event.pointerId) return;
      endDragRef.current();
    };
    const onBlur = () => endDragRef.current();
    handle.dataset.dragging = "true";
    document.body.classList.add("conversation-pane-dragging");
    dragRef.current = {
      side,
      pointerId: event.pointerId,
      startX: event.clientX,
      originPx,
      otherPx: side === "left" ? displayed.rightPx : displayed.leftPx,
      otherCollapsed: side === "left" ? displayed.rightCollapsed : displayed.leftCollapsed,
      openWidth,
      collapsed: storedCollapsed,
      moved: false,
      storedCollapsed,
      visualCollapsed,
      startLayout,
      last: { collapsed: storedCollapsed, openWidth, px: originPx, visualCollapsed, snapOpenToMin: false },
      handle,
      onMove,
      onUp,
      onBlur,
    };
    handle.focus();
    window.addEventListener("pointermove", onMove, true);
    window.addEventListener("pointerup", onUp, true);
    window.addEventListener("pointercancel", onUp, true);
    window.addEventListener("blur", onBlur);
    try {
      handle.setPointerCapture(event.pointerId);
    } catch {
      // 捕获失败时仍靠 window 上的监听收尾。
    }
  }, [moveDrag]);

  const canvasWidth = useCallback(() => canvasRef.current?.clientWidth ?? 0, []);

  const togglePane = useCallback((side: ConversationPaneSide) => {
    const current = layoutRef.current;
    const collapsed = side === "left" ? current.leftCollapsed : current.rightCollapsed;
    const openWidth = side === "left" ? current.leftWidth : current.rightWidth;
    if (!collapsed) {
      commit(withPane(current, side, { collapsed: true, openWidth }));
      return;
    }
    commit(revealPane(current, side, canvasWidth(), hasRightRef.current));
  }, [canvasWidth, commit]);

  const expandPane = useCallback((side: ConversationPaneSide) => {
    commit(revealPane(layoutRef.current, side, canvasWidth(), hasRightRef.current));
  }, [canvasWidth, commit]);

  const resetPane = useCallback((side: ConversationPaneSide) => {
    const seeded = withPane(layoutRef.current, side, {
      collapsed: false,
      openWidth: side === "left" ? CONVERSATION_LAYOUT.leftDefault : CONVERSATION_LAYOUT.rightDefault,
    });
    commit(revealPane(seeded, side, canvasWidth(), hasRightRef.current));
  }, [canvasWidth, commit]);

  const onSplitterKeyDown = useCallback((side: ConversationPaneSide, event: ReactKeyboardEvent<HTMLElement>) => {
    if (!wideRef.current) return;
    const canvas = canvasRef.current;
    if (!canvas) return;
    if (event.key === "Enter") {
      event.preventDefault();
      togglePane(side);
      return;
    }
    const next = nudgeConversationPane({
      key: event.key,
      side,
      layout: layoutRef.current,
      canvasWidth: canvas.clientWidth,
      hasRight: hasRightRef.current,
    });
    if (event.key === "ArrowLeft" || event.key === "ArrowRight" || event.key === "Home" || event.key === "End") event.preventDefault();
    if (next) commit(next);
  }, [commit, togglePane]);

  const splitterProps = useCallback((side: ConversationPaneSide) => {
    const collapsed = side === "left" ? layout.leftCollapsed || squeezed.left : layout.rightCollapsed || squeezed.right;
    const width = side === "left" ? layout.leftWidth : layout.rightWidth;
    const shown = collapsed ? collapsedWidth(side) : width;
    return {
      className: "pane-splitter",
      role: "separator" as const,
      "aria-orientation": "vertical" as const,
      "aria-label": side === "left" ? "调整常用操作栏宽度" : "调整任务栏宽度",
      "aria-valuemin": 0,
      "aria-valuemax": CONVERSATION_LAYOUT.maxWidth,
      "aria-valuenow": shown,
      "aria-valuetext": collapsed ? "已折叠" : `${shown} 像素`,
      tabIndex: 0,
      title: "拖动调整宽度，双击恢复默认",
      onPointerDown: (event: ReactPointerEvent<HTMLElement>) => startDrag(side, event),
      onDoubleClick: () => resetPane(side),
      onKeyDown: (event: ReactKeyboardEvent<HTMLElement>) => onSplitterKeyDown(side, event),
    };
  }, [layout.leftCollapsed, layout.leftWidth, layout.rightCollapsed, layout.rightWidth, onSplitterKeyDown, resetPane, squeezed.left, squeezed.right, startDrag]);

  return {
    canvasRef,
    wide,
    leftCollapsed: layout.leftCollapsed,
    rightCollapsed: layout.rightCollapsed,
    rightCollapsedVisual: squeezed.right,
    expandPane,
    togglePane,
    splitterProps,
  };
}
