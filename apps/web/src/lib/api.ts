// API 请求封装 — 从 App.tsx 提取

import { apiURL, sessionHeaders } from "./runtime";

/**
 * 普通请求的等待上限。
 *
 * 它同时是一道**健康探针**：服务端忙到 15 秒还不回话（单 SQLite 连接被长事务占住、
 * SSH/WSL 慢探测）时，早一点说出来比让用户盯着转圈好。所以这个数**不该**为了
 * 迁就长任务而放大 —— 长任务由下面那张表单独领预算。
 */
const requestTimeoutMs = 15_000;

/** 探测类长任务：服务端自己有 10~45 秒的预算（registry 查询、逐工具探测）。 */
const probeTimeoutMs = 60_000;
/** 外部进程/网络类长任务：服务端按 30 秒~2 分钟设计（单条 git 命令、MCP 探针、SSH 握手、UAC 等待）。 */
const externalTaskTimeoutMs = 120_000;
/** 安装类长任务：服务端按 15 分钟设计（`Config.AgentUpdateTimeout`，见 `defaultAgentUpdateTimeout`）。 */
const installTimeoutMs = 16 * 60_000;

/**
 * 长任务端点表 —— **唯一一份**。
 *
 * 为什么需要它：`api()` 原先对所有请求都用同一个 15 秒预算，而服务端有一整族端点的
 * **设计上限是分钟级**（`AUTO_AGENT_UPDATE_TIMEOUT` 默认 15 分钟、`gitCommandTimeout` 30 秒、
 * `agentProbeTimeout` 45 秒、MCP 探针 25 秒、终端 UAC 2 分钟……）。两边的预算一错位，
 * 症状就是**执行超过 15 秒的操作被判成失败**：装/升级 CLI、跑 git 写操作、诊断、SSH
 * 连接，全都会在界面上报一句"控制服务未在 15 秒内响应"，而其中一部分（走
 * `s.runtimeCtx` 的那些）服务端其实还在跑、最后是成功的 —— 用户看到的是假失败，
 * 更糟的是他会据此**再点一次**。
 *
 * 入表的判据只有一条：**服务端能在这件事上合法地花超过 15 秒**。它有两种成立方式，
 * 每条都要指得出依据，指不出就别放进来 —— 一张"看着像长任务"的表会把真正的卡死
 * （服务端真挂了）也拖到分钟级才报，那正是上面那个 15 秒要防的事：
 *   - 服务端给了**明确预算**（git 命令 30s、MCP 探针 25s、CLI 装/升级 15min……）；
 *   - 或者它**根本没有预算**，花多久都算正常（全树遍历 `fs/search`、要打多次外部
 *     网络往返的 OAuth 起始）。这一档同样不能用 15 秒去判失败。
 *
 * 分档照抄服务端的预算量级，不搞"一律给满"：
 *   - `probeTimeoutMs`  ｜ registry 往返、逐工具探测
 *   - `externalTaskTimeoutMs` ｜ 单条 git 命令（30s）、MCP 探针（25s）、SSH 握手、终端 UAC（2min）
 *   - `installTimeoutMs` ｜ npm 全局安装/升级、运行时下载（15min）
 *
 * ⚠️ **必须带方法**。同一条路径上往往同时挂着读与写（`GET /api/projects` 列项目、
 * `POST /api/projects` 建项目；`GET /git/branches` 列分支、`POST /git/branches` 建分支；
 * `GET /api/ssh-connections` 列连接、`POST` 建连接）。只按路径匹配的话，那些**快**的读
 * 会跟着拿到分钟级预算 —— 那既丢掉了 15 秒的早发现，又让它们失去"重试一次"的资格。
 */
const longRunningEndpoints: Array<{ methods: string[]; pattern: RegExp; timeoutMs: number }> = [
	// ── 装 / 升级 / 修复 / 运行时 ── 服务端一律取 agentUpdateTimeout（默认 15 分钟）
	{ methods: ["POST"], pattern: /^\/api\/runners\/[^/]+\/(agents\/[^/]+\/(install|update|repair)|runtime\/install)$/, timeoutMs: installTimeoutMs },
	// 旧路径，与上面的 agents/{id}/update 是同一段（agent_routes.go 的薄委托）
	{ methods: ["POST"], pattern: /^\/api\/runners\/[^/]+\/(claude|codex)\/update$/, timeoutMs: installTimeoutMs },

	// ── git 写操作 ── **全部**写操作。判据是"它们统统走同一个 backend"：单条 git 命令
	// 的上限是 gitCommandTimeout = 30 秒（git.go 的 localGitBackend.runGit），SSH 项目还要
	// 叠加远端往返。所以 `git add` 在超大仓库上、`fetch/pull/push` 在慢网络上，都能合法地
	// 超过 15 秒 —— 按"这条看着快不快"逐条挑，只会挑漏（stage/stage-all 当初就是这么被漏掉的）。
	// 读操作不在此列（GET 那一档另有 2x 重试兜底，见文件的说明）。
	// 手机端的同一族操作早已按这个量级分档（features/git/mobile-git-request.ts:26-30）。
	{ methods: ["POST"], pattern: /^\/api\/projects\/[^/]+\/git\/[^/]+/, timeoutMs: externalTaskTimeoutMs },

	// ── 会起外部进程或走网络的其他写操作 ──
	// MCP 探针上限 mcpDefaultProbeTimeout = 25 秒、runtime-check 20 秒。
	// （`/api/mcp/preview` 不在此列：它是纯解析、不起进程，快。）
	{ methods: ["POST"], pattern: /^\/api\/mcp\/(test-draft|import|runtime-check)$/, timeoutMs: externalTaskTimeoutMs },
	{ methods: ["POST"], pattern: /^\/api\/mcp\/servers\/[^/]+\/(test|oauth\/start)$/, timeoutMs: externalTaskTimeoutMs },
	// SSH：preflight 是 4 次远端往返（echo / readDir / claude --version / 审批隧道）；
	// `PUT /{id}` 那条是"改完顺手连一次"（带 connect:true），同样要握手。
	{ methods: ["POST"], pattern: /^\/api\/ssh-connections(\/preflight|\/[^/]+\/(test|connect))?$/, timeoutMs: externalTaskTimeoutMs },
	{ methods: ["PUT"], pattern: /^\/api\/ssh-connections\/[^/]+$/, timeoutMs: externalTaskTimeoutMs },
	// 终端：起进程本身是同步阻塞的，走 RunAsAdmin 时上限 2 分钟（terminalElevationPromptTimeout）。
	// 列表是 GET，不在这一档。
	{ methods: ["POST", "DELETE"], pattern: /^\/api\/projects\/[^/]+\/terminal\/sessions(\/[^/]+)?$/, timeoutMs: externalTaskTimeoutMs },
	// 建 worktree（跑 git worktree add）
	{ methods: ["POST"], pattern: /^\/api\/conversations\/[^/]+\/workspaces$/, timeoutMs: externalTaskTimeoutMs },
	// 编排的两条重活（合并分支 / 清理工作区资源）：
	//   · merge-main 是**一串** git 命令（status → rev-parse → merge-base --is-ancestor →
	//     merge --no-ff），单条上限就是 gitCommandTimeout = 30 秒，慢盘/大仓库上叠起来超 15 秒很正常；
	//   · cleanup 服务端自建 30 秒上下文（orchestration.go 的 WithTimeout）+ 10 秒收尾事务，
	//     中间还有 closeWorktreeLockingProcesses —— 那是"等占用工作区的进程退出"，
	//     本项目实测这类等待要 15~17 秒。
	// 它们都是写操作、都已被 detachWriteContext 脱开：客户端 15 秒放弃之后服务端照样跑完并成功，
	// 于是用户看到的是"控制服务未在 15 秒内响应"，再点一次合并会撞 409（分支已合并）。
	// 同族的 pause/resume/stop 不进这张表：它们是单条事务，快得能当假死探测用。
	{ methods: ["POST"], pattern: /^\/api\/tasks\/[^/]+\/orchestration\/(merge-main|cleanup)$/, timeoutMs: externalTaskTimeoutMs },
	// 建项目 / 校验项目：SSH 分支要三次远端往返
	{ methods: ["POST"], pattern: /^\/api\/projects(\/validate)?$/, timeoutMs: externalTaskTimeoutMs },
	// 批量把建议转成任务 / 删除：N 条 finding = N 次写库，服务端在单事务里同步做完
	{ methods: ["POST"], pattern: /^\/api\/projects\/[^/]+\/insights\/(to-task|delete)$/, timeoutMs: externalTaskTimeoutMs },
	// 删项目：服务端**无界**（deleteProjectLocked 用 s.runtimeCtx），要删会话、停 runner、
	// 清 worktree。同族的"删会话"早就在 ConversationPage 里单独放宽到 120 秒，这条漏在外面。
	// 它还是写操作 —— 已被 detachWriteContext 脱开，所以 15 秒一到只得到"删除失败"，
	// 而刷新之后项目**没了**（用户会以为没删掉，再删一次）。
	{ methods: ["DELETE"], pattern: /^\/api\/projects\/[^/]+$/, timeoutMs: externalTaskTimeoutMs },
	// 清空会话：一个事务里裁剪历史，进程收尾挪在锁外（源码注释明说 may block）。
	{ methods: ["POST"], pattern: /^\/api\/conversations\/[^/]+\/clear$/, timeoutMs: externalTaskTimeoutMs },
	// 保存文件：服务端一次超时都没有（filesystem.go 里没有 WithTimeout），SSH 项目存大文件
	// 会超 15 秒。同样是写操作、同样已脱开 —— "保存失败"而文件其实写进去了，是最坏的一种：
	// 用户会重试，于是要么重复保存、要么撞上 workspace 冲突。
	{ methods: ["PUT"], pattern: /^\/api\/projects\/[^/]+\/fs\/write$/, timeoutMs: externalTaskTimeoutMs },
	// 全树遍历（本机 filepath.WalkDir，无时间上限，只靠命中条数收尾）
	{ methods: ["GET"], pattern: /^\/api\/projects\/[^/]+\/fs\/search$/, timeoutMs: externalTaskTimeoutMs },

	// ── 探测类 ──
	// 逐工具探测受 agentProbeTimeout = 45 秒约束；这里是"读一台机器上的全部工具"
	{ methods: ["GET"], pattern: /^\/api\/runners\/[^/]+\/(agents|diagnostics)$/, timeoutMs: probeTimeoutMs },
	{ methods: ["GET"], pattern: /^\/api\/runners\/[^/]+\/agents\/[^/]+\/diagnose$/, timeoutMs: probeTimeoutMs },
	// registry 往返：实测 2~3 秒/次，最坏一次 14.6 秒才失败（docs/42 §25）—— 正好贴着 15 秒
	{ methods: ["POST"], pattern: /^\/api\/runners\/[^/]+\/agents\/[^/]+\/check-update$/, timeoutMs: probeTimeoutMs },
	{ methods: ["GET"], pattern: /^\/api\/runtimes\/catalog$/, timeoutMs: probeTimeoutMs },
];

/**
 * 给出这次请求的等待预算。
 *
 * 命中长任务的请求**一律不重试**：那张幂等重试（GET 放宽到 2x）是为"服务端瞬时忙"设计的，
 * 用在分钟级的重活上等于把整件事**再跑一遍** —— 一次 push、一次 npm 安装都不是能随便重发的。
 */
function requestBudgetMs(path: string, method: string): number {
	const query = path.indexOf("?");
	const bare = query >= 0 ? path.slice(0, query) : path;
	for (const entry of longRunningEndpoints) {
		if (entry.methods.includes(method) && entry.pattern.test(bare)) return entry.timeoutMs;
	}
	return requestTimeoutMs;
}

/** 把等待上限写成给人看的时长（"90 秒" / "16 分钟"）。 */
function formatWait(ms: number): string {
	const seconds = Math.round(ms / 1000);
	return seconds >= 60 ? `${Math.round(seconds / 60)} 分钟` : `${seconds} 秒`;
}

export type APIError = Error & {
  status: number;
  code?: string;
  details?: Record<string, string>;
};

/** 这次请求的方法。预算与重试资格都要看它（同一条路径上常常同时挂着读与写）。 */
const requestMethod = (init?: RequestInit) => (init?.method ?? "GET").toUpperCase();

function retryCountFor(init?: RequestInit, path = ""): number {
  const method = requestMethod(init);
  if (requestBudgetMs(path, method) > requestTimeoutMs) return 0;
  return method === "GET" || method === "HEAD" || method === "OPTIONS" ? 2 : 0;
}

export async function api<T>(path: string, init?: RequestInit, retries = retryCountFor(init, path)): Promise<T> {
	return apiWithTimeout<T>(path, init, retries, requestBudgetMs(path, requestMethod(init)));
}

/**
 * 显式给预算的那个入口（调用方知道得比表多时用它：整库清理 15 分钟、批量删会话 2 分钟）。
 *
 * 省略 `timeoutMs` 时**同样查表** —— 两个入口如果连默认值都不一样，就会长出
 * "为什么这个调用没有拿到长预算"这种只看调用点看不出来的差别。
 */
export async function apiWithTimeout<T>(path: string, init?: RequestInit, retries = retryCountFor(init, path), timeoutMs = requestBudgetMs(path, requestMethod(init))): Promise<T> {
  let lastError: unknown;
  const signal = init?.signal;
  // 服务端忙（单 SQLite 连接被长事务占住、SSH/WSL 慢探测等）时，一次 15s 超时
  // 往往只是瞬时抖动而非服务不可用。幂等方法（GET/HEAD/OPTIONS）最多给一次
  // 放宽到 2x 的重试机会；仍超时才提示重启，避免单个慢请求误导用户。
  let timeoutRetried = false;
  for (let attempt = 0; attempt <= retries; attempt++) {
    try {
      if (signal?.aborted) throw new DOMException("Aborted", "AbortError");
      const headers = sessionHeaders(init?.headers);
      if (!headers.has("Content-Type")) headers.set("Content-Type", "application/json");
      const attemptTimeoutMs = timeoutRetried ? timeoutMs * 2 : timeoutMs;
      const controller = new AbortController();
      const timeout = globalThis.setTimeout(() => controller.abort(), attemptTimeoutMs);
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
          // 内部超时触发：重试过一次仍超时（或本次无重试资格）才给出最终错误。
          //
          // ⚠️ 长任务那一档**不能说"失败"**。它们的服务端预算本来就是分钟级，而且其中
          // 一部分（走 s.runtimeCtx 的那些）**客户端断开也不中止** —— 说成失败会让用户
          // 重来一次，而第一次其实成功了。这里如实说"还在跑"，并让他去刷新看结果。
          const long = timeoutMs > requestTimeoutMs;
          lastError = new Error(
            timeoutRetried
              ? `控制服务持续未响应，请重启 Milevia 后重试。`
              : long
                ? `这项操作超过 ${formatWait(timeoutMs)}仍未返回。服务端可能仍在后台继续执行 —— 稍后刷新即可看到结果，不要重复提交。`
                : `控制服务未在 ${Math.round(attemptTimeoutMs / 1000)} 秒内响应，请稍后重试。`,
          );
          if (!timeoutRetried && attempt < retries) {
            timeoutRetried = true;
            await new Promise((resolve) => setTimeout(resolve, 500 * (attempt + 1)));
            continue;
          }
          throw lastError;
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
        const err = new Error(message) as APIError;
        err.status = response.status;
        if (typeof body?.code === "string") err.code = body.code;
        if (body?.details && typeof body.details === "object" && !Array.isArray(body.details)) err.details = body.details as Record<string, string>;
        throw err;
      }
      const err5xx = new Error(message) as APIError;
      err5xx.status = response.status;
      if (typeof body?.code === "string") err5xx.code = body.code;
      if (body?.details && typeof body.details === "object" && !Array.isArray(body.details)) err5xx.details = body.details as Record<string, string>;
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
