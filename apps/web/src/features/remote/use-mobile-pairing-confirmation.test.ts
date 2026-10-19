import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const source = await readFile(new URL("./use-mobile-pairing-confirmation.ts", import.meta.url), "utf8");

test("移动端配对确认将待确认令牌和实例作为同一状态单元", () => {
  assert.match(source, /const \[pendingClaim, setPendingClaim\] = useState\(\{ token: "", instanceID: "" \}\);/);
  assert.match(source, /setPendingClaim\(\{ token: result\.accessToken, instanceID: result\.instanceId \|\| "" \}\);/);
  assert.match(source, /pendingToken: pendingClaim\.token,/);
  assert.match(source, /instanceID: pendingClaim\.instanceID,/);
});

test("移动端配对确认只在终态后清理等待状态", () => {
  assert.match(source, /if \(!result\.accessToken\) return;/);
  assert.match(source, /const claim = pendingClaim;\s*clearConfirmation\(\);\s*callbacksRef\.current\.onConfirmed\(claim\);/);
  assert.match(source, /clearConfirmation\(\);\s*callbacksRef\.current\.onExpired\(\);/);
});
