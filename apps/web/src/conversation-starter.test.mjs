import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

// 新会话起点（2026-10-28 方案 A）：Milevia 品牌图标 + 「有什么可以帮你？」问候
// + 四个带图标的胶囊建议。旧版「新会话 kicker + 从一个任务开始 + 2×2 文字按钮」
// 已整块移除 —— 这里把新结构钉住，防止回退或两份口径并存。
const conversationPage = await readFile(new URL("./pages/ConversationPage.tsx", import.meta.url), "utf8");
const conversationStyles = await readFile(new URL("./conversation.css", import.meta.url), "utf8");

test("新会话起点：品牌 mark + 问候语 + 图标胶囊的结构", () => {
  // 品牌图标复用 /milevia-mark.svg（与 DashboardPage / AppVersionTag 同一份资源）。
  assert.match(conversationPage, /className="conversation-starter-mark" src="\/milevia-mark\.svg"/);
  // 问候语与副题；旧版文案不再出现。
  assert.match(conversationPage, /<h2>有什么可以帮你？<\/h2><p>描述一个任务，或从下面挑一个常用的开始<\/p>/);
  assert.doesNotMatch(conversationPage, /从一个任务开始/);
  // 四个建议胶囊都带各自的图标组件，且仍走 startFromSuggestion 填草稿。
  assert.match(conversationPage, /icon: <StarterReviewIcon \/>/);
  assert.match(conversationPage, /icon: <StarterLocateIcon \/>/);
  assert.match(conversationPage, /icon: <StarterBuildIcon \/>/);
  assert.match(conversationPage, /icon: <StarterExplainIcon \/>/);
  assert.match(conversationPage, /\{suggestion\.icon\}<span>\{suggestion\.label\}<\/span><\/button>/);
  assert.match(conversationPage, /onClick=\{\(\) => startFromSuggestion\(suggestion\.prompt\)\}/);
});

test("conversation.css 承载胶囊与图标配色判据，旧版样式已移除", () => {
  // 居中一列布局（旧版是左对齐 grid + kicker）。
  assert.match(conversationStyles, /\.conversation-starter \{[^}]*flex-direction: column;[^}]*align-items: center;/s);
  // 胶囊形态：46px 高、药丸圆角、白底淡影。
  assert.match(conversationStyles, /\.conversation-starter-options button \{[^}]*min-height: 46px;[^}]*border-radius: 999px;/s);
  // 图标配色只在这一处定义：主色描边 + 浅绿填充面 + 实心点。
  assert.match(conversationStyles, /\.starter-chip-icon \{[^}]*stroke: #2c7567;/s);
  assert.match(conversationStyles, /\.starter-chip-icon \.starter-chip-accent \{ fill: #eaf5ee; stroke: none; \}/);
  assert.match(conversationStyles, /\.starter-chip-icon \.starter-chip-dot \{ fill: #2c7567; stroke: none; \}/);
  // 旧版判据不允许残留：kicker 样式、2×2 网格、按钮左移 hover。
  assert.doesNotMatch(conversationStyles, /\.conversation-starter > span/);
  assert.doesNotMatch(conversationStyles, /\.conversation-starter-options \{[^}]*grid-template-columns/s);
  assert.doesNotMatch(conversationStyles, /\.conversation-starter-options button[^{]*\{[^}]*transform: translateX/s);
  // 居中补偿：timeline 内边距左 16 右 0，空态用 left:-8px 拉回真居中。
  assert.match(conversationStyles, /\.conversation-starter \{[^}]*left: -8px;/s);
  // 空会话输入框强调对齐真实 DOM：composer 已无 textarea，选择器必须是 .composer-edit。
  assert.match(conversationStyles, /\.composer\.empty-session \.composer-edit \{/);
  assert.doesNotMatch(conversationStyles, /\.composer\.empty-session textarea/);
});
