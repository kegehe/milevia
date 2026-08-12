import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useNavigate, useParams, useSearchParams } from "react-router-dom";
import { useProjectContext } from "../stores/useProjectStore";
import type { Event, Message } from "../lib/types";
import type { Task, TaskDetail, VerificationRun } from "../features/tasks/task-model";
import "../orchestration.css";

type OrchestrationConfig = { projectId: string; enabled: boolean; mainBranch: string; agentId: "claude-code" | "codex"; verificationCommands: string[]; maxFixRounds: number; frozenReason?: string };
type OrchestrationJob = { id: string; taskId: string; position: number; status: string; attempt?: number; baseDevSha?: string; taskBranch?: string; worktreePath?: string; conversationId?: string; batchId?: string; humanDecision?: string; resourcesCleanedAt?: string; lastError?: string; createdAt?: string; updatedAt?: string };
type OrchestrationBatch = { id: string; name: string; conversationStrategy: "new" | "continue"; status: "active" | "needs_human" | "paused" | "awaiting_main" | "completed"; taskCount: number; completedCount: number; createdAt: string; updatedAt: string };
type ConversationHistory = { messages: Message[]; events: Event[] };
type EvidenceTab = "conversation" | "verification" | "timeline" | "git";

const runningStatuses = new Set(["preparing", "implementing", "checking"]);
const refreshingStatuses = new Set(["queued", "preparing", "implementing", "checking"]);

function normalizedVerificationCommands(value: string) {
  return value.split("\n").map((item) => item.trim()).filter(Boolean);
}

function statusLabel(status: string) {
  const labels: Record<string, string> = { queued: "等待执行", preparing: "准备工作区", implementing: "Agent 执行中", checking: "验证与审查", paused: "已暂停", stopped: "已停止", removing: "清理中", needs_human: "需要处理", awaiting_main: "待合并 main", integrated_to_dev: "待合并 main", released_to_main: "已合并 main" };
  return labels[status] || status;
}

function phaseLabel(phase: string) {
  return phase === "baseline" ? "基线验证" : phase === "task" ? "任务验证" : phase === "review" ? "独立审查" : phase === "integration" ? "集成验证" : phase;
}

function eventLabel(type: string) {
  const labels: Record<string, string> = {
    "orchestration.queued": "已加入自动编排队列", "orchestration.preparing": "开始准备工作区", "orchestration.stopped": "编排已停止", "task.run_started": "Agent 开始执行", "task.run_succeeded": "Agent 执行完成", "task.run_failed": "Agent 执行失败", "task.accepted": "任务已确认", "task.changes_requested": "已要求修改"
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
  const [history, setHistory] = useState<ConversationHistory | null>(null);
  const [evidenceTab, setEvidenceTab] = useState<EvidenceTab>("conversation");
  const [busy, setBusy] = useState("");
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [confirmMerge, setConfirmMerge] = useState(false);
	const [confirmCleanup, setConfirmCleanup] = useState(false);
	const [batchComposerOpen, setBatchComposerOpen] = useState(false);
  const [batchName, setBatchName] = useState("");
  const [targetBatchID, setTargetBatchID] = useState("");
	const [batchFilterID, setBatchFilterID] = useState("");
	const [conversationStrategy, setConversationStrategy] = useState<"new" | "continue">("new");
	const [decision, setDecision] = useState("");
  const mounted = useRef(true);
  const overviewRequestVersion = useRef(0);
  const selectedRequestVersion = useRef(0);
  const [selectedEnqueue, setSelectedEnqueue] = useState<Set<string>>(new Set());
  const [draftTaskIDs, setDraftTaskIDs] = useState<string[]>([]);
  const selectedID = params.get("job") || "";
  const selected = jobs.find((job) => job.id === selectedID) || jobs[0] || null;
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

  const loadSelected = useCallback(async () => {
    const requestVersion = ++selectedRequestVersion.current;
    if (!selected) {
      if (mounted.current && requestVersion === selectedRequestVersion.current) { setDetail(null); setHistory(null); }
      return;
    }
    const nextDetail = await api<TaskDetail>(`/api/tasks/${selected.taskId}`);
    if (!mounted.current || requestVersion !== selectedRequestVersion.current) return;
    setDetail(nextDetail);
    if (!selected.conversationId) { setHistory(null); return; }
    const nextHistory = await api<ConversationHistory>(`/api/conversations/${selected.conversationId}?limit=120`);
    if (mounted.current && requestVersion === selectedRequestVersion.current) setHistory(nextHistory);
  }, [api, selected?.id, selected?.taskId, selected?.conversationId]);

  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);
  useEffect(() => { void loadOverview().catch((cause) => setError(cause instanceof Error ? cause.message : "无法加载自动编排")); }, [loadOverview, setError]);
  useEffect(() => { void loadSelected().catch((cause) => setError(cause instanceof Error ? cause.message : "无法加载编排任务详情")); }, [loadSelected, setError]);
  useEffect(() => {
    if (!jobs.some((job) => refreshingStatuses.has(job.status))) return;
    const timer = window.setInterval(() => { void loadOverview().catch(() => undefined); void loadSelected().catch(() => undefined); }, 5_000);
    return () => window.clearInterval(timer);
  }, [jobs, loadOverview, loadSelected]);

  const counts = useMemo(() => ({ active: jobs.filter((job) => runningStatuses.has(job.status)).length, waiting: jobs.filter((job) => job.status === "queued").length, blocked: jobs.filter((job) => job.status === "needs_human").length }), [jobs]);
  const messages = useMemo(() => (history?.messages || []).slice(-24), [history]);
  const queuedTaskIDs = useMemo(() => jobs.filter((job) => job.status === "queued" || job.status === "paused").sort((left, right) => left.position - right.position).map((job) => job.taskId), [jobs]);
  const enqueueableTasks = useMemo(() => {
    const queued = new Set(jobs.map((job) => job.taskId));
    return tasks.filter((task) => (task.status === "todo" || task.status === "action_required") && !queued.has(task.id))
      .sort((left, right) => left.position - right.position || left.createdAt.localeCompare(right.createdAt));
  }, [jobs, tasks]);
	const selectedDraftTasks = useMemo(() => draftTaskIDs.map((taskID) => enqueueableTasks.find((task) => task.id === taskID)).filter((task): task is Task => Boolean(task)), [draftTaskIDs, enqueueableTasks]);
	const visibleJobs = useMemo(() => batchFilterID ? jobs.filter((job) => job.batchId === batchFilterID) : jobs, [batchFilterID, jobs]);

  useEffect(() => {
    const candidateIDs = new Set(enqueueableTasks.map((task) => task.id));
    setSelectedEnqueue((previous) => new Set([...previous].filter((taskID) => candidateIDs.has(taskID))));
    setDraftTaskIDs((previous) => previous.filter((taskID) => candidateIDs.has(taskID)));
  }, [enqueueableTasks]);
  const selectJob = (job: OrchestrationJob) => {
    selectedRequestVersion.current += 1;
    setDetail(null);
    setHistory(null);
    setEvidenceTab("conversation");
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
  const toggleEnqueue = (taskID: string) => setSelectedEnqueue((previous) => {
    const next = new Set(previous);
    if (next.has(taskID)) {
      next.delete(taskID);
      setDraftTaskIDs((draft) => draft.filter((id) => id !== taskID));
    } else {
      next.add(taskID);
      setDraftTaskIDs((draft) => [...draft, taskID]);
    }
    return next;
  });
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
		if (await queueAction("cleanup", `/api/tasks/${selected.taskId}/orchestration/cleanup`, { method: "POST", body: JSON.stringify({ confirmUnmerged }) })) setConfirmCleanup(false);
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
      <div><p className="orchestration-eyebrow">PROJECT WORKFLOW</p><h1>自动编排</h1><p>以独立编排任务组织队列、执行证据、验收与分支集成。</p></div>
      <div className="orchestration-head-actions"><span className={`orchestration-enabled ${config?.enabled ? "on" : "off"}`}>{config?.enabled ? "已启用" : "未启用"}</span><button type="button" className="primary" disabled={!config?.enabled} onClick={() => setBatchComposerOpen(true)}>新建编排任务</button><button type="button" className="secondary" onClick={() => setSettingsOpen(true)}>配置</button><button type="button" className="secondary" onClick={() => navigate(`/projects/${projectId}/tasks`)}>管理任务</button></div>
    </header>
    {config?.frozenReason && <div className="orchestration-freeze" role="alert"><b>队列已冻结</b><span>{config.frozenReason}</span></div>}
    <section className="orchestration-summary" aria-label="编排概览"><div><span>执行中</span><b>{counts.active}</b></div><div><span>等待队列</span><b>{counts.waiting}</b></div><div className={counts.blocked ? "attention" : ""}><span>需要处理</span><b>{counts.blocked}</b></div><dl><div><dt>编排任务</dt><dd>{batches.length} 个</dd></div><div><dt>目标分支</dt><dd>{config?.mainBranch || "main"}</dd></div><div><dt>最大修复</dt><dd>{config?.maxFixRounds ?? "--"} 轮</dd></div></dl></section>
    <div className="orchestration-console">
      <aside className="orchestration-queue" aria-label="自动编排队列"><header><div><h2>队列</h2><span>{visibleJobs.length} 项</span></div><button type="button" title="刷新队列" aria-label="刷新队列" disabled={Boolean(busy)} onClick={() => void loadOverview()}>↻</button></header>{batches.length > 0 && <section className="orchestration-batch-list"><h3>编排任务</h3><button type="button" className={!batchFilterID ? "selected" : ""} onClick={() => setBatchFilterID("")}>全部 <small>{jobs.length}</small></button>{batches.map((batch) => <button key={batch.id} type="button" className={batchFilterID === batch.id ? "selected" : ""} onClick={() => setBatchFilterID(batch.id)}><b title={batch.name}>{batch.name}</b><small>{batch.status === "active" ? "执行中" : batch.status === "needs_human" ? "待决策" : batch.status === "paused" ? "已暂停" : batch.status === "awaiting_main" ? "待合并" : "已完成"} · {batch.completedCount}/{batch.taskCount}</small></button>)}</section>}{visibleJobs.length === 0 ? <p className="orchestration-empty">{batchFilterID ? "该编排任务暂无队列项。" : "暂无已入队任务。"}</p> : <ol>{visibleJobs.map((job) => {
        const reorderable = !batchFilterID && (job.status === "queued" || job.status === "paused");
        const removable = reorderable || job.status === "stopped";
        const queuedIndex = queuedTaskIDs.indexOf(job.taskId);
        return <li key={job.id}><button type="button" className={`${selected?.id === job.id ? "selected " : ""}${reorderable || removable ? "has-queue-actions" : ""}`} onClick={() => selectJob(job)}><span className={`orchestration-status-dot ${job.status}`} /><span className="orchestration-job-name"><b>#{job.position} {taskTitleByID.get(job.taskId) || "未命名任务"}</b><small>{statusLabel(job.status)}</small></span>{job.lastError && <i title={job.lastError}>!</i>}</button>{(reorderable || removable) && <div className="orchestration-queue-actions"><button type="button" title="上移" aria-label="上移" disabled={Boolean(busy) || queuedIndex <= 0} onClick={() => void moveJob(job.taskId, "up")}>↑</button><button type="button" title="下移" aria-label="下移" disabled={Boolean(busy) || queuedIndex < 0 || queuedIndex >= queuedTaskIDs.length - 1} onClick={() => void moveJob(job.taskId, "down")}>↓</button>{removable && <button type="button" title="移出队列" aria-label="移出队列" disabled={Boolean(busy)} onClick={() => void queueAction(`dequeue:${job.taskId}`, `/api/tasks/${job.taskId}/orchestration/dequeue`, { method: "DELETE" })}>{busy === `dequeue:${job.taskId}` ? "…" : "×"}</button>}</div>}</li>;
      })}</ol>}<section className="orchestration-candidates" aria-labelledby="orchestration-candidates-title"><header><h3 id="orchestration-candidates-title">候选任务</h3><button type="button" className="secondary" disabled={Boolean(busy) || selectedEnqueue.size === 0} onClick={() => setBatchComposerOpen(true)}>{`加入 (${selectedEnqueue.size})`}</button></header>{enqueueableTasks.length ? <ul>{enqueueableTasks.map((task) => { const summary = candidateSummary(task); return <li key={task.id}><label title={summary}><input type="checkbox" disabled={Boolean(busy)} checked={selectedEnqueue.has(task.id)} onChange={() => toggleEnqueue(task.id)} /><span>{summary}</span></label></li>; })}</ul> : <p className="orchestration-empty">没有可加入队列的任务。</p>}</section></aside>
      <main className="orchestration-detail">{selected ? <>
        <header className="orchestration-detail-head"><div><div className="orchestration-detail-kicker"><span className={`orchestration-status-tag ${selected.status}`}>{statusLabel(selected.status)}</span><time>更新于 {formatDate(selected.updatedAt)}</time></div><h2>{detail?.title || "加载任务详情中..."}</h2><p>{detail?.description || "该任务正在等待详情加载。"}</p></div><div className="orchestration-detail-actions">{["queued", "preparing", "implementing", "checking"].includes(selected.status) && <button type="button" className="secondary" disabled={Boolean(busy)} onClick={() => void action("pause")}>{busy === "pause" ? "暂停中" : "暂停"}</button>}{["paused", "stopped"].includes(selected.status) && <button type="button" className="primary" disabled={Boolean(busy)} onClick={() => void action("resume")}>{busy === "resume" ? "处理中" : "继续"}</button>}{["preparing", "implementing", "checking", "paused", "needs_human"].includes(selected.status) && <button type="button" className="danger-text" disabled={Boolean(busy)} onClick={() => void action("stop")}>{busy === "stop" ? "停止中" : "停止"}</button>}{["awaiting_main", "integrated_to_dev"].includes(selected.status) && <button type="button" className="primary" disabled={Boolean(busy)} onClick={() => setConfirmMerge(true)}>合并至 main</button>}{selected.taskBranch && !selected.resourcesCleanedAt && ["released_to_main", "stopped", "needs_human"].includes(selected.status) && <button type="button" className="secondary" disabled={Boolean(busy)} onClick={() => setConfirmCleanup(true)}>清理资源</button>}</div></header>
        {selected.lastError && <div className="orchestration-job-error"><b>需要处理</b><span>{selected.lastError}</span></div>}
				{selected.status === "needs_human" && <section className="orchestration-decision"><h3>需要人工决策</h3><p>提交的内容会写入本次编排对话，并作为下一次执行的上下文。</p><textarea value={decision} disabled={Boolean(busy)} placeholder="说明业务规则、取舍或下一步处理方式" onChange={(event) => setDecision(event.target.value)} /><button type="button" className="primary" disabled={Boolean(busy) || !decision.trim()} onClick={() => void submitDecision()}>{busy === "decision" ? "提交中" : "提交决策并继续"}</button></section>}
        <section className="orchestration-facts" aria-label="Git 记录"><div><span>任务分支</span><code title={selected.taskBranch}>{selected.taskBranch || "--"}</code></div><div><span>基线 SHA</span><code title={selected.baseDevSha}>{shortSHA(selected.baseDevSha)}</code></div><div><span>工作区</span><code title={selected.worktreePath}>{selected.worktreePath || "已清理"}</code></div><div><span>执行轮次</span><b>{selected.attempt || 0} / {config?.maxFixRounds ?? "--"}</b></div></section>
        <section className="orchestration-timeline"><header><h3>执行轨迹</h3><span>{detail?.events.length || 0} 条事件</span></header>{detail?.events.length ? <ol>{detail.events.slice().reverse().map((event) => <li key={event.id}><time>{formatDate(event.createdAt)}</time><span className={`timeline-marker ${event.type.includes("failed") || event.type.includes("changes") ? "warn" : ""}`} /><div><b>{eventLabel(event.type)}</b><small>{event.type}</small></div></li>)}</ol> : <p className="orchestration-empty">执行事件将在任务开始后出现。</p>}</section>
      </> : <div className="orchestration-empty-main"><h2>选择一个编排任务</h2><p>从左侧队列查看执行状态、质量证据和分支信息。</p></div>}</main>
      <aside className="orchestration-evidence" aria-label="编排证据">{selected ? <><header><h2>执行证据</h2><a href={selected.conversationId ? `/projects/${projectId}/conversations/${selected.conversationId}?readonly=true` : undefined} onClick={(event) => { if (!selected.conversationId) event.preventDefault(); }} aria-disabled={!selected.conversationId}>完整对话</a></header><div className="orchestration-evidence-tabs" role="tablist"><button role="tab" aria-selected={evidenceTab === "conversation"} className={evidenceTab === "conversation" ? "active" : ""} onClick={() => setEvidenceTab("conversation")}>对话</button><button role="tab" aria-selected={evidenceTab === "verification"} className={evidenceTab === "verification" ? "active" : ""} onClick={() => setEvidenceTab("verification")}>验证</button><button role="tab" aria-selected={evidenceTab === "timeline"} className={evidenceTab === "timeline" ? "active" : ""} onClick={() => setEvidenceTab("timeline")}>运行</button><button role="tab" aria-selected={evidenceTab === "git"} className={evidenceTab === "git" ? "active" : ""} onClick={() => setEvidenceTab("git")}>Git</button></div>{evidenceTab === "conversation" && <div className="orchestration-message-list">{messages.length ? messages.map((message) => <article key={message.id} className={message.role === "user" ? "user" : "assistant"}><header><span>{message.role === "user" ? "任务指令" : "执行输出"}</span><time>{formatDate(message.createdAt)}</time></header><p>{message.content}</p></article>) : <p className="orchestration-empty">暂无可显示的执行对话。</p>}</div>}{evidenceTab === "verification" && <VerificationEvidence runs={detail?.verificationRuns || []} />}{evidenceTab === "timeline" && <div className="orchestration-run-list">{detail?.runs.length ? detail.runs.map((run) => <div key={run.id}><b>第 {run.sequence} 次执行</b><span>{run.status}</span><time>{formatDate(run.finishedAt || run.createdAt)}</time>{run.failureReason && <small>{run.failureReason}</small>}</div>) : <p className="orchestration-empty">尚未产生 Agent 执行记录。</p>}</div>}{evidenceTab === "git" && <div className="orchestration-git-evidence"><div><span>目标分支</span><code>{config?.mainBranch || "main"}</code></div><div><span>任务分支</span><code>{selected.taskBranch || "--"}</code></div><div><span>基线</span><code>{selected.baseDevSha || "--"}</code></div><div><span>工作区</span><code>{selected.worktreePath || "已清理"}</code></div></div>}</> : null}</aside>
    </div>
    {confirmMerge && selected && <div className="orchestration-confirm-backdrop" role="presentation"><section className="orchestration-confirm" role="dialog" aria-modal="true" aria-labelledby="merge-main-title"><h2 id="merge-main-title">合并至 main</h2><p>将 <code>{selected.taskBranch}</code> 合并到 <code>{config?.mainBranch || "main"}</code>。发生冲突时会中止合并并保留任务状态。</p><footer><button type="button" className="secondary" disabled={Boolean(busy)} onClick={() => setConfirmMerge(false)}>取消</button><button type="button" className="primary" disabled={Boolean(busy)} onClick={() => void action("merge-main")}>{busy === "merge-main" ? "合并中" : "确认合并"}</button></footer></section></div>}
		{confirmCleanup && selected && <div className="orchestration-confirm-backdrop" role="presentation"><section className="orchestration-confirm" role="dialog" aria-modal="true" aria-labelledby="cleanup-title"><h2 id="cleanup-title">清理 worktree 与分支</h2>{selected.status === "released_to_main" ? <p>该任务已合并至 <code>{config?.mainBranch || "main"}</code>，将移除对应 worktree 和任务分支。</p> : <p className="orchestration-cleanup-warning">此任务尚未合并至 <code>{config?.mainBranch || "main"}</code>。清理会删除 worktree 和任务分支，未合并提交将无法通过本页面恢复。</p>}<footer><button type="button" className="secondary" disabled={Boolean(busy)} onClick={() => setConfirmCleanup(false)}>取消</button><button type="button" className="danger" disabled={Boolean(busy)} onClick={() => void cleanup(selected.status !== "released_to_main")}>{busy === "cleanup" ? "清理中" : selected.status === "released_to_main" ? "确认清理" : "我确认未合并也清理"}</button></footer></section></div>}
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

function VerificationEvidence({ runs }: { runs: VerificationRun[] }) {
  if (!runs.length) return <p className="orchestration-empty">尚未记录验证命令。</p>;
  return <div className="orchestration-verification-list">{runs.map((run) => <details key={run.id} open={run.status === "failed"}><summary><span className={`verification-result ${run.status}`}>{run.status === "passed" ? "通过" : run.status === "failed" ? "失败" : "进行中"}</span><b>{phaseLabel(run.phase)}</b><time>{formatDate(run.completedAt || run.createdAt)}</time></summary><code>{run.command}</code>{run.reviewedSha && <small>SHA {shortSHA(run.reviewedSha)}</small>}{run.output && <pre>{run.output}</pre>}</details>)}</div>;
}
