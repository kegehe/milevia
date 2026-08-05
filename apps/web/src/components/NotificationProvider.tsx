// 全局通知 Provider — 跨项目 WebSocket 通知 + Sonner Toast + BroadcastChannel + Browser Notification

import { createContext, useContext, useEffect, useRef, useCallback, useState } from "react";
import { useNavigate, useLocation } from "react-router-dom";
import { Toaster, toast } from "sonner";
import { api } from "../lib/api";
import { createWebSocket } from "../lib/runtime";
import {
  type NotificationEvent,
  notificationConversationURL,
  priorityForType,
  toastVariantForType,
} from "../lib/notifications";

const UnreadNotificationContext = createContext(0);

/** 未读通知数量，跨路由保持在常驻 Provider 中。 */
export function useUnreadCount() {
  return useContext(UnreadNotificationContext);
}

export function NotificationProvider({ children }: { children: React.ReactNode }) {
  const [unreadCount, setUnreadCount] = useState(0);
  const navigate = useNavigate();
  const location = useLocation();
  const wsRef = useRef<WebSocket | null>(null);
  const dismissedRef = useRef(new Set<string>());
  const dismissedAllAtRef = useRef(0); // dismiss-all 的时间戳，用于过滤旧通知
  const unreadNotificationsRef = useRef(new Map<string, number>());
  const shownRef = useRef(new Set<string>()); // 跟踪 WebSocket 已展示的通知 ID（防止重连重复）
  const broadcastShownRef = useRef(new Set<string>()); // 跟踪 BroadcastChannel 已处理的通知 ID（防止重复 inc）
  const initializedRef = useRef(false); // 初始加载完成标志，防止 WebSocket 历史通知与初始加载竞态
  const channelRef = useRef<BroadcastChannel | null>(null);
  const pathnameRef = useRef(location.pathname);
  pathnameRef.current = location.pathname;

  const syncUnreadCount = useCallback(() => {
    setUnreadCount(unreadNotificationsRef.current.size);
  }, []);

  const markUnread = useCallback((event: Pick<NotificationEvent, "id" | "createdAt">) => {
    if (dismissedRef.current.has(event.id)) return;
    const createdAt = new Date(event.createdAt).getTime();
    if (dismissedAllAtRef.current && Number.isFinite(createdAt) && createdAt <= dismissedAllAtRef.current) return;
    unreadNotificationsRef.current.set(event.id, Number.isFinite(createdAt) ? createdAt : Date.now());
    syncUnreadCount();
  }, [syncUnreadCount]);

  const markDismissed = useCallback((id: string, broadcast = false) => {
    dismissedRef.current.add(id);
    unreadNotificationsRef.current.delete(id);
    syncUnreadCount();
    if (broadcast) channelRef.current?.postMessage({ type: "dismissed", id });
  }, [syncUnreadCount]);

  const markAllDismissed = useCallback((dismissedAt: number, broadcast = false) => {
    if (dismissedAt < dismissedAllAtRef.current) return;
    dismissedRef.current.clear();
    dismissedAllAtRef.current = dismissedAt;
    for (const [id, createdAt] of unreadNotificationsRef.current) {
      if (createdAt <= dismissedAt) unreadNotificationsRef.current.delete(id);
    }
    syncUnreadCount();
    if (broadcast) channelRef.current?.postMessage({ type: "dismissed-all", dismissedAt });
  }, [syncUnreadCount]);

  // 初始加载未读通知：设置 count 并将已有通知 ID 标记到 shownRef，
  // 防止 WebSocket 连接后历史通知重复弹 toast 和重复 inc
  useEffect(() => {
    let stale = false;
    api<NotificationEvent[]>("/api/notifications")
      .then((list) => {
        if (stale || !Array.isArray(list)) return;
        for (const n of list) {
          shownRef.current.add(n.id);
          markUnread(n);
        }
        initializedRef.current = true;
      })
      .catch(() => {
        if (!stale) initializedRef.current = true; // 即使失败也放行，避免永久阻塞 toast
      });
    return () => {
      stale = true;
    };
  }, [markUnread]);

  // 判断用户是否正在查看产生通知的会话页面（审批去重）
  // 使用 ref 读取 pathname，避免路由变化导致回调重建
  const isViewingConversation = useCallback(
    (conversationId?: string) => {
      if (!conversationId) return false;
      return pathnameRef.current.includes(`/conversations/${conversationId}`);
    },
    [],
  );

  // 标记通知已读
  const dismissNotification = useCallback((id: string) => {
    if (dismissedRef.current.has(id)) return;
    void api(`/api/notifications/${id}/dismiss`, { method: "POST" })
      .then(() => markDismissed(id, true))
      .catch(() => {});
  }, [markDismissed]);

  // 处理收到的通知
  const handleNotification = useCallback(
    (event: NotificationEvent, fromBroadcast = false) => {
      if (dismissedRef.current.has(event.id)) return;

      // dismiss-all 后过滤：创建时间早于 dismiss-all 时间戳的通知视为已处理
      if (dismissedAllAtRef.current && new Date(event.createdAt).getTime() < dismissedAllAtRef.current) return;

      // 审批去重：如果用户正在查看该会话，不弹 toast
      if (event.type === "approval.pending" && isViewingConversation(event.conversationId)) {
        return;
      }

      // BroadcastChannel 收到的通知（来自其他 Tab 的转发）：
      // - 如果本 Tab 已通过 WebSocket 处理过（shownRef 有记录），跳过
      // - 否则只更新未读计数，不弹 toast（toast 由本 Tab 的 WebSocket 消息触发）
      if (fromBroadcast) {
        if (shownRef.current.has(event.id)) return;
        if (broadcastShownRef.current.has(event.id)) return;
        broadcastShownRef.current.add(event.id);
        // broadcastShownRef 容量限制
        if (broadcastShownRef.current.size > 1000) {
          broadcastShownRef.current.clear();
          broadcastShownRef.current.add(event.id);
        }
        markUnread(event);
        return;
      }

      // WebSocket 通知去重：防止重连后历史通知重复弹 toast
      if (shownRef.current.has(event.id)) return;
      shownRef.current.add(event.id);

      // shownRef 容量限制，防止长时间运行内存泄漏
      if (shownRef.current.size > 1000) {
        shownRef.current.clear();
        shownRef.current.add(event.id);
      }

      // 初始加载完成前的通知（页面刷新后 WebSocket 历史通知）：
      // 计入未读集合，但不弹 toast；初始 REST 结果会与该集合合并。
      if (!initializedRef.current) {
        markUnread(event);
        return;
      }

      // 通过 BroadcastChannel 通知其他 Tab
      channelRef.current?.postMessage({
        type: "notification",
        payload: event,
      });

      const isHighPriority = event.priority === "high" || priorityForType(event.type) === "high";
      const duration = isHighPriority ? Infinity : 8000;
      const variant = toastVariantForType(event.type);

      toast[variant](event.title, {
        id: `notif-${event.id}`, // 去重：同一通知只显示一个
        description: event.body,
        duration,
        action: {
          label: "查看详情",
          onClick: () => {
            navigate(notificationConversationURL(event));
            dismissNotification(event.id);
          },
        },
        onDismiss: () => dismissNotification(event.id),
      });

      markUnread(event);

      // 页面不可见时，显示浏览器桌面通知
      if (document.visibilityState === "hidden" && Notification.permission === "granted") {
        new Notification(event.title, {
          body: event.body,
          tag: event.id,
        });
      }
    },
    [navigate, isViewingConversation, dismissNotification, markUnread],
  );

  // WebSocket 连接 + 自动重连
  useEffect(() => {
    let reconnectAttempts = 0;
    let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
    let cancelled = false;

    const connect = () => {
      if (cancelled) return;
      const ws = createWebSocket("/ws/notifications");

      ws.onopen = () => {
        const isReconnect = reconnectAttempts > 0;
        reconnectAttempts = 0;
        if (!isReconnect) return; // 首次连接由初始加载 useEffect 处理
        // 重连后重新加载未读通知 ID，更新 shownRef 防止历史通知重复弹 toast
        // 重置 initializedRef，让重连后的历史通知被拦截（不弹 toast、不 inc）
        initializedRef.current = false;
        api<NotificationEvent[]>("/api/notifications")
          .then((list) => {
            if (!Array.isArray(list)) return;
            for (const n of list) {
              shownRef.current.add(n.id);
              markUnread(n);
            }
            initializedRef.current = true;
          })
          .catch(() => {
            initializedRef.current = true; // 失败也放行
          });
      };

      ws.onmessage = (raw) => {
        try {
          const event: NotificationEvent = JSON.parse(raw.data);
          handleNotification(event);
        } catch {
          /* ignore parse errors */
        }
      };

      ws.onclose = () => {
        if (cancelled) return;
        reconnectAttempts++;
        const delay = Math.min(500 * Math.pow(2, reconnectAttempts - 1), 15000);
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
  }, [handleNotification, markUnread]);

  // BroadcastChannel 跨 Tab 同步
  useEffect(() => {
    const channel = new BroadcastChannel("app-notifications");
    channelRef.current = channel;
    channel.onmessage = (e: MessageEvent) => {
      if (e.data?.type === "notification") {
        handleNotification(e.data.payload as NotificationEvent, true);
      } else if (e.data?.type === "dismissed" && typeof e.data.id === "string") {
        markDismissed(e.data.id);
      } else if (e.data?.type === "dismissed-all" && typeof e.data.dismissedAt === "number") {
        markAllDismissed(e.data.dismissedAt);
      }
    };
    return () => {
      channel.close();
      channelRef.current = null;
    };
  }, [handleNotification, markDismissed, markAllDismissed]);

  // 请求浏览器通知权限（延迟到用户交互后）
  useEffect(() => {
    const requestPermission = () => {
      if (Notification.permission === "default") {
        void Notification.requestPermission();
      }
      document.removeEventListener("click", requestPermission);
      document.removeEventListener("keydown", requestPermission);
    };
    document.addEventListener("click", requestPermission);
    document.addEventListener("keydown", requestPermission);
    return () => {
      document.removeEventListener("click", requestPermission);
      document.removeEventListener("keydown", requestPermission);
    };
  }, []);

  // 监听 NotificationCenter 的已读事件，并同步其他标签页。
  useEffect(() => {
    const onDismissed = (e: Event) => {
      const id = (e as CustomEvent<{ id?: string }>).detail?.id;
      if (id) markDismissed(id, true);
    };
    const onDismissedAll = (event: Event) => {
      const raw = (event as CustomEvent<{ dismissedAt?: string }>).detail?.dismissedAt;
      const dismissedAt = raw ? new Date(raw).getTime() : NaN;
      markAllDismissed(Number.isFinite(dismissedAt) ? dismissedAt : Date.now(), true);
    };
    window.addEventListener("notification:dismissed", onDismissed);
    window.addEventListener("notification:dismissed-all", onDismissedAll);
    return () => {
      window.removeEventListener("notification:dismissed", onDismissed);
      window.removeEventListener("notification:dismissed-all", onDismissedAll);
    };
  }, [markDismissed, markAllDismissed]);

  return (
    <UnreadNotificationContext.Provider value={unreadCount}>
      <Toaster
        position="bottom-right"
        closeButton
      />
      {children}
    </UnreadNotificationContext.Provider>
  );
}
