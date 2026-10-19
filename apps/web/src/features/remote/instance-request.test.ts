import assert from "node:assert/strict";
import test from "node:test";
import { remoteInstancesPath } from "./instance-request";

test("实例列表使用固定的远端读取端点", () => {
  assert.equal(remoteInstancesPath, "/v1/instances");
});
