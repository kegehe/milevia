export type SnapshotNotice = { id: string; merge?: "append" | "replace" };

type SnapshotLike<N extends SnapshotNotice> = {
  projects: Array<{ conversations: Array<{ id: string; notices?: N[] }> }>;
};

export function mergeSnapshotNotices<S extends SnapshotLike<N>, N extends SnapshotNotice>(
  snapshot: S,
  pendingNotices: Map<string, N[]>,
  parseSnapshotNotices: (raw: unknown) => N[],
  mergeNotices: (serverNotices: N[], remaining: N[]) => N[],
): S {
  const projects = snapshot.projects.map((project) => ({
    ...project,
    conversations: project.conversations.map((conversation) => {
      const serverNotices = parseSnapshotNotices(conversation.notices);
      const local = pendingNotices.get(conversation.id);
      if (!local || local.length === 0) return { ...conversation, notices: serverNotices };
      const serverIDs = new Set(serverNotices.map((notice) => notice.id));
      const remaining = local.filter((notice) => notice.merge === "replace" || !serverIDs.has(notice.id));
      if (remaining.length > 0) pendingNotices.set(conversation.id, remaining);
      else pendingNotices.delete(conversation.id);
      return { ...conversation, notices: mergeNotices(serverNotices, remaining) };
    }),
  }));
  return { ...snapshot, projects } as S;
}
