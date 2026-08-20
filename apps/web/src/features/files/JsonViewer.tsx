import { useMemo, useState } from "react";
import JSON5 from "json5";
import { parse as parseJSONC, printParseErrorCode, type ParseError } from "jsonc-parser";
import type { FileInfo } from "./file-model";
import { CodeFileView } from "./CodeFileView";

interface JsonViewerProps {
  content: string;
  stat: FileInfo;
  fontSize: number;
}

type ParseResult = { value: unknown } | { error: string };

export function JsonViewer({ content, stat, fontSize }: JsonViewerProps) {
  const [mode, setMode] = useState<"source" | "tree">("source");
  const parsed = useMemo(() => mode === "tree" ? parseJSONDocument(content, stat.name) : null, [content, mode, stat.name]);
  const isTooLarge = stat.size > 2 * 1024 * 1024;

  return (
    <div className="json-viewer">
      <div className="file-preview-mode" role="group" aria-label="JSON 查看模式">
        <button type="button" className={mode === "source" ? "active" : ""} onClick={() => setMode("source")}>源码</button>
        <button type="button" className={mode === "tree" ? "active" : ""} onClick={() => setMode("tree")} disabled={isTooLarge}>结构</button>
      </div>
      {mode === "source" ? (
        <CodeFileView content={content} filename={stat.name} fontSize={fontSize} />
      ) : isTooLarge ? (
        <div className="file-preview-message">文件超过 2MB，结构预览已禁用。</div>
      ) : parsed && "error" in parsed ? (
        <div className="file-preview-message error">JSON 解析失败：{parsed.error}</div>
      ) : parsed ? (
        <div className="json-tree" style={{ fontSize: `${fontSize}px` }}>
          <JsonNode label="根" value={parsed.value} path="$" depth={0} />
        </div>
      ) : null}
    </div>
  );
}

function parseJSONDocument(content: string, filename: string): ParseResult {
  const lower = filename.toLowerCase();
  try {
    if (lower.endsWith(".json5")) return { value: JSON5.parse(content) };
    if (lower.endsWith(".jsonc")) {
      const errors: ParseError[] = [];
      const value = parseJSONC(content, errors, { allowTrailingComma: true, disallowComments: false });
      if (errors.length > 0) {
        const first = errors[0];
        return { error: `${printParseErrorCode(first.error)}（位置 ${first.offset}）` };
      }
      return { value };
    }
    return { value: JSON.parse(content) };
  } catch (error) {
    return { error: error instanceof Error ? error.message : "无效内容" };
  }
}

function JsonNode({ label, value, path, depth }: { label: string; value: unknown; path: string; depth: number }) {
  const isContainer = Array.isArray(value) || (value !== null && typeof value === "object");
  const [expanded, setExpanded] = useState(depth === 0);
  const [visibleCount, setVisibleCount] = useState(100);

  if (!isContainer) {
    return <div className="json-node json-scalar"><span className="json-key">{label}</span><span className="json-separator">: </span><span className={`json-value ${jsonValueClass(value)}`}>{formatJSONValue(value)}</span></div>;
  }

  const entries = Array.isArray(value)
    ? value.map((item, index) => [String(index), item] as const)
    : Object.entries(value as Record<string, unknown>);
  const visibleEntries = entries.slice(0, visibleCount);
  const suffix = Array.isArray(value) ? `[${entries.length}]` : `{${entries.length}}`;

  return (
    <div className="json-node json-container">
      <div className="json-node-head">
        <button type="button" className="json-toggle" onClick={() => setExpanded((current) => !current)} aria-label={expanded ? "折叠" : "展开"}>{expanded ? "-" : "+"}</button>
        <span className="json-key">{label}</span><span className="json-separator">: </span><span className="json-summary">{suffix}</span>
        <button type="button" className="json-path-copy" onClick={() => void navigator.clipboard?.writeText(path)} title="复制 JSON Path">路径</button>
      </div>
      {expanded && (
        <div className="json-children">
          {visibleEntries.map(([key, child]) => <JsonNode key={key} label={Array.isArray(value) ? `[${key}]` : key} value={child} path={jsonChildPath(path, key, Array.isArray(value))} depth={depth + 1} />)}
          {visibleCount < entries.length && <button type="button" className="json-more" onClick={() => setVisibleCount((count) => count + 100)}>显示更多（剩余 {entries.length - visibleCount}）</button>}
        </div>
      )}
    </div>
  );
}

function jsonChildPath(parent: string, key: string, isArray: boolean): string {
  if (isArray) return `${parent}[${key}]`;
  return /^[A-Za-z_$][\w$]*$/.test(key) ? `${parent}.${key}` : `${parent}[${JSON.stringify(key)}]`;
}

function jsonValueClass(value: unknown): string {
  if (value === null) return "null";
  return typeof value;
}

function formatJSONValue(value: unknown): string {
  if (typeof value === "string") return JSON.stringify(value);
  return String(value);
}
