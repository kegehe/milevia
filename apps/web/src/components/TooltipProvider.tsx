import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";

type TooltipState = { anchor: HTMLElement; text: string; title?: string; desc?: string };
type TooltipPosition = { left: number; top: number; side: "top" | "bottom" };
type InputModality = "mouse" | "touch" | "pen" | "keyboard" | "unknown";

const TOOLTIP_ID = "milevia-global-tooltip";
const MAX_TOOLTIP_LENGTH = 360;
const normalizedTitles = new WeakMap<HTMLElement, string>();

function clearNormalizedTooltip(target: HTMLElement) {
  const previous = normalizedTitles.get(target);
  if (previous === undefined) return;
  if (target.dataset.tooltip === previous) target.removeAttribute("data-tooltip");
  normalizedTitles.delete(target);
}

function normalizeTooltip(target: HTMLElement) {
  const title = target.getAttribute("title");
  if (title === "") return;
  if (!title?.trim()) {
    clearNormalizedTooltip(target);
    return;
  }
  const text = title.trim();
  normalizedTitles.set(target, text);
  if (target.dataset.tooltip !== text) target.setAttribute("data-tooltip", text);
  // 保留空属性以关闭原生提示，同时让 React 后续移除 title 时产生可观察的变化。
  // 直接 removeAttribute 会让 React 移除已不存在的属性，旧 data-tooltip 因而永久残留。
  target.setAttribute("title", "");
}

function normalizeTooltipTree(root: Node) {
  if (root instanceof HTMLElement) normalizeTooltip(root);
  if (root instanceof Element || root instanceof DocumentFragment) root.querySelectorAll<HTMLElement>("[title]").forEach(normalizeTooltip);
}

function tooltipTarget(node: EventTarget | null) {
  return node instanceof Element ? node.closest<HTMLElement>("[data-tooltip-title], [data-tooltip], [title]") : null;
}

export function isHoverPointerType(pointerType: string) {
  return pointerType === "mouse";
}

function tooltipPreview(text: string) {
  if (text.length <= MAX_TOOLTIP_LENGTH) return text;
  return `内容较长，仅显示前 ${MAX_TOOLTIP_LENGTH} 个字符：\n${text.slice(0, MAX_TOOLTIP_LENGTH).trimEnd()}`;
}

function updateTooltipDescription(anchor: HTMLElement, visible: boolean) {
  const descriptions = (anchor.getAttribute("aria-describedby") || "").split(/\s+/).filter(Boolean).filter((id) => id !== TOOLTIP_ID);
  if (visible) descriptions.push(TOOLTIP_ID);
  if (descriptions.length > 0) anchor.setAttribute("aria-describedby", descriptions.join(" "));
  else anchor.removeAttribute("aria-describedby");
}

export function TooltipProvider({ children }: { children: React.ReactNode }) {
  const [tooltip, setTooltip] = useState<TooltipState | null>(null);
  const [position, setPosition] = useState<TooltipPosition | null>(null);
  const tooltipRef = useRef<HTMLDivElement>(null);
  const activeAnchorRef = useRef<HTMLElement | null>(null);
  const inputModalityRef = useRef<InputModality>("unknown");

  const hideTooltip = useCallback((anchor?: HTMLElement) => {
    const activeAnchor = activeAnchorRef.current;
    if (anchor && activeAnchor !== anchor) return;
    if (activeAnchor?.isConnected) updateTooltipDescription(activeAnchor, false);
    activeAnchorRef.current = null;
    setTooltip(null);
    setPosition(null);
  }, []);

  const showTooltip = useCallback((anchor: HTMLElement) => {
    // 结构化悬浮卡片：data-tooltip-title(标题) + data-tooltip-desc(描述)。
    // 用于技能等需要“名称标题 + 简短描述”的场景，不走超长截断逻辑。
    const title = anchor.dataset.tooltipTitle?.trim();
    if (title) {
      if (activeAnchorRef.current !== anchor && activeAnchorRef.current?.isConnected) updateTooltipDescription(activeAnchorRef.current, false);
      updateTooltipDescription(anchor, true);
      activeAnchorRef.current = anchor;
      setPosition(null);
      setTooltip({ anchor, text: "", title, desc: anchor.dataset.tooltipDesc?.trim() });
      return;
    }
    // title 可能刚更新、尚未经过 MutationObserver；读取最新值，但不在事件中转换 DOM。
    const text = anchor.getAttribute("title")?.trim() || anchor.dataset.tooltip?.trim();
    if (!text) {
      hideTooltip(anchor);
      return;
    }
    if (activeAnchorRef.current !== anchor && activeAnchorRef.current?.isConnected) updateTooltipDescription(activeAnchorRef.current, false);
    updateTooltipDescription(anchor, true);
    activeAnchorRef.current = anchor;
    setPosition(null);
    setTooltip({ anchor, text: tooltipPreview(text) });
  }, [hideTooltip]);

  const placeTooltip = useCallback(() => {
    if (!tooltip || !tooltipRef.current) return;
    if (!tooltip.anchor.isConnected) {
      hideTooltip(tooltip.anchor);
      return;
    }
    const anchor = tooltip.anchor.getBoundingClientRect();
    const popup = tooltipRef.current.getBoundingClientRect();
    const inset = 10;
    const gap = 10;
    const spaceAbove = anchor.top - inset - gap;
    const spaceBelow = window.innerHeight - anchor.bottom - inset - gap;
    const side = spaceAbove >= popup.height || spaceAbove >= spaceBelow ? "top" : "bottom";
    const top = side === "top"
      ? Math.max(inset, anchor.top - popup.height - gap)
      : Math.min(Math.max(inset, anchor.bottom + gap), window.innerHeight - popup.height - inset);
    const left = Math.min(Math.max(anchor.left + anchor.width / 2 - popup.width / 2, inset), window.innerWidth - popup.width - inset);
    setPosition((current) => current?.left === left && current.top === top && current.side === side ? current : { left, top, side });
  }, [hideTooltip, tooltip]);

  useEffect(() => {
    const root = document.body;
    normalizeTooltipTree(root);
    const observerOptions: MutationObserverInit = { subtree: true, childList: true, attributes: true, attributeFilter: ["title", "data-tooltip", "data-tooltip-title", "data-tooltip-desc"] };
    const observer = new MutationObserver((records) => {
      const titleTargets = new Set<HTMLElement>();
      for (const record of records) {
        if (record.type === "attributes" && record.attributeName === "title" && record.target instanceof HTMLElement) titleTargets.add(record.target);
      }
      // 转换器自身的属性写入不参与观察，避免把内部的 title="" 误判为调用方清空提示。
      observer.disconnect();
      try {
        for (const target of titleTargets) {
          if (target.getAttribute("title") === "") clearNormalizedTooltip(target);
          else normalizeTooltip(target);
        }
        for (const record of records) {
          if (record.type === "childList") record.addedNodes.forEach(normalizeTooltipTree);
        }
      } finally {
        observer.observe(root, observerOptions);
      }
      const activeAnchor = activeAnchorRef.current;
      if (activeAnchor && records.some((record) => record.type === "attributes" && record.target === activeAnchor)) showTooltip(activeAnchor);
    });
    observer.observe(root, observerOptions);

    const pointerOver = (event: PointerEvent) => {
      const target = tooltipTarget(event.target);
      const pointerType = event.pointerType || "unknown";
      inputModalityRef.current = pointerType === "mouse" || pointerType === "touch" || pointerType === "pen" ? pointerType : "unknown";
      if (!isHoverPointerType(pointerType)) {
        hideTooltip();
        return;
      }
      if (target) showTooltip(target);
    };
    const pointerDown = (event: PointerEvent) => {
      const pointerType = event.pointerType || "unknown";
      inputModalityRef.current = pointerType === "mouse" || pointerType === "touch" || pointerType === "pen" ? pointerType : "unknown";
      // 触摸/手写笔点击可能随后触发 focusin；先清理旧提示，避免点按后残留。
      if (!isHoverPointerType(pointerType)) hideTooltip();
    };
    const pointerOut = (event: PointerEvent) => {
      const target = tooltipTarget(event.target);
      if (!target || event.relatedTarget instanceof Node && target.contains(event.relatedTarget)) return;
      hideTooltip(target);
    };
    const focusIn = (event: FocusEvent) => {
      const target = tooltipTarget(event.target);
      // 触摸按钮获得焦点不是用户在请求帮助；键盘焦点仍需要提示。
      if (target && inputModalityRef.current !== "touch" && inputModalityRef.current !== "pen") showTooltip(target);
    };
    const focusOut = (event: FocusEvent) => {
      const target = tooltipTarget(event.target);
      if (!target) return;
      queueMicrotask(() => {
        if (!(document.activeElement instanceof Node && target.contains(document.activeElement))) hideTooltip(target);
      });
    };
    const keyDown = () => {
      inputModalityRef.current = "keyboard";
    };
    document.addEventListener("pointerdown", pointerDown, true);
    document.addEventListener("pointerover", pointerOver, true);
    document.addEventListener("pointerout", pointerOut, true);
    document.addEventListener("focusin", focusIn, true);
    document.addEventListener("focusout", focusOut, true);
    document.addEventListener("keydown", keyDown, true);
    return () => {
      observer.disconnect();
      document.removeEventListener("pointerdown", pointerDown, true);
      document.removeEventListener("pointerover", pointerOver, true);
      document.removeEventListener("pointerout", pointerOut, true);
      document.removeEventListener("focusin", focusIn, true);
      document.removeEventListener("focusout", focusOut, true);
      document.removeEventListener("keydown", keyDown, true);
    };
  }, [hideTooltip, showTooltip]);

  useLayoutEffect(() => {
    if (!tooltip) return;
    placeTooltip();
    window.addEventListener("resize", placeTooltip);
    window.addEventListener("scroll", placeTooltip, true);
    return () => {
      window.removeEventListener("resize", placeTooltip);
      window.removeEventListener("scroll", placeTooltip, true);
    };
  }, [placeTooltip, tooltip]);

  useEffect(() => {
    if (!tooltip) return;
    const observer = new MutationObserver(() => {
      if (!tooltip.anchor.isConnected) hideTooltip(tooltip.anchor);
    });
    observer.observe(document.body, { childList: true, subtree: true });
    return () => observer.disconnect();
  }, [hideTooltip, tooltip]);

  return <>{children}{tooltip && createPortal(
    <div ref={tooltipRef} id={TOOLTIP_ID} className="app-tooltip" role="tooltip" data-side={position?.side} data-positioned={position ? "true" : undefined} data-structured={tooltip.title ? "true" : undefined} style={position ? { left: position.left, top: position.top } : undefined}>
      {tooltip.title
        ? <>
            <span className="app-tooltip-title">{tooltip.title}</span>
            {tooltip.desc && <span className="app-tooltip-desc">{tooltip.desc}</span>}
          </>
        : tooltip.text}
    </div>,
    document.body,
  )}</>;
}
