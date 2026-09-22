import { FormEvent, useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import QRCode from "qrcode";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { api, asRecord } from "../lib/api";
import { isDesktop } from "../lib/runtime";
import { copyToClipboard } from "../lib/clipboard";
import { DEVICE_ALIAS_MAX_LENGTH, addOrReplaceDevice, activeDevice, deviceDisplayName, deviceLabel, devicePlatform, markDeviceRevoked, normalizeAlias, reconcileDevices, readActiveToken, readDevices, removeDevice, setActiveToken, setDeviceAlias, updateDevice, type MobileDevice } from "../lib/mobile-devices";
import { MOBILE_PROJECT_ORDER_STORAGE_KEY, dismissMobileDragHint, persistOrder, readMobileDragHint, sortProjectIds } from "../lib/project-order";
import { useCardDragReorder } from "../lib/use-card-drag";
import { systemItemFromEvent, eventDiagnostic, cliOutputDiagnostic, getApproval } from "../lib/timeline";
import type { SystemVariant } from "../lib/types";
import { markdownCodeComponents } from "../components/MarkdownCodeBlock";
import { priorityLabels, statusLabels, type Priority } from "../features/tasks/task-model";
// 电脑端那一屏的纯逻辑（五个服务档位 / 心跳措辞 / 实例 ID 缩写 / 刷新的结果文案）。
// 抽成模块的理由见文件头：那是一条**优先级级联**，扫源码正则验不出"哪一档赢"
// （2026-09-17 变异检验实测漏网），必须用行为断言守。
import { desktopServiceView, heartbeatAgoText, refreshDesktopStatusMessage, shortInstanceID } from "../features/remote/desktop-service";
// 「已绑定手机」那一段的纯逻辑（绑定项归一化 / 最近同步的四档 / 平台文案）。
// 和上面同一个理由：`lastUsedAt` 的 undefined 与 null 必须走不同分支，合并成一个
// 就跟「还没回来」写成「没有数据」是同一个错，而那种错源码级断言看不出来。
import { normalizeBindings, phoneSyncView, platformLabel, syncAgeFrom, type DesktopBinding } from "../features/remote/desktop-phone";
import { applyPendingTaskMutations, mutationReflectsInSnapshot, newPendingTaskID, withResolvedCreateID, type PendingTaskMutation } from "../lib/task-mutations";
// 会话末尾那条「正在处理」状态条的判据（徽标 / 阶段词 / 已耗时）。三份读数来自三个不同来源，
// 收口在纯函数里，页面只调用 + 渲染 —— 理由见 lib/processing-indicator.ts 的文件头。
import { formatElapsed, latestNotice, processingBadge, processingStageText } from "../lib/processing-indicator";
// 「新建会话」的乐观更新规则。会话先在**本机**建好（立刻能进去、能打字），命令只在后台
// 发给电脑端 —— 手机端的操作不该等电脑端点头。规则的每一条分支在
// src/lib/conversation-mutations.ts 里有独立的行为用例。
import { applyPendingConversations, isPendingConversationID, matchPendingConversation, newPendingConversationID, type PendingConversation } from "../lib/conversation-mutations";
// 文件视图。它是**整块复用**桌面端那套文件工作台：适配器只负责把 REST 调用翻译成
// 云端的中继请求（见 mobile-fs-request.ts），树/查看器/编辑器/JSON/SQLite 全部是同一份实现。
import { FilesPanel, type FilesPanelHandle } from "../features/files/FilesPanel";
import { createMobileFsRequest, type MobileRpcReply } from "../features/files/mobile-fs-request";
// Git 工作台同属一个子态槽位，同样是**整块复用**桌面端那套（见 mobile-git-request.ts）。
import { MobileGitPanel, type MobileGitPanelHandle } from "../features/git/MobileGitPanel";
import { projectFileReference } from "../lib/project-path";
import type { NavigationGuard } from "../components/ProjectLayout";
import { useNavigate } from "react-router-dom";
import { Capacitor } from "@capacitor/core";
import { App as CapacitorApp } from "@capacitor/app";
import type { PluginListenerHandle } from "@capacitor/core";
import { BarcodeFormat, BarcodeScanner, LensFacing } from "@capacitor-mlkit/barcode-scanning";
import { invoke } from "@tauri-apps/api/core";
import { useDocumentVisible } from "../lib/useDocumentVisible";
import { checkMobileUpdate, type MobileUpdateState } from "../features/updater/mobile-update";
// markdown 渲染的样式依赖必须由本页面自己声明：真机上 main.tsx 全局引入过它，
// 所以"少这一行"在真机看不出来 —— 但任何绕过 main.tsx 的入口（预览夹具、单页测试）
// 都会渲染出裸的 pre / blockquote / 复制按钮。页面自包含更稳。
import "../markdown.css";
import "./mobile-remote.css";
// 电脑端（`!mobileApp`）那一屏是**桌面管理页**，不是手机页：它有自己的页头、两栏工作台与
// 一套 `desktop-remote-*` 类名。样式放独立文件，免得与手机页那套 860px 卡片列互相串味。
import "./desktop-remote.css";

type Instance = { instanceId: string; name: string; status: string; lastAgentSequence: number; lastSeenAt?: string };
// ⚠️ `title` / `description` 是**线协议字段**，不是我们自己的数据：服务端 Go 侧那条 `json:"title"`
// 没有 omitempty（键一定在），但值仍可能是 null —— 而 relay 还会再穿一次 JSON。
// 所以类型写成 `string | null` 而不是 `string`：**让 tsc 去守这件事** —— 任何"直接 `.trim()` /
// `.toLowerCase()`"的直取都会被编译期挡下，而不是等到真机上整页白屏（2026-09-18 复查收紧）。
// 历史上这里是 `string`，而 `taskSummary` 里那句直取的 `title.trim()` 真炸过（有实测复现）。
type Task = { id: string; title: string | null; description?: string | null; priority: string; status: string; updatedAt: string };
type Message = { id: string; runId?: string; role: "user" | "assistant"; content: string; createdAt: string };
// Notice 是"运行过程"而非"对话内容"：API 重试、上下文压缩、后台任务、执行失败。
// 手机端原先只转发 user/assistant 消息，这类事件在到达时被直接丢弃，于是电脑上
// 已经显示"重试中 / 正在压缩上下文 / 执行失败"，手机端却像卡住了一样什么都没有。
type RemoteNotice = {
  id: string;
  runId: string;
  createdAt: string;
  variant: SystemVariant | "error";
  title: string;
  detail?: string;
  // task 卡片按成功/失败/中性着色，与桌面端 system-card 的 state 后缀同源。
  state?: string;
  // 同 id 再次到达时怎么处理：
  //   "append"  —— 逐行到达的内容（CLI 输出），新行并进同一条；
  //   "replace" —— 状态推进（工具确认 pending → allow/deny），用最新状态覆盖。
  // 缺省 = 同 id 视为重复，保留先到的那条。
  merge?: "append" | "replace";
};
type RemoteConversation = { id: string; title: string; status: string; agentId: string; lastActivityAt: string; isCurrent: boolean; messages: Message[]; notices?: RemoteNotice[] };
// 电脑端数据库里的「常用提示词 / 常用命令」条目。字段与 control-server 的 Shortcut 一一对应
// （只少了手机端用不到的 createdAt/updatedAt）。
type RemoteShortcut = { id: string; name: string; description: string; kind: string; template: string; scope: string; defaultAction: string; groupName?: string; pinned: boolean; enabled: boolean; sortOrder: number; projectIds: string[] };
// 电脑端（或 SSH 远端）文件系统上扫出来的技能。source 决定分组标题（用户 / 项目 / 系统插件）。
type RemoteSkill = { name: string; description: string; agent: string; env: string; source: string };
type ProjectEnvironment = "windows" | "wsl" | "remote-linux";
type Project = { id: string; name: string; runner: string; environment?: ProjectEnvironment; running?: boolean; gitBranch: string; tasks: Task[]; conversations: RemoteConversation[]; skills?: RemoteSkill[] };
type Snapshot = { snapshotRevision: number; observedAt: string; projects: Project[]; shortcuts?: RemoteShortcut[] };
type CommandState = { commandId: string; status: string; result?: unknown };
type AcceptedCommand = { commandId: string; status: string };
// 一次快照拉取的真实结果。"updated/unchanged" 都算成功，区别只在版本号有没有前进；
// "skipped" 表示这次请求没有真正发出去（没有选中的实例 / 已被更新的请求取代）。
// 手动刷新必须拿到这个结果才能给出"已刷新 / 已是最新 / 刷新失败"的可信反馈——只靠
// setSnapshot 是看不出有没有生效的。
type SnapshotFetchOutcome = "updated" | "unchanged" | "failed" | "skipped";
type SnapshotFetchResult = { outcome: SnapshotFetchOutcome; snapshot: Snapshot | null; reason?: string };
// 实例列表请求的结果。superseded 必须与 failed 分开：列表每 5 秒被轮询拉一次，
// 手动刷新那一次被轮询抢答是常态而不是故障（见 loadInstances 注释）。
type InstanceFetchResult = { instances: Instance[]; outcome: "ok" | "failed" | "superseded"; reason?: string };
// 刷新按钮的结果条：refreshing 与其它状态互斥，at 为完成时刻（refreshing 时为空）。
type RefreshStatus = { state: "refreshing" | "success" | "unchanged" | "failed"; message: string; at: Date | null };
type PendingMessage = { requestId: string; content: string; createdAt: string };
type ProcessingConversation = { count: number; startedAt: number; snapshotRevision: number };
type ProcessingConversations = Record<string, ProcessingConversation>;

const terminalCommandStatuses = ["completed", "failed", "expired", "cancelled", "indeterminate"];

// 会话视图里刷新状态条的存活时间。顶栏折成一行才省下 ~40px，这条回执不该把它吃回去；
// 6 秒足够看清"已刷新/失败"，又不至于让人以为它要一直挂着。仅会话视图生效（见该 effect）。
const REMOTE_REFRESH_STATUS_TTL_MS = 6000;

function commandStatusRank(status: string): number {
  if (status === "queued" || status === "pending") return 0;
  if (status === "received" || status === "executing") return 1;
  return terminalCommandStatuses.includes(status) ? 2 : 1;
}

function mergeCommandState(current: CommandState | null, next: CommandState): CommandState {
  if (!current || current.commandId !== next.commandId) return next;
  // Poll responses can arrive out of order. A terminal state must never be
  // replaced by an older queued/executing response.
  if (commandStatusRank(next.status) < commandStatusRank(current.status)) return current;
  if (terminalCommandStatuses.includes(current.status) && !terminalCommandStatuses.includes(next.status)) return current;
  return next;
}

// 见下方 taskPriorityLabel：规范状态一律取桌面看板那份 statusLabels，这里只补服务端可能出现的
// 非规范状态。曾经手机端把 action_required 写成"需要操作"，而同一屏的分类胶囊、桌面看板与
// insights 都叫"需处理"——同一个状态在一屏里两个名字，就是没共用一份枚举的代价。
function taskStatusLabel(status: string): string {
  const labels: Record<string, string> = {
    ...statusLabels,
    queued: "排队中",
    completed: "已完成",
    failed: "失败",
    blocked: "已阻塞",
  };
  return labels[status] || status || "未知状态";
}

function taskStatusClass(status: string): string {
  return `status-${status.replace(/[^a-z0-9_-]/gi, "-") || "unknown"}`;
}

// 优先级文案复用桌面看板那份（task-model.ts 的 priorityLabels），不在手机端另写一份枚举。
function taskPriorityLabel(priority: string): string {
  return priorityLabels[priority as Priority] || priority || "普通";
}

// 优先级选择器的档位顺序也直接取那份枚举的书写顺序（urgent/high/normal/low），
// 手机端不手写第二份清单 —— 桌面端哪天加一档，这里自动跟着出现。（旧实现把四个
// value 与四句中文一字一句抄在 option 里，两处枚举各改各的。）
const priorityOptions = Object.keys(priorityLabels) as Priority[];

// 移动端优先级选择器：**不许用原生 select**。
//
// 症状（用户报的）：在手机上编辑任务时点「优先级」，跳出来的是系统自己那个很难看的
// 单选页面（Android 是整屏列表、iOS 是底部滚轮），而不是在表单里就地选一下。
// 原生 select 弹出的那一层由系统绘制，CSS 一行都管不到 —— 想让它跟本页是一套视觉，
// 只能自己画。这里做成一行分段按钮：四档同屏可见、点一下就选中，和任务分类栏、
// 「新会话」选 Agent 是同一套交互语言。
//
// 三条别动的：① 选中态只由 `aria-checked` 决定（样式与无障碍语义同源，不会各说各话）；
// ② 每颗都必须是 `type="button"`，否则回车/触摸会顺手把整个表单提交掉；
// ③ 标题要写成 span 而不是原生 label 元素包住这排按钮 —— button 是 labelable 元素，
//    包进去之后"点标题文字"会变成"点第一颗按钮"（无声地把优先级选成「紧急」）。
function MobilePriorityPicker({ name, value, onChange }: { name: string; value: string; onChange: (next: string) => void }) {
  return <div className="mobile-priority-field">
    <span className="mobile-priority-field-label" id={`${name}-label`}>优先级</span>
    <div className="mobile-priority-picker" role="radiogroup" aria-labelledby={`${name}-label`}>
      {priorityOptions.map((option) => <button key={option} type="button" role="radio" aria-checked={value === option} className="mobile-priority-option" onClick={() => onChange(option)}>{priorityLabels[option]}</button>)}
    </div>
  </div>;
}

// 分类胶囊的文案同样取 statusLabels：以前这两份是各写一遍的，谁改一处另一处就悄悄对不上。
//
// 2026-09-16 按用户要求去掉两颗：
//   · 「全部」—— 这条栏的语义是"按状态分类"，再放一颗"不分类"属于另一个层级；去掉之后默认落在
//     「待处理」（打开面板时最该动手的那一档），而**队列整体为空**时另有整体空态（见 taskPanelEmpty），
//     所以不会出现"以为任务没了"的读法。
//   · 「已取消」—— 终态，不该占一颗常驻位。但它也没有被"遗忘"：真存在已取消任务时，栏下那行
//     `.mobile-task-hidden` 会如实报出条数 —— 静默少一条比多一颗胶囊更糟（MOBILE-UI 的完备性规则）。
const taskFilters = [
  { id: "todo", label: statusLabels.todo },
  { id: "running", label: statusLabels.running },
  { id: "awaiting_review", label: statusLabels.awaiting_review },
  { id: "action_required", label: statusLabels.action_required },
  { id: "done", label: statusLabels.done },
] as const;

type TaskFilter = typeof taskFilters[number]["id"];

function taskSummary(task: Task): string {
  // ⚠️ `title` 必须显式兜 null/undefined：它是**线协议字段**，`type Task` 里那句 `title: string`
  // 只是我们的假设，不是保证（本页编辑路径早就写着 `task.title || ""`，桌面端 `taskDisplayTitle`
  // 也一样兜底）。一个 null 就能在 `useMemo` / 渲染里抛错 → 错误边界接管 → **整个手机页白屏**，
  // 2026-09-18 复查用夹具（`title: null` 的任务）实测复现过：
  // `TypeError: Cannot read properties of null (reading 'trim')`，而且**不用碰搜索框**就能触发。
  const title = (task.title || "").trim();
  if (title) return title;
  const description = task.description?.trim() || "";
  return description || "暂无任务内容";
}

// 手机上对任务做的每一次增删改：先在本地生效，再把命令发给电脑端。
//
// 老实现是"发命令 → 轮询等最多 30 秒 → 再整份重拉快照"，整个过程把整屏置为 busy。
// 真机实测（2026-09-16）命令往返从下午的 3 秒劣化到晚上的 19~75 秒，全部越过 30 秒
// 上限：用户看到的是"点了没反应、过一会儿还报失败"，而桌面端其实晚几十秒才执行完。
//
// 现在改成：
//   1. 本地立刻改（乐观），立刻给反馈（那张卡片标「同步中」）；
//   2. 命令在后台发出去，**不**阻塞面板上任何别的操作；
//   3. 命令进入终态后重拉一次快照，确认无误再把本地改动摘掉；
//   4. 云端拒绝 / 电脑端离线 / 命令过期，就把这次改动撤掉并说明原因。
//
// 叠加与"是否已反映到快照"这两条规则在 src/lib/task-mutations.ts，有独立的逻辑用例。
function conversationStatusLabel(status: string): string {
  // `creating` 是**本机专有**的一档（见 lib/conversation-mutations.ts）：这条会话已经在本机
  // 建好、用户已经能进去打字，只是电脑端还没来得及分配真 id。不写这一档，历史列表里
  // 刚建的那条会显示成英文的 "creating"。
  const labels: Record<string, string> = { idle: "就绪", creating: "创建中", queued: "排队中", running: "执行中", completed: "已完成", failed: "失败", stopped: "已停止" };
  return labels[status] || status || "未知状态";
}

// ⚠️ 已知例外（docs/42 §15）：本组件桌面端与手机端共用，而**手机端目前读不到
// 工具目录**（手机走云端 /api/remote/*，控制服务的 GET /api/agents 不在那条通道上）。
// 因此这里暂时保留按工具 ID 的清单；强行改成读目录会让手机端把工具显示成裸 id，
// 那是比现在更差的退化。正确修法是把工具目录放进手机快照（或让云端转发该端点），
// 那是一条 REMOTE-CONTRACT 变更，单独排期。
function conversationAgentLabel(agentId: string): string {
  return agentId === "codex" ? "Codex" : "Claude Code";
}

// 状态卡图标沿用桌面时间线那一套字形，两端看到的是同一个符号。
const noticeIcons: Record<RemoteNotice["variant"], string> = { compact: "◐", compact_result: "✓", compact_boundary: "≡", api_retry: "↻", task: "▸", error: "!" };

// 工具确认的只读文案。桌面端是工具卡上的 allow/deny 按钮，手机端不做交互，
// 只用一张卡说明"卡在哪了"；键是 approval 事件的类型后缀。
const approvalLabels: Record<string, { title: string; state: string }> = {
  pending: { title: "等待工具确认", state: "info" },
  allow: { title: "工具已确认", state: "success" },
  deny: { title: "工具已拒绝", state: "failed" },
  aborted: { title: "工具确认已中止", state: "info" },
  timeout: { title: "工具确认超时", state: "failed" },
};

// 一次会话只保留最近这一段运行记录：时间久了状态卡会越积越多，而用户翻历史时
// 关心的是刚刚发生了什么，不是三天前的每一次重试。
const maxConversationNotices = 24;

// 「＋」面板里的技能分组。来源键与桌面端 ConversationPage 的 skillSourceMeta 完全一致，
// 顺序也照抄（项目 > 用户 > 系统/官方），两端看到的技能分组不会长得不一样。
const skillSourceOrder = ["project", "user", "plugin"];
const skillSourceLabels: Record<string, string> = { project: "项目", user: "用户", plugin: "系统/官方" };

// 技能片段：与桌面端 mergeSkillPrompt 的文案**逐字一致**（见 docs/22）。技能本身只是文件系统上
// 的一个 SKILL.md，手机端不可能去读它，只能把"名称 + 描述"拼成一句可引用的提示词让 CLI 自己去
// 加载——所以这段文案必须和桌面端一字不差，否则同一个技能在两端会让 Agent 收到不同的指令。
function skillPrompt(skill: RemoteSkill): string {
  return skill.description && skill.description !== skill.name
    ? `请使用技能 <${skill.name}>：${skill.description}`
    : `请使用技能 <${skill.name}>`;
}

// 「用户写的正文」与「已引用的技能」拼成真正发出去的文本：引用在前、正文在后，中间空一行。
// 这里是"输入框里显示什么"与"实际发什么"之间唯一的分界线 —— 手机端只显示一颗胶囊，
// 电脑端 CLI 收到的仍是那句完整的自然语言引用（headless 通道不认 `/skill-name`，见 docs/22）。
// 与桌面端 ConversationPage 的 composeSkillMessage 必须**逐字一致**，否则同一个技能两端发出的指令不同。
function composeSkillMessage(text: string, refs: RemoteSkill[]): string {
  if (refs.length === 0) return text.trim();
  const body = refs.map(skillPrompt).join("\n");
  return text.trim() ? `${body}\n\n${text.trim()}` : body;
}

// 「引用某条消息」的本地状态。**只留一条**（与微信 / DeepSeek 一致）：再点一条就换掉，
// 不做多引用队列 —— 输入条上方那行胶囊一旦有两条，就会把输入框顶高一整行，
// 而且用户很难看出"哪条引用会跟着这次发送走"。
type MessageQuote = { id: string; role: "user" | "assistant"; content: string };

// 引用块的行前缀。**空行也要带 ">"**：markdown 把空行当作引用结束，不补的话一段带空行的
// 长引用会被拆成「引用 + 普通段落」两截，电脑端读起来像两件事。
// 超长引用截断到 1200 字并**显式写明已截断** —— 悄悄截断比不截断更糟：用户以为整段都发过去了。
const QUOTE_MAX_CHARS = 1200;
function quoteBlock(quote: MessageQuote): string {
  const clipped = quote.content.length > QUOTE_MAX_CHARS;
  const body = clipped ? `${quote.content.slice(0, QUOTE_MAX_CHARS)}\n（引用内容过长，已截断）` : quote.content;
  return body.split("\n").map((line) => (line ? `> ${line}` : ">")).join("\n");
}

// 「引用块 + 用户正文」拼成一段，再交给 composeSkillMessage 去加技能指令。
// 顺序刻意是**技能 → 引用 → 正文**：技能指令是给 Agent 的命令，引用与正文都是它的宾语。
// 复用 composeSkillMessage 而不是在这里再拼一次技能片段 —— 那段文案必须与桌面端逐字一致，
// 只能有一个来源。
function withQuoteBlock(text: string, quote: MessageQuote | null): string {
  if (!quote) return text;
  const body = text.trim();
  return body ? `${quoteBlock(quote)}\n\n${body}` : quoteBlock(quote);
}

// 乐观气泡（`pending-*`）与流式占位（`stream-*`）这两种消息不能当"历史消息"对待：
// 前者还没真的发出去（对它「重新发送」会重复发一条），后者内容还在变（引用它等于引用半句话）。
// 两种都只留「复制」。
// `pending-*`（本地乐观气泡）与 `stream-*`（流式占位）都是"还没落定"的占位 id。
// 它们被云端消息顶替时 id 会变，而**那不是一条新消息** —— 判据见 arrivingKeys 那个 effect。
function isPlaceholderKey(key: string): boolean {
  return key.startsWith("pending-") || key.startsWith("stream-");
}

function isTransientMessage(message: Message): boolean {
  return isPlaceholderKey(message.id);
}

// 「这条回复正在被写入」——只有它带元信息行那三点。
// ⚠️ 与 `isTransientMessage` 分开是有意的：那个是"还没落定"，含 `pending-*`（乐观气泡）。
// 乐观气泡的语义是"还没发出去"，把它说成"正在写"是**说错话** —— 与
// `transientReason(…, "row" | "quote" | "resend")` 里"两种未落定各有各的说法"同一条纪律。
function isStreamingMessage(message: Message): boolean {
  return message.id.startsWith("stream-");
}

// 「这条消息此刻为什么只能复制」的**唯一文案来源**。三处共用：图标行分组的 aria-label
// （读屏用户在浏览模式下进到这一组时能听到）、两个禁用图标的 title / aria-label。
// 分开写三份的结果是"改一处、另两处说错原因" —— 而这两句话在项目里被明确要求**必须分开**：
// 把「还没发出去」说成「正在写入」是在替它编一个不存在的进度。
function transientReason(message: Message, action: "row" | "quote" | "resend"): string {
  const streaming = message.id.startsWith("stream-");
  if (action === "resend") return "这条消息还没发出去，不用重发";
  if (action === "row") return streaming ? "回复还在写入中，暂时只能复制" : "这条消息还没发出去，暂时只能复制";
  return streaming ? "回复还在写入中，暂时不能引用" : "这条消息还没发出去，暂时不能引用";
}

// 消息卡片上那几个动作的图标。**只有图标、没有文字**（2026-09-20 按用户要求从"底部动作面板"
// 改回图标行）：面板那一版把目标预览卡、三行「标题 + 副说明」、「取消」全铺在一张白板上，
// 信息量是动作本身的好几倍，而这几个动作每个都只有一句无歧义的意思。
// 代价全部转嫁到可识别性上，所以三件事必须一起做，缺一条就是"看不懂的图标"：
//   ① `aria-label` + `title` 都写全（读屏念得出、桌面端悬停看得见）；
//   ② 失焦/禁用态不能只靠颜色（`.mobile-message-action-icon:disabled` 另有降透明度）；
//   ③ 图标本身取通用形状（复制=重叠方块、引用=左侧竖条＋正文线、重发=回卷箭头）。
// 画 30px、靠 ::after 外扩 7px 补到 44×44 热区 —— 与「⋯」同一档；**相邻两颗的间距因此
// 必须是 14px**（外扩 7+7），小于它两颗热区就互相压住，点左边实际命中右边。
function MessageActionIcon({ kind }: { kind: "copy" | "quote" | "resend" | "copied" | "failed" }) {
  if (kind === "copy") return <svg viewBox="0 0 24 24" aria-hidden="true" fill="none" stroke="currentColor" strokeWidth="1.9" strokeLinecap="round" strokeLinejoin="round"><rect x="9" y="9" width="11" height="11" rx="2.2" /><path d="M15 5.5H6.5A2.5 2.5 0 0 0 4 8v8.5" /></svg>;
  // 复制成功的就地反馈：图标从"两个方块"变成勾。文字版（旧面板那行「已复制」）没有了，
  // 这里就必须让图标自己变 —— 只有颜色变化对色觉障碍用户等于没反馈。
  if (kind === "copied") return <svg viewBox="0 0 24 24" aria-hidden="true" fill="none" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round" strokeLinejoin="round"><path className="mobile-action-check" d="M4.5 12.5 9.5 17.5 19.5 7" /></svg>;
  if (kind === "failed") return <svg viewBox="0 0 24 24" aria-hidden="true" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round"><path d="M12 6.5v7.5" /><path d="M12 17.6v.1" /></svg>;
  if (kind === "quote") return <svg viewBox="0 0 24 24" aria-hidden="true" fill="none" stroke="currentColor" strokeWidth="1.9" strokeLinecap="round" strokeLinejoin="round"><rect x="3.4" y="5" width="2.6" height="14" rx="1.3" fill="currentColor" stroke="none" /><path d="M9.6 8h11" /><path d="M9.6 12h11" /><path d="M9.6 16h6.4" /></svg>;
  return <svg viewBox="0 0 24 24" aria-hidden="true" fill="none" stroke="currentColor" strokeWidth="1.9" strokeLinecap="round" strokeLinejoin="round"><path d="M9 14 4 9l5-5" /><path d="M4 9h10.5A5.5 5.5 0 0 1 20 14.5V20" /></svg>;
}

// 快捷方式在手机端该走哪条路。fill 只渲染、内容回填到手机输入框；run / confirm 直接在电脑端
// 执行（与桌面端点击同一条服务端路径）。
// 片段（snippet）永远是 fill：服务端的 run 接口明确拒绝片段（"snippets cannot run"）。
function shortcutAction(shortcut: RemoteShortcut): "fill" | "run" | "confirm" {
  if (shortcut.kind === "snippet") return "fill";
  return shortcut.defaultAction === "run" || shortcut.defaultAction === "confirm" ? shortcut.defaultAction : "fill";
}

// 命令结果里取渲染后的正文。形状与电脑端 /preview 的响应一致：{shortcut, content, requiresConfirmation}。
function shortcutContentFromCommandResult(result: unknown): string {
  if (!result || typeof result !== "object") return "";
  const content = (result as { content?: unknown }).content;
  return typeof content === "string" ? content : "";
}

function noticeTime(createdAt: string): string {
  const parsed = new Date(createdAt);
  if (Number.isNaN(parsed.getTime())) return "";
  // 只到分钟：卡片右侧的时间是辅助信息，秒在窄屏上只会把标题挤窄。
  return parsed.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}

// 消息时间：当天只给时刻（19:31），跨天才补日期（9/12 19:31）。
// 原来的 toLocaleString() 输出"2026/9/13 19:31:15"——年份和秒对读对话毫无帮助，
// 却占掉气泡顶部最显眼的一行。
function messageTime(createdAt: string): string {
  const parsed = new Date(createdAt);
  if (Number.isNaN(parsed.getTime())) return "";
  const now = new Date();
  const clock = parsed.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  if (parsed.toDateString() === now.toDateString()) return clock;
  const day = `${parsed.getMonth() + 1}/${parsed.getDate()}`;
  // 跨年还要带年份，否则"12/31 19:41"会被读成今年的。
  return `${parsed.getFullYear() === now.getFullYear() ? day : `${parsed.getFullYear()}/${day}`} ${clock}`;
}

// noticeFromEventFields 解析一条状态/诊断事件本身（不含会话归属），解析规则全部
// 来自桌面时间线（systemItemFromEvent / eventDiagnostic），手机端不维护第二套文案
// 枚举。解析不出内容的（CLI init 等）返回 null，由调用方丢弃。
function noticeFromEventFields(type: string, payload: unknown, id: string, runId: string, createdAt: string): RemoteNotice | null {
  const source = { id, type, payload: asRecord(payload), runId, createdAt };
  // CLI 输出：逐行到达，聚合成同一条卡（与桌面 buildTimeline 共用 cliOutputDiagnostic）。
  // 不聚合的话，一次编译失败能刷出几十张卡片，把重试/压缩那几张真要紧的挤下去。
  if (type === "stderr") {
    const message = typeof source.payload.message === "string" ? String(source.payload.message) : "";
    const output = cliOutputDiagnostic([message]);
    if (!output) return null; // 心跳行等噪声（过滤规则见 isIgnoredCLIStderr）
    return { id: `stderr:${runId || createdAt}`, runId, createdAt, variant: "error", title: output.title, detail: output.detail, merge: "append" };
  }
  // 工具确认：桌面把它做成工具卡上的按钮；手机端只读，但必须显示——运行卡在等确认时，
  // 用户至少要知道"为什么没动静"，而不是以为进程死了。
  const approval = getApproval(source);
  if (approval) {
    const command = typeof approval.toolInput.command === "string" ? approval.toolInput.command.replace(/\s+/g, " ").trim() : "";
    const detail = command ? `${approval.toolName}：${command.slice(0, 120)}${command.length > 120 ? "…" : ""}` : approval.toolName;
    // 状态取事件类型后缀而不是 getApproval 的 status：aborted/timeout 在那边被归成
    // "pending"，沿用会让已经中止的确认一直显示成"等待确认"（5 分钟无人操作就会超时，
    // 这条路径并不罕见）。
    const resolution = type.slice("approval.".length);
    const label = approvalLabels[resolution] || approvalLabels.pending;
    return {
      id: `approval:${approval.approvalId}`,
      runId,
      createdAt,
      variant: "task",
      title: label.title,
      detail: resolution === "pending" ? `${detail} · 请在电脑上确认` : detail,
      state: label.state,
      merge: "replace",
    };
  }
  const system = systemItemFromEvent(source);
  if (system) {
    const state = typeof system.metadata?.state === "string" ? system.metadata.state : "";
    return { id: system.id, runId: system.runId, createdAt: system.createdAt, variant: system.variant, title: system.title, detail: system.detail, state };
  }
  const diagnostic = eventDiagnostic(source);
  // deferFallback 指的是"只有退出码、没有原因"的那条（Codex exited: exit status N）。
  // 桌面端会等同一次运行有没有更详细的诊断再决定展示它，手机端没有那套归并，
  // 直接跳过，免得用一句没有信息量的话盖住真正的错误。
  if (diagnostic && !diagnostic.deferFallback) {
    return { id: source.id || `${type}:${createdAt}`, runId, createdAt, variant: "error", title: diagnostic.title, detail: diagnostic.detail };
  }
  return null;
}

// noticeFromRealtimeEvent 处理实时通道的事件：除了事件本身，还必须先拿到它属于
// 哪个会话。返回 null 的事件交给调用方按原逻辑处理（会话创建、流式增量、普通消息）。
function noticeFromRealtimeEvent(event: { eventId?: string; type?: string; taskRunId?: string; payload?: unknown; createdAt?: string }): { conversationId: string; notice: RemoteNotice } | null {
  const record = asRecord(event.payload);
  // 事件必须自带会话归属：状态事件由 CLI 产生，payload 里本来没有会话字段，
  // 是控制端在中继副本上补写的（见 remoteEventPayloadWithConversation）。拿不到
  // 就返回 null 让快照回放补上——宁可晚一步，也不把 A 会话的重试贴到 B 会话里。
  const conversationId = typeof record.conversationId === "string" ? record.conversationId : "";
  if (!conversationId) return null;
  const notice = noticeFromEventFields(
    typeof event.type === "string" ? event.type : "",
    record,
    typeof event.eventId === "string" ? event.eventId : "",
    typeof event.taskRunId === "string" ? event.taskRunId : "",
    typeof event.createdAt === "string" && event.createdAt ? event.createdAt : new Date().toISOString(),
  );
  return notice ? { conversationId, notice } : null;
}

// noticesFromSnapshot 解析快照回放的记录。快照里给的是事件原文（type + payload），
// 与实时通道同源但少一层信封，所以这里必须走同一个解析函数——否则会出现"实时
// 到达时是一张正常的状态卡，刷新之后变成一张没有类型、配色全丢的空卡"。
function noticesFromSnapshot(raw: unknown): RemoteNotice[] {
  if (!Array.isArray(raw)) return [];
  let items: RemoteNotice[] = [];
  for (const entry of raw) {
    const record = asRecord(entry);
    const notice = noticeFromEventFields(
      String(record.type || ""),
      record.payload,
      String(record.id || ""),
      String(record.runId || ""),
      String(record.createdAt || ""),
    );
    // 逐条并进：同一个 approvalId 的 pending → allow/deny、同一个 run 的多行输出，
    // 都要按各自的 merge 语义收成一条。
    if (notice) items = appendNotice(items, notice);
  }
  return items;
}

function appendNotice(notices: RemoteNotice[] | undefined, notice: RemoteNotice): RemoteNotice[] {
  const current = notices || [];
  const index = current.findIndex((item) => item.id === notice.id);
  if (index < 0) {
    const next = [...current, notice];
    return next.length > maxConversationNotices ? next.slice(next.length - maxConversationNotices) : next;
  }
  if (notice.merge === "replace") {
    const existing = current[index];
    // 快照有上传节流，可能带着比本地更旧的状态回来（工具确认已经在电脑上被允许，
    // 但快照里还只有 pending）。只有不更旧的才允许覆盖，否则界面会从"已确认"
    // 倒退回"等待确认"，用户以为又被卡住了。
    if ((Date.parse(notice.createdAt) || 0) < (Date.parse(existing.createdAt) || 0)) return current;
    return current.map((item, at) => (at === index ? notice : item));
  }
  if (notice.merge === "append" && notice.detail) {
    const existing = current[index];
    // 同一行可能因 SSE 重连被重复投递；按行去重，免得"CLI 输出"里同一句出现两遍。
    const lines = (existing.detail || "").split("\n");
    if (lines.includes(notice.detail)) return current;
    const detail = existing.detail ? `${existing.detail}\n${notice.detail}` : notice.detail;
    return current.map((item, at) => (at === index ? { ...existing, detail } : item));
  }
  return current;
}

// mergeNotices 取两份记录的并集。逐条并进而不是按 id 覆盖：CLI 输出要累积行、
// 工具确认要保留最新状态，两种语义只有 appendNotice 这层知道，覆盖式合并会把
// 已经收到的 stderr 行丢掉。
function mergeNotices(existing: RemoteNotice[], incoming: RemoteNotice[]): RemoteNotice[] {
  if (existing.length === 0) return incoming;
  if (incoming.length === 0) return existing;
  let merged = existing;
  for (const notice of incoming) merged = appendNotice(merged, notice);
  return merged.slice().sort((left, right) => (Date.parse(left.createdAt) || 0) - (Date.parse(right.createdAt) || 0));
}

function projectEnvironment(project: Project): ProjectEnvironment {
  if (project.environment === "windows" || project.environment === "wsl" || project.environment === "remote-linux") return project.environment;
  return project.runner.startsWith("ssh-") ? "remote-linux" : "wsl";
}

function projectEnvironmentLabel(environment: ProjectEnvironment): string {
  return environment === "windows" ? "Windows" : environment === "wsl" ? "WSL" : "SSH";
}

function ProjectEnvironmentIcon({ environment }: { environment: ProjectEnvironment }) {
  if (environment === "windows") return <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M3 5.5 10.5 4v7H3v-5.5ZM13 3.5 21 2v9h-8v-7.5ZM3 13h7.5v7L3 18.5V13ZM13 13h8v9l-8-1.5V13Z" /></svg>;
  if (environment === "wsl") return <svg viewBox="0 0 24 24" aria-hidden="true"><g fill="currentColor"><path fillRule="evenodd" d="M9 7.8h6c3.2 1.2 4.2 3.4 4 6.2-.2 2.8-1 5.2-3.4 6.6-1.2.8-6 .8-7.2 0C6 19.2 5.2 16.8 5 14c-.2-2.8.8-5 4-6.2Zm-.4 7.8a3.4 3.8 0 1 0 6.8 0 3.4 3.8 0 1 0-6.8 0ZM12 1.2a4.2 4.2 0 1 0 0 8.4 4.2 4.2 0 1 0 0-8.4Zm-1.8 3a1 1 0 1 0 0 2 1 1 0 1 0 0-2Zm3.6 0a1 1 0 1 0 0 2 1 1 0 1 0 0-2Zm-2.9 2.7h2.2L12 8.8Z" /><path d="M16.5 10.8c2.3 1 3.7 3.2 3.7 5.4 0 2.2-1.6 3.4-4 3.4-1 0-1.6-.6-1.8-1.2.4-2.2.8-5.4 2.1-7.6ZM7.5 10.8c-2.3 1-3.7 3.2-3.7 5.4 0 2.2 1.6 3.4 4 3.4 1 0 1.6-.6 1.8-1.2-.4-2.2-.8-5.4-2.1-7.6Z" /></g></svg>;
  return <svg viewBox="0 0 24 24" aria-hidden="true"><rect x="4" y="4" width="16" height="6" rx="1.2" /><rect x="4" y="14" width="16" height="6" rx="1.2" /><path d="M8 7h.01M8 17h.01M12 7h5M12 17h5" /></svg>;
}

// taskFromCommandResult 从任务命令的回执里取出被创建的任务（含电脑端分配的真 id）。
//
// 形状与 conversationFromCommandResult 一样防御：本地控制服务把 handler 的响应体原样
// 转回来，可能是任务对象本身，也可能包一层 { task: ... }；认不出来就返回 null，
// 调用方退回"按内容对账"的老路。
function taskFromCommandResult(result: unknown): Task | null {
  if (!result || typeof result !== "object") return null;
  const raw = (result as { task?: unknown }).task;
  const value = raw && typeof raw === "object" ? raw : result;
  const item = value as Partial<Task> & { id?: unknown };
  if (typeof item.id !== "string" || item.id.trim() === "") return null;
  return {
    id: item.id,
    title: typeof item.title === "string" ? item.title : "",
    description: typeof item.description === "string" ? item.description : "",
    priority: typeof item.priority === "string" ? item.priority : "normal",
    status: typeof item.status === "string" ? item.status : "todo",
    updatedAt: typeof item.updatedAt === "string" ? item.updatedAt : new Date().toISOString(),
  };
}

function conversationIDFromCommandResult(result: unknown): string {
  if (!result || typeof result !== "object") return "";
  const value = result as { id?: unknown; conversation?: { id?: unknown } };
  if (typeof value.id === "string") return value.id;
  return typeof value.conversation?.id === "string" ? value.conversation.id : "";
}

function commandFailureDetail(result: unknown): string {
  if (!result || typeof result !== "object") return "";
  const error = (result as { error?: unknown }).error;
  return typeof error === "string" ? error.trim() : "";
}

function normalizeCommandState(value: unknown, fallbackCommandID = ""): CommandState {
  if (!value || typeof value !== "object") return { commandId: fallbackCommandID, status: "indeterminate" };
  const raw = value as { commandId?: unknown; status?: unknown; result?: unknown; command?: { commandId?: unknown } };
  return {
    commandId: typeof raw.commandId === "string" ? raw.commandId
      : typeof raw.command?.commandId === "string" ? raw.command.commandId
        : fallbackCommandID,
    status: typeof raw.status === "string" ? raw.status : "indeterminate",
    result: raw.result,
  };
}

function conversationFromCommandResult(result: unknown): RemoteConversation | null {
  if (!result || typeof result !== "object") return null;
  const raw = (result as { conversation?: unknown }).conversation;
  const value = raw && typeof raw === "object" ? raw : result;
  const item = value as Partial<RemoteConversation> & { id?: unknown };
  if (typeof item.id !== "string" || item.id.trim() === "") return null;
  return {
    id: item.id,
    title: typeof item.title === "string" ? item.title : "新会话",
    status: typeof item.status === "string" ? item.status : "idle",
    agentId: typeof item.agentId === "string" ? item.agentId : "",
    lastActivityAt: typeof item.lastActivityAt === "string" ? item.lastActivityAt : new Date().toISOString(),
    isCurrent: item.isCurrent !== false,
    messages: Array.isArray(item.messages) ? item.messages as Message[] : [],
  };
}

// Native builds do not have a proxy origin. Keep a production fallback so an
// APK built without a local .env can still reach the single public endpoint.
const configuredCloudURL = (import.meta.env.VITE_CLOUD_URL as string | undefined)?.replace(/\/$/, "") || "";
// In Vite development use the local /v1 proxy to avoid browser CORS. Native
// and production builds keep an absolute public URL.
const cloudURL = Capacitor.isNativePlatform()
  ? configuredCloudURL || "https://keyanjia.info:8443"
  : import.meta.env.DEV ? "" : configuredCloudURL;
const cloudRequestTimeoutMs = 15_000;

type BarcodeDetectorResult = { rawValue?: string };
type BarcodeDetectorLike = new (options?: { formats?: string[] }) => { detect(source: HTMLVideoElement): Promise<BarcodeDetectorResult[]> };

// 原生分支（`BarcodeScanner.startScan()`）把摄像头预览画在 **WebView 之下**，
// 所以"能不能看见画面"完全由 WebView 自身 + 它上层每一层 DOM 的不透明度决定。
// 这里必须连 <html> 一起清底：`style.css` 的 `:root { background: #f3f8f4 }` 给根元素
// 铺了一层不透明画布底色，只清 body / #root 压不住它 —— 症状就是"点了扫码，摄像头
// 其实已经启动，但整块画面被根元素底色挡住，看起来像没调用起来"。
// 两处挂载点必须同时改（<html> 与 <body>），少挂一处会让 `html.barcode-scanner-active`
// 那一组清底规则整组失效。
const barCodeScannerActiveClass = "barcode-scanner-active";
function setScannerActive(active: boolean) {
  document.documentElement.classList.toggle(barCodeScannerActiveClass, active);
  document.body.classList.toggle(barCodeScannerActiveClass, active);
}

function pairingFromScan(value: string): { pairingID: string; code: string } {
  try {
    const parsed = new URL(value);
    return {
      pairingID: parsed.searchParams.get("pairingId") || parsed.searchParams.get("pairing_id") || "",
      code: parsed.searchParams.get("code") || "",
    };
  } catch {
    return { pairingID: "", code: "" };
  }
}

// The QR code has to point at an absolute http(s) address that the phone can
// reach. The desktop WebView origin is a tauri:// URL, and a cloud deployment
// without MILEVIA_CLOUD_APP_URL returns a relative path — falling back to the
// local origin in either case would produce a code that no phone can open, so
// return an empty string and let the caller explain the problem instead.
function pairingURLWithCode(value: string | undefined, pairingID: string, pairingCode: string): string {
  const candidates = [(value || "").trim(), cloudURL ? `${cloudURL}/mobile` : ""];
  for (const candidate of candidates) {
    if (!candidate) continue;
    let parsed: URL;
    try {
      parsed = new URL(candidate);
    } catch {
      continue;
    }
    if (parsed.protocol !== "http:" && parsed.protocol !== "https:") continue;
    parsed.searchParams.set("pairingId", pairingID);
    // ⚠️ 二维码**必须**带上这次配对的 6 位校验码，否则「扫码配对」这条链路整条是死的
    // （2026-09-17 用户报障："扫了电脑上的二维码，手机端没有任何反应，最后还是要手输校验码"）。
    // 手机端 acceptScannedPairing() 只有读到 6 位数字才会真的去调 claimPairing()；只给
    // pairingId 的话，扫码的结果就只是"预填了一个会话句柄"，用户仍必须把电脑屏幕上的数字
    // 手打一遍 —— 二维码相对校验码不再是并列的第二种方式，而是一段装饰。
    // 泄露面：校验码 5 分钟失效、单次有效，就算随 URL 进了浏览器历史/代理日志也换不到可用
    // 令牌 —— 云端令牌记录的"激活时间"只由电脑端「确认绑定」写入，鉴权时要求它非空
    // （apps/cloud-control/internal/cloud/server.go），所以真正的授权闸门始终是电脑端那一次点击。
    // 手机扫码时只解析、从不加载这个 URL（见 pairingFromScan）。
    if (/^\d{6}$/.test(pairingCode)) parsed.searchParams.set("code", pairingCode);
    else parsed.searchParams.delete("code");
    return parsed.toString();
  }
  return "";
}

function idempotencyKey() {
  return globalThis.crypto?.randomUUID?.() || `${Date.now()}-${Math.random().toString(36).slice(2)}`;
}

// 配对流程状态行的三态（外加一条中性提示）。`state` 直接喂给 CSS 的 `data-state`，
// 颜色/spinner 判断只写在样式表里，JSX 不散写。
type PairingNoticeState = "idle" | "waiting" | "success" | "error";
type PairingNotice = { text: string; state: PairingNoticeState };

// 二维码剩余秒数。云端返回的 expiresAt 是唯一权威；解析不出来就返回 0（当作没有有效期）。
function secondsUntil(value: string | undefined): number {
  const deadline = Date.parse(value || "");
  if (!Number.isFinite(deadline)) return 0;
  return Math.max(0, Math.round((deadline - Date.now()) / 1000));
}

function countdownText(seconds: number): string {
  return `${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, "0")}`;
}

// 实例状态在云端是英文枚举（online / offline / machine_offline），直接渲染就是一颗写着
// "online" 的绿胶囊。文案在这里翻，类名仍用原始值 —— CSS 的 `.mobile-status.offline` 靠它上色。
const instanceStatusLabels: Record<string, string> = { online: "在线", offline: "离线", machine_offline: "电脑未响应" };

// 设备行上的时间：**只到分钟**。这行是概览（那一台是不是还活着），跟着秒跳没有意义。
function shortSyncTime(value: string | undefined): string {
  if (!value) return "";
  const at = new Date(value);
  if (Number.isNaN(at.getTime())) return "";
  return `${at.getMonth() + 1}/${at.getDate()} ${String(at.getHours()).padStart(2, "0")}:${String(at.getMinutes()).padStart(2, "0")}`;
}

// 「当前电脑」卡副标题上的时间：**今天只写时:分** —— 这条卡片说的是"现在这台机器"，
// 挂一个和今天同一天的日期只是白占宽度（昨天以前才需要写月/日）。
function syncClockText(value: string | undefined): string {
  if (!value) return "";
  const at = new Date(value);
  if (Number.isNaN(at.getTime())) return "";
  const clock = `${String(at.getHours()).padStart(2, "0")}:${String(at.getMinutes()).padStart(2, "0")}`;
  const now = new Date();
  const sameDay = at.getFullYear() === now.getFullYear() && at.getMonth() === now.getMonth() && at.getDate() === now.getDate();
  return sameDay ? clock : `${at.getMonth() + 1}/${at.getDate()} ${clock}`;
}

// 「当前电脑」卡的副标题 = 这台电脑最后一次同步的时间。
// 旧版写「事件序号 7 · 2026/9/17 16:01:45」：事件序号是排障用的内部量，用户读不懂；
// 带秒的时间占掉大半行宽。项目数也**不在这里** —— 它在下面「选择项目」标题右侧，
// 同屏出现两次同一个数字只会让人以为是两回事。
function deviceSyncText(lastSeenAt: string | undefined): string {
  const seen = syncClockText(lastSeenAt);
  return seen ? `${seen} 同步` : "尚未连接";
}

// 多电脑列表里每一行的副标题。**三种真相要分开**（同"空列表"那条规则）：
// 真的离线 / 绑定已失效（被另一台手机顶掉）/ 还没探测过（状态未知）。
function deviceStatusText(device: MobileDevice): string {
  if (device.revoked) return "绑定已失效，需要重新扫码配对";
  const seen = shortSyncTime(device.lastSeenAt);
  if (device.status === "online") return seen ? `在线 · ${seen} 同步` : "在线";
  if (device.status === "offline") return seen ? `离线 · 上次 ${seen}` : "离线";
  return seen ? `上次 ${seen}` : "状态未知";
}
function instanceStatusLabel(status: string): string {
  return instanceStatusLabels[status] || status || "未知";
}

// 云端的错误原因是这里最准确的信号，但它以英文短语返回，而且不能靠状态码推断
// 场景——409 既可能是配对码冲突，也可能是"电脑当前离线"。因此按原因做定向翻译，
// 认不出的原因原样透出，避免给出与实际场景不符的提示。
/**
 * 会话里被反引号包起来的**项目内文件路径** → 可点的入口。
 *
 * 手机上要看文件，九成是为了看 AI 刚改了什么；而 AI 引用文件时最常说 `src/main.ts`。
 * 没有这个入口，用户得从会话退出去、进文件视图、再一层层点进去。
 *
 * 只接管**行内** code：块级代码走 `pre`（`markdownCodeComponents` 里那个带复制按钮的），
 * 它的内层 `<code>` 带 `language-*` 类名，这里必须原样放行 —— 否则一整块代码会被当成
 * 一个路径，点下去必然报"文件不存在"。
 *
 * 判据在 `lib/project-path.ts`，那边有独立用例。**误判比漏判糟得多**：漏了用户还能自己找，
 * 误了就多一个点下去报错的按钮。
 */
function MobileInlineCode({ className, children, onOpenFile }: { className?: string; children?: ReactNode; onOpenFile: (path: string) => void }) {
  const text = typeof children === "string" ? children : "";
  const isBlock = typeof className === "string" && className.includes("language-");
  const reference = isBlock ? null : projectFileReference(text);
  if (!reference) return <code className={className}>{children}</code>;
  return <button type="button" className="mobile-message-file-link" title={`打开 ${reference.path}${reference.line ? `:${reference.line}` : ""}`} onClick={() => onOpenFile(reference.path)}>{children}<span aria-hidden="true">↗</span></button>;
}

function localizeCloudError(raw: string, status: number): string {
  const text = raw.toLowerCase();
  if (text.includes("instance_offline")) return "电脑当前离线，请等电脑上线后再试";
  if (text.includes("pairing is not ready for confirmation")) return "手机还没有提交校验码，请先扫码并输入校验码";
  if (text.includes("too many pairing attempts")) return "配对尝试次数过多，请让电脑重新生成二维码";
  if (text.includes("pairing code is ambiguous")) return "校验码发生冲突，请让电脑重新生成二维码";
  if (text.includes("pairing code has expired or was already used")) return "配对码已过期或已被使用，请让电脑重新生成";
  if (text.includes("pairing code is invalid")) return "配对码无效或已过期，请让电脑重新生成";
  if (text.includes("pairing session not found")) return "配对会话不存在，请让电脑重新生成二维码";
  if (text.includes("invalid user token")) return "云端令牌已失效，请重新配对";
  if (text.includes("instance access denied")) return "当前令牌没有访问权限";
  if (text.includes("instance not found")) return "找不到已配对的电脑";
  if (text.includes("too many requests")) return "请求过于频繁，请稍后再试";
  if (text.includes("idempotency key conflicts")) return "该操作与之前的请求冲突，请稍后重试";
  if (text.includes("unsupported command type")) return "当前版本不支持该操作";
  if (text.includes("payload must be valid json")) return "操作内容格式不正确或超过大小限制";
  return raw || `请求失败 (${status})`;
}

// deviceToken 用来代表"某一台**非当前**的电脑"发请求（切换面板里的在线状态就是逐台探测
// 出来的）。不传就是当前设备 —— 13 处业务调用都属于后者，不需要知道多设备的存在。
async function cloud<T>(path: string, init?: RequestInit, deviceToken?: string): Promise<T> {
  const headers = new Headers(init?.headers);
  headers.set("Content-Type", "application/json");
  // 这次请求代表"当前选中的那台电脑"。手机端要连多台，全靠这里只读一个设备表 ——
  // 13 处业务调用都不需要知道现在连的是哪台。
  const token = deviceToken ?? readActiveToken();
  if (token) headers.set("Authorization", `Bearer ${token}`);
  const controller = new AbortController();
  const timeout = globalThis.setTimeout(() => controller.abort(), cloudRequestTimeoutMs);
  const abort = () => controller.abort();
  if (init?.signal?.aborted) controller.abort();
  else init?.signal?.addEventListener("abort", abort, { once: true });
  let response: Response;
  try {
    response = await fetch(`${cloudURL}${path}`, { ...init, headers, signal: controller.signal });
    const body = await response.json().catch(() => null);
    if (!response.ok) {
      if (response.status === 401) {
        // The stored token is unusable: expired, revoked from the desktop, or
        // never activated because the pairing was not confirmed. Keeping it
        // would make every later request fail the same way, so drop it and let
        // the page fall back to the pairing flow.
        // **不能把记录删掉**：被另一台手机顶替（云端确认新手机时会 revoke 旧令牌）时，
        // 这条记录是界面唯一能解释"为什么突然连不上"的现场 —— 删了用户只会看到配对凭空消失。
        if (token) {
          markDeviceRevoked(token);
          // 只有"当前这台"被拒时才把整页拉回配对流程：探测其它电脑时收到 401
          // 只该让那一行标成失效，不该把用户正在用的电脑也切掉。
          if (token === readActiveToken()) globalThis.dispatchEvent(new Event("milevia:token-cleared"));
        }
        throw new Error("云端令牌已失效或被撤销，请重新配对");
      }
      const rawError = body && typeof body.error === "string" ? body.error : "";
      throw new Error(localizeCloudError(rawError, response.status));
    }
    if (body === null || body === undefined) throw new Error("云端返回格式无效，请稍后重试");
    return body as T;
  } catch (cause) {
    if (controller.signal.aborted && !init?.signal?.aborted) {
      throw new Error("云端请求超时，请检查手机网络后重试");
    }
    throw cause;
  } finally {
    globalThis.clearTimeout(timeout);
    init?.signal?.removeEventListener("abort", abort);
  }
}

// 服务端每 15 秒往这条流里写一条 `: keepalive` 注释（见 cloud-control 的 mobileEventStream）。
// 客户端必须**自己**盯着"还有没有字节进来"，不能只靠 read() 报错：
// 半开连接（手机换网、NAT 表超时、系统把 socket 悄悄收走）在本地看起来一切正常 ——
// read() 一直挂着，既没有事件也没有错误，于是上面那个 1s→30s 的退避重连**永远轮不到**，
// 实时通道就此静默，直到用户重开 App。这正是文档里记过的"agent 卡死、重连循环再也没跑过"
// 在客户端的镜像（那次修的是服务端）。
//
// 判定取 45 秒 = 连着三次没收到 keepalive。误判的代价只是重连一次（很便宜，
// 而且带 Last-Event-ID 从断点续传），漏判的代价是实时通道一直不恢复。
const streamIdleTimeoutMs = 45_000;

async function consumeMobileEventStream(url: string, token: string, lastEventID: string, onMessage: (event: MessageEvent) => void, signal: AbortSignal) {
  const headers = new Headers({ Accept: "text/event-stream", Authorization: `Bearer ${token}` });
  if (lastEventID) headers.set("Last-Event-ID", lastEventID);
  // 用**内部**的 controller，不直接 abort 调用方那个 signal：调用方的 signal 表示
  // "这次订阅该结束了"（卸载 / 换电脑），内部的表示"这条连接已经不新鲜了、该换一条"。
  // 混在一起的话上层分不清是"该退出"还是"该重连"，会把一次超时当成卸载而不再重连。
  const controller = new AbortController();
  const abortByCaller = () => controller.abort();
  if (signal.aborted) controller.abort();
  else signal.addEventListener("abort", abortByCaller, { once: true });
  let idleTimer: number | undefined;
  const armIdleWatchdog = () => {
    if (idleTimer !== undefined) window.clearTimeout(idleTimer);
    idleTimer = window.setTimeout(() => controller.abort(), streamIdleTimeoutMs);
  };
  try {
    const response = await fetch(url, { headers, signal: controller.signal });
    if (!response.ok) throw new Error(`stream request failed (${response.status})`);
    if (!response.body) throw new Error("stream response has no body");
    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    let buffer = "";
    let eventID = "";
    let data: string[] = [];
    const dispatch = () => {
      if (data.length > 0) onMessage(new MessageEvent("message", { data: data.join("\n"), lastEventId: eventID }));
      data = [];
      eventID = "";
    };
    const processLine = (line: string) => {
      if (line === "") { dispatch(); return; }
      if (line.startsWith(":")) return;
      const separator = line.indexOf(":");
      const field = separator < 0 ? line : line.slice(0, separator);
      const value = separator < 0 ? "" : line.slice(separator + 1).replace(/^ /, "");
      if (field === "id") eventID = value;
      if (field === "data") data.push(value);
    };
    armIdleWatchdog();
    while (!controller.signal.aborted) {
      const next = await reader.read();
      // 每读到一段字节就重新起表：注释行（keepalive）也算，它本来就只为"我还活着"而存在。
      armIdleWatchdog();
      if (next.done) {
        buffer += decoder.decode();
        if (buffer) processLine(buffer);
        break;
      }
      buffer += decoder.decode(next.value, { stream: true });
      const lines = buffer.split(/\r?\n/);
      buffer = lines.pop() || "";
      for (const line of lines) processLine(line);
    }
    dispatch();
  } finally {
    if (idleTimer !== undefined) window.clearTimeout(idleTimer);
    signal.removeEventListener("abort", abortByCaller);
  }
}

/** 「选择项目」里的一张卡片。浮起的那张复用同一份标记 —— 两处各写一遍迟早会长歪。 */
function MobileProjectChoice({ item, busy, open, isDragSlot = false, ghost = false }: {
  item: Project;
  busy: boolean;
  open: (project: Project) => void;
  /** 正在被拖走：原位渲染成虚线占位槽。 */
  isDragSlot?: boolean;
  /** 作为「浮起的卡片」渲染：不接收指针、不进 Tab 序、也不参与拖拽命中。 */
  ghost?: boolean;
}) {
  const environment = projectEnvironment(item);
  const running = item.running === true;
  const environmentLabel = projectEnvironmentLabel(environment);
  const branch = item.gitBranch || "默认分支";
  return <button type="button" className={`mobile-project-choice${isDragSlot ? " is-drag-slot" : ""}`} data-card-id={ghost ? undefined : item.id} aria-hidden={ghost || undefined} tabIndex={ghost ? -1 : undefined} onClick={ghost ? undefined : () => void open(item)} disabled={ghost ? undefined : busy}><span className={`mobile-project-mark ${environment}`} aria-hidden="true"><ProjectEnvironmentIcon environment={environment} /></span><span className="mobile-project-choice-content"><span className="mobile-project-choice-head"><strong title={item.name}>{item.name}</strong><span className="mobile-project-running" data-state={running ? "running" : "idle"}><i></i>{running ? "运行中" : "未运行"}</span></span><small className="mobile-project-choice-meta" title={`${environmentLabel} · ${branch}`}>{`${environmentLabel} · ${branch} · ${item.tasks.length} 个任务 · ${item.conversations?.length || 0} 个会话`}</small></span><span className="mobile-project-choice-chevron" aria-hidden="true">›</span></button>;
}

export default function MobileRemotePage() {
  const navigate = useNavigate();
  // Capacitor 原生 WebView 可能同时注入桌面运行时对象；原生包始终
  // 使用移动端项目选择/对话布局，避免被桌面分支误判。
  const mobileApp = Capacitor.isNativePlatform() || !isDesktop();
  // 手机连过哪些电脑。列表与"当前"都归 mobile-devices 管（含旧单令牌的迁移），
  // 组件这里只是它的一份镜像，用于渲染切换面板。
  const [devices, setDevices] = useState<MobileDevice[]>(() => reconcileDevices());
  const [token, setToken] = useState(() => readActiveToken());
  const [mobileUpdate, setMobileUpdate] = useState<MobileUpdateState | null>(null);
  const [updateDismissed, setUpdateDismissed] = useState(false);
  const [instances, setInstances] = useState<Instance[]>([]);
  const [instanceID, setInstanceID] = useState("");
  const [snapshot, setSnapshot] = useState<Snapshot | null>(null);
  const [selectedProject, setSelectedProject] = useState("");
  const [selectedConversation, setSelectedConversation] = useState("");
  // 会话历史改成按钮 + 弹窗（原来是下拉框：长标题会把选项撑得非常高）。
  const [conversationHistoryOpen, setConversationHistoryOpen] = useState(false);
  const [newConversationProject, setNewConversationProject] = useState<Project | null>(null);
  const [newConversationAgent, setNewConversationAgent] = useState<"claude-code" | "codex">("claude-code");
  const [mobileView, setMobileView] = useState<"projects" | "conversation">("projects");
  // 手机上的"侧滑返回"（Chrome/WebView 的返回手势、iOS 边缘手势）本质是**历史后退**。
  // 只在 React state 里切视图时历史里没有这一层，手势就什么也不做——所以进项目要真的压一层历史。
  // 这个 ref 记录"我们压过一层、还没退掉"：保证只压一次（HTTP 响应与 SSE 事件可能都触发进入），
  // 并且退出时把它退干净，否则历史里留一个空层，用户下一次返回会被白吞。
  const conversationHistoryRef = useRef(false);
  // 会话视图的**子态**：文件 / Git 工作台。它是一个槽位，不是两个布尔 ——
  // 它们都不是独立视图、不另压历史（理由见 `openFiles`），而且返回键链与
  // leaveConversationView 是"两处都要改"的结构：漏一处就会出现"点错也能执行"。
  // 收成一个槽位之后，"有哪些子态"只有一个真相来源，也不可能两个同时开着。
  const [sessionSub, setSessionSub] = useState<null | "files" | "git">(null);
  const filesOpen = sessionSub === "files";
  const gitOpen = sessionSub === "git";
  // 从会话里点某个文件路径进来时要自动打开的那个文件（相对项目根）。
  // 面板消费掉之后必须清空，否则下次再进文件视图又会自动打开同一个文件。
  const [filesInitialPath, setFilesInitialPath] = useState<string | null>(null);
  // 换一个值 = 把文件面板整个重建（清空标签、重取目录树）。手机端「刷新」的语义就是
  // "重新对账"，缓存里那份内容已经不可信了。
  const [filesEpoch, setFilesEpoch] = useState(0);
  const filesPanelRef = useRef<FilesPanelHandle | null>(null);
  // Git 视图登记的"里面还有没有上一层"（diff / 冲突解决）。与文件面板的 showTree 同构：
  // 外层猜一个布尔量再回传必然分叉，所以由面板自己回答。
  const gitPanelRef = useRef<MobileGitPanelHandle | null>(null);
  // 面板登记的"放弃未保存的更改？"守卫。刷新会丢掉正在编辑的内容，
  // 必须先问一声 —— 复用面板与桌面端同一条确认流程，不另造一个提示。
  const filesGuardRef = useRef<NavigationGuard | null>(null);
  const [tasksOpen, setTasksOpen] = useState(false);
  // 会话视图的顶栏只有「返回 + 会话名 + ⋯」这一行：刷新、历史会话、新会话、任务队列都收进 ⋯ 菜单。
  // 原来它们各占一行（标题条 + 一行三个按钮），实测吃掉 199px —— 接近 390×800 屏幕的四分之一。
  const [headerMenuOpen, setHeaderMenuOpen] = useState(false);
  // 任务队列是个整页模态层：打开时焦点要搬进面板、关闭时还给触发它的那颗按钮（见下面的焦点 effect）。
  const taskPanelRef = useRef<HTMLElement | null>(null);
  // ⋯ 菜单按钮：既是弹出层的锚点，也是任务面板关闭后焦点的落点（原来是「任务」按钮，现在它就是
  // 菜单里的「任务队列」那一项，关掉面板时它已经不在 DOM 里了）。
  const headerMenuButtonRef = useRef<HTMLButtonElement | null>(null);
  // 底部「我的电脑」面板同样需要两个落点：面板本身（打开时把焦点搬进去）与卡片上那颗「切换 / 管理」
  // 按钮（关闭时还回来）。少了它，键盘 / 读屏用户打开面板后仍在背后那屏里打转。
  const devicesSheetRef = useRef<HTMLElement | null>(null);
  const deviceSwitchButtonRef = useRef<HTMLButtonElement | null>(null);
  // 消息动作图标行（2026-09-20）要两个落点：图标行本身（展开时把焦点交给第一颗可用图标），
  // 以及**触发它的那颗「⋯」**（收起时焦点要还回去）。
  // 这颗「⋯」每条气泡各有一个，所以不能像设备面板那样用一个固定 ref 兜底 ——
  // 展开时把 event.currentTarget 记下来。
  const messageActionsRef = useRef<HTMLDivElement | null>(null);
  const messageActionTriggerRef = useRef<HTMLButtonElement | null>(null);
  // 复制反馈的复位计时器（1.6s，与代码块复制键同一档）。卸载时要清掉，
  // 否则流式消息重排、面板关闭之后还可能排一个没人收的 timer。
  const messageCopyTimerRef = useRef<number | null>(null);
  // 入场动画的两块账（见下面那个 effect）：已见过的条目 key，以及"当前这一批属于哪条会话"。
  // 用 ref 不用 state：它们只在 effect 内读写，进 state 只会多出无意义的重渲染。
  const seenTimelineKeysRef = useRef<Set<string> | null>(null);
  const arriveScopeRef = useRef("");
  // 上一批的 key（**有序**：要认出"同一个位置被顶替"这件事，光有集合不够）。
  const previousTimelineKeysRef = useRef<string[]>([]);
  // 默认落在「待处理」：没有「全部」之后，面板打开时必须已经站在某一档上（否则"没有一颗高亮"和
  // "列表里是全量"会互相矛盾）。待处理是队列里最该动手的一档；队列整体为空时由空态兜底。
  const [taskFilter, setTaskFilter] = useState<TaskFilter>("todo");
  // 面板里的搜索词（2026-09-18 按用户要求加，位置在分类栏**上面**）：语义是"**在当前这一档里**再收一次"
  // —— 分类是主筛选，搜索是在它里面找。所以搜索不是第二条平行的筛选，而是先收窄、再过分类：
  // 胶囊计数与列表都从"搜索命中的那批"出发。⚠️ 计数若还按全量算，就会出现"胶囊写着 6、点进去是空的"，
  // 那正是当初去掉「全部」要避免的"列表与分类对不上"（见本文件 taskFilters 上面那段）。
  const [taskQuery, setTaskQuery] = useState("");
  // 卡片上「详情」展开的是哪一条（同时只开一条，避免一屏里多段长描述把列表撑散）。
  // 描述在卡片上只给一行预览，完整描述与完整更新时间仍然找得到，不是被删掉了。
  const [expandedTask, setExpandedTask] = useState("");
  // 「新建任务」弹层。创建入口**只有**面板右下角那颗加号：以前表单常驻列表底部，
  // 每个分类页底部都挂着一张两百多像素的表单，滚到底才看得见，还顺手吃掉了列表的可见高度。
  const [creatingTask, setCreatingTask] = useState(false);
  // 只属于创建弹层的失败原因。页面级那条 `.mobile-error` 是**在弹层遮罩之下**的，
  // 创建失败时用户看不到它 —— 弹层会停在原地、却看不出为什么。所以这里单独留一份，
  // 由弹层自己渲染出来（`.mobile-task-modal-error`）。
  const [createError, setCreateError] = useState("");
  // 新建任务时选的优先级。之前这一格根本没有入口、payload 里写死 "normal"：用户在手机上
  // 建不出「紧急」的任务，只能先建好、再进编辑弹层改一次 —— 而编辑弹层里那一格又是
  // 原生 select（见 MobilePriorityPicker）。两处一起补成一个可就地选择的分段选择器。
  const [createPriority, setCreatePriority] = useState("normal");
  const [messageDraft, setMessageDraft] = useState("");
  // 已引用、尚未发送的技能。点技能**不再**把「技能名 + 描述」整段塞进输入框：技能描述动辄上百字，
  // 手机上会直接铺满整屏，而 setMessageDraft 是整体覆盖 —— 用户写到一半的草稿会被无声吃掉。
  // 现在记成一颗可删除的胶囊，发送那一刻才由 composeSkillMessage 展开成完整引用指令。
  const [skillRefs, setSkillRefs] = useState<RemoteSkill[]>([]);
  // 已引用、尚未发送的那条消息（见 MessageQuote）。与技能引用同一条生命周期规则：
  // 发送时与草稿一起乐观清空、失败时一起还回来、换会话/换项目时一起清掉。
  const [quoteRef, setQuoteRef] = useState<MessageQuote | null>(null);
  // 正在展开图标行的**那条消息的 id**（空串＝没有，与 expandedTask 同一个写法）。
  // 记 id 而不是 Message 对象：流式消息的正文每一帧都在变，记住对象就会把"展开那一刻的旧正文"
  // 带进后续动作 —— 上一版那张面板正是为此要在动作执行前再查一次最新内容，现在这个坑从根上没了。
  const [messageActionId, setMessageActionId] = useState("");
  // 复制那一颗图标的就地反馈。图标行不是浮层，反馈只能靠图标本身变（方块 → 勾）：
  // 页面级那条 `.mobile-error` 距离太远，而"动作做完就收"再加一层 toast 只是多一套跨层状态。
  const [messageCopyState, setMessageCopyState] = useState<"idle" | "copied" | "failed">("idle");
  // 「这一批是刚到的」——入场动画只对它们跑（2026-09-20 动效）。
  // 为什么必须有这个状态，而不是在列表容器上挂一个"就绪"开关：CSS 入场动画的触发条件是
  // "元素**开始匹配**一条带 animation 的规则"。容器开关从"不匹配"切到"匹配"时，
  // **已经在 DOM 里的那批条目会一起重放**（切换会话 = 整屏闪一次）；
  // 而逐条挂在条目自己身上时，只有新挂载的那条会匹配，才是"新到的才动画"。
  // 两种写法在静态截图里长得一样，只有真点一次才分得出来 —— 探针里有对应用例。
  const [arrivingKeys, setArrivingKeys] = useState<string[]>([]);
  // 输入条左侧「＋」弹出的工具面板。**内容与电脑端左侧快捷栏同源**：常用提示词 / 常用命令 /
  // 技能三组来自快照（电脑端数据库 + 文件系统扫描），会话入口与输入两组是手机端独有的本地动作。
  const [composerToolsOpen, setComposerToolsOpen] = useState(false);
  // 正在执行的那条快捷方式的 id：点过之后按钮显示"发送中"并挡住重复点击（与桌面端 shortcutBusy 同义）。
  // 走的是异步命令通道，一次往返要等电脑端接单 + 回执，没有这个状态用户会以为没点上而连点。
  const [shortcutBusy, setShortcutBusy] = useState("");
  // defaultAction=confirm 的快捷方式要先弹确认框：它在电脑端执行，手机上误触的代价不在这一屏。
  const [confirmShortcut, setConfirmShortcut] = useState<RemoteShortcut | null>(null);
  const composerShellRef = useRef<HTMLDivElement | null>(null);
  // 会话视图顶栏是 sticky 的（见 mobile-remote.css「会话视图顶栏冻结」）。刷新状态条要钉在它
  // 正下方，因此需要它的实测高度 —— 拿 ref 而不是 querySelector，免得和取景层那些同名节点抢。
  const headerRef = useRef<HTMLElement | null>(null);
  const [title, setTitle] = useState("");
  const [description, setDescription] = useState("");
  const [editingTask, setEditingTask] = useState<Task | null>(null);
  const [editTitle, setEditTitle] = useState("");
  const [editDescription, setEditDescription] = useState("");
  const [editPriority, setEditPriority] = useState("normal");
  // 编辑弹层自己的失败原因，与 `createError` 同构、同样由弹层渲染（`.mobile-task-modal-error`）。
  // 原来这里写的是页面级的 `setError` —— 而那一条在遮罩**之下**，弹层不关就永远看不到，
  // 于是"任务描述为空"在编辑路径上表现为"点了保存、什么都没发生"。
  const [editError, setEditError] = useState("");
  const [deletingTask, setDeletingTask] = useState<Task | null>(null);
  const [busy, setBusy] = useState(false);
  // 手动刷新的可见反馈。用户点了刷新之后必须马上看到"在转"、结束后立刻看到结果与时刻，
  // 否则界面完全没有变化，只能靠猜（刷新按钮原先就是这个问题）。
  const [refreshing, setRefreshing] = useState(false);
  const [refreshStatus, setRefreshStatus] = useState<RefreshStatus | null>(null);
  const refreshingRef = useRef(false);
  const [processingConversations, setProcessingConversations] = useState<ProcessingConversations>({});
  const [error, setError] = useState("");
  // 通知权限的初值。
  // ⚠️ 原生包里**没有通知通道**：`capacitor.plugins.json` 只注册了扫码与 App 两个插件，
  //    `AndroidManifest.xml` 也没声明 `POST_NOTIFICATIONS`，而 Android WebView 不会把
  //    Web Notification 接到系统通知栏 —— 所以这里在原生平台直接判 `unsupported`，
  //    把那颗"点了永远拿不到权限"的「开启通知」收起来，而不是在顶栏留一颗哑按钮
  //    （2026-09-20 可用性排查的结论）。
  //    手机浏览器里 Web Notification 是真能用的，所以只在原生平台短路。
  //    将来给原生包接上本地通知（`@capacitor/local-notifications` + manifest 权限声明）之后，
  //    把 `Capacitor.isNativePlatform()` 这一项去掉就能恢复入口。
  const [notificationPermission, setNotificationPermission] = useState<NotificationPermission | "unsupported">(() => Capacitor.isNativePlatform() || typeof Notification === "undefined" ? "unsupported" : Notification.permission);
  const [pairingCode, setPairingCode] = useState(() => new URLSearchParams(location.search).get("code") || "");
  const [manualPairingCode, setManualPairingCode] = useState("");
  const [pairingID, setPairingID] = useState(() => new URLSearchParams(location.search).get("pairingId") || new URLSearchParams(location.search).get("pairing_id") || "");
  // 配对流程的状态行。**必须带状态分级**，不能是一句裸字符串：等待电脑确认、成功、失败
  // 三种情况的视觉完全一样时，这行 12px 小字会被整条忽略 —— 上一轮用户就是这么报的
  // "扫了码手机端没有任何反应"（当时它其实显示了"请手动输入校验码"）。
  // 同时它**不能挂在配对面板里面**：绑定成功后 `showMobilePairing` 立刻变 false，
  // 面板连同里面那句"电脑已确认，绑定完成"一起卸载，成功的确认从来没被看见过。
  const [pairingNotice, setPairingNotice] = useState<PairingNotice | null>(null);
  // 二维码有效期。云端返回的 expiresAt 是唯一权威，本地只把它显示成倒计时并据此判"已失效"。
  const [pairingExpiresAt, setPairingExpiresAt] = useState("");
  const [pairingSecondsLeft, setPairingSecondsLeft] = useState(0);
  const [pairingCopied, setPairingCopied] = useState(false);
  const [pairingURL, setPairingURL] = useState("");
  const [pairingQR, setPairingQR] = useState("");
  const [pairingExpanded, setPairingExpanded] = useState(false);
  const [pairingReadyForConfirm, setPairingReadyForConfirm] = useState(false);
  const [pairingConfirmed, setPairingConfirmed] = useState(false);
  // 「解除绑定」会吊销令牌、不可逆，必须先过一道确认。多电脑之后它必须指明**对象**
  // （哪一台），所以存的是设备令牌而不是一个 boolean。
  const [confirmUnbind, setConfirmUnbind] = useState("");
  // 项目页顶部「当前电脑」条的底部切换面板。
  const [devicesOpen, setDevicesOpen] = useState(false);
  // 正在改备注的那台电脑（存**令牌**，空串＝没有）。与 `confirmUnbind` 同一个写法：
  // 面板里每台各有一颗「备注」，存 boolean 就分不清改的是哪一台。
  const [aliasTarget, setAliasTarget] = useState("");
  // 备注输入框的草稿。**只在打开弹层那一刻灌入**，不在 devices 刷新时回灌 ——
  // 其它设备的在线状态每 30 秒探测一次并整表写回，回灌会把用户打到一半的字冲掉。
  const [aliasDraft, setAliasDraft] = useState("");
  // 桌面端远程服务状态：Agent 未注册时云端根本没有这台电脑，二维码无从生成。
  const [agentStatus, setAgentStatus] = useState<{ ready: boolean; instanceId: string; cloudUrl: string; heartbeatAt: string } | null>(null);
  // 服务状态本身也是"三态"：还没读回来 / 读回来了 / 读不到。用 null 一个值表达不了后两者。
  const [agentStatusState, setAgentStatusState] = useState<"loading" | "loaded" | "failed">("loading");
  // 心跳的年龄（毫秒）。单独存状态而不是渲染期读 Date.now()：读数会过期，
  // 必须有东西让它自己重算 —— 否则 Agent 停了之后页面会永远停在"运行中"。
  const [heartbeatAgeMs, setHeartbeatAgeMs] = useState<number | null>(null);
  // 「重新注册」把主栏从配对切回注册引导。表单**只有一处**（在主栏里），侧栏只给入口。
  const [reenrollOpen, setReenrollOpen] = useState(false);
  // 这台电脑当前绑定的手机（桌面端才有）。`bindingsReady=false` 表示"读不到"，
  // 此时**不能**渲染成"没有手机绑定" —— 那是"空列表三种真相"里的老坑。
  const [bindings, setBindings] = useState<DesktopBinding[]>([]);
  // 绑定信息有四态，别退化成一个布尔：loading / loaded（含空列表）/ unregistered（云端还没这台
  // 电脑）/ failed（请求失败）。后两者都要**明确说出来**，不能静默不渲染。
  const [bindingsState, setBindingsState] = useState<"loading" | "loaded" | "unregistered" | "failed">("loading");
  // 手机「最近同步」的年龄，**必须自己随时间重算**：读数只会在轮询回来时才变，
  // 只读一次 Date.now() 的话，手机从后台被杀掉之后这张卡会永远停在"在线"上 ——
  // 和"远程服务"当初那屏静止的假绿是同一个坑（阈值见 desktop-phone.ts）。
  const [phoneSyncAgeMs, setPhoneSyncAgeMs] = useState<number | null>(null);
  // 最近一次**轮询**重读绑定信息失败了（读数还在，只是不新了）。
  // 它和 `bindingsState === "failed"` 是两件事：那个是"手上什么都没有"，这个是
  // "手上这份可能过期"。合成一个就会出现"一次超时把绑定卡整块清掉、10 秒后又长回来"。
  const [bindingsStale, setBindingsStale] = useState(false);
  // 是否已经成功读过一次绑定信息。用来把"挂载时那两次调用"里的**第二次**降级成静默重读
  // （见下面那个 effect）：只有手上什么都没有时，才允许把界面打回加载态/失败态。
  const bindingsLoadedOnceRef = useRef(false);
  // 请求代际（见 `loadBindings`）：晚发的请求赢，旧请求的返回值一律丢掉。
  const bindingsRequestGenerationRef = useRef(0);
  // 轮询**防叠加**：上一轮还没回来就跳过这一轮。
  // 本地控制服务只有一个 SQLite 连接，而 `api()` 对 GET 带两级重试 —— 一次"卡住"的轮询
  // 可能占 45 秒、发 3 个请求。没有这道闸，10 秒的间隔会在服务变慢时把它压出四五个
  // 在飞的请求，而服务本来就慢 ⇒ 越等越慢（项目里"outbox 热循环"那次就是这个形状）。
  // 只拦轮询：用户点「刷新状态」必须总能真的发出去。
  const bindingsPollInFlightRef = useRef(false);
  const [agentEnrollToken, setAgentEnrollToken] = useState("");
  const [agentEnrollBusy, setAgentEnrollBusy] = useState(false);
  const [agentEnrollMessage, setAgentEnrollMessage] = useState("");
  const [agentEnrollWaiting, setAgentEnrollWaiting] = useState(false);
  const [scanning, setScanning] = useState(false);
  const [scanError, setScanError] = useState("");
  // 权限被"永久拒绝"（用户选了不再询问）时系统不会再弹窗，只能去系统设置里放开；
  // 这时给一条可点的入口，否则用户只能对着"请在系统设置中允许摄像头"猜路径。
  const [scanNeedsSettings, setScanNeedsSettings] = useState(false);
  // 手电筒是可选能力：设备没有闪光灯 / 拿不到可用性时报 false，那颗按钮就永远不渲染，
  // 不让"没有闪光灯"变成一条错误提示。
  const [scanTorchAvailable, setScanTorchAvailable] = useState(false);
  const [scanTorchOn, setScanTorchOn] = useState(false);
  const [scanVideo, setScanVideo] = useState<HTMLVideoElement | null>(null);
  const nativeScanListener = useRef<PluginListenerHandle | null>(null);
  const messageInputRef = useRef<HTMLTextAreaElement | null>(null);
  const manualCodeRef = useRef<HTMLInputElement | null>(null);
  // 配对页（整页浮层）的焦点管理用：容器自己（tabIndex=-1 的程序化聚焦目标）+ 打开它的那颗按钮。
  const pairingPageRef = useRef<HTMLElement | null>(null);
  const pairingTriggerRef = useRef<HTMLElement | null>(null);
  const selectedConversationRef = useRef("");
  // 刚刚发生过的「本地临时会话 id → 电脑端真 id」替换。换 id 不是换会话：用户还停在同一条
  // 会话上，下面那个复位 effect（清草稿 / 清技能胶囊 / 收面板）必须认出来并跳过，
  // 否则「新建会话之后立刻打字」会被一条后台回执无声地清掉。
  const conversationIDSwapRef = useRef<{ projectId: string; from: string; to: string } | null>(null);
  const pendingMessageRef = useRef(new Map<string, Map<string, PendingMessage>>());
  // 还没被电脑端确认的任务增删改（见 PendingTaskMutation）。放在 ref 里是因为它会被
  // 「命令回调」和「快照落地」两条异步链路读写；配套的 revision 只用来触发重渲染。
  const pendingTaskRef = useRef(new Map<string, PendingTaskMutation>());
  const [pendingTaskRevision, setPendingTaskRevision] = useState(0);
  const markPendingTasksChanged = useCallback(() => setPendingTaskRevision((current) => current + 1), []);
  // 还没被电脑端分配真 id 的新建会话（见 lib/conversation-mutations.ts）。和待确认任务同
  // 一套写法：真实数据在 ref 里（两条异步链路都读写它），revision 只负责触发重渲染。
  const pendingConversationsRef = useRef(new Map<string, PendingConversation>());
  const [pendingConversationRevision, setPendingConversationRevision] = useState(0);
  const markPendingConversationsChanged = useCallback(() => setPendingConversationRevision((current) => current + 1), []);
  // 用户在"还没建成的会话"里发出的消息。真 id 一到就按原样的 clientRequestId 补发 ——
  // 有了它，新建会话之后可以立刻打字发送，不必等那一次往返。
  // 存 `draft` 与 `content` 两份：失败时要还回去的是用户自己写的正文，不是拼上技能描述的那一段。
  const deferredMessagesRef = useRef(new Map<string, Array<PendingMessage & { draft: string; quote: MessageQuote | null }>>());
  // 每一张卡片自己的"处理中"状态：老实现是一个全局 busy 把整屏按钮一起按住，
  // 用户在等一条命令时连别的任务都动不了。
  const [pendingTaskBusy, setPendingTaskBusy] = useState<Set<string>>(() => new Set());
  // revision 记录这条实时消息到达时的快照版本。快照一旦前进到更新的版本，
  // 就说明云端已经反映了那个时刻的状态（消息仍在，或者已被删除），此时不能再
  // 把实时消息当作"快照还没包含"补回去，否则电脑端删掉的消息会在手机上复活。
  const realtimeMessagesRef = useRef(new Map<string, { conversationId: string; message: Message; revision: number }>());
  // 流式增量：按 CLI 的 messageId 累积尚未落地为正式消息的助手文本。正式
  // assistant.message 到达时用同一事件里的 agentMessageId 认领并替换占位内容，
  // 因此这里不参与快照对账，下次快照整体覆盖时会自然清掉残留。
  const streamingMessagesRef = useRef(new Map<string, { conversationId: string; content: string; createdAt: string }>());
  // 实时收到、但服务端快照还没回放的运行记录（重试/压缩/失败）。快照是权威历史，
  // 但它有上传节流：不在这里留一份，刚冒出来的"API 重试中"会被下一次快照抹掉。
  // 每条记录一旦出现在快照里就从这里移除，所以它不会无限增长。
  const pendingNoticesRef = useRef(new Map<string, RemoteNotice[]>());
  const instancesRequestGenerationRef = useRef(0);
  const snapshotRevisionRef = useRef(-1);
  const snapshotLoadGenerationRef = useRef(0);
  const scanAccepted = useRef(false);
  const initialPairing = useRef({
    pairingID: new URLSearchParams(location.search).get("pairingId") || new URLSearchParams(location.search).get("pairing_id") || "",
    code: new URLSearchParams(location.search).get("code") || "",
    attempted: false,
  });
  const notifiedEventIDs = useRef(new Set<string>());
  const [commandState, setCommandState] = useState<CommandState | null>(null);
  const [pendingAccessToken, setPendingAccessToken] = useState("");

  // 页面可见性：切到后台时暂停轮询与快照兜底，回到前台立即补一次，省电又省流量。
  const documentVisible = useDocumentVisible();
  const documentVisibleRef = useRef(documentVisible);
  // 上一次的可见性，用来识别「由隐藏转为可见」这个瞬间。
  const wasDocumentVisibleRef = useRef(documentVisible);
  // 消息列表是否「贴着底部」。用户主动向上翻阅时不能夺走滚动位置。
  const stickToBottomRef = useRef(true);
  // 消息列里一条消息都没有（空态上屏）。渲染里算出来的是后置值，而输入条高度的
  // ResizeObserver 挂在 `[mobileView, project?.id]` 上，闭包里读不到最新的那个布尔，
  // 所以同步一份进 ref。
  const emptyThreadRef = useRef(false);
  // 安卓物理返回键的当前处理逻辑。放在 ref 里，监听只注册一次也能拿到最新状态。
  const backHandlerRef = useRef<() => boolean>(() => false);

  // 桌面端探测电脑端 Agent 是否已注册到云端。未注册时配对无法进行，页面应引导
  // 用户做一次注册，而不是让用户反复点击注定返回 503 的"生成二维码"。
  //
  // ⚠️ `ready` 只说明**凭据在不在**，Agent 进程崩了它依然是 true。所以这一屏要答的
  // "服务到底在不在跑"必须看 `lastAgentHeartbeatAt`：Agent 每 750ms 会打一次本机的
  // /api/remote/overview，control-server 把那次调用的时刻记在内存里回给我们。
  const loadAgentStatus = useCallback(async (): Promise<"ready" | "unregistered" | "failed"> => {
    if (!isDesktop()) return "failed";
    try {
      const value = await api<{ ready?: boolean; instanceId?: string; cloudUrl?: string; lastAgentHeartbeatAt?: string }>("/api/remote/agent-status");
      setAgentStatus({
        ready: Boolean(value?.ready),
        instanceId: value?.instanceId || "",
        cloudUrl: value?.cloudUrl || "",
        heartbeatAt: value?.lastAgentHeartbeatAt || "",
      });
      setAgentStatusState("loaded");
      return value?.ready ? "ready" : "unregistered";
    } catch {
      // 读不到也要分得清"没读到"和"未注册"：前者是失败（要重试），后者是事实（要注册）。
      setAgentStatus(null);
      setAgentStatusState("failed");
      return "failed";
    }
  }, []);

  // 这台电脑当前绑定的手机。一台电脑同一时间只服务一台（云端在"确认绑定"的事务里顶替），
  // 所以配对卡片必须能先说出"你要顶掉的是谁" —— 否则用户点完确认才发现另一台手机被断开。
  //
  // 三态而不是布尔：`ready:false`（云端还不知道这台电脑）与"请求失败"必须分开渲染。
  // 旧版把它压成一个 `bindingsReady`，失败时整块**什么都不渲染** —— 用户分不清
  // "没有手机绑定"和"没读到"。（项目规则：「还没回来」绝不能写成「没有数据」。）
  //
  // ⚠️ `silent` 是给**轮询**用的，两件事一起做：
  //   ① 不把状态打回 `loading` —— 否则卡片每 10 秒闪一次「正在读取…」，
  //      把一份本来稳定的读数演成"一直在重连"；
  //   ② 失败时**不动** `bindingsState`（保留上一次的读数，只置 `bindingsStale`）——
  //      把它打成 `failed` 会让绑定卡整块换成"读取失败"、页头那颗胶囊一起消失，
  //      然后 10 秒后再自己长回来。那同样是闪烁，而且丢的正是"这台电脑绑了谁"这条
  //      用户此刻最需要的信息。**读不到"最新"不等于"什么都没有"。**
  const loadBindings = useCallback(async (options?: { silent?: boolean }) => {
    if (!isDesktop()) return;
    // 请求代际：**晚发的必须赢**。`api()` 对 GET 带两级重试（500ms + 1000ms 退避，
    // 单次超时 15s → 重试那一次翻到 30s），所以一个先发出的请求完全可能比后发出的晚回来。
    // 没有这道闸，一次"先失败"的旧请求会覆盖掉一次"后成功"的新结果 ——
    // 表现就是卡片自己挂上"重读失败"，而其实刚刚才读成功过。
    // 同一个文件里的 `loadInstances`（`instancesRequestGenerationRef`）就是这个写法，别各造一套。
    const generation = ++bindingsRequestGenerationRef.current;
    if (!options?.silent) setBindingsState("loading");
    try {
      const value = await api<{ bindings?: unknown; ready?: boolean }>("/api/remote/bindings");
      if (generation !== bindingsRequestGenerationRef.current) return;
      // 归一化在纯模块里：`lastUsedAt` 的「缺键」与「null」必须保留成两种状态。
      const items = normalizeBindings(value?.bindings);
      setBindings(items);
      setBindingsState(value?.ready === false ? "unregistered" : "loaded");
      setBindingsStale(false);
      bindingsLoadedOnceRef.current = true;
      // 年龄**在这里就地量一次**，不留给下面那个 5 秒的定时器：定时器是 effect，
      // 要等这一帧绘制完才跑，于是中间那一帧 `phoneSyncAgeMs` 还是 null ——
      // 而"有读数、却拿不到年龄"在判据里是 unsupported，页头会先闪一下
      // 「已绑定手机」再跳成「手机在线」。两个 setState 在同一次 await 之后，
      // React 会合批成一次渲染，全程只有正确的值。
      setPhoneSyncAgeMs(syncAgeFrom(items[0]?.lastUsedAt));
    } catch {
      // 被更新的一次取代了：这一趟的失败不再代表"现在的读数不新了"，什么都不做。
      if (generation !== bindingsRequestGenerationRef.current) return;
      if (options?.silent) {
        setBindingsStale(true);
        return;
      }
      // 首次读 / 手动刷新失败：这时手上确实没有可信读数，才允许清空 + 报失败。
      setBindings([]);
      setBindingsState("failed");
    }
  }, []);

  // 「解除绑定」：把这台电脑当前绑定的手机断开（手机丢了、准备换设备时用）。
  // 做完立刻重读一次，让界面上的"当前绑定"与云端状态一致，而不是本地猜。
  async function revokeBindings() {
    setBusy(true); setError("");
    try {
      await api("/api/remote/bindings/revoke", { method: "POST" });
      await loadBindings();
      setPairingNotice({ text: "已断开这台电脑上的手机绑定", state: "idle" });
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "解除绑定失败");
    } finally {
      setBusy(false);
    }
  }

  const markConversationProcessing = useCallback((conversationID: string, snapshotRevision: number) => {
    setProcessingConversations((current) => {
      const existing = current[conversationID];
      return {
        ...current,
        [conversationID]: {
          count: (existing?.count || 0) + 1,
          startedAt: existing?.startedAt || Date.now(),
          snapshotRevision: existing?.snapshotRevision ?? snapshotRevision,
        },
      };
    });
  }, []);

  const clearConversationProcessing = useCallback((conversationID: string, all = false) => {
    setProcessingConversations((current) => {
      const existing = current[conversationID];
      if (!existing) return current;
      if (!all && existing.count > 1) return { ...current, [conversationID]: { ...existing, count: existing.count - 1 } };
      const next = { ...current };
      delete next[conversationID];
      return next;
    });
  }, []);

  // 扫码结果的唯一接收点：原生（`barcodesScanned` 事件）与 Web（BarcodeDetector 轮询）
  // 两条路都汇到这里，所以幂等守卫只写在这一处。ML Kit 是**逐帧持续上报**的 —— 同一张码
  // 会被反复识别，没有 `scanAccepted` 这道闸，下层就会对着同一张码反复走 claim 流程。
  // 因此 `if (scanAccepted.current) return true` 必须在最前面（连"不是配对码"的报错都
  // 不能被反复刷出来 —— 那一支返回 false 且不置位，只在真识别到码时才走到）。
  //
  // 依赖数组写 `[]` 是有意的：这里引用的 `closeScanOverlay` / `claimPairing` 都是组件内
  // 普通函数，会被捕获成**首帧那一版**。之所以安全，是因为它们只读"显式传入的参数 +
  // setState 函数"——`closeScanOverlay` 只调三个 setState；`claimPairing` 那两个 state
  // 默认参数（pairingID/pairingCode）在扫码路径上总被显式传参覆盖，永远不生效。
  // 换成带依赖的 useCallback 只会让 identity 频繁变化，进而把下面两个平台 effect 反复重启。
  const acceptScannedPairing = useCallback((value: string) => {
    const scanned = pairingFromScan(value);
    if (!scanned.pairingID) {
      setScanError("这不是 Milevia 配对二维码，请对准电脑端刚生成的二维码");
      return false;
    }
    if (scanAccepted.current) return true;
    scanAccepted.current = true;
    setPairingID(scanned.pairingID);
    setPairingCode(scanned.code || "");
    // 扫到之后立刻关层：走同一个出口（而不是就地写 setScanning(false)），
    // 这样"同一拍撤掉清底类名"这条快路径在成功/取消两条路上都生效。
    closeScanOverlay();
    if (/^\d{6}$/.test(scanned.code)) {
      void claimPairing(scanned.pairingID, scanned.code);
    } else {
      setPairingNotice({ text: "已识别配对会话，但二维码里没有校验码，请手动输入电脑端显示的 6 位数字", state: "idle" });
      // 二维码有意不含校验码（避免经浏览器历史或代理日志泄漏），扫码后必须
      // 人工补输，因此直接聚焦输入框，省掉一次寻找动作。
      window.setTimeout(() => manualCodeRef.current?.focus(), 0);
    }
    return true;
  }, []);

  // 取景层"在屏幕上"期间，整链必须同时保持两件事：① 清底（让 WebView 之下的摄像头透上来）；
  // ② 其余同级区块不绘制（`visibility: hidden`，顺带移出焦点与无障碍树）。
  //
  // 这两件事由**这一个** effect 拥有，不要散到两个平台分支里去 —— 散开必然踩两个坑：
  //   ① 失败态依然留在屏幕上（此时 scanning 已回 false），平台分支的清理会把类名撤掉 →
  //      浮层背后那屏恢复可见、**键盘还能 Tab 到被盖住的按钮**（浮层就不再是模态了）；
  //   ② 类名的增删若与浮层的挂载分处两个时机，关闭时会闪一帧"层已摘掉、页面还被隐藏着"
  //      的空屏（原生分支上那一帧还会再闪一下摄像头画面）。
  // 声明位置必须在两个平台 effect **之前**：同一次提交里 passive effect 按声明顺序执行，
  // 原生分支要保证 startScan() 之前底已经清好。
  const scanOverlayVisible = scanning || Boolean(scanError);
  useEffect(() => {
    setScannerActive(scanOverlayVisible);
    return () => setScannerActive(false);
  }, [scanOverlayVisible]);

  useEffect(() => {
    if (!scanning || !Capacitor.isNativePlatform()) return;
    let cancelled = false;
    let started = false;
    let timeout: number | undefined;
    scanAccepted.current = false;

    const stop = async () => {
      if (timeout !== undefined) window.clearTimeout(timeout);
      const listener = nativeScanListener.current;
      nativeScanListener.current = null;
      await listener?.remove().catch(() => undefined);
      if (started) await BarcodeScanner.stopScan().catch(() => undefined);
      setScanTorchAvailable(false);
      setScanTorchOn(false);
    };

    void (async () => {
      try {
        const { supported } = await BarcodeScanner.isSupported();
        if (!supported) throw new Error("此设备没有可用摄像头");
        let permission = await BarcodeScanner.checkPermissions();
        if (permission.camera === "prompt" || permission.camera === "prompt-with-rationale") {
          permission = await BarcodeScanner.requestPermissions();
        }
        if (permission.camera !== "granted" && permission.camera !== "limited") {
          // 走到这里可能是"用户刚拒绝"，也可能是"早就拒绝了、系统不再弹窗"。无论哪种，
          // 都只有系统设置这一条路能修好，所以直接把设置入口放出来。
          if (!cancelled) setScanNeedsSettings(true);
          throw new Error("未获得摄像头权限，请在系统设置中允许 Milevia 使用摄像头");
        }
        if (cancelled) return;

        nativeScanListener.current = await BarcodeScanner.addListener("barcodesScanned", ({ barcodes }) => {
          for (const barcode of barcodes) {
            if (acceptScannedPairing(barcode.rawValue || barcode.displayValue)) return;
          }
        });
        if (cancelled) return;
        await BarcodeScanner.startScan({ formats: [BarcodeFormat.QrCode], lensFacing: LensFacing.Back });
        started = true;
        // 这一段**不是**冗余的，删掉会让相机永久开着：清理函数（见下方 return）会在
        // `cancelled = true` 之后立刻调 `stop()`，而那时 `started` 还是 false（`startScan`
        // 尚未 resolve）→ `stop()` 里 `if (started) stopScan()` 整支跳过，相机没被停。
        // 随后 `startScan` 才 resolve、`started` 才变 true —— 若不在这里补一次，就再也没人
        // 会去停它了：listener 已被摘掉、超时也没设，用户看到的是"浮层关了但摄像头一直开着"。
        // 走 `stop()` 而不是就地写 stopScan()，是为了让"摘 listener / 关灯 / 复位状态"只在一个地方。
        if (cancelled) {
          await stop();
          return;
        }
        // 手电筒可用性单独问一次：问不到就当"没有"，不把异常冒泡成扫码失败。
        void BarcodeScanner.isTorchAvailable()
          .then((value) => { if (!cancelled) setScanTorchAvailable(Boolean((value as { available?: boolean })?.available)); })
          .catch(() => undefined);
        timeout = window.setTimeout(() => {
          if (!cancelled) {
            setScanError("30 秒内未识别到二维码，请靠近电脑屏幕、调亮亮度或打开手电筒后重试");
            setScanning(false);
          }
        }, 30_000);
      } catch (cause) {
        if (!cancelled) {
          setScanError(cause instanceof Error ? cause.message : "无法启动二维码扫描，请稍后重试");
          setScanning(false);
        }
      }
    })();

    return () => {
      cancelled = true;
      void stop();
    };
  }, [scanning, acceptScannedPairing]);

  // Web 分支（手机浏览器 / 桌面预览）：画面是自己画的，用 getUserMedia + BarcodeDetector。
  // 清底类名由上面那个 effect 统一管，这里不再碰（`.mobile-scan[data-backdrop="solid"]`
  // 会自己铺底，所以两条分支共用同一套取景层样式）。
  useEffect(() => {
    if (!scanning || Capacitor.isNativePlatform() || !scanVideo) return;
    let cancelled = false;
    let stream: MediaStream | undefined;
    const timeout = window.setTimeout(() => {
      if (!cancelled) {
        setScanError("30 秒内未识别到二维码，请靠近电脑屏幕、调亮亮度后重试");
        setScanning(false);
      }
    }, 30_000);
    const detectorCtor = (globalThis as typeof globalThis & { BarcodeDetector?: BarcodeDetectorLike }).BarcodeDetector;
    if (!detectorCtor) {
      window.clearTimeout(timeout);
      setScanError("当前浏览器不支持二维码识别，请用手机系统相机扫码，或改用「使用校验码」配对");
      setScanning(false);
      return;
    }
    const mediaDevices = navigator.mediaDevices;
    if (!mediaDevices?.getUserMedia) {
      window.clearTimeout(timeout);
      setScanError("当前设备无法访问摄像头，请检查浏览器摄像头权限");
      setScanning(false);
      return;
    }
    void mediaDevices.getUserMedia({ video: { facingMode: { ideal: "environment" } }, audio: false })
      .then(async (nextStream) => {
        if (cancelled) { nextStream.getTracks().forEach((track) => track.stop()); return; }
        stream = nextStream;
        scanVideo.srcObject = nextStream;
        await scanVideo.play();
        let detector: { detect(source: HTMLVideoElement): Promise<BarcodeDetectorResult[]> };
        try {
          detector = new detectorCtor({ formats: ["qr_code"] });
        } catch {
          throw new Error("当前 Android WebView 不支持二维码识别，请改用「使用校验码」配对");
        }
        while (!cancelled) {
          const results = await detector.detect(scanVideo).catch(() => []);
          if (results.some((item) => acceptScannedPairing(item.rawValue || ""))) break;
          if (results.some((item) => item.rawValue)) {
            setScanError("扫描到的二维码不是 Milevia 配对二维码，请对准电脑端刚生成的那一张");
          }
          await new Promise((resolve) => window.setTimeout(resolve, 250));
        }
      })
      .catch((cause: unknown) => {
        if (!cancelled) {
          setScanError(cause instanceof Error && cause.message.includes("不支持") ? cause.message : "无法打开摄像头，请在浏览器设置里允许本站使用摄像头后重试");
          setScanning(false);
        }
      });
    return () => {
      cancelled = true;
      window.clearTimeout(timeout);
      stream?.getTracks().forEach((track) => track.stop());
      if (scanVideo) scanVideo.srcObject = null;
    };
  }, [scanning, scanVideo, acceptScannedPairing]);

  // 返回本次请求的真实结果，供"手动刷新"判断该给什么反馈。
  // 三个结果必须分开：列表请求每 5 秒被轮询触发一次，手动刷新那一次**很容易被轮询抢答**
  // （手机上一次列表请求要几秒，而轮询周期就是 5 秒）。把"被更新的请求取代"当成失败，
  // 就会在一切正常时弹出一句"刷新失败：没有连上云端"——这正是用户看不懂的那种假失败。
  //   ok         = 拿到了列表（可能是空数组：云端确实一台电脑都没有）
  //   failed     = 请求真的失败了
  //   superseded = 这次请求被更新的请求取代，结果由那一次落地
  const loadInstances = useCallback(async (): Promise<InstanceFetchResult> => {
    const requestGeneration = ++instancesRequestGenerationRef.current;
    const superseded: InstanceFetchResult = { instances: [], outcome: "superseded" };
    if (!readActiveToken()) {
      if (requestGeneration !== instancesRequestGenerationRef.current) return superseded;
      setInstances([]);
      setInstanceID("");
      // 没有令牌是"还没配对"，属于拿到了真实状态（空的），不是失败。
      return { instances: [], outcome: "ok" };
    }
    setError("");
    // 这次请求代表哪台设备：下面回填信息时必须用同一个令牌，否则切换设备的空档里
    // 会把 A 的状态写到 B 的记录上。
    const requestToken = readActiveToken();
    try {
      const result = await cloud<Instance[]>("/v1/instances");
      if (requestGeneration !== instancesRequestGenerationRef.current) return superseded;
      const nextInstances = Array.isArray(result) ? result : [];
      setInstances(nextInstances);
      setInstanceID((current) => nextInstances.some((item) => item.instanceId === current)
        ? current
        : nextInstances[0]?.instanceId || "");
      // 回填"这台电脑叫什么、在不在线"。切换面板上那一行就是它 —— 一个令牌只知道
      // 自己那台电脑，所以这里的回填同时也是"跨设备列表"唯一的信息来源（逐台探测）。
      if (requestToken && nextInstances.length > 0) {
        const info = nextInstances[0];
        updateDevice(requestToken, { instanceId: info.instanceId || "", name: info.name || "", status: info.status || "", lastSeenAt: info.lastSeenAt || "" });
        setDevices(readDevices());
      }
      if (!Array.isArray(result)) {
        setError("云端返回了无效的电脑实例列表");
      }
      return { instances: nextInstances, outcome: "ok" };
    } catch (cause) {
      if (requestGeneration !== instancesRequestGenerationRef.current) return superseded;
      const reason = cause instanceof TypeError ? "无法连接云端，请确认 keyanjia.info:8443 已放行并可访问" : cause instanceof Error ? cause.message : "无法加载电脑实例";
      setError(reason);
      return { instances: [], outcome: "failed", reason };
    }
  }, []);

  useEffect(() => {
    snapshotRevisionRef.current = -1;
    // These maps are scoped to the selected desktop instance. Retaining them
    // across a logout or instance switch can make a coincidentally reused
    // conversation ID appear to be processing in the new instance.
    pendingMessageRef.current.clear();
    realtimeMessagesRef.current.clear();
    streamingMessagesRef.current.clear();
    pendingNoticesRef.current.clear();
    // 待确认的任务改动同样按实例隔离：换一台电脑之后，上一台那份"本地已生效、
    // 还没被确认"的改动不能再叠到新电脑的快照上。
    if (pendingTaskRef.current.size > 0) {
      pendingTaskRef.current.clear();
      setPendingTaskRevision((current) => current + 1);
    }
    // 待确认的会话与排队的消息同理：本地那条临时会话只在**发起它的那台电脑**上才有意义，
    // 换机器之后它永远等不到真身，留着就是一条点不动、发不出的幽灵会话。
    if (pendingConversationsRef.current.size > 0 || deferredMessagesRef.current.size > 0) {
      pendingConversationsRef.current.clear();
      deferredMessagesRef.current.clear();
      setPendingConversationRevision((current) => current + 1);
    }
    setPendingTaskBusy(new Set());
    setProcessingConversations({});
  }, [instanceID]);
  const loadSnapshot = useCallback(async (options?: { force?: boolean; instanceId?: string }): Promise<SnapshotFetchResult> => {
    // 允许显式指定实例：手动刷新刚刚才拉过实例列表，选中的电脑可能已经失效并被换掉，
    // 这时要请求"列表里实际选中的那台"，而不是这一轮闭包里那台已经不存在的。
    const target = options?.instanceId || instanceID;
    if (!target) return { outcome: "skipped", snapshot: null };
    const requestGeneration = ++snapshotLoadGenerationRef.current;
    const force = options?.force === true;
    // 版本基线。请求的实例与"当前选中的实例"不是同一台时（刚配对完、或选中的电脑已从列表
    // 里消失）没有可比基线，必须按全新实例处理：拿 B 的版本号去比 A 的，既可能让守卫不通过、
    // 把 B 的快照整个丢掉，又会把"什么都没应用"报成"已是最新"。
    const baseline = options?.instanceId && options.instanceId !== instanceID ? -1 : snapshotRevisionRef.current;
    setError("");
    try {
      // 带上已知版本号：云端在内容未变化时只回一个小标记，省掉一次全量传输。
      // 手动刷新（force）必须跳过这个短路——被短路掉的长轮询看不出来，但用户点了刷新
      // 却什么都没发生，就等于"刷新按钮坏了"（本轮修复的原始投诉）。而且本地快照可能
      // 落后于 snapshotRevisionRef（例如上一次失败后回落到 localStorage 缓存），
      // 这种情况下短路会把"显示着旧数据"判成"已是最新"。
      const query = force ? "" : `?revision=${baseline}`;
      const value = await cloud<Snapshot & { unchanged?: boolean }>(`/v1/instances/${encodeURIComponent(target)}/snapshot${query}`);
      if (value?.unchanged) return { outcome: "unchanged", snapshot: null };
      if (!value || !Array.isArray(value.projects)) {
        throw new Error("云端返回了无效的项目快照");
      }
      // Snapshots from older mobile builds may not carry a revision. Treat
      // those as the initial revision instead of dropping a valid project
      // list because `undefined >= -1` is false.
      value.snapshotRevision = typeof value.snapshotRevision === "number" && Number.isFinite(value.snapshotRevision)
        ? value.snapshotRevision
        : 0;
      if (requestGeneration === snapshotLoadGenerationRef.current && value.snapshotRevision >= baseline) {
        snapshotRevisionRef.current = value.snapshotRevision;
        const pending = pendingMessageRef.current;
        if (pending.size > 0) {
          value.projects = value.projects.map((item) => ({ ...item, conversations: item.conversations.map((entry) => {
            const messages = pending.get(entry.id);
            if (!messages || messages.size === 0) return entry;
            // Consume persisted messages as a multiset so two identical
            // prompts do not accidentally clear both optimistic entries.
            const persistedCounts = new Map<string, number>();
            for (const message of entry.messages) {
              if (message.role === "user") persistedCounts.set(message.content, (persistedCounts.get(message.content) || 0) + 1);
            }
            const additions: Message[] = [];
            const matchedRequestIDs = new Set<string>();
            for (const pendingMessage of messages.values()) {
              const count = persistedCounts.get(pendingMessage.content) || 0;
              if (count > 0) {
                persistedCounts.set(pendingMessage.content, count - 1);
                matchedRequestIDs.add(pendingMessage.requestId);
              }
              else additions.push({ id: `pending-${pendingMessage.requestId}`, role: "user", content: pendingMessage.content, createdAt: pendingMessage.createdAt });
            }
            for (const requestID of matchedRequestIDs) messages.delete(requestID);
            if (messages.size === 0) pending.delete(entry.id);
            return additions.length > 0 ? { ...entry, messages: [...entry.messages, ...additions] } : entry;
          }) }));
        }
        // A message event can arrive before the Agent has uploaded its next
        // snapshot. Preserve those messages while accepting the older snapshot
        // so streamed output cannot briefly disappear from the mobile view.
        const realtimeMessages = realtimeMessagesRef.current;
        if (realtimeMessages.size > 0) {
          value.projects = value.projects.map((item) => ({ ...item, conversations: item.conversations.map((entry) => {
            let changed = false;
            const messages = entry.messages.map((message) => {
              const live = realtimeMessages.get(message.id);
              if (!live || live.conversationId !== entry.id) return message;
              realtimeMessages.delete(message.id);
              // The recovery snapshot intentionally bounds older content. If
              // the realtime event has the full message, prefer it so a fast
              // snapshot cannot overwrite a complete assistant response with
              // its 2000-character prefix.
              if (live.message.content.length > message.content.length) {
                changed = true;
                return live.message;
              }
              return message;
            });
            const extras = [...realtimeMessages.values()]
              .filter((item) => item.conversationId === entry.id
                && item.revision >= value.snapshotRevision
                && !entry.messages.some((message) => message.id === item.message.id))
              .map((item) => {
                realtimeMessages.delete(item.message.id);
                return item.message;
              });
            return changed || extras.length > 0 ? { ...entry, messages: [...messages, ...extras] } : entry;
          }) }));
        }
        // 增量不落库，因此不在快照里：整体覆盖会把正在输出的回复抹掉。这里按
        // 累积内容把它补回，正式消息到达时替换、之后随快照自然消失。
        const streaming = streamingMessagesRef.current;
        if (streaming.size > 0) {
          value.projects = value.projects.map((item) => {
            const conversations = item.conversations.map((entry) => {
              let touched = false;
              const messages = entry.messages.map((message) => {
                if (!message.id.startsWith("stream-")) return message;
                const chunk = streaming.get(message.id.slice("stream-".length));
                if (!chunk || chunk.conversationId !== entry.id) return message;
                touched = true;
                return { ...message, content: chunk.content };
              });
              const pending: Message[] = [];
              for (const [messageID, chunk] of streaming) {
                if (chunk.conversationId !== entry.id) continue;
                const placeholderID = `stream-${messageID}`;
                if (messages.some((message) => message.id === placeholderID)) continue;
                pending.push({ id: placeholderID, role: "assistant", content: chunk.content, createdAt: chunk.createdAt });
                touched = true;
              }
              return touched ? { ...entry, messages: [...messages, ...pending] } : entry;
            });
            const changed = conversations.some((entry, index) => entry !== item.conversations[index]);
            return changed ? { ...item, conversations } : item;
          });
        }
        // 快照里的 notices 是事件原文，先按与实时通道同一套规则解析；再把本次刚收到、
        // 还没进快照的那几条并回来——少了后者，刚冒出来的"API 重试中"会被下一次快照覆盖掉。
        const pendingNotices = pendingNoticesRef.current;
        value.projects = value.projects.map((item) => ({ ...item, conversations: item.conversations.map((entry) => {
          const serverNotices = noticesFromSnapshot(entry.notices);
          const local = pendingNotices.get(entry.id);
          if (!local || local.length === 0) return { ...entry, notices: serverNotices };
          const serverIDs = new Set(serverNotices.map((notice) => notice.id));
          // "服务端已有同名记录"不等于"本地这条可以扔"。工具确认（replace 型）必须
          // 一直留着：快照有上传节流，下一次可能只带回更旧的 pending，此时若本地那条
          // 已被清掉，合并结果就会是 pending —— 界面从"已确认"倒退回"等待确认"。
          const remaining = local.filter((notice) => notice.merge === "replace" || !serverIDs.has(notice.id));
          if (remaining.length > 0) pendingNotices.set(entry.id, remaining);
          else pendingNotices.delete(entry.id);
          return { ...entry, notices: mergeNotices(serverNotices, remaining) };
        }) }));
        setSnapshot(value);
        localStorage.setItem(`milevia.snapshot.${target}`, JSON.stringify(value));
      }
      // 版本号有没有前进，就是"这次刷新到底有没有带回来新东西"的判据。
      return { outcome: value.snapshotRevision > baseline ? "updated" : "unchanged", snapshot: value };
    } catch (cause) {
      if (requestGeneration !== snapshotLoadGenerationRef.current) return { outcome: "skipped", snapshot: null };
      const reason = cause instanceof Error ? cause.message : "无法加载项目快照";
      const cached = localStorage.getItem(`milevia.snapshot.${target}`);
      if (cached) {
        try {
          const cachedValue = JSON.parse(cached) as Snapshot;
          setSnapshot(cachedValue);
          setError("当前显示的是最近一次同步快照");
          // 有缓存也要如实报失败：界面确实停在旧数据上，不能让刷新按钮显示"已刷新"。
          // 原因写短一点——"当前显示的是最近一次同步快照"那句由页面上的错误条负责说明，
          // 两边各说一句，避免同一条红框提示连说两遍。
          return { outcome: "failed", snapshot: cachedValue, reason: "无法连接云端" };
        } catch { /* ignore invalid cache */ }
      }
      setError(reason);
      return { outcome: "failed", snapshot: null, reason };
    }
  }, [instanceID]);

  const applyRealtimeEvent = useCallback((raw: MessageEvent): boolean => {
    try {
      const event = JSON.parse(String(raw.data)) as { eventId?: string; type?: string; taskRunId?: string; payload?: unknown; createdAt?: string };
      if (!event || !event.type || !event.payload) return false;
      const payload = event.payload as Partial<Message> & {
        conversationId?: string;
        projectId?: string;
        status?: string;
        // 流式增量事件的字段：messageId 是 CLI 自己的消息 id，与最终
        // assistant.message 上的 agentMessageId 一致，据此认领占位内容。
        messageId?: string;
        delta?: string;
        agentMessageId?: string;
      };
      if (event.type === "conversation.created" && typeof payload.conversationId === "string" && typeof payload.projectId === "string") {
        setSnapshot((current) => {
          if (!current) return current;
          return {
            ...current,
            projects: current.projects.map((item) => item.id !== payload.projectId || item.conversations.some((entry) => entry.id === payload.conversationId)
              ? item
              : {
                ...item,
                conversations: [{
                  id: payload.conversationId!,
                  title: "新会话",
                  status: "idle",
                  agentId: "",
                  lastActivityAt: event.createdAt || new Date().toISOString(),
                  isCurrent: true,
                  messages: [],
                }, ...item.conversations.map((entry) => ({ ...entry, isCurrent: false }))],
              }),
          };
        });
        // 视图切换**不在这里**做：这条事件只是把真身塞进快照，而"用户现在停在本地那条
        // 临时会话上、要把他接过去"是认领 effect 的活（它同时兜住命令回执与兜底快照
        // 两条来源）。在这里各自写一遍，三条链路就会各有各的行为。
        return true;
      }
      if (event.type === "assistant.delta") {
        const delta = typeof payload.delta === "string" ? payload.delta : "";
        const conversationId = typeof payload.conversationId === "string" ? payload.conversationId : "";
        if (typeof payload.messageId !== "string" || payload.messageId === "" || delta === "" || conversationId === "") return false;
        const placeholderID = `stream-${payload.messageId}`;
        const streaming = streamingMessagesRef.current;
        const previous = streaming.get(payload.messageId);
        const accumulated = {
          conversationId,
          content: (previous?.content || "") + delta,
          createdAt: previous?.createdAt || event.createdAt || new Date().toISOString(),
        };
        streaming.set(payload.messageId, accumulated);
        setSnapshot((current) => {
          if (!current) return current;
          return { ...current, projects: current.projects.map((item) => ({ ...item, conversations: item.conversations.map((entry) => {
            if (entry.id !== conversationId) return entry;
            const placeholder: Message = { id: placeholderID, role: "assistant", content: accumulated.content, createdAt: accumulated.createdAt };
            const index = entry.messages.findIndex((message) => message.id === placeholderID);
            return index < 0
              ? { ...entry, messages: [...entry.messages, placeholder] }
              : { ...entry, messages: entry.messages.map((message, at) => (at === index ? placeholder : message)) };
          }) })) };
        });
        return true;
      }
      if ((event.type === "user.message" || event.type === "assistant.message") && typeof payload.id === "string" && typeof payload.conversationId === "string" && typeof payload.content === "string") {
        const role: Message["role"] = event.type === "assistant.message" ? "assistant" : "user";
        const realtimeMessage: Message = { id: payload.id, runId: payload.runId, role, content: payload.content, createdAt: payload.createdAt || event.createdAt || new Date().toISOString() };
        realtimeMessagesRef.current.set(realtimeMessage.id, { conversationId: payload.conversationId, message: realtimeMessage, revision: snapshotRevisionRef.current });
        if (realtimeMessagesRef.current.size > 500) {
          const oldest = realtimeMessagesRef.current.keys().next().value;
          if (oldest) realtimeMessagesRef.current.delete(oldest);
        }
        if (role === "user") {
          const pending = pendingMessageRef.current.get(payload.conversationId);
          if (pending) {
            const matching = [...pending.values()].find((item) => item.content === payload.content);
            if (matching) pending.delete(matching.requestId);
            if (pending.size === 0) pendingMessageRef.current.delete(payload.conversationId);
          }
        }
        // 正式消息到达后由它取代流式占位，两者在同一次状态更新里交接，避免
        // 中间出现「占位 + 正式」并存的重复帧。
        const agentMessageID = typeof payload.agentMessageId === "string" ? payload.agentMessageId : "";
        const placeholderID = agentMessageID ? `stream-${agentMessageID}` : "";
        if (agentMessageID) streamingMessagesRef.current.delete(agentMessageID);
        setSnapshot((current) => {
          if (!current) return current;
          return { ...current, projects: current.projects.map((item) => ({ ...item, conversations: item.conversations.map((entry) => {
            if (entry.id !== payload.conversationId) return entry;
            const base = placeholderID === "" ? entry.messages : entry.messages.filter((message) => message.id !== placeholderID);
            const exists = base.some((message) => message.id === payload.id);
            const optimisticIndex = role === "user" ? base.findIndex((message) => message.id.startsWith("pending-") && message.role === "user" && message.content === payload.content) : -1;
            if (exists) return base.length === entry.messages.length ? entry : { ...entry, messages: base };
            const messages = optimisticIndex >= 0
              ? base.map((message, index) => index === optimisticIndex ? realtimeMessage : message)
              : [...base, realtimeMessage];
            return { ...entry, messages };
          }) })) };
        });
        return true;
      }
      // 运行状态类事件（API 重试、上下文压缩、后台任务、执行失败）：以前落到这里
      // 就返回 false 被丢掉，手机端只看得到对话内容，看不到"正在重试""压缩失败"。
      const parsed = noticeFromRealtimeEvent({ eventId: event.eventId, type: event.type, taskRunId: event.taskRunId, payload, createdAt: event.createdAt });
      if (parsed) {
        const pendingNotices = pendingNoticesRef.current;
        pendingNotices.set(parsed.conversationId, appendNotice(pendingNotices.get(parsed.conversationId), parsed.notice));
        setSnapshot((current) => {
          if (!current) return current;
          return {
            ...current,
            projects: current.projects.map((item) => ({ ...item, conversations: item.conversations.map((entry) => entry.id === parsed.conversationId ? { ...entry, notices: appendNotice(entry.notices, parsed.notice) } : entry) })),
          };
        });
        return true;
      }
    } catch { /* malformed realtime payloads are recovered by the snapshot */ }
    return false;
  }, []);

  const notifyHiddenMobileEvent = useCallback((raw: MessageEvent) => {
    if (!document.hidden || typeof Notification === "undefined" || Notification.permission !== "granted") return;
    try {
      const event = JSON.parse(String(raw.data)) as { eventId?: string; type?: string; payload?: unknown };
      if (!event.eventId || notifiedEventIDs.current.has(event.eventId)) return;
      // Conversation messages are already visible when the user returns. Task
      // and run state changes are the events that need an interruption notice.
      if (!event.type || !/^(task\.|run\.|approval\.)/.test(event.type)) return;
      notifiedEventIDs.current.add(event.eventId);
      const payload = event.payload && typeof event.payload === "object" ? event.payload as { summary?: unknown; status?: unknown } : {};
      const detail = typeof payload.summary === "string" ? payload.summary : typeof payload.status === "string" ? `状态：${payload.status}` : event.type;
      new Notification("Milevia", { body: detail, tag: event.eventId });
    } catch {
      // Malformed events are recovered by the normal snapshot refresh.
    }
  }, []);

  // 云端判定令牌不可用时（过期、被电脑端撤销、被另一台手机顶替、或配对尚未确认）
  // 云请求层会标记那台设备并广播该事件，这里把页面状态一起复位，回到配对流程，
  // 避免用户卡在一串注定失败的请求里。
  useEffect(() => {
    const onTokenCleared = () => {
      // 设备记录留在列表里（`revoked` 已由云请求层置位）：多电脑时用户需要看到
      // "是这一台被顶掉了"，而不是所有配对一起消失。
      const remaining = readDevices();
      const stillActive = readActiveToken();
      // ⚠️ 被顶掉的**正是当前这台**时必须顺移到下一台可用的电脑 —— 与 `unbindDevice` 同一条
      // 产品语义："一台电脑被另一台手机接管"不该让整台手机停下来。
      // 这里以前把 `token` 写死成空串（`next` 只用在文案里），于是出现"文案说「可切换到其它
      // 电脑」、页面却把用户按在配对页上"：配对页打开时背景是 inert 的，而「切换」入口就在
      // 背后那一屏，用户根本够不着（2026-09-20 第二轮自查抓到，与 `unbindDevice` 是同一族
      // 缺陷的另一半）。
      const keep = stillActive && remaining.some((item) => item.token === stillActive && !item.revoked)
        ? stillActive
        : remaining.find((item) => !item.revoked)?.token || "";
      setDevices(remaining);
      // ⚠️ 必须**同时**写 storage：`setToken` 只改组件 state，而 `cloud()` 是从 storage 取令牌的
      // （`deviceToken ?? readActiveToken()`）—— 只写 state 会让界面显示"已切到另一台电脑"、
      // 实际每个请求仍然带着被吊销的那台令牌，全部 401（`switchDevice` 里 `setActiveToken` 与
      // `setToken` 成对出现就是这个原因；本轮第二轮自查抓到）。
      // `keep` 为空（一台可用的都不剩）时把 active 清掉也是对的：留着被吊销的那个，
      // 下次冷启动会再去撞一次 401，又提示一遍，永远好不了。设备记录不受影响（仍在列表里标着
      // "需重新配对"），清掉的只是"当前选中是谁"。
      setActiveToken(keep);
      setToken(keep);
      setInstances([]);
      setInstanceID("");
      setSnapshot(null);
      setSelectedProject("");
      setSelectedConversation("");
      // 这一条**不走 `openPairingPanel`**（它要先把"为什么"写进状态行，不能走那个会清空状态行的
      // 出口），所以设备面板得在这里自己收 —— 否则面板盖在配对页上，页面等于没打开。
      setDevicesOpen(false);
      // 还有电脑可用时**不弹配对页**（那不需要"重新配对"），只在页面顶部留一条"发生了什么"；
      // 一台都不剩时才把页面叫起来 —— 否则用户面对的是"没有电脑、也没有入口"（同 `unbindDevice`）。
      setPairingExpanded(!keep);
      setPairingNotice({
        text: keep
          ? "这台电脑的绑定已失效（可能已被另一台手机绑定），已切到另一台电脑"
          : "云端令牌已失效，请重新配对",
        state: "error",
      });
      // 顺移之后要重新拉一次实例：设备卡的渲染条件是 `instance` 存在，而设备面板只能从那张卡进，
      // 不拉的话用户既看不到卡、也没有入口去切别的电脑。`loadInstances` 的依赖是空的、内部按
      // `readActiveToken()` 取令牌，所以紧挨着 `setToken` 调用不会拿到旧闭包（与 `unbindDevice` 同）。
      if (keep) void loadInstances();
    };
    globalThis.addEventListener("milevia:token-cleared", onTokenCleared);
    return () => globalThis.removeEventListener("milevia:token-cleared", onTokenCleared);
    // `loadInstances` 是空依赖的 useCallback（常量引用），挂上去不会让这个 effect 反复重建。
  }, [loadInstances]);
  useEffect(() => {
    documentVisibleRef.current = documentVisible;
  }, [documentVisible]);

  // 启动后在后台查一次手机端有没有新版本。Android 包没法自己下载安装，所以这里
  // 只把结果做成一条横幅，点「立即更新」交给系统浏览器下载（见 mobile-update.ts）。
  // 失败一律静默：更新提醒不值得在启动路径上打扰用户，也不该让页面出现红字。
  useEffect(() => {
    if (!Capacitor.isNativePlatform()) return;
    const controller = new AbortController();
    void checkMobileUpdate(controller.signal)
      .then((next) => {
        if (next) setMobileUpdate(next);
      })
      .catch(() => undefined);
    return () => controller.abort();
  }, []);
  useEffect(() => {
    if (!token.trim()) return;
    void loadInstances();
    // 后台不轮询。visibilitychange 会重跑本效果，因此回到前台时上面这句会立刻补一次。
    if (!documentVisible) return;
    const timer = window.setInterval(() => { void loadInstances(); }, 5000);
    return () => window.clearInterval(timer);
  }, [token, loadInstances, documentVisible]);
  useEffect(() => { void loadSnapshot(); }, [loadSnapshot]);
  useEffect(() => {
    if (!instanceID) return;
    const tokenValue = readActiveToken();
    const controller = new AbortController();
    let stopped = false;
    let lastEventID = "";
    // SSE is the fast path; periodic snapshots remain active because
    // conversation output is produced independently of task events.
    // SSE is the normal fast path. Keep a slower safety poll for proxies or
    // networks that silently drop events, without competing with every event
    // refresh and increasing full-snapshot traffic threefold.
    // SSE 是快速通道，仍保留一个较慢的快照兜底，兼容会静默丢事件的代理与网络。
    // 后台不跑兜底轮询（SSE 连接本身保留），回到前台由可见性效果补刷一次快照。
    let fallbackTimer: number | null = null;
    const startFallbackTimer = () => {
      if (fallbackTimer !== null) return;
      fallbackTimer = window.setInterval(() => {
        if (!documentVisibleRef.current) return;
        void loadSnapshot();
      }, 15_000);
    };
    startFallbackTimer();
    let refreshTimer: number | null = null;
    const scheduleSnapshotRefresh = (delay: number) => {
      // Trailing throttle: a busy assistant stream cannot keep postponing
      // durable task metadata and history reconciliation indefinitely.
      if (refreshTimer !== null) return;
      refreshTimer = window.setTimeout(() => {
        refreshTimer = null;
        // 后台不拉快照：SSE 事件在后台仍会到达，若不拦住，前面「隐藏时暂停
        // 轮询」就会被这条路径完全抵消。回到前台时可见性效果会整体补刷一次。
        if (!documentVisibleRef.current) return;
        void loadSnapshot();
      }, delay);
    };
    const wait = (delay: number) => new Promise<void>((resolve) => {
      const timer = window.setTimeout(resolve, delay);
      controller.signal.addEventListener("abort", () => { window.clearTimeout(timer); resolve(); }, { once: true });
    });
    const runStream = async () => {
      let retryDelay = 1000;
      while (!stopped && !controller.signal.aborted) {
        try {
          const query = new URLSearchParams({ instanceId: instanceID });
          await consumeMobileEventStream(`${cloudURL}/v1/stream?${query.toString()}`, tokenValue, lastEventID, (event) => {
            if (event.lastEventId) lastEventID = event.lastEventId;
            notifyHiddenMobileEvent(event);
            const isMessage = applyRealtimeEvent(event);
            // 消息内容已经随事件到达，不需要为它再拉整份快照：那会让手机在每条
            // 助手消息后重新下载全部项目与会话，是移动端延迟最大的单一来源。结构性
            // 事件（任务、会话生命周期）仍需对账，漏掉的事件由慢速兜底轮询收敛。
            if (!isMessage) scheduleSnapshotRefresh(500);
          }, controller.signal);
          if (stopped || controller.signal.aborted) break;
          await wait(retryDelay);
          retryDelay = 1000;
        } catch {
          if (stopped || controller.signal.aborted) break;
          startFallbackTimer();
          await wait(retryDelay);
          retryDelay = Math.min(retryDelay * 2, 30_000);
        }
      }
    };
    void runStream();
    return () => {
      stopped = true;
      controller.abort();
      if (fallbackTimer !== null) window.clearInterval(fallbackTimer);
      if (refreshTimer !== null) window.clearTimeout(refreshTimer);
    };
  }, [instanceID, loadSnapshot, applyRealtimeEvent, notifyHiddenMobileEvent]);
  // 回到前台补刷一次快照，避免用户先看到后台期间的旧状态。
  // 只在「隐藏 → 可见」的瞬间触发：实例切换等其它原因引起的重跑不需要额外拉快照。
  // （实例列表由上面的轮询效果在可见性变化时自动补拉，这里不重复。）
  useEffect(() => {
    const wasVisible = wasDocumentVisibleRef.current;
    wasDocumentVisibleRef.current = documentVisible;
    if (!documentVisible || wasVisible) return;
    if (!instanceID) return;
    void loadSnapshot();
  }, [documentVisible, instanceID, loadSnapshot]);
  useEffect(() => {
    let cancelled = false;
    if (!pairingURL) {
      setPairingQR("");
      return () => { cancelled = true; };
    }
    void QRCode.toDataURL(pairingURL, { width: 240, margin: 1, errorCorrectionLevel: "M" })
      .then((dataURL) => { if (!cancelled) setPairingQR(dataURL); })
      .catch(() => { if (!cancelled) setPairingQR(""); });
    return () => { cancelled = true; };
  }, [pairingURL]);
  // 二维码倒计时。以前只在文案里写死"有效期 5 分钟"：到点之后那张**已经失效的码还挂在屏幕上**，
  // 用户拿着它反复扫，手机端只会回"配对已失效，请让电脑重新生成"。现在按云端的 expiresAt 走秒，
  // 到 0 由 `pairingExpired` 把二维码判死（灰度 + 遮罩 + 就地重新生成）。
  useEffect(() => {
    const deadline = Date.parse(pairingExpiresAt);
    if (!Number.isFinite(deadline)) return;
    const tick = () => setPairingSecondsLeft(Math.max(0, Math.round((deadline - Date.now()) / 1000)));
    tick();
    const timer = window.setInterval(tick, 1000);
    return () => window.clearInterval(timer);
  }, [pairingExpiresAt]);
  // 成功提示自己会走：绑定完成后它孤零零留在项目页上没必要（也没地方点掉）。
  // 只给 success 挂计时器 —— 等待/失败两条是用户还需要读的信息，不该被时间吃掉。
  useEffect(() => {
    if (pairingNotice?.state !== "success") return;
    const timer = window.setTimeout(() => setPairingNotice(null), 8000);
    return () => window.clearTimeout(timer);
  }, [pairingNotice]);
  useEffect(() => {
    if (!commandState || terminalCommandStatuses.includes(commandState.status)) return;
    const timer = window.setInterval(() => {
      void cloud<CommandState & { command?: { commandId?: string } }>(`/v1/commands/${encodeURIComponent(commandState.commandId)}`)
        .then((value) => setCommandState((current) => mergeCommandState(current, normalizeCommandState(value, commandState.commandId))))
        .catch(() => undefined);
    }, 5000);
    return () => window.clearInterval(timer);
  }, [commandState?.commandId, commandState?.status]);
  useEffect(() => {
    if (!pendingAccessToken || !pairingID) return;
    const timer = window.setInterval(() => {
      void cloud<{ status: string }>(`/v1/pairings/${encodeURIComponent(pairingID)}/status`)
        .then((state) => {
          if (state.status === "confirmed") {
            // 一台新电脑绑上了：进设备表、并成为当前设备（同时镜像旧键，见 mobile-devices）。
            addOrReplaceDevice({ token: pendingAccessToken, instanceId: instanceID, name: instance?.name || "" });
            setDevices(readDevices());
            setToken(pendingAccessToken);
            setPendingAccessToken("");
            setPairingExpanded(false);
            setPairingNotice({ text: "电脑已确认，绑定完成", state: "success" });
            void loadInstances();
          } else if (["expired", "cancelled"].includes(state.status)) {
            setPendingAccessToken("");
            setPairingNotice({ text: "配对已失效，请让电脑重新生成二维码", state: "error" });
          }
        })
        .catch(() => undefined);
    }, 1500);
    return () => window.clearInterval(timer);
  }, [pendingAccessToken, pairingID, loadInstances]);
  // 桌面端跟踪配对会话：云端只允许在手机提交校验码之后确认，提前点击必然被
  // 拒绝。让"确认绑定"跟着会话状态启用，用户就不必盲点并对着报错猜原因。
  useEffect(() => {
    if (mobileApp || !pairingID.trim() || pairingConfirmed) return;
    let cancelled = false;
    const poll = () => {
      void api<{ status?: string }>(`/api/remote/pairing/status?pairingId=${encodeURIComponent(pairingID.trim())}`)
        .then((state) => {
          if (cancelled) return;
          const status = String(state?.status || "");
          if (status === "confirmed") {
            setPairingConfirmed(true);
            setPairingReadyForConfirm(false);
            setPairingNotice({ text: "已确认绑定，手机可以开始使用", state: "success" });
          } else if (status === "expired" || status === "cancelled") {
            setPairingReadyForConfirm(false);
            setPairingNotice({ text: "配对已失效，请重新生成二维码", state: "error" });
          } else if (status === "claimed") {
            setPairingReadyForConfirm(true);
            setPairingNotice({ text: "手机已扫码提交，请点击「确认绑定」", state: "waiting" });
          } else {
            setPairingReadyForConfirm(false);
          }
        })
        .catch(() => undefined);
    };
    poll();
    const timer = window.setInterval(poll, 2000);
    return () => { cancelled = true; window.clearInterval(timer); };
  }, [mobileApp, pairingID, pairingConfirmed]);
  // 桌面端进入页面时探测一次远程服务状态。
  useEffect(() => {
    if (mobileApp) return;
    void loadAgentStatus();
  }, [mobileApp, loadAgentStatus]);
  // 桌面端进入页面时读一次"现在绑着哪台手机"。它和 agentStatus 一样属于页面打开时
  // 就应该在那儿的信息：用户点生成二维码之前，得先看见自己要顶掉谁。
  //
  // ⚠️ 依赖写 `agentStatus?.ready`（布尔），**不是 `agentStatus`（对象）**：后者每次读回状态
  // 都是新对象，注册期间那 2 秒一次的轮询会让这张卡跟着重读，界面上闪成"正在读取…"。
  //
  // 未注册时**也要读**：服务端对未注册的实例回 `{ready:false}`（不是错误，见 remoteBindings），
  // 卡片据此说"远程服务未注册，暂时读不到绑定信息"——比停在"正在读取…"诚实。
  // 所以这里不能写成"未就绪就早退"：那条早退以前成立只因为首次运行时 agentStatus 还是 null。
  //
  // ⚠️ 这个 effect 在挂载时会跑**两次**（先 `agentStatus` 为 null、再 `ready` 变 true），
  // 也就是"开一次页面发两次请求"。第二次必须走 `silent`，否则：① 卡片会闪一次
  // 「正在读取…」；② 第二次偶发失败会把第一次刚读到的内容清成"读取失败"。
  // 但**首次**不能 silent —— 首次必须能显示加载态与失败态，
  // 否则"还没回来"就会被渲染成"没有手机绑定"（这个坑踩过三次）。
  useEffect(() => {
    if (mobileApp) return;
    void loadBindings({ silent: bindingsLoadedOnceRef.current });
  }, [mobileApp, agentStatus?.ready, loadBindings]);
  // 心跳年龄要自己走。桌面端这一屏没有别的轮询，只在渲染期读 Date.now() 的话，
  // Agent 停掉之后页面会一直显示"运行中"（实测：一屏静止的假绿）。
  // 5 秒一算：阈值是 15 秒，最坏 5 秒的判定延迟，代价可以忽略。
  useEffect(() => {
    if (mobileApp) return;
    const measure = () => {
      const at = Date.parse(agentStatus?.heartbeatAt || "");
      setHeartbeatAgeMs(Number.isFinite(at) ? Date.now() - at : null);
    };
    measure();
    const timer = window.setInterval(measure, 5000);
    return () => window.clearInterval(timer);
  }, [mobileApp, agentStatus?.heartbeatAt]);
  // 手机「最近同步」的年龄也要自己走 —— 和上面心跳同一套理由，但**必须分开两个 effect**：
  // 合成一个的话，`agentStatus.heartbeatAt` 每次刷新都会把手机的年龄计算一起重置。
  // 5 秒一算：阈值是 45 秒，最坏 5 秒的判定延迟。
  // ⚠️ 取年龄用纯模块里的 `syncAgeFrom`，和 `loadBindings` 落地那一刻用的是同一个函数 ——
  // 两处各写一遍 `Date.parse` 会分叉出"刚读到时在线、5 秒后跳未同步"的抖动。
  useEffect(() => {
    if (mobileApp) return;
    const measure = () => setPhoneSyncAgeMs(syncAgeFrom(bindings[0]?.lastUsedAt));
    measure();
    const timer = window.setInterval(measure, 5000);
    return () => window.clearInterval(timer);
  }, [mobileApp, bindings]);
  // 「手机现在还在不在用」是一件**会自己变**的事实：手机打开 / 切后台 / 断网都不经过这一页，
  // 所以只有轮询能发现。没有它的时候，绑定信息只在"进页面"和"点刷新"那两刻是新的 ——
  // 用户在手机上确认绑定后回到电脑前，看到的还是几分钟前那句"还没有手机绑定这台电脑"。
  //
  // 三件事缺一不可：
  //   ① **静默重读**（`silent`），否则每轮都闪一次「正在读取…」；
  //   ② **页面不可见时停表**：切走的标签页不该继续打云端（手机端也是这么做的）；
  //   ③ **防叠加**：上一轮还没回来就跳过这一轮（理由见 `bindingsPollInFlightRef`）。
  //      `.finally` 比在 `loadBindings` 里收尾更稳：无论成功、失败、被取代，闸都会放下来。
  // 依赖里带 `agentStatus?.ready` —— 未注册时云端会回 `ready:false`，
  // 注册成功那一刻必须立刻重读一次，而不是干等一个轮询周期。
  useEffect(() => {
    if (mobileApp) return;
    if (!documentVisible) return;
    const timer = window.setInterval(() => {
      if (bindingsPollInFlightRef.current) return;
      bindingsPollInFlightRef.current = true;
      void loadBindings({ silent: true }).finally(() => { bindingsPollInFlightRef.current = false; });
    }, 10_000);
    return () => window.clearInterval(timer);
  }, [mobileApp, documentVisible, agentStatus?.ready, loadBindings]);
  // 注册是异步的：Agent 子进程要连上云端并回报凭据后才可用，因此注册后轮询到
  // 就绪或超时为止，让用户看到结果而不是自行猜测。
  //
  // ⚠️ 两个**终态回执都写在卡片外面**（`pairingNotice` 那条，渲染在两栏之上）：
  //   ① 成功那一刻 `agentStatus.ready` 变 true ⇒ `desktopNeedsEnroll` 变 false ⇒ 注册卡
  //      连同卡片里的 `agentEnrollMessage` 一起卸载 —— 写在卡片里的"已就绪"用户永远看不到；
  //   ② 超时那条同理不可靠：用户可能在等待期间点了「取消」，卡片已经切回配对卡。
  //   这是配对状态行踩过的同一个坑（回执不能挂在会被卸载的容器里）。
  // 卡片里的 `agentEnrollMessage` 只留给**提交本身**的结果（那时卡片一定在屏幕上）。
  useEffect(() => {
    if (!agentEnrollWaiting) return;
    let attempts = 0;
    const timer = window.setInterval(() => {
      attempts += 1;
      void loadAgentStatus().then((outcome) => {
        if (outcome === "ready") {
          setAgentEnrollWaiting(false);
          // 成功要顺手把"重新注册"那个入口收掉，否则主栏会停在
          // "已就绪，但还摆着让你注册的表单"这种自相矛盾的状态上。
          setReenrollOpen(false);
          setAgentEnrollMessage("");
          setPairingNotice({ text: "远程服务已就绪，现在可以生成二维码了。", state: "success" });
          return;
        }
        if (attempts >= 15) {
          setAgentEnrollWaiting(false);
          setAgentEnrollMessage("");
          setPairingNotice({ text: "仍未检测到注册结果，请查看数据目录下的 milevia-agent.log 后重试。", state: "error" });
        }
      });
    }, 2000);
    return () => window.clearInterval(timer);
  }, [agentEnrollWaiting, loadAgentStatus]);

  const instance = instances.find((item) => item.instanceId === instanceID);
  // 界面上看到的项目列表 = 服务端快照 + 还没被电脑端确认的本地改动。
  //
  // 两个 revision 都只是触发器：待确认改动存在 ref 里，改了 ref 不会重渲染。
  // 会话那一层要在任务那一层**之后**叠：新建会话时用户往往紧接着就发第一条消息，
  // 而那条消息是挂在会话卡上的（见 pendingConversationCard），顺序反了就会丢。
  const projects = useMemo(
    () => applyPendingConversations(
      applyPendingTaskMutations(snapshot?.projects || [], pendingTaskRef.current.values()),
      pendingConversationsRef.current.values(),
      (conversationId) => Array.from(pendingMessageRef.current.get(conversationId)?.values() || []),
    ),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [snapshot, pendingTaskRevision, pendingConversationRevision],
  );

  // ---- 项目卡片排序（按住浮起 → 跟着手指走 → 邻居让位）----
  // 顺序只存本机：快照的权威顺序在服务端，这里只是"这台手机上我想先看到哪个"。
  const [orderVersion, setOrderVersion] = useState(0);
  const [dragHint, setDragHint] = useState(() => readMobileDragHint());
  const orderedProjects = useMemo(() => {
    const byId = new Map(projects.map((project) => [project.id, project]));
    return sortProjectIds(projects.map((project) => project.id), MOBILE_PROJECT_ORDER_STORAGE_KEY)
      .map((id) => byId.get(id))
      .filter((project): project is Project => Boolean(project));
    // orderVersion 是拖拽落库后的人工触发器：localStorage 变了本身不会重渲染。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [projects, orderVersion]);
  const commitProjectOrder = useCallback((ids: string[]) => {
    persistOrder(ids, MOBILE_PROJECT_ORDER_STORAGE_KEY);
    setOrderVersion((version) => version + 1);
  }, []);
  const noteReordered = useCallback(() => { setDragHint(false); dismissMobileDragHint(); }, []);
  const { gridRef: projectPickerRef, ghostRef: projectDragGhostRef, draggingId: draggingProjectId, lifted: projectLifted, onPointerDown: onProjectPointerDown } = useCardDragReorder({
    // busy 时不给拖：那一刻卡片本身是禁用的，抓住一张禁用卡会让人以为列表卡死了。
    enabled: mobileApp && mobileView === "projects" && !busy && orderedProjects.length > 1,
    orderKey: orderedProjects.map((project) => project.id).join("|"),
    commit: commitProjectOrder,
    onReordered: noteReordered,
  });
  const draggingProject = draggingProjectId ? orderedProjects.find((project) => project.id === draggingProjectId) ?? null : null;
  // 每一次快照落地都对一次账：已经反映在快照里的待确认改动就该摘掉「同步中」。
  //
  // 这条不是"命令成功"那条路径的重复 —— 它兜的是"命令没在 30 秒窗口内拿到回执"
  // 那种情形：那时我们**故意**不回滚（回滚会删掉一张可能已经建好的卡片），于是本地
  // 卡片只能靠快照自己收敛。没有这个 effect，那张卡会一直挂着「同步中」。
  useEffect(() => {
    const current = pendingTaskRef.current;
    if (!snapshot || current.size === 0) return;
    let changed = false;
    for (const mutation of Array.from(current.values())) {
      if (!mutationReflectsInSnapshot(mutation, snapshot.projects, current.values())) continue;
      current.delete(mutation.taskId);
      setPendingTaskBusy((previous) => {
        if (!previous.has(mutation.taskId)) return previous;
        const next = new Set(previous);
        next.delete(mutation.taskId);
        return next;
      });
      changed = true;
    }
    if (changed) markPendingTasksChanged();
  }, [snapshot, markPendingTasksChanged]);
  // 待确认会话的**唯一**认领点：真身一旦出现在快照里，就把本地那条换成它。
  //
  // 三条来源最终都落到同一个 snapshot 上，所以判据只写在这里一份，不散到三条链路里：
  //   ① SSE 的 conversation.created（快车道，通常几百毫秒）；
  //   ② 命令回执（回执里带回真身，会先塞进快照）；
  //   ③ 15 秒兜底快照（SSE 断掉时唯一的收敛路径）。
  // 只认命令回执那条的话，SSE 断线时用户就会永远停在那条本地会话上。
  useEffect(() => {
    const registry = pendingConversationsRef.current;
    if (registry.size === 0 || !snapshot) return;
    for (const item of Array.from(registry.values())) {
      const owning = snapshot.projects.find((entry) => entry.id === item.projectId);
      if (!owning) continue;
      // 同项目里同时新建两条时，谁也不许把对方已认下的那条会话再认一遍。
      const claimed = Array.from(registry.values())
        .filter((other) => other.id !== item.id && other.resolvedId)
        .map((other) => other.resolvedId as string);
      const realID = matchPendingConversation(item, owning.conversations, claimed);
      if (!realID) continue;
      // ① 先把真 id 认下来（本地卡改用真 id 继续顶着；排队的消息到这一刻才第一次有机会补发）。
      if (!item.resolvedId) resolvePendingConversation(item.id, realID);
      // ② 交棒要等**快照版本号真的前进过**，不能只看"快照里出现了这条会话"：
      //    `conversation.created` 事件会先把会话塞进当前那版快照，而紧随其后的刷新拿到的
      //    很可能还是"创建之前"的那一版（版本号没动、内容里没有它）。按"出现了就交棒"，
      //    那一刷就会把卡片和用户刚打的消息整块盖掉（浏览器探针实测复现过）。
      if (snapshot.snapshotRevision > item.seenRevision) adoptPendingConversation(item.id, realID);
    }
    // 依赖只写 snapshot：待确认表在 ref 里，快照每前进一次就重新对一遍账。
  }, [snapshot]);
  const project = projects.find((item) => item.id === selectedProject);
  // Keep the agent visible wherever the mobile conversation title is shown.
  // The API title remains untouched; this is presentation-only decoration.
  const conversations = useMemo(() => (project?.conversations || []).map((item) => ({
    ...item,
    title: `${item.title || "未命名会话"} · ${conversationAgentLabel(item.agentId)}`,
    // 会话名在这里只做一件事：给历史列表当主标题。会话页顶栏**不用**它 —— 它是首条用户消息
    // 截出来的前 80 字，挂在顶栏那行大字上是句半截的话，顶栏改用项目名（见下面的 showConversationTitle）。
  })), [project?.conversations]);
  const conversation = conversations.find((item) => item.id === selectedConversation) || conversations.find((item) => item.isCurrent) || conversations[0];
  // 当前这条会话是不是"本地已经建好、电脑端还没分配真 id"。它只影响两件事：
  // 快捷方式暂时不能下发（那条命令必须带真 id），以及消息要走延迟队列（见 sendConversationMessage）。
  const conversationPending = Boolean(conversation && isPendingConversationID(conversation.id));
  // 中继发信器：文件与 Git 两个适配器**共用同一份**。
  //
  // 它一次带全实例 id、项目与工作区（conversationId），并把 timeoutMs 原样送出去
  // （云端只夹取数值、不解析 op —— 见 docs/41 §3.2）。它同时是"换电脑 / 换项目 /
  // 换会话"的依赖单元：两个适配器都只在它变化时重建。
  //
  // 会话还是"本地已建好、电脑端没分配真 id"时**不能**把它的 id 传下去：
  // 服务端要按这个 id 去查工作区，查不到会让整个子视图报错。这时退回项目共享工作区。
  const rpcTransport = useMemo(() => {
    const projectID = project?.id ?? "";
    const conversationID = conversation && !conversationPending ? conversation.id : "";
    return (op: string, params: unknown, timeoutMs?: number) =>
      cloud<MobileRpcReply>(`/v1/instances/${encodeURIComponent(instanceID)}/rpc`, {
        method: "POST",
        body: JSON.stringify({ op, projectId: projectID, conversationId: conversationID || undefined, params, timeoutMs }),
      });
  }, [instanceID, project?.id, conversation?.id, conversationPending]);
  // 文件适配器**持有缓存**（内容 + 子树），所以绝不能在每次渲染时重建 ——
  // 重建就是缓存清零，用户在目录里点两下会不停重新拉取。
  const filesAdapter = useMemo(() => createMobileFsRequest({ transport: rpcTransport }), [rpcTransport]);
  // 消息与运行记录按时间混排：用户要看的是"什么时候发生了什么"，而不是
  // "对话"和"状态"两个割裂的列表。sort 是稳定的，同一时刻消息在前、状态卡在后。
  const conversationTimeline = useMemo(() => {
    const entries: Array<
      | { kind: "message"; key: string; at: number; message: Message }
      | { kind: "notice"; key: string; at: number; notice: RemoteNotice }
    > = [];
    for (const message of conversation?.messages || []) {
      entries.push({ kind: "message", key: message.id, at: Date.parse(message.createdAt) || 0, message });
    }
    for (const notice of conversation?.notices || []) {
      entries.push({ kind: "notice", key: notice.id, at: Date.parse(notice.createdAt) || 0, notice });
    }
    return entries.sort((left, right) => left.at - right.at);
  }, [conversation]);
  // 图标行**是否真的开着**：`messageActionId` 记的是"哪条消息的 id"，而那条消息可能已经不在这份
  // 快照里了（换会话已经清了，但云端重新下发一份不含它的快照不会走那条路）。这种"谁也不对应"的
  // 状态必须当成**关着**，否则按一次返回键只会把它清掉、界面上什么都不发生 —— 用户读到的是
  // "返回键失灵了一次"（与 leaveConversationView 里那条"不让谁也收不掉的状态吃掉一次返回键"同因）。
  // 焦点 effect 不需要这个判据：届时 `messageActionsRef.current` 是 null，可选链直接空过。
  // 入场动画的判据（2026-09-20 动效）：算出"这一批里哪些 key 是刚到的"。
  // 四条规矩，缺一条都会出可见的毛病：
  //   ① **进会话视图 / 换会话后的第一批不算"新到"** —— 那是整段历史一次性挂载，
  //      全给动画就是"整屏依次浮起"，看起来像页面在抖。用 arriveScopeRef 认这个"第一批"。
  //   ② 只在真的新增时 setState：流式回复每帧都在改 content，若拿"内容"当判据，
  //      每帧都会算出一批"新的"（其实 id 没变）→ 动画被反复触发。这里只比 key。
  //   ③ **"占位被正式消息顶替"不是新消息**（见下面 replacedPlaceholder）—— 这条是第三轮
  //      复查实测抓出来的：`pending-*` / `stream-*` 被云端消息取代时 key 会变，
  //      React 卸载旧元素挂新元素 ⇒ 入场动画**重放一次**（同一位置同一个气泡又浮一下）。
  //      加动效之前 key 变化只是静默重挂，谁也看不出来 —— 所以这条只有真点一次才现形。
  //   ④ 标记是**追加**不是覆盖：两条消息在 400ms 窗口内先后到达时，覆盖会把前一条的标记
  //      摘掉、等于在动画途中把 animation 声明撤掉（元素会瞬间跳到终态）。
  //      探针 12.8 用 animationstart 数真实播放次数守 ③。
  useEffect(() => {
    if (mobileView !== "conversation") {
      arriveScopeRef.current = "";
      seenTimelineKeysRef.current = null;
      previousTimelineKeysRef.current = [];
      return;
    }
    const scope = conversation?.id || "";
    const keys = conversationTimeline.map((entry) => entry.key);
    if (arriveScopeRef.current !== scope) {
      arriveScopeRef.current = scope;
      seenTimelineKeysRef.current = new Set(keys);
      previousTimelineKeysRef.current = keys;
      setArrivingKeys([]);
      return;
    }
    const seen = seenTimelineKeysRef.current;
    if (!seen) {
      seenTimelineKeysRef.current = new Set(keys);
      previousTimelineKeysRef.current = keys;
      return;
    }
    const previous = previousTimelineKeysRef.current;
    const currentSet = new Set(keys);
    const fresh = keys.filter((key) => !seen.has(key));
    const gone = previous.filter((key) => !currentSet.has(key));
    for (const key of keys) seen.add(key);
    previousTimelineKeysRef.current = keys;
    // "换 id 不换消息"：消失的**全是**占位、且新增的不比消失的多 ⇒ 这是顶替，不是新消息。
    // 反过来（消失里混了真消息、或新增比消失多）就照常当新到 —— 报错方向宁可是"多动画一次"。
    const replacedPlaceholder = gone.length > 0
      && gone.every((key) => isPlaceholderKey(key))
      && fresh.length > 0
      && fresh.length <= gone.length;
    if (fresh.length > 0 && !replacedPlaceholder) {
      setArrivingKeys((current) => Array.from(new Set([...current, ...fresh])));
    }
  }, [conversationTimeline, mobileView, conversation?.id]);

  // 入场动画一结束就摘掉**这一条**的标记。
  // ⚠️ 原来这里是"setTimeout 400ms 后整体清空"，两个问题（第三轮复查实测抓出来的）：
  //   ① 会误伤"在窗口边缘刚加上"的新标记 —— 前一条的 timer 到期时把后一条刚加上的标记一起带走，
  //      那条的动画要么根本没开始、要么中途被撤掉（animation 声明消失 = 元素瞬间跳到终态）；
  //   ② 异步时序还会和 reduced-motion 的切换纠缠。
  // 按"动画结束"摘就没有窗口：谁跑完谁下榜，互不影响。
  // 兜底：没跑动画的（reduced-motion 下 `animation: none`）标记会留到切会话时清，
  // 一次会话内累积几十个字符串可以忽略。
  const clearArriving = useCallback((key: string) => {
    setArrivingKeys((current) => (current.includes(key) ? current.filter((item) => item !== key) : current));
  }, []);

  const messageActionOpen = conversationTimeline.some((entry) => entry.kind === "message" && entry.message.id === messageActionId);
  const conversationProcessing = Boolean(conversation && (conversation.status === "running" || processingConversations[conversation.id]));
  // ── 状态条的三份读数（徽标 / 阶段词 / 已耗时）────────────────────────────────
  //
  // ⚠️ 时间基准取 `processingConversations[id].startedAt` —— 这个字段原先**只写不读**
  //（见 markConversationProcessing，`'算了但没人读'` 就是一条假装存在的边界）。这里把它
  // 真正消费掉：只在"那一刻真的记了时间"时才把它当锚点，否则交给服务端给的时刻。
  //
  // `startedAt` 只有在**本机亲眼看到这条会话开跑**时才存在。应用在运行中途被杀掉、重开之后
  // 从快照里读到 `status === "running"` 的情况没有这个数（processingConversations 是内存态，
  // 不落盘）。那种情况下绝不能从 0 开始计时 —— 那会显示「3 秒」而它其实已经跑了一小时，
  // 是一句**编出来的读数**。退回服务端的 lastActivityAt：它是这条会话最后一次真实活动的时刻，
  // 虽是下界（可能偏小于真实起点），但它是**有据可查**的。两个都没有就不显示占比。
  const processingSince = conversation
    ? (processingConversations[conversation.id]?.startedAt
      ?? (Date.parse(conversation.lastActivityAt || "") || null))
    : null;
  // 每秒走一格。**挂点必须是 conversationProcessing 而不是消息条数**：只看消息条数的话，
  // AI 想出三十秒而中间不吐字（最常见的一段），读数就整段冻住 —— 那正是最该显示秒数的时刻。
  // 1s 的 interval 在会话页停下时会被依赖清掉（conversationProcessing 转 false）。
  const [processingClock, setProcessingClock] = useState(0);
  useEffect(() => {
    if (!conversationProcessing) return;
    setProcessingClock(Date.now());
    const timer = window.setInterval(() => setProcessingClock(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, [conversationProcessing]);
  // `formatElapsed` 自带 10 秒门槛（且返回空串）。空串 ⇒ 这一格不渲染，
  // 于是"没到 10 秒"与"读不到时间"两种情况在界面上是同一种占位，不需要第二个判据。
  const processingElapsed = processingSince === null ? "" : formatElapsed(processingClock - processingSince);
  // 徽标：agentId → 名字 + 配色档位。fallback 落在 Claude Code（与旧观感一致）。
  const processingAgent = processingBadge(conversation?.agentId ?? "");
  // 阶段词：最新一条运行通知的**标题**（它就是桌面时间线上那句话，手机端不另写枚举
  // —— noticeFromEventFields 已经把 SystemItem 的 title 原样带过来了）。
  // 通知按 createdAt 取最大（不是数组末项，见 latestNotice 里的说明）。
  const processingStage = processingStageText({ notice: latestNotice(conversation?.notices) });
  const conversationMessageCount = conversation?.messages?.length ?? 0;
  // 流式回复只更新最后一条消息的 content、不改变条数，所以还要盯着它的长度，
  // 否则「贴底跟随」在 AI 正在输出时不会生效。
  const lastMessageLength = conversationMessageCount > 0
    ? (conversation?.messages?.[conversationMessageCount - 1]?.content?.length ?? 0)
    : 0;

  // 消息列表不是独立滚动容器（整页滚动），因此监听 window。
  // 「贴近底部」留 100px 容差：足以吸收地址栏收放带来的视口变化，又不会在用户
  // 轻微上滑阅读时把视口拽回底部。
  useEffect(() => {
    const root = document.documentElement;
    const measure = () => {
      stickToBottomRef.current = root.scrollHeight - window.scrollY - window.innerHeight < 100;
    };
    // 软键盘弹起会把 WebView 整体压矮（Manifest 是 adjustResize，输入条因此被顶到键盘上方），
    // 而页面 scrollY 一动不动 —— 文档底部连同最后几条消息一起落进被压掉的那一截里，正好被
    // 输入条和键盘压住。用户明明已经滚到底，点一下输入框还要手动再划一下才看得见最新消息。
    //
    // 所以"视口在变矮"的整段过程里**不重算**「贴底」：此刻"距底距离"凭空多了被压掉的那一截，
    // 重算必然把贴底判成 false —— 那正是要修的症状（旧代码把 measure 直接挂在 resize 上，
    // 于是键盘一弹起，之后新消息、流式回复都不再跟随）。判定保持弹起前的值：
    // 弹起前贴着底的补一次贴底，弹起前在翻历史的一动不动（那正是用户要的位置）。
    //
    // 判据用**累计**矮了多少，而不是这一帧矮了多少：有些机型把键盘弹出拆成几帧下发，单帧
    // 可能只有几十像素，逐帧比大小时每一帧都会被判成"不是键盘"，中间那几帧的 measure 却已经
    // 把贴底顶掉了。settledViewportHeight 只在视口长回来时才往前走，所以它始终是"这一轮
    // 收缩的起点"（= 键盘弹起前的高度）。80px 这条线用来把地址栏收放（五六十像素，整段都
    // 碰不到）排除在补底之外。
    let settledViewportHeight = window.innerHeight;
    const onResize = () => {
      const height = window.innerHeight;
      if (height < settledViewportHeight) {
        if (settledViewportHeight - height > 80 && stickToBottomRef.current) {
          window.scrollTo({ top: root.scrollHeight });
        }
        return;
      }
      settledViewportHeight = height;
      measure();
    };
    measure();
    window.addEventListener("scroll", measure, { passive: true });
    window.addEventListener("resize", onResize);
    return () => {
      window.removeEventListener("scroll", measure);
      window.removeEventListener("resize", onResize);
    };
  }, []);
  // 新消息到达、AI 正在流式输出、或切换会话时，只要用户本就贴着底部就继续跟随；
  // 用户正在向上翻阅历史时不打扰。
  useEffect(() => {
    if (!mobileApp || mobileView !== "conversation") return;
    if (!stickToBottomRef.current) return;
    window.scrollTo({ top: document.documentElement.scrollHeight });
  }, [mobileApp, mobileView, selectedConversation, conversationMessageCount, lastMessageLength, conversationProcessing]);

  // 系统侧滑 / 浏览器返回按钮 / 网页版手势返回：历史已经在后退了，这里只把视图同步回项目列表，
  // **不再动历史**（否则会再多退一层，把用户直接带出应用）。安卓返回键是同一件事的另一条链路，
  // 见下面注册的 backButton 监听。
  // 已知限制（不做对称处理）：用浏览器"前进"回到会话层时不会再自动进入会话视图（手机端没有前进
  // 按钮，桌面端/长按历史列表最多是"少一次反应"）；要对称就得再记一份 selectedProject，收益不抵复杂度。
  useEffect(() => {
    const onPopState = () => {
      if (!conversationHistoryRef.current) return;
      conversationHistoryRef.current = false;
      leaveConversationView();
    };
    window.addEventListener("popstate", onPopState);
    return () => window.removeEventListener("popstate", onPopState);
  }, []);
  // 进程被系统回收后又恢复时，历史里可能留着上一次的会话层标记，而视图已经回到项目列表。
  // 如实把它记成"还没退掉的一层"，这样用户下一次返回就能把它用掉，而不是被空吞一次。
  useEffect(() => {
    conversationHistoryRef.current = window.history.state?.mileviaMobileConversation === true;
  }, []);

  // 安卓物理返回键：与页内「←」保持同一顺序——先关弹层，再退出会话视图，最后最小化应用。
  // 不注册监听时 Capacitor 的默认返回回调会吞掉事件，返回键等于完全失效。
  useEffect(() => {
    backHandlerRef.current = () => {
      // 弹层正在提交时，与弹层内「关闭/取消」按钮的 disabled 规则保持一致：
      // 吞掉返回事件，避免中途丢弃已经在进行的请求。
      if (busy && (editingTask || deletingTask || creatingTask || newConversationProject || confirmShortcut || confirmUnbind)) return true;
      // ⋯ 菜单是页面里最上层的一小块浮层，返回键先收它，别一步退到项目列表。
      if (headerMenuOpen) { setHeaderMenuOpen(false); return true; }
      // 快捷方式确认框与上面两个任务弹层同样处理。漏掉它的症状实测过：返回键一路走到
      // exitConversationView()，视图退回项目列表，而确认框（backdrop 是 position: fixed）
      // 还浮在项目列表上、「执行」仍然可点 —— 用户会看到一条"在项目列表上执行电脑端命令"
      // 的弹窗，而且那一下会真的发出去。
      if (confirmShortcut) { setConfirmShortcut(null); return true; }
      if (editingTask) { setEditingTask(null); return true; }
      if (deletingTask) { setDeletingTask(null); return true; }
      // 「新建任务」是面板之上的一层，必须排在 tasksOpen 之前 —— 排在后面的话返回键会先把
      // 整页面板收掉，弹层却还浮在会话页上（而且它是 position: fixed 的 backdrop）。
      if (creatingTask) { setCreatingTask(false); return true; }
      if (newConversationProject) { cancelNewConversation(); return true; }
      // 「解除绑定」确认框：它和任务弹层一样是 position: fixed 的一层，漏掉这一句
      // 返回键会直接去收下面的配对面板，确认框留在屏上（而且「确认解除」仍然可点）。
      if (confirmUnbind) { setConfirmUnbind(""); return true; }
      // 「备注」弹层是设备面板**之上**的一层（z-index 30 > 面板的 20）。必须排在
      // `devicesOpen` **之前** —— 排在后面时返回键会先把整个面板收掉，而弹层还浮在
      // 项目列表上（与 confirmUnbind 同一条成因，实测过同一个形状）。
      if (aliasTarget) { closeAliasEditor(); return true; }
      // 底部切换面板是 position: fixed 的一层，返回键先收它（它比配对面板更浅）。
      if (devicesOpen) { setDevicesOpen(false); return true; }
      // 取景层有两个渲染条件（scanning / scanError），按扫描态判断会在失败态漏掉它。
      if (scanning || scanError) { closeScanOverlay(); return true; }
      if (tasksOpen) { setTasksOpen(false); return true; }
      // 展开着的消息图标行虽然不是浮层（它就在气泡里、不遮任何东西），返回键也先收它：
      // 返回的语义是"收掉当前这一层动作"，直接退出会话视图等于把用户刚展开的状态丢掉。
      // ⚠️ 判据用 `messageActionOpen`（**界面上真的有这一排**），不能用 `messageActionId`：
      // 后者可能指向一条已经不在快照里的消息，那时这一下返回键会被一个谁也看不见的状态白吃掉。
      // 它排在 tasksOpen 之后：任务面板是 inset:0 的整页层，用户点不到被它盖住的气泡，
      // 两者不可能同时开 —— 这条顺序只是"要是真同时开着，先收整页那一层"。
      if (messageActionOpen) { closeMessageActions(); return true; }
      if (conversationHistoryOpen) { setConversationHistoryOpen(false); return true; }
      if (pairingExpanded) { closePairingPanel(); return true; }
      // 文件面板：先让面板自己退一层（查看器 → 文件列表），它已经到底了才关整个面板。
      // 顺序不能反 —— 反过来用户在查看器里按返回会被直接弹回会话，正在看的文件就没了。
      if (mobileApp && mobileView === "conversation" && filesOpen) {
        if (filesPanelRef.current?.showTree()) return true;
        closeFiles();
        return true;
      }
      // Git 工作台同理，而且更要紧：它的详情层是"正在看的那份 diff"，
      // 顺序反了会把用户从 diff 里直接弹回会话。
      if (mobileApp && mobileView === "conversation" && gitOpen) {
        if (gitPanelRef.current?.showTopLevel()) return true;
        closeGit();
        return true;
      }
      if (mobileApp && mobileView === "conversation") {
        exitConversationView();
        return true;
      }
      return false;
    };
  });
  useEffect(() => {
    if (!Capacitor.isNativePlatform()) return;
    let listener: PluginListenerHandle | null = null;
    let cancelled = false;
    void CapacitorApp.addListener("backButton", () => {
      // 没有可回退的层级时最小化应用（与页内返回按钮一致，符合 Android 根页面返回习惯）。
      if (!backHandlerRef.current()) void CapacitorApp.minimizeApp().catch(() => undefined);
    }).then((handle) => {
      if (cancelled) { void handle.remove(); return; }
      listener = handle;
    }).catch(() => undefined);
    return () => {
      cancelled = true;
      if (listener) void listener.remove();
    };
  }, []);

  // Normalize an older cached snapshot as well as network responses. This
  // keeps revision comparisons numeric during offline recovery.
  useEffect(() => {
    if (!snapshot || Number.isFinite(snapshot.snapshotRevision)) return;
    setSnapshot((current) => current && !Number.isFinite(current.snapshotRevision)
      ? { ...current, snapshotRevision: 0 }
      : current);
  }, [snapshot]);

  // SSE can be unavailable while snapshots continue to refresh. Clear the
  // local optimistic indicator once a completed assistant reply is present.
  useEffect(() => {
    if (!snapshot || Object.keys(processingConversations).length === 0) return;
    const completedIDs = new Set<string>();
    for (const [conversationID, state] of Object.entries(processingConversations)) {
      const remoteConversation = snapshot.projects.flatMap((item) => item.conversations).find((item) => item.id === conversationID);
      if (!remoteConversation) {
        completedIDs.add(conversationID);
        continue;
      }
      if (["failed", "stopped", "cancelled"].includes(remoteConversation.status)) {
        if (snapshot.snapshotRevision > state.snapshotRevision) completedIDs.add(conversationID);
        continue;
      }
      // A run can finish without an assistant message (for example, a
      // startup failure, cancellation, or service restart). Once a newer
      // durable snapshot says the conversation is no longer running, the
      // indicator must not remain stuck. Keep it while the optimistic user
      // message is still waiting to be persisted so a queued command cannot
      // be cleared by an unrelated snapshot update.
      const pending = pendingMessageRef.current.get(conversationID);
      const hasPendingMessage = Boolean(pending && pending.size > 0);
      if (snapshot.snapshotRevision > state.snapshotRevision && remoteConversation.status !== "running" && !hasPendingMessage) completedIDs.add(conversationID);
    }
    if (completedIDs.size > 0) {
      setProcessingConversations((current) => {
        const next = { ...current };
        completedIDs.forEach((id) => delete next[id]);
        return next;
      });
    }
  }, [snapshot, processingConversations]);
  // 搜索命中的任务（还没过分类）。匹配**范围**与电脑端 `TaskQueue` 一致（`queueTasks` 里那句
  // `title.includes(term) || description.includes(term)`）：标题 + 描述、忽略大小写；
  // 不做分词，也不搜状态与优先级（状态有分类栏，优先级不是高频检索项）。
  // ⚠️ 两处与电脑端**有意不同**，别照抄过去：① 两个字段都兜 null（见下）；
  // ② 胶囊计数取自"命中之后"（电脑端 `taskCounts` 仍按全量算，因此它在有搜索词时会出现
  // "胶囊写着 6、点进去是空的"——那是移动端去掉「全部」时明确要避免的那种对不上）。
  const matchedTasks = useMemo(() => {
    const tasks = project?.tasks || [];
    const term = taskQuery.trim().toLowerCase();
    if (!term) return tasks;
    // ⚠️ 两个字段都要兜：`title` 是线协议字段，可能编成 null（见 `taskSummary` 那段说明）。
    // 这一行在**每次按键**上跑遍**队列里的每一条任务**（包含用户当前看不到的那些状态），
    // 所以它比卡片渲染更早、也更容易撞上脏数据 —— 抛在这里同样是整页白屏。
    return tasks.filter((task) => (task.title || "").toLowerCase().includes(term) || (task.description || "").toLowerCase().includes(term));
  }, [project?.tasks, taskQuery]);
  // 分类胶囊上的计数取自**搜索命中之后**的那一批：这样"胶囊说几条、点进去就是几条"这条恒等式
  // 在有搜索词时依然成立（没有搜索词时它与全量完全一致，观感不变）。
  const taskCounts = useMemo(() => Object.fromEntries(taskFilters.map((filter) => [filter.id, matchedTasks.filter((task) => task.status === filter.id).length])) as Record<TaskFilter, number>, [matchedTasks]);
  // 搜索命中里**真的列得出来**的那些（= 被这 5 档覆盖到的）。空态第二句的判据必须是它，
  // 不能直接用 `matchedTasks.length`：那里面还包含 `cancelled / queued` 这类"没有任何分类覆盖"的任务，
  // 它们**点遍 5 档也找不到** —— 这时说"别的分类里有命中"就是骗人（用户会以为是自己没点对档）。
  const listedMatchCount = useMemo(() => taskFilters.reduce((sum, filter) => sum + taskCounts[filter.id], 0), [taskCounts]);
  const visibleTasks = useMemo(() => {
    // 没有「全部」这一档了：列表永远等于"当前分类命中的那些"，两者不可能再对不上。
    return matchedTasks.filter((task) => task.status === taskFilter);
  }, [matchedTasks, taskFilter]);
  // 「已取消」不再是分类，但它必须仍然"报得出数"：去掉「全部」之后，**没有胶囊覆盖的状态**
  // 在界面上就一个入口都没有了 —— 静默消失比多一颗胶囊糟得多（MOBILE-UI 的完备性规则）。
  //
  // ⚠️ 判据不能只认 `cancelled` 这一个字面值。`taskStatusLabel` 上面那段注释写着它专门补了
  // `queued / completed / failed / blocked`，理由是"**服务端可能出现的非规范状态**"，
  // `.mobile-task-status` 里也为此配了色 —— 也就是说这些状态本来是会出现在卡片上的。
  // 它们同样不在这 5 档里：只认 cancelled 的话，这些任务既没有胶囊、也不进这行计数，
  // 就是彻底无声无息。所以这里按"**没有任何分类覆盖**"来数。
  // （对照：⋯ 菜单那颗角标是按 `!= done && != cancelled` 算的，它数得出来、面板里却找不到，
  //   两边一对就是"任务丢了"的观感。）
  const hiddenTaskNote = useMemo(() => {
    const tasks = project?.tasks || [];
    const hidden = tasks.filter((task) => !taskFilters.some((filter) => filter.id === task.status));
    if (hidden.length === 0) return "";
    // 全是「已取消」时说得具体一点（它是唯一一个"被有意去掉"的状态）；混进别的状态时说共性。
    const onlyCancelled = hidden.every((task) => task.status === "cancelled");
    return `另有 ${hidden.length} 条${onlyCancelled ? "已取消的" : "不在以上分类的"}任务未列出`;
  }, [project?.tasks]);
  // 面板头不再有副标题（2026-09-16 按用户要求去掉）："共 9 个任务" / "待验收 0 / 6" 这类数字
  // 与分类胶囊上的计数是同一份信息的第二遍展示，而胶囊就在它下面一行 —— 去掉之后头栏只剩标题。

  // 空列表分三种**够得到的**真相：搜索没命中 / 分类筛出来的空 / 队列本来就空。
  //
  // ⚠️ 判据是**队列总条数**，不是"当前分类是不是全部"（那个判据随「全部」一起没了）。
  // 队列本来就空时必须说"队列里还没有任务"：这时候按分类点名（"「待处理」下没有任务"）会被读成
  // "分类的问题"，而真相是这一整个队列都空着 —— 两者该干的下一件事完全不同。
  //
  // ⚠️ 有搜索词时**前两句都不能说**（2026-09-18 加搜索时定的）：
  // 「「待处理」下没有任务」是假话 —— 队列里可能就有，只是不匹配这个词；
  // 「没有匹配的任务」在**别的分类下命中**时也是假话 —— 用户会以为整个队列里都没有。
  // 所以按"其它分类有没有命中"分成两句，各说各的真相（与"空列表的三种真相必须分开渲染"同一条规则）。
  // 判据用 `listedMatchCount`（列得出来的命中数）而不是 `matchedTasks.length`：后者的口径更宽，
  // 会把"没有任何分类覆盖"的任务也算成"别处有命中"，而那批任务恰恰是点遍 5 档也找不到的。
  //
  // 为什么不写第三种"正在同步 / 同步失败"：面板的渲染条件是
  // `mobileApp && mobileView === "conversation" && project && tasksOpen`，而 project 就是从
  // snapshot 里取的 —— 面板能出现就说明快照已经在手上了，"请求中"这一态在这里**永远到不了**，
  // 写成分支只会得到一段死代码。快照没同步上的情况由页面级那条错误条（"当前显示的是最近一次
  // 同步快照"）与刷新状态条负责，不在这里假装知道。
  //
  // 空态**只给一行标题**（2026-09-16 按用户要求删掉了下面那行说明）：用户要的就是这一句。
  const taskPanelEmpty = useMemo(() => {
    if (visibleTasks.length > 0) return null;
    if ((project?.tasks.length ?? 0) === 0) return { state: "empty" as const, title: "队列里还没有任务" };
    const label = taskFilters.find((item) => item.id === taskFilter)?.label || "";
    const term = taskQuery.trim();
    if (term) {
      return listedMatchCount > 0
        ? { state: "nomatch" as const, title: `「${label}」下没有匹配「${term}」的任务` }
        : { state: "nomatch" as const, title: `没有匹配「${term}」的任务` };
    }
    return { state: "filtered" as const, title: `「${label}」下没有任务` };
  }, [listedMatchCount, project?.tasks.length, taskFilter, taskQuery, visibleTasks.length]);

  // 关掉面板就把「详情」收起、把搜索词清掉：否则下次打开时上一轮展开的那条还是展开的，
  // 而且上一轮的搜索词会**无声地**继续过滤列表 —— 用户看到的是"任务莫名其妙少了"（看不见的筛选器
  // 比看得见的空态糟得多）。用 tasksOpen 做依赖能覆盖所有关闭路径（返回键 / 侧滑 / 关闭键 / 项目消失）。
  useEffect(() => {
    if (!tasksOpen) {
      setExpandedTask("");
      setTaskQuery("");
    }
  }, [tasksOpen]);

  // 「打开创建弹层时清掉上一次的失败原因」这件事**写在加号的 onClick 里**，不要写成
  // `useEffect(() => { if (creatingTask) setCreateError("") }, [creatingTask])`：
  // 失败回滚那条路径也会把 creatingTask 置为 true，而它**正是要**把理由留在弹层里 ——
  // 那个 effect 会在同一次提交之后立刻把 setCreateError(reason) 抹掉，症状是"弹层开着、
  // 输入也回来了，但一句原因都没有"（browser 探针 probe-task-create 的失败态断言抓到过）。
  // 判断"谁开的弹层"是这里的全部内容：用户点的 → 清；回滚开的 → 留着。

  // 整页模态层的焦点管理：打开时把焦点搬进面板、把背后的内容标成 inert（不进 Tab 顺序、读屏忽略），
  // 关闭时还给触发它的按钮。少了这一步，键盘 / 读屏用户打开面板后仍在背后那屏里打转，而视觉上
  // 明明已经"进到面板里了"。inert 在不支持它的旧 WebView 上会被忽略，不影响其余行为。
  // 依赖里必须带上"面板会不会被渲染"的那几个条件（project / mobileView / mobileApp），不能只写
  // tasksOpen：面板的渲染条件是 `mobileApp && mobileView === "conversation" && project && tasksOpen`，
  // 一旦 project 消失或视图切回列表，面板会先于 tasksOpen 离场——若 effect 不重跑，cleanup 就不执行，
  // 背景会永久留在 inert 上（整页看得见、点不动）。
  useEffect(() => {
    if (!tasksOpen) return;
    const panel = taskPanelRef.current;
    if (!panel) return;
    // 触发按钮是菜单里的「任务队列」，点它的同一次渲染里菜单就收起了 —— 到这一步 activeElement 已经
    // 变成 document.body。body 不是一个有意义的落点（focus 它等于没聚焦），必须换成 ⋯ 按钮。
    const active = document.activeElement;
    const restoreTo = active instanceof HTMLElement && active !== document.body ? active : headerMenuButtonRef.current;
    const blocked: HTMLElement[] = [];
    const block = (element: Element | null) => {
      // 面板本身、以及包含面板的祖先都不能 inert —— inert 会让整棵子树失效，包括面板自己。
      if (!(element instanceof HTMLElement) || element === panel || element.contains(panel)) return;
      // 对话框（编辑 / 删除 / 新建任务 / 快捷方式确认）是**盖在面板之上**的那一层，不是背景，
      // 一起 inert 掉就会出现"弹窗看得见、点不动"。这在「新建任务」之前是隐性侥幸：弹层是在
      // inert 那一遍之后才挂上去的，effect 不重跑就没事；一旦重跑（project 换 id 等）就会把它们冻住。
      if (element.matches('[role="dialog"], [role="alertdialog"]')) return;
      element.inert = true;
      blocked.push(element);
    };
    for (const sibling of Array.from(panel.closest("main")?.children || [])) block(sibling);
    for (const sibling of Array.from(panel.parentElement?.children || [])) block(sibling);
    panel.focus({ preventScroll: true });
    return () => {
      for (const element of blocked) element.inert = false;
      if (restoreTo && document.contains(restoreTo)) restoreTo.focus({ preventScroll: true });
    };
  }, [tasksOpen, mobileApp, mobileView, project?.id]);

  // 「我的电脑」面板的焦点管理：与任务面板同一套（搬焦点 + 背景 inert + 关闭还焦点）。
  // 依赖必须带上 mobileApp / mobileView —— 面板的渲染条件是
  // `mobileApp && mobileView === "projects" && devicesOpen`；只挂 devicesOpen 的话，切视图时
  // 面板先离场而 effect 不重跑，cleanup 不执行，背景会永久留在 inert 上（整页看得见、点不动）。
  useEffect(() => {
    if (!devicesOpen) return;
    const panel = devicesSheetRef.current;
    if (!panel) return;
    const active = document.activeElement;
    const restoreTo = active instanceof HTMLElement && active !== document.body ? active : deviceSwitchButtonRef.current;
    const blocked: HTMLElement[] = [];
    const block = (element: Element | null) => {
      if (!(element instanceof HTMLElement) || element === panel || element.contains(panel)) return;
      // 盖在面板之上的对话框（解绑确认框）不是背景：一起 inert 掉会变成"看得见、点不动"。
      if (element.matches('[role="dialog"], [role="alertdialog"]')) return;
      element.inert = true;
      blocked.push(element);
    };
    for (const sibling of Array.from(panel.closest("main")?.children || [])) block(sibling);
    for (const sibling of Array.from(panel.parentElement?.children || [])) block(sibling);
    panel.focus({ preventScroll: true });
    return () => {
      for (const element of blocked) element.inert = false;
      if (restoreTo && document.contains(restoreTo)) restoreTo.focus({ preventScroll: true });
    };
  }, [devicesOpen, mobileApp, mobileView]);

  // 复制反馈的计时器在整页卸载时也要清掉（与 markdown 代码块那颗复制键同一套做法）：
  // 它是个 1.6s 的 setTimeout，流式消息重排 / 热更新都可能让面板先消失。
  useEffect(() => () => {
    if (messageCopyTimerRef.current !== null) window.clearTimeout(messageCopyTimerRef.current);
  }, []);

  // 图标行展开后把焦点交给**第一颗可用**的图标：读屏用户点开「⋯」后会停在原地
  // （那颗按钮还在，只是旁边多了一排），不搬焦点的话他得再往后 Tab 才知道多了什么。
  // 禁用态那颗不接收焦点（`button:not(:disabled)`），否则焦点会落在"按不动的那颗"上。
  // `preventScroll` 是必须的：消息列自己会贴底，聚焦触发一次滚动会把用户看到的这几条顶走。
  useEffect(() => {
    if (!messageActionId) return;
    messageActionsRef.current?.querySelector<HTMLButtonElement>("button:not(:disabled)")?.focus({ preventScroll: true });
  }, [messageActionId]);

  // 图标行的收起：点它之外的任何地方、按 Esc 都收。
  // ⚠️ 用 document 上的 pointerdown，**不铺透明遮罩** —— 遮罩虽然也能"点外部收起"，
  // 但它是 inset:0 的一层，会把消息列的滑动一起吃掉（长按拖动滚不动），
  // 而这里收起只需要一个"按在别处"的信号。顶栏 ⋯ 菜单用的是同一套（见上面那个 effect）。
  // 判定用 `closest(".mobile-message-head-actions")`：图标行与触发它的「⋯」都在这个容器里，
  // 所以"再点一次「⋯」"走的是它自己的 toggle，"点图标行内部"不会误收。
  useEffect(() => {
    if (!messageActionId) return;
    function onPointerDown(event: PointerEvent) {
      const target = event.target;
      if (target instanceof Element && target.closest(".mobile-message-head-actions")) return;
      closeMessageActions();
    }
    function onKeyDown(event: KeyboardEvent) {
      if (event.key !== "Escape") return;
      closeMessageActions();
      // 收起后焦点还给那颗「⋯」。它是气泡的一部分、随气泡重渲染，理论上一直在；
      // 但仍然先问一次 document.contains —— 换会话 / 会话重载后它可能已经不在树里了。
      const trigger = messageActionTriggerRef.current;
      if (trigger && document.contains(trigger)) trigger.focus({ preventScroll: true });
    }
    document.addEventListener("pointerdown", onPointerDown);
    document.addEventListener("keydown", onKeyDown);
    return () => {
      document.removeEventListener("pointerdown", onPointerDown);
      document.removeEventListener("keydown", onKeyDown);
    };
  }, [messageActionId]);

  // 配对页是**用户主动要才出现**的整页浮层，不再自己冒出来。
  // 旧判据 `!token.trim() || instances.length === 0 || pairingExpanded` 有两个问题：
  //  ① 它把这块界面长在项目列表**之上**，把列表整个推下去；
  //  ② "读不到实例列表"（网络抖动 / 电脑临时离线）与"从来没有配过对"被合成同一个条件 ——
  //     前者会让用户看到一块要他重新配对的界面，而他那台电脑只是这一次没应答。
  const showMobilePairing = mobileApp && mobileView === "projects" && pairingExpanded;
  // 从来没有配过对（只认**令牌**）：页面上给一张空态卡，点它才进配对页。
  // 判据刻意不认 `instances.length === 0` —— 那还有"有令牌但这次没读到实例"这一种真相，
  // 该由项目列表的空态与刷新状态条解释，不能栽赃成"还没配对"。
  const showPairingStart = mobileApp && mobileView === "projects" && !pairingExpanded && !token.trim();
  // 三步指示器的当前步。整条流程只有两个可观察的中间态（已提交等电脑确认 / 已确认），
  // 其余（还没提交、提交后失效被回退）都算第 1 步 —— 失效时要把用户带回"再来一次"，
  // 而不是停在第 2 步假装还在等。
  const pairingStep = pairingNotice?.state === "success" ? 3 : pairingNotice?.state === "waiting" ? 2 : 1;
  // 配对状态行**只有一个元素**，但有两处挂载点：配对页里（用户正看着这一页）与页面级
  // （配对页关掉之后它才是唯一的回执）。两处都靠 `pairingNotice` 这一个来源，视觉与文案不重复实现。
  //
  // ⚠️ 页面里那一处必须**显式排掉 success**：绑定成功后 `showMobilePairing` 与这句话是同一次
  // 状态更新里变的（`setPairingExpanded(false)` + `setPairingNotice(success)`），万一哪天有人
  // 只改了其中一处、让成功提示留在页里，它会跟着页面一起卸载 —— 用户什么都看不到
  // （旧版正是这么丢掉"电脑已确认，绑定完成"的，实测过）。
  const pairingNoticeBox = pairingNotice ? <p className={`mobile-pairing-notice mobile-pairing-notice-${pairingNotice.state}`} data-state={pairingNotice.state} role={pairingNotice.state === "error" ? "alert" : "status"}>{pairingNotice.state === "waiting" && <span className="mobile-pairing-notice-spinner" aria-hidden="true" />}<span>{pairingNotice.text}</span></p> : null;
  // 页内那一处挂载点**只服务 idle / error** 两种：
  //  · waiting —— 上面那张等待卡已经把同一句话完整说过一遍（标题 + 说明 + 出口），
  //    再摆一条 12px 的状态行就是同屏说两遍（第一版实测就是这样，两句话隔了 20px 并肩站着）；
  //  · success —— 页面会跟着一起收起（`setPairingExpanded(false)` 与它是同一次更新），
  //    它必须活在页外，否则用户什么都看不到（旧版实测丢过"电脑已确认，绑定完成"）。
  const pairingPageNotice = pairingNotice && (pairingNotice.state === "idle" || pairingNotice.state === "error") ? pairingNoticeBox : null;

  // 配对页（整页浮层）的焦点管理：与任务面板 / 「我的电脑」面板同一套（搬焦点 + 背景 inert +
  // 关闭还焦点）。依赖只挂 showMobilePairing 就够 —— 它已经把 mobileApp / mobileView 包进去了，
  // 而页面离场时 cleanup 必须跑（不然背景会永久留在 inert 上：整页看得见、点不动）。
  useEffect(() => {
    if (!showMobilePairing) return;
    const panel = pairingPageRef.current;
    if (!panel) return;
    const blocked: HTMLElement[] = [];
    const block = (element: Element | null) => {
      if (!(element instanceof HTMLElement) || element === panel || element.contains(panel)) return;
      // 盖在这一页之上的对话框（取景层、任务弹层）不是背景：一起 inert 掉会变成"看得见、点不动"。
      if (element.matches('[role="dialog"], [role="alertdialog"]')) return;
      element.inert = true;
      blocked.push(element);
    };
    for (const sibling of Array.from(panel.closest("main")?.children || [])) block(sibling);
    for (const sibling of Array.from(panel.parentElement?.children || [])) block(sibling);
    panel.focus({ preventScroll: true });
    return () => {
      for (const element of blocked) element.inert = false;
      const restoreTo = pairingTriggerRef.current;
      // ⚠️ 「还给触发它的按钮」只对"主动关闭"成立：从「我的电脑」面板进来时，面板在同一次
      // 点击里就被关掉了，那颗「＋ 添加电脑或重新配对」已经不在文档里 —— 此时焦点只能落 body。
      if (restoreTo && document.contains(restoreTo)) restoreTo.focus({ preventScroll: true });
    };
  }, [showMobilePairing]);

  // 二维码是否已失效：云端给过有效期 + 倒计时走到 0 + 还没确认。**写成派生值而不是
  // "到点把二维码清掉"的 effect** —— 它是纯时间函数，算出来比存起来可靠，也不会出现
  // "谁先谁后"（effect 清掉二维码 vs 用户刚好点确认）。
  const pairingExpired = Boolean(pairingID) && pairingExpiresAt !== "" && pairingSecondsLeft <= 0 && !pairingConfirmed;
  // 当前设备与可用设备数。
  const activeDeviceRecord = devices.find((item) => item.token === token) || activeDevice() || null;
  // 当前电脑的显示名。**备注（本地）优先，其次是云端报回来的真名** —— 两处读数在这里
  // 合成一次就够，别让下面每个渲染点各拼一遍：当前电脑卡读的是 `instance.name`（云端），
  // 而备注在设备表里，分头拼必然漏掉其中一处，症状是"面板里改了备注、卡片上还是老名字"。
  const activeAlias = normalizeAlias(activeDeviceRecord?.alias || "");
  // `instance` 缺位时（冷启动没读回 `/v1/instances`，见下面那张卡的注释）**备注仍然要显示**：
  // 它是纯本地读数，不依赖云端 —— 这正是它比真名更可靠的地方。此时没有真名可回落，
  // 直接给空串，由渲染分支去说"这台电脑的信息没读回来"。
  const activeDeviceName = instance ? deviceDisplayName({ alias: activeAlias, name: instance.name || instance.instanceId }) : activeAlias;
  // 正在改备注的那条记录（弹层要用它的真名做占位与提示）。
  const aliasTargetRecord = devices.find((item) => item.token === aliasTarget) || null;
  // 解绑确认框里点名的那台。**点的是显示名**（备注优先）—— 确认框的作用就是让用户确认
  // "要断的是这一台"，用真名反而可能对不上他刚在列表里看到的那个名字。
  const confirmUnbindRecord = devices.find((item) => item.token === confirmUnbind) || null;
  const usableDevices = devices.filter((item) => !item.revoked);
  // 设备面板的入口按钮文案：≥2 台时面板的主功能是"切电脑" ⇒ 「切换」；
  // 只有一台时进去只能添加/重新配对/解绑 ⇒ 「管理」。**文案跟着主功能走**，
  // 单台也保留这颗按钮 —— 否则只有一台电脑的用户没有任何入口去重新配对或解绑。
  const devicePanelLabel = usableDevices.length >= 2 ? "切换" : "管理";
  const pairingCountdown = countdownText(pairingSecondsLeft);
  // 取景层的画面来源由平台决定：原生是"WebView 之下的摄像头"（页面里画不了，只能靠清底透出来），
  // Web 是页面内的 <video>。前者决定 <video> 是否渲染，后者与 scanLiveNative 一起决定 data-backdrop。
  const nativeScanPlatform = Capacitor.isNativePlatform();
  // 只有"原生 + 正在扫描"这一种组合背后真的有摄像头（data-backdrop="camera"）。其余情况
  // （Web 分支、以及相机起不来之后的报错态）背后没有画面，取景层必须自己铺一层不透明底
  // —— 否则清掉根元素底色之后会直接露出浏览器画布。
  const scanLiveNative = scanning && nativeScanPlatform;

  useEffect(() => {
    if (!selectedProject || projects.some((item) => item.id === selectedProject)) return;
    setSelectedProject("");
    setSelectedConversation("");
    exitConversationView();
  }, [projects, selectedProject]);
  useEffect(() => {
    // ⚠️ 新建会话那条链路上，`selectedConversation` 会从**本地临时 id** 换成电脑端**真 id**
    // （见 resolvePendingConversation）。那一次不是"换会话"：用户还停在同一条会话上，
    // 输入框里正打的草稿、挂着的技能胶囊都该原样留着 —— 否则"新建会话之后立刻打字"
    // 这个最常见的动作会被一条后台回执无声地清掉。
    //
    // 标记**不在这里消费**（不写成"读一次就清空"）：StrictMode 与 Fast Refresh 都会把
    // effect 多跑一遍，消费式的写法第二遍就会误判成换会话。标记只在"这次的选择确实不是
    // 那一次替换"时才清掉，所以重复执行的结果一致。
    const swap = conversationIDSwapRef.current;
    const isIDSwap = Boolean(swap && swap.projectId === selectedProject && swap.to === selectedConversation);
    if (!isIDSwap) conversationIDSwapRef.current = null;
    if (isIDSwap) return;
    setMessageDraft("");
    // 技能引用与草稿同一条规则：换会话 / 换项目就清掉。引用属于原来那个会话，
    // 留着会变成"新会话的输入框上挂着上一个会话的技能"，而且它比草稿更容易被忽略
    // （草稿至少还看得出内容不对）。
    setSkillRefs([]);
    // 被引用的消息属于原来那条会话：留着会变成"新会话的输入框上挂着上一个会话的某条消息"，
    // 而它比技能胶囊更容易被忽略 —— 胶囊上写着技能名，引用胶囊上写着的是**新会话里也有**的一句话。
    setQuoteRef(null);
    // 展开着的图标行同样按 id 记着"另一条会话里的某条消息"。不清的话，返回键会被一次
    // "谁也收不掉"的状态吃掉一次（图标行已经因为找不到那条消息而不渲染了，状态却还在）。
    closeMessageActions();
    setConversationHistoryOpen(false);
    setComposerToolsOpen(false);
    // 换会话（含从历史会话里点另一条）时，顶栏 ⋯ 菜单里的「历史会话 N / 任务队列 N」都是旧项目的
    // 计数，留着会显示成另一条会话的数字 —— 一并收起，让用户重新看一眼当前值。
    setHeaderMenuOpen(false);
    // 文件面板是**会话视图的子态**，而它绑的是"哪条会话的哪个工作区"（适配器按
    // conversation.id 建、缓存也跟着那条会话）。换了会话/项目还留着它，用户会看到
    // 另一条会话的文件树；而它一旦因为 `project` 暂时为空而不渲染（令牌失效那条链
    // 会把 selectedProject 清空），下次进任何一个项目它又会自己冒出来 ——
    // 状态还在，面板就还在。守卫同样要清：它注册的是那个即将卸载的面板。
    // Git 视图同理：它绑的更是"哪个工作区"（切错工作区就是切错分支），而且适配器
    // 手里还攥着那条会话的 stateToken。一个槽位一起收。
    setSessionSub(null);
    setFilesInitialPath(null);
    filesGuardRef.current = null;
  }, [selectedProject, selectedConversation]);
  useEffect(() => {
    selectedConversationRef.current = selectedConversation;
  }, [selectedConversation]);
  // 自增高上限必须与 CSS 的 max-height 一致（两者分叉会让输入框在某一侧被裁）。
  // ＋2 是取整余量：scrollHeight 取整后可能比真实内容矮不到 1px，直接当高度用会让最后一行贴边。
  useEffect(() => {
    const input = messageInputRef.current;
    if (!input) return;
    input.style.height = "auto";
    input.style.height = `${Math.min(input.scrollHeight + 2, 140)}px`;
  }, [messageDraft]);
  // 面板外的任意按下都收起面板（输入条与面板同属一个外壳，点它们不算"外面"）。
  useEffect(() => {
    if (!composerToolsOpen) return;
    function onPointerDown(event: PointerEvent) {
      const target = event.target;
      if (target instanceof Element && target.closest(".mobile-composer-shell")) return;
      setComposerToolsOpen(false);
    }
    document.addEventListener("pointerdown", onPointerDown);
    return () => document.removeEventListener("pointerdown", onPointerDown);
  }, [composerToolsOpen]);
  // 顶栏 ⋯ 菜单：面板外的任意按下、或按 Esc 都收起（与输入条工具面板同一套规则）。
  useEffect(() => {
    if (!headerMenuOpen) return;
    function onPointerDown(event: PointerEvent) {
      const target = event.target;
      if (target instanceof Element && target.closest(".mobile-header-menu")) return;
      setHeaderMenuOpen(false);
    }
    function onKeyDown(event: KeyboardEvent) {
      if (event.key !== "Escape") return;
      setHeaderMenuOpen(false);
      // 收起后焦点必须回到 ⋯：不还焦点，键盘用户会掉在 document.body 上，得从头 Tab 一遍。
      headerMenuButtonRef.current?.focus();
    }
    document.addEventListener("pointerdown", onPointerDown);
    document.addEventListener("keydown", onKeyDown);
    return () => {
      document.removeEventListener("pointerdown", onPointerDown);
      document.removeEventListener("keydown", onKeyDown);
    };
  }, [headerMenuOpen]);
  // 会话内容的底部预留要跟着输入条实际高度走：写死一个数值在多行输入（输入条能长到 158px）
  // 或打开工具面板时会失效，最后几条消息被压住。这里把实测高度写进 CSS 变量供样式消费。
  useEffect(() => {
    const shell = composerShellRef.current;
    if (!shell) return;
    let lastHeight = 0;
    const apply = () => {
      const height = Math.ceil(shell.getBoundingClientRect().height);
      document.documentElement.style.setProperty("--mobile-composer-height", `${height}px`);
      // 输入条长高（多行输入、展开工具面板）时，文档底部预留虽然跟着变大，但浏览器会保持
      // 原来的 scrollY —— 于是最后几条消息被推到输入条底下压住。只在"本来就贴着底"时
      // 补一次贴底，正在往上翻的用户不打扰。
      //
      // 空态下**不补**：那里根本没有"底部"可粘，而空态卡本身是「在顶栏与输入条之间居中」的，
      // 一补就把整页往上推几十像素、卡片顶被顶栏切掉（展开工具面板时实测 33px）。
      if (height > lastHeight && stickToBottomRef.current && !emptyThreadRef.current) {
        window.scrollTo({ top: document.documentElement.scrollHeight });
      }
      lastHeight = height;
    };
    apply();
    const observer = new ResizeObserver(apply);
    observer.observe(shell);
    return () => {
      observer.disconnect();
      document.documentElement.style.removeProperty("--mobile-composer-height");
    };
  }, [mobileView, project?.id]);
  // 刷新状态条钉在冻结顶栏正下方，而顶栏高度含安全区、随设备与字号变 —— 同一个理由，
  // 量出来写进 CSS 变量，别在样式里写死一个 58px（刘海一变就对不上）。
  useEffect(() => {
    const header = headerRef.current;
    if (!header) return;
    const apply = () => {
      document.documentElement.style.setProperty("--mobile-header-height", `${Math.ceil(header.getBoundingClientRect().height)}px`);
    };
    apply();
    const observer = new ResizeObserver(apply);
    observer.observe(header);
    return () => {
      observer.disconnect();
      document.documentElement.style.removeProperty("--mobile-header-height");
    };
  }, [mobileView, project?.id]);
  // 刷新状态条是"一次性回执"，不是常驻读数：钉到顶栏下面之后它就会一直占着那 ~40px
  // （顶栏当初折成一行省下来的正是这块地方），所以**只在会话视图**里让终态自己收掉。
  // "正在刷新…"（at 为 null）不收 —— 那时候还没有结果可看。项目列表页保持原样：
  // 那一页的刷新按钮本来就在文档顶部，状态条不会被滚出视口。
  useEffect(() => {
    if (mobileView !== "conversation") return;
    if (!refreshStatus?.at) return;
    const timer = window.setTimeout(() => setRefreshStatus(null), REMOTE_REFRESH_STATUS_TTL_MS);
    return () => window.clearTimeout(timer);
  }, [mobileView, refreshStatus?.at]);

  // 电脑端的「刷新状态」。**不能复用 refreshNow**：那条链路是给手机写的 ——
  // 它去拉 /v1/instances 与云端快照，而电脑端没有云端令牌，于是必然走到
  // `token.trim() ? … : "尚未配对电脑，请先扫码配对"` 那一支。电脑端不可能"扫码配对"，
  // 它自己就是被配对的那台机器，用户照做只会更糊涂（实测就是这个文案）。
  // 这一屏的"刷新"只该重读本机那两个接口：远程服务状态 + 当前绑定的手机。
  async function refreshDesktopStatus() {
    if (refreshingRef.current) return;
    refreshingRef.current = true;
    setRefreshing(true);
    setRefreshStatus({ state: "refreshing", message: "正在读取本机服务状态…", at: null });
    try {
      const outcome = await loadAgentStatus();
      await loadBindings();
      // 文案在纯模块里（三态分开，且都保证不说手机那一侧的话）—— 见 desktop-service.ts。
      setRefreshStatus({ ...refreshDesktopStatusMessage(outcome), at: new Date() });
    } catch {
      setRefreshStatus({ state: "failed", message: "刷新失败：本机服务没有响应", at: new Date() });
    } finally {
      refreshingRef.current = false;
      setRefreshing(false);
    }
  }

  // 「刷新」按钮。三步都必须做，少一步用户就看不出有没有生效：
  //   ① 立刻把按钮置为"转圈 + 禁用"，让点击当场有反应；
  //   ② 真的回云端取一份（loadSnapshot 的 force 分支绕过 revision 短路）；
  //   ③ 把结果写成一条带时刻的状态条——成功/无变化/失败各有各的说法，不再是一点动静都没有。
  async function refreshNow() {
    // 双击只当一次：重复请求不但白费流量，两条重叠的反馈还会互相覆盖。
    if (refreshingRef.current) return;
    refreshingRef.current = true;
    setRefreshing(true);
    setRefreshStatus({ state: "refreshing", message: "正在刷新…", at: null });
    try {
      const list = await loadInstances();
      if (list.outcome === "failed") {
        // 带上真实原因（网络不通 / 令牌失效 / 云端返回异常），别再写死一句"没有连上云端"。
        setRefreshStatus({ state: "failed", message: `刷新失败：${list.reason || "请稍后重试"}`, at: new Date() });
        return;
      }
      // 与 loadInstances 选实例的规则保持一致（没变就留着，失效了就退回第一台）：
      // 两处规则一旦分叉，刷新就会去请求一台已经不是当前选中的电脑。
      // superseded 时拿不到这一趟的列表（结果由抢答的那次请求落地），退回当前选中的实例继续。
      const known = list.outcome === "ok" ? list.instances : null;
      const target = known
        ? (known.some((item) => item.instanceId === instanceID) ? instanceID : known[0]?.instanceId || "")
        : instanceID;
      if (!target) {
        if (list.outcome === "superseded") {
          // 列表这一趟没落地、当前也没有选中的实例：接下来的自动同步会把列表与快照补齐，
          // 这里既不能报成功也不能报失败。
          setRefreshStatus({ state: "unchanged", message: "电脑列表已更新，正在同步数据…", at: new Date() });
          return;
        }
        setRefreshStatus({
          state: "failed",
          message: token.trim() ? "没有找到在线的电脑，请确认电脑端已启动并联网" : "尚未配对电脑，请先扫码配对",
          at: new Date(),
        });
        return;
      }
      const result = await loadSnapshot({ force: true, instanceId: target });
      if (result.outcome === "updated") {
        setRefreshStatus({ state: "success", message: `已刷新 · ${result.snapshot?.projects.length ?? 0} 个项目`, at: new Date() });
      } else if (result.outcome === "unchanged") {
        setRefreshStatus({ state: "unchanged", message: "已是最新，电脑端没有新的变化", at: new Date() });
      } else if (result.outcome === "failed") {
        setRefreshStatus({ state: "failed", message: `刷新失败：${result.reason || "请稍后重试"}`, at: new Date() });
      } else {
        // skipped：这次请求被更新的请求取代了，界面会由那一次的结果收尾。
        setRefreshStatus({ state: "unchanged", message: "已更新电脑列表，正在同步数据…", at: new Date() });
      }
    } catch {
      // loadInstances / loadSnapshot 内部都各自兜住了异常，这里是最后一道保险：
      // 万一有意外抛出也必须把状态条收尾，否则按钮会永远停在"正在刷新…"的转圈状态——
      // 那比原来的"点了没反应"更糟。
      setRefreshStatus({ state: "failed", message: "刷新失败：请稍后重试", at: new Date() });
    } finally {
      refreshingRef.current = false;
      setRefreshing(false);
    }
  }

  // ⚠️ 这里原来有一个 `saveToken(event)`：手动粘贴令牌的入口。
  // 它在 2026-09-20 的可用性排查里被判为死代码 —— 绑定的表单早就不存在了，全仓没有任何调用点
  // （配对只剩"扫码"与"6 位校验码"两条路）。删掉而不是留着：留着它会让人以为界面上还有这个入口。
  // 要恢复的话照这三件事写：`addOrReplaceDevice({ token, instanceId: "" })` 进设备表 →
  // `setInstances([])` 等第一次 /v1/instances 回来补电脑名 → `void loadInstances()`。

  async function enableMobileNotifications() {
    if (typeof Notification === "undefined") return;
    try {
      const permission = await Notification.requestPermission();
      setNotificationPermission(permission);
    } catch {
      setError("无法请求通知权限，请在浏览器或系统设置中允许通知");
    }
  }

  async function claimPairing(pairingIDValue = pairingID, pairingCodeValue = pairingCode) {
    if (!pairingIDValue.trim() || !/^\d{6}$/.test(pairingCodeValue.trim())) {
      setError("二维码缺少有效的配对信息，请让电脑重新生成二维码");
      return;
    }
    setBusy(true); setError("");
    try {
      const result = await cloud<{ instanceId: string; status: string }>(`/v1/pairings/${encodeURIComponent(pairingIDValue.trim())}/claim`, { method: "POST", body: JSON.stringify({ code: pairingCodeValue.trim(), deviceName: deviceLabel(), platform: devicePlatform(Capacitor.getPlatform()) }) });
      const accessToken = (result as { accessToken?: string }).accessToken;
      if (accessToken) setPendingAccessToken(accessToken);
      // 把配对页一起打开：**深链**进来的人（用系统相机扫电脑上的二维码 → 打开这个 URL）
      // 此刻已经在流程中间了，他要看的是"第 2 步 · 等待电脑确认"，而不是项目列表上一条孤零零的回执。
      // 在应用内扫码那条路上这一句是 no-op（页面本来就开着），所以放在这里而不是调用点 ——
      // 两个入口共用同一条链，也就不会再出现"某一条忘了开页"。
      // 放在**提交成功之后**（而不是进函数就开）：提交在路上时露出第 1 步那颗可点的「扫描二维码」
      // 会让人以为还能重来一次。
      setPairingExpanded(true);
      setPairingNotice({ text: "已扫码并提交，请在电脑上点击「确认绑定」", state: "waiting" });
    } catch (cause) { setError(cause instanceof Error ? cause.message : "配对失败"); }
    finally { setBusy(false); }
  }

  // 「扫描二维码」的唯一入口。五次复位必须写在同一个地方：少复位一项，复用上一次的
  // 结果就会串到这一次上（最典型的是 scanAccepted 残留 —— 相机起来后会对任何一张
  // 二维码都判"已经接受过"，看起来像扫码没反应）。
  function beginPairingScan() {
    scanAccepted.current = false;
    setScanError("");
    setScanNeedsSettings(false);
    setScanTorchAvailable(false);
    setScanTorchOn(false);
    setScanning(true);
  }

  // 关掉取景层。scanError 必须一起清掉：它是取景层的第二个渲染条件（失败态要留在层里），
  // 不清就会关不掉。浮层清退的两处（返回键 / leaveConversationView）也走这一个出口。
  //
  // 这里**同步**撤一次清底类名，不等 effect 清理：类名归上面那个 effect 拥有，而它的清理是
  // passive 的（可能落在下一次绘制之后），中间那一帧会出现"取景层已摘掉、页面还被
  // visibility: hidden 盖着"的空屏 —— 原生分支上还会再多闪一下摄像头画面。
  // effect 里那句 setScannerActive 仍是权威（覆盖超时/卸载等路径），这里只是把它提前一拍。
  function closeScanOverlay() {
    setScannerActive(false);
    setScanning(false);
    setScanError("");
  }

  // 「打开系统设置」按钮只在原生分支出现（scanNeedsSettings 只由原生分支置位）——
  // 浏览器里"系统设置"没有对应入口，提示语本身已经写了该去浏览器站点设置里改。
  // 这里的 catch 因此是**防御性**的：真要在 Web 上调到 openSettings()，插件会抛
  // "This method is not implemented on web."，不该把这句英文丢给用户。
  async function openScannerSettings() {
    try {
      await BarcodeScanner.openSettings();
    } catch {
      setScanError("无法自动打开系统设置，请到「设置 → 应用 → Milevia → 权限」中允许使用摄像头");
    }
  }

  // 手电筒开关。判据只能用"回读到的真实状态"，**不能**用"调用有没有抛异常"：
  // 插件实现里 `enableTorch()` / `disableTorch()` 在相机尚未就绪时（`camera == null`，
  // 见 BarcodeScanner.java）是**静默 return** 的 —— 不抛异常、正常 resolve，但灯没亮。
  // 按"没抛就算成功"来置状态，界面就会显示"手电筒已打开"而实际是黑的。
  // 所以用 `toggleTorch()` 发声、再 `isTorchEnabled()` 回读；回读与切换前相同即说明这次
  // 切换没生效（相机还没就绪），此时把按钮收起来 —— 与"设备没有闪光灯"一样处理，
  // 不留一个点了没反应的开关。
  async function toggleScanTorch() {
    try {
      await BarcodeScanner.toggleTorch();
      const { enabled } = await BarcodeScanner.isTorchEnabled();
      if (Boolean(enabled) === scanTorchOn) {
        setScanTorchAvailable(false);
        setScanTorchOn(false);
        return;
      }
      setScanTorchOn(Boolean(enabled));
    } catch {
      // 真抛异常（相机已停等）走这里，处理同上。
      setScanTorchAvailable(false);
      setScanTorchOn(false);
    }
  }

  useEffect(() => {
    const { pairingID: initialPairingID, code: initialPairingCode } = initialPairing.current;
    if (!mobileApp || initialPairing.current.attempted || !initialPairingID || !/^\d{6}$/.test(initialPairingCode)) return;
    initialPairing.current.attempted = true;
    void claimPairing(initialPairingID, initialPairingCode);
  }, [mobileApp]);

  async function claimPairingByCode(event: FormEvent) {
    event.preventDefault();
    const code = manualPairingCode.trim();
    if (!/^\d{6}$/.test(code)) {
      setError("请输入 6 位校验码");
      return;
    }
    setBusy(true); setError("");
    try {
      const result = await cloud<{ pairingId: string; instanceId: string; status: string; accessToken?: string }>("/v1/pairings/claim", { method: "POST", body: JSON.stringify({ code, deviceName: deviceLabel(), platform: devicePlatform(Capacitor.getPlatform()) }) });
      setPairingID(result.pairingId || "");
      setPairingCode(code);
      if (result.accessToken) setPendingAccessToken(result.accessToken);
      setPairingNotice({ text: "校验码已提交，等待电脑确认", state: "waiting" });
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "校验码配对失败");
    } finally {
      setBusy(false);
    }
  }

  async function confirmDesktopPairing() {
    if (!pairingID.trim()) return;
    setBusy(true); setError("");
    try {
      await api(`/api/remote/pairing/confirm`, { method: "POST", body: JSON.stringify({ pairingId: pairingID.trim() }) });
      setPairingConfirmed(true);
      setPairingReadyForConfirm(false);
      setPairingNotice({ text: "已确认绑定，手机可以开始使用", state: "success" });
      // 确认的同时云端把上一台手机顶掉了，所以"当前绑定的手机"必须重读，
      // 否则卡片上写着的还是旧那台，用户会以为换绑没生效。
      void loadBindings();
    } catch (cause) { setError(cause instanceof Error ? cause.message : "确认绑定失败"); }
    finally { setBusy(false); }
  }

  // 注册是电脑端的一次性部署动作：令牌只注入本次 Agent 子进程，注册成功后凭据
  // 由 DPAPI 保存，令牌既不落盘也不会进入后续启动环境。
  // 「打开数据目录」：排障要看 milevia-agent.log，而它在应用数据目录下。
  // 这个命令设置页已经在用（SettingsPage 的存储用量那一块），不新开通道。
  async function openAgentDataDirectory() {
    if (!isDesktop()) return;
    try {
      await invoke("open_app_data_directory");
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "无法打开应用数据目录");
    }
  }

  async function enrollRemoteAgent(event: FormEvent) {
    event.preventDefault();
    const token = agentEnrollToken.trim();
    if (!token) return;
    if (!isDesktop()) {
      setAgentEnrollMessage("请在 Milevia 桌面应用中完成注册。");
      return;
    }
    setAgentEnrollBusy(true);
    setAgentEnrollMessage("");
    // 收掉上一次留下的终态回执（可能还是好几轮之前那句"已就绪"）——不然它会和
    // 本次注册过程中的提示同时挂在屏幕上，看起来像刚出的结果。
    setPairingNotice(null);
    try {
      await invoke("enroll_remote_agent", { enrollmentToken: token });
      setAgentEnrollToken("");
      setAgentEnrollMessage("已提交注册，正在等待电脑端连接云端……");
      setAgentEnrollWaiting(true);
    } catch (cause) {
      setAgentEnrollMessage(cause instanceof Error ? cause.message : String(cause));
    } finally {
      setAgentEnrollBusy(false);
    }
  }

  async function createDesktopPairing() {
    setBusy(true); setError("");
    try {
      const value = await api<{ pairingId: string; code: string; pairingURL?: string; expiresAt?: string }>("/api/remote/pairing", { method: "POST" });
      // 校验码是"同步生成、随即就要进二维码"的：这里少传一次，手机扫到码之后就只能
      // 退回手输校验码（见 pairingURLWithCode 的注释）。
      const qrURL = pairingURLWithCode(value.pairingURL, value.pairingId || "", value.code || "");
      setPairingID(value.pairingId || "");
      setPairingCode(value.code || "");
      setPairingURL(qrURL);
      // 有效期与二维码同一次落地：分开写会让倒计时先渲染出 "0:00"，二维码闪一帧"已失效"。
      setPairingExpiresAt(value.expiresAt || "");
      setPairingSecondsLeft(secondsUntil(value.expiresAt));
      setPairingCopied(false);
      setPairingReadyForConfirm(false);
      setPairingConfirmed(false);
      if (qrURL) {
        setPairingNotice({ text: "二维码已生成，请用手机扫码；在本机点击「确认绑定」后完成", state: "idle" });
      } else {
        // 云端未配置公网地址时只能使用校验码。明确说出来，而不是留一块
        // 空白让用户反复点击。
        setPairingNotice({ text: "已生成校验码，请在手机上输入这 6 位数字", state: "idle" });
        setError("云端未配置公网地址（MILEVIA_CLOUD_APP_URL），二维码不可用；请改用校验码配对。");
      }
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "无法生成配对二维码");
      // 最常见的原因是 Agent 还没注册，顺手刷新一次状态以便页面给出正确引导。
      void loadAgentStatus();
    }
    finally { setBusy(false); }
  }

  // 解除当前手机的绑定：先请云端吊销这台设备在该实例上的令牌，再清理本地
  // 状态。云端不可达时仍然完成本地清理——用户的意图是让这台手机停止访问，
  // 而不是修好一次网络请求，所以本地解绑不能依赖请求成功。
  async function unbindDevice() {
    const target = instanceID.trim();
    setBusy(true); setError("");
    try {
      if (target) {
        await cloud(`/v1/instances/${encodeURIComponent(target)}/revoke`, {
          method: "POST",
          body: JSON.stringify({ scope: "mobile" }),
        });
      }
    } catch {
      // 忽略云端错误：本地已经解除绑定，用户随时可以重新配对。
    } finally {
      // 只摘掉**当前这台**：手机上的其它电脑不受影响（旧实现把唯一的键删掉，
      // 多设备之后那等于把全部配对一起清空）。
      removeDevice(readActiveToken());
      setDevices(readDevices());
      // ⚠️ 当前设备必须**回读**，不能写死 `setToken("")`：`removeDevice` 已经顺手把"当前设备"
      // 顺移到下一台**可用**的电脑（见 mobile-devices.ts 那句 `setActiveToken(next.find(...))`）。
      // 写死会让组件状态与 storage 打架 —— store 里当前设备是 B，而 `token` 是空串，
      // 页面于是按"还没配对"渲染出首启空态卡（"还没有连接电脑 / ＋ 添加电脑"），
      // 用户会以为所有配对一起没了。（2026-09-20 修：这条以前只是让内联配对块多显示一次，
      // 首启空态卡上线之后它变成了一块**说假话的屏**。）
      const nextToken = readActiveToken();
      setToken(nextToken);
      setInstances([]); setInstanceID(""); setSnapshot(null);
      setSelectedProject(""); setSelectedConversation("");
      setPairingURL(""); setPairingCode(""); setPairingID(""); setPairingQR("");
      setPairingExpiresAt(""); setPairingSecondsLeft(0); setPairingCopied(false);
      setPairingReadyForConfirm(false); setPairingConfirmed(false);
      // 还有别的电脑可用时**不要**弹配对页：那不是"必须重新配对"，只是这台没了。
      // 一台都不剩时才把配对页叫起来 —— 否则用户面对的是"没有电脑、也没有入口"。
      setPairingNotice({ text: nextToken ? "已解除绑定，已切到另一台电脑" : "已解除绑定，请重新配对", state: "idle" });
      setPairingExpanded(!nextToken);
      // 要弹配对页的那一支同样得先收掉设备面板（同层 + 面板在 DOM 里更靠后 ⇒ 会盖住配对页）；
      // 还有别的电脑可用时不弹页，面板留着让用户接着挑。
      if (!nextToken) setDevicesOpen(false);
      // 顺移之后要重新拉一次实例：设备卡的渲染条件是 `instance` 存在，而设备面板只能从那张卡进，
      // 不拉的话用户既看不到卡、也没有入口去切别的电脑（`probe-scan-overlay` 的多设备夹具覆盖这条）。
      if (nextToken) void loadInstances();
      setBusy(false);
    }
  }

  // 「解除绑定」是一次不可逆动作（云端吊销这台手机在该实例上的令牌），所以按钮只负责
  // 把确认框叫起来，真正干活的是确认框里的那一下。
  function requestUnbind(deviceToken: string) {
    setConfirmUnbind(deviceToken);
  }

  // 「备注」弹层的开/关。名字只存在这台手机上（见 mobile-devices 里 `alias` 那段），
  // 所以**没有请求、没有 busy、没有失败分支** —— 打开时把当前值灌进草稿，保存时写回 storage。
  function openAliasEditor(deviceToken: string) {
    const record = readDevices().find((item) => item.token === deviceToken);
    setAliasDraft(normalizeAlias(record?.alias || ""));
    setAliasTarget(deviceToken);
  }

  function closeAliasEditor() {
    setAliasTarget("");
    setAliasDraft("");
  }

  // 保存与清除走同一条路：`setDeviceAlias` 里把"什么算空"收成了一处
  // （全空格 = 清除 = 回落真名），这里不再判断一遍。
  function saveAlias(event: FormEvent) {
    event.preventDefault();
    if (!aliasTarget) return;
    setDeviceAlias(aliasTarget, aliasDraft);
    setDevices(readDevices());
    closeAliasEditor();
  }

  async function confirmUnbindDevice() {
    const target = confirmUnbind;
    setConfirmUnbind("");
    if (!target) return;
    if (target === readActiveToken()) {
      await unbindDevice();
      return;
    }
    await unbindDeviceByToken(target);
  }

  // 解绑一台**不是当前**的电脑：它有自己的令牌，所以吊销请求要带上它自己的凭据。
  // 云端不可达时仍然完成本地摘除 —— 用户的意图是"让这台手机不再连着它"，
  // 而不是修好一次网络请求（与 unbindDevice 同一条原则）。
  async function unbindDeviceByToken(deviceToken: string) {
    const device = readDevices().find((item) => item.token === deviceToken);
    setBusy(true); setError("");
    try {
      if (device?.instanceId) {
        await cloud(`/v1/instances/${encodeURIComponent(device.instanceId)}/revoke`, { method: "POST", body: JSON.stringify({ scope: "mobile" }) }, deviceToken);
      }
    } catch {
      // 忽略云端错误：本地摘掉即可，用户随时可以重新扫码。
    } finally {
      removeDevice(deviceToken);
      setDevices(readDevices());
      setBusy(false);
    }
  }

  // 打开配对页（「＋ 添加电脑或重新配对」/ 首启空态卡）。顺手把上一轮留下的两样东西清掉：
  //  ① 状态行 —— 上一次失败的 "配对已失效" 跟这一次没有任何关系，留着只会让用户以为又失败了；
  //  ② 六格里那个 6 位校验码 —— 校验码是一次性的，留着它下一次打开时格子是满的、提交键是亮的，
  //     用户会以为"已经填好了"，点下去只会拿到一句"配对已失效"。
  // ⚠️ 清状态行同时还有一条**结构性**作用：这一页必须总是从第 1 步打开。若把上一轮那个
  //    还没被电脑确认的 waiting 留在里头，页面会直接停在第 2 步 —— 而那一屏上**没有任何回到
  //    "扫码 / 输码"的出口**（唯一的按钮是「先返回项目」），用户就被困在那里等它过期。
  // 同时记住"是谁打开的"：关闭时焦点要还回去（见下面那个焦点管理 effect）。
  function openPairingPanel() {
    const active = document.activeElement;
    pairingTriggerRef.current = active instanceof HTMLElement && active !== document.body ? active : null;
    setPairingNotice(null);
    setManualPairingCode("");
    // ⚠️ 进配对页之前必须把「我的电脑」面板收掉：两者都是 `position: fixed` 且**同为 z-index 20**，
    // 而面板在 DOM 里更靠后 —— 同时开着的话面板会盖在配对页上面（用户以为"点了没反应"，
    // 其实页面就在下面）。写在这个共用出口里而不是各个调用点：调用点有面板底部那颗按钮、
    // 首启空态卡、还有「令牌失效」那条（它不走这里，单独收），漏一个就是一块点不动的死屏。
    setDevicesOpen(false);
    setPairingExpanded(true);
  }

  // 收起配对页。状态行**只有"等待电脑确认"那条要留下来**：用户回到项目列表之后，
  // 页面级那条"已提交，等待电脑确认"是他唯一的进度指示 —— 收掉它，屏幕上就什么都没有了，
  // 而配对其实还在进行（电脑那边点确认依然生效）。
  // 其余三种（还没提交 / 失效 / 已经成功）都该跟着页面一起收，否则下次开页会看到上一轮的结果。
  function closePairingPanel() {
    setPairingExpanded(false);
    setPairingNotice((current) => (current?.state === "waiting" ? current : null));
  }

  // ── 多电脑：切换当前设备 + 逐台探测 ──────────────────────────────────────
  //
  // 云端一个令牌只能问自己那台电脑（`listInstances` 带 scope 时 `where instance_id=$1`），
  // 所以"别的电脑在不在线"只能**逐台用自己的令牌探测**。N 很小（手机一般连 2~5 台），
  // 因此不新增聚合接口 —— 把 N 个长期凭据塞进一个请求体去换"一次拿全"，不划算。
  const probeOtherDevices = useCallback(async () => {
    const others = readDevices().filter((item) => !item.revoked && item.token !== readActiveToken());
    if (others.length === 0) return;
    await Promise.all(others.map(async (item) => {
      try {
        const result = await cloud<Instance[]>("/v1/instances", undefined, item.token);
        const info = (Array.isArray(result) ? result : [])[0];
        if (info) {
          updateDevice(item.token, { instanceId: info.instanceId || item.instanceId, name: info.name || item.name, status: info.status || "", lastSeenAt: info.lastSeenAt || "" });
        }
      } catch {
        // 单台探测失败只影响它自己那一行（标成离线），不让整个面板转圈。
        updateDevice(item.token, { status: "offline" });
      }
    }));
    setDevices(readDevices());
  }, []);

  // 切换当前电脑。等价于"换一个令牌 + 把上一台的现场全部收掉"：
  // 顺序必须是 先落当前设备（storage 同步写）→ 再复位 → 最后拉新电脑的实例。
  // instanceID 置空是有意的：`/v1/instances` 万一失败，界面宁可显示"没有数据"，
  // 也不能把上一台电脑的项目列表挂在新令牌底下（那是"看错机器"的事故）。
  function switchDevice(nextToken: string) {
    if (!nextToken) return;
    setDevicesOpen(false);
    if (nextToken === readActiveToken()) return;
    setActiveToken(nextToken);
    setDevices(readDevices());
    setToken(nextToken);
    setInstances([]);
    setInstanceID("");
    setSnapshot(null);
    setSelectedProject("");
    setSelectedConversation("");
    setCommandState(null);
    setRefreshStatus(null);
    setError("");
    setPairingNotice(null);
    exitConversationView();
    void loadInstances();
  }

  // 打开切换面板：立刻用本地缓存渲染（零等待），同时后台把其它几台的状态探一遍。
  function openDevicesSheet() {
    setDevices(readDevices());
    setDevicesOpen(true);
    void probeOtherDevices();
  }

  // 非当前那几台电脑的慢轮询。当前设备仍是 5s（上面那个 effect），其余 30s 一次就够 ——
  // 它们的在线状态只是切换面板上的一行字，不值得按当前设备那个频率去问。
  // 声明位置必须在 `probeOtherDevices` **之后**：它是 useCallback 的 const，写前面会 TDZ。
  useEffect(() => {
    if (!documentVisible) return;
    if (devices.filter((item) => !item.revoked).length < 2) return;
    void probeOtherDevices();
    const timer = window.setInterval(() => { void probeOtherDevices(); }, 30_000);
    return () => window.clearInterval(timer);
    // 依赖用 `devices.length` 而不是 `devices`：probe 每次写回一个新数组，
    // 直接依赖它会把自己一遍遍重启（30s 定时器变成死循环）。
  }, [devices.length, documentVisible, probeOtherDevices]);

  // 校验码复制。桌面端把码和二维码摆在一起，但用户常常要用别的方式把码交给手机
  // （自己给自己发消息、贴到另一台设备），所以给一个明确的复制出口 + 结果反馈。
  async function copyPairingCode() {
    const ok = await copyToClipboard(pairingCode);
    setPairingCopied(ok);
    if (!ok) setPairingNotice({ text: "复制失败，请手动抄下这 6 位数字", state: "error" });
  }

  // ── 任务增删改：本地先生效，命令在后台走 ────────────────────────────────────
  //
  // 这三个操作以前都是"发命令 → 等最多 30 秒 → 再整份重拉快照"，期间整屏 busy。
  // 真机上命令往返劣化到 19~75 秒后，用户看到的就是"点了没反应、最后还报失败"，
  // 而桌面端其实晚几十秒才执行完。现在本地立刻改、立刻标「同步中」，命令在后台
  // 跑，成功/失败再回来收尾。见 PendingTaskMutation 那段注释。

  function setTaskBusy(taskID: string, busy: boolean) {
    setPendingTaskBusy((current) => {
      const next = new Set(current);
      if (busy) next.add(taskID);
      else next.delete(taskID);
      return next;
    });
  }

  function trackPendingTask(mutation: PendingTaskMutation) {
    pendingTaskRef.current.set(mutation.taskId, mutation);
    markPendingTasksChanged();
  }

  // notePendingCommand 只把命令 id 记回待确认项（用于诊断与后续对账）。
  function notePendingCommand(taskID: string, commandId: string) {
    const mutation = pendingTaskRef.current.get(taskID);
    if (mutation && commandId) pendingTaskRef.current.set(taskID, { ...mutation, commandId });
  }

  // resolvePendingCreate 把新建的临时 id 换成电脑端回执里的真 id，返回换完之后用来
  // 收尾的那个 id（没换成就是原 id）。
  //
  // 键、值、以及卡片上的「同步中」必须一起换：待确认表和 pendingTaskBusy 都是按 id
  // 索引的，漏掉任何一个，那张卡都会在同步途中丢掉「同步中」，后续的成功对账也找不回它。
  function resolvePendingCreate(pendingID: string, realID: string, commandId: string): string {
    const mutation = pendingTaskRef.current.get(pendingID);
    if (!mutation || realID === pendingID) return pendingID;
    const resolved = { ...withResolvedCreateID(mutation, realID), commandId };
    if (resolved.taskId === pendingID) return pendingID;
    pendingTaskRef.current.delete(pendingID);
    pendingTaskRef.current.set(resolved.taskId, resolved);
    setTaskBusy(pendingID, false);
    setTaskBusy(resolved.taskId, true);
    markPendingTasksChanged();
    return resolved.taskId;
  }

  function dropPendingTask(taskID: string) {
    if (pendingTaskRef.current.delete(taskID)) markPendingTasksChanged();
  }

  // runTaskCommand 把命令发出去并等它进入终态，**不**碰界面状态。
  // 所有收尾（撤销本地改动 / 重拉快照 / 提示）都在调用方。
  //
  // settled 必须单独带出来：命令没有在窗口内拿到终态，和命令明确失败是两回事 ——
  // 前者撤掉本地改动等于删掉一张可能已经建好的卡片（见 settleTaskMutation）。
  async function runTaskCommand(body: Record<string, unknown>): Promise<{ commandId: string; settled: boolean; result: unknown; error: string }> {
    try {
      const accepted = await cloud<AcceptedCommand>(`/v1/instances/${encodeURIComponent(instanceID)}/commands`, {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey() },
        body: JSON.stringify(body),
      });
      setCommandState(accepted);
      const finalState = await waitForCommand(accepted.commandId);
      if (!finalState) return { commandId: accepted.commandId, settled: false, result: undefined, error: "" };
      if (finalState.status !== "completed") {
        return { commandId: accepted.commandId, settled: true, result: finalState.result, error: commandFailureDetail(finalState.result) || "电脑端没有执行这条操作" };
      }
      return { commandId: accepted.commandId, settled: true, result: finalState.result, error: "" };
    } catch (cause) {
      // 请求没发出去（断网 / 令牌失效），或者发出去了但响应丢了（15 秒超时）。
      // 两种都按"明确失败"处理、回滚本地改动：前者确定没发生，后者极罕见；而把
      // 它当"结果未知"留着，会让每一次断网都变成一张永远摘不掉的「同步中」卡片。
      // 真的发生"响应丢了但命令已送达"时，桌面端会把任务建出来，下一次快照就会
      // 把它带回列表里。
      return { commandId: "", settled: true, result: undefined, error: cause instanceof Error ? cause.message : "命令发送失败" };
    }
  }

  // settleTaskMutation 是每一次乐观改动的收尾：
  //   · 命令明确失败 → 撤掉本地改动，把原因说清楚；
  //   · 命令成功 → 重拉一次快照，只有快照真的反映了结果才把「同步中」摘掉；
  //   · 只是还没拿到回执 → **什么都不撤**，只如实说明。
  //
  // 最后一条是有意的：30 秒没等到终态，不等于电脑端没做。这时候撤掉卡片，就是把一张
  // 可能已经建好的任务从用户眼前删掉；而留着它最坏也只是多显示一会儿「同步中」。
  // 快照落地时还会再对账一次（mutationReflectsInSnapshot 用的就是快照）。命令真的
  // 没送达时，云端那条 8 秒宽限期判定会把终态推回来，下一次轮询就收尾了。
  async function settleTaskMutation(taskID: string, outcome: { settled: boolean; error: string }, onRollback?: (reason: string) => void) {
    if (!outcome.settled) {
      setError("电脑端还没有回执，操作可能仍在进行；同步完成后会自动更新");
      return;
    }
    if (outcome.error) {
      dropPendingTask(taskID);
      setTaskBusy(taskID, false);
      setError(outcome.error);
      onRollback?.(outcome.error);
      return;
    }
    const result = await loadSnapshot({ force: true });
    const mutation = pendingTaskRef.current.get(taskID);
    if (!mutation) {
      setTaskBusy(taskID, false);
      return;
    }
    if (result.outcome === "failed") {
      setTaskBusy(taskID, false);
      setError("已同步到电脑端，但云端快照还没更新，稍后刷新即可");
      return;
    }
    // 待确认集合整份传进去：新建的判据在"还没有真 id"时要数同内容的条数（见
    // mutationReflectsInSnapshot），只传自己那一条会把两条同名任务判错。
    if (mutationReflectsInSnapshot(mutation, (result.snapshot || snapshot)?.projects, pendingTaskRef.current.values())) dropPendingTask(taskID);
    setTaskBusy(taskID, false);
  }

  async function createTask(event: FormEvent) {
    event.preventDefault();
    if (!project) return;
    const trimmedTitle = title.trim();
    const trimmedDescription = description.trim();
    // 标题是**可选**字段，任务说明才是必填 —— 与电脑端（TaskBoard 把「任务名称」标成"可选"、
    // 给「任务说明」挂 `required`）以及 control-server `validateTaskInput`（`task description is required`）
    // 一致。这里原来判的却是标题那一格，正好两边都判反：按电脑端习惯只填说明的用户
    // 会撞上一颗**静默禁用**的按钮（点了完全没反应），只填标题的用户则会被服务端打回来。
    // `required` 只看"值是不是空串"，一片空格的说明能绕过它，所以这里再兜一次；
    // 提示必须落在弹层自己的 `createError` 上（页面级那条 `.mobile-error` 在遮罩之下）。
    if (!trimmedDescription) {
      setCreateError("任务描述不能为空");
      return;
    }
    // 用户点的那一档就在提交这一刻定下来：后面清空表单、关弹层都不该再动它。
    const chosenPriority = createPriority || "normal";
    const projectID = project.id;
    const optimisticID = newPendingTaskID();
    const stamp = new Date().toISOString();
    setError(""); setCreateError("");
    // 1) 本地先摆出来。用临时 id：桌面端会分配真 id，快照刷新后由真卡片取代。
    trackPendingTask({
      commandId: "",
      kind: "create",
      projectId: projectID,
      taskId: optimisticID,
      task: { id: optimisticID, title: trimmedTitle, description: trimmedDescription, priority: chosenPriority, status: "todo", updatedAt: stamp },
    });
    setTaskBusy(optimisticID, true);
    // 新任务的状态是待处理，一定落在「待处理」那一档。用户可能正停在别的分类上，
    // 不把面板切过去，刚建好的卡片就落在当前这一档看不见的地方 —— 那和"没生效"
    // 在用户眼里没有区别，乐观更新也就白做了。
    setTaskFilter("todo");
    // 弹层立刻收起、输入框立刻清空：用户已经看到任务出现在队列里了，没有理由再等。
    // 优先级一并回到默认「普通」，否则下一条会在用户没看这一格的情况下沿用上一条的档位。
    setTitle(""); setDescription(""); setCreatePriority("normal"); setCreatingTask(false); setCreateError("");
    const outcome = await runTaskCommand({
      type: "task.create",
      projectId: projectID,
      payload: { title: trimmedTitle, description: trimmedDescription, priority: chosenPriority },
    });
    // 电脑端把新建任务的**真 id** 放在回执里。就地换成它之后，"这张卡片落地了吗"
    // 就不用再靠标题+描述去猜（见 withResolvedCreateID）。
    const created = taskFromCommandResult(outcome.result);
    const settleID = created ? resolvePendingCreate(optimisticID, created.id, outcome.commandId) : optimisticID;
    if (!created) notePendingCommand(optimisticID, outcome.commandId);
    await settleTaskMutation(settleID, outcome, (reason) => {
      // 回滚时把用户刚打的字放回输入框并重新打开弹层：任务已经被撤掉了，
      // 至少不让他把这些内容再打一遍。优先级也要一起放回去 —— 少了它，
      // 弹层重新打开时那一格显示的是默认「普通」，而用户明明选过「紧急」。
      setTitle(trimmedTitle);
      setDescription(trimmedDescription);
      setCreatePriority(chosenPriority);
      setCreateError(reason);
      setCreatingTask(true);
    });
  }

  async function sendTaskCommand(taskID: string, type: string, payload: Record<string, unknown> = {}): Promise<boolean> {
    // 下发 / 验收 / 停止没有乐观改动可叠加，但同样要给即时反馈：命中了就先把这张
    // 卡片标成「同步中」并按住它自己的按钮，别让用户以为没点上而连点。
    setTaskBusy(taskID, true);
    const commandPayload = type === "task.review" ? { action: "accept", ...payload } : payload;
    const outcome = await runTaskCommand({ type, taskId: taskID, payload: commandPayload });
    notePendingCommand(taskID, outcome.commandId);
    await settleTaskMutation(taskID, outcome);
    return !outcome.error;
  }

  function openTaskEditor(task: Task) {
    setEditingTask(task);
    setEditTitle(task.title || "");
    setEditDescription(task.description || "");
    setEditPriority(task.priority || "normal");
    // 与创建弹层同理：上一次的失败原因不能带到这一次（那个理由是给上一条任务的）。
    setEditError("");
  }

  async function saveTaskEdit(event: FormEvent) {
    event.preventDefault();
    if (!editingTask) return;
    const trimmedDescription = editDescription.trim();
    // 字段语义与创建路径完全一致：标题可选、任务说明必填（理由见 createTask 上面那段）。
    // 提示改走弹层自己的 `editError` —— 原来写的 `setError` 是页面级那条，它在遮罩**之下**，
    // 弹层不关就永远看不到，症状就是"点了保存、什么都没发生"。
    if (!trimmedDescription) {
      setEditError("任务描述不能为空");
      return;
    }
    const patch = { title: editTitle.trim(), description: trimmedDescription, priority: editPriority };
    const taskID = editingTask.id;
    // 卡片上的改动立刻可见，弹层立刻收起。
    trackPendingTask({ commandId: "", kind: "update", projectId: selectedProject, taskId: taskID, patch: { ...patch, updatedAt: new Date().toISOString() } });
    setTaskBusy(taskID, true);
    setEditingTask(null);
    await sendTaskCommand(taskID, "task.update", patch);
  }

  async function confirmTaskDelete() {
    if (!deletingTask) return;
    const taskID = deletingTask.id;
    trackPendingTask({ commandId: "", kind: "delete", projectId: selectedProject, taskId: taskID });
    setTaskBusy(taskID, true);
    setDeletingTask(null);
    await sendTaskCommand(taskID, "task.delete");
  }

  // 把一段文本放进草稿、收起面板、把焦点还给输入框 —— 手机上没有"先关面板、再点输入框"这个动作。
  // 等新草稿渲染进 textarea 之后再聚焦，否则光标会落在旧内容的末尾。
  function fillComposer(text: string) {
    setMessageDraft(text);
    setComposerToolsOpen(false);
    requestAnimationFrame(() => messageInputRef.current?.focus());
  }

  // 技能：与桌面端 useSkill 一样只挂一颗引用胶囊，**不碰用户已经写好的草稿**
  // （旧实现是直接调 fillComposer 把整段引用覆盖进输入框）。
  // 技能本身只是文件系统上的 SKILL.md，手机端不可能去读它，展开成"名称 + 描述"那句指令
  // 推迟到发送那一刻，见 composeSkillMessage。
  function fillSkill(skill: RemoteSkill) {
    setSkillRefs((current) => current.some((item) => item.name === skill.name && item.source === skill.source) ? current : [...current, skill]);
    setComposerToolsOpen(false);
    requestAnimationFrame(() => messageInputRef.current?.focus());
  }

  // 移除一颗引用胶囊。同名技能只可能在同一个来源出现一次，所以用 (名字, 来源) 定位。
  function removeSkillRef(skill: RemoteSkill) {
    setSkillRefs((current) => current.filter((item) => !(item.name === skill.name && item.source === skill.source)));
  }

  // ── 消息卡片操作（2026-09-20 方案 B：气泡右上角那颗「⋯」）────────────────────

  // 展开某条消息的图标行。触发它的那颗「⋯」记下来：收起时（Esc）焦点要还给它。
  // 同一时刻只允许有一条展开 —— 点另一条的「⋯」会先把这条换掉，不会出现两排图标。
  function openMessageActions(trigger: HTMLButtonElement, messageID: string) {
    messageActionTriggerRef.current = trigger;
    setMessageCopyState("idle");
    setMessageActionId(messageID);
  }

  // 收起的**唯一出口**：点外部、Esc、返回键、侧滑、换会话、以及"动作已经做完"全都走它。
  // 两处状态必须一起收 —— 漏掉反馈的重置，下一条消息展开时会直接带着上一条的勾。
  function closeMessageActions() {
    if (messageCopyTimerRef.current !== null) {
      window.clearTimeout(messageCopyTimerRef.current);
      messageCopyTimerRef.current = null;
    }
    setMessageCopyState("idle");
    setMessageActionId("");
  }

  // 复制整条消息：与桌面端 ConversationPage 的 MessageCard 是同一个实现（lib/clipboard 两端共用），
  // 复制的是 Markdown 原文 —— 粘回编辑器仍带格式，与代码块那颗复制键（只复制代码）互补。
  async function copyMessageBody(content: string) {
    const copied = await copyToClipboard(content);
    setMessageCopyState(copied ? "copied" : "failed");
    if (messageCopyTimerRef.current !== null) window.clearTimeout(messageCopyTimerRef.current);
    messageCopyTimerRef.current = window.setTimeout(() => setMessageCopyState("idle"), 1_600);
  }

  // 引用到输入框。与技能引用同一套做法：**不碰用户写好的草稿**，只在输入条上方挂一颗胶囊，
  // 发送那一刻才由 withQuoteBlock 展开成引用块。
  // 入参收「引用对象 / 正文」这样的纯数据（而不是 Message）：图标行每帧都跟着会话重渲染，
  // 点下去拿到的就是**那一刻**的正文 —— 流式消息引用到的是它当前写到的地方，不是旧快照。
  function quoteMessage(quote: MessageQuote) {
    setQuoteRef(quote);
    // 先收图标行、再交焦点：收起会触发一次重渲染，顺序反过来的话输入框会刚拿到焦点就被抢走。
    closeMessageActions();
    requestAnimationFrame(() => messageInputRef.current?.focus());
  }

  // 重新发送：把原文写回输入框，**绝不自动发出**（这里是"填回去、用户改完再发"）。
  // 有草稿时追加、不覆盖 —— 项目踩过"整体覆盖把用户写到一半的草稿无声吃掉"这个坑。
  function resendMessage(content: string) {
    setMessageDraft((current) => (current.trim() ? `${current.replace(/\s+$/, "")}\n${content}` : content));
    closeMessageActions();
    setComposerToolsOpen(false);
    requestAnimationFrame(() => {
      const input = messageInputRef.current;
      if (!input) return;
      input.focus();
      // 光标落到末尾：追加之后用户接着要改的就是刚填进来的这一段。
      input.setSelectionRange(input.value.length, input.value.length);
    });
  }

  // 快捷方式入口。fill 只让电脑端渲染（模板里的 ${project.path} 这类变量手机端没有），
  // 把渲染结果填进手机输入框；run / confirm 直接在电脑端执行。
  // confirm 先在手机上弹确认框：这条命令会在电脑上跑，误触的代价不在这一屏。
  async function applyShortcut(shortcut: RemoteShortcut) {
    if (!conversation || busy || shortcutBusy) return;
    // 会话还没拿到真 id 时，这条命令在电脑端找不到目标，必然失败。与其让它失败，
    // 不如当场说清楚 —— 这个窗口通常只有一秒（见 createConversationForProject）。
    if (conversationPending) { setError("会话还在创建中，等电脑端接上后再试"); return; }
    const action = shortcutAction(shortcut);
    if (action === "confirm") { setComposerToolsOpen(false); setConfirmShortcut(shortcut); return; }
    await sendShortcutCommand(shortcut.id, action);
  }

  // 走云端命令通道触发电脑端同一条捷径路径：
  //   fill           → 电脑端 /preview 渲染后把正文回传，手机端填入自己的输入框
  //   run / confirm  → 电脑端 /run 真的执行（渲染规则、命令包装、运行审计全在电脑端，
  //                    两端不可能跑出不同结果）
  //
  // 只有 fill 需要等：正文是电脑端渲染出来的，手机端拿不到那个结果就没东西可填。
  // run / confirm 的执行与正文全在电脑端，手机端不需要结果就能收尾 —— 所以命令发出去
  // 就立刻放行，出错再由后台那条链路回报。把这条也挂成 await，等于让用户为一个
  // **他根本不看的返回值**等最长 30 秒。
  async function sendShortcutCommand(shortcutID: string, action: "fill" | "run" | "confirm") {
    if (!conversation || shortcutBusy) return;
    if (conversationPending) { setError("会话还在创建中，等电脑端接上后再试"); return; }
    setShortcutBusy(shortcutID);
    setError("");
    try {
      const accepted = await cloud<AcceptedCommand>(`/v1/instances/${encodeURIComponent(instanceID)}/commands`, {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey() },
        body: JSON.stringify({ type: "conversation.shortcut", payload: { conversationId: conversation.id, shortcutId: shortcutID, action } }),
      });
      setCommandState(accepted);
      if (action !== "fill") {
        setComposerToolsOpen(false);
        void waitForCommand(accepted.commandId).then((finalState) => {
          if (finalState?.status === "completed") {
            // 执行类的捷径会在电脑端产生新消息/状态，拉一次快照让手机端跟上。
            void loadSnapshot();
            return;
          }
          // 没拿到终态 ≠ 没执行（同消息那条策略）：命令已被云端受理就会送达。
          setError(finalState
            ? `快捷方式执行失败：${commandFailureDetail(finalState.result) || "请检查电脑端 Agent 状态"}`
            : "快捷方式已下发，状态暂时无法确认；稍后会自动更新");
        });
        return;
      }
      const finalState = await waitForCommand(accepted.commandId);
      if (!finalState || finalState.status !== "completed") {
        setError(finalState ? `快捷方式执行失败：${commandFailureDetail(finalState.result) || "请检查电脑端 Agent 状态"}` : "快捷方式仍在处理中，请稍后重试");
        return;
      }
      const content = shortcutContentFromCommandResult(finalState.result);
      if (!content) {
        setError("快捷方式没有可填入的内容");
        return;
      }
      fillComposer(content);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "快捷方式执行失败");
    } finally {
      setShortcutBusy("");
    }
  }

  async function confirmShortcutRun() {
    const shortcut = confirmShortcut;
    if (!shortcut) return;
    setConfirmShortcut(null);
    await sendShortcutCommand(shortcut.id, "confirm");
  }

  // 软键盘上没有顺手的 Shift+Enter（Enter 已经在 onKeyDown 里被当成发送），
  // 所以"换行"必须留一个显式入口，否则手机上根本写不出多行消息。
  function insertComposerLineBreak() {
    const input = messageInputRef.current;
    const start = input?.selectionStart ?? messageDraft.length;
    const end = input?.selectionEnd ?? start;
    setMessageDraft(`${messageDraft.slice(0, start)}\n${messageDraft.slice(end)}`);
    setComposerToolsOpen(false);
    const caret = start + 1;
    requestAnimationFrame(() => {
      const element = messageInputRef.current;
      if (!element) return;
      element.focus();
      element.setSelectionRange(caret, caret);
    });
  }

  async function sendConversationMessage(event: FormEvent) {
    event.preventDefault();
    // draft = 用户自己在输入框里写的正文；content = 拼上技能引用 / 引用块之后真正上云的那条消息。
    // 所有"写回输入框"的路径都只能用 draft —— 否则一次发送失败就会把整段技能描述重新灌回输入框。
    // 「引用」与「技能」一样不计入 draft：它是一颗可以单独摘掉的胶囊，不是用户写的正文。
    const draft = messageDraft.trim();
    if (!conversation || (!draft && skillRefs.length === 0 && !quoteRef)) return;
    // 先拼引用块再交给 composeSkillMessage，得到的顺序是「技能指令 → 引用块 → 正文」：
    // 技能那段文案必须与桌面端逐字一致，只能由 composeSkillMessage 生成（见 withQuoteBlock）。
    const content = composeSkillMessage(withQuoteBlock(draft, quoteRef), skillRefs);
    const conversationID = conversation.id;
    const clientRequestId = idempotencyKey();
    const createdAt = new Date().toISOString();
    const optimisticID = `pending-${clientRequestId}`;
    // 发送时无条件恢复贴底跟随：键盘弹出会因 adjustResize 缩小视口、把「贴底」
    // 判定顶掉，若不在这里重置，用户发完消息不会自动跟随自己的新消息与回复。
    stickToBottomRef.current = true;
    const pending = pendingMessageRef.current.get(conversationID) || new Map<string, PendingMessage>();
    pending.set(clientRequestId, { requestId: clientRequestId, content, createdAt });
    pendingMessageRef.current.set(conversationID, pending);
    // ⚠️ 「Agent 正在跑」的指示**不在这里**打：排队等真 id 的那条消息根本还没发出去，
    // 先把指示亮起来就是在替电脑端许一个它还不知道的承诺。它由 dispatchConversationMessage
    // 在命令真的交给云端那一刻点，两条路径（直发 / 补发）因此都只点一次。
    setSnapshot((current) => current ? { ...current, projects: current.projects.map((item) => ({ ...item, conversations: item.conversations.map((entry) => entry.id === conversationID && !entry.messages.some((message) => message.id === optimisticID) ? { ...entry, messages: [...entry.messages, { id: optimisticID, role: "user", content, createdAt }] } : entry) })) } : current);
    setMessageDraft("");
    // 引用与草稿一起乐观清空；下面每条失败回填分支都会把胶囊一并放回去。
    setSkillRefs([]);
    setQuoteRef(null);
    setError("");
    // 会话本身还没在电脑端建好（新建会话的乐观窗口，见 createConversationForProject）：
    // 这条命令此刻发出去必然"找不到会话"。先把消息排队，真 id 一到立刻补发 ——
    // 用户不必为了那次往返而写不了字。
    // 会话还在由本地那张卡顶着（真 id 拿到之前，以及拿到之后快照追上之前）时，
    // 气泡是画在那张卡上的（见 applyPendingConversations），得让叠加层重算一次。
    const overlaid = isPendingConversationID(conversationID)
      || Array.from(pendingConversationsRef.current.values()).some((item) => item.resolvedId === conversationID);
    if (isPendingConversationID(conversationID)) {
      const queue = deferredMessagesRef.current.get(conversationID) || [];
      // draft / quote 与 content 分开存：失败时要还回去的是**用户自己写的正文与被引用的那条**，
      // 不是拼上技能描述之后的那一整段。
      // ⚠️ 老实说：这条路上 `quote` **今天基本恒为 null** —— 待建会话的线程里只有 `pending-*` 乐观气泡，
      // 而引用对这类消息是禁用的（见 isTransientMessage）。留着它的理由不是"现在用得上"，而是
      // "**漏掉它才是静默丢数据**"：一旦哪天允许引用，丢的就是用户刚挂上的那条引用，
      // 而他只会看到自己写的字回来了、以为引用还在。三行代码换掉这一类不可见的错。
      queue.push({ requestId: clientRequestId, content, createdAt, draft, quote: quoteRef });
      deferredMessagesRef.current.set(conversationID, queue);
      markPendingConversationsChanged();
      return;
    }
    if (overlaid) markPendingConversationsChanged();
    await dispatchConversationMessage(conversationID, content, clientRequestId, createdAt, (reason) => {
      setMessageDraft((current) => current.trim() ? current : draft);
      setSkillRefs((current) => current.length > 0 ? current : skillRefs);
      setQuoteRef((current) => current ?? quoteRef);
      setError(reason);
    });
  }

  // 把一条已经在本机摆好的消息交给云端的命令通道。乐观气泡由调用方先摆好（含"排队等真
  // id"那条路径），这里只负责发命令与收尾；`onRejected` 只在**明确失败**时被调用一次。
  //
  // 这里**不碰全局 busy**：发消息完全是本机的操作，那条命令只是事后发给电脑端的一条通知。
  async function dispatchConversationMessage(conversationID: string, content: string, clientRequestId: string, createdAt: string, onRejected: (reason: string) => void) {
    const optimisticID = `pending-${clientRequestId}`;
    const dropOptimistic = () => setSnapshot((current) => current ? { ...current, projects: current.projects.map((item) => ({ ...item, conversations: item.conversations.map((entry) => entry.id === conversationID ? { ...entry, messages: entry.messages.filter((message) => message.id !== optimisticID) } : entry) })) } : current);
    // 命令马上要交给云端了 —— 从这一刻起"电脑端在跑"才是句实话。
    markConversationProcessing(conversationID, snapshotRevisionRef.current);
    try {
      const accepted = await cloud<{ commandId: string; status: string }>(`/v1/instances/${encodeURIComponent(instanceID)}/commands`, {
        method: "POST",
        headers: { "Idempotency-Key": clientRequestId },
        body: JSON.stringify({ type: "conversation.message", payload: { conversationId: conversationID, content, clientRequestId } }),
      });
      setCommandState(accepted);
      const queued = pendingMessageRef.current.get(conversationID);
      while (queued && queued.size > 50) {
        const oldest = queued.keys().next().value;
        if (!oldest) break;
        queued.delete(oldest);
      }
      // The optimistic entry and SSE event are already visible. One background
      // snapshot reconciles status/history without a polling storm when the
      // Agent uploads its next snapshot.
      void loadSnapshot();
      void waitForCommand(accepted.commandId).then((finalState) => {
        if (finalState?.status === "completed") {
          void loadSnapshot();
          return;
        }
        const stillOnConversation = selectedConversationRef.current === conversationID;
        const anotherMessagePending = (pendingMessageRef.current.get(conversationID)?.size || 0) > 0;
        // ⚠️ 没拿到终态（null）**不等于**电脑端没做：命令已经被云端受理，只是我们没在
        // 30 秒窗口里看到结果。这时候撤气泡、把正文放回输入框，就是删掉一条很可能已经
        // 送达的消息、再诱导用户重发一次。与任务那三条同一套策略（见 settleTaskMutation）。
        if (!finalState) {
          if (stillOnConversation && !anotherMessagePending) setError("消息已发出，状态暂时无法确认；稍后会自动更新");
          return;
        }
        // 明确失败（云端拒绝 / 命令过期 / 已取消）：撤气泡，把内容还回去。
        clearConversationProcessing(conversationID);
        const failed = pendingMessageRef.current.get(conversationID);
        failed?.delete(clientRequestId);
        if (failed?.size === 0) pendingMessageRef.current.delete(conversationID);
        dropOptimistic();
        if (stillOnConversation && !anotherMessagePending) {
          onRejected(`消息执行失败：${commandFailureDetail(finalState.result) || "输入内容仍保留在输入框"}`);
        }
      });
    } catch (cause) {
      // 请求没发出去（断网 / 令牌失效）：这条消息确定没送达，撤气泡 + 回填。
      clearConversationProcessing(conversationID);
      const failed = pendingMessageRef.current.get(conversationID);
      failed?.delete(clientRequestId);
      if (failed?.size === 0) pendingMessageRef.current.delete(conversationID);
      dropOptimistic();
      if (selectedConversationRef.current === conversationID) onRejected(cause instanceof Error ? cause.message : "消息发送失败");
    }
  }

  // 电脑端把真 id 交回来了（命令回执里带的，或 SSE 的 conversation.created 认出来的）。
  // 这一步只做三件事：认下这个 id（本地卡从这一刻起用真 id 渲染）、把按 id 索引的表搬过去、
  // 补发排队的消息。
  //
  // ⚠️ 它**不**摘掉本地卡。摘卡（adoptPendingConversation）要等快照版本号真的前进过 ——
  // 拿到 id 就摘会在一次迟到的刷新里把卡片连同用户刚打的消息一起抹掉，SSE 抢先塞进快照的
  // 那条会话本身就是个空壳，顶不住。两条都实测复现过（见 .tmp/probe-conversation-optimistic.mjs）。
  function resolvePendingConversation(pendingID: string, realID: string) {
    const registry = pendingConversationsRef.current;
    const item = registry.get(pendingID);
    if (!item || !realID || realID === pendingID) return;
    registry.set(pendingID, { ...item, resolvedId: realID });
    movePendingConversationState(pendingID, realID, item.projectId);
    markPendingConversationsChanged();
    flushDeferredConversationMessages(pendingID, realID);
  }

  // 把按 id 索引的三张表从临时 id 搬到真 id 上。
  // 漏掉任何一个，用户眼前那条会话都会换一副样子：气泡没了、转圈停了、或者被弹回列表。
  // 幂等：搬过一次之后再调（源键已不存在）全是空操作。
  function movePendingConversationState(pendingID: string, realID: string, projectId: string) {
    const pendingMessages = pendingMessageRef.current.get(pendingID);
    if (pendingMessages) {
      pendingMessageRef.current.delete(pendingID);
      const existing = pendingMessageRef.current.get(realID);
      pendingMessageRef.current.set(realID, existing ? new Map([...existing, ...pendingMessages]) : pendingMessages);
    }
    setProcessingConversations((current) => {
      const from = current[pendingID];
      if (!from) return current;
      const next = { ...current };
      const to = next[realID];
      next[realID] = to ? { ...to, count: to.count + from.count } : from;
      delete next[pendingID];
      return next;
    });
    if (selectedConversationRef.current === pendingID) {
      selectedConversationRef.current = realID;
      // 记下这一次替换，让复位 effect 认出"这不是换会话"（详见 conversationIDSwapRef）。
      conversationIDSwapRef.current = { projectId, from: pendingID, to: realID };
      setSelectedConversation(realID);
    }
  }

  // 补发：会话在这一刻才真正存在，用户之前敲的消息现在才能送出去。
  // 同一批只会被其中一条路径取走（取走即删表项），所以不会重复发。
  function flushDeferredConversationMessages(pendingID: string, realID: string) {
    const deferred = deferredMessagesRef.current.get(pendingID) || [];
    if (deferred.length === 0) return;
    deferredMessagesRef.current.delete(pendingID);
    // ⚠️ **逐条按顺序发**，不许并发：电脑端执行远程命令是串行的，而并发 POST 的到达顺序
    // 没有任何保证。用户在会话还没建好时连发三条（这正是本机先建会话要支持的用法），
    // 并发补发就会让电脑端按乱序执行，回复也跟着乱序。
    void (async () => {
      for (const message of deferred) {
        await dispatchConversationMessage(realID, message.content, message.requestId, message.createdAt, (reason) => {
          setMessageDraft((current) => (current.trim() ? current : message.draft));
          // 引用比草稿更需要还回来：被引用的那条消息在列表里可能已经滚出屏幕，
          // 用户没法"再点一次"把它找回来（技能胶囊至少还能在面板里重新点一遍）。
          setQuoteRef((current) => current ?? message.quote);
          setError(reason);
        });
      }
    })();
  }

  // 电脑端上传了一版**包含这条会话**的快照 —— 本地那张卡可以退场了，此后一切以快照为准。
  // 挪表与补发是 resolvePendingConversation 的活（它一定先跑过），这里只做两件事。
  function adoptPendingConversation(pendingID: string, realID: string) {
    if (!pendingConversationsRef.current.delete(pendingID)) return;
    // ① 把还没落地的乐观气泡就地补进快照里那条会话。
    //
    // 这一补不是多余的：本地卡之所以能顶着气泡，靠的是叠加层每帧按 `pendingMessageRef`
    // 补一遍；卡一退场，那个来源就没了。而快照里那条会话很可能是 `conversation.created`
    // 抢先塞进去的**空壳**（"补气泡"只在快照被**应用**时才跑，塞进去的那一刻不跑），
    // 于是用户的刚发出去的消息会消失到电脑端下一次上传快照为止。
    // 用同一套 `pending-<requestId>` id 并按 id 去重，因此与 loadSnapshot 的对账不打架。
    const messages = pendingMessageRef.current.get(realID);
    if (messages && messages.size > 0) {
      const bubbles = Array.from(messages.values()).map((message) => ({ id: `pending-${message.requestId}`, role: "user" as const, content: message.content, createdAt: message.createdAt }));
      setSnapshot((current) => current ? {
        ...current,
        projects: current.projects.map((project) => ({
          ...project,
          conversations: project.conversations.map((entry) => {
            if (entry.id !== realID) return entry;
            const seen = new Set(entry.messages.map((message) => message.id));
            const additions = bubbles.filter((bubble) => !seen.has(bubble.id));
            return additions.length > 0 ? { ...entry, messages: [...entry.messages, ...additions] } : entry;
          }),
        })),
      } : current);
    }
    markPendingConversationsChanged();
  }

  // 30 秒窗口没等到终态之后，在后台继续**稀疏地**盯一会儿这条命令。
  //
  // 为什么必须有这一段：不撤是对的（电脑端很可能已经建好了），但"不撤"的另一面是
  // 万一它真的失败了，用户手上就多一条永远点不动、消息永远发不出去的幽灵会话 ——
  // 那是比重试一次糟糕得多的状态（用户在它里面发的消息只会排队、永远不出去）。
  // 这里给它一个有界的结局：确认失败就撤掉，确认成功就交给认领 effect。
  //
  // ⚠️ **窗口必须比云端的命令有效期长**，而且不是长一点点就行：
  //   云端 `createCommand` 给命令的有效期是 **5 分钟**（`server.go` 的 `expires`），
  //   而"判死"那条 SQL 只在**有人查询**这条命令时才跑（`getCommand` 里的
  //   `update ... set status='expired' where expires_at<=now() and status='queued'`）。
  //   也就是说：手机若在 5 分钟前就停止追问，那条命令会永远停在 queued，这条兜底也就
  //   永远等不到那个 expired —— 幽灵会话留在这一屏上。所以取 **6 分钟**（5 分钟 + 余量）。
  //   改云端那个有效期，这里必须跟着改。
  //
  // 5 秒一次而不是 waitForCommand 那套 500ms：这是兜底，不该为它在移动网络上下几百个请求。
  // 最坏情况（命令真的丢了）是 6 分钟里约 72 个请求，且只在"30 秒没等到终态"这条罕见
  // 路径上发生。
  async function settlePendingConversationInBackground(pendingID: string, commandId: string) {
    const deadline = Date.now() + 360_000;
    while (Date.now() < deadline) {
      if (!pendingConversationsRef.current.has(pendingID)) return; // 已被认领或已撤
      await new Promise((resolve) => window.setTimeout(resolve, 5000));
      if (!pendingConversationsRef.current.has(pendingID)) return;
      try {
        const value = await cloud<CommandState & { command?: { commandId?: string } }>(`/v1/commands/${encodeURIComponent(commandId)}`);
        const state = normalizeCommandState(value, commandId);
        if (!terminalCommandStatuses.includes(state.status)) continue;
        if (state.status !== "completed") {
          failPendingConversation(pendingID, `无法创建会话：${commandFailureDetail(state.result) || "请检查电脑端 Agent 状态"}`);
        }
        // completed：会话确实建好了，只是回执晚了 —— 认领 effect 会把它接走。
        return;
      } catch {
        // 一次读不到不算失败，下一个周期再问。
      }
    }
  }

  // 会话**明确**没能建成（电脑端拒绝 / 命令过期 / 取消）：撤掉本地那条，并把用户在这条
  // 会话里敲过的内容放回输入框。只有明确失败才走这里 —— 没拿到回执时不许撤
  // （撤了就是删掉一条电脑端可能已经建好的会话，同任务那三条的策略）。
  function failPendingConversation(pendingID: string, reason: string) {
    const registry = pendingConversationsRef.current;
    const item = registry.get(pendingID);
    if (!item) return;
    // 已经认下真 id 了就不再回滚。
    //
    // `resolvedId` 只可能来自两条路，两条都意味着"这条会话在电脑端确实存在"：命令回执
    // 里带回了它，或者快照里已经出现了它。这时候再撤掉本地卡、把已经发出去的正文塞回
    // 输入框、还把用户踢回项目列表，就是当着用户的面删掉一条活着的会话 —— 而命令层面的
    // 失败（后端的终态报告晚到、甚至自相矛盾）不该有这种后果。会话本身交给快照去对账。
    if (item.resolvedId) { setError(reason); return; }
    // 用户此刻是不是就停在这条会话上。回填正文只在这时候做：输入框只有**一个**，
    // 用户已经走开到别的会话（甚至别的项目）时，把他没读过的这段字灌进别人家的输入框，
    // 比丢掉这段字更糟 —— 他会以为是当前那条会话的草稿。
    const wasOnConversation = selectedConversationRef.current === pendingID;
    registry.delete(pendingID);
    const deferred = deferredMessagesRef.current.get(pendingID) || [];
    deferredMessagesRef.current.delete(pendingID);
    pendingMessageRef.current.delete(pendingID);
    setProcessingConversations((current) => {
      if (!current[pendingID]) return current;
      const next = { ...current };
      delete next[pendingID];
      return next;
    });
    markPendingConversationsChanged();
    if (wasOnConversation) {
      selectedConversationRef.current = "";
      setSelectedConversation("");
      exitConversationView();
    }
    if (deferred.length > 0 && wasOnConversation) {
      const text = deferred.map((message) => message.draft).join("\n\n");
      setMessageDraft((current) => (current.trim() ? current : text));
      // ⚠️ 引用**也必须还回来**（2026-09-20 复查抓出来的漏项）：它与草稿是同一次发送的两半。
      // 只还正文的话，用户看到自己写的字回到输入框、以为引用还挂着 —— 实际那条引用已经随失败
      // 一起没了，再发一次就少一段引用，而且他不会知道。
      // 队列里可能有几条（会话没过门时连发过），而引用是**单槽**的：还**最后**那一条
      // （＝失败那一刻输入条上挂着的那条），与"草稿拼起来还、单槽的东西还最后一个"同一口径。
      // （同 queue.push 那条注释：`quote` 今天基本恒为 null，这里是防"漏掉就静默丢"。）
      const lastQuote = [...deferred].reverse().find((message) => message.quote)?.quote ?? null;
      if (lastQuote) setQuoteRef((current) => current ?? lastQuote);
    }
    setError(reason);
  }

  // 新建会话：**先在本地建好**，再把命令发给电脑端。用户点完立刻就在会话里，可以打字、
  // 可以发送；电脑端分配真 id 是后台的事（消息会排队等它，见 sendConversationMessage）。
  //
  // 老实现是"发命令 → await 等终态（最长 30 秒）→ 才切视图"，期间整屏 busy：SSE 一断，
  // 用户点了「创建会话」就是对着一个按钮全灰的弹层干等半分钟。
  async function createConversationForProject(projectValue: Project, agentId?: "claude-code" | "codex") {
    if (!agentId) {
      openNewConversation(projectValue);
      return;
    }
    const pendingID = newPendingConversationID();
    // ① 本地立刻建好并进去。这一步全是本机的：不 await、不占 busy、不等电脑端。
    pendingConversationsRef.current.set(pendingID, {
      id: pendingID,
      projectId: projectValue.id,
      agentId,
      createdAt: new Date().toISOString(),
      commandId: "",
      // 发起这一刻项目里已有的会话 —— 命令回执丢掉时靠它把真身认回来。
      knownConversationIds: (projectValue.conversations || []).map((item) => item.id),
      // 交棒要等快照版本号真的前进过（见 PendingConversation.seenRevision）。
      // 取**当前这份快照对象**的版本号，而不是 `snapshotRevisionRef`：断网时页面会回落到
      // localStorage 缓存（那条路径只 setSnapshot、不动 ref），两者会分叉，而 SSE 塞进会话
      // 的正是当前这份快照 —— 拿 ref 比就会把"没前进"误判成"前进过"。
      seenRevision: snapshot?.snapshotRevision ?? snapshotRevisionRef.current,
    });
    markPendingConversationsChanged();
    setError("");
    setSelectedProject(projectValue.id);
    setSelectedConversation(pendingID);
    selectedConversationRef.current = pendingID;
    setNewConversationProject(null);
    setTasksOpen(false);
    setPairingExpanded(false);
    enterConversationView();
    // ② 命令在后台走。成功/失败都只影响本地那条卡的位置与文案，不阻塞任何别的操作。
    try {
      const accepted = await cloud<AcceptedCommand>(`/v1/instances/${encodeURIComponent(instanceID)}/commands`, {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey() },
        body: JSON.stringify({ type: "conversation.create", projectId: projectValue.id, payload: { agentId } }),
      });
      setCommandState(accepted);
      const tracked = pendingConversationsRef.current.get(pendingID);
      if (!tracked) return; // 已经被快照/SSE 认领了
      pendingConversationsRef.current.set(pendingID, { ...tracked, commandId: accepted.commandId });
      const finalState = await waitForCommand(accepted.commandId);
      const afterWait = pendingConversationsRef.current.get(pendingID);
      if (!afterWait) return; // 同上
      if (!finalState) {
        // 没在窗口内拿到终态 ≠ 电脑端没建。不撤、也不报错，只如实说明：
        // 命令已被云端受理就会送达，下一次快照（SSE 或 15 秒兜底）会把它认回来。
        // 同时挂一条有界的后台兜底，免得它真的失败时留下一条谁也清不掉的幽灵会话。
        setError("会话已在本机建好，电脑端尚未回执；同步完成后会自动接上");
        void settlePendingConversationInBackground(pendingID, accepted.commandId);
        return;
      }
      if (finalState.status !== "completed") {
        failPendingConversation(pendingID, `无法创建会话：${commandFailureDetail(finalState.result) || "请检查电脑端 Agent 状态"}`);
        return;
      }
      // 回执里带回了真 id：认下它（本地那张卡改用真 id 渲染，并补发排队的消息）。
      //
      // **不**在这里把卡片塞进快照：塞进去也只是本地的一厢情愿，紧接着那次刷新
      // （`loadSnapshot` 的判据是 `revision >= baseline`，服务端还没上传新一版时同版本号
      // 也会被接受）会把它整份盖掉 —— 卡片和用户刚打的消息一起消失。
      // 交棒只由认领 effect 收尾，判据是"快照版本号真的前进过、而且里面已经有这条会话"。
      const realID = conversationFromCommandResult(finalState.result)?.id || conversationIDFromCommandResult(finalState.result);
      if (realID) resolvePendingConversation(pendingID, realID);
      void loadSnapshot();
    } catch (cause) {
      failPendingConversation(pendingID, cause instanceof Error ? cause.message : "无法创建会话");
    }
  }

  async function openMobileProject(projectValue: Project) {
    setSelectedProject(projectValue.id);
    // 进入项目时强制跟到底部，避免带着上一个会话的滚动位置。
    stickToBottomRef.current = true;
    setTasksOpen(false);
    setPairingExpanded(false);
    const existingConversation = projectValue.conversations?.find((entry) => entry.isCurrent) || projectValue.conversations?.[0];
    if (existingConversation) {
      setSelectedConversation(existingConversation.id);
      enterConversationView();
      return;
    }
    setNewConversationAgent("claude-code");
    setNewConversationProject(projectValue);
  }

  function openNewConversation(projectValue: Project) {
    setNewConversationAgent("claude-code");
    setNewConversationProject(projectValue);
  }

  function cancelNewConversation() {
    if (!busy) setNewConversationProject(null);
  }

  async function waitForCommand(commandID: string) {
    const deadline = Date.now() + 30_000;
    while (Date.now() < deadline) {
      const remaining = deadline - Date.now();
      if (remaining <= 0) break;
      const requestController = new AbortController();
      const requestTimeout = window.setTimeout(() => requestController.abort(), Math.min(remaining, cloudRequestTimeoutMs));
      try {
        const response = await cloud<CommandState & { command?: { commandId?: string } }>(`/v1/commands/${encodeURIComponent(commandID)}`, { signal: requestController.signal });
        const state = normalizeCommandState(response, commandID);
        setCommandState((current) => mergeCommandState(current, state));
        if (terminalCommandStatuses.includes(state.status)) return state;
      } catch {
        // A transient mobile network failure must not be treated as command
        // failure. Keep polling until the bounded wait expires.
      } finally {
        window.clearTimeout(requestTimeout);
      }
      await new Promise((resolve) => window.setTimeout(resolve, Math.min(500, Math.max(0, deadline - Date.now()))));
    }
    return null;
  }

  // 打开项目文件。它是**会话视图的子态**，不另压一层历史。
  //
  // 为什么不压：`exitConversationView` 靠一次 `history.back()` 退掉会话层，而
  // `history.back()` 是异步的、退掉哪一层由浏览器决定。文件视图若也压一层，
  // 那一次 back() 会先退掉文件层，可会话层的标记已经被清掉了 —— 用户停在会话视图里，
  // 而"再按一次返回"已经没有层可退。文件视图并不需要"能被系统手势退出"这个语义
  // （返回键由我们自己接），所以按"面板"处理更稳：返回 = 关面板。
  function openFiles(initialPath?: string) {
    setHeaderMenuOpen(false);
    // 只有明确的字符串才算"要打开某个文件"：菜单项把它当 onClick 用时收到的是事件对象，
    // 直接传下去会让面板拿一个 MouseEvent 去查文件。
    setFilesInitialPath(typeof initialPath === "string" && initialPath.trim() ? initialPath.trim() : null);
    setSessionSub("files");
  }

  // 打开 Git 工作台。同样是一个**会话视图的子态**，不另压一层历史（理由同上）。
  function openGit() {
    setHeaderMenuOpen(false);
    setSessionSub("git");
  }

  // 关闭文件面板回到会话。**不碰历史**：会话那一层原样留着。
  function closeFiles() {
    setSessionSub(null);
    filesGuardRef.current = null;
  }

  // 关闭 Git 工作台回到会话。同样不碰历史。守卫（"里面还有没有上一层"）要清 ——
  // 留着会指向一个已经卸载的面板。
  function closeGit() {
    setSessionSub(null);
    gitPanelRef.current = null;
  }

  // 「刷新仓库状态」= 让工作台重读一遍仓库。
  //
  // **不能**复用页面上那颗「刷新」（它做的是重新同步云端快照）：那两件事语义不同，
  // 而且快照里根本没有仓库状态。刷新按钮就在 GitBar 上，这里只是顶栏菜单的同一件事。
  function reloadGit() {
    gitPanelRef.current?.reload();
  }

  // 手机端「刷新」= 重新对账：清掉适配器缓存 + 重建整个文件面板。
  //
  // 重建会丢掉正在编辑的内容，所以**先走面板自己那条"放弃未保存的更改？"确认** ——
  // 复用同一条流程，而不是在这里另弹一个提示（两套提示早晚有一天措辞与行为不一致）。
  function reloadFiles() {
    const proceed = () => {
      filesAdapter.invalidateAll();
      setFilesEpoch((value) => value + 1);
    };
    const guard = filesGuardRef.current;
    if (guard) guard(proceed);
    else proceed();
  }

  // 文件面板登记的导航守卫。它在"有没有未保存的编辑"每次变化时重新登记，
  // 关面板时也要清掉，否则会指着一个已经卸载的面板。
  // useCallback 不能省：面板那个 effect 把它写进了依赖，每次渲染换一个身份会让
  // 守卫被反复登记/清空（"放弃未保存的更改"那条链会在渲染之间短暂变成 null）。
  const registerFilesGuard = useCallback((guard: NavigationGuard | null) => {
    filesGuardRef.current = guard;
  }, []);

  // 图片字节的取数入口。同样必须记忆化 —— 面板里的 `useMobileMedia` 把它写进依赖，
  // 每次渲染换一个新对象会让每张图都重新解析一遍（连自造的死循环）。
  const filesMedia = useMemo(() => ({ resolve: (path: string) => filesAdapter.resolveMedia(path) }), [filesAdapter]);

  // 只切视图、不动历史：给"历史已经退过了"的那条链路用。
  function leaveConversationView() {
    setMobileView("projects");
    setTasksOpen(false);
    // 侧滑返回会走 popstate → 这里：视图退到项目列表时，压在上面的弹窗必须一起关掉，
    // 否则回到项目列表后还挂着一个"选会话"弹窗（任务抽屉 / 顶栏 ⋯ 菜单 / 快捷方式确认框同理）。
    setConversationHistoryOpen(false);
    setHeaderMenuOpen(false);
    // 确认框尤其不能漏：它的 backdrop 是 position: fixed，会直接盖在项目列表上，
    // 而里面的「执行」是真会往电脑端发命令的（实测：漏掉这一行时，退出视图后仍能点执行）。
    setConfirmShortcut(null);
    // 「编辑任务 / 删除任务」这两张弹层同样是 position: fixed 的 backdrop，同样只在会话视图里
    // 有意义。原来这条链漏了它们：在任务队列里点了编辑再侧滑返回，视图已经退回项目列表，
    // 而编辑弹窗还浮在项目列表上（返回键那条链里有两行，这里没有 —— 两处必须一致）。
    setEditingTask(null);
    setDeletingTask(null);
    setCreatingTask(false);
    // 消息动作图标行（2026-09-20）：它虽然不是浮层，但状态是"某条消息的 id"，
    // 而这条链**不走 backHandlerRef** —— 漏掉的话侧滑退回项目列表后 id 还留着，
    // 而它对应的气泡已经不在渲染里了（再进会话时又会凭空冒出一排图标）。
    // 与配对页不同，它没有"要继续等下去"的语义，所以连复制反馈一起清干净。
    closeMessageActions();
    // 「解除绑定」确认框同理：它也是 fixed backdrop，而且里面的「确认解除」会真的吊销令牌。
    setConfirmUnbind("");
    // 「备注」弹层同样：fixed backdrop（z-index 30，比面板更高），侧滑返回时必须在这里收，
    // 否则回到项目列表后它还浮在屏上。这条链**不走 backHandlerRef**，两处必须一致。
    closeAliasEditor();
    // 底部「选择电脑」面板是整层浮层，侧滑返回时不收掉会盖在项目列表上。
    setDevicesOpen(false);
    // 工具面板也要收：它渲染在会话块内部，退出视图后看不见，但状态还在 —— 重新进入
    // **同一个**项目时 selectedProject/selectedConversation 都没变，下面那个清理 effect
    // 不会重跑，面板就会自己冒出来。
    setComposerToolsOpen(false);
    // 取景层同样是 position: fixed 的整层浮层，侧滑返回时必须在**这里**收掉：popstate
    // 链路不走 backHandlerRef，漏掉它就会带着一个"正在扫码"的整层浮层退回项目列表，
    // 而且摄像头一直开着。
    closeScanOverlay();
    // 配对页（2026-09-20）同理：它也是 position: fixed 的一整页。状态行**不一起收** ——
    // 等待电脑确认那条要留下来（见 `closePairingPanel`），否则用户退回来之后
    // 页面会变成"什么都没有发生"，而配对其实还在进行。
    setPairingExpanded(false);
    // 会话子态（文件 / Git）是会话视图的**一部分**（见 openFiles），退出会话视图时
    // 必须一起收掉。漏掉它的症状与上面那些浮层同族：回到项目列表后状态还留着，
    // 下次进同一个项目会自己冒出来（清理 effect 只按 selectedProject/selectedConversation
    // 变化触发）。
    // 两个守卫也要清 —— `closeFiles` 会清文件那个，这条链走的是 `setSessionSub`
    // 而不是它，两处必须一致：留着会指向一个已经卸载的面板，"放弃未保存的更改"
    // 那条链会拿着它去问一个不存在的编辑态。Git 那个守卫指向的是同一类东西。
    setSessionSub(null);
    filesGuardRef.current = null;
    gitPanelRef.current = null;
  }

  // 进入项目会话视图：压一层历史，让系统侧滑/浏览器返回键有东西可退。
  function enterConversationView() {
    if (!conversationHistoryRef.current) {
      const current = window.history.state;
      const base = current && typeof current === "object" ? current : {};
      // react-router 把 { usr, key, idx } 存在 history.state 里，压历史时必须保留它并把 idx 递增，
      // 否则路由按 idx 差值计算"退了几步"会算错。标记位只用于识别"这一层是我们压的"。
      window.history.pushState({
        ...base,
        ...(typeof base.idx === "number" ? { idx: base.idx + 1 } : {}),
        mileviaMobileConversation: true,
      }, "");
      conversationHistoryRef.current = true;
    }
    setMobileView("conversation");
  }

  // 离开项目会话视图（页内「←」、安卓返回键、项目消失、重新配对）：视图立刻切回项目列表，
  // 同时把我们压的那层历史退掉。注意"退历史"只在这里做——系统侧滑触发的那次 popstate 里
  // 历史已经退过了，再退一次会把用户直接带出应用。
  function exitConversationView() {
    leaveConversationView();
    if (conversationHistoryRef.current) {
      conversationHistoryRef.current = false;
      window.history.back();
    }
  }

  function goBack() {
    if (mobileApp && mobileView === "conversation" && filesOpen) {
      // 与返回键同一条顺序：先退面板里的那一层，退不动了才关面板。
      if (filesPanelRef.current?.showTree()) return;
      closeFiles();
      return;
    }
    if (mobileApp && mobileView === "conversation" && gitOpen) {
      if (gitPanelRef.current?.showTopLevel()) return;
      closeGit();
      return;
    }
    if (mobileApp && mobileView === "conversation") {
      exitConversationView();
      setPairingExpanded(false);
      return;
    }
    if (window.history.length > 1 && !Capacitor.isNativePlatform()) {
      navigate(-1);
    } else if (Capacitor.isNativePlatform()) {
      void CapacitorApp.minimizeApp().catch(() => undefined);
    } else if (!Capacitor.isNativePlatform()) {
      navigate("/");
    }
  }

  // 会话视图专属的顶栏内容：标题固定用**项目名**。会话名不是"名字"而是**首条用户消息截出来的
  // 前 80 字**（编排会话更是直接拿任务标题拼的），它会随对话内容变、长度不受控 —— 挂在顶栏这行
  // 最大的字上，用户读到的是一句半截的话，而不是"我在哪儿"。项目名才是这一屏要回答的问题；
  // 具体是哪条会话，⋯ 菜单里的「历史会话」已经能看。
  // 三个会话入口与刷新收进 ⋯ 菜单。未完成任务数做成 ⋯ 上的小角标 —— 它是"一眼可见的状态"，
  // 收进菜单就等于看不见了。
  //
  // 两个条件分开写：title 只看视图（project 短暂消失时退到兜底文案，别闪回品牌名），
  // active 要求 project 存在（菜单每一项都要读 project.tasks / project.id，缺了会整块崩）。
  const showConversationTitle = Boolean(mobileApp && mobileView === "conversation");
  const conversationHeaderActive = Boolean(showConversationTitle && project);
  // 子态打开时顶栏要说的是"我在看文件 / 我在看 Git"，不是项目名 —— 与 showConversationTitle
  // 同源，因为它们都是会话视图的子态。两者由同一个槽位派生，不可能同时为真。
  const filesHeaderActive = Boolean(conversationHeaderActive && filesOpen);
  const gitHeaderActive = Boolean(conversationHeaderActive && gitOpen);
  const subHeaderActive = filesHeaderActive || gitHeaderActive;
  // 「←」的去处在两个视图里不同：看子态时是回会话，在会话里是回项目列表。
  // 写成一处判据，免得两处 aria/文案各说一半。
  const mobileBackLabel = subHeaderActive ? "返回会话" : mobileView === "conversation" ? "返回项目" : "返回";
  // 「还没跑完」＝ done / cancelled 之外的一切，与共享层 filterQueueTasks(…, "active") 同一口径。
  // 手机端的 Task 是从快照裁剪出来的精简类型（status 只是 string），接不上那份函数的 Task 类型，
  // 所以这里就地写一遍；将来改口径时两处必须一起改。
  const activeTaskCount = conversationHeaderActive ? (project?.tasks || []).filter((task) => task.status !== "done" && task.status !== "cancelled").length : 0;
  // 「就绪」是默认态、没有任何信息量，不占位；其余状态（排队中 / 执行中 / 已完成 / 失败 / 已停止）
  // 都值得一眼看到，尤其"失败"和"已停止" —— 用户需要知道上次为什么没跑完。
  const conversationState = conversation?.status && conversation.status !== "idle" ? conversationStatusLabel(conversation.status) : "";

  // 「＋」面板里与电脑端同源的三组内容。
  //
  // 可见性过滤放在手机端，是因为快照只发一份快捷方式库（用 projectIds 表达"绑定到哪些项目"），
  // 复制到每个项目里会让同一份数据在快照中出现多个版本。判据与电脑端 listShortcuts 的 SQL
  // 逐字对应：`scope='local' or 绑定到本项目`。
  //
  // **唯一的有意分叉**：已停用（enabled=false）的条目这里直接不显示，而电脑端是"灰显 + 可点进
  // 编辑器改"（它拉的是 ?includeDisabled=true）。手机端没有快捷方式编辑器，露出一条永远点不动
  // 的胶囊只是噪音 —— 停用状态要到电脑上改，手机端只负责"能用的那些"。（两侧的**排序**必须是
  // 同一份：见 control-server remoteSnapshotShortcuts 的 order by。）
  // 分组边界照抄桌面端：prompt + snippet 归「常用提示词」，command_request 归「常用命令」。
  const projectShortcuts = (snapshot?.shortcuts || []).filter((item) => item.enabled && (item.scope === "local" || item.projectIds.includes(project?.id || "")));
  const promptShortcuts = projectShortcuts.filter((item) => item.kind === "prompt" || item.kind === "snippet");
  const commandShortcuts = projectShortcuts.filter((item) => item.kind === "command_request");
  const projectSkills = project?.skills || [];
  // 技能按来源分组，只保留有内容的组；顺序固定为 项目 > 用户 > 系统/官方。
  const skillGroups = skillSourceOrder.map((source) => ({ source, label: skillSourceLabels[source], items: projectSkills.filter((skill) => skill.source === source) })).filter((group) => group.items.length > 0);

  // ── 会话页空态（2026-09-17 重做）────────────────────────────────────────────
  // 旧版是消息列里一行 12px 灰字（「该会话暂无对话内容。」／「请先新建会话。」）：不说是哪个 Agent、
  // 不说下一步能干什么，而「新建会话」的唯一入口还藏在顶栏 ⋯ 里 —— 新项目点进来就是个死胡同。
  // 现在按「**有下一步动作**的空态」处理：白卡 + 圆形图标 + 一句说明 + 可点起手，
  // 与任务队列的 `.mobile-task-empty` 同一套视觉语言（见 MOBILE-UI.md「会话页空态禁止一行灰字」）。
  //
  // 三条判据，缺一条就会说错话：
  //   ① `conversation` 为空 → 这个项目一条会话都没有，空态的主体必须是「新建会话」主按钮；
  //   ② `conversationProcessing` → Agent 正在跑，这时还说"还没有消息"是自我打脸，
  //      而下面那条 `.mobile-agent-processing` 状态条已经把话说完了，所以**一个空态都不渲染**；
  //   ③ 其余 → 有会话、没消息，给起手。
  //
  // 起手胶囊只收「点下去只把内容填进输入框」的提示词（`shortcutAction === "fill"`）：
  // run / confirm 那一类会在电脑端**真执行命令**，摆在空态上误触的代价太高。
  // 按名字取前 3 条 —— 顺序由服务端给（与「＋」面板同一份），手机端不重排。
  const emptyStarters = promptShortcuts.filter((item) => shortcutAction(item) === "fill").slice(0, 3);
  // 「全部工具 · N 个技能 · N 条提示词 · N 条命令」里的计数部分：0 的那一项直接不出现，
  // 免得写成「0 个技能」（与「＋」面板里"组标题带条数、0 也照显示"是有意分叉 ——
  // 那里是面板正文要交代数据来源，这里只是一句入口后缀，塞不下三个 0）。
  const emptyToolSummary = [
    projectSkills.length > 0 ? `${projectSkills.length} 个技能` : "",
    promptShortcuts.length > 0 ? `${promptShortcuts.length} 条提示词` : "",
    commandShortcuts.length > 0 ? `${commandShortcuts.length} 条命令` : "",
  ].filter(Boolean).join(" · ");
  const conversationEmpty = !conversation ? (
    <div className="mobile-empty-start" data-kind="no-conversation">
      <span className="mobile-empty-start-mark" aria-hidden="true">
        <svg viewBox="0 0 24 24"><path d="M20 12a8 8 0 0 1-8 8H7l-3 3V12a8 8 0 0 1 8-8 8 8 0 0 1 8 8Z" /><path d="M12 8.5v6" /><path d="M9 11.5h6" /></svg>
      </span>
      <strong>这个项目还没有会话</strong>
      <p>新建一条会话就能和 Agent 对话。会话留在电脑上，可以随时回来接着聊。</p>
      <button type="button" className="mobile-empty-start-primary" disabled={busy} onClick={() => { if (project) openNewConversation(project); }}>新建会话</button>
      <small className="mobile-empty-start-note">创建时可以选择用 Claude Code 还是 Codex 执行。</small>
    </div>
  ) : conversationProcessing ? null : (
    <div className="mobile-empty-start" data-kind="no-message" data-tools={composerToolsOpen ? "open" : "closed"}>
      <span className="mobile-empty-start-mark" aria-hidden="true">
        <svg viewBox="0 0 24 24"><path d="M20 12a8 8 0 0 1-8 8H7l-3 3V12a8 8 0 0 1 8-8 8 8 0 0 1 8 8Z" /><path d="M9 11h6" /><path d="M9 15h3" /></svg>
      </span>
      <strong>和 {conversationAgentLabel(conversation.agentId)} 开始对话</strong>
      <p>在下面的输入框里写下要做的事，Agent 会在这台电脑上执行。{emptyStarters.length > 0 ? "也可以先挑一条：" : ""}</p>
      {emptyStarters.length > 0 && <div className="mobile-empty-starters">
        {/* 文案外面必须套一层 span：`<button>` 在 Chromium 里会把内容包成匿名 flex 项，
            直接写在按钮上的 `text-overflow: ellipsis` 对它不生效（长提示词名会硬切）；
            套一层之后由这个 span 自己撑省略号。见 mobile-remote.css 里那段说明。 */}
        {emptyStarters.map((item) => <button type="button" key={item.id} disabled={busy || Boolean(shortcutBusy)} onClick={() => void applyShortcut(item)} title={item.template}><span>{shortcutBusy === item.id ? "填入中…" : item.name}</span></button>)}
      </div>}
      {/* 入口只负责把输入条上方那个面板打开，不顺手聚焦输入框 —— 用户是来翻工具的，
          一聚焦就把软键盘顶上来，反而挡住了要看的东西。 */}
      <button type="button" className="mobile-empty-tools" disabled={busy} onClick={() => setComposerToolsOpen(true)} aria-expanded={composerToolsOpen} aria-controls="mobile-composer-tools">
        <svg viewBox="0 0 24 24" aria-hidden="true" focusable="false"><path d="M12 5v14" /><path d="M5 12h14" /></svg>
        <span>全部工具{emptyToolSummary ? ` · ${emptyToolSummary}` : ""}</span>
      </button>
    </div>
  );
  // 空态**真正上屏**的条件，三个一起成立才算：
  //   ① 会话视图真的开着 —— 类名只加在 `<main>` 上，而 `<main>` 是**整页共用**的。项目列表
  //      那一屏（`mobileView === "projects"`）根本没有消息列，若只按"时间线为空"判断，
  //      那里也会挂上 `mobile-empty-mode`（会话为空时它确实成立），类名就成了谎话。
  //      当前 CSS 侥幸没炸只是因为每条规则都带 `.mobile-conversation-mode` 前缀 —— 靠侥幸
  //      不算防线，一旦有人补一条裸 `.mobile-empty-mode {}` 就会在项目列表上生效。
  //   ② 消息列里一条时间线都没有；
  //   ③ 不是「Agent 正在跑」那一种（见上面 ②）。
  // 单独算一个布尔给 `data-empty` 用 —— 直接用 conversationEmpty 会被读成"只要这个节点存在
  // 就该居中"，于是**有消息的会话也带上 data-empty**（探针抓到的第一条）。
  const conversationViewActive = Boolean(mobileApp && mobileView === "conversation" && project);
  const showConversationEmpty = conversationViewActive && conversationTimeline.length === 0 && conversationEmpty !== null;
  // 同步给输入条那个 ResizeObserver 用（它挂在 `[mobileView, project?.id]` 上，
  // 闭包里拿不到最新的这个布尔）。放在 effect 里而不是渲染期直写 ref：effect 在浏览器
  // 布局前就跑完了，用户不可能在它之前点开工具面板。
  useEffect(() => { emptyThreadRef.current = showConversationEmpty; }, [showConversationEmpty]);

  // ── 电脑端那一屏的派生值（2026-09-17 方案 B）──────────────────────────────────
  // 这一屏与手机页共用组件，但**数据来源完全不同**：它没有云端令牌，拿不到快照、也发不出命令。
  // 所以它只答三个问题 —— 本机的远程服务在不在跑、怎么把手机绑上来、现在绑的是谁。
  //
  // 五档状态（读取中 / 读不到 / 未注册 / 运行中 / **无心跳**）的判定与文案
  // 都在 `features/remote/desktop-service.ts` 里 —— 那是一条优先级级联，用行为断言守着。
  // `ready` 只说明凭据在不在：Agent 进程崩了它依然是 true，那时手机端什么都收不到，
  // 而旧版会显示"已就绪"（stale 这一档就是补这个洞；它的**措辞**不许咬定是哪种原因，
  // 见那个模块里的注释）。
  const desktopService = desktopServiceView(agentStatusState, agentStatus, heartbeatAgeMs);
  // 主栏是"现在该做的动作"：注册引导 → 配对。**表单只有这一处**，
  // 侧栏的「重新注册」只负责把主栏切回注册态。
  //
  // ⚠️ 必须先问"读到了吗"再问"注册了吗"：`agentStatus` 还没回来时它是 null，
  // `!agentStatus?.ready` 同样是 true —— 直接拿它当"未注册"，主栏会在**冷启动那几秒**
  // 摆出一张"请粘贴部署注册令牌"的表单，而页头与侧栏同一时刻写着"读不到本机服务状态"：
  // 两个互相矛盾的真相同屏（首次打开大库时控制服务要做迁移，最长可能几十秒）。
  // 所以"注册引导"这条路的入口条件写成"读到了、而且确实未注册"。
  const desktopNeedsEnroll = agentStatusState === "loaded" && (!agentStatus?.ready || reenrollOpen);
  const desktopInstanceID = agentStatus?.instanceId || "";
  const boundPhone = bindings[0] || null;
  // 「这台手机现在还在不在用」。四档（没读到 / 云端不提供 / 没同步过 / 在线·未同步）
  // 的判据在纯模块里，页面只负责把年龄递给它。
  const phoneSync = phoneSyncView(boundPhone, phoneSyncAgeMs);
  const boundPhonePlatform = platformLabel(boundPhone?.platform);

  return <main className={`mobile-remote${mobileApp ? "" : " desktop-remote"} ${mobileApp && mobileView === "conversation" ? "mobile-conversation-mode" : ""}${showConversationEmpty ? " mobile-empty-mode" : ""}`}>
    {editingTask && <div className="mobile-task-modal-backdrop" role="dialog" aria-modal="true" aria-labelledby="mobile-task-edit-title"><form className="mobile-task-modal" onSubmit={(event) => void saveTaskEdit(event)}><header><h2 id="mobile-task-edit-title">编辑任务</h2><button type="button" onClick={() => setEditingTask(null)} disabled={busy} aria-label="关闭">×</button></header><label>标题（可选）<input value={editTitle} onChange={(event) => setEditTitle(event.target.value)} placeholder="留空时用描述代替" /></label><label>描述<textarea value={editDescription} onChange={(event) => setEditDescription(event.target.value)} required rows={4} /></label><MobilePriorityPicker name="mobile-task-edit-priority" value={editPriority} onChange={setEditPriority} />{editError && <p className="mobile-task-modal-error" role="alert">{editError}</p>}<footer><button type="button" onClick={() => setEditingTask(null)} disabled={busy}>取消</button><button type="submit" disabled={busy}>保存</button></footer></form></div>}
    {deletingTask && <div className="mobile-task-modal-backdrop" role="dialog" aria-modal="true" aria-labelledby="mobile-task-delete-title"><section className="mobile-task-modal mobile-task-delete-modal"><header><h2 id="mobile-task-delete-title">删除任务</h2><button type="button" onClick={() => setDeletingTask(null)} disabled={busy} aria-label="关闭">×</button></header><p>确定删除“{taskSummary(deletingTask)}”吗？删除后无法恢复。</p><footer><button type="button" onClick={() => setDeletingTask(null)} disabled={busy}>取消</button><button type="button" className="mobile-task-delete-confirm" onClick={() => void confirmTaskDelete()} disabled={busy}>确认删除</button></footer></section></div>}
    {/* 新建任务：入口只有面板右下角那颗加号，表单收进这个弹层。复用编辑/删除那两张弹层的
        `.mobile-task-modal*` 样式与结构（居中小卡 + 头部 × + 底部主次按钮），不另造一套视觉；
        字段与提交仍走原来的 title/description 状态与 createTask。 */}
    {creatingTask && <div className="mobile-task-modal-backdrop" role="dialog" aria-modal="true" aria-labelledby="mobile-task-create-title"><form className="mobile-task-modal" onSubmit={(event) => void createTask(event)}><header><h2 id="mobile-task-create-title">新建任务</h2><button type="button" onClick={() => setCreatingTask(false)} disabled={busy} aria-label="关闭">×</button></header><label>标题（可选）<input value={title} onChange={(event) => setTitle(event.target.value)} placeholder="留空时用描述代替" /></label><label>描述<textarea value={description} onChange={(event) => setDescription(event.target.value)} required placeholder="说明背景、范围和要完成的事" rows={3} /></label><MobilePriorityPicker name="mobile-task-create-priority" value={createPriority} onChange={setCreatePriority} />{createError && <p className="mobile-task-modal-error" role="alert">{createError}</p>}<footer><button type="button" onClick={() => setCreatingTask(false)} disabled={busy}>取消</button><button type="submit" disabled={busy}>创建</button></footer></form></div>}
    {confirmShortcut && <div className="mobile-task-modal-backdrop" role="dialog" aria-modal="true" aria-labelledby="mobile-shortcut-confirm-title"><section className="mobile-task-modal mobile-task-delete-modal"><header><h2 id="mobile-shortcut-confirm-title">执行「{confirmShortcut.name}」</h2><button type="button" onClick={() => setConfirmShortcut(null)} disabled={busy || Boolean(shortcutBusy)} aria-label="关闭">×</button></header><p>这条命令会在电脑端执行。模板内容：</p><pre className="mobile-shortcut-confirm-template">{confirmShortcut.template}</pre><footer><button type="button" onClick={() => setConfirmShortcut(null)} disabled={busy || Boolean(shortcutBusy)}>取消</button><button type="button" className="mobile-task-delete-confirm" onClick={() => void confirmShortcutRun()} disabled={busy || Boolean(shortcutBusy)}>执行</button></footer></section></div>}
    {/* 扫码取景层。必须是 <main> 的**直接子元素**：清底时 `html.barcode-scanner-active
        .mobile-remote > *:not(.mobile-scan)` 会把其余同级块整体不绘制，而取景窗要靠
        "挖空 + 100vmax 影子"透出画面 —— 一旦被嵌进配对面板里，就会连取景窗一起被隐藏
        （旧实现正是嵌在 .mobile-pairing 内部，靠两条补丁规则才勉强透出来）。

        渲染条件带上 scanError：相机起不来时 `scanning` 会立刻回 false，如果只按 scanning
        渲染，用户只会看到取景层闪一下消失、连一句"为什么"都没有（旧实现把提示挂在配对面板
        里，扫码期间那整块是 visibility: hidden，等于从来没提示过）。留在这一层里才能既说明
        原因、又给"重新扫描 / 打开系统设置"。

        `data-phase` 是给"矮视口 + 失败态"用的：失败态多出一张报错卡，会把取景窗挤小到
        222px（844×390）/ 76px（720×200），而这时窗口里本来就没有画面、只写着"相机未开启"。
        极矮视口下由 CSS 按这个属性把空框收掉（见 mobile-remote.css 的第二级媒体查询）。 */}
    {(scanOverlayVisible) && <div className="mobile-scan" data-backdrop={scanLiveNative ? "camera" : "solid"} data-phase={scanning ? "live" : "error"} role="dialog" aria-modal="true" aria-label="扫描配对二维码">
      {/* 原生分支不渲染 <video>：画面由原生插件画在 WebView 之下，页面上只需要留一个"洞"。 */}
      {!nativeScanPlatform && <video className="mobile-scan-video" ref={setScanVideo} muted playsInline autoPlay />}
      {/* DOM 顺序＝列方向上的视觉顺序，必须是「文案 → 遮罩 → 底部按钮」：
          遮罩是这一段里**会伸缩**的部分（flex: 1），它要待在文案与按钮之间；
          放在文案前面就会把文案挤到取景窗下面去（改布局时踩过一次）。 */}
      <div className="mobile-scan-text">
        <strong>{scanning ? "将电脑上的二维码放入框内" : "扫码没有成功"}</strong>
        <small>{scanning ? "二维码在电脑端「扫码配对」里生成；扫到后在电脑上点击「确认绑定」即可。" : "可以重新扫描，也可以在下面改用 6 位校验码完成配对。"}</small>
      </div>
      <div className="mobile-scan-mask" aria-hidden="true">
        <div className="mobile-scan-window">
          <i className="mobile-scan-corner top-left" /><i className="mobile-scan-corner top-right" />
          <i className="mobile-scan-corner bottom-left" /><i className="mobile-scan-corner bottom-right" />
          {/* 失败态的取景窗里没有画面（相机根本没起来）。空着会被读成"卡住了"，
              所以补一句状态说明；扫描中这里只放扫描线。 */}
          {scanning ? <i className="mobile-scan-laser" /> : <span className="mobile-scan-window-note">相机未开启</span>}
        </div>
      </div>
      <div className="mobile-scan-footer">
        {/* 报错必须画在取景层内部：旧实现的提示挂在配对面板里，而扫码期间配对面板整体
            visibility: hidden —— "扫到的不是配对码"这类提示因此从来没被看见过。 */}
        {scanError && <p className="mobile-scan-error" role="alert"><span>{scanError}</span>{scanNeedsSettings && <button className="mobile-scan-settings" type="button" onClick={() => void openScannerSettings()}>打开系统设置</button>}</p>}
        <div className="mobile-scan-actions">
          {scanning && scanTorchAvailable && <button className="mobile-scan-torch" type="button" aria-pressed={scanTorchOn} aria-label={scanTorchOn ? "关闭手电筒" : "打开手电筒"} title={scanTorchOn ? "关闭手电筒" : "打开手电筒"} onClick={() => void toggleScanTorch()}><svg viewBox="0 0 24 24" aria-hidden="true" focusable="false" fill="none" stroke="currentColor" strokeWidth="1.9" strokeLinecap="round" strokeLinejoin="round"><path d="M13 2 4.5 13H11l-1 9 8.5-11H12l1-9Z" /></svg></button>}
          {scanning
            ? <button className="mobile-scan-cancel" type="button" onClick={closeScanOverlay}>取消扫描</button>
            : <><button className="mobile-scan-cancel" type="button" onClick={closeScanOverlay}>关闭</button><button className="mobile-scan-retry" type="button" onClick={beginPairingScan}>重新扫描</button></>}
        </div>
      </div>
    </div>}
    {mobileApp ? <header className="mobile-remote-header" ref={headerRef}><div className="mobile-remote-title">{(!mobileApp || mobileView === "conversation") && <button className="mobile-back" type="button" onClick={goBack} title={mobileBackLabel} aria-label={mobileBackLabel}><svg viewBox="0 0 24 24" width="18" height="18" aria-hidden="true" focusable="false" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round"><path d="M19 12H5" /><path d="M11 18l-6-6 6-6" /></svg></button>}<div className="mobile-brand"><img className="mobile-brand-mark" src="/milevia-mark.svg" width="36" height="36" alt="" /><h1>{filesHeaderActive ? "项目文件" : gitHeaderActive ? "Git 工作台" : showConversationTitle ? (project?.name || "项目对话") : "Milevia"}</h1></div>{filesHeaderActive && project?.gitBranch && <span className="mobile-files-workspace" title="当前项目分支；手机端快照里还没有每个会话的工作区信息">{project.gitBranch}</span>}{conversationHeaderActive && conversationState && <span className={`mobile-conversation-state mobile-conversation-state-${conversation?.status}`}>{conversationState}</span>}</div><div className="mobile-header-actions">{!conversationHeaderActive && notificationPermission === "default" && <button className="mobile-notification-button" type="button" onClick={() => void enableMobileNotifications()} title="开启后台通知">开启通知</button>}{!conversationHeaderActive && <button className="mobile-refresh" type="button" onClick={() => void refreshNow()} disabled={refreshing} aria-busy={refreshing} title="从电脑端重新同步"><svg className="mobile-refresh-icon" viewBox="0 0 24 24" width="13" height="13" aria-hidden="true" focusable="false" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><polyline points="23 4 23 10 17 10" /><polyline points="1 20 1 14 7 14" /><path d="M3.51 9a9 9 0 0 1 14.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0 0 20.49 15" /></svg><span>刷新</span></button>}{filesHeaderActive && <div className="mobile-header-menu"><button className="mobile-header-menu-button" ref={headerMenuButtonRef} type="button" aria-haspopup="menu" aria-expanded={headerMenuOpen} aria-controls="mobile-files-menu-sheet" onClick={() => setHeaderMenuOpen((open) => !open)} aria-label="文件操作" title="文件操作"><span aria-hidden="true">⋯</span></button>{headerMenuOpen && <div className="mobile-header-menu-sheet" id="mobile-files-menu-sheet" role="menu" aria-label="文件操作"><div className="mobile-header-menu-info"><span>项目文件</span><small>{project?.gitBranch || "项目工作区"}</small></div><button type="button" role="menuitem" onClick={() => { setHeaderMenuOpen(false); reloadFiles(); }}><span>刷新文件</span><small>重新读取目录与内容</small></button></div>}</div>}{gitHeaderActive && <div className="mobile-header-menu"><button className="mobile-header-menu-button" ref={headerMenuButtonRef} type="button" aria-haspopup="menu" aria-expanded={headerMenuOpen} aria-controls="mobile-git-menu-sheet" onClick={() => setHeaderMenuOpen((open) => !open)} aria-label="Git 操作" title="Git 操作"><span aria-hidden="true">⋯</span></button>{headerMenuOpen && <div className="mobile-header-menu-sheet" id="mobile-git-menu-sheet" role="menu" aria-label="Git 操作"><div className="mobile-header-menu-info"><span>Git 工作台</span><small>{project?.gitBranch || "项目工作区"}</small></div><button type="button" role="menuitem" onClick={() => { setHeaderMenuOpen(false); reloadGit(); }}><span>刷新仓库状态</span><small>重新读取变更、分支与提交</small></button></div>}</div>}{conversationHeaderActive && project && !subHeaderActive && <div className="mobile-header-menu"><button className="mobile-header-menu-button" ref={headerMenuButtonRef} type="button" aria-haspopup="menu" aria-expanded={headerMenuOpen} aria-controls="mobile-header-menu-sheet" onClick={() => setHeaderMenuOpen((open) => !open)} aria-label="更多操作" title="更多操作">{activeTaskCount > 0 && <span className="mobile-header-menu-badge" aria-hidden="true">{activeTaskCount}</span>}<span aria-hidden="true">⋯</span></button>{headerMenuOpen && <div className="mobile-header-menu-sheet" id="mobile-header-menu-sheet" role="menu" aria-label="更多操作">{activeDeviceRecord && <div className="mobile-header-menu-info mobile-header-menu-device"><span>当前电脑</span><small>{deviceDisplayName(activeDeviceRecord)} · {activeDeviceRecord.revoked ? "绑定已失效" : activeDeviceRecord.status === "online" ? "在线" : "离线"}</small></div>}{conversation && <div className="mobile-header-menu-info"><span>执行 Agent</span><small>{conversationAgentLabel(conversation.agentId)}</small></div>}<button type="button" role="menuitem" onClick={() => { setHeaderMenuOpen(false); void refreshNow(); }} disabled={refreshing} aria-busy={refreshing}><span>刷新</span><small>{refreshing ? "同步中…" : "重新同步电脑端"}</small></button>{notificationPermission === "default" && <button type="button" role="menuitem" onClick={() => { setHeaderMenuOpen(false); void enableMobileNotifications(); }}><span>开启通知</span><small>后台提醒</small></button>}<div className="mobile-header-menu-separator" role="none" /><button type="button" role="menuitem" onClick={() => openFiles()}><span>项目文件</span><small>查看与编辑</small></button><button type="button" role="menuitem" onClick={() => openGit()}><span>Git 工作台</span><small>变更 · 差异 · 提交</small></button><button type="button" role="menuitem" onClick={() => { setHeaderMenuOpen(false); setConversationHistoryOpen(true); }} disabled={conversations.length === 0}><span>历史会话</span><small>{conversations.length}</small></button><button type="button" role="menuitem" onClick={() => { setHeaderMenuOpen(false); void createConversationForProject(project); }} disabled={busy}><span>新会话</span><small>新建</small></button><button type="button" role="menuitem" onClick={() => { setHeaderMenuOpen(false); setTasksOpen(true); }}><span>任务队列</span><small>{project.tasks.length}</small></button></div>}</div>}</div></header> : <header className="desktop-remote-header">
      <div className="desktop-remote-head-main">
        <button className="desktop-remote-back" type="button" onClick={goBack} aria-label="返回项目总览" title="返回项目总览"><svg viewBox="0 0 24 24" width="18" height="18" aria-hidden="true" focusable="false" fill="none" stroke="currentColor" strokeWidth="1.9" strokeLinecap="round" strokeLinejoin="round"><path d="M19 12H5" /><path d="M11 18l-6-6 6-6" /></svg></button>
        <div className="desktop-remote-head-text">
          {/* kicker 是电脑端管理页的通用写法（设置页 / Agent 档案页同一套）：一行小字带上
              "这是哪一面"，比把品牌名放大成 30px 更能回答"我在哪"。 */}
          <span className="desktop-remote-kicker"><svg viewBox="0 0 24 24" width="13" height="13" aria-hidden="true" focusable="false" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round"><rect x="3" y="4" width="18" height="12" rx="1.5" /><path d="M8 20h8" /><path d="M12 16v4" /></svg>REMOTE CONTROL</span>
          <h1>远程控制</h1>
        </div>
      </div>
      <div className="desktop-remote-head-actions">
        <span className="desktop-remote-service" data-state={desktopService.state} role="status"><i className="desktop-remote-led" aria-hidden="true" />{desktopService.label}</span>
        {/* 「被手机连着」这件事必须在**页头**就能看见：进这一页的人有一半是为了确认
            "手机现在能不能用"，而侧栏那张卡要低头找。页头这颗胶囊说的就是侧栏同一份读数
            （`phoneSync.headerChip`），没绑定时整颗不渲染 —— 不摆一句"未绑定"占位。
            轮询重读失败时改说「读不到手机状态」：继续说「手机在线」是拿旧读数冒充新读数，
            改说「手机未同步」则是把**我们读不到**栽赃给手机（用户会跑去手机上找原因）。
            "不知道"就必须说成"不知道"。措辞与页头那句「读不到本机服务状态」同构。
            `data-state` 给颜色，`role=status` 让读屏也能听到它的变化。 */}
        {bindingsState === "loaded" && boundPhone && <span className="desktop-remote-phone-chip" data-state={bindingsStale ? "stale" : phoneSync.state} role="status"><i className="desktop-remote-led" aria-hidden="true" />{bindingsStale ? "读不到手机状态" : phoneSync.headerChip}</span>}
        <button className="desktop-remote-btn ghost" type="button" onClick={() => void refreshDesktopStatus()} disabled={refreshing} aria-busy={refreshing}>刷新状态</button>
      </div>
    </header>}
    {refreshStatus && <div className={`mobile-refresh-status mobile-refresh-status-${refreshStatus.state}`} role="status">{refreshStatus.state === "refreshing" && <span className="mobile-refresh-spinner" aria-hidden="true" />}<span className="mobile-refresh-status-text">{refreshStatus.message}</span>{refreshStatus.at && <time dateTime={refreshStatus.at.toISOString()}>{refreshStatus.at.toLocaleTimeString()}</time>}</div>}
    {mobileUpdate && !updateDismissed && <section className="mobile-update" role="status"><div className="mobile-update-text"><strong>发现新版本 v{mobileUpdate.release.version}</strong><small>当前 v{mobileUpdate.currentVersion}{mobileUpdate.release.size ? ` · ${(mobileUpdate.release.size / 1024 / 1024).toFixed(1)} MB` : ""}{mobileUpdate.release.notes ? ` · ${mobileUpdate.release.notes.slice(0, 40)}` : ""}</small></div><a className="mobile-update-action" href={mobileUpdate.release.url} target="_blank" rel="noreferrer">立即更新</a><button className="mobile-update-dismiss" type="button" onClick={() => setUpdateDismissed(true)} aria-label="稍后提醒" title="稍后提醒">✕</button></section>}
    {/* 首启（这台手机还没有任何电脑）：给一张空态卡，而不是把配对表单摊在项目列表上。
        与任务队列 / 会话页的空态同族（白底 + 圆形图标 + 一行说明 + 一颗主按钮）；
        没有可选择的项目时连「选择项目」那一段一起不渲染 —— 摆一个空标题加一行灰字
        是"有下一步动作的空态禁止一行灰字"那条规则里点名不许出现的东西。 */}
    {showPairingStart && <section className="mobile-pairing-start">
      <span className="mobile-pairing-start-icon" aria-hidden="true"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round"><rect x="3" y="4" width="18" height="12" rx="1.5" /><path d="M8 20h8" /><path d="M12 16v4" /></svg></span>
      <h2>还没有连接电脑</h2>
      <p>在电脑端打开「远程控制」生成二维码，用这台手机扫一下就能连上。</p>
      <button type="button" onClick={openPairingPanel}>＋ 添加电脑</button>
    </section>}
    {/* 配对页（整页浮层，与任务面板同一档 z-index）。它取代了以前"长在项目列表上面的两块"：
        一块「扫码配对」、一块「使用校验码」，各带一个 h2 —— 读起来像页面有两件事要做，
        实际是同一件事的两种做法，只该有一组入口。
        现在的分工：主按钮＝扫码（九成用户的路径，实心），校验码＝次（描边 + 六格输入）。
        三步指示器是这一版存在的理由 ——「等待电脑确认」那段时间必须有地方待着，
        而它恰恰是整条流程最容易卡住的地方（手机扫完其实什么都没发生）。
        电脑端**不复用**这一页（2026-09-17 方案 B）：电脑端要的是"大二维码 + 右侧状态栏"，
        全在 desktop-remote 那套里。 */}
    {showMobilePairing && <section className="mobile-pairing-page" ref={pairingPageRef} tabIndex={-1} role="dialog" aria-modal="true" aria-labelledby="mobile-pairing-title">
      <header className="mobile-pairing-page-head">
        <button className="mobile-pairing-back" type="button" onClick={closePairingPanel} title="返回项目" aria-label="返回项目"><svg viewBox="0 0 24 24" width="18" height="18" aria-hidden="true" focusable="false" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round"><path d="M19 12H5" /><path d="M11 18l-6-6 6-6" /></svg></button>
        <h2 id="mobile-pairing-title">添加电脑</h2>
        <button className="mobile-pairing-close" type="button" onClick={closePairingPanel} aria-label="关闭">×</button>
      </header>
      <ol className="mobile-pairing-steps">
        <li data-state={pairingStep > 1 ? "done" : "current"}><i>1</i>扫码 / 输码</li>
        <li data-state={pairingStep > 2 ? "done" : pairingStep === 2 ? "current" : "todo"}><i>2</i>电脑上确认</li>
        <li data-state={pairingStep === 3 ? "done" : "todo"}><i>3</i>连上</li>
      </ol>
      <div className="mobile-pairing-page-body">
        {/* 页内那一处挂载点：只服务 idle / error，见 `pairingPageNotice` 上面那段说明。 */}
        {pairingPageNotice}
        {pairingStep === 2
          ? <div className="mobile-pairing-wait" role="status">
            <span className="mobile-pairing-wait-spin" aria-hidden="true" />
            <strong>已提交，等待电脑确认</strong>
            <small>请到电脑上点「确认绑定」。手机这边不用再操作，电脑点完会自动连上。</small>
            {/* 「先返回项目」不是"取消配对"（手机上也没有取消配对这个接口）：会话还在云端挂着，
                电脑那边点确认依然生效 —— 回到项目列表后页面级那条等待回执会接着转。
                措辞必须说清这一点，写成「取消」会让人以为配对真的被撤销了。 */}
            <button type="button" onClick={closePairingPanel}>先返回项目</button>
          </div>
          : <>
            {/* 这里刻意**不画取景框**：原生分支的摄像头由插件画在 WebView 之下，页面里再画一个
                "框"只会是一块假的取景区（真画面只在 `.mobile-scan` 那层透出来）。所以这一格是
                一张说明卡：告诉用户二维码在电脑端的哪一页，而不是假装这里能看到画面。 */}
            <figure className="mobile-pairing-figure">
              <span className="mobile-pairing-figure-mark" aria-hidden="true"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round"><rect x="3" y="3" width="7" height="7" rx="1.5" /><rect x="14" y="3" width="7" height="7" rx="1.5" /><rect x="3" y="14" width="7" height="7" rx="1.5" /><path d="M14 14h3.2v3.2H14z" /><path d="M17.8 17.8H21V21h-3.2z" /><path d="M14 20.4h1.2" /><path d="M20.4 14h.6" /></svg></span>
              <figcaption>
                <b>电脑端：「远程控制」→「生成二维码」</b>
                <small>扫码只是提交申请；真正生效要在那台电脑上点「确认绑定」。</small>
              </figcaption>
            </figure>
            <button className="mobile-pairing-scan-button" type="button" onClick={beginPairingScan} disabled={busy || scanning}>扫描二维码</button>
            <div className="mobile-pairing-or"><span>或者</span></div>
            <form className="mobile-pairing-code" onSubmit={(event) => void claimPairingByCode(event)}>
              <label className="mobile-pairing-code-label" htmlFor="mobile-pairing-code">扫码不方便时，填电脑上显示的 6 位校验码</label>
              {/* 六格输入：一个透明的真 input 盖在 6 个格子上（受控值永远来自它）。
                  真 input 不能被藏起来（display:none / 不可见都会拿不到焦点、也读不出值），
                  所以是"透明 + 覆盖"，焦点指示交给格子容器 :focus-within 画。
                  `aria-label` 必须留着 —— 扫到不带校验码的二维码时要把焦点搬到这里，
                  探针就是按这个标签找它的。 */}
              <div className="mobile-pairing-code-field">
                <input id="mobile-pairing-code" ref={manualCodeRef} inputMode="numeric" pattern="[0-9]{6}" maxLength={6} value={manualPairingCode} onChange={(event) => setManualPairingCode(event.target.value.replace(/\D/g, "").slice(0, 6))} aria-label="6 位校验码" placeholder="" autoComplete="one-time-code" />
                {[0, 1, 2, 3, 4, 5].map((index) => <i key={index} aria-hidden="true" data-filled={manualPairingCode.length > index ? "true" : "false"}>{manualPairingCode[index] || ""}</i>)}
              </div>
              <button className="mobile-pairing-code-submit" type="submit" disabled={busy || manualPairingCode.length !== 6}>验证并配对</button>
            </form>
            <p className="mobile-pairing-tip">扫到码后会停在这一页等你确认 —— 「确认绑定」要在电脑上点。</p>
          </>}
      </div>
    </section>}
    {/* 配对状态行的页面级挂载点：配对页关掉之后它才是唯一的回执。
        **不能在页里也留一份**（重复），也不能只在页里（绑定成功后 showMobilePairing 立刻变 false，
        挂在页里的那句"电脑已确认，绑定完成"会跟着一起卸载，用户什么都看不到 —— 实测过）。
        三态（等待/成功/失败）各自有视觉，不再是一行 12px 绿字。 */}
    {!showMobilePairing && pairingNotice && (!mobileApp || mobileView === "projects") && pairingNoticeBox}
    {error && <div className="mobile-error" role="alert">{error}</div>}
    {/* ── 电脑端那一屏（方案 B：两栏工作台）────────────────────────────────────
        与手机页共用组件、**不共用数据源**：这一屏没有云端令牌，拿不到快照、也发不出命令。
        所以它只答三件事 —— 本机远程服务在不在跑、怎么把手机绑上来、现在绑的是谁。
        主栏＝"现在该做的那件事"（注册引导 / 扫码配对），侧栏＝这台电脑的状态与"谁在用"。
        侧栏两张卡（远程服务 / 已绑定手机）都是**上一版完全没有**的信息：
        旧版就绪时屏幕上没有一个字说明服务状态，`.mobile-instance-status` 的渲染条件还挂着
        一个永远拿不到的 `instance`（它只从需要令牌的 /v1/instances 来）。 */}
    {!mobileApp && <div className="desktop-remote-body">
      <div className="desktop-remote-grid">
        <div className="desktop-remote-main">
          {desktopNeedsEnroll ? <section className="desktop-remote-card amber" aria-labelledby="desktop-remote-enroll-title">
            <header className="desktop-remote-card-head">
              <h2 id="desktop-remote-enroll-title">{agentStatus?.ready ? "重新注册远程服务" : "先完成远程服务注册"}</h2>
              {agentStatus?.ready && <button className="desktop-remote-btn ghost sm" type="button" onClick={() => setReenrollOpen(false)} disabled={agentEnrollBusy}>取消</button>}
            </header>
            <p className="desktop-remote-note">电脑端 Agent 还没有连接到云端，手机此时无法配对。请粘贴管理员提供的部署注册令牌完成一次注册；令牌只在本次注册使用，不会保存到磁盘，也不会进入安装包。</p>
            <form className="desktop-remote-enroll" onSubmit={(event) => void enrollRemoteAgent(event)}>
              <input type="password" value={agentEnrollToken} onChange={(event) => setAgentEnrollToken(event.target.value)} placeholder="部署注册令牌" aria-label="部署注册令牌" autoComplete="off" />
              <button className="desktop-remote-btn amber" type="submit" disabled={agentEnrollBusy || !agentEnrollToken.trim()}>{agentEnrollBusy ? "提交中" : "注册远程服务"}</button>
            </form>
            {agentEnrollMessage && <p className="desktop-remote-note" role="status">{agentEnrollMessage}</p>}
          </section> : agentStatusState === "loaded" ? <section className="desktop-remote-card" aria-labelledby="desktop-remote-pairing-title">
            <header className="desktop-remote-card-head">
              <h2 id="desktop-remote-pairing-title">用手机扫这个二维码</h2>
              <button className="desktop-remote-btn ghost sm" type="button" onClick={() => void createDesktopPairing()} disabled={busy}>{pairingID ? "重新生成" : "生成二维码"}</button>
            </header>
            <p className="desktop-remote-note">扫码只是"提交申请"；真正的授权是你在<b>这台电脑上</b>点下「确认绑定」。</p>
            {pairingQR ? <div className="desktop-remote-pairing-body">
              <div className="desktop-remote-qr-wrap" data-expired={pairingExpired ? "true" : "false"} data-spent={pairingConfirmed ? "true" : "false"}>
                <img className="desktop-remote-qr" src={pairingQR} alt="Milevia 配对二维码" />
                {/* 死码必须从"还能扫"的状态里摘出来：以前到期只换一句提示文字，那张码还大大方方
                    挂在屏上，用户拿着它反复扫，手机端只会回"配对已失效"。 */}
                {pairingExpired && <div className="desktop-remote-qr-dead" role="status"><strong>二维码已失效</strong><small>超过有效期后它就扫不动了，点「重新生成」再扫。</small></div>}
                {pairingConfirmed && <div className="desktop-remote-qr-dead" role="status"><strong>已使用</strong><small>这台手机已经绑定好了；要换设备请点「重新生成」。</small></div>}
              </div>
              <div className="desktop-remote-pairing-side">
                {/* 三步里的第 2 步是整条流程最容易卡住的地方：手机扫完其实什么都没发生，
                    必须有人在这台电脑上点确认。把它写成有先后关系的清单，用户才不会在手机前等。 */}
                <ol className="desktop-remote-steps">
                  <li data-done={pairingReadyForConfirm || pairingConfirmed ? "true" : "false"}>用手机扫左侧二维码</li>
                  <li data-done={pairingReadyForConfirm || pairingConfirmed ? "true" : "false"}>手机上会显示「已提交」，本页的确认按钮随之亮起</li>
                  <li data-done={pairingConfirmed ? "true" : "false"}>在本机点「确认绑定」，手机端立刻生效</li>
                </ol>
                {pairingCode && <div className="desktop-remote-code"><span className="desktop-remote-code-label">扫码失败时手输这 6 位</span><strong>{pairingCode}</strong><button className="desktop-remote-btn ghost sm" type="button" onClick={() => void copyPairingCode()} disabled={pairingCopied}>{pairingCopied ? "已复制" : "复制"}</button></div>}
                {pairingExpiresAt !== "" && !pairingConfirmed && <p className="desktop-remote-countdown" data-expired={pairingExpired ? "true" : "false"}>{pairingExpired ? "二维码已失效，请重新生成" : `二维码 ${pairingCountdown} 后失效`}</p>}
                {/* 按钮自己说清楚在等什么：禁用态配一句"等待手机扫码…"，比一颗灰着的
                    「确认绑定」更能让人知道下一步该干什么。 */}
                <button className="desktop-remote-btn confirm" data-ready={pairingReadyForConfirm && !pairingExpired ? "true" : "false"} type="button" onClick={() => void confirmDesktopPairing()} disabled={busy || !pairingReadyForConfirm || pairingExpired || pairingConfirmed}>{pairingConfirmed ? "已绑定" : pairingReadyForConfirm ? "确认绑定" : "等待手机扫码…"}</button>
              </div>
            </div> : <p className="desktop-remote-empty">还没有生成二维码。点右上角「生成二维码」开始。</p>}
            {boundPhone && <div className="desktop-remote-warn" role="status">⚠️ 这台电脑同一时间只服务一台手机。确认绑定新的手机后，<b>{boundPhone.deviceName || "当前这台"}</b> 会立刻断开，需要重新扫码才能恢复。</div>}
          </section> : <section className="desktop-remote-card" aria-labelledby="desktop-remote-pending-title">
            {/* 第三种态：**还没读到**（读取中 / 读不到）。这里不许摆注册表单 —— 那不是
                "未注册"，而"未注册"才是唯一需要令牌的场景。读失败时给一颗重试，
                因为这时候唯一有意义的动作是再读一次，而不是去要一个部署令牌。 */}
            <header className="desktop-remote-card-head">
              <h2 id="desktop-remote-pending-title">{agentStatusState === "loading" ? "正在读取服务状态" : "读不到本机服务状态"}</h2>
              {agentStatusState === "failed" && <button className="desktop-remote-btn ghost sm" type="button" onClick={() => void refreshDesktopStatus()} disabled={refreshing}>重试</button>}
            </header>
            <p className="desktop-remote-note">{agentStatusState === "loading" ? "正在确认这台电脑的远程服务是否已就绪；读到之前不判断要不要注册。" : desktopService.hint}</p>
          </section>}
        </div>
        <aside className="desktop-remote-aside">
          <section className="desktop-remote-card soft" aria-labelledby="desktop-remote-service-title">
            <header className="desktop-remote-card-head"><h2 id="desktop-remote-service-title">远程服务</h2><span className="desktop-remote-chip" data-state={desktopService.state}>{desktopService.chip}</span></header>
            <dl className="desktop-remote-kv">
              <dt>云端</dt><dd>{agentStatus?.cloudUrl ? <code>{agentStatus.cloudUrl}</code> : "—"}</dd>
              <dt>实例 ID</dt><dd>{desktopInstanceID ? <code title={desktopInstanceID}>{shortInstanceID(desktopInstanceID)}</code> : "—"}</dd>
              <dt>最近心跳</dt><dd>{agentStatus?.ready ? heartbeatAgoText(heartbeatAgeMs) : "—"}</dd>
            </dl>
            {desktopService.hint && <p className="desktop-remote-hint" data-tone={desktopService.state === "failed" || desktopService.state === "stale" ? "warn" : "plain"}>{desktopService.hint}</p>}
            <div className="desktop-remote-card-actions">
              <button className="desktop-remote-btn ghost sm" type="button" onClick={() => void openAgentDataDirectory()}>打开数据目录</button>
              {agentStatus?.ready && <button className="desktop-remote-btn ghost sm" type="button" onClick={() => setReenrollOpen(true)} disabled={reenrollOpen}>重新注册</button>}
            </div>
            <p className="desktop-remote-hint">排障看数据目录下的 milevia-agent.log。</p>
          </section>
          <section className="desktop-remote-card soft" aria-labelledby="desktop-remote-bound-title">
            <header className="desktop-remote-card-head"><h2 id="desktop-remote-bound-title">已绑定手机</h2>{boundPhone && <button className="desktop-remote-btn warn sm" type="button" onClick={() => void revokeBindings()} disabled={busy}>解除绑定</button>}</header>
            {/* 四种"没有内容"必须分开说：正在读 / 读失败 / 云端还不知道这台电脑 / 真的没有手机。
                旧版把它们压成一个 bindingsReady，失败时整块**什么都不渲染** —— 用户分不清
                "没有手机绑定"和"没读到"。 */}
            {bindingsState === "loading" ? <p className="desktop-remote-hint" role="status">正在读取…</p>
              : bindingsState === "failed" ? <p className="desktop-remote-hint" data-tone="warn" role="alert">读取失败。点右上角「刷新状态」重试。</p>
              : bindingsState === "unregistered" ? <p className="desktop-remote-hint">远程服务未注册，暂时读不到绑定信息。</p>
              : boundPhone ? <>
                {/* 状态灯 + 胶囊两者互补、不是重复：灯给"一眼扫过去"的颜色，胶囊给准确措辞
                    （「在线」/「未同步」/「已绑定手机」三档措辞见 desktop-phone.ts）。
                    灯的档位挂在 `data-state` 上，不靠类名 —— 裸 `online`/`idle` 这类词
                    在这个项目里串过味（见 MEMORY 的类名纪律）。
                    ⚠️ 读数过期（`bindingsStale`）时灯与胶囊也要跟着换：这时握着的是**上一次**
                    读到的状态，继续亮绿灯／写「在线」就是拿旧读数冒充新的 ——
                    mobile-remote.css 里那条"离线时亮绿灯等于骗人"对这里同样成立。
                    但**不许**改写成「未同步」：那是把"我们读不到"说成手机的事实。用「读不到」。 */}
                <div className="desktop-remote-phone">
                  <span className="desktop-remote-phone-mark" data-state={bindingsStale ? "stale" : phoneSync.state} aria-hidden="true"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.9" strokeLinecap="round" strokeLinejoin="round"><rect x="6" y="2.5" width="12" height="19" rx="2.5" /><path d="M10.5 18.5h3" /></svg><i className="desktop-remote-phone-led" /></span>
                  <div className="desktop-remote-phone-who"><strong>{boundPhone.deviceName || "未命名手机"}</strong><small>{[boundPhonePlatform, boundPhone.activatedAt ? `绑定于 ${new Date(boundPhone.activatedAt).toLocaleString()}` : "绑定时间未知"].filter(Boolean).join(" · ")}</small></div>
                  {(bindingsStale || phoneSync.chip) && <span className="desktop-remote-phone-state" data-state={bindingsStale ? "stale" : phoneSync.state}>{bindingsStale ? "读不到" : phoneSync.chip}</span>}
                </div>
                <dl className="desktop-remote-kv">
                  {/* 「最近同步」是这一屏新增的那个读数：只看绑定关系回答不了"手机现在能不能用"。
                      读不到时给「—」而不是编一句"未同步" —— 那一档另有 hint 说清是谁的问题。 */}
                  <dt>最近同步</dt><dd>{phoneSync.agoText || "—"}</dd>
                </dl>
                {/* 轮询重读失败时，**先说我们这一侧读不到**，再说年龄。两句不能同时出现：
                    同一个位置上摆两段解释，用户会以为是两件事。
                    `phoneSync.hint` 讲的是"手机为什么没在同步"（切后台 / 没打开 / 连不上），
                    而我们自己读失败时那个归因是不成立的，所以这一档只留下面这一句。 */}
                {bindingsStale
                  ? <p className="desktop-remote-hint" data-tone="warn" role="status">最近一次重读绑定信息失败，上面显示的是最后一次读到的内容。</p>
                  : phoneSync.hint && <p className="desktop-remote-hint" data-tone={phoneSync.state === "online" || phoneSync.state === "unsupported" ? "plain" : "warn"}>{phoneSync.hint}</p>}
              </>
              : <p className="desktop-remote-hint">还没有手机绑定这台电脑。</p>}
            {bindings.length > 1 && <p className="desktop-remote-hint">另有 {bindings.length - 1} 台旧手机仍持有权限；「解除绑定」会一并断开。</p>}
          </section>
        </aside>
      </div>
    </div>}
    {/* 「当前电脑」卡（2026-09-17 定稿）：[图标块 + 右下角状态灯] [电脑名 +「在线」] [「切换」]。
        名字与状态必须常驻可见 —— 多电脑之后最容易出的事故是"命令发错机器"。
        图标块**复用项目卡那一套**（`.mobile-project-mark`，窄屏会自动降到 40px）；
        状态灯给颜色速读、「在线」胶囊给准确措辞（两者互补，不是重复），胶囊紧跟电脑名后面。
        副标题是"什么时候同步的 + 这台上有几个项目"，不再显示事件序号（那是排障用的内部量）。 */}
    {/* 设备卡：**只要还有令牌就要渲染**，哪怕这次没把实例信息读回来。
        2026-09-20 实测过的缺陷：条件曾经是 `&& instance`，于是"有令牌 + 冷启动时
        `/v1/instances` 失败"（断网、云端故障，或刚 switchDevice/解绑之后那次重拉失败）
        会让这张卡消失 —— 而「切换 / 管理 / 添加电脑或重新配对 / 解绑」**全挂在它上面**
        （`openDevicesSheet` 的唯一调用点就是这里的按钮），配对页又只认"完全没有令牌"。
        结果整页只剩一个「刷新」（探针实测 `clickable: ["刷新"]`）。
        现在 instance 缺失时照常出卡、只是把读数换成一个说得清的"没读回来"，
        入口不再跟着读数一起消失 —— 设备面板本身用的是本地设备表（`readDevices()`），
        不需要云端读数就能用。 */}
    {mobileApp && mobileView === "projects" && (instance || token.trim()) && <section className="mobile-device-card" data-unresolved={instance ? undefined : "true"} aria-label={instance ? "当前电脑" : "电脑信息未读回"}>
      <span className="mobile-project-mark mobile-device-mark" aria-hidden="true">
        <svg viewBox="0 0 24 24"><rect x="3" y="4" width="18" height="12" rx="1.5" /><path d="M8 20h8" /><path d="M12 16v4" /></svg>
        <i className={`mobile-device-led${instance && instance.status === "online" ? "" : " is-away"}`} />
      </span>
      {/* 三种真相分开说（同"空列表有三种真相"那条规则）：真的读到 / 读到但离线（上面的 is-away）
          / 这次根本没读回来（下面这一支）。第二种与第三种绝不能写成同一句话。 */}
      {instance
        ? <span className="mobile-device-who">
          <b>{activeDeviceName}<span className={`mobile-device-pill ${instance.status}`}>{instanceStatusLabel(instance.status)}</span></b>
          <small>{deviceSyncText(instance.lastSeenAt)}</small>
        </span>
        : <span className="mobile-device-who">
          {/* 备注是**纯本地**读数，所以云端这一次没读回来时它照样显示 —— 用户至少还认得出
              这是哪台电脑。但副标题仍然老实说读数没回来：名字有了不等于这台电脑是好的，
              "有名字就说没事"会让人以为可以正常用。（与上面那句"三种真相分开说"同一条。） */}
          <b>{activeDeviceName || "这台电脑的信息没读回来"}<span className="mobile-device-pill offline">未同步</span></b>
          <small>电脑可能离线或网络不通。点右侧可以换一台、重新配对，或先刷新。</small>
        </span>}
      <button className="mobile-device-switch" type="button" ref={deviceSwitchButtonRef} onClick={openDevicesSheet} aria-haspopup="dialog">{instance ? devicePanelLabel : "管理"}<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M6 9l6 6 6-6" /></svg></button>
    </section>}
    {/* `!showPairingStart`：首启（还没有任何电脑）时这一段整块不渲染 —— 那时 projects 必然为空，
        留在屏上就是"「选择项目」+ 一行『暂无项目或电脑尚未同步』"的空标题，而上面那张空态卡
        已经把"接下来该干什么"说清楚了。拖拽排序也要它（没有卡片就没有顺序可排）。 */}
    {mobileApp && mobileView === "projects" && !showPairingStart && <><section className="mobile-project-picker" ref={projectPickerRef} onPointerDown={onProjectPointerDown}><div className="mobile-section-heading"><h2>选择项目</h2><span>{snapshot ? `${projects.length} 个项目` : "加载中"}</span></div>{dragHint && orderedProjects.length > 1 && <p className="mobile-project-drag-hint">按住项目卡片可拖动排序</p>}{projects.length === 0 ? <p className="mobile-empty">暂无项目或电脑尚未同步。</p> : orderedProjects.map((item) => <MobileProjectChoice key={item.id} item={item} busy={busy} isDragSlot={draggingProjectId === item.id} open={openMobileProject} />)}</section>{draggingProject && <div className={`card-drag-ghost${projectLifted ? " is-lifted" : ""}`} ref={projectDragGhostRef} aria-hidden="true"><div className="card-drag-ghost-inner"><MobileProjectChoice item={draggingProject} busy={false} open={openMobileProject} ghost /></div></div>}</>}
    {/* 解绑是不可逆动作（云端吊销这台手机的令牌），复用任务弹层那一套视觉，不另造一套。 */}
    {confirmUnbind && <div className="mobile-task-modal-backdrop" role="dialog" aria-modal="true" aria-labelledby="mobile-unbind-title"><section className="mobile-task-modal mobile-task-delete-modal"><header><h2 id="mobile-unbind-title">解除绑定</h2><button type="button" onClick={() => setConfirmUnbind("")} disabled={busy} aria-label="关闭">×</button></header><p>这台手机将失去对「{deviceDisplayName({ alias: confirmUnbindRecord?.alias || "", name: confirmUnbindRecord?.name || instance?.name || "当前电脑" })}」的访问权限，需要重新扫码配对才能恢复。</p><footer><button type="button" onClick={() => setConfirmUnbind("")} disabled={busy}>取消</button><button type="button" className="mobile-task-delete-confirm" onClick={() => void confirmUnbindDevice()} disabled={busy}>确认解除</button></footer></section></div>}
    {/* 「备注」弹层：给某一台电脑起/改/清一个**只在这台手机上生效**的名字。
        三个设计点：
        ① 它是设备面板**之上**的一层（z-index 30 > 面板的 20），返回键与侧滑返回两处都要收
           （见 backHandlerRef 与 leaveConversationView）—— 漏一处就会出现"面板收了、
           弹层还浮在项目列表上"（与 confirmUnbind 同一条，实测过同一个形状）。
        ② 设备面板那个焦点 effect 的 inert 规则会跳过 `[role="dialog"]`，所以这张弹层
           天生留在可交互集合里，不需要为它再写一套。
        ③ 提示语必须说清"只在这台手机上生效"：用户很容易以为改的是电脑本身的名字
           （那是 `/v1/instances` 报的 `deviceName`，手机改不了它）。
        ④ 单字段 + 留空即恢复：没有"保存空名字"这种状态。
        ⑤ 渲染条件带上 `devicesOpen`：它是面板的**子层**，不该比面板活得久。
           今天没有可达路径能让面板先关掉（弹层背板盖住整屏，面板上的按钮点不到；
           `leaveConversationView` 与返回键链都各自收过它），但"靠今天恰好没有那条路"
           不值得赌 —— 少一个条件，将来任何一处新增的 `setDevicesOpen(false)` 都会让
           这层浮在项目列表上。写在条件里，就不需要每个关面板的地方都记得它。 */}
    {devicesOpen && aliasTarget && <div className="mobile-task-modal-backdrop" role="dialog" aria-modal="true" aria-labelledby="mobile-device-alias-title"><form className="mobile-task-modal" onSubmit={saveAlias}><header><h2 id="mobile-device-alias-title">给这台电脑起个名字</h2><button type="button" onClick={closeAliasEditor} aria-label="关闭">×</button></header><label>备注名<input value={aliasDraft} maxLength={DEVICE_ALIAS_MAX_LENGTH} autoFocus enterKeyHint="done" autoComplete="off" placeholder={aliasTargetRecord?.name || "例如：公司那台"} onChange={(event) => setAliasDraft(event.target.value)} /></label><p className="mobile-device-alias-hint">只在这台手机上生效，电脑本身的名字不会变。留空即恢复原名{aliasTargetRecord?.name ? `「${aliasTargetRecord.name}」` : ""}。</p><footer>{aliasTargetRecord?.alias ? <button type="button" className="mobile-device-alias-clear" onClick={() => { setDeviceAlias(aliasTarget, ""); setDevices(readDevices()); closeAliasEditor(); }}>清除备注</button> : null}<button type="button" onClick={closeAliasEditor}>取消</button><button type="submit">保存</button></footer></form></div>}
    {/* 「我的电脑」面板（底部弹层）：切换电脑、添加/重新配对、逐台解绑**都在这一层**。
        以前这些散在三处（页面底部的两个游离按钮 + 配对面板里的"我的电脑"总账 + 这个弹层），
        现在合成一处 —— 点设备卡片上的「切换 / 管理」就进来。
        点背板或按返回键都关。 */}
    {devicesOpen && <div className="mobile-device-sheet-backdrop" role="dialog" aria-modal="true" aria-labelledby="mobile-device-sheet-title" onClick={() => setDevicesOpen(false)}>
      <section className="mobile-device-sheet" ref={devicesSheetRef} tabIndex={-1} onClick={(event) => event.stopPropagation()}>
        <header><h2 id="mobile-device-sheet-title">我的电脑</h2><button type="button" onClick={() => setDevicesOpen(false)} aria-label="关闭">×</button></header>
        <p className="mobile-device-sheet-note">一台手机可以连多台电脑；每台电脑同一时间只服务一台手机。换电脑、改名字或重新配对都从这里开始。</p>
        {devices.map((item) => <div className="mobile-device-row" key={item.token} data-current={item.token === token ? "true" : "false"} data-revoked={item.revoked ? "true" : "false"}>
          {/* 行主体是"切到这台"，右侧「备注 / 解绑」是另外两颗按钮 —— 所以行**不能**再是 <button>：
              里面嵌不下更多按钮（嵌套按钮非法）。 */}
          <button className="mobile-device-row-main" type="button" onClick={() => switchDevice(item.token)} disabled={item.revoked}>
            <span className="mobile-device-row-who"><b>{deviceDisplayName(item)}</b>
              {/* 起了备注之后，"这台到底是哪台"就只能靠真名回答 —— 所以备注生效时真名要留在
                  **同一行**里（比"再点一次备注才知道原名"省一层），但排在状态行之上、用更淡的一档，
                  不跟在线状态抢注意力。真名还没从云端回填时（空串）这一行不渲染，不留一条空行。 */}
              {item.alias !== "" && item.name !== "" && <small className="mobile-device-row-origin">原名 {item.name}</small>}
              <small>{deviceStatusText(item)}</small></span>
            {/* 失效的那台不给"在线/离线"标签：它的状态是"要重新配对"，两者混淆会让人以为点一下就能连上。 */}
            <span className={`mobile-device-tag${!item.revoked && item.status === "online" ? " is-online" : ""}`}>{item.revoked ? "需重新配对" : item.status === "online" ? "在线" : "离线"}</span>
            {/* 当前那台给 ✓；其余**未失效**的给一个 ›（和项目卡同一套"可进入"语言）——
                否则除了当前那行，用户看不出别的行能点。失效那行不给，它点不动。 */}
            {item.token === token
              ? <span className="mobile-device-tick" aria-hidden="true">✓</span>
              : !item.revoked && <span className="mobile-device-chevron" aria-hidden="true">›</span>}
          </button>
          {/* 动作列**竖排**（不是并排）：320px 屏上面板内容宽只有 292px，并排时
              "名字 + 状态胶囊 + ✓ + 备注 + 解绑"会把名字挤到不足 80px（约 5 个字一行），
              而名字正是这一行存在的理由。竖排只占一颗按钮的宽度，名字拿回约 50px。
              顺序：备注在上、解绑在下 —— 危险动作留在最右下角。 */}
          <span className="mobile-device-row-actions">
            <button type="button" className="mobile-device-row-alias" onClick={() => openAliasEditor(item.token)} disabled={busy} aria-label={`给「${deviceDisplayName(item)}」改备注`}>{item.alias ? "改备注" : "备注"}</button>
            <button type="button" className="mobile-device-row-unbind" onClick={() => requestUnbind(item.token)} disabled={busy} aria-label={`解绑「${deviceDisplayName(item)}」`}>解绑</button>
          </span>
        </div>)}
        {/* 「添加电脑」与"重新配对此设备"本来就是同一个动作（都走扫码配对），合并成一颗；
            面板副标题把那层含义说清楚。 */}
        <button className="mobile-device-add" type="button" onClick={() => { setDevicesOpen(false); openPairingPanel(); }}>＋ 添加电脑或重新配对</button>
      </section>
    </div>}
    {mobileApp && mobileView === "conversation" && project && gitOpen && <section className="mobile-git-view" aria-label="Git 工作台"><MobileGitPanel
      ref={gitPanelRef}
      projectId={project.id}
      conversationId={conversation && !conversationPending ? conversation.id : undefined}
      transport={rpcTransport}
    /></section>}
    {mobileApp && mobileView === "conversation" && project && filesOpen && <section className="mobile-files" aria-label="项目文件"><FilesPanel
      key={filesEpoch}
      ref={filesPanelRef}
      projectId={project.id}
      conversationId={conversation && !conversationPending ? conversation.id : undefined}
      runner={project.runner}
      request={filesAdapter.request}
      media={filesMedia}
      mobile
      disableDownload
      // 手机端没有"工作区此刻被谁占着"的实时读数（租约只在写入那一刻由服务端判定），
      // 所以**不预先把面板锁成只读**：锁早了会让用户在 AI 其实没跑的时候也改不了文件。
      // 真的被占着时服务端会给一句明确的 409，面板会把它显示出来。
      isWorkspaceOccupied={false}
      registerNavigationGuard={registerFilesGuard}
      initialPath={filesInitialPath}
      onInitialPathConsumed={() => setFilesInitialPath(null)}
    /></section>}
    {mobileApp && mobileView === "conversation" && project && !subHeaderActive && <section className="mobile-conversation"><div className="mobile-message-list" data-empty={showConversationEmpty ? "true" : undefined}>{conversationTimeline.length ? conversationTimeline.map((entry, index) => entry.kind === "message" ? <article className={`mobile-message ${entry.message.role}`} key={entry.key} data-transient={isTransientMessage(entry.message) ? "true" : undefined} data-arrive={arrivingKeys.includes(entry.key) ? "true" : undefined} data-actions={messageActionId === entry.message.id ? "open" : undefined} onAnimationEnd={(event) => { if (event.target === event.currentTarget) clearArriving(entry.key); }}><div className="mobile-message-head"><small>{entry.message.role === "user" ? "我" : "Agent"} · {messageTime(entry.message.createdAt)}{isStreamingMessage(entry.message) && <span className="mobile-message-writing" role="img" aria-label="正在写入"><i aria-hidden="true" /><i aria-hidden="true" /><i aria-hidden="true" /></span>}</small><div className="mobile-message-head-actions">{messageActionId === entry.message.id && <div className="mobile-message-actions" id={`mobile-message-actions-${index}`} ref={messageActionsRef} role="group" aria-label={`${entry.message.role === "user" ? "我" : "Agent"}这条消息的操作${isTransientMessage(entry.message) ? `（${transientReason(entry.message, "row")}）` : ""}`}><button type="button" className="mobile-message-action-icon" data-copy-state={messageCopyState} aria-label={messageCopyState === "copied" ? "已复制到剪贴板" : messageCopyState === "failed" ? "复制失败，请重试" : "复制这条消息"} title="复制 Markdown 原文" onClick={() => void copyMessageBody(entry.message.content)}><MessageActionIcon kind={messageCopyState === "idle" ? "copy" : messageCopyState} /></button><span className="mobile-message-action-status" role="status" aria-live="polite">{messageCopyState === "copied" ? "已复制到剪贴板" : messageCopyState === "failed" ? "复制失败，请检查剪贴板权限" : ""}</span><button type="button" className="mobile-message-action-icon" disabled={isTransientMessage(entry.message)} aria-label={`引用${entry.message.role === "user" ? "我" : "Agent"}这条消息到输入框`} title={isTransientMessage(entry.message) ? transientReason(entry.message, "quote") : "引用到输入框"} onClick={() => quoteMessage({ id: entry.message.id, role: entry.message.role, content: entry.message.content })}><MessageActionIcon kind="quote" /></button>{entry.message.role === "user" && <button type="button" className="mobile-message-action-icon" disabled={isTransientMessage(entry.message)} aria-label="把这条消息的原文填回输入框" title={isTransientMessage(entry.message) ? transientReason(entry.message, "resend") : "填回输入框，改完再发"} onClick={() => resendMessage(entry.message.content)}><MessageActionIcon kind="resend" /></button>}</div>}<button type="button" className="mobile-message-more" aria-expanded={messageActionId === entry.message.id} aria-controls={messageActionId === entry.message.id ? `mobile-message-actions-${index}` : undefined} aria-label={`${entry.message.role === "user" ? "我" : "Agent"}这条消息的操作`} title="更多操作" onClick={(event) => { if (messageActionId === entry.message.id) { closeMessageActions(); return; } openMessageActions(event.currentTarget, entry.message.id); }}>⋯</button></div></div><div className="mobile-message-markdown markdown"><ReactMarkdown remarkPlugins={[remarkGfm]} skipHtml components={{ ...markdownCodeComponents, a: ({ href, children }) => <a href={href} target="_blank" rel="noreferrer">{children}</a>, code: ({ className, children }) => <MobileInlineCode className={className} onOpenFile={openFiles}>{children}</MobileInlineCode> }}>{entry.message.content}</ReactMarkdown></div></article> : <article className={`mobile-notice mobile-notice-${entry.notice.variant}${entry.notice.state ? ` mobile-notice-${entry.notice.state}` : ""}`} key={entry.key} data-arrive={arrivingKeys.includes(entry.key) ? "true" : undefined} onAnimationEnd={(event) => { if (event.target === event.currentTarget) clearArriving(entry.key); }}><span className="mobile-notice-icon" aria-hidden="true">{noticeIcons[entry.notice.variant]}</span><div className="mobile-notice-text"><strong>{entry.notice.title}</strong>{entry.notice.detail && <small>{entry.notice.detail}</small>}</div>{noticeTime(entry.notice.createdAt) && <time className="mobile-notice-time">{noticeTime(entry.notice.createdAt)}</time>}</article>) : conversationEmpty}{conversationProcessing && <div className="mobile-agent-processing" data-agent={processingAgent.tone} role="status" aria-live="polite">{/* 状态条的三格读音各有来源，判据全在 lib/processing-indicator.ts ——
      这一格里**不许再出现任何条件判断**，否则就是"服务端算了一套、界面又另判一套"。
      ① 品牌徽标：把 Agent 名从"和正文同字号同色的一句话"里拆出来，变成一眼可辨的标签
         （旧版里「Claude Code」与「正在处理...」长得一模一样，扫过去分不清身份和状态）。
         品牌色只上在这一格（橙 / 墨），状态条本体的绿底绿字一个像素都没动。 */}
    <span className="mobile-agent-processing-badge" aria-hidden="true">{processingAgent.label}</span>{/* ② 阶段词：这一刻到底在干什么（压缩上下文 / 网络重试 / 执行后台任务…），
         而不是永远一句"正在处理"。 */}
    <span className="mobile-agent-processing-stage">{processingStage}</span>{/* ③ 已耗时：**读数**，不是装饰。10 秒以下为空串时不渲染 ——
         秒级任务上闪一个「3 秒」只是噪音，还会让人以为它比实际慢。 */}
    {processingElapsed && <span className="mobile-agent-processing-elapsed" aria-hidden="true">{processingElapsed}</span>}</div>}</div>{tasksOpen && <section className="mobile-task-panel" ref={taskPanelRef} tabIndex={-1} role="dialog" aria-modal="true" aria-labelledby="mobile-task-panel-title">
      <header className="mobile-task-panel-header">
        <h3 id="mobile-task-panel-title">任务队列</h3>
        <button type="button" className="mobile-task-panel-close" onClick={() => setTasksOpen(false)} aria-label="关闭任务队列" title="关闭">×</button>
      </header>
      {/* 搜索框在分类栏**上面**（2026-09-18 按用户要求）：先选分类、再在这一档里搜。
          两个装饰（放大镜、清除键）都自己画 —— 原生 `type="search"` 的这两颗由系统绘制、
          本页 CSS 一行都管不到，与当初那颗原生 select 是同一类问题（所以 `::-webkit-search-*`
          两条在样式里被显式关掉）。清除键只在有词时渲染，省掉一条 `[hidden]` 与 `display` 的纠缠。 */}
      <div className="mobile-task-search">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="M11 4a7 7 0 1 0 0 14 7 7 0 0 0 0-14Z" /><path d="m16 16 4 4" /></svg>
        <input type="search" value={taskQuery} autoComplete="off" enterKeyHint="search" aria-label="在任务队列中搜索" placeholder="搜索任务…" onChange={(event) => setTaskQuery(event.target.value)} />
        {/* 清空键：`onMouseDown` 吞掉默认行为，焦点才会留在输入框里 —— 否则点一下就会失去焦点、
            软键盘收起、整屏跳一下，用户还得再点回输入框才能接着打字。 */}
        {taskQuery !== "" && <button type="button" className="mobile-task-search-clear" title="清空搜索" aria-label="清空搜索" onMouseDown={(event) => event.preventDefault()} onClick={() => setTaskQuery("")}>×</button>}
      </div>
      <nav className="mobile-task-filters" aria-label="任务状态分类" role="tablist">
        {taskFilters.map((filter) => {
          // 计数取自"搜索命中之后"的那批（`taskCounts`）：有搜索词时它才与下一行的列表严格相等。
          // 没有搜索词时它就是各分类的全量，与加搜索之前完全一致。
          const count = taskCounts[filter.id];
          // data-count 只给样式用：计数为 0 的那一档要退到背景里（一排五颗都在喊的时候没人知道该看哪）。
          // 变体一律走 data-* 属性选择器，不另造类名（MOBILE-UI「别用裸词做类名」）。
          return <button type="button" role="tab" aria-selected={taskFilter === filter.id} className={taskFilter === filter.id ? "active" : ""} data-count={count === 0 ? "0" : "n"} key={filter.id} onClick={() => setTaskFilter(filter.id)}>{filter.label}<span>{count}</span></button>;
        })}
      </nav>
      {hiddenTaskNote && <p className="mobile-task-hidden">{hiddenTaskNote}</p>}
      <div className="mobile-task-panel-body">
        <div className="mobile-task-list">
          {taskPanelEmpty ? <div className="mobile-task-empty" data-state={taskPanelEmpty.state} role="status">
            <span className="mobile-task-empty-mark" aria-hidden="true">{taskPanelEmpty.state === "nomatch" ? <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round"><path d="M11 4a7 7 0 1 0 0 14 7 7 0 0 0 0-14Z" /><path d="m16 16 4 4" /></svg> : taskPanelEmpty.state === "filtered" ? <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round"><path d="M3 5h18l-7 8v6l-4-2v-4Z" /></svg> : <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round"><path d="M4 7h16" /><path d="M4 12h10" /><path d="M4 17h7" /></svg>}</span>
            <strong>{taskPanelEmpty.title}</strong>
          </div> : visibleTasks.map((task) => { const canEditTask = task.status === "todo" || task.status === "action_required"; const taskDescription = task.description?.trim() || ""; const taskSyncing = pendingTaskBusy.has(task.id); return <article className="mobile-task" key={task.id} data-status={task.status} data-syncing={taskSyncing ? "true" : undefined}>
            <header className="mobile-task-head">
              <span className={`mobile-task-status ${taskStatusClass(task.status)}`}>{taskStatusLabel(task.status)}</span>
              {/* 「同步中」是乐观更新的另一半：本地已经生效，但电脑端还没确认。
                  没有它，用户没法区分"已经同步好了"和"还在路上"；有了它，即使云端慢
                  或者干脆离线，这张卡片也如实说明现在的状态，而不是装作已经成功。 */}
              {taskSyncing && <span className="mobile-task-syncing" role="status">同步中</span>}
              <span className="mobile-task-priority" data-priority={task.priority}>{taskPriorityLabel(task.priority)}</span>
              <time className="mobile-task-time" dateTime={task.updatedAt}>{messageTime(task.updatedAt)}</time>
            </header>
            <strong className="mobile-task-title">{taskSummary(task)}</strong>
            {taskDescription && <p className="mobile-task-desc">{taskDescription}</p>}
            <footer className="mobile-task-foot">
              {task.status === "todo" || task.status === "action_required" ? <button type="button" className="mobile-task-primary" disabled={taskSyncing} onClick={() => void sendTaskCommand(task.id, "task.dispatch")}>下发</button> : null}
              {task.status === "awaiting_review" ? <button type="button" className="mobile-task-primary" disabled={taskSyncing} onClick={() => void sendTaskCommand(task.id, "task.review")}>验收</button> : null}
              {task.status === "running" ? <button type="button" className="mobile-task-primary" disabled={taskSyncing} onClick={() => void sendTaskCommand(task.id, "task.stop")}>停止</button> : null}
              <span className="mobile-task-foot-gap" />
              <button type="button" className="mobile-task-detail" aria-expanded={expandedTask === task.id} aria-controls={expandedTask === task.id ? `mobile-task-detail-${task.id}` : undefined} onClick={() => setExpandedTask((current) => current === task.id ? "" : task.id)}>详情</button>
              <button type="button" className="mobile-task-edit" data-mobile-task-edit="true" hidden={!canEditTask} disabled={taskSyncing || !canEditTask} onClick={() => openTaskEditor(task)}>编辑</button>
              <button type="button" className="mobile-task-delete" data-mobile-task-delete="true" disabled={taskSyncing || task.status === "running"} onClick={() => setDeletingTask(task)}>删除</button>
            </footer>
            {expandedTask === task.id && <div className="mobile-task-expanded" id={`mobile-task-detail-${task.id}`}>
              <p>{taskDescription || "暂无任务描述"}</p>
              <time dateTime={task.updatedAt}>更新于 {new Date(task.updatedAt).toLocaleString()}</time>
            </div>}
          </article>; })}
        </div>
      </div>
      {/* 创建任务的唯一入口。面板是 fixed + inset:0 的整页层，所以这颗用 absolute 挂在面板右下角：
          滚动的是 panel-body，浮钮不该跟着列表滚走。右下角也避开输入条（输入条在会话层，被面板盖住）。 */}
      <button type="button" className="mobile-task-fab" onClick={() => { setCreateError(""); setCreatingTask(true); }} disabled={busy} aria-label="新建任务" title="新建任务">
        <svg viewBox="0 0 24 24" width="22" height="22" aria-hidden="true" focusable="false" fill="none" stroke="currentColor" strokeWidth="2.6" strokeLinecap="round"><path d="M12 5v14" /><path d="M5 12h14" /></svg>
      </button>
    </section>}<div className="mobile-composer-shell" ref={composerShellRef}>{composerToolsOpen && <section className="mobile-composer-tools" id="mobile-composer-tools" aria-label="工具">
<div className="mobile-tool-group"><h3>常用提示词<span>{promptShortcuts.length}</span></h3>{promptShortcuts.length === 0 ? <p className="mobile-tool-empty">电脑端还没有添加常用提示词。</p> : <div className="mobile-tool-chips">{promptShortcuts.map((item) => <button type="button" key={item.id} disabled={busy || !conversation || Boolean(shortcutBusy)} onClick={() => void applyShortcut(item)} title={item.template}>{shortcutBusy === item.id ? "处理中" : item.name}</button>)}</div>}</div>
<div className="mobile-tool-group"><h3>常用命令<span>{commandShortcuts.length}</span></h3>{commandShortcuts.length === 0 ? <p className="mobile-tool-empty">电脑端还没有添加常用命令。</p> : <div className="mobile-tool-chips">{commandShortcuts.map((item) => <button type="button" key={item.id} disabled={busy || !conversation || Boolean(shortcutBusy)} onClick={() => void applyShortcut(item)} title={item.template}>{shortcutBusy === item.id ? "处理中" : item.name}</button>)}</div>}</div>
<div className="mobile-tool-group"><h3>技能<span>{projectSkills.length}</span></h3>{projectSkills.length === 0 ? <p className="mobile-tool-empty">电脑端未发现可用技能。</p> : <div className="mobile-tool-subgroups">{skillGroups.map((group) => <div className="mobile-tool-subgroup" key={group.source}><h4>{group.label}<span>{group.items.length}</span></h4><div className="mobile-tool-chips">{group.items.map((skill) => <button type="button" key={`${group.source}-${skill.name}`} disabled={busy || !conversation} onClick={() => fillSkill(skill)} title={skill.description}>{skill.name}</button>)}</div></div>)}</div>}</div>
<div className="mobile-tool-group"><h3>输入</h3><div className="mobile-tool-chips"><button type="button" disabled={busy || !conversation} onClick={insertComposerLineBreak}>换行</button><button type="button" disabled={!messageDraft && skillRefs.length === 0 && !quoteRef} onClick={() => { setMessageDraft(""); setSkillRefs([]); setQuoteRef(null); setComposerToolsOpen(false); }}>清空草稿</button></div></div>
</section>}<form className="mobile-composer" onSubmit={sendConversationMessage}>{skillRefs.length > 0 && <div className="mobile-skill-refs" role="group" aria-label="已引用的技能">{skillRefs.map((skill) => <span className="mobile-skill-ref" key={`${skill.source}-${skill.name}`}><span className="mobile-skill-ref-name" title={skill.description || skill.name}>{skill.name}</span><button type="button" className="mobile-skill-ref-remove" title={`移除技能 ${skill.name}`} aria-label={`移除技能 ${skill.name}`} disabled={busy} onClick={() => removeSkillRef(skill)}>×</button></span>)}<span className="mobile-skill-ref-hint">发送时展开为完整引用</span></div>}
{/* 被引用的消息（消息操作面板里的「引用到输入框」）：与技能引用同一形态的一行胶囊 ——
    正文留给用户自己写、发送那一刻才展开成引用块。**只挂一条**（见 MessageQuote）。
    正文必须夹列宽 + 单行省略：这里回显的是别人写的内容，长度不可控
    （本文件记过四次的「回显用户输入」坑，第四例就是它）。 */}
{quoteRef && <div className="mobile-quote-refs" role="group" aria-label="已引用的消息"><span className="mobile-quote-ref"><MessageActionIcon kind="quote" /><span className="mobile-quote-ref-text" title={quoteRef.content}>{quoteRef.content}</span><button type="button" className="mobile-quote-ref-remove" title="取消引用" aria-label={`取消引用${quoteRef.role === "user" ? "我" : "Agent"}的这条消息`} disabled={busy} onClick={() => setQuoteRef(null)}>×</button></span><span className="mobile-quote-ref-hint">发送时展开为引用块</span></div>}
<div className="mobile-composer-box"><button type="button" className="mobile-composer-tool" aria-label="工具" title="工具" aria-expanded={composerToolsOpen} aria-controls="mobile-composer-tools" onClick={() => setComposerToolsOpen((open) => !open)} disabled={busy}><svg viewBox="0 0 24 24" width="18" height="18" aria-hidden="true" focusable="false" fill="none" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round"><path d="M12 5v14" /><path d="M5 12h14" /></svg></button><textarea ref={messageInputRef} value={messageDraft} onChange={(event) => setMessageDraft(event.target.value)} onKeyDown={(event) => { if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing) { event.preventDefault(); event.currentTarget.form?.requestSubmit(); } }} placeholder={conversation ? "输入消息..." : "请先新建会话"} aria-label="输入消息" rows={1} disabled={busy || !conversation} /><button type="submit" className="mobile-composer-send" disabled={busy || !conversation || (!messageDraft.trim() && skillRefs.length === 0 && !quoteRef)} aria-label="发送消息" title="发送消息"><svg viewBox="0 0 24 24" width="18" height="18" aria-hidden="true" focusable="false" fill="none" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round" strokeLinejoin="round"><path d="M12 19V5" /><path d="M5 12l7-7 7 7" /></svg></button></div></form></div></section>}
    {newConversationProject && <div className="mobile-new-conversation-backdrop" role="dialog" aria-modal="true" aria-labelledby="mobile-new-conversation-title"><section className="mobile-new-conversation-dialog"><header><div><h2 id="mobile-new-conversation-title">新会话</h2><p>选择执行 Agent</p></div><button type="button" onClick={cancelNewConversation} disabled={busy} aria-label="关闭">×</button></header><div className="mobile-agent-options" role="radiogroup" aria-label="选择执行 Agent"><button type="button" role="radio" aria-checked={newConversationAgent === "claude-code"} className={newConversationAgent === "claude-code" ? "active" : ""} onClick={() => setNewConversationAgent("claude-code")}><strong>Claude Code</strong><small>使用 Claude Code 执行</small></button><button type="button" role="radio" aria-checked={newConversationAgent === "codex"} className={newConversationAgent === "codex" ? "active" : ""} onClick={() => setNewConversationAgent("codex")}><strong>Codex</strong><small>使用 Codex 执行</small></button></div><footer><button type="button" className="mobile-new-conversation-cancel" onClick={cancelNewConversation} disabled={busy}>取消</button><button type="button" className="mobile-new-conversation-confirm" onClick={() => void createConversationForProject(newConversationProject, newConversationAgent)} disabled={busy}>创建会话</button></footer></section></div>}
    {conversationHistoryOpen && <div className="mobile-conversation-history-backdrop" role="dialog" aria-modal="true" aria-labelledby="mobile-conversation-history-title" onClick={(event) => { if (event.target === event.currentTarget) setConversationHistoryOpen(false); }}><section className="mobile-conversation-history-dialog"><header><div><h2 id="mobile-conversation-history-title">历史会话</h2><small>{conversations.length} 个会话</small></div><button type="button" onClick={() => setConversationHistoryOpen(false)} aria-label="关闭历史会话">×</button></header>{conversations.length === 0 ? <p className="mobile-empty">该项目还没有会话。</p> : <ul className="mobile-conversation-history-list">{conversations.map((item) => <li key={item.id}><button type="button" className={`mobile-conversation-history-item${item.id === conversation?.id ? " active" : ""}`} aria-current={item.id === conversation?.id ? "true" : undefined} onClick={() => { setSelectedConversation(item.id); setConversationHistoryOpen(false); }}><strong>{item.title || "未命名会话"}</strong><small>{conversationStatusLabel(item.status)}{item.lastActivityAt ? ` · ${new Date(item.lastActivityAt).toLocaleString()}` : ""}</small></button></li>)}</ul>}</section></div>}
  </main>;
}
