import assert from "node:assert/strict";
import test from "node:test";

import {
  AGENT_HEARTBEAT_FRESH_MS,
  desktopServiceState,
  desktopServiceView,
  heartbeatAgoText,
  isAgentAlive,
  refreshDesktopStatusMessage,
  shortInstanceID,
  type DesktopAgentStatus,
} from "./desktop-service";

const READY: DesktopAgentStatus = {
  ready: true,
  instanceId: "8f3c1d2a-4b5e-4f60-9a71-2c6d8e0fa91b",
  cloudUrl: "https://keyanjia.info:8443",
  heartbeatAt: new Date().toISOString(),
};
const UNREGISTERED: DesktopAgentStatus = { ready: false, instanceId: "", cloudUrl: "", heartbeatAt: "" };

// 这一段是从页面组件里抽出来的**优先级级联**，所以必须用行为断言守，不能扫源码正则：
// 把 `agentAlive` 换成 `true` 之后五句文案都还在，源码级断言照样绿 —— 2026-09-17 变异检验实测漏网。
test("desktop service state walks the cascade in priority order", () => {
  // ① 还没读回来 > 一切：这时不该对"注册没注册"下任何结论。
  assert.equal(desktopServiceState("loading", READY, 100), "loading");
  assert.equal(desktopServiceState("loading", null, null), "loading");
  // ② 读不到 > 未注册：前者要重试，后者要注册，混起来用户会去点错的按钮。
  assert.equal(desktopServiceState("failed", READY, 100), "failed");
  assert.equal(desktopServiceState("failed", null, null), "failed");
  // ③ 凭据不在 ⇒ 未注册（心跳是 null 也不影响这一档）。
  assert.equal(desktopServiceState("loaded", UNREGISTERED, null), "unregistered");
  assert.equal(desktopServiceState("loaded", null, null), "unregistered");
  // ④ ready + 心跳新鲜 ⇒ 运行中。
  assert.equal(desktopServiceState("loaded", READY, 0), "live");
  assert.equal(desktopServiceState("loaded", READY, AGENT_HEARTBEAT_FRESH_MS - 1), "live");
  // ⑤ ready 但心跳停了 ⇒ **无心跳**。这一档是旧版完全缺的：
  //    凭据还在（所以不是"未注册"），可手机端什么都收不到。
  assert.equal(desktopServiceState("loaded", READY, AGENT_HEARTBEAT_FRESH_MS), "stale");
  assert.equal(desktopServiceState("loaded", READY, 12 * 60 * 1000), "stale");
  assert.equal(desktopServiceState("loaded", READY, null), "stale");
});

test("desktop service freshness threshold and liveness stay pinned to the design values", () => {
  // 阈值是设计参数（Agent 心跳 750ms × 20），不是随手写的数：引用常量的断言两边同步变，
  // 所以必须有一条把数值本身钉死。
  assert.equal(AGENT_HEARTBEAT_FRESH_MS, 15_000);
  assert.equal(AGENT_HEARTBEAT_FRESH_MS, Math.round(AGENT_HEARTBEAT_FRESH_MS));
  assert.equal(isAgentAlive(0), true);
  assert.equal(isAgentAlive(AGENT_HEARTBEAT_FRESH_MS - 1), true);
  assert.equal(isAgentAlive(AGENT_HEARTBEAT_FRESH_MS), false);
  // 负数（控制服务与本机时钟不同步 / 时钟往回跳）按"新鲜"处理：那说明心跳时刻落在将来，
  // 不可能是"进程死了"。这条**故意钉住**行为 —— 它是判据 `age < 阈值` 的自然结果，
  // 但"负数该怎么办"没写下来时很容易被下一个人当成 bug 改掉。
  assert.equal(isAgentAlive(-1), true);
  assert.equal(heartbeatAgoText(-1), "刚刚");
  // NaN / null 都不能算活着（曾经写成 `ageMs < 阈值` 的话 NaN 会让它变 stale，行为一致，
  // 但 `null < 阈值` 在 JS 里是 **true**（null 转成 0）—— 那会让"从没听到过心跳"被判成"运行中"。
  // 这一条就是为它守的。）
  assert.equal(isAgentAlive(null), false);
  assert.equal(isAgentAlive(Number.NaN), false);
});

test("desktop service text covers every state and only stale/failed carry a fix", () => {
  const loading = desktopServiceView("loading", null, null);
  const failed = desktopServiceView("failed", null, null);
  const unregistered = desktopServiceView("loaded", UNREGISTERED, null);
  const live = desktopServiceView("loaded", READY, 1000);
  const stale = desktopServiceView("loaded", READY, 60 * 60 * 1000);

  assert.deepEqual([loading.state, failed.state, unregistered.state, live.state, stale.state],
    ["loading", "failed", "unregistered", "live", "stale"]);
  // 五档文案两两不同：任何两档共用一句话，用户就分不出自己在哪一档。
  const labels = [loading, failed, unregistered, live, stale].map((item) => item.label);
  assert.equal(new Set(labels).size, 5, `五档文案应当各不相同：${JSON.stringify(labels)}`);
  const chips = [loading, failed, unregistered, live, stale].map((item) => item.chip);
  assert.equal(new Set(chips).size, 5, `五档胶囊也应当各不相同：${JSON.stringify(chips)}`);
  // 页头用整句、侧栏用一个词 —— 两者不能是同一份（否则侧栏那颗胶囊会撑破卡片头）。
  for (const item of [loading, failed, unregistered, live, stale]) {
    assert.ok(item.chip.length <= 4, `胶囊过长：${item.chip}`);
    assert.ok(item.label.length >= item.chip.length, "整句不该比胶囊短");
  }
  // "正常"两档不给 hint（没事别说话）；出问题的两档必须给能照着做的修法。
  assert.equal(live.hint, "");
  assert.equal(loading.hint, "");
  assert.match(failed.hint, /重启 Milevia/);
  assert.match(stale.hint, /收不到本机 Agent 的心跳/);
  // ⚠️ **心跳停的两种原因都要说出来**：那条 ticker 只在 `runConnection`（连着云端时）里跑，
  // 所以"收不到心跳"既可能是 Agent 没在运行，也可能只是连不上云端。只写其中一种，
  // 就会把另一种情况的用户指去错的动作（重启进程 / 查网络，是两件事）。
  // 2026-09-17 复查时发现旧文案写的是"Agent 未在运行 / 重启 Milevia 之后重试"，正是这个毛病。
  assert.match(stale.hint, /没在运行/);
  assert.match(stale.hint, /连不上云端/);
  assert.match(stale.hint, /旧数据/);
  assert.match(unregistered.hint, /注册/);
});

test("heartbeat age is phrased in human units with two distinct empty cases", () => {
  assert.equal(heartbeatAgoText(null), "尚未收到");
  assert.equal(heartbeatAgoText(Number.NaN), "尚未收到");
  assert.equal(heartbeatAgoText(0), "刚刚");
  // 秒那一档按**四舍五入后的秒数**判：4499ms 舍成 4 秒 → "刚刚"；4500ms 舍成 5 秒 → "5 秒前"。
  assert.equal(heartbeatAgoText(4_499), "刚刚");
  assert.equal(heartbeatAgoText(4_500), "5 秒前");
  assert.equal(heartbeatAgoText(5_000), "5 秒前");
  assert.equal(heartbeatAgoText(59_000), "59 秒前");
  assert.equal(heartbeatAgoText(60_000), "1 分钟前");
  // 大单位用 floor，所以每一档的上下界都是"分/时/天"的自然边界：
  // 3599 秒是 59 分（不是 60 分，更不是 1 小时），86399 秒是 23 小时（不是 1 天）。
  assert.equal(heartbeatAgoText(3_599_000), "59 分钟前");
  assert.equal(heartbeatAgoText(3_600_000), "1 小时前");
  assert.equal(heartbeatAgoText(86_399_000), "23 小时前");
  assert.equal(heartbeatAgoText(86_400_000), "1 天前");
  assert.equal(heartbeatAgoText(3 * 86_400_000), "3 天前");
  // 措辞里不该出现"1970"这类由 0 值泄漏出来的东西 —— 服务端回空串正是为了避免它。
  for (const age of [0, 1, 5_000, 3_600_000, 86_400_000]) {
    assert.doesNotMatch(heartbeatAgoText(age), /1970|Invalid|NaN/);
  }
});

test("instance id is abbreviated for display but keeps both ends recognisable", () => {
  const full = "8f3c1d2a-4b5e-4f60-9a71-2c6d8e0fa91b";
  const short = shortInstanceID(full);
  assert.equal(short, "8f3c1d…a91b");
  assert.ok(short.length < full.length);
  assert.ok(short.startsWith("8f3c1d") && short.endsWith("a91b"));
  assert.equal(shortInstanceID(" 8f3c1d2a "), "8f3c1d2a", "两侧空白要先 trim");
  assert.equal(shortInstanceID("abc"), "abc");
  assert.equal(shortInstanceID(""), "");
  // 12 字以内原样返回（不为了好看把短 ID 也切开）。
  assert.equal(shortInstanceID("0123456789ab"), "0123456789ab");
  assert.equal(shortInstanceID("0123456789abc"), "012345…9abc");
});

test("desktop refresh speaks in this machine's terms, never the phone's", () => {
  const ready = refreshDesktopStatusMessage("ready");
  assert.equal(ready.state, "success");
  assert.match(ready.message, /服务状态与绑定信息/);

  const unregistered = refreshDesktopStatusMessage("unregistered");
  assert.equal(unregistered.state, "failed");
  assert.match(unregistered.message, /尚未注册/);

  const failed = refreshDesktopStatusMessage("failed");
  assert.equal(failed.state, "failed");
  assert.match(failed.message, /Milevia 正在运行/);

  // ⚠️ 三条都不许出现手机那一侧的动作。`refreshNow`（手机）在没有令牌时会说
  // 「尚未配对电脑，请先扫码配对」—— 电脑端自己就是被配对的那台机器，这句话在真机上是纯误导。
  for (const outcome of ["ready", "unregistered", "failed"] as const) {
    const { message } = refreshDesktopStatusMessage(outcome);
    assert.doesNotMatch(message, /扫码配对/);
    assert.doesNotMatch(message, /配对电脑/);
  }
});
