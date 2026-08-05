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

test("unwraps Codex shell wrapper and labels command execution as terminal command", () => {
  const timeline = buildTimeline([], [
    event("cmd-start", "item.started", { item: { id: "item_1", type: "command_execution", command: "/usr/bin/zsh -lc 'cat hello.txt'", aggregated_output: "", exit_code: null, status: "in_progress" } }),
    event("cmd-done", "item.completed", { item: { id: "item_1", type: "command_execution", command: "/usr/bin/zsh -lc 'cat hello.txt'", aggregated_output: "hello\n", exit_code: 0, status: "completed" } }),
  ]);
  const tools = timeline.filter((item) => item.kind === "tool");
  assert.equal(tools.length, 1);
  const action = (tools[0] as any).action;
  assert.equal(action.name, "终端命令");
  assert.equal(action.input.command, "cat hello.txt");
  assert.equal(action.output?.content, "hello\n");
});

test("formats Codex file_change with readable Chinese labels", () => {
  const timeline = buildTimeline([], [
    event("file-done", "item.completed", { item: { id: "item_2", type: "file_change", changes: [{ path: "/tmp/proj/hello.txt", kind: "add" }, { path: "/tmp/proj/foo.py", kind: "modify" }], status: "completed" } }),
  ]);
  const tools = timeline.filter((item) => item.kind === "tool");
  assert.equal(tools.length, 1);
  const action = (tools[0] as any).action;
  assert.equal(action.name, "文件修改");
  assert.ok(action.input.description.includes("新增"), `description should contain 新增: ${action.input.description}`);
  assert.ok(action.input.description.includes("修改"), `description should contain 修改: ${action.input.description}`);
  assert.ok(action.output?.content.includes("新增"), `output should contain 新增: ${action.output?.content}`);
});
