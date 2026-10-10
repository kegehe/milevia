import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useNavigate, useParams, useSearchParams } from "react-router-dom";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { useProjectContext } from "../stores/useProjectStore";
import type { AgentID, Event, Message } from "../lib/types";
import { agentDisplayName, agentEntry, agentPermissionModes, permissionCopy, catalogAgentID, useAgentCatalog } from "../lib/agent-registry";
import type { Task, TaskDetail } from "../features/tasks/task-model";
import { markdownCodeComponents } from "../components/MarkdownCodeBlock";
import { ExternalLink } from "../components/ExternalLink";
import "../markdown.css";
import "../conversation.css";
import "../orchestration.css";

type OrchestrationConfig = { projectId: string; enabled: boolean; mainBranch: string; devBranch: string; agentId: AgentID; verificationCommands: string[]; maxFixRounds: number; frozenReason?: string };
type OrchestrationJob = { id: string; projectId: string; taskId: string; taskTitle?: string; taskDescription?: string; position: number; status: string; attempt?: number; baseDevSha?: string; targetBranch?: string; taskBranch?: string; worktreePath?: string; conversationId?: string; batchId?: string; humanDecision?: string; resourcesCleanedAt?: string; lastError?: string; createdAt?: string; updatedAt?: string; executionMode?: ExecutionMode };
type OrchestrationBatch = { id: string; name: string; conversationStrategy: "new" | "continue"; status: "not_started" | "active" | "needs_human" | "paused" | "awaiting_main" | "completed"; started: boolean; taskCount: number; completedCount: number; createdAt: string; updatedAt: string; executionMode?: ExecutionMode; targetBranch?: string };
// 计划的执行方式：worktree = 隔离工作树（默认，拉临时分支、完成后合并）；branch = 直接写入
// 项目工作目录里那个已存在的分支（不建分支、不提交、不合并）。
type ExecutionMode = "worktree" | "branch";
type GitBranchOption = { name: string; remote: boolean; current: boolean };
type ReleaseSnapshot = { id: string; projectId: string; devSha: string; branch: string; status: string; createdAt: string; confirmedAt?: string };
type ConversationHistory = { conversation?: { agentId: AgentID }; activeRunId?: string | null; messages: Message[]; events: Event[]; hasMore: boolean; nextCursor: string };

const runningStatuses = new Set(["preparing", "implementing", "checking"]);
// 新建编排任务时一次定好的执行配置：创建后写入项目编排配置，之后加入队列的任务都沿用快照。
// executionMode 与 targetBranch 是**计划级**的：它们只写进这个计划的子任务快照，不改项目配置。
type BatchPolicyDraft = { mainBranch: string; devBranch: string; agentId: AgentID; maxFixRounds: number; executionMode: ExecutionMode; targetBranch: string };
const refreshingStatuses = new Set(["queued", "preparing", "implementing", "checking"]);
const fallbackTaskTitle = "未命名任务";

const defaultBatchPolicy: BatchPolicyDraft = { mainBranch: "main", devBranch: "dev", agentId: "claude-code", maxFixRounds: 3, executionMode: "worktree", targetBranch: "" };

function statusLabel(status: string, targetBranch = "main") {
  const labels: Record<string, string> = { queued: "等待执行", preparing: "准备工作区", implementing: "Agent 执行中", checking: "收尾中", paused: "已暂停", stopped: "已停止", removing: "清理中", needs_human: "需要处理", awaiting_main: `待合并 ${targetBranch}`, integrated_to_dev: `待合并 ${targetBranch}`, released_to_main: `已合并 ${targetBranch}`, applied_to_branch: `已写入 ${targetBranch}` };
  return labels[status] || status;
}

function orchestrationActivityLabel(status: string, agentID: AgentID) {
  const agentName = agentDisplayName(agentID);
  if (status === "queued") return "任务正在队列中等待执行";
  if (status === "preparing") return "正在准备独立工作区";
  if (status === "implementing") return `${agentName} 正在处理任务`;
  if (status === "checking") return "正在提交实现并收尾";
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

// 与控制服务 validOrchestrationBranch 同规则：先在弹窗里拦下非法分支名，
// 否则用户会在提交后收到一句无法定位到字段的 400。
const orchestrationBranchPattern = /^[A-Za-z0-9][A-Za-z0-9._/-]{0,120}$/;
function validOrchestrationBranch(value: string) {
  return orchestrationBranchPattern.test(value) && !value.includes("..") && !value.endsWith("/");
}

function validMaxFixRounds(value: number) {
  return Number.isInteger(value) && value >= 1 && value <= 10;
}

function orchestrationPlanStatusLabel(batch: OrchestrationBatch) {
  // 未开始优先于一切：这时子任务全是 queued，按下面推导会落到「子任务推进中」，
  // 会让一个还没提交的草稿看起来正在跑。
  if (!batch.started) return "未开始";
  if (batch.taskCount === 0) return "暂无子任务";
  if (batch.completedCount === batch.taskCount) return "子任务已全部完成";
  if (batch.status === "needs_human") return "有子任务需要处理";
  if (batch.status === "paused") return "有子任务已暂停";
  if (["awaiting_main", "released_to_main", "integrated_to_dev"].includes(batch.status)) return "有子任务待合并";
  return "子任务推进中";
}

// 直接写入的计划必须一眼可辨：它的子任务改的是用户自己的工作目录，而且不会留下可回滚的
// 分支或提交。标记是**附加**信息，不替代进度文案——"有子任务待合并"这类状态照旧要说出来。
function orchestrationPlanModeLabel(batch: OrchestrationBatch) {
  if (batch.executionMode !== "branch") return "";
  return `直接写入 ${batch.targetBranch || "目标分支"}`;
}

function orchestrationPlanLabel(batch: OrchestrationBatch) {
  const mode = orchestrationPlanModeLabel(batch);
  const status = orchestrationPlanStatusLabel(batch);
  return mode ? `${mode} · ${status}` : status;
}

// 发布快照的状态沿用任务状态名：awaiting_main = 固定快照已生成、等用户合入稳定分支；
// released_to_main = 已确认稳定分支包含该快照，快照内任务已标记为已发布。
function releaseStatusLabel(status: string, mainBranch: string) {
  const labels: Record<string, string> = { awaiting_main: `待合入 ${mainBranch}`, released_to_main: `已合入 ${mainBranch}` };
  return labels[status] || status;
}

function OrchestrationConversationMessage({ message, agentID }: { message: Message; agentID: AgentID }) {
  const isUser = message.role === "user";
  const agentName = agentDisplayName(agentID);
  return <div className="timeline-entry message-entry"><article className={`message ${message.role}`}><header><span className="message-avatar">{isUser ? "你" : agentName.slice(0, 1).toUpperCase()}</span><b>{isUser ? "你" : agentName}</b><time>{formatDate(message.createdAt)}</time></header><div className="markdown"><ReactMarkdown remarkPlugins={[remarkGfm]} skipHtml components={{ ...markdownCodeComponents, a: ({ href, children }) => <ExternalLink href={href} target="_blank" rel="noreferrer">{children}</ExternalLink> }}>{message.content}</ReactMarkdown></div></article></div>;
}

function ScrollNavigationIcon({ direction }: { direction: "top" | "previous" | "next" | "bottom" }) {
  const edge = direction === "top" || direction === "bottom";
  const up = direction === "top" || direction === "previous";
  return <svg className="scroll-btn-icon" viewBox="0 0 24 24" aria-hidden="true">
    {edge && <path d={up ? "M6 5.5h12" : "M6 18.5h12"} />}
    <path d={up ? "M12 18V7M8.5 10.5 12 7l3.5 3.5" : "M12 6v11m-3.5-3.5L12 17l3.5-3.5"} />
  </svg>;
}

// 右栏「所属编排任务」区块：说明当前子任务属于哪个计划，并给出该计划的计划级动作。
// 「开始执行」只在计划还没开始时出现——那是用户要的那道闸门：加进队列的子任务不会自己跑。
// 计划级动作单独成块，不和上面的子任务级操作混在一排。区块的可访问名直接用标题
// （与「执行过程」一致），不再额外挂 aria-label，否则同一句话会被读两遍。
function OrchestrationPlanPanel({ batch, busyKey, onDelete, onStart }: { batch: OrchestrationBatch; busyKey: string; onDelete: () => void; onStart: () => void }) {
  // 「启动中」只在**这一次启动**进行中时写：原来传的是布尔（有任意操作在跑就为真），
  // 于是删别的计划、提交决策时，这个按钮也会写着"启动中" —— 而它其实没在启动（2026-09-29 复查）。
  // 禁用仍然按"任意操作进行中"保守处理。
  const starting = busyKey === `start-batch:${batch.id}`;
  const busy = Boolean(busyKey);
  return <section className="orchestration-plan-link"><header><h3>所属编排任务</h3><span>{orchestrationPlanLabel(batch)}</span></header><div className="orchestration-plan-link-body"><span className="orchestration-plan-link-main"><b title={batch.name}>{batch.name}</b><small>{batch.completedCount}/{batch.taskCount} 子任务</small></span>{!batch.started && <button type="button" className="primary" title="开始按顺序执行这个编排任务里的子任务" aria-label="开始执行" disabled={busy} onClick={onStart}>{starting ? "启动中" : "开始执行"}</button>}<button type="button" className="danger-text" title="删除这个编排任务（子任务会保留在「全部子任务」中）" aria-label="删除编排任务" disabled={busy} onClick={onDelete}>删除编排任务</button></div></section>;
}

export default function OrchestrationPage() {
  const agentOptions = useAgentCatalog();
  const { projectId } = useParams<{ projectId: string }>();
  const { api, projects, setError } = useProjectContext();
  const navigate = useNavigate();
  const [params, setParams] = useSearchParams();
  const [config, setConfig] = useState<OrchestrationConfig | null>(null);
  const [jobs, setJobs] = useState<OrchestrationJob[]>([]);
	const [batches, setBatches] = useState<OrchestrationBatch[]>([]);
	const [releases, setReleases] = useState<ReleaseSnapshot[]>([]);
  const [tasks, setTasks] = useState<Task[]>([]);
  const [detail, setDetail] = useState<TaskDetail | null>(null);
  const [detailLoading, setDetailLoading] = useState(false);
  const [detailError, setDetailError] = useState("");
  const [history, setHistory] = useState<ConversationHistory | null>(null);
  const [busy, setBusy] = useState("");
	const [confirmMerge, setConfirmMerge] = useState(false);
	const [confirmCleanup, setConfirmCleanup] = useState(false);
	const [confirmRelease, setConfirmRelease] = useState<ReleaseSnapshot | null>(null);
	const [confirmDeleteBatch, setConfirmDeleteBatch] = useState<OrchestrationBatch | null>(null);
	const [branchSettingsOpen, setBranchSettingsOpen] = useState(false);
	const [branchDraft, setBranchDraft] = useState({ mainBranch: "", devBranch: "" });
	const [closeLockingProcesses, setCloseLockingProcesses] = useState(false);
	const [batchComposerOpen, setBatchComposerOpen] = useState(false);
  const [batchName, setBatchName] = useState("");
	const [batchFilterID, setBatchFilterID] = useState("");
	const [conversationStrategy, setConversationStrategy] = useState<"new" | "continue">("new");
	const [batchPolicy, setBatchPolicy] = useState<BatchPolicyDraft>(defaultBatchPolicy);
	// 「直接写入」模式的目标分支下拉：只在打开弹窗时拉一次本地分支列表。
	const [branchOptions, setBranchOptions] = useState<GitBranchOption[]>([]);
	const [branchOptionsError, setBranchOptionsError] = useState("");
	const [decision, setDecision] = useState("");
  const mounted = useRef(true);
  const tasksRef = useRef<Task[]>([]);
  const overviewRequestVersion = useRef(0);
  const selectedRequestVersion = useRef(0);
  const selectedRequestInFlight = useRef(0);
  const historyRef = useRef<ConversationHistory | null>(null);
  const conversationScrollRef = useRef<HTMLDivElement>(null);
  const userMessageElements = useRef(new Map<string, HTMLDivElement>());
  const [selectedEnqueue, setSelectedEnqueue] = useState<Set<string>>(new Set());
  const selectedID = params.get("job") || "";
  const scopedJobs = useMemo(() => batchFilterID ? jobs.filter((job) => job.batchId === batchFilterID) : jobs, [batchFilterID, jobs]);
  const selected = scopedJobs.find((job) => job.id === selectedID) || scopedJobs[0] || null;
  const taskTitleByID = useMemo(() => {
    const titles = new Map(tasks.map((task) => [task.id, task.title || fallbackTaskTitle]));
    jobs.forEach((job) => { if (!titles.has(job.taskId) && job.taskTitle) titles.set(job.taskId, job.taskTitle); });
    return titles;
  }, [jobs, tasks]);

  const loadOverview = useCallback(async () => {
    if (!projectId) return;
    const requestVersion = ++overviewRequestVersion.current;
    const [nextConfig, nextJobs, nextTasks, nextBatches, nextReleases] = await Promise.all([
      api<OrchestrationConfig>(`/api/projects/${projectId}/orchestration/config`),
      api<OrchestrationJob[]>(`/api/projects/${projectId}/orchestration`),
      api<Task[]>(`/api/projects/${projectId}/tasks`),
		api<OrchestrationBatch[]>(`/api/projects/${projectId}/orchestration/batches`),
		api<ReleaseSnapshot[]>(`/api/projects/${projectId}/orchestration/releases`),
    ]);
    if (!mounted.current || requestVersion !== overviewRequestVersion.current) return;
    setConfig(nextConfig);
    setJobs(nextJobs);
    setTasks(nextTasks);
		setBatches(nextBatches);
		setReleases(nextReleases);
    tasksRef.current = nextTasks;
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
          const fallbackTask = tasksRef.current.find((task) => task.id === selected.taskId);
          if (fallbackTask) {
            setDetail({ ...fallbackTask, canDispatch: false, runs: [], events: [], verificationRuns: [] });
          } else if (selected.taskTitle) {
            const now = new Date().toISOString();
            setDetail({ id: selected.taskId, title: selected.taskTitle, description: selected.taskDescription || "", priority: "normal", pinned: false, position: selected.position, status: "todo", dependsOn: [], blockedBy: [], blocks: [], canDispatch: false, runs: [], events: [], verificationRuns: [], createdAt: selected.createdAt || now, updatedAt: selected.updatedAt || now });
          } else {
            setDetail(null);
          }
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
    // 未开始的计划里，子任务会一直停在 queued；只有用户点「开始执行」才可能变化，为它每 5 秒
    // 空转轮询没有意义（一个放着不动的草稿会让页面永久刷新）。
    const startedBatchIDs = new Set(batches.filter((batch) => batch.started).map((batch) => batch.id));
    const needsRefresh = (job: OrchestrationJob) => refreshingStatuses.has(job.status) && (job.status !== "queued" || !job.batchId || startedBatchIDs.has(job.batchId));
    if (!jobs.some(needsRefresh)) return;
    const timer = window.setInterval(() => {
      void loadOverview().catch(() => undefined);
      if (selected && needsRefresh(selected)) void loadSelected("latest").catch(() => undefined);
    }, 5_000);
    return () => window.clearInterval(timer);
  }, [jobs, batches, loadOverview, loadSelected, selected?.id, selected?.status]);

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
	const visibleJobs = scopedJobs;
	const activeBatch = batchFilterID ? batches.find((batch) => batch.id === batchFilterID) || null : null;
	// 右栏「所属编排任务」按子任务反查它属于哪个计划，而不是用 batchFilterID：筛选可以指向
	// 另一个计划，而这里要说清的正是「当前这个子任务属于谁」。子任务脱组后 batch_id 为空，
	// 没有可删的计划，整块不显示。
	const selectedBatch = selected?.batchId ? batches.find((batch) => batch.id === selected.batchId) || null : null;
	// 右栏要显示的计划。空计划（一个子任务都没有）没有任何子任务可选中，右栏拿不到 selected，
	// 那时只有①栏正在筛选的计划能说明用户指的是谁——不兜这一层，空计划就没有任何删除入口
	// （②栏的归档按钮已经移除，①栏行也恢复成纯选择），废弃计划会永久堆积。
	// 过滤视图下 selectedBatch 与 activeBatch 恒等，这个兜底不会指向另一个计划。
	const panelBatch = selectedBatch ?? activeBatch;
	// 给每个列出的子任务提供“上移/下移”。排序接口要求一次提交整条项目内排队子任务，
	// 所以箭头仅在当前可见组恰好就是全部可排序项时显示（单计划或默认全部列表均满足）。
	const reorderableTaskIDs = visibleJobs
		.filter((job) => job.status === "queued" || job.status === "paused")
		.sort((left, right) => left.position - right.position)
		.map((job) => job.taskId);
	const showReorderArrows = reorderableTaskIDs.length > 0 && reorderableTaskIDs.length === queuedTaskIDs.length;
	// 空态可见性：还没有任何「编排任务」/子任务时隐藏子任务分区；候选分区仅在
	// 确实还有可选任务时出现，避免左侧一列堆叠三行无意义的空提示。
	const hasPlans = batches.length > 0;
	const showSubtasksPane = hasPlans || jobs.length > 0;
	// 候选任务分区在「有任务可选」时出现；没有可加入的编排任务时按钮禁用并说明原因。
	const showCandidatesPane = enqueueableTasks.length > 0;
	const planEmptyHint = enqueueableTasks.length > 0
		? "还没有编排任务：点击「+ 新建」，再回到下方候选任务勾选加入。"
		: "还没有编排任务：可先到「管理任务」新建任务，再回来创建。";
	const candidatesEmptyHint = hasPlans ? "请先在上方选中一个编排任务。" : "请先在上方创建一个编排任务。";
	// 执行配置的本地校验：与控制服务的校验规则一致，避免提交后才收到 400。
	const mainBranchValid = validOrchestrationBranch(batchPolicy.mainBranch.trim());
	const devBranchValid = validOrchestrationBranch(batchPolicy.devBranch.trim());
	const maxFixRoundsValid = validMaxFixRounds(batchPolicy.maxFixRounds);
	// 「直接写入」的前置条件。服务端只接受**服务端本机 runner** 的项目跑自动编排
	// （app.go isLocalRunnerID：Windows 服务端是 ""/windows-local，其余平台是 ""/wsl-local），
	// 判据按平台而定，所以这里读服务端下发的 project.localRunner，不自己按 runner id 拼。
	// 自己拼的代价不是 400 而是派发期 needs_human：Windows 服务端上的 wsl-local 项目会先
	// 过这道闸门、建出计划，然后在 orchestration.go 的派发前置检查上失败并冻结整个项目队列。
	// 旧服务端不下发这一位时取不到值 → 判为"不是本机"（禁用这个模式）：宁可少给一个模式，
	// 也不要在"不知道"的时候给出一个可能必失败的模式。
	const batchProject = projects.find((item) => item.id === projectId) || null;
	const localRunner = !batchProject || batchProject.localRunner;
	const currentBranchName = branchOptions.find((branch) => branch.current)?.name || "";
	const targetBranchDraft = batchPolicy.targetBranch.trim();
	const directModeSelected = batchPolicy.executionMode === "branch";
	const directModeBlockedReason = !localRunner
		? "「直接写入」只支持运行在服务端本机运行器上的项目；当前项目在远端或其他运行器上（例如 Windows 服务端上的 WSL 项目）。"
		: branchOptionsError
			? `无法读取项目分支列表：${branchOptionsError}`
			: branchOptions.length === 0
				? "项目里还没有可选的本地分支，无法指定目标分支。"
				: "";
	// 选了非当前检出的分支不硬拦（用户可能在弹窗打开之后才切分支），但一直提示；
	// 服务端派发前以当时的 HEAD 为准，不一致会停下并要求用户自己切。
	const targetBranchMismatch = directModeSelected && Boolean(currentBranchName) && Boolean(targetBranchDraft) && targetBranchDraft !== currentBranchName;
	const batchPolicyIssue = !mainBranchValid
		? "稳定分支格式无效：需以字母或数字开头，只能包含字母、数字、. _ / -，且不能以 / 结尾或包含 .."
		: !devBranchValid
			? "开发分支格式无效：需以字母或数字开头，只能包含字母、数字、. _ / -，且不能以 / 结尾或包含 .."
			: !maxFixRoundsValid
				? "最大修复轮次需为 1~10 之间的整数。"
				: directModeSelected && directModeBlockedReason
					? directModeBlockedReason
					: directModeSelected && !validOrchestrationBranch(targetBranchDraft)
						? "目标分支无效：请从项目已有的本地分支里选择。"
						: "";
	const subtasksEmptyHint = activeBatch
		? enqueueableTasks.length
			? "该编排任务还没有子任务：可勾选下方候选任务加入。"
			: "该编排任务还没有子任务：可先到「管理任务」新建任务后再加入。"
		: "还没有子任务。";
	// 发布快照从开发分支取 SHA、以稳定分支作为合入目标；配置缺失或队列冻结时先禁用创建，
	// 否则用户只会拿到一个无法在界面上定位原因的 409。
	const mainBranchName = config?.mainBranch || defaultBatchPolicy.mainBranch;
	const devBranchName = config?.devBranch || defaultBatchPolicy.devBranch;
	const releaseBlockedReason = !config ? "编排配置尚未加载完成" : config.frozenReason ? "队列已冻结，解除冻结后才能创建发布快照" : "";
	const branchDraftIssue = !validOrchestrationBranch(branchDraft.mainBranch.trim())
		? "稳定分支格式无效：需以字母或数字开头，只能包含字母、数字、. _ / -，且不能以 / 结尾或包含 .."
		: !validOrchestrationBranch(branchDraft.devBranch.trim())
			? "开发分支格式无效：需以字母或数字开头，只能包含字母、数字、. _ / -，且不能以 / 结尾或包含 .."
			: "";

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
  }, [enqueueableTasks]);
  const selectJob = (job: OrchestrationJob) => {
    if (job.batchId && batches.some((batch) => batch.id === job.batchId)) setBatchFilterID(job.batchId);
    selectedRequestVersion.current += 1;
    setDetail(null);
    setDetailLoading(true);
    setDetailError("");
    historyRef.current = null;
    setHistory(null);
    setParams({ job: job.id });
  };
  // 三级队列视图：最上方选择一个「编排任务」，中间列出其全部子任务。
  const planOrderedJobs = (batchID: string) => jobs.filter((job) => job.batchId === batchID).sort((left, right) => left.position - right.position);
  // 选中编排任务后自动打开其实时状态最适合关注的一个子任务（正在运行的优先，
  // 否则最新入队待执行的），避免右侧对话区停留在上一个编排任务的内容上。
  const selectPlan = (batchID: string) => {
    const job = selected?.batchId === batchID && selected ? selected : undefined;
    const candidates = planOrderedJobs(batchID);
    const target = job || candidates.find((item) => runningStatuses.has(item.status) || ["queued", "paused"].includes(item.status)) || candidates[0];
    setBatchFilterID(batchID);
    if (target) {
      selectedRequestVersion.current += 1;
      setDetail(null);
      setDetailLoading(true);
      setDetailError("");
      historyRef.current = null;
      setHistory(null);
      setParams({ job: target.id });
    }
  };
  // 首次进入且未通过 URL 指定任务时，自动聚焦最近创建的编排任务，让中间区域
  // 有内容可展示；深度链接「job」或用户已选择时均不干扰。
  useEffect(() => {
    if (batchFilterID || selectedID) return;
    if (!batches.length) return;
    // 若仍存在未归入任何「编排任务」的旧式单条入队任务，则留在“全部子任务”视图，
    // 避免它们被默认聚焦最近计划而暂时隐藏；新建的项目通常都是批量编排、走不到这里。
    if (!jobs.some((job) => !job.batchId)) setBatchFilterID(batches[0].id);
  }, [batchFilterID, batches, jobs, selectedID]);
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
  };
  // 打开弹窗时用当前项目编排配置预填，用户可在这一次性改掉执行策略。
  // 配置还没加载成功时必须先补取一次：否则会拿默认值预填，提交后把用户已有的
  // 分支 / Agent / 修复轮次静默覆盖成默认值（旧版靠「未启用 → 按钮禁用」挡住了这一步）。
  const openBatchComposer = async () => {
    let current = config;
    if (!current) {
      if (!projectId) return;
      try {
        current = await api<OrchestrationConfig>(`/api/projects/${projectId}/orchestration/config`);
        if (!mounted.current) return;
        setConfig(current);
      } catch (cause) {
        setError(cause instanceof Error ? cause.message : "编排配置尚未加载完成，请稍后重试。");
        return;
      }
    }
    if (!current) return;
    setBatchName("");
    setConversationStrategy("new");
    setBatchPolicy({
      mainBranch: current.mainBranch || defaultBatchPolicy.mainBranch,
      devBranch: current.devBranch || defaultBatchPolicy.devBranch,
      agentId: current.agentId || defaultBatchPolicy.agentId,
      maxFixRounds: current.maxFixRounds || defaultBatchPolicy.maxFixRounds,
      executionMode: "worktree",
      targetBranch: "",
    });
    // 本地分支列表只服务「直接写入」下拉，失败或慢都不该拖住建计划这一步：弹窗先开，
    // 列表在后台补上。拉不到就禁用「直接写入」并说明原因，比让用户选完才吃 409 好。
    setBranchOptions([]);
    setBranchOptionsError("");
    setBatchComposerOpen(true);
    void (async () => {
      try {
        const branches = await api<GitBranchOption[]>(`/api/projects/${projectId}/git/branches`);
        if (!mounted.current) return;
        setBranchOptions(branches.filter((branch) => !branch.remote));
      } catch (cause) {
        if (!mounted.current) return;
        setBranchOptionsError(cause instanceof Error ? cause.message : "无法读取项目分支列表");
      }
    })();
  };
  const closeBatchComposer = () => {
    setBatchName("");
    setConversationStrategy("new");
    setBatchComposerOpen(false);
  };
  // 新建编排任务只收集名称、上下文继承和执行配置：任务在创建后由用户在候选列表里勾选加入。
  const createBatch = async () => {
    const name = batchName.trim();
    if (!projectId || !name) { setError("请填写编排任务名称"); return; }
    setBusy("batch");
    try {
      const batch = await api<OrchestrationBatch>(`/api/projects/${projectId}/orchestration/batches`, {
        method: "POST",
        body: JSON.stringify({ name, conversationStrategy, mainBranch: batchPolicy.mainBranch.trim(), devBranch: batchPolicy.devBranch.trim(), agentId: batchPolicy.agentId, maxFixRounds: batchPolicy.maxFixRounds, executionMode: batchPolicy.executionMode, targetBranch: batchPolicy.executionMode === "branch" ? batchPolicy.targetBranch.trim() : "" }),
      });
      closeBatchComposer();
      // 计划此时已经建成：刷新失败只能报「刷新失败」，否则用户会以为没建上而重复创建。
      try { await loadOverview(); } catch (cause) { setError(`编排任务已创建，但队列列表刷新失败：${cause instanceof Error ? cause.message : "请点右上角刷新"}`); }
      // 建完立即聚焦新计划，用户接着就能在候选任务里勾选加入。
      setBatchFilterID(batch.id);
    } catch (cause) { setError(cause instanceof Error ? cause.message : "无法创建编排任务"); }
    finally { if (mounted.current) setBusy(""); }
  };
  // 把勾选的候选任务追加到当前选中的编排任务；队列顺序取候选列表顺序，与控制台看到的顺序一致。
  const addCandidatesToBatch = async () => {
    if (!projectId || !activeBatch || selectedEnqueue.size === 0) return;
    const taskIDs = enqueueableTasks.filter((task) => selectedEnqueue.has(task.id)).map((task) => task.id);
    if (!taskIDs.length) return;
    if (await queueAction("add-tasks", `/api/projects/${projectId}/orchestration/batches/${activeBatch.id}/tasks`, { method: "POST", body: JSON.stringify({ taskIds: taskIDs }) })) setSelectedEnqueue(new Set());
  };
	// 放行一个计划：闸门一开，调度器才会按 queue_position 依次取它的子任务。与删除同一形态——
	// 接口一返回成功就按成功收尾，刷新失败只报「刷新失败」，不能把已经生效的开始误报成失败
	// （那会让用户反复点，而重复点击本身是幂等的）。
	const startBatch = async (batch: OrchestrationBatch) => {
		if (!projectId) return;
		setBusy(`start-batch:${batch.id}`);
		try {
			await api(`/api/projects/${projectId}/orchestration/batches/${batch.id}/start`, { method: "POST", body: "{}" });
			setBatches((previous) => previous.map((item) => (item.id === batch.id ? { ...item, started: true, status: "active" } : item)));
			try { await Promise.all([loadOverview(), loadSelected()]); }
			catch (cause) { setError(`编排任务已开始执行，但队列列表刷新失败：${cause instanceof Error ? cause.message : "请点右上角刷新"}`); }
		} catch (cause) { setError(cause instanceof Error ? cause.message : "无法开始执行编排任务"); }
		finally { if (mounted.current) setBusy(""); }
	};
	// 删除编排任务只摘掉分组标签：子任务连同它们的 worktree、分支和执行对话都保留，
	// 脱组后会落到「全部子任务」视图。成功后必须清掉 batchFilterID——留着会让列表按
	// 一个已不存在的 id 过滤成空。是否有未结束子任务由服务端判定（前端分不清
	// 「只有 stopped」和「有 paused」，两者在 batch.status 里是同一个桶）。
	// 刻意不走 queueAction：它把「刷新失败」也归到操作失败里，而这里刷新失败只是列表
	// 陈旧，会让已经删掉的计划继续占着筛选——②栏空掉、脱组的子任务看不见。删除一旦
	// 返回成功就按成功收尾，刷新失败单独报，与 createBatch 的处理一致。
	const deleteBatch = async (batch: OrchestrationBatch) => {
		if (!projectId) return;
		setBusy(`delete-batch:${batch.id}`);
		try {
			await api(`/api/projects/${projectId}/orchestration/batches/${batch.id}`, { method: "DELETE" });
			// 本地先把这次删除的效果落下来——服务端做的正是同一件事（删 batch 行 + 把子任务
			// batch_id 清空）。刷新是 best-effort，若只靠刷新，刷新失败时界面会一直留着一个
			// 已经不存在的计划，再点删除只会拿到 404；顺带也省掉「等刷新才有反应」的延迟。
			setBatches((previous) => previous.filter((item) => item.id !== batch.id));
			setJobs((previous) => previous.map((job) => (job.batchId === batch.id ? { ...job, batchId: "" } : job)));
			setBatchFilterID("");
			setConfirmDeleteBatch(null);
			try { await Promise.all([loadOverview(), loadSelected()]); }
			catch (cause) { setError(`编排任务已删除，但队列列表刷新失败：${cause instanceof Error ? cause.message : "请点右上角刷新"}`); }
		} catch (cause) { setError(cause instanceof Error ? cause.message : "无法删除编排任务"); }
		finally { if (mounted.current) setBusy(""); }
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
	const dequeueSelected = async () => {
		if (!selected || !["queued", "paused", "stopped"].includes(selected.status)) return;
		await queueAction(`dequeue:${selected.taskId}`, `/api/tasks/${selected.taskId}/orchestration/dequeue`, { method: "DELETE" });
	};
	// 发布快照：把开发分支当前提交固定成不可变分支，用户自行合入稳定分支后再回来确认。
	const createRelease = async () => {
		if (!projectId) return;
		await queueAction("release", `/api/projects/${projectId}/orchestration/releases`, { method: "POST" });
	};
	const confirmReleaseMerged = async () => {
		if (!projectId || !confirmRelease) return;
		if (await queueAction(`release-confirm:${confirmRelease.id}`, `/api/projects/${projectId}/orchestration/releases/${confirmRelease.id}/confirm`, { method: "POST" })) setConfirmRelease(null);
	};
	const openBranchSettings = () => {
		if (!config) return;
		setBranchDraft({ mainBranch: config.mainBranch || defaultBatchPolicy.mainBranch, devBranch: config.devBranch || defaultBatchPolicy.devBranch });
		setBranchSettingsOpen(true);
	};
	// 开发分支是发布快照的来源，稳定分支是快照的合入目标；两者都不能只靠「新建编排任务」
	// 顺带修改，否则已有计划的项目无法调整验收来源分支。
	const saveBranchSettings = async () => {
		if (!projectId || !config) return;
		const mainBranch = branchDraft.mainBranch.trim();
		const devBranch = branchDraft.devBranch.trim();
		if (!validOrchestrationBranch(mainBranch) || !validOrchestrationBranch(devBranch)) return;
		setBusy("branches");
		try {
			const saved = await api<OrchestrationConfig>(`/api/projects/${projectId}/orchestration/config`, { method: "PUT", body: JSON.stringify({ ...config, mainBranch, devBranch }) });
			if (!mounted.current) return;
			setConfig(saved);
			setBranchSettingsOpen(false);
			// 分支名同时决定快照来源与任务合并目标，保存后必须让列表与标签同步。
			try { await loadOverview(); } catch (cause) { setError(`分支已保存，但列表刷新失败：${cause instanceof Error ? cause.message : "请点右上角刷新"}`); }
		} catch (cause) { setError(cause instanceof Error ? cause.message : "无法保存编排分支"); }
		finally { if (mounted.current) setBusy(""); }
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
    <h1 className="orchestration-page-title">自动编排</h1>
    <header className="orchestration-page-head">
      <div className="orchestration-head-summary" role="region" aria-label="编排概览"><div><span>执行中</span><b>{counts.active}</b></div><div><span>等待队列</span><b>{counts.waiting}</b></div><div className={counts.blocked ? "attention" : ""}><span>需要处理</span><b>{counts.blocked}</b></div><div className="detail"><span>编排任务</span><b>{batches.length} 个</b></div><div className="detail"><span>新任务目标分支</span><b title={config?.mainBranch || "main"}>{config?.mainBranch || "main"}</b></div><div className="detail"><span>最大修复</span><b>{config?.maxFixRounds ?? "--"} 轮</b></div></div>
      <div className="orchestration-head-actions"><button type="button" className="primary" disabled={Boolean(busy)} onClick={() => void openBatchComposer()}>新建编排任务</button><button type="button" className="secondary" onClick={() => navigate(`/projects/${projectId}/tasks`)}>管理任务</button></div>
    </header>
    {config?.frozenReason && <div className="orchestration-freeze" role="alert"><b>队列已冻结</b><span>{config.frozenReason}</span></div>}
    <div className="orchestration-console">
      <aside className="orchestration-queue" aria-label="自动编排 — 三级队列视图">
  <header className="orchestration-queue-toolbar"><div className="orchestration-queue-toolbar-top"><h2>自动编排</h2><span>{jobs.length} 条队列记录 · {batches.length} 个编排任务</span></div><button type="button" title="刷新队列" aria-label="刷新队列" disabled={Boolean(busy)} onClick={() => void loadOverview()}>↻</button></header>
  <section className="orchestration-row orchestration-planpane" aria-labelledby="orchestration-plan-title">
    <header><div><h3 id="orchestration-plan-title">① 编排任务</h3>{batches.length > 0 && <span>{batches.length} 个</span>}</div><button type="button" className="primary" title="新建编排任务" aria-label="新建编排任务" disabled={Boolean(busy)} onClick={() => void openBatchComposer()}>+ 新建</button></header>
    {batches.length ? <ol className="orchestration-planlist">{batches.map((batch) => <li key={batch.id}><button type="button" className={`orchestration-planitem${batchFilterID === batch.id ? " selected" : ""}`} disabled={Boolean(busy)} onClick={() => void selectPlan(batch.id)} title={batch.name}><span className={`orchestration-status-dot ${batch.status}`} /><span className="orchestration-planitem-main"><b>{batch.name}</b><small>{orchestrationPlanLabel(batch)}</small></span><span className="orchestration-planitem-count">{batch.completedCount}/{batch.taskCount}</span></button></li>)}</ol> : <p className="orchestration-empty">{planEmptyHint}</p>}
  </section>
  {showSubtasksPane && <section className="orchestration-row orchestration-subtasks" aria-labelledby="orchestration-subtasks-title">
    <header><div className="orchestration-subtasks-title"><h3 id="orchestration-subtasks-title">② 子任务</h3></div><div className="orchestration-subtasks-head-actions">{visibleJobs.length > 0 && <span>{visibleJobs.length} 项</span>}</div></header>
    {visibleJobs.length === 0 ? <p className="orchestration-empty">{subtasksEmptyHint}</p> : <ol className="orchestration-joblist">{visibleJobs.map((job) => {
      const queueable = job.status === "queued" || job.status === "paused";
      const removable = queueable || job.status === "stopped";
      const reorderIndex = reorderableTaskIDs.indexOf(job.taskId);
      return <li key={job.id}><button type="button" className={`${selected?.id === job.id ? "selected " : ""}${removable ? "has-queue-actions" : ""}`} aria-current={selected?.id === job.id} onClick={() => selectJob(job)}><span className={`orchestration-status-dot ${job.status}`} /><span className="orchestration-job-name"><b>#{job.position} {taskTitleByID.get(job.taskId) || fallbackTaskTitle}</b><small>{statusLabel(job.status, job.targetBranch)}{job.attempt ? ` · 第 ${job.attempt} 轮` : ""}</small></span>{job.lastError && <i title={job.lastError}>!</i>}</button>{removable && <div className="orchestration-queue-actions">{showReorderArrows && reorderIndex > -1 && <>
        <button type="button" title="上移" aria-label="上移" disabled={Boolean(busy) || reorderIndex <= 0} onClick={() => void moveJob(job.taskId, "up")}>↑</button>
        <button type="button" title="下移" aria-label="下移" disabled={Boolean(busy) || reorderIndex < 0 || reorderIndex >= reorderableTaskIDs.length - 1} onClick={() => void moveJob(job.taskId, "down")}>↓</button>
      </>}<button type="button" className="danger" title="移出队列" aria-label="移出队列" disabled={Boolean(busy) || !removable} onClick={() => void queueAction(`dequeue:${job.taskId}`, `/api/tasks/${job.taskId}/orchestration/dequeue`, { method: "DELETE" })}>{busy === `dequeue:${job.taskId}` ? "…" : "×"}</button></div>}</li>;
    })}</ol>}
  </section>}
  {showCandidatesPane && <section className="orchestration-row orchestration-candidates" aria-labelledby="orchestration-candidates-title">
    <header><h3 id="orchestration-candidates-title">③ 候选任务</h3><div className="orchestration-candidates-actions">{activeBatch ? <span className="orchestration-candidates-target" title={activeBatch.name}>加入「{activeBatch.name}」</span> : <span className="orchestration-candidates-target muted">{candidatesEmptyHint}</span>}<button type="button" className="secondary" title={activeBatch ? `把勾选的任务加入「${activeBatch.name}」` : candidatesEmptyHint} disabled={Boolean(busy) || !activeBatch || selectedEnqueue.size === 0} onClick={() => void addCandidatesToBatch()}>{busy === "add-tasks" ? "加入中" : selectedEnqueue.size ? `加入 (${selectedEnqueue.size})` : "加入"}</button></div></header><ul>{enqueueableTasks.map((task) => { const summary = candidateSummary(task); return <li key={task.id}><label title={summary}><input type="checkbox" disabled={Boolean(busy) || !activeBatch} checked={selectedEnqueue.has(task.id)} onChange={() => toggleEnqueue(task.id)} /><span>{summary}</span></label></li>; })}</ul>
  </section>}
  <section className="orchestration-row orchestration-releases" aria-labelledby="orchestration-release-title">
    <header><div><h3 id="orchestration-release-title">④ 发布快照</h3>{releases.length > 0 && <span>{releases.length} 个</span>}</div><button type="button" className="primary" title={releaseBlockedReason || `把开发分支 ${devBranchName} 当前提交固定成验收快照`} disabled={Boolean(busy) || Boolean(releaseBlockedReason)} onClick={() => void createRelease()}>{busy === "release" ? "创建中" : "+ 快照"}</button></header>
    <div className="orchestration-release-source"><span>来源分支</span><code title={devBranchName}>{devBranchName}</code><button type="button" title={config ? "修改稳定分支与开发分支" : "编排配置尚未加载完成"} disabled={Boolean(busy) || !config} onClick={openBranchSettings}>设置</button></div>
    {releases.length === 0 ? <p className="orchestration-empty">还没有发布快照：创建后把快照分支合入 {mainBranchName}，再回来确认，快照内的任务才会标记为已发布。</p> : <ol className="orchestration-release-list">{releases.map((release) => <li key={release.id}>
      <div className="orchestration-release-head"><b title={release.branch}>{release.branch}</b><span className={`orchestration-release-state ${release.status}`}>{releaseStatusLabel(release.status, mainBranchName)}</span></div>
      <small><code title={release.devSha}>{shortSHA(release.devSha)}</code>{release.status === "released_to_main" ? ` · 确认于 ${formatDate(release.confirmedAt)}` : ` · 创建于 ${formatDate(release.createdAt)}`}</small>
      {release.status === "awaiting_main" && <button type="button" className="primary" disabled={Boolean(busy)} onClick={() => setConfirmRelease(release)}>确认已合入 {mainBranchName}</button>}
    </li>)}</ol>}
  </section>
</aside>
      <main className="orchestration-conversation">{selected ? <>
        <header><div><h2>完整对话</h2><span>{messages.length} 条消息</span></div><a href={selected.conversationId ? `/projects/${projectId}/conversations/${selected.conversationId}?readonly=true` : undefined} onClick={(event) => { if (!selected.conversationId) event.preventDefault(); }} aria-disabled={!selected.conversationId}>在对话页打开</a></header>
        <div className="orchestration-conversation-content" ref={conversationScrollRef} onScroll={updateCurrentUserMessageIndex}><section className="timeline orchestration-conversation-timeline">{messages.length ? messages.map((message) => <div key={message.id} ref={message.role === "user" ? (element) => { if (element) userMessageElements.current.set(message.id, element); else userMessageElements.current.delete(message.id); } : undefined}><OrchestrationConversationMessage message={message} agentID={selectedAgentID} /></div>) : <p className="orchestration-empty">暂无可显示的执行对话。</p>}{activityLabel && <div className="run-indicator orchestration-run-indicator" role="status"><span></span>{activityLabel}</div>}</section></div>
        <div className="scroll-buttons orchestration-scroll-buttons"><button type="button" className="scroll-btn scroll-to-top" title="回到顶部" aria-label="回到顶部" onClick={scrollToTop}><ScrollNavigationIcon direction="top" /></button><button type="button" className="scroll-btn scroll-to-previous-message" title="上一条我的消息" aria-label="上一条我的消息" disabled={currentUserMessageIndex <= 0} onClick={() => scrollToUserMessage(currentUserMessageIndex - 1)}><ScrollNavigationIcon direction="previous" /></button><button type="button" className="scroll-btn scroll-to-next-message" title="下一条我的消息" aria-label="下一条我的消息" disabled={currentUserMessageIndex < 0 || currentUserMessageIndex >= userMessages.length - 1} onClick={() => scrollToUserMessage(currentUserMessageIndex + 1)}><ScrollNavigationIcon direction="next" /></button><button type="button" className="scroll-btn scroll-to-bottom" title="回到底部" aria-label="回到底部" onClick={scrollToBottom}><ScrollNavigationIcon direction="bottom" /></button></div>
      </> : <div className="orchestration-empty-main"><h2>选择一个编排任务</h2><p>从左侧队列查看完整对话和任务详情。</p></div>}</main>
      <aside className="orchestration-detail" aria-label="任务详情">{selected ? <>
        <header className="orchestration-detail-head"><div><div className="orchestration-detail-kicker"><span className={`orchestration-status-tag ${selected.status}`}>{statusLabel(selected.status, selected.targetBranch)}</span><time>更新于 {formatDate(selected.updatedAt)}</time></div><h2>{detail?.title || (detailLoading ? "加载任务详情中..." : "任务详情不可用")}</h2><p>{detail?.description || (detailLoading ? "正在加载任务详情。" : detailError || "暂时无法获取该任务详情。")}</p>{detailError && <button type="button" className="orchestration-detail-retry" onClick={() => void loadSelected()}>重试</button>}</div><div className="orchestration-detail-actions">{["queued", "preparing", "implementing", "checking"].includes(selected.status) && <button type="button" className="secondary" disabled={Boolean(busy)} onClick={() => void action("pause")}>{busy === "pause" ? "暂停中" : "暂停"}</button>}{["paused", "stopped"].includes(selected.status) && <button type="button" className="primary" disabled={Boolean(busy)} onClick={() => void action("resume")}>{busy === "resume" ? "处理中" : "继续执行"}</button>}{["preparing", "implementing", "checking", "paused", "needs_human"].includes(selected.status) && <button type="button" className="danger-text" disabled={Boolean(busy)} onClick={() => void action("stop")}>{busy === "stop" ? "停止中" : "停止"}</button>}{["awaiting_main", "integrated_to_dev"].includes(selected.status) && selected.executionMode !== "branch" && <button type="button" className="primary" disabled={Boolean(busy)} onClick={() => setConfirmMerge(true)}>合并至 {selected.targetBranch || "目标分支"}</button>}{selected.taskBranch && selected.executionMode !== "branch" && !selected.resourcesCleanedAt && ["released_to_main", "stopped", "needs_human"].includes(selected.status) && <button type="button" className="secondary" disabled={Boolean(busy)} onClick={() => setConfirmCleanup(true)}>清理资源</button>}{["queued", "paused", "stopped"].includes(selected.status) && <button type="button" className="danger-text" disabled={Boolean(busy)} onClick={() => void dequeueSelected()}>{busy === `dequeue:${selected.taskId}` ? "移出中" : "移出队列"}</button>}</div></header>
        {selected.lastError && <div className="orchestration-job-error"><b>需要处理</b><span>{selected.lastError}</span></div>}
				{selected.status === "needs_human" && <section className="orchestration-decision"><h3>需要人工决策</h3><p>提交的内容会写入本次编排对话，并作为下一次执行的上下文。</p><textarea value={decision} disabled={Boolean(busy)} placeholder="说明业务规则、取舍或下一步处理方式" onChange={(event) => setDecision(event.target.value)} /><button type="button" className="primary" disabled={Boolean(busy) || !decision.trim()} onClick={() => void submitDecision()}>{busy === "decision" ? "提交中" : "提交决策并继续"}</button></section>}
        {selected.executionMode === "branch" ? <section className="orchestration-facts" aria-label="Git 记录"><div><span>执行方式</span><code>直接写入</code></div><div><span>目标分支</span><code title={selected.targetBranch}>{selected.targetBranch || "--"}</code></div><div><span>基线 SHA</span><code title={selected.baseDevSha}>{shortSHA(selected.baseDevSha)}</code></div><div><span>已执行轮次</span><b>{selected.attempt || 0}</b></div></section> : <section className="orchestration-facts" aria-label="Git 记录"><div><span>任务分支</span><code title={selected.taskBranch}>{selected.taskBranch || "--"}</code></div><div><span>基线 SHA</span><code title={selected.baseDevSha}>{shortSHA(selected.baseDevSha)}</code></div><div><span>工作区</span><code title={selected.worktreePath}>{selected.worktreePath || "已清理"}</code></div><div><span>已执行轮次</span><b>{selected.attempt || 0}</b></div></section>}
        {selected.executionMode === "branch" && selected.status === "applied_to_branch" && <p className="orchestration-direct-note" role="note">改动就在你的项目工作目录里，本页面不会替你提交；确认无误后在任务页验收。</p>}
        {panelBatch && <OrchestrationPlanPanel batch={panelBatch} busyKey={busy} onDelete={() => setConfirmDeleteBatch(panelBatch)} onStart={() => void startBatch(panelBatch)} />}
        <section className="orchestration-timeline"><header><h3>执行过程</h3><span>{detail?.events.length || 0} 条事件</span></header>{detail?.events.length ? <ol>{detail.events.slice().reverse().map((event) => <li key={event.id}><time>{formatDate(event.createdAt)}</time><span className={`timeline-marker ${event.type.includes("failed") || event.type.includes("changes") || event.type.includes("needs_human") ? "warn" : ""}`} /><div><b>{eventLabel(event.type)}</b><small>{event.type}</small></div></li>)}</ol> : <p className="orchestration-empty">执行事件将在任务开始后出现。</p>}</section>
      </> : <>{panelBatch && <OrchestrationPlanPanel batch={panelBatch} busyKey={busy} onDelete={() => setConfirmDeleteBatch(panelBatch)} onStart={() => void startBatch(panelBatch)} />}{panelBatch ? <p className="orchestration-empty">该编排任务还没有子任务：可在 ③ 候选任务里勾选加入，或直接删除这个编排任务。</p> : <div className="orchestration-empty-main"><h2>选择一个编排任务</h2><p>从左侧队列查看任务详情。</p></div>}</>}</aside>
    </div>
    {confirmMerge && selected && <div className="orchestration-confirm-backdrop" role="presentation"><section className="orchestration-confirm" role="dialog" aria-modal="true" aria-labelledby="merge-main-title"><h2 id="merge-main-title">合并至 {selected.targetBranch || "目标分支"}</h2><p>将 <code>{selected.taskBranch}</code> 合并到任务入队时锁定的 <code>{selected.targetBranch || "目标分支"}</code>。发生冲突时会中止合并并保留任务状态。</p><footer><button type="button" className="secondary" disabled={Boolean(busy)} onClick={() => setConfirmMerge(false)}>取消</button><button type="button" className="primary" disabled={Boolean(busy)} onClick={() => void action("merge-main")}>{busy === "merge-main" ? "合并中" : "确认合并"}</button></footer></section></div>}
		{confirmCleanup && selected && <div className="orchestration-confirm-backdrop" role="presentation"><section className="orchestration-confirm" role="dialog" aria-modal="true" aria-labelledby="cleanup-title"><h2 id="cleanup-title">清理 worktree 与分支</h2>{selected.status === "released_to_main" ? <p>该任务已合并至 <code>{selected.targetBranch || "目标分支"}</code>，将移除对应 worktree 和任务分支。</p> : <p className="orchestration-cleanup-warning">此任务尚未合并至 <code>{selected.targetBranch || "目标分支"}</code>。清理会删除 worktree 和任务分支，未合并提交将无法通过本页面恢复。</p>}<label className="orchestration-force-close"><input type="checkbox" checked={closeLockingProcesses} disabled={Boolean(busy)} onChange={(event) => setCloseLockingProcesses(event.target.checked)} /><span>关闭正在占用此 worktree 的程序后清理</span><small>可能会强制关闭 VS Code、终端或 AI CLI 中打开该目录的进程；其中未保存内容会丢失。</small></label><footer><button type="button" className="secondary" disabled={Boolean(busy)} onClick={() => { setConfirmCleanup(false); setCloseLockingProcesses(false); }}>取消</button><button type="button" className="danger" disabled={Boolean(busy)} onClick={() => void cleanup(selected.status !== "released_to_main")}>{busy === "cleanup" ? "清理中" : selected.status === "released_to_main" ? "确认清理" : "我确认未合并也清理"}</button></footer></section></div>}
		{confirmRelease && <div className="orchestration-confirm-backdrop" role="presentation"><section className="orchestration-confirm" role="dialog" aria-modal="true" aria-labelledby="release-confirm-title"><h2 id="release-confirm-title">确认快照已合入 {mainBranchName}</h2><p>将校验 <code>{mainBranchName}</code> 是否已包含固定快照 <code>{confirmRelease.branch}</code>（<code>{shortSHA(confirmRelease.devSha)}</code>）。校验通过后，快照内的任务会标记为已发布并收口为完成；未包含时不会改动任何记录。</p><footer><button type="button" className="secondary" disabled={Boolean(busy)} onClick={() => setConfirmRelease(null)}>取消</button><button type="button" className="primary" disabled={Boolean(busy)} onClick={() => void confirmReleaseMerged()}>{busy === `release-confirm:${confirmRelease.id}` ? "确认中" : "确认已合入"}</button></footer></section></div>}
		{confirmDeleteBatch && <div className="orchestration-confirm-backdrop" role="presentation"><section className="orchestration-confirm" role="dialog" aria-modal="true" aria-labelledby="delete-batch-title"><h2 id="delete-batch-title">删除编排任务「{confirmDeleteBatch.name}」</h2><p>只删除这条编排任务本身：它的 <b>{confirmDeleteBatch.taskCount}</b> 个子任务会保留在「全部子任务」中，worktree、任务分支与执行对话都不受影响。</p>{confirmDeleteBatch.conversationStrategy === "continue" && <p className="orchestration-cleanup-warning">该编排任务启用了「继承上一任务对话摘要」，脱组后这些子任务再次执行时不再继承上一任务的对话。</p>}<p>只有没有任何未结束子任务时才能删除。若有：等待中或暂停中的先移出队列，执行中的先停止，「待合并」的先合并至目标分支。</p><footer><button type="button" className="secondary" disabled={Boolean(busy)} onClick={() => setConfirmDeleteBatch(null)}>取消</button><button type="button" className="danger" disabled={Boolean(busy)} onClick={() => void deleteBatch(confirmDeleteBatch)}>{busy === `delete-batch:${confirmDeleteBatch.id}` ? "删除中" : "确认删除"}</button></footer></section></div>}
		{branchSettingsOpen && <div className="orchestration-confirm-backdrop" role="presentation"><section className="orchestration-composer-dialog" role="dialog" aria-modal="true" aria-labelledby="branch-settings-title">
			<header className="orchestration-composer-head">
				<div><h2 id="branch-settings-title">编排分支设置</h2><p>稳定分支是任务合并目标，开发分支是发布快照的来源。</p></div>
				<button type="button" title="关闭" aria-label="关闭" disabled={Boolean(busy)} onClick={() => setBranchSettingsOpen(false)}>x</button>
			</header>
			<div className="orchestration-composer-body">
				<section className="orchestration-composer-section">
					<h3>分支</h3>
					<div className="orchestration-composer-grid">
						<label>稳定分支<input autoFocus value={branchDraft.mainBranch} disabled={Boolean(busy)} aria-invalid={!validOrchestrationBranch(branchDraft.mainBranch.trim())} placeholder="main" onChange={(event) => setBranchDraft((previous) => ({ ...previous, mainBranch: event.target.value }))} /></label>
						<label>开发分支<input value={branchDraft.devBranch} disabled={Boolean(busy)} aria-invalid={!validOrchestrationBranch(branchDraft.devBranch.trim())} placeholder="dev" onChange={(event) => setBranchDraft((previous) => ({ ...previous, devBranch: event.target.value }))} /></label>
					</div>
					{branchDraftIssue ? <p className="orchestration-composer-error" role="status">{branchDraftIssue}</p> : <p className="orchestration-composer-note">保存后写入项目编排策略：之后加入队列的任务沿用新配置，已有任务保留入队时的快照。</p>}
				</section>
			</div>
			<footer className="orchestration-composer-foot"><button type="button" className="secondary" disabled={Boolean(busy)} onClick={() => setBranchSettingsOpen(false)}>取消</button><button type="button" className="primary" disabled={Boolean(busy) || Boolean(branchDraftIssue)} onClick={() => void saveBranchSettings()}>{busy === "branches" ? "保存中" : "保存分支"}</button></footer>
		</section></div>}
		{batchComposerOpen && <div className="orchestration-confirm-backdrop" role="presentation"><section className="orchestration-composer-dialog" role="dialog" aria-modal="true" aria-labelledby="batch-title">
			<header className="orchestration-composer-head">
				<div><h2 id="batch-title">新建编排任务</h2></div>
				<button type="button" title="关闭" aria-label="关闭" disabled={Boolean(busy)} onClick={closeBatchComposer}>x</button>
			</header>
			<div className="orchestration-composer-body">
				<section className="orchestration-composer-section">
					<h3>基本信息</h3>
					<div className="orchestration-composer-grid">
						<label className="wide">名称<input autoFocus maxLength={120} value={batchName} disabled={Boolean(busy)} placeholder="例如：支付流程修复" onChange={(event) => setBatchName(event.target.value)} /></label>
						<label className="wide">后续任务上下文<select value={conversationStrategy} disabled={Boolean(busy)} onChange={(event) => setConversationStrategy(event.target.value as "new" | "continue")}><option value="new">不继承上一任务上下文</option><option value="continue">继承上一任务对话摘要</option></select><small>每个任务都会新建执行会话并使用自己的 worktree；仅将上一任务的对话摘要带入下一任务。</small></label>
					</div>
				</section>
				<section className="orchestration-composer-section">
					<h3>执行方式</h3>
					<div className="orchestration-composer-grid">
						<label className="wide">子任务在哪跑<select value={batchPolicy.executionMode} disabled={Boolean(busy)} onChange={(event) => { const nextMode = event.target.value as ExecutionMode; setBatchPolicy((previous) => ({ ...previous, executionMode: nextMode, targetBranch: nextMode === "branch" ? previous.targetBranch || currentBranchName : previous.targetBranch })); }}><option value="worktree">隔离工作树（默认）</option><option value="branch" disabled={Boolean(directModeBlockedReason)}>直接写入已有分支</option></select><small>{directModeSelected ? "Agent 直接在项目工作目录里改文件：不建分支、不建工作区、不自动提交，也没有需要合并的东西。" : "从目标分支拉一条临时分支到独立工作区实施，完成后由你确认合并。"}</small></label>
						{directModeSelected && <label className="wide">目标分支<select aria-label="目标分支" value={targetBranchDraft} disabled={Boolean(busy)} aria-invalid={!validOrchestrationBranch(targetBranchDraft)} onChange={(event) => setBatchPolicy((previous) => ({ ...previous, targetBranch: event.target.value }))}>{branchOptions.map((branch) => <option key={branch.name} value={branch.name}>{branch.current ? `${branch.name}（当前检出）` : branch.name}</option>)}</select><small>{currentBranchName ? `项目工作区当前检出 ${currentBranchName}；执行时会校验，不一致会停下并要求你先自己切分支。` : "执行时会校验项目工作区正检出所选分支。"}</small></label>}
					</div>
					{directModeSelected && (targetBranchMismatch
						? <p className="orchestration-composer-warning" role="status">{`项目工作区当前检出 ${currentBranchName}，与所选目标分支 ${targetBranchDraft} 不一致：开始执行前请先在项目目录里切到 ${targetBranchDraft}。Milevia 不会替你切换分支。`}</p>
						: <p className="orchestration-composer-warning" role="note">直接写入模式下 Agent 会在项目工作目录中直接修改文件，不会自动提交、不会创建分支、也不会切换分支。目录里可能已有你尚未提交的改动，Agent 被要求不得回退它们——请自行确认后再验收。本项目在该任务运行期间会被独占，其他会话的 AI 任务需排队。</p>)}
				</section>
				<section className="orchestration-composer-section">
					<h3>执行配置</h3>
					<div className="orchestration-composer-grid">
						<label>稳定分支<input value={batchPolicy.mainBranch} disabled={Boolean(busy)} aria-invalid={!mainBranchValid} placeholder="main" onChange={(event) => setBatchPolicy((previous) => ({ ...previous, mainBranch: event.target.value }))} /></label>
						<label>开发分支<input value={batchPolicy.devBranch} disabled={Boolean(busy)} aria-invalid={!devBranchValid} placeholder="dev" onChange={(event) => setBatchPolicy((previous) => ({ ...previous, devBranch: event.target.value }))} /></label>
						<label>执行 Agent<select value={batchPolicy.agentId} disabled={Boolean(busy)} onChange={(event) => setBatchPolicy((previous) => ({ ...previous, agentId: event.target.value as BatchPolicyDraft["agentId"] }))}>{agentOptions.length === 0
							? <option value={batchPolicy.agentId}>读取工具目录中…</option>
							: agentOptions.map((entry) => <option key={entry.id} value={entry.id}>{entry.name}</option>)}</select></label>
						<label>最大修复轮次<input type="number" min={1} max={10} value={batchPolicy.maxFixRounds} disabled={Boolean(busy)} aria-invalid={!maxFixRoundsValid} onChange={(event) => setBatchPolicy((previous) => ({ ...previous, maxFixRounds: Number(event.target.value) }))} /></label>
					</div>
					{batchPolicyIssue ? <p className="orchestration-composer-error" role="status">{batchPolicyIssue}</p> : <p className="orchestration-composer-note">该配置会写入项目编排策略，对所有之后加入队列的任务生效；开发分支用于生成发布验收快照。</p>}
				</section>
			</div>
			<footer className="orchestration-composer-foot"><button type="button" className="secondary" disabled={Boolean(busy)} onClick={closeBatchComposer}>取消</button><button type="button" className="primary" disabled={Boolean(busy) || !batchName.trim() || Boolean(batchPolicyIssue)} onClick={() => void createBatch()}>{busy === "batch" ? "创建中" : "创建编排任务"}</button></footer>
		</section></div>}
  </div>;
}

