import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const source = await readFile(new URL("./use-remote-instances.ts", import.meta.url), "utf8");

test("实例读取以请求代次、当前令牌和设备代次隔离响应", () => {
  assert.match(source, /const requestGeneration = \+\+requestGenerationRef\.current;/);
  assert.match(source, /const requestScope = current\.scopeKey;/);
  assert.match(source, /requestGeneration !== requestGenerationRef\.current \|\| requestToken !== optionsRef\.current\.readToken\(\) \|\| requestScope !== optionsRef\.current\.scopeKey/);
  assert.match(source, /const optionsRef = useRef\(options\);/);
  assert.match(source, /\}, \[\]\);/);
  assert.match(source, /const superseded: InstanceLoadResult<T> = \{ instances: \[\], outcome: "superseded" \}/);
});

test("实例读取为无令牌、实例失效和设备摘要提供稳定收敛", () => {
  assert.match(source, /if \(!requestToken\) \{/);
  assert.match(source, /setInstances\(\[\]\);\s*setInstanceID\(""\);/);
  assert.match(source, /nextInstances\.some\(\(item\) => item\.instanceId === current\)/);
  assert.match(source, /current\.patchDevice\(requestToken, \{/);
});
