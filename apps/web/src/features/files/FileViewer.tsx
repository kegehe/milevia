import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Highlight, type Token } from "prism-react-renderer";
import type { PrismTheme } from "prism-react-renderer";
import type { FileInfo } from "./file-model";
import { detectLanguage, isImageFile, isEditableFile, formatSize } from "./file-model";
import { FileIcon } from "./FileIcon";

// ─── 自定义浅色主题（与网站绿色工作室主题一致）────────────────────────────

const studioLightTheme: PrismTheme = {
  plain: {
    color: "#19332c",
    backgroundColor: "#f7fbf8",
  },
  styles: [
    { types: ["comment", "prolog", "doctype", "cdata"], style: { color: "#6d8479", fontStyle: "italic" as const } },
    { types: ["punctuation"], style: { color: "#5e7f73" } },
    { types: ["namespace"], style: { color: "#31594f" } },
    { types: ["property", "tag", "boolean", "number", "constant", "symbol", "class-name"], style: { color: "#1f6055" } },
    { types: ["selector", "attr-name", "string", "char", "builtin"], style: { color: "#2c7567" } },
    { types: ["operator", "entity", "url"], style: { color: "#31594f" } },
    { types: ["atrule", "attr-value", "keyword"], style: { color: "#245d52" } },
    { types: ["function", "variable"], style: { color: "#19332c" } },
    { types: ["regex", "important"], style: { color: "#b64d3c" } },
    { types: ["inserted"], style: { color: "#277566" } },
    { types: ["deleted"], style: { color: "#a54a39" } },
  ],
};

// ─── 搜索匹配类型 ──────────────────────────────────────────────────────────

interface SearchMatch {
  /** 行号 (0-based) */
  line: number;
  /** 该行内匹配的起始列 (0-based) */
  from: number;
  /** 该行内匹配的结束列 (exclusive, 0-based) */
  to: number;
  /** 全局匹配序号 */
  index: number;
}

interface FileViewerProps {
  content: string;
  stat: FileInfo;
  projectId: string;
  onEdit: () => void;
  readOnly: boolean;
  fontSize?: number;
  onIncreaseFont?: () => void;
  onDecreaseFont?: () => void;
  canIncreaseFont?: boolean;
  canDecreaseFont?: boolean;
}

// ─── 工具函数 ───────────────────────────────────────────────────────────────

/** 在文本中查找所有匹配位置，返回按行组织的匹配列表 */
function computeMatches(content: string, query: string): SearchMatch[] {
  if (!query) return [];
  const escaped = query.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const regex = new RegExp(escaped, "gi");
  const matches: SearchMatch[] = [];

  // 预计算每行的起始偏移
  const lineStarts: number[] = [0];
  for (let i = 0; i < content.length; i++) {
    if (content[i] === "\n") {
      lineStarts.push(i + 1);
    }
  }

  let match: RegExpExecArray | null;
  let globalIndex = 0;
  while ((match = regex.exec(content)) !== null) {
    const offset = match.index;
    // 用二分查找确定行号
    let lo = 0, hi = lineStarts.length - 1;
    while (lo <= hi) {
      const mid = (lo + hi) >>> 1;
      if (lineStarts[mid] <= offset) lo = mid + 1;
      else hi = mid - 1;
    }
    const line = hi;
    const lineStart = lineStarts[line];
    const from = offset - lineStart;
    const to = from + match[0].length;
    matches.push({ line, from, to, index: globalIndex++ });
  }
  return matches;
}

/** 按行分组匹配 */
function groupMatchesByLine(matches: SearchMatch[]): Map<number, SearchMatch[]> {
  const map = new Map<number, SearchMatch[]>();
  for (const m of matches) {
    const arr = map.get(m.line);
    if (arr) arr.push(m);
    else map.set(m.line, [m]);
  }
  return map;
}

export function FileViewer({
  content,
  stat,
  projectId,
  onEdit,
  readOnly,
  fontSize = 13,
  onIncreaseFont,
  onDecreaseFont,
  canIncreaseFont = true,
  canDecreaseFont = true,
}: FileViewerProps) {
  const codeRef = useRef<HTMLPreElement>(null);
  const searchInputRef = useRef<HTMLInputElement>(null);
  const [imgError, setImgError] = useState(false);
  const [imgLoading, setImgLoading] = useState(true);
  const language = detectLanguage(stat.name);

  // ─── 搜索状态 ─────────────────────────────────────────────────────────

  const [showSearch, setShowSearch] = useState(false);
  const [searchQuery, setSearchQuery] = useState("");
  const [currentMatchIndex, setCurrentMatchIndex] = useState(0);

  const allMatches = useMemo(
    () => computeMatches(content, searchQuery),
    [content, searchQuery]
  );

  const matchesByLine = useMemo(
    () => groupMatchesByLine(allMatches),
    [allMatches]
  );

  // 搜索词或内容变化时重置当前索引
  useEffect(() => {
    setCurrentMatchIndex(0);
  }, [searchQuery, content]);

  // 当 searchQuery 非空且有匹配时，滚动到第一个匹配
  useEffect(() => {
    if (allMatches.length > 0 && searchQuery) {
      requestAnimationFrame(() => {
        const lineEl = codeRef.current?.querySelector(
          `.code-line:nth-child(${allMatches[0].line + 1})`
        );
        if (lineEl) {
          lineEl.scrollIntoView({ block: "center", behavior: "smooth" });
        }
      });
    }
  }, [allMatches, searchQuery]);

  // 打开搜索栏时聚焦输入框并选中全部文本
  useEffect(() => {
    if (showSearch) {
      requestAnimationFrame(() => {
        requestAnimationFrame(() => {
          searchInputRef.current?.focus();
          searchInputRef.current?.select();
        });
      });
    }
  }, [showSearch]);

  // ─── 导航 ─────────────────────────────────────────────────────────────

  const scrollToMatch = useCallback((matchIndex: number) => {
    const match = allMatches[matchIndex];
    if (!match || !codeRef.current) return;
    // 找到对应行 DOM 元素
    const lineEl = codeRef.current.querySelector(
      `.code-line:nth-child(${match.line + 1})`
    );
    if (lineEl) {
      lineEl.scrollIntoView({ block: "center", behavior: "smooth" });
    }
  }, [allMatches]);

  const goToNext = useCallback(() => {
    if (allMatches.length === 0) return;
    const next = (currentMatchIndex + 1) % allMatches.length;
    setCurrentMatchIndex(next);
    scrollToMatch(next);
  }, [allMatches, currentMatchIndex, scrollToMatch]);

  const goToPrev = useCallback(() => {
    if (allMatches.length === 0) return;
    const prev = (currentMatchIndex - 1 + allMatches.length) % allMatches.length;
    setCurrentMatchIndex(prev);
    scrollToMatch(prev);
  }, [allMatches, currentMatchIndex, scrollToMatch]);

  const closeSearch = useCallback(() => {
    setShowSearch(false);
    setSearchQuery("");
  }, []);

  // ─── 快捷键 ───────────────────────────────────────────────────────────

  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      // Ctrl/Cmd+F：仅在文本文件时打开搜索
      if ((e.ctrlKey || e.metaKey) && e.key === "f") {
        if (!stat.isText || isImageFile(stat.mimeType)) return;
        e.preventDefault();
        if (!showSearch) {
          setShowSearch(true);
        } else {
          // 已打开：聚焦并选中全部文本
          searchInputRef.current?.focus();
          searchInputRef.current?.select();
        }
        return;
      }
      // Escape：关闭搜索
      if (e.key === "Escape" && showSearch) {
        e.preventDefault();
        closeSearch();
      }
    };
    document.addEventListener("keydown", handler);
    return () => document.removeEventListener("keydown", handler);
  }, [closeSearch, showSearch, stat.isText, stat.mimeType]);

  // 搜索栏内 Enter / Shift+Enter 处理
  const handleSearchKeyDown = useCallback((e: React.KeyboardEvent) => {
    if (e.key === "Enter") {
      e.preventDefault();
      if (e.shiftKey) goToPrev();
      else goToNext();
    }
    if (e.key === "Escape") {
      e.preventDefault();
      e.stopPropagation();
      // 阻止原生事件冒泡到 document listener 中
      e.nativeEvent.stopImmediatePropagation();
      closeSearch();
      // 让输入框失去焦点，防止浏览器默认行为
      (e.target as HTMLInputElement)?.blur();
    }
  }, [goToNext, goToPrev, closeSearch]);

  // ─── 切换图片文件时重置加载状态 ───────────────────────────────────────

  useEffect(() => {
    if (isImageFile(stat.mimeType)) {
      setImgError(false);
      setImgLoading(true);
      // 切换文件时也重置搜索状态
      setShowSearch(false);
      setSearchQuery("");
    }
  }, [stat.path, stat.mimeType]);

  // ─── 图片预览 ─────────────────────────────────────────────────────────

  if (isImageFile(stat.mimeType)) {
    const src = `/api/projects/${projectId}/fs/raw?path=${encodeURIComponent(stat.path)}`;
    return (
      <div className="file-viewer image-preview">
        {imgLoading && !imgError && <div className="image-preview-loading">加载中...</div>}
        {imgError ? (
          <div className="image-preview-error">
            <div>⚠️ 图片加载失败</div>
            <a className="binary-info-download" href={src} download={stat.name}>
              下载
            </a>
          </div>
        ) : (
          <img
            src={src}
            alt={stat.name}
            onLoad={() => setImgLoading(false)}
            onError={() => { setImgLoading(false); setImgError(true); }}
            style={imgLoading ? { position: "absolute", width: "1px", height: "1px", opacity: 0, pointerEvents: "none", overflow: "hidden" } : undefined}
          />
        )}
      </div>
    );
  }

  // ─── 二进制文件 ───────────────────────────────────────────────────────

  if (!stat.isText) {
    return (
      <div className="file-viewer binary-info">
        <FileIcon iconKey="file" size={48} />
        <div className="binary-info-name">{stat.name}</div>
        <div className="binary-info-details">
          <div>类型：{stat.mimeType || "未知"}</div>
          <div>大小：{formatSize(stat.size)}</div>
          <div>修改时间：{stat.modTime}</div>
          <div>权限：{stat.mode}</div>
        </div>
        <a
          className="binary-info-download"
          href={`/api/projects/${projectId}/fs/raw?path=${encodeURIComponent(stat.path)}`}
          download={stat.name}
        >
          下载
        </a>
      </div>
    );
  }

  // ─── 文本文件：prism 语法高亮 + 搜索 ──────────────────────────────────

  return (
    <div className="file-viewer text-viewer">
      <div className="file-viewer-toolbar">
        <span className="file-viewer-file-path" title={stat.path}><FileIcon iconKey="file" size={15} /><span>{stat.path}</span></span>
        <div className="file-viewer-actions">
          <div className="file-viewer-font-controls">
          <button
            className="file-viewer-font-btn"
            onClick={onDecreaseFont}
            disabled={!canDecreaseFont}
            title="缩小字号"
            aria-label="缩小字号"
          >
            A−
          </button>
          <span className="file-viewer-font-size">{fontSize}px</span>
          <button
            className="file-viewer-font-btn"
            onClick={onIncreaseFont}
            disabled={!canIncreaseFont}
            title="放大字号"
            aria-label="放大字号"
          >
            A+
          </button>
          </div>
          {isEditableFile(stat) && !readOnly && <button className="file-viewer-edit-btn" onClick={onEdit}>编辑</button>}
          {readOnly && isEditableFile(stat) && <span className="file-viewer-readonly-hint" title="AI 正在运行中，文件编辑已锁定">只读</span>}
        </div>
      </div>

      {/* ─── 搜索栏 ──────────────────────────────────────────────────── */}
      {showSearch && (
        <div className="file-viewer-search-bar">
          <div className="file-viewer-search-row">
            <div className="file-viewer-search-input-wrap">
              <svg className="file-viewer-search-icon" viewBox="0 0 16 16" aria-hidden="true" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round">
                <circle cx="6.5" cy="6.5" r="4.5" />
                <path d="m14 14-3.5-3.5" />
              </svg>
              <input
                ref={searchInputRef}
                className="file-viewer-search-input"
                type="text"
                value={searchQuery}
                onChange={(e) => setSearchQuery(e.target.value)}
                onKeyDown={handleSearchKeyDown}
                placeholder="搜索..."
                aria-label="搜索文件内容"
              />
            </div>
            <span className="file-viewer-search-count">
              {searchQuery ? (
                allMatches.length > 0
                  ? `${currentMatchIndex + 1} / ${allMatches.length}`
                  : "无匹配"
              ) : null}
            </span>
            <button
              className="file-viewer-search-nav-btn"
              onClick={goToPrev}
              disabled={allMatches.length === 0}
              title="上一个 (Shift+Enter)"
              aria-label="上一个匹配"
            >
              <svg viewBox="0 0 16 16" aria-hidden="true"><path d="m4 10 4-4 4 4" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" /></svg>
            </button>
            <button
              className="file-viewer-search-nav-btn"
              onClick={goToNext}
              disabled={allMatches.length === 0}
              title="下一个 (Enter)"
              aria-label="下一个匹配"
            >
              <svg viewBox="0 0 16 16" aria-hidden="true"><path d="m4 6 4 4 4-4" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" /></svg>
            </button>
            <button
              className="file-viewer-search-close"
              onClick={closeSearch}
              title="关闭 (Escape)"
              aria-label="关闭搜索"
            >
              ×
            </button>
          </div>
        </div>
      )}

      <Highlight theme={studioLightTheme} code={content} language={language}>
        {({ style, tokens, getLineProps, getTokenProps }) => (
          <pre className="file-viewer-code" style={{ ...style, fontSize: `${fontSize}px` }} ref={codeRef}>
            {tokens.map((line, i) => {
              const lineProps = getLineProps({ line, key: i });
              const lineMatches = matchesByLine.get(i);

              return (
                <div {...lineProps} key={i} className={`${lineProps.className} code-line`} data-line={i}>
                  <span className="line-number">{i + 1}</span>
                  <span className="line-content">
                    {lineMatches && lineMatches.length > 0
                      ? renderLineWithHighlights(line, getTokenProps, lineMatches, currentMatchIndex)
                      : line.map((token, j) => (
                          <span key={j} {...getTokenProps({ token, key: j })} />
                        ))}
                  </span>
                </div>
              );
            })}
          </pre>
        )}
      </Highlight>
    </div>
  );
}

// ─── 带高亮的行渲染 ────────────────────────────────────────────────────────

/**
 * 渲染带有搜索高亮的代码行。
 * 由于 prism token 可能跨越匹配边界，这里核心逻辑是：
 * 1. 获取整行文本字符串
 * 2. 遍历每个 token，追踪当前行内字符偏移
 * 3. 当 token 与某个匹配区域重叠时，在重叠区间内包裹 <mark>
 * 4. 当前激活的匹配使用额外的 CSS class
 */
function renderLineWithHighlights(
  line: Token[],
  getTokenProps: (props: { token: Token; key: number | string }) => Record<string, unknown>,
  lineMatches: SearchMatch[],
  currentMatchIndex: number
): React.ReactNode[] {
  // 合并排序匹配区间（同一个行内可能有紧邻的匹配）
  const sorted = [...lineMatches].sort((a, b) => a.from - b.from);

  const nodes: React.ReactNode[] = [];
  let charPos = 0; // 当前行内光标位置
  let matchIdx = 0; // sorted 数组中的当前指针

  for (let j = 0; j < line.length; j++) {
    const token = line[j];
    const tokenLen = token.content.length;
    const tokenStart = charPos;
    const tokenEnd = tokenStart + tokenLen;

    // 找出所有与当前 token 重叠的匹配
    const overlaps: SearchMatch[] = [];
    while (matchIdx < sorted.length && sorted[matchIdx].from < tokenEnd) {
      const m = sorted[matchIdx];
      if (m.to > tokenStart) {
        overlaps.push(m);
      }
      if (m.to <= tokenEnd) {
        matchIdx++;
      } else {
        // 匹配跨越到下一个 token，保留此 match 指针
        break;
      }
    }

    if (overlaps.length === 0) {
      // 无匹配：直接渲染原始 token
      nodes.push(<span key={j} {...getTokenProps({ token, key: j })} />);
    } else {
      // 有重叠：需要拆分为 文本片段 + mark 片段
      const fragments: React.ReactNode[] = [];
      let fragStart = tokenStart;

      for (const m of overlaps) {
        const matchStart = Math.max(m.from, tokenStart);
        const matchEnd = Math.min(m.to, tokenEnd);

        // 匹配前的不高亮部分
        if (fragStart < matchStart) {
          fragments.push(
            <span key={`${j}_${m.index}_pre_${fragStart}`}>
              {token.content.slice(fragStart - tokenStart, matchStart - tokenStart)}
            </span>
          );
        }

        // 高亮部分
        const isCurrent = m.index === currentMatchIndex;
        fragments.push(
          <mark
            key={`${j}_${m.index}_hl`}
            className={`search-highlight${isCurrent ? " current" : ""}`}
            data-match-index={m.index}
          >
            {token.content.slice(matchStart - tokenStart, matchEnd - tokenStart)}
          </mark>
        );

        fragStart = matchEnd;
      }

      // 最后的剩余部分
      if (fragStart < tokenEnd) {
        fragments.push(
          <span key={`${j}_post_${fragStart}`}>
            {token.content.slice(fragStart - tokenStart)}
          </span>
        );
      }

      nodes.push(
        <span key={j} {...{ className: (getTokenProps({ token, key: j }) as Record<string, string>)?.className }}>
          {fragments}
        </span>
      );
    }

    charPos = tokenEnd;
  }

  return nodes;
}
