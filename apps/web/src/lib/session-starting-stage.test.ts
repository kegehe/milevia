// 「正在恢复会话上下文」这条阶段的生命周期：CLI 一开口就必须结束。
// 少了收尾，"正在恢复"会一直挂到本轮结束 —— 一边流式输出、状态条还在说在恢复。
import { test } from "node:test";
import assert from "node:assert/strict";
import { latestActiveStage } from "./run-indicator";
import type { Event } from "./types";

function event(type: string, createdAt: string, payload: Record<string, unknown> = {}): Event {
  return { id: createdAt + type, type, payload, runId: "run-1", createdAt };
}

test("启动阶段：CLI 还没开口时显示", () => {
  const events: Event[] = [
    event("system", "2026-10-26T09:31:19.000Z", { subtype: "session_starting", resume: true, historyMessages: 3745 }),
  ];
  const stage = latestActiveStage(events, "run-1");
  assert.equal(stage?.title, "正在恢复会话上下文");
});

test("启动阶段：init 到达后立刻结束", () => {
  const events: Event[] = [
    event("system", "2026-10-26T09:31:19.000Z", { subtype: "session_starting", resume: true }),
    event("system", "2026-10-26T09:32:20.000Z", { subtype: "init" }),
  ];
  assert.equal(latestActiveStage(events, "run-1"), null);
});

test("启动阶段：别的会话的事件不能把它关掉", () => {
  const events: Event[] = [
    event("system", "2026-10-26T09:31:19.000Z", { subtype: "session_starting", resume: true }),
    { id: "other", type: "assistant", payload: {}, runId: "run-2", createdAt: "2026-10-26T09:31:30.000Z" },
  ];
  assert.equal(latestActiveStage(events, "run-1")?.title, "正在恢复会话上下文");
});
