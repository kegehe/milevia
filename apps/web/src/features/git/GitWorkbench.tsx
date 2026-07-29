import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import { changeState, formatGitTime, groupChanges, shortOID, type GitBranch, type GitChange, type GitCommit, type GitDiff, type GitOperation, type GitSnapshot } from "./git-model";

type Request = <T>(path: string, init?: RequestInit) => Promise<T>;
type Tab = "overview" | "changes" | "history" | "branches" | "operations";
type GitOperationResult = { operationId: string; status: "succeeded" | "failed" | "needs_attention"; errorMessage?: string };
type Confirmation =
  | { type: "commit" }
  | { type: "discard-worktree"; path: string; untracked: boolean }
  | { type: "discard-all" };

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
  const mountedRef = useRef(true);
  useEffect(() => {
    mountedRef.current = true;
    return () => { mountedRef.current = false; };
  }, []);

  const closeDiff = () => { diffRequest.current++; setSelectedDiff(null); };

  const reload = useCallback(async (manual = false) => {
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
      if (!mountedRef.current) return;
      setSnapshot(nextSnapshot);
      setChanges(nextChanges);
      setCommits(nextCommits);
      setBranches(nextBranches);
      setOperations(nextOperations);
    } catch (cause) {
      if (mountedRef.current) fail(cause instanceof Error ? cause.message : "无法读取 Git 仓库");
    } finally {
      if (mountedRef.current) {
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
    if (!snapshot?.stateToken || !mountedRef.current) return;
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
      if (mountedRef.current) fail(cause instanceof Error ? cause.message : "无法执行 Git 操作");
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

  return <section id="workspace-panel-git" className="git-workbench workspace-panel" role="tabpanel" aria-labelledby="workspace-tab-git" hidden={!active}>
    <header className="git-workbench-head">
      <div><label>PROJECT REPOSITORY</label><h2>Git 工作台</h2><p>{snapshot?.head.detached ? "HEAD 分离状态" : snapshot?.head.branch || "读取仓库中"}</p></div>
      <div className="git-workbench-actions"><button type="button" className="secondary" disabled={refreshing || Boolean(mutating)} onClick={() => void reload(true)}>{refreshing ? "刷新中" : "刷新"}</button></div>
    </header>
    <nav className="git-tabs" aria-label="Git 工作台视图">{tabs.map((item) => <button type="button" key={item.id} className={tab === item.id ? "active" : ""} aria-pressed={tab === item.id} onClick={() => { setTab(item.id); closeDiff(); }}>{item.label}{item.id === "changes" && changeCount > 0 ? <b>{changeCount}</b> : null}</button>)}</nav>
    <main className="git-workbench-body">
      {loading ? <div className="git-empty">正在读取仓库状态</div> : <>
        {tab === "overview" && <Overview snapshot={snapshot} changeCount={changeCount} />}
        {tab === "changes" && <Changes grouped={grouped} selectedDiff={selectedDiff} openDiff={openDiff} closeDiff={closeDiff} mutatePath={mutatePath} stageAll={stageAll} unstageAll={unstageAll} requestDiscardWorktree={(path, untracked) => setConfirmation({ type: "discard-worktree", path, untracked })} requestDiscardAll={() => setConfirmation({ type: "discard-all" })} requestCommit={() => setConfirmation({ type: "commit" })} commitMessage={commitMessage} setCommitMessage={setCommitMessage} mutating={mutating} changeCount={changeCount} />}
        {tab === "history" && <History commits={commits} />}
        {tab === "branches" && <Branches branches={branches} />}
        {tab === "operations" && <Operations operations={operations} />}
      </>}
    </main>
    {confirmation && <GitConfirmation confirmation={confirmation} snapshot={snapshot} stagedCount={grouped.staged.length} trackedChangeCount={trackedChangeCount} untrackedChangeCount={untrackedChangeCount} commitMessage={commitMessage} busy={Boolean(mutating)} close={() => setConfirmation(null)} commit={commit} discardWorktree={discardWorktree} discardAll={discardAll} />}
  </section>;
}

function Overview({ snapshot, changeCount }: { snapshot: GitSnapshot | null; changeCount: number }) {
  if (!snapshot) return <div className="git-empty">无法读取仓库状态</div>;
  const { head, worktree } = snapshot;
  return <div className="git-overview">
    <section className="git-head-card"><div><span>当前引用</span><b>{head.detached ? "HEAD" : head.branch || "未初始化"}</b><code>{shortOID(head.oid)}</code></div><dl><div><dt>上游</dt><dd>{head.upstream || "未设置"}</dd></div><div><dt>领先</dt><dd>{head.ahead}</dd></div><div><dt>落后</dt><dd>{head.behind}</dd></div></dl></section>
    <section className="git-stat-grid" aria-label="工作区统计"><GitStat label="变更文件" value={changeCount} /><GitStat label="已暂存" value={worktree.staged} /><GitStat label="未跟踪" value={worktree.untracked} /><GitStat label="冲突" value={worktree.conflicted} /></section>
    <section className="git-summary-list"><div><span>工作区修改</span><b>{worktree.modified}</b></div><div><span>删除</span><b>{worktree.deleted}</b></div><div><span>重命名</span><b>{worktree.renamed}</b></div></section>
  </div>;
}

function GitStat({ label, value }: { label: string; value: number }) { return <div><span>{label}</span><b>{value}</b></div>; }

function Changes({ grouped, selectedDiff, openDiff, closeDiff, mutatePath, stageAll, unstageAll, requestDiscardWorktree, requestDiscardAll, requestCommit, commitMessage, setCommitMessage, mutating, changeCount }: { grouped: { staged: GitChange[]; worktree: GitChange[] }; selectedDiff: GitDiff | null; openDiff: (change: GitChange, stage: "worktree" | "index") => Promise<void>; closeDiff: () => void; mutatePath: (action: "stage" | "unstage", path: string) => void; stageAll: () => void; unstageAll: () => void; requestDiscardWorktree: (path: string, untracked: boolean) => void; requestDiscardAll: () => void; requestCommit: () => void; commitMessage: string; setCommitMessage: (value: string) => void; mutating: string; changeCount: number }) {
  return <div className="git-changes-view">
    <section className="git-change-group"><header><h3>已暂存</h3><div className="git-group-actions"><span>{grouped.staged.length}</span><button type="button" className={`git-icon-btn${mutating === "unstage-all" ? " spinning" : ""}`} title={mutating === "unstage-all" ? "取消暂存中" : "全部取消暂存"} aria-label="全部取消暂存" disabled={mutating !== "" || grouped.staged.length === 0} onClick={unstageAll}>{mutating === "unstage-all" ? <svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round"><path d="M14 8A6 6 0 1 1 8 2" /><polyline points="8 2 8 5.5 11 2.5" /></svg> : <svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round"><line x1="10" y1="8" x2="3" y2="8" /><polyline points="5.5 5.5 3 8 5.5 10.5" /></svg>}</button></div></header><ChangeList changes={grouped.staged} stage="index" openDiff={openDiff} mutatePath={mutatePath} requestDiscard={requestDiscardWorktree} mutating={mutating} empty="没有已暂存变更" /></section>
    <CommitPanel stagedCount={grouped.staged.length} value={commitMessage} setValue={setCommitMessage} open={requestCommit} disabled={mutating !== ""} />
    <section className="git-change-group"><header><h3>工作区</h3><div className="git-group-actions"><span>{grouped.worktree.length}</span><button type="button" className={`git-icon-btn${mutating === "stage-all" ? " spinning" : ""}`} title={mutating === "stage-all" ? "暂存中" : "全部暂存"} aria-label="全部暂存" disabled={mutating !== "" || grouped.worktree.length === 0} onClick={stageAll}>{mutating === "stage-all" ? <svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round"><path d="M14 8A6 6 0 1 1 8 2" /><polyline points="8 2 8 5.5 11 2.5" /></svg> : <svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round"><line x1="6" y1="8" x2="13" y2="8" /><polyline points="10.5 5.5 13 8 10.5 10.5" /></svg>}</button><button type="button" className="git-icon-btn danger" title="丢弃全部未提交改动" aria-label="丢弃全部未提交改动" disabled={mutating !== "" || changeCount === 0} onClick={requestDiscardAll}><svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round"><polyline points="2 4.5 14 4.5" /><path d="M5 4.5V3a1 1 0 0 1 1-1h4a1 1 0 0 1 1 1v1.5" /><path d="M3.5 4.5l.7 9a1.5 1.5 0 0 0 1.5 1.4h4.6a1.5 1.5 0 0 0 1.5-1.4l.7-9" /><line x1="6.5" y1="7" x2="6.5" y2="12.5" /><line x1="9.5" y1="7" x2="9.5" y2="12.5" /></svg></button></div></header><ChangeList changes={grouped.worktree} stage="worktree" openDiff={openDiff} mutatePath={mutatePath} requestDiscard={requestDiscardWorktree} mutating={mutating} empty="工作区没有变更" /></section>
    {selectedDiff && <section className="git-diff"><header><div><span>{selectedDiff.stage === "index" ? "已暂存差异" : "工作区差异"}</span><b>{selectedDiff.path}</b></div><button type="button" className="git-close" title="关闭差异" aria-label="关闭差异" onClick={closeDiff}>×</button></header><pre>{selectedDiff.content || "该文件没有可显示的文本差异。"}</pre></section>}
  </div>;
}

function ChangeList({ changes, stage, openDiff, mutatePath, requestDiscard, mutating, empty }: { changes: GitChange[]; stage: "worktree" | "index"; openDiff: (change: GitChange, stage: "worktree" | "index") => Promise<void>; mutatePath: (action: "stage" | "unstage", path: string) => void; requestDiscard: (path: string, untracked: boolean) => void; mutating: string; empty: string }) {
  if (changes.length === 0) return <p className="git-list-empty">{empty}</p>;
  const action = stage === "index" ? "unstage" : "stage";
  return <div className="git-change-list">{changes.map((change) => <div key={`${stage}-${change.path}`}><button type="button" onClick={() => void openDiff(change, stage)}><span className={`git-change-kind ${change.conflicted ? "conflicted" : ""}`}>{changeState(change)}</span><b>{change.path}</b>{change.originalPath ? <small>{change.originalPath}</small> : null}</button><div className="git-inline-actions"><button type="button" className="git-inline-action" disabled={mutating !== ""} onClick={() => mutatePath(action, change.path)}>{mutating === `${action}:${change.path}` ? "处理中" : action === "stage" ? "暂存" : "取消暂存"}</button>{stage === "worktree" && <button type="button" className="git-inline-action danger" disabled={mutating !== "" || change.conflicted} onClick={() => requestDiscard(change.path, change.untracked)}>{change.untracked ? "删除" : "撤销"}</button>}</div></div>)}</div>;
}

function CommitPanel({ stagedCount, value, setValue, open, disabled }: { stagedCount: number; value: string; setValue: (value: string) => void; open: () => void; disabled: boolean }) {
  return <section className="git-commit-panel"><label htmlFor="git-commit-message">提交信息</label><textarea id="git-commit-message" value={value} maxLength={4000} disabled={disabled || stagedCount === 0} onChange={(event) => setValue(event.target.value)} placeholder={stagedCount === 0 ? "暂存文件后即可提交" : "简要说明本次变更"} /><footer><span>{stagedCount === 0 ? "没有已暂存文件" : `将提交 ${stagedCount} 个文件`}</span><button type="button" className="primary" disabled={disabled || stagedCount === 0 || !value.trim() || value.split("\n")[0].length > 72} onClick={open}>提交</button></footer></section>;
}

function GitConfirmation({ confirmation, snapshot, stagedCount, trackedChangeCount, untrackedChangeCount, commitMessage, busy, close, commit, discardWorktree, discardAll }: { confirmation: Confirmation; snapshot: GitSnapshot | null; stagedCount: number; trackedChangeCount: number; untrackedChangeCount: number; commitMessage: string; busy: boolean; close: () => void; commit: () => void; discardWorktree: (path: string, untracked: boolean) => void; discardAll: (includeUntracked: boolean) => void }) {
  const [includeUntracked, setIncludeUntracked] = useState(confirmation.type === "discard-all" && untrackedChangeCount > 0 && trackedChangeCount === 0);
  const isCommit = confirmation.type === "commit";
  const title = isCommit ? "确认提交" : confirmation.type === "discard-all" ? "丢弃全部未提交改动" : confirmation.untracked ? "删除未跟踪文件" : "撤销工作区更改";
  const confirm = () => {
    if (confirmation.type === "commit") commit();
    else if (confirmation.type === "discard-all") discardAll(includeUntracked);
    else discardWorktree(confirmation.path, confirmation.untracked);
  };
  return <div className="backdrop git-confirmation-backdrop" role="dialog" aria-modal="true" aria-labelledby="git-confirmation-title"><section className="modal git-confirmation-dialog"><header><div><label>GIT OPERATION</label><h2 id="git-confirmation-title">{title}</h2></div><button type="button" title="关闭" disabled={busy} onClick={close}>×</button></header><div className="git-confirmation-body">{confirmation.type === "commit" ? <><p>将在 <b>{snapshot?.head.branch || "当前 HEAD"}</b> 创建本地提交，包含 {stagedCount} 个已暂存文件。</p><pre>{commitMessage}</pre></> : confirmation.type === "discard-all" ? <><p>已暂存和工作区中的受跟踪改动都会恢复到 <code>HEAD</code>，此操作无法在页面中撤销。</p><label className="git-confirmation-check"><input type="checkbox" checked={includeUntracked} onChange={(event) => setIncludeUntracked(event.target.checked)} />同时删除未跟踪文件</label></> : <p>{confirmation.untracked ? <>将永久删除 <code>{confirmation.path}</code>。</> : <>将 <code>{confirmation.path}</code> 恢复到暂存区版本，已暂存内容不会改变。</>}</p>}</div><footer><button type="button" className="secondary" disabled={busy} onClick={close}>取消</button><button type="button" className={isCommit ? "primary" : "primary danger"} disabled={busy} onClick={confirm}>{busy ? "处理中" : isCommit ? "确认提交" : "确认丢弃"}</button></footer></section></div>;
}

function History({ commits }: { commits: GitCommit[] }) { return commits.length === 0 ? <div className="git-empty">没有可显示的提交记录</div> : <div className="git-history">{commits.map((commit) => <article key={commit.oid}><code>{shortOID(commit.oid)}</code><div><b>{commit.subject || "(无提交说明)"}</b><span>{commit.author} · {formatGitTime(commit.authoredAt)}</span></div></article>)}</div>; }

function Branches({ branches }: { branches: GitBranch[] }) { return branches.length === 0 ? <div className="git-empty">没有可显示的分支</div> : <div className="git-branches">{branches.map((branch) => <article key={`${branch.remote ? "remote" : "local"}-${branch.name}`}><span className={branch.current ? "current" : ""}>{branch.current ? "当前" : branch.remote ? "远端" : "本地"}</span><b>{branch.name}</b><small>{branch.upstream || ""}</small></article>)}</div>; }

function Operations({ operations }: { operations: GitOperation[] }) { return operations.length === 0 ? <div className="git-empty">暂无操作记录</div> : <div className="git-operations">{operations.map((operation) => <article key={operation.id}><span className={`git-operation-status ${operation.status}`}>{operation.status}</span><div><b>{operation.type}</b><p>{operation.requestSummary}</p>{operation.errorMessage ? <small>{operation.errorMessage}</small> : null}</div><time>{formatGitTime(operation.finishedAt || operation.startedAt || operation.requestedAt)}</time></article>)}</div>; }
