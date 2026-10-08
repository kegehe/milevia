// 外链点击的判据（纯函数：页面只渲染结论）。
//
// 为什么单独抽出来：这套判据**分平台、分按键、分协议**，四五个条件塞进 JSX 里就没法在 CI 里
// 逐格验证，而每一格都对应一个用户看得见的后果 —— 桌面端"点下去毫无反应"，或者 Web 端
// 被抢掉浏览器原生的后台标签页。判据放这儿，`external-link.test.ts` 逐格钉住；
// `ExternalLink.tsx` 只把结论翻成 `preventDefault` + `openExternal`。
//
// 全部结论都是 2026-09-29 用真组件 + 真 Chromium 量出来的（`apps/web/.tmp/link-click/`），
// 不是照文档推的：判据是 `invoke("open_external")` 有没有被调用（调了 = 接管 = 桌面端能打开）。

import { externalLinkProtocols } from "@milevia/sdk";

/** 桌面端能交给宿主的协议（`packages/sdk` 与 Rust `open_external` 的白名单同款）。 */
export type ExternalLinkClick = { action: "open-external"; url: string } | { action: "native" };

export type ExternalLinkModifiers = {
  ctrl?: boolean;
  meta?: boolean;
  shift?: boolean;
  alt?: boolean;
};

const HTTP = /^https?:\/\//i;
const MAILTO = /^mailto:/i;
/** 协议相对地址（`//host/path`）：**排除** `//` 开头的其它东西（如 `///`）。 */
const PROTOCOL_RELATIVE = /^\/\/[^/]/;

/**
 * 这一格的 href 能不能交给宿主打开；不能就返回 undefined（保持浏览器原生行为）。
 *
 * 桌面端比 Web 端多认两档，因为宿主的 `open_external` 是把 URL 交给**系统协议处理器**：
 *   · `mailto:` —— 有确定去处（系统邮件客户端）；
 *   · 协议相对 `//host` —— **必须先归一成绝对地址**：SDK 里 `new URL("//host")` 会抛异常并
 *     静默 return，直接接管反而比不接管更糟（Web 端原本靠浏览器自己解析，是能用的）。
 * 其余协议（`ftp:`/`file:`/`javascript:`/`tel:`）一律不碰：`file:` 与 `javascript:` 是要
 * 挡在门外的，`ftp:` 在所有现代浏览器里都已失效（Chrome 88 起移除 FTP），`tel:` 桌面端
 * 没有去处。不碰它们至少不会把 Web 端本来能用的行为弄坏。
 *
 * 最后一道是**真解析 + 共享白名单**（`externalLinkProtocols` 与 `openExternal` 同一份）：
 * 只有 `new URL()` 解析得动、且协议在白名单里的地址才算"能开"。这样"判据放行 ⇒ SDK 一定
 * 会接受"成了结构性事实，而不是靠两处各写一遍的字符串碰巧一致。反例都在实测里核过：
 * `//host:99999/x`（端口越界）与 `https://`（无主机）会解析失败 → 判为 native，而浏览器
 * 对这种 href 本来就什么都不做（`.tmp/probe-link-meta.mjs` 量过：不同窗导航、不开新页）。
 */
function openableURL(href: string, desktop: boolean): string | undefined {
  let candidate: string | undefined;
  if (HTTP.test(href)) candidate = href;
  else if (desktop && MAILTO.test(href)) candidate = href;
  else if (desktop && PROTOCOL_RELATIVE.test(href)) candidate = `https:${href}`;
  if (!candidate) return undefined;
  let parsed: URL;
  try {
    parsed = new URL(candidate);
  } catch {
    return undefined;
  }
  return externalLinkProtocols(desktop).includes(parsed.protocol) ? candidate : undefined;
}

/**
 * 这个 href 在**当前平台**上算不算"外链"（= 会被改道给宿主/浏览器的地址）。
 *
 * `ExternalLink` 用它决定要不要补 `target="_blank"`，这一条是安全相关：判据在 Web 端会把
 * **带修饰键的点击**交还浏览器，而那只有在锚点带 `target` 时才等于"开新标签页"——
 * 实测（`.tmp/probe-link-meta.mjs`）：**无 `target` 的锚点 + Win 键(Meta)+左键 = 同窗导航**，
 * 整个应用会被外部页面顶掉（Ctrl/Shift/中键不受影响，有 `target` 时逐格都安全）。
 */
export function isExternalHref(href: string | undefined, desktop: boolean): boolean {
  return Boolean(href && openableURL(href, desktop));
}

/**
 * 这次点击该不该由我们接管。
 *
 * - `button`：0 左键、1 中键。右键（2）与其余按键一律不碰，交给浏览器（上下文菜单等）。
 * - 中键：**只有桌面端接管**。桌面端不接管就是死链（中键只发 `auxclick`，不接管就落到
 *   默认动作开新窗口，被外壳 Deny）；Web/手机不接管才是对的（浏览器原生后台标签页）。
 * - 左键 + 修饰键：**只有桌面端接管**。桌面端 `click` 照常触发、`preventDefault()` 能压掉
 *   "开新标签页"的默认动作，所以接管有效；Web 端反过来 —— 拦下来会把原生的后台标签页
 *   变成 `window.open` 的前台标签页。
 */
export function decideExternalLinkClick(
  href: string | undefined,
  context: { desktop: boolean; button: number; modifiers?: ExternalLinkModifiers },
): ExternalLinkClick {
  const { desktop, button, modifiers = {} } = context;
  if (button !== 0 && button !== 1) return { action: "native" };
  const url = href ? openableURL(href, desktop) : undefined;
  if (!url) return { action: "native" };
  if (button === 1) return desktop ? { action: "open-external", url } : { action: "native" };
  if (!desktop && (modifiers.ctrl || modifiers.meta || modifiers.shift || modifiers.alt)) {
    return { action: "native" };
  }
  return { action: "open-external", url };
}
