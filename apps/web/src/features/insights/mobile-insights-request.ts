/**
 * 手机端的优化建议取数适配器：把桌面端那套 REST 调用翻译成云端的中继请求。
 *
 * 与 `features/files/mobile-fs-request.ts` 同一套打法：`InsightsPanel` 从设计上只通过
 * 一个注入口取数（`request: <T>(path, init?) => Promise<T>`），所有
 * `/api/projects/{id}/insights*` 都过它。所以在手机端**换掉这个注入口**，列表 / 扫描 /
 * 复核 / 转任务 / 编辑 / 删除 / 不再提示就能整块复用 —— 不必为手机端再写一套建议界面。
 *
 * 通道侧对应 `POST /v1/instances/{id}/rpc`（fs / git / conversation / insights 共用一条，
 * 见 `features/remote/mobile-rpc.ts`）：
 *   { op, projectId, conversationId, params, timeoutMs? } -> { ok, status, data?, error? }
 * op 名单与路径参数校验在电脑端 remote_insights.go；云端不解析 op。
 *
 * 手机端没有同源 /api 通道（原生 WebView 打到资源源只会拿到 SPA index.html），
 * 这个适配器是建议面板在手机上唯一的取数方式。
 */
import type { MobileRpcReply, MobileRpcTransport } from "../remote/mobile-rpc";

export interface MobileInsightsRequestOptions {
  transport: MobileRpcTransport;
}

export interface MobileInsightsRequest {
  request: <T>(path: string, init?: RequestInit) => Promise<T>;
}

/** 内部拆出来的一次调用：发哪个 op、带什么 params。 */
interface ParsedCall {
  op: string;
  params: unknown;
}

/**
 * 集合路由表：REST 后缀 → op。方法写死在这里，`init.method` 只用来校验调用方没有
 * 写错，不作为取值来源（两处各说一遍必然有一天不同步）。
 *
 * GET 走查询串（中继层查询类 op 只收字符串映射），其余走 JSON 请求体 ——
 * 与电脑端 remote_insights.go 的 Query 声明一一对应。
 */
const ROUTES = {
  "": { op: "insights.list", method: "GET" },
  "/scan": { op: "insights.scan", method: "POST" },
  "/cancel": { op: "insights.cancel", method: "POST" },
  "/verify": { op: "insights.verify", method: "POST" },
  "/to-task": { op: "insights.toTask", method: "POST" },
  "/delete": { op: "insights.delete", method: "POST" },
} as const;

/**
 * 单条建议路由：URL 是 `/insights/{findingID}[/to-task|/dismiss]`，findingID 会被
 * 电脑端拼进本地路由（PathParams 白名单 + UUID 校验），所以只能从 params 的声明位
 * 传下去，体里那份到不了 URL。后缀相同的先判（to-task / dismiss 在集合表里也有
 * 同名片段），剩下的按方法区分：PATCH 是编辑，DELETE 是删除。
 */
function matchFindingRoute(suffix: string, method: string): string | undefined {
  if (suffix.endsWith("/to-task")) return method === "POST" ? "insights.findingToTask" : undefined;
  if (suffix.endsWith("/dismiss")) {
    if (method === "POST") return "insights.findingDismiss";
    if (method === "DELETE") return "insights.findingDismissClear";
    return undefined;
  }
  if (method === "PATCH") return "insights.findingUpdate";
  if (method === "DELETE") return "insights.findingDelete";
  return undefined;
}

const INSIGHTS_PATH = /^\/api\/projects\/[^/]+\/insights(\/[^?]*)?(?:\?(.*))?$/;

/**
 * 创建适配器。返回的 `request` 是闭包方法（身份稳定），但仍应由调用方 useMemo 包一层，
 * 让它只在 transport 变化（换实例 / 换项目 / 换会话）时重建。
 */
export function createMobileInsightsRequest(options: MobileInsightsRequestOptions): MobileInsightsRequest {
  const { transport } = options;

  async function call(op: string, params: unknown): Promise<unknown> {
    const reply: MobileRpcReply = await transport(op, params);
    if (reply?.ok) return reply.data;
    // 业务失败：把服务端那句面向用户的中文**原样**抛出去（它已经是给用户看的）。
    // InsightsPanel 只按 message 展示，不按 HTTP 码分支 —— 与文件适配器要区分
    // 409 版本冲突不同，这里没有按码走的需求，Error 一层就够。
    throw new Error(reply?.error || "操作失败");
  }

  return {
    async request<T>(path: string, init?: RequestInit): Promise<T> {
      const parsed = parseInsightsCall(path, init);
      return (await call(parsed.op, parsed.params)) as T;
    },
  };
}

function parseInsightsCall(path: string, init?: RequestInit): ParsedCall {
  const match = INSIGHTS_PATH.exec(path);
  if (!match) {
    throw new Error(`不是优化建议的请求：${path}`);
  }
  const suffix = match[1] ?? "";
  const method = (init?.method ?? "GET").toUpperCase();

  if (suffix in ROUTES) {
    const route = suffix as keyof typeof ROUTES;
    const descriptor = ROUTES[route];
    if (method !== descriptor.method) {
      // 方法写错是接线错误，必须当场炸出来：静默按表里的方法发出去，
      // 会让一个本该报错的调用看起来"成功了"，直到用户发现数据没变。
      throw new Error(`优化建议端点 ${route || "/"} 的方法是 ${descriptor.method}，收到的是 ${method}`);
    }
    if (route === "") {
      // 列表的游标轮询：sinceScan / sinceSeq 必须是字符串映射（中继层查询类 op
      // 只收字符串，数字会被明确拒绝）。没有游标就发空 params。
      const query = new URLSearchParams(match[2] ?? "");
      const params: Record<string, string> = {};
      const sinceScan = query.get("sinceScan");
      const sinceSeq = query.get("sinceSeq");
      if (sinceScan) params.sinceScan = sinceScan;
      if (sinceSeq) params.sinceSeq = sinceSeq;
      return { op: descriptor.op, params };
    }
    return { op: descriptor.op, params: parseBody(init) };
  }

  const op = matchFindingRoute(suffix, method);
  if (!op) {
    throw new Error(`手机端没有映射这个优化建议端点：${suffix || "/"}（${method}）`);
  }
  const findingID = decodeURIComponent(suffix.replace(/\/(to-task|dismiss)$/, "").replace(/^\//, ""));
  return { op, params: { ...parseBody(init), findingID } };
}

function parseBody(init?: RequestInit): Record<string, unknown> {
  if (typeof init?.body !== "string" || init.body === "") return {};
  try {
    const parsed: unknown = JSON.parse(init.body);
    if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
      return parsed as Record<string, unknown>;
    }
  } catch {
    // 落到下面统一报错：接线错误要说清是哪一处，不要吞掉。
  }
  throw new Error("优化建议的请求体必须是 JSON 对象");
}
