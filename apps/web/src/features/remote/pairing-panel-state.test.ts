import assert from "node:assert/strict";
import test from "node:test";
import { noticeAfterClosingPairingPanel, pairingPanelOpenState } from "./pairing-panel-state";

test("配对页关闭时只保留等待电脑确认的状态", () => {
  assert.deepEqual(noticeAfterClosingPairingPanel({ text: "等待", state: "waiting" }), { text: "等待", state: "waiting" });
  assert.equal(noticeAfterClosingPairingPanel({ text: "失败", state: "error" }), null);
  assert.equal(noticeAfterClosingPairingPanel({ text: "成功", state: "success" }), null);
});

test("打开配对页会复位上一次的临时输入和浮层", () => {
  assert.deepEqual(pairingPanelOpenState(), { notice: null, manualCode: "", devicesOpen: false, expanded: true });
});
