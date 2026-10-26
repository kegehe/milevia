import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

// 设置页的“代码字号”必须真的作用到 Git 差异视图与冲突解决的结果编辑器。
//
// 回归：这两处曾经写死 `.git-diff-code { font: 12px/… }` 和 `fontSize={13}`，
// 把设置里的字号调大后它们看起来还是原样，而设置页的说明写着会作用于差异视图。

const [workbench, conflict, styles] = await Promise.all([
  readFile(new URL("./GitWorkbench.tsx", import.meta.url), "utf8"),
  readFile(new URL("./ConflictSolveView.tsx", import.meta.url), "utf8"),
  readFile(new URL("../../git.css", import.meta.url), "utf8"),
]);

test("差异视图把代码字号传给 .git-diff-code，而不是写死像素", () => {
  assert.match(workbench, /import \{ useCodeFontSize \} from "\.\.\/files\/useCodeFontSize";/);
  assert.match(workbench, /function DiffViewer[\s\S]*?const \{ fontSize \} = useCodeFontSize\(\);/);
  assert.match(workbench, /"--git-code-font-size": `\$\{fontSize\}px`/);
  // CSS 侧只能有变量（缺省值可以留），不能再有写死的 font 简写或小屏覆盖。
  assert.doesNotMatch(styles, /\.git-diff-code \{[^}]*font:\s*\d+px/);
  assert.match(styles, /\.git-diff-code \{[^}]*font-size:\s*var\(--git-code-font-size, 12px\)/);
  assert.doesNotMatch(styles, /\.git-diff-code \{ max-height: 250px; font-size: \d+px; \}/);
  assert.match(styles, /\.git-workbench\[data-mobile="true"\] \.git-diff-code \{ font-size: var\(--git-code-font-size, 13px\); \}/);
  assert.match(styles, /\.git-diff-line \.git-diff-number \{[^}]*font-size: min\(1em, 13px\)/);
  // 窄窗（≤680px）行号列收窄到 40px，行号必须压回 11px，否则 4 位数会顶出列宽。
  assert.match(styles, /\.git-diff-line \.git-diff-number \{ font-size: 11px; \}/);
});

test("冲突解决的结果编辑器复用文件编辑器的代码字号", () => {
  assert.match(conflict, /const \{ fontSize \} = useCodeFontSize\(\);/);
  assert.match(conflict, /<CodeFileView content=\{workingText\} filename=\{fileName\} fontSize=\{fontSize\}/);
  assert.doesNotMatch(conflict, /filename=\{fileName\} fontSize=\{13\}/);
});
