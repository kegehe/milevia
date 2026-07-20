import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

import { groupChanges } from "./git-model.ts";

test("keeps staged and worktree entries for the same path distinct", () => {
  const grouped = groupChanges([
    { path: "app.go", staged: true, modified: true, untracked: false, deleted: false, renamed: false, conflicted: false },
    { path: "new.txt", staged: false, modified: false, untracked: true, deleted: false, renamed: false, conflicted: false },
  ]);

  assert.deepEqual(grouped.staged.map((change) => change.path), ["app.go"]);
  assert.deepEqual(grouped.worktree.map((change) => change.path), ["app.go", "new.txt"]);
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
  assert.match(source, /if \(requestID === diffRequest\.current\) setSelectedDiff\(diff\);/);
});
