import { useEffect, useRef, useState } from "react";
import { Highlight } from "prism-react-renderer";
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
  const [imgError, setImgError] = useState(false);
  const [imgLoading, setImgLoading] = useState(true);
  const language = detectLanguage(stat.name);

  // 切换图片文件时重置加载状态（避免残留旧图片的错误/加载态）
  useEffect(() => {
    if (isImageFile(stat.mimeType)) {
      setImgError(false);
      setImgLoading(true);
    }
  }, [stat.path, stat.mimeType]);

  // 图片预览
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

  // 二进制文件
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

  // 文本文件：prism 语法高亮
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
      <Highlight theme={studioLightTheme} code={content} language={language}>
        {({ style, tokens, getLineProps, getTokenProps }) => (
          <pre className="file-viewer-code" style={{ ...style, fontSize: `${fontSize}px` }} ref={codeRef}>
            {tokens.map((line, i) => {
              const lineProps = getLineProps({ line, key: i });
              return (
                <div {...lineProps} key={i} className={`${lineProps.className} code-line`}>
                  <span className="line-number">{i + 1}</span>
                  <span className="line-content">
                    {line.map((token, j) => (
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
