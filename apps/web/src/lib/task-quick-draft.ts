const STORAGE_PREFIX = "milevia.task-quick-draft.v1:";

type DraftStorage = Pick<Storage, "getItem" | "setItem" | "removeItem">;

function storageKey(projectID: string): string {
  return `${STORAGE_PREFIX}${encodeURIComponent(projectID)}`;
}

function browserStorage(): DraftStorage | null {
  if (typeof window === "undefined") return null;
  try {
    return window.localStorage;
  } catch {
    return null;
  }
}

export function readTaskQuickDraft(projectID: string, storage: DraftStorage | null = browserStorage()): string {
  if (!storage) return "";
  try {
    return storage.getItem(storageKey(projectID)) || "";
  } catch {
    return "";
  }
}

export function persistTaskQuickDraft(projectID: string, text: string, storage: DraftStorage | null = browserStorage()): boolean {
  if (!storage) return false;
  try {
    const key = storageKey(projectID);
    if (text) storage.setItem(key, text);
    else storage.removeItem(key);
    return true;
  } catch {
    return false;
  }
}
