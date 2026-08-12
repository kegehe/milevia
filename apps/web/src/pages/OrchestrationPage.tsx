import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useNavigate, useParams, useSearchParams } from "react-router-dom";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { useProjectContext } from "../stores/useProjectStore";
import type { AgentID, Event, Message } from "../lib/types";
import type { Task, TaskDetail } from "../features/tasks/task-model";
import "../markdown.css";
import "../conversation.css";
import "../orchestration.css";

type OrchestrationConfig = { projectId: string; enabled: boolean; mainBranch: string; agentId: "claude-code" | "codex"; verificationCommands: string[]; maxFixRounds: number; frozenReason?: string };
type OrchestrationJob = { id: string; taskId: string; position: number; status: string; attempt?: number; baseDevSha?: string; targetBranch?: string; taskBranch?: string; worktreePath?: string; conversationId?: string; batchId?: string; humanDecision?: string; resourcesCleanedAt?: string; lastError?: string; createdAt?: string; updatedAt?: string };
type OrchestrationBatch = { id: string; name: string; conversationStrategy: "new" | "continue"; status: "active" | "needs_human" | "paused" | "awaiting_main" | "completed"; taskCount: number; completedCount: number; createdAt: string; updatedAt: string };
type ConversationHistory = { conversation?: { agentId: AgentID }; activeRunId?: string | null; messages: Message[]; events: Event[]; hasMore: boolean; nextCursor: string };

const runningStatuses = new Set(["preparing", "implementing", "checking"]);
const refreshingStatuses = new Set(["queued", "preparing", "implementing", "checking"]);

function normalizedVerificationCommands(value: string) {
  return value.split("\n").map((item) => item.trim()).filter(Boolean);
}

function uniqueTaskIDs(taskIDs: string[]) {
  return [...new Set(taskIDs)];
}

function statusLabel(status: string, targetBranch = "main") {
  const labels: Record<string, string> = { queued: "等待执行", preparing: "准备工作区", implementing: "Agent 执行中", checking: "验证与审查", paused: "已暂停", stopped: "已停止", removing: "清理中", needs_human: "需要处理", awaiting_main: `待合并 ${targetBranch}`, integrated_to_dev: `待合并 ${targetBranch}`, released_to_main: `已合并 ${targetBranch}` };
  return labels[status] || status;
}

function orchestrationActivityLabel(status: string, agentID: AgentID) {
  const agentName = agentID === "codex" ? "Codex" : "Claude Code";
  if (status === "queued") return "任务正在队列中等待执行";
  if (status === "preparing") return "正在准备独立工作区";
  if (status === "implementing") return `${agentName} 正在处理任务`;
  if (status === "checking") return "正在验证变更并进行独立审查";
  return "";
}

function eventLabel(type: string) {
  const labels: Record<string, string> = {
    "orchestration.queued": "已加入自动编排队列", "orchestration.preparing": "开始准备工作区", "orchestration.stopped": "编排已停止", "orchestration.needs_human": "需要人工决策", "task.run_started": "Agent 开始执行", "task.run_succeeded": "Agent 执行完成", "task.run_failed": "Agent 执行失败", "task.accepted": "任务已确认", "task.changes_requested": "已要求修改"
  };
  return labels[type] || type.replace(/[._]/g, " ");
}

function formatDate(value?: string) {
  if (!value) return "--";
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? "--" : date.toLocaleString("zh-CN", { month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit" });
}

function shortSHA(value?: string) { return value ? value.slice(0, 12) : "--"; }

function candidateSummary(task: Task) {
  return task.description.replace(/\s+/g, " ").trim() || "未填写任务内容";
}

function OrchestrationConversationMessage({ message, agentID }: { message: Message; agentID: "claude-code" | "codex" }) {
  const isUser = message.role === "user";
  const agentName = agentID === "codex" ? "Codex" : "Claude";
  return <div className="timeline-entry message-entry"><article className={`message ${message.role}`}><header><span className="message-avatar">{isUser ? "你" : agentID === "codex" ? "<>" : "C"}</span><b>{isUser ? "你" : agentName}</b><time>{formatDate(message.createdAt)}</time></header><div className="markdown"><ReactMarkdown remarkPlugins={[remarkGfm]} skipHtml components={{ a: ({ href, children }) => <a href={href} target="_blank" rel="noreferrer">{children}</a> }}>{message.content}</ReactMarkdown></div></article></div>;
}

function ScrollNavigationIcon({ direction }: { direction: "top" | "previous" | "next" | "bottom" }) {
  const edge = direction === "top" || direction === "bottom";
  const up = direction === "top" || direction === "previous";
  return <svg className="scroll-btn-icon" viewBox="0 0 24 24" aria-hidden="true">
    {edge && <path d={up ? "M6 5.5h12" : "M6 18.5h12"} />}
    <path d={up ? "M12 18V7M8.5 10.5 12 7l3.5 3.5" : "M12 6v11m-3.5-3.5L12 17l3.5-3.5"} />
  </svg>;
}

export default function OrchestrationPage() {
  const { projectId } = useParams<{ projectId: string }>();
  const { api, setError } = useProjectContext();
  const navigate = useNavigate();
  const [params, setParams] = useSearchParams();
  const [config, setConfig] = useState<OrchestrationConfig | null>(null);
  const [jobs, setJobs] = useState<OrchestrationJob[]>([]);
	const [batches, setBatches] = useState<OrchestrationBatch[]>([]);
  const [tasks, setTasks] = useState<Task[]>([]);
  const [detail, setDetail] = useState<TaskDetail | null>(null);
  const [detailLoading, setDetailLoading] = useState(false);
  const [detailError, setDetailError] = useState("");
  const [history, setHistory] = useState<ConversationHistory | null>(null);
  const [busy, setBusy] = useState("");
  const [settingsOpen, setSettingsOpen] = useState(false);
	const [confirmMerge, setConfirmMerge] = useState(false);
	const [confirmCleanup, setConfirmCleanup] = useState(false);
	const [confirmDeleteBatch, setConfirmDeleteBatch] = useState(false);
	const [closeLockingProcesses, setCloseLockingProcesses] = useState(false);
	const [batchComposerOpen, setBatchComposerOpen] = useState(false);
  const [batchName, setBatchName] = useState("");
  const [targetBatchID, setTargetBatchID] = useState("");
	const [batchFilterID, setBatchFilterID] = useState("");
	const [conversationStrategy, setConversationStrategy] = useState<"new" | "continue">("new");
	const [decision, setDecision] = useState("");
  const mounted = useRef(true);
  const overviewRequestVersion = useRef(0);
  const selectedRequestVersion = useRef(0);
  const selectedRequestInFlight = useRef(0);
  const historyRef = useRef<ConversationHistory | null>(null);
  const conversationScrollRef = useRef<HTMLDivElement>(null);
  const userMessageElements = useRef(new Map<string, HTMLDivElement>());
  const [selectedEnqueue, setSelectedEnqueue] = useState<Set<string>>(new Set());
  const [draftTaskIDs, setDraftTaskIDs] = useState<string[]>([]);
  const selectedID = params.get("job") || "";
  const scopedJobs = useMemo(() => batchFilterID ? jobs.filter((job) => job.batchId === batchFilterID) : jobs, [batchFilterID, jobs]);
  const selected = scopedJobs.find((job) => job.id === selectedID) || scopedJobs[0] || null;
  const taskTitleByID = useMemo(() => new Map(tasks.map((task) => [task.id, task.title || "未命名任务"])), [tasks]);

  const loadOverview = useCallback(async () => {
    if (!projectId) return;
    const requestVersion = ++overviewRequestVersion.current;
    const [nextConfig, nextJobs, nextTasks, nextBatches] = await Promise.all([
      api<OrchestrationConfig>(`/api/projects/${projectId}/orchestration/config`),
      api<OrchestrationJob[]>(`/api/projects/${projectId}/orchestration`),
      api<Task[]>(`/api/projects/${projectId}/tasks`),
		api<OrchestrationBatch[]>(`/api/projects/${projectId}/orchestration/batches`),
    ]);
    if (!mounted.current || requestVersion !== overviewRequestVersion.current) return;
    setConfig(nextConfig);
    setJobs(nextJobs);
    setTasks(nextTasks);
		setBatches(nextBatches);
  }, [api, projectId]);

  const loadSelected = useCallback(async (historyMode: "full" | "latest" = "full") => {
    if (historyMode === "latest" && selectedRequestInFlight.current !== 0) return;
    const requestVersion = ++selectedRequestVersion.current;
    selectedRequestInFlight.current = requestVersion;
    try {
      if (!selected) {
        if (mounted.current && requestVersion === selectedRequestVersion.current) {
          setDetail(null);
          setDetailLoading(false);
          setDetailError("");
          historyRef.current = null;
          setHistory(null);
        }
        return;
      }
      if (mounted.current && requestVersion === selectedRequestVersion.current) {
        setDetailLoading(true);
        setDetailError("");
      }
      try {
        const nextDetail = await api<TaskDetail>(`/api/tasks/${selected.taskId}`);
        if (!mounted.current || requestVersion !== selectedRequestVersion.current) return;
        setDetail(nextDetail);
      } catch (cause) {
        if (mounted.current && requestVersion === selectedRequestVersion.current) {
          setDetail(null);
          setDetailError(cause instanceof Error ? cause.message : "无法加载任务详情");
        }
      }
      if (!mounted.current || requestVersion !== selectedRequestVersion.current) return;
      if (!selected.conversationId) {
        historyRef.current = null;
        setHistory(null);
        return;
      }
      const messages = new Map<string, Message>();
      const events = new Map<string, Event>();
      const shouldLoadFullHistory = historyMode === "full" || historyRef.current === null;
      const previousHistory = shouldLoadFullHistory ? null : historyRef.current;
      previousHistory?.messages.forEach((message) => messages.set(message.id, message));
      previousHistory?.events.forEach((event) => events.set(event.id, event));
      let agentId = previousHistory?.conversation?.agentId;
      let activeRunId = previousHistory?.activeRunId || null;
      let cursor = "";
      do {
        const query = new URLSearchParams({ limit: "1000" });
        if (cursor) query.set("cursor", cursor);
        const page = await api<ConversationHistory>(`/api/conversations/${selected.conversationId}?${query}`);
        if (!mounted.current || requestVersion !== selectedRequestVersion.current) return;
        page.messages.forEach((message) => messages.set(message.id, message));
        page.events.forEach((event) => events.set(event.id, event));
        agentId = page.conversation?.agentId || agentId;
        activeRunId = page.activeRunId || null;
        cursor = shouldLoadFullHistory && page.hasMore ? page.nextCursor : "";
      } while (cursor);
      if (mounted.current && requestVersion === selectedRequestVersion.current) {
        const nextHistory = {
          conversation: agentId ? { agentId } : undefined,
          activeRunId,
          messages: [...messages.values()].sort((left, right) => left.createdAt.localeCompare(right.createdAt) || left.id.localeCompare(right.id)),
          events: [...events.values()].sort((left, right) => left.createdAt.localeCompare(right.createdAt) || left.id.localeCompare(right.id)),
          hasMore: false,
          nextCursor: "",
        };
        historyRef.current = nextHistory;
        setHistory(nextHistory);
      }
    } finally {
      if (selectedRequestInFlight.current === requestVersion) {
        selectedRequestInFlight.current = 0;
        if (mounted.current && requestVersion === selectedRequestVersion.current) setDetailLoading(false);
      }
    }
  }, [api, selected?.id, selected?.taskId, selected?.conversationId]);

  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);
  useEffect(() => { void loadOverview().catch((cause) => setError(cause instanceof Error ? cause.message : "无法加载自动编排")); }, [loadOverview, setError]);
  useEffect(() => { void loadSelected().catch((cause) => setError(cause instanceof Error ? cause.message : "无法加载编排任务详情")); }, [loadSelected, setError]);
  useEffect(() => {
    if (!jobs.some((job) => refreshingStatuses.has(job.status))) return;
    const timer = window.setInterval(() => {
      void loadOverview().catch(() => undefined);
      if (selected && refreshingStatuses.has(selected.status)) void loadSelected("latest").catch(() => undefined);
    }, 5_000);
    return () => window.clearInterval(timer);
  }, [jobs, loadOverview, loadSelected, selected?.id, selected?.status]);

  const counts = useMemo(() => ({ active: jobs.filter((job) => runningStatuses.has(job.status)).length, waiting: jobs.filter((job) => job.status === "queued").length, blocked: jobs.filter((job) => job.status === "needs_human").length }), [jobs]);
  const messages = useMemo(() => history?.messages || [], [history]);
  const userMessages = useMemo(() => messages.filter((message) => message.role === "user"), [messages]);
  const [currentUserMessageIndex, setCurrentUserMessageIndex] = useState(-1);
  const selectedAgentID = history?.conversation?.agentId || config?.agentId || "claude-code";
  const activityLabel = selected ? orchestrationActivityLabel(selected.status, selectedAgentID) : "";
  const updateCurrentUserMessageIndex = useCallback(() => {
    const container = conversationScrollRef.current;
    if (!container || userMessages.length === 0) { setCurrentUserMessageIndex(-1); return; }
    const containerTop = container.getBoundingClientRect().top;
    let index = -1;
    userMessages.forEach((message, candidate) => {
      if ((userMessageElements.current.get(message.id)?.getBoundingClientRect().top || Infinity) <= containerTop + 16) index = candidate;
    });
    setCurrentUserMessageIndex((current) => current === index ? current : index);
  }, [userMessages]);
  const scrollToTop = () => { conversationScrollRef.current?.scrollTo({ top: 0, behavior: "smooth" }); setCurrentUserMessageIndex(userMessages.length ? 0 : -1); };
  const scrollToBottom = () => {
    const container = conversationScrollRef.current;
    if (container) container.scrollTo({ top: container.scrollHeight - container.clientHeight, behavior: "smooth" });
    setCurrentUserMessageIndex(userMessages.length - 1);
  };
  const scrollToUserMessage = (index: number) => {
    const message = userMessages[index];
    const element = message && userMessageElements.current.get(message.id);
    element?.scrollIntoView({ behavior: "smooth", block: "start" });
    if (element) setCurrentUserMessageIndex(index);
  };
  useEffect(() => { setCurrentUserMessageIndex(-1); }, [selected?.id]);
  useEffect(() => {
    const frame = requestAnimationFrame(updateCurrentUserMessageIndex);
    return () => cancelAnimationFrame(frame);
  }, [messages, updateCurrentUserMessageIndex]);
  const queuedTaskIDs = useMemo(() => jobs.filter((job) => job.status === "queued" || job.status === "paused").sort((left, right) => left.position - right.position).map((job) => job.taskId), [jobs]);
  const enqueueableTasks = useMemo(() => {
    const queued = new Set(jobs.map((job) => job.taskId));
    return tasks.filter((task) => (task.status === "todo" || task.status === "action_required") && !queued.has(task.id))
      .sort((left, right) => left.position - right.position || left.createdAt.localeCompare(right.createdAt));
  }, [jobs, tasks]);
	const selectedDraftTasks = useMemo(() => uniqueTaskIDs(draftTaskIDs).map((taskID) => enqueueableTasks.find((task) => task.id === taskID)).filter((task): task is Task => Boolean(task)), [draftTaskIDs, enqueueableTasks]);
	const visibleJobs = scopedJobs;

  useEffect(() => {
    if (selected?.id === selectedID) return;
    const next = new URLSearchParams(params);
    if (selected) next.set("job", selected.id);
    else next.delete("job");
    setParams(next, { replace: true });
  }, [params, selected, selectedID, setParams]);

  useEffect(() => {
    const candidateIDs = new Set(enqueueableTasks.map((task) => task.id));
    setSelectedEnqueue((previous) => new Set([...previous].filter((taskID) => candidateIDs.has(taskID))));
    setDraftTaskIDs((previous) => uniqueTaskIDs(previous.filter((taskID) => candidateIDs.has(taskID))));
  }, [enqueueableTasks]);
  const selectJob = (job: OrchestrationJob) => {
    selectedRequestVersion.current += 1;
    setDetail(null);
    setDetailLoading(true);
    setDetailError("");
    historyRef.current = null;
    setHistory(null);
    setParams({ job: job.id });
  };
  const action = async (name: "pause" | "resume" | "stop" | "merge-main") => {
    if (!selected) return;
    setBusy(name);
    try {
      await api(`/api/tasks/${selected.taskId}/orchestration/${name}`, { method: "POST", body: "{}" });
      setConfirmMerge(false);
      await Promise.all([loadOverview(), loadSelected()]);
    } catch (cause) { setError(cause instanceof Error ? cause.message : "编排操作失败"); }
    finally { if (mounted.current) setBusy(""); }
  };
  const saveConfig = async (nextConfig: OrchestrationConfig) => {
    if (!projectId) return;
    setBusy("config");
    try {
      await api(`/api/projects/${projectId}/orchestration/config`, { method: "PUT", body: JSON.stringify(nextConfig) });
      setSettingsOpen(false);
      await loadOverview();
    } catch (cause) { setError(cause instanceof Error ? cause.message : "无法保存编排配置"); }
    finally { if (mounted.current) setBusy(""); }
  };
  const queueAction = async (key: string, path: string, init: RequestInit): Promise<boolean> => {
    setBusy(key);
    try {
      await api(path, init);
      await Promise.all([loadOverview(), loadSelected()]);
      return true;
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "队列操作失败");
      await Promise.all([loadOverview(), loadSelected()]).catch(() => undefined);
      return false;
    }
    finally { if (mounted.current) setBusy(""); }
  };
  const toggleEnqueue = (taskID: string) => {
    setSelectedEnqueue((previous) => {
      const next = new Set(previous);
      if (next.has(taskID)) next.delete(taskID);
      else next.add(taskID);
      return next;
    });
    setDraftTaskIDs((previous) => previous.includes(taskID)
      ? previous.filter((id) => id !== taskID)
      : [...previous, taskID]);
  };
  const moveDraftTask = (taskID: string, direction: "up" | "down") => setDraftTaskIDs((previous) => {
    const index = previous.indexOf(taskID);
    const target = direction === "up" ? index - 1 : index + 1;
    if (index < 0 || target < 0 || target >= previous.length) return previous;
    const next = [...previous];
    [next[index], next[target]] = [next[target], next[index]];
    return next;
  });
  const createBatch = async () => {
    const taskIDs = selectedDraftTasks.map((task) => task.id);
		if (!taskIDs.length || !projectId) return;
		const existingBatch = targetBatchID && batches.find((batch) => batch.id === targetBatchID);
		if (!existingBatch && !batchName.trim()) { setError("请填写编排任务名称"); return; }
		const path = existingBatch ? `/api/projects/${projectId}/orchestration/batches/${existingBatch.id}/tasks` : `/api/projects/${projectId}/orchestration/batches`;
		const body = existingBatch ? { taskIds: taskIDs } : { name: batchName.trim(), taskIds: taskIDs, conversationStrategy };
		if (await queueAction("batch", path, { method: "POST", body: JSON.stringify(body) })) {
      setSelectedEnqueue(new Set());
      setDraftTaskIDs([]);
			setBatchName("");
			setTargetBatchID("");
			setConversationStrategy("new");
			setBatchComposerOpen(false);
    }
  };
	const submitDecision = async () => {
		if (!selected || !decision.trim()) return;
		if (await queueAction("decision", `/api/tasks/${selected.taskId}/orchestration/decision`, { method: "POST", body: JSON.stringify({ decision: decision.trim() }) })) setDecision("");
	};
	const cleanup = async (confirmUnmerged: boolean) => {
		if (!selected) return;
		if (await queueAction("cleanup", `/api/tasks/${selected.taskId}/orchestration/cleanup`, { method: "POST", body: JSON.stringify({ confirmUnmerged, closeLockingProcesses }) })) {
			setConfirmCleanup(false);
			setCloseLockingProcesses(false);
		}
	};
	const deleteBatch = async () => {
		if (!projectId || !batchFilterID) return;
		setBusy("delete-batch");
		try {
			await api(`/api/projects/${projectId}/orchestration/batches/${batchFilterID}`, { method: "DELETE" });
			setConfirmDeleteBatch(false);
			setBatchFilterID("");
			await loadOverview();
		} catch (cause) { setError(cause instanceof Error ? cause.message : "无法归档编排任务"); }
		finally { if (mounted.current) setBusy(""); }
	};
	const dequeueSelected = async () => {
		if (!selected || !["queued", "paused", "stopped"].includes(selected.status)) return;
		await queueAction(`dequeue:${selected.taskId}`, `/api/tasks/${selected.taskId}/orchestration/dequeue`, { method: "DELETE" });
	};
  const moveJob = async (taskID: string, direction: "up" | "down") => {
    if (!projectId) return;
    const index = queuedTaskIDs.indexOf(taskID);
    const target = direction === "up" ? index - 1 : index + 1;
    if (index < 0 || target < 0 || target >= queuedTaskIDs.length) return;
    const reordered = [...queuedTaskIDs];
    [reordered[index], reordered[target]] = [reordered[target], reordered[index]];
    await queueAction(`reorder:${taskID}`, `/api/projects/${projectId}/orchestration/order`, { method: "PATCH", body: JSON.stringify({ taskIds: reordered }) });
  };

  if (!projectId) return null;
  return <div id="workspace-panel-orchestration" className="workspace-tab-panel orchestration-page" role="tabpanel" aria-labelledby="workspace-tab-orchestration">
    <header className="orchestration-page-head">
      <div><h1>自动编排</h1></div>
      <div className="orchestration-head-actions"><span className={`orchestration-enabled ${config?.enabled ? "on" : "off"}`}>{config?.enabled ? "已启用" : "未启用"}</span><button type="button" className="primary" disabled={!config?.enabled} onClick={() => setBatchComposerOpen(true)}>新建编排任务</button><button type="button" className="secondary" onClick={() => setSettingsOpen(true)}>配置</button><button type="button" className="secondary" onClick={() => navigate(`/projects/${projectId}/tasks`)}>管理任务</button></div>
    </header>
    {config?.frozenReason && <div className="orchestration-freeze" role="alert"><b>队列已冻结</b><span>{config.frozenReason}</span></div>}
    <section className="orchestration-summary" aria-label="编排概览"><div><span>执行中</span><b>{counts.active}</b></div><div><span>等待队列</span><b>{counts.waiting}</b></div><div className={counts.blocked ? "attention" : ""}><span>需要处理</span><b>{counts.blocked}</b></div><dl><div><dt>编排任务</dt><dd>{batches.length} 个</dd></div><div><dt>新任务目标分支</dt><dd>{config?.mainBranch || "main"}</dd></div><div><dt>最大修复</dt><dd>{config?.maxFixRounds ?? "--"} 轮</dd></div></dl></section>
    <div className="orchestration-console">
      <aside className="orchestration-queue" aria-label="自动编排队列"><header><div><h2>队列</h2><span>{visibleJobs.length} 项</span></div><button type="button" title="刷新队列" aria-label="刷新队列" disabled={Boolean(busy)} onClick={() => void loadOverview()}>↻</button></header>{batches.length > 0 && <div className="orchestration-batch-filter"><label>编排任务<select value={batchFilterID} onChange={(event) => setBatchFilterID(event.target.value)}><option value="">全部 ({jobs.length})</option>{batches.map((batch) => <option key={batch.id} value={batch.id}>{batch.name} ({batch.completedCount}/{batch.taskCount})</option>)}</select></label>{batchFilterID && <button type="button" className="danger-text" disabled={Boolean(busy)} onClick={() => setConfirmDeleteBatch(true)}>归档编排任务</button>}</div>}{visibleJobs.length === 0 ? <p className="orchestration-empty">{batchFilterID ? "该编排任务暂无队列项。" : "暂无已入队任务。"}</p> : <ol>{visibleJobs.map((job) => {
        const reorderable = !batchFilterID && (job.status === "queued" || job.status === "paused");
        const removable = reorderable || job.status === "stopped";
        const queuedIndex = queuedTaskIDs.indexOf(job.taskId);
        return <li key={job.id}><button type="button" className={`${selected?.id === job.id ? "selected " : ""}${reorderable || removable ? "has-queue-actions" : ""}`} onClick={() => selectJob(job)}><span className={`orchestration-status-dot ${job.status}`} /><span className="orchestration-job-name"><b>#{job.position} {taskTitleByID.get(job.taskId) || "未命名任务"}</b><small>{statusLabel(job.status, job.targetBranch)}</small></span>{job.lastError && <i title={job.lastError}>!</i>}</button>{(reorderable || removable) && <div className="orchestration-queue-actions"><button type="button" title="上移" aria-label="上移" disabled={Boolean(busy) || queuedIndex <= 0} onClick={() => void moveJob(job.taskId, "up")}>↑</button><button type="button" title="下移" aria-label="下移" disabled={Boolean(busy) || queuedIndex < 0 || queuedIndex >= queuedTaskIDs.length - 1} onClick={() => void moveJob(job.taskId, "down")}>↓</button>{removable && <button type="button" title="移出队列" aria-label="移出队列" disabled={Boolean(busy)} onClick={() => void queueAction(`dequeue:${job.taskId}`, `/api/tasks/${job.taskId}/orchestration/dequeue`, { method: "DELETE" })}>{busy === `dequeue:${job.taskId}` ? "…" : "×"}</button>}</div>}</li>;
      })}</ol>}<section className="orchestration-candidates" aria-labelledby="orchestration-candidates-title"><header><h3 id="orchestration-candidates-title">候选任务</h3><button type="button" className="secondary" disabled={Boolean(busy) || selectedEnqueue.size === 0} onClick={() => setBatchComposerOpen(true)}>{`加入 (${selectedEnqueue.size})`}</button></header>{enqueueableTasks.length ? <ul>{enqueueableTasks.map((task) => { const summary = candidateSummary(task); return <li key={task.id}><label title={summary}><input type="checkbox" disabled={Boolean(busy)} checked={selectedEnqueue.has(task.id)} onChange={() => toggleEnqueue(task.id)} /><span>{summary}</span></label></li>; })}</ul> : <p className="orchestration-empty">没有可加入队列的任务。</p>}</section></aside>
      <main className="orchestration-conversation">{selected ? <>
        <header><div><h2>完整对话</h2><span>{messages.length} 条消息</span></div><a href={selected.conversationId ? `/projects/${projectId}/conversations/${selected.conversationId}?readonly=true` : undefined} onClick={(event) => { if (!selected.conversationId) event.preventDefault(); }} aria-disabled={!selected.conversationId}>在对话页打开</a></header>
        <div className="orchestration-conversation-content" ref={conversationScrollRef} onScroll={updateCurrentUserMessageIndex}><section className="timeline orchestration-conversation-timeline">{messages.length ? messages.map((message) => <div key={message.id} ref={message.role === "user" ? (element) => { if (element) userMessageElements.current.set(message.id, element); else userMessageElements.current.delete(message.id); } : undefined}><OrchestrationConversationMessage message={message} agentID={selectedAgentID} /></div>) : <p className="orchestration-empty">暂无可显示的执行对话。</p>}{activityLabel && <div className="run-indicator orchestration-run-indicator" role="status"><span></span>{activityLabel}</div>}</section></div>
        <div className="scroll-buttons orchestration-scroll-buttons"><button type="button" className="scroll-btn scroll-to-top" title="回到顶部" aria-label="回到顶部" onClick={scrollToTop}><ScrollNavigationIcon direction="top" /></button><button type="button" className="scroll-btn scroll-to-previous-message" title="上一条我的消息" aria-label="上一条我的消息" disabled={currentUserMessageIndex <= 0} onClick={() => scrollToUserMessage(currentUserMessageIndex - 1)}><ScrollNavigationIcon direction="previous" /></button><button type="button" className="scroll-btn scroll-to-next-message" title="下一条我的消息" aria-label="下一条我的消息" disabled={currentUserMessageIndex < 0 || currentUserMessageIndex >= userMessages.length - 1} onClick={() => scrollToUserMessage(currentUserMessageIndex + 1)}><ScrollNavigationIcon direction="next" /></button><button type="button" className="scroll-btn scroll-to-bottom" title="回到底部" aria-label="回到底部" onClick={scrollToBottom}><ScrollNavigationIcon direction="bottom" /></button></div>
      </> : <div className="orchestration-empty-main"><h2>选择一个编排任务</h2><p>从左侧队列查看完整对话和任务详情。</p></div>}</main>
      <aside className="orchestration-detail" aria-label="任务详情">{selected ? <>
        <header className="orchestration-detail-head"><div><div className="orchestration-detail-kicker"><span className={`orchestration-status-tag ${selected.status}`}>{statusLabel(selected.status, selected.targetBranch)}</span><time>更新于 {formatDate(selected.updatedAt)}</time></div><h2>{detail?.title || (detailLoading ? "加载任务详情中..." : "任务详情不可用")}</h2><p>{detail?.description || (detailLoading ? "正在加载任务详情。" : detailError || "暂时无法获取该任务详情。")}</p>{detailError && <button type="button" className="orchestration-detail-retry" onClick={() => void loadSelected()}>重试</button>}</div><div className="orchestration-detail-actions">{["queued", "preparing", "implementing", "checking"].includes(selected.status) && <button type="button" className="secondary" disabled={Boolean(busy)} onClick={() => void action("pause")}>{busy === "pause" ? "暂停中" : "暂停"}</button>}{["paused", "stopped"].includes(selected.status) && <button type="button" className="primary" disabled={Boolean(busy)} onClick={() => void action("resume")}>{busy === "resume" ? "处理中" : "继续执行"}</button>}{["preparing", "implementing", "checking", "paused", "needs_human"].includes(selected.status) && <button type="button" className="danger-text" disabled={Boolean(busy)} onClick={() => void action("stop")}>{busy === "stop" ? "停止中" : "停止"}</button>}{["awaiting_main", "integrated_to_dev"].includes(selected.status) && <button type="button" className="primary" disabled={Boolean(busy)} onClick={() => setConfirmMerge(true)}>合并至 {selected.targetBranch || "目标分支"}</button>}{selected.taskBranch && !selected.resourcesCleanedAt && ["released_to_main", "stopped", "needs_human"].includes(selected.status) && <button type="button" className="secondary" disabled={Boolean(busy)} onClick={() => setConfirmCleanup(true)}>清理资源</button>}{["queued", "paused", "stopped"].includes(selected.status) && <button type="button" className="danger-text" disabled={Boolean(busy)} onClick={() => void dequeueSelected()}>{busy === `dequeue:${selected.taskId}` ? "移出中" : "移出队列"}</button>}</div></header>
        {selected.lastError && <div className="orchestration-job-error"><b>需要处理</b><span>{selected.lastError}</span></div>}
				{selected.status === "needs_human" && <section className="orchestration-decision"><h3>需要人工决策</h3><p>提交的内容会写入本次编排对话，并作为下一次执行的上下文。</p><textarea value={decision} disabled={Boolean(busy)} placeholder="说明业务规则、取舍或下一步处理方式" onChange={(event) => setDecision(event.target.value)} /><button type="button" className="primary" disabled={Boolean(busy) || !decision.trim()} onClick={() => void submitDecision()}>{busy === "decision" ? "提交中" : "提交决策并继续"}</button></section>}
        <section className="orchestration-facts" aria-label="Git 记录"><div><span>任务分支</span><code title={selected.taskBranch}>{selected.taskBranch || "--"}</code></div><div><span>基线 SHA</span><code title={selected.baseDevSha}>{shortSHA(selected.baseDevSha)}</code></div><div><span>工作区</span><code title={selected.worktreePath}>{selected.worktreePath || "已清理"}</code></div><div><span>已执行轮次</span><b>{selected.attempt || 0}</b></div></section>
        <section className="orchestration-timeline"><header><h3>执行过程</h3><span>{detail?.events.length || 0} 条事件</span></header>{detail?.events.length ? <ol>{detail.events.slice().reverse().map((event) => <li key={event.id}><time>{formatDate(event.createdAt)}</time><span className={`timeline-marker ${event.type.includes("failed") || event.type.includes("changes") || event.type.includes("needs_human") ? "warn" : ""}`} /><div><b>{eventLabel(event.type)}</b><small>{event.type}</small></div></li>)}</ol> : <p className="orchestration-empty">执行事件将在任务开始后出现。</p>}</section>
      </> : <div className="orchestration-empty-main"><h2>选择一个编排任务</h2><p>从左侧队列查看任务详情。</p></div>}</aside>
    </div>
    {confirmMerge && selected && <div className="orchestration-confirm-backdrop" role="presentation"><section className="orchestration-confirm" role="dialog" aria-modal="true" aria-labelledby="merge-main-title"><h2 id="merge-main-title">合并至 {selected.targetBranch || "目标分支"}</h2><p>将 <code>{selected.taskBranch}</code> 合并到任务入队时锁定的 <code>{selected.targetBranch || "目标分支"}</code>。发生冲突时会中止合并并保留任务状态。</p><footer><button type="button" className="secondary" disabled={Boolean(busy)} onClick={() => setConfirmMerge(false)}>取消</button><button type="button" className="primary" disabled={Boolean(busy)} onClick={() => void action("merge-main")}>{busy === "merge-main" ? "合并中" : "确认合并"}</button></footer></section></div>}
		{confirmDeleteBatch && batchFilterID && <div className="orchestration-confirm-backdrop" role="presentation"><section className="orchestration-confirm" role="dialog" aria-modal="true" aria-labelledby="delete-batch-title"><h2 id="delete-batch-title">归档编排任务</h2><p>该计划将从列表隐藏，但队列任务、对话上下文、执行记录及 worktree 都会保留，不会影响正在等待或执行的任务。</p><footer><button type="button" className="secondary" disabled={Boolean(busy)} onClick={() => setConfirmDeleteBatch(false)}>取消</button><button type="button" className="danger" disabled={Boolean(busy)} onClick={() => void deleteBatch()}>{busy === "delete-batch" ? "归档中" : "确认归档"}</button></footer></section></div>}
		{confirmCleanup && selected && <div className="orchestration-confirm-backdrop" role="presentation"><section className="orchestration-confirm" role="dialog" aria-modal="true" aria-labelledby="cleanup-title"><h2 id="cleanup-title">清理 worktree 与分支</h2>{selected.status === "released_to_main" ? <p>该任务已合并至 <code>{selected.targetBranch || "目标分支"}</code>，将移除对应 worktree 和任务分支。</p> : <p className="orchestration-cleanup-warning">此任务尚未合并至 <code>{selected.targetBranch || "目标分支"}</code>。清理会删除 worktree 和任务分支，未合并提交将无法通过本页面恢复。</p>}<label className="orchestration-force-close"><input type="checkbox" checked={closeLockingProcesses} disabled={Boolean(busy)} onChange={(event) => setCloseLockingProcesses(event.target.checked)} /><span>关闭正在占用此 worktree 的程序后清理</span><small>可能会强制关闭 VS Code、终端或 AI CLI 中打开该目录的进程；其中未保存内容会丢失。</small></label><footer><button type="button" className="secondary" disabled={Boolean(busy)} onClick={() => { setConfirmCleanup(false); setCloseLockingProcesses(false); }}>取消</button><button type="button" className="danger" disabled={Boolean(busy)} onClick={() => void cleanup(selected.status !== "released_to_main")}>{busy === "cleanup" ? "清理中" : selected.status === "released_to_main" ? "确认清理" : "我确认未合并也清理"}</button></footer></section></div>}
		{batchComposerOpen && <div className="orchestration-confirm-backdrop" role="presentation"><section className="orchestration-settings-dialog" role="dialog" aria-modal="true" aria-labelledby="batch-title"><header><div><p className="orchestration-eyebrow">EXECUTION PLAN</p><h2 id="batch-title">新建或追加编排任务</h2></div><button type="button" title="关闭" aria-label="关闭" disabled={Boolean(busy)} onClick={() => setBatchComposerOpen(false)}>x</button></header><div className="orchestration-settings-form"><label className="wide">加入到<select value={targetBatchID} disabled={Boolean(busy)} onChange={(event) => setTargetBatchID(event.target.value)}><option value="">新建编排任务</option>{batches.filter((batch) => batch.status !== "completed").map((batch) => <option key={batch.id} value={batch.id}>{batch.name}</option>)}</select></label>{!targetBatchID && <><label className="wide">名称<input value={batchName} disabled={Boolean(busy)} placeholder="例如：支付流程修复" onChange={(event) => setBatchName(event.target.value)} /></label><label className="wide">后续任务上下文<select value={conversationStrategy} disabled={Boolean(busy)} onChange={(event) => setConversationStrategy(event.target.value as "new" | "continue")}><option value="new">不继承上一任务上下文</option><option value="continue">继承上一任务对话摘要</option></select><small>每个任务都会新建执行会话并使用自己的 worktree；仅将上一任务的对话摘要带入下一任务。</small></label></>}</div><section className="orchestration-draft-order"><header><h3>执行顺序</h3><span>{selectedDraftTasks.length} 项</span></header>{selectedDraftTasks.length ? <ol>{selectedDraftTasks.map((task, index) => <li key={task.id}><span title={candidateSummary(task)}>{candidateSummary(task)}</span><div><button type="button" title="上移" aria-label="上移" disabled={Boolean(busy) || index === 0} onClick={() => moveDraftTask(task.id, "up")}>↑</button><button type="button" title="下移" aria-label="下移" disabled={Boolean(busy) || index === selectedDraftTasks.length - 1} onClick={() => moveDraftTask(task.id, "down")}>↓</button><button type="button" title="移除" aria-label="移除" disabled={Boolean(busy)} onClick={() => toggleEnqueue(task.id)}>×</button></div></li>)}</ol> : <p>请先在候选任务中选择任务。</p>}</section><footer><button type="button" className="secondary" disabled={Boolean(busy)} onClick={() => setBatchComposerOpen(false)}>取消</button><button type="button" className="primary" disabled={Boolean(busy) || selectedDraftTasks.length === 0 || (!targetBatchID && !batchName.trim())} onClick={() => void createBatch()}>{busy === "batch" ? "处理中" : targetBatchID ? "追加到编排任务" : "创建并加入队列"}</button></footer></section></div>}
    {settingsOpen && config && <OrchestrationSettings config={config} busy={busy === "config"} close={() => setSettingsOpen(false)} save={saveConfig} />}
  </div>;
}

function OrchestrationSettings({ config, busy, close, save }: { config: OrchestrationConfig; busy: boolean; close: () => void; save: (config: OrchestrationConfig) => Promise<void> }) {
  const [enabled, setEnabled] = useState(config.enabled);
  const [mainBranch, setMainBranch] = useState(config.mainBranch);
  const [agentId, setAgentId] = useState<OrchestrationConfig["agentId"]>(config.agentId);
  const [commands, setCommands] = useState(config.verificationCommands.join("\n"));
  const [maxFixRounds, setMaxFixRounds] = useState(config.maxFixRounds);
  const verificationCommands = normalizedVerificationCommands(commands);
  const verificationError = enabled && verificationCommands.length === 0;
  return <div className="orchestration-confirm-backdrop" role="presentation"><section className="orchestration-settings-dialog" role="dialog" aria-modal="true" aria-labelledby="orchestration-settings-title"><header><div><p className="orchestration-eyebrow">PROJECT POLICY</p><h2 id="orchestration-settings-title">编排配置</h2></div><button type="button" title="关闭" aria-label="关闭" disabled={busy} onClick={close}>x</button></header><label className="orchestration-setting-toggle"><input type="checkbox" checked={enabled} disabled={busy} onChange={(event) => setEnabled(event.target.checked)} />启用自动队列</label><div className="orchestration-settings-form"><label>稳定分支<input value={mainBranch} disabled={busy} required onChange={(event) => setMainBranch(event.target.value)} /></label><label>执行 Agent<select value={agentId} disabled={busy} onChange={(event) => setAgentId(event.target.value as OrchestrationConfig["agentId"])}><option value="claude-code">Claude Code</option><option value="codex">Codex</option></select></label><label className="wide">验证命令<textarea value={commands} aria-invalid={verificationError} disabled={busy} required placeholder="每行一条命令" onChange={(event) => setCommands(event.target.value)} />{verificationError && <small className="orchestration-field-error">启用自动编排时至少需要一条验证命令。</small>}</label><label>最大修复轮次<input type="number" min={1} max={10} value={maxFixRounds} disabled={busy} onChange={(event) => setMaxFixRounds(Number(event.target.value))} /></label></div><p>配置仅作用于之后加入队列的任务。</p><footer><button type="button" className="secondary" disabled={busy} onClick={close}>取消</button><button type="button" className="primary" disabled={busy || !mainBranch.trim() || verificationError} onClick={() => void save({ ...config, enabled, mainBranch: mainBranch.trim(), agentId, verificationCommands, maxFixRounds })}>{busy ? "保存中" : "保存配置"}</button></footer></section></div>;
}
