import assert from "node:assert/strict";
import test from "node:test";
import { readFile } from "node:fs/promises";

test("令牌失效监听器只注册一次，并转发最新回调", async () => {
  const source = await readFile(new URL("./use-token-cleared-listener.ts", import.meta.url), "utf8");
  assert.match(source, /const callbackRef = useRef\(onTokenCleared\);/);
  assert.match(source, /globalThis\.addEventListener\("milevia:token-cleared", listener\);/);
  assert.match(source, /return \(\) => globalThis\.removeEventListener\("milevia:token-cleared", listener\);/);
  assert.match(source, /\}, \[\]\);/);
});
