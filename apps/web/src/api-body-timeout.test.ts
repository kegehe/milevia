import assert from "node:assert/strict";
import test from "node:test";
import { apiWithTimeout } from "./lib/api";

const flush = () => new Promise<void>((resolve) => setImmediate(resolve));

function stalledResponse(signal: AbortSignal, status = 200): Response {
  return new Response(new ReadableStream({
    start(controller) {
      controller.enqueue(new TextEncoder().encode('{"value":'));
      signal.addEventListener("abort", () => controller.error(new DOMException("Aborted", "AbortError")), { once: true });
    },
  }), { status });
}

for (const status of [200, 400, 503]) {
  test(`正文停滞受请求总超时保护，写请求不重试（HTTP ${status}）`, async (t) => {
    t.mock.timers.enable({ apis: ["setTimeout"] });
    let calls = 0;
    let internalSignal: AbortSignal;
    t.mock.method(globalThis, "fetch", async (_url: unknown, init: RequestInit) => {
      calls++;
      internalSignal = init.signal!;
      return stalledResponse(internalSignal, status);
    });
    const result = assert.rejects(apiWithTimeout("/body", { method: "POST" }, 0, 1000), /控制服务未在 1 秒内响应/);
    await flush();
    t.mock.timers.tick(1001);
    await result;
    assert.equal(internalSignal!.aborted, true);
    assert.equal(calls, 1);
  });

  test(`调用方在收到响应头后仍能取消正文读取（HTTP ${status}）`, async (t) => {
    const caller = new AbortController();
    t.mock.method(globalThis, "fetch", async (_url: unknown, init: RequestInit) => stalledResponse(init.signal!, status));
    const result = assert.rejects(apiWithTimeout("/body", { signal: caller.signal }, 0, 1000), { name: "AbortError" });
    await flush();
    caller.abort();
    await result;
  });
}

test("GET 正文超时会重试一次，并能返回第二次的完整结果", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  let calls = 0;
  t.mock.method(globalThis, "fetch", async (_url: unknown, init: RequestInit) => {
    return ++calls === 1 ? stalledResponse(init.signal!) : Response.json({ value: "complete" });
  });
  const pending = apiWithTimeout("/body", undefined, 1, 1000);
  await flush();
  t.mock.timers.tick(1001);
  await flush();
  t.mock.timers.tick(501);
  assert.deepEqual(await pending, { value: "complete" });
  assert.equal(calls, 2);
});

test("完整正文与无效 JSON 结束后释放超时和调用方取消监听", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const caller = new AbortController();
  const signals: AbortSignal[] = [];
  t.mock.method(globalThis, "fetch", async (_url: unknown, init: RequestInit) => {
    signals.push(init.signal!);
    return signals.length === 1 ? Response.json({ value: 1 }) : new Response("not json");
  });
  assert.deepEqual(await apiWithTimeout("/body", { signal: caller.signal }, 0, 1000), { value: 1 });
  await assert.rejects(apiWithTimeout("/body", { signal: caller.signal }, 0, 1000), /服务响应格式无效/);
  caller.abort();
  t.mock.timers.tick(2000);
  assert.ok(signals.every((signal) => !signal.aborted));
});
