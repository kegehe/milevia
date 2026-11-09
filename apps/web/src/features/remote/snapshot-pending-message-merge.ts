import { mergeMessageAttachments } from "../files/message-attachment-model";

export type PendingMessage = { requestId: string; content: string; createdAt: string; attachments?: { path: string }[] };

type SnapshotMessage = { id: string; role: string; content: string; createdAt: string; clientRequestId?: string; attachments?: { path: string }[] };
export function pendingMessageContentKey(message: { content: string; attachments?: { path: string }[] }): string {
  return JSON.stringify([message.content, mergeMessageAttachments(message.attachments || []).map((file) => file.path)]);
}

// New servers return the durable request identity, including an empty string
// for messages sent elsewhere. Content matching is only a legacy fallback.
export function pendingMessageMatches(pending: PendingMessage, message: Pick<SnapshotMessage, "content" | "attachments" | "clientRequestId">): boolean {
  return typeof message.clientRequestId === "string"
    ? message.clientRequestId === pending.requestId
    : pendingMessageContentKey(pending) === pendingMessageContentKey(message);
}

export function hasOtherPendingMessages(pending: Map<string, PendingMessage> | undefined, requestId: string): boolean {
  return pending !== undefined && [...pending.keys()].some((id) => id !== requestId);
}
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
      const persistedRequests = new Set<string>();
      for (const message of conversation.messages) {
        if (message.role !== "user") continue;
        if (typeof message.clientRequestId === "string") {
          if (message.clientRequestId) persistedRequests.add(message.clientRequestId);
          continue;
        }
        const key = pendingMessageContentKey(message);
        persistedCounts.set(key, (persistedCounts.get(key) || 0) + 1);
      }
      const additions: M[] = [];
      const matchedRequestIDs = new Set<string>();
      for (const message of queued.values()) {
        if (persistedRequests.has(message.requestId)) {
          matchedRequestIDs.add(message.requestId);
          continue;
        }
        const key = pendingMessageContentKey(message);
        const count = persistedCounts.get(key) || 0;
        if (count > 0) {
          persistedCounts.set(key, count - 1);
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
