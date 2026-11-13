import assert from "node:assert/strict";
import test from "node:test";

import {
  INSTANCE_OFFLINE_CODE,
  INSTANCE_OFFLINE_MESSAGE,
  localizeRpcReply,
  rpcFailureMessage,
  type MobileRpcReply,
} from "./mobile-rpc";

// "请求进行中电脑掉线"是**云端**自己造的失败：回包是 HTTP 200 + ok:false，
// error 里是云端写的机器串，而适配器会把 error 原样显示。必须按 code 翻成中文。
test("掉线回包按 code 翻成中文，英文原文不外泄", () => {
  const reply: MobileRpcReply = {
    ok: false,
    status: 409,
    error: "instance_offline: the computer is not connected right now",
    code: INSTANCE_OFFLINE_CODE,
  };
  assert.equal(rpcFailureMessage(reply), INSTANCE_OFFLINE_MESSAGE);
  const localized = localizeRpcReply(reply);
  assert.equal(localized.error, INSTANCE_OFFLINE_MESSAGE);
  assert.equal(localized.status, 409, "其余字段必须原样保留");
  assert.equal(localized.code, INSTANCE_OFFLINE_CODE, "码不能在翻译时丢掉");
});

// 旧版云端还没有 code，只能退回去匹配文案 —— 升级顺序反过来时（新手机 + 旧云端）
// 这条是唯一能命中的判据。
test("旧版云端没有 code 时按文案兜底", () => {
  const reply: MobileRpcReply = {
    ok: false,
    status: 409,
    error: "instance_offline: the computer is not connected right now",
  };
  assert.equal(rpcFailureMessage(reply), INSTANCE_OFFLINE_MESSAGE);
  assert.equal(localizeRpcReply(reply).error, INSTANCE_OFFLINE_MESSAGE);
});

// 认不出的失败原样透出，**尤其不能**把空 error 填成"操作失败"：
// 页面里那些调用点各自带着更贴切的中文兜底（如"无法读取可用模型"）。
test("认不出的失败原样返回，空 error 不被改写", () => {
  const unknown: MobileRpcReply = { ok: false, status: 500, error: "磁盘写满了" };
  assert.equal(localizeRpcReply(unknown), unknown);
  assert.equal(rpcFailureMessage(unknown), "磁盘写满了");

  const silent: MobileRpcReply = { ok: false, status: 500 };
  assert.equal(localizeRpcReply(silent), silent);
  assert.equal(rpcFailureMessage(silent), "操作失败");
});

// 成功回包一律不碰：它没有 error 字段，也不该被补上一个。
test("成功回包原样返回", () => {
  const reply: MobileRpcReply = { ok: true, status: 200, data: { entries: [] } };
  assert.equal(localizeRpcReply(reply), reply);
});
