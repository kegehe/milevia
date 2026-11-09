import { useEffect, useRef, useState } from "react";
import type { MessageAttachment } from "../../lib/types";
import { uploadFiles } from "./file-upload";
import { normalizeAttachmentPath } from "./message-attachment-model";

type Props = {
  projectID: string;
  disabled: boolean;
  mobile?: boolean;
  attachments: MessageAttachment[];
  request: <T>(path: string, init?: RequestInit) => Promise<T>;
  onAdd: (attachments: MessageAttachment[]) => void;
  onBusy: (busy: boolean) => void;
  onFail: (message: string) => void;
};

export async function uploadMessageFiles(files: File[], projectID: string, request: Props["request"], mobile = false, signal = new AbortController().signal, onFail?: (message: string) => void): Promise<MessageAttachment[]> {
  signal.throwIfAborted();
  if (files.length > 8) throw new Error("每条消息最多添加 8 个附件");
  if (files.reduce((sum, file) => sum + file.size, 0) > 20 * 1024 * 1024) throw new Error("附件总计不能超过 20 MiB");
  const base = `.milevia-attachments`;
  // LAN HTTP may lack randomUUID. This directory ID is not an auth token;
  // createOnly writes still reject accidental filename collisions.
  const id = globalThis.crypto?.randomUUID?.() ?? `upload-${Date.now()}-${Math.random().toString(36).slice(2)}-${Math.random().toString(36).slice(2)}`;
  const dir = `${base}/${id}`;
  const endpoint = `/api/projects/${projectID}/fs`;
  // The filesystem endpoints enforce the conversation workspace and write lease.
  await request(`${endpoint}/mkdir`, { method: "POST", body: JSON.stringify({ path: base }), signal });
  await request(`${endpoint}/mkdir`, { method: "POST", body: JSON.stringify({ path: dir }), signal });
  const result = await uploadFiles({ files, dirPath: dir, endpoint: `${endpoint}/write`, request, mobile, signal });
  signal.throwIfAborted();
  if (result.failures.length) {
    const message = result.failures.map((failure) => `${failure.name}：${failure.message}`).join("；");
    if (!result.uploaded.length) throw new Error(message);
    onFail?.(message);
  }
  return result.uploaded.map((path) => {
    const file = files.find((file) => path === `${dir}/${file.name}`);
    return { path, mediaType: file?.type, size: file?.size };
  });
}

export function MessageAttachmentPicker({ projectID, disabled, attachments, request, onAdd, onFail, onBusy, mobile = false }: Props) {
  const input = useRef<HTMLInputElement>(null);
  const controller = useRef(new AbortController());
  const busyRef = useRef(false);
  useEffect(() => {
    const current = new AbortController(); controller.current = current;
    return () => { current.abort(); onBusy(false); };
  }, []);
  const [busy, setBusy] = useState(false);
  const changeBusy = (value: boolean) => { busyRef.current = value; setBusy(value); onBusy(value); };
  const [path, setPath] = useState("");
  const [open, setOpen] = useState(false);
  const addPath = async () => {
    if (!path.trim() || disabled || busyRef.current) return;
    const current = controller.current;
    changeBusy(true);
    try {
      const info = await request<{ isDir: boolean; mimeType: string; size: number }>(`/api/projects/${projectID}/fs/stat?path=${encodeURIComponent(path.trim())}`, { signal: current.signal });
      if (info.isDir) throw new Error("请选择文件");
      if (attachments.length >= 8) throw new Error("每条消息最多添加 8 个附件");
      if (info.size > 10 * 1024 * 1024 || info.size + attachments.reduce((sum, a) => sum + (a.size || 0), 0) > 20 * 1024 * 1024) throw new Error("单个附件最多 10 MiB，总计最多 20 MiB");
      if (current.signal.aborted) return;
      onAdd([{ path: normalizeAttachmentPath(path), mediaType: info.mimeType, size: info.size }]);
      setPath(""); setOpen(false);
    } catch (cause) { if (!current.signal.aborted) onFail(cause instanceof Error ? cause.message : "附件添加失败"); }
    finally { if (!current.signal.aborted) changeBusy(false); }
  };
  return <span className="message-attachment-picker">
    <button type="button" className="composer-icon-btn" title="添加图片或文件附件" aria-label="添加附件" disabled={disabled || busy} onClick={() => setOpen(!open)}><svg className="composer-action-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="m8 12 6-6a3 3 0 0 1 4 4l-8 8a5 5 0 0 1-7-7l8-8" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" /></svg></button>
    <input ref={input} type="file" multiple hidden disabled={disabled || busy} onChange={async (event) => {
      const files = Array.from(event.target.files ?? []); event.target.value = "";
      if (!files.length || disabled || busyRef.current) return;
      const current = controller.current;
      changeBusy(true);
      try {
        if (files.length + attachments.length > 8) throw new Error("每条消息最多添加 8 个附件");
        if (files.reduce((sum, file) => sum + file.size, 0) + attachments.reduce((sum, a) => sum + (a.size || 0), 0) > 20 * 1024 * 1024) throw new Error("附件总计不能超过 20 MiB");
        const added = await uploadMessageFiles(files, projectID, request, mobile, current.signal, onFail);
        if (!current.signal.aborted) { onAdd(added); setOpen(false); }
      } catch (cause) { if (!current.signal.aborted) onFail(cause instanceof Error ? cause.message : "附件上传失败"); }
      finally { if (!current.signal.aborted) changeBusy(false); }
    }} />
    {open && <span className="message-attachment-menu" role="group" aria-label="添加附件">
      <button type="button" disabled={disabled || busy} onClick={() => input.current?.click()}>{busy ? "上传中…" : "上传图片 / 文件"}</button>
      <label>或填写工作区相对路径<input aria-label="附件路径" disabled={disabled || busy} value={path} onChange={(event) => setPath(event.target.value)} placeholder="例如 docs/design.png" onKeyDown={(event) => { if (event.key === "Enter") { event.preventDefault(); void addPath(); } }} /></label>
      <button type="button" disabled={disabled || busy || !path.trim()} onClick={() => void addPath()}>引用文件</button>
      <small>上传的附件保存在当前工作区，可在文件页管理。</small>
      <small>{mobile ? "手机上传单个最多 256 KiB；" : "单个最多 10 MiB，总计 20 MiB；"}Claude / CodeBuddy 对不超过 5 MiB 的 PNG、JPEG、GIF、WebP 使用图片输入，其他文件通过路径引用。</small>
    </span>}
  </span>;
}
