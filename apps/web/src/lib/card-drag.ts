// 卡片「按住浮起 → 拖动排序」的纯逻辑层。
// 这里只放能在 Node 里直接断言的判定与几何计算；指针事件、DOM、动画在 use-card-drag.ts。
//
// 手感的四条硬规则（改之前先读这里，配套断言在 card-drag.test.ts）：
//  ① 鼠标（含触控笔）位移超过 6px 就抓起 —— 拖动是训练有素的手势，不该让人等长按；
//  ② 鼠标原地按住 240ms 也会抓起 —— 「按住卡片就有悬浮感」这句话得成立；
//  ③ 触摸必须长按 320ms，长按期间位移超过 10px 就放弃 —— 否则会和页面滚动抢手势；
//  ④ 抓起后按主导轴判前后（两列网格看左右、单列看上下），不是按"离哪个更近"。

/** 鼠标/触控笔：位移超过这么多像素即视为抓起。 */
export const MOUSE_LIFT_MOVE_PX = 6;
/** 鼠标/触控笔：原地按住这么久也抓起。 */
export const MOUSE_LIFT_HOLD_MS = 240;
/** 触摸：必须按住这么久才抓起。 */
export const TOUCH_LIFT_HOLD_MS = 320;
/** 触摸：长按期间位移超过这么多像素就放弃（把手势还给页面滚动）。 */
export const TOUCH_ABORT_MOVE_PX = 10;

/** 把浮起的卡片贴到视口上下边缘这么多像素内时开始自动滚动。 */
export const AUTOSCROLL_EDGE_PX = 96;
/** 自动滚动的每帧最大位移（px）。 */
export const AUTOSCROLL_MAX_PX = 22;

export type LiftDecision = "idle" | "lift" | "abort";

export type LiftInput = {
  pointerType: string;
  /** 自 pointerdown 起算的累计位移（px）。 */
  moved: number;
  /** 自 pointerdown 起算的时长（ms）。 */
  held: number;
};

/** 长按定时器该挂多久：触摸要更久，鼠标短一些。 */
export function liftHoldMs(pointerType: string): number {
  return pointerType === "touch" ? TOUCH_LIFT_HOLD_MS : MOUSE_LIFT_HOLD_MS;
}

/** 一次指针手势该「继续等」「抓起」还是「放弃」。 */
export function decideLift(input: LiftInput): LiftDecision {
  if (input.pointerType === "touch") {
    if (input.moved > TOUCH_ABORT_MOVE_PX) return "abort";
    return input.held >= TOUCH_LIFT_HOLD_MS ? "lift" : "idle";
  }
  if (input.moved > MOUSE_LIFT_MOVE_PX) return "lift";
  return input.held >= MOUSE_LIFT_HOLD_MS ? "lift" : "idle";
}

export type CardRect = { left: number; top: number; width: number; height: number };

export type DropSlot = {
  /** 落点参照的槽位在当前完整顺序里的下标。 */
  index: number;
  /** true = 插到这个槽位之后，false = 插到它之前。 */
  after: boolean;
};

/**
 * 指针落在哪个槽位、落在该槽位的哪一侧。
 *
 * 先取中心点最近的槽位（两列网格里就是「离哪张卡最近」），再按**主导轴**判前后：
 * 把偏移量按槽位自身尺寸归一化后，哪个方向偏离得多就按那个方向判。
 * 两列网格里主导轴是水平（左半 → 插到它前面、右半 → 插到它后面），
 * 窄屏单列时自然退化成垂直，同一份代码两种布局都成立。
 */
export function resolveDropSlot(point: { x: number; y: number }, rects: CardRect[]): DropSlot | null {
  if (rects.length === 0) return null;

  let best = 0;
  let bestDistance = Number.POSITIVE_INFINITY;
  for (let index = 0; index < rects.length; index += 1) {
    const rect = rects[index];
    const dx = point.x - (rect.left + rect.width / 2);
    const dy = point.y - (rect.top + rect.height / 2);
    const distance = dx * dx + dy * dy;
    if (distance < bestDistance) {
      bestDistance = distance;
      best = index;
    }
  }

  const rect = rects[best];
  const centerX = rect.left + rect.width / 2;
  const centerY = rect.top + rect.height / 2;
  const halfWidth = rect.width / 2 || 1;
  const halfHeight = rect.height / 2 || 1;
  const normalizedX = Math.abs(point.x - centerX) / halfWidth;
  const normalizedY = Math.abs(point.y - centerY) / halfHeight;
  const after = normalizedX >= normalizedY ? point.x > centerX : point.y > centerY;
  return { index: best, after };
}

/**
 * 指针贴边时该滚多少（负=向上、正=向下），0 表示不用滚。
 * 越靠边越快，但在边界处必须是 0，否则指针停在边缘不动时页面会自己一直滚。
 */
export function autoscrollDelta(pointerY: number, viewportHeight: number): number {
  // 视口高度拿不到时（隐藏容器、布局还没算完）什么都不做。
  // 不挡这一下的话 bottomEdge 会变成负数，指针"永远在底边之下"——页面会自己一路滚到底。
  if (!Number.isFinite(viewportHeight) || viewportHeight <= 0) return 0;
  if (pointerY < AUTOSCROLL_EDGE_PX) {
    const depth = AUTOSCROLL_EDGE_PX - Math.max(0, pointerY);
    const ratio = depth / AUTOSCROLL_EDGE_PX;
    return -Math.max(1, Math.round(ratio * AUTOSCROLL_MAX_PX));
  }
  const bottomEdge = viewportHeight - AUTOSCROLL_EDGE_PX;
  if (pointerY > bottomEdge) {
    const depth = Math.min(viewportHeight, pointerY) - bottomEdge;
    const ratio = depth / AUTOSCROLL_EDGE_PX;
    return Math.max(1, Math.round(ratio * AUTOSCROLL_MAX_PX));
  }
  return 0;
}
