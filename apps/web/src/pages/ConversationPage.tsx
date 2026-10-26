// 对话页 — 从 App.tsx Chat 组件提取
// 完整保留了原 Chat 组件的全部功能：消息列表、WebSocket 实时事件、输入框、权限管理、会话切换、所有弹窗

import { FormEvent, memo, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { useParams, useNavigate, useSearchParams, useOutletContext, useLocation } from "react-router-dom";
import type { ProjectLayoutOutletContext } from "../components/ProjectLayout";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import "../markdown.css";
import "../permission.css";
import "../conversation.css";
import "../run-indicator.css";
import "../tasks.css";
import "../git.css";
import "../run.css";
import { TaskQueue } from "../features/tasks/TaskQueue";
import { ProjectAiConfigDialog } from "../features/run/ProjectAiConfigDialog";
import { ConfirmDialog } from "../components/ConfirmDialog";
import { AgentLogo } from "../components/AgentLogo";
import { agentLogoKey } from "../lib/cli-tools-view";
import { useProjectContext } from "../stores/useProjectStore";
import { useUIPreferences, type AppPreferences } from "../stores/useUIPreferences";
import type {
  Conversation, Message, Event, Shortcut, ShortcutEditorState,
  ConversationWorkspace,
  ConversationCheckpoint,
  PermissionMode, AgentID, AgentStatus, TimelineItem, ToolAction, AgentNode, AgentLog,
  AgentExecution, ConversationUsageResponse, ConversationModels, AgentModelOption,
  RunnerInfo, CheckUpdateResult, UpdateResult, SystemItem, SystemVariant, AgentProfile,
  Skill, AgentCommandOption, ProjectCommands, MCPInjectionStatus, MCPTransport,
} from "../lib/types";
import { NON_GIT_BRANCH } from "../lib/types";
import { api, apiWithTimeout, asRecord } from "../lib/api";
import {
  agentDisplayName, agentEntry, agentPermissionModes, agentSupportsSlashCommands, catalogAgentID,
  permissionCopy, runnerAgentStatus, runnerAgentReady, useAgentCatalogState,
  withRunnerAgentStatus,
} from "../lib/agent-registry";
import { createWebSocket } from "../lib/runtime";
import { caretTextBefore, hidePastePop, hidePastePopIfDetached, insertPasteCardAtSelection, isComposerBlankResidue, serializeComposer, shouldFoldPaste } from "../lib/composer-paste";
import {
  formatTime, formatHistoryTime, formatTokens, formatDuration,
  formatCost, runDuration, contextLevel, contextStateText, requiredShortcutVariables,
  isNarrowConversationLayout, websocketAssistantMessageID, isTemporaryMessage,
  mergeConversationItems, mergeReloadMessages, agentStatusLabel,
} from "../lib/utils";
import {
  buildTimeline, buildAgentExecutions, subagentTextIndex,
  isSubagentMessage, flattenAgents, timelineContentVersion,
} from "../lib/timeline";
import { formatStageClock, latestActiveStage, pickRunIndicatorVariant, pickRunTextVariant, splitLabelChars, type RunIndicatorVariant, type RunTextVariant } from "../lib/run-indicator";
import {
  MAX_OPEN_CONVERSATION_TABS, closeConversationTab, markConversationTabRead, openConversationTab, recordConversationActivity,
  readClosedConversationIds, clearConversationTabClosed, markConversationTabClosed, readConversationTabs, writeConversationTabs, type ConversationTabsState,
} from "../lib/conversation-tabs";
import { readConversationPanels, writeConversationPanels, type ConversationPanelKey, type ConversationPanelsState } from "../lib/conversation-panels";
import { modelList } from "../lib/model-options";
import { dialogParamURL } from "../lib/dialog-params";
import { copyToClipboard } from "../lib/clipboard";
import { checkpointHasCode, rewindConfirmAction, rewindConfirmBody, rewindConfirmTitle, rewindHint, rewindPrimaryMode } from "../lib/conversation-rewind";
import { dropAfterCutoff } from "../lib/conversation-cutoff";
import { useDocumentVisible } from "../lib/useDocumentVisible";
import { useLiveStateEventsFor } from "../components/LiveEventsProvider";
import { markdownCodeComponents } from "../components/MarkdownCodeBlock";
import { ServiceLogo } from "../components/ServiceLogo";
import { transportLabel } from "../features/mcp/mcp-model";
import type { GitBranch, GitSnapshot } from "../features/git/git-model";
import { ExternalLink } from "../components/ExternalLink";

function requiresForceStop(cause: unknown): boolean {
  return typeof cause === "object" && cause !== null && (cause as { code?: unknown }).code === "active_runs_present";
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

// 时间线行内的回退入口（2026-10-23 上卡片）：头部「节点」列表撤除后，回退与复制同住
// 消息卡片下方的动作行 —— 同一支撤销箭头，尺寸跟复制钮一致（28px ghost 图标钮）。
function RewindInlineIcon() {
  return <svg className="message-action-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M4.6 9.4h9.6a4.7 4.7 0 0 1 0 9.4H9.4" /><path d="M8.2 5.8 4.6 9.4l3.6 3.6" /></svg>;
}

// 复制按钮的图标：从伪元素画法换成与回退箭头同一支 SVG 描边语言；
// 复制成功后整颗换成勾（1.5 秒后复原，见 MessageCard 的 copied 态）。
function CopyIcon() {
  return <svg className="message-action-icon" viewBox="0 0 24 24" aria-hidden="true"><rect x="9" y="9" width="11" height="11" rx="2" /><path d="M5 15V6a2 2 0 0 1 2-2h9" /></svg>;
}
function CopyDoneIcon() {
  return <svg className="message-action-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M5 12.5 10 17.5 19 7" /></svg>;
}

// 方案 A「图标化降噪」：头部次要操作在桌面端只露图标（文字保留在 DOM 里，供
// ≤820px 的「操作」菜单显示与读屏使用）。新会话语义是「新建对话」而非「新建目录」，
// 主按钮改用加号；文件夹加号让给「新建工作区」。
function PlusIcon() {
  return <svg className="conversation-head-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M12 5v14M5 12h14" /></svg>;
}

function WorkspaceFolderIcon() {
  return <svg className="conversation-head-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M3.5 6.2c0-1 .8-1.8 1.8-1.8h4l2 2.2h7.4c1 0 1.8.8 1.8 1.8v9.4c0 1-.8 1.8-1.8 1.8H5.3c-1 0-1.8-.8-1.8-1.8V6.2Z" /></svg>;
}

function WorkspaceCreateIcon() {
  return <svg className="conversation-head-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M3.5 6.2c0-1 .8-1.8 1.8-1.8h4l2 2.2h7.4c1 0 1.8.8 1.8 1.8v9.4c0 1-.8 1.8-1.8 1.8H5.3c-1 0-1.8-.8-1.8-1.8V6.2Z" /><path d="M12 10.4v5M9.5 12.9h5" /></svg>;
}

function WorkspaceRemoveIcon() {
  return <svg className="conversation-head-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M3.5 6.2c0-1 .8-1.8 1.8-1.8h4l2 2.2h7.4c1 0 1.8.8 1.8 1.8v9.4c0 1-.8 1.8-1.8 1.8H5.3c-1 0-1.8-.8-1.8-1.8V6.2Z" /><path d="M9.5 13.5h5" /></svg>;
}

function NewConversationDialogIcon() {
  return <svg className="new-conversation-dialog-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M5.5 4.5h8.7l4.3 4.3v9.7a1.5 1.5 0 0 1-1.5 1.5H7a1.5 1.5 0 0 1-1.5-1.5v-14Z" /><path d="M14 4.5v4.6h4.5M12 11v5M9.5 13.5h5" /></svg>;
}

// 工具图标：优先**官方产品图标**（判据是 `lib/cli-tools-view.ts` 的 `agentLogoKey` ——
// 按工具 ID 白名单，与 Cli管理页同一份，不在这里再认一遍）。白名单之外回落到
// **中性图形**，绝不能落到某个已知工具的图标上 —— 那会让新工具看起来像是 Claude。
function AgentToolIcon({ agent }: { agent: AgentID }) {
  const logo = agentLogoKey(agent);
  if (logo) return <AgentLogo logo={logo} size={18} />;
  return <svg className="new-conversation-agent-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M12 3.5 20 8v8l-8 4.5L4 16V8l8-4.5Z" /><path d="M12 3.5V12m0 0 8-4m-8 4-8-4" /></svg>;
}

function ProjectConfigIcon() {
  return <svg className="conversation-head-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="m12 3.5 8 3v5.6c0 4.3-3 7.8-8 9.4-5-1.6-8-5.1-8-9.4V6.5l8-3Z" /><path d="M9 11.2 11 13.2 15.2 9" /></svg>;
}

// 滚动导航图标：统一 V 形笔画（stroke 走 .scroll-btn-icon 的 1.9 圆角描边）。
// 顶部/底部用上下 V 形；上一条/下一条用左右 V 形表达「序列里前一条 / 后一条」。
function ScrollNavigationIcon({ direction }: { direction: "top" | "previous" | "next" | "bottom" }) {
  const paths: Record<typeof direction, string> = {
    top: "m7 14.2 5-4.7 5 4.7",
    bottom: "m7 9.8 5 4.7 5-4.7",
    previous: "M14.2 7 9.5 12l4.7 5",
    next: "M9.8 7l4.7 5-4.7 5",
  };
  return <svg className="scroll-btn-icon" viewBox="0 0 24 24" aria-hidden="true"><path d={paths[direction]} /></svg>;
}

// 用户消息圆点轨：一条用户消息对应一个圆点，常驻时间线右缘。
// 当前视口所在消息的圆点放大加深（判据直接用 userMessageNavigationIndex，不在此重算视口位置）；
// 悬浮/聚焦弹出预览卡（序号 · 时间 · 内容摘要），点击复用 scrollToUserMessage 跳转。
// 消息很多时轨道内部自滚动（隐藏滚动条），当前圆点变化时自动滚进可视范围，不影响时间线滚动。
const UserMessageDotsRail = memo(function UserMessageDotsRail({ messages, activeIndex, onJump }: {
  messages: Message[];
  activeIndex: number;
  onJump: (index: number) => void;
}) {
  const wrapRef = useRef<HTMLDivElement | null>(null);
  const railRef = useRef<HTMLDivElement | null>(null);
  const dotRefs = useRef(new Map<string, HTMLButtonElement>());
  const [hoveredIndex, setHoveredIndex] = useState<number | null>(null);
  const [previewTop, setPreviewTop] = useState(0);

  const previewIndex = hoveredIndex !== null && hoveredIndex < messages.length ? hoveredIndex : null;
  const previewMessage = previewIndex !== null ? messages[previewIndex] : null;

  const showPreview = useCallback((index: number) => {
    const dot = dotRefs.current.get(messages[index]?.id);
    const wrap = wrapRef.current;
    if (!dot || !wrap) return;
    const dotRect = dot.getBoundingClientRect();
    const wrapRect = wrap.getBoundingClientRect();
    // 预览卡垂直对齐悬浮的圆点；贴近轨道两端时收进来，避免越出时间线。
    const center = dotRect.top - wrapRect.top + dotRect.height / 2;
    setPreviewTop(Math.min(Math.max(center, 34), Math.max(wrapRect.height - 34, 34)));
    setHoveredIndex(index);
  }, [messages]);

  // 当前消息变化时，把对应圆点滚进轨道可视范围（只滚轨道自己，不碰时间线）。
  useEffect(() => {
    const rail = railRef.current;
    const active = activeIndex >= 0 ? dotRefs.current.get(messages[activeIndex]?.id) : null;
    if (!rail || !active) return;
    if (active.offsetTop < rail.scrollTop + 8) rail.scrollTop = active.offsetTop - 16;
    else if (active.offsetTop + active.offsetHeight > rail.scrollTop + rail.clientHeight - 8) rail.scrollTop = active.offsetTop + active.offsetHeight - rail.clientHeight + 16;
  }, [activeIndex, messages]);

  if (messages.length === 0) return null;
  return (
    <div className="message-dots-wrap" ref={wrapRef} onMouseLeave={() => setHoveredIndex(null)}>
      <div className="message-dots" ref={railRef} role="navigation" aria-label="我的消息导航">
        {messages.map((message, index) => (
          <button key={message.id} type="button" ref={(el) => { if (el) dotRefs.current.set(message.id, el); else dotRefs.current.delete(message.id); }}
            className={`message-dot-btn${index === activeIndex ? " active" : ""}`}
            aria-label={`跳到第 ${index + 1} 条我的消息`}
            aria-current={index === activeIndex ? "true" : undefined}
            onMouseEnter={() => showPreview(index)}
            onFocus={() => showPreview(index)}
            onBlur={() => setHoveredIndex((current) => current === index ? null : current)}
            onClick={() => onJump(index)}>
            <span className="message-dot" />
          </button>
        ))}
      </div>
      {previewMessage && previewIndex !== null && <div className="message-dots-preview" style={{ top: `${previewTop}px` }} onClick={() => onJump(previewIndex)}>
        <span className="message-dots-preview-meta">第 {previewIndex + 1} 条 · {formatTime(previewMessage.createdAt)}</span>
        <span className="message-dots-preview-body">{previewMessage.content}</span>
      </div>}
    </div>
  );
});

function ComposerActionIcon({ action }: { action: "clear" | "handoff" | "continue" | "send" | "schedule" }) {
  if (action === "clear") return <svg className="composer-action-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="m6 7 1 12h10l1-12M9 7V4.5h6V7M4.5 7h15M10 11v4.5M14 11v4.5" /></svg>;
  if (action === "handoff") return <svg className="composer-action-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M5 7.5h9M11 4.5l3 3-3 3M19 16.5h-9M13 13.5l-3 3 3 3" /></svg>;
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

// ---- 常用命令目录（docs/37）------------------------------------------------
//
// 「新增常用命令」原来是一个自由文本输入框，用户填的 `/xxx` 未必是 CLI 认得的命令：填错
// 只会得到一句 "Unknown command"（还白留一条用户消息与一个 Run），填 `/review` 这种已被
// CLI 隐藏的命令则照常执行、照常花钱。现在默认从 CLI 自己的命令目录里选。
//
// 目录来自 Claude CLI 的 `system/init` 事件（`slash_commands`），由控制服务在真实运行时
// 采样或按需探测——这是唯一完整的来源：本机实测 47 条命令里有 17 条是编译进 CLI 的能力，
// 磁盘上没有文件，任何文件扫描都看不到它们。

// slashCommandName 取模板里的斜杠命令名（不含 `/` 与参数）；不是斜杠命令时返回空串。
// 与后端 slashCommandName 同一套语义。
function slashCommandName(template: string): string {
  const trimmed = template.trim();
  if (!trimmed.startsWith("/")) return "";
  const head = trimmed.split(/\s+/)[0];
  return head.slice(1);
}

// closestCommandName 给出"最像"的候选，用于把打错的 /compac 指向 /compact。
// 只有前缀命中或编辑距离足够近才给建议——宁可不说，也不要把无关命令推给用户。
function closestCommandName(input: string, candidates: string[]): string {
  const needle = input.toLowerCase();
  let best = "";
  let bestScore = Number.POSITIVE_INFINITY;
  for (const candidate of candidates) {
    const name = candidate.toLowerCase();
    const score = name.startsWith(needle) ? 0.5 : editDistance(needle, name);
    if (score < bestScore) {
      bestScore = score;
      best = candidate;
    }
  }
  return bestScore <= 2 ? best : "";
}

// editDistance 是标准的 Levenshtein 距离（命令名都在 20 字符内，两行滚动数组足够）。
function editDistance(a: string, b: string): number {
  if (a === b) return 0;
  let previous = Array.from({ length: b.length + 1 }, (_, index) => index);
  for (let i = 1; i <= a.length; i += 1) {
    const current = [i];
    for (let j = 1; j <= b.length; j += 1) {
      current[j] = Math.min(previous[j] + 1, current[j - 1] + 1, previous[j - 1] + (a[i - 1] === b[j - 1] ? 0 : 1));
    }
    previous = current;
  }
  return previous[b.length];
}

const commandGroupLabels: Record<string, string> = {
  builtin: "内置命令",
  skill: "技能命令",
  project: "本项目自定义",
  user: "我的自定义",
  plugin: "插件命令",
  other: "其它",
};

// commandDisplayName 是命令在列表里的主标题：优先中文名，其次 `名称 <参数提示>`。
function commandDisplayName(command: AgentCommandOption): string {
  return command.label || command.name;
}

// CommandCatalogList 渲染可搜索的命令列表。搜索时平铺全部命中项，不搜索时分组展示，
// 且把长尾（技能/插件/其它）收在后面——40 多条平铺会让人无从下手（docs/37 §3.1 策展规则）。
function CommandCatalogList({ commands, selected, onSelect }: { commands: AgentCommandOption[]; selected: string; onSelect: (command: AgentCommandOption) => void }) {
  const [search, setSearch] = useState("");
  const needle = search.trim().toLowerCase();
  const matches = useMemo(() => {
    if (!needle) return commands;
    return commands.filter((command) => `${command.name} ${command.label || ""} ${command.description || ""}`.toLowerCase().includes(needle));
  }, [commands, needle]);

  const groups = useMemo(() => {
    if (needle) return [{ key: "search", title: `匹配 ${matches.length} 条`, items: matches }];
    const recommended = matches.filter((command) => command.recommended);
    const sections = [{ key: "recommended", title: "建议", items: recommended }];
    const rest = matches.filter((command) => !command.recommended);
    for (const key of ["project", "user", "plugin", "skill", "builtin", "other"]) {
      const items = rest.filter((command) => command.group === key);
      if (items.length) sections.push({ key, title: commandGroupLabels[key], items });
    }
    return sections.filter((section) => section.items.length > 0);
  }, [matches, needle]);

  return <>
    <input className="command-picker-search" autoFocus type="search" value={search} onChange={(event) => setSearch(event.target.value)} placeholder="搜索命令，例如：compact / 上下文 / 审查" aria-label="搜索命令" />
    <div className="command-picker-list" role="listbox" aria-label="可用命令">
      {groups.length === 0 && <p className="command-picker-empty">没有匹配的命令</p>}
      {groups.map((group) => <div className="command-picker-group" key={group.key}>
        <p className="command-picker-group-title">{group.title}</p>
        {group.items.map((command) => <button type="button" role="option" aria-selected={command.name === selected} className={`command-picker-item${command.name === selected ? " selected" : ""}`} key={command.name} onClick={() => onSelect(command)}>
          <span className="command-picker-name">/{command.name}{command.argumentHint ? <code>{command.argumentHint}</code> : null}</span>
          <span className="command-picker-label">{commandDisplayName(command)}{command.terminalOnly ? <em className="command-picker-flag">仅终端</em> : null}</span>
          {command.description && <span className="command-picker-desc">{command.description}</span>}
        </button>)}
      </div>)}
    </div>
  </>;
}

// 运行指示器：7 个动画变体（波点/轨道/思绪波纹/星芒/追点/三点走三角/CLI 盲文）
// 按 runId 哈希随机抽一个，同一 run 内不换（判据在 lib/run-indicator.ts）。
// cli 变体的字符与阶段计时都由本组件内部的 interval 驱动 —— 计时状态必须收在
// 这里而不是页面根上：页面根每秒 setState 会强迫整个会话页树重渲染，
// 收在这里每秒只重渲染这一行。clockStartMs 传 null 表示无阶段事件，不显示计时。
const runIndicatorCliFrames = ["⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"];
// 文字位变体的拆字由这里渲染：rt-bounce 把阶段词拆成单字 span 逐字起舞，
// 其余变体整段一个节点（渐变 clip:text 的背景要铺满整段，拆开就扫不过去了）。
function RunStageText({ textVariant, label }: { textVariant: RunTextVariant; label: string }) {
  if (textVariant !== "bounce") return label;
  return splitLabelChars(label).map((ch, index) => (
    <span key={index} style={{ animationDelay: `${index * 70}ms` }}>{ch}</span>
  ));
}
function RunIndicatorView({ variant, textVariant, label, clockStartMs }: { variant: RunIndicatorVariant; textVariant: RunTextVariant; label: string; clockStartMs: number | null }) {
  const [cliFrame, setCliFrame] = useState(0);
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (variant !== "cli") return;
    setCliFrame(0);
    const timer = window.setInterval(() => setCliFrame((value) => value + 1), 80);
    return () => window.clearInterval(timer);
  }, [variant]);
  useEffect(() => {
    if (clockStartMs === null) return;
    setNow(Date.now());
    const timer = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, [clockStartMs]);
  const clock = clockStartMs !== null ? formatStageClock(now - clockStartMs) : "";
  return (
    <div className={`run-indicator ri-${variant}`}>
      {variant === "dots" && <span className="ri-anim ri-dots" aria-hidden="true"><i></i><i></i><i></i></span>}
      {variant === "orbit" && <span className="ri-anim ri-orbit" aria-hidden="true"><span className="ri-orbit-spin"><i></i><i></i></span></span>}
      {variant === "ripple" && <span className="ri-anim ri-ripple" aria-hidden="true"><i></i></span>}
      {variant === "spark" && <span className="ri-anim ri-spark" aria-hidden="true"><svg viewBox="0 0 24 24"><path fill="currentColor" d="M12 2 C13 8 16 11 22 12 C16 13 13 16 12 22 C11 16 8 13 2 12 C8 11 11 8 12 2 Z" /></svg></span>}
      {variant === "chase" && <span className="ri-anim ri-chase" aria-hidden="true">{Array.from({ length: 6 }, (_, index) => <i className="ri-chase-dot" key={index} />)}</span>}
      {variant === "tri" && <span className="ri-anim ri-tri" aria-hidden="true"><i></i><i></i><i></i></span>}
      {variant === "cli" && <span className="ri-anim ri-cli" aria-hidden="true">{runIndicatorCliFrames[cliFrame % runIndicatorCliFrames.length]}</span>}
      <span className={`ri-stage rt-${textVariant}`}><RunStageText textVariant={textVariant} label={label} /></span>
      {clock && <span className="ri-clock">· {clock}</span>}
    </div>
  );
}

function ConversationClearIcon({ running = false }: { running?: boolean }) {
  return running
    ? <svg className="conversation-clear-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M7 5.5h10M9.5 5.5v-2h5v2M6.5 8l.8 10.5h9.4L17.5 8M10 11.5v4M14 11.5v4" /><path d="M4.5 12.5 2.8 10.8 4.5 9.1" /></svg>
    : <svg className="conversation-clear-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M7 5.5h10M9.5 5.5v-2h5v2M6.5 8l.8 10.5h9.4L17.5 8M10 11.5v4M14 11.5v4" /></svg>;
}

// ComposerModelPicker —— 底部栏的模型选择器。Claude Code / Codex 的 CLI 都能在会话里换模型
// （/model），这里提供等价能力：选择写进会话级覆盖（conversations.model_override），
// 由控制服务在下一次运行时以 --model / -c model= 注入；切换模型会让长驻会话按新模型重启，
// 因此运行中禁用入口（与权限菜单一致）。
// 弹层用 portal + fixed 定位：移动端 .composer-usage 有 overflow-x:auto，内联绝对定位会被裁掉。
function ComposerModelPicker({ conversationID, agentID, selected, displayed, disabled, runActive, busy, onSelect, onFail }: { conversationID: string; agentID: AgentID; selected: string; displayed: string; disabled: boolean; runActive: boolean; busy: boolean; onSelect: (model: string) => Promise<boolean>; onFail: (message: string) => void }) {
  const [open, setOpen] = useState(false);
  const [loading, setLoading] = useState(false);
  const [view, setView] = useState<ConversationModels | null>(null);
  const [custom, setCustom] = useState("");
  const [position, setPosition] = useState<{ left: number; bottom: number; width: number } | null>(null);
  const triggerRef = useRef<HTMLButtonElement | null>(null);
  const menuRef = useRef<HTMLDivElement | null>(null);
  // requestSeq 用来标识"最新一次目录请求"：切换会话或重新打开都会 +1，旧响应据此丢弃。
  const requestSeq = useRef(0);
  const agentName = agentDisplayName(agentID);
  const buttonModel = selected || view?.effective || displayed;

  useEffect(() => {
    let cancelled = false;
    void api<ConversationModels>(`/api/conversations/${conversationID}/models`).then((data) => {
      if (!cancelled) setView(data);
    }).catch(() => {
      // 打开菜单时会再次请求并显示错误；预取失败不打扰当前对话。
    });
    return () => { cancelled = true; };
  }, [conversationID]);

  // 打开时定位：弹层贴在触发按钮上方，宽度按视口收敛，避免贴边溢出。
  const placeMenu = useCallback(() => {
    const rect = triggerRef.current?.getBoundingClientRect();
    if (!rect) return;
    const width = Math.min(340, Math.max(260, window.innerWidth - 24));
    const left = Math.min(Math.max(12, rect.left), Math.max(12, window.innerWidth - width - 12));
    setPosition({ left, bottom: Math.max(12, window.innerHeight - rect.top + 8), width });
  }, []);

  const close = useCallback(() => setOpen(false), []);

  // 切换会话时清掉上一个会话的目录与草稿：不同会话可能是不同的 Agent（Claude / Codex），
  // 残留的目录会把上一个会话的模型列给下一个人看。
  // 同时推进序号：正在飞行的 /models 响应回来时若序号已变，就丢弃它——否则它会把上一个
  // 会话的目录重新写回来（切换会话正好发生在请求飞行途中时，这条竞态就会触发）。
  useEffect(() => { requestSeq.current += 1; setView(null); setCustom(""); setOpen(false); }, [conversationID]);

  // 菜单开着时用户仍能直接在输入框回车发起任务（菜单浮在输入框上方，不挡输入）。任务一旦
  // 开始就不能切模型了，此时按钮既点不动（selectConversationModel 会直接返回 false）又不像
  // 被禁用，看上去像"点了没反应"。任务一开始就收摊。
  useEffect(() => { if (runActive) setOpen(false); }, [runActive]);

  useEffect(() => {
    if (!open) return;
    const onPointerDown = (event: MouseEvent) => {
      const target = event.target as Node | null;
      if (target && (menuRef.current?.contains(target) || triggerRef.current?.contains(target))) return;
      close();
    };
    // 捕获阶段监听 scroll 是为了在时间线滚动时收摊；但弹层自己也能滚（列表较长时），
    // 不排除弹层内部的话，滚动选择列表会立刻把菜单关掉。
    // 用 globalThis.Event：本文件从 lib/types 引入了同名的事件类型。
    const onScroll = (event: globalThis.Event) => {
      const target = event.target as Node | null;
      if (target && menuRef.current?.contains(target)) return;
      close();
    };
    document.addEventListener("mousedown", onPointerDown);
    window.addEventListener("resize", close);
    window.addEventListener("scroll", onScroll, true);
    return () => {
      document.removeEventListener("mousedown", onPointerDown);
      window.removeEventListener("resize", close);
      window.removeEventListener("scroll", onScroll, true);
    };
  }, [open, close]);

  const toggle = async () => {
    if (disabled || busy) return;
    if (open) { close(); return; }
    placeMenu();
    setOpen(true);
    setLoading(true);
    const seq = (requestSeq.current += 1);
    try {
      const data = await api<ConversationModels>(`/api/conversations/${conversationID}/models`);
      // 过期响应（已切会话或有更新的请求）一律丢弃，别把旧目录写回来。
      if (seq !== requestSeq.current) return;
      setView(data);
    } catch (cause) {
      if (seq !== requestSeq.current) return;
      onFail(cause instanceof Error ? cause.message : "无法读取可用模型");
      close();
    } finally {
      if (seq === requestSeq.current) setLoading(false);
    }
  };

  const choose = async (model: string) => {
    if (busy) return;
    // 选中的就是当前值（例如已经在"跟随配置"又点了一次"跟随配置"）时后端不会改任何东西，
    // 直接关掉弹层即可，不要摆出一个"没反应"的界面。
    if (model === selected) { setCustom(""); close(); return; }
    const ok = await onSelect(model);
    if (ok) { setCustom(""); close(); }
  };

  const selectedLabel = selected || view?.effective || "";
  // 候选项可能整个缺席：服务端把"该工具没有模型目录"（CodeBuddy）表示成空目录，而
  // Go 的 nil 切片会序列化成 null —— 只把可选链挂在外层对象上、随后直接读字段的
  // length（改动前的写法），遇到 null 就会抛 "reading 'length'"，把整个工作区面板
  // 打成兜底 UI。空目录照旧走下面的"没有可用的候选项"分支，自定义输入始终可用。
  // 打开弹层前还不知道来源（要等 /models 回来），所以只说确定的事，不猜"跟随 CLI 默认"。
  const customValid = /^[A-Za-z0-9][A-Za-z0-9._:@/+-]*$/.test(custom.trim());
  // 目录缺席（null）与空目录（[]）在界面上是同一件事：没有候选项可选，走自定义输入。
  const modelOptions = modelList(view);

  return <>
    <button ref={triggerRef} type="button" className={`composer-usage-model composer-model-trigger${open ? " open" : ""}${busy ? " busy" : ""}`} title={`${agentName} 模型：${buttonModel}，点击切换`} aria-haspopup="dialog" aria-expanded={open} disabled={disabled} onClick={() => void toggle()}>
      <span className="composer-model-name">{buttonModel}</span><span className="composer-model-caret" aria-hidden="true" />
    </button>
    {open && createPortal(<div ref={menuRef} className="model-menu" role="dialog" aria-label={`选择 ${agentName} 模型`} style={position ? { left: position.left, bottom: position.bottom, width: position.width } : undefined}>
      <header><div><b>选择 {agentName} 模型</b></div><button type="button" className="model-menu-close" title="关闭" aria-label="关闭" onClick={close}>×</button></header>
      {loading && !view ? <p className="model-menu-loading">正在读取可用模型…</p> : <>
        {modelOptions.length ? <ul className="model-menu-list">{modelOptions.map((option: AgentModelOption) => <li key={option.id}>
          <button type="button" className={`model-menu-item${selected === option.id ? " selected" : ""}`} disabled={busy} title={option.description || option.id} onClick={() => void choose(option.id)}>
            <span><b>{option.label || option.id}{option.alias && <em className="model-menu-alias">别名</em>}</b>{option.label && option.label !== option.id && <small>{option.id}</small>}</span>
          </button>
        </li>)}</ul> : <p className="model-menu-empty">没有可用的候选项。</p>}
        <div className="model-menu-custom">
          <label htmlFor="composer-model-custom">自定义模型名</label>
          <div><input id="composer-model-custom" value={custom} maxLength={128} placeholder="例如 opus、claude-opus-5、gpt-5.6-sol" disabled={busy} onChange={(event) => setCustom(event.target.value)} onKeyDown={(event) => { if (event.key === "Enter" && customValid && !busy) { event.preventDefault(); void choose(custom.trim()); } }} /><button type="button" className="primary" disabled={busy || !customValid} onClick={() => void choose(custom.trim())}>{busy ? "切换中" : "使用"}</button></div>
        </div>
        {selectedLabel && <p className="model-menu-current">当前模型：<b>{selectedLabel}</b></p>}
        {(view?.reasoningOptions?.length ?? 0) > 0 && <label className="model-menu-effort">推理强度<select value={view?.reasoningEffort || ""} disabled={busy} onChange={async (event) => { const effort = event.target.value; try { await api(`/api/conversations/${conversationID}/reasoning-effort`, { method: "POST", body: JSON.stringify({ reasoningEffort: effort }) }); setView((current) => current ? { ...current, reasoningEffort: effort } : current); } catch (cause) { onFail(cause instanceof Error ? cause.message : "推理强度切换失败"); } }}><option value="">默认强度</option>{view?.reasoningOptions?.map((effort) => <option key={effort} value={effort}>{effort}</option>)}</select></label>}
        {view?.note && <p className="model-menu-note">{view.note}</p>}
        <p className="model-menu-hint">切换后下一条消息生效，本会话内持续有效；仅影响模型，不影响凭据与权限。</p>
      </>}
    </div>, document.body)}
  </>;
}

// MCP 托盘的插头图标：无品牌图标的回落、空态与错误态都用它。
function ComposerMcpIcon() {
  return <svg className="composer-mcp-icon" viewBox="0 0 24 24" aria-hidden="true"><rect x="4" y="4" width="16" height="16" rx="3" /><path d="M9 9.5v5M9 9.5a2 2 0 1 0-2 2M15 14.5a2 2 0 1 0 2-2M15 9.5v5" /></svg>;
}

// 托盘上最多摆几个 server 图标（其余只在 ⋯ 菜单里）。见 ComposerMcpTray 里的布局说明。
const mcpTrayVisibleLimit = 4;

// ── MCP 图标托盘：数据源是「最近一次注入快照」（GET /api/projects/{id}/mcp/status）──
// 只展示 CLI 实际注入过的服务器，不展示"配置了但没注入"的——对话里能用什么以注入为准；
// 超过 mcpTrayVisibleLimit 个时托盘只摆前 N 个，剩下的在 ⋯ 里逐条列全。
// 三态分开渲染：读取中 / 读取失败（可重试）/ 快照（含"本次未注入"的明示），绝不把读不到说成没有。
// 弹层沿用 ComposerModelPicker 的 portal + fixed 定位模式，避免被卡片的滚动容器裁掉。
function ComposerMcpTray({ projectID, runKey }: { projectID: string; runKey: string }) {
  const [status, setStatus] = useState<MCPInjectionStatus | null>(null);
  const [phase, setPhase] = useState<"loading" | "ready" | "error">("loading");
  const [open, setOpen] = useState(false);
  const triggerRef = useRef<HTMLButtonElement | null>(null);
  const menuRef = useRef<HTMLDivElement | null>(null);
  const [position, setPosition] = useState<{ left: number; bottom: number; width: number } | null>(null);
  const navigate = useNavigate();
  // requestSeq 与 ComposerBranchPill 同款防竞态：切项目后旧响应晚到不得覆盖新状态。
  const requestSeq = useRef(0);

  const load = useCallback(async () => {
    const seq = requestSeq.current += 1;
    // 已有快照先留着显示，新响应到了再换，避免每次刷新都闪一下空态。
    setPhase((prev) => (prev === "ready" ? prev : "loading"));
    try {
      const data = await api<MCPInjectionStatus>(`/api/projects/${projectID}/mcp/status`);
      if (seq !== requestSeq.current) return;
      setStatus(data);
      setPhase("ready");
    } catch {
      if (seq !== requestSeq.current) return;
      setPhase((prev) => (prev === "ready" ? prev : "error"));
    }
  }, [projectID]);

  useEffect(() => { void load(); }, [load, runKey]);

  const placeMenu = useCallback(() => {
    const rect = triggerRef.current?.getBoundingClientRect();
    if (!rect) return;
    const width = Math.min(340, Math.max(260, window.innerWidth - 24));
    const left = Math.min(Math.max(12, rect.left), Math.max(12, window.innerWidth - width - 12));
    setPosition({ left, bottom: Math.max(12, window.innerHeight - rect.top + 8), width });
  }, []);

  const close = useCallback(() => setOpen(false), []);

  useEffect(() => {
    if (!open) return;
    const onPointerDown = (event: MouseEvent) => {
      const target = event.target as Node | null;
      if (target && (menuRef.current?.contains(target) || triggerRef.current?.contains(target))) return;
      close();
    };
    const onScroll = (event: globalThis.Event) => {
      const target = event.target as Node | null;
      if (target && menuRef.current?.contains(target)) return;
      close();
    };
    document.addEventListener("mousedown", onPointerDown);
    window.addEventListener("resize", close);
    window.addEventListener("scroll", onScroll, true);
    return () => {
      document.removeEventListener("mousedown", onPointerDown);
      window.removeEventListener("resize", close);
      window.removeEventListener("scroll", onScroll, true);
    };
  }, [open, close]);

  const toggle = () => {
    if (open) { close(); return; }
    void load(); // 打开面板时顺带刷新一次，注入快照在 run 开始后才落库，打开即取最新。
    placeMenu();
    setOpen(true);
  };

  const tileTitle = (name: string, displayName: string, origin: string, transport: string) =>
    `${displayName || name} · 已注入（${origin === "project" ? "项目" : "全局"}） · ${transportLabel(transport as MCPTransport)}`;

  // 托盘最多摆 4 个 tile，其余只在 ⋯ 的菜单里出现（菜单本来就逐条列全）。
  // 上限是**布局**要求、不是审美：工具坞是一行 flex、托盘 flex:none 不缩，而 1200–1300px
  // 视口下右侧任务栏占据栅格第三列、对话区只剩 510px —— 实测第 5 个 tile 起发送键就被挤出
  // 卡片压到任务栏上（5 个溢 18px、8 个溢 114px）。工具坞基类的 flex-wrap 是第二道兜底，
  // 两者一起保证这一档不再溢出。
  const trayServers = status?.servers.slice(0, mcpTrayVisibleLimit) ?? [];

  return (
    <span className="mcp-tray" role="group" aria-label="MCP 服务器">
      {phase === "error" ? (
        <button type="button" className="mcp-tile" title="MCP 状态读取失败 · 点击重试" aria-label="MCP 状态读取失败，点击重试" onClick={() => void load()}>
          <ComposerMcpIcon />
          <i className="mcp-tile-dot warn" aria-hidden="true" />
        </button>
      ) : trayServers.length > 0 ? trayServers.map((srv) => (
        <span key={srv.name} className="mcp-tile" title={tileTitle(srv.name, srv.displayName, srv.origin, srv.transport)}>
          <ServiceLogo service={srv.name} fallback={<span aria-hidden="true">{(srv.displayName || srv.name).slice(0, 1).toUpperCase()}</span>} size={14} />
          <i className="mcp-tile-dot" aria-hidden="true" />
        </span>
      )) : (
        <span className="mcp-tile" title={phase === "loading" ? "正在读取 MCP 状态…" : status?.note || "本次会话未注入任何 MCP 服务器"}>
          <ComposerMcpIcon />
        </span>
      )}
      <button ref={triggerRef} type="button" className={`mcp-tile mcp-tile-more${open ? " open" : ""}`} title={status && status.serverCount > 0 ? `全部 MCP 服务器（共 ${status.serverCount} 个）` : "全部 MCP 服务器"} aria-label="全部 MCP 服务器" aria-haspopup="dialog" aria-expanded={open} onClick={toggle}>⋯</button>
      {open && createPortal(
        <div ref={menuRef} className="mcp-menu" role="dialog" aria-label="MCP 服务器" style={position ? { left: position.left, bottom: position.bottom, width: position.width } : undefined}>
          <header>
            <div><label>MCP SERVERS</label><b>最近一次注入</b></div>
            <button type="button" className="mcp-menu-manage" onClick={() => { close(); navigate("/mcp-manager"); }}>管理连接</button>
          </header>
          {phase === "loading" && <p className="mcp-menu-note">正在读取注入状态…</p>}
          {phase === "error" && <p className="mcp-menu-note mcp-menu-error">MCP 状态读取失败。<button type="button" onClick={() => void load()}>重试</button></p>}
          {phase === "ready" && status && (status.serverCount > 0 ? (
            <ul className="mcp-menu-list">
              {status.servers.map((srv) => (
                <li key={srv.name} className="mcp-menu-item">
                  <span className="mcp-tile" aria-hidden="true">
                    <ServiceLogo service={srv.name} fallback={<span>{(srv.displayName || srv.name).slice(0, 1).toUpperCase()}</span>} size={14} />
                    <i className="mcp-tile-dot" aria-hidden="true" />
                  </span>
                  <span className="mcp-menu-copy"><b>{srv.displayName || srv.name}</b><small>{transportLabel(srv.transport as MCPTransport)}</small></span>
                  <span className="mcp-menu-origin">{srv.origin === "project" ? "项目" : "全局"}</span>
                </li>
              ))}
            </ul>
          ) : (
            <p className="mcp-menu-note">{status.note || "本次会话未注入任何 MCP 服务器。"}</p>
          ))}
          {phase === "ready" && status?.updatedAt && !status.updatedAt.startsWith("0001-") && <p className="mcp-menu-foot">注入快照 · {new Date(status.updatedAt).toLocaleString("zh-CN")}</p>}
        </div>,
        document.body,
      )}
    </span>
  );
}

// ── 上下文进度环：要有「窗口 + token 读数」两个可信值才画弧；读不到显示 "--" 并解释原因 ──
// （不把"计算中/不可用"画成 0%。环色即档位：normal 绿 / warning 琥珀 / danger 红。）
function ContextRing({ usage }: { usage: ConversationUsageResponse | null }) {
  const context = usage?.context;
  const tokens = context?.contextInputTokens;
  const window_ = context?.contextWindow;
  const percent = tokens && window_ ? Math.min(100, Math.round(tokens / window_ * 100)) : null;
  const level = contextLevel(context);
  const circumference = 2 * Math.PI * 9;
  const dash = percent === null ? 0 : Math.max(percent <= 0 ? 0 : 0.5, percent) / 100 * circumference;
  return (
    <span className={`ctx-ring ctx-${level}`} role="img" aria-label={contextStateText(context)} title={contextStateText(context)}>
      <svg viewBox="0 0 22 22" aria-hidden="true">
        <circle className="ctx-ring-track" cx="11" cy="11" r="9" />
        {percent !== null && <circle className="ctx-ring-fill" cx="11" cy="11" r="9" strokeDasharray={`${dash} ${circumference}`} />}
      </svg>
      <b aria-hidden="true">{percent === null ? "--" : `${percent}%`}</b>
    </span>
  );
}

// ── 分支胶囊：读当前工作区（会话级）的 git 摘要，分支/同步/未提交改动都是服务端算好的读数 ──
// 三态分开渲染：读取失败（灰，可重试）/ detached HEAD（琥珀）/ 正常（绿，含 ↑↓ 同步与 N 处改动）。
// 分支状态卡（方案 C）：点击向上弹出一张卡（胶囊在页面最底行，向下弹会跑出视口）——
// 上半是当前分支详情（短 SHA / ↑↓ 待推送落后 / N处未提交改动，「工作区干净」只在服务端计数为 0 时画）
// 和「查看改动」直达 Git 工作台；分隔线下半才是切换列表。刷新时机：挂载、会话切换、run 开始与结束
// （runKey 变化）——分支切换发生在 Git 工作台页面，回到会话页时组件重挂载即取新值。
function ComposerBranchPill({ projectID, conversationID, runKey }: { projectID: string; conversationID: string; runKey: string }) {
  const navigate = useNavigate();
  const [snapshot, setSnapshot] = useState<GitSnapshot | null>(null);
  const [branches, setBranches] = useState<GitBranch[]>([]);
  const [menuOpen, setMenuOpen] = useState(false);
  const [switching, setSwitching] = useState(false);
  // 切换分支沿用全站自绘确认框（原生 confirm 在桌面外壳里很突兀）。点分支只打开
  // 确认层，真正的切换在确认后执行 —— 与「移除工作区」「回退对话」同一套。
  const [pendingSwitch, setPendingSwitch] = useState<GitBranch | null>(null);
  const [phase, setPhase] = useState<"loading" | "ready" | "error">("loading");
  const [errorText, setErrorText] = useState("");
  const branchRequestSeq = useRef(0);
  // requestSeq 标识"最新一次请求"：切换会话/工作区时 props 变化会重发请求，
  // 旧会话的响应若晚到，据此丢弃——分支胶囊绝不能显示上一个工作区的分支。
  const requestSeq = useRef(0);

  const load = useCallback(async () => {
    const seq = requestSeq.current += 1;
    // 已有快照先留着显示，新响应到了再换，避免每次刷新都闪一下空态。
    setPhase((prev) => (prev === "ready" ? prev : "loading"));
    try {
      const query = conversationID ? `?conversationId=${encodeURIComponent(conversationID)}` : "";
      const data = await api<GitSnapshot>(`/api/projects/${projectID}/git/summary${query}`);
      if (seq !== requestSeq.current) return;
      setSnapshot(data);
      setPhase("ready");
    } catch (cause) {
      if (seq !== requestSeq.current) return;
      setPhase((prev) => (prev === "ready" ? prev : "error"));
      setErrorText(cause instanceof Error ? cause.message : "分支读取失败");
    }
  }, [projectID, conversationID]);

  const loadBranches = useCallback(async () => {
    const seq = ++branchRequestSeq.current;
    try {
      const query = conversationID ? `?conversationId=${encodeURIComponent(conversationID)}` : "";
      const data = await api<GitBranch[]>(`/api/projects/${projectID}/git/branches${query}`);
      if (seq !== branchRequestSeq.current) return;
      setBranches(data.filter((branch) => !branch.remote));
      setMenuOpen(true);
    } catch (cause) {
      if (seq !== branchRequestSeq.current) return;
      setErrorText(cause instanceof Error ? cause.message : "分支读取失败");
      setPhase("error");
    }
  }, [projectID, conversationID]);

  const switchBranch = useCallback((branch: GitBranch) => {
    if (!snapshot?.stateToken || branch.current || switching) return;
    setMenuOpen(false);
    setPendingSwitch(branch);
  }, [snapshot, switching]);

  const switchBranchConfirmed = useCallback(async () => {
    const branch = pendingSwitch;
    if (!branch || !snapshot?.stateToken || switching) return;
    // 确认层先留着显示"处理中"，切换结束（成功或失败）再收掉 —— 与移除工作区/回退同一套反馈。
    setSwitching(true);
    try {
      const query = conversationID ? `?conversationId=${encodeURIComponent(conversationID)}` : "";
      const result = await api<{ status: string; errorMessage?: string }>(`/api/projects/${projectID}/git/switch${query}`, { method: "POST", body: JSON.stringify({ branch: branch.name, remote: false, stateToken: snapshot.stateToken }) });
      if (result.status !== "succeeded") throw new Error(result.errorMessage || "分支切换失败");
      setMenuOpen(false);
      await load();
    } catch (cause) {
      setErrorText(cause instanceof Error ? cause.message : "分支切换失败");
      setPhase("error");
    } finally {
      setSwitching(false);
      setPendingSwitch(null);
    }
  }, [conversationID, load, pendingSwitch, projectID, snapshot, switching]);

  useEffect(() => {
    if (!menuOpen) return;
    const close = (event: MouseEvent) => {
      if (!(event.target as Element).closest(".branch-switcher")) setMenuOpen(false);
    };
    const key = (event: KeyboardEvent) => { if (event.key === "Escape") setMenuOpen(false); };
    document.addEventListener("mousedown", close);
    document.addEventListener("keydown", key);
    return () => { document.removeEventListener("mousedown", close); document.removeEventListener("keydown", key); };
  }, [menuOpen]);

  useEffect(() => {
    branchRequestSeq.current += 1;
    setMenuOpen(false);
    // 会话/工作区切换（或新一轮开始）时收掉确认层：分支名属于上一个工作区，留着会切错。
    setPendingSwitch(null);
    void load();
  }, [load, runKey]);

  // 未提交改动数优先用服务端给的 changes 清单（一个文件同时 staged+modified 只算一处），
  // 旧版响应没带清单时才退回分项计数相加（含 conflicted）。
  const dirtyCount = snapshot
    ? snapshot.changes ? snapshot.changes.length
      : snapshot.worktree.staged + snapshot.worktree.modified + snapshot.worktree.untracked + snapshot.worktree.deleted + snapshot.worktree.renamed + snapshot.worktree.conflicted
    : 0;

  if (phase === "error") {
    return (
      <button type="button" className="branch-pill error" title={`分支读取失败：${errorText} · 点击重试`} onClick={() => void load()}>
        <BranchPillIcon />
        读取失败
      </button>
    );
  }
  if (phase !== "ready" || !snapshot || !snapshot.head) {
    return <span className="branch-pill" title="正在读取分支状态…"><BranchPillIcon />…</span>;
  }
  const head = snapshot.head;
  const switchConfirm = pendingSwitch && createPortal(<ConfirmDialog title="切换分支" confirmLabel="切换分支" busy={switching} message={<>将切换到分支 <b>{pendingSwitch.name}</b>。工作区必须干净且没有正在运行的 AI 任务，是否继续？</>} onConfirm={switchBranchConfirmed} onCancel={() => { if (!switching) setPendingSwitch(null); }} />, document.body);
  // 状态卡对 detached / 正常分支是同一份 JSX，只差胶囊本身（原先两份菜单各写一遍，收口到这里）。
  const branchCard = menuOpen && (
    <div className="branch-card" role="group" aria-label="分支">
      <div className="bc-label">当前分支</div>
      <div className="bc-head">
        <BranchPillIcon />
        <span className="bc-branch">{head.detached ? "detached HEAD" : head.branch || "（空仓库）"}</span>
        <span className="bc-sha">{head.oid.slice(0, 7)}</span>
      </div>
      <div className="bc-stats">
        {head.ahead > 0 && <span className="stat">{`↑${head.ahead} 待推送`}</span>}
        {head.behind > 0 && <span className="stat">{`↓${head.behind} 落后`}</span>}
        {dirtyCount > 0 ? <span className="stat amber">{`${dirtyCount}处未提交改动`}</span> : <span className="stat">工作区干净</span>}
      </div>
      <div className="bc-actions">
        <button type="button" className="bc-btn" onClick={() => { setMenuOpen(false); navigate(`/projects/${projectID}/git`); }}>
          查看改动
          <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 12h15M13 6l6 6-6 6" /></svg>
        </button>
      </div>
      <hr className="bc-div" />
      <div className="bc-label">切换到</div>
      {branches.length === 0 ? <div className="bc-empty">没有可切换的本地分支</div> : branches.map((branch) => <button key={branch.name} type="button" title={branch.name} className={branch.current ? "bc-row current" : "bc-row"} disabled={branch.current || switching} onClick={() => void switchBranch(branch)}><span className="name">{branch.current && <span className="bc-check" aria-hidden="true">✓</span>}{branch.name}</span>{branch.current && <small>当前</small>}</button>)}
    </div>
  );
  if (head.detached) {
    return (
      <span className="branch-switcher">
        <button type="button" className="branch-pill detached" title={`工作区处于 detached HEAD（${head.oid.slice(0, 7)}），点击打开分支状态卡`} onClick={() => menuOpen ? setMenuOpen(false) : void loadBranches()}>
          <BranchPillIcon />
          detached HEAD
          {dirtyCount > 0 && <span className="dirty">·{dirtyCount}处改动</span>}
          <span className={`branch-chevron${menuOpen ? " open" : ""}`} aria-hidden="true"><svg viewBox="0 0 24 24"><path d="M6 9l6 6 6-6" /></svg></span>
        </button>
        {branchCard}
        {switchConfirm}
      </span>
    );
  }
  const syncBits = [head.ahead > 0 ? `↑${head.ahead}` : "", head.behind > 0 ? `↓${head.behind}` : ""].filter(Boolean).join("");
  return (
    <span className="branch-switcher">
      <button type="button" className="branch-pill" disabled={switching} title={`当前分支 ${head.branch} · 点击打开分支状态卡`} onClick={() => menuOpen ? setMenuOpen(false) : void loadBranches()}>
        <BranchPillIcon />
        <span className="branch-name">{head.branch || "（空仓库）"}</span>
        {syncBits && <span className="sync">{syncBits}</span>}
        {dirtyCount > 0 && <span className="dirty">·{dirtyCount}处改动</span>}
        <span className={`branch-chevron${menuOpen ? " open" : ""}`} aria-hidden="true"><svg viewBox="0 0 24 24"><path d="M6 9l6 6 6-6" /></svg></span>
      </button>
      {branchCard}
      {switchConfirm}
    </span>
  );
}

function BranchPillIcon() {
  return <svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="6" cy="6" r="2.2" /><circle cx="6" cy="18" r="2.2" /><circle cx="18" cy="8" r="2.2" /><path d="M6 8.2v7.6M18 10.2c0 3.6-4.4 3.4-7.6 4" /></svg>;
}

function ComposerRunnerInfo({ runnerID, agentID, projectID, conversationID, isGitRepo, run, runLabel, usage, readOnly, stopping, onStop }: { runnerID: string; agentID: AgentID; projectID: string; conversationID: string; isGitRepo: boolean; run: string; runLabel: string; usage: ConversationUsageResponse | null; readOnly: boolean; stopping: boolean; onStop: () => void }) {
  const [runner, setRunner] = useState<RunnerInfo | null>(null);
  const [checking, setChecking] = useState(false);
  const [updateInfo, setUpdateInfo] = useState<CheckUpdateResult | null>(null);
  const [updating, setUpdating] = useState(false);
  const [updateError, setUpdateError] = useState<string | null>(null);
  const [showConfirm, setShowConfirm] = useState(false);
  const tool = runnerAgentStatus(runner, agentID);
  const toolName = agentDisplayName(agentID);

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
      // ⚠️ 带 force：这一页**自己**有一套检查窗口（localStorage 记时间戳，10 分钟内不自动
      // 复查、之后每 30 分钟静默复查）。服务端若再缓存一层，用户点「检查更新」就会拿到
      // 那份缓存读数 —— 而这句话的意思恰恰是"现在去查一次"。
      const result = await api<CheckUpdateResult>(`/api/runners/${runnerID}/agents/${encodeURIComponent(agentID)}/check-update?force=true`, { method: "POST" });
      setUpdateInfo(result);
      // 记录检查时间戳，供自动检查的缓存窗口使用。
      try { window.localStorage.setItem(cacheKey, String(Date.now())); } catch { /* localStorage 可能在隐私模式下不可用 */ }
    } catch (err) {
      if (!silent) setUpdateInfo({ updateAvailable: false, currentVersion: tool?.version || "", error: err instanceof Error ? err.message : "检查更新失败" });
    } finally {
      if (!silent) setChecking(false);
    }
  }, [runnerID, agentID, cacheKey, tool?.version]);

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
    setRunner((prev) => (prev ? withRunnerAgentStatus(prev, agentID, "updating") : prev));
    try {
      const result = await api<UpdateResult>(`/api/runners/${runnerID}/agents/${encodeURIComponent(agentID)}/update`, { method: "POST" });
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
  const manualUpdateCommand = `${agentEntry(agentID)?.commandName ?? agentID} update`;
  const currentVersion = formatToolVersion(tool?.version);
  const latestVersion = formatToolVersion(updateInfo?.latestVersion);

  return (<>
    <div className="composer-meta">
      <span className="composer-meta-group">
      <span className={`runner-inline ${runnerStatusClass}${run ? " run-active" : ""}`} title={tool?.reason} role={run ? "status" : undefined} aria-live={run ? "polite" : undefined}><i aria-hidden="true"></i><span>{run ? runLabel : tool?.status === "ready" ? `${toolName} ${currentVersion || tool.version}` : tool?.status === "updating" ? "更新中..." : tool?.reason || `${toolName} 不可用`}</span></span>
      {/* 非 Git 目录项目没有分支可读：/api/projects/{id}/git/summary 对它一律 409，
          画出来就是一条常驻的「读取失败」胶囊（点了也永远失败）。判据与 GitWorkbenchPage、
          手机端共用 NON_GIT_BRANCH 这一个哨兵，由宿主演算后传进来。 */}
      {isGitRepo && <ComposerBranchPill projectID={projectID} conversationID={conversationID} runKey={run} />}
      {run && <button className="runner-stop" type="button" disabled={readOnly} onClick={onStop} title={readOnly ? "只读会话无法停止" : stopping ? "再次请求停止当前对话" : "停止当前对话"} aria-label={readOnly ? "只读会话无法停止" : stopping ? "再次请求停止当前对话" : "停止当前对话"}><span aria-hidden="true"></span>{stopping ? "再次停止" : "停止"}</button>}
      {!run && tool?.status === "ready" && <button type="button" className="runner-inline-btn" disabled={checking || updating} onClick={() => void handleCheckUpdate()}>{checking ? "检查中..." : "检查更新"}</button>}
      {!run && !updating && hasUpdate && autoUpdatable && <button type="button" className="runner-inline-btn update-available" onClick={() => setShowConfirm(true)}>更新至 {latestVersion || updateInfo?.latestVersion}</button>}
      {!run && !updating && hasUpdate && !autoUpdatable && <span className="runner-inline-manual" title={`${toolName} 检测到新版本 ${latestVersion || updateInfo?.latestVersion || ""}，当前运行器暂不支持应用内自动更新，请在${manualUpdateEnv}手动执行 ${manualUpdateCommand}`}>发现新版本 {latestVersion || updateInfo?.latestVersion} · 需手动更新</span>}
      {!run && !updating && updateInfo && !hasUpdate && !updateInfo.error && <span className="runner-inline-uptodate" title={`${toolName} 已是最新版本`}>已是最新版本</span>}
      {!run && !updating && updateInfo?.error && <span className="runner-inline-error" title={updateInfo.error}>{updateInfo.error}</span>}
      </span>
      <span className="composer-meta-push" aria-hidden="true"></span>
      {usage && <span className="turns" title="本次会话已完成的对话轮数"><svg className="turns-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M20 11.6c0 3.9-3.6 7-8 7-.9 0-1.8-.1-2.6-.4L4.5 19.5l1.1-3.2C4.6 15.2 4 13.5 4 11.6c0-3.9 3.6-7 8-7s8 3.1 8 7Z" /></svg><b>{usage.session.taskCount}</b>次对话</span>}
    </div>
    {/* 更新 CLI 确认弹窗（方案A 绿色小弹窗）：全局 .modal 被全站共用，这里的一切皮肤
        都走 .modal.update-confirm-dialog / .update-confirm-* 专属类，不动全局。
        版本对比独立成行（这是本弹窗最重要的信息），打扰提示单独成条不再埋在句子里。
        版本号沿用 formatToolVersion 口径，读不到就退回原始串，不画空。 */}
    {showConfirm && <div className="backdrop" role="dialog" aria-modal="true"><section className="modal update-confirm-dialog"><header><span className="update-confirm-icon" aria-hidden="true"><AgentToolIcon agent={agentID} /></span><div className="update-confirm-heading"><label>更新 {toolName}</label><h2>确认更新到新版本？</h2></div><button type="button" className="update-confirm-close" title="关闭" aria-label="关闭" onClick={() => setShowConfirm(false)}>×</button></header><div className="update-confirm-body"><div className="update-confirm-versions" role="group" aria-label="版本对比"><span className="update-confirm-version is-current"><small>当前版本</small><code>{currentVersion || tool?.version || "—"}</code></span><svg className="update-confirm-arrow" viewBox="0 0 24 24" aria-hidden="true"><path d="M4 12h15M13 6l6 6-6 6" /></svg><span className="update-confirm-version is-latest"><small>最新版本</small><code>{latestVersion || updateInfo?.latestVersion || "—"}</code></span></div><p className="update-confirm-note"><svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="9" /><path d="M12 8h.01M12 11.5V16" /></svg>更新期间将无法使用 AI 对话功能，预计需要数十秒。</p></div><footer><button type="button" className="secondary" onClick={() => setShowConfirm(false)}>取消</button><button type="button" className="primary" onClick={() => void handleUpdate()}>确认更新</button></footer></section></div>}
    {updateError && <div className="backdrop" role="dialog" aria-modal="true"><section className="modal dialog-studio update-error-dialog"><header><div><label>更新错误</label><h2>更新失败</h2></div><button type="button" className="dialog-close" title="关闭" aria-label="关闭" onClick={() => setUpdateError(null)}>×</button></header><div className="update-error-reason" role="alert">{updateError}</div><footer><button type="button" className="secondary" onClick={() => setUpdateError(null)}>关闭</button></footer></section></div>}
  </>);
}

function UsageDialogIcon() {
  return <svg className="usage-dialog-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M5 19V10M10 19V5M15 19v-7M20 19V8" /><path d="M3.5 19.5h17" /></svg>;
}

// 方案 D（图表主角）的半圆仪表：上下文要有「窗口 + 读数」两个可信值才画弧，
// 缺一边就是「计算中」，只留轨道和占位符，不把读不到画成 0%。
const USAGE_ARC_RADIUS = 88;
const USAGE_ARC_LENGTH = Math.PI * USAGE_ARC_RADIUS;
function UsageArcGauge({ percent, level, pendingText }: { percent: number | null; level: string; pendingText: string }) {
  const clamped = percent === null ? null : Math.min(100, Math.max(0, percent));
  return <div className={`usage-arc ${level}`} role="img" aria-label={clamped === null ? pendingText : `上下文已使用 ${clamped}%`}>
    <svg width="196" height="104" viewBox="0 0 208 116" aria-hidden="true">
      <path className="usage-arc-track" d="M 16 108 A 88 88 0 0 1 192 108" />
      {clamped !== null && <path className="usage-arc-fill" d="M 16 108 A 88 88 0 0 1 192 108" strokeDasharray={`${clamped / 100 * USAGE_ARC_LENGTH} ${USAGE_ARC_LENGTH}`} />}
    </svg>
    <div className="usage-arc-center"><b>{clamped === null ? "--" : `${clamped}%`}</b><span>上下文已用</span></div>
  </div>;
}

// 指标条的小图标。kind 与文案一一对应，改文案时同步改这里的路径。
type UsageMetricKind = "duration" | "ttft" | "turns" | "steps" | "tools" | "subagents";
const USAGE_METRIC_PATHS: Record<UsageMetricKind, React.ReactNode> = {
  duration: <><circle cx="12" cy="12" r="9" /><path d="M12 7v5l3 2" /></>,
  ttft: <path d="M13 2v7h6L11 22v-7H5z" />,
  turns: <><path d="M8 6h13M8 12h13M8 18h13" /><circle cx="4" cy="6" r="1" /><circle cx="4" cy="12" r="1" /><circle cx="4" cy="18" r="1" /></>,
  steps: <><rect x="4" y="4" width="16" height="16" rx="2" /><path d="M9 4v16M4 9h16" /></>,
  tools: <path d="M14.7 6.3a4.5 4.5 0 0 0-6 6L3 18v3h3l5.7-5.7a4.5 4.5 0 0 0 6-6L14 13l-3-3z" />,
  subagents: <><rect x="5" y="7" width="14" height="12" rx="2" /><circle cx="9.5" cy="12.5" r="1" /><circle cx="14.5" cy="12.5" r="1" /><path d="M12 7V4M9 4h6" /></>,
};
function UsageMetricIcon({ kind }: { kind: UsageMetricKind }) {
  return <svg className="usage-metric-icon" viewBox="0 0 24 24" aria-hidden="true">{USAGE_METRIC_PATHS[kind]}</svg>;
}

function UsageChartCardIcon({ kind }: { kind: "tokens" | "cost" }) {
  return <svg className="usage-card-icon" viewBox="0 0 24 24" aria-hidden="true">{kind === "tokens"
    ? <><path d="m12 3 9 5-9 5-9-5z" /><path d="m3 14.5 9 5 9-5" /></>
    : <><path d="M12 3v18M5 8l7-5 7 5" /><path d="M3 21h18" /></>}</svg>;
}

function UsageDialogHeader({ close }: { close: () => void }) {
  return <header><div className="usage-dialog-heading"><span className="usage-dialog-mark"><UsageDialogIcon /></span><h2 id="usage-title">使用状态</h2></div><button type="button" className="usage-dialog-close" title="关闭" aria-label="关闭" onClick={close}><DialogCloseIcon /></button></header>;
}

function UsageDialog({ agentID, usage, close }: { agentID: AgentID; usage: ConversationUsageResponse | null; close: () => void }) {
  const agentName = agentDisplayName(agentID);
  // 计时器仅在本组件挂载时（即弹窗打开时）运行，避免在对话页顶层每秒触发整页重渲染。
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, []);
  if (usage && !usage.available) return <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="usage-title"><section className="modal usage-dialog"><UsageDialogHeader close={close} /><div className="usage-body usage-unavailable"><p className="usage-note">{usage.reason || "当前工具未提供可验证的使用统计。"}</p></div><footer><button className="secondary" onClick={close}>关闭</button></footer></section></div>;
  const task = usage?.currentRun ?? usage?.latestRun;
  // 与模型选择器同一条规矩：按模型聚合的用量可能是 null（Go 的 nil 切片），
  // 只对 usage 做可选链时这里同样会在 null 上读 .length 抛错。
  const modelUsage = modelList(usage);
  const active = Boolean(usage?.currentRun && usage.currentRun.status === "running");
  const hasTaskUsage = Boolean(task?.hasResult);
  const session = usage?.session;
  const context = usage?.context;
  const contextWindow = context?.contextWindow ?? 0;
  const contextTokens = context?.contextInputTokens ?? 0;
  // 窗口与读数都齐了才承认这个百分比；缺一边是「计算中」，不能画成 0%。
  const contextPercent = contextWindow > 0 && contextTokens > 0 ? Math.min(100, Math.round(contextTokens / contextWindow * 100)) : null;
  const contextUsed = contextTokens && contextWindow ? `${formatTokens(contextTokens)} / ${formatTokens(contextWindow)}` : contextTokens ? `已用 ${formatTokens(contextTokens)}` : context?.available === false ? context.reason || "当前工具未提供上下文快照" : context?.hasResult ? "当前工具未提供上下文快照" : "等待上下文快照";
  const contextRemain = contextWindow > 0 && contextTokens > 0 ? formatTokens(Math.max(0, contextWindow - contextTokens)) : null;
  const sessionTokens = session ? session.inputTokens + session.outputTokens + session.cacheReadTokens + session.cacheCreationTokens : 0;
  const taskTokenTotal = hasTaskUsage && task ? task.inputTokens + task.outputTokens + task.cacheReadTokens + task.cacheCreationTokens : 0;
  const taskSegments = hasTaskUsage && task ? [
    { kind: "seg-cache-read", label: "缓存读取", value: task.cacheReadTokens },
    { kind: "seg-input", label: "输入", value: task.inputTokens },
    { kind: "seg-cache-create", label: "缓存创建", value: task.cacheCreationTokens },
    { kind: "seg-output", label: "输出", value: task.outputTokens },
  ] : [];
  const maxModelCost = modelUsage.length ? Math.max(...modelUsage.map((model) => model.estimatedCostUsd)) : 0;
  // 徽章文案不直接吐后端 status（内部叫法不许上屏）；latestRun 残留 running 而判活为假时，
  // 也按运行中渲染而不是露出英文原文。
  const badgeRunning = active || task?.status === "running";
  const taskStatusText = !task
    ? ""
    : active || task.status === "running"
      ? "运行中"
      : task.status === "completed" ? "已完成" : task.status === "stopped" ? "已停止" : task.status === "failed" ? "失败" : task.status;
  // 三态分开：「真的 0」≠「没查成」≠「还在算」。
  const taskNote = !task
    ? "当前会话还没有可用的任务统计。"
    : active
      ? "任务结束后给出最终统计。"
      : task.reason || `该任务未获得 ${agentName} 的最终统计数据。`;
  return <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="usage-title"><section className="modal usage-dialog"><UsageDialogHeader close={close} /><div className="usage-body">
    <section className="usage-arc-banner">
      <UsageArcGauge percent={contextPercent} level={contextLevel(context)} pendingText={contextStateText(context)} />
      <div className="usage-arc-meta">
        <div className="usage-arc-row"><span>已用 / 窗口</span><b title={contextUsed}>{contextUsed}</b></div>
        {contextRemain && <div className="usage-arc-row"><span>剩余可用</span><b>{contextRemain}</b></div>}
        {session && <div className="usage-arc-row"><span>会话累计 tokens</span><b>{formatTokens(sessionTokens)}</b></div>}
        {session && <div className="usage-arc-row"><span>会话费用估算</span><b>{formatCost(session.estimatedCostUsd)}</b></div>}
        {session && <div className="usage-arc-row"><span>任务次数</span><b>{session.taskCount}</b></div>}
      </div>
      <span className={`context-state ${contextLevel(context)}`}>{contextStateText(context)}</span>
    </section>
    {task && <section className="usage-task-head"><h3>当前任务</h3><span className={`usage-run-badge ${badgeRunning ? "running" : task.status}`}>{badgeRunning && <i aria-hidden="true" />}{taskStatusText}</span>{task.model && <span className="usage-task-model" title={task.model}>{task.model}</span>}</section>}
    <div className="usage-chart-cols">
      <section className="usage-chart-card">
        <h4><UsageChartCardIcon kind="tokens" />本次 Token 构成{task && hasTaskUsage && taskTokenTotal > 0 && <span>共 {formatTokens(taskTokenTotal)} tokens · 费用 {formatCost(task.estimatedCostUsd)}</span>}</h4>
        {task && hasTaskUsage && taskTokenTotal > 0 ? <>
          <div className="usage-token-bar">{taskSegments.filter((segment) => segment.value > 0).map((segment) => <i key={segment.kind} className={segment.kind} style={{ flex: `${segment.value} 1 0%` }} title={`${segment.label} ${formatTokens(segment.value)}`} />)}</div>
          <div className="usage-token-legend">{taskSegments.map((segment) => <div key={segment.kind}><i className={segment.kind} />{segment.label}<b>{formatTokens(segment.value)}</b></div>)}</div>
        </> : <p className="usage-note">{hasTaskUsage && taskTokenTotal === 0 ? "本任务没有产生 token 记录。" : taskNote}</p>}
      </section>
      {modelUsage.length > 0 && <section className="usage-chart-card">
        <h4><UsageChartCardIcon kind="cost" />模型费用<span>包含子代理{session ? ` · 会话累计 ${formatCost(session.estimatedCostUsd)}` : ""}</span></h4>
        <div className="usage-model-bars">{modelUsage.map((model) => <div key={model.model} className="usage-model-bar">
          <div className="usage-model-line"><b title={model.model}>{model.model}</b><em>{formatCost(model.estimatedCostUsd)}</em></div>
          <div className="usage-model-lane"><i style={{ width: `${maxModelCost > 0 ? Math.max(2, Math.round(model.estimatedCostUsd / maxModelCost * 100)) : 0}%` }} /></div>
          <span className="usage-model-detail">{formatTokens(model.inputTokens)} 输入 · {formatTokens(model.outputTokens)} 输出</span>
        </div>)}</div>
      </section>}
    </div>
    {task && <div className="usage-metric-strip">
      <div className={`usage-metric${active ? " live" : ""}`}><UsageMetricIcon kind="duration" /><b>{formatDuration(runDuration(task, now))}</b><span>耗时</span></div>
      <div className="usage-metric"><UsageMetricIcon kind="ttft" /><b>{task.ttftMs > 0 ? formatDuration(task.ttftMs) : "等待响应"}</b><span>首个响应</span></div>
      <div className="usage-metric"><UsageMetricIcon kind="turns" /><b>{task.agentTurns || "--"}</b><span>Agent 轮次</span></div>
      <div className="usage-metric"><UsageMetricIcon kind="steps" /><b>{task.modelSteps || "--"}</b><span>模型步骤</span></div>
      <div className="usage-metric"><UsageMetricIcon kind="tools" /><b>{task.toolCalls || "--"}</b><span>工具调用</span></div>
      <div className="usage-metric"><UsageMetricIcon kind="subagents" /><b>{task.subagentCount || "0"}</b><span>子代理</span></div>
    </div>}
    <p className="usage-disclaimer">费用为客户端事件估算值，不代表账单金额。</p>
  </div><footer><button className="secondary" onClick={close}>关闭</button></footer></section></div>;
}

function HistorySearchIcon() {
  return <svg className="history-search-icon" viewBox="0 0 24 24" aria-hidden="true"><circle cx="10.5" cy="10.5" r="5.5" /><path d="m15 15 4.2 4.2" /></svg>;
}

type ConversationHistoryPage = { items: Conversation[]; nextCursor: string };
// 列表刷新的结果。`stale` 表示这次请求已被更晚的一次刷新超越、结果已丢弃，
// 此时 `items` 的空白**不代表**"项目里没有会话"——调用方不能据此下结论。
type ConversationHistoryResult = { items: Conversation[]; stale: boolean };
// 由"当前页面上的弹窗"持有的 search 参数。无 id 分支恢复会话、补全 URL 时必须原样带过去，
// 否则用户刚打开的弹窗会被那次 navigate 顺手关掉。（`readonly` 是会话视图模式，不在此列。）
const DIALOG_SEARCH_PARAMS = ["history", "new", "usage", "execution", "config"] as const;
type ConversationActivityPosition = { createdAt: string; id: string };
type ConversationActivityItem = { conversationId: string; events: Event[]; latestPosition?: ConversationActivityPosition | null; truncated: boolean };
type ConversationActivityResponse = { conversations: ConversationActivityItem[]; missingConversationIds: string[] };

function ConversationHistoryDialog({ conversations, historyQuery, loading, error, activeID, busyID, deletingID, deleteAllBusy, close, activate, view, search, deleteOne, deleteAll, hasMore, loadingMore, loadMore }: {
  conversations: Conversation[];
  /** 父组件当前的查询词。用来判断"打开弹窗"要不要再搜一次（打开时两者相等 ⇒ 不搜）。 */
  historyQuery: string;
  /** 列表请求还在飞。缺了它，加载中的空列表会被渲染成「没有匹配的会话」。 */
  loading: boolean;
  /** 最后一次列表刷新的失败原因。失败同样没有结论，不能显示成"没有记录"。 */
  error: string;
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
  // 打开弹窗时父组件已按 `historyQuery` 刷过一次列表，所以只有等用户真的改了关键词
  // （本地 query 与父组件当前查询不一致）才需要再搜。曾经无条件 debounce 200ms 再搜一次，
  // 每次打开都发两个互相作废的请求，先到的那次必然白跑 —— 列表"卡在空"就是这个窗口。
  useEffect(() => {
    if (query === historyQuery) return;
    const timer = window.setTimeout(() => search(query), 200);
    return () => window.clearTimeout(timer);
  }, [query, historyQuery, search]);
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
  // 空列表有三种真相，必须分开："还在请求" / "请求失败" / "真的没有"。
  // 把它们渲染成同一句话，正是用户报的"历史对话框出来了，但没有显示任何历史记录"。
  const emptyText = loading ? "正在加载会话…" : error ? `会话列表加载失败：${error}` : "没有匹配的会话";
  const emptyClass = loading ? "history-empty history-loading" : error ? "history-empty history-error" : "history-empty";
  const statusText = conversations.length > 0 ? "" : loading ? "正在加载…" : error ? "加载失败" : "";
  return <div className="backdrop history-backdrop" role="dialog" aria-modal="true" aria-labelledby="conversation-history-title"><section className="modal conversation-history"><header><div className="conversation-history-heading"><span className="conversation-history-mark"><HistoryIcon /></span><div><h2 id="conversation-history-title">会话历史</h2></div></div><button type="button" className="conversation-history-close" title="关闭" aria-label="关闭" onClick={close}><DialogCloseIcon /></button></header><div className="history-toolbar"><label className="history-search"><HistorySearchIcon /><input autoFocus value={query} onChange={(event) => setQuery(event.target.value)} onKeyDown={keyDown} placeholder="搜索会话标题或内容" /></label><span>{statusText || (query ? `${conversations.length} 个匹配` : `${conversations.length} 个会话`)}</span></div><div className="history-list">{conversations.length === 0 ? <p className={emptyClass}>{emptyText}</p> : conversations.map((item, index) => {
    const orchestration = item.isOrchestration === true;
    const runningElsewhere = item.status === "running" && item.id !== activeID;
    const state = busyID === item.id ? "切换中" : item.id === activeID ? "当前会话" : runningElsewhere ? "运行中" : orchestration ? "自动编排" : "";
    const isDeleting = deletingID === item.id;
    const rowDisabled = Boolean(busyID) || isDeleting || Boolean(deleteAllBusy);
    return <div key={item.id} className={`history-item ${busyID === item.id ? "activating" : ""} ${item.id === activeID ? "active" : ""} ${index === selected ? "selected" : ""} ${runningElsewhere ? "running" : ""}`} onMouseEnter={() => setSelected(index)}><button type="button" className="history-item-select" disabled={rowDisabled} onClick={() => runningElsewhere || orchestration ? view(item) : select(item)}><span className="history-item-main"><span className="history-item-title"><b>{item.title || "新会话"}</b><span className={`history-item-agent ${item.agentId}`}>{agentDisplayName(item.agentId)}</span>{orchestration && <span className="history-item-orchestration">自动编排</span>}</span><small>{item.preview || "尚未发送消息"}</small></span><span className="history-item-meta">{state && <em>{state}</em>}<time>{formatHistoryTime(item.lastActivityAt)}</time></span></button>{!orchestration && <button type="button" className="history-item-delete" title="删除此会话" aria-label={`删除会话 ${item.title || "新会话"}`} disabled={rowDisabled} onClick={() => deleteOne(item)}><ConversationDeleteIcon /></button>}</div>;
  })}</div>{hasMore && <button className="secondary load-earlier-history" type="button" disabled={loadingMore} onClick={loadMore}>{loadingMore ? "加载中" : "加载更多会话"}</button>}<footer><span>{busyID ? "正在切换会话" : statusText || `${conversations.length} 条记录`}</span><div className="history-footer-actions"><button className="secondary history-delete-all" type="button" title="清除全部历史对话" disabled={Boolean(busyID) || deletingID !== "" || deleteAllBusy} onClick={deleteAll}>清除全部</button><button className="secondary" type="button" onClick={close}>关闭</button></div></footer></section></div>;
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
  // 每个工具的默认权限模式由服务端偏好给出（agentPermissionModes 按工具目录索引）。
  // 它还没回来时用目录声明的默认值，**不去猜另一个工具的设置** —— 猜错的表现是
  // 用户选了 Claude 却拿到 Codex 的沙箱策略。
  const permissionForAgent = (agent: AgentID): PermissionMode => defaults.agentPermissionModes?.[agent] ?? agentEntry(agent)?.defaultPermissionMode ?? "approval_required";
  const [agentId, setAgentId] = useState<AgentID>(defaults.defaultAgentId);
  const [permissionMode, setPermissionMode] = useState<PermissionMode>(() => permissionForAgent(defaults.defaultAgentId));
  const [creating, setCreating] = useState(false);
  const [runner, setRunner] = useState<RunnerInfo | null>(null);
  const [runnerLoading, setRunnerLoading] = useState(true);
  const [runnerError, setRunnerError] = useState("");
  const [profiles, setProfiles] = useState<AgentProfile[]>([]);
  const [profileID, setProfileID] = useState("");
  const agentSelectedByUser = useRef(false);
  const navigate = useNavigate();
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
  const catalogState = useAgentCatalogState();
  const catalogEntries = catalogState.entries;
  // 对话只能选「已实现 AgentRunner、可真正运行」的工具。目录里还有"已收录但尚未接通
  // 对话运行"的工具（runnableInProject=false）：它们出现在管理页可安装可登录，但
  // **不在新建会话里被列为可选项**，避免出现一个点了跑不起来的卡片。
  // 用 useMemo 固定引用：filter 每次渲染生成新数组，若直接进下方 useEffect 依赖会造成
  // 每次渲染都重跑那个"选默认 Agent"的 effect（值相同 setState 会跳过，但仍是无效负
  // 载，且横竖与目录稳定性无关）。
  const runnableAgentEntries = useMemo(() => catalogEntries.filter((entry) => entry.runnableInProject !== false), [catalogEntries]);
  // 目录也参与"检查中"：还在读目录时不能给出工具清单，否则会先闪一个空列表，
  // 而"还没读到"与"没有工具"必须分开渲染。
  const capabilitiesLoading = defaultsLoading || runnerLoading || !catalogState.loaded;
  const isAgentAvailable = (agent: AgentID) => runnerAgentReady(runner, agent);
  useEffect(() => {
    if (capabilitiesLoading || defaultsError) return;
    const preferredAgent = defaults.defaultAgentId;
    // 回落顺序由目录决定，不再写死"先试 claude 再试 codex"。
    const fallbackAgent = runnableAgentEntries.map(catalogAgentID).find(isAgentAvailable);
    const nextAgent = isAgentAvailable(preferredAgent) ? preferredAgent : fallbackAgent ?? preferredAgent;
    if (agentSelectedByUser.current && isAgentAvailable(agentId)) return;
    setAgentId(nextAgent);
    setPermissionMode(permissionForAgent(nextAgent));
  }, [agentId, capabilitiesLoading, runnableAgentEntries, defaults.defaultAgentId, defaultsError, runner]);
  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => { if (event.key === "Escape" && !creating) close(); };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [close, creating]);
  const selectAgent = (next: AgentID) => { agentSelectedByUser.current = true; setAgentId(next); setProfileID(""); setPermissionMode(permissionForAgent(next)); };
  const submit = async () => { if (defaultsError || capabilitiesLoading || !isAgentAvailable(agentId)) return; setCreating(true); try { await create(agentId, permissionMode, profileID || undefined); } finally { setCreating(false); } };
  const selectedAgentName = agentDisplayName(agentId);
  const fallbackReason = !defaultsError && !capabilitiesLoading && !isAgentAvailable(defaults.defaultAgentId) && runnableAgentEntries.some((entry) => isAgentAvailable(catalogAgentID(entry)))
    ? `${agentDisplayName(defaults.defaultAgentId)} 当前不可用，已选择可用工具。`
    : "";
  const unavailableReason = runnerAgentStatus(runner, agentId)?.reason;
  // 权限选项由目录的 permissionModes 生成，文案按**模式**取（同一模式在不同工具上同名）。
  const permissionOptions: Array<{ mode: PermissionMode; title: string; detail: string }> = agentPermissionModes(agentId).map((mode) => ({ mode, ...permissionCopy[mode] }));
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
            {/* 方案 B（2026-09-24，与手机端新会话同一套语言）：三列徽标大卡，图形主导。
                副标题 = 版本 + 厂商；就绪/不可用仍是一行独立读数（不可用必须显式说，
                不能只靠卡片变灰）；「默认」角标挪到左上，右上让给选中勾。 */}
            <div className="new-conversation-agent-grid" role="radiogroup" aria-label="CLI 工具">
              {runnableAgentEntries.map((entry) => {
                const agent = catalogAgentID(entry);
                const available = isAgentAvailable(agent);
                const selected = agentId === agent;
                const status = runnerAgentStatus(runner, agent);
                const name = entry.name;
                const subtitle = [formatToolVersion(status?.version), entry.vendor].filter(Boolean).join(" · ");
                return <button key={agent} type="button" className={`new-conversation-agent-card${selected ? " selected" : ""}`} role="radio" aria-checked={selected} disabled={!available || creating} title={!available ? status?.reason || `${name} 不可用` : name} onClick={() => selectAgent(agent)}>
                  <span className={`new-conversation-agent-mark ${agent}`}><AgentToolIcon agent={agent} /></span>
                  <span className="new-conversation-agent-copy"><b>{name}</b><small>{subtitle}</small></span>
                  <span className={`new-conversation-agent-state ${available ? "ready" : "unavailable"}`}>{available ? "已就绪" : "不可用"}</span>
                  {selected && <i className="new-conversation-agent-check" aria-hidden="true">✓</i>}
                  {agent === defaults.defaultAgentId && <em className="new-conversation-agent-default">默认</em>}
                </button>;
              })}
            </div>
            {runnableAgentEntries.length > 0 && !runnableAgentEntries.some((entry) => isAgentAvailable(catalogAgentID(entry))) && <div className="new-conversation-no-tool"><p className="new-conversation-error inline">当前没有可用的 CLI 工具。{runnerError || unavailableReason || "请先安装并登录，再回来创建会话。"}</p><button type="button" className="new-conversation-manage-tools" onClick={() => { close(); navigate("/cli-tools"); }}>去管理 CLI 工具</button></div>}
          </section>
          <section className="new-conversation-section new-conversation-permission-section" aria-labelledby="new-conversation-permission-label">
            <div className="new-conversation-section-heading"><div><span>02</span><h3 id="new-conversation-permission-label">执行权限</h3></div><small>{selectedAgentName}</small></div>
            {/* 权限从纵向卡列表改为分段控件（与手机端同语言）：档位少（2~3）时
                分段比逐档卡片更轻；每档的说明收敛成分段下方的一行提示，选中哪档念哪档。 */}
            <div className="new-conversation-permission-seg" role="radiogroup" aria-label={`${selectedAgentName} 执行权限`}>
              {permissionOptions.map((option) => <button key={option.mode} type="button" role="radio" aria-checked={permissionMode === option.mode}
                className={permissionMode === option.mode ? "active" : ""}
                title={option.detail} disabled={creating} onClick={() => setPermissionMode(option.mode)}>{option.title}</button>)}
            </div>
            <p className="new-conversation-permission-hint">{permissionOptions.find((option) => option.mode === permissionMode)?.detail}</p>
          </section>
          {/* 配置档案（胶囊行，方案 1）：所有档案一眼可见、选哪个点哪个，不再藏进下拉。
              档案是低频高级选项，整行放在权限之后、创建按钮之前。 */}
          {availableProfiles.length > 0 && <div className="new-conversation-profile-chips" role="radiogroup" aria-label="配置档案（可选）">
            <span className="new-conversation-profile-chips-label">配置档案<small>可选</small></span>
            <button type="button" role="radio" aria-checked={profileID === ""} className={`new-conversation-profile-chip${profileID === "" ? " selected" : ""}`} disabled={creating} onClick={() => setProfileID("")}>
              <span className="new-conversation-profile-dot" aria-hidden="true"></span><b>CLI 当前登录配置</b>
            </button>
            {availableProfiles.map((profile) => <button key={profile.id} type="button" role="radio" aria-checked={profileID === profile.id}
              className={`new-conversation-profile-chip${profileID === profile.id ? " selected" : ""}`} disabled={creating}
              onClick={() => setProfileID(profile.id)}>
              <span className="new-conversation-profile-dot" aria-hidden="true"></span><b>{profile.name}</b>
              {profile.model && <span className="new-conversation-profile-model">{profile.model}</span>}
            </button>)}
          </div>}
        </>}
      </div>
      <footer><span className="new-conversation-summary">{capabilitiesLoading || defaultsError ? "" : `${selectedAgentName} · ${permissionOptions.find((option) => option.mode === permissionMode)?.title || "默认权限"}`}</span><button className="secondary" type="button" disabled={creating} onClick={close}>取消</button><button className="primary" type="button" disabled={Boolean(defaultsError) || capabilitiesLoading || creating || !isAgentAvailable(agentId)} onClick={() => void submit()}>{creating ? "创建中..." : "创建会话"}</button></footer>
    </section>
  </div>;
}

function FullControlConfirmationDialog({ close, confirm, changing, agentID }: { close: () => void; confirm: () => Promise<void>; changing: boolean; agentID: string }) {
  const agentName = agentDisplayName(agentID);
  return <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="full-control-title" onClick={(e) => { if (e.target === e.currentTarget) close(); }}><section className="modal dialog-studio permission-dialog"><header><div><label>{agentName.toUpperCase()} PERMISSION</label><h2 id="full-control-title">切换为完全控制</h2></div><button type="button" className="dialog-close" title="关闭" aria-label="关闭" disabled={changing} onClick={close}>×</button></header><p className="permission-confirmation">完全控制会允许 {agentName} 直接执行所有命令，不再逐条确认；若该工具提供沙箱，此模式会绕过它。</p><footer><button className="secondary" disabled={changing} onClick={close}>取消</button><button className="primary danger" disabled={changing} onClick={() => void confirm()}>{changing ? "切换中" : "确认切换"}</button></footer></section></div>;
}

// 消息卡片（2026-10-23 动作行改版）：复制与回退同住气泡下方的 .message-actions 行 ——
//   - 复制：所有消息都有，28px ghost 图标钮（与回退同一支 SVG 描边语言）；
//   - 回退：只有用户消息、且这一轮在服务端留了节点时才有（节点按 runId 关联），
//     hover / focus 该消息时显形；时间线上最后一条用户消息常驻显形（可发现性兜底）；
//   - 这一轮还在跑：动作行先说"本轮结束后可回退到此处"（节点要等 run 成功结束才生成）。
// 显形策略收在 conversation.css 的 @media (hover: hover) 里：触屏没有 hover，直接常显。
const MessageCard = memo(function MessageCard({ message, agentID, fail, checkpoint, checkpointRunning, discarded, rewindDisabled, checkpointBusy, rewind, persistent }: {
  message: Message;
  agentID: AgentID;
  fail: (message: string) => void;
  /** 这一轮的回退节点（按 runId 关联，只有用户消息可能有）。 */
  checkpoint?: ConversationCheckpoint;
  /** 这一轮正在跑：节点要等 run 成功结束才生成，动作行先挂 pending 说明。 */
  checkpointRunning?: boolean;
  /** 回退到这个节点会丢几轮（MessageList 按 discardedTurnsAfter 同一判据算好传入）。 */
  discarded?: number;
  rewindDisabled?: boolean;
  checkpointBusy?: boolean;
  /** 回退回调（MessageList 里已按 rewindPrimaryMode 钉死默认动作，身份稳定不破 memo）。 */
  rewind?: (checkpoint: ConversationCheckpoint) => void;
  /** 时间线上最后一条用户消息：动作行常驻显形。 */
  persistent?: boolean;
}) {
  const isUser = message.role === "user";
  const agentName = agentDisplayName(agentID);
  const [copied, setCopied] = useState(false);
  const copiedTimer = useRef<number | null>(null);

  useEffect(() => () => {
    if (copiedTimer.current) window.clearTimeout(copiedTimer.current);
  }, []);

  const copy = async () => {
    if (!(await copyToClipboard(message.content))) {
      fail("无法复制消息，请检查浏览器剪贴板权限。");
      return;
    }
    setCopied(true);
    if (copiedTimer.current) window.clearTimeout(copiedTimer.current);
    copiedTimer.current = window.setTimeout(() => setCopied(false), 1_500);
  };

  // 回退按钮的提示与行内小字：代价说明沿用共用模块（无代码快照 / 会丢几轮），
  // hover 显形时可见；确认框里还有完整版。
  const notes = checkpoint ? rewindHint(checkpoint, discarded ?? 0) : "";
  const rewindTitle = checkpoint ? `回退到本轮结束时：${checkpointHasCode(checkpoint) ? "代码与对话都回到这一刻" : "只回退对话（该节点没有代码快照）"}${(discarded ?? 0) > 0 ? `，之后 ${discarded} 轮不再显示` : ""}` : "";

  return <article className={`message ${message.role}`}><header><span className="message-avatar">{isUser ? "你" : agentName.slice(0, 1).toUpperCase()}</span><b>{isUser ? "你" : agentName}</b><time>{formatTime(message.createdAt)}</time></header><div className="markdown"><Markdown content={message.content} /></div><div className="message-actions" data-keep={persistent ? "" : undefined}><button type="button" className={`message-copy reveal${copied ? " copied" : ""}`} title={copied ? "已复制" : "复制消息"} aria-label={copied ? "已复制消息" : "复制消息"} onClick={() => void copy()}>{copied ? <CopyDoneIcon /> : <CopyIcon />}</button>{copied && <span className="copy-feedback" role="status">已复制</span>}{checkpoint && rewind && <button type="button" className="message-rewind reveal" disabled={rewindDisabled || checkpointBusy} title={rewindTitle} aria-label="回退到此处" onClick={() => rewind(checkpoint)}><RewindInlineIcon /></button>}{checkpoint && notes && <small className="reveal">{notes}</small>}{!checkpoint && checkpointRunning && <span className="checkpoint-pending">本轮结束后可回退到此处</span>}</div></article>;
});

const SystemCard = memo(function SystemCard({ system }: { system: SystemItem }) {
  const icon: Record<SystemVariant, string> = { compact: "◐", compact_result: "✓", compact_boundary: "≡", api_retry: "↻", session: "◌", task: "▸" };
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
  return <span className="markdown-image-reference" role="note">图片：{externalImage ? <ExternalLink href={externalImage} target="_blank" rel="noreferrer">{label}</ExternalLink> : label}</span>;
}

const Markdown = memo(function Markdown({ content }: { content: string }) {
  return <ReactMarkdown remarkPlugins={[remarkGfm]} skipHtml components={{ ...markdownCodeComponents, a: ({ href, children }) => <ExternalLink href={href} target="_blank" rel="noreferrer">{children}</ExternalLink>, img: MarkdownImage }}>{content}</ReactMarkdown>;
});

// MCP 工具名形如 mcp__<server>__<tool>；拆出可读的两段，避免界面直接显示原始标识。
function mcpToolMeta(name: string): { server: string; tool: string } | null {
  if (!name.startsWith("mcp__")) return null;
  const rest = name.slice(5);
  const sep = rest.indexOf("__");
  if (sep < 0) return { server: rest, tool: "" };
  return { server: rest.slice(0, sep), tool: rest.slice(sep + 2) };
}

function toolCardTitle(name: string): string {
  if (name === "Bash") return "终端命令";
  const mcp = mcpToolMeta(name);
  if (mcp) return `MCP 工具调用 · ${mcp.server}`;
  return name;
}

const ToolCard = memo(function ToolCard({ action, resolving, decide }: { action: ToolAction; resolving: string; decide: (approvalId: string, decision: "allow" | "deny") => Promise<void> }) {
  const mcp = mcpToolMeta(action.name);
  const command = typeof action.input.command === "string" ? action.input.command : "";
  const description = typeof action.input.description === "string" ? action.input.description : mcp ? `${mcp.server} · ${mcp.tool}` : action.name;
  const parameterPreview = mcp && !command ? JSON.stringify(action.input ?? {}, null, 2) : "";
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
  return <article className={`tool-card ${waiting ? "waiting" : ""}`}><header><div><span className="tool-icon" aria-hidden="true">{">_"}</span><div><b>{toolCardTitle(action.name)}</b><small>{description}</small></div></div><div className="tool-meta"><time>{formatTime(action.createdAt)}</time><span className={`tool-status ${statusClass}`}>{status}</span></div></header>{command && <pre className="command"><code>{command}</code></pre>}{!command && parameterPreview && <pre className="command"><code>{parameterPreview}</code></pre>}{waiting && approval && <div className="approval-actions"><span>{mcp ? "该 MCP 工具调用将由本次会话执行。" : "此命令将会在当前项目目录执行。"}</span><div><button className="secondary" disabled={resolving === approval.approvalId} onClick={() => void decide(approval.approvalId, "deny")}>拒绝</button><button className="primary" disabled={resolving === approval.approvalId} onClick={() => void decide(approval.approvalId, "allow")}>{resolving === approval.approvalId ? "处理中" : "允许执行"}</button></div></div>}{action.output && (shouldCollapseOutput ? <details><summary>{outputLabel}<span>{outputPreview}</span></summary><pre className="output">{output}</pre></details> : <pre className={`output inline ${failed ? "error-output" : ""}`}>{output}</pre>)}</article>;
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
  return <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="agent-execution-title" onClick={(e) => { if (e.target === e.currentTarget) close(); }}><section className="modal agent-execution-dialog"><header><div><h2 id="agent-execution-title">子代理过程</h2><p>{execution.incomplete ? "主回合已完成，但部分子代理的最终结果未送达。" : "查看每个子代理已收到的输出、工具调用和结果。"}</p></div><button type="button" className="dialog-close" title="关闭" aria-label="关闭" onClick={close}>×</button></header><div className="agent-execution-summary-bar">{(["running", "completed", "failed", "stopped", "unresolved"] as AgentStatus[]).map((s) => { const c = counts[s]; if (!c) return null; return <span key={s} className={`summary-chip ${s}`}><span className="dot"></span>{agentStatusLabel(s)}: {c}</span>; })}</div><div className="agent-execution-body"><nav className="agent-tree" aria-label="子代理列表">{execution.agents.map((agent) => <AgentTree key={agent.id} agent={agent} selectedID={selectedID} select={setSelectedID} depth={0} />)}</nav><section className="agent-log-panel">{selected ? <><header><div><span className={`agent-status ${selected.status}`}>{agentStatusLabel(selected.status)}</span><h3>{selected.summary}</h3></div><small>{formatTime(selected.createdAt)}</small></header><div className="agent-log-list">{selected.logs.length === 0 ? <p className="agent-log-empty">尚未收到该子代理的过程输出。</p> : selected.logs.map((log) => <article className={`agent-log ${log.kind} ${log.isError ? "failed" : ""}`} key={log.id}><header><span className={`log-kind-badge ${log.kind}`}>{log.kind === "text" ? "文本" : log.kind === "tool" ? "工具" : log.kind === "result" ? "结果" : "错误"}</span><b>{log.title}</b><time>{formatTime(log.createdAt)}</time></header><details open={log.kind === "text"}><summary>{log.detail.replace(/\s+/g, " ").slice(0, 180) || "无输出"}</summary><pre>{log.detail || "(无输出)"}</pre></details></article>)}</div></> : <p className="agent-log-empty">没有可查看的子代理。</p>}</section></div><footer><span>{agents.length} 个子代理 · {counts.completed} 完成{counts.failed > 0 ? ` · ${counts.failed} 失败` : ""}{counts.running > 0 ? ` · ${counts.running} 执行中` : ""}</span><button className="secondary" type="button" onClick={close}>关闭</button></footer></section></div>;
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
            {conversation && <span className="conversation-tab-agent">{agentDisplayName(conversation.agentId)}</span>}
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
  const mcp = mcpToolMeta(action.name);
  const command = typeof action.input.command === "string" ? action.input.command : "";
  const description = typeof action.input.description === "string" ? action.input.description : "";
  const fallback = mcp ? `${mcp.server} · ${mcp.tool}` : "";
  const detail = command || (mcp ? JSON.stringify(action.input ?? {}) : "");
  const approval = action.approval;
  if (!approval) return null;
  return <div className="approval-banner">
    <div className="approval-banner-body">
      <span className="approval-banner-icon">⏳</span>
      <div className="approval-banner-info">
        <b>{mcp ? "等待确认 MCP 工具调用" : "等待确认命令执行"}</b>
        <span>{description || command || fallback}</span>
      </div>
      {detail && <code className="approval-banner-command">{detail}</code>}
      <div className="approval-banner-actions">
        <button type="button" className="secondary" disabled={resolving === approval.approvalId} onClick={() => void decide(approval.approvalId, "deny")}>拒绝</button>
        <button type="button" className="primary" disabled={resolving === approval.approvalId} onClick={() => void decide(approval.approvalId, "allow")}>{resolving === approval.approvalId ? "处理中" : "允许执行"}</button>
        <button type="button" className="approval-banner-scroll" onClick={scrollToCard} title="滚动到命令卡片">↓</button>
      </div>
    </div>
  </div>;
}

function ShortcutVariablesDialog({ state, close, run }: { state: { shortcut: Shortcut; variables: Record<string, string> }; close: () => void; run: (variables: Record<string, string>) => void }) {
  const required = requiredShortcutVariables(state.shortcut.template);
  const [variables, setVariables] = useState(state.variables);
  useEffect(() => { setVariables(state.variables); }, [state]);
  const submit = (event: FormEvent) => { event.preventDefault(); run(variables); };
  return <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="shortcut-variables-title"><section className="modal dialog-studio shortcut-variables-dialog"><header><div><h2 id="shortcut-variables-title">{state.shortcut.name}</h2></div><button type="button" className="dialog-close" title="关闭" aria-label="关闭" onClick={close}>×</button></header><form onSubmit={submit}><div className="shortcut-editor-body">{required.includes("selection") && <label>选中内容<textarea autoFocus required value={variables.selection || ""} onChange={(event) => setVariables((old) => ({ ...old, selection: event.target.value }))} placeholder="粘贴需要处理的内容" /></label>}{required.includes("error") && <label>错误信息<textarea autoFocus={!required.includes("selection")} required value={variables.error || ""} onChange={(event) => setVariables((old) => ({ ...old, error: event.target.value }))} placeholder="粘贴报错信息" /></label>}</div><footer><button type="button" className="secondary" onClick={close}>取消</button><button className="primary">发送</button></footer></form></section></div>;
}

// ShortcutEditor —— 新增/编辑快捷项。命令类的默认入口是「从 CLI 命令目录里选」，
// 自由输入降级为第二入口（docs/37 §3.1）：
//   - CLI 命令（`/compact`）由 CLI 自己执行，只能从目录里选，用户凭空写一个就是碰运气；
//   - 自定义 shell 命令（`pnpm test`）本质是提示词，交给模型执行，无法枚举，必须保留输入。
function ShortcutEditor({ projectID, state, close, refresh, fail, agentID, catalog, catalogLoading, refreshCatalog }: { projectID: string; state: ShortcutEditorState; close: () => void; refresh: () => Promise<void>; fail: (message: string) => void; agentID: AgentID; catalog: ProjectCommands | null; catalogLoading: boolean; refreshCatalog: () => Promise<void> }) {
  const shortcut = state.shortcut;
  const [name, setName] = useState(shortcut?.name || "");
  const [template, setTemplate] = useState(shortcut?.template || "");
  const [enabled, setEnabled] = useState(shortcut?.enabled ?? true);
  const [busy, setBusy] = useState(false);
  const [pendingConfirm, setPendingConfirm] = useState<{ title: string; message: React.ReactNode; danger?: boolean; onConfirm: () => void; onCancel: () => void } | null>(null);
  const isCommand = state.kind === "command_request";
  const isSnippet = state.kind === "snippet";
  // 有的 CLI 不解析斜杠命令（例如 Codex 的 `codex exec`：斜杠只存在于它的 TUI 层），
  // 因此命令类只剩"自定义 shell 命令"这一条路。
  // 判据来自目录的能力声明，不再写死"只有 codex 不行"——否则新工具会被默认当成支持。
  const supportsCLICommands = agentSupportsSlashCommands(agentID);
  const [mode, setMode] = useState<"cli" | "custom">(() => (isCommand && supportsCLICommands && (slashCommandName(shortcut?.template || "") !== "" || !shortcut) ? "cli" : "custom"));
  const [selected, setSelected] = useState(() => slashCommandName(shortcut?.template || ""));
  // 名称默认跟随命令的中文名，但只在用户没自己改过时才覆盖（免得把用户起的名冲掉）。
  const [autoNamed, setAutoNamed] = useState(() => (shortcut?.name || "") === "" || (shortcut?.name || "") === (shortcut?.template || "").trim());

  useEffect(() => {
    setName(state.shortcut?.name || "");
    setTemplate(state.shortcut?.template || "");
    setEnabled(state.shortcut?.enabled ?? true);
    setSelected(slashCommandName(state.shortcut?.template || ""));
    setAutoNamed(!state.shortcut || (state.shortcut.name || "") === (state.shortcut.template || "").trim());
    setMode(isCommand && supportsCLICommands && (slashCommandName(state.shortcut?.template || "") !== "" || !state.shortcut) ? "cli" : "custom");
  }, [state, isCommand, supportsCLICommands]);

  // 选中的命令来自目录时，模板由命令名派生，避免"选了 A 却存下 B"。
  const resolvedTemplate = mode === "cli" ? `/${selected}` : template;
  const selectedCommand = catalog?.commands.find((command) => command.name === selected);

  const pickCommand = (command: AgentCommandOption) => {
    setSelected(command.name);
    if (autoNamed || !name.trim()) {
      setName(command.label || command.name);
      setAutoNamed(true);
    }
  };

  const save = async (event: FormEvent) => {
    event.preventDefault();
    if (mode === "cli" && !selected) { fail("请先从列表里选择一条命令"); return; }
    setBusy(true);
    const scope = shortcut?.scope || "local";
    try {
      await api(shortcut ? `/api/shortcuts/${shortcut.id}` : "/api/shortcuts", {
        method: shortcut ? "PATCH" : "POST",
        body: JSON.stringify({
          name,
          description: shortcut?.description || "",
          kind: state.kind,
          template: resolvedTemplate,
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

  return <><div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="shortcut-editor-title"><section className={`modal shortcut-editor${isCommand ? " command-editor" : " prompt-editor"}`}><header><div className="shortcut-editor-heading"><span className="shortcut-editor-mark"><ShortcutCategoryIcon kind={isCommand ? "command" : "prompt"} /></span><div><label>{isCommand ? "COMMON COMMAND" : "COMMON PROMPT"}</label><h2 id="shortcut-editor-title">{shortcut ? `编辑${isCommand ? "命令" : "提示词"}` : `新增${isCommand ? "命令" : "提示词"}`}</h2></div></div><button type="button" className="shortcut-editor-close" title="关闭" aria-label="关闭" disabled={busy} onClick={close}><DialogCloseIcon /></button></header><form onSubmit={(event) => void save(event)}><div className="shortcut-editor-body">
    {isCommand && <div className="command-picker-modes" role="tablist" aria-label="命令类型">
      <button type="button" role="tab" aria-selected={mode === "cli"} className={mode === "cli" ? "active" : ""} disabled={!supportsCLICommands} onClick={() => setMode("cli")}>CLI 命令</button>
      <button type="button" role="tab" aria-selected={mode === "custom"} className={mode === "custom" ? "active" : ""} onClick={() => setMode("custom")}>自定义 shell 命令</button>
    </div>}
    {isCommand && mode === "cli" && <div className="command-picker">
      {catalogLoading && <p className="command-picker-hint">正在读取 {agentDisplayName(agentID)} 的命令目录…</p>}
      {!catalogLoading && catalog && <>
        <p className="command-picker-hint">{catalog.note}{catalog.authoritative && catalog.source !== "static" ? "（来自 CLI 本身，选中的命令一定会被识别）" : "（当前是候选列表，可点「刷新」从 CLI 读取完整目录）"}</p>
        <CommandCatalogList commands={catalog.commands} selected={selected} onSelect={pickCommand} />
      </>}
      {!catalogLoading && !catalog && <p className="command-picker-hint">没有读到命令目录。可以切到「自定义 shell 命令」，或稍后重试。</p>}
      <div className="command-picker-footer">
        <button type="button" className="secondary" disabled={catalogLoading} onClick={() => void refreshCatalog()}>{catalogLoading ? "读取中" : "刷新目录"}</button>
        {selected && <span className="command-picker-chosen">已选 <code>/{selected}</code></span>}
      </div>
    </div>}
    {(!isCommand || mode === "custom") && <label className="shortcut-editor-field"><span>{isCommand ? "命令内容" : "提示词内容"}</span><textarea required maxLength={12000} value={template} onChange={(event) => setTemplate(event.target.value)} placeholder={isCommand ? "例如：pnpm test" : "描述希望 Claude 在当前项目完成的工作"} /></label>}
    <label className="shortcut-editor-field"><span>名称 <small>最多 64 个字符</small></span><input required maxLength={64} value={name} onChange={(event) => { setAutoNamed(false); setName(event.target.value); }} placeholder={isCommand ? "例如：压缩上下文" : "例如：审查当前改动"} /></label>
    {shortcut && <label className="shortcut-enabled"><input type="checkbox" checked={enabled} onChange={(event) => setEnabled(event.target.checked)} /><span>启用此项</span></label>}
    {isCommand && mode === "cli" && <p className="shortcut-editor-hint">斜杠命令由 {agentDisplayName(agentID)} 自己执行（如 <code>/compact</code>）；其中 <code>/clear</code> 由应用本地清空会话（新建空白会话，旧会话保留在历史中）。清单来自 CLI 自报的命令目录，因此不会出现"填了却不被识别"的命令。</p>}
    {isCommand && mode === "custom" && <p className="shortcut-editor-hint">{supportsCLICommands ? "自由输入适合「让 AI 在本项目执行某条 shell 命令」（如 pnpm test）：它本质是提示词，无法枚举成列表。" : "Codex 在非交互模式下不支持斜杠命令，请填写希望它执行的 shell 命令（如 pnpm test）。"}执行时会遵循当前会话的权限设置。</p>}
    {isCommand && !supportsCLICommands && <p className="shortcut-editor-hint warn">当前会话是 Codex：斜杠命令不可用，已切到自定义 shell 命令。</p>}
    {isCommand && mode === "cli" && selectedCommand && selectedCommand.terminalOnly && <p className="shortcut-editor-hint warn"><code>/{selectedCommand.name}</code> 的交互绑在本地终端，在应用里执行可能只得到一句提示。</p>}
  </div><footer>{shortcut && <button type="button" className="danger-text" disabled={busy} onClick={() => void remove()}>删除</button>}<div className="shortcut-editor-footer-actions"><button type="button" className="secondary" disabled={busy} onClick={close}>取消</button><button className="primary" disabled={busy || (isCommand && mode === "cli" && !selected)}>{busy ? "保存中" : "保存"}</button></div></footer></form></section></div>{pendingConfirm && createPortal(<ConfirmDialog title={pendingConfirm.title} message={pendingConfirm.message} danger={pendingConfirm.danger} onConfirm={pendingConfirm.onConfirm} onCancel={pendingConfirm.onCancel} />, document.body)}</>;
}

// ---- 消息列表（独立 memo 组件，阻断对话页其他状态变化导致的列表重渲染） ----

// 回退入口（2026-10-23 上卡片）：原先单独的 CheckpointAnchor 并进了 MessageCard 的
// 动作行（气泡下方、与复制同区）。两种"没有按钮"的情形由 MessageCard 如实说清：
//   - 这一轮还在跑：节点要等 run 成功结束才生成 → 动作行显示"本轮结束后可回退"；
//   - 这一轮失败/被停止：服务端不会留节点 → 什么都不显示（不是坏了，是没有这个时间点）。
const MessageList = memo(function MessageList({ timeline, agentID, fail, resolving, decide, projectId, onViewTask, executionByRun, onOpenExecution, registerUserMessageElement, checkpointByRun, activeRunId, checkpointBusy, rewindDisabled, discardedTurnsAfter, rewindToCheckpoint }: {
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
  checkpointByRun: Map<string, ConversationCheckpoint>;
  activeRunId: string;
  checkpointBusy: boolean;
  rewindDisabled: boolean;
  discardedTurnsAfter: (checkpoint: ConversationCheckpoint) => number;
  rewindToCheckpoint: (checkpoint: ConversationCheckpoint, mode: "conversation" | "code" | "conversation_and_code", fork?: boolean) => void;
}) {
  const anchoredExecutionRunIDs = useMemo(() => new Set(timeline.flatMap((item) => item.kind === "message" && item.message.role === "user" && item.message.runId && executionByRun.has(item.message.runId) ? [item.message.runId] : [])), [executionByRun, timeline]);
  // 回退回调：默认动作按节点能力钉死（有代码快照 → 恢复全部；没有 → 只恢复对话，
  // 判据在共用模块 rewindPrimaryMode）。useCallback 保持身份稳定 —— MessageCard 是
  // memo 的，回调身份一变整条时间线就跟着重渲染。
  const rewindDefault = useCallback((target: ConversationCheckpoint) => rewindToCheckpoint(target, rewindPrimaryMode(target)), [rewindToCheckpoint]);
  // 时间线上最后一条用户消息：它的动作行常驻显形（回退入口的可发现性兜底）。
  const persistentUserID = useMemo(() => {
    for (let index = timeline.length - 1; index >= 0; index--) {
      const item = timeline[index];
      if (item.kind === "message" && item.message.role === "user") return item.message.id;
    }
    return "";
  }, [timeline]);
  return <>
    {timeline.map((item) => {
      const checkpoint = item.kind === "message" && item.message.role === "user" && item.message.runId ? checkpointByRun.get(item.message.runId) : undefined;
      const isUserMessage = item.kind === "message" && item.message.role === "user";
      return <div key={item.id} className="timeline-item" data-timeline-kind={item.kind} data-user-message-id={isUserMessage ? item.message.id : undefined} ref={isUserMessage ? (element) => { registerUserMessageElement(item.message.id, element); } : undefined}><div className={`timeline-entry ${item.kind === "message" ? "message-entry" : item.kind}`}>{item.kind === "message" ? <MessageCard message={item.message} agentID={agentID} fail={fail} checkpoint={checkpoint} checkpointRunning={isUserMessage && item.message.runId === activeRunId} discarded={checkpoint ? discardedTurnsAfter(checkpoint) : 0} rewindDisabled={rewindDisabled} checkpointBusy={checkpointBusy} rewind={rewindDefault} persistent={isUserMessage && item.message.id === persistentUserID} /> : item.kind === "tool" ? <ToolCard action={item.action} resolving={resolving} decide={decide} /> : item.kind === "system" ? <SystemCard system={item.system} /> : <ErrorCard item={item} projectId={projectId} onViewTask={onViewTask} />}</div>{item.kind === "message" && item.message.role === "user" && item.message.runId && executionByRun.get(item.message.runId) && <AgentExecutionCard execution={executionByRun.get(item.message.runId)!} open={() => onOpenExecution(item.message.runId!)} />}</div>;
    })}
    {executionByRun && Array.from(executionByRun.values()).filter((execution) => !anchoredExecutionRunIDs.has(execution.runId)).map((execution) => <AgentExecutionCard key={execution.runId} execution={execution} open={() => onOpenExecution(execution.runId)} />)}
  </>;
});

// ---- 主组件 ---------------------------------------------------------------

export default function ConversationPage() {
  const { projectId, conversationId: urlConversationId } = useParams<{ projectId: string; conversationId: string }>();
  const navigate = useNavigate();
  const [searchParams] = useSearchParams();
  const location = useLocation();
  const { api: projectApi, setError, getConversationDraft, saveConversationDraft, flushConversationDraft } = useProjectContext();
  const { appPreferences, appPreferencesLoading, appPreferencesError } = useUIPreferences();
  const { project } = useOutletContext<ProjectLayoutOutletContext>();
  const fail = setError;
  // 弹窗参数的开关 —— 全部经下面两个函数，不再直接调 setSearchParams。
  //
  // setSearchParams 按**相对路径**解析，基准是闭包里的 location；而关闭弹窗的调用常常
  // 发生在 await 之后（resetConversationView 由 loadConversation / newConversation 收尾
  // 时调用），闭包还停在旧会话上，算出来的地址会把用户推回上一个会话、并复活它 search 里
  // 的弹窗参数。实测：点「创建会话」后 URL 又跳回旧会话 + ?new=true，新会话弹窗重新打开，
  // 用户以为没建成，反复点 → 攒出一堆空会话（诊断见 lib/dialog-params.ts 的文件头）。
  //
  // 因此这里：用 locationRef（**最新** location）算绝对地址；没有要改的参数就不发导航。
  // 开＝push（Back 能关弹窗，维持原有语义），关＝replace（关弹窗不该往历史里塞条目）。
  const locationRef = useRef({ pathname: location.pathname, search: location.search });
  locationRef.current = { pathname: location.pathname, search: location.search };
  const openDialogParam = useCallback((name: string, value = "true") => {
    const target = dialogParamURL(locationRef.current, name, value);
    if (target) navigate(target);
  }, [navigate]);
  const closeDialogParam = useCallback((name: string) => {
    const target = dialogParamURL(locationRef.current, name, null);
    if (target) navigate(target, { replace: true });
  }, [navigate]);
  // 弹窗控制辅助函数 — 名字保持原样，调用处不必改。
  const closeHistory = () => closeDialogParam("history");
  const closeNewConversation = () => closeDialogParam("new");
  const closeUsage = () => closeDialogParam("usage");
  const closeAgentExecution = () => closeDialogParam("execution");
  const closeAiConfig = () => closeDialogParam("config");
  const openUsage = () => openDialogParam("usage");
  const openNewConversationParam = () => {
    suppressConversationRestoreRef.current = false;
    openDialogParam("new");
  };
  const openAiConfig = () => {
    if (readOnlyConversation) return;
    openDialogParam("config");
  };
  const openExecutionParam = useCallback((runId: string) => openDialogParam("execution", runId), [openDialogParam]);

  // 对话核心状态
  const [conversation, setConversation] = useState<Conversation | null>(null);
  const suppressConversationRestoreRef = useRef(false);
  const [conversationWorkspaces, setConversationWorkspaces] = useState<ConversationWorkspace[]>([]);
  const [conversationCheckpoints, setConversationCheckpoints] = useState<ConversationCheckpoint[]>([]);
  const [checkpointBusy, setCheckpointBusy] = useState(false);
  // 节点按 runId 索引：节点与消息的稳定关联是 run（不是内容），界面据此把回退入口挂到
  // 产生它的那条用户消息下面。Codex 的 nativeTurnId 是原生 turn id，不能拿来对消息，
  // 所以服务端额外存了 run_id（老节点靠迁移回填）。
  const checkpointByRun = useMemo(() => {
    const map = new Map<string, ConversationCheckpoint>();
    conversationCheckpoints.forEach((checkpoint) => { if (checkpoint.runId) map.set(checkpoint.runId, checkpoint); });
    return map;
  }, [conversationCheckpoints]);
  // 节点是 run 成功结束后才在服务端生成的，所以每轮结束时必须重新拉一次：
  // 只在切会话时拉的话，"刚跑完的这一轮"永远不会出现在列表里（新会话甚至一直是灰按钮）。
  const refreshConversationCheckpoints = useCallback(async (conversationID?: string) => {
    const target = conversationID || conversationRef.current?.id || "";
    if (!target) { setConversationCheckpoints([]); return; }
    try {
      const result = await projectApi<{ checkpoints: ConversationCheckpoint[] }>(`/api/conversations/${target}/checkpoints`);
      if (conversationRef.current?.id !== target) return; // 回包跨了会话就丢掉
      setConversationCheckpoints(result.checkpoints || []);
    } catch (cause) {
      fail(cause instanceof Error ? cause.message : "无法加载回退节点");
    }
  }, [fail, projectApi]);
  const [conversationWorkspaceLabels, setConversationWorkspaceLabels] = useState<Record<string, string>>({});
  const [archiveWorkspaceID, setArchiveWorkspaceID] = useState("");
  const [workspaceBusy, setWorkspaceBusy] = useState(false);
  const [conversationHistory, setConversationHistory] = useState<Conversation[]>([]);
  const [historyQuery, setHistoryQuery] = useState("");
  // 只表示"列表还没回来"。缺了它，弹窗会把"还在请求中"和"真的一条都没有"渲染成
  // 同一句话，用户看到的就是"历史对话框出来了，但没有显示任何历史记录"。
  const [historyLoading, setHistoryLoading] = useState(false);
  // 最后一次列表刷新的失败原因。失败同样"没有结论"，不能顺势显示"没有匹配的会话"。
  const [historyError, setHistoryError] = useState("");
  const [conversationHistoryCursor, setConversationHistoryCursor] = useState("");
  const [conversationTabs, setConversationTabs] = useState<ConversationTabsState>({ openConversationIds: [], activeConversationId: null, readPositions: {}, latestPositions: {}, unreadConversationIds: [] });
  const [loadingMoreConversationHistory, setLoadingMoreConversationHistory] = useState(false);
  const [messages, setMessages] = useState<Message[]>([]);
  const [events, setEvents] = useState<Event[]>([]);

  // 回退到这个节点会丢几轮：数"节点之后还有几条顶层用户消息"。列表本身已经被服务端的
  // cutoff 过滤过，所以这里数出来的就是用户眼前还看得到、马上要消失的那几轮。
  const discardedTurnsAfter = useCallback((checkpoint: ConversationCheckpoint) => messages.filter((message) => message.role === "user" && !message.parentToolUseId && message.createdAt > checkpoint.createdAt).length, [messages]);

  // 页面可见性：窗口最小化或切到后台时返回 false，用来给本页的两处轮询加闸。
  // 后台标签页会把 setInterval 限流到 ≥1 分钟，隐藏期间的轮询既不准也白费请求与电量。
  // 实时性由 WebSocket 兜底（它不受定时器限流影响），回到前台时各效果会重跑并立即补一次。
  const documentVisible = useDocumentVisible();

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
    if (!conversation?.id) {
      setConversationCheckpoints([]);
      return;
    }
    void refreshConversationCheckpoints(conversation.id);
  }, [conversation?.id, refreshConversationCheckpoints]);

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
    if (!conversation || workspaceBusy || run || clearing || stopping || handingOff || readOnlyConversation) return;
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
    if (!conversation || workspaceBusy || workspace.active || run || clearing || stopping || handingOff || readOnlyConversation) return;
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

  const archiveWorkspace = (workspace: ConversationWorkspace) => {
    if (!conversation || workspaceBusy || workspace.active || workspace.mode !== "isolated_worktree" || run || clearing || stopping || handingOff || readOnlyConversation) return;
    // 目标会话在打开确认层时就钉死：确认层不是模态阻塞的，期间用户完全可能切走。
    const targetConversationID = conversation.id;
    const token = openPendingConfirm({
      title: "移除工作区",
      message: `移除工作区 #${workspace.generation}？仅当没有未提交变更且专用分支已合并时才能移除。该目录和专用分支将被移除，历史运行记录会保留。`,
      confirmLabel: "移除工作区",
      danger: true,
      onConfirm: () => {
        if (pendingConfirmBusyRef.current) return;
        pendingConfirmBusyRef.current = true;
        setPendingConfirmBusy(true);
        void archiveWorkspaceConfirmed(targetConversationID, workspace).finally(() => closePendingConfirm(token));
      },
      onCancel: () => { if (!pendingConfirmBusyRef.current) closePendingConfirm(token); },
    });
  };

  const archiveWorkspaceConfirmed = async (conversationID: string, workspace: ConversationWorkspace) => {
    // 往返期间用户可能已经切到别的会话：绝不能拿旧工作区去删新会话的目录。
    if (conversationRef.current?.id !== conversationID) return;
    setWorkspaceBusy(true);
    try {
      await projectApi(`/api/conversations/${conversationID}/workspaces/${workspace.id}`, { method: "DELETE" });
      setConversationWorkspaces((items) => items.map((item) => item.id === workspace.id ? { ...item, state: "archived", archivedAt: new Date().toISOString() } : item));
      setArchiveWorkspaceID((current) => current === workspace.id ? "" : current);
    } catch (cause) {
      fail(cause instanceof Error ? cause.message : "无法移除工作区");
    } finally {
      setWorkspaceBusy(false);
    }
  };

  // 回退入口只打开确认层：文案与"默认发哪个 mode"都在 lib/conversation-rewind 里，
  // 与手机端共用一份（回退不可撤销，两端必须说清同一件事：丢几轮、代码会不会被改回去）。
  const rewindToCheckpoint = (checkpoint: ConversationCheckpoint, mode: "conversation" | "code" | "conversation_and_code", fork = false) => {
    if (!conversation || checkpointBusy || run || clearing || stopping || handingOff || readOnlyConversation) return;
    const request = { mode, fork, discarded: discardedTurnsAfter(checkpoint) };
    // 目标会话在打开确认层时就钉死（确认层不阻塞路由），与手机端 confirmRewind 同一条理由。
    const targetConversationID = conversation.id;
    const token = openPendingConfirm({
      title: rewindConfirmTitle(request),
      message: rewindConfirmBody(checkpoint, request),
      confirmLabel: rewindConfirmAction(request, false),
      busyLabel: rewindConfirmAction(request, true),
      danger: true,
      onConfirm: () => {
        if (pendingConfirmBusyRef.current) return;
        pendingConfirmBusyRef.current = true;
        setPendingConfirmBusy(true);
        void rewindToCheckpointConfirmed(targetConversationID, checkpoint, request).finally(() => closePendingConfirm(token));
      },
      onCancel: () => { if (!pendingConfirmBusyRef.current) closePendingConfirm(token); },
    });
  };

  const rewindToCheckpointConfirmed = async (conversationID: string, checkpoint: ConversationCheckpoint, request: { mode: "conversation" | "code" | "conversation_and_code"; fork?: boolean; discarded: number }) => {
    // 确认之后才真正执行：这期间用户可能已经切走，回退绝不能落到另一条会话上。
    if (conversationRef.current?.id !== conversationID) return;
    setCheckpointBusy(true);
    try {
      // 回退可能要把整棵工作区写回去（秒级到分钟级）。用默认的 15 秒超时会让桌面对
      // 一次**其实成功**的回退报"失败"；服务端那侧已经不跟请求上下文走（见 checkpoint.go），
      // 所以这里只要给足预算。
      const result = await apiWithTimeout<{ conversation?: Conversation; checkpoint: ConversationCheckpoint; forked: boolean }>(`/api/conversations/${conversationID}/rewind`, { method: "POST", body: JSON.stringify({ checkpointId: checkpoint.id, mode: request.mode, fork: request.fork }) }, 180_000);
      if (conversationRef.current?.id !== conversationID) return; // 往返期间用户可能已经切走
      if (result.forked && result.conversation) {
        navigate(conversationURL(result.conversation.id));
        return;
      }
      setConversationCheckpoints((items) => items.map((item) => (item.createdAt > checkpoint.createdAt || (item.createdAt === checkpoint.createdAt && item.id > checkpoint.id)) ? { ...item, status: "rewound" } : item.id === checkpoint.id ? { ...item, status: "active" } : item));
      const detail = await apiWithTimeout<{ conversation: Conversation; messages: Message[]; events: Event[]; activeRunId: string | null }>(`/api/conversations/${conversationID}?limit=400`, {}, undefined, 30_000);
      if (conversationRef.current?.id !== conversationID) return;
      resetConversationView(detail.conversation);
      setMessages(detail.messages || []);
      setEvents(detail.events || []);
      setRun(detail.activeRunId || "");
      // 这两样也是"回退之后该变了"的派生视图：↑ 的输入历史（服务端已按节点过滤）与
      // 上下文用量（否则环上还挂着被回退掉那一轮的 token）。
      setHistoryRefresh((version) => version + 1);
      void refreshUsage().catch(() => undefined);
    } catch (cause) {
      fail(cause instanceof Error ? cause.message : "回退失败");
    } finally {
      setCheckpointBusy(false);
    }
  };
  // 时间线的回退回调走 ref 转发：MessageList 是 memo 的，回调身份一变整条时间线就跟着
  // 重渲染，而这个回调依赖了会话/运行状态等一大堆每帧都可能变的值。
  const rewindRef = useRef(rewindToCheckpoint);
  rewindRef.current = rewindToCheckpoint;
  const rewindFromTimeline = useCallback((target: ConversationCheckpoint, mode: "conversation" | "code" | "conversation_and_code") => { void rewindRef.current(target, mode); }, []);
  const [text, setText] = useState("");  const [inputHistory, setInputHistory] = useState<string[]>([]);
  const [historyRefresh, setHistoryRefresh] = useState(0);
  const [run, setRun] = useState("");
  const [sending, setSending] = useState(false);
  const [clearing, setClearing] = useState(false);
  const [handingOff, setHandingOff] = useState(false);
  const [resolving, setResolving] = useState("");
  const [stopping, setStopping] = useState(false);
  const [changingPermission, setChangingPermission] = useState(false);
  const [changingModel, setChangingModel] = useState(false);
  // 弹窗状态 — 通过 URL search params 驱动
  const showHistory = searchParams.get("history") === "true";
	const readOnlyConversation = searchParams.get("readonly") === "true" || conversation?.isOrchestration === true;
  // 回退的禁用条件：跑着、清空中、正在停止、正在交接、只读会话时都不能回退。头部入口与
  // 时间线上的入口共用这一份判据（服务端也会拒，但界面不该让用户点了才知道）。
  const rewindDisabled = readOnlyConversation || checkpointBusy || Boolean(run) || clearing || stopping || handingOff;
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
  // 预约发送时一并暂存的技能引用。所有"清掉预约"的出口（取消、切换会话、真正发出）
  // 都必须连它一起清，否则下一轮预约会带上上一轮失效的引用。
  const pendingSendSkillRefsRef = useRef<Skill[]>([]);
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
  // 常用命令目录（GET /api/projects/{id}/commands，见 docs/37）。命令标签据此标注
  // "当前 CLI 未提供"，命令选择器据此列出可选命令。
  const [commandCatalog, setCommandCatalog] = useState<ProjectCommands | null>(null);
  const [commandCatalogLoading, setCommandCatalogLoading] = useState(false);
  // 技能按来源分组后，各来源组的折叠状态。插件组（系统/官方自带，含大量 marketplace 样板）默认折叠。
  const [skillGroupsCollapsed, setSkillGroupsCollapsed] = useState<Partial<Record<Skill["source"], boolean>>>({ plugin: true });
  // 已引用、尚未发送的技能。
  // 点技能**不再**把「技能名 + 描述」整段铺进输入框：技能描述动辄上百字，会把草稿顶满三行；
  // 而 setComposerText 是整体覆盖，用户写到一半的内容会被无声吃掉（这是比"难看"更严重的问题）。
  // 现在记成一颗可删除的胶囊，发送那一刻才由 composeSkillMessage 展开成完整引用指令 ——
  // 发出去的文本与旧实现逐字一致，变的只是"用户在输入框里看到什么"。
  const [skillRefs, setSkillRefs] = useState<Skill[]>([]);
  // 侧栏模块（常用提示词+常用命令合并为一个整体折叠、技能、任务队列）的折叠状态，按项目持久化。
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
  // 用户是否已经用"加载更早记录"往回翻过页。翻过之后游标就指向比第 1 页更早的位置，
  // 而 reload / 轮询拿回来的永远是最新一页的回包 —— 拿它的 nextCursor 覆盖分页状态会把
  // 游标倒回最新一页的边界：已经翻过的页要重翻一遍，按钮也会在明明已经到底之后重新亮起。
  // messages / events 该合并照旧合并（按 id 去重），只有分页状态受这个标记保护。
  const pagedBackRef = useRef(false);
  const [pendingConfirm, setPendingConfirm] = useState<{ title: string; message: React.ReactNode; confirmLabel?: string; busyLabel?: string; danger?: boolean; className?: string; icon?: React.ReactNode; onConfirm: () => void; onCancel: () => void } | null>(null);
  const [pendingConfirmBusy, setPendingConfirmBusy] = useState(false);
  // pendingConfirm 是全页共用的一格。每个打开者领一个递增令牌，清理时只清"还是自己的"那一格 ——
  // 否则 A 会话上那个慢操作（移除工作区/回退）的 finally 会把期间在 B 会话上打开的新确认框一起清掉。
  const pendingConfirmTokenRef = useRef(0);
  const pendingConfirmRef = useRef(pendingConfirm);
  pendingConfirmRef.current = pendingConfirm;

  const openPendingConfirm = (config: { title: string; message: React.ReactNode; confirmLabel?: string; busyLabel?: string; danger?: boolean; className?: string; icon?: React.ReactNode; onConfirm: () => void; onCancel: () => void }) => {
    const token = ++pendingConfirmTokenRef.current;
    pendingConfirmBusyRef.current = false;
    setPendingConfirmBusy(false);
    setPendingConfirm(config);
    return token;
  };

  const closePendingConfirm = (token: number) => {
    if (pendingConfirmTokenRef.current !== token) return;
    pendingConfirmTokenRef.current += 1; // 同一格再次清理时不重复生效
    pendingConfirmBusyRef.current = false;
    setPendingConfirmBusy(false);
    setPendingConfirm(null);
  };

  // 确认层挂在"当前这条会话"上：切走时收掉，否则旧会话的回退/移除确认会浮在新会话上。
  // 必须走一遍 onCancel：强制停止那条靠它复位 stopping，直接清空会让页面级标志漏成 true。
  // 真正执行前还会再校验会话 id（见 archiveWorkspaceConfirmed / rewindToCheckpointConfirmed）。
  useEffect(() => {
    const orphaned = pendingConfirmRef.current;
    pendingConfirmTokenRef.current += 1; // 作废在飞行的 finally
    pendingConfirmRef.current = null;
    pendingConfirmBusyRef.current = false;
    setPendingConfirmBusy(false);
    setPendingConfirm(null);
    if (orphaned) orphaned.onCancel();
  }, [conversation?.id]);
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
  const composerEditRef = useRef<HTMLDivElement | null>(null);
  const finishedRunIds = useRef(new Set<string>());
  const usageRequestVersion = useRef(0);
  const usageConversationID = useRef<string | null>(null);
  const usageRefreshInFlight = useRef(false);
  const conversationRef = useRef<Conversation | null>(null);
  conversationRef.current = conversation;
  textRef.current = text;
  const runRef = useRef("");
  runRef.current = run;
  const conversationTransitionRef = useRef(false);
  const conversationRouteVersion = useRef(0);
  const conversationHistoryRequestVersion = useRef(0);
  const unavailableConversationIdsRef = useRef(new Set<string>());
  const conversationPageActiveRef = useRef(true);
  const conversationDeletionPendingRef = useRef(false);
  conversationDeletionPendingRef.current = Boolean(deletingConversation) || deleteAllConversationsBusy;
  useEffect(() => {
    conversationPageActiveRef.current = true;
    return () => { conversationPageActiveRef.current = false; };
  }, []);
  // 正在飞行的列表刷新，按"同一种刷新"去重（见 requestConversationHistory）。
  const conversationHistoryInflight = useRef(new Map<string, Promise<ConversationHistoryResult>>());
  // 每次渲染都取最新的 search 参数：恢复会话时要用"此刻"的弹窗参数，而不是挂载那一刻的。
  const searchParamsRef = useRef(searchParams);
  searchParamsRef.current = searchParams;
  // 切换项目时本组件**不会**重挂载（同一个路由元素），所以要有一个"当前项目"的真值来源：
  // 上一个项目那份列表结果飞回来时必须能被认出来，不能落到新项目的页面上。
  const projectIDRef = useRef(projectId);
  projectIDRef.current = projectId;
  const urlConversationIdRef = useRef(urlConversationId);
  urlConversationIdRef.current = urlConversationId;
  const conversationTabsRef = useRef<ConversationTabsState>(conversationTabs);
  conversationTabsRef.current = conversationTabs;
  const stopRunRef = useRef<() => Promise<void>>(async () => {});
  // The stop endpoint is asynchronous. Keep a short request lock so a double
  // click does not create concurrent stop requests, while still allowing a
  // retry after the first request has returned `stopping`.
  const stopRequestInFlightRef = useRef(false);
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

  // text → 编辑器 DOM 的单向同步（contenteditable 改造的核心接缝）。
  // 所有"外部写回输入框"的路径（草稿恢复、历史翻阅、发送失败回填、清空、斜杠纠错建议、
  // 引用消息…）都汇在 setText/setComposerText 上，这一个 effect 就是它们触达 DOM 的唯一入口：
  // 序列化结果与 text 一致（= 用户正在打字的常规路径）时绝不动 DOM，否则光标会跳；
  // 不一致（= 外部整体覆盖）才整体重写为纯文本。卡片只经由粘贴产生，外部回填是纯文本是可接受的降级。
  // 「会话 id 变了」是无条件重写的例外：两个会话的草稿可能恰好是同一个字符串，
  // 只比 text 会让旧会话 DOM 里的卡片残留到新会话（显示/删除/浮层全部错位）。
  // 比对用 textRef.current 而不是闭包里的 text：passive effect 异步调度，快速打字时
  // effect(t1) 可能flush 在 DOM 已到 t2 之后 —— 比 state 会误判成外部覆盖而重写、光标跳末尾；
  // textRef 在每个输入事件里同步更新（全部 setText 路径都维护它），键 2 已把 ref 推到 t2，
  // 迟到的 effect(t1) 就会被正确跳过。
  const composerSyncedConversationRef = useRef<string | undefined>(undefined);
  useEffect(() => {
    const el = composerEditRef.current;
    if (!el) return;
    if (composerSyncedConversationRef.current === conversation?.id && serializeComposer(el) === textRef.current) return;
    composerSyncedConversationRef.current = conversation?.id;
    hidePastePop();
    el.textContent = textRef.current;
    // 光标在编辑器里且内容被外部覆盖时，把光标送到末尾，避免停留在越界位置。
    if (document.activeElement === el) {
      const sel = window.getSelection();
      const range = document.createRange();
      range.selectNodeContents(el);
      range.collapse(false);
      sel?.removeAllRanges();
      sel?.addRange(range);
    }
  }, [text, conversation?.id]);

  // 卡片上的 ✕ 移除走的是 DOM 事件（contenteditable=false 区域不产生 React input 事件）：
  // 监听后把草稿重算一遍，否则删了卡片发送的还是旧草稿。
  // 经 ref 转发最新的处理器再挂一次监听：直接把 handleTextChange 闭包进 [] effect
  // 会把首帧的 setComposerText（带旧 projectId）钉死，跨项目切换后草稿写串。
  const pasteChangedRef = useRef<() => void>(() => {});
  pasteChangedRef.current = () => {
    const el = composerEditRef.current;
    if (el) handleTextChange(serializeComposer(el));
  };
  useEffect(() => {
    const el = composerEditRef.current;
    if (!el) return;
    const onChanged = () => pasteChangedRef.current();
    el.addEventListener("composer-paste-changed", onChanged);
    // 浮层是挂在本组件之外的 body 单例：卸载必须收掉，否则跨页面残留。
    return () => {
      el.removeEventListener("composer-paste-changed", onChanged);
      hidePastePop();
    };
  }, []);

  // 技能引用属于当前会话。resetConversationView 负责主路径，这里也覆盖关闭最后一个
  // Tab 等直接清空会话的入口，避免引用在会话之间残留。
  useEffect(() => { setSkillRefs([]); }, [conversation?.id]);

  // Route changes must invalidate a pending clear before promise callbacks can
  // write the previous conversation back into the page.
  useLayoutEffect(() => {
    conversationRouteVersion.current++;
  }, [projectId, urlConversationId]);

  useEffect(() => {
    unavailableConversationIdsRef.current.clear();
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

  const selectConversationTab = useCallback(async (conversationID: string, followShared = false) => {
    const selectionIsAvailable = () => conversationPageActiveRef.current && projectIDRef.current === projectId &&
      !unavailableConversationIdsRef.current.has(conversationID) && (!followShared || !conversationDeletionPendingRef.current);
    if (!projectId || !conversationID || !selectionIsAvailable()) return;
    const routeVersion = conversationRouteVersion.current;
    const routePath = window.location.pathname;
    suppressConversationRestoreRef.current = false;
    let makeRoomForSharedConversation = false;
    const tabs = conversationTabsRef.current;
    if (!tabs.openConversationIds.includes(conversationID) && tabs.openConversationIds.length >= MAX_OPEN_CONVERSATION_TABS) {
      if (!followShared) {
        fail(`每个项目最多同时打开 ${MAX_OPEN_CONVERSATION_TABS} 个会话，请先关闭一个 Tab。`);
        return;
      }
      makeRoomForSharedConversation = true;
    }
    try {
      await projectApi<Conversation>(`/api/conversations/${conversationID}/activate`, { method: "POST" });
    } catch (cause) {
      if (!selectionIsAvailable() || conversationRouteVersion.current !== routeVersion || window.location.pathname !== routePath) return;
      fail(cause instanceof Error ? cause.message : "无法切换当前会话");
      return;
    }
    if (!selectionIsAvailable() || conversationRouteVersion.current !== routeVersion || window.location.pathname !== routePath) return;
    if (makeRoomForSharedConversation) {
      const currentTabs = conversationTabsRef.current;
      if (!currentTabs.openConversationIds.includes(conversationID) && currentTabs.openConversationIds.length >= MAX_OPEN_CONVERSATION_TABS) {
        // 远端已将它设为共享选择。请求成功后再替换当前标签，避免网络失败
        // 时本地无故丢失一个标签。
        const displacedID = currentTabs.activeConversationId || currentTabs.openConversationIds[0];
        const next = closeConversationTab(currentTabs, displacedID);
        conversationTabsRef.current = next;
        writeConversationTabs(projectId, next);
        setConversationTabs(next);
      }
    }
    if (!rememberConversationTab(conversationID)) {
      fail(`每个项目最多同时打开 ${MAX_OPEN_CONVERSATION_TABS} 个会话，请先关闭一个 Tab。`);
      return;
    }
    if (projectId && conversationID !== urlConversationId) navigate(`/projects/${projectId}/conversations/${conversationID}`);
  }, [fail, navigate, projectApi, projectId, rememberConversationTab, urlConversationId]);

  // 会话目录每 8 秒与服务端对账一次。isCurrent 代表跨端共享选择，
  // 因此手机切换后桌面应跟随它，而不能继续停在自己的旧标签页。
  useEffect(() => {
    if (!projectId) return;
    const sharedCurrent = conversationHistory.find((item) => item.isCurrent);
    // Closing a tab is a local UI decision. The server keeps `isCurrent` for
    // cross-device selection, so it must not resurrect a tab dismissed here.
    if (!sharedCurrent || sharedCurrent.id === urlConversationId || readClosedConversationIds(projectId).includes(sharedCurrent.id)) return;
    void selectConversationTab(sharedCurrent.id, true);
  }, [conversationHistory, projectId, selectConversationTab, urlConversationId]);

  const closeConversationTabFromUI = useCallback((conversationID: string) => {
    if (!projectId) return;
    markConversationTabClosed(projectId, conversationID);
    const next = closeConversationTab(conversationTabsRef.current, conversationID);
    conversationTabsRef.current = next;
    writeConversationTabs(projectId, next);
    setConversationTabs(next);
    if (next.openConversationIds.length === 0) {
      suppressConversationRestoreRef.current = true;
      setConversation(null);
      setMessages([]);
      setEvents([]);
      setRun("");
      setUsage(null);
      setText("");
      textRef.current = "";
    }
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

  // 内部"补全/纠正 URL"的导航统一走这里：把用户此刻打开的弹窗参数原样带过去。
  // 这类导航只是把 URL 修成"当前会话"，不是"重置页面状态"；顺手关掉用户刚点开的历史弹窗，
  // 看上去就像"我点的那一下被吞了"——删除当前会话、恢复会话都踩过这个坑。
  const conversationURL = useCallback((conversationID?: string) => {
    const carried = new URLSearchParams();
    DIALOG_SEARCH_PARAMS.forEach((name) => { const value = searchParamsRef.current.get(name); if (value !== null) carried.set(name, value); });
    const query = carried.toString();
    const base = conversationID ? `/projects/${projectId}/conversations/${conversationID}` : `/projects/${projectId}/conversations`;
    return `${base}${query ? `?${query}` : ""}`;
  }, [projectId]);

  const removeUnavailableConversationTabs = useCallback((conversationIDs: string[]) => {
    if (!conversationPageActiveRef.current || !projectId || projectId !== projectIDRef.current || conversationIDs.length === 0) return;
    const unavailable = new Set(conversationIDs);
    // Also reject late list/activation responses that still refer to these IDs.
    conversationIDs.forEach((id) => unavailableConversationIdsRef.current.add(id));
    setConversationHistory((current) => current.filter((item) => !unavailable.has(item.id)));
    const previous = conversationTabsRef.current;
    let next = previous;
    conversationIDs.forEach((conversationID) => { next = closeConversationTab(next, conversationID); });
    if (next !== previous) {
      conversationTabsRef.current = next;
      writeConversationTabs(projectId, next);
      setConversationTabs(next);
    }
    if (conversationRef.current && unavailable.has(conversationRef.current.id)) {
      conversationRef.current = null;
      setConversation(null);
      setMessages([]);
      setEvents([]);
      setRun("");
      setUsage(null);
      setText("");
      textRef.current = "";
      setSkillRefs([]);
    }
    // Deletion/activity requests may finish after the user has navigated away.
    const currentRouteID = urlConversationIdRef.current;
    if (currentRouteID && unavailable.has(currentRouteID)) {
      // BrowserRouter updates the address before React commits the new route.
      if (window.location.pathname === `/projects/${projectId}/conversations/${currentRouteID}`) {
        navigate(conversationURL(next.activeConversationId || undefined), { replace: true });
      }
    }
  }, [conversationURL, navigate, projectId]);

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
    // 清除 URL 中的 addFile 参数（同样走 dialogParamURL：这个 effect 会在 URL 变化后重跑，
    // 直接 setSearchParams 会照闭包里的旧 location 算地址）
    closeDialogParam("addFile");
    // 将文件路径追加到输入框
    const prefix = textRef.current.trim() ? `${textRef.current}\n` : "";
    setComposerText(`${prefix}@${filePath} `, conversation.id);
  }, [projectId, conversation?.id, searchParams, closeDialogParam, setComposerText]);

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
    // 技能引用属于上一个会话：草稿跟着会话走，引用不该跟着漂到新会话去。
    setSkillRefs([]);
    setMessages([]); setEvents([]); setRun(""); setUsage(null); historyIndex.current = null; draftBeforeHistory.current = ""; finishedRunIds.current.clear(); pendingUserDrafts.current.clear(); assistantOutputRuns.current.clear(); retractedMessageRuns.current.clear(); setShowPermissionMenu(false); setShowFullControlConfirmation(false); closeAgentExecution(); setHasMoreHistory(false); setHasMoreMessageHistory(false); setHistoryCursor(""); pagedBackRef.current = false; setLoadingOlderHistory(false); setCurrentUserMessageIndex(-1); setPendingPreviousUserMessageID(null); setHasNewContent(false); userNearBottom.current = true; setConversation(next);
  };

  // 刷新函数。
  // `background` 供后台轮询使用：它只"认领"当前这一版结果，**从不自增版本号**。
  // 否则后台那次会把前台刚发出的刷新作废掉（打开弹窗 / 切回对话 Tab 时就是这样），
  // 让调用方拿到一个空列表，再被误判成"项目里没有会话"。
  //
  // 同一种刷新只发一次：打开弹窗与"切回对话 Tab 后的会话恢复"会同时要同一份列表，
  // 各发一次的话后发的那次必然把先发的作废掉，先发的调用方就只剩一个空列表。
  const requestConversationHistory = useCallback((query: string, cursor = "", append = false, background = false): Promise<ConversationHistoryResult> => {
    if (!projectId) return Promise.resolve({ items: [], stale: false });
    if (!background) setHistoryLoading(true);
    // 键里必须带 projectId：本组件在"直接从一个项目跳到另一个项目"时不会重挂载，
    // 少了这一段，新项目那次刷新会**复用上一个项目正在飞行的请求**并把它的列表拿过来。
    const key = `${projectId}|${background ? "bg" : "fg"}|${query}|${cursor}|${append ? "1" : "0"}`;
    const inflight = conversationHistoryInflight.current;
    const running = inflight.get(key);
    if (running) return running;
    const requestVersion = background ? conversationHistoryRequestVersion.current : ++conversationHistoryRequestVersion.current;
    const requestProjectID = projectId;
    const params = new URLSearchParams({ limit: "100" });
    if (query.trim()) params.set("q", query.trim());
    if (cursor) params.set("cursor", cursor);
    const pending = (async (): Promise<ConversationHistoryResult> => {
      try {
        const page = await projectApi<ConversationHistoryPage>(`/api/projects/${projectId}/conversations?${params.toString()}`);
        // 项目已经切走：这份结果属于上一个项目，对当前页面**没有结论**（更不是"没有会话"）。
        if (requestProjectID !== projectIDRef.current) return { items: [], stale: true };
        // 被更晚的一次刷新超越：结果丢弃，并且**必须**告诉调用方"这次没有结论"。
        // 曾经这里返回空数组，调用方据此认定"项目里没有会话"，于是 POST 新建一个会话、
        // 再把历史列表覆盖成那一条 —— 项目里凭空多出来的会话就是这么来的。
        if (requestVersion !== conversationHistoryRequestVersion.current) return { items: [], stale: true };
        const items = page.items.filter((item) => !unavailableConversationIdsRef.current.has(item.id));
        setHistoryError("");
        setConversationHistory((current) => {
          if (!append) return items;
          const known = new Set(current.map((item) => item.id));
          return [...current, ...items.filter((item) => !known.has(item.id))];
        });
        setConversationHistoryCursor(page.nextCursor);
        return { items, stale: false };
      } catch (cause) {
        // 这次刷新失败了：同样**没有结论**，把原因如实交给弹窗，别让它显示成"没有记录"。
        if (!background && requestVersion === conversationHistoryRequestVersion.current) {
          setHistoryError(cause instanceof Error ? cause.message : "无法加载会话列表");
        }
        throw cause;
      } finally {
        inflight.delete(key);
        // 只有"最后一次"请求才有资格结束加载态：更早的那次若也清，弹窗会先闪一下
        // 「没有匹配的会话」再被真实结果替换。后台轮询不参与加载态。
        if (!background && requestVersion === conversationHistoryRequestVersion.current) setHistoryLoading(false);
      }
    })();
    inflight.set(key, pending);
    return pending;
  }, [projectId, projectApi]);

  // 手机通过中继激活会话后，服务端会广播 projects 失效事件。立即重读目录，
  // 让桌面跟随共享选择，不必等 8 秒兜底轮询。
  useLiveStateEventsFor("projects", projectId, useCallback(() => {
    // 搜索结果不能被后台目录刷新覆盖，但它也不能阻断共享会话跟随；当前会话
    // 按 isCurrent 排在目录首位，单独读取后直接切换即可。
    if (projectId) {
      void projectApi<ConversationHistoryPage>(`/api/projects/${projectId}/conversations?limit=1`)
        .then((page) => {
          const sharedCurrent = page.items.find((item) => item.isCurrent);
          if (sharedCurrent && sharedCurrent.id !== urlConversationId && !readClosedConversationIds(projectId || "").includes(sharedCurrent.id)) {
            void selectConversationTab(sharedCurrent.id, true);
          }
        })
        .catch(() => undefined);
    }
    if (!historyQuery.trim()) void requestConversationHistory("", "", false, true).catch(() => undefined);
  }, [historyQuery, projectApi, projectId, requestConversationHistory, selectConversationTab, urlConversationId]));

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
  // 列表刷新走 background=true：轮询只"顺手刷新"，不作废前台的刷新（见 requestConversationHistory）。
  useEffect(() => {
    if (!projectId || conversationTabs.openConversationIds.length === 0) return;
    // 窗口不可见时既不拉也不起定时器——这里每轮固定两个请求，后台空跑最费。可见性变化会
    // 重跑本效果，所以回到前台时下面那句会立刻补一次，不必等到下个周期。
    if (!documentVisible) return;
    const refreshBackgroundTabs = () => {
      void syncConversationActivity().catch(() => undefined);
      if (!historyQuery.trim()) void requestConversationHistory("", "", false, true).catch(() => undefined);
    };
    refreshBackgroundTabs();
    const timer = window.setInterval(refreshBackgroundTabs, 8_000);
    return () => window.clearInterval(timer);
  }, [conversationTabs.openConversationIds.length, documentVisible, historyQuery, projectId, requestConversationHistory, syncConversationActivity]);

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
    openDialogParam("history");
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

  // WebSocket 事件是主要刷新路径；运行中再保留一个低频兜底，避免事件
  // 丢失或 CLI 长时间不发事件时，底部上下文状态停在旧快照。
  useEffect(() => {
    if (!documentVisible || !run || !conversation?.id) return undefined;
    let cancelled = false;
    const tick = async () => {
      if (cancelled || usageRefreshInFlight.current) return;
      usageRefreshInFlight.current = true;
      try {
        await refreshUsage();
      } catch {
        // WebSocket 刷新路径负责报告错误；兜底轮询失败时静默等待下一次。
      } finally {
        usageRefreshInFlight.current = false;
      }
    };
    void tick();
    const timer = window.setInterval(() => { void tick(); }, 1000);
    return () => {
      cancelled = true;
      window.clearInterval(timer);
    };
  }, [conversation?.id, documentVisible, refreshUsage, run]);

  // 加载或创建对话
  useEffect(() => {
    if (!projectId || conversationTransitionRef.current) return;
    let cancelled = false;
    const abort = new AbortController();
    const isCurrentRoute = () => !cancelled && !conversationTransitionRef.current &&
      (!urlConversationId || !unavailableConversationIdsRef.current.has(urlConversationId));
    async function loadConversation() {
      try {
        if (!urlConversationId && suppressConversationRestoreRef.current) {
          setConversation(null);
          setMessages([]);
          setEvents([]);
          setRun("");
          setUsage(null);
          setText("");
          textRef.current = "";
          return;
        }
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
				if (fallbackID && fallbackID !== detail.conversation.id) navigate(conversationURL(fallbackID), { replace: true });
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

        // 否则使用最新对话或创建新对话。
        // 被并发刷新超越的那次结果**没有结论**，不等于"项目里没有会话"：这里只重试，
        // 绝不拿它判空 —— 曾因此 POST 出一个凭空多出来的会话，并把历史列表覆盖成那一条。
        let result = await refreshConversationHistory();
        if (cancelled || conversationTransitionRef.current) return;
        for (let attempt = 0; result.stale && attempt < 3 && !cancelled && !conversationTransitionRef.current; attempt++) {
          await new Promise((resolve) => setTimeout(resolve, 60));
          result = await refreshConversationHistory();
          if (cancelled || conversationTransitionRef.current) return;
        }
        // 仍被超越就先放走：等下一次挂载或用户操作来收尾，也好过凭空造一个会话。
        if (result.stale) return;
        const list = result.items;
        // Returning to the project without a conversation ID must not reopen a
        // tab the user deliberately closed. Restore the saved active tab first;
        // otherwise use the newest history item that is not dismissed.
        const restoredID = conversationTabsRef.current.activeConversationId;
        const restored = restoredID ? list.find((item) => item.id === restoredID) : undefined;
        const closed = new Set(readClosedConversationIds(projectId || ""));
        // If every historical conversation was deliberately closed, keep the
        // workspace empty. Creating one here would immediately resurrect the
        // last tab the user just closed. A brand-new conversation is created
        // only when the project has no history at all or when the user clicks
        // the explicit new-conversation action.
        const next = restored || list.find((item) => !closed.has(item.id)) || (list.length === 0
          ? await projectApi<Conversation>(`/api/projects/${projectId}/conversations`, { method: "POST" })
          : null);
        if (!cancelled && !conversationTransitionRef.current) {
          if (!next) {
            setConversation(null);
            setMessages([]);
            setEvents([]);
            setRun("");
            setUsage(null);
            setText("");
            textRef.current = "";
            return;
          }
          if (conversationRef.current?.id === next.id) setConversation(next);
          else resetConversationView(next);
          if (list.length === 0) setConversationHistory([next]);
          // 更新 URL 到当前对话。恢复会话只是"把 URL 补全成当前会话"，不是"重置页面状态"：
          // 走 conversationURL 把弹窗参数原样带过去，否则刚打开的历史弹窗会被这次 replace
          // 顺手关掉（用户点的那一下就此被吞掉）。
          if (next.id && !urlConversationId) {
            navigate(conversationURL(next.id), { replace: true });
          }
        }
      } catch (cause) {
        if (!cancelled) {
          if (urlConversationId && typeof cause === "object" && cause !== null && (cause as { status?: unknown }).status === 404) {
            removeUnavailableConversationTabs([urlConversationId]);
            return;
          }
          fail(cause instanceof Error ? cause.message : "无法打开会话");
          if (urlConversationId) navigate(conversationURL(), { replace: true });
        }
      }
    }
    void loadConversation();
    return () => { cancelled = true; abort.abort(); };
  }, [projectId, urlConversationId, readOnlyConversation, clearing, fail, refreshConversationHistory, projectApi, navigate, conversationURL, rememberConversationTab, removeUnavailableConversationTabs, reopenArchivedConversation]);

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

  // 加载常用命令目录（docs/37）：命令标签的"未提供"提示与命令选择器都用它。
  // probe=0：只是打开项目不该拉起 CLI 进程——目录优先来自真实运行的采样，冷启动时
  // 由用户打开选择器或点「刷新」再探测。加载失败静默为空（选择器会显示候选并说明）。
  useEffect(() => {
    if (!projectId) return;
    const agentId = conversation?.agentId || "claude-code";
    let cancelled = false;
    setCommandCatalog(null);
    setCommandCatalogLoading(true);
    void (async () => {
      try {
        const view = await projectApi<ProjectCommands>(`/api/projects/${projectId}/commands?agentId=${encodeURIComponent(agentId)}&probe=0`);
        if (!cancelled) setCommandCatalog(view);
      } catch { if (!cancelled) setCommandCatalog(null); }
      finally { if (!cancelled) setCommandCatalogLoading(false); }
    })();
    return () => { cancelled = true; };
  }, [projectId, conversation?.agentId, projectApi]);

  // refreshCommandCatalog 主动重取（含探针）：用户在命令选择器里点「刷新目录」时调用。
  const refreshCommandCatalog = useCallback(async () => {
    if (!projectId) return;
    const agentId = conversationRef.current?.agentId || "claude-code";
    setCommandCatalogLoading(true);
    try {
      const view = await projectApi<ProjectCommands>(`/api/projects/${projectId}/commands?agentId=${encodeURIComponent(agentId)}&refresh=1`);
      setCommandCatalog(view);
    } catch (cause) {
      fail(cause instanceof Error ? cause.message : "无法读取命令目录");
    } finally {
      setCommandCatalogLoading(false);
    }
  }, [projectId, projectApi, fail]);

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
        const data = await projectApi<{ messages: Message[]; events: Event[]; activeRunId: string | null; hasMore: boolean; hasMoreMessages: boolean; nextCursor: string; messagesCutoff?: string; messagesVisibleAfter?: string }>(`/api/conversations/${conversation.id}?limit=400`);
        if (!isCurrentConversation()) return false;
        data.messages.forEach((message) => { if (message.role === "assistant") recordAssistantOutput(message.runId || "", message.content); });
        // 过滤掉被撤回的用户消息 — 停止时如果助手还没有输出，后端可能仍会返回用户消息
        const filteredMessages = data.messages.filter((message) => !(message.role === "user" && message.runId && retractedMessageRuns.current.has(message.runId)));
        // mergeReloadMessages 会**保留**本地已有的消息（流式输出还没落库时不能丢），所以服务端
        // 按节点时间过滤过的那份挡不住本地缓存里的回合 —— 合并完还要按同一个 cutoff 再滤一遍，
        // 否则在别的设备上发起的回退，本机屏幕上看不出来（内容还在）。
        setMessages((current) => dropAfterCutoff(mergeReloadMessages(filteredMessages, current), data.messagesCutoff, data.messagesVisibleAfter));
        setEvents((current) => dropAfterCutoff(mergeConversationItems(data.events, current), data.messagesCutoff, data.messagesVisibleAfter));
        // 这份回包是请求发出时的快照：若请求期间 run 已推进到更晚的值（例如预约发送
        // 自动发出的新回合，或另一处已启动的新 run），不能拿旧快照把它覆盖/清空，
        // 否则 isConversationIdle 会误判为空闲、运行指示也会丢失。
        setRun((current) => {
          if (data.activeRunId) return data.activeRunId;
          if (current && current !== runAtRequest) return current;
          return "";
        });
        // 最新一页的回包只负责补内容，不碰分页状态：用户往回翻过页时，这份 nextCursor
        // 会把游标倒回最新一页的边界（见 pagedBackRef）。
        if (!pagedBackRef.current) {
          setHasMoreHistory(data.hasMore); setHasMoreMessageHistory(data.hasMoreMessages); setHistoryCursor(data.nextCursor || "");
        }
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
          // 节点就是这一轮跑完后生成的，终态一到就重新拉列表：否则"刚跑完的这一轮"永远不会
          // 出现在回退入口里（新会话甚至因为列表为空而一直是灰按钮）。run.started 不必拉。
          if (event.type !== "run.started") void refreshConversationCheckpoints(conversation.id);
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
  // 上一次的可见性，用来识别「由隐藏转为可见」那一跳：本效果在 setup 时不主动拉取
  // （首次加载交给 WebSocket 那侧），所以回到前台必须显式补一次，否则运行指示会停在
  // 后台的旧状态上，一直等到下个周期。
  const prevRunPollVisible = useRef(documentVisible);
  useEffect(() => {
    // 先无条件同步可见性：这样 becameVisible 只会在真正的「隐藏 → 可见」那一跳为真。
    // 若把它放到下面的 !run 守卫之后，run 为空的那些可见性变化不会更新 ref，等下次
    // 启动运行的瞬间就会误判成"刚从后台回来"，白打一次请求。
    const becameVisible = documentVisible && !prevRunPollVisible.current;
    prevRunPollVisible.current = documentVisible;
    // A remote/mobile dispatch can produce assistant events while the local
    // WebSocket is reconnecting. Keep reconciliation alive even when both the
    // local run and the cached conversation status have gone stale; otherwise
    // a missed event leaves only the initial dispatch message visible.
    if (!conversation?.id) return undefined;
    // 窗口不可见时停掉回退轮询。实时性由 WebSocket 保证（它不受后台定时器限流影响），
    // 这条只是连接不可靠时的兜底，隐藏期间没有兜底的价值。
    if (!documentVisible) return undefined;
    let cancelled = false;
    let pollInFlight = false;
    const poll = async () => {
      if (pollInFlight) return;
      pollInFlight = true;
      try {
        const full = await projectApi<{ messages: Message[]; events: Event[]; activeRunId: string | null; hasMore: boolean; hasMoreMessages: boolean; nextCursor: string }>(`/api/conversations/${conversation.id}?limit=400`);
        if (cancelled) return;
        full.messages.forEach((message) => { if (message.role === "assistant") recordAssistantOutput(message.runId || "", message.content); });
        // 过滤掉被撤回的用户消息
        const pollMessages = full.messages.filter((message) => !(message.role === "user" && message.runId && retractedMessageRuns.current.has(message.runId)));
        setMessages((current) => mergeReloadMessages(pollMessages, current));
        setEvents((current) => mergeConversationItems(full.events, current));
        setRun(full.activeRunId || "");
        // 同 reload：轮询拿到的也是最新一页，不能覆盖已经往回翻过的分页游标。
        if (!pagedBackRef.current) {
          setHasMoreHistory(full.hasMore); setHasMoreMessageHistory(full.hasMoreMessages); setHistoryCursor(full.nextCursor || "");
        }
        setStopping(false);
      } catch {
        // silently ignore
      } finally {
        pollInFlight = false;
      }
    };
    if (becameVisible) void poll();
    const interval = window.setInterval(() => { void poll(); }, 15_000);
    return () => {
      cancelled = true;
      window.clearInterval(interval);
    };
  }, [documentVisible, run, conversation?.id, conversation?.status, projectApi, recordAssistantOutput]);

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
      // 技能引用属于原来那个会话，跟着预约一起作废（草稿里只回用户正文）。
      pendingSendSkillRefsRef.current = [];
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
    const frame = requestAnimationFrame(() => composerRef.current?.querySelector<HTMLElement>(".composer-edit")?.focus());
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
      // 从这一刻起游标不再是"最新一页的边界"，最新页回包不许再覆盖它。
      pagedBackRef.current = true;
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

  // 技能的引用文案。与手机端 MobileRemotePage 的 skillPrompt 必须**逐字一致**（见 docs/22）：
  // 同一个技能在两端要让 Agent 收到同一句指令。
  const mergeSkillPrompt = (skill: Skill) =>
    skill.description && skill.description !== skill.name
      ? `请使用技能 <${skill.name}>：${skill.description}`
      : `请使用技能 <${skill.name}>`;

  // 「用户写的正文」与「已引用的技能」拼成真正发出去的文本：引用在前、正文在后，中间空一行。
  // 这里是"输入框里显示什么"与"实际发什么"之间唯一的分界线 —— 输入框里只有一颗胶囊，
  // 而 CLI 收到的仍是那句完整的自然语言引用（headless 通道不认 `/skill-name`，见 docs/22）。
  // 只引用技能、没写正文也允许发送，与旧行为一致（点技能后直接回车就能发出这条引用）。
  const composeSkillMessage = (text: string, refs: Skill[]) => {
    if (refs.length === 0) return text.trim();
    const body = refs.map(mergeSkillPrompt).join("\n");
    return text.trim() ? `${body}\n\n${text.trim()}` : body;
  };

  const addSkillRef = (skill: Skill) => {
    setSkillRefs((current) => current.some((item) => item.name === skill.name && item.source === skill.source) ? current : [...current, skill]);
    requestAnimationFrame(() => composerRef.current?.querySelector<HTMLElement>(".composer-edit")?.focus());
  };

  const removeSkillRef = (skill: Skill) => {
    setSkillRefs((current) => current.filter((item) => !(item.name === skill.name && item.source === skill.source)));
  };

  // 业务方法
  const sendContent = async (rawContent: string, clearDraft = true, options: { restoreOnFailure?: boolean; notifyFailure?: boolean; clientRequestId?: string; skillRefs?: Skill[] } = {}): Promise<boolean> => {
    const { restoreOnFailure = true, notifyFailure = true } = options;
    const refs = options.skillRefs ?? [];
    if (!conversation || readOnlyConversation || sending || clearing || stopping || handingOff || shortcutBusy || changingModel) return false;
    // draft = 用户在输入框里写的正文；content = 加上技能引用后真正上线的文本。
    // 所有"写回输入框"的路径只能用 draft：否则一次发送失败就会把整段技能描述重新灌回输入框，
    // 又回到"输入框被技能描述铺满"的老问题。
    const draft = rawContent.trim();
    if (!draft && refs.length === 0) return false;
    if (draft === "/resume") {
      if (clearDraft) {
        setComposerText("", conversation.id);
        // 这条命令不会发出去（只是打开历史弹窗），输入框被清空 —— 引用要跟着清，
        // 否则用户关掉弹窗后会看到"空输入框 + 一颗不知道从哪来的技能胶囊"。
        setSkillRefs([]);
      }
      openConversationHistory();
      return false;
    }
    if (draft === "/clear") {
      // /clear 是 CLI 的"清屏"命令：headless（-p --output-format stream-json）模式下
      // CLI 只发一个 conversation_reset 事件、内部重置自己的会话，不会在应用界面清空历史，
      // 导致与 Claude Code / Codex 里的展示效果不一致。这里拦截并走应用自己的
      // "清空上下文"流程（新建空白会话、旧会话保留在历史中），让 /clear 真正清空对话历史。
      clearConversationContext();
      return false;
    }
    // Codex 的 `codex exec` 不解析斜杠命令（斜杠只存在于它的 TUI 层，实测 /status 会被
    // 当成普通提示词，模型转头去找一个同名文件）。这是确定无解的组合，直接拦下并说明原因，
    // 不让它变成一次莫名其妙的运行。目录里"没有但可能仍可用"的命令不在这里拦（见下面的
    // 输入提示）——那种情况只提示、不阻断。
    const slashName = slashCommandName(draft);
    if (slashName && !agentSupportsSlashCommands(conversation.agentId)) {
      fail(`${agentDisplayName(conversation.agentId)} 不支持斜杠命令（/${slashName}）。请改用自定义 shell 命令，或新建一个支持斜杠命令的会话。`);
      return false;
    }
    const conversationID = conversation.id;
    const clientRequestId = options.clientRequestId ?? crypto.randomUUID();
    const routeVersion = conversationRouteVersion.current;
    const content = composeSkillMessage(draft, refs);
    setSending(true);
    if (clearDraft) {
      setComposerText("", conversationID);
      // 引用与草稿一起乐观清空；发送失败时在 catch 里一并恢复（见下）。
      setSkillRefs([]);
    }
    setShowPermissionMenu(false); historyIndex.current = null; draftBeforeHistory.current = "";
    try {
      const data = await projectApi<{ message: Message; runId: string }>(`/api/conversations/${conversationID}/messages`, { method: "POST", body: JSON.stringify({ content, clientRequestId }) });
      if (conversationRef.current?.id !== conversationID) return false;
      // 撤回 / 上箭头重发要恢复的是"用户自己写的东西"，所以这里存 draft 而不是 data.message.content：
      // 后者带着整段技能引用，写回输入框就等于把胶囊重新变成一大段文本（正是本次要修的问题）。
      if (!assistantOutputRuns.current.has(data.runId) && !finishedRunIds.current.has(data.runId)) pendingUserDrafts.current.set(data.runId, draft);
      setMessages((old) => [...old, data.message]); if (draft) appendInputHistory(draft); setHistoryRefresh((version) => version + 1); setRun(finishedRunIds.current.has(data.runId) ? "" : data.runId);
      followAfterDispatch();
      void refreshUsage().catch((cause) => fail(cause instanceof Error ? cause.message : "无法刷新用量统计"));
      void refreshConversationHistory().catch((cause) => fail(cause instanceof Error ? cause.message : "无法刷新会话历史"));
      return true;
    } catch (cause) {
      if (conversationRef.current?.id !== conversationID || conversationRouteVersion.current !== routeVersion) return false;
      if (notifyFailure) fail(cause instanceof Error ? cause.message : "无法发送消息");
      if (clearDraft && restoreOnFailure) {
        // 正文与引用各自回到原来的位置：文本回输入框，技能回胶囊。
        setComposerText(draft, conversationID);
        setSkillRefs(refs);
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
    if (changingModel) return;
    setShowSendMenu(false);
    void sendContent(text, true, { skillRefs });
  };

  // 预约发送：将输入内容暂存，等当前对话（含所有子代理）彻底空闲后再真正发送。
  // 技能引用一并暂存到 ref，等真正发出时才由 sendContent 展开成完整引用指令。
  const scheduleSend = () => {
    const content = text.trim();
    // 只有技能引用、没有正文时同样可以预约（与"点技能后直接发送"一致）。
    if ((!content && skillRefs.length === 0) || !conversation) return;
    if (sending || clearing || stopping || handingOff || shortcutBusy || changingModel) return;
    setShowSendMenu(false);
    pendingSendRef.current = content;
    pendingSendSkillRefsRef.current = skillRefs;
    pendingSendConversationRef.current = conversation.id;
    pendingSendRequestIDRef.current = crypto.randomUUID();
    // 新预约视为“尚未开始等待空闲”，让 effect 在空闲成立时重新挂起延迟发送
    // （否则对话当前已空闲时，wasScheduledIdleRef 为 true，空闲转变检测不会触发）。
    wasScheduledIdleRef.current = false;
    setPendingSendContent(content);
    setComposerText("", conversation.id);
    setSkillRefs([]);
  };

  // 仅清空预约状态（不写回输入框）。用于 stopRun / clearCurrentConversation——
  // 这两条流程自己有草稿恢复（restorePendingUserDraft / resetConversationView），
  // 若这里预填输入框会污染 textRef，破坏它们对"输入框为空才恢复"的判断或被 reset 覆盖。
  const clearScheduledSend = () => {
    const pending = pendingSendRef.current;
    pendingSendRef.current = null;
    pendingSendSkillRefsRef.current = [];
    pendingSendConversationRef.current = null;
    pendingSendRequestIDRef.current = null;
    if (pending != null) setPendingSendContent(null);
  };

  // 取消预约（用户主动点取消按钮）：清空预约状态并把暂存内容写回输入框，方便继续编辑或改为立即发送。
  // 技能引用同样回到胶囊，而不是被展开成一段长文本。
  const cancelScheduledSend = () => {
    const conversationID = conversation?.id;
    const pending = pendingSendRef.current;
    const refs = pendingSendSkillRefsRef.current;
    if (!conversationID || pending == null) return;
    clearScheduledSend();
    const existing = textRef.current.trim();
    setComposerText(existing ? `${existing}\n${pending}` : pending, conversationID);
    setSkillRefs(refs);
    requestAnimationFrame(() => composerRef.current?.querySelector<HTMLElement>(".composer-edit")?.focus());
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
    if (changingModel) return false;
    const conversationID = pendingSendConversationRef.current;
    // `?? ""`：预约里可能只有技能引用、正文是空串，甚至 pendingSendRef 已被取消清成 null。
    // 归一成字符串之后，下面"预约是否还是同一条"的比较仍然成立（null !== "" 就是"已被取消"）。
    const draft = pendingSendRef.current ?? "";
    const clientRequestId = pendingSendRequestIDRef.current;
    const refs = pendingSendSkillRefsRef.current;
    // draft 为空但带着技能引用，同样是一条可发的消息（点技能就能发）。
    if (!conversationID || (!draft && refs.length === 0) || !clientRequestId) return false;
    if (readOnlyConversation) return false; // 会话变为只读（编排/readonly）时不能发送，保留预约不丢失
    if (sending || clearing || stopping || handingOff || shortcutBusy) return false;
    if (conversationRef.current?.id !== conversationID) return false; // 切换会话后不越界发送
    if (!isConversationIdle()) return false; // 延迟期间又有了新的运行，继续等待
    // 自动发送可能撞上瞬时的后端拒绝（工作区被并发的 insight 扫描 / Git 操作占用、
    // 会话重建、CLI 忙碌等）。带退避重试几次，避免一失败就把内容弹回输入框；
    // 前几次静默重试（不弹错误、不写回输入框），最后一次才提示并回退。
    const backoff = [1500, 3000, 6000, 10000];
    for (let attempt = 0; attempt <= backoff.length; attempt++) {
      if (pendingSendRef.current !== draft || pendingSendConversationRef.current !== conversationID || pendingSendRequestIDRef.current !== clientRequestId) return false; // 预约已被取消/切换会话
      if (conversationRef.current?.id !== conversationID) return false;
      if (!isConversationIdleRef.current()) return false; // 重试期间又有了新的运行，交回空闲检测重新挂起
      const last = attempt === backoff.length;
      const ok = await sendContentRef.current(draft, true, { restoreOnFailure: last, notifyFailure: last, clientRequestId, skillRefs: refs });
      if (ok) {
        pendingSendRef.current = null;
        pendingSendSkillRefsRef.current = [];
        pendingSendConversationRef.current = null;
        pendingSendRequestIDRef.current = null;
        setPendingSendContent((current) => current === draft ? null : current);
        return true;
      }
      if (last) {
        // 最后一次也失败：sendContent 若走了 catch（restoreOnFailure）已把内容写回输入框；
        // 否则（被 sendContent 守卫拦截，如 sending 卡住）在这里手动写回，避免预约内容丢失。
        if (!textRef.current.trim()) { setComposerText(draft, conversationID); setSkillRefs(refs); }
        pendingSendRef.current = null;
        pendingSendSkillRefsRef.current = [];
        pendingSendConversationRef.current = null;
        pendingSendRequestIDRef.current = null;
        setPendingSendContent((current) => current === draft ? null : current);
        return true;
      }
      await new Promise((resolve) => setTimeout(resolve, backoff[attempt]));
    }
    return true;
  }, [changingModel, clearing, handingOff, isConversationIdle, readOnlyConversation, sending, shortcutBusy, stopping]);

  const fireScheduledSendRef = useRef(fireScheduledSend);
  fireScheduledSendRef.current = fireScheduledSend;
  const isConversationIdleRef = useRef(isConversationIdle);
  isConversationIdleRef.current = isConversationIdle;

  // 监听预约内容：会话“从运行中变为彻底空闲”时延迟一拍再真正发送。
  // run.* 事件会立刻把 run 置空并触发 reload，后端也还在对上一个 run 做收尾；
  // 立即 POST 可能撞上瞬时 4xx，被 sendContent 的失败回退写回输入框。
  // 延迟后再重查空闲仍成立才发送，显著降低这类竞态。
  useEffect(() => {
    if (changingModel) {
      if (scheduledSendTimerRef.current != null) {
        window.clearTimeout(scheduledSendTimerRef.current);
        scheduledSendTimerRef.current = null;
      }
      wasScheduledIdleRef.current = false;
      return;
    }
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
  }, [changingModel, isConversationIdle, pendingSendContent, clearing, readOnlyConversation, sending, shortcutBusy, stopping]);

  useEffect(() => () => {
    if (scheduledSendTimerRef.current != null) window.clearTimeout(scheduledSendTimerRef.current);
  }, []);

  const runShortcut = async (shortcut: Shortcut, variables: Record<string, string> = {}, variablesReady = false) => {
    if (!conversation || readOnlyConversation || sending || clearing || stopping || handingOff || shortcutBusy || changingModel) return;
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
    if (conversationTransitionRef.current || sending || clearing || stopping || handingOff || shortcutBusy || workspaceBusy || !projectId) { closeNewConversation(); return; }
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
    if (conversationTransitionRef.current || !conversation || readOnlyConversation || sending || clearing || stopping || handingOff || shortcutBusy) return;
    if (run) {
      // 运行中时先停止，再关闭当前会话并新建一个会话。
      const token = openPendingConfirm({
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
          void stopAndClearRef.current().finally(() => closePendingConfirm(token));
        },
        onCancel: () => { if (!pendingConfirmBusyRef.current) closePendingConfirm(token); },
      });
      return;
    }
    const token = openPendingConfirm({
      title: "关闭并新建会话",
      message: "将关闭当前会话并打开一个新的空白会话。当前内容仍可在历史会话中恢复。",
      confirmLabel: "关闭并新建",
      className: "conversation-clear-dialog",
      icon: <ConversationClearIcon />,
      onConfirm: () => {
        if (pendingConfirmBusyRef.current) return;
        pendingConfirmBusyRef.current = true;
        setPendingConfirmBusy(true);
        void clearCurrentConversation().finally(() => closePendingConfirm(token));
      },
      onCancel: () => { if (!pendingConfirmBusyRef.current) closePendingConfirm(token); },
    });
  };

  const handoffConversationContext = async () => {
    if (!conversation || conversation.agentId !== "codex" || readOnlyConversation || sending || clearing || stopping || shortcutBusy || handingOff) return;
    const conversationID = conversation.id;
    const routeVersion = conversationRouteVersion.current;
    const routeConversationID = urlConversationId;
    const handoffStillOwnsView = () =>
      conversationRef.current?.id === conversationID &&
      conversationRouteVersion.current === routeVersion &&
      (!routeConversationID || routeConversationID === conversationID);
    setHandingOff(true);
    try {
      const handoff = await projectApi<{ id: string; status: string; targetConversationId?: string }>(`/api/conversations/${conversation.id}/handoff`, { method: "POST" });
      for (let attempt = 0; attempt < 180; attempt++) {
        await new Promise((resolve) => window.setTimeout(resolve, 1000));
        if (!handoffStillOwnsView()) return;
        const current = await projectApi<{ status: string; targetConversationId?: string; errorMessage?: string }>(`/api/conversation-handoffs/${handoff.id}`);
        if (current.status === "failed") throw new Error(current.errorMessage || "上下文交接失败");
        if (current.status !== "completed" || !current.targetConversationId) continue;
        const detail = await projectApi<{ conversation: Conversation }>(`/api/conversations/${current.targetConversationId}?limit=1`);
        if (!handoffStillOwnsView()) return;
        replaceConversationTab(conversationID, detail.conversation.id);
        resetConversationView(detail.conversation);
        if (projectId) navigate(`/projects/${projectId}/conversations/${detail.conversation.id}`, { replace: true });
        void refreshConversationHistory();
        return;
      }
      throw new Error("上下文交接超时，请稍后从历史会话重试");
    } catch (cause) {
      if (handoffStillOwnsView()) fail(cause instanceof Error ? cause.message : "上下文交接失败");
    } finally { setHandingOff(false); }
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
          const result = await refreshConversationHistory();
          // 被超越的那次刷新没有结论（items 为空 ≠ 项目里没有会话）：这里只当"没找到"
          // 处理，把下面的原始请求错误如实报出去。
          const current = result.items.find((item) => item.isCurrent);
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
    // The workspace may be intentionally empty after its last tab was closed;
    // opening a history item must still be allowed in that state.
    if (item.id === conversation?.id || item.isOrchestration) { closeHistory(); return; }
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
	      // 顺序不能反：closeHistory 按 locationRef 里的**最新 location** 拼绝对地址，而
	      // locationRef 要等路由变化引发下一次渲染才更新 —— 同一次点击里先切 Tab 再关弹窗，
	      // 它拿到的仍是旧会话的 pathname，会把刚 push 出去的新会话 URL replace 回旧会话。
	      // 现象就是"在历史弹窗里点另一个会话，怎么点都切不过去"（探针抓到的两次 pushState：
	      // c2 之后立刻又推 c1）。先关弹窗只动 search，再切会话定路径。
	      closeHistory();
	      selectConversationTab(item.id);
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
		const token = openPendingConfirm({
			title: "删除会话",
			message: `将永久删除会话「${item.title || "新会话"}」及其全部消息，无法恢复。`,
			confirmLabel: "删除",
			danger: true,
			className: "conversation-delete-dialog",
			onConfirm: () => {
				if (pendingConfirmBusyRef.current) return;
				pendingConfirmBusyRef.current = true;
				setPendingConfirmBusy(true);
				void deleteHistoryConversationConfirmed(item.id).finally(() => closePendingConfirm(token));
			},
			onCancel: () => { if (!pendingConfirmBusyRef.current) closePendingConfirm(token); },
		});
	};

	const deleteHistoryConversationConfirmed = async (conversationID: string) => {
		setDeletingConversation(conversationID);
		try {
			// 单条删除允许较长等待：级联删除会同步清掉消息/事件/运行记录，历史很长
			// 的会话在慢速磁盘上可能超过默认 15s 请求超时。
			await apiWithTimeout(`/api/conversations/${conversationID}`, { method: "DELETE" }, 0, 120_000);
		} catch (cause) {
			if (conversationPageActiveRef.current && projectIDRef.current === projectId) fail(cause instanceof Error ? cause.message : "无法删除会话");
			return;
		} finally {
			setDeletingConversation("");
		}
		// 关闭该会话对应的 Tab；若它正是当前 URL 会话，会切到相邻 Tab 或新建会话。
		if (!conversationPageActiveRef.current || projectIDRef.current !== projectId) return;
		removeUnavailableConversationTabs([conversationID]);
		void refreshConversationHistory().catch((cause) => fail(cause instanceof Error ? cause.message : "无法刷新会话历史"));
	};

	const deleteAllHistoryConversations = () => {
		if (!projectId || deletingConversation || deleteAllConversationsBusy) return;
		const token = openPendingConfirm({
			title: "清除全部历史对话",
			message: "将永久删除该项目下的全部会话历史（自动编排会话只读保留），无法恢复。",
			confirmLabel: "清除全部",
			danger: true,
			className: "conversation-delete-all-dialog",
			onConfirm: () => {
				if (pendingConfirmBusyRef.current) return;
				pendingConfirmBusyRef.current = true;
				setPendingConfirmBusy(true);
				void deleteAllHistoryConversationsConfirmed().finally(() => closePendingConfirm(token));
			},
			onCancel: () => { if (!pendingConfirmBusyRef.current) closePendingConfirm(token); },
		});
	};

	const deleteAllHistoryConversationsConfirmed = async () => {
		setDeleteAllConversationsBusy(true);
		let deletedIDs: string[] = [];
		try {
			const result = await apiWithTimeout<{ deleted: number; skipped: number; deletedIds?: string[] }>(`/api/projects/${projectId}/conversations`, { method: "DELETE" }, 0, 120_000);
			deletedIDs = result.deletedIds || [];
		} catch (cause) {
			if (conversationPageActiveRef.current && projectIDRef.current === projectId) fail(cause instanceof Error ? cause.message : "无法清除全部历史对话");
			return;
		} finally {
			setDeleteAllConversationsBusy(false);
		}
		if (!conversationPageActiveRef.current || projectIDRef.current !== projectId) return;
		if (deletedIDs.length > 0) {
			removeUnavailableConversationTabs(deletedIDs);
		}
		void refreshConversationHistory().catch((cause) => fail(cause instanceof Error ? cause.message : "无法刷新会话历史"));
	};

  // 切换会话级模型：写 conversations.model_override。服务端会退役按旧模型启动的长驻会话，
  // 下一条消息带新 --model --resume 重启，因此这里与权限切换同样只在空闲时放行。
  const selectConversationModel = async (model: string): Promise<boolean> => {
    if (!conversation || readOnlyConversation || (conversation.modelOverride || "") === model || run || clearing || stopping || handingOff) return false;
    const conversationID = conversation.id;
    setChangingModel(true);
    try {
      const updated = await projectApi<Conversation>(`/api/conversations/${conversationID}/model`, { method: "POST", body: JSON.stringify({ model }) });
      if (conversationRef.current?.id !== conversationID) return true;
      setConversation(updated);
      return true;
    } catch (cause) {
      fail(cause instanceof Error ? cause.message : "无法切换模型");
      return false;
    } finally {
      setChangingModel(false);
    }
  };

  const changePermissionMode = async (permissionMode: PermissionMode) => {
    if (!conversation || readOnlyConversation || conversation.permissionMode === permissionMode || run || clearing || stopping || handingOff) return;
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
    if (!conversation || run || clearing || stopping || handingOff) return;
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
    if (!conversation || readOnlyConversation || !run || handingOff || stopRequestInFlightRef.current) return;
    stopRequestInFlightRef.current = true;
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
        const token = openPendingConfirm({
          title: "强制停止",
          message: "该对话还有其他排队中或执行中的请求，强制停止将一并取消它们。是否继续？",
          danger: true,
          onConfirm: () => {
            closePendingConfirm(token);
            void stopRunInternal(true, runID).then((result) => { retractUserMessage(); restorePendingUserDraft(conversationID, routeVersion, runID, draftToRestore); if (result !== "stopping") setStopping(false); }).catch((cause) => {
              fail(cause instanceof Error ? cause.message : "无法停止任务");
              setStopping(false);
            });
          },
          onCancel: () => { closePendingConfirm(token); setStopping(false); },
        });
        return;
      }
      fail(cause instanceof Error ? cause.message : "无法停止任务");
      setStopping(false);
      return;
    }
    finally {
      stopRequestInFlightRef.current = false;
    }
  };
  stopRunRef.current = stopRun;

  const handleTextChange = (value: string) => {
    historyIndex.current = null;
    draftBeforeHistory.current = "";
    setComposerText(value);
  };

  // contenteditable 输入路径：每次按键/输入把 DOM 序列化回草稿。
  // 序列化规则唯一来源是 lib/composer-paste.ts —— 卡片在这里还原成完整原文，
  // 所以草稿、发送、历史记录拿到的都是完整文本。
  const handleComposerInput = (event: React.FormEvent<HTMLDivElement>) => {
    const el = event.currentTarget;
    // 卡片失联（键盘 Backspace/拖拽删除，而非 ✕ 路径）时浮层会残留：锚卡片已不在 DOM。
    hidePastePopIfDetached();
    // IME 组合期间不做 DOM 归一化（会打断选字）。
    // 归一化只针对「清空后的残留」：Chromium 可能留下孤立 <br> 或只含 <br> 的空 <div>。
    // 文本节点一律算内容 —— 空编辑器里敲一个空格也是内容，不许吞（textarea 允许以空格开头）。
    if (!(event.nativeEvent as InputEvent).isComposing
      && el.childNodes.length > 0
      && Array.from(el.childNodes).every(isComposerBlankResidue)) {
      el.innerHTML = "";
    }
    handleTextChange(serializeComposer(el));
  };

  // 粘贴分流：超过阈值收成内联卡片（发送时还原为全文），以内走纯文本插入。
  // 一律 preventDefault：富文本/图片粘贴不许直接进 DOM —— 标签看不见但 serialize 口径外，
  // 「界面上有、发出去没有」正是本项目最忌的显示与发送分家。
  const handleComposerPaste = (event: React.ClipboardEvent<HTMLDivElement>) => {
    event.preventDefault();
    const pasted = event.clipboardData.getData("text/plain");
    if (!pasted) return;
    const el = event.currentTarget;
    if (shouldFoldPaste(pasted)) {
      el.focus();
      insertPasteCardAtSelection(el, pasted);
      handleTextChange(serializeComposer(el));
      return;
    }
    // execCommand 走浏览器原生插入（保留撤销栈、触发 input 事件）；失败时手工补文本节点兜底。
    if (!document.execCommand("insertText", false, pasted)) {
      el.appendChild(document.createTextNode(pasted));
      handleTextChange(serializeComposer(el));
    }
  };

  const handleComposerKeyDown = (event: React.KeyboardEvent<HTMLDivElement>) => {
    // keyCode 229 是 IME 组合处理的兜底信号：个别 WebKit 版本确认选词的 Enter
    // 先派发 compositionend 再派发 keydown，isComposing 已为 false，会误发送。
    if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing && event.nativeEvent.keyCode !== 229) {
      event.preventDefault();
      composerRef.current?.requestSubmit();
      return;
    }
    navigateInputHistory(event);
  };

  const navigateInputHistory = (event: React.KeyboardEvent<HTMLDivElement>) => {
    if (event.nativeEvent.isComposing || event.shiftKey || (event.key !== "ArrowUp" && event.key !== "ArrowDown")) return;
    const editor = event.currentTarget;
    const selection = window.getSelection();
    if (!selection || selection.rangeCount === 0) return;
    const browsing = historyIndex.current !== null;
    // 判据与 textarea 旧实现一一对应：非浏览态要求光标 collapsed；
    // 上箭头还要求光标在第一行（光标之前的文本里没有换行），否则它只是普通的行间移动。
    if (!browsing && !selection.isCollapsed) return;
    if (event.key === "ArrowUp") {
      if (inputHistory.length === 0 || (!browsing && caretTextBefore(editor).includes("\n"))) return;
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

  // 阶段计时：本阶段从最新一条「进行中」阶段事件（压缩/重试/后台任务）起算，
  // 阶段一变自然归零，run 结束即停表 —— 口径对齐 CLI 的"当前动作计时"。
  // 判据收口在 lib/run-indicator.ts；没有阶段事件时退回 runLabel 且不显示计时
  //（时间戳读不到就是读不到，不许拿 0 起点编出一个大数）。
  // 计时的每秒 tick 在 RunIndicatorView 内部，不放在页面根上。
  const stageItem = useMemo(() => (run ? latestActiveStage(events, run) : null), [events, run]);
  const runIndicatorVariant = run ? pickRunIndicatorVariant(run) : null;
  // 文字位变体：与图形位同一套 runId 哈希机制（种子加 ":text" 后缀，互不绑死），
  // 同一 run 内图形与文字的花样都稳定（判据在 lib/run-indicator.ts）。
  const runTextVariant = run ? pickRunTextVariant(run) : null;
  const stageCreatedAtMs = stageItem ? Date.parse(stageItem.createdAt) : Number.NaN;
  const stageStartMs = Number.isFinite(stageCreatedAtMs) ? stageCreatedAtMs : null;
  const activeAgentName = conversation ? agentDisplayName(conversation.agentId) : "";
  const permissionLabel = conversation ? permissionCopy[conversation.permissionMode]?.title ?? conversation.permissionMode : "";
  const runLabel = activeAgentName ? `${activeAgentName} 正在处理任务` : "正在处理任务";
  // 阶段词：有进行中的阶段事件用它的标题（桌面时间线同一份文案），否则退回「XX 正在处理任务」。
  const stageLabel = stageItem ? stageItem.title : runLabel;
  const currentUsage = usage?.currentRun ?? usage?.latestRun;
  // 会话指定的模型优先展示：它是"接下来会用的模型"，而用量里的 model 是"上一次实际用的
  // 模型"（第三方网关还可能改写模型名，见 docs/36 §2.7）。实际值仍可在使用状态弹窗查看。
  const displayedModel = conversation?.modelOverride || usage?.context.model || currentUsage?.model || activeAgentName;
  const promptShortcuts = shortcuts.filter((shortcut) => shortcut.kind === "prompt" || shortcut.kind === "snippet");
  const commandShortcuts = shortcuts.filter((shortcut) => shortcut.kind === "command_request");
  const userMessageNavigationIndex = userMessages.length === 0 ? -1 : Math.max(0, Math.min(currentUserMessageIndex, userMessages.length - 1));
  const inputHistoryPosition = historyIndex.current === null ? null : historyIndex.current + 1;
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
    requestAnimationFrame(() => composerRef.current?.querySelector<HTMLElement>(".composer-edit")?.focus());
  };

  // 技能描述过长时截断，供悬浮卡片展示，避免过长的描述撑爆卡片。
  // 描述与技能名相同(或为空)时返回空串，悬浮卡片只显示标题不重复。
  const truncateSkillDescription = (description: string | undefined, name: string, max = 120) => {
    const text = (description || "").trim();
    if (!text || text === name) return "";
    if (text.length <= max) return text;
    return `${text.slice(0, max).trimEnd()}…`;
  };

  // 点技能 = 往输入框上方挂一颗可删除的引用胶囊，**不再碰用户已经写好的正文**
  // （旧实现是 setComposerText 整段覆盖草稿）。
  // 引用文案的展开推迟到发送那一刻，见 composeSkillMessage。
  const useSkill = (skill: Skill) => {
    if (!conversation || readOnlyConversation || sending || clearing || stopping || Boolean(shortcutBusy) || skillsLoading) return;
    addSkillRef(skill);
  };

  // commandAvailability 把命令标签分成三态：可用 / 当前 CLI 未提供 / 当前 CLI 不支持。
  // 只有目录权威（来自 CLI 本身）时才敢说"未提供"——静态候选下没找到不代表失效（docs/37 §3.6）。
  const commandAvailability = useCallback((shortcut: Shortcut): "ok" | "stale" | "unsupported" => {
    const commandName = slashCommandName(shortcut.template);
    if (!commandName) return "ok"; // 自定义 shell 命令：不是斜杠命令，与 CLI 命令目录无关
    if (conversation && !agentSupportsSlashCommands(conversation.agentId)) return "unsupported";
    if (commandCatalog?.authoritative && !commandCatalog.commands.some((command) => command.name === commandName)) return "stale";
    return "ok";
  }, [commandCatalog, conversation?.agentId]);

  const renderShortcutCell = (shortcut: Shortcut | undefined, kind: "prompt" | "command_request", placeholder = false, beforeRun?: () => void) => {
    if (!shortcut && placeholder) return <div className="quick-tag-slot" aria-hidden="true" />;
    if (!shortcut) return <button type="button" className={`quick-tag-empty${kind === "command_request" ? " command-tag" : ""}`} disabled={readOnlyConversation} onClick={() => { beforeRun?.(); setShortcutEditor({ kind }); }}>{kind === "command_request" ? "添加命令" : "添加提示词"}</button>;
    const availability = kind === "command_request" ? commandAvailability(shortcut) : "ok";
    const blocked = availability === "unsupported";
    // 目录外的命令不硬拦（`/review` 这类被 CLI 隐藏但确实能跑）：标注 + 在 title 里说明，
    // 让用户自己决定。硬拦会误伤这些命令（实测 /review 可执行，只是不在目录里）。
    const warn = availability === "stale";
    const title = blocked
      ? `${shortcut.template}\n\nCodex 不支持斜杠命令，这条命令在 Codex 会话里不可用`
      : warn
        ? `${shortcut.template}\n\n当前 CLI 的命令目录里没有这条命令，执行后可能只会得到一句 Unknown command`
        : shortcut.enabled ? shortcut.template : `${shortcut.template}\n\n${shortcut.name}已停用`;
    return <div className={`quick-tag ${shortcut.enabled ? "" : "disabled"}${kind === "command_request" ? " command-tag" : ""}${blocked ? " blocked" : ""}${warn ? " stale" : ""}`} key={shortcut.id}><button type="button" disabled={readOnlyConversation || !shortcut.enabled || !conversation || blocked || sending || clearing || stopping || handingOff || Boolean(shortcutBusy)} onClick={() => { beforeRun?.(); void runShortcut(shortcut); }} title={title}><span className="quick-tag-text">{shortcutBusy === shortcut.id ? "发送中" : shortcut.name}</span>{(blocked || warn) && <span className="quick-tag-warn" aria-hidden="true">!</span>}</button><button type="button" className="quick-tag-edit" title={`编辑 ${shortcut.name}`} aria-label={`编辑 ${shortcut.name}`} disabled={readOnlyConversation || clearing || handingOff || Boolean(shortcutBusy)} onClick={() => { setShowMobileShortcuts(false); setShortcutEditor({ kind: shortcut.kind, shortcut }); }}><ShortcutMoreIcon /></button></div>;
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

  // 输入框里的斜杠命令提示（docs/37 §3.8）。手敲的 `/xxx` 与"添加命令"是同一个根因，
  // 共用同一份目录即可：目录里没有时**只提示、不阻断**——目录有 10 分钟 TTL，刚建的
  // 自定义命令可能还没进缓存，硬拦会误伤。
  const composerSlashHint = useMemo(() => {
    const commandName = slashCommandName(text);
    if (!commandName || (conversation && !agentSupportsSlashCommands(conversation.agentId))) return null;
    if (!commandCatalog?.authoritative) return null;
    if (commandCatalog.commands.some((command) => command.name === commandName)) return null;
    return { commandName, suggestion: closestCommandName(commandName, commandCatalog.commands.map((command) => command.name)) };
  }, [text, commandCatalog, conversation?.agentId]);


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
    // 桌面端（collapsible）渲染为「技能」色块的块头（.quick-block-head），与提示词/命令块同构；
    // 移动端输入框菜单（非折叠）沿用原有 .quick-tag-group 标题行，样式随 .skill-tag 全局改版同步。
    const heading = collapsible
      ? <div className="quick-block-head">
          <button type="button" className="quick-block-title" aria-expanded={!collapsed} title={collapsed ? "展开技能模块" : "折叠技能模块"} onClick={toggle}>
            <span className="quick-block-title-text">技能 Skill</span>
            <QuickTagChevron />
          </button>
          <b className="quick-block-count">{skillsLoading ? "…" : skills.length}</b>
        </div>
      : <div className="quick-tag-heading">
          <span className="quick-tag-heading-label"><SkillTagIcon /><span className="quick-tag-heading-title">技能 (Skill)</span></span>
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
                  <button type="button" className="skill-tag" disabled={readOnlyConversation || !conversation || sending || clearing || stopping || handingOff || Boolean(shortcutBusy)} data-tooltip-title={skill.name} data-tooltip-desc={truncateSkillDescription(skill.description, skill.name)} onClick={() => useSkill(skill)}>
                    <span className="skill-tag-text">{skill.name}</span>
                  </button>
                </li>
              ))}</ul>}
            </div>
          );
        })}</div>;
    return collapsible
      ? <div className={`quick-block skill-block${collapsed ? " collapsed" : ""}`}>
          {heading}
          {!collapsed && body}
        </div>
      : <div className="quick-tag-group skill-tags">{heading}{body}</div>;
  };

  return <>
    {createPortal(<div className={`head-actions-menu${showMobileActions ? " mobile-open" : ""}`}>
        <button className="head-actions-mobile-toggle" type="button" aria-expanded={showMobileActions} onClick={() => setShowMobileActions((open) => !open)}>操作</button>
        <div className="head-actions">
          {/* 方案 A 图标化降噪：次要操作桌面端只露图标（.icon-action 34px 方钮 + title 提示），
              文字留在 .action-label 里，≤820px 的「操作」菜单再显示。 */}
          <button className="conversation-head-action secondary icon-action" type="button" disabled={sending} onClick={openConversationHistory} title="历史" aria-label="会话历史"><HistoryIcon /><span className="action-label">历史</span></button>
          {/* 会话回退（checkpoint）2026-10-23 上卡片：头部「节点」列表已撤除，回退入口
              在每条用户消息卡片的动作行上（与复制同区）。节点仍按 runId 关联消息、
              仍随回合刷新（refreshConversationCheckpoints），只是不再有第二个入口。
              代价（已与用户确认）：「分支 / 恢复代码 / 恢复对话」三种变体动作随列表
              一并撤下 —— 需要时要按变体 C 加第三颗虚线图标钮或恢复精简列表。 */}
          {conversationWorkspaces.length > 0 && <label className="conversation-workspace-picker"><WorkspaceFolderIcon /><span>工作区</span><select aria-label="会话工作区" value={conversationWorkspaces.find((item) => item.active)?.id || ""} disabled={workspaceBusy || Boolean(run) || clearing || stopping || handingOff || readOnlyConversation} onChange={(event) => { const selected = conversationWorkspaces.find((item) => item.id === event.target.value); if (selected) void activateWorkspace(selected); }}><option value="" disabled>选择工作区</option>{conversationWorkspaces.filter((item) => item.state === "ready").map((item) => <option key={item.id} value={item.id}>{item.mode === "isolated_worktree" ? `工作区 #${item.generation}` : "项目工作区"}</option>)}</select></label>}
          <button className="conversation-head-action secondary workspace-create-action icon-action" type="button" disabled={workspaceBusy || Boolean(run) || clearing || stopping || handingOff || readOnlyConversation} onClick={() => void createIsolatedWorkspace()} title={workspaceBusy ? "正在创建工作区…" : "创建会话级 Git 工作区"} aria-label={workspaceBusy ? "正在创建工作区" : "创建会话级 Git 工作区"}><WorkspaceCreateIcon /><span className="action-label">{workspaceBusy ? "处理中" : "新建工作区"}</span></button>
          {conversationWorkspaces.some((item) => item.mode === "isolated_worktree" && item.state === "ready" && !item.active) && <><label className="conversation-workspace-picker workspace-remove-picker"><span>移除目标</span><select aria-label="移除工作区" value={archiveWorkspaceID} disabled={workspaceBusy || Boolean(run) || clearing || stopping || handingOff || readOnlyConversation} onChange={(event) => setArchiveWorkspaceID(event.target.value)}>{conversationWorkspaces.filter((item) => item.mode === "isolated_worktree" && item.state === "ready").map((item) => <option key={item.id} value={item.id}>工作区 #{item.generation}</option>)}</select></label><button className="conversation-head-action secondary workspace-remove-action icon-action" type="button" disabled={!archiveWorkspaceID || workspaceBusy || Boolean(run) || clearing || stopping || handingOff || readOnlyConversation} onClick={() => { const workspace = conversationWorkspaces.find((item) => item.id === archiveWorkspaceID); if (workspace) void archiveWorkspace(workspace); }} title="移除选中的闲置工作区" aria-label="移除选中的闲置工作区"><WorkspaceRemoveIcon /><span className="action-label">移除工作区</span></button></>}
          <button className="conversation-head-action secondary icon-action" type="button" disabled={readOnlyConversation} onClick={openAiConfig} title="AI 配置" aria-label="AI 配置"><ProjectConfigIcon /><span className="action-label">AI 配置</span></button>
          <div className="permission-menu">
          <button className={`permission-trigger ${conversation?.permissionMode === "full_control" ? "full" : ""}`} type="button" aria-haspopup="menu" aria-expanded={showPermissionMenu} disabled={readOnlyConversation || !!run || clearing || stopping || handingOff || changingPermission} onClick={() => setShowPermissionMenu((open) => !open)}><PermissionModeIcon /><span>{permissionLabel}</span></button>
            {showPermissionMenu && <div className="permission-popover" role="menu">{agentPermissionModes(conversation?.agentId || "").map((mode) => <button key={mode} className={conversation?.permissionMode === mode ? (mode === "full_control" ? "selected full" : "selected") : ""} onClick={() => void changePermissionMode(mode)}><b>{permissionCopy[mode].title}</b><span>{permissionCopy[mode].detail}</span></button>)}</div>}
          </div>
          <button className="conversation-head-action new-conversation-action primary" type="button" disabled={readOnlyConversation || sending || stopping || handingOff || conversationTabs.openConversationIds.length >= MAX_OPEN_CONVERSATION_TABS} onClick={openNewConversationParam}><PlusIcon /><span>新会话</span></button>
        </div>
      </div>, document.querySelector('.head-actions-slot') || document.body)}
    {/* 对话面板：快捷方式、对话内容和任务队列 */}
    <section className="conversation-canvas" data-queue-collapsed={!readOnlyConversation && conversationPanels.taskQueue ? "true" : undefined}>
      <aside className="quick-tag-rail" aria-label="常用操作">
        <div className="quick-actions-row">
          {/* 方案 F 色块分区：提示词 / 命令 / 技能各一块浅色底板，白色胶囊浮在上面。
              三块独立折叠（shortcuts / commands / skills），新增入口收进各自块头右端。 */}
          <div className={`quick-block prompt-block${conversationPanels.shortcuts ? " collapsed" : ""}`}>
            <div className="quick-block-head">
              <button type="button" className="quick-block-title" aria-expanded={!conversationPanels.shortcuts} title={conversationPanels.shortcuts ? "展开常用提示词" : "折叠常用提示词"} onClick={() => toggleConversationPanel("shortcuts")}>
                <span className="quick-block-title-text">常用提示词</span>
                <QuickTagChevron />
              </button>
              <b className="quick-block-count">{promptShortcuts.length}</b>
              <button type="button" className="quick-block-add" title="新增常用提示词" aria-label="新增常用提示词" disabled={readOnlyConversation} onClick={() => setShortcutEditor({ kind: "prompt" })}><ShortcutAddIcon /></button>
            </div>
            {!conversationPanels.shortcuts && <ShortcutSortableList items={promptShortcuts} kind="prompt" renderItem={renderPromptCell} draggingDisabled={sortableDraggingDisabled} onReorder={reorderKind} />}
          </div>
          <div className={`quick-block command-block${conversationPanels.commands ? " collapsed" : ""}`}>
            <div className="quick-block-head">
              <button type="button" className="quick-block-title" aria-expanded={!conversationPanels.commands} title={conversationPanels.commands ? "展开常用命令" : "折叠常用命令"} onClick={() => toggleConversationPanel("commands")}>
                <span className="quick-block-title-text">常用命令</span>
                <QuickTagChevron />
              </button>
              <b className="quick-block-count">{commandShortcuts.length}</b>
              <button type="button" className="quick-block-add" title="新增常用命令" aria-label="新增常用命令" disabled={readOnlyConversation} onClick={() => setShortcutEditor({ kind: "command_request" })}><ShortcutAddIcon /></button>
            </div>
            {!conversationPanels.commands && <ShortcutSortableList items={commandShortcuts} kind="command_request" renderItem={renderCommandCell} draggingDisabled={sortableDraggingDisabled} onReorder={reorderKind} />}
          </div>
          {renderSkillGroup({ collapsible: true, collapsed: conversationPanels.skills, onToggle: () => toggleConversationPanel("skills") })}
        </div>
      </aside>
      <section className="chat-center" id="conversation-panel" role="tabpanel" aria-labelledby={conversationTabs.activeConversationId ? `conversation-tab-${conversationTabs.activeConversationId}` : undefined}>
      <ConversationTabStrip state={conversationTabs} conversations={conversationHistory} workspaceLabels={conversationWorkspaceLabels} select={selectConversationTab} close={closeConversationTabFromUI} create={openNewConversationParam} openHistory={openConversationHistory} />
      <section className="timeline" ref={timelineRef} onScroll={onTimelineScroll}>
        <div ref={top} />
        {hasMoreHistory && <button className="secondary load-earlier-history" type="button" disabled={loadingOlderHistory || sending} onClick={() => void loadOlderHistory()}>{loadingOlderHistory ? "加载中" : "加载更早记录"}</button>}
        {isEmptyConversation && <section className="conversation-starter" aria-label="新会话建议"><span>新会话</span><h2>从一个任务开始</h2><div className="conversation-starter-options">{starterSuggestions.map((suggestion) => <button key={suggestion.label} type="button" onClick={() => startFromSuggestion(suggestion.prompt)}>{suggestion.label}</button>)}</div></section>}
        <MessageList timeline={timeline} agentID={conversation?.agentId || "claude-code"} fail={fail} resolving={resolving} decide={decide} projectId={project.id} onViewTask={onViewTask} executionByRun={executionByRun} onOpenExecution={openExecutionParam} registerUserMessageElement={registerUserMessageElement} checkpointByRun={checkpointByRun} activeRunId={run} checkpointBusy={checkpointBusy} rewindDisabled={rewindDisabled} discardedTurnsAfter={discardedTurnsAfter} rewindToCheckpoint={rewindFromTimeline} />
        {run && runIndicatorVariant && runTextVariant && <RunIndicatorView variant={runIndicatorVariant} textVariant={runTextVariant} label={stageLabel} clockStartMs={stageStartMs} />}
        <div ref={bottom} />
      </section>
      <UserMessageDotsRail messages={userMessages} activeIndex={userMessageNavigationIndex} onJump={scrollToUserMessage} />
      <div className={`scroll-buttons${hasNewContent ? " has-new" : ""}`}>
        <button type="button" className="scroll-btn scroll-to-top" title="回到顶部" aria-label="回到顶部" onClick={scrollToTop}><ScrollNavigationIcon direction="top" /></button>
        <button type="button" className="scroll-btn scroll-to-previous-message" title="上一条我的消息" aria-label="上一条我的消息" disabled={loadingOlderHistory || (userMessageNavigationIndex <= 0 && !hasMoreMessageHistory)} onClick={goToPreviousUserMessage}><ScrollNavigationIcon direction="previous" /></button>
        <button type="button" className="scroll-btn scroll-to-next-message" title="下一条我的消息" aria-label="下一条我的消息" disabled={userMessageNavigationIndex < 0 || userMessageNavigationIndex >= userMessages.length - 1} onClick={goToNextUserMessage}><ScrollNavigationIcon direction="next" /></button>
        <button type="button" className={`scroll-btn scroll-to-bottom${hasNewContent ? " pulse" : ""}`} title="回到底部" aria-label="回到底部" onClick={() => scrollToBottomNextFrame("auto")}><ScrollNavigationIcon direction="bottom" /></button>
      </div>
      <form ref={composerRef} className={`composer${pendingApproval ? " has-approval" : ""}${isEmptyConversation ? " empty-session" : ""}`} onSubmit={(event) => void send(event)}>
        <fieldset className="composer-model-switch-lock" disabled={changingModel}>
        {pendingApproval && <ApprovalBanner action={pendingApproval} resolving={resolving} decide={decide} scrollToCard={() => { const el = timelineRef.current?.querySelector(".timeline-entry.tool .tool-card.waiting"); if (el) el.scrollIntoView({ behavior: "smooth", block: "center" }); }} />}
        {skillRefs.length > 0 && <div className="composer-skill-refs" role="group" aria-label="已引用的技能">
          {skillRefs.map((skill) => <span className="composer-skill-ref" key={`${skill.source}-${skill.name}`}>
            <span className="composer-skill-ref-name" title={truncateSkillDescription(skill.description, skill.name) || skill.name}><SkillTagIcon /><span>{skill.name}</span></span>
            <button type="button" className="composer-skill-ref-remove" title={`移除技能 ${skill.name}`} aria-label={`移除技能 ${skill.name}`} disabled={readOnlyConversation || sending || clearing || stopping || handingOff || Boolean(shortcutBusy)} onClick={() => removeSkillRef(skill)}>×</button>
          </span>)}
        </div>}
        {composerSlashHint && <div className="composer-slash-hint" role="status">
          <span>当前 CLI 的命令目录里没有 <code>/{composerSlashHint.commandName}</code>{composerSlashHint.suggestion ? "，" : "，发送后只会得到一句 Unknown command。"}</span>
          {composerSlashHint.suggestion && <button type="button" onClick={() => setComposerText(`/${composerSlashHint.suggestion} `, conversation?.id)}>你是想用 <code>/{composerSlashHint.suggestion}</code> 吗？</button>}
        </div>}
        <div className="composer-card">
        <div className="composer-input-area">
          {inputHistoryPosition !== null && inputHistory.length > 0 && <span className="composer-history-position" role="status" aria-label={`输入历史第 ${inputHistoryPosition} 条，共 ${inputHistory.length} 条`}>{inputHistoryPosition}/{inputHistory.length}</span>}
      <div ref={composerEditRef}
        className="composer-edit"
        contentEditable={!(readOnlyConversation || sending || clearing || handingOff || changingModel || Boolean(shortcutBusy))}
        role="textbox"
        aria-multiline="true"
        aria-label={readOnlyConversation ? "自动编排执行中，仅供查看" : `描述希望 ${activeAgentName} 在当前项目中完成的工作`}
        data-placeholder={readOnlyConversation ? "自动编排执行中，仅供查看" : `描述希望 ${activeAgentName} 在当前项目中完成的工作...`}
        data-disabled={(readOnlyConversation || sending || clearing || handingOff || changingModel || Boolean(shortcutBusy)) || undefined}
        onInput={handleComposerInput}
        onPaste={handleComposerPaste}
        onKeyDown={handleComposerKeyDown}
      />
          <button className={`composer-mobile-shortcut-toggle${showMobileShortcuts ? " open" : ""}`} type="button" title="快捷操作" aria-label="快捷操作" aria-controls="mobile-shortcut-menu" aria-expanded={showMobileShortcuts} disabled={readOnlyConversation || sending || clearing || stopping || handingOff || Boolean(shortcutBusy)} onClick={() => setShowMobileShortcuts((open) => !open)}><ComposerShortcutIcon /></button>
        </div>
        {showMobileShortcuts && <section className="mobile-shortcut-menu" id="mobile-shortcut-menu" aria-label="快捷操作">
          <div className="quick-tag-group"><div className="quick-tag-heading"><span className="quick-tag-heading-label"><ShortcutCategoryIcon kind="prompt" /><span>常用提示词</span><b className="quick-tag-count">{promptShortcuts.length}</b></span><button type="button" title="新增常用提示词" aria-label="新增常用提示词" disabled={readOnlyConversation} onClick={() => { setShowMobileShortcuts(false); setShortcutEditor({ kind: "prompt" }); }}><ShortcutAddIcon /></button></div><ShortcutSortableList items={promptShortcuts} kind="prompt" renderItem={renderMobilePromptCell} draggingDisabled={sortableDraggingDisabled} onReorder={reorderKind} /></div>
          <div className="quick-tag-group command-tags"><div className="quick-tag-heading"><span className="quick-tag-heading-label"><ShortcutCategoryIcon kind="command" /><span>常用命令</span><b className="quick-tag-count">{commandShortcuts.length}</b></span><button type="button" title="新增常用命令" aria-label="新增常用命令" disabled={readOnlyConversation} onClick={() => { setShowMobileShortcuts(false); setShortcutEditor({ kind: "command_request" }); }}><ShortcutAddIcon /></button></div><ShortcutSortableList items={commandShortcuts} kind="command_request" renderItem={renderMobileCommandCell} draggingDisabled={sortableDraggingDisabled} onReorder={reorderKind} /></div>
          {renderSkillGroup()}
        </section>}
          <div className="composer-dock">
            <ComposerMcpTray projectID={project.id} runKey={run} />
            <ContextRing usage={usage} />
            <span className="composer-dock-push" aria-hidden="true"></span>
            {conversation?.id ? <ComposerModelPicker conversationID={conversation.id} agentID={conversation?.agentId || "claude-code"} selected={conversation?.modelOverride || ""} displayed={displayedModel} disabled={readOnlyConversation || Boolean(run) || stopping || changingModel} runActive={Boolean(run)} busy={changingModel} onSelect={selectConversationModel} onFail={fail} /> : <span className="composer-usage-model" title={displayedModel}>{displayedModel}</span>}
            <button className="composer-icon-btn danger composer-clear" type="button" title="清空内容并新建对话" aria-label="清空内容并新建对话" disabled={readOnlyConversation || sending || clearing || stopping || Boolean(shortcutBusy) || handingOff} onClick={clearConversationContext}><ComposerActionIcon action="clear" /></button>
            <button className="composer-icon-btn composer-continue" type="button" title="继续任务" aria-label="继续任务" disabled={readOnlyConversation || sending || clearing || stopping || handingOff} onClick={() => void sendContent("继续", false)}><ComposerActionIcon action="continue" /></button>
            {run && <button className="secondary composer-action composer-stop" type="button" disabled={readOnlyConversation} onClick={() => void stopRun()}>{stopping ? "再次停止" : "停止"}</button>}
            <button className="composer-icon-btn composer-usage-open" type="button" title={!run && !usage ? "用量将在工具就绪后显示" : "使用状态"} aria-label="使用状态" disabled={!run && !usage} onClick={openUsage}><svg className="composer-action-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M5 19V10M10 19V5M15 19v-7M20 19V8" /><path d="M3.5 19.5h17" /></svg></button>
            <span className="composer-send-wrap"><button className="primary composer-action composer-send" disabled={readOnlyConversation || (!text.trim() && skillRefs.length === 0) || sending || clearing || stopping || Boolean(shortcutBusy) || handingOff}><ComposerActionIcon action="send" /><span>{sending ? "发送中" : "发送"}</span></button><button className={`composer-send-more${showSendMenu ? " open" : ""}`} type="button" title="发送方式" aria-label="发送方式" aria-haspopup="menu" aria-expanded={showSendMenu} disabled={readOnlyConversation || sending || clearing || stopping || Boolean(shortcutBusy) || handingOff} onClick={() => setShowSendMenu((value) => !value)}><svg className="composer-send-more-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M6 9.5 12 15.5 18 9.5" /></svg></button>{showSendMenu && <span className="send-menu" role="menu"><button className="send-menu-item" type="button" role="menuitem" disabled={readOnlyConversation || (!text.trim() && skillRefs.length === 0) || sending || clearing || stopping || Boolean(shortcutBusy) || handingOff} onClick={(evt) => { setShowSendMenu(false); evt.currentTarget.form?.requestSubmit(); }}><ComposerActionIcon action="send" /><span><b>立即发送</b><small>立即交给 {activeAgentName}，在下一轮工具调用后继续</small></span></button><button className="send-menu-item" type="button" role="menuitem" disabled={readOnlyConversation || (!text.trim() && skillRefs.length === 0) || sending || clearing || stopping || Boolean(shortcutBusy) || handingOff} onClick={() => void scheduleSend()}><ComposerActionIcon action="schedule" /><span><b>预约发送</b><small>当前任务（含子代理）全部结束后再发送</small></span></button></span>}</span>
          </div>
        </div>
        <ComposerRunnerInfo runnerID={project.runner} agentID={conversation?.agentId || "claude-code"} projectID={project.id} conversationID={conversation?.id || ""} isGitRepo={project.gitBranch !== NON_GIT_BRANCH} run={run} runLabel={runLabel} usage={usage} readOnly={readOnlyConversation} stopping={stopping} onStop={() => void stopRun()} />
        {pendingSendContent !== null && <div className="composer-pending"><span className="composer-pending-icon"><svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="13.2" r="6.2" /><path d="M12 10.5V13l1.8 1.2" /></svg></span><span className="composer-pending-text"><b>已预约发送</b><small>{run ? "等待当前任务完成..." : "等待子代理完成..."}<span className="composer-pending-preview">{pendingSendContent ? (pendingSendContent.length > 40 ? `${pendingSendContent.slice(0, 40)}…` : pendingSendContent) : "已引用的技能"}</span></small></span><button className="composer-pending-cancel" type="button" title="撤回预约并带回输入框" onClick={cancelScheduledSend}>取消</button></div>}
        </fieldset>
      </form>
      </section>
      {!readOnlyConversation && <aside className="task-queue-rail" aria-label="任务队列" data-collapsed={conversationPanels.taskQueue ? "true" : undefined}>
        <TaskQueue projectID={project.id} conversationID={conversation?.id || ""} permissionMode={conversation?.permissionMode} request={projectApi} fail={fail} dispatchDisabled={clearing || stopping || handingOff || !conversation} onDispatched={handleTaskDispatched} openBoard={(taskID) => navigate(`/projects/${project.id}/tasks${taskID ? `/${taskID}` : ""}`)} collapsed={conversationPanels.taskQueue} onToggleCollapsed={() => toggleConversationPanel("taskQueue")} />
      </aside>}
    </section>
    {showNewConversation && <NewConversationDialog runnerID={project.runner} defaults={appPreferences} defaultsLoading={appPreferencesLoading} defaultsError={appPreferencesError} close={closeNewConversation} create={newConversation} />}
    {showHistory && <ConversationHistoryDialog conversations={conversationHistory} historyQuery={historyQuery} loading={historyLoading} error={historyError} activeID={conversation?.id || ""} busyID={activatingConversation} deletingID={deletingConversation} deleteAllBusy={deleteAllConversationsBusy} close={closeHistory} activate={activateConversation} view={viewConversation} search={searchConversationHistory} deleteOne={deleteHistoryConversation} deleteAll={deleteAllHistoryConversations} hasMore={Boolean(conversationHistoryCursor)} loadingMore={loadingMoreConversationHistory} loadMore={loadMoreConversationHistory} />}
    {showFullControlConfirmation && <FullControlConfirmationDialog close={() => setShowFullControlConfirmation(false)} confirm={confirmFullControl} changing={changingPermission} agentID={conversation?.agentId || ""} />}
    {showAgentExecution && agentExecutions.find((execution) => execution.runId === showAgentExecution) && <AgentExecutionDialog execution={agentExecutions.find((execution) => execution.runId === showAgentExecution)!} close={closeAgentExecution} />}
    {showUsage && <UsageDialog agentID={conversation?.agentId || "claude-code"} usage={usage} close={closeUsage} />}
    {showAiConfig && <ProjectAiConfigDialog projectId={project.id} runnerID={project.runner} close={closeAiConfig} />}
    {shortcutEditor && <ShortcutEditor projectID={project.id} state={shortcutEditor} close={() => setShortcutEditor(null)} refresh={refreshShortcuts} fail={fail} agentID={conversation?.agentId || "claude-code"} catalog={commandCatalog} catalogLoading={commandCatalogLoading} refreshCatalog={refreshCommandCatalog} />}
    {shortcutVariables && <ShortcutVariablesDialog state={shortcutVariables} close={() => setShortcutVariables(null)} run={(variables) => { setShortcutVariables(null); void runShortcut(shortcutVariables.shortcut, variables, true); }} />}
    {pendingConfirm && createPortal(<ConfirmDialog title={pendingConfirm.title} message={pendingConfirm.message} confirmLabel={pendingConfirm.confirmLabel} busyLabel={pendingConfirm.busyLabel} danger={pendingConfirm.danger} busy={pendingConfirmBusy} className={pendingConfirm.className} icon={pendingConfirm.icon} onConfirm={pendingConfirm.onConfirm} onCancel={pendingConfirm.onCancel} />, document.body)}
  </>;
}
