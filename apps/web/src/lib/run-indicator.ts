// 桌面端会话页「运行指示器」的判据（纯函数，无 React）。
//
// 三份职责收口在这里，页面只负责调用 + 渲染：
//   1. 动画变体抽取 —— 用户定稿了 7 个动画（波点 / 轨道 / 思绪波纹 / 星芒 /
//      追点 / 三点走三角 / CLI 盲文），每次 run 随机显示一个。抽取以 runId
//      做确定性哈希：同一 run 重渲染、WebSocket 重连都不会"换花样"，
//      不同 run 之间分布均匀 —— 这就是"每次随机选一个"的可测试实现。
//   2. 阶段词 —— 复用桌面时间线的 systemItemFromEvent（手机端也用同一份），
//      不在这里造第二套文案；只保留"进行中"的阶段，结果类标题不当阶段词。
//   3. 阶段计时 —— CLI 口径：从本阶段事件起算，阶段一变就归零；
//      10 秒内不显示（沿用手机端 processingClockVisibleAfterMs 的阈值理由：
//      秒级任务上闪一个「0:03」只增加噪音）。
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
  for (let index = 0; index < events.length; index += 1) {
    const item = systemItemFromEvent(events[index]);
    if (!item || item.runId !== runId) continue;
    const at = Date.parse(item.createdAt) || -Infinity;
    if (at >= bestAt) {
      best = item;
      bestAt = at;
    }
  }
  if (!best) return null;
  if (best.variant === "compact_result" || best.variant === "compact_boundary") return null;
  const state = best.metadata?.state;
  if (state === "success" || state === "failed" || state === "info") return null;
  return best;
}

// 计时显示阈值：默认 10 秒内不显示。与手机端 processingClockVisibleAfterMs 同口径同理由。
// 移动端传 minMs = 0 立即显示（用户定稿的「正在压缩上下文 · 8s」形态）。
export const stageClockVisibleAfterMs = 10_000;

// 把毫秒差格式化成 CLI 风格的读数：8s / 59s / 1m05s。
// minMs 是显示门槛（毫秒）：低于它返回空串。桌面默认 10 秒（秒级任务上闪读数只加噪音）；
// 移动端传 0 立即显示。
// 负值（时钟偏差）与 NaN 一律返回空串 —— 显示「-3s」「NaNs」是纯粹的坏；
// NaN 与任何数比较恒为 false，必须单独拦，不能靠门槛那半句兜住。
export function formatStageClock(elapsedMs: number, minMs: number = stageClockVisibleAfterMs): string {
  if (!Number.isFinite(elapsedMs)) return "";
  if (elapsedMs < minMs) return "";
  const totalSeconds = Math.floor(elapsedMs / 1000);
  if (totalSeconds < 60) return `${totalSeconds}s`;
  const minutes = Math.floor(totalSeconds / 60);
  const seconds = totalSeconds % 60;
  return `${minutes}m${seconds < 10 ? "0" : ""}${seconds}s`;
}
