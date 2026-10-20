import { useEffect, useMemo, useRef, useSyncExternalStore } from "react";
import { createMobileConversationHistory, type HistoryAdapters } from "./mobile-conversation-history";

export function useMobileConversationHistory<M, I>(options: HistoryAdapters<M, I> & {
  instanceID: string;
  activeToken: string;
  deviceEpoch: number;
  projectID: string;
  readToken: () => string;
}) {
  const scope = JSON.stringify([options.instanceID, options.activeToken, options.deviceEpoch, options.projectID]);
  const scopeRef = useRef(scope);
  scopeRef.current = scope;
  const controller = useMemo(() => createMobileConversationHistory({
    ...options,
    isCurrent: () => Boolean(options.instanceID) && scopeRef.current === scope && options.readToken() === options.activeToken,
    schedule: (callback) => {
      const timer = window.setTimeout(callback, 0);
      return () => window.clearTimeout(timer);
    },
    // 设备/实例/项目不变时保留同一控制器，避免内联适配器导致重复加载。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }), [scope]);
  const state = useSyncExternalStore(controller.subscribe, controller.getSnapshot, controller.getSnapshot);
  useEffect(() => {
    controller.activate();
    return () => controller.dispose();
  }, [controller]);
  return { ...state, load: controller.load, loadDirectory: controller.loadDirectory };
}
