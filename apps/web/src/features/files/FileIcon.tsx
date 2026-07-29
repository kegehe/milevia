/**
 * 文件图标 SVG 组件 — 替换 emoji，与绿色工作室主题配色一致
 */

import { type JSX } from "react";

// ─── 图标路径数据 ──────────────────────────────────────────────────────────────

/** 通用文件 */
const FILE_PATHS = [
  "M6 2a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h8a2 2 0 0 0 2-2V8l-6-6H6zm5.5 0L16 6.5V4a2 2 0 0 0-2-2h-2.5z",
];

/** 文件夹 */
const FOLDER_PATHS = [
  "M2 4a1 1 0 0 1 1-1h4.586a1 1 0 0 1 .707.293l1.414 1.414a1 1 0 0 0 .707.293H13a1 1 0 0 1 1 1v8a1 1 0 0 1-1 1H3a1 1 0 0 1-1-1V4z",
];

/** 文件夹（打开） */
const FOLDER_OPEN_PATHS = [
  "M2 5a1 1 0 0 1 1-1h3.586a1 1 0 0 1 .707.293l1.414 1.414a1 1 0 0 0 .707.293H13a1 1 0 0 1 1 1v1H3.5A1.5 1.5 0 0 0 2 8.5V14a1 1 0 0 1-1-1V5z",
  "M3.5 8A1.5 1.5 0 0 0 2 9.5V13a1 1 0 0 0 1 1h10a1 1 0 0 0 1-1V9.5A1.5 1.5 0 0 0 12.5 8h-9z",
];

/** TypeScript / TSX — 蓝色系 */
const TS_PATHS = [
  "M6 2a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h8a2 2 0 0 0 2-2V8l-6-6H6zm5.5 0L16 6.5V4a2 2 0 0 0-2-2h-2.5z",
  "M7 10.5h2m-1 0V14m3-3.5h2",
];

/** JavaScript / JSX — 黄色系 */
const JS_PATHS = [
  "M6 2a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h8a2 2 0 0 0 2-2V8l-6-6H6zm5.5 0L16 6.5V4a2 2 0 0 0-2-2h-2.5z",
  "M7.5 11c0 .83-.67 1.5-1.5 1.5S4.5 11.83 4.5 11m7 0c0 .83-.67 1.5-1.5 1.5S9.5 11.83 9.5 11",
];

/** CSS / SCSS — 粉紫色系 */
const CSS_PATHS = [
  "M6 2a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h8a2 2 0 0 0 2-2V8l-6-6H6zm5.5 0L16 6.5V4a2 2 0 0 0-2-2h-2.5z",
  "M5.5 10l1.5 2-1.5 2m5-4l-1.5 2 1.5 2",
];

/** HTML — 橙色系 */
const HTML_PATHS = [
  "M6 2a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h8a2 2 0 0 0 2-2V8l-6-6H6zm5.5 0L16 6.5V4a2 2 0 0 0-2-2h-2.5z",
  "M5 10.5l2 1.5-2 1.5m6-3l-2 1.5 2 1.5",
];

/** JSON — 绿色系 */
const JSON_PATHS = [
  "M6 2a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h8a2 2 0 0 0 2-2V8l-6-6H6zm5.5 0L16 6.5V4a2 2 0 0 0-2-2h-2.5z",
  "M6 9v1a2 2 0 0 0 2 2h0a2 2 0 0 0 2-2V9m0 4v-1a2 2 0 0 1 2-2h0a2 2 0 0 1 2 2v1",
];

/** Markdown — 青色系 */
const MD_PATHS = [
  "M6 2a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h8a2 2 0 0 0 2-2V8l-6-6H6zm5.5 0L16 6.5V4a2 2 0 0 0-2-2h-2.5z",
  "M5 12h2l1.5-2L10 12h2V8h-2v2.5L8.5 8 7 10.5V8H5v4z",
];

/** Python — 蓝绿色系 */
const PY_PATHS = [
  "M6 2a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h8a2 2 0 0 0 2-2V8l-6-6H6zm5.5 0L16 6.5V4a2 2 0 0 0-2-2h-2.5z",
  "M7 9h2v1H7zm0 2h2v1H7z",
];

/** Go — 青蓝色系 */
const GO_PATHS = [
  "M6 2a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h8a2 2 0 0 0 2-2V8l-6-6H6zm5.5 0L16 6.5V4a2 2 0 0 0-2-2h-2.5z",
  "M8 10.5a2 2 0 1 1 0 3 2 2 0 0 1 0-3z",
];

/** Rust — 深橙红系 */
const RUST_PATHS = [
  "M6 2a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h8a2 2 0 0 0 2-2V8l-6-6H6zm5.5 0L16 6.5V4a2 2 0 0 0-2-2h-2.5z",
  "M8 9l2 2-2 2",
];

/** 配置文件 (yaml/toml/env) — 灰蓝色系 */
const CONFIG_PATHS = [
  "M6 2a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h8a2 2 0 0 0 2-2V8l-6-6H6zm5.5 0L16 6.5V4a2 2 0 0 0-2-2h-2.5z",
  "M6 9h4m-4 2h4m-4 2h3",
];

/** Shell — 黄绿色系 */
const SHELL_PATHS = [
  "M6 2a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h8a2 2 0 0 0 2-2V8l-6-6H6zm5.5 0L16 6.5V4a2 2 0 0 0-2-2h-2.5z",
  "M7 10l2 1.5L7 13",
];

/** Java — 橙色系 */
const JAVA_PATHS = [
  "M6 2a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h8a2 2 0 0 0 2-2V8l-6-6H6zm5.5 0L16 6.5V4a2 2 0 0 0-2-2h-2.5z",
  "M7 10h2v1.5H7zm0 3h2",
];

/** Ruby — 红色系 */
const RUBY_PATHS = [
  "M6 2a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h8a2 2 0 0 0 2-2V8l-6-6H6zm5.5 0L16 6.5V4a2 2 0 0 0-2-2h-2.5z",
  "M8 9l2 2.5-2 2.5",
];

/** SQL — 琥珀色系 */
const SQL_PATHS = [
  "M6 2a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h8a2 2 0 0 0 2-2V8l-6-6H6zm5.5 0L16 6.5V4a2 2 0 0 0-2-2h-2.5z",
  "M7 10.5h2m-1 0V14",
];

/** Git — 红橙色系 */
const GIT_PATHS = [
  "M6 2a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h8a2 2 0 0 0 2-2V8l-6-6H6zm5.5 0L16 6.5V4a2 2 0 0 0-2-2h-2.5z",
  "M8 9v4m-2-2h4",
];

/** 图片 — 紫粉色系 */
const IMAGE_PATHS = [
  "M6 2a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h8a2 2 0 0 0 2-2V8l-6-6H6zm5.5 0L16 6.5V4a2 2 0 0 0-2-2h-2.5z",
  "M8 11a1 1 0 1 0 0-2 1 1 0 0 0 0 2zm-3 2l2-2 1.5 1.5L11 10l3 3H5z",
];

/** 锁文件 — 红橙色系 */
const LOCK_PATHS = [
  "M6 2a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h8a2 2 0 0 0 2-2V8l-6-6H6zm5.5 0L16 6.5V4a2 2 0 0 0-2-2h-2.5z",
  "M8 10v-1a2 2 0 1 1 4 0v1m-5 0h6v4H7v-4z",
];

/** 压缩包 — 棕色系 */
const ZIP_PATHS = [
  "M6 2a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h8a2 2 0 0 0 2-2V8l-6-6H6zm5.5 0L16 6.5V4a2 2 0 0 0-2-2h-2.5z",
  "M8 9h2m-2 2h2m-2 2h2",
];

// ─── 颜色映射 ──────────────────────────────────────────────────────────────────

const COLORS = {
  folder: "#55a697",
  folderOpen: "#2c7567",
  file: "#71887e",
  ts: "#3178c6",
  js: "#f0db4f",
  css: "#a855f7",
  html: "#e34c26",
  json: "#2c7567",
  md: "#0891b2",
  py: "#3572a5",
  go: "#00add8",
  rust: "#ce422b",
  config: "#6b7280",
  shell: "#65a30d",
  image: "#c026d3",
  lock: "#dc2626",
  zip: "#92400e",
  java: "#f89820",
  ruby: "#cc342d",
  sql: "#e38c00",
  git: "#f05033",
} as const;

// ─── 图标定义 ──────────────────────────────────────────────────────────────────

interface IconDef {
  paths: string[];
  color: string;
  fillRule?: "evenodd";
}

const ICONS: Record<string, IconDef> = {
  folder: { paths: FOLDER_PATHS, color: COLORS.folder },
  "folder-open": { paths: FOLDER_OPEN_PATHS, color: COLORS.folderOpen },
  file: { paths: FILE_PATHS, color: COLORS.file },
  ts: { paths: TS_PATHS, color: COLORS.ts },
  js: { paths: JS_PATHS, color: COLORS.js },
  css: { paths: CSS_PATHS, color: COLORS.css },
  html: { paths: HTML_PATHS, color: COLORS.html },
  json: { paths: JSON_PATHS, color: COLORS.json },
  md: { paths: MD_PATHS, color: COLORS.md },
  py: { paths: PY_PATHS, color: COLORS.py },
  go: { paths: GO_PATHS, color: COLORS.go },
  rust: { paths: RUST_PATHS, color: COLORS.rust },
  config: { paths: CONFIG_PATHS, color: COLORS.config },
  shell: { paths: SHELL_PATHS, color: COLORS.shell },
  image: { paths: IMAGE_PATHS, color: COLORS.image },
  lock: { paths: LOCK_PATHS, color: COLORS.lock },
  zip: { paths: ZIP_PATHS, color: COLORS.zip },
  java: { paths: JAVA_PATHS, color: COLORS.java },
  ruby: { paths: RUBY_PATHS, color: COLORS.ruby },
  sql: { paths: SQL_PATHS, color: COLORS.sql },
  git: { paths: GIT_PATHS, color: COLORS.git },
};

// ─── 组件 ──────────────────────────────────────────────────────────────────────

interface FileIconProps {
  iconKey: string;
  size?: number;
  className?: string;
  expanded?: boolean;
}

export function FileIcon({ iconKey, size = 16, className, expanded }: FileIconProps): JSX.Element {
  // 文件夹打开时用不同图标
  const key = iconKey === "folder" && expanded ? "folder-open" : iconKey;
  const def = ICONS[key] || ICONS.file;
  const isFolder = key === "folder" || key === "folder-open";

  return (
    <svg
      className={className}
      width={size}
      height={size}
      viewBox="0 0 16 16"
      fill="none"
      xmlns="http://www.w3.org/2000/svg"
      style={{ flexShrink: 0 }}
    >
      {def.paths.map((d, i) => (
        <path
          key={i}
          d={d}
          fill={def.color}
          fillOpacity={isFolder ? 1 : i === 0 ? 0.2 : 1}
          fillRule={def.fillRule}
        />
      ))}
    </svg>
  );
}

/**
 * 获取图标颜色（用于非 SVG 场景的回退）
 */
export function getFileIconColor(iconKey: string): string {
  const def = ICONS[iconKey] || ICONS.file;
  return def.color;
}
