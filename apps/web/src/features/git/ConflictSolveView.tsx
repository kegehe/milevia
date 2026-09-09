import { useCallback, useEffect, useMemo, useState } from "react";

import type { GitConflictContent } from "./git-model";
import { countConflictMarkers, parseConflictBlocks, resolveConflictBlock, type ConflictChoice } from "./conflict-blocks";
import { CodeFileView } from "../files/CodeFileView";

type Request = <T>(path: string, init?: RequestInit) => Promise<T>;
type ResolveAction = "ours" | "theirs" | "delete" | "working";
type SuggestionStatus = "running" | "completed" | "failed" | "cancelled";
type ConflictSuggestion = {
  id: string;
  projectId: string;
  path: string;
  agent: string;
  status: SuggestionStatus;
  merged?: string;
  explanation?: string;
  error?: string;
};

interface ConflictSolveViewProps {
  projectID: string;
  conversationId?: string;
  path: string;
  conflictPaths: string[];
  request: Request;
  fail: (message: string) => void;
  oursLabel: string;
  theirsLabel: string;
  busy: boolean;
  onResolve: (path: string, action: ResolveAction, content?: string) => void;
  onOpenFile: (path: string) => void;
  onClose: () => void;
}

// 把文件名截为 basename，交给语言检测（source-language 按扩展名/文件名判断）。
function baseName(path: string): string {
  const segments = path.split("/");
  return segments[segments.length - 1] || path;
}

function snippet(text: string, maxLines = 5): string {
  const lines = text.split("\n");
  const shown = lines.slice(0, maxLines);
  if (lines.length > maxLines) shown.push(`… 共 ${lines.length} 行`);
  return shown.join("\n");
}

export function ConflictSolveView({ projectID, conversationId, path, conflictPaths, request, fail, oursLabel, theirsLabel, busy, onResolve, onOpenFile, onClose }: ConflictSolveViewProps) {
  const [content, setContent] = useState<GitConflictContent | null>(null);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState("");
  const [workingText, setWorkingText] = useState<string | null>(null);

  const load = useCallback(() => {
    let alive = true;
    setLoading(true);
    setLoadError("");
    setContent(null);
    setWorkingText(null);
    const apiPath = `/api/projects/${projectID}/git/conflicts/content?path=${encodeURIComponent(path)}${conversationId ? `&conversationId=${encodeURIComponent(conversationId)}` : ""}`;
    request<GitConflictContent>(apiPath)
      .then((data) => {
        if (!alive) return;
        setContent(data);
        if (data.working !== undefined) setWorkingText(data.working);
        else setWorkingText(null);
      })
      .catch((cause) => {
        if (!alive) return;
        setLoadError(cause instanceof Error ? cause.message : "无法读取冲突文件内容");
      })
      .finally(() => {
        if (alive) setLoading(false);
      });
    return () => {
      alive = false;
    };
  }, [projectID, conversationId, path, request]);

  useEffect(() => load(), [load]);

  const apiPath = useMemo(() => {
    const suffix = conversationId ? `?conversationId=${encodeURIComponent(conversationId)}` : "";
    return (rel: string) => `/api/projects/${projectID}/git/${rel}${suffix}`;
  }, [projectID, conversationId]);
  const [suggestion, setSuggestion] = useState<ConflictSuggestion | null>(null);
  const [suggestAgent, setSuggestAgent] = useState<"claude-code" | "codex">("claude-code");
  const [suggestStarting, setSuggestStarting] = useState(false);

  const refreshSuggestion = useCallback(async (id: string) => {
    try {
      const data = await request<ConflictSuggestion>(apiPath(`conflicts/suggestions/${id}`));
      setSuggestion(data);
    } catch (cause) {
      setSuggestion((previous) => previous ? { ...previous, status: "failed", error: cause instanceof Error ? cause.message : "无法读取 AI 建议状态" } : previous);
    }
  }, [request, apiPath]);

  const startSuggestion = useCallback(async () => {
    if (suggestStarting) return;
    setSuggestStarting(true);
    try {
      const data = await request<ConflictSuggestion>(apiPath("conflicts/suggest"), { method: "POST", body: JSON.stringify({ path, agent: suggestAgent }) });
      setSuggestion(data);
    } catch (cause) {
      fail(cause instanceof Error ? cause.message : "无法启动 AI 建议");
    } finally {
      setSuggestStarting(false);
    }
  }, [apiPath, request, fail, path, suggestAgent, suggestStarting]);

  const cancelSuggestion = useCallback(async () => {
    if (!suggestion) return;
    try {
      await request<{ cancelled: boolean }>(apiPath(`conflicts/suggestions/${suggestion.id}/cancel`), { method: "POST" });
    } catch {
      // 忽略取消请求错误，轮询会自然收敛到终态。
    }
    void refreshSuggestion(suggestion.id);
  }, [apiPath, request, suggestion, refreshSuggestion]);

  // 建议生成是异步长任务：每 2s 轮询直到终态。
  useEffect(() => {
    if (!suggestion || suggestion.status !== "running") return;
    const timer = window.setInterval(() => { void refreshSuggestion(suggestion.id); }, 2000);
    return () => window.clearInterval(timer);
  }, [suggestion?.id, suggestion?.status, refreshSuggestion]);

  const currentIndex = conflictPaths.indexOf(path);
  const prevPath = currentIndex > 0 ? conflictPaths[currentIndex - 1] : null;
  const nextPath = currentIndex >= 0 && currentIndex < conflictPaths.length - 1 ? conflictPaths[currentIndex + 1] : null;

  const blocks = useMemo(() => (workingText === null ? [] : parseConflictBlocks(workingText)), [workingText]);
  const remaining = useMemo(() => (workingText === null ? 0 : countConflictMarkers(workingText)), [workingText]);
  const editable = content !== null && !content.binary && !content.oversized && content.kind !== "modify-delete" && content.kind !== "delete-modify" && content.kind !== "both-deleted";
  const canMarkResolved = workingText !== null && remaining === 0 && !busy;

  const applyBlock = (index: number, choice: ConflictChoice) => {
    if (workingText === null) return;
    setWorkingText(resolveConflictBlock(workingText, index, choice));
  };

  const fileName = baseName(path);
  const resolvedBadge = editable && canMarkResolved ? <span className="git-conflict-state ok">已全部解决</span> : remaining > 0 ? <span className="git-conflict-state">未解决 {remaining} 块</span> : null;

  if (loading) return <div className="git-empty">正在读取冲突内容…</div>;
  if (loadError || !content) return <div className="git-conflict-solve"><header className="git-diff-like-head"><div><span>冲突解决</span><b>{path}</b></div><button type="button" className="git-close" title="关闭" aria-label="关闭" onClick={onClose}>×</button></header><div className="git-empty">{loadError || "无法读取冲突内容"}</div></div>;

  const hasDeleteSide = content.oursDeleted || content.theirsDeleted;
  const showWholeOurs = !content.oursDeleted;
  const showWholeTheirs = !content.theirsDeleted;
  const showDeleteFile = content.kind === "both-deleted" || (content.kind === "modify-delete" && content.theirsDeleted) || (content.kind === "delete-modify" && content.oursDeleted);

  return <section className="git-conflict-solve">
    <header className="git-conflict-solve-head">
      <div>
        <span>冲突解决</span>
        <b title={path}>{path}</b>
      </div>
      <div className="git-conflict-solve-head-actions">
        {conflictPaths.length > 1 ? <div className="git-conflict-nav"><button type="button" title="上一个冲突文件" aria-label="上一个冲突文件" disabled={busy || suggestion?.status === "running" || !prevPath} onClick={() => prevPath && onOpenFile(prevPath)}>‹</button><span>{currentIndex + 1}/{conflictPaths.length}</span><button type="button" title="下一个冲突文件" aria-label="下一个冲突文件" disabled={busy || suggestion?.status === "running" || !nextPath} onClick={() => nextPath && onOpenFile(nextPath)}>›</button></div> : null}
        {resolvedBadge}
        <button type="button" className="git-close" title="关闭" aria-label="关闭" onClick={onClose}>×</button>
      </div>
    </header>

    <div className="git-conflict-solve-meta">
      <span>当前：<b>{oursLabel}</b></span>
      <span>传入：<b>{theirsLabel}</b></span>
      {hasDeleteSide ? <span className="git-conflict-kind danger">涉及删除</span> : null}
    </div>

    <div className="git-conflict-solve-toolbar">
      {showWholeOurs ? <button type="button" className="secondary" disabled={busy} onClick={() => onResolve(path, "ours")} title="整文件采用当前侧">整体采用当前</button> : null}
      {showWholeTheirs ? <button type="button" className="secondary" disabled={busy} onClick={() => onResolve(path, "theirs")} title="整文件采用传入侧">整体采用传入</button> : null}
      {showDeleteFile ? <button type="button" className="secondary danger" disabled={busy} onClick={() => onResolve(path, "delete")} title="将该文件以删除收场">删除文件</button> : null}
      {editable ? <label className="git-conflict-ai-launch"><select value={suggestAgent} disabled={busy || suggestStarting || suggestion?.status === "running"} onChange={(event) => setSuggestAgent(event.target.value as "claude-code" | "codex")} aria-label="AI 模型"><option value="claude-code">Claude</option><option value="codex">Codex</option></select><button type="button" className="secondary" disabled={busy || suggestStarting || suggestion?.status === "running"} onClick={() => void startSuggestion()}>{suggestStarting ? "启动中…" : suggestion?.status === "running" ? "AI 生成中…" : "AI 生成建议"}</button></label> : null}
      {canMarkResolved ? <button type="button" className="primary" onClick={() => workingText !== null && onResolve(path, "working", workingText)}>标记为已解决</button> : null}
    </div>

    {!editable ? <NonTextConflict content={content} oursLabel={oursLabel} theirsLabel={theirsLabel} busy={busy} onResolve={onResolve} path={path} />
      : <>
        {suggestion ? <div className="git-conflict-ai" role="region" aria-label="AI 建议">
          <header><span>AI 建议{suggestion.agent === "codex" ? "（Codex）" : "（Claude）"}</span>{suggestion.status === "running" ? <span className="git-conflict-ai-status running">生成中…</span> : suggestion.status === "completed" ? <span className="git-conflict-ai-status ok">完成</span> : <span className="git-conflict-ai-status error">{suggestion.status === "cancelled" ? "已取消" : "失败"}</span>}</header>
          {suggestion.status === "running" ? <>
            <p className="git-conflict-ai-hint">AI 正在阅读三方内容并生成合并建议，通常需要十几秒到一分钟；可先处理其它冲突文件。</p>
            <footer><button type="button" className="secondary" disabled={busy} onClick={() => void cancelSuggestion()}>取消生成</button></footer>
          </>
            : suggestion.status === "completed" ? <>
              {suggestion.explanation ? <p className="git-conflict-ai-explanation">{suggestion.explanation}</p> : null}
              <pre className="git-conflict-ai-result">{suggestion.merged}</pre>
              <footer><button type="button" className="secondary" disabled={busy} onClick={() => setSuggestion(null)}>拒绝</button><button type="button" className="primary" disabled={busy} onClick={() => suggestion.merged !== undefined && onResolve(path, "working", suggestion.merged)}>接受并标记为已解决</button></footer>
            </> : <>
              <p className="git-conflict-ai-hint error">{suggestion.error || (suggestion.status === "cancelled" ? "AI 建议已取消" : "生成失败，请重试或手动解决")}</p>
              <footer><button type="button" className="secondary" disabled={busy} onClick={() => setSuggestion(null)}>关闭</button>{suggestion.status === "failed" ? <button type="button" className="secondary" disabled={busy || suggestStarting} onClick={() => void startSuggestion()}>重试</button> : null}</footer>
            </>}
        </div> : null}
        {blocks.length > 0 ? <div className="git-conflict-blocks" role="region" aria-label={`${blocks.length} 个待解决冲突块`}>
          {blocks.map((block, index) => (
            <article className="git-conflict-block" key={`${index}-${block.ours}-${block.theirs}`}>
              <header><span className="git-conflict-block-no">冲突块 {index + 1}/{blocks.length}</span><div className="git-conflict-block-actions">
                <button type="button" disabled={busy} onClick={() => applyBlock(index, "ours")} title="保留当前侧内容，移除本块标记">采用当前</button>
                <button type="button" disabled={busy} onClick={() => applyBlock(index, "theirs")} title="采用传入侧内容，移除本块标记">采用传入</button>
                <button type="button" className="secondary" disabled={busy} onClick={() => applyBlock(index, "both")} title="两侧内容都保留">都保留</button>
              </div></header>
              <div className="git-conflict-block-panes">
                <div className="git-conflict-block-pane ours"><span>当前（{oursLabel}）</span><pre>{snippet(block.ours)}</pre></div>
                {block.base !== null ? <div className="git-conflict-block-pane base"><span>共同祖先</span><pre>{snippet(block.base)}</pre></div> : null}
                <div className="git-conflict-block-pane theirs"><span>传入（{theirsLabel}）</span><pre>{snippet(block.theirs)}</pre></div>
              </div>
            </article>
          ))}
        </div> : null}
        <div className="git-conflict-editor">
          <header><span>结果文件（可直接编辑，无冲突标记后即可“标记为已解决”）</span></header>
          {workingText === null ? <div className="git-empty">该文件暂无可编辑内容</div>
            : <CodeFileView content={workingText} filename={fileName} fontSize={13} editable onChange={setWorkingText} />}
        </div>
      </>}
  </section>;
}

function NonTextConflict({ content, oursLabel, theirsLabel, busy, onResolve, path }: { content: GitConflictContent; oursLabel: string; theirsLabel: string; busy: boolean; onResolve: (path: string, action: ResolveAction) => void; path: string }) {
  const messages: string[] = [];
  if (content.binary) messages.push("这是一个二进制文件，无法在此逐块编辑，请用整文件操作或外部工具解决。");
  else if (content.oversized) messages.push("文件过大，无法在此展示全文，请用整文件操作或外部工具解决。");
  if (content.theirsDeleted && !content.oursDeleted) messages.push(`该文件在“${theirsLabel}”中被删除，当前“${oursLabel}”保留着它。`);
  if (content.oursDeleted && !content.theirsDeleted) messages.push(`该文件在“${oursLabel}”中被删除，传入“${theirsLabel}”保留着它。`);
  if (content.oursDeleted && content.theirsDeleted) messages.push("该文件在两侧都被删除。");
  if (messages.length === 0) messages.push("该文件不能按冲突块逐段编辑，请使用上方整文件操作。");

  return <div className="git-conflict-nontext" role="note">
    <p>{messages.join(" ")}</p>
    <div>
      {!content.oursDeleted ? <button type="button" className="secondary" disabled={busy} onClick={() => onResolve(path, "ours")}>保留当前（{oursLabel}）</button> : null}
      {!content.theirsDeleted ? <button type="button" className="secondary" disabled={busy} onClick={() => onResolve(path, "theirs")}>保留传入（{theirsLabel}）</button> : null}
      {content.oursDeleted && content.theirsDeleted ? <button type="button" className="secondary danger" disabled={busy} onClick={() => onResolve(path, "delete")}>删除文件</button>
        : content.oursDeleted ? <button type="button" className="secondary danger" disabled={busy} onClick={() => onResolve(path, "delete")}>按当前删除文件</button>
          : content.theirsDeleted ? <button type="button" className="secondary danger" disabled={busy} onClick={() => onResolve(path, "delete")}>按传入删除文件</button> : null}
    </div>
  </div>;
}
