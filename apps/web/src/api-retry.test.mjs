import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { api } from "./lib/api.ts";

const apiModule = await readFile(new URL("./lib/api.ts", import.meta.url), "utf8");

test("only safe API methods receive automatic retries", () => {
  assert.match(apiModule, /function retryCountFor\(init\?: RequestInit, path = ""\): number/);
  assert.match(apiModule, /const requestMethod = \(init\?: RequestInit\) => \(init\?\.method \?\? "GET"\)\.toUpperCase\(\);/);
  assert.match(apiModule, /return method === "GET" \|\| method === "HEAD" \|\| method === "OPTIONS" \? 2 : 0;/);
  assert.match(apiModule, /export async function api<T>\(path: string, init\?: RequestInit, retries = retryCountFor\(init, path\)\)/);
  // 长任务一律不重试：那张"放宽到 2x 再来一次"是给服务端瞬时忙用的，套在分钟级的
  // 重活上等于把 npm 安装 / git push 整件事再跑一遍。
  assert.match(apiModule, /if \(requestBudgetMs\(path, method\) > requestTimeoutMs\) return 0;/);
  assert.match(apiModule, /return apiWithTimeout<T>\(path, init, retries, requestBudgetMs\(path, requestMethod\(init\)\)\);/);
  // 预算**必须带方法**：同一条路径上常同时挂着读与写（GET/POST /api/projects 与
  // /git/branches、GET/POST /api/ssh-connections），只按路径匹配会把快的读也拖成分钟级。
  assert.match(apiModule, /entry\.methods\.includes\(method\) && entry\.pattern\.test\(bare\)/);
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

test("preserves structured conflict details for actionable client feedback", async () => {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async () => ({
    ok: false,
    status: 409,
    json: async () => ({
      error: "项目工作区正被其他 AI 任务或 Git 操作占用，请等待当前操作完成后重试。",
      code: "workspace_occupied",
      details: { ownerKind: "git_operation", ownerSummary: "Git 操作正在使用项目工作区" },
    }),
  });
  try {
    await assert.rejects(api("/workspace", { method: "POST" }, 0), (cause) => {
      return cause instanceof Error
        && cause.message.includes("项目工作区")
        && cause.code === "workspace_occupied"
        && cause.details?.ownerKind === "git_operation";
    });
  } finally {
    globalThis.fetch = originalFetch;
  }
});
