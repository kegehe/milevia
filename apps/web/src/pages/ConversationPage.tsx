// 对话页 — 从 App.tsx Chat 组件提取
// 完整保留了原 Chat 组件的全部功能：消息列表、WebSocket 实时事件、输入框、权限管理、会话切换、所有弹窗

import { FormEvent, memo, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
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
  RunnerInfo, CheckUpdateResult, UpdateResult, SystemItem, SystemVariant, AgentProfile,
} from "../lib/types";
import { api, asRecord } from "../lib/api";
import { createWebSocket } from "../lib/runtime";
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

function requiresForceStop(cause: unknown): boolean {
  return typeof cause === "object" && cause !== null && (cause as { code?: unknown }).code === "active_runs_present";
}

function copyWithLegacyClipboard(content: string) {
  const textarea = document.createElement("textarea");
  textarea.value = content;
  textarea.setAttribute("readonly", "");
  textarea.style.position = "fixed";
  textarea.style.opacity = "0";
  document.body.append(textarea);
  try {
    textarea.select();
    if (!document.execCommand("copy")) throw new Error("clipboard unavailable");
  } finally {
    textarea.remove();
  }
}

async function copyMessageText(content: string) {
  if (navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(content);
      return;
    } catch {
      // Permission policies can reject the modern API while legacy copy works.
    }
  }
  copyWithLegacyClipboard(content);
}

// 合并现有草稿与预约内容：已约内容优先追加到末尾，避免续期时互相覆盖。
function mergeDraftAndPending(draft: string, pending: string) {
  const cleaned = draft.trim();
  return cleaned ? `${cleaned}\n${pending}` : pending;
}

// ---- 子组件 ---------------------------------------------------------------

function PermissionModeIcon() {
  return <svg className="conversation-head-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M12 3.5 19 6v5.2c0 4.2-2.8 7.5-7 9.3-4.2-1.8-7-5.1-7-9.3V6l7-2.5Z" /><path d="M9.2 12.1 11.1 14l3.8-4" /></svg>;
}

function HistoryIcon() {
  return <svg className="conversation-head-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M4.5 12a7.5 7.5 0 1 0 2.2-5.3L4.5 9" /><path d="M4.5 4.8V9h4.2M12 7.8v4.7l3.1 1.8" /></svg>;
}

function NewConversationIcon() {
  return <svg className="conversation-head-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M5 5.5h9.7L19 9.8v8.7a1.8 1.8 0 0 1-1.8 1.8H6.8A1.8 1.8 0 0 1 5 18.5v-13Z" /><path d="M14.5 5.5v4.7H19M12 12v5M9.5 14.5h5" /></svg>;
}

function ScrollNavigationIcon({ direction }: { direction: "top" | "previous" | "next" | "bottom" }) {
  const edge = direction === "top" || direction === "bottom";
  const up = direction === "top" || direction === "previous";
  return <svg className="scroll-btn-icon" viewBox="0 0 24 24" aria-hidden="true">
    {edge && <path d={up ? "M6 5.5h12" : "M6 18.5h12"} />}
    <path d={up ? "M12 18V7M8.5 10.5 12 7l3.5 3.5" : "M12 6v11m-3.5-3.5L12 17l3.5-3.5"} />
  </svg>;
}

function ComposerActionIcon({ action }: { action: "clear" | "continue" | "send" | "schedule" }) {
  if (action === "clear") return <svg className="composer-action-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="m6 7 1 12h10l1-12M9 7V4.5h6V7M4.5 7h15M10 11v4.5M14 11v4.5" /></svg>;
  if (action === "continue") return <svg className="composer-action-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M5 12h12M13 7.5l4.5 4.5-4.5 4.5" /></svg>;
  if (action === "schedule") return <svg className="composer-action-icon" viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="13.2" r="6.2" /><path d="M12 10.5V13l1.8 1.2M9.5 4.5v-2M14.5 4.5v-2M4.8 8.5l-1.5-1.5M12 2.5l1.6 1.6" /></svg>;
  return <svg className="composer-action-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="m4.5 4.5 15 7.2-6.6 2.1-2.1 6.7-6.3-16Z" /><path d="m12.9 13.8 3-3" /></svg>;
}

function ShortcutCategoryIcon({ kind }: { kind: "prompt" | "command" }) {
  return kind === "prompt"
    ? <svg className="quick-tag-heading-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M5.5 6.5A2.5 2.5 0 0 1 8 4h8a2.5 2.5 0 0 1 2.5 2.5v6A2.5 2.5 0 0 1 16 15H11l-3.5 3v-3H8a2.5 2.5 0 0 1-2.5-2.5v-6Z" /><path d="M9 8.5h6M9 11.5h3.5" /></svg>
    : <svg className="quick-tag-heading-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M5.5 5.5h13v13h-13zM8.5 9.5 11 12l-2.5 2.5M13.5 14.5h2" /></svg>;
}

function ShortcutAddIcon() {
  return <svg className="quick-tag-control-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M12 5v14M5 12h14" /></svg>;
}

function ShortcutMoreIcon() {
  return <svg className="quick-tag-control-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M6.5 12h.01M12 12h.01M17.5 12h.01" /></svg>;
}

type SortableShortcutKind = "prompt" | "command_request";

// ShortcutSortableList 渲染某一类别（提示词/命令）快捷方式的竖向排序列表，支持
// HTML5 拖拽排序：拖动时本地预览顺序，落下时通过 onReorder 一次性提交新顺序。
// 使用与后端 reorderShortcuts 匹配的 kind 语义（prompt 列统一归类为 "prompt"）。
function ShortcutSortableList({ items, kind, renderItem, draggingDisabled, onReorder }: {
  items: Shortcut[];
  kind: SortableShortcutKind;
  renderItem: (shortcut: Shortcut | undefined, kind: SortableShortcutKind) => React.ReactNode;
  draggingDisabled: boolean;
  onReorder: (kind: SortableShortcutKind, orderedIDs: string[]) => Promise<void>;
}) {
  const [order, setOrder] = useState<Shortcut[]>(items);
  const [dragID, setDragID] = useState<string | null>(null);
  const [dropTarget, setDropTarget] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  useEffect(() => { setOrder(items); }, [items]);

  const dataKind = kind === "command_request" ? "command_request" : "prompt";

  const onDragStart = (event: React.DragEvent<HTMLLIElement>, id: string) => {
    if (draggingDisabled || saving) { event.preventDefault(); return; }
    setOrder(items);
    setDragID(id);
    setDropTarget(id);
    event.dataTransfer.effectAllowed = "move";
    event.dataTransfer.setData("text/plain", id);
  };

  const onDragOver = (event: React.DragEvent<HTMLLIElement>, overID: string) => {
    event.preventDefault();
    if (!dragID || dragID === overID || !items.some((s) => s.id === dragID)) return;
    event.dataTransfer.dropEffect = "move";
    setDropTarget(overID);
    setOrder((prev) => {
      const from = prev.findIndex((s) => s.id === dragID);
      const to = prev.findIndex((s) => s.id === overID);
      if (from === -1 || to === -1 || from === to) return prev;
      const next = [...prev];
      const [moved] = next.splice(from, 1);
      next.splice(to, 0, moved);
      return next;
    });
  };

  const commit = () => {
    if (!dragID) return;
    // 顺序未发生变化（拖回原位）时不需要调用后端。
    const sameOrder = order.length === items.length && order.every((s, index) => s.id === items[index].id);
    setDragID(null);
    setDropTarget(null);
    if (sameOrder) return;
    setSaving(true);
    const orderedIDs = order.map((s) => s.id);
    void onReorder(dataKind, orderedIDs)
      .catch(() => { /* 失败时父组件负责回滚 refresh */ })
      .finally(() => setSaving(false));
  };

  const onDrop = (event: React.DragEvent<HTMLElement>) => {
    event.preventDefault();
    commit();
  };

  if (items.length === 0) {
    return <div className="quick-tag-list">{renderItem(undefined, kind)}</div>;
  }

  return <ul className="quick-tag-list sortable" onDragOver={(e) => e.preventDefault()} onDrop={onDrop} onDragEnd={() => { setDragID(null); setDropTarget(null); }}>
    {order.map((shortcut) => (
      <li key={shortcut.id}
          className={`quick-tag-item${dragID === shortcut.id ? " dragging" : dragID && dropTarget === shortcut.id ? " drop-over" : ""}`}
          data-id={shortcut.id}
          draggable={!draggingDisabled && !saving}
          onDragStart={(e) => onDragStart(e, shortcut.id)}
          onDragOver={(e) => onDragOver(e, shortcut.id)}
          onDragEnd={() => { setDragID(null); setDropTarget(null); }}>
        {renderItem(shortcut, kind)}
      </li>
    ))}
  </ul>;
}

function DialogCloseIcon() {
  return <svg className="shortcut-editor-close-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="m7 7 10 10M17 7 7 17" /></svg>;
}

function ConversationClearIcon({ running = false }: { running?: boolean }) {
  return running
    ? <svg className="conversation-clear-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M7 5.5h10M9.5 5.5v-2h5v2M6.5 8l.8 10.5h9.4L17.5 8M10 11.5v4M14 11.5v4" /><path d="M4.5 12.5 2.8 10.8 4.5 9.1" /></svg>
    : <svg className="conversation-clear-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M7 5.5h10M9.5 5.5v-2h5v2M6.5 8l.8 10.5h9.4L17.5 8M10 11.5v4M14 11.5v4" /></svg>;
}

function ComposerRunnerInfo({ runnerID, agentID, run, runLabel, permissionMode, usage, displayedModel, contextLabel, contextLevel, onShowUsage, stopping, onStop }: { runnerID: string; agentID: AgentID; run: string; runLabel: string; permissionMode?: string; usage: ConversationUsageResponse | null; displayedModel: string; contextLabel: string; contextLevel: string; onShowUsage: () => void; stopping: boolean; onStop: () => void }) {
  const [runner, setRunner] = useState<RunnerInfo | null>(null);
  const [checking, setChecking] = useState(false);
  const [updateInfo, setUpdateInfo] = useState<CheckUpdateResult | null>(null);
  const [updating, setUpdating] = useState(false);
  const [updateError, setUpdateError] = useState<string | null>(null);
  const [showConfirm, setShowConfirm] = useState(false);
  const tool = agentID === "codex" ? runner?.codex : runner?.claude;
  const toolName = agentID === "codex" ? "Codex" : "Claude Code";
  const agentPath = agentID === "codex" ? "codex" : "claude";

  // 自动检查的缓存键：按 runner + agent 隔离，10 分钟内不重复请求 npm。
  const cacheKey = `milevia:update-check:${runnerID}:${agentID}`;
  const autoChecked = useRef(false);
  // 切换 runner 或 agent 时重置自动检查守卫，确保每个工具都会被检查一次。
  useEffect(() => { autoChecked.current = false; }, [cacheKey]);

  const refreshRunner = useCallback(async (): Promise<RunnerInfo | null> => {
    try {
      const list = await api<RunnerInfo[]>("/api/runners");
      const match = list.find((r) => r.id === runnerID);
      if (match) {
        setRunner(match);
        return match;
      }
    } catch {
      /* ignore — runner info is best-effort */
    }
    return null;
  }, [runnerID]);

  useEffect(() => {
    void refreshRunner();
  }, [refreshRunner]);

  const handleCheckUpdate = useCallback(async (silent = false): Promise<void> => {
    if (!silent) setChecking(true);
    // 手动检查先清空旧结果以便 UI 反映"检查中"；静默检查保留旧结果，避免后台失败时抹掉已有结论。
    if (!silent) setUpdateInfo(null);
    try {
      const result = await api<CheckUpdateResult>(`/api/runners/${runnerID}/${agentPath}/check-update`, { method: "POST" });
      setUpdateInfo(result);
      // 记录检查时间戳，供自动检查的缓存窗口使用。
      try { window.localStorage.setItem(cacheKey, String(Date.now())); } catch { /* localStorage 可能在隐私模式下不可用 */ }
    } catch (err) {
      if (!silent) setUpdateInfo({ updateAvailable: false, currentVersion: tool?.version || "", error: err instanceof Error ? err.message : "检查更新失败" });
    } finally {
      if (!silent) setChecking(false);
    }
  }, [runnerID, agentPath, cacheKey, tool?.version]);

  // 主动检查：runner 就绪后自动检查一次（受 10 分钟缓存窗口约束），之后每 30 分钟静默复查。
  useEffect(() => {
    if (!runner || tool?.status !== "ready" || updating) return;
    // 已有运行中的对话时不自动检查，避免分散注意力；用户仍可手动检查。
    if (run) return;
    if (autoChecked.current) return;
    autoChecked.current = true;
    let cached = false;
    try {
      const stamp = window.localStorage.getItem(cacheKey);
      if (stamp && Date.now() - Number(stamp) < 10 * 60 * 1000) cached = true;
    } catch { /* localStorage 不可用则直接检查 */ }
    if (!cached) void handleCheckUpdate(true);
  }, [runner, tool?.status, updating, run, cacheKey, handleCheckUpdate]);

  // 定时轮询：每 30 分钟静默复查一次（仅在无运行中对话时）。
  useEffect(() => {
    if (!runner || tool?.status !== "ready" || run || updating) return;
    const id = window.setInterval(() => { void handleCheckUpdate(true); }, 30 * 60 * 1000);
    return () => window.clearInterval(id);
  }, [runner, tool?.status, run, updating, handleCheckUpdate]);

  // 当后端报告 runner 正在更新（如页面刷新后检测到），定期轮询直到状态恢复
  useEffect(() => {
    if (!runner || updating || tool?.status !== "updating") return;
    const id = window.setInterval(() => { void refreshRunner(); }, 5_000);
    return () => window.clearInterval(id);
  }, [runner, tool?.status, updating, refreshRunner]);

  const handleUpdate = async () => {
    setShowConfirm(false);
    setUpdating(true);
    // 立即将状态置为 updating，触发底部"更新中..."脉冲动画
    setRunner((prev) => !prev ? prev : agentID === "codex" ? prev.codex ? { ...prev, codex: { ...prev.codex, status: "updating" as const } } : prev : { ...prev, claude: { ...prev.claude, status: "updating" as const } });
    try {
      const result = await api<UpdateResult>(`/api/runners/${runnerID}/${agentPath}/update`, { method: "POST" });
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
      // 重置自动检查守卫并清除缓存时间戳：runner 状态恢复 ready 后，主动检查 effect 会
      // 自动复查一次（缓存已失效，必然执行），立即展示"已是最新版本"。
      autoChecked.current = false;
      try { window.localStorage.removeItem(cacheKey); } catch { /* localStorage 不可用则忽略 */ }
    }
  };

  const runnerStatusClass = run ? "running" : tool?.status || "unavailable";

  const canShowUsage = Boolean(run) || tool?.status === "ready";
  return (<>
    <span className="composer-status-group">
      <span className={`runner-inline ${runnerStatusClass}${run ? " run-active" : ""}`} title={tool?.reason} role={run ? "status" : undefined} aria-live={run ? "polite" : undefined}><i aria-hidden="true"></i><span>{run ? runLabel : tool?.status === "ready" ? `${toolName} ${tool.version}` : tool?.status === "updating" ? "更新中..." : tool?.reason || `${toolName} 不可用`}</span></span>
      {!run && tool?.status === "ready" && <button className="runner-inline-btn" disabled={checking || updating} onClick={() => void handleCheckUpdate()}>{checking ? "检查中..." : "检查更新"}</button>}
      {!run && !updating && updateInfo?.updateAvailable && !updateInfo.error && <button className="runner-inline-btn update-available" onClick={() => setShowConfirm(true)}>更新至 {updateInfo.latestVersion}</button>}
      {!run && !updating && updateInfo && !updateInfo.updateAvailable && !updateInfo.error && <span className="runner-inline-uptodate" title={`${toolName} 已是最新版本`}>已是最新版本</span>}
      {!run && !updating && updateInfo?.error && <span className="runner-inline-error" title={updateInfo.error}>{updateInfo.error}</span>}
      {canShowUsage ? <span className="composer-usage"><span className="composer-usage-model" title={displayedModel}>{displayedModel}</span><span className={`composer-usage-context ${contextLevel}`}>{contextLabel}</span><span className="composer-usage-count">{usage ? `${usage.session.taskCount} 次对话` : "加载中"}</span><button className="usage-trigger" type="button" onClick={onShowUsage}>使用状态</button></span> : <span className="composer-usage pending">用量将在工具就绪后显示</span>}
    </span>
    {showConfirm && <div className="backdrop" role="dialog" aria-modal="true"><section className="modal"><header><div><label>更新 {toolName}</label><h2>确认更新 {toolName}</h2></div><button title="关闭" onClick={() => setShowConfirm(false)}>x</button></header><p className="permission-confirmation">当前版本：<b>{tool?.version}</b> → 最新版本：<b>{updateInfo?.latestVersion}</b>。更新期间将无法使用 AI 对话功能，更新预计需要数十秒。</p><footer><button className="secondary" onClick={() => setShowConfirm(false)}>取消</button><button className="primary" onClick={() => void handleUpdate()}>确认更新</button></footer></section></div>}
    {updateError && <div className="backdrop" role="dialog" aria-modal="true"><section className="modal update-error-dialog"><header><div><label>更新错误</label><h2>更新失败</h2></div><button title="关闭" onClick={() => setUpdateError(null)}>x</button></header><div className="update-error-reason" role="alert">{updateError}</div><footer><button className="secondary" onClick={() => setUpdateError(null)}>关闭</button></footer></section></div>}
  </>);
}

function UsageDialogIcon() {
  return <svg className="usage-dialog-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M5 19V10M10 19V5M15 19v-7M20 19V8" /><path d="M3.5 19.5h17" /></svg>;
}

function UsageDialogHeader({ agentName, close }: { agentName: string; close: () => void }) {
  return <header><div className="usage-dialog-heading"><span className="usage-dialog-mark"><UsageDialogIcon /></span><div><label>{agentName.toUpperCase()} USAGE</label><h2 id="usage-title">使用状态</h2></div></div><button type="button" className="usage-dialog-close" title="关闭" aria-label="关闭" onClick={close}><DialogCloseIcon /></button></header>;
}

function UsageDialog({ agentID, usage, currentRun, close }: { agentID: AgentID; usage: ConversationUsageResponse | null; currentRun: RunUsage | undefined; close: () => void }) {
  const agentName = agentID === "codex" ? "Codex" : "Claude Code";
  // 计时器仅在本组件挂载时（即弹窗打开时）运行，避免在对话页顶层每秒触发整页重渲染。
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, []);
  if (usage && !usage.available) return <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="usage-title"><section className="modal usage-dialog"><UsageDialogHeader agentName={agentName} close={close} /><div className="usage-body usage-unavailable"><p className="usage-note">{usage.reason || "当前工具未提供可验证的使用统计。"}</p></div><footer><button className="secondary" onClick={close}>关闭</button></footer></section></div>;
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
  const context = usage?.context;
  const contextWindow = context?.contextWindow ?? 0;
  const contextTokens = context?.contextInputTokens ?? 0;
  const contextPercent = contextWindow > 0 ? Math.min(100, Math.round(contextTokens / contextWindow * 100)) : 0;
  const contextDetail = contextTokens && contextWindow ? `${formatTokens(contextTokens)} / ${formatTokens(contextWindow)}` : contextTokens ? `已用 ${formatTokens(contextTokens)}` : context?.available === false ? context.reason || "当前工具未提供上下文快照" : context?.hasResult ? "当前工具未提供上下文快照" : "等待上下文快照";
  return <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="usage-title"><section className="modal usage-dialog"><UsageDialogHeader agentName={agentName} close={close} /><div className="usage-body">
    <section className="usage-context-overview"><div><span>当前会话上下文</span><b>{contextDetail}</b></div><span className={`context-state ${contextLevel(context)}`}>{contextLabel(context)}</span>{contextWindow > 0 && <div className={`usage-context-meter ${contextLevel(context)}`} aria-label={`上下文使用 ${contextPercent}%`}><i style={{ width: `${contextPercent}%` }} /></div>}</section>
    <section className="usage-section"><div className="usage-section-head"><h3>当前任务</h3>{task?.model && <span className="usage-model-label" title={task.model}>{task.model}</span>}</div>{task ? <><dl className="usage-grid task-usage-grid">{metrics.map(([label, value]) => <div key={String(label)} className={label === "状态" ? `usage-metric-status ${active ? "running" : task.status}` : ""}><dt>{label}</dt><dd>{value}</dd></div>)}</dl>{!hasTaskUsage && !active && <p className="usage-note">{task.reason || `该任务未获得 ${agentName} 的最终统计数据。`}</p>}</> : <p className="usage-note">当前会话还没有可用的任务统计。</p>}</section>
    {session && <section className="usage-section"><div className="usage-section-head"><h3>当前会话</h3><span>{session.taskCount} 次任务</span></div><dl className="usage-grid session-grid"><div><dt>Agent 轮次</dt><dd>{session.agentTurns}</dd></div><div><dt>模型步骤</dt><dd>{session.modelSteps}</dd></div><div><dt>工具调用</dt><dd>{session.toolCalls}</dd></div><div><dt>输入 / 输出</dt><dd>{formatTokens(session.inputTokens)} / {formatTokens(session.outputTokens)}</dd></div><div><dt>缓存读取 / 创建</dt><dd>{formatTokens(session.cacheReadTokens)} / {formatTokens(session.cacheCreationTokens)}</dd></div><div><dt>费用估算</dt><dd>{formatCost(session.estimatedCostUsd)}</dd></div></dl></section>}
    {usage?.models.length ? <section className="usage-section"><div className="usage-section-head"><h3>模型用量</h3><span>包含子代理</span></div><div className="model-usage-list">{usage.models.map((model) => <div key={model.model}><b>{model.model}</b><span>{formatTokens(model.inputTokens)} 输入 · {formatTokens(model.outputTokens)} 输出</span><em>{formatCost(model.estimatedCostUsd)}</em></div>)}</div></section> : null}
    <p className="usage-disclaimer">费用为客户端事件估算值，不代表账单金额。</p>
  </div><footer><button className="secondary" onClick={close}>关闭</button></footer></section></div>;
}

function HistorySearchIcon() {
  return <svg className="history-search-icon" viewBox="0 0 24 24" aria-hidden="true"><circle cx="10.5" cy="10.5" r="5.5" /><path d="m15 15 4.2 4.2" /></svg>;
}

type ConversationHistoryPage = { items: Conversation[]; nextCursor: string };

function ConversationHistoryDialog({ conversations, activeID, busyID, close, activate, search, hasMore, loadingMore, loadMore }: { conversations: Conversation[]; activeID: string; busyID: string; close: () => void; activate: (item: Conversation) => Promise<void>; search: (query: string) => void; hasMore: boolean; loadingMore: boolean; loadMore: () => void }) {
  const [query, setQuery] = useState("");
  const [selected, setSelected] = useState(0);
  useEffect(() => {
    const timer = window.setTimeout(() => search(query), 200);
    return () => window.clearTimeout(timer);
  }, [query, search]);
  useEffect(() => { setSelected(0); }, [conversations, query]);
  const select = (item: Conversation) => { if (item.id !== activeID && item.status !== "running" && !busyID) void activate(item); };
  const keyDown = (event: React.KeyboardEvent<HTMLInputElement>) => {
    if (event.key === "Escape") { event.preventDefault(); close(); return; }
    if (event.key === "ArrowDown") { event.preventDefault(); setSelected((index) => Math.min(conversations.length - 1, index + 1)); return; }
    if (event.key === "ArrowUp") { event.preventDefault(); setSelected((index) => Math.max(0, index - 1)); return; }
    if (event.key === "Enter" && conversations[selected]) { event.preventDefault(); select(conversations[selected]); }
  };
  return <div className="backdrop history-backdrop" role="dialog" aria-modal="true" aria-labelledby="conversation-history-title"><section className="modal conversation-history"><header><div className="conversation-history-heading"><span className="conversation-history-mark"><HistoryIcon /></span><div><h2 id="conversation-history-title">会话历史</h2></div></div><button type="button" className="conversation-history-close" title="关闭" aria-label="关闭" onClick={close}><DialogCloseIcon /></button></header><div className="history-toolbar"><label className="history-search"><HistorySearchIcon /><input autoFocus value={query} onChange={(event) => setQuery(event.target.value)} onKeyDown={keyDown} placeholder="搜索会话标题或内容" /></label><span>{query ? `${conversations.length} 个匹配` : `${conversations.length} 个会话`}</span></div><div className="history-list">{conversations.length === 0 ? <p className="history-empty">没有匹配的会话</p> : conversations.map((item, index) => {
    const runningElsewhere = item.status === "running" && item.id !== activeID;
    const state = busyID === item.id ? "切换中" : item.id === activeID ? "当前会话" : runningElsewhere ? "运行中" : "";
    return <button key={item.id} className={`history-item ${busyID === item.id ? "activating" : ""} ${item.id === activeID ? "active" : ""} ${index === selected ? "selected" : ""} ${runningElsewhere ? "running" : ""}`} disabled={Boolean(busyID) || runningElsewhere} onMouseEnter={() => setSelected(index)} onClick={() => select(item)}><span className="history-item-main"><span className="history-item-title"><b>{item.title || "新会话"}</b><span className={`history-item-agent ${item.agentId === "codex" ? "codex" : "claude"}`}>{item.agentId === "codex" ? "Codex" : "Claude Code"}</span></span><small>{item.preview || "尚未发送消息"}</small></span><span className="history-item-meta">{state && <em>{state}</em>}<time>{formatHistoryTime(item.lastActivityAt)}</time></span></button>;
  })}</div>{hasMore && <button className="secondary load-earlier-history" type="button" disabled={loadingMore} onClick={loadMore}>{loadingMore ? "加载中" : "加载更多会话"}</button>}<footer><span>{busyID ? "正在切换会话" : `${conversations.length} 条记录`}</span><button className="secondary" type="button" onClick={close}>关闭</button></footer></section></div>;
}

function NewConversationDialog({ runnerID, close, create }: { runnerID: string; close: () => void; create: (agentId: AgentID, permissionMode: PermissionMode, profileID?: string) => Promise<void> }) {
  const [agentId, setAgentId] = useState<AgentID>("claude-code");
  const [permissionMode, setPermissionMode] = useState<PermissionMode>("full_control");
  const [creating, setCreating] = useState(false);
  const [runner, setRunner] = useState<RunnerInfo | null>(null);
  const [profiles, setProfiles] = useState<AgentProfile[]>([]);
  const [profileID, setProfileID] = useState("");
  const [profilesSupported, setProfilesSupported] = useState(false);
  const navigate = useNavigate();
  useEffect(() => { api<RunnerInfo[]>("/api/runners").then((items) => setRunner(items.find((item) => item.id === runnerID) || null)).catch(() => setRunner(null)); }, [runnerID]);
  useEffect(() => { api<AgentProfile[]>(`/api/runners/${runnerID}/agent-profiles`).then((items) => { setProfiles(items); setProfilesSupported(true); }).catch(() => { setProfiles([]); setProfilesSupported(false); }); }, [runnerID]);
  const availableProfiles = useMemo(() => profiles.filter((profile) => profile.agentId === agentId && profile.enabled && profile.state === "active" && profile.authMode === "cli_managed"), [agentId, profiles]);
  useEffect(() => {
    if (availableProfiles.some((profile) => profile.id === profileID)) return;
    setProfileID("");
  }, [availableProfiles, profileID]);
  const selectAgent = (next: AgentID) => { setAgentId(next); setProfileID(""); setPermissionMode(next === "codex" ? "workspace_write" : "full_control"); };
  const codexStatus = runner?.codex;
  const codexReady = codexStatus?.status === "ready";
	const codexAvailable = codexReady;
  const submit = async () => { if (agentId === "codex" && !codexAvailable) return; setCreating(true); try { await create(agentId, permissionMode, profileID || undefined); } finally { setCreating(false); } };
  const codex = agentId === "codex";
  return <div className="backdrop" role="dialog" aria-modal="true" onClick={(e) => { if (e.target === e.currentTarget) close(); }}><section className="modal permission-dialog"><header><div><h2>新会话</h2></div><button title="关闭" onClick={close}>x</button></header><div className="permission-options"><button className={!codex ? "active" : ""} onClick={() => selectAgent("claude-code")}><b>Claude Code</b></button><button className={codex ? "active" : ""} disabled={Boolean(runner) && !codexAvailable} title={codexStatus?.reason} onClick={() => selectAgent("codex")}><b>Codex</b></button></div>{availableProfiles.length > 0 && <label className="conversation-profile-select"><span>配置档案</span><select value={profileID} onChange={(event) => setProfileID(event.target.value)}><option value="">使用原有 CLI 配置</option>{availableProfiles.map((profile) => <option key={profile.id} value={profile.id}>{profile.name}{profile.model ? ` (${profile.model})` : ""}</option>)}</select></label>}{profilesSupported && <button className="conversation-profile-manage" type="button" onClick={() => navigate("/agent-profiles")}>管理配置档案</button>}<div className="permission-options">{codex ? <><button className={permissionMode === "read_only" ? "active" : ""} onClick={() => setPermissionMode("read_only")}><b>仅分析</b><span>只读检查，不修改项目。</span></button><button className={permissionMode === "workspace_write" ? "active" : ""} onClick={() => setPermissionMode("workspace_write")}><b>项目内执行</b><span>可在项目范围内读写和执行。</span></button><button className={permissionMode === "full_control" ? "active" : ""} onClick={() => setPermissionMode("full_control")}><b>完全控制</b><span>Codex 可直接执行命令，不受沙箱限制。</span></button></> : <><button className={permissionMode === "approval_required" ? "active" : ""} onClick={() => setPermissionMode("approval_required")}><b>默认权限</b><span>每条终端命令执行前等待确认。</span></button><button className={permissionMode === "full_control" ? "active" : ""} onClick={() => setPermissionMode("full_control")}><b>完全控制</b><span>Claude 可直接执行命令，不会等待确认。</span></button></>}</div><footer><button className="secondary" onClick={close}>取消</button><button className="primary" disabled={creating || (codex && !codexAvailable)} onClick={() => void submit()}>{creating ? "创建中" : "创建会话"}</button></footer></section></div>;
}

function FullControlConfirmationDialog({ close, confirm, changing, isCodex }: { close: () => void; confirm: () => Promise<void>; changing: boolean; isCodex?: boolean }) {
  const agentName = isCodex ? "Codex" : "Claude";
  return <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="full-control-title" onClick={(e) => { if (e.target === e.currentTarget) close(); }}><section className="modal permission-dialog"><header><div><label>{agentName.toUpperCase()} PERMISSION</label><h2 id="full-control-title">切换为完全控制</h2></div><button title="关闭" disabled={changing} onClick={close}>x</button></header><p className="permission-confirmation">{isCodex ? `完全控制会允许 Codex 绕过沙箱限制，直接执行所有命令，不再受项目目录约束。` : `完全控制会允许 Claude 在当前项目中直接执行所有命令，不再等待确认。`}</p><footer><button className="secondary" disabled={changing} onClick={close}>取消</button><button className="primary danger" disabled={changing} onClick={() => void confirm()}>{changing ? "切换中" : "确认切换"}</button></footer></section></div>;
}

const MessageCard = memo(function MessageCard({ message, agentID, fail }: { message: Message; agentID: AgentID; fail: (message: string) => void }) {
  const isUser = message.role === "user";
  const agentName = agentID === "codex" ? "Codex" : "Claude";
  const [copied, setCopied] = useState(false);
  const copiedTimer = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(() => () => {
    if (copiedTimer.current) window.clearTimeout(copiedTimer.current);
  }, []);

  const copy = async () => {
    try {
      await copyMessageText(message.content);
      setCopied(true);
      if (copiedTimer.current) window.clearTimeout(copiedTimer.current);
      copiedTimer.current = window.setTimeout(() => setCopied(false), 1_500);
    } catch {
      fail("无法复制消息，请检查浏览器剪贴板权限。");
    }
  };

  return <article className={`message ${message.role}`}><header><span className="message-avatar">{isUser ? "你" : agentID === "codex" ? "<>" : "C"}</span><b>{isUser ? "你" : agentName}</b><time>{formatTime(message.createdAt)}</time></header><div className="markdown"><Markdown content={message.content} /></div>{isUser && <button className={`message-copy${copied ? " copied" : ""}`} type="button" title={copied ? "已复制" : "复制消息"} aria-label={copied ? "已复制消息" : "复制消息"} onClick={() => void copy()} />}</article>;
});

const SystemCard = memo(function SystemCard({ system }: { system: SystemItem }) {
  const icon: Record<SystemVariant, string> = { compact: "◐", compact_result: "✓", compact_boundary: "≡", api_retry: "↻", task: "▸" };
  const variantClass = system.variant === "compact_result" && system.detail ? "compact_result failed" : system.variant;
  // task 卡片按 metadata.state 追加成功/失败/中性后缀，失败换成警告图标。
  const state = system.metadata?.state;
  const taskState = system.variant === "task" ? (state === "failed" || state === "success" ? state : "info") : null;
  return <article className={taskState ? `system-card task ${taskState}` : `system-card ${variantClass}`}><header><span className="system-card-icon" aria-hidden="true">{taskState === "failed" ? "!" : icon[system.variant]}</span><div><b>{system.title}</b>{system.detail && <small>{system.detail}</small>}</div></header><time className="system-card-time">{formatTime(system.createdAt)}</time></article>;
});

const ErrorCard = memo(function ErrorCard({ item, projectId, onViewTask }: { item: TimelineItem & { kind: "error" }; projectId: string; onViewTask: (taskId: string) => void }) {
  return <article className="error-card"><header><span className="error-card-icon">!</span><div><b>{item.title}</b><time>{formatTime(item.createdAt)}</time></div></header><pre>{item.detail}</pre>{item.taskId && <footer className="error-card-footer"><button type="button" className="secondary" onClick={() => onViewTask(item.taskId!)}>查看任务详情</button></footer>}</article>;
});

function MarkdownImage({ src, alt }: { src?: string; alt?: string }) {
  const label = alt?.trim() || "未命名图片";
  const externalImage = src && /^https:\/\//i.test(src) ? src : "";
  return <span className="markdown-image-reference" role="note">图片：{externalImage ? <a href={externalImage} target="_blank" rel="noreferrer">{label}</a> : label}</span>;
}

const Markdown = memo(function Markdown({ content }: { content: string }) {
  return <ReactMarkdown remarkPlugins={[remarkGfm]} skipHtml components={{ a: ({ href, children }) => <a href={href} target="_blank" rel="noreferrer">{children}</a>, img: MarkdownImage }}>{content}</ReactMarkdown>;
});

const ToolCard = memo(function ToolCard({ action, resolving, decide }: { action: ToolAction; resolving: string; decide: (approvalId: string, decision: "allow" | "deny") => Promise<void> }) {
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
  const isFileChange = action.name === "文件修改";
  const outputLabel = failed ? isFileChange ? "查看文件变更" : "查看错误输出" : isFileChange ? "查看文件内容" : "查看命令输出";
  const outputPreview = output.replace(/\s+/g, " ").trim().slice(0, 180);
  const statusClass = failed ? "failed" : stopped || denied ? "denied" : waiting ? "pending" : "";
  return <article className={`tool-card ${waiting ? "waiting" : ""}`}><header><div><span className="tool-icon" aria-hidden="true">{">_"}</span><div><b>{action.name === "Bash" ? "终端命令" : action.name}</b><small>{description}</small></div></div><div className="tool-meta"><time>{formatTime(action.createdAt)}</time><span className={`tool-status ${statusClass}`}>{status}</span></div></header>{command && <pre className="command"><code>{command}</code></pre>}{waiting && approval && <div className="approval-actions"><span>此命令将会在当前项目目录执行。</span><div><button className="secondary" disabled={resolving === approval.approvalId} onClick={() => void decide(approval.approvalId, "deny")}>拒绝</button><button className="primary" disabled={resolving === approval.approvalId} onClick={() => void decide(approval.approvalId, "allow")}>{resolving === approval.approvalId ? "处理中" : "允许执行"}</button></div></div>}{action.output && (shouldCollapseOutput ? <details><summary>{outputLabel}<span>{outputPreview}</span></summary><pre className="output">{output}</pre></details> : <pre className={`output inline ${failed ? "error-output" : ""}`}>{output}</pre>)}</article>;
});

function AgentExecutionCard({ execution, open }: { execution: AgentExecution; open: () => void }) {
  const agents = flattenAgents(execution.agents);
  const counts = agents.reduce<Record<AgentStatus, number>>((value, agent) => { value[agent.status]++; return value; }, { pending: 0, running: 0, completed: 0, failed: 0, stopped: 0, unresolved: 0 });
  const state = execution.incomplete ? "结果未收齐" : execution.status === "completed" ? "已完成" : execution.status === "failed" ? "执行失败" : ["stopped", "interrupted", "cancelled"].includes(execution.status) ? "已停止" : "执行中";
  const lastActivity = agents.flatMap((agent) => agent.logs).reduce<AgentLog | undefined>((latest, log) => !latest || new Date(log.createdAt).getTime() > new Date(latest.createdAt).getTime() ? log : latest, undefined);
  return <section className={`agent-execution-card ${execution.incomplete ? "incomplete" : execution.status}`} aria-label="子代理执行过程"><header><div><span className="agent-execution-icon">A</span><div><b>子代理执行过程</b><small>{state}{counts.running ? ` · ${counts.running} 个执行中` : ""}</small></div></div><button className="secondary" onClick={open}>查看过程</button></header><div className="agent-execution-summary"><span>{agents.length} 个子代理</span><span>{counts.completed} 已完成</span>{counts.stopped > 0 && <span>{counts.stopped} 已停止</span>}{counts.failed > 0 && <span className="failed">{counts.failed} 失败</span>}{counts.unresolved > 0 && <span className="incomplete">{counts.unresolved} 未收齐</span>}</div>{execution.incomplete && <p className="agent-execution-warning">主回合已结束，但没有收到全部子代理的最终结果。已收到的过程记录仍可查看。</p>}{lastActivity && <p className="agent-execution-last"><b>{lastActivity.title}</b><span>{lastActivity.detail.replace(/\s+/g, " ")}</span></p>}</section>;
}

function AgentExecutionDialog({ execution, close }: { execution: AgentExecution; close: () => void }) {
  const agents = flattenAgents(execution.agents);
  const counts = agents.reduce<Record<AgentStatus, number>>((value, agent) => { value[agent.status]++; return value; }, { pending: 0, running: 0, completed: 0, failed: 0, stopped: 0, unresolved: 0 });
  const [selectedID, setSelectedID] = useState(agents[0]?.id || "");
  const selected = agents.find((agent) => agent.id === selectedID) || agents[0];
  return <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="agent-execution-title" onClick={(e) => { if (e.target === e.currentTarget) close(); }}><section className="modal agent-execution-dialog"><header><div><h2 id="agent-execution-title">子代理过程</h2><p>{execution.incomplete ? "主回合已完成，但部分子代理的最终结果未送达。" : "查看每个子代理已收到的输出、工具调用和结果。"}</p></div><button title="关闭" onClick={close}>x</button></header><div className="agent-execution-summary-bar">{(["running", "completed", "failed", "stopped", "unresolved"] as AgentStatus[]).map((s) => { const c = counts[s]; if (!c) return null; return <span key={s} className={`summary-chip ${s}`}><span className="dot"></span>{agentStatusLabel(s)}: {c}</span>; })}</div><div className="agent-execution-body"><nav className="agent-tree" aria-label="子代理列表">{execution.agents.map((agent) => <AgentTree key={agent.id} agent={agent} selectedID={selectedID} select={setSelectedID} depth={0} />)}</nav><section className="agent-log-panel">{selected ? <><header><div><span className={`agent-status ${selected.status}`}>{agentStatusLabel(selected.status)}</span><h3>{selected.summary}</h3></div><small>{formatTime(selected.createdAt)}</small></header><div className="agent-log-list">{selected.logs.length === 0 ? <p className="agent-log-empty">尚未收到该子代理的过程输出。</p> : selected.logs.map((log) => <article className={`agent-log ${log.kind} ${log.isError ? "failed" : ""}`} key={log.id}><header><span className={`log-kind-badge ${log.kind}`}>{log.kind === "text" ? "文本" : log.kind === "tool" ? "工具" : log.kind === "result" ? "结果" : "错误"}</span><b>{log.title}</b><time>{formatTime(log.createdAt)}</time></header><details open={log.kind === "text"}><summary>{log.detail.replace(/\s+/g, " ").slice(0, 180) || "无输出"}</summary><pre>{log.detail || "(无输出)"}</pre></details></article>)}</div></> : <p className="agent-log-empty">没有可查看的子代理。</p>}</section></div><footer><span>{agents.length} 个子代理 · {counts.completed} 完成{counts.failed > 0 ? ` · ${counts.failed} 失败` : ""}{counts.running > 0 ? ` · ${counts.running} 执行中` : ""}</span><button className="secondary" type="button" onClick={close}>关闭</button></footer></section></div>;
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
  return <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="shortcut-variables-title"><section className="modal shortcut-variables-dialog"><header><div><h2 id="shortcut-variables-title">{state.shortcut.name}</h2></div><button title="关闭" onClick={close}>x</button></header><form onSubmit={submit}><div className="shortcut-editor-body">{required.includes("selection") && <label>选中内容<textarea autoFocus required value={variables.selection || ""} onChange={(event) => setVariables((old) => ({ ...old, selection: event.target.value }))} placeholder="粘贴需要处理的内容" /></label>}{required.includes("error") && <label>错误信息<textarea autoFocus={!required.includes("selection")} required value={variables.error || ""} onChange={(event) => setVariables((old) => ({ ...old, error: event.target.value }))} placeholder="粘贴报错信息" /></label>}</div><footer><button type="button" className="secondary" onClick={close}>取消</button><button className="primary">发送</button></footer></form></section></div>;
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
    } catch (cause) { fail(cause instanceof Error ? cause.message : "无法保存快捷任务"); }
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
        } catch (cause) { fail(cause instanceof Error ? cause.message : "无法删除快捷任务"); }
        finally { setBusy(false); }
      })(),
      onCancel: () => setPendingConfirm(null),
    });
  };

  return <><div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="shortcut-editor-title"><section className={`modal shortcut-editor${isCommand ? " command-editor" : " prompt-editor"}`}><header><div className="shortcut-editor-heading"><span className="shortcut-editor-mark"><ShortcutCategoryIcon kind={isCommand ? "command" : "prompt"} /></span><div><label>{isCommand ? "COMMON COMMAND" : "COMMON PROMPT"}</label><h2 id="shortcut-editor-title">{shortcut ? `编辑${isCommand ? "命令" : "提示词"}` : `新增${isCommand ? "命令" : "提示词"}`}</h2></div></div><button type="button" className="shortcut-editor-close" title="关闭" aria-label="关闭" disabled={busy} onClick={close}><DialogCloseIcon /></button></header><form onSubmit={(event) => void save(event)}><div className="shortcut-editor-body"><label className="shortcut-editor-field"><span>名称 <small>最多 64 个字符</small></span><input autoFocus required maxLength={64} value={name} onChange={(event) => setName(event.target.value)} placeholder={isCommand ? "例如：清空终端" : "例如：审查当前改动"} /></label><label className="shortcut-editor-field"><span>{isCommand ? "命令内容" : "提示词内容"}</span><textarea required maxLength={12000} value={template} onChange={(event) => setTemplate(event.target.value)} placeholder={isCommand ? "例如：/compact" : "描述希望 Claude 在当前项目完成的工作"} /></label>{shortcut && <label className="shortcut-enabled"><input type="checkbox" checked={enabled} onChange={(event) => setEnabled(event.target.checked)} /><span>启用此项</span></label>}{isCommand && <p className="shortcut-editor-hint">以 <code>/</code> 开头的内容会作为斜杠命令直接发送给 CLI 执行（如 <code>/compact</code>、<code>/clear</code>）；其余内容作为普通命令提示发送。执行时会遵循当前会话的权限设置。</p>}</div><footer>{shortcut && <button type="button" className="danger-text" disabled={busy} onClick={() => void remove()}>删除</button>}<div className="shortcut-editor-footer-actions"><button type="button" className="secondary" disabled={busy} onClick={close}>取消</button><button className="primary" disabled={busy}>{busy ? "保存中" : "保存"}</button></div></footer></form></section></div>{pendingConfirm && createPortal(<ConfirmDialog title={pendingConfirm.title} message={pendingConfirm.message} danger={pendingConfirm.danger} onConfirm={pendingConfirm.onConfirm} onCancel={pendingConfirm.onCancel} />, document.body)}</>;
}

// ---- 消息列表（独立 memo 组件，阻断对话页其他状态变化导致的列表重渲染） ----

const MessageList = memo(function MessageList({ timeline, agentID, fail, resolving, decide, projectId, onViewTask, executionByRun, onOpenExecution, registerUserMessageElement }: {
  timeline: TimelineItem[];
  agentID: AgentID;
  fail: (message: string) => void;
  resolving: string;
  decide: (approvalId: string, decision: "allow" | "deny") => Promise<void>;
  projectId: string;
  onViewTask: (taskId: string) => void;
  executionByRun: Map<string, AgentExecution>;
  onOpenExecution: (runId: string) => void;
  registerUserMessageElement: (messageId: string, element: HTMLDivElement | null) => void;
}) {
  const anchoredExecutionRunIDs = useMemo(() => new Set(timeline.flatMap((item) => item.kind === "message" && item.message.role === "user" && item.message.runId && executionByRun.has(item.message.runId) ? [item.message.runId] : [])), [executionByRun, timeline]);
  return <>
    {timeline.map((item) => <div key={item.id} data-user-message-id={item.kind === "message" && item.message.role === "user" ? item.message.id : undefined} ref={item.kind === "message" && item.message.role === "user" ? (element) => { registerUserMessageElement(item.message.id, element); } : undefined}><div className={`timeline-entry ${item.kind === "message" ? "message-entry" : item.kind}`}>{item.kind === "message" ? <MessageCard message={item.message} agentID={agentID} fail={fail} /> : item.kind === "tool" ? <ToolCard action={item.action} resolving={resolving} decide={decide} /> : item.kind === "system" ? <SystemCard system={item.system} /> : <ErrorCard item={item} projectId={projectId} onViewTask={onViewTask} />}</div>{item.kind === "message" && item.message.role === "user" && item.message.runId && executionByRun.get(item.message.runId) && <AgentExecutionCard execution={executionByRun.get(item.message.runId)!} open={() => onOpenExecution(item.message.runId!)} />}</div>)}
    {executionByRun && Array.from(executionByRun.values()).filter((execution) => !anchoredExecutionRunIDs.has(execution.runId)).map((execution) => <AgentExecutionCard key={execution.runId} execution={execution} open={() => onOpenExecution(execution.runId)} />)}
  </>;
});

// ---- 主组件 ---------------------------------------------------------------

export default function ConversationPage() {
  const { projectId, conversationId: urlConversationId } = useParams<{ projectId: string; conversationId: string }>();
  const navigate = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();
  const { api: projectApi, setError, getConversationDraft, saveConversationDraft, flushConversationDraft } = useProjectContext();
  const { project } = useOutletContext<ProjectLayoutOutletContext>();
  const fail = setError;
  // 弹窗控制辅助函数 — 使用不可变模式创建新的 URLSearchParams
  const closeHistory = () => setSearchParams((prev) => { const next = new URLSearchParams(prev); next.delete("history"); return next; });
  const closeNewConversation = () => setSearchParams((prev) => { const next = new URLSearchParams(prev); next.delete("new"); return next; });
  const closeUsage = () => setSearchParams((prev) => { const next = new URLSearchParams(prev); next.delete("usage"); return next; });
  const closeAgentExecution = () => setSearchParams((prev) => { const next = new URLSearchParams(prev); next.delete("execution"); return next; });
  const openUsage = () => setSearchParams((prev) => { const next = new URLSearchParams(prev); next.set("usage", "true"); return next; });
  const openNewConversationParam = () => setSearchParams((prev) => { const next = new URLSearchParams(prev); next.set("new", "true"); return next; });
  const openExecutionParam = useCallback((runId: string) => setSearchParams((prev) => { const next = new URLSearchParams(prev); next.set("execution", runId); return next; }), [setSearchParams]);

  // 对话核心状态
  const [conversation, setConversation] = useState<Conversation | null>(null);
  const [conversationHistory, setConversationHistory] = useState<Conversation[]>([]);
  const [historyQuery, setHistoryQuery] = useState("");
  const [conversationHistoryCursor, setConversationHistoryCursor] = useState("");
  const [loadingMoreConversationHistory, setLoadingMoreConversationHistory] = useState(false);
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
  const [showPermissionMenu, setShowPermissionMenu] = useState(false);
  // 预约发送：内容暂存到当前对话（含子代理）彻底空闲后再真正发出。
  // 用 ref 保存并发安全的待发内容 + 一个 state 驱动 UI。
  const [pendingSendContent, setPendingSendContent] = useState<string | null>(null);
  const [showSendMenu, setShowSendMenu] = useState(false);
  const pendingSendRef = useRef<string | null>(null);
  const pendingSendConversationRef = useRef<string | null>(null);
  // 点击外部关闭权限菜单
  useEffect(() => {
    if (!showPermissionMenu) return;
    const handleClickOutside = (e: MouseEvent) => {
      const target = e.target as HTMLElement;
      if (target.closest(".permission-menu")) return;
      setShowPermissionMenu(false);
    };
    document.addEventListener("mousedown", handleClickOutside);
    return () => document.removeEventListener("mousedown", handleClickOutside);
  }, [showPermissionMenu]);

  // 点击外部关闭发送方式菜单
  useEffect(() => {
    if (!showSendMenu) return;
    const handleClickOutside = (e: MouseEvent) => {
      const target = e.target as HTMLElement;
      if (target.closest(".composer-send-wrap")) return;
      setShowSendMenu(false);
    };
    document.addEventListener("mousedown", handleClickOutside);
    return () => document.removeEventListener("mousedown", handleClickOutside);
  }, [showSendMenu]);

  const [activatingConversation, setActivatingConversation] = useState("");
  const [usage, setUsage] = useState<ConversationUsageResponse | null>(null);
  const [shortcuts, setShortcuts] = useState<Shortcut[]>([]);
  const [shortcutEditor, setShortcutEditor] = useState<ShortcutEditorState | null>(null);
  const [shortcutVariables, setShortcutVariables] = useState<{ shortcut: Shortcut; variables: Record<string, string> } | null>(null);
  const [shortcutBusy, setShortcutBusy] = useState("");
  const [hasMoreHistory, setHasMoreHistory] = useState(false);
  const [hasMoreMessageHistory, setHasMoreMessageHistory] = useState(false);
  const [historyCursor, setHistoryCursor] = useState("");
  const [pendingConfirm, setPendingConfirm] = useState<{ title: string; message: React.ReactNode; confirmLabel?: string; danger?: boolean; className?: string; icon?: React.ReactNode; onConfirm: () => void; onCancel: () => void } | null>(null);
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
  const textRef = useRef("");
  const finishedRunIds = useRef(new Set<string>());
  const usageRequestVersion = useRef(0);
  const usageConversationID = useRef<string | null>(null);
  const conversationRef = useRef<Conversation | null>(null);
  conversationRef.current = conversation;
  textRef.current = text;
  const conversationTransitionRef = useRef(false);
  const conversationRouteVersion = useRef(0);
  const conversationHistoryRequestVersion = useRef(0);
  const conversationActivationTail = useRef<Promise<void>>(Promise.resolve());
  const stopRunRef = useRef<() => Promise<void>>(async () => {});
  const stopAndClearRef = useRef<() => Promise<void>>(async () => {});
  const pendingUserDrafts = useRef(new Map<string, string>());
  const assistantOutputRuns = useRef(new Set<string>());
  // 跟踪被撤回的用户消息 runId — reload 时需过滤避免后端数据重新加回来
  const retractedMessageRuns = useRef(new Set<string>());
  const userNearBottom = useRef(true);
  const [hasNewContent, setHasNewContent] = useState(false);
  const lastReloadRequestedAt = useRef(0);
  const lastReloadRunID = useRef<string | null>(null);
  const addFileHandledRef = useRef<string | null>(null);

  const setComposerText = useCallback((value: string, conversationID = conversationRef.current?.id, persist = true) => {
    textRef.current = value;
    setText(value);
    if (persist && projectId && conversationID) saveConversationDraft(projectId, conversationID, value);
  }, [projectId, saveConversationDraft]);

  // Route changes must invalidate a pending clear before promise callbacks can
  // write the previous conversation back into the page.
  useLayoutEffect(() => {
    conversationRouteVersion.current++;
  }, [projectId, urlConversationId]);

  // 草稿归属于具体项目与会话。对话页卸载后，ProjectProvider 与浏览器缓存仍会保留它。
  useLayoutEffect(() => {
    if (!projectId || !conversation?.id) return;
    const draft = getConversationDraft(projectId, conversation.id);
    setComposerText(draft, conversation.id);
  }, [conversation?.id, getConversationDraft, projectId, setComposerText]);

  // 处理从文件树"添加到对话"传来的文件路径
  useEffect(() => {
    if (!projectId || !conversation?.id) return;
    const addFile = searchParams.get("addFile");
    if (addFile !== "true") return;
    const filePath = sessionStorage.getItem("milevia_add_file_to_chat");
    if (!filePath) return;
    // 同一会话只处理一次（避免 searchParams 清除后重新触发）
    const dedupKey = `${conversation.id}:${filePath}`;
    if (addFileHandledRef.current === dedupKey) return;
    addFileHandledRef.current = dedupKey;
    sessionStorage.removeItem("milevia_add_file_to_chat");
    // 清除 URL 中的 addFile 参数
    setSearchParams((prev) => { const next = new URLSearchParams(prev); next.delete("addFile"); return next; });
    // 将文件路径追加到输入框
    const prefix = textRef.current.trim() ? `${textRef.current}\n` : "";
    setComposerText(`${prefix}@${filePath} `, conversation.id);
  }, [projectId, conversation?.id, searchParams, setSearchParams, setComposerText]);

  useEffect(() => () => {
    const conversationID = conversationRef.current?.id;
    if (!projectId || !conversationID) return;
    // 预约尚未触发的内容在页面卸载时写回草稿，避免丢失；下次进入同会话时自然恢复为可编辑文本。
    let textToPersist = historyIndex.current === null ? textRef.current : draftBeforeHistory.current;
    if (pendingSendConversationRef.current === conversationID && pendingSendRef.current != null) {
      textToPersist = mergeDraftAndPending(textToPersist, pendingSendRef.current);
    }
    saveConversationDraft(projectId, conversationID, textToPersist);
    flushConversationDraft(projectId, conversationID);
  }, [flushConversationDraft, projectId, saveConversationDraft]);

  const rememberFinishedRun = (runID: string) => {
    const finished = finishedRunIds.current;
    finished.add(runID);
    if (finished.size > 128) {
      const oldest = finished.values().next().value;
      if (oldest !== undefined) finished.delete(oldest);
    }
  };

  const recordAssistantOutput = useCallback((runID: string, content: string) => {
    if (!runID || !content.trim()) return;
    const outputs = assistantOutputRuns.current;
    outputs.add(runID);
    if (outputs.size > 128) {
      const oldest = outputs.values().next().value;
      if (oldest !== undefined) outputs.delete(oldest);
    }
    pendingUserDrafts.current.delete(runID);
    // 一旦助手有输出，该消息就不再需要被撤回
    retractedMessageRuns.current.delete(runID);
  }, []);

  const restorePendingUserDraft = (conversationID: string, routeVersion: number, runID: string, draft: string | undefined) => {
    if (!draft || conversationRef.current?.id !== conversationID || conversationRouteVersion.current !== routeVersion) return;
    pendingUserDrafts.current.delete(runID);
    const restoredText = textRef.current === "" ? draft : textRef.current;
    textRef.current = restoredText;
    if (projectId) saveConversationDraft(projectId, conversationID, restoredText);
    setText((current) => current === "" ? draft : current);
    historyIndex.current = null;
    draftBeforeHistory.current = "";
  };

  const resetConversationView = (next: Conversation) => {
    const nextDraft = projectId ? getConversationDraft(projectId, next.id) : "";
    setComposerText(nextDraft, next.id);
    setMessages([]); setEvents([]); setRun(""); setUsage(null); historyIndex.current = null; draftBeforeHistory.current = ""; finishedRunIds.current.clear(); pendingUserDrafts.current.clear(); assistantOutputRuns.current.clear(); retractedMessageRuns.current.clear(); setShowPermissionMenu(false); setShowFullControlConfirmation(false); closeAgentExecution(); setHasMoreHistory(false); setHasMoreMessageHistory(false); setHistoryCursor(""); setLoadingOlderHistory(false); setCurrentUserMessageIndex(-1); setPendingPreviousUserMessageID(null); setHasNewContent(false); userNearBottom.current = true; setConversation(next);
  };

  // 刷新函数
  const requestConversationHistory = useCallback(async (query: string, cursor = "", append = false) => {
    if (!projectId) return [] as Conversation[];
    const requestVersion = ++conversationHistoryRequestVersion.current;
    const params = new URLSearchParams({ limit: "100" });
    if (query.trim()) params.set("q", query.trim());
    if (cursor) params.set("cursor", cursor);
    const page = await projectApi<ConversationHistoryPage>(`/api/projects/${projectId}/conversations?${params.toString()}`);
    if (requestVersion !== conversationHistoryRequestVersion.current) return [] as Conversation[];
    setConversationHistory((current) => {
      if (!append) return page.items;
      const known = new Set(current.map((item) => item.id));
      return [...current, ...page.items.filter((item) => !known.has(item.id))];
    });
    setConversationHistoryCursor(page.nextCursor);
    return page.items;
  }, [projectId, projectApi]);

  const refreshConversationHistory = useCallback(async () => {
    setHistoryQuery("");
    return requestConversationHistory("");
  }, [requestConversationHistory]);

  const searchConversationHistory = useCallback((query: string) => {
    setHistoryQuery(query);
    void requestConversationHistory(query).catch((cause) => fail(cause instanceof Error ? cause.message : "无法搜索会话历史"));
  }, [fail, requestConversationHistory]);

  const loadMoreConversationHistory = useCallback(() => {
    if (loadingMoreConversationHistory || !conversationHistoryCursor) return;
    setLoadingMoreConversationHistory(true);
    void requestConversationHistory(historyQuery, conversationHistoryCursor, true)
      .catch((cause) => fail(cause instanceof Error ? cause.message : "无法加载更多会话"))
      .finally(() => setLoadingMoreConversationHistory(false));
  }, [conversationHistoryCursor, fail, historyQuery, loadingMoreConversationHistory, requestConversationHistory]);

  const refreshShortcuts = useCallback(async () => {
    if (!projectId) return;
    setShortcuts(await projectApi<Shortcut[]>(`/api/projects/${projectId}/shortcuts?includeDisabled=true`));
  }, [projectId, projectApi]);

  const openConversationHistory = () => {
    setSearchParams((prev) => { const next = new URLSearchParams(prev); next.set("history", "true"); return next; });
    void refreshConversationHistory().catch((cause) => fail(cause instanceof Error ? cause.message : "无法刷新会话历史"));
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
    const abort = new AbortController();
    const isCurrentRoute = () => !cancelled && !conversationTransitionRef.current;
    const activateCurrentRoute = async () => {
      let release!: () => void;
      const previous = conversationActivationTail.current;
      conversationActivationTail.current = new Promise<void>((resolve) => { release = resolve; });
      await previous.catch(() => undefined);
      try {
        if (!isCurrentRoute()) return null;
        return await projectApi<Conversation>(`/api/conversations/${urlConversationId}/activate`, { method: "POST", signal: abort.signal });
      } finally {
        release();
      }
    };
    async function loadConversation() {
      try {
        // 直接链接必须按 ID 查询，不能依赖历史列表的当前分页结果。
        if (urlConversationId) {
          const detail = await projectApi<{ conversation: Conversation & { projectId: string } }>(`/api/conversations/${urlConversationId}?limit=1`, { signal: abort.signal });
          if (!isCurrentRoute()) return;
          if (detail.conversation.projectId !== projectId) throw new Error("指定会话不属于当前项目");
          if (detail.conversation.status === "running" && !detail.conversation.isCurrent) throw new Error("指定会话正在其他项目窗口中运行");
          const activated = await activateCurrentRoute();
          if (isCurrentRoute()) {
            if (!activated) return;
            // 从 /conversations 自动跳到同一会话的详情 URL 时，消息请求可能已经完成。
            // 保留同一会话的内容，避免这次路由确认把已加载历史重新清空。
            if (conversationRef.current?.id === activated.id) {
              setConversation(activated);
            } else {
              resetConversationView(activated);
            }
            void refreshConversationHistory().catch((cause) => fail(cause instanceof Error ? cause.message : "无法刷新会话历史"));
            return;
          }
        }

        // 否则使用最新对话或创建新对话
        const list = await refreshConversationHistory();
        if (cancelled || conversationTransitionRef.current) return;
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
        if (!cancelled) {
          fail(cause instanceof Error ? cause.message : "无法打开会话");
          if (urlConversationId) navigate(`/projects/${projectId}/conversations`, { replace: true });
        }
      }
    }
    void loadConversation();
    return () => { cancelled = true; abort.abort(); };
  }, [projectId, urlConversationId, clearing, fail, refreshConversationHistory, projectApi, navigate]);

  // 加载快捷方式
  useEffect(() => {
    if (!projectId) return;
    let cancelled = false;
    void (async () => {
      try {
        await projectApi("/api/shortcuts/defaults", { method: "POST" });
        if (!cancelled) await refreshShortcuts();
      } catch (cause) { if (!cancelled) fail(cause instanceof Error ? cause.message : "无法加载快捷任务"); }
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
    let hasOpenedSocket = false;
    const isCurrentConversation = () => !cancelled && conversationRef.current?.id === conversation.id;

    const reload = async (runID?: string): Promise<boolean> => {
      if (!isCurrentConversation()) return false;
      if (runID) lastReloadRunID.current = runID;
      lastReloadRequestedAt.current = Date.now();
      try {
        const data = await projectApi<{ messages: Message[]; events: Event[]; activeRunId: string | null; hasMore: boolean; hasMoreMessages: boolean; nextCursor: string }>(`/api/conversations/${conversation.id}?limit=400`);
        if (!isCurrentConversation()) return false;
        data.messages.forEach((message) => { if (message.role === "assistant") recordAssistantOutput(message.runId || "", message.content); });
        // 过滤掉被撤回的用户消息 — 停止时如果助手还没有输出，后端可能仍会返回用户消息
        const filteredMessages = data.messages.filter((message) => !(message.role === "user" && message.runId && retractedMessageRuns.current.has(message.runId)));
        setMessages((current) => mergeReloadMessages(filteredMessages, current));
        setEvents((current) => mergeConversationItems(data.events, current));
        setRun(data.activeRunId || ""); setHasMoreHistory(data.hasMore); setHasMoreMessageHistory(data.hasMoreMessages); setHistoryCursor(data.nextCursor || "");
        return true;
      } catch (cause) {
        if (isCurrentConversation()) fail(cause instanceof Error ? cause.message : "无法刷新会话");
        return false;
      }
    };

    const connect = () => {
      if (!isCurrentConversation()) return;
      reconnectAttempts++;
      socket = createWebSocket(`/ws/conversations/${conversation.id}`);
      socket.onopen = () => {
        if (!isCurrentConversation()) return;
        reconnectAttempts = 0;
        const reconnecting = hasOpenedSocket;
        hasOpenedSocket = true;
        if (reconnecting) {
          void reload();
          return;
        }
        // 首次 HTTP 加载失败时，实时连接建立后补试一次；成功时不重复请求相同历史。
        void initialHistoryLoad.then((loaded) => {
          if (!loaded && isCurrentConversation()) void reload();
        });
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
            recordAssistantOutput(canonicalRunID, canonicalContent);
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
            recordAssistantOutput(event.runId, textPart);
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
          void refreshUsage().catch((cause) => fail(cause instanceof Error ? cause.message : "无法刷新用量统计"));
        }
        if (event.type.startsWith("run.")) {
          const runPayload = asRecord(event.payload);
          if (typeof runPayload.interruptedMarker === "string" && runPayload.interruptedMarker) {
            const markerMessage: Message = { id: `${event.id}:interrupted`, runId: event.runId, role: "assistant", content: runPayload.interruptedMarker, createdAt: event.createdAt };
            setMessages((old) => mergeConversationItems(old, [markerMessage]));
          }
          pendingUserDrafts.current.delete(event.runId);
          assistantOutputRuns.current.delete(event.runId);
          // 不在这里清理 retractedMessageRuns。
          // 如果之前 stopRun 标记了撤回，需要在后续 reload 中过滤用户消息。
          // reload 完成后 recordAssistantOutput 会根据情况清理（如果有助手输出），
          // 否则 run 结束后 retractedMessageRuns 中的条目由大小限制或对话切换清理。
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

    // 历史消息不应依赖实时连接。WebSocket 建立慢或不可用时，仍要立即显示已持久化内容。
    const initialHistoryLoad = reload();
    connect();

    return () => {
      cancelled = true;
      if (reconnectTimer) clearTimeout(reconnectTimer);
      try { socket.close(); } catch { /* already closed */ }
    };
  }, [conversation?.id, fail, recordAssistantOutput, refreshUsage, projectApi]);

  // 刷新使用统计
  useEffect(() => {
    if (!conversation) return undefined;
    let cancelled = false;
    void refreshUsage().catch((cause) => { if (!cancelled) fail(cause instanceof Error ? cause.message : "无法加载用量统计"); });
    return () => { cancelled = true; };
  }, [conversation?.id, fail, refreshUsage]);

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
          full.messages.forEach((message) => { if (message.role === "assistant") recordAssistantOutput(message.runId || "", message.content); });
          // 过滤掉被撤回的用户消息
          const pollMessages = full.messages.filter((message) => !(message.role === "user" && message.runId && retractedMessageRuns.current.has(message.runId)));
          setMessages((current) => mergeReloadMessages(pollMessages, current));
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
  }, [run, conversation?.id, projectApi, recordAssistantOutput]);

  // 对话切换时重置输入历史浏览状态（历史本身按项目保留，不清空）
  useEffect(() => {
    if (!conversation) return;
    pendingUserDrafts.current.clear();
    assistantOutputRuns.current.clear();
    retractedMessageRuns.current.clear();
    historyIndex.current = null;
    draftBeforeHistory.current = "";
    // 切换到了别的会话：把仍挂在此前会话上的预约内容写回其草稿，避免被带入新会话或误发。
    const scheduledConversationID = pendingSendConversationRef.current;
    const scheduledContent = pendingSendRef.current;
    if (scheduledConversationID && scheduledContent != null && scheduledConversationID !== conversation.id) {
      if (projectId) {
        const currentDraft = getConversationDraft(projectId, scheduledConversationID) || "";
        saveConversationDraft(projectId, scheduledConversationID, mergeDraftAndPending(currentDraft, scheduledContent));
        flushConversationDraft(projectId, scheduledConversationID);
      }
      pendingSendRef.current = null;
      pendingSendConversationRef.current = null;
      setPendingSendContent(null);
    }
    setShowSendMenu(false);
  }, [conversation?.id]);

  // 加载项目级输入历史（在新建/清空对话后仍保留）
  useEffect(() => {
    if (!projectId) return undefined;
    // 切换项目时退出历史浏览，避免索引对新历史越界
    historyIndex.current = null;
    draftBeforeHistory.current = "";
    let cancelled = false;
    const loadHistory = async () => {
      try {
        const history = await projectApi<string[]>(`/api/projects/${projectId}/input-history?limit=100`);
        if (!cancelled) setInputHistory(history);
      } catch (cause) { if (!cancelled) fail(cause instanceof Error ? cause.message : "无法加载输入历史"); }
    };
    void loadHistory();
    return () => { cancelled = true; };
  }, [projectId, fail, historyRefresh, projectApi]);

  // 计算属性
  const knownSubagentTexts = useMemo(() => subagentTextIndex(events), [events]);
  const primaryMessages = useMemo(() => messages.filter((message) => !isSubagentMessage(message, knownSubagentTexts)), [knownSubagentTexts, messages]);
  const userMessages = useMemo(() => primaryMessages.filter((message) => message.role === "user"), [primaryMessages]);
  const timeline = useMemo(() => buildTimeline(primaryMessages, events), [events, primaryMessages]);
  const agentExecutions = useMemo(() => buildAgentExecutions(events), [events]);
  const executionByRun = useMemo(() => new Map(agentExecutions.map((execution) => [execution.runId, execution])), [agentExecutions]);
  const visibleContentVersion = useMemo(() => timelineContentVersion(timeline, agentExecutions), [timeline, agentExecutions]);
  const isEmptyConversation = timeline.length === 0;

  useEffect(() => {
    if (!isEmptyConversation || text.trim() || sending || clearing || stopping || showHistory || showNewConversation || showUsage || showAgentExecution) return;
    const frame = requestAnimationFrame(() => composerRef.current?.querySelector("textarea")?.focus());
    return () => cancelAnimationFrame(frame);
  }, [clearing, isEmptyConversation, sending, showAgentExecution, showHistory, showNewConversation, showUsage, stopping, text]);

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
      data.messages.forEach((message) => { if (message.role === "assistant") recordAssistantOutput(message.runId || "", message.content); });
      // 过滤掉被撤回的用户消息
      const olderMessages = data.messages.filter((message) => !(message.role === "user" && message.runId && retractedMessageRuns.current.has(message.runId)));
      setMessages((current) => mergeConversationItems(olderMessages, current));
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
      fail(cause instanceof Error ? cause.message : "无法加载更早的会话历史");
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
      // 窄屏下 timeline overflow: visible，滚动容器是 window
      window.scrollTo({ top: document.documentElement.scrollHeight, behavior });
    } else {
      const container = timelineRef.current;
      if (!container) return;
      container.scrollTo({ top: Math.max(0, container.scrollHeight - container.clientHeight), behavior });
    }
    setHasNewContent(false);
    userNearBottom.current = true;
    setCurrentUserMessageIndex(userMessages.length - 1);
  }, [userMessages.length]);

  // 延迟一帧再滚动到底部，确保 DOM 已更新后再读取 scrollHeight
  const scrollToBottomNextFrame = useCallback((behavior: ScrollBehavior = "auto") => {
    requestAnimationFrame(() => scrollToBottom(behavior));
  }, [scrollToBottom]);

  const followAfterDispatch = useCallback(() => {
    userNearBottom.current = true;
    setHasNewContent(false);
    scrollToBottomNextFrame("auto");
  }, [scrollToBottomNextFrame]);

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
      const composerHeight = `${Math.ceil(composer.getBoundingClientRect().height)}px`;
      timelineElement.style.setProperty("--composer-height", composerHeight);
      timelineElement.parentElement?.style.setProperty("--composer-height", composerHeight);
      if (!followBottom) return;
      // 去抖：取消上一帧的滚动请求，只保留最新一次
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
      timelineElement.parentElement?.style.removeProperty("--composer-height");
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
      scrollToBottomNextFrame("auto");
    } else {
      setHasNewContent((prev) => prev ? prev : true);
    }
  }, [visibleContentVersion, run, pendingApproval, scrollToBottomNextFrame]);

  // 业务方法
  const sendContent = async (rawContent: string, clearDraft = true) => {
    if (!conversation || sending || clearing || stopping || shortcutBusy) return;
    const content = rawContent.trim();
    if (!content) return;
    if (content === "/resume") {
      if (clearDraft) {
        setComposerText("", conversation.id);
      }
      openConversationHistory();
      return;
    }
    const conversationID = conversation.id;
    const routeVersion = conversationRouteVersion.current;
    setSending(true);
    if (clearDraft) {
      setComposerText("", conversationID);
    }
    setShowPermissionMenu(false); historyIndex.current = null; draftBeforeHistory.current = "";
    try {
      const data = await projectApi<{ message: Message; runId: string }>(`/api/conversations/${conversationID}/messages`, { method: "POST", body: JSON.stringify({ content }) });
      if (conversationRef.current?.id !== conversationID) return;
      if (!assistantOutputRuns.current.has(data.runId) && !finishedRunIds.current.has(data.runId)) pendingUserDrafts.current.set(data.runId, data.message.content);
      setMessages((old) => [...old, data.message]); appendInputHistory(data.message.content); setHistoryRefresh((version) => version + 1); setRun(finishedRunIds.current.has(data.runId) ? "" : data.runId);
      followAfterDispatch();
      void refreshUsage().catch((cause) => fail(cause instanceof Error ? cause.message : "无法刷新用量统计"));
      void refreshConversationHistory().catch((cause) => fail(cause instanceof Error ? cause.message : "无法刷新会话历史"));
    } catch (cause) {
      if (conversationRef.current?.id !== conversationID || conversationRouteVersion.current !== routeVersion) return;
      fail(cause instanceof Error ? cause.message : "无法发送消息");
      if (clearDraft) {
        setComposerText(content, conversationID);
        if (projectId) flushConversationDraft(projectId, conversationID);
      }
    }
    finally { setSending(false); }
  };

  const send = (event: FormEvent) => {
    event.preventDefault();
    setShowSendMenu(false);
    void sendContent(text);
  };

  // 预约发送：将输入内容暂存，等当前对话（含所有子代理）彻底空闲后再真正发送。
  const scheduleSend = () => {
    const content = text.trim();
    if (!content || !conversation) return;
    if (sending || clearing || stopping || shortcutBusy) return;
    setShowSendMenu(false);
    pendingSendRef.current = content;
    pendingSendConversationRef.current = conversation.id;
    setPendingSendContent(content);
    setComposerText("", conversation.id);
  };

  // 仅清空预约状态（不写回输入框）。用于 stopRun / clearCurrentConversation——
  // 这两条流程自己有草稿恢复（restorePendingUserDraft / resetConversationView），
  // 若这里预填输入框会污染 textRef，破坏它们对"输入框为空才恢复"的判断或被 reset 覆盖。
  const clearScheduledSend = () => {
    const pending = pendingSendRef.current;
    pendingSendRef.current = null;
    pendingSendConversationRef.current = null;
    if (pending != null) setPendingSendContent(null);
  };

  // 取消预约（用户主动点取消按钮）：清空预约状态并把暂存内容写回输入框，方便继续编辑或改为立即发送。
  const cancelScheduledSend = () => {
    const conversationID = conversation?.id;
    const pending = pendingSendRef.current;
    if (!conversationID || pending == null) return;
    clearScheduledSend();
    const existing = textRef.current.trim();
    setComposerText(existing ? `${existing}\n${pending}` : pending, conversationID);
    requestAnimationFrame(() => composerRef.current?.querySelector("textarea")?.focus());
  };

  // 会话彻底空闲（主回合结束，且没有子代理仍在运行）时，自动发出预约内容。
  const isConversationIdle = useCallback(() => {
    if (run) return false;
    for (const execution of agentExecutions) {
      for (const agent of flattenAgents(execution.agents)) {
        if (agent.status === "running" || agent.status === "pending") return false;
      }
    }
    return true;
  }, [agentExecutions, run]);

  // 监听预约内容：一旦会话空闲便真正发送，随后清理预约状态。
  useEffect(() => {
    if (!pendingSendRef.current) return;
    if (sending || clearing || stopping || shortcutBusy) return;
    const conversationID = pendingSendConversationRef.current;
    const content = pendingSendRef.current;
    if (!conversationID || !content) return;
    if (conversationRef.current?.id !== conversationID) return; // 切换会话后不越界发送
    if (!isConversationIdle()) return; // 主回合或子代理仍在运行，继续等待
    pendingSendRef.current = null;
    pendingSendConversationRef.current = null;
    // 发送成功/失败后统一清除 UI，避免残留预约提示。
    // 使用默认 clearDraft=true：与普通发送行为一致——成功后清空输入框，
    // 失败时 sendContent 会把内容写回输入框（不丢失预约内容）。
    // 预约本身在 scheduleSend 时已清空输入框，因此这里使用默认行为最安全。
    void (async () => {
      try {
        await sendContent(content);
      } finally {
        setPendingSendContent((current) => current === content ? null : current);
      }
    })();
  }, [isConversationIdle, pendingSendContent, clearing, sendContent, sending, shortcutBusy, stopping]);

  const runShortcut = async (shortcut: Shortcut, variables: Record<string, string> = {}, variablesReady = false) => {
    if (!conversation || sending || clearing || stopping || shortcutBusy) return;
    const required = requiredShortcutVariables(shortcut.template);
    if (!variablesReady && required.length) { setShortcutVariables({ shortcut, variables }); return; }
    if (shortcut.defaultAction === "fill") {
      const conversationID = conversation.id;
      setShortcutBusy(shortcut.id);
      try {
        const preview = await projectApi<{ content: string }>(`/api/conversations/${conversationID}/shortcuts/${shortcut.id}/preview`, { method: "POST", body: JSON.stringify({ variables }) });
        if (conversationRef.current?.id === conversationID) setComposerText(preview.content, conversationID);
      } catch (cause) { fail(cause instanceof Error ? cause.message : "无法准备快捷任务"); }
      finally { setShortcutBusy(""); }
      return;
    }
    setShortcutBusy(shortcut.id);
    try {
      const action = shortcut.defaultAction === "confirm" ? "confirm" : "run";
      const conversationID = conversation.id;
      const data = await projectApi<{ message: Message; runId: string }>(`/api/conversations/${conversationID}/shortcuts/${shortcut.id}/run`, { method: "POST", body: JSON.stringify({ variables, action }) });
      if (conversationRef.current?.id !== conversationID) return;
      if (!assistantOutputRuns.current.has(data.runId) && !finishedRunIds.current.has(data.runId)) pendingUserDrafts.current.set(data.runId, data.message.content);
      setMessages((old) => [...old, data.message]);
      appendInputHistory(data.message.content);
      setHistoryRefresh((version) => version + 1);
      setRun(finishedRunIds.current.has(data.runId) ? "" : data.runId);
      followAfterDispatch();
      void refreshUsage().catch((cause) => fail(cause instanceof Error ? cause.message : "无法刷新用量统计"));
      void refreshConversationHistory().catch((cause) => fail(cause instanceof Error ? cause.message : "无法刷新会话历史"));
    } catch (cause) { fail(cause instanceof Error ? cause.message : "无法运行快捷任务"); }
    finally { setShortcutBusy(""); }
  };

  const newConversation = async (agentId: AgentID, permissionMode: PermissionMode, profileID?: string) => {
    if (sending || clearing || stopping || shortcutBusy || run || !projectId) { closeNewConversation(); return; }
    conversationTransitionRef.current = true;
    setClearing(true);
    try {
      const next = await projectApi<Conversation>(`/api/projects/${projectId}/conversations?new=true`, { method: "POST", body: JSON.stringify({ agentId, permissionMode, profileId: profileID || "" }) });
      resetConversationView(next);
      navigate(`/projects/${projectId}/conversations/${next.id}`, { replace: true });
      void refreshConversationHistory().catch((cause) => fail(cause instanceof Error ? cause.message : "无法刷新会话历史"));
    }
    catch (cause) { fail(cause instanceof Error ? cause.message : "无法新建会话"); }
    finally { conversationTransitionRef.current = false; setClearing(false); }
  };

  const clearConversationContext = () => {
    if (!conversation || sending || clearing || stopping || shortcutBusy) return;
    if (run) {
      // 运行中时先停止再清空
      setPendingConfirm({
        title: "停止并清空",
        message: "当前对话正在运行中，将先停止运行（包括排队中的请求）再清空上下文。当前内容仍可在历史会话中恢复。",
        confirmLabel: "停止并清空",
        danger: true,
        className: "conversation-clear-dialog is-running",
        icon: <ConversationClearIcon running />,
        onConfirm: () => {
          setPendingConfirm(null);
          void stopAndClearRef.current();
        },
        onCancel: () => setPendingConfirm(null),
      });
      return;
    }
    setPendingConfirm({
      title: "清空上下文",
      message: "将开始一个新的空白会话，当前内容仍可在历史会话中恢复。",
      confirmLabel: "开始新会话",
      className: "conversation-clear-dialog",
      icon: <ConversationClearIcon />,
      onConfirm: () => {
        setPendingConfirm(null);
        void clearCurrentConversation();
      },
      onCancel: () => setPendingConfirm(null),
    });
  };

  const stopAndClear = async () => {
    if (!conversation || stopping) return;
    // 运行可能已在确认框等待期间自然结束
    if (!run) {
      void clearCurrentConversation();
      return;
    }
    const currentRun = run;
    const conversationID = conversation.id;
    setStopping(true);
    try {
      await stopRunInternal(false);
    } catch (cause) {
      if (requiresForceStop(cause)) {
        try {
          await stopRunInternal(true);
        } catch (forceCause) {
          fail(forceCause instanceof Error ? forceCause.message : "无法强制停止任务");
          setStopping(false);
          return;
        }
      } else {
        fail(cause instanceof Error ? cause.message : "无法停止任务");
        setStopping(false);
        return;
      }
    }
    // 等待后端确认对话状态变为 idle（最多 10 秒）
    let idle = false;
    for (let i = 0; i < 20; i++) {
      await new Promise((resolve) => setTimeout(resolve, 500));
      try {
        const status = await projectApi<{ conversation: Conversation; activeRunId: string | null }>(`/api/conversations/${conversationID}?limit=1`);
        if (status.conversation.status === "idle" && !status.activeRunId) { idle = true; break; }
      } catch { /* ignore polling errors */ }
    }
    setStopping(false);
    if (!idle) {
      // 超时时不清空 run，因为后端可能仍在 running
      fail("停止超时，请稍后重试清空操作。");
      return;
    }
    // 只清空我们正在停止的 run，避免误清空其他新启动的 run
    setRun((active) => active === currentRun ? "" : active);
    void clearCurrentConversation(true);
  };
  stopAndClearRef.current = stopAndClear;

  const clearCurrentConversation = async (skipRunGuard = false) => {
    if (!conversation || sending || clearing || shortcutBusy || (!skipRunGuard && run)) return;
    // 用户主动清空会话：清空预约状态，让预约内容随之作废。
    // 这里不清写回输入框——resetConversationView 会用新会话草稿重置输入框，写回会被覆盖。
    clearScheduledSend();
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
      void refreshConversationHistory().catch((cause) => fail(cause instanceof Error ? cause.message : "无法刷新会话历史"));
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
      if (clearStillOwnsView()) fail(cause instanceof Error ? cause.message : "无法清除会话上下文");
    } finally {
      conversationTransitionRef.current = false;
      setClearing(false);
    }
  };

  const activateConversation = async (item: Conversation) => {
    if (!conversation || item.id === conversation.id || item.status === "running") { closeHistory(); return; }
    if (sending || clearing || stopping || shortcutBusy) { closeHistory(); return; }
    setActivatingConversation(item.id);
    try {
      const next = await projectApi<Conversation>(`/api/conversations/${item.id}/activate`, { method: "POST" });
      resetConversationView(next); closeHistory();
      if (projectId) navigate(`/projects/${projectId}/conversations/${next.id}`, { replace: true });
      void refreshConversationHistory().catch((cause) => fail(cause instanceof Error ? cause.message : "无法刷新会话历史"));
    } catch (cause) { fail(cause instanceof Error ? cause.message : "无法切换会话"); }
    finally { setActivatingConversation(""); }
  };

  const changePermissionMode = async (permissionMode: PermissionMode) => {
    if (!conversation || conversation.permissionMode === permissionMode || run || clearing || stopping) return;
    if (permissionMode === "full_control") { setShowPermissionMenu(false); setShowFullControlConfirmation(true); return; }
    const conversationID = conversation.id;
    setChangingPermission(true);
    try {
      const updated = await projectApi<Conversation>(`/api/conversations/${conversationID}/permission-mode`, { method: "POST", body: JSON.stringify({ permissionMode }) });
      if (conversationRef.current?.id !== conversationID) return;
      setConversation(updated);
      setShowPermissionMenu(false);
    }
    catch (cause) { fail(cause instanceof Error ? cause.message : "无法修改权限模式"); }
    finally { setChangingPermission(false); }
  };

  const confirmFullControl = async () => {
    if (!conversation || run || clearing || stopping) return;
    const conversationID = conversation.id;
    setChangingPermission(true);
    try {
      const updated = await projectApi<Conversation>(`/api/conversations/${conversationID}/permission-mode`, { method: "POST", body: JSON.stringify({ permissionMode: "full_control" }) });
      if (conversationRef.current?.id !== conversationID) return;
      setConversation(updated);
      setShowFullControlConfirmation(false);
    } catch (cause) { fail(cause instanceof Error ? cause.message : "无法修改权限模式"); }
    finally { setChangingPermission(false); }
  };

  const decide = useCallback(async (approvalId: string, decision: "allow" | "deny") => {
    setResolving(approvalId);
    try { await projectApi(`/api/approvals/${approvalId}`, { method: "POST", body: JSON.stringify({ decision }) }); }
    catch (cause) { fail(cause instanceof Error ? cause.message : "无法处理命令审批"); }
    finally { setResolving(""); }
  }, [projectApi, fail]);

  // 稳定回调：避免每次渲染新建函数导致 MessageList memo 失效
  const onViewTask = useCallback((taskId: string) => { navigate(`/projects/${project.id}/tasks/${taskId}`); }, [navigate, project.id]);
  const registerUserMessageElement = useCallback((messageId: string, element: HTMLDivElement | null) => {
    if (element) userMessageElements.current.set(messageId, element);
    else userMessageElements.current.delete(messageId);
  }, []);

  const stopRunInternal = async (force: boolean, requestedRunID = run) => {
    const runID = requestedRunID;
    if (!runID) return;
    const url = force ? `/api/runs/${runID}/stop?force=true` : `/api/runs/${runID}/stop`;
    const result = await projectApi<{ status: string }>(url, { method: "POST" });
    if (result.status !== "stopping") { rememberFinishedRun(runID); setRun((active) => active === runID ? "" : active); }
    return result.status;
  };

  const stopRun = async () => {
    if (!conversation || !run || stopping) return;
    const conversationID = conversation.id;
    const routeVersion = conversationRouteVersion.current;
    const runID = run;
    // 用户主动停止：仅清空预约状态，避免停止后又把预约自动发射开启新一轮。
    // 不写回输入框——restorePendingUserDraft 依赖"输入框为空"才恢复中断草稿，预填会破坏它。
    clearScheduledSend();
    // 助手没有输出时，需要撤回用户消息并恢复草稿
    const shouldRetractMessage = !assistantOutputRuns.current.has(runID);
    const draftToRestore = shouldRetractMessage ? pendingUserDrafts.current.get(runID) : undefined;
    setStopping(true);
    // retractUserMessage 在执行时重新检查 assistantOutputRuns，避免竞态：
    // 如果在异步 stop 请求期间 WebSocket 收到了助手输出，就不应撤回消息
    const retractUserMessage = () => {
      if (!shouldRetractMessage) return;
      if (assistantOutputRuns.current.has(runID)) return;
      const retracted = retractedMessageRuns.current;
      retracted.add(runID);
      if (retracted.size > 128) {
        const oldest = retracted.values().next().value;
        if (oldest !== undefined) retracted.delete(oldest);
      }
      setMessages((items) => items.filter((item) => !(item.role === "user" && item.runId === runID)));
    };
    try {
      const result = await stopRunInternal(false);
      if (result !== "stopping") setStopping(false);
      retractUserMessage();
      restorePendingUserDraft(conversationID, routeVersion, runID, draftToRestore);
    }
    catch (cause) {
      if (requiresForceStop(cause)) {
        setPendingConfirm({
          title: "强制停止",
          message: "该对话还有其他排队中或执行中的请求，强制停止将一并取消它们。是否继续？",
          danger: true,
          onConfirm: () => {
            setPendingConfirm(null);
            void stopRunInternal(true, runID).then((result) => { retractUserMessage(); restorePendingUserDraft(conversationID, routeVersion, runID, draftToRestore); if (result !== "stopping") setStopping(false); }).catch((cause) => {
              fail(cause instanceof Error ? cause.message : "无法停止任务");
              setStopping(false);
            });
          },
          onCancel: () => { setPendingConfirm(null); setStopping(false); },
        });
        return;
      }
      fail(cause instanceof Error ? cause.message : "无法停止任务");
      setStopping(false);
      return;
    }
  };
  stopRunRef.current = stopRun;

  const handleTextChange = (value: string) => {
    historyIndex.current = null;
    draftBeforeHistory.current = "";
    setComposerText(value);
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
      setComposerText(inputHistory[historyIndex.current!], undefined, false);
      return;
    }
    const currentIndex = historyIndex.current;
    if (currentIndex === null) return;
    event.preventDefault();
    if (currentIndex < inputHistory.length - 1) {
      const nextIndex = currentIndex + 1;
      historyIndex.current = nextIndex;
      setComposerText(inputHistory[nextIndex], undefined, false);
    } else {
      historyIndex.current = null;
      setComposerText(draftBeforeHistory.current, undefined, false);
      draftBeforeHistory.current = "";
    }
  };

  // 追加一条输入历史，连续重复内容压缩为一条，最多保留 100 条
  const appendInputHistory = useCallback((content: string) => {
    setInputHistory((items) => {
      if (items.length > 0 && items[items.length - 1] === content) return items;
      const next = [...items, content];
      return next.length > 100 ? next.slice(next.length - 100) : next;
    });
  }, []);

  const handleTaskDispatched = (message: Message, runID: string) => {
    if (!assistantOutputRuns.current.has(runID) && !finishedRunIds.current.has(runID)) pendingUserDrafts.current.set(runID, message.content);
    setMessages((items) => [...items, message]);
    appendInputHistory(message.content);
    setHistoryRefresh((version) => version + 1);
    setRun(finishedRunIds.current.has(runID) ? "" : runID);
    void refreshUsage().catch((cause) => fail(cause instanceof Error ? cause.message : "无法刷新用量统计"));
    void refreshConversationHistory().catch((cause) => fail(cause instanceof Error ? cause.message : "无法刷新会话历史"));
  };

  const isCodex = conversation?.agentId === "codex";
  const permissionLabel = isCodex ? (conversation?.permissionMode === "read_only" ? "仅分析" : conversation?.permissionMode === "full_control" ? "完全控制" : "项目内执行") : conversation?.permissionMode === "full_control" ? "完全控制" : "默认权限";
  const runLabel = isCodex ? "Codex 正在处理任务" : "Claude 正在处理任务";
  const currentUsage = usage?.currentRun ?? usage?.latestRun;
  const displayedModel = usage?.context.model || currentUsage?.model || (isCodex ? "Codex" : "Claude Code");
  const promptShortcuts = shortcuts.filter((shortcut) => shortcut.kind === "prompt" || shortcut.kind === "snippet");
  const commandShortcuts = shortcuts.filter((shortcut) => shortcut.kind === "command_request");
  const userMessageNavigationIndex = userMessages.length === 0 ? -1 : Math.max(0, Math.min(currentUserMessageIndex, userMessages.length - 1));
  const starterSuggestions = [
    { label: "审查改动", prompt: "请审查当前工作区的改动，重点检查潜在问题、风险和缺少的测试。" },
    { label: "定位问题", prompt: "请帮我定位并分析以下问题的原因：" },
    { label: "实现功能", prompt: "请帮我在当前项目中实现以下功能：" },
    { label: "解释代码", prompt: "请解释以下代码的作用、关键逻辑和需要注意的地方：" },
  ];

  const startFromSuggestion = (prompt: string) => {
    historyIndex.current = null;
    draftBeforeHistory.current = "";
    setComposerText(prompt);
    requestAnimationFrame(() => composerRef.current?.querySelector("textarea")?.focus());
  };

  const renderShortcutCell = (shortcut: Shortcut | undefined, kind: "prompt" | "command_request", placeholder = false) => {
    if (!shortcut && placeholder) return <div className="quick-tag-slot" aria-hidden="true" />;
    if (!shortcut) return <button type="button" className={`quick-tag-empty${kind === "command_request" ? " command-tag" : ""}`} onClick={() => setShortcutEditor({ kind })}>{kind === "command_request" ? "添加命令" : "添加提示词"}</button>;
    return <div className={`quick-tag ${shortcut.enabled ? "" : "disabled"}${kind === "command_request" ? " command-tag" : ""}`} key={shortcut.id}><button type="button" disabled={!shortcut.enabled || !conversation || sending || clearing || stopping || Boolean(shortcutBusy)} onClick={() => void runShortcut(shortcut)} title={shortcut.enabled ? shortcut.template : `${shortcut.template}\n\n${shortcut.name}已停用`}><span className="quick-tag-text">{shortcutBusy === shortcut.id ? "发送中" : shortcut.name}</span></button><button type="button" className="quick-tag-edit" title={`编辑 ${shortcut.name}`} aria-label={`编辑 ${shortcut.name}`} disabled={clearing || Boolean(shortcutBusy)} onClick={() => setShortcutEditor({ kind: shortcut.kind, shortcut })}><ShortcutMoreIcon /></button></div>;
  };

  const reorderKind = useCallback(async (kind: SortableShortcutKind, orderedIDs: string[]) => {
    try {
      await projectApi(`/api/projects/${projectId}/shortcuts/reorder`, {
        method: "PUT",
        body: JSON.stringify({ kind, orderedIds: orderedIDs }),
      });
    } catch (cause) {
      fail(cause instanceof Error ? cause.message : "无法保存排序");
    } finally {
      await refreshShortcuts();
    }
  }, [projectId, projectApi, fail, refreshShortcuts]);

  const renderPromptCell = (shortcut: Shortcut | undefined) => renderShortcutCell(shortcut, "prompt");
  const renderCommandCell = (shortcut: Shortcut | undefined) => renderShortcutCell(shortcut, "command_request");
  const sortableDraggingDisabled = sending || clearing || stopping || Boolean(shortcutBusy);

  return <>
    {createPortal(<div className={`head-actions-menu${showMobileActions ? " mobile-open" : ""}`}>
        <button className="head-actions-mobile-toggle" type="button" aria-expanded={showMobileActions} onClick={() => setShowMobileActions((open) => !open)}>操作</button>
        <div className="head-actions">
          <div className="permission-menu">
            <button className={`permission-trigger ${conversation?.permissionMode === "full_control" ? "full" : ""}`} type="button" aria-haspopup="menu" aria-expanded={showPermissionMenu} disabled={!!run || clearing || stopping || changingPermission} onClick={() => setShowPermissionMenu((open) => !open)}><PermissionModeIcon /><span>{permissionLabel}</span></button>
            {showPermissionMenu && <div className="permission-popover" role="menu">{isCodex ? <><button className={conversation?.permissionMode === "read_only" ? "selected" : ""} onClick={() => void changePermissionMode("read_only")}><b>仅分析</b><span>只读检查，不修改项目。</span></button><button className={conversation?.permissionMode === "workspace_write" ? "selected" : ""} onClick={() => void changePermissionMode("workspace_write")}><b>项目内执行</b><span>可在当前项目范围内读写和执行。</span></button><button className={conversation?.permissionMode === "full_control" ? "selected full" : ""} onClick={() => void changePermissionMode("full_control")}><b>完全控制</b><span>不受沙箱限制，命令直接执行</span></button></> : <><button className={conversation?.permissionMode === "approval_required" ? "selected" : ""} onClick={() => void changePermissionMode("approval_required")}><b>默认权限</b><span>命令执行前需要确认</span></button><button className={conversation?.permissionMode === "full_control" ? "selected full" : ""} onClick={() => void changePermissionMode("full_control")}><b>完全控制</b><span>命令直接执行</span></button></>}</div>}
          </div>
          <button className="conversation-head-action secondary" type="button" disabled={sending} onClick={openConversationHistory}><HistoryIcon /><span>历史</span></button>
          <button className="conversation-head-action new-conversation-action primary" type="button" disabled={!!run || sending || stopping} onClick={openNewConversationParam}><NewConversationIcon /><span>新会话</span></button>
        </div>
      </div>, document.querySelector('.head-actions-slot') || document.body)}
    {/* 对话面板：快捷方式、对话内容和任务队列 */}
    <section className="conversation-canvas">
      <aside className="quick-tag-rail" aria-label="常用操作">
        <div className="quick-actions-row">
          <div className="quick-tag-group"><div className="quick-tag-heading"><span className="quick-tag-heading-label"><ShortcutCategoryIcon kind="prompt" /><span>常用提示词</span></span><button type="button" title="新增常用提示词" aria-label="新增常用提示词" onClick={() => setShortcutEditor({ kind: "prompt" })}><ShortcutAddIcon /></button></div><ShortcutSortableList items={promptShortcuts} kind="prompt" renderItem={renderPromptCell} draggingDisabled={sortableDraggingDisabled} onReorder={reorderKind} /></div>
          <div className="quick-tag-group command-tags"><div className="quick-tag-heading"><span className="quick-tag-heading-label"><ShortcutCategoryIcon kind="command" /><span>常用命令</span></span><button type="button" title="新增常用命令" aria-label="新增常用命令" onClick={() => setShortcutEditor({ kind: "command_request" })}><ShortcutAddIcon /></button></div><ShortcutSortableList items={commandShortcuts} kind="command_request" renderItem={renderCommandCell} draggingDisabled={sortableDraggingDisabled} onReorder={reorderKind} /></div>
        </div>
        <div className="quick-actions-mobile">
          <div className="quick-tag-group"><div className="quick-tag-heading"><span className="quick-tag-heading-label"><ShortcutCategoryIcon kind="prompt" /><span>常用提示词</span></span><button type="button" title="新增常用提示词" aria-label="新增常用提示词" onClick={() => setShortcutEditor({ kind: "prompt" })}><ShortcutAddIcon /></button></div><ShortcutSortableList items={promptShortcuts} kind="prompt" renderItem={renderPromptCell} draggingDisabled={sortableDraggingDisabled} onReorder={reorderKind} /></div>
          <div className="quick-tag-group command-tags"><div className="quick-tag-heading"><span className="quick-tag-heading-label"><ShortcutCategoryIcon kind="command" /><span>常用命令</span></span><button type="button" title="新增常用命令" aria-label="新增常用命令" onClick={() => setShortcutEditor({ kind: "command_request" })}><ShortcutAddIcon /></button></div><ShortcutSortableList items={commandShortcuts} kind="command_request" renderItem={renderCommandCell} draggingDisabled={sortableDraggingDisabled} onReorder={reorderKind} /></div>
        </div>
      </aside>
      <section className="chat-center">
      <section className="timeline" ref={timelineRef} onScroll={onTimelineScroll}>
        <div ref={top} />
        {hasMoreHistory && <button className="secondary load-earlier-history" type="button" disabled={loadingOlderHistory || sending} onClick={() => void loadOlderHistory()}>{loadingOlderHistory ? "加载中" : "加载更早记录"}</button>}
        {isEmptyConversation && <section className="conversation-starter" aria-label="新会话建议"><span>新会话</span><h2>从一个任务开始</h2><div className="conversation-starter-options">{starterSuggestions.map((suggestion) => <button key={suggestion.label} type="button" onClick={() => startFromSuggestion(suggestion.prompt)}>{suggestion.label}</button>)}</div></section>}
        <MessageList timeline={timeline} agentID={conversation?.agentId || "claude-code"} fail={fail} resolving={resolving} decide={decide} projectId={project.id} onViewTask={onViewTask} executionByRun={executionByRun} onOpenExecution={openExecutionParam} registerUserMessageElement={registerUserMessageElement} />
        {run && <div className="run-indicator"><span></span>{runLabel}</div>}
        <div ref={bottom} />
      </section>
      <div className={`scroll-buttons${hasNewContent ? " has-new" : ""}`}>
        <button type="button" className="scroll-btn scroll-to-top" title="回到顶部" aria-label="回到顶部" onClick={scrollToTop}><ScrollNavigationIcon direction="top" /></button>
        <button type="button" className="scroll-btn scroll-to-previous-message" title="上一条我的消息" aria-label="上一条我的消息" disabled={loadingOlderHistory || (userMessageNavigationIndex <= 0 && !hasMoreMessageHistory)} onClick={goToPreviousUserMessage}><ScrollNavigationIcon direction="previous" /></button>
        <button type="button" className="scroll-btn scroll-to-next-message" title="下一条我的消息" aria-label="下一条我的消息" disabled={userMessageNavigationIndex < 0 || userMessageNavigationIndex >= userMessages.length - 1} onClick={goToNextUserMessage}><ScrollNavigationIcon direction="next" /></button>
        <button type="button" className={`scroll-btn scroll-to-bottom${hasNewContent ? " pulse" : ""}`} title="回到底部" aria-label="回到底部" onClick={() => scrollToBottomNextFrame("auto")}><ScrollNavigationIcon direction="bottom" /></button>
      </div>
      <form ref={composerRef} className={`composer${pendingApproval ? " has-approval" : ""}${isEmptyConversation ? " empty-session" : ""}`} onSubmit={(event) => void send(event)}>
        {pendingApproval && <ApprovalBanner action={pendingApproval} resolving={resolving} decide={decide} scrollToCard={() => { const el = timelineRef.current?.querySelector(".timeline-entry.tool .tool-card.waiting"); if (el) el.scrollIntoView({ behavior: "smooth", block: "center" }); }} />}
        <textarea value={text} onChange={(event) => handleTextChange(event.target.value)} onKeyDown={(event) => { if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing) { event.preventDefault(); event.currentTarget.form?.requestSubmit(); return; } navigateInputHistory(event); }} placeholder={`描述希望${isCodex ? " Codex" : " Claude"}在当前项目中完成的工作...`} disabled={sending || clearing || stopping || Boolean(shortcutBusy)} />
        <div className="composer-footer"><ComposerRunnerInfo runnerID={project.runner} agentID={conversation?.agentId || "claude-code"} run={run} runLabel={runLabel} permissionMode={conversation?.permissionMode} usage={usage} displayedModel={displayedModel} contextLabel={contextLabel(usage?.context).replace(/^上下文 /, "")} contextLevel={contextLevel(usage?.context)} onShowUsage={openUsage} stopping={stopping} onStop={() => void stopRun()} /><span className="composer-actions"><button className="secondary composer-action composer-clear" type="button" disabled={sending || clearing || stopping || Boolean(shortcutBusy)} onClick={clearConversationContext}><ComposerActionIcon action="clear" /><span>清空</span></button><button className="secondary composer-action composer-continue" type="button" disabled={sending || clearing || stopping} onClick={() => void sendContent("继续", false)}><ComposerActionIcon action="continue" /><span>继续</span></button>{run && <button className="secondary composer-action composer-stop" type="button" disabled={stopping} onClick={() => void stopRun()}>{stopping ? "停止中" : "停止"}</button>}<span className="composer-send-wrap"><button className="primary composer-action composer-send" disabled={!text.trim() || sending || clearing || stopping || Boolean(shortcutBusy)}><ComposerActionIcon action="send" /><span>{sending ? "发送中" : "发送"}</span></button><button className={`composer-send-more${showSendMenu ? " open" : ""}`} type="button" title="发送方式" aria-label="发送方式" aria-haspopup="menu" aria-expanded={showSendMenu} disabled={sending || clearing || stopping || Boolean(shortcutBusy)} onClick={() => setShowSendMenu((value) => !value)}><svg className="composer-send-more-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M6 9.5 12 15.5 18 9.5" /></svg></button>{showSendMenu && <span className="send-menu" role="menu"><button className="send-menu-item" type="button" role="menuitem" disabled={!text.trim() || sending || clearing || stopping || Boolean(shortcutBusy)} onClick={(evt) => { setShowSendMenu(false); evt.currentTarget.form?.requestSubmit(); }}><ComposerActionIcon action="send" /><span><b>立即发送</b><small>立即交给 {isCodex ? "Codex" : "Claude"}，在下一轮工具调用后继续</small></span></button><button className="send-menu-item" type="button" role="menuitem" disabled={!text.trim() || sending || clearing || stopping || Boolean(shortcutBusy)} onClick={() => void scheduleSend()}><ComposerActionIcon action="schedule" /><span><b>预约发送</b><small>当前任务（含子代理）全部结束后再发送</small></span></button></span>}</span></span></div>
        {pendingSendContent && <div className="composer-pending"><span className="composer-pending-icon"><svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="13.2" r="6.2" /><path d="M12 10.5V13l1.8 1.2" /></svg></span><span className="composer-pending-text"><b>已预约发送</b><small>{run ? "等待当前任务完成..." : "等待子代理完成..."}<span className="composer-pending-preview">{pendingSendContent.length > 40 ? `${pendingSendContent.slice(0, 40)}…` : pendingSendContent}</span></small></span><button className="composer-pending-cancel" type="button" title="撤回预约并带回输入框" onClick={cancelScheduledSend}>取消</button></div>}
      </form>
      </section>
      <aside className="task-queue-rail" aria-label="任务队列">
        <TaskQueue projectID={project.id} conversationID={conversation?.id || ""} permissionMode={conversation?.permissionMode} request={projectApi} fail={fail} dispatchDisabled={clearing || stopping || !conversation} onDispatched={handleTaskDispatched} openBoard={(taskID) => navigate(`/projects/${project.id}/tasks${taskID ? `/${taskID}` : ""}`)} />
      </aside>
    </section>
    {showNewConversation && <NewConversationDialog runnerID={project.runner} close={closeNewConversation} create={newConversation} />}
    {showHistory && <ConversationHistoryDialog conversations={conversationHistory} activeID={conversation?.id || ""} busyID={activatingConversation} close={closeHistory} activate={activateConversation} search={searchConversationHistory} hasMore={Boolean(conversationHistoryCursor)} loadingMore={loadingMoreConversationHistory} loadMore={loadMoreConversationHistory} />}
    {showFullControlConfirmation && <FullControlConfirmationDialog close={() => setShowFullControlConfirmation(false)} confirm={confirmFullControl} changing={changingPermission} isCodex={isCodex} />}
    {showAgentExecution && agentExecutions.find((execution) => execution.runId === showAgentExecution) && <AgentExecutionDialog execution={agentExecutions.find((execution) => execution.runId === showAgentExecution)!} close={closeAgentExecution} />}
    {showUsage && <UsageDialog agentID={conversation?.agentId || "claude-code"} usage={usage} currentRun={currentUsage} close={closeUsage} />}
    {shortcutEditor && <ShortcutEditor projectID={project.id} state={shortcutEditor} close={() => setShortcutEditor(null)} refresh={refreshShortcuts} fail={fail} />}
    {shortcutVariables && <ShortcutVariablesDialog state={shortcutVariables} close={() => setShortcutVariables(null)} run={(variables) => { setShortcutVariables(null); void runShortcut(shortcutVariables.shortcut, variables, true); }} />}
    {pendingConfirm && createPortal(<ConfirmDialog title={pendingConfirm.title} message={pendingConfirm.message} confirmLabel={pendingConfirm.confirmLabel} danger={pendingConfirm.danger} className={pendingConfirm.className} icon={pendingConfirm.icon} onConfirm={pendingConfirm.onConfirm} onCancel={pendingConfirm.onCancel} />, document.body)}
  </>;
}
