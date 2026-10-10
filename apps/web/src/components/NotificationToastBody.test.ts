import assert from "node:assert/strict";
import { test } from "node:test";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { ToastCountdownClock } from "./toast-countdown-clock";

const here = dirname(fileURLToPath(import.meta.url));
const read = (name: string) => readFileSync(join(here, name), "utf8");

test("countdown clock counts down while running", () => {
  let t = 0;
  const clock = new ToastCountdownClock(8000, () => t);
  clock.resume();
  assert.equal(clock.isRunning(), true);

  t = 3000;
  assert.equal(clock.remaining(), 5000);
  t = 7999;
  assert.equal(clock.remaining(), 1);
});

test("pause freezes the snapshot and resume continues from the pause point", () => {
  let t = 0;
  const clock = new ToastCountdownClock(8000, () => t);
  clock.resume();

  t = 3000;
  clock.pause();
  assert.equal(clock.isRunning(), false);
  assert.equal(clock.remaining(), 5000);

  // 暂停期间墙钟照走，但快照不跟
  t = 10_000;
  assert.equal(clock.remaining(), 5000);

  // 恢复后从暂停点继续走
  clock.resume();
  t = 12_000;
  assert.equal(clock.remaining(), 3000);
});

test("pause/resume are idempotent and never go negative", () => {
  let t = 0;
  const clock = new ToastCountdownClock(8000, () => t);
  clock.pause(); // 未运行时暂停：空操作
  assert.equal(clock.remaining(), 8000);

  clock.resume();
  t = 9999; // 越过终点
  assert.equal(clock.remaining(), 0);
  clock.pause();
  assert.equal(clock.remaining(), 0);

  clock.resume(); // 已耗尽：拒绝再走（对应 toast 即将自动关闭）
  assert.equal(clock.isRunning(), false);
});

// —— 结构断言：三处接线（Provider → 正文组件 → CSS）少任何一环都立刻红 ——

test("provider wires the notif-toast class and the countdown body", () => {
  const provider = read("NotificationProvider.tsx");
  // className 与正文组件必须同时接线，否则样式钩子或倒计时元素有一边落空
  assert.match(provider, /className:\s*"notif-toast"/);
  assert.match(provider, /<NotificationToastBody\b/);
  // 时长唯一来源：duration 与传给正文的 durationMs 同出一个常量
  assert.match(provider, /NOTIFICATION_TOAST_DURATION_MS/);
  assert.match(provider, /isHighPriority \? Infinity : NOTIFICATION_TOAST_DURATION_MS/);
});

test("countdown body mirrors sonner pause conditions and drives the bar from JS", () => {
  const body = read("NotificationToastBody.tsx");
  assert.match(body, /ToastCountdownClock/); // 计时必须走收口的时钟，不得另起炉灶
  // 两个暂停条件必须真的注册了监听（钉到 addEventListener 接线上，
  // 而不是 grep 关键词 —— removeEventListener/注释里的同名词挡不住漏接线）
  assert.match(body, /toaster\?\.addEventListener\("mouseenter"/); // 镜像 sonner expanded（容器悬停暂停）
  assert.match(body, /document\.addEventListener\("visibilitychange"/); // 镜像 sonner isDocumentHidden
  assert.match(body, /scaleX\(/); // 进度条由 JS 内联驱动，非 CSS 动画
  assert.match(body, /保留至处理/); // 高优先级常驻文案
  assert.match(body, /aria-hidden/); // 倒计时元素不进读数
});

test("notification.css styles the track anchored to the toast bottom", () => {
  const css = read("../notification.css");
  assert.match(css, /\.notif-progress\s*\{[^}]*position:\s*absolute[^}]*bottom:\s*0/);
  assert.match(css, /\.notif-progress-bar\s*\{[^}]*transform-origin/);
  assert.match(css, /\.notif-remaining\s*\{[^}]*position:\s*absolute/);
  assert.match(css, /\.notif-project\s*\{[^}]*color/);
  // 图标居中修复：grid 的轨道对齐必须显式居中（sonner 的 justify-content:flex-start
  // 会把 16px 单列轨道压到最左，svg 偏左 6px，透明圆环底下肉眼可见）
  assert.match(css, /\[data-sonner-toast\] \[data-icon\]\s*\{[^}]*justify-content:\s*center[^}]*\}/);
  // 关闭按钮必须收进卡片内（sonner 默认骑在左上角外沿，悬出 6px）
  assert.match(
    css,
    /\[data-sonner-toast\]\.notif-toast \[data-close-button\]\s*\{[^}]*right:\s*8px[^}]*transform:\s*none/,
  );
});
