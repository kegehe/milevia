export type GitHead = { oid: string; branch: string; detached: boolean; upstream: string; ahead: number; behind: number };
export type GitWorktreeSummary = { staged: number; modified: number; untracked: number; deleted: number; renamed: number; conflicted: number };
export type GitSnapshot = { repositoryState: "ready"; head: GitHead; worktree: GitWorktreeSummary; observedAt?: string; stateToken?: string };
export type GitChange = { path: string; originalPath?: string; staged: boolean; modified: boolean; untracked: boolean; deleted: boolean; renamed: boolean; conflicted: boolean };
export type GitDiff = { path: string; stage: "worktree" | "index"; content: string };
export type GitCommit = { oid: string; parents: string[]; subject: string; author: string; authoredAt: string };
export type GitBranch = { name: string; remote: boolean; current: boolean; upstream?: string };
export type GitOperation = { id: string; projectId: string; type: GitOperationType; status: GitOperationStatus; requestSummary: string; beforeState: string; afterState: string; errorCode?: string; errorMessage?: string; requestedAt: string; startedAt?: string; finishedAt?: string };
export type GitOperationType = "stage" | "unstage" | "stage_all" | "unstage_all" | "commit" | "commit_amend" | "discard_worktree" | "discard_all" | "fetch" | "pull" | "push" | "create_branch" | "switch_branch";
export type GitOperationStatus = "queued" | "running" | "succeeded" | "failed" | "cancelled" | "needs_attention";

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

export function shortOID(oid: string): string { return oid.slice(0, 8); }

export function formatGitTime(value: string): string {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? "" : date.toLocaleString("zh-CN", { month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit" });
}
