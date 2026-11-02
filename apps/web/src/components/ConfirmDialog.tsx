import { ReactNode, useEffect, useRef } from "react";

export interface ConfirmDialogObject {
  title: string;
  meta?: string;
}

export interface ConfirmDialogProps {
  title: string;
  message?: ReactNode;
  confirmLabel?: string;
  busyLabel?: string;
  danger?: boolean;
  busy?: boolean;
  className?: string;
  icon?: ReactNode;
  /** 居中轻量版式（删除任务确认，方案D）：无标题栏与关闭按钮，居中警示图标 + 标题，
   *  底部按钮等宽铺满；Esc / 点击遮罩可关。只影响显式开启的调用方，老版式一行不动。 */
  centered?: boolean;
  /** 操作对象卡片：把要删的东西摆出来看。标题必填；元信息读不到就不传、不渲染。 */
  object?: ConfirmDialogObject;
  onConfirm: () => void | Promise<void>;
  onCancel: () => void;
}

function ConfirmCloseIcon() {
  return <svg className="confirm-dialog-close-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="m7 7 10 10M17 7 7 17" /></svg>;
}

// 居中版式 danger 时的默认警示图标：描边红与 .error 横幅同源（#d4523e），圆底 #fdeee9 同源。
function ConfirmWarnIcon() {
  return <svg className="confirm-dialog-warn-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M12 4.2 21 19.5H3L12 4.2Z" /><path d="M12 10.2v4.4" /><path d="M12 17.2v.2" /></svg>;
}

export function ConfirmDialog({ title, message, confirmLabel = "确认", busyLabel = "处理中", danger, busy, className, icon, centered, object, onConfirm, onCancel }: ConfirmDialogProps) {
  // Hooks 一律无条件调用（不能因为 centered 分支而增减）。
  // 遮罩点击关闭要求「按下与松开都在遮罩上」：从卡片文字按下、拖到遮罩松手的选字操作
  // 会让 click 落在遮罩上，只判 click 会把弹窗误关。
  const backdropDown = useRef(false);
  // Esc 关闭只挂在居中版式上：老版式被十几个页面共用，行为保持原样，不在这里顺手改全局。
  useEffect(() => {
    if (!centered) return;
    const onKeyDown = (event: KeyboardEvent) => { if (event.key === "Escape" && !busy) onCancel(); };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [centered, busy, onCancel]);

  if (centered) {
    const shownIcon = icon ?? (danger ? <ConfirmWarnIcon /> : null);
    return (
      <div
        className="backdrop"
        role="dialog"
        aria-modal="true"
        onMouseDown={(event) => { backdropDown.current = event.target === event.currentTarget; }}
        onClick={(event) => { if (backdropDown.current && event.target === event.currentTarget && !busy) onCancel(); }}
      >
        <section className={`modal confirm-dialog confirm-dialog-centered${className ? ` ${className}` : ""}`}>
          {shownIcon && <span className="confirm-dialog-centered-icon">{shownIcon}</span>}
          <h2>{title}</h2>
          {object && <div className="confirm-dialog-object"><b>{object.title}</b>{object.meta && <small>{object.meta}</small>}</div>}
          {message && <p className="permission-confirmation">{message}</p>}
          <footer>
            {/* 焦点初始落在「取消」：危险确认里 Enter 必须落在安全动作上，不能落在删除上。 */}
            <button type="button" className="secondary" autoFocus disabled={busy} onClick={onCancel}>取消</button>
            <button className={danger ? "primary danger" : "primary"} disabled={busy} onClick={() => void onConfirm()}>
              {busy ? busyLabel : confirmLabel}
            </button>
          </footer>
        </section>
      </div>
    );
  }

  return (
    <div className="backdrop" role="dialog" aria-modal="true">
      <section className={`modal confirm-dialog${className ? ` ${className}` : ""}`}>
        <header>
          <div className="confirm-dialog-heading">{icon && <span className="confirm-dialog-icon">{icon}</span>}<h2>{title}</h2></div>
          <button type="button" className="confirm-dialog-close" title="关闭" aria-label="关闭" disabled={busy} onClick={onCancel}><ConfirmCloseIcon /></button>
        </header>
        <p className="permission-confirmation">{message}</p>
        <footer>
          <button type="button" className="secondary" disabled={busy} onClick={onCancel}>取消</button>
          <button className={danger ? "primary danger" : "primary"} disabled={busy} onClick={() => void onConfirm()}>
            {busy ? busyLabel : confirmLabel}
          </button>
        </footer>
      </section>
    </div>
  );
}
