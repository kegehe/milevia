import assert from "node:assert/strict";
import test from "node:test";

import { buildTimeline } from "./timeline.ts";
import type { Event } from "./types.ts";

const at = "2026-07-30T11:29:00.000Z";

function event(id: string, type: string, payload: unknown): Event {
  return { id, type, payload, runId: "run-1", createdAt: at };
}

test("appends original English errors after the Chinese fallback", () => {
  const timeline = buildTimeline([], [
    event("detail", "error", { message: "Authentication expired. Run codex login." }),
    event("terminal", "run.failed", { error: "Codex exited: exit status 1" }),
  ]);

  const errors = timeline.filter((item) => item.kind === "error");
  assert.deepEqual(errors.map((item) => [item.title, item.detail]), [["执行错误", "任务执行失败，请查看任务日志后重试。：Authentication expired. Run codex login."]]);
});

test("appends original English turn failure details after the Chinese fallback", () => {
  const detailed = buildTimeline([], [event("turn", "turn.failed", { error: { message: "Request rejected by the service" } })]);
  assert.deepEqual(detailed.filter((item) => item.kind === "error").map((item) => item.detail), ["任务执行失败，请查看任务日志后重试。：Request rejected by the service"]);

  const fallback = buildTimeline([], [event("terminal", "run.failed", { error: "Codex exited: exit status 1" })]);
  assert.deepEqual(fallback.filter((item) => item.kind === "error").map((item) => item.detail), ["任务执行失败，请查看任务日志后重试。：Codex exited: exit status 1"]);
});
