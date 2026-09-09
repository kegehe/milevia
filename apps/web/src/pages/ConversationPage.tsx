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
import { ProjectAiConfigDialog } from "../features/run/ProjectAiConfigDialog";
import { ConfirmDialog } from "../components/ConfirmDialog";
import { useProjectContext } from "../stores/useProjectStore";
import { useUIPreferences, type AppPreferences } from "../stores/useUIPreferences";
import type {
  Conversation, Message, Event, Shortcut, ShortcutEditorState,
  ConversationWorkspace,
  PermissionMode, AgentID, AgentStatus, TimelineItem, ToolAction, AgentNode, AgentLog,
  AgentExecution, RunUsage, ConversationUsageResponse,
  RunnerInfo, CheckUpdateResult, UpdateResult, SystemItem, SystemVariant, AgentProfile,
  Skill,
} from "../lib/types";
import { api, apiWithTimeout, asRecord } from "../lib/api";
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
import {
  MAX_OPEN_CONVERSATION_TABS, closeConversationTab, markConversationTabRead, openConversationTab, recordConversationActivity,
  readClosedConversationIds, clearConversationTabClosed, markConversationTabClosed, readConversationTabs, writeConversationTabs, type ConversationTabsState,
} from "../lib/conversation-tabs";
import { readConversationPanels, writeConversationPanels, type ConversationPanelKey, type ConversationPanelsState } from "../lib/conversation-panels";

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

function ConversationDeleteIcon() {
  return <svg className="conversation-delete-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M4.5 7h15M9 7V4.5h6V7M7 7l.8 12.5h8.4L17 7M10 11v5M14 11v5" /></svg>;
}

function NewConversationIcon() {
  return <svg className="conversation-head-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M5 5.5h9.7L19 9.8v8.7a1.8 1.8 0 0 1-1.8 1.8H6.8A1.8 1.8 0 0 1 5 18.5v-13Z" /><path d="M14.5 5.5v4.7H19M12 12v5M9.5 14.5h5" /></svg>;
}

function NewConversationDialogIcon() {
  return <svg className="new-conversation-dialog-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M5.5 4.5h8.7l4.3 4.3v9.7a1.5 1.5 0 0 1-1.5 1.5H7a1.5 1.5 0 0 1-1.5-1.5v-14Z" /><path d="M14 4.5v4.6h4.5M12 11v5M9.5 13.5h5" /></svg>;
}

function AgentToolIcon({ agent }: { agent: AgentID }) {
  return agent === "codex"
    ? <svg className="new-conversation-agent-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M8.1 4.5h7.8l4.1 7.5-4.1 7.5H8.1L4 12l4.1-7.5Z" /><path d="m9.2 9.3 2.8 2.7-2.8 2.7M14.2 14.7h1.5" /></svg>
    : <svg className="new-conversation-agent-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M12 3.8c4.5 0 7.5 3.1 7.5 7.2 0 4.7-3.5 8-8.2 8.4l-3.8 1.8.8-3.3C5.9 16.6 4.5 14.1 4.5 11c0-4.1 3-7.2 7.5-7.2Z" /><path d="M8.5 11.5h7M8.5 14.5h4.4" /></svg>;
}

function ConversationPermissionIcon({ mode }: { mode: PermissionMode }) {
  if (mode === "read_only") return <svg className="new-conversation-permission-icon" viewBox="0 0 24 24" aria-hidden="true"><circle cx="10.5" cy="10.5" r="5" /><path d="m14.2 14.2 4.3 4.3M8.5 10.5h4" /></svg>;
  if (mode === "workspace_write") return <svg className="new-conversation-permission-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M4.5 6.5h15v11h-15zM8.5 10.5 11 13l-2.5 2.5M13.5 15.5h2.5" /></svg>;
  if (mode === "approval_required") return <svg className="new-conversation-permission-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M12 3.8 19 6.5v5.1c0 4.2-2.8 7.4-7 8.9-4.2-1.5-7-4.7-7-8.9V6.5l7-2.7Z" /><path d="M12 8.5v3.8M12 16h.01" /></svg>;
  return <svg className="new-conversation-permission-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M12 3.8 19 6.5v5.1c0 4.2-2.8 7.4-7 8.9-4.2-1.5-7-4.7-7-8.9V6.5l7-2.7Z" /><path d="m8.8 12 2.1 2.1 4.4-4.4" /></svg>;
}

function ProfileSelectIcon() {
  return <svg className="new-conversation-profile-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M5 5.5h14v13H5zM8 9h8M8 12h8M8 15h4" /></svg>;
}

function ProjectConfigIcon() {
  return <svg className="conversation-head-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="m12 3.5 8 3v5.6c0 4.3-3 7.8-8 9.4-5-1.6-8-5.1-8-9.4V6.5l8-3Z" /><path d="M9 11.2 11 13.2 15.2 9" /></svg>;
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

function ComposerShortcutIcon() {
  return <svg className="composer-shortcut-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M12 5v14M5 12h14" /></svg>;
}

function ShortcutCategoryIcon({ kind }: { kind: "prompt" | "command" }) {
  return kind === "prompt"
    ? <svg className="quick-tag-heading-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M5.5 6.5A2.5 2.5 0 0 1 8 4h8a2.5 2.5 0 0 1 2.5 2.5v6A2.5 2.5 0 0 1 16 15H11l-3.5 3v-3H8a2.5 2.5 0 0 1-2.5-2.5v-6Z" /><path d="M9 8.5h6M9 11.5h3.5" /></svg>
    : <svg className="quick-tag-heading-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M5.5 5.5h13v13h-13zM8.5 9.5 11 12l-2.5 2.5M13.5 14.5h2" /></svg>;
}

function ShortcutAddIcon() {
  return <svg className="quick-tag-control-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M12 5v14M5 12h14" /></svg>;
}

function SkillTagIcon() {
  return <svg className="quick-tag-heading-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M9.5 3.5h5l1 4.3 3.6 2.6-1.6 5.8-4.5.4-1 4.9h-5l-1-4.9-4.5-.4-1.6-5.8 3.6-2.6 1-4.3Z" /><circle cx="12" cy="9.6" r="1.6" /></svg>;
}

function ShortcutMoreIcon() {
  return <svg className="quick-tag-control-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M6.5 12h.01M12 12h.01M17.5 12h.01" /></svg>;
}

// 模块标题行的折叠指示箭头：展开时朝上（点击收起），折叠后朝下（点击展开），
// 旋转由 .quick-tag-group.collapsed 统一驱动，不单独写死方向。
function QuickTagChevron() {
  return <svg className="quick-tag-heading-chevron" viewBox="0 0 24 24" aria-hidden="true"><path d="m6 15 6-6 6 6" /></svg>;
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

function ComposerRunnerInfo({ runnerID, agentID, run, runLabel, permissionMode, usage, displayedModel, contextLabel, contextLevel, onShowUsage, readOnly, stopping, onStop }: { runnerID: string; agentID: AgentID; run: string; runLabel: string; permissionMode?: string; usage: ConversationUsageResponse | null; displayedModel: string; contextLabel: string; contextLevel: string; onShowUsage: () => void; readOnly: boolean; stopping: boolean; onStop: () => void }) {
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

  // 有可更新的新版本，以及当前 runner 是否支持应用内自动更新（缺省视为支持；跨端 runner
  // 如 wsl-local 由后端标为 false，此时提示"需手动更新"而不是给出点了必失败的更新按钮，
  // 也不会把"无法自动升级"误渲染成"已是最新版本"）。
  const hasUpdate = Boolean(updateInfo?.updateAvailable && !updateInfo.error);
  const autoUpdatable = updateInfo?.autoUpdatable !== false;
  const manualUpdateEnv = runner?.environment === "wsl" ? "WSL 内" : runner?.environment === "windows" ? "Windows 侧" : runner?.environment === "remote-linux" ? "远程服务器上" : "目标环境";
  const manualUpdateCommand = agentPath === "codex" ? "codex update" : "claude update";

  const canShowUsage = Boolean(run) || tool?.status === "ready";
  return (<>
    <span className="composer-status-group">
      <span className={`runner-inline ${runnerStatusClass}${run ? " run-active" : ""}`} title={tool?.reason} role={run ? "status" : undefined} aria-live={run ? "polite" : undefined}><i aria-hidden="true"></i><span>{run ? runLabel : tool?.status === "ready" ? `${toolName} ${tool.version}` : tool?.status === "updating" ? "更新中..." : tool?.reason || `${toolName} 不可用`}</span></span>
      {run && <button className="runner-stop" type="button" disabled={readOnly || stopping} onClick={onStop} title={readOnly ? "只读会话无法停止" : stopping ? "正在停止当前对话" : "停止当前对话"} aria-label={readOnly ? "只读会话无法停止" : stopping ? "正在停止当前对话" : "停止当前对话"}><span aria-hidden="true"></span>{stopping ? "停止中" : "停止"}</button>}
      {!run && tool?.status === "ready" && <button className="runner-inline-btn" disabled={checking || updating} onClick={() => void handleCheckUpdate()}>{checking ? "检查中..." : "检查更新"}</button>}
      {!run && !updating && hasUpdate && autoUpdatable && <button className="runner-inline-btn update-available" onClick={() => setShowConfirm(true)}>更新至 {updateInfo?.latestVersion}</button>}
      {!run && !updating && hasUpdate && !autoUpdatable && <span className="runner-inline-manual" title={`${toolName} 检测到新版本 ${updateInfo?.latestVersion ?? ""}，当前运行器暂不支持应用内自动更新，请在${manualUpdateEnv}手动执行 ${manualUpdateCommand}`}>发现新版本 {updateInfo?.latestVersion} · 需手动更新</span>}
      {!run && !updating && updateInfo && !hasUpdate && !updateInfo.error && <span className="runner-inline-uptodate" title={`${toolName} 已是最新版本`}>已是最新版本</span>}
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
type ConversationActivityPosition = { createdAt: string; id: string };
type ConversationActivityItem = { conversationId: string; events: Event[]; latestPosition?: ConversationActivityPosition | null; truncated: boolean };
type ConversationActivityResponse = { conversations: ConversationActivityItem[]; missingConversationIds: string[] };

function ConversationHistoryDialog({ conversations, activeID, busyID, deletingID, deleteAllBusy, close, activate, view, search, deleteOne, deleteAll, hasMore, loadingMore, loadMore }: {
  conversations: Conversation[];
  activeID: string;
  busyID: string;
  deletingID: string;
  deleteAllBusy: boolean;
  close: () => void;
  activate: (item: Conversation) => Promise<void>;
  view: (item: Conversation) => void;
  search: (query: string) => void;
  deleteOne: (item: Conversation) => void;
  deleteAll: () => void;
  hasMore: boolean;
  loadingMore: boolean;
  loadMore: () => void;
}) {
  const [query, setQuery] = useState("");
  const [selected, setSelected] = useState(0);
  useEffect(() => {
    const timer = window.setTimeout(() => search(query), 200);
    return () => window.clearTimeout(timer);
  }, [query, search]);
  useEffect(() => { setSelected(0); }, [conversations, query]);
  const select = (item: Conversation) => {
    if (busyID || item.id === activeID) return;
    // 自动编排会话是后台只读会话，不能切换为当前会话，始终以只读方式查看。
    if (item.isOrchestration) { view(item); return; }
    if (item.status === "running") { view(item); return; }
    void activate(item);
  };
  const keyDown = (event: React.KeyboardEvent<HTMLInputElement>) => {
    if (event.key === "Escape") { event.preventDefault(); close(); return; }
    if (event.key === "ArrowDown") { event.preventDefault(); setSelected((index) => Math.min(conversations.length - 1, index + 1)); return; }
    if (event.key === "ArrowUp") { event.preventDefault(); setSelected((index) => Math.max(0, index - 1)); return; }
    if (event.key === "Enter" && conversations[selected]) { event.preventDefault(); select(conversations[selected]); }
  };
  return <div className="backdrop history-backdrop" role="dialog" aria-modal="true" aria-labelledby="conversation-history-title"><section className="modal conversation-history"><header><div className="conversation-history-heading"><span className="conversation-history-mark"><HistoryIcon /></span><div><h2 id="conversation-history-title">会话历史</h2></div></div><button type="button" className="conversation-history-close" title="关闭" aria-label="关闭" onClick={close}><DialogCloseIcon /></button></header><div className="history-toolbar"><label className="history-search"><HistorySearchIcon /><input autoFocus value={query} onChange={(event) => setQuery(event.target.value)} onKeyDown={keyDown} placeholder="搜索会话标题或内容" /></label><span>{query ? `${conversations.length} 个匹配` : `${conversations.length} 个会话`}</span></div><div className="history-list">{conversations.length === 0 ? <p className="history-empty">没有匹配的会话</p> : conversations.map((item, index) => {
    const orchestration = item.isOrchestration === true;
    const runningElsewhere = item.status === "running" && item.id !== activeID;
    const state = busyID === item.id ? "切换中" : item.id === activeID ? "当前会话" : runningElsewhere ? "运行中" : orchestration ? "自动编排" : "";
    const isDeleting = deletingID === item.id;
    const rowDisabled = Boolean(busyID) || isDeleting || Boolean(deleteAllBusy);
    return <div key={item.id} className={`history-item ${busyID === item.id ? "activating" : ""} ${item.id === activeID ? "active" : ""} ${index === selected ? "selected" : ""} ${runningElsewhere ? "running" : ""}`} onMouseEnter={() => setSelected(index)}><button type="button" className="history-item-select" disabled={rowDisabled} onClick={() => runningElsewhere || orchestration ? view(item) : select(item)}><span className="history-item-main"><span className="history-item-title"><b>{item.title || "新会话"}</b><span className={`history-item-agent ${item.agentId === "codex" ? "codex" : "claude"}`}>{item.agentId === "codex" ? "Codex" : "Claude Code"}</span>{orchestration && <span className="history-item-orchestration">自动编排</span>}</span><small>{item.preview || "尚未发送消息"}</small></span><span className="history-item-meta">{state && <em>{state}</em>}<time>{formatHistoryTime(item.lastActivityAt)}</time></span></button>{!orchestration && <button type="button" className="history-item-delete" title="删除此会话" aria-label={`删除会话 ${item.title || "新会话"}`} disabled={rowDisabled} onClick={() => deleteOne(item)}><ConversationDeleteIcon /></button>}</div>;
  })}</div>{hasMore && <button className="secondary load-earlier-history" type="button" disabled={loadingMore} onClick={loadMore}>{loadingMore ? "加载中" : "加载更多会话"}</button>}<footer><span>{busyID ? "正在切换会话" : `${conversations.length} 条记录`}</span><div className="history-footer-actions"><button className="secondary history-delete-all" type="button" title="清除全部历史对话" disabled={Boolean(busyID) || deletingID !== "" || deleteAllBusy} onClick={deleteAll}>清除全部</button><button className="secondary" type="button" onClick={close}>关闭</button></div></footer></section></div>;
}

// formatToolVersion 把 CLI 上报的版本号归一化为展示文本：跨端 runner 的裸输出可能带
// "codex-cli " 前缀 / " (Claude Code)" 后缀 / 前导 "v"，统一剥除后补回 "v" 前缀；
// 空串返回空，由调用方回退到其它文案。
function formatToolVersion(version: string | undefined): string {
  const trimmed = (version ?? "")
    .replace(/^codex-cli\s+/i, "")
    .replace(/\s+\(Claude Code\)$/i, "")
    .replace(/^v/i, "")
    .trim();
  return trimmed ? `v${trimmed}` : "";
}

function NewConversationDialog({ runnerID, defaults, defaultsLoading, defaultsError, close, create }: { runnerID: string; defaults: AppPreferences; defaultsLoading: boolean; defaultsError: string; close: () => void; create: (agentId: AgentID, permissionMode: PermissionMode, profileID?: string) => Promise<void> }) {
  const permissionForAgent = (agent: AgentID): PermissionMode => agent === "codex" ? defaults.codexPermissionMode : defaults.claudePermissionMode;
  const [agentId, setAgentId] = useState<AgentID>(defaults.defaultAgentId);
  const [permissionMode, setPermissionMode] = useState<PermissionMode>(() => permissionForAgent(defaults.defaultAgentId));
  const [creating, setCreating] = useState(false);
  const [runner, setRunner] = useState<RunnerInfo | null>(null);
  const [runnerLoading, setRunnerLoading] = useState(true);
  const [runnerError, setRunnerError] = useState("");
  const [profiles, setProfiles] = useState<AgentProfile[]>([]);
  const [profileID, setProfileID] = useState("");
  const agentSelectedByUser = useRef(false);
  useEffect(() => {
    let cancelled = false;
    agentSelectedByUser.current = false;
    setRunnerLoading(true);
    setRunnerError("");
    api<RunnerInfo>(`/api/runners/${encodeURIComponent(runnerID)}/status`)
      .then((item) => { if (!cancelled) setRunner(item); })
      .catch((cause: unknown) => {
        if (cancelled) return;
        setRunner(null);
        // 状态接口 404（如 wsl-local 未注册）或超时时，把真实原因透出，而不是
        // 误提示成“CLI 未安装/未登录”。
        setRunnerError(cause instanceof Error ? cause.message : "无法读取 Runner 状态。");
      })
      .finally(() => { if (!cancelled) setRunnerLoading(false); });
    return () => { cancelled = true; };
  }, [runnerID]);
  useEffect(() => { api<AgentProfile[]>(`/api/runners/${runnerID}/agent-profiles`).then((items) => setProfiles(items)).catch(() => setProfiles([])); }, [runnerID]);
  const availableProfiles = useMemo(() => profiles.filter((profile) => profile.agentId === agentId && profile.enabled && profile.state === "active" && profile.authMode === "cli_managed"), [agentId, profiles]);
  useEffect(() => {
    if (availableProfiles.some((profile) => profile.id === profileID)) return;
    setProfileID("");
  }, [availableProfiles, profileID]);
  const codexStatus = runner?.codex;
  const codexAvailable = codexStatus?.status === "ready";
  const claudeStatus = runner?.claude;
  const claudeAvailable = claudeStatus?.status === "ready";
  const capabilitiesLoading = defaultsLoading || runnerLoading;
  const isAgentAvailable = (agent: AgentID) => agent === "codex" ? codexAvailable : claudeAvailable;
  useEffect(() => {
    if (capabilitiesLoading || defaultsError) return;
    const preferredAgent = defaults.defaultAgentId;
    const nextAgent = isAgentAvailable(preferredAgent)
      ? preferredAgent
      : isAgentAvailable("claude-code")
        ? "claude-code"
        : isAgentAvailable("codex")
          ? "codex"
          : preferredAgent;
    if (agentSelectedByUser.current && isAgentAvailable(agentId)) return;
    setAgentId(nextAgent);
    setPermissionMode(permissionForAgent(nextAgent));
  }, [agentId, capabilitiesLoading, claudeAvailable, codexAvailable, defaults.defaultAgentId, defaultsError]);
  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => { if (event.key === "Escape" && !creating) close(); };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [close, creating]);
  const selectAgent = (next: AgentID) => { agentSelectedByUser.current = true; setAgentId(next); setProfileID(""); setPermissionMode(permissionForAgent(next)); };
  const submit = async () => { if (defaultsError || capabilitiesLoading || !isAgentAvailable(agentId)) return; setCreating(true); try { await create(agentId, permissionMode, profileID || undefined); } finally { setCreating(false); } };
  const codex = agentId === "codex";
  const selectedAgentName = codex ? "Codex" : "Claude Code";
  const fallbackReason = !defaultsError && !capabilitiesLoading && !isAgentAvailable(defaults.defaultAgentId) && (claudeAvailable || codexAvailable)
    ? `${defaults.defaultAgentId === "codex" ? "Codex" : "Claude Code"} 当前不可用，已选择可用工具。`
    : "";
  const unavailableReason = agentId === "codex" ? codexStatus?.reason : claudeStatus?.reason;
  const permissionOptions: Array<{ mode: PermissionMode; title: string; detail: string }> = codex
    ? [{ mode: "read_only", title: "仅分析", detail: "只读检查，不修改项目文件。" }, { mode: "workspace_write", title: "项目内执行", detail: "可在当前项目范围内读写和执行。" }, { mode: "full_control", title: "完全控制", detail: "直接执行命令，不受沙箱限制。" }]
    : [{ mode: "approval_required", title: "默认权限", detail: "终端命令执行前需要确认。" }, { mode: "full_control", title: "完全控制", detail: "直接执行命令，不再等待确认。" }];
  return <div className="backdrop new-conversation-backdrop" role="dialog" aria-modal="true" aria-labelledby="new-conversation-title" onClick={(event) => { if (event.target === event.currentTarget && !creating) close(); }}>
    <section className="modal new-conversation-dialog">
      <header>
        <div className="new-conversation-dialog-heading"><span className="new-conversation-dialog-mark"><NewConversationDialogIcon /></span><div><h2 id="new-conversation-title">创建新会话</h2></div></div>
        <button className="new-conversation-dialog-close" type="button" title="关闭" aria-label="关闭" disabled={creating} onClick={close}><DialogCloseIcon /></button>
      </header>
      <div className="new-conversation-dialog-body">
        {capabilitiesLoading ? <div className="new-conversation-loading"><span></span>正在检查 CLI 工具和默认设置...</div> : defaultsError ? <p className="new-conversation-error">无法读取默认设置：{defaultsError}</p> : <>
          {fallbackReason && <p className="new-conversation-notice">{fallbackReason}</p>}
          <section className="new-conversation-section" aria-labelledby="new-conversation-agent-label">
            <div className="new-conversation-section-heading"><div><span>01</span><h3 id="new-conversation-agent-label">选择 CLI 工具</h3></div></div>
            <div className="new-conversation-agent-grid" role="radiogroup" aria-label="CLI 工具">
              {(["claude-code", "codex"] as AgentID[]).map((agent) => {
                const available = isAgentAvailable(agent);
                const selected = agentId === agent;
                const status = agent === "codex" ? codexStatus : claudeStatus;
                const name = agent === "codex" ? "Codex" : "Claude Code";
                const detail = agent === "codex" ? "OpenAI CLI" : "Anthropic CLI";
                const subtitle = formatToolVersion(status?.version) || detail;
                return <button key={agent} type="button" className={`new-conversation-agent-card${selected ? " selected" : ""}`} role="radio" aria-checked={selected} disabled={!available || creating} title={!available ? status?.reason || `${name} 不可用` : name} onClick={() => selectAgent(agent)}>
                  <span className={`new-conversation-agent-mark ${agent === "codex" ? "codex" : "claude"}`}><AgentToolIcon agent={agent} /></span><span className="new-conversation-agent-copy"><b>{name}</b><small>{subtitle}</small></span>{agent === defaults.defaultAgentId && <em>默认</em>}<span className={`new-conversation-agent-state ${available ? "ready" : "unavailable"}`}>{available ? "已就绪" : "不可用"}</span>
                </button>;
              })}
            </div>
            {!claudeAvailable && !codexAvailable && <p className="new-conversation-error inline">当前 Runner 没有可用的 CLI 工具。{runnerError || unavailableReason || "请检查 CLI 安装与登录状态。"}</p>}
          </section>
          {availableProfiles.length > 0 && <label className="new-conversation-profile-select"><span><ProfileSelectIcon />配置档案 <small>可选</small></span><select value={profileID} disabled={creating} onChange={(event) => setProfileID(event.target.value)}><option value="">使用 CLI 当前登录配置</option>{availableProfiles.map((profile) => <option key={profile.id} value={profile.id}>{profile.name}{profile.model ? ` (${profile.model})` : ""}</option>)}</select></label>}
          <section className="new-conversation-section new-conversation-permission-section" aria-labelledby="new-conversation-permission-label">
            <div className="new-conversation-section-heading"><div><span>02</span><h3 id="new-conversation-permission-label">执行权限</h3></div><small>{selectedAgentName}</small></div>
            <div className="new-conversation-permission-list" role="radiogroup" aria-label={`${selectedAgentName} 执行权限`}>
              {permissionOptions.map((option) => <button key={option.mode} type="button" className={`new-conversation-permission-card${permissionMode === option.mode ? " selected" : ""}${option.mode === "full_control" ? " elevated" : ""}`} role="radio" aria-checked={permissionMode === option.mode} disabled={creating} onClick={() => setPermissionMode(option.mode)}><span className="new-conversation-permission-mark"><ConversationPermissionIcon mode={option.mode} /></span><span><b>{option.title}</b><small>{option.detail}</small></span><i aria-hidden="true"></i></button>)}
            </div>
          </section>
        </>}
      </div>
      <footer><span className="new-conversation-summary">{capabilitiesLoading || defaultsError ? "" : `${selectedAgentName} · ${permissionOptions.find((option) => option.mode === permissionMode)?.title || "默认权限"}`}</span><button className="secondary" type="button" disabled={creating} onClick={close}>取消</button><button className="primary" type="button" disabled={Boolean(defaultsError) || capabilitiesLoading || creating || !isAgentAvailable(agentId)} onClick={() => void submit()}>{creating ? "创建中..." : "创建会话"}</button></footer>
    </section>
  </div>;
}

function FullControlConfirmationDialog({ close, confirm, changing, isCodex }: { close: () => void; confirm: () => Promise<void>; changing: boolean; isCodex?: boolean }) {
  const agentName = isCodex ? "Codex" : "Claude";
  return <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="full-control-title" onClick={(e) => { if (e.target === e.currentTarget) close(); }}><section className="modal permission-dialog"><header><div><label>{agentName.toUpperCase()} PERMISSION</label><h2 id="full-control-title">切换为完全控制</h2></div><button title="关闭" disabled={changing} onClick={close}>x</button></header><p className="permission-confirmation">{isCodex ? `完全控制会允许 Codex 绕过沙箱限制，直接执行所有命令，不再受项目目录约束。` : `完全控制会允许 Claude 在当前项目中直接执行所有命令，不再等待确认。`}</p><footer><button className="secondary" disabled={changing} onClick={close}>取消</button><button className="primary danger" disabled={changing} onClick={() => void confirm()}>{changing ? "切换中" : "确认切换"}</button></footer></section></div>;
}

const MessageCard = memo(function MessageCard({ message, agentID, fail }: { message: Message; agentID: AgentID; fail: (message: string) => void }) {
  const isUser = message.role === "user";
  const agentName = agentID === "codex" ? "Codex" : "Claude";
  const [copied, setCopied] = useState(false);
  const copiedTimer = useRef<number | null>(null);

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
  return <article className="error-card"><div className="error-card__header"><span className="error-card__icon" aria-hidden="true">!</span><div className="error-card__heading"><b>{item.title}</b><time>{formatTime(item.createdAt)}</time></div></div><div className="error-card__body"><pre aria-label={`${item.title}详情`}>{item.detail}</pre></div>{item.taskId && <footer className="error-card__footer"><button type="button" className="secondary" onClick={() => onViewTask(item.taskId!)}>查看任务详情</button></footer>}</article>;
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

function ConversationTabStrip({ state, conversations, workspaceLabels, select, close, create, openHistory }: {
  state: ConversationTabsState;
  conversations: Conversation[];
  workspaceLabels: Record<string, string>;
  select: (conversationId: string) => void;
  close: (conversationId: string) => void;
  create: () => void;
  openHistory: () => void;
}) {
  const byID = new Map(conversations.map((conversation) => [conversation.id, conversation]));
  const navigateConversationTabs = (event: React.KeyboardEvent<HTMLElement>) => {
    if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return;
    const tabs = Array.from(event.currentTarget.querySelectorAll<HTMLButtonElement>('[role="tab"]'));
    const currentIndex = tabs.indexOf(document.activeElement as HTMLButtonElement);
    if (currentIndex < 0 || tabs.length === 0) return;
    event.preventDefault();
    const nextIndex = event.key === "Home" ? 0 : event.key === "End" ? tabs.length - 1 : (currentIndex + (event.key === "ArrowRight" ? 1 : -1) + tabs.length) % tabs.length;
    const nextTab = tabs[nextIndex];
    nextTab.focus();
    const conversationID = nextTab.dataset.conversationId;
    if (conversationID) select(conversationID);
  };
  return <nav className="conversation-tab-strip" aria-label="已打开会话">
    <div className="conversation-tab-list" role="tablist" onKeyDown={navigateConversationTabs}>
      {state.openConversationIds.map((id) => {
        const conversation = byID.get(id);
        const active = state.activeConversationId === id;
        const unread = state.unreadConversationIds.includes(id);
        const label = conversation?.title || "会话";
        const workspaceLabel = workspaceLabels[id] || "工作区";
        const stateClass = conversation?.status === "running" ? "running" : conversation?.status === "archived" ? "archived" : "idle";
        return <div className={`conversation-tab ${active ? "active" : ""} ${stateClass}`} role="presentation" key={id}>
          <button data-conversation-id={id} id={`conversation-tab-${id}`} type="button" role="tab" tabIndex={active ? 0 : -1} aria-controls="conversation-panel" aria-selected={active} title={`${label} - ${workspaceLabel}`} onClick={() => select(id)}>
            <span className="conversation-tab-state" aria-hidden="true" />
            <span className="conversation-tab-copy"><span className="conversation-tab-label">{label}</span><span className="conversation-tab-workspace">{workspaceLabel}</span></span>
            {unread && <span className="conversation-tab-unread" aria-label="有未读活动" />}
            {conversation && <span className="conversation-tab-agent">{conversation.agentId === "codex" ? "Codex" : "Claude Code"}</span>}
          </button>
          <button type="button" className="conversation-tab-close" title={`关闭 ${label}`} aria-label={`关闭 ${label}`} onClick={() => close(id)}>x</button>
        </div>;
      })}
    </div>
    <button type="button" className="conversation-tab-history" title="会话历史" aria-label="会话历史" onClick={openHistory}><HistoryIcon /><span>历史</span></button>
    <button type="button" className="conversation-tab-add" title="新建会话" aria-label="新建会话" disabled={state.openConversationIds.length >= MAX_OPEN_CONVERSATION_TABS} onClick={create}>+</button>
  </nav>;
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

  return <><div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="shortcut-editor-title"><section className={`modal shortcut-editor${isCommand ? " command-editor" : " prompt-editor"}`}><header><div className="shortcut-editor-heading"><span className="shortcut-editor-mark"><ShortcutCategoryIcon kind={isCommand ? "command" : "prompt"} /></span><div><label>{isCommand ? "COMMON COMMAND" : "COMMON PROMPT"}</label><h2 id="shortcut-editor-title">{shortcut ? `编辑${isCommand ? "命令" : "提示词"}` : `新增${isCommand ? "命令" : "提示词"}`}</h2></div></div><button type="button" className="shortcut-editor-close" title="关闭" aria-label="关闭" disabled={busy} onClick={close}><DialogCloseIcon /></button></header><form onSubmit={(event) => void save(event)}><div className="shortcut-editor-body"><label className="shortcut-editor-field"><span>名称 <small>最多 64 个字符</small></span><input autoFocus required maxLength={64} value={name} onChange={(event) => setName(event.target.value)} placeholder={isCommand ? "例如：清空终端" : "例如：审查当前改动"} /></label><label className="shortcut-editor-field"><span>{isCommand ? "命令内容" : "提示词内容"}</span><textarea required maxLength={12000} value={template} onChange={(event) => setTemplate(event.target.value)} placeholder={isCommand ? "例如：/compact" : "描述希望 Claude 在当前项目完成的工作"} /></label>{shortcut && <label className="shortcut-enabled"><input type="checkbox" checked={enabled} onChange={(event) => setEnabled(event.target.checked)} /><span>启用此项</span></label>}{isCommand && <p className="shortcut-editor-hint">以 <code>/</code> 开头的内容会作为斜杠命令直接发送给 CLI 执行（如 <code>/compact</code>）；其中 <code>/clear</code> 由应用本地清空会话（新建空白会话，旧会话保留在历史中）。其余内容作为普通命令提示发送。执行时会遵循当前会话的权限设置。</p>}</div><footer>{shortcut && <button type="button" className="danger-text" disabled={busy} onClick={() => void remove()}>删除</button>}<div className="shortcut-editor-footer-actions"><button type="button" className="secondary" disabled={busy} onClick={close}>取消</button><button className="primary" disabled={busy}>{busy ? "保存中" : "保存"}</button></div></footer></form></section></div>{pendingConfirm && createPortal(<ConfirmDialog title={pendingConfirm.title} message={pendingConfirm.message} danger={pendingConfirm.danger} onConfirm={pendingConfirm.onConfirm} onCancel={pendingConfirm.onCancel} />, document.body)}</>;
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
    {timeline.map((item) => <div key={item.id} className="timeline-item" data-timeline-kind={item.kind} data-user-message-id={item.kind === "message" && item.message.role === "user" ? item.message.id : undefined} ref={item.kind === "message" && item.message.role === "user" ? (element) => { registerUserMessageElement(item.message.id, element); } : undefined}><div className={`timeline-entry ${item.kind === "message" ? "message-entry" : item.kind}`}>{item.kind === "message" ? <MessageCard message={item.message} agentID={agentID} fail={fail} /> : item.kind === "tool" ? <ToolCard action={item.action} resolving={resolving} decide={decide} /> : item.kind === "system" ? <SystemCard system={item.system} /> : <ErrorCard item={item} projectId={projectId} onViewTask={onViewTask} />}</div>{item.kind === "message" && item.message.role === "user" && item.message.runId && executionByRun.get(item.message.runId) && <AgentExecutionCard execution={executionByRun.get(item.message.runId)!} open={() => onOpenExecution(item.message.runId!)} />}</div>)}
    {executionByRun && Array.from(executionByRun.values()).filter((execution) => !anchoredExecutionRunIDs.has(execution.runId)).map((execution) => <AgentExecutionCard key={execution.runId} execution={execution} open={() => onOpenExecution(execution.runId)} />)}
  </>;
});

// ---- 主组件 ---------------------------------------------------------------

export default function ConversationPage() {
  const { projectId, conversationId: urlConversationId } = useParams<{ projectId: string; conversationId: string }>();
  const navigate = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();
  const { api: projectApi, setError, getConversationDraft, saveConversationDraft, flushConversationDraft } = useProjectContext();
  const { appPreferences, appPreferencesLoading, appPreferencesError } = useUIPreferences();
  const { project } = useOutletContext<ProjectLayoutOutletContext>();
  const fail = setError;
  // 弹窗控制辅助函数 — 使用不可变模式创建新的 URLSearchParams
  const closeHistory = () => setSearchParams((prev) => { const next = new URLSearchParams(prev); next.delete("history"); return next; });
  const closeNewConversation = () => setSearchParams((prev) => { const next = new URLSearchParams(prev); next.delete("new"); return next; });
  const closeUsage = () => setSearchParams((prev) => { const next = new URLSearchParams(prev); next.delete("usage"); return next; });
  const closeAgentExecution = () => setSearchParams((prev) => { const next = new URLSearchParams(prev); next.delete("execution"); return next; });
  const openUsage = () => setSearchParams((prev) => { const next = new URLSearchParams(prev); next.set("usage", "true"); return next; });
  const openNewConversationParam = () => setSearchParams((prev) => { const next = new URLSearchParams(prev); next.set("new", "true"); return next; });
  const openAiConfig = () => {
    if (readOnlyConversation) return;
    setSearchParams((prev) => { const next = new URLSearchParams(prev); next.set("config", "true"); return next; });
  };
  const closeAiConfig = () => setSearchParams((prev) => { const next = new URLSearchParams(prev); next.delete("config"); return next; });
  const openExecutionParam = useCallback((runId: string) => setSearchParams((prev) => { const next = new URLSearchParams(prev); next.set("execution", runId); return next; }), [setSearchParams]);

  // 对话核心状态
  const [conversation, setConversation] = useState<Conversation | null>(null);
  const [conversationWorkspaces, setConversationWorkspaces] = useState<ConversationWorkspace[]>([]);
  const [conversationWorkspaceLabels, setConversationWorkspaceLabels] = useState<Record<string, string>>({});
  const [archiveWorkspaceID, setArchiveWorkspaceID] = useState("");
  const [workspaceBusy, setWorkspaceBusy] = useState(false);
  const [conversationHistory, setConversationHistory] = useState<Conversation[]>([]);
  const [historyQuery, setHistoryQuery] = useState("");
  const [conversationHistoryCursor, setConversationHistoryCursor] = useState("");
  const [conversationTabs, setConversationTabs] = useState<ConversationTabsState>({ openConversationIds: [], activeConversationId: null, readPositions: {}, latestPositions: {}, unreadConversationIds: [] });
  const [loadingMoreConversationHistory, setLoadingMoreConversationHistory] = useState(false);
  const [messages, setMessages] = useState<Message[]>([]);
  const [events, setEvents] = useState<Event[]>([]);

  useEffect(() => {
    if (!conversation) {
      setConversationWorkspaces([]);
      return;
    }
    let cancelled = false;
    void projectApi<ConversationWorkspace[]>(`/api/conversations/${conversation.id}/workspaces`)
      .then((items) => {
        if (cancelled) return;
        setConversationWorkspaces(items);
        const active = items.find((item) => item.active && item.state === "ready");
        if (active) setConversationWorkspaceLabels((current) => ({ ...current, [conversation.id]: active.mode === "isolated_worktree" ? `工作区 #${active.generation}` : "项目工作区" }));
      })
      .catch((cause) => { if (!cancelled) fail(cause instanceof Error ? cause.message : "无法读取会话工作区"); });
    return () => { cancelled = true; };
  }, [conversation?.id, fail, projectApi]);

  useEffect(() => {
    const openIDs = conversationTabs.openConversationIds;
    if (openIDs.length === 0) return;
    let cancelled = false;
    void Promise.all(openIDs.map(async (conversationID) => {
      try {
        const items = await projectApi<ConversationWorkspace[]>(`/api/conversations/${conversationID}/workspaces`);
        const active = items.find((item) => item.active && item.state === "ready");
        return [conversationID, active ? (active.mode === "isolated_worktree" ? `工作区 #${active.generation}` : "项目工作区") : "工作区"] as const;
      } catch {
        return [conversationID, "工作区"] as const;
      }
    })).then((entries) => {
      if (cancelled) return;
      setConversationWorkspaceLabels((current) => ({ ...current, ...Object.fromEntries(entries) }));
    });
    return () => { cancelled = true; };
  }, [conversationTabs.openConversationIds.join(","), projectApi]);

  useEffect(() => {
    const candidates = conversationWorkspaces.filter((item) => item.mode === "isolated_worktree" && item.state === "ready" && !item.active);
    if (!candidates.some((item) => item.id === archiveWorkspaceID)) {
      setArchiveWorkspaceID(candidates[0]?.id || "");
    }
  }, [archiveWorkspaceID, conversationWorkspaces]);

  const createIsolatedWorkspace = async () => {
    if (!conversation || workspaceBusy || run || clearing || stopping || readOnlyConversation) return;
    setWorkspaceBusy(true);
    try {
      const workspace = await projectApi<ConversationWorkspace>(`/api/conversations/${conversation.id}/workspaces`, { method: "POST" });
      if (!workspace) return;
      await projectApi(`/api/conversations/${conversation.id}/workspaces/${workspace.id}/activate`, { method: "POST" });
      setConversationWorkspaces((items) => [
        { ...workspace, active: true },
        ...items.filter((item) => item.id !== workspace.id).map((item) => ({ ...item, active: false })),
      ]);
      setConversationWorkspaceLabels((current) => ({ ...current, [conversation.id]: `工作区 #${workspace.generation}` }));
    } catch (cause) {
      fail(cause instanceof Error ? cause.message : "无法创建工作区");
    } finally {
      setWorkspaceBusy(false);
    }
  };

  const activateWorkspace = async (workspace: ConversationWorkspace) => {
    if (!conversation || workspaceBusy || workspace.active || run || clearing || stopping || readOnlyConversation) return;
    setWorkspaceBusy(true);
    try {
      await projectApi(`/api/conversations/${conversation.id}/workspaces/${workspace.id}/activate`, { method: "POST" });
      setConversationWorkspaces((items) => items.map((item) => ({ ...item, active: item.id === workspace.id })));
      setConversationWorkspaceLabels((current) => ({ ...current, [conversation.id]: workspace.mode === "isolated_worktree" ? `工作区 #${workspace.generation}` : "项目工作区" }));
    } catch (cause) {
      fail(cause instanceof Error ? cause.message : "无法切换会话工作区");
    } finally {
      setWorkspaceBusy(false);
    }
  };

  const archiveWorkspace = async (workspace: ConversationWorkspace) => {
    if (!conversation || workspaceBusy || workspace.active || workspace.mode !== "isolated_worktree" || run || clearing || stopping || readOnlyConversation) return;
    if (!window.confirm(`移除工作区 #${workspace.generation}？仅当没有未提交变更且专用分支已合并时才能移除。该目录和专用分支将被移除，历史运行记录会保留。`)) return;
    setWorkspaceBusy(true);
    try {
      await projectApi(`/api/conversations/${conversation.id}/workspaces/${workspace.id}`, { method: "DELETE" });
      setConversationWorkspaces((items) => items.map((item) => item.id === workspace.id ? { ...item, state: "archived", archivedAt: new Date().toISOString() } : item));
      setArchiveWorkspaceID((current) => current === workspace.id ? "" : current);
    } catch (cause) {
      fail(cause instanceof Error ? cause.message : "无法移除工作区");
    } finally {
      setWorkspaceBusy(false);
    }
  };
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
	const readOnlyConversation = searchParams.get("readonly") === "true" || conversation?.isOrchestration === true;
  const showNewConversation = searchParams.get("new") === "true";
  const showUsage = searchParams.get("usage") === "true";
  const showAgentExecution = searchParams.get("execution");
  const showAiConfig = searchParams.get("config") === "true";
  const [showMobileActions, setShowMobileActions] = useState(false);
  const [showMobileShortcuts, setShowMobileShortcuts] = useState(false);
  const [showFullControlConfirmation, setShowFullControlConfirmation] = useState(false);
  const [showPermissionMenu, setShowPermissionMenu] = useState(false);
  // 预约发送：内容暂存到当前对话（含子代理）彻底空闲后再真正发出。
  // 用 ref 保存并发安全的待发内容 + 一个 state 驱动 UI。
  const [pendingSendContent, setPendingSendContent] = useState<string | null>(null);
  const [showSendMenu, setShowSendMenu] = useState(false);
  const pendingSendRef = useRef<string | null>(null);
  const pendingSendConversationRef = useRef<string | null>(null);
  const pendingSendRequestIDRef = useRef<string | null>(null);
  // 预约自动发送：仅在“空闲状态刚成立”时挂起一次延迟触发，避免 run.* 事件后的
  // reload 回包与后端收尾尚未完成时立即 POST 撞上瞬时 4xx，导致内容被回退到输入框。
  const scheduledSendTimerRef = useRef<number | null>(null);
  const wasScheduledIdleRef = useRef(false);
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

  useEffect(() => {
    if (!showMobileShortcuts) return;
    const closeOnOutsideClick = (event: MouseEvent) => {
      const target = event.target as HTMLElement;
      if (target.closest(".mobile-shortcut-menu") || target.closest(".composer-mobile-shortcut-toggle")) return;
      setShowMobileShortcuts(false);
    };
    const closeOnEscape = (event: KeyboardEvent) => {
      if (event.key === "Escape") setShowMobileShortcuts(false);
    };
    document.addEventListener("mousedown", closeOnOutsideClick);
    document.addEventListener("keydown", closeOnEscape);
    return () => {
      document.removeEventListener("mousedown", closeOnOutsideClick);
      document.removeEventListener("keydown", closeOnEscape);
    };
  }, [showMobileShortcuts]);

  useEffect(() => { setShowMobileShortcuts(false); }, [conversation?.id]);

  const [activatingConversation, setActivatingConversation] = useState("");
  const [deletingConversation, setDeletingConversation] = useState("");
  const [deleteAllConversationsBusy, setDeleteAllConversationsBusy] = useState(false);
  const [usage, setUsage] = useState<ConversationUsageResponse | null>(null);
  const [shortcuts, setShortcuts] = useState<Shortcut[]>([]);
  const [skills, setSkills] = useState<Skill[]>([]);
  const [skillsLoading, setSkillsLoading] = useState(false);
  // 技能按来源分组后，各来源组的折叠状态。插件组（系统/官方自带，含大量 marketplace 样板）默认折叠。
  const [skillGroupsCollapsed, setSkillGroupsCollapsed] = useState<Partial<Record<Skill["source"], boolean>>>({ plugin: true });
  // 侧栏模块（常用提示词/常用命令/技能/任务队列）的折叠状态，按项目持久化。
  // state 记录它属于哪个项目，持久化 effect 只在项目一致时才写回，
  // 避免“切换项目”的那一帧把上一项目的折叠状态写进新项目。
  const [conversationPanelsState, setConversationPanelsState] = useState<{ projectId: string; collapsed: ConversationPanelsState }>(() => ({ projectId: projectId || "", collapsed: readConversationPanels(projectId || "") }));
  const conversationPanels = conversationPanelsState.collapsed;
  useEffect(() => {
    if (!projectId) return;
    setConversationPanelsState((current) => current.projectId === projectId ? current : { projectId, collapsed: readConversationPanels(projectId) });
  }, [projectId]);
  useEffect(() => {
    if (!projectId || conversationPanelsState.projectId !== projectId) return;
    writeConversationPanels(projectId, conversationPanelsState.collapsed);
  }, [conversationPanelsState, projectId]);
  const toggleConversationPanel = useCallback((key: ConversationPanelKey) => {
    setConversationPanelsState((current) => ({ ...current, collapsed: { ...current.collapsed, [key]: !current.collapsed[key] } }));
  }, []);
  const [shortcutEditor, setShortcutEditor] = useState<ShortcutEditorState | null>(null);
  const [shortcutVariables, setShortcutVariables] = useState<{ shortcut: Shortcut; variables: Record<string, string> } | null>(null);
  const [shortcutBusy, setShortcutBusy] = useState("");
  const [hasMoreHistory, setHasMoreHistory] = useState(false);
  const [hasMoreMessageHistory, setHasMoreMessageHistory] = useState(false);
  const [historyCursor, setHistoryCursor] = useState("");
  const [pendingConfirm, setPendingConfirm] = useState<{ title: string; message: React.ReactNode; confirmLabel?: string; danger?: boolean; className?: string; icon?: React.ReactNode; onConfirm: () => void; onCancel: () => void } | null>(null);
  const [pendingConfirmBusy, setPendingConfirmBusy] = useState(false);
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
  const pendingConfirmBusyRef = useRef(false);
  const historyIndex = useRef<number | null>(null);
  const draftBeforeHistory = useRef("");
  const textRef = useRef("");
  const finishedRunIds = useRef(new Set<string>());
  const usageRequestVersion = useRef(0);
  const usageConversationID = useRef<string | null>(null);
  const conversationRef = useRef<Conversation | null>(null);
  conversationRef.current = conversation;
  textRef.current = text;
  const runRef = useRef("");
  runRef.current = run;
  const conversationTransitionRef = useRef(false);
  const conversationRouteVersion = useRef(0);
  const conversationHistoryRequestVersion = useRef(0);
  const conversationTabsRef = useRef<ConversationTabsState>(conversationTabs);
  conversationTabsRef.current = conversationTabs;
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

  useEffect(() => {
    const restored = readConversationTabs(projectId || "");
    conversationTabsRef.current = restored;
    setConversationTabs(restored);
  }, [projectId]);

  const rememberConversationTab = useCallback((conversationID: string) => {
    if (!projectId) return false;
    const next = openConversationTab(conversationTabsRef.current, conversationID);
    if (!next) return false;
    // A deliberate history/direct-link open is an explicit request to restore
    // a conversation that was previously closed in this browser window. Only
    // clear the marker after the tab was actually admitted.
    clearConversationTabClosed(projectId, conversationID);
    conversationTabsRef.current = next;
    writeConversationTabs(projectId, next);
    setConversationTabs(next);
    return true;
  }, [projectId]);

  const selectConversationTab = useCallback((conversationID: string) => {
    if (!rememberConversationTab(conversationID)) {
      fail(`每个项目最多同时打开 ${MAX_OPEN_CONVERSATION_TABS} 个会话，请先关闭一个 Tab。`);
      return;
    }
    if (projectId && conversationID !== urlConversationId) navigate(`/projects/${projectId}/conversations/${conversationID}`);
  }, [fail, navigate, projectId, rememberConversationTab, urlConversationId]);

  const closeConversationTabFromUI = useCallback((conversationID: string) => {
    if (!projectId) return;
    markConversationTabClosed(projectId, conversationID);
    const next = closeConversationTab(conversationTabsRef.current, conversationID);
    conversationTabsRef.current = next;
    writeConversationTabs(projectId, next);
    setConversationTabs(next);
    if (conversationID !== urlConversationId) return;
    const nextID = next.activeConversationId;
    navigate(nextID ? `/projects/${projectId}/conversations/${nextID}` : `/projects/${projectId}/conversations`);
  }, [navigate, projectId, urlConversationId]);

  const replaceConversationTab = useCallback((closedConversationID: string, nextConversationID: string) => {
    if (!projectId) return;
    markConversationTabClosed(projectId, closedConversationID);
    const closed = closeConversationTab(conversationTabsRef.current, closedConversationID);
    const next = openConversationTab(closed, nextConversationID);
    if (!next) return;
    conversationTabsRef.current = next;
    writeConversationTabs(projectId, next);
    setConversationTabs(next);
  }, [projectId]);

  const removeUnavailableConversationTabs = useCallback((conversationIDs: string[]) => {
    if (!projectId || conversationIDs.length === 0) return;
    const unavailable = new Set(conversationIDs);
    const previous = conversationTabsRef.current;
    let next = previous;
    conversationIDs.forEach((conversationID) => { next = closeConversationTab(next, conversationID); });
    if (next !== previous) {
      conversationTabsRef.current = next;
      writeConversationTabs(projectId, next);
      setConversationTabs(next);
    }
    if (urlConversationId && unavailable.has(urlConversationId)) {
      navigate(next.activeConversationId ? `/projects/${projectId}/conversations/${next.activeConversationId}` : `/projects/${projectId}/conversations`, { replace: true });
    }
  }, [navigate, projectId, urlConversationId]);

  const markActiveConversationTabRead = useCallback(() => {
    const conversationID = conversationRef.current?.id;
    if (!projectId || !conversationID) return;
    const next = markConversationTabRead(conversationTabsRef.current, conversationID);
    if (next === conversationTabsRef.current) return;
    conversationTabsRef.current = next;
    writeConversationTabs(projectId, next);
    setConversationTabs(next);
  }, [projectId]);

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
    const previousConversationID = conversationRef.current?.id;
    if (projectId && previousConversationID && previousConversationID !== next.id) flushConversationDraft(projectId, previousConversationID);
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

  const reopenArchivedConversation = useCallback(async (conversationID: string, signal?: AbortSignal) => {
    for (let attempt = 0; attempt < 8; attempt++) {
      try {
        const activated = await projectApi<Conversation>(`/api/conversations/${conversationID}/activate`, { method: "POST", signal });
        if (activated.status !== "archived") return activated;
      } catch (cause) {
        const status = typeof cause === "object" && cause !== null ? (cause as { status?: unknown }).status : undefined;
        if (status !== 409 || attempt === 7) throw cause;
      }
      if (signal?.aborted) throw new DOMException("Aborted", "AbortError");
      await new Promise((resolve) => setTimeout(resolve, 250));
    }
    throw new Error("会话仍在停止，请稍后重试");
  }, [projectApi]);

  const syncConversationActivity = useCallback(async () => {
    if (!projectId) return;
    const state = conversationTabsRef.current;
    if (state.openConversationIds.length === 0) return;
    const data = await projectApi<ConversationActivityResponse>(`/api/projects/${projectId}/conversations/activity`, {
      method: "POST",
      body: JSON.stringify({ cursors: state.openConversationIds.map((conversationId) => ({ conversationId, after: state.latestPositions[conversationId] || state.readPositions[conversationId] })) }),
    });
    removeUnavailableConversationTabs(data.missingConversationIds || []);
    let next = conversationTabsRef.current;
    data.conversations.forEach((item) => {
      const isActiveAtBottom = item.conversationId === conversationRef.current?.id && userNearBottom.current;
      next = recordConversationActivity(next, item.conversationId, item.latestPosition || null, item.events.length > 0, isActiveAtBottom);
    });
    if (next === conversationTabsRef.current) return;
    conversationTabsRef.current = next;
    writeConversationTabs(projectId, next);
    setConversationTabs(next);
  }, [projectApi, projectId, removeUnavailableConversationTabs]);

  // Background tabs share a bounded incremental activity poll instead of a
  // WebSocket per tab. The selected tab still reloads complete persisted history.
  useEffect(() => {
    if (!projectId || conversationTabs.openConversationIds.length === 0) return;
    const refreshBackgroundTabs = () => {
      void syncConversationActivity().catch(() => undefined);
      if (!historyQuery.trim()) void requestConversationHistory("").catch(() => undefined);
    };
    refreshBackgroundTabs();
    const timer = window.setInterval(refreshBackgroundTabs, 8_000);
    return () => window.clearInterval(timer);
  }, [conversationTabs.openConversationIds.length, historyQuery, projectId, requestConversationHistory, syncConversationActivity]);

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
    async function loadConversation() {
      try {
        // 直接链接必须按 ID 查询，不能依赖历史列表的当前分页结果。
		if (urlConversationId) {
			let detail = await projectApi<{ conversation: Conversation & { projectId: string } }>(`/api/conversations/${urlConversationId}?limit=1`, { signal: abort.signal });
			if (!isCurrentRoute()) return;
			if (detail.conversation.projectId !== projectId) {
              removeUnavailableConversationTabs([urlConversationId]);
              return;
            }
			// A cleared history entry is readable but archived. Reopen it before
			// rendering the composer so the next message resumes its native session.
			if (detail.conversation.status === "archived" && !detail.conversation.isOrchestration) {
				const activated = await reopenArchivedConversation(detail.conversation.id, abort.signal);
				if (!isCurrentRoute()) return;
				detail = { conversation: { ...activated, projectId: detail.conversation.projectId } };
			}
			if (!rememberConversationTab(detail.conversation.id)) {
              fail(`每个项目最多同时打开 ${MAX_OPEN_CONVERSATION_TABS} 个会话，请先关闭一个 Tab。`);
				const fallbackID = conversationTabsRef.current.activeConversationId;
				if (fallbackID && fallbackID !== detail.conversation.id) navigate(`/projects/${projectId}/conversations/${fallbackID}`, { replace: true });
				return;
            }
			setConversationHistory((current) => [detail.conversation, ...current.filter((item) => item.id !== detail.conversation.id)]);
            // URL 代表这个窗口当前查看的会话；不再调用 activate，也不会影响
            // 其他会话的后台运行或把它们变成只读。
            if (conversationRef.current?.id === detail.conversation.id) setConversation(detail.conversation);
            else resetConversationView(detail.conversation);
            void refreshConversationHistory().catch((cause) => fail(cause instanceof Error ? cause.message : "无法刷新会话历史"));
            return;
        }

        // 否则使用最新对话或创建新对话
        const list = await refreshConversationHistory();
        if (cancelled || conversationTransitionRef.current) return;
        // Returning to the project without a conversation ID must not reopen a
        // tab the user deliberately closed. Restore the saved active tab first;
        // otherwise use the newest history item that is not dismissed.
        const restoredID = conversationTabsRef.current.activeConversationId;
        const restored = restoredID ? list.find((item) => item.id === restoredID) : undefined;
        const closed = new Set(readClosedConversationIds(projectId || ""));
        const next = restored || list.find((item) => !closed.has(item.id)) || await projectApi<Conversation>(`/api/projects/${projectId}/conversations`, { method: "POST" });
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
          if (urlConversationId && typeof cause === "object" && cause !== null && (cause as { status?: unknown }).status === 404) {
            removeUnavailableConversationTabs([urlConversationId]);
            return;
          }
          fail(cause instanceof Error ? cause.message : "无法打开会话");
          if (urlConversationId) navigate(`/projects/${projectId}/conversations`, { replace: true });
        }
      }
    }
    void loadConversation();
    return () => { cancelled = true; abort.abort(); };
  }, [projectId, urlConversationId, readOnlyConversation, clearing, fail, refreshConversationHistory, projectApi, navigate, rememberConversationTab, removeUnavailableConversationTabs, reopenArchivedConversation]);

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

  // 加载 Skill：按当前会话 agentId 过滤展示对应 CLI 的技能。失败时静默为空。
  useEffect(() => {
    if (!projectId) return;
    const agentId = conversation?.agentId || "claude-code";
    let cancelled = false;
    // agentId 变化（切换到另一 CLI 的会话）时先清空旧技能，避免短暂显示上一 CLI 的技能。
    setSkills([]);
    setSkillsLoading(true);
    void (async () => {
      try {
        const list = await projectApi<Skill[]>(`/api/projects/${projectId}/skills?agentId=${encodeURIComponent(agentId)}`);
        if (!cancelled) setSkills(list);
      } catch { if (!cancelled) setSkills([]); }
      finally { if (!cancelled) setSkillsLoading(false); }
    })();
    return () => { cancelled = true; };
  }, [projectId, conversation?.agentId, projectApi]);

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
      const runAtRequest = runRef.current;
      try {
        const data = await projectApi<{ messages: Message[]; events: Event[]; activeRunId: string | null; hasMore: boolean; hasMoreMessages: boolean; nextCursor: string }>(`/api/conversations/${conversation.id}?limit=400`);
        if (!isCurrentConversation()) return false;
        data.messages.forEach((message) => { if (message.role === "assistant") recordAssistantOutput(message.runId || "", message.content); });
        // 过滤掉被撤回的用户消息 — 停止时如果助手还没有输出，后端可能仍会返回用户消息
        const filteredMessages = data.messages.filter((message) => !(message.role === "user" && message.runId && retractedMessageRuns.current.has(message.runId)));
        setMessages((current) => mergeReloadMessages(filteredMessages, current));
        setEvents((current) => mergeConversationItems(data.events, current));
        // 这份回包是请求发出时的快照：若请求期间 run 已推进到更晚的值（例如预约发送
        // 自动发出的新回合，或另一处已启动的新 run），不能拿旧快照把它覆盖/清空，
        // 否则 isConversationIdle 会误判为空闲、运行指示也会丢失。
        setRun((current) => {
          if (data.activeRunId) return data.activeRunId;
          if (current && current !== runAtRequest) return current;
          return "";
        }); setHasMoreHistory(data.hasMore); setHasMoreMessageHistory(data.hasMoreMessages); setHistoryCursor(data.nextCursor || "");
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
      pendingSendRequestIDRef.current = null;
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
    markActiveConversationTabRead();
    setCurrentUserMessageIndex(userMessages.length - 1);
  }, [markActiveConversationTabRead, userMessages.length]);

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
    if (nearBottom) {
      setHasNewContent(false);
      markActiveConversationTabRead();
    }
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
        markActiveConversationTabRead();
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
      if (nearBottom) {
        setHasNewContent((prev) => prev ? false : prev);
        markActiveConversationTabRead();
      }
      scheduleCurrentUserMessageIndexUpdate();
    };
    window.addEventListener("scroll", onScroll, { passive: true });
    media.addEventListener("change", onLayoutChange);
    onLayoutChange();
    return () => {
      window.removeEventListener("scroll", onScroll);
      media.removeEventListener("change", onLayoutChange);
    };
  }, [markActiveConversationTabRead, scheduleCurrentUserMessageIndexUpdate]);

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

  // 自动滚动。用 useLayoutEffect 而非 useEffect：紧跟 DOM 提交同步决策，
  // 避免上一次自动滚动排队中的 scroll 事件先于本 effect 触发，把 userNearBottom
  // 误翻为 false（内容已增长而 scrollTop 还停在旧底部，nearBottom 会算成 false）。
  // 真正滚动仍延后到下一帧（scrollToBottomNextFrame），确保 scrollHeight 已就绪。
  useLayoutEffect(() => {
    if (pendingApproval) return;
    if (userNearBottom.current) {
      scrollToBottomNextFrame("auto");
    } else {
      setHasNewContent((prev) => prev ? prev : true);
    }
  }, [visibleContentVersion, run, pendingApproval, scrollToBottomNextFrame]);

  // 业务方法
  const sendContent = async (rawContent: string, clearDraft = true, options: { restoreOnFailure?: boolean; notifyFailure?: boolean; clientRequestId?: string } = {}): Promise<boolean> => {
    const { restoreOnFailure = true, notifyFailure = true } = options;
    if (!conversation || readOnlyConversation || sending || clearing || stopping || shortcutBusy) return false;
    const content = rawContent.trim();
    if (!content) return false;
    if (content === "/resume") {
      if (clearDraft) {
        setComposerText("", conversation.id);
      }
      openConversationHistory();
      return false;
    }
    if (content === "/clear") {
      // /clear 是 CLI 的"清屏"命令：headless（-p --output-format stream-json）模式下
      // CLI 只发一个 conversation_reset 事件、内部重置自己的会话，不会在应用界面清空历史，
      // 导致与 Claude Code / Codex 里的展示效果不一致。这里拦截并走应用自己的
      // "清空上下文"流程（新建空白会话、旧会话保留在历史中），让 /clear 真正清空对话历史。
      clearConversationContext();
      return false;
    }
    const conversationID = conversation.id;
    const clientRequestId = options.clientRequestId ?? crypto.randomUUID();
    const routeVersion = conversationRouteVersion.current;
    setSending(true);
    if (clearDraft) {
      setComposerText("", conversationID);
    }
    setShowPermissionMenu(false); historyIndex.current = null; draftBeforeHistory.current = "";
    try {
      const data = await projectApi<{ message: Message; runId: string }>(`/api/conversations/${conversationID}/messages`, { method: "POST", body: JSON.stringify({ content, clientRequestId }) });
      if (conversationRef.current?.id !== conversationID) return false;
      if (!assistantOutputRuns.current.has(data.runId) && !finishedRunIds.current.has(data.runId)) pendingUserDrafts.current.set(data.runId, data.message.content);
      setMessages((old) => [...old, data.message]); appendInputHistory(data.message.content); setHistoryRefresh((version) => version + 1); setRun(finishedRunIds.current.has(data.runId) ? "" : data.runId);
      followAfterDispatch();
      void refreshUsage().catch((cause) => fail(cause instanceof Error ? cause.message : "无法刷新用量统计"));
      void refreshConversationHistory().catch((cause) => fail(cause instanceof Error ? cause.message : "无法刷新会话历史"));
      return true;
    } catch (cause) {
      if (conversationRef.current?.id !== conversationID || conversationRouteVersion.current !== routeVersion) return false;
      if (notifyFailure) fail(cause instanceof Error ? cause.message : "无法发送消息");
      if (clearDraft && restoreOnFailure) {
        setComposerText(content, conversationID);
        if (projectId) flushConversationDraft(projectId, conversationID);
      }
      return false;
    }
    finally { setSending(false); }
  };

  const sendContentRef = useRef(sendContent);
  sendContentRef.current = sendContent;

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
    pendingSendRequestIDRef.current = crypto.randomUUID();
    // 新预约视为“尚未开始等待空闲”，让 effect 在空闲成立时重新挂起延迟发送
    // （否则对话当前已空闲时，wasScheduledIdleRef 为 true，空闲转变检测不会触发）。
    wasScheduledIdleRef.current = false;
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
    pendingSendRequestIDRef.current = null;
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

  // 真正发出预约内容。仅在“空闲刚成立”的延迟触发中被调用，随后清理预约状态。
  const fireScheduledSend = useCallback(async (): Promise<boolean> => {
    const conversationID = pendingSendConversationRef.current;
    const content = pendingSendRef.current;
    const clientRequestId = pendingSendRequestIDRef.current;
    if (!conversationID || !content || !clientRequestId) return false;
    if (readOnlyConversation) return false; // 会话变为只读（编排/readonly）时不能发送，保留预约不丢失
    if (sending || clearing || stopping || shortcutBusy) return false;
    if (conversationRef.current?.id !== conversationID) return false; // 切换会话后不越界发送
    if (!isConversationIdle()) return false; // 延迟期间又有了新的运行，继续等待
    // 自动发送可能撞上瞬时的后端拒绝（工作区被并发的 insight 扫描 / Git 操作占用、
    // 会话重建、CLI 忙碌等）。带退避重试几次，避免一失败就把内容弹回输入框；
    // 前几次静默重试（不弹错误、不写回输入框），最后一次才提示并回退。
    const backoff = [1500, 3000, 6000, 10000];
    for (let attempt = 0; attempt <= backoff.length; attempt++) {
      if (pendingSendRef.current !== content || pendingSendConversationRef.current !== conversationID || pendingSendRequestIDRef.current !== clientRequestId) return false; // 预约已被取消/切换会话
      if (conversationRef.current?.id !== conversationID) return false;
      if (!isConversationIdleRef.current()) return false; // 重试期间又有了新的运行，交回空闲检测重新挂起
      const last = attempt === backoff.length;
      const ok = await sendContentRef.current(content, true, { restoreOnFailure: last, notifyFailure: last, clientRequestId });
      if (ok) {
        pendingSendRef.current = null;
        pendingSendConversationRef.current = null;
        pendingSendRequestIDRef.current = null;
        setPendingSendContent((current) => current === content ? null : current);
        return true;
      }
      if (last) {
        // 最后一次也失败：sendContent 若走了 catch（restoreOnFailure）已把内容写回输入框；
        // 否则（被 sendContent 守卫拦截，如 sending 卡住）在这里手动写回，避免预约内容丢失。
        if (!textRef.current.trim()) setComposerText(content, conversationID);
        pendingSendRef.current = null;
        pendingSendConversationRef.current = null;
        pendingSendRequestIDRef.current = null;
        setPendingSendContent((current) => current === content ? null : current);
        return true;
      }
      await new Promise((resolve) => setTimeout(resolve, backoff[attempt]));
    }
    return true;
  }, [clearing, isConversationIdle, readOnlyConversation, sending, shortcutBusy, stopping]);

  const fireScheduledSendRef = useRef(fireScheduledSend);
  fireScheduledSendRef.current = fireScheduledSend;
  const isConversationIdleRef = useRef(isConversationIdle);
  isConversationIdleRef.current = isConversationIdle;

  // 监听预约内容：会话“从运行中变为彻底空闲”时延迟一拍再真正发送。
  // run.* 事件会立刻把 run 置空并触发 reload，后端也还在对上一个 run 做收尾；
  // 立即 POST 可能撞上瞬时 4xx，被 sendContent 的失败回退写回输入框。
  // 延迟后再重查空闲仍成立才发送，显著降低这类竞态。
  useEffect(() => {
    if (!pendingSendRef.current) {
      if (scheduledSendTimerRef.current != null) {
        window.clearTimeout(scheduledSendTimerRef.current);
        scheduledSendTimerRef.current = null;
      }
      wasScheduledIdleRef.current = isConversationIdle();
      return;
    }
    const idle = isConversationIdle();
    if (idle && !wasScheduledIdleRef.current) {
      // 刚进入空闲：挂起一次延迟触发。
      if (scheduledSendTimerRef.current != null) window.clearTimeout(scheduledSendTimerRef.current);
      scheduledSendTimerRef.current = window.setTimeout(() => {
        scheduledSendTimerRef.current = null;
        void fireScheduledSendRef.current().then((fired) => {
          // 若因发送中/停止中/清空中被拦截而未真正发出，重置状态让下一次空闲成立时重试。
          if (!fired && pendingSendRef.current) wasScheduledIdleRef.current = false;
        }).catch(() => {
          // sendContent 内部已兜底，这里仅防止意外拒绝导致未处理 Promise。
          if (pendingSendRef.current) wasScheduledIdleRef.current = false;
        });
      }, 400);
    } else if (!idle) {
      // 又开始了新运行：取消挂起的预约发送。
      if (scheduledSendTimerRef.current != null) {
        window.clearTimeout(scheduledSendTimerRef.current);
        scheduledSendTimerRef.current = null;
      }
    }
    wasScheduledIdleRef.current = idle;
    // 注意：不要把 sendContent 加进依赖。它每次渲染都重建（非 useCallback），
    // 且本 effect 只通过 fireScheduledSendRef 间接调用它，加了会让此 effect 每
    // 次渲染都跑一遍，白白重复 idle 判定。
  }, [isConversationIdle, pendingSendContent, clearing, readOnlyConversation, sending, shortcutBusy, stopping]);

  useEffect(() => () => {
    if (scheduledSendTimerRef.current != null) window.clearTimeout(scheduledSendTimerRef.current);
  }, []);

  const runShortcut = async (shortcut: Shortcut, variables: Record<string, string> = {}, variablesReady = false) => {
    if (!conversation || readOnlyConversation || sending || clearing || stopping || shortcutBusy) return;
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
    // 清屏（模板恰好为 /clear）也是 CLI 的本地命令：headless 模式下只发
    // conversation_reset、不会在应用界面清空历史。点击清屏快捷项直接走应用自己的
    // "清空上下文"流程，与输入框里键入 /clear 的行为一致。
    if (shortcut.template.trim() === "/clear") {
      clearConversationContext();
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
    if (conversationTransitionRef.current || sending || clearing || stopping || shortcutBusy || workspaceBusy || !projectId) { closeNewConversation(); return; }
    if (conversationTabs.openConversationIds.length >= MAX_OPEN_CONVERSATION_TABS) {
      closeNewConversation();
      fail(`每个项目最多同时打开 ${MAX_OPEN_CONVERSATION_TABS} 个会话，请先关闭一个 Tab。`);
      return;
    }
    conversationTransitionRef.current = true;
    setClearing(true);
    try {
      const activeWorkspaceID = conversationWorkspaces.find((item) => item.active)?.id || "";
      const next = await projectApi<Conversation>(`/api/projects/${projectId}/conversations?new=true`, { method: "POST", body: JSON.stringify({ agentId, permissionMode, profileId: profileID || "", workspaceId: activeWorkspaceID }) });
      rememberConversationTab(next.id);
      resetConversationView(next);
      navigate(`/projects/${projectId}/conversations/${next.id}`, { replace: true });
      void refreshConversationHistory().catch((cause) => fail(cause instanceof Error ? cause.message : "无法刷新会话历史"));
    }
    catch (cause) { fail(cause instanceof Error ? cause.message : "无法新建会话"); }
    finally { conversationTransitionRef.current = false; setClearing(false); }
  };

  const clearConversationContext = () => {
    if (conversationTransitionRef.current || !conversation || readOnlyConversation || sending || clearing || stopping || shortcutBusy) return;
    if (run) {
      // 运行中时先停止，再关闭当前会话并新建一个会话。
      pendingConfirmBusyRef.current = false;
      setPendingConfirmBusy(false);
      setPendingConfirm({
        title: "停止并新建会话",
        message: "当前对话正在运行中，将先停止运行（包括排队中的请求），关闭当前会话后再新建空白会话。当前内容仍可在历史会话中恢复。",
        confirmLabel: "停止并新建",
        danger: true,
        className: "conversation-clear-dialog is-running",
        icon: <ConversationClearIcon running />,
        onConfirm: () => {
          if (pendingConfirmBusyRef.current) return;
          pendingConfirmBusyRef.current = true;
          setPendingConfirmBusy(true);
          void stopAndClearRef.current().finally(() => {
            pendingConfirmBusyRef.current = false;
            setPendingConfirmBusy(false);
            setPendingConfirm(null);
          });
        },
        onCancel: () => { if (!pendingConfirmBusyRef.current) setPendingConfirm(null); },
      });
      return;
    }
    pendingConfirmBusyRef.current = false;
    setPendingConfirmBusy(false);
    setPendingConfirm({
      title: "关闭并新建会话",
      message: "将关闭当前会话并打开一个新的空白会话。当前内容仍可在历史会话中恢复。",
      confirmLabel: "关闭并新建",
      className: "conversation-clear-dialog",
      icon: <ConversationClearIcon />,
      onConfirm: () => {
        if (pendingConfirmBusyRef.current) return;
        pendingConfirmBusyRef.current = true;
        setPendingConfirmBusy(true);
        void clearCurrentConversation().finally(() => {
          pendingConfirmBusyRef.current = false;
          setPendingConfirmBusy(false);
          setPendingConfirm(null);
        });
      },
      onCancel: () => { if (!pendingConfirmBusyRef.current) setPendingConfirm(null); },
    });
  };

  const stopAndClear = async () => {
    if (!conversation || stopping) return;
    // 运行可能已在确认框等待期间自然结束
    if (!run) {
      return clearCurrentConversation();
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
      fail("停止超时，请稍后重试关闭并新建会话。");
      return;
    }
    // 只清空我们正在停止的 run，避免误清空其他新启动的 run
    setRun((active) => active === currentRun ? "" : active);
    return clearCurrentConversation(true);
  };
  stopAndClearRef.current = stopAndClear;

  const clearCurrentConversation = async (skipRunGuard = false) => {
    if (conversationTransitionRef.current || !conversation || sending || clearing || shortcutBusy || (!skipRunGuard && run)) return;
    // 用户主动关闭会话并新建：清空预约状态，让预约内容随之作废。
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
      let next: Conversation | null = null;
      let lastCause: unknown;
      // 刚结束一轮运行时，后端状态可能还没来得及落到 idle。对 409 做短暂退避，
      // 避免用户必须关闭提示框并重新走一遍清空流程。
      for (let attempt = 0; attempt < 4; attempt++) {
        try {
          next = await projectApi<Conversation>(`/api/conversations/${conversationID}/clear`, { method: "POST" });
          lastCause = undefined;
          break;
        } catch (cause) {
          lastCause = cause;
          const status = typeof cause === "object" && cause !== null ? (cause as { status?: unknown }).status : undefined;
          if (status !== 409 || attempt === 3) throw cause;
          await new Promise((resolve) => setTimeout(resolve, 250 * (attempt + 1)));
        }
      }
      if (!next) throw lastCause ?? new Error("无法清除会话上下文");
      if (!clearStillOwnsView()) return;
      replaceConversationTab(conversationID, next.id);
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
            replaceConversationTab(conversationID, current.id);
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
    if (!conversation || item.id === conversation.id || item.isOrchestration) { closeHistory(); return; }
    if (sending || clearing || stopping || shortcutBusy) { closeHistory(); return; }
	    if (!conversationTabs.openConversationIds.includes(item.id) && conversationTabs.openConversationIds.length >= MAX_OPEN_CONVERSATION_TABS) {
	      closeHistory();
	      fail(`每个项目最多同时打开 ${MAX_OPEN_CONVERSATION_TABS} 个会话，请先关闭一个 Tab。`);
	      return;
	    }
	    setActivatingConversation(item.id);
	    try {
	      if (item.status === "archived") {
	        await reopenArchivedConversation(item.id);
	      }
	      selectConversationTab(item.id);
	      closeHistory();
	    } catch (cause) {
	      fail(cause instanceof Error ? cause.message : "无法恢复历史会话");
	    } finally {
	      setActivatingConversation("");
	    }
  };

	const viewConversation = (item: Conversation) => {
		activateConversation(item);
	};

	const deleteHistoryConversation = (item: Conversation) => {
		if (!projectId || deletingConversation || deleteAllConversationsBusy) return;
		pendingConfirmBusyRef.current = false;
		setPendingConfirmBusy(false);
		setPendingConfirm({
			title: "删除会话",
			message: `将永久删除会话「${item.title || "新会话"}」及其全部消息，无法恢复。`,
			confirmLabel: "删除",
			danger: true,
			className: "conversation-delete-dialog",
			onConfirm: () => {
				if (pendingConfirmBusyRef.current) return;
				pendingConfirmBusyRef.current = true;
				setPendingConfirmBusy(true);
				void deleteHistoryConversationConfirmed(item.id).finally(() => {
					pendingConfirmBusyRef.current = false;
					setPendingConfirmBusy(false);
					setPendingConfirm(null);
				});
			},
			onCancel: () => { if (!pendingConfirmBusyRef.current) setPendingConfirm(null); },
		});
	};

	const deleteHistoryConversationConfirmed = async (conversationID: string) => {
		setDeletingConversation(conversationID);
		try {
			// 单条删除允许较长等待：级联删除会同步清掉消息/事件/运行记录，历史很长
			// 的会话在慢速磁盘上可能超过默认 15s 请求超时。
			await apiWithTimeout(`/api/conversations/${conversationID}`, { method: "DELETE" }, 0, 120_000);
		} catch (cause) {
			fail(cause instanceof Error ? cause.message : "无法删除会话");
			return;
		} finally {
			setDeletingConversation("");
		}
		// 关闭该会话对应的 Tab；若它正是当前 URL 会话，会切到相邻 Tab 或新建会话。
		removeUnavailableConversationTabs([conversationID]);
		void refreshConversationHistory().catch((cause) => fail(cause instanceof Error ? cause.message : "无法刷新会话历史"));
	};

	const deleteAllHistoryConversations = () => {
		if (!projectId || deletingConversation || deleteAllConversationsBusy) return;
		pendingConfirmBusyRef.current = false;
		setPendingConfirmBusy(false);
		setPendingConfirm({
			title: "清除全部历史对话",
			message: "将永久删除该项目下的全部会话历史（自动编排会话只读保留），无法恢复。",
			confirmLabel: "清除全部",
			danger: true,
			className: "conversation-delete-all-dialog",
			onConfirm: () => {
				if (pendingConfirmBusyRef.current) return;
				pendingConfirmBusyRef.current = true;
				setPendingConfirmBusy(true);
				void deleteAllHistoryConversationsConfirmed().finally(() => {
					pendingConfirmBusyRef.current = false;
					setPendingConfirmBusy(false);
					setPendingConfirm(null);
				});
			},
			onCancel: () => { if (!pendingConfirmBusyRef.current) setPendingConfirm(null); },
		});
	};

	const deleteAllHistoryConversationsConfirmed = async () => {
		setDeleteAllConversationsBusy(true);
		let deletedIDs: string[] = [];
		try {
			const result = await apiWithTimeout<{ deleted: number; skipped: number; deletedIds?: string[] }>(`/api/projects/${projectId}/conversations`, { method: "DELETE" }, 0, 120_000);
			deletedIDs = result.deletedIds || [];
		} catch (cause) {
			fail(cause instanceof Error ? cause.message : "无法清除全部历史对话");
			return;
		} finally {
			setDeleteAllConversationsBusy(false);
		}
		if (deletedIDs.length > 0) {
			removeUnavailableConversationTabs(deletedIDs);
		}
		void refreshConversationHistory().catch((cause) => fail(cause instanceof Error ? cause.message : "无法刷新会话历史"));
	};

  const changePermissionMode = async (permissionMode: PermissionMode) => {
    if (!conversation || readOnlyConversation || conversation.permissionMode === permissionMode || run || clearing || stopping) return;
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
    if (!conversation || readOnlyConversation || !run || stopping) return;
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

  // 把 skill 名称 + 描述转成一段可引用的提示词文本，插入输入框（见 docs/22）。
  const mergeSkillPrompt = (skill: Skill) =>
    skill.description && skill.description !== skill.name
      ? `请使用技能 <${skill.name}>：${skill.description}`
      : `请使用技能 <${skill.name}>`;

  // 技能描述过长时截断，供悬浮卡片展示，避免过长的描述撑爆卡片。
  // 描述与技能名相同(或为空)时返回空串，悬浮卡片只显示标题不重复。
  const truncateSkillDescription = (description: string | undefined, name: string, max = 120) => {
    const text = (description || "").trim();
    if (!text || text === name) return "";
    if (text.length <= max) return text;
    return `${text.slice(0, max).trimEnd()}…`;
  };

  const useSkill = (skill: Skill) => {
    if (!conversation || readOnlyConversation || sending || clearing || stopping || Boolean(shortcutBusy) || skillsLoading) return;
    setComposerText(mergeSkillPrompt(skill));
    requestAnimationFrame(() => composerRef.current?.querySelector("textarea")?.focus());
  };

  const renderShortcutCell = (shortcut: Shortcut | undefined, kind: "prompt" | "command_request", placeholder = false, beforeRun?: () => void) => {
    if (!shortcut && placeholder) return <div className="quick-tag-slot" aria-hidden="true" />;
    if (!shortcut) return <button type="button" className={`quick-tag-empty${kind === "command_request" ? " command-tag" : ""}`} disabled={readOnlyConversation} onClick={() => { beforeRun?.(); setShortcutEditor({ kind }); }}>{kind === "command_request" ? "添加命令" : "添加提示词"}</button>;
    return <div className={`quick-tag ${shortcut.enabled ? "" : "disabled"}${kind === "command_request" ? " command-tag" : ""}`} key={shortcut.id}><button type="button" disabled={readOnlyConversation || !shortcut.enabled || !conversation || sending || clearing || stopping || Boolean(shortcutBusy)} onClick={() => { beforeRun?.(); void runShortcut(shortcut); }} title={shortcut.enabled ? shortcut.template : `${shortcut.template}\n\n${shortcut.name}已停用`}><span className="quick-tag-text">{shortcutBusy === shortcut.id ? "发送中" : shortcut.name}</span></button><button type="button" className="quick-tag-edit" title={`编辑 ${shortcut.name}`} aria-label={`编辑 ${shortcut.name}`} disabled={readOnlyConversation || clearing || Boolean(shortcutBusy)} onClick={() => { setShowMobileShortcuts(false); setShortcutEditor({ kind: shortcut.kind, shortcut }); }}><ShortcutMoreIcon /></button></div>;
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
  const renderMobilePromptCell = (shortcut: Shortcut | undefined) => renderShortcutCell(shortcut, "prompt", false, () => setShowMobileShortcuts(false));
  const renderMobileCommandCell = (shortcut: Shortcut | undefined) => renderShortcutCell(shortcut, "command_request", false, () => setShowMobileShortcuts(false));
  const sortableDraggingDisabled = readOnlyConversation || sending || clearing || stopping || Boolean(shortcutBusy);

  // Skill 组内容：标题 + 按来源分组的技能标签列表。技能点击填入输入框；加载中 / 空态单独呈现。
  const skillSourceMeta: Record<Skill["source"], { label: string; className: string; title: string }> = {
    plugin: { label: "系统/官方", className: "plugin", title: "来自插件 / 官方 marketplace 的技能（含内置样板），~/.claude/plugins" },
    user: { label: "用户", className: "user", title: "用户目录中的自建技能（~/.claude/skills）" },
    project: { label: "项目", className: "project", title: "当前项目自带的技能（<项目>/.claude/skills）" },
  };
  const skillSources: Skill["source"][] = ["plugin", "user", "project"];
  const skillsBySource = (source: Skill["source"]) => skills.filter((skill) => skill.source === source);
  const toggleSkillGroup = (source: Skill["source"]) => setSkillGroupsCollapsed((prev) => ({ ...prev, [source]: !prev[source] }));

  const renderSkillGroup = (options?: { collapsible?: boolean; collapsed?: boolean; onToggle?: () => void }) => {
    const collapsible = Boolean(options?.collapsible);
    const collapsed = collapsible && Boolean(options?.collapsed);
    const toggle = options?.onToggle ?? (() => {});
    const heading = collapsible
      ? <button type="button" className="quick-tag-heading quick-tag-heading-toggle skill-heading-toggle" aria-expanded={!collapsed} title={collapsed ? "展开技能模块" : "折叠技能模块"} onClick={toggle}>
          <span className="quick-tag-heading-label"><SkillTagIcon /><span>技能 (Skill)</span></span>
          <span className="skill-source-hint" title="全部来源">{skillsLoading ? "加载中" : `${skills.length} 个`}</span>
          <QuickTagChevron />
        </button>
      : <div className="quick-tag-heading">
          <span className="quick-tag-heading-label"><SkillTagIcon /><span>技能 (Skill)</span></span>
          <span className="skill-source-hint" title="全部来源"> {skillsLoading ? "加载中" : `${skills.length} 个`}</span>
        </div>;
    const body = skillsLoading ? <div className="quick-tag-list"><div className="quick-tag-empty skill-loading">加载中…</div></div>
      : skills.length === 0 ? <div className="quick-tag-list"><div className="quick-tag-empty skill-empty">未发现 Skill</div></div>
      : <div className="skill-source-groups">{skillSources.map((source) => {
          const group = skillsBySource(source);
          if (group.length === 0) return null;
          const meta = skillSourceMeta[source];
          const groupCollapsed = Boolean(skillGroupsCollapsed[source]);
          return (
            <div key={source} className={`skill-source-group${groupCollapsed ? " collapsed" : ""}`}>
              <button type="button" className="skill-source-heading" onClick={() => toggleSkillGroup(source)} title={meta.title} aria-expanded={!groupCollapsed}>
                <em className={`skill-source-chip ${meta.className}`}>{meta.label}</em>
                <span className="skill-source-count">{group.length}</span>
                <span className={`skill-source-chevron${groupCollapsed ? "" : " open"}`} aria-hidden="true" />
              </button>
              {!groupCollapsed && <ul className="quick-tag-list">{group.map((skill) => (
                <li key={skill.name} className="quick-tag-item skill-item">
                  <button type="button" className="skill-tag" disabled={readOnlyConversation || !conversation || sending || clearing || stopping || Boolean(shortcutBusy)} data-tooltip-title={skill.name} data-tooltip-desc={truncateSkillDescription(skill.description, skill.name)} onClick={() => useSkill(skill)}>
                    <span className="skill-tag-text">{skill.name}</span>
                  </button>
                </li>
              ))}</ul>}
            </div>
          );
        })}</div>;
    return (
      <div className={`quick-tag-group skill-tags${collapsible && collapsed ? " collapsed" : ""}`}>
        {heading}
        {!(collapsible && collapsed) && body}
      </div>
    );
  };

  return <>
    {createPortal(<div className={`head-actions-menu${showMobileActions ? " mobile-open" : ""}`}>
        <button className="head-actions-mobile-toggle" type="button" aria-expanded={showMobileActions} onClick={() => setShowMobileActions((open) => !open)}>操作</button>
        <div className="head-actions">
          <button className="conversation-head-action secondary" type="button" disabled={sending} onClick={openConversationHistory}><HistoryIcon /><span>历史</span></button>
          {conversationWorkspaces.length > 0 && <label className="conversation-workspace-picker"><span>工作区</span><select aria-label="会话工作区" value={conversationWorkspaces.find((item) => item.active)?.id || ""} disabled={workspaceBusy || Boolean(run) || clearing || stopping || readOnlyConversation} onChange={(event) => { const selected = conversationWorkspaces.find((item) => item.id === event.target.value); if (selected) void activateWorkspace(selected); }}><option value="" disabled>选择工作区</option>{conversationWorkspaces.filter((item) => item.state === "ready").map((item) => <option key={item.id} value={item.id}>{item.mode === "isolated_worktree" ? `工作区 #${item.generation}` : "项目工作区"}</option>)}</select></label>}
          <button className="conversation-head-action secondary workspace-create-action" type="button" disabled={workspaceBusy || Boolean(run) || clearing || stopping || readOnlyConversation} onClick={() => void createIsolatedWorkspace()} title="创建会话级 Git 工作区"><span>{workspaceBusy ? "处理中" : "新建工作区"}</span></button>
          {conversationWorkspaces.some((item) => item.mode === "isolated_worktree" && item.state === "ready" && !item.active) && <><label className="conversation-workspace-picker workspace-remove-picker"><span>移除目标</span><select aria-label="移除工作区" value={archiveWorkspaceID} disabled={workspaceBusy || Boolean(run) || clearing || stopping || readOnlyConversation} onChange={(event) => setArchiveWorkspaceID(event.target.value)}>{conversationWorkspaces.filter((item) => item.mode === "isolated_worktree" && item.state === "ready" && !item.active).map((item) => <option key={item.id} value={item.id}>工作区 #{item.generation}</option>)}</select></label><button className="conversation-head-action secondary workspace-remove-action" type="button" disabled={!archiveWorkspaceID || workspaceBusy || Boolean(run) || clearing || stopping || readOnlyConversation} onClick={() => { const workspace = conversationWorkspaces.find((item) => item.id === archiveWorkspaceID); if (workspace) void archiveWorkspace(workspace); }} title="移除选中的闲置工作区"><span>移除工作区</span></button></>}
          <button className="conversation-head-action secondary" type="button" disabled={readOnlyConversation} onClick={openAiConfig}><ProjectConfigIcon /><span>AI 配置</span></button>
          <div className="permission-menu">
          <button className={`permission-trigger ${conversation?.permissionMode === "full_control" ? "full" : ""}`} type="button" aria-haspopup="menu" aria-expanded={showPermissionMenu} disabled={readOnlyConversation || !!run || clearing || stopping || changingPermission} onClick={() => setShowPermissionMenu((open) => !open)}><PermissionModeIcon /><span>{permissionLabel}</span></button>
            {showPermissionMenu && <div className="permission-popover" role="menu">{isCodex ? <><button className={conversation?.permissionMode === "read_only" ? "selected" : ""} onClick={() => void changePermissionMode("read_only")}><b>仅分析</b><span>只读检查，不修改项目。</span></button><button className={conversation?.permissionMode === "workspace_write" ? "selected" : ""} onClick={() => void changePermissionMode("workspace_write")}><b>项目内执行</b><span>可在当前项目范围内读写和执行。</span></button><button className={conversation?.permissionMode === "full_control" ? "selected full" : ""} onClick={() => void changePermissionMode("full_control")}><b>完全控制</b><span>不受沙箱限制，命令直接执行</span></button></> : <><button className={conversation?.permissionMode === "approval_required" ? "selected" : ""} onClick={() => void changePermissionMode("approval_required")}><b>默认权限</b><span>命令执行前需要确认</span></button><button className={conversation?.permissionMode === "full_control" ? "selected full" : ""} onClick={() => void changePermissionMode("full_control")}><b>完全控制</b><span>命令直接执行</span></button></>}</div>}
          </div>
          <button className="conversation-head-action new-conversation-action primary" type="button" disabled={readOnlyConversation || sending || stopping || conversationTabs.openConversationIds.length >= MAX_OPEN_CONVERSATION_TABS} onClick={openNewConversationParam}><NewConversationIcon /><span>新会话</span></button>
        </div>
      </div>, document.querySelector('.head-actions-slot') || document.body)}
    {/* 对话面板：快捷方式、对话内容和任务队列 */}
    <section className="conversation-canvas" data-queue-collapsed={!readOnlyConversation && conversationPanels.taskQueue ? "true" : undefined}>
      <aside className="quick-tag-rail" aria-label="常用操作">
        <div className="quick-actions-row">
          <div className={`quick-tag-group${conversationPanels.prompt ? " collapsed" : ""}`}><div className="quick-tag-heading"><button type="button" className="quick-tag-heading-toggle" aria-expanded={!conversationPanels.prompt} title={conversationPanels.prompt ? "展开常用提示词" : "折叠常用提示词"} onClick={() => toggleConversationPanel("prompt")}><ShortcutCategoryIcon kind="prompt" /><span className="quick-tag-heading-title">常用提示词</span><b className="quick-tag-count">{promptShortcuts.length}</b><QuickTagChevron /></button><button type="button" title="新增常用提示词" aria-label="新增常用提示词" disabled={readOnlyConversation} onClick={() => setShortcutEditor({ kind: "prompt" })}><ShortcutAddIcon /></button></div>{!conversationPanels.prompt && <ShortcutSortableList items={promptShortcuts} kind="prompt" renderItem={renderPromptCell} draggingDisabled={sortableDraggingDisabled} onReorder={reorderKind} />}</div>
          <div className={`quick-tag-group command-tags${conversationPanels.command ? " collapsed" : ""}`}><div className="quick-tag-heading"><button type="button" className="quick-tag-heading-toggle" aria-expanded={!conversationPanels.command} title={conversationPanels.command ? "展开常用命令" : "折叠常用命令"} onClick={() => toggleConversationPanel("command")}><ShortcutCategoryIcon kind="command" /><span className="quick-tag-heading-title">常用命令</span><b className="quick-tag-count">{commandShortcuts.length}</b><QuickTagChevron /></button><button type="button" title="新增常用命令" aria-label="新增常用命令" disabled={readOnlyConversation} onClick={() => setShortcutEditor({ kind: "command_request" })}><ShortcutAddIcon /></button></div>{!conversationPanels.command && <ShortcutSortableList items={commandShortcuts} kind="command_request" renderItem={renderCommandCell} draggingDisabled={sortableDraggingDisabled} onReorder={reorderKind} />}</div>
          {renderSkillGroup({ collapsible: true, collapsed: conversationPanels.skills, onToggle: () => toggleConversationPanel("skills") })}
        </div>
      </aside>
      <section className="chat-center" id="conversation-panel" role="tabpanel" aria-labelledby={conversationTabs.activeConversationId ? `conversation-tab-${conversationTabs.activeConversationId}` : undefined}>
      <ConversationTabStrip state={conversationTabs} conversations={conversationHistory} workspaceLabels={conversationWorkspaceLabels} select={selectConversationTab} close={closeConversationTabFromUI} create={openNewConversationParam} openHistory={openConversationHistory} />
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
        <div className="composer-input-area">
          <textarea value={text} onChange={(event) => handleTextChange(event.target.value)} onKeyDown={(event) => { if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing) { event.preventDefault(); event.currentTarget.form?.requestSubmit(); return; } navigateInputHistory(event); }} placeholder={readOnlyConversation ? "自动编排执行中，仅供查看" : `描述希望${isCodex ? " Codex" : " Claude"}在当前项目中完成的工作...`} disabled={readOnlyConversation || sending || clearing || stopping || Boolean(shortcutBusy)} />
          <button className={`composer-mobile-shortcut-toggle${showMobileShortcuts ? " open" : ""}`} type="button" title="快捷操作" aria-label="快捷操作" aria-controls="mobile-shortcut-menu" aria-expanded={showMobileShortcuts} disabled={readOnlyConversation || sending || clearing || stopping || Boolean(shortcutBusy)} onClick={() => setShowMobileShortcuts((open) => !open)}><ComposerShortcutIcon /></button>
        </div>
        {showMobileShortcuts && <section className="mobile-shortcut-menu" id="mobile-shortcut-menu" aria-label="快捷操作">
          <div className="quick-tag-group"><div className="quick-tag-heading"><span className="quick-tag-heading-label"><ShortcutCategoryIcon kind="prompt" /><span>常用提示词</span><b className="quick-tag-count">{promptShortcuts.length}</b></span><button type="button" title="新增常用提示词" aria-label="新增常用提示词" disabled={readOnlyConversation} onClick={() => { setShowMobileShortcuts(false); setShortcutEditor({ kind: "prompt" }); }}><ShortcutAddIcon /></button></div><ShortcutSortableList items={promptShortcuts} kind="prompt" renderItem={renderMobilePromptCell} draggingDisabled={sortableDraggingDisabled} onReorder={reorderKind} /></div>
          <div className="quick-tag-group command-tags"><div className="quick-tag-heading"><span className="quick-tag-heading-label"><ShortcutCategoryIcon kind="command" /><span>常用命令</span><b className="quick-tag-count">{commandShortcuts.length}</b></span><button type="button" title="新增常用命令" aria-label="新增常用命令" disabled={readOnlyConversation} onClick={() => { setShowMobileShortcuts(false); setShortcutEditor({ kind: "command_request" }); }}><ShortcutAddIcon /></button></div><ShortcutSortableList items={commandShortcuts} kind="command_request" renderItem={renderMobileCommandCell} draggingDisabled={sortableDraggingDisabled} onReorder={reorderKind} /></div>
          {renderSkillGroup()}
        </section>}
        <div className="composer-footer"><ComposerRunnerInfo runnerID={project.runner} agentID={conversation?.agentId || "claude-code"} run={run} runLabel={runLabel} permissionMode={conversation?.permissionMode} usage={usage} displayedModel={displayedModel} contextLabel={contextLabel(usage?.context).replace(/^上下文 /, "")} contextLevel={contextLevel(usage?.context)} onShowUsage={openUsage} readOnly={readOnlyConversation} stopping={stopping} onStop={() => void stopRun()} /><span className="composer-actions"><button className="secondary composer-action composer-clear" type="button" disabled={readOnlyConversation || sending || clearing || stopping || Boolean(shortcutBusy)} onClick={clearConversationContext}><ComposerActionIcon action="clear" /><span>清空</span></button><button className="secondary composer-action composer-continue" type="button" disabled={readOnlyConversation || sending || clearing || stopping} onClick={() => void sendContent("继续", false)}><ComposerActionIcon action="continue" /><span>继续</span></button>{run && <button className="secondary composer-action composer-stop" type="button" disabled={readOnlyConversation || stopping} onClick={() => void stopRun()}>{stopping ? "停止中" : "停止"}</button>}<span className="composer-send-wrap"><button className="primary composer-action composer-send" disabled={readOnlyConversation || !text.trim() || sending || clearing || stopping || Boolean(shortcutBusy)}><ComposerActionIcon action="send" /><span>{sending ? "发送中" : "发送"}</span></button><button className={`composer-send-more${showSendMenu ? " open" : ""}`} type="button" title="发送方式" aria-label="发送方式" aria-haspopup="menu" aria-expanded={showSendMenu} disabled={readOnlyConversation || sending || clearing || stopping || Boolean(shortcutBusy)} onClick={() => setShowSendMenu((value) => !value)}><svg className="composer-send-more-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M6 9.5 12 15.5 18 9.5" /></svg></button>{showSendMenu && <span className="send-menu" role="menu"><button className="send-menu-item" type="button" role="menuitem" disabled={readOnlyConversation || !text.trim() || sending || clearing || stopping || Boolean(shortcutBusy)} onClick={(evt) => { setShowSendMenu(false); evt.currentTarget.form?.requestSubmit(); }}><ComposerActionIcon action="send" /><span><b>立即发送</b><small>立即交给 {isCodex ? "Codex" : "Claude"}，在下一轮工具调用后继续</small></span></button><button className="send-menu-item" type="button" role="menuitem" disabled={readOnlyConversation || !text.trim() || sending || clearing || stopping || Boolean(shortcutBusy)} onClick={() => void scheduleSend()}><ComposerActionIcon action="schedule" /><span><b>预约发送</b><small>当前任务（含子代理）全部结束后再发送</small></span></button></span>}</span></span></div>
        {pendingSendContent && <div className="composer-pending"><span className="composer-pending-icon"><svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="13.2" r="6.2" /><path d="M12 10.5V13l1.8 1.2" /></svg></span><span className="composer-pending-text"><b>已预约发送</b><small>{run ? "等待当前任务完成..." : "等待子代理完成..."}<span className="composer-pending-preview">{pendingSendContent.length > 40 ? `${pendingSendContent.slice(0, 40)}…` : pendingSendContent}</span></small></span><button className="composer-pending-cancel" type="button" title="撤回预约并带回输入框" onClick={cancelScheduledSend}>取消</button></div>}
      </form>
      </section>
      {!readOnlyConversation && <aside className="task-queue-rail" aria-label="任务队列" data-collapsed={conversationPanels.taskQueue ? "true" : undefined}>
        <TaskQueue projectID={project.id} conversationID={conversation?.id || ""} permissionMode={conversation?.permissionMode} request={projectApi} fail={fail} dispatchDisabled={clearing || stopping || !conversation} onDispatched={handleTaskDispatched} openBoard={(taskID) => navigate(`/projects/${project.id}/tasks${taskID ? `/${taskID}` : ""}`)} collapsed={conversationPanels.taskQueue} onToggleCollapsed={() => toggleConversationPanel("taskQueue")} />
      </aside>}
    </section>
    {showNewConversation && <NewConversationDialog runnerID={project.runner} defaults={appPreferences} defaultsLoading={appPreferencesLoading} defaultsError={appPreferencesError} close={closeNewConversation} create={newConversation} />}
    {showHistory && <ConversationHistoryDialog conversations={conversationHistory} activeID={conversation?.id || ""} busyID={activatingConversation} deletingID={deletingConversation} deleteAllBusy={deleteAllConversationsBusy} close={closeHistory} activate={activateConversation} view={viewConversation} search={searchConversationHistory} deleteOne={deleteHistoryConversation} deleteAll={deleteAllHistoryConversations} hasMore={Boolean(conversationHistoryCursor)} loadingMore={loadingMoreConversationHistory} loadMore={loadMoreConversationHistory} />}
    {showFullControlConfirmation && <FullControlConfirmationDialog close={() => setShowFullControlConfirmation(false)} confirm={confirmFullControl} changing={changingPermission} isCodex={isCodex} />}
    {showAgentExecution && agentExecutions.find((execution) => execution.runId === showAgentExecution) && <AgentExecutionDialog execution={agentExecutions.find((execution) => execution.runId === showAgentExecution)!} close={closeAgentExecution} />}
    {showUsage && <UsageDialog agentID={conversation?.agentId || "claude-code"} usage={usage} currentRun={currentUsage} close={closeUsage} />}
    {showAiConfig && <ProjectAiConfigDialog projectId={project.id} runnerID={project.runner} close={closeAiConfig} />}
    {shortcutEditor && <ShortcutEditor projectID={project.id} state={shortcutEditor} close={() => setShortcutEditor(null)} refresh={refreshShortcuts} fail={fail} />}
    {shortcutVariables && <ShortcutVariablesDialog state={shortcutVariables} close={() => setShortcutVariables(null)} run={(variables) => { setShortcutVariables(null); void runShortcut(shortcutVariables.shortcut, variables, true); }} />}
    {pendingConfirm && createPortal(<ConfirmDialog title={pendingConfirm.title} message={pendingConfirm.message} confirmLabel={pendingConfirm.confirmLabel} danger={pendingConfirm.danger} busy={pendingConfirmBusy} className={pendingConfirm.className} icon={pendingConfirm.icon} onConfirm={pendingConfirm.onConfirm} onCancel={pendingConfirm.onCancel} />, document.body)}
  </>;
}
