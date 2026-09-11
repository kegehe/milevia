// 更新清单的读写与分发 —— 桌面端 release.mjs 与手机端 release-mobile.mjs 共用。
//
// 这里的硬约束只有一条：**每张清单里的下载地址，必须指向"拿到这张清单的那台客户端
// 真能下到的地方"。**
//
//   - 自建源那张（keyanjia.info:8443/updates/latest.json）：地址指向自建服务器，
//     国内下载快，是主用路径。
//   - GitHub Pages 那张（kegehe.github.io/milevia/latest.json）：地址指向 GitHub
//     Release 资产。它存在的唯一理由是"自建源整个挂了"时，App 还能查到、还能下到。
//
// 两张内容一致、地址都指向自建源的话，自建源挂掉时 App 会先弹出"发现新版本"，
// 再在下载那一步必然失败 —— 等于承诺了一个它做不到的更新。所以某个平台若在 GitHub
// 上没有对应资产，那张清单里就【不写它的下载信息】（表现为"没有更新"）：
// 宁可少提示一次，也不要给出一个注定失败的下载。
//
// 顶层的 version/notes/pub_date 属于桌面端（桌面端更新器只读它）；platforms.android
// 属于手机端。两个脚本各自只写自己那部分，另一部分原样保留。

import { execFileSync } from "node:child_process";
import { existsSync, readFileSync, writeFileSync } from "node:fs";

/** 读第一份存在的清单；一份都没有则返回 null。 */
export function readManifest(candidates) {
  for (const file of candidates) {
    if (!existsSync(file)) continue;
    try {
      return { file, manifest: JSON.parse(readFileSync(file, "utf8")) };
    } catch (error) {
      throw new Error(`${file} 不是合法 JSON：${error.message}`);
    }
  }
  return null;
}

/** 就地把某个平台段替换/新增，其它段原样保留。 */
export function setPlatform(manifest, key, entry) {
  if (!manifest.platforms || typeof manifest.platforms !== "object") manifest.platforms = {};
  manifest.platforms[key] = entry;
  return manifest;
}

/** 写清单：统一两空格缩进 + 末尾换行，避免每次发布都产生无意义的 diff。 */
export function writeManifest(file, manifest) {
  writeFileSync(file, JSON.stringify(manifest, null, 2) + "\n", "utf8");
}

/** 取 url 末段作为文件名（去掉 query/fragment）。 */
function fileNameOf(url) {
  if (typeof url !== "string") return "";
  const clean = url.split("?")[0].split("#")[0];
  return clean.substring(clean.lastIndexOf("/") + 1);
}

/**
 * 某平台对应的 Release 标签：android 跟自己的版本号走，其余跟顶层版本号走。
 * 手机端与桌面端版本号从 0.1.6 起各自递增，所以必须按平台分别取。
 */
export function releaseTagFor(manifest, key) {
  if (key === "android") {
    const version = manifest.platforms?.android?.version;
    return typeof version === "string" && version ? `v${version}` : null;
  }
  return typeof manifest.version === "string" && manifest.version ? `v${manifest.version}` : null;
}

/**
 * 从 git remote 推出 GitHub Release 资产的基地址，避免把 owner/repo 写死在脚本里。
 * 支持 https://github.com/o/r.git 与 git@github.com:o/r.git 两种写法；推断不出来返回 null。
 */
export function githubReleaseBase(repoRoot) {
  let remote = "";
  try {
    remote = execFileSync("git", ["remote", "get-url", "origin"], { cwd: repoRoot, encoding: "utf8" }).trim();
  } catch {
    return null;
  }
  const match = remote.match(/github\.com[/:]([^/]+)\/([^/]+?)(?:\.git)?$/);
  return match ? `https://github.com/${match[1]}/${match[2]}/releases/download` : null;
}

/**
 * 生成自建源那张清单：每个平台的地址都重写到自建基地址（按文件名重写，因此
 * 无论读进来的是哪张清单，写出去的都自洽）。
 */
export function toSelfHostedManifest(manifest, base) {
  const platforms = {};
  for (const [key, entry] of Object.entries(manifest.platforms ?? {})) {
    const fileName = fileNameOf(entry?.url);
    platforms[key] = fileName ? { ...entry, url: `${base}/${fileName}` } : { ...entry };
  }
  return { ...manifest, platforms };
}

/**
 * 生成 GitHub Pages 那张（备用源）清单：地址换到 GitHub Release 资产；
 * 没有已发布资产的平台整段去掉 —— 备用源宁可"看起来没有更新"，也不给注定失败的下载。
 *
 * publishedTags 是"确认已上传到 GitHub 的标签集合"，由调用方在上传成功后给出：
 * 只有在确实传上去之后，备用源才敢宣称能下载。
 */
export function toFallbackManifest(manifest, { base, publishedTags }) {
  const platforms = {};
  for (const [key, entry] of Object.entries(manifest.platforms ?? {})) {
    const tag = releaseTagFor(manifest, key);
    const fileName = fileNameOf(entry?.url);
    if (!base || !tag || !fileName || !publishedTags.has(tag)) continue;
    platforms[key] = { ...entry, url: `${base}/${tag}/${fileName}` };
  }
  return { ...manifest, platforms };
}

/**
 * 把资产发到 GitHub Release（不存在就先创建）。返回是否成功。
 *
 * 失败只警告不中断：清单会自动降级为"备用源不提供下载"，不存在"清单说有、实际下不到"
 * 的中间状态。gh 未安装或未登录同样按失败处理。
 */
export function publishGithubRelease({ repoRoot, tag, title, notes, files }) {
  const existing = files.filter((file) => existsSync(file));
  if (existing.length === 0) {
    console.warn("〔警告〕没有可上传的资产，跳过 GitHub Release。");
    return false;
  }
  const gh = (...ghArgs) => execFileSync("gh", ghArgs, { cwd: repoRoot, stdio: "inherit" });
  try {
    execFileSync("gh", ["auth", "status"], { cwd: repoRoot, stdio: "ignore" });
  } catch {
    console.warn("〔警告〕gh 未安装或未登录，跳过 GitHub Release；备用源清单将不提供下载地址。");
    return false;
  }
  try {
    execFileSync("gh", ["release", "view", tag], { cwd: repoRoot, stdio: "ignore" });
    console.log(`GitHub Release ${tag} 已存在，覆盖上传资产…`);
  } catch {
    console.log(`GitHub Release ${tag} 不存在，创建…`);
    gh("release", "create", tag, "--title", title, "--notes", notes);
  }
  gh("release", "upload", tag, ...existing, "--clobber");
  console.log(`已上传到 GitHub Release ${tag}：${existing.map((file) => file.split(/[\\/]/).pop()).join("、")}`);
  return true;
}

/**
 * 上传后清掉远端更新目录里的历史安装包，每个平台只留最新一份。
 *
 * 服务器磁盘紧张（40G 用了 27G），而每个 APK / 安装包有二三十 MB，几次发布就能
 * 吃掉上百 MB。历史版本在这里没有保留价值：每个平台的下载地址由 latest.json
 * 指向，只有被它引用的那一份是有效产物，其余都是再也不会被下载的死文件。
 *
 * entries: [{ glob, keep }]，keep 传 null 表示该形态已不再发布，历史文件全清。
 * 只删除 glob 命中的文件，因此桌面端发版不会碰到手机的 APK，反之亦然。
 * 代价：想"回退到上一版"就没有现成安装包了，需要时从 GitHub Release 或本地
 * release/ 目录取。
 */
export function pruneRemoteDownloads({ target, entries }) {
  const sep = target.lastIndexOf(":");
  if (sep < 0) {
    throw new Error(`MILEVIA_DEPLOY_TARGET 格式应为 user@host:/path，收到：${target}`);
  }
  const host = target.slice(0, sep);
  const dir = target.slice(sep + 1).replace(/\/+$/, "");
  const lines = [`cd '${dir}' || exit 1`];
  for (const { glob, keep } of entries) {
    const list = `ls ${glob} 2>/dev/null`;
    // grep -vx 是整行精确排除，避免版本号互相是前缀时误删（0.1.6 与 0.1.60）。
    lines.push(keep ? `${list} | grep -vx '${keep}' | xargs -r rm -f` : `${list} | xargs -r rm -f`);
  }
  lines.push(`echo '--- 更新目录现有安装包 ---'`);
  lines.push(`ls -1 ${entries.map((entry) => entry.glob).join(" ")} 2>/dev/null || true`);
  execFileSync("ssh", [host, lines.join("\n")], { stdio: "inherit" });
}
