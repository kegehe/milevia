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
 *     -> { ok, status, data?, error?, code? }
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

/**
 * 云端自己造的那条掉线失败码（cloud 的 `failRPCRequestsForInstance`）。
 *
 * 它与 HTTP 409 那条不是一回事：请求**已经发给电脑、正在等回话**时电脑掉线
 * （或被同一台电脑的新连接顶替），云端会就地把它判死，回包是 HTTP 200 + `ok:false`，
 * 于是适配器会把这个 `error` **原样显示**给用户 —— 它必须是中文。
 */
export const INSTANCE_OFFLINE_CODE = "instance_offline";

/** 那句话的中文形态，与云端 `instanceOfflineMessage` 逐字相同。 */
export const INSTANCE_OFFLINE_MESSAGE = "电脑当前离线，请等电脑上线后再试";

/**
 * 这条失败是不是"电脑在请求进行中掉线"。
 *
 * 码优先；文案兜底只服务"旧版云端还没有 code"这一种情况。
 */
function isInstanceOffline(reply: Pick<MobileRpcReply, "error" | "code">): boolean {
  if (reply.code === INSTANCE_OFFLINE_CODE) return true;
  // error 是网络来的（`cloud()` 里断言出来的 T），先确认它真是字符串再匹配。
  return typeof reply.error === "string" && reply.error.includes(INSTANCE_OFFLINE_CODE);
}

/**
 * 把一次 `ok:false` 的回包翻成一句给用户看的话。
 *
 * **先看码，再看文案**：码是服务端显式声明的判据；文案只是它顺带带出来的说法，
 * 会被本地化、会被改措辞（本项目已经因为按文案分支踩过一次，见
 * mobile-git-request.ts 的 FAILURE_CODES 说明）。认不出的失败原样透出 ——
 * 电脑端写的那句本来就是中文。
 */
export function rpcFailureMessage(reply: Pick<MobileRpcReply, "error" | "code"> | null | undefined): string {
  if (reply && isInstanceOffline(reply)) return INSTANCE_OFFLINE_MESSAGE;
  return reply?.error || "操作失败";
}

/**
 * 通道边界上的同一件事：把回包里的 `error` 换成给人看的说法，其余字段原样保留。
 *
 * 适配器（文件 / Git）会自己调 `rpcFailureMessage`；这里是给**不经过适配器**的调用点
 * （会话、优化建议等直接读 `reply.error` 的地方）兜的同一层。
 *
 * **只改认得出来的那一种失败**：其余回包原样返回。不能顺手把空 `error` 填成
 * "操作失败" —— 那些调用点各自带着更贴切的中文兜底（如"无法读取可用模型"）。
 */
export function localizeRpcReply<T extends MobileRpcReply>(reply: T): T {
  if (reply.ok || !isInstanceOffline(reply)) return reply;
  return { ...reply, error: INSTANCE_OFFLINE_MESSAGE };
}
