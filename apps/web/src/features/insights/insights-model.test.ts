import assert from "node:assert/strict";
import test from "node:test";

import {
  buildInsightPrompt,
  filterFindingsByType,
  insightFindingCounts,
  insightSeverityLabels,
  insightThemeLabels,
  insightThemes,
  insightTypeLabels,
  normalizeInsightSeverity,
  normalizeInsightTheme,
  normalizeInsightType,
  sortFindings,
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

test("buildInsightPrompt renders user-facing prompt with file hint", () => {
  const finding = makeFinding("f1", {
    type: "bug",
    severity: "high",
    title: "列表加载很慢",
    summary: "列表页打开要等好几秒",
    fileHint: "src/List.tsx",
  });
  const prompt = buildInsightPrompt(finding);
  assert.ok(prompt.includes("【优化建议】列表加载很慢"));
  assert.ok(prompt.includes("类型：有 bug"));
  assert.ok(prompt.includes("严重度：高"));
  assert.ok(prompt.includes("说明：列表页打开要等好几秒"));
  assert.ok(prompt.includes("相关位置：src/List.tsx"));
  assert.ok(prompt.includes("请据此排查并修复"));
});

test("buildInsightPrompt omits file hint line when absent", () => {
  const finding = makeFinding("f2", {
    type: "feature",
    severity: "normal",
    title: "可加导出按钮",
    summary: "在列表页加一个导出功能",
    fileHint: "",
  });
  const prompt = buildInsightPrompt(finding);
  assert.ok(!prompt.includes("相关位置"));
  assert.ok(prompt.includes("【优化建议】可加导出按钮"));
  assert.ok(prompt.includes("类型：可新增功能"));
});

test("buildInsightPrompt normalizes invalid type/severity", () => {
  const finding = makeFinding("f3", { type: "weird" as unknown as InsightType, severity: "extreme", title: "T", summary: "S" });
  const prompt = buildInsightPrompt(finding);
  // 非法 type → optimization，非法 severity → normal
  assert.ok(prompt.includes("类型：可优化项"));
  assert.ok(prompt.includes("严重度：普通"));
});
