import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const component = await readFile(new URL("./App.tsx", import.meta.url), "utf8");

test("only safe API methods receive automatic retries", () => {
  assert.match(component, /function retryCountFor\(init\?: RequestInit\): number/);
  assert.match(component, /const method = \(init\?\.method \?\? "GET"\)\.toUpperCase\(\);/);
  assert.match(component, /return method === "GET" \|\| method === "HEAD" \|\| method === "OPTIONS" \? 2 : 0;/);
  assert.match(component, /async function api<T>\(path: string, init\?: RequestInit, retries = retryCountFor\(init\)\)/);
});
