import assert from "node:assert/strict";
import test from "node:test";
import { coalesceRefresh } from "./refresh-coalesce.ts";

const tick = () => new Promise((resolve) => setTimeout(resolve, 0));

test("并发调用合并成一次在飞请求 + 至多一次尾部补刷", async () => {
  let calls = 0;
  let release: (() => void) | null = null;
  const refresh = coalesceRefresh(async () => {
    calls += 1;
    await new Promise<void>((resolve) => { release = resolve; });
  });

  // 握手期间来 20 次调用，只应触发 1 个请求。
  const inFlight = Array.from({ length: 20 }, () => refresh());
  assert.equal(calls, 1);

  release?.();
  await Promise.all(inFlight);
  // 尾部补刷恰好一次，之后不再继续自我触发。
  assert.equal(calls, 2);
  await tick();
  assert.equal(calls, 2);
});

test("串行调用各自发请求，不会因为合并而漏刷新", async () => {
  let calls = 0;
  const refresh = coalesceRefresh(async () => { calls += 1; });
  await refresh();
  await refresh();
  await refresh();
  assert.equal(calls, 3);
});

test("请求失败也要解除在飞状态，后续调用仍会重新发起", async () => {
  let calls = 0;
  let shouldFail = true;
  const refresh = coalesceRefresh(async () => {
    calls += 1;
    if (shouldFail) throw new Error("boom");
  });

  await assert.rejects(refresh(), /boom/);
  shouldFail = false;
  await refresh();
  assert.equal(calls, 2);
});

test("在飞期间的调用不额外发请求，但会拿到同一个 promise", async () => {
  let calls = 0;
  let release: (() => void) | null = null;
  const refresh = coalesceRefresh(async () => {
    calls += 1;
    await new Promise<void>((resolve) => { release = resolve; });
  });
  const first = refresh();
  const second = refresh();
  assert.equal(first, second);
  release?.();
  await first;
  assert.equal(calls, 2); // 首次 + 合并出来的那一次尾部补刷
});
