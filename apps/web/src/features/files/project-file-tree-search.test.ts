import assert from "node:assert/strict";
import test from "node:test";
import { searchResultSummary, splitSearchMatch } from "./ProjectFileTree";

// 文件搜索的结果列表逻辑：不碰 React、不碰网络，只驱动两个纯函数。
// 守的两条：
//   1. 命中位置标得准 —— 多条结果里同名文件（index.ts）一抓一大把，
//      标错了用户就看不出"为什么它算命中"；
//   2. 服务端截断（上限 100）必须说出来 —— 不说的话用户会把"前 100 个"当成全部命中，
//      然后以为项目里就没有别的了。

test("splitSearchMatch 标出全部命中位置（大小写不敏感）", () => {
  assert.deepEqual(splitSearchMatch("IndexTree.tsx", "index"), [
    { text: "Index", hit: true },
    { text: "Tree.tsx", hit: false },
  ]);
  assert.deepEqual(splitSearchMatch("a-b-a", "A"), [
    { text: "a", hit: true },
    { text: "-b-", hit: false },
    { text: "a", hit: true },
  ]);
});

test("splitSearchMatch 无命中 / 空词时整段原样返回", () => {
  assert.deepEqual(splitSearchMatch("README.md", "zzz"), [{ text: "README.md", hit: false }]);
  // 空词（或只有空白）不能返回空数组 —— 调用方直接把片段拼成文本，
  // 返回空数组等于把这个文件名渲染成空白行。
  assert.deepEqual(splitSearchMatch("README.md", ""), [{ text: "README.md", hit: false }]);
  assert.deepEqual(splitSearchMatch("README.md", "   "), [{ text: "README.md", hit: false }]);
});

test("splitSearchMatch 命中在词尾时不丢尾片段", () => {
  assert.deepEqual(splitSearchMatch("main.go", "go"), [
    { text: "main.", hit: false },
    { text: "go", hit: true },
  ]);
});

test("searchResultSummary 到上限时说清只列了前一批", () => {
  assert.equal(searchResultSummary(1), "1 个匹配");
  assert.equal(searchResultSummary(99), "99 个匹配");
  assert.match(searchResultSummary(100), /只列出前 100 个/);
});
