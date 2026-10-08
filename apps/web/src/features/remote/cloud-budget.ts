/**
 * 手机端经云端发请求时，**本方**该等多久。
 *
 * ── 为什么必须是一条独立的不变量 ──────────────────────────────────────────
 *
 * 手机 → 云端 → 电脑 →（答复原路返回）这条链上有**两个计时器**：手机端自己的
 * AbortController，和云端等电脑回话的那个上限。它们必须满足
 *
 *     手机端等待上限  >  云端等待上限
 *
 * 否则手机先报"超时"、云端还在等一个早就丢掉的响应 —— 用户看到失败，而那次 push、
 * 那个文件写入其实还在电脑上跑。这条不变量在云端侧是明文写着的
 * （`apps/cloud-control/internal/cloud/rpc.go` 的 `rpcDefaultTimeout`：
 * "它必须大于手机端自己的请求超时"）。
 *
 * 2026-09 之前这里两边都是 15 秒：云端 **默认**就等 20 秒，而手机 15 秒就放弃 ——
 * 不变量从一开始就是破的，且破在"文件适配器从不提议超时"这条最常见的路径上。
 *
 * ── 两个数从哪来 ────────────────────────────────────────────────────────
 *
 * 都抄自 `rpc.go`，**那边改了这边要跟着看**（三端预算的惯例：同一根预算的三端表达）：
 *   - `rpcDefaultTimeout = 20s`：手机端没提议时云端的等待上限；
 *   - `rpcMaxTimeout = 120s`：手机端提议值被云端夹取的上界（提议再大也会被夹到这里）。
 *
 * 余量给 5 秒：云端收到 → 转发给电脑 → 拿到答复 → 写回来，这几步都落在两个计时器之外。
 */

/** 云端在手机端未提议时的等待上限（`rpc.go` 的 `rpcDefaultTimeout`）。 */
export const CLOUD_DEFAULT_WAIT_MS = 20_000;

/** 手机端提议值被云端夹取的上界（`rpc.go` 的 `rpcMaxTimeout`）。 */
export const CLOUD_MAX_WAIT_MS = 120_000;

/** 答复在链路上往返的余量。 */
export const CLOUD_RESPONSE_MARGIN_MS = 5_000;

/**
 * 手机端自己的等待上限。
 *
 * `proposedWaitMs` 是调用方**请云端等**的那个数（RPC 通道的 `timeoutMs`：客户端提议、
 * 云端夹取 —— 见 `features/remote/mobile-rpc.ts`）。只让云端多等没有意义：手机端的
 * 计时器必须跟着一起放长，否则提议本身就被自己的默认值抵消掉了 —— 这正是
 * `mobile-git-request.ts` 按 op 分档（读 20s / 本地写 45s / 网络写 60s）之后仍然
 * "push 必然超时"的原因。
 */
export function cloudClientWaitMs(proposedWaitMs?: number): number {
  const floor = CLOUD_DEFAULT_WAIT_MS + CLOUD_RESPONSE_MARGIN_MS;
  if (!proposedWaitMs || proposedWaitMs <= 0) return floor;
  const clamped = Math.min(proposedWaitMs, CLOUD_MAX_WAIT_MS);
  return Math.max(floor, clamped + CLOUD_RESPONSE_MARGIN_MS);
}
