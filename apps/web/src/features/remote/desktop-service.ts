// 电脑端「远程控制」页的**纯逻辑**：五个服务档位的判定、心跳年龄的措辞、实例 ID 的缩写。
//
// 为什么单独一个模块（而不是留在页面组件里）：
//   ① 这一段是**优先级级联**（loading → failed → unregistered → live → stale），
//      而"哪一档赢"用扫源码正则验不出来 —— 把 `agentAlive` 换成 `true`，五句文案全都还在，
//      源码级断言照样绿，只有真浏览器探针能看见（2026-09-17 变异检验实测漏网）。
//      抽成纯函数之后，同一条变异当场被行为断言挡住。
//   ② 页面组件 3800+ 行，任何"能不能只靠这几个字段算出来"的判断都不该埋在 JSX 里。
//
// 类名注意：这里不 import React，也不碰 DOM —— 才可以在 `node --test` / `tsx --test` 下直接跑。

/** Agent 的心跳周期是 750ms（见 agent.runConnection 那条 ticker），这里取 20 个周期。
 *  放宽到 15 秒是为了容忍一次卡顿 / 一次 GC，而不是为了"看起来别那么快变红"：
 *  真停了之后最多 15 秒页面就会说出来，比永远显示"运行中"强得多。 */
export const AGENT_HEARTBEAT_FRESH_MS = 15_000;

/** `/api/remote/agent-status` 的读取状态。"还没回来"和"读不到"必须分开：前者是加载态，后者要能重试。 */
export type DesktopAgentView = "loading" | "loaded" | "failed";

export type DesktopAgentStatus = {
  /** 只说明**凭据在不在** —— Agent 进程崩了它依然是 true。 */
  ready: boolean;
  instanceId: string;
  cloudUrl: string;
  /** 最近一次听到 Agent 心跳的时刻（RFC3339）。空串＝本次进程启动后一次都没听到过。 */
  heartbeatAt: string;
};

export type DesktopServiceState = "loading" | "failed" | "unregistered" | "live" | "stale";

export type DesktopServiceView = {
  state: DesktopServiceState;
  /** 页头那颗胶囊用整句（地方宽裕）。 */
  label: string;
  /** 侧栏卡片头上只给一个词（旁边已经有「远程服务」这个标题了）。 */
  chip: string;
  /** 需要用户动手时才给；空串表示没什么可说的。 */
  hint: string;
};

/**
 * 心跳还算不算"活着"。判据是 `0 <= 年龄 < 15s` 的宽松版：只要不是 null / NaN 且小于阈值就算。
 * 负数（时钟往回跳、或控制服务与本机时钟不同步）**算新鲜** —— 那说明心跳时刻落在将来，
 * 不可能是"进程死了"；`heartbeatAgoText` 会把负数夹到 0 显示"刚刚"。
 * ⚠️ `null < 阈值` 在 JS 里是 `true`（null 转成 0），所以必须显式排除 null ——
 * 漏掉它会让"本次启动后从没听到过心跳"被判成"运行中"。
 */
export function isAgentAlive(heartbeatAgeMs: number | null): boolean {
  return heartbeatAgeMs !== null && Number.isFinite(heartbeatAgeMs) && heartbeatAgeMs < AGENT_HEARTBEAT_FRESH_MS;
}

/**
 * 五个档位，判据顺序就是优先级：
 *   读取中 → 读不到 → 未注册 → 运行中 → 无心跳
 *
 * `ready` 只回答"凭据在不在"，所以它之后还必须问一次心跳 —— 这正是旧版缺的那一档：
 * 凭据还在而页面写着"已就绪"，用户对着一个什么都不转的手机端找原因。
 */
export function desktopServiceState(
  view: DesktopAgentView,
  status: DesktopAgentStatus | null,
  heartbeatAgeMs: number | null,
): DesktopServiceState {
  if (view === "loading") return "loading";
  if (view === "failed") return "failed";
  if (!status?.ready) return "unregistered";
  return isAgentAlive(heartbeatAgeMs) ? "live" : "stale";
}

const DESKTOP_SERVICE_TEXT: Record<DesktopServiceState, Omit<DesktopServiceView, "state">> = {
  loading: { label: "正在读取服务状态…", chip: "读取中", hint: "" },
  failed: { label: "读不到本机服务状态", chip: "读不到", hint: "本机控制服务没有响应，重启 Milevia 后再试。" },
  unregistered: { label: "远程服务未注册", chip: "未注册", hint: "先在左边完成一次注册，手机才能配对。" },
  live: { label: "远程服务运行中", chip: "运行中", hint: "" },
  // 凭据还在（所以不是"未注册"），但心跳停了。
  // ⚠️ **措辞不能把原因说死**：那条 750ms 的 ticker 长在 `agent.runConnection` 里，
  // 只在"与云端建立连接"期间才跑 —— 所以心跳停既可能是 Agent 没在运行，也可能只是
  // 它现在连不上云端。两者的修法不同（重启进程 vs 查网络），页面只报**观测到的事实**
  // （收不到心跳），把两种可能都摆出来，并给一句同时成立的动作。手机端看到的都是旧数据。
  stale: {
    label: "远程服务没有心跳",
    chip: "无心跳",
    hint: "凭据还在，但已经收不到本机 Agent 的心跳 —— 可能是它没在运行，也可能只是暂时连不上云端；两种情况下手机端都只能看到旧数据。确认 Milevia 正在运行、且本机能连上云端。",
  },
};

export function desktopServiceView(
  view: DesktopAgentView,
  status: DesktopAgentStatus | null,
  heartbeatAgeMs: number | null,
): DesktopServiceView {
  const state = desktopServiceState(view, status, heartbeatAgeMs);
  return { state, ...DESKTOP_SERVICE_TEXT[state] };
}

/**
 * 「最近心跳 8 秒前」。空值有**两种**来源，措辞必须分开：
 *   · 本次进程启动后一次都没听到过 → "尚未收到"（可能刚启动，也可能是从没连上）
 *   · 听到过但已经很久 → "N 分钟前"
 * 把 0 当成 `time.Unix(0,0)` 会让页面写"1970 年"，那比不显示更糟（服务端因此回空串）。
 */
export function heartbeatAgoText(ageMs: number | null): string {
  if (ageMs === null || !Number.isFinite(ageMs)) return "尚未收到";
  const seconds = Math.max(0, Math.round(ageMs / 1000));
  if (seconds < 5) return "刚刚";
  if (seconds < 60) return `${seconds} 秒前`;
  // 大单位一律用 floor：用 round 的话 3599 秒会先被舍成 60 分钟、再跳进"1 小时前"，
  // 于是"60 分钟前"这一档永远不可达，而 59 分 59 秒会被显示成 1 小时（实测过）。
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes} 分钟前`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours} 小时前`;
  return `${Math.floor(hours / 24)} 天前`;
}

/** 实例 ID 在界面上只用来"对暗号"，不需要全文；中间省略比截尾更容易认出是同一个串。 */
export function shortInstanceID(value: string): string {
  const trimmed = value.trim();
  if (trimmed.length <= 12) return trimmed;
  return `${trimmed.slice(0, 6)}…${trimmed.slice(-4)}`;
}

/**
 * 电脑端「刷新状态」的结果文案。
 *
 * ⚠️ **电脑端的刷新不能复用手机那条链路**：手机那条会去拉 `/v1/instances` 与云端快照，
 * 而电脑端没有云端令牌 ⇒ 必然走到 `尚未配对电脑，请先扫码配对`。电脑端**自己就是被配对的那台机器**，
 * 照着这句话做只会更糊涂（2026-09-17 审计的 A3，实测就是这个文案）。
 * 三态分开：读到没读到 / 没注册 / 读到了。
 */
export function refreshDesktopStatusMessage(outcome: "ready" | "unregistered" | "failed"): { state: "success" | "failed"; message: string } {
  if (outcome === "ready") return { state: "success", message: "已更新服务状态与绑定信息" };
  if (outcome === "unregistered") return { state: "failed", message: "远程服务尚未注册，请先完成注册" };
  return { state: "failed", message: "读不到本机服务状态，请确认 Milevia 正在运行" };
}
