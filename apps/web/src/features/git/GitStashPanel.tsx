import { formatGitTime, type GitStash } from "./git-model";

export function GitStashPanel({ stashes, busy, dirty, blocked, hasHead, requestCreate, requestApply }: {
  stashes: GitStash[] | null;
  busy: boolean;
  dirty: boolean;
  blocked: boolean;
  hasHead: boolean;
  requestCreate: () => void;
  requestApply: (stash: GitStash) => void;
}) {
  const unavailableReason = !hasHead ? "创建首次提交后才能使用 Stash" : blocked ? "请先完成当前 Git 操作并解决冲突" : "";
  return <section className="git-change-group git-stash-panel" aria-label="收起的改动（Stash）">
    <header><h3>收起的改动（Stash）</h3><button type="button" className="secondary" disabled={busy || !dirty || Boolean(unavailableReason)} title={unavailableReason || "保存所有未提交改动和未跟踪文件，清空工作区"} onClick={requestCreate}>收起改动</button></header>
    <p className="git-stash-hint">临时保存改动后可切换分支或移除工作区。备份由同一仓库的工作区共享，恢复后仍保留。</p>
    {stashes === null ? <p className="git-stash-hint" role="status">无法读取 Stash 列表，请点击顶部刷新重试。</p>
      : stashes.length === 0 ? <p className="git-stash-hint">没有收起的改动</p>
        : <ul className="git-stash-list">{stashes.map((stash) => <li key={`${stash.ref}:${stash.oid}`}>
          <div><b title={stash.subject}>{stash.subject}</b><small>{stash.ref} · {formatGitTime(stash.createdAt)}</small></div>
          <button type="button" className="secondary" disabled={busy || dirty || Boolean(unavailableReason)} title={unavailableReason || (dirty ? "请先提交、收起或丢弃当前改动，再恢复 Stash" : "恢复改动，可选择是否还原暂存状态，并保留 Stash 备份")} onClick={() => requestApply(stash)}>恢复</button>
        </li>)}</ul>}
  </section>;
}
