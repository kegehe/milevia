// 「通知」分组按环境只显示能用得上的开关 —— 结构断言。
//
// 为什么必须钉住：桌面端（Tauri/WebView2）与原生包（Capacitor）里 Web Notification 是死路
// （WebView2 把授权与渲染都交给宿主，wry 两者都没实现），而 Windows 弹窗走 Rust winrt Toast、
// 调用点被 isDesktop() 挡着 —— 两条路各有一半环境是"看着能点、其实永远无效"的。
// 2026-09-22 那次排查就是因为设置页把死开关照常摆出来、点了只报"系统通知已被拒绝"，
// 而那个 denied 存在应用自己的 EBWebView profile 里，用户在界面上根本没有地方改。
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const [page, store, lib] = await Promise.all([
  readFile(new URL("./pages/SettingsPage.tsx", import.meta.url), "utf8"),
  readFile(new URL("./stores/useUIPreferences.tsx", import.meta.url), "utf8"),
  readFile(new URL("./lib/notifications.ts", import.meta.url), "utf8"),
]);

test("设置页按能力收起两条 Web Notification 开关", () => {
  assert.ok(page.includes('const systemNotificationsAvailable = notificationPermission !== "unsupported";'),
    "以 store 报出的 unsupported 作为唯一收口信号");
  const gate = page.indexOf("{systemNotificationsAvailable && <>");
  const windowsCard = page.indexOf('title="Windows 弹窗通知"');
  assert.ok(gate >= 0, "存在按能力渲染的闸门");
  for (const title of ['title="使用系统通知"', 'title="应用在后台时通知"']) {
    const at = page.indexOf(title);
    assert.ok(at > gate && at < windowsCard, `${title} 必须落在闸门内、Windows 弹窗卡片之前`);
  }
});

test("Windows 弹窗通知只在桌面端出现", () => {
  assert.ok(page.includes('{isDesktop() && <SettingCard wide title="Windows 弹窗通知"'),
    "Web 端勾了不会有任何效果（调用点被 isDesktop() 挡着），不能摆在页面上");
});

test("不支持的环境不再出现 disabled 死分支与 WebView 文案", () => {
  // 卡片既然整块收起，就不该再留"按 unsupported 置灰"或描述里的 unsupported 分支这种永远不成立的防御。
  assert.ok(!page.includes('disabled={notificationPermission === "unsupported"}'), "残留的 disabled 守卫");
  assert.ok(!page.includes('notificationPermission === "unsupported" ?'), "描述三元里残留的 unsupported 分支");
  // 桌面端无处可改的 denied 不该再被当成用户的设置问题报出来。
  assert.ok(!page.includes("请在系统或 WebView 设置中允许"), "报错文案仍指向 WebView 设置");
  assert.ok(page.includes("请在浏览器或系统的通知设置里允许"), "被拒时的文案改为面向浏览器与系统设置");
});

test("权限状态以环境判定为唯一真相，桌面/原生直接报 unsupported", () => {
  assert.ok(store.includes("if (!webNotificationsAvailable()) return \"unsupported\";"),
    "初值判定必须走环境闸门，否则桌面端读到的是 WebView2 持久化的 denied");
  assert.ok(store.includes("if (!webNotificationsAvailable()) return \"unsupported\" as const;"),
    "请求入口同样必须走闸门（原来的写法连 requestPermission() 都不会调用）");
  assert.ok(lib.includes("export function webNotificationsSupported("), "判定收敛在 lib/notifications.ts 一处");
});
