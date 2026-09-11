#!/usr/bin/env node
// Milevia 桌面端发版脚本 —— 把"升版本号 + 打包 + 签名 + 生成清单"封装成一条命令。
//
// 只在你【明确要发版】时手动跑；日常 commit 完全不碰它，不影响正常开发。
//
// 手机端（Android）已独立到 scripts/release-mobile.mjs：Android 的
// versionCode/versionName、APK 构建、以及清单里的 platforms.android 段，全部由那个
// 脚本独占管理。本脚本不再碰 apps/web/android，也不会覆盖 android 段 —— 否则桌面端
// 发版会在本地偷偷升一次 Android 版本号、白建一个既不上传也不进清单的 APK，
// 让 build.gradle 的数字跑到线上包前面去。
//
// 用法（在仓库根号目录）：
//   node scripts/release.mjs 0.1.1 "修复了…\n新增了…"
//   node scripts/release.mjs --bump-only 0.1.1      # 只同步三处版本号，不打包
//   node scripts/release.mjs 0.1.1 --no-build       # 不重新出包，签现有安装包（需已 build）
//   node scripts/release.mjs 0.1.1 --deploy         # 打包+签名后上传：自建更新源 + GitHub Release 资产
//
// 环境变量：
//   MILEVIA_PRIVKEY         私钥文件路径（默认 ~/.tauri/milevia-updater.key）
//   MILEVIA_PASSFILE        私钥口令文件（默认 ~/.tauri/milevia-updater-password.txt）
//   MILEVIA_DOWNLOAD_BASE   自建源清单里安装包的下载基地址（默认自建服务器
//                           https://keyanjia.info:8443/updates，可覆盖成其它云存储源）
//   MILEVIA_DEPLOY_TARGET   --deploy 的 scp 目标目录（如 root@host:/var/www/milevia/dist/updates/）
//                           gh 未安装或未登录时会跳过 GitHub 上传并告警，不中断发布。
//
// 清单会写两份，下载地址分别指向"拿到那份清单的客户端真能下到的地方"：
//   release/updates/latest.json → 自建源（上传到服务器的 /updates/），国内快，主用路径
//   仓库根 latest.json           → GitHub Pages 备用源，地址指向 GitHub Release 资产
// 详见 scripts/lib/update-manifest.mjs 顶部说明：备用源宁可表现为"没有更新"，
// 也不要先弹"发现新版本"再在下载那一步必然失败。

import { execFileSync } from "node:child_process";
import { copyFileSync, existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
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
const HOME = homedir();

const args = process.argv.slice(2);
const bumpOnly = args.includes("--bump-only");
const noBuild = args.includes("--no-build");
const deploy = args.includes("--deploy");
const positional = args.filter((a) => !a.startsWith("--"));

if (positional.length < 1) {
  const msg =
    "缺少版本号参数。\n示例:\n  node scripts/release.mjs 0.1.1 \"修复了XX\"\n  node scripts/release.mjs --bump-only 0.2.0";
  throw new Error(msg);
}

const nextVersion = positional[0];
const notes = positional.slice(1).join(" ") || "Milevia 新版本";

if (!/^\d+\.\d+\.\d+$/.test(nextVersion)) {
  throw new Error(`版本号必须形如 0.1.1（MAJOR.MINOR.PATCH），收到：${nextVersion}`);
}

/* ── 需要同步的版本文件 ─────────────────────────────────── */
// 只同步桌面端三处：tauri.conf.json / Cargo.toml / desktop/package.json。
// Android 的 versionCode/versionName 归 scripts/release-mobile.mjs 独占管理。
const tauriConf = join(repoRoot, "apps/desktop/src-tauri/tauri.conf.json");
const cargoToml = join(repoRoot, "apps/desktop/src-tauri/Cargo.toml");
const desktopPkg = join(repoRoot, "apps/desktop/package.json");

function readJson(file) { return JSON.parse(readFileSync(file, "utf8")); }
function writeJson(file, obj) { writeFileSync(file, JSON.stringify(obj, null, 2) + "\n", "utf8"); }

function readCargoVersion(file) {
  const match = readFileSync(file, "utf8").match(/^version\s*=\s*"([^"]+)"/m);
  return match ? match[1] : null;
}

/* ── 1. 校验当前三处版本一致，然后一起升到新版本 ─────────── */
const current = {
  tauriConf: readJson(tauriConf).version,
  cargo: readCargoVersion(cargoToml),
  desktopPkg: readJson(desktopPkg).version,
};

const uniq = new Set(Object.values(current));
if (uniq.size > 1) {
  const listing = Object.entries(current).map(([k, v]) => `  ${k}: ${v}`).join("\n");
  throw new Error(`三处版本号当前不一致，先手动统一再发版：\n${listing}`);
}

console.log(`当前版本 ${current.tauriConf} → 新版本 ${nextVersion}`);

const tauri = readJson(tauriConf);
tauri.version = nextVersion;
writeJson(tauriConf, tauri);

const cargo = readFileSync(cargoToml, "utf8")
  .replace(/^(version\s*=\s*")[^"]+(")/m, `$1${nextVersion}$2`);
writeFileSync(cargoToml, cargo, "utf8");

const dpkg = readJson(desktopPkg);
dpkg.version = nextVersion;
writeJson(desktopPkg, dpkg);

console.log(`已同步三处版本号：tauri.conf.json / Cargo.toml / desktop/package.json → ${nextVersion}`);

if (bumpOnly) {
  console.log("bump-only：仅升级版本号，未打包、未签名。");
  process.exit(0);
}

/* ── 2. 打包（复用现有 desktop build：web + assets + sidecar + tauri build）── */
if (!noBuild) {
  console.log("开始打包（web + sidecar + tauri build）…");
  execFileSync("pnpm", ["--filter", "@milevia/desktop", "build"], {
    cwd: repoRoot,
    stdio: "inherit",
    // Windows 下 pnpm 是 .cmd shim，需经 shell 才能被 Node 解析到。
    shell: process.platform === "win32",
    env: buildEnv(),
  });
}

/* ── 3. 定位 NSIS 安装包并签名 ─────────────────────────── */
const productName = readJson(tauriConf).productName ?? "Milevia";
const installerName = `${productName}_${nextVersion}_x64-setup.exe`;
const installer = join(
  repoRoot,
  "apps/desktop/src-tauri/target/release/bundle/nsis",
  installerName,
);
if (!existsSync(installer)) {
  throw new Error(`未找到安装包：${installer}\n请确认已执行 build（或去掉 --no-build）。`);
}
console.log(`安装包：${installer}`);

const privkey = process.env.MILEVIA_PRIVKEY || join(HOME, ".tauri", "milevia-updater.key");
const passfile = process.env.MILEVIA_PASSFILE || join(HOME, ".tauri", "milevia-updater-password.txt");
if (!existsSync(privkey)) throw new Error(`找不到私钥：${privkey}`);
if (!existsSync(passfile)) throw new Error(`找不到口令文件：${passfile}`);

const releaseDir = join(repoRoot, "release");
mkdirSync(releaseDir, { recursive: true });


// 签名：位置参数 <FILE>；私钥路径与口令走环境变量（不进命令行，避免出现在进程/日志里）。
// 直接调用 apps/desktop 里挂装的 tauri CLI JS 入口，绕开 pnpm 子命令解析问题。
const tauriCli = join(repoRoot, "apps/desktop/node_modules/@tauri-apps/cli/tauri.js");
if (!existsSync(tauriCli)) {
  throw new Error(`找不到 tauri CLI 入口：${tauriCli}\n请确认已安装 @tauri-apps/cli。`);
}
execFileSync(
  "node", [tauriCli, "signer", "sign", installer],
  {
    cwd: repoRoot,
    stdio: "inherit",
    env: {
      ...process.env,
      TAURI_SIGNING_PRIVATE_KEY_PATH: privkey,
      // 口令文件若带 UTF-8 BOM（PowerShell Set-Content 默认会加），剥离后再用，
      // 否则 BOM 字符会混进口令导致 "wrong password"。显式用 \uFEFF 匹配 BOM。
      TAURI_SIGNING_PRIVATE_KEY_PASSWORD: readFileSync(passfile, "utf8")
        .replace(/\uFEFF/, "")
        .trim(),
    },
  },
);
const signature = readFileSync(`${installer}.sig`, "utf8").trim();

/* ── 4. 生成两份清单 ───────────────────────────────────── */
// 安装包下载基地址默认为自建服务器（keyanjia.info:8443，国内可达），
// 以摆脱对 GitHub 的强依赖（GitHub Pages / Releases 在国内网络下不稳定会“无法检查更新”）。
// 可用 MILEVIA_DOWNLOAD_BASE 覆盖成其它源（如 Cloudflare R2 / 对象存储）。
const downloadBase = (process.env.MILEVIA_DOWNLOAD_BASE || "https://keyanjia.info:8443/updates").replace(/\/+$/, "");

// 读写与分发逻辑在 scripts/lib/update-manifest.mjs，两份清单的下载地址分别指向
// 自建源和 GitHub Release 资产（原因见该文件顶部说明）。
//
// 关键点：读现有清单后【只改 windows-x86_64 段】。platforms.android 原样保留 ——
// 它由 scripts/release-mobile.mjs 独占维护，在这里覆盖掉会让手机端的更新提示失效。
const storedManifest = readManifest([
  join(repoRoot, "release", "updates", "latest.json"),
  join(repoRoot, "latest.json"),
]);
const manifest = storedManifest?.manifest ?? { platforms: {} };
manifest.version = nextVersion;
manifest.notes = notes;
manifest.pub_date = new Date().toISOString(); // ISO 8601 UTC
setPlatform(manifest, "windows-x86_64", {
  signature,
  url: `${downloadBase}/${installerName}`,
});

// 供自托管源上传的目录：把【安装包 + latest.json】放一起，scp/rsync 整个目录即可。
const updatesDir = join(repoRoot, "release", "updates");
mkdirSync(updatesDir, { recursive: true });
const selfHostedManifestPath = join(updatesDir, "latest.json");
writeManifest(selfHostedManifestPath, toSelfHostedManifest(manifest, downloadBase));
copyFileSync(installer, join(updatesDir, installerName));
console.log(`已生成自建源清单：${selfHostedManifestPath}（url 指向 ${downloadBase}/）`);
console.log(`已生成自托管上传目录：${updatesDir}`);

/* ── 5. 发布：GitHub Release 资产 + 自建源 ───────────────── */
const githubBase = githubReleaseBase(repoRoot);
const publishedTags = new Set();
if (deploy) {
  // 先把资产传到 GitHub Release，再据此写 Pages 那张备用清单 —— 只有确实传上去了，
  // 备用源才敢宣称能下载（否则它会先弹"发现新版本"再在下载那一步失败）。
  const uploaded = githubBase && publishGithubRelease({
    repoRoot,
    tag: `v${nextVersion}`,
    title: `${productName} v${nextVersion}`,
    notes,
    files: [installer],
  });
  if (uploaded) publishedTags.add(`v${nextVersion}`);
  // android 段若已存在，说明手机端脚本发布过、也把它自己的 APK 传上去过；
  // 记下它的标签，让备用源也能给出手机的下载地址。
  const androidVersion = manifest.platforms?.android?.version;
  if (typeof androidVersion === "string" && androidVersion) publishedTags.add(`v${androidVersion}`);
}

// 仓库根那份由 GitHub Pages 提供，是桌面端与手机端的备用检查端点。
const fallback = toFallbackManifest(manifest, { base: githubBase, publishedTags });
const fallbackManifestPath = join(repoRoot, "latest.json");
writeManifest(fallbackManifestPath, fallback);
const fallbackPlatforms = Object.keys(fallback.platforms);
console.log(
  `已生成 Pages 备用清单：${fallbackManifestPath}` +
    `（可下载平台：${fallbackPlatforms.length ? fallbackPlatforms.join("、") : "无 —— 备用源只报告版本、不提供下载，表现为“没有更新”"}）`,
);

/* ── 6. 部署 + 打印后续动作 ────────────────────────────── */
if (deploy) {
  // 把 install 包与清单 scp 到服务器的 /updates/ 静态目录（对应 endpoints 的 keyanjia.info:8443/updates）。
  // 需先保证远端 /var/www/milevia/dist/updates/ 存在。
  const target = process.env.MILEVIA_DEPLOY_TARGET;
  if (!target) {
    throw new Error("--deploy 需要设置 MILEVIA_DEPLOY_TARGET（scp 目标目录，例如 root@host:/var/www/milevia/dist/updates/）");
  }
  console.log(`正在通过 scp 上传到 ${target} …`);
  execFileSync("scp", [join(updatesDir, installerName), selfHostedManifestPath, target], { stdio: "inherit" });
  console.log(`已上传安装包与升级清单：${target}`);

  // 服务器磁盘紧张：桌面的安装包只保留本次这一份。
  // 只清同形态的历史文件，不会碰到手机端的 APK；.msi 已不再发布，历史上若有残留一并清掉。
  pruneRemoteDownloads({
    target,
    entries: [
      { glob: "Milevia_*_x64-setup.exe", keep: installerName },
      { glob: "Milevia_*_x64_en-US.msi", keep: null },
    ],
  });

  // 上传后立即回读远程清单做自检：
  //   - 若命中 SPA 兜底（文件缺失 + 未配 nginx /updates），会返回 200 + index.html，
  //     updater 会因 JSON 解析失败【硬失败且不落到 GitHub 兜底端点】，这里必须当场拦截。
  //   - 返回 404（已配 /updates 但文件缺失）同样拦截。
  console.log(`正在校验远程清单 ${downloadBase}/latest.json …`);
  const probe = await fetch(`${downloadBase}/latest.json`);
  if (!probe.ok) {
    throw new Error(`远程清单返回 HTTP ${probe.status}：请确认已把 release/updates 上传到服务器 /var/www/milevia/dist/updates/，且 nginx 已配置 location /updates/ 并 reload。`);
  }
  const probeText = await probe.text();
  let remoteManifest;
  try {
    remoteManifest = JSON.parse(probeText);
  } catch {
    throw new Error(
      "远程 /updates/latest.json 不是合法 JSON——很可能命中了 SPA 兜底（服务器返回了 index.html 而不是清单）。\n" +
      "请先在服务器 Nginx 应用基础设施/nginx-keyanjia-8443.conf.example 里的 location /updates/ 并 reload，再重试 --deploy。",
    );
  }
  if (remoteManifest.version !== nextVersion) {
    throw new Error(`远程清单版本异常：期望 ${nextVersion}，实际 ${remoteManifest.version}。可能命中了旧缓存，请检查服务器文件与 CDN 缓存。`);
  }
  console.log(`校验通过：${downloadBase}/latest.json 已返回版本 ${nextVersion}。`);
} else {
  console.log("\n发布动作已完成（未上传，未加 --deploy）。下一步:");
  console.log("  上传自托管源（国内可达，先在服务器建好 updates 目录）：scp -r release/updates/. root@<your-server>:/var/www/milevia/dist/updates/");
  console.log(`     —— 或直接用：MILEVIA_DEPLOY_TARGET=root@host:/var/www/milevia/dist/updates/ node scripts/release.mjs ${nextVersion} --deploy`);
  console.log("  --deploy 会自动把安装包传到 GitHub Release（标签 v" + nextVersion + "）；gh 未登录时会跳过并告警，此时备用源不提供下载地址。");
  console.log("  手机端请改用：node scripts/release-mobile.mjs <版本> \"说明\" --deploy");
  console.log("\n注意：已安装旧版（内置 GitHub 端点）的机器无法走应用内升级，需手动安装一次新 exe 才能救活。");
}

/** 构建环境：sidecar（go-sqlite3）需要 CGO + 可运行的 C 编译器。
 *  脚本自己在常见位置找 gcc 并加入 PATH、强制 CGO_ENABLED=1，
 *  避免“明明装了 mingw 却不在当前 shell PATH”导致 go build 产出 stub 或直接失败。 */
function buildEnv() {
  const env = { ...process.env, CGO_ENABLED: process.env.CGO_ENABLED || "1" };
  if (process.platform !== "win32") return env;

  const found = findCompilerDir();
  if (found) {
    // Windows normally exposes `Path`, whereas inherited Node environments
    // can expose `PATH`. Preserve the original spelling for child processes.
    const pathKey = Object.prototype.hasOwnProperty.call(env, "Path") ? "Path" : "PATH";
    const currentPath = env[pathKey] || "";
    const sep = currentPath.includes(";") ? ";" : ":";
    if (!currentPath.split(sep).some((p) => p.toLowerCase() === found.toLowerCase())) {
      env[pathKey] = found + (currentPath ? sep + currentPath : "");
    }
  }
  const gcc = execFileSync("go", ["env", "CC"], { encoding: "utf8" }).trim();
  if (!gcc) {
    console.warn("〔警告〕未解析到 C 编译器（go env CC 为空），go-sqlite3 将无法编译，见 build-sidecar.mjs 的报错。");
  }
  return env;
}

/** 在常见的 MinGW/MSYS 安装位置定位 gcc.exe，命中返回其所在目录。 */
function findCompilerDir() {
  const candidates = [
    "C:/mingw64/bin", "C:/mingw64/mingw64/bin",
    "C:/msys64/mingw64/bin", "C:/TDM-GCC-64/bin",
    "C:/msys2/mingw64/bin", "C:/tools/msys64/mingw64/bin",
  ];
  for (const dir of candidates) {
    if (existsSync(join(dir, "gcc.exe"))) return dir;
  }
  return null;
}
