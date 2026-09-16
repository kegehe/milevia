import { FormEvent, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { useNavigate } from "react-router-dom";
import { anchorForSlot, compareTaskOrder, isSameSlot, isTaskAwaitingMainMerge, isTaskOrchestrating, positionForMove, priorityLabels, Request, Task, TaskDetail, TaskStatus, Priority, VerificationRun, taskDisplayStatus, taskDisplayStatusClass, taskDisplayTitle } from "./task-model";
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
type DropPlacement = { beforeTaskID?: string; afterTaskID?: string };
type PendingConfirm = { title: string; message: React.ReactNode; danger?: boolean; confirmLabel?: string; className?: string; icon?: React.ReactNode; onConfirm: () => void; onCancel: () => void } | null;
type ExecutionPolicy = "approval_required" | "full_control" | "read_only" | "workspace_write";
type OrchestrationConfig = { projectId: string; enabled: boolean; mainBranch: string; devBranch: string; agentId: "claude-code" | "codex"; verificationCommands: string[]; maxFixRounds: number; frozenReason?: string };

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
  const [query, setQuery] = useState("");
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

  const activeTasks = useMemo(() => showHistoricalCancelled ? tasks : tasks.filter((task) => task.status !== "cancelled"), [showHistoricalCancelled, tasks]);
  const visibleTasks = useMemo(() => {
    const term = query.trim().toLowerCase();
    return term ? activeTasks.filter((task) => task.title.toLowerCase().includes(term) || task.description.toLowerCase().includes(term)) : activeTasks;
  }, [activeTasks, query]);
  // 看板的列是写死的六个状态，而后端 `tasks.status` 没有 CHECK 约束（task.go）——
  // 真出现没建模的状态值，这条任务哪一列都不属于，看板会**静默少一条**。
  // 列数/拖拽落点都依赖固定列，改不了；那就至少说出来，别让"看不见"变成"没有"。
  // （「列表」视图有兜底分组，能看见也能清理，所以提示里指向它。）
  const uncoveredStatusCount = useMemo(() => {
    const covered = new Set([...columnDefinitions, historicalCancelledColumn].flatMap((definition) => definition.statuses));
    return tasks.filter((task) => !covered.has(task.status)).length;
  }, [tasks]);
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
  // 勾选态一律按 `every/some` 在可见集合上算，别拿「已选个数」和「可见个数」直接比大小：
  // 选中项可能来自上一次搜索（清理 effect 只删"已不存在的任务"，不删"被过滤掉的"），
  // 两边数量碰巧相等时顶部复选框会显示全选，实际却还有可见任务没被勾上。
  const visibleSelection = useMemo(() => {
    let picked = 0;
    for (const task of visibleTasks) if (selectedIDs.has(task.id)) picked++;
    return { total: visibleTasks.length, all: visibleTasks.length > 0 && picked === visibleTasks.length, partial: picked > 0 && picked < visibleTasks.length };
  }, [selectedIDs, visibleTasks]);
  // 按「分类」（= 一个状态组，看板列与列表分组共用）批量勾选：未全选则补齐，已全选则清空。
  const toggleGroup = (ids: string[]) => {
    if (ids.length === 0) return;
    let picked = 0;
    for (const id of ids) if (selectedIDs.has(id)) picked++;
    const shouldSelect = picked < ids.length;
    setSelectedIDs((prev) => {
      const next = new Set(prev);
      for (const id of ids) { if (shouldSelect) next.add(id); else next.delete(id); }
      return next;
    });
  };
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

  // 下发前先弹确认框：展示将交给 Agent 执行的任务说明与执行权限，
  // 重下发时额外提示已执行次数——每次下发都会启动一次新的 Agent 执行并消耗额度。
  const beginDispatch = () => {
    if (!detail) return;
    if (!mountedRef.current) return;
    const priorRuns = detail.runs.length;
    setPendingConfirm({
      title: priorRuns > 0 ? "重新下发任务" : "下发任务",
      message: (
        <>
          <p className="task-dispatch-title">「{taskDisplayTitle(detail)}」</p>
          <p className="task-dispatch-note">将使用<b>「{policyLabel(permissionMode)}」</b>权限，把任务说明交给 Agent 开始执行。</p>
          {priorRuns > 0 && <p className="task-dispatch-warning">该任务此前已执行 <b>{priorRuns}</b> 次，重新下发将再次启动 Agent 执行并消耗额度。</p>}
        </>
      ),
      className: "task-dispatch-dialog",
      icon: <svg className="task-dispatch-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M5 13l11-1L8 20l3-7-7 3L5 4l14 7.5" /><path d="M19 17v-3M19 21h.01" /></svg>,
      confirmLabel: priorRuns > 0 ? "确认重新下发" : "确认下发",
      onConfirm: () => { if (mountedRef.current) { setPendingConfirm(null); void dispatch(); } },
      onCancel: () => { if (mountedRef.current) setPendingConfirm(null); },
    });
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

  // ↑/↓ 的作用范围 = 用户当前看到的顺序：看板视图里是「同一列内」的先后（同状态任务），
  // 列表视图里是全局 position 顺序。原来一律按全局顺序取邻居，于是在看板里点第 2 张卡的
  // 「上移」，换位对象是同列以外的任务（别的列/别的状态），当前列看上去毫无变化。
  const scopeList = (all: Task[]) => (showHistoricalCancelled ? all : all.filter((task) => task.status !== "cancelled"));
  const moveScope = (task: Task, all: Task[] = activeTasks): Task[] => {
    const definition = view === "board" ? columnDefinitions.find((item) => item.statuses.includes(task.status)) : undefined;
    const scoped = definition ? all.filter((item) => definition.statuses.includes(item.status)) : all;
    return [...scoped].sort(compareTaskOrder);
  };

  const moveTask = async (taskID: string, direction: "up" | "down") => {
    const task = activeTasks.find((item) => item.id === taskID);
    if (!task) return;
    if (!mountedRef.current) return;
    setBusy("move");
    try {
      // 邻居与 position 都按**服务端最新列表**算：本地 tasks 最长可能落后一个轮询周期
      // （另一个视图/另一台设备刚重排过），用过期快照算出来的位置会与真实顺序错位。
      const latest = await loadTasks();
      if (!mountedRef.current) return;
      const refreshed = latest.find((item) => item.id === taskID);
      if (!refreshed) return;
      const ordered = moveScope(refreshed, scopeList(latest));
      const index = ordered.findIndex((item) => item.id === taskID);
      const neighbor = ordered[index + (direction === "up" ? -1 : 1)];
      if (index < 0 || !neighbor) return;
      // 与相邻任务「换位」= 插到邻居的 before/after。原来用 neighbor.position ± 0.5，
      // 连续上移会在密集列表里算出重复 position，顺序随即退化到 createdAt 兜底。
      const position = positionForMove(latest, task.id, neighbor.id, direction === "up" ? "before" : "after");
      // 算不出安全落点（锚点被删／历史数据有重复 position）：不写服务端，提示用户重试。
      if (position === null) {
        fail("任务顺序数据异常，已重新同步，请再试一次");
        return;
      }
      await request(`/api/tasks/${task.id}`, { method: "PATCH", body: JSON.stringify({ title: refreshed.title, description: refreshed.description, priority: refreshed.priority, position }) });
      if (!mountedRef.current) return;
      await refresh(task.id);
    } catch (cause) { if (mountedRef.current) fail(cause instanceof Error ? cause.message : "无法调整任务顺序"); }
    finally { if (mountedRef.current) setBusy(""); }
  };

  const handleDrop = async (taskID: string, targetColumnID: string, placement: DropPlacement) => {
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
      // 没有任何锚点（列被搜索过滤成空）时不猜位置：保持原 position，不发请求。
      const anchor = placement.beforeTaskID ?? placement.afterTaskID;
      if (!anchor || anchor === taskID) return;
      const where: "before" | "after" = placement.beforeTaskID ? "before" : "after";
      if (!mountedRef.current) return;
      setBusy("move");
      try {
        // position 必须按**服务端最新列表**算（本地 tasks 最长落后一个轮询周期，
        // 用过期快照算出的位置会与真实顺序错位）；锚点仍是用户拖拽时看到的那一行，
        // 所以"放到这一行之前/之后"的意图完整保留。
        const latest = await loadTasks();
        if (!mountedRef.current) return;
        // 已经是这个位置（"拖到自己正下方"等无效手势）：不写，免得把间隙越挤越小
        if (isSameSlot(latest, task.id, anchor, where)) return;
        const newPosition = positionForMove(latest, task.id, anchor, where);
        // 算不出安全落点（锚点被删／历史数据有重复 position）：不写服务端并提示，列表刚拉过已是新的。
        if (newPosition === null) {
          fail("任务顺序数据异常，已重新同步，请再试一次");
          return;
        }
        // title/description/priority 用最新的值回填：PATCH 会无条件重写这三项，
        // 用本地旧快照会把别的端刚改的文案覆盖回去。
        const refreshed = latest.find((item) => item.id === taskID) ?? task;
        await request(`/api/tasks/${task.id}`, { method: "PATCH", body: JSON.stringify({ title: refreshed.title, description: refreshed.description, priority: refreshed.priority, position: newPosition }) });
        if (!mountedRef.current) return;
        await refresh();
      } catch (cause) { if (mountedRef.current) fail(cause instanceof Error ? cause.message : "无法调整任务顺序"); }
      finally { if (mountedRef.current) setBusy(""); }
    } else {
      if (task.status === targetStatus) return;
      if (!mountedRef.current) return;
      setBusy("move");
      try {
        // 只用来判断"目标列是不是空的"（空列 = 换列不改位置），同样按用户可见范围判断。
        const visibleColumn = visibleTasks.filter((t) => targetDef.statuses.includes(t.status))
          .sort(compareTaskOrder);
        const anchor = placement.beforeTaskID ?? placement.afterTaskID ?? null;
        // 位置按服务端最新列表算（同列分支的理由一致）；空列则保留原 position 不改全局顺序。
        const latest = await loadTasks();
        if (!mountedRef.current) return;
        const refreshed = latest.find((item) => item.id === taskID) ?? task;
        let newPosition: number;
        if (visibleColumn.length === 0 || !anchor) {
          // 空列（或目标列被搜索过滤成空）：换列不改动全局队列顺序，保留原 position。
          // position 互不相同，保留原值不会与其它任务撞位。
          newPosition = refreshed.position;
        } else {
          const computed = positionForMove(latest, task.id, anchor, placement.beforeTaskID ? "before" : "after");
          if (computed === null) {
            fail("任务顺序数据异常，已重新同步，请再试一次");
            return;
          }
          newPosition = computed;
        }
        await request(`/api/tasks/${task.id}`, { method: "PATCH", body: JSON.stringify({ title: refreshed.title, description: refreshed.description, priority: refreshed.priority, position: newPosition, status: targetStatus }) });
        if (!mountedRef.current) return;
        await refresh();
      } catch (cause) { if (mountedRef.current) fail(cause instanceof Error ? cause.message : "无法移动任务"); }
      finally { if (mountedRef.current) setBusy(""); }
    }
  };

  // 详情弹窗里 ↑/↓ 的可用性：与 moveTask 用同一份顺序，避免"按钮亮着但点了没反应"。
  const moveOrder = detail ? moveScope(detail) : [];
  const moveIndex = detail ? moveOrder.findIndex((item) => item.id === detail.id) : -1;

  return <section className="task-workspace" aria-label="项目任务">
    <header className="task-workspace-head">
      <div className="task-workspace-actions">
        {batchMode ? <>
          <div className="task-batch-bar">
            <label className="task-batch-select-all" title={query.trim() ? "只选中当前搜索出的任务" : "选中全部任务"}>
              <input className="task-check" type="checkbox" checked={visibleSelection.all} disabled={visibleSelection.total === 0} ref={(el) => { if (el) el.indeterminate = visibleSelection.partial; }} onChange={() => (visibleSelection.all ? deselectAll() : selectAll())} aria-label="全选当前可见任务" />
              <span>全选</span>
            </label>
            {/* 搜索框在批量模式下被整块换掉，筛选条件因此在界面上"消失"了 —— 若不说明，
                「点列头」选出来的其实是筛过的那几条，用户会以为选的是整列。 */}
            <span className="task-batch-hint" data-scope={query.trim() ? "filtered" : "all"}>{query.trim() ? `筛选生效：只作用于搜索出的 ${visibleTasks.length} 项` : view === "board" ? "点列头复选框可整列选择" : "点分组头复选框可整组选择"}</span>
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
            <label className="task-toolbar-search"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M11 4a7 7 0 1 0 0 14 7 7 0 0 0 0-14Z" /><path d="m16 16 4 4" /></svg><input type="search" value={query} placeholder="搜索任务…" aria-label="搜索任务" onChange={(event) => setQuery(event.target.value)} /></label>
            <button className="task-toolbar-action" onClick={() => navigate(`/projects/${projectID}/orchestration`)}><TaskToolbarIcon name="workflow" />自动编排</button>
            <button className="task-toolbar-action" onClick={enterBatchMode}><TaskToolbarIcon name="batch" />批量管理</button>
            <button className="task-toolbar-action task-toolbar-create" onClick={() => setEditor({})}><TaskToolbarIcon name="plus" />新建任务</button>
          </div>
        </>}
      </div>
    </header>
    {visibleTasks.length === 0 ? <div className="task-empty"><h3>还没有任务</h3><p>将可验证的开发事项加入项目，手动下发执行。</p><button className="primary" onClick={() => setEditor({})}>新建任务</button></div> : view === "board" ? <>{uncoveredStatusCount > 0 && <p className="task-board-note" role="status">有 <b>{uncoveredStatusCount}</b> 条任务的状态不在看板列里，切到「列表」可查看与清理。</p>}<TaskBoardColumns tasks={visibleTasks} showHistoricalCancelled={showHistoricalCancelled} open={openDetail} onDrop={handleDrop} batchMode={batchMode} selectedIDs={selectedIDs} toggleSelect={toggleSelect} toggleGroup={toggleGroup} /></> : <TaskList tasks={visibleTasks} showHistoricalCancelled={showHistoricalCancelled} open={openDetail} batchMode={batchMode} selectedIDs={selectedIDs} toggleSelect={toggleSelect} toggleGroup={toggleGroup} />}
    {editor && <TaskEditor projectID={projectID} task={editor.task} request={request} close={() => setEditor(null)} saved={async (taskID) => { setEditor(null); await refresh(taskID); }} fail={fail} />}
    {detail && <TaskDetailDialog detail={detail} permissionMode={permissionMode} busy={busy} close={() => setDetail(null)} refresh={() => refresh(detail.id)} beginDispatch={beginDispatch} enqueue={enqueue} orchestrationEnabled={Boolean(orchestration?.enabled)} transition={transition} deleteTask={deleteTask} confirmTransition={confirmTransition} edit={() => { const task = tasks.find((item) => item.id === detail.id); if (task) { setDetail(null); setEditor({ task }); } }} move={moveTask} canMoveUp={moveIndex > 0} canMoveDown={moveIndex >= 0 && moveIndex < moveOrder.length - 1} request={request} fail={fail} />}
    {pendingConfirm && createPortal(<ConfirmDialog title={pendingConfirm.title} message={pendingConfirm.message} danger={pendingConfirm.danger} confirmLabel={pendingConfirm.confirmLabel} className={pendingConfirm.className} icon={pendingConfirm.icon} onConfirm={pendingConfirm.onConfirm} onCancel={pendingConfirm.onCancel} />, document.body)}
  </section>;
}

function TaskBoardColumns({ tasks, showHistoricalCancelled, open, onDrop, batchMode, selectedIDs, toggleSelect, toggleGroup }: { tasks: Task[]; showHistoricalCancelled: boolean; open: (taskID: string) => void; onDrop: (taskID: string, columnID: string, placement: DropPlacement) => Promise<void>; batchMode: boolean; selectedIDs: Set<string>; toggleSelect: (taskID: string) => void; toggleGroup: (ids: string[]) => void }) {
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
    .sort(compareTaskOrder);

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
    }).sort(compareTaskOrder);
    // 落点只看**本次 drop 事件的坐标**（落在第几行、上半还是下半），不读 dragOverIndex：
    // 那是 dragover 写下的 React state，跨列移动或在列内空白处松手时可能残留着别的列/上一个
    // 位置的下标，落点会差一格甚至退化成"插到末尾"。坐标口径与侧栏队列行内落点完全一致。
    const renderedRows = [...(columnRefs.current[columnID]?.querySelectorAll<HTMLElement>(".task-item") || [])];
    const hitIndex = renderedRows.findIndex((row) => {
      const rect = row.getBoundingClientRect();
      return e.clientY < rect.top + rect.height / 2;
    });
    // 命中某行上半 = 插到它前面；落在所有行下半/列内空白 = 插到已渲染部分的末尾
    const targetIndex = hitIndex >= 0 ? hitIndex : renderedRows.length;
    // 槽位（targetIndex）是"插入线"位置（0..n）；翻译成"锚点 + 侧别"由 anchorForSlot 统一负责
    // （取槽位上一行的 after、贴头取首行的 before），看板与侧栏共用同一条规则。
    const target = anchorForSlot(columnTasks, targetIndex);
    if (!target) {
      // 目标列在可见范围内为空：不带锚点，交给 handleDrop 保留原 position（换列不改全局顺序）
      await onDrop(dragData.taskID, columnID, {});
      return;
    }
    await onDrop(dragData.taskID, columnID, target.placement === "before" ? { beforeTaskID: target.anchorID } : { afterTaskID: target.anchorID });
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
    // 列头复选框 = 「只选/只清这一列」。原来只有顶部一个全选，想清掉「已完成」得一列一列手工勾。
    const picked = batchMode ? items.filter((task) => selectedIDs.has(task.id)).length : 0;
    return <section
      className={`task-board-column column-${definition.id}${isDragOver ? " drag-over" : ""}`}
      key={definition.id}
      ref={(el) => { columnRefs.current[definition.id] = el; }}
      onDragOver={acceptsDrops ? (e) => handleColumnDragOver(e, definition.id) : undefined}
      onDragLeave={acceptsDrops ? (e) => handleColumnDragLeave(e, definition.id) : undefined}
      onDrop={acceptsDrops ? (e) => handleColumnDrop(e, definition.id) : undefined}
    >
      <header><h3>{definition.label}</h3><span className="task-board-column-tools">{batchMode && <input className="task-check" type="checkbox" checked={items.length > 0 && picked === items.length} disabled={items.length === 0} ref={(el) => { if (el) el.indeterminate = picked > 0 && picked < items.length; }} onChange={() => toggleGroup(items.map((task) => task.id))} aria-label={`${items.length > 0 && picked === items.length ? "取消选择" : "全选"}「${definition.label}」列 ${items.length} 个任务`} title={`${items.length > 0 && picked === items.length ? "取消选择" : "全选"}「${definition.label}」列`} />}<b>{items.length}</b></span></header>
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

function TaskList({ tasks, showHistoricalCancelled, open, batchMode, selectedIDs, toggleSelect, toggleGroup }: { tasks: Task[]; showHistoricalCancelled: boolean; open: (taskID: string) => void; batchMode: boolean; selectedIDs: Set<string>; toggleSelect: (taskID: string) => void; toggleGroup: (ids: string[]) => void }) {
  const sorted = [...tasks].sort(compareTaskOrder);
  const renderRow = (task: Task) => {
    const title = taskDisplayTitle(task);
    const description = task.description.trim();
    const selected = batchMode && selectedIDs.has(task.id);
    return <button key={task.id} className={`task-list-row${selected ? " batch-selected" : ""}`} aria-pressed={batchMode ? selected : undefined} onClick={() => open(task.id)}>{batchMode && <span className="task-list-check-col"><span className={`task-batch-check${selected ? " checked" : ""}`} aria-hidden="true"></span></span>}<span className="task-list-main"><b>{title}</b>{description && description !== title.trim() && <small>{description}</small>}</span><StatusBadge task={task} /><em className={`priority-${task.priority}`}>{priorityLabels[task.priority]}</em><time>{formatDate(task.updatedAt)}</time></button>;
  };
  const head = <div className="task-list-head">{batchMode && <span className="task-list-check-col"></span>}<span>任务</span><span>状态</span><span>优先级</span><span>最近更新</span></div>;
  // 列表原本是一条扁平队列，看不出「分类」也就无从只选某一类。批量模式改为按状态分组，
  // 每组给一个「全选本组」的复选框，与看板列头的能力对齐；退出批量后顺序完全照旧。
  if (!batchMode) return <div className="task-list">{head}{sorted.map(renderRow)}</div>;
  const definitions = showHistoricalCancelled ? [...columnDefinitions, historicalCancelledColumn] : columnDefinitions;
  const groups = definitions.map((definition) => ({ definition, items: sorted.filter((task) => definition.statuses.includes(task.status)) })).filter((group) => group.items.length > 0);
  // 兜底组：后端的 `tasks.status` 只是 `text not null default 'todo'`、**没有 CHECK 约束**
  // （见 control-server/internal/app/task.go），前端这个 union 是类型不是运行时保证。
  // 真出现没建模的状态值时，分组不能把它悄悄吞掉 —— 非批量列表能看到多少行，批量模式就必须看到多少行。
  const knownStatuses = new Set(definitions.flatMap((definition) => definition.statuses));
  const unknownItems = sorted.filter((task) => !knownStatuses.has(task.status));
  if (unknownItems.length > 0) groups.push({ definition: { id: "unknown-status", label: "其他状态", statuses: [] }, items: unknownItems });
  return <div className="task-list batch-mode">{head}{groups.map(({ definition, items }) => {
    const ids = items.map((task) => task.id);
    const picked = items.filter((task) => selectedIDs.has(task.id)).length;
    return <section className="task-list-group" key={definition.id}>
      <header className="task-list-group-head">
        <span className="task-list-check-col"><input className="task-check" type="checkbox" checked={items.length > 0 && picked === items.length} ref={(el) => { if (el) el.indeterminate = picked > 0 && picked < items.length; }} onChange={() => toggleGroup(ids)} aria-label={`${items.length > 0 && picked === items.length ? "取消选择" : "全选"}「${definition.label}」${items.length} 个任务`} title={`${items.length > 0 && picked === items.length ? "取消选择" : "全选"}「${definition.label}」`} /></span>
        <span className="task-list-group-label">{definition.label}<b>{items.length}</b></span>
        {picked > 0 && <span className="task-list-group-picked">已选 {picked}</span>}
      </header>
      {items.map(renderRow)}
    </section>;
  })}</div>;
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
    aria-pressed={batchMode ? selected : undefined}
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
    {/* 复选框与状态徽标同一行：原来它独占一个网格行，批量模式下每张卡会凭空高出约 26px。 */}
    <div className="task-item-top">{batchMode && <span className={`task-batch-check${selected ? " checked" : ""}`} aria-hidden="true" onClick={(e) => { e.stopPropagation(); toggleSelect(task.id); }}></span>}<StatusBadge task={task} /><span className={`task-item-priority priority-${task.priority}`} title={`${priorityLabels[task.priority]}优先级`} aria-label={`${priorityLabels[task.priority]}优先级`} /></div>
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
      // 新建：position 传 0 = 服务端约定的"追加到队列末尾"哨兵值（改写成 max(position)+1）。
      // 编辑：**不带** position —— 后端 Position 为指针，缺省即保留原值。不要回传编辑器打开时的
      // 快照值：面板开着期间（最长可能一个轮询周期）任务可能被拖拽重排，回传旧值会把它顶回去。
      // 这与队列内联编辑的约定一致（见 TaskQueue.inlineEdit）。
      const payload = task ? { title, description, priority } : { title, description, priority, position: 0 };
      const result = await request<Task>(task ? `/api/tasks/${task.id}` : `/api/projects/${projectID}/tasks`, { method: task ? "PATCH" : "POST", body: JSON.stringify(payload) });
      if (!mountedRef.current) return;
      await saved(result.id);
    } catch (cause) { if (mountedRef.current) fail(cause instanceof Error ? cause.message : "无法保存任务"); }
    finally { if (mountedRef.current) setBusy(false); }
  };
  return <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="task-editor-title"><section className="modal task-dialog task-editor-dialog"><header className="task-editor-header"><div><span>任务</span><h2 id="task-editor-title">{task ? "编辑任务" : "新建任务"}</h2></div><button type="button" className="task-editor-close" title="关闭" aria-label="关闭" disabled={busy} onClick={close}>x</button></header><form className="task-editor-form" onSubmit={(event) => void save(event)}><div className="task-form"><section className="task-form-section"><div className="task-form-section-head"><h3>基本信息</h3></div><div className="task-form-basic-grid"><label className="task-form-title">任务名称 <small>可选</small><input autoFocus maxLength={120} value={title} onChange={(event) => setTitle(event.target.value)} placeholder="例如：实现项目任务看板" /></label><label>优先级<select value={priority} onChange={(event) => setPriority(event.target.value as Priority)}>{Object.entries(priorityLabels).map(([value, label]) => <option key={value} value={value}>{label}</option>)}</select></label></div></section><section className="task-form-section"><div className="task-form-section-head"><h3>任务内容</h3></div><label>任务说明<textarea required maxLength={12000} value={description} onChange={(event) => setDescription(event.target.value)} placeholder="说明背景、范围、限制和需要完成的实现。" /></label></section></div><footer><button type="button" className="secondary" disabled={busy} onClick={close}>取消</button><button className="primary" disabled={busy}>{busy ? "保存中" : "保存任务"}</button></footer></form></section></div>;
}

function TaskDetailDialog({ detail, permissionMode, busy, close, refresh, beginDispatch, enqueue, orchestrationEnabled, transition, deleteTask, confirmTransition, edit, move, canMoveUp, canMoveDown, request, fail }: { detail: TaskDetail; permissionMode?: ExecutionPolicy; busy: string; close: () => void; refresh: () => Promise<void>; beginDispatch: () => void; enqueue: () => Promise<void>; orchestrationEnabled: boolean; transition: (action: "reopen" | "stop") => Promise<void>; deleteTask: () => void; confirmTransition: () => void; edit: () => void; move: (taskID: string, direction: "up" | "down") => Promise<void>; canMoveUp: boolean; canMoveDown: boolean; request: Request; fail: (message: string) => void }) {
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
  return <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="task-detail-title"><section className="modal task-dialog task-detail-dialog"><header className="task-detail-header"><div className="task-detail-heading"><h2 id="task-detail-title">{taskDisplayTitle(detail)}</h2><div><StatusBadge task={detail} /><span>更新于 {formatDate(detail.updatedAt)}</span></div></div><button type="button" className="task-detail-close" title="关闭" aria-label="关闭" onClick={close}>x</button></header><div className="task-detail-body"><div className="task-detail-meta"><span className={`priority-${detail.priority}`}>{priorityLabels[detail.priority]}优先级</span><span>执行权限：{policyLabel(permissionMode)}</span></div><section className="task-detail-section"><h3>任务说明</h3><p>{detail.description}</p></section><section className="task-detail-section"><h3>执行记录</h3>{detail.runs.length === 0 ? <p className="task-detail-empty">尚未下发。</p> : <div className="task-run-list"><div className="task-run-list-head"><span>次数</span><span>状态</span><span>时间</span></div>{detail.runs.map((run) => <div key={run.id}><b>第 {run.sequence} 次</b><span>{run.status}</span><time>{formatDate(run.createdAt)}</time>{run.failureReason && <small>{run.failureReason}</small>}</div>)}</div>}</section><section className="task-detail-section"><h3>验证记录</h3>{detail.verificationRuns.length === 0 ? <p className="task-detail-empty">尚无验证记录。</p> : <div className="verification-run-list">{detail.verificationRuns.map((run) => <details key={run.id} className="verification-run" open={run.status === "running"}><summary><b>{verificationPhaseLabel(run.phase)}</b><span className={`verification-status ${run.status}`}>{run.status === "running" ? "进行中" : run.status === "passed" ? "通过" : "失败"}</span><time>{formatDate(run.completedAt || run.createdAt)}</time></summary><p>{run.command}{run.reviewedSha ? ` @ ${run.reviewedSha.slice(0, 12)}` : ""}</p>{run.output && <pre>{run.output}</pre>}</details>)}</div>}</section>{reviewAction && <div className="task-review-sheet" ref={reviewSheetRef}><h3>要求修改</h3><textarea autoFocus required disabled={reviewSubmitting} value={note} onChange={(event) => setNote(event.target.value)} placeholder="说明需要补充或修改的内容" /><div><button className="secondary" disabled={reviewSubmitting} onClick={() => setReviewAction(null)}>返回</button><button className="primary" disabled={reviewSubmitting} onClick={() => void submitReview("request_changes")}>{reviewSubmitting ? "提交中" : "提交要求"}</button></div></div>}</div><footer className="task-detail-actions"><div className="task-detail-main-actions">{(detail.status === "todo" || detail.status === "action_required") && <><button className="secondary" disabled={Boolean(busy)} onClick={edit}>编辑</button><button className="secondary" disabled={Boolean(busy) || !canMoveUp} onClick={() => void move(detail.id, "up")}>上移</button><button className="secondary" disabled={Boolean(busy) || !canMoveDown} onClick={() => void move(detail.id, "down")}>下移</button></>}{detail.status === "awaiting_review" && <><button className="secondary" disabled={reviewBusy} onClick={() => setReviewAction("request_changes")}>要求修改</button><button className="primary" disabled={reviewBusy} onClick={() => void submitReview("accept")}>{reviewSubmitting ? "提交中" : "确认完成"}</button></>}{detail.status === "running" && <button className="task-detail-stop" disabled={Boolean(busy)} onClick={() => void transition("stop")}>{busy === "stop" ? "停止中" : "停止任务"}</button>}{(detail.status === "todo" || detail.status === "action_required") && <>{orchestrationEnabled && (canResumeStoppedOrchestration || !detail.orchestrationStatus) && <button className="secondary" disabled={Boolean(busy)} onClick={() => void enqueue()}>{busy === "enqueue" ? "处理中" : canResumeStoppedOrchestration ? "继续自动编排" : "加入自动队列"}</button>}<button className="primary" disabled={!detail.canDispatch || Boolean(busy)} onClick={beginDispatch}>{busy === "dispatch" ? "下发中" : "下发任务"}</button></>}{detail.status === "done" && <button className="secondary" disabled={Boolean(busy)} onClick={confirmTransition}>重新打开</button>}</div><button className="task-detail-delete" disabled={Boolean(busy)} onClick={() => void deleteTask()}>{busy === "delete" ? "删除中" : "删除任务"}</button></footer></section></div>;
}

function formatDate(value: string): string {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? "" : date.toLocaleString("zh-CN", { month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit" });
}
