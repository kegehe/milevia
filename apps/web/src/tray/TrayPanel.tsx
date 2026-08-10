import { useCallback, useEffect, useRef } from "react";
import "./tray-panel.css";

/**
 * 托盘品牌面板（替代原生托盘菜单）。
 *
 * 运行在独立的无边框透明窗口 `tray-panel` 中，通过注入的
 * `window.__MILEVIA_TRAY_ACTIONS__` 桥调用 Rust command。
 *
 * 当前为最简形态：显示 Milevia + 退出。后续需要更多功能时在此扩展。
 */
type TrayActions = {
  showMain: () => void;
  close: () => void;
  quit: () => void;
  resize?: (width: number, height: number) => void;
};

declare global {
  interface Window {
    __MILEVIA_TRAY_ACTIONS__?: TrayActions;
  }
}

function actions(): TrayActions {
  return (
    window.__MILEVIA_TRAY_ACTIONS__ ?? {
      showMain: () => {},
      close: () => {},
      quit: () => {},
    }
  );
}

export function TrayPanel() {
  const rootRef = useRef<HTMLElement>(null);

  // 内容自适应：渲染后按实际内容尺寸调整窗口
  const remeasure = useCallback(() => {
    const el = rootRef.current;
    if (!el) return;
    const w = el.scrollWidth;
    const h = el.scrollHeight;
    if (w > 0 && h > 0) actions().resize?.(w, h);
  }, []);
  useEffect(() => {
    const raf = requestAnimationFrame(remeasure);
    return () => cancelAnimationFrame(raf);
  }, [remeasure]);

  // Esc 关闭面板
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") actions().close();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  return (
    <main className="tray-panel" ref={rootRef}>
      <header className="tray-panel-brand">
        <img className="tray-panel-mark" src="/milevia-mark.svg" alt="" />
        <span className="tray-panel-brand-name">
          <strong>Mile</strong>
          <em>via</em>
        </span>
      </header>

      <nav className="tray-panel-items" aria-label="Milevia 快捷操作">
        <button
          className="tray-panel-item"
          onClick={() => actions().showMain()}
        >
          显示 Milevia
        </button>
        <div className="tray-panel-sep" />
        <button
          className="tray-panel-item tray-panel-item-danger"
          onClick={() => actions().quit()}
        >
          退出
        </button>
      </nav>
    </main>
  );
}
