import assert from "node:assert/strict";
import test from "node:test";
import { createRemoteSnapshotLoader } from "./use-remote-snapshot-loader";

type Snapshot = { snapshotRevision: number; projects: string[] };

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((ok, fail) => { resolve = ok; reject = fail; });
  return { promise, resolve, reject };
}

function setup(request: (path: string) => Promise<Snapshot>, cache: string | null = null) {
  const revision = { current: -1 };
  const generation = { current: 0 };
  const paths: string[] = [];
  const applied: Snapshot[] = [];
  const loader = createRemoteSnapshotLoader(() => ({
    instanceID: () => "instance-a",
    readToken: () => "token-a",
    request: (path) => { paths.push(path); return request(path); },
    requestPath: (id, baseline, force) => `/snapshots/${id}?revision=${baseline}&force=${force}`,
    normalize: (value) => {
      if (!Array.isArray(value.projects)) throw new Error("invalid snapshot");
      return { ...value, snapshotRevision: Number.isFinite(value.snapshotRevision) ? value.snapshotRevision : 0 };
    },
    mergeSnapshot: (value) => ({ ...value, projects: [...value.projects, "merged"] }),
    setSnapshot: (value) => applied.push(value),
    reportError: () => undefined,
    readCache: () => cache,
    writeCache: () => undefined,
    unavailableMessage: "unavailable",
  }), revision, generation);
  return { loader, revision, generation, paths, applied };
}

test("remote snapshot loader ignores a superseded request", async () => {
  const first = deferred<Snapshot>();
  const second = deferred<Snapshot>();
  let calls = 0;
  const state = setup(() => (++calls === 1 ? first.promise : second.promise));
  const oldRequest = state.loader();
  const currentRequest = state.loader();
  second.resolve({ snapshotRevision: 2, projects: ["new"] });
  first.resolve({ snapshotRevision: 1, projects: ["old"] });
  assert.equal((await oldRequest).outcome, "skipped");
  assert.equal((await currentRequest).outcome, "updated");
  assert.deepEqual(state.applied, [{ snapshotRevision: 2, projects: ["new", "merged"] }]);
});

test("remote snapshot loader ignores an old device epoch even when its token is reused", async () => {
  const pending = deferred<Snapshot>();
  let scope = 1;
  const revision = { current: -1 };
  const generation = { current: 0 };
  const applied: Snapshot[] = [];
  const loader = createRemoteSnapshotLoader(() => ({
    instanceID: () => "instance-a",
    readToken: () => "token-a",
    scopeKey: scope,
    request: () => pending.promise,
    requestPath: () => "/snapshots/instance-a",
    normalize: (value) => value,
    mergeSnapshot: (value) => value,
    setSnapshot: (value) => applied.push(value),
    reportError: () => undefined,
    readCache: () => null,
    writeCache: () => undefined,
    unavailableMessage: "unavailable",
  }), revision, generation);
  const request = loader();
  scope = 2;
  pending.resolve({ snapshotRevision: 1, projects: ["old"] });
  assert.deepEqual(await request, { outcome: "skipped", snapshot: null });
  assert.deepEqual(applied, []);
});

test("remote snapshot loader sends force refresh through the request path", async () => {
  const state = setup(async () => ({ snapshotRevision: 1, projects: [] }));
  await state.loader({ force: true });
  assert.deepEqual(state.paths, ["/snapshots/instance-a?revision=-1&force=true"]);
});

test("remote snapshot loader merges realtime state into cached fallback", async () => {
  const state = setup(async () => { throw new Error("offline"); }, JSON.stringify({ snapshotRevision: 3, projects: ["cached"] }));
  const result = await state.loader();
  assert.equal(result.outcome, "failed");
  assert.deepEqual(result.snapshot, { snapshotRevision: 3, projects: ["cached", "merged"] });
});

test("remote snapshot loader skips when no instance is selected", async () => {
  const revision = { current: -1 };
  const generation = { current: 0 };
  const loader = createRemoteSnapshotLoader(() => ({
    instanceID: () => "",
    readToken: () => "token-a",
    request: async () => ({ snapshotRevision: 1, projects: [] }),
    requestPath: () => "",
    normalize: (value) => value,
    mergeSnapshot: (value) => value,
    setSnapshot: () => undefined,
    reportError: () => undefined,
    readCache: () => null,
    writeCache: () => undefined,
    unavailableMessage: "unavailable",
  }), revision, generation);
  assert.deepEqual(await loader(), { outcome: "skipped", snapshot: null });
});
