import { useEffect, useRef } from "react";

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
}) {
  const eventRef = useRef(options.onEvent);
  const refreshSnapshotRef = useRef(options.refreshSnapshot);
  const refreshGitRef = useRef(options.refreshGit);
  const isCurrentTokenRef = useRef(options.isCurrentToken);
  const visibleRef = useRef(options.visible);
  const wasVisibleRef = useRef(options.visible);
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
      const timer = window.setTimeout(resolve, delay);
      controller.signal.addEventListener("abort", () => { window.clearTimeout(timer); resolve(); }, { once: true });
    });
    const runStream = async () => {
      let retryDelay = 1000;
      while (!stopped && !controller.signal.aborted) {
        try {
          await consumeMobileEventStream(options.streamURL, options.activeToken, lastEventID, (event) => {
            if (stopped || !isCurrentTokenRef.current(options.activeToken)) return;
            if (event.lastEventId) lastEventID = event.lastEventId;
            if (!eventRef.current(event)) {
              scheduleSnapshotRefresh();
              scheduleGitRefresh();
            }
          }, controller.signal);
          if (stopped || controller.signal.aborted) break;
          await wait(retryDelay);
          retryDelay = 1000;
        } catch {
          if (stopped || controller.signal.aborted) break;
          startFallbackTimer();
          await wait(retryDelay);
          retryDelay = Math.min(retryDelay * 2, 30_000);
        }
      }
    };
    startFallbackTimer();
    void runStream();
    return () => {
      stopped = true;
      controller.abort();
      if (fallbackTimer !== null) window.clearInterval(fallbackTimer);
      if (refreshTimer !== null) window.clearTimeout(refreshTimer);
      if (gitRefreshTimer !== null) window.clearTimeout(gitRefreshTimer);
    };
  }, [options.instanceID, options.activeToken, options.deviceEpoch, options.streamURL]);
  useEffect(() => {
    const wasVisible = wasVisibleRef.current;
    wasVisibleRef.current = options.visible;
    if (!options.visible || wasVisible || !options.instanceID) return;
    void refreshSnapshotRef.current();
    refreshGitRef.current();
  }, [options.visible, options.instanceID]);
}
