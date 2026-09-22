// Service Worker 的注册策略 —— 应用外壳（桌面 Tauri / 手机 App）一律不注册。
//
// ── 为什么应用外壳必须没有 SW ────────────────────────────────────────────
// 桌面端和手机 App 的页面与资源都是**随包发布的本地文件**：外壳和 /assets/index-<hash>.js
// 同生共死。缓存它们没有任何收益，却会引入一个必然踩中的失败模式：
//
//   1. SW 缓存里留着上一版的外壳 index.html（`caches.put` 成功一次就会留着，
//      而缓存名不变就永远不会失效）；
//   2. 升级后第一次启动，若 SW 的 `fetch` 打不通（升级时旧进程刚被杀、WebView2
//      浏览器进程还在交接），`sw.js` 的兜底会把这份**旧外壳**当成文档返回；
//   3. 旧外壳引用 `/assets/index-<旧hash>.js`，新安装包里没有这个文件，
//      而 Tauri 的资源处理器对**任何找不到的路径**都兜底返回 index.html
//      （tauri `manager/mod.rs` 的 `get_asset`，第三级 fallback），content-type 是 text/html；
//   4. `<script type="module">` 拿到 HTML → MIME 检查失败 → React 从不挂载 → **白屏**。
//
// 实测复现与生产缓存痕迹见 `.tmp/whitescreen/repro.mjs`（旧资源 URL 的响应体是 HTML，
// 与 `%LOCALAPPDATA%\com.milevia.desktop\EBWebView` 里那次事故的缓存条目形状一致）。
// 第二次启动会自愈（成功导航会把旧外壳覆盖掉），所以现象是「升级后白屏一次，重启就好」——
// 但它每次升级都会再来一次。
//
// 真正的网页（手机浏览器打开远程页）仍然注册 SW：那边离线可用是有意义的，
// 且外壳与资源由同一个服务端发布，不会出现「HTML 冒充 JS」的错配。

export type ServiceWorkerPolicy =
  /** 注销历史注册并清空 CacheStorage —— 应用外壳走这条，同时修复已装旧版本的机器。 */
  | "purge"
  /** 正常注册。 */
  | "register"
  /** 什么都不做（托盘面板窗口，清理交给主窗）。 */
  | "skip";

export function serviceWorkerPolicy(env: {
  isTray: boolean;
  isDesktop: boolean;
  isNativePlatform: boolean;
  protocol: string;
}): ServiceWorkerPolicy {
  // 托盘面板与主窗共用一个 profile / 同一个源，主窗已经清过了。
  if (env.isTray) return "skip";
  if (env.isDesktop || env.isNativePlatform) return "purge";
  return env.protocol.startsWith("http") ? "register" : "skip";
}

/**
 * 注销 SW 并清空 CacheStorage。
 *
 * 只改注册条件是不够的：已经装过旧版本的机器，profile 里那份旧外壳和旧注册还在，
 * 不主动清掉的话，本次修复上线后的第一次启动仍会白屏，之后也一直带着这个隐患。
 * 失败不影响启动（没有 SW 时下一次导航本来就走网络），所以整体吞掉异常。
 */
export async function purgeServiceWorkerState(): Promise<void> {
  // 注销与清缓存分别 try：两者是独立的，注销失败不能连累清缓存 ——
  // 真正造成白屏的是缓存里的旧外壳，注册只是它的载体。
  try {
    if (typeof navigator !== "undefined" && "serviceWorker" in navigator) {
      const registrations = await navigator.serviceWorker.getRegistrations();
      await Promise.all(registrations.map((registration) => registration.unregister()));
    }
  } catch {
    // 注销失败就退而求其次：至少把缓存清掉。
  }
  try {
    if (typeof caches !== "undefined") {
      // 这里删掉该 source 下的全部缓存（不按名字前缀过滤）：应用外壳跑在
      // tauri.localhost / Capacitor 自己的 origin 上，这个源只属于本应用，
      // 而历史版本用过的缓存名未必都在同一前缀下，宁可清干净。
      const keys = await caches.keys();
      await Promise.all(keys.map((key) => caches.delete(key)));
    }
  } catch {
    // CacheStorage 不可用时无需处理：没有缓存就没有「旧外壳」这条路径。
  }
}
