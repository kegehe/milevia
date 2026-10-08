// 手机端（Android）应用内更新检查。
//
// 与桌面端不同，Android 包没法自己完成"下载并安装"：拉起系统安装器需要
// REQUEST_INSTALL_PACKAGES 权限加一段原生代码。所以这里先做最小但可靠的闭环 ——
// 比对版本号、把下载地址交给系统浏览器（与页面上其它外链同一机制：Capacitor 会把
// 非应用域名的导航交给系统浏览器）。用户点一下就能装上新包，不必再在电脑和手机
// 之间倒文件。
//
// 两个更新源，与桌面端 tauri.conf.json 的 updater endpoints 一致：
//   主源 = 自建服务器（国内快）；备源 = GitHub Pages（自建源不可达时才用）。
// 两张清单的下载地址各自指向同侧的主机，所以任一源都能真的下到包 —— 详见发布脚本
// scripts/lib/update-manifest.mjs。
//
// 解析与多源回退的纯逻辑在 ./android-release（不 import Capacitor，便于单测）。

import { App } from "@capacitor/app";
import { Capacitor } from "@capacitor/core";
import { resolveAndroidRelease, type AndroidRelease } from "./android-release";

export const MOBILE_UPDATE_MANIFEST_URLS = [
  // 主源：自建服务器。与桌面端 tauri.conf.json 的 endpoints 第一条相同。
  "https://keyanjia.info:8443/updates/latest.json",
  // 备源：GitHub Pages。只有主源整个不可达时才会走到这里。
  "https://kegehe.github.io/milevia/latest.json",
] as const;

export type MobileUpdateState = {
  /** 当前安装的 versionName。 */
  currentVersion: string;
  /** 当前安装的 versionCode（字符串形式）。 */
  currentBuild: string;
  release: AndroidRelease;
};

/** 当前安装的 Android 包版本（versionName + versionCode）。 */
export type MobileAppInfo = { version: string; build: string };

/**
 * 读取当前安装的包版本。**与 checkMobileUpdate 分开**：那条只在"有新版本"时才返回，
 * 没有更新时版本号读完即弃 —— 于是"我装的是哪一版"在界面上就永远没有来源
 * （移动端此前正因此一处都不显示版本）。
 *
 * 非原生端返回 null：浏览器 / 桌面没有"已安装的包版本"这回事，而且 Web 端
 * `App.getInfo()` 会抛 unimplemented（见 @capacitor/app 的 web 实现）。
 */
export async function readMobileAppInfo(): Promise<MobileAppInfo | null> {
  if (!Capacitor.isNativePlatform()) return null;
  try {
    const info = await App.getInfo();
    const version = typeof info?.version === "string" ? info.version.trim() : "";
    if (!version) return null;
    return { version, build: typeof info?.build === "string" ? info.build.trim() : "" };
  } catch {
    // 读不到就当作"不知道"，不编一个版本号出来。
    return null;
  }
}

/**
 * 查询是否有可用的 Android 新版本。仅在原生端生效；Web 端返回 null。
 * 读不到自身 versionCode 时不提示更新（宁可漏报也不要让用户装上一个装不上的包）。
 */
export async function checkMobileUpdate(signal?: AbortSignal): Promise<MobileUpdateState | null> {
  if (!Capacitor.isNativePlatform()) return null;

  const info = await App.getInfo();
  const currentVersionCode = Number(info.build);
  if (!Number.isFinite(currentVersionCode)) return null;

  const release = await resolveAndroidRelease({
    currentVersionCode,
    manifestUrls: MOBILE_UPDATE_MANIFEST_URLS,
    fetchManifest: async (url) => {
      const response = await fetch(url, { cache: "no-store", signal });
      if (!response.ok) throw new Error(`${url} 返回 HTTP ${response.status}`);
      return response.json();
    },
    isAborted: () => signal?.aborted === true,
  });
  if (!release) return null;

  return { currentVersion: info.version, currentBuild: info.build, release };
}
