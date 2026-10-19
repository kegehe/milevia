export type RealtimeMessage = { id: string; content: string };

export type RealtimeMessageBuffer<M extends RealtimeMessage> = {
  conversationId: string;
  message: M;
  revision: number;
};

type SnapshotLike<M extends RealtimeMessage> = {
  projects: Array<{ conversations: Array<{ id: string; messages: M[] }> }>;
};

// A snapshot only consumes a realtime message after it contains at least the
// same content. Older snapshots must retain the buffer for the next merge.
export function mergeSnapshotRealtimeMessages<S extends SnapshotLike<M>, M extends RealtimeMessage>(
  snapshot: S,
  realtimeMessages: Map<string, RealtimeMessageBuffer<M>>,
  minimumRevision: number,
): S {
  if (realtimeMessages.size === 0) return snapshot;
  const projects = snapshot.projects.map((project) => ({
    ...project,
    conversations: project.conversations.map((conversation) => {
      let changed = false;
      const messages = conversation.messages.map((message) => {
        const live = realtimeMessages.get(message.id);
        if (!live || live.conversationId !== conversation.id) return message;
        if (live.message.content.length > message.content.length) {
          changed = true;
          return live.message;
        }
        realtimeMessages.delete(message.id);
        return message;
      });
      const extras = [...realtimeMessages.values()]
        .filter((item) => item.conversationId === conversation.id
          && item.revision >= minimumRevision
          && !conversation.messages.some((message) => message.id === item.message.id))
        .map((item) => item.message);
      return changed || extras.length > 0 ? { ...conversation, messages: [...messages, ...extras] } : conversation;
    }),
  }));
  return { ...snapshot, projects } as S;
}
