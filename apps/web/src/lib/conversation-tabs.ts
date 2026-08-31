export const MAX_OPEN_CONVERSATION_TABS = 12;

export type ConversationActivityPosition = {
  createdAt: string;
  id: string;
};

export type ConversationTabsState = {
  openConversationIds: string[];
  activeConversationId: string | null;
  readPositions: Record<string, ConversationActivityPosition>;
  latestPositions: Record<string, ConversationActivityPosition>;
  unreadConversationIds: string[];
};

type StorageLike = Pick<Storage, "getItem" | "setItem">;

const tabListeners = new Map<string, Set<() => void>>();

function storageKey(projectId: string): string {
  return `milevia.conversation-tabs.v1:${encodeURIComponent(projectId)}`;
}

const emptyState = (): ConversationTabsState => ({ openConversationIds: [], activeConversationId: null, readPositions: {}, latestPositions: {}, unreadConversationIds: [] });

function validPosition(value: unknown): ConversationActivityPosition | null {
  if (!value || typeof value !== "object") return null;
  const record = value as Record<string, unknown>;
  return typeof record.createdAt === "string" && record.createdAt && typeof record.id === "string" && record.id
    ? { createdAt: record.createdAt, id: record.id }
    : null;
}

function positionsForOpenTabs(value: unknown, openConversationIds: string[]): Record<string, ConversationActivityPosition> {
  if (!value || typeof value !== "object") return {};
  const positions: Record<string, ConversationActivityPosition> = {};
  const record = value as Record<string, unknown>;
  openConversationIds.forEach((id) => {
    const position = validPosition(record[id]);
    if (position) positions[id] = position;
  });
  return positions;
}

function validState(value: unknown): ConversationTabsState | null {
  if (!value || typeof value !== "object") return null;
  const record = value as Record<string, unknown>;
  if (!Array.isArray(record.openConversationIds) || !record.openConversationIds.every((id) => typeof id === "string" && id.length > 0)) return null;
  const openConversationIds = [...new Set(record.openConversationIds)].slice(0, MAX_OPEN_CONVERSATION_TABS);
  const activeConversationId = typeof record.activeConversationId === "string" && openConversationIds.includes(record.activeConversationId)
    ? record.activeConversationId
    : openConversationIds[0] || null;
  const readPositions = positionsForOpenTabs(record.readPositions, openConversationIds);
  const latestPositions = positionsForOpenTabs(record.latestPositions, openConversationIds);
  const unreadConversationIds = Array.isArray(record.unreadConversationIds)
    ? [...new Set(record.unreadConversationIds.filter((id): id is string => typeof id === "string" && openConversationIds.includes(id)))]
    : [];
  return { openConversationIds, activeConversationId, readPositions, latestPositions, unreadConversationIds };
}

export function readConversationTabs(projectId: string, storage: StorageLike | null = typeof window === "undefined" ? null : window.sessionStorage): ConversationTabsState {
  if (!storage || !projectId) return emptyState();
  try {
    return validState(JSON.parse(storage.getItem(storageKey(projectId)) || "null")) || emptyState();
  } catch {
    return emptyState();
  }
}

export function writeConversationTabs(projectId: string, state: ConversationTabsState, storage: StorageLike | null = typeof window === "undefined" ? null : window.sessionStorage): void {
  if (!storage || !projectId) return;
  storage.setItem(storageKey(projectId), JSON.stringify(state));
  tabListeners.get(projectId)?.forEach((listener) => listener());
}

export function subscribeConversationTabs(projectId: string, listener: () => void): () => void {
  if (!projectId) return () => undefined;
  let listeners = tabListeners.get(projectId);
  if (!listeners) {
    listeners = new Set();
    tabListeners.set(projectId, listeners);
  }
  listeners.add(listener);
  return () => {
    listeners?.delete(listener);
    if (listeners?.size === 0) tabListeners.delete(projectId);
  };
}

export function openConversationTab(state: ConversationTabsState, conversationId: string): ConversationTabsState | null {
  if (!conversationId) return state;
  if (state.openConversationIds.includes(conversationId)) return { ...state, activeConversationId: conversationId };
  if (state.openConversationIds.length >= MAX_OPEN_CONVERSATION_TABS) return null;
  return { ...state, openConversationIds: [...state.openConversationIds, conversationId], activeConversationId: conversationId };
}

export function closeConversationTab(state: ConversationTabsState, conversationId: string): ConversationTabsState {
  const index = state.openConversationIds.indexOf(conversationId);
  if (index < 0) return state;
  const openConversationIds = state.openConversationIds.filter((id) => id !== conversationId);
  const activeConversationId = state.activeConversationId === conversationId
    ? openConversationIds[index] || openConversationIds[index - 1] || null
    : state.activeConversationId;
  const { [conversationId]: _readPosition, ...readPositions } = state.readPositions;
  const { [conversationId]: _latestPosition, ...latestPositions } = state.latestPositions;
  return { ...state, openConversationIds, activeConversationId, readPositions, latestPositions, unreadConversationIds: state.unreadConversationIds.filter((id) => id !== conversationId) };
}

export function recordConversationActivity(state: ConversationTabsState, conversationId: string, latestPosition: ConversationActivityPosition | null, hasNewActivity: boolean, readImmediately: boolean): ConversationTabsState {
  if (!latestPosition || !state.openConversationIds.includes(conversationId)) return state;
  const samePosition = (position: ConversationActivityPosition | undefined) => position?.createdAt === latestPosition.createdAt && position.id === latestPosition.id;
  const alreadyUnread = state.unreadConversationIds.includes(conversationId);
  if ((!readImmediately && !hasNewActivity && samePosition(state.latestPositions[conversationId])) || (readImmediately && samePosition(state.latestPositions[conversationId]) && samePosition(state.readPositions[conversationId]) && !alreadyUnread)) return state;
  const latestPositions = { ...state.latestPositions, [conversationId]: latestPosition };
  const readPositions = { ...state.readPositions };
  const unread = new Set(state.unreadConversationIds);
  if (readImmediately) {
    readPositions[conversationId] = latestPosition;
    unread.delete(conversationId);
  } else if (hasNewActivity) {
    unread.add(conversationId);
  }
  return { ...state, readPositions, latestPositions, unreadConversationIds: [...unread] };
}

export function markConversationTabRead(state: ConversationTabsState, conversationId: string): ConversationTabsState {
  const latestPosition = state.latestPositions[conversationId];
  if (!latestPosition && !state.unreadConversationIds.includes(conversationId)) return state;
  const readPositions = latestPosition ? { ...state.readPositions, [conversationId]: latestPosition } : state.readPositions;
  return { ...state, readPositions, unreadConversationIds: state.unreadConversationIds.filter((id) => id !== conversationId) };
}
