import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const stylesheet = await readFile(new URL("./conversation.css", import.meta.url), "utf8");
const component = await readFile(new URL("./App.tsx", import.meta.url), "utf8");

test("desktop canvas preserves the rail width while using a small left inset", () => {
  assert.match(stylesheet, /\.conversation-canvas\s*\{[^}]*grid-template-columns:\s*300px\s+minmax\(0,\s*1fr\)[^}]*margin:\s*0\s+0\s+0\s+24px;/s);
  assert.doesNotMatch(stylesheet, /@media \(max-width: 1080px\)\s*\{\s*\.conversation-canvas\s*\{[^}]*grid-template-columns:\s*250px/s);
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
