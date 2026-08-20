import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { transform } from "esbuild";

// 从 run/linkify.ts 编译出 computeLineLinks（纯 TS、无 JSX，可直接从 data URL 引入），
// 验证文件查看器按行计算可点击链接区间的偏移逻辑。
const source = await readFile(new URL("../run/linkify.ts", import.meta.url), "utf8");
const { code } = await transform(source, { loader: "ts", format: "esm", target: "es2022" });
const { computeLineLinks } = await import(`data:text/javascript;base64,${Buffer.from(code).toString("base64")}`);

test("turns a whole-line URL into a clickable range", () => {
  const u = "https://example.com/docs";
  assert.deepEqual(computeLineLinks(u), [{ from: 0, to: u.length, url: u }]);
});

test("links a URL embedded in text, keeping surrounding text as ranges", () => {
  const u = "https://x.com/a";
  const start = "see ".length;
  assert.deepEqual(computeLineLinks(`see ${u} for details`), [
    { from: start, to: start + u.length, url: u },
  ]);
});

test("strips trailing punctuation from the link address", () => {
  const u = "https://pkg.dev/v2";
  const start = "install from ".length;
  assert.deepEqual(computeLineLinks(`install from ${u},`), [
    { from: start, to: start + u.length, url: u },
  ]);
});

test("links multiple URLs on one line", () => {
  const a = "http://a.com";
  const b = "http://b.com";
  const aStart = "a ".length;
  const bStart = aStart + a.length + " b ".length;
  assert.deepEqual(computeLineLinks(`a ${a} b ${b}`), [
    { from: aStart, to: aStart + a.length, url: a },
    { from: bStart, to: bStart + b.length, url: b },
  ]);
});

test("leaves a line without URLs with no link ranges", () => {
  assert.deepEqual(computeLineLinks("const x = 1;"), []);
});
