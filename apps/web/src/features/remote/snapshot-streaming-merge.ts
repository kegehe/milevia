export type StreamingMessage = { id: string; content: string };
export type StreamingMessageBuffer = { conversationId: string; content: string; createdAt: string };

type SnapshotLike<M extends StreamingMessage> = {
  projects: Array<{ conversations: Array<{ id: string; messages: M[] }> }>;
};

export function mergeSnapshotStreamingMessages<S extends SnapshotLike<M>, M extends StreamingMessage>(
  snapshot: S,
  streaming: Map<string, StreamingMessageBuffer>,
  createPlaceholder: (messageID: string, chunk: StreamingMessageBuffer) => M,
): S {
  if (streaming.size === 0) return snapshot;
  const projects = snapshot.projects.map((project) => ({
    ...project,
    conversations: project.conversations.map((conversation) => {
      let changed = false;
      const messages = conversation.messages.map((message) => {
        if (!message.id.startsWith("stream-")) return message;
        const chunk = streaming.get(message.id.slice("stream-".length));
        if (!chunk || chunk.conversationId !== conversation.id) return message;
        changed = true;
        return { ...message, content: chunk.content };
      });
      const pending: M[] = [];
      for (const [messageID, chunk] of streaming) {
        if (chunk.conversationId !== conversation.id) continue;
        const placeholderID = "stream-" + messageID;
        if (messages.some((message) => message.id === placeholderID)) continue;
        pending.push(createPlaceholder(messageID, chunk));
        changed = true;
      }
      return changed ? { ...conversation, messages: [...messages, ...pending] } : conversation;
    }),
  }));
  return { ...snapshot, projects } as S;
}
