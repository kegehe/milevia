// 项目卡片的自定义排序顺序 — 基于 localStorage 持久化。
// 维护一个 project id 数组，数组顺序即卡片展示顺序。
// 新增项目追加到末尾；删除项目自动移除。拖拽重排后写回。

const STORAGE_KEY = "milevia:project-order";

function readRaw(): string[] {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (!raw) return [];
    const parsed = JSON.parse(raw);
    return Array.isArray(parsed) ? parsed.filter((id) => typeof id === "string") : [];
  } catch {
    return [];
  }
}

function writeRaw(ids: string[]): void {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(ids));
  } catch {
    // localStorage 不可用时静默降级——顺序仅在内存中保留。
  }
}

/**
 * 依据已保存的顺序对项目 id 排序（纯函数，不写副作用）：
 * 已保存的按保存顺序排列，未保存的（新项目）按传入顺序追加在末尾，
 * 保持后端返回的相对顺序（通常即创建时间倒序）。
 * 已删除但仍残留在存储中的 id 会被自然丢弃（不在返回值里）。
 */
export function sortProjectIds(ids: string[]): string[] {
  const order = readRaw();
  const indexById = new Map<string, number>();
  order.forEach((id, index) => indexById.set(id, index));

  const known: string[] = [];
  const unknown: string[] = [];
  for (const id of ids) {
    if (indexById.has(id)) known.push(id);
    else unknown.push(id);
  }
  known.sort((a, b) => (indexById.get(a) ?? 0) - (indexById.get(b) ?? 0));
  return [...known, ...unknown];
}

/**
 * 把当前实际展示的完整顺序固化到存储中：吸纳新增项目、清理已删除项目。
 * 仅在顺序确实变化时写入。应在副作用（useEffect）中调用，不要在渲染期调用。
 */
export function persistOrder(ids: string[]): void {
  const current = readRaw();
  const same = current.length === ids.length && current.every((id, i) => id === ids[i]);
  if (!same) writeRaw(ids);
}

/** 将某个项目移动到目标位置，并持久化新的顺序。返回更新后的完整顺序。 */
export function moveProject(currentIds: string[], fromId: string, toId: string): string[] {
  const fromIndex = currentIds.indexOf(fromId);
  const toIndex = currentIds.indexOf(toId);
  if (fromIndex === -1 || toIndex === -1 || fromIndex === toIndex) return currentIds;

  const next = [...currentIds];
  const [moved] = next.splice(fromIndex, 1);
  next.splice(toIndex, 0, moved);
  writeRaw(next);
  return next;
}
