// 优化建议「批量删除」：只守「接线还在不在」与「规则顺序对不对」。
//
// 选择集的增删/收敛、全选判据这类逻辑在 insights-model.test.ts 里做行为断言（那边能挡变异）；
// 这里守的是"页面有没有把它接上去"，以及 CSS 里那两条靠顺序生效的规则 ——
// 用扫源码正则验优先级只会自证，所以顺序类断言一律写成 indexOf 比较。
//
// 行为层（复选框几何、请求体、删完列表真的少人）在 .tmp/probe-insights-bulk.mjs 里，
// 那条链是变异脚本覆盖不到的那一半（见 TOOLING：每条防线要配 node 断言 + 探针断言两套）。
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

// 读源码前先统一行尾：本仓工作区是 CRLF（`.gitattributes` 的 eol=lf 只在提交时生效），
// 以后谁在这里写 `\n` 锚点都会静默失配（2026-09-12 那次排查了三轮）。
const normalize = (text) => text.replace(/\r\n/g, "\n");
const panel = normalize(await readFile(new URL("./InsightsPanel.tsx", import.meta.url), "utf8"));
const styles = normalize(await readFile(new URL("./insights.css", import.meta.url), "utf8"));
const model = normalize(await readFile(new URL("./insights-model.ts", import.meta.url), "utf8"));

/** 断言"不该出现"之前先剥注释：注释里原样写出的属性名会把否定断言喂饱（踩过两次）。 */
const stripComments = (text) => text.replace(/\/\*[\s\S]*?\*\//g, "").replace(/^\s*\/\/.*$/gm, "");
const code = stripComments(panel);

const count = (text, pattern) => (text.match(pattern) ?? []).length;

test("三个列表都接上选择模式（少传一处就会出现「有的卡片勾不了」）", () => {
  assert.equal(count(panel, /selectable=\{selectMode\}/g), 3, "有效列表 / 已失效 / 已忽略各一处");
  assert.equal(count(panel, /selected=\{selected\.has\(finding\.id\)\}/g), 3);
  assert.equal(count(panel, /onToggleSelect=\{\(\) => toggleSelect\(finding\.id\)\}/g), 3);
});

test("选择集在每次刷新后收敛，且没变化时不换引用", () => {
  assert.match(panel, /const listedIds = \[\.\.\.res\.findings, \.\.\.\(res\.invalidated \?\? \[\]\), \.\.\.\(res\.dismissed \?\? \[\]\)\]\.map\(\(f\) => f\.id\)/);
  assert.match(panel, /const next = pruneInsightSelection\(prev, listedIds\)/);
  // 2 秒轮询下每次换身份会让整棵树白重渲染，所以"没变化就返回原引用"是要求不是优化。
  assert.match(panel, /return next\.size === prev\.size \? prev : next;/);
});

test("切项目清空选择集与选择模式（否则删的是上一个项目的 id）", () => {
  const effect = panel.slice(panel.indexOf("agentSelectionInitializedRef.current = false;"), panel.indexOf("}, [projectID]);"));
  assert.match(effect, /setSelectMode\(false\)/);
  assert.match(effect, /setSelected\(new Set\(\)\)/);
});

test("退出多选 = 关模式 + 清选择集（两件事必须同处发生）", () => {
  const exit = panel.slice(panel.indexOf("const exitSelectMode = () =>"), panel.indexOf("// 删选中的"));
  assert.match(exit, /setSelectMode\(false\)/);
  assert.match(exit, /setSelected\(new Set\(\)\)/);
  // 开关按钮复用同一个出口：只有一处能退出，就不会出现"关了模式却留着选择"。
  assert.match(panel, /onClick=\{\(\) => \(selectMode \? exitSelectMode\(\) : setSelectMode\(true\)\)\}/);
  assert.match(panel, /\{selectMode \? "退出多选" : "多选"\}/);
});

test("全选只作用于当前筛选下的可见列表", () => {
  assert.match(panel, /const visibleIds = visible\.map\(\(f\) => f\.id\)/);
  assert.match(panel, /const bulkSelection = insightSelectionSummary\(selected, visibleIds\)/);
  assert.match(panel, /onChange=\{\(\) => setSelected\(\(prev\) => toggleInsightSelectionAll\(prev, visibleIds\)\)\}/);
  // 传全量 findings 会让"筛选生效时全选"静默变成全选所有：
  assert.doesNotMatch(code, /toggleInsightSelectionAll\(prev, findings\.map/);
});

test("全选框的三态与可访问名跟着可见集合走", () => {
  assert.match(panel, /checked=\{bulkSelection\.all\}/);
  assert.match(panel, /if \(el\) el\.indeterminate = bulkSelection\.partial;/);
  assert.match(panel, /aria-label=\{bulkSelection\.all \? "取消选择当前列表" : "全选当前列表"\}/);
  assert.match(panel, /\{bulkSelection\.all \? "取消全选" : "全选"\}/);
});

test("筛选生效时界面必须说明范围被改小了", () => {
  assert.match(panel, /data-scope=\{filter === "all" \? "all" : "filtered"\}/);
  assert.match(panel, /筛选生效：全选只作用于「\$\{insightTypeLabels\[filter\]\}」下的 \$\{bulkSelection\.total\} 条/);
  // 提示必须读真实数字，不是写死的常量。
  assert.match(styles, /\.insights-bulk-hint\[data-scope="filtered"\]/);
});

test("两条删除路径走同一个端点、但请求体必须分得开", () => {
  assert.equal(count(panel, /\/insights\/delete`/g), 2);
  assert.match(panel, /body: JSON\.stringify\(\{ findingIds: ids \}\)/);
  assert.match(panel, /body: JSON\.stringify\(\{ scope: "open" \}\)/);
  // 「全部删除」绝不能退化成"把当前看到的 id 全发过去"——截断时那样删不干净。
  assert.doesNotMatch(code, /scope: "all"|scope: "dismissed"|scope: "invalidated"/);
  // 0 条不发请求（按钮虽然禁用，函数自身也要拦一道）。空选择集那一支要连 return 一起在
  // （少了 return 就会带着空数组发请求，确认框还挂着"删除 0 条"）—— 见下面那条整块断言。
  assert.match(panel, /if \(bulkDeleting\) return;/);
});

test("复核进行中不给「全部删除」，但逐条级别的删除照旧", () => {
  // 「全部添加为任务」在扫描进行中就是这个待遇；「全部删除」在复核进行中同理：
  // 复核 worker 已经按批次把建议送给 agent 了，删掉它们既停不下来也已经花掉，
  // 而进度条还会继续数到 N（用户看到"复核 300/500"而列表是空的）。
  assert.match(panel, /disabled=\{bulkDeleting \|\| openCount === 0 \|\| anyVerifying\}/);
  assert.match(panel, /title=\{anyVerifying\s*\n?\s*\? "复核进行中：先停止复核（或等它跑完）再全部删除，否则这些建议会被白核一遍"/);
  // 「删除选中」不能被一起闸掉：它等价于逐张点卡片上的「删除」，本来就是允许的。
  assert.match(panel, /disabled=\{bulkDeleting \|\| selected\.size === 0\}/);
});

test("两个确认框各自独立，且条数来自真实读数", () => {
  assert.match(panel, /\{confirmBulkDelete === "selected" && createPortal\(/);
  assert.match(panel, /\{confirmBulkDelete === "all" && createPortal\(/);
  assert.match(panel, /确定删除选中的 <b>\{selected\.size\}<\/b> 条建议/);
  assert.match(panel, /确定删除当前全部 <b>\{openCount\}<\/b> 条有效建议/);
  // "其中 N 条来自折叠区"必须是真的算出来的（selectedFoldedCount），不是固定文案。
  assert.match(panel, /const selectedFoldedCount =/);
  assert.match(panel, /\{selectedFoldedCount > 0 && <>其中 \{selectedFoldedCount\} 条来自/);
  // 截断时要说清"会一并删掉没显示的那些"。
  assert.match(panel, /\{truncated && <>列表当前只显示前 \{findingsLimit \|\| openCount\} 条/);
});

test("卡片点选是「控件优先」：整卡可点但按钮/链接/输入框不受影响", () => {
  assert.match(panel, /function isCardControl\(target: EventTarget \| null\): boolean \{/);
  assert.match(panel, /target\.closest\("button,a,input,select,textarea,label"\) !== null/);
  assert.match(panel, /onClick=\{selectable \? \(event\) => \{ if \(isCardControl\(event\.target\)\) return; onToggleSelect\?\.\(\); \} : undefined\}/);
});

test("卡片复选框与类型徽标同一行，且是可聚焦的原生控件", () => {
  // 单起一个网格行会让每张卡凭空长高一行（任务看板批量管理踩过同一个坑）。
  // 切片必须从首行容器往后找闭合，别从文件里找"下一个徽标"——编辑态里也有一个徽标，在它之前。
  const topStart = panel.indexOf('<div className="insight-card-top">');
  assert.ok(topStart > 0, "卡片首行容器还在");
  const top = panel.slice(topStart, panel.indexOf("</div>", topStart));
  assert.match(top, /className="insight-check"/);
  assert.match(top, /insight-card-type/);
  // 复选框必须排在这一行的第一个（排在徽标后面就不叫"同一行"了，只是恰好挨着）。
  assert.ok(top.indexOf('className="insight-check"') < top.indexOf("insight-card-type"), "复选框排在徽标之前");
  assert.match(panel, /aria-label=\{`选择「\$\{finding\.title\}」`\}/);
  // 卡片上的复选框是整卡点选的补充，不是替代品：它得能 Tab 到、能按空格。
  assert.doesNotMatch(top, /aria-hidden/);
});

test("选择模式的样式与任务看板同一套（18×18 / 1.5px 描边 / 圆角 5px / 勾用伪元素）", () => {
  // 尺寸与描边都要钉死数值：只断言"有 width/height"挡不住 18→14、1.5px→1px 这类走样
  // （同一条框在任务看板上是 18/1.5/5，两边不一致本身就是误导）。
  assert.match(styles, /\.insight-check \{[\s\S]{0,400}width: 18px;[\s\S]{0,200}height: 18px;[\s\S]{0,200}border: 1\.5px solid #a9c4b6;[\s\S]{0,200}border-radius: 5px;/);
  assert.match(styles, /\.insight-check::after \{[\s\S]{0,300}border-left: 2px solid #fff;/);
  assert.match(styles, /\.insight-card\.selectable \{ cursor: pointer; \}/);
});

test("悬停规则显式排除已选/半选（否则鼠标停在那颗上会掉回浅底）", () => {
  assert.match(styles, /\.insight-check:hover:not\(:disabled\):not\(:checked\):not\(:indeterminate\)/);
  assert.match(styles, /\.insight-check:checked,\s*\n\.insight-check:indeterminate \{/);
});

test("选中态规则必须排在卡片悬停规则之后（同特异性，靠顺序生效）", () => {
  const hover = styles.indexOf(".insight-card:hover {");
  const selected = styles.indexOf(".insight-card.selected {");
  assert.ok(hover > 0, "卡片悬停规则还在");
  assert.ok(selected > 0, "选中态规则还在");
  assert.ok(selected > hover, "选中态必须晚于悬停规则，否则悬停会把选中态盖掉");
  // 折叠区（已失效/已忽略）的置灰规则更早，选中态同样要能盖住它们。
  assert.ok(selected > styles.indexOf(".insight-card.invalidated {"));
  assert.ok(selected > styles.indexOf(".insight-card.dismissed {"));
});

test("选择条与两个危险动作分得开（一个实底、一个描边）", () => {
  assert.match(styles, /\.insights-bulk \.danger \{[\s\S]{0,400}background: #c0463a;/);
  assert.match(styles, /\.insights-bulk \.danger\.ghost \{[^}]*background: #fff;/);
  assert.match(styles, /\.insights-bulk \.danger:disabled \{/);
});

test("截断提示必须报出真实总数（不能只报看得见的那些）", () => {
  // openCount 是后端给的真实总数（不受 insightFindingsListLimit 截断），三个「全部…」动作
  // 在后端也是全量执行 —— 提示里丢掉它，用户就会以为"全部"= 列表上这 500 条。
  const notice = panel.slice(panel.indexOf("{truncated && ("), panel.indexOf("</section>\n          )}"));
  assert.match(notice, /有效建议共 \{openCount\} 条/);
  assert.match(notice, /列表仅显示最近 \{findingsLimit \|\| openCount\} 条/);
  assert.match(notice, /「全部添加为任务」「验证全部」「全部删除」作用于全部 \{openCount\} 条/);
  // 四个确认框/汇总行上的数字也必须是 openCount（不是 findings.length）。
  assert.doesNotMatch(code, /全部 <b>\{findings\.length\}/);
  assert.match(panel, /共 \{openCount\} 条有效建议/);
});

test("一条都没删掉时不报成「成功删除 0 条」", () => {
  assert.match(panel, /if \(res\.deleted > 0\) \{/);
  assert.match(panel, /toast\.info\(`选中的 \$\{res\.skipped\} 条建议已不在列表里，未做改动`\)/);
});

test("选择集在确认框开着时被清空 → 收起确认框，不留一个删 0 条的框", () => {
  // 2 秒轮询会边删边刷新：选中的建议被别处删光时，确认框里的 selected.size 会变成 0，
  // 而 deleteSelected 原本在这条路径上直接 return —— 框永远挂着、点确认什么都不发生。
  const fn = panel.slice(panel.indexOf("const deleteSelected = async () => {"), panel.indexOf("// 删全部有效建议"));
  assert.match(fn, /if \(bulkDeleting\) return;/);
  // 整块断言（不是"里面有这几个词"）：少了 return、换成别的分支、改掉文案都会在这里红。
  assert.match(fn, /if \(ids\.length === 0\) \{\n      setConfirmBulkDelete\(null\);\n      toast\.info\("选中的建议已不在列表里，无需删除"\);\n      return;\n    \}/);
  // 失败时保留选择集的理由必须写出来：后端整批一个事务 = 失败即没删，重试才有意义。
  assert.match(fn, /失败即一条都没删/);
});

test("窄屏只收起发现性提示，范围提示必须留着", () => {
  // 与任务看板批量栏同一条约定：发现性提示可以被藏起来，说明"选择范围被改小了"的那条不能。
  // 只断言"收起了某条"会漏掉"把两条一起藏掉"，所以这里盯的是选择器本身带 data-scope 限定。
  assert.match(styles, /@media \(max-width: 820px\) \{[\s\S]{0,400}\.insights-bulk-hint\[data-scope="all"\] \{ display: none; \}/);
  assert.match(styles, /\.insights-bulk-hint\[data-scope="filtered"\] \{ color: #8b6524; \}/);
});

test("模型层导出选择集纯函数（行为断言在 insights-model.test.ts）", () => {
  for (const name of ["toggleInsightSelection", "toggleInsightSelectionAll", "pruneInsightSelection", "insightSelectionSummary"]) {
    assert.match(model, new RegExp(`export function ${name}\\(`), `${name} 必须导出`);
  }
});
