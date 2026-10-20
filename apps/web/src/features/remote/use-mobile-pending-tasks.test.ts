import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const source = await readFile(new URL("./use-mobile-pending-tasks.ts", import.meta.url), "utf8");

test("待确认任务域集中管理登记、忙碌状态和临时任务 ID 替换", () => {
  assert.match(source, /const pendingTaskRef = useRef\(new Map<string, PendingTaskMutation>\(\)\);/);
  assert.match(source, /const \[pendingTaskBusy, setPendingTaskBusy\] = useState<Set<string>>\(\(\) => new Set\(\)\);/);
  assert.match(source, /const resolved = \{ \.\.\.withResolvedCreateID\(mutation, realID\), commandId \};/);
  assert.match(source, /setTaskBusy\(pendingID, false\);\s*setTaskBusy\(resolved\.taskId, true\);/);
  assert.match(source, /const clearPendingTasks = useCallback\(\(\) => \{[\s\S]*?pendingTaskRef\.current\.clear\(\);\s*setPendingTaskBusy\(new Set\(\)\);/);
});
