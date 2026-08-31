import { useSyncExternalStore } from "react";
import { readConversationTabs, subscribeConversationTabs } from "./conversation-tabs";

export function useActiveConversationId(projectId: string | undefined): string | undefined {
  const value = useSyncExternalStore(
    (listener) => subscribeConversationTabs(projectId || "", listener),
    () => projectId ? readConversationTabs(projectId).activeConversationId : null,
    () => null,
  );
  return value || undefined;
}
