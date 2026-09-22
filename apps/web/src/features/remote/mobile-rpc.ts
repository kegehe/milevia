/**
 * 手机端远程调用（RPC）通道的共用类型。
 *
 * 这条通道承载**多个领域**的操作：文件（`fs.*`）与 Git（`git.*`）。
 * 抽在这里而不是留在某个领域的适配器里，是因为 `ok` / `status` / `data` / `error`
 * 四个字段的**语义**属于通道，不属于任何一个领域 —— 抄第二份必然有一天两边不一致，
 * 而症状是"文件那边好好的、Git 那边把业务失败当通道失败处理"。
 *
 * 通道契约（与云端 `rpcResponse`、电脑端 `relayRPCRequest` 一一对应）：
 *
 *   POST /v1/instances/{id}/rpc
 *   { op, projectId, conversationId, params, timeoutMs? }
 *     -> { ok, status, data?, error? }
 *
 * **HTTP 码只表示这条通道是否走通**（409 电脑离线 / 504 电脑没回话 / 413 请求帧太大），
 * 响应体里的 `ok` 才表示那次操作是否成功。见 docs/40 §7.1、docs/41 §4.2。
 */

/** 一次中继请求的答复。与云端 `rpcResponse` 一一对应。 */
export interface MobileRpcReply {
  ok: boolean;
  status: number;
  data?: unknown;
  error?: string;
  /**
   * 电脑端给的**稳定机器码**（如 409 工作区被占用时的 `workspace_occupied`）。
   *
   * 有它就必须按它分支，不要按 `error` 那句文案：那句话会被电脑端本地化
   * （control-server 的 `localizedErrorText`），拿它当判据的分支会在真实链路上
   * 静默失效，而单测喂原文照样绿。没有可判的码时它是空的，那时才退回去匹配文案。
   */
  code?: string;
}

/**
 * 发送一条远程操作。**实现方（页面）负责**：带上实例 id 与令牌、把网络层的失败
 * 转成 reject（消息已是给人看的中文），而把 `ok:false` 原样交给适配器 ——
 * 那是"操作失败"，不是"通道失败"，两者的界面分支不同。
 *
 * `timeoutMs` 是**客户端提议、云端夹取**的等待上限（见 docs/41 §3.2）：云端不解析 op，
 * 所以它无从知道"push 要比读状态等更久"；而适配器知道 —— 让它提，云端只负责夹在
 * 合法区间内。省略即用云端默认值（fs 的读写都够用，所以文件适配器一直不传）。
 */
export type MobileRpcTransport = (op: string, params: unknown, timeoutMs?: number) => Promise<MobileRpcReply>;
