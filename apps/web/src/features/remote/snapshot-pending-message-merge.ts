export type PendingMessage = { requestId: string; content: string; createdAt: string };

type SnapshotMessage = { id: string; role: string; content: string; createdAt: string };
type SnapshotLike<M extends SnapshotMessage> = {
  projects: Array<{ conversations: Array<{ id: string; messages: M[] }> }>;
};

export function mergeSnapshotPendingMessages<S extends SnapshotLike<M>, M extends SnapshotMessage>(
  snapshot: S,
  pending: Map<string, Map<string, PendingMessage>>,
  createOptimisticMessage: (message: PendingMessage) => M,
): S {
  if (pending.size === 0) return snapshot;
  const projects = snapshot.projects.map((project) => ({
    ...project,
    conversations: project.conversations.map((conversation) => {
      const queued = pending.get(conversation.id);
      if (!queued || queued.size === 0) return conversation;
      const persistedCounts = new Map<string, number>();
      for (const message of conversation.messages) {
        if (message.role === "user") persistedCounts.set(message.content, (persistedCounts.get(message.content) || 0) + 1);
      }
      const additions: M[] = [];
      const matchedRequestIDs = new Set<string>();
      for (const message of queued.values()) {
        const count = persistedCounts.get(message.content) || 0;
        if (count > 0) {
          persistedCounts.set(message.content, count - 1);
          matchedRequestIDs.add(message.requestId);
        } else additions.push(createOptimisticMessage(message));
      }
      for (const requestID of matchedRequestIDs) queued.delete(requestID);
      if (queued.size === 0) pending.delete(conversation.id);
      return additions.length > 0 ? { ...conversation, messages: [...conversation.messages, ...additions] } : conversation;
    }),
  }));
  return { ...snapshot, projects } as S;
}
