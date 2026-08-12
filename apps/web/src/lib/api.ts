// API 请求封装 — 从 App.tsx 提取

import { apiURL, sessionHeaders } from "./runtime";

const requestTimeoutMs = 15_000;

function retryCountFor(init?: RequestInit): number {
  const method = (init?.method ?? "GET").toUpperCase();
  return method === "GET" || method === "HEAD" || method === "OPTIONS" ? 2 : 0;
}

export async function api<T>(path: string, init?: RequestInit, retries = retryCountFor(init)): Promise<T> {
  let lastError: unknown;
  const signal = init?.signal;
  for (let attempt = 0; attempt <= retries; attempt++) {
    try {
      if (signal?.aborted) throw new DOMException("Aborted", "AbortError");
      const headers = sessionHeaders(init?.headers);
      if (!headers.has("Content-Type")) headers.set("Content-Type", "application/json");
      const controller = new AbortController();
      const timeout = globalThis.setTimeout(() => controller.abort(), requestTimeoutMs);
      const abort = () => controller.abort();
      signal?.addEventListener("abort", abort, { once: true });
      let response: Response;
      try {
        response = await fetch(apiURL(path), {
          ...init,
          headers,
          signal: controller.signal,
        });
      } catch (cause) {
        if (controller.signal.aborted && !signal?.aborted) {
          throw new Error("控制服务未在 15 秒内响应，请重启 Milevia 后重试。");
        }
        throw cause;
      } finally {
        globalThis.clearTimeout(timeout);
        signal?.removeEventListener("abort", abort);
      }
      if (response.ok) {
        if (response.status === 204 || response.status === 205) return undefined as T;
        try {
          return await response.json() as T;
        } catch (cause: unknown) {
          if (cause instanceof DOMException && cause.name === "AbortError") throw cause;
          throw new Error("服务响应格式无效，请稍后重试。");
        }
      }
      const body = await response.json().catch(() => null);
      const message = body?.error || `请求失败（状态码 ${response.status}）`;
      if (response.status >= 400 && response.status < 500) {
        const err = new Error(message) as Error & { status: number; code?: string };
        err.status = response.status;
        if (typeof body?.code === "string") err.code = body.code;
        throw err;
      }
      const err5xx = new Error(message) as Error & { status: number; code?: string };
      err5xx.status = response.status;
      if (typeof body?.code === "string") err5xx.code = body.code;
      lastError = err5xx;
      if (attempt < retries) await new Promise((resolve) => setTimeout(resolve, 500 * (attempt + 1)));
    } catch (cause: unknown) {
      if (cause instanceof DOMException && cause.name === "AbortError") throw cause;
      if (cause instanceof TypeError) {
        lastError = new Error("无法连接到服务，请检查服务是否在运行。");
        if (attempt < retries) await new Promise((resolve) => setTimeout(resolve, 500 * (attempt + 1)));
        continue;
      }
      throw cause;
    }
  }
  throw lastError;
}

/** 安全地将未知值转换为可索引对象，用于 WebSocket 事件负载的防御性访问。
 *  返回 any 是为了兼容 .content[]/.entries() 等深度链式访问——这是有意的设计取舍。 */
export function asRecord(value: unknown): Record<string, any> {
  if (value === null || value === undefined) return Object.create(null) as Record<string, any>;
  if (typeof value !== "object") return Object.create(null) as Record<string, any>;
  if (Array.isArray(value)) return Object.create(null) as Record<string, any>;
  return value as Record<string, any>;
}
