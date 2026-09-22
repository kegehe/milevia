// 设置页「分页 + 卡片」结构的结构断言（挡变异；行为断言见 .tmp/probe-settings-tabs.mjs）。
//
// 为什么要成对：结构断言只能证明"文本还在"，挡不住"接线了但行为是错的"；
// 反过来行为探针不在 `node --test` 里跑、容易忘。两边都要有。
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const [page, css] = await Promise.all([
  readFile(new URL("./pages/SettingsPage.tsx", import.meta.url), "utf8"),
  readFile(new URL("./pages/settings.css", import.meta.url), "utf8"),
]);

test("设置页用分页（tablist）而不是常驻锚点导航", () => {
  assert.match(page, /className="settings-tabs"/);
  assert.match(page, /role="tablist"/);
  assert.match(page, /role="tab"/);
  assert.match(page, /role="tabpanel"/);
  // 旧的锚点导航必须真的没了，否则两套导航会并存
  assert.doesNotMatch(page, /className="settings-nav"/);
});

test("分页表是唯一的分组来源，且六个分组齐全", () => {
  // 顺序、标题、面板内容都从 TAB_ORDER / TAB_LABELS 取；这里钉住条目本身。
  assert.match(page, /const TAB_ORDER: TabId\[\] = \["general", "notifications", "security", "tasks", "data", "about"\]/);
  for (const id of ["general", "notifications", "security", "tasks", "data", "about"]) {
    assert.match(page, new RegExp(`activeTab === "${id}" &&`), `缺少 ${id} 面板的渲染分支`);
  }
});

test("分页状态以 hash 为唯一真相（刷新与前进后退都靠它）", () => {
  assert.match(page, /window\.history\.replaceState\(null, "", `#\$\{next\}`\)/);
  assert.match(page, /addEventListener\("hashchange", sync\)/);
  // 组件内不得另外维护一份与 hash 并行的分组 state 来源
  assert.match(page, /useState<TabId>\(readTabFromHash\)/);
});

test("分页键盘导航按 roving tabindex 接线", () => {
  assert.match(page, /onKeyDown=\{onTabKeyDown\}/);
  for (const key of ["ArrowRight", "ArrowLeft", "Home", "End"]) {
    assert.match(page, new RegExp(`event\\.key === "${key}"`), `缺少 ${key} 的处理`);
  }
  assert.match(page, /tabIndex=\{activeTab === id \? 0 : -1\}/);
});

test("设置项改用卡片，并保留危险变体", () => {
  assert.match(page, /function SettingCard\(/);
  assert.match(page, /className="settings-danger-group settings-grid-full"/);
  assert.match(css, /\.settings-card\s*\{/);
  assert.match(css, /\.settings-card\.danger\s*\{/);
});

test("分页与白板咬合：激活页背景必须等于白板背景", () => {
  // 这两条是"视觉上连成一体"的唯一判据，任一被改都会静默导致割裂感。
  const activeBg = css.match(/\.settings-tabs button\.active\s*\{[^}]*background:\s*([^;]+);/)?.[1]?.trim();
  const boardBg = css.match(/\.settings-board\s*\{[^}]*background:\s*([^;]+);/)?.[1]?.trim();
  assert.ok(activeBg, "找不到激活分页的背景色");
  assert.equal(activeBg, boardBg, `激活分页背景(${activeBg})与白板背景(${boardBg})必须一致`);
});

test("窄屏下选中的分页会被滚进可视区（溢出时它本来在视口外）", () => {
  // 380px 实测：分页栏 scrollWidth 408 > clientWidth 352，选「关于」时它会被裁掉。
  // 断言必须按位置/顺序钉：光断言"文本里有 scrollLeft"等于没断言。
  const effect = page.match(/useEffect\(\(\) => \{\s*const bar = tabsRef\.current;[\s\S]*?\}, \[activeTab\]\)/)?.[0];
  assert.ok(effect, "找不到随 activeTab 生效的分页栏滚动补偿 effect");
  // 两个边界分支都要在（只写左边界时，向右溢出仍会漏）
  assert.match(effect, /bar\.scrollLeft -= barBox\.left - itemBox\.left/);
  assert.match(effect, /bar\.scrollLeft \+= itemBox\.right - barBox\.right/);
  // 分页栏必须真的挂上了 ref，否则 effect 里的 tabsRef 永远是 null（静默失效）
  assert.match(page, /role="tablist"[^>]*ref=\{tabsRef\}/);
});

test("窄屏会折叠成单列（否则卡片会被压扁）", () => {
  assert.match(css, /@media \(max-width: 900px\)/);
  // 该断点内必须把两列改成单列
  const block = css.split("@media (max-width: 900px)")[1]?.split("}")[0] ?? "";
  assert.match(css.slice(css.indexOf("@media (max-width: 900px)"), css.indexOf("@media (max-width: 900px)") + 200),
    /grid-template-columns:\s*minmax\(0, 1fr\)/);
  assert.ok(block !== undefined);
});
