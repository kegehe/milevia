// 手机端系统通知的纯逻辑。
//
// 单独成文件、**不 import 任何 Capacitor 模块**，是为了能被 `node --test` 直接跑
// （同 features/updater/android-release.ts 与 lib/mobile-devices.ts 的既有做法）。
// 真正碰插件的那一层留在 MobileRemotePage 里，本文件只回答"该不该发、发什么、跳哪"。
//
// 背景：手机端的系统通知有两条通道，语义完全不同 ——
//   · 原生包（Capacitor Android）走 @capacitor/local-notifications 的**本地通知**，
//     由 App 自己把已经拿到的事件丢进系统通知栏。不需要服务器、不需要 FCM/厂商推送，
//     但**前提是 App 进程还活着**。
//   · 浏览器（手机直接开网页）走 Web Notification API，判定见 lib/notifications.ts
//     的 webNotificationsSupported —— 它回答的是"Web Notification 能不能用"，
//     原生包里那个答案仍然是不能用（Android WebView 不把 Web Notification 接到通知栏）。
// 两者不要合并成一个函数：同一个问题"能不能发通知"，两边成立的理由不同。

/** 当前环境的通知投递通道。 */
export function notificationBackend(env: {
  isNativePlatform: boolean;
  hasNotificationAPI: boolean;
}): "native" | "web" | "none" {
  if (env.isNativePlatform) return "native";
  return env.hasNotificationAPI ? "web" : "none";
}

/**
 * 插件权限态 → 页面既有的 `NotificationPermission | "unsupported"` 口径。
 *
 * 插件用的是 `'prompt' | 'prompt-with-rationale' | 'granted' | 'denied'`，后两者与
 * `NotificationPermission` 同名；前两者都是"还没授权"，一律映射成 `"default"` ——
 * 页面上那两颗「开启通知」入口判的就是 `=== "default"`，映射过来它们的判定一个字不用改。
 *
 * 读不出可识别的取值时返回 `"unsupported"`：收起入口，而不是留一颗点了没反应的哑按钮。
 */
export function toNotificationPermission(display: unknown): NotificationPermission | "unsupported" {
  if (display === "granted" || display === "denied" || display === "default") return display;
  if (display === "prompt" || display === "prompt-with-rationale") return "default";
  return "unsupported";
}

/**
 * 是否值得打扰用户。
 *
 * 会话消息（`assistant.delta` / `assistant.message` 等）不在其中：用户回到 App 就能看到，
 * 不需要额外提醒。需要打断的是任务与运行的**状态变化**、以及等待审批。
 * 这个判据与改版前 notifyHiddenMobileEvent 里那条正则完全一致，没有放宽也没有收紧。
 */
export function isInterruptingMobileEvent(type: unknown): boolean {
  return typeof type === "string" && /^(task\.|run\.|approval\.)/.test(type);
}

export interface MobileNotificationContent {
  title: string;
  body: string;
  /** 点通知后要跳去的项目 / 会话。都可能为空，此时靠 taskId 反查（见下）。 */
  projectId: string;
  conversationId: string;
  /**
   * 任务 id —— 点通知时定位项目的依据。
   *
   * 两个来源都要取，缺一不可：
   *   · **信封上的 `taskId`**：`task.*` 事件走 `recordTaskEventTx`，任务 id 落在 outbox 行的
   *     `task_id` 列上，由 Agent 转发成信封字段；这类事件的 payload 里没有项目/会话字段
   *     （`remoteEventPayloadWithConversation` 只给显式传了 conversationID 的那条重载补写，
   *     而 `recordTaskEventTx` 走的是不带会话的那条）。
   *   · **payload 里的 `taskId`**：运行结束时控制端主动塞进 payload 的那个
   *     （`app.go` 的 `payload["taskId"] = payloadTaskID`），因为前端要把错误卡链回任务详情。
   *     这类事件（`run.*`）的信封 taskId 反而是空的。
   * 只取其中一个，就会有一半的事件点通知后没反应。
   */
  taskId: string;
}

/**
 * 从 SSE 事件里取出通知文案与跳转目标。
 *
 * 文案的取法与改版前一致：优先 `payload.summary`，其次 `状态：<status>`，最后退回事件类型。
 * 不值得打扰的事件返回 null。
 */
export function mobileNotificationContent(event: { type?: unknown; taskId?: unknown; payload?: unknown }): MobileNotificationContent | null {
  if (!isInterruptingMobileEvent(event.type)) return null;
  const payload = event.payload && typeof event.payload === "object" ? event.payload as Record<string, unknown> : {};
  const summary = typeof payload.summary === "string" ? payload.summary : "";
  const status = typeof payload.status === "string" ? payload.status : "";
  return {
    title: "Milevia",
    body: summary || (status ? `状态：${status}` : String(event.type)),
    projectId: typeof payload.projectId === "string" ? payload.projectId : "",
    conversationId: typeof payload.conversationId === "string" ? payload.conversationId : "",
    // 信封优先，payload 兜底（两者的来源见 taskId 字段的说明）。
    taskId: typeof event.taskId === "string" && event.taskId
      ? event.taskId
      : typeof payload.taskId === "string" ? payload.taskId : "",
  };
}

/**
 * 找出通知该跳到哪个项目。
 *
 * 优先信 `projectId`；它缺失时（`task.*` 事件都是这种情况）按 `taskId` 回快照里反查 ——
 * 两个都没有就没法定位，返回 undefined 让调用方挂起或放弃，而不是随便跳一个项目。
 */
export function pickNotificationProject<T extends { id: string; tasks: { id: string }[] }>(
  projects: T[],
  target: { projectId: string; taskId: string },
): T | undefined {
  if (target.projectId) return projects.find((item) => item.id === target.projectId);
  if (!target.taskId) return undefined;
  return projects.find((item) => item.tasks.some((task) => task.id === target.taskId));
}

/**
 * 事件 id 是字符串，而插件的通知 id 必须是 32 位整数 —— 取稳定哈希。
 *
 * 必须是纯函数且无随机量：同一条事件每次要得到同一个 id，这样重复投递会**覆盖**同一条
 * 通知，而不是在通知栏里堆出一串。结果固定落在 `[0, 0x7fffffff]`（`x & 0x7fffffff` 拿到的是
 * 非负 int32，`Math.abs` 会在 -2147483648 上溢出到 int32 之外），0 让给 1 以保证非零。
 */
export function notificationIDFromEventID(eventId: string): number {
  let hash = 0;
  for (let i = 0; i < eventId.length; i++) hash = (hash * 31 + eventId.charCodeAt(i)) | 0;
  return (hash & 0x7fffffff) || 1;
}

/**
 * 现在该不该发"你看不见，所以提醒你"的通知。
 *
 * 浏览器只能看 `document.hidden`。原生包多一条更可靠的信号：Capacitor 的前后台事件
 * （`App.addListener("appStateChange")`）—— Android WebView 里 `visibilitychange` 是否
 * 可靠触发**没有验证记录**，若它不触发，只判 `document.hidden` 会让通知永远发不出去；
 * 反过来它若在 App 前台误判成 hidden，只判它就会在用户正看着时弹通知。两条取或，
 * 任一条说"用户不在看"就提醒。
 */
export function shouldNotifyWhileAway(env: {
  isNative: boolean;
  nativeInBackground: boolean;
  documentHidden: boolean;
}): boolean {
  return env.documentHidden || (env.isNative && env.nativeInBackground);
}

/**
 * Android 通知渠道。
 *
 * ⚠️ 两个必须一次做对的点：
 *  1. `channelId` 指向**不存在**的渠道时，通知**不会发出**（不是退回默认渠道）——
 *     所以必须在发通知之前把渠道建好。
 *  2. 渠道一旦创建，**配置不可改**：改重要性/震动/声音只能换一个新的 id。
 * 因此重要性定为 4（HIGH）：横幅弹出 + 声音 + 震动。任务完成这类提醒要的就是"打断一下"；
 * 定成 3（DEFAULT）只会静默躺在通知栏里，等于没提醒。
 *
 * 这里只放纯数据、不带插件类型，调用方建渠道时再转成插件的 `Channel`。
 */
export const MOBILE_NOTIFICATION_CHANNEL = {
  id: "milevia-tasks",
  name: "任务通知",
  description: "任务完成、失败与需要人工介入时提醒",
  importance: 4,
  vibration: true,
} as const;
