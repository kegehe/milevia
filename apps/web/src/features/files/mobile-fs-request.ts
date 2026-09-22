/**
 * 手机端的文件取数适配器：把桌面端那套 REST 调用翻译成云端的中继请求。
 *
 * 为什么是这个形状：`FilesPanel` 从设计上只通过一个注入口取数
 * （`request: <T>(path, init) => Promise<T>`），所有 `/api/projects/{id}/fs/*` 都过它。
 * 所以在手机端**换掉这个注入口**，目录树 / 查看器 / 编辑器 / JSON / SQLite 预览
 * 就能整块复用 —— 不必为手机端再写一套文件界面。
 *
 * 通道侧对应的是 `POST /v1/instances/{id}/rpc`（文件与 Git 共用这一条；
 * 见 `features/remote/mobile-rpc.ts`）：
 *   { op, projectId, conversationId, params, timeoutMs? } -> { ok, status, data?, error? }
 * 其中 `status` 是该次操作的结果码（409 版本冲突 / 409 lease 占用 / 400 参数错）。
 * **HTTP 码只表示这条通道是否走通**，业务失败仍是 ok:false —— 见 docs/40 §7.1。
 */
import type { FileContent, FileEntry, FileInfo, TreeResponse } from "./file-model";
import type { MobileRpcReply, MobileRpcTransport } from "../remote/mobile-rpc";

// 这两个类型属于通道、不属于文件（Git 适配器也要用），定义在 features/remote/mobile-rpc.ts。
// 这里再导出一次是为了让既有引用（本文件的单测、手机页）不必改导入路径。
export type { MobileRpcReply, MobileRpcTransport };

/** `/fs/open` 的响应。字段与服务端 fsOpenResponse 对齐。 */
export interface MobileOpenPayload {
  stat: FileInfo;
  /** 键一定在、值可能是 null（内容被省略时）—— 所以类型必须是 `string | null`。 */
  content: string | null;
  encoding?: "base64";
  version: string;
  bytes: number;
  omittedReason?: "too_large" | "binary";
  /** 文件**本身**能否编辑的唯一判据。AI 运行中被锁是另一回事（写入时才判定）。 */
  editable: boolean;
  readOnlyReason?: "file_too_large" | "binary_file";
}

export interface MobileFsRequestOptions {
  transport: MobileRpcTransport;
  /**
   * 一次 `fs.tree` 拿几层。默认 3（服务端上限 5）。
   *
   * 取值理由：手机到电脑每次往返是几百毫秒到一秒，逐目录懒加载意味着每展开一层
   * 等一次。一次多拿几层之后，展开子目录由下面的**子树缓存**直接命中，零往返。
   */
  treeDepth?: number;
}

export interface MobileFsRequest {
  request: <T>(path: string, init?: RequestInit) => Promise<T>;
  /**
   * 丢掉全部缓存。页面上的「刷新」按钮必须调它 ——
   * 否则 AI 在电脑上改完文件、用户点刷新，适配器会拿缓存把旧内容还回去。
   */
  invalidateAll: () => void;
  /**
   * 把项目内的图片解析成可以放进 `<img>` 的字节。
   *
   * 手机端**不能**再拼 `/api/projects/{id}/fs/raw?path=…` 交给 `<img src>`：
   * 那是一次浏览器导航，带不了 `Authorization` 头，而手机的 WebView 也到不了
   * 电脑的本机端口。字节只能从中继取回来（`fs.open` 对小图片内联 base64），
   * 再由调用方转成 object URL。
   *
   * 拿不到时**不抛异常**，而是回一条给用户看的说明：图片加载不出来是常态
   * （超过内联上限、或不是图片），调用方要据此显示原因，而不是一句"加载失败"。
   */
  resolveMedia: (path: string) => Promise<MobileMediaResolution>;
}

export type MobileMediaResolution =
  | { kind: "ready"; base64: string; mimeType: string }
  | { kind: "unavailable"; message: string };

/** 内部拆出来的一条调用。 */
interface ParsedCall {
  route: FsRoute;
  query: URLSearchParams;
  body: Record<string, unknown>;
}

type FsRoute = keyof typeof ROUTES;

/**
 * 内容缓存最多留几条。取值只需覆盖"来回翻几个文件"这个尺度：一个文件在手机上
 * 翻来翻去是常态，但没人会同时需要十几份内容常驻。
 */
const openCacheLimit = 12;

// 路由表是**唯一**一份 REST → op 的映射。方法一并写死在这里：`init.method` 只用来
// 校验调用方没有写错，不作为取值来源（两处各说一遍必然有一天不同步）。
const ROUTES = {
  "/tree": { op: "fs.tree", method: "GET", kind: "query" },
  "/stat": { op: "fs.open", method: "GET", kind: "query" },
  "/read": { op: "fs.open", method: "GET", kind: "query" },
  "/search": { op: "fs.search", method: "GET", kind: "query" },
  "/sqlite/tables": { op: "fs.sqlite.tables", method: "GET", kind: "query" },
  "/sqlite/schema": { op: "fs.sqlite.schema", method: "GET", kind: "query" },
  "/sqlite/rows": { op: "fs.sqlite.rows", method: "GET", kind: "query" },
  "/write": { op: "fs.write", method: "PUT", kind: "body" },
  "/mkdir": { op: "fs.mkdir", method: "POST", kind: "body" },
  "/rename": { op: "fs.rename", method: "POST", kind: "body" },
  "/remove": { op: "fs.remove", method: "DELETE", kind: "query" },
} as const;

// 桌面端存在、但手机端**故意不支持**的端点。它们要的是"浏览器直接去取字节"
// （<img src> 或 <a download>），而那条路上带不了 Authorization 头，手机也到不了
// 电脑的本机端口。所以图片改走 /fs/open 的 base64，二进制则只给元信息。
const UNSUPPORTED: Record<string, string> = {
  "/raw": "手机端不支持按地址取原始文件，图片请通过文件内容预览",
  "/download": "手机端不提供文件下载，请在电脑上获取",
  "/download-ticket": "手机端不提供文件下载，请在电脑上获取",
};

/** 带机器可读 `code` 的错误，让调用方能按种类分支，而不是去匹配中文文案。 */
export class MobileFsError extends Error {
  readonly code: "content_omitted" | "unsupported" | "operation_failed" | "wiring";
  readonly status: number;
  readonly payload?: MobileOpenPayload;

  constructor(code: MobileFsError["code"], message: string, status: number, payload?: MobileOpenPayload) {
    super(message);
    this.name = "MobileFsError";
    this.code = code;
    this.status = status;
    this.payload = payload;
  }
}

/**
 * 认出"服务端省掉了内容"这件事（文件太大 / 不是文本）。
 *
 * 收成一个函数而不是让调用方自己 `instanceof` + 读 `code` + 读 `payload.omittedReason`：
 * 那是一串内部约定，抄在调用点上早晚会有一处抄错，而抄错的症状是**渲染出空文件**。
 */
export function contentOmittedFrom(error: unknown): { message: string; reason: "too_large" | "binary" } | null {
  if (!(error instanceof MobileFsError) || error.code !== "content_omitted") return null;
  return {
    message: error.message,
    reason: error.payload?.omittedReason === "binary" ? "binary" : "too_large",
  };
}

export function createMobileFsRequest(options: MobileFsRequestOptions): MobileFsRequest {
  const { transport } = options;
  const treeDepth = options.treeDepth ?? 3;

  // 内容缓存按**路径**索引：`/fs/stat` 与 `/fs/read` 都映射到 fs.open，
  // 没有这一层，打开一个文件就是两次往返（几百毫秒到一秒 ×2），白等一半。
  const openCache = new Map<string, MobileOpenPayload>();
  // 子树缓存：一次 fs.tree（带 depth）会拿到好几层，这里把它们**摊平**成
  // 「目录 → 该目录的直接子项」，于是后续 fetchDir("src/lib") 直接命中，零往返。
  // 摊平之后失效范围也能说清楚：某个目录的列表变了，只丢它自己那一条。
  const treeCache = new Map<string, TreeResponse>();

  function dropTree(path: string): void {
    treeCache.delete(path);
    // 受影响目录下面的条目也一起丢：目录被删掉/改名之后，它们已经不存在了。
    const prefix = path === "" ? "" : `${path}/`;
    for (const key of [...treeCache.keys()]) {
      if (path !== "" && key.startsWith(prefix)) treeCache.delete(key);
    }
  }

  function invalidateAfterMutation(call: ParsedCall): void {
    // 判据按"这次改动会让**哪个目录的列表**变化"来定，而不是一律清空：
    // 一律清空会让上一次展开的子树全丢，用户每删一个文件都要重新点回去。
    switch (call.route) {
      case "/write": {
        const path = stringValue(call.body.path);
        openCache.delete(path);
        // 新建文件会改变父目录的列表；覆盖写不会，但分不清楚更安全 ——
        // 丢一条缓存只是多一次往返，留着过期数据是错的。
        dropTree(parentDir(path));
        break;
      }
      case "/mkdir": {
        dropTree(parentDir(stringValue(call.body.path)));
        break;
      }
      case "/rename": {
        const oldPath = stringValue(call.body.oldPath);
        const newPath = stringValue(call.body.newPath);
        dropTree(parentDir(oldPath));
        dropTree(parentDir(newPath));
        dropTree(oldPath);
        // 重命名会改路径，两个路径上的内容缓存都要丢。
        openCache.delete(oldPath);
        openCache.delete(newPath);
        for (const key of [...openCache.keys()]) {
          if (oldPath !== "" && key.startsWith(`${oldPath}/`)) openCache.delete(key);
        }
        break;
      }
      case "/remove": {
        const path = stringValue(call.query.get("path"));
        dropTree(parentDir(path));
        dropTree(path);
        openCache.delete(path);
        for (const key of [...openCache.keys()]) {
          if (path !== "" && key.startsWith(`${path}/`)) openCache.delete(key);
        }
        break;
      }
      default:
        break;
    }
  }

  async function call(route: FsRoute, params: unknown): Promise<unknown> {
    const descriptor = ROUTES[route];
    const reply = await transport(descriptor.op, params);
    if (reply?.ok) return reply.data;
    // 业务失败：把服务端那句面向用户的中文**原样**抛出去（它已经是给用户看的），
    // 同时带上 status —— `FilesPanel` 靠 `status === 409` 走版本冲突分支。
    throw new MobileFsError("operation_failed", reply?.error || "操作失败", reply?.status ?? 0);
  }

  async function openFile(path: string): Promise<MobileOpenPayload> {
    const cached = openCache.get(path);
    if (cached) return cached;
    const payload = (await call("/read", { path })) as MobileOpenPayload | null;
    if (!payload || typeof payload !== "object" || !payload.stat) {
      throw new MobileFsError("operation_failed", "电脑端返回的文件信息无法解析", 200);
    }
    rememberOpen(path, payload);
    return payload;
  }

  /**
   * 记住一份打开过的内容，并把它挪到队尾。
   *
   * **必须有上限**：每条最多 320 KiB（图片 base64 后也在同一量级），手机端翻一遍
   * 目录就是几十兆 —— 而没有上限时这份缓存活到页面销毁为止。超出就丢最早打开的那条；
   * 丢掉只是多一次往返（下次打开会重新取），留着才是持续涨的内存。
   */
  function rememberOpen(path: string, payload: MobileOpenPayload): void {
    // 先删再插：Map 保持插入顺序，不这样做的话一个被反复打开的文件会一直停在
    // 队首，成为第一个被淘汰的对象 —— 恰好淘汰掉最常用的那个。
    openCache.delete(path);
    openCache.set(path, payload);
    while (openCache.size > openCacheLimit) {
      const oldest = openCache.keys().next().value;
      if (oldest === undefined) break;
      openCache.delete(oldest);
    }
  }

  /** 把一次带 depth 的响应摊平成「目录 → 直接子项」。 */
  function primeTree(dirPath: string, response: TreeResponse): void {
    treeCache.set(dirPath, {
      entries: (response.entries ?? []).map(withoutChildren),
      truncated: response.truncated ?? false,
      skippedDirs: response.skippedDirs ?? 0,
    });
    for (const entry of response.entries ?? []) {
      if (!entry.isDir || !entry.children) continue;
      primeTree(entry.path, { entries: entry.children });
    }
  }

  async function fetchTree(path: string): Promise<TreeResponse> {
    const cached = treeCache.get(path);
    if (cached) return cached;
    const params: Record<string, string> = { depth: String(treeDepth) };
    if (path) params.path = path;
    const response = (await call("/tree", params)) as TreeResponse;
    primeTree(path, response ?? { entries: [] });
    return treeCache.get(path) ?? { entries: [] };
  }

  return {
    invalidateAll(): void {
      openCache.clear();
      treeCache.clear();
    },

    async resolveMedia(path: string): Promise<MobileMediaResolution> {
      let payload: MobileOpenPayload;
      try {
        payload = await openFile(path);
      } catch (error) {
        // 传输层失败（电脑离线 / 超时）在这里变成一句可显示的说明：
        // 图片加载不出来不该把整页炸掉，尤其是 Markdown 里的一张插图。
        return { kind: "unavailable", message: error instanceof Error ? error.message : "读不到这个文件" };
      }
      if (payload.content === null) {
        return {
          kind: "unavailable",
          message:
            payload.omittedReason === "binary"
              ? `${payload.stat?.name || "这个文件"} 不是手机端能显示的图片`
              : omittedMessage(payload),
        };
      }
      if (payload.encoding !== "base64") {
        return { kind: "unavailable", message: `${payload.stat?.name || "这个文件"} 不是图片` };
      }
      return {
        kind: "ready",
        base64: payload.content,
        mimeType: payload.stat?.mimeType || "application/octet-stream",
      };
    },

    async request<T>(path: string, init?: RequestInit): Promise<T> {
      const parsed = parseFsCall(path, init);
      switch (parsed.route) {
        case "/tree":
          return (await fetchTree(parsed.query.get("path") ?? "")) as T;

        // stat 与 read 都命中同一份 fs.open：一次往返就够。
        // 桌面端拆成两跳是为了先看 isText/mimeType 决定怎么渲染，
        // 而 /fs/open 已经把这两件事合在一次答复里了。
        case "/stat":
          return (await openFile(stringValue(parsed.query.get("path")))).stat as T;

        case "/read": {
          const payload = await openFile(stringValue(parsed.query.get("path")));
          if (payload.content === null) {
            // **绝不返回空内容冒充空文件**：内容被省略时若把 "" 交出去，
            // 查看器会渲染出一个空文件，用户以为文件是空的。
            // 带上 payload，让查看器（后续实现）能据此渲染元信息卡。
            throw new MobileFsError("content_omitted", omittedMessage(payload), 200, payload);
          }
          const content: FileContent = {
            content: payload.content,
            version: payload.version,
            stat: payload.stat,
          };
          // encoding 只在真的是 base64 时才挂上去：服务端那个字段带 omitempty，
          // 这里无条件写个 `encoding: undefined` 会让"键在不在"与电脑端不一致
          // （`"encoding" in payload` 这类判断会得到相反的结果）。
          if (payload.encoding) content.encoding = payload.encoding;
          // 「能不能改」必须由服务端说了算，**原样透传**：它取决于内容发不发得回去
          // （要减掉 JSON 转义与中继信封的余量），客户端按扩展名自己判会多标出
          // 一条 256–320 KiB 的可编辑带 —— 用户改完按保存才失败。
          if (typeof payload.editable === "boolean") content.editable = payload.editable;
          if (payload.readOnlyReason) content.readOnlyReason = payload.readOnlyReason;
          return content as T;
        }

        case "/search":
          return (await call("/search", queryObject(parsed.query))) as T;

        case "/sqlite/tables":
        case "/sqlite/schema":
        case "/sqlite/rows":
          return (await call(parsed.route, queryObject(parsed.query))) as T;

        case "/write":
        case "/mkdir":
        case "/rename": {
          const result = await call(parsed.route, parsed.body);
          invalidateAfterMutation(parsed);
          return result as T;
        }

        case "/remove": {
          const result = await call(parsed.route, queryObject(parsed.query));
          invalidateAfterMutation(parsed);
          return result as T;
        }
      }
      // 路由表里加了端点、switch 里忘了接，这里必须炸出来。
      // 少了这句，新端点会静默返回 undefined，调用方只会看到"数据是空的"。
      throw new MobileFsError("wiring", `没有实现这个文件端点：${parsed.route}`, 0);
    },
  };
}

/** 摊平之后 children 没用了，留着会让每次读缓存都重新遍历一遍深树。 */
function withoutChildren(entry: FileEntry): FileEntry {
  if (!entry.children) return entry;
  const { children: _dropped, ...rest } = entry;
  return rest;
}

function omittedMessage(payload: MobileOpenPayload): string {
  const name = payload.stat?.name || "这个文件";
  if (payload.omittedReason === "binary") {
    return `${name} 不是文本文件，手机端只能显示它的基本信息`;
  }
  return `${name} 太大（${formatBytes(payload.stat?.size ?? 0)}），无法在手机上打开，请在电脑上查看`;
}

function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return "—";
  if (bytes < 1024) return `${bytes} B`;
  const units = ["KB", "MB", "GB"];
  let value = bytes / 1024;
  let index = 0;
  while (value >= 1024 && index < units.length - 1) {
    value /= 1024;
    index += 1;
  }
  return `${value.toFixed(1)} ${units[index]}`;
}

function stringValue(value: unknown): string {
  return typeof value === "string" ? value : "";
}

function parentDir(path: string): string {
  const slash = path.lastIndexOf("/");
  return slash >= 0 ? path.slice(0, slash) : "";
}

function queryObject(query: URLSearchParams): Record<string, string> {
  const result: Record<string, string> = {};
  for (const [key, value] of query) result[key] = value;
  return result;
}

const FS_PATH = /^\/api\/projects\/[^/]+\/fs(\/[^?]*)?(?:\?(.*))?$/;

function parseFsCall(path: string, init?: RequestInit): ParsedCall {
  const match = FS_PATH.exec(path);
  if (!match) {
    throw new MobileFsError("wiring", `不是文件系统的请求：${path}`, 0);
  }
  const suffix = match[1] ?? "";
  if (UNSUPPORTED[suffix]) {
    throw new MobileFsError("unsupported", UNSUPPORTED[suffix], 0);
  }
  if (!(suffix in ROUTES)) {
    throw new MobileFsError("wiring", `手机端没有映射这个文件端点：${suffix || "/"}`, 0);
  }
  const route = suffix as FsRoute;
  const descriptor = ROUTES[route];
  if (init?.method && init.method.toUpperCase() !== descriptor.method) {
    // 方法写错是接线错误，必须当场炸出来：静默按表里的方法发出去，
    // 会让一个本该报错的调用看起来"成功了"，直到用户发现数据没变。
    throw new MobileFsError(
      "wiring",
      `文件端点 ${route} 的方法是 ${descriptor.method}，收到的是 ${init.method}`,
      0,
    );
  }
  if (descriptor.kind === "body") {
    return { route, query: new URLSearchParams(), body: parseBody(init) };
  }
  const query = new URLSearchParams(match[2] ?? "");
  // conversationId 由页面在构造这个适配器时定死（文件视图是绑工作区的）。
  // 从查询里剔掉而不是原样透传：服务端会把它当**工作区**参数，而不是文件路径参数 ——
  // 留在 params 里会让它出现在不该出现的地方（比如作为 fs.write 的请求体字段）。
  query.delete("conversationId");
  return { route, query, body: {} };
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
  throw new MobileFsError("wiring", "文件写入的请求体必须是 JSON 对象", 0);
}
