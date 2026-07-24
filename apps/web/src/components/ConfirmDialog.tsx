import { ReactNode } from "react";

export interface ConfirmDialogProps {
  title: string;
  message: ReactNode;
  confirmLabel?: string;
  danger?: boolean;
  busy?: boolean;
  onConfirm: () => void | Promise<void>;
  onCancel: () => void;
}

export function ConfirmDialog({ title, message, confirmLabel = "确认", danger, busy, onConfirm, onCancel }: ConfirmDialogProps) {
  return (
    <div className="backdrop" role="dialog" aria-modal="true">
      <section className="modal">
        <header>
          <h2>{title}</h2>
          <button title="关闭" disabled={busy} onClick={onCancel}>x</button>
        </header>
        <p className="permission-confirmation">{message}</p>
        <footer>
          <button className="secondary" disabled={busy} onClick={onCancel}>取消</button>
          <button className={danger ? "primary danger" : "primary"} disabled={busy} onClick={() => void onConfirm()}>
            {busy ? "处理中" : confirmLabel}
          </button>
        </footer>
      </section>
    </div>
  );
}
