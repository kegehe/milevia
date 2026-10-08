import type { AnchorHTMLAttributes, MouseEvent, ReactNode } from "react";
import { decideExternalLinkClick, isExternalHref } from "../lib/external-link";
import { isDesktop, openExternal } from "../lib/runtime";

/**
 * 外链锚点：把**会被宿主拒掉的**点击改道到 openExternal，其余属性一律原样透传。
 *
 * 为什么要改道：Tauri 的两个窗口都 `.on_new_window(|_, _| NewWindowResponse::Deny)`，
 * `on_navigation` 也只放行本地源 —— 任何走到"浏览器默认动作"的点击在桌面端都**毫无反应**
 * （不是打开得慢，是没有任何动静），而 `window.open` 那条退路同样被拒。判据全在
 * `lib/external-link.ts`（纯函数、逐格可测），本组件只把结论翻成
 * `preventDefault` + `openExternal`。
 *
 * 实测结论（2026-09-29，`.tmp/link-click/` 真组件 + 真 Chromium，判据是
 * `invoke("open_external")` 有没有被调用）：
 *   · 左键 http(s)：接管 ✓；
 *   · **Ctrl/Cmd/Shift+左键**：`click` 照常触发、`preventDefault()` 能压掉"开新标签页"这个
 *     默认动作，所以桌面端**也有效**；Web 端则相反，这里**不拦**，把后台标签页还给浏览器；
 *   · **中键**：只发 `auxclick`、不发 `click` —— 桌面端必须挂 onAuxClick，否则是死链
 *     （默认动作开的新窗口被外壳 Deny）；Web/手机不拦才是对的（原生后台标签页）；
 *   · **`mailto:`**：桌面端接管（宿主交给系统邮件客户端，与 Web/手机端的原生行为对齐）；
 *   · **协议相对 `//host`**：桌面端**先归一成 `https://host`** 再接管 —— 直接丢给
 *     openExternal 会因为 `new URL("//host")` 抛异常而静默什么都不做，比不接管更糟。
 *
 * ⚠️ 别把"桌面端接管"读成"桌面端什么协议都能开"：`ftp:`/`file:`/`javascript:`/`tel:` 一概不接管，
 * 桌面端本来也打不开（导航白名单只放行本地源，`open_external` 只放行 http/https/mailto）。
 * 不接管它们只是为了不在 Web 端把这些协议弄坏 —— `ftp:` 现在在所有浏览器里都已失效
 * （Chrome 88 起移除 FTP），它哪一端都是死链，不值得为它放宽任何白名单。
 *
 * 为什么不动属性：调用方各自有既定语义（文件页只给 `isExternal` 加 target，
 * 消息里的 Markdown 一律加 target）。所以 `target`/`rel` 由调用方决定，本组件只多挂
 * onClick / onAuxClick —— **只有一个例外**：调用方**没给** `target` 的外链，这里补
 * `target="_blank"`。原因是安全相关：Web 端会把带修饰键的点击交还浏览器，而那只有在有
 * `target` 时才等于"开新标签页"；实测无 `target` 的锚点按 **Win 键(Meta)+左键**会**同窗导航**，
 * 把整个应用顶掉（Ctrl/Shift/中键不受影响）。补了的代价为零（内部链接不受影响 —— 它们
 * 本来就"不是外链"，走不到这一档）。
 */
export function ExternalLink({ href, children, onClick, onAuxClick, ...attributes }: { href?: string; children: ReactNode } & AnchorHTMLAttributes<HTMLAnchorElement>) {
  const desktop = isDesktop();
  // 调用方显式给的 `target` 一律不动；没给且是外链才补 `_blank`（见上面那段）。
  const target = attributes.target ?? (isExternalHref(href, desktop) ? "_blank" : undefined);
  const intercept = (event: MouseEvent<HTMLAnchorElement>) => {
    const decision = decideExternalLinkClick(href, {
      desktop,
      button: event.button,
      modifiers: { ctrl: event.ctrlKey, meta: event.metaKey, shift: event.shiftKey, alt: event.altKey },
    });
    if (decision.action !== "open-external") return;
    event.preventDefault();
    void openExternal(decision.url);
  };
  return (
    <a
      {...attributes}
      href={href}
      target={target}
      onClick={(event) => {
        onClick?.(event);
        if (event.defaultPrevented) return;
        intercept(event);
      }}
      onAuxClick={(event) => {
        onAuxClick?.(event);
        if (event.defaultPrevented) return;
        intercept(event);
      }}
    >
      {children}
    </a>
  );
}
