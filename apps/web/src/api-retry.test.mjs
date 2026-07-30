import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { api } from "./lib/api.ts";

const apiModule = await readFile(new URL("./lib/api.ts", import.meta.url), "utf8");

test("only safe API methods receive automatic retries", () => {
  assert.match(apiModule, /function retryCountFor\(init\?: RequestInit\): number/);
  assert.match(apiModule, /const method = \(init\?\.method \?\? "GET"\)\.toUpperCase\(\);/);
  assert.match(apiModule, /return method === "GET" \|\| method === "HEAD" \|\| method === "OPTIONS" \? 2 : 0;/);
  assert.match(apiModule, /export async function api<T>\(path: string, init\?: RequestInit, retries = retryCountFor\(init\)\)/);
});

test("successful responses with invalid JSON use a Chinese fallback", () => {
  assert.match(apiModule, /服务响应格式无效，请稍后重试。/);
  assert.match(apiModule, /response\.status === 204 \|\| response\.status === 205/);
});

test("does not turn an aborted response body into an invalid-response error", async () => {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async () => ({
    ok: true,
    status: 200,
    json: async () => { throw new DOMException("Aborted", "AbortError"); },
  });
  try {
    await assert.rejects(api("/cancel", undefined, 0), (cause) => cause instanceof DOMException && cause.name === "AbortError");
  } finally {
    globalThis.fetch = originalFetch;
  }
});
