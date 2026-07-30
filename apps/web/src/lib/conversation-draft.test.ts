import assert from "node:assert/strict";
import test from "node:test";
import {
  conversationDraftKey,
  getConversationDraft,
  persistConversationDraft,
  readConversationDraft,
  readConversationDrafts,
  updateConversationDraft,
} from "./conversation-draft.ts";

function memoryStorage(initial: Record<string, string> = {}) {
  const values = new Map(Object.entries(initial));
  return {
    get length() { return values.size; },
    getItem: (key: string) => values.get(key) ?? null,
    setItem: (key: string, value: string) => { values.set(key, value); },
    removeItem: (key: string) => { values.delete(key); },
    key: (index: number) => [...values.keys()][index] ?? null,
  };
}

test("conversation drafts are isolated by project and conversation", () => {
  const drafts = updateConversationDraft({}, "project/a", "conversation:1", "未发送内容");
  const next = updateConversationDraft(drafts, "project/a", "conversation:2", "另一份草稿");

  assert.equal(getConversationDraft(next, "project/a", "conversation:1"), "未发送内容");
  assert.equal(getConversationDraft(next, "project/a", "conversation:2"), "另一份草稿");
  assert.notEqual(conversationDraftKey("project/a", "conversation:1"), conversationDraftKey("project/a", "conversation:1:extra"));
});

test("clearing a draft removes only the selected conversation", () => {
  const drafts = {
    ...updateConversationDraft({}, "project-1", "conversation-1", "草稿"),
    ...updateConversationDraft({}, "project-1", "conversation-2", "保留"),
  };
  const next = updateConversationDraft(drafts, "project-1", "conversation-1", "");

  assert.equal(getConversationDraft(next, "project-1", "conversation-1"), "");
  assert.equal(getConversationDraft(next, "project-1", "conversation-2"), "保留");
});

test("a successful persistent clear also removes a stale memory fallback", () => {
  const staleFallback = updateConversationDraft({}, "project-1", "conversation-1", "旧草稿");
  const nextFallback = updateConversationDraft(staleFallback, "project-1", "conversation-1", "");

  assert.equal(getConversationDraft(nextFallback, "project-1", "conversation-1"), "");
});

test("invalid persisted data is ignored", () => {
  const storage = memoryStorage({
    "auto.conversation-draft.v2:invalid": "not-json",
    "auto.conversation-draft.v2:expired": JSON.stringify({ text: "过期草稿", updatedAt: 0 }),
  });
  assert.deepEqual(readConversationDrafts(storage), {});
  assert.equal(storage.length, 0);
});

test("persisting one draft does not replace drafts from another browser tab", () => {
  const storage = memoryStorage();
  assert.equal(persistConversationDraft("project-1", "conversation-1", "第一份草稿", storage), true);
  assert.equal(persistConversationDraft("project-2", "conversation-2", "第二份草稿", storage), true);

  assert.equal(readConversationDraft("project-1", "conversation-1", storage), "第一份草稿");
  assert.equal(readConversationDraft("project-2", "conversation-2", storage), "第二份草稿");
});

test("legacy drafts migrate when first restored", () => {
  const draftKey = conversationDraftKey("project", "conversation");
  const storage = memoryStorage({ "auto.conversation-drafts.v1": JSON.stringify({ [draftKey]: "旧版草稿" }) });

  assert.equal(readConversationDraft("project", "conversation", storage), "旧版草稿");
  assert.equal(storage.getItem("auto.conversation-drafts.v1"), null);
  assert.equal(readConversationDraft("project", "conversation", storage), "旧版草稿");
});

test("stored drafts retain only the most recent fifty conversations", () => {
  const storage = memoryStorage();
  for (let index = 0; index < 51; index++) {
    persistConversationDraft("project", `conversation-${index}`, `草稿 ${index}`, storage);
  }

  assert.equal(readConversationDraft("project", "conversation-0", storage), null);
  assert.equal(readConversationDraft("project", "conversation-50", storage), "草稿 50");
  assert.equal(Object.keys(readConversationDrafts(storage)).length, 50);
});
