import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const page = await readFile(new URL("./pages/ConversationPage.tsx", import.meta.url), "utf8");

test("Codex context handoff is explicit and switches only after completion", () => {
  assert.match(page, /conversation\?\.agentId === "codex"/);
  assert.match(page, /\/api\/conversations\/\$\{conversation\.id\}\/handoff/);
  assert.match(page, /\/api\/conversation-handoffs\/\$\{handoff\.id\}/);
  assert.match(page, /current\.status !== "completed" \|\| !current\.targetConversationId/);
  assert.match(page, /const handoffStillOwnsView = \(\) =>/);
  assert.match(page, /if \(!handoffStillOwnsView\(\)\) return;/);
  assert.match(page, /replaceConversationTab\(conversationID, detail\.conversation\.id\)/);
});

test("context handoff freezes every message-dispatch path", () => {
  assert.match(page, /if \(!conversation \|\| readOnlyConversation \|\| sending \|\| clearing \|\| stopping \|\| handingOff \|\| shortcutBusy(?: \|\| changingModel)?\) return false;/);
  assert.match(page, /if \(sending \|\| clearing \|\| stopping \|\| handingOff \|\| shortcutBusy(?: \|\| changingModel)?\) return;/);
  assert.match(page, /if \(sending \|\| clearing \|\| stopping \|\| handingOff \|\| shortcutBusy(?: \|\| changingModel)?\) return false;/);
  assert.match(page, /disabled=\{readOnlyConversation \|\| sending \|\| clearing \|\| stopping \|\| handingOff(?: \|\| changingModel)? \|\| Boolean\(shortcutBusy\)\}/);
});
