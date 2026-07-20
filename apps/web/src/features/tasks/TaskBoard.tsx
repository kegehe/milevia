import { FormEvent, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { isTaskBlocked, priorityLabels, Request, statusLabels, Task, TaskDetail, TaskStatus, Priority, taskDisplayTitle } from "./task-model";
type EditorState = { task?: Task } | null;
type ReviewAction = "accept" | "request_changes";

const columnDefinitions: { id: string; label: string; statuses: TaskStatus[] }[] = [
  { id: "todo", label: "待处理", statuses: ["todo"] },
  { id: "blocked", label: "受阻", statuses: ["todo", "action_required"] },
  { id: "running", label: "执行中", statuses: ["running"] },
  { id: "awaiting_review", label: "待验收", statuses: ["awaiting_review"] },
  { id: "action_required", label: "需处理", statuses: ["action_required"] },
  { id: "done", label: "已完成", statuses: ["done"] },
];

export function TaskBoard({ projectID, initialTaskID, permissionMode, request, fail, close, onDispatched }: { projectID: string; initialTaskID?: string; permissionMode?: "approval_required" | "full_control"; request: Request; fail: (message: string) => void; close: () => void; onDispatched: (message: { id: string; role: "user" | "assistant"; content: string; createdAt: string }, runID: string) => void }) {
  const [tasks, setTasks] = useState<Task[]>([]);
  const [view, setView] = useState<"board" | "list">("board");
  const [includeCancelled, setIncludeCancelled] = useState(false);
  const [editor, setEditor] = useState<EditorState>(null);
  const [detail, setDetail] = useState<TaskDetail | null>(null);
  const [busy, setBusy] = useState("");

  const loadTasks = useCallback(async () => {
    const next = await request<Task[]>(`/api/projects/${projectID}/tasks`);
    setTasks(next);
    return next;
  }, [projectID, request]);

  const loadDetail = useCallback(async (taskID: string) => {
    const next = await request<TaskDetail>(`/api/tasks/${taskID}`);
    setDetail(next);
    return next;
  }, [request]);

  useEffect(() => { void loadTasks().catch((cause) => fail(cause instanceof Error ? cause.message : "无法加载任务")); }, [fail, loadTasks]);
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
    void loadDetail(initialTaskID).catch((cause) => fail(cause instanceof Error ? cause.message : "无法加载任务详情"));
  }, [fail, initialTaskID, loadDetail]);

  const visibleTasks = useMemo(() => includeCancelled ? tasks : tasks.filter((task) => task.status !== "cancelled"), [includeCancelled, tasks]);
  const openDetail = (taskID: string) => { void loadDetail(taskID).catch((cause) => fail(cause instanceof Error ? cause.message : "无法加载任务详情")); };
  const refresh = async (taskID?: string) => {
    await loadTasks();
    if (taskID) await loadDetail(taskID);
  };

  const dispatch = async () => {
    if (!detail) return;
    setBusy("dispatch");
    try {
      const result = await request<{ message: { id: string; role: "user" | "assistant"; content: string; createdAt: string }; runId: string }>(`/api/tasks/${detail.id}/dispatch`, { method: "POST", body: "{}" });
      onDispatched(result.message, result.runId);
      void loadTasks().catch((cause) => fail(cause instanceof Error ? cause.message : "无法刷新任务列表"));
      close();
    } catch (cause) { fail(cause instanceof Error ? cause.message : "无法下发任务"); }
    finally { setBusy(""); }
  };

  const transition = async (action: "cancel" | "reopen" | "stop") => {
    if (!detail) return;
    setBusy(action);
    try { await request(`/api/tasks/${detail.id}/${action}`, { method: "POST", body: "{}" }); await refresh(detail.id); }
    catch (cause) { fail(cause instanceof Error ? cause.message : "无法更新任务"); }
    finally { setBusy(""); }
  };
  const moveTask = async (taskID: string, direction: "up" | "down") => {
    const ordered = [...visibleTasks].sort((left, right) => left.position - right.position || left.title.localeCompare(right.title, "zh-CN"));
    const index = ordered.findIndex((task) => task.id === taskID);
    const neighbor = ordered[index + (direction === "up" ? -1 : 1)];
    const task = ordered[index];
    if (!task || !neighbor) return;
    setBusy("move");
    try {
      const position = direction === "up" ? neighbor.position - 0.5 : neighbor.position + 0.5;
      await request(`/api/tasks/${task.id}`, { method: "PATCH", body: JSON.stringify({ title: task.title, description: task.description, acceptanceCriteria: task.acceptanceCriteria, priority: task.priority, position }) });
      await refresh(task.id);
    } catch (cause) { fail(cause instanceof Error ? cause.message : "无法调整任务顺序"); }
    finally { setBusy(""); }
  };

  return <section className="task-workspace" aria-label="项目任务">
    <header className="task-workspace-head">
      <div><label>PROJECT TASKS</label><h2>任务编排</h2><p>手动下发，人工验收。当前执行权限：{permissionMode === "full_control" ? "完全控制" : "默认权限"}。</p></div>
      <div className="task-workspace-actions">
        <div className="task-view-switch" aria-label="任务视图"><button className={view === "board" ? "active" : ""} onClick={() => setView("board")}>看板</button><button className={view === "list" ? "active" : ""} onClick={() => setView("list")}>列表</button></div>
        <label className="task-cancelled-toggle"><input type="checkbox" checked={includeCancelled} onChange={(event) => setIncludeCancelled(event.target.checked)} />显示已取消</label>
        <button className="secondary" onClick={close}>返回对话</button>
        <button className="primary" onClick={() => setEditor({})}>新建任务</button>
      </div>
    </header>
    {visibleTasks.length === 0 ? <div className="task-empty"><h3>还没有任务</h3><p>将可验证的开发事项加入项目，再按依赖顺序手动下发。</p><button className="primary" onClick={() => setEditor({})}>新建任务</button></div> : view === "board" ? <TaskBoardColumns tasks={visibleTasks} open={openDetail} /> : <TaskList tasks={visibleTasks} open={openDetail} />}
    {editor && <TaskEditor projectID={projectID} task={editor.task} tasks={tasks} request={request} close={() => setEditor(null)} saved={async (taskID) => { setEditor(null); await refresh(taskID); }} fail={fail} />}
    {detail && <TaskDetailDialog detail={detail} permissionMode={permissionMode} busy={busy} close={() => setDetail(null)} refresh={() => refresh(detail.id)} dispatch={dispatch} transition={transition} edit={() => { const task = tasks.find((item) => item.id === detail.id); if (task) { setDetail(null); setEditor({ task }); } }} move={moveTask} canMoveUp={visibleTasks.some((item) => item.position < detail.position)} canMoveDown={visibleTasks.some((item) => item.position > detail.position)} request={request} fail={fail} />}
  </section>;
}

function TaskBoardColumns({ tasks, open }: { tasks: Task[]; open: (taskID: string) => void }) {
  const grouped = (definition: typeof columnDefinitions[number]) => tasks.filter((task) => {
    const blocked = isTaskBlocked(task);
    if (definition.id === "blocked") return blocked;
    return definition.statuses.includes(task.status) && !blocked;
  });
  return <div className="task-board-columns">{columnDefinitions.map((definition) => <section className={`task-board-column column-${definition.id}`} key={definition.id}><header><h3>{definition.label}</h3><b>{grouped(definition).length}</b></header><div>{grouped(definition).map((task) => <TaskItem key={task.id} task={task} open={open} />)}</div></section>)}</div>;
}

function TaskList({ tasks, open }: { tasks: Task[]; open: (taskID: string) => void }) {
  const sorted = [...tasks].sort((left, right) => left.position - right.position || left.title.localeCompare(right.title, "zh-CN"));
  return <div className="task-list"><div className="task-list-head"><span>任务</span><span>状态</span><span>优先级</span><span>依赖</span><span>最近更新</span></div>{sorted.map((task) => <button key={task.id} className="task-list-row" onClick={() => open(task.id)}><span><b>{taskDisplayTitle(task)}</b><small>{task.description}</small></span><StatusBadge task={task} /><em className={`priority-${task.priority}`}>{priorityLabels[task.priority]}</em><span>{task.blockedBy.length ? `受阻 ${task.blockedBy.length}` : task.dependsOn.length ? `前置 ${task.dependsOn.length}` : "无"}</span><time>{formatDate(task.updatedAt)}</time></button>)}</div>;
}

function TaskItem({ task, open }: { task: Task; open: (taskID: string) => void }) {
  return <button className={`task-item priority-${task.priority}`} onClick={() => open(task.id)}><div className="task-item-top"><StatusBadge task={task} /><span>{priorityLabels[task.priority]}</span></div><b>{taskDisplayTitle(task)}</b><p>{task.description}</p>{task.blockedBy.length > 0 ? <small className="task-blocked-by">等待：{task.blockedBy.map((item) => item.title).join("、")}</small> : <small>{task.dependsOn.length ? `前置任务 ${task.dependsOn.length}` : "可独立处理"}</small>}</button>;
}

function StatusBadge({ task }: { task: Task }) {
  const blocked = isTaskBlocked(task);
  const queued = task.status === "running" && task.lastRun?.status === "queued";
  const label = blocked ? "受阻" : queued ? "队列中" : statusLabels[task.status];
  return <span className={`task-status ${blocked ? "blocked" : queued ? "action_required" : task.status}`}>{label}</span>;
}

function TaskEditor({ projectID, task, tasks, request, close, saved, fail }: { projectID: string; task?: Task; tasks: Task[]; request: Request; close: () => void; saved: (taskID: string) => Promise<void>; fail: (message: string) => void }) {
  const [title, setTitle] = useState(task?.title || "");
  const [description, setDescription] = useState(task?.description || "");
  const [acceptanceCriteria, setAcceptanceCriteria] = useState(task?.acceptanceCriteria || "");
  const [priority, setPriority] = useState<Priority>(task?.priority || "normal");
  const [dependencies, setDependencies] = useState<string[]>(task?.dependsOn.map((item) => item.taskId) || []);
  const [busy, setBusy] = useState(false);
  const save = async (event: FormEvent) => {
    event.preventDefault(); setBusy(true);
    try {
      const payload = { title, description, acceptanceCriteria, priority, position: task?.position || 0, predecessorTaskIds: dependencies };
      const result = await request<Task>(task ? `/api/tasks/${task.id}` : `/api/projects/${projectID}/tasks`, { method: task ? "PATCH" : "POST", body: JSON.stringify(payload) });
      await saved(result.id);
    } catch (cause) { fail(cause instanceof Error ? cause.message : "无法保存任务"); }
    finally { setBusy(false); }
  };
  return <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="task-editor-title"><section className="modal task-dialog"><header><div><label>PROJECT TASK</label><h2 id="task-editor-title">{task ? "编辑任务" : "新建任务"}</h2></div><button title="关闭" disabled={busy} onClick={close}>x</button></header><form onSubmit={(event) => void save(event)}><div className="task-form"><label>任务名称 <small>（可选）</small><input autoFocus maxLength={120} value={title} onChange={(event) => setTitle(event.target.value)} placeholder="例如：实现项目任务看板" /></label><label>任务说明<textarea required maxLength={12000} value={description} onChange={(event) => setDescription(event.target.value)} placeholder="说明背景、范围、限制和需要完成的实现。" /></label><label>验收条件 <small>（可选）</small><textarea maxLength={12000} value={acceptanceCriteria} onChange={(event) => setAcceptanceCriteria(event.target.value)} placeholder="用可验证的结果描述完成标准。" /></label><label>优先级<select value={priority} onChange={(event) => setPriority(event.target.value as Priority)}>{Object.entries(priorityLabels).map(([value, label]) => <option key={value} value={value}>{label}</option>)}</select></label><fieldset><legend>前置任务</legend>{tasks.filter((item) => item.id !== task?.id && (item.status !== "cancelled" || dependencies.includes(item.id))).map((item) => <label className="task-dependency-option" key={item.id}><input type="checkbox" checked={dependencies.includes(item.id)} onChange={(event) => setDependencies((current) => event.target.checked ? [...current, item.id] : current.filter((id) => id !== item.id))} /> <span>{item.title}</span><em>{statusLabels[item.status]}</em></label>)}</fieldset></div><footer><button type="button" className="secondary" disabled={busy} onClick={close}>取消</button><button className="primary" disabled={busy}>{busy ? "保存中" : "保存任务"}</button></footer></form></section></div>;
}

function TaskDetailDialog({ detail, permissionMode, busy, close, refresh, dispatch, transition, edit, move, canMoveUp, canMoveDown, request, fail }: { detail: TaskDetail; permissionMode?: "approval_required" | "full_control"; busy: string; close: () => void; refresh: () => Promise<void>; dispatch: () => Promise<void>; transition: (action: "cancel" | "reopen" | "stop") => Promise<void>; edit: () => void; move: (taskID: string, direction: "up" | "down") => Promise<void>; canMoveUp: boolean; canMoveDown: boolean; request: Request; fail: (message: string) => void }) {
  const [reviewAction, setReviewAction] = useState<ReviewAction | null>(null);
  const [note, setNote] = useState("");
  const [reviewSubmitting, setReviewSubmitting] = useState(false);
  const reviewSheetRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (reviewAction && reviewSheetRef.current) {
      reviewSheetRef.current.scrollIntoView({ behavior: "smooth", block: "nearest" });
    }
  }, [reviewAction]);
  const submitReview = async (action: ReviewAction) => {
    if (reviewSubmitting) return;
    if (action === "request_changes" && !note.trim()) { fail("请填写需要修改的原因"); return; }
    setReviewSubmitting(true);
    try { await request(`/api/tasks/${detail.id}/review`, { method: "POST", body: JSON.stringify({ action, note: action === "accept" ? "" : note }) }); setReviewAction(null); setNote(""); await refresh(); }
    catch (cause) { fail(cause instanceof Error ? cause.message : "无法提交验收"); }
    finally { setReviewSubmitting(false); }
  };
  const confirmTransition = (action: "cancel" | "reopen") => { if (window.confirm(action === "cancel" ? "取消后不会自动解除下游任务阻塞。确认取消？" : "重新打开会重新阻塞依赖此任务的后续任务。确认继续？")) void transition(action); };
  const queued = detail.status === "running" && detail.lastRun?.status === "queued";
  const reviewBusy = Boolean(busy) || reviewSubmitting;
  return <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="task-detail-title"><section className="modal task-dialog task-detail-dialog"><header><div><label>PROJECT TASK</label><h2 id="task-detail-title">{taskDisplayTitle(detail)}</h2><StatusBadge task={detail} /></div><button title="关闭" disabled={reviewBusy} onClick={close}>x</button></header><div className="task-detail-body"><div className="task-detail-meta"><span className={`priority-${detail.priority}`}>{priorityLabels[detail.priority]}优先级</span><span>更新于 {formatDate(detail.updatedAt)}</span></div><section><h3>任务说明</h3><p>{detail.description}</p></section>{detail.acceptanceCriteria && <section><h3>验收条件</h3><p>{detail.acceptanceCriteria}</p></section>}<section><h3>依赖关系</h3>{detail.blockedBy.length > 0 ? <p className="task-blocked-by">当前受阻：{detail.blockedBy.map((item) => item.title).join("、")}</p> : <p>没有未完成的前置任务。</p>}{detail.blocks.length > 0 && <p>完成后将解锁：{detail.blocks.map((item) => item.title).join("、")}</p>}</section><section><h3>下发内容</h3><pre className="task-prompt">{detail.promptPreview}</pre><p>执行权限：{permissionMode === "full_control" ? "完全控制" : "默认权限"}</p>{detail.blockReason && <p className="task-dispatch-reason">{detail.blockReason}</p>}</section><section><h3>执行记录</h3>{detail.runs.length === 0 ? <p>尚未下发。</p> : <div className="task-run-list">{detail.runs.map((run) => <div key={run.id}><b>第 {run.sequence} 次</b><span>{run.status}</span><time>{formatDate(run.createdAt)}</time>{run.failureReason && <small>{run.failureReason}</small>}</div>)}</div>}</section></div><footer className="task-detail-actions">{(detail.status === "todo" || detail.status === "action_required") && <><button className="secondary" disabled={Boolean(busy)} onClick={edit}>编辑</button><button className="secondary" disabled={Boolean(busy) || !canMoveUp} onClick={() => void move(detail.id, "up")}>上移</button><button className="secondary" disabled={Boolean(busy) || !canMoveDown} onClick={() => void move(detail.id, "down")}>下移</button></>}{detail.status === "awaiting_review" && <><button className="secondary" disabled={reviewBusy} onClick={() => setReviewAction("request_changes")}>要求修改</button><button className="primary" disabled={reviewBusy} onClick={() => void submitReview("accept")}>{reviewSubmitting ? "提交中" : "确认完成"}</button></>}{detail.status === "running" && <button className="danger-text" disabled={Boolean(busy) || queued} title={queued ? "排队任务不能单独停止" : undefined} onClick={() => void transition("stop")}>{queued ? "队列中" : busy === "stop" ? "停止中" : "停止任务"}</button>}{(detail.status === "todo" || detail.status === "action_required") && <><button className="danger-text" disabled={Boolean(busy)} onClick={() => confirmTransition("cancel")}>取消任务</button><button className="primary" disabled={!detail.canDispatch || Boolean(busy)} onClick={() => void dispatch()}>{busy === "dispatch" ? "下发中" : "下发任务"}</button></>}{(detail.status === "done" || detail.status === "cancelled") && <button className="secondary" disabled={Boolean(busy)} onClick={() => confirmTransition("reopen")}>重新打开</button>}</footer>{reviewAction && <div className="task-review-sheet" ref={reviewSheetRef}><h3>要求修改</h3><textarea autoFocus required disabled={reviewSubmitting} value={note} onChange={(event) => setNote(event.target.value)} placeholder="说明需要补充或修改的内容" /><div><button className="secondary" disabled={reviewSubmitting} onClick={() => setReviewAction(null)}>返回</button><button className="primary" disabled={reviewSubmitting} onClick={() => void submitReview("request_changes")}>{reviewSubmitting ? "提交中" : "提交要求"}</button></div></div>}</section></div>;
}

function formatDate(value: string): string {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? "" : date.toLocaleString("zh-CN", { month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit" });
}
