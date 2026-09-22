// 卡片拖拽的判定与几何：全部是纯函数，直接调用来断言。
// 这些数字（6px / 240ms / 320ms / 10px）是手感本身，不是实现细节 —— 改动必须在这里留下痕迹。
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  AUTOSCROLL_EDGE_PX,
  MOUSE_LIFT_HOLD_MS,
  MOUSE_LIFT_MOVE_PX,
  TOUCH_ABORT_MOVE_PX,
  TOUCH_LIFT_HOLD_MS,
  autoscrollDelta,
  decideLift,
  liftHoldMs,
  resolveDropSlot,
  type CardRect,
} from "./card-drag";

const rect = (left: number, top: number, width = 200, height = 120): CardRect => ({ left, top, width, height });

// 这条看着"只是抄了一遍常量"，其实是整套测试里唯一有咬合力的那条：
// 下面所有断言都拿常量当期望值，常量一改两边同步变，改坏阈值照样全绿。
// 手感就是这几个数 —— 动它们必须显式改这里，让 diff 里留下痕迹。
test("手感阈值的具体数值被钉死", () => {
  assert.equal(MOUSE_LIFT_MOVE_PX, 6);
  assert.equal(MOUSE_LIFT_HOLD_MS, 240);
  assert.equal(TOUCH_LIFT_HOLD_MS, 320);
  assert.equal(TOUCH_ABORT_MOVE_PX, 10);
});

test("鼠标：位移过阈值就抓起，不需要长按", () => {
  assert.equal(decideLift({ pointerType: "mouse", moved: MOUSE_LIFT_MOVE_PX - 1, held: 0 }), "idle");
  assert.equal(decideLift({ pointerType: "mouse", moved: MOUSE_LIFT_MOVE_PX + 1, held: 0 }), "lift");
});

test("鼠标：原地按住也会抓起（「按住卡片就有悬浮感」这句话靠这条成立）", () => {
  assert.equal(decideLift({ pointerType: "mouse", moved: 0, held: MOUSE_LIFT_HOLD_MS - 1 }), "idle");
  assert.equal(decideLift({ pointerType: "mouse", moved: 0, held: MOUSE_LIFT_HOLD_MS }), "lift");
});

test("触控笔按鼠标那套规则走，不按触摸那套", () => {
  assert.equal(decideLift({ pointerType: "pen", moved: MOUSE_LIFT_MOVE_PX + 1, held: 0 }), "lift");
  assert.equal(liftHoldMs("pen"), MOUSE_LIFT_HOLD_MS);
});

test("触摸：位移先于长按发生就放弃（把滚动交还给页面）", () => {
  // 已经按住很久，但手指划出去了 —— 这是滚动，不是拖卡片。
  assert.equal(decideLift({ pointerType: "touch", moved: TOUCH_ABORT_MOVE_PX + 1, held: TOUCH_LIFT_HOLD_MS * 4 }), "abort");
  // 长按本身不 abort，仍然等满时长才浮起。
  assert.equal(decideLift({ pointerType: "touch", moved: 0, held: TOUCH_LIFT_HOLD_MS - 1 }), "idle");
  assert.equal(decideLift({ pointerType: "touch", moved: 0, held: TOUCH_LIFT_HOLD_MS }), "lift");
  assert.equal(liftHoldMs("touch"), TOUCH_LIFT_HOLD_MS);
});

test("触摸的长按阈值必须比鼠标长：短按就浮起会和滚动抢手势", () => {
  assert.ok(TOUCH_LIFT_HOLD_MS > MOUSE_LIFT_HOLD_MS);
});

test("落点：没有槽位时给 null，不是给 0", () => {
  assert.equal(resolveDropSlot({ x: 10, y: 10 }, []), null);
});

test("落点：单张卡片时按左右的哪一半判前后", () => {
  const rects = [rect(0, 0)];
  assert.deepEqual(resolveDropSlot({ x: 20, y: 60 }, rects), { index: 0, after: false });
  assert.deepEqual(resolveDropSlot({ x: 180, y: 60 }, rects), { index: 0, after: true });
});

test("落点：两列网格里按水平判定（左右），不按垂直", () => {
  // 两列：0 在左上、1 在右上、2 在左下、3 在右下。
  const rects = [rect(0, 0), rect(220, 0), rect(0, 140), rect(220, 140)];
  // 指针停在右下那张的左半 → 插到它前面。
  assert.deepEqual(resolveDropSlot({ x: 240, y: 200 }, rects), { index: 3, after: false });
  // 右半 → 插到它后面。
  assert.deepEqual(resolveDropSlot({ x: 400, y: 200 }, rects), { index: 3, after: true });
  // 左下那张同理，且必须认到左下（index 2）而不是"最近的上排"。
  assert.deepEqual(resolveDropSlot({ x: 20, y: 200 }, rects), { index: 2, after: false });
  assert.deepEqual(resolveDropSlot({ x: 190, y: 200 }, rects), { index: 2, after: true });
});

test("落点：窄屏单列时按垂直判定（上下）", () => {
  const rects = [rect(0, 0, 320, 100), rect(0, 110, 320, 100)];
  // 上排的下半 → 插到它后面；上半 → 插到它前面。
  assert.deepEqual(resolveDropSlot({ x: 160, y: 90 }, rects), { index: 0, after: true });
  assert.deepEqual(resolveDropSlot({ x: 160, y: 20 }, rects), { index: 0, after: false });
  assert.deepEqual(resolveDropSlot({ x: 160, y: 200 }, rects), { index: 1, after: true });
});

test("落点：取中心点最近的槽位（两列之间的空档靠这个收敛）", () => {
  const rects = [rect(0, 0), rect(400, 0)];
  assert.equal(resolveDropSlot({ x: 100, y: 60 }, rects)?.index, 0);
  assert.equal(resolveDropSlot({ x: 480, y: 60 }, rects)?.index, 1);
});

test("自动滚动：中间不滚，贴边才滚，且越靠边越快", () => {
  assert.equal(autoscrollDelta(400, 800), 0);
  // 边界处必须是 0，否则指针停在边缘不动时页面会自己一直滚。
  assert.equal(autoscrollDelta(AUTOSCROLL_EDGE_PX, 800), 0);
  assert.equal(autoscrollDelta(800 - AUTOSCROLL_EDGE_PX, 800), 0);
  const shallow = autoscrollDelta(AUTOSCROLL_EDGE_PX - 8, 800);
  const deep = autoscrollDelta(2, 800);
  assert.ok(shallow < 0);
  assert.ok(deep < shallow);
  const bottomShallow = autoscrollDelta(800 - AUTOSCROLL_EDGE_PX + 8, 800);
  const bottomDeep = autoscrollDelta(798, 800);
  assert.ok(bottomShallow > 0);
  assert.ok(bottomDeep > bottomShallow);
  // 指针跑到视口外（拖到浏览器窗口下方）也不该无限加速。
  assert.equal(autoscrollDelta(5000, 800), autoscrollDelta(800, 800));
});

test("视口高度不可用时什么都不滚（否则底边成负数 = 指针永远贴边、页面一路滚到底）", () => {
  assert.equal(autoscrollDelta(400, 0), 0);
  assert.equal(autoscrollDelta(400, -1), 0);
  assert.equal(autoscrollDelta(400, Number.NaN), 0);
  assert.equal(autoscrollDelta(400, Number.POSITIVE_INFINITY), 0);
});
