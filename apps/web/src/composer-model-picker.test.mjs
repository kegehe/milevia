import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

// 底部模型选择器（docs/36）——会话级模型覆盖的前端契约。
//
// 修的是什么：底部栏原来只把模型名当纯文本显示，用户没法像在 Claude Code / Codex 的
// `/model` 里那样换模型。现在它是个可点入口，选择写进**会话级**覆盖
// （conversations.model_override），由控制服务在下一次运行以 --model / -c model= 注入。
//
// 这里锁住四条容易回退的性质：
//   1. 读写都打会话级模型接口，而不是去改项目级 profile（后者会连累同项目其他会话）；
//   2. 运行中不允许切换——换模型要退役长驻进程，忙时切会撞上 409；
//   3. 底部显示的是"接下来会用的模型"（会话指定优先），不是"上次实际用的"；
//   4. 弹层走 portal + fixed，否则移动端 .composer-usage 的 overflow-x:auto 会把它裁掉。
//
// 换行先归一化成 LF（仓库 core.autocrlf=true，工作区是 CRLF，带 \r 的锚点会静默失配）。
const normalize = (text) => text.replace(/\r\n/g, "\n");
const conversationPage = normalize(await readFile(new URL("./pages/ConversationPage.tsx", import.meta.url), "utf8"));
const stylesheet = normalize(await readFile(new URL("./conversation.css", import.meta.url), "utf8"));
const types = normalize(await readFile(new URL("./lib/types.ts", import.meta.url), "utf8"));

test("the picker reads the catalog and writes a session-scoped override", () => {
  const picker = conversationPage.match(/function ComposerModelPicker\([\s\S]*?\n\}/)?.[0] ?? "";
  assert.ok(picker, "找不到 ComposerModelPicker");
  assert.match(picker, /api<ConversationModels>\(`\/api\/conversations\/\$\{conversationID\}\/models`\)/);
  assert.doesNotMatch(picker, /agent-profiles/, "模型选择器不得去改项目级档案");

  const selectBody = conversationPage.match(/const selectConversationModel = async \(model: string\)[\s\S]*?\n  \};/)?.[0] ?? "";
  assert.ok(selectBody, "找不到 selectConversationModel");
  assert.match(selectBody, /`\/api\/conversations\/\$\{conversationID\}\/model`, \{ method: "POST", body: JSON\.stringify\(\{ model \}\) \}/);
  // 后端返回更新后的会话，前端据此立刻刷新底部显示，不必等下一次用量刷新。
  assert.match(selectBody, /setConversation\(updated\)/);

  // "跟随配置"发的是空串（= 清除覆盖），与后端语义一致。
  assert.match(picker, /onClick=\{\(\) => void choose\(""\)\}/);
});

test("switching is blocked while a run is active", () => {
  // 与权限菜单同一条规则：运行中/清空中/停止中都不放行。
  assert.match(conversationPage, /disabled=\{readOnly \|\| Boolean\(run\) \|\| stopping \|\| changingModel\}/);
  const selectBody = conversationPage.match(/const selectConversationModel = async \(model: string\)[\s\S]*?\n  \};/)?.[0] ?? "";
  assert.match(selectBody, /if \(!conversation \|\| readOnlyConversation \|\| \(conversation\.modelOverride \|\| ""\) === model \|\| run \|\| clearing \|\| stopping\) return false;/);
});

test("the footer shows the model that will be used next", () => {
  assert.match(conversationPage, /const displayedModel = conversation\?\.modelOverride \|\| usage\?\.context\.model \|\| currentUsage\?\.model/);
});

test("the popover escapes the scrolling composer footer", () => {
  // .composer-usage 在移动端是 overflow-x:auto——内联绝对定位的弹层会被裁掉。
  assert.match(conversationPage, /createPortal\(<div ref=\{menuRef\} className="model-menu"/);
  assert.match(stylesheet, /\.model-menu \{\n  position: fixed;/);
});

test("scrolling inside the menu does not dismiss it", () => {
  // 捕获阶段监听 scroll 用来在时间线滚动时收摊；菜单列表自己也滚（模型多时），
  // 不排除菜单内部就会"一滚就关"。
  const scrollHandler = conversationPage.match(/const onScroll = \(event: globalThis\.Event\) => \{[\s\S]*?\n    \};/)?.[0] ?? "";
  assert.ok(scrollHandler, "找不到 onScroll");
  assert.match(scrollHandler, /menuRef\.current\?\.contains\(target\)/);
  assert.match(scrollHandler, /close\(\)/);
});

test("switching conversations drops the previous catalog", () => {
  // 相邻的两个会话可能是不同 Agent，残留目录会把上一个会话的模型列给下一个人看。
  assert.match(conversationPage, /useEffect\(\(\) => \{ requestSeq\.current \+= 1; setView\(null\); setCustom\(""\); setOpen\(false\); \}, \[conversationID\]\);/);
  // 切换会话正好发生在 /models 飞行途中时，旧响应必须被丢弃，否则它会把旧目录写回来。
  assert.match(conversationPage, /const seq = \(requestSeq\.current \+= 1\);/);
  assert.match(conversationPage, /if \(seq !== requestSeq\.current\) return;\n      setView\(data\);/);
});

test("starting a run while the menu is open dismisses it", () => {
  // 菜单浮在输入框上方，用户可以直接回车发起任务；任务中切模型是被拒绝的，
  // 菜单留在那里会变成"点了没反应"。
  assert.match(conversationPage, /useEffect\(\(\) => \{ if \(runActive\) setOpen\(false\); \}, \[runActive\]\);/);
  assert.match(conversationPage, /runActive=\{Boolean\(run\)\}/);
});

test("custom model names are validated with the same charset as the backend", () => {
  // 与 conversation_models.go 的 modelOverridePattern 保持一致：字母数字开头，
  // 后接字母数字与 . _ : - / @ +。
  assert.match(conversationPage, /const customValid = \/\^\[A-Za-z0-9\]\[A-Za-z0-9\._:@\/\+\-\]\*\$\/\.test\(custom\.trim\(\)\);/);
  assert.match(conversationPage, /maxLength=\{128\}/);
});

test("the conversation payload carries the override so the chip survives reloads", () => {
  assert.match(types, /export type Conversation = \{[^}]*modelOverride\?: string/);
  assert.match(types, /export type ConversationModels = \{/);
});
