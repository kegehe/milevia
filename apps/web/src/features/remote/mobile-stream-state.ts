import type { DeviceStore } from "../../lib/mobile-devices";

export function mobileStreamScope(streamURL: string): string {
  const url = new URL(streamURL, "https://localhost");
  return `${url.origin}/${encodeURIComponent(url.searchParams.get("instanceId") || "")}`;
}

function readCursor(store: DeviceStore, key: string): string | null {
  try {
    const value = store.getItem(key);
    return value !== null && /^(0|[1-9]\d*)$/.test(value) && Number.isSafeInteger(Number(value)) ? value : null;
  } catch { return null; }
}

export function readMobileStreamCursor(store: DeviceStore, scope: string): string | null {
  return readCursor(store, `milevia.stream-cursor.v1:${scope}`);
}

export function readMobileNotificationBaseline(store: DeviceStore, scope: string): string | null {
  return readCursor(store, `milevia.notification-baseline.v1:${scope}`);
}

export function advanceMobileStreamCursor(previous: string, incoming: string): string {
  if (!/^(0|[1-9]\d*)$/.test(incoming) || !Number.isSafeInteger(Number(incoming))) return previous;
  return !previous || Number(incoming) > Number(previous) ? incoming : previous;
}

function initializeNotificationBaseline(store: DeviceStore, scope: string, cursor: string): void {
  // 基线固定为首次接收的位置，不能随断点推进，否则重启会漏掉离线期间的结果。
  if (readMobileNotificationBaseline(store, scope) === null) {
    store.setItem(`milevia.notification-baseline.v1:${scope}`, cursor);
  }
}

/** 只有事件处理完（包括写入待投递队列）后，才允许推进持久断点。 */
export function saveMobileStreamCursor(store: DeviceStore, scope: string, cursor: string): void {
  if (!/^(0|[1-9]\d*)$/.test(cursor) || !Number.isSafeInteger(Number(cursor))) return;
  const previous = readMobileStreamCursor(store, scope);
  if (previous !== null && Number(previous) >= Number(cursor)) return;
  store.setItem(`milevia.stream-cursor.v1:${scope}`, cursor);
}

export async function initializeMobileStreamCursor(store: DeviceStore, scope: string, historyURL: string, token: string, signal: AbortSignal): Promise<string> {
  // 基线落盘后，断点那次写入可能失败；此时仍需从该基线追赶，不能重新跳到最新。
  const saved = readMobileStreamCursor(store, scope) ?? readMobileNotificationBaseline(store, scope);
  if (saved !== null) {
    initializeNotificationBaseline(store, scope, saved);
    saveMobileStreamCursor(store, scope, saved);
    return saved;
  }
  // 首次启用只建立基线；断点一旦存在，重启后必须继续用它，而不是重新跳到最新。
  const controller = new AbortController();
  const abort = () => controller.abort();
  if (signal.aborted) controller.abort();
  else signal.addEventListener("abort", abort, { once: true });
  const timer = setTimeout(abort, 15_000);
  try {
    const response = await fetch(historyURL, { headers: { Authorization: `Bearer ${token}` }, signal: controller.signal });
    if (!response.ok) throw new Error(`无法建立通知接收断点 (${response.status})`);
    const history: unknown = await response.json();
    if (!Array.isArray(history)) throw new Error("通知事件列表格式无效");
    const cursor = String(Math.max(0, ...history.map((item) => Number.isSafeInteger(item?.agentSequence) && item.agentSequence > 0 ? item.agentSequence : 0)));
    if (controller.signal.aborted) throw new Error("建立通知接收断点已取消或超时");
    initializeNotificationBaseline(store, scope, cursor);
    saveMobileStreamCursor(store, scope, cursor);
    return cursor;
  } finally {
    clearTimeout(timer);
    signal.removeEventListener("abort", abort);
    controller.abort();
  }
}
