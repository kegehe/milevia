import assert from "node:assert/strict";
import { test } from "node:test";

import {
  PHONE_SYNC_FRESH_MS,
  normalizeBindings,
  phoneSyncAgoText,
  phoneSyncState,
  phoneSyncView,
  platformLabel,
  syncAgeFrom,
  type DesktopBinding,
} from "./desktop-phone";

// 手机这一块的断言分两层：
//   ① **行为断言**（本文件）—— 直接调函数，改一个分支/阈值当场红。
//   ② **接线断言**（mobile-remote-agent.test.mjs）—— 页面里那两个 useEffect 还在不在。
// 只做 ② 会漏"只有接线、判据是错的"，只做 ① 会漏"函数写对了但没人调"。

/** 阈值本身要钉死：写成魔数的话，把它从 90 秒改到 5 秒只需要改一处，
 *  引用常量的断言两边同步变、照样绿。
 *
 *  ⚠️ 90 秒是**算出来的**（30 秒节流窗口 + 5 秒轮询 = 35 秒最坏陈旧度，取 ≈2.6 倍余量），
 *  所以把这条断言改成别的数之前，先去 desktop-phone.ts 把那行算式重算一遍 ——
 *  不是"顺手调大一点"。 */
test("在线判据的阈值是推导出来的设计参数，不是随手写的数", () => {
  assert.equal(PHONE_SYNC_FRESH_MS, 90_000);
});

test("绑定项归一化：非对象项丢掉，不渲染成一行空白", () => {
  const items = normalizeBindings([null, "x", 42, { deviceName: "Xiaomi 14" }, { deviceName: "iPhone" }]);
  assert.equal(items.length, 2);
  assert.deepEqual(items.map((item) => item.deviceName), ["Xiaomi 14", "iPhone"]);
});

test("绑定项归一化：非数组一律当空列表", () => {
  for (const raw of [undefined, null, "x", 42, {}]) {
    assert.deepEqual(normalizeBindings(raw), []);
  }
});

test("lastUsedAt 的「键不存在」与「值为 null」必须是两回事", () => {
  // 云端旧版本：整个键都没有。
  const unsupported = normalizeBindings([{ deviceName: "A" }]);
  assert.equal("lastUsedAt" in unsupported[0], false, "缺键时不许补出这个键");
  assert.equal(phoneSyncState(unsupported[0], null), "unsupported");

  // 云端新版本、但这台手机绑定后没发过请求：键在、值是 null。
  const never = normalizeBindings([{ deviceName: "A", lastUsedAt: null }]);
  assert.equal("lastUsedAt" in never[0], true);
  assert.equal(phoneSyncState(never[0], null), "never");
});

test("lastUsedAt 收到空串当「没同步过」，不当成时间", () => {
  const binding: DesktopBinding = { deviceName: "A", activatedAt: "", lastUsedAt: "" };
  assert.equal(phoneSyncState(binding, 0), "never");
});

test("lastUsedAt 收到不认识的类型时不许下结论（既不是时间，也不是「没同步过」）", () => {
  // 值不是字符串也不是 null（比如将来上游改成 epoch 毫秒）：我们**读不懂**，
  // 那就只能说"读不到这项读数"，不能说"这台手机没在同步" —— 后者是把我们的
  // 解析失败栽赃给手机，用户会跑去手机上找原因。
  for (const raw of [1737072000000, true, {}, []]) {
    const items = normalizeBindings([{ deviceName: "A", lastUsedAt: raw }]);
    assert.equal("lastUsedAt" in items[0], false, `值 ${JSON.stringify(raw)} 不该被当成读数`);
    assert.equal(phoneSyncState(items[0], 0), "unsupported", `值 ${JSON.stringify(raw)} 应当走 unsupported`);
  }
});

test("syncAgeFrom：合法时间给年龄，缺/非法一律 null（两个调用点共用它）", () => {
  const now = Date.now();
  const age = syncAgeFrom(new Date(now - 3_000).toISOString());
  assert.ok(age !== null && age >= 3_000 && age < 5_000, `age=${age}`);
  for (const raw of [null, undefined, "", "not-a-time"]) {
    assert.equal(syncAgeFrom(raw), null, `syncAgeFrom(${JSON.stringify(raw)})`);
  }
  // 负数（时刻落在将来）要原样返回，不许夹到 0：判据靠"负数算新鲜"，
  // 夹到 0 会让"时钟往回跳"和"刚好现在"变得不可区分（虽然都判在线，但语义不同）。
  const future = syncAgeFrom(new Date(now + 60_000).toISOString());
  assert.ok(future !== null && future < 0, `future=${future}`);
});

test("有值但解析不出来时归 unsupported，不是「没同步过」", () => {
  // `null` 是云端在说"这台手机绑定后一次都没同步过"；而一个我们读不懂的值
  // 什么也没说 —— 那是**解析失败**。两者都在"没拿到可用年龄"这一列，
  // 但把它们并成一个，就等于又一次把我们的失败说成手机的事实。
  const neverCase = normalizeBindings([{ deviceName: "A", lastUsedAt: null }])[0];
  assert.equal(phoneSyncState(neverCase, syncAgeFrom(neverCase.lastUsedAt)), "never");

  const garbage = normalizeBindings([{ deviceName: "A", lastUsedAt: "not-a-time" }])[0];
  assert.equal("lastUsedAt" in garbage, true, "字符串值本身是认识的类型，要保留下来");
  assert.equal(phoneSyncState(garbage, syncAgeFrom(garbage.lastUsedAt)), "unsupported");
  assert.equal(phoneSyncView(garbage, syncAgeFrom(garbage.lastUsedAt)).agoText, "");
});

test("新不新鲜按年龄分岔，边界取左闭右开", () => {
  const binding: DesktopBinding = { deviceName: "A", activatedAt: "", lastUsedAt: "2026-09-17T16:00:00Z" };
  assert.equal(phoneSyncState(binding, 0), "online");
  assert.equal(phoneSyncState(binding, PHONE_SYNC_FRESH_MS - 1), "online");
  assert.equal(phoneSyncState(binding, PHONE_SYNC_FRESH_MS), "idle");
  assert.equal(phoneSyncState(binding, 10 * 60_000), "idle");
});

test("负年龄算新鲜：时刻落在将来不可能是「手机没在同步」", () => {
  const binding: DesktopBinding = { deviceName: "A", activatedAt: "", lastUsedAt: "2026-09-17T16:00:00Z" };
  assert.equal(phoneSyncState(binding, -5_000), "online");
  assert.equal(phoneSyncAgoText(-5_000), "刚刚");
});

test("没有绑定项时是 unsupported，不是 never", () => {
  assert.equal(phoneSyncState(null, null), "unsupported");
});

test("unsupported 那一档的文案里不许出现「未同步」", () => {
  const view = phoneSyncView({ deviceName: "A", activatedAt: "" }, null);
  assert.equal(view.state, "unsupported");
  assert.doesNotMatch(view.hint, /未同步/, "旧云端被说成手机没在同步，等于把宿主的问题栽赃给手机");
  assert.equal(view.chip, "");
  assert.equal(view.headerChip, "已绑定手机");
});

test("never / idle 的胶囊都是「未同步」，细节由 agoText 与 hint 承担", () => {
  const neverView = phoneSyncView({ deviceName: "A", activatedAt: "", lastUsedAt: null }, null);
  assert.equal(neverView.chip, "未同步");
  assert.equal(neverView.headerChip, "手机未同步");
  assert.equal(neverView.agoText, "还没有同步过");

  const idleView = phoneSyncView({ deviceName: "A", activatedAt: "", lastUsedAt: "2026-09-17T16:00:00Z" }, 3 * 60_000);
  assert.equal(idleView.chip, "未同步");
  assert.equal(idleView.agoText, "3 分钟前");
  assert.match(idleView.hint, /也可能连不上云端/, "idle 必须同时给出两种原因");
});

test("online 不给 hint：没事可做的时候别塞一句废话", () => {
  const view = phoneSyncView({ deviceName: "A", activatedAt: "", lastUsedAt: "2026-09-17T16:00:00Z" }, 1_000);
  assert.equal(view.state, "online");
  assert.equal(view.chip, "在线");
  assert.equal(view.headerChip, "手机在线");
  assert.equal(view.hint, "");
});

test("年龄文案：大单位取 floor，让「60 分钟前」这一档可达", () => {
  assert.equal(phoneSyncAgoText(3_000), "刚刚");
  assert.equal(phoneSyncAgoText(42_000), "42 秒前");
  assert.equal(phoneSyncAgoText(59_000), "59 秒前");
  assert.equal(phoneSyncAgoText(60_000), "1 分钟前");
  assert.equal(phoneSyncAgoText(3_599_000), "59 分钟前");
  assert.equal(phoneSyncAgoText(3_600_000), "1 小时前");
  assert.equal(phoneSyncAgoText(86_400_000), "1 天前");
  assert.equal(phoneSyncAgoText(Number.NaN), "还没有同步过");
});

test("平台只认闭集，未知值塌成空串（空串＝手机没上报，整行不渲染）", () => {
  assert.equal(platformLabel("android"), "Android");
  assert.equal(platformLabel("iOS"), "iOS");
  assert.equal(platformLabel("web"), "浏览器");
  assert.equal(platformLabel(""), "");
  assert.equal(platformLabel(undefined), "");
  assert.equal(platformLabel("android; drop table"), "");
  assert.equal(platformLabel("Windows"), "");
});
