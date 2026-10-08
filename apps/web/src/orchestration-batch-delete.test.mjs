import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const pageSource = await readFile(new URL("./pages/OrchestrationPage.tsx", import.meta.url), "utf8");
const cssSource = await readFile(new URL("./orchestration.css", import.meta.url), "utf8");

// 「编排任务」曾经只能新建、不能删除：归档实现被拆掉后，①栏只剩一个纯选择按钮，废弃的计划
// 永久堆积、计数只涨不跌。这里守住整条调用链，避免入口再被悄悄摘掉。
// 删除是**计划级**动作，入口固定在右栏详情：①栏行的点击是「切换筛选」，把红色 × 叠在它
// 右边既容易误触，也让计划级动作混进子任务级操作那一堆。
test("删除入口在右栏详情的「所属编排任务」区块，并调用 DELETE 接口", () => {
  assert.match(pageSource, /const deleteBatch = async \(batch: OrchestrationBatch\) => \{/);
  assert.match(pageSource, /`\/api\/projects\/\$\{projectId\}\/orchestration\/batches\/\$\{batch\.id\}`, \{ method: "DELETE" \}/);
  assert.match(pageSource, /<h3>所属编排任务<\/h3>/);
  assert.match(pageSource, /aria-label="删除编排任务"/);
  // 接线也要锁住：把 onClick/onDelete 改成空函数，上面那些存在性断言照样全绿，功能却是死的。
  assert.match(pageSource, /onDelete=\{\(\) => setConfirmDeleteBatch\(panelBatch\)\}/);
  assert.match(pageSource, /onClick=\{onDelete\}/);
  assert.match(pageSource, /onClick=\{\(\) => void deleteBatch\(confirmDeleteBatch\)\}/);
});

// ①栏那一行必须是**纯选择按钮**。这条不能靠「源码里搜不到某个类名」来守——className 是模板
// 串，永远搜不到；得把 <ol class="orchestration-planlist"> 那一段切出来数按钮。
test("①栏计划行保持纯选择，不挂任何删除动作", () => {
  const open = pageSource.indexOf('<ol className="orchestration-planlist">');
  assert.ok(open > -1, "找不到①栏计划列表");
  const planList = pageSource.slice(open, pageSource.indexOf("</ol>", open));
  assert.equal((planList.match(/<button/g) || []).length, 1, "①栏每个计划行只该有一个按钮（选择）");
  assert.doesNotMatch(planList, /删除|danger|aria-label="删除编排任务"/);
});

// 空计划（一个子任务都没有）在右栏没有可选中的子任务，只能靠①栏的筛选说明用户指的是谁。
// 所以这个区块必须在 `selected` 三元之外也渲染一次——否则空计划没有任何删除入口，
// 废弃计划照样永久堆积（②栏的归档按钮已经被移除）。
test("空计划也有删除入口：区块在两个分支都渲染，并有 activeBatch 兜底", () => {
  assert.match(pageSource, /const panelBatch = selectedBatch \?\? activeBatch;/);
  assert.equal((pageSource.match(/\{panelBatch && <OrchestrationPlanPanel/g) || []).length, 2, "区块要在「有子任务」和「空计划」两个分支各渲染一次");
});

// 删掉当前正在筛选的计划后必须清掉 batchFilterID：留着它，scopedJobs 会按一个已不存在的
// id 过滤成空列表，中间对话区和右栏一起空白。
test("删除成功后立刻清空筛选，且不被刷新失败连累", () => {
  assert.match(pageSource, /const \[confirmDeleteBatch, setConfirmDeleteBatch\] = useState<OrchestrationBatch \| null>\(null\);/);
  // 断言用「顺序」而不是「文本相邻」：相邻性会被中间插入的注释/语句一句话打破（本次就踩到），
  // 而真正要守的性质是顺序——DELETE 成功后立刻清筛选、关弹窗，且这些都发生在刷新之前，
  // 所以刷新失败不会把它们跳过。
  const slug = "`/api/projects/${projectId}/orchestration/batches/${batch.id}`, { method: \"DELETE\" }";
  const deleteAt = pageSource.indexOf(`await api(${slug})`);
  assert.ok(deleteAt > -1, "找不到 DELETE 调用");
  const clearFilterAt = pageSource.indexOf('setBatchFilterID("")', deleteAt);
  const closeDialogAt = pageSource.indexOf("setConfirmDeleteBatch(null)", deleteAt);
  const refreshAt = pageSource.indexOf("await Promise.all([loadOverview(), loadSelected()])", deleteAt);
  assert.ok(clearFilterAt > deleteAt, "清筛选必须在 DELETE 之后");
  assert.ok(closeDialogAt > deleteAt, "关弹窗必须在 DELETE 之后");
  assert.ok(refreshAt > clearFilterAt && refreshAt > closeDialogAt, "刷新必须在收尾之后，否则刷新失败会把收尾一起跳过");
  // 本地也要把删除的效果立即落下来，否则刷新失败时界面会留着一个已不存在的计划。
  assert.ok(pageSource.indexOf("previous.filter((item) => item.id !== batch.id)", deleteAt) > deleteAt, "缺少乐观移除计划");
  assert.ok(pageSource.indexOf('job.batchId === batch.id ? { ...job, batchId: "" }', deleteAt) > deleteAt, "缺少乐观把子任务脱组");
  // 也因此不能退回 queueAction：它把刷新失败也算成操作失败。
  assert.doesNotMatch(pageSource, /queueAction\(`delete-batch:/);
});

// 弹窗必须讲清代价：删的只是分组标签，子任务和它们的 worktree / 分支 / 对话都留着。
// 子任务受「已编排任务不得硬删」的审计保留策略保护，所以这里不能偷偷连 job 一起删。
test("删除确认弹窗说明子任务会被保留", () => {
  assert.match(pageSource, /子任务会保留在「全部子任务」中/);
  assert.match(pageSource, /worktree、任务分支与执行对话都不受影响/);
});

// 新区块要有自己的样式；@media 里的右栏规则是给 .orchestration-detail-actions 的，不覆盖它。
test("「所属编排任务」区块有配套样式", () => {
  assert.match(cssSource, /\.orchestration-plan-link \{/);
  assert.match(cssSource, /\.orchestration-plan-link-body \{/);
});
