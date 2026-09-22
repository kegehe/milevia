// 项目卡片的自定义排序顺序 — 基于 localStorage 持久化。
// 维护一个 project id 数组，数组顺序即卡片展示顺序。
// 新增项目追加到末尾；删除项目自动移除。拖拽重排后写回。

export const PROJECT_ORDER_STORAGE_KEY = "milevia:project-order";
// 手机端另起一把键：开发时桌面页面与手机页面是同一个 origin（都在 localhost:5173），
// 共用一把键会让「在手机上排好的顺序」把桌面端的顺序冲掉。真机上虽不同源，但分开更不容易出错。
export const MOBILE_PROJECT_ORDER_STORAGE_KEY = "milevia:mobile-project-order";

function readRaw(key: string = PROJECT_ORDER_STORAGE_KEY): string[] {
  try {
    const raw = localStorage.getItem(key);
    if (!raw) return [];
    const parsed = JSON.parse(raw);
    return Array.isArray(parsed) ? parsed.filter((id) => typeof id === "string") : [];
  } catch {
    return [];
  }
}

function writeRaw(ids: string[], key: string = PROJECT_ORDER_STORAGE_KEY): void {
  try {
    localStorage.setItem(key, JSON.stringify(ids));
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
export function sortProjectIds(ids: string[], key: string = PROJECT_ORDER_STORAGE_KEY): string[] {
  const order = readRaw(key);
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
export function persistOrder(ids: string[], key: string = PROJECT_ORDER_STORAGE_KEY): void {
  const current = readRaw(key);
  const same = current.length === ids.length && current.every((id, i) => id === ids[i]);
  if (!same) writeRaw(ids, key);
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

/**
 * 把 fromId 挪到「槽位 targetIndex 之前 / 之后」。targetIndex 指**当前完整数组**里的下标。
 *
 * 两个容易写错的地方：
 * ① 摘掉自己之后，位于其后的目标槽位会整体左移一位，插入下标要补偿；
 * ② fromId === targetIndex 是「插到自己身上」，必须原样返回 —— 否则往回拖一点点就会抖。
 */
export function reorderIds(ids: string[], fromId: string, targetIndex: number, after: boolean): string[] {
  const from = ids.indexOf(fromId);
  if (from < 0 || targetIndex < 0 || targetIndex >= ids.length || from === targetIndex) return ids;

  const next = ids.filter((_, index) => index !== from);
  const anchor = targetIndex - (from < targetIndex ? 1 : 0);
  const insert = Math.max(0, Math.min(next.length, anchor + (after ? 1 : 0)));
  next.splice(insert, 0, fromId);
  return next;
}

const MOBILE_DRAG_HINT_STORAGE_KEY = "milevia:mobile-project-drag-hint";

/**
 * 手机端「按住项目卡片可拖动排序」的提示**是否还该显示**：
 * true = 还没拖成过（要显示），false = 已经拖成过一次（永久收起）。
 * 注意判据是反的 —— 读名字容易读成"是否已收起"，改条件时先看这里。
 */
export function readMobileDragHint(): boolean {
  try {
    return localStorage.getItem(MOBILE_DRAG_HINT_STORAGE_KEY) !== "1";
  } catch {
    return true;
  }
}

export function dismissMobileDragHint(): void {
  try {
    localStorage.setItem(MOBILE_DRAG_HINT_STORAGE_KEY, "1");
  } catch {
    // localStorage 不可用时无须处理——提示会在下次会话里再出现一次。
  }
}

export function resetProjectOrder(key: string = PROJECT_ORDER_STORAGE_KEY): void {
  try {
    localStorage.removeItem(key);
  } catch {
    // localStorage 不可用时无需额外处理。
  }
}
