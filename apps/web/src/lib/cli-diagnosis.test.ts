import assert from "node:assert/strict";
import test from "node:test";
import {
  describeRepair,
  diagnoseMoment,
  diagnosisBadge,
  diagnosisEmptyText,
  diagnosisLabel,
  diagnosisMetaLine,
  diagnosisTone,
  isConclusiveDiagnosis,
  offeredRemedies,
  pathFactState,
  preflightNotes,
  resolvedIssues,
  resolvedSummary,
  skippedDiagnosisText,
} from "./cli-diagnosis";
import type { AgentDiagnosis, DiagnoseIssue, DiagnosePathFact } from "./cli-diagnosis";

function diagnosis(partial: Partial<AgentDiagnosis>): AgentDiagnosis {
  return {
    agentId: "claude-code",
    status: "ok",
    version: "",
    issues: [],
    paths: [],
    limitations: [],
    diagnosedAt: "2026-09-21T00:00:00Z",
    ...partial,
  };
}

// 这条是本组最要紧的：**"没查成"绝不能念成"没有问题"**。
// 它是本项目反复出现的那一族错（把读不到写成没有），而诊断结论是用户唯一
// 能读到"这台机器到底怎么样"的地方。
test("未知状态一律念成「检测未完成」，绝不回落到「没有问题」", () => {
  for (const status of ["unknown", "channel-failed", "", "something-new-from-a-newer-server"]) {
    assert.equal(diagnosisLabel(status), "检测未完成", `status=${status}`);
    assert.notEqual(diagnosisLabel(status), "没有问题");
  }
  assert.equal(diagnosisLabel("ok"), "没有问题");
});

test("五种真相各有各的说法，两两不同", () => {
  const statuses = ["ok", "broken", "not-installed", "unknown", "unsupported"];
  const labels = statuses.map(diagnosisLabel);
  assert.equal(new Set(labels).size, statuses.length, `文案重复：${labels.join(" / ")}`);
  // 每个状态一个专属观感；`ok` 与"没查成"必须不同档（前者绿、后者灰且虚线）。
  assert.equal(diagnosisTone("ok"), "ok");
  assert.equal(diagnosisTone("broken"), "bad");
  assert.equal(diagnosisTone("unknown"), "unknown");
  assert.notEqual(diagnosisTone("ok"), diagnosisTone("unknown"));
  // 认不出的状态要落到 unknown 这一档，而不是 ok。
  assert.equal(diagnosisTone("brand-new-status"), "unknown");
});

test("offeredRemedies 按 id 去重，且保持服务端给的顺序", () => {
  const report = diagnosis({
    status: "broken",
    issues: [
      {
        code: "active-package-broken",
        severity: "blocker",
        summary: "半装",
        evidence: ["…"],
        remedies: [
          { id: "restore-backup", label: "回滚", detail: "先回滚" },
          { id: "reinstall", label: "重装", detail: "再重装" },
        ],
      },
      {
        code: "command-shim-missing",
        severity: "warning",
        summary: "入口没了",
        evidence: ["…"],
        // 与上一条重复 reinstall，且顺序与 remedyOrder 不同 —— 界面**不重排**：
        // 执行顺序是服务端的决定。
        remedies: [
          { id: "rebuild-shim", label: "重建入口", detail: "本地重建" },
          { id: "reinstall", label: "重装", detail: "再重装" },
        ],
      },
    ],
  });
  assert.deepEqual(offeredRemedies(report).map((item) => item.id), ["restore-backup", "reinstall", "rebuild-shim"]);
});

// ⚠️ 这里**只断言文案**：成败（`RepairResult.success`）是服务端算的，界面直接消费它 ——
// 同一条规则不在两处各实现一遍。那条规则本身由 Go 侧用例钉（"skipped 不算失败"）。
test("describeRepair 有一条失败就指名是谁，且不许把成功的包装成修复完成", () => {
  const failed = describeRepair([
    { id: "restore-backup", label: "回滚", ok: true, detail: "已回滚到 2.1.216" },
    { id: "reinstall", label: "重装", ok: false, detail: "npm 退出码 1" },
  ]);
  assert.match(failed, /重装/);
  assert.match(failed, /npm 退出码 1/);
  // 失败时**不能**把成功的那些包装成"修复完成"。
  assert.doesNotMatch(failed, /已回滚到/);
});

test("describeRepair 成功时把每一步的结果都说出来", () => {
  const done = describeRepair([
    { id: "restore-backup", label: "回滚", ok: true, detail: "已回滚到 2.1.216" },
    { id: "rebuild-shim", label: "重建入口", ok: true, detail: "入口已重建" },
  ]);
  assert.match(done, /已回滚到 2\.1\.216/);
  assert.match(done, /入口已重建/);
  // 空结果（动作被服务端全部丢掉）也要有个说法，不能是空串。
  assert.notEqual(describeRepair([]), "");
});

test("pathFactState 把五种读数分开说，尤其「没查」不许念成结论", () => {
  const fact = (partial: Partial<DiagnosePathFact>): DiagnosePathFact => ({
    source: "effective",
    label: "",
    path: "/x",
    checked: true,
    exists: true,
    probed: true,
    works: true,
    version: "2.1.216",
    ...partial,
  });
  assert.equal(pathFactState(fact({ checked: false, exists: false, probed: false, works: false })), "未核对");
  assert.equal(pathFactState(fact({ exists: false, probed: false, works: false })), "不存在");
  assert.equal(pathFactState(fact({ probed: false, works: false, version: "" })), "存在，未实测");
  assert.equal(pathFactState(fact({ works: false })), "存在但跑不起来");
  assert.equal(pathFactState(fact({})), "可执行");
  // 五种说法两两不同 —— 合并任何两个都会让用户去处理一件不存在的事。
  const labels = [
    fact({ checked: false, exists: false, probed: false, works: false }),
    fact({ exists: false, probed: false, works: false }),
    fact({ probed: false, works: false, version: "" }),
    fact({ works: false }),
    fact({}),
  ].map(pathFactState);
  assert.equal(new Set(labels).size, labels.length, `文案重复：${labels.join(" / ")}`);
});

// 「没有问题」与「有问题」之间还有一档：**能用，但有几处会绊住你**。
// 只念 status 会让界面出现「没有问题 · 2 项症状」这种自相矛盾的一行。
test("diagnosisBadge 同时看结论与症状，ok 但有需要留意的就不念成没有问题", () => {
  const clean = diagnosisBadge(diagnosis({ status: "ok", issues: [] }));
  assert.equal(clean.label, "没有问题");
  assert.equal(clean.tone, "ok");

  const warned = diagnosisBadge(diagnosis({
    status: "ok",
    issues: [{ code: "command-shim-missing", severity: "warning", summary: "入口没了", evidence: [], remedies: [] }],
  }));
  assert.match(warned.label, /要留意/);
  assert.notEqual(warned.tone, "ok");
  assert.doesNotMatch(warned.label, /没有问题/);

  // info 是事实说明（例如"官方安装器装的，平台不接管升级"），不算"要留意"。
  const info = diagnosisBadge(diagnosis({
    status: "ok",
    issues: [{ code: "native-unmanaged", severity: "info", summary: "官方安装器", evidence: [], remedies: [] }],
  }));
  assert.equal(info.label, "没有问题");

  // 用不了的仍然是"发现问题"，不会被"要留意"盖过去。
  const broken = diagnosisBadge(diagnosis({
    status: "broken",
    issues: [{ code: "binary-broken", severity: "blocker", summary: "半装", evidence: [], remedies: [] }],
  }));
  assert.equal(broken.label, "发现问题");
  assert.equal(broken.tone, "bad");
});

// 「没执行」与「执行失败」是两件事。合并的后果：用户收到一句"修复失败"，
// 而实际症状可能已经修好了（服务端那边两者也只差一个 skipped 标志）。
test("describeRepair 把「没执行」与「执行失败」分开说", () => {
  const skipped = describeRepair([
    { id: "rebuild-shim", label: "重建入口", ok: true, detail: "入口已重建" },
    { id: "restore-backup", label: "回滚", ok: false, skipped: true, detail: "当前状态下这个动作不适用" },
  ]);
  assert.match(skipped, /入口已重建/);
  // 必须说出来有人没执行 —— 静默少跑一个动作比报失败更坏。
  assert.match(skipped, /另有 1 个动作没执行/);
  // 而且要说清**为什么**（确认框里承诺过这句话），并且跳过的理由不能混进
  // "做成了什么"那一栏（`ok` 为假的动作，它的 detail 是理由不是结果）。
  assert.match(skipped, /当前状态下这个动作不适用/);
  assert.doesNotMatch(skipped, /入口已重建；当前状态下/);

  // 真的执行失败时只说那一步、不说"另有"（与上面那条对照）。
  const failed = describeRepair([
    { id: "rebuild-shim", label: "重建入口", ok: false, detail: "写入口失败" },
    { id: "restore-backup", label: "回滚", ok: false, skipped: true },
  ]);
  assert.match(failed, /重建入口/);
  assert.doesNotMatch(failed, /另有/);
});

// 中止之后剩下的动作：只有数量还不够，必须指名是谁、为什么没跑。
test("describeRepair 把多个没执行的动作收敛成一句可读的话", () => {
  const many = describeRepair([
    { id: "restore-backup", label: "回滚", ok: true, detail: "已回滚到 2.1.216" },
    { id: "cleanup-interrupted", label: "清理残骸", ok: false, skipped: true, detail: "前一步没有成功，这个动作没有执行" },
    { id: "rebuild-shim", label: "重建入口", ok: false, skipped: true, detail: "当前状态下这个动作不适用" },
    { id: "reinstall", label: "重装", ok: false, skipped: true, detail: "前一步没有成功，这个动作没有执行" },
  ]);
  assert.match(many, /已回滚到 2\.1\.216/);
  assert.match(many, /另有 3 个动作没执行/);
  assert.match(many, /清理残骸/);
  assert.match(many, /另有 1 个/);
  // 收敛到 2 条理由，不能把一行撑爆。
  assert.doesNotMatch(many, /重装：前一步/);
});

function issue(code: string, summary: string): DiagnoseIssue {
  return { code, severity: "blocker", summary, evidence: [], remedies: [] };
}

// 「修复完成」这句话本身证明不了任何事（docs/43 §6.3）。差集才是证据。
test("resolvedIssues 用 code 当身份算差集", () => {
  const before = diagnosis({ issues: [issue("binary-broken", "文件在但跑不起来"), issue("record-stale", "登记位置已不存在")] });
  // 修复后 binary-broken 没了、record-stale 还在；外加一条新症状。
  const after = diagnosis({ issues: [issue("record-stale", "登记位置已不存在"), issue("runtime-too-old", "Node 太低")] });

  const resolved = resolvedIssues(before, after);
  assert.deepEqual(resolved.map((item) => item.code), ["binary-broken"]);
  // 还在的**不算已解决**，新出现的也不算 —— 只有真的消失的才算。
  assert.ok(!resolved.some((item) => item.code === "record-stale" || item.code === "runtime-too-old"));

  // 拿不到"修复前"就什么都别声称。
  assert.deepEqual(resolvedIssues(undefined, after), []);
});

// ⚠️ 这条是本组最要紧的一条：**"没查成"的那份报告不能拿来算差集**。
// 它在实际链路上真的发生过：修复期间维护位是服务端自己置的，若重跑诊断发生在释放之前，
// 「修复后」那份只带一条 maintenance-active（零症状）⇒ 差集把**所有**旧症状都判成
// "已解决"，修复失败也照说不误。那是把"没查"写成"已解决"。
test("resolvedIssues 在「修复后」没查成时什么都不声称", () => {
  const before = diagnosis({ issues: [issue("binary-broken", "文件在但跑不起来"), issue("record-stale", "登记位置已不存在")] });

  const maintenance = diagnosis({
    status: "unknown",
    issues: [{ code: "maintenance-active", severity: "info", summary: "正在安装或升级", evidence: [], remedies: [] }],
  });
  assert.deepEqual(resolvedIssues(before, maintenance), [], "没查成的那份不许当证据");
  assert.equal(resolvedSummary(resolvedIssues(before, maintenance)), "");

  // 通道坏掉那一档同样是"没查成"（认不出的状态也归 unknown）。
  assert.deepEqual(resolvedIssues(before, diagnosis({ status: "channel-failed", issues: [] })), []);
  // 而有结论的"零症状"才是真的"都好了"。
  const clean = diagnosis({ status: "ok", issues: [] });
  assert.equal(resolvedIssues(before, clean).length, 2);
  // 跨端"该环境不提供"是**有结论**的（它是确定的结论，不是没查成）。
  assert.equal(isConclusiveDiagnosis(diagnosis({ status: "unsupported" })), true);
  assert.equal(isConclusiveDiagnosis(diagnosis({ status: "unknown" })), false);
});

// 没查成的那份报告**也是零症状**（跨端就是这样），所以空态必须看结论。
test("diagnosisEmptyText 在没查成时不许说「没有发现任何症状」", () => {
  const conclusive = diagnosis({ status: "ok", issues: [] });
  assert.match(diagnosisEmptyText(conclusive), /没有发现任何症状/);
  const inconclusive = diagnosis({ status: "unknown", issues: [] });
  assert.doesNotMatch(diagnosisEmptyText(inconclusive), /没有发现任何症状/);
  assert.match(diagnosisEmptyText(inconclusive), /没有得出结论/);
});

// skipped 有两种成因：① 没什么可疑的（不必详查）；② 通道坏了（**根本没查**）。
// 说成同一句，通道坏掉时每个工具都会被盖章"当前没有可疑迹象"。
test("skippedDiagnosisText 分清「不必详查」与「根本没查」", () => {
  const ready = skippedDiagnosisText(true);
  const broken = skippedDiagnosisText(false);
  assert.match(ready, /没有可疑迹象/);
  assert.doesNotMatch(broken, /没有可疑迹象/);
  assert.match(broken, /当前不可用/);
  assert.notEqual(ready, broken);
});

test("preflightNotes 判据一致时只说一句，分歧时才补第二句", () => {
  assert.deepEqual(preflightNotes({ installOk: true, upgradeOk: true }), []);
  // 安装与升级共用同一句（服务端就是这么算的）⇒ 不重复说两遍。
  const shared = preflightNotes({ installOk: false, installReason: "没有可用的 npm", upgradeOk: false, upgradeReason: "没有可用的 npm" });
  assert.equal(shared.length, 1);
  assert.match(shared[0], /没有可用的 npm/);
  // 真分歧时两条都要说 —— 那正是这两个字段存在的理由。
  const split = preflightNotes({ installOk: false, installReason: "装不了", upgradeOk: false, upgradeReason: "升不了" });
  assert.equal(split.length, 2);
  assert.match(split[1], /升级也不行/);
  // 只有安装失败时不说升级那句（它没失败）。
  assert.equal(preflightNotes({ installOk: false, installReason: "装不了", upgradeOk: true }).length, 1);
});

test("diagnosisMetaLine 必须带上「什么时候测的」", () => {
  const line = diagnosisMetaLine(diagnosis({ version: "2.1.216", diagnosedAt: new Date().toISOString() }));
  assert.match(line, /实测版本 2\.1\.216/);
  assert.match(line, /诊断于 今天/);
  // 读不出时间时不许编 —— 但也不该把版本一起丢掉。
  const noTime = diagnosisMetaLine(diagnosis({ version: "2.1.216", diagnosedAt: "不是时间" }));
  assert.match(noTime, /实测版本/);
  assert.doesNotMatch(noTime, /诊断于/);
  assert.equal(diagnosisMetaLine(diagnosis({ version: "", diagnosedAt: "" })), "");
  // 老日期要带上月日（否则"上周那次"与"刚才那次"分不出来）。
  const old = diagnosisMetaLine(diagnosis({ version: "", diagnosedAt: "2020-03-04T09:07:00Z" }));
  assert.match(old, /3月4日/);
  assert.equal(diagnoseMoment(""), "");
  assert.equal(diagnoseMoment("乱写的"), "");
});

test("resolvedSummary 没有解决任何症状时返回空串（界面据此不渲染那一行）", () => {
  assert.equal(resolvedSummary([]), "");
  const one = resolvedSummary([issue("binary-broken", "文件在但跑不起来")]);
  assert.match(one, /已解决 1 项/);
  assert.match(one, /文件在但跑不起来/);
  // 很多项时收敛到 limit，不能把一行撑爆。
  const many = resolvedSummary([
    issue("a", "症状甲"), issue("b", "症状乙"), issue("c", "症状丙"), issue("d", "症状丁"),
  ], 2);
  assert.match(many, /已解决 4 项/);
  assert.match(many, /另有 2 项/);
  assert.doesNotMatch(many, /症状丙/);
});
