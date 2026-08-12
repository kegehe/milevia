import { FormEvent, MouseEvent, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { toast } from "sonner";

import { canOfferDispatch, canRedispatch, filterQueueTasks, isTaskAwaitingMainMerge, isTaskOrchestrating, priorityLabels, Priority, Request, sortQueueTasks, statusLabels, taskDisplayStatus, taskDisplayStatusClass, taskRunStatusLabel, Task, TaskDetail, TaskFilter, taskDisplayTitle, taskQueueNote } from "./task-model";
import { ConfirmDialog } from "../../components/ConfirmDialog";

type DispatchedMessage = { id: string; role: "user" | "assistant"; content: string; createdAt: string };
type DispatchResult = { message: DispatchedMessage; runId: string };

const filters: { id: TaskFilter; label: string }[] = [
  { id: "active", label: "全部" },
  { id: "todo", label: "待处理" },
  { id: "running", label: "执行中" },
  { id: "awaiting_review", label: "待验收" },
];

type QueueDragData = { taskID: string; sourceIndex: number };

// WebView2 上 dataTransfer 的自定义 MIME 数据往返不可靠（getData 常读回空），
// 拖拽目标数据改走内存：拖拽源写入、拖放目标读取。行组件彼此独立，故用模块级 ref。
const queueDragPayloadRef: { current: QueueDragData | null } = { current: null };
type PendingConfirm = { title: string; message: React.ReactNode; confirmLabel?: string; danger?: boolean; className?: string; icon?: React.ReactNode; onConfirm: () => void; onCancel: () => void } | null;
type ExecutionPolicy = "approval_required" | "full_control" | "read_only" | "workspace_write";

function policyLabel(policy?: ExecutionPolicy): string {
  if (policy === "full_control") return "完全控制";
  if (policy === "read_only") return "仅分析";
  if (policy === "workspace_write") return "项目内执行";
  return "默认权限";
}

function TaskQueueIcon() {
  return <svg className="task-queue-title-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M5 6.5h14M5 12h14M5 17.5h14M7.5 6.5v0M7.5 12v0M7.5 17.5v0" /></svg>;
}

// 一键验收确认弹窗标题图标：对勾徽标，与待验收态的语义呼应。
function ReviewAllIcon() {
  return <svg className="review-all-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M4.5 12.5 9.8 18 19.5 6.5" /></svg>;
}

// 下发确认弹窗标题图标：纸飞机，与队列里"下发任务"按钮的语义一致。
function DispatchIcon() {
  return <svg className="task-dispatch-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="m21 3-7.6 18-3.3-7.1L3 10.6 21 3Z" /><path d="m10.1 13.9 4.3-4.3" /></svg>;
}

function DispatchCloseIcon() {
  return <svg className="task-dispatch-close-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="m7 7 10 10M17 7 7 17" /></svg>;
}

export function TaskQueue({ projectID, conversationID, permissionMode, request, fail, dispatchDisabled = false, onDispatched, openBoard }: { projectID: string; conversationID: string; permissionMode?: ExecutionPolicy; request: Request; fail: (message: string) => void; dispatchDisabled?: boolean; onDispatched: (message: DispatchedMessage, runID: string) => void; openBoard: (taskID?: string) => void }) {
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
  const [approvingAll, setApprovingAll] = useState(false);
  const [inlineDetailID, setInlineDetailID] = useState<string | null>(null);
  const [inlineEditRequestedID, setInlineEditRequestedID] = useState<string | null>(null);
  const [inlineDetail, setInlineDetail] = useState<TaskDetail | null>(null);
  const [inlineDetailLoading, setInlineDetailLoading] = useState(false);
  const [inlineBusy, setInlineBusy] = useState("");
  const [pendingConfirm, setPendingConfirm] = useState<PendingConfirm>(null);
  const confirmationRequest = useRef(0);
  const inlineRequest = useRef(0);
  const redispatchRequest = useRef(0);
  const reviewAllRequest = useRef(0);
  const tasksRequestVersion = useRef(0);
  const pinLockRef = useRef(false);
  const mountedRef = useRef(true);
  const conversationIDRef = useRef(conversationID);
  conversationIDRef.current = conversationID;

  useEffect(() => {
    mountedRef.current = true;
    return () => { mountedRef.current = false; };
  }, []);

  useEffect(() => {
    confirmationRequest.current++;
    inlineRequest.current++;
    redispatchRequest.current++;
    setConfirmingTaskID(null);
    setDetail(null);
    setLoadingDetail(false);
    setDispatching(false);
    setRedispatchingTaskID(null);
    setApprovingAll(false);
    setInlineDetailID(null);
    setInlineEditRequestedID(null);
    setInlineDetail(null);
    setInlineDetailLoading(false);
    setInlineBusy("");
  }, [conversationID]);

  const loadTasks = useCallback(async () => {
    const requestVersion = ++tasksRequestVersion.current;
    const next = await request<Task[]>(`/api/projects/${projectID}/tasks`);
    if (mountedRef.current && requestVersion === tasksRequestVersion.current) setTasks(next);
  }, [projectID, request]);

  useEffect(() => { void loadTasks().catch((cause) => { if (mountedRef.current) fail(cause instanceof Error ? cause.message : "无法加载任务队列"); }); }, [fail, loadTasks]);
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
    if (dispatchDisabled) return;
    const requestID = ++confirmationRequest.current;
    setConfirmingTaskID(taskID);
    setDetail(null);
    setLoadingDetail(true);
    try {
      const next = await request<TaskDetail>(`/api/tasks/${taskID}`);
      if (confirmationRequest.current === requestID && mountedRef.current) setDetail(next);
    } catch (cause) {
      if (confirmationRequest.current === requestID && mountedRef.current) {
        fail(cause instanceof Error ? cause.message : "无法检查任务下发条件");
        setConfirmingTaskID(null);
      }
    } finally {
      if (confirmationRequest.current === requestID && mountedRef.current) setLoadingDetail(false);
    }
  };

  const openInlineDetail = async (taskID: string, edit = false) => {
    const requestID = ++inlineRequest.current;
    setInlineDetailID(taskID);
    setInlineEditRequestedID(edit ? taskID : null);
    setInlineDetail(null);
    setInlineDetailLoading(true);
    setInlineBusy("");
    // 内联详情已含完整的验收操作：打开详情时收起行内验收面板，避免两套验收 UI 并存。
    if (reviewingTaskID === taskID) { setReviewingTaskID(null); setReviewNote(""); }
    try {
      const next = await request<TaskDetail>(`/api/tasks/${taskID}`);
      if (inlineRequest.current === requestID && mountedRef.current) setInlineDetail(next);
    } catch (cause) {
      if (inlineRequest.current === requestID && mountedRef.current) fail(cause instanceof Error ? cause.message : "无法加载任务详情");
    } finally {
      if (inlineRequest.current === requestID && mountedRef.current) setInlineDetailLoading(false);
    }
  };

  const closeInlineDetail = () => {
    inlineRequest.current++;
    setInlineDetailID(null);
    setInlineEditRequestedID(null);
    setInlineDetail(null);
    setInlineDetailLoading(false);
    setInlineBusy("");
  };

  const inlineDispatch = async () => {
    if (dispatchDisabled || !conversationID || !inlineDetail || !inlineDetail.canDispatch) return;
    if (!mountedRef.current) return;
    const requestID = inlineRequest.current;
    const dispatchConversationID = conversationID;
    setInlineBusy("dispatch");
    try {
      const result = await request<DispatchResult>(`/api/tasks/${inlineDetail.id}/dispatch`, { method: "POST", body: JSON.stringify({ conversationId: dispatchConversationID }) });
      if (inlineRequest.current !== requestID || conversationIDRef.current !== dispatchConversationID || !mountedRef.current) return;
      onDispatched(result.message, result.runId);
      closeInlineDetail();
      await loadTasks();
    } catch (cause) {
      if (inlineRequest.current !== requestID || conversationIDRef.current !== dispatchConversationID || !mountedRef.current) return;
      fail(cause instanceof Error ? cause.message : "无法下发任务");
      void loadTasks().catch(() => undefined);
    } finally {
      if (inlineRequest.current === requestID && mountedRef.current) setInlineBusy("");
    }
  };

  const inlineTransition = async (action: "reopen" | "stop") => {
    if (!inlineDetail) return;
    if (!mountedRef.current) return;
    const taskID = inlineDetail.id;
    const requestID = ++inlineRequest.current;
    setInlineBusy(action);
    try {
      await request(`/api/tasks/${taskID}/${action}`, { method: "POST", body: "{}" });
      if (!mountedRef.current || inlineRequest.current !== requestID) return;
      const next = await request<TaskDetail>(`/api/tasks/${taskID}`);
      if (!mountedRef.current || inlineRequest.current !== requestID) return;
      setInlineDetail(next);
      await loadTasks();
    } catch (cause) { if (mountedRef.current && inlineRequest.current === requestID) fail(cause instanceof Error ? cause.message : "无法更新任务"); }
    finally { if (mountedRef.current && inlineRequest.current === requestID) setInlineBusy(""); }
  };

  const inlineDelete = () => {
    if (!inlineDetail) return;
    setPendingConfirm({
      title: "删除任务",
      message: <>确认删除任务「<b>{taskDisplayTitle(inlineDetail)}</b>」？此操作不可撤销，所有执行记录将被永久删除。</>,
      danger: true,
      onConfirm: () => void (async () => {
        if (!mountedRef.current) return;
        const taskID = inlineDetail.id;
        const requestID = ++inlineRequest.current;
        setPendingConfirm(null);
        setInlineBusy("delete");
        try {
          await request(`/api/tasks/${taskID}`, { method: "DELETE" });
          if (!mountedRef.current || inlineRequest.current !== requestID) return;
          closeInlineDetail();
          await loadTasks();
        } catch (cause) { if (mountedRef.current && inlineRequest.current === requestID) fail(cause instanceof Error ? cause.message : "无法删除任务"); }
        finally { if (mountedRef.current && inlineRequest.current === requestID) setInlineBusy(""); }
      })(),
      onCancel: () => { if (mountedRef.current) setPendingConfirm(null); },
    });
  };

  // 任务列表行的快速删除：不依赖打开内联详情，直接用列表里的任务快照弹确认框。
  const deleteTask = (task: Task) => {
    if (!mountedRef.current) return;
    setPendingConfirm({
      title: "删除任务",
      message: <>确认删除任务「<b>{taskDisplayTitle(task)}</b>」？此操作不可撤销，所有执行记录将被永久删除。</>,
      danger: true,
      onConfirm: () => void (async () => {
        if (!mountedRef.current) return;
        const taskID = task.id;
        setPendingConfirm(null);
        try {
          await request(`/api/tasks/${taskID}`, { method: "DELETE" });
          if (!mountedRef.current) return;
          // 行删除可能发生在内联详情或内联验收打开时：一并关闭，避免残留指向已删任务的界面。
          if (inlineDetailID === taskID) closeInlineDetail();
          if (reviewingTaskID === taskID) { setReviewingTaskID(null); setReviewNote(""); }
          await loadTasks();
        } catch (cause) { if (mountedRef.current) fail(cause instanceof Error ? cause.message : "无法删除任务"); }
      })(),
      onCancel: () => { if (mountedRef.current) setPendingConfirm(null); },
    });
  };

  const inlineEdit = async (patch: { title: string; description: string; priority: Priority }): Promise<{ ok: boolean; message?: string }> => {
    if (!inlineDetail) return { ok: false };
    if (!mountedRef.current) return { ok: false };
    const taskID = inlineDetail.id;
    // 不传 position：后端 Position 为指针，nil 时保留原值，避免用可能过期的 inlineDetail 快照
    // 覆盖拖拽后的最新排序位（详情面板打开期间任务仍可被拖拽重排）。
    const requestID = ++inlineRequest.current;
    setInlineBusy("edit");
    try {
      await request(`/api/tasks/${taskID}`, { method: "PATCH", body: JSON.stringify(patch) });
      if (!mountedRef.current || inlineRequest.current !== requestID) return { ok: false };
      const next = await request<TaskDetail>(`/api/tasks/${taskID}`);
      if (!mountedRef.current || inlineRequest.current !== requestID) return { ok: false };
      setInlineDetail(next);
      // 列表刷新失败不影响保存结果：PATCH 已成功，队列留给下一次轮询（10s）收敛即可，
      // 否则会把一次成功保存误报成"保存失败"。
      void loadTasks().catch(() => undefined);
      return { ok: true };
    } catch (cause) {
      // 保存失败不弹全局错误横幅（远离编辑表单，容易被忽略/遮挡队列头部），
      // 而是把错误消息原样返回，由内联编辑表单紧贴保存按钮展示。
      if (mountedRef.current && inlineRequest.current === requestID) return { ok: false, message: cause instanceof Error ? cause.message : "无法保存任务" };
      return { ok: false };
    } finally { if (mountedRef.current && inlineRequest.current === requestID) setInlineBusy(""); }
  };

  const inlineReview = async (action: "accept" | "request_changes", note: string) => {
    if (!inlineDetail) return;
    if (action === "request_changes" && !note.trim()) { fail("请填写需要修改的原因"); return; }
    const taskID = inlineDetail.id;
    const requestID = ++inlineRequest.current;
    try {
      await request(`/api/tasks/${taskID}/review`, { method: "POST", body: JSON.stringify({ action, note: action === "accept" ? "" : note }) });
      if (!mountedRef.current || inlineRequest.current !== requestID) return;
      const next = await request<TaskDetail>(`/api/tasks/${taskID}`);
      if (!mountedRef.current || inlineRequest.current !== requestID) return;
      setInlineDetail(next);
      await loadTasks();
    } catch (cause) { if (mountedRef.current && inlineRequest.current === requestID) fail(cause instanceof Error ? cause.message : "无法提交验收"); }
  };

  const dispatch = async () => {
    if (dispatchDisabled || !conversationID || !detail || detail.id !== confirmingTaskID || !detail.canDispatch) return;
    if (!mountedRef.current) return;
    const dispatchConversationID = conversationID;
    const requestID = ++confirmationRequest.current;
    setDispatching(true);
    try {
      const result = await request<DispatchResult>(`/api/tasks/${detail.id}/dispatch`, { method: "POST", body: JSON.stringify({ conversationId: dispatchConversationID }) });
      if (confirmationRequest.current !== requestID || conversationIDRef.current !== dispatchConversationID || !mountedRef.current) return;
      onDispatched(result.message, result.runId);
      setConfirmingTaskID(null);
      setDetail(null);
      await loadTasks();
    } catch (cause) {
      if (confirmationRequest.current === requestID && conversationIDRef.current === dispatchConversationID && mountedRef.current) {
        fail(cause instanceof Error ? cause.message : "无法下发任务");
        void loadTasks().catch(() => undefined);
      }
    } finally {
      if (confirmationRequest.current === requestID && mountedRef.current) setDispatching(false);
    }
  };

  const confirmTransition = () => {
    setPendingConfirm({
      title: "重新打开",
      message: "确认重新打开该任务？",
      onConfirm: () => { if (mountedRef.current) { setPendingConfirm(null); void inlineTransition("reopen"); } },
      onCancel: () => { if (mountedRef.current) setPendingConfirm(null); },
    });
  };

  const submitReview = async (taskID: string, action: "accept" | "request_changes") => {
    if (reviewSubmitting) return;
    if (action === "request_changes" && !reviewNote.trim()) { fail("请填写需要修改的原因"); return; }
    if (!mountedRef.current) return;
    setReviewSubmitting(true);
    try {
      await request(`/api/tasks/${taskID}/review`, { method: "POST", body: JSON.stringify({ action, note: action === "accept" ? "" : reviewNote }) });
      if (!mountedRef.current) return;
      setReviewingTaskID(null);
      setReviewNote("");
      await loadTasks();
    } catch (cause) {
      if (mountedRef.current) fail(cause instanceof Error ? cause.message : "无法提交验收");
    } finally {
      if (mountedRef.current) setReviewSubmitting(false);
    }
  };

  const redispatchTask = async (event: MouseEvent<HTMLButtonElement>, taskID: string) => {
    event.stopPropagation();
    if (dispatchDisabled || !conversationID) return;
    if (!mountedRef.current) return;
    const dispatchConversationID = conversationID;
    const requestID = ++redispatchRequest.current;
    setRedispatchingTaskID(taskID);
    try {
      const result = await request<DispatchResult>(`/api/tasks/${taskID}/dispatch`, { method: "POST", body: JSON.stringify({ conversationId: dispatchConversationID }) });
      if (redispatchRequest.current !== requestID || conversationIDRef.current !== dispatchConversationID || !mountedRef.current) return;
      onDispatched(result.message, result.runId);
      // 重新下发后任务进入执行态：关闭可能指向待验收快照的内联详情/验收面板，
      // 避免残留按钮对已运行任务发出 409。
      if (inlineDetailID === taskID) closeInlineDetail();
      if (reviewingTaskID === taskID) { setReviewingTaskID(null); setReviewNote(""); }
      await loadTasks();
    } catch (cause) {
      if (redispatchRequest.current === requestID && conversationIDRef.current === dispatchConversationID && mountedRef.current) fail(cause instanceof Error ? cause.message : "无法重新下发任务");
    } finally {
      if (redispatchRequest.current === requestID && mountedRef.current) setRedispatchingTaskID(null);
    }
  };

  const handleQueueDrop = async (taskID: string, targetIndex: number) => {
    const task = tasks.find((t) => t.id === taskID);
    if (!task) return;
    const currentIndex = queueTasks.findIndex((t) => t.id === taskID);
    if (currentIndex < 0 || currentIndex === targetIndex) return;
    const reordered = [...queueTasks];
    const [moved] = reordered.splice(currentIndex, 1);
    const insertionIndex = currentIndex < targetIndex ? targetIndex - 1 : targetIndex;
    reordered.splice(insertionIndex, 0, moved);
    const prev = reordered[insertionIndex - 1];
    const next = reordered[insertionIndex + 1];
    let newPosition: number;
    if (!prev) newPosition = next ? next.position - 1 : task.position;
    else if (!next) newPosition = prev.position + 1;
    else newPosition = (prev.position + next.position) / 2;
    if (!mountedRef.current) return;
    try {
      await request(`/api/tasks/${task.id}`, { method: "PATCH", body: JSON.stringify({ title: task.title, description: task.description, priority: task.priority, position: newPosition }) });
      if (!mountedRef.current) return;
      await loadTasks();
    } catch (cause) { if (mountedRef.current) fail(cause instanceof Error ? cause.message : "无法调整任务顺序"); }
  };

  const confirmReviewAll = () => {
    const count = taskCounts.awaiting_review;
    if (count === 0 || approvingAll) return;
    setPendingConfirm({
      title: "一键验收",
      confirmLabel: "确认验收",
      className: "review-all-dialog",
      icon: <ReviewAllIcon />,
      message: <>将验收当前项目全部 <b className="review-all-count">{count}</b> 个待验收任务，标记为完成。<span className="review-all-note">正在自动编排验证中的任务会被跳过，不纳入本次验收。</span></>,
      onConfirm: () => void (async () => {
        if (!mountedRef.current) return;
        // 批量验收使用独立请求令牌，避免与单任务确认（confirmationRequest）相互串扰。
        const requestID = ++reviewAllRequest.current;
        setPendingConfirm(null);
        setApprovingAll(true);
        try {
          const result = await request<{ accepted: number; skipped: number; total: number }>(`/api/projects/${projectID}/tasks/review-all`, { method: "POST", body: "{}" });
          if (reviewAllRequest.current !== requestID || !mountedRef.current) return;
          if (result.skipped > 0) toast.success(`已验收 ${result.accepted} 个任务，${result.skipped} 个正在自动编排验证中已跳过`);
          else if (result.total > 0) toast.success(`已验收 ${result.accepted} 个任务`);
        } catch (cause) {
          if (reviewAllRequest.current === requestID && mountedRef.current) fail(cause instanceof Error ? cause.message : "一键验收失败");
        } finally {
          if (reviewAllRequest.current === requestID && mountedRef.current) setApprovingAll(false);
          void loadTasks().catch(() => undefined);
        }
      })(),
      onCancel: () => { if (mountedRef.current) setPendingConfirm(null); },
    });
  };

  const setPinned = async (taskID: string, pinned: boolean) => {
    if (pinLockRef.current || !mountedRef.current) return;
    const task = tasks.find((t) => t.id === taskID);
    if (!task) return;
    pinLockRef.current = true;
    // 置顶只切换 pinned 标记，不改 position：pinned 优先排序会让它稳居第一，
    // 而保留原 position 使取消置顶时自然回到原位，也避免 position 反复减一负向漂移。
    const patch = (t: Task, p: boolean) => JSON.stringify({ title: t.title, description: t.description, priority: t.priority, pinned: p, position: t.position });
    try {
      if (pinned) {
        // 单置顶：点谁谁到第一个，同时取消其它任务原来的置顶。
        await request(`/api/tasks/${task.id}`, { method: "PATCH", body: patch(task, true) });
        const others = tasks.filter((t) => t.pinned && t.id !== taskID);
        for (const other of others) {
          await request(`/api/tasks/${other.id}`, { method: "PATCH", body: patch(other, false) });
        }
      } else {
        await request(`/api/tasks/${task.id}`, { method: "PATCH", body: patch(task, false) });
      }
      if (!mountedRef.current) return;
      await loadTasks();
    } catch (cause) { if (mountedRef.current) fail(cause instanceof Error ? cause.message : "无法置顶任务"); }
    finally { pinLockRef.current = false; }
  };

  const quickCreate = async (event: FormEvent) => {
    event.preventDefault();
    if (!quickDescription.trim() || quickCreating) return;
    if (!mountedRef.current) return;
    setQuickCreating(true);
    try {
      await request<Task>(`/api/projects/${projectID}/tasks`, { method: "POST", body: JSON.stringify({ title: "", description: quickDescription.trim(), priority: "normal", position: 0 }) });
      if (!mountedRef.current) return;
      setQuickDescription("");
      setQuickCreateOpen(false);
      await loadTasks();
    } catch (cause) {
      if (mountedRef.current) fail(cause instanceof Error ? cause.message : "无法创建任务");
    } finally {
      if (mountedRef.current) setQuickCreating(false);
    }
  };

  const closeQuickCreate = () => {
    if (quickCreating) return;
    setQuickCreateOpen(false);
    setQuickDescription("");
  };

  return <div className={`task-queue ${mobileOpen ? "mobile-open" : ""}`}>
    <button type="button" className="task-queue-mobile-toggle" aria-expanded={mobileOpen} onClick={() => setMobileOpen((open) => !open)}>任务 <b>{taskCounts.active}</b></button>
    <div className="task-queue-panel">
      <header className="task-queue-head"><div><TaskQueueIcon /><span>任务队列</span><b>{taskCounts.active}</b></div><div className="task-queue-head-actions">{taskCounts.awaiting_review > 0 && <button type="button" className="task-queue-review-all" disabled={approvingAll} title="一键验收全部待验收任务" aria-label="一键验收全部待验收任务" onClick={confirmReviewAll}>{approvingAll ? "验收中" : "一键验收"}{!approvingAll && <b>{taskCounts.awaiting_review}</b>}</button>}<button type="button" className="task-queue-add" title="快速创建任务" aria-label="快速创建任务" onClick={() => { setQuickCreateOpen(true); setQuickDescription(""); }}>+</button><button type="button" className="task-queue-all" onClick={() => openBoard()}>查看全部</button></div></header>
      {quickCreateOpen && <form className="task-queue-quick-create" onSubmit={(event) => void quickCreate(event)}><textarea autoFocus required maxLength={12000} rows={2} value={quickDescription} disabled={quickCreating} onChange={(event) => setQuickDescription(event.target.value)} onKeyDown={(event) => { if (event.key === "Enter" && !event.ctrlKey && !event.shiftKey) { event.preventDefault(); const form = event.currentTarget.form; if (form) form.requestSubmit(); } }} placeholder="输入任务说明，回车快速创建…" /><div className="task-queue-quick-create-actions"><button type="button" className="secondary" disabled={quickCreating} onClick={closeQuickCreate}>取消</button><button type="submit" className="primary" disabled={!quickDescription.trim() || quickCreating}>{quickCreating ? "创建中" : "创建"}</button></div></form>}
      <nav className="task-queue-filters" aria-label="任务筛选">{filters.map((item) => <button type="button" key={item.id} className={filter === item.id ? "active" : ""} aria-pressed={filter === item.id} onClick={() => setFilter(item.id)}>{item.label}<span>{taskCounts[item.id]}</span></button>)}</nav>
      <div className="task-queue-list">{queueTasks.length === 0 ? <p className="task-queue-empty">当前筛选没有任务。</p> : queueTasks.map((task, index) => <TaskQueueRow key={task.id} task={task} index={index} open={(edit = false) => void openInlineDetail(task.id, edit)} confirm={openConfirmation} redispatch={redispatchTask} redispatching={redispatchingTaskID === task.id} dispatchDisabled={dispatchDisabled} openReview={(taskID) => { setReviewingTaskID(taskID); setReviewNote(""); }} closeReview={() => { setReviewingTaskID(null); setReviewNote(""); }} reviewingTaskID={reviewingTaskID} reviewNote={reviewNote} setReviewNote={setReviewNote} reviewSubmitting={reviewSubmitting} submitReview={submitReview} onDrop={handleQueueDrop}
        inlineDetailID={inlineDetailID}
        inlineDetail={inlineDetail}
        inlineEditRequestedID={inlineEditRequestedID}
        inlineDetailLoading={inlineDetailLoading}
        inlineBusy={inlineBusy}
        closeInlineDetail={closeInlineDetail}
        inlineDispatch={inlineDispatch}
        inlineTransition={inlineTransition}
        inlineDelete={inlineDelete}
        deleteTask={deleteTask}
        inlineEdit={inlineEdit}
        inlineReview={inlineReview}
        openBoard={openBoard}
        confirmTransition={confirmTransition}
        setPinned={setPinned}
      />)}</div>
    </div>
    {confirmingTask && createPortal(<DispatchConfirmation task={confirmingTask} detail={detail} loading={loadingDetail} dispatching={dispatching} dispatchDisabled={dispatchDisabled} permissionMode={permissionMode} close={closeConfirmation} openBoard={() => openBoard(confirmingTask.id)} dispatch={dispatch} />, document.body)}
    {pendingConfirm && createPortal(<ConfirmDialog title={pendingConfirm.title} message={pendingConfirm.message} confirmLabel={pendingConfirm.confirmLabel} danger={pendingConfirm.danger} className={pendingConfirm.className} icon={pendingConfirm.icon} onConfirm={pendingConfirm.onConfirm} onCancel={pendingConfirm.onCancel} />, document.body)}
  </div>;
}

function TaskQueueRow({ task, index, open, confirm, redispatch, redispatching, dispatchDisabled, openReview, closeReview, reviewingTaskID, reviewNote, setReviewNote, reviewSubmitting, submitReview, onDrop, inlineDetailID, inlineDetail, inlineEditRequestedID, inlineDetailLoading, inlineBusy, closeInlineDetail, inlineDispatch, inlineTransition, inlineDelete, deleteTask, inlineEdit, inlineReview, openBoard, confirmTransition, setPinned }: { task: Task; index: number; open: (edit?: boolean) => void; confirm: (event: MouseEvent<HTMLButtonElement>, taskID: string) => Promise<void>; redispatch: (event: MouseEvent<HTMLButtonElement>, taskID: string) => Promise<void>; redispatching: boolean; dispatchDisabled: boolean; openReview: (taskID: string) => void; closeReview: () => void; reviewingTaskID: string | null; reviewNote: string; setReviewNote: (value: string) => void; reviewSubmitting: boolean; submitReview: (taskID: string, action: "accept" | "request_changes") => Promise<void>; onDrop: (taskID: string, targetIndex: number) => Promise<void>; inlineDetailID: string | null; inlineDetail: TaskDetail | null; inlineEditRequestedID: string | null; inlineDetailLoading: boolean; inlineBusy: string; closeInlineDetail: () => void; inlineDispatch: () => Promise<void>; inlineTransition: (action: "reopen" | "stop") => Promise<void>; inlineDelete: () => void; deleteTask: (task: Task) => void; inlineEdit: (patch: { title: string; description: string; priority: Priority }) => Promise<{ ok: boolean; message?: string }>; inlineReview: (action: "accept" | "request_changes", note: string) => Promise<void>; openBoard: (taskID?: string) => void; confirmTransition: () => void; setPinned: (taskID: string, pinned: boolean) => Promise<void> }) {
  const [isDragging, setIsDragging] = useState(false);
  const [dragOverPosition, setDragOverPosition] = useState<"before" | "after" | null>(null);
  const [hoverOpen, setHoverOpen] = useState(false);
  const [pinning, setPinning] = useState(false);
  const rowRef = useRef<HTMLElement | null>(null);
  const hoverTimer = useRef<number | undefined>(undefined);
  const canHover = useMemo(() => typeof window !== "undefined" && window.matchMedia("(hover: hover) and (pointer: fine)").matches, []);
  const didDragRef = useRef(false);
  const mouseDownPosRef = useRef<{ x: number; y: number } | null>(null);
  const queued = task.status === "running" && task.lastRun?.status === "queued";
  const status = queued ? "队列中" : statusLabels[task.status];
  const note = taskQueueNote(task);
  const isReviewing = reviewingTaskID === task.id;
  const reviewRef = useRef<HTMLDivElement>(null);

  const handlePinToTop = async (event: MouseEvent<HTMLButtonElement>) => {
    event.stopPropagation();
    if (pinning) return;
    setPinning(true);
    try { await setPinned(task.id, !task.pinned); } finally { setPinning(false); }
  };
  useEffect(() => {
    if (!isReviewing || !reviewRef.current) return;
    reviewRef.current.scrollIntoView({ behavior: "smooth", block: "center" });
  }, [isReviewing]);

  // 当行进入内联详情或验收态时，关闭悬浮预览，避免重叠。
  useEffect(() => {
    if (inlineDetailID === task.id || isReviewing) setHoverOpen(false);
  }, [inlineDetailID, isReviewing, task.id]);

  // 卸载时清理悬浮计时器。
  useEffect(() => () => { if (hoverTimer.current) window.clearTimeout(hoverTimer.current); }, []);

  // 任务切换（列表刷新后行被复用到新任务）时关闭悬浮预览。
  useEffect(() => { setHoverOpen(false); }, [task.id]);

  const showHover = () => {
    if (hoverTimer.current) window.clearTimeout(hoverTimer.current);
    if (!canHover || isDragging || inlineDetailID === task.id || isReviewing) return;
    hoverTimer.current = window.setTimeout(() => setHoverOpen(true), 280);
  };
  const hideHover = () => {
    if (hoverTimer.current) window.clearTimeout(hoverTimer.current);
    // 延迟关闭，给鼠标从行移动到悬浮卡片留出衔接时间。
    hoverTimer.current = window.setTimeout(() => setHoverOpen(false), 120);
  };

  const handleMouseDown = (e: React.MouseEvent) => {
    mouseDownPosRef.current = { x: e.clientX, y: e.clientY };
  };
  const handleDragStart = (e: React.DragEvent) => {
    e.stopPropagation();
    if (hoverTimer.current) window.clearTimeout(hoverTimer.current);
    setHoverOpen(false);
    setIsDragging(true);
    didDragRef.current = true;
    const data: QueueDragData = { taskID: task.id, sourceIndex: index };
    queueDragPayloadRef.current = data;
    e.dataTransfer.setData("application/x-queue-drag", JSON.stringify(data));
    e.dataTransfer.effectAllowed = "move";
  };
  const handleDragEnd = () => {
    setIsDragging(false);
    setDragOverPosition(null);
    queueDragPayloadRef.current = null;
    setTimeout(() => { didDragRef.current = false; }, 200);
  };
  const handleClick = (e: React.MouseEvent, edit = false) => {
    // Suppress click if a real drag just happened (dragstart fired)
    if (didDragRef.current) { e.preventDefault(); e.stopPropagation(); return; }
    // 键盘激活（Tab+Enter/Space）时 e.detail 为 0，且没有 mousedown 可对照：
    // 直接放行，否则会用上一次鼠标点击留下的 mouseDownPosRef 算出一个大位移，
    // 把键盘打开行/编辑误判为拖拽而吞掉。
    if (e.detail === 0) { open(edit); return; }
    // Also suppress if the mouse moved more than a few pixels — the user
    // intended to drag even if the browser didn't fire dragstart.
    const down = mouseDownPosRef.current;
    if (down) {
      const dx = e.clientX - down.x;
      const dy = e.clientY - down.y;
      if (Math.sqrt(dx * dx + dy * dy) > 4) { e.preventDefault(); e.stopPropagation(); return; }
    }
    open(edit);
  };
  const handleDragOverRow = (e: React.DragEvent) => {
    e.preventDefault(); e.stopPropagation();
    e.dataTransfer.dropEffect = "move";
    const rect = e.currentTarget.getBoundingClientRect();
    setDragOverPosition(e.clientY < rect.top + rect.height / 2 ? "before" : "after");
  };
  const handleDropRow = (e: React.DragEvent) => {
    e.preventDefault(); e.stopPropagation();
    setDragOverPosition(null);
    // 优先读内存 payload（WebView2 可靠通道）；仅在缺失时回退 dataTransfer（浏览器通道）。
    let dragData: QueueDragData | null = queueDragPayloadRef.current;
    if (!dragData) {
      const data = e.dataTransfer.getData("application/x-queue-drag");
      if (!data) return;
      dragData = JSON.parse(data) as QueueDragData;
    }
    queueDragPayloadRef.current = null;
    if (dragData.taskID === task.id) return;
    const rect = e.currentTarget.getBoundingClientRect();
    void onDrop(dragData.taskID, e.clientY < rect.top + rect.height / 2 ? index : index + 1);
  };

  return <article ref={rowRef} className={`task-queue-row${isDragging ? " dragging" : ""}${dragOverPosition ? ` drag-over-${dragOverPosition}` : ""}`} draggable onDragStart={handleDragStart} onDragEnd={handleDragEnd} onDragOver={handleDragOverRow} onDrop={handleDropRow} onMouseDown={handleMouseDown} onMouseEnter={showHover} onMouseLeave={hideHover} onClick={(e) => { if (didDragRef.current) { e.preventDefault(); e.stopPropagation(); } }}>
    <button type="button" className="task-queue-open" onClick={handleClick}>
      <span className={`task-status ${taskDisplayStatusClass(task)}`}>{taskDisplayStatus(task)}</span>
      <b>{taskDisplayTitle(task)}</b>
      {note && <small>{note}</small>}
    </button>
    <div className="task-queue-row-actions">
      {canOfferDispatch(task) && <button type="button" className="task-queue-dispatch" draggable={false} disabled={dispatchDisabled} title="下发任务" aria-label="下发任务" onClick={(event) => void confirm(event, task.id)}><svg viewBox="0 0 24 24" aria-hidden="true"><path d="m21 3-7.6 18-3.3-7.1L3 10.6 21 3Z" /><path d="m10.1 13.9 4.3-4.3" /></svg></button>}
      {canRedispatch(task) && !isReviewing && <button type="button" className={`task-queue-redispatch${task.status === "awaiting_review" ? " awaiting-review" : ""}`} draggable={false} disabled={dispatchDisabled || redispatching} title={redispatching ? "正在重新下发" : "重新下发任务"} aria-label={redispatching ? "正在重新下发" : "重新下发任务"} onClick={(event) => void redispatch(event, task.id)}><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M19 8V4l-2.8 2.8A7.5 7.5 0 1 0 19 13" /><path d="M19 4v4h-4" /></svg></button>}
      {task.status === "awaiting_review" && !isTaskOrchestrating(task) && !isTaskAwaitingMainMerge(task) && !isReviewing && inlineDetailID !== task.id && <button type="button" className="task-queue-review" draggable={false} title="验收" aria-label="验收" onClick={(event) => { event.stopPropagation(); openReview(task.id); }}><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4.5 12.5 9.8 18 19.5 6.5" /></svg></button>}
      <button type="button" className="task-queue-pin" draggable={false} disabled={pinning} title={task.pinned ? "取消置顶" : "置顶"} aria-label={task.pinned ? "取消置顶" : "置顶"} onClick={(event) => void handlePinToTop(event)}>
        <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M14.4 6L14 4H7.7L8 6 5 9.3c-.3.3-.5.7-.5 1.1 0 .4.3.6.6.6h4.2l-1.2 6.2c-.1.4.2.8.6.8.2 0 .4-.1.5-.2L15 12l3.9.1c.4 0 .6-.3.6-.6 0-.4-.2-.8-.5-1.1L14.4 6z" /></svg>
      </button>
      {(task.status === "todo" || task.status === "action_required") && <button type="button" className="task-queue-edit secondary" draggable={false} title="编辑任务" aria-label="编辑任务" onClick={(event) => { event.stopPropagation(); handleClick(event, true); }}><svg viewBox="0 0 24 24" aria-hidden="true"><path d="m5 16.5-.8 3.3 3.3-.8L18 8.5 15.5 6 5 16.5Z" /><path d="m14.5 7 2.5 2.5" /></svg></button>}
      <button type="button" className="task-queue-delete" draggable={false} title="删除任务" aria-label="删除任务" onClick={(event) => { event.stopPropagation(); deleteTask(task); }}><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4.5 6.8h15" /><path d="M9.8 6.8V5.4A1.4 1.4 0 0 1 11.2 4h1.6a1.4 1.4 0 0 1 1.4 1.4v1.4" /><path d="M6.6 6.8l.7 12a1.4 1.4 0 0 0 1.4 1.3h6.6a1.4 1.4 0 0 0 1.4-1.3l.7-12" /><path d="M10 10.6v5.4M14 10.6v5.4" /></svg></button>
    </div>
    {isReviewing && <div className="task-queue-inline-review" ref={reviewRef}>
      <textarea autoFocus required disabled={reviewSubmitting} value={reviewNote} onChange={(event) => setReviewNote(event.target.value)} placeholder="说明需要补充或修改的内容，留空即确认完成…" onDragStart={(e) => e.stopPropagation()} />
      <div className="task-queue-inline-review-actions">
        <button type="button" className="secondary" draggable={false} disabled={reviewSubmitting} onClick={(event) => { event.stopPropagation(); submitReview(task.id, "accept"); }}>确认完成</button>
        <button type="button" className="primary" draggable={false} disabled={!reviewNote.trim() || reviewSubmitting} onClick={(event) => { event.stopPropagation(); submitReview(task.id, "request_changes"); }}>{reviewSubmitting ? "提交中" : "要求修改"}</button>
        <button type="button" className="danger-text" draggable={false} disabled={reviewSubmitting} onClick={(event) => { event.stopPropagation(); closeReview(); }}>取消</button>
      </div>
    </div>}
    {inlineDetailID === task.id && <InlineTaskDetail detail={inlineDetail} loading={inlineDetailLoading} busy={inlineBusy} initialEditing={inlineEditRequestedID === task.id} dispatchDisabled={dispatchDisabled} close={closeInlineDetail} dispatch={inlineDispatch} transition={inlineTransition} deleteTask={inlineDelete} edit={inlineEdit} review={inlineReview} openBoard={() => openBoard(inlineDetailID)} confirmTransition={confirmTransition} />}
    {hoverOpen && rowRef.current && createPortal(<TaskHoverCard task={task} anchor={rowRef.current} onEnter={showHover} onLeave={hideHover} />, document.body)}
  </article>;
}

function TaskHoverCard({ task, anchor, onEnter, onLeave }: { task: Task; anchor: HTMLElement; onEnter: () => void; onLeave: () => void }) {
  const [pos, setPos] = useState<{ left: number; top: number } | null>(null);
  const margin = 10;

  useLayoutEffect(() => {
    const place = () => {
      const rect = anchor.getBoundingClientRect();
      const cardW = 340;
      const cardMaxH = 420;
      const vw = window.innerWidth;
      const vh = window.innerHeight;
      let left: number;
      // 优先放在行的右侧；右侧空间不足时放左侧。
      if (rect.right + margin + cardW <= vw) left = rect.right + margin;
      else if (rect.left - margin - cardW >= 0) left = rect.left - margin - cardW;
      else left = Math.max(margin, Math.min(rect.right + margin, vw - margin - cardW));
      let top: number;
      // 尽量贴着行顶部；若下方放不下则上移，保证卡片不超出视口上下边界。
      if (rect.top + cardMaxH <= vh) {
        top = rect.top;
      } else {
        top = Math.max(margin, Math.min(rect.top, vh - margin - cardMaxH));
      }
      setPos({ left, top });
    };
    place();
    window.addEventListener("resize", place);
    window.addEventListener("scroll", place, true);
    return () => { window.removeEventListener("resize", place); window.removeEventListener("scroll", place, true); };
  }, [anchor]);

  const note = taskQueueNote(task);
  const queued = task.status === "running" && task.lastRun?.status === "queued";
  const hasContent = Boolean(task.description || task.blockedBy.length > 0 || task.lastRun);

  return <div className="task-hover-card" role="tooltip" style={pos ? { left: pos.left, top: pos.top, maxHeight: 420, opacity: 1 } : { opacity: 0 }} onMouseEnter={onEnter} onMouseLeave={onLeave}>
    <div className="task-hover-card-head">
      <span className={`task-status ${taskDisplayStatusClass(task)}`}>{queued ? "队列中" : taskDisplayStatus(task)}</span>
      <b>{taskDisplayTitle(task)}</b>
      <span className={`task-hover-priority priority-${task.priority}`}>{priorityLabels[task.priority]}优先级</span>
    </div>
    {note && <p className="task-hover-note">{note}</p>}
    {hasContent ? <>
      {task.description && <div className="task-hover-section"><b>任务说明</b><p>{task.description}</p></div>}
      {task.blockedBy.length > 0 && <div className="task-hover-section"><b>依赖阻塞</b><p>{task.blockedBy.map((item) => item.title).join("、")}</p></div>}
      {task.lastRun && <div className="task-hover-section"><b>最近执行</b><p>第 {task.lastRun.sequence} 次 · {taskRunStatusLabel(task.lastRun.status)}{task.lastRun.failureReason ? `：${task.lastRun.failureReason}` : ""}</p></div>}
    </> : <p className="task-hover-empty">该任务暂无详细说明。</p>}
    <p className="task-hover-hint">点击查看完整详情与操作</p>
  </div>;
}

function DispatchConfirmation({ task, detail, loading, dispatching, dispatchDisabled, permissionMode, close, openBoard, dispatch }: { task: Task; detail: TaskDetail | null; loading: boolean; dispatching: boolean; dispatchDisabled: boolean; permissionMode?: ExecutionPolicy; close: () => void; openBoard: () => void; dispatch: () => Promise<void> }) {
  const canDispatch = Boolean(detail?.canDispatch) && !loading && !dispatchDisabled;
  const eligibility = loading ? "正在检查任务状态..." : detail?.canDispatch ? "任务满足下发条件。" : detail?.blockReason || "当前任务不能下发。";
  const description = detail?.description || task.description;
  return <div className="backdrop task-dispatch-backdrop" role="dialog" aria-modal="true" aria-labelledby="task-dispatch-title">
    <section className="modal task-dispatch-dialog">
      <header>
        <div className="task-dispatch-heading">
          <span className="task-dispatch-heading-icon" aria-hidden="true"><DispatchIcon /></span>
          <div>
            <h2 id="task-dispatch-title">确认下发</h2>
            <p>请核对任务信息，确认后将进入执行环境。</p>
          </div>
        </div>
        <button type="button" className="task-dispatch-close" title="关闭" aria-label="关闭" disabled={dispatching} onClick={close}><DispatchCloseIcon /></button>
      </header>
      <div className="task-dispatch-body">
        <div className="task-dispatch-title">
          <div className="task-dispatch-tags">
            <span className={`task-status ${task.status}`}>{statusLabels[task.status]}</span>
            <em className={`task-dispatch-priority priority-${task.priority}`}>{priorityLabels[task.priority]}优先级</em>
          </div>
          <h3>{taskDisplayTitle(task)}</h3>
          {description && <p className="task-dispatch-description">{description}</p>}
        </div>
        <dl>
          <div><dt>当前权限</dt><dd>{policyLabel(permissionMode)}</dd></div>
          <div><dt>状态检查</dt><dd className={canDispatch ? "eligible" : loading ? "checking" : "blocked"}>{eligibility}</dd></div>
        </dl>
      </div>
      <footer>
        <button type="button" className="secondary" disabled={dispatching} onClick={openBoard}>查看完整详情</button>
        <span>
          <button type="button" className="secondary" disabled={dispatching} onClick={close}>取消</button>
          <button type="button" className="primary" disabled={!canDispatch || dispatching} onClick={() => void dispatch()}>{dispatching ? "下发中" : "确认下发"}</button>
        </span>
      </footer>
    </section>
  </div>;
}

function InlineTaskDetail({ detail, loading, busy, initialEditing = false, dispatchDisabled, close, dispatch, transition, deleteTask, edit, review, openBoard, confirmTransition }: { detail: TaskDetail | null; loading: boolean; busy: string; initialEditing?: boolean; dispatchDisabled: boolean; close: () => void; dispatch: () => Promise<void>; transition: (action: "reopen" | "stop") => Promise<void>; deleteTask: () => void; edit: (patch: { title: string; description: string; priority: Priority }) => Promise<{ ok: boolean; message?: string }>; review: (action: "accept" | "request_changes", note: string) => Promise<void>; openBoard: () => void; confirmTransition: () => void }) {
  const detailRef = useRef<HTMLDivElement>(null);
  const editActionsRef = useRef<HTMLDivElement>(null);
  const [localReviewNote, setLocalReviewNote] = useState("");
  const [localReviewSubmitting, setLocalReviewSubmitting] = useState(false);
  // 内联编辑态：进入时用当前 detail 快照初始化，保存成功后由父组件刷新 detail 并退出编辑。
  const [editing, setEditing] = useState(initialEditing);
  const [editTitle, setEditTitle] = useState("");
  const [editDescription, setEditDescription] = useState("");
  const [editPriority, setEditPriority] = useState<Priority>("normal");
  // 保存失败的错误消息：紧贴保存按钮展示，避免只出现在页面右上角的全局错误横幅
  // （离编辑表单远、且会盖住任务队列头部，用户容易错过导致以为"无法保存"）。
  const [saveError, setSaveError] = useState("");
  const mountedRef = useRef(true);
  useEffect(() => {
    mountedRef.current = true;
    return () => { mountedRef.current = false; };
  }, []);

  useEffect(() => { if (detailRef.current && detail) detailRef.current.scrollIntoView({ behavior: "smooth", block: "center" }); }, [detail]);

  // 切换到其它任务（detail.id 变化）时退出编辑态，避免把上一任务的草稿带到新任务。
  useEffect(() => {
    if (!detail) {
      setEditing(false);
      return;
    }
    if (!initialEditing || (detail.status !== "todo" && detail.status !== "action_required")) {
      setEditing(false);
      return;
    }
    setEditTitle(detail.title);
    setEditDescription(detail.description);
    setEditPriority(detail.priority);
    setSaveError("");
    setEditing(true);
  }, [detail?.id, initialEditing]);

  // 快速编辑进入时，编辑表单可能比任务队列可见区域更高：若底部操作（保存/取消）被
  // 滚动容器截断，把它们滚入视野，避免看不到保存按钮而无法保存。仅在实际越出可视
  // 区域时滚动，高度足够的屏幕上不做无谓跳动。saveError 出现会让表单变高、把操作栏
  // 往下推，需要重新滚动一次，否则保存按钮会被新出现的报错顶出可视区。
  useEffect(() => {
    if (!editing || !detail || !editActionsRef.current) return;
    const actions = editActionsRef.current;
    let container = actions.parentElement;
    while (container) {
      const style = window.getComputedStyle(container);
      const scrollable = (style.overflowY === "auto" || style.overflowY === "scroll") && container.scrollHeight > container.clientHeight;
      if (scrollable) {
        const containerRect = container.getBoundingClientRect();
        const actionsRect = actions.getBoundingClientRect();
        if (actionsRect.bottom > containerRect.bottom + 1) {
          actions.scrollIntoView({ behavior: "smooth", block: "end" });
        } else if (actionsRect.top < containerRect.top - 1) {
          actions.scrollIntoView({ behavior: "smooth", block: "start" });
        }
        break;
      }
      container = container.parentElement;
    }
  }, [editing, detail, saveError]);

  if (loading) return <div className="task-inline-detail"><div className="task-inline-detail-body"><p className="task-inline-loading">加载任务详情...</p></div></div>;
  if (!detail) return <div className="task-inline-detail"><div className="task-inline-detail-body"><p className="task-inline-loading">无法加载任务详情。</p></div></div>;

  const queued = detail.status === "running" && detail.lastRun?.status === "queued";
  const reviewBusy = Boolean(busy) || localReviewSubmitting;
  const editBusy = busy === "edit";
  // 仅待处理/需处理状态可编辑：后端禁止在 running/awaiting_review 下改执行内容
  // （会与进行中或待验收的 run 冲突，返回 409），与任务页详情弹窗的编辑入口一致。
  const canEdit = detail.status === "todo" || detail.status === "action_required";

  const startEdit = () => {
    setEditTitle(detail.title);
    setEditDescription(detail.description);
    setEditPriority(detail.priority);
    setSaveError("");
    setEditing(true);
  };
  const cancelEdit = () => { if (!editBusy) setEditing(false); };
  const saveEdit = async () => {
    if (editBusy || !mountedRef.current) return;
    if (!editDescription.trim()) return;
    const result = await edit({ title: editTitle.trim(), description: editDescription.trim(), priority: editPriority });
    // 保存失败时在表单内保留错误信息并保留用户输入，便于查看原因后重试。
    if (!result.ok) {
      if (result.message && mountedRef.current) setSaveError(result.message);
      return;
    }
    // 仅在保存成功（父组件已刷新 detail）后退出编辑态。
    if (mountedRef.current) setEditing(false);
  };

  const doReview = async (action: "accept" | "request_changes") => {
    if (localReviewSubmitting) return;
    if (action === "request_changes" && !localReviewNote.trim()) return;
    if (!mountedRef.current) return;
    setLocalReviewSubmitting(true);
    try { await review(action, localReviewNote); if (mountedRef.current) setLocalReviewNote(""); }
    finally { if (mountedRef.current) setLocalReviewSubmitting(false); }
  };

  return <div className="task-inline-detail" ref={detailRef} onDragStart={(e) => e.stopPropagation()}>
    <div className="task-inline-detail-head">
      <div className="task-inline-detail-title">
        <span className={`task-status ${queued ? "action_required" : detail.status}`}>{queued ? "队列中" : statusLabels[detail.status]}</span>
        <b>{taskDisplayTitle(detail)}</b>
        <span className={`task-inline-priority priority-${detail.priority}`}>{priorityLabels[detail.priority]}优先级</span>
      </div>
      <button type="button" className="task-inline-close" title="关闭详情" disabled={reviewBusy || editBusy} onClick={close}>x</button>
    </div>
    {editing ? <form className="task-inline-edit-form" onSubmit={(event) => { event.preventDefault(); void saveEdit(); }}>
      <label className="task-inline-edit-field"><span>任务名称 <small>可选</small></span><input maxLength={120} value={editTitle} disabled={editBusy} onChange={(event) => setEditTitle(event.target.value)} placeholder="例如：实现项目任务看板" /></label>
      <label className="task-inline-edit-field"><span>任务说明</span><textarea required maxLength={12000} value={editDescription} disabled={editBusy} onChange={(event) => setEditDescription(event.target.value)} placeholder="说明背景、范围、限制和需要完成的实现。" /></label>
      <label className="task-inline-edit-field task-inline-edit-priority"><span>优先级</span><select value={editPriority} disabled={editBusy} onChange={(event) => setEditPriority(event.target.value as Priority)}>{Object.entries(priorityLabels).map(([value, label]) => <option key={value} value={value}>{label}</option>)}</select></label>
      {saveError && <p className="task-inline-edit-error" role="alert">{saveError}</p>}
      <div className="task-inline-edit-actions" ref={editActionsRef}>
        <button type="button" className="secondary" disabled={editBusy} onClick={cancelEdit}>取消</button>
        <button type="submit" className="primary" disabled={editBusy || !editDescription.trim()}>{editBusy ? "保存中" : "保存"}</button>
      </div>
    </form> : <>
    <div className="task-inline-detail-body">
      {detail.description && <p className="task-inline-description">{detail.description}</p>}
      {detail.runs.length > 0 && <div className="task-inline-section"><b>执行记录</b><div className="task-inline-runs">{detail.runs.map((run) => <span key={run.id} className="task-inline-run"><em>{run.status}</em><small>第 {run.sequence} 次 · {formatDate(run.createdAt)}</small></span>)}</div></div>}
      {detail.blockReason && <p className="task-dispatch-reason">{detail.blockReason}</p>}
    </div>
    <div className="task-inline-detail-actions">
      {(detail.status === "todo" || detail.status === "action_required") && <>
        {detail.canDispatch && <button className="primary" disabled={dispatchDisabled || Boolean(busy)} onClick={() => void dispatch()}>{busy === "dispatch" ? "下发中" : "下发任务"}</button>}
      </>}
      {detail.status === "running" && <button className="danger-text" disabled={Boolean(busy) || queued} onClick={() => void transition("stop")}>{queued ? "队列中" : busy === "stop" ? "停止中" : "停止任务"}</button>}
      {detail.status === "awaiting_review" && !isTaskAwaitingMainMerge(detail) && <>
        <button className="secondary" disabled={reviewBusy} onClick={() => void doReview("accept")}>{localReviewSubmitting ? "提交中" : "确认完成"}</button>
        <div className="task-inline-review-reject">
          <textarea autoFocus required disabled={localReviewSubmitting} value={localReviewNote} onChange={(event) => setLocalReviewNote(event.target.value)} placeholder="说明需要修改的内容…" />
          <button className="primary" disabled={!localReviewNote.trim() || localReviewSubmitting} onClick={() => void doReview("request_changes")}>{localReviewSubmitting ? "提交中" : "要求修改"}</button>
        </div>
      </>}
      {detail.status === "done" && <button className="secondary" disabled={Boolean(busy)} onClick={confirmTransition}>重新打开</button>}
      {canEdit && <button className="secondary" disabled={Boolean(busy)} onClick={() => startEdit()}>编辑</button>}
      <span className="task-inline-actions-sep" />
      <button className="danger-text" disabled={Boolean(busy)} onClick={() => void deleteTask()}>{busy === "delete" ? "删除中" : "删除任务"}</button>
      <button className="secondary" disabled={reviewBusy} onClick={openBoard}>查看完整详情</button>
    </div>
    </>}
  </div>;
}

function formatDate(value: string): string {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? "" : date.toLocaleString("zh-CN", { month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit" });
}
