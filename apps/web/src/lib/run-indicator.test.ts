// 运行指示器判据的测试：分类（阶段词该取哪个 / 变体抽取是否稳定）与
// 读数（计时格式化）各一组 —— 只写一套等于漏一半（见 processing-indicator.test.ts 的教训）。
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  formatStageClock,
  latestActiveStage,
  pickRunIndicatorVariant,
  pickRunTextVariant,
  runIndicatorVariants,
  runTextVariants,
  splitLabelChars,
  stageClockVisibleAfterMs,
  type RunIndicatorVariant,
  type RunTextVariant,
} from "./run-indicator";
import type { Event } from "./types";

function systemEvent(id: string, createdAt: string, payload: Record<string, unknown>): Event {
  return { id, type: "system", payload, runId: "run-1", createdAt };
}

// ── 分类：变体抽取 ────────────────────────────────────────────────────────
test("变体抽取：同一 runId 永远抽到同一个（重渲染不换花样）", () => {
  const first = pickRunIndicatorVariant("run-abc-123");
  for (let i = 0; i < 20; i += 1) {
    assert.equal(pickRunIndicatorVariant("run-abc-123"), first);
  }
});

test("变体抽取：结果必在 7 个定稿变体之内", () => {
  const valid = new Set<string>(runIndicatorVariants);
  for (let i = 0; i < 200; i += 1) {
    assert.ok(valid.has(pickRunIndicatorVariant(`run-${i}`)), `run-${i} 抽到了集合外的变体`);
  }
});

test("变体抽取：7 个变体都能被抽到（不是名义随机、实际只剩两三个）", () => {
  const seen = new Set<RunIndicatorVariant>();
  for (let i = 0; i < 2000; i += 1) {
    seen.add(pickRunIndicatorVariant(`run-${i}-${i * 7}`));
  }
  assert.deepEqual(
    runIndicatorVariants.filter((variant) => !seen.has(variant)).sort(),
    [],
    "有变体从未被抽到：随机分布有洞",
  );
});

test("变体抽取：哈希吃到完整 runId（改一位就换变体的概率足够高）", () => {
  const base = pickRunIndicatorVariant("run-shared-prefix");
  let changed = 0;
  for (let i = 0; i < 50; i += 1) {
    if (pickRunIndicatorVariant(`run-shared-prefix-${i}`) !== base) changed += 1;
  }
  assert.ok(changed >= 10, `50 个不同 runId 里只有 ${changed} 个换了变体，哈希可能只读了前缀`);
});

// ── 分类：文字位变体抽取（与图形位同一套机制的第二条随机轴） ──────────────
test("文字变体抽取：同一 runId 永远抽到同一个（重渲染/重连不换花样）", () => {
  const first = pickRunTextVariant("run-abc-123");
  for (let i = 0; i < 20; i += 1) {
    assert.equal(pickRunTextVariant("run-abc-123"), first);
  }
});

test("文字变体抽取：结果必在 5 个定稿变体之内", () => {
  const valid = new Set<string>(runTextVariants);
  for (let i = 0; i < 200; i += 1) {
    assert.ok(valid.has(pickRunTextVariant(`run-${i}`)), `run-${i} 抽到了集合外的文字变体`);
  }
});

test("文字变体抽取：5 个变体都能被抽到（不是名义随机、实际只剩两三个）", () => {
  const seen = new Set<RunTextVariant>();
  for (let i = 0; i < 2000; i += 1) {
    seen.add(pickRunTextVariant(`run-${i}-${i * 7}`));
  }
  assert.deepEqual(
    runTextVariants.filter((variant) => !seen.has(variant)).sort(),
    [],
    "有文字变体从未被抽到：随机分布有洞",
  );
});

test("文字变体抽取：种子确实带 ':text' 后缀（与图形位同哈希但独立投影）", () => {
  // 第一版这里写的是"两个枚举值字符串相等计数"——两个枚举没有交集字符串，
  // same 恒为 0，就算实现退化成与图形位共用种子它也永远绿（无效断言）。
  // 改为钉种子推导：测试内独立重算 FNV-1a，去掉 ':text' 后缀或改哈希必红。
  const fnv = (input: string) => {
    let hash = 0x811c9dc5;
    for (let index = 0; index < input.length; index += 1) {
      hash ^= input.charCodeAt(index);
      hash = Math.imul(hash, 0x01000193);
    }
    return Math.abs(hash);
  };
  for (const runId of ["run-1", "run-abc-123", "run-axis-check", "会话:锚点"]) {
    const expected = runTextVariants[fnv(`${runId}:text`) % runTextVariants.length];
    assert.equal(pickRunTextVariant(runId), expected, `runId=${runId} 的文字变体偏离种子推导`);
  }
  // 顺带钉住"后缀真的参与了投影"：无后缀推导与有后缀推导对同一批 runId
  // 不能给出完全相同的序列（否则后缀形同虚设，两条轴实际同源）。
  const withSuffix = ["run-1", "run-abc-123", "run-axis-check"].map(pickRunTextVariant);
  const withoutSuffix = ["run-1", "run-abc-123", "run-axis-check"].map(
    (runId) => runTextVariants[fnv(runId) % runTextVariants.length],
  );
  assert.notDeepEqual(withSuffix, withoutSuffix, "':text' 后缀没有参与投影，两条轴实际同源");
});

test("拆字：空格换成不换行空格（inline-block span 里普通空格会塌掉），其余原样", () => {
  assert.deepEqual(splitLabelChars("ab c"), ["a", "b", "\u00A0", "c"]);
  assert.equal(splitLabelChars("正在处理").length, 4);
  assert.deepEqual(splitLabelChars(""), []);
});

// ── 分类：阶段词取哪条 ────────────────────────────────────────────────────
test("阶段词：压缩中/重试中是进行中阶段", () => {
  const events = [
    systemEvent("e1", "2026-10-12T10:00:00Z", { subtype: "status", status: "compacting" }),
    systemEvent("e2", "2026-10-12T10:00:05Z", { subtype: "api_retry", attempt: 2 }),
  ];
  const stage = latestActiveStage(events, "run-1");
  assert.ok(stage);
  assert.equal(stage.title, "API 重试中");
});

test("阶段词：最新信号是结果类 ⇒ 当前没有进行中的阶段 ⇒ 回退 null", () => {
  const events = [
    systemEvent("e1", "2026-10-12T10:00:00Z", { subtype: "status", status: "compacting" }),
    systemEvent("e2", "2026-10-12T10:00:30Z", { subtype: "status", compact_result: "success" }),
  ];
  // 压缩已经被它自己的结果事件关闭：绝不能把更早的「正在压缩上下文」捞出来当阶段词，
  // 否则指示器会挂着「正在压缩上下文 · 45s」的假话。
  assert.equal(latestActiveStage(events, "run-1"), null);
  // 任务终态同理
  const events2 = [
    systemEvent("e3", "2026-10-12T10:00:00Z", { subtype: "task_updated", tasks: [{ id: "t1", status: "running" }] }),
    systemEvent("e4", "2026-10-12T10:00:40Z", { subtype: "task_updated", tasks: [{ id: "t1", status: "completed" }] }),
  ];
  assert.equal(latestActiveStage(events2, "run-1"), null);
});

test("阶段词：多个进行中阶段取 createdAt 最新的（顺序不可靠，时间才可靠）", () => {
  const events = [
    systemEvent("e-new", "2026-10-12T10:00:20Z", { subtype: "status", status: "compacting" }),
    systemEvent("e-old", "2026-10-12T10:00:10Z", { subtype: "api_retry" }),
  ];
  assert.equal(latestActiveStage(events, "run-1")?.title, "正在压缩上下文");
});

test("阶段词：没有进行中阶段时如实返回 null（不许把读不到写成有）", () => {
  assert.equal(latestActiveStage([], "run-1"), null);
  assert.equal(latestActiveStage([systemEvent("e1", "2026-10-12T10:00:00Z", { subtype: "status", compact_result: "success" })], "run-1"), null);
  // 非 system 事件（工具输出、消息）不产生阶段
  assert.equal(latestActiveStage([{ id: "t1", type: "tool", payload: {}, runId: "run-1", createdAt: "2026-10-12T10:00:00Z" }], "run-1"), null);
});

test("阶段词：只认当前 runId —— 上一轮 run 的阶段事件不许串进新一轮", () => {
  const events = [
    { id: "e-old", type: "system", payload: { subtype: "api_retry" }, runId: "run-prev", createdAt: "2026-10-12T10:00:30Z" },
    { id: "e-old2", type: "system", payload: { subtype: "status", status: "compacting" }, runId: "run-prev", createdAt: "2026-10-12T10:00:10Z" },
  ];
  // 当前 run 没有任何阶段事件：上一轮的重试中就算再新也不算数
  assert.equal(latestActiveStage(events, "run-current"), null);
  // 当前 run 自己的事件正常命中
  events.push({ id: "e-new", type: "system", payload: { subtype: "status", status: "compacting" }, runId: "run-current", createdAt: "2026-10-12T11:00:00Z" });
  assert.equal(latestActiveStage(events, "run-current")?.title, "正在压缩上下文");
});

// ── 读数：阶段计时 ────────────────────────────────────────────────────────
test("计时：10 秒内不显示；阈值本身按手机端同口径（ms < 阈值）显示 10s", () => {
  assert.equal(formatStageClock(0), "");
  assert.equal(formatStageClock(9_999), "");
  assert.equal(formatStageClock(stageClockVisibleAfterMs), "10s");
});

test("计时：过阈值按 CLI 风格显示秒与分", () => {
  assert.equal(formatStageClock(stageClockVisibleAfterMs + 1), "10s");
  assert.equal(formatStageClock(59_999), "59s");
  assert.equal(formatStageClock(61_000), "1m01s");
  assert.equal(formatStageClock(600_000), "10m00s");
});

test("计时：坏输入（NaN / 负数）返回空串，绝不显示 NaNs 或负数", () => {
  assert.equal(formatStageClock(Number.NaN), "");
  assert.equal(formatStageClock(-3_000), "");
});

test("计时：minMs=0 立即显示（移动端口径），负数仍拦", () => {
  assert.equal(formatStageClock(0, 0), "0s");
  assert.equal(formatStageClock(8_000, 0), "8s");
  assert.equal(formatStageClock(-3_000, 0), "");
  assert.equal(formatStageClock(Number.NaN, 0), "");
});
