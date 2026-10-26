import assert from "node:assert/strict";
import test from "node:test";
import { waitForOAuthFlowResult } from "./oauth-flow";

test("取消等待立即结束且不查询授权状态", async () => {
  const controller = new AbortController();
  let reads = 0;
  const pending = waitForOAuthFlowResult(async () => { reads += 1; return { status: "done" }; }, controller.signal, 60000);
  controller.abort();
  assert.equal((await pending).ok, false);
  assert.equal(reads, 0);
});

test("查询途中取消后，迟到的 done 不能被判为成功", async () => {
  const controller = new AbortController();
  const result = await waitForOAuthFlowResult(async () => {
    controller.abort();
    return { status: "done" };
  }, controller.signal, 0);
  assert.equal(result.ok, false);
  assert.match(result.error || "", /取消/);
});

test("拒绝、服务端取消与超时都有终态", async () => {
  for (const status of ["error", "canceled"]) {
    const result = await waitForOAuthFlowResult(async () => ({ status, error: "拒绝" }), undefined, 0);
    assert.deepEqual(result, { ok: false, error: "拒绝" });
  }
  const result = await waitForOAuthFlowResult(async () => ({ status: "pending" }), undefined, 0, 2);
  assert.match(result.error || "", /超时/);
});

test("真实成功才返回 ok", async () => {
  assert.deepEqual(await waitForOAuthFlowResult(async () => ({ status: "done" }), undefined, 0), { ok: true });
});

test("默认轮询次数覆盖后端 10 分钟有效期", async () => {
  // 后端 mcpOAuthFlowTTL = 10 分钟（120 × 5s）。第 61 次才完成授权时不能提前判超时。
  let reads = 0;
  const result = await waitForOAuthFlowResult(async () => {
    reads += 1;
    return { status: reads > 60 ? "done" : "pending" };
  }, undefined, 0);
  assert.equal(result.ok, true);
  assert.equal(reads, 61);
});
