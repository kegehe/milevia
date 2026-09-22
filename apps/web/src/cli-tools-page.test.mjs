import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

// 管理页的几条硬要求（docs/42 §8.2）：
//   1. "正在读取 / 无法检测 / 真的没有"三档必须分开渲染（工具目录本身也是三态）；
//   2. "不可安装"的各档理由文案各不相同；
//   3. 判据全部来自服务端，页面不重判一遍。
// 这些都属于"合并了也不会报错，但用户会被支去做错事"的类型，所以用源码断言钉住。
//
// **断言前先剥注释**：注释里出现同一个字符串会让 doesNotMatch 空转，也会让 match
// 在"代码已经改坏、注释还留着"时假绿。

const raw = await readFile(new URL("./pages/CliToolsPage.tsx", import.meta.url), "utf8");
const css = await readFile(new URL("./pages/cli-tools.css", import.meta.url), "utf8");
const app = await readFile(new URL("./App.tsx", import.meta.url), "utf8");
const dashboard = await readFile(new URL("./pages/DashboardPage.tsx", import.meta.url), "utf8");
const model = await readFile(new URL("./lib/cli-diagnosis.ts", import.meta.url), "utf8");

function stripComments(source) {
  return source.replace(/\/\*[\s\S]*?\*\//g, "").replace(/^\s*\/\/.*$/gm, "");
}

const code = stripComments(raw);

test("工具清单来自服务端目录，页面里不写死工具 ID", () => {
  // 页面遍历的是 registry 的目录（连 loaded / error 一起拿：只取 entries 的话，
  // "还没读到"与"读失败"都会渲染成"一个工具都没有"）。
  assert.match(code, /useAgentCatalogState\(\)/);
  assert.match(code, /catalog\.entries\.map\(\(entry\) => \{/);
  for (const forbidden of ['"claude-code"', '"codex"']) {
    assert.ok(!code.includes(forbidden), `页面里出现了写死的工具 ID ${forbidden}`);
  }
});

test("三档状态分开渲染，且「无法检测」不说成「未安装」", () => {
  assert.match(code, /viewState === "loading"/);
  // 通道失败：独立分支、独立文案，并明确否认"未安装"。
  assert.match(code, /viewState === "ready" && view && !view\.probeOk/);
  assert.match(code, /这不是"工具未安装"/);
  // 工具目录自己也是三态：读失败 / 还没读到 / 真的没有。
  assert.match(code, /catalog\.error/);
  assert.match(code, /!catalog\.loaded/);
  assert.match(code, /已读到工具目录，但里面没有工具/);
  assert.match(code, /正在读取工具目录…/);
  // 反面：不许把"读不到"直接渲染成工具的"未安装"徽标。
  assert.doesNotMatch(code, /probeError[^\n]*未安装/);
});

test("能不能安装的判据来自服务端，页面不重判一遍", () => {
  // meetsMinimumFor 是服务端逐工具算好的"当前运行时够不够"。页面必须消费它 ——
  // 只看 runtime.installed 的话，Node 16（装了但太低）与 npm 缺失这两种情况都会
  // 亮出"安装"按钮，而服务端的闸门会拒（点了必失败）。
  assert.match(code, /meetsMinimumFor\?\.includes\(entry\.id\)/);
  assert.match(code, /Boolean\(runtime\?\.npmVersion\)/);
  assert.match(code, /canInstallAgent\(item, entry\)/);
  // 反面：安装按钮不许再只看 runtime.installed。
  assert.doesNotMatch(code, /item\?\.installSupported && runtime\?\.installed && <button/);
});

test("「不可安装」的理由来自服务端，页面只在运行时那一档自己说", () => {
  // ① 该环境不提供该工具 / ② 未授权 —— 都由服务端的 installBlockedReason 下发。
  assert.match(code, /item\?\.installBlockedReason/);
  // ③ 运行时缺失/过低/npm 不可用：这三档页面自己说得清（它手上有 runtime 状态）。
  assert.match(code, /需要先安装 Node\.js 运行时/);
  assert.match(code, /随包的 npm 不可用/);
  assert.match(code, /请先升级运行时/);
  // 未授权走服务端显式下发的判据，**不是**让界面去认理由文案里有没有"授权"两个字。
  assert.doesNotMatch(code, /installBlockedReason\?\.(includes|match)/);
});

test("不能应用内升级时给手动命令，不给出点了必失败的按钮", () => {
  // 升级那一档要过两个条件：服务端说能自动升级（autoUpdatable），且不需要授权。
  assert.match(code, /hasUpdate && item\.autoUpdatable && !item\.upgradeNeedsGrant && item\.operation !== "running" && <button/);
  assert.match(code, /hasUpdate && !item\.autoUpdatable && <span/);
  assert.match(code, /需在目标环境手动执行 \{entry\.commandName\} update/);
  // 两条提示互斥："平台不支持升级"与"授权还没给"是两回事，同时出现用户不知道该做哪个。
  assert.match(code, /hasUpdate && item\.autoUpdatable && item\.upgradeNeedsGrant && <span/);
});

test("有任务在进行时不给操作入口", () => {
  // operation=running 时服务端会返回 409，界面不该亮出按钮。
  assert.match(code, /operation !== "running"/);
  assert.match(code, /该工具上已有任务在进行/);
});

test("未授权时不给安装/升级按钮，且授权入口只给一处", () => {
  // 跨端安装会在别人的机器上执行 npm 并落一整套运行时，未授权时服务端会 403。
  assert.match(code, /\{!view\.remoteInstallAllowed && <button/);
  assert.match(code, /view\.remoteInstallAllowed && runtime\?\.installSupported && !runtime\.installed && <button/);
  // 升级那一档的判据是服务端下发的 upgradeNeedsGrant，不是前端自己拼安装方式字符串。
  assert.match(code, /item\.upgradeNeedsGrant/);
  assert.doesNotMatch(code, /installKindUsed === "npm-global/);
  // 授权入口只给一处（运行时卡片）：每个工具再各来一个同义按钮，
  // 未授权的机器上会出现 N+1 个"允许在此主机安装"。
  assert.equal((code.match(/允许在此主机安装/g) ?? []).length, 1);
});

test("安装与升级都要先确认，且确认框里点明目标环境", () => {
  assert.match(code, /pending && <div className="backdrop"/);
  assert.match(code, /目标环境：<b>\{environmentLabel\}<\/b>/);
  assert.match(code, /平台自己的工具链目录/);
  assert.match(code, /不需要管理员权限/);
  assert.match(code, /校验官方 SHA256/);
});

test("路由与首页入口都已接上", () => {
  assert.match(app, /import CliToolsPage from "\.\/pages\/CliToolsPage";/);
  assert.match(app, /<Route path="\/cli-tools" element=\{<CliToolsPage \/>\} \/>/);
  assert.match(dashboard, /navigate\("\/cli-tools"\)/);
  assert.match(dashboard, /<CliToolsIcon \/>/);
  assert.match(dashboard, /function CliToolsIcon\(\)/);
});

test("样式：三档徽标各有独立观感，且窄屏下动作按钮铺满", () => {
  assert.match(css, /\.cli-tools-badge\.ready \{/);
  assert.match(css, /\.cli-tools-badge\.pending \{/);
  assert.match(css, /\.cli-tools-badge\.missing \{/);
  // "无法检测"用琥珀色（可操作的是环境），与"未安装"的灰不同。
  assert.match(css, /\.cli-tools-probe \{ border-color: #f0d9a8/);
  assert.match(css, /@media \(max-width: 680px\)[\s\S]*?\.cli-tools-actions \{ flex-direction: column;/);
});

// ── 故障诊断（docs/43） ────────────────────────────────────────────────────

test("诊断结论的文案只有一份来源，页面不自己判定", () => {
  // 结论怎么念（含"认不出的状态按检测未完成说"）全部在 lib/cli-diagnosis.ts 里，
  // 因为那是**能写反的判据**。页面只调它 —— 页面的文案表必然与服务端漂移。
  assert.match(model, /export function diagnosisLabel/);
  assert.match(model, /export function diagnosisTone/);
  // 兜底方向必须是"检测未完成"，**不能是"没有问题"** —— 那是这套文案里最要紧的一句。
  assert.match(model, /return DIAGNOSIS_LABELS\[status\] \?\? "检测未完成";/);
  assert.match(code, /from "\.\.\/lib\/cli-diagnosis"/);
  // 徽标读的是 diagnosisBadge（**同时**看结论与症状），不是单独念 status ——
  // 否则会出现「没有问题 · 2 项症状」这种自相矛盾的一行（见下一条）。
  assert.match(code, /diagnosisBadge\(diagnosis\)/);
  assert.doesNotMatch(code, /diagnosisLabel\(diagnosis\.status\)/);
  // 反面：页面里不许再有一份标签表。
  assert.doesNotMatch(code, /DIAGNOSIS_LABELS/);
});

test("徽标同时看结论与症状，且「未实测 / 未核对」不许念成结论", () => {
  // 判据在模型里（能写反的东西一律放那里），页面只消费。
  assert.match(model, /export function diagnosisBadge/);
  assert.match(model, /能用，有 \$\{noteworthy\} 项要留意/);
  // 路径事实那一列有三档"没有结论"，必须分开说。
  assert.match(model, /if \(!fact\.checked\) return "未核对";/);
  assert.match(model, /if \(!fact\.probed\) return "存在，未实测";/);
  assert.match(model, /export type DiagnosePathFact/);
  assert.match(model, /checked: boolean/);
  assert.match(model, /probed: boolean/);
});

test("诊断自身三档分开渲染，且「没查成」不说成「没问题」", () => {
  // 加载 / 失败 / 有结论，三档各有各的文案。
  assert.match(code, /diagnoseState === "loading"/);
  assert.match(code, /diagnoseState === "error"/);
  assert.match(code, /检测未完成/);
  // 失败那一档必须明确否认"没有问题"——否则用户会以为这台机器一切正常。
  assert.match(code, /这不代表"没有问题"/);
  assert.match(code, /diagnoseState === "ready"/);
  // 观感按服务端给的 status 走（data-tone 是变体约定，别用裸词类名）。
  assert.match(code, /data-tone=\{tone\}/);
  // 服务端说"这一轮没详查"的工具要被消费，而不是当成"没有问题"。
  assert.match(code, /skippedDiagnoses\[key\]/);
  // 而"没详查"的两句说法（没什么可疑的 / 通道根本没通）在模型里，页面只消费。
  assert.match(code, /skippedDiagnosisText\(diagnoseChannel\?\.ok \?\? true\)/);
  assert.match(model, /本轮没有详查/);
  assert.match(model, /本轮没有检测这个工具/);
  // 通道状态必须被消费：不消费它，通道坏掉时每个工具都会被盖章"没有可疑迹象"。
  assert.match(code, /setDiagnoseChannel\(\{ ok: result\.probeOk/);
});

test("检测与列表是两条路：诊断不进列表热路径", () => {
  // 诊断要跑多次子进程、扫目录、读审计，所以它是按需端点，有独立的状态与代际。
  assert.match(code, /\/diagnostics`/);
  assert.match(code, /\/diagnose`/);
  assert.match(code, /diagnoseGeneration/);
  // 进页面后自动跑一次（管理页真的被打开时才深查）。
  assert.match(code, /void runDiagnostics\(selectedRunner\)/);
});

test("修复动作全部来自服务端，页面不硬编码动作名、也不重排顺序", () => {
  // 症状上的按钮就是服务端下发的 remedies（连 label/detail 都是服务端给的）。
  assert.match(code, /issue\.remedies\.length > 0/);
  assert.match(code, /issue\.remedies\.map\(\(remedy\) => <button/);
  assert.match(code, /\{remedy\.label\}/);
  // 请求体只复述服务端给的 id。
  assert.match(code, /remedies: action\.remedies\.map\(\(remedy\) => remedy\.id\)/);
  // 反面：页面里不许出现任何一个修复动作 id，也不许自己排序。
  // （`install-runtime` 不算 —— 那是"安装 Node 运行时"这个既有动作的 kind，
  //   与同名 remedy 只是字面撞车。）
  for (const forbidden of ["reinstall", "rebuild-shim", "restore-backup", "cleanup-interrupted"]) {
    assert.ok(!code.includes(forbidden), `页面里出现了硬编码的修复动作 id：${forbidden}`);
  }
  assert.doesNotMatch(code, /remedies\.sort\(/);
  assert.doesNotMatch(code, /remedyOrder/);
});

test("修不了的症状不给按钮，而是说清只能手动处理", () => {
  // 给一个点了没反应的按钮比不给更坏。
  assert.match(code, /这个症状平台不能自动修/);
  assert.match(code, /修复全部（/);
  // 确认框里逐步列出将要做什么（用户点的是"改我机器"的动作）。
  assert.match(code, /cli-tools-confirm-steps/);
  assert.match(code, /服务端只会执行当前诊断允许的动作/);
  // 但**不许宣称执行顺序**：界面拿到的是"症状里出现动作的次序"，真正执行的次序由
  // 服务端 remedyOrder 定（可能正好相反）。说一句自己做不到的承诺比不说更坏。
  assert.doesNotMatch(code, /按下面的顺序执行/);
  assert.match(code, /实际先后由服务端按依赖关系安排/);
});

test("修复后换上新诊断，而不是只弹一句「修复完成」", () => {
  // 服务端回填的是修复后重跑的诊断；直接换上去，用户才能当场看出症状有没有消失。
  assert.match(code, /setDiagnoses\(\(current\) => \(\{ \.\.\.current, \[key\]: result\.diagnosis \}\)\)/);
  assert.match(code, /describeRepair\(result\.applied\)/);
  // 成败**消费服务端算好的那个**（`RepairResult.success`），前端不自己重算一遍 ——
  // 同一条规则在两处各实现一遍，改一处另一处不跟随。
  assert.match(code, /if \(result\.success\) toast\.success\(message\)/);
  assert.match(code, /else toast\.error\(message\)/);
  assert.doesNotMatch(code, /describeRepair\(result\.applied\)\.ok/);
});

test("「修复完成」之外还要说清解决了什么，以及谁没执行", () => {
  // 只说"修复完成"证明不了任何事 —— 差集才是证据（docs/43 §6.3）。
  // ⚠️ 差集必须在**换掉旧诊断之前**算，换完就只剩新报告了。
  assert.match(code, /const note = resolvedSummary\(resolvedIssues\(diagnoses\[key\], result\.diagnosis\)\)/);
  assert.match(code, /setResolvedNotes\(/);
  // ⚠️ 它必须渲染在展开面板**之外**：修好之后批量诊断会把该工具判为"不必详查"，
  // diagnosis 被移除 —— 放在面板里就永远看不见了，而那一刻正是最该看见它的时候。
  assert.match(code, /\{resolvedNotes\[key\] && <p className="cli-tools-resolved"/);
  const resolvedLine = code.indexOf("cli-tools-resolved");
  const panelStart = code.indexOf("cli-tools-diagnosis\">");
  assert.ok(resolvedLine > 0 && panelStart > 0 && resolvedLine < panelStart,
    "「已解决 N 项」必须排在诊断面板之前（否则它被面板的渲染条件挡住）");
  assert.match(model, /export function resolvedIssues/);
  assert.match(model, /export function resolvedSummary/);
  // ⚠️ 差集必须挡掉"修复后那份没查成"：否则它会把**所有**旧症状判成"已解决"。
  assert.match(model, /if \(!isConclusiveDiagnosis\(after\)\) return \[\];/);
  // 「没执行」不能被算成「执行失败」——判据在模型里，页面只消费。
  assert.match(model, /!step\.ok && !step\.skipped/);
  assert.match(model, /另有 \$\{skipped\.length\} 个动作没执行/);
  // 跳过的理由不能混进"做成了什么"那一栏（它也带 detail，但那是"为什么没执行"）。
  assert.match(model, /step\.detail && !step\.skipped/);
  assert.match(css, /\.cli-tools-resolved \{/);
  // 而这条提示**只对那一次修复有效**：一发起新动作就要先清掉，否则它会一直挂着，
  // 说一件早已被后续变更推翻的事。
  assert.match(code, /setResolvedNotes\(\(current\) => \(\{ \.\.\.current, \[staleKey\]: "" \}\)\)/);
});

test("服务端算好的每个字段都有渲染路径上的消费点", () => {
  // 「一份没查成的报告也是零症状」⇒ 空态必须看结论，不能只说"没有发现任何症状"。
  assert.match(code, /diagnosisEmptyText\(diagnosis\)/);
  assert.match(model, /export function diagnosisEmptyText/);
  // 报告是**快照**：必须写出什么时候测的，否则旧读数会被当成此刻的事实。
  assert.match(code, /diagnosisMetaLine\(diagnosis\)/);
  assert.match(model, /诊断于 \$\{when\}/);
  // 上次失败也要有时间，否则"上周那次"与"刚才那次"分不出来。
  assert.match(code, /diagnoseMoment\(diagnosis\.lastFailure\.createdAt\)/);
  // 预检：安装与升级共用判据时只说一句，分歧时才补第二句 —— 那两个字段因此真的被读。
  assert.match(code, /preflightNotes\(diagnosis\.preflight\)/);
  assert.match(model, /export function preflightNotes/);
  assert.match(model, /upgradeReason !== preflight\.installReason/);
  // 通道状态与 runnerId 也要消费（前者决定 skipped 的说法，后者校验响应归属）。
  assert.match(code, /result\.runnerId !== runnerID/);
});

test("样式：诊断四档观感各不相同，且「没查成」与「没问题」一眼可分", () => {
  assert.match(css, /\.cli-tools-diagnosis-line\[data-tone="ok"\]/);
  assert.match(css, /\.cli-tools-diagnosis-line\[data-tone="warn"\]/);
  assert.match(css, /\.cli-tools-diagnosis-line\[data-tone="bad"\]/);
  assert.match(css, /\.cli-tools-diagnosis-line\[data-tone="unknown"\]/);
  // unknown 必须是虚线灰：它是四档里唯一"什么都没查出来"的一档，
  // 画成绿的实心就等于把"没查"说成"没问题"。
  assert.match(css, /\.cli-tools-diagnosis-line\[data-tone="unknown"\][^}]*dashed/);
  // 变体一律走 data-*：页面不许再拼动态类名 —— 拼出来的类名一旦取值出乎意料，
  // 就是个没人样式化的死类（而 data-* 至少能被 grep 到）。
  assert.doesNotMatch(code, /cli-tools-diagnosis-\$\{/);
  assert.doesNotMatch(code, /cli-tools-severity-\$\{/);
  assert.match(code, /className="cli-tools-severity" data-severity=\{issue\.severity\}/);
  assert.match(css, /\.cli-tools-severity\[data-severity="blocker"\]/);
  // 上次失败的原文要能看完（限高可滚），不能因为长就被截掉。
  assert.match(css, /\.cli-tools-failure \{[^}]*overflow: auto/);
  // 窄屏下"所有位置"那张表横向可滚，别把页面撑破。
  assert.match(css, /@media \(max-width: 680px\)[\s\S]*?\.cli-tools-paths \{ display: block; overflow-x: auto; \}/);
});
