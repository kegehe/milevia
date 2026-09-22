// 手机端「新建会话」的乐观更新规则。
//
// 和 `task-mutations.ts` 同一套思路：会话先在**本机**建好 —— 立刻出现在列表里、能进去、
// 能打字，命令只是事后发给电脑端的一条消息。电脑端分配的真 id 回来之后，再把本地那条
// 换成真身。
//
// 为什么必须单独一个模块：这段规则错了的表现是"用户刚建的会话凭空消失一次"或
// "在还没建成的会话里发的消息永远发不出去"。两种都发生在乐观更新那条异步链上，
// 靠读代码看不出来，必须能直接跑行为断言。
//
// ⚠️ 会话说到底是**电脑端的**资源：真 id 由电脑端分配，消息也只能发给真 id。所以
// 本地这条是"占位"，不是"副本" —— 它存在的唯一理由是让用户不必等那一次往返。

/** 本地临时会话 id 的前缀。是不是"还没落地的会话"全靠它判断，因此生成与判断放在一起。 */
export const PENDING_CONVERSATION_ID_PREFIX = "pending-conversation-";

export function isPendingConversationID(id: string): boolean {
  return id.startsWith(PENDING_CONVERSATION_ID_PREFIX);
}

export function newPendingConversationID(): string {
  const suffix = globalThis.crypto?.randomUUID?.() || `${Date.now()}-${Math.random().toString(36).slice(2)}`;
  return `${PENDING_CONVERSATION_ID_PREFIX}${suffix}`;
}

/** 一条"本地已经建好、等电脑端分配真 id"的会话。 */
export type PendingConversation = {
  /** 本地临时 id。界面选中态、乐观消息、运行指示全用它索引。 */
  id: string;
  projectId: string;
  /** claude-code / codex。真身还没回来之前，那张卡上的 Agent 名只能靠它。 */
  agentId: string;
  createdAt: string;
  /** 命令发出后回填，只用于诊断。 */
  commandId: string;
  /**
   * 发起创建这一刻，这个项目里**已经**有哪些会话。
   * 命令回执丢了（30 秒没等到终态）时，靠它把"快照里多出来的那条"认回来 ——
   * 那时我们手上没有任何 id，只能这样对暗号。
   */
  knownConversationIds: string[];
  /**
   * 命令回执里带回来的真 id。有它就按 id 精确认领，不再用上面那条启发式
   * （启发式在"同时在电脑端手动建了一条会话"时会认错人）。
   */
  resolvedId?: string;
  /**
   * 登记这条会话时，本机看到的快照版本号。
   *
   * ⚠️ 交棒（把本地卡摘掉、完全以快照为准）的判据是"快照版本号**真的前进过**"，
   * 而不是"快照里出现了这条会话"：`conversation.created` 事件会先把会话塞进**当前**那版
   * 快照里，而紧随其后的那次刷新拿到的很可能还是"创建之前"的那一版 —— 版本号没变，
   * 内容里没有这条会话。按"出现了就交棒"，那一刷就会把卡片和用户刚打的消息整块盖掉。
   */
  seenRevision: number;
};

export type PendingConversationMessage = { requestId: string; content: string; createdAt: string };

/** 快照里一条会话的形状。只列叠加层要读写的字段，多出来的（notices 等）原样穿透。 */
export type PendingConversationCard = {
  id: string;
  title: string;
  status: string;
  agentId: string;
  lastActivityAt: string;
  isCurrent: boolean;
  messages: { id: string; role: "user" | "assistant"; content: string; createdAt: string }[];
};

/**
 * 本地那张「创建中」的会话卡。`status: "creating"` 是本机专有的，云端永远不会返回它 ——
 * 文案在页面的 conversationStatusLabel 里补一档。
 *
 * 卡片上要带上已经发出去的消息：这时候会话还不在快照里，那条消息也没有别的地方可挂，
 * 不带就等于用户打完字、气泡凭空不见了。
 */
export function pendingConversationCard(item: PendingConversation, messages: PendingConversationMessage[]): PendingConversationCard {
  return {
    // ⚠️ 真 id 一到就**立刻**改用它，但这条本地卡要留到「快照版本号真的前进过、而且里面
    // 有这条会话」为止（见 seenRevision 与调用方的认领 effect）。拿到 id 就删卡、指望
    // 快照顶上是不行的：紧随其后的那次刷新很可能还是"创建之前"那一版 —— 卡片连同用户
    // 刚打的消息会当着用户的面消失一次。
    id: item.resolvedId || item.id,
    title: "新会话",
    status: item.resolvedId ? "idle" : "creating",
    agentId: item.agentId,
    lastActivityAt: item.createdAt,
    isCurrent: true,
    messages: messages.map((message) => ({
      id: `pending-${message.requestId}`,
      role: "user" as const,
      content: message.content,
      createdAt: message.createdAt,
    })),
  };
}

/**
 * 把待确认的会话叠到快照上。一个待确认项有两种落法，按"快照里有没有它"分流：
 *
 *   · 快照里**还没有** → 在列表最前画一张卡，其余会话让位（isCurrent = false）；
 *   · 快照里**已经有**（`conversation.created` 抢先塞进来的那种）→ 不画卡，改为把
 *     还没落地的乐观气泡**补到那条会话上**。这一步不能省：快照里那条是空心壳，
 *     补不上就等于用户的刚发出去的消息凭空消失一帧，而链路上没有任何人负责补它。
 *
 * 没有任何待确认项时**原样返回入参**（同一个数组引用）—— 调用方拿它去喂 useMemo，
 * 每次都给新对象会让整条依赖链失效、白白重渲染。
 */
export function applyPendingConversations<P extends { id: string; conversations: PendingConversationCard[] }>(
  projects: P[],
  pending: Iterable<PendingConversation>,
  messagesFor: (conversationId: string) => PendingConversationMessage[],
): P[] {
  const items = Array.from(pending);
  if (items.length === 0) return projects;
  const byProject = new Map<string, PendingConversation[]>();
  for (const item of items) {
    const list = byProject.get(item.projectId) || [];
    list.push(item);
    byProject.set(item.projectId, list);
  }
  return projects.map((project) => {
    const list = byProject.get(project.id);
    if (!list) return project;
    const present = new Set(project.conversations.map((entry) => entry.id));
    const cards: PendingConversationCard[] = [];
    const topUps = new Map<string, PendingConversationCard["messages"]>();
    for (const item of list) {
      const card = pendingConversationCard(item, messagesFor(item.resolvedId || item.id));
      if (!present.has(card.id)) { cards.push(card); continue; }
      // 没有气泡要补就别登记：登记一个空数组会让下面"没什么可做"的短路失效，
      // 白白给每个项目造一批新对象（调用方拿它喂 useMemo）。
      if (card.messages.length > 0) topUps.set(card.id, card.messages);
    }
    if (cards.length === 0 && topUps.size === 0) return project;
    const conversations = project.conversations.map((entry) => {
      let next = entry;
      const extra = topUps.get(entry.id);
      if (extra && extra.length > 0) {
        // 去重按气泡 id：`loadSnapshot` 的对账也会补同一批（同一套 `pending-<requestId>` id），
        // 两边都补一次不许变成两条。
        const seen = new Set(entry.messages.map((message) => message.id));
        const additions = extra.filter((message) => !seen.has(message.id));
        if (additions.length > 0) next = { ...next, messages: [...next.messages, ...additions] };
      }
      // 只有"本地确实要顶一张卡上去"时才让位。没有卡还清 isCurrent，界面上就没有当前会话了。
      if (cards.length > 0 && next.isCurrent) next = { ...next, isCurrent: false };
      return next;
    });
    return { ...project, conversations: cards.length > 0 ? [...cards, ...conversations] : conversations };
  });
}

/**
 * 快照里哪条会话是这条待确认会话的真身。空串＝还没出现，继续等。
 *
 * 两条判据的优先级是有意的：
 *   ① 命令回执给了真 id → **必须**等它真的出现在快照里才认领。只凭 id 就换，
 *      那一帧快照里还没有这条会话，界面会闪成"别的会话"（或整块空掉）。
 *   ② 没拿到回执 → 项目里第一条"发起创建时不存在的"会话就是它。
 */
export function matchPendingConversation(
  pending: PendingConversation,
  conversations: { id: string }[] | undefined,
  claimedIDs: Iterable<string> = [],
): string {
  // 已经被**别的**待确认会话认下的 id 要排掉：同一个项目里同时新建两条会话时，两条的
  // 判据长得一模一样（都是"第一条以前没见过的会话"），不排掉就会双双认到同一条上，
  // 于是两条会话排队的消息全灌进其中一条，另一条永远空着。
  const claimed = new Set(claimedIDs);
  const list = (conversations || []).filter((entry) => !claimed.has(entry.id));
  if (pending.resolvedId) return list.some((entry) => entry.id === pending.resolvedId) ? pending.resolvedId : "";
  const known = new Set(pending.knownConversationIds);
  const candidate = list.find((entry) => !known.has(entry.id) && !isPendingConversationID(entry.id));
  return candidate?.id || "";
}
