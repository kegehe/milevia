// 桌面端会话页「运行指示器」的判据（纯函数，无 React）。
//
// 三份职责收口在这里，页面只负责调用 + 渲染：
//   1. 动画变体抽取 —— 用户定稿了 7 个动画（波点 / 轨道 / 思绪波纹 / 星芒 /
//      追点 / 三点走三角 / CLI 盲文），每次 run 随机显示一个。抽取以 runId
//      做确定性哈希：同一 run 重渲染、WebSocket 重连都不会"换花样"，
//      不同 run 之间分布均匀 —— 这就是"每次随机选一个"的可测试实现。
//   2. 阶段词 —— 复用桌面时间线的 systemItemFromEvent（手机端也用同一份），
//      不在这里造第二套文案；只保留"进行中"的阶段，结果类标题不当阶段词。
//   3. 计时起点 —— 总耗时从本轮最早的消息、事件或用量 startedAt 起算，全程不归零。
//      有进行中的阶段时再给出该阶段的起点（阶段一变归零）。两颗读数同时返回，
//      时间戳读不到的那颗是 null，不用「现在」编起点。
import { systemItemFromEvent } from "./timeline";
import type { Event, SystemItem } from "./types";

export type RunIndicatorVariant = "dots" | "orbit" | "ripple" | "spark" | "chase" | "tri" | "cli";

export const runIndicatorVariants: RunIndicatorVariant[] = [
  "dots", "orbit", "ripple", "spark", "chase", "tri", "cli",
];

// 文字位变体：阶段词（.ri-stage）自己的动效，与图形位变体相互独立。
// K 高亮扫过 / L 填色擦除 / M 弹跳字符（需拆字）/ N 双色脉冲 / O 液态填充。
export type RunTextVariant = "shimmerPro" | "wipe" | "bounce" | "pulse" | "liquid";

export const runTextVariants: RunTextVariant[] = [
  "shimmerPro", "wipe", "bounce", "pulse", "liquid",
];

// FNV-1a 哈希：把种子串稳定映射到一个数。
// 不用 Math.random()：那会随每次重渲染重抽，动画在 run 中途换花样；
// 也不用 state 存抽取结果：那样 WebSocket 重连、页面重开都会丢。
function fnv1a(input: string): number {
  let hash = 0x811c9dc5;
  for (let index = 0; index < input.length; index += 1) {
    hash ^= input.charCodeAt(index);
    hash = Math.imul(hash, 0x01000193);
  }
  return Math.abs(hash);
}

// 图形位：runId 直接做种子。
export function pickRunIndicatorVariant(runId: string): RunIndicatorVariant {
  return runIndicatorVariants[fnv1a(runId) % runIndicatorVariants.length];
}

// 文字位：种子加 ":text" 后缀 —— 与图形位抽取独立（同一 run 的图形/文字不绑死），
// 但同一 run 内仍然稳定：重渲染、重连都不换花样。
export function pickRunTextVariant(runId: string): RunTextVariant {
  return runTextVariants[fnv1a(`${runId}:text`) % runTextVariants.length];
}

// 弹跳变体（rt-bounce）需要把阶段词拆成单字 span 逐字起舞。
// 空格换成不换行空格：inline-block 的 span 里普通空格会塌掉，词间距就丢了。
export function splitLabelChars(label: string): string[] {
  return Array.from(label).map((ch) => (ch === " " ? "\u00A0" : ch));
}

// 当前 run 有没有「正在进行的阶段」？有就返回那个阶段事件，没有返回 null。
//
// 判定是"最新信号获胜"，不是"最近一条进行中的事件获胜"：
//   · 先取当前 runId 下 createdAt 最新的一条阶段事件；
//   · 它是进行中类（compact / api_retry / 非终态 task）→ 就是当前阶段；
//   · 它是结果类（compact_result 压缩完成/失败、compact_boundary、带终态
//     metadata.state 的 task）→ 说明最新的那件事**已经结束**，当前没有
//     进行中的阶段 → 返回 null，让页面回退到「XX 正在处理任务」。
//     ⚠️ 不能因为"更早还有一条 compact"就把它捞出来当阶段词 —— 那个阶段
//     已经被它自己的结果事件关闭了，捞出来就是「压缩完成后还挂着
//     正在压缩上下文 · 45s」的假话。
//     （反向场景同理：重试事件比压缩事件新 → 当前就是在重试。）
// 同 runId 过滤：同一会话的历史里堆着之前每一轮 run 的阶段事件，不过滤的话
// 新一轮 run 会把上一轮的阶段当成自己的（同会话跨 run 串数据，与"读不到写成
// 没有"同级的坏）。按 createdAt 取最新（服务端合并通知时顺序不保证与时间一致）。
export function latestActiveStage(events: Event[], runId: string): SystemItem | null {
  let best: SystemItem | null = null;
  let bestAt = -Infinity;
  // 这一轮最后一条事件的时间（不分类别）：判断"CLI 是否已经开口"用。和找 best 合并成
  // 一趟 —— 这个函数每秒都会被调用一次，分开再扫一遍就是白花一倍。
  let latestEventAt = -Infinity;
  for (let index = 0; index < events.length; index += 1) {
    const event = events[index];
    if (event.runId === runId) {
      const at = Date.parse(event.createdAt) || -Infinity;
      if (at > latestEventAt) latestEventAt = at;
    }
    const item = systemItemFromEvent(event);
    if (!item || item.runId !== runId) continue;
    const at = Date.parse(item.createdAt) || -Infinity;
    if (at >= bestAt) {
      best = item;
      bestAt = at;
    }
  }
  if (!best) return null;
  if (best.variant === "compact_result" || best.variant === "compact_boundary") return null;
  // "正在恢复会话上下文" 只覆盖 CLI 还没开口的那一段：只要这一轮之后又来了任何事件
  // （CLI 的 init、assistant、工具输出），进程就已经起来了，阶段必须结束。少了这一步，
  // 它会一直挂到本轮结束 —— 一边在流式输出、状态条还写着"正在恢复"。
  if (best.variant === "session" && latestEventAt > bestAt) return null;
  const state = best.metadata?.state;
  if (state === "success" || state === "failed" || state === "info") return null;
  return best;
}

// 指示器的两颗计时起点。
//
// totalStartMs：本轮任务耗时。取当前 runId 下最早的消息、事件，以及用量接口的
// startedAt。上一轮的时间再早也不算。全程不因阶段切换而改起点。
// stageStartMs：进行中阶段的耗时，从该阶段 createdAt 起算，阶段一变就换起点。
// 没有进行中的阶段，或阶段时间读不到时，这一颗是 null，总耗时照常返回。
// 两颗都读不到时都是 null。不用调用时刻冒充起点，否则刷新页面会归零。
export function indicatorClockStart(input: {
  runId: string;
  stage: { createdAt: string } | null;
  messages: ReadonlyArray<{ runId?: string; createdAt: string }>;
  events: ReadonlyArray<{ runId: string; createdAt: string }>;
  startedAt?: string | null;
}): { stageStartMs: number | null; totalStartMs: number | null } {
  let totalStartMs = Infinity;
  const startedAt = input.startedAt ? Date.parse(input.startedAt) : Number.NaN;
  if (Number.isFinite(startedAt)) totalStartMs = startedAt;
  for (let index = 0; index < input.messages.length; index += 1) {
    const message = input.messages[index];
    if (message.runId !== input.runId) continue;
    const at = Date.parse(message.createdAt);
    if (Number.isFinite(at) && at < totalStartMs) totalStartMs = at;
  }
  for (let index = 0; index < input.events.length; index += 1) {
    const event = input.events[index];
    if (event.runId !== input.runId) continue;
    const at = Date.parse(event.createdAt);
    if (Number.isFinite(at) && at < totalStartMs) totalStartMs = at;
  }
  const stageAt = input.stage ? Date.parse(input.stage.createdAt) : Number.NaN;
  return {
    stageStartMs: Number.isFinite(stageAt) ? stageAt : null,
    totalStartMs: totalStartMs === Infinity ? null : totalStartMs,
  };
}

// 函数默认门槛仍是 10 秒。桌面运行指示器和移动端都显式传 0，任务一开始就显示。
// 不要把这个默认值当成桌面端的界面行为。
export const stageClockVisibleAfterMs = 10_000;

// 把毫秒差格式化成 CLI 风格的读数：8s / 59s / 1m05s / 1h01m05s。
// minMs 是显示门槛（毫秒）：低于它返回空串。不传时用上面的 10 秒默认值；
// 桌面指示器与移动端传 0，立即显示。
// 负值（时钟偏差）与 NaN 一律返回空串 —— 显示「-3s」「NaNs」是纯粹的坏；
// NaN 与任何数比较恒为 false，必须单独拦，不能靠门槛那半句兜住。
export function formatStageClock(elapsedMs: number, minMs: number = stageClockVisibleAfterMs): string {
  if (!Number.isFinite(elapsedMs)) return "";
  if (elapsedMs < minMs) return "";
  const totalSeconds = Math.floor(elapsedMs / 1000);
  if (totalSeconds < 60) return `${totalSeconds}s`;
  const seconds = totalSeconds % 60;
  const totalMinutes = Math.floor(totalSeconds / 60);
  if (totalMinutes < 60) return `${totalMinutes}m${seconds < 10 ? "0" : ""}${seconds}s`;
  const hours = Math.floor(totalMinutes / 60);
  const minutes = totalMinutes % 60;
  return `${hours}h${minutes < 10 ? "0" : ""}${minutes}m${seconds < 10 ? "0" : ""}${seconds}s`;
}
