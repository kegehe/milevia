/**
 * 文件浏览器类型定义与工具函数
 */

import type { FilePreviewKind } from "./source-language";
export { detectLanguage } from "./source-language";

// ─── API 响应类型 ───────────────────────────────────────────────────────────

export interface FileEntry {
  name: string;
  path: string;
  isDir: boolean;
  size?: number;
  modTime?: string;
  /**
   * 只在带 `depth` 的 `/fs/tree` 响应里出现（手机端一次多拿几层时）。
   * 桌面端的扁平调用不带它，所以是可选的。
   *
   * 判据分工：`children` 缺席且 `unreadable` 不为真 ⇒ **空目录**；
   * `unreadable` 为真 ⇒ **这个目录读不到**（权限、或读到一半被删掉）。
   * 两者都不能靠"有没有 children"单独区分，别把它们合成一个字段。
   */
  children?: FileEntry[];
  unreadable?: boolean;
}

export interface FileInfo {
  name: string;
  path: string;
  isDir: boolean;
  size: number;
  modTime: string;
  mode: string;
  isText: boolean;
  mimeType: string;
}

export interface FileContent {
  content: string;
  encoding?: "base64";
  version: string;
  stat: FileInfo;
  /**
   * 服务端判定的"这个文件能不能改"。**只有手机端会有**（走 `/fs/open`），桌面端是
   * undefined，界面回落到 `isEditableFile(stat)` 那个本地判据。
   *
   * 为什么必须信服务端：能不能编辑不取决于文件本身，而取决于**内容能不能原样发回去** ——
   * 那要减去 JSON 转义与中继信封的余量，而这两个数只有服务端知道。客户端按扩展名
   * 自己判，会把一个 300 KiB 的源码文件标成可编辑，用户改完按保存才失败。
   */
  editable?: boolean;
  /** `editable` 为 false 时的原因，用来在界面上说清"为什么不能改"。 */
  readOnlyReason?: "file_too_large" | "binary_file";
}

export interface TreeResponse {
  entries: FileEntry[];
  /** 条目太多、这次没取全（配额被裁）。**必须如实告诉用户**，不能静默丢弃。 */
  truncated?: boolean;
  /**
   * 因忽略名单跳过的依赖/构建目录数（node_modules、.git、dist 等）。
   *
   * 它与 `truncated` 是两件事：跳过依赖目录是**预期行为**（"隐藏了 3 个依赖目录"），
   * `truncated` 才是"这次没取全"。合成一个布尔量就没法对用户说清是哪一种。
   */
  skippedDirs?: number;
}

export interface SearchResponse {
  entries: FileEntry[];
}

// ─── UI 状态类型 ────────────────────────────────────────────────────────────

export interface OpenFile {
  path: string;
  name: string;
  content: string;
  originalContent: string;
  version: string;
  language: string;
  isDirty: boolean;
  stat: FileInfo;
  previewKind: FilePreviewKind;
  contentLoaded: boolean;
  /**
   * 服务端**故意没给内容**时（文件太大 / 不是文本）的原因与说明。
   *
   * 非空时这个标签页只能渲染元信息卡：内容字段是空的，而 `previewKind` 是按扩展名
   * 算出来的（一个大 .ts 文件仍然算出 "source"）—— 按它渲染就是一个空白编辑器，
   * 用户会以为文件是空的。手机端才有这个状态。
   */
  omitted?: { message: string; reason: "too_large" | "binary" };
  /**
   * 服务端说了"这个文件不能编辑"（`/fs/open` 的 `editable: false`）。只有手机端会有。
   *
   * 它与 `omitted` 是两件事：`omitted` 是**连内容都没给**，这个是**给了完整内容但
   * 不该改**（内容大到发不回去）。手机端有一条 256–320 KiB 的只读带，桌面端没有，
   * 所以这里必须是可选字段：桌面端为 undefined，一概沿用原来的本地判据。
   */
  editable?: boolean;
  readOnlyReason?: "file_too_large" | "binary_file";
}

// ─── 语言检测 ───────────────────────────────────────────────────────────────

/**
 * 获取文件图标键（用于 FileIcon 组件）
 */
export function getFileIcon(entry: FileEntry): string {
  if (entry.isDir) return "folder";
  const lower = entry.name.toLowerCase();
  // 按完整文件名匹配
  const nameMap: Record<string, string> = {
    makefile: "shell",
    dockerfile: "shell",
    rakefile: "ruby",
    gemfile: "ruby",
    procfile: "shell",
    vagrantfile: "ruby",
    license: "file",
    readme: "md",
    changelog: "md",
  };
  if (nameMap[lower]) return nameMap[lower];
  // dotfile 前缀匹配（.env.local, .env.production 等也应显示为 config 图标）
  const dotPrefixMap: Record<string, string> = {
    ".env": "config",
    ".gitignore": "git",
    ".dockerignore": "config",
    ".eslintrc": "config",
    ".prettierrc": "config",
    ".editorconfig": "config",
    ".babelrc": "config",
    ".npmrc": "config",
    ".nvmrc": "config",
    ".pylintrc": "config",
  };
  for (const prefix of Object.keys(dotPrefixMap)) {
    if (lower === prefix || lower.startsWith(prefix + ".")) {
      return dotPrefixMap[prefix];
    }
  }
  // 按扩展名匹配
  const ext = lower.includes(".")
    ? "." + lower.split(".").pop()!
    : "";
  const iconKeyMap: Record<string, string> = {
    ".ts": "ts",
    ".tsx": "ts",
    ".js": "js",
    ".jsx": "js",
    ".mjs": "js",
    ".cjs": "js",
    ".css": "css",
    ".scss": "css",
    ".less": "css",
    ".html": "html",
    ".htm": "html",
    ".vue": "html",
    ".svelte": "html",
    ".json": "json",
    ".json5": "json",
    ".jsonc": "json",
    ".md": "md",
    ".markdown": "md",
    ".mdx": "md",
    ".py": "py",
    ".go": "go",
    ".rs": "rust",
    ".rb": "ruby",
    ".java": "java",
    ".kt": "java",
    ".scala": "java",
    ".yaml": "config",
    ".yml": "config",
    ".toml": "config",
    ".ini": "config",
    ".cfg": "config",
    ".conf": "config",
    ".sh": "shell",
    ".bash": "shell",
    ".zsh": "shell",
    ".sql": "sql",
    ".graphql": "config",
    ".gql": "config",
    ".png": "image",
    ".jpg": "image",
    ".jpeg": "image",
    ".gif": "image",
    ".xml": "html",
    ".svg": "image",
    ".map": "json",
    ".c": "config",
    ".h": "config",
    ".cpp": "config",
    ".hpp": "config",
    ".cs": "config",
    ".dart": "config",
    ".swift": "config",
    ".lua": "config",
    ".r": "config",
    ".proto": "config",
    ".diff": "config",
    ".patch": "config",
    ".log": "config",
    ".txt": "file",
    ".webp": "image",
    ".ico": "image",
    ".pdf": "file",
    ".db": "sql",
    ".sqlite": "sql",
    ".sqlite3": "sql",
    ".zip": "zip",
    ".tar": "zip",
    ".gz": "zip",
    ".lock": "lock",
  };
  return iconKeyMap[ext] || "file";
}

/**
 * 格式化文件大小
 */
export function formatSize(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return "—";
  if (bytes === 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB", "PB"];
  const i = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1);
  const value = bytes / Math.pow(1024, i);
  return value.toFixed(i === 0 ? 0 : 1) + " " + units[i];
}

/**
 * 判断文件是否为图片（SVG 除外，SVG 是文本文件可编辑）
 */
export function isImageFile(mimeType: string): boolean {
  return mimeType.startsWith("image/") && mimeType !== "image/svg+xml";
}

/**
 * 判断文件是否可编辑（文本文件且非图片）
 */
export function isEditableFile(stat: FileInfo): boolean {
  return stat.isText && !isImageFile(stat.mimeType);
}

/**
 * 获取文件路径中的目录部分
 */
export function getDirPath(path: string): string {
  const idx = path.lastIndexOf("/");
  return idx >= 0 ? path.substring(0, idx) : "";
}

/**
 * 获取文件路径中的文件名部分
 */
export function getBaseName(path: string): string {
  const idx = path.lastIndexOf("/");
  return idx >= 0 ? path.substring(idx + 1) : path;
}

// ─── 跨页面深链用的会话内传参 key ────────────────────────────────────────────
// 其它页面跳到文件页时，通过 sessionStorage 传"要打开哪个文件"（与"添加到对话"的
// milevia_add_file_to_chat 同一套路）。放在 feature 模块里而不是某个页面里，避免
// 页面之间互相 import。
export const OPEN_FILE_STORAGE_KEY = "milevia_open_file";
