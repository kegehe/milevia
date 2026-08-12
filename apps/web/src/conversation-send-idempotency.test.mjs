import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const page = await readFile(new URL("./pages/ConversationPage.tsx", import.meta.url), "utf8");

test("scheduled retries reuse a client request ID", () => {
  assert.match(page, /const pendingSendRequestIDRef = useRef<string \| null>\(null\);/);
  assert.match(page, /pendingSendRequestIDRef\.current = crypto\.randomUUID\(\);/);
  assert.match(page, /const clientRequestId = options\.clientRequestId \?\? crypto\.randomUUID\(\);/);
  assert.match(page, /JSON\.stringify\(\{ content, clientRequestId \}\)/);
  assert.match(page, /sendContentRef\.current\(content, true, \{ restoreOnFailure: last, notifyFailure: last, clientRequestId \}\)/);
  assert.match(page, /pendingSendRequestIDRef\.current = null;/);
});
