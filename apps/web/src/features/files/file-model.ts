/**
 * 文件浏览器类型定义与工具函数
 */

// ─── API 响应类型 ───────────────────────────────────────────────────────────

export interface FileEntry {
  name: string;
  path: string;
  isDir: boolean;
  size?: number;
  modTime?: string;
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
}

export interface TreeResponse {
  entries: FileEntry[];
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
}

// ─── 语言检测 ───────────────────────────────────────────────────────────────

const extToLanguage: Record<string, string> = {
  ".ts": "typescript",
  ".tsx": "tsx",
  ".js": "javascript",
  ".jsx": "jsx",
  ".mjs": "javascript",
  ".cjs": "javascript",
  ".css": "css",
  ".scss": "scss",
  ".less": "less",
  ".html": "html",
  ".htm": "html",
  ".json": "json",
  ".json5": "json",
  ".jsonc": "json",
  ".md": "markdown",
  ".mdx": "markdown",
  ".py": "python",
  ".go": "go",
  ".rs": "rust",
  ".rb": "ruby",
  ".java": "java",
  ".kt": "kotlin",
  ".scala": "scala",
  ".c": "c",
  ".cpp": "cpp",
  ".h": "c",
  ".hpp": "cpp",
  ".cs": "csharp",
  ".toml": "toml",
  ".yaml": "yaml",
  ".yml": "yaml",
  ".xml": "xml",
  ".svg": "xml",
  ".sh": "bash",
  ".bash": "bash",
  ".zsh": "bash",
  ".sql": "sql",
  ".graphql": "graphql",
  ".gql": "graphql",
  ".vue": "html",
  ".svelte": "html",
  ".dart": "dart",
  ".swift": "swift",
  ".lua": "lua",
  ".r": "r",
  ".proto": "protobuf",
  ".env": "bash",
  ".gitignore": "bash",
  ".dockerignore": "bash",
  ".conf": "ini",
  ".ini": "ini",
  ".cfg": "ini",
  ".log": "log",
  ".diff": "diff",
  ".patch": "diff",
  ".txt": "text",
  ".lock": "json",
  ".map": "json",
};

const nameToLanguage: Record<string, string> = {
  makefile: "makefile",
  dockerfile: "dockerfile",
  rakefile: "ruby",
  gemfile: "ruby",
  procfile: "text",
  vagrantfile: "ruby",
  license: "text",
  readme: "markdown",
  changelog: "markdown",
};

/**
 * 根据文件名检测语言类型
 */
export function detectLanguage(filename: string): string {
  const lower = filename.toLowerCase();
  if (nameToLanguage[lower]) {
    return nameToLanguage[lower];
  }
  const ext = lower.includes(".") ? "." + lower.split(".").pop()! : "";
  return extToLanguage[ext] || "text";
}

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
