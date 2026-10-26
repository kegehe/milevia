import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

// 项目总览首屏三态回归。
//
// 问题：projects 初始是空数组，列表请求还在飞、或失败时，页面会直接渲染
// 「还没有项目」+「加载项目」按钮。用户会以为项目丢了，甚至在失败时被引导
// 去重复添加。现在 loading / failed / empty 必须分开，且空态只能出现在
// "已经成功拉到一份空列表"之后。

const [dashboard, store, styles] = await Promise.all([
  readFile(new URL("./pages/DashboardPage.tsx", import.meta.url), "utf8"),
  readFile(new URL("./stores/useProjectStore.tsx", import.meta.url), "utf8"),
  readFile(new URL("./pages/dashboard.css", import.meta.url), "utf8"),
]);

test("store 暴露列表首屏三态，并在成功/失败时各自落地", () => {
  // 三个状态缺一不可：只有 loading 无法区分"确定为空"，只有 loaded 无法区分失败。
  assert.match(store, /projectsLoading: boolean;/);
  assert.match(store, /projectsLoaded: boolean;/);
  assert.match(store, /projectsLoadError: string;/);
  // 首屏请求期间标记在飞（状态初始值也必须是 true，否则第一帧会先闪一下空态）。
  assert.match(store, /const \[projectsLoading, setProjectsLoading\] = useState\(true\);/);
  // 只在首屏阶段跟踪 loading：成功加载后若还随轮询来回切换，会让 context
  // value 持续换引用，整页无意义重渲染（与既有去重逻辑冲突）。
  assert.match(store, /const firstLoad = !projectsLoadedRef\.current;/);
  assert.match(store, /if \(firstLoad\) setProjectsLoading\(true\);/);
  // 拿到列表（哪怕是空数组）即视为已加载，并清掉上一次的错误。
  assert.match(store, /projectsLoadedRef\.current = true;/);
  assert.match(store, /setProjectsLoaded\(true\);/);
  assert.match(store, /setProjectsLoadError\(""\);/);
  // 失败时除了 6s 自动消失的全局 error，还要留一份持久错误供总览页展示；
  // 但只服务于"从未成功加载"，已加载后的轮询失败不能凭空长出错误面板。
  assert.match(store, /if \(!projectsLoadedRef\.current\) setProjectsLoadError\(message\);/);
  // 请求结束（含失败）必须归位 loading，否则骨架会永久停留。
  assert.match(store, /finally \{\s*\n\s*if \(firstLoad && requestVersion === projectRequestVersion\.current\) setProjectsLoading\(false\);/);
});

test("总览页：加载中显示骨架，失败显示重试，空态排在最后", () => {
  const loadingBranch = dashboard.indexOf("showInitialLoading ? <ProjectGridSkeleton />");
  const failedBranch = dashboard.indexOf('className="empty load-failed"');
  const emptyBranch = dashboard.indexOf("还没有项目");

  assert.ok(loadingBranch >= 0, "加载中应渲染骨架卡而不是空态");
  assert.ok(failedBranch >= 0, "加载失败应有独立的错误态");
  assert.ok(emptyBranch >= 0, "确实没有项目时仍要保留空态");
  // 顺序即优先级：正在加载 / 加载失败 都不能落到「还没有项目」。
  assert.ok(loadingBranch < failedBranch && failedBranch < emptyBranch, "空态必须排在加载中与失败态之后");

  // 两个判定都必须以"从未成功加载"为前提；已加载成功的空列表不该被后续轮询失败改写。
  assert.match(dashboard, /const showInitialLoading = !projectsLoaded && !projectsLoadError && projectsLoading;/);
  assert.match(dashboard, /const showLoadFailed = !projectsLoaded && Boolean\(projectsLoadError\);/);
  // 计数在列表到手前显示占位符，避免首屏四个 0 同样误导。
  assert.match(dashboard, /<b>\{projectsLoaded \? item\.count : "—"\}<\/b>/);
  // 失败态给的是「重试」，不是「加载项目」——失败时不该引导用户重复添加。
  const failedBlock = dashboard.slice(failedBranch, emptyBranch);
  assert.match(failedBlock, /onClick=\{retryLoad\}/);
  assert.doesNotMatch(failedBlock, /\/projects\/import/);
});

test("骨架与失败态有对应样式，且尊重减少动效偏好", () => {
  assert.match(styles, /\.project-card\.project-skeleton-card\s*\{/);
  assert.match(styles, /@keyframes project-skeleton-pulse/);
  assert.match(styles, /prefers-reduced-motion: reduce[^}]*project-skeleton/);
  assert.match(styles, /\.load-failed\s*\{/);
});
