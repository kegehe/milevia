import assert from "node:assert/strict";
import test from "node:test";

import { buildTimeline } from "./timeline.ts";
import type { Event } from "./types.ts";

const at = "2026-07-30T11:29:00.000Z";

function event(id: string, type: string, payload: unknown): Event {
  return { id, type, payload, runId: "run-1", createdAt: at };
}

test("localizes 'No completion record' background-task notices in Chinese", () => {
  const timeline = buildTimeline([], [
    event("sys", "system", { type: "system", subtype: "task_notification", summary: 'No completion record was found for background agent "审查News/订阅/日报页面" from the previous session. It may have been stopped, or it may have been running when the previous Claude Code process exited — either way its transcript is saved on disk, so its progress is not lost.' }),
  ]);
  const system = timeline.find((item): item is { kind: "system"; system: { variant: string; title: string; detail?: string } } => item.kind === "system");
  assert.ok(system, "expected a system item");
  assert.equal(system.system.title, "后台代理未找到完成记录");
  assert.equal(system.system.detail, "「审查News/订阅/日报页面」可能仍在运行，或其进程已在会话退出后终止。");
});

test("keeps template detail even when taskStatus already decides the title", () => {
  // 当 payload.status 标记失败、且 summary 是英文 Background command 模板时，
  // 标题由状态决定，但 detail 必须仍保留退出码与命令名（不可丢失）。
  const timeline = buildTimeline([], [
    event("sys", "system", { type: "system", subtype: "task_notification", status: "failed", summary: 'Background command "npm run build" failed (exit code 1)' }),
  ]);
  const system = timeline.find((item) => item.kind === "system") as { kind: "system"; system: { title: string; detail?: string } } | undefined;
  assert.ok(system, "expected a system item");
  assert.equal(system.system.title, "后台任务失败");
  assert.equal(system.system.detail, "失败（退出码 1）npm run build");
});

test("recordMatch overrides a contradictory failed taskStatus (extreme combo)", () => {
  // 极端防御：CLI 同时发 status=failed 又发 No-completion-record 摘要时，
  // 后者是更具体的中性特征，应主导标题与状态，避免「失败标题 + 中性正文」的自相矛盾。
  const timeline = buildTimeline([], [
    event("sys", "system", { type: "system", subtype: "task_notification", status: "failed", summary: 'No completion record was found for background agent "X" from the previous session.' }),
  ]);
  const system = timeline.find((item) => item.kind === "system") as { kind: "system"; system: { title: string; detail?: string; metadata?: { state?: string } } } | undefined;
  assert.ok(system, "expected a system item");
  assert.equal(system.system.title, "后台代理未找到完成记录");
  assert.equal(system.system.detail, "「X」可能仍在运行，或其进程已在会话退出后终止。");
  assert.equal(system.system.metadata?.state, "info");
});

test("keeps unrecognized background-task summaries verbatim (graceful fallback)", () => {
  const timeline = buildTimeline([], [
    event("sys", "system", { type: "system", subtype: "task_notification", summary: "Some brand new CLI wording that changes constantly" }),
  ]);
  const system = timeline.find((item) => item.kind === "system") as { kind: "system"; system: { title: string; detail?: string } } | undefined;
  assert.ok(system, "expected a system item");
  assert.equal(system.system.title, "后台任务通知");
  assert.equal(system.system.detail, "Some brand new CLI wording that changes constantly");
});

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
