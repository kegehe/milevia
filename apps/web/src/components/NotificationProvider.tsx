// 全局通知 Provider — 跨项目 WebSocket 通知 + Sonner Toast + BroadcastChannel + Browser Notification

import { createContext, useContext, useEffect, useRef, useCallback, useState } from "react";
import { useNavigate, useLocation } from "react-router-dom";
import { Toaster, toast } from "sonner";
import { invoke } from "@tauri-apps/api/core";
import { api } from "../lib/api";
import { createWebSocket, isDesktop } from "../lib/runtime";
import {
  type NotificationEvent,
  isWindowsNotifyType,
  isWithinQuietHours,
  notificationTargetURL,
  notificationTimeNs,
  priorityForType,
  reconcileUnreadNotifications,
  rememberNotificationID,
  toastVariantForType,
  trimUnreadNotifications,
  windowsToastLines,
} from "../lib/notifications";
import { useUIPreferences, type LocalPreferences } from "../stores/useUIPreferences";
import { NotificationToastBody } from "./NotificationToastBody";

/** 通知 toast 的自动收起时长。唯一的时长来源：
 * sonner 的 duration、倒计时文字与进度条都从它推导（见 NotificationToastBody）。 */
const NOTIFICATION_TOAST_DURATION_MS = 8000;

const UnreadNotificationContext = createContext(0);
const NotificationRefreshContext = createContext<() => void>(() => {});

/** 未读通知数量，跨路由保持在常驻 Provider 中。 */
export function useUnreadCount() {
  return useContext(UnreadNotificationContext);
}

/** 重新拉取未读通知：服务端删掉了通知目标（项目/会话/定时任务）后由 /ws/events 触发。 */
export function useNotificationRefresh() {
  return useContext(NotificationRefreshContext);
}

export function NotificationProvider({ children }: { children: React.ReactNode }) {
  const { localPreferences } = useUIPreferences();
  const [unreadCount, setUnreadCount] = useState(0);
  const navigate = useNavigate();
  const location = useLocation();
  const wsRef = useRef<WebSocket | null>(null);
  const dismissedRef = useRef(new Set<string>());
  const dismissedAllAtRef = useRef(0n); // 纳秒精度，与服务端全部已读边界一致。
  const unreadNotificationsRef = useRef(new Map<string, bigint>());
  const shownRef = useRef(new Set<string>()); // 跟踪 WebSocket 已展示的通知 ID（防止重连重复）
  const broadcastShownRef = useRef(new Set<string>()); // 跟踪 BroadcastChannel 已处理的通知 ID（防止重复 inc）
  const activeToastIdsRef = useRef(new Set<string>());
  const initializedRef = useRef(false); // 初始加载完成标志，防止 WebSocket 历史通知与初始加载竞态
  const unreadRequestSeq = useRef(0); // 首拉、重连与后台失效刷新共用序号，拒绝旧快照。
  const channelRef = useRef<BroadcastChannel | null>(null);
  const pathnameRef = useRef(location.pathname);
  const localPreferencesRef = useRef<LocalPreferences>(localPreferences);
  pathnameRef.current = location.pathname;
  localPreferencesRef.current = localPreferences;

  const syncUnreadCount = useCallback(() => {
    trimUnreadNotifications(unreadNotificationsRef.current);
    // 已读、过期或超量移除的通知，其持久弹窗也必须一起收起。
    for (const id of activeToastIdsRef.current) {
      if (!unreadNotificationsRef.current.has(id)) {
        toast.dismiss(`notif-${id}`);
        activeToastIdsRef.current.delete(id);
      }
    }
    setUnreadCount(unreadNotificationsRef.current.size);
  }, []);

  const unreadCreatedAt = useCallback((event: Pick<NotificationEvent, "id" | "createdAt">): bigint | null => {
    if (dismissedRef.current.has(event.id)) return null;
    const createdAt = notificationTimeNs(event.createdAt);
    if (createdAt !== null && createdAt <= dismissedAllAtRef.current) return null;
    return createdAt ?? BigInt(Date.now()) * 1_000_000n;
  }, []);

  const markUnread = useCallback((event: Pick<NotificationEvent, "id" | "createdAt">) => {
    const createdAt = unreadCreatedAt(event);
    if (createdAt === null) return;
    unreadNotificationsRef.current.set(event.id, createdAt);
    syncUnreadCount();
  }, [syncUnreadCount, unreadCreatedAt]);

  const markAllDismissed = useCallback((dismissedAt: bigint, broadcast = false) => {
    if (dismissedAt < dismissedAllAtRef.current) return;
    dismissedRef.current.clear();
    dismissedAllAtRef.current = dismissedAt;
    for (const [id, createdAt] of unreadNotificationsRef.current) {
      if (createdAt <= dismissedAt) unreadNotificationsRef.current.delete(id);
    }
    syncUnreadCount();
    if (broadcast) channelRef.current?.postMessage({
      type: "dismissed-all",
      dismissedAt: Number(dismissedAt / 1_000_000n), // 保留旧标签页可识别的毫秒字段。
      dismissedAtNs: dismissedAt,
    });
  }, [syncUnreadCount]);

  // 重新拉取未读列表并对账：服务端已删掉的通知（项目/会话/定时任务被删）要从角标里消失。
  // recordShown 为真时把结果记入 shownRef（WebSocket 重连路径沿用旧行为，避免历史通知重弹）。
  const refreshUnread = useCallback(async (recordShown: boolean) => {
    const seq = ++unreadRequestSeq.current;
    const shouldRecordShown = recordShown || !initializedRef.current;
    // 先拍快照：拉取期间新到的通知不在里面，不能因这次响应里还没有它就被抹掉角标。
    const beforeFetch = new Set(unreadNotificationsRef.current.keys());
    try {
      const list = await api<NotificationEvent[]>("/api/notifications");
      if (seq !== unreadRequestSeq.current || !Array.isArray(list)) return;
      // REST 按新到旧返回；按旧到新记入，滚动淘汰时优先保留最新历史。
      if (shouldRecordShown) {
        for (const notification of [...list].reverse()) rememberNotificationID(shownRef.current, notification.id);
      }
      const serverUnread = new Map<string, bigint>();
      for (const notification of list) {
        const createdAt = unreadCreatedAt(notification);
        if (createdAt !== null) serverUnread.set(notification.id, createdAt);
      }
      reconcileUnreadNotifications(unreadNotificationsRef.current, beforeFetch, serverUnread);
      syncUnreadCount();
    } catch {
      /* 拉取失败就保留现有角标，下次事件或重连再校正 */
    } finally {
      // 只有最新请求能够结束初始化/重连补发保护，旧请求完成不能提前放行弹窗。
      if (seq === unreadRequestSeq.current) initializedRef.current = true;
    }
  }, [syncUnreadCount, unreadCreatedAt]);

  const refreshUnreadNow = useCallback(() => {
    void refreshUnread(false).finally(() => {
      // 通知中心若正开着，也需要重拉列表（角标由上面的未读集合直接更新）。
      window.dispatchEvent(new Event("notification:invalidated"));
    });
  }, [refreshUnread]);

  const markDismissed = useCallback((id: string, broadcast = false) => {
    rememberNotificationID(dismissedRef.current, id);
    unreadNotificationsRef.current.delete(id);
    syncUnreadCount();
    if (broadcast) channelRef.current?.postMessage({ type: "dismissed", id });
    // 清理前库中可能暂时超限：忽略后 REST 会补回下一条，角标也必须重新对账。
    refreshUnreadNow();
  }, [syncUnreadCount, refreshUnreadNow]);

  // 初始加载未读通知：设置 count 并将已有通知 ID 标记到 shownRef，
  // 防止 WebSocket 连接后历史通知重复弹 toast 和重复 inc
  useEffect(() => {
    void refreshUnread(true);
    return () => {
      unreadRequestSeq.current++;
    };
  }, [refreshUnread]);

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
      if (unreadCreatedAt(event) === null) return;

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
        rememberNotificationID(broadcastShownRef.current, event.id);
        markUnread(event);
        return;
      }

      // WebSocket 通知去重：防止重连后历史通知重复弹 toast
      if (shownRef.current.has(event.id)) return;
      rememberNotificationID(shownRef.current, event.id);

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

      const preferences = localPreferencesRef.current;
      const mappedPriority = priorityForType(event.type);
      const isHighPriority = event.priority === "high" || mappedPriority === "high";
      const isNormalPriority = !isHighPriority && (event.priority === "normal" || mappedPriority === "normal");
      const inQuietHours = preferences.quietHoursEnabled && isWithinQuietHours(new Date(), preferences.quietHoursStart, preferences.quietHoursEnd);
      const shouldPresent = isHighPriority || (!inQuietHours && (isNormalPriority ? preferences.taskNotificationsEnabled : preferences.lowPriorityNotificationsEnabled));

      // 显示偏好只控制临时提醒；通知中心仍保留服务端保留期内的通知。
      if (!shouldPresent) {
        markUnread(event);
        return;
      }
      const duration = isHighPriority ? Infinity : NOTIFICATION_TOAST_DURATION_MS;
      const variant = toastVariantForType(event.type);

      // 标题行内附项目名小标（方案E轨迹卡）；后端字段按可能为空处理
      const titleNode = event.projectName ? (
        <>
          {event.title}
          <span className="notif-project">{event.projectName}</span>
        </>
      ) : (
        event.title
      );

      activeToastIdsRef.current.add(event.id);
      toast[variant](titleNode, {
        id: `notif-${event.id}`, // 去重：同一通知只显示一个
        className: "notif-toast", // 方案E轨迹卡样式钩子（notification.css 末段）
        description: <NotificationToastBody body={event.body} durationMs={duration} />,
        duration,
        action: {
          label: "查看详情",
          onClick: () => {
            activeToastIdsRef.current.delete(event.id);
            navigate(notificationTargetURL(event));
            dismissNotification(event.id);
          },
        },
        onDismiss: () => {
          // Sonner 的程序化 dismiss 也会触发回调；只有用户关闭才写入已读状态。
          if (!activeToastIdsRef.current.delete(event.id)) return;
          dismissNotification(event.id);
        },
        onAutoClose: () => activeToastIdsRef.current.delete(event.id),
      });

      markUnread(event);

      // Windows 系统右下角弹窗通知：用户开启 + 任务完成类 + 主窗口不在前台时发起。
      // 内容为方案A分层完整版：标题=事件 title，第二行「项目「X」· 任务Y」，第三行耗时
      // （均为服务端算好的字段，经 windowsToastLines 组装，缺失时省略对应行）；
      // 点击后跳到对应项目。用 document.hasFocus() 判断主窗口是否在前台：聚焦时已有
      // 应用内 toast，不叠加系统弹窗造成双重提醒；切到别处（窗口失焦/最小化）才弹。
      if (isDesktop() && preferences.windowsToastsEnabled && isWindowsNotifyType(event.type) && !document.hasFocus()) {
        const toastLines = windowsToastLines(event);
        invoke("show_system_notification", {
          path: notificationTargetURL(event),
          title: toastLines.title,
          line1: toastLines.line1,
          line2: toastLines.line2,
        }).catch(() => {
          /* 系统通知失败不影响应用内提醒 */
        });
      }

      // 页面不可见时，显示浏览器桌面通知
      if (preferences.systemNotificationsEnabled && preferences.notifyWhenHidden && document.visibilityState === "hidden" && typeof Notification !== "undefined" && Notification.permission === "granted") {
        new Notification(event.title, {
          body: event.body,
          tag: event.id,
        });
      }
    },
    [navigate, isViewingConversation, dismissNotification, markUnread, unreadCreatedAt],
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
        if (cancelled || wsRef.current !== ws) return;
        const isReconnect = reconnectAttempts > 0;
        reconnectAttempts = 0;
        if (!isReconnect) return; // 首次连接由初始加载 useEffect 处理
        // 重连后重新加载未读通知 ID，更新 shownRef 防止历史通知重复弹 toast
        // 重置 initializedRef，让重连后的历史通知被拦截（不弹 toast、不 inc）
        initializedRef.current = false;
        void refreshUnread(true);
      };

      ws.onmessage = (raw: MessageEvent) => {
        if (cancelled || wsRef.current !== ws) return;
        try {
          const event: NotificationEvent = JSON.parse(raw.data);
          handleNotification(event);
        } catch {
          /* ignore parse errors */
        }
      };

      ws.onclose = () => {
        if (cancelled || wsRef.current !== ws) return;
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
  }, [handleNotification, refreshUnread]);

  // BroadcastChannel 跨 Tab 同步
  useEffect(() => {
    const channel = new BroadcastChannel("app-notifications");
    channelRef.current = channel;
    channel.onmessage = (e: MessageEvent) => {
      if (e.data?.type === "notification") {
        handleNotification(e.data.payload as NotificationEvent, true);
      } else if (e.data?.type === "dismissed" && typeof e.data.id === "string") {
        markDismissed(e.data.id);
      } else if (e.data?.type === "dismissed-all" && typeof e.data.dismissedAtNs === "bigint") {
        markAllDismissed(e.data.dismissedAtNs);
      } else if (e.data?.type === "dismissed-all" && typeof e.data.dismissedAt === "number" && Number.isFinite(e.data.dismissedAt)) {
        // 兼容升级前标签页发送的毫秒时间戳。
        markAllDismissed(BigInt(Math.trunc(e.data.dismissedAt)) * 1_000_000n);
      }
    };
    return () => {
      channel.close();
      channelRef.current = null;
    };
  }, [handleNotification, markDismissed, markAllDismissed]);

  // 监听 NotificationCenter 的已读事件，并同步其他标签页。
  useEffect(() => {
    const onDismissed = (e: Event) => {
      const id = (e as CustomEvent<{ id?: string }>).detail?.id;
      if (id) markDismissed(id, true);
    };
    const onDismissedAll = (event: Event) => {
      const raw = (event as CustomEvent<{ dismissedAt?: string }>).detail?.dismissedAt;
      const dismissedAt = raw ? notificationTimeNs(raw) : null;
      markAllDismissed(dismissedAt ?? BigInt(Date.now()) * 1_000_000n, true);
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
      <NotificationRefreshContext.Provider value={refreshUnreadNow}>
        <Toaster
          position="bottom-right"
          closeButton
        />
        {children}
      </NotificationRefreshContext.Provider>
    </UnreadNotificationContext.Provider>
  );
}
