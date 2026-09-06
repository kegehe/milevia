import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const page = await readFile(new URL("./pages/MobileRemotePage.tsx", import.meta.url), "utf8");
const styles = await readFile(new URL("./pages/mobile-remote.css", import.meta.url), "utf8");

test("mobile new conversations choose and submit the selected agent", () => {
  assert.match(page, /newConversationAgent.*useState<"claude-code" \| "codex">/s);
  assert.match(page, /payload: \{ agentId \}/);
  assert.match(page, /role="radio"[\s\S]*newConversationAgent === "claude-code"/);
  assert.match(page, /role="radio"[\s\S]*newConversationAgent === "codex"/);
  assert.match(page, /createConversationForProject\(newConversationProject, newConversationAgent\)/);
});

test("mobile processing indicator waits for durable completion", () => {
  assert.match(page, /mobile-agent-processing/);
  assert.match(page, /snapshotRevision: number/);
  assert.match(page, /snapshot\.snapshotRevision > state\.snapshotRevision/);
  assert.match(page, /hasPendingMessage[\s\S]*remoteConversation\.status !== "running"/);
  assert.match(page, /startup failure, cancellation, or service restart/);
  assert.doesNotMatch(page, /clearConversationProcessing\(payload\.conversationId, true\)/);
  assert.match(styles, /@keyframes mobile-agent-processing-dot/);
});

test("mobile processing state is scoped to the selected instance", () => {
  assert.match(page, /pendingMessageRef\.current\.clear\(\)/);
  assert.match(page, /realtimeMessagesRef\.current\.clear\(\)/);
  assert.match(page, /setProcessingConversations\(\{\}\)/);
});

test("mobile snapshots normalize missing revisions for offline recovery", () => {
  assert.match(page, /Number\.isFinite\(value\.snapshotRevision\)/);
  assert.match(page, /Number\.isFinite\(snapshot\.snapshotRevision\)/);
});

test("mobile projects without a conversation open agent selection before creating", () => {
  assert.match(page, /if \(existingConversation\)[\s\S]*?setMobileView\("conversation"\);[\s\S]*?setNewConversationProject\(projectValue\);/);
  assert.match(page, /if \(!agentId\) \{[\s\S]*?openNewConversation\(projectValue\);[\s\S]*?return;/);
});

test("mobile conversation titles include the active agent", () => {
  assert.match(page, /function conversationAgentLabel\(agentId: string\)/);
  assert.match(page, /title: `\$\{item\.title \|\| "未命名会话"\} · \$\{conversationAgentLabel\(item\.agentId\)\}`/);
  assert.match(styles, /\.mobile-new-conversation-backdrop\s*\{/);
});

test("mobile composer only shows the send button when its textarea has content", () => {
  assert.match(page, /<button type="submit" disabled=\{busy \|\| !conversation \|\| !messageDraft\.trim\(\)\}/);
  assert.match(styles, /\.mobile-composer\s*\{[^}]*display:\s*flex;/s);
  assert.match(styles, /\.mobile-composer textarea\s*\{[^}]*flex:\s*1;/s);
  assert.match(styles, /\.mobile-composer button\s*\{[^}]*display:\s*none;/s);
  assert.match(styles, /\.mobile-composer button:not\(:disabled\)\s*\{\s*display:\s*grid;/);
});

test("mobile task controls support dispatch, edit, and delete commands", () => {
  assert.match(page, /task\.dispatch/);
  assert.match(page, /task\.update/);
  assert.match(page, /task\.delete/);
  assert.match(page, /编辑任务/);
  assert.match(page, /确认删除/);
  assert.match(styles, /\.mobile-task-modal-backdrop/);
  assert.match(styles, /\.mobile-task-delete/);
});

test("mobile task controls reconcile busy and status changes without duplicates", () => {
  assert.match(page, /data-mobile-task-delete/);
  assert.match(page, /remove\.disabled = busy \|\| task\.status === "running"/);
  assert.match(page, /edit\.hidden = !canEdit/);
  assert.match(page, /任务描述不能为空/);
});

test("mobile pairing QR URLs contain only the pairing identifier", () => {
  assert.match(page, /function pairingURLWithCode\(value: string \| undefined, pairingID: string\): string/);
  assert.match(page, /parsed\.searchParams\.set\("pairingId", pairingID\);/);
  assert.match(page, /parsed\.searchParams\.delete\("code"\);/);
  assert.match(page, /pairingURLWithCode\(value\.pairingURL, value\.pairingId \|\| ""\)/);
  assert.doesNotMatch(page, /pairingURLWithCode\([^\n]*value\.code/);
});

test("mobile QR scans without an embedded code fall back to manual verification", () => {
  assert.match(page, /setPairingID\(scanned\.pairingID\);/);
  assert.match(page, /setPairingCode\(scanned\.code \|\| ""\);/);
  assert.match(page, /if \(\/\^\\d\{6\}\$\/\.test\(scanned\.code\)\)[\s\S]*?claimPairing\(scanned\.pairingID, scanned\.code\)/);
  assert.match(page, /setPairingStatus\("[^"]*6[^"]*"\);/);
  assert.match(page, /mobile-pairing-manual/);
  assert.match(page, /\/v1\/pairings\/claim/);
});

test("mobile conversations render markdown and project cards expose keyboard activation", () => {
  assert.match(page, /<ReactMarkdown remarkPlugins=\{\[remarkGfm\]\}/);
  assert.match(page, /className="mobile-message-markdown markdown"/);
  assert.match(page, /className=\{`mobile-project \$\{project\?\.id === item\.id \? "selected" : ""\}`\}[^>]*role="button"[^>]*tabIndex=\{0\}/);
  assert.match(page, /if \(event\.key === "Enter" \|\| event\.key === " "\)/);
});

test("mobile web exposes notification permission when the browser supports it", () => {
  assert.match(page, /notificationPermission === "default"/);
  assert.match(page, /setNotificationPermission\(permission\)/);
  assert.match(page, /onClick=\{\(\) => void enableMobileNotifications\(\)\}/);
  assert.doesNotMatch(page, /Notification\.permission !== "granted" && <button className="mobile-notification-button"/);
});

test("legacy QR URLs automatically submit their embedded pairing code", () => {
  assert.match(page, /const initialPairing = useRef\(\{/);
  assert.match(page, /get\("pairingId"\) \|\| new URLSearchParams\(location\.search\)\.get\("pairing_id"\)/);
  assert.match(page, /const \{ pairingID: initialPairingID, code: initialPairingCode \} = initialPairing\.current/);
  assert.match(page, /initialPairing\.current\.attempted/);
  assert.match(page, /initialPairing\.current\.attempted = true/);
  assert.match(page, /if \(!mobileApp \|\| initialPairing\.current\.attempted \|\| !initialPairingID \|\| !\/\^\\d\{6\}\$\//);
  assert.match(page, /void claimPairing\(initialPairingID, initialPairingCode\)/);
});

test("mobile cloud request timeout covers response body parsing", () => {
  assert.match(page, /response = await fetch[\s\S]*?const body = await response\.json\(\)/);
  assert.match(page, /const body = await response\.json\(\)\.catch\(\(\) => null\);[\s\S]*?finally \{/);
});

test("mobile event stream uses an Authorization header instead of a URL token", () => {
  assert.match(page, /Accept: "text\/event-stream", Authorization: `Bearer \$\{token\}`/);
  assert.match(page, /headers\.set\("Last-Event-ID", lastEventID\)/);
  assert.doesNotMatch(page, /access_token/);
  assert.doesNotMatch(page, /new EventSource\(/);
});
