import { createContext, useContext, useEffect, useRef } from "react";
import { advanceMobileStreamCursor } from "./mobile-stream-state";

export type SharedMobileEvent = { instanceID: string; activeToken: string; event: MessageEvent };
export const MobileRemoteEventStreamContext = createContext<{
  subscribe: (handler: (message: SharedMobileEvent) => void) => () => void;
} | null>(null);

export const streamIdleTimeoutMs = 45_000;

export async function consumeMobileEventStream(url: string, token: string, lastEventID: string, onMessage: (event: MessageEvent) => void, signal: AbortSignal) {
  const headers = new Headers({ Accept: "text/event-stream", Authorization: `Bearer ${token}` });
  if (lastEventID) headers.set("Last-Event-ID", lastEventID);
  const controller = new AbortController();
  const abortByCaller = () => controller.abort();
  if (signal.aborted) controller.abort();
  else signal.addEventListener("abort", abortByCaller, { once: true });
  let idleTimer: number | undefined;
  const armIdleWatchdog = () => {
    if (idleTimer !== undefined) window.clearTimeout(idleTimer);
    idleTimer = window.setTimeout(() => controller.abort(), streamIdleTimeoutMs);
  };
  try {
    armIdleWatchdog();
    const response = await fetch(url, { headers, signal: controller.signal });
    if (!response.ok) throw new Error(`stream request failed (${response.status})`);
    if (!response.body) throw new Error("stream response has no body");
    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    let buffer = "";
    let eventID = "";
    let data: string[] = [];
    const dispatch = () => {
      if (data.length > 0) onMessage(new MessageEvent("message", { data: data.join("\n"), lastEventId: eventID }));
      data = [];
      eventID = "";
    };
    const processLine = (line: string) => {
      if (line === "") { dispatch(); return; }
      if (line.startsWith(":")) return;
      const separator = line.indexOf(":");
      const field = separator < 0 ? line : line.slice(0, separator);
      const value = separator < 0 ? "" : line.slice(separator + 1).replace(/^ /, "");
      if (field === "id") eventID = value;
      if (field === "data") data.push(value);
    };
    armIdleWatchdog();
    while (!controller.signal.aborted) {
      const next = await reader.read();
      armIdleWatchdog();
      if (next.done) {
        buffer += decoder.decode();
        if (buffer) processLine(buffer);
        break;
      }
      buffer += decoder.decode(next.value, { stream: true });
      const lines = buffer.split(/\r?\n/);
      buffer = lines.pop() || "";
      for (const line of lines) processLine(line);
    }
    dispatch();
  } finally {
    controller.abort();
    if (idleTimer !== undefined) window.clearTimeout(idleTimer);
    signal.removeEventListener("abort", abortByCaller);
  }
}

type RefreshSnapshot = () => Promise<unknown> | unknown;

export function useMobileRemoteEventStream(options: {
  instanceID: string;
  activeToken: string;
  deviceEpoch: number;
  visible: boolean;
  streamURL: string;
  isCurrentToken: (token: string) => boolean;
  onEvent: (event: MessageEvent) => boolean;
  refreshSnapshot: RefreshSnapshot;
  refreshGit: () => void;
  initialCursor?: (signal: AbortSignal) => Promise<string>;
  onCursor?: (cursor: string) => void;
  onError?: (error: unknown) => void;
}) {
  const shared = useContext(MobileRemoteEventStreamContext);
  const eventRef = useRef(options.onEvent);
  const refreshSnapshotRef = useRef(options.refreshSnapshot);
  const refreshGitRef = useRef(options.refreshGit);
  const isCurrentTokenRef = useRef(options.isCurrentToken);
  const visibleRef = useRef(options.visible);
  const wasVisibleRef = useRef(options.visible);
  const cursorRef = useRef(options.onCursor);
  const errorRef = useRef(options.onError);
  const initialCursorRef = useRef(options.initialCursor);
  cursorRef.current = options.onCursor;
  errorRef.current = options.onError;
  initialCursorRef.current = options.initialCursor;
  eventRef.current = options.onEvent;
  refreshSnapshotRef.current = options.refreshSnapshot;
  refreshGitRef.current = options.refreshGit;
  isCurrentTokenRef.current = options.isCurrentToken;
  visibleRef.current = options.visible;
  useEffect(() => {
    if (!options.instanceID || !options.activeToken) return;
    const controller = new AbortController();
    let stopped = false;
    let lastEventID = "";
    let fallbackTimer: number | null = null;
    let refreshTimer: number | null = null;
    let gitRefreshTimer: number | null = null;
    const startFallbackTimer = () => {
      if (fallbackTimer !== null) return;
      fallbackTimer = window.setInterval(() => {
        if (visibleRef.current) void refreshSnapshotRef.current();
      }, 15_000);
    };
    const scheduleSnapshotRefresh = () => {
      if (refreshTimer !== null) return;
      refreshTimer = window.setTimeout(() => {
        refreshTimer = null;
        if (visibleRef.current) void refreshSnapshotRef.current();
      }, 500);
    };
    const scheduleGitRefresh = () => {
      if (gitRefreshTimer !== null) return;
      gitRefreshTimer = window.setTimeout(() => {
        gitRefreshTimer = null;
        if (visibleRef.current) refreshGitRef.current();
      }, 3000);
    };
    const wait = (delay: number) => new Promise<void>((resolve) => {
      const done = () => { window.clearTimeout(timer); controller.signal.removeEventListener("abort", done); resolve(); };
      const timer = window.setTimeout(done, delay);
      controller.signal.addEventListener("abort", done, { once: true });
    });
    const runStream = async () => {
      let retryDelay = 1000;
      let initialized = false;
      while (!stopped && !controller.signal.aborted) {
        try {
          if (!initialized) {
            lastEventID = await initialCursorRef.current?.(controller.signal) ?? "";
            if (stopped || controller.signal.aborted) break;
            initialized = true;
          }
          await consumeMobileEventStream(options.streamURL, options.activeToken, lastEventID, (event) => {
            if (stopped || !isCurrentTokenRef.current(options.activeToken)) return;
            if (!eventRef.current(event)) {
              scheduleSnapshotRefresh();
              scheduleGitRefresh();
            }
            // 处理与持久入队失败时不推进断点，重连后才能重新处理这条事件。
            if (event.lastEventId) {
              const nextCursor = advanceMobileStreamCursor(lastEventID, event.lastEventId);
              if (nextCursor !== lastEventID) {
                cursorRef.current?.(nextCursor);
                lastEventID = nextCursor;
              }
            }
          }, controller.signal);
          if (stopped || controller.signal.aborted) break;
          await wait(retryDelay);
          retryDelay = 1000;
        } catch (error) {
          if (stopped || controller.signal.aborted) break;
          errorRef.current?.(error);
          startFallbackTimer();
          await wait(retryDelay);
          retryDelay = Math.min(retryDelay * 2, 30_000);
        }
      }
    };
    startFallbackTimer();
    // 页面只订阅常驻连接，进入设置页时仅移除页面监听，不关闭连接。
    const unsubscribe = shared?.subscribe((message) => {
      if (stopped || message.instanceID !== options.instanceID || message.activeToken !== options.activeToken ||
        !isCurrentTokenRef.current(options.activeToken)) return;
      if (!eventRef.current(message.event)) {
        scheduleSnapshotRefresh();
        scheduleGitRefresh();
      }
    });
    if (!shared) void runStream();
    return () => {
      stopped = true;
      unsubscribe?.();
      controller.abort();
      if (fallbackTimer !== null) window.clearInterval(fallbackTimer);
      if (refreshTimer !== null) window.clearTimeout(refreshTimer);
      if (gitRefreshTimer !== null) window.clearTimeout(gitRefreshTimer);
    };
  }, [options.instanceID, options.activeToken, options.deviceEpoch, options.streamURL, shared]);
  useEffect(() => {
    const wasVisible = wasVisibleRef.current;
    wasVisibleRef.current = options.visible;
    if (!options.visible || wasVisible || !options.instanceID) return;
    void refreshSnapshotRef.current();
    refreshGitRef.current();
  }, [options.visible, options.instanceID]);
}
