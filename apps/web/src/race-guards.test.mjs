import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const [fileTree, taskBoard, taskQueue, gitWorkbench, projectStore, sshManager, runPanel, orchestrationPage, orchestrationStyles] = await Promise.all([
  readFile(new URL("./features/files/ProjectFileTree.tsx", import.meta.url), "utf8"),
  readFile(new URL("./features/tasks/TaskBoard.tsx", import.meta.url), "utf8"),
  readFile(new URL("./features/tasks/TaskQueue.tsx", import.meta.url), "utf8"),
  readFile(new URL("./features/git/GitWorkbench.tsx", import.meta.url), "utf8"),
  readFile(new URL("./stores/useProjectStore.tsx", import.meta.url), "utf8"),
  readFile(new URL("./pages/SSHManagerPage.tsx", import.meta.url), "utf8"),
  readFile(new URL("./features/run/ProjectRunPanel.tsx", import.meta.url), "utf8"),
  readFile(new URL("./pages/OrchestrationPage.tsx", import.meta.url), "utf8"),
  readFile(new URL("./orchestration.css", import.meta.url), "utf8"),
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
  assert.match(taskBoard, /requestVersion === orchestrationRequestVersion\.current\) setOrchestration\(config\);/);
  assert.match(taskQueue, /const tasksRequestVersion = useRef\(0\);/);
  assert.match(taskQueue, /requestVersion === tasksRequestVersion\.current\) setTasks\(next\)/);
  assert.match(taskQueue, /const requestID = \+\+inlineRequest\.current;[\s\S]*?inlineRequest\.current !== requestID/s);
  assert.match(orchestrationPage, /const selectedRequestVersion = useRef\(0\);/);
  assert.match(orchestrationPage, /const overviewRequestVersion = useRef\(0\);/);
  assert.match(orchestrationPage, /const requestVersion = \+\+overviewRequestVersion\.current;/);
  assert.match(orchestrationPage, /requestVersion !== overviewRequestVersion\.current/);
  assert.match(orchestrationPage, /requestVersion !== selectedRequestVersion\.current/);
  assert.match(orchestrationPage, /selectedRequestVersion\.current \+= 1;[\s\S]*?setDetail\(null\);[\s\S]*?setHistory\(null\);/);
});

test("orchestration workspace exposes plan, queue, decision and verification controls", () => {
	assert.match(orchestrationPage, /orchestration\/batches/);
	assert.match(orchestrationPage, /orchestration\/batches\/\$\{existingBatch\.id\}\/tasks/);
  assert.match(orchestrationPage, /orchestration\/order/);
  assert.match(orchestrationPage, /orchestration\/dequeue/);
  assert.match(orchestrationPage, /merge-main/);
	assert.match(orchestrationPage, /orchestration\/decision/);
	assert.match(orchestrationPage, /orchestration\/cleanup/);
  assert.match(orchestrationPage, /conversationStrategy/);
	assert.match(orchestrationPage, /const \[draftTaskIDs, setDraftTaskIDs\] = useState<string\[\]>\(\[\]\);/);
	assert.match(orchestrationPage, /const selectedDraftTasks = useMemo/);
	assert.match(orchestrationPage, /moveDraftTask\(task\.id, "up"\)/);
	assert.match(orchestrationPage, /batchFilterID/);
	assert.doesNotMatch(orchestrationPage, /batches\.slice\(0, 5\)/);
	assert.match(orchestrationPage, /\["released_to_main", "stopped", "needs_human"\]\.includes\(selected\.status\)/);
	assert.match(orchestrationPage, /继承上一任务对话摘要/);
  assert.match(orchestrationPage, /const runningStatuses = new Set\(\["preparing", "implementing", "checking"\]\);/);
  assert.match(orchestrationPage, /verificationError = enabled && verificationCommands\.length === 0/);
  assert.match(orchestrationPage, /function candidateSummary\(task: Task\)/);
  assert.match(orchestrationPage, /const summary = candidateSummary\(task\); return <li key=\{task\.id\}><label title=\{summary\}>/);
  assert.doesNotMatch(orchestrationPage, /候选任务[\s\S]*?未命名任务/);
  assert.match(orchestrationPage, /if \(await queueAction\([\s\S]*?setSelectedEnqueue\(new Set\(\)\);/);
  assert.match(orchestrationPage, /id="workspace-panel-orchestration"[\s\S]*?role="tabpanel"[\s\S]*?aria-labelledby="workspace-tab-orchestration"/);
  assert.match(orchestrationStyles, /\.orchestration-queue \.orchestration-queue-actions button\s*\{[^}]*width:\s*21px;[^}]*min-height:\s*21px;/s);
	assert.match(orchestrationStyles, /\.orchestration-head-actions \{ flex-wrap: wrap; \}/);
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
