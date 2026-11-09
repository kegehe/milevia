import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const page = await readFile(new URL("./pages/ConversationPage.tsx", import.meta.url), "utf8");

test("scheduled retries reuse a client request ID", () => {
  assert.match(page, /const pendingSendRequestIDRef = useRef<string \| null>\(null\);/);
  assert.match(page, /pendingSendRequestIDRef\.current = crypto\.randomUUID\(\);/);
  assert.match(page, /const clientRequestId = options\.clientRequestId \?\? crypto\.randomUUID\(\);/);
  assert.match(page, /JSON\.stringify\(\{ content, clientRequestId, attachments: files \}\)/);
  // 预约发送使用自己的快照，不能清空后来新写的草稿；最终失败统一合并恢复。
  assert.match(page, /sendContentRef\.current\(draft, false, \{ restoreOnFailure: false, notifyFailure: last, clientRequestId, skillRefs: refs, attachments: files \}\)/);
  assert.match(page, /if \(last\) \{[\s\S]*?mergeDraftAndPending\(textRef\.current, draft\)[\s\S]*?addAttachments\(files\);/);
  assert.match(page, /pendingSendRequestIDRef\.current = null;/);
});
