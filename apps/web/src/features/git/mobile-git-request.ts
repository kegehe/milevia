/**
 * 手机端的 Git 取数适配器：把桌面端那套 REST 调用翻译成云端的中继请求。
 *
 * 为什么是这个形状：`GitWorkbench` 从设计上只通过一个注入口取数
 * （`request: <T>(path, init) => Promise<T>`，见 `features/git/GitWorkbench.tsx:46`），
 * 所有 `/api/projects/{id}/git/*` 都过它。所以在手机端**换掉这个注入口**，
 * 变更列表 / diff 渲染 / 提交历史 / 提交详情 / 分支 / 操作记录 / 冲突解决
 * 就能整块复用 —— 不必为手机端再写一套 Git 界面。
 *
 * 与文件适配器（`features/files/mobile-fs-request.ts`）同构，三处**有意的不同**：
 *
 *  1. **写操作前无条件换一次 `stateToken`**（见 §3.4 of docs/41）。服务端的乐观锁
 *     令牌只有 2 分钟有效期，而手机上"看一眼 diff → 想一想 → 点提交"很容易越过去。
 *     令牌换取一次 `git.summary` 的代价，换掉"提交时报一句英文"。
 *  2. **超时按 op 分档**：读 20s / 本地写 45s / 网络写 60s。服务端单条 git 命令上限
 *     是 30s（`git.go` 的 gitCommandTimeout），20s 的默认值会让 push/fetch **必然超时**。
 *     云端不认识 op，所以这个数由这里提议、由云端夹取。
 *  3. **不缓存任何内容**。Git 状态变得比文件内容快得多（AI 每改一个文件都会动它），
 *     缓存 diff 只会把过期内容当新的给用户看。**连令牌也不留**：写操作一律现取一次，
 *     多出来的那次往返换掉"一个魔法数 + 一类漏判"，比留一份可能过期的令牌划算。
 *     所以这里没有 `invalidateAll` 那种"清缓存"的出口 —— 没有东西可清。
 */
import type { MobileRpcReply, MobileRpcTransport } from "../remote/mobile-rpc";

/** 只读操作：本地一条 git 命令是毫秒级，SSH 项目要算 SFTP 往返。 */
const READ_TIMEOUT_MS = 20_000;
/** 本地写操作：服务端单条 git 命令上限 30s，加上库与通道余量。 */
const LOCAL_WRITE_TIMEOUT_MS = 45_000;
/** 网络写操作（fetch / pull / push）：同样受 30s 命令上限约束，多留远端握手的余量。 */
const NETWORK_WRITE_TIMEOUT_MS = 60_000;

type GitMethod = "GET" | "POST";

interface Route {
  /** `/api/projects/{id}/git` 之后的路径模板，可含 `{name}` 段。 */
  pattern: string;
  op: string;
  method: GitMethod;
  /** query：params 是查询参数；body：params 是 JSON 请求体。 */
  kind: "query" | "body";
  timeoutMs: number;
  /** 该操作带服务端的 stateToken 乐观锁 ⇒ 发之前必须换一个新鲜的（见文件头 §1）。 */
  needsStateToken?: boolean;
}

/**
 * REST → op 的映射是**唯一一份**，方法也写死在这里。
 *
 * 顺序有意义：**字面路径必须排在占位符前面**。`/commits/amend` 与 `/commits/{oid}`
 * 都是两段，谁先匹配谁赢 —— 让 `{oid}` 先试，amend 就会被当成一个 oid 送去读提交。
 * （方法不同也能区分这两条，但那是第二个保险，不是第一个理由。）
 */
const ROUTES: Route[] = [
  // ── 只读 ────────────────────────────────────────────────────────────────
  { pattern: "/summary", op: "git.summary", method: "GET", kind: "query", timeoutMs: READ_TIMEOUT_MS },
  { pattern: "/changes", op: "git.changes", method: "GET", kind: "query", timeoutMs: READ_TIMEOUT_MS },
  { pattern: "/diff", op: "git.diff", method: "GET", kind: "query", timeoutMs: READ_TIMEOUT_MS },
  { pattern: "/log", op: "git.log", method: "GET", kind: "query", timeoutMs: READ_TIMEOUT_MS },
  { pattern: "/operations", op: "git.operations", method: "GET", kind: "query", timeoutMs: READ_TIMEOUT_MS },
  { pattern: "/branches", op: "git.branches", method: "GET", kind: "query", timeoutMs: READ_TIMEOUT_MS },
  { pattern: "/conflicts", op: "git.conflicts", method: "GET", kind: "query", timeoutMs: READ_TIMEOUT_MS },
  { pattern: "/conflicts/content", op: "git.conflictContent", method: "GET", kind: "query", timeoutMs: READ_TIMEOUT_MS },
  { pattern: "/conflicts/suggestions/{suggestionID}", op: "git.suggestion", method: "GET", kind: "query", timeoutMs: READ_TIMEOUT_MS },
  { pattern: "/commits/{oid}", op: "git.commit", method: "GET", kind: "query", timeoutMs: READ_TIMEOUT_MS },
  { pattern: "/commits/{oid}/diff", op: "git.commitDiff", method: "GET", kind: "query", timeoutMs: READ_TIMEOUT_MS },

  // ── 本地写（字面路径必须先于上面的占位符被考虑，见 ROUTES 的说明）──────────
  { pattern: "/commits/amend", op: "git.commitAmend", method: "POST", kind: "body", timeoutMs: LOCAL_WRITE_TIMEOUT_MS, needsStateToken: true },
  { pattern: "/commits", op: "git.commitCreate", method: "POST", kind: "body", timeoutMs: LOCAL_WRITE_TIMEOUT_MS, needsStateToken: true },
  { pattern: "/stage", op: "git.stage", method: "POST", kind: "body", timeoutMs: LOCAL_WRITE_TIMEOUT_MS, needsStateToken: true },
  { pattern: "/unstage", op: "git.unstage", method: "POST", kind: "body", timeoutMs: LOCAL_WRITE_TIMEOUT_MS, needsStateToken: true },
  { pattern: "/stage-all", op: "git.stageAll", method: "POST", kind: "body", timeoutMs: LOCAL_WRITE_TIMEOUT_MS, needsStateToken: true },
  { pattern: "/unstage-all", op: "git.unstageAll", method: "POST", kind: "body", timeoutMs: LOCAL_WRITE_TIMEOUT_MS, needsStateToken: true },
  { pattern: "/discard", op: "git.discard", method: "POST", kind: "body", timeoutMs: LOCAL_WRITE_TIMEOUT_MS, needsStateToken: true },
  { pattern: "/switch", op: "git.switch", method: "POST", kind: "body", timeoutMs: LOCAL_WRITE_TIMEOUT_MS, needsStateToken: true },
  // 新建分支与只读的分支列表**共用同一个路径**，靠方法区分（`gitCreateBranch` 不需要令牌）。
  { pattern: "/branches", op: "git.createBranch", method: "POST", kind: "body", timeoutMs: LOCAL_WRITE_TIMEOUT_MS },
  { pattern: "/conflicts/resolve", op: "git.conflictResolve", method: "POST", kind: "body", timeoutMs: LOCAL_WRITE_TIMEOUT_MS, needsStateToken: true },
  { pattern: "/conflicts/abort", op: "git.conflictAbort", method: "POST", kind: "body", timeoutMs: LOCAL_WRITE_TIMEOUT_MS, needsStateToken: true },
  { pattern: "/conflicts/continue", op: "git.conflictContinue", method: "POST", kind: "body", timeoutMs: LOCAL_WRITE_TIMEOUT_MS, needsStateToken: true },
  // 生成建议是异步的（立刻 202 + 轮询 git.suggestion），所以按读的档位等。
  { pattern: "/conflicts/suggest", op: "git.conflictSuggest", method: "POST", kind: "body", timeoutMs: READ_TIMEOUT_MS },
  { pattern: "/conflicts/suggestions/{suggestionID}/cancel", op: "git.suggestionCancel", method: "POST", kind: "body", timeoutMs: LOCAL_WRITE_TIMEOUT_MS },

  // ── 网络写 ──────────────────────────────────────────────────────────────
  { pattern: "/fetch", op: "git.fetch", method: "POST", kind: "body", timeoutMs: NETWORK_WRITE_TIMEOUT_MS, needsStateToken: true },
  { pattern: "/pull", op: "git.pull", method: "POST", kind: "body", timeoutMs: NETWORK_WRITE_TIMEOUT_MS, needsStateToken: true },
  { pattern: "/push", op: "git.push", method: "POST", kind: "body", timeoutMs: NETWORK_WRITE_TIMEOUT_MS, needsStateToken: true },
];

/**
 * 失败的种类。
 *
 * 判据**优先落在服务端给的稳定错误码上**（`reply.code`，来自 control-server 的
 * `httpErrorCode`），只有没带码的失败才退回去匹配文案。
 *
 * 为什么不能只匹配文案：`writeError` 会把面向用户的句子**本地化**
 * （control-server 的 `localizedErrorText`）。"project workspace is occupied by
 * another run or Git operation" 到了手机上已经是"项目工作区正被其他 AI 任务或 Git
 * 操作占用…" —— 按原文匹配的那条分支在真实链路上**永远不命中**，而单测喂原文照样绿。
 * 这正是本项目反复强调的"判据必须量真正发出去的那个东西"。
 */
export type MobileGitFailureKind =
  | "stale_state"
  | "workspace_busy"
  | "changes_gone"
  | "nothing_to_do"
  | "runner_offline"
  | "other";

/** 稳定错误码 → 种类。码表来自 control-server 的 httpErrorCode，改那边要同时看这里。 */
const FAILURE_CODES: Record<string, MobileGitFailureKind> = {
  workspace_occupied: "workspace_busy",
};

/**
 * 没有码的失败只能匹配文案。**它们都是"会原样上路"的那一类**：
 * 未登记进翻译表的英文句子会被 `localizedErrorText` 变成 `中文兜底句：原文`，
 * 原文仍以子串形式存在，所以这些 needle 依然命中。
 *
 * 两条纪律：① 只有确认过"服务端不会整句替换它"的句子才能写在这里；
 * ② 服务端一旦给某条加了翻译，这里必须跟着改 —— 否则是静默失效，不会有任何测试红。
 */
const FAILURE_MATCHERS: Array<{ kind: MobileGitFailureKind; needle: string }> = [
  { kind: "stale_state", needle: "Git state changed" },
  // 同时留英文与中文两种写法：中文那份是当前真实上路的形态（服务端有翻译条目），
  // 英文那份留着是因为它本来就是服务端那侧的原文，将来本地化链若被绕过仍然有效。
  { kind: "workspace_busy", needle: "project workspace is occupied" },
  { kind: "workspace_busy", needle: "项目工作区正被其他 AI 任务或 Git 操作占用" },
  { kind: "changes_gone", needle: "no longer available" },
  { kind: "nothing_to_do", needle: "no eligible Git changes" },
  { kind: "runner_offline", needle: "runner_offline" },
];

export function classifyGitFailure(message: string, code?: string): MobileGitFailureKind {
  if (code) {
    const byCode = FAILURE_CODES[code];
    if (byCode) return byCode;
  }
  for (const matcher of FAILURE_MATCHERS) {
    if (message.includes(matcher.needle)) return matcher.kind;
  }
  return "other";
}

/**
 * 「仓库状态已变化」这条提示的原文。
 *
 * 它同时是两样东西：发给宿主的**可恢复提示**，以及抛出去那个错误的 `message`。
 * 两边必须**逐字相同** —— 宿主靠这个常量认出"这件事我已经用提示说过了"，
 * 从而不在红色错误条里重复一遍（那里原来会出现一句"中文兜底句：Git state changed…"）。
 */
export const STALE_STATE_NOTICE = "仓库状态已经变化（可能是电脑上有别的改动），已为你刷新，请确认后再提交";

/** 带机器可读 `code` 的错误，让调用方能按种类分支，而不是去匹配中文文案。 */
export class MobileGitError extends Error {
  readonly code: "operation_failed" | "wiring" | MobileGitFailureKind;
  readonly status: number;

  constructor(code: MobileGitError["code"], message: string, status: number) {
    super(message);
    this.name = "MobileGitError";
    this.code = code;
    this.status = status;
  }
}

export interface MobileGitRequestOptions {
  transport: MobileRpcTransport;
  /**
   * 服务端说"仓库状态已变化"时调它，由宿主（Git 视图）去重载并告诉用户。
   *
   * 适配器**不能**自己重载：`GitWorkbench` 的 `reload()` 在组件里，适配器拿不到。
   * 所以这里只发一个信号，传一句已经写好的话给宿主显示 ——
   * 它是**可恢复的提示**，不该被塞进 `fail()` 的错误条（那会把整屏盖掉）。
   */
  onStale?: (message: string) => void;
  /**
   * 那条可恢复提示**已经不需要了**（用户重试了一次写操作并且成功）。
   *
   * 为什么需要这个出口：提示是绿色的、不带错误语义，若没有人来撤它，
   * 它就一直挂在屏幕上 —— 用户看到的是"已为你刷新，请确认后再提交"，
   * 而实际上早就提交成功了。
   *
   * 只在**写操作成功**时触发，不是"任何请求成功"：那段失败之后宿主会立刻重载仓库
   * （一串只读请求），只读成功就把提示撤掉的话，它只会闪 300 毫秒。
   */
  onRecovered?: () => void;
}

export interface MobileGitRequest {
  request: <T>(path: string, init?: RequestInit) => Promise<T>;
}

interface ParsedCall {
  route: Route;
  pathParams: Record<string, string>;
  query: URLSearchParams;
  body: Record<string, unknown>;
}

const GIT_PATH = /^\/api\/projects\/[^/]+\/git(\/[^?]*)?(?:\?(.*))?$/;

export function createMobileGitRequest(options: MobileGitRequestOptions): MobileGitRequest {
  const { transport, onStale, onRecovered } = options;
  // "上一条可恢复提示还挂着吗"。只有写操作成功才撤（见 onRecovered 的说明）。
  let stalePending = false;

  async function callRaw(route: Route, params: unknown): Promise<unknown> {
    const reply: MobileRpcReply = await transport(route.op, params, route.timeoutMs);
    if (reply?.ok) {
      if (stalePending && route.needsStateToken) {
        stalePending = false;
        onRecovered?.();
      }
      return reply.data;
    }
    const message = reply?.error || "操作失败";
    // 码优先，文案兜底 —— 服务端会把文案本地化，见 classifyGitFailure 的说明。
    throw new MobileGitError(classifyGitFailure(message, reply?.code), message, reply?.status ?? 0);
  }

  /**
   * 为一次写操作换一个新令牌。
   *
   * **无条件换**，不看时钟：写操作本就是用户主动、低频的动作，多付一次往返
   * （几百毫秒）换掉"一个魔法数 + 一类漏判"是划算的。
   * 万一实测证明大仓库上这次额外读太慢，**再**引入时间阈值 —— 那时它是有测量支撑的。
   */
  async function fetchStateToken(): Promise<string> {
    const summary = (await callRaw(routeForPattern("/summary"), {})) as { stateToken?: string } | null;
    return typeof summary?.stateToken === "string" ? summary.stateToken : "";
  }

  return {
    async request<T>(path: string, init?: RequestInit): Promise<T> {
      const parsed = parseGitCall(path, init);

      if (parsed.route.needsStateToken) {
        // 令牌由服务端签发、2 分钟有效，且**绑的是当时那份仓库状态**。带着界面上那份
        // 旧令牌去写，最常见的结局是 409 + 一句服务端文案。这里换成新鲜的再发。
        const token = await fetchStateToken();
        parsed.body = { ...parsed.body, stateToken: token };
      }

      const params = parsed.route.kind === "query" ? queryObject(parsed.query, parsed.pathParams) : parsed.body;
      try {
        return (await callRaw(parsed.route, params)) as T;
      } catch (error) {
        if (error instanceof MobileGitError && error.code === "stale_state") {
          // 换了新令牌还是过期 ⇒ 电脑上的仓库在这几百毫秒里真的变了
          // （多半是 AI 正在跑、或者别处刚提交）。这一档是可恢复的：
          // 让宿主去重载，并说清"已为你刷新、请确认后再提交"。
          //
          // 抛出去的那条错误**用同一句话**：宿主会拿它去比对，从而不在红色错误条里
          // 重复一遍（服务端原文到了这里已经变成"中文兜底句：Git state changed…"）。
          stalePending = true;
          onStale?.(STALE_STATE_NOTICE);
          throw new MobileGitError("stale_state", STALE_STATE_NOTICE, error.status);
        }
        throw error;
      }
    },
  };
}

/** 按模板取一条路由。只给适配器内部用（如取 `/summary`），不参与 REST 解析。 */
function routeForPattern(pattern: string): Route {
  const route = ROUTES.find((item) => item.pattern === pattern);
  if (!route) throw new MobileGitError("wiring", `适配器里没有这条路：${pattern}`, 0);
  return route;
}

/**
 * 把一次 REST 调用解析成 op 调用。
 *
 * 匹配**先按方法过滤**，再按声明顺序试模板：两条路由共用同一路径（`/branches` 的
 * 只读与新建）或共享同一形状（`/commits/amend` 与 `/commits/{oid}`）时，
 * 只有"方法 + 顺序"这两条同时成立才不会串味。
 */
export function parseGitCall(path: string, init?: RequestInit): ParsedCall {
  const match = GIT_PATH.exec(path);
  if (!match) {
    throw new MobileGitError("wiring", `不是 Git 工作台的请求：${path}`, 0);
  }
  const suffix = match[1] ?? "";
  const method = (init?.method ?? "GET").toUpperCase() as GitMethod;
  const segments = splitSegments(suffix);

  for (const route of ROUTES) {
    if (route.method !== method) continue;
    const pathParams = matchPattern(route.pattern, segments);
    if (!pathParams) continue;
    if (route.kind === "body") {
      return { route, pathParams, query: new URLSearchParams(), body: { ...pathParams, ...parseBody(init) } };
    }
    const query = new URLSearchParams(match[2] ?? "");
    // conversationId 由页面在构造这个适配器时定死（Git 视图是绑工作区的，
    // 而多会话可能各自落在不同 worktree 上）。从查询里剔掉而不是原样透传：
    // 服务端会把它当**工作区**参数，而 params 是 op 自己的参数袋 ——
    // 留在里面就会出现在不该出现的地方（比如成为 git 提交的请求体字段）。
    query.delete("conversationId");
    return { route, pathParams, query, body: {} };
  }

  // 路径认得出来、但没有哪个 (路径, 方法) 组合对应得上：这是接线错误，
  // 必须当场炸出来。静默返回 undefined 的话，调用方只会看到"数据是空的"。
  throw new MobileGitError("wiring", `手机端没有映射这个 Git 端点：${method} ${suffix || "/"}`, 0);
}

function splitSegments(suffix: string): string[] {
  return suffix.split("/").filter((segment) => segment !== "");
}

/** 模板匹配；命中时返回捕获到的路径参数，否则 null。 */
function matchPattern(pattern: string, segments: string[]): Record<string, string> | null {
  const expected = splitSegments(pattern);
  if (expected.length !== segments.length) return null;
  const captured: Record<string, string> = {};
  for (let index = 0; index < expected.length; index += 1) {
    const part = expected[index];
    if (part.startsWith("{") && part.endsWith("}")) {
      captured[part.slice(1, -1)] = decodeURIComponent(segments[index]);
      continue;
    }
    if (part !== segments[index]) return null;
  }
  return captured;
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
  throw new MobileGitError("wiring", "Git 写入的请求体必须是 JSON 对象", 0);
}

function queryObject(query: URLSearchParams, pathParams: Record<string, string>): Record<string, string> {
  const result: Record<string, string> = { ...pathParams };
  for (const [key, value] of query) result[key] = value;
  return result;
}
