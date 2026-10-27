import { MOBILE_NOTIFICATION_CHANNEL, type MobileNotificationContent } from "../../lib/mobile-notify";
import type { DeviceStore } from "../../lib/mobile-devices";

export const MOBILE_NOTIFICATION_MAX_AGE_MS = 24 * 60 * 60 * 1000;
const MAX_PENDING = 100;
const MAX_DELIVERED = 1000;
const MAX_ATTEMPTS = 3;

export type NotificationDiagnostic = { at: number; status: "scheduled" | "failed"; message: string };
export type PendingMobileNotification = {
  id: string;
  eventType?: string;
  content: MobileNotificationContent;
  createdAt: number;
  attempts: number;
  retryAt: number;
};

type QueueState = {
  pending: PendingMobileNotification[];
  delivered: Array<{ id: string; at: number }>;
  diagnostic: NotificationDiagnostic | null;
};

/** 与 React 和原生插件隔离；持久化、并发去重与重试可直接做行为测试。 */
export class MobileNotificationDelivery {
  private state: QueueState = { pending: [], delivered: [], diagnostic: null };
  private running = false;
  private stopped = false;
  private timer: ReturnType<typeof setTimeout> | undefined;
  private readonly key: string;

  constructor(private readonly options: {
    scope: string;
    store: DeviceStore;
    send: (item: PendingMobileNotification) => Promise<void>;
    enabled: () => boolean;
    shouldSend?: (item: PendingMobileNotification) => boolean;
    now?: () => number;
    onDiagnostic?: (diagnostic: NotificationDiagnostic) => void;
  }) {
    this.key = `milevia.notification-delivery.v1:${options.scope}`;
    try {
      const saved = JSON.parse(options.store.getItem(this.key) || "null") as QueueState | null;
      if (saved && Array.isArray(saved.pending) && Array.isArray(saved.delivered)) {
        this.state.pending = saved.pending.filter((item) => typeof item?.id === "string" && (item.eventType === undefined || typeof item.eventType === "string") && item.content &&
          ["title", "body", "projectId", "conversationId", "taskId"].every((key) => typeof item.content[key as keyof MobileNotificationContent] === "string") &&
          Number.isFinite(item.createdAt) && Number.isInteger(item.attempts) && item.attempts >= 0 && Number.isFinite(item.retryAt));
        this.state.delivered = saved.delivered.filter((item) => typeof item?.id === "string" && Number.isFinite(item.at));
        if (saved.diagnostic && Number.isFinite(saved.diagnostic.at) &&
          ["scheduled", "failed"].includes(saved.diagnostic.status) && typeof saved.diagnostic.message === "string") this.state.diagnostic = saved.diagnostic;
      }
    } catch { /* 损坏的本地记录不阻断新事件。 */ }
    this.prune();
  }

  private now() { return this.options.now?.() ?? Date.now(); }

  private prune() {
    const oldest = this.now() - MOBILE_NOTIFICATION_MAX_AGE_MS;
    this.state.pending = this.state.pending.filter((item) => item.createdAt >= oldest).slice(-MAX_PENDING);
    this.state.delivered = this.state.delivered.filter((item) => item.at >= oldest).slice(-MAX_DELIVERED);
  }

  private persist() {
    this.prune();
    // 不吞存储错误：持久化失败时不能让上游误以为已经安全接收并推进断点。
    this.options.store.setItem(this.key, JSON.stringify(this.state));
  }

  get diagnostic() { return this.state.diagnostic; }
  get pendingCount() { this.prune(); return this.state.pending.length; }

  enqueue(id: string, content: MobileNotificationContent, createdAt = this.now(), eventType?: string): void {
    this.prune();
    if (!id || createdAt < this.now() - MOBILE_NOTIFICATION_MAX_AGE_MS ||
      this.state.delivered.some((item) => item.id === id) || this.state.pending.some((item) => item.id === id)) return;
    const previous = this.state;
    this.state = { ...previous, pending: [...previous.pending, { id, content, createdAt, attempts: 0, retryAt: 0, ...(eventType ? { eventType } : {}) }] };
    try { this.persist(); } catch (error) {
      // persist 内会裁剪满队列；保存失败时必须连同被裁剪的旧记录一起恢复。
      this.state = previous;
      throw error;
    }
  }

  /** 前台已处理的事件也要去重；不能清除之前投递失败的待发送项。 */
  rememberHandled(id: string): void {
    this.prune();
    if (!id || this.state.pending.some((item) => item.id === id) || this.state.delivered.some((item) => item.id === id)) return;
    const previous = this.state;
    this.state = { ...previous, delivered: [...previous.delivered, { id, at: this.now() }] };
    try { this.persist(); } catch (error) { this.state = previous; throw error; }
  }

  /** 启动、回前台或权限恢复时重新尝试；自动重试每轮最多三次。 */
  resume(): Promise<void> {
    for (const item of this.state.pending) { item.attempts = 0; item.retryAt = 0; }
    return this.flush();
  }

  private settle(item: PendingMobileNotification, diagnostic = this.state.diagnostic): void {
    const next: QueueState = {
      pending: this.state.pending.filter((entry) => entry !== item),
      delivered: [...this.state.delivered, { id: item.id, at: this.now() }].slice(-MAX_DELIVERED),
      diagnostic,
    };
    this.options.store.setItem(this.key, JSON.stringify(next));
    this.state = next;
  }

  async flush(): Promise<void> {
    if (this.stopped || this.running) return;
    if (this.timer !== undefined) clearTimeout(this.timer);
    this.timer = undefined;
    this.running = true;
    try {
      this.prune();
      for (const item of [...this.state.pending]) {
        if (this.stopped || !this.options.enabled()) break;
        if (item.attempts >= MAX_ATTEMPTS || item.retryAt > this.now()) continue;
        try {
          if (this.options.shouldSend && !this.options.shouldSend(item)) {
            // 用户关闭分类或进入免打扰后，已排队的普通通知也应静默处理，不误报发送成功。
            this.settle(item);
            continue;
          }
          await this.options.send(item);
          if (this.stopped) return;
          this.settle(item, { at: this.now(), status: "scheduled", message: "通知已交给系统，请在通知栏确认显示。" });
        } catch (error) {
          if (this.stopped) return;
          item.attempts++;
          item.retryAt = this.now() + (item.attempts === 1 ? 1000 : 5000);
          this.state.diagnostic = { at: this.now(), status: "failed", message: error instanceof Error ? error.message.slice(0, 250) : "系统通知发送失败" };
          this.persist();
        }
        this.options.onDiagnostic?.(this.state.diagnostic!);
      }
    } finally {
      this.running = false;
      const retries = this.state.pending.filter((item) => item.attempts < MAX_ATTEMPTS);
      if (!this.stopped && this.options.enabled() && retries.length) {
        this.timer = setTimeout(() => { void this.flush().catch((error) => console.warn("[mobile-notifications]", error)); },
          Math.max(100, Math.min(...retries.map((item) => item.retryAt)) - this.now()));
      }
    }
  }

  stop() {
    this.stopped = true;
    if (this.timer !== undefined) clearTimeout(this.timer);
  }
}

/** 恢复时只补结果通知，不重放过时的“正在运行 / 等待审批”等中间状态。 */
export function isRecoverableMobileNotification(type: unknown): boolean {
  return typeof type === "string" && (type === "project.batch_completed" ||
    /^(run\.(completed|failed|stopped|interrupted)|task\.(done|awaiting_review|run_succeeded|run_failed|run_stopped|run_interrupted))$/.test(type));
}

type NativeNotificationAPI = {
  checkPermissions: () => Promise<{ display: string }>;
  areEnabled: () => Promise<{ value: boolean }>;
  createChannel: (channel: typeof MOBILE_NOTIFICATION_CHANNEL) => Promise<void>;
  listChannels: () => Promise<{ channels: Array<{ id: string; importance?: number }> }>;
  schedule: (options: { notifications: Array<{
    id: number; title: string; body: string; channelId: string; autoCancel: boolean; isExactNotification: boolean;
    extra: MobileNotificationContent & { instanceId: string };
  }> }) => Promise<unknown>;
};

export async function sendNativeMobileNotification(api: NativeNotificationAPI, id: number, content: MobileNotificationContent, instanceId: string, isCurrent = () => true): Promise<void> {
  if ((await api.checkPermissions()).display !== "granted" || !(await api.areEnabled()).value) {
    throw new Error("系统尚未允许 Milevia 发送通知，请在系统通知设置中开启。");
  }
  // 创建失败必须上报并重试，不能忽略后继续向不存在的渠道投递。
  try {
    await api.createChannel({ ...MOBILE_NOTIFICATION_CHANNEL });
    const channel = (await api.listChannels()).channels.find((item) => item.id === MOBILE_NOTIFICATION_CHANNEL.id);
    if (!channel || channel.importance === 0) throw new Error("“任务通知”类别未开启，请在系统通知设置中允许。");
  } catch (error) {
    // Android 7（项目 minSdk 24）没有渠道 API，插件明确返回 UNAVAILABLE。
    // 仅跳过这个能力缺失；权限、创建失败、渠道关闭等真实错误仍向上抛。
    if (!error || typeof error !== "object" || !("code" in error) || error.code !== "UNAVAILABLE") throw error;
  }
  if (!isCurrent()) throw new Error("通知所属设备已切换，暂停发送。");
  await api.schedule({ notifications: [{
    id, title: content.title, body: content.body, channelId: MOBILE_NOTIFICATION_CHANNEL.id,
    extra: { ...content, instanceId }, autoCancel: true, isExactNotification: false,
  }] });
}
