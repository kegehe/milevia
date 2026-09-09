import { useCallback, useEffect, useMemo, useRef, useState, type KeyboardEvent, type ReactNode } from "react";

import { toast } from "sonner";

import { changeState, commitFileStatusLabel, formatGitTime, groupChanges, shortOID, type GitBranch, type GitChange, type GitCommit, type GitCommitDetail, type GitCommitFile, type GitConflictOverview, type GitDiff, type GitOperation, type GitSnapshot } from "./git-model";
import { ConflictSolveView } from "./ConflictSolveView";

type Request = <T>(path: string, init?: RequestInit) => Promise<T>;
type Tab = "changes" | "branches";
type ResolveAction = "ours" | "theirs" | "delete" | "working";
type GitOperationResult = { operationId: string; status: "succeeded" | "failed" | "needs_attention"; errorMessage?: string };
type DiffLine = { content: string; kind: "added" | "removed" | "context" | "hunk" | "meta"; oldLine?: number; newLine?: number };
type Confirmation =
  | { type: "commit" }
  | { type: "amend" }
  | { type: "discard-worktree"; path: string; untracked: boolean }
  | { type: "discard-all" }
  | { type: "fetch"; remote: string }
  | { type: "pull"; remote: string; branch: string }
  | { type: "push"; remote: string; branch: string; setUpstream: boolean }
  | { type: "switch-branch"; branch: string }
  | { type: "conflict-abort" }
  | { type: "conflict-finish" };

// 提交历史与操作记录的分页每页条数。
const HISTORY_PAGE_SIZE = 50;
const OPS_PAGE_SIZE = 50;
// 操作记录的筛选选项（types 与 git-model 的 GitOperationType 对齐）。
const OPERATION_TYPE_VALUES = ["stage", "unstage", "stage_all", "unstage_all", "commit", "commit_amend", "discard_worktree", "discard_all", "fetch", "pull", "push", "create_branch", "switch_branch", "resolve_conflict", "conflict_abort", "conflict_continue"];
const OPERATION_STATUS_VALUES = ["queued", "running", "succeeded", "failed", "cancelled", "needs_attention"];

const tabs: { id: Tab; label: string }[] = [
  { id: "changes", label: "变更" },
  { id: "branches", label: "分支" },
];

function gitPullTarget(snapshot: GitSnapshot | null): { remote: string; branch: string } {
  const upstream = snapshot?.head.upstream || "";
  const separator = upstream.indexOf("/");
  if (separator > 0 && separator < upstream.length - 1) {
    return { remote: upstream.slice(0, separator), branch: upstream.slice(separator + 1) };
  }
  return { remote: "origin", branch: snapshot?.head.branch || "" };
}

export function GitWorkbench({ projectID, conversationId, request, fail, active }: { projectID: string; conversationId?: string; request: Request; fail: (message: string) => void; active: boolean }) {
  const [tab, setTab] = useState<Tab>("changes");
  const [snapshot, setSnapshot] = useState<GitSnapshot | null>(null);
  const [changes, setChanges] = useState<GitChange[]>([]);
  const [branches, setBranches] = useState<GitBranch[]>([]);
  const [operations, setOperations] = useState<GitOperation[]>([]);
  const [conflictOverview, setConflictOverview] = useState<GitConflictOverview | null>(null);
  const [selectedDiff, setSelectedDiff] = useState<GitDiff | null>(null);
  const [conflictPath, setConflictPath] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [mutating, setMutating] = useState("");
  const [commitMessage, setCommitMessage] = useState("");
  const [confirmation, setConfirmation] = useState<Confirmation | null>(null);
  const diffRequest = useRef(0);
  const reloadRequest = useRef(0);
  const conflictPathRef = useRef<string | null>(null);
  const mountedRef = useRef(true);
	const withWorkspace = (path: string) => `${path}${path.includes("?") ? "&" : "?"}${conversationId ? `conversationId=${encodeURIComponent(conversationId)}` : ""}`;
  useEffect(() => {
    mountedRef.current = true;
    return () => { mountedRef.current = false; };
  }, []);

  const closeDiff = () => { diffRequest.current++; setSelectedDiff(null); };
  const closeConflict = () => { conflictPathRef.current = null; setConflictPath(null); };
  const openConflict = (path: string) => { diffRequest.current++; setSelectedDiff(null); conflictPathRef.current = path; setConflictPath(path); };
  const selectTab = (next: Tab) => { setTab(next); closeDiff(); closeConflict(); };
  const handleTabKeyDown = (event: KeyboardEvent<HTMLButtonElement>, current: Tab) => {
    const currentIndex = tabs.findIndex((item) => item.id === current);
    const targetIndex = event.key === "ArrowRight" ? (currentIndex + 1) % tabs.length
      : event.key === "ArrowLeft" ? (currentIndex - 1 + tabs.length) % tabs.length
        : event.key === "Home" ? 0 : event.key === "End" ? tabs.length - 1 : -1;
    if (targetIndex < 0) return;
    event.preventDefault();
    const next = tabs[targetIndex].id;
    selectTab(next);
    requestAnimationFrame(() => document.getElementById(`git-tab-${next}`)?.focus());
  };

  const reload = useCallback(async (manual = false) => {
    const requestID = ++reloadRequest.current;
    if (manual) { if (!mountedRef.current) return; setRefreshing(true); }
    else { if (!mountedRef.current) return; setLoading(true); }
    try {
      const base = `/api/projects/${projectID}/git`;
      // conflicts 只读总览：偶尔失败不应拖垮整个工作台，单独降级为空。
      const [nextSnapshot, nextChanges, nextBranches, nextOperations, nextConflicts] = await Promise.all([
        request<GitSnapshot>(withWorkspace(`${base}/summary`)),
        request<GitChange[]>(withWorkspace(`${base}/changes`)),
        request<GitBranch[]>(withWorkspace(`${base}/branches`)),
        request<GitOperation[]>(withWorkspace(`${base}/operations`)),
        request<GitConflictOverview>(withWorkspace(`${base}/conflicts`)).catch(() => null),
      ]);
      if (!mountedRef.current || requestID !== reloadRequest.current) return;
      setSnapshot(nextSnapshot);
      setChanges(nextChanges);
      setBranches(nextBranches);
      setOperations(nextOperations);
      setConflictOverview(nextConflicts);
      // 正在解决的路径已不在冲突清单中（已解决/中止）时，退出该文件的解决视图。
      if (conflictPathRef.current && nextConflicts && !nextConflicts.files.some((file) => file.path === conflictPathRef.current)) closeConflict();
    } catch (cause) {
      if (mountedRef.current && requestID === reloadRequest.current) fail(cause instanceof Error ? cause.message : "无法读取 Git 仓库");
    } finally {
      if (mountedRef.current && requestID === reloadRequest.current) {
        setLoading(false);
        setRefreshing(false);
      }
    }
  }, [projectID, request, fail, conversationId]);

  useEffect(() => { if (active) void reload().catch(() => undefined); }, [active, reload]);

  const grouped = useMemo(() => groupChanges(changes), [changes]);
  const changeCount = changes.length;
  const trackedChangeCount = changes.filter((change) => !change.untracked).length;
  const untrackedChangeCount = changes.filter((change) => change.untracked).length;
  const openDiff = async (change: GitChange, stage: "worktree" | "index") => {
    const requestID = ++diffRequest.current;
    try {
      const diff = await request<GitDiff>(withWorkspace(`/api/projects/${projectID}/git/diff?path=${encodeURIComponent(change.path)}&stage=${stage}`));
      if (requestID === diffRequest.current && mountedRef.current) setSelectedDiff(diff);
    } catch (cause) { if (mountedRef.current) fail(cause instanceof Error ? cause.message : "无法读取差异"); }
  };

  const mutate = async (key: string, endpoint: string, payload: Record<string, unknown>, success?: () => void) => {
    if (!mountedRef.current) return;
    if (!snapshot?.stateToken) {
      fail("Git 状态尚未准备完成，请刷新后重试");
      return;
    }
    setMutating(key);
    try {
      const result = await request<GitOperationResult>(withWorkspace(`/api/projects/${projectID}/git/${endpoint}`), { method: "POST", body: JSON.stringify({ ...payload, stateToken: snapshot.stateToken }) });
      await reload(true);
      if (result.status === "needs_attention") {
        closeDiff();
        setConfirmation(null);
      }
      if (result.status !== "succeeded") throw new Error(result.errorMessage || "Git 操作未完成，请查看操作记录");
      if (!mountedRef.current) return;
      closeDiff();
      success?.();
    } catch (cause) {
      if (mountedRef.current) {
        fail(cause instanceof Error ? cause.message : "无法执行 Git 操作");
        void reload(true).catch(() => undefined);
      }
    } finally {
      if (mountedRef.current) setMutating("");
    }
  };

  const mutatePath = (action: "stage" | "unstage", path: string) => mutate(`${action}:${path}`, action, { paths: [path] });
  const stageAll = () => mutate("stage-all", "stage-all", {});
  const unstageAll = () => mutate("unstage-all", "unstage-all", {});
  const commit = () => mutate("commit", "commits", { message: commitMessage }, () => { setCommitMessage(""); setConfirmation(null); });
  const commitAmend = () => mutate("commit_amend", "commits/amend", { message: commitMessage }, () => { setCommitMessage(""); setConfirmation(null); });
  const discardWorktree = (path: string, untracked: boolean) => mutate(`discard:${path}`, "discard", { mode: "worktree", paths: [path], includeUntracked: untracked }, () => setConfirmation(null));
  const discardAll = (includeUntracked: boolean) => mutate("discard-all", "discard", { mode: "all", includeUntracked }, () => setConfirmation(null));
  const fetchRemote = (remote: string) => mutate("fetch", "fetch", { remote }, () => setConfirmation(null));
  const pullRemote = (remote: string, branch: string) => mutate("pull", "pull", { remote, branch }, () => setConfirmation(null));
  const pushBranch = (remote: string, branch: string, setUpstream: boolean) => mutate("push", "push", { remote, branch, setUpstream }, () => setConfirmation(null));
  const switchBranch = (branch: string) => mutate("switch-branch", "switch", { branch }, () => setConfirmation(null));
  const pullTarget = gitPullTarget(snapshot);

  const resolveConflict = (path: string, action: ResolveAction, content?: string) => {
    const payload: Record<string, unknown> = { path, action };
    if (content !== undefined) payload.content = content;
    mutate(`resolve:${path}:${action}`, "conflicts/resolve", payload, () => {
      closeConflict();
      setConfirmation(null);
    });
  };
  const requestAbortConflict = () => setConfirmation({ type: "conflict-abort" });
  const requestFinishConflict = () => setConfirmation({ type: "conflict-finish" });
  const abortConflict = () => mutate("conflict-abort", "conflicts/abort", {}, () => { closeConflict(); setConfirmation(null); });
  const finishConflict = () => mutate("conflict-finish", "conflicts/continue", {}, () => { closeConflict(); setConfirmation(null); });

  const createBranch = async (name: string, startPoint: string) => {
    if (!mountedRef.current) return;
    setMutating("create-branch");
    try {
      const result = await request<GitOperationResult>(withWorkspace(`/api/projects/${projectID}/git/branches`), { method: "POST", body: JSON.stringify({ name, startPoint }) });
      await reload(true);
      if (result.status !== "succeeded" && result.status !== "needs_attention") throw new Error(result.errorMessage || "创建分支失败");
      closeDiff();
      setConfirmation(null);
    } catch (cause) {
      if (mountedRef.current) {
        fail(cause instanceof Error ? cause.message : "无法创建分支");
        void reload(true).catch(() => undefined);
      }
    } finally {
      if (mountedRef.current) setMutating("");
    }
  };

  return <section id="workspace-panel-git" className="git-workbench workspace-panel" role="tabpanel" aria-labelledby="workspace-tab-git" hidden={!active}>
    <GitBar snapshot={snapshot} loading={loading} mutating={mutating} refreshing={refreshing} operations={operations} projectID={projectID} conversationId={conversationId} request={request} fail={fail} requestRefresh={() => void reload(true)} requestFetch={() => setConfirmation({ type: "fetch", remote: snapshot?.head.upstream?.split("/")[0] || "origin" })} requestPull={() => setConfirmation({ type: "pull", remote: pullTarget.remote, branch: pullTarget.branch })} requestPush={() => setConfirmation({ type: "push", remote: snapshot?.head.upstream?.split("/")[0] || "origin", branch: snapshot?.head.branch || "", setUpstream: !snapshot?.head.upstream })} />
    <nav className="git-tabs" aria-label="Git工作台视图">
      <div className="git-tab-list" role="tablist" aria-label="Git工作台视图">{tabs.map((item) => <button type="button" key={item.id} id={`git-tab-${item.id}`} role="tab" aria-controls={`git-view-${item.id}`} className={tab === item.id ? "active" : ""} aria-selected={tab === item.id} tabIndex={tab === item.id ? 0 : -1} onClick={() => selectTab(item.id)} onKeyDown={(event) => handleTabKeyDown(event, item.id)}>{item.label}{item.id === "changes" && changeCount > 0 ? <b>{changeCount}</b> : null}</button>)}</div>
    </nav>
    <main className={`git-workbench-body${tab === "changes" ? " changes-active" : ""}`}>
      {loading ? <div className="git-empty">正在读取仓库状态</div> : !snapshot ? <div className="git-empty">无法读取仓库状态</div> : <>
        {tab === "changes" && <div id="git-view-changes" role="tabpanel" aria-labelledby="git-tab-changes"><Changes grouped={grouped} selectedDiff={selectedDiff} openDiff={openDiff} closeDiff={closeDiff} conflictOverview={conflictOverview} conflictPath={conflictPath} openConflict={openConflict} closeConflict={closeConflict} resolveConflict={resolveConflict} requestAbortConflict={requestAbortConflict} requestFinishConflict={requestFinishConflict} projectID={projectID} conversationId={conversationId} request={request} fail={fail} mutatePath={mutatePath} stageAll={stageAll} unstageAll={unstageAll} requestDiscardWorktree={(path, untracked) => setConfirmation({ type: "discard-worktree", path, untracked })} requestDiscardAll={() => setConfirmation({ type: "discard-all" })} requestCommit={() => setConfirmation({ type: "commit" })} requestAmend={() => setConfirmation({ type: "amend" })} commitMessage={commitMessage} setCommitMessage={setCommitMessage} mutating={mutating} changeCount={changeCount} /></div>}
        {tab === "branches" && <div id="git-view-branches" role="tabpanel" aria-labelledby="git-tab-branches"><Branches branches={branches} mutating={mutating} projectID={projectID} conversationId={conversationId} request={request} fail={fail} requestSwitchBranch={(branch) => setConfirmation({ type: "switch-branch", branch })} requestCreateBranch={(name, startPoint) => void createBranch(name, startPoint)} /></div>}
      </>}
    </main>
    {confirmation && <GitConfirmation confirmation={confirmation} snapshot={snapshot} conflictOverview={conflictOverview} stagedCount={grouped.staged.length} trackedChangeCount={trackedChangeCount} untrackedChangeCount={untrackedChangeCount} commitMessage={commitMessage} busy={Boolean(mutating)} close={() => setConfirmation(null)} commit={commit} commitAmend={commitAmend} discardWorktree={discardWorktree} discardAll={discardAll} fetchRemote={fetchRemote} pullRemote={pullRemote} pushBranch={pushBranch} switchBranch={switchBranch} abortConflict={abortConflict} finishConflict={finishConflict} />}
  </section>;
}

export function GitBar({ snapshot, loading, mutating, refreshing, operations, projectID, conversationId, request, fail, requestRefresh, requestFetch, requestPull, requestPush }: { snapshot: GitSnapshot | null; loading: boolean; mutating: string; refreshing: boolean; operations: GitOperation[]; projectID: string; conversationId?: string; request: Request; fail: (message: string) => void; requestRefresh: () => void; requestFetch: () => void; requestPull: () => void; requestPush: () => void }) {
  const withWorkspace = (path: string) => `${path}${path.includes("?") ? "&" : "?"}${conversationId ? `conversationId=${encodeURIComponent(conversationId)}` : ""}`;
  const [opsOpen, setOpsOpen] = useState(false);
  const [opsItems, setOpsItems] = useState<GitOperation[]>([]);
  const [opsLoading, setOpsLoading] = useState(false);
  const [opsHasMore, setOpsHasMore] = useState(false);
  const [opsType, setOpsType] = useState("");
  const [opsStatus, setOpsStatus] = useState("");
  const [opsQuery, setOpsQuery] = useState("");
  const [opsPendingQuery, setOpsPendingQuery] = useState("");
  const opsRequest = useRef(0);
  const fetchOps = (skip: number, append: boolean) => {
    const requestID = ++opsRequest.current;
    setOpsLoading(true);
    const params = new URLSearchParams({ limit: String(OPS_PAGE_SIZE) });
    if (skip > 0) params.set("skip", String(skip));
    if (opsType) params.set("type", opsType);
    if (opsStatus) params.set("status", opsStatus);
    if (opsPendingQuery) params.set("q", opsPendingQuery);
    request<GitOperation[]>(withWorkspace(`/api/projects/${projectID}/git/operations?${params.toString()}`))
      .then((items) => { if (requestID === opsRequest.current) { setOpsHasMore(items.length === OPS_PAGE_SIZE); setOpsItems((prev) => append ? [...prev, ...items] : items); } })
      .catch((cause) => { if (requestID === opsRequest.current) { if (!append) { setOpsHasMore(false); setOpsItems([]); } fail(cause instanceof Error ? cause.message : "无法读取操作记录"); } })
      .finally(() => { if (requestID === opsRequest.current) setOpsLoading(false); });
  };
  // 打开操作记录时读取第一页；筛选条件或检索词变化后重新读取。
  useEffect(() => {
    if (!opsOpen) return;
    fetchOps(0, false);
    // withWorkspace / request 每次渲染重建，纳入依赖会重复拉取。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [opsOpen, opsType, opsStatus, opsPendingQuery]);
  // 检索词防抖后再查询，避免每次按键都打一次接口。
  useEffect(() => {
    const timer = window.setTimeout(() => setOpsPendingQuery(opsQuery), 300);
    return () => window.clearTimeout(timer);
  }, [opsQuery]);
  const loadMoreOps = () => fetchOps(opsItems.length, true);
  const barRef = useRef<HTMLDivElement>(null);
  const head = snapshot?.head;
  const syncLabel = head?.upstream
    ? head.ahead === 0 && head.behind === 0 ? "与上游同步" : head.ahead > 0 && head.behind === 0 ? `领先 ${head.ahead}` : head.ahead === 0 && head.behind > 0 ? `落后 ${head.behind}` : `领先 ${head.ahead} · 落后 ${head.behind}`
    : "未跟踪上游";
  const needsAttention = operations.some((operation) => operation.status === "failed" || operation.status === "needs_attention");
  // 点击外部或 Escape 键关闭操作记录。沿用 NotificationCenter 的 ref.contains 模式：
  // 只监听不拦截，外部点击（如切换工作区 tab）会正常穿透。
  useEffect(() => {
    if (!opsOpen) return;
    const handleClickOutside = (event: MouseEvent) => {
      const target = event.target as Node;
      if (barRef.current?.contains(target)) return;
      setOpsOpen(false);
    };
    const handleKeyDown = (event: globalThis.KeyboardEvent) => {
      if (event.key === "Escape") setOpsOpen(false);
    };
    document.addEventListener("mousedown", handleClickOutside);
    document.addEventListener("keydown", handleKeyDown);
    return () => {
      document.removeEventListener("mousedown", handleClickOutside);
      document.removeEventListener("keydown", handleKeyDown);
    };
  }, [opsOpen]);
  return <div className="git-bar" ref={barRef}>
    <div className="git-bar-ref">
      {loading && !snapshot ? <><span className="git-bar-label">当前引用</span><span className="git-bar-loading">正在读取仓库状态…</span></> : <>
        <span className="git-bar-label">当前引用</span>
        <div className="git-bar-name">
          <b title={`当前 HEAD ${head?.oid ?? ""}`}>{head?.detached ? "HEAD (游离指针)" : head?.branch || "未初始化"}</b>
          {head?.oid ? <CopyOID oid={head.oid} title={`点击复制当前 HEAD 提交 ID：${head.oid}`} /> : null}
        </div>
        <span className={`git-sync-badge${head?.upstream ? "" : " muted"}`}>{syncLabel}</span>
      </>}
    </div>
    <div className="git-bar-actions">
      <button type="button" className="secondary" disabled={loading || Boolean(mutating) || head?.detached || !head?.branch || !head?.upstream} onClick={() => { setOpsOpen(false); requestPull(); }} title={loading ? "正在读取仓库状态" : head?.detached || !head?.branch ? "游离指针状态无法拉取" : !head?.upstream ? "未设置上游分支" : undefined}>拉取</button>
      <button type="button" className="secondary" disabled={loading || Boolean(mutating)} onClick={() => { setOpsOpen(false); requestFetch(); }}>获取</button>
      <button type="button" className="secondary" disabled={loading || Boolean(mutating) || head?.detached || !head?.branch || head?.ahead === 0} onClick={() => { setOpsOpen(false); requestPush(); }} title={loading ? "正在读取仓库状态" : head?.detached || !head?.branch ? "游离指针状态无法推送" : head?.ahead === 0 ? "没有需要推送的提交" : undefined}>推送</button>
      <button type="button" className={`git-refresh${refreshing ? " spinning" : ""}`} title={refreshing ? "正在刷新仓库状态" : "刷新仓库状态"} aria-label={refreshing ? "正在刷新仓库状态" : "刷新仓库状态"} disabled={refreshing || Boolean(mutating)} onClick={requestRefresh}><RefreshIcon /></button>
      <button type="button" className={`git-ops-trigger${opsOpen ? " active" : ""}${needsAttention ? " attention" : ""}`} title="操作记录" aria-label="操作记录" aria-expanded={opsOpen} aria-controls="git-ops-popover" disabled={operations.length === 0} onClick={() => setOpsOpen(!opsOpen)}><HistoryIcon />{needsAttention ? <i className="git-ops-dot" aria-hidden="true" /> : null}</button>
    </div>
    {opsOpen && <div id="git-ops-popover" className="git-ops-popover" role="dialog" aria-label="操作记录"><header><div><span>审计记录</span><h3>操作记录</h3></div><button type="button" className="git-confirmation-close" title="关闭" aria-label="关闭" onClick={() => setOpsOpen(false)}><CloseIcon /></button></header><div className="git-ops-popover-body"><div className="git-ops-filters"><select value={opsType} aria-label="按操作类型筛选" onChange={(event) => setOpsType(event.target.value)}><option value="">全部类型</option>{OPERATION_TYPE_VALUES.map((value) => <option key={value} value={value}>{operationTypeLabel(value)}</option>)}</select><select value={opsStatus} aria-label="按操作状态筛选" onChange={(event) => setOpsStatus(event.target.value)}><option value="">全部状态</option>{OPERATION_STATUS_VALUES.map((value) => <option key={value} value={value}>{operationStatusLabel(value)}</option>)}</select><div className="git-ops-search"><input type="text" value={opsQuery} placeholder="搜索操作…" aria-label="搜索操作记录" onChange={(event) => setOpsQuery(event.target.value)} />{opsQuery ? <button type="button" className="git-history-search-clear" title="清除搜索" aria-label="清除搜索" onClick={() => setOpsQuery("")}>×</button> : null}</div></div><Operations operations={opsItems} loading={opsLoading} />{opsHasMore && !opsLoading ? <button type="button" className="git-ops-load-more" onClick={loadMoreOps}>加载更多操作</button> : null}</div></div>}
  </div>;
}

function PlusIcon() { return <svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round"><path d="M8 3v10M3 8h10" /></svg>; }

function UndoIcon() { return <svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round"><path d="M13.5 11a5.5 5.5 0 0 0-9.4-3.9L2.5 8.7" /><path d="M2.5 4.5v4.2h4.2" /></svg>; }

function PendingIcon() { return <svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round"><path d="M14 8A6 6 0 1 1 8 2" /><polyline points="8 2 8 5.5 11 2.5" /></svg>; }

function RefreshIcon() { return <svg viewBox="0 0 16 16" width="16" height="16" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round"><path d="M13 5.8A5.5 5.5 0 1 0 13.4 10" /><path d="M13 2.5v3.3H9.7" /></svg>; }

function CloseIcon() { return <svg viewBox="0 0 16 16" width="16" height="16" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round"><path d="m4 4 8 8M12 4l-8 8" /></svg>; }

function HistoryIcon() { return <svg viewBox="0 0 16 16" width="16" height="16" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round"><path d="M3.2 8a4.8 4.8 0 1 1 1.4 3.4" /><path d="M3 4.2v3.4h3.4" /><path d="M8 5v3l2 1.4" /></svg>; }

function Changes({ grouped, selectedDiff, openDiff, closeDiff, conflictOverview, conflictPath, openConflict, closeConflict, resolveConflict, requestAbortConflict, requestFinishConflict, projectID, conversationId, request, fail, mutatePath, stageAll, unstageAll, requestDiscardWorktree, requestDiscardAll, requestCommit, requestAmend, commitMessage, setCommitMessage, mutating, changeCount }: {
  grouped: { staged: GitChange[]; worktree: GitChange[] };
  selectedDiff: GitDiff | null;
  openDiff: (change: GitChange, stage: "worktree" | "index") => Promise<void>;
  closeDiff: () => void;
  conflictOverview: GitConflictOverview | null;
  conflictPath: string | null;
  openConflict: (path: string) => void;
  closeConflict: () => void;
  resolveConflict: (path: string, action: ResolveAction, content?: string) => void;
  requestAbortConflict: () => void;
  requestFinishConflict: () => void;
  projectID: string;
  conversationId?: string;
  request: Request;
  fail: (message: string) => void;
  mutatePath: (action: "stage" | "unstage", path: string) => void;
  stageAll: () => void;
  unstageAll: () => void;
  requestDiscardWorktree: (path: string, untracked: boolean) => void;
  requestDiscardAll: () => void;
  requestCommit: () => void;
  requestAmend: () => void;
  commitMessage: string;
  setCommitMessage: (value: string) => void;
  mutating: string;
  changeCount: number;
}) {
  const conflictedCount = grouped.worktree.filter((change) => change.conflicted).length;
  const context = conflictOverview?.context;
  const operationVerb = conflictOperationVerb(context?.operationType);
  const openFile = (change: GitChange, stage: "worktree" | "index") => {
    if (change.conflicted) openConflict(change.path);
    else { closeConflict(); void openDiff(change, stage); }
  };
  const contextPhrase = context && context.operationType !== "none" && context.theirsLabel ? `${context.theirsLabel} → ${context.oursLabel}` : "";
  return <div className="git-changes-view">
    <div className="git-changes-sidebar">
      {conflictedCount > 0
        ? <div className="git-conflict-banner" role="alert"><b>⚠ {conflictedCount} 个冲突文件需要解决</b><span>{operationVerb && contextPhrase ? `正在${operationVerb} ${contextPhrase}，点击文件逐块处理` : "在下方工作区列表中点击文件进入解决视图"}</span></div>
        : context?.canFinish
          ? <div className="git-conflict-banner finished" role="status"><b>✓ 冲突已全部解决</b><span>{operationVerb}可完成，提交后将结束本次操作</span><div className="git-conflict-banner-actions"><button type="button" className="primary" disabled={mutating !== ""} onClick={requestFinishConflict}>完成{operationVerb}</button><button type="button" className="secondary danger" disabled={mutating !== ""} onClick={requestAbortConflict}>中止并还原</button></div></div>
          : null}
      <section className="git-change-group"><header><h3>已暂存</h3><div className="git-group-actions"><span>{grouped.staged.length}</span><button type="button" className={`git-icon-btn${mutating === "unstage-all" ? " spinning" : ""}`} title={mutating === "unstage-all" ? "取消暂存中" : "全部取消暂存"} aria-label="全部取消暂存" disabled={mutating !== "" || grouped.staged.length === 0} onClick={unstageAll}>{mutating === "unstage-all" ? <PendingIcon /> : <UndoIcon />}</button></div></header><ChangeList changes={grouped.staged} stage="index" openDiff={openFile} mutatePath={mutatePath} requestDiscard={requestDiscardWorktree} mutating={mutating} empty="没有已暂存变更" /></section>
      <CommitPanel stagedCount={grouped.staged.length} value={commitMessage} setValue={setCommitMessage} open={requestCommit} openAmend={requestAmend} disabled={mutating !== ""} />
      <section className="git-change-group"><header><h3>工作区</h3><div className="git-group-actions"><span>{grouped.worktree.length}</span><button type="button" className={`git-icon-btn${mutating === "stage-all" ? " spinning" : ""}`} title={mutating === "stage-all" ? "暂存中" : conflictedCount > 0 ? "存在冲突文件，请先逐个解决" : "全部暂存"} aria-label="全部暂存" disabled={mutating !== "" || grouped.worktree.length === 0 || conflictedCount > 0} onClick={stageAll}>{mutating === "stage-all" ? <PendingIcon /> : <PlusIcon />}</button><button type="button" className="git-icon-btn danger" title="撤销全部未提交改动" aria-label="撤销全部未提交改动" disabled={mutating !== "" || changeCount === 0} onClick={requestDiscardAll}><UndoIcon /></button></div></header><ChangeList changes={grouped.worktree} stage="worktree" openDiff={openFile} mutatePath={mutatePath} requestDiscard={requestDiscardWorktree} mutating={mutating} empty="工作区没有变更" /></section>
    </div>
    <section className="git-changes-content" aria-label="文件内容">
      {conflictPath
        ? <ConflictSolveView key={conflictPath} projectID={projectID} conversationId={conversationId} path={conflictPath} conflictPaths={(conflictOverview?.files.map((file) => file.path) ?? [conflictPath])} request={request} fail={fail} oursLabel={context?.oursLabel || "当前"} theirsLabel={context?.theirsLabel || "传入"} busy={mutating !== ""} onResolve={resolveConflict} onOpenFile={openConflict} onClose={closeConflict} />
        : selectedDiff ? <DiffViewer diff={selectedDiff} close={closeDiff} /> : <div className="git-diff-placeholder"><div className="git-diff-placeholder-icon" aria-hidden="true"><PendingIcon /></div><h3>选择一个文件查看变更</h3><p>从左侧工作区或已暂存列表中点击文件，差异内容会显示在这里。</p></div>}
    </section>
  </div>;
}

function conflictOperationVerb(operationType: GitConflictOverview["context"]["operationType"] | undefined): string {
  return ({ merge: "合并", rebase: "变基", "cherry-pick": "挑选", revert: "还原", none: "" })[operationType ?? "none"] || "";
}

export function parseDiffContent(content: string): DiffLine[] {
  const source = content.split(/\r?\n/);
  if (source.at(-1) === "") source.pop();
  const unified = source.some((line) => line.startsWith("@@ "));
  let oldLine = 1;
  let newLine = 1;
  let sawHunk = false;
  return source.map((line) => {
    const hunk = line.match(/^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/);
    if (hunk) {
      sawHunk = true;
      oldLine = Number(hunk[1]);
      newLine = Number(hunk[2]);
      return { content: line, kind: "hunk" };
    }
    // 首个 hunk 之前的全部是文件头（diff --git、new file mode、rename from、Binary files 等），统一按元信息显示。
    if (unified && !sawHunk) return { content: line, kind: "meta" };
    if (unified && line.startsWith("\\ No newline")) return { content: line, kind: "meta" };
    if (unified && line.startsWith("+")) return { content: line.slice(1), kind: "added", newLine: newLine++ };
    if (unified && line.startsWith("-")) return { content: line.slice(1), kind: "removed", oldLine: oldLine++ };
    if (unified && line.startsWith(" ")) return { content: line.slice(1), kind: "context", oldLine: oldLine++, newLine: newLine++ };
    return { content: line, kind: "context", oldLine: oldLine++, newLine: newLine++ };
  });
}

function DiffViewer({ diff, close }: { diff: GitDiff; close: () => void }) {
  const lines = parseDiffContent(diff.content);
  const stageLabel = diff.stage === "index" ? "已暂存差异" : diff.stage === "commit" ? "提交差异" : "工作区差异";
  return <section className="git-diff"><header><div><span>{stageLabel}</span><b>{diff.path}</b></div><button type="button" className="git-close" title="关闭差异" aria-label="关闭差异" onClick={close}>×</button></header>{lines.length ? <div className="git-diff-code" role="region" aria-label={`${diff.path} 的${stageLabel}`}>{lines.map((line, index) => <div className={`git-diff-line ${line.kind}`} key={`${index}-${line.content}`}><span className="git-diff-number">{line.oldLine ?? ""}</span><span className="git-diff-number">{line.newLine ?? ""}</span><span className="git-diff-prefix" aria-hidden="true">{line.kind === "added" ? "+" : line.kind === "removed" ? "-" : ""}</span><code>{line.content}</code></div>)}</div> : <p className="git-diff-empty">该文件没有可显示的文本差异。</p>}</section>;
}

function ChangeList({ changes, stage, openDiff, mutatePath, requestDiscard, mutating, empty }: { changes: GitChange[]; stage: "worktree" | "index"; openDiff: (change: GitChange, stage: "worktree" | "index") => void; mutatePath: (action: "stage" | "unstage", path: string) => void; requestDiscard: (path: string, untracked: boolean) => void; mutating: string; empty: string }) {
  if (changes.length === 0) return <p className="git-list-empty">{empty}</p>;
  const action = stage === "index" ? "unstage" : "stage";
  return <div className="git-change-list">{changes.map((change) => {
    const conflictBlocked = change.conflicted && stage === "worktree";
    return <div key={`${stage}-${change.path}`}><button type="button" onClick={() => openDiff(change, stage)}><span className={`git-change-kind ${change.conflicted ? "conflicted" : ""}`}>{changeState(change)}</span><b>{change.path}</b>{change.originalPath ? <small>{change.originalPath}</small> : null}</button><div className="git-inline-actions"><button type="button" className={`git-inline-icon${mutating === `${action}:${change.path}` ? " spinning" : ""}`} title={mutating === `${action}:${change.path}` ? "处理中" : conflictBlocked ? "冲突文件请先进入右侧解决视图" : action === "stage" ? "暂存" : "取消暂存"} aria-label={action === "stage" ? "暂存" : "取消暂存"} disabled={mutating !== "" || conflictBlocked} onClick={() => mutatePath(action, change.path)}>{mutating === `${action}:${change.path}` ? <PendingIcon /> : action === "stage" ? <PlusIcon /> : <UndoIcon />}</button>{stage === "worktree" && <button type="button" className="git-inline-icon danger" title={change.untracked ? "删除未跟踪文件" : "撤销工作区改动"} aria-label={change.untracked ? "删除未跟踪文件" : "撤销工作区改动"} disabled={mutating !== "" || change.conflicted} onClick={() => requestDiscard(change.path, change.untracked)}>{change.untracked ? <svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round"><polyline points="2 4.5 14 4.5" /><path d="M5 4.5V3a1 1 0 0 1 1-1h4a1 1 0 0 1 1 1v1.5" /><path d="M3.5 4.5l.7 9a1.5 1.5 0 0 0 1.5 1.4h4.6a1.5 1.5 0 0 0 1.5-1.4l.7-9" /></svg> : <UndoIcon />}</button>}</div></div>;
  })}</div>;
}

function CommitPanel({ stagedCount, value, setValue, open, openAmend, disabled }: { stagedCount: number; value: string; setValue: (value: string) => void; open: () => void; openAmend: () => void; disabled: boolean }) {
  return <section className="git-commit-panel"><label htmlFor="git-commit-message">提交信息</label><textarea id="git-commit-message" value={value} maxLength={4000} disabled={disabled || stagedCount === 0} onChange={(event) => setValue(event.target.value)} placeholder={stagedCount === 0 ? "暂存文件后即可提交" : "简要说明本次变更"} /><footer><span>{stagedCount === 0 ? "没有已暂存文件" : `将提交 ${stagedCount} 个文件`}</span><div className="git-commit-actions"><button type="button" className="secondary git-amend-btn" title="修改最近一次提交" disabled={disabled || !value.trim()} onClick={openAmend}>修改最近一次提交</button><button type="button" className="primary" disabled={disabled || stagedCount === 0 || !value.trim() || value.split("\n")[0].length > 72} onClick={open}>提交</button></div></footer></section>;
}

function GitConfirmation({ confirmation, snapshot, conflictOverview, stagedCount, trackedChangeCount, untrackedChangeCount, commitMessage, busy, close, commit, commitAmend, discardWorktree, discardAll, fetchRemote, pullRemote, pushBranch, switchBranch, abortConflict, finishConflict }: {
  confirmation: Confirmation;
  snapshot: GitSnapshot | null;
  conflictOverview: GitConflictOverview | null;
  stagedCount: number;
  trackedChangeCount: number;
  untrackedChangeCount: number;
  commitMessage: string;
  busy: boolean;
  close: () => void;
  commit: () => void;
  commitAmend: () => void;
  discardWorktree: (path: string, untracked: boolean) => void;
  discardAll: (includeUntracked: boolean) => void;
  fetchRemote: (remote: string) => void;
  pullRemote: (remote: string, branch: string) => void;
  pushBranch: (remote: string, branch: string, setUpstream: boolean) => void;
  switchBranch: (branch: string) => void;
  abortConflict: () => void;
  finishConflict: () => void;
}) {
  const [includeUntracked, setIncludeUntracked] = useState(confirmation.type === "discard-all" && untrackedChangeCount > 0 && trackedChangeCount === 0);
  const isCommit = confirmation.type === "commit";
  const isAmend = confirmation.type === "amend";
  const isFetch = confirmation.type === "fetch";
  const isPull = confirmation.type === "pull";
  const isPush = confirmation.type === "push";
  const isSwitch = confirmation.type === "switch-branch";
  const isAbort = confirmation.type === "conflict-abort";
  const isFinish = confirmation.type === "conflict-finish";
  const isDestructive = confirmation.type === "discard-worktree" || confirmation.type === "discard-all" || isAbort;
  const operationVerb = conflictOperationVerb(conflictOverview?.context?.operationType);
  const context = conflictOverview?.context;
  const title = isCommit ? "确认提交" : isAmend ? "确认修改提交" : isFetch ? "获取远端更新" : isPull ? "拉取远端更新" : isPush ? "推送到远端" : isSwitch ? "切换分支" : isAbort ? `中止${operationVerb}并还原` : isFinish ? `完成${operationVerb}` : confirmation.type === "discard-all" ? "丢弃全部未提交改动" : confirmation.type === "discard-worktree" && confirmation.untracked ? "删除未跟踪文件" : "撤销工作区更改";
  const isUntracked = confirmation.type === "discard-worktree" && confirmation.untracked;
  const discardPath = confirmation.type === "discard-worktree" ? confirmation.path : "";
  const riskLabel = confirmation.type === "discard-worktree" && !confirmation.untracked ? "工作区内容将恢复到暂存版本" : isAbort ? `当前${operationVerb}产生的所有改动都会丢弃，仓库将还原到操作前状态` : "此操作无法在页面中撤销";
  const categoryLabel = isCommit || isAmend ? "本地提交" : isFetch || isPull ? "远端操作" : isPush ? "远端操作" : isSwitch ? "分支操作" : isAbort || isFinish ? "冲突处理" : "高风险操作";
  const confirm = () => {
    if (confirmation.type === "commit") commit();
    else if (confirmation.type === "amend") commitAmend();
    else if (confirmation.type === "discard-all") discardAll(includeUntracked);
    else if (confirmation.type === "discard-worktree") discardWorktree(confirmation.path, confirmation.untracked);
    else if (confirmation.type === "fetch") fetchRemote(confirmation.remote);
    else if (confirmation.type === "pull") pullRemote(confirmation.remote, confirmation.branch);
    else if (confirmation.type === "push") pushBranch(confirmation.remote, confirmation.branch, confirmation.setUpstream);
    else if (confirmation.type === "switch-branch") switchBranch(confirmation.branch);
    else if (confirmation.type === "conflict-abort") abortConflict();
    else if (confirmation.type === "conflict-finish") finishConflict();
  };
  const confirmLabel = busy ? "处理中" : isCommit ? "确认提交" : isAmend ? "确认修改" : isFetch ? "确认获取" : isPull ? "确认拉取" : isPush ? "确认推送" : isSwitch ? "确认切换" : isAbort ? "确认中止" : isFinish ? `确认完成${operationVerb}` : isUntracked ? "确认删除" : "确认撤销";
  return <div className="backdrop git-confirmation-backdrop" role="dialog" aria-modal="true" aria-labelledby="git-confirmation-title"><section className={`modal git-confirmation-dialog${isDestructive ? " destructive" : ""}`}><header><div><span>{categoryLabel}</span><h2 id="git-confirmation-title">{title}</h2></div><button type="button" className="git-confirmation-close" title="关闭" aria-label="关闭" disabled={busy} onClick={close}><CloseIcon /></button></header><div className="git-confirmation-body">
    {confirmation.type === "commit" ? <><p className="git-confirmation-lead">将在 <b>{snapshot?.head.branch || "当前 HEAD"}</b> 创建本地提交，包含 {stagedCount} 个已暂存文件。</p><pre>{commitMessage}</pre></>
      : confirmation.type === "amend" ? <><div className="git-confirmation-risk"><span>注意</span><p>修改最近一次提交会改写本地历史。若已推送到远端，普通推送会被拒绝（需强制推送）。</p></div><p className="git-confirmation-lead">将 <b>{snapshot?.head.branch || "当前 HEAD"}</b> 的最近一次提交改写为：</p><pre>{commitMessage}</pre>{stagedCount > 0 ? <div className="git-confirmation-scope"><span>操作说明</span><b>已暂存的 {stagedCount} 个文件改动会一并并入该提交</b></div> : <div className="git-confirmation-scope"><span>操作说明</span><b>无已暂存改动，仅重写提交信息</b></div>}</>
      : confirmation.type === "fetch" ? <><p className="git-confirmation-lead">从 <b>{confirmation.remote}</b> 获取最新更新。</p><div className="git-confirmation-scope"><span>操作说明</span><b>更新远端引用并清理已删除的分支引用</b><small>工作树不会改变</small></div></>
      : confirmation.type === "pull" ? <><p className="git-confirmation-lead">从 <b>{confirmation.remote}</b> 拉取 <b>{confirmation.branch}</b> 并合并最新更新。</p><div className="git-confirmation-scope"><span>操作说明</span><b>仅允许快进合并，历史分叉时会失败以保护提交</b><small>有未提交改动时，冲突文件可能导致拉取失败</small></div></>
      : confirmation.type === "push" ? <><p className="git-confirmation-lead">将 <b>{confirmation.branch}</b> 推送到 <b>{confirmation.remote}</b>。</p><div className="git-confirmation-scope"><span>操作说明</span><b>仅允许普通推送，non-fast-forward 将被拒绝</b>{confirmation.setUpstream ? <small>将同时设置上游跟踪</small> : null}</div></>
      : confirmation.type === "switch-branch" ? <><div className="git-confirmation-risk"><span>注意</span><p>切换分支前需确保工作区干净且无进行中的 AI 任务。</p></div><p className="git-confirmation-lead">将切换到分支 <b>{confirmation.branch}</b>。</p><div className="git-confirmation-scope"><span>当前分支</span><b>{snapshot?.head.branch || "HEAD"}</b></div></>
      : confirmation.type === "conflict-abort" ? <><div className="git-confirmation-risk"><span>注意</span><p>{riskLabel}</p></div>{context && context.operationType !== "none" ? <p className="git-confirmation-lead">正在{operationVerb} <b>{context.theirsLabel || ""} → {context.oursLabel}</b>，中止后将回到操作前状态。</p> : <p className="git-confirmation-lead">将中止当前冲突操作并丢弃所有已做解决的改动。</p>}</>
      : confirmation.type === "conflict-finish" ? <><p className="git-confirmation-lead">所有冲突文件都已解决，将{operationVerb} <b>{context?.theirsLabel || ""} → {context?.oursLabel || ""}</b> 并结束本次操作。</p><div className="git-confirmation-scope"><span>操作说明</span><b>解决结果会以{operationVerb}提交的形式保留</b></div></>
      : <><div className="git-confirmation-risk"><span>注意</span><p>{riskLabel}</p></div>{confirmation.type === "discard-all" ? <><p className="git-confirmation-lead">已暂存和工作区中的受跟踪改动都会恢复到 <code>HEAD</code>。</p><div className="git-confirmation-scope"><span>受影响内容</span><b>{trackedChangeCount} 个受跟踪文件</b>{untrackedChangeCount > 0 ? <small>另有 {untrackedChangeCount} 个未跟踪文件可选删除</small> : null}</div><label className="git-confirmation-check"><input type="checkbox" checked={includeUntracked} onChange={(event) => setIncludeUntracked(event.target.checked)} />同时删除未跟踪文件</label></> : <><p className="git-confirmation-lead">{isUntracked ? "将永久删除以下未跟踪文件。" : "将以下文件恢复到暂存版本，已暂存内容不会改变。"}</p><code className="git-confirmation-path">{discardPath}</code></>}</>}
  </div><footer><button type="button" className="secondary" disabled={busy} onClick={close}>取消</button><button type="button" className={isDestructive ? "primary danger" : "primary"} disabled={busy} onClick={confirm}>{confirmLabel}</button></footer></section></div>;
}

async function copyToClipboard(text: string): Promise<boolean> {
  if (navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // 回退到传统拷贝方式
    }
  }
  try {
    const textarea = document.createElement("textarea");
    textarea.value = text;
    textarea.setAttribute("readonly", "");
    textarea.style.position = "fixed";
    textarea.style.opacity = "0";
    document.body.append(textarea);
    textarea.select();
    const ok = document.execCommand("copy");
    textarea.remove();
    return ok;
  } catch {
    return false;
  }
}

// 可点击复制完整 commit id 的短 id 徽标；点击复制后短暂显示“已复制”。
function CopyOID({ oid, title }: { oid: string; title?: string }) {
  const [copied, setCopied] = useState(false);
  const copy = async (event: { stopPropagation: () => void }) => {
    event.stopPropagation();
    const ok = await copyToClipboard(oid);
    if (ok) {
      setCopied(true);
      toast.success("已复制提交 ID");
      window.setTimeout(() => setCopied(false), 1200);
    } else {
      toast.error("复制失败，请手动复制");
    }
  };
  return <code className={copied ? "git-oid copied" : "git-oid"} title={title ?? `点击复制完整提交 ID：${oid}`} role="button" tabIndex={0} aria-label={`复制提交 ID ${oid}`} onClick={copy} onKeyDown={(event) => { if (event.key === "Enter" || event.key === " ") { event.preventDefault(); void copy(event); } }}>{copied ? <span className="git-oid-copied">已复制</span> : shortOID(oid)}</code>;
}

function CommitHistory({ commits, loading, selectedOID, onSelect }: { commits: GitCommit[]; loading: boolean; selectedOID?: string; onSelect: (commit: GitCommit) => void }) {
  if (loading) return <div className="git-empty">正在读取该分支的历史</div>;
  if (commits.length === 0) return <div className="git-empty">该分支没有可显示的提交记录</div>;
  return <div className="git-history">{commits.map((commit) => <article key={commit.oid} className={commit.oid === selectedOID ? "selected" : ""} title="点击查看该提交的变更" tabIndex={0} onClick={() => onSelect(commit)} onKeyDown={(event) => { if (event.key === "Enter" || event.key === " ") { event.preventDefault(); onSelect(commit); } }}><CopyOID oid={commit.oid} /><div><b>{commit.subject || "(无提交说明)"}</b><span>{commit.parents.length > 1 ? <i className="git-commit-merge-badge" title="合并提交">合并</i> : null}{commit.author} · {formatGitTime(commit.authoredAt)}</span></div></article>)}</div>;
}

// 提交详情视图：头部提交摘要，正文为提交信息全文 + 文件列表|差异内容双栏（与“变更”tab 同构）。
function CommitDetailView({ commit, detail, loading, error, selectedFile, fileDiff, fileDiffLoading, onOpenFile, onCloseFile, onBack }: { commit: GitCommit; detail: GitCommitDetail | null; loading: boolean; error: string; selectedFile: GitCommitFile | null; fileDiff: GitDiff | null; fileDiffLoading: boolean; onOpenFile: (file: GitCommitFile) => void; onCloseFile: () => void; onBack: () => void }) {
  const isMerge = commit.parents.length > 1;
  const body = detail && detail.message !== detail.subject ? detail.message.slice(detail.subject.length).replace(/^\n+/, "") : "";
  return <div className="git-commit-detail">
    <header className="git-commit-detail-header">
      <button type="button" className="git-commit-back" onClick={onBack} title="返回提交历史">‹ 返回历史</button>
      <div className="git-commit-detail-meta">
        <b>{commit.subject || "(无提交说明)"}</b>
        <span><CopyOID oid={commit.oid} /> {commit.author} · {formatGitTime(commit.authoredAt)}</span>
      </div>
      {isMerge ? <i className="git-commit-merge-badge" title="合并提交，显示相对第一个父提交的差异">合并</i> : null}
    </header>
    {body ? <pre className="git-commit-message">{body}</pre> : null}
    {error ? <div className="git-empty">{error}</div> : <div className="git-commit-detail-body">
      <div className="git-commit-files" aria-label="变更文件列表">
        {loading ? <div className="git-empty">正在读取提交详情</div> : !detail ? <div className="git-empty">暂无提交详情</div> : detail.files.length === 0 ? <div className="git-empty">该提交没有文件变更</div> : detail.files.map((file) => <button type="button" key={file.path} className={selectedFile?.path === file.path ? "selected" : ""} title="点击查看该文件的差异" onClick={() => onOpenFile(file)}><span className={`git-commit-file-status ${file.status}`}>{commitFileStatusLabel(file.status)}</span><span className="git-commit-file-path"><b>{file.path}</b>{file.originalPath ? <small>{file.originalPath}</small> : null}</span><span className="git-commit-file-stats">{file.binary ? <i>二进制</i> : <><i className="additions">+{file.additions}</i><i className="deletions">-{file.deletions}</i></>}</span></button>)}
      </div>
      <section className="git-changes-content" aria-label="文件差异">
        {fileDiff ? <DiffViewer diff={fileDiff} close={onCloseFile} /> : <div className="git-diff-placeholder"><div className="git-diff-placeholder-icon" aria-hidden="true"><PendingIcon /></div><h3>{fileDiffLoading ? "正在读取差异" : "选择一个文件查看变更"}</h3><p>{fileDiffLoading ? "差异内容即将显示。" : "从左侧变更文件列表中点击文件，该提交中的差异内容会显示在这里。"}</p></div>}
      </section>
    </div>}
  </div>;
}

function Branches({ branches, mutating, projectID, conversationId, request, fail, requestSwitchBranch, requestCreateBranch }: { branches: GitBranch[]; mutating: string; projectID: string; conversationId?: string; request: Request; fail: (message: string) => void; requestSwitchBranch: (branch: string) => void; requestCreateBranch: (name: string, startPoint: string) => void }) {
	const withWorkspace = (path: string) => `${path}${path.includes("?") ? "&" : "?"}${conversationId ? `conversationId=${encodeURIComponent(conversationId)}` : ""}`;
  const [showForm, setShowForm] = useState(false);
  const [newName, setNewName] = useState("");
  const [startPoint, setStartPoint] = useState("");
  const [selected, setSelected] = useState("");
  const [branchCommits, setBranchCommits] = useState<GitCommit[]>([]);
  const [historyLoading, setHistoryLoading] = useState(false);
  const [historyHasMore, setHistoryHasMore] = useState(false);
  const [historyQuery, setHistoryQuery] = useState("");
  const [pendingQuery, setPendingQuery] = useState("");
  const historyRequest = useRef(0);
  const prevSelectionRef = useRef("");
  const [selectedCommit, setSelectedCommit] = useState<GitCommit | null>(null);
  const [commitDetail, setCommitDetail] = useState<GitCommitDetail | null>(null);
  const [detailLoading, setDetailLoading] = useState(false);
  const [detailError, setDetailError] = useState("");
  const [selectedFile, setSelectedFile] = useState<GitCommitFile | null>(null);
  const [fileDiff, setFileDiff] = useState<GitDiff | null>(null);
  const [fileDiffLoading, setFileDiffLoading] = useState(false);
  const detailRequest = useRef(0);
  const fileDiffRequest = useRef(0);
  const local = branches.filter((b) => !b.remote);
  const remote = branches.filter((b) => b.remote);
  const busy = Boolean(mutating);
  const currentName = branches.find((b) => b.current)?.name || "";
  // 选中分支不存在时回退到当前分支，保证始终有一个分支处于选中态。
  const effectiveSelection = selected && branches.some((b) => b.name === selected) ? selected : currentName;
  const handleCreate = () => {
    if (!newName.trim()) return;
    requestCreateBranch(newName.trim(), startPoint.trim());
    setNewName("");
    setStartPoint("");
    setShowForm(false);
    setSelected(newName.trim());
  };
  // 分支切换（currentName 变化）后重置检索词，回到该分支第一页。
  useEffect(() => {
    if (prevSelectionRef.current === effectiveSelection) return;
    prevSelectionRef.current = effectiveSelection;
    setHistoryQuery("");
    setPendingQuery("");
  }, [effectiveSelection]);

  // 检索词防抖后再提交，避免每次按键都跑一遍 git log。
  useEffect(() => {
    const timer = window.setTimeout(() => setPendingQuery(historyQuery), 300);
    return () => window.clearTimeout(timer);
  }, [historyQuery]);

  // 按选中分支拉取提交历史第一页；分支或检索词变化后自动回到第一页。
  useEffect(() => {
    if (!projectID || !effectiveSelection) return;
    const requestID = ++historyRequest.current;
    setHistoryLoading(true);
    const params = new URLSearchParams({ ref: effectiveSelection, limit: String(HISTORY_PAGE_SIZE) });
    if (pendingQuery) params.set("q", pendingQuery);
    request<GitCommit[]>(withWorkspace(`/api/projects/${projectID}/git/log?${params.toString()}`))
      .then((commits) => { if (requestID === historyRequest.current) { setHistoryHasMore(commits.length === HISTORY_PAGE_SIZE); setBranchCommits(commits); } })
      .catch((cause) => { if (requestID === historyRequest.current) { setHistoryHasMore(false); setBranchCommits([]); fail(cause instanceof Error ? cause.message : "无法读取该分支的历史"); } })
      .finally(() => { if (requestID === historyRequest.current) setHistoryLoading(false); });
    return () => { historyRequest.current++; };
    // withWorkspace 仅追加 conversationId，纳入依赖会导致每次渲染重复拉取。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [projectID, effectiveSelection, pendingQuery, request, fail]);

  // 加载更多：在当前已显示的提交之后继续取一页，相同检索词下追加。
  const loadMoreHistory = () => {
    if (!projectID || !effectiveSelection) return;
    const skip = branchCommits.length;
    const requestID = ++historyRequest.current;
    setHistoryLoading(true);
    const params = new URLSearchParams({ ref: effectiveSelection, limit: String(HISTORY_PAGE_SIZE), skip: String(skip) });
    if (pendingQuery) params.set("q", pendingQuery);
    request<GitCommit[]>(withWorkspace(`/api/projects/${projectID}/git/log?${params.toString()}`))
      .then((commits) => { if (requestID === historyRequest.current) { setHistoryHasMore(commits.length === HISTORY_PAGE_SIZE); setBranchCommits((prev) => [...prev, ...commits]); } })
      .catch((cause) => { if (requestID === historyRequest.current) fail(cause instanceof Error ? cause.message : "无法读取该分支的历史"); })
      .finally(() => { if (requestID === historyRequest.current) setHistoryLoading(false); });
  };

  // 切换分支时退出提交详情，避免残留其它分支的详情数据。
  useEffect(() => {
    detailRequest.current++;
    fileDiffRequest.current++;
    setSelectedCommit(null);
    setCommitDetail(null);
    setDetailError("");
    setSelectedFile(null);
    setFileDiff(null);
  }, [effectiveSelection]);

  const openCommit = (commit: GitCommit) => {
    fileDiffRequest.current++;
    setSelectedFile(null);
    setFileDiff(null);
    if (selectedCommit?.oid === commit.oid) return;
    setSelectedCommit(commit);
    setCommitDetail(null);
    setDetailError("");
    const requestID = ++detailRequest.current;
    setDetailLoading(true);
    request<GitCommitDetail>(withWorkspace(`/api/projects/${projectID}/git/commits/${commit.oid}`))
      .then((detail) => { if (requestID === detailRequest.current) setCommitDetail(detail); })
      .catch((cause) => { if (requestID === detailRequest.current) setDetailError(cause instanceof Error ? cause.message : "无法读取提交详情"); })
      .finally(() => { if (requestID === detailRequest.current) setDetailLoading(false); });
  };

  const closeCommit = () => {
    detailRequest.current++;
    fileDiffRequest.current++;
    setSelectedCommit(null);
    setCommitDetail(null);
    setDetailError("");
    setSelectedFile(null);
    setFileDiff(null);
  };

  const openCommitFile = (file: GitCommitFile) => {
    if (!selectedCommit) return;
    setSelectedFile(file);
    const requestID = ++fileDiffRequest.current;
    setFileDiff(null);
    setFileDiffLoading(true);
    request<{ oid: string; path: string; content: string }>(withWorkspace(`/api/projects/${projectID}/git/commits/${selectedCommit.oid}/diff?path=${encodeURIComponent(file.path)}`))
      .then((payload) => { if (requestID === fileDiffRequest.current) setFileDiff({ path: payload.path, stage: "commit", content: payload.content }); })
      .catch((cause) => { if (requestID === fileDiffRequest.current) fail(cause instanceof Error ? cause.message : "无法读取提交差异"); })
      .finally(() => { if (requestID === fileDiffRequest.current) setFileDiffLoading(false); });
  };

  const closeCommitFile = () => {
    fileDiffRequest.current++;
    setSelectedFile(null);
    setFileDiff(null);
  };
  const branchRow = (branch: GitBranch, tag: ReactNode) => {
    const isSelected = branch.name === effectiveSelection;
    return <article key={branch.name} className={isSelected ? "selected" : ""} title={isSelected ? "已选中，右侧显示该分支历史" : "点击查看该分支历史"} tabIndex={0} onClick={() => setSelected(branch.name)} onKeyDown={(event) => { if (event.key === "Enter" || event.key === " ") { event.preventDefault(); setSelected(branch.name); } }}>{tag}<b>{branch.name}</b><small className="git-branch-upstream">{branch.upstream || (branch.remote ? "" : "无上游")}</small>{!branch.remote && !branch.current && <button type="button" className="git-branch-switch-btn" disabled={busy} onClick={(event) => { event.stopPropagation(); requestSwitchBranch(branch.name); }}>切换</button>}</article>;
  };
  if (branches.length === 0) return <div className="git-branches-view"><div className="git-empty">没有可显示的分支</div></div>;
  return <div className="git-branches-view">
    <div className="git-branch-toolbar">
      <button type="button" className="secondary" disabled={busy} onClick={() => setShowForm(!showForm)}>新建分支</button>
    </div>
    {showForm && <div className="git-branch-create">
      <input type="text" value={newName} onChange={(e) => setNewName(e.target.value)} placeholder="分支名" disabled={busy} className="git-branch-input" />
      <input type="text" value={startPoint} onChange={(e) => setStartPoint(e.target.value)} placeholder="起始点（可选，默认为 HEAD）" disabled={busy} className="git-branch-input" />
      <button type="button" className="primary" disabled={busy || !newName.trim()} onClick={handleCreate}>创建</button>
      <button type="button" className="secondary" disabled={busy} onClick={() => setShowForm(false)}>取消</button>
    </div>}
    <div className="git-branch-layout">
      <div className="git-branch-list-pane" aria-label="分支列表">
        {local.length > 0 && <div className="git-branches"><h3 className="git-branch-group-title">本地分支</h3>{local.map((branch) => branchRow(branch, <span className={branch.current ? "current" : ""}>{branch.current ? "当前" : "本地"}</span>))}</div>}
        {remote.length > 0 && <div className="git-branches"><h3 className="git-branch-group-title">远端分支</h3>{remote.map((branch) => branchRow(branch, <span>远端</span>))}</div>}
      </div>
      <div className="git-branch-history-pane" aria-label={selectedCommit ? `提交 ${selectedCommit.oid} 的详情` : `分支 ${effectiveSelection} 的提交历史`}>
        {selectedCommit ? <CommitDetailView commit={selectedCommit} detail={commitDetail} loading={detailLoading} error={detailError} selectedFile={selectedFile} fileDiff={fileDiff} fileDiffLoading={fileDiffLoading} onOpenFile={openCommitFile} onCloseFile={closeCommitFile} onBack={closeCommit} /> : <>
          <header className="git-branch-history-header"><h3>提交历史</h3><div className="git-history-search"><input type="text" value={historyQuery} placeholder="搜索提交信息…" aria-label="按提交信息搜索" onChange={(event) => setHistoryQuery(event.target.value)} />{historyQuery ? <button type="button" className="git-history-search-clear" title="清除搜索" aria-label="清除搜索" onClick={() => setHistoryQuery("")}>×</button> : null}</div><code className="git-branch-history-ref">{effectiveSelection}</code></header>
          <CommitHistory commits={branchCommits} loading={historyLoading} onSelect={openCommit} />
          {historyHasMore && !historyLoading ? <button type="button" className="git-history-load-more" onClick={loadMoreHistory}>加载更多提交</button> : null}
        </>}
      </div>
    </div>
  </div>;
}

function operationStatusLabel(status: string): string { return ({ queued: "等待中", running: "进行中", succeeded: "已完成", failed: "失败", cancelled: "已取消", needs_attention: "需检查" })[status] || status; }

function operationTypeLabel(type: string): string { return ({ stage: "暂存", unstage: "取消暂存", stage_all: "全部暂存", unstage_all: "全部取消暂存", commit: "提交", commit_amend: "修改提交", discard_worktree: "撤销工作区改动", discard_all: "丢弃全部改动", fetch: "获取", pull: "拉取", push: "推送", create_branch: "创建分支", switch_branch: "切换分支", resolve_conflict: "解决冲突", conflict_abort: "中止操作", conflict_continue: "完成操作" })[type] || type; }

function Operations({ operations, loading = false }: { operations: GitOperation[]; loading?: boolean }) { if (loading && operations.length === 0) return <div className="git-empty">正在读取操作记录</div>; if (operations.length === 0) return <div className="git-empty">暂无操作记录</div>; return <div className="git-operations">{operations.map((operation) => <article key={operation.id}><span className={`git-operation-status ${operation.status}`}>{operationStatusLabel(operation.status)}</span><div><b>{operationTypeLabel(operation.type)}</b><p>{operation.requestSummary}</p>{operation.errorMessage ? <small>{operation.errorMessage}</small> : null}</div><time title={operation.finishedAt || operation.startedAt || operation.requestedAt}>{formatGitTime(operation.finishedAt || operation.startedAt || operation.requestedAt)}</time></article>)}</div>; }
