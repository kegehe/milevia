import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const stylesheet = await readFile(new URL("./conversation.css", import.meta.url), "utf8");
// 代码已重构，相关代码现在分布在这些文件中：
const types = await readFile(new URL("./lib/types.ts", import.meta.url), "utf8");
const timelineLib = await readFile(new URL("./lib/timeline.ts", import.meta.url), "utf8");
const utilsLib = await readFile(new URL("./lib/utils.ts", import.meta.url), "utf8");
const projectLayout = await readFile(new URL("./components/ProjectLayout.tsx", import.meta.url), "utf8");
const conversationPage = await readFile(new URL("./pages/ConversationPage.tsx", import.meta.url), "utf8");
const taskQueue = await readFile(new URL("./features/tasks/TaskQueue.tsx", import.meta.url), "utf8");
const gitWorkbench = await readFile(new URL("./features/git/GitWorkbench.tsx", import.meta.url), "utf8");
const runPanel = await readFile(new URL("./features/run/ProjectRunPanel.tsx", import.meta.url), "utf8");
const runStyles = await readFile(new URL("./run.css", import.meta.url), "utf8");

const workspaceStyles = stylesheet.slice(stylesheet.indexOf("/* Desktop conversation workspace:"));

test("message bubbles use their content width without exceeding the reading measure", () => {
  assert.match(stylesheet, /\.message\s*\{[^}]*display:\s*flex;[^}]*min-width:\s*0;[^}]*width:\s*min\(760px,\s*100%\);[^}]*flex-direction:\s*column;[^}]*align-items:\s*flex-start;/s);
  assert.match(stylesheet, /\.message\.user\s*\{\s*align-items:\s*flex-end;\s*\}/);
  assert.match(stylesheet, /\.message\s*>\s*\.markdown\s*\{[^}]*box-sizing:\s*border-box;[^}]*width:\s*fit-content;[^}]*max-width:\s*100%;[^}]*min-width:\s*0;/s);
});

test("desktop canvas uses two working columns without a page inset", () => {
  assert.match(workspaceStyles, /\.conversation-canvas\s*\{[^}]*width:\s*100%;[^}]*grid-template-columns:\s*minmax\(280px,\s*320px\)\s+minmax\(0,\s*1fr\);[^}]*margin:\s*0;/s);
  assert.doesNotMatch(workspaceStyles, /chat-right/);
});

test("head actions portal into the project header so the canvas grid stays clean", () => {
  // 操作栏(权限/历史/新会话)通过 createPortal 渲染到 .project-head 的
  // .head-actions-slot，不参与 .conversation-canvas 网格布局。
  // display: contents 会让 .head-actions 漏到网格挤占左列首行，必须避免。
  assert.doesNotMatch(stylesheet, /\.head-actions-menu\s*\{[^}]*display:\s*contents/s);
  assert.match(conversationPage, /createPortal[\s\S]*head-actions-menu[\s\S]*head-actions-slot/);
  assert.match(projectLayout, /head-actions-slot/);
  assert.match(stylesheet, /\.head-actions-slot\s*\{/);
});

test("desktop conversation is a full-height two-column workspace", () => {
  assert.match(workspaceStyles, /\.chat\s*\{[^}]*height:\s*100dvh;[^}]*min-height:\s*0;[^}]*overflow:\s*hidden;/s);
  assert.match(workspaceStyles, /\.conversation-canvas\s*\{[^}]*width:\s*100%;[^}]*min-height:\s*0;[^}]*grid-template-columns:\s*minmax\(280px,\s*320px\)\s+minmax\(0,\s*1fr\);/s);
  assert.match(workspaceStyles, /\.quick-tag-rail\s*\{[^}]*align-self:\s*stretch;[^}]*min-height:\s*0;[^}]*overflow-y:\s*auto;/s);
  assert.match(workspaceStyles, /\.chat-center\s*\{[^}]*display:\s*grid;[^}]*min-height:\s*0;[^}]*grid-template-rows:\s*minmax\(0,\s*1fr\)\s+auto;/s);
  assert.match(workspaceStyles, /\.chat-center > \.timeline\s*\{[^}]*min-height:\s*0;[^}]*overflow-y:\s*auto;/s);
  assert.match(workspaceStyles, /\.chat-center > \.composer\s*\{[^}]*position:\s*static;/s);
  assert.doesNotMatch(conversationPage, /chat-right/);
});

test("conversation content reserves space for its controls without legacy footer padding", () => {
  assert.match(workspaceStyles, /\.chat-center > \.timeline\s*\{[^}]*padding:\s*32px\s+16px\s+20px;/s);
  assert.match(workspaceStyles, /@media \(max-width: 820px\)[\s\S]*?\.chat-center > \.timeline\s*\{[^}]*padding:\s*20px\s+56px\s+max\(32px, var\(--composer-height, 0px\)\)\s+16px;/);
  assert.doesNotMatch(workspaceStyles, /padding-bottom:\s*(?:130|160)px;/);
  assert.match(conversationPage, /const updateBottomSafeArea = \(\) => \{\s*const followBottom = userNearBottom\.current;\s*timelineElement\.style\.setProperty\("--composer-height", `\$\{Math\.ceil\(composer\.getBoundingClientRect\(\)\.height\)\}px`\);\s*if \(!followBottom\) return;/s);
  assert.match(conversationPage, /const observer = new ResizeObserver\(updateBottomSafeArea\);/);
  assert.match(conversationPage, /if \(userNearBottom\.current\) scrollToBottom\("auto"\);/);
});

test("approval and task-board views keep working inside the workspace", () => {
  assert.match(workspaceStyles, /\.chat-center > \.composer\.has-approval\s*\{[^}]*padding-top:\s*0;/s);
  assert.match(workspaceStyles, /\.chat-center \.approval-banner-body\s*\{[^}]*width:\s*100%;[^}]*min-width:\s*0;[^}]*flex-wrap:\s*wrap;[^}]*padding:\s*12px\s+0;/s);
  assert.match(workspaceStyles, /\.workspace-content > \.workspace-tab-panel\s*\{[^}]*min-height:\s*0;[^}]*flex:\s*1;[^}]*overflow:\s*auto;/);
});

test("Git workbench and project runner are first-class workspace tabs", () => {
  // Types 在 lib/types.ts
  assert.match(types, /type WorkspaceTab = "conversation" \| "tasks" \| "files" \| "git" \| "run";/);
  // ProjectLayout 管理 workspace tabs
  assert.match(projectLayout, /const \[workspaceTab, setWorkspaceTab\] = useState<WorkspaceTab>/);
  assert.match(projectLayout, /<nav className="workspace-tabs" role="tablist" aria-label="项目工作区" onKeyDown=\{navigateWorkspaceTabs\}>/);
  assert.match(projectLayout, /role="tab" aria-controls="workspace-panel-git"/);
  assert.match(projectLayout, /const navigateWorkspaceTabs = \(event: React\.KeyboardEvent<HTMLElement>\) => \{/);
  assert.match(projectLayout, /\["ArrowLeft", "ArrowRight", "Home", "End"\]/);
  assert.doesNotMatch(projectLayout, /const \[showGit, setShowGit\]/);
  assert.doesNotMatch(projectLayout, /const \[showRun, setShowRun\]/);
  assert.match(workspaceStyles, /\.workspace-tabs\s*\{[^}]*display:\s*flex;[^}]*overflow-x:\s*auto;/s);
  assert.match(workspaceStyles, /\.workspace-content\s*\{[^}]*display:\s*flex;[^}]*min-height:\s*0;[^}]*flex:\s*1;[^}]*overflow:\s*hidden;/s);
  assert.match(gitWorkbench, /useEffect\(\(\) => \{ if \(active\) void reload\(\)\.catch\(\(\) => undefined\); \}, \[active, reload\]\);/);
  assert.match(runPanel, /if \(!active\) return;/);
  // 左右分栏：.run-body 水平排列，左侧日志 flex:1，右侧侧栏固定宽度
  assert.match(runStyles, /\.run-body\s*\{[^}]*display:\s*flex;[^}]*min-height:\s*0;[^}]*overflow:\s*hidden;/s);
  assert.match(runStyles, /\.run-sidebar\s*\{[^}]*width:\s*340px;[^}]*overflow-y:\s*auto;/s);
  // 日志终端使用深色高对比度底色，并保留 ANSI 语义颜色。
  assert.match(runStyles, /\.run-log-terminal\s*\{[^}]*background:\s*#172b27;[^}]*color:\s*#d9e8df;/s);
  assert.match(runPanel, /toAnsiSegments\(entry\.text\)/);
  assert.match(runStyles, /\.ansi-green, \.ansi-bright-green\s*\{\s*color:\s*#a9e57e;/);
  // 移动端回退为上下布局
  assert.match(runStyles, /@media \(max-width: 820px\)\s*\{[\s\S]*?\.run-body\s*\{[^}]*flex-direction:\s*column;[^}]*overflow-y:\s*auto;/s);
});

test("switching projects does not render an outlet with the previous project context", () => {
  assert.match(projectLayout, /const \[resolvedProjectID, setResolvedProjectID\] = useState<string \| null>\(null\);/);
  assert.match(projectLayout, /setLoading\(true\);\s*setError\(""\);\s*setProject\(null\);/s);
  assert.match(projectLayout, /setResolvedProjectID\(projectId\);\s*setLoading\(false\);/s);
  assert.match(projectLayout, /if \(loading \|\| resolvedProjectID !== projectId\)/);
});

test("conversation follows streaming updates and explicitly follows dispatched messages", () => {
  assert.match(timelineLib, /function timelineContentVersion\(timeline: TimelineItem\[\], executions: AgentExecution\[\]\): string/);
  assert.match(conversationPage, /const visibleContentVersion = useMemo\(\(\) => timelineContentVersion\(timeline, agentExecutions\), \[timeline, agentExecutions\]\);/);
  assert.match(conversationPage, /scrollToBottom\("auto"\);/);
  assert.match(conversationPage, /\[visibleContentVersion, run, pendingApproval, scrollToBottom\]/);
  assert.doesNotMatch(conversationPage, /\[timeline\.length, run, pendingApproval, scrollToBottom\]/);
  assert.match(conversationPage, /if \(isNarrowConversationLayout\(\)\) \{\s*window\.scrollTo\(\{ top: document\.documentElement\.scrollHeight, behavior \}\);/s);
  assert.match(conversationPage, /container\.scrollTo\(\{ top: Math\.max\(0, container\.scrollHeight - container\.clientHeight\), behavior \}\);/);
  assert.match(conversationPage, /const followAfterDispatch = useCallback\(\(\) => \{\s*userNearBottom\.current = true;\s*setHasNewContent\(false\);\s*requestAnimationFrame\(\(\) => scrollToBottom\("auto"\)\);/s);
  assert.match(conversationPage, /setRun\(finishedRunIds\.current\.has\(data\.runId\) \? "" : data\.runId\);\s*followAfterDispatch\(\);/);
  assert.match(utilsLib, /export function isNarrowConversationLayout/);
  assert.match(conversationPage, /useEffect\(\(\) => \{\s*userNearBottom\.current = true;\s*setHasNewContent\(false\);/s);
});

test("conversation controls navigate direct user messages and continue through older pages", () => {
  assert.match(conversationPage, /const userMessages = useMemo\(\(\) => primaryMessages\.filter\(\(message\) => message\.role === "user"\)/);
  assert.match(conversationPage, /const userMessageElements = useRef\(new Map<string, HTMLDivElement>\(\)\)/);
  assert.match(conversationPage, /data-user-message-id=\{item\.kind === "message" && item\.message\.role === "user"/);
  assert.match(conversationPage, /const goToPreviousUserMessage = \(\) => \{/);
  assert.match(conversationPage, /if \(loadingOlderHistory\) return;/);
  assert.match(conversationPage, /if \(index === -1 && hasMoreMessageHistory\)/);
  assert.match(conversationPage, /setPendingPreviousUserMessageID\(userMessages\[0\]\.id\)/);
  assert.match(conversationPage, /hasMoreMessageHistory/);
  assert.match(conversationPage, /void loadOlderHistory\(\)\.then/);
  assert.match(conversationPage, /disabled=\{loadingOlderHistory \|\| \(userMessageNavigationIndex <= 0 && !hasMoreMessageHistory\)\}/);
  assert.match(conversationPage, /if \(conversationRef\.current\?\.id !== conversationID\) return;/);
  assert.match(conversationPage, /let low = 0;\s*let high = userMessages\.length - 1;/s);
  assert.match(conversationPage, /const scheduleCurrentUserMessageIndexUpdate = useCallback/);
  assert.match(conversationPage, /if \(userMessageIndexFrame\.current !== null\) return;/);
  assert.match(conversationPage, /title="上一条我的消息"/);
  assert.match(conversationPage, /title="下一条我的消息"/);
  assert.match(stylesheet, /\.scroll-btn:disabled\s*\{/);
});

test("clearing context starts a fresh conversation with the current agent and policy", () => {
  assert.match(conversationPage, /const clearConversationContext = \(\) => \{/);
  assert.match(conversationPage, /title: "清空上下文"/);
  assert.match(conversationPage, /message: "将开始一个新的空白会话，当前内容仍可在历史会话中恢复。"/);
  assert.match(conversationPage, /const clearCurrentConversation = async \(\) => \{/);
  assert.match(conversationPage, /`\/api\/conversations\/\$\{conversationID\}\/clear`/);
  assert.match(conversationPage, /if \(!conversation \|\| sending \|\| clearing \|\| shortcutBusy\) return;/);
  assert.match(conversationPage, /if \(!conversation \|\| sending \|\| clearing \|\| shortcutBusy\) return;/);
  assert.match(conversationPage, /const isCurrentConversation = \(\) => !cancelled && conversationRef\.current\?\.id === conversation\.id;/);
  assert.match(conversationPage, /socket\.onmessage = \(raw\) => \{\s*if \(!isCurrentConversation\(\)\) return;/s);
  assert.match(conversationPage, /if \(sending \|\| clearing \|\| shortcutBusy\) \{ closeHistory\(\); return; \}/);
  assert.match(conversationPage, /disabled=\{!!run \|\| clearing \|\| changingPermission\}/);
  assert.match(conversationPage, /if \(!conversation \|\| conversation\.permissionMode === permissionMode \|\| run \|\| clearing\) return;/);
  assert.match(conversationPage, /const current = list\.find\(\(item\) => item\.isCurrent\);/);
  assert.match(conversationPage, /if \(conversationRef\.current\?\.id !== conversationID\) return;\s*setConversation\(updated\);/s);
  assert.match(conversationPage, /const conversationTransitionRef = useRef\(false\);/);
  assert.match(conversationPage, /const conversationRouteVersion = useRef\(0\);/);
  assert.match(conversationPage, /useLayoutEffect\(\(\) => \{\s*conversationRouteVersion\.current\+\+;/s);
  assert.match(conversationPage, /conversationRouteVersion\.current\+\+;/);
  assert.match(conversationPage, /if \(!projectId \|\| conversationTransitionRef\.current\) return;/);
  assert.match(conversationPage, /const clearStillOwnsView = \(\) =>/);
  assert.match(conversationPage, /conversationRouteVersion\.current === routeVersion/);
  assert.match(conversationPage, /setHasNewContent\(false\); userNearBottom\.current = true;/);
  assert.match(conversationPage, /<TaskQueue projectID=\{project\.id\} conversationID=\{conversation\?\.id \|\| ""\}/);
  assert.match(taskQueue, /conversationId: dispatchConversationID/);
  assert.match(taskQueue, /conversationIDRef\.current !== dispatchConversationID/);
  assert.match(taskQueue, /setDispatching\(false\);\s*setRedispatchingTaskID\(null\);/);
  assert.match(taskQueue, /const redispatchRequest = useRef\(0\);/);
  assert.match(conversationPage, /<button className="secondary" type="button" disabled=\{Boolean\(run\) \|\| sending \|\| clearing \|\| Boolean\(shortcutBusy\)\} onClick=\{clearConversationContext\}>清空<\/button><button className="secondary" type="button" disabled=\{sending \|\| clearing\} onClick=\{\(\) => void sendContent\("继续", false\)\}>继续<\/button>/);
});

test("creating a conversation keeps the newly navigated route", () => {
  const newConversation = conversationPage.match(/const newConversation = async[\s\S]*?\n  };\n\n  const clearConversationContext/);
  assert.ok(newConversation, "newConversation handler should exist");
  assert.match(newConversation[0], /`\/api\/projects\/\$\{projectId\}\/conversations\?new=true`/);
  assert.match(newConversation[0], /navigate\(`\/projects\/\$\{projectId\}\/conversations\/\$\{next\.id\}`, \{ replace: true \}\);/);
  // The target route omits ?new, so a second search-param navigation here
  // would resolve against the old conversation route and undo this redirect.
  assert.doesNotMatch(newConversation[0], /navigate\(`\/projects\/\$\{projectId\}\/conversations\/\$\{next\.id\}`, \{ replace: true \}\);[\s\S]*closeNewConversation\(\);/);
});

test("wide screens render aligned shortcut rows and narrow screens keep grouped order", () => {
  assert.match(stylesheet, /\.quick-tag-row\s*\{[^}]*grid-template-columns:\s*repeat\(2,\s*minmax\(0,\s*1fr\)\);/s);
  assert.match(stylesheet, /\.quick-tag-slot\s*\{[^}]*min-height:\s*30px;/s);
  assert.match(stylesheet, /\.quick-actions-mobile\s*\{[^}]*display:\s*none;/s);
  assert.match(stylesheet, /@media \(max-width: 820px\)[\s\S]*?\.quick-actions-row\s*\{[^}]*display:\s*none;/);
  assert.match(stylesheet, /@media \(max-width: 820px\)[\s\S]*?\.quick-actions-mobile\s*\{[^}]*display:\s*grid;/);
  assert.match(stylesheet, /@media \(max-width: 820px\)[\s\S]*?\.quick-actions-mobile \.quick-tag > button:first-child,\s*\.quick-actions-mobile \.quick-tag-empty\s*\{[^}]*flex:\s*1;[^}]*max-width:\s*none;/);
  assert.match(conversationPage, /const shortcutRowCount = Math\.max\(promptShortcutItems\.length, commandShortcutItems\.length\);/);
  assert.match(conversationPage, /className="quick-tag-slot"/);
  assert.doesNotMatch(stylesheet, /\.quick-actions-row\s*\{[^}]*overflow-x:\s*auto/s);
});

test("agent work is progressive: primary chat stays clean and trace details remain available", () => {
  assert.match(timelineLib, /function subagentTextIndex\(events: Event\[\]\)/);
  assert.match(timelineLib, /function isSubagentMessage\(message: Message, indexedTexts: Map<string, number\[\]>\)/);
  assert.match(timelineLib, /if \(message\.runId\) return false;/);
  assert.match(utilsLib, /function mergeConversationItems<T extends \{ id: string; createdAt: string \}>/);
  assert.match(utilsLib, /function websocketAssistantMessageID\(eventID: string, index: number\)/);
  assert.match(conversationPage, /const primaryMessages = useMemo\(\(\) => messages\.filter\(\(message\) => !isSubagentMessage\(message, knownSubagentTexts\)\)/);
  assert.match(conversationPage, /const agentExecutions = useMemo\(\(\) => buildAgentExecutions\(events\)/);
  assert.match(timelineLib, /\["completed", "failed", "stopped", "interrupted", "cancelled"\]\.includes\(execution\.status\)/);
  assert.match(conversationPage, /loadOlderHistory/);
  assert.match(conversationPage, /<AgentExecutionCard key=\{execution\.runId\}/);
  assert.match(conversationPage, /<AgentExecutionDialog execution=/);
  assert.match(timelineLib, /node\.status = "unresolved"/);
  assert.match(conversationPage, /setMessages\(\(current\) => mergeConversationItems\(data\.messages, current\)\)/);
  assert.match(conversationPage, /setEvents\(\(current\) => mergeConversationItems\(data\.events, current\)\)/);
  assert.match(stylesheet, /\.agent-execution-card\s*\{/);
  assert.match(stylesheet, /\.agent-execution-dialog\s*\{/);
});
