import assert from "node:assert/strict";
import test from "node:test";
import { shouldShowIcpFiling } from "./icp-filing";

test("网页端显示备案号", () => {
  assert.equal(shouldShowIcpFiling({ desktop: false, native: false }), true);
});

test("桌面端外壳不显示", () => {
  assert.equal(shouldShowIcpFiling({ desktop: true, native: false }), false);
});

test("手机原生包不显示", () => {
  assert.equal(shouldShowIcpFiling({ desktop: false, native: true }), false);
});

test("两边同时成立时也不显示（Capacitor 注入桌面运行时的极端情况）", () => {
  assert.equal(shouldShowIcpFiling({ desktop: true, native: true }), false);
});
