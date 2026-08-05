export type DesktopRuntimeConfig = {
  apiBase: string;
  wsBase: string;
  sessionToken: string;
};

declare global {
  interface Window {
    __MILEVIA_DESKTOP_RUNTIME__?: DesktopRuntimeConfig;
  }
}

function desktopRuntime(): DesktopRuntimeConfig | undefined {
  if (typeof window === "undefined") return undefined;
  const runtime = window.__MILEVIA_DESKTOP_RUNTIME__;
  if (!runtime?.apiBase || !runtime.wsBase || !runtime.sessionToken) return undefined;
  return runtime;
}

export function apiURL(path: string): string {
  const runtime = desktopRuntime();
  return runtime ? new URL(path, runtime.apiBase).toString() : path;
}

export function sessionHeaders(headers?: HeadersInit): Headers {
  const result = new Headers(headers);
  const runtime = desktopRuntime();
  if (runtime) result.set("X-Milevia-Session", runtime.sessionToken);
  return result;
}

export function createWebSocket(path: string): WebSocket {
  const runtime = desktopRuntime();
  if (!runtime) {
    const protocol = window.location.protocol === "https:" ? "wss" : "ws";
    return new WebSocket(`${protocol}://${window.location.host}${path}`);
  }
  const url = new URL(path, runtime.wsBase).toString();
  return new WebSocket(url, `milevia-session.${runtime.sessionToken}`);
}
