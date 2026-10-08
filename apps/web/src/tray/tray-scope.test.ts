import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

// 回归防护：托盘窗口与主窗口共用同一份打包产物（main.tsx 静态 import TrayPanel），
// tray-panel.css 里针对 html/body/#root 的 overflow:hidden 若不限定作用域，
// 会把主窗口的页面滚动锁死（项目总览多项目时看不到下方卡片）。见 git 记录。

test("tray-panel.css 的全局根元素规则必须限定在 tray-window 作用域", () => {
  const css = readFileSync(new URL("./tray-panel.css", import.meta.url), "utf8");

  // overflow:hidden / 透明背景必须挂在 .tray-window 类下，而不是裸的 html, body, #root。
  assert.match(
    css,
    /html\.tray-window,\s*html\.tray-window body,\s*html\.tray-window #root\s*\{[\s\S]*?overflow:\s*hidden/,
    "根元素 overflow:hidden 必须被 .tray-window 限定",
  );
  assert.doesNotMatch(
    css,
    /(^|})\s*html,\s*body,\s*#root\s*\{/,
    "不允许出现未限定的 html, body, #root 规则（会把主窗口滚动锁死）",
  );
  assert.doesNotMatch(
    css,
    /(^|})\s*body\s*\{\s*padding:\s*0/,
    "body 的 padding 重置同样要限定作用域",
  );
});

test("TrayPanel 挂载时添加、卸载时移除 tray-window 标记", () => {
  const source = readFileSync(new URL("./TrayPanel.tsx", import.meta.url), "utf8");

  assert.match(source, /classList\.add\(["']tray-window["']\)/);
  assert.match(source, /classList\.remove\(["']tray-window["']\)/);
  // 必须用 useLayoutEffect：标记要在首帧绘制前生效，窗口即便立即 show 也不闪实底。
  assert.match(source, /useLayoutEffect\([\s\S]*classList\.add\(["']tray-window["']\)/);
});

test("TrayPanel 不设右上角 X 关闭按钮，Esc 仍可关闭", () => {
  const source = readFileSync(new URL("./TrayPanel.tsx", import.meta.url), "utf8");

  // 点击面板外部会失焦自动隐藏（Rust 侧 WindowEvent::Focused(false)→hide），
  // 右上角的 X 属于冗余控件，不应存在。
  assert.doesNotMatch(source, /tray-panel-close/);
  assert.doesNotMatch(source, /title="关闭面板"/);
  assert.doesNotMatch(source, /aria-label="关闭面板"/);
  // 面板内保留 Esc 关闭作为键盘可达路径（展开「更多会话」时 Esc 先收起子菜单，再关面板）。
  // 按**处理器自身**取片段来断言，别用"从 return 往下数 N 个字"——注释一变长就会假红。
  const onEscape = source.match(/const onKey = \(e: KeyboardEvent\) => \{([\s\S]*?)\n    \};/);
  assert.ok(onEscape, "找不到 Esc 键盘处理器");
  assert.match(onEscape[1], /close\?\.\(\)/, "Esc 仍须能关掉面板");
  assert.match(onEscape[1], /setShowMore\(false\)/, "子菜单展开时 Esc 应先收起它");
});

// 回归防护：「更多会话…」曾经只有 onMouseEnter 一条展开路径 —— 键盘、触屏都打不开，
// 读屏也不知道折叠区里还有内容。见 issue「托盘面板的『更多会话』只能用鼠标悬停展开」。

test("「更多会话…」必须能用键盘/触屏打开，且向读屏暴露展开态", () => {
  const source = readFileSync(new URL("./TrayPanel.tsx", import.meta.url), "utf8");

  // 点击即切换展开态（<button> 上的 click 覆盖键盘 Enter/Space 与触屏点按两条路径）。
  assert.match(source, /onClick=\{toggleMore\}/, "缺少点击切换：键盘与触屏都打不开");
  assert.match(source, /setShowMore\(\(value\) =>/, "切换要用函数式更新，别读可能过期的 showMore");
  // 鼠标点击与键盘/触屏点击要区别对待：鼠标已经在行上、hover 早展开了，再点一下不该收起来。
  // 这个判据必须默认 false —— 读屏/AT 合成的 click 不带指针事件，默认 true 会把它们误判成鼠标点击。
  assert.match(source, /const hoverPointerRef = useRef\(false\)/, "hover 判据的初值必须是 false");
  assert.match(source, /event\.detail > 0 && hoverPointerRef\.current/);
  // 展开态要能被读屏念出来，并指向被展开的内容。
  assert.match(source, /aria-expanded=\{showMore\}/);
  assert.match(source, /aria-controls=\{MORE_SUBMENU_ID\}/);
  assert.match(source, /id=\{MORE_SUBMENU_ID\}/);
  // hover 展开必须带指针类型判定：触屏点按会补发 pointerenter，否则"展开又立刻收起"。
  assert.match(source, /onPointerEnter=\{openMoreOnHover\}/);
  assert.match(source, /pointerType === "mouse"/);
  assert.doesNotMatch(source, /onMouseEnter=\{openMore\}/, "不应再只靠 onMouseEnter 展开");
});

test("收起「更多会话…」时窗口尺寸回滚，且左侧面板不被重贴推走", () => {
  const source = readFileSync(new URL("./TrayPanel.tsx", import.meta.url), "utf8");

  // 展开时若曾向上让位（resizeUp），收起必须把窗口缩回去，否则一级面板悬在鼠标点上方。
  // 左侧面板靠 resizeExpand 整体左移让出子菜单区，重贴（relocate）会把它推右一截，
  // 所以重贴只允许发生在右侧。
  // ⚠️ 这是**结构守卫**：实测当前内容高度下让位分支打不到（子菜单需 410px、面板已有 429px），
  // 断言的是"将来它真触发时行为正确"，不是当下跑得到的路径。
  assert.match(
    source,
    /!showMore && growShiftedRef\.current && panelSide === "right" && r\.resize/,
    "收起时的窗口回滚缺少 panelSide 保护",
  );
  // remeasure 依赖 panelSide，否则收起时读到的还是打开时的旧方向。
  assert.match(source, /\[showMore, submenuReserve, panelSide\]/);
});

test("窗口宽度一律按 clientWidth 量，浮层溢出不许进宽度公式", () => {
  const raw = readFileSync(new URL("./TrayPanel.tsx", import.meta.url), "utf8");
  // 注释里会讲到 scrollWidth（正是为了解释为什么不能用），所以判据只看代码。
  const code = raw.replace(/\/\/[^\n]*/g, "");

  // 子菜单是 absolute 浮层，展开时它的溢出**会**被算进 .tray-panel 的 scrollWidth
  // （实测 169 → 326）。拿它当宽度基数再叠加预留宽就是双重计入 —— 展开态会请求一个
  // 宽出一整截的窗口（实测 433 → 590），多出来的透明窗口会吞掉点击，还会随展开/收起来回变
  // （hover 阶段宽度必须稳定）。clientWidth 不受浮层溢出影响，展开/收起两态完全一致。
  assert.doesNotMatch(code, /scrollWidth/, "窗口尺寸不许用 scrollWidth 量（会把浮层溢出算进来）");
  assert.equal(
    (code.match(/el\.clientWidth/g) ?? []).length,
    3,
    "预布局 / remeasure / layFinalSide 三处都要用 clientWidth 量一级宽",
  );
});

test("没有预留宽时不许做左侧布局（--left 的 margin 会把面板推出窗口）", () => {
  const source = readFileSync(new URL("./TrayPanel.tsx", import.meta.url), "utf8");

  // 会话 ≤3 条时预留宽为 0（没有子菜单可放），此时判成 left 会给一级加 .tray-root--left 的
  // margin-left:264px，而这条路径不会调 resizeExpand 让位（窗口宽度也没含预留宽）→
  // 面板从 264px 处开始画，整个落到 ~171px 宽的窗口之外被裁掉，用户看到"点了托盘什么都没出现"。
  // 真实可达：托盘图标在屏幕右侧（右侧放不下子菜单）+ 最近会话 ≤3 条。
  assert.match(
    source,
    /const side = extra > 0 \? decideSide\(\) : "right";/,
    "方向只在有预留宽时才允许判成 left",
  );
});
