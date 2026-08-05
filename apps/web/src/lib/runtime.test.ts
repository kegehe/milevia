import assert from "node:assert/strict";
import test from "node:test";

import { apiURL, sessionHeaders } from "./runtime";

test("浏览器模式保留相对 API 地址且不添加会话头", () => {
  assert.equal(apiURL("/api/health"), "/api/health");
  assert.equal(sessionHeaders().has("X-Milevia-Session"), false);
});
