import { FormEvent, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { useNavigate } from "react-router-dom";
import { isTaskAwaitingMainMerge, isTaskOrchestrating, priorityLabels, Request, Task, TaskDetail, TaskStatus, Priority, VerificationRun, taskDisplayStatus, taskDisplayStatusClass, taskDisplayTitle } from "./task-model";
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
type OrchestrationConfig = { projectId: string; enabled: boolean; mainBranch: string; agentId: "claude-code" | "codex"; verificationCommands: string[]; maxFixRounds: number; frozenReason?: string };

const DRAG_CLICK_SUPPRESSION_MS = 400;
// 看板每列初始渲染数量；滚动到底部后再追加一批，避免一次性渲染过长的列。
const COLUMN_PAGE_SIZE = 8;

function policyLabel(policy?: ExecutionPolicy): string {
  if (policy === "full_control") return "完全控制";
  if (policy === "read_only") return "仅分析";
  if (policy === "workspace_write") return "项目内执行";
  return "默认权限";
}


function verificationPhaseLabel(phase: VerificationRun["phase"]): string {
  if (phase === "task") return "任务验证";
  if (phase === "review") return "独立审查";
  return phase;
}

function TaskToolbarIcon({ name }: { name: "board" | "list" | "workflow" | "batch" | "plus" }) {
  const paths = {
    board: <><rect x="3" y="3" width="5" height="5" rx="1" /><rect x="12" y="3" width="5" height="5" rx="1" /><rect x="3" y="12" width="5" height="5" rx="1" /><rect x="12" y="12" width="5" height="5" rx="1" /></>,
    list: <><path d="M8 5h9M8 10h9M8 15h9" /><path d="M4 5h.01M4 10h.01M4 15h.01" /></>,
    workflow: <><circle cx="5" cy="5" r="2" /><circle cx="15" cy="6" r="2" /><circle cx="10" cy="15" r="2" /><path d="m6.8 6.1 6.4-.8M6.1 6.8l2.8 6.4M13.8 7.7l-2.6 5.5" /></>,
    batch: <><rect x="3" y="3" width="5" height="5" rx="1" /><rect x="3" y="12" width="5" height="5" rx="1" /><path d="M11 5.5h6M11 14.5h6" /></>,
    plus: <><path d="M10 4v12M4 10h12" /></>,
  };
  return <svg className="task-toolbar-icon" viewBox="0 0 20 20" aria-hidden="true" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round">{paths[name]}</svg>;
}

export function TaskBoard({ projectID, initialTaskID, permissionMode, request, fail, close, onDispatched }: { projectID: string; initialTaskID?: string; permissionMode?: ExecutionPolicy; request: Request; fail: (message: string) => void; close: () => void; onDispatched: (message: { id: string; role: "user" | "assistant"; content: string; createdAt: string }, runID: string) => void }) {
  const navigate = useNavigate();
  const [tasks, setTasks] = useState<Task[]>([]);
  const [view, setView] = useState<"board" | "list">("board");
  const [showHistoricalCancelled, setShowHistoricalCancelled] = useState(false);
  const [editor, setEditor] = useState<EditorState>(null);
  const [detail, setDetail] = useState<TaskDetail | null>(null);
  const [busy, setBusy] = useState("");
  const [orchestration, setOrchestration] = useState<OrchestrationConfig | null>(null);
  const [pendingConfirm, setPendingConfirm] = useState<PendingConfirm>(null);
  const [batchMode, setBatchMode] = useState(false);
  const [selectedIDs, setSelectedIDs] = useState<Set<string>>(new Set());
  const [batchDeleting, setBatchDeleting] = useState(false);
  const mountedRef = useRef(true);
  const tasksRequestVersion = useRef(0);
  const detailRequestVersion = useRef(0);
  const orchestrationRequestVersion = useRef(0);

  useEffect(() => {
    mountedRef.current = true;
    return () => { mountedRef.current = false; };
  }, []);

  const loadTasks = useCallback(async () => {
    const requestVersion = ++tasksRequestVersion.current;
    const next = await request<Task[]>(`/api/projects/${projectID}/tasks`);
    if (mountedRef.current && requestVersion === tasksRequestVersion.current) setTasks(next);
    return next;
  }, [projectID, request]);
  const loadOrchestration = useCallback(async () => {
    const requestVersion = ++orchestrationRequestVersion.current;
    const config = await request<OrchestrationConfig>(`/api/projects/${projectID}/orchestration/config`);
    if (mountedRef.current && requestVersion === orchestrationRequestVersion.current) setOrchestration(config);
  }, [projectID, request]);

  const loadDetail = useCallback(async (taskID: string) => {
    const requestVersion = ++detailRequestVersion.current;
    const next = await request<TaskDetail>(`/api/tasks/${taskID}`);
    if (mountedRef.current && requestVersion === detailRequestVersion.current) setDetail(next);
    return next;
  }, [request]);

  useEffect(() => { void loadTasks().catch((cause) => { if (mountedRef.current) fail(cause instanceof Error ? cause.message : "无法加载任务"); }); }, [fail, loadTasks]);
  useEffect(() => { void loadOrchestration().catch((cause) => { if (mountedRef.current) fail(cause instanceof Error ? cause.message : "无法加载自动编排配置"); }); }, [fail, loadOrchestration]);
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
      const action = detail.orchestrationStatus === "stopped" ? "resume" : "enqueue";
      await request(`/api/tasks/${detail.id}/orchestration/${action}`, { method: "POST", body: "{}" });
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
      await request(`/api/tasks/${task.id}`, { method: "PATCH", body: JSON.stringify({ title: task.title, description: task.description, priority: task.priority, position }) });
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
        await request(`/api/tasks/${task.id}`, { method: "PATCH", body: JSON.stringify({ title: task.title, description: task.description, priority: task.priority, position: newPosition }) });
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
        await request(`/api/tasks/${task.id}`, { method: "PATCH", body: JSON.stringify({ title: task.title, description: task.description, priority: task.priority, position: newPosition, status: targetStatus }) });
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
          <div className="task-toolbar-group task-view-tools">
            <div className="task-view-switch" aria-label="任务视图">
              <button className={view === "board" ? "active" : ""} aria-pressed={view === "board"} onClick={() => setView("board")}><TaskToolbarIcon name="board" />看板</button>
              <button className={view === "list" ? "active" : ""} aria-pressed={view === "list"} onClick={() => setView("list")}><TaskToolbarIcon name="list" />列表</button>
            </div>
            <label className="task-cancelled-toggle">
              <input type="checkbox" checked={showHistoricalCancelled} onChange={(event) => setShowHistoricalCancelled(event.target.checked)} />
              <span className="task-cancelled-toggle-control" aria-hidden="true"><i /></span>
              <span>显示历史已取消</span>
            </label>
          </div>
          <div className="task-toolbar-group task-toolbar-commands">
            <button className="task-toolbar-action" onClick={() => navigate(`/projects/${projectID}/orchestration`)}><TaskToolbarIcon name="workflow" />自动编排</button>
            <button className="task-toolbar-action" onClick={enterBatchMode}><TaskToolbarIcon name="batch" />批量管理</button>
            <button className="task-toolbar-action task-toolbar-create" onClick={() => setEditor({})}><TaskToolbarIcon name="plus" />新建任务</button>
          </div>
        </>}
      </div>
    </header>
    {visibleTasks.length === 0 ? <div className="task-empty"><h3>还没有任务</h3><p>将可验证的开发事项加入项目，手动下发执行。</p><button className="primary" onClick={() => setEditor({})}>新建任务</button></div> : view === "board" ? <TaskBoardColumns tasks={visibleTasks} showHistoricalCancelled={showHistoricalCancelled} open={openDetail} onDrop={handleDrop} batchMode={batchMode} selectedIDs={selectedIDs} toggleSelect={toggleSelect} /> : <TaskList tasks={visibleTasks} open={openDetail} batchMode={batchMode} selectedIDs={selectedIDs} toggleSelect={toggleSelect} />}
    {editor && <TaskEditor projectID={projectID} task={editor.task} request={request} close={() => setEditor(null)} saved={async (taskID) => { setEditor(null); await refresh(taskID); }} fail={fail} />}
    {detail && <TaskDetailDialog detail={detail} permissionMode={permissionMode} busy={busy} close={() => setDetail(null)} refresh={() => refresh(detail.id)} dispatch={dispatch} enqueue={enqueue} orchestrationEnabled={Boolean(orchestration?.enabled)} transition={transition} deleteTask={deleteTask} confirmTransition={confirmTransition} edit={() => { const task = tasks.find((item) => item.id === detail.id); if (task) { setDetail(null); setEditor({ task }); } }} move={moveTask} canMoveUp={visibleTasks.some((item) => item.position < detail.position)} canMoveDown={visibleTasks.some((item) => item.position > detail.position)} request={request} fail={fail} />}
    {pendingConfirm && createPortal(<ConfirmDialog title={pendingConfirm.title} message={pendingConfirm.message} danger={pendingConfirm.danger} onConfirm={pendingConfirm.onConfirm} onCancel={pendingConfirm.onCancel} />, document.body)}
  </section>;
}

function TaskBoardColumns({ tasks, showHistoricalCancelled, open, onDrop, batchMode, selectedIDs, toggleSelect }: { tasks: Task[]; showHistoricalCancelled: boolean; open: (taskID: string) => void; onDrop: (taskID: string, columnID: string, index: number) => Promise<void>; batchMode: boolean; selectedIDs: Set<string>; toggleSelect: (taskID: string) => void }) {
  const [dragOverColumn, setDragOverColumn] = useState<string | null>(null);
  const [dragOverIndex, setDragOverIndex] = useState<number | null>(null);
  const [draggingTaskID, setDraggingTaskID] = useState<string | null>(null);
  // 每列已渲染的任务数量，滚到底部时增量加载更多，避免一次性渲染过长的列。
  // 批量模式下展开全部，便于勾选与全选。
  const [visibleCounts, setVisibleCounts] = useState<Record<string, number>>({});
  const columnRefs = useRef<Record<string, HTMLElement | null>>({});
  const sentinelRefs = useRef<Record<string, HTMLElement | null>>({});
  const observerRef = useRef<IntersectionObserver | null>(null);
  const dragGestureActive = useRef(false);
  const dragClickSuppressedUntil = useRef(0);
  // WebView2 上 dataTransfer 的自定义 MIME 数据往返不可靠（getData 常读回空）。
  // 拖拽目标数据改用内存 ref 传递——仅保留 dataTransfer 触发拖拽，不再承担传参。
  const dragPayloadRef = useRef<DragData | null>(null);
  // 用 useMemo 稳定 definitions 引用：否则勾选“显示历史已取消”时每次渲染都会生成新数组，
  // 导致下方的 IntersectionObserver effect 反复重建，新 observer 对仍在视口内的哨兵立即触发
  // 回调，形成连锁加载直到哨兵卸载——无限滚动会退化为一次性全量加载。
  const definitions = useMemo(() => showHistoricalCancelled ? [...columnDefinitions, historicalCancelledColumn] : columnDefinitions, [showHistoricalCancelled]);
  const grouped = (definition: typeof columnDefinitions[number]) => tasks.filter((task) => definition.statuses.includes(task.status))
    .sort((left, right) => left.position - right.position || new Date(right.createdAt).getTime() - new Date(left.createdAt).getTime() || left.title.localeCompare(right.title, "zh-CN"));

  // IntersectionObserver 观察各列底部的哨兵元素，进入视口即扩大该列可见数量。
  useEffect(() => {
    const observer = new IntersectionObserver((entries) => {
      for (const entry of entries) {
        if (!entry.isIntersecting) continue;
        const columnID = (entry.target as HTMLElement).dataset.column;
        if (!columnID) continue;
        setVisibleCounts((prev) => ({ ...prev, [columnID]: (prev[columnID] ?? COLUMN_PAGE_SIZE) + COLUMN_PAGE_SIZE }));
      }
    }, { root: null, rootMargin: "200px", threshold: 0 });
    observerRef.current = observer;
    for (const id of Object.keys(sentinelRefs.current)) {
      const el = sentinelRefs.current[id];
      if (el) observer.observe(el);
    }
    return () => { observer.disconnect(); observerRef.current = null; };
  }, [definitions]);

  const registerSentinel = (columnID: string, el: HTMLElement | null) => {
    sentinelRefs.current[columnID] = el;
    if (el && observerRef.current) observerRef.current.observe(el);
  };

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
    // 优先读内存 payload（WebView2 可靠通道）；仅在缺失时回退 dataTransfer（浏览器通道）。
    let dragData: DragData | null = dragPayloadRef.current;
    if (!dragData) {
      const data = e.dataTransfer.getData("application/x-task-drag");
      if (!data) return;
      dragData = JSON.parse(data) as DragData;
    }
    dragPayloadRef.current = null;
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
    dragPayloadRef.current = data;
    e.dataTransfer.setData("application/x-task-drag", JSON.stringify(data));
    e.dataTransfer.effectAllowed = "move";
  };
  const handleTaskDragEnd = () => {
    dragGestureActive.current = false;
    dragClickSuppressedUntil.current = Date.now() + DRAG_CLICK_SUPPRESSION_MS;
    dragPayloadRef.current = null;
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
    // awaiting_review 与 running/done/cancelled 一样处于"工作流锁定"态：跨列拖拽
    // 没有合法目标（后端 canUpdateTaskStatus 只放行 todo↔action_required），
    // 拖着只会 409，改为走验收/重新下发按钮。
    if (task.status === "running" || task.status === "done" || task.status === "cancelled" || task.status === "awaiting_review") return false;
    return true;
  };

  return <div className={`task-board-columns columns-${definitions.length}`} onClickCapture={handleBoardClickCapture}>{definitions.map((definition) => {
    const items = grouped(definition);
    const limit = batchMode ? items.length : (visibleCounts[definition.id] ?? COLUMN_PAGE_SIZE);
    const visible = items.slice(0, limit);
    const hidden = Math.max(0, items.length - visible.length);
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
        {hidden > 0 && <div className="task-show-more" ref={(el) => registerSentinel(definition.id, el)} data-column={definition.id}>还有 {hidden} 项，滚动加载更多…</div>}
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
    <div className="task-item-top"><StatusBadge task={task} /><span className={`task-item-priority priority-${task.priority}`} title={`${priorityLabels[task.priority]}优先级`} aria-label={`${priorityLabels[task.priority]}优先级`} /></div>
    <b>{title}</b>
    {description && description !== title.trim() && <p>{description}</p>}
    <small className="task-item-updated">更新于 {formatDate(task.updatedAt)}</small>
  </button>;
}

function StatusBadge({ task }: { task: Task }) {
  return <span className={`task-status ${taskDisplayStatusClass(task)}`}>{taskDisplayStatus(task)}</span>;
}

function TaskEditor({ projectID, task, request, close, saved, fail }: { projectID: string; task?: Task; tasks?: Task[]; request: Request; close: () => void; saved: (taskID: string) => Promise<void>; fail: (message: string) => void }) {
  const [title, setTitle] = useState(task?.title || "");
  const [description, setDescription] = useState(task?.description || "");
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
      const payload = { title, description, priority, position: task?.position || 0 };
      const result = await request<Task>(task ? `/api/tasks/${task.id}` : `/api/projects/${projectID}/tasks`, { method: task ? "PATCH" : "POST", body: JSON.stringify(payload) });
      if (!mountedRef.current) return;
      await saved(result.id);
    } catch (cause) { if (mountedRef.current) fail(cause instanceof Error ? cause.message : "无法保存任务"); }
    finally { if (mountedRef.current) setBusy(false); }
  };
  return <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="task-editor-title"><section className="modal task-dialog task-editor-dialog"><header className="task-editor-header"><div><span>任务</span><h2 id="task-editor-title">{task ? "编辑任务" : "新建任务"}</h2></div><button type="button" className="task-editor-close" title="关闭" aria-label="关闭" disabled={busy} onClick={close}>x</button></header><form className="task-editor-form" onSubmit={(event) => void save(event)}><div className="task-form"><section className="task-form-section"><div className="task-form-section-head"><h3>基本信息</h3></div><div className="task-form-basic-grid"><label className="task-form-title">任务名称 <small>可选</small><input autoFocus maxLength={120} value={title} onChange={(event) => setTitle(event.target.value)} placeholder="例如：实现项目任务看板" /></label><label>优先级<select value={priority} onChange={(event) => setPriority(event.target.value as Priority)}>{Object.entries(priorityLabels).map(([value, label]) => <option key={value} value={value}>{label}</option>)}</select></label></div></section><section className="task-form-section"><div className="task-form-section-head"><h3>任务内容</h3></div><label>任务说明<textarea required maxLength={12000} value={description} onChange={(event) => setDescription(event.target.value)} placeholder="说明背景、范围、限制和需要完成的实现。" /></label></section></div><footer><button type="button" className="secondary" disabled={busy} onClick={close}>取消</button><button className="primary" disabled={busy}>{busy ? "保存中" : "保存任务"}</button></footer></form></section></div>;
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
  const reviewBusy = Boolean(busy) || reviewSubmitting || isTaskOrchestrating(detail) || isTaskAwaitingMainMerge(detail);
  const canResumeStoppedOrchestration = detail.orchestrationStatus === "stopped";
  return <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="task-detail-title"><section className="modal task-dialog task-detail-dialog"><header className="task-detail-header"><div className="task-detail-heading"><h2 id="task-detail-title">{taskDisplayTitle(detail)}</h2><div><StatusBadge task={detail} /><span>更新于 {formatDate(detail.updatedAt)}</span></div></div><button type="button" className="task-detail-close" title="关闭" aria-label="关闭" onClick={close}>x</button></header><div className="task-detail-body"><div className="task-detail-meta"><span className={`priority-${detail.priority}`}>{priorityLabels[detail.priority]}优先级</span><span>执行权限：{policyLabel(permissionMode)}</span></div><section className="task-detail-section"><h3>任务说明</h3><p>{detail.description}</p></section><section className="task-detail-section"><h3>执行记录</h3>{detail.runs.length === 0 ? <p className="task-detail-empty">尚未下发。</p> : <div className="task-run-list"><div className="task-run-list-head"><span>次数</span><span>状态</span><span>时间</span></div>{detail.runs.map((run) => <div key={run.id}><b>第 {run.sequence} 次</b><span>{run.status}</span><time>{formatDate(run.createdAt)}</time>{run.failureReason && <small>{run.failureReason}</small>}</div>)}</div>}</section><section className="task-detail-section"><h3>验证记录</h3>{detail.verificationRuns.length === 0 ? <p className="task-detail-empty">尚无验证记录。</p> : <div className="verification-run-list">{detail.verificationRuns.map((run) => <details key={run.id} className="verification-run" open={run.status === "running"}><summary><b>{verificationPhaseLabel(run.phase)}</b><span className={`verification-status ${run.status}`}>{run.status === "running" ? "进行中" : run.status === "passed" ? "通过" : "失败"}</span><time>{formatDate(run.completedAt || run.createdAt)}</time></summary><p>{run.command}{run.reviewedSha ? ` @ ${run.reviewedSha.slice(0, 12)}` : ""}</p>{run.output && <pre>{run.output}</pre>}</details>)}</div>}</section>{reviewAction && <div className="task-review-sheet" ref={reviewSheetRef}><h3>要求修改</h3><textarea autoFocus required disabled={reviewSubmitting} value={note} onChange={(event) => setNote(event.target.value)} placeholder="说明需要补充或修改的内容" /><div><button className="secondary" disabled={reviewSubmitting} onClick={() => setReviewAction(null)}>返回</button><button className="primary" disabled={reviewSubmitting} onClick={() => void submitReview("request_changes")}>{reviewSubmitting ? "提交中" : "提交要求"}</button></div></div>}</div><footer className="task-detail-actions"><div className="task-detail-main-actions">{(detail.status === "todo" || detail.status === "action_required") && <><button className="secondary" disabled={Boolean(busy)} onClick={edit}>编辑</button><button className="secondary" disabled={Boolean(busy) || !canMoveUp} onClick={() => void move(detail.id, "up")}>上移</button><button className="secondary" disabled={Boolean(busy) || !canMoveDown} onClick={() => void move(detail.id, "down")}>下移</button></>}{detail.status === "awaiting_review" && <><button className="secondary" disabled={reviewBusy} onClick={() => setReviewAction("request_changes")}>要求修改</button><button className="primary" disabled={reviewBusy} onClick={() => void submitReview("accept")}>{reviewSubmitting ? "提交中" : "确认完成"}</button></>}{detail.status === "running" && <button className="task-detail-stop" disabled={Boolean(busy)} onClick={() => void transition("stop")}>{busy === "stop" ? "停止中" : "停止任务"}</button>}{(detail.status === "todo" || detail.status === "action_required") && <>{orchestrationEnabled && (canResumeStoppedOrchestration || !detail.orchestrationStatus) && <button className="secondary" disabled={Boolean(busy)} onClick={() => void enqueue()}>{busy === "enqueue" ? "处理中" : canResumeStoppedOrchestration ? "继续自动编排" : "加入自动队列"}</button>}<button className="primary" disabled={!detail.canDispatch || Boolean(busy)} onClick={() => void dispatch()}>{busy === "dispatch" ? "下发中" : "下发任务"}</button></>}{detail.status === "done" && <button className="secondary" disabled={Boolean(busy)} onClick={confirmTransition}>重新打开</button>}</div><button className="task-detail-delete" disabled={Boolean(busy)} onClick={() => void deleteTask()}>{busy === "delete" ? "删除中" : "删除任务"}</button></footer></section></div>;
}

function formatDate(value: string): string {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? "" : date.toLocaleString("zh-CN", { month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit" });
}
