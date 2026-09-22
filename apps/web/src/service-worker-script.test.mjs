// sw.js 的行为测试 —— 在 Node 里搭一个够用的假 Service Worker 作用域，
// 把真实的 public/sw.js 跑起来，验证缓存与兜底策略。
//
// 为什么值得单独测：这份脚本的兜底一旦写松，就会把缓存里的 index.html 当成
// JS/CSS 交给页面（或者把服务端兜底的 HTML 存进资源 key），最终表现为
// 「升级后白屏」。这些分支只在断网 / 升级瞬间才会走到，手测很难覆盖。

import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const ORIGIN = "https://milevia.test";
const SOURCE = await readFile(new URL("../public/sw.js", import.meta.url), "utf8");
const CACHE_NAME = SOURCE.match(/const CACHE = "([^"]+)"/)[1];

function fakeResponse({ body = "", contentType = "text/plain", ok = true, type = "basic" } = {}) {
  return {
    ok,
    type,
    body,
    headers: { get: (name) => (name.toLowerCase() === "content-type" ? contentType : null) },
    clone: () => fakeResponse({ body, contentType, ok, type }),
  };
}

const html = (body) => fakeResponse({ body, contentType: "text/html" });

function fakeRequest(url, { mode = "same-origin", method = "GET" } = {}) {
  return { url: new URL(url, ORIGIN).href, method, mode };
}

/** 建一个假 SW 作用域，返回派发事件与观测缓存的能力。 */
function createScope({
  network = () => fakeResponse({}),
  cachedShell = null,
  existingCaches = [],
  failOpen = false,
  failKeys = false,
} = {}) {
  const handlers = new Map();
  const stores = new Map();
  const calls = { skipWaiting: 0, claim: 0 };

  for (const name of existingCaches) stores.set(name, new Map());
  const store = (name) => {
    if (!stores.has(name)) stores.set(name, new Map());
    return stores.get(name);
  };
  const keyOf = (input) => (typeof input === "string" ? new URL(input, ORIGIN).href : input.url);

  // 预置一份「上一版遗留下来的外壳」，模拟升级前那次成功导航缓存下来的 index.html。
  if (cachedShell !== null) store(CACHE_NAME).set(`${ORIGIN}/`, cachedShell);

  const sandbox = {
    URL,
    Promise,
    console,
    // 真实 fetch 不会同步抛错，失败一律是 rejected promise；桩也要照这个语义来。
    fetch: (input) => {
      try {
        return Promise.resolve(network(keyOf(input)));
      } catch (error) {
        return Promise.reject(error);
      }
    },
    caches: {
      // match 允许传字符串（脚本里 caches.match("/") 就是这种用法）。
      match: async (input) => {
        for (const entries of stores.values()) {
          const hit = entries.get(keyOf(input));
          if (hit) return hit;
        }
        return undefined;
      },
      keys: async () => {
        if (failKeys) throw new Error("keys unavailable");
        return [...stores.keys()];
      },
      delete: async (name) => stores.delete(name),
      open: async (name) => {
        if (failOpen) throw new Error("cache storage unavailable");
        const entries = store(name);
        return {
          match: async (input) => entries.get(keyOf(input)),
          add: async (url) => {
            const res = await network(new URL(url, ORIGIN).href);
            if (!res.ok) throw new Error(`add failed: ${url}`);
            entries.set(keyOf(url), res);
          },
          put: async (input, res) => { entries.set(keyOf(input), res); },
        };
      },
    },
    self: {
      location: { origin: ORIGIN },
      addEventListener: (type, handler) => handlers.set(type, handler),
      skipWaiting: () => { calls.skipWaiting += 1; return Promise.resolve(); },
      clients: { claim: () => { calls.claim += 1; return Promise.resolve(); } },
    },
  };
  vm.runInNewContext(SOURCE, sandbox, { filename: "sw.js" });

  const drainMicrotasks = async () => { for (let i = 0; i < 8; i += 1) await Promise.resolve(); };
  /** 派发一次 fetch；handler 没调 respondWith（例如 api 请求）时返回 undefined。 */
  const dispatchFetch = (request) => {
    let responded;
    handlers.get("fetch")({ request, respondWith: (promise) => { responded = promise; } });
    return responded;
  };
  const dispatchLifecycle = async (type) => {
    let waited;
    handlers.get(type)({ waitUntil: (promise) => { waited = promise; } });
    await waited;
    await drainMicrotasks();
  };

  return {
    dispatchFetch,
    dispatchLifecycle,
    drainMicrotasks,
    cachedUrls: () => [...stores.values()].flatMap((entries) => [...entries.keys()]).sort(),
    storeNames: () => [...stores.keys()],
    calls,
  };
}

// ── 兜底策略 ────────────────────────────────────────────────────────────────

test("资源请求失败时绝不拿缓存里的 HTML 顶包（白屏的最后一环）", async () => {
  const scope = createScope({
    network: () => { throw new Error("network down"); },
    cachedShell: html("<!doctype html>上一版外壳"),
  });
  await assert.rejects(
    scope.dispatchFetch(fakeRequest("/assets/index-abc.js")),
    "资源请求失败必须直接失败，不能回退成缓存的 HTML",
  );
});

test("导航请求失败时回退到缓存外壳（网页的离线可用性保留）", async () => {
  const scope = createScope({
    network: () => { throw new Error("network down"); },
    cachedShell: html("<!doctype html>上一版外壳"),
  });
  const fallback = await scope.dispatchFetch(fakeRequest("/", { mode: "navigate" }));
  assert.equal(fallback.body, "<!doctype html>上一版外壳");
});

test("服务端把 HTML 兜底给资源请求（Tauri 就是这样）时，不写进缓存", async () => {
  const scope = createScope({ network: () => html("<!doctype html>index") });
  const response = await scope.dispatchFetch(fakeRequest("/assets/index-abc.js"));
  await scope.drainMicrotasks();
  assert.equal(response.body, "<!doctype html>index", "响应原样返回（兜底是服务端行为，SW 改不了）");
  assert.deepEqual(scope.cachedUrls(), [], "但绝不能把它按 .js 的 key 存下来");
});

test("正常的 JS 响应会被缓存", async () => {
  const scope = createScope({
    network: () => fakeResponse({ body: "console.log(1)", contentType: "text/javascript" }),
  });
  await scope.dispatchFetch(fakeRequest("/assets/index-abc.js"));
  await scope.drainMicrotasks();
  assert.deepEqual(scope.cachedUrls(), [`${ORIGIN}/assets/index-abc.js`]);
});

test("非 200 / 跨域 / 失败的响应不写缓存", async () => {
  const scope = createScope({
    network: () => fakeResponse({ body: "nope", contentType: "text/html", ok: false }),
  });
  await scope.dispatchFetch(fakeRequest("/index.html", { mode: "navigate" }));
  await scope.drainMicrotasks();
  assert.deepEqual(scope.cachedUrls(), []);
});

test("api / ws / 非 GET 请求不进 SW", async () => {
  const scope = createScope();
  assert.equal(scope.dispatchFetch(fakeRequest("/api/projects")), undefined);
  assert.equal(scope.dispatchFetch(fakeRequest("/v1/snapshot")), undefined);
  assert.equal(scope.dispatchFetch(fakeRequest("/ws/events")), undefined);
  assert.equal(scope.dispatchFetch(fakeRequest("/assets/index-abc.js", { method: "POST" })), undefined);
});

// ── 生命周期 ────────────────────────────────────────────────────────────────

test("activate 只清本应用的历史缓存，不动同一 origin 上别人的缓存", async () => {
  const scope = createScope({ existingCaches: ["milevia-shell-v1", "some-other-cache", CACHE_NAME] });
  await scope.dispatchLifecycle("activate");
  assert.deepEqual(scope.storeNames().sort(), ["some-other-cache", CACHE_NAME].sort());
  assert.equal(scope.calls.claim, 1, "清完缓存要 claim 客户端");
});

test("缓存全 miss 的离线导航要明确失败，不能 resolve 成 undefined", async () => {
  const scope = createScope({ network: () => { throw new Error("offline"); } });
  await assert.rejects(scope.dispatchFetch(fakeRequest("/", { mode: "navigate" })));
});

test("install 时个别预缓存失败也不能让整次安装失败", async () => {
  const scope = createScope({
    network: (url) => {
      if (url.endsWith("/mobile")) throw new Error("boom");
      return html("ok");
    },
  });
  await scope.dispatchLifecycle("install");
  // 装不上 SW 会退化成「旧 SW 永远留着」，比少一个预缓存条目糟得多。
  assert.equal(scope.calls.skipWaiting, 1);
});

test("CacheStorage 打不开时 SW 仍要装完并接管（否则旧 SW 一直留着）", async () => {
  const install = createScope({ failOpen: true });
  await install.dispatchLifecycle("install");
  assert.equal(install.calls.skipWaiting, 1);

  const activate = createScope({ failKeys: true });
  await activate.dispatchLifecycle("activate");
  assert.equal(activate.calls.claim, 1);
});

test("缓存名已随本次修复推进（不推进的话旧缓存永远不会被清）", () => {
  assert.notEqual(CACHE_NAME, "milevia-shell-v1");
});
