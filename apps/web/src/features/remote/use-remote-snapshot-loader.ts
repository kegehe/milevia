import { useCallback, useRef } from "react";
import { isCurrentRemoteRequest } from "./remote-request-guard";

export type SnapshotWithRevision = { snapshotRevision: number };
export type SnapshotFetchOutcome = "updated" | "unchanged" | "failed" | "skipped";
export type SnapshotFetchResult<S> = { outcome: SnapshotFetchOutcome; snapshot: S | null; reason?: string };
export type SnapshotLoadOptions = { force?: boolean; instanceId?: string; silent?: boolean };

type MutableNumber = { current: number };

export type RemoteSnapshotLoaderOptions<S extends SnapshotWithRevision> = {
  instanceID: () => string;
  readToken: () => string;
  scopeKey?: unknown;
  request: (path: string) => Promise<S & { unchanged?: boolean }>;
  requestPath: (instanceID: string, baseline: number, force: boolean) => string;
  normalize: (value: S & { unchanged?: boolean }) => S;
  mergeSnapshot: (snapshot: S, baseline: number) => S;
  setSnapshot: (snapshot: S) => void;
  reportError: (message: string) => void;
  readCache: (instanceID: string) => string | null;
  writeCache: (instanceID: string, snapshot: S) => void;
  unavailableMessage: string;
  cacheFailureMessage?: string;
};

// Kept outside React so request races and cache fallback can be tested without
// rendering a component. The refs belong to the hook and are invalidated when
// the selected remote instance changes.
export function createRemoteSnapshotLoader<S extends SnapshotWithRevision>(
  options: () => RemoteSnapshotLoaderOptions<S>,
  revisionRef: MutableNumber,
  generationRef: MutableNumber,
) {
  return async (loadOptions?: SnapshotLoadOptions): Promise<SnapshotFetchResult<S>> => {
    const current = options();
    const selectedInstanceID = current.instanceID();
    const target = loadOptions?.instanceId || selectedInstanceID;
    if (!target) return { outcome: "skipped", snapshot: null };
    const requestGeneration = ++generationRef.current;
    const requestToken = current.readToken();
    const requestScope = current.scopeKey;
    const baseline = loadOptions?.instanceId && loadOptions.instanceId !== selectedInstanceID ? -1 : revisionRef.current;
    if (!loadOptions?.silent) current.reportError("");
    try {
      const response = await current.request(current.requestPath(target, baseline, loadOptions?.force === true));
      if (!isCurrentRemoteRequest(requestGeneration, generationRef.current, requestToken, options().readToken()) || requestScope !== options().scopeKey) {
        return { outcome: "skipped", snapshot: null };
      }
      if (response?.unchanged) return { outcome: "unchanged", snapshot: null };
      const snapshot = current.normalize(response);
      if (requestGeneration === generationRef.current && snapshot.snapshotRevision >= baseline) {
        revisionRef.current = snapshot.snapshotRevision;
        const merged = current.mergeSnapshot(snapshot, baseline);
        current.setSnapshot(merged);
        current.writeCache(target, merged);
        return { outcome: merged.snapshotRevision > baseline ? "updated" : "unchanged", snapshot: merged };
      }
      return { outcome: "unchanged", snapshot };
    } catch (cause) {
      if (!isCurrentRemoteRequest(requestGeneration, generationRef.current, requestToken, options().readToken()) || requestScope !== options().scopeKey) {
        return { outcome: "skipped", snapshot: null };
      }
      const reason = cause instanceof Error ? cause.message : current.unavailableMessage;
      const cached = current.readCache(target);
      if (cached) {
        try {
          const cachedSnapshot = current.normalize(JSON.parse(cached) as S);
          const merged = current.mergeSnapshot(cachedSnapshot, Number.NEGATIVE_INFINITY);
          current.setSnapshot(merged);
          if (!loadOptions?.silent) current.reportError("当前显示的是最近一次同步快照");
          return { outcome: "failed", snapshot: merged, reason: current.cacheFailureMessage || current.unavailableMessage };
        } catch { /* Invalid cache falls through to the request error. */ }
      }
      if (!loadOptions?.silent) current.reportError(reason);
      return { outcome: "failed", snapshot: null, reason };
    }
  };
}

export function useRemoteSnapshotLoader<S extends SnapshotWithRevision>(options: RemoteSnapshotLoaderOptions<S>) {
  const revisionRef = useRef(-1);
  const generationRef = useRef(0);
  const optionsRef = useRef(options);
  optionsRef.current = options;
  const loadRef = useRef(createRemoteSnapshotLoader(() => optionsRef.current, revisionRef, generationRef));
  const resetSnapshotRevision = useCallback(() => {
    // Invalidate a request from the previous instance before its completion can
    // update the new instance's screen.
    generationRef.current += 1;
    revisionRef.current = -1;
  }, []);
  return { loadSnapshot: loadRef.current, snapshotRevisionRef: revisionRef, resetSnapshotRevision };
}
