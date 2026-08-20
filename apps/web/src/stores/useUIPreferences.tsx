import { createContext, useCallback, useContext, useEffect, useState, type ReactNode } from "react";
import { api } from "../lib/api";
import type { AgentID, PermissionMode } from "../lib/types";
import { isClockTime } from "../lib/notifications";

const STORAGE_KEY = "milevia:settings:v1";

export type AppPreferences = {
  defaultAgentId: AgentID;
  claudePermissionMode: Extract<PermissionMode, "approval_required" | "full_control">;
  codexPermissionMode: Extract<PermissionMode, "read_only" | "workspace_write" | "full_control">;
  updatedAt?: string;
};

export type LocalPreferences = {
  systemNotificationsEnabled: boolean;
  notifyWhenHidden: boolean;
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
  updateAppPreferences: (patch: Partial<Pick<AppPreferences, "defaultAgentId" | "claudePermissionMode" | "codexPermissionMode">>) => Promise<AppPreferences>;
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
};

const defaultLocalPreferences: LocalPreferences = {
  systemNotificationsEnabled: false,
  notifyWhenHidden: true,
  taskNotificationsEnabled: true,
  lowPriorityNotificationsEnabled: false,
  quietHoursEnabled: false,
  quietHoursStart: "22:00",
  quietHoursEnd: "08:00",
  draftAutoSave: true,
  draftRetentionDays: 30,
};

const UIPreferencesContext = createContext<UIPreferencesContextValue | null>(null);

function getNotificationPermission(): NotificationPermission | "unsupported" {
  if (typeof Notification === "undefined") return "unsupported";
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

  const updateAppPreferences = useCallback(async (patch: Partial<Pick<AppPreferences, "defaultAgentId" | "claudePermissionMode" | "codexPermissionMode">>) => {
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
    if (typeof Notification === "undefined") return "unsupported" as const;
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
