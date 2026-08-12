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
