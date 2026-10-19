import assert from "node:assert/strict";
import test from "node:test";
import { readFile } from "node:fs/promises";

test("实例轮询以令牌、设备代次和可见性作为生命周期边界", async () => {
  const source = await readFile(new URL("./use-remote-instance-polling.ts", import.meta.url), "utf8");
  assert.match(source, /if \(!options\.activeToken\.trim\(\)\) return;/);
  assert.match(source, /void loadRef\.current\(\);/);
  assert.match(source, /if \(!options\.visible\) return;/);
  assert.match(source, /window\.setInterval\(\(\) => \{ void loadRef\.current\(\); \}, 5000\)/);
  assert.match(source, /\[options\.activeToken, options\.deviceEpoch, options\.visible\]/);
});
