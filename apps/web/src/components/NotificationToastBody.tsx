import { useEffect, useRef, useState } from "react";
import { ToastCountdownClock } from "./toast-countdown-clock";

/**
 * 通知 toast 的正文（方案E轨迹卡）：描述正文 + 右下角「Ns 后自动收起」提示
 * + 底部倒计时进度条。倒计时文字与进度条共用同一次 remaining 读数，永远一致。
 *
 * 高优先级通知（durationMs 为 Infinity）没有自动收起：进度条满格、
 * 文案固定为「保留至处理」。
 *
 * 暂停接线必须镜像 sonner v2 内部 Timer 的两个暂停条件，否则读数与真实
 * 消失时机脱节：
 * - 容器悬停：sonner 在 ol[data-sonner-toaster] 上把 expanded 置 true 并暂停；
 * - 页面隐藏：sonner 的 useIsDocumentHidden 无条件暂停。
 */
export function NotificationToastBody({ body, durationMs }: { body: string; durationMs: number }) {
  const isHold = !Number.isFinite(durationMs);
  const [remainingMs, setRemainingMs] = useState(durationMs);
  const anchorRef = useRef<HTMLSpanElement | null>(null);

  useEffect(() => {
    if (isHold) return;
    const clock = new ToastCountdownClock(durationMs);
    const anchor = anchorRef.current;
    const toaster = anchor?.closest("[data-sonner-toaster]") ?? null;

    let hovered = toaster?.matches(":hover") ?? false;
    const sync = () => {
      if (hovered || document.hidden) clock.pause();
      else clock.resume();
    };
    const onToasterEnter = () => {
      hovered = true;
      sync();
    };
    const onToasterLeave = () => {
      hovered = false;
      sync();
    };
    const onVisibilityChange = () => sync();

    toaster?.addEventListener("mouseenter", onToasterEnter);
    toaster?.addEventListener("mouseleave", onToasterLeave);
    document.addEventListener("visibilitychange", onVisibilityChange);
    sync();

    let raf = 0;
    let paintedBucket = -1;
    const tick = () => {
      const left = clock.remaining();
      // 100ms 粒度刷新：进度条平滑、秒数变化必刷新，避免每帧 setState
      const bucket = Math.ceil(left / 100);
      if (bucket !== paintedBucket) {
        paintedBucket = bucket;
        setRemainingMs(left);
      }
      if (left > 0) raf = requestAnimationFrame(tick);
    };
    raf = requestAnimationFrame(tick);

    return () => {
      cancelAnimationFrame(raf);
      toaster?.removeEventListener("mouseenter", onToasterEnter);
      toaster?.removeEventListener("mouseleave", onToasterLeave);
      document.removeEventListener("visibilitychange", onVisibilityChange);
      clock.pause();
    };
  }, [durationMs, isHold]);

  const ratio = isHold ? 1 : Math.max(0, remainingMs) / durationMs;
  const seconds = Math.max(0, Math.ceil(remainingMs / 1000));

  return (
    // display:contents：不产生盒子、不影响 [data-description] 的排版；
    // 绝对定位的进度条/提示以 toast（li，position:absolute）为包含块。
    <span ref={anchorRef} style={{ display: "contents" }}>
      {body}
      <span className="notif-remaining" aria-hidden="true">
        {isHold ? "保留至处理" : `${seconds}s 后自动收起`}
      </span>
      <i className="notif-progress" aria-hidden="true">
        <i className="notif-progress-bar" style={{ transform: `scaleX(${ratio})` }} />
      </i>
    </span>
  );
}
