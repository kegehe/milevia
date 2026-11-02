// 任务导出文件的解析、预览与文件名生成（docs/50）。
//
// 这里的校验清单必须与服务端 normalizeTaskTransferFile 一一对应：前端拦得住，
// 服务端 400 就只是兜底；两边漂开时用户会拿到一条没有上下文的固定短句。
//
// 成环检测同样在内存里做 Kahn，并且只统计**真正会建立的边**——源状态为 done 的前置
// 在导入时不会重建依赖（否则后继永远无法下发），把它算进环会误报。

import type { TaskExportFile, TaskExportItem } from "../features/tasks/task-model";

export const TASK_TRANSFER_FORMAT = "milevia.tasks";
export const TASK_TRANSFER_VERSION = 1;
export const TASK_TRANSFER_MAX_TASKS = 2000;
export const TASK_TRANSFER_MAX_BYTES = 64 * 1024 * 1024;
export const TASK_TRANSFER_DESCRIPTION_LIMIT = 12000;

const KEY_PATTERN = /^[A-Za-z0-9_-]{1,64}$/;

export type TaskTransferParseResult =
  | { ok: true; file: TaskExportFile }
  | { ok: false; reason: string };

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function descriptionLength(description: string): number {
  // 与 Go 的 []rune 对齐：按 code point 计数，而不是 UTF-16 单元。
  return Array.from(description).length;
}

function invalidKeySet(tasks: TaskExportItem[]): Set<string> {
  const invalid = new Set<string>();
  for (const task of tasks) {
    const description = (task.description || "").trim();
    if (!description || descriptionLength(description) > TASK_TRANSFER_DESCRIPTION_LIMIT) invalid.add(task.key);
  }
  return invalid;
}

function hasDependencyCycle(tasks: TaskExportItem[], invalid: Set<string>): boolean {
  const done = new Set<string>();
  const indegree = new Map<string, number>();
  const edges = new Map<string, string[]>();
  for (const task of tasks) {
    if (task.status === "done") done.add(task.key);
    if (!invalid.has(task.key)) indegree.set(task.key, 0);
  }
  for (const task of tasks) {
    if (invalid.has(task.key)) continue;
    for (const predecessor of task.dependsOn || []) {
      if (invalid.has(predecessor) || done.has(predecessor)) continue;
      const list = edges.get(predecessor);
      if (list) list.push(task.key);
      else edges.set(predecessor, [task.key]);
      indegree.set(task.key, (indegree.get(task.key) || 0) + 1);
    }
  }
  const queue = [...indegree.entries()].filter(([, degree]) => degree === 0).map(([key]) => key);
  let visited = 0;
  while (queue.length > 0) {
    const key = queue.shift() as string;
    visited++;
    for (const next of edges.get(key) || []) {
      const degree = (indegree.get(next) || 0) - 1;
      indegree.set(next, degree);
      if (degree === 0) queue.push(next);
    }
  }
  return visited !== indegree.size;
}

// normalizeTransferTimestamp 把任何 Date 能解析的时间变成 RFC3339（毫秒精度）。
// 精度从纳秒降到毫秒只影响展示，换来的是「前端放行 = 服务端一定能解码」。
function normalizeTransferTimestamp(value: unknown): string | undefined {
  if (typeof value !== "string") return undefined;
  const epoch = Date.parse(value);
  if (Number.isNaN(epoch)) return undefined;
  return new Date(epoch).toISOString();
}

export function parseTaskTransferFile(text: string): TaskTransferParseResult {
  // 记事本「另存为」会带 UTF-8 BOM，JSON.parse 会直接抛错。
  const raw = (text || "").replace(/^\uFEFF/, "");
  if (!raw.trim()) return { ok: false, reason: "文件是空的。" };
  // 只认 NUL：UTF-16 文本被当 UTF-8 解码时必然出现 \u0000。不能用 U+FFFD 判断——
  // 任务说明里真的含替换字符（或被别的工具写坏一条）时，整个文件会被误判成「编码错误」，
  // 用户怎么另存都导不进去。真正的结构问题交给后面的 JSON.parse。
  if (raw.includes("\u0000")) {
    return { ok: false, reason: "文件编码无法识别，请以 UTF-8 保存后再导入。" };
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return { ok: false, reason: "文件不是有效的 JSON。" };
  }
  if (!isRecord(parsed)) return { ok: false, reason: "文件结构不正确。" };
  if (parsed.format !== TASK_TRANSFER_FORMAT) {
    return { ok: false, reason: "这不是 Milevia 的任务导出文件。" };
  }
  if (parsed.version !== TASK_TRANSFER_VERSION) {
    return { ok: false, reason: "任务导出文件版本不受支持，请升级 Milevia 后重试。" };
  }
  if (!Array.isArray(parsed.tasks)) return { ok: false, reason: "文件里没有任务列表。" };
  if (parsed.tasks.length === 0) return { ok: false, reason: "文件里没有任务。" };
  if (parsed.tasks.length > TASK_TRANSFER_MAX_TASKS) {
    return { ok: false, reason: "一次最多导入 " + TASK_TRANSFER_MAX_TASKS + " 条任务，请分批导出。" };
  }

  const tasks: TaskExportItem[] = [];
  const keys = new Set<string>();
  for (const entry of parsed.tasks) {
    if (!isRecord(entry)) return { ok: false, reason: "任务条目格式不正确。" };
    const key = typeof entry.key === "string" ? entry.key.trim() : "";
    if (!KEY_PATTERN.test(key) || keys.has(key)) {
      return { ok: false, reason: "任务编号重复或格式不正确，请确认文件未被手工改动。" };
    }
    keys.add(key);
    // null 按「没有依赖」处理：Go 侧解码到 []string 时 null 就是零值，前端不该比服务端更严。
    const dependsOnRaw = entry.dependsOn;
    if (dependsOnRaw !== undefined && dependsOnRaw !== null && !Array.isArray(dependsOnRaw)) {
      return { ok: false, reason: "任务依赖格式不正确。" };
    }
    const dependsOn: string[] = [];
    const seenDependencies = new Set<string>();
    for (const rawDependency of (dependsOnRaw as unknown[] | undefined) || []) {
      if (typeof rawDependency !== "string") return { ok: false, reason: "任务依赖格式不正确。" };
      const dependency = rawDependency.trim();
      if (dependency === key) return { ok: false, reason: "任务不能依赖自己。" };
      if (seenDependencies.has(dependency)) continue;
      seenDependencies.add(dependency);
      dependsOn.push(dependency);
    }
    tasks.push({
      key,
      title: typeof entry.title === "string" ? entry.title : undefined,
      description: typeof entry.description === "string" ? entry.description : "",
      priority: typeof entry.priority === "string" ? entry.priority : undefined,
      pinned: entry.pinned === true,
      status: typeof entry.status === "string" ? entry.status.trim().toLowerCase() : undefined,
      // 服务端用 *time.Time 解码，只认 RFC3339。Date.parse 却接受 "2026-10-20" 这种纯日期，
      // 原样透传会让整份文件 400（文案还是「请求内容不是有效的 JSON」）。这里统一规范成
      // toISOString()，解析不了的直接丢弃（服务端用 now）。
      createdAt: normalizeTransferTimestamp(entry.createdAt),
      dependsOn,
    });
  }
  for (const task of tasks) {
    for (const dependency of task.dependsOn || []) {
      if (!keys.has(dependency)) {
        return { ok: false, reason: "任务依赖指向了文件里不存在的任务。" };
      }
    }
  }
  if (hasDependencyCycle(tasks, invalidKeySet(tasks))) {
    return { ok: false, reason: "任务依赖关系存在循环，无法导入。" };
  }

  return {
    ok: true,
    file: {
      format: TASK_TRANSFER_FORMAT,
      version: TASK_TRANSFER_VERSION,
      exportedAt: typeof parsed.exportedAt === "string" ? parsed.exportedAt : undefined,
      source: isRecord(parsed.source) && typeof parsed.source.project === "string" ? { project: parsed.source.project } : undefined,
      droppedDependencies: typeof parsed.droppedDependencies === "number" ? parsed.droppedDependencies : 0,
      tasks,
    },
  };
}

/**
 * 预览：条数 / 依赖数 / 与现有任务重复的条数（按 trim 后的说明比对，与服务端一致）。
 * 重复判定还要把本次文件里排在前面、同样说明的条目算进去——服务端会跳过它们。
 */
export function previewTaskImport(
  file: TaskExportFile,
  existing: { description?: string | null }[],
): { tasks: number; dependencies: number; duplicates: number } {
  const seen = new Set(existing.map((task) => (task.description || "").trim()));
  let duplicates = 0;
  let dependencies = 0;
  for (const task of file.tasks) {
    const description = (task.description || "").trim();
    if (!description || descriptionLength(description) > TASK_TRANSFER_DESCRIPTION_LIMIT) continue;
    if (seen.has(description)) {
      // 重复任务不落库，它自己的依赖也不会重建——统计口径与服务端保持一致。
      duplicates++;
      continue;
    }
    dependencies += task.dependsOn?.length || 0;
    seen.add(description);
  }
  return { tasks: file.tasks.length, dependencies, duplicates };
}

/**
 * 导入请求体只带服务端实际解码的三个字段。
 *
 * exportedAt 在服务端是 time.Time、droppedDependencies 是 int：原样回传时，手工编辑成
 * 纯日期或小数的文件会让整份请求解码失败（400「请求内容不是有效的 JSON」）。这两项本来
 * 也只是导出端的提示信息，导入端不使用。
 */
export function taskTransferImportBody(file: TaskExportFile): { format: string; version: number; tasks: TaskExportItem[] } {
  return { format: file.format, version: file.version, tasks: file.tasks };
}

/** 任务-<项目名>-YYYYMMDD.json；项目名替换 Windows 非法字符并截断。 */
export function taskTransferFileName(projectName: string, date: Date = new Date()): string {
  // 按 code point 截断：String.slice 会切断代理对，留下孤立 surrogate，Windows 文件名会变成乱码。
  const cleaned = Array.from((projectName || "").replace(/[\\/:*?"<>|]/g, "-").replace(/[\u0000-\u001f]/g, "-").replace(/\s+/g, " ").trim()).slice(0, 40).join("");
  const stamp = String(date.getFullYear()) + String(date.getMonth() + 1).padStart(2, "0") + String(date.getDate()).padStart(2, "0");
  return cleaned ? "任务-" + cleaned + "-" + stamp + ".json" : "任务-" + stamp + ".json";
}
