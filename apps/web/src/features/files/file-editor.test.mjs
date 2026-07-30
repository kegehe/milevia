import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const source = await readFile(new URL("./FileEditor.tsx", import.meta.url), "utf8");

test("editor loading failures use a Chinese fallback", () => {
  assert.match(source, /setLoadError\("无法加载编辑器，请刷新页面后重试。"\)/);
  assert.doesNotMatch(source, /setLoadError\(err instanceof Error \? err\.message/);
});
