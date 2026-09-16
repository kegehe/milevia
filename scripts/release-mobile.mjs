#!/usr/bin/env node
// Milevia 手机端发版脚本 —— 升 Android 版本号 + 出 APK + 更新清单里的 android 段。
//
// 与 release.mjs 分开的原因：那条流程绑定桌面端（会一并升 tauri/Cargo/desktop 三处
// 版本、并要求 tauri 签名），而手机端要的是完全不同的清单策略（见下面 §4）。
//
// 用法（在仓库根目录）：
//   node scripts/release-mobile.mjs 0.1.6 "修复了…"
//   node scripts/release-mobile.mjs --bump-only 0.1.6      # 只同步 Android 版本号
//   node scripts/release-mobile.mjs 0.1.6 "…" --no-build   # 复用已有 APK，只更新清单
//   node scripts/release-mobile.mjs 0.1.6 "…" --deploy     # 上传：自建更新源 + GitHub Release 资产
//
// 环境变量：
//   MILEVIA_DOWNLOAD_BASE  自建源清单里 APK 的下载基地址（默认自建服务器
//                          https://keyanjia.info:8443/updates）
//   MILEVIA_DEPLOY_TARGET  --deploy 的 scp 目标（如 root@host:/var/www/milevia/dist/updates/）
//   MILEVIA_ANDROID_KEYSTORE / _PASSWORD / _KEY_ALIAS / _KEY_PASSWORD
//                           Android 正式签名（可选）。也可放 apps/web/android/keystore.properties。
//                           两者都没有时回退 debug 签名 —— 注意 debug 签名只在同一台机器上
//                           有效，换机器后新包无法覆盖安装，需要先卸载（手机上会丢配对令牌）。
//
// 清单会写两份，下载地址分别指向两个能真正下到包的地方（见 scripts/lib/update-manifest.mjs）：
//   release/updates/latest.json → 自建源（传到服务器的 /updates/），国内快，主用路径
//   仓库根 latest.json           → GitHub Pages 备用源，地址指向 GitHub Release 资产
// gh 未安装或未登录时会跳过 GitHub 上传并告警，此时备用源清单【不提供下载地址】——
// 它只会表现为"没有更新"，而不会先弹提示再下载失败。
//
// 注意：versionCode 必须单调递增，Android 只认它。versionName 是给人看的，因此允许
// 手机端与桌面端各自递增（桌面端 release.mjs 已不再改 Android 版本号）。

import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { copyFileSync, existsSync, mkdirSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import {
  githubReleaseBase,
  pruneRemoteDownloads,
  publishGithubRelease,
  readManifest,
  setPlatform,
  toFallbackManifest,
  toSelfHostedManifest,
  writeManifest,
} from "./lib/update-manifest.mjs";

const repoRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const webDir = join(repoRoot, "apps/web");
const androidDir = join(webDir, "android");
const releaseDir = join(repoRoot, "release");
const updatesDir = join(releaseDir, "updates");

const args = process.argv.slice(2);
const bumpOnly = args.includes("--bump-only");
const noBuild = args.includes("--no-build");
const deploy = args.includes("--deploy");
const positional = args.filter((a) => !a.startsWith("--"));

if (positional.length < 1) {
  throw new Error(
    "缺少版本号参数。\n示例:\n  node scripts/release-mobile.mjs 0.1.6 \"修复了XX\"\n  node scripts/release-mobile.mjs --bump-only 0.1.6",
  );
}
const nextVersion = positional[0];
const notes = positional.slice(1).join(" ") || "Milevia 手机端新版本";
if (!/^\d+\.\d+\.\d+$/.test(nextVersion)) {
  throw new Error(`版本号必须形如 0.1.6（MAJOR.MINOR.PATCH），收到：${nextVersion}`);
}

/* ── 1. 升 Android 版本号 ────────────────────────────────── */
const gradleFile = join(androidDir, "app/build.gradle");
const gradleSource = readFileSync(gradleFile, "utf8");
// 锚定行首，避免把注释里出现的 versionCode 之类文本当成真值（桌面脚本已踩过这个坑）。
const codeMatch = gradleSource.match(/^[ \t]*versionCode\s+(\d+)/m);
const nameMatch = gradleSource.match(/^[ \t]*versionName\s+"([^"]+)"/m);
if (!codeMatch || !nameMatch) {
  throw new Error(`无法从 ${gradleFile} 解析 versionCode / versionName，请确认 app/build.gradle 格式未变。`);
}
const currentCode = Number(codeMatch[1]);
const currentName = nameMatch[1];
// 同一版本重跑（例如只补出包）不再递增，避免 versionCode 被无意义地抬高。
const nextCode = currentName === nextVersion ? currentCode : currentCode + 1;

writeFileSync(
  gradleFile,
  gradleSource
    .replace(/^([ \t]*versionCode\s+)\d+/m, `$1${nextCode}`)
    .replace(/^([ \t]*versionName\s+")[^"]+(")/m, `$1${nextVersion}$2`),
  "utf8",
);
console.log(`已同步 Android 版本号：versionCode ${currentCode} → ${nextCode}，versionName ${currentName} → ${nextVersion}`);

if (bumpOnly) {
  console.log("--bump-only：到此为止，未构建、未改清单。");
  process.exit(0);
}

/* ── 2. 构建 APK ─────────────────────────────────────────── */
const hasKeystore =
  existsSync(join(androidDir, "keystore.properties")) || Boolean(process.env.MILEVIA_ANDROID_KEYSTORE);
const buildType = hasKeystore ? "release" : "debug";
const gradleTask = hasKeystore ? "assembleRelease" : "assembleDebug";
if (!hasKeystore) {
  console.warn(
    "〔警告〕未找到 Android 正式签名配置（apps/web/android/keystore.properties 或 MILEVIA_ANDROID_KEYSTORE）：\n" +
      "         本次产出 debug 签名包。它只能覆盖安装同一台机器、同一个 debug keystore 打出的包；\n" +
      "         换机器或系统重装后，安装会因签名不一致失败，必须先卸载（手机上需重新配对）。",
  );
}
const apkFileName = `Milevia_${nextVersion}_android.apk`;
const apkPath = join(releaseDir, apkFileName);
const builtApk = join(androidDir, `app/build/outputs/apk/${buildType}/app-${buildType}.apk`);

if (!noBuild) {
  // 直接用 node 跑各工具的入口，不经过 pnpm 的 shell shim：这里要的是稳定的
  // "web 产物 + 原生工程同步 + gradle 出包"三步，任何一步失败都应该立刻停下。
  const tscBin = join(webDir, "node_modules/typescript/bin/tsc");
  const viteBin = join(webDir, "node_modules/vite/bin/vite.js");
  const capBin = join(webDir, "node_modules/@capacitor/cli/bin/capacitor");
  for (const [name, path] of [["typescript", tscBin], ["vite", viteBin], ["@capacitor/cli", capBin]]) {
    if (!existsSync(path)) throw new Error(`找不到 ${name} 的可执行入口：${path}\n请先在 apps/web 下安装依赖。`);
  }

  console.log("① 构建 Web 产物（tsc -b && vite build）…");
  execFileSync(process.execPath, [tscBin, "-b"], { cwd: webDir, stdio: "inherit" });
  execFileSync(process.execPath, [viteBin, "build"], { cwd: webDir, stdio: "inherit" });

  console.log("② 同步到原生工程（cap sync android）…");
  execFileSync(process.execPath, [capBin, "sync", "android"], { cwd: webDir, stdio: "inherit" });

  console.log(`③ 构建 APK（${gradleTask}）…`);
  execFileSync(process.platform === "win32" ? "gradlew.bat" : "./gradlew", [gradleTask], {
    cwd: androidDir,
    stdio: "inherit",
    shell: process.platform === "win32",
  });
}

if (!existsSync(builtApk)) {
  throw new Error(
    `未找到 Android 安装包：${builtApk}\n` +
      `  当前按「${hasKeystore ? "已配置正式 keystore" : "未配置正式 keystore"}」选择了 ${gradleTask}。` +
      (noBuild ? "\n  你带了 --no-build，请去掉该参数重新构建。" : ""),
  );
}
mkdirSync(releaseDir, { recursive: true });
copyFileSync(builtApk, apkPath);
const apkSize = statSync(apkPath).size;
const apkSha256 = createHash("sha256").update(readFileSync(apkPath)).digest("hex");
console.log(`Android 安装包（${buildType} 签名）：${apkPath}`);
console.log(`  大小 ${apkSize} 字节 · sha256 ${apkSha256}`);

/* ── 3. 更新清单里的 android 段 ───────────────────────────── */
// 清单与桌面端共用。顶层的 version/notes/pub_date 以及 windows-x86_64 段属于桌面端，
// 这里【只替换 platforms.android】。若顺手把顶层 version 改成手机端版本号，桌面端会
// 认为有新版本可升、而它的下载地址仍指向旧的 .exe —— 一次手机发版就会把桌面端的更新
// 提示弄坏，所以必须原样保留。
//
// 清单读写与分发在 scripts/lib/update-manifest.mjs：这里写出的两份分别是"自建源"和
// "GitHub Pages 备用源"，备用源会把下载地址换到 GitHub Release 资产（拿不到资产时
// 不提供下载地址，表现为"没有更新"—— 绝不给一个注定失败的下载）。
const stored = readManifest([join(updatesDir, "latest.json"), join(repoRoot, "latest.json")]);
if (!stored) {
  throw new Error(
    `找不到现有更新清单（找过 ${join(updatesDir, "latest.json")} 与 ${join(repoRoot, "latest.json")}）。\n` +
      "  请先用桌面端发版脚本生成一份，本脚本只在它之上合并 android 段，不会凭空造清单。",
  );
}
const manifest = stored.manifest;
const downloadBase = (process.env.MILEVIA_DOWNLOAD_BASE || "https://keyanjia.info:8443/updates").replace(/\/+$/, "");
setPlatform(manifest, "android", {
  version: nextVersion,
  versionCode: nextCode,
  notes,
  pub_date: new Date().toISOString(),
  url: `${downloadBase}/${apkFileName}`,
  size: apkSize,
  sha256: apkSha256,
  // Tauri 更新器把 platforms 读成 HashMap<String, { url, signature }>，signature 不是可选项 ——
  // 任何一段缺它都会让【整份清单】反序列化失败，报 "missing field `signature`"，而且这发生在
  // 版本比较之前：哪怕版本相同、根本不该更新，桌面端的检查更新也会直接报错。
  // android 不是 Tauri 的 target、这个字段永远不会被读取，但不给就会连累桌面端。
  // 这里放 APK 的 sha256（真实校验值），而不是伪造一个 minisign 签名。
  signature: apkSha256,
});

mkdirSync(updatesDir, { recursive: true });
const selfHostedManifestPath = join(updatesDir, "latest.json");
writeManifest(selfHostedManifestPath, toSelfHostedManifest(manifest, downloadBase));
copyFileSync(apkPath, join(updatesDir, apkFileName));
console.log(`已更新清单（仅替换 platforms.android，桌面端字段未动）：${selfHostedManifestPath}`);
console.log(`已生成自托管上传目录：${updatesDir}`);

/* ── 4. 发布：GitHub Release 资产 + 自建源 ────────────────── */
const githubBase = githubReleaseBase(repoRoot);
const publishedTags = new Set();
if (deploy) {
  // 先把 APK 传到 GitHub Release，再据此写 Pages 备用清单：只有确实传上去了，
  // 备用源才敢宣称能下载。
  const uploaded = githubBase && publishGithubRelease({
    repoRoot,
    tag: `v${nextVersion}`,
    title: `Milevia v${nextVersion}（Android）`,
    notes,
    files: [apkPath],
  });
  if (uploaded) publishedTags.add(`v${nextVersion}`);
  // 桌面端那份若已存在，说明桌面端脚本发布过并上传过它的安装包；
  // 记下它的标签，让备用源也能给出桌面的下载地址。
  if (manifest.platforms?.["windows-x86_64"] && typeof manifest.version === "string" && manifest.version) {
    publishedTags.add(`v${manifest.version}`);
  }
}

const fallback = toFallbackManifest(manifest, { base: githubBase, publishedTags });
const fallbackManifestPath = join(repoRoot, "latest.json");
writeManifest(fallbackManifestPath, fallback);
const fallbackPlatforms = Object.keys(fallback.platforms);
console.log(
  `已生成 Pages 备用清单：${fallbackManifestPath}` +
    `（可下载平台：${fallbackPlatforms.length ? fallbackPlatforms.join("、") : "无 —— 备用源只报告版本、不提供下载"}）`,
);

/* ── 5. 可选：上传到自建更新源 ────────────────────────────── */
if (deploy) {
  const target = process.env.MILEVIA_DEPLOY_TARGET;
  if (!target) {
    throw new Error("--deploy 需要设置 MILEVIA_DEPLOY_TARGET（scp 目标目录，例如 root@host:/var/www/milevia/dist/updates/）");
  }
  console.log(`正在通过 scp 上传 APK 与清单到 ${target} …`);
  execFileSync("scp", [join(updatesDir, apkFileName), selfHostedManifestPath, target], { stdio: "inherit" });
  console.log("已上传。手机上打开 App 即可看到「发现新版本」提示。");

  // 服务器磁盘紧张：手机的 APK 只保留本次这一份（单个近 30MB，不清理几次就上百 MB）。
  // 只清 android 形态的历史文件，不会碰到桌面端的安装包。
  pruneRemoteDownloads({
    target,
    entries: [{ glob: "Milevia_*_android.apk", keep: apkFileName }],
  });
  process.exit(0);
}

console.log(`\n后续动作：\n  scp release/updates/${apkFileName} release/updates/latest.json <服务器>:/var/www/milevia/dist/updates/\n  或直接重跑并加 --deploy（同时会把 APK 传到 GitHub Release）。`);
