// 「正在处理」状态条的判据：全部是纯函数，直接调用来断言。
//
// 这一套守的是**读数与分类**两件事，各写一组 —— 2026-09-22 实证过：
// 把某个判据写死时，"分类"用例照样绿、只有"读数"用例会红，只写一套就等于漏一半。
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import {
  condenseStageDetail,
  formatElapsed,
  latestNotice,
  processingBadge,
  processingClockVisibleAfterMs,
  processingStageFallback,
  processingStageText,
  type ProcessingStageInput,
} from "./processing-indicator";

// ── 分类：阶段词该取哪一个来源 ────────────────────────────────────────────
test("阶段词：用通知自己的标题，而不是在手机端再翻译一遍", () => {
  // 这一条守的是"同一句话只有一个作者"。桌面时间线（systemItemFromEvent）已经把事件译成
  // 中文标题，手机端通过 RemoteNotice.title 原样收到 —— 阶段词直接用它。
  // 变异检验：把实现改回 `variant → 词` 映射表，这条必红（因为它给的标题是自定义字符串）。
  const input: ProcessingStageInput = { notice: { variant: "api_retry", title: "API 重试中" } };
  assert.equal(processingStageText(input), "API 重试中");
  // 换一个变体配一个标题，仍取标题：证明取的是 title 而不是"按 variant 查出来的词"
  assert.equal(
    processingStageText({ notice: { variant: "compact", title: "正在压缩上下文" } }),
    "正在压缩上下文",
  );
  assert.equal(
    processingStageText({ notice: { variant: "compact_boundary", title: "上下文压缩摘要" } }),
    "上下文压缩摘要",
  );
});

test("阶段词：标题优先于 detail（状态条只有一行，该显示名字不是细节）", () => {
  // 「第 2/5 次重试：服务过载」是细节，它在下面对应那张状态卡里本来就有；
  // 状态条那一行要显示的是"这件事叫什么"。
  const input: ProcessingStageInput = {
    notice: { variant: "api_retry", title: "API 重试中", detail: "第 2/5 次重试：服务过载" },
  };
  assert.equal(processingStageText(input), "API 重试中");
});

test("阶段词：标题缺失时用变体兜底，不认识就落「正在处理」", () => {
  // 服务端只给了 subtype、标题还没落地时，至少要说对"在干什么"。这两条是兜底词，
  // 不是翻译表 —— 所以它们是 noticeStageFallbacks 里仅有的两项。
  assert.equal(processingStageText({ notice: { variant: "compact" } }), "正在压缩上下文");
  assert.equal(processingStageText({ notice: { variant: "api_retry" } }), "网络重试中");
  // 表里没有的变体 + 没有标题 ⇒ 兜底（**不能**把英文标识符漏到界面上）
  const unknown: ProcessingStageInput = { notice: { variant: "brand_new_variant" } };
  assert.equal(processingStageText(unknown), processingStageFallback);
  assert.doesNotMatch(processingStageText(unknown), /brand_new_variant/);
});

test("阶段词：error 的标题照常显示（一次失败必须说出来，不能装成还在跑）", () => {
  // error 是**唯一**必须显示自己那句话的变体：把「执行失败」吞掉、落回中性的「正在处理」，
  // 就等于把一次失败说成还在跑。它在处理规则上和别的变体没有区别（都是"用标题"），
  // 但这里单独钉一条用例，是因为它是**错得最贵的**那一种。
  assert.equal(processingStageText({ notice: { variant: "error", title: "执行失败" } }), "执行失败");
  // 兜底词本身不允许暗示"成功"或"失败"任何一方 —— 它只是个占位。
  assert.equal(processingStageFallback, "正在处理");
});

test("阶段词兜底表自我一致：只含缺标题时的兜底词，不含已完成/失败态", () => {
  // 从**源码文本**里把表读出来（模块没导出那张表，也不该为了测试而导出
  // —— 导出就等于给了第二个改它的入口）。判据：键集合恰好是两项。
  // 多一项就说明有人开始往里抄桌面文案了，那正是本模块要防的漂移。
  const source = readFileSync(new URL("./processing-indicator.ts", import.meta.url), "utf8");
  const start = source.indexOf("const noticeStageFallbacks");
  // ⚠️ 抽取必须**先自证锚点可靠**，否则这段断言会以两种方式悄悄失效：
  //   · 抽空 ⇒ keys 变成 []，看上去"表里没有多余项"，其实什么都没读到；
  //   · 抽歪 ⇒ 把后面别的对象当成了表。
  // 所以下面先钉住：锚点存在、且截出来的正是那张表（以 "= {" 开头）。
  assert.ok(start >= 0, "锚点「const noticeStageFallbacks」没找到 —— 表被改名/删掉了，这条断言已经失效");
  const end = source.indexOf("};", start);
  assert.ok(end > start, "兜底表的收尾「};」没找到，抽取区间无效");
  const table = source.slice(start, end);
  assert.match(
    table,
    /^const noticeStageFallbacks\s*:\s*Record<string, string>\s*=\s*\{/,
    "截出来的不是兜底表本身（抽取起点跑偏了）—— 先修断言再谈结论",
  );
  const keys = [...table.matchAll(/^\s{2}(\w+):/gm)].map((match) => match[1]);
  assert.deepEqual(keys, ["compact", "api_retry"]);
  // 「已完成」这一档绝不能进兜底表：它的语气与进行中相反，而兜底只会出现在"正在跑"的时候。
  assert.ok(!keys.includes("compact_result"), "已完成态的措辞不能当成进行中的兜底词");
});

test("阶段词：没有任何通知才落兜底", () => {
  assert.equal(processingStageText({ notice: null }), processingStageFallback);
  assert.equal(processingStageText({ notice: { variant: "task" } }), processingStageFallback);
  assert.equal(processingStageText({ notice: { variant: "task", title: "" } }), processingStageFallback);
  assert.equal(processingStageText({ notice: { variant: "task", title: "   " } }), processingStageFallback);
});

// ── 读数：detail 的压平与截断 ─────────────────────────────────────────────
test("detail 压平：只取首行 + 合并空白（状态条必须单行）", () => {
  // 多行 detail 会把状态条撑成两行，而这一页有「贴底跟随」判定，
  // 条高变化会让视角跟着跳 —— 所以这条不是"好看"，是"不许变高"。
  assert.equal(condenseStageDetail("第一行\n第二行\n第三行"), "第一行");
  assert.equal(condenseStageDetail("  前后有空格  "), "前后有空格");
  assert.equal(condenseStageDetail("中间\t\t有制表符"), "中间 有制表符");
  assert.equal(condenseStageDetail(""), "");
  assert.equal(condenseStageDetail(undefined), "");
  assert.equal(condenseStageDetail(null), "");
  assert.equal(condenseStageDetail("   \n  "), "");
});

test("detail 截断：超长加省略号，未超长不动", () => {
  const short = "mobile-remote.css";
  assert.equal(condenseStageDetail(short), short);
  const long = "x".repeat(40);
  const result = condenseStageDetail(long);
  assert.ok(result.length <= 19, `截断后长度应 <= 19，实际 ${result.length}`);
  assert.ok(result.endsWith("…"));
});

// ── 读数：耗时分档 ────────────────────────────────────────────────────────
test("耗时阈值被钉死（10 秒以下不显示）", () => {
  // 与 card-drag 那条同理：数字本身就是设计参数，动它必须显式改这里。
  assert.equal(processingClockVisibleAfterMs, 10_000);
});

test("耗时：10 秒以下空串，以上按秒/分/小时分档", () => {
  assert.equal(formatElapsed(0), "");
  assert.equal(formatElapsed(9_999), "");
  assert.equal(formatElapsed(10_000), "10 秒");
  assert.equal(formatElapsed(24_000), "24 秒");
  assert.equal(formatElapsed(59_000), "59 秒");
  assert.equal(formatElapsed(60_000), "1 分 0 秒");
  assert.equal(formatElapsed(84_000), "1 分 24 秒");
  assert.equal(formatElapsed(3_599_000), "59 分 59 秒");
  assert.equal(formatElapsed(3_600_000), "1 小时 0 分");
  assert.equal(formatElapsed(3_900_000), "1 小时 5 分");
});

test("耗时：不出现时钟读数（要让读者自己换算的那种）", () => {
  // 判据：输出里不能有 `24:31` / `1:24:07` 这种冒号分隔的读数。
  const samples = [24_000, 84_000, 3_600_000, 3_900_000, 7_200_000];
  for (const ms of samples) {
    assert.doesNotMatch(formatElapsed(ms), /\d:\d/, `${ms}ms 出现了时钟读数`);
  }
});

test("耗时：负值与非法值当 0（不显示「-3 秒」这种坏读数）", () => {
  // 客户端与服务端时钟有偏差时会出现负值。
  assert.equal(formatElapsed(-1), "");
  assert.equal(formatElapsed(-60_000), "");
  assert.equal(formatElapsed(Number.NaN), "");
  assert.equal(formatElapsed(Number.POSITIVE_INFINITY), "");
  // ⚠️ 上面这几条**都不是**在测 `ms < 0` 那半句 —— 它们全被 10 秒门槛先挡掉了
  //（任何负数都 < 10_000，门槛那行的返回与负值无关）。2026-09-22 用变异检验证实：
  // 删掉 `ms < 0` 后上面四条全绿。真正的判据是下面这条**结构断言**：
  // 这半句在当前阈值下与门槛不可能同时为真，它是纯防御分支；
  // 若哪天门槛被改成非正数，它就会立刻变成唯一去路（那正是 -3 秒 会出现的时候）。
  assert.ok(processingClockVisibleAfterMs > 0, "门槛若不再为正，`ms < 0` 就不再是死分支 —— 这条断言逼着改门槛的人同时回来看这里");
});

// ── 分类：徽标 ────────────────────────────────────────────────────────────
test("徽标：codex 与 Claude Code 分开，未知回落 Claude Code", () => {
  assert.deepEqual(processingBadge("codex"), { label: "Codex", tone: "codex" });
  assert.deepEqual(processingBadge("claude-code"), { label: "Claude Code", tone: "claude" });
  // 未知 agent 的默认必须与**改版前的观感一致**（原先就显示 Claude Code）。
  assert.deepEqual(processingBadge(""), { label: "Claude Code", tone: "claude" });
  assert.deepEqual(processingBadge("something-else"), { label: "Claude Code", tone: "claude" });
});

// ── 读数：最新通知的挑选 ──────────────────────────────────────────────────
test("最新通知：按 createdAt 取最大，不看数组顺序", () => {
  // 服务端合并通知（merge: "append"）时顺序不保证与时间一致，
  // 所以不能直接取数组末项 —— 这条用乱序数组把两种实现区分开。
  const picked = latestNotice([
    { createdAt: "2026-09-22T10:00:05.000Z", variant: "task" },
    { createdAt: "2026-09-22T10:00:09.000Z", variant: "api_retry" },
    { createdAt: "2026-09-22T10:00:01.000Z", variant: "compact" },
  ]);
  assert.equal(picked?.variant, "api_retry");
});

test("最新通知：空列表返回 null，缺时间戳时仍给出一条", () => {
  assert.equal(latestNotice([]), null);
  assert.equal(latestNotice(undefined), null);
  const noTime = latestNotice([{ variant: "task" }, { variant: "compact" }]);
  assert.ok(noTime, "全部缺时间戳时至少要给出一条，不能返回 null");
});

test("最新通知：时间戳并列时取靠后那条（>= 而不是 >）", () => {
  // createdAt 直接来自服务端事件（`String(record.createdAt || "")`），**没有**「同一毫秒
  // 只能有一条」的保证；mergeNotices 又按 createdAt 升序排过，所以并列时"数组里靠后"
  // 就等于"更晚到达"。`>=` 保留后者、`>` 会退回前者 —— 后者才是对的那条。
  // （2026-09-22 变异检验抓到：把 `>=` 改成 `>` 一条测试都不红。）
  const same = "2026-09-22T10:00:09.000Z";
  const picked = latestNotice([
    { createdAt: same, variant: "compact" },
    { createdAt: same, variant: "api_retry" },
  ]);
  assert.equal(picked?.variant, "api_retry", "并列时应取靠后（更晚到达）的那条");
});
