// 手机端"我连过哪些电脑"的唯一存储入口。
//
// 背景（2026-09-17）：云端一个令牌只能看见一台电脑（`userAuth` 把 instance 塞进请求
// 上下文，`listInstances` 带 scope 时 `where instance_id=$1`），所以"一个手机连多台电脑"
// 在数据层就是"存多个令牌 + 选其中一个当当前"。之前这里只存了单个
// `milevia.cloud.token`，手机因此天生只能连一台。
//
// 三条必须守住的规则：
//   1. **令牌是这台设备的本地主键**（云端一台电脑同时只发一个有效令牌，见顶替逻辑）。
//   2. **旧键 `milevia.cloud.token` 双向兼容**：读的时候要认它，写的时候要镜像它 ——
//      新包与旧包会共存（用户上午用新包绑第二台，下午回退到旧包）。
//   3. **"失效"不等于"没有"**：被另一台手机顶掉时，记录要留着并标记 `revoked`，
//      否则用户只会看到"配对莫名消失了"，而不是"这台电脑已经被另一台手机接管"。

export const DEVICES_STORAGE_KEY = "milevia.cloud.devices";
export const ACTIVE_STORAGE_KEY = "milevia.cloud.active";
export const LEGACY_TOKEN_STORAGE_KEY = "milevia.cloud.token";

/**
 * 「备注名」的长度上限。**必须与 UI 的 `maxLength` 用同一个常量**：两处各写一个数字，
 * 迟早会出现"输入框允许 30 字、存储只留 24 字"这种前半截生效、后半截被吃掉的怪事。
 *
 * 为什么是 24（**实测过的数，不是拍的**）：320px 屏的设备面板行里，名字可用宽度 126~143px
 * （探针 `probe-mobile-pairing-page` 量的），中文 14px 字号约 9 字/行 ⇒ 24 字是 **3 行**、
 * 整行高 113px（探针钉了 <140px 的上限，防它继续涨）。选 24 就是"能认出是哪台"和
 * "不把列表撑成一片"之间的折中。
 * ⚠️ **别把"两行封顶"当成这里的依据**：卡片 `.mobile-device-who b` 有 `-webkit-line-clamp: 2`，
 * 面板行**没有** —— 长的备注名在卡片上会被截断、在面板行里则完整显示 3 行，这是两个刻意不同的取舍。
 */
export const DEVICE_ALIAS_MAX_LENGTH = 24;

export type MobileDevice = {
  /** 本地主键，也是发请求时用的凭据。 */
  token: string;
  /** 首次成功拉到实例列表后回填（旧数据迁移过来的记录一开始不知道）。 */
  instanceId: string;
  /** 电脑名（来自 `/v1/instances`），切换面板上显示的就是它。 */
  name: string;
  /**
   * 用户给这台电脑起的**备注名**（"公司那台""跑 CI 的"）。空串＝没起过。
   *
   * 三条语义必须钉住，否则很容易被后来的人"优化"坏：
   *   1. **纯本地**：这是"这台手机上的这个人"的私有标注，不上传云端、也不回写电脑端的
   *      `deviceName`（那是手机报给电脑的名字，与"我给电脑起的名"是两件事）。
   *   2. **只覆盖显示，不替代 `name`**：真名照旧留着，面板小字里还要靠它说"原名是什么"。
   *   3. **可以随时清空**：清空即回落到 `name`，不是"名字变成空"。
   */
  alias: string;
  /** online / offline。 */
  status: string;
  boundAt: string;
  lastSeenAt: string;
  /** 云端已拒绝这个令牌：过期，或被另一台手机顶替。置位后不再用它发任何请求。 */
  revoked: boolean;
};

export type DeviceStore = Pick<Storage, "getItem" | "setItem" | "removeItem">;

/**
 * 备注名的唯一归一化入口（读写两侧都走它，避免"存的时候没 trim、比的时候 trim"这类不一致）。
 *
 * 三件事，顺序不能换：
 *   1. **先折叠空白**：备注可能在输入法里带进换行（移动端回车换行、粘贴多行）——
 *      直接存进去会让设备面板那一行被撑成三行以上。折叠成单空格后，它才是"一行文字"。
 *   2. **再 trim**：全空格 = 清空，不能存成 `"   "`（那会让界面显示一个空白名字）。
 *   3. **最后截断**：长度上限在这里兜底 —— 老数据、被手改过的 localStorage 都可能超长。
 *
 * ⚠️ 截断按**码点**（`Array.from`）而不是 `slice`（UTF-16 码元）：`.slice(0, 24)` 正好切在
 * 代理对中间时会留下一个**孤立代理** —— 界面上是一个方块（�），存储里是一串再也拼不回去的
 * 坏数据。备注名里放 emoji 的人不多，但"用户输入什么都能原样存回来"是底线。
 * 单测里有一条专钉这个（`🎉` × 30 不能切出孤立代理）。**别为了省一次数组分配改回 `slice`。**
 */
export function normalizeAlias(raw: string): string {
  const collapsed = raw.replace(/\s+/g, " ").trim();
  return Array.from(collapsed).slice(0, DEVICE_ALIAS_MAX_LENGTH).join("");
}

/**
 * 设备在任何界面上的显示名。**这是唯一判据来源** —— 四个位置（当前电脑卡 / 设备面板每一行 /
 * 解绑确认框 / 会话视图 ⋯ 菜单的「当前电脑」）、共 6 个调用点都调它。
 *
 * 为什么不各处各写一遍 `alias || name || "未命名电脑"`：那样迟早会漏掉一处
 * （当前电脑卡读的是云端的 `instance.name`，它最容易漏），症状是"面板里改了备注、
 * 卡片上还是老名字"——用户会以为备注没生效，然后再改一遍。
 *
 * 参数收窄成 `Pick<…>` 而不是完整的 `MobileDevice`：卡片那边手上只有云端的 `instance`
 * + 本地记录的 `alias`，凑不出一个完整设备对象，不该为了调用它去伪造 `token` / `boundAt`。
 */
export function deviceDisplayName(device: Pick<MobileDevice, "alias" | "name">): string {
  return normalizeAlias(device.alias) || device.name.trim() || "未命名电脑";
}

function defaultStore(): DeviceStore | null {
  return typeof localStorage === "undefined" ? null : localStorage;
}

/** 兜底设备名：电脑端"当前绑定的手机"那一行显示的就是它。 */
export function deviceLabel(userAgent?: string): string {
  const ua = userAgent ?? (typeof navigator === "undefined" ? "" : navigator.userAgent);
  if (!ua) return "手机";
  // Android WebView 的 UA 里通常带着型号（`Android 14; SM-S9210 Build/...`），
  // 型号比"Android 手机"有用得多 —— 用户有不止一台手机时，这一行要能区分。
  const android = /Android[^;]*;\s*([^;)]+?)(?:\s+Build[/;]|\s*\))/i.exec(ua);
  const model = android?.[1]?.trim();
  if (model && /[a-z0-9]/i.test(model) && !/^wv$/i.test(model)) return model.slice(0, 40);
  if (/iPhone/i.test(ua)) return "iPhone";
  if (/iPad/i.test(ua)) return "iPad";
  if (/Android/i.test(ua)) return "Android 手机";
  if (/Windows|Macintosh|Linux/i.test(ua)) return "手机浏览器";
  return "手机";
}

/**
 * 上报给云端的**平台**标识。闭集，与云端 `sanitizePlatform` 一一对应：
 * 云端只认 android / ios / web，其它值一律塌成空串（不认识就不猜）。
 *
 * 为什么不直接把 `Capacitor.getPlatform()` 塞进请求体：这个模块是"能不 import 就不 import"
 * 的纯模块（`node --test` 直接跑），把 Capacitor 引进来会把整条测试链拖上原生桩。
 * 页面取值、这里收口，两边各自可测。
 */
export function devicePlatform(raw: string): string {
  const value = (raw || "").trim().toLowerCase();
  return value === "android" || value === "ios" || value === "web" ? value : "";
}

function normalizeDevice(raw: unknown): MobileDevice | null {
  if (!raw || typeof raw !== "object") return null;
  const value = raw as Record<string, unknown>;
  const token = typeof value.token === "string" ? value.token.trim() : "";
  if (!token) return null;
  return {
    token,
    instanceId: typeof value.instanceId === "string" ? value.instanceId : "",
    name: typeof value.name === "string" ? value.name : "",
    // 旧记录里**根本没有 `alias` 这个键**（这个字段是后加的）—— 与"起过备注但清空了"都归一化成
    // 空串。两者的区别只在"要不要保留清除按钮"，而那是弹层里读当前值就够的事，不值得多存一档。
    alias: typeof value.alias === "string" ? normalizeAlias(value.alias) : "",
    status: typeof value.status === "string" ? value.status : "",
    boundAt: typeof value.boundAt === "string" ? value.boundAt : "",
    lastSeenAt: typeof value.lastSeenAt === "string" ? value.lastSeenAt : "",
    revoked: value.revoked === true,
  };
}

export function readDevices(store: DeviceStore | null = defaultStore()): MobileDevice[] {
  if (!store) return [];
  try {
    const parsed = JSON.parse(store.getItem(DEVICES_STORAGE_KEY) || "[]");
    if (!Array.isArray(parsed)) return [];
    return parsed.map(normalizeDevice).filter((item): item is MobileDevice => item !== null);
  } catch {
    // 存储被写坏时宁可当"没有设备"（会走重新配对），也不要让整个页面崩在解析上。
    return [];
  }
}

export function writeDevices(devices: MobileDevice[], store: DeviceStore | null = defaultStore()): void {
  if (!store) return;
  store.setItem(DEVICES_STORAGE_KEY, JSON.stringify(devices));
}

export function readActiveToken(store: DeviceStore | null = defaultStore()): string {
  if (!store) return "";
  return store.getItem(ACTIVE_STORAGE_KEY) || "";
}

/** 设置当前设备。**同时镜像旧键**，让还没升级的旧包仍然认得这个令牌。 */
export function setActiveToken(token: string, store: DeviceStore | null = defaultStore()): void {
  if (!store) return;
  if (token) {
    store.setItem(ACTIVE_STORAGE_KEY, token);
    store.setItem(LEGACY_TOKEN_STORAGE_KEY, token);
  } else {
    store.removeItem(ACTIVE_STORAGE_KEY);
    store.removeItem(LEGACY_TOKEN_STORAGE_KEY);
  }
}

export function activeDevice(store: DeviceStore | null = defaultStore()): MobileDevice | undefined {
  const token = readActiveToken(store);
  const devices = readDevices(store);
  if (token) {
    const exact = devices.find((item) => item.token === token);
    if (exact) return exact;
  }
  // 当前键缺失（或指向一条已经不存在的记录）时退到第一台可用的，
  // 免得页面卡在"有设备但没选中"这种没法渲染的中间态。
  return devices.find((item) => !item.revoked) || devices[0];
}

/**
 * 读出设备列表，并跟旧键对齐：
 *   · 有旧令牌但列表为空 → 迁移成唯一一台设备（instanceId 等第一次请求回来再回填）；
 *   · 旧键指向的令牌不在列表里（旧包刚配了一台新的）→ 补一条记录并设为当前。
 * 两个方向都要管，否则新包/旧包来回切一次就会"丢"一台电脑。
 */
export function reconcileDevices(store: DeviceStore | null = defaultStore()): MobileDevice[] {
  if (!store) return [];
  const devices = readDevices(store);
  const legacy = (store.getItem(LEGACY_TOKEN_STORAGE_KEY) || "").trim();
  if (!legacy) {
    if (!readActiveToken(store) && devices.length > 0) {
      const usable = devices.find((item) => !item.revoked) || devices[0];
      setActiveToken(usable.token, store);
    }
    return devices;
  }
  const known = devices.find((item) => item.token === legacy);
  if (known) {
    if (readActiveToken(store) !== legacy) setActiveToken(legacy, store);
    return devices;
  }
  const migrated = readDevices(store);
  const adopted: MobileDevice = {
    token: legacy,
    instanceId: "",
    name: "",
    alias: "",
    status: "",
    boundAt: new Date().toISOString(),
    lastSeenAt: "",
    revoked: false,
  };
  const next = [...migrated.filter((item) => item.token !== legacy), adopted];
  writeDevices(next, store);
  setActiveToken(legacy, store);
  return next;
}

/**
 * 加一台（或覆盖同 instanceId 的旧记录）并设为当前。配对成功那一刻走这里。
 *
 * ⚠️ **备注必须跟着走**：同一台电脑重新配对会拿到新令牌，旧记录被这条 filter 掉 ——
 * 如果备注只留在旧记录上，用户重新配对一次名字就没了，而"重新配对"恰恰是他为了
 * 恢复连接最常做的操作（顶替、令牌过期都要重扫）。这是本功能最容易漏的一条路径，
 * 所以有专门的单测守着。
 */
export function addOrReplaceDevice(
  input: { token: string; instanceId: string; name?: string },
  store: DeviceStore | null = defaultStore(),
): MobileDevice[] {
  if (!store) return [];
  const now = new Date().toISOString();
  const previous = readDevices(store);
  // 继承来源：优先"同一台电脑"（instanceId 相同），其次"同一个令牌"（instanceId 还没回填时
  // 只能靠令牌认人）。两条都不是时＝真·新电脑，备注自然是空。
  const inherited = previous.find((item) => (input.instanceId && item.instanceId === input.instanceId) || item.token === input.token);
  const devices = previous.filter((item) => {
    if (item.token === input.token) return false;
    // 同一台电脑重新配对会拿到新令牌：旧记录必须让位，否则切换面板上会出现
    // 两台名字一样的"电脑"，其中一台怎么点都没反应。
    if (input.instanceId && item.instanceId === input.instanceId) return false;
    return true;
  });
  const device: MobileDevice = {
    token: input.token,
    instanceId: input.instanceId,
    name: input.name || "",
    alias: inherited ? normalizeAlias(inherited.alias) : "",
    status: "",
    boundAt: now,
    lastSeenAt: "",
    revoked: false,
  };
  const next = [...devices, device];
  writeDevices(next, store);
  setActiveToken(input.token, store);
  return next;
}

export function updateDevice(
  token: string,
  patch: Partial<Omit<MobileDevice, "token">>,
  store: DeviceStore | null = defaultStore(),
): MobileDevice[] {
  if (!store) return [];
  const next = readDevices(store).map((item) => (item.token === token ? { ...item, ...patch } : item));
  writeDevices(next, store);
  return next;
}

export function removeDevice(token: string, store: DeviceStore | null = defaultStore()): MobileDevice[] {
  if (!store) return [];
  const next = readDevices(store).filter((item) => item.token !== token);
  writeDevices(next, store);
  if (readActiveToken(store) === token) setActiveToken(next.find((item) => !item.revoked)?.token || "", store);
  return next;
}

/**
 * 给某台电脑起/改/清备注名。**纯本地写**，不发任何请求、也不动 `name`。
 *
 * 传空（或全空格）＝清除备注、回落到电脑真名；这里用 `normalizeAlias` 收口，
 * 免得调用方各自判断"什么算空"。
 */
export function setDeviceAlias(
  token: string,
  alias: string,
  store: DeviceStore | null = defaultStore(),
): MobileDevice[] {
  if (!store) return [];
  return updateDevice(token, { alias: normalizeAlias(alias) }, store);
}

/**
 * 令牌被云端拒绝：**留着记录、打上标记**，不要静默删掉。
 * 被另一台手机顶替时这是唯一的现场 —— 界面靠它说"这台电脑已被另一台手机接管，
 * 需要重新扫码"，而不是让配对记录凭空消失。
 */
export function markDeviceRevoked(token: string, store: DeviceStore | null = defaultStore()): MobileDevice[] {
  if (!store) return [];
  return updateDevice(token, { revoked: true, status: "revoked" }, store);
}
