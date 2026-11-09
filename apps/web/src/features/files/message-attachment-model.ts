import type { MessageAttachment } from "../../lib/types";

// Match the server's path spelling before optimistic messages or drafts are
// compared. Keep '..' intact so the server can reject it rather than hide it.
export function normalizeAttachmentPath(path: string): string {
  const value = path.trim().replaceAll("\\", "/");
  const normalized = value.split("/").filter((part) => part !== "" && part !== ".").join("/");
  // Preserve an absolute prefix so the server rejects it rather than silently
  // treating an absolute filename as a different workspace-relative file.
  return value.startsWith("/") ? `/${normalized}` : normalized;
}

export function mergeMessageAttachments(...groups: readonly MessageAttachment[][]): MessageAttachment[] {
  const files = new Map<string, MessageAttachment>();
  for (const group of groups) for (const file of group) {
    const path = normalizeAttachmentPath(file.path);
    if (path && !files.has(path)) files.set(path, { ...file, path });
  }
  // Restoration must not silently discard files. The composer/server enforce
  // the send limits and let the user remove excess files if drafts are merged.
  return [...files.values()];
}

export function readAttachmentDraft(raw: string | null): MessageAttachment[] {
  try {
    const saved: unknown = JSON.parse(raw || "[]");
    return mergeMessageAttachments(Array.isArray(saved) ? saved.filter((item): item is MessageAttachment =>
      item !== null && typeof item === "object" && typeof item.path === "string" && item.path.length <= 1024 &&
      (item.size === undefined || (typeof item.size === "number" && Number.isFinite(item.size) && item.size >= 0)) &&
      (item.mediaType === undefined || typeof item.mediaType === "string")
    ) : []);
  } catch { return []; }
}

export function attachmentMessageBody(message: { content: string; attachments?: MessageAttachment[] }): string {
  return [message.content, ...(message.attachments || []).map((file) => `@${file.path}`)].filter(Boolean).join("\n\n");
}
