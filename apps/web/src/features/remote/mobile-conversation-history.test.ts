import assert from "node:assert/strict";
import test from "node:test";
import { createMobileConversationHistory, type HistoryAdapters } from "./mobile-conversation-history";
import { newPendingConversationID } from "../../lib/conversation-mutations";

type Reply = Awaited<ReturnType<HistoryAdapters<string, string>["request"]>>;
function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (cause: unknown) => void;
  const promise = new Promise<T>((ok, fail) => { resolve = ok; reject = fail; });
  return { promise, resolve, reject };
}
function setup(request: HistoryAdapters<string, string>["request"]) {
  let current = true;
  const scheduled = new Set<() => void>();
  const controller = createMobileConversationHistory<string, string>({
    request,
    messages: (value) => Array.isArray(value) ? value.filter((item): item is string => typeof item === "string") : [],
    index: (value) => typeof value === "string" ? value : null,
    merge: (previous, next) => [...new Set([...previous, ...next])],
    isCurrent: () => current,
    schedule: (callback) => { scheduled.add(callback); return () => { scheduled.delete(callback); }; },
  });
  return { controller, scheduled, invalidate: () => { current = false; }, next: () => { for (const callback of [...scheduled]) { scheduled.delete(callback); callback(); } } };
}
const tick = () => new Promise<void>((resolve) => setImmediate(resolve));

test("历史链在首屏落地后续取消息页，合并重复消息，并停止于消息专用标记", async () => {
  const calls: Record<string, string>[] = [];
  const state = setup(async (_project, _conversation, _op, params) => {
    calls.push(params);
    return { ok: true, data: params.cursor
      ? { messages: ["one", "two"], hasMore: true, hasMoreMessages: false, nextCursor: "events-only" }
      : { messages: ["two"], hasMoreMessages: true, nextCursor: "older" } };
  });
  const loading = state.controller.load("p", "c");
  await tick();
  assert.deepEqual(state.controller.getSnapshot().pages.c.messages, ["two"]);
  assert.equal(state.controller.getSnapshot().pages.c.loading, true);
  state.next();
  await loading;
  assert.deepEqual(calls, [{ limit: "50", messagesOnly: "true" }, { limit: "50", messagesOnly: "true", cursor: "older" }]);
  assert.deepEqual(state.controller.getSnapshot().pages.c.messages, ["two", "one"]);
  assert.equal(state.controller.getSnapshot().pages.c.hasMore, false);
});

test("同一会话只存在一条在途历史链，即使再次打开时传入了不同页大小", async () => {
  const pending = deferred<Reply>();
  let count = 0;
  const state = setup(() => { count += 1; return pending.promise; });
  const first = state.controller.load("p", "c");
  assert.equal(state.controller.load("p", "c", "", 25), first);
  pending.resolve({ ok: true, data: { messages: ["one"] } });
  await first;
  assert.equal(count, 1);
});

test("413 从同一游标折半重试，缩小后的大小沿用到后续页", async () => {
  const calls: Record<string, string>[] = [];
  const state = setup(async (_p, _c, _op, params) => {
    calls.push(params);
    if (params.limit === "50") return { ok: false, status: 413 };
    return { ok: true, data: { messages: [params.cursor || "first"], hasMoreMessages: !params.cursor, nextCursor: params.cursor ? "" : "next" } };
  });
  const loading = state.controller.load("p", "c");
  await tick();
  state.next();
  await loading;
  assert.deepEqual(calls.map((params) => [params.limit, params.cursor || ""]), [["50", ""], ["25", ""], ["25", "next"]]);
});

test("设备或项目已切换时晚到的历史成功和失败均不能发布状态", async () => {
  for (const success of [true, false]) {
    const pending = deferred<Reply>();
    const state = setup(() => pending.promise);
    const loading = state.controller.load("p", "c");
    const before = state.controller.getSnapshot();
    state.invalidate();
    if (success) pending.resolve({ ok: true, data: { messages: ["old"], hasMoreMessages: true, nextCursor: "next" } });
    else pending.reject(new Error("old failure"));
    await loading;
    assert.equal(state.controller.getSnapshot(), before);
    assert.equal(state.scheduled.size, 0);
  }
});

test("卸载会取消自动续页，StrictMode 重新激活后旧请求仍失效", async () => {
  let count = 0;
  const state = setup(async () => { count += 1; return { ok: true, data: { messages: [], hasMoreMessages: true, nextCursor: "next" } }; });
  const loading = state.controller.load("p", "c");
  await tick();
  assert.equal(state.scheduled.size, 1);
  state.controller.dispose();
  state.controller.activate();
  await loading;
  state.next();
  assert.equal(count, 1);
  assert.equal(state.scheduled.size, 0);
});

test("重复历史游标终止续页并保留已读消息与可见错误", async () => {
  const state = setup(async () => ({ ok: true, data: { messages: ["one"], hasMoreMessages: true, nextCursor: "same" } }));
  const loading = state.controller.load("p", "c");
  await tick();
  state.next();
  await loading;
  assert.deepEqual(state.controller.getSnapshot().pages.c.messages, ["one"]);
  assert.match(state.controller.getSnapshot().pages.c.error, /重复/);
  assert.equal(state.controller.getSnapshot().pages.c.loading, false);
});

test("临时会话与空目标不发历史请求", async () => {
  let count = 0;
  const state = setup(async () => { count += 1; return { ok: true }; });
  await state.controller.load("p", newPendingConversationID());
  await state.controller.load("", "c");
  await state.controller.load("p", "");
  assert.equal(count, 0);
});

test("目录按游标读全并过滤无效条目", async () => {
  const state = setup(async (_p, _c, op, params) => {
    assert.equal(op, "conversation.list");
    return { ok: true, data: params.cursor ? { items: ["b", null] } : { items: ["a"], nextCursor: "next" } };
  });
  await state.controller.loadDirectory("p");
  assert.deepEqual(state.controller.getSnapshot().directory, ["a", "b"]);
  assert.equal(state.controller.getSnapshot().directoryProjectID, "p");
  assert.equal(state.controller.getSnapshot().directoryLoading, false);
});

test("新目录请求胜出，旧成功或失败都不覆盖新目录与加载状态", async () => {
  for (const success of [true, false]) {
    const pending = deferred<Reply>();
    let count = 0;
    const state = setup(() => ++count === 1 ? pending.promise : Promise.resolve({ ok: true, data: { items: ["new"] } }));
    const old = state.controller.loadDirectory("p");
    await state.controller.loadDirectory("p");
    const before = state.controller.getSnapshot();
    if (success) pending.resolve({ ok: true, data: { items: ["old"] } });
    else pending.reject(new Error("old"));
    await old;
    assert.equal(state.controller.getSnapshot(), before);
  }
});

test("目录设备失效后不写入旧结果，也不发下一页请求", async () => {
  const pending = deferred<Reply>();
  const state = setup(() => pending.promise);
  const loading = state.controller.loadDirectory("p");
  const before = state.controller.getSnapshot();
  state.invalidate();
  pending.resolve({ ok: true, data: { items: ["old"], nextCursor: "next" } });
  await loading;
  assert.equal(state.controller.getSnapshot(), before);
});

// 分页缓存在本地。电脑端（或另一台手机）回退之后，缓存里被丢掉的那几轮不会自己消失 ——
// 回包带的 messagesCutoff / messagesVisibleAfter 要能把它滤掉，否则手机端会继续显示
// 用户刚刚在电脑上扔掉的内容。
test("历史页按会话回退节点的可见性过滤本地缓存", async () => {
  const seen: Array<{ cutoff?: string; visibleAfter?: string }> = [];
  let current = true;
  const controller = createMobileConversationHistory<string, string>({
    request: async () => ({ ok: true, data: { messages: ["kept", "discarded"], hasMoreMessages: false, nextCursor: "", messagesCutoff: "cut", messagesVisibleAfter: "after" } }),
    messages: (value) => Array.isArray(value) ? value.filter((item): item is string => typeof item === "string") : [],
    index: (value) => typeof value === "string" ? value : null,
    merge: (previous, next) => [...new Set([...previous, ...next])],
    filterRewound: (messages, cutoff, visibleAfter) => {
      seen.push({ cutoff, visibleAfter });
      return messages.filter((message) => message !== "discarded");
    },
    isCurrent: () => current,
    schedule: () => () => undefined,
  });
  await controller.load("p", "c");
  assert.deepEqual(seen, [{ cutoff: "cut", visibleAfter: "after" }]);
  assert.deepEqual(controller.getSnapshot().pages.c.messages, ["kept"]);
});
