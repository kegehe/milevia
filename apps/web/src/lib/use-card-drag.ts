// 卡片「按住浮起 → 拖动排序」的指针/动画层。
//
// 为什么不用原生 HTML5 拖放：浏览器给的拖拽影像是**静态位图**，既不能缩放倾斜、
// 也没有阴影，拖起来就是一张半透明的原尺寸快照；而且拖动过程中被挤开的卡片不会动，
// 只能松手后整块跳一次。要「浮起 + 跟着指针走 + 邻居让位」这三件事，只能自己接指针。
//
// 三个关键分层（改的时候别把它们揉在一起）：
//  ① 判定：位移/长按/落点/自动滚动 → lib/card-drag.ts（纯函数，有单测）
//  ② 跟随：外层 .card-drag-ghost 只做 translate，**不能有 transition**（否则拖在指针后面）
//  ③ 浮起：内层 .card-drag-ghost-inner 做 scale/rotate，**必须有 transition**（抬起这一下要弹）
//
// FLIP：每次提交新顺序前先记下所有卡片的位置，React 渲染完在 useLayoutEffect 里
// 把位移反向贴回去再播动画 —— 于是被挤开的卡片是滑走的，不是跳过去的。
//
// 整个状态机只在挂载时建一次，window 上的四个监听器也只在挂载时挂一次，
// 可变状态一律走 ref —— 这样不存在"监听器身份变了、旧的那个没摘掉"的隐患。
import { useCallback, useEffect, useLayoutEffect, useRef, useState, type PointerEvent as ReactPointerEvent } from "react";
import { autoscrollDelta, decideLift, liftHoldMs, resolveDropSlot } from "./card-drag";
import { reorderIds } from "./project-order";

/** 卡片上用来认身份的属性：谁挂了它，谁就能被拖动。 */
export const CARD_DRAG_ATTRIBUTE = "data-card-id";
const CARD_SELECTOR = `[${CARD_DRAG_ATTRIBUTE}]`;
const INTERACTIVE_SELECTOR = "button, a, input, textarea, select, [contenteditable='true']";
/** 松手后让浮起的卡片「落座」；这段时间里紧随其后的那次 click 要被吃掉。 */
const SETTLE_MS = 200;
const SETTLE_EASING = "cubic-bezier(.2,.9,.25,1)";
const FLIP_MS = 200;
const FLIP_EASING = "cubic-bezier(.2,.85,.3,1)";
const CLICK_SUPPRESS_MS = 600;
const ORDER_KEY_SEPARATOR = "\u0000";

type Session = {
  pointerId: number;
  pointerType: string;
  cardEl: HTMLElement;
  cardId: string;
  startX: number;
  startY: number;
  startAt: number;
  lastX: number;
  lastY: number;
  moved: number;
  grabOffsetX: number;
  grabOffsetY: number;
  /** 抓起那一刻卡片的尺寸：浮起卡片要按它摆，否则会跟着内容自己伸缩。 */
  width: number;
  height: number;
  holdTimer: number;
  /** 已经真的浮起（浮起卡片出场、原位变占位槽）。 */
  live: boolean;
  /** 已经提交过的顺序 key，避免同一顺序被重复提交。 */
  committed: string;
  orderAtLift: string[];
  touchGuardAttached: boolean;
  /** 抓起那一刻探到的滚动容器：贴边自动滚动要滚的是它，不是想当然的 window。 */
  scroller: HTMLElement | null;
};

type Offset = { left: number; top: number };

export type CardDragOptions = {
  /** 少于两张卡片时没有排序可言，直接关掉。 */
  enabled: boolean;
  /** 当前渲染顺序（可见卡片的 id 按序拼接）：作为 FLIP 的触发键。 */
  orderKey: string;
  /** 提交可见卡片的新顺序（父级负责并回完整顺序并持久化）。 */
  commit: (ids: string[]) => void;
  /** 顺序真的变了之后回调（用来收起提示）。 */
  onReordered?: () => void;
};

function readOrder(grid: HTMLElement | null): string[] {
  if (!grid) return [];
  return Array.from(grid.querySelectorAll<HTMLElement>(CARD_SELECTOR))
    .map((element) => element.getAttribute(CARD_DRAG_ATTRIBUTE) ?? "")
    .filter(Boolean);
}

function readOffsets(grid: HTMLElement | null): Map<string, Offset> {
  const offsets = new Map<string, Offset>();
  grid?.querySelectorAll<HTMLElement>(CARD_SELECTOR).forEach((element) => {
    const id = element.getAttribute(CARD_DRAG_ATTRIBUTE) ?? "";
    if (!id) return;
    const rect = element.getBoundingClientRect();
    offsets.set(id, { left: rect.left, top: rect.top });
  });
  return offsets;
}

/**
 * 找最近的可滚动祖先（找不到就退回窗口的滚动元素）。
 * 手机端的「选择项目」整页由窗口滚，但保留这条探测：写死 window 会在"列表自带滚动容器"的
 * 场景里**静默失效**（指针贴边了却不滚，看着像自动滚动没实现，其实是滚错了对象）。
 */
function resolveScroller(element: HTMLElement | null): HTMLElement | null {
  let node = element?.parentElement ?? null;
  while (node && node !== document.body && node !== document.documentElement) {
    const overflowY = getComputedStyle(node).overflowY;
    if ((overflowY === "auto" || overflowY === "scroll") && node.scrollHeight > node.clientHeight + 1) return node;
    node = node.parentElement;
  }
  return (document.scrollingElement as HTMLElement | null) ?? null;
}

function prefersReducedMotion(): boolean {
  return typeof window.matchMedia === "function" && window.matchMedia("(prefers-reduced-motion: reduce)").matches;
}

export function useCardDragReorder(options: CardDragOptions) {
  const gridRef = useRef<HTMLDivElement | null>(null);
  const ghostRef = useRef<HTMLDivElement | null>(null);
  const [draggingId, setDraggingId] = useState<string | null>(null);
  const [lifted, setLifted] = useState(false);

  const sessionRef = useRef<Session | null>(null);
  const draggingIdRef = useRef<string | null>(null);
  const frameRef = useRef(0);
  /** 提交前记下的位置，供 FLIP 反算位移。 */
  const flipRef = useRef<Map<string, Offset> | null>(null);
  const flipAnimationsRef = useRef(new Map<string, Animation>());
  const suppressRef = useRef<{ el: HTMLElement | null; until: number }>({ el: null, until: 0 });
  // 落座收尾的令牌：settle 的 setTimeout 是异步的，而丢掉一张卡之后用户可能**立刻**抓起另一张。
  // 没有这个令牌，上一轮那次收尾会把新一轮的 draggingId 清成 null —— 表现是「卡片还在手上，
  // 屏幕上却什么都没有」，松手后又能正常落座（最难查的那种半坏）。每次 lift 自增即可作废旧的收尾。
  const settleTokenRef = useRef(0);
  // 监听器只挂一次，回调里一律走 ref 读最新值。
  const optionsRef = useRef(options);
  useEffect(() => { optionsRef.current = options; });

  // ---- 状态机：只在挂载时建一次 ----
  const beginRef = useRef<(event: ReactPointerEvent<HTMLDivElement>) => void>(() => undefined);
  useEffect(() => {
    const commit = (ids: string[]) => optionsRef.current.commit(ids);

    const stopFrame = () => {
      if (frameRef.current) {
        cancelAnimationFrame(frameRef.current);
        frameRef.current = 0;
      }
    };

    // 每帧：跟随指针 + 重算落点 + 贴边自动滚动。
    // 读取（量 rect）全部排在写入（改 transform / 滚动）之前，避免每帧强制同步布局。
    // 量 rect 是**每帧都做**、刻意不缓存的：自动滚动会在指针不动的情况下让卡片整体位移，
    // 缓存成「指针没动就不重量」会漏掉这一整类更新（贴边滚起来落点就再也不变了）。
    // 卡片是十几个的量级，一次 getBoundingClientRect 的代价远低于漏更新的代价。
    const tick = () => {
      frameRef.current = 0;
      const session = sessionRef.current;
      if (!session?.live) return;

      const grid = gridRef.current;
      if (grid) {
        const cards = Array.from(grid.querySelectorAll<HTMLElement>(CARD_SELECTOR));
        const ids = cards.map((element) => element.getAttribute(CARD_DRAG_ATTRIBUTE) ?? "");
        const rects = cards.map((element) => {
          const rect = element.getBoundingClientRect();
          return { left: rect.left, top: rect.top, width: rect.width, height: rect.height };
        });
        const slot = resolveDropSlot({ x: session.lastX, y: session.lastY }, rects);
        if (slot) {
          const next = reorderIds(ids, session.cardId, slot.index, slot.after);
          const nextKey = next.join(ORDER_KEY_SEPARATOR);
          if (nextKey !== ids.join(ORDER_KEY_SEPARATOR) && nextKey !== session.committed) {
            session.committed = nextKey;
            flipRef.current = new Map(cards.map((element, index) => [ids[index], { left: rects[index].left, top: rects[index].top }]));
            commit(next);
          }
        }
      }

      const ghost = ghostRef.current;
      if (ghost) {
        ghost.style.transform = `translate3d(${session.lastX - session.grabOffsetX}px, ${session.lastY - session.grabOffsetY}px, 0)`;
      }

      const scroller = session.scroller;
      const windowScroller = !scroller || scroller === document.scrollingElement;
      const delta = autoscrollDelta(session.lastY, windowScroller ? window.innerHeight : scroller.clientHeight);
      if (delta) {
        if (windowScroller) window.scrollBy(0, delta);
        else scroller.scrollTop += delta;
      }

      frameRef.current = requestAnimationFrame(tick);
    };

    const startFrame = () => {
      if (!frameRef.current) frameRef.current = requestAnimationFrame(tick);
    };

    const lift = (session: Session) => {
      if (session.live) return;
      session.live = true;
      session.committed = "";
      session.orderAtLift = readOrder(gridRef.current);
      session.scroller = resolveScroller(gridRef.current);
      // 作废上一轮还在飞的落座收尾（见 settleTokenRef 的说明）。
      settleTokenRef.current += 1;
      document.body.classList.add("card-drag-active");
      // 鼠标路径下浮起时往往已经选中了几个字，顺手清掉。
      window.getSelection()?.removeAllRanges();
      draggingIdRef.current = session.cardId;
      setDraggingId(session.cardId);
      startFrame();
    };

    const preventTouchScroll = (event: TouchEvent) => {
      if (sessionRef.current?.live) event.preventDefault();
    };

    const detach = (session: Session) => {
      if (session.holdTimer) window.clearTimeout(session.holdTimer);
      session.holdTimer = 0;
      if (session.touchGuardAttached) {
        session.cardEl.removeEventListener("touchmove", preventTouchScroll);
        session.touchGuardAttached = false;
      }
      document.body.classList.remove("card-drag-active");
      stopFrame();
    };

    const clearDragging = () => {
      draggingIdRef.current = null;
      setDraggingId(null);
    };

    /** 量槽位当前的位置（落座动画的终点）。抽成函数是为了让"在下一帧再量"这件事有唯一入口。 */
    const slotRect = (cardId: string): DOMRect | null =>
      gridRef.current
        ?.querySelector<HTMLElement>(`[${CARD_DRAG_ATTRIBUTE}="${CSS.escape(cardId)}"]`)
        ?.getBoundingClientRect() ?? null;

    /** 松手：让浮起的卡片滑回槽位，落座之后再把它从文档里摘掉。 */
    const settle = (session: Session) => {
      const ghost = ghostRef.current;
      const from = `translate3d(${session.lastX - session.grabOffsetX}px, ${session.lastY - session.grabOffsetY}px, 0)`;
      // 落座动画缺一环就直接收场：WAAPI 不可用时 animate 会抛，抛在 pointerup 里就等于
      // draggingId 永远清不掉（浮起卡片挂在屏幕上不走了）。宁可少一段动画，也不能卡住状态。
      if (!ghost || !slotRect(session.cardId) || prefersReducedMotion() || typeof ghost.animate !== "function") {
        setLifted(false);
        clearDragging();
        return;
      }
      // 帧循环已经停了（detach 里 stopFrame），把浮起卡片钉到指针最后的位置再去量终点：
      // 不补这一下，它会先回退到上一帧的位置再开始滑。
      ghost.style.transform = from;
      // 内层撤掉 is-lifted，靠 CSS 过渡把缩放/倾斜收回 1。
      setLifted(false);
      const token = settleTokenRef.current;
      // ⚠️ 终点**必须等下一帧再量**。松手的同一个事件里父级会顺手收起「按住卡片可拖动排序」
      // 提示，那次重排让整张列表原地往上挪一行（实测 25.4px）；当场量到的是重排前的坐标，
      // 卡片会停在槽位下方、摘掉浮起层时可见地跳一下。等一帧再量才是它真正要落的位置。
      requestAnimationFrame(() => {
        if (settleTokenRef.current !== token) return;
        const target = slotRect(session.cardId);
        const current = ghostRef.current;
        // 这一帧里被摘掉了（会话已取消 / 卡片被快照刷掉）：直接收场，别把浮起层留在屏幕上。
        if (!target || !current || typeof current.animate !== "function") {
          clearDragging();
          return;
        }
        current.animate(
          [{ transform: from }, { transform: `translate3d(${target.left}px, ${target.top}px, 0)` }],
          { duration: SETTLE_MS, easing: SETTLE_EASING, fill: "forwards" },
        );
        window.setTimeout(() => {
          if (settleTokenRef.current !== token) return;
          clearDragging();
        }, SETTLE_MS);
      });
    };

    const endSession = (session: Session, mode: "settle" | "cancel" | "tap") => {
      detach(session);
      if (sessionRef.current === session) sessionRef.current = null;
      if (mode === "tap") return;

      if (mode === "cancel") {
        // Esc / 手势被打断：顺序退回抓起前那一刻，浮起卡片直接消失（不做落座动画）。
        if (session.live && readOrder(gridRef.current).join(ORDER_KEY_SEPARATOR) !== session.orderAtLift.join(ORDER_KEY_SEPARATOR)) {
          flipRef.current = readOffsets(gridRef.current);
          commit(session.orderAtLift);
        }
        setLifted(false);
        clearDragging();
        return;
      }

      // 浮起过就吃掉紧随其后的那次 click：卡片的主操作是「点开项目」，
      // 拖完手一松却进了项目页，是最招人烦的一种错觉。
      suppressRef.current = { el: session.cardEl, until: performance.now() + CLICK_SUPPRESS_MS };
      if (session.live && readOrder(gridRef.current).join(ORDER_KEY_SEPARATOR) !== session.orderAtLift.join(ORDER_KEY_SEPARATOR)) {
        optionsRef.current.onReordered?.();
      }
      settle(session);
    };

    const onHoldElapsed = () => {
      const session = sessionRef.current;
      if (!session) return;
      session.holdTimer = 0;
      if (decideLift({ pointerType: session.pointerType, moved: session.moved, held: performance.now() - session.startAt }) === "lift") {
        lift(session);
      }
    };

    const onPointerMove = (event: PointerEvent) => {
      const session = sessionRef.current;
      if (!session || event.pointerId !== session.pointerId) return;
      session.lastX = event.clientX;
      session.lastY = event.clientY;
      session.moved = Math.max(session.moved, Math.hypot(event.clientX - session.startX, event.clientY - session.startY));
      if (!session.live) {
        const decision = decideLift({ pointerType: session.pointerType, moved: session.moved, held: performance.now() - session.startAt });
        if (decision === "abort") { endSession(session, "cancel"); return; }
        if (decision !== "lift") return;
        lift(session);
      }
      // 浮起之后不再让浏览器做滚动/选中（这个监听器是非 passive 的）。
      // 浮起之前不拦 —— 等着浮起的那段时间里，页面该滚就滚、该选中就选中。
      event.preventDefault();
    };

    const onPointerUp = (event: PointerEvent) => {
      const session = sessionRef.current;
      if (!session || event.pointerId !== session.pointerId) return;
      endSession(session, session.live ? "settle" : "tap");
    };

    const onPointerCancel = (event: PointerEvent) => {
      const session = sessionRef.current;
      if (!session || event.pointerId !== session.pointerId) return;
      endSession(session, "cancel");
    };

    const onKeyDown = (event: KeyboardEvent) => {
      const session = sessionRef.current;
      if (!session || event.key !== "Escape") return;
      event.preventDefault();
      endSession(session, "cancel");
    };

    // 长按期间浏览器可能弹自己的菜单（Android 的文本选择工具条就属于这一类）：一旦弹出来，
    // 即使它不 pointercancel 掉手势，用户也会被一个系统浮层挡住。只在「手上有活」时吃掉它。
    const onContextMenu = (event: MouseEvent) => {
      if (sessionRef.current) event.preventDefault();
    };

    beginRef.current = (event) => {
      if (!optionsRef.current.enabled || event.button !== 0) return;
      if (sessionRef.current) return;
      const target = event.target as HTMLElement | null;
      if (!target) return;
      const grid = gridRef.current;
      const cardEl = target.closest<HTMLElement>(CARD_SELECTOR);
      if (!grid || !cardEl || !grid.contains(cardEl)) return;
      // 卡片里面嵌着的控件（删除按钮之类）不该被当成「抓起卡片」。
      // 但要排除卡片自己：手机端的卡片本身就是一个 <button>（桌面端是 <article>），
      // 判据必须是「最近的那个可交互祖先不是卡片」才算真的嵌了控件 ——
      // 写成 `target.closest(INTERACTIVE_SELECTOR)` 会把手机端整张卡片一起挡掉（一次都拖不动）。
      const control = target.closest(INTERACTIVE_SELECTOR);
      if (control && control !== cardEl) return;
      const cardId = cardEl.getAttribute(CARD_DRAG_ATTRIBUTE);
      if (!cardId) return;

      const rect = cardEl.getBoundingClientRect();
      const session: Session = {
        pointerId: event.pointerId,
        pointerType: event.pointerType || "mouse",
        cardEl,
        cardId,
        startX: event.clientX,
        startY: event.clientY,
        startAt: performance.now(),
        lastX: event.clientX,
        lastY: event.clientY,
        moved: 0,
        grabOffsetX: event.clientX - rect.left,
        grabOffsetY: event.clientY - rect.top,
        width: rect.width,
        height: rect.height,
        holdTimer: 0,
        live: false,
        committed: "",
        orderAtLift: readOrder(grid),
        touchGuardAttached: false,
        scroller: null,
      };
      sessionRef.current = session;
      session.holdTimer = window.setTimeout(onHoldElapsed, liftHoldMs(session.pointerType));
      if (session.pointerType === "touch") {
        // pointermove 的 preventDefault 拦不住滚动，只有非 passive 的 touchmove 能。
        session.cardEl.addEventListener("touchmove", preventTouchScroll, { passive: false });
        session.touchGuardAttached = true;
      }
    };

    window.addEventListener("pointermove", onPointerMove, { passive: false });
    window.addEventListener("pointerup", onPointerUp);
    window.addEventListener("pointercancel", onPointerCancel);
    window.addEventListener("keydown", onKeyDown);
    window.addEventListener("contextmenu", onContextMenu);

    return () => {
      window.removeEventListener("pointermove", onPointerMove);
      window.removeEventListener("pointerup", onPointerUp);
      window.removeEventListener("pointercancel", onPointerCancel);
      window.removeEventListener("keydown", onKeyDown);
      window.removeEventListener("contextmenu", onContextMenu);
      const session = sessionRef.current;
      if (session) {
        if (session.holdTimer) window.clearTimeout(session.holdTimer);
        if (session.touchGuardAttached) session.cardEl.removeEventListener("touchmove", preventTouchScroll);
        sessionRef.current = null;
      }
      stopFrame();
      document.body.classList.remove("card-drag-active");
      flipAnimationsRef.current.forEach((animation) => animation.cancel());
      flipAnimationsRef.current.clear();
      beginRef.current = () => undefined;
    };
  }, []);

  const onPointerDown = useCallback((event: ReactPointerEvent<HTMLDivElement>) => beginRef.current(event), []);

  // ---- 浮起卡片出场：先摆到指针位置，下一帧再加 is-lifted 播抬起动画 ----
  useLayoutEffect(() => {
    if (!draggingId) { setLifted(false); return; }
    const session = sessionRef.current;
    const ghost = ghostRef.current;
    if (!session || !ghost) return;
    ghost.style.width = `${session.width}px`;
    ghost.style.height = `${session.height}px`;
    ghost.style.transform = `translate3d(${session.lastX - session.grabOffsetX}px, ${session.lastY - session.grabOffsetY}px, 0)`;
    // 只在会话还活着时补这一帧：极快的「抓起就松手」里 endSession 已经跑完并把 lifted 置回 false，
    // 这一帧要是照补，卡片会在落座动画里反向弹大一下。
    const handle = requestAnimationFrame(() => { if (sessionRef.current?.live) setLifted(true); });
    return () => cancelAnimationFrame(handle);
  }, [draggingId]);

  // ---- FLIP：把被挤开的卡片从旧位置滑到新位置 ----
  useLayoutEffect(() => {
    const before = flipRef.current;
    flipRef.current = null;
    if (!before) return;
    const grid = gridRef.current;
    // 同上：布局副作用里抛异常会炸掉整棵子树，缺 waapi 时退化成「直接跳位」就好。
    if (!grid || prefersReducedMotion() || typeof Element.prototype.animate !== "function") return;
    for (const element of grid.querySelectorAll<HTMLElement>(CARD_SELECTOR)) {
      const id = element.getAttribute(CARD_DRAG_ATTRIBUTE) ?? "";
      // 被拖走那张留在原地当占位槽：它就是"洞"，跟着滑没有意义，还得反着赔一段动画。
      if (!id || id === draggingIdRef.current) continue;
      const previous = before.get(id);
      if (!previous) continue;
      const rect = element.getBoundingClientRect();
      const dx = previous.left - rect.left;
      const dy = previous.top - rect.top;
      if (Math.abs(dx) < 0.5 && Math.abs(dy) < 0.5) continue;
      flipAnimationsRef.current.get(id)?.cancel();
      const animation = element.animate(
        [{ transform: `translate(${dx}px, ${dy}px)` }, { transform: "translate(0px, 0px)" }],
        { duration: FLIP_MS, easing: FLIP_EASING },
      );
      flipAnimationsRef.current.set(id, animation);
      animation.addEventListener("finish", () => {
        if (flipAnimationsRef.current.get(id) === animation) flipAnimationsRef.current.delete(id);
      });
    }
  }, [options.orderKey]);

  // ---- 吃掉「拖完那一下」的 click ----
  useEffect(() => {
    const onClick = (event: MouseEvent) => {
      const guard = suppressRef.current;
      if (!guard.el || performance.now() > guard.until) return;
      const target = event.target as Node | null;
      if (!target || !guard.el.contains(target)) return;
      suppressRef.current = { el: null, until: 0 };
      event.stopPropagation();
      event.preventDefault();
    };
    document.addEventListener("click", onClick, true);
    return () => document.removeEventListener("click", onClick, true);
  }, []);

  return { gridRef, ghostRef, draggingId, lifted, onPointerDown };
}
