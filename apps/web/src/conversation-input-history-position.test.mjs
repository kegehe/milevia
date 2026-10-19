import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const page = await readFile(new URL("./pages/ConversationPage.tsx", import.meta.url), "utf8");
const styles = await readFile(new URL("./conversation.css", import.meta.url), "utf8");

test("input history position uses the current history index and hides at the newest draft", () => {
  assert.match(page, /const inputHistoryPosition = historyIndex\.current === null \? null : historyIndex\.current \+ 1;/);
  assert.match(page, /inputHistoryPosition !== null && inputHistory\.length > 0/);
  assert.match(page, /\{inputHistoryPosition\}\/\{inputHistory\.length\}/);
  assert.match(page, /aria-label=\{`输入历史第 \$\{inputHistoryPosition\} 条，共 \$\{inputHistory\.length\} 条`\}/);
});

test("input history position is anchored to the composer input area", () => {
  assert.match(page, /className="composer-history-position"/);
  assert.match(styles, /\.composer-input-area \{ position: relative; \}/);
  assert.match(styles, /\.composer-input-area textarea \{ padding-right: 54px; \}/);
  assert.match(styles, /\.composer-history-position \{[\s\S]*position: absolute;[\s\S]*top: 8px;[\s\S]*right: 11px;/);
});
