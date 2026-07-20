import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const stylesheet = await readFile(new URL("./conversation.css", import.meta.url), "utf8");

test("desktop canvas preserves the rail width while using a small left inset", () => {
  assert.match(stylesheet, /\.conversation-canvas\s*\{[^}]*grid-template-columns:\s*300px\s+minmax\(0,\s*1fr\)[^}]*margin:\s*0\s+0\s+0\s+24px;/s);
  assert.doesNotMatch(stylesheet, /@media \(max-width: 1080px\)\s*\{\s*\.conversation-canvas\s*\{[^}]*grid-template-columns:\s*250px/s);
});

test("wide screens align shortcut groups in shared rows and narrow screens restore group order", () => {
  assert.match(stylesheet, /\.quick-actions-row\s*\{[^}]*grid-template-columns:\s*repeat\(2,\s*minmax\(0,\s*1fr\)\);/s);
  assert.match(stylesheet, /@media \(max-width: 820px\)[\s\S]*?\.quick-actions-row\s*\{[^}]*grid-template-columns:\s*1fr;/);
  assert.match(stylesheet, /\.quick-actions-row \.quick-tag-group,\s*\.quick-actions-row \.quick-tag-list\s*\{[^}]*display:\s*contents;/s);
  assert.match(stylesheet, /\.quick-tag-group:first-child \.quick-tag-heading,[^}]*\.quick-tag-group:first-child \.quick-tag,[^}]*\.quick-tag-group:first-child \.quick-tag-empty\s*\{[^}]*grid-column:\s*1;/s);
  assert.match(stylesheet, /\.command-tags \.quick-tag-heading,[^}]*\.command-tags \.quick-tag,[^}]*\.command-tags \.quick-tag-empty\s*\{[^}]*grid-column:\s*2;/s);
  assert.match(stylesheet, /@media \(max-width: 820px\)[\s\S]*?\.quick-actions-row \.quick-tag-group\s*\{[^}]*display:\s*grid;/);
  assert.doesNotMatch(stylesheet, /\.quick-actions-row\s*\{[^}]*overflow-x:\s*auto/s);
});
