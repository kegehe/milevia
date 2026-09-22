import { createContext, useCallback, useContext, useEffect, useState, type ReactNode } from "react";
import { Capacitor } from "@capacitor/core";
import { api } from "../lib/api";
import type { AgentID, PermissionMode } from "../lib/types";
import { isDesktop } from "../lib/runtime";
import { isClockTime, webNotificationsSupported } from "../lib/notifications";

const STORAGE_KEY = "milevia:settings:v1";

export type AppPreferences = {
  defaultAgentId: AgentID;
  claudePermissionMode: Extract<PermissionMode, "approval_required" | "full_control">;
  codexPermissionMode: Extract<PermissionMode, "read_only" | "workspace_write" | "full_control">;
  // 按工具目录索引的默认权限模式（服务端派生视图）。新代码读它，不再分别读上面两个
  // 历史字段 —— 那两个字段是存储层的历史包袱，前端不该再去理解它们的对应关系。
  agentPermissionModes?: Partial<Record<string, PermissionMode>>;
  autoReview: boolean;
  updatedAt?: string;
};

export type LocalPreferences = {
  systemNotificationsEnabled: boolean;
  notifyWhenHidden: boolean;
  windowsToastsEnabled: boolean;
  taskNotificationsEnabled: boolean;
  lowPriorityNotificationsEnabled: boolean;
  quietHoursEnabled: boolean;
  quietHoursStart: string;
  quietHoursEnd: string;
  draftAutoSave: boolean;
  draftRetentionDays: 7 | 30 | 90;
};

type UIPreferencesContextValue = {
  appPreferences: AppPreferences;
  appPreferencesLoading: boolean;
  appPreferencesError: string;
  updateAppPreferences: (patch: Partial<Pick<AppPreferences, "defaultAgentId" | "claudePermissionMode" | "codexPermissionMode" | "autoReview">>) => Promise<AppPreferences>;
  localPreferences: LocalPreferences;
  updateLocalPreferences: (patch: Partial<LocalPreferences>) => void;
  resetLocalPreferences: () => void;
  notificationPermission: NotificationPermission | "unsupported";
  requestSystemNotificationPermission: () => Promise<NotificationPermission | "unsupported">;
};

const safeAppDefaults: AppPreferences = {
  defaultAgentId: "claude-code",
  claudePermissionMode: "approval_required",
  codexPermissionMode: "workspace_write",
  autoReview: false,
};

const defaultLocalPreferences: LocalPreferences = {
  systemNotificationsEnabled: false,
  notifyWhenHidden: true,
  windowsToastsEnabled: false,
  taskNotificationsEnabled: true,
  lowPriorityNotificationsEnabled: false,
  quietHoursEnabled: false,
  quietHoursStart: "22:00",
  quietHoursEnd: "08:00",
  draftAutoSave: true,
  draftRetentionDays: 30,
};

const UIPreferencesContext = createContext<UIPreferencesContextValue | null>(null);

/**
 * 当前环境能不能真的用上 Web Notification（判定依据见 lib/notifications.ts）。
 * 桌面端与原生包里这条 API 是死路：权限永远拿不到 granted、`new Notification()` 也没人渲染，
 * 所以这里一律报"不支持"，而不是把 WebView2 那个无法被用户改掉的 denied 当成"被拒绝"摆出去。
 */
function webNotificationsAvailable(): boolean {
  return webNotificationsSupported({
    hasNotificationAPI: typeof Notification !== "undefined",
    isDesktop: isDesktop(),
    isNativePlatform: Capacitor.isNativePlatform(),
  });
}

function getNotificationPermission(): NotificationPermission | "unsupported" {
  if (!webNotificationsAvailable()) return "unsupported";
  return Notification.permission;
}

function isRetentionDays(value: unknown): value is LocalPreferences["draftRetentionDays"] {
  return value === 7 || value === 30 || value === 90;
}

function readLocalPreferences(): LocalPreferences {
  try {
    const raw = window.localStorage.getItem(STORAGE_KEY);
    if (!raw) {
      // Earlier versions could already have browser permission without an
      // explicit product preference. Preserve that behavior on first upgrade.
      return { ...defaultLocalPreferences, systemNotificationsEnabled: getNotificationPermission() === "granted" };
    }
    const parsed: unknown = JSON.parse(raw);
    if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) return defaultLocalPreferences;
    const value = parsed as Partial<LocalPreferences>;
    return {
      systemNotificationsEnabled: typeof value.systemNotificationsEnabled === "boolean" ? value.systemNotificationsEnabled : defaultLocalPreferences.systemNotificationsEnabled,
      notifyWhenHidden: typeof value.notifyWhenHidden === "boolean" ? value.notifyWhenHidden : defaultLocalPreferences.notifyWhenHidden,
      windowsToastsEnabled: typeof value.windowsToastsEnabled === "boolean" ? value.windowsToastsEnabled : defaultLocalPreferences.windowsToastsEnabled,
      taskNotificationsEnabled: typeof value.taskNotificationsEnabled === "boolean" ? value.taskNotificationsEnabled : defaultLocalPreferences.taskNotificationsEnabled,
      lowPriorityNotificationsEnabled: typeof value.lowPriorityNotificationsEnabled === "boolean" ? value.lowPriorityNotificationsEnabled : defaultLocalPreferences.lowPriorityNotificationsEnabled,
      quietHoursEnabled: typeof value.quietHoursEnabled === "boolean" ? value.quietHoursEnabled : defaultLocalPreferences.quietHoursEnabled,
      quietHoursStart: isClockTime(value.quietHoursStart) ? value.quietHoursStart : defaultLocalPreferences.quietHoursStart,
      quietHoursEnd: isClockTime(value.quietHoursEnd) ? value.quietHoursEnd : defaultLocalPreferences.quietHoursEnd,
      draftAutoSave: typeof value.draftAutoSave === "boolean" ? value.draftAutoSave : defaultLocalPreferences.draftAutoSave,
      draftRetentionDays: isRetentionDays(value.draftRetentionDays) ? value.draftRetentionDays : defaultLocalPreferences.draftRetentionDays,
    };
  } catch {
    return defaultLocalPreferences;
  }
}

function persistLocalPreferences(preferences: LocalPreferences): void {
  try {
    window.localStorage.setItem(STORAGE_KEY, JSON.stringify(preferences));
  } catch {
    // Private mode or a full storage quota should not block the current session.
  }
}

export function UIPreferencesProvider({ children }: { children: ReactNode }) {
  const [appPreferences, setAppPreferences] = useState<AppPreferences>(safeAppDefaults);
  const [appPreferencesLoading, setAppPreferencesLoading] = useState(true);
  const [appPreferencesError, setAppPreferencesError] = useState("");
  const [localPreferences, setLocalPreferences] = useState<LocalPreferences>(readLocalPreferences);
  const [notificationPermission, setNotificationPermission] = useState<NotificationPermission | "unsupported">(getNotificationPermission);

  useEffect(() => { persistLocalPreferences(localPreferences); }, [localPreferences]);

  useEffect(() => {
    let cancelled = false;
    api<AppPreferences>("/api/preferences")
      .then((preferences) => { if (!cancelled) setAppPreferences(preferences); })
      .catch((cause: unknown) => { if (!cancelled) setAppPreferencesError(cause instanceof Error ? cause.message : "无法加载应用偏好"); })
      .finally(() => { if (!cancelled) setAppPreferencesLoading(false); });
    return () => { cancelled = true; };
  }, []);

  useEffect(() => {
    const refreshPermission = () => setNotificationPermission(getNotificationPermission());
    document.addEventListener("visibilitychange", refreshPermission);
    return () => document.removeEventListener("visibilitychange", refreshPermission);
  }, []);

  const updateAppPreferences = useCallback(async (patch: Partial<Pick<AppPreferences, "defaultAgentId" | "claudePermissionMode" | "codexPermissionMode" | "autoReview">>) => {
    const next = await api<AppPreferences>("/api/preferences", { method: "PATCH", body: JSON.stringify(patch) });
    setAppPreferences(next);
    setAppPreferencesError("");
    return next;
  }, []);

  const updateLocalPreferences = useCallback((patch: Partial<LocalPreferences>) => {
    setLocalPreferences((current) => ({ ...current, ...patch }));
  }, []);

  const resetLocalPreferences = useCallback(() => {
    setLocalPreferences(defaultLocalPreferences);
  }, []);

  const requestSystemNotificationPermission = useCallback(async () => {
    if (!webNotificationsAvailable()) return "unsupported" as const;
    const permission = Notification.permission === "default" ? await Notification.requestPermission() : Notification.permission;
    setNotificationPermission(permission);
    if (permission === "granted") updateLocalPreferences({ systemNotificationsEnabled: true });
    return permission;
  }, [updateLocalPreferences]);

  return <UIPreferencesContext.Provider value={{ appPreferences, appPreferencesLoading, appPreferencesError, updateAppPreferences, localPreferences, updateLocalPreferences, resetLocalPreferences, notificationPermission, requestSystemNotificationPermission }}>{children}</UIPreferencesContext.Provider>;
}

export function useUIPreferences(): UIPreferencesContextValue {
  const context = useContext(UIPreferencesContext);
  if (!context) throw new Error("useUIPreferences must be used within UIPreferencesProvider");
  return context;
}
