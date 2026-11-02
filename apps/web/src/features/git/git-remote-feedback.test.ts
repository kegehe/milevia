import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

import { remoteOpFailureTitle, remoteOpRunningText, remoteOpSuccessText, type GitSnapshot } from "./git-model.ts";

// —— 远端操作（获取 / 拉取 / 推送）的结果读数 ——
//
// 这组测试对着的是报告里的原话："推送、获取、拉取成功和失败都没有反馈"。
// 反馈本身没法在 SSR 里点出来（它要跑异步请求），所以这里分两层：
//   · **读数怎么算** —— 纯函数，直接断言数字；
//   · **有没有人调用它** —— 源码闸门，盯住"三类状态都落到工作台内部"这件事。

function sampleSnapshot(overrides: Partial<GitSnapshot["head"]> = {}): GitSnapshot {
  return {
    repositoryState: "ready",
    head: { oid: "abc123def456".padEnd(40, "0"), branch: "main", detached: false, upstream: "origin/main", ahead: 2, behind: 1, ...overrides },
    worktree: { staged: 0, modified: 0, untracked: 0, deleted: 0, renamed: 0, conflicted: 0 },
  };
}

test("远端操作的成功读数是数出来的：推送 / 拉取 / 获取各算各的", () => {
  const before = sampleSnapshot({ ahead: 3, behind: 2 });
  const after = sampleSnapshot({ ahead: 0, behind: 0 });

  // 推送：推掉的就是 ahead 减去的那个差。
  assert.equal(remoteOpSuccessText("push", "origin", "main", before, after), "已推送 3 个提交到 origin/main");
  // 拉取：拉到的是 before.behind 那些。
  assert.equal(remoteOpSuccessText("pull", "origin", "main", before, after), "已拉取 2 个提交（origin/main）");
  // 获取：远端前进了多少，就是 behind 变大的那个差。
  assert.equal(remoteOpSuccessText("fetch", "origin", "", before, sampleSnapshot({ ahead: 3, behind: 5 })), "获取完成：origin 上有 3 个新提交");
});

test("远端操作成功但什么都没变时也要说出结果，而不是沉默", () => {
  const synced = sampleSnapshot({ ahead: 0, behind: 0 });

  // 成功但什么都没变 —— 这恰恰是最需要说话的一种（"没变"和"没做"长得一样）。
  assert.equal(remoteOpSuccessText("push", "origin", "main", synced, synced), "已推送到 origin/main");
  assert.equal(remoteOpSuccessText("pull", "origin", "main", synced, synced), "已是最新（origin/main）");
  assert.equal(remoteOpSuccessText("fetch", "origin", "", synced, synced), "获取完成：当前分支没有新提交");

  // 还没对账（after 为 null）时给的是"做完了"的最小读数 —— 反馈必须先行，不能等对账。
  assert.equal(remoteOpSuccessText("push", "origin", "main", synced, null), "已推送到 origin/main");
  // ⚠️ 获取那一档**不能说"没有新提交"**：对账落地前我们根本不知道远端有没有前进，
  // 说了就等于把"还不知道"写成"没有"，紧接着它又会自己改口。
  assert.equal(remoteOpSuccessText("fetch", "origin", "", synced, null), "获取完成：已更新 origin 的远端引用");
});

test("拉取之后仍然落后时不说已是最新", () => {
  // 远端在拉取过程中又动了：报"已是最新"会骗用户以为已经同步上了。
  const before = sampleSnapshot({ ahead: 0, behind: 1 });
  const after = sampleSnapshot({ ahead: 0, behind: 2 });
  assert.equal(remoteOpSuccessText("pull", "origin", "main", before, after), "已拉取，但仍有 2 个提交未合入");
});

test("进行中与失败的文案要说清楚对象", () => {
  assert.equal(remoteOpRunningText("fetch", "origin", ""), "正在从 origin 获取远端更新…");
  assert.equal(remoteOpRunningText("pull", "origin", "main"), "正在从 origin 拉取 main…");
  assert.equal(remoteOpRunningText("push", "origin", "main"), "正在推送到 origin/main…");
  assert.equal(remoteOpFailureTitle("fetch"), "获取失败");
  assert.equal(remoteOpFailureTitle("pull"), "拉取失败");
  assert.equal(remoteOpFailureTitle("push"), "推送失败");
});

test("远端操作的三态都留下结果读数，失败不再走页面级红条", () => {
  const raw = readFileSync(new URL("./GitWorkbench.tsx", import.meta.url), "utf8");
  // 先剥注释再断言：这几串在注释里也原样出现过（说明"为什么要这么写"），
  // 不剥的话把对应代码整段注释掉照样能过。
  const source = raw.replace(/^\s*\/\/.*$/gm, "").replace(/\/\*[\s\S]*?\*\//g, "");
  const mutate = source.slice(source.indexOf("const mutate = async"), source.indexOf("const mutatePath ="));

  // 三类远端操作都要带上自己的身份 —— 没有它就说不出"推给谁、推了几个"。
  assert.match(source, /mutate\("fetch", "fetch", \{ remote \},[\s\S]*?\{ kind: "fetch", remote, branch: "" \}\)/);
  assert.match(source, /mutate\("pull", "pull", \{ remote, branch \},[\s\S]*?\{ kind: "pull", remote, branch \}\)/);
  assert.match(source, /mutate\("push", "push", \{ remote, branch, setUpstream \},[\s\S]*?\{ kind: "push", remote, branch \}\)/);

  // 进行中：操作一开始就把读数换成"正在对哪个远端做什么"。
  assert.match(mutate, /if \(remote\) setRemoteFeedback\(\{ kind: "running", op: remote\.kind, title: remoteOpRunningText\(remote\.kind, remote\.remote, remote\.branch\) \}\);/);
  // 成功：也要说话（远端操作成功的信号常常是"界面什么都没变"）。
  assert.match(mutate, /if \(remote\) setRemoteFeedback\(\{ kind: "success", op: remote\.kind, title: remoteOpSuccessText\(/);
  // 失败：落在工作台内部，且**不再**调 fail()。
  assert.match(mutate, /if \(remote\) setRemoteFeedback\(\{ kind: "error", op: remote\.kind, title: remoteOpFailureTitle\(remote\.kind\), detail: message \}\);\s*else fail\(message\);/);
  // 补数字时必须复核仍是成功态：等待期间用户又点了一次别的操作，就不该改这一条。
  assert.match(mutate, /setRemoteFeedback\(\(current\) => \(current\?\.kind === "success"/);
});

test("远端操作结束后读数不能停在进行中", () => {
  const raw = readFileSync(new URL("./GitWorkbench.tsx", import.meta.url), "utf8");
  const source = raw.replace(/^\s*\/\/.*$/gm, "").replace(/\/\*[\s\S]*?\*\//g, "");
  const mutate = source.slice(source.indexOf("const mutate = async"), source.indexOf("const mutatePath ="));

  // 「仓库状态已变化」那一支：服务端在**执行前**就拒了，这一趟已经结束。
  // 读数必须落到"失败"上 —— 停在"正在推送到 origin/main…"会让人以为推送还在跑，
  // 而它永远不会结束（实测过：那一支原本既没判成功也没判失败）。
  const staleBranch = mutate.slice(mutate.indexOf("isGitStateChangedError(cause)"), mutate.indexOf("} else if (mountedRef.current) {"));
  assert.match(staleBranch, /setRemoteFeedback\(\{ kind: "error", op: remote\.kind/);
  // 兜底：收尾时若读数还写着"进行中"，就把它收掉（任何新增的分支都受这条约束）。
  assert.match(mutate, /setRemoteFeedback\(\(current\) => \(current\?\.kind === "running" && current\.op === remote\.kind \? null : current\)\)/);
});

test("读数条的状态类名必须带命名空间，不能撞上全局的 .error", () => {
  const raw = readFileSync(new URL("./GitWorkbench.tsx", import.meta.url), "utf8");
  const source = raw.replace(/^\s*\/\/.*$/gm, "").replace(/\/\*[\s\S]*?\*\//g, "");

  // 全局有一条 .error（style.css）：它是**窗口右上角的固定定位红条**
  // （position: fixed; top: 16px; right: 16px; z-index: 13000）。
  // 读数条若也叫 error，就会被那条规则一并吃进去 —— 而它自己的规则没有覆盖
  // position/top/right，结果这条读数会脱离工作台、浮到窗口右上角。
  // （实测过：这么写时探针只加载 git.css，端到端全绿，真页面上却是错的。）
  assert.match(source, /className=\{`git-op-feedback git-op-feedback-\$\{remoteFeedback\.kind\}`\}/);
  assert.doesNotMatch(source, /className=\{`git-op-feedback \$\{/);

  // 样式这一侧同样不许用裸状态类。
  const css = readFileSync(new URL("../../git.css", import.meta.url), "utf8");
  assert.doesNotMatch(css, /\.git-op-feedback\.(error|success|running)\b/);
  assert.match(css, /\.git-op-feedback-error\b/);
});

test("结果读数渲染在工具栏下面，且失败原因就地显示在确认框里", () => {
  const raw = readFileSync(new URL("./GitWorkbench.tsx", import.meta.url), "utf8");
  const source = raw.replace(/^\s*\/\/.*$/gm, "").replace(/\/\*[\s\S]*?\*\//g, "");

  // 反馈条在工作台内部（不是页面级的红条），成功 / 进行中 / 失败各有各的语义。
  assert.match(source, /git-op-feedback/);
  assert.match(source, /remoteFeedback\.kind === "error" \? "alert" : "status"/);
  // 确认框里也要有一份：失败时框子不关，而工作台那条被模态遮罩挡着。
  assert.match(source, /const remoteError = confirmOp !== null && remoteFeedback\?\.kind === "error" && remoteFeedback\.op === confirmOp \? remoteFeedback : null;/);
  assert.match(source, /\{remoteError \? <div className="git-confirmation-error" role="alert">/);
  // 进行中按钮说在做什么，而不是笼统的"处理中"。
  assert.match(source, /const remoteBusy = busy && confirmOp !== null && remoteFeedback\?\.kind === "running" && remoteFeedback\.op === confirmOp;/);
  assert.match(source, /const confirmLabel = remoteBusy \? remoteFeedback\.title : busy \? "处理中"/);
});

// —— 「提交并推送」：两段串联的读数与失败语义 ——

test("the commit-and-push flow reports its two phases separately", () => {
  const raw = readFileSync(new URL("./GitWorkbench.tsx", import.meta.url), "utf8");
  const source = raw.replace(/^\s*\/\/.*$/gm, "").replace(/\/\*[\s\S]*?\*\//g, "");

  // 两段各自有"进行中"文案 —— 用户要看得见现在是在本地提交还是在跟远端通信。
  assert.match(source, /commitPushRunningText\("commit", remote, branch\)/);
  assert.match(source, /commitPushRunningText\("push", remote, branch\)/);

  // ⚠️ 令牌接力：第二段必须用**对账后重新签发**的那枚令牌。
  // 用第一段那枚（提交改动了仓库，它当场作废）会直接 409。
  const flow = source.slice(source.indexOf("const commitAndPush = async"), source.indexOf("const commitAmend = "));
  assert.match(flow, /snapshotRef\.current\?\.stateToken/);
  assert.match(flow, /const nextToken = beforePush\?\.stateToken/);
  assert.match(flow, /stateToken: nextToken/);

  // 推了几个提交要靠**前后两个读数做差**，不是拿"提交了一个"推算的。
  assert.match(flow, /commitPushPushedCount\(beforePush, snapshotRef\.current\)/);

  // 失败要按段说清"本地现在处于什么状态"。
  assert.match(flow, /commitPushFailureTitle\(phase, outcome\)/);
  assert.match(flow, /commitPushFailureDetail\(phase, outcome, /);
  // 服务端说"结果无法确认"时不能当成"确定失败"。
  assert.match(flow, /outcome = "unknown"/);
});

test("the push phase failure closes the confirmation so the commit is not made twice", () => {
  const raw = readFileSync(new URL("./GitWorkbench.tsx", import.meta.url), "utf8");
  const source = raw.replace(/^\s*\/\/.*$/gm, "").replace(/\/\*[\s\S]*?\*\//g, "");
  const flow = source.slice(source.indexOf("const commitAndPush = async"), source.indexOf("const commitAmend = "));

  // 推送段失败 = 提交**已经在本地**了。确认框必须收掉 ——
  // 留着它，用户再点一次「确认提交并推送」就会多出一个提交。
  assert.match(flow, /if \(phase === "push"\) \{[\s\S]{0,200}?setConfirmation\(null\)/);

  // 而提交段**确定**失败时不收确认框：用户改好提交信息就能就地重试。
  // （"确定"是关键词：结果不确定的那一支必须收 —— 那正是不能让人重试的情况。）
  const definiteFailure = flow.slice(flow.indexOf('if (created.status !== "succeeded")'), flow.indexOf("setHistoryRevision("));
  assert.ok(definiteFailure.length > 0);
  assert.doesNotMatch(definiteFailure, /setConfirmation\(null\)/);
  // 不确定的那一支（服务端说"没法核实"）反而要收：提交可能已经建成了。
  assert.match(flow, /if \(created\.status === "needs_attention"\) \{[\s\S]{0,200}?setConfirmation\(null\)/);
});

test("the commit-and-push readout is a first-class operation, not a piggyback on push", () => {
  const raw = readFileSync(new URL("./GitWorkbench.tsx", import.meta.url), "utf8");
  const source = raw.replace(/^\s*\/\/.*$/gm, "").replace(/\/\*[\s\S]*?\*\//g, "");

  // 它有自己的 op 标识 —— 否则确认框里那条失败读数会被别的操作的读数顶掉。
  assert.match(source, /op: "commit-push"/);
  // 确认框要认这个 op（否则两段失败的详情不会显示在框里）。
  assert.match(source, /isCommitPush \? "commit-push" : null/);
  // 走的是现成的两个端点，不新增服务端组合端点 ——
  // 手机端中继的白名单是显式枚举的（remote_git.go），新增端点等于给手机端白送写能力。
  assert.match(source, /git\/commits`/);
  assert.match(source, /git\/push`/);
});
