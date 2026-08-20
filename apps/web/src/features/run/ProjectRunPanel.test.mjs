import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const source = await readFile(new URL("./ProjectRunPanel.tsx", import.meta.url), "utf8");

test("refreshes uptime every second only while the project process is running", () => {
  assert.match(source, /const \[now, setNow\] = useState\(\(\) => Date\.now\(\)\);/);
  assert.match(source, /if \(status\?\.status !== "running" \|\| !status\.startedAt\) return;/);
  assert.match(source, /const refresh = \(\) => setNow\(Date\.now\(\)\);[\s\S]*?const timer = setInterval\(refresh, 1_000\);[\s\S]*?return \(\) => clearInterval\(timer\);/);
  assert.match(source, /formatUptime\(new Date\(status\.startedAt\), now\)/);
});
