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
const taskStyles = await readFile(new URL("./tasks.css", import.meta.url), "utf8");
const gitWorkbench = await readFile(new URL("./features/git/GitWorkbench.tsx", import.meta.url), "utf8");
const runPanel = await readFile(new URL("./features/run/ProjectRunPanel.tsx", import.meta.url), "utf8");
const runStyles = await readFile(new URL("./run.css", import.meta.url), "utf8");

const workspaceStyles = stylesheet.slice(stylesheet.indexOf("/* Desktop conversation workspace:"));
const conversationContentStyles = stylesheet.slice(stylesheet.indexOf("/* 对话内容："));

test("message bubbles use their content width without exceeding the reading measure", () => {
  assert.match(stylesheet, /\.message\s*\{[^}]*display:\s*flex;[^}]*min-width:\s*0;[^}]*width:\s*min\(760px,\s*100%\);[^}]*flex-direction:\s*column;[^}]*align-items:\s*flex-start;/s);
  assert.match(conversationContentStyles, /\.message\s*\{\s*width:\s*fit-content;\s*max-width:\s*min\(832px,\s*100%\);\s*\}/);
  assert.match(conversationContentStyles, /\.message\.user\s*\{\s*width:\s*fit-content;\s*max-width:\s*min\(680px,\s*100%\);\s*\}/);
  assert.match(conversationContentStyles, /\.message\s*>\s*\.markdown\s*\{\s*width:\s*fit-content;\s*max-width:\s*100%;/);
});

test("user messages can be copied from an icon-only control", () => {
  assert.match(conversationPage, /navigator\.clipboard\?\.writeText/);
  assert.match(conversationPage, /await navigator\.clipboard\.writeText\(content\);\s*return;\s*} catch \{/s);
  assert.match(conversationPage, /catch \{[\s\S]*?copyWithLegacyClipboard\(content\);/);
  assert.match(conversationPage, /document\.execCommand\("copy"\)/);
  assert.match(conversationPage, /try \{\s*textarea\.select\(\);[\s\S]*?\} finally \{\s*textarea\.remove\(\);\s*\}/);
  assert.match(conversationPage, /isUser && <button className=\{`message-copy/);
  assert.match(conversationPage, /title=\{copied \? "已复制" : "复制消息"\}/);
  assert.match(stylesheet, /\.message-copy\s*\{[^}]*width:\s*26px;[^}]*height:\s*26px;[^}]*background:\s*transparent;/s);
  assert.match(stylesheet, /\.message-copy::before,[\s\S]*?\.message-copy::after/);
});

test("stopping an unanswered direct prompt restores it to the composer", () => {
  assert.match(conversationPage, /const pendingUserDrafts = useRef\(new Map<string, string>\(\)\);/);
  assert.match(conversationPage, /const assistantOutputRuns = useRef\(new Set<string>\(\)\);/);
  assert.match(conversationPage, /const recordAssistantOutput = useCallback[\s\S]*?if \(outputs\.size > 128\)[\s\S]*?pendingUserDrafts\.current\.delete\(runID\);/);
  assert.match(conversationPage, /pendingUserDrafts\.current\.set\(data\.runId, data\.message\.content\)/);
  assert.match(conversationPage, /const draftToRestore = assistantOutputRuns\.current\.has\(runID\) \? undefined : pendingUserDrafts\.current\.get\(runID\);/);
  assert.match(conversationPage, /const restorePendingUserDraft = \(conversationID: string, routeVersion: number, runID: string, draft: string \| undefined\) => \{\s*if \(!draft \|\| conversationRef\.current\?\.id !== conversationID \|\| conversationRouteVersion\.current !== routeVersion\) return;/s);
  assert.match(conversationPage, /setText\(\(current\) => current === "" \? draft : current\);/);
  assert.match(conversationPage, /saveConversationDraft\(projectId, conversationID, restoredText\)/);
  assert.match(conversationPage, /const conversationID = conversation\.id;\s*const routeVersion = conversationRouteVersion\.current;\s*const runID = run;/s);
  assert.match(conversationPage, /restorePendingUserDraft\(conversationID, routeVersion, runID, draftToRestore\);/);
  assert.match(conversationPage, /stopRunInternal\(true, runID\)\.then\(\(result\) => \{ restorePendingUserDraft\(conversationID, routeVersion, runID, draftToRestore\);/);
  assert.match(conversationPage, /pendingUserDrafts\.current\.delete\(event\.runId\);\s*assistantOutputRuns\.current\.delete\(event\.runId\);/s);
});

test("all composer text sources are cached and conversation switches use the target draft", () => {
  assert.match(conversationPage, /const setComposerText = useCallback/);
  assert.match(conversationPage, /if \(conversationRef\.current\?\.id === conversationID\) setComposerText\(preview\.content, conversationID\);/);
  assert.match(conversationPage, /setComposerText\(inputHistory\[historyIndex\.current!\], undefined, false\);/);
  assert.match(conversationPage, /setComposerText\(draftBeforeHistory\.current, undefined, false\);/);
  assert.match(conversationPage, /const nextDraft = projectId \? getConversationDraft\(projectId, next\.id\) : "";/);
  assert.doesNotMatch(conversationPage, /resetConversationView\(next, false\)/);
});

test("input-history previews do not replace the unsent draft when the page closes", () => {
  assert.match(conversationPage, /const textToPersist = historyIndex\.current === null \? textRef\.current : draftBeforeHistory\.current;/);
  assert.match(conversationPage, /saveConversationDraft\(projectId, conversationID, textToPersist\);/);
});

test("a failed send restores its draft only while the original conversation remains active", () => {
  assert.match(conversationPage, /const conversationID = conversation\.id;\s*const routeVersion = conversationRouteVersion\.current;\s*setSending\(true\);/s);
  assert.match(conversationPage, /catch \(cause\) \{\s*if \(conversationRef\.current\?\.id !== conversationID \|\| conversationRouteVersion\.current !== routeVersion\) return;\s*fail\(cause instanceof Error \? cause\.message : "无法发送消息"\);\s*if \(clearDraft\) \{/s);
});

test("conversation entries keep card styles separate from their layout and align user messages to the reading column", () => {
  assert.match(conversationPage, /className=\{`timeline-entry \$\{item\.kind === "message" \? "message-entry" : item\.kind\}`\}/);
  assert.doesNotMatch(conversationPage, /className=\{`timeline-entry \$\{item\.kind\}`\}/);
  assert.match(stylesheet, /\.timeline-entry\.message-entry,\s*\.timeline-entry\.tool,\s*\.timeline-entry\.error\s*\{\s*display:\s*flex;\s*width:\s*min\(832px,\s*100%\);\s*margin-right:\s*auto;\s*margin-left:\s*auto;\s*padding:\s*0;\s*\}/s);
  assert.match(stylesheet, /\.timeline-entry\.message-entry:has\(\.message\.user\)\s*\{\s*justify-content:\s*flex-end;\s*\}/);
  assert.match(stylesheet, /\.message\.user\s*\{\s*align-items:\s*flex-end;\s*margin-left:\s*auto;\s*margin-right:\s*0;\s*\}/);
  assert.match(stylesheet, /\.timeline::before\s*\{\s*display:\s*none;\s*\}/);
  assert.match(stylesheet, /\.timeline-entry::before\s*\{\s*display:\s*none;\s*\}/);
  assert.match(stylesheet, /\.chat-center \.scroll-buttons\s*\{[^}]*right:\s*18px;[^}]*bottom:\s*calc\(var\(--composer-height,\s*0px\)\s*\+\s*18px\);/s);
  assert.match(stylesheet, /\.scroll-btn-icon\s*\{[^}]*stroke:\s*currentColor;/s);
  assert.match(conversationPage, /<ScrollNavigationIcon direction="top"\s*\/>/);
  assert.match(conversationPage, /<ScrollNavigationIcon direction="bottom"\s*\/>/);
  assert.match(stylesheet, /@media \(min-width: 821px\) and \(max-width: 1199px\)[\s\S]*?\.task-queue-rail\s*\{[^}]*right:\s*56px;/s);
  assert.match(conversationContentStyles, /\.chat-center > \.timeline\s*\{[^}]*padding:\s*28px\s+24px\s+24px;/s);
  assert.match(conversationContentStyles, /@media \(max-width: 820px\)[\s\S]*?\.chat-center > \.timeline\s*\{[^}]*padding:\s*20px\s+16px\s+max\(32px,\s*var\(--composer-height,\s*0px\)\)\s+16px;/s);
});

test("desktop canvas uses three working columns without a page inset", () => {
  assert.match(workspaceStyles, /\.conversation-canvas\s*\{[^}]*width:\s*100%;[^}]*grid-template-columns:\s*minmax\(260px,\s*300px\)\s+minmax\(440px,\s*1fr\)\s+minmax\(280px,\s*340px\);[^}]*margin:\s*0;/s);
  assert.match(conversationPage, /<aside className="quick-tag-rail"[\s\S]*?<section className="chat-center">[\s\S]*?<aside className="task-queue-rail"/);
});

test("stop and clear retries with force and reports a force-stop failure", () => {
  assert.match(conversationPage, /\/api\/runs\/\$\{runID\}\/stop\?force=true/);
  assert.match(conversationPage, /function requiresForceStop\(cause: unknown\): boolean[\s\S]*?active_runs_present/);
  assert.match(conversationPage, /if \(requiresForceStop\(cause\)\)/);
  assert.doesNotMatch(conversationPage, /message\.includes\("queued"\)/);
  assert.match(conversationPage, /const result = await stopRunInternal\(false\);\s*if \(result !== "stopping"\) setStopping\(false\);/s);
  assert.match(conversationPage, /projectApi<\{ conversation: Conversation; activeRunId: string \| null \}>\(`\/api\/conversations\/\$\{conversationID\}\?limit=1`\)/);
  assert.match(conversationPage, /status\.conversation\.status === "idle" && !status\.activeRunId/);
  assert.match(conversationPage, /await stopRunInternal\(true\);\s*}\s*catch \(forceCause\) \{\s*fail\(forceCause instanceof Error \? forceCause\.message : "无法强制停止任务"\);\s*setStopping\(false\);\s*return;/s);
  assert.doesNotMatch(conversationPage, /try \{ await stopRunInternal\(true\); \} catch \{ \/\* best effort \*\/ \}/);
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

test("project workspaces expose global request failures", () => {
  assert.match(projectLayout, /const \{ error: globalError, setError: setGlobalError, refreshProjects \} = useProjectContext\(\);/);
  assert.match(projectLayout, /\{globalError && <div className="error" role="alert"><span>\{globalError\}<\/span><button type="button" title="关闭错误提示" onClick=\{\(\) => setGlobalError\(""\)\}>x<\/button><\/div>\}/);
  assert.match(projectLayout, /<button className="back-projects" type="button" title="返回项目列表" aria-label="返回项目列表"[\s\S]*?<BackProjectsIcon \/><\/button>\s*<h2>\{project\.name\}<\/h2>/);
  assert.doesNotMatch(projectLayout, /<label>\{project\.runner\}<\/label>/);
  assert.doesNotMatch(projectLayout, /<code>\{project\.pathDisplay\}<\/code>/);
});

test("desktop conversation is a full-height three-column workspace", () => {
  assert.match(workspaceStyles, /\.chat\s*\{[^}]*height:\s*100dvh;[^}]*min-height:\s*0;[^}]*overflow:\s*hidden;/s);
  assert.match(workspaceStyles, /\.conversation-canvas\s*\{[^}]*position:\s*relative;[^}]*width:\s*100%;[^}]*min-height:\s*0;[^}]*grid-template-columns:\s*minmax\(260px,\s*300px\)\s+minmax\(440px,\s*1fr\)\s+minmax\(280px,\s*340px\);/s);
  assert.match(workspaceStyles, /\.quick-tag-rail\s*\{[^}]*align-self:\s*stretch;[^}]*min-height:\s*0;[^}]*overflow-y:\s*auto;/s);
  assert.match(workspaceStyles, /\.task-queue-rail\s*\{[^}]*display:\s*flex;[^}]*min-width:\s*0;[^}]*min-height:\s*0;[^}]*border-left:/s);
  assert.match(workspaceStyles, /\.task-queue-rail \.task-queue-list\s*\{[^}]*min-height:\s*0;[^}]*flex:\s*1;[^}]*overflow-y:\s*auto;/s);
  assert.match(workspaceStyles, /\.chat-center\s*\{[^}]*display:\s*grid;[^}]*min-height:\s*0;[^}]*grid-template-rows:\s*minmax\(0,\s*1fr\)\s+auto;/s);
  assert.match(workspaceStyles, /\.chat-center > \.timeline\s*\{[^}]*min-height:\s*0;[^}]*overflow-y:\s*auto;/s);
  assert.match(workspaceStyles, /\.chat-center > \.composer\s*\{[^}]*position:\s*static;/s);
  assert.match(workspaceStyles, /@media \(min-width: 821px\) and \(max-width: 1199px\)[\s\S]*?\.task-queue-rail\s*\{[^}]*position:\s*absolute;[^}]*top:\s*16px;[^}]*right:\s*56px;/s);
  assert.match(workspaceStyles, /@media \(max-width: 820px\)[\s\S]*?\.conversation-canvas\s*\{[^}]*grid-template-columns:\s*minmax\(0,\s*1fr\);[^}]*grid-template-rows:\s*auto\s+auto\s+minmax\(0,\s*1fr\);/s);
  assert.match(workspaceStyles, /@media \(max-width: 820px\)[\s\S]*?\.task-queue-rail\s*\{[^}]*grid-column:\s*1;[^}]*grid-row:\s*2;/s);
});

test("task queue has one named landmark", () => {
  assert.match(conversationPage, /<aside className="task-queue-rail" aria-label="任务队列">/);
  assert.match(taskQueue, /return <div className=\{`task-queue \$\{mobileOpen \? "mobile-open" : ""\}`\}>/);
  assert.doesNotMatch(taskQueue, /<section className=\{`task-queue[^>]*aria-label="任务队列"/);
});

test("shortcut and task cards keep their intrinsic row heights", () => {
  assert.match(stylesheet, /\.quick-actions-row\s*\{[^}]*align-content:\s*start;[^}]*gap:\s*10px;/s);
  assert.match(taskStyles, /\.task-queue-list\s*\{[^}]*align-content:\s*start;[^}]*gap:\s*6px;/s);
});

test("conversation content reserves space for its controls without legacy footer padding", () => {
  assert.match(workspaceStyles, /\.chat-center > \.timeline\s*\{[^}]*padding:\s*32px\s+0\s+20px\s+16px;/s);
  assert.match(workspaceStyles, /@media \(max-width: 820px\)[\s\S]*?\.chat-center > \.timeline\s*\{[^}]*padding:\s*20px\s+0\s+max\(32px, var\(--composer-height, 0px\)\)\s+16px;/);
  assert.doesNotMatch(workspaceStyles, /padding-bottom:\s*(?:130|160)px;/);
  assert.match(conversationPage, /const updateBottomSafeArea = \(\) => \{\s*const followBottom = userNearBottom\.current;\s*const composerHeight = `\$\{Math\.ceil\(composer\.getBoundingClientRect\(\)\.height\)\}px`;\s*timelineElement\.style\.setProperty\("--composer-height", composerHeight\);\s*timelineElement\.parentElement\?\.style\.setProperty\("--composer-height", composerHeight\);\s*if \(!followBottom\) return;/s);
  assert.match(conversationPage, /const observer = new ResizeObserver\(updateBottomSafeArea\);/);
  assert.match(conversationPage, /if \(userNearBottom\.current\) scrollToBottom\("auto"\);/);
});

test("Codex and Claude use the same version check and update controls", () => {
  assert.match(conversationPage, /const agentPath = agentID === "codex" \? "codex" : "claude";/);
  assert.match(conversationPage, /\/api\/runners\/\$\{runnerID\}\/\$\{agentPath\}\/check-update/);
  assert.match(conversationPage, /\/api\/runners\/\$\{runnerID\}\/\$\{agentPath\}\/update/);
  assert.match(conversationPage, /!run && tool\?\.status === "ready" && <button className="runner-inline-btn"/);
  assert.match(conversationPage, /确认更新 \{toolName\}/);
  assert.doesNotMatch(conversationPage, /status: "ready" as const/);
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
  // 左右分栏：.run-body 水平排列，左侧日志 flex:1，右侧侧栏保持受控宽度并可独立滚动。
  assert.match(runStyles, /\.run-body\s*\{[^}]*display:\s*flex;[^}]*min-height:\s*0;[^}]*overflow:\s*hidden;/s);
  assert.match(runStyles, /\.run-sidebar\s*\{[^}]*width:\s*348px;[^}]*min-width:\s*300px;[^}]*max-width:\s*420px;[^}]*overflow-y:\s*auto;/s);
  // 日志终端使用浅薄荷底色与足够的文字对比度，并保留 ANSI 语义颜色。
  assert.match(runStyles, /\.run-log-terminal\s*\{[^}]*background:\s*#f4faf6;/s);
  assert.match(runStyles, /\.run-log-terminal\s*\{[^}]*color:\s*#29473d;/s);
  assert.match(runPanel, /toAnsiSegments\(runLogText\(entry\)\)/);
  assert.match(runStyles, /\.ansi-green, \.ansi-bright-green\s*\{\s*color:\s*#287156;/);
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
  assert.match(conversationPage, /\[visibleContentVersion, run, pendingApproval, scrollToBottomNextFrame\]/);
  assert.doesNotMatch(conversationPage, /\[timeline\.length, run, pendingApproval, scrollToBottom\]/);
  // 窄屏下 timeline overflow: visible，滚动容器是 window
  assert.match(conversationPage, /if \(isNarrowConversationLayout\(\)\) \{\s*\/\/ 窄屏下 timeline overflow: visible/s);
  assert.match(conversationPage, /window\.scrollTo\(\{ top: document\.documentElement\.scrollHeight, behavior \}\);/);
  assert.match(conversationPage, /container\.scrollTo\(\{ top: Math\.max\(0, container\.scrollHeight - container\.clientHeight\), behavior \}\);/);
  // scrollToBottomNextFrame 延迟一帧确保 DOM 更新后再读取 scrollHeight
  assert.match(conversationPage, /const scrollToBottomNextFrame = useCallback\(\(behavior: ScrollBehavior = "auto"\) => \{\s*requestAnimationFrame\(\(\) => scrollToBottom\(behavior\)\);/s);
  assert.match(conversationPage, /const followAfterDispatch = useCallback\(\(\) => \{\s*userNearBottom\.current = true;\s*setHasNewContent\(false\);\s*scrollToBottomNextFrame\("auto"\);/s);
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
  assert.match(conversationPage, /const clearCurrentConversation = async \(skipRunGuard = false\) => \{/);
  assert.match(conversationPage, /`\/api\/conversations\/\$\{conversationID\}\/clear`/);
  assert.match(conversationPage, /if \(!conversation \|\| sending \|\| clearing \|\| stopping \|\| shortcutBusy\) return;/);
  assert.match(conversationPage, /if \(!conversation \|\| sending \|\| clearing \|\| stopping \|\| shortcutBusy\) return;/);
  assert.match(conversationPage, /const isCurrentConversation = \(\) => !cancelled && conversationRef\.current\?\.id === conversation\.id;/);
  assert.match(conversationPage, /socket\.onmessage = \(raw\) => \{\s*if \(!isCurrentConversation\(\)\) return;/s);
  assert.match(conversationPage, /if \(sending \|\| clearing \|\| stopping \|\| shortcutBusy\) \{ closeHistory\(\); return; \}/);
  assert.match(conversationPage, /disabled=\{!!run \|\| clearing \|\| stopping \|\| changingPermission\}/);
  assert.match(conversationPage, /if \(!conversation \|\| conversation\.permissionMode === permissionMode \|\| run \|\| clearing \|\| stopping\) return;/);
  assert.match(conversationPage, /if \(!conversation \|\| run \|\| clearing \|\| stopping\) return;/);
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
  assert.match(conversationPage, /<TaskQueue projectID=\{project\.id\} conversationID=\{conversation\?\.id \|\| ""\}[\s\S]*?dispatchDisabled=\{clearing \|\| stopping \|\| !conversation\}/);
  assert.match(taskQueue, /conversationId: dispatchConversationID/);
  assert.match(taskQueue, /conversationIDRef\.current !== dispatchConversationID/);
  assert.match(taskQueue, /setDispatching\(false\);\s*setRedispatchingTaskID\(null\);/);
  assert.match(taskQueue, /const redispatchRequest = useRef\(0\);/);
  assert.match(conversationPage, /<button className="secondary composer-action composer-clear" type="button" disabled=\{sending \|\| clearing \|\| stopping \|\| Boolean\(shortcutBusy\)\} onClick=\{clearConversationContext\}><ComposerActionIcon action="clear" \/><span>清空<\/span><\/button>/);
  assert.match(conversationPage, /<button className="secondary composer-action composer-continue" type="button" disabled=\{sending \|\| clearing \|\| stopping\} onClick=\{\(\) => void sendContent\("继续", false\)\}><ComposerActionIcon action="continue" \/><span>继续<\/span><\/button>/);
  assert.match(conversationPage, /<button className="primary composer-action composer-send" disabled=\{!text\.trim\(\) \|\| sending \|\| clearing \|\| stopping \|\| Boolean\(shortcutBusy\)\}><ComposerActionIcon action="send" \/>/);
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
  assert.match(stylesheet, /\.quick-actions-row \.quick-tag > button:first-child,\s*\.quick-actions-row \.quick-tag-empty\s*\{[^}]*overflow:\s*hidden;[^}]*text-overflow:\s*ellipsis;[^}]*white-space:\s*nowrap;/s);
  assert.match(conversationPage, /const shortcutRowCount = Math\.max\(promptShortcutItems\.length, commandShortcutItems\.length\);/);
  assert.match(conversationPage, /title=\{shortcut\.enabled \? shortcut\.template : `\$\{shortcut\.template\}\\n\\n\$\{shortcut\.name\}已停用`\}/);
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

test("known Codex stdin notices do not render as errors", () => {
  assert.match(timelineLib, /const ignoredCodexStderr = new Set\(\["Reading additional input from stdin\.\.\."\]\);/);
  assert.match(timelineLib, /function isIgnoredCLIStderr\(message: string\): boolean/);
  assert.match(timelineLib, /event\.type === "stderr"[\s\S]*?!isIgnoredCLIStderr\(payload\.message\)/);
});
