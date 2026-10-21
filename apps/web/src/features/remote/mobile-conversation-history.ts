import { asRecord } from "../../lib/api";
import { isPendingConversationID } from "../../lib/conversation-mutations";

export type ConversationHistoryPage<M> = { messages: M[]; nextCursor: string; hasMore: boolean; loading: boolean; error: string };
export type HistoryState<M, I> = {
  pages: Record<string, ConversationHistoryPage<M>>;
  directory: I[] | null;
  directoryProjectID: string;
  directoryLoading: boolean;
  directoryError: string;
};
export type HistoryAdapters<M, I> = {
  request: (projectID: string, conversationID: string, op: string, params: Record<string, string>, timeoutMs: number) => Promise<{ ok: boolean; status?: number; error?: string; data?: unknown }>;
  messages: (value: unknown) => M[];
  index: (value: unknown) => I | null;
  merge: (previous: M[], next: M[]) => M[];
  // filterRewound 把"会话回退节点"的可见性套用在这份历史页上：分页缓存是**本地**的，
  // 电脑端（或另一台手机）回退之后，这里存着的被丢掉的那几轮不会自己消失。服务端每份
  // history 回包都带 messagesCutoff / messagesVisibleAfter，用它把缓存里被回退掉的条目滤掉。
  // 不传就不滤（老调用方）。
  filterRewound?: (messages: M[], cutoff: string | undefined, visibleAfter: string | undefined) => M[];
};

// 一个控制器只服务一台设备上的一个项目。请求不能在 await 后换用新设备的适配器。
export function createMobileConversationHistory<M, I>(options: HistoryAdapters<M, I> & {
  isCurrent: () => boolean;
  schedule: (callback: () => void) => () => void;
}) {
  let state: HistoryState<M, I> = { pages: {}, directory: null, directoryProjectID: "", directoryLoading: false, directoryError: "" };
  let active = true;
  let generation = 0;
  let directoryGeneration = 0;
  const listeners = new Set<() => void>();
  const loads = new Map<string, Promise<void>>();
  const scheduled = new Set<() => void>();
  const current = (epoch: number) => active && generation === epoch && options.isCurrent();
  const publish = (patch: Partial<HistoryState<M, I>>) => {
    state = { ...state, ...patch };
    listeners.forEach((listener) => listener());
  };
  const updatePage = (id: string, patch: Partial<ConversationHistoryPage<M>>) => {
    const previous = state.pages[id] || { messages: [], nextCursor: "", hasMore: false, loading: false, error: "" };
    publish({ pages: { ...state.pages, [id]: { ...previous, ...patch } } });
  };
  const load = (projectID: string, conversationID: string, cursor = "", initialPageSize = 50): Promise<void> => {
    const epoch = generation;
    if (!current(epoch) || !projectID || !conversationID || isPendingConversationID(conversationID)) return Promise.resolve();
    // 整条会话链只有一个所有者，缩页重试与自动续页都在这条链内完成。
    const key = JSON.stringify([projectID, conversationID]);
    const pending = loads.get(key);
    if (pending) return pending;
    const run = async () => {
      let next = cursor;
      let pageSize = initialPageSize;
      const visited = new Set<string>();
      updatePage(conversationID, { loading: true, error: "" });
      try {
        while (current(epoch)) {
          const params: Record<string, string> = { limit: String(pageSize), messagesOnly: "true" };
          if (next) params.cursor = next;
          const reply = await options.request(projectID, conversationID, "conversation.history", params, 30_000);
          if (!current(epoch)) return;
          if (!reply.ok && reply.status === 413 && pageSize > 1) {
            pageSize = Math.max(1, Math.floor(pageSize / 2));
            continue;
          }
          if (!reply.ok) throw new Error(reply.error || "无法读取会话历史");
          const page = asRecord(reply.data);
          const rawMessages = options.messages(page?.messages);
          const messages = options.filterRewound
            ? options.filterRewound(rawMessages, typeof page?.messagesCutoff === "string" ? page.messagesCutoff : undefined, typeof page?.messagesVisibleAfter === "string" ? page.messagesVisibleAfter : undefined)
            : rawMessages;
          const nextCursor = typeof page?.nextCursor === "string" ? page.nextCursor : "";
          const hasMore = page?.hasMoreMessages === true && Boolean(nextCursor);
          visited.add(next);
          updatePage(conversationID, {
            messages: options.merge(next ? state.pages[conversationID]?.messages || [] : [], messages),
            nextCursor, hasMore, loading: hasMore, error: "",
          });
          if (!hasMore) return;
          if (visited.has(nextCursor)) throw new Error("电脑端返回了重复的历史游标，请稍后重试");
          next = nextCursor;
          // 首屏先落地，再给浏览器一次绘制机会。取消后唤醒等待者以释放整条链。
          await new Promise<void>((resolve) => {
            const finish = () => { scheduled.delete(cancel); resolve(); };
            const stop = options.schedule(finish);
            const cancel = () => { stop(); finish(); };
            scheduled.add(cancel);
          });
        }
      } catch (cause) {
        if (current(epoch)) updatePage(conversationID, { loading: false, error: cause instanceof Error ? cause.message : "无法读取会话历史" });
      }
    };
    const promise = run().finally(() => { if (loads.get(key) === promise) loads.delete(key); });
    loads.set(key, promise);
    return promise;
  };
  const loadDirectory = async (projectID: string) => {
    const epoch = generation;
    if (!current(epoch) || !projectID) return;
    const requestGeneration = ++directoryGeneration;
    const valid = () => current(epoch) && directoryGeneration === requestGeneration;
    publish({ directoryLoading: true, directoryError: "" });
    try {
      const collected: I[] = [];
      const visited = new Set<string>();
      let cursor = "";
      do {
        visited.add(cursor);
        const reply = await options.request(projectID, "", "conversation.list", cursor ? { limit: "100", cursor } : { limit: "100" }, 20_000);
        if (!valid()) return;
        if (!reply.ok) throw new Error(reply.error || "无法读取电脑端会话目录");
        const page = asRecord(reply.data);
        for (const entry of Array.isArray(page?.items) ? page.items : []) {
          const item = options.index(entry);
          if (item) collected.push(item);
        }
        cursor = typeof page?.nextCursor === "string" ? page.nextCursor : "";
        if (cursor && visited.has(cursor)) throw new Error("电脑端返回了重复的会话目录游标，请稍后重试");
      } while (cursor);
      publish({ directory: collected, directoryProjectID: projectID });
    } catch (cause) {
      if (valid()) publish({ directoryError: cause instanceof Error ? cause.message : "无法读取电脑端会话目录" });
    } finally {
      if (valid()) publish({ directoryLoading: false });
    }
  };
  return {
    getSnapshot: () => state,
    subscribe: (listener: () => void) => { listeners.add(listener); return () => { listeners.delete(listener); }; },
    load, loadDirectory,
    activate: () => { active = true; },
    dispose: () => {
      active = false;
      generation += 1;
      scheduled.forEach((cancel) => cancel());
      scheduled.clear();
      loads.clear();
    },
  };
}
