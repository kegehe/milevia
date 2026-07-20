import { FormEvent, MouseEvent, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { createPortal } from "react-dom";

import { canOfferDispatch, canRedispatch, filterQueueTasks, isTaskBlocked, priorityLabels, Request, sortQueueTasks, statusLabels, Task, TaskDetail, TaskFilter, taskDisplayTitle, taskQueueNote } from "./task-model";

type DispatchedMessage = { id: string; role: "user" | "assistant"; content: string; createdAt: string };
type DispatchResult = { message: DispatchedMessage; runId: string };

const filters: { id: TaskFilter; label: string }[] = [
  { id: "active", label: "全部" },
  { id: "todo", label: "待处理" },
  { id: "running", label: "执行中" },
  { id: "awaiting_review", label: "待验收" },
];

export function TaskQueue({ projectID, permissionMode, request, fail, onDispatched, openBoard }: { projectID: string; permissionMode?: "approval_required" | "full_control"; request: Request; fail: (message: string) => void; onDispatched: (message: DispatchedMessage, runID: string) => void; openBoard: (taskID?: string) => void }) {
  const [tasks, setTasks] = useState<Task[]>([]);
  const [filter, setFilter] = useState<TaskFilter>("active");
  const [confirmingTaskID, setConfirmingTaskID] = useState<string | null>(null);
  const [detail, setDetail] = useState<TaskDetail | null>(null);
  const [loadingDetail, setLoadingDetail] = useState(false);
  const [dispatching, setDispatching] = useState(false);
  const [mobileOpen, setMobileOpen] = useState(false);
  const [quickCreateOpen, setQuickCreateOpen] = useState(false);
  const [quickDescription, setQuickDescription] = useState("");
  const [quickCreating, setQuickCreating] = useState(false);
  const [reviewNote, setReviewNote] = useState("");
  const [reviewingTaskID, setReviewingTaskID] = useState<string | null>(null);
  const [reviewSubmitting, setReviewSubmitting] = useState(false);
  const [redispatchingTaskID, setRedispatchingTaskID] = useState<string | null>(null);
  const [inlineDetailID, setInlineDetailID] = useState<string | null>(null);
  const [inlineDetail, setInlineDetail] = useState<TaskDetail | null>(null);
  const [inlineDetailLoading, setInlineDetailLoading] = useState(false);
  const [inlineBusy, setInlineBusy] = useState("");
  const confirmationRequest = useRef(0);
  const inlineRequest = useRef(0);

  const loadTasks = useCallback(async () => {
    const next = await request<Task[]>(`/api/projects/${projectID}/tasks`);
    setTasks(next);
  }, [projectID, request]);

  useEffect(() => { void loadTasks().catch((cause) => fail(cause instanceof Error ? cause.message : "无法加载任务队列")); }, [fail, loadTasks]);
  useEffect(() => {
    const interval = window.setInterval(() => { void loadTasks().catch(() => undefined); }, 10_000);
    return () => window.clearInterval(interval);
  }, [loadTasks]);

  const queueTasks = useMemo(() => sortQueueTasks(filterQueueTasks(tasks, filter)), [filter, tasks]);
  const taskCounts = useMemo(() => Object.fromEntries(filters.map((item) => [item.id, filterQueueTasks(tasks, item.id).length])) as Record<TaskFilter, number>, [tasks]);
  const confirmingTask = tasks.find((task) => task.id === confirmingTaskID);

  const closeConfirmation = () => {
    if (dispatching) return;
    confirmationRequest.current++;
    setConfirmingTaskID(null);
    setDetail(null);
    setLoadingDetail(false);
  };

  const openConfirmation = async (event: MouseEvent<HTMLButtonElement>, taskID: string) => {
    event.stopPropagation();
    const requestID = ++confirmationRequest.current;
    setConfirmingTaskID(taskID);
    setDetail(null);
    setLoadingDetail(true);
    try {
      const next = await request<TaskDetail>(`/api/tasks/${taskID}`);
      if (confirmationRequest.current === requestID) setDetail(next);
    } catch (cause) {
      if (confirmationRequest.current === requestID) {
        fail(cause instanceof Error ? cause.message : "无法检查任务下发条件");
        setConfirmingTaskID(null);
      }
    } finally {
      if (confirmationRequest.current === requestID) setLoadingDetail(false);
    }
  };

  const openInlineDetail = async (taskID: string) => {
    const requestID = ++inlineRequest.current;
    setInlineDetailID(taskID);
    setInlineDetail(null);
    setInlineDetailLoading(true);
    setInlineBusy("");
    try {
      const next = await request<TaskDetail>(`/api/tasks/${taskID}`);
      if (inlineRequest.current === requestID) setInlineDetail(next);
    } catch (cause) {
      if (inlineRequest.current === requestID) fail(cause instanceof Error ? cause.message : "无法加载任务详情");
    } finally {
      if (inlineRequest.current === requestID) setInlineDetailLoading(false);
    }
  };

  const closeInlineDetail = () => {
    inlineRequest.current++;
    setInlineDetailID(null);
    setInlineDetail(null);
    setInlineDetailLoading(false);
    setInlineBusy("");
  };

  const inlineDispatch = async () => {
    if (!inlineDetail || !inlineDetail.canDispatch) return;
    setInlineBusy("dispatch");
    try {
      const result = await request<DispatchResult>(`/api/tasks/${inlineDetail.id}/dispatch`, { method: "POST", body: "{}" });
      onDispatched(result.message, result.runId);
      closeInlineDetail();
      await loadTasks();
    } catch (cause) {
      fail(cause instanceof Error ? cause.message : "无法下发任务");
      void loadTasks().catch(() => undefined);
    } finally {
      setInlineBusy("");
    }
  };

  const inlineTransition = async (action: "cancel" | "reopen" | "stop") => {
    if (!inlineDetail) return;
    setInlineBusy(action);
    try {
      await request(`/api/tasks/${inlineDetail.id}/${action}`, { method: "POST", body: "{}" });
      const next = await request<TaskDetail>(`/api/tasks/${inlineDetail.id}`);
      setInlineDetail(next);
      await loadTasks();
    } catch (cause) { fail(cause instanceof Error ? cause.message : "无法更新任务"); }
    finally { setInlineBusy(""); }
  };

  const inlineReview = async (action: "accept" | "request_changes", note: string) => {
    if (!inlineDetail) return;
    if (action === "request_changes" && !note.trim()) { fail("请填写需要修改的原因"); return; }
    try {
      await request(`/api/tasks/${inlineDetail.id}/review`, { method: "POST", body: JSON.stringify({ action, note: action === "accept" ? "" : note }) });
      const next = await request<TaskDetail>(`/api/tasks/${inlineDetail.id}`);
      setInlineDetail(next);
      await loadTasks();
    } catch (cause) { fail(cause instanceof Error ? cause.message : "无法提交验收"); }
  };

  const dispatch = async () => {
    if (!detail || detail.id !== confirmingTaskID || !detail.canDispatch) return;
    setDispatching(true);
    try {
      const result = await request<DispatchResult>(`/api/tasks/${detail.id}/dispatch`, { method: "POST", body: "{}" });
      onDispatched(result.message, result.runId);
      setConfirmingTaskID(null);
      setDetail(null);
      await loadTasks();
    } catch (cause) {
      fail(cause instanceof Error ? cause.message : "无法下发任务");
      void loadTasks().catch(() => undefined);
    } finally {
      setDispatching(false);
    }
  };

  const confirmTransition = (action: "cancel" | "reopen") => {
    if (window.confirm(action === "cancel" ? "取消后不会自动解除下游任务阻塞。确认取消？" : "重新打开会重新阻塞依赖此任务的后续任务。确认继续？")) {
      void inlineTransition(action);
    }
  };

  const submitReview = async (taskID: string, action: "accept" | "request_changes") => {
    if (reviewSubmitting) return;
    if (action === "request_changes" && !reviewNote.trim()) { fail("请填写需要修改的原因"); return; }
    setReviewSubmitting(true);
    try {
      await request(`/api/tasks/${taskID}/review`, { method: "POST", body: JSON.stringify({ action, note: action === "accept" ? "" : reviewNote }) });
      setReviewingTaskID(null);
      setReviewNote("");
      await loadTasks();
    } catch (cause) {
      fail(cause instanceof Error ? cause.message : "无法提交验收");
    } finally {
      setReviewSubmitting(false);
    }
  };

  const redispatchTask = async (event: MouseEvent<HTMLButtonElement>, taskID: string) => {
    event.stopPropagation();
    setRedispatchingTaskID(taskID);
    try {
      const result = await request<DispatchResult>(`/api/tasks/${taskID}/dispatch`, { method: "POST", body: "{}" });
      onDispatched(result.message, result.runId);
      await loadTasks();
    } catch (cause) {
      fail(cause instanceof Error ? cause.message : "无法重新下发任务");
    } finally {
      setRedispatchingTaskID(null);
    }
  };

  const quickCreate = async (event: FormEvent) => {
    event.preventDefault();
    if (!quickDescription.trim() || quickCreating) return;
    setQuickCreating(true);
    try {
      await request<Task>(`/api/projects/${projectID}/tasks`, { method: "POST", body: JSON.stringify({ title: "", description: quickDescription.trim(), acceptanceCriteria: "", priority: "normal", position: 0 }) });
      setQuickDescription("");
      setQuickCreateOpen(false);
      await loadTasks();
    } catch (cause) {
      fail(cause instanceof Error ? cause.message : "无法创建任务");
    } finally {
      setQuickCreating(false);
    }
  };

  const closeQuickCreate = () => {
    if (quickCreating) return;
    setQuickCreateOpen(false);
    setQuickDescription("");
  };

  return <section className={`task-queue ${mobileOpen ? "mobile-open" : ""}`} aria-label="任务队列">
    <button type="button" className="task-queue-mobile-toggle" aria-expanded={mobileOpen} onClick={() => setMobileOpen((open) => !open)}>任务 <b>{taskCounts.active}</b></button>
    <div className="task-queue-panel">
      <header className="task-queue-head"><div><span>任务队列</span><b>{taskCounts.active}</b></div><div className="task-queue-head-actions"><button type="button" className="task-queue-add" title="快速创建任务" onClick={() => { setQuickCreateOpen(true); setQuickDescription(""); }}>+</button><button type="button" className="task-queue-all" onClick={() => openBoard()}>查看全部</button></div></header>
      {quickCreateOpen && <form className="task-queue-quick-create" onSubmit={(event) => void quickCreate(event)}><textarea autoFocus required maxLength={12000} rows={2} value={quickDescription} disabled={quickCreating} onChange={(event) => setQuickDescription(event.target.value)} placeholder="输入任务说明，回车快速创建…" /><div className="task-queue-quick-create-actions"><button type="button" className="secondary" disabled={quickCreating} onClick={closeQuickCreate}>取消</button><button type="submit" className="primary" disabled={!quickDescription.trim() || quickCreating}>{quickCreating ? "创建中" : "创建"}</button></div></form>}
      <nav className="task-queue-filters" aria-label="任务筛选">{filters.map((item) => <button type="button" key={item.id} className={filter === item.id ? "active" : ""} aria-pressed={filter === item.id} onClick={() => setFilter(item.id)}>{item.label}<span>{taskCounts[item.id]}</span></button>)}</nav>
      <div className={`task-queue-list${reviewingTaskID ? " reviewing" : ""}`}>{queueTasks.length === 0 ? <p className="task-queue-empty">当前筛选没有任务。</p> : queueTasks.map((task) => <TaskQueueRow key={task.id} task={task} open={() => openInlineDetail(task.id)} confirm={openConfirmation} redispatch={redispatchTask} redispatching={redispatchingTaskID === task.id} openReview={(taskID) => { setReviewingTaskID(taskID); setReviewNote(""); }} closeReview={() => { setReviewingTaskID(null); setReviewNote(""); }} reviewingTaskID={reviewingTaskID} reviewNote={reviewNote} setReviewNote={setReviewNote} reviewSubmitting={reviewSubmitting} submitReview={submitReview} />)}</div>
    </div>
    {confirmingTask && createPortal(<DispatchConfirmation task={confirmingTask} detail={detail} loading={loadingDetail} dispatching={dispatching} permissionMode={permissionMode} close={closeConfirmation} openBoard={() => openBoard(confirmingTask.id)} dispatch={dispatch} />, document.body)}
    {inlineDetailID && <InlineTaskDetail detail={inlineDetail} loading={inlineDetailLoading} busy={inlineBusy} close={closeInlineDetail} dispatch={inlineDispatch} transition={inlineTransition} review={inlineReview} openBoard={() => openBoard(inlineDetailID)} confirmTransition={confirmTransition} />}
  </section>;
}

function TaskQueueRow({ task, open, confirm, redispatch, redispatching, openReview, closeReview, reviewingTaskID, reviewNote, setReviewNote, reviewSubmitting, submitReview }: { task: Task; open: () => void; confirm: (event: MouseEvent<HTMLButtonElement>, taskID: string) => Promise<void>; redispatch: (event: MouseEvent<HTMLButtonElement>, taskID: string) => Promise<void>; redispatching: boolean; openReview: (taskID: string) => void; closeReview: () => void; reviewingTaskID: string | null; reviewNote: string; setReviewNote: (value: string) => void; reviewSubmitting: boolean; submitReview: (taskID: string, action: "accept" | "request_changes") => Promise<void> }) {
  const blocked = isTaskBlocked(task);
  const queued = task.status === "running" && task.lastRun?.status === "queued";
  const status = blocked ? "受阻" : queued ? "队列中" : statusLabels[task.status];
  const note = taskQueueNote(task);
  const isReviewing = reviewingTaskID === task.id;
  const reviewRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!isReviewing || !reviewRef.current) return;
    reviewRef.current.scrollIntoView({ behavior: "smooth", block: "nearest" });
  }, [isReviewing]);
  return <article className="task-queue-row">
    <button type="button" className="task-queue-open" onClick={open}>
      <span className={`task-status ${blocked ? "blocked" : queued ? "action_required" : task.status}`}>{status}</span>
      <b>{taskDisplayTitle(task)}</b>
      <small>{note}</small>
    </button>
    <div className="task-queue-row-actions">
      <em className={`priority-${task.priority}`}>{priorityLabels[task.priority]}</em>
      {canOfferDispatch(task) && <button type="button" className="task-queue-dispatch" onClick={(event) => void confirm(event, task.id)}>下发</button>}
      {canRedispatch(task) && <button type="button" className="task-queue-redispatch" disabled={redispatching} onClick={(event) => void redispatch(event, task.id)}>{redispatching ? "下发中" : "重新下发"}</button>}
      {task.status === "awaiting_review" && !isReviewing && <button type="button" className="task-queue-review secondary" onClick={(event) => { event.stopPropagation(); openReview(task.id); }}>验收</button>}
    </div>
    {isReviewing && <div className="task-queue-inline-review" ref={reviewRef}>
      <textarea autoFocus required disabled={reviewSubmitting} value={reviewNote} onChange={(event) => setReviewNote(event.target.value)} placeholder="说明需要补充或修改的内容，留空即确认完成…" />
      <div className="task-queue-inline-review-actions">
        <button type="button" className="secondary" disabled={reviewSubmitting} onClick={(event) => { event.stopPropagation(); submitReview(task.id, "accept"); }}>确认完成</button>
        <button type="button" className="primary" disabled={!reviewNote.trim() || reviewSubmitting} onClick={(event) => { event.stopPropagation(); submitReview(task.id, "request_changes"); }}>{reviewSubmitting ? "提交中" : "要求修改"}</button>
        <button type="button" className="danger-text" disabled={reviewSubmitting} onClick={(event) => { event.stopPropagation(); closeReview(); }}>取消</button>
      </div>
    </div>}
  </article>;
}

function DispatchConfirmation({ task, detail, loading, dispatching, permissionMode, close, openBoard, dispatch }: { task: Task; detail: TaskDetail | null; loading: boolean; dispatching: boolean; permissionMode?: "approval_required" | "full_control"; close: () => void; openBoard: () => void; dispatch: () => Promise<void> }) {
  const canDispatch = Boolean(detail?.canDispatch) && !loading;
  const eligibility = loading ? "正在检查依赖和任务状态..." : detail?.canDispatch ? "任务满足下发条件。" : detail?.blockReason || "当前任务不能下发。";
  return <div className="backdrop task-dispatch-backdrop" role="dialog" aria-modal="true" aria-labelledby="task-dispatch-title"><section className="modal task-dispatch-dialog"><header><div><label>DISPATCH TASK</label><h2 id="task-dispatch-title">确认下发</h2></div><button type="button" title="关闭" disabled={dispatching} onClick={close}>x</button></header><div className="task-dispatch-body"><div className="task-dispatch-title"><span className={`task-status ${task.status}`}>{statusLabels[task.status]}</span><h3>{taskDisplayTitle(task)}</h3><em className={`priority-${task.priority}`}>{priorityLabels[task.priority]}优先级</em></div><p className="task-dispatch-description">{detail?.description || task.description}</p><dl><div><dt>当前权限</dt><dd>{permissionMode === "full_control" ? "完全控制" : "默认权限"}</dd></div><div><dt>依赖检查</dt><dd className={canDispatch ? "eligible" : "blocked"}>{eligibility}</dd></div></dl></div><footer><button type="button" className="secondary" disabled={dispatching} onClick={openBoard}>查看完整详情</button><span><button type="button" className="secondary" disabled={dispatching} onClick={close}>取消</button><button type="button" className="primary" disabled={!canDispatch || dispatching} onClick={() => void dispatch()}>{dispatching ? "下发中" : "确认下发"}</button></span></footer></section></div>;
}

function InlineTaskDetail({ detail, loading, busy, close, dispatch, transition, review, openBoard, confirmTransition }: { detail: TaskDetail | null; loading: boolean; busy: string; close: () => void; dispatch: () => Promise<void>; transition: (action: "cancel" | "reopen" | "stop") => Promise<void>; review: (action: "accept" | "request_changes", note: string) => Promise<void>; openBoard: () => void; confirmTransition: (action: "cancel" | "reopen") => void }) {
  const detailRef = useRef<HTMLDivElement>(null);
  const [localReviewNote, setLocalReviewNote] = useState("");
  const [localReviewSubmitting, setLocalReviewSubmitting] = useState(false);

  useEffect(() => {
    if (detailRef.current && detail) {
      detailRef.current.scrollIntoView({ behavior: "smooth", block: "nearest" });
    }
  }, [detail]);

  if (loading) return <div className="task-inline-detail"><div className="task-inline-detail-body"><p className="task-inline-loading">加载任务详情...</p></div></div>;
  if (!detail) return <div className="task-inline-detail"><div className="task-inline-detail-body"><p className="task-inline-loading">无法加载任务详情。</p></div></div>;

  const blocked = isTaskBlocked(detail);
  const queued = detail.status === "running" && detail.lastRun?.status === "queued";
  const reviewBusy = Boolean(busy) || localReviewSubmitting;

  const doReview = async (action: "accept" | "request_changes") => {
    if (localReviewSubmitting) return;
    if (action === "request_changes" && !localReviewNote.trim()) return;
    setLocalReviewSubmitting(true);
    try {
      await review(action, localReviewNote);
      setLocalReviewNote("");
    } finally {
      setLocalReviewSubmitting(false);
    }
  };

  return <div className="task-inline-detail" ref={detailRef}>
    <div className="task-inline-detail-head">
      <div className="task-inline-detail-title">
        <span className={`task-status ${blocked ? "blocked" : queued ? "action_required" : detail.status}`}>{blocked ? "受阻" : queued ? "队列中" : statusLabels[detail.status]}</span>
        <b>{taskDisplayTitle(detail)}</b>
        <span className={`task-inline-priority priority-${detail.priority}`}>{priorityLabels[detail.priority]}优先级</span>
      </div>
      <button type="button" className="task-inline-close" title="关闭详情" disabled={reviewBusy} onClick={close}>x</button>
    </div>
    <div className="task-inline-detail-body">
      {detail.description && <p className="task-inline-description">{detail.description}</p>}
      {detail.acceptanceCriteria && <div className="task-inline-section"><b>验收条件</b><p>{detail.acceptanceCriteria}</p></div>}
      <div className="task-inline-section">
        <b>依赖关系</b>
        {detail.blockedBy.length > 0 ? <p className="task-blocked-by">受阻：{detail.blockedBy.map((item) => item.title).join("、")}</p> : <p>无阻塞。</p>}
        {detail.blocks.length > 0 && <p>完成后解锁：{detail.blocks.map((item) => item.title).join("、")}</p>}
      </div>
      {detail.runs.length > 0 && <div className="task-inline-section"><b>执行记录</b><div className="task-inline-runs">{detail.runs.map((run) => <span key={run.id} className="task-inline-run"><em>{run.status}</em><small>第 {run.sequence} 次 · {formatDate(run.createdAt)}</small></span>)}</div></div>}
      {detail.blockReason && <p className="task-dispatch-reason">{detail.blockReason}</p>}
    </div>
    <div className="task-inline-detail-actions">
      {(detail.status === "todo" || detail.status === "action_required") && <>
        {detail.canDispatch && <button className="primary" disabled={Boolean(busy)} onClick={() => void dispatch()}>{busy === "dispatch" ? "下发中" : "下发任务"}</button>}
        <button className="danger-text" disabled={Boolean(busy)} onClick={() => confirmTransition("cancel")}>取消任务</button>
      </>}
      {detail.status === "running" && <button className="danger-text" disabled={Boolean(busy) || queued} onClick={() => void transition("stop")}>{queued ? "队列中" : busy === "stop" ? "停止中" : "停止任务"}</button>}
      {detail.status === "awaiting_review" && <>
        <button className="secondary" disabled={reviewBusy} onClick={() => void doReview("accept")}>{localReviewSubmitting ? "提交中" : "确认完成"}</button>
        <div className="task-inline-review-reject">
          <textarea autoFocus required disabled={localReviewSubmitting} value={localReviewNote} onChange={(event) => setLocalReviewNote(event.target.value)} placeholder="说明需要修改的内容…" />
          <button className="primary" disabled={!localReviewNote.trim() || localReviewSubmitting} onClick={() => void doReview("request_changes")}>{localReviewSubmitting ? "提交中" : "要求修改"}</button>
        </div>
      </>}
      {(detail.status === "done" || detail.status === "cancelled") && <button className="secondary" disabled={Boolean(busy)} onClick={() => confirmTransition("reopen")}>重新打开</button>}
      <button className="secondary" disabled={reviewBusy} onClick={openBoard}>查看完整详情</button>
    </div>
  </div>;
}

function formatDate(value: string): string {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? "" : date.toLocaleString("zh-CN", { month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit" });
}