import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const apiModule = await readFile(new URL("./lib/api.ts", import.meta.url), "utf8");

test("only safe API methods receive automatic retries", () => {
  assert.match(apiModule, /function retryCountFor\(init\?: RequestInit\): number/);
  assert.match(apiModule, /const method = \(init\?\.method \?\? "GET"\)\.toUpperCase\(\);/);
  assert.match(apiModule, /return method === "GET" \|\| method === "HEAD" \|\| method === "OPTIONS" \? 2 : 0;/);
  assert.match(apiModule, /export async function api<T>\(path: string, init\?: RequestInit, retries = retryCountFor\(init\)\)/);
});