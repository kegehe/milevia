import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { consumeMobileEventStream } from "./use-mobile-remote-event-stream";

const source = await readFile(new URL("./use-mobile-remote-event-stream.ts", import.meta.url), "utf8");

test("移动端事件流携带认证和断点，并监测静默断流", () => {
  assert.match(source, /Authorization: `Bearer \$\{token\}`/);
  assert.match(source, /headers\.set\("Last-Event-ID", lastEventID\)/);
  assert.match(source, /export const streamIdleTimeoutMs = 45_000;/);
  assert.match(source, /idleTimer = window\.setTimeout\(\(\) => controller\.abort\(\), streamIdleTimeoutMs\)/);
});

test("移动端事件流按设备、令牌与可见性隔离生命周期", () => {
  assert.match(source, /\[options\.instanceID, options\.activeToken, options\.deviceEpoch, options\.streamURL, shared\]/);
  assert.match(source, /if \(stopped \|\| !isCurrentTokenRef\.current\(options\.activeToken\)\) return;/);
  assert.match(source, /if \(!options\.visible \|\| wasVisible \|\| !options\.instanceID\) return;/);
  assert.match(source, /void refreshSnapshotRef\.current\(\);\s*refreshGitRef\.current\(\);/);
});

test("移动端事件流清理所有轮询和节流定时器", () => {
  assert.match(source, /window\.clearInterval\(fallbackTimer\)/);
  assert.match(source, /window\.clearTimeout\(refreshTimer\)/);
  assert.match(source, /window\.clearTimeout\(gitRefreshTimer\)/);
  assert.match(source, /controller\.abort\(\);/);
});

test("事件解析跨字节块，携带零断点与认证，回调失败时中止旧连接", async (t) => {
  const descriptor = Object.getOwnPropertyDescriptor(globalThis, "window");
  Object.defineProperty(globalThis, "window", { configurable: true, value: globalThis });
  t.after(() => {
    if (descriptor) Object.defineProperty(globalThis, "window", descriptor);
    else Reflect.deleteProperty(globalThis, "window");
  });
  let requestSignal: AbortSignal | undefined;
  t.mock.method(globalThis, "fetch", async (_url: string, init: RequestInit) => {
    assert.equal(new Headers(init.headers).get("Last-Event-ID"), "0");
    assert.equal(new Headers(init.headers).get("Authorization"), "Bearer test-token");
    requestSignal = init.signal as AbortSignal;
    const encoder = new TextEncoder();
    return new Response(new ReadableStream({ start(controller) {
      for (const chunk of [": keepalive\n\nid: 1\nda", "ta: {\"ok\":\n", "data: true}\n\n"]) controller.enqueue(encoder.encode(chunk));
      controller.close();
    } }));
  });
  let calls = 0;
  await assert.rejects(consumeMobileEventStream("/v1/stream", "test-token", "0", (event) => {
    calls++;
    assert.equal(event.lastEventId, "1");
    assert.deepEqual(JSON.parse(event.data), { ok: true });
    throw new Error("持久入队失败");
  }, new AbortController().signal), /持久入队失败/);
  assert.equal(calls, 1);
  assert.equal(requestSignal?.aborted, true);
});
