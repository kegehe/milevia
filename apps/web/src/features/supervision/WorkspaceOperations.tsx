import { useEffect, useState } from "react";
import { api } from "../../lib/api";

type Operation = { id: string; path: string; state: "active" | "unconfirmed" | "release_pending"; updatedAt: string };
const stateText = { active: "执行中", unconfirmed: "退出状态待核对", release_pending: "已结束，等待交还" };

export function WorkspaceOperations({ projectId }: { projectId: string }) {
  const [operations, setOperations] = useState<Operation[]>([]);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState("");
  const [refresh, setRefresh] = useState(0);
  useEffect(() => {
    let disposed = false;
    let timer: ReturnType<typeof setTimeout>;
    setOperations([]); setError(""); setBusy("");
    async function poll() {
      try {
        const value = await api<Operation[]>(`/api/projects/${projectId}/workspace-operations`);
        if (!disposed) { setOperations(value); setError(""); }
      } catch (cause) { if (!disposed) setError(cause instanceof Error ? cause.message : String(cause)); }
      if (!disposed) timer = setTimeout(() => void poll(), 5000);
    }
    void poll();
    return () => { disposed = true; clearTimeout(timer); };
  }, [projectId, refresh]);
  async function reconcile(operation: Operation) {
    setBusy(operation.id); setError("");
    try {
      await api(`/api/projects/${projectId}/workspace-operations/${operation.id}/reconcile`, { method: "POST" });
      setRefresh(value => value + 1);
    } catch (cause) { setError(cause instanceof Error ? cause.message : String(cause)); }
    finally { setBusy(""); }
  }
  return <details className="supervision-workspace-operations">
    <summary>普通操作与目录交接{operations.length ? `（${operations.length}）` : ""}</summary>
    <p>普通操作结束并交还保护后，托管才能接管目录。交还失败会自动重试；退出状态不明的操作会保留保护。这里只显示当前项目连接下、由当前控制服务登记的操作，其他连接或控制服务的占用仍以目标核对结果为准。</p>
    {error && <p role="alert">{error}</p>}
    {!operations.length && !error && <p>当前控制服务没有普通操作的目录占用记录。</p>}
    {operations.map(operation => <div key={operation.id}>
      <p>{stateText[operation.state]} · <code>{operation.path}</code></p>
      <p>最近记录：{new Date(operation.updatedAt).toLocaleString()}</p>
      {operation.state === "unconfirmed" && <p>请先核对并停止原目标上的执行。仅有进程退出猜测或手动停止声明不会解除保护；核对按钮只接受目标已交还的回执。</p>}
      {operation.state !== "active" && <button type="button" disabled={!!busy} onClick={() => void reconcile(operation)}>{busy === operation.id ? "正在核对…" : operation.state === "release_pending" ? "重试交还" : "核对目标交还状态"}</button>}
    </div>)}
  </details>;
}
