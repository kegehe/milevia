const STORAGE_PREFIX = "auto.conversation-draft.v2:";
const LEGACY_STORAGE_KEY = "auto.conversation-drafts.v1";
const MAX_STORED_DRAFTS = 50;
const DEFAULT_RETENTION_DAYS = 30;

export type ConversationDrafts = Record<string, string>;

type DraftStorage = Pick<Storage, "getItem" | "setItem" | "removeItem" | "key" | "length">;
type StoredDraft = { text: string; updatedAt: number };
type StoredDraftEntry = StoredDraft & { storageKey: string; draftKey: string };

export function conversationDraftKey(projectID: string, conversationID: string): string {
  return `${encodeURIComponent(projectID)}:${encodeURIComponent(conversationID)}`;
}

export function updateConversationDraft(drafts: ConversationDrafts, projectID: string, conversationID: string, text: string): ConversationDrafts {
  const key = conversationDraftKey(projectID, conversationID);
  const { [key]: _, ...remaining } = drafts;
  if (!text) return remaining;
  const entries = [...Object.entries(remaining), [key, text] as const];
  return Object.fromEntries(entries.slice(-MAX_STORED_DRAFTS));
}

export function getConversationDraft(drafts: ConversationDrafts, projectID: string, conversationID: string): string {
  return drafts[conversationDraftKey(projectID, conversationID)] || "";
}

export function readConversationDrafts(storage: DraftStorage | null = browserStorage(), maxAgeDays = DEFAULT_RETENTION_DAYS): ConversationDrafts {
  if (!storage) return {};
  try {
    return Object.fromEntries(pruneStoredDrafts(storage, "", maxAgeDays).map((draft) => [draft.draftKey, draft.text]));
  } catch {
    return {};
  }
}

export function countStoredConversationDrafts(maxAgeDays = DEFAULT_RETENTION_DAYS, storage: DraftStorage | null = browserStorage()): number {
  if (!storage || !Number.isFinite(maxAgeDays) || maxAgeDays <= 0) return 0;
  try {
    return listStoredDrafts(storage, maxAgeDays).length;
  } catch {
    return 0;
  }
}

export function clearExpiredConversationDrafts(maxAgeDays = DEFAULT_RETENTION_DAYS, storage: DraftStorage | null = browserStorage()): number {
  if (!storage || !Number.isFinite(maxAgeDays) || maxAgeDays <= 0) return 0;
  try {
    const keys: string[] = [];
    for (let index = 0; index < storage.length; index++) {
      const key = storage.key(index);
      if (key?.startsWith(STORAGE_PREFIX)) keys.push(key);
    }
    let cleared = 0;
    for (const key of keys) {
      const draft = parseStoredDraft(storage.getItem(key));
      if (!draft || isExpired(draft, maxAgeDays)) {
        storage.removeItem(key);
        cleared++;
      }
    }
    return cleared;
  } catch {
    return 0;
  }
}

export function clearStoredConversationDrafts(storage: DraftStorage | null = browserStorage()): number {
  if (!storage) return 0;
  try {
    const storageKeys: string[] = [];
    for (let index = 0; index < storage.length; index++) {
      const key = storage.key(index);
      if (key?.startsWith(STORAGE_PREFIX)) storageKeys.push(key);
    }
    for (const key of storageKeys) storage.removeItem(key);
    storage.removeItem(LEGACY_STORAGE_KEY);
    return storageKeys.length;
  } catch {
    return 0;
  }
}

export function readConversationDraft(projectID: string, conversationID: string, storage: DraftStorage | null = browserStorage(), maxAgeDays = DEFAULT_RETENTION_DAYS): string | null {
  if (!storage) return null;
  const storageKey = storageKeyFor(projectID, conversationID);
  try {
    const draft = parseStoredDraft(storage.getItem(storageKey));
    if (!draft || isExpired(draft, maxAgeDays)) {
      storage.removeItem(storageKey);
      const legacyDraft = getConversationDraft(readLegacyConversationDrafts(storage), projectID, conversationID);
      if (!legacyDraft) return null;
      persistConversationDraft(projectID, conversationID, legacyDraft, storage, maxAgeDays);
      return legacyDraft;
    }
    return draft.text;
  } catch {
    return null;
  }
}

// Each draft uses its own key so concurrent browser tabs never rewrite another draft's value.
export function persistConversationDraft(projectID: string, conversationID: string, text: string, storage: DraftStorage | null = browserStorage(), maxAgeDays = DEFAULT_RETENTION_DAYS): boolean {
  if (!storage) return false;
  const storageKey = storageKeyFor(projectID, conversationID);
  try {
    migrateLegacyConversationDrafts(storage);
    if (!text) {
      storage.removeItem(storageKey);
      return true;
    }
    pruneStoredDrafts(storage, storageKey, maxAgeDays);
    storage.setItem(storageKey, JSON.stringify({ text, updatedAt: Date.now() } satisfies StoredDraft));
    pruneStoredDrafts(storage, storageKey, maxAgeDays);
    return true;
  } catch {
    return false;
  }
}

function storageKeyFor(projectID: string, conversationID: string): string {
  return `${STORAGE_PREFIX}${conversationDraftKey(projectID, conversationID)}`;
}

function migrateLegacyConversationDrafts(storage: DraftStorage): void {
  const legacyDrafts = readLegacyConversationDrafts(storage);
  const entries = Object.entries(legacyDrafts).slice(-MAX_STORED_DRAFTS);
  if (entries.length === 0) return;
  const updatedAt = Date.now();
  for (const [draftKey, text] of entries) {
    const storageKey = `${STORAGE_PREFIX}${draftKey}`;
    if (!storage.getItem(storageKey)) storage.setItem(storageKey, JSON.stringify({ text, updatedAt } satisfies StoredDraft));
  }
  storage.removeItem(LEGACY_STORAGE_KEY);
}

function readLegacyConversationDrafts(storage: DraftStorage): ConversationDrafts {
  const raw = storage.getItem(LEGACY_STORAGE_KEY);
  if (!raw) return {};
  try {
    const parsed: unknown = JSON.parse(raw);
    if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) return {};
    return Object.fromEntries(Object.entries(parsed).filter(([, value]) => typeof value === "string"));
  } catch {
    return {};
  }
}

function pruneStoredDrafts(storage: DraftStorage, preservedStorageKey = "", maxAgeDays = DEFAULT_RETENTION_DAYS): StoredDraftEntry[] {
  const drafts = listStoredDrafts(storage, maxAgeDays);
  const excess = drafts.length - MAX_STORED_DRAFTS;
  if (excess <= 0) return drafts;

  const evicted = new Set<string>();
  for (const draft of drafts) {
    if (evicted.size === excess) break;
    if (draft.storageKey === preservedStorageKey) continue;
    storage.removeItem(draft.storageKey);
    evicted.add(draft.storageKey);
  }
  return drafts.filter((draft) => !evicted.has(draft.storageKey));
}

function listStoredDrafts(storage: DraftStorage, maxAgeDays = DEFAULT_RETENTION_DAYS): StoredDraftEntry[] {
  const storageKeys: string[] = [];
  for (let index = 0; index < storage.length; index++) {
    const key = storage.key(index);
    if (key?.startsWith(STORAGE_PREFIX)) storageKeys.push(key);
  }

  const drafts: StoredDraftEntry[] = [];
  for (const storageKey of storageKeys) {
    const draft = parseStoredDraft(storage.getItem(storageKey));
    if (!draft || isExpired(draft, maxAgeDays)) {
      storage.removeItem(storageKey);
      continue;
    }
    drafts.push({ ...draft, storageKey, draftKey: storageKey.slice(STORAGE_PREFIX.length) });
  }
  return drafts.sort((left, right) => left.updatedAt - right.updatedAt);
}

function parseStoredDraft(raw: string | null): StoredDraft | null {
  if (!raw) return null;
  try {
    const parsed: unknown = JSON.parse(raw);
    if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) return null;
    const { text, updatedAt } = parsed as Partial<StoredDraft>;
    return typeof text === "string" && typeof updatedAt === "number" && Number.isFinite(updatedAt) ? { text, updatedAt } : null;
  } catch {
    return null;
  }
}

function isExpired(draft: StoredDraft, maxAgeDays: number): boolean {
  return Date.now() - draft.updatedAt > maxAgeDays * 24 * 60 * 60 * 1_000;
}

function browserStorage(): DraftStorage | null {
  if (typeof window === "undefined") return null;
  try {
    return window.localStorage;
  } catch {
    return null;
  }
}
