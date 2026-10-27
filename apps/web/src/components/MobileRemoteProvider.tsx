import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { useNavigate } from "react-router-dom";
import { Capacitor, type PluginListenerHandle } from "@capacitor/core";
import { App as CapacitorApp } from "@capacitor/app";
import { LocalNotifications } from "@capacitor/local-notifications";
import { Toaster } from "sonner";
import { cloudURL } from "../features/remote/cloud-url";
import { MobileRemoteEventStreamContext, useMobileRemoteEventStream, type SharedMobileEvent } from "../features/remote/use-mobile-remote-event-stream";
import { mobileStreamScope, initializeMobileStreamCursor, readMobileNotificationBaseline, saveMobileStreamCursor } from "../features/remote/mobile-stream-state";
import { MobileNotificationDelivery, MOBILE_NOTIFICATION_MAX_AGE_MS, isRecoverableMobileNotification, sendNativeMobileNotification, type NotificationDiagnostic } from "../features/remote/mobile-notification-delivery";
import { MOBILE_DEVICES_CHANGED_EVENT, markDeviceRevoked, readActiveToken, readDevices, reconcileDevices, setActiveToken, updateDevice } from "../lib/mobile-devices";
import { mobileNotificationContent, notificationIDFromEventID, shouldNotifyWhileAway, shouldPresentMobileNotification, toNotificationPermission } from "../lib/mobile-notify";
import { useDocumentVisible } from "../lib/useDocumentVisible";
import { useUIPreferences } from "../stores/useUIPreferences";

type NotificationTarget = { instanceId: string; projectId: string; conversationId: string; taskId: string };
type MobileNotifications = {
  permission: NotificationPermission | "unsupported";
  requestPermission: () => Promise<NotificationPermission | "unsupported">;
  testNotification: () => Promise<void>;
  diagnostic: NotificationDiagnostic | null;
  pendingTarget: NotificationTarget | null;
  clearTarget: () => void;
};

const MobileNotificationsContext = createContext<MobileNotifications | null>(null);
export function useMobileNotifications() { return useContext(MobileNotificationsContext); }

function initialPermission(): NotificationPermission | "unsupported" {
  if (Capacitor.isNativePlatform()) return "default";
  return typeof Notification === "undefined" ? "unsupported" : Notification.permission;
}

function currentConnection() {
  const token = readActiveToken();
  const device = readDevices().find((item) => item.token === token && !item.revoked);
  return { activeToken: device ? token : "", instanceID: device?.instanceId || "" };
}

function isCurrentMobileToken(token: string): boolean {
  return !!token && token === readActiveToken() && readDevices().some((item) => item.token === token && !item.revoked);
}

/** 常驻于移动端路由之上：一条事件连接，同时供页面刷新和系统通知使用。 */
export function MobileRemoteProvider({ children }: { children: ReactNode }) {
  const navigate = useNavigate();
  const navigateRef = useRef(navigate);
  navigateRef.current = navigate;
  const { localPreferences, updateLocalPreferences } = useUIPreferences();
  const preferencesRef = useRef(localPreferences);
  preferencesRef.current = localPreferences;
  const [connection, setConnection] = useState(() => { reconcileDevices(); return currentConnection(); });
  const connectionRef = useRef(connection);
  connectionRef.current = connection;
  const [permission, setPermission] = useState(initialPermission);
  const [diagnostic, setDiagnostic] = useState<NotificationDiagnostic | null>(null);
  const [pendingTarget, setPendingTarget] = useState<NotificationTarget | null>(null);
  const backgroundRef = useRef(false);
  const awaySinceRef = useRef<number | null>(document.hidden ? Date.now() : null);
  const awayIntervalsRef = useRef<Array<{ start: number; end: number }>>([]);
  const visible = useDocumentVisible();
  const startedAtRef = useRef(Date.now());
  const queueRef = useRef<MobileNotificationDelivery | null>(null);
  const handlersRef = useRef(new Set<(message: SharedMobileEvent) => void>());
  const sharedStream = useMemo(() => ({ subscribe: (handler: (message: SharedMobileEvent) => void) => {
    handlersRef.current.add(handler);
    return () => { handlersRef.current.delete(handler); };
  } }), []);
  const streamURL = connection.instanceID ? `${cloudURL}/v1/stream?${new URLSearchParams({ instanceId: connection.instanceID })}` : "";
  const scope = streamURL ? mobileStreamScope(streamURL) : "";
  const allowsNotification = useCallback((type: string) => shouldPresentMobileNotification(type, preferencesRef.current) &&
    (Capacitor.isNativePlatform() || preferencesRef.current.notifyWhenHidden), []);

  const reportError = useCallback((error: unknown) => {
    const message = error instanceof Error ? error.message : "通知连接异常";
    setDiagnostic({ at: Date.now(), status: "failed", message: message.slice(0, 250) });
    // 仅记录错误原因，不输出令牌、事件正文或用户消息。
    console.warn("[mobile-notifications]", message);
  }, []);

  const syncPermission = useCallback(async () => {
    const next = Capacitor.isNativePlatform()
      ? toNotificationPermission((await LocalNotifications.checkPermissions()).display)
      : initialPermission();
    setPermission(next);
    return next;
  }, []);

  const requestPermission = useCallback(async () => {
    const next = Capacitor.isNativePlatform()
      ? toNotificationPermission((await LocalNotifications.requestPermissions()).display)
      : typeof Notification === "undefined" ? "unsupported" : await Notification.requestPermission();
    setPermission(next);
    if (next === "granted") {
      // 先更新 ref，避免同一轮事件循环里的首条通知仍读到旧开关。
      preferencesRef.current = { ...preferencesRef.current, systemNotificationsEnabled: true };
      updateLocalPreferences({ systemNotificationsEnabled: true });
      void queueRef.current?.resume().catch(reportError);
    }
    return next;
  }, [reportError, updateLocalPreferences]);

  useEffect(() => {
    const sync = () => setConnection((previous) => {
      const next = currentConnection();
      return previous.activeToken === next.activeToken && previous.instanceID === next.instanceID ? previous : next;
    });
    window.addEventListener(MOBILE_DEVICES_CHANGED_EVENT, sync);
    window.addEventListener("storage", sync);
    sync();
    return () => {
      window.removeEventListener(MOBILE_DEVICES_CHANGED_EVENT, sync);
      window.removeEventListener("storage", sync);
    };
  }, []);

  useEffect(() => {
    if (!connection.activeToken || connection.instanceID) return;
    // 旧配对记录可能只有令牌。设置页启动也必须能回填电脑标识，不能依赖项目页先挂载。
    const token = connection.activeToken;
    let cancelled = false;
    let loading = false;
    let retry: ReturnType<typeof setTimeout> | undefined;
    let request: AbortController | undefined;
    const resolveInstance = async () => {
      if (cancelled || loading || !isCurrentMobileToken(token) || currentConnection().instanceID) return;
      if (retry !== undefined) clearTimeout(retry);
      loading = true;
      request = new AbortController();
      const timer = setTimeout(() => request?.abort(), 15_000);
      try {
        const response = await fetch(`${cloudURL}/v1/instances`, { headers: { Authorization: `Bearer ${token}` }, signal: request.signal });
        if (!response.ok) {
          if (!cancelled && isCurrentMobileToken(token) && (response.status === 401 || response.status === 403)) markDeviceRevoked(token);
          throw new Error(`无法恢复通知所属电脑 (${response.status})`);
        }
        const instances: unknown = await response.json();
        const info = Array.isArray(instances) ? instances[0] : null;
        if (!info || typeof info.instanceId !== "string" || !info.instanceId) throw new Error("尚未获取通知所属电脑，请检查电脑连接。");
        if (!cancelled && !request.signal.aborted && isCurrentMobileToken(token) && !currentConnection().instanceID) {
          updateDevice(token, { instanceId: info.instanceId, name: typeof info.name === "string" ? info.name : "" });
        }
      } catch (error) {
        if (!cancelled) reportError(error);
      } finally {
        clearTimeout(timer);
        loading = false;
        if (!cancelled && isCurrentMobileToken(token) && !currentConnection().instanceID) retry = setTimeout(() => { void resolveInstance(); }, 15_000);
      }
    };
    const onOnline = () => { void resolveInstance(); };
    window.addEventListener("online", onOnline);
    void resolveInstance();
    return () => {
      cancelled = true;
      request?.abort();
      if (retry !== undefined) clearTimeout(retry);
      window.removeEventListener("online", onOnline);
    };
  }, [connection.activeToken, connection.instanceID, reportError]);

  useEffect(() => {
    if (!scope) { queueRef.current = null; setDiagnostic(null); return; }
    const instanceId = connection.instanceID;
    const token = connection.activeToken;
    let cancelled = false;
    const queue = new MobileNotificationDelivery({
      scope, store: localStorage,
      enabled: () => preferencesRef.current.systemNotificationsEnabled && isCurrentMobileToken(token),
      // 旧队列未保存事件类型，保留原投递语义；新事件在每次重试时重新检查用户偏好。
      shouldSend: (item) => !item.eventType || allowsNotification(item.eventType),
      send: async (item) => {
        const id = notificationIDFromEventID(`${scope}:${item.id}`);
        if (Capacitor.isNativePlatform()) await sendNativeMobileNotification(LocalNotifications, id, item.content, instanceId,
          () => !cancelled && preferencesRef.current.systemNotificationsEnabled && isCurrentMobileToken(token) && (!item.eventType || allowsNotification(item.eventType)));
        else {
          if (typeof Notification === "undefined" || Notification.permission !== "granted") throw new Error("浏览器尚未允许系统通知。");
          new Notification(item.content.title, { body: item.content.body, tag: `${scope}:${item.id}` });
        }
      },
      onDiagnostic: (value) => { setDiagnostic(value); if (value.status === "failed") console.warn("[mobile-notifications]", value.message); },
    });
    queueRef.current = queue;
    setDiagnostic(queue.diagnostic);
    void queue.resume().catch(reportError);
    return () => { cancelled = true; queue.stop(); if (queueRef.current === queue) queueRef.current = null; };
  }, [scope, connection.activeToken, connection.instanceID, reportError, allowsNotification]);

  useEffect(() => {
    if (localPreferences.systemNotificationsEnabled) void queueRef.current?.resume().catch(reportError);
  }, [localPreferences.systemNotificationsEnabled, reportError]);

  useEffect(() => {
    let cancelled = false;
    let stateChanges = 0;
    const listeners: PluginListenerHandle[] = [];
    const keep = (handle: PluginListenerHandle) => { if (cancelled) void handle.remove(); else listeners.push(handle); };
    const resume = () => {
      void syncPermission().then(() => { if (!cancelled) return queueRef.current?.resume(); }).catch(reportError);
    };
    resume();
    const recordAwayState = () => {
      const away = document.hidden || backgroundRef.current;
      if (away && awaySinceRef.current === null) awaySinceRef.current = Date.now();
      if (!away && awaySinceRef.current !== null) {
        awayIntervalsRef.current = [...awayIntervalsRef.current, { start: awaySinceRef.current, end: Date.now() }].slice(-20);
        awaySinceRef.current = null;
      }
    };
    const onVisible = () => { recordAwayState(); if (!document.hidden) resume(); };
    window.addEventListener("online", resume);
    document.addEventListener("visibilitychange", onVisible);
    if (Capacitor.isNativePlatform()) {
      const initialStateRevision = stateChanges;
      void CapacitorApp.getState().then(({ isActive }) => {
        if (!cancelled && stateChanges === initialStateRevision) { backgroundRef.current = !isActive; recordAwayState(); }
      }).catch(reportError);
      void CapacitorApp.addListener("appStateChange", ({ isActive }) => {
        stateChanges++;
        backgroundRef.current = !isActive;
        recordAwayState();
        if (isActive) resume();
      }).then(keep).catch(reportError);
      void LocalNotifications.addListener("localNotificationActionPerformed", ({ notification }) => {
        const extra = notification.extra as Partial<NotificationTarget> | undefined;
        if (!extra || (!extra.projectId && !extra.taskId)) return;
        const target = {
          instanceId: typeof extra.instanceId === "string" ? extra.instanceId : connectionRef.current.instanceID,
          projectId: typeof extra.projectId === "string" ? extra.projectId : "",
          conversationId: typeof extra.conversationId === "string" ? extra.conversationId : "",
          taskId: typeof extra.taskId === "string" ? extra.taskId : "",
        };
        const device = readDevices().find((item) => item.instanceId === target.instanceId && !item.revoked);
        if (!device) return;
        if (readActiveToken() !== device.token) setActiveToken(device.token);
        setPendingTarget(target);
        navigateRef.current("/mobile");
      }).then(keep).catch(reportError);
    }
    return () => {
      cancelled = true;
      for (const handle of listeners) void handle.remove();
      window.removeEventListener("online", resume);
      document.removeEventListener("visibilitychange", onVisible);
    };
  }, [reportError, syncPermission]);

  useMobileRemoteEventStream({
    ...connection, deviceEpoch: 0, visible, streamURL,
    isCurrentToken: isCurrentMobileToken,
    initialCursor: (signal) => initializeMobileStreamCursor(localStorage, scope,
      `${cloudURL}/v1/events?${new URLSearchParams({ instanceId: connection.instanceID })}`, connection.activeToken, signal),
    onCursor: (cursor) => saveMobileStreamCursor(localStorage, scope, cursor),
    onError: (error) => {
      reportError(error);
      if (error instanceof Error && /\(401\)|\(403\)/.test(error.message)) {
        markDeviceRevoked(connection.activeToken);
        window.dispatchEvent(new Event("milevia:token-cleared"));
      }
    },
    onEvent: (event) => {
      const value = JSON.parse(String(event.data)) as { eventId?: string; type?: string; createdAt?: string; taskId?: string; payload?: unknown };
      const baseline = readMobileNotificationBaseline(localStorage, scope);
      if (baseline === null) throw new Error("通知接收基线不可用，请检查本地存储。");
      const afterBaseline = Number(event.lastEventId) > Number(baseline);
      const createdAt = Date.parse(value.createdAt || "");
      const recent = !Number.isFinite(createdAt) || createdAt >= Date.now() - MOBILE_NOTIFICATION_MAX_AGE_MS;
      const away = shouldNotifyWhileAway({ isNative: Capacitor.isNativePlatform(), nativeInBackground: backgroundRef.current, documentHidden: document.hidden });
      // 锁屏期间积压、回前台才被 JS 处理的完成事件，也属于补发，不能按“此刻在前台”丢弃。
      const missedWhileAway = awayIntervalsRef.current.some((interval) => createdAt >= interval.start && createdAt <= interval.end);
      const recovered = Number.isFinite(createdAt) && (createdAt < startedAtRef.current || missedWhileAway) && isRecoverableMobileNotification(value.type);
      const content = mobileNotificationContent(value);
      if (content && value.eventId && afterBaseline) {
        if (recent && (away || recovered) && allowsNotification(value.type || "")) {
          queueRef.current?.enqueue(value.eventId, content, Number.isFinite(createdAt) ? createdAt : Date.now(), value.type);
          void queueRef.current?.flush().catch(reportError);
        } else {
          queueRef.current?.rememberHandled(value.eventId);
        }
      }
      for (const handler of handlersRef.current) {
        try { handler({ ...connection, event }); } catch (error) { reportError(error); }
      }
      return true;
    },
    refreshSnapshot: () => {}, refreshGit: () => {},
  });

  const testNotification = useCallback(async () => {
    try {
      if (!Capacitor.isNativePlatform()) throw new Error("测试通知仅适用于手机 App。");
      await sendNativeMobileNotification(LocalNotifications, notificationIDFromEventID(`milevia-test:${Date.now()}`), {
        title: "Milevia 测试通知", body: "收到这条通知，说明系统通知通道可以使用。", projectId: "", conversationId: "", taskId: "",
      }, connectionRef.current.instanceID);
      setDiagnostic({ at: Date.now(), status: "scheduled", message: "测试通知已交给系统，请查看通知栏。" });
    } catch (error) { reportError(error); throw error; }
  }, [reportError]);

  const clearTarget = useCallback(() => setPendingTarget(null), []);
  return <MobileRemoteEventStreamContext.Provider value={sharedStream}>
    <MobileNotificationsContext.Provider value={{ permission, requestPermission, testNotification, diagnostic, pendingTarget, clearTarget }}>
      {children}
      <Toaster position="top-center" />
    </MobileNotificationsContext.Provider>
  </MobileRemoteEventStreamContext.Provider>;
}
