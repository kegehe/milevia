import { type JSX } from "react";

const ICONS = {
  folder: { color: "#2c7567" },
  "folder-open": { color: "#245d52" },
  file: { color: "#4f786d", label: "" },
  ts: { color: "#2c7567", label: "TS" },
  js: { color: "#806d24", label: "JS" },
  css: { color: "#58706e", label: "CS" },
  html: { color: "#9b6648", label: "HT" },
  json: { color: "#2c7567", label: "{}" },
  md: { color: "#347b71", label: "M" },
  py: { color: "#427265", label: "PY" },
  go: { color: "#3e7a83", label: "GO" },
  rust: { color: "#a4583c", label: "RS" },
  config: { color: "#52785e", label: "C" },
  shell: { color: "#52785e", label: ">_" },
  image: { color: "#8a5d7f", label: "I" },
  lock: { color: "#b65041", label: "L" },
  zip: { color: "#8b6a3d", label: "Z" },
  java: { color: "#9b6b35", label: "J" },
  ruby: { color: "#a0504b", label: "R" },
  sql: { color: "#8a6d25", label: "DB" },
  git: { color: "#b55745", label: "G" },
} as const;

interface FileIconProps {
  iconKey: string;
  size?: number;
  className?: string;
  expanded?: boolean;
}

function FolderIcon({ color, open }: { color: string; open: boolean }) {
  if (open) {
    return <>
      <path d="M3.5 7V5.8c0-.9.7-1.6 1.6-1.6h3.6l1.6 1.8h7.6c.9 0 1.6.7 1.6 1.6v2.1" fill="#edf8f1" stroke={color} strokeWidth="1.5" strokeLinejoin="round" />
      <path d="M3.1 9.2c.2-.8.9-1.4 1.8-1.4h14.3c1.1 0 1.8 1 1.5 2L19 17.1c-.2.8-.9 1.4-1.8 1.4H4.6c-1 0-1.7-.9-1.5-1.9l.5-7.4Z" fill="#d9eee0" stroke={color} strokeWidth="1.5" strokeLinejoin="round" />
    </>;
  }

  return <path d="M3.5 6.2c0-1 .8-1.8 1.8-1.8h3.4l1.8 2h7.2c1 0 1.8.8 1.8 1.8v8.1c0 1-.8 1.8-1.8 1.8H5.3c-1 0-1.8-.8-1.8-1.8V6.2Z" fill="#e5f4ea" stroke={color} strokeWidth="1.5" strokeLinejoin="round" />;
}

function DocumentIcon({ color, label }: { color: string; label: string }) {
  return <>
    <path d="M6 3.2h6.8L17.5 8v9.8c0 .8-.7 1.5-1.5 1.5H6c-.8 0-1.5-.7-1.5-1.5V4.7c0-.8.7-1.5 1.5-1.5Z" fill="#fbfefb" stroke={color} strokeWidth="1.5" strokeLinejoin="round" />
    <path d="M12.7 3.4V8h4.5" fill="none" stroke={color} strokeWidth="1.5" strokeLinejoin="round" />
    {label && <text x="11" y="15.5" fill={color} fontSize={label.length > 1 ? "5.1" : "6.2"} fontWeight="800" textAnchor="middle">{label}</text>}
  </>;
}

export function FileIcon({ iconKey, size = 16, className, expanded }: FileIconProps): JSX.Element {
  const key = iconKey === "folder" && expanded ? "folder-open" : iconKey;
  const definition = ICONS[key as keyof typeof ICONS] || ICONS.file;
  const isFolder = key === "folder" || key === "folder-open";
  const label = "label" in definition ? definition.label : "";

  return <svg className={["file-icon", className].filter(Boolean).join(" ")} width={size} height={size} viewBox="0 0 24 24" aria-hidden="true" style={{ flexShrink: 0 }}>
    {isFolder ? <FolderIcon color={definition.color} open={key === "folder-open"} /> : <DocumentIcon color={definition.color} label={label} />}
  </svg>;
}

export function getFileIconColor(iconKey: string): string {
  return (ICONS[iconKey as keyof typeof ICONS] || ICONS.file).color;
}
