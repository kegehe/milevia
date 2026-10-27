import { useCallback } from "react";
import { useLiveStateEvents } from "./LiveEventsProvider";
import { useNotificationRefresh } from "./NotificationProvider";

/** 位于两个 Provider 内部；重连的 all 信号也必须补齐断线期间遗漏的通知清理。 */
export function NotificationInvalidationBridge() {
  const refreshUnread = useNotificationRefresh();
  useLiveStateEvents(useCallback((event) => {
    if (event.type === "notifications" || event.type === "all") refreshUnread();
  }, [refreshUnread]));
  return null;
}
