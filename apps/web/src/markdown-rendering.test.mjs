import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import React from "react";
import { renderToStaticMarkup } from "react-dom/server";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";

const stylesheet = await readFile(new URL("./markdown.css", import.meta.url), "utf8");

test("markdown keeps ordinary unordered-list markers while preserving task-list layout", () => {
  const rendered = renderToStaticMarkup(React.createElement(ReactMarkdown, { remarkPlugins: [remarkGfm] }, "- ordinary item\n- [x] done"));
  assert.match(rendered, /<ul class="contains-task-list">/);
  assert.match(rendered, /<li>ordinary item<\/li>/);
  assert.match(rendered, /<li class="task-list-item">[\s\S]*done<\/li>/);
  assert.match(stylesheet, /\.markdown ul \{[^}]*list-style:\s*disc;/);
  assert.match(stylesheet, /\.markdown ol \{[^}]*list-style:\s*decimal;/);
  assert.match(stylesheet, /\.markdown ul\.contains-task-list,\s*\.markdown li\.task-list-item \{[^}]*list-style:\s*none;/s);
  assert.match(stylesheet, /\.markdown ul\.contains-task-list \{[^}]*padding-left:\s*0;/s);
  assert.match(stylesheet, /\.markdown ul\.contains-task-list\s*>\s*li:not\(\.task-list-item\)\s*\{[^}]*margin-left:\s*22px;[^}]*list-style:\s*disc;/s);
  assert.doesNotMatch(stylesheet, /\.markdown ul li::marker\s*\{[^}]*content:\s*["']?["'];/);
});

test("markdown blocks stay inside mobile message bubbles and scroll horizontally when needed", () => {
  assert.match(stylesheet, /\.markdown pre \{[^}]*width:\s*100%;[^}]*overflow-x:\s*auto;[^}]*max-width:\s*100%;/s);
  assert.match(stylesheet, /\.markdown table \{[^}]*width:\s*max-content;[^}]*min-width:\s*100%;[^}]*max-width:\s*100%;[^}]*overflow-x:\s*auto;/s);
  assert.match(stylesheet, /\.markdown a \{[^}]*overflow-wrap:\s*anywhere;[^}]*word-break:\s*break-word;/s);
  assert.match(stylesheet, /\.markdown td \{[^}]*overflow-wrap:\s*anywhere;[^}]*word-break:\s*break-word;/s);
});
