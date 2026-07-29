// 对话页 — 从 App.tsx Chat 组件提取
// 完整保留了原 Chat 组件的全部功能：消息列表、WebSocket 实时事件、输入框、权限管理、会话切换、所有弹窗

import { FormEvent, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { useParams, useNavigate, useSearchParams, useOutletContext } from "react-router-dom";
import type { ProjectLayoutOutletContext } from "../components/ProjectLayout";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import "../markdown.css";
import "../permission.css";
import "../conversation.css";
import "../stop.css";
import "../tasks.css";
import "../git.css";
import "../run.css";
import { TaskQueue } from "../features/tasks/TaskQueue";
import { ConfirmDialog } from "../components/ConfirmDialog";
import { useProjectContext } from "../stores/useProjectStore";
import type {
  Conversation, Message, Event, Shortcut, ShortcutEditorState,
  PermissionMode, AgentID, AgentStatus, TimelineItem, ToolAction, AgentNode, AgentLog,
  AgentExecution, RunUsage, ConversationUsageResponse,
  RunnerInfo, CheckUpdateResult, UpdateResult,
} from "../lib/types";
import { api } from "../lib/api";
import { asRecord } from "../lib/api";
import {
  formatTime, formatHistoryTime, formatTokens, formatDuration,
  formatCost, runDuration, contextLabel, contextLevel, requiredShortcutVariables,
  isNarrowConversationLayout, websocketAssistantMessageID, isTemporaryMessage,
  mergeConversationItems, mergeReloadMessages, agentStatusLabel,
} from "../lib/utils";
import {
  buildTimeline, buildAgentExecutions, subagentTextIndex,
  isSubagentMessage, flattenAgents, timelineContentVersion,
} from "../lib/timeline";

// ---- 子组件 ---------------------------------------------------------------

function ComposerRunnerInfo({ runnerID, agentID, run, runLabel, permissionMode, usage, displayedModel, contextLabel, contextLevel, onShowUsage, stopping, onStop }: { runnerID: string; agentID: AgentID; run: string; runLabel: string; permissionMode?: string; usage: ConversationUsageResponse | null; displayedModel: string; contextLabel: string; contextLevel: string; onShowUsage: () => void; stopping: boolean; onStop: () => void }) {
  const [runner, setRunner] = useState<RunnerInfo | null>(null);
  const [checking, setChecking] = useState(false);
  const [updateInfo, setUpdateInfo] = useState<CheckUpdateResult | null>(null);
  const [updating, setUpdating] = useState(false);
  const [updateError, setUpdateError] = useState<string | null>(null);
  const [showConfirm, setShowConfirm] = useState(false);

  const refreshRunner = useCallback(async () => {
    try {
      const list = await api<RunnerInfo[]>("/api/runners");
      const match = list.find((r) => r.id === runnerID);
      if (match) setRunner(match);
    } catch {
      /* ignore — runner info is best-effort */
    }
  }, [runnerID]);

  useEffect(() => {
    void refreshRunner();
  }, [refreshRunner]);

  // 当后端报告 runner 正在更新（如页面刷新后检测到），定期轮询直到状态恢复
  useEffect(() => {
    const claudeStatus = runner?.claude?.status;
    if (!runner || updating || claudeStatus !== "updating") return;
    const id = window.setInterval(() => { void refreshRunner(); }, 5_000);
    return () => window.clearInterval(id);
  }, [runner, updating, refreshRunner]);

  const handleCheckUpdate = async () => {
    setChecking(true);
    setUpdateInfo(null);
    try {
      const result = await api<CheckUpdateResult>(`/api/runners/${runnerID}/claude/check-update`, { method: "POST" });
      setUpdateInfo(result);
    } catch (err) {
      setUpdateInfo({ updateAvailable: false, currentVersion: runner?.claude?.version || "", error: err instanceof Error ? err.message : "检查更新失败" });
    } finally {
      setChecking(false);
    }
  };

  const handleUpdate = async () => {
    setShowConfirm(false);
    setUpdating(true);
    // 立即将状态置为 updating，触发底部"更新中..."脉冲动画
    setRunner((prev) => prev ? { ...prev, claude: { ...prev.claude, status: "updating" as const } } : prev);
    try {
      const result = await api<UpdateResult>(`/api/runners/${runnerID}/claude/update`, { method: "POST" });
      if (result.success) {
        setUpdateInfo(null);
      } else {
        setUpdateError(result.error || "更新失败，请稍后重试。");
      }
    } catch (err) {
      setUpdateError(err instanceof Error ? err.message : "更新失败，请稍后重试。");
    } finally {
      setUpdating(false);
      // 从后端拉取最新 runner 信息，刷新版本号与状态
      await refreshRunner();
      // 若 refreshRunner 未能更新状态（如网络异常），确保 updating 状态不会残留
      setRunner((prev) => prev?.claude?.status === "updating" ? { ...prev, claude: { ...prev.claude, status: "ready" as const } } : prev);
    }
  };

  const tool = agentID === "codex" ? runner?.codex : runner?.claude;
  const toolName = agentID === "codex" ? "Codex" : "Claude Code";
  const runnerStatusClass = run ? "running" : tool?.status || "unavailable";

  const canShowUsage = Boolean(run) || tool?.status === "ready";
  return (<>
    <span className="composer-status-group">
      <span className={`runner-inline ${runnerStatusClass}`} title={tool?.reason}><i></i>{run ? runLabel : tool?.status === "ready" ? `${toolName} ${tool.version}` : tool?.status === "updating" ? "更新中..." : tool?.reason || `${toolName} 不可用`}</span>
      {agentID === "claude-code" && !run && tool?.status === "ready" && <button className="runner-inline-btn" disabled={checking || updating} onClick={() => void handleCheckUpdate()}>{checking ? "检查中..." : "检查更新"}</button>}
      {agentID === "claude-code" && !run && !updating && updateInfo?.updateAvailable && !updateInfo.error && <button className="runner-inline-btn update-available" onClick={() => setShowConfirm(true)}>更新至 {updateInfo.latestVersion}</button>}
      {agentID === "claude-code" && !run && !updating && updateInfo?.error && <span className="runner-inline-error" title={updateInfo.error}>{updateInfo.error}</span>}
      {canShowUsage ? <span className="composer-usage"><span title={displayedModel}>{displayedModel}</span><span className={contextLevel}>{contextLabel}</span><span>{usage ? `${usage.session.taskCount} 次对话` : "加载中"}</span><button className="usage-trigger" type="button" onClick={onShowUsage}>使用状态</button></span> : <span className="composer-usage pending">用量将在工具就绪后显示</span>}
    </span>
    {showConfirm && <div className="backdrop" role="dialog" aria-modal="true"><section className="modal"><header><div><label>UPDATE CLAUDE CODE</label><h2>确认更新 Claude Code</h2></div><button title="关闭" onClick={() => setShowConfirm(false)}>x</button></header><p className="permission-confirmation">当前版本：<b>{tool?.version}</b> → 最新版本：<b>{updateInfo?.latestVersion}</b>。更新期间将无法使用 AI 对话功能，更新预计需要数十秒。</p><footer><button className="secondary" onClick={() => setShowConfirm(false)}>取消</button><button className="primary" onClick={() => void handleUpdate()}>确认更新</button></footer></section></div>}
    {updateError && <div className="backdrop" role="dialog" aria-modal="true"><section className="modal"><header><div><label>UPDATE ERROR</label><h2>更新失败</h2></div><button title="关闭" onClick={() => setUpdateError(null)}>x</button></header><p className="permission-confirmation">{updateError}</p><footer><button className="secondary" onClick={() => setUpdateError(null)}>关闭</button></footer></section></div>}
  </>);
}

function UsageDialog({ agentID, usage, currentRun, now, close }: { agentID: AgentID; usage: ConversationUsageResponse | null; currentRun: RunUsage | undefined; now: number; close: () => void }) {
  if (usage && !usage.available) return <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="usage-title"><section className="modal usage-dialog"><header><div><label>{agentID === "codex" ? "CODEX" : "AI"} USAGE</label><h2 id="usage-title">使用状态</h2></div><button title="关闭" onClick={close}>x</button></header><div className="usage-body"><p className="usage-note">{usage.reason || "当前工具未提供可验证的使用统计。"}</p></div><footer><button className="secondary" onClick={close}>关闭</button></footer></section></div>;
  const task = usage?.currentRun ?? usage?.latestRun;
  const active = Boolean(usage?.currentRun && usage.currentRun.status === "running");
  const hasTaskUsage = Boolean(task?.hasResult);
  const metrics = task ? [
    ["状态", active ? "运行中" : task.status === "completed" ? "已完成" : task.status === "stopped" ? "已停止" : task.status === "failed" ? "失败" : task.status],
    ["耗时", formatDuration(runDuration(task, now))],
    ["首个响应", task.ttftMs > 0 ? formatDuration(task.ttftMs) : "等待响应"],
    ["Agent 轮次", task.agentTurns || "--"],
    ["模型步骤", task.modelSteps || "--"],
    ["工具调用", task.toolCalls || "--"],
    ["子代理", task.subagentCount || "0"],
    ["输入 / 输出", hasTaskUsage ? `${formatTokens(task.inputTokens)} / ${formatTokens(task.outputTokens)}` : "等待最终统计"],
    ["缓存读取 / 创建", hasTaskUsage ? `${formatTokens(task.cacheReadTokens)} / ${formatTokens(task.cacheCreationTokens)}` : "等待最终统计"],
    ["费用估算", hasTaskUsage ? formatCost(task.estimatedCostUsd) : "等待最终统计"],
  ] : [];
  const session = usage?.session;
  const agentName = agentID === "codex" ? "Codex" : "Claude Code";
  return <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="usage-title"><section className="modal usage-dialog"><header><div><label>{agentName.toUpperCase()} USAGE</label><h2 id="usage-title">使用状态</h2></div><button title="关闭" onClick={close}>x</button></header><div className="usage-body">
    <section className="usage-section"><div className="usage-section-head"><h3>当前任务</h3>{task?.model && <span>{task.model}</span>}</div>{task ? <><dl className="usage-grid">{metrics.map(([label, value]) => <div key={String(label)}><dt>{label}</dt><dd>{value}</dd></div>)}</dl>{!hasTaskUsage && !active && <p className="usage-note">{task.reason || `该任务未获得 ${agentName} 的最终统计数据。`}</p>}</> : <p className="usage-note">当前会话还没有可用的任务统计。</p>}</section>
    <section className="usage-section"><div className="usage-section-head"><h3>当前会话</h3><span className={`context-state ${contextLevel(usage?.context)}`}>{contextLabel(usage?.context)}</span></div>{(usage?.context.contextWindow ?? 0) > 0 && <p className="context-detail">{usage?.context.contextInputTokens ? `${formatTokens(usage.context.contextInputTokens)} / ${formatTokens(usage.context.contextWindow!)}` : usage?.context.available === false ? usage.context.reason || "当前工具未提供上下文快照" : usage?.context.hasResult ? "当前工具未提供上下文快照" : "等待上下文快照"}</p>}{session && <dl className="usage-grid session-grid"><div><dt>任务次数</dt><dd>{session.taskCount}</dd></div><div><dt>Agent 轮次</dt><dd>{session.agentTurns}</dd></div><div><dt>模型步骤</dt><dd>{session.modelSteps}</dd></div><div><dt>工具调用</dt><dd>{session.toolCalls}</dd></div><div><dt>输入 / 输出</dt><dd>{formatTokens(session.inputTokens)} / {formatTokens(session.outputTokens)}</dd></div><div><dt>缓存读取 / 创建</dt><dd>{formatTokens(session.cacheReadTokens)} / {formatTokens(session.cacheCreationTokens)}</dd></div><div><dt>费用估算</dt><dd>{formatCost(session.estimatedCostUsd)}</dd></div></dl>}</section>
    {usage?.models.length ? <section className="usage-section"><div className="usage-section-head"><h3>模型用量</h3><span>包含子代理</span></div><div className="model-usage-list">{usage.models.map((model) => <div key={model.model}><b>{model.model}</b><span>{formatTokens(model.inputTokens)} 输入 · {formatTokens(model.outputTokens)} 输出</span><em>{formatCost(model.estimatedCostUsd)}</em></div>)}</div></section> : null}
    <p className="usage-disclaimer">费用为客户端事件估算值，不代表账单金额。</p>
  </div><footer><button className="secondary" onClick={close}>关闭</button></footer></section></div>;
}

function ConversationHistoryDialog({ conversations, activeID, busyID, close, activate }: { conversations: Conversation[]; activeID: string; busyID: string; close: () => void; activate: (item: Conversation) => Promise<void> }) {
  const [query, setQuery] = useState("");
  const [selected, setSelected] = useState(0);
  const filtered = useMemo(() => {
    const keyword = query.trim().toLocaleLowerCase();
    return keyword ? conversations.filter((item) => `${item.title} ${item.preview || ""}`.toLocaleLowerCase().includes(keyword)) : conversations;
  }, [conversations, query]);
  useEffect(() => { setSelected(0); }, [query]);
  const select = (item: Conversation) => { if (item.id !== activeID && item.status !== "running" && !busyID) void activate(item); };
  const keyDown = (event: React.KeyboardEvent<HTMLInputElement>) => {
    if (event.key === "Escape") { event.preventDefault(); close(); return; }
    if (event.key === "ArrowDown") { event.preventDefault(); setSelected((index) => Math.min(filtered.length - 1, index + 1)); return; }
    if (event.key === "ArrowUp") { event.preventDefault(); setSelected((index) => Math.max(0, index - 1)); return; }
    if (event.key === "Enter" && filtered[selected]) { event.preventDefault(); select(filtered[selected]); }
  };
  return <div className="backdrop history-backdrop" role="dialog" aria-modal="true" aria-label="会话历史"><section className="modal conversation-history"><header><div><h2>恢复会话</h2></div><button title="关闭" onClick={close}>x</button></header><input autoFocus value={query} onChange={(event) => setQuery(event.target.value)} onKeyDown={keyDown} placeholder="搜索历史会话" /><div className="history-list">{filtered.length === 0 ? <p className="history-empty">没有匹配的会话</p> : filtered.map((item, index) => {
    const runningElsewhere = item.status === "running" && item.id !== activeID;
    return <button key={item.id} className={`history-item ${busyID === item.id ? "activating" : ""} ${item.id === activeID ? "active" : ""} ${index === selected ? "selected" : ""}`} disabled={Boolean(busyID) || runningElsewhere} onMouseEnter={() => setSelected(index)} onClick={() => select(item)}><span><b>{item.title || "新会话"}</b><small>{item.agentId === "codex" ? "Codex" : "Claude Code"} · {item.preview || "尚未发送消息"}</small></span><em>{busyID === item.id ? "切换中…" : item.id === activeID ? "当前" : runningElsewhere ? "运行中" : formatHistoryTime(item.lastActivityAt)}</em></button>;
  })}</div><footer><small>输入 /resume 可再次打开</small><button className="secondary" onClick={close}>关闭</button></footer></section></div>;
}

function NewConversationDialog({ runnerID, close, create }: { runnerID: string; close: () => void; create: (agentId: AgentID, permissionMode: PermissionMode) => Promise<void> }) {
  const [agentId, setAgentId] = useState<AgentID>("claude-code");
  const [permissionMode, setPermissionMode] = useState<PermissionMode>("full_control");
  const [creating, setCreating] = useState(false);
  const [runner, setRunner] = useState<RunnerInfo | null>(null);
  useEffect(() => { api<RunnerInfo[]>("/api/runners").then((items) => setRunner(items.find((item) => item.id === runnerID) || null)).catch(() => setRunner(null)); }, [runnerID]);
  const selectAgent = (next: AgentID) => { setAgentId(next); setPermissionMode(next === "codex" ? "workspace_write" : "full_control"); };
  const codexStatus = runner?.codex;
  const codexReady = codexStatus?.status === "ready";
  const submit = async () => { if (agentId === "codex" && !codexReady) return; setCreating(true); try { await create(agentId, permissionMode); } finally { setCreating(false); } };
  const codex = agentId === "codex";
  return <div className="backdrop" role="dialog" aria-modal="true"><section className="modal permission-dialog"><header><div><h2>新会话</h2></div><button title="关闭" onClick={close}>x</button></header><div className="permission-options"><button className={!codex ? "active" : ""} onClick={() => selectAgent("claude-code")}><b>Claude Code</b></button><button className={codex ? "active" : ""} disabled={Boolean(runner) && !codexReady} title={codexStatus?.reason} onClick={() => selectAgent("codex")}><b>Codex</b></button></div><div className="permission-options">{codex ? <><button className={permissionMode === "read_only" ? "active" : ""} onClick={() => setPermissionMode("read_only")}><b>仅分析</b><span>只读检查，不修改项目。</span></button><button className={permissionMode === "workspace_write" ? "active" : ""} onClick={() => setPermissionMode("workspace_write")}><b>项目内执行</b><span>可在项目范围内读写和执行。</span></button><button className={permissionMode === "full_control" ? "active" : ""} onClick={() => setPermissionMode("full_control")}><b>完全控制</b><span>Codex 可直接执行命令，不受沙箱限制。</span></button></> : <><button className={permissionMode === "approval_required" ? "active" : ""} onClick={() => setPermissionMode("approval_required")}><b>默认权限</b><span>每条终端命令执行前等待确认。</span></button><button className={permissionMode === "full_control" ? "active" : ""} onClick={() => setPermissionMode("full_control")}><b>完全控制</b><span>Claude 可直接执行命令，不会等待确认。</span></button></>}</div><footer><button className="secondary" onClick={close}>取消</button><button className="primary" disabled={creating || (codex && !codexReady)} onClick={() => void submit()}>{creating ? "创建中" : "创建会话"}</button></footer></section></div>;
}

function FullControlConfirmationDialog({ close, confirm, changing, isCodex }: { close: () => void; confirm: () => Promise<void>; changing: boolean; isCodex?: boolean }) {
  const agentName = isCodex ? "Codex" : "Claude";
  return <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="full-control-title"><section className="modal permission-dialog"><header><div><label>{agentName.toUpperCase()} PERMISSION</label><h2 id="full-control-title">切换为完全控制</h2></div><button title="关闭" disabled={changing} onClick={close}>x</button></header><p className="permission-confirmation">{isCodex ? `完全控制会允许 Codex 绕过沙箱限制，直接执行所有命令，不再受项目目录约束。` : `完全控制会允许 Claude 在当前项目中直接执行所有命令，不再等待确认。`}</p><footer><button className="secondary" disabled={changing} onClick={close}>取消</button><button className="primary danger" disabled={changing} onClick={() => void confirm()}>{changing ? "切换中" : "确认切换"}</button></footer></section></div>;
}

function MessageCard({ message, agentID }: { message: Message; agentID: AgentID }) {
  const isUser = message.role === "user";
  const agentName = agentID === "codex" ? "Codex" : "Claude";
  return <article className={`message ${message.role}`}><header><span className="message-avatar">{isUser ? "你" : agentID === "codex" ? "O" : "C"}</span><b>{isUser ? "你" : agentName}</b><time>{formatTime(message.createdAt)}</time></header><div className="markdown"><Markdown content={message.content} /></div></article>;
}

function ErrorCard({ item }: { item: TimelineItem & { kind: "error" } }) {
  return <article className="error-card"><header><span className="error-card-icon">!</span><div><b>{item.title}</b><time>{formatTime(item.createdAt)}</time></div></header><p>{item.detail}</p></article>;
}

function MarkdownImage({ src, alt }: { src?: string; alt?: string }) {
  const label = alt?.trim() || "未命名图片";
  const externalImage = src && /^https:\/\//i.test(src) ? src : "";
  return <span className="markdown-image-reference" role="note">图片：{externalImage ? <a href={externalImage} target="_blank" rel="noreferrer">{label}</a> : label}</span>;
}

function Markdown({ content }: { content: string }) {
  return <ReactMarkdown remarkPlugins={[remarkGfm]} skipHtml components={{ a: ({ href, children }) => <a href={href} target="_blank" rel="noreferrer">{children}</a>, img: MarkdownImage }}>{content}</ReactMarkdown>;
}

function ToolCard({ action, resolving, decide }: { action: ToolAction; resolving: string; decide: (approvalId: string, decision: "allow" | "deny") => Promise<void> }) {
  const command = typeof action.input.command === "string" ? action.input.command : "";
  const description = typeof action.input.description === "string" ? action.input.description : action.name;
  const approval = action.approval;
  const waiting = approval?.status === "pending" && !action.output;
  const denied = approval?.status === "deny";
  const failed = !denied && (action.output?.isError || action.runStatus === "failed");
  const stopped = action.runStatus === "stopped";
  const status = denied ? "已拒绝" : failed ? "执行失败" : stopped ? "已停止" : action.output ? "已完成" : waiting ? "等待确认" : approval?.status === "allow" ? "已允许" : action.runStatus === "completed" ? "已结束" : "执行中";
  const output = action.output?.content || "(无输出)";
  const shouldCollapseOutput = output.length > 260 || output.split("\n").length > 5;
  const outputPreview = output.replace(/\s+/g, " ").trim().slice(0, 180);
  const statusClass = failed ? "failed" : stopped || denied ? "denied" : waiting ? "pending" : "";
  return <article className={`tool-card ${waiting ? "waiting" : ""}`}><header><div><span className="tool-icon">$</span><div><b>{action.name === "Bash" ? "终端命令" : action.name}</b><small>{description}</small></div></div><div className="tool-meta"><time>{formatTime(action.createdAt)}</time><span className={`tool-status ${statusClass}`}>{status}</span></div></header>{command && <pre className="command"><code>{command}</code></pre>}{waiting && approval && <div className="approval-actions"><span>此命令将会在当前项目目录执行。</span><div><button className="secondary" disabled={resolving === approval.approvalId} onClick={() => void decide(approval.approvalId, "deny")}>拒绝</button><button className="primary" disabled={resolving === approval.approvalId} onClick={() => void decide(approval.approvalId, "allow")}>{resolving === approval.approvalId ? "处理中" : "允许执行"}</button></div></div>}{action.output && (shouldCollapseOutput ? <details><summary>{failed ? "查看错误输出" : "查看命令输出"}<span>{outputPreview}</span></summary><pre className="output">{output}</pre></details> : <pre className={`output inline ${failed ? "error-output" : ""}`}>{output}</pre>)}</article>;
}

function AgentExecutionCard({ execution, open }: { execution: AgentExecution; open: () => void }) {
  const agents = flattenAgents(execution.agents);
  const counts = agents.reduce<Record<AgentStatus, number>>((value, agent) => { value[agent.status]++; return value; }, { pending: 0, running: 0, completed: 0, failed: 0, stopped: 0, unresolved: 0 });
  const state = execution.incomplete ? "结果未收齐" : execution.status === "completed" ? "已完成" : execution.status === "failed" ? "执行失败" : ["stopped", "interrupted", "cancelled"].includes(execution.status) ? "已停止" : "执行中";
  const lastActivity = agents.flatMap((agent) => agent.logs).reduce<AgentLog | undefined>((latest, log) => !latest || new Date(log.createdAt).getTime() > new Date(latest.createdAt).getTime() ? log : latest, undefined);
  return <section className={`agent-execution-card ${execution.incomplete ? "incomplete" : execution.status}`} aria-label="子代理执行过程"><header><div><span className="agent-execution-icon">A</span><div><b>子代理执行过程</b><small>{state}{counts.running ? ` · ${counts.running} 个执行中` : ""}</small></div></div><button className="secondary" onClick={open}>查看过程</button></header><div className="agent-execution-summary"><span>{agents.length} 个子代理</span><span>{counts.completed} 已完成</span>{counts.stopped > 0 && <span>{counts.stopped} 已停止</span>}{counts.failed > 0 && <span className="failed">{counts.failed} 失败</span>}{counts.unresolved > 0 && <span className="incomplete">{counts.unresolved} 未收齐</span>}</div>{execution.incomplete && <p className="agent-execution-warning">主回合已结束，但没有收到全部子代理的最终结果。已收到的过程记录仍可查看。</p>}{lastActivity && <p className="agent-execution-last"><b>{lastActivity.title}</b><span>{lastActivity.detail.replace(/\s+/g, " ")}</span></p>}</section>;
}

function AgentExecutionDialog({ execution, close }: { execution: AgentExecution; close: () => void }) {
  const agents = flattenAgents(execution.agents);
  const [selectedID, setSelectedID] = useState(agents[0]?.id || "");
  const selected = agents.find((agent) => agent.id === selectedID) || agents[0];
  return <div className="backdrop agent-execution-backdrop" role="dialog" aria-modal="true" aria-labelledby="agent-execution-title"><section className="agent-execution-dialog"><header><div><label>CLAUDE CODE EXECUTION</label><h2 id="agent-execution-title">子代理过程</h2><p>{execution.incomplete ? "主回合已完成，但部分子代理的最终结果未送达。" : "查看每个子代理已收到的输出、工具调用和结果。"}</p></div><button title="关闭" onClick={close}>x</button></header><div className="agent-execution-body"><nav className="agent-tree" aria-label="子代理列表">{execution.agents.map((agent) => <AgentTree key={agent.id} agent={agent} selectedID={selectedID} select={setSelectedID} depth={0} />)}</nav><section className="agent-log-panel">{selected ? <><header><div><span className={`agent-status ${selected.status}`}>{agentStatusLabel(selected.status)}</span><h3>{selected.summary}</h3></div><small>{formatTime(selected.createdAt)}</small></header><div className="agent-log-list">{selected.logs.length === 0 ? <p className="agent-log-empty">尚未收到该子代理的过程输出。</p> : selected.logs.map((log) => <article className={`agent-log ${log.kind} ${log.isError ? "failed" : ""}`} key={log.id}><header><b>{log.title}</b><time>{formatTime(log.createdAt)}</time></header><details open={log.kind === "text"}><summary>{log.detail.replace(/\s+/g, " ").slice(0, 180) || "无输出"}</summary><pre>{log.detail || "(无输出)"}</pre></details></article>)}</div></> : <p className="agent-log-empty">没有可查看的子代理。</p>}</section></div></section></div>;
}

function AgentTree({ agent, selectedID, select, depth }: { agent: AgentNode; selectedID: string; select: (id: string) => void; depth: number }) {
  const cappedDepth = Math.min(depth, 6);
  return <div className="agent-tree-branch"><button className={`agent-tree-row ${agent.id === selectedID ? "selected" : ""}`} style={{ paddingLeft: `${12 + cappedDepth * 16}px` }} onClick={() => select(agent.id)}><span className={`agent-status ${agent.status}`}></span><span><b>{agent.summary}</b><small>{agentStatusLabel(agent.status)} · {agent.logs.length} 条记录</small></span></button>{agent.children.map((child) => <AgentTree key={child.id} agent={child} selectedID={selectedID} select={select} depth={depth + 1} />)}</div>;
}

function ApprovalBanner({ action, resolving, decide, scrollToCard }: { action: ToolAction; resolving: string; decide: (approvalId: string, decision: "allow" | "deny") => Promise<void>; scrollToCard: () => void }) {
  const command = typeof action.input.command === "string" ? action.input.command : "";
  const description = typeof action.input.description === "string" ? action.input.description : "";
  const approval = action.approval;
  if (!approval) return null;
  return <div className="approval-banner">
    <div className="approval-banner-body">
      <span className="approval-banner-icon">⏳</span>
      <div className="approval-banner-info">
        <b>等待确认命令执行</b>
        <span>{description || command}</span>
      </div>
      <code className="approval-banner-command">{command}</code>
      <div className="approval-banner-actions">
        <button className="secondary" disabled={resolving === approval.approvalId} onClick={() => void decide(approval.approvalId, "deny")}>拒绝</button>
        <button className="primary" disabled={resolving === approval.approvalId} onClick={() => void decide(approval.approvalId, "allow")}>{resolving === approval.approvalId ? "处理中" : "允许执行"}</button>
        <button className="approval-banner-scroll" onClick={scrollToCard} title="滚动到命令卡片">↓</button>
      </div>
    </div>
  </div>;
}

function ShortcutVariablesDialog({ state, close, run }: { state: { shortcut: Shortcut; variables: Record<string, string> }; close: () => void; run: (variables: Record<string, string>) => void }) {
  const required = requiredShortcutVariables(state.shortcut.template);
  const [variables, setVariables] = useState(state.variables);
  useEffect(() => { setVariables(state.variables); }, [state]);
  const submit = (event: FormEvent) => { event.preventDefault(); run(variables); };
  return <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="shortcut-variables-title"><section className="modal shortcut-variables-dialog"><header><div><label>COMMON PROMPT</label><h2 id="shortcut-variables-title">{state.shortcut.name}</h2></div><button title="关闭" onClick={close}>x</button></header><form onSubmit={submit}><div className="shortcut-editor-body">{required.includes("selection") && <label>选中内容<textarea autoFocus required value={variables.selection || ""} onChange={(event) => setVariables((old) => ({ ...old, selection: event.target.value }))} placeholder="粘贴需要处理的内容" /></label>}{required.includes("error") && <label>错误信息<textarea autoFocus={!required.includes("selection")} required value={variables.error || ""} onChange={(event) => setVariables((old) => ({ ...old, error: event.target.value }))} placeholder="粘贴报错信息" /></label>}</div><footer><button type="button" className="secondary" onClick={close}>取消</button><button className="primary">发送</button></footer></form></section></div>;
}

function ShortcutEditor({ projectID, state, close, refresh, fail }: { projectID: string; state: ShortcutEditorState; close: () => void; refresh: () => Promise<void>; fail: (message: string) => void }) {
  const shortcut = state.shortcut;
  const [name, setName] = useState(shortcut?.name || "");
  const [template, setTemplate] = useState(shortcut?.template || "");
  const [enabled, setEnabled] = useState(shortcut?.enabled ?? true);
  const [busy, setBusy] = useState(false);
  const [pendingConfirm, setPendingConfirm] = useState<{ title: string; message: React.ReactNode; danger?: boolean; onConfirm: () => void; onCancel: () => void } | null>(null);
  const isCommand = state.kind === "command_request";
  const isSnippet = state.kind === "snippet";

  useEffect(() => {
    setName(state.shortcut?.name || "");
    setTemplate(state.shortcut?.template || "");
    setEnabled(state.shortcut?.enabled ?? true);
  }, [state]);

  const save = async (event: FormEvent) => {
    event.preventDefault();
    setBusy(true);
    const scope = shortcut?.scope || "local";
    try {
      await api(shortcut ? `/api/shortcuts/${shortcut.id}` : "/api/shortcuts", {
        method: shortcut ? "PATCH" : "POST",
        body: JSON.stringify({
          name,
          description: shortcut?.description || "",
          kind: state.kind,
          template,
          scope,
          defaultAction: isCommand ? "confirm" : isSnippet ? "fill" : "run",
          groupName: isCommand ? "常用命令" : "常用提示词",
          pinned: true,
          enabled,
          sortOrder: shortcut?.sortOrder || 0,
          projectIds: scope === "project" ? (shortcut?.projectIds.length ? shortcut.projectIds : [projectID]) : [],
        }),
      });
      await refresh();
      close();
    } catch (cause) { fail(cause instanceof Error ? cause.message : "Unable to save shortcut"); }
    finally { setBusy(false); }
  };
  const remove = () => {
    if (!shortcut) return;
    setPendingConfirm({
      title: "删除快捷方式",
      message: <>删除"<b>{shortcut.name}</b>"？</>,
      danger: true,
      onConfirm: () => void (async () => {
        setPendingConfirm(null);
        setBusy(true);
        try {
          await api(`/api/shortcuts/${shortcut.id}`, { method: "DELETE" });
          await refresh();
          close();
        } catch (cause) { fail(cause instanceof Error ? cause.message : "Unable to delete shortcut"); }
        finally { setBusy(false); }
      })(),
      onCancel: () => setPendingConfirm(null),
    });
  };

  return <><div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="shortcut-editor-title"><section className="modal shortcut-editor"><header><div><label>{isCommand ? "COMMON COMMAND" : "COMMON PROMPT"}</label><h2 id="shortcut-editor-title">{shortcut ? `编辑${isCommand ? "命令" : "提示词"}` : `新增${isCommand ? "命令" : "提示词"}`}</h2></div><button title="关闭" disabled={busy} onClick={close}>x</button></header><form onSubmit={(event) => void save(event)}><div className="shortcut-editor-body"><label>名称<input autoFocus required maxLength={64} value={name} onChange={(event) => setName(event.target.value)} placeholder={isCommand ? "例如：清空终端" : "例如：审查当前改动"} /></label><label>{isCommand ? "命令" : "提示词"}<textarea required maxLength={12000} value={template} onChange={(event) => setTemplate(event.target.value)} placeholder={isCommand ? "例如：clear" : "描述希望 Claude 在当前项目完成的工作"} /></label>{shortcut && <label className="shortcut-enabled"><input type="checkbox" checked={enabled} onChange={(event) => setEnabled(event.target.checked)} />启用此项</label>}{isCommand && <p>点击标签后会按当前会话权限请求执行该命令。</p>}</div><footer>{shortcut && <button type="button" className="danger-text" disabled={busy} onClick={() => void remove()}>删除</button>}<span></span><button type="button" className="secondary" disabled={busy} onClick={close}>取消</button><button className="primary" disabled={busy}>{busy ? "保存中" : "保存"}</button></footer></form></section></div>{pendingConfirm && createPortal(<ConfirmDialog title={pendingConfirm.title} message={pendingConfirm.message} danger={pendingConfirm.danger} onConfirm={pendingConfirm.onConfirm} onCancel={pendingConfirm.onCancel} />, document.body)}</>;
}

// ---- 主组件 ---------------------------------------------------------------

export default function ConversationPage() {
  const { projectId, conversationId: urlConversationId } = useParams<{ projectId: string; conversationId: string }>();
  const navigate = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();
  const { api: projectApi, setError } = useProjectContext();
  const { project } = useOutletContext<ProjectLayoutOutletContext>();
  const fail = setError;
  // 弹窗控制辅助函数 — 使用不可变模式创建新的 URLSearchParams
  const closeHistory = () => setSearchParams((prev) => { const next = new URLSearchParams(prev); next.delete("history"); return next; });
  const closeNewConversation = () => setSearchParams((prev) => { const next = new URLSearchParams(prev); next.delete("new"); return next; });
  const closeUsage = () => setSearchParams((prev) => { const next = new URLSearchParams(prev); next.delete("usage"); return next; });
  const closeAgentExecution = () => setSearchParams((prev) => { const next = new URLSearchParams(prev); next.delete("execution"); return next; });
  const openUsage = () => setSearchParams((prev) => { const next = new URLSearchParams(prev); next.set("usage", "true"); return next; });
  const openNewConversationParam = () => setSearchParams((prev) => { const next = new URLSearchParams(prev); next.set("new", "true"); return next; });
  const openExecutionParam = (runId: string) => setSearchParams((prev) => { const next = new URLSearchParams(prev); next.set("execution", runId); return next; });

  // 对话核心状态
  const [conversation, setConversation] = useState<Conversation | null>(null);
  const [conversationHistory, setConversationHistory] = useState<Conversation[]>([]);
  const [messages, setMessages] = useState<Message[]>([]);
  const [events, setEvents] = useState<Event[]>([]);
  const [text, setText] = useState("");
  const [inputHistory, setInputHistory] = useState<string[]>([]);
  const [historyRefresh, setHistoryRefresh] = useState(0);
  const [run, setRun] = useState("");
  const [sending, setSending] = useState(false);
  const [clearing, setClearing] = useState(false);
  const [resolving, setResolving] = useState("");
  const [stopping, setStopping] = useState(false);
  const [changingPermission, setChangingPermission] = useState(false);
  // 弹窗状态 — 通过 URL search params 驱动
  const showHistory = searchParams.get("history") === "true";
  const showNewConversation = searchParams.get("new") === "true";
  const showUsage = searchParams.get("usage") === "true";
  const showAgentExecution = searchParams.get("execution");
  const [showMobileActions, setShowMobileActions] = useState(false);
  const [showFullControlConfirmation, setShowFullControlConfirmation] = useState(false);
  const [activatingConversation, setActivatingConversation] = useState("");
  const [showPermissionMenu, setShowPermissionMenu] = useState(false);
  const [usage, setUsage] = useState<ConversationUsageResponse | null>(null);
  const [usageNow, setUsageNow] = useState(Date.now());
  const [shortcuts, setShortcuts] = useState<Shortcut[]>([]);
  const [shortcutEditor, setShortcutEditor] = useState<ShortcutEditorState | null>(null);
  const [shortcutVariables, setShortcutVariables] = useState<{ shortcut: Shortcut; variables: Record<string, string> } | null>(null);
  const [shortcutBusy, setShortcutBusy] = useState("");
  const [hasMoreHistory, setHasMoreHistory] = useState(false);
  const [hasMoreMessageHistory, setHasMoreMessageHistory] = useState(false);
  const [historyCursor, setHistoryCursor] = useState("");
  const [pendingConfirm, setPendingConfirm] = useState<{ title: string; message: React.ReactNode; danger?: boolean; onConfirm: () => void; onCancel: () => void } | null>(null);
  const [loadingOlderHistory, setLoadingOlderHistory] = useState(false);
  const [currentUserMessageIndex, setCurrentUserMessageIndex] = useState(-1);
  const [pendingPreviousUserMessageID, setPendingPreviousUserMessageID] = useState<string | null>(null);

  // Refs
  const bottom = useRef<HTMLDivElement>(null);
  const top = useRef<HTMLDivElement>(null);
  const composerRef = useRef<HTMLFormElement>(null);
  const timelineRef = useRef<HTMLElement>(null);
  const userMessageElements = useRef(new Map<string, HTMLDivElement>());
  const userMessageIndexFrame = useRef<number | null>(null);
  const bottomSafeAreaFrame = useRef<number | null>(null);
  const historyIndex = useRef<number | null>(null);
  const draftBeforeHistory = useRef("");
  const finishedRunIds = useRef(new Set<string>());
  const usageRequestVersion = useRef(0);
  const usageConversationID = useRef<string | null>(null);
  const conversationRef = useRef<Conversation | null>(null);
  conversationRef.current = conversation;
  const conversationTransitionRef = useRef(false);
  const conversationRouteVersion = useRef(0);
  const stopRunRef = useRef<() => Promise<void>>(async () => {});
  const userNearBottom = useRef(true);
  const [hasNewContent, setHasNewContent] = useState(false);
  const lastReloadRequestedAt = useRef(0);
  const lastReloadRunID = useRef<string | null>(null);

  // Route changes must invalidate a pending clear before promise callbacks can
  // write the previous conversation back into the page.
  useLayoutEffect(() => {
    conversationRouteVersion.current++;
  }, [projectId, urlConversationId]);

  const rememberFinishedRun = (runID: string) => {
    const finished = finishedRunIds.current;
    finished.add(runID);
    if (finished.size > 128) {
      const oldest = finished.values().next().value;
      if (oldest !== undefined) finished.delete(oldest);
    }
  };

  // 刷新函数
  const refreshConversationHistory = useCallback(async () => {
    if (!projectId) return [] as Conversation[];
    const list = await projectApi<Conversation[]>(`/api/projects/${projectId}/conversations`);
    setConversationHistory(list);
    return list;
  }, [projectId, projectApi]);

  const refreshShortcuts = useCallback(async () => {
    if (!projectId) return;
    setShortcuts(await projectApi<Shortcut[]>(`/api/projects/${projectId}/shortcuts?includeDisabled=true`));
  }, [projectId, projectApi]);

  const openConversationHistory = () => {
    setSearchParams((prev) => { const next = new URLSearchParams(prev); next.set("history", "true"); return next; });
    void refreshConversationHistory().catch((cause) => fail(cause instanceof Error ? cause.message : "Unable to refresh conversation history"));
  };

  const refreshUsage = useCallback(async () => {
    const conversationID = conversation?.id;
    if (!conversationID) return;
    const requestVersion = ++usageRequestVersion.current;
    let data: ConversationUsageResponse;
    try {
      data = await projectApi<ConversationUsageResponse>(`/api/conversations/${conversationID}/usage`);
    } catch (cause) {
      if (requestVersion === usageRequestVersion.current && usageConversationID.current === conversationID) throw cause;
      return;
    }
    if (requestVersion === usageRequestVersion.current && usageConversationID.current === conversationID) setUsage(data);
  }, [conversation?.id, projectApi]);

  useEffect(() => {
    usageConversationID.current = conversation?.id || null;
    usageRequestVersion.current += 1;
    setUsage(null);
  }, [conversation?.id]);

  // 加载或创建对话
  useEffect(() => {
    if (!projectId || conversationTransitionRef.current) return;
    let cancelled = false;
    async function loadConversation() {
      try {
        const list = await refreshConversationHistory();
        if (cancelled || conversationTransitionRef.current) return;

        // 如果 URL 里指定了 conversationId，尝试直接激活它
        if (urlConversationId) {
          const found = list.find((c) => c.id === urlConversationId);
          if (found && found.id && found.status !== "running") {
            const activated = await projectApi<Conversation>(`/api/conversations/${found.id}/activate`, { method: "POST" });
            if (!cancelled && !conversationTransitionRef.current) {
              setConversation(activated);
              setMessages([]);
              setEvents([]);
              setRun("");
              setUsage(null);
              setInputHistory([]);
              historyIndex.current = null;
              draftBeforeHistory.current = "";
              finishedRunIds.current.clear();
              setShowPermissionMenu(false);
              closeAgentExecution();
              setHasMoreHistory(false);
              setHasMoreMessageHistory(false);
              setHistoryCursor("");
              return;
            }
          }
        }

        // 否则使用最新对话或创建新对话
        const next = list[0] ?? await projectApi<Conversation>(`/api/projects/${projectId}/conversations`, { method: "POST" });
        if (!cancelled && !conversationTransitionRef.current) {
          setConversation(next);
          if (list.length === 0) setConversationHistory([next]);
          // 更新 URL 到当前对话
          if (next.id && !urlConversationId) {
            navigate(`/projects/${projectId}/conversations/${next.id}`, { replace: true });
          }
        }
      } catch (cause) {
        if (!cancelled) fail(cause instanceof Error ? cause.message : "Unable to create conversation");
      }
    }
    void loadConversation();
    return () => { cancelled = true; };
  }, [projectId, urlConversationId, clearing, fail, refreshConversationHistory, projectApi, navigate]);

  // 加载快捷方式
  useEffect(() => {
    if (!projectId) return;
    let cancelled = false;
    void (async () => {
      try {
        await projectApi("/api/shortcuts/defaults", { method: "POST" });
        if (!cancelled) await refreshShortcuts();
      } catch (cause) { if (!cancelled) fail(cause instanceof Error ? cause.message : "Unable to load shortcuts"); }
    })();
    return () => { cancelled = true; };
  }, [fail, refreshShortcuts, projectApi, projectId]);

  // WebSocket 连接
  useEffect(() => {
    if (!conversation) return undefined;
    let cancelled = false;
    let socket: WebSocket;
    let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
    let reconnectAttempts = 0;
    const isCurrentConversation = () => !cancelled && conversationRef.current?.id === conversation.id;

    const reload = async (runID?: string) => {
      if (!isCurrentConversation()) return;
      if (runID) lastReloadRunID.current = runID;
      lastReloadRequestedAt.current = Date.now();
      try {
        const data = await projectApi<{ messages: Message[]; events: Event[]; activeRunId: string | null; hasMore: boolean; hasMoreMessages: boolean; nextCursor: string }>(`/api/conversations/${conversation.id}?limit=400`);
        if (!isCurrentConversation()) return;
        setMessages((current) => mergeReloadMessages(data.messages, current));
        setEvents((current) => mergeConversationItems(data.events, current));
        setRun(data.activeRunId || ""); setHasMoreHistory(data.hasMore); setHasMoreMessageHistory(data.hasMoreMessages); setHistoryCursor(data.nextCursor || "");
      } catch (cause) { if (isCurrentConversation()) fail(cause instanceof Error ? cause.message : "Unable to refresh conversation"); }
    };

    const connect = () => {
      if (!isCurrentConversation()) return;
      reconnectAttempts++;
      socket = new WebSocket(`${location.protocol === "https:" ? "wss" : "ws"}://${location.host}/ws/conversations/${conversation.id}`);
      socket.onopen = () => {
        if (!isCurrentConversation()) return;
        reconnectAttempts = 0;
        void reload();
      };
      socket.onmessage = (raw) => {
        if (!isCurrentConversation()) return;
        const event = JSON.parse(raw.data) as Event;
        setEvents((old) => old.some((item) => item.id === event.id) ? old : [...old, event]);
        if (event.type === "assistant.message") {
          const m = asRecord(event.payload);
          if (typeof m.id === "string" && typeof m.content === "string") {
            const canonicalID = String(m.id);
            const canonicalContent = String(m.content);
            const canonicalRunID = event.runId;
            const canonicalParent = typeof m.parentToolUseId === "string" ? m.parentToolUseId : "";
            const canonicalCreatedAt = typeof m.createdAt === "string" ? m.createdAt : event.createdAt;
            setMessages((old) => {
              const filtered = old.filter((item) => {
                if (!isTemporaryMessage(item) || item.role !== "assistant") return true;
                if (item.runId !== canonicalRunID) return true;
                if ((item.parentToolUseId || "") !== canonicalParent) return true;
                if (item.content !== canonicalContent) return true;
                return false;
              });
              return mergeConversationItems(
                filtered,
                [{ id: canonicalID, runId: canonicalRunID, role: "assistant" as const, content: canonicalContent, parentToolUseId: canonicalParent, createdAt: canonicalCreatedAt }],
              );
            });
          }
        }
        if (event.type === "assistant") {
          const content = asRecord(asRecord(event.payload).message).content || [];
          const parentToolUseId = typeof asRecord(event.payload).parent_tool_use_id === "string" ? asRecord(event.payload).parent_tool_use_id : "";
          for (const [index, part] of content.entries()) {
            if (part?.type !== "text" || typeof part.text !== "string") continue;
            const textPart = part.text;
            setMessages((old) => {
              const alreadyCanonical = old.some(
                (item) =>
                  item.role === "assistant" &&
                  item.runId === event.runId &&
                  (item.parentToolUseId || "") === parentToolUseId &&
                  item.content === textPart &&
                  !isTemporaryMessage(item),
              );
              if (alreadyCanonical) return old;
              return mergeConversationItems(old, [
                {
                  id: websocketAssistantMessageID(event.id, index),
                  runId: event.runId,
                  role: "assistant" as const,
                  content: textPart,
                  parentToolUseId,
                  createdAt: event.createdAt,
                },
              ]);
            });
          }
        }
        if (event.type === "usage.updated" || event.type.startsWith("run.")) {
          void refreshUsage().catch((cause) => fail(cause instanceof Error ? cause.message : "Unable to refresh usage"));
        }
        if (event.type.startsWith("run.")) {
          const runPayload = asRecord(event.payload);
          if (typeof runPayload.interruptedMarker === "string" && runPayload.interruptedMarker) {
            const markerMessage: Message = { id: `${event.id}:interrupted`, runId: event.runId, role: "assistant", content: runPayload.interruptedMarker, createdAt: event.createdAt };
            setMessages((old) => mergeConversationItems(old, [markerMessage]));
          }
          rememberFinishedRun(event.runId);
          setRun((active) => active === event.runId ? "" : active);
          setStopping(false);
          const sameRun = lastReloadRunID.current === event.runId;
          const recentReload = Date.now() - lastReloadRequestedAt.current < 500;
          if (!sameRun || !recentReload) {
            void reload(event.runId);
          }
        }
      };
      socket.onerror = () => {
        // onclose fires after this, handling reconnection
      };
      socket.onclose = (closeEvent) => {
        if (!isCurrentConversation()) return;
        if (closeEvent.code === 1000) return;
        const maxAttempts = 12;
        if (reconnectAttempts >= maxAttempts) {
          fail("实时连接已断开，请刷新页面重新建立连接。");
          setRun("");
          setStopping(false);
          void reload();
          return;
        }
        const delay = Math.min(500 * Math.pow(2, reconnectAttempts - 1), 15_000);
        reconnectTimer = setTimeout(connect, delay);
      };
    };

    connect();

    return () => {
      cancelled = true;
      if (reconnectTimer) clearTimeout(reconnectTimer);
      try { socket.close(); } catch { /* already closed */ }
    };
  }, [conversation?.id, fail, refreshUsage, projectApi]);

  // 刷新使用统计
  useEffect(() => {
    if (!conversation) return undefined;
    let cancelled = false;
    void refreshUsage().catch((cause) => { if (!cancelled) fail(cause instanceof Error ? cause.message : "Unable to load usage"); });
    return () => { cancelled = true; };
  }, [conversation?.id, fail, refreshUsage]);

  // 运行计时器
  useEffect(() => {
    if (!run) return undefined;
    setUsageNow(Date.now());
    const timer = window.setInterval(() => setUsageNow(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, [run]);

  // HTTP 轮询回退
  useEffect(() => {
    if (!run || !conversation?.id) return undefined;
    let cancelled = false;
    const poll = async () => {
      try {
        const data = await projectApi<{ activeRunId: string | null }>(`/api/conversations/${conversation.id}?limit=1`);
        if (cancelled) return;
        if (!data.activeRunId) {
          const full = await projectApi<{ messages: Message[]; events: Event[]; activeRunId: string | null; hasMore: boolean; hasMoreMessages: boolean; nextCursor: string }>(`/api/conversations/${conversation.id}?limit=400`);
          if (cancelled) return;
          setMessages((current) => mergeReloadMessages(full.messages, current));
          setEvents((current) => mergeConversationItems(full.events, current));
          setRun(full.activeRunId || ""); setHasMoreHistory(full.hasMore); setHasMoreMessageHistory(full.hasMoreMessages); setHistoryCursor(full.nextCursor || "");
          setStopping(false);
        }
      } catch {
        // silently ignore
      }
    };
    const interval = window.setInterval(() => { void poll(); }, 15_000);
    return () => {
      cancelled = true;
      window.clearInterval(interval);
    };
  }, [run, conversation?.id, projectApi]);

  // 对话切换时重置输入历史
  useEffect(() => {
    if (!conversation) return;
    historyIndex.current = null;
    draftBeforeHistory.current = "";
    setInputHistory([]);
  }, [conversation?.id]);

  // 加载输入历史
  useEffect(() => {
    if (!conversation) return undefined;
    let cancelled = false;
    const loadHistory = async () => {
      try {
        const history = await projectApi<string[]>(`/api/conversations/${conversation.id}/input-history?limit=100`);
        if (!cancelled) setInputHistory(history);
      } catch (cause) { if (!cancelled) fail(cause instanceof Error ? cause.message : "Unable to load input history"); }
    };
    void loadHistory();
    return () => { cancelled = true; };
  }, [conversation?.id, fail, historyRefresh, projectApi]);

  // 计算属性
  const knownSubagentTexts = useMemo(() => subagentTextIndex(events), [events]);
  const primaryMessages = useMemo(() => messages.filter((message) => !isSubagentMessage(message, knownSubagentTexts)), [knownSubagentTexts, messages]);
  const userMessages = useMemo(() => primaryMessages.filter((message) => message.role === "user"), [primaryMessages]);
  const timeline = useMemo(() => buildTimeline(primaryMessages, events), [events, primaryMessages]);
  const agentExecutions = useMemo(() => buildAgentExecutions(events), [events]);
  const executionByRun = useMemo(() => new Map(agentExecutions.map((execution) => [execution.runId, execution])), [agentExecutions]);
  const anchoredExecutionRunIDs = useMemo(() => new Set(primaryMessages.flatMap((message) => message.role === "user" && message.runId && executionByRun.has(message.runId) ? [message.runId] : [])), [executionByRun, primaryMessages]);
  const visibleContentVersion = useMemo(() => timelineContentVersion(timeline, agentExecutions), [timeline, agentExecutions]);

  const loadOlderHistory = async (): Promise<boolean> => {
    if (!conversation || !historyCursor || loadingOlderHistory || sending) return false;
    const conversationID = conversation.id;
    const narrowLayout = isNarrowConversationLayout();
    const container = timelineRef.current;
    const scrollTop = container?.scrollTop || 0;
    const scrollHeight = narrowLayout ? document.documentElement.scrollHeight : container?.scrollHeight || 0;
    userNearBottom.current = false;
    setLoadingOlderHistory(true);
    try {
      const data = await projectApi<{ messages: Message[]; events: Event[]; hasMore: boolean; hasMoreMessages: boolean; nextCursor: string }>(`/api/conversations/${conversationID}?limit=400&cursor=${encodeURIComponent(historyCursor)}`);
      if (conversationRef.current?.id !== conversationID) return false;
      setMessages((current) => mergeConversationItems(data.messages, current));
      setEvents((current) => mergeConversationItems(data.events, current));
      setHasMoreHistory(data.hasMore);
      setHasMoreMessageHistory(data.hasMoreMessages);
      setHistoryCursor(data.nextCursor || "");
      requestAnimationFrame(() => {
        if (conversationRef.current?.id !== conversationID) return;
        const heightDelta = (narrowLayout ? document.documentElement.scrollHeight : container?.scrollHeight || 0) - scrollHeight;
        if (heightDelta <= 0) return;
        if (narrowLayout) window.scrollBy({ top: heightDelta });
        else if (container) container.scrollTop = scrollTop + heightDelta;
      });
      return true;
    } catch (cause) {
      fail(cause instanceof Error ? cause.message : "Unable to load earlier conversation history");
      return false;
    } finally { setLoadingOlderHistory(false); }
  };

  const pendingApproval = useMemo(() => {
    for (const item of timeline) {
      if (item.kind === "tool") {
        const approval = item.action.approval;
        if (approval?.status === "pending" && !item.action.output) return item.action;
      }
    }
    return null;
  }, [timeline]);

  // 滚动
  const scrollToBottom = useCallback((behavior: ScrollBehavior = "smooth") => {
    if (isNarrowConversationLayout()) {
      window.scrollTo({ top: document.documentElement.scrollHeight, behavior });
      setHasNewContent(false);
      userNearBottom.current = true;
      setCurrentUserMessageIndex(userMessages.length - 1);
      return;
    }
    const container = timelineRef.current;
    if (!container) return;
    container.scrollTo({ top: Math.max(0, container.scrollHeight - container.clientHeight), behavior });
    setHasNewContent(false);
    userNearBottom.current = true;
    setCurrentUserMessageIndex(userMessages.length - 1);
  }, [userMessages.length]);

  const followAfterDispatch = useCallback(() => {
    userNearBottom.current = true;
    setHasNewContent(false);
    requestAnimationFrame(() => scrollToBottom("auto"));
  }, [scrollToBottom]);

  const scrollToTop = useCallback(() => {
    userNearBottom.current = false;
    setCurrentUserMessageIndex(userMessages.length > 0 ? 0 : -1);
    if (isNarrowConversationLayout()) {
      top.current?.scrollIntoView({ behavior: "smooth", block: "start" });
      return;
    }
    const container = timelineRef.current;
    if (!container) { top.current?.scrollIntoView({ behavior: "smooth", block: "start" }); return; }
    container.scrollTo({ top: 0, behavior: "smooth" });
  }, [userMessages.length]);

  const currentUserMessageIndexAtViewport = useCallback(() => {
    if (userMessages.length === 0) return -1;
    const viewportTop = isNarrowConversationLayout() ? 0 : timelineRef.current?.getBoundingClientRect().top || 0;
    let low = 0;
    let high = userMessages.length - 1;
    let index = 0;
    while (low <= high) {
      const candidate = Math.floor((low + high) / 2);
      const element = userMessageElements.current.get(userMessages[candidate].id);
      if (!element || element.getBoundingClientRect().top > viewportTop + 16) {
        high = candidate - 1;
      } else {
        index = candidate;
        low = candidate + 1;
      }
    }
    return index;
  }, [userMessages]);

  const updateCurrentUserMessageIndex = useCallback(() => {
    const next = currentUserMessageIndexAtViewport();
    setCurrentUserMessageIndex((current) => current === next ? current : next);
  }, [currentUserMessageIndexAtViewport]);

  const scheduleCurrentUserMessageIndexUpdate = useCallback(() => {
    if (userMessageIndexFrame.current !== null) return;
    userMessageIndexFrame.current = requestAnimationFrame(() => {
      userMessageIndexFrame.current = null;
      updateCurrentUserMessageIndex();
    });
  }, [updateCurrentUserMessageIndex]);

  const scrollToUserMessage = useCallback((index: number) => {
    const message = userMessages[index];
    const element = message && userMessageElements.current.get(message.id);
    if (!element) return false;
    userNearBottom.current = false;
    element.scrollIntoView({ behavior: "smooth", block: "start" });
    setCurrentUserMessageIndex(index);
    return true;
  }, [userMessages]);

  const goToPreviousUserMessage = () => {
    if (loadingOlderHistory) return;
    const index = currentUserMessageIndexAtViewport();
    if (index > 0) {
      scrollToUserMessage(index - 1);
      return;
    }
    if (index === -1 && hasMoreMessageHistory) {
      setPendingPreviousUserMessageID("");
      void loadOlderHistory().then((loaded) => { if (!loaded) setPendingPreviousUserMessageID(null); });
      return;
    }
    if (index === 0 && hasMoreMessageHistory) {
      setPendingPreviousUserMessageID(userMessages[0].id);
      void loadOlderHistory().then((loaded) => { if (!loaded) setPendingPreviousUserMessageID(null); });
    }
  };

  const goToNextUserMessage = () => {
    const index = currentUserMessageIndexAtViewport();
    if (index >= 0 && index < userMessages.length - 1) scrollToUserMessage(index + 1);
  };

  const onTimelineScroll = () => {
    if (isNarrowConversationLayout()) return;
    const container = timelineRef.current;
    if (!container) return;
    const nearBottom = container.scrollHeight - container.scrollTop - container.clientHeight < 64;
    userNearBottom.current = nearBottom;
    if (nearBottom) setHasNewContent(false);
    scheduleCurrentUserMessageIndexUpdate();
  };

  // 对话切换时重置滚动位置
  useEffect(() => {
    userNearBottom.current = true;
    setHasNewContent(false);
    setCurrentUserMessageIndex(-1);
    setPendingPreviousUserMessageID(null);
  }, [conversation?.id]);

  // 窄屏滚动处理
  useEffect(() => {
    const media = window.matchMedia("(max-width: 820px)");
    const onScroll = () => {
      if (!media.matches) return;
      const distanceFromBottom = document.documentElement.scrollHeight - window.scrollY - window.innerHeight;
      const composerHeight = composerRef.current?.getBoundingClientRect().height ?? 80;
      const threshold = Math.max(composerHeight, 60);
      const nearBottom = distanceFromBottom <= threshold;
      userNearBottom.current = nearBottom;
      if (nearBottom) {
        setHasNewContent((prev) => prev ? false : prev);
      }
      scheduleCurrentUserMessageIndexUpdate();
    };
    const onLayoutChange = () => {
      if (media.matches) {
        onScroll();
        return;
      }
      const container = timelineRef.current;
      if (!container) return;
      const nearBottom = container.scrollHeight - container.scrollTop - container.clientHeight < 64;
      userNearBottom.current = nearBottom;
      if (nearBottom) setHasNewContent((prev) => prev ? false : prev);
      scheduleCurrentUserMessageIndexUpdate();
    };
    window.addEventListener("scroll", onScroll, { passive: true });
    media.addEventListener("change", onLayoutChange);
    onLayoutChange();
    return () => {
      window.removeEventListener("scroll", onScroll);
      media.removeEventListener("change", onLayoutChange);
    };
  }, [scheduleCurrentUserMessageIndexUpdate]);

  useEffect(() => () => {
    if (userMessageIndexFrame.current !== null) cancelAnimationFrame(userMessageIndexFrame.current);
  }, []);

  useLayoutEffect(() => {
    const composer = composerRef.current;
    const timelineElement = timelineRef.current;
    if (!composer || !timelineElement) return;
    const updateBottomSafeArea = () => {
      const followBottom = userNearBottom.current;
      timelineElement.style.setProperty("--composer-height", `${Math.ceil(composer.getBoundingClientRect().height)}px`);
      if (!followBottom) return;
      if (bottomSafeAreaFrame.current !== null) cancelAnimationFrame(bottomSafeAreaFrame.current);
      bottomSafeAreaFrame.current = requestAnimationFrame(() => {
        bottomSafeAreaFrame.current = null;
        if (userNearBottom.current) scrollToBottom("auto");
      });
    };
    const observer = new ResizeObserver(updateBottomSafeArea);
    observer.observe(composer);
    updateBottomSafeArea();
    return () => {
      observer.disconnect();
      if (bottomSafeAreaFrame.current !== null) {
        cancelAnimationFrame(bottomSafeAreaFrame.current);
        bottomSafeAreaFrame.current = null;
      }
      timelineElement.style.removeProperty("--composer-height");
    };
  }, [pendingApproval, scrollToBottom]);

  useEffect(() => {
    const frame = requestAnimationFrame(updateCurrentUserMessageIndex);
    return () => cancelAnimationFrame(frame);
  }, [updateCurrentUserMessageIndex, visibleContentVersion]);

  // 历史导航
  useEffect(() => {
    if (pendingPreviousUserMessageID === null || loadingOlderHistory) return;
    if (pendingPreviousUserMessageID === "" && userMessages.length > 0) {
      setPendingPreviousUserMessageID(null);
      requestAnimationFrame(() => { scrollToUserMessage(userMessages.length - 1); });
      return;
    }
    const currentIndex = userMessages.findIndex((message) => message.id === pendingPreviousUserMessageID);
    if (currentIndex > 0) {
      setPendingPreviousUserMessageID(null);
      requestAnimationFrame(() => { scrollToUserMessage(currentIndex - 1); });
      return;
    }
    if (!hasMoreMessageHistory) {
      setPendingPreviousUserMessageID(null);
      return;
    }
    void loadOlderHistory().then((loaded) => { if (!loaded) setPendingPreviousUserMessageID(null); });
  }, [hasMoreMessageHistory, loadOlderHistory, loadingOlderHistory, pendingPreviousUserMessageID, scrollToUserMessage, userMessages]);

  // Ctrl+C 停止
  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (!run || stopping) return;
      if ((event.ctrlKey || event.metaKey) && event.key === "c") {
        const selection = window.getSelection();
        if (selection && selection.toString().trim()) return;
        const tag = document.activeElement?.tagName;
        if (tag === "TEXTAREA" || tag === "INPUT") return;
        event.preventDefault();
        void stopRunRef.current();
      }
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [run, stopping]);

  // 自动滚动
  useEffect(() => {
    if (pendingApproval) return;
    if (userNearBottom.current) {
      scrollToBottom("auto");
    } else {
      setHasNewContent((prev) => prev ? prev : true);
    }
  }, [visibleContentVersion, run, pendingApproval, scrollToBottom]);

  // 业务方法
  const sendContent = async (rawContent: string, clearDraft = true) => {
    if (!conversation || sending || clearing || shortcutBusy) return;
    const content = rawContent.trim();
    if (!content) return;
    if (content === "/resume") { if (clearDraft) setText(""); openConversationHistory(); return; }
    const conversationID = conversation.id;
    setSending(true);
    if (clearDraft) setText("");
    setShowPermissionMenu(false); historyIndex.current = null; draftBeforeHistory.current = "";
    try {
      const data = await projectApi<{ message: Message; runId: string }>(`/api/conversations/${conversationID}/messages`, { method: "POST", body: JSON.stringify({ content }) });
      if (conversationRef.current?.id !== conversationID) return;
      setMessages((old) => [...old, data.message]); setInputHistory((old) => [...old.slice(-99), data.message.content]); setHistoryRefresh((version) => version + 1); setRun(finishedRunIds.current.has(data.runId) ? "" : data.runId);
      followAfterDispatch();
      void refreshUsage().catch((cause) => fail(cause instanceof Error ? cause.message : "Unable to refresh usage"));
      void refreshConversationHistory().catch((cause) => fail(cause instanceof Error ? cause.message : "Unable to refresh conversation history"));
    } catch (cause) { fail(cause instanceof Error ? cause.message : "Unable to send message"); if (clearDraft) setText(content); }
    finally { setSending(false); }
  };

  const send = (event: FormEvent) => {
    event.preventDefault();
    void sendContent(text);
  };

  const runShortcut = async (shortcut: Shortcut, variables: Record<string, string> = {}, variablesReady = false) => {
    if (!conversation || sending || clearing || shortcutBusy) return;
    const required = requiredShortcutVariables(shortcut.template);
    if (!variablesReady && required.length) { setShortcutVariables({ shortcut, variables }); return; }
    if (shortcut.defaultAction === "fill") {
      setShortcutBusy(shortcut.id);
      try {
        const preview = await projectApi<{ content: string }>(`/api/conversations/${conversation.id}/shortcuts/${shortcut.id}/preview`, { method: "POST", body: JSON.stringify({ variables }) });
        setText(preview.content);
      } catch (cause) { fail(cause instanceof Error ? cause.message : "Unable to prepare shortcut"); }
      finally { setShortcutBusy(""); }
      return;
    }
    setShortcutBusy(shortcut.id);
    try {
      const action = shortcut.defaultAction === "confirm" ? "confirm" : "run";
      const conversationID = conversation.id;
      const data = await projectApi<{ message: Message; runId: string }>(`/api/conversations/${conversationID}/shortcuts/${shortcut.id}/run`, { method: "POST", body: JSON.stringify({ variables, action }) });
      if (conversationRef.current?.id !== conversationID) return;
      setMessages((old) => [...old, data.message]);
      setInputHistory((old) => [...old.slice(-99), data.message.content]);
      setHistoryRefresh((version) => version + 1);
      setRun(finishedRunIds.current.has(data.runId) ? "" : data.runId);
      followAfterDispatch();
      void refreshUsage().catch((cause) => fail(cause instanceof Error ? cause.message : "Unable to refresh usage"));
      void refreshConversationHistory().catch((cause) => fail(cause instanceof Error ? cause.message : "Unable to refresh conversation history"));
    } catch (cause) { fail(cause instanceof Error ? cause.message : "Unable to run shortcut"); }
    finally { setShortcutBusy(""); }
  };

  const resetConversationView = (next: Conversation, clearDraft = true) => {
    setMessages([]); setEvents([]); setRun(""); setUsage(null); if (clearDraft) setText(""); setInputHistory([]); historyIndex.current = null; draftBeforeHistory.current = ""; finishedRunIds.current.clear(); setShowPermissionMenu(false); setShowFullControlConfirmation(false); closeAgentExecution(); setHasMoreHistory(false); setHasMoreMessageHistory(false); setHistoryCursor(""); setLoadingOlderHistory(false); setCurrentUserMessageIndex(-1); setPendingPreviousUserMessageID(null); setHasNewContent(false); userNearBottom.current = true; setConversation(next);
  };

  const newConversation = async (agentId: AgentID, permissionMode: PermissionMode) => {
    if (sending || clearing || shortcutBusy || run || !projectId) { closeNewConversation(); return; }
    conversationTransitionRef.current = true;
    setClearing(true);
    try {
      const next = await projectApi<Conversation>(`/api/projects/${projectId}/conversations?new=true`, { method: "POST", body: JSON.stringify({ agentId, permissionMode }) });
      resetConversationView(next);
      navigate(`/projects/${projectId}/conversations/${next.id}`, { replace: true });
      void refreshConversationHistory().catch((cause) => fail(cause instanceof Error ? cause.message : "Unable to refresh conversation history"));
    }
    catch (cause) { fail(cause instanceof Error ? cause.message : "Unable to start a new conversation"); }
    finally { conversationTransitionRef.current = false; setClearing(false); }
  };

  const clearConversationContext = () => {
    if (!conversation || run || sending || clearing || shortcutBusy) return;
    setPendingConfirm({
      title: "清空上下文",
      message: "将开始一个新的空白会话，当前内容仍可在历史会话中恢复。",
      onConfirm: () => {
        setPendingConfirm(null);
        void clearCurrentConversation();
      },
      onCancel: () => setPendingConfirm(null),
    });
  };

  const clearCurrentConversation = async () => {
    if (!conversation || run || sending || clearing || shortcutBusy) return;
    const conversationID = conversation.id;
    const routeVersion = conversationRouteVersion.current;
    const routeConversationID = urlConversationId;
    const clearStillOwnsView = () =>
      conversationRef.current?.id === conversationID &&
      conversationRouteVersion.current === routeVersion &&
      (!routeConversationID || routeConversationID === conversationID);
    conversationTransitionRef.current = true;
    setClearing(true);
    try {
      const next = await projectApi<Conversation>(`/api/conversations/${conversationID}/clear`, { method: "POST" });
      if (!clearStillOwnsView()) return;
      resetConversationView(next);
      if (projectId) navigate(`/projects/${projectId}/conversations/${next.id}`, { replace: true });
      void refreshConversationHistory().catch((cause) => fail(cause instanceof Error ? cause.message : "Unable to refresh conversation history"));
    } catch (cause) {
      // The request can fail after the server has committed the new
      // conversation. Reconcile before leaving the page on a historical one.
      if (clearStillOwnsView()) {
        try {
          const list = await refreshConversationHistory();
          const current = list.find((item) => item.isCurrent);
          if (clearStillOwnsView() && current && current.id !== conversationID) {
            resetConversationView(current);
            if (projectId) navigate(`/projects/${projectId}/conversations/${current.id}`, { replace: true });
            return;
          }
        } catch {
          // Preserve the original request error below when reconciliation fails.
        }
      }
      if (clearStillOwnsView()) fail(cause instanceof Error ? cause.message : "Unable to clear conversation context");
    } finally {
      conversationTransitionRef.current = false;
      setClearing(false);
    }
  };

  const activateConversation = async (item: Conversation) => {
    if (!conversation || item.id === conversation.id || item.status === "running") { closeHistory(); return; }
    if (sending || clearing || shortcutBusy) { closeHistory(); return; }
    setActivatingConversation(item.id);
    try {
      const next = await projectApi<Conversation>(`/api/conversations/${item.id}/activate`, { method: "POST" });
      resetConversationView(next, false); closeHistory();
      if (projectId) navigate(`/projects/${projectId}/conversations/${next.id}`, { replace: true });
      void refreshConversationHistory().catch((cause) => fail(cause instanceof Error ? cause.message : "Unable to refresh conversation history"));
    } catch (cause) { fail(cause instanceof Error ? cause.message : "Unable to switch conversation"); }
    finally { setActivatingConversation(""); }
  };

  const changePermissionMode = async (permissionMode: PermissionMode) => {
    if (!conversation || conversation.permissionMode === permissionMode || run || clearing) return;
    if (permissionMode === "full_control") { setShowPermissionMenu(false); setShowFullControlConfirmation(true); return; }
    const conversationID = conversation.id;
    setChangingPermission(true);
    try {
      const updated = await projectApi<Conversation>(`/api/conversations/${conversationID}/permission-mode`, { method: "POST", body: JSON.stringify({ permissionMode }) });
      if (conversationRef.current?.id !== conversationID) return;
      setConversation(updated);
      setShowPermissionMenu(false);
    }
    catch (cause) { fail(cause instanceof Error ? cause.message : "Unable to change permission mode"); }
    finally { setChangingPermission(false); }
  };

  const confirmFullControl = async () => {
    if (!conversation || run || clearing) return;
    const conversationID = conversation.id;
    setChangingPermission(true);
    try {
      const updated = await projectApi<Conversation>(`/api/conversations/${conversationID}/permission-mode`, { method: "POST", body: JSON.stringify({ permissionMode: "full_control" }) });
      if (conversationRef.current?.id !== conversationID) return;
      setConversation(updated);
      setShowFullControlConfirmation(false);
    } catch (cause) { fail(cause instanceof Error ? cause.message : "Unable to change permission mode"); }
    finally { setChangingPermission(false); }
  };

  const decide = async (approvalId: string, decision: "allow" | "deny") => {
    setResolving(approvalId);
    try { await projectApi(`/api/approvals/${approvalId}`, { method: "POST", body: JSON.stringify({ decision }) }); }
    catch (cause) { fail(cause instanceof Error ? cause.message : "Unable to resolve command approval"); }
    finally { setResolving(""); }
  };

  const stopRunInternal = async (force: boolean) => {
    const runID = run;
    if (!runID) return;
    const url = force ? `/api/runs/${runID}/stop?force=true` : `/api/runs/${runID}/stop`;
    const result = await projectApi<{ status: string }>(url, { method: "POST" });
    if (result.status !== "stopping") { rememberFinishedRun(runID); setRun((active) => active === runID ? "" : active); setStopping(false); }
  };

  const stopRun = async () => {
    if (!run || stopping) return;
    setStopping(true);
    try {
      await stopRunInternal(false);
    }
    catch (cause) {
      const message = cause instanceof Error ? cause.message : "";
      if (message.includes("queued") || message.includes("other queued") || message.includes("other queued or running")) {
        setPendingConfirm({
          title: "强制停止",
          message: "该对话还有其他排队中或执行中的请求，强制停止将一并取消它们。是否继续？",
          danger: true,
          onConfirm: () => {
            setPendingConfirm(null);
            void stopRunInternal(true).catch((cause) => {
              fail(cause instanceof Error ? cause.message : "Unable to stop run");
              setStopping(false);
            });
          },
          onCancel: () => { setPendingConfirm(null); setStopping(false); },
        });
        return;
      }
      fail(cause instanceof Error ? cause.message : "Unable to stop run");
      setStopping(false);
    }
  };
  stopRunRef.current = stopRun;

  const handleTextChange = (value: string) => {
    historyIndex.current = null;
    draftBeforeHistory.current = "";
    setText(value);
  };

  const navigateInputHistory = (event: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if (event.nativeEvent.isComposing || event.shiftKey || (event.key !== "ArrowUp" && event.key !== "ArrowDown")) return;
    const textarea = event.currentTarget;
    const browsing = historyIndex.current !== null;
    if (!browsing && textarea.selectionStart !== textarea.selectionEnd) return;
    if (event.key === "ArrowUp") {
      if (inputHistory.length === 0 || (!browsing && textarea.value.slice(0, textarea.selectionStart).includes("\n"))) return;
      event.preventDefault();
      if (!browsing) {
        draftBeforeHistory.current = text;
        historyIndex.current = inputHistory.length - 1;
      } else {
        historyIndex.current = Math.max(0, historyIndex.current! - 1);
      }
      setText(inputHistory[historyIndex.current!]);
      return;
    }
    const currentIndex = historyIndex.current;
    if (currentIndex === null) return;
    event.preventDefault();
    if (currentIndex < inputHistory.length - 1) {
      const nextIndex = currentIndex + 1;
      historyIndex.current = nextIndex;
      setText(inputHistory[nextIndex]);
    } else {
      historyIndex.current = null;
      setText(draftBeforeHistory.current);
      draftBeforeHistory.current = "";
    }
  };

  const handleTaskDispatched = (message: Message, runID: string) => {
    setMessages((items) => [...items, message]);
    setInputHistory((items) => [...items.slice(-99), message.content]);
    setHistoryRefresh((version) => version + 1);
    setRun(finishedRunIds.current.has(runID) ? "" : runID);
    void refreshUsage().catch((cause) => fail(cause instanceof Error ? cause.message : "Unable to refresh usage"));
    void refreshConversationHistory().catch((cause) => fail(cause instanceof Error ? cause.message : "Unable to refresh conversation history"));
  };

  const isCodex = conversation?.agentId === "codex";
  const permissionLabel = isCodex ? (conversation?.permissionMode === "read_only" ? "仅分析" : conversation?.permissionMode === "full_control" ? "完全控制" : "项目内执行") : conversation?.permissionMode === "full_control" ? "完全控制" : "默认权限";
  const runLabel = isCodex ? "Codex 正在处理已发送内容" : conversation?.permissionMode === "full_control" ? "Claude 正在处理已发送内容" : "Claude 正在分析或等待命令确认";
  const currentUsage = usage?.currentRun ?? usage?.latestRun;
  const displayedModel = usage?.context.model || currentUsage?.model || (isCodex ? "Codex" : "Claude Code");
  const promptShortcuts = shortcuts.filter((shortcut) => shortcut.kind === "prompt" || shortcut.kind === "snippet");
  const commandShortcuts = shortcuts.filter((shortcut) => shortcut.kind === "command_request");
  const promptShortcutItems = promptShortcuts.length > 0 ? promptShortcuts : [undefined];
  const commandShortcutItems = commandShortcuts.length > 0 ? commandShortcuts : [undefined];
  const userMessageNavigationIndex = userMessages.length === 0 ? -1 : Math.max(0, Math.min(currentUserMessageIndex, userMessages.length - 1));
  const shortcutRowCount = Math.max(promptShortcutItems.length, commandShortcutItems.length);

  const renderShortcutCell = (shortcut: Shortcut | undefined, kind: "prompt" | "command_request", placeholder = false) => {
    if (!shortcut && placeholder) return <div className="quick-tag-slot" aria-hidden="true" />;
    if (!shortcut) return <button type="button" className={`quick-tag-empty${kind === "command_request" ? " command-tag" : ""}`} onClick={() => setShortcutEditor({ kind })}>{kind === "command_request" ? "添加命令" : "添加提示词"}</button>;
    return <div className={`quick-tag ${shortcut.enabled ? "" : "disabled"}${kind === "command_request" ? " command-tag" : ""}`} key={shortcut.id}><button type="button" disabled={!shortcut.enabled || !conversation || sending || clearing || Boolean(shortcutBusy)} onClick={() => void runShortcut(shortcut)} title={shortcut.enabled ? shortcut.template : `${shortcut.name}已停用`}>{shortcutBusy === shortcut.id ? "发送中" : shortcut.name}</button><button type="button" className="quick-tag-edit" title={`编辑 ${shortcut.name}`} disabled={clearing || Boolean(shortcutBusy)} onClick={() => setShortcutEditor({ kind: shortcut.kind, shortcut })}>...</button></div>;
  };

  return <>
    {createPortal(<div className={`head-actions-menu${showMobileActions ? " mobile-open" : ""}`}>
        <button className="head-actions-mobile-toggle" type="button" aria-expanded={showMobileActions} onClick={() => setShowMobileActions((open) => !open)}>操作</button>
        <div className="head-actions">
          <div className="permission-menu">
            <button className={`permission-trigger ${conversation?.permissionMode === "full_control" ? "full" : ""}`} disabled={!!run || clearing || changingPermission} onClick={() => setShowPermissionMenu((open) => !open)}>{permissionLabel}</button>
            {showPermissionMenu && <div className="permission-popover" role="menu">{isCodex ? <><button className={conversation?.permissionMode === "read_only" ? "selected" : ""} onClick={() => void changePermissionMode("read_only")}><b>仅分析</b><span>只读检查，不修改项目。</span></button><button className={conversation?.permissionMode === "workspace_write" ? "selected" : ""} onClick={() => void changePermissionMode("workspace_write")}><b>项目内执行</b><span>可在当前项目范围内读写和执行。</span></button><button className={conversation?.permissionMode === "full_control" ? "selected full" : ""} onClick={() => void changePermissionMode("full_control")}><b>完全控制</b><span>不受沙箱限制，命令直接执行</span></button></> : <><button className={conversation?.permissionMode === "approval_required" ? "selected" : ""} onClick={() => void changePermissionMode("approval_required")}><b>默认权限</b><span>命令执行前需要确认</span></button><button className={conversation?.permissionMode === "full_control" ? "selected full" : ""} onClick={() => void changePermissionMode("full_control")}><b>完全控制</b><span>命令直接执行</span></button></>}</div>}
          </div>
          <button className="secondary" disabled={sending} onClick={openConversationHistory}>历史</button>
          <button className="secondary" disabled={!!run || sending} onClick={openNewConversationParam}>新会话</button>
        </div>
      </div>, document.querySelector('.head-actions-slot') || document.body)}
    {/* 对话面板：快捷方式 + 时间线 + 输入框 */}
    <section className="conversation-canvas">
      <aside className="quick-tag-rail" aria-label="常用操作">
        <div className="quick-actions-row">
          <div className="quick-actions-headings"><div className="quick-tag-heading"><span>常用提示词</span><button type="button" title="新增常用提示词" onClick={() => setShortcutEditor({ kind: "prompt" })}>+</button></div><div className="quick-tag-heading command-tag-heading"><span>常用命令</span><button type="button" title="新增常用命令" onClick={() => setShortcutEditor({ kind: "command_request" })}>+</button></div></div>
          {Array.from({ length: shortcutRowCount }, (_, index) => <div className="quick-tag-row" key={`shortcut-row-${index}`}>{renderShortcutCell(promptShortcutItems[index], "prompt", !promptShortcutItems[index])}{renderShortcutCell(commandShortcutItems[index], "command_request", !commandShortcutItems[index])}</div>)}
        </div>
        <div className="quick-actions-mobile">
          <div className="quick-tag-group"><div className="quick-tag-heading"><span>常用提示词</span><button type="button" title="新增常用提示词" onClick={() => setShortcutEditor({ kind: "prompt" })}>+</button></div><div className="quick-tag-list">{promptShortcutItems.map((shortcut, index) => <div key={shortcut?.id || `empty-prompt-${index}`}>{renderShortcutCell(shortcut, "prompt")}</div>)}</div></div>
          <div className="quick-tag-group command-tags"><div className="quick-tag-heading"><span>常用命令</span><button type="button" title="新增常用命令" onClick={() => setShortcutEditor({ kind: "command_request" })}>+</button></div><div className="quick-tag-list">{commandShortcutItems.map((shortcut, index) => <div key={shortcut?.id || `empty-command-${index}`}>{renderShortcutCell(shortcut, "command_request")}</div>)}</div></div>
        </div>
        <TaskQueue projectID={project.id} conversationID={conversation?.id || ""} permissionMode={conversation?.permissionMode} request={projectApi} fail={fail} dispatchDisabled={clearing || !conversation} onDispatched={handleTaskDispatched} openBoard={(taskID) => navigate(`/projects/${project.id}/tasks${taskID ? `/${taskID}` : ""}`)} />
      </aside>
      <section className="chat-center">
      <section className="timeline" ref={timelineRef} onScroll={onTimelineScroll}>
        <div ref={top} />
        {hasMoreHistory && <button className="secondary load-earlier-history" type="button" disabled={loadingOlderHistory || sending} onClick={() => void loadOlderHistory()}>{loadingOlderHistory ? "加载中" : "加载更早记录"}</button>}
        {timeline.length === 0 && <div className="empty conversation-empty"><h2>开始与 {isCodex ? "Codex" : "Claude Code"} 对话</h2><p>选择常用操作，或直接描述希望工具完成的工作。</p><button className="secondary" type="button" onClick={() => composerRef.current?.querySelector("textarea")?.focus()}>开始输入</button></div>}
        {timeline.map((item) => <div key={item.id} data-user-message-id={item.kind === "message" && item.message.role === "user" ? item.message.id : undefined} ref={item.kind === "message" && item.message.role === "user" ? (element) => { if (element) userMessageElements.current.set(item.message.id, element); else userMessageElements.current.delete(item.message.id); } : undefined}><div className={`timeline-entry ${item.kind}`}>{item.kind === "message" ? <MessageCard message={item.message} agentID={conversation?.agentId || "claude-code"} /> : item.kind === "tool" ? <ToolCard action={item.action} resolving={resolving} decide={decide} /> : <ErrorCard item={item} />}</div>{item.kind === "message" && item.message.role === "user" && item.message.runId && executionByRun.get(item.message.runId) && <AgentExecutionCard execution={executionByRun.get(item.message.runId)!} open={() => openExecutionParam(item.message.runId!)} />}</div>)}
        {agentExecutions.filter((execution) => !anchoredExecutionRunIDs.has(execution.runId)).map((execution) => <AgentExecutionCard key={execution.runId} execution={execution} open={() => openExecutionParam(execution.runId)} />)}
        {run && <div className="run-indicator"><span></span>{runLabel}</div>}
        <div ref={bottom} />
      </section>
      <div className={`scroll-buttons${hasNewContent ? " has-new" : ""}`}>
        <button type="button" className="scroll-btn scroll-to-top" title="回到顶部" aria-label="回到顶部" onClick={scrollToTop}>⇞</button>
        <button type="button" className="scroll-btn scroll-to-previous-message" title="上一条我的消息" aria-label="上一条我的消息" disabled={loadingOlderHistory || (userMessageNavigationIndex <= 0 && !hasMoreMessageHistory)} onClick={goToPreviousUserMessage}>↑</button>
        <button type="button" className="scroll-btn scroll-to-next-message" title="下一条我的消息" aria-label="下一条我的消息" disabled={userMessageNavigationIndex < 0 || userMessageNavigationIndex >= userMessages.length - 1} onClick={goToNextUserMessage}>↓</button>
        <button type="button" className={`scroll-btn scroll-to-bottom${hasNewContent ? " pulse" : ""}`} title="回到底部" aria-label="回到底部" onClick={() => scrollToBottom("auto")}>⇟</button>
      </div>
      <form ref={composerRef} className={`composer${pendingApproval ? " has-approval" : ""}`} onSubmit={(event) => void send(event)}>
        {pendingApproval && <ApprovalBanner action={pendingApproval} resolving={resolving} decide={decide} scrollToCard={() => { const el = timelineRef.current?.querySelector(".timeline-entry.tool .tool-card.waiting"); if (el) el.scrollIntoView({ behavior: "smooth", block: "center" }); }} />}
        <textarea value={text} onChange={(event) => handleTextChange(event.target.value)} onKeyDown={(event) => { if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing) { event.preventDefault(); event.currentTarget.form?.requestSubmit(); return; } navigateInputHistory(event); }} placeholder={`描述希望${isCodex ? " Codex" : " Claude"}在当前项目中完成的工作...`} disabled={sending || clearing || Boolean(shortcutBusy)} />
        <div><ComposerRunnerInfo runnerID={project.runner} agentID={conversation?.agentId || "claude-code"} run={run} runLabel={runLabel} permissionMode={conversation?.permissionMode} usage={usage} displayedModel={displayedModel} contextLabel={contextLabel(usage?.context).replace(/^上下文 /, "")} contextLevel={contextLevel(usage?.context)} onShowUsage={openUsage} stopping={stopping} onStop={() => void stopRun()} /><span className="composer-actions"><button className="secondary" type="button" disabled={Boolean(run) || sending || clearing || Boolean(shortcutBusy)} onClick={clearConversationContext}>清空</button><button className="secondary" type="button" disabled={sending || clearing} onClick={() => void sendContent("继续", false)}>继续</button>{run && <button className="secondary" type="button" disabled={stopping} onClick={() => void stopRun()}>{stopping ? "停止中" : "停止"}</button>}<button className="primary" disabled={!text.trim() || sending || clearing || Boolean(shortcutBusy)}>{sending ? "发送中" : "发送"}</button></span></div>
      </form>
      </section>
    </section>
    {showNewConversation && <NewConversationDialog runnerID={project.runner} close={closeNewConversation} create={newConversation} />}
    {showHistory && <ConversationHistoryDialog conversations={conversationHistory} activeID={conversation?.id || ""} busyID={activatingConversation} close={closeHistory} activate={activateConversation} />}
    {showFullControlConfirmation && <FullControlConfirmationDialog close={() => setShowFullControlConfirmation(false)} confirm={confirmFullControl} changing={changingPermission} isCodex={isCodex} />}
    {showAgentExecution && agentExecutions.find((execution) => execution.runId === showAgentExecution) && <AgentExecutionDialog execution={agentExecutions.find((execution) => execution.runId === showAgentExecution)!} close={closeAgentExecution} />}
    {showUsage && <UsageDialog agentID={conversation?.agentId || "claude-code"} usage={usage} currentRun={currentUsage} now={usageNow} close={closeUsage} />}
    {shortcutEditor && <ShortcutEditor projectID={project.id} state={shortcutEditor} close={() => setShortcutEditor(null)} refresh={refreshShortcuts} fail={fail} />}
    {shortcutVariables && <ShortcutVariablesDialog state={shortcutVariables} close={() => setShortcutVariables(null)} run={(variables) => { setShortcutVariables(null); void runShortcut(shortcutVariables.shortcut, variables, true); }} />}
    {pendingConfirm && createPortal(<ConfirmDialog title={pendingConfirm.title} message={pendingConfirm.message} danger={pendingConfirm.danger} onConfirm={pendingConfirm.onConfirm} onCancel={pendingConfirm.onCancel} />, document.body)}
  </>;
}
