import { forwardRef, useCallback, useEffect, useImperativeHandle, useMemo, useRef, useState, type KeyboardEvent, type ReactNode } from "react";

import { toast } from "sonner";

import { changeState, commitFileStatusLabel, formatGitTime, groupChanges, isMergeCommit, shortOID, type GitBranch, type GitChange, type GitCommit, type GitCommitDetail, type GitCommitFile, type GitConflictOverview, type GitDiff, type GitOperation, type GitSnapshot } from "./git-model";
import { ConflictSolveView } from "./ConflictSolveView";

type Request = <T>(path: string, init?: RequestInit) => Promise<T>;
type Tab = "changes" | "branches";
type ResolveAction = "ours" | "theirs" | "delete" | "working";

/**
 * 为什么要重读仓库状态 —— **它决定界面做什么、以及取哪几样**，不只是个标签。
 *
 *  - `initial`   首屏：占位（骨架），取全套。
 *  - `manual`    用户点了刷新：转圈，取全套（用户要的就是"全部给我最新的"）。
 *  - `reconcile` 刚做完一次写操作**或知道仓库可能变了**：不置任何 loading，
 *                在位的内容一步都不动。
 *  - `probe`     事件驱动的后台对账：**先只问一句 summary**（353 字节），
 *                和手上这份一模一样就到此为止。
 *
 * 手机端每次中继往返的固定成本约 300ms（实测：本机到云端 RTT ~150ms，
 * 一个来回就是两次），而 Git 页在 AI 跑动时会反复对账 —— 所以"少发一次"与
 * "不发"是两件事，`probe` 存在的理由就是后者。
 */
type ReloadReason = "initial" | "manual" | "reconcile" | "probe";
interface ReloadOptions {
  /** 刚做过分支操作（切分支 / 新建）：分支列表必须一起对账。 */
  branches?: boolean;
}

type GitOperationResult = { operationId: string; status: "succeeded" | "failed" | "needs_attention"; errorMessage?: string };
type DiffLine = { content: string; kind: "added" | "removed" | "context" | "hunk" | "meta"; oldLine?: number; newLine?: number };
type Confirmation =
  | { type: "commit" }
  | { type: "amend" }
  | { type: "discard-worktree"; path: string; untracked: boolean }
  | { type: "discard-all" }
  | { type: "fetch"; remote: string }
  | { type: "pull"; remote: string; branch: string }
  | { type: "push"; remote: string; branch: string; setUpstream: boolean }
  | { type: "switch-branch"; branch: string }
  | { type: "conflict-abort" }
  | { type: "conflict-finish" };

// 提交历史与操作记录的分页每页条数。
const HISTORY_PAGE_SIZE = 50;
const OPS_PAGE_SIZE = 50;
// 操作记录的筛选选项（types 与 git-model 的 GitOperationType 对齐）。
const OPERATION_TYPE_VALUES = ["stage", "unstage", "stage_all", "unstage_all", "commit", "commit_amend", "discard_worktree", "discard_all", "fetch", "pull", "push", "create_branch", "switch_branch", "resolve_conflict", "conflict_abort", "conflict_continue"];
const OPERATION_STATUS_VALUES = ["queued", "running", "succeeded", "failed", "cancelled", "needs_attention"];

const tabs: { id: Tab; label: string }[] = [
  { id: "changes", label: "变更" },
  { id: "branches", label: "分支" },
];

function gitPullTarget(snapshot: GitSnapshot | null): { remote: string; branch: string } {
  const upstream = snapshot?.head.upstream || "";
  const separator = upstream.indexOf("/");
  if (separator > 0 && separator < upstream.length - 1) {
    return { remote: upstream.slice(0, separator), branch: upstream.slice(separator + 1) };
  }
  return { remote: "origin", branch: snapshot?.head.branch || "" };
}

/**
 * 两份 summary 说的是不是同一份仓库状态。
 *
 * 判据有两层，**优先用服务端给的清单摘要**（`changesRevision`，它把改动清单与每个文件的
 * 指纹一起算进去了），没有它才退回比 `head` 与 `worktree` 计数。
 *
 * 为什么不能只看计数（第一版就是这么写的，注释里还振振有词地说"计数一样就不会有多值得看的
 * 差别"，那句是错的）：**一个文件恢复干净、另一个文件同时被改**，计数一模一样，清单却换了人。
 * 症状是探针每次判"没变"、永远早退 —— 手机端一直显示那份过期清单，用户对着一条已经恢复干净的
 * 行点暂存，服务端按 stateToken 判"仓库状态已变化"，而真正被改的文件根本不在列表里。
 *
 * 比对**不认** `observedAt`（每次都是新时间戳）与 `stateToken`（服务端每次现签的随机 UUID，
 * 跟内容无关）—— 带上它们任何一个，这个比较就永远为假，probe 等于没写。
 */
function sameGitSnapshot(a: GitSnapshot | null, b: GitSnapshot | null): boolean {
  if (!a || !b) return false;
  // 旧版服务端没有 `changesRevision`：两边都有才用它，缺了退回计数比对（= 旧行为，不更差）。
  if (a.changesRevision !== undefined && b.changesRevision !== undefined && a.changesRevision !== b.changesRevision) return false;
  return a.head.oid === b.head.oid
    && a.head.branch === b.head.branch
    && a.head.upstream === b.head.upstream
    && a.head.ahead === b.head.ahead
    && a.head.behind === b.head.behind
    && a.worktree.staged === b.worktree.staged
    && a.worktree.modified === b.worktree.modified
    && a.worktree.untracked === b.worktree.untracked
    && a.worktree.deleted === b.worktree.deleted
    && a.worktree.renamed === b.worktree.renamed
    && a.worktree.conflicted === b.worktree.conflicted;
}


/** 手机端的返回键要问"里面还有没有上一层"，而那一层只有这里知道。 */
export interface GitWorkbenchHandle {
  /**
   * 退出详情层（diff / 冲突解决）回到列表。返回 false 表示已经在最外层，
   * 宿主该退出整个 Git 视图了。
   */
  showTopLevel: () => boolean;
  /**
   * 重新读一遍仓库状态。
   *
   * 手机端顶栏 ⋯ 菜单里的「刷新仓库状态」走它，而不是让宿主去重取云端快照 ——
   * 那两件事语义完全不同（一个是"再读一眼仓库"，另一个是"重新同步整个页面"）。
   */
  reload: () => void;
  /**
   * 后台对账：**不置任何 loading、不动在位的内容**，先只问一句 summary，
   * 没变化就到此为止。
   *
   * 手机端在收到结构事件（AI 的工具调用、运行起止）时节流调用它 ——
   * "停在 Git 页上自动看到电脑那边的变化"靠这条，而不是靠定时轮询：
   * 没有事件就没有一次请求，有事件也只在**真的变了**时才取第二趟。
   */
  refreshInBackground: () => void;
}

interface GitWorkbenchProps {
  projectID: string;
  conversationId?: string;
  request: Request;
  fail: (message: string) => void;
  active: boolean;
  /**
   * 手机端形态：变更列表与 diff **两级推进**（桌面端是两栏并排），
   * 操作记录的原生 `<select>` 换成胶囊，diff 软换行。
   *
   * 与 `FilesPanel` 的 `mobile` 同构 —— 那一支的实际体量只有"一个 prop + 一批 CSS"，
   * 见 docs/41 §6.2。
   */
  mobile?: boolean;
  /**
   * 项目当前是不是 git 仓库（桌面端由 project.gitBranch 判定）。
   *
   * 非 git 项目也能进入 Git 工作台：为 false 时渲染「初始化 Git 仓库」空态，
   * 不再尝试读取仓库状态（后端 git 接口对非 git 目录会失败）。
   */
  initialIsGitRepo?: boolean;
  /** git init 成功后由宿主刷新项目（更新 gitBranch），默认空操作。 */
  onGitInitialized?: () => void | Promise<void>;
}

export const GitWorkbench = forwardRef<GitWorkbenchHandle, GitWorkbenchProps>(function GitWorkbench({ projectID, conversationId, request, fail, active, mobile = false, initialIsGitRepo = true, onGitInitialized }, ref) {
  const [tab, setTab] = useState<Tab>("changes");
  const [snapshot, setSnapshot] = useState<GitSnapshot | null>(null);
  // 非 git 项目一开始就是 false；git init 成功后置 true 并正常加载仓库。
  const [isGitRepo, setIsGitRepo] = useState(initialIsGitRepo);
  const [initializingRepo, setInitializingRepo] = useState(false);
  const [changes, setChanges] = useState<GitChange[]>([]);
  const [branches, setBranches] = useState<GitBranch[]>([]);
  const [operations, setOperations] = useState<GitOperation[]>([]);
  const [conflictOverview, setConflictOverview] = useState<GitConflictOverview | null>(null);
  // 冲突总览的**第三档：读不到**。没有它，"这次没读到"会被渲染成"没有冲突" ——
  // 那正是本项目明令禁止的"把读不到写成没有"（详见 reload 里的说明）。
  const [conflictsState, setConflictsState] = useState<"loading" | "ready" | "unavailable">("loading");
  const [selectedDiff, setSelectedDiff] = useState<GitDiff | null>(null);
  const [conflictPath, setConflictPath] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [mutating, setMutating] = useState("");
  const [commitMessage, setCommitMessage] = useState("");
  const [confirmation, setConfirmation] = useState<Confirmation | null>(null);
  const diffRequest = useRef(0);
  const reloadRequest = useRef(0);
  // 刷新出口。`reload` 在下面才定义（它依赖若干个 state 回调），而 ref 句柄要能调它，
  // 所以中间隔一层 ref —— 直接引用会撞上"用到未初始化的 const"。
  const reloadRef = useRef<(reason?: ReloadReason, options?: ReloadOptions) => Promise<void>>(async () => undefined);
  const conflictPathRef = useRef<string | null>(null);
  const mountedRef = useRef(true);
  // 下面这几个 ref 都是"`reload` 里的判断要读的当前值"，一律**不进 `reload` 的依赖**：
  // 它们一变（切个 tab、项目变回非 git…）就会重建 `reload`，而 `reload` 是装载 effect 的
  // 依赖项 —— 那等于又给"用户随手切一下整个仓库就重读一遍"开了条口子，与上面 fail 那条同理。
  const snapshotRef = useRef<GitSnapshot | null>(null);           // 手上那份已落地的 summary
  const tabRef = useRef<Tab>("changes");
  const isGitRepoRef = useRef(initialIsGitRepo);
  const reloadInFlightRef = useRef(false);                        // 有没有一趟重读在飞
  // 被让路的 probe（见 reload 开头）：宿主那边是"首事件开窗"的节流，窗口内的事件不会再排
  // 第二次 —— 丢掉一次就等于"这次变化永远不出现"，直到用户手动刷新。所以是**推迟**不是丢弃。
  const probePendingRef = useRef(false);
  const conflictRequest = useRef(0);                              // 冲突总览自己的作废号
  useEffect(() => { tabRef.current = tab; }, [tab]);
  useEffect(() => { isGitRepoRef.current = isGitRepo; }, [isGitRepo]);
  // ⚠️ `isGitRepo` 是 `useState(initialIsGitRepo)` 锁存的，而**没有**这条同步的话，
  // prop 后来变成"是仓库了"也进不来：手机端会永远停在「未初始化 Git 仓库」空态上，
  // 连 ⋯ 里的「刷新仓库状态」都救不了（它读回来的是仓库内容，但渲染在 `if (!isGitRepo)`
  // 那一步就被空态挡掉了）—— 唯一出路是退出视图再进来。而空态那句文案恰恰是让用户
  // "去电脑上初始化再回来"，于是它把人送进一个回不来的地方。
  // 单向即可：prop 只会告诉我们"它现在是仓库了"（非 git 的判定由页面按 gitBranch 给出，
  // 这里不需要反向把 true 改回 false —— 那会让 git init 刚成功的工作台瞬间塌回空态）。
  useEffect(() => { if (initialIsGitRepo) setIsGitRepo(true); }, [initialIsGitRepo]);
  // 错误落点只走 ref，**不进 `reload` 的依赖**。
  //
  // `reload` 是下面那个"装载 / 刷新" effect 的依赖项，而 `fail` 是宿主传进来的回调
  // （签名 `(message: string) => void`，宿主完全可以正当地写成行内闭包 —— 手机端的
  // `MobileGitPanel` 当初就是那么写的）。把它的身份放进依赖，等于规定"宿主每重渲染一次，
  // 整个仓库就重读一遍"，而重读会 `setLoading(true)` ⇒ 界面闪回「正在读取仓库状态」。
  // 手机端宿主每秒重渲染若干次（`assistant.delta` 每个流式片段一次 `setSnapshot`、
  // `loadInstances` 每 5 秒一次），于是这个工作台常亮地闪。
  // 复现与量化见 `.tmp/mobile-git-refresh`（修复前：每次重渲染 5 个中继请求 + 1 次闪烁）。
  const failRef = useRef(fail);
  useEffect(() => { failRef.current = fail; }, [fail]);
  const withWorkspace = (path: string) => `${path}${path.includes("?") ? "&" : "?"}${conversationId ? `conversationId=${encodeURIComponent(conversationId)}` : ""}`;
  useEffect(() => {
    mountedRef.current = true;
    return () => { mountedRef.current = false; };
  }, []);

  const closeDiff = () => { diffRequest.current++; setSelectedDiff(null); };
  const closeConflict = () => { conflictPathRef.current = null; setConflictPath(null); };
  const openConflict = (path: string) => { diffRequest.current++; setSelectedDiff(null); conflictPathRef.current = path; setConflictPath(path); };
  const selectTab = (next: Tab) => {
    setTab(next);
    closeDiff();
    closeConflict();
    // 切到「分支」时才补一次分支列表：AI 在电脑上换过分支的话，用户在「变更」tab 上
    // 停留期间那些后台对账都按"没在看分支"跳过了它，不补就会拿着一份很旧的列表
    // （而且没有任何东西会去纠正它）。带上 branches 明确要它，不靠 tabRef
    // —— 那一刻 tabRef 还没同步到新值。
    if (next === "branches") void reloadRef.current("reconcile", { branches: true });
  };
  const handleTabKeyDown = (event: KeyboardEvent<HTMLButtonElement>, current: Tab) => {
    const currentIndex = tabs.findIndex((item) => item.id === current);
    const targetIndex = event.key === "ArrowRight" ? (currentIndex + 1) % tabs.length
      : event.key === "ArrowLeft" ? (currentIndex - 1 + tabs.length) % tabs.length
        : event.key === "Home" ? 0 : event.key === "End" ? tabs.length - 1 : -1;
    if (targetIndex < 0) return;
    event.preventDefault();
    const next = tabs[targetIndex].id;
    selectTab(next);
    requestAnimationFrame(() => document.getElementById(`git-tab-${next}`)?.focus());
  };

  // 手机端「列表 ⇄ 详情」那一层是**派生**的，不是独立状态：详情有没有开着完全由
  // selectedDiff / conflictPath 决定。独立记一个布尔必然有一天与它们不同步，
  // 而症状是"返回键吃掉一次、界面上什么都不发生"（第 40 篇踩过同一个形状）。
  const mobileDetailOpen = selectedDiff !== null || conflictPath !== null;
  useImperativeHandle(ref, () => ({
    showTopLevel: () => {
      if (conflictPathRef.current) { closeConflict(); return true; }
      if (selectedDiff) { closeDiff(); return true; }
      return false;
    },
    reload: () => { void reloadRef.current("manual"); },
    refreshInBackground: () => { void reloadRef.current("probe"); },
  }), [selectedDiff]);

  const reload = useCallback(async (reason: ReloadReason = "initial", options?: ReloadOptions) => {
    if (!mountedRef.current) return;
    // probe 是**唯一一个"用户没要求"的档位**（宿主收到结构事件就调它），所以它要多两道闸：
    //
    //  · 非 git 项目：工作台在空态里从不读仓库（后端对非 git 目录会失败）。放它过去，
    //    用户停在「未初始化 Git 仓库」上会莫名收到一条红色错误条 —— 而那件事他没做、
    //    也不知道该做什么。手动刷新不走这道闸：那是用户按的，出错就该当场说。
    //  · 已经有一趟重读在飞：让给它。这不是省一次请求，是**防作废** —— `++reloadRequest`
    //    就是在飞那一趟的作废开关，而 probe 领号前还要先问一句话（几百毫秒），这段时间里
    //    完全可能有人开始整份读。probe 抢到号，那一趟的结果（尤其 branches / operations）
    //    就再也写不进去，症状是「分支」tab 显示成空列表 —— 正是"把读不到写成没有"。
    if (reason === "probe" && !isGitRepoRef.current) return;
    if (reason === "probe" && reloadInFlightRef.current) { probePendingRef.current = true; return; }
    // 「手上还没有一份完整读数」时，**任何档位都按整份读**：首屏失败之后靠一次事件驱动的
    // probe 恢复时最要紧 —— 那一档本来只取 summary + changes，分支与操作记录就永远补不上。
    // 判据是"有没有落地过一份完整读数"，不是"这是第几次调用"。
    const full = reason === "initial" || reason === "manual" || snapshotRef.current === null;
    const base = `/api/projects/${projectID}/git`;
    let requestID = 0;
    try {
      // ① `probe` 先只问一句 summary（353 字节）：它跟手上这份一模一样就到此为止，
      //    没有理由去取 10KB 的变更列表和另外两样。**只有 probe 走这条串行路** ——
      //    它是后台对账，多一趟往返没人等；其余档位把 summary 与另外三样并发发出去
      //    （见下面的 Promise.all），否则"先等 summary 再取列表"会白白多出一整趟中继往返。
      //    比对用什么（`changesRevision` 优先、退回 head 与 worktree 计数）见 sameGitSnapshot 的说明。
      let probeSnapshot: GitSnapshot | null = null;
      if (reason === "probe") {
        probeSnapshot = await request<GitSnapshot>(withWorkspace(`${base}/summary`));
        if (!mountedRef.current) return;
        if (sameGitSnapshot(probeSnapshot, snapshotRef.current)) return;
        // 问这一句话的工夫里又有人开始重读了：它的数据比这句 summary 新，让给它 —— 但要记账，
        // 等那一趟落地后补做（见 probePendingRef）。
        if (reloadInFlightRef.current) { probePendingRef.current = true; return; }
      }

      // ⚠️ 号**在这一步才领**。上面两条早退都不动 `reloadRequest` —— 一个只问了一句话、
      // 什么都没改的 probe，不该有权把别人在飞的结果作废。
      requestID = ++reloadRequest.current;
      reloadInFlightRef.current = true;
      if (reason === "initial") setLoading(true);
      if (reason === "manual") setRefreshing(true);
      // reconcile / probe **什么都不置**。在位的内容一步都不动 —— 这正是"停在页面上自动
      // 保持最新"与"一直闪"之间唯一的区别（首屏那句「正在读取仓库状态」就是那一闪，
      // 见下面 render 里的 loading 分支）。任何"后台刷新也要转个圈"的想法都会把它带回来。
      const [nextSnapshot, nextChanges, nextBranches, nextOperations] = await Promise.all([
        probeSnapshot ? Promise.resolve(probeSnapshot) : request<GitSnapshot>(withWorkspace(`${base}/summary`)),
        request<GitChange[]>(withWorkspace(`${base}/changes`)),
        // 分支列表只在四种情况下要：首屏/手动刷新、用户正看着「分支」tab、刚做过分支操作、
        // 或刚切到那个 tab（`selectTab` 会把 `branches: true` 明确传进来 —— 切 tab 那一刻
        // `tabRef` 还没同步到新值，靠它自己判会漏）。
        // 其余档位不取 —— 它不会因为 AI 改了几个文件而变，而 probe 的间隔只有几秒。
        full || options?.branches === true || tabRef.current === "branches"
          ? request<GitBranch[]>(withWorkspace(`${base}/branches`))
          : null,
        // 操作记录（7KB，是后台那一组里最大的一份）只服务 GitBar 上那个红点：
        // 用户自己那次操作的失败由 fail() 直接说给用户听，不靠红点兜底；
        // 操作记录弹层打开时本来就会自己查一页。
        // **写后对账照取**：那正是刚刚多出一条审计记录的时刻（红点该亮的也是那一刻），
        // 而且它由用户动作触发、频率低。**probe 不取** —— 它一次可能只有几百毫秒的间隔。
        full || reason === "reconcile" ? request<GitOperation[]>(withWorkspace(`${base}/operations`)) : null,
      ]);
      if (!mountedRef.current || requestID !== reloadRequest.current) return;
      snapshotRef.current = nextSnapshot;
      setSnapshot(nextSnapshot);
      setChanges(nextChanges);
      if (nextBranches) setBranches(nextBranches);
      if (nextOperations) setOperations(nextOperations);
      // 主体已经落地：左栏可以显示了。**手动刷新的转圈也到此为止** ——
      // 下面那份冲突总览是这一组里最慢的一条，不该让"刷新"按钮跟着它多转半秒。
      setLoading(false);
      setRefreshing(false);
      void loadConflicts();
    } catch (cause) {
      // **整条 probe 链静默**（不只是它第一句话）：那是后台对账，用户没要求过它，
      // 网络抖一下就在他眼皮底下弹一条红条子是噪音 —— 判据用 reason，不用"领没领号"
      // （后者只覆盖第一跳，第二跳 /changes 超时照样会弹，正是这里修掉的漏网）。
      // 代价说清楚：仓库**持续**读不到时，停在这一页的人会看着上一份数据而没人告诉他。
      // 兜住它的是下一次手动刷新（那时会报），以及任何一次写操作的对账。
      if (reason !== "probe" && requestID !== 0 && mountedRef.current && requestID === reloadRequest.current) failRef.current(cause instanceof Error ? cause.message : "无法读取 Git 仓库");
    } finally {
      // `requestID === 0` = 这次调用压根没领号（probe 的两条早退）⇒ 什么都不该由它收尾。
      // 少了这一条，一个恰好赶上"还没有任何一趟重读"的早退 probe 会把 loading 抹成 false，
      // 界面就从骨架跳成「无法读取仓库状态」—— 一次纯读取的后台动作把首屏判死。
      if (requestID !== 0 && mountedRef.current && requestID === reloadRequest.current) {
        reloadInFlightRef.current = false;
        setLoading(false);
        setRefreshing(false);
        // 补做被让路的那次后台对账（推迟 ≠ 丢弃）。
        if (probePendingRef.current) { probePendingRef.current = false; void reload("probe"); }
      }
    }

    /**
     * 冲突总览：**单独一路，不挡首屏**。
     *
     * 两条理由，一条是速度、一条是纪律：
     *
     *  1. 它是这一组里最慢的一条。干净仓库上它要跑 `ls-files -u` 加**四次
     *     `rev-parse -q --verify <X>_HEAD`**，而 Windows 上每一次都是一次进程创建
     *     （实测 37ms 起），合起来 165ms —— 比 summary 的 86ms 慢一倍。把它摊在
     *     首屏的 `Promise.all` 里，等于让"看一眼变更列表"这件事等冲突探测。
     *  2. 它**不能**降级成"空"（第一版是 `.catch(() => null)`）。那个写法把"这次没读到"
     *     渲染成了"没有冲突"，而仓库真的处在冲突中时，用户看到的会是一个完全正常的仓库，
     *     一个字都不说 —— 正撞在本项目"把读不到写成没有"的红线上。所以它有自己的三档：
     *     读到 `ready` / 读取中 `loading` / **读不到 `unavailable`**；延后取不等于放弃取，
     *     这三档在延后期间照旧成立（横幅本来也由变更列表里的 `conflicted` 标记驱动）。
     */
    async function loadConflicts() {
      // ⚠️ 它**自己**一个作废号，不共用 `reloadRequest`。共用的话，一次"只问了一句话、
      // 什么都没改"的 probe 也会把在飞的那份冲突总览判成过期，而 probe 早退时并不会补发
      // 一份新的 —— 这份读数就永久停在「读取中」，`conflictsState` 既不落到 ready 也不落到
      // unavailable，界面上一个字都不说。冲突那三档（读到 / 读取中 / **读不到**）正是
      // 为了"必然有结论"才立的，不能让一个后台对账把它拖住。
      const id = ++conflictRequest.current;
      try {
        const value = await request<GitConflictOverview>(withWorkspace(`${base}/conflicts`));
        if (!mountedRef.current || id !== conflictRequest.current) return;
        setConflictOverview(value);
        setConflictsState("ready");
        // 正在解决的路径已不在冲突清单中（已解决/中止）时，退出该文件的解决视图。
        if (conflictPathRef.current && !value.files.some((file) => file.path === conflictPathRef.current)) closeConflict();
      } catch {
        if (!mountedRef.current || id !== conflictRequest.current) return;
        setConflictsState("unavailable");
      }
    }
  }, [projectID, request, conversationId]);

  useEffect(() => { reloadRef.current = reload; }, [reload]);
  useEffect(() => {
    // 身份（项目 / 会话 / 取数通道）变了 ⇒ 在飞的那份冲突总览属于**上一个工作区**，必须作废：
    // 它现在只认自己那个号，若不在这里点名作废，它会带着旧工作区的冲突清单落地
    // （界面上就是"B 的页面弹出 A 的冲突横幅"，点进去路径都不对）。这条正是它从前
    // 共用 `reloadRequest` 时白拿到的保护，拆号之后要自己补回来。
    conflictRequest.current += 1;
    if (active && isGitRepo) void reload("initial").catch(() => undefined);
  }, [active, isGitRepo, reload]);

  const grouped = useMemo(() => groupChanges(changes), [changes]);
  const changeCount = changes.length;
  const trackedChangeCount = changes.filter((change) => !change.untracked).length;
  const untrackedChangeCount = changes.filter((change) => change.untracked).length;
  const openDiff = async (change: GitChange, stage: "worktree" | "index") => {
    const requestID = ++diffRequest.current;
    try {
      const diff = await request<GitDiff>(withWorkspace(`/api/projects/${projectID}/git/diff?path=${encodeURIComponent(change.path)}&stage=${stage}`));
      if (requestID === diffRequest.current && mountedRef.current) setSelectedDiff(diff);
    } catch (cause) { if (mountedRef.current) fail(cause instanceof Error ? cause.message : "无法读取差异"); }
  };

  const mutate = async (key: string, endpoint: string, payload: Record<string, unknown>, success?: () => void, options?: ReloadOptions) => {
    if (!mountedRef.current) return;
    if (!snapshot?.stateToken) {
      fail("Git 状态尚未准备完成，请刷新后重试");
      return;
    }
    setMutating(key);
    // 这次写到底成没成 —— 决定 busy 要不要等对账（见 finally）。
    let wrote = false;
    try {
      const result = await request<GitOperationResult>(withWorkspace(`/api/projects/${projectID}/git/${endpoint}`), { method: "POST", body: JSON.stringify({ ...payload, stateToken: snapshot.stateToken }) });
      if (!mountedRef.current) return;
      if (result.status === "needs_attention") {
        closeDiff();
        setConfirmation(null);
      }
      if (result.status !== "succeeded") throw new Error(result.errorMessage || "Git 操作未完成，请查看操作记录");
      wrote = true;
      // **反馈先行**：确认框该关、提交信息该清、差异面板该退 —— 这些不再等重读。
      // 旧写法把它们放在 `await` 的那次重读之后，用户点完「确认提交」要等近一秒
      // （手机上：写一趟中继 + 读一趟中继）才看到对话框关上，那就是"点一下没反应"。
      closeDiff();
      success?.();
    } catch (cause) {
      if (mountedRef.current) fail(cause instanceof Error ? cause.message : "无法执行 Git 操作");
    } finally {
      // ⚠️ 但 **busy 要留到对账落地**，而不是写回来就撤。
      //
      // 写操作会**作废手上那个 stateToken**：它是乐观锁，绑的是写入前那份仓库状态
      // （`gitStateToken` 存着当时的 snapshot / changes / 每个文件的指纹）。新令牌要等
      // 对账把 summary 取回来才到手。这段窗口（本机 ~90ms，手机 ~350ms）里再发一次写，
      // 服务端判定的就是"仓库状态已变化"—— 电脑上对着同一行连点两下就能撞上，
      // 而手机端不会（那边的适配器每次写之前都无条件换一个新令牌）。
      //
      // 于是分工是：**反馈不等它，"能再点"等它。**
      // 失败与 needs_attention 同样要对这一次账：半途失败也会在电脑上留下痕迹。
      if (wrote) {
        // 成功：busy 扣到对账落地。末尾那个 `.catch` 是保险 —— `reload` 自己吞业务错误，
        // 但万一它抛出（那是个 bug），"清 busy 那一句不执行 ⇒ 按钮永远禁着"不该是后果。
        await reload("reconcile", options).catch(() => undefined);
      } else {
        // 失败：对账照发，但**不等它**。
        // 等它的代价是实打实的卡死：写请求超时（推送的预算是 120s）之后确认框还开着，
        // 而框里三颗按钮全是 `disabled={busy}` —— 用户没有任何能按的东西，只能杀进程。
        // 那几十秒的"防连点"不值这个价。代价是这次失败后若立刻再写，可能撞一次
        // "仓库状态已变化"：那是一句可读、可重试的错，与卡死的弹窗不是一个量级。
        void reload("reconcile", options);
      }
      if (mountedRef.current) setMutating("");
    }
  };

  const mutatePath = (action: "stage" | "unstage", path: string) => mutate(`${action}:${path}`, action, { paths: [path] });
  const stageAll = () => mutate("stage-all", "stage-all", {});
  const unstageAll = () => mutate("unstage-all", "unstage-all", {});
  const commit = () => mutate("commit", "commits", { message: commitMessage }, () => { setCommitMessage(""); setConfirmation(null); });
  const commitAmend = () => mutate("commit_amend", "commits/amend", { message: commitMessage }, () => { setCommitMessage(""); setConfirmation(null); });
  const discardWorktree = (path: string, untracked: boolean) => mutate(`discard:${path}`, "discard", { mode: "worktree", paths: [path], includeUntracked: untracked }, () => setConfirmation(null));
  const discardAll = (includeUntracked: boolean) => mutate("discard-all", "discard", { mode: "all", includeUntracked }, () => setConfirmation(null));
  const fetchRemote = (remote: string) => mutate("fetch", "fetch", { remote }, () => setConfirmation(null));
  const pullRemote = (remote: string, branch: string) => mutate("pull", "pull", { remote, branch }, () => setConfirmation(null));
  const pushBranch = (remote: string, branch: string, setUpstream: boolean) => mutate("push", "push", { remote, branch, setUpstream }, () => setConfirmation(null));
  const switchBranch = (branch: string) => mutate("switch-branch", "switch", { branch }, () => setConfirmation(null), { branches: true });
  const pullTarget = gitPullTarget(snapshot);

  // 非 git 项目在空态里发起 git init：成功后置 isGitRepo、通知宿主刷新项目、再加载仓库状态。
  const initializeRepo = async () => {
    if (!mountedRef.current || initializingRepo) return;
    setInitializingRepo(true);
    try {
      const result = await request<{ gitBranch: string; gitReady: boolean }>(`/api/projects/${projectID}/git/init`, { method: "POST" });
      if (result.gitReady) {
        // 装载 effect 依赖 `isGitRepo`，这一置就会自己触发一次首屏读取 ——
        // 这里不再显式 `reload()`（旧版两处都写，等于每次初始化发两轮请求，
        // 只是靠 reloadRequest 的序号把前一轮的结果丢掉而已）。
        setIsGitRepo(true);
        void onGitInitialized?.();
      }
    } catch (cause) {
      if (mountedRef.current) fail(cause instanceof Error ? cause.message : "无法初始化 Git 仓库");
    } finally {
      if (mountedRef.current) setInitializingRepo(false);
    }
  };

  const resolveConflict = (path: string, action: ResolveAction, content?: string) => {
    const payload: Record<string, unknown> = { path, action };
    if (content !== undefined) payload.content = content;
    mutate(`resolve:${path}:${action}`, "conflicts/resolve", payload, () => {
      closeConflict();
      setConfirmation(null);
    });
  };
  const requestAbortConflict = () => setConfirmation({ type: "conflict-abort" });
  const requestFinishConflict = () => setConfirmation({ type: "conflict-finish" });
  const abortConflict = () => mutate("conflict-abort", "conflicts/abort", {}, () => { closeConflict(); setConfirmation(null); });
  const finishConflict = () => mutate("conflict-finish", "conflicts/continue", {}, () => { closeConflict(); setConfirmation(null); });

  const createBranch = async (name: string, startPoint: string) => {
    if (!mountedRef.current) return;
    setMutating("create-branch");
    let wrote = false;
    try {
      const result = await request<GitOperationResult>(withWorkspace(`/api/projects/${projectID}/git/branches`), { method: "POST", body: JSON.stringify({ name, startPoint }) });
      if (!mountedRef.current) return;
      // 与 mutate 同一条：反馈先行，busy 留到对账落地（新建分支会改分支列表，所以带上 branches）。
      if (result.status !== "succeeded" && result.status !== "needs_attention") throw new Error(result.errorMessage || "创建分支失败");
      wrote = true;
      closeDiff();
      setConfirmation(null);
    } catch (cause) {
      if (mountedRef.current) fail(cause instanceof Error ? cause.message : "无法创建分支");
    } finally {
      // 同 mutate：成功才等对账（令牌要换新），失败不等（别把用户扣在卡死的弹窗里）。
      if (wrote) await reload("reconcile", { branches: true }).catch(() => undefined);
      else void reload("reconcile", { branches: true });
      if (mountedRef.current) setMutating("");
    }
  };

  if (!isGitRepo) {
    return <section id="workspace-panel-git" className="git-workbench workspace-panel" role="tabpanel" aria-labelledby="workspace-tab-git" hidden={!active} data-mobile={mobile ? "true" : undefined}>
      <div className="git-init-empty">
        <span className="git-init-empty-mark"><svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="8.5" /><path d="M12 8.5V15M8.7 9.3v5.4M15.3 9.3v5.4M12 8a.9.9 0 1 0 .01 0" /></svg></span>
        <h3>未初始化 Git 仓库</h3>
        <p>当前项目目录还不是 Git 仓库。初始化后将在此查看文件变更、分支与提交历史。</p>
        {/* 手机端不摆这颗按钮：中继的 op 表里**没有** `git.init`（见 remote_git.go），
            点了只会得到一句"手机端没有映射这个 Git 端点：POST /init" ——
            一颗按了必然报技术错误的按钮，比没有按钮更糟。改成把该做的事说清楚。 */}
        {/* 文案要说**怎么做**，不能只说一句"请在电脑上初始化"就完事：
            手机端看到的项目信息来自云端快照，电脑端刚初始化完这里未必立刻知道 ——
            所以给出那个能主动同步一次的动作（页面把它接成"重取项目信息"，见 MobileRemotePage 的
            reloadGit）。另外工作台自己也盯住了 prop 变化：一旦同步回来的是"已经是仓库了"，
            它会当场读仓库，不需要用户退出重进。 */}
        {mobile
          ? <p className="git-init-empty-note">手机端不提供初始化操作。请在电脑上初始化这个项目，然后在右上角 ⋯ 里点「刷新仓库状态」。</p>
          : <button className="primary" type="button" disabled={initializingRepo} onClick={() => void initializeRepo()}>{initializingRepo ? "正在初始化..." : "初始化 Git 仓库"}</button>}
        {mobile ? null : <p className="git-init-empty-note">初始化后建议先提交一个初始 commit，方能使用隔离工作区 / 自动编排。</p>}
      </div>
    </section>;
  }
  return <section id="workspace-panel-git" className="git-workbench workspace-panel" role="tabpanel" aria-labelledby="workspace-tab-git" hidden={!active} data-mobile={mobile ? "true" : undefined} data-detail={mobileDetailOpen ? "open" : undefined}>
    <GitBar snapshot={snapshot} loading={loading} mutating={mutating} refreshing={refreshing} operations={operations} projectID={projectID} conversationId={conversationId} request={request} fail={fail} mobile={mobile} requestRefresh={() => void reload("manual")} requestFetch={() => setConfirmation({ type: "fetch", remote: snapshot?.head.upstream?.split("/")[0] || "origin" })} requestPull={() => setConfirmation({ type: "pull", remote: pullTarget.remote, branch: pullTarget.branch })} requestPush={() => setConfirmation({ type: "push", remote: snapshot?.head.upstream?.split("/")[0] || "origin", branch: snapshot?.head.branch || "", setUpstream: !snapshot?.head.upstream })} />
    <nav className="git-tabs" aria-label="Git工作台视图">
      <div className="git-tab-list" role="tablist" aria-label="Git工作台视图">{tabs.map((item) => <button type="button" key={item.id} id={`git-tab-${item.id}`} role="tab" aria-controls={`git-view-${item.id}`} className={tab === item.id ? "active" : ""} aria-selected={tab === item.id} tabIndex={tab === item.id ? 0 : -1} onClick={() => selectTab(item.id)} onKeyDown={(event) => handleTabKeyDown(event, item.id)}>{item.label}{item.id === "changes" && changeCount > 0 ? <b>{changeCount}</b> : null}</button>)}</div>
    </nav>
    <main className={`git-workbench-body${tab === "changes" ? " changes-active" : ""}`}>
      {/* ⚠️ 判据是 `loading` 本身，**不是** `loading && !snapshot`。
          这个工作台重读的触发之一就是"工作区身份变了"（projectID / conversationId / request
          换了人，手机端还会因为电脑那边切了会话而被动换），而那时手上的 snapshot 是**上一个
          工作区**的 —— 拿它顶上，用户看到的是 A 的变更列表，点下去可能写进 B。
          `loading` 只由首屏那一档置位（后台对账一概不置），所以它本身就等于
          "屏幕上这份数据不属于当前工作区"，不需要再加一个 snapshot 非空的限定。 */}
      {loading ? <GitSkeleton /> : !snapshot ? <div className="git-empty">无法读取仓库状态</div> : <>
        {tab === "changes" && <div id="git-view-changes" role="tabpanel" aria-labelledby="git-tab-changes"><Changes grouped={grouped} selectedDiff={selectedDiff} openDiff={openDiff} closeDiff={closeDiff} conflictOverview={conflictOverview} conflictsState={conflictsState} conflictPath={conflictPath} openConflict={openConflict} closeConflict={closeConflict} resolveConflict={resolveConflict} requestAbortConflict={requestAbortConflict} requestFinishConflict={requestFinishConflict} projectID={projectID} conversationId={conversationId} request={request} fail={fail} mobile={mobile} mutatePath={mutatePath} stageAll={stageAll} unstageAll={unstageAll} requestDiscardWorktree={(path, untracked) => setConfirmation({ type: "discard-worktree", path, untracked })} requestDiscardAll={() => setConfirmation({ type: "discard-all" })} requestCommit={() => setConfirmation({ type: "commit" })} requestAmend={() => setConfirmation({ type: "amend" })} commitMessage={commitMessage} setCommitMessage={setCommitMessage} mutating={mutating} changeCount={changeCount} /></div>}
        {tab === "branches" && <div id="git-view-branches" role="tabpanel" aria-labelledby="git-tab-branches"><Branches branches={branches} mutating={mutating} projectID={projectID} conversationId={conversationId} request={request} fail={fail} requestSwitchBranch={(branch) => setConfirmation({ type: "switch-branch", branch })} requestCreateBranch={(name, startPoint) => void createBranch(name, startPoint)} /></div>}
      </>}
    </main>
    {confirmation && <GitConfirmation confirmation={confirmation} snapshot={snapshot} conflictOverview={conflictOverview} stagedCount={grouped.staged.length} trackedChangeCount={trackedChangeCount} untrackedChangeCount={untrackedChangeCount} commitMessage={commitMessage} busy={Boolean(mutating)} close={() => setConfirmation(null)} commit={commit} commitAmend={commitAmend} discardWorktree={discardWorktree} discardAll={discardAll} fetchRemote={fetchRemote} pullRemote={pullRemote} pushBranch={pushBranch} switchBranch={switchBranch} abortConflict={abortConflict} finishConflict={finishConflict} />}
  </section>;
});

export function GitBar({ snapshot, loading, mutating, refreshing, operations, projectID, conversationId, request, fail, mobile = false, requestRefresh, requestFetch, requestPull, requestPush }: { snapshot: GitSnapshot | null; loading: boolean; mutating: string; refreshing: boolean; operations: GitOperation[]; projectID: string; conversationId?: string; request: Request; fail: (message: string) => void; mobile?: boolean; requestRefresh: () => void; requestFetch: () => void; requestPull: () => void; requestPush: () => void }) {
  const withWorkspace = (path: string) => `${path}${path.includes("?") ? "&" : "?"}${conversationId ? `conversationId=${encodeURIComponent(conversationId)}` : ""}`;
  const [opsOpen, setOpsOpen] = useState(false);
  const [opsItems, setOpsItems] = useState<GitOperation[]>([]);
  const [opsLoading, setOpsLoading] = useState(false);
  const [opsHasMore, setOpsHasMore] = useState(false);
  const [opsType, setOpsType] = useState("");
  const [opsStatus, setOpsStatus] = useState("");
  const [opsQuery, setOpsQuery] = useState("");
  const [opsPendingQuery, setOpsPendingQuery] = useState("");
  const opsRequest = useRef(0);
  const fetchOps = (skip: number, append: boolean) => {
    const requestID = ++opsRequest.current;
    setOpsLoading(true);
    const params = new URLSearchParams({ limit: String(OPS_PAGE_SIZE) });
    if (skip > 0) params.set("skip", String(skip));
    if (opsType) params.set("type", opsType);
    if (opsStatus) params.set("status", opsStatus);
    if (opsPendingQuery) params.set("q", opsPendingQuery);
    request<GitOperation[]>(withWorkspace(`/api/projects/${projectID}/git/operations?${params.toString()}`))
      .then((items) => { if (requestID === opsRequest.current) { setOpsHasMore(items.length === OPS_PAGE_SIZE); setOpsItems((prev) => append ? [...prev, ...items] : items); } })
      .catch((cause) => { if (requestID === opsRequest.current) { if (!append) { setOpsHasMore(false); setOpsItems([]); } fail(cause instanceof Error ? cause.message : "无法读取操作记录"); } })
      .finally(() => { if (requestID === opsRequest.current) setOpsLoading(false); });
  };
  // 打开操作记录时读取第一页；筛选条件或检索词变化后重新读取。
  useEffect(() => {
    if (!opsOpen) return;
    fetchOps(0, false);
    // withWorkspace / request 每次渲染重建，纳入依赖会重复拉取。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [opsOpen, opsType, opsStatus, opsPendingQuery]);
  // 检索词防抖后再查询，避免每次按键都打一次接口。
  useEffect(() => {
    const timer = window.setTimeout(() => setOpsPendingQuery(opsQuery), 300);
    return () => window.clearTimeout(timer);
  }, [opsQuery]);
  const loadMoreOps = () => fetchOps(opsItems.length, true);
  const barRef = useRef<HTMLDivElement>(null);
  const head = snapshot?.head;
  const syncLabel = head?.upstream
    ? head.ahead === 0 && head.behind === 0 ? "与上游同步" : head.ahead > 0 && head.behind === 0 ? `领先 ${head.ahead}` : head.ahead === 0 && head.behind > 0 ? `落后 ${head.behind}` : `领先 ${head.ahead} · 落后 ${head.behind}`
    : "未跟踪上游";
  const needsAttention = operations.some((operation) => operation.status === "failed" || operation.status === "needs_attention");
  // 点击外部或 Escape 键关闭操作记录。沿用 NotificationCenter 的 ref.contains 模式：
  // 只监听不拦截，外部点击（如切换工作区 tab）会正常穿透。
  useEffect(() => {
    if (!opsOpen) return;
    const handleClickOutside = (event: MouseEvent) => {
      const target = event.target as Node;
      if (barRef.current?.contains(target)) return;
      setOpsOpen(false);
    };
    const handleKeyDown = (event: globalThis.KeyboardEvent) => {
      if (event.key === "Escape") setOpsOpen(false);
    };
    document.addEventListener("mousedown", handleClickOutside);
    document.addEventListener("keydown", handleKeyDown);
    return () => {
      document.removeEventListener("mousedown", handleClickOutside);
      document.removeEventListener("keydown", handleKeyDown);
    };
  }, [opsOpen]);
  return <div className="git-bar" ref={barRef} data-mobile={mobile ? "true" : undefined}>
    <div className="git-bar-ref">
      {/* 与 body 那条同源：只要在读，就不显示手上那份（它可能属于上一个工作区）。 */}
      {loading ? <><span className="git-bar-label">当前引用</span><span className="git-bar-loading">正在读取仓库状态…</span></> : <>
        <span className="git-bar-label">当前引用</span>
        <div className="git-bar-name">
          <b title={`当前 HEAD ${head?.oid ?? ""}`}>{head?.detached ? "HEAD (游离指针)" : head?.branch || "未初始化"}</b>
          {head?.oid ? <CopyOID oid={head.oid} title={`点击复制当前 HEAD 提交 ID：${head.oid}`} /> : null}
        </div>
        <span className={`git-sync-badge${head?.upstream ? "" : " muted"}`}>{syncLabel}</span>
      </>}
    </div>
    <div className="git-bar-actions">
      <button type="button" className="secondary" disabled={loading || Boolean(mutating) || head?.detached || !head?.branch || !head?.upstream} onClick={() => { setOpsOpen(false); requestPull(); }} title={loading ? "正在读取仓库状态" : head?.detached || !head?.branch ? "游离指针状态无法拉取" : !head?.upstream ? "未设置上游分支" : undefined}>拉取</button>
      <button type="button" className="secondary" disabled={loading || Boolean(mutating)} onClick={() => { setOpsOpen(false); requestFetch(); }}>获取</button>
      <button type="button" className="secondary" disabled={loading || Boolean(mutating) || head?.detached || !head?.branch || head?.ahead === 0} onClick={() => { setOpsOpen(false); requestPush(); }} title={loading ? "正在读取仓库状态" : head?.detached || !head?.branch ? "游离指针状态无法推送" : head?.ahead === 0 ? "没有需要推送的提交" : undefined}>推送</button>
      <button type="button" className={`git-refresh${refreshing ? " spinning" : ""}`} title={refreshing ? "正在刷新仓库状态" : "刷新仓库状态"} aria-label={refreshing ? "正在刷新仓库状态" : "刷新仓库状态"} disabled={refreshing || Boolean(mutating)} onClick={requestRefresh}><RefreshIcon /></button>
      <button type="button" className={`git-ops-trigger${opsOpen ? " active" : ""}${needsAttention ? " attention" : ""}`} title="操作记录" aria-label="操作记录" aria-expanded={opsOpen} aria-controls="git-ops-popover" disabled={operations.length === 0} onClick={() => setOpsOpen(!opsOpen)}><HistoryIcon />{needsAttention ? <i className="git-ops-dot" aria-hidden="true" /> : null}</button>
    </div>
    {opsOpen && <div id="git-ops-popover" className="git-ops-popover" role="dialog" aria-label="操作记录"><header><div><span>审计记录</span><h3>操作记录</h3></div><button type="button" className="git-confirmation-close" title="关闭" aria-label="关闭" onClick={() => setOpsOpen(false)}><CloseIcon /></button></header><div className="git-ops-popover-body"><div className="git-ops-filters">{mobile ? <><FilterChips label="按操作类型筛选" value={opsType} options={[{ value: "", label: "全部类型" }, ...OPERATION_TYPE_VALUES.map((value) => ({ value, label: operationTypeLabel(value) }))]} onChange={setOpsType} /><FilterChips label="按操作状态筛选" value={opsStatus} options={[{ value: "", label: "全部状态" }, ...OPERATION_STATUS_VALUES.map((value) => ({ value, label: operationStatusLabel(value) }))]} onChange={setOpsStatus} /></> : <><select value={opsType} aria-label="按操作类型筛选" onChange={(event) => setOpsType(event.target.value)}><option value="">全部类型</option>{OPERATION_TYPE_VALUES.map((value) => <option key={value} value={value}>{operationTypeLabel(value)}</option>)}</select><select value={opsStatus} aria-label="按操作状态筛选" onChange={(event) => setOpsStatus(event.target.value)}><option value="">全部状态</option>{OPERATION_STATUS_VALUES.map((value) => <option key={value} value={value}>{operationStatusLabel(value)}</option>)}</select></>}<div className="git-ops-search"><input type="text" value={opsQuery} placeholder="搜索操作…" aria-label="搜索操作记录" onChange={(event) => setOpsQuery(event.target.value)} />{opsQuery ? <button type="button" className="git-history-search-clear" title="清除搜索" aria-label="清除搜索" onClick={() => setOpsQuery("")}>×</button> : null}</div></div><Operations operations={opsItems} loading={opsLoading} />{opsHasMore && !opsLoading ? <button type="button" className="git-ops-load-more" onClick={loadMoreOps}>加载更多操作</button> : null}</div></div>}
  </div>;
}

function PlusIcon() { return <svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round"><path d="M8 3v10M3 8h10" /></svg>; }

function UndoIcon() { return <svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round"><path d="M13.5 11a5.5 5.5 0 0 0-9.4-3.9L2.5 8.7" /><path d="M2.5 4.5v4.2h4.2" /></svg>; }

function PendingIcon() { return <svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round"><path d="M14 8A6 6 0 1 1 8 2" /><polyline points="8 2 8 5.5 11 2.5" /></svg>; }

function RefreshIcon() { return <svg viewBox="0 0 16 16" width="16" height="16" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round"><path d="M13 5.8A5.5 5.5 0 1 0 13.4 10" /><path d="M13 2.5v3.3H9.7" /></svg>; }

function CloseIcon() { return <svg viewBox="0 0 16 16" width="16" height="16" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round"><path d="m4 4 8 8M12 4l-8 8" /></svg>; }

function HistoryIcon() { return <svg viewBox="0 0 16 16" width="16" height="16" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round"><path d="M3.2 8a4.8 4.8 0 1 1 1.4 3.4" /><path d="M3 4.2v3.4h3.4" /><path d="M8 5v3l2 1.4" /></svg>; }

/**
 * 筛选用的胶囊行（**只在手机端**替代原生 `<select>`）。
 *
 * 原生 `<select>` 在手机端本项目明令不用：它的弹层由系统绘制、样式一行都管不到，
 * 而这页的其余分类栏（任务队列等）一直是胶囊行。桌面端保持 `<select>` 不变 ——
 * 那里它更好用（可键盘操作、选项多时不需要横向滚动）。
 */
function FilterChips({ label, value, options, onChange }: { label: string; value: string; options: Array<{ value: string; label: string }>; onChange: (value: string) => void }) {
  return <div className="git-ops-chips" role="group" aria-label={label}>
    {options.map((item) => <button type="button" key={item.value || "all"} className={value === item.value ? "active" : ""} aria-pressed={value === item.value} onClick={() => onChange(item.value)}>{item.label}</button>)}
  </div>;
}

/**
 * 首屏骨架：进 Git 视图时先给出**结构**，而不是一句「正在读取仓库状态」占着整屏。
 *
 * 手机端首屏那一次是走云中继的（实测本机到云端 RTT ~150ms，一个来回 ~300ms，
 * 加上最慢那条读接口 165ms，约 465ms），这段时间里屏幕本来是空的。骨架让
 * "页面已经在、正在填内容"立刻成立，比一句话等着更接近真实进度。
 *
 * 它同时收窄了那句 loading 文案的出场机会：只有**真的没有内容可显示**（`!snapshot`）
 * 时才顶上，后台对账一概不碰 —— 于是它再也不会像以前那样每隔几秒闪一次。
 */
function GitSkeleton() {
  return <div className="git-skeleton" role="status">
    <span className="git-skeleton-sr">正在读取仓库状态</span>
    {[0, 1].map((group) => <div className="git-skeleton-group" key={group} aria-hidden="true">
      <header />
      {[0, 1, 2].map((row) => <div className="git-skeleton-row" key={row}><i /><em /></div>)}
    </div>)}
  </div>;
}

function Changes({ grouped, selectedDiff, openDiff, closeDiff, conflictOverview, conflictsState, conflictPath, openConflict, closeConflict, resolveConflict, requestAbortConflict, requestFinishConflict, projectID, conversationId, request, fail, mobile, mutatePath, stageAll, unstageAll, requestDiscardWorktree, requestDiscardAll, requestCommit, requestAmend, commitMessage, setCommitMessage, mutating, changeCount }: {
  grouped: { staged: GitChange[]; worktree: GitChange[] };
  selectedDiff: GitDiff | null;
  openDiff: (change: GitChange, stage: "worktree" | "index") => Promise<void>;
  closeDiff: () => void;
  conflictOverview: GitConflictOverview | null;
  /** 冲突总览的三档。`unavailable` 必须**显式**渲染，不许落回"没有冲突"。 */
  conflictsState: "loading" | "ready" | "unavailable";
  conflictPath: string | null;
  openConflict: (path: string) => void;
  closeConflict: () => void;
  resolveConflict: (path: string, action: ResolveAction, content?: string) => void;
  requestAbortConflict: () => void;
  requestFinishConflict: () => void;
  projectID: string;
  conversationId?: string;
  request: Request;
  fail: (message: string) => void;
  mobile?: boolean;
  mutatePath: (action: "stage" | "unstage", path: string) => void;
  stageAll: () => void;
  unstageAll: () => void;
  requestDiscardWorktree: (path: string, untracked: boolean) => void;
  requestDiscardAll: () => void;
  requestCommit: () => void;
  requestAmend: () => void;
  commitMessage: string;
  setCommitMessage: (value: string) => void;
  mutating: string;
  changeCount: number;
}) {
  const conflictedCount = grouped.worktree.filter((change) => change.conflicted).length;
  const context = conflictOverview?.context;
  const operationVerb = conflictOperationVerb(context?.operationType);
  const openFile = (change: GitChange, stage: "worktree" | "index") => {
    if (change.conflicted) openConflict(change.path);
    else { closeConflict(); void openDiff(change, stage); }
  };
  const contextPhrase = context && context.operationType !== "none" && context.theirsLabel ? `${context.theirsLabel} → ${context.oursLabel}` : "";
  return <div className="git-changes-view" data-mobile={mobile ? "true" : undefined}>
    <div className="git-changes-sidebar">
      {/* 冲突总览读不到时**必须说出来**。它与"没有冲突"是两件事：仓库真的处在冲突中
          而这一栏读不到时，界面若不吭声，用户看到的就是一个完全正常的仓库。 */}
      {conflictsState === "unavailable" && <div className="git-conflict-unavailable" role="status"><b>读不到冲突状态</b><span>这次没能从电脑上读到仓库的冲突信息。下方文件列表里的「冲突」标记仍然可靠；点顶部的刷新可以重试。</span></div>}
      {conflictedCount > 0
        ? <div className="git-conflict-banner" role="alert"><b>⚠ {conflictedCount} 个冲突文件需要解决</b><span>{operationVerb && contextPhrase ? `正在${operationVerb} ${contextPhrase}，点击文件逐块处理` : "在下方工作区列表中点击文件进入解决视图"}</span></div>
        : context?.canFinish
          ? <div className="git-conflict-banner finished" role="status"><b>✓ 冲突已全部解决</b><span>{operationVerb}可完成，提交后将结束本次操作</span><div className="git-conflict-banner-actions"><button type="button" className="primary" disabled={mutating !== ""} onClick={requestFinishConflict}>完成{operationVerb}</button><button type="button" className="secondary danger" disabled={mutating !== ""} onClick={requestAbortConflict}>中止并还原</button></div></div>
          : null}
      <section className="git-change-group"><header><h3>已暂存</h3><div className="git-group-actions"><span>{grouped.staged.length}</span><button type="button" className={`git-icon-btn${mutating === "unstage-all" ? " spinning" : ""}`} title={mutating === "unstage-all" ? "取消暂存中" : "全部取消暂存"} aria-label="全部取消暂存" disabled={mutating !== "" || grouped.staged.length === 0} onClick={unstageAll}>{mutating === "unstage-all" ? <PendingIcon /> : <UndoIcon />}</button></div></header><ChangeList changes={grouped.staged} stage="index" openDiff={openFile} mutatePath={mutatePath} requestDiscard={requestDiscardWorktree} mutating={mutating} empty="没有已暂存变更" /></section>
      <CommitPanel stagedCount={grouped.staged.length} value={commitMessage} setValue={setCommitMessage} open={requestCommit} openAmend={requestAmend} disabled={mutating !== ""} />
      <section className="git-change-group"><header><h3>工作区</h3><div className="git-group-actions"><span>{grouped.worktree.length}</span><button type="button" className={`git-icon-btn${mutating === "stage-all" ? " spinning" : ""}`} title={mutating === "stage-all" ? "暂存中" : conflictedCount > 0 ? "存在冲突文件，请先逐个解决" : "全部暂存"} aria-label="全部暂存" disabled={mutating !== "" || grouped.worktree.length === 0 || conflictedCount > 0} onClick={stageAll}>{mutating === "stage-all" ? <PendingIcon /> : <PlusIcon />}</button><button type="button" className="git-icon-btn danger" title="撤销全部未提交改动" aria-label="撤销全部未提交改动" disabled={mutating !== "" || changeCount === 0} onClick={requestDiscardAll}><UndoIcon /></button></div></header><ChangeList changes={grouped.worktree} stage="worktree" openDiff={openFile} mutatePath={mutatePath} requestDiscard={requestDiscardWorktree} mutating={mutating} empty="工作区没有变更" /></section>
    </div>
    <section className="git-changes-content" aria-label="文件内容">
      {/* 手机端是「列表 ⇄ 详情」两级推进（桌面端两栏并排，不需要这颗按钮）。
          它同时也是"当前有没有上一层"的可见答案。 */}
      {mobile && (selectedDiff || conflictPath) && <button type="button" className="git-mobile-back" onClick={() => { closeConflict(); closeDiff(); }}>← 变更列表</button>}
      {conflictPath
        ? <ConflictSolveView key={conflictPath} projectID={projectID} conversationId={conversationId} path={conflictPath} conflictPaths={(conflictOverview?.files.map((file) => file.path) ?? [conflictPath])} request={request} fail={fail} oursLabel={context?.oursLabel || "当前"} theirsLabel={context?.theirsLabel || "传入"} busy={mutating !== ""} mobile={mobile} onResolve={resolveConflict} onOpenFile={openConflict} onClose={closeConflict} />
        : selectedDiff ? <DiffViewer diff={selectedDiff} close={closeDiff} /> : <div className="git-diff-placeholder"><div className="git-diff-placeholder-icon" aria-hidden="true"><PendingIcon /></div><h3>选择一个文件查看变更</h3></div>}
    </section>
  </div>;
}

function conflictOperationVerb(operationType: GitConflictOverview["context"]["operationType"] | undefined): string {
  return ({ merge: "合并", rebase: "变基", "cherry-pick": "挑选", revert: "还原", none: "" })[operationType ?? "none"] || "";
}

export function parseDiffContent(content: string): DiffLine[] {
  const source = content.split(/\r?\n/);
  if (source.at(-1) === "") source.pop();
  const unified = source.some((line) => line.startsWith("@@ "));
  let oldLine = 1;
  let newLine = 1;
  let sawHunk = false;
  return source.map((line) => {
    const hunk = line.match(/^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/);
    if (hunk) {
      sawHunk = true;
      oldLine = Number(hunk[1]);
      newLine = Number(hunk[2]);
      return { content: line, kind: "hunk" };
    }
    // 首个 hunk 之前的全部是文件头（diff --git、new file mode、rename from、Binary files 等），统一按元信息显示。
    if (unified && !sawHunk) return { content: line, kind: "meta" };
    if (unified && line.startsWith("\\ No newline")) return { content: line, kind: "meta" };
    if (unified && line.startsWith("+")) return { content: line.slice(1), kind: "added", newLine: newLine++ };
    if (unified && line.startsWith("-")) return { content: line.slice(1), kind: "removed", oldLine: oldLine++ };
    if (unified && line.startsWith(" ")) return { content: line.slice(1), kind: "context", oldLine: oldLine++, newLine: newLine++ };
    return { content: line, kind: "context", oldLine: oldLine++, newLine: newLine++ };
  });
}

function DiffViewer({ diff, close }: { diff: GitDiff; close: () => void }) {
  const lines = parseDiffContent(diff.content);
  const stageLabel = diff.stage === "index" ? "已暂存差异" : diff.stage === "commit" ? "提交差异" : "工作区差异";
  return <section className="git-diff"><header><div><span>{stageLabel}</span><b>{diff.path}</b></div><button type="button" className="git-close" title="关闭差异" aria-label="关闭差异" onClick={close}>×</button></header>{lines.length ? <div className="git-diff-code" role="region" aria-label={`${diff.path} 的${stageLabel}`}>{lines.map((line, index) => <div className={`git-diff-line ${line.kind}`} key={`${index}-${line.content}`}><span className="git-diff-number">{line.oldLine ?? ""}</span><span className="git-diff-number">{line.newLine ?? ""}</span><span className="git-diff-prefix" aria-hidden="true">{line.kind === "added" ? "+" : line.kind === "removed" ? "-" : ""}</span><code>{line.content}</code></div>)}</div> : <p className="git-diff-empty">该文件没有可显示的文本差异。</p>}</section>;
}

function ChangeList({ changes, stage, openDiff, mutatePath, requestDiscard, mutating, empty }: { changes: GitChange[]; stage: "worktree" | "index"; openDiff: (change: GitChange, stage: "worktree" | "index") => void; mutatePath: (action: "stage" | "unstage", path: string) => void; requestDiscard: (path: string, untracked: boolean) => void; mutating: string; empty: string }) {
  if (changes.length === 0) return <p className="git-list-empty">{empty}</p>;
  const action = stage === "index" ? "unstage" : "stage";
  return <div className="git-change-list">{changes.map((change) => {
    const conflictBlocked = change.conflicted && stage === "worktree";
    return <div key={`${stage}-${change.path}`}><button type="button" onClick={() => openDiff(change, stage)}><span className={`git-change-kind ${change.conflicted ? "conflicted" : ""}`}>{changeState(change)}</span><b>{change.path}</b>{change.originalPath ? <small>{change.originalPath}</small> : null}</button><div className="git-inline-actions"><button type="button" className={`git-inline-icon${mutating === `${action}:${change.path}` ? " spinning" : ""}`} title={mutating === `${action}:${change.path}` ? "处理中" : conflictBlocked ? "冲突文件请先进入右侧解决视图" : action === "stage" ? "暂存" : "取消暂存"} aria-label={action === "stage" ? "暂存" : "取消暂存"} disabled={mutating !== "" || conflictBlocked} onClick={() => mutatePath(action, change.path)}>{mutating === `${action}:${change.path}` ? <PendingIcon /> : action === "stage" ? <PlusIcon /> : <UndoIcon />}</button>{stage === "worktree" && <button type="button" className="git-inline-icon danger" title={change.untracked ? "删除未跟踪文件" : "撤销工作区改动"} aria-label={change.untracked ? "删除未跟踪文件" : "撤销工作区改动"} disabled={mutating !== "" || change.conflicted} onClick={() => requestDiscard(change.path, change.untracked)}>{change.untracked ? <svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round"><polyline points="2 4.5 14 4.5" /><path d="M5 4.5V3a1 1 0 0 1 1-1h4a1 1 0 0 1 1 1v1.5" /><path d="M3.5 4.5l.7 9a1.5 1.5 0 0 0 1.5 1.4h4.6a1.5 1.5 0 0 0 1.5-1.4l.7-9" /></svg> : <UndoIcon />}</button>}</div></div>;
  })}</div>;
}

// 提交信息首行的长度上限，与后端 git_operations.go 的校验保持一致。
export const MAX_COMMIT_SUBJECT_LENGTH = 72;

// 首行长度必须与后端 gitCommit / gitAmendCommit 的算法对齐，那里是
// strings.TrimSpace(message) 之后再取第一行做 utf8.RuneCountInString：
//   - 先 trim：否则 "\n\n<73 字>" 这种以空行开头的信息，前端取到的首行是空串、
//     判定合法放行，后端 trim 后数出 73 字直接 400。
//   - 再按码点算：JS 的 .length 数的是 UTF-16 码元，一个 emoji 在前端算 2、后端算 1，
//     按钮会比后端更早变灰，把明明能提交的信息拦下来。
// （JS 的 trim 与 Go 的 TrimSpace 只在 U+0085 这类冷门空白符上不同，提交信息里碰不到。）
export function commitSubjectLength(message: string): number {
  return Array.from(message.trim().split("\n")[0]).length;
}

export function CommitPanel({ stagedCount, value, setValue, open, openAmend, disabled }: { stagedCount: number; value: string; setValue: (value: string) => void; open: () => void; openAmend: () => void; disabled: boolean }) {
  const subjectLength = commitSubjectLength(value);
  const subjectOverLimit = subjectLength > MAX_COMMIT_SUBJECT_LENGTH;
  return <section className="git-commit-panel"><label htmlFor="git-commit-message">提交信息</label><textarea id="git-commit-message" value={value} maxLength={4000} disabled={disabled || stagedCount === 0} aria-invalid={subjectOverLimit || undefined} aria-describedby={subjectOverLimit ? "git-commit-subject-warning" : undefined} onChange={(event) => setValue(event.target.value)} placeholder={stagedCount === 0 ? "暂存文件后即可提交" : "简要说明本次变更"} />
    {/* 首行超长会让「提交」变灰，必须当场说明原因，否则用户只会以为按钮坏了。
        文案直接点明「提交」和「修改最近一次提交」都会受影响 —— 只说限制，
        用户仍会去点那个同样变灰的修改按钮。
        提示不给按钮加 title：禁用的按钮不派发鼠标事件，title 根本不会弹出。 */}
    {subjectOverLimit ? <p className="git-commit-subject-warning" id="git-commit-subject-warning" role="alert">提交信息首行最多 {MAX_COMMIT_SUBJECT_LENGTH} 个字符，超出后无法提交，也无法修改最近一次提交。请缩短首行，多行说明写在第二行起。</p> : null}
    {/* 计数器从能输入的那一刻就常驻，而不是等超限才出现：这条 72 字的规矩要在用户写之前就看得见。 */}
    <footer><div className="git-commit-meta"><span>{stagedCount === 0 ? "没有已暂存文件" : `将提交 ${stagedCount} 个文件`}</span>{stagedCount === 0 ? null : <span className={`git-commit-subject-count${subjectOverLimit ? " over" : ""}`} title={`提交信息首行的字符数，上限 ${MAX_COMMIT_SUBJECT_LENGTH}`}>首行 {subjectLength}/{MAX_COMMIT_SUBJECT_LENGTH}</span>}</div><div className="git-commit-actions"><button type="button" className="secondary git-amend-btn" title="修改最近一次提交" disabled={disabled || !value.trim() || subjectOverLimit} onClick={openAmend}>修改最近一次提交</button><button type="button" className="primary" disabled={disabled || stagedCount === 0 || !value.trim() || subjectOverLimit} onClick={open}>提交</button></div></footer></section>;
}

function GitConfirmation({ confirmation, snapshot, conflictOverview, stagedCount, trackedChangeCount, untrackedChangeCount, commitMessage, busy, close, commit, commitAmend, discardWorktree, discardAll, fetchRemote, pullRemote, pushBranch, switchBranch, abortConflict, finishConflict }: {
  confirmation: Confirmation;
  snapshot: GitSnapshot | null;
  conflictOverview: GitConflictOverview | null;
  stagedCount: number;
  trackedChangeCount: number;
  untrackedChangeCount: number;
  commitMessage: string;
  busy: boolean;
  close: () => void;
  commit: () => void;
  commitAmend: () => void;
  discardWorktree: (path: string, untracked: boolean) => void;
  discardAll: (includeUntracked: boolean) => void;
  fetchRemote: (remote: string) => void;
  pullRemote: (remote: string, branch: string) => void;
  pushBranch: (remote: string, branch: string, setUpstream: boolean) => void;
  switchBranch: (branch: string) => void;
  abortConflict: () => void;
  finishConflict: () => void;
}) {
  const [includeUntracked, setIncludeUntracked] = useState(confirmation.type === "discard-all" && untrackedChangeCount > 0 && trackedChangeCount === 0);
  const isCommit = confirmation.type === "commit";
  const isAmend = confirmation.type === "amend";
  const isFetch = confirmation.type === "fetch";
  const isPull = confirmation.type === "pull";
  const isPush = confirmation.type === "push";
  const isSwitch = confirmation.type === "switch-branch";
  const isAbort = confirmation.type === "conflict-abort";
  const isFinish = confirmation.type === "conflict-finish";
  const isDestructive = confirmation.type === "discard-worktree" || confirmation.type === "discard-all" || isAbort;
  const operationVerb = conflictOperationVerb(conflictOverview?.context?.operationType);
  const context = conflictOverview?.context;
  const title = isCommit ? "确认提交" : isAmend ? "确认修改提交" : isFetch ? "获取远端更新" : isPull ? "拉取远端更新" : isPush ? "推送到远端" : isSwitch ? "切换分支" : isAbort ? `中止${operationVerb}并还原` : isFinish ? `完成${operationVerb}` : confirmation.type === "discard-all" ? "丢弃全部未提交改动" : confirmation.type === "discard-worktree" && confirmation.untracked ? "删除未跟踪文件" : "撤销工作区更改";
  const isUntracked = confirmation.type === "discard-worktree" && confirmation.untracked;
  const discardPath = confirmation.type === "discard-worktree" ? confirmation.path : "";
  const riskLabel = confirmation.type === "discard-worktree" && !confirmation.untracked ? "工作区内容将恢复到暂存版本" : isAbort ? `当前${operationVerb}产生的所有改动都会丢弃，仓库将还原到操作前状态` : "此操作无法在页面中撤销";
  const categoryLabel = isCommit || isAmend ? "本地提交" : isFetch || isPull ? "远端操作" : isPush ? "远端操作" : isSwitch ? "分支操作" : isAbort || isFinish ? "冲突处理" : "高风险操作";
  const confirm = () => {
    if (confirmation.type === "commit") commit();
    else if (confirmation.type === "amend") commitAmend();
    else if (confirmation.type === "discard-all") discardAll(includeUntracked);
    else if (confirmation.type === "discard-worktree") discardWorktree(confirmation.path, confirmation.untracked);
    else if (confirmation.type === "fetch") fetchRemote(confirmation.remote);
    else if (confirmation.type === "pull") pullRemote(confirmation.remote, confirmation.branch);
    else if (confirmation.type === "push") pushBranch(confirmation.remote, confirmation.branch, confirmation.setUpstream);
    else if (confirmation.type === "switch-branch") switchBranch(confirmation.branch);
    else if (confirmation.type === "conflict-abort") abortConflict();
    else if (confirmation.type === "conflict-finish") finishConflict();
  };
  const confirmLabel = busy ? "处理中" : isCommit ? "确认提交" : isAmend ? "确认修改" : isFetch ? "确认获取" : isPull ? "确认拉取" : isPush ? "确认推送" : isSwitch ? "确认切换" : isAbort ? "确认中止" : isFinish ? `确认完成${operationVerb}` : isUntracked ? "确认删除" : "确认撤销";
  return <div className="backdrop git-confirmation-backdrop" role="dialog" aria-modal="true" aria-labelledby="git-confirmation-title"><section className={`modal git-confirmation-dialog${isDestructive ? " destructive" : ""}`}><header><div><span>{categoryLabel}</span><h2 id="git-confirmation-title">{title}</h2></div><button type="button" className="git-confirmation-close" title="关闭" aria-label="关闭" disabled={busy} onClick={close}><CloseIcon /></button></header><div className="git-confirmation-body">
    {confirmation.type === "commit" ? <><p className="git-confirmation-lead">将在 <b>{snapshot?.head.branch || "当前 HEAD"}</b> 创建本地提交，包含 {stagedCount} 个已暂存文件。</p><pre>{commitMessage}</pre></>
      : confirmation.type === "amend" ? <><div className="git-confirmation-risk"><span>注意</span><p>修改最近一次提交会改写本地历史。若已推送到远端，普通推送会被拒绝（需强制推送）。</p></div><p className="git-confirmation-lead">将 <b>{snapshot?.head.branch || "当前 HEAD"}</b> 的最近一次提交改写为：</p><pre>{commitMessage}</pre>{stagedCount > 0 ? <div className="git-confirmation-scope"><span>操作说明</span><b>已暂存的 {stagedCount} 个文件改动会一并并入该提交</b></div> : <div className="git-confirmation-scope"><span>操作说明</span><b>无已暂存改动，仅重写提交信息</b></div>}</>
      : confirmation.type === "fetch" ? <><p className="git-confirmation-lead">从 <b>{confirmation.remote}</b> 获取最新更新。</p><div className="git-confirmation-scope"><span>操作说明</span><b>更新远端引用并清理已删除的分支引用</b><small>工作树不会改变</small></div></>
      : confirmation.type === "pull" ? <><p className="git-confirmation-lead">从 <b>{confirmation.remote}</b> 拉取 <b>{confirmation.branch}</b> 并合并最新更新。</p><div className="git-confirmation-scope"><span>操作说明</span><b>仅允许快进合并，历史分叉时会失败以保护提交</b><small>有未提交改动时，冲突文件可能导致拉取失败</small></div></>
      : confirmation.type === "push" ? <><p className="git-confirmation-lead">将 <b>{confirmation.branch}</b> 推送到 <b>{confirmation.remote}</b>。</p><div className="git-confirmation-scope"><span>操作说明</span><b>仅允许普通推送，non-fast-forward 将被拒绝</b>{confirmation.setUpstream ? <small>将同时设置上游跟踪</small> : null}</div></>
      : confirmation.type === "switch-branch" ? <><div className="git-confirmation-risk"><span>注意</span><p>切换分支前需确保工作区干净且无进行中的 AI 任务。</p></div><p className="git-confirmation-lead">将切换到分支 <b>{confirmation.branch}</b>。</p><div className="git-confirmation-scope"><span>当前分支</span><b>{snapshot?.head.branch || "HEAD"}</b></div></>
      : confirmation.type === "conflict-abort" ? <><div className="git-confirmation-risk"><span>注意</span><p>{riskLabel}</p></div>{context && context.operationType !== "none" ? <p className="git-confirmation-lead">正在{operationVerb} <b>{context.theirsLabel || ""} → {context.oursLabel}</b>，中止后将回到操作前状态。</p> : <p className="git-confirmation-lead">将中止当前冲突操作并丢弃所有已做解决的改动。</p>}</>
      : confirmation.type === "conflict-finish" ? <><p className="git-confirmation-lead">所有冲突文件都已解决，将{operationVerb} <b>{context?.theirsLabel || ""} → {context?.oursLabel || ""}</b> 并结束本次操作。</p><div className="git-confirmation-scope"><span>操作说明</span><b>解决结果会以{operationVerb}提交的形式保留</b></div></>
      : <><div className="git-confirmation-risk"><span>注意</span><p>{riskLabel}</p></div>{confirmation.type === "discard-all" ? <><p className="git-confirmation-lead">已暂存和工作区中的受跟踪改动都会恢复到 <code>HEAD</code>。</p><div className="git-confirmation-scope"><span>受影响内容</span><b>{trackedChangeCount} 个受跟踪文件</b>{untrackedChangeCount > 0 ? <small>另有 {untrackedChangeCount} 个未跟踪文件可选删除</small> : null}</div><label className="git-confirmation-check"><input type="checkbox" checked={includeUntracked} onChange={(event) => setIncludeUntracked(event.target.checked)} />同时删除未跟踪文件</label></> : <><p className="git-confirmation-lead">{isUntracked ? "将永久删除以下未跟踪文件。" : "将以下文件恢复到暂存版本，已暂存内容不会改变。"}</p><code className="git-confirmation-path">{discardPath}</code></>}</>}
  </div><footer><button type="button" className="secondary" disabled={busy} onClick={close}>取消</button><button type="button" className={isDestructive ? "primary danger" : "primary"} disabled={busy} onClick={confirm}>{confirmLabel}</button></footer></section></div>;
}

async function copyToClipboard(text: string): Promise<boolean> {
  if (navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // 回退到传统拷贝方式
    }
  }
  try {
    const textarea = document.createElement("textarea");
    textarea.value = text;
    textarea.setAttribute("readonly", "");
    textarea.style.position = "fixed";
    textarea.style.opacity = "0";
    document.body.append(textarea);
    textarea.select();
    const ok = document.execCommand("copy");
    textarea.remove();
    return ok;
  } catch {
    return false;
  }
}

// 可点击复制完整 commit id 的短 id 徽标；点击复制后短暂显示“已复制”。
function CopyOID({ oid, title }: { oid: string; title?: string }) {
  const [copied, setCopied] = useState(false);
  const copy = async (event: { stopPropagation: () => void }) => {
    event.stopPropagation();
    const ok = await copyToClipboard(oid);
    if (ok) {
      setCopied(true);
      toast.success("已复制提交 ID");
      window.setTimeout(() => setCopied(false), 1200);
    } else {
      toast.error("复制失败，请手动复制");
    }
  };
  return <code className={copied ? "git-oid copied" : "git-oid"} title={title ?? `点击复制完整提交 ID：${oid}`} role="button" tabIndex={0} aria-label={`复制提交 ID ${oid}`} onClick={copy} onKeyDown={(event) => { if (event.key === "Enter" || event.key === " ") { event.preventDefault(); void copy(event); } }}>{copied ? <span className="git-oid-copied">已复制</span> : shortOID(oid)}</code>;
}

export function CommitHistory({ commits, loading, error = "", selectedOID, onSelect }: { commits: GitCommit[]; loading: boolean; error?: string; selectedOID?: string; onSelect: (commit: GitCommit) => void }) {
  if (loading) return <div className="git-empty">正在读取该分支的历史</div>;
  // 「读不到」绝不能写成「没有」：前者是关于我们这次读取的，后者是关于仓库的事实。
  // 空态里有下一步动作（点顶部的刷新重试），所以按项目约定走**卡片**，不是一行灰字。
  if (error !== "" && commits.length === 0) return <div className="git-history-error" role="status"><b>读不到提交历史</b><span>{error}</span><small>点顶部的「刷新仓库状态」可以重试。</small></div>;
  if (commits.length === 0) return <div className="git-empty">该分支没有可显示的提交记录</div>;
  return <div className="git-history">{commits.map((commit) => <article key={commit.oid} className={commit.oid === selectedOID ? "selected" : ""} title="点击查看该提交的变更" tabIndex={0} onClick={() => onSelect(commit)} onKeyDown={(event) => { if (event.key === "Enter" || event.key === " ") { event.preventDefault(); onSelect(commit); } }}><CopyOID oid={commit.oid} /><div><b>{commit.subject || "(无提交说明)"}</b><span>{isMergeCommit(commit) ? <i className="git-commit-merge-badge" title="合并提交">合并</i> : null}{commit.author} · {formatGitTime(commit.authoredAt)}</span></div></article>)}</div>;
}

// 提交详情视图：头部提交摘要，正文为提交信息全文 + 文件列表|差异内容双栏（与“变更”tab 同构）。
function CommitDetailView({ commit, detail, loading, error, selectedFile, fileDiff, fileDiffLoading, onOpenFile, onCloseFile, onBack }: { commit: GitCommit; detail: GitCommitDetail | null; loading: boolean; error: string; selectedFile: GitCommitFile | null; fileDiff: GitDiff | null; fileDiffLoading: boolean; onOpenFile: (file: GitCommitFile) => void; onCloseFile: () => void; onBack: () => void }) {
  const isMerge = isMergeCommit(commit);
  const body = detail && detail.message !== detail.subject ? detail.message.slice(detail.subject.length).replace(/^\n+/, "") : "";
  return <div className="git-commit-detail">
    <header className="git-commit-detail-header">
      <button type="button" className="git-commit-back" onClick={onBack} title="返回提交历史">‹ 返回历史</button>
      <div className="git-commit-detail-meta">
        <b>{commit.subject || "(无提交说明)"}</b>
        <span><CopyOID oid={commit.oid} /> {commit.author} · {formatGitTime(commit.authoredAt)}</span>
      </div>
      {isMerge ? <i className="git-commit-merge-badge" title="合并提交，显示相对第一个父提交的差异">合并</i> : null}
    </header>
    {body ? <pre className="git-commit-message">{body}</pre> : null}
    {error ? <div className="git-empty">{error}</div> : <div className="git-commit-detail-body">
      <div className="git-commit-files" aria-label="变更文件列表">
        {loading ? <div className="git-empty">正在读取提交详情</div> : !detail ? <div className="git-empty">暂无提交详情</div> : detail.files.length === 0 ? <div className="git-empty">该提交没有文件变更</div> : detail.files.map((file) => <button type="button" key={file.path} className={selectedFile?.path === file.path ? "selected" : ""} title="点击查看该文件的差异" onClick={() => onOpenFile(file)}><span className={`git-commit-file-status ${file.status}`}>{commitFileStatusLabel(file.status)}</span><span className="git-commit-file-path"><b>{file.path}</b>{file.originalPath ? <small>{file.originalPath}</small> : null}</span><span className="git-commit-file-stats">{file.binary ? <i>二进制</i> : <><i className="additions">+{file.additions}</i><i className="deletions">-{file.deletions}</i></>}</span></button>)}
      </div>
      <section className="git-changes-content" aria-label="文件差异">
        {fileDiff ? <DiffViewer diff={fileDiff} close={onCloseFile} /> : <div className="git-diff-placeholder"><div className="git-diff-placeholder-icon" aria-hidden="true"><PendingIcon /></div><h3>{fileDiffLoading ? "正在读取差异" : "选择一个文件查看变更"}</h3><p>{fileDiffLoading ? "差异内容即将显示。" : "从变更文件列表里点击文件，该提交中的差异会显示在这里。"}</p></div>}
      </section>
    </div>}
  </div>;
}

function Branches({ branches, mutating, projectID, conversationId, request, fail, requestSwitchBranch, requestCreateBranch }: { branches: GitBranch[]; mutating: string; projectID: string; conversationId?: string; request: Request; fail: (message: string) => void; requestSwitchBranch: (branch: string) => void; requestCreateBranch: (name: string, startPoint: string) => void }) {
  const withWorkspace = (path: string) => `${path}${path.includes("?") ? "&" : "?"}${conversationId ? `conversationId=${encodeURIComponent(conversationId)}` : ""}`;
  const [showForm, setShowForm] = useState(false);
  const [newName, setNewName] = useState("");
  const [startPoint, setStartPoint] = useState("");
  const [selected, setSelected] = useState("");
  const [branchCommits, setBranchCommits] = useState<GitCommit[]>([]);
  const [historyLoading, setHistoryLoading] = useState(false);
  // 「读不到提交历史」必须与「这条分支真的没有提交」分开渲染。
  // 原来只有 loading / 空 两态，于是 git.log 失败时列表区写的是"该分支没有可显示的
  // 提交记录" —— 一句关于**数据**的断言，而真相是**没读到**。这是本项目的红线
  // （`mobile-remote-agent` 那条"没有冲突"的教训同族）。提交详情那侧一直有 error 档，
  // 历史列表这里漏了，症状不对称。
  const [historyError, setHistoryError] = useState("");
  const [historyHasMore, setHistoryHasMore] = useState(false);
  const [historyQuery, setHistoryQuery] = useState("");
  const [pendingQuery, setPendingQuery] = useState("");
  const historyRequest = useRef(0);
  const prevSelectionRef = useRef("");
  const [selectedCommit, setSelectedCommit] = useState<GitCommit | null>(null);
  const [commitDetail, setCommitDetail] = useState<GitCommitDetail | null>(null);
  const [detailLoading, setDetailLoading] = useState(false);
  const [detailError, setDetailError] = useState("");
  const [selectedFile, setSelectedFile] = useState<GitCommitFile | null>(null);
  const [fileDiff, setFileDiff] = useState<GitDiff | null>(null);
  const [fileDiffLoading, setFileDiffLoading] = useState(false);
  const detailRequest = useRef(0);
  const fileDiffRequest = useRef(0);
  const local = branches.filter((b) => !b.remote);
  const remote = branches.filter((b) => b.remote);
  const busy = Boolean(mutating);
  const currentName = branches.find((b) => b.current)?.name || "";
  // 选中分支不存在时回退到当前分支，保证始终有一个分支处于选中态。
  const effectiveSelection = selected && branches.some((b) => b.name === selected) ? selected : currentName;
  const handleCreate = () => {
    if (!newName.trim()) return;
    requestCreateBranch(newName.trim(), startPoint.trim());
    setNewName("");
    setStartPoint("");
    setShowForm(false);
    setSelected(newName.trim());
  };
  // 分支切换（currentName 变化）后重置检索词，回到该分支第一页。
  useEffect(() => {
    if (prevSelectionRef.current === effectiveSelection) return;
    prevSelectionRef.current = effectiveSelection;
    setHistoryQuery("");
    setPendingQuery("");
  }, [effectiveSelection]);

  // 检索词防抖后再提交，避免每次按键都跑一遍 git log。
  useEffect(() => {
    const timer = window.setTimeout(() => setPendingQuery(historyQuery), 300);
    return () => window.clearTimeout(timer);
  }, [historyQuery]);

  // 按选中分支拉取提交历史第一页；分支或检索词变化后自动回到第一页。
  useEffect(() => {
    if (!projectID || !effectiveSelection) return;
    const requestID = ++historyRequest.current;
    setHistoryLoading(true);
    setHistoryError("");
    const params = new URLSearchParams({ ref: effectiveSelection, limit: String(HISTORY_PAGE_SIZE) });
    if (pendingQuery) params.set("q", pendingQuery);
    request<GitCommit[]>(withWorkspace(`/api/projects/${projectID}/git/log?${params.toString()}`))
      .then((commits) => { if (requestID === historyRequest.current) { setHistoryHasMore(commits.length === HISTORY_PAGE_SIZE); setBranchCommits(commits); } })
      // 落成**就地**的那一档（与提交详情同一种做法），不再往页面级错误条上抛：
      // 两处说同一件事时，用户关掉错误条之后剩下的那句就成了唯一的说法 —— 而它是假的。
      .catch((cause) => { if (requestID === historyRequest.current) { setHistoryHasMore(false); setBranchCommits([]); setHistoryError(cause instanceof Error ? cause.message : "无法读取该分支的历史"); } })
      .finally(() => { if (requestID === historyRequest.current) setHistoryLoading(false); });
    return () => { historyRequest.current++; };
    // withWorkspace 仅追加 conversationId，纳入依赖会导致每次渲染重复拉取。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [projectID, effectiveSelection, pendingQuery, request, fail]);

  // 加载更多：在当前已显示的提交之后继续取一页，相同检索词下追加。
  const loadMoreHistory = () => {
    if (!projectID || !effectiveSelection) return;
    const skip = branchCommits.length;
    const requestID = ++historyRequest.current;
    setHistoryLoading(true);
    setHistoryError("");
    const params = new URLSearchParams({ ref: effectiveSelection, limit: String(HISTORY_PAGE_SIZE), skip: String(skip) });
    if (pendingQuery) params.set("q", pendingQuery);
    request<GitCommit[]>(withWorkspace(`/api/projects/${projectID}/git/log?${params.toString()}`))
      .then((commits) => { if (requestID === historyRequest.current) { setHistoryHasMore(commits.length === HISTORY_PAGE_SIZE); setBranchCommits((prev) => [...prev, ...commits]); } })
      // 「加载更多」失败时列表还在，所以那一档显示在**列表下方**，不顶掉已有内容。
      .catch((cause) => { if (requestID === historyRequest.current) setHistoryError(cause instanceof Error ? cause.message : "无法读取该分支的历史"); })
      .finally(() => { if (requestID === historyRequest.current) setHistoryLoading(false); });
  };

  // 切换分支时退出提交详情，避免残留其它分支的详情数据。
  useEffect(() => {
    detailRequest.current++;
    fileDiffRequest.current++;
    setSelectedCommit(null);
    setCommitDetail(null);
    setDetailError("");
    setSelectedFile(null);
    setFileDiff(null);
  }, [effectiveSelection]);

  const openCommit = (commit: GitCommit) => {
    fileDiffRequest.current++;
    setSelectedFile(null);
    setFileDiff(null);
    if (selectedCommit?.oid === commit.oid) return;
    setSelectedCommit(commit);
    setCommitDetail(null);
    setDetailError("");
    const requestID = ++detailRequest.current;
    setDetailLoading(true);
    request<GitCommitDetail>(withWorkspace(`/api/projects/${projectID}/git/commits/${commit.oid}`))
      .then((detail) => { if (requestID === detailRequest.current) setCommitDetail(detail); })
      .catch((cause) => { if (requestID === detailRequest.current) setDetailError(cause instanceof Error ? cause.message : "无法读取提交详情"); })
      .finally(() => { if (requestID === detailRequest.current) setDetailLoading(false); });
  };

  const closeCommit = () => {
    detailRequest.current++;
    fileDiffRequest.current++;
    setSelectedCommit(null);
    setCommitDetail(null);
    setDetailError("");
    setSelectedFile(null);
    setFileDiff(null);
  };

  const openCommitFile = (file: GitCommitFile) => {
    if (!selectedCommit) return;
    setSelectedFile(file);
    const requestID = ++fileDiffRequest.current;
    setFileDiff(null);
    setFileDiffLoading(true);
    request<{ oid: string; path: string; content: string }>(withWorkspace(`/api/projects/${projectID}/git/commits/${selectedCommit.oid}/diff?path=${encodeURIComponent(file.path)}`))
      .then((payload) => { if (requestID === fileDiffRequest.current) setFileDiff({ path: payload.path, stage: "commit", content: payload.content }); })
      .catch((cause) => { if (requestID === fileDiffRequest.current) fail(cause instanceof Error ? cause.message : "无法读取提交差异"); })
      .finally(() => { if (requestID === fileDiffRequest.current) setFileDiffLoading(false); });
  };

  const closeCommitFile = () => {
    fileDiffRequest.current++;
    setSelectedFile(null);
    setFileDiff(null);
  };
  const branchRow = (branch: GitBranch, tag: ReactNode) => {
    const isSelected = branch.name === effectiveSelection;
    return <article key={branch.name} className={isSelected ? "selected" : ""} title={isSelected ? "已选中，右侧显示该分支历史" : "点击查看该分支历史"} tabIndex={0} onClick={() => setSelected(branch.name)} onKeyDown={(event) => { if (event.key === "Enter" || event.key === " ") { event.preventDefault(); setSelected(branch.name); } }}>{tag}<b>{branch.name}</b><small className="git-branch-upstream">{branch.upstream || (branch.remote ? "" : "无上游")}</small>{!branch.remote && !branch.current && <button type="button" className="git-branch-switch-btn" disabled={busy} onClick={(event) => { event.stopPropagation(); requestSwitchBranch(branch.name); }}>切换</button>}</article>;
  };
  if (branches.length === 0) return <div className="git-branches-view"><div className="git-empty">没有可显示的分支</div></div>;
  return <div className="git-branches-view">
    <div className="git-branch-toolbar">
      <button type="button" className="secondary" disabled={busy} onClick={() => setShowForm(!showForm)}>新建分支</button>
    </div>
    {showForm && <div className="git-branch-create">
      <input type="text" value={newName} onChange={(e) => setNewName(e.target.value)} placeholder="分支名" disabled={busy} className="git-branch-input" />
      <input type="text" value={startPoint} onChange={(e) => setStartPoint(e.target.value)} placeholder="起始点（可选，默认为 HEAD）" disabled={busy} className="git-branch-input" />
      <button type="button" className="primary" disabled={busy || !newName.trim()} onClick={handleCreate}>创建</button>
      <button type="button" className="secondary" disabled={busy} onClick={() => setShowForm(false)}>取消</button>
    </div>}
    <div className="git-branch-layout">
      <div className="git-branch-list-pane" aria-label="分支列表">
        {local.length > 0 && <div className="git-branches"><h3 className="git-branch-group-title">本地分支</h3>{local.map((branch) => branchRow(branch, <span className={branch.current ? "current" : ""}>{branch.current ? "当前" : "本地"}</span>))}</div>}
        {remote.length > 0 && <div className="git-branches"><h3 className="git-branch-group-title">远端分支</h3>{remote.map((branch) => branchRow(branch, <span>远端</span>))}</div>}
      </div>
      <div className="git-branch-history-pane" aria-label={selectedCommit ? `提交 ${selectedCommit.oid} 的详情` : `分支 ${effectiveSelection} 的提交历史`}>
        {selectedCommit ? <CommitDetailView commit={selectedCommit} detail={commitDetail} loading={detailLoading} error={detailError} selectedFile={selectedFile} fileDiff={fileDiff} fileDiffLoading={fileDiffLoading} onOpenFile={openCommitFile} onCloseFile={closeCommitFile} onBack={closeCommit} /> : <>
          <header className="git-branch-history-header"><h3>提交历史</h3><div className="git-history-search"><input type="text" value={historyQuery} placeholder="搜索提交信息…" aria-label="按提交信息搜索" onChange={(event) => setHistoryQuery(event.target.value)} />{historyQuery ? <button type="button" className="git-history-search-clear" title="清除搜索" aria-label="清除搜索" onClick={() => setHistoryQuery("")}>×</button> : null}</div><code className="git-branch-history-ref">{effectiveSelection}</code></header>
          <CommitHistory commits={branchCommits} loading={historyLoading} error={historyError} onSelect={openCommit} />
          {historyHasMore && !historyLoading ? <button type="button" className="git-history-load-more" onClick={loadMoreHistory}>加载更多提交</button> : null}
          {/* 「加载更多」失败：列表还在，所以只在下面补一行说明，不顶掉已读到的内容。 */}
          {historyError !== "" && branchCommits.length > 0 ? <p className="git-history-error-inline" role="status">{historyError}</p> : null}
        </>}
      </div>
    </div>
  </div>;
}

function operationStatusLabel(status: string): string { return ({ queued: "等待中", running: "进行中", succeeded: "已完成", failed: "失败", cancelled: "已取消", needs_attention: "需检查" })[status] || status; }

function operationTypeLabel(type: string): string { return ({ stage: "暂存", unstage: "取消暂存", stage_all: "全部暂存", unstage_all: "全部取消暂存", commit: "提交", commit_amend: "修改提交", discard_worktree: "撤销工作区改动", discard_all: "丢弃全部改动", fetch: "获取", pull: "拉取", push: "推送", create_branch: "创建分支", switch_branch: "切换分支", resolve_conflict: "解决冲突", conflict_abort: "中止操作", conflict_continue: "完成操作" })[type] || type; }

function Operations({ operations, loading = false }: { operations: GitOperation[]; loading?: boolean }) { if (loading && operations.length === 0) return <div className="git-empty">正在读取操作记录</div>; if (operations.length === 0) return <div className="git-empty">暂无操作记录</div>; return <div className="git-operations">{operations.map((operation) => <article key={operation.id}><span className={`git-operation-status ${operation.status}`}>{operationStatusLabel(operation.status)}</span><div><b>{operationTypeLabel(operation.type)}</b><p>{operation.requestSummary}</p>{operation.errorMessage ? <small>{operation.errorMessage}</small> : null}</div><time title={operation.finishedAt || operation.startedAt || operation.requestedAt}>{formatGitTime(operation.finishedAt || operation.startedAt || operation.requestedAt)}</time></article>)}</div>; }
