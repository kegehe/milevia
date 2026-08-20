import { useCallback, useEffect, useState } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { apiURL, isDesktop, sessionHeaders } from "../../lib/runtime";
import type { FileInfo } from "./file-model";
import { formatSize, getDirPath, isEditableFile } from "./file-model";
import type { FilePreviewKind } from "./source-language";
import { CodeFileView } from "./CodeFileView";
import { JsonViewer } from "./JsonViewer";
import { SqliteViewer } from "./SqliteViewer";
import { FileIcon } from "./FileIcon";

interface FileViewerProps {
  content: string;
  stat: FileInfo;
  previewKind: FilePreviewKind;
  projectId: string;
  request: <T>(path: string, init?: RequestInit) => Promise<T>;
  onEdit: () => void;
  readOnly: boolean;
  fontSize: number;
  onIncreaseFont: () => void;
  onDecreaseFont: () => void;
  canIncreaseFont: boolean;
  canDecreaseFont: boolean;
}

export function FileViewer({ content, stat, previewKind, projectId, request, onEdit, readOnly, fontSize, onIncreaseFont, onDecreaseFont, canIncreaseFont, canDecreaseFont }: FileViewerProps) {
  const [imageErrorPath, setImageErrorPath] = useState<string | null>(null);
  const [sqliteInvalidPath, setSqliteInvalidPath] = useState<string | null>(null);
  const imageError = imageErrorPath === stat.path;
  const sqliteInvalid = sqliteInvalidPath === stat.path;
  const handleImageError = useCallback(() => setImageErrorPath(stat.path), [stat.path]);
  const handleNotDatabase = useCallback(() => setSqliteInvalidPath(stat.path), [stat.path]);
  const activePreviewKind = previewKind === "sqlite" && sqliteInvalid ? "binary" : previewKind;
  const canEdit = isEditableFile(stat) && activePreviewKind !== "large" && activePreviewKind !== "sqlite";

  return (
    <div className="file-viewer text-viewer">
      <div className="file-viewer-toolbar">
        <span className="file-viewer-file-path" title={stat.path}>
          <FileIcon iconKey="file" size={15} />
          <span>{stat.path}</span>
        </span>
        <div className="file-viewer-actions">
          <div className="file-viewer-font-controls">
            <button type="button" className="file-viewer-font-btn" onClick={onDecreaseFont} disabled={!canDecreaseFont} title="缩小字号">A-</button>
            <span className="file-viewer-font-size">{fontSize}px</span>
            <button type="button" className="file-viewer-font-btn" onClick={onIncreaseFont} disabled={!canIncreaseFont} title="放大字号">A+</button>
          </div>
          {canEdit && !readOnly && <button type="button" className="file-viewer-edit-btn" onClick={onEdit}>编辑</button>}
          {canEdit && readOnly && <span className="file-viewer-readonly-hint">只读</span>}
        </div>
      </div>
      {activePreviewKind === "image" && <ImagePreview projectId={projectId} stat={stat} failed={imageError} onError={handleImageError} />}
      {activePreviewKind === "sqlite" && <SqliteViewer projectId={projectId} path={stat.path} request={request} onNotDatabase={handleNotDatabase} />}
      {activePreviewKind === "json" && <JsonViewer content={content} stat={stat} fontSize={fontSize} />}
      {activePreviewKind === "markdown" && <MarkdownPreview content={content} projectId={projectId} baseDir={getDirPath(stat.path)} fontSize={fontSize} />}
      {activePreviewKind === "source" && <CodeFileView content={content} filename={stat.name} fontSize={fontSize} />}
      {activePreviewKind === "large" && <FileMessage projectId={projectId} stat={stat} message="文本文件超过 10MB，无法在页面中打开。" />}
      {activePreviewKind === "binary" && <FileMessage projectId={projectId} stat={stat} message={sqliteInvalid ? "该文件不是有效的 SQLite 数据库，无法直接预览。" : "该文件是二进制文件，无法直接预览。"} />}
    </div>
  );
}

function ImagePreview({ projectId, stat, failed, onError }: { projectId: string; stat: FileInfo; failed: boolean; onError: () => void }) {
  const url = `/api/projects/${projectId}/fs/raw?path=${encodeURIComponent(stat.path)}`;
  if (failed) return <FileMessage projectId={projectId} stat={stat} message="图片加载失败。" />;
  return <div className="image-preview"><ProjectImage url={url} alt={stat.name} onError={onError} /></div>;
}

function FileMessage({ projectId, stat, message }: { projectId: string; stat: FileInfo; message: string }) {
  const url = `/api/projects/${projectId}/fs/download?path=${encodeURIComponent(stat.path)}`;
  return <div className="binary-info"><FileIcon iconKey="file" size={48} /><div className="binary-info-name">{stat.name}</div><div className="binary-info-details"><div>{message}</div><div>类型：{stat.mimeType || "未知"}</div><div>大小：{formatSize(stat.size)}</div><div>修改时间：{stat.modTime}</div></div><DownloadLink url={url} filename={stat.name} /></div>;
}

function MarkdownPreview({ content, projectId, baseDir, fontSize }: { content: string; projectId: string; baseDir: string; fontSize: number }) {
  return <div className="file-viewer-markdown markdown" style={{ fontSize: `${fontSize}px` }}><ReactMarkdown remarkPlugins={[remarkGfm]} skipHtml components={{ a: ({ href, children }) => <a href={safeHref(href)} {...(isExternal(href) ? { target: "_blank", rel: "noreferrer" } : {})}>{children}</a>, img: ({ src, alt }) => <MarkdownImage src={markdownImageUrl(src ?? "", baseDir, projectId)} alt={alt ?? ""} /> }}>{content}</ReactMarkdown></div>;
}

function MarkdownImage({ src, alt }: { src: string; alt: string }) {
  if (!src.startsWith("/api/")) return <img src={src} alt={alt} loading="lazy" />;
  return <ProjectImage url={src} alt={alt} loading="lazy" />;
}

function ProjectImage({ url, alt, onError, loading }: { url: string; alt: string; onError?: () => void; loading?: "eager" | "lazy" }) {
  const desktop = isDesktop();
  const [source, setSource] = useState<string | null>(() => desktop ? null : apiURL(url));

  useEffect(() => {
    if (!desktop) {
      setSource(apiURL(url));
      return;
    }
    setSource(null);
    let cancelled = false;
    let objectURL = "";
    const controller = new AbortController();
    void fetch(apiURL(url), { headers: sessionHeaders(), signal: controller.signal })
      .then(async (response) => {
        if (!response.ok) throw new Error(await responseError(response));
        return response.blob();
      })
      .then((blob) => {
        if (cancelled) return;
        objectURL = URL.createObjectURL(blob);
        setSource(objectURL);
      })
      .catch((error: unknown) => {
        if (!cancelled && !(error instanceof DOMException && error.name === "AbortError")) onError?.();
      });
    return () => {
      cancelled = true;
      controller.abort();
      if (objectURL) URL.revokeObjectURL(objectURL);
    };
  }, [desktop, onError, url]);

  return source ? <img src={source} alt={alt} loading={loading} onError={onError} /> : <span className="image-preview-loading" role="status" aria-label="图片加载中" />;
}

function DownloadLink({ url, filename }: { url: string; filename: string }) {
  const desktop = isDesktop();
  const [downloading, setDownloading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const download = async () => {
    if (downloading) return;
    setDownloading(true);
    setError(null);
    try {
      const target = new URL(url, window.location.origin);
      if (!target.pathname.endsWith("/download")) throw new Error("下载地址无效");
      const path = target.searchParams.get("path");
      if (!path) throw new Error("下载路径无效");
      const response = await fetch(apiURL(`${target.pathname.slice(0, -"/download".length)}/download-ticket`), {
        method: "POST",
        headers: sessionHeaders({ "Content-Type": "application/json" }),
        body: JSON.stringify({ path }),
      });
      if (!response.ok) throw new Error(await responseError(response));
      const ticket = await response.json() as { url?: string };
      if (!ticket.url) throw new Error("下载授权无效");
      const link = document.createElement("a");
      link.href = apiURL(ticket.url);
      link.download = filename;
      link.style.display = "none";
      document.body.append(link);
      link.click();
      link.remove();
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : "下载失败，请稍后重试。");
    } finally {
      setDownloading(false);
    }
  };

  if (!desktop) return <a className="binary-info-download" href={apiURL(url)} download={filename}>下载</a>;
  return <><button type="button" className="binary-info-download" onClick={() => void download()} disabled={downloading}>{downloading ? "下载中..." : "下载"}</button>{error && <div className="file-preview-message error" role="alert">下载失败：{error}</div>}</>;
}

async function responseError(response: Response): Promise<string> {
  const body = await response.json().catch(() => null);
  return typeof body?.error === "string" ? body.error : `请求失败（状态码 ${response.status}）`;
}

function safeHref(href: string | undefined): string | undefined {
  if (!href || !/^[a-z][a-z0-9+.-]*:/i.test(href) || /^(https?|ftp|mailto|tel):/i.test(href)) return href;
  return undefined;
}

function isExternal(href: string | undefined): boolean { return Boolean(href && /^(https?|ftp):|^\/\//i.test(href)); }

function markdownImageUrl(src: string, baseDir: string, projectId: string): string {
  if (/^(https?:|data:|\/\/)/i.test(src)) return src;
  const path = src.split(/[?#]/, 1)[0];
  const parts = baseDir.split("/").filter(Boolean);
  for (const part of path.split("/")) { if (!part || part === ".") continue; if (part === "..") parts.pop(); else parts.push(part); }
  return `/api/projects/${projectId}/fs/raw?path=${encodeURIComponent(parts.join("/"))}`;
}
