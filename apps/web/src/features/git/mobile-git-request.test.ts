import assert from "node:assert/strict";
import test from "node:test";

import {
  classifyGitFailure,
  createMobileGitRequest,
  MobileGitError,
  parseGitCall,
  STALE_STATE_NOTICE,
} from "./mobile-git-request";
import type { MobileRpcReply, MobileRpcTransport } from "../remote/mobile-rpc";

// 记账桩：记录每次发出的 op / params / timeoutMs，并按脚本回话。
//
// 桩**没配这一枪就直接抛**（与文件适配器那份同一条约定）：靠"第几枪"分派的桩一旦扣错枪，
// 断言会读到空值而空跑通过 —— 先炸出来比事后查便宜得多。
function makeTransport(script: (op: string, params: unknown, timeoutMs?: number) => MobileRpcReply | undefined) {
  const calls: Array<{ op: string; params: unknown; timeoutMs?: number }> = [];
  const transport: MobileRpcTransport = async (op, params, timeoutMs) => {
    calls.push({ op, params, timeoutMs });
    const reply = script(op, params, timeoutMs);
    if (!reply) throw new Error(`stub has no reply for ${op} ${JSON.stringify(params)}`);
    return reply;
  };
  return { transport, calls };
}

const ok = (data: unknown): MobileRpcReply => ({ ok: true, status: 200, data });
const failure = (status: number, error: string): MobileRpcReply => ({ ok: false, status, error });

const OID = "0f1e2d3c4b5a69788796a5b4c3d2e1f001234567";
const SUGGESTION_ID = "3f2b1c4e-5a6d-4e8f-9b0c-1d2e3f4a5b6c";
const git = (suffix: string) => `/api/projects/p1/git${suffix}`;

// ─── 路由表：REST → op 的唯一一份映射 ──────────────────────────────────────

test("every REST call the workbench makes maps to a git op", () => {
  const expected: Array<{ path: string; method: string; op: string }> = [
    { path: "/summary", method: "GET", op: "git.summary" },
    { path: "/changes", method: "GET", op: "git.changes" },
    { path: "/diff?path=a.go&stage=worktree", method: "GET", op: "git.diff" },
    { path: "/log?limit=50", method: "GET", op: "git.log" },
    { path: "/operations?limit=50", method: "GET", op: "git.operations" },
    { path: `/commits/${OID}`, method: "GET", op: "git.commit" },
    { path: `/commits/${OID}/diff?path=a.go`, method: "GET", op: "git.commitDiff" },
    { path: "/conflicts", method: "GET", op: "git.conflicts" },
    { path: "/conflicts/content?path=a.go", method: "GET", op: "git.conflictContent" },
    { path: `/conflicts/suggestions/${SUGGESTION_ID}`, method: "GET", op: "git.suggestion" },
    { path: "/branches", method: "GET", op: "git.branches" },
    { path: "/stage", method: "POST", op: "git.stage" },
    { path: "/unstage", method: "POST", op: "git.unstage" },
    { path: "/stage-all", method: "POST", op: "git.stageAll" },
    { path: "/unstage-all", method: "POST", op: "git.unstageAll" },
    { path: "/commits", method: "POST", op: "git.commitCreate" },
    { path: "/commits/amend", method: "POST", op: "git.commitAmend" },
    { path: "/discard", method: "POST", op: "git.discard" },
    { path: "/fetch", method: "POST", op: "git.fetch" },
    { path: "/pull", method: "POST", op: "git.pull" },
    { path: "/push", method: "POST", op: "git.push" },
    { path: "/branches", method: "POST", op: "git.createBranch" },
    { path: "/switch", method: "POST", op: "git.switch" },
    { path: "/conflicts/resolve", method: "POST", op: "git.conflictResolve" },
    { path: "/conflicts/abort", method: "POST", op: "git.conflictAbort" },
    { path: "/conflicts/continue", method: "POST", op: "git.conflictContinue" },
    { path: "/conflicts/suggest", method: "POST", op: "git.conflictSuggest" },
    { path: `/conflicts/suggestions/${SUGGESTION_ID}/cancel`, method: "POST", op: "git.suggestionCancel" },
  ];
  for (const item of expected) {
    const parsed = parseGitCall(git(item.path), { method: item.method });
    assert.equal(parsed.route.op, item.op, `${item.method} ${item.path}`);
  }
});

// 两条路由共用同一路径（只读的分支列表 / 新建分支），只靠方法区分。
// 只按路径匹配的话，`POST /branches` 会读到分支列表并把用户的新分支当成"已经建好了"。
test("the same path with different methods resolves to different ops", () => {
  assert.equal(parseGitCall(git("/branches"), { method: "GET" }).route.op, "git.branches");
  assert.equal(parseGitCall(git("/branches"), { method: "POST" }).route.op, "git.createBranch");
});

// `/commits/amend` 与 `/commits/{oid}` 都是两段：让占位符先试的话，
// 修改最近一次提交会被当成"读一个叫 amend 的提交"。
test("a literal segment wins over a path placeholder", () => {
  assert.equal(parseGitCall(git("/commits/amend"), { method: "POST" }).route.op, "git.commitAmend");
  assert.equal(parseGitCall(git(`/commits/${OID}`), { method: "GET" }).route.op, "git.commit");
  assert.deepEqual(parseGitCall(git(`/commits/${OID}`), { method: "GET" }).pathParams, { oid: OID });
});

test("an unmapped Git endpoint is a wiring error and sends nothing", async () => {
  const { transport, calls } = makeTransport(() => ok({}));
  const adapter = createMobileGitRequest({ transport });
  await assert.rejects(
    () => adapter.request(git("/stash")),
    (error: unknown) => error instanceof MobileGitError && error.code === "wiring",
  );
  assert.equal(calls.length, 0, "a wiring error must not send anything");
});

test("a wrong method is a wiring error rather than a silent rewrite", async () => {
  const { transport, calls } = makeTransport(() => ok({}));
  const adapter = createMobileGitRequest({ transport });
  await assert.rejects(
    () => adapter.request(git("/summary"), { method: "POST" }),
    (error: unknown) => error instanceof MobileGitError && error.code === "wiring",
  );
  assert.equal(calls.length, 0);
});

test("conversationId from the query is dropped — the envelope owns the workspace", () => {
  const parsed = parseGitCall(git("/changes?conversationId=conv-forged&limit=50"));
  assert.equal(parsed.query.get("conversationId"), null);
  assert.equal(parsed.query.get("limit"), "50");
});

test("path params reach the op params", () => {
  const parsed = parseGitCall(git(`/commits/${OID}/diff?path=src%2Fmain.go`));
  assert.deepEqual(parsed.pathParams, { oid: OID });
  // query 类操作的 params 是查询参数与路径参数的并集。
  assert.equal(parsed.query.get("path"), "src/main.go");
});

// ─── 写操作：无条件换一个新鲜的 stateToken ──────────────────────────────────

// 这是本适配器与文件适配器最重要的一处不同。服务端的乐观锁令牌只有 2 分钟有效期，
// 而"看一眼 diff → 想一想 → 点提交"很容易越过去；过期后的症状是一句英文报错。
test("every write fetches a fresh state token first", async () => {
  let issued = 0;
  const { transport, calls } = makeTransport((op, params) => {
    if (op === "git.summary") {
      issued += 1;
      return ok({ stateToken: `tok-${issued}` });
    }
    return ok({ operationId: "op-1", status: "succeeded" });
  });
  const adapter = createMobileGitRequest({ transport });

  await adapter.request(git("/stage"), { method: "POST", body: JSON.stringify({ paths: ["a.go"], stateToken: "tok-stale" }) });
  assert.deepEqual(calls.map((call) => call.op), ["git.summary", "git.stage"]);
  assert.deepEqual((calls[1].params as { stateToken: string }).stateToken, "tok-1");

  // 第二次写操作要**再换一个**，不能沿用界面上那份（它可能已经过期）。
  await adapter.request(git("/commits"), { method: "POST", body: JSON.stringify({ message: "hello", stateToken: "tok-1" }) });
  assert.deepEqual(calls.map((call) => call.op), ["git.summary", "git.stage", "git.summary", "git.commitCreate"]);
  assert.deepEqual((calls[3].params as { stateToken: string }).stateToken, "tok-2");
});

// 令牌换取只服务于写操作：读操作不该多付一次往返。
test("reads do not pay for a token", async () => {
  const { transport, calls } = makeTransport(() => ok({}));
  const adapter = createMobileGitRequest({ transport });
  await adapter.request(git("/changes"));
  assert.deepEqual(calls.map((call) => call.op), ["git.changes"]);
});

// 建分支与生成建议都不带乐观锁，所以它们也不该多付一次往返。
test("writes without optimistic locking skip the token refresh", async () => {
  const { transport, calls } = makeTransport(() => ok({}));
  const adapter = createMobileGitRequest({ transport });
  await adapter.request(git("/branches"), { method: "POST", body: JSON.stringify({ name: "feature/x", startPoint: "" }) });
  assert.deepEqual(calls.map((call) => call.op), ["git.createBranch"]);
});

// 拖下来的是新令牌，别的业务字段一个都不能丢 —— 丢了就是"每次提交都失败在参数上"。
test("the refreshed token is merged without dropping business fields", async () => {
  const { transport, calls } = makeTransport((op) => (op === "git.summary" ? ok({ stateToken: "tok-new" }) : ok({})));
  const adapter = createMobileGitRequest({ transport });
  await adapter.request(git("/discard"), {
    method: "POST",
    body: JSON.stringify({ paths: ["a.go"], mode: "worktree", includeUntracked: true, stateToken: "tok-old" }),
  });
  assert.deepEqual(calls[1].params, {
    paths: ["a.go"],
    mode: "worktree",
    includeUntracked: true,
    stateToken: "tok-new",
  });
});

// ─── 超时分档 ──────────────────────────────────────────────────────────────

// 默认 20s 会让 push/fetch 必然超时（服务端单条 git 命令上限是 30s）。
test("timeouts are tiered by operation class", async () => {
  const { transport, calls } = makeTransport((op) => (op === "git.summary" ? ok({ stateToken: "t" }) : ok({})));
  const adapter = createMobileGitRequest({ transport });

  await adapter.request(git("/diff?path=a.go&stage=worktree"));
  await adapter.request(git("/stage"), { method: "POST", body: JSON.stringify({ paths: ["a.go"] }) });
  await adapter.request(git("/push"), { method: "POST", body: JSON.stringify({ remote: "origin", branch: "main" }) });

  const timeouts = calls
    .filter((call) => call.op !== "git.summary")
    .map((call) => call.timeoutMs);
  assert.deepEqual(timeouts, [20_000, 45_000, 60_000]);
  // 网络写必须比服务端的 30s 命令上限宽，否则手机先报超时、电脑还在跑。
  assert.ok((timeouts[2] as number) > 30_000);
});

// ─── 失败分类 ──────────────────────────────────────────────────────────────

test("server sentences map to machine-readable failure kinds", () => {
  assert.equal(classifyGitFailure("Git state changed; refresh the repository"), "stale_state");
  assert.equal(
    classifyGitFailure("project workspace is occupied by another run or Git operation"),
    "workspace_busy",
  );
  assert.equal(classifyGitFailure("selected Git paths are no longer available"), "changes_gone");
  assert.equal(classifyGitFailure("there are no eligible Git changes"), "nothing_to_do");
  assert.equal(classifyGitFailure("runner_offline"), "runner_offline");
  assert.equal(classifyGitFailure("Git commit message must be between 1 and 4000 characters"), "other");
});

// 「工作区被占用」这句**到了手机上已经不是英文了**：电脑端的 writeError 会把它本地化成
// "项目工作区正被其他 AI 任务或 Git 操作占用…"。上面那条用例喂的是本地化**之前**的原文，
// 所以它单独存在时是假绿 —— 判据量错了东西。这里喂真实上路的形态。
test("the localized workspace-occupied sentence is still recognized", () => {
  assert.equal(
    classifyGitFailure("项目工作区正被其他 AI 任务或 Git 操作占用，请等待当前操作完成后重试。"),
    "workspace_busy",
  );
  // 未登记翻译的那些会被包成 `中文兜底句：原文`，原文仍是子串 —— 这是它们能匹配的前提。
  assert.equal(
    classifyGitFailure("当前操作与进行中的操作冲突，请稍后重试。：Git state changed; refresh the repository"),
    "stale_state",
  );
});

// 稳定错误码优先级最高：它是唯一不会被本地化改写的判据（app.go 的 httpErrorCode）。
test("a stable error code wins over the message text", () => {
  // 文案完全不认识，但带了码 —— 仍要判对。
  assert.equal(classifyGitFailure("任意改写过的中文文案", "workspace_occupied"), "workspace_busy");
  // 码不认识的失败，退回文案。
  assert.equal(classifyGitFailure("Git state changed; refresh the repository", "something_new"), "stale_state");
});

test("a business failure keeps the server sentence and its status", async () => {
  const { transport } = makeTransport(() => failure(409, "project workspace is occupied by another run or Git operation"));
  const adapter = createMobileGitRequest({ transport });
  await assert.rejects(
    () => adapter.request(git("/changes")),
    (error: unknown) =>
      error instanceof MobileGitError &&
      error.code === "workspace_busy" &&
      error.status === 409 &&
      error.message.includes("occupied by another run"),
  );
});

// 换了新令牌还是过期 ⇒ 电脑上的仓库真的变了。这一档要**通知宿主去重载**，
// 而不是悄悄失败（那会让用户看着一份过期状态反复点同一个按钮）。
//
// 抛出去的那条错误必须**逐字**是那条提示：宿主靠这个相等关系认出"这件事我已经用
// 绿色提示说过了"，从而不在红色错误条里重复一遍（服务端原文在这里已经是
// "中文兜底句：Git state changed…" 那种形态，直接把原文抛出去会让用户看到它）。
test("a stale state asks the host to reload and still fails the call", async () => {
  const notices: string[] = [];
  const { transport } = makeTransport((op) =>
    op === "git.summary" ? ok({ stateToken: "tok-new" }) : failure(409, "Git state changed; refresh the repository"),
  );
  const adapter = createMobileGitRequest({ transport, onStale: (message) => notices.push(message) });
  await assert.rejects(
    () => adapter.request(git("/stage"), { method: "POST", body: JSON.stringify({ paths: ["a.go"] }) }),
    (error: unknown) =>
      error instanceof MobileGitError && error.code === "stale_state" && error.message === STALE_STATE_NOTICE,
  );
  assert.equal(notices.length, 1);
  assert.equal(notices[0], STALE_STATE_NOTICE);
});

// 那条绿色提示必须**自己撤掉**：用户重试一次写操作并成功之后，它还挂着就变成谎话
// （"请确认后再提交"而其实已经提交成功）。
// 关键在"只在**写**成功时撤"：失败之后宿主会立刻重载仓库（一串只读请求），
// 只读成功也撤的话，这条提示只会闪 300 毫秒 —— 等于没有。
test("the stale notice is cleared by a successful write, not by the reload reads", async () => {
  const notices: string[] = [];
  let recoveries = 0;
  let stale = true;
  const { transport } = makeTransport((op) => {
    if (op === "git.summary") return ok({ stateToken: "tok-new" });
    if (op === "git.stage" && stale) return failure(409, "Git state changed; refresh the repository");
    return ok({});
  });
  const adapter = createMobileGitRequest({
    transport,
    onStale: (message) => notices.push(message),
    onRecovered: () => { recoveries += 1; },
  });

  await assert.rejects(() => adapter.request(git("/stage"), { method: "POST", body: JSON.stringify({ paths: ["a.go"] }) }));
  assert.equal(notices.length, 1, "the failure must raise the notice");
  // 宿主随后的重载：一串只读请求成功，**不该**撤掉提示。
  await adapter.request(git("/changes"));
  await adapter.request(git("/diff?path=a.go&stage=worktree"));
  assert.equal(recoveries, 0, "a successful read must not clear the notice");

  // 用户重试写操作并成功 —— 这时才撤。
  stale = false;
  await adapter.request(git("/stage"), { method: "POST", body: JSON.stringify({ paths: ["a.go"] }) });
  assert.equal(recoveries, 1, "a successful write must clear the notice");
  // 只撤一次，别每写一次都喊一遍。
  await adapter.request(git("/stage"), { method: "POST", body: JSON.stringify({ paths: ["a.go"] }) });
  assert.equal(recoveries, 1);
});

test("a plain read failure does not trigger the stale notice", async () => {
  const notices: string[] = [];
  const { transport } = makeTransport(() => failure(400, "Git path is not available in the selected change set"));
  const adapter = createMobileGitRequest({ transport, onStale: (message) => notices.push(message) });
  await assert.rejects(() => adapter.request(git("/diff?path=a.go&stage=worktree")));
  assert.deepEqual(notices, []);
});

// ─── 不缓存内容 ────────────────────────────────────────────────────────────

// Git 状态变得比文件内容快得多（AI 每改一个文件都会动它），缓存 diff 就是把过期内容
// 当新的给用户看。这条把"没有内容缓存"钉成**有意的性质**，免得后来有人"顺手加一层缓存"。
test("nothing is cached: the same read twice means two requests", async () => {
  const { transport, calls } = makeTransport(() => ok({ path: "a.go", content: "@@" }));
  const adapter = createMobileGitRequest({ transport });
  await adapter.request(git("/diff?path=a.go&stage=worktree"));
  await adapter.request(git("/diff?path=a.go&stage=worktree"));
  assert.deepEqual(calls.map((call) => call.op), ["git.diff", "git.diff"]);
});

// 令牌换取失败（电脑掉线 / 超时）时，要原样把那次失败抛出去，
// 不能"换令牌失败 → 拿旧令牌硬发一次" —— 那会拿到一个指不到原因的 409。
test("a failed token refresh fails the write instead of reusing an old token", async () => {
  const { transport, calls } = makeTransport((op) =>
    op === "git.summary" ? failure(409, "instance_offline") : ok({}),
  );
  const adapter = createMobileGitRequest({ transport });
  await assert.rejects(
    () => adapter.request(git("/stage"), { method: "POST", body: JSON.stringify({ paths: ["a.go"], stateToken: "tok-old" }) }),
    (error: unknown) => error instanceof MobileGitError && error.message.includes("instance_offline"),
  );
  assert.deepEqual(calls.map((call) => call.op), ["git.summary"], "the write must not be sent with a stale token");
});
