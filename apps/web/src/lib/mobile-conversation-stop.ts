/**
 * 手机端"停止当前对话"的判据层。
 *
 * 单独抽成纯函数，是因为这条链路上真正会出错的地方既不是那个按钮、也不是那句提示，而是
 * **把电脑端的答复读成了什么**：停止的答复有四种（真的在停 / 本来就没在跑 / 还有排队中的
 * 请求要用户确认 / 失败），而它们在线上都长成一个 JSON。判错的症状不是崩，是"点了停止，
 * 界面什么也没说"或者"弹出一句用户看不懂的话"。页面里那几十行 JSX 没法为这件事写断言，
 * 这里能。
 *
 * 通道形态见 docs/40（一条 WSS 请求-响应、op 名单只在电脑端一份）；这份只管
 * `conversation.stop` 这一个 op 的答复怎么读。
 */

import type { MobileRpcReply } from "../features/remote/mobile-rpc";

export type ConversationStopOutcome =
  /** 电脑端已经在停了。界面保持"停止中"，等实时事件/快照把运行态收掉。 */
  | { kind: "stopping" }
  /** 这条会话**没有活跃的一轮**（服务端为此专门回 `idle`，而不是报错）。 */
  | { kind: "idle" }
  /** 有一轮在排队/运行，但它不由这台电脑控制（服务端原样回那一轮自己的状态，如 `queued`）。 */
  | { kind: "uncontrolled" }
  /** 这条对话还有排队中/执行中的请求，单独停这一个会留下正在跑的那个。需要用户确认。 */
  | { kind: "needs-force" }
  | { kind: "failed"; message: string };

/** 停止请求请云端的等多久（`timeoutMs`：客户端提议、云端夹取）。
 *
 *  为什么不吃默认的 20s：服务端这个 handler 要拿一把**全局**的流式互斥锁（`streamMu`），
 *  而跟它争同一把锁的还有"某条会话正在冷启动一个 AI 进程"（WSL/SSH 冷启动、或另一端
 *  正在发消息 —— 那条路径把锁一直握到 `runner.StartSession` 与首个 `Send` 返回）。
 *  默认 20s 在这条路径上会被真实地跑爆，症状是手机拿到一句"电脑端没有在 20 秒内回应这次请求"，
 *  而那次停止其实在几秒后真的生效了 —— 正是本仓诊断过多次的"客户端预算低于服务端预算 = 假失败"。
 *  45s 与 Git 那条"本地写"同档（那边同样是被服务端自己的耗时逼出来的）。
 *  手机端自己的等待上限由 `cloudClientWaitMs` 在这个数之上再加 5s（见 features/remote/cloud-budget.ts）。
 */
export const conversationStopTimeoutMs = 45_000;

/** 停止请求的 params。
 *
 * 无 force 时是**空对象**而不是 `{force:"false"}`：中继那条通道把 params 整个当查询串
 * （`Query: true`），而它要求取值都是字符串 —— 塞一个布尔 `true` 进去，电脑端会回一句
 * "params 必须是字符串键值对"，那是用户完全无法理解的失败。写成函数而不是就地写字符串，
 * 就是为了让这条约束有一个能被断言的地方。
 */
export function conversationStopParams(force: boolean): Record<string, string> {
  return force ? { force: "true" } : {};
}

/** 停止键与强制停止确认框共用的文案。与桌面端 ConversationPage 逐字一致 —— 同一个动作在
 * 两端说法不同，用户会以为是两件事。 */
export const conversationStopConfirmCopy = {
  title: "强制停止",
  message: "该对话还有其他排队中或执行中的请求，强制停止将一并取消它们。是否继续？",
  confirm: "强制停止",
} as const;

export function conversationStopOutcome(reply: MobileRpcReply): ConversationStopOutcome {
  if (!reply.ok) {
    // 判据必须是**码**，不是文案：那句 error 由电脑端本地化（writeError →
    // localizedHTTPErrorText），拿它当判据的分支会在真实链路上静默失效，而喂原文的单测
    // 照样绿。active_runs_present 正是 httpErrorCode 专门为这件事产出的码。
    if (reply.code === "active_runs_present") return { kind: "needs-force" };
    const message = (reply.error || "").trim();
    return { kind: "failed", message: message || "电脑端没有执行这次停止" };
  }
  // "stopping" 是唯一表示"真的在停"的状态。其余都表示这一趟什么都没停，但两种情况**要分开**：
  //   · idle —— 这条会话没有活跃的一轮（服务端专门回它，而不是报错："按下停止"与"那一轮
  //     恰好自己跑完"是常态竞态）；
  //   · 其它（queued / completed / failed / stopped …）—— stopRunByID 原样回那一轮自己的
  //     状态，意味着它还在，只是不由这台电脑控制。对用户要说的那句与"已经跑完了"完全不同：
  //     前者是"稍后重试"，后者是"不用管了"。合成一句就会把还在排队的说成"没有在运行的任务"。
  const status = readStopStatus(reply.data);
  if (status === "stopping") return { kind: "stopping" };
  return status === "idle" || status === "" ? { kind: "idle" } : { kind: "uncontrolled" };
}

/** 这一趟什么都没停时给用户看的那句话。由调用方上屏，所以文案与判据放在一起 ——
 *  "还没停成"和"已经跑完了"是两件事，分开说。 */
export function conversationStopIdleMessage(outcome: ConversationStopOutcome): string {
  return outcome.kind === "uncontrolled"
    ? "这一轮暂时停不下来（可能还在排队，或不由这台电脑控制），请稍后重试。"
    : "这条对话当前没有在运行的任务，可能刚刚已经结束了。";
}

function readStopStatus(data: unknown): string {
  if (!data || typeof data !== "object") return "";
  const status = (data as { status?: unknown }).status;
  return typeof status === "string" ? status : "";
}
