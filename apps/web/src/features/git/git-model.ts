export type GitHead = { oid: string; branch: string; detached: boolean; upstream: string; ahead: number; behind: number };
export type GitWorktreeSummary = { staged: number; modified: number; untracked: number; deleted: number; renamed: number; conflicted: number };
export type GitSnapshot = { repositoryState: "ready"; head: GitHead; worktree: GitWorktreeSummary; observedAt?: string; stateToken?: string };
export type GitChange = { path: string; originalPath?: string; staged: boolean; modified: boolean; untracked: boolean; deleted: boolean; renamed: boolean; conflicted: boolean };
export type GitDiff = { path: string; stage: "worktree" | "index" | "commit"; content: string };
export type GitCommit = { oid: string; parents: string[]; subject: string; author: string; authoredAt: string };
export type GitCommitFile = { path: string; originalPath?: string; status: string; additions: number; deletions: number; binary: boolean };
export type GitCommitDetail = { oid: string; parents: string[]; author: string; authoredAt: string; committer: string; committedAt: string; subject: string; message: string; files: GitCommitFile[] };
export type GitBranch = { name: string; remote: boolean; current: boolean; upstream?: string };
export type GitOperation = { id: string; projectId: string; workspaceId?: string; workspacePath?: string; type: GitOperationType; status: GitOperationStatus; requestSummary: string; beforeState: string; afterState: string; errorCode?: string; errorMessage?: string; requestedAt: string; startedAt?: string; finishedAt?: string };
export type GitOperationType = "stage" | "unstage" | "stage_all" | "unstage_all" | "commit" | "commit_amend" | "discard_worktree" | "discard_all" | "fetch" | "pull" | "push" | "create_branch" | "switch_branch" | "resolve_conflict" | "conflict_abort" | "conflict_continue";
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
