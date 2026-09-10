import assert from "node:assert/strict";
import { mock } from "node:test";
import test from "node:test";
import { api } from "./lib/api.ts";

// 内部 15s 超时不应立刻判服务不可用：幂等请求应获得一次放宽到 2x 的重试机会，
// 服务端只是瞬时忙（单 SQLite 连接被长事务占住/慢探测）时第二次能成功，避免
// 误导用户去重启 Milevia。真持续无响应才报"持续未响应"。
test("retries an idempotent request once with a longer window on timeout", async () => {
  mock.timers.enable({ apis: ["setTimeout"] });
  const originalFetch = globalThis.fetch;
  try {
    let fetchCalls = 0;
    // fetch 永不返回：只有被内部 AbortController 中止时才以 AbortError 拒绝。
    globalThis.fetch = (_url, init) => {
      fetchCalls++;
      return new Promise((_resolve, reject) => {
        const signal = init.signal;
        const abort = () => reject(new DOMException("Aborted", "AbortError"));
        if (signal?.aborted) {
          abort();
          return;
        }
        signal?.addEventListener("abort", abort, { once: true });
      });
    };
    const flush = async () => {
      await Promise.resolve();
      await new Promise((resolve) => setImmediate(resolve));
    };

    const pending = api("/timeout", { method: "GET" });
    // 立即挂上断言与兜底 catch，避免窗口耗尽瞬间出现 unhandledRejection。
    const rejected = assert.rejects(pending, /控制服务持续未响应，请重启 Milevia 后重试/);
    pending.catch(() => {});

    // 第一次尝试：默认 15s 窗口到点 → 内部超时。
    mock.timers.tick(15_001);
    await flush();
    assert.equal(fetchCalls, 1, "first attempt should time out without rejecting");

    // 回退 500ms 后自动发起第二次尝试（此时应有 fetchCalls=2）。
    mock.timers.tick(501);
    await flush();
    assert.equal(fetchCalls, 2, "timeout should be retried once for an idempotent GET");

    // 第二次尝试窗口放宽为 2x（30s）：过 15s 仍不应拒绝。
    mock.timers.tick(15_001);
    await flush();
    assert.equal(fetchCalls, 2, "retry is still within its longer window");

    // 第二次尝试的 30s 窗口也耗尽 → 最终以"持续未响应"拒绝。
    mock.timers.tick(15_001);
    await flush();
    await rejected;
  } finally {
    globalThis.fetch = originalFetch;
    mock.timers.reset();
  }
});

test("non-idempotent requests still fail fast on timeout without auto-retry", async () => {
  mock.timers.enable({ apis: ["setTimeout"] });
  const originalFetch = globalThis.fetch;
  try {
    let fetchCalls = 0;
    globalThis.fetch = (_url, init) => {
      fetchCalls++;
      return new Promise((_resolve, reject) => {
        const signal = init.signal;
        const abort = () => reject(new DOMException("Aborted", "AbortError"));
        if (signal?.aborted) {
          abort();
          return;
        }
        signal?.addEventListener("abort", abort, { once: true });
      });
    };
    const flush = async () => {
      await Promise.resolve();
      await new Promise((resolve) => setImmediate(resolve));
    };

    const pending = api("/timeout", { method: "POST" });
    const rejected = assert.rejects(pending, /控制服务未在 15 秒内响应，请稍后重试/);
    pending.catch(() => {});
    mock.timers.tick(15_001);
    await flush();
    await rejected;
    assert.equal(fetchCalls, 1, "POST should not auto-retry on timeout (double-submit risk)");
  } finally {
    globalThis.fetch = originalFetch;
    mock.timers.reset();
  }
});
