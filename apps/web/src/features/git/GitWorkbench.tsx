import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import { changeState, formatGitTime, groupChanges, shortOID, type GitBranch, type GitChange, type GitCommit, type GitDiff, type GitOperation, type GitSnapshot } from "./git-model";

type Request = <T>(path: string, init?: RequestInit) => Promise<T>;
type Tab = "overview" | "changes" | "history" | "branches" | "operations";

const tabs: { id: Tab; label: string }[] = [
  { id: "overview", label: "概览" },
  { id: "changes", label: "变更" },
  { id: "history", label: "历史" },
  { id: "branches", label: "分支" },
  { id: "operations", label: "操作记录" },
];

export function GitWorkbench({ projectID, request, fail, close }: { projectID: string; request: Request; fail: (message: string) => void; close: () => void }) {
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

  useEffect(() => { void reload().catch(() => undefined); }, [reload]);

  const grouped = useMemo(() => groupChanges(changes), [changes]);
  const changeCount = changes.length;
  const openDiff = async (change: GitChange, stage: "worktree" | "index") => {
    const requestID = ++diffRequest.current;
    try {
      const diff = await request<GitDiff>(`/api/projects/${projectID}/git/diff?path=${encodeURIComponent(change.path)}&stage=${stage}`);
      if (requestID === diffRequest.current && mountedRef.current) setSelectedDiff(diff);
    } catch (cause) { if (mountedRef.current) fail(cause instanceof Error ? cause.message : "无法读取差异"); }
  };
  const mutatePath = async (action: "stage" | "unstage", path: string) => {
    if (!snapshot?.stateToken) return;
    if (!mountedRef.current) return;
    setMutating(`${action}:${path}`);
    try {
      await request(`/api/projects/${projectID}/git/${action}`, { method: "POST", body: JSON.stringify({ paths: [path], stateToken: snapshot.stateToken }) });
      if (!mountedRef.current) return;
      setSelectedDiff(null);
      await reload(true);
    } catch (cause) { if (mountedRef.current) fail(cause instanceof Error ? cause.message : "无法执行 Git 操作"); }
    finally { if (mountedRef.current) setMutating(""); }
  };

  return <div className="git-workbench-backdrop" role="presentation" onMouseDown={(event) => { if (event.currentTarget === event.target) close(); }}>
    <section className="git-workbench" role="dialog" aria-modal="true" aria-label="Git 仓库工作台">
      <header className="git-workbench-head">
        <div><label>PROJECT REPOSITORY</label><h2>Git 工作台</h2><p>{snapshot?.head.detached ? "HEAD 分离状态" : snapshot?.head.branch || "读取仓库中"}</p></div>
        <div className="git-workbench-actions"><button type="button" className="secondary" disabled={refreshing} onClick={() => void reload(true)}>{refreshing ? "刷新中" : "刷新"}</button><button type="button" className="git-close" title="关闭 Git 工作台" aria-label="关闭 Git 工作台" onClick={close}>×</button></div>
      </header>
      <nav className="git-tabs" aria-label="Git 工作台视图">{tabs.map((item) => <button type="button" key={item.id} className={tab === item.id ? "active" : ""} aria-pressed={tab === item.id} onClick={() => { setTab(item.id); closeDiff(); }}>{item.label}{item.id === "changes" && changeCount > 0 ? <b>{changeCount}</b> : null}</button>)}</nav>
      <main className="git-workbench-body">
        {loading ? <div className="git-empty">正在读取仓库状态</div> : <>
          {tab === "overview" && <Overview snapshot={snapshot} changeCount={changeCount} />}
          {tab === "changes" && <Changes grouped={grouped} selectedDiff={selectedDiff} openDiff={openDiff} closeDiff={closeDiff} mutate={mutatePath} mutating={mutating} />}
          {tab === "history" && <History commits={commits} />}
          {tab === "branches" && <Branches branches={branches} />}
          {tab === "operations" && <Operations operations={operations} />}
        </>}
      </main>
    </section>
  </div>;
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

function Changes({ grouped, selectedDiff, openDiff, closeDiff, mutate, mutating }: { grouped: { staged: GitChange[]; worktree: GitChange[] }; selectedDiff: GitDiff | null; openDiff: (change: GitChange, stage: "worktree" | "index") => Promise<void>; closeDiff: () => void; mutate: (action: "stage" | "unstage", path: string) => Promise<void>; mutating: string }) {
  return <div className="git-changes-view"><section className="git-change-group"><header><h3>已暂存</h3><span>{grouped.staged.length}</span></header><ChangeList changes={grouped.staged} stage="index" openDiff={openDiff} mutate={mutate} mutating={mutating} empty="没有已暂存变更" /></section><section className="git-change-group"><header><h3>工作区</h3><span>{grouped.worktree.length}</span></header><ChangeList changes={grouped.worktree} stage="worktree" openDiff={openDiff} mutate={mutate} mutating={mutating} empty="工作区没有变更" /></section>{selectedDiff && <section className="git-diff"><header><div><span>{selectedDiff.stage === "index" ? "已暂存差异" : "工作区差异"}</span><b>{selectedDiff.path}</b></div><button type="button" className="git-close" title="关闭差异" aria-label="关闭差异" onClick={closeDiff}>×</button></header><pre>{selectedDiff.content || "该文件没有可显示的文本差异。"}</pre></section>}</div>;
}

function ChangeList({ changes, stage, openDiff, mutate, mutating, empty }: { changes: GitChange[]; stage: "worktree" | "index"; openDiff: (change: GitChange, stage: "worktree" | "index") => Promise<void>; mutate: (action: "stage" | "unstage", path: string) => Promise<void>; mutating: string; empty: string }) {
  if (changes.length === 0) return <p className="git-list-empty">{empty}</p>;
  const action = stage === "index" ? "unstage" : "stage";
  return <div className="git-change-list">{changes.map((change) => <div key={`${stage}-${change.path}`}><button type="button" onClick={() => void openDiff(change, stage)}><span className={`git-change-kind ${change.conflicted ? "conflicted" : ""}`}>{changeState(change)}</span><b>{change.path}</b>{change.originalPath ? <small>{change.originalPath}</small> : null}</button><button type="button" className="git-inline-action" disabled={mutating !== ""} onClick={() => void mutate(action, change.path)}>{mutating === `${action}:${change.path}` ? "处理中" : action === "stage" ? "暂存" : "取消暂存"}</button></div>)}</div>;
}

function History({ commits }: { commits: GitCommit[] }) { return commits.length === 0 ? <div className="git-empty">没有可显示的提交记录</div> : <div className="git-history">{commits.map((commit) => <article key={commit.oid}><code>{shortOID(commit.oid)}</code><div><b>{commit.subject || "(无提交说明)"}</b><span>{commit.author} · {formatGitTime(commit.authoredAt)}</span></div></article>)}</div>; }

function Branches({ branches }: { branches: GitBranch[] }) { return branches.length === 0 ? <div className="git-empty">没有可显示的分支</div> : <div className="git-branches">{branches.map((branch) => <article key={`${branch.remote ? "remote" : "local"}-${branch.name}`}><span className={branch.current ? "current" : ""}>{branch.current ? "当前" : branch.remote ? "远端" : "本地"}</span><b>{branch.name}</b><small>{branch.upstream || ""}</small></article>)}</div>; }

function Operations({ operations }: { operations: GitOperation[] }) { return operations.length === 0 ? <div className="git-empty">暂无操作记录</div> : <div className="git-operations">{operations.map((operation) => <article key={operation.id}><span className={`git-operation-status ${operation.status}`}>{operation.status}</span><div><b>{operation.type}</b><p>{operation.requestSummary}</p>{operation.errorMessage ? <small>{operation.errorMessage}</small> : null}</div><time>{formatGitTime(operation.finishedAt || operation.startedAt || operation.requestedAt)}</time></article>)}</div>; }
