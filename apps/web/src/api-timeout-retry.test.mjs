import assert from "node:assert/strict";
import { mock } from "node:test";
import test from "node:test";
import { api } from "./lib/api.ts";

// 内部 15s 超时不应立刻判服务不可用：幂等请求应获得一次放宽到 2x 的重试机会，
// 服务端只是瞬时忙（单 SQLite 连接被长事务占住/慢探测）时第二次能成功，避免
// 误导用户去重启 Milevia。真持续无响应才报"持续未响应"。
test("retries an idempotent request once with a longer window on timeout", async () => {
  mock.timers.enable({ apis: ["setTimeout"] });
  const originalFetch = globalThis.fetch;
  try {
    let fetchCalls = 0;
    // fetch 永不返回：只有被内部 AbortController 中止时才以 AbortError 拒绝。
    globalThis.fetch = (_url, init) => {
      fetchCalls++;
      return new Promise((_resolve, reject) => {
        const signal = init.signal;
        const abort = () => reject(new DOMException("Aborted", "AbortError"));
        if (signal?.aborted) {
          abort();
          return;
        }
        signal?.addEventListener("abort", abort, { once: true });
      });
    };
    const flush = async () => {
      await Promise.resolve();
      await new Promise((resolve) => setImmediate(resolve));
    };

    const pending = api("/timeout", { method: "GET" });
    // 立即挂上断言与兜底 catch，避免窗口耗尽瞬间出现 unhandledRejection。
    const rejected = assert.rejects(pending, /控制服务持续未响应，请重启 Milevia 后重试/);
    pending.catch(() => {});

    // 第一次尝试：默认 15s 窗口到点 → 内部超时。
    mock.timers.tick(15_001);
    await flush();
    assert.equal(fetchCalls, 1, "first attempt should time out without rejecting");

    // 回退 500ms 后自动发起第二次尝试（此时应有 fetchCalls=2）。
    mock.timers.tick(501);
    await flush();
    assert.equal(fetchCalls, 2, "timeout should be retried once for an idempotent GET");

    // 第二次尝试窗口放宽为 2x（30s）：过 15s 仍不应拒绝。
    mock.timers.tick(15_001);
    await flush();
    assert.equal(fetchCalls, 2, "retry is still within its longer window");

    // 第二次尝试的 30s 窗口也耗尽 → 最终以"持续未响应"拒绝。
    mock.timers.tick(15_001);
    await flush();
    await rejected;
  } finally {
    globalThis.fetch = originalFetch;
    mock.timers.reset();
  }
});

test("non-idempotent requests still fail fast on timeout without auto-retry", async () => {
  mock.timers.enable({ apis: ["setTimeout"] });
  const originalFetch = globalThis.fetch;
  try {
    let fetchCalls = 0;
    globalThis.fetch = (_url, init) => {
      fetchCalls++;
      return new Promise((_resolve, reject) => {
        const signal = init.signal;
        const abort = () => reject(new DOMException("Aborted", "AbortError"));
        if (signal?.aborted) {
          abort();
          return;
        }
        signal?.addEventListener("abort", abort, { once: true });
      });
    };
    const flush = async () => {
      await Promise.resolve();
      await new Promise((resolve) => setImmediate(resolve));
    };

    const pending = api("/timeout", { method: "POST" });
    const rejected = assert.rejects(pending, /控制服务未在 15 秒内响应，请稍后重试/);
    pending.catch(() => {});
    mock.timers.tick(15_001);
    await flush();
    await rejected;
    assert.equal(fetchCalls, 1, "POST should not auto-retry on timeout (double-submit risk)");
  } finally {
    globalThis.fetch = originalFetch;
    mock.timers.reset();
  }
});

// 长任务表的意义：装/升级 CLI、跑 git 写操作这类操作，**服务端自己的预算是分钟级**
// （agentUpdateTimeout 默认 15 分钟）。客户端拿 15 秒去等，得到的就是"升级成功了、
// 界面说失败了"—— 用户还会据此再点一次。这里钉住三件事：等得住、不误报失败、
// 以及超时文案不说"失败"（服务端多半还在跑）。
test("long-running endpoints wait for the server's own budget instead of 15s", async () => {
  mock.timers.enable({ apis: ["setTimeout"] });
  const originalFetch = globalThis.fetch;
  try {
    let fetchCalls = 0;
    globalThis.fetch = (_url, init) => {
      fetchCalls++;
      return new Promise((_resolve, reject) => {
        const signal = init.signal;
        const abort = () => reject(new DOMException("Aborted", "AbortError"));
        if (signal?.aborted) {
          abort();
          return;
        }
        signal?.addEventListener("abort", abort, { once: true });
      });
    };
    const flush = async () => {
      await Promise.resolve();
      await new Promise((resolve) => setImmediate(resolve));
    };

    const pending = api("/api/runners/windows-local/agents/claude-code/update", { method: "POST" });
    let settled = false;
    pending.then(() => { settled = true; }, () => { settled = true; });
    const rejected = assert.rejects(pending, /这项操作超过 16 分钟仍未返回。服务端可能仍在后台继续执行/);
    pending.catch(() => {});

    // 15 秒到点：普通请求在这里就该判失败了，升级不。
    mock.timers.tick(15_001);
    await flush();
    assert.equal(settled, false, "升级不该在 15 秒被判失败");
    assert.equal(fetchCalls, 1, "升级没有重试资格（重跑一次 npm 安装不是重试，是重做）");

    // 服务端自己的预算（15 分钟）走完才轮到客户端说话。
    mock.timers.tick(16 * 60_000 - 15_001 + 1);
    await flush();
    await rejected;
  } finally {
    globalThis.fetch = originalFetch;
    mock.timers.reset();
  }
});

test("long-running endpoints are never auto-retried, even when they are GETs", async () => {
  mock.timers.enable({ apis: ["setTimeout"] });
  const originalFetch = globalThis.fetch;
  try {
    let fetchCalls = 0;
    globalThis.fetch = (_url, init) => {
      fetchCalls++;
      return new Promise((_resolve, reject) => {
        const signal = init.signal;
        const abort = () => reject(new DOMException("Aborted", "AbortError"));
        if (signal?.aborted) {
          abort();
          return;
        }
        signal?.addEventListener("abort", abort, { once: true });
      });
    };
    const flush = async () => {
      await Promise.resolve();
      await new Promise((resolve) => setImmediate(resolve));
    };

    // /api/runtimes/catalog 是 GET，但它是 registry 往返（服务端预算到秒级），
    // 不属于"服务端瞬时忙"那一档 —— 重试一次只是把同样的网络往返再打一遍。
    const pending = api("/api/runtimes/catalog");
    const rejected = assert.rejects(pending, /这项操作超过 1 分钟仍未返回/);
    pending.catch(() => {});
    mock.timers.tick(60_001);
    await flush();
    await rejected;
    assert.equal(fetchCalls, 1, "长任务 GET 也不该吃到那次 2x 重试");
  } finally {
    globalThis.fetch = originalFetch;
    mock.timers.reset();
  }
});

// 表容易漏 —— 漏一个的后果就是"这个按钮又变成 15 秒判失败"。所以把 CLI 管理页
// （pages/CliToolsPage.tsx）真正会打的那几条端点逐个钉住：15 秒到点时**不能**有结论。
// 这也是把"入表"这件事从注释变成断言：以后页面新增一个长端点，这里就该多一行。
/**
 * 钉住"这一组 (方法, 路径) 在 15 秒时**不能**有结论"。
 *
 * 两个用途：CLI 管理页真正会打的那几条（漏一个就是"这个按钮又变回 15 秒判失败"），
 * 以及其余逐条确证过的长任务。反向对照（不在表里的仍在 15 秒判失败）由调用方自己补 ——
 * 没有它，这一片断言在"表退化成什么都匹配"时照样全绿。
 */
async function expectNoVerdictAt15s(cases, { controlMethod, controlPath }) {
  mock.timers.enable({ apis: ["setTimeout"] });
  const originalFetch = globalThis.fetch;
  try {
    globalThis.fetch = (_url, init) =>
      new Promise((_resolve, reject) => {
        const signal = init.signal;
        const abort = () => reject(new DOMException("Aborted", "AbortError"));
        if (signal?.aborted) {
          abort();
          return;
        }
        signal?.addEventListener("abort", abort, { once: true });
      });
    const flush = async () => {
      await Promise.resolve();
      await new Promise((resolve) => setImmediate(resolve));
    };

    for (const [method, path] of cases) {
      const controller = new AbortController();
      const pending = api(path, { method, signal: controller.signal });
      let settled = false;
      pending.then(() => { settled = true; }, () => { settled = true; });
      pending.catch(() => {});
      mock.timers.tick(15_001);
      await flush();
      assert.equal(settled, false, `${method} ${path} 在 15 秒就被判了失败（没进长任务表？）`);
      controller.abort();
      await flush();
    }

    // 反向对照：不在表里的请求**仍然**在 15 秒判失败。没有这一条，上面那片断言在
    // "表退化成什么都匹配"时照样全绿 —— 而那种表恰恰把它要防的"真卡死"拖到分钟级才报。
    const control = api(controlPath, { method: controlMethod });
    const controlRejected = assert.rejects(control, /控制服务未在 15 秒内响应/);
    control.catch(() => {});
    mock.timers.tick(15_001);
    await flush();
    await controlRejected;
  } finally {
    globalThis.fetch = originalFetch;
    mock.timers.reset();
  }
}

test("every CLI-management endpoint a page calls is on the long-task table", async () => {
  await expectNoVerdictAt15s([
    ["POST", "/api/runners/windows-local/agents/claude-code/install"],
    ["POST", "/api/runners/windows-local/agents/claude-code/update"],
    ["POST", "/api/runners/windows-local/agents/codex/repair"],
    ["POST", "/api/runners/windows-local/runtime/install"],
    ["GET", "/api/runners/windows-local/agents/claude-code/diagnose"],
    ["POST", "/api/runners/windows-local/agents/claude-code/check-update"],
    ["GET", "/api/runtimes/catalog"],
    // 这台机器上的工具清单与批量诊断（页面的卡片区靠它们）
    ["GET", "/api/runners/windows-local/agents"],
    ["GET", "/api/runners/windows-local/diagnostics"],
  ], { controlMethod: "POST", controlPath: "/api/preferences" });
});

test("同一条路径上的读不算长任务：判据必须看方法", async () => {
  // 这几条路径的**写**操作都在表里（POST /api/projects 建项目、POST /git/branches 建分支、
  // POST /api/ssh-connections 建连接、POST /terminal/sessions 起终端）。如果判据只看路径
  // 不看方法，这些快读就会跟着拿到分钟级预算 —— 既丢掉 15 秒的早发现，
  // 又让它们失去"重试一次"的资格。
  mock.timers.enable({ apis: ["setTimeout"] });
  const originalFetch = globalThis.fetch;
  try {
    globalThis.fetch = (_url, init) =>
      new Promise((_resolve, reject) => {
        const signal = init.signal;
        const abort = () => reject(new DOMException("Aborted", "AbortError"));
        if (signal?.aborted) {
          abort();
          return;
        }
        signal?.addEventListener("abort", abort, { once: true });
      });
    const flush = async () => {
      await Promise.resolve();
      await new Promise((resolve) => setImmediate(resolve));
    };

    for (const path of ["/api/projects", "/api/projects/p1/git/branches", "/api/ssh-connections", "/api/projects/p1/terminal/sessions"]) {
      const read = api(path, { method: "GET" });
      const readRejected = assert.rejects(read, /控制服务未在 15 秒内响应|控制服务持续未响应/);
      read.catch(() => {});
      mock.timers.tick(15_001);
      await flush();
      mock.timers.tick(501);
      await flush();
      mock.timers.tick(30_001);
      await flush();
      await readRejected;
    }
  } finally {
    globalThis.fetch = originalFetch;
    mock.timers.reset();
  }
});

test("其余逐条确证过的长任务端点也在表里", async () => {
  // 每一条都对应一类"报失败、其实做成了"的用户可见后果：
  //   · 删项目 —— 说失败，刷新后项目没了，用户以为没删掉、再删一次；
  //   · 清空会话 —— 说失败，历史其实已经清了；
  //   · 保存文件 —— 说失败，文件其实写进去了，重试就是重复保存或撞 workspace 冲突；
  //   · git 写操作 —— 说失败，提交/推送其实已经发生；
  //   · 编排的合并/清理 —— 合并说失败（其实已合并，再点会吃 409"分支不可合并"）、
  //     清理说失败（其实已经清干净，用户白等一轮）。
  // 这几条都是**写操作**，服务端已经被 detachWriteContext 脱开，所以"客户端放弃"与
  // "事情是否发生"在这里会分家 —— 正是这一档最需要长预算。
  await expectNoVerdictAt15s([
    ["DELETE", "/api/projects/p1"],
    ["POST", "/api/conversations/c1/clear"],
    ["PUT", "/api/projects/p1/fs/write"],
    ["POST", "/api/projects/p1/git/push"],
    ["POST", "/api/projects/p1/git/fetch"],
    ["POST", "/api/projects/p1/git/stage-all"],
    ["POST", "/api/projects/p1/git/commits"],
    ["POST", "/api/tasks/t1/orchestration/merge-main"],
    ["POST", "/api/tasks/t1/orchestration/cleanup"],
    // 同族的 pause/resume/stop 是单条事务，**故意**不进表：它们要保留 15 秒的假死探测。
  ], { controlMethod: "POST", controlPath: "/api/tasks/t1/orchestration/stop" });
});
