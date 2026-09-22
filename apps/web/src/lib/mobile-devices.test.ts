import assert from "node:assert/strict";
import { test } from "node:test";
import {
  ACTIVE_STORAGE_KEY,
  DEVICES_STORAGE_KEY,
  DEVICE_ALIAS_MAX_LENGTH,
  LEGACY_TOKEN_STORAGE_KEY,
  addOrReplaceDevice,
  activeDevice,
  deviceDisplayName,
  deviceLabel,
  devicePlatform,
  markDeviceRevoked,
  normalizeAlias,
  readActiveToken,
  readDevices,
  reconcileDevices,
  removeDevice,
  setActiveToken,
  setDeviceAlias,
  updateDevice,
  writeDevices,
  type DeviceStore,
} from "./mobile-devices";

function fakeStore(seed: Record<string, string> = {}): DeviceStore & { dump: () => Record<string, string> } {
  const map = new Map(Object.entries(seed));
  return {
    getItem: (key) => (map.has(key) ? (map.get(key) as string) : null),
    setItem: (key, value) => void map.set(key, value),
    removeItem: (key) => void map.delete(key),
    dump: () => Object.fromEntries(map),
  };
}

test("手机端设备表：旧单令牌会被迁移成一台设备，并把旧键继续镜像回去", () => {
  const store = fakeStore({ [LEGACY_TOKEN_STORAGE_KEY]: "mvt_old" });
  const devices = reconcileDevices(store);
  assert.equal(devices.length, 1);
  assert.equal(devices[0].token, "mvt_old");
  // 迁移过来的记录还不知道它对应哪台电脑：instanceId 要等第一次 /v1/instances 回填。
  assert.equal(devices[0].instanceId, "");
  assert.equal(readActiveToken(store), "mvt_old");
  // 旧键必须还在：用户回退到旧版本 App 时仍然要能配对成功。
  assert.equal(store.dump()[LEGACY_TOKEN_STORAGE_KEY], "mvt_old");
});

test("手机端设备表：旧包配对的新电脑会被新包补进列表（双向兼容）", () => {
  const store = fakeStore();
  addOrReplaceDevice({ token: "mvt_a", instanceId: "pc-a", name: "家里台式机" }, store);
  // 模拟用户回退到旧包又配了一台：旧包只写 milevia.cloud.token。
  store.setItem(LEGACY_TOKEN_STORAGE_KEY, "mvt_b");
  const devices = reconcileDevices(store);
  assert.equal(devices.length, 2, "旧包新配的那台必须被认领，而不是被忽略");
  assert.ok(devices.some((item) => item.token === "mvt_b"));
  assert.equal(readActiveToken(store), "mvt_b", "当前设备跟随旧键，否则用户会以为新配的没生效");
});

test("手机端设备表：同一台电脑重新配对时旧记录让位（不会出现两台同名电脑）", () => {
  const store = fakeStore();
  addOrReplaceDevice({ token: "mvt_old", instanceId: "pc-a", name: "家里台式机" }, store);
  addOrReplaceDevice({ token: "mvt_new", instanceId: "pc-a", name: "家里台式机" }, store);
  const devices = readDevices(store);
  assert.equal(devices.length, 1);
  assert.equal(devices[0].token, "mvt_new");
  assert.equal(readActiveToken(store), "mvt_new");
});

test("手机端设备表：被顶掉的那台要留记录并标记，不能静默消失", () => {
  const store = fakeStore();
  addOrReplaceDevice({ token: "mvt_a", instanceId: "pc-a", name: "家里台式机" }, store);
  addOrReplaceDevice({ token: "mvt_b", instanceId: "pc-b", name: "公司笔记本" }, store);
  setActiveToken("mvt_a", store);
  markDeviceRevoked("mvt_a", store);
  const devices = readDevices(store);
  assert.equal(devices.length, 2, "被顶替的记录必须留着，界面靠它解释'为什么突然连不上了'");
  assert.equal(devices.find((item) => item.token === "mvt_a")?.revoked, true);
  // 当前设备被标记后，activeDevice 不能继续把它当可用设备返回。
  assert.equal(activeDevice(store)?.token, "mvt_a", "当前设备仍指向它（界面要能显示'需要重新配对'）");
  assert.equal(activeDevice(store)?.revoked, true);
});

test("手机端设备表：解绑一台后当前设备自动落到剩下那台", () => {
  const store = fakeStore();
  addOrReplaceDevice({ token: "mvt_a", instanceId: "pc-a", name: "家里台式机" }, store);
  addOrReplaceDevice({ token: "mvt_b", instanceId: "pc-b", name: "公司笔记本" }, store);
  removeDevice("mvt_b", store);
  assert.deepEqual(readDevices(store).map((item) => item.token), ["mvt_a"]);
  assert.equal(readActiveToken(store), "mvt_a");
  assert.equal(store.dump()[LEGACY_TOKEN_STORAGE_KEY], "mvt_a");
});

test("手机端设备表：存储被写坏时当作没有设备，而不是抛异常", () => {
  const store = fakeStore({ [DEVICES_STORAGE_KEY]: "{not json" });
  assert.deepEqual(readDevices(store), []);
  const store2 = fakeStore({ [DEVICES_STORAGE_KEY]: JSON.stringify([{ name: "没有令牌" }, null, "x"]) });
  assert.deepEqual(readDevices(store2), []);
});

test("手机端设备表：回填电脑名与在线状态", () => {
  const store = fakeStore();
  addOrReplaceDevice({ token: "mvt_a", instanceId: "pc-a" }, store);
  updateDevice("mvt_a", { name: "家里台式机", status: "online", lastSeenAt: "2026-09-17T06:00:00Z" }, store);
  const device = readDevices(store)[0];
  assert.equal(device.name, "家里台式机");
  assert.equal(device.status, "online");
  assert.equal(device.lastSeenAt, "2026-09-17T06:00:00Z");
});

test("手机端设备表：写空列表也要把旧键一起清掉", () => {
  const store = fakeStore();
  writeDevices([], store);
  setActiveToken("", store);
  assert.equal(store.dump()[ACTIVE_STORAGE_KEY], undefined);
  assert.equal(store.dump()[LEGACY_TOKEN_STORAGE_KEY], undefined, "旧键残留会让旧包以为还配对着");
});

// ── 备注名（2026-09-21）────────────────────────────────────────────────────
// 这一个字段最容易出的三类错：① 归一化漏在某一侧（存进去带换行、比的时候是另一串）；
// ② 重新配对把备注弄丢；③ "清空"被实现成"名字变成空"。三条各有一个用例。

test("备注名：归一化折叠空白、去首尾、按上限截断", () => {
  assert.equal(normalizeAlias("  公司那台  "), "公司那台");
  // 移动端文本框的回车会带进换行：直接存会让面板那一行被撑高，必须先折叠成单空格。
  assert.equal(normalizeAlias("公司那台\n跑 CI"), "公司那台 跑 CI");
  assert.equal(normalizeAlias("公司\t\t那台"), "公司 那台");
  // 全空白 = 清空，不能存成"看起来有值、显示是空白"的一串空格。
  assert.equal(normalizeAlias("   \n  "), "");
  assert.equal(Array.from(normalizeAlias("电".repeat(50))).length, DEVICE_ALIAS_MAX_LENGTH);
});

// 截断必须按**码点**走。`.slice(0, 24)` 按 UTF-16 码元切，正好切在代理对中间时会留下
// 一个孤立代理 —— 界面上是方块（�），存储里是拼不回去的坏数据。这类"看着只是少一个字符"的
// 缺陷最容易在 code review 里滑过去，所以专门钉一条。
test("备注名：超长截断不会切坏 emoji（不留孤立代理）", () => {
  const sliced = normalizeAlias("🎉".repeat(30));
  assert.equal(Array.from(sliced).length, DEVICE_ALIAS_MAX_LENGTH, "按码点数截断");
  assert.equal(sliced.includes("\uFFFD"), false, "不能出现替换字符（孤立代理会被渲染成这个）");
  assert.doesNotMatch(
    sliced,
    /[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/,
    "不能留下半个代理对",
  );
  // 混合内容：23 个 ASCII + 1 个 emoji 正好 24 码点（但 25 个码元），emoji 必须完整保留。
  const mixed = normalizeAlias("a".repeat(23) + "🎉");
  assert.equal(mixed, "a".repeat(23) + "🎉");
  assert.equal(Array.from(mixed).length, 24);
});

test("备注名：显示名优先用备注，清空后备注回落真名", () => {
  assert.equal(deviceDisplayName({ alias: "公司那台", name: "DESKTOP-8F2K" }), "公司那台");
  assert.equal(deviceDisplayName({ alias: "", name: "DESKTOP-8F2K" }), "DESKTOP-8F2K");
  // 只有空格也算没起备注 —— 否则界面上会出现一个"看不见的名字"。
  assert.equal(deviceDisplayName({ alias: "   ", name: "DESKTOP-8F2K" }), "DESKTOP-8F2K");
  // 两个都没有时才是"未命名电脑"（真名还没从云端回填的那一刻）。
  assert.equal(deviceDisplayName({ alias: "", name: "" }), "未命名电脑");
});

test("备注名：写进去、读出来，并且不动电脑真名", () => {
  const store = fakeStore();
  addOrReplaceDevice({ token: "mvt_a", instanceId: "pc-a", name: "DESKTOP-8F2K" }, store);
  setDeviceAlias("mvt_a", "公司那台", store);
  const device = readDevices(store)[0];
  assert.equal(device.alias, "公司那台");
  assert.equal(device.name, "DESKTOP-8F2K", "真名必须留着：面板小字要靠它说'原名是什么'");
  // 清空 = 回落真名，而不是把 alias 写成空白串留在存储里。
  setDeviceAlias("mvt_a", "  ", store);
  assert.equal(readDevices(store)[0].alias, "");
  assert.equal(deviceDisplayName(readDevices(store)[0]), "DESKTOP-8F2K");
});

test("备注名：同一台电脑重新配对时必须继承，不能被新记录顶掉", () => {
  const store = fakeStore();
  addOrReplaceDevice({ token: "mvt_old", instanceId: "pc-a", name: "DESKTOP-8F2K" }, store);
  setDeviceAlias("mvt_old", "公司那台", store);
  // 重新配对（顶替 / 令牌过期都要走这一步）会拿到新令牌，旧记录被替换掉。
  addOrReplaceDevice({ token: "mvt_new", instanceId: "pc-a", name: "DESKTOP-8F2K" }, store);
  const devices = readDevices(store);
  assert.equal(devices.length, 1);
  assert.equal(devices[0].token, "mvt_new");
  assert.equal(devices[0].alias, "公司那台", "重新配对丢备注，等于让用户白起一次名字");
});

test("备注名：instanceId 还没回填时，靠令牌认人也要继承", () => {
  const store = fakeStore();
  addOrReplaceDevice({ token: "mvt_old", instanceId: "", name: "" }, store);
  setDeviceAlias("mvt_old", "家里那台", store);
  addOrReplaceDevice({ token: "mvt_old", instanceId: "pc-a", name: "DESKTOP-8F2K" }, store);
  assert.equal(readDevices(store)[0].alias, "家里那台");
});

test("备注名：真·新电脑不会被旁边那台的备注串味", () => {
  const store = fakeStore();
  addOrReplaceDevice({ token: "mvt_a", instanceId: "pc-a", name: "DESKTOP-8F2K" }, store);
  setDeviceAlias("mvt_a", "公司那台", store);
  addOrReplaceDevice({ token: "mvt_b", instanceId: "pc-b", name: "MacBook-Pro" }, store);
  assert.equal(readDevices(store).find((item) => item.token === "mvt_b")?.alias, "");
  assert.equal(deviceDisplayName(readDevices(store).find((item) => item.token === "mvt_b")!), "MacBook-Pro");
});

test("备注名：旧存储里没有这个键时读成空串（向后兼容）", () => {
  const store = fakeStore({
    [DEVICES_STORAGE_KEY]: JSON.stringify([{ token: "mvt_a", instanceId: "pc-a", name: "DESKTOP-8F2K", status: "online" }]),
  });
  const device = readDevices(store)[0];
  assert.equal(device.alias, "");
  assert.equal(deviceDisplayName(device), "DESKTOP-8F2K");
});

test("备注名：被顶掉的那台也留着备注（记录不删，备注也不删）", () => {
  const store = fakeStore();
  addOrReplaceDevice({ token: "mvt_a", instanceId: "pc-a", name: "DESKTOP-8F2K" }, store);
  setDeviceAlias("mvt_a", "公司那台", store);
  markDeviceRevoked("mvt_a", store);
  assert.equal(readDevices(store)[0].alias, "公司那台");
});

test("手机端上报的设备名：从 UA 里取型号，取不到时给一个能看懂的名字", () => {
  assert.equal(
    deviceLabel("Mozilla/5.0 (Linux; Android 14; SM-S9210 Build/UP1A; wv) AppleWebKit/537.36 Chrome/120 Mobile Safari/537.36"),
    "SM-S9210",
  );
  assert.equal(deviceLabel("Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15"), "iPhone");
  assert.equal(deviceLabel("Mozilla/5.0 (Linux; Android 13; wv) AppleWebKit/537.36"), "Android 手机");
  assert.equal(deviceLabel(""), "手机");
});

// 上报给云端的平台标识。**闭集**，与云端 `sanitizePlatform` 一一对应 ——
// 两边任何一边放宽，另一边就会把不认识的值渲染到电脑端屏幕上。
// 空串是正常结果（浏览器里打开 /mobile、或旧包不带这个字段），不是错误。
test("手机端上报的平台：只认闭集，其它一律塌成空串", () => {
  assert.equal(devicePlatform("android"), "android");
  assert.equal(devicePlatform("iOS"), "ios");
  assert.equal(devicePlatform(" web "), "web");
  assert.equal(devicePlatform(""), "");
  assert.equal(devicePlatform("windows"), "");
  assert.equal(devicePlatform("android; drop table"), "");
});
