import assert from "node:assert/strict";
import { test } from "node:test";
import { formatNotificationTimeAgo } from "./NotificationCenter";

test("notification relative time follows the supplied current time", () => {
  const createdAt = "2026-09-09T00:00:00.000Z";
  const base = Date.parse(createdAt);

  assert.equal(formatNotificationTimeAgo(createdAt, base + 59_000), "刚刚");
  assert.equal(formatNotificationTimeAgo(createdAt, base + 60_000), "1 分钟前");
  assert.equal(formatNotificationTimeAgo(createdAt, base + 60 * 60_000), "1 小时前");
});
