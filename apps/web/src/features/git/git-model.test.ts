import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { createElement } from "react";
import { renderToString } from "react-dom/server";

import { groupChanges, isMergeCommit, type GitCommit, type GitOperation, type GitSnapshot } from "./git-model.ts";
import { CommitHistory, CommitPanel, GitBar, GitWorkbench, MAX_COMMIT_SUBJECT_LENGTH, commitSubjectLength, parseDiffContent } from "./GitWorkbench.tsx";

test("keeps staged and worktree entries for the same path distinct", () => {
  const grouped = groupChanges([
    { path: "app.go", staged: true, modified: true, untracked: false, deleted: false, renamed: false, conflicted: false },
    { path: "new.txt", staged: false, modified: false, untracked: true, deleted: false, renamed: false, conflicted: false },
  ]);

  assert.deepEqual(grouped.staged.map((change) => change.path), ["app.go"]);
  assert.deepEqual(grouped.worktree.map((change) => change.path), ["app.go", "new.txt"]);
});

test("treats a commit with missing parents as a non-merge commit", () => {
  // 根提交没有父提交，服务端可能把 parents 序列化成 null。直接读 .length 会抛错，
  // 而 Git 工作台没有错误边界，一次抛错会让整页变空白。
  assert.equal(isMergeCommit({ parents: null as unknown as string[] }), false);
  assert.equal(isMergeCommit({ parents: [] }), false);
  assert.equal(isMergeCommit({ parents: ["abc"] }), false);
  assert.equal(isMergeCommit({ parents: ["abc", "def"] }), true);
});

test("never reads commit.parents.length directly", () => {
  // 回归闸门：根提交的 parents 可能是 null，只有 isMergeCommit 能安全判断合并提交。
  // 谁把 .parents.length 写回来，分支页就会再次整页空白。
  const source = readFileSync(new URL("./GitWorkbench.tsx", import.meta.url), "utf8");

  assert.doesNotMatch(source, /parents\.length/);
  assert.match(source, /isMergeCommit\(commit\)/);
});

test("renders unified diffs with aligned old and new line numbers", () => {
  const lines = parseDiffContent("diff --git a/readme.md b/readme.md\n@@ -4,2 +4,2 @@\n old\n-old value\n+new value\n tail\n");

  assert.deepEqual(lines.map(({ kind, oldLine, newLine, content }) => ({ kind, oldLine, newLine, content })), [
    { kind: "meta", oldLine: undefined, newLine: undefined, content: "diff --git a/readme.md b/readme.md" },
    { kind: "hunk", oldLine: undefined, newLine: undefined, content: "@@ -4,2 +4,2 @@" },
    { kind: "context", oldLine: 4, newLine: 4, content: "old" },
    { kind: "removed", oldLine: 5, newLine: undefined, content: "old value" },
    { kind: "added", oldLine: undefined, newLine: 5, content: "new value" },
    { kind: "context", oldLine: 6, newLine: 6, content: "tail" },
  ]);
});

test("renders untracked file contents as numbered code lines", () => {
  const lines = parseDiffContent("first line\nsecond line\n");

  assert.deepEqual(lines.map(({ oldLine, newLine, content }) => ({ oldLine, newLine, content })), [
    { oldLine: 1, newLine: 1, content: "first line" },
    { oldLine: 2, newLine: 2, content: "second line" },
  ]);
});

test("closes a diff inside the workbench without navigating away from the project", () => {
  const source = readFileSync(new URL("./GitWorkbench.tsx", import.meta.url), "utf8");

  assert.doesNotMatch(source, /window\.history\.back\(\)/);
  assert.match(source, /const closeDiff = \(\) => \{ diffRequest\.current\+\+; setSelectedDiff\(null\); \};/);
});

test("keeps a late diff response from replacing the most recently selected file", () => {
  const source = readFileSync(new URL("./GitWorkbench.tsx", import.meta.url), "utf8");

  assert.match(source, /const diffRequest = useRef\(0\);/);
  assert.match(source, /const requestID = \+\+diffRequest\.current;/);
  assert.match(source, /if \(requestID === diffRequest\.current && mountedRef\.current\) setSelectedDiff\(diff\);/);
});

test("closes destructive confirmations when an operation result is uncertain", () => {
  const source = readFileSync(new URL("./GitWorkbench.tsx", import.meta.url), "utf8");

  assert.match(source, /if \(result\.status === "needs_attention"\) \{\s+closeDiff\(\);\s+setConfirmation\(null\);\s+\}/);
});

test("reports an unavailable Git state instead of silently ignoring a mutation", () => {
  const source = readFileSync(new URL("./GitWorkbench.tsx", import.meta.url), "utf8");

  assert.match(source, /if \(!snapshot\?\.stateToken\) \{\s+fail\("Git 状态尚未准备完成，请刷新后重试"\);\s+return;\s+\}/);
  // 这里原本还断言了失败分支那句重读调用（`void reload(true).catch(...)`）。现在写操作的收尾
  // 分了两段（成功 await / 失败 void），断言整体搬到下面那条测试里 —— 那条会剥注释、按分支判，
  // 光"字符串在文件里出现过"是拦不住"把代码注释掉"的。
});

test("keeps the workbench a compact 2-tab layout with a persistent status bar", () => {
  const source = readFileSync(new URL("./GitWorkbench.tsx", import.meta.url), "utf8");

  assert.match(source, /type Tab = "changes" \| "branches";/);
  assert.doesNotMatch(source, /"overview"|"operations"/);
  assert.match(source, /function GitBar\(/);
  assert.match(source, /function Branches\(/);
});

test("closes the operations popover via ref.contains outside-click instead of a blocking backdrop", () => {
  const source = readFileSync(new URL("./GitWorkbench.tsx", import.meta.url), "utf8");

  assert.match(source, /barRef\.current\?\.contains\(target\)/);
  assert.match(source, /if \(event\.key === "Escape"\) setOpsOpen\(false\);/);
  assert.doesNotMatch(source, /git-ops-backdrop/);
});

const noop = () => undefined;

function sampleSnapshot(overrides: Partial<GitSnapshot["head"]> = {}): GitSnapshot {
  return {
    repositoryState: "ready",
    head: { oid: "abc123def456", branch: "feature/hello", detached: false, upstream: "origin/feature/hello", ahead: 2, behind: 1, ...overrides },
    worktree: { staged: 1, modified: 2, untracked: 3, deleted: 0, renamed: 0, conflicted: 0 },
  };
}

test("ssr smoke: workbench initial render executes without throwing", () => {
  const html = renderToString(createElement(GitWorkbench, {
    projectID: "p1",
    request: async () => ({}) as never,
    fail: () => undefined,
    active: true,
  }));

  assert.match(html, /git-workbench/);
  assert.match(html, /正在读取仓库状态/);
});

test("ssr smoke: commit history renders a root commit whose parents are null", () => {
  // 服务端对根提交返回 parents:null。这条渲染路径曾经直接读 .length 抛错，
  // 把整个 Git 工作台打成空白；这里断言它不再抛错、也不误报"合并"徽标。
  const commits: GitCommit[] = [
    { oid: "c9283c7c1234567890abcdef1234567890abcd", parents: null as unknown as string[], subject: "首个提交", author: "tangmaoke", authoredAt: "2026-06-05T14:33:35Z" },
    { oid: "f26ce29c1234567890abcdef1234567890abcd", parents: ["c9283c7c1234567890abcdef1234567890abcd"], subject: "桌面端", author: "tangmaoke", authoredAt: "2026-09-09T09:02:57Z" },
  ];

  const html = renderToString(createElement(CommitHistory, { commits, loading: false, onSelect: noop }));

  assert.match(html, /首个提交/);
  assert.match(html, /桌面端/);
  assert.doesNotMatch(html, /git-commit-merge-badge/);
});

test("重读期间不复用上一份读数：loading 档一律显示占位", () => {
  const source = readFileSync(new URL("./GitWorkbench.tsx", import.meta.url), "utf8");

  // 为什么不能写成 `loading && !snapshot`：重读的触发之一就是**工作区身份变了**
  // （projectID / conversationId / request 换人 —— 手机端还会因为电脑那边切了会话而被动换），
  // 那时手上的 snapshot 是**上一个工作区**的。拿它顶上，用户看到的是 A 的变更列表、
  // 点下去可能写进 B。`loading` 只由首屏那一档置位（后台对账一概不置），
  // 所以它本身就等于"屏幕上这份数据不属于当前工作区"，不必再加 snapshot 非空的限定。
  assert.match(source, /\{loading \? <GitSkeleton \/>/);
  assert.match(source, /\{loading \? <><span className="git-bar-label">/);
  // 先剥注释再查这条：上面的 ⚠️ 注释为了说明"为什么不能那么写"，把那句原样写了一遍
  // （同一个形状在 mobile-remote-agent.test.mjs 里也用过一次）。
  const code = source.replace(/^\s*\/\/.*$/gm, "").replace(/\/\*[\s\S]*?\*\//g, "");
  assert.doesNotMatch(code, /loading && !snapshot/);
});

test("写操作：反馈不等对账、成功才等令牌、失败不许把 busy 扣到对账结束", () => {
  const raw = readFileSync(new URL("./GitWorkbench.tsx", import.meta.url), "utf8");
  // ⚠️ **先剥注释再断言**：这几串在注释里也原样出现过（说明"为什么要这么写"），不剥的话
  // 把对应代码整段注释掉照样能过 —— 审查者用变异测试正是这么骗过去的（改完 153 条全绿）。
  const source = raw.replace(/^\s*\/\/.*$/gm, "").replace(/\/\*[\s\S]*?\*\//g, "");
  const mutate = source.slice(source.indexOf("const mutate = async"), source.indexOf("const mutatePath ="));

  const feedback = mutate.indexOf("success?.();");
  const successFrom = mutate.indexOf("if (wrote) {");
  const failureFrom = mutate.indexOf("} else {");
  const clearBusy = mutate.indexOf('setMutating("")');
  assert.ok(feedback > 0 && successFrom > feedback, "反馈（关确认框 / 清提交信息 / 退差异面板）必须在两段对账之前落地");
  assert.ok(failureFrom > successFrom && clearBusy > failureFrom, "两段对账之后才是清 busy");

  const successBranch = mutate.slice(successFrom, failureFrom);
  const failureBranch = mutate.slice(failureFrom, clearBusy);

  // 成功：await 对账（写操作作废了手上的 stateToken，新令牌要等 summary 回来）；
  // 而且要带 `.catch`：万一 reload 抛出，"清 busy 那句不执行 ⇒ 按钮永远禁着"不该是后果。
  assert.match(successBranch, /await reload\("reconcile", options\)\.catch\(\(\) => undefined\);/);
  assert.doesNotMatch(successBranch, /void reload\("reconcile", options\);/);
  // 失败：反过来 —— 发对账但**不等**它（写请求超时后确认框还开着、三颗按钮全是 disabled={busy}，
  // 等对账就是让用户没有任何能按的东西）。两段对调会让这两条同时红。
  assert.match(failureBranch, /void reload\("reconcile", options\);/);
  assert.doesNotMatch(failureBranch, /await reload\(/);
});

test("ssr smoke: GitBar loaded state renders branch, sync badge and actions", () => {
  const html = renderToString(createElement(GitBar, {
    snapshot: sampleSnapshot(),
    loading: false,
    mutating: "",
    refreshing: false,
    operations: [],
    projectID: "p1",
    request: async () => ([] as never),
    fail: () => undefined,
    requestRefresh: noop,
    requestFetch: noop,
    requestPull: noop,
    requestPush: noop,
  }));

  assert.match(html, /feature\/hello/);
  assert.match(html, /领先 2 · 落后 1/);
  assert.match(html, /abc123de/); // 短 OID（前 8 位）
  assert.match(html, />拉取</);
  assert.match(html, />推送</);
  assert.doesNotMatch(html, /git-ops-popover-body/); // 关闭时不渲染 popover 本体
});

test("ssr smoke: GitBar shows muted badge when no upstream and red dot when failed ops exist", () => {
  const noUpstream = renderToString(createElement(GitBar, {
    snapshot: sampleSnapshot({ upstream: "", ahead: 0, behind: 0 }),
    loading: false,
    mutating: "",
    refreshing: false,
    operations: [],
    projectID: "p1",
    request: async () => ([] as never),
    fail: () => undefined,
    requestRefresh: noop,
    requestFetch: noop,
    requestPull: noop,
    requestPush: noop,
  }));
  assert.match(noUpstream, /未跟踪上游/);
  assert.match(noUpstream, /git-sync-badge muted/);

  const ops: GitOperation[] = [{
    id: "op-1", projectId: "p1", type: "push", status: "failed", requestSummary: "push main",
    beforeState: "{}", afterState: "{}", errorMessage: "rejected", requestedAt: "2026-09-01T00:00:00Z",
  }];
  const withDot = renderToString(createElement(GitBar, {
    snapshot: sampleSnapshot(),
    loading: false,
    mutating: "",
    refreshing: false,
    operations: ops,
    projectID: "p1",
    request: async () => ([] as never),
    fail: () => undefined,
    requestRefresh: noop,
    requestFetch: noop,
    requestPull: noop,
    requestPush: noop,
  }));
  assert.match(withDot, /git-ops-trigger attention/);
  assert.match(withDot, /git-ops-dot/);
});

// 取某个按钮自身的 HTML，用来判断它是否被禁用。
// 按 class 定位而不是按文案：「提交」是「修改最近一次提交」的子串，
// 按文案写正则很容易挑中旁边那个按钮，断言就成了空转。
function buttonHTML(html: string, className: string): string {
  const match = html.match(new RegExp(`<button[^>]*class="[^"]*\\b${className}\\b[^"]*"[^>]*>[^<]*</button>`));
  assert.ok(match, `带 class="${className}" 的按钮没有渲染出来`);
  return match[0];
}

test("counts the commit subject by code points, exactly like the server", () => {
  // 服务端用 utf8.RuneCountInString 数首行。前端若改用 .length（UTF-16 码元），
  // 一个 emoji 会被算成 2，于是按钮比后端更早变灰，把能提交的信息拦下来。
  // 🚀 是增补平面字符，UTF-16 里占两个码元。
  const emoji = "🚀".repeat(40);
  assert.equal(emoji.length, 80);
  assert.equal(commitSubjectLength(emoji), 40);

  // 只看首行：第二行再长也不参与校验。
  assert.equal(commitSubjectLength(`${emoji}\n${"x".repeat(500)}`), 40);
  assert.equal(commitSubjectLength(""), 0);
});

test("measures the first line the server will see, not the raw first line", () => {
  // 服务端先 strings.TrimSpace 再取首行。以空行开头的提交信息若按原始首行算，
  // 前端会看到空串、判定合法放行，后端却数出 73 个字回 400 —— 变灰的毛病
  // 会以相反的方向复发：按钮能点，点完报错。
  const leadingBlankLines = `\n\n${"x".repeat(MAX_COMMIT_SUBJECT_LENGTH + 1)}`;
  assert.equal(commitSubjectLength(leadingBlankLines), MAX_COMMIT_SUBJECT_LENGTH + 1);
  assert.match(buttonHTML(renderCommitPanel(leadingBlankLines), "primary"), /disabled/);

  // trim 掉的只是整条信息的首尾空白，中间的换行照旧把正文和首行隔开。
  assert.equal(commitSubjectLength(`\n短首行\n${"y".repeat(200)}  \n`), 3);
  // 首行的前导缩进也在 trim 范围内，和后端一样不算进长度。
  assert.equal(commitSubjectLength(`  ${"x".repeat(70)}`), 70);
});

// 渲染提交面板并清掉 React 在相邻文本节点之间插入的 <!-- -->，便于直接比对文案。
function renderCommitPanel(value: string, stagedCount = 1): string {
  return renderToString(createElement(CommitPanel, { stagedCount, value, setValue: noop, open: noop, openAmend: noop, disabled: false })).replaceAll("<!-- -->", "");
}

test("explains why the commit button is disabled when the subject is too long", () => {
  const html = renderCommitPanel("x".repeat(MAX_COMMIT_SUBJECT_LENGTH + 1), 2);

  // 回归闸门：按钮变灰的同时必须给出原因，否则用户只会以为按钮坏了。
  assert.match(buttonHTML(html, "primary"), /disabled/);
  assert.match(html, /git-commit-subject-warning/);
  assert.match(html, /role="alert"/);
  assert.match(html, /首行最多 72 个字符/);
  assert.match(html, /首行 73\/72/);
  assert.match(html, /git-commit-subject-count over/);
  // 超长时「修改最近一次提交」同样过不了服务端校验，不能放行。
  assert.match(buttonHTML(html, "git-amend-btn"), /disabled/);
});

test("keeps the commit button usable at exactly the limit", () => {
  const atLimit = renderCommitPanel("x".repeat(MAX_COMMIT_SUBJECT_LENGTH));
  assert.doesNotMatch(buttonHTML(atLimit, "primary"), /disabled/);
  assert.doesNotMatch(atLimit, /git-commit-subject-warning/);

  // 只有首行受 72 字符限制：73 字符的首行换成"短首行 + 长正文"后就该恢复可提交。
  const withBody = renderCommitPanel(`短首行\n${"y".repeat(200)}`);
  assert.doesNotMatch(buttonHTML(withBody, "primary"), /disabled/);
  assert.match(withBody, /首行 3\/72/);
});

test("shows the subject limit before the user can hit it", () => {
  // 这一条针对报告里的「无声」：规矩要在用户动手写之前就看得见，
  // 而不是等写超了才凭空冒出来。
  const empty = renderCommitPanel("", 2);
  assert.match(empty, /首行 0\/72/);
  assert.doesNotMatch(empty, /git-commit-subject-count over/);
  assert.doesNotMatch(empty, /git-commit-subject-warning/);

  // 没有已暂存文件时输入框是禁用的，此时不该再挂一个 0/72 的计数器。
  assert.doesNotMatch(renderCommitPanel("", 0), /git-commit-subject-count/);
});

test("never counts the subject with UTF-16 length again", () => {
  // 回归闸门：首行长度只能由 commitSubjectLength 一处算出（后端是 TrimSpace 后
  // utf8.RuneCountInString，见 git_operations.go 的 gitCommit / gitAmendCommit）。
  // 数的是"取首行"这个动作出现的次数，而不是某种具体写法 —— 只匹配 `[0].length`
  // 的话，套一层 Array.from 或先赋给临时变量再 .length 都能绕过去。
  // 片段里含反斜杠转义，必须用 String.raw：写成正则时 \n 会被当成真的换行符，
  // 断言就永远成立，闸门空转。
  const source = readFileSync(new URL("./GitWorkbench.tsx", import.meta.url), "utf8");
  assert.equal(source.split(String.raw`split("\n")[0]`).length - 1, 1);
  assert.match(source, /commitSubjectLength\(/);
});
