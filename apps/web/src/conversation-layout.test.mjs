import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const stylesheet = await readFile(new URL("./conversation.css", import.meta.url), "utf8");

test("desktop canvas preserves the rail width while using a small left inset", () => {
  assert.match(stylesheet, /\.conversation-canvas\s*\{[^}]*grid-template-columns:\s*300px\s+minmax\(0,\s*1fr\)[^}]*margin:\s*0\s+0\s+0\s+24px;/s);
  assert.doesNotMatch(stylesheet, /@media \(max-width: 1080px\)\s*\{\s*\.conversation-canvas\s*\{[^}]*grid-template-columns:\s*250px/s);
});

test("wide screens place shortcut groups side by side while their items remain vertical", () => {
  assert.match(stylesheet, /\.quick-actions-row\s*\{[^}]*grid-template-columns:\s*repeat\(2,\s*minmax\(0,\s*1fr\)\);/s);
  assert.match(stylesheet, /@media \(max-width: 820px\)[\s\S]*?\.quick-actions-row\s*\{[^}]*grid-template-columns:\s*1fr;/);
  assert.match(stylesheet, /\.quick-actions-row \.quick-tag-list\s*\{[^}]*display:\s*grid;[^}]*overflow-x:\s*visible;/s);
  assert.doesNotMatch(stylesheet, /\.quick-actions-row\s*\{[^}]*overflow-x:\s*auto/s);
});
