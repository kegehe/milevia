// 会话回退节点（checkpoint）的客户端判据 —— 桌面端与手机端**共用一份**。
//
// 服务端按节点时间过滤了它返回的消息与事件，但两端的列表都不是"服务端返回什么就显示什么"：
//   - 桌面端 reload 会**合并**本地已有的消息（流式输出还没来得及落库时不能丢）；
//   - 手机端的消息是"历史分页缓存 ∪ 快照"，历史页一旦读过就在本地留了一份。
// 于是只过滤服务端那一份挡不住已经被本地缓存下来的回合：用户回退了，屏幕上却还挂着被丢掉的
// 对话（另一台设备发起的回退尤其明显 —— 本机根本没参与那次操作）。
//
// 所以服务端把节点时间（messagesCutoff）也发给客户端，客户端据此把本地那份里比节点更新的
// 内容丢掉。判据与服务端**逐字对齐**：服务端是 `created_at <= cutoff` 保留，这里就是
// "严格晚于 cutoff 的丢掉"，不能写成 >=（那会连节点那一轮的消息一起丢掉）。

/** 带 createdAt 的最小形状：消息、事件、通知都满足。 */
type CutoffFilterable = { createdAt: string };

/** 把时间戳解析成毫秒；解析不出来时返回 null（这种条目一律保留，不拿坏数据去猜）。 */
function timestamp(value: string | undefined | null): number | null {
  if (!value) return null;
  const parsed = Date.parse(value);
  return Number.isNaN(parsed) ? null : parsed;
}

/**
 * 丢掉"被回退掉"的内容：比节点更新、但又发生在**最近一次回退之前**的那些。
 *
 * 为什么还要 `visibleAfter`：回退之后新发生的一轮（哪怕失败/被停止）不属于被丢掉的历史，
 * 必须照常显示 —— 失败的回合不生成新节点，只看 cutoff 会把用户刚发的那条消息连同失败
 * 卡片一起藏掉。判据与服务端的 checkpointVisibilityClause 逐字对齐。
 *
 * `cutoff` 为空（这条会话还没有节点）时原样返回：没有回退就不该有任何东西被藏起来。
 */
export function dropAfterCutoff<T extends CutoffFilterable>(items: readonly T[], cutoff: string | undefined | null, visibleAfter?: string | undefined | null): T[] {
  const at = timestamp(cutoff);
  if (at === null) return [...items];
  const after = timestamp(visibleAfter);
  return items.filter((item) => {
    const itemAt = timestamp(item.createdAt);
    if (itemAt === null) return true;
    if (itemAt <= at) return true;
    return after !== null && itemAt > after;
  });
}

/** 节点时间是否有效（服务端可能因为老版本不返回这个字段）。 */
export function hasCutoff(cutoff: string | undefined | null): boolean {
  return timestamp(cutoff) !== null;
}
