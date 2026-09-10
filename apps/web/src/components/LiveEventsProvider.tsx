// 全局「状态已变化」事件监听 Provider — 订阅控制服务 /ws/events 失效信号。
// 服务端仅在数据真正变化时才广播（新建会话、定时任务增删改/运行流转、运行起停等），
// 页面据此做按需刷新，把固定的 10s 轮询降为「事件驱动 + 兜底慢轮询」：
// 状态未变化时不再持续发请求，变化时也不再等到下一个轮询周期。
// 仿 ProcessStatusProvider / NotificationProvider 的独立单例形态（重连 + BroadcastChannel 跨 Tab）。

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  type ReactNode,
} from "react";
import { createWebSocket } from "../lib/runtime";

/** 服务端 /ws/events 推送的失效信号。 */
export type StateEvent = {
  type: "projects" | "scheduled-tasks" | "all" | string;
  projectId?: string;
};

type LiveEventsHandler = (event: StateEvent) => void;

const LiveEventsContext = createContext<(handler: LiveEventsHandler) => () => void>(() => () => {});

const STATE_EVENT_CHANNEL = "app-state-events";
const RECONNECT_MAX_DELAY = 15_000;

/**
 * 订阅失效信号。handler 变化时原地替换（不重连 WS），组件卸载时自动取消。
 */
export function useLiveStateEvents(handler: LiveEventsHandler): void {
  const subscribe = useContext(LiveEventsContext);
  const ref = useRef(handler);
  ref.current = handler;
  useEffect(() => subscribe((event) => ref.current(event)), [subscribe]);
}

/**
 * 便捷订阅：仅当事件 type 匹配且 result 命中 projectId 时才回调。
 * type 为 "all" 时不过滤类型；projectId 省略时不过滤项目。
 */
export function useLiveStateEventsFor(
  type: "all" | "projects" | "scheduled-tasks",
  projectId: string | undefined,
  onEvent: LiveEventsHandler,
): void {
  useLiveStateEvents(
    useMemo(() => {
      if (!onEvent) return () => {};
      return (event: StateEvent) => {
        if (type !== "all" && event.type !== type) return;
        if (projectId !== undefined && event.projectId !== undefined && event.projectId !== projectId) return;
        onEvent(event);
      };
    }, [type, projectId, onEvent]),
  );
}

export function LiveEventsProvider({ children }: { children: ReactNode }) {
  const handlersRef = useRef(new Set<LiveEventsHandler>());
  const wsRef = useRef<WebSocket | null>(null);
  const channelRef = useRef<BroadcastChannel | null>(null);

  const subscribe = useCallback((handler: LiveEventsHandler) => {
    handlersRef.current.add(handler);
    return () => {
      handlersRef.current.delete(handler);
    };
  }, []);

  const dispatch = useCallback((event: StateEvent) => {
    if (!event || typeof event.type !== "string") return;
    for (const handler of handlersRef.current) {
      try {
        handler(event);
      } catch {
        /* 单个监听器异常不得阻断其余监听器 */
      }
    }
  }, []);

  // WS 实时 + 自动重连 + 转发到其他 Tab
  useEffect(() => {
    let reconnectAttempts = 0;
    let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
    let cancelled = false;

    const connect = () => {
      if (cancelled) return;
      const ws = createWebSocket("/ws/events");
      ws.onopen = () => {
        reconnectAttempts = 0;
      };
      ws.onmessage = (raw: MessageEvent) => {
        try {
          const event: StateEvent = JSON.parse(raw.data);
          dispatch(event);
          channelRef.current?.postMessage({ type: "state", payload: event });
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
      ws.onerror = () => {
        ws.close();
      };
      wsRef.current = ws;
    };
    connect();

    return () => {
      cancelled = true;
      if (reconnectTimer) clearTimeout(reconnectTimer);
      wsRef.current?.close();
      wsRef.current = null;
    };
  }, [dispatch]);

  // BroadcastChannel 跨 Tab 实时一致
  useEffect(() => {
    const channel = new BroadcastChannel(STATE_EVENT_CHANNEL);
    channelRef.current = channel;
    channel.onmessage = (e: MessageEvent) => {
      if (e.data?.type === "state") {
        const event = e.data.payload as StateEvent;
        if (event && typeof event.type === "string") dispatch(event);
      }
    };
    return () => {
      channel.close();
      channelRef.current = null;
    };
  }, [dispatch]);

  const value = useMemo(() => subscribe, [subscribe]);
  return <LiveEventsContext.Provider value={value}>{children}</LiveEventsContext.Provider>;
}