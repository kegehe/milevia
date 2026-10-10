/**
 * 通知 toast 倒计时的核心读数（方案E轨迹卡）。
 *
 * 纯逻辑、无 DOM：组件（NotificationToastBody）负责驱动与暂停接线，
 * 行为由 NotificationToastBody.test.ts 直接钉住。
 *
 * 为什么不用 CSS 动画：sonner v2 的内部 Timer 会在「容器悬停（expanded）」
 * 和「页面隐藏」时暂停计时，CSS 动画跟不住这两个暂停，进度条/文案会和
 * toast 的真实消失时机脱节（悬停时条走完了、toast 却不走；切后台回来
 * 条已经空了、toast 却还在）。
 */
export class ToastCountdownClock {
  private remainingMs: number;
  /** 当前连续运行段的起点时间戳（pause 时结算） */
  private segmentStart = 0;
  private running = false;

  constructor(
    readonly durationMs: number,
    private readonly now: () => number = Date.now,
  ) {
    this.remainingMs = durationMs;
  }

  isRunning(): boolean {
    return this.running;
  }

  /** 暂停：把已流逝的时间结算进剩余量，之后的 remaining() 冻结在快照上。 */
  pause(): void {
    if (!this.running) return;
    this.remainingMs = Math.max(0, this.remainingMs - (this.now() - this.segmentStart));
    this.running = false;
  }

  /** 从暂停点继续；已耗尽后再 resume 是空操作（对应 toast 即将自动关闭）。 */
  resume(): void {
    if (this.running || this.remainingMs <= 0) return;
    this.segmentStart = this.now();
    this.running = true;
  }

  /** 当前剩余毫秒：运行中按时间源即时推算，暂停时取暂停时刻的快照。 */
  remaining(): number {
    if (!this.running) return this.remainingMs;
    return Math.max(0, this.remainingMs - (this.now() - this.segmentStart));
  }
}
