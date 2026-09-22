// 手机端「按住项目卡片 → 拖动排序」：只守「接线还在不在」与「规则顺序对不对」。
// 阈值与落点这类逻辑在 lib/card-drag.test.ts、lib/project-order.test.ts 里做行为断言 ——
// 用扫源码正则验优先级，改反了照样绿（见 TOOLING 里那条教训）。
//
// 同时守一条硬要求：**桌面端继续用原生 HTML5 拖放**，不许被无意中改成指针版。
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const mobile = await readFile(new URL("./pages/MobileRemotePage.tsx", import.meta.url), "utf8");
const mobileStyles = await readFile(new URL("./pages/mobile-remote.css", import.meta.url), "utf8");
const sharedStyles = await readFile(new URL("./style.css", import.meta.url), "utf8");
const hook = await readFile(new URL("./lib/use-card-drag.ts", import.meta.url), "utf8");
const dashboard = await readFile(new URL("./pages/DashboardPage.tsx", import.meta.url), "utf8");

/** 断言"不该出现"之前先剥注释：注释里原样写出的类名/属性名会把断言喂饱。 */
const stripComments = (text) => text.replace(/\/\*[\s\S]*?\*\//g, "").replace(/^\s*\/\/.*$/gm, "");
const mobileCode = stripComments(mobile);

test("桌面端保持原生 HTML5 拖放，没有被换成指针版", () => {
  assert.match(dashboard, /draggable onClick=\{open\}/);
  assert.match(dashboard, /onDragStart=\{\(event\) => onDragStart\(event, project\.id\)\}/);
  assert.match(dashboard, /moveProject\(orderedIds, fromId, id\)/);
  assert.doesNotMatch(stripComments(dashboard), /useCardDragReorder|data-card-id|card-drag-ghost|dashboard-drag-hint/);
});

test("手机端「选择项目」接线：容器挂 ref 与指针入口，卡片挂 data-card-id", () => {
  assert.match(mobile, /<section className="mobile-project-picker" ref=\{projectPickerRef\} onPointerDown=\{onProjectPointerDown\}>/);
  assert.match(mobile, /<MobileProjectChoice key=\{item\.id\} item=\{item\} busy=\{busy\} isDragSlot=\{draggingProjectId === item\.id\} open=\{openMobileProject\} \/>/);
  assert.match(mobile, /data-card-id=\{ghost \? undefined : item\.id\}/);
});

test("列表真的按本机顺序渲染，而且用的是手机端那把钥匙", () => {
  // 顺序必须在渲染处生效，只写个 memo 不接上去等于没做。
  assert.match(mobile, /orderedProjects\.map\(\(item\) =>/);
  assert.match(mobile, /sortProjectIds\(projects\.map\(\(project\) => project\.id\), MOBILE_PROJECT_ORDER_STORAGE_KEY\)/);
  assert.match(mobile, /persistOrder\(ids, MOBILE_PROJECT_ORDER_STORAGE_KEY\)/);
  // 桌面端那把钥匙是 moveProject 写的，手机端不许碰它。
  assert.doesNotMatch(mobileCode, /persistOrder\(ids\)/);
});

test("手机端卡片不使用原生 draggable（会和手指滚动抢手势）", () => {
  assert.doesNotMatch(mobileCode, /draggable/);
});

test("浮起卡片是两层结构，且渲染在列表容器之外", () => {
  assert.match(mobile, /className=\{`card-drag-ghost\$\{projectLifted \? " is-lifted" : ""\}`\} ref=\{projectDragGhostRef\} aria-hidden="true"><div className="card-drag-ghost-inner">/);
  // 必须挂在 </section> 之后：留在容器里就会被当成一张"卡片"量进落点几何。
  assert.match(mobile, /<\/section>\{draggingProject && <div className=\{`card-drag-ghost/);
});

test("浮起的那张卡片必须不可交互：它一直压在手指底下", () => {
  assert.match(mobile, /tabIndex=\{ghost \? -1 : undefined\}/);
  assert.match(mobile, /aria-hidden=\{ghost \|\| undefined\}/);
  assert.match(mobile, /onClick=\{ghost \? undefined : \(\) => void open\(item\)\}/);
  // 浮起卡片不能被 disabled —— 那会掉到 opacity .6，看起来像"拖到一半变灰了"。
  assert.match(mobile, /disabled=\{ghost \? undefined : busy\}/);
});

test("共享浮起层：外层不能有过渡、内层必须有", () => {
  assert.match(sharedStyles, /\.card-drag-ghost\s*\{[^}]*position:\s*fixed;[^}]*pointer-events:\s*none;/s);
  // 外层带 transition 会让卡片拖在手指后面（每帧都在补上一帧的位置）。
  assert.doesNotMatch(sharedStyles, /\.card-drag-ghost\s*\{[^}]*transition:/s);
  assert.match(sharedStyles, /\.card-drag-ghost-inner\s*\{[^}]*transition:\s*transform/s);
  assert.match(sharedStyles, /\.card-drag-ghost\.is-lifted\s+\.card-drag-ghost-inner\s*\{[^}]*transform:\s*scale\(1\.035\)\s*rotate\(/s);
});

test("手机端占位槽用 visibility 隐藏子节点，且压掉悬停与按下态", () => {
  assert.match(mobileStyles, /\.mobile-project-choice\.is-drag-slot\s*>\s*\*\s*\{\s*visibility:\s*hidden;\s*\}/);
  assert.doesNotMatch(mobileStyles, /\.mobile-project-choice\.is-drag-slot\s*>\s*\*\s*\{\s*display:\s*none/);
  // 悬停/按下那两条是 (0,3,0)，必须被同特异性的规则显式压掉，不能指望"颜色刚好一样"。
  assert.match(mobileStyles, /\.mobile-project-choice\.is-drag-slot:hover[^{]*\{[^}]*box-shadow:\s*none;/s);
  assert.match(mobileStyles, /\.mobile-project-choice\.is-drag-slot:active[^{]*\{[^}]*box-shadow:\s*none;/s);
});

test("占位槽规则必须排在卡片的 :hover / :active 之后（同特异性，靠源码顺序取胜）", () => {
  assert.equal(mobileStyles.split(".mobile-project-choice:not(:disabled):hover").length - 1, 1);
  const hover = mobileStyles.indexOf(".mobile-project-choice:not(:disabled):hover");
  const active = mobileStyles.indexOf(".mobile-project-choice:not(:disabled):active");
  const slot = mobileStyles.indexOf(".mobile-project-choice.is-drag-slot,");
  assert.ok(hover > 0 && active > 0 && slot > 0);
  assert.ok(slot > hover, "占位槽规则被挪到悬停之前：手指一停就把虚线槽盖成实心卡片");
  assert.ok(slot > active, "占位槽规则被挪到按下之前：按着的那张会掉回卡片外观");
});

test("浮起卡片在手机端有一套自己的观感（虚线环 + 比悬停重的阴影）", () => {
  assert.match(mobileStyles, /\.card-drag-ghost\.is-lifted\s+\.mobile-project-choice\s*\{[^}]*outline:\s*1\.5px dashed/s);
  assert.match(mobileStyles, /\.card-drag-ghost\.is-lifted\s+\.mobile-project-choice\s*\{[^}]*box-shadow:/s);
});

test("拖动期间整页锁住选中，否则会一路刷出蓝色选区", () => {
  assert.match(sharedStyles, /body\.card-drag-active\s*\{[^}]*user-select:\s*none;/s);
  assert.match(hook, /document\.body\.classList\.add\("card-drag-active"\)/);
  assert.match(hook, /document\.body\.classList\.remove\("card-drag-active"\)/);
});

test("FLIP：被拖走的那张不参与，邻居才从旧位置滑过去", () => {
  assert.match(hook, /if \(!id \|\| id === draggingIdRef\.current\) continue;/);
  assert.match(hook, /element\.animate\(\s*\[\{ transform: `translate\(\$\{dx\}px, \$\{dy\}px\)` \}, \{ transform: "translate\(0px, 0px\)" \}\]/);
  assert.match(hook, /useLayoutEffect\(\(\) => \{/, "FLIP 必须在 paint 之前跑，否则会先闪一下旧位置");
});

test("松手后要吃掉紧随其后的 click：卡片的主操作是进项目", () => {
  assert.match(hook, /document\.addEventListener\("click", onClick, true\)/);
  assert.match(hook, /event\.stopPropagation\(\);\s*event\.preventDefault\(\);/);
  assert.match(hook, /suppressRef\.current = \{ el: session\.cardEl, until: performance\.now\(\) \+ CLICK_SUPPRESS_MS \}/);
});

test("Esc 取消要把顺序退回抓起前那一刻", () => {
  assert.match(hook, /event\.key !== "Escape"/);
  assert.match(hook, /commit\(session\.orderAtLift\)/);
});

test("手机端卡片本身就是 <button>：不能因为「卡片可交互」就拒绝抓起", () => {
  // 实测踩过：写成 `if (target.closest(INTERACTIVE_SELECTOR)) return;` 时，
  // 桌面端没事（卡片是 <article>，里面那个全屏按钮是 pointer-events:none），
  // 手机端**一次都拖不动** —— 卡片自己就是按钮，closest 命中的正是它。
  assert.match(hook, /if \(control && control !== cardEl\) return;/);
  assert.doesNotMatch(hook, /if \(target\.closest\(INTERACTIVE_SELECTOR\)\) return;/);
});

test("落座收尾必须带令牌校验：丢掉一张卡后 200ms 内再抓起另一张，新卡不能凭空消失", () => {
  // settle 的收尾是 setTimeout，而"丢掉一张再抓起另一张"可以发生在那 200ms 之内。
  // 没有令牌，上一轮的收尾会把新一轮的 draggingId 清成 null：卡片还在手上，屏幕上什么都没有。
  assert.match(hook, /const settleTokenRef = useRef\(0\);/);
  assert.match(hook, /settleTokenRef\.current \+= 1;/);
  assert.match(hook, /const token = settleTokenRef\.current;/);
  // 两处都要有：一处守「这一帧是不是已经过期」，一处守「落座收尾是不是已经过期」。
  // 只留一处等于另一条路径没有护栏，所以这里按条数断言，不是按"出现过"。
  assert.equal(
    hook.split("if (settleTokenRef.current !== token) return;").length - 1,
    2,
    "令牌校验必须同时守在 rAF 回调与落座收尾上",
  );
});

test("落座终点必须等下一帧再量：父级会在松手这一下收起提示、整列上移一行", () => {
  // 实测踩过：松手的同一个事件里父级会把「按住卡片可拖动排序」那行提示摘掉，
  // 整张列表原地往上挪 25.4px。当场量到的终点是重排前的坐标 —— 卡片停在槽位下方，
  // 摘掉浮起层时肉眼可见地跳一下。判据是"测量语句在结构上晚于 rAF 入口"。
  const tokenAt = hook.indexOf("const token = settleTokenRef.current;");
  const rafAt = hook.indexOf("requestAnimationFrame(() => {", tokenAt);
  const measureAt = hook.indexOf("const target = slotRect(session.cardId);");
  assert.ok(tokenAt > 0, "找不到落座动画的令牌入口");
  assert.ok(rafAt > tokenAt, "终点测量必须包在 requestAnimationFrame 回调里（等父级重排落定）");
  assert.ok(measureAt > rafAt, "终点是在 rAF 里量的，不是松手当场量的");
  assert.doesNotMatch(hook, /const target = slot\.getBoundingClientRect\(\);/, "不许退回「当场量槽位」的写法");
});

test("WAAPI 不可用时不许抛：抛在 pointerup 里等于拖动状态永远清不掉", () => {
  assert.match(hook, /typeof ghost\.animate !== "function"/);
  assert.match(hook, /typeof Element\.prototype\.animate !== "function"/);
});

test("浮起那一帧要确认会话还活着（抓起就松手时不许把 lifted 又补成 true）", () => {
  assert.match(hook, /requestAnimationFrame\(\(\) => \{ if \(sessionRef\.current\?\.live\) setLifted\(true\); \}\)/);
});

test("长按期间要吃掉浏览器自己的菜单（系统浮层会掐断手势）", () => {
  assert.match(hook, /if \(sessionRef\.current\) event\.preventDefault\(\);/);
  assert.match(hook, /window\.addEventListener\("contextmenu", onContextMenu\)/);
  assert.match(hook, /window\.removeEventListener\("contextmenu", onContextMenu\)/);
});

test("手机端卡片关掉浏览器自己的长按能力（文本选择与放大镜会打断手势判定）", () => {
  assert.match(mobileStyles, /\.mobile-project-choice\s*\{\s*user-select:\s*none;\s*-webkit-user-select:\s*none;\s*-webkit-touch-callout:\s*none;\s*\}/);
});

test("指针事件走非 passive：浮起后要能拦住滚动与选中", () => {
  assert.match(hook, /addEventListener\("pointermove", onPointerMove, \{ passive: false \}\)/);
  // pointermove 的 preventDefault 拦不住滚动，触摸端得靠非 passive 的 touchmove。
  assert.match(hook, /addEventListener\("touchmove", preventTouchScroll, \{ passive: false \}\)/);
});

test("自动滚动滚的是**探到的那个滚动容器**，不是想当然的 window", () => {
  assert.match(hook, /function resolveScroller\(element: HTMLElement \| null\): HTMLElement \| null \{/);
  assert.match(hook, /session\.scroller = resolveScroller\(gridRef\.current\)/);
  assert.match(hook, /else scroller\.scrollTop \+= delta;/);
  assert.match(hook, /const delta = autoscrollDelta\(session\.lastY, windowScroller \? window\.innerHeight : scroller\.clientHeight\);/);
});

test("减弱动效偏好下不播浮起过渡", () => {
  assert.match(sharedStyles, /@media \(prefers-reduced-motion: reduce\)\s*\{\s*\.card-drag-ghost-inner\s*\{\s*transition:\s*none;\s*\}\s*\}/);
  assert.match(hook, /prefers-reduced-motion: reduce/);
});
