import assert from "node:assert/strict";
import test from "node:test";
import { readFile } from "node:fs/promises";

test("快照刷新会首次读取，并在 Agent 探测期间进行静默强制补刷", async () => {
  const source = await readFile(new URL("./use-remote-snapshot-refresh.ts", import.meta.url), "utf8");
  assert.match(source, /useEffect\(\(\) => \{ void loadRef\.current\(\); \}, \[options\.load\]\);/);
  assert.match(source, /if \(!options\.hasPendingAgentProbe\) return;/);
  assert.match(source, /loadRef\.current\(\{ force: true, silent: true \}\)/);
  assert.match(source, /\}, 3000\)/);
});
