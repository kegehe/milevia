import { FormEvent, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { priorityLabels, Request, statusLabels, Task, TaskDetail, TaskStatus, Priority, taskDisplayTitle } from "./task-model";
import { ConfirmDialog } from "../../components/ConfirmDialog";
type EditorState = { task?: Task } | null;
type ReviewAction = "accept" | "request_changes";

const columnDefinitions: { id: string; label: string; statuses: TaskStatus[] }[] = [
  { id: "todo", label: "待处理", statuses: ["todo"] },
  { id: "running", label: "执行中", statuses: ["running"] },
  { id: "awaiting_review", label: "待验收", statuses: ["awaiting_review"] },
  { id: "action_required", label: "需处理", statuses: ["action_required"] },
  { id: "done", label: "已完成", statuses: ["done"] },
];

type DragData = { taskID: string; sourceColumnID: string; sourceIndex: number };
type PendingConfirm = { title: string; message: React.ReactNode; danger?: boolean; onConfirm: () => void; onCancel: () => void } | null;

export function TaskBoard({ projectID, initialTaskID, permissionMode, request, fail, close, onDispatched }: { projectID: string; initialTaskID?: string; permissionMode?: "approval_required" | "full_control"; request: Request; fail: (message: string) => void; close: () => void; onDispatched: (message: { id: string; role: "user" | "assistant"; content: string; createdAt: string }, runID: string) => void }) {
  const [tasks, setTasks] = useState<Task[]>([]);
  const [view, setView] = useState<"board" | "list">("board");
  const [includeCancelled, setIncludeCancelled] = useState(false);
  const [editor, setEditor] = useState<EditorState>(null);
  const [detail, setDetail] = useState<TaskDetail | null>(null);
  const [busy, setBusy] = useState("");
  const [pendingConfirm, setPendingConfirm] = useState<PendingConfirm>(null);
  const mountedRef = useRef(true);

  useEffect(() => {
    mountedRef.current = true;
    return () => { mountedRef.current = false; };
  }, []);

  const loadTasks = useCallback(async () => {
    const next = await request<Task[]>(`/api/projects/${projectID}/tasks`);
    if (mountedRef.current) setTasks(next);
    return next;
  }, [projectID, request]);

  const loadDetail = useCallback(async (taskID: string) => {
    const next = await request<TaskDetail>(`/api/tasks/${taskID}`);
    if (mountedRef.current) setDetail(next);
    return next;
  }, [request]);

  useEffect(() => { void loadTasks().catch((cause) => { if (mountedRef.current) fail(cause instanceof Error ? cause.message : "无法加载任务"); }); }, [fail, loadTasks]);
  useEffect(() => {
    const interval = window.setInterval(() => { void loadTasks().catch(() => undefined); }, 10_000);
    return () => window.clearInterval(interval);
  }, [loadTasks]);
  useEffect(() => {
    if (!detail) return;
    const taskID = detail.id;
    const interval = window.setInterval(() => { void loadDetail(taskID).catch(() => undefined); }, 10_000);
    return () => window.clearInterval(interval);
  }, [detail?.id, loadDetail]);
  useEffect(() => {
    if (!initialTaskID) return;
    void loadDetail(initialTaskID).catch((cause) => { if (mountedRef.current) fail(cause instanceof Error ? cause.message : "无法加载任务详情"); });
  }, [fail, initialTaskID, loadDetail]);

  const visibleTasks = useMemo(() => includeCancelled ? tasks : tasks.filter((task) => task.status !== "cancelled"), [includeCancelled, tasks]);
  const openDetail = (taskID: string) => { void loadDetail(taskID).catch((cause) => { if (mountedRef.current) fail(cause instanceof Error ? cause.message : "无法加载任务详情"); }); };
  const refresh = async (taskID?: string) => {
    await loadTasks();
    if (taskID) await loadDetail(taskID);
  };

  const dispatch = async () => {
    if (!detail) return;
    if (!mountedRef.current) return;
    setBusy("dispatch");
    try {
      const result = await request<{ message: { id: string; role: "user" | "assistant"; content: string; createdAt: string }; runId: string }>(`/api/tasks/${detail.id}/dispatch`, { method: "POST", body: "{}" });
      if (!mountedRef.current) return;
      onDispatched(result.message, result.runId);
      void loadTasks().catch((cause) => { if (mountedRef.current) fail(cause instanceof Error ? cause.message : "无法刷新任务列表"); });
      close();
    } catch (cause) { if (mountedRef.current) fail(cause instanceof Error ? cause.message : "无法下发任务"); }
    finally { if (mountedRef.current) setBusy(""); }
  };

  const transition = async (action: "cancel" | "reopen" | "stop") => {
    if (!detail) return;
    if (!mountedRef.current) return;
    setBusy(action);
    const doTransition = async (force: boolean) => {
      const url = force ? `/api/tasks/${detail.id}/${action}?force=true` : `/api/tasks/${detail.id}/${action}`;
      await request(url, { method: "POST", body: "{}" });
    };
    try {
      await doTransition(false);
      if (!mountedRef.current) return;
      await refresh(detail.id);
    }
    catch (cause) {
      if (!mountedRef.current) return;
      const message = cause instanceof Error ? cause.message : "";
      if (action === "stop" && (message.includes("queued") || message.includes("other queued"))) {
        setPendingConfirm({
          title: "强制停止",
          message: "该对话还有其他排队中或执行中的任务，强制停止将一并取消它们。是否继续？",
          danger: true,
          onConfirm: () => void (async () => {
            if (!mountedRef.current) return;
            setPendingConfirm(null);
            setBusy("stop");
            try {
              await doTransition(true);
              if (!mountedRef.current) return;
              await refresh(detail.id);
            } catch (cause2) { if (mountedRef.current) fail(cause2 instanceof Error ? cause2.message : "无法更新任务"); }
            finally { if (mountedRef.current) setBusy(""); }
          })(),
          onCancel: () => { if (mountedRef.current) { setPendingConfirm(null); setBusy(""); } },
        });
        return;
      }
      fail(cause instanceof Error ? cause.message : "无法更新任务");
    }
    if (mountedRef.current) setBusy("");
  };
  const deleteTask = () => {
    if (!detail) return;
    setPendingConfirm({
      title: "删除任务",
      message: <>确认删除任务「<b>{taskDisplayTitle(detail)}</b>」？此操作不可撤销，所有执行记录将被永久删除。</>,
      danger: true,
      onConfirm: () => void (async () => {
        if (!mountedRef.current) return;
        setPendingConfirm(null);
        setBusy("delete");
        try { await request(`/api/tasks/${detail.id}`, { method: "DELETE" }); if (mountedRef.current) { setDetail(null); await loadTasks(); } }
        catch (cause) { if (mountedRef.current) fail(cause instanceof Error ? cause.message : "无法删除任务"); }
        finally { if (mountedRef.current) setBusy(""); }
      })(),
      onCancel: () => { if (mountedRef.current) setPendingConfirm(null); },
    });
  };
  const confirmTransition = (action: "cancel" | "reopen") => {
    setPendingConfirm({
      title: action === "cancel" ? "取消任务" : "重新打开",
      message: action === "cancel" ? "确认取消该任务？" : "确认重新打开该任务？",
      danger: action === "cancel",
      onConfirm: () => { if (mountedRef.current) { setPendingConfirm(null); void transition(action); } },
      onCancel: () => { if (mountedRef.current) setPendingConfirm(null); },
    });
  };

  const moveTask = async (taskID: string, direction: "up" | "down") => {
    const ordered = [...visibleTasks].sort((left, right) => new Date(right.createdAt).getTime() - new Date(left.createdAt).getTime() || left.position - right.position || left.title.localeCompare(right.title, "zh-CN"));
    const index = ordered.findIndex((task) => task.id === taskID);
    const neighbor = ordered[index + (direction === "up" ? -1 : 1)];
    const task = ordered[index];
    if (!task || !neighbor) return;
    if (!mountedRef.current) return;
    setBusy("move");
    try {
      const position = direction === "up" ? neighbor.position - 0.5 : neighbor.position + 0.5;
      await request(`/api/tasks/${task.id}`, { method: "PATCH", body: JSON.stringify({ title: task.title, description: task.description, acceptanceCriteria: task.acceptanceCriteria, priority: task.priority, position }) });
      if (!mountedRef.current) return;
      await refresh(task.id);
    } catch (cause) { if (mountedRef.current) fail(cause instanceof Error ? cause.message : "无法调整任务顺序"); }
    finally { if (mountedRef.current) setBusy(""); }
  };

  const handleDrop = async (taskID: string, targetColumnID: string, targetIndex: number) => {
    const task = tasks.find((t) => t.id === taskID);
    if (!task) return;
    const sourceDef = columnDefinitions.find((d) => d.statuses.includes(task.status));
    const targetDef = columnDefinitions.find((d) => d.id === targetColumnID);
    if (!targetDef) return;
    let targetStatus: TaskStatus;
    targetStatus = targetDef.statuses[0];
    const sameColumn = sourceDef?.id === targetColumnID;

    if (sameColumn) {
      const columnTasks = visibleTasks.filter((t) => targetDef.statuses.includes(t.status))
        .sort((left, right) => new Date(right.createdAt).getTime() - new Date(left.createdAt).getTime() || left.position - right.position || left.title.localeCompare(right.title, "zh-CN"));
      const currentIndex = columnTasks.findIndex((t) => t.id === taskID);
      if (currentIndex < 0 || currentIndex === targetIndex) return;
      const reordered = [...columnTasks];
      reordered.splice(currentIndex, 1);
      reordered.splice(targetIndex, 0, columnTasks[currentIndex]);
      const prev = reordered[targetIndex - 1];
      const next = reordered[targetIndex + 1];
      let newPosition: number;
      if (!prev) newPosition = next ? next.position - 1 : task.position;
      else if (!next) newPosition = prev.position + 1;
      else newPosition = (prev.position + next.position) / 2;
      if (!mountedRef.current) return;
      setBusy("move");
      try {
        await request(`/api/tasks/${task.id}`, { method: "PATCH", body: JSON.stringify({ title: task.title, description: task.description, acceptanceCriteria: task.acceptanceCriteria, priority: task.priority, position: newPosition }) });
        if (!mountedRef.current) return;
        await refresh(task.id);
      } catch (cause) { if (mountedRef.current) fail(cause instanceof Error ? cause.message : "无法调整任务顺序"); }
      finally { if (mountedRef.current) setBusy(""); }
    } else {
      if (task.status === targetStatus) return;
      if (!mountedRef.current) return;
      setBusy("move");
      try {
        const columnTasks = visibleTasks.filter((t) => targetDef.statuses.includes(t.status))
          .sort((left, right) => new Date(right.createdAt).getTime() - new Date(left.createdAt).getTime() || left.position - right.position || left.title.localeCompare(right.title, "zh-CN"));
        const prev = columnTasks[targetIndex - 1];
        const next = columnTasks[targetIndex];
        let newPosition: number;
        if (columnTasks.length === 0) newPosition = task.position;
        else if (!prev) newPosition = next ? next.position - 1 : task.position;
        else if (!next) newPosition = prev.position + 1;
        else newPosition = (prev.position + next.position) / 2;
        await request(`/api/tasks/${task.id}`, { method: "PATCH", body: JSON.stringify({ title: task.title, description: task.description, acceptanceCriteria: task.acceptanceCriteria, priority: task.priority, position: newPosition, status: targetStatus }) });
        if (!mountedRef.current) return;
        await refresh(task.id);
      } catch (cause) { if (mountedRef.current) fail(cause instanceof Error ? cause.message : "无法移动任务"); }
      finally { if (mountedRef.current) setBusy(""); }
    }
  };

  return <section className="task-workspace" aria-label="项目任务">
    <header className="task-workspace-head">
      <div><label>PROJECT TASKS</label><h2>任务编排</h2><p>手动下发，人工验收。每个任务独立执行。当前执行权限：{permissionMode === "full_control" ? "完全控制" : "默认权限"}。</p></div>
      <div className="task-workspace-actions">
        <div className="task-view-switch" aria-label="任务视图"><button className={view === "board" ? "active" : ""} onClick={() => setView("board")}>看板</button><button className={view === "list" ? "active" : ""} onClick={() => setView("list")}>列表</button></div>
        <label className="task-cancelled-toggle"><input type="checkbox" checked={includeCancelled} onChange={(event) => setIncludeCancelled(event.target.checked)} />显示已取消</label>
        <button className="secondary" onClick={close}>返回对话</button>
        <button className="primary" onClick={() => setEditor({})}>新建任务</button>
      </div>
    </header>
    {visibleTasks.length === 0 ? <div className="task-empty"><h3>还没有任务</h3><p>将可验证的开发事项加入项目，手动下发执行。</p><button className="primary" onClick={() => setEditor({})}>新建任务</button></div> : view === "board" ? <TaskBoardColumns tasks={visibleTasks} open={openDetail} onDrop={handleDrop} /> : <TaskList tasks={visibleTasks} open={openDetail} />}
    {editor && <TaskEditor projectID={projectID} task={editor.task} request={request} close={() => setEditor(null)} saved={async (taskID) => { setEditor(null); await refresh(taskID); }} fail={fail} />}
    {detail && <TaskDetailDialog detail={detail} permissionMode={permissionMode} busy={busy} close={() => setDetail(null)} refresh={() => refresh(detail.id)} dispatch={dispatch} transition={transition} deleteTask={deleteTask} confirmTransition={confirmTransition} edit={() => { const task = tasks.find((item) => item.id === detail.id); if (task) { setDetail(null); setEditor({ task }); } }} move={moveTask} canMoveUp={visibleTasks.some((item) => item.position < detail.position)} canMoveDown={visibleTasks.some((item) => item.position > detail.position)} request={request} fail={fail} />}
    {pendingConfirm && createPortal(<ConfirmDialog title={pendingConfirm.title} message={pendingConfirm.message} danger={pendingConfirm.danger} onConfirm={pendingConfirm.onConfirm} onCancel={pendingConfirm.onCancel} />, document.body)}
  </section>;
}

function TaskBoardColumns({ tasks, open, onDrop }: { tasks: Task[]; open: (taskID: string) => void; onDrop: (taskID: string, columnID: string, index: number) => Promise<void> }) {
  const [showAllDone, setShowAllDone] = useState(false);
  const [dragOverColumn, setDragOverColumn] = useState<string | null>(null);
  const [dragOverIndex, setDragOverIndex] = useState<number | null>(null);
  const [draggingTaskID, setDraggingTaskID] = useState<string | null>(null);
  const columnRefs = useRef<Record<string, HTMLElement | null>>({});
  const grouped = (definition: typeof columnDefinitions[number]) => tasks.filter((task) => definition.statuses.includes(task.status))
    .sort((left, right) => new Date(right.createdAt).getTime() - new Date(left.createdAt).getTime() || left.position - right.position || left.title.localeCompare(right.title, "zh-CN"));
  const DONE_LIMIT = 5;

  const handleColumnDragOver = (e: React.DragEvent, columnID: string) => {
    e.preventDefault();
    e.dataTransfer.dropEffect = "move";
    setDragOverColumn(columnID);
  };
  const handleColumnDragLeave = (e: React.DragEvent, columnID: string) => {
    const columnEl = columnRefs.current[columnID];
    if (columnEl && columnEl.contains(e.relatedTarget as Node)) return;
    setDragOverColumn(null);
    setDragOverIndex(null);
  };
  const handleColumnDrop = async (e: React.DragEvent, columnID: string) => {
    e.preventDefault();
    setDragOverColumn(null);
    setDragOverIndex(null);
    const data = e.dataTransfer.getData("application/x-task-drag");
    if (!data) return;
    const dragData: DragData = JSON.parse(data);
    const columnTasks = tasks.filter((t) => {
      const def = columnDefinitions.find((d) => d.id === columnID);
      if (!def) return false;
      return def.statuses.includes(t.status);
    }).sort((left, right) => new Date(right.createdAt).getTime() - new Date(left.createdAt).getTime() || left.position - right.position || left.title.localeCompare(right.title, "zh-CN"));
    const targetIndex = dragOverIndex !== null ? Math.min(dragOverIndex, columnTasks.length) : columnTasks.length;
    await onDrop(dragData.taskID, columnID, targetIndex);
  };
  const handleTaskDragStart = (e: React.DragEvent, taskID: string, columnID: string, index: number) => {
    setDraggingTaskID(taskID);
    const data: DragData = { taskID, sourceColumnID: columnID, sourceIndex: index };
    e.dataTransfer.setData("application/x-task-drag", JSON.stringify(data));
    e.dataTransfer.effectAllowed = "move";
  };
  const handleTaskDragEnd = () => {
    setDraggingTaskID(null);
    setDragOverColumn(null);
    setDragOverIndex(null);
  };
  const handleTaskDragOver = (e: React.DragEvent, columnID: string, index: number) => {
    e.preventDefault();
    e.stopPropagation();
    e.dataTransfer.dropEffect = "move";
    setDragOverColumn(columnID);
    setDragOverIndex(index);
  };
  const isDraggable = (task: Task, _columnID: string): boolean => {
    if (task.status === "running" || task.status === "done") return false;
    return true;
  };

  return <div className="task-board-columns">{columnDefinitions.map((definition) => {
    const items = grouped(definition);
    const isDone = definition.id === "done";
    const visible = isDone && !showAllDone ? items.slice(0, DONE_LIMIT) : items;
    const hidden = isDone ? Math.max(0, items.length - DONE_LIMIT) : 0;
    const isDragOver = dragOverColumn === definition.id;
    return <section
      className={`task-board-column column-${definition.id}${isDragOver ? " drag-over" : ""}`}
      key={definition.id}
      ref={(el) => { columnRefs.current[definition.id] = el; }}
      onDragOver={(e) => handleColumnDragOver(e, definition.id)}
      onDragLeave={(e) => handleColumnDragLeave(e, definition.id)}
      onDrop={(e) => handleColumnDrop(e, definition.id)}
    >
      <header><h3>{definition.label}</h3><b>{items.length}</b></header>
      <div>
        {visible.map((task, index) => (
          <TaskItem key={task.id} task={task} open={open} columnID={definition.id} index={index} isDragging={draggingTaskID === task.id} draggable={isDraggable(task, definition.id)} onDragStart={handleTaskDragStart} onDragEnd={handleTaskDragEnd} onDragOver={handleTaskDragOver} />
        ))}
        {isDone && !showAllDone && hidden > 0 && <button className="task-show-more" onClick={() => setShowAllDone(true)}>显示更多（+{hidden}）</button>}
        <div className={`task-drop-indicator${dragOverColumn === definition.id && dragOverIndex === visible.length ? " active" : ""}`} onDragOver={(e) => handleTaskDragOver(e, definition.id, visible.length)} onDrop={(e) => handleColumnDrop(e, definition.id)} />
      </div>
    </section>;
  })}</div>;
}

function TaskList({ tasks, open }: { tasks: Task[]; open: (taskID: string) => void }) {
  const sorted = [...tasks].sort((left, right) => new Date(right.createdAt).getTime() - new Date(left.createdAt).getTime() || left.position - right.position || left.title.localeCompare(right.title, "zh-CN"));
  return <div className="task-list"><div className="task-list-head"><span>任务</span><span>状态</span><span>优先级</span><span>最近更新</span></div>{sorted.map((task) => <button key={task.id} className="task-list-row" onClick={() => open(task.id)}><span><b>{taskDisplayTitle(task)}</b><small>{task.description}</small></span><StatusBadge task={task} /><em className={`priority-${task.priority}`}>{priorityLabels[task.priority]}</em><time>{formatDate(task.updatedAt)}</time></button>)}</div>;
}

function TaskItem({ task, open, columnID, index, isDragging, draggable, onDragStart, onDragEnd, onDragOver }: { task: Task; open: (taskID: string) => void; columnID: string; index: number; isDragging: boolean; draggable: boolean; onDragStart: (e: React.DragEvent, taskID: string, columnID: string, index: number) => void; onDragEnd: () => void; onDragOver: (e: React.DragEvent, columnID: string, index: number) => void }) {
  const dragged = useRef(false);
  return <button
    className={`task-item priority-${task.priority}${isDragging ? " dragging" : ""}${!draggable ? " not-draggable" : ""}`}
    draggable={draggable}
    onClick={() => { if (dragged.current) { dragged.current = false; return; } open(task.id); }}
    onDragStart={(e) => { if (!draggable) { e.preventDefault(); return; } dragged.current = true; onDragStart(e, task.id, columnID, index); }}
    onDragEnd={() => { setTimeout(() => { dragged.current = false; }, 0); onDragEnd(); }}
    onDragOver={(e) => { if (draggable) onDragOver(e, columnID, index); }}
  >
    <div className="task-item-top"><StatusBadge task={task} />{draggable && <span className="task-drag-handle" title="拖拽排序">⠿</span>}<span>{priorityLabels[task.priority]}</span></div>
    <b>{taskDisplayTitle(task)}</b>
    <p>{task.description}</p>
  </button>;
}

function StatusBadge({ task }: { task: Task }) {
  const queued = task.status === "running" && task.lastRun?.status === "queued";
  const label = queued ? "队列中" : statusLabels[task.status];
  return <span className={`task-status ${queued ? "action_required" : task.status}`}>{label}</span>;
}

function TaskEditor({ projectID, task, request, close, saved, fail }: { projectID: string; task?: Task; tasks?: Task[]; request: Request; close: () => void; saved: (taskID: string) => Promise<void>; fail: (message: string) => void }) {
  const [title, setTitle] = useState(task?.title || "");
  const [description, setDescription] = useState(task?.description || "");
  const [acceptanceCriteria, setAcceptanceCriteria] = useState(task?.acceptanceCriteria || "");
  const [priority, setPriority] = useState<Priority>(task?.priority || "normal");
  const [busy, setBusy] = useState(false);
  const mountedRef = useRef(true);
  useEffect(() => {
    mountedRef.current = true;
    return () => { mountedRef.current = false; };
  }, []);
  const save = async (event: FormEvent) => {
    event.preventDefault();
    if (!mountedRef.current) return;
    setBusy(true);
    try {
      const payload = { title, description, acceptanceCriteria, priority, position: task?.position || 0 };
      const result = await request<Task>(task ? `/api/tasks/${task.id}` : `/api/projects/${projectID}/tasks`, { method: task ? "PATCH" : "POST", body: JSON.stringify(payload) });
      if (!mountedRef.current) return;
      await saved(result.id);
    } catch (cause) { if (mountedRef.current) fail(cause instanceof Error ? cause.message : "无法保存任务"); }
    finally { if (mountedRef.current) setBusy(false); }
  };
  return <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="task-editor-title"><section className="modal task-dialog"><header><div><label>PROJECT TASK</label><h2 id="task-editor-title">{task ? "编辑任务" : "新建任务"}</h2></div><button title="关闭" disabled={busy} onClick={close}>x</button></header><form onSubmit={(event) => void save(event)}><div className="task-form"><label>任务名称 <small>（可选）</small><input autoFocus maxLength={120} value={title} onChange={(event) => setTitle(event.target.value)} placeholder="例如：实现项目任务看板" /></label><label>任务说明<textarea required maxLength={12000} value={description} onChange={(event) => setDescription(event.target.value)} placeholder="说明背景、范围、限制和需要完成的实现。" /></label><label>验收条件 <small>（可选）</small><textarea maxLength={12000} value={acceptanceCriteria} onChange={(event) => setAcceptanceCriteria(event.target.value)} placeholder="用可验证的结果描述完成标准。" /></label><label>优先级<select value={priority} onChange={(event) => setPriority(event.target.value as Priority)}>{Object.entries(priorityLabels).map(([value, label]) => <option key={value} value={value}>{label}</option>)}</select></label></div><footer><button type="button" className="secondary" disabled={busy} onClick={close}>取消</button><button className="primary" disabled={busy}>{busy ? "保存中" : "保存任务"}</button></footer></form></section></div>;
}

function TaskDetailDialog({ detail, permissionMode, busy, close, refresh, dispatch, transition, deleteTask, confirmTransition, edit, move, canMoveUp, canMoveDown, request, fail }: { detail: TaskDetail; permissionMode?: "approval_required" | "full_control"; busy: string; close: () => void; refresh: () => Promise<void>; dispatch: () => Promise<void>; transition: (action: "cancel" | "reopen" | "stop") => Promise<void>; deleteTask: () => void; confirmTransition: (action: "cancel" | "reopen") => void; edit: () => void; move: (taskID: string, direction: "up" | "down") => Promise<void>; canMoveUp: boolean; canMoveDown: boolean; request: Request; fail: (message: string) => void }) {
  const [reviewAction, setReviewAction] = useState<ReviewAction | null>(null);
  const [note, setNote] = useState("");
  const [reviewSubmitting, setReviewSubmitting] = useState(false);
  const reviewSheetRef = useRef<HTMLDivElement>(null);
  const mountedRef = useRef(true);
  useEffect(() => {
    mountedRef.current = true;
    return () => { mountedRef.current = false; };
  }, []);
  useEffect(() => { if (reviewAction && reviewSheetRef.current) reviewSheetRef.current.scrollIntoView({ behavior: "smooth", block: "nearest" }); }, [reviewAction]);
  const submitReview = async (action: ReviewAction) => {
    if (reviewSubmitting) return;
    if (action === "request_changes" && !note.trim()) { fail("请填写需要修改的原因"); return; }
    if (!mountedRef.current) return;
    setReviewSubmitting(true);
    try { await request(`/api/tasks/${detail.id}/review`, { method: "POST", body: JSON.stringify({ action, note: action === "accept" ? "" : note }) }); if (mountedRef.current) { setReviewAction(null); setNote(""); } await refresh(); }
    catch (cause) { if (mountedRef.current) fail(cause instanceof Error ? cause.message : "无法提交验收"); }
    finally { if (mountedRef.current) setReviewSubmitting(false); }
  };
  const reviewBusy = Boolean(busy) || reviewSubmitting;
  return <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="task-detail-title"><section className="modal task-dialog task-detail-dialog"><header><div><label>PROJECT TASK</label><h2 id="task-detail-title">{taskDisplayTitle(detail)}</h2><StatusBadge task={detail} /></div><button title="关闭" disabled={reviewBusy} onClick={close}>x</button></header><div className="task-detail-body"><div className="task-detail-meta"><span className={`priority-${detail.priority}`}>{priorityLabels[detail.priority]}优先级</span><span>更新于 {formatDate(detail.updatedAt)}</span></div><section><h3>任务说明</h3><p>{detail.description}</p></section>{detail.acceptanceCriteria && <section><h3>验收条件</h3><p>{detail.acceptanceCriteria}</p></section>}<section><h3>下发内容</h3><pre className="task-prompt">{detail.promptPreview}</pre><p>执行权限：{permissionMode === "full_control" ? "完全控制" : "默认权限"}</p>{detail.blockReason && <p className="task-dispatch-reason">{detail.blockReason}</p>}</section><section><h3>执行记录</h3>{detail.runs.length === 0 ? <p>尚未下发。</p> : <div className="task-run-list"><div className="task-run-list-head"><span>次数</span><span>状态</span><span>时间</span></div>{detail.runs.map((run) => <div key={run.id}><b>第 {run.sequence} 次</b><span>{run.status}</span><time>{formatDate(run.createdAt)}</time>{run.failureReason && <small>{run.failureReason}</small>}</div>)}</div>}</section>{reviewAction && <div className="task-review-sheet" ref={reviewSheetRef}><h3>要求修改</h3><textarea autoFocus required disabled={reviewSubmitting} value={note} onChange={(event) => setNote(event.target.value)} placeholder="说明需要补充或修改的内容" /><div><button className="secondary" disabled={reviewSubmitting} onClick={() => setReviewAction(null)}>返回</button><button className="primary" disabled={reviewSubmitting} onClick={() => void submitReview("request_changes")}>{reviewSubmitting ? "提交中" : "提交要求"}</button></div></div>}</div><footer className="task-detail-actions">{(detail.status === "todo" || detail.status === "action_required") && <><button className="secondary" disabled={Boolean(busy)} onClick={edit}>编辑</button><button className="secondary" disabled={Boolean(busy) || !canMoveUp} onClick={() => void move(detail.id, "up")}>上移</button><button className="secondary" disabled={Boolean(busy) || !canMoveDown} onClick={() => void move(detail.id, "down")}>下移</button></>}{detail.status === "awaiting_review" && <><button className="secondary" disabled={reviewBusy} onClick={() => setReviewAction("request_changes")}>要求修改</button><button className="primary" disabled={reviewBusy} onClick={() => void submitReview("accept")}>{reviewSubmitting ? "提交中" : "确认完成"}</button></>}{detail.status === "running" && <button className="danger-text" disabled={Boolean(busy)} onClick={() => void transition("stop")}>{busy === "stop" ? "停止中" : "停止任务"}</button>}{(detail.status === "todo" || detail.status === "action_required") && <><button className="danger-text" disabled={Boolean(busy)} onClick={() => confirmTransition("cancel")}>取消任务</button><button className="primary" disabled={!detail.canDispatch || Boolean(busy)} onClick={() => void dispatch()}>{busy === "dispatch" ? "下发中" : "下发任务"}</button></>}{(detail.status === "done" || detail.status === "cancelled") && <button className="secondary" disabled={Boolean(busy)} onClick={() => confirmTransition("reopen")}>重新打开</button>}<button className="danger-text" disabled={Boolean(busy)} onClick={() => void deleteTask()}>{busy === "delete" ? "删除中" : "删除任务"}</button></footer></section></div>;
}

function formatDate(value: string): string {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? "" : date.toLocaleString("zh-CN", { month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit" });
}
