import { api } from "../../lib/api";

type PendingWrite = { identity: string; key: string; body: string };
type Storage = Pick<globalThis.Storage, "getItem" | "setItem" | "removeItem">;
const prefix = "milevia.supervision.write:";

// Retain the exact body and key after an ambiguous network failure. A refreshed
// stateVersion must not turn "retry" into a second instruction or new session.
export function prepareSupervisionWrite(storage: Storage, path: string, method: string, value: unknown): PendingWrite {
  const body = JSON.stringify(value);
  const identity = JSON.stringify(value && typeof value === "object" && !Array.isArray(value)
    ? Object.fromEntries(Object.entries(value).filter(([name]) => name !== "stateVersion")) : value);
  const name = prefix + method + " " + path;
  let saved: PendingWrite | null = null;
  try { saved = JSON.parse(storage.getItem(name) ?? "null"); } catch { /* Ignore damaged local receipts. */ }
  if (saved?.identity === identity && typeof saved.key === "string" && typeof saved.body === "string") return saved;
  // LAN HTTP pages may lack randomUUID; this is a deduplication ID, not a token.
  const key = globalThis.crypto?.randomUUID?.() ?? `supervision-${Date.now()}-${Math.random().toString(36).slice(2)}-${Math.random().toString(36).slice(2)}`;
  const pending = { identity, key, body };
  storage.setItem(name, JSON.stringify(pending));
  return pending;
}

export async function supervisionWrite<T = unknown>(path: string, method: "POST" | "PUT", value: unknown): Promise<T> {
  const name = prefix + method + " " + path;
  const pending = prepareSupervisionWrite(sessionStorage, path, method, value);
  const clear = () => {
    // A late response cannot remove a newer request's receipt.
    const saved = sessionStorage.getItem(name);
    if (saved === JSON.stringify(pending)) sessionStorage.removeItem(name);
  };
  try {
    const result = await api<T>(path, { method, body: pending.body, headers: { "Idempotency-Key": pending.key } });
    clear();
    return result;
  } catch (cause) {
    const status = (cause as { status?: number } | null)?.status;
    if (status && status >= 400 && status < 500 && status !== 408) clear();
    throw cause;
  }
}
