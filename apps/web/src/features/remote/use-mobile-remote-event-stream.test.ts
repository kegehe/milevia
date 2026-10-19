import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const source = await readFile(new URL("./use-mobile-remote-event-stream.ts", import.meta.url), "utf8");

test("移动端事件流携带认证和断点，并监测静默断流", () => {
  assert.match(source, /Authorization: `Bearer \$\{token\}`/);
  assert.match(source, /headers\.set\("Last-Event-ID", lastEventID\)/);
  assert.match(source, /export const streamIdleTimeoutMs = 45_000;/);
  assert.match(source, /idleTimer = window\.setTimeout\(\(\) => controller\.abort\(\), streamIdleTimeoutMs\)/);
});

test("移动端事件流按设备、令牌与可见性隔离生命周期", () => {
  assert.match(source, /\[options\.instanceID, options\.activeToken, options\.deviceEpoch, options\.streamURL\]/);
  assert.match(source, /if \(stopped \|\| !isCurrentTokenRef\.current\(options\.activeToken\)\) return;/);
  assert.match(source, /if \(!options\.visible \|\| wasVisible \|\| !options\.instanceID\) return;/);
  assert.match(source, /void refreshSnapshotRef\.current\(\);\s*refreshGitRef\.current\(\);/);
});

test("移动端事件流清理所有轮询和节流定时器", () => {
  assert.match(source, /window\.clearInterval\(fallbackTimer\)/);
  assert.match(source, /window\.clearTimeout\(refreshTimer\)/);
  assert.match(source, /window\.clearTimeout\(gitRefreshTimer\)/);
  assert.match(source, /controller\.abort\(\);/);
});
