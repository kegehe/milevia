import { validateFileName } from "./file-model";

export const FILE_UPLOAD_LIMIT = 10 * 1024 * 1024;
// base64 膨胀后仍低于中继的 384 KiB 帧预算，为路径与信封留出余量。
export const MOBILE_FILE_UPLOAD_LIMIT = 256 * 1024;

export interface UploadResult {
  uploaded: string[];
  failures: { name: string; message: string }[];
}

interface UploadOptions {
  files: readonly File[];
  dirPath: string;
  endpoint: string;
  request: <T>(path: string, init?: RequestInit) => Promise<T>;
  mobile?: boolean;
  signal: AbortSignal;
  onProgress?: (completed: number) => void;
}

export async function uploadFiles({ files, dirPath, endpoint, request, mobile, signal, onProgress }: UploadOptions): Promise<UploadResult> {
  const result: UploadResult = { uploaded: [], failures: [] };
  const limit = mobile ? MOBILE_FILE_UPLOAD_LIMIT : FILE_UPLOAD_LIMIT;
  // 逐个读取和写入，避免多份 base64 常驻内存，也不争抢工作区锁。
  for (const [index, file] of files.entries()) {
    signal.throwIfAborted();
    try {
      const nameError = validateFileName(file.name);
      if (nameError) throw new Error(nameError);
      if (file.size > limit) {
        throw new Error(mobile ? "手机端单个文件最大支持 256 KiB，请在电脑端上传较大文件" : "单个文件最大支持 10 MiB");
      }
      const bytes = new Uint8Array(await file.arrayBuffer());
      signal.throwIfAborted();
      const chunks: string[] = [];
      for (let offset = 0; offset < bytes.length; offset += 0x8000) {
        chunks.push(String.fromCharCode(...bytes.subarray(offset, offset + 0x8000)));
      }
      const path = dirPath ? `${dirPath}/${file.name}` : file.name;
      await request(endpoint, {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ path, content: btoa(chunks.join("")), encoding: "base64", createOnly: true }),
        signal,
      });
      result.uploaded.push(path);
    } catch (error) {
      signal.throwIfAborted();
      result.failures.push({ name: file.name, message: error instanceof Error ? error.message : "上传失败" });
    }
    onProgress?.(index + 1);
  }
  return result;
}

/** 只接受系统文件，不拦截页面内的文字、链接拖动；目录不能冒充零字节文件上传。 */
export function droppedFiles(data: DataTransfer): { files: File[]; hasDirectories: boolean } {
  const items = Array.from(data.items ?? []).filter((item) => item.kind === "file");
  if (items.length === 0) return { files: Array.from(data.files), hasDirectories: false };
  const files: File[] = [];
  let hasDirectories = false;
  for (const item of items) {
    if (item.webkitGetAsEntry?.()?.isDirectory) {
      hasDirectories = true;
      continue;
    }
    const file = item.getAsFile();
    if (file) files.push(file);
  }
  return { files, hasDirectories };
}
