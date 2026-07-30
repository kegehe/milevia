import { ReactNode } from "react";

export interface ConfirmDialogProps {
  title: string;
  message: ReactNode;
  confirmLabel?: string;
  danger?: boolean;
  busy?: boolean;
  className?: string;
  icon?: ReactNode;
  onConfirm: () => void | Promise<void>;
  onCancel: () => void;
}

function ConfirmCloseIcon() {
  return <svg className="confirm-dialog-close-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="m7 7 10 10M17 7 7 17" /></svg>;
}

export function ConfirmDialog({ title, message, confirmLabel = "确认", danger, busy, className, icon, onConfirm, onCancel }: ConfirmDialogProps) {
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
            {busy ? "处理中" : confirmLabel}
          </button>
        </footer>
      </section>
    </div>
  );
}
