// ── @milevia/sdk — 平台运行时适配层 ──────────────────────────────────────────
// Web 端和桌面端（Tauri）共用同一套前端代码。
// 桌面端由 Tauri 壳通过 initialization_script 注入
//   window.__MILEVIA_DESKTOP_RUNTIME__，
// Web 端则无此注入，走 Vite proxy 转发路径。

// ── 类型 ────────────────────────────────────────────────────────────────────

export type DesktopRuntimeConfig = {
  apiBase: string;
  wsBase: string;
  sessionToken: string;
  /** 窗口角色：主窗口 `app`；托盘面板窗口 `tray`。Web 端无注入，此字段缺省。 */
  mode?: "app" | "tray";
};

export type Platform = "web" | "desktop";

// ── 全局类型声明 ────────────────────────────────────────────────────────────

declare global {
  interface Window {
    __MILEVIA_DESKTOP_RUNTIME__?: DesktopRuntimeConfig;
  }
}

// ── 平台检测 ────────────────────────────────────────────────────────────────

/** 获取桌面端运行时配置（仅在 Tauri 环境下有效）。 */
export function getDesktopRuntime(): DesktopRuntimeConfig | undefined {
  if (typeof window === "undefined") return undefined;
  const runtime = window.__MILEVIA_DESKTOP_RUNTIME__;
  if (!runtime?.apiBase || !runtime.wsBase || !runtime.sessionToken) return undefined;
  return runtime;
}

/** 检测当前运行平台。 */
export function getPlatform(): Platform {
  return getDesktopRuntime() ? "desktop" : "web";
}

/** 便捷断言：当前是否运行在桌面端 Tauri WebView 中。 */
export function isDesktop(): boolean {
  // Capacitor 原生包即使被宿主注入桌面运行时字段，也必须走移动端 UI。
  const capacitor = (globalThis as typeof globalThis & { Capacitor?: { isNativePlatform?: () => boolean } }).Capacitor;
  if (capacitor?.isNativePlatform?.()) return false;
  return getPlatform() === "desktop";
}

/** 便捷断言：当前是否运行在浏览器（Web 端）。 */
export function isWeb(): boolean {
  return getPlatform() === "web";
}

// ── 网络适配 ────────────────────────────────────────────────────────────────

/**
 * 将 API 路径解析为完整 URL。
 * - 桌面端：基于 sidecar apiBase 拼接绝对地址
 * - Web 端：保持相对路径，由 Vite dev-server 或反向代理转发
 */
export function apiURL(path: string): string {
  const runtime = getDesktopRuntime();
  return runtime ? new URL(path, runtime.apiBase).toString() : path;
}

/**
 * 构造 API 请求头。
 * - 桌面端：附加 X-Milevia-Session 的会话令牌
 * - Web 端：保持原始头不变
 */
export function sessionHeaders(headers?: HeadersInit): Headers {
  const result = new Headers(headers);
  const runtime = getDesktopRuntime();
  if (runtime) result.set("X-Milevia-Session", runtime.sessionToken);
  return result;
}

/**
 * 创建 WebSocket 连接，自动适配桌面/Web 两种场景。
 * - 桌面端：直连 sidecar wsBase，携带 milevia-session.<token> 协议子协商
 * - Web 端：基于当前 location 拼接 ws:// 或 wss://
 */
export function createWebSocket(path: string): WebSocket {
  const runtime = getDesktopRuntime();
  if (!runtime) {
    const protocol = window.location.protocol === "https:" ? "wss" : "ws";
    return new WebSocket(`${protocol}://${window.location.host}${path}`);
  }
  const url = new URL(path, runtime.wsBase).toString();
  return new WebSocket(url, `milevia-session.${runtime.sessionToken}`);
}

/**
 * 外链放行的协议 —— **桌面端与 Web 端各一份**。
 *
 * 导出它，是因为前端 `ExternalLink` 的判据（`apps/web/src/lib/external-link.ts`）必须读同一份：
 * 两处各写一份必然漂移，而漂移的表现恰是"判据说能开、SDK 却静默丢弃"＝点了没反应。
 * 协议名一律带冒号、小写（`URL.protocol` 的形态）。
 */
export function externalLinkProtocols(desktop: boolean): readonly string[] {
  return desktop ? DESKTOP_PROTOCOLS : WEB_PROTOCOLS;
}

const WEB_PROTOCOLS = ["http:", "https:"] as const;
const DESKTOP_PROTOCOLS = ["http:", "https:", "mailto:"] as const;

/**
 * 在系统默认浏览器中打开外部链接。
 * - 桌面端：调用 Rust `open_external` command，由系统协议处理器打开（WebView 本身不导航）
 * - Web 端：开新标签页
 *
 * 白名单**分平台**（`externalLinkProtocols`）：桌面端多放行 `mailto:` —— 宿主那条路会把它
 * 交给系统邮件客户端，与 Web/手机端点 `mailto:` 的原生行为一致；Web 端这一档落到
 * `window.open`，多放行只会让浏览器拿到自己处理不了的协议（换一个空标签页），所以 Web 端
 * 不动。两档都只认**枚举出来的协议**，不认 `file:`/`javascript:`/自定义 scheme —— 这个参数
 * 最终会被交给系统协议处理器。
 *
 * 平台判据用 `isDesktop()`（而不是裸 `getDesktopRuntime()`）：它内建 Capacitor 判据，
 * 与 `ExternalLink` 用的是同一个判据；否则"原生包被注入了桌面运行时字段"这种边角上，
 * 两边会做出不同判断。
 */
export async function openExternal(url: string): Promise<void> {
  let parsed: URL;
  try {
    parsed = new URL(url);
  } catch {
    // 解析不了的静默丢弃。注意 `//host`（协议相对地址）也走这一支：调用方
    // （`ExternalLink` → `lib/external-link.ts`）必须先把它归一成绝对地址再送进来。
    return;
  }
  const desktop = isDesktop();
  if (!externalLinkProtocols(desktop).includes(parsed.protocol)) return;

  if (desktop) {
    const internals = (window as unknown as {
      __TAURI_INTERNALS__?: { invoke: (command: string, args?: Record<string, unknown>) => Promise<unknown> };
    }).__TAURI_INTERNALS__;
    if (internals?.invoke) {
      try {
        await internals.invoke("open_external", { url });
        return;
      } catch {
        // invoke 失败时退回 window.open（桌面端会被新窗口拦截，效果为空；Web 端不受影响）
      }
    }
  }
  const opened = window.open(url, "_blank", "noopener,noreferrer");
  opened?.focus();
}
