import assert from "node:assert/strict";
import test from "node:test";

import {
  filterFindingsByType,
  insightFindingCounts,
  insightSeverityLabels,
  insightThemeLabels,
  insightThemes,
  insightTypeLabels,
  insightLinkedTaskLabel,
  insightVerificationLabels,
  isInsightDismissed,
  maxInsightEventSeq,
  normalizeInsightSeverity,
  normalizeInsightTheme,
  normalizeInsightType,
  normalizeInsightVerification,
  sortFindings,
  toggleInsightType,
  type InsightFinding,
  type InsightType,
} from "./insights-model.ts";

const makeFinding = (id: string, overrides: Partial<InsightFinding> = {}): InsightFinding => ({
  id,
  projectId: "p1",
  scanId: "s1",
  type: "optimization",
  severity: "normal",
  title: `Title ${id}`,
  summary: "Summary text",
  status: "open",
  createdAt: "2026-08-11T00:00:00Z",
  ...overrides,
});

test("sorts findings by type then severity", () => {
  const f1 = makeFinding("high-bug", { type: "bug", severity: "high" });
  const f2 = makeFinding("low-feature", { type: "feature", severity: "low" });
  const f3 = makeFinding("normal-style", { type: "style", severity: "normal" });
  // 乱序输入：feature 应排在 bug 之后。
  const sorted = sortFindings([f3, f2, f1]);
  assert.deepEqual(
    sorted.map((f) => f.id),
    ["high-bug", "normal-style", "low-feature"],
  );
});

test("sorts findings by severity within same type", () => {
  const low = makeFinding("low", { type: "bug", severity: "low" });
  const high = makeFinding("high", { type: "bug", severity: "high" });
  const normal = makeFinding("normal", { type: "bug", severity: "normal" });
  assert.deepEqual(
    sortFindings([normal, low, high]).map((f) => f.id),
    ["high", "normal", "low"],
  );
});

test("filters findings by type", () => {
  const bug = makeFinding("b", { type: "bug" });
  const feature = makeFinding("f", { type: "feature" });
  const all = filterFindingsByType([bug, feature], "all");
  const bugs = filterFindingsByType([bug, feature], "bug");
  assert.equal(all.length, 2);
  assert.deepEqual(bugs.map((f) => f.id), ["b"]);
});

test("unknown type filter yields empty", () => {
  const bug = makeFinding("b", { type: "bug" });
  assert.deepEqual(filterFindingsByType([bug], "feature" as InsightType), []);
});

test("counts findings by type", () => {
  const findings = [
    makeFinding("1", { type: "bug" }),
    makeFinding("2", { type: "bug" }),
    makeFinding("3", { type: "feature" }),
  ];
  assert.deepEqual(insightFindingCounts(findings), { all: 3, bug: 2, style: 0, optimization: 0, feature: 1 });
});

test("normalizes invalid type and severity", () => {
  assert.equal(normalizeInsightType("weird"), "optimization");
  assert.equal(normalizeInsightType("bug"), "bug");
  assert.equal(normalizeInsightSeverity("extreme"), "normal");
  assert.equal(normalizeInsightSeverity("high"), "high");
});

test("labels map to Chinese user-facing text", () => {
  assert.equal(insightTypeLabels.bug, "有 bug");
  assert.equal(insightTypeLabels.feature, "可新增功能");
  assert.equal(insightSeverityLabels.high, "高");
});

test("normalizes theme to a known value, unknown → empty (全面分析)", () => {
  assert.equal(normalizeInsightTheme("security"), "security");
  assert.equal(normalizeInsightTheme("stability"), "stability");
  assert.equal(normalizeInsightTheme("bogus"), "");
  assert.equal(normalizeInsightTheme(""), "");
  assert.equal(normalizeInsightTheme(undefined), "");
});

test("theme labels render Chinese for every known theme", () => {
  assert.equal(insightThemeLabels.security, "安全");
  assert.equal(insightThemeLabels.performance, "性能");
  assert.equal(insightThemeLabels.ux, "UX");
  assert.equal(insightThemeLabels.architecture, "架构");
  assert.equal(insightThemeLabels.stability, "稳定性");
  assert.equal(insightThemeLabels[""], "全面分析");
  // 列表不含空串（空串在 UI 单独当作"全部"）。
  assert.ok(!insightThemes.includes(""));
  assert.equal(insightThemes.length, 5);
});

test("normalizes verification result to a known value, unknown → 未验证", () => {
  assert.equal(normalizeInsightVerification("pending"), "pending");
  assert.equal(normalizeInsightVerification("valid"), "valid");
  assert.equal(normalizeInsightVerification("invalid"), "invalid");
  assert.equal(normalizeInsightVerification("failed"), "failed");
  assert.equal(normalizeInsightVerification("bogus"), "");
  assert.equal(normalizeInsightVerification(""), "");
  assert.equal(normalizeInsightVerification(undefined), "");
});

test("verification result labels render Chinese for every known state", () => {
  assert.equal(insightVerificationLabels[""], "未验证");
  assert.equal(insightVerificationLabels.pending, "验证中…");
  assert.equal(insightVerificationLabels.valid, "已复核 · 仍存在");
  assert.equal(insightVerificationLabels.invalid, "已失效");
  assert.equal(insightVerificationLabels.failed, "验证失败");
});

test("finding carries verification fields through sorting", () => {
  const verified = makeFinding("v", { severity: "high", verificationResult: "valid", verificationNote: "src/List.tsx 仍未做分页", verifiedAt: "2026-08-14T10:00:00Z" });
  const stale = makeFinding("s", { severity: "normal", verificationResult: "invalid" });
  const failed = makeFinding("f", { severity: "low", verificationResult: "failed" });
  const sorted = sortFindings([failed, stale, verified]);
  assert.deepEqual(
    sorted.map((f) => f.id),
    ["v", "s", "f"],
  );
  assert.equal(sorted[0].verificationNote, "src/List.tsx 仍未做分页");
  assert.equal(normalizeInsightVerification(sorted[1].verificationResult), "invalid");
});

test("toggleInsightType keeps at least one type selected", () => {
  // 取消到只剩一项时，再取消应保持原样：空数组在后端等价于"全查"，与用户意图相反。
  assert.deepEqual(toggleInsightType(["bug"], "bug"), ["bug"]);
  assert.deepEqual(toggleInsightType(["bug", "style"], "bug"), ["style"]);
  assert.deepEqual(toggleInsightType(["bug"], "feature"), ["bug", "feature"]);
});

test("maxInsightEventSeq returns the largest seq", () => {
  assert.equal(maxInsightEventSeq([]), 0);
  assert.equal(maxInsightEventSeq([
    { id: "a", seq: 3, ts: "t", level: "info", message: "m" },
    { id: "b", seq: 11, ts: "t", level: "info", message: "m" },
    { id: "c", seq: 7, ts: "t", level: "info", message: "m" },
  ]), 11);
});

test("insightLinkedTaskLabel describes the linked task state", () => {
  assert.equal(insightLinkedTaskLabel(makeFinding("a")), null);
  assert.equal(insightLinkedTaskLabel(makeFinding("a", { linkedTaskStatus: "todo" })), "已转为任务 · 待办");
  assert.equal(insightLinkedTaskLabel(makeFinding("a", { linkedTaskStatus: "running" })), "已转为任务 · 进行中");
  // 终态后可重新添加（后端不再拦截）。
  assert.equal(insightLinkedTaskLabel(makeFinding("a", { linkedTaskStatus: "done" })), "已转为任务 · 已完成（可重新添加）");
  assert.equal(insightLinkedTaskLabel(makeFinding("a", { linkedTaskStatus: "cancelled" })), "已转为任务 · 已取消（可重新添加）");
});

test("isInsightDismissed only matches the dismissed status", () => {
  assert.equal(isInsightDismissed(makeFinding("a")), false);
  assert.equal(isInsightDismissed(makeFinding("a", { status: "open" })), false);
  assert.equal(isInsightDismissed(makeFinding("a", { status: "dismissed" })), true);
});
