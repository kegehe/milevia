import { useCallback, useEffect, useMemo, useRef, useState, type KeyboardEvent } from "react";

import { changeState, formatGitTime, groupChanges, shortOID, type GitBranch, type GitChange, type GitCommit, type GitDiff, type GitOperation, type GitSnapshot, type GitWorktreeSummary } from "./git-model";

type Request = <T>(path: string, init?: RequestInit) => Promise<T>;
type Tab = "overview" | "changes" | "history" | "branches" | "operations";
type GitOperationResult = { operationId: string; status: "succeeded" | "failed" | "needs_attention"; errorMessage?: string };
type DiffLine = { content: string; kind: "added" | "removed" | "context" | "hunk" | "meta"; oldLine?: number; newLine?: number };
type Confirmation =
  | { type: "commit" }
  | { type: "discard-worktree"; path: string; untracked: boolean }
  | { type: "discard-all" }
  | { type: "fetch"; remote: string }
  | { type: "push"; remote: string; branch: string; setUpstream: boolean }
  | { type: "switch-branch"; branch: string };

const tabs: { id: Tab; label: string }[] = [
  { id: "overview", label: "概览" },
  { id: "changes", label: "变更" },
  { id: "history", label: "历史" },
  { id: "branches", label: "分支" },
  { id: "operations", label: "操作记录" },
];

export function GitWorkbench({ projectID, request, fail, active }: { projectID: string; request: Request; fail: (message: string) => void; active: boolean }) {
  const [tab, setTab] = useState<Tab>("overview");
  const [snapshot, setSnapshot] = useState<GitSnapshot | null>(null);
  const [changes, setChanges] = useState<GitChange[]>([]);
  const [commits, setCommits] = useState<GitCommit[]>([]);
  const [branches, setBranches] = useState<GitBranch[]>([]);
  const [operations, setOperations] = useState<GitOperation[]>([]);
  const [selectedDiff, setSelectedDiff] = useState<GitDiff | null>(null);
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [mutating, setMutating] = useState("");
  const [commitMessage, setCommitMessage] = useState("");
  const [confirmation, setConfirmation] = useState<Confirmation | null>(null);
  const diffRequest = useRef(0);
  const reloadRequest = useRef(0);
  const mountedRef = useRef(true);
  useEffect(() => {
    mountedRef.current = true;
    return () => { mountedRef.current = false; };
  }, []);

  const closeDiff = () => { diffRequest.current++; setSelectedDiff(null); };
  const selectTab = (next: Tab) => { setTab(next); closeDiff(); };
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
      const [nextSnapshot, nextChanges, nextCommits, nextBranches, nextOperations] = await Promise.all([
        request<GitSnapshot>(`${base}/summary`),
        request<GitChange[]>(`${base}/changes`),
        request<GitCommit[]>(`${base}/log?ref=HEAD&limit=50`),
        request<GitBranch[]>(`${base}/branches`),
        request<GitOperation[]>(`${base}/operations`),
      ]);
      if (!mountedRef.current || requestID !== reloadRequest.current) return;
      setSnapshot(nextSnapshot);
      setChanges(nextChanges);
      setCommits(nextCommits);
      setBranches(nextBranches);
      setOperations(nextOperations);
    } catch (cause) {
      if (mountedRef.current && requestID === reloadRequest.current) fail(cause instanceof Error ? cause.message : "无法读取 Git 仓库");
    } finally {
      if (mountedRef.current && requestID === reloadRequest.current) {
        setLoading(false);
        setRefreshing(false);
      }
    }
  }, [projectID, request, fail]);

  useEffect(() => { if (active) void reload().catch(() => undefined); }, [active, reload]);

  const grouped = useMemo(() => groupChanges(changes), [changes]);
  const changeCount = changes.length;
  const trackedChangeCount = changes.filter((change) => !change.untracked).length;
  const untrackedChangeCount = changes.filter((change) => change.untracked).length;
  const openDiff = async (change: GitChange, stage: "worktree" | "index") => {
    const requestID = ++diffRequest.current;
    try {
      const diff = await request<GitDiff>(`/api/projects/${projectID}/git/diff?path=${encodeURIComponent(change.path)}&stage=${stage}`);
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
      const result = await request<GitOperationResult>(`/api/projects/${projectID}/git/${endpoint}`, { method: "POST", body: JSON.stringify({ ...payload, stateToken: snapshot.stateToken }) });
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
  const discardWorktree = (path: string, untracked: boolean) => mutate(`discard:${path}`, "discard", { mode: "worktree", paths: [path], includeUntracked: untracked }, () => setConfirmation(null));
  const discardAll = (includeUntracked: boolean) => mutate("discard-all", "discard", { mode: "all", includeUntracked }, () => setConfirmation(null));
  const fetchRemote = (remote: string) => mutate("fetch", "fetch", { remote }, () => setConfirmation(null));
  const pushBranch = (remote: string, branch: string, setUpstream: boolean) => mutate("push", "push", { remote, branch, setUpstream }, () => setConfirmation(null));
  const switchBranch = (branch: string) => mutate("switch-branch", "switch", { branch }, () => setConfirmation(null));

  const createBranch = async (name: string, startPoint: string) => {
    if (!mountedRef.current) return;
    setMutating("create-branch");
    try {
      const result = await request<GitOperationResult>(`/api/projects/${projectID}/git/branches`, { method: "POST", body: JSON.stringify({ name, startPoint }) });
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
    <nav className="git-tabs" aria-label="Git工作台视图">
      <div className="git-tab-list" role="tablist" aria-label="Git工作台视图">{tabs.map((item) => <button type="button" key={item.id} id={`git-tab-${item.id}`} role="tab" aria-controls={`git-view-${item.id}`} className={tab === item.id ? "active" : ""} aria-selected={tab === item.id} tabIndex={tab === item.id ? 0 : -1} onClick={() => selectTab(item.id)} onKeyDown={(event) => handleTabKeyDown(event, item.id)}>{item.label}{item.id === "changes" && changeCount > 0 ? <b>{changeCount}</b> : null}</button>)}</div>
      <div className="git-workbench-actions"><button type="button" className={`git-refresh${refreshing ? " spinning" : ""}`} title={refreshing ? "正在刷新仓库状态" : "刷新仓库状态"} aria-label={refreshing ? "正在刷新仓库状态" : "刷新仓库状态"} disabled={refreshing || Boolean(mutating)} onClick={() => void reload(true)}><RefreshIcon /></button></div>
    </nav>
    <main className="git-workbench-body">
      {loading ? <div className="git-empty">正在读取仓库状态</div> : <>
        {tab === "overview" && <div id="git-view-overview" role="tabpanel" aria-labelledby="git-tab-overview"><Overview snapshot={snapshot} changeCount={changeCount} commits={commits} branches={branches} operations={operations} requestFetch={() => setConfirmation({ type: "fetch", remote: snapshot?.head.upstream?.split("/")[0] || "origin" })} requestPush={() => setConfirmation({ type: "push", remote: snapshot?.head.upstream?.split("/")[0] || "origin", branch: snapshot?.head.branch || "", setUpstream: !snapshot?.head.upstream })} mutating={mutating} /></div>}
        {tab === "changes" && <div id="git-view-changes" role="tabpanel" aria-labelledby="git-tab-changes"><Changes grouped={grouped} selectedDiff={selectedDiff} openDiff={openDiff} closeDiff={closeDiff} mutatePath={mutatePath} stageAll={stageAll} unstageAll={unstageAll} requestDiscardWorktree={(path, untracked) => setConfirmation({ type: "discard-worktree", path, untracked })} requestDiscardAll={() => setConfirmation({ type: "discard-all" })} requestCommit={() => setConfirmation({ type: "commit" })} commitMessage={commitMessage} setCommitMessage={setCommitMessage} mutating={mutating} changeCount={changeCount} /></div>}
        {tab === "history" && <div id="git-view-history" role="tabpanel" aria-labelledby="git-tab-history"><History commits={commits} /></div>}
        {tab === "branches" && <div id="git-view-branches" role="tabpanel" aria-labelledby="git-tab-branches"><Branches branches={branches} snapshot={snapshot} mutating={mutating} requestFetch={() => setConfirmation({ type: "fetch", remote: snapshot?.head.upstream?.split("/")[0] || "origin" })} requestPush={() => setConfirmation({ type: "push", remote: snapshot?.head.upstream?.split("/")[0] || "origin", branch: snapshot?.head.branch || "", setUpstream: !snapshot?.head.upstream })} requestSwitchBranch={(branch) => setConfirmation({ type: "switch-branch", branch })} requestCreateBranch={(name, startPoint) => void createBranch(name, startPoint)} /></div>}
        {tab === "operations" && <div id="git-view-operations" role="tabpanel" aria-labelledby="git-tab-operations"><Operations operations={operations} /></div>}
      </>}
    </main>
    {confirmation && <GitConfirmation confirmation={confirmation} snapshot={snapshot} stagedCount={grouped.staged.length} trackedChangeCount={trackedChangeCount} untrackedChangeCount={untrackedChangeCount} commitMessage={commitMessage} busy={Boolean(mutating)} close={() => setConfirmation(null)} commit={commit} discardWorktree={discardWorktree} discardAll={discardAll} fetchRemote={fetchRemote} pushBranch={pushBranch} switchBranch={switchBranch} />}
  </section>;
}

function Overview({ snapshot, changeCount, commits, branches, operations, requestFetch, requestPush, mutating }: { snapshot: GitSnapshot | null; changeCount: number; commits: GitCommit[]; branches: GitBranch[]; operations: GitOperation[]; requestFetch: () => void; requestPush: () => void; mutating: string }) {
  if (!snapshot) return <div className="git-empty">无法读取仓库状态</div>;
  const { head, worktree } = snapshot;
  const remoteName = head.upstream?.split("/")[0] || "origin";
  const localBranches = branches.filter((b) => !b.remote);
  const remoteBranches = branches.filter((b) => b.remote);
  const currentBranch = branches.find((b) => b.current);
  const recentCommits = commits.slice(0, 6);
  const recentOps = operations.filter((op) => op.status === "succeeded" || op.status === "failed" || op.status === "needs_attention").slice(0, 5);
  const syncLabel = head.upstream ? (head.ahead === 0 && head.behind === 0 ? "与上游同步" : head.ahead > 0 && head.behind === 0 ? `领先 ${head.ahead}` : head.ahead === 0 && head.behind > 0 ? `落后 ${head.behind}` : `领先 ${head.ahead} · 落后 ${head.behind}`) : "";
  return <div className="git-overview">
    <section className="git-overview-hero" aria-label="仓库概览">
      <div className="git-hero-ref" title={head.oid}>
        <span className="git-hero-label">当前引用</span>
        <b>{head.detached ? "HEAD (游离指针)" : head.branch || "未初始化"}</b>
        <code>{shortOID(head.oid)}</code>
      </div>
      <dl className="git-hero-stats">
        <div className={head.ahead > 0 ? "same" : ""}><dt>领先</dt><dd>{head.ahead}</dd></div>
        <div className={head.behind > 0 ? "warn" : ""}><dt>落后</dt><dd>{head.behind}</dd></div>
        <div><dt>上游</dt><dd className="git-hero-upstream" title={head.upstream}>{head.upstream || "未设置"}</dd></div>
      </dl>
      <div className="git-hero-meta">
        {head.upstream ? <span>{syncLabel}</span> : <span>未跟踪上游</span>}
        {head.upstream ? <span><code>{remoteName}</code> 远端分支 {remoteBranches.length} 个</span> : null}
        {snapshot.observedAt ? <span>刷新于 {formatGitTime(snapshot.observedAt)}</span> : null}
      </div>
      <div className="git-overview-remote"><button type="button" className="secondary" disabled={Boolean(mutating)} onClick={requestFetch}>获取更新</button><button type="button" className="secondary" disabled={Boolean(mutating) || head.detached || !head.branch || head.ahead === 0} onClick={requestPush} title={head.detached || !head.branch ? "游离指针状态无法推送" : head.ahead === 0 ? "没有需要推送的提交" : undefined}>推送</button></div>
    </section>
    <GitStatPanel worktree={worktree} changeCount={changeCount} />
    <section className="git-overview-feeds" aria-label="仓库动态">
      <section className="git-overview-feed">
        <header><h3>最近提交</h3>{commits.length > 5 ? <button type="button" className="git-feed-more" onClick={() => document.getElementById("git-tab-history")?.click()}>查看全部</button> : null}</header>
        {recentCommits.length === 0 ? <div className="git-feed-empty">暂无提交</div> : <div className="git-feed-list">{recentCommits.map((commit) => <article key={commit.oid} title={commit.subject}><code>{shortOID(commit.oid)}</code><b>{commit.subject || "(无提交说明)"}</b><span>{commit.author}</span><time>{formatGitTime(commit.authoredAt)}</time></article>)}</div>}
      </section>
      <section className="git-overview-feed">
        <header><h3>分支</h3><span className="git-feed-count">{localBranches.length} 本地 · {remoteBranches.length} 远端</span></header>
        <div className="git-feed-branches">
          {currentBranch ? <article className="git-feed-current"><span className="git-feed-tag current">当前</span><b>{currentBranch.name}</b><small>{currentBranch.upstream || "未跟踪上游"}</small></article> : null}
          {localBranches.filter((b) => !b.current).slice(0, 4).map((branch) => <article key={branch.name}><span className="git-feed-tag">本地</span><b>{branch.name}</b><small>{branch.upstream || ""}</small></article>)}
        </div>
      </section>
      <section className="git-overview-feed">
        <header><h3>最近操作</h3>{operations.length > 5 ? <button type="button" className="git-feed-more" onClick={() => document.getElementById("git-tab-operations")?.click()}>查看全部</button> : null}</header>
        {recentOps.length === 0 ? <div className="git-feed-empty">暂无操作记录</div> : <div className="git-feed-list ops">{recentOps.map((op) => <article key={op.id}><span className={`git-operation-status ${op.status}`}>{operationStatusLabel(op.status)}</span><b>{operationTypeLabel(op.type)}</b><span>{op.errorMessage || op.requestSummary}</span><time>{formatGitTime(op.finishedAt || op.startedAt || op.requestedAt)}</time></article>)}</div>}
      </section>
    </section>
  </div>;
}

function GitStatPanel({ worktree, changeCount }: { worktree: GitWorktreeSummary; changeCount: number }) {
  const stats: { label: string; value: number; attention?: boolean; title?: string }[] = [
    { label: "变更文件", value: changeCount, title: "有改动的文件总数（一个文件可能同时计入下方多个分项）" },
    { label: "已暂存", value: worktree.staged, title: "已加入暂存区的文件" },
    { label: "未跟踪", value: worktree.untracked, title: "尚未被 Git 跟踪的新文件" },
    { label: "修改", value: worktree.modified, title: "已跟踪文件的内容改动" },
    { label: "删除", value: worktree.deleted, title: "已删除的受跟踪文件" },
    { label: "重命名", value: worktree.renamed, title: "已重命名/移动的文件" },
    { label: "冲突", value: worktree.conflicted, attention: worktree.conflicted > 0, title: "需要手动解决的合并冲突" },
  ];
  return <section className="git-stat-grid" aria-label="工作区统计">{stats.map((stat) => <div key={stat.label} className={stat.attention ? "attention" : ""} title={stat.title}><span>{stat.label}</span><b>{stat.value}</b></div>)}</section>;
}

function PlusIcon() { return <svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round"><path d="M8 3v10M3 8h10" /></svg>; }

function UndoIcon() { return <svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round"><path d="M13.5 11a5.5 5.5 0 0 0-9.4-3.9L2.5 8.7" /><path d="M2.5 4.5v4.2h4.2" /></svg>; }

function PendingIcon() { return <svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round"><path d="M14 8A6 6 0 1 1 8 2" /><polyline points="8 2 8 5.5 11 2.5" /></svg>; }

function RefreshIcon() { return <svg viewBox="0 0 16 16" width="16" height="16" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round"><path d="M13 5.8A5.5 5.5 0 1 0 13.4 10" /><path d="M13 2.5v3.3H9.7" /></svg>; }

function CloseIcon() { return <svg viewBox="0 0 16 16" width="16" height="16" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round"><path d="m4 4 8 8M12 4l-8 8" /></svg>; }

function Changes({ grouped, selectedDiff, openDiff, closeDiff, mutatePath, stageAll, unstageAll, requestDiscardWorktree, requestDiscardAll, requestCommit, commitMessage, setCommitMessage, mutating, changeCount }: { grouped: { staged: GitChange[]; worktree: GitChange[] }; selectedDiff: GitDiff | null; openDiff: (change: GitChange, stage: "worktree" | "index") => Promise<void>; closeDiff: () => void; mutatePath: (action: "stage" | "unstage", path: string) => void; stageAll: () => void; unstageAll: () => void; requestDiscardWorktree: (path: string, untracked: boolean) => void; requestDiscardAll: () => void; requestCommit: () => void; commitMessage: string; setCommitMessage: (value: string) => void; mutating: string; changeCount: number }) {
  return <div className="git-changes-view">
    <section className="git-change-group"><header><h3>已暂存</h3><div className="git-group-actions"><span>{grouped.staged.length}</span><button type="button" className={`git-icon-btn${mutating === "unstage-all" ? " spinning" : ""}`} title={mutating === "unstage-all" ? "取消暂存中" : "全部取消暂存"} aria-label="全部取消暂存" disabled={mutating !== "" || grouped.staged.length === 0} onClick={unstageAll}>{mutating === "unstage-all" ? <PendingIcon /> : <UndoIcon />}</button></div></header><ChangeList changes={grouped.staged} stage="index" openDiff={openDiff} mutatePath={mutatePath} requestDiscard={requestDiscardWorktree} mutating={mutating} empty="没有已暂存变更" /></section>
    <CommitPanel stagedCount={grouped.staged.length} value={commitMessage} setValue={setCommitMessage} open={requestCommit} disabled={mutating !== ""} />
    <section className="git-change-group"><header><h3>工作区</h3><div className="git-group-actions"><span>{grouped.worktree.length}</span><button type="button" className={`git-icon-btn${mutating === "stage-all" ? " spinning" : ""}`} title={mutating === "stage-all" ? "暂存中" : "全部暂存"} aria-label="全部暂存" disabled={mutating !== "" || grouped.worktree.length === 0} onClick={stageAll}>{mutating === "stage-all" ? <PendingIcon /> : <PlusIcon />}</button><button type="button" className="git-icon-btn danger" title="撤销全部未提交改动" aria-label="撤销全部未提交改动" disabled={mutating !== "" || changeCount === 0} onClick={requestDiscardAll}><UndoIcon /></button></div></header><ChangeList changes={grouped.worktree} stage="worktree" openDiff={openDiff} mutatePath={mutatePath} requestDiscard={requestDiscardWorktree} mutating={mutating} empty="工作区没有变更" /></section>
    {selectedDiff && <DiffViewer diff={selectedDiff} close={closeDiff} />}
  </div>;
}

export function parseDiffContent(content: string): DiffLine[] {
  const source = content.split(/\r?\n/);
  if (source.at(-1) === "") source.pop();
  const unified = source.some((line) => line.startsWith("@@ "));
  let oldLine = 1;
  let newLine = 1;
  return source.map((line) => {
    const hunk = line.match(/^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/);
    if (hunk) {
      oldLine = Number(hunk[1]);
      newLine = Number(hunk[2]);
      return { content: line, kind: "hunk" };
    }
    if (unified && (line.startsWith("diff --git ") || line.startsWith("index ") || line.startsWith("--- ") || line.startsWith("+++ ") || line.startsWith("\\ No newline"))) return { content: line, kind: "meta" };
    if (unified && line.startsWith("+")) return { content: line.slice(1), kind: "added", newLine: newLine++ };
    if (unified && line.startsWith("-")) return { content: line.slice(1), kind: "removed", oldLine: oldLine++ };
    if (unified && line.startsWith(" ")) return { content: line.slice(1), kind: "context", oldLine: oldLine++, newLine: newLine++ };
    return { content: line, kind: "context", oldLine: oldLine++, newLine: newLine++ };
  });
}

function DiffViewer({ diff, close }: { diff: GitDiff; close: () => void }) {
  const lines = parseDiffContent(diff.content);
  const stageLabel = diff.stage === "index" ? "已暂存差异" : "工作区差异";
  return <section className="git-diff"><header><div><span>{stageLabel}</span><b>{diff.path}</b></div><button type="button" className="git-close" title="关闭差异" aria-label="关闭差异" onClick={close}>×</button></header>{lines.length ? <div className="git-diff-code" role="region" aria-label={`${diff.path} 的${stageLabel}`}>{lines.map((line, index) => <div className={`git-diff-line ${line.kind}`} key={`${index}-${line.content}`}><span className="git-diff-number">{line.oldLine ?? ""}</span><span className="git-diff-number">{line.newLine ?? ""}</span><span className="git-diff-prefix" aria-hidden="true">{line.kind === "added" ? "+" : line.kind === "removed" ? "-" : ""}</span><code>{line.content}</code></div>)}</div> : <p className="git-diff-empty">该文件没有可显示的文本差异。</p>}</section>;
}

function ChangeList({ changes, stage, openDiff, mutatePath, requestDiscard, mutating, empty }: { changes: GitChange[]; stage: "worktree" | "index"; openDiff: (change: GitChange, stage: "worktree" | "index") => Promise<void>; mutatePath: (action: "stage" | "unstage", path: string) => void; requestDiscard: (path: string, untracked: boolean) => void; mutating: string; empty: string }) {
  if (changes.length === 0) return <p className="git-list-empty">{empty}</p>;
  const action = stage === "index" ? "unstage" : "stage";
  return <div className="git-change-list">{changes.map((change) => <div key={`${stage}-${change.path}`}><button type="button" onClick={() => void openDiff(change, stage)}><span className={`git-change-kind ${change.conflicted ? "conflicted" : ""}`}>{changeState(change)}</span><b>{change.path}</b>{change.originalPath ? <small>{change.originalPath}</small> : null}</button><div className="git-inline-actions"><button type="button" className={`git-inline-icon${mutating === `${action}:${change.path}` ? " spinning" : ""}`} title={mutating === `${action}:${change.path}` ? "处理中" : action === "stage" ? "暂存" : "取消暂存"} aria-label={action === "stage" ? "暂存" : "取消暂存"} disabled={mutating !== ""} onClick={() => mutatePath(action, change.path)}>{mutating === `${action}:${change.path}` ? <PendingIcon /> : action === "stage" ? <PlusIcon /> : <UndoIcon />}</button>{stage === "worktree" && <button type="button" className="git-inline-icon danger" title={change.untracked ? "删除未跟踪文件" : "撤销工作区改动"} aria-label={change.untracked ? "删除未跟踪文件" : "撤销工作区改动"} disabled={mutating !== "" || change.conflicted} onClick={() => requestDiscard(change.path, change.untracked)}>{change.untracked ? <svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round"><polyline points="2 4.5 14 4.5" /><path d="M5 4.5V3a1 1 0 0 1 1-1h4a1 1 0 0 1 1 1v1.5" /><path d="M3.5 4.5l.7 9a1.5 1.5 0 0 0 1.5 1.4h4.6a1.5 1.5 0 0 0 1.5-1.4l.7-9" /></svg> : <UndoIcon />}</button>}</div></div>)}</div>;
}

function CommitPanel({ stagedCount, value, setValue, open, disabled }: { stagedCount: number; value: string; setValue: (value: string) => void; open: () => void; disabled: boolean }) {
  return <section className="git-commit-panel"><label htmlFor="git-commit-message">提交信息</label><textarea id="git-commit-message" value={value} maxLength={4000} disabled={disabled || stagedCount === 0} onChange={(event) => setValue(event.target.value)} placeholder={stagedCount === 0 ? "暂存文件后即可提交" : "简要说明本次变更"} /><footer><span>{stagedCount === 0 ? "没有已暂存文件" : `将提交 ${stagedCount} 个文件`}</span><button type="button" className="primary" disabled={disabled || stagedCount === 0 || !value.trim() || value.split("\n")[0].length > 72} onClick={open}>提交</button></footer></section>;
}

function GitConfirmation({ confirmation, snapshot, stagedCount, trackedChangeCount, untrackedChangeCount, commitMessage, busy, close, commit, discardWorktree, discardAll, fetchRemote, pushBranch, switchBranch }: { confirmation: Confirmation; snapshot: GitSnapshot | null; stagedCount: number; trackedChangeCount: number; untrackedChangeCount: number; commitMessage: string; busy: boolean; close: () => void; commit: () => void; discardWorktree: (path: string, untracked: boolean) => void; discardAll: (includeUntracked: boolean) => void; fetchRemote: (remote: string) => void; pushBranch: (remote: string, branch: string, setUpstream: boolean) => void; switchBranch: (branch: string) => void }) {
  const [includeUntracked, setIncludeUntracked] = useState(confirmation.type === "discard-all" && untrackedChangeCount > 0 && trackedChangeCount === 0);
  const isCommit = confirmation.type === "commit";
  const isFetch = confirmation.type === "fetch";
  const isPush = confirmation.type === "push";
  const isSwitch = confirmation.type === "switch-branch";
  const isDestructive = confirmation.type === "discard-worktree" || confirmation.type === "discard-all";
  const title = isCommit ? "确认提交" : isFetch ? "获取远端更新" : isPush ? "推送到远端" : isSwitch ? "切换分支" : confirmation.type === "discard-all" ? "丢弃全部未提交改动" : confirmation.type === "discard-worktree" && confirmation.untracked ? "删除未跟踪文件" : "撤销工作区更改";
  const isUntracked = confirmation.type === "discard-worktree" && confirmation.untracked;
  const discardPath = confirmation.type === "discard-worktree" ? confirmation.path : "";
  const riskLabel = confirmation.type === "discard-worktree" && !confirmation.untracked ? "工作区内容将恢复到暂存版本" : "此操作无法在页面中撤销";
  const categoryLabel = isCommit ? "本地提交" : isFetch ? "远端操作" : isPush ? "远端操作" : isSwitch ? "分支操作" : "高风险操作";
  const confirm = () => {
    if (confirmation.type === "commit") commit();
    else if (confirmation.type === "discard-all") discardAll(includeUntracked);
    else if (confirmation.type === "discard-worktree") discardWorktree(confirmation.path, confirmation.untracked);
    else if (confirmation.type === "fetch") fetchRemote(confirmation.remote);
    else if (confirmation.type === "push") pushBranch(confirmation.remote, confirmation.branch, confirmation.setUpstream);
    else if (confirmation.type === "switch-branch") switchBranch(confirmation.branch);
  };
  const confirmLabel = busy ? "处理中" : isCommit ? "确认提交" : isFetch ? "确认获取" : isPush ? "确认推送" : isSwitch ? "确认切换" : isUntracked ? "确认删除" : "确认撤销";
  return <div className="backdrop git-confirmation-backdrop" role="dialog" aria-modal="true" aria-labelledby="git-confirmation-title"><section className={`modal git-confirmation-dialog${isDestructive ? " destructive" : ""}`}><header><div><span>{categoryLabel}</span><h2 id="git-confirmation-title">{title}</h2></div><button type="button" className="git-confirmation-close" title="关闭" aria-label="关闭" disabled={busy} onClick={close}><CloseIcon /></button></header><div className="git-confirmation-body">{confirmation.type === "commit" ? <><p className="git-confirmation-lead">将在 <b>{snapshot?.head.branch || "当前 HEAD"}</b> 创建本地提交，包含 {stagedCount} 个已暂存文件。</p><pre>{commitMessage}</pre></> : confirmation.type === "fetch" ? <><p className="git-confirmation-lead">从 <b>{confirmation.remote}</b> 获取最新更新。</p><div className="git-confirmation-scope"><span>操作说明</span><b>更新远端引用并清理已删除的分支引用</b><small>工作树不会改变</small></div></> : confirmation.type === "push" ? <><p className="git-confirmation-lead">将 <b>{confirmation.branch}</b> 推送到 <b>{confirmation.remote}</b>。</p><div className="git-confirmation-scope"><span>操作说明</span><b>仅允许普通推送，non-fast-forward 将被拒绝</b>{confirmation.setUpstream ? <small>将同时设置上游跟踪</small> : null}</div></> : confirmation.type === "switch-branch" ? <><div className="git-confirmation-risk"><span>注意</span><p>切换分支前需确保工作区干净且无进行中的 AI 任务。</p></div><p className="git-confirmation-lead">将切换到分支 <b>{confirmation.branch}</b>。</p><div className="git-confirmation-scope"><span>当前分支</span><b>{snapshot?.head.branch || "HEAD"}</b></div></> : <><div className="git-confirmation-risk"><span>注意</span><p>{riskLabel}</p></div>{confirmation.type === "discard-all" ? <><p className="git-confirmation-lead">已暂存和工作区中的受跟踪改动都会恢复到 <code>HEAD</code>。</p><div className="git-confirmation-scope"><span>受影响内容</span><b>{trackedChangeCount} 个受跟踪文件</b>{untrackedChangeCount > 0 ? <small>另有 {untrackedChangeCount} 个未跟踪文件可选删除</small> : null}</div><label className="git-confirmation-check"><input type="checkbox" checked={includeUntracked} onChange={(event) => setIncludeUntracked(event.target.checked)} />同时删除未跟踪文件</label></> : <><p className="git-confirmation-lead">{isUntracked ? "将永久删除以下未跟踪文件。" : "将以下文件恢复到暂存版本，已暂存内容不会改变。"}</p><code className="git-confirmation-path">{discardPath}</code></>}</>}</div><footer><button type="button" className="secondary" disabled={busy} onClick={close}>取消</button><button type="button" className={isDestructive ? "primary danger" : "primary"} disabled={busy} onClick={confirm}>{confirmLabel}</button></footer></section></div>;
}

function History({ commits }: { commits: GitCommit[] }) { return commits.length === 0 ? <div className="git-empty">没有可显示的提交记录</div> : <div className="git-history">{commits.map((commit) => <article key={commit.oid}><code>{shortOID(commit.oid)}</code><div><b>{commit.subject || "(无提交说明)"}</b><span>{commit.author} · {formatGitTime(commit.authoredAt)}</span></div></article>)}</div>; }

function Branches({ branches, snapshot, mutating, requestFetch, requestPush, requestSwitchBranch, requestCreateBranch }: { branches: GitBranch[]; snapshot: GitSnapshot | null; mutating: string; requestFetch: () => void; requestPush: () => void; requestSwitchBranch: (branch: string) => void; requestCreateBranch: (name: string, startPoint: string) => void }) {
  const [showForm, setShowForm] = useState(false);
  const [newName, setNewName] = useState("");
  const [startPoint, setStartPoint] = useState("");
  const local = branches.filter((b) => !b.remote);
  const remote = branches.filter((b) => b.remote);
  const busy = Boolean(mutating);
  const handleCreate = () => {
    if (!newName.trim()) return;
    requestCreateBranch(newName.trim(), startPoint.trim());
    setNewName("");
    setStartPoint("");
    setShowForm(false);
  };
  return <div className="git-branches-view">
    <div className="git-branch-toolbar">
      <button type="button" className="secondary" disabled={busy} onClick={requestFetch}>获取更新</button>
      <button type="button" className="secondary" disabled={busy || !snapshot?.head.branch} onClick={requestPush}>推送</button>
      <button type="button" className="secondary" disabled={busy} onClick={() => setShowForm(!showForm)}>新建分支</button>
    </div>
    {showForm && <div className="git-branch-create">
      <input type="text" value={newName} onChange={(e) => setNewName(e.target.value)} placeholder="分支名" disabled={busy} className="git-branch-input" />
      <input type="text" value={startPoint} onChange={(e) => setStartPoint(e.target.value)} placeholder="起始点（可选，默认为 HEAD）" disabled={busy} className="git-branch-input" />
      <button type="button" className="primary" disabled={busy || !newName.trim()} onClick={handleCreate}>创建</button>
      <button type="button" className="secondary" disabled={busy} onClick={() => setShowForm(false)}>取消</button>
    </div>}
    {branches.length === 0 ? <div className="git-empty">没有可显示的分支</div> : <>
      {local.length > 0 && <div className="git-branches"><h3 className="git-branch-group-title">本地分支</h3>{local.map((branch) => <article key={`local-${branch.name}`}><span className={branch.current ? "current" : ""}>{branch.current ? "当前" : "本地"}</span><b>{branch.name}</b><small>{branch.upstream || ""}</small>{!branch.current && <button type="button" className="git-branch-switch-btn" disabled={busy} onClick={() => requestSwitchBranch(branch.name)}>切换</button>}</article>)}</div>}
      {remote.length > 0 && <div className="git-branches"><h3 className="git-branch-group-title">远端分支</h3>{remote.map((branch) => <article key={`remote-${branch.name}`}><span>远端</span><b>{branch.name}</b><small>{branch.upstream || ""}</small></article>)}</div>}
    </>}
  </div>;
}

function operationStatusLabel(status: string): string { return ({ queued: "等待中", running: "进行中", succeeded: "已完成", failed: "失败", needs_attention: "需检查" })[status] || status; }

function operationTypeLabel(type: string): string { return ({ stage: "暂存", unstage: "取消暂存", stage_all: "全部暂存", unstage_all: "全部取消暂存", commit: "提交", discard_worktree: "撤销工作区改动", discard_all: "丢弃全部改动", fetch: "获取更新", push: "推送", create_branch: "创建分支", switch_branch: "切换分支" })[type] || type; }

function Operations({ operations }: { operations: GitOperation[] }) { return operations.length === 0 ? <div className="git-empty">暂无操作记录</div> : <div className="git-operations">{operations.map((operation) => <article key={operation.id}><span className={`git-operation-status ${operation.status}`}>{operationStatusLabel(operation.status)}</span><div><b>{operationTypeLabel(operation.type)}</b><p>{operation.requestSummary}</p>{operation.errorMessage ? <small>{operation.errorMessage}</small> : null}</div><time title={operation.finishedAt || operation.startedAt || operation.requestedAt}>{formatGitTime(operation.finishedAt || operation.startedAt || operation.requestedAt)}</time></article>)}</div>; }
