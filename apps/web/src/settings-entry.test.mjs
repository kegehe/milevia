import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const [appSource, dashboardSource] = await Promise.all([
  readFile(new URL("./App.tsx", import.meta.url), "utf8"),
  readFile(new URL("./pages/DashboardPage.tsx", import.meta.url), "utf8"),
]);

test("设置页已注册到应用路由", () => {
  assert.match(appSource, /import SettingsPage from "\.\/pages\/SettingsPage"/);
  assert.match(appSource, /<Route path="\/settings" element=\{<SettingsPage\s*\/>\}\s*\/>/);
});

test("首页提供可访问的设置入口", () => {
  assert.match(dashboardSource, /title="设置"/);
  assert.match(dashboardSource, /aria-label="设置"/);
  assert.match(dashboardSource, /navigate\("\/settings"\)/);
});
