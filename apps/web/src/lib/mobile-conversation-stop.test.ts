// 「停止当前对话」的答复该怎么读：纯函数，直接调用来断言。
//
// 这一套守的是**分支走对了没有**，而不是文案本身 —— 四种答复（真的在停 / 本来就没在跑 /
// 要用户确认强制停止 / 失败）在线上都长成一个 JSON，读错不会有任何报错，只会让界面
// 沉默或者弹出一句用户看不懂的话。
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  conversationStopConfirmCopy,
  conversationStopIdleMessage,
  conversationStopOutcome,
  conversationStopParams,
} from "./mobile-conversation-stop";
import type { MobileRpcReply } from "../features/remote/mobile-rpc";

function reply(overrides: Partial<MobileRpcReply>): MobileRpcReply {
  return { ok: true, status: 200, ...overrides };
}

// ── 失败：按码分支，不按文案 ───────────────────────────────────────────────
test("还有排队中的请求时按码要求强制停止，而不是去匹配那句话", () => {
  // 关键判据：error 故意写成一句**与电脑端真实文案不同**的话。若实现改成匹配文案
  // （例如 `error.includes("排队")`），这条必红 —— 而喂真实文案的单测抓不到这个回归，
  // 因为真实文案会被本地化、会被改措辞，那一刻分支就静默失效了。
  const outcome = conversationStopOutcome(reply({
    ok: false,
    status: 409,
    code: "active_runs_present",
    error: "写死的一句话，与真实文案无关",
  }));
  assert.deepEqual(outcome, { kind: "needs-force" });
});

test("其它失败把电脑端那句话原样带出来，没有就兜底", () => {
  assert.deepEqual(
    conversationStopOutcome(reply({ ok: false, status: 409, error: "这条对话由自动编排任务接管，请在编排面板里暂停或停止它的队列。" })),
    { kind: "failed", message: "这条对话由自动编排任务接管，请在编排面板里暂停或停止它的队列。" },
  );
  // 空 error / 纯空白都要退到兜底句：给用户看一个空字符串等于什么都不说。
  assert.deepEqual(conversationStopOutcome(reply({ ok: false, status: 500 })), { kind: "failed", message: "电脑端没有执行这次停止" });
  assert.deepEqual(conversationStopOutcome(reply({ ok: false, status: 500, error: "   " })), { kind: "failed", message: "电脑端没有执行这次停止" });
});

// ── 成功：只有 stopping 才是"真的在停" ─────────────────────────────────────
test("stopping 才是真的停了，其余状态一律算这一趟什么都没停 —— 但两种要说不同的话", () => {
  assert.deepEqual(conversationStopOutcome(reply({ status: 202, data: { status: "stopping" } })), { kind: "stopping" });
  // idle 是电脑端专门为"没有活跃的一轮"返回的状态（而不是报错）。
  assert.deepEqual(conversationStopOutcome(reply({ data: { status: "idle" } })), { kind: "idle" });
  // 剩下的都是 stopRunByID 原样回的那一轮自己的状态：它还在，只是不由本实例控制。
  // 它们**不能**被当成 idle —— 那会把"还在排队"说成"没有在运行的任务"，
  // 而用户该做的动作完全不同（稍后重试 vs 什么都不用做）。
  for (const status of ["queued", "completed", "failed", "stopped", "interrupted"]) {
    assert.deepEqual(conversationStopOutcome(reply({ data: { status } })), { kind: "uncontrolled" }, `status=${status}`);
  }
  // data 缺失/不是对象同样按"没停成"处理，不许抛。缺状态时按 idle 说 —— 这是常见的那一种
  // （电脑端正常回 idle；只有它明确报了一个别的状态才认为那一轮还在）。
  assert.deepEqual(conversationStopOutcome(reply({})), { kind: "idle" });
  assert.deepEqual(conversationStopOutcome(reply({ data: null })), { kind: "idle" });
  assert.deepEqual(conversationStopOutcome(reply({ data: "stopping" })), { kind: "idle" });
});

test("这一趟什么都没停时的两句话必须不同", () => {
  const idle = conversationStopIdleMessage({ kind: "idle" });
  const uncontrolled = conversationStopIdleMessage({ kind: "uncontrolled" });
  assert.equal(idle, "这条对话当前没有在运行的任务，可能刚刚已经结束了。");
  assert.notEqual(idle, uncontrolled);
  // 后者要点出"它还在，只是没停成"，并给出该做什么（稍后重试）。
  assert.match(uncontrolled, /重试/);
  assert.ok(!uncontrolled.includes("没有在运行"), `不能把还在排队的那一轮说成没在跑：${uncontrolled}`);
});

// ── 请求形状 ──────────────────────────────────────────────────────────────
test("params 里的 force 必须是字符串的 true", () => {
  // 中继那条通道把 params 整个当查询串，且要求取值都是字符串。塞布尔 true 进去，
  // 电脑端会回一句"params 必须是字符串键值对"—— 用户完全无法理解的失败。
  assert.deepEqual(conversationStopParams(true), { force: "true" });
  // 不强制时是空对象：不要发 {force:"false"}，那会在电脑端多一个"是否显式否定了"的分支。
  assert.deepEqual(conversationStopParams(false), {});
});

// ── 文案与桌面端逐字一致 ───────────────────────────────────────────────────
test("确认框文案与桌面端逐字一致", () => {
  // 同一个动作在两端的说法不同，用户会以为是两件事。桌面端那句写在
  // ConversationPage.stopRun 的 pendingConfirm 里，这里是它的第二份 —— 所以钉死。
  assert.equal(conversationStopConfirmCopy.title, "强制停止");
  assert.equal(conversationStopConfirmCopy.message, "该对话还有其他排队中或执行中的请求，强制停止将一并取消它们。是否继续？");
});
