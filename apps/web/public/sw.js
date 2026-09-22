// Milevia 远程页的 Service Worker —— 只服务于「真正的网页」（手机浏览器打开远程页）。
//
// 桌面端（Tauri）与手机 App（Capacitor）**不注册**本文件，并且会主动注销历史注册、
// 清空 CacheStorage，原因见 src/lib/service-worker.ts 顶部：随包发布的本地资源一旦被
// SW 缓存，升级后旧外壳会引用新包里已不存在的 /assets/index-<hash>.js，
// 而缺资源的请求会被兜底成 HTML，最终表现为白屏。
//
// 这份脚本仍然要写得足够安全，原因同上：网页那边服务端也可能对缺失路径返回
// SPA 兜底的 index.html，缓存与兜底策略一旦写松，同样的白屏会在网页上重演。
//
// CACHE 版本号规则：改动本文件任何逻辑时都要往上抬一位。名字不变浏览器就不会重装 SW，
// 旧缓存也就永远不会被 activate 清掉（这正是历史版本踩坑的地方）。
const CACHE = "milevia-shell-v2";
// 只清理本应用自己的缓存：同一个 origin 上可能有别的页面也用 CacheStorage，
// 不能因为版本升级就把别人的东西删掉。
const CACHE_PREFIX = "milevia-shell-";
const SHELL = ["/", "/mobile", "/manifest.webmanifest", "/milevia-mark.svg"];

self.addEventListener("install", (event) => {
  event.waitUntil(
    caches
      .open(CACHE)
      // 逐个预缓存而不是 addAll：任何一个 URL 失败都不该让整次安装失败 ——
      // 装不上 SW 会退化成「旧 SW 永远留着」，那比缺一个预缓存条目糟得多。
      .then((cache) => Promise.all(SHELL.map((url) => cache.add(url).catch(() => undefined))))
      // 连缓存都打不开（配额/存储被禁用）时也要让 SW 装完并接管，
      // 否则旧 SW 会一直留着继续喂旧外壳——那才是白屏的源头。
      .catch(() => undefined)
      .then(() => self.skipWaiting()),
  );
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    caches
      .keys()
      // 清掉历史版本的缓存：旧外壳是白屏的源头，必须随版本一起作废。
      .then((keys) =>
        Promise.all(
          keys
            .filter((key) => key !== CACHE && key.startsWith(CACHE_PREFIX))
            .map((key) => caches.delete(key)),
        ),
      )
      // 清理失败不应连累接管：拿不到缓存列表也得 claim。
      .catch(() => undefined)
      .then(() => self.clients.claim()),
  );
});

self.addEventListener("fetch", (event) => {
  const request = event.request;
  if (request.method !== "GET" || new URL(request.url).origin !== self.location.origin) return;
  const path = new URL(request.url).pathname;
  if (path.startsWith("/api/") || path.startsWith("/v1/") || path.startsWith("/ws/")) return;

  const isNavigation = request.mode === "navigate";

  event.respondWith(
    fetch(request)
      .then((response) => {
        // 只缓存「正常、同源、类型对得上」的响应。
        // 关键一条：非导航请求如果拿到 text/html，说明服务端把 HTML 兜底给了资源请求
        // （Tauri 对缺失路径就是这么做的），这种响应绝不能进缓存 ——
        // 否则一个 .js key 下会长期躺着一份 HTML，后续任何一次兜底都会直接白屏。
        const isHtmlForAsset = !isNavigation && (response.headers.get("content-type") || "").includes("text/html");
        if (response.ok && response.type === "basic" && !isHtmlForAsset) {
          const copy = response.clone();
          caches.open(CACHE).then((cache) => cache.put(request, copy)).catch(() => undefined);
        }
        return response;
      })
      .catch(() => {
        // 兜底只对导航生效：资源请求失败就让它失败，由页面自己报错，
        // 而不是拿缓存里的 HTML 去冒充 JS/CSS（那正是白屏的最后一环）。
        if (!isNavigation) throw new Error("milevia: offline");
        return caches
          .match(request)
          .then((cached) => cached || caches.match("/"))
          // 缓存里也没有外壳（首次离线访问）：明确失败。
          // 不能 resolve 成 undefined —— respondWith 收到非 Response 会抛一条
          // 与真实原因无关的 TypeError，排查时很误导。
          .then((cached) => {
            if (!cached) throw new Error("milevia: offline");
            return cached;
          });
      }),
  );
});
