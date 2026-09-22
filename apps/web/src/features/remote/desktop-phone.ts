// 电脑端「已绑定手机」这一块的**纯逻辑**。
//
// 为什么单独一个模块（和 `desktop-service.ts` 同一个理由）：
//   「这台手机现在还在不在用」是一段**优先级级联**（读到没读到 → 云端给不给这个字段 →
//   有没有同步过 → 新不新鲜），而"哪一档赢"用扫源码正则验不出来 —— 把阈值改成 0、
//   或者把 `undefined` 和 `null` 合并成一个分支，四句文案全都还在，源码级断言照样绿。
//   抽成纯函数之后，同一条变异当场被行为断言挡住。
//
// 类名注意：这里不 import React，也不碰 DOM —— 才可以在 `node --test` / `tsx --test` 下直接跑。

/**
 * 「手机在线」的新鲜度阈值。**这个数是推出来的，不是拍的**，改它之前先把下面这行算式重算一遍：
 *
 *   手机端「当前设备」每 5 秒轮询一次 `/v1/instances`（见 docs/38 §5.4）；
 *   云端在 `userAuth` 里按 30 秒窗口节流写 `last_used_at`，条件是
 *   `last_used_at < now() - 30 seconds` —— 注意这个条件只在**下一次轮询**时才被求值，
 *   所以两次写入的真实间隔不是 30 秒，而是 **30（窗口）+ 5（轮询）= 35 秒**：
 *     t0 写入 → t0+5…t0+30 六次轮询全部不满足 → t0+35 才写第二笔
 *   ⇒ 手机**正在以 5 秒节奏轮询**时，`now - lastUsedAt` 的最坏值是 35 秒上下。
 *
 * 阈值必须比它宽出一个整周期的余量：手机在后台被系统限流、某次请求慢几秒、
 * 一次 GC 停顿，都会让实际节奏变成 10 秒甚至更稀 —— 而那些都是**正常的**。
 * 阈值贴着 35 秒写，一次抖动就会让页面把"正在用的手机"说成"未同步"，
 * 而"谎报离线"比"晚报离线"坏得多（用户会开始不信这个读数，然后它就没用了）。
 * 取 **90 秒 ≈ 2.6 倍**最坏陈旧度：既留足一整个周期的余量，
 * 手机真的关掉之后最多 1 分半页面就会说出来。
 *
 * ⚠️ **那个 30 秒节流窗口在云端**（`apps/cloud-control/internal/cloud/server.go` 的 `userAuth`，
 * 条件写在 SQL 的 `where` 里）。改窗口必须回来重算这个阈值 —— 两边注释互相指认，
 * 因为这条耦合跨了语言和仓库目录，没有编译器帮忙：窗口调到 120 秒而这里不动，
 * 手机明明在用也会被判成"未同步"。
 */
export const PHONE_SYNC_FRESH_MS = 90_000;

/**
 * 云端 `GET /v1/agent/bindings` 里的一项。
 *
 * ⚠️ `lastUsedAt` 有**三种**状态，绝不能压成两种：
 *   · 键不存在（`undefined`）→ 本机连的云端**版本较旧**，它压根不提供这项读数
 *   · `null`（或空串）      → 云端知道这件事，但这台手机**绑定后一次都没同步过**
 *   · 时间串                → 有读数，按年龄分新鲜/陈旧
 * 把前两者合并，就等于把"还没回来"写成"没有数据" —— 这个坑这个项目踩过三次了。
 */
export type DesktopBinding = {
  deviceName: string;
  activatedAt: string;
  lastUsedAt?: string | null;
  /** android / ios / web；空串＝手机没上报（旧包），界面整行不渲染。 */
  platform?: string;
};

export type PhoneSyncState = "unsupported" | "never" | "online" | "idle";

export type PhoneSyncView = {
  state: PhoneSyncState;
  /** 页头那颗胶囊（地方宽裕，可以说完整）；空串＝这一档不给页头胶囊。 */
  headerChip: string;
  /** 手机名右边那颗小胶囊，一个词。 */
  chip: string;
  /** 「最近同步 …」那一行的值。 */
  agoText: string;
  /** 需要解释时才给；空串表示没什么可说的。 */
  hint: string;
};

/**
 * 后端返回的一串原始项 → 干净的绑定列表。数组里出现非对象项时整项丢掉，
 * 而不是渲染成一行空白（"未知手机 · 绑定时间未知" 这种假信息比不显示更糟）。
 */
export function normalizeBindings(raw: unknown): DesktopBinding[] {
  if (!Array.isArray(raw)) return [];
  const items: DesktopBinding[] = [];
  for (const entry of raw) {
    if (!entry || typeof entry !== "object") continue;
    const value = entry as Record<string, unknown>;
    const binding: DesktopBinding = {
      deviceName: typeof value.deviceName === "string" ? value.deviceName : "",
      activatedAt: typeof value.activatedAt === "string" ? value.activatedAt : "",
    };
    // 只有**键真的在、且值是我们认识的类型**时才写进对象：
    //   · 键不在                      → 保持 undefined，走 unsupported
    //   · 值为 null                   → 显式写 null，走 never
    //   · 值是别的东西（数字时间戳？）→ **不许**当成时间、也不许当成"没同步过"，
    //     同样保持 undefined。一句"这台手机没在同步"是我们**下不了的结论**，
    //     说出口就是把读不到栽赃给手机。
    // 另外：给对象显式赋 undefined 会让 `"lastUsedAt" in item` 变成 true，所以这里
    // 只在认识的情况下赋值，不做 `binding.lastUsedAt = undefined` 这种等价写法。
    if ("lastUsedAt" in value) {
      if (typeof value.lastUsedAt === "string") binding.lastUsedAt = value.lastUsedAt;
      else if (value.lastUsedAt === null) binding.lastUsedAt = null;
    }
    if (typeof value.platform === "string") binding.platform = value.platform;
    items.push(binding);
  }
  return items;
}

/**
 * 年龄 → 文案。和 `heartbeatAgoText` 同一套刻度，但**空值那一档措辞不同**：
 * 心跳是"还没听到过"（可能刚启动），手机是"还没同步过"（绑定后一次请求都没发）。
 * 大单位一律 floor：round 会让 3599 秒先被舍成 60 分钟、直接跳进"1 小时前"，
 * 于是"60 分钟前"这一档永远不可达。
 */
export function phoneSyncAgoText(ageMs: number | null): string {
  if (ageMs === null || !Number.isFinite(ageMs)) return "还没有同步过";
  const seconds = Math.max(0, Math.round(ageMs / 1000));
  if (seconds < 5) return "刚刚";
  if (seconds < 60) return `${seconds} 秒前`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes} 分钟前`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours} 小时前`;
  return `${Math.floor(hours / 24)} 天前`;
}

/**
 * 四档，判据顺序就是优先级：
 *   读到没读到（unsupported）→ 有没有同步过（never）→ 新不新鲜（online / idle）
 *
 * ⚠️ 判据必须先问「云端给了吗」再问「新鲜吗」：`lastUsedAt` 是 `undefined` 时，
 * `Date.parse(undefined)` 是 NaN，任何"NaN 就当陈旧"的写法都会让**旧云端**被说成
 * 「这台手机没在同步」—— 那是把"宿主没给读数"栽赃给手机。unsupported 是独立一档。
 *
 * ⚠️ 同理，「有值但解析不出来」也归 unsupported，**不是** never：
 *   · `null` / `""` → 云端明确说"这台手机绑定后一次都没同步过" → never
 *   · `"not-a-time"` → 值是给到了，但**我们读不懂** → unsupported（不下结论）
 * 两者都在"没拿到可用年龄"这一列，但归因完全不同：一个是手机的事实，一个是我们的失败。
 */
export function phoneSyncState(binding: DesktopBinding | null, ageMs: number | null): PhoneSyncState {
  if (!binding) return "unsupported";
  if (binding.lastUsedAt === undefined) return "unsupported";
  if (!binding.lastUsedAt) return "never";
  if (ageMs === null || !Number.isFinite(ageMs)) return "unsupported";
  // 负数（时钟往回跳）算新鲜：那说明时刻落在将来，不可能是"手机没在同步"。
  return ageMs < PHONE_SYNC_FRESH_MS ? "online" : "idle";
}

export function phoneSyncView(binding: DesktopBinding | null, ageMs: number | null): PhoneSyncView {
  const state = phoneSyncState(binding, ageMs);
  const agoText = phoneSyncAgoText(ageMs);
  switch (state) {
    case "unsupported":
      return {
        state,
        headerChip: "已绑定手机",
        chip: "",
        agoText: "",
        // 这一档**不是**手机的错，所以只陈述事实、不给动作（用户此刻能做的只有升级云端，
        // 而那件事在电脑上做不了）。措辞里不许出现"未同步"三个字。
        // 两种成因（旧云端不给 / 值读不懂）共用一句：都是"我们这边读不到"。
        hint: "读不到「最近同步」这项读数，只能确认绑定关系。若本机连的云端版本较旧，更新云端后这一项会自己出现。",
      };
    case "never":
      return {
        state,
        headerChip: "手机未同步",
        chip: "未同步",
        agoText: "还没有同步过",
        hint: "这台手机绑定之后还没有向云端发过请求。它可能只是还没打开，也可能连不上云端。",
      };
    case "online":
      return { state, headerChip: "手机在线", chip: "在线", agoText, hint: "" };
    case "idle":
      return {
        state,
        headerChip: "手机未同步",
        chip: "未同步",
        agoText,
        // 措辞不许把原因说死：手机端切到后台会被系统限流，App 没打开和网络不通
        // 在云端看起来**一模一样**。只报观测到的事实，把两种可能都摆出来。
        hint: `手机最近一次同步是${agoText}。它可能只是切到了后台或没打开，也可能连不上云端；两种情况下电脑端收到的都是同一份旧数据。`,
      };
  }
}

/** 平台 → 中文。空串表示手机没上报（旧包），调用方据此整条不渲染。 */
export function platformLabel(platform: string | undefined): string {
  switch ((platform || "").trim().toLowerCase()) {
    case "android":
      return "Android";
    case "ios":
      return "iOS";
    case "web":
      return "浏览器";
    default:
      return "";
  }
}

/**
 * 「最近同步」时刻 → 年龄（毫秒）。读不出来（没绑定 / 缺键 / 值不是合法时间）一律 null。
 *
 * 两个调用点必须**共用这一个函数**：① 数据落地的那一刻就地量一次（避免定时器要等
 * 下一帧才跑，中间那一帧年龄是 null ⇒ 先闪一下"未同步"）；② 每 5 秒的定时器。
 * 两处各写一遍 `Date.parse`，将来只要有一处漏了 `Number.isFinite` 检查，
 * 就会变成 NaN 被当成"有年龄"或 null 被当成"没同步过"，而那种抖动只在真机上看得见。
 */
export function syncAgeFrom(lastUsedAt: string | null | undefined): number | null {
  const at = Date.parse(lastUsedAt || "");
  return Number.isFinite(at) ? Date.now() - at : null;
}
