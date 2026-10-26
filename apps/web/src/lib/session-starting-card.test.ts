// 「正在恢复会话上下文」这张卡片的可见窗口：只在那段"CLI 还没开口"的空白里出现。
// 卡片一旦留下，回看时就是一句永远为真的假话；所以三条边界都要钉住。
import { test } from "node:test";
import assert from "node:assert/strict";
import { buildTimeline } from "./timeline";
import type { Event } from "./types";

function event(type: string, createdAt: string, payload: Record<string, unknown> = {}, runId = "run-1"): Event {
  return { id: createdAt + type, type, payload, runId, createdAt };
}

const starting = event("system", "2026-10-26T09:31:19.000Z", { subtype: "session_starting", resume: true, historyMessages: 3745 });
const sessionCards = (events: Event[]) => buildTimeline([], events).filter((item) => item.kind === "system" && item.system.variant === "session");

test("启动阶段：还没开口时有一张卡", () => {
  assert.equal(sessionCards([starting]).length, 1);
});

test("启动阶段：init 到达后卡片消失", () => {
  const cards = sessionCards([starting, event("system", "2026-10-26T09:32:20.000Z", { subtype: "init" })]);
  assert.equal(cards.length, 0);
});

test("启动阶段：本轮结束后卡片消失（即使没收到 init）", () => {
  const cards = sessionCards([starting, event("run.completed", "2026-10-26T09:33:00.000Z", { status: "completed" })]);
  assert.equal(cards.length, 0);
});

test("启动阶段：别的会话的后续事件不影响它", () => {
  const cards = sessionCards([starting, event("assistant", "2026-10-26T09:31:30.000Z", {}, "run-2")]);
  assert.equal(cards.length, 1);
});
