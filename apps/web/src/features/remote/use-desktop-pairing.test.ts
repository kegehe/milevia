import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const source = await readFile(new URL("./use-desktop-pairing.ts", import.meta.url), "utf8");

test("桌面配对 Hook 统一管理生成、确认与轮询状态", () => {
  assert.match(source, /const \[pairingID, setPairingID\] = useState/);
  assert.match(source, /const \[pairingReadyForConfirm, setPairingReadyForConfirm\] = useState\(false\)/);
  assert.match(source, /const \[pairingConfirmed, setPairingConfirmed\] = useState\(false\)/);
  assert.match(source, /current\.request<PairingResponse>\("\/api\/remote\/pairing", \{ method: "POST" \}\)/);
  assert.match(source, /current\.request\("\/api\/remote\/pairing\/confirm", \{ method: "POST", body: JSON\.stringify\(\{ pairingId: target \}\) \}\)/);
  assert.match(source, /useDesktopPairingStatusPoll\(\{/);
});

test("桌面配对生成和确认保留终态与失败处理", () => {
  assert.match(source, /setPairingReadyForConfirm\(false\);\s*setPairingConfirmed\(false\);/);
  assert.match(source, /current\.onGenerated\(\);/);
  assert.match(source, /if \(status === "confirmed"\) complete\(\)/);
  assert.match(source, /else if \(status === "expired" \|\| status === "cancelled"\)/);
  assert.match(source, /callbacksRef\.current\.onConfirmed\(\);/);
  assert.match(source, /current\.reportError\(cause instanceof Error \? cause\.message : "无法生成配对二维码"\)/);
  assert.match(source, /current\.reportError\(cause instanceof Error \? cause\.message : "确认绑定失败"\)/);
});
