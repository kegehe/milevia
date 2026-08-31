import assert from "node:assert/strict";
import test from "node:test";
import { MAX_OPEN_CONVERSATION_TABS, closeConversationTab, markConversationTabRead, openConversationTab, readConversationTabs, recordConversationActivity, writeConversationTabs } from "./conversation-tabs.ts";

function memoryStorage(initial: Record<string, string> = {}) {
  const values = new Map(Object.entries(initial));
  return { getItem: (key: string) => values.get(key) ?? null, setItem: (key: string, value: string) => { values.set(key, value); } };
}

test("conversation tabs are project-scoped and restore only within the browser window", () => {
  const storage = memoryStorage();
  const state = { openConversationIds: ["a", "b"], activeConversationId: "b", readPositions: {}, latestPositions: {}, unreadConversationIds: [] };
  writeConversationTabs("project-a", state, storage);
  assert.deepEqual(readConversationTabs("project-a", storage), state);
  assert.deepEqual(readConversationTabs("project-b", storage), { openConversationIds: [], activeConversationId: null, readPositions: {}, latestPositions: {}, unreadConversationIds: [] });
});

test("opening and closing a tab retains a deterministic adjacent active tab", () => {
  const opened = openConversationTab({ openConversationIds: ["a"], activeConversationId: "a", readPositions: {}, latestPositions: {}, unreadConversationIds: [] }, "b");
  assert.deepEqual(opened, { openConversationIds: ["a", "b"], activeConversationId: "b", readPositions: {}, latestPositions: {}, unreadConversationIds: [] });
  assert.deepEqual(closeConversationTab(opened!, "b"), { openConversationIds: ["a"], activeConversationId: "a", readPositions: {}, latestPositions: {}, unreadConversationIds: [] });
});

test("closing a restored unavailable tab removes its activity state and selects an adjacent tab", () => {
  const state = {
    openConversationIds: ["a", "missing", "b"], activeConversationId: "missing",
    readPositions: { missing: { createdAt: "2026-01-01T00:00:00.000Z", id: "event-a" } },
    latestPositions: { missing: { createdAt: "2026-01-01T00:00:01.000Z", id: "event-b" } },
    unreadConversationIds: ["missing"],
  };
  assert.deepEqual(closeConversationTab(state, "missing"), {
    openConversationIds: ["a", "b"], activeConversationId: "b", readPositions: {}, latestPositions: {}, unreadConversationIds: [],
  });
});

test("opening a new tab respects the per-project limit", () => {
  const full = { openConversationIds: Array.from({ length: MAX_OPEN_CONVERSATION_TABS }, (_, index) => `c-${index}`), activeConversationId: "c-0", readPositions: {}, latestPositions: {}, unreadConversationIds: [] };
  assert.equal(openConversationTab(full, "overflow"), null);
});

test("background activity is unread until its tab reaches the read watermark", () => {
  const initial = { openConversationIds: ["a", "b"], activeConversationId: "a", readPositions: { b: { createdAt: "2026-01-01T00:00:00.000Z", id: "event-a" } }, latestPositions: {}, unreadConversationIds: [] };
  const active = recordConversationActivity(initial, "a", { createdAt: "2026-01-01T00:00:01.000Z", id: "event-b" }, true, true);
  const background = recordConversationActivity(active, "b", { createdAt: "2026-01-01T00:00:02.000Z", id: "event-c" }, true, false);
  assert.deepEqual(background.unreadConversationIds, ["b"]);
  const read = markConversationTabRead(background, "b");
  assert.deepEqual(read.unreadConversationIds, []);
  assert.deepEqual(read.readPositions.b, { createdAt: "2026-01-01T00:00:02.000Z", id: "event-c" });
});
