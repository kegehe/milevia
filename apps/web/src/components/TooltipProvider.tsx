import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";

type TooltipState = { anchor: HTMLElement; text: string; title?: string; desc?: string };
type TooltipPosition = { left: number; top: number; side: "top" | "bottom" };

const TOOLTIP_ID = "milevia-global-tooltip";
const MAX_TOOLTIP_LENGTH = 360;

function normalizeTooltip(target: HTMLElement) {
  const title = target.getAttribute("title");
  if (!title?.trim()) return;
  target.setAttribute("data-tooltip", title.trim());
  target.removeAttribute("title");
}

function normalizeTooltipTree(root: Node) {
  if (root instanceof HTMLElement) normalizeTooltip(root);
  if (root instanceof Element || root instanceof DocumentFragment) root.querySelectorAll<HTMLElement>("[title]").forEach(normalizeTooltip);
}

function tooltipTarget(node: EventTarget | null) {
  return node instanceof Element ? node.closest<HTMLElement>("[data-tooltip-title], [data-tooltip], [title]") : null;
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

  const hideTooltip = useCallback((anchor?: HTMLElement) => {
    const activeAnchor = activeAnchorRef.current;
    if (anchor && activeAnchor !== anchor) return;
    if (activeAnchor?.isConnected) updateTooltipDescription(activeAnchor, false);
    activeAnchorRef.current = null;
    setTooltip(null);
    setPosition(null);
  }, []);

  const showTooltip = useCallback((anchor: HTMLElement) => {
    normalizeTooltip(anchor);
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
    const text = anchor.dataset.tooltip?.trim();
    if (!text) return;
    if (activeAnchorRef.current !== anchor && activeAnchorRef.current?.isConnected) updateTooltipDescription(activeAnchorRef.current, false);
    updateTooltipDescription(anchor, true);
    activeAnchorRef.current = anchor;
    setPosition(null);
    setTooltip({ anchor, text: tooltipPreview(text) });
  }, []);

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
    const observer = new MutationObserver((records) => {
      for (const record of records) {
        if (record.type === "attributes" && record.target instanceof HTMLElement) normalizeTooltip(record.target);
        if (record.type === "childList") record.addedNodes.forEach(normalizeTooltipTree);
      }
    });
    observer.observe(root, { subtree: true, childList: true, attributes: true, attributeFilter: ["title"] });

    const pointerOver = (event: PointerEvent) => {
      const target = tooltipTarget(event.target);
      if (target) showTooltip(target);
    };
    const pointerOut = (event: PointerEvent) => {
      const target = tooltipTarget(event.target);
      if (!target || event.relatedTarget instanceof Node && target.contains(event.relatedTarget)) return;
      hideTooltip(target);
    };
    const focusIn = (event: FocusEvent) => {
      const target = tooltipTarget(event.target);
      if (target) showTooltip(target);
    };
    const focusOut = (event: FocusEvent) => {
      const target = tooltipTarget(event.target);
      if (!target) return;
      queueMicrotask(() => {
        if (!(document.activeElement instanceof Node && target.contains(document.activeElement))) hideTooltip(target);
      });
    };
    document.addEventListener("pointerover", pointerOver, true);
    document.addEventListener("pointerout", pointerOut, true);
    document.addEventListener("focusin", focusIn, true);
    document.addEventListener("focusout", focusOut, true);
    return () => {
      observer.disconnect();
      document.removeEventListener("pointerover", pointerOver, true);
      document.removeEventListener("pointerout", pointerOut, true);
      document.removeEventListener("focusin", focusIn, true);
      document.removeEventListener("focusout", focusOut, true);
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
