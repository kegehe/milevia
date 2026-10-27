import { useCallback, useEffect, useLayoutEffect, useRef, useState, type MouseEvent } from "react";
import { createPortal } from "react-dom";

type MenuItem = { label: string; disabled?: boolean; title?: string; action: () => void };
type MenuState = { x: number; y: number; label: string; items: MenuItem[] };
const handledEvents = new WeakSet<Event>();

export function useContextMenu() {
  const [state, setState] = useState<MenuState | null>(null);
  const closeMenu = useCallback(() => setState(null), []);
  const openMenu = (event: MouseEvent, label: string, items: MenuItem[]) => {
    // 最内层菜单处理一次即可；保留原生冒泡，让已有菜单的 document 关闭监听继续工作。
    if (handledEvents.has(event.nativeEvent)) return;
    handledEvents.add(event.nativeEvent);
    event.preventDefault();
    setState({ x: event.clientX, y: event.clientY, label, items });
  };
  return { openMenu, closeMenu, menu: state && <ContextMenu state={state} close={closeMenu} /> };
}

function ContextMenu({ state, close }: { state: MenuState; close: () => void }) {
  const ref = useRef<HTMLDivElement>(null);

  useLayoutEffect(() => {
    const menu = ref.current!;
    const previousFocus = document.activeElement;
    const bounds = menu.getBoundingClientRect();
    menu.style.left = `${Math.max(4, Math.min(state.x, window.innerWidth - bounds.width - 4))}px`;
    menu.style.top = `${Math.max(4, Math.min(state.y, window.innerHeight - bounds.height - 4))}px`;
    (menu.querySelector<HTMLButtonElement>("button:not(:disabled)") ?? menu).focus({ preventScroll: true });
    return () => {
      if (menu.contains(document.activeElement) && previousFocus instanceof HTMLElement && previousFocus.isConnected) {
        previousFocus.focus({ preventScroll: true });
      }
    };
  }, [state]);

  useEffect(() => {
    const outside = (event: Event) => {
      if (event.target instanceof Node && ref.current?.contains(event.target)) return;
      close();
    };
    const keydown = (event: KeyboardEvent) => {
      if (event.key === "Escape" || event.key === "Tab") {
        if (event.key === "Escape") event.preventDefault();
        close();
      }
    };
    document.addEventListener("pointerdown", outside, true);
    document.addEventListener("contextmenu", outside, true);
    document.addEventListener("scroll", outside, true);
    document.addEventListener("keydown", keydown);
    window.addEventListener("resize", close);
    window.addEventListener("blur", close);
    return () => {
      document.removeEventListener("pointerdown", outside, true);
      document.removeEventListener("contextmenu", outside, true);
      document.removeEventListener("scroll", outside, true);
      document.removeEventListener("keydown", keydown);
      window.removeEventListener("resize", close);
      window.removeEventListener("blur", close);
    };
  }, [close]);

  return createPortal(<div ref={ref} className="app-context-menu" role="menu" aria-label={state.label} tabIndex={-1}
    style={{ left: state.x, top: state.y }}
    onContextMenu={(event) => { event.preventDefault(); event.stopPropagation(); }}
    onKeyDown={(event) => {
      if (!["ArrowDown", "ArrowUp", "Home", "End"].includes(event.key)) return;
      event.preventDefault();
      const buttons = Array.from(ref.current!.querySelectorAll<HTMLButtonElement>("button:not(:disabled)"));
      const current = buttons.indexOf(document.activeElement as HTMLButtonElement);
      const index = event.key === "Home" ? 0 : event.key === "End" ? buttons.length - 1
        : (current + (event.key === "ArrowDown" ? 1 : -1) + buttons.length) % buttons.length;
      buttons[index]?.focus();
    }}>
    {state.items.map((item) => <button key={item.label} type="button" role="menuitem" disabled={item.disabled} title={item.title}
      onClick={(event) => { event.stopPropagation(); close(); item.action(); }}>{item.label}</button>)}
  </div>, document.body);
}

/** 在菜单抢走焦点前保存选区；允许跨内容选择，但排除与当前内容无关的残留选区。 */
export function selectedTextWithin(element: HTMLElement): string {
  const selection = window.getSelection();
  if (!selection?.rangeCount || !selection.getRangeAt(0).intersectsNode(element)) return "";
  return selection.toString();
}
