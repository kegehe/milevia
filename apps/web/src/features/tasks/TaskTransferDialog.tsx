// 任务导出 / 导入对话框（docs/50）。
//
// 纯逻辑（解析、预览、文件名）在 lib/task-transfer.ts，这里只负责交互与结果文案。
// 导出保存走 Blob + a[download]：已实测 WebView2 会静默存进系统「下载」文件夹，
// 所以成功态必须把保存位置写出来，否则用户不知道文件去了哪。

import { ChangeEvent, FormEvent, useRef, useState } from "react";

import { TASK_TRANSFER_MAX_BYTES, TASK_TRANSFER_MAX_TASKS, parseTaskTransferFile, previewTaskImport, taskTransferFileName, taskTransferImportBody } from "../../lib/task-transfer";
import type { Request, Task, TaskExportFile, TaskImportResult } from "./task-model";

type TransferMode = "export" | "import";

export function TaskTransferDialog({ mode, projectID, projectName, tasks, request, close, onImported }: {
  mode: TransferMode;
  projectID: string;
  projectName: string;
  tasks: Task[];
  request: Request;
  close: () => void;
  onImported: () => Promise<void>;
}) {
  const [scope, setScope] = useState<"active" | "all">("active");
  const [busy, setBusy] = useState(false);
  const [localError, setLocalError] = useState<string | null>(null);
  const [exported, setExported] = useState<{ name: string; count: number; dropped: number } | null>(null);
  const [importFile, setImportFile] = useState<TaskExportFile | null>(null);
  const [importName, setImportName] = useState("");
  // 去重默认开启；关闭后连文件内相同说明的条目也会各自建一条（与服务端 dedupe=false 一致）。
  const [dedupe, setDedupe] = useState(true);
  const [imported, setImported] = useState<TaskImportResult | null>(null);
  const fileInput = useRef<HTMLInputElement>(null);

  const totalCount = tasks.length;
  const activeCount = tasks.filter((task) => task.status !== "done" && task.status !== "cancelled").length;

  const runExport = async (event: FormEvent) => {
    event.preventDefault();
    if (busy) return;
    setBusy(true);
    setLocalError(null);
    try {
      const file = await request<TaskExportFile>(`/api/projects/${projectID}/tasks/export?scope=${scope}`);
      // 同时保护连接旧服务端的客户端，避免下载无法重新导入的文件。
      if (file.tasks.length > TASK_TRANSFER_MAX_TASKS) throw new Error("当前导出范围超过 2000 条任务，请缩小导出范围。");
      const name = taskTransferFileName(projectName);
      saveJSONFile(file, name);
      setExported({ name, count: file.tasks.length, dropped: file.droppedDependencies || 0 });
    } catch (cause) {
      setLocalError(errorMessage(cause, "导出失败，请稍后重试。"));
    } finally {
      setBusy(false);
    }
  };

  const pickFile = async (event: ChangeEvent<HTMLInputElement>) => {
    const picked = event.target.files?.[0];
    // 清空 value：同一个文件再次选择时也要触发 change。
    event.target.value = "";
    if (!picked) return;
    setLocalError(null);
    setImported(null);
    if (picked.size > TASK_TRANSFER_MAX_BYTES) {
      setImportFile(null);
      setLocalError("文件超过 64MB，请分批导出后再导入。");
      return;
    }
    let text = "";
    try {
      text = await picked.text();
    } catch {
      setLocalError("无法读取文件，请重试。");
      return;
    }
    const parsed = parseTaskTransferFile(text);
    if (!parsed.ok) {
      setImportFile(null);
      setLocalError(parsed.reason);
      return;
    }
    setImportFile(parsed.file);
    setImportName(picked.name);
  };

  const runImport = async (event: FormEvent) => {
    event.preventDefault();
    if (!importFile || busy) return;
    setBusy(true);
    setLocalError(null);
    try {
      const result = await request<TaskImportResult>(`/api/projects/${projectID}/tasks/import${dedupe ? "" : "?dedupe=false"}`, {
        method: "POST",
        body: JSON.stringify(taskTransferImportBody(importFile)),
      });
      setImported(result);
      setImportFile(null);
    } catch (cause) {
      setLocalError(errorMessage(cause, "导入失败，请稍后重试。"));
    } finally {
      setBusy(false);
    }
  };

  const finishImport = async () => {
    // 刷新失败也要关闭：任务已经导入成功，把用户困在结果页只会让他重复导入；
    // 下一次 10 秒轮询会把列表补齐，所以这里吞掉异常而不是让点击处理器抛出未捕获错误。
    try {
      await onImported();
    } catch {
      // ignore: 列表会在下一次轮询刷新
    } finally {
      close();
    }
  };

  const title = mode === "export" ? "导出任务" : "导入任务";
  // 派生而不是 state：勾选去重时预览要立刻跟着变。
  const preview = importFile ? previewTaskImport(importFile, dedupe ? tasks : []) : null;

  return <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="task-transfer-title"><section className="modal task-dialog task-editor-dialog task-transfer-dialog">
    <header className="task-editor-header">
      <div><span>任务</span><h2 id="task-transfer-title">{title}</h2></div>
      <button type="button" className="task-editor-close" title="关闭" aria-label="关闭" disabled={busy} onClick={close}>x</button>
    </header>
    {mode === "export" ? renderExport() : renderImport()}
  </section></div>;

  function renderExport() {
    if (exported) {
      return <div className="task-editor-form"><div className="task-form"><section className="task-form-section">
        <p className="task-transfer-lead">已导出 {exported.count} 条任务。</p>
        <p className="task-transfer-hint">文件已保存到系统「下载」文件夹：<b>{exported.name}</b>（同名文件会自动加序号）。</p>
        {exported.dropped > 0 && <p className="task-transfer-hint">另有 {exported.dropped} 条依赖指向未导出的任务，已忽略。</p>}
      </section></div><footer><button type="button" className="primary" onClick={close}>知道了</button></footer></div>;
    }
    const empty = scope === "active" ? activeCount === 0 : totalCount === 0;
    const tooMany = (scope === "active" ? activeCount : totalCount) > TASK_TRANSFER_MAX_TASKS;
    return <form className="task-editor-form" onSubmit={(event) => void runExport(event)}>
      <div className="task-form"><section className="task-form-section">
        <div className="task-form-section-head"><h3>导出范围</h3></div>
        <div className="task-transfer-options">
          <label><input type="radio" name="task-export-scope" checked={scope === "active"} onChange={() => setScope("active")} />仅未完成（{activeCount}）</label>
          <label><input type="radio" name="task-export-scope" checked={scope === "all"} onChange={() => setScope("all")} />全部任务（{totalCount}）</label>
        </div>
        <p className="task-transfer-hint">选择「全部任务」时，已完成 / 已取消的任务也会导出，但导入后会变成待处理。</p>
        <p className="task-transfer-hint">导出文件包含任务全文，请妥善保管。</p>
        <p className={tooMany ? "error" : "task-transfer-hint"}>单次导出最多 2000 条、64MB。{tooMany ? "当前范围超过条数上限，请缩小导出范围。" : ""}</p>
        {localError && <p className="error" role="alert">{localError}</p>}
      </section></div>
      <footer>
        <button type="button" className="secondary" disabled={busy} onClick={close}>取消</button>
        <button className="primary" disabled={busy || empty || tooMany}>{busy ? "导出中" : empty ? "没有可导出的任务" : "导出"}</button>
      </footer>
    </form>;
  }

  function renderImport() {
    if (imported) {
      const invalid = imported.invalid;
      return <div className="task-editor-form"><div className="task-form"><section className="task-form-section">
        <p className="task-transfer-lead">已导入 {imported.imported} 条任务{imported.duplicates > 0 ? `，跳过 ${imported.duplicates} 条重复` : ""}。</p>
        {imported.dependencies > 0 && <p className="task-transfer-hint">重建了 {imported.dependencies} 条依赖。</p>}
        {imported.satisfiedDependencies > 0 && <p className="task-transfer-hint">{imported.satisfiedDependencies} 条依赖因前置任务已完成而不再阻塞。</p>}
        {imported.droppedDependencies > 0 && <p className="task-transfer-hint">{imported.droppedDependencies} 条依赖因目标任务不存在被忽略。</p>}
        {invalid.length > 0 && <div className="task-transfer-invalid">
          <b>{invalid.length} 条任务无法导入：</b>
          <ul>{invalid.slice(0, 5).map((entry) => <li key={entry.key}>{entry.key}：{entry.reason}</li>)}</ul>
          {invalid.length > 5 && <p>另有 {invalid.length - 5} 条未列出。</p>}
        </div>}
      </section></div><footer><button type="button" className="primary" onClick={() => void finishImport()}>完成</button></footer></div>;
    }
    if (importFile && preview) {
      return <form className="task-editor-form" onSubmit={(event) => void runImport(event)}>
        <div className="task-form"><section className="task-form-section">
          <div className="task-form-section-head"><h3>{importName || "任务导出文件"}</h3></div>
          <p className="task-transfer-lead">共 {preview.tasks} 条任务 · {preview.dependencies} 条依赖{preview.duplicates > 0 ? ` · ${preview.duplicates} 条与现有任务重复，将跳过` : ""}</p>
          <p className="task-transfer-hint">导入后所有任务都是「待处理」；前置已完成的依赖不会阻塞。</p>
          <label className="task-transfer-check"><input type="checkbox" checked={dedupe} onChange={(event) => setDedupe(event.target.checked)} />跳过与现有任务说明相同的条目</label>
          {localError && <p className="error" role="alert">{localError}</p>}
        </section></div>
        <footer>
          <button type="button" className="secondary" disabled={busy} onClick={() => setImportFile(null)}>重新选择</button>
          <button className="primary" disabled={busy}>{busy ? "导入中，请不要关闭窗口" : `导入 ${preview.tasks} 条`}</button>
        </footer>
      </form>;
    }
    return <div className="task-editor-form"><div className="task-form"><section className="task-form-section">
      <p className="task-transfer-lead">选择从另一台电脑导出的任务文件。</p>
      <p className="task-transfer-hint">与现有任务说明完全相同的条目会被跳过；依赖会按文件内的顺序重建。</p>
      <input ref={fileInput} className="task-transfer-file" type="file" accept=".json,application/json" onChange={(event) => void pickFile(event)} />
      {localError && <p className="error" role="alert">{localError}</p>}
    </section></div><footer><button type="button" className="secondary" onClick={close}>取消</button></footer></div>;
  }
}

function errorMessage(cause: unknown, fallback: string): string {
  return cause instanceof Error && cause.message ? cause.message : fallback;
}

// WebView2 会把 blob 下载静默写进系统「下载」文件夹（docs/50 已实测）。
// 延迟 revoke：立即释放偶尔会让下载拿不到内容。
function saveJSONFile(value: unknown, fileName: string) {
  const blob = new Blob([JSON.stringify(value, null, 2)], { type: "application/json" });
  if (blob.size > TASK_TRANSFER_MAX_BYTES) throw new Error("当前导出范围超过 64MB，无法生成可导入的文件，请缩小导出范围。");
  const url = URL.createObjectURL(blob);
  const link = document.createElement("a");
  link.href = url;
  link.download = fileName;
  link.style.display = "none";
  document.body.append(link);
  link.click();
  link.remove();
  window.setTimeout(() => URL.revokeObjectURL(url), 10_000);
}
