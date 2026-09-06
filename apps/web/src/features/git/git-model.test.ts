import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { createElement } from "react";
import { renderToString } from "react-dom/server";

import { groupChanges, type GitOperation, type GitSnapshot } from "./git-model.ts";
import { GitBar, GitWorkbench, parseDiffContent } from "./GitWorkbench.tsx";

test("keeps staged and worktree entries for the same path distinct", () => {
  const grouped = groupChanges([
    { path: "app.go", staged: true, modified: true, untracked: false, deleted: false, renamed: false, conflicted: false },
    { path: "new.txt", staged: false, modified: false, untracked: true, deleted: false, renamed: false, conflicted: false },
  ]);

  assert.deepEqual(grouped.staged.map((change) => change.path), ["app.go"]);
  assert.deepEqual(grouped.worktree.map((change) => change.path), ["app.go", "new.txt"]);
});

test("renders unified diffs with aligned old and new line numbers", () => {
  const lines = parseDiffContent("diff --git a/readme.md b/readme.md\n@@ -4,2 +4,2 @@\n old\n-old value\n+new value\n tail\n");

  assert.deepEqual(lines.map(({ kind, oldLine, newLine, content }) => ({ kind, oldLine, newLine, content })), [
    { kind: "meta", oldLine: undefined, newLine: undefined, content: "diff --git a/readme.md b/readme.md" },
    { kind: "hunk", oldLine: undefined, newLine: undefined, content: "@@ -4,2 +4,2 @@" },
    { kind: "context", oldLine: 4, newLine: 4, content: "old" },
    { kind: "removed", oldLine: 5, newLine: undefined, content: "old value" },
    { kind: "added", oldLine: undefined, newLine: 5, content: "new value" },
    { kind: "context", oldLine: 6, newLine: 6, content: "tail" },
  ]);
});

test("renders untracked file contents as numbered code lines", () => {
  const lines = parseDiffContent("first line\nsecond line\n");

  assert.deepEqual(lines.map(({ oldLine, newLine, content }) => ({ oldLine, newLine, content })), [
    { oldLine: 1, newLine: 1, content: "first line" },
    { oldLine: 2, newLine: 2, content: "second line" },
  ]);
});

test("closes a diff inside the workbench without navigating away from the project", () => {
  const source = readFileSync(new URL("./GitWorkbench.tsx", import.meta.url), "utf8");

  assert.doesNotMatch(source, /window\.history\.back\(\)/);
  assert.match(source, /const closeDiff = \(\) => \{ diffRequest\.current\+\+; setSelectedDiff\(null\); \};/);
});

test("keeps a late diff response from replacing the most recently selected file", () => {
  const source = readFileSync(new URL("./GitWorkbench.tsx", import.meta.url), "utf8");

  assert.match(source, /const diffRequest = useRef\(0\);/);
  assert.match(source, /const requestID = \+\+diffRequest\.current;/);
  assert.match(source, /if \(requestID === diffRequest\.current && mountedRef\.current\) setSelectedDiff\(diff\);/);
});

test("closes destructive confirmations when an operation result is uncertain", () => {
  const source = readFileSync(new URL("./GitWorkbench.tsx", import.meta.url), "utf8");

  assert.match(source, /if \(result\.status === "needs_attention"\) \{\s+closeDiff\(\);\s+setConfirmation\(null\);\s+\}/);
});

test("reports an unavailable Git state instead of silently ignoring a mutation", () => {
  const source = readFileSync(new URL("./GitWorkbench.tsx", import.meta.url), "utf8");

  assert.match(source, /if \(!snapshot\?\.stateToken\) \{\s+fail\("Git 状态尚未准备完成，请刷新后重试"\);\s+return;\s+\}/);
  assert.match(source, /void reload\(true\)\.catch\(\(\) => undefined\);/);
});

test("keeps the workbench a compact 2-tab layout with a persistent status bar", () => {
  const source = readFileSync(new URL("./GitWorkbench.tsx", import.meta.url), "utf8");

  assert.match(source, /type Tab = "changes" \| "branches";/);
  assert.doesNotMatch(source, /"overview"|"operations"/);
  assert.match(source, /function GitBar\(/);
  assert.match(source, /function Branches\(/);
});

test("closes the operations popover via ref.contains outside-click instead of a blocking backdrop", () => {
  const source = readFileSync(new URL("./GitWorkbench.tsx", import.meta.url), "utf8");

  assert.match(source, /barRef\.current\?\.contains\(target\)/);
  assert.match(source, /if \(event\.key === "Escape"\) setOpsOpen\(false\);/);
  assert.doesNotMatch(source, /git-ops-backdrop/);
});

const noop = () => undefined;

function sampleSnapshot(overrides: Partial<GitSnapshot["head"]> = {}): GitSnapshot {
  return {
    repositoryState: "ready",
    head: { oid: "abc123def456", branch: "feature/hello", detached: false, upstream: "origin/feature/hello", ahead: 2, behind: 1, ...overrides },
    worktree: { staged: 1, modified: 2, untracked: 3, deleted: 0, renamed: 0, conflicted: 0 },
  };
}

test("ssr smoke: workbench initial render executes without throwing", () => {
  const html = renderToString(createElement(GitWorkbench, {
    projectID: "p1",
    request: async () => ({}) as never,
    fail: () => undefined,
    active: true,
  }));

  assert.match(html, /git-workbench/);
  assert.match(html, /正在读取仓库状态/);
});

test("ssr smoke: GitBar loaded state renders branch, sync badge and actions", () => {
  const html = renderToString(createElement(GitBar, {
    snapshot: sampleSnapshot(),
    loading: false,
    mutating: "",
    refreshing: false,
    operations: [],
    requestRefresh: noop,
    requestFetch: noop,
    requestPull: noop,
    requestPush: noop,
  }));

  assert.match(html, /feature\/hello/);
  assert.match(html, /领先 2 · 落后 1/);
  assert.match(html, /abc123de/); // 短 OID（前 8 位）
  assert.match(html, />拉取</);
  assert.match(html, />推送</);
  assert.doesNotMatch(html, /git-ops-popover-body/); // 关闭时不渲染 popover 本体
});

test("ssr smoke: GitBar shows muted badge when no upstream and red dot when failed ops exist", () => {
  const noUpstream = renderToString(createElement(GitBar, {
    snapshot: sampleSnapshot({ upstream: "", ahead: 0, behind: 0 }),
    loading: false,
    mutating: "",
    refreshing: false,
    operations: [],
    requestRefresh: noop,
    requestFetch: noop,
    requestPull: noop,
    requestPush: noop,
  }));
  assert.match(noUpstream, /未跟踪上游/);
  assert.match(noUpstream, /git-sync-badge muted/);

  const ops: GitOperation[] = [{
    id: "op-1", projectId: "p1", type: "push", status: "failed", requestSummary: "push main",
    beforeState: "{}", afterState: "{}", errorMessage: "rejected", requestedAt: "2026-09-01T00:00:00Z",
  }];
  const withDot = renderToString(createElement(GitBar, {
    snapshot: sampleSnapshot(),
    loading: false,
    mutating: "",
    refreshing: false,
    operations: ops,
    requestRefresh: noop,
    requestFetch: noop,
    requestPull: noop,
    requestPush: noop,
  }));
  assert.match(withDot, /git-ops-trigger attention/);
  assert.match(withDot, /git-ops-dot/);
});
