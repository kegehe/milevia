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

test("orchestration workspace exposes plan, queue and decision controls without mandatory verification", () => {
	assert.match(orchestrationPage, /orchestration\/batches/);
	// 新建只负责建计划；任务一律在候选列表里勾选后追加到当前计划。
	assert.match(orchestrationPage, /orchestration\/batches\/\$\{activeBatch\.id\}\/tasks/);
	assert.match(orchestrationPage, /const addCandidatesToBatch = async \(\) =>/);
  assert.match(orchestrationPage, /orchestration\/order/);
  assert.match(orchestrationPage, /orchestration\/dequeue/);
  assert.match(orchestrationPage, /merge-main/);
	assert.match(orchestrationPage, /orchestration\/decision/);
	assert.match(orchestrationPage, /orchestration\/cleanup/);
  assert.match(orchestrationPage, /conversationStrategy/);
	// 弹窗只收集名称、上下文继承与执行配置，不再有「加入到」下拉与启用开关。
	assert.match(orchestrationPage, /const \[batchPolicy, setBatchPolicy\] = useState<BatchPolicyDraft>\(defaultBatchPolicy\);/);
	assert.match(orchestrationPage, /const createBatch = async \(\) => \{[\s\S]*?method: "POST",[\s\S]*?maxFixRounds: batchPolicy\.maxFixRounds/);
	assert.doesNotMatch(orchestrationPage, /加入到/);
	assert.doesNotMatch(orchestrationPage, /启用自动队列|启用自动编排/);
	assert.doesNotMatch(orchestrationPage, /orchestration-eyebrow|EXECUTION PLAN|PROJECT POLICY/);
	assert.match(orchestrationPage, /batchFilterID/);
	assert.doesNotMatch(orchestrationPage, /batches\.slice\(0, 5\)/);
	assert.match(orchestrationPage, /\["released_to_main", "stopped", "needs_human"\]\.includes\(selected\.status\)/);
	assert.match(orchestrationPage, /继承上一任务对话摘要/);
  assert.match(orchestrationPage, /const runningStatuses = new Set\(\["preparing", "implementing", "checking"\]\);/);
  assert.match(orchestrationPage, /checking: "收尾中"/);
  assert.doesNotMatch(orchestrationPage, /验证命令|启用自动编排时至少需要一条验证命令/);
  assert.match(orchestrationPage, /最大修复轮次/);
  assert.match(orchestrationPage, /function candidateSummary\(task: Task\)/);
  assert.match(orchestrationPage, /const summary = candidateSummary\(task\); return <li key=\{task\.id\}><label title=\{summary\}>/);
  assert.doesNotMatch(orchestrationPage, /候选任务[\s\S]*?未命名任务/);
  assert.match(orchestrationPage, /if \(await queueAction\([\s\S]*?setSelectedEnqueue\(new Set\(\)\);/);
  assert.match(orchestrationPage, /id="workspace-panel-orchestration"[\s\S]*?role="tabpanel"[\s\S]*?aria-labelledby="workspace-tab-orchestration"/);
  assert.match(orchestrationStyles, /\.orchestration-queue \.orchestration-queue-actions button\s*\{[^}]*width:\s*21px;[^}]*min-height:\s*21px;/s);
	assert.match(orchestrationStyles, /\.orchestration-head-actions \{ flex-wrap: wrap; \}/);
	assert.match(orchestrationStyles, /\.orchestration-composer-dialog \{/);
});

test("orchestration workspace loads complete history and keeps a single-column mobile layout", () => {
  assert.match(orchestrationPage, /type ConversationHistory = \{ conversation\?: \{ agentId: AgentID \}; activeRunId\?: string \| null; messages: Message\[\]; events: Event\[\]; hasMore: boolean; nextCursor: string \}/);
  assert.match(orchestrationPage, /const shouldLoadFullHistory = historyMode === "full" \|\| historyRef\.current === null;/);
  assert.match(orchestrationPage, /const query = new URLSearchParams\(\{ limit: "1000" \}\);/);
  assert.match(orchestrationPage, /if \(cursor\) query\.set\("cursor", cursor\);/);
  assert.match(orchestrationPage, /cursor = shouldLoadFullHistory && page\.hasMore \? page\.nextCursor : "";/);
  assert.match(orchestrationPage, /\} while \(cursor\);/);
  assert.match(orchestrationPage, /requestVersion !== selectedRequestVersion\.current\) return;/);
  assert.match(orchestrationPage, /if \(historyMode === "latest" && selectedRequestInFlight\.current !== 0\) return;/);
  assert.match(orchestrationPage, /if \(selected && refreshingStatuses\.has\(selected\.status\)\) void loadSelected\("latest"\)/);
  const conversationPanel = orchestrationPage.match(/<main className="orchestration-conversation">[\s\S]*?<\/main>/)?.[0] || "";
  assert.match(conversationPanel, /<h2>完整对话<\/h2>/);
  assert.doesNotMatch(conversationPanel, /<section className="orchestration-timeline">/);
  assert.match(orchestrationPage, /import "\.\.\/conversation\.css";/);
  assert.match(orchestrationPage, /<section className="timeline orchestration-conversation-timeline">/);
  assert.match(orchestrationPage, /<div className="timeline-entry message-entry"><article className=\{`message \$\{message\.role\}`\}/);
  assert.match(orchestrationPage, /<ReactMarkdown remarkPlugins=\{\[remarkGfm\]\}/);
  assert.match(orchestrationPage, /const selectedAgentID = history\?\.conversation\?\.agentId \|\| config\?\.agentId \|\| "claude-code";/);
  assert.match(conversationPanel, /agentID=\{selectedAgentID\}/);
  assert.match(orchestrationPage, /function orchestrationActivityLabel\(status: string, agentID: AgentID\)/);
  assert.match(conversationPanel, /className="run-indicator orchestration-run-indicator" role="status"/);
  assert.match(orchestrationPage, /function ScrollNavigationIcon\(\{ direction \}: \{ direction: "top" \| "previous" \| "next" \| "bottom" \}\)/);
  assert.match(conversationPanel, /className="scroll-buttons orchestration-scroll-buttons"/);
  assert.match(conversationPanel, /title="回到顶部"/);
  assert.match(conversationPanel, /title="上一条我的消息"/);
  assert.match(conversationPanel, /title="下一条我的消息"/);
  assert.match(conversationPanel, /title="回到底部"/);
  assert.match(orchestrationPage, /<aside className="orchestration-detail"[\s\S]*?<section className="orchestration-timeline">/);
  assert.match(orchestrationStyles, /\.orchestration-page \{ display: flex; height: 100%; min-height: 0; flex-direction: column; overflow: hidden; \}/);
  assert.match(orchestrationStyles, /\.orchestration-console \{ min-height: 0; flex: 1 1 0; grid-template-rows: minmax\(0, 1fr\); overflow: hidden; \}/);
  assert.match(orchestrationStyles, /\.orchestration-console > \* \{ min-height: 0; \}/);
  assert.match(orchestrationStyles, /\.orchestration-conversation \{ position: relative;/);
  assert.match(orchestrationStyles, /\.orchestration-scroll-buttons \{ position: absolute; right: 18px; bottom: 18px; \}/);
  assert.match(orchestrationStyles, /\.orchestration-queue, \.orchestration-detail \{ overflow-y: auto; overflow-x: hidden; \}/);
  assert.match(orchestrationStyles, /@media \(max-width: 820px\) \{[\s\S]*?\.orchestration-console \{ grid-template-columns: 1fr; \}/);
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

test("Git operation history follows the selected conversation workspace", () => {
  assert.match(gitWorkbench, /request<GitOperation\[\]>\(withWorkspace\(`\$\{base\}\/operations`\)\)/);
});
