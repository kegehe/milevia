import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const pageSource = await readFile(new URL("./pages/OrchestrationPage.tsx", import.meta.url), "utf8");

// 发布快照的后端接口一度只存在于 orchestration.go、既没注册路由也没有界面入口，
// 用户只能手动改库触发。这里守住三条调用链，避免再次被悄悄摘掉。
test("发布快照列表随编排总览一起加载", () => {
  assert.match(pageSource, /api<ReleaseSnapshot\[\]>\(`\/api\/projects\/\$\{projectId\}\/orchestration\/releases`\)/);
  assert.match(pageSource, /setReleases\(nextReleases\)/);
});

test("编排页提供创建发布快照与确认合入稳定分支的入口", () => {
  assert.match(pageSource, /④ 发布快照/);
  assert.match(pageSource, /`\/api\/projects\/\$\{projectId\}\/orchestration\/releases`, \{ method: "POST" \}/);
  assert.match(pageSource, /`\/api\/projects\/\$\{projectId\}\/orchestration\/releases\/\$\{confirmRelease\.id\}\/confirm`, \{ method: "POST" \}/);
});

test("开发分支可在界面上独立配置", () => {
  // 开发分支决定快照来源，必须能脱离「新建编排任务」单独修改。
  assert.match(pageSource, /orchestration\/config`, \{ method: "PUT"/);
  assert.match(pageSource, /开发分支是发布快照的来源/);
});
