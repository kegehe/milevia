import assert from "node:assert/strict";
import test from "node:test";
import { interactiveNotice, retainAskQuestions, scheduledSendReady, selectableProfiles, type MobileNotice } from "./mobile-session-controls";

test("提问事件收成可作答的通知，终态合并时留下题目", () => {
  const pending = interactiveNotice("ask.pending", {
    askId: "ask-1",
    questions: [{ id: "q1", header: "数据库", question: "选哪个？", options: [{ label: "PostgreSQL" }, { label: "SQLite" }] }],
  }, "e1", "run-1", "2026-10-29T01:00:00Z");
  assert.equal(pending?.ask?.status, "pending");
  assert.equal(pending?.ask?.questions.length, 1);
  const answered = interactiveNotice("ask.answered", { askId: "ask-1", answers: { q1: "SQLite" } }, "e2", "run-1", "2026-10-29T01:01:00Z");
  assert.equal(answered?.ask?.questions.length, 0);
  const kept = retainAskQuestions(answered as MobileNotice, pending as MobileNotice);
  assert.equal(kept.ask?.questions[0]?.question, "选哪个？");
  assert.equal(kept.ask?.status, "answered");
});

test("工具确认带上 approvalId，手机可以允许或拒绝", () => {
  const notice = interactiveNotice("approval.pending", {
    approvalId: "ap-1",
    status: "pending",
    toolName: "Bash",
    toolInput: { command: "pnpm test" },
  }, "e1", "run-1", "2026-10-29T01:00:00Z");
  assert.equal(notice?.approvalId, "ap-1");
  assert.equal(notice?.approvalDecision, "pending");
  assert.match(notice?.detail || "", /pnpm test/);
});

test("配置档案只留下这个工具上、CLI 自己持有登录态的启用项", () => {
  const choices = selectableProfiles([
    { id: "a", agentId: "codex", name: "工作", enabled: true, isDefault: true, authMode: "cli_managed", state: "active" },
    { id: "b", agentId: "codex", name: "密钥", enabled: true, isDefault: false, authMode: "api_key", state: "active" },
    { id: "c", agentId: "claude-code", name: "Claude", enabled: true, isDefault: false, authMode: "cli_managed", state: "active" },
    { id: "d", agentId: "codex", name: "停用", enabled: false, isDefault: false, authMode: "cli_managed", state: "active" },
  ], "codex");
  assert.deepEqual(choices.map((item) => item.id), ["a"]);
});

test("预约发送在这一轮结束后才放行", () => {
  assert.equal(scheduledSendReady("running", true, false), false);
  assert.equal(scheduledSendReady("idle", true, false), false);
  assert.equal(scheduledSendReady("idle", false, true), false);
  assert.equal(scheduledSendReady("idle", false, false), true);
});
