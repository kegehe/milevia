import assert from "node:assert/strict";
import test from "node:test";

import { decideExternalLinkClick, isExternalHref } from "./external-link";

/**
 * 外链点击判据的**逐格**表。
 *
 * 这张表的每一格都对应一个用户看得见的后果，而且都是 2026-09-29 实测出来的
 * （`apps/web/.tmp/link-click/`，真组件 + 真 Chromium，判据 = `invoke("open_external")`
 * 有没有被调用）：桌面端"点下去毫无反应"、Web 端被抢掉后台标签页，两种都真发生过。
 * 判据抽成纯函数就是为了能在这里逐格钉住 —— 塞在 JSX 里只能靠肉眼看。
 */

const LEFT = 0;
const MIDDLE = 1;
const RIGHT = 2;

/** 只关心动作时用它；关心改道后的地址时用下面的 `openURL`。 */
const action = (href: string | undefined, context: Parameters<typeof decideExternalLinkClick>[1]) =>
  decideExternalLinkClick(href, context).action;

/** 断言这一格一定是"改道给宿主"，并回传改道后的地址。 */
function openURL(href: string, context: Parameters<typeof decideExternalLinkClick>[1]): string {
  const decision = decideExternalLinkClick(href, context);
  assert.equal(decision.action, "open-external", `${href} 没有被改道（桌面端 = 点了没反应）`);
  return decision.action === "open-external" ? decision.url : "";
}

const DESKTOP = { desktop: true, button: LEFT };
const WEB = { desktop: false, button: LEFT };

test("桌面端：左键 http(s) 改道给宿主", () => {
  assert.equal(openURL("https://example.com/a?b=1", DESKTOP), "https://example.com/a?b=1");
  assert.equal(openURL("http://example.com/", DESKTOP), "http://example.com/");
  // 协议名大小写不敏感（两个正则都带 i；`new URL` 与 Rust 的 `Url::parse` 也都会把 scheme
  // 规范成小写，所以大写形态一路都能过）。
  assert.equal(openURL("HTTPS://Example.com/A", DESKTOP), "HTTPS://Example.com/A");
  assert.equal(openURL("MAILTO:Foo@Example.com", DESKTOP), "MAILTO:Foo@Example.com");
});

test("桌面端：修饰键也照改 —— click 照常触发，preventDefault 能压掉「开新标签页」", () => {
  // 这一格曾经被我误判成"死链"：实测 ctrl/meta/shift + 左键时 `click` 事件照常触发，
  // 且 preventDefault() 能压掉 Chromium 开新标签页的默认动作，所以走的是 openExternal。
  for (const key of ["ctrl", "meta", "shift", "alt"] as const) {
    assert.equal(
      action("https://example.com/", { desktop: true, button: LEFT, modifiers: { [key]: true } }),
      "open-external",
      `桌面端 ${key}+左键没有改道`,
    );
  }
});

test("桌面端：中键改道 —— 中键只发 auxclick，不改道就是死链", () => {
  // 默认动作开的新窗口会被外壳 `NewWindowResponse::Deny` 吃掉，且不报错。
  assert.equal(openURL("https://example.com/", { desktop: true, button: MIDDLE }), "https://example.com/");
});

test("桌面端：mailto: 改道（GFM 自动链接出来的裸邮箱就落在这）", () => {
  assert.equal(openURL("mailto:foo@example.com", DESKTOP), "mailto:foo@example.com");
});

test("桌面端：协议相对地址先归一成 https 绝对地址再改道", () => {
  // 不归一就会栽在 SDK 的 `new URL("//host")` 上：那里解析失败后静默 return，
  // 于是"接管"反而比不接管更糟（Web 端原本靠浏览器自己解析，是能用的）。
  assert.equal(openURL("//example.com/pr", DESKTOP), "https://example.com/pr");
  assert.equal(openURL("//example.com", DESKTOP), "https://example.com");
  // `///` 不是协议相对地址，别乱归一。
  assert.equal(action("///example.com", DESKTOP), "native");
});

test("桌面端：其余协议一律不碰（改道只会把它们弄成死链，还可能把 file:/javascript: 交出去）", () => {
  for (const href of ["ftp://example.com/f", "file:///C:/Windows/win.ini", "javascript:alert(1)", "tel:+8613800000000", "不是链接", ""]) {
    assert.equal(action(href, DESKTOP), "native", `桌面端不该改道 ${href}`);
  }
  assert.equal(action(undefined, DESKTOP), "native", "没有 href 时不该改道");
});

test("Web/手机端：普通左键 http(s) 改道（与桌面端一致），带修饰键则还给浏览器", () => {
  assert.equal(action("https://example.com/", WEB), "open-external");
  // 拦下来会把浏览器原生的**后台标签页**换成 window.open 的**前台标签页** —— 这是改道带来的
  // 回归，所以带修饰键的一律不拦（桌面端相反：那边不拦才是死链）。
  for (const key of ["ctrl", "meta", "shift", "alt"] as const) {
    assert.equal(
      action("https://example.com/", { desktop: false, button: LEFT, modifiers: { [key]: true } }),
      "native",
      `Web 端 ${key}+左键被抢了`,
    );
  }
});

test("Web/手机端：中键还给浏览器（原生后台标签页），mailto: 与协议相对也不拦", () => {
  assert.equal(action("https://example.com/", { desktop: false, button: MIDDLE }), "native");
  // mailto: 在浏览器里本来就是原生行为；协议相对地址浏览器自己会按当前协议解析。
  assert.equal(action("mailto:foo@example.com", WEB), "native");
  assert.equal(action("//example.com/pr", WEB), "native");
});

test("右键与其它按键永远不碰", () => {
  for (const desktop of [true, false]) {
    assert.equal(action("https://example.com/", { desktop, button: RIGHT }), "native");
    assert.equal(action("https://example.com/", { desktop, button: 3 }), "native");
  }
});

test("解析不了的地址不算外链：判 native 交给浏览器（实测它本来就什么都不做）", () => {
  // 判据最后一道是**真解析 + 共享白名单**（`externalLinkProtocols` 与 openExternal 同一份），
  // 所以"判据放行 ⇒ SDK 一定会接受"是结构性事实。这几个畸形地址解析必然失败：
  for (const href of ["https://", "//example.com:99999/x"]) {
    assert.equal(action(href, DESKTOP), "native", `桌面端不该改道 ${href}`);
    assert.equal(action(href, WEB), "native", `Web 端不该改道 ${href}`);
  }
});

test("isExternalHref：组件据此决定补不补 target（分平台）", () => {
  // 补 target 是安全相关：Web 端会把"带修饰键的点击"交还浏览器，只有带 target 的锚点
  // 才是"开新标签页"；无 target 的锚点 + Win 键(Meta) 会**同窗导航**把应用顶掉（实测）。
  assert.equal(isExternalHref("https://example.com/", true), true);
  assert.equal(isExternalHref("https://example.com/", false), true);
  assert.equal(isExternalHref("mailto:foo@example.com", true), true);
  // Web 端不接管 mailto:/协议相对（浏览器原生就能处理），所以也不需要补 target。
  assert.equal(isExternalHref("mailto:foo@example.com", false), false);
  assert.equal(isExternalHref("//example.com/pr", true), true);
  assert.equal(isExternalHref("//example.com/pr", false), false);
  for (const href of ["/projects/1", "#anchor", "file:///C:/x", "javascript:void(0)", "https://", undefined, ""]) {
    assert.equal(isExternalHref(href, true), false, `${JSON.stringify(href)} 不该被当成外链`);
  }
});
