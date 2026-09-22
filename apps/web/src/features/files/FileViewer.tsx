import { useCallback, useEffect, useState } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { apiURL, isDesktop, sessionHeaders } from "../../lib/runtime";
import { markdownCodeComponents } from "../../components/MarkdownCodeBlock";
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
	conversationId?: string;
  request: <T>(path: string, init?: RequestInit) => Promise<T>;
  onEdit: () => void;
  readOnly: boolean;
  fontSize: number;
  onIncreaseFont: () => void;
  onDecreaseFont: () => void;
  canIncreaseFont: boolean;
  canDecreaseFont: boolean;
  /**
   * 手机端才传：图片字节要从中继取回来（见 `MobileFsRequest.resolveMedia`）。
   *
   * 桌面端不传，仍然走 `/fs/raw` 那条 URL 路径 —— 那是本机接口，浏览器直接导航过去
   * 就能拿到字节（session 头由 `ProjectImage` 自己补）。手机端两条都不成立：
   * `/fs/raw` 是一次浏览器导航带不了令牌，而手机的 WebView 也到不了电脑的本机端口。
   */
  media?: FileViewerMedia;
  /** 手机端不提供文件下载；为 true 时不给「下载」按钮，改成一句说明。 */
  disableDownload?: boolean;
  /**
   * 手机端布局：只读源码也要**软换行**。
   *
   * 查看与编辑必须一致 —— 只在编辑器里开换行的话，用户看完再点"编辑"，同一段代码
   * 会从"一行到底、右半边被裁掉"变成"折行显示"，位置全变。而 `CodeFileView` 的默认
   * 是不换行（桌面端要横向滚动看长行），所以这里必须显式传下去。
   */
  mobile?: boolean;
  /**
   * 服务端省掉了文件内容时的说明（文件太大 / 不是文本）。
   *
   * 非空时**只渲染元信息卡**，不看 `previewKind` —— 因为这时候根本没有内容可渲染，
   * 而 `previewKind` 是按扩展名算的，一个大 .ts 文件会算出 "source"，
   * 按它渲染就是一个空白编辑器，用户会以为文件是空的。
   */
  omittedMessage?: string;
  /**
   * 服务端判定的"这个文件能不能改"（只有手机端会给，桌面端为 undefined）。
   *
   * 必须在**服务端**判：能不能编辑取决于内容发不发得回去（要减掉 JSON 转义与中继信封的
   * 余量），客户端按扩展名自己判会多出一条 256–320 KiB 的"可编辑"带，用户改完按保存才失败。
   */
  editable?: boolean;
  /** `editable` 为 false 时的原因，用来在工具条上说清"为什么不能改"。 */
  readOnlyReason?: "file_too_large" | "binary_file";
}

/** 手机端的图片字节来源。 */
export interface FileViewerMedia {
  resolve(path: string): Promise<MediaResolution>;
}

export type MediaResolution =
  | { kind: "ready"; base64: string; mimeType: string }
  | { kind: "unavailable"; message: string };

export function FileViewer({ content, stat, previewKind, projectId, conversationId, request, onEdit, readOnly, fontSize, onIncreaseFont, onDecreaseFont, canIncreaseFont, canDecreaseFont, media, disableDownload, mobile, omittedMessage, editable, readOnlyReason }: FileViewerProps) {
  const [imageErrorPath, setImageErrorPath] = useState<string | null>(null);
  const [sqliteInvalidPath, setSqliteInvalidPath] = useState<string | null>(null);
  const imageError = imageErrorPath === stat.path;
  const sqliteInvalid = sqliteInvalidPath === stat.path;
  const handleImageError = useCallback(() => setImageErrorPath(stat.path), [stat.path]);
  const handleNotDatabase = useCallback(() => setSqliteInvalidPath(stat.path), [stat.path]);
  const activePreviewKind = previewKind === "sqlite" && sqliteInvalid ? "binary" : previewKind;
  // `editable === false` 是服务端的判定，优先级高于下面的本地判据：它知道内容能不能
  // 原样发回去，而 `isEditableFile` 只看扩展名与 isText。
  const serverReadOnly = editable === false;
  // 没有内容可渲染时，编辑按钮也不能亮：那会把用户带进一个空编辑器，
  // 而它一保存就把整个文件覆盖成空的。
  const canEdit = !omittedMessage && !serverReadOnly && isEditableFile(stat) && activePreviewKind !== "large" && activePreviewKind !== "sqlite";
  const readOnlyHint = readOnlyReason === "binary_file" ? "只读 · 非文本文件" : "只读 · 文件较大";


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
          {/* 服务端说不能改时，按钮根本不渲染 —— 所以那句解释必须**另起一支**，
              否则用户只看到一个没有「编辑」的工具条，看不出是文件太大还是别的什么。
              内容被省略时不再重复：正文那张元信息卡已经说清了。 */}
          {!canEdit && !omittedMessage && serverReadOnly && <span className="file-viewer-readonly-hint">{readOnlyHint}</span>}
        </div>
      </div>
      {omittedMessage !== undefined
        ? <FileMessage projectId={projectId} conversationId={conversationId} stat={stat} message={omittedMessage} disableDownload={disableDownload} />
        : <>
          {activePreviewKind === "image" && <ImagePreview projectId={projectId} conversationId={conversationId} media={media} disableDownload={disableDownload} stat={stat} failed={imageError} onError={handleImageError} />}
          {activePreviewKind === "sqlite" && <SqliteViewer projectId={projectId} conversationId={conversationId} path={stat.path} request={request} onNotDatabase={handleNotDatabase} />}
          {activePreviewKind === "json" && <JsonViewer content={content} stat={stat} fontSize={fontSize} />}
          {activePreviewKind === "markdown" && <MarkdownPreview content={content} projectId={projectId} conversationId={conversationId} baseDir={getDirPath(stat.path)} fontSize={fontSize} media={media} />}
          {activePreviewKind === "source" && <CodeFileView content={content} filename={stat.name} fontSize={fontSize} wrap={mobile} />}
          {activePreviewKind === "large" && <FileMessage projectId={projectId} conversationId={conversationId} stat={stat} disableDownload={disableDownload} message="文本文件超过 10MB，无法在页面中打开。" />}
          {activePreviewKind === "binary" && <FileMessage projectId={projectId} conversationId={conversationId} stat={stat} disableDownload={disableDownload} message={sqliteInvalid ? "该文件不是有效的 SQLite 数据库，无法直接预览。" : "该文件是二进制文件，无法直接预览。"} />}
        </>}
    </div>
  );
}

/**
 * 手机端：把中继取回来的字节变成 `<img>` 能用的地址。
 *
 * 用 object URL 而不是 `data:` —— CSP 经常会拦 `data:`，而且一长串 base64 留在
 * 元素属性里既费内存又会出现在 DOM 检查器里。object URL 必须在卸载时撤销，
 * 所以这件事只能由一个 React 组件做：适配器是纯模块，管不了生命周期。
 */
function useMobileMedia(media: FileViewerMedia | undefined, path: string): { url: string | null; message: string | null } {
  const [state, setState] = useState<{ url: string | null; message: string | null }>({ url: null, message: null });
  useEffect(() => {
    if (!media) return;
    let cancelled = false;
    let createdUrl = "";
    setState({ url: null, message: null });
    void media
      .resolve(path)
      .then((resolution) => {
        if (cancelled) return;
        if (resolution.kind === "unavailable") {
          setState({ url: null, message: resolution.message });
          return;
        }
        const url = URL.createObjectURL(base64ToBlob(resolution.base64, resolution.mimeType));
        if (cancelled) {
          URL.revokeObjectURL(url);
          return;
        }
        createdUrl = url;
        setState({ url, message: null });
      })
      .catch(() => {
        if (!cancelled) setState({ url: null, message: "读不到这个图片" });
      });
    return () => {
      cancelled = true;
      if (createdUrl) URL.revokeObjectURL(createdUrl);
    };
  }, [media, path]);
  return state;
}

function base64ToBlob(base64: string, mimeType: string): Blob {
  const binary = atob(base64);
  const bytes = new Uint8Array(binary.length);
  for (let index = 0; index < binary.length; index += 1) bytes[index] = binary.charCodeAt(index);
  return new Blob([bytes], { type: mimeType });
}

function ImagePreview({ projectId, conversationId, media, disableDownload, stat, failed, onError }: { projectId: string; conversationId?: string; media?: FileViewerMedia; disableDownload?: boolean; stat: FileInfo; failed: boolean; onError: () => void }) {
  const mobile = useMobileMedia(media, stat.path);
  if (media) {
    // 拿不到字节时把**原因**说出来（超过内联上限 / 不是图片 / 电脑离线），
    // 一句"图片加载失败"会让用户以为是文件坏了或者网络有问题。
    if (mobile.message) return <FileMessage projectId={projectId} conversationId={conversationId} stat={stat} disableDownload={disableDownload} message={mobile.message} />;
    if (!mobile.url) return <div className="image-preview"><span className="image-preview-loading" role="status" aria-label="图片加载中" /></div>;
    return <div className="image-preview"><img src={mobile.url} alt={stat.name} onError={onError} /></div>;
  }
  const url = `/api/projects/${projectId}/fs/raw?path=${encodeURIComponent(stat.path)}${conversationId ? `&conversationId=${encodeURIComponent(conversationId)}` : ""}`;
  if (failed) return <FileMessage projectId={projectId} conversationId={conversationId} stat={stat} message="图片加载失败。" />;
  return <div className="image-preview"><ProjectImage url={url} alt={stat.name} onError={onError} /></div>;
}

function FileMessage({ projectId, conversationId, stat, message, disableDownload }: { projectId: string; conversationId?: string; stat: FileInfo; message: string; disableDownload?: boolean }) {
  // 手机端连下载地址都不拼：那个 URL 在手机上没有任何一条路径能走通，
  // 摆在 DOM 里只会让人以为"点一下就能下载"。
  const url = disableDownload ? "" : `/api/projects/${projectId}/fs/download?path=${encodeURIComponent(stat.path)}${conversationId ? `&conversationId=${encodeURIComponent(conversationId)}` : ""}`;
  return <div className="binary-info"><FileIcon iconKey="file" size={48} /><div className="binary-info-name">{stat.name}</div><div className="binary-info-details"><div>{message}</div><div>类型：{stat.mimeType || "未知"}</div><div>大小：{formatSize(stat.size)}</div><div>修改时间：{stat.modTime}</div></div>{disableDownload ? <div className="binary-info-download-note">手机端不提供文件下载，请在电脑上获取。</div> : <DownloadLink url={url} filename={stat.name} />}</div>;
}

function MarkdownPreview({ content, projectId, conversationId, baseDir, fontSize, media }: { content: string; projectId: string; conversationId?: string; baseDir: string; fontSize: number; media?: FileViewerMedia }) {
  return <div className="file-viewer-markdown markdown" style={{ fontSize: `${fontSize}px` }}><ReactMarkdown remarkPlugins={[remarkGfm]} skipHtml components={{ ...markdownCodeComponents, a: ({ href, children }) => <a href={safeHref(href)} {...(isExternal(href) ? { target: "_blank", rel: "noreferrer" } : {})}>{children}</a>, img: ({ src, alt }) => <MarkdownImage src={src ?? ""} alt={alt ?? ""} baseDir={baseDir} projectId={projectId} conversationId={conversationId} media={media} /> }}>{content}</ReactMarkdown></div>;
}

function MarkdownImage({ src, alt, baseDir, projectId, conversationId, media }: { src: string; alt: string; baseDir: string; projectId: string; conversationId?: string; media?: FileViewerMedia }) {
  const relative = markdownImagePath(src, baseDir);
  // 外链（http/https/data/协议相对）由 <img> 直接加载，不经项目文件通道。
  if (relative === null) return <img src={src} alt={alt} loading="lazy" />;
  if (media) return <MarkdownProjectImage media={media} path={relative} alt={alt} />;
  return <ProjectImage url={rawFileUrl(projectId, relative, conversationId)} alt={alt} loading="lazy" />;
}

/**
 * Markdown 里的项目内插图（手机端）。
 *
 * 一张图读不出来**不能**把整篇文档带崩：拿不到就退回 alt 文本的占位，
 * 与桌面端 `<img>` 挂掉时的表现一致。
 */
function MarkdownProjectImage({ media, path, alt }: { media: FileViewerMedia; path: string; alt: string }) {
  const state = useMobileMedia(media, path);
  if (state.message) return <span className="markdown-image-unavailable" role="img" aria-label={alt}>{alt || "图片"}</span>;
  if (!state.url) return <span className="image-preview-loading" role="status" aria-label="图片加载中" />;
  return <img src={state.url} alt={alt} loading="lazy" />;
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

/**
 * 把 Markdown 里的图片地址折成**项目内相对路径**；返回 null 表示这个地址不该走
 * 项目文件通道（外链、data URI、或者本来就已经是绝对接口地址）。
 *
 * 折成相对路径而不是直接拼 URL：手机端要用同一个路径去中继取字节，
 * 两端必须从同一个值出发，否则"桌面能看到、手机看不到"会变成一个查不出来的差异。
 */
function markdownImagePath(src: string, baseDir: string): string | null {
  if (/^(https?:|data:|\/\/|\/api\/)/i.test(src)) return null;
  const path = src.split(/[?#]/, 1)[0];
  const parts = baseDir.split("/").filter(Boolean);
  for (const part of path.split("/")) { if (!part || part === ".") continue; if (part === "..") parts.pop(); else parts.push(part); }
  return parts.join("/");
}

function rawFileUrl(projectId: string, relativePath: string, conversationId?: string): string {
  return `/api/projects/${projectId}/fs/raw?path=${encodeURIComponent(relativePath)}${conversationId ? `&conversationId=${encodeURIComponent(conversationId)}` : ""}`;
}
