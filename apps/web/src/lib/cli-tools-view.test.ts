import assert from "node:assert/strict";
import test from "node:test";
import {
  bannerFor,
  buildManualInstallRows,
  buildToolCard,
  canInstallTool,
  cardMark,
  cardTint,
  agentLogoKey,
  currentVersionLine,
  installBlockNote,
  latestVersionLine,
  runnerLabel,
  runtimeLatestLine,
} from "./cli-tools-view";
import type { LatestRead, RuntimeStatus, RunnerAgentItem, ToolCardInput } from "./cli-tools-view";
import type { AgentCatalogEntry } from "./agent-registry";
import type { AgentDiagnosis } from "./cli-diagnosis";
import { preflightNote } from "./cli-diagnosis";

// ── 夹具 ───────────────────────────────────────────────────────────────────
// 这三个夹具是**成对设计**的：默认值代表"一切正常"，每个用例只改它关心的那一位。
// 只写"坏"的那一半，断言会退化成"随便哪个分支通过就行"。

function entry(partial: Partial<AgentCatalogEntry> = {}): AgentCatalogEntry {
  return {
    id: "claude-code",
    name: "Claude Code",
    vendor: "Anthropic",
    installKind: "npm-global",
    npmPackage: "@anthropic-ai/claude-code",
    commandName: "claude",
    minRuntimeVersion: "18.0.0",
    supportsInstall: true,
    permissionModes: [],
    defaultPermissionMode: "approval_required",
    requires: [],
    slashCommands: true,
    mcpInjection: "config-file",
    readiness: "version",
    ...partial,
  };
}

function runtime(partial: Partial<RuntimeStatus> = {}): RuntimeStatus {
  return {
    id: "node",
    installed: true,
    version: "v22.14.0",
    npmVersion: "10.9.2",
    origin: "managed",
    meetsMinimumFor: ["claude-code"],
    installSupported: true,
    updateAvailable: false,
    ...partial,
  };
}

function item(partial: Partial<RunnerAgentItem> = {}): RunnerAgentItem {
  return {
    id: "claude-code",
    installed: true,
    version: "2.0.28",
    ready: true,
    installSupported: true,
    updateSupported: true,
    autoUpdatable: true,
    ...partial,
  };
}

const LATEST: LatestRead = { state: "ready", latest: "2.0.31", updateAvailable: true };
const UP_TO_DATE: LatestRead = { state: "ready", latest: "2.0.28", updateAvailable: false };

function input(partial: Partial<ToolCardInput> = {}): ToolCardInput {
  return {
    entry: entry(),
    item: item(),
    runtime: runtime(),
    latest: UP_TO_DATE,
    skipped: false,
    channelOk: true,
    remoteInstallAllowed: true,
    ...partial,
  };
}

function diagnosis(partial: Partial<AgentDiagnosis> = {}): AgentDiagnosis {
  return {
    agentId: "claude-code",
    status: "ok",
    version: "2.0.28",
    issues: [],
    paths: [],
    limitations: [],
    diagnosedAt: "2026-09-23T01:00:00Z",
    ...partial,
  };
}

// ── 「读不到」不许写成「没有」────────────────────────────────────────────────

test("最新版本读失败时，绝不说「已是最新」", () => {
  const card = buildToolCard(input({ latest: { state: "error", error: "连不上 registry" } }));
  assert.notEqual(card.statusText, "已是最新，不用管它。");
  assert.equal(card.statusTone, "unknown");
  // 读数那一行也不能显示成版本号。
  assert.equal(card.latestText, "读不到");
  // 反过来：读到了且是最新，才允许说"已是最新"。
  assert.equal(buildToolCard(input({ latest: UP_TO_DATE })).statusText, "已是最新，不用管它。");
});

test("「读取中」与「已是最新」是两句话、两种观感", () => {
  const loading = buildToolCard(input({ latest: { state: "loading" } }));
  assert.equal(loading.latestTone, "loading");
  assert.equal(loading.latestText, "读取中…");
  assert.notEqual(loading.statusText, "已是最新，不用管它。");
  assert.equal(loading.icon, "loading");
  assert.equal(buildToolCard(input({ latest: UP_TO_DATE })).icon, "ok");
});

test("「未安装」与「拿不到状态」是两件事", () => {
  const missing = buildToolCard(input({ item: item({ installed: false, version: "" }) }));
  assert.equal(missing.currentText, "未安装");
  assert.equal(missing.statusText, "还没装。");

  const unknown = buildToolCard(input({ item: undefined }));
  assert.equal(unknown.currentText, "状态未知");
  assert.notEqual(unknown.currentText, "未安装");
  assert.notEqual(unknown.statusText, missing.statusText);
});

test("「还在读」既不是「未安装」也不是「拿不到」—— 它有自己的档", () => {
  // 三态（正在读 / 读失败 / 真的没有）里最容易漏的一档。写成"状态未知"就是把
  // "正在读"讲成"读不到"，用户会去查一个并不存在的问题。
  const reading = buildToolCard(input({ item: undefined, reading: true }));
  assert.equal(reading.currentText, "读取中…");
  assert.notEqual(reading.currentText, "状态未知");
  assert.equal(reading.statusText, "正在读取…");
  assert.notEqual(reading.statusText, "拿不到它的状态。");
  assert.equal(reading.icon, "loading");
  assert.equal(reading.currentTone, "loading");
  assert.equal(reading.primary, undefined, "读数还没到，不许亮按钮（能不能装/升都还没读到）");
  assert.equal(reading.note, undefined, "也不许编一句说明出来");

  // 换执行环境那一瞬间：`items` 里还是**上一个环境**的读数 —— 这一档必须把它盖掉。
  const switching = buildToolCard(input({ item: item({ version: "1.0.0" }), reading: true }));
  assert.equal(switching.currentText, "读取中…");
  assert.notEqual(switching.currentText, "1.0.0");
  assert.equal(switching.primary, undefined);

  // 反面：读数一到就照常走后面的分支（不是永远停在"正在读"）。
  const settled = buildToolCard(input({ latest: UP_TO_DATE }));
  assert.equal(settled.currentText, "2.0.28");
  assert.equal(settled.icon, "ok");
});

test("未安装时不把「有新版本」当成卖点，主操作是安装", () => {
  const card = buildToolCard(input({ item: item({ installed: false, version: "" }), latest: LATEST }));
  assert.equal(card.latestTone, "plain", "没装的人看到的最新版本不该带“新”的观感");
  assert.equal(card.updateAvailable, false);
  assert.equal(card.primary?.kind, "install");
});

// ── 缺陷修复：预检必须进按钮判据 ───────────────────────────────────────────

test("预检说升级不行时，不给升级按钮（只给原因）", () => {
  const blocked = buildToolCard(input({
    latest: LATEST,
    diagnosis: diagnosis({ preflight: { installOk: false, installReason: "目标目录没有写权限", upgradeOk: false, upgradeReason: "目标目录没有写权限" } }),
  }));
  assert.notEqual(blocked.primary?.kind, "update", "预检说做不了，就不能亮出升级按钮");
  assert.equal(blocked.canUpdate, false);
  assert.match(blocked.note ?? "", /目标目录没有写权限/);
  // 反面：预检说行（或没有预检）时按钮要在 —— 否则这条断言可以靠"永远不给"变绿。
  const ok = buildToolCard(input({
    latest: LATEST,
    diagnosis: diagnosis({ preflight: { installOk: true, upgradeOk: true } }),
  }));
  assert.equal(ok.primary?.kind, "update");
  assert.equal(ok.canUpdate, true);
});

test("有可修的症状时，主操作是修复而不是升级", () => {
  const card = buildToolCard(input({
    latest: LATEST,
    diagnosis: diagnosis({
      status: "ok",
      issues: [
        { code: "path-shadowed", severity: "warning", summary: "命令可能跑到旧版本上", evidence: ["…"], remedies: [{ id: "rebuild-shim", label: "重建入口", detail: "…" }] },
      ],
    }),
  }));
  assert.equal(card.primary?.kind, "repair");
  assert.equal(card.statusText, "命令可能跑到旧版本上");
  assert.equal(card.statusTone, "warn");
});

test("症状存在但没有可自动修的动作时，不给按钮、并说清只能手动处理", () => {
  const card = buildToolCard(input({
    diagnosis: diagnosis({
      status: "broken",
      issues: [{ code: "two-installs", severity: "blocker", summary: "装了两份", evidence: ["…"], remedies: [] }],
    }),
  }));
  assert.equal(card.primary, undefined);
  assert.match(card.note ?? "", /不能自动修/);
  assert.equal(card.statusTone, "bad");
});

test("info 级症状不算「要留意」，不该顶掉「已是最新」", () => {
  const card = buildToolCard(input({
    latest: UP_TO_DATE,
    diagnosis: diagnosis({
      issues: [{ code: "note", severity: "info", summary: "装了两份，但都能用", evidence: [], remedies: [] }],
    }),
  }));
  assert.equal(card.statusText, "已是最新，不用管它。");
  assert.equal(card.statusTone, "ok");
});

test("跨端工具不做应用内升级：给手动命令，不给按钮", () => {
  const card = buildToolCard(input({ latest: LATEST, item: item({ autoUpdatable: false }) }));
  assert.equal(card.primary?.kind, undefined);
  assert.equal(card.canUpdate, false);
  assert.match(card.note ?? "", /手动执行 claude update/);
});

test("升级要先授权时，不给按钮（与「平台不支持升级」是两回事）", () => {
  const card = buildToolCard(input({ latest: LATEST, item: item({ upgradeNeedsGrant: true }) }));
  assert.equal(card.primary?.kind, undefined);
  assert.match(card.note ?? "", /先在此主机授权/);
  // 两条说法必须不同：同时出现用户不知道该做哪个。
  const unsupported = buildToolCard(input({ latest: LATEST, item: item({ autoUpdatable: false }) }));
  assert.notEqual(card.note, unsupported.note);
});

test("有任务在跑时不给任何操作入口", () => {
  for (const candidate of [
    input({ latest: LATEST, item: item({ operation: "running" }) }),
    input({ item: item({ installed: false, operation: "running" }) }),
  ]) {
    const card = buildToolCard(candidate);
    assert.equal(card.primary, undefined);
    assert.match(card.note ?? "", /已有任务在进行/);
  }
});

// ── 「能不能装」的三件事 ───────────────────────────────────────────────────

test("能不能装：环境、npm、最低运行时三件事缺一不可", () => {
  assert.equal(canInstallTool(input({ item: item({ installed: false }) })), true);
  // ① 环境不支持
  assert.equal(canInstallTool(input({ item: item({ installed: false, installSupported: false }) })), false);
  // ② npm 不可用
  assert.equal(canInstallTool(input({ item: item({ installed: false }), runtime: runtime({ npmVersion: "" }) })), false);
  // ③ 运行时过低（装了，但不在 meetsMinimumFor 里）
  assert.equal(canInstallTool(input({ item: item({ installed: false }), runtime: runtime({ meetsMinimumFor: [] }) })), false);
  // ④ meetsMinimumFor 为 null（服务端还没算）时不许当成"满足"
  assert.equal(canInstallTool(input({ item: item({ installed: false }), runtime: runtime({ meetsMinimumFor: null }) })), false);
});

test("四档「不能装」各有各的说法，两两不同", () => {
  const unsupported = installBlockNote(input({ item: item({ installed: false, installSupported: false, installBlockedReason: "这个执行环境不提供该工具。" }) }));
  const noRuntime = installBlockNote(input({ item: item({ installed: false }), runtime: runtime({ installed: false, version: "" }) }));
  const noNpm = installBlockNote(input({ item: item({ installed: false }), runtime: runtime({ npmVersion: "" }) }));
  const tooOld = installBlockNote(input({ item: item({ installed: false }), runtime: runtime({ meetsMinimumFor: [] }) }));
  const texts = [unsupported, noRuntime, noNpm, tooOld];
  assert.equal(new Set(texts).size, texts.length, `文案重复：${texts.join(" / ")}`);
  assert.match(noRuntime, /需要先安装 Node\.js 运行时/);
  assert.match(noNpm, /随包的 npm 不可用/);
  assert.match(tooOld, /请先升级运行时/);
});

test("不能装时卡片给原因、不给按钮", () => {
  const card = buildToolCard(input({
    item: item({ installed: false, version: "" }),
    runtime: runtime({ meetsMinimumFor: [] }),
  }));
  assert.equal(card.primary, undefined);
  assert.match(card.note ?? "", /请先升级运行时/);
});

// ── 预检那一句 ─────────────────────────────────────────────────────────────

test("预检没说不行为空串；说不行时只有一句、且不含「也」", () => {
  assert.equal(preflightNote(undefined), "");
  assert.equal(preflightNote({ installOk: true, upgradeOk: true }), "");
  const blocked = preflightNote({ installOk: false, installReason: "运行时版本过低", upgradeOk: false, upgradeReason: "运行时版本过低" });
  assert.match(blocked, /运行时版本过低/);
  // 「升级**也**不行」那个说法预设了前面还有一句"现在装不了" —— 而服务端把两者写在
  // 同一个判断里（agent_diagnose.go:862），installOk 为真时 upgradeOk 一定也为真，
  // 所以那个分支不可达。留一句带"也"的话，等于承认存在"能装、不能升"这种情形。
  assert.doesNotMatch(blocked, /也/, "「也」预设了前一句存在，而它产生不出来");
});

// ── 版本两行 ───────────────────────────────────────────────────────────────

test("「当前版本」四态：未安装 / 已安装无版本号 / 有版本号 / 拿不到状态", () => {
  const texts = [
    currentVersionLine(undefined).text,
    currentVersionLine(item({ installed: false })).text,
    currentVersionLine(item({ version: "" })).text,
    currentVersionLine(item({ version: "2.0.28" })).text,
  ];
  assert.equal(new Set(texts).size, texts.length, `文案重复：${texts.join(" / ")}`);
  assert.equal(currentVersionLine(undefined).text, "状态未知");
  assert.notEqual(currentVersionLine(undefined).text, "未安装");
});

test("「最新版本」四态：读取中 / 读不到 / 读到 / 空版本号", () => {
  assert.equal(latestVersionLine({ state: "loading" }, true).text, "读取中…");
  assert.equal(latestVersionLine({ state: "error", error: "x" }, true).text, "读不到");
  assert.equal(latestVersionLine({ state: "ready", latest: "", updateAvailable: false }, true).text, "—");
  assert.equal(latestVersionLine({ state: "ready", latest: "2.0.31", updateAvailable: true }, true).tone, "new");
  assert.equal(latestVersionLine({ state: "ready", latest: "2.0.31", updateAvailable: true }, false).tone, "plain");
});

// 「运行依赖」那一行：**读不到最新版本不许说"已是最新"**。
//
// latestVersion 是服务端的 omitempty 字段，只在真取到 Node 版本索引时才有值；离线 /
// registry 不可达时它为空，updateAvailable 也随之 false —— 只按 updateAvailable 二分，
// 就会对着一个"根本没查成"的环境亮绿灯。这条用例把那一档单独钉住。
test("运行依赖读数：读不到最新版本时不许写「已是最新」", () => {
  const base: RuntimeStatus = {
    id: "node", installed: true, version: "v20.11.0", npmVersion: "10.2.4", origin: "system",
    meetsMinimumFor: ["claude-code"], installSupported: true, updateAvailable: false,
  };
  const unknown = runtimeLatestLine({ ...base, latestVersion: "" });
  assert.equal(unknown.tone, "unknown");
  assert.doesNotMatch(unknown.text, /已是最新/);

  const ok = runtimeLatestLine({ ...base, latestVersion: "v22.14.0" });
  assert.deepEqual(ok, { text: "已是最新", tone: "ok" });

  const update = runtimeLatestLine({ ...base, latestVersion: "v22.14.0", updateAvailable: true });
  assert.equal(update.tone, "update");
  assert.match(update.text, /v22\.14\.0/);
});

// ── 页面级汇总 ─────────────────────────────────────────────────────────────

test("横幅：有新版本但升不了 > 用不了 > 要留意，无事时整条不存在", () => {
  const blockedUpdate = buildToolCard(input({ latest: LATEST, item: item({ autoUpdatable: false }) }));
  const unusable = buildToolCard(input({
    item: item({ id: "codex" }),
    entry: entry({ id: "codex", name: "Codex" }),
    diagnosis: diagnosis({ status: "broken", issues: [{ code: "x", severity: "blocker", summary: "用不了", evidence: [], remedies: [] }] }),
  }));
  const attention = buildToolCard(input({
    diagnosis: diagnosis({ issues: [{ code: "y", severity: "warning", summary: "要留意", evidence: [], remedies: [{ id: "rebuild-shim", label: "重建", detail: "…" }] }] }),
  }));

  assert.match(bannerFor([blockedUpdate])?.text ?? "", /有新版本/);
  assert.match(bannerFor([unusable])?.text ?? "", /现在用不了/);
  assert.match(bannerFor([attention])?.text ?? "", /需要处理一下/);
  // 排他：第一条分支赢。
  assert.equal(bannerFor([blockedUpdate, unusable, attention])?.tone, "warn");
  assert.match(bannerFor([blockedUpdate, unusable, attention])?.text ?? "", /有新版本/);
  // 无事时整条不渲染（不是渲染一条"都没问题"的横幅）。
  assert.equal(bannerFor([buildToolCard(input({ latest: UP_TO_DATE }))]), undefined);
});

test("横幅点名的那个工具，和真正能点「修好它」的那个是同一个", () => {
  const healthy = buildToolCard(input({ latest: UP_TO_DATE }));
  const sick = buildToolCard(input({
    entry: entry({ id: "codex", name: "Codex" }),
    item: item({ id: "codex" }),
    diagnosis: diagnosis({ agentId: "codex", issues: [{ code: "z", severity: "warning", summary: "有事", evidence: [], remedies: [{ id: "rebuild-shim", label: "重建", detail: "…" }] }] }),
  }));
  const banner = bannerFor([healthy, sick]);
  assert.equal(banner?.agentID, "codex", "横幅点名的工具必须带着那个能修的动作");
});

// 「有新版本但升不了」这一档有四种成因，**四种都没有可修的动作**。
// 以前横幅一律写"升级前要先处理一个问题 / 修好它"：文案断言了一个不存在的问题，
// 按钮点下去只是打开抽屉（与旁边那颗「看看是什么问题」同一个效果）。
// 现在：没有 repair 动作就不给按钮，改说卡片自己那句 note（成因的唯一来源）。
test("横幅：升不了但没有可修动作时不给按钮，照卡片的原因说", () => {
  // 成因一：跨端只能手动升（updateDecision 只给 note）。
  const manual = buildToolCard(input({ latest: LATEST, item: item({ autoUpdatable: false }) }));
  const manualBanner = bannerFor([manual]);
  assert.equal(manualBanner?.actionLabel, undefined, "没有可修动作就不该给「修好它」");
  assert.match(manualBanner?.text ?? "", /但现在升不了/, "要说清「升不了」，不能说「要先处理一个问题」");
  assert.doesNotMatch(manualBanner?.text ?? "", /要先处理一个问题/);
  assert.match(manualBanner?.text ?? "", /手动执行/, "原因取自卡片自己的 note");

  // 成因二：要先授权。
  const grant = buildToolCard(input({ latest: LATEST, item: item({ upgradeNeedsGrant: true }) }));
  assert.equal(bannerFor([grant])?.actionLabel, undefined);
  assert.match(bannerFor([grant])?.text ?? "", /授权/);

  // 成因三：该工具正有任务在跑 —— 整条不出：正确答案是"等它跑完"，
  // 而卡片自己已经在说「正在处理…」。
  const running = buildToolCard(input({ latest: LATEST, item: item({ operation: "running" }) }));
  assert.equal(running.running, true);
  assert.equal(bannerFor([running]), undefined, "正在跑的工具不该上横幅");

  // 有 repair 动作的那一档不受影响：按钮与文案照旧。
  const repairable = buildToolCard(input({
    latest: LATEST,
    item: item({ autoUpdatable: false }),
    diagnosis: diagnosis({ issues: [{ code: "w", severity: "blocker", summary: "坏了", evidence: [], remedies: [{ id: "rebuild-shim", label: "重建", detail: "…" }] }] }),
  }));
  assert.equal(repairable.primary?.kind, "repair");
  assert.equal(bannerFor([repairable])?.actionLabel, "修好它");
});

// 横幅说的"升不了的原因"必须是**升级受阻的原因**，不是那张卡此刻在说的别的事。
//
// 卡片上的 `note` 会先被诊断相关的分支占用（④ 修不了 / ⑤ 没查成），而 `update.available`
// 在那些分支里已经是 true —— 早先直接读 `card.note` 就会产出：
//   「Claude Code 有新版本 2.0.31，但现在升不了：这不代表它没问题 —— 只是这一次没查成。」
// 真正的原因（跨端只能手动更新）一个字都没上屏，后半句还在说诊断。
// 2026-09-29 由独立复查实测出来，所以这条断言钉的是**文案里必须有真正的原因**。
test("横幅：升不了的原因取 updateNote，不拿卡片那句诊断话", () => {
  const inconclusive = buildToolCard(input({
    latest: LATEST,
    item: item({ autoUpdatable: false }),
    diagnosis: diagnosis({ status: "unknown" }), // ⑤ 没查成：它会写一条关于诊断的 note
  }));
  assert.match(inconclusive.note ?? "", /没查成/, "前提：这张卡的 note 说的是诊断");
  assert.match(inconclusive.updateNote ?? "", /手动执行/, "前提：升级受阻的原因在 updateNote 里");

  const banner = bannerFor([inconclusive]);
  assert.match(banner?.text ?? "", /手动执行.*update/, "横幅要说出真正的升级原因");
  assert.doesNotMatch(banner?.text ?? "", /没查成/, "横幅不能拿诊断那句话当升级原因");
});

// 最新版本那一趟还没回来时不上横幅：那时 latestText 是"读取中…"，
// 横幅只会说出「X 有新版本 读取中…」这种半句话（重读期间 view 没被清掉）。
test("横幅：最新版本还在读的时候不说'有新版本 读取中…'", () => {
  const reading = buildToolCard(input({ latest: LATEST, item: item({ autoUpdatable: false }), reading: true }));
  assert.equal(reading.latestTone, "loading");
  assert.equal(reading.updateAvailable, true, "前提：这一档的 updateAvailable 仍是上一轮的真值");
  assert.equal(bannerFor([reading]), undefined);
});

test("报告没有结论时，绝不说「已是最新，不用管它」", () => {
  // 一份没查成的报告也是**零症状** —— 而零症状最容易被念成"没问题"。
  const inconclusive = diagnosis({ status: "unknown", issues: [], limitations: ["跨端这台机器上没核对过"] });
  const card = buildToolCard(input({ latest: UP_TO_DATE, diagnosis: inconclusive }));
  assert.equal(card.statusTone, "unknown");
  assert.match(card.statusText, /没检查成功/);
  assert.notEqual(card.statusText, "已是最新，不用管它。");
  // 面板里那句也要跟着结论走，而不是说"没有发现任何症状"。
  assert.ok(card.detailNotes.some((line) => /没有得出结论/.test(line)), card.detailNotes.join(" / "));
});

test("没查成时，更新这一件事照样说（它是另一个读数）", () => {
  const inconclusive = diagnosis({ status: "unknown" });
  const canUpdateCard = buildToolCard(input({ latest: LATEST, diagnosis: inconclusive }));
  assert.equal(canUpdateCard.primary?.kind, "update");
  // 但"升不了"时仍然不给按钮，而且理由要给出来 —— 两个分支共用同一个判据。
  const blocked = buildToolCard(input({
    latest: LATEST,
    diagnosis: diagnosis({ status: "unknown", preflight: { installOk: false, installReason: "没有写权限", upgradeOk: false, upgradeReason: "没有写权限" } }),
  }));
  assert.equal(blocked.primary?.kind, undefined);
  assert.equal(blocked.statusTone, "unknown");
});

test("「本轮没详查」的两种成因各说各的，且都不冒充「没问题」", () => {
  const okChannel = buildToolCard(input({ latest: UP_TO_DATE, skipped: true, channelOk: true }));
  const badChannel = buildToolCard(input({ latest: UP_TO_DATE, skipped: true, channelOk: false }));
  assert.ok(okChannel.detailNotes.some((line) => /没有可疑迹象/.test(line)), okChannel.detailNotes.join(" / "));
  assert.ok(badChannel.detailNotes.some((line) => /当前不可用/.test(line)), badChannel.detailNotes.join(" / "));
  assert.notDeepEqual(okChannel.detailNotes, badChannel.detailNotes);
});

test("牌面按**厂商**分档，认不出的厂商落中性档而不是回落到某个已知厂商", () => {
  assert.equal(cardTint("Anthropic"), "anthropic");
  assert.equal(cardTint("OpenAI"), "openai");
  // 新厂商（服务端加了目录条目、前端还没见过）必须落中性档 —— 回落到 anthropic
  // 就是把未知工具标成 Claude，正是本项目消灭过的那类错。
  for (const vendor of ["", "某新厂商", "Google DeepMind"]) {
    assert.equal(cardTint(vendor), "other", `vendor=${vendor}`);
  }
  assert.equal(cardMark("Claude Code"), "CC");
  assert.equal(cardMark("Codex"), "CO");
  assert.equal(cardMark("CodeBuddy Code"), "CC");
});

test("官方图标按工具 ID 白名单，白名单之外一律 null（回落两字母牌）", () => {
  assert.equal(agentLogoKey("claude-code"), "claude");
  assert.equal(agentLogoKey("codex"), "openai");
  assert.equal(agentLogoKey("codebuddy"), "codebuddy");
  // 服务端新加的工具、拼错的 ID、空串：null —— 界面回落两字母牌，
  // **绝不**回落到某个已知产品的图标（与 cardTint 的中性档同一个理由：
  // 把别的工具盖上 Claude 的星芒，就是"把未知工具显示成 Claude Code"那一族错）。
  for (const id of ["gemini-cli", "claude", "Claude-Code", "", "claude-code "]) {
    assert.equal(agentLogoKey(id), null, `id=${JSON.stringify(id)}`);
  }
  // buildToolCard 把它带出来（页面只渲染，不再自己认一遍）。
  assert.equal(buildToolCard(input()).logo, "claude");
  assert.equal(buildToolCard(input({ entry: entry({ id: "gemini-cli" }) })).logo, null);
});

test("说明那一句分两档：做不到才 amber，纯陈述是灰", () => {
  // 装不了 / 升不了 / 要手动执行 / 要先去授权 —— 这四类都是"有一件事你做不到"。
  assert.equal(buildToolCard(input({ item: item({ installed: false, version: "" }), runtime: runtime({ meetsMinimumFor: [] }) })).noteTone, "warn");
  assert.equal(buildToolCard(input({ latest: LATEST, item: item({ autoUpdatable: false }) })).noteTone, "warn");
  assert.equal(buildToolCard(input({ latest: LATEST, item: item({ upgradeNeedsGrant: true }) })).noteTone, "warn");
  // 纯陈述一律灰：装好就能用 / 没查成 / 读不到最新版本 / 有任务在跑。
  assert.equal(buildToolCard(input({ item: item({ installed: false, version: "" }) })).noteTone, "muted");
  assert.equal(buildToolCard(input({ latest: UP_TO_DATE, diagnosis: diagnosis({ status: "unknown" }) })).noteTone, "muted");
  assert.equal(buildToolCard(input({ latest: { state: "error", error: "x" } })).noteTone, "muted");
  // 反面：不许全都染琥珀（那会让真正要留意的那些被淹没）。
  const warnCount = [input({ latest: LATEST, item: item({ autoUpdatable: false }) }), input({ item: item({ installed: false, version: "" }) })]
    .map((candidate) => buildToolCard(candidate).noteTone);
  assert.notDeepEqual(warnCount, ["warn", "warn"]);
});

// ── 执行环境那个标签 ───────────────────────────────────────────────────────

test("执行环境只念机器名：本机两档是 Windows / WSL，远端仍用主机名", () => {
  // ⚠️ 夹具用的是**服务端真的会发的取值**（`app.go` 的 windowsLocalMeta / wslLocalRunnerMeta
  // 写的就是小写 "windows" / "wsl"）。别为了"看着自然"改成 "Windows 11" 那种字符串 ——
  // 服务端从不发它，按 environment 认环境的判据会静静地永不命中，而测试照样全绿。
  assert.equal(runnerLabel({ name: "Windows Local Runner", environment: "windows" }), "Windows");
  assert.equal(runnerLabel({ name: "WSL Local Runner (Ubuntu-22.04)", environment: "wsl" }), "WSL");
  // 那两行的内容里不许再夹着内部叫法或后缀。
  for (const label of [
    runnerLabel({ name: "Windows Local Runner", environment: "windows" }),
    runnerLabel({ name: "WSL Local Runner (Ubuntu-22.04)", environment: "wsl" }),
  ]) {
    assert.doesNotMatch(label, /Runner|Local|（|\(/);
  }
  // 远端 SSH：那时 name 就是主机名，是用户唯一认得出的东西 —— 不能换成环境词。
  assert.equal(runnerLabel({ name: "prod", environment: "ssh" }), "prod");
  assert.equal(runnerLabel({ name: "build-host", environment: "remote-linux" }), "build-host");
  // 缺字段时不许渲染出空标签，也不许把 undefined 漏到界面上。
  assert.equal(runnerLabel({ environment: "wsl" }), "WSL");
  assert.equal(runnerLabel({}), "未知环境");
  assert.equal(runnerLabel(undefined), "");
});

// ── 手动安装命令 ───────────────────────────────────────────────────────────

test("手动安装命令：npm 全局分发的工具给命令，别的分发方式**不回落**", () => {
  const rows = buildManualInstallRows([
    entry({ id: "claude-code", name: "Claude Code", npmPackage: "@anthropic-ai/claude-code" }),
    entry({ id: "codex", name: "Codex", npmPackage: "@openai/codex" }),
    // 将来若有非 npm 分发的工具：**不进这一列**。回落成 `npm install -g` 等于
    // 教用户做错事 —— 与 installBlockNote 里"四档不回落"是同一条纪律。
    entry({ id: "native-thing", name: "Native Thing", installKind: "native", npmPackage: "@x/native" }),
    // 声明是 npm 全局但没给包名：也拼不出命令，同样不进列（而不是拼出半条）。
    entry({ id: "broken", name: "Broken", npmPackage: "" }),
  ]);

  assert.deepEqual(rows, [
    { id: "claude-code", name: "Claude Code", command: "npm install -g @anthropic-ai/claude-code@latest" },
    { id: "codex", name: "Codex", command: "npm install -g @openai/codex@latest" },
  ]);
});

test("手动安装命令：清单不随「装没装」变（它是手册，不是状态面板）", () => {
  // 这条钉住的是"别把已装的工具过滤掉"：已装的工具拿同一条命令就是升级，
  // 而这一块存在的意义正是"平台装不了时自己上手"。
  const catalog = [entry({ id: "claude-code" }), entry({ id: "codex", npmPackage: "@openai/codex" })];
  assert.equal(buildManualInstallRows(catalog).length, 2);
  // 空目录 → 空清单（页面据此整块不渲染，而不是渲染一个空框）。
  assert.deepEqual(buildManualInstallRows([]), []);
});
