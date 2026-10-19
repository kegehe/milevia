import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const source = await readFile(new URL("./use-mobile-pairing-claim.ts", import.meta.url), "utf8");

test("移动端扫码与手输配对共用同一份请求、忙碌和错误处理", () => {
  assert.match(source, /claimMobilePairing\(current\.request, current\.device\(\), \{ pairingID: input\.pairingID, code: input\.code \}\)/);
  assert.match(source, /current\.setBusy\(true\);\s*current\.clearError\(\);/);
  assert.match(source, /finally \{\s*current\.setBusy\(false\);\s*\}/);
});

test("移动端配对提交在网络请求前拒绝不完整二维码或非法校验码", () => {
  assert.match(source, /!\/\^\\d\{6\}\$\/\.test\(input\.code\.trim\(\)\) \|\| \(input\.pairingID !== undefined && !input\.pairingID\.trim\(\)\)/);
  assert.match(source, /current\.reportError\(input\.validationError\);\s*return null;/);
  assert.match(source, /current\.reportError\(cause instanceof Error \? cause\.message : input\.failureError\);/);
});
