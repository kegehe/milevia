import assert from "node:assert/strict";
import test from "node:test";
import { snapshotRequestPath } from "./snapshot-request";

test("普通快照请求携带已知版本，强制刷新跳过版本短路", () => {
  assert.equal(snapshotRequestPath("desktop/a", 7), "/v1/instances/desktop%2Fa/snapshot?revision=7");
  assert.equal(snapshotRequestPath("desktop/a", 7, true), "/v1/instances/desktop%2Fa/snapshot");
});
