import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const stylesheet = await readFile(new URL("./conversation.css", import.meta.url), "utf8");
const component = await readFile(new URL("./App.tsx", import.meta.url), "utf8");

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

test("desktop conversation is a full-height two-column workspace", () => {
  assert.match(workspaceStyles, /\.chat\s*\{[^}]*height:\s*100dvh;[^}]*min-height:\s*0;[^}]*overflow:\s*hidden;/s);
  assert.match(workspaceStyles, /\.conversation-canvas\s*\{[^}]*width:\s*100%;[^}]*min-height:\s*0;[^}]*grid-template-columns:\s*minmax\(280px,\s*320px\)\s+minmax\(0,\s*1fr\);/s);
  assert.match(workspaceStyles, /\.quick-tag-rail\s*\{[^}]*align-self:\s*stretch;[^}]*min-height:\s*0;[^}]*overflow-y:\s*auto;/s);
  assert.match(workspaceStyles, /\.chat-center\s*\{[^}]*display:\s*grid;[^}]*min-height:\s*0;[^}]*grid-template-rows:\s*minmax\(0,\s*1fr\)\s+auto;/s);
  assert.match(workspaceStyles, /\.chat-center > \.timeline\s*\{[^}]*min-height:\s*0;[^}]*overflow-y:\s*auto;/s);
  assert.match(workspaceStyles, /\.chat-center > \.composer\s*\{[^}]*position:\s*static;/s);
  assert.doesNotMatch(component, /chat-right/);
});

test("conversation content reserves space for its controls without legacy footer padding", () => {
  assert.match(workspaceStyles, /\.chat-center > \.timeline\s*\{[^}]*padding:\s*32px\s+clamp\(32px,\s*4vw,\s*48px\)\s+40px;/s);
  assert.match(workspaceStyles, /@media \(max-width: 820px\)[\s\S]*?\.chat-center > \.timeline\s*\{[^}]*padding:\s*20px\s+56px\s+32px\s+16px;/);
  assert.doesNotMatch(workspaceStyles, /padding-bottom:\s*(?:130|160)px;/);
});

test("approval and task-board views keep working inside the workspace", () => {
  assert.match(workspaceStyles, /\.chat-center > \.composer\.has-approval\s*\{[^}]*padding-top:\s*0;/s);
  assert.match(workspaceStyles, /\.chat-center \.approval-banner-body\s*\{[^}]*width:\s*100%;[^}]*min-width:\s*0;[^}]*flex-wrap:\s*wrap;[^}]*padding:\s*12px\s+0;/s);
  assert.match(workspaceStyles, /@media \(min-width: 821px\)[\s\S]*?\.chat > \.task-workspace\s*\{[^}]*flex:\s*1;[^}]*min-height:\s*0;[^}]*overflow:\s*auto;/);
});

test("conversation follows every visible streaming update only while the reader is at the bottom", () => {
  assert.match(component, /function timelineContentVersion\(timeline: TimelineItem\[\], executions: AgentExecution\[\]\): string/);
  assert.match(component, /const visibleContentVersion = useMemo\(\(\) => timelineContentVersion\(timeline, agentExecutions\), \[timeline, agentExecutions\]\);/);
  assert.match(component, /scrollToBottom\("auto"\);/);
  assert.match(component, /\[visibleContentVersion, run, pendingApproval, scrollToBottom\]/);
  assert.doesNotMatch(component, /\[timeline\.length, run, pendingApproval, scrollToBottom\]/);
  assert.match(component, /if \(isNarrowConversationLayout\(\)\) \{\s*bottom\.current\?\.scrollIntoView\(\{ behavior, block: "end" \}\);/s);
  assert.match(component, /const media = window\.matchMedia\("\(max-width: 820px\)"\);/);
  assert.match(component, /useEffect\(\(\) => \{\s*userNearBottom\.current = true;\s*setHasNewContent\(false\);/s);
});

test("conversation controls navigate direct user messages and continue through older pages", () => {
  assert.match(component, /const userMessages = useMemo\(\(\) => primaryMessages\.filter\(\(message\) => message\.role === "user"\)/);
  assert.match(component, /const userMessageElements = useRef\(new Map<string, HTMLDivElement>\(\)\)/);
  assert.match(component, /data-user-message-id=\{item\.kind === "message" && item\.message\.role === "user"/);
  assert.match(component, /const goToPreviousUserMessage = \(\) => \{/);
  assert.match(component, /if \(loadingOlderHistory\) return;/);
  assert.match(component, /if \(index === -1 && hasMoreMessageHistory\)/);
  assert.match(component, /setPendingPreviousUserMessageID\(userMessages\[0\]\.id\)/);
  assert.match(component, /hasMoreMessageHistory/);
  assert.match(component, /void loadOlderHistory\(\)\.then/);
  assert.match(component, /disabled=\{loadingOlderHistory \|\| \(userMessageNavigationIndex <= 0 && !hasMoreMessageHistory\)\}/);
  assert.match(component, /if \(conversationRef\.current\?\.id !== conversationID\) return;/);
  assert.match(component, /let low = 0;\s*let high = userMessages\.length - 1;/s);
  assert.match(component, /const scheduleCurrentUserMessageIndexUpdate = useCallback/);
  assert.match(component, /if \(userMessageIndexFrame\.current !== null\) return;/);
  assert.match(component, /title="上一条我的消息"/);
  assert.match(component, /title="下一条我的消息"/);
  assert.match(stylesheet, /\.scroll-btn:disabled\s*\{/);
});

test("wide screens render aligned shortcut rows and narrow screens keep grouped order", () => {
  assert.match(stylesheet, /\.quick-tag-row\s*\{[^}]*grid-template-columns:\s*repeat\(2,\s*minmax\(0,\s*1fr\)\);/s);
  assert.match(stylesheet, /\.quick-tag-slot\s*\{[^}]*min-height:\s*42px;/s);
  assert.match(stylesheet, /\.quick-actions-mobile\s*\{[^}]*display:\s*none;/s);
  assert.match(stylesheet, /@media \(max-width: 820px\)[\s\S]*?\.quick-actions-row\s*\{[^}]*display:\s*none;/);
  assert.match(stylesheet, /@media \(max-width: 820px\)[\s\S]*?\.quick-actions-mobile\s*\{[^}]*display:\s*grid;/);
  assert.match(stylesheet, /@media \(max-width: 820px\)[\s\S]*?\.quick-actions-mobile \.quick-tag > button:first-child,\.quick-actions-mobile \.quick-tag-empty\s*\{[^}]*flex:\s*1;[^}]*max-width:\s*none;/);
  assert.match(component, /const shortcutRowCount = Math\.max\(promptShortcutItems\.length, commandShortcutItems\.length\);/);
  assert.match(component, /className="quick-tag-slot"/);
  assert.doesNotMatch(stylesheet, /\.quick-actions-row\s*\{[^}]*overflow-x:\s*auto/s);
});

test("agent work is progressive: primary chat stays clean and trace details remain available", () => {
  assert.match(component, /function subagentTextIndex\(events: Event\[\]\)/);
  assert.match(component, /function isSubagentMessage\(message: Message, indexedTexts: Map<string, number\[\]>\)/);
  assert.match(component, /if \(message\.runId\) return false;/);
  assert.match(component, /function mergeConversationItems<T extends \{ id: string; createdAt: string \}>/);
  assert.match(component, /function websocketAssistantMessageID\(eventID: string, index: number\)/);
  assert.match(component, /const primaryMessages = useMemo\(\(\) => messages\.filter\(\(message\) => !isSubagentMessage\(message, knownSubagentTexts\)\)/);
  assert.match(component, /const agentExecutions = useMemo\(\(\) => buildAgentExecutions\(events\)/);
  assert.match(component, /\["completed", "failed", "stopped", "interrupted", "cancelled"\]\.includes\(execution\.status\)/);
  assert.match(component, /loadOlderHistory/);
  assert.match(component, /<AgentExecutionCard key=\{execution\.runId\}/);
  assert.match(component, /<AgentExecutionDialog execution=/);
  assert.match(component, /node\.status = "unresolved"/);
  assert.match(component, /setMessages\(\(current\) => mergeConversationItems\(data\.messages, current\)\)/);
  assert.match(component, /setEvents\(\(current\) => mergeConversationItems\(data\.events, current\)\)/);
  assert.match(stylesheet, /\.agent-execution-card\s*\{/);
  assert.match(stylesheet, /\.agent-execution-dialog\s*\{/);
});
