// 刷新请求的并发合并。
//
// 背景：项目总览页在**每一条** /ws/events 状态事件上都会调 refreshProjects()，服务端广播
// 一密集就会瞬间发出几十个完全相同的请求。而 WebView2 / Chromium 对同一 host 只开 6 条
// HTTP/1.1 连接，这些重复请求把连接池占满之后，用户随后点开项目发的那一发请求只能排在
// 队尾 —— 15s 拿不到连接、重试的 30s 窗口还排在队尾，最后报「控制服务持续未响应」。
//
// 合并之后同一时刻最多只有一个在飞；期间来的调用只登记"结束后再补一次"，让最后一次
// 事件的结果仍然能被拉回来，不会因为合并而漏掉状态变化。

export type RefreshFn = () => Promise<void>;

/** 把 refresh 包成"同一时刻只跑一个 + 至多补跑一次"的入口。 */
export function coalesceRefresh(run: RefreshFn): RefreshFn {
  let inFlight: Promise<void> | null = null;
  let queued = false;
  const start = (): Promise<void> => {
    inFlight = run().finally(() => {
      inFlight = null;
      if (queued) {
        queued = false;
        void start();
      }
    });
    return inFlight;
  };
  return () => {
    if (inFlight) {
      queued = true;
      return inFlight;
    }
    return start();
  };
}
