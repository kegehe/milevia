import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

// 主机指纹确认回归。
//
// 问题：预检一完成就自动勾选「我已核对并确认此主机指纹」，用户没有比对也会被当成
// 已确认，防冒充主机的那道确认形同虚设。指纹必须由用户主动勾选；只有在远端指纹
// 与已保存的不一致时，才允许用户在核对新旧指纹后显式确认并重新固定。

const source = await readFile(new URL("./pages/SSHManagerPage.tsx", import.meta.url), "utf8");

test("预检结果不能代替用户确认主机指纹", () => {
  // 不允许把预检结果直接写进确认态（Boolean(result.hostKey) 就是原来的 bug）。
  assert.doesNotMatch(source, /setHostKeyConfirmed\(Boolean/);
  // 每次预检都清空确认态：换了指纹就必须重新核对。
  assert.match(source, /setHostKeyConfirmed\(false\);/);
  // 保存前仍要有确认拦截。
  assert.match(source, /if \(!hostKeyConfirmed\)/);
});

test("指纹变更时并排展示新旧指纹，且重新确认后才允许保存", () => {
  assert.match(source, /hostKeyChanged/);
  assert.match(source, /savedFingerprint/);
  // 受控例外：普通预检失败仍被挡住，只有“指纹变了”这一种情况能走保存。
  assert.match(source, /if \(!preflight\?\.ok && !preflight\?\.hostKeyChanged\)/);
  assert.match(source, /const canSavePreflight = Boolean\(preflight\?\.ok \|\| preflight\?\.hostKeyChanged\);/);
});
