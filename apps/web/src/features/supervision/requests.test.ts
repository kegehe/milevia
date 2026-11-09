import assert from "node:assert/strict";
import test from "node:test";
import { prepareSupervisionWrite } from "./requests";

function storage() {
  const entries = new Map<string, string>();
  return { getItem: (key: string) => entries.get(key) ?? null, setItem: (key: string, value: string) => { entries.set(key, value); }, removeItem: (key: string) => { entries.delete(key); } };
}

test("托管写入响应丢失后按原始版本和请求标识重试", () => {
  const store = storage();
  const original = prepareSupervisionWrite(store, "/api/supervision/s/instructions", "POST", { stateVersion: 1, content: "保留改动" });
  const retry = prepareSupervisionWrite(store, "/api/supervision/s/instructions", "POST", { stateVersion: 4, content: "保留改动" });
  assert.deepEqual(retry, original);
  assert.equal(JSON.parse(retry.body).stateVersion, 1);
  const changed = prepareSupervisionWrite(store, "/api/supervision/s/instructions", "POST", { stateVersion: 4, content: "补充新要求" });
  assert.notEqual(changed.key, original.key);
});

test("托管请求标识按会话、项目和操作隔离", () => {
  const store = storage(); const body = { stateVersion: 1, action: "pause" };
  const first = prepareSupervisionWrite(store, "/api/supervision/s/control", "POST", body);
  const second = prepareSupervisionWrite(store, "/api/supervision/other/control", "POST", body);
  assert.notEqual(first.key, second.key);
  assert.deepEqual(prepareSupervisionWrite(store, "/api/supervision/s/control", "POST", body), first);
});
