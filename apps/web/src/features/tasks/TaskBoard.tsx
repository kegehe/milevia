import { FormEvent, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { isTaskOrchestrating, priorityLabels, Request, Task, TaskDetail, TaskStatus, Priority, taskDisplayStatus, taskDisplayStatusClass, taskDisplayTitle } from "./task-model";
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
const historicalCancelledColumn: { id: string; label: string; statuses: TaskStatus[] } = { id: "cancelled", label: "历史已取消", statuses: ["cancelled"] };

type DragData = { taskID: string; sourceColumnID: string; sourceIndex: number };
type PendingConfirm = { title: string; message: React.ReactNode; danger?: boolean; onConfirm: () => void; onCancel: () => void } | null;
type ExecutionPolicy = "approval_required" | "full_control" | "read_only" | "workspace_write";
type OrchestrationConfig = { projectId: string; enabled: boolean; mainBranch: string; devBranch: string; verificationCommands: string[]; maxFixRounds: number; frozenReason?: string };
type OrchestrationJob = { id: string; taskId: string; position: number; status: string; lastError?: string };
type ReleaseSnapshot = { id: string; devSha: string; branch: string; status: string; createdAt: string; confirmedAt?: string };

const DRAG_CLICK_SUPPRESSION_MS = 400;

function policyLabel(policy?: ExecutionPolicy): string {
  if (policy === "full_control") return "完全控制";
  if (policy === "read_only") return "仅分析";
  if (policy === "workspace_write") return "项目内执行";
  return "默认权限";
}

export function TaskBoard({ projectID, initialTaskID, permissionMode, request, fail, close, onDispatched }: { projectID: string; initialTaskID?: string; permissionMode?: ExecutionPolicy; request: Request; fail: (message: string) => void; close: () => void; onDispatched: (message: { id: string; role: "user" | "assistant"; content: string; createdAt: string }, runID: string) => void }) {
  const [tasks, setTasks] = useState<Task[]>([]);
  const [view, setView] = useState<"board" | "list">("board");
  const [showHistoricalCancelled, setShowHistoricalCancelled] = useState(false);
  const [editor, setEditor] = useState<EditorState>(null);
  const [detail, setDetail] = useState<TaskDetail | null>(null);
  const [busy, setBusy] = useState("");
  const [orchestration, setOrchestration] = useState<OrchestrationConfig | null>(null);
  const [orchestrationJobs, setOrchestrationJobs] = useState<OrchestrationJob[]>([]);
  const [releaseSnapshots, setReleaseSnapshots] = useState<ReleaseSnapshot[]>([]);
  const [orchestrationOpen, setOrchestrationOpen] = useState(false);
  const [pendingConfirm, setPendingConfirm] = useState<PendingConfirm>(null);
  const [batchMode, setBatchMode] = useState(false);
  const [selectedIDs, setSelectedIDs] = useState<Set<string>>(new Set());
  const [batchDeleting, setBatchDeleting] = useState(false);
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
  const loadOrchestration = useCallback(async () => {
    const [config, jobs, releases] = await Promise.all([request<OrchestrationConfig>(`/api/projects/${projectID}/orchestration/config`), request<OrchestrationJob[]>(`/api/projects/${projectID}/orchestration`), request<ReleaseSnapshot[]>(`/api/projects/${projectID}/orchestration/releases`)]);
    if (mountedRef.current) { setOrchestration(config); setOrchestrationJobs(jobs); setReleaseSnapshots(releases); }
  }, [projectID, request]);

  const loadDetail = useCallback(async (taskID: string) => {
    const next = await request<TaskDetail>(`/api/tasks/${taskID}`);
    if (mountedRef.current) setDetail(next);
    return next;
  }, [request]);

  useEffect(() => { void loadTasks().catch((cause) => { if (mountedRef.current) fail(cause instanceof Error ? cause.message : "无法加载任务"); }); }, [fail, loadTasks]);
  useEffect(() => { void loadOrchestration().catch((cause) => { if (mountedRef.current) fail(cause instanceof Error ? cause.message : "无法加载自动编排配置"); }); }, [fail, loadOrchestration]);
  useEffect(() => {
    const interval = window.setInterval(() => { void loadTasks().catch(() => undefined); }, 10_000);
    return () => window.clearInterval(interval);
  }, [loadTasks]);
  useEffect(() => {
    const hasActiveJob = orchestrationJobs.some((job) => ["queued", "preparing", "implementing", "checking"].includes(job.status));
    if (!hasActiveJob) return;
    const interval = window.setInterval(() => { void loadOrchestration().catch(() => undefined); }, 5_000);
    return () => window.clearInterval(interval);
  }, [loadOrchestration, orchestrationJobs]);
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

  const visibleTasks = useMemo(() => showHistoricalCancelled ? tasks : tasks.filter((task) => task.status !== "cancelled"), [showHistoricalCancelled, tasks]);
  useEffect(() => {
    if (!batchMode || selectedIDs.size === 0) return;
    const currentIDs = new Set(tasks.map((t) => t.id));
    setSelectedIDs((prev) => {
      let changed = false;
      const next = new Set<string>();
      prev.forEach((id) => { if (currentIDs.has(id)) next.add(id); else changed = true; });
      return changed ? next : prev;
    });
  }, [batchMode, selectedIDs.size, tasks]);

  const enterBatchMode = () => { setBatchMode(true); setSelectedIDs(new Set()); };
  const exitBatchMode = () => { setBatchMode(false); setSelectedIDs(new Set()); };
  const toggleSelect = (taskID: string) => {
    setSelectedIDs((prev) => {
      const next = new Set(prev);
      if (next.has(taskID)) next.delete(taskID); else next.add(taskID);
      return next;
    });
  };
  const selectAll = () => { setSelectedIDs(new Set(visibleTasks.map((t) => t.id))); };
  const deselectAll = () => { setSelectedIDs(new Set()); };
  const batchDelete = () => {
    if (selectedIDs.size === 0) return;
    const currentTaskIDs = new Set(tasks.map((t) => t.id));
    const validIDs = [...selectedIDs].filter((id) => currentTaskIDs.has(id));
    if (validIDs.length === 0) { setSelectedIDs(new Set()); return; }
    const count = validIDs.length;
    setPendingConfirm({
      title: "批量删除任务",
      message: <>确认删除选中的 <b>{count}</b> 个任务？此操作不可撤销，所有执行记录将被永久删除。</>,
      danger: true,
      onConfirm: () => void (async () => {
        if (!mountedRef.current) return;
        setPendingConfirm(null);
        setBatchDeleting(true);
        let failed = 0;
        try {
          await Promise.all(validIDs.map((id) => request(`/api/tasks/${id}`, { method: "DELETE" }).catch(() => { failed++; })));
          if (!mountedRef.current) return;
          setSelectedIDs(new Set());
          await loadTasks();
          if (failed > 0 && mountedRef.current) fail(`${failed} 个任务删除失败`);
        } catch (cause) {
          if (mountedRef.current) fail(cause instanceof Error ? cause.message : "批量删除失败");
        } finally {
          if (mountedRef.current) setBatchDeleting(false);
        }
      })(),
      onCancel: () => { if (mountedRef.current) setPendingConfirm(null); },
    });
  };

  const openDetail = (taskID: string) => { if (batchMode) { toggleSelect(taskID); return; } void loadDetail(taskID).catch((cause) => { if (mountedRef.current) fail(cause instanceof Error ? cause.message : "无法加载任务详情"); }); };
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

  const enqueue = async () => {
    if (!detail) return;
    setBusy("enqueue");
    try {
      await request(`/api/tasks/${detail.id}/orchestration/enqueue`, { method: "POST", body: "{}" });
      await Promise.all([refresh(detail.id), loadOrchestration()]);
    } catch (cause) { if (mountedRef.current) fail(cause instanceof Error ? cause.message : "无法加入自动队列"); }
    finally { if (mountedRef.current) setBusy(""); }
  };

  const transition = async (action: "reopen" | "stop") => {
    if (!detail) return;
    if (!mountedRef.current) return;
    setBusy(action);
    try {
      await request(`/api/tasks/${detail.id}/${action}`, { method: "POST", body: "{}" });
      if (!mountedRef.current) return;
      await refresh(detail.id);
    }
    catch (cause) {
      if (!mountedRef.current) return;
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
  const confirmTransition = () => {
    setPendingConfirm({
      title: "重新打开",
      message: "确认重新打开该任务？",
      onConfirm: () => { if (mountedRef.current) { setPendingConfirm(null); void transition("reopen"); } },
      onCancel: () => { if (mountedRef.current) setPendingConfirm(null); },
    });
  };

  const moveTask = async (taskID: string, direction: "up" | "down") => {
    const ordered = [...visibleTasks].sort((left, right) => left.position - right.position || new Date(right.createdAt).getTime() - new Date(left.createdAt).getTime() || left.title.localeCompare(right.title, "zh-CN"));
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
    if (task.status === "cancelled") return;
    const sourceDef = columnDefinitions.find((d) => d.statuses.includes(task.status));
    const targetDef = columnDefinitions.find((d) => d.id === targetColumnID);
    if (!targetDef) return;
    let targetStatus: TaskStatus;
    targetStatus = targetDef.statuses[0];
    const sameColumn = sourceDef?.id === targetColumnID;

    if (sameColumn) {
      const columnTasks = visibleTasks.filter((t) => targetDef.statuses.includes(t.status))
        .sort((left, right) => left.position - right.position || new Date(right.createdAt).getTime() - new Date(left.createdAt).getTime() || left.title.localeCompare(right.title, "zh-CN"));
      const currentIndex = columnTasks.findIndex((t) => t.id === taskID);
      if (currentIndex < 0) return;
      const reordered = [...columnTasks];
      const [moved] = reordered.splice(currentIndex, 1);
      const insertionIndex = currentIndex < targetIndex ? targetIndex - 1 : targetIndex;
      if (insertionIndex === currentIndex) return;
      reordered.splice(insertionIndex, 0, moved);
      const prev = reordered[insertionIndex - 1];
      const next = reordered[insertionIndex + 1];
      let newPosition: number;
      if (!prev) newPosition = next ? next.position - 1 : task.position;
      else if (!next) newPosition = prev.position + 1;
      else newPosition = (prev.position + next.position) / 2;
      if (!mountedRef.current) return;
      setBusy("move");
      try {
        await request(`/api/tasks/${task.id}`, { method: "PATCH", body: JSON.stringify({ title: task.title, description: task.description, acceptanceCriteria: task.acceptanceCriteria, priority: task.priority, position: newPosition }) });
        if (!mountedRef.current) return;
        await refresh();
      } catch (cause) { if (mountedRef.current) fail(cause instanceof Error ? cause.message : "无法调整任务顺序"); }
      finally { if (mountedRef.current) setBusy(""); }
    } else {
      if (task.status === targetStatus) return;
      if (!mountedRef.current) return;
      setBusy("move");
      try {
        const columnTasks = visibleTasks.filter((t) => targetDef.statuses.includes(t.status))
          .sort((left, right) => left.position - right.position || new Date(right.createdAt).getTime() - new Date(left.createdAt).getTime() || left.title.localeCompare(right.title, "zh-CN"));
        const prev = columnTasks[targetIndex - 1];
        const next = columnTasks[targetIndex];
        let newPosition: number;
        if (columnTasks.length === 0) newPosition = task.position;
        else if (!prev) newPosition = next ? next.position - 1 : task.position;
        else if (!next) newPosition = prev.position + 1;
        else newPosition = (prev.position + next.position) / 2;
        await request(`/api/tasks/${task.id}`, { method: "PATCH", body: JSON.stringify({ title: task.title, description: task.description, acceptanceCriteria: task.acceptanceCriteria, priority: task.priority, position: newPosition, status: targetStatus }) });
        if (!mountedRef.current) return;
        await refresh();
      } catch (cause) { if (mountedRef.current) fail(cause instanceof Error ? cause.message : "无法移动任务"); }
      finally { if (mountedRef.current) setBusy(""); }
    }
  };

  return <section className="task-workspace" aria-label="项目任务">
    <header className="task-workspace-head">
      <div className="task-workspace-actions">
        {batchMode ? <>
          <div className="task-batch-bar">
            <label className="task-batch-select-all"><input type="checkbox" checked={selectedIDs.size === visibleTasks.length && visibleTasks.length > 0} ref={(el) => { if (el) el.indeterminate = selectedIDs.size > 0 && selectedIDs.size < visibleTasks.length; }} onChange={() => selectedIDs.size === visibleTasks.length ? deselectAll() : selectAll()} />全选</label>
            <span className="task-batch-count">已选 {selectedIDs.size} 项</span>
            <button className="danger" disabled={selectedIDs.size === 0 || batchDeleting} onClick={batchDelete}>{batchDeleting ? "删除中" : `删除 (${selectedIDs.size})`}</button>
            <button className="secondary" disabled={batchDeleting} onClick={exitBatchMode}>取消</button>
          </div>
        </> : <>
          <div className="task-view-switch" aria-label="任务视图"><button className={view === "board" ? "active" : ""} onClick={() => setView("board")}>看板</button><button className={view === "list" ? "active" : ""} onClick={() => setView("list")}>列表</button></div>
          <label className="task-cancelled-toggle"><input type="checkbox" checked={showHistoricalCancelled} onChange={(event) => setShowHistoricalCancelled(event.target.checked)} />显示历史已取消</label>
          <button className="secondary" onClick={() => setOrchestrationOpen(true)}>自动编排</button>
          <button className="secondary" onClick={enterBatchMode}>批量管理</button>
          <button className="primary" onClick={() => setEditor({})}>新建任务</button>
        </>}
      </div>
    </header>
    {visibleTasks.length === 0 ? <div className="task-empty"><h3>还没有任务</h3><p>将可验证的开发事项加入项目，手动下发执行。</p><button className="primary" onClick={() => setEditor({})}>新建任务</button></div> : view === "board" ? <TaskBoardColumns tasks={visibleTasks} showHistoricalCancelled={showHistoricalCancelled} open={openDetail} onDrop={handleDrop} batchMode={batchMode} selectedIDs={selectedIDs} toggleSelect={toggleSelect} /> : <TaskList tasks={visibleTasks} open={openDetail} batchMode={batchMode} selectedIDs={selectedIDs} toggleSelect={toggleSelect} />}
    {editor && <TaskEditor projectID={projectID} task={editor.task} request={request} close={() => setEditor(null)} saved={async (taskID) => { setEditor(null); await refresh(taskID); }} fail={fail} />}
    {detail && <TaskDetailDialog detail={detail} permissionMode={permissionMode} busy={busy} close={() => setDetail(null)} refresh={() => refresh(detail.id)} dispatch={dispatch} enqueue={enqueue} orchestrationEnabled={Boolean(orchestration?.enabled)} transition={transition} deleteTask={deleteTask} confirmTransition={confirmTransition} edit={() => { const task = tasks.find((item) => item.id === detail.id); if (task) { setDetail(null); setEditor({ task }); } }} move={moveTask} canMoveUp={visibleTasks.some((item) => item.position < detail.position)} canMoveDown={visibleTasks.some((item) => item.position > detail.position)} request={request} fail={fail} />}
    {orchestrationOpen && orchestration && <OrchestrationDialog config={orchestration} jobs={orchestrationJobs} releases={releaseSnapshots} request={request} close={() => setOrchestrationOpen(false)} saved={loadOrchestration} fail={fail} />}
    {pendingConfirm && createPortal(<ConfirmDialog title={pendingConfirm.title} message={pendingConfirm.message} danger={pendingConfirm.danger} onConfirm={pendingConfirm.onConfirm} onCancel={pendingConfirm.onCancel} />, document.body)}
  </section>;
}

function TaskBoardColumns({ tasks, showHistoricalCancelled, open, onDrop, batchMode, selectedIDs, toggleSelect }: { tasks: Task[]; showHistoricalCancelled: boolean; open: (taskID: string) => void; onDrop: (taskID: string, columnID: string, index: number) => Promise<void>; batchMode: boolean; selectedIDs: Set<string>; toggleSelect: (taskID: string) => void }) {
  const [showAllDone, setShowAllDone] = useState(false);
  const [dragOverColumn, setDragOverColumn] = useState<string | null>(null);
  const [dragOverIndex, setDragOverIndex] = useState<number | null>(null);
  const [draggingTaskID, setDraggingTaskID] = useState<string | null>(null);
  const columnRefs = useRef<Record<string, HTMLElement | null>>({});
  const dragGestureActive = useRef(false);
  const dragClickSuppressedUntil = useRef(0);
  const definitions = showHistoricalCancelled ? [...columnDefinitions, historicalCancelledColumn] : columnDefinitions;
  const grouped = (definition: typeof columnDefinitions[number]) => tasks.filter((task) => definition.statuses.includes(task.status))
    .sort((left, right) => left.position - right.position || new Date(right.createdAt).getTime() - new Date(left.createdAt).getTime() || left.title.localeCompare(right.title, "zh-CN"));
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
      const def = definitions.find((d) => d.id === columnID);
      if (!def) return false;
      return def.statuses.includes(t.status);
    }).sort((left, right) => left.position - right.position || new Date(right.createdAt).getTime() - new Date(left.createdAt).getTime() || left.title.localeCompare(right.title, "zh-CN"));
    const targetIndex = dragOverIndex !== null ? Math.min(dragOverIndex, columnTasks.length) : columnTasks.length;
    await onDrop(dragData.taskID, columnID, targetIndex);
  };
  const handleTaskDragStart = (e: React.DragEvent, taskID: string, columnID: string, index: number) => {
    dragGestureActive.current = true;
    dragClickSuppressedUntil.current = Date.now() + DRAG_CLICK_SUPPRESSION_MS;
    setDraggingTaskID(taskID);
    const data: DragData = { taskID, sourceColumnID: columnID, sourceIndex: index };
    e.dataTransfer.setData("application/x-task-drag", JSON.stringify(data));
    e.dataTransfer.effectAllowed = "move";
  };
  const handleTaskDragEnd = () => {
    dragGestureActive.current = false;
    dragClickSuppressedUntil.current = Date.now() + DRAG_CLICK_SUPPRESSION_MS;
    setDraggingTaskID(null);
    setDragOverColumn(null);
    setDragOverIndex(null);
  };
  const handleTaskDragOver = (e: React.DragEvent, columnID: string, index: number) => {
    e.preventDefault();
    e.stopPropagation();
    e.dataTransfer.dropEffect = "move";
    const rect = e.currentTarget.getBoundingClientRect();
    const targetIndex = e.clientY < rect.top + rect.height / 2 ? index : index + 1;
    setDragOverColumn(columnID);
    setDragOverIndex(targetIndex);
  };
  const handleBoardClickCapture = (e: React.MouseEvent) => {
    if (!dragGestureActive.current && Date.now() >= dragClickSuppressedUntil.current) return;
    e.preventDefault();
    e.stopPropagation();
  };
  const isDraggable = (task: Task, _columnID: string): boolean => {
    if (task.status === "running" || task.status === "done" || task.status === "cancelled") return false;
    return true;
  };

  return <div className={`task-board-columns columns-${definitions.length}`} onClickCapture={handleBoardClickCapture}>{definitions.map((definition) => {
    const items = grouped(definition);
    const isDone = definition.id === "done";
    const visible = isDone && !showAllDone && !batchMode ? items.slice(0, DONE_LIMIT) : items;
    const hidden = isDone && !batchMode ? Math.max(0, items.length - DONE_LIMIT) : 0;
    const isDragOver = dragOverColumn === definition.id;
    const acceptsDrops = definition.id !== "cancelled";
    return <section
      className={`task-board-column column-${definition.id}${isDragOver ? " drag-over" : ""}`}
      key={definition.id}
      ref={(el) => { columnRefs.current[definition.id] = el; }}
      onDragOver={acceptsDrops ? (e) => handleColumnDragOver(e, definition.id) : undefined}
      onDragLeave={acceptsDrops ? (e) => handleColumnDragLeave(e, definition.id) : undefined}
      onDrop={acceptsDrops ? (e) => handleColumnDrop(e, definition.id) : undefined}
    >
      <header><h3>{definition.label}</h3><b>{items.length}</b></header>
      <div>
        {visible.map((task, index) => (
          <TaskItem key={task.id} task={task} open={open} columnID={definition.id} index={index} isDragging={draggingTaskID === task.id} dropTarget={dragOverColumn === definition.id && dragOverIndex === index} draggable={!batchMode && isDraggable(task, definition.id)} onDragStart={handleTaskDragStart} onDragEnd={handleTaskDragEnd} onDragOver={handleTaskDragOver} batchMode={batchMode} selected={selectedIDs.has(task.id)} toggleSelect={toggleSelect} />
        ))}
        {isDone && !showAllDone && hidden > 0 && <button className="task-show-more" onClick={() => setShowAllDone(true)}>显示更多（+{hidden}）</button>}
        {acceptsDrops && <div className={`task-drop-indicator${dragOverColumn === definition.id && dragOverIndex === visible.length ? " active" : ""}`} onDragOver={(e) => handleTaskDragOver(e, definition.id, visible.length)} onDrop={(e) => handleColumnDrop(e, definition.id)} />}
      </div>
    </section>;
  })}</div>;
}

function TaskList({ tasks, open, batchMode, selectedIDs, toggleSelect }: { tasks: Task[]; open: (taskID: string) => void; batchMode: boolean; selectedIDs: Set<string>; toggleSelect: (taskID: string) => void }) {
  const sorted = [...tasks].sort((left, right) => left.position - right.position || new Date(right.createdAt).getTime() - new Date(left.createdAt).getTime() || left.title.localeCompare(right.title, "zh-CN"));
  return <div className={`task-list${batchMode ? " batch-mode" : ""}`}><div className="task-list-head">{batchMode && <span className="task-list-check-col"></span>}<span>任务</span><span>状态</span><span>优先级</span><span>最近更新</span></div>{sorted.map((task) => { const title = taskDisplayTitle(task); const description = task.description.trim(); return <button key={task.id} className={`task-list-row${batchMode && selectedIDs.has(task.id) ? " batch-selected" : ""}`} onClick={() => open(task.id)}>{batchMode && <span className="task-list-check-col"><span className={`task-batch-check${selectedIDs.has(task.id) ? " checked" : ""}`}></span></span>}<span><b>{title}</b>{description && description !== title.trim() && <small>{description}</small>}</span><StatusBadge task={task} /><em className={`priority-${task.priority}`}>{priorityLabels[task.priority]}</em><time>{formatDate(task.updatedAt)}</time></button>; })}</div>;
}

function TaskItem({ task, open, columnID, index, isDragging, dropTarget, draggable, onDragStart, onDragEnd, onDragOver, batchMode, selected, toggleSelect }: { task: Task; open: (taskID: string) => void; columnID: string; index: number; isDragging: boolean; dropTarget: boolean; draggable: boolean; onDragStart: (e: React.DragEvent, taskID: string, columnID: string, index: number) => void; onDragEnd: () => void; onDragOver: (e: React.DragEvent, columnID: string, index: number) => void; batchMode: boolean; selected: boolean; toggleSelect: (taskID: string) => void }) {
  const dragged = useRef(false);
  const dragAttempted = useRef(false);
  const mouseDownRef = useRef<{ x: number; y: number } | null>(null);
  const suppressClickUntil = useRef(0);
  const title = taskDisplayTitle(task);
  const description = task.description.trim();
  const handleMouseDown = (e: React.MouseEvent) => {
    mouseDownRef.current = { x: e.clientX, y: e.clientY };
  };
  const handleMouseMove = (e: React.MouseEvent) => {
    const down = mouseDownRef.current;
    if (down) {
      const dx = e.clientX - down.x;
      const dy = e.clientY - down.y;
      if (Math.sqrt(dx * dx + dy * dy) > 4) dragAttempted.current = true;
    }
  };
  return <button
    className={`task-item priority-${task.priority}${isDragging ? " dragging" : ""}${dropTarget ? " drop-target" : ""}${!draggable ? " not-draggable" : ""}${batchMode && selected ? " batch-selected" : ""}`}
    draggable={draggable}
    onMouseDown={handleMouseDown}
    onMouseMove={handleMouseMove}
    onMouseUp={() => {
      mouseDownRef.current = null;
      if (!dragAttempted.current) return;
      suppressClickUntil.current = Date.now() + DRAG_CLICK_SUPPRESSION_MS;
      dragAttempted.current = false;
    }}
    onClick={(e) => {
      if (dragged.current || dragAttempted.current || Date.now() < suppressClickUntil.current) { e.preventDefault(); e.stopPropagation(); return; }
      if (e.detail === 0) { open(task.id); return; }
      open(task.id);
    }}
    onDragStart={(e) => { if (!draggable) { e.preventDefault(); return; } dragged.current = true; dragAttempted.current = true; suppressClickUntil.current = Date.now() + DRAG_CLICK_SUPPRESSION_MS; onDragStart(e, task.id, columnID, index); }}
    onDragEnd={() => { dragged.current = false; dragAttempted.current = false; mouseDownRef.current = null; suppressClickUntil.current = Date.now() + DRAG_CLICK_SUPPRESSION_MS; onDragEnd(); }}
    onDragOver={(e) => { if (draggable) onDragOver(e, columnID, index); }}
  >
    {batchMode && <span className={`task-batch-check${selected ? " checked" : ""}`} onClick={(e) => { e.stopPropagation(); toggleSelect(task.id); }}></span>}
    <div className="task-item-top"><StatusBadge task={task} /><span>{priorityLabels[task.priority]}</span></div>
    <b>{title}</b>
    {description && description !== title.trim() && <p>{description}</p>}
  </button>;
}

function StatusBadge({ task }: { task: Task }) {
  return <span className={`task-status ${taskDisplayStatusClass(task)}`}>{taskDisplayStatus(task)}</span>;
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

function OrchestrationDialog({ config, jobs, releases, request, close, saved, fail }: { config: OrchestrationConfig; jobs: OrchestrationJob[]; releases: ReleaseSnapshot[]; request: Request; close: () => void; saved: () => Promise<void>; fail: (message: string) => void }) {
  const [enabled, setEnabled] = useState(config.enabled);
  const [mainBranch, setMainBranch] = useState(config.mainBranch);
  const [devBranch, setDevBranch] = useState(config.devBranch);
  const [commands, setCommands] = useState(config.verificationCommands.join("\n"));
  const [maxFixRounds, setMaxFixRounds] = useState(config.maxFixRounds);
  const [saving, setSaving] = useState(false);
  const [action, setAction] = useState("");
  const save = async (event: FormEvent) => {
    event.preventDefault(); setSaving(true);
    try { await request(`/api/projects/${config.projectId}/orchestration/config`, { method: "PUT", body: JSON.stringify({ enabled, mainBranch, devBranch, verificationCommands: commands.split("\n").map((item) => item.trim()).filter(Boolean), maxFixRounds }) }); await saved(); close(); }
    catch (cause) { fail(cause instanceof Error ? cause.message : "无法保存自动编排配置"); }
    finally { setSaving(false); }
  };
  const runAction = async (key: string, path: string) => {
    setAction(key);
    try { await request(path, { method: "POST", body: "{}" }); await saved(); }
    catch (cause) { fail(cause instanceof Error ? cause.message : "自动编排操作失败"); }
    finally { setAction(""); }
  };
  return <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="orchestration-title"><section className="modal task-dialog"><header><div><label>AUTOMATION</label><h2 id="orchestration-title">严格串行编排</h2></div><button title="关闭" disabled={saving || Boolean(action)} onClick={close}>x</button></header><form onSubmit={(event) => void save(event)}><div className="task-form"><label><input type="checkbox" checked={enabled} onChange={(event) => setEnabled(event.target.checked)} />启用自动队列</label><label>稳定分支<input required value={mainBranch} onChange={(event) => setMainBranch(event.target.value)} /></label><label>集成分支<input required value={devBranch} onChange={(event) => setDevBranch(event.target.value)} /></label><label>验证命令<textarea required value={commands} onChange={(event) => setCommands(event.target.value)} placeholder="每行一条命令" /></label><label>最大修复轮次<input type="number" min={1} max={10} value={maxFixRounds} onChange={(event) => setMaxFixRounds(Number(event.target.value))} /></label>{config.frozenReason && <p className="task-dispatch-reason">队列已冻结：{config.frozenReason}</p>}<section><h3>自动队列</h3>{jobs.length === 0 ? <p>暂无自动任务。</p> : <div className="task-run-list">{jobs.map((job) => <div key={job.id}><b>#{job.position}</b><span>{job.status}</span>{job.lastError && <small>{job.lastError}</small>}{job.status === "queued" && <button type="button" className="secondary" disabled={Boolean(action)} onClick={() => void runAction(`pause:${job.id}`, `/api/tasks/${job.taskId}/orchestration/pause`)}>暂停</button>}{(job.status === "paused" || job.status === "needs_human") && <button type="button" className="primary" disabled={Boolean(action)} onClick={() => void runAction(`resume:${job.id}`, `/api/tasks/${job.taskId}/orchestration/resume`)}>{action === `resume:${job.id}` ? "恢复中" : "恢复"}</button>}</div>)}</div>}</section><section><h3>验收快照</h3><button type="button" className="secondary" disabled={Boolean(action) || !config.enabled || Boolean(config.frozenReason)} onClick={() => void runAction("release", `/api/projects/${config.projectId}/orchestration/release`)}>{action === "release" ? "创建中" : "创建当前 dev 验收快照"}</button>{releases.length === 0 ? <p>尚未创建验收快照。</p> : <div className="task-run-list">{releases.map((release) => <div key={release.id}><b>{release.branch}</b><span>{release.status}</span><small>{release.devSha}</small>{release.status === "awaiting_main" && <button type="button" className="primary" disabled={Boolean(action)} onClick={() => void runAction(`confirm:${release.id}`, `/api/projects/${config.projectId}/orchestration/releases/${release.id}/confirm-main`)}>{action === `confirm:${release.id}` ? "确认中" : "确认已合并 main"}</button>}</div>)}</div>}</section></div><footer><button type="button" className="secondary" disabled={saving || Boolean(action)} onClick={close}>取消</button><button className="primary" disabled={saving || Boolean(action)}>{saving ? "保存中" : "保存配置"}</button></footer></form></section></div>;
}

function TaskDetailDialog({ detail, permissionMode, busy, close, refresh, dispatch, enqueue, orchestrationEnabled, transition, deleteTask, confirmTransition, edit, move, canMoveUp, canMoveDown, request, fail }: { detail: TaskDetail; permissionMode?: ExecutionPolicy; busy: string; close: () => void; refresh: () => Promise<void>; dispatch: () => Promise<void>; enqueue: () => Promise<void>; orchestrationEnabled: boolean; transition: (action: "reopen" | "stop") => Promise<void>; deleteTask: () => void; confirmTransition: () => void; edit: () => void; move: (taskID: string, direction: "up" | "down") => Promise<void>; canMoveUp: boolean; canMoveDown: boolean; request: Request; fail: (message: string) => void }) {
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
  const reviewBusy = Boolean(busy) || reviewSubmitting || isTaskOrchestrating(detail);
  return <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="task-detail-title"><section className="modal task-dialog task-detail-dialog"><header><div><label>PROJECT TASK</label><h2 id="task-detail-title">{taskDisplayTitle(detail)}</h2><StatusBadge task={detail} /></div><button title="关闭" disabled={reviewBusy} onClick={close}>x</button></header><div className="task-detail-body"><div className="task-detail-meta"><span className={`priority-${detail.priority}`}>{priorityLabels[detail.priority]}优先级</span><span>更新于 {formatDate(detail.updatedAt)}</span></div><section><h3>任务说明</h3><p>{detail.description}</p></section>{detail.acceptanceCriteria && <section><h3>验收条件</h3><p>{detail.acceptanceCriteria}</p></section>}<section><h3>下发内容</h3><pre className="task-prompt">{detail.promptPreview}</pre><p>执行权限：{policyLabel(permissionMode)}</p>{detail.blockReason && <p className="task-dispatch-reason">{detail.blockReason}</p>}</section><section><h3>执行记录</h3>{detail.runs.length === 0 ? <p>尚未下发。</p> : <div className="task-run-list"><div className="task-run-list-head"><span>次数</span><span>状态</span><span>时间</span></div>{detail.runs.map((run) => <div key={run.id}><b>第 {run.sequence} 次</b><span>{run.status}</span><time>{formatDate(run.createdAt)}</time>{run.failureReason && <small>{run.failureReason}</small>}</div>)}</div>}</section>{reviewAction && <div className="task-review-sheet" ref={reviewSheetRef}><h3>要求修改</h3><textarea autoFocus required disabled={reviewSubmitting} value={note} onChange={(event) => setNote(event.target.value)} placeholder="说明需要补充或修改的内容" /><div><button className="secondary" disabled={reviewSubmitting} onClick={() => setReviewAction(null)}>返回</button><button className="primary" disabled={reviewSubmitting} onClick={() => void submitReview("request_changes")}>{reviewSubmitting ? "提交中" : "提交要求"}</button></div></div>}</div><footer className="task-detail-actions">{(detail.status === "todo" || detail.status === "action_required") && <><button className="secondary" disabled={Boolean(busy)} onClick={edit}>编辑</button><button className="secondary" disabled={Boolean(busy) || !canMoveUp} onClick={() => void move(detail.id, "up")}>上移</button><button className="secondary" disabled={Boolean(busy) || !canMoveDown} onClick={() => void move(detail.id, "down")}>下移</button></>}{detail.status === "awaiting_review" && <><button className="secondary" disabled={reviewBusy} onClick={() => setReviewAction("request_changes")}>要求修改</button><button className="primary" disabled={reviewBusy} onClick={() => void submitReview("accept")}>{reviewSubmitting ? "提交中" : "提交要求"}</button></>}{detail.status === "running" && <button className="danger-text" disabled={Boolean(busy)} onClick={() => void transition("stop")}>{busy === "stop" ? "停止中" : "停止任务"}</button>}{(detail.status === "todo" || detail.status === "action_required") && <>{orchestrationEnabled && <button className="secondary" disabled={Boolean(busy)} onClick={() => void enqueue()}>{busy === "enqueue" ? "入队中" : "加入自动队列"}</button>}<button className="primary" disabled={!detail.canDispatch || Boolean(busy)} onClick={() => void dispatch()}>{busy === "dispatch" ? "下发中" : "下发任务"}</button></>}{detail.status === "done" && <button className="secondary" disabled={Boolean(busy)} onClick={confirmTransition}>重新打开</button>}<button className="danger-text" disabled={Boolean(busy)} onClick={() => void deleteTask()}>{busy === "delete" ? "删除中" : "删除任务"}</button></footer></section></div>;
}

function formatDate(value: string): string {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? "" : date.toLocaleString("zh-CN", { month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit" });
}
