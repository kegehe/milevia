import assert from "node:assert/strict";
import test from "node:test";
import { purgeServiceWorkerState, serviceWorkerPolicy } from "./service-worker.ts";

// ── 注册策略矩阵 ────────────────────────────────────────────────────────────
// 桌面端与手机 App 的页面资源随包发布，绝不能注册 SW；已经注册过的还要主动清掉。

test("桌面端走 purge：注销历史注册并清缓存", () => {
  assert.equal(
    serviceWorkerPolicy({ isTray: false, isDesktop: true, isNativePlatform: false, protocol: "https:" }),
    "purge",
  );
});

test("手机 App（Capacitor 原生）走 purge", () => {
  assert.equal(
    serviceWorkerPolicy({ isTray: false, isDesktop: false, isNativePlatform: true, protocol: "https:" }),
    "purge",
  );
});

test("托盘面板窗口不动注册（主窗已经处理过）", () => {
  for (const isDesktop of [true, false]) {
    assert.equal(
      serviceWorkerPolicy({ isTray: true, isDesktop, isNativePlatform: false, protocol: "https:" }),
      "skip",
    );
  }
});

test("普通网页仍然注册 SW（手机浏览器打开的远程页）", () => {
  assert.equal(
    serviceWorkerPolicy({ isTray: false, isDesktop: false, isNativePlatform: false, protocol: "https:" }),
    "register",
  );
  assert.equal(
    serviceWorkerPolicy({ isTray: false, isDesktop: false, isNativePlatform: false, protocol: "http:" }),
    "register",
  );
});

test("非 http(s) 页面（如 file:）不注册", () => {
  assert.equal(
    serviceWorkerPolicy({ isTray: false, isDesktop: false, isNativePlatform: false, protocol: "file:" }),
    "skip",
  );
});

// ── 清理动作 ────────────────────────────────────────────────────────────────

/** 临时替换全局（Node 里 navigator 是只读 getter，只能改属性描述符），跑完还原。 */
function withGlobals<T>(values: Record<string, unknown>, run: () => Promise<T>): Promise<T> {
  const saved = new Map<string, PropertyDescriptor | undefined>();
  for (const [key, value] of Object.entries(values)) {
    saved.set(key, Object.getOwnPropertyDescriptor(globalThis, key));
    Object.defineProperty(globalThis, key, { value, configurable: true, writable: true });
  }
  return run().finally(() => {
    for (const [key, descriptor] of saved) {
      if (descriptor) Object.defineProperty(globalThis, key, descriptor);
      else Reflect.deleteProperty(globalThis, key);
    }
  });
}

test("purgeServiceWorkerState 注销全部注册并删掉全部缓存", async () => {
  const unregistered: string[] = [];
  const deleted: string[] = [];

  await withGlobals({
    navigator: {
      serviceWorker: {
        getRegistrations: async () => [
          { unregister: async () => { unregistered.push("a"); return true; } },
          { unregister: async () => { unregistered.push("b"); return true; } },
        ],
      },
    },
    caches: {
      keys: async () => ["milevia-shell-v1", "milevia-shell-v2"],
      delete: async (key: string) => { deleted.push(key); return true; },
    },
  }, () => purgeServiceWorkerState());

  assert.deepEqual(unregistered, ["a", "b"], "两个注册都要注销");
  assert.deepEqual(deleted.sort(), ["milevia-shell-v1", "milevia-shell-v2"], "缓存要清空");
});

test("没有 SW API 时仍然会清缓存（缓存才是白屏的根源，注册只是载体）", async () => {
  const deleted: string[] = [];
  await withGlobals({
    navigator: {},
    caches: {
      keys: async () => ["milevia-shell-v1"],
      delete: async (key: string) => { deleted.push(key); return true; },
    },
  }, () => purgeServiceWorkerState());

  assert.deepEqual(deleted, ["milevia-shell-v1"]);
});

test("navigator 与 caches 都不可用时静默返回", async () => {
  await withGlobals({ navigator: {}, caches: undefined }, () => purgeServiceWorkerState());
});

test("注销失败时仍然会去清缓存", async () => {
  const deleted: string[] = [];
  await withGlobals({
    navigator: { serviceWorker: { getRegistrations: async () => { throw new Error("nope"); } } },
    caches: {
      keys: async () => ["milevia-shell-v1"],
      delete: async (key: string) => { deleted.push(key); return true; },
    },
  }, () => purgeServiceWorkerState());

  assert.deepEqual(deleted, ["milevia-shell-v1"], "注销失败不能连着跳过清缓存");
});
