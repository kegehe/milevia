// 升级状态 → 界面文案的纯映射。
//
// 静默升级把「下载」挪到了用户点击之前：检查到新版本就后台预下载，下载完才提示。
// 于是"要不要弹横幅"不再是"有没有新版本"，而是"包备好了没有"。这套判断全在这里，
// 组件只负责渲染，也方便把各个相位（含失败降级）逐一测掉。

export type UpdateDownloadPhase = "idle" | "downloading" | "ready" | "failed";

export type UpdateDownload = {
  phase: UpdateDownloadPhase;
  received: number;
  total: number | null;
  error?: string | null;
};

export type UpdateInfo = {
  currentVersion: string;
  version: string;
  notes?: string | null;
};

export type UpdaterStatus = {
  appVersion: string;
  status: "checking" | "complete" | "failed";
  update: UpdateInfo | null;
  error?: string | null;
  /** 后台静默预下载的进度；老版本 Rust 不带这个字段 */
  download?: UpdateDownload | null;
};

/** 认不出的相位（缺字段 / 旧版 Rust / 测试夹具）一律当作 idle —— 那是"还没开始下载"。 */
export function downloadPhase(status: UpdaterStatus | null): UpdateDownloadPhase {
  const phase = status?.download?.phase;
  return phase === "downloading" || phase === "ready" || phase === "failed" ? phase : "idle";
}

/** 轮询 `get_updater_status` 的节奏（毫秒）。 */
export const CHECK_POLL_MS = 250;
export const DOWNLOAD_POLL_MS = 2_000;
export const IDLE_POLL_MS = 30_000;
/** 检查阶段的密集轮询上限（240 × 250ms ≈ 60s），超过就降到慢节奏继续等。 */
export const CHECK_POLL_BUDGET = 240;

/**
 * 下一次查询升级状态的间隔（毫秒）。
 *
 * 刻意**没有"不再轮询"这一档**：状态不只由启动检查改写 —— 打开托盘面板、点设置页的
 * "检查更新"都会重查一次（可能带来新版本，也可能让静默下载重新跑起来），而"后台下好
 * 之后提示用户"正是靠横幅继续问下去才浮得出来。慢节奏问一句的成本可以忽略。
 */
export function nextStatusPollMs(status: UpdaterStatus | null, checkingPolls: number): number {
  if (status?.status === "checking" && checkingPolls <= CHECK_POLL_BUDGET) return CHECK_POLL_MS;
  if (downloadPhase(status) === "downloading") return DOWNLOAD_POLL_MS;
  return IDLE_POLL_MS;
}

/** 后台下载百分比；总大小未知时为 null（界面改显示"下载中"而不是编一个数）。 */
export function downloadPercent(status: UpdaterStatus | null): number | null {
  if (downloadPhase(status) !== "downloading") return null;
  const total = status?.download?.total;
  if (!total || total <= 0) return null;
  const received = status?.download?.received ?? 0;
  return Math.min(100, Math.max(0, Math.round((received / total) * 100)));
}

export type BannerUpdate =
  | { kind: "none" }
  | { kind: "ready"; update: UpdateInfo }
  | { kind: "manual"; update: UpdateInfo };

/**
 * 手上是否有一个"已下载并验签、可以直接安装"的包。
 *
 * 与最近一次检查成不成功无关：包本身已经过签名校验，装它是安全的，而那次失败
 * 往往只是网络抖动（自建源暂时不可达）。Rust 侧失败时也刻意保住它（见
 * `apply_check_failure`），这里不认就等于白保。
 */
export function readyUpdate(status: UpdaterStatus | null): UpdateInfo | null {
  if (!status?.update) return null;
  return downloadPhase(status) === "ready" ? status.update : null;
}

/** 有没有可安装的更新（决定界面要不要给出"立即安装 / 立即升级"这个动作）。 */
export function installableUpdate(status: UpdaterStatus | null): UpdateInfo | null {
  if (!status?.update) return null;
  // 检查成功过 → 给"发现新版本"的入口（点了现场下载）；
  // 包已备好 → 即使最近一次检查失败，也能直接装。
  return readyUpdate(status) ?? (status.status === "complete" ? status.update : null);
}

/**
 * 主界面右上角横幅该显示什么。
 *
 * `dismissedVersion` 是用户点过 ✕ 的版本号（不是布尔值）：关掉 0.1.8 的提示
 * 不该连 0.1.9 的提示一起吞掉。
 *
 * 后台静默下载期间返回 `none` —— 这是「静默」的落点：下载不该打扰任何人。
 * 下载失败、或压根没开始下载时退回 `manual`（发现新版本 → 点击后再下载安装），
 * 保证静默下载失败不会让用户失去升级能力。
 */
export function bannerUpdate(
  status: UpdaterStatus | null,
  dismissedVersion: string | null,
): BannerUpdate {
  const update = status?.update;
  if (!update || dismissedVersion === update.version) return { kind: "none" };
  const phase = downloadPhase(status);
  // 已备好的包优先于"最近一次检查失败"：能装就是能装，别把它藏起来。
  if (phase === "ready") return { kind: "ready", update };
  if (phase === "downloading") return { kind: "none" };
  // "发现新版本"这个说法得有一次成功的检查背书，不能拿旧公告吓人。
  if (status?.status !== "complete") return { kind: "none" };
  return { kind: "manual", update };
}

/** 设置页「关于」卡片的描述文案。 */
export function settingsUpdateDescription(
  status: UpdaterStatus | null,
  error: string | null,
): string {
  const update = status?.update;
  // 已备好的包优先于一切"检查失败"（无论错误来自本次手动检查还是共享状态）：
  // 包已经验签，不联网也能装，藏起来只会让那次下载白费。失败原因在括号里
  // 如实带出来，不假装检查是好的。
  if (update && downloadPhase(status) === "ready") {
    const notes = update.notes ? ` 更新内容：${update.notes.trim().slice(0, 80)}` : "";
    // 只在共享状态也说"失败"时才提这次失败：本地错误可能是上一轮留下的，
    // 别处发起的重查成功后它还挂着 —— 挂在"已就绪"后面就是一条假消息。
    const reason = status?.status === "failed" ? error || status.error : null;
    const failure = reason ? `（最近一次检查失败：${reason}，不影响安装）` : "";
    return `新版本 v${update.version} 已下载完成，点击「立即安装」后应用会自动重启。${notes}${failure}`;
  }
  if (error) return error;
  if (!status) return "正在读取更新状态。";
  if (status.status === "failed") return status.error || "更新检查失败，请稍后重试。";
  if (status.status === "checking") return "正在检查更新…";
  if (!update) return "当前已是最新版本。";
  const notes = update.notes ? `：${update.notes.trim().slice(0, 80)}` : "";
  switch (downloadPhase(status)) {
    case "downloading": {
      const percent = downloadPercent(status);
      return `正在后台下载 v${update.version}${
        percent == null ? "" : `（${percent}%）`
      }，完成后即可一键安装。`;
    }
    case "failed":
      return `发现新版本 v${update.version}：后台下载未完成，点击「立即升级」重新下载。`;
    default:
      return `发现新版本 v${update.version}${notes}`;
  }
}

/** 设置页主按钮文案。 */
export function settingsInstallLabel(status: UpdaterStatus | null, installing: boolean): string {
  if (installing) return "升级中";
  switch (downloadPhase(status)) {
    case "ready":
      return "立即安装";
    case "downloading":
      return "下载中";
    default:
      return "立即升级";
  }
}

/** 设置页主按钮是否禁用：下载中不给点，免得"点了没反应"。 */
export function settingsInstallDisabled(
  status: UpdaterStatus | null,
  checking: boolean,
  installing: boolean,
): boolean {
  return checking || installing || downloadPhase(status) === "downloading";
}

/** 手动点「检查更新」之后的提示语。 */
export function checkResultToast(status: UpdaterStatus): string {
  if (status.status === "failed") return status.error || "无法检查更新，请稍后重试。";
  const update = status.update;
  if (!update) return "当前已是最新版本";
  switch (downloadPhase(status)) {
    case "ready":
      return `发现新版本 v${update.version}，已下载完成，可直接安装`;
    case "downloading":
      return `发现新版本 v${update.version}，正在后台下载`;
    default:
      return `发现新版本 v${update.version}`;
  }
}
