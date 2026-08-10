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
