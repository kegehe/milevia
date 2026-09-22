import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

// 命令选择器（docs/37）——「常用命令」从自由输入改成从 CLI 真实可用的命令里选。
//
// 修的是什么：编辑器原来是一个自由文本框，用户填的 `/xxx` 未必被 CLI 识别（填错只得到一句
// Unknown command，还白留一条用户消息与一个 Run；填 `/review` 这种已被隐藏的命令则照常执行、
// 照常花钱）。现在默认从 CLI 自己的命令目录里选。
//
// 这里锁住五条容易回退的性质：
//   1. 目录来自控制服务（CLI 的 init 事件），前端不自己编造命令表；
//   2. 打开项目不带探针（probe=0），只有用户点「刷新」才 refresh=1；
//   3. "当前 CLI 未提供" 只在目录权威时显示——静态候选下没找到不代表失效；
//   4. Codex 会话不提供斜杠命令入口（codex exec 不解析斜杠），但保留自定义 shell 命令；
//   5. 输入框里手敲的未知斜杠命令只提示、不阻断（目录有 TTL，硬拦会误伤刚建的命令），
//      而 Codex 的斜杠命令是确定无解的组合，直接拦下。
//
// 换行先归一化成 LF（仓库 core.autocrlf=true，工作区是 CRLF，带 \r 的锚点会静默失配）。
const normalize = (text) => text.replace(/\r\n/g, "\n");
const conversationPage = normalize(await readFile(new URL("./pages/ConversationPage.tsx", import.meta.url), "utf8"));
const stylesheet = normalize(await readFile(new URL("./conversation.css", import.meta.url), "utf8"));
const types = normalize(await readFile(new URL("./lib/types.ts", import.meta.url), "utf8"));

test("the catalog comes from the control service", () => {
  // 打开项目：probe=0——只是打开一个项目不该拉起 CLI 进程。
  assert.match(conversationPage, /`\/api\/projects\/\$\{projectId\}\/commands\?agentId=\$\{encodeURIComponent\(agentId\)\}&probe=0`/);
  // 用户点「刷新目录」：refresh=1——忽略服务端 TTL 重新探测。
  const refreshBody = conversationPage.match(/const refreshCommandCatalog = useCallback\([\s\S]*?\n  \}, \[[^\]]*\]\);/)?.[0] ?? "";
  assert.ok(refreshBody, "找不到 refreshCommandCatalog");
  assert.match(refreshBody, /refresh=1/);
  // 目录类型必须带权威标记：前端靠它区分"CLI 说没有"与"我们没读到"。
  assert.match(types, /export type ProjectCommands = \{[\s\S]*?authoritative: boolean;/);
});

test("the picker fills the template from the selected command", () => {
  // 选中的命令决定模板，避免"选了 A 却存下 B"。
  assert.match(conversationPage, /const resolvedTemplate = mode === "cli" \? `\/\$\{selected\}` : template;/);
  assert.match(conversationPage, /template: resolvedTemplate,/);
  // 列表项展示命令名，选中态可辨。
  assert.match(conversationPage, /role="option" aria-selected=\{command\.name === selected\}/);
  // 搜索是长尾命令的唯一入口，必须存在。
  assert.match(conversationPage, /placeholder="搜索命令，例如：compact \/ 上下文 \/ 审查"/);
});

test("a stale command is only flagged when the catalog is authoritative", () => {
  const availability = conversationPage.match(/const commandAvailability = useCallback\([\s\S]*?\n  \}, \[[^\]]*\]\);/)?.[0] ?? "";
  assert.ok(availability, "找不到 commandAvailability");
  // 判据来自目录的能力声明（slashCommands），不再写死 codex。
  assert.match(availability, /conversation && !agentSupportsSlashCommands\(conversation\.agentId\)\) return "unsupported";/);
  assert.match(availability, /commandCatalog\?\.authoritative && !commandCatalog\.commands\.some/);
  // 自定义 shell 命令不是斜杠命令，与命令目录无关，永远可用。
  assert.match(availability, /if \(!commandName\) return "ok";/);
});

test("codex conversations do not offer slash commands", () => {
  // 编辑器里 CLI 命令那一档对 Codex 禁用，并说明原因。
  assert.match(conversationPage, /const supportsCLICommands = agentSupportsSlashCommands\(agentID\);/);
  assert.match(conversationPage, /disabled=\{!supportsCLICommands\} onClick=\{\(\) => setMode\("cli"\)\}/);
  // 发送路径上再拦一层：任何调用方都不该把 `/xxx` 当普通提示词发给 Codex。
  assert.match(conversationPage, /const slashName = slashCommandName\(draft\);\n    if \(slashName && !agentSupportsSlashCommands\(conversation\.agentId\)\) \{/);
});

test("typing an unknown slash command warns instead of blocking", () => {
  const hint = conversationPage.match(/const composerSlashHint = useMemo\([\s\S]*?\n  \}, \[[^\]]*\]\);/)?.[0] ?? "";
  assert.ok(hint, "找不到 composerSlashHint");
  // 只有目录权威时才提示（否则会把有效命令说成不存在）。
  assert.match(hint, /if \(!commandCatalog\?\.authoritative\) return null;/);
  // 目录里有的命令不提示。
  assert.match(hint, /commandCatalog\.commands\.some\(\(command\) => command\.name === commandName\)\) return null;/);
  // 提示带"最像的那条"，点一下就能改对。
  assert.match(conversationPage, /你是想用 <code>\/\{composerSlashHint\.suggestion\}<\/code> 吗？/);
  assert.match(conversationPage, /onClick=\{\(\) => setComposerText\(`\/\$\{composerSlashHint\.suggestion\} `, conversation\?\.id\)\}/);
  // sendContent 里没有针对"未知命令"的 return false——提示不等于阻断。
  const sendBody = conversationPage.match(/const sendContent = async \([\s\S]*?\n  \};/)?.[0] ?? "";
  assert.ok(sendBody, "找不到 sendContent");
  assert.doesNotMatch(sendBody, /closestCommandName/);
});

test("the picker is reachable from the rail with the catalog wired in", () => {
  assert.match(conversationPage, /agentID=\{conversation\?\.agentId \|\| "claude-code"\} catalog=\{commandCatalog\} catalogLoading=\{commandCatalogLoading\} refreshCatalog=\{refreshCommandCatalog\}/);
  assert.match(stylesheet, /\.command-picker-list \{ display: grid; max-height: 264px; gap: 10px; overflow-y: auto;/);
  assert.match(stylesheet, /\.quick-tag\.stale > button:first-child/);
});
