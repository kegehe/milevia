export type GitHead = { oid: string; branch: string; detached: boolean; upstream: string; ahead: number; behind: number };
export type GitWorktreeSummary = { staged: number; modified: number; untracked: number; deleted: number; renamed: number; conflicted: number };
export type GitSnapshot = { repositoryState: "ready"; head: GitHead; worktree: GitWorktreeSummary; observedAt?: string; stateToken?: string;
  /**
   * 服务端算的"改动清单 + 每个文件指纹"的摘要（见 control-server 的 gitChangesRevision）。
   * 手机端那个只取 summary 的后台探针靠它判断清单有没有变 —— 只比计数会漏掉
   * "一个文件恢复干净、另一个同时被改"。旧版服务端没有这个字段，缺了就退回计数比对。
   */
  changesRevision?: string; changes?: GitChange[] };
export type GitChange = { path: string; originalPath?: string; staged: boolean; modified: boolean; untracked: boolean; deleted: boolean; renamed: boolean; conflicted: boolean };
export type GitDiff = { path: string; stage: "worktree" | "index" | "commit"; content: string };
export type GitCommit = { oid: string; parents: string[]; subject: string; author: string; authoredAt: string };
export type GitCommitFile = { path: string; originalPath?: string; status: string; additions: number; deletions: number; binary: boolean };
export type GitCommitDetail = { oid: string; parents: string[]; author: string; authoredAt: string; committer: string; committedAt: string; subject: string; message: string; files: GitCommitFile[] };
export type GitBranch = { name: string; remote: boolean; current: boolean; upstream?: string };
export type GitStash = { oid: string; ref: string; subject: string; createdAt: string };
export type GitOperation = { id: string; projectId: string; workspaceId?: string; workspacePath?: string; type: GitOperationType; status: GitOperationStatus; requestSummary: string; beforeState: string; afterState: string; errorCode?: string; errorMessage?: string; requestedAt: string; startedAt?: string; finishedAt?: string };
export type GitOperationType = "stage" | "unstage" | "stage_all" | "unstage_all" | "commit" | "commit_amend" | "discard_worktree" | "discard_all" | "fetch" | "pull" | "push" | "create_branch" | "switch_branch" | "stash_create" | "stash_apply" | "resolve_conflict" | "conflict_abort" | "conflict_continue";
export type GitOperationStatus = "queued" | "running" | "succeeded" | "failed" | "cancelled" | "needs_attention";

// —— Git 冲突解决相关类型（对应后端 git_conflicts.go 的 JSON）——
export type GitConflictOperationType = "merge" | "rebase" | "cherry-pick" | "revert" | "none";
export type GitConflictContext = { operationType: GitConflictOperationType; oursLabel: string; theirsLabel: string; canAbort: boolean; canFinish: boolean };
export type GitConflictFile = { path: string; kind: "content" | "add-add" | "modify-delete" | "delete-modify" | "both-deleted" | string; oursDeleted: boolean; theirsDeleted: boolean };
export type GitConflictOverview = { context: GitConflictContext; files: GitConflictFile[] };
export type GitConflictContent = {
  path: string;
  kind: string;
  binary: boolean;
  oversized: boolean;
  oursDeleted: boolean;
  theirsDeleted: boolean;
  base?: string;
  ours?: string;
  theirs?: string;
  working?: string;
  suggestionAgents?: { id: string; name: string }[];
};

export function groupChanges(changes: GitChange[]): { staged: GitChange[]; worktree: GitChange[] } {
  return {
    staged: changes.filter((change) => change.staged),
    worktree: changes.filter((change) => change.modified || change.untracked || change.deleted || change.renamed || change.conflicted),
  };
}

export function changeState(change: GitChange): string {
  if (change.conflicted) return "冲突";
  if (change.untracked) return "未跟踪";
  if (change.deleted) return "删除";
  if (change.renamed) return "重命名";
  return "修改";
}

export function commitFileStatusLabel(status: string): string {
  return ({ added: "新增", modified: "修改", deleted: "删除", renamed: "重命名", copied: "复制", typechanged: "类型变更" })[status] || status;
}

export function shortOID(oid: string): string { return oid.slice(0, 8); }

// 提交是否为合并提交（父提交多于一个）。
// 必须容忍 parents 缺失：根提交没有父提交，旧版服务端会把它序列化成 null，
// 直接读 .length 会抛错。本页没有错误边界，一次抛错会让整个工作台变空白。
export function isMergeCommit(commit: Pick<GitCommit, "parents">): boolean {
  return (commit.parents?.length ?? 0) > 1;
}

export function formatGitTime(value: string): string {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? "" : date.toLocaleString("zh-CN", { month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit" });
}

// —— 远端操作（获取 / 拉取 / 推送）的结果读数 ——
//
// 为什么单独立一组：这三类是**唯一一类"结果不在本机"的写操作** —— 暂存、提交改的是本地，
// 做完界面上一定看得见变化；获取/拉取/推送做完可能"什么都没变"（远端本来就没更新，
// 或者推上去的就是刚才那份）。不给它们结果读数，用户就只能靠"点完有没有反应"猜，
// 而"没反应"和"失败了"在界面上长得一模一样。
//
// 三条判据：
//  1. **成功也要说话**。远端操作成功的信号常常是"零变化"，不说等于没做。
//  2. **数量是数出来的，不是编的**。推送/拉取/获取各有一份可比的前后读数
//     （ahead/behind），用它算出"推了几个 / 拉了几个 / 远端多了几个"。
//  3. **失败要能落地成一句话**。服务端只给得出结论（"推送被拒绝"），
//     前端补上"推给谁、推了什么"，用户才知道下一步该做什么。

/** 远端操作的三种。fetch 不带分支（它取的是整个远端的引用），pull/push 带。 */
export type GitRemoteOpKind = "fetch" | "pull" | "push";

/** 进行中：说清楚"正在对哪个远端做什么"，否则用户分不清是在等网络还是界面卡住了。 */
export function remoteOpRunningText(kind: GitRemoteOpKind, remote: string, branch: string): string {
  if (kind === "fetch") return `正在从 ${remote} 获取远端更新…`;
  if (kind === "pull") return `正在从 ${remote} 拉取 ${branch}…`;
  return `正在推送到 ${remote}/${branch}…`;
}

/** 失败的标题：短到能当标题，详细原因另开一行。 */
export function remoteOpFailureTitle(kind: GitRemoteOpKind): string {
  if (kind === "fetch") return "获取失败";
  if (kind === "pull") return "拉取失败";
  return "推送失败";
}

/**
 * 成功后的读数 —— **数出来的**，不是"成功了就报一句成功"。
 *
 * 三种操作各有一对可比的读数：
 *
 *  · push：推之前本地领先上游 `ahead` 个，推之后应当变成 0 ⇒ 推掉的就是 `ahead` 减去的那个差。
 *  · pull：拉之前本地落后上游 `behind` 个，快进合并之后应当变成 0 ⇒ 拉到的就是它。
 *  · fetch：fetch 不动本地分支，但会更新远端引用 ⇒ 上游的 `behind` 变大多少，
 *    就是远端前进了多少（"远端有 3 个新提交待拉取"）。
 *
 * `after` 传 null（还没对账完）时只给"做完了"这个最小读数 —— 反馈必须**先行**，
 * 不能等对账回来才说话；随后对账落地再用真数字把它补准。
 */
export function remoteOpSuccessText(kind: GitRemoteOpKind, remote: string, branch: string, before: GitSnapshot | null, after: GitSnapshot | null): string {
  if (kind === "push") {
    const pushed = before && after ? Math.max(0, before.head.ahead - after.head.ahead) : 0;
    return pushed > 0 ? `已推送 ${pushed} 个提交到 ${remote}/${branch}` : `已推送到 ${remote}/${branch}`;
  }
  if (kind === "pull") {
    const merged = before ? Math.max(0, before.head.behind) : 0;
    // 拉完之后仍然落后：说明远端在这期间又动了（少见，但真会出现），别报"已是最新"。
    if (after && after.head.behind > 0) return `已拉取，但仍有 ${after.head.behind} 个提交未合入`;
    return merged > 0 ? `已拉取 ${merged} 个提交（${remote}/${branch}）` : `已是最新（${remote}/${branch}）`;
  }
  // ⚠️ **"还没数出来"不等于"没有"**。对账落地前（after 为 null）我们并不知道远端有没有前进，
  // 那一刻若直接说"当前分支没有新提交"，就是把"还不知道"写成了"没有"——
  // 紧接着对账回来它又变成"有 2 个新提交"，用户会看到一句话自己改口。
  if (!before || !after) return `获取完成：已更新 ${remote} 的远端引用`;
  const gained = Math.max(0, after.head.behind - before.head.behind);
  return gained > 0 ? `获取完成：${remote} 上有 ${gained} 个新提交` : "获取完成：当前分支没有新提交";
}
// —— 「提交并推送」的两段复合操作 ——
//
// 为什么单独立一组：提交和推送是**两件独立的事**，被同一个按钮串在一起，
// 但两段失败的后果完全不同，把它们混成一句话读数就会骗人：
//
//  · commit 段失败 = 本地什么都没发生，用户改好提交信息可以直接重试；
//  · push 段失败   = 提交**已经在本地了**，说成「什么都没发生」会让人重复提交。
//
// 所以读数必须**按段分开**，并且在 push 段失败时明确告诉用户「别再提交一次」。

/** 「提交并推送」的两段。 */
export type GitCommitPushPhase = "commit" | "push";

/**
 * 「提交并推送」的落点：推给哪个远端、哪个分支，要不要顺带建立上游跟踪。
 *
 * 上游已经存在时推的是它（远端名从 upstream 里切出来），没有上游时推
 * `origin/<当前分支>` 并建立跟踪 —— 首次推送就是这么走的。
 */
export type CommitPushTarget = { remote: string; branch: string; setUpstream: boolean };

/**
 * 推送落点的推导 —— **工具栏那个「推送」按钮与提交区的「提交并推送」共用它**。
 *
 * 两处各算一次，早晚会对不上（上游从 origin 换到别的远端时）。
 *
 * ⚠️ **远端名从 upstream 的第一段切**（`origin/main` → `origin`），
 * 不是从 upstream 里找最后一个 `/`：分支名里可以带 `/`（`origin/feature/x`），
 * 按最后一个切会切出 `x` 当远端名。
 */
export function gitPushTarget(snapshot: GitSnapshot | null): CommitPushTarget {
  const upstream = snapshot?.head.upstream || "";
  const separator = upstream.indexOf("/");
  const remote = separator > 0 ? upstream.slice(0, separator) : "origin";
  return {
    remote,
    branch: snapshot?.head.branch || "",
    setUpstream: !upstream,
  };
}

/**
 * 「提交并推送」这个入口的全部状态：推去哪、现在为什么推不了。
 *
 * 为什么要打包在一起算：**落点与能不能推是同一件事的两面**，
 * 分开算就会出现"推 origin/main"配一句"游离指针无法推送"这种自相矛盾的界面。
 */
export type CommitPushOption = { target: CommitPushTarget; detached: boolean };

/** 从仓库状态算出「提交并推送」的落点与是否游离指针。 */
export function gitCommitPushOption(snapshot: GitSnapshot | null): CommitPushOption {
  return { target: gitPushTarget(snapshot), detached: snapshot?.head.detached ?? false };
}

/** 下拉项的标题：有落点就写清楚推去哪，没有就只能写「提交并推送」。 */
export function commitPushMenuLabel(target: CommitPushTarget | null): string {
  return target && target.branch ? `提交并推送到 ${target.remote}/${target.branch}` : "提交并推送";
}

/** 下拉项的副标题：说清这是**两步**合成的一次操作。 */
export function commitPushMenuHint(target: CommitPushTarget | null): string {
  if (!target) return "";
  return target.setUpstream
    ? "先创建本地提交，再推送到远端并建立上游跟踪"
    : "先创建本地提交，再推送到远端";
}

/**
 * 「提交并推送」现在做不了的原因（**空串 = 可以做**）。
 *
 * 为什么把理由做成一个函数而不是散在组件里：这个入口有三组各自独立的
 * 前置条件（要暂存、要写信息、要能推 —— 最后一组再拆成"游离指针"与"没有分支名"），
 * 任何一条不满足都只该告诉用户**那一条**。合成一句"暂存并填写提交信息后即可"
 * 是在猜用户缺哪一样。
 *
 * 「不能忙」这一条**不在这里判**：工作台忙时整个触发键都会禁用
 * （CommitPanel 的 buttonDisabled），菜单根本打不开，轮不到说理由。
 */
export function commitPushUnavailableReason(input: {
  stagedCount: number;
  message: string;
  subjectOverLimit: boolean;
  detached: boolean;
  hasBranch: boolean;
}): string {
  if (input.stagedCount === 0) return "没有已暂存文件可提交";
  if (!input.message.trim()) return "请先填写提交信息";
  if (input.subjectOverLimit) return "提交信息首行过长，无法提交";
  // 游离指针与"没有分支名"是两回事：前者是 HEAD 不在分支上，后者是分支名读不到。
  // 说错会让用户去查错的东西（工具栏那个「推送」按钮用的是"游离指针"这个词）。
  if (input.detached) return "游离指针状态无法推送";
  if (!input.hasBranch) return "当前没有可推送的分支";
  return "";
}

/**
 * 结局有两种，区别很大：
 *
 *  · failed  = 明确失败（服务端说没成）。
 *  · unknown = **结果无法确认**（服务端说"可能做完了，但我没法核实"）。
 *
 * unknown 时不能说"什么都没发生" —— 那是把"不知道"写成"没有"。
 * 所以这两种结局必须分开说。
 */
export type GitCommitPushOutcome = "failed" | "unknown";

/** 读数条与确认框里的操作标识：三种远端操作，外加「提交并推送」这个两段复合操作。 */
export type GitFeedbackOp = GitRemoteOpKind | "commit-push";

/** 进行中：说清楚现在是在本地提交，还是在跟远端通信。 */
export function commitPushRunningText(phase: GitCommitPushPhase, remote: string, branch: string): string {
  if (phase === "commit") return "正在创建本地提交…";
  return `正在推送到 ${remote}/${branch}…`;
}

/** 失败的标题：短到能当标题，详细原因另开一行。 */
export function commitPushFailureTitle(phase: GitCommitPushPhase, outcome: GitCommitPushOutcome): string {
  if (outcome === "unknown") return phase === "commit" ? "提交结果未确认" : "推送结果未确认";
  return phase === "commit" ? "提交失败" : "推送失败";
}

/**
 * 失败的详细说明 —— **把"现在处于什么状态"说清楚**。
 *
 * 服务端只给得出结论（"提交信息为空"、"推送被拒绝"），前端负责补上"所以现在该干嘛"：
 * commit 段失败说"本地什么都没发生"，push 段失败说"提交已经在了，别重复提交"。
 *
 * ⚠️ outcome=unknown（服务端无法核实结果）时**不能**说"什么都没发生"：
 * 提交可能其实建成了、推送可能其实推上去了 —— 说"没有"会让用户重复提交，
 * 而那正是这个功能最不能出的错。所以那一档只说"无法确认，请刷新后核对"。
 */
export function commitPushFailureDetail(phase: GitCommitPushPhase, outcome: GitCommitPushOutcome, message: string): string {
  // 服务端给的句子可能自带句末标点，接后半句时先去掉 —— 否则会出现「……推送。。提交已在……」。
  const text = message.trim().replace(/[。.．!?！？\s]+$/, "");
  if (outcome === "unknown") {
    return phase === "commit"
      ? `${text}。提交是否落地无法确认，请刷新仓库核对历史后再决定是否重试`
      : `${text}。提交已在本地创建，推送是否成功无法确认，请刷新后核对远端分支，不要重复提交`;
  }
  return phase === "commit"
    ? `${text}。本地没有产生任何提交，可以直接重试`
    : `${text}。提交已在本地创建，不要重复提交；需要推送时用工具栏的「推送」`;
}

/**
 * 成功后的读数 —— 推送了几个提交是**数出来的**。
 *
 * ⚠️ 这个数字是**推上去的提交总数**，不是"这次新建了几个提交"。
 * 本次操作只新建了**一个**提交；但如果本地本来就领先上游若干个（此前没推），
 * 那次推送会把它们**一起**送上去 —— 所以数字可能大于 1。
 *
 * 因此措辞把两件事分开说（"已提交" / "已推送 N 个提交"），不能让 N 读起来
 * 像"你新建了 N 个提交"。
 */
export function commitPushSuccessText(remote: string, branch: string, pushed: number): string {
  return pushed > 0 ? `已提交，已推送 ${pushed} 个提交到 ${remote}/${branch}` : `已提交，已推送到 ${remote}/${branch}`;
}

/**
 * 这次到底推了几个提交：**推送前**的 ahead 减去**推送后**的 ahead。
 *
 * 判据与 `remoteOpSuccessText("push")` 完全一致（同一对读数、同一份算法）——
 * "推了几个"永远是数出来的，不是拿"提交了一个"推算的。
 *
 * ⚠️ **没有上游时数不出来**（首次推送）：本地有多少个提交从来就没有"领先上游"这个说法
 * （`branch.ab` 那行只在有上游时才出现，ahead 恒为 0）。这时候报"推了 1 个"是**编的** ——
 * 实际推上去的是整个分支。所以这种情况返回 0，只说"推上去了"，不说推了几个。
 *
 * 对不上时（例如期间远端又动了）同样退回到 0，宁可少报也不要报错数。
 *
 * @param before 推送**前**的快照（提交已经完成、推送还没开始那一刻）
 * @param after  推送**后**的快照
 */
export function commitPushPushedCount(before: GitSnapshot | null, after: GitSnapshot | null): number {
  if (!before || !after) return 0;
  if (!before.head.upstream) return 0;
  return Math.max(0, before.head.ahead - after.head.ahead);
}

