// 「正在恢复会话上下文」这条阶段事件的映射：服务端在拉起 CLI 之前落一条
// system/session_starting，桌面与手机共用 systemItemFromEvent 这一份解析。
// 这里守两件事：① 文案与"是否 resume"一致；② 历史条数只做解释，不编造预计时长。
import { test } from "node:test";
import assert from "node:assert/strict";
import { systemItemFromEvent } from "./timeline";

const base = { id: "e1", runId: "r1", createdAt: "2026-10-26T09:31:19Z" };

test("session_starting：恢复会话时说出历史条数", () => {
  const item = systemItemFromEvent({
    ...base,
    type: "system",
    payload: { type: "system", subtype: "session_starting", resume: true, historyMessages: 3745 },
  });
  assert.ok(item);
  assert.equal(item.variant, "session");
  assert.equal(item.title, "正在恢复会话上下文");
  assert.match(item.detail || "", /3745 条历史消息/);
});

test("session_starting：新会话不说历史", () => {
  const item = systemItemFromEvent({
    ...base,
    type: "system",
    payload: { type: "system", subtype: "session_starting", resume: false },
  });
  assert.ok(item);
  assert.equal(item.title, "正在启动 Claude 会话");
  assert.doesNotMatch(item.detail || "", /条历史消息/);
});

test("session_starting：没有历史条数时不给一个像时长的假承诺", () => {
  const item = systemItemFromEvent({
    ...base,
    type: "system",
    payload: { type: "system", subtype: "session_starting", resume: true, historyMessages: 0 },
  });
  assert.ok(item);
  assert.doesNotMatch(item.detail || "", /秒|分钟/);
});
