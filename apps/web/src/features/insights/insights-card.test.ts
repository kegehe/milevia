import assert from "node:assert/strict";
import test from "node:test";
import { registerHooks } from "node:module";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import type { InsightFinding } from "./insights-model";

// Node renders the real component; asset imports use the same URL-string shape
// as Vite without loading image files as JavaScript.
const assetHooks = registerHooks({
  load(url, context, nextLoad) {
    if (/\.(svg|png|webp)$/.test(url)) return { format: "module", source: `export default ${JSON.stringify(url)};`, shortCircuit: true };
    return nextLoad(url, context);
  },
});
const { InsightsFindingCard } = await import("./InsightsPanel");
assetHooks.deregister();

const render = (overrides: Partial<InsightFinding> = {}) => renderToStaticMarkup(createElement(InsightsFindingCard, {
  finding: {
    id: "finding", projectId: "project", scanId: "scan", type: "optimization", severity: "normal",
    title: "完善启动说明", summary: "帮助首次使用", status: "open", createdAt: "2026-11-11T00:00:00Z", ...overrides,
  },
  projectID: "project",
  request: async <T,>(): Promise<T> => { throw new Error("render must not send requests"); },
  onDeleted: () => {}, onDismissed: () => {}, fail: () => {},
}));

test("suggestion evidence and acceptance render as escaped, collapsible text", () => {
  const html = render({ evidence: "README <script>alert(1)</script>\n待运行验证", acceptanceCriteria: "按步骤启动成功" });
  assert.match(html, /<details class="insight-details"><summary>依据与验收要点<\/summary>/);
  assert.match(html, /<dt>分析依据<\/dt>/);
  assert.match(html, /&lt;script&gt;alert\(1\)&lt;\/script&gt;/);
  assert.match(html, /待运行验证/);
  assert.match(html, /<dt>验收要点<\/dt><dd>按步骤启动成功<\/dd>/);
  assert.doesNotMatch(html, /<details[^>]* open/);
  assert.doesNotMatch(html, /<script>/);
});

test("older suggestions with no details retain their normal actions", () => {
  const html = render();
  assert.doesNotMatch(html, /<details/);
  assert.match(html, /添加到任务/);
  assert.match(html, /完善启动说明/);
});

test("linked active tasks disable conversion while terminal tasks allow it", () => {
  const active = render({ linkedTaskStatus: "running", linkedTaskTitle: "完善说明" });
  assert.match(active, /disabled="" title="已转为任务 · 进行中"/);
  for (const state of ["done", "cancelled"]) {
    const terminal = render({ linkedTaskStatus: state });
    assert.match(terminal, /class="insight-action" title="添加到任务，通过任务下发执行（复核中暂不可转）"/);
  }
});
