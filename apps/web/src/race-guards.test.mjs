import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const [fileTree, taskBoard, taskQueue, gitWorkbench, projectStore, sshManager, runPanel] = await Promise.all([
  readFile(new URL("./features/files/ProjectFileTree.tsx", import.meta.url), "utf8"),
  readFile(new URL("./features/tasks/TaskBoard.tsx", import.meta.url), "utf8"),
  readFile(new URL("./features/tasks/TaskQueue.tsx", import.meta.url), "utf8"),
  readFile(new URL("./features/git/GitWorkbench.tsx", import.meta.url), "utf8"),
  readFile(new URL("./stores/useProjectStore.tsx", import.meta.url), "utf8"),
  readFile(new URL("./pages/SSHManagerPage.tsx", import.meta.url), "utf8"),
  readFile(new URL("./features/run/ProjectRunPanel.tsx", import.meta.url), "utf8"),
]);

test("file tree invalidates in-flight loads without resetting their epochs", () => {
  assert.match(fileTree, /private pendingLoadIDs = new Map<string, number>\(\);/);
  assert.match(fileTree, /const loadID = \+\+this\.nextLoadID;/);
  assert.match(fileTree, /if \(this\.pendingLoadIDs\.get\(cacheKey\) === loadID\) \{\s*this\.pendingLoads\.delete\(cacheKey\);\s*this\.pendingLoadIDs\.delete\(cacheKey\);/s);
  assert.match(fileTree, /for \(const key of new Set\(\[\.\.\.this\.dirEpochs\.keys\(\), \.\.\.this\.pendingLoads\.keys\(\)\]\)\) \{\s*this\.dirEpochs\.set\(key, \(this\.dirEpochs\.get\(key\) \|\| 0\) \+ 1\);/s);
  assert.doesNotMatch(fileTree, /this\.dirEpochs\.clear\(\)/);
});

test("task views only apply their latest list, detail, and orchestration response", () => {
  assert.match(taskBoard, /const tasksRequestVersion = useRef\(0\);/);
  assert.match(taskBoard, /const detailRequestVersion = useRef\(0\);/);
  assert.match(taskBoard, /const orchestrationRequestVersion = useRef\(0\);/);
  assert.match(taskBoard, /requestVersion === tasksRequestVersion\.current\) setTasks\(next\)/);
  assert.match(taskBoard, /requestVersion === detailRequestVersion\.current\) setDetail\(next\)/);
  assert.match(taskBoard, /requestVersion === orchestrationRequestVersion\.current\) \{ setOrchestration\(config\);/);
  assert.match(taskQueue, /const tasksRequestVersion = useRef\(0\);/);
  assert.match(taskQueue, /requestVersion === tasksRequestVersion\.current\) setTasks\(next\)/);
  assert.match(taskQueue, /const requestID = \+\+inlineRequest\.current;[\s\S]*?inlineRequest\.current !== requestID/s);
});

test("Git, project dashboard, and SSH lists discard stale responses", () => {
  assert.match(gitWorkbench, /const reloadRequest = useRef\(0\);/);
  assert.match(gitWorkbench, /const requestID = \+\+reloadRequest\.current;/);
  assert.match(gitWorkbench, /requestID !== reloadRequest\.current/);
  assert.match(projectStore, /const projectRequestVersion = useRef\(0\);/);
  assert.match(projectStore, /const statusRequestVersion = useRef\(0\);/);
  assert.match(projectStore, /requestVersion !== projectRequestVersion\.current\) return;/);
  assert.match(projectStore, /requestVersion !== statusRequestVersion\.current\) return;/);
  assert.match(sshManager, /const connectionsRequestVersion = useRef\(0\);/);
  assert.match(sshManager, /requestVersion === connectionsRequestVersion\.current\) setConnections\(list\)/);
  assert.match(sshManager, /const profilesRequestVersion = useRef\(0\);/);
  assert.match(runPanel, /const statusRequestVersionRef = useRef\(0\);/);
  assert.match(runPanel, /const requestVersion = \+\+statusRequestVersionRef\.current;/);
  assert.match(runPanel, /requestVersion !== statusRequestVersionRef\.current/);
});
