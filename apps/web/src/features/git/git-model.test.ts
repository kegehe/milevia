import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

import { groupChanges } from "./git-model.ts";
import { parseDiffContent } from "./GitWorkbench.tsx";

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
