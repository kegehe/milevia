import assert from "node:assert/strict";
import test from "node:test";
import { isCurrentRemoteRequest } from "./remote-request-guard";

test("仅当前代次且令牌未切换的同步请求可以落地", () => {
  assert.equal(isCurrentRemoteRequest(4, 4, "token-a", "token-a"), true);
  assert.equal(isCurrentRemoteRequest(3, 4, "token-a", "token-a"), false);
  assert.equal(isCurrentRemoteRequest(4, 4, "token-a", "token-b"), false);
});
