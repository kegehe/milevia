import assert from "node:assert/strict";
import test from "node:test";
import { persistTaskQuickDraft, readTaskQuickDraft } from "./task-quick-draft.ts";

function memoryStorage() {
  const values = new Map<string, string>();
  return {
    getItem: (key: string) => values.get(key) ?? null,
    setItem: (key: string, value: string) => { values.set(key, value); },
    removeItem: (key: string) => { values.delete(key); },
  };
}

test("task quick drafts are isolated by project and can be cleared", () => {
  const storage = memoryStorage();
  persistTaskQuickDraft("project/a", "待确认的任务", storage);
  persistTaskQuickDraft("project/b", "另一份任务", storage);
  assert.equal(readTaskQuickDraft("project/a", storage), "待确认的任务");
  assert.equal(readTaskQuickDraft("project/b", storage), "另一份任务");
  persistTaskQuickDraft("project/a", "", storage);
  assert.equal(readTaskQuickDraft("project/a", storage), "");
  assert.equal(readTaskQuickDraft("project/b", storage), "另一份任务");
});
