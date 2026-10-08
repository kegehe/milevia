import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const pageSource = await readFile(new URL("./pages/OrchestrationPage.tsx", import.meta.url), "utf8");
const cssSource = await readFile(new URL("./orchestration.css", import.meta.url), "utf8");
// 后端常量：直接模式的终态名一旦改了，前端的 statusLabel 会静默退回显示英文原文，
// 所以两个仓库里的这一份必须对齐（前端无法 import Go，只能这样钉）。
const orchestrationSource = await readFile(new URL("../../control-server/internal/app/orchestration.go", import.meta.url), "utf8");

// 计划级执行方式：worktree（现状）/ branch（直接在已有分支上跑）。用户要的核心是
// 「指定一个已存在的分支、不建 worktree、不合并、工作区有未提交改动也能跑」。
test("新建计划时可以选「直接写入已有分支」并指定目标分支", () => {
  assert.match(pageSource, /type ExecutionMode = "worktree" \| "branch";/);
  assert.match(pageSource, /<option value="branch" disabled=\{Boolean\(directModeBlockedReason\)\}>直接写入已有分支<\/option>/);
  // 目标分支下拉来自项目的本地分支列表，且只列本地分支。
  assert.match(pageSource, /const branches = await api<GitBranchOption\[\]>\(`\/api\/projects\/\$\{projectId\}\/git\/branches`\);/);
  assert.match(pageSource, /setBranchOptions\(branches\.filter\(\(branch\) => !branch\.remote\)\);/);
  // 执行方式与目标分支必须进请求体，否则界面上选了也会静默退回隔离工作树。
  assert.match(pageSource, /executionMode: batchPolicy\.executionMode, targetBranch: batchPolicy\.executionMode === "branch" \? batchPolicy\.targetBranch\.trim\(\) : ""/);
  // 切到直接模式时用当前检出的分支预填，避免下拉停在空值上。
  assert.match(pageSource, /nextMode === "branch" \? previous\.targetBranch \|\| currentBranchName : previous\.targetBranch/);
});

// 远端项目跑不了自动编排（服务端 isLocalRunnerID 会拒），必须在选之前就禁用并说明原因。
test("非本地项目禁用「直接写入」并给出原因", () => {
  assert.match(pageSource, /const localRunner = !batchProject \|\| batchProject\.runner === "" \|\| batchProject\.runner === "windows-local" \|\| batchProject\.runner === "wsl-local";/);
  assert.match(pageSource, /"「直接写入」目前只支持本地运行器的项目。"|「直接写入」目前只支持本地运行器的项目。/);
  assert.match(pageSource, /: directModeSelected && directModeBlockedReason/);
});

// 目标分支不是当前检出的分支时不硬拦（用户可能刚切过），但必须一直提示；
// 服务端派发前以当时的 HEAD 为准，不一致会停下。
test("目标分支与工作区当前分支不一致时给出提示", () => {
  assert.match(pageSource, /const targetBranchMismatch = directModeSelected && Boolean\(currentBranchName\) && Boolean\(targetBranchDraft\) && targetBranchDraft !== currentBranchName;/);
  assert.match(pageSource, /与所选目标分支 \$\{targetBranchDraft\} 不一致/);
  assert.match(pageSource, /Milevia 不会替你切换分支/);
});

// 这一模式会直接改用户的工作目录且不留下可回滚产物，警告文案必须写在界面上。
test("直接模式在弹窗里写明后果", () => {
  assert.match(pageSource, /直接写入模式下 Agent 会在项目工作目录中直接修改文件，不会自动提交、不会创建分支、也不会切换分支。/);
  assert.match(pageSource, /本项目在该任务运行期间会被独占，其他会话的 AI 任务需排队。/);
  assert.match(cssSource, /\.orchestration-composer-warning \{/);
});

// 终态：作业显示「已写入 <分支>」，而不是原始英文或「已合并」。
test("直接模式终态显示为「已写入 <分支>」", () => {
  assert.match(orchestrationSource, /orchestrationApplied\s+= "applied_to_branch"/);
  assert.match(pageSource, /applied_to_branch: `已写入 \$\{targetBranch\}`/);
  assert.match(cssSource, /\.orchestration-status-dot\.applied_to_branch \{ background: #62a071; \}|\.orchestration-status-dot\.released_to_main, \.orchestration-status-dot\.applied_to_branch \{ background: #62a071; \}/);
});

// ①栏/右栏必须一眼看出哪些计划在动用户的工作目录。
test("计划列表与右栏标出「直接写入 <分支>」", () => {
  assert.match(pageSource, /function orchestrationPlanModeLabel\(batch: OrchestrationBatch\) \{/);
  assert.match(pageSource, /return `直接写入 \$\{batch\.targetBranch \|\| "目标分支"\}`;/);
  assert.match(pageSource, /<small>\{orchestrationPlanLabel\(batch\)\}<\/small>/);
  assert.match(pageSource, /<span>\{orchestrationPlanLabel\(batch\)\}<\/span>/);
});

// 右栏的 Git 记录块对直接模式换一版：没有「任务分支」「工作区」，只有执行方式与目标分支。
test("右栏 Git 记录在直接模式下换成执行方式与目标分支", () => {
  assert.match(pageSource, /\{selected\.executionMode === "branch" \? <section className="orchestration-facts" aria-label="Git 记录"><div><span>执行方式<\/span><code>直接写入<\/code><\/div><div><span>目标分支<\/span>/);
  assert.match(pageSource, /改动就在你的项目工作目录里，本页面不会替你提交/);
});

// 合并与清理在直接模式下没有对象：显式关掉，而不是靠「任务分支为空」隐式碰巧不显示。
test("直接模式不出现合并与清理入口", () => {
  assert.match(pageSource, /\{\["awaiting_main", "integrated_to_dev"\]\.includes\(selected\.status\) && selected\.executionMode !== "branch" && <button type="button" className="primary"/);
  assert.match(pageSource, /\{selected\.taskBranch && selected\.executionMode !== "branch" && !selected\.resourcesCleanedAt && \["released_to_main", "stopped", "needs_human"\]\.includes\(selected\.status\)/);
});
