import { useEffect, useLayoutEffect, useRef, useState, useCallback } from "react";
import type { ReactNode } from "react";
import { listen, type UnlistenFn } from "@tauri-apps/api/event";
import "./tray-panel.css";

/**
 * 托盘品牌面板。
 *
 * 运行在独立无边框透明窗口 `tray-panel` 中，通过注入的
 * `window.__MILEVIA_TRAY_ACTIONS__` 桥调用 Rust command。
 *
 * 功能：
 * - 显示 Milevia（回到主窗口）
 * - 在线更新：每次面板打开自动检查一次；有新版可点击安装；也可手动再查
 * - 退出
 *
 * 注意：该窗口常驻复用——打开只是 show、失焦关闭只是 hide，组件不会重挂载；
 * 因此"每次打开自动检查"依赖 Rust 在打开面板时发来的 `tray://panel-opened` 事件。
 */

type UpdateInfo = {
  currentVersion: string;
  version: string;
  notes?: string | null;
};

type UpdaterResult = {
  appVersion: string;
  status: "checking" | "complete" | "failed";
  update: UpdateInfo | null;
  error?: string | null;
};

type InstallResult = {
  installed: boolean;
};

type TrayActions = {
  showMain: () => void;
  close: () => void;
  quit: () => void;
  resize?: (width: number, height: number) => void;
  // 更新相关（Tauri 注入；浏览器/测试环境缺失）
  getUpdaterStatus?: () => Promise<UpdaterResult>;
  checkForUpdate?: () => Promise<UpdaterResult>;
  installUpdate?: () => Promise<InstallResult>;
};

declare global {
  interface Window {
    __MILEVIA_TRAY_ACTIONS__?: Partial<TrayActions>;
  }
}

/** 更新行的派生阶段。 */
type UpdatePhase =
  | "idle" // 首次打开事件到达前 / 尚未执行过检查
  | "checking"
  | "available"
  | "upToDate"
  | "checkError"
  | "installError";

export function TrayPanel() {
  const rootRef = useRef<HTMLDivElement>(null);
  const [phase, setPhase] = useState<UpdatePhase>("idle");
  const [version, setVersion] = useState<string>(""); // 当前版本（从检查结果回填）
  const [available, setAvailable] = useState<string>(""); // 检测到的新版本
  const [message, setMessage] = useState<string>(""); // 失败/说明文案

  const runRef = useRef(0); // 每代自增：辨别我们自己发起的检查 vs 过期/被他者覆盖的结果
  const pollTimer = useRef<number | undefined>(undefined); // 协作轮询的定时器句柄

  // 托盘窗口与主窗口共用同一份 CSS bundle：给 <html> 加 tray-window 标记，
  // 让 tray-panel.css 里针对 html/body/#root 的透明背景与 overflow:hidden
  // 只在本窗口生效，避免主窗口项目总览等页面被全局 overflow:hidden 锁死滚动。
  // useLayoutEffect 保证标记在首帧绘制前加上（窗口即便立刻 show 也不闪实底）。
  useLayoutEffect(() => {
    document.documentElement.classList.add("tray-window");
    return () => document.documentElement.classList.remove("tray-window");
  }, []);

  // 卸载/换代时清理可能挂起的协作轮询定时器。
  useEffect(
    () => () => {
      if (pollTimer.current !== undefined) window.clearTimeout(pollTimer.current);
    },
    [],
  );

  // 把一个终态（complete / failed）落成面板文案。
  const settleFrom = useCallback((r: UpdaterResult) => {
    setVersion(r.appVersion);
    if (r.status === "failed") {
      setPhase("checkError");
      setMessage(r.error ?? "更新检查失败，请稍后重试");
    } else if (r.update) {
      setAvailable(r.update.version);
      setMessage(r.update.notes ?? "");
      setPhase("available");
    } else {
      setPhase("upToDate");
    }
  }, []);

  // 协作型终态等待：当我们自己发起的 check_for_update_now 返回的是共享态的
  // checking（意味着此刻有**另一并发**检查正持有 Rust 全局状态、将先去 post
  // 终态），就退而本地轮询 get_updater_status（不额外打网络）直到它落出 checking。
  // 轮询只在“自己不再是最新”时触发，通常一步即中；上限防御异常卡死。
  const pollToTerminal = useCallback(
    (gen: number) => {
      const get = window.__MILEVIA_TRAY_ACTIONS__?.getUpdaterStatus;
      if (!get) {
        // 无轮询通道：无法分辨他者终态，回到可重试的错误态（下次点击会再触发）。
        if (gen === runRef.current) {
          setPhase("checkError");
          setMessage("仍在检查中，请点击重试");
        }
        pollTimer.current = undefined;
        return;
      }
      Promise.resolve(get())
        .then((r): void => {
          if (gen !== runRef.current) return; // 已有新的一次检查接管，停止轮询
          pollTimer.current = undefined;
          if (r.status === "complete" || r.status === "failed") {
            settleFrom(r); // 他者已 posting 终态（结果与本次检查等价），采用之
            return;
          }
          // 仍是 checking：可能刚发起、网络很慢或 45s 超时临近；继续等一小段，
          // 但绑上限，避免检查阻塞时无限轮询。
          let attempts = 0;
          const tick = () => {
            if (gen !== runRef.current) return;
            attempts += 1;
            Promise.resolve(get())
              .then((next) => {
                if (gen !== runRef.current) return;
                if (next.status === "checking") {
                  if (attempts < 220) {
                    pollTimer.current = window.setTimeout(tick, 200); // ~45s 上限后放弃
                    return;
                  }
                  setPhase("checkError");
                  setMessage("仍在检查中，请点击重试");
                  return;
                }
                settleFrom(next);
              })
              .catch(() => {
                if (gen !== runRef.current) return;
                pollTimer.current = undefined;
                setPhase("checkError");
                setMessage("仍在检查中，请点击重试");
              });
          };
          pollTimer.current = window.setTimeout(tick, 200);
        })
        .catch(() => {
          if (gen !== runRef.current) return;
          pollTimer.current = undefined;
          setPhase("checkError");
          setMessage("仍在检查中，请点击重试");
        });
    },
    [settleFrom],
  );

  // 执行一次版本检查（打开自动 / 手动共用）。调用 Rust 的 check_for_update_now
  // （内部会把它设为共享态的最新一次，并返回本次或他子的终态）。
  // - 返回值非 checking：即为我们（最新）的终态，直接 settle。
  // - 返回值仍 checking：说明某并发（如主窗启动 prime）正在 post 终态 →
  //   交给 pollToTerminal 本地轮询收敛，不额外发起网络。
  const runCheck = useCallback(() => {
    if (pollTimer.current !== undefined) window.clearTimeout(pollTimer.current);
    const check = window.__MILEVIA_TRAY_ACTIONS__?.checkForUpdate;
    if (!check) {
      setPhase("checkError");
      setMessage("当前环境不支持在线检查");
      return;
    }
    runRef.current += 1;
    const generation = runRef.current;
    setPhase("checking");
    setAvailable("");
    setMessage("");
    Promise.resolve(check())
      .then((r) => {
        if (generation !== runRef.current) return; // 已发新一轮检查，丢弃过期
        if (r.status === "checking") {
          pollToTerminal(generation); // 他者并发正在 post，协作轮询拿终态
          return;
        }
        settleFrom(r);
      })
      .catch((error: unknown) => {
        if (generation !== runRef.current) return;
        setPhase("checkError");
        setMessage(error instanceof Error ? error.message : "更新检查失败，请稍后重试");
      });
  }, [pollToTerminal, settleFrom]);

  // 每次面板打开自动检查一次：监听 Rust 在 open_tray_panel 里发出的
  // `tray://panel-opened`。组件只挂载一次，靠该事件跨次触发。
  useEffect(() => {
    let unlisten: UnlistenFn | undefined;
    let cancelled = false;
    void listen("tray://panel-opened", () => {
      if (!cancelled) runCheck();
    }).then((fn) => {
      if (cancelled) fn();
      else unlisten = fn;
    });
    return () => {
      cancelled = true;
      unlisten?.();
    };
  }, [runCheck]);

  // 安装更新：先隐藏面板，再调用 Rust 的 install_update，成功后 Rust 会重启应用。
  const applyUpdate = useCallback(() => {
    const install = window.__MILEVIA_TRAY_ACTIONS__?.installUpdate;
    const close = window.__MILEVIA_TRAY_ACTIONS__?.close;
    if (!install) return;
    // 结束可能挂起中的协作轮询：安装态与检查态的终态互斥，避免竞相写 state。
    runRef.current += 1;
    if (pollTimer.current !== undefined) window.clearTimeout(pollTimer.current);
    pollTimer.current = undefined;
    setPhase("checking");
    setMessage("");
    close?.();
    Promise.resolve(install()).catch((error: unknown) => {
      // 失败：面板已隐藏，下次打开会自动重查；此处置 installError，重开可由自动重查复位。
      setMessage(error instanceof Error ? error.message : "更新安装失败，请稍后重试");
      setPhase("installError");
      console.error("[tray] update install failed", error);
    });
  }, []);

  const reCheck = useCallback(() => {
    runCheck();
  }, [runCheck]);

  // 内容自适应：渲染后按内容（行高/最大文案宽）调整窗口尺寸。
  const remeasure = useCallback(() => {
    const el = rootRef.current;
    if (!el) return;
    const w = el.scrollWidth;
    const h = el.scrollHeight;
    if (w > 0 && h > 0 && window.__MILEVIA_TRAY_ACTIONS__?.resize) {
      window.__MILEVIA_TRAY_ACTIONS__.resize(w, h);
    }
  }, []);
  useEffect(() => {
    const raf = requestAnimationFrame(remeasure);
    return () => cancelAnimationFrame(raf);
  }, [remeasure, phase, available, message, version]);

  // Esc 关闭面板
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") window.__MILEVIA_TRAY_ACTIONS__?.close?.();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  // —— 渲染 ——
  const showMain = () => window.__MILEVIA_TRAY_ACTIONS__?.showMain?.();

  const spin = phase === "checking";

  const rowClass =
    "tray-panel-item tray-panel-item--update" +
    (spin ? " tray-panel-item--update-spin" : "") +
    (phase === "available" ? " tray-panel-item--update-new" : "") +
    (phase === "checkError" || phase === "installError"
      ? " tray-panel-item--update-error"
      : "");

  let row: ReactNode;
  switch (phase) {
    case "available":
      row = (
        <button type="button" className={rowClass} onClick={applyUpdate}>
          <span className="tray-update-text">
            <span className="tray-update-label">发现新版本 v{available}，点击更新</span>
            <span className="tray-update-sub">
              当前 v{version}
              {message ? ` · ${message.trim().slice(0, 40)}` : ""}
            </span>
          </span>
        </button>
      );
      break;
    case "upToDate":
      row = (
        <button type="button" className={rowClass} onClick={reCheck}>
          <span className="tray-update-text">
            <span className="tray-update-label">已是最新版本</span>
            <span className="tray-update-sub">当前 v{version} · 点击检查更新</span>
          </span>
        </button>
      );
      break;
    case "checkError":
      row = (
        <button type="button" className={rowClass} onClick={reCheck}>
          <span className="tray-update-text">
            <span className="tray-update-label">检查更新失败，点击重试</span>
            {message && <span className="tray-update-sub">{message}</span>}
          </span>
        </button>
      );
      break;
    case "installError":
      row = (
        <button type="button" className={rowClass} onClick={reCheck}>
          <span className="tray-update-text">
            <span className="tray-update-label">更新失败，点击重试</span>
            {message && <span className="tray-update-sub">{message}</span>}
          </span>
        </button>
      );
      break;
    case "idle":
      row = (
        <button type="button" className={rowClass} onClick={reCheck}>
          检查更新
        </button>
      );
      break;
    default: // checking（含降级兜底）
      row = (
        <button
          type="button"
          className={rowClass}
          disabled
          aria-busy="true"
        >
          正在检查更新…
        </button>
      );
  }

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
        <button type="button" className="tray-panel-item" onClick={showMain}>
          显示 Milevia
        </button>

        <div className="tray-panel-sep" />

        {row}

        <div className="tray-panel-sep" />

        <button
          type="button"
          className="tray-panel-item tray-panel-item-danger"
          onClick={() => window.__MILEVIA_TRAY_ACTIONS__?.quit?.()}
        >
          退出
        </button>
      </nav>
    </main>
  );
}
