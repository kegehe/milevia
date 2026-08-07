// 全局进程运行状态 Provider — REST 兜底 + WebSocket 实时 + 自动重连 + BroadcastChannel 跨 Tab。
// 仿 NotificationProvider 的独立单例形态：单一 owner 持有进程状态，避免与会话状态轮询写同一对象。

import { createContext, useContext, useEffect, useRef, useCallback, useState } from "react";
import { api } from "../lib/api";
import { createWebSocket } from "../lib/runtime";
import type { ProjectProcessStatus, ProjectProcessStatusMap, RunStatus, RunStatusEvent } from "../lib/types";

const ProcessStatusContext = createContext<ProjectProcessStatusMap>({});

/** 项目开发进程运行状态映射（key: 项目 id）。 */
export function useProcessStatusMap() {
  return useContext(ProcessStatusContext);
}

const PROCESS_POLL_INTERVAL = 10_000;
const RECONNECT_MAX_DELAY = 15_000;

function mergeEvent(
  prev: ProjectProcessStatusMap,
  event: RunStatusEvent,
): ProjectProcessStatusMap {
  const id = event.projectId;
  const existing = prev[id] ?? { runStatus: "stopped" as RunStatus };
  const next: ProjectProcessStatus = {
    runStatus: event.status,
    runUpdatedAt: Date.now(),
  };
  // pid/startedAt 仅在事件携带时更新，否则保留；stopped 表示进程已无意义，清空细节。
  if (event.status === "stopped") {
    delete next.runPid;
    delete next.runStartedAt;
  } else {
    if (event.pid != null) next.runPid = event.pid;
    else if (existing.runPid !== undefined) next.runPid = existing.runPid;
    if (event.startedAt != null) next.runStartedAt = event.startedAt;
    else if (existing.runStartedAt !== undefined) next.runStartedAt = existing.runStartedAt;
  }
  if (existing.runUpdatedAt !== undefined && sameStatus(next, existing)) return prev;
  return { ...prev, [id]: next };
}

function sameStatus(a: ProjectProcessStatus, b: ProjectProcessStatus): boolean {
  return a.runStatus === b.runStatus && a.runPid === b.runPid && a.runStartedAt === b.runStartedAt;
}

// REST 快照过期阈值：若某个项目的实时状态超过该时长未经 WS 更新，则视为
// WS 已断/错过事件，回退采纳 REST 兜底值，避免"运行中"卡死在进程已停止的残余态。
const REST_STALE_AFTER_MS = 25_000;

export function ProcessStatusProvider({ children }: { children: React.ReactNode }) {
  const [processStatuses, setProcessStatuses] = useState<ProjectProcessStatusMap>({});
  const wsRef = useRef<WebSocket | null>(null);
  const channelRef = useRef<BroadcastChannel | null>(null);

  const applyEvent = useCallback((event: RunStatusEvent) => {
    setProcessStatuses((prev) => mergeEvent(prev, event));
  }, []);

  const applyRest = useCallback((items: unknown) => {
    if (!Array.isArray(items)) return;
    const next: ProjectProcessStatusMap = {};
    for (const item of items) {
      if (typeof item === "object" && item !== null && typeof (item as { id?: unknown }).id === "string") {
        const it = item as { id: string; runStatus?: RunStatus; runPid?: number | null; runStartedAt?: string | null };
        const status: ProjectProcessStatus = {
          runStatus: it.runStatus ?? "stopped",
        };
        if (typeof it.runPid === "number") status.runPid = it.runPid;
        if (typeof it.runStartedAt === "string") status.runStartedAt = it.runStartedAt;
        next[it.id] = status;
      }
    }
    setProcessStatuses((prev) => {
      // REST 仅作为兜底：对每个项目，若实时状态依然新鲜（WS 近期推送过）则保持实时值，
      // 否则（WS 未连/错过事件/重启后进程已停）采用 REST 值收敛。无变化时不触发重渲染。
      const merged: ProjectProcessStatusMap = { ...next };
      const now = Date.now();
      for (const id of Object.keys(next)) {
        const live = prev[id];
        if (!live) continue;
        // 缺 runUpdatedAt（从未被 WS 刷新，如重启后 SOLELY 由 REST 填充/WS 未连）视为即时过期，
        // 允许 REST 兜底纠偏；否则按 WS 最近更新时间判断 25s 是否过期。
        const stale = live.runUpdatedAt === undefined || now - live.runUpdatedAt > REST_STALE_AFTER_MS;
        if (live.runStatus !== next[id].runStatus && !stale) {
          merged[id] = live;
          continue;
        }
        if (sameStatus(live, next[id])) {
          // 无实质变化：保留 live 的时间戳，避免 REST 把实时时间戳冲掉。
          merged[id] = live;
          continue;
        }
        merged[id] = next[id];
      }
      return merged;
    });
  }, []);

  const poll = useCallback(() => {
    void api<unknown>("/api/projects/processes/statuses").then(applyRest).catch(() => {});
  }, [applyRest]);

  // 初始 + 10s 兜底轮询
  useEffect(() => {
    poll();
    const interval = window.setInterval(poll, PROCESS_POLL_INTERVAL);
    return () => window.clearInterval(interval);
  }, [poll]);

  // WS 实时 + 自动重连 + 转发到其他 Tab
  useEffect(() => {
    let reconnectAttempts = 0;
    let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
    let cancelled = false;

    const connect = () => {
      if (cancelled) return;
      const ws = createWebSocket("/ws/processes");
      ws.onopen = () => { reconnectAttempts = 0; };
      ws.onmessage = (raw: MessageEvent) => {
        try {
          const event: RunStatusEvent = JSON.parse(raw.data);
          if (!event || typeof event.projectId !== "string" || !event.status) return;
          applyEvent(event);
          channelRef.current?.postMessage({ type: "status", payload: event });
        } catch {
          /* 忽略无法解析的帧 */
        }
      };
      ws.onclose = () => {
        if (cancelled) return;
        reconnectAttempts++;
        const delay = Math.min(500 * Math.pow(2, reconnectAttempts - 1), RECONNECT_MAX_DELAY);
        reconnectTimer = setTimeout(connect, delay);
      };
      ws.onerror = () => { ws.close(); };
      wsRef.current = ws;
    };
    connect();

    return () => {
      cancelled = true;
      if (reconnectTimer) clearTimeout(reconnectTimer);
      wsRef.current?.close();
      wsRef.current = null;
    };
  }, [applyEvent]);

  // BroadcastChannel 跨 Tab 实时一致
  useEffect(() => {
    const channel = new BroadcastChannel("app-process-status");
    channelRef.current = channel;
    channel.onmessage = (e: MessageEvent) => {
      if (e.data?.type === "status") {
        const event = e.data.payload as RunStatusEvent;
        if (event && typeof event.projectId === "string" && event.status) applyEvent(event);
      }
    };
    return () => {
      channel.close();
      channelRef.current = null;
    };
  }, [applyEvent]);

  return (
    <ProcessStatusContext.Provider value={processStatuses}>
      {children}
    </ProcessStatusContext.Provider>
  );
}
