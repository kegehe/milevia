import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

// 「支持哪些工具」由服务端目录决定；冲突分析随内容响应下发，兼容手机中继。
//
// 这个文件守的是本次整理要消灭的那一类写法：
//
//   agentID === "codex" ? "Codex" : "Claude Code"
//
// 它的默认分支永远落在 Claude 上 —— 新增第三个工具时界面会把它静默标成
// "Claude Code"，**编译不报错、原测试也不报错**。所以这里改为按"危险形状"扫描源码，
// 而不是逐个文件去钉具体某一行。

const registry = await readFile(new URL("./lib/agent-registry.ts", import.meta.url), "utf8");

const SOURCES = {
  "pages/ConversationPage.tsx": await readFile(new URL("./pages/ConversationPage.tsx", import.meta.url), "utf8"),
  "pages/AgentProfilesPage.tsx": await readFile(new URL("./pages/AgentProfilesPage.tsx", import.meta.url), "utf8"),
  "pages/ScheduledTasksPage.tsx": await readFile(new URL("./pages/ScheduledTasksPage.tsx", import.meta.url), "utf8"),
  "pages/OrchestrationPage.tsx": await readFile(new URL("./pages/OrchestrationPage.tsx", import.meta.url), "utf8"),
  "features/run/ProjectAiConfigDialog.tsx": await readFile(new URL("./features/run/ProjectAiConfigDialog.tsx", import.meta.url), "utf8"),
  "features/insights/InsightsPanel.tsx": await readFile(new URL("./features/insights/InsightsPanel.tsx", import.meta.url), "utf8"),
  "features/git/ConflictSolveView.tsx": await readFile(new URL("./features/git/ConflictSolveView.tsx", import.meta.url), "utf8"),
};

// 危险形状 = 三元式的**兜底分支**落在另一个工具的文案/类名上，或者前端自己维护
// 一份工具清单。前者会让新工具冒充老工具，后者是"两份名单必然分叉"。
const DANGEROUS = [
  '? "Codex" : "Claude Code"',
  '? "Codex" : "Claude"',
  '? "codex" : "claude"',
  '? " Claude" : ',
  '? " Codex" : ',
  '(["claude-code", "codex"]',
  '[{ value: "claude-code"',
];

/**
 * 去掉注释。
 *
 * 负向断言（"不该出现某个写法"）**必须先剥注释**：注释里往往正写着"为什么不要这么写"，
 * 于是断言永远不可能红 —— 那样一条防线是空转的。本项目为此踩过两次。
 */
function stripComments(source) {
  return source
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .split("\n")
    .map((line) => line.replace(/\/\/.*$/, ""))
    .join("\n");
}

test("前端按工具 ID 分支的危险形状已经清零", () => {
  for (const [path, source] of Object.entries(SOURCES)) {
    const code = stripComments(source);
    for (const pattern of DANGEROUS) {
      assert.ok(
        !code.includes(pattern),
        `${path} 里仍有危险形状 ${JSON.stringify(pattern)}：新增工具会被静默当成已有工具`,
      );
    }
  }
});

test("工具名与能力只从 registry 取，页面里不再自己判断", () => {
  const code = stripComments(SOURCES["pages/ConversationPage.tsx"]);
  assert.match(code, /const toolName = agentDisplayName\(agentID\);/);
  assert.match(code, /const supportsCLICommands = agentSupportsSlashCommands\(agentID\);/);
  // 工具清单由目录驱动（地图出多少个工具就有多少张卡片），且对话只列出"可运行"的子集：
  // 目录里可能还有已收录、但尚未接通 AgentRunner 的工具（runnableInProject=false）。
  assert.match(code, /const runnableAgentEntries = useMemo\(\(\) => catalogEntries\.filter\(\(entry\) => entry\.runnableInProject !== false\), \[catalogEntries\]\)/);
  assert.match(code, /\{runnableAgentEntries\.map\(\(entry\) => \{/);
});

test("registry 的显示名不会回落到另一个工具的名字", async () => {
  const source = stripComments(registry);
  // 未知 ID 原样返回 id：`?? agentID`。写成 `?? "Claude Code"` 就把"不知道"说成了"就是它"。
  assert.match(source, /return agentEntry\(agentID\)\?\.name \?\? agentID;/);
  assert.doesNotMatch(source, /agentEntry\(agentID\)\?\.name \?\? "Claude/);
  // 未加载完成时"读不到"必须与"没有工具"分开表达。
  assert.match(source, /loaded: boolean;/);
  assert.match(source, /error: string;/);
});

test("工具状态只读 agents[]，不再回落读 claude / codex 两个旧字段", () => {
  const source = stripComments(registry);
  assert.match(source, /return runner\?\.agents\?\.find\(\(item\) => item\.id === agentID\)/);
  assert.doesNotMatch(source, /runner\?\.claude/);
  assert.doesNotMatch(source, /runner\?\.codex/);
});

test("优化建议只列出已接通只读分析协议且当前环境就绪的工具", () => {
  const code = stripComments(SOURCES["features/insights/InsightsPanel.tsx"]);
  assert.match(code, /entry\.supportsProjectInsights === true && runnerAgentReady\(agentRunner, entry\.id\)/);
  assert.match(code, /agentRunnerLoading \|\| agentRunnerError \|\| availableAgentOptions\.length === 0/);
});

test("目录 id 到 AgentID 的收窄只有一个转换点", () => {
  const source = stripComments(registry);
  assert.match(source, /export function catalogAgentID\(entry: AgentCatalogEntry\): AgentID \{/);
  for (const [path, content] of Object.entries(SOURCES)) {
    const code = stripComments(content);
    // 表单控件的 `event.target.value as AgentID`（把 input 的值收窄）是允许的；
    // 不允许的是把**目录条目**或**整份清单**硬转型 —— 那种散落会让
    // "这里还没适配新工具"这条信息消失。
    assert.ok(!/as AgentID\[\]/.test(code), `${path} 里出现了 as AgentID[]（整份清单的硬转型）`);
    assert.ok(!/entry\.id as AgentID/.test(code), `${path} 里出现了目录条目的散落转型`);
  }
});

test("剩余的手机工具目录例外有据可依", async () => {
  // 手机端目前读不到控制服务的 GET /api/agents（它走云端 /api/remote/*），
  // MobileRemotePage 暂时保留本地清单；ConflictSolveView 已由冲突内容接口下发名单。
  const source = await readFile(new URL("./pages/MobileRemotePage.tsx", import.meta.url), "utf8");
  assert.match(source, /已知例外（docs\/42 §15）/);
});
