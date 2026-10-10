import assert from "node:assert/strict";
import test from "node:test";
import {
  bannerUpdate,
  checkResultToast,
  downloadFailureReason,
  downloadPercent,
  downloadPhase,
  installableUpdate,
  nextStatusPollMs,
  readyUpdate,
  settingsInstallDisabled,
  settingsInstallLabel,
  settingsUpdateDescription,
  CHECK_POLL_BUDGET,
  CHECK_POLL_MS,
  DOWNLOAD_POLL_MS,
  IDLE_POLL_MS,
  type UpdaterStatus,
} from "./update-view";

function status(partial: Partial<UpdaterStatus> = {}): UpdaterStatus {
  return {
    appVersion: "0.1.7",
    status: "complete",
    update: { currentVersion: "0.1.7", version: "0.1.8", notes: "修复若干问题" },
    error: null,
    download: { phase: "idle", received: 0, total: null },
    ...partial,
  };
}

// 这组里最要紧的一条：**下载期间必须一声不吭**。
// 静默升级的全部意义就在这儿 —— 后台预下载一旦弹了横幅，就退化成"点一下再等"，
// 白下载一场。
test("后台下载中不弹任何横幅", () => {
  const view = bannerUpdate(
    status({ download: { phase: "downloading", received: 512, total: 1024 } }),
    null,
  );
  assert.deepEqual(view, { kind: "none" });
});

test("下载完成才提示，且提示的是「已就绪」而不是「发现新版本」", () => {
  const view = bannerUpdate(status({ download: { phase: "ready", received: 1024, total: 1024 } }), null);
  assert.equal(view.kind, "ready");
  assert.equal(view.kind === "ready" ? view.update.version : "", "0.1.8");
});

// 静默下载失败不能让用户失去升级能力：退回"发现新版本 → 点击后再下载安装"。
test("下载失败退回手动升级，而不是装作没这回事", () => {
  for (const phase of ["failed", "idle"] as const) {
    const view = bannerUpdate(status({ download: { phase, received: 0, total: null } }), null);
    assert.equal(view.kind, "manual", `${phase} 应当退回手动升级`);
  }
});

// Rust 侧在检查失败时刻意保住了已下好的包（apply_check_failure）；前端要是
// 拿"检查失败"一票否决，那个包就白保了 —— 明明不联网也能装。
test("已备好的包不受「最近一次检查失败」影响", () => {
  const offline = status({
    status: "failed",
    error: "网络不可达",
    download: { phase: "ready", received: 2048, total: 2048 },
  });
  assert.equal(bannerUpdate(offline, null).kind, "ready");
  assert.equal(installableUpdate(offline)?.version, "0.1.8");
  assert.equal(readyUpdate(offline)?.version, "0.1.8");
  assert.match(settingsUpdateDescription(offline, null), /已下载完成/);
  // 但失败本身也要如实说出来，别让用户以为检查是好的
  assert.match(settingsUpdateDescription(offline, null), /最近一次检查失败：网络不可达/);
});

test("没有备好的包时，检查失败不给安装入口", () => {
  for (const phase of ["idle", "downloading", "failed"] as const) {
    const offline = status({
      status: "failed",
      error: "网络不可达",
      download: { phase, received: 0, total: null },
    });
    assert.equal(installableUpdate(offline), null, `${phase} 不该给安装入口`);
    assert.equal(readyUpdate(offline), null);
    assert.equal(bannerUpdate(offline, null).kind, "none");
  }
});

test("检查成功时，只要有公告版本就给安装入口（未备好也退回现场下载）", () => {
  assert.equal(installableUpdate(status())?.version, "0.1.8");
  assert.equal(installableUpdate(status({ update: null })), null);
  assert.equal(installableUpdate(null), null);
});

test("缺 download 字段（旧版 Rust）按未开始下载处理，仍然给手动升级入口", () => {
  const legacy = status({ download: undefined });
  assert.equal(downloadPhase(legacy), "idle");
  assert.equal(bannerUpdate(legacy, null).kind, "manual");
});

test("没有新版本 / 还在检查 / 用户关掉了：都不弹", () => {
  assert.deepEqual(bannerUpdate(status({ update: null }), null), { kind: "none" });
  assert.deepEqual(bannerUpdate(status({ status: "checking" }), null), { kind: "none" });
  assert.deepEqual(bannerUpdate(status({ status: "failed", update: null }), null), { kind: "none" });
  assert.deepEqual(bannerUpdate(status(), "0.1.8"), { kind: "none" });
  assert.deepEqual(bannerUpdate(null, null), { kind: "none" });
});

// 关掉的是"这一版"的提示：用户关掉 0.1.8 之后，0.1.9 该提示还是要提示，
// 否则一次"稍后提醒"会把后续所有版本的提示一起吞掉。
test("关掉某一版的提示不影响下一版", () => {
  const next = status({ update: { currentVersion: "0.1.7", version: "0.1.9" } });
  assert.equal(bannerUpdate(next, "0.1.8").kind, "manual");
  assert.equal(bannerUpdate(next, "0.1.9").kind, "none");
});

test("认不出的相位一律当 idle，不猜", () => {
  const weird = status({ download: { phase: "wat" as never, received: 0, total: null } });
  assert.equal(downloadPhase(weird), "idle");
  assert.equal(downloadPercent(weird), null);
});

// 这一条守的是"后台下好之后要能提示用户"：横幅必须在终态继续慢速续约。
// 谁要是把"终态不再轮询"优化回来，这里就该红。
test("终态也继续慢速续约，绝不彻底停下", () => {
  assert.equal(nextStatusPollMs(status({ update: null }), 1), IDLE_POLL_MS);
  assert.equal(
    nextStatusPollMs(status({ download: { phase: "ready", received: 2, total: 2 } }), 1),
    IDLE_POLL_MS,
  );
  assert.equal(nextStatusPollMs(status({ status: "failed", update: null }), 1), IDLE_POLL_MS);
  assert.equal(nextStatusPollMs(null, 0), IDLE_POLL_MS);
});

test("检查中密集问、下载中按秒问、超过预算降到慢节奏", () => {
  const checking = status({ status: "checking" });
  assert.equal(nextStatusPollMs(checking, 1), CHECK_POLL_MS);
  assert.equal(nextStatusPollMs(checking, CHECK_POLL_BUDGET), CHECK_POLL_MS);
  assert.equal(nextStatusPollMs(checking, CHECK_POLL_BUDGET + 1), IDLE_POLL_MS);

  assert.equal(
    nextStatusPollMs(status({ download: { phase: "downloading", received: 1, total: 2 } }), 0),
    DOWNLOAD_POLL_MS,
  );
});

test("下载百分比：总大小未知或非法时给 null，不让界面编数字", () => {
  assert.equal(downloadPercent(status({ download: { phase: "downloading", received: 5, total: null } })), null);
  assert.equal(downloadPercent(status({ download: { phase: "downloading", received: 5, total: 0 } })), null);
  assert.equal(downloadPercent(status({ download: { phase: "downloading", received: 512, total: 2048 } })), 25);
  // 只读的进度事件偶尔会先于 Content-Length 到位，别算出 150% 这种数
  assert.equal(downloadPercent(status({ download: { phase: "downloading", received: 4096, total: 2048 } })), 100);
  // 不在下载中就没有百分比可言
  assert.equal(downloadPercent(status({ download: { phase: "ready", received: 2048, total: 2048 } })), null);
});

test("设置页描述跟着相位走：下载中报进度、就绪说可以直接装", () => {
  assert.match(
    settingsUpdateDescription(status({ download: { phase: "downloading", received: 512, total: 2048 } }), null),
    /正在后台下载 v0\.1\.8（25%）/,
  );
  assert.match(
    settingsUpdateDescription(status({ download: { phase: "ready", received: 2048, total: 2048 } }), null),
    /已下载完成/,
  );
  assert.match(
    settingsUpdateDescription(status({ download: { phase: "failed", received: 0, total: null } }), null),
    /后台下载未完成/,
  );
  assert.match(settingsUpdateDescription(status(), null), /发现新版本 v0\.1\.8/);
});

// 静默下载失败时，用户看到的只有"后台下载未完成，点击后重新下载"—— 拿不到任何
// 原因，只能瞎点。Rust 侧一直在写 download.error（已本地化），前端却从没人读它。
test("后台下载失败的原因要显示出来，不能只说「未完成」", () => {
  const failed = status({
    download: {
      phase: "failed",
      received: 0,
      total: null,
      error: "下载更新失败：网络不可用（error sending request）",
    },
  });
  const text = settingsUpdateDescription(failed, null);
  assert.match(text, /后台下载未完成/);
  assert.match(text, /下载更新失败：网络不可用/);
  // 原因在括号里，与"发现新版本"那句连成一句，不是另起一行
  assert.match(text, /后台下载未完成（下载更新失败/);

  assert.equal(downloadFailureReason(failed), "下载更新失败：网络不可用（error sending request）");
  // 只有 failed 相位才谈得上"失败原因"：其它相位即便残留着旧字段也不能当失败用。
  for (const phase of ["idle", "downloading", "ready"] as const) {
    const other = status({ download: { phase, received: 0, total: null, error: "上一轮的原因" } });
    assert.equal(downloadFailureReason(other), null, `${phase} 不该报失败原因`);
    assert.doesNotMatch(settingsUpdateDescription(other, null), /上一轮的原因/);
  }
  // 老版本 Rust 不带 error 字段，或字段是空白：照旧只说"未完成"，不能崩也不能编。
  assert.equal(downloadFailureReason(status({ download: { phase: "failed", received: 0, total: null } })), null);
  assert.equal(
    downloadFailureReason(status({ download: { phase: "failed", received: 0, total: null, error: "   " } })),
    null,
  );
  assert.match(
    settingsUpdateDescription(status({ download: { phase: "failed", received: 0, total: null } }), null),
    /后台下载未完成，点击「立即升级」重新下载。/,
  );
  assert.equal(downloadFailureReason(null), null);
});

test("设置页描述里，本地错误与检查失败优先于相位文案", () => {
  assert.equal(settingsUpdateDescription(status(), "安装失败"), "安装失败");
  assert.equal(
    settingsUpdateDescription(status({ status: "failed", error: "网络不可达", update: null }), null),
    "网络不可达",
  );
  assert.equal(settingsUpdateDescription(status({ status: "checking" }), null), "正在检查更新…");
  assert.equal(settingsUpdateDescription(status({ update: null }), null), "当前已是最新版本。");
  // 读不到状态（IPC 出错）时也不能装作没事
  assert.equal(settingsUpdateDescription(null, "控制服务无响应"), "控制服务无响应");
});

// 手动点"检查更新"失败时，错误会先落到组件本地的 updaterError 上。它要是盖过
// "已就绪"，卡片就会说"网络不可达"而按钮写着"立即安装" —— 自相矛盾。
test("本地检查错误也不能盖掉已备好的包", () => {
  const failed = status({
    status: "failed",
    error: "网络不可达",
    download: { phase: "ready", received: 2, total: 2 },
  });
  const text = settingsUpdateDescription(failed, "网络不可达");
  assert.match(text, /已下载完成/);
  assert.match(text, /最近一次检查失败：网络不可达/);
  // 没备好包时，本地错误照旧优先
  assert.equal(settingsUpdateDescription(status(), "网络不可达"), "网络不可达");
});

// 反过来也要守：本地错误可能是上一轮留下的（别处发起的重查已经成功），
// 这时把它挂在"已就绪"后面就是一条假消息。
test("共享状态已经成功时，不再提本地的旧错误", () => {
  const ready = status({ status: "complete", error: null, download: { phase: "ready", received: 2, total: 2 } });
  const text = settingsUpdateDescription(ready, "网络不可达");
  assert.match(text, /已下载完成/);
  assert.doesNotMatch(text, /网络不可达/);
});

test("下载中不给点「立即升级」，就绪才叫「立即安装」", () => {
  const downloading = status({ download: { phase: "downloading", received: 1, total: 2 } });
  assert.equal(settingsInstallLabel(downloading, false), "下载中");
  assert.equal(settingsInstallDisabled(downloading, false, false), true);

  const ready = status({ download: { phase: "ready", received: 2, total: 2 } });
  assert.equal(settingsInstallLabel(ready, false), "立即安装");
  assert.equal(settingsInstallDisabled(ready, false, false), false);

  // 没备好包时仍是老文案：点了才现场下载
  assert.equal(settingsInstallLabel(status(), false), "立即升级");
  assert.equal(settingsInstallDisabled(status(), false, false), false);

  // 检查中 / 安装中一律不可点
  assert.equal(settingsInstallDisabled(ready, true, false), true);
  assert.equal(settingsInstallDisabled(ready, false, true), true);
  assert.equal(settingsInstallLabel(ready, true), "升级中");
});

test("检查完的提示语要说明「现在能不能直接装」", () => {
  assert.equal(
    checkResultToast(status({ download: { phase: "ready", received: 2, total: 2 } })),
    "发现新版本 v0.1.8，已下载完成，可直接安装",
  );
  assert.equal(
    checkResultToast(status({ download: { phase: "downloading", received: 1, total: 2 } })),
    "发现新版本 v0.1.8，正在后台下载",
  );
  assert.equal(checkResultToast(status()), "发现新版本 v0.1.8");
  assert.equal(checkResultToast(status({ update: null })), "当前已是最新版本");
  assert.equal(
    checkResultToast(status({ status: "failed", update: null, error: "网络不可达" })),
    "网络不可达",
  );
});
