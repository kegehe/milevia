import assert from "node:assert/strict";
import test from "node:test";

import { buildDraftServerPayload, cardActionLabel, connectPlanFor, credentialPlaceholder, credentialsSatisfied, draftProbeValues, groupPresetsByCategory, keyValueLines, parseKeyValueLines, parseLines, presetBadges, presetGuidanceLines, runtimeCommandsFor, transportLabel, wizardStartsAt } from "./mcp-model.ts";
import type { MCPPreset } from "../../lib/types.ts";

// 2026-09-15 修的模板缺陷里，凡是「靠几行代码的顺序 / 优先级成立」的规则都放在这里做**行为**断言。
// 起因：变异检验发现「运行时检查不再采用模板声明的依赖」这种变异**扫源码的断言挡不住**
// （文本里 `presetMeta?.requires` 与 `return fromPreset` 都还在，只是优先级被写反了）。

const remoteGitHub: MCPPreset = {
  id: "github",
  name: "github",
  displayName: "GitHub",
  description: "查看与修改代码仓库、Issue、PR",
  summary: "查看与修改代码仓库、Issue、PR",
  category: "常用服务",
  icon: "code",
  transport: "http",
  url: "https://api.githubcopilot.com/mcp/",
  environments: ["windows", "wsl", "remote-linux"],
  oauth: true,
  credentials: [{ key: "Authorization", target: "header", label: "GitHub Personal Access Token", description: "", valuePrefix: "Bearer " }],
};

const oauthOnly: MCPPreset = {
  id: "notion",
  name: "notion",
  displayName: "Notion",
  description: "读写你的 Notion 页面与数据库",
  summary: "读写你的 Notion 页面与数据库",
  category: "常用服务",
  icon: "docs",
  transport: "http",
  url: "https://mcp.notion.com/mcp",
  environments: ["windows", "wsl", "remote-linux"],
  oauth: true,
};

const localGitHub: MCPPreset = {
  id: "github-local",
  name: "github-local",
  displayName: "GitHub（本地容器）",
  description: "网络受限时在本地容器里跑 GitHub 工具",
  summary: "网络受限时在本地容器里跑 GitHub 工具",
  category: "本机运行",
  icon: "code",
  transport: "stdio",
  command: "docker",
  args: ["run", "-i", "--rm", "-e", "GITHUB_PERSONAL_ACCESS_TOKEN", "ghcr.io/github/github-mcp-server"],
  environments: ["windows", "wsl", "remote-linux"],
  requires: [{ command: "docker", label: "Docker" }],
  credentials: [{ key: "GITHUB_PERSONAL_ACCESS_TOKEN", target: "env", label: "GitHub Personal Access Token", description: "" }],
};

const localNoSetup: MCPPreset = {
  id: "memory",
  name: "memory",
  displayName: "长期记忆",
  description: "让 AI 记住跨会话的信息",
  summary: "让 AI 记住跨会话的信息",
  category: "本机运行",
  icon: "memory",
  transport: "stdio",
  command: "npx",
  args: ["-y", "@modelcontextprotocol/server-memory"],
  environments: ["windows", "wsl", "remote-linux"],
  requires: [{ command: "npx", label: "Node.js" }],
};

test("parseKeyValueLines 跳过注释行，并把畸形行挡在外面", () => {
  const parsed = parseKeyValueLines([
    "# Authorization=Bearer <你的 Token>  ← GitHub Personal Access Token",
    "  LOG_LEVEL=info  ",
    "",
    "   ",
    "NO_EQUALS_SIGN",
    "=no_key",
    "ROOT=${PROJECT_DIR}/docs",
  ].join("\n"));
  assert.deepEqual(parsed, { LOG_LEVEL: "info", ROOT: "${PROJECT_DIR}/docs" });
});

test("模板凭据提示永远不会变成值（两条规则必须成对）", () => {
  const env = presetGuidanceLines(localGitHub);
  const header = presetGuidanceLines(remoteGitHub);

  // 提示行确实把键名带过来了。
  assert.ok(env.envText.includes("GITHUB_PERSONAL_ACCESS_TOKEN"));
  assert.ok(header.headersText.includes("Authorization"));

  // 但解析回来必须是空的 —— 否则用户只是「点了模板、还没填」，就会存下一条空凭据。
  assert.deepEqual(parseKeyValueLines(env.envText), {});
  assert.deepEqual(parseKeyValueLines(header.headersText), {});
  assert.deepEqual(parseKeyValueLines(env.headersText), {});
  assert.deepEqual(parseKeyValueLines(header.envText), {});

  // 用户把注释行改成真值后，必须能解析出来。
  const filled = env.envText.replace(/^#\s*/, "").replace("<GitHub Personal Access Token>", "ghp_xxx");
  assert.deepEqual(parseKeyValueLines(filled), { GITHUB_PERSONAL_ACCESS_TOKEN: "ghp_xxx" });
});

test("取消注释并替换占位符后，值里不含任何残留说明文字", () => {
  // 行尾追加说明会被并进值里（`KEY=xx  ← 说明`），token 末尾多一段可见尾巴 ——
  // 表现为 401 而不是「没填」，排查成本极高。故值位置只允许出现待替换的占位符。
  const env = presetGuidanceLines(localGitHub);
  const header = presetGuidanceLines(remoteGitHub);

  for (const text of [env.envText, header.headersText]) {
    const uncommented = text.replace(/^#\s*/, "");
    const value = uncommented.slice(uncommented.indexOf("=") + 1);
    assert.match(value, /^Bearer <[^<>]+>$|^<[^<>]+>$/, `值位置只能是占位符，实际：${value}`);
    assert.doesNotMatch(value, /←|（|）|:/, `值位置混入了说明文字：${value}`);
  }

  // http 形态必须带上 Bearer 前缀，用户不用自己去查格式。
  assert.equal(header.headersText, "# Authorization=Bearer <GitHub Personal Access Token>");
  assert.equal(env.envText, "# GITHUB_PERSONAL_ACCESS_TOKEN=<GitHub Personal Access Token>");
});

test("模板凭据按落点分流到环境变量或请求头", () => {
  const env = presetGuidanceLines(localGitHub);
  assert.equal(env.headersText, "");
  assert.ok(env.envText.startsWith("# GITHUB_PERSONAL_ACCESS_TOKEN="));

  const header = presetGuidanceLines(remoteGitHub);
  assert.equal(header.envText, "");
  assert.ok(header.headersText.startsWith("# Authorization="));
});

test("运行时检查命令：模板声明的依赖优先于表单里的启动命令", () => {
  // 模板有 requires：用它，而不是表单里那个（表单可能已被用户改过，模板才知道这版配置要什么）。
  assert.deepEqual(runtimeCommandsFor(localGitHub, "stdio", "node"), ["docker"]);
  // 没有模板来源时才回落到表单命令，且只有 stdio 有意义。
  assert.deepEqual(runtimeCommandsFor(null, "stdio", "  npx  "), ["npx"]);
  assert.deepEqual(runtimeCommandsFor(null, "http", "npx"), []);
  assert.deepEqual(runtimeCommandsFor(null, "stdio", "   "), []);
  // http 形态的模板不带 requires，检查清单为空（环境无关，无需检查）。
  assert.deepEqual(runtimeCommandsFor(remoteGitHub, "http", ""), []);
  // 模板带多个依赖时全部保留。
  assert.deepEqual(runtimeCommandsFor({ ...localGitHub, requires: [{ command: "docker", label: "Docker" }, { command: "npx", label: "Node.js" }] }, "stdio", "node"), ["docker", "npx"]);
});

test("transportLabel 覆盖三种传输类型", () => {
  assert.equal(transportLabel("stdio"), "本地进程");
  assert.equal(transportLabel("http"), "远程 HTTP");
  assert.equal(transportLabel("sse"), "远程 SSE");
});

test("parseLines 与 keyValueLines 的形状与表单一致", () => {
  assert.deepEqual(parseLines("-y\n\n  @playwright/mcp@latest  \n"), ["-y", "@playwright/mcp@latest"]);
  assert.deepEqual(parseLines("   "), []);
  assert.equal(keyValueLines(undefined), "");
  assert.equal(keyValueLines({ A: "1", B: "2" }), "A=1\nB=2");
});

// ─────────────────────────────────────────────────────────────────────────────
// 一键连接：用户「要不要动手、动哪一步」全部由这里算，页面只负责渲染。
// ─────────────────────────────────────────────────────────────────────────────

test("凭据占位符带上条目声明的前缀，用户不用记 Bearer", () => {
  assert.equal(credentialPlaceholder(remoteGitHub.credentials![0]), "Bearer <GitHub Personal Access Token>");
  assert.equal(credentialPlaceholder(localGitHub.credentials![0]), "<GitHub Personal Access Token>");
});

test("connectPlanFor 分别回答「能不能授权 / 要不要填密钥 / 要不要先装东西」", () => {
  assert.deepEqual(connectPlanFor(oauthOnly), { canOAuth: true, needsCredential: false, needsInstall: false });
  assert.deepEqual(connectPlanFor(remoteGitHub), { canOAuth: true, needsCredential: true, needsInstall: false });
  assert.deepEqual(connectPlanFor(localGitHub), { canOAuth: false, needsCredential: true, needsInstall: true });
  assert.deepEqual(connectPlanFor(localNoSetup), { canOAuth: false, needsCredential: false, needsInstall: true });
  assert.deepEqual(connectPlanFor(null), { canOAuth: false, needsCredential: false, needsInstall: false });
});

test("目录卡片徽标只说用户要不要动手，不带协议名词", () => {
  assert.deepEqual(presetBadges(oauthOnly), ["点一次授权即可"]);
  assert.deepEqual(presetBadges(remoteGitHub), ["可授权，也可填密钥"]);
  assert.deepEqual(presetBadges(localGitHub), ["先装 Docker", "需要 GitHub Personal Access Token"]);
  assert.deepEqual(presetBadges(localNoSetup), ["先装 Node.js"]);
  // 用户不懂 MCP：这些词一个都不该出现在目录上。
  for (const preset of [oauthOnly, remoteGitHub, localGitHub, localNoSetup]) {
    for (const badge of presetBadges(preset)) {
      assert.doesNotMatch(badge, /stdio|http|sse|npx|uvx|环境变量|请求头|作用域/, `徽标出现协议名词：${badge}`);
    }
  }
});

test("卡片按钮文案对纯授权服务直说「用浏览器登录」", () => {
  assert.equal(cardActionLabel(oauthOnly), "用浏览器登录");
  assert.equal(cardActionLabel(remoteGitHub), "连接");
  assert.equal(cardActionLabel(localNoSetup), "检查并连接");
});

test("向导起点：能授权的条目先问准备，什么都不需要的直接进检查", () => {
  assert.equal(wizardStartsAt(oauthOnly), "credential");
  assert.equal(wizardStartsAt(remoteGitHub), "credential");
  assert.equal(wizardStartsAt(localGitHub), "credential");
  // 本地起步、又没有凭据要求（记忆这类）—— 让用户白点一次「下一步」没有价值。
  assert.equal(wizardStartsAt(localNoSetup), "check");
});

test("凭据屏放行规则：能授权时一个字不填也放行，只能填密钥时必须填全", () => {
  // 能授权 ⇒ 用户可以改去点授权，不该被「请填写」卡住。
  assert.equal(credentialsSatisfied(oauthOnly, {}), true);
  assert.equal(credentialsSatisfied(remoteGitHub, {}), true);
  // 只能填密钥 ⇒ 声明的每一项都必须填。
  assert.equal(credentialsSatisfied(localGitHub, {}), false);
  assert.equal(credentialsSatisfied(localGitHub, { GITHUB_PERSONAL_ACCESS_TOKEN: "   " }), false);
  assert.equal(credentialsSatisfied(localGitHub, { GITHUB_PERSONAL_ACCESS_TOKEN: "ghp_x" }), true);
  // 无凭据声明 ⇒ 直接放行。
  assert.equal(credentialsSatisfied(localNoSetup, {}), true);
});

test("分组保持服务端给的顺序，不本地重排", () => {
  const groups = groupPresetsByCategory([remoteGitHub, oauthOnly, localGitHub, localNoSetup]);
  assert.deepEqual(groups.map((group) => group.category), ["常用服务", "本机运行"]);
  assert.deepEqual(groups[0].items.map((item) => item.id), ["github", "notion"]);
  assert.deepEqual(groups[1].items.map((item) => item.id), ["github-local", "memory"]);
});

test("创建请求把默认值全给上，密钥只带条目声明的前缀", () => {
  const payload = buildDraftServerPayload(
    remoteGitHub,
    { Authorization: "ghp_secret", 不存在的键: "忽略" },
    false,
    ["windows", "wsl", "remote-linux"],
    ["claude-code"],
  );
  assert.equal(payload.name, "github");
  assert.equal(payload.description, remoteGitHub.summary);
  // 默认值：全局 + 全部环境 + 保存即启用 —— 这些用户答不出来，也不该由他答。
  assert.equal(payload.scope, "global");
  assert.equal(payload.projectId, "");
  assert.deepEqual(payload.environments, ["windows", "wsl", "remote-linux"]);
  assert.deepEqual(payload.agents, ["claude-code"]);
  assert.equal(payload.enabled, true);
  // 前缀由条目声明补上，用户只粘了裸 token。
  assert.deepEqual(payload.headerSecrets, { Authorization: "Bearer ghp_secret" });
  // 未声明 / 未填的凭据不写进请求，避免存下一条空值。
  assert.deepEqual(payload.envSecrets, {});
  assert.deepEqual(payload.headers, {});
  assert.deepEqual(payload.autoApproveTools, []);
});

test("「以后不用再问」只影响白名单，并把裸 token 交给会话级前置信任", () => {
  const trust = buildDraftServerPayload(localGitHub, { GITHUB_PERSONAL_ACCESS_TOKEN: "ghp_x" }, true, ["windows"], ["claude-code"]);
  assert.deepEqual(trust.autoApproveTools, ["mcp__github-local__*"]);
  // 环境变量凭据没有前缀概念，原样保存。
  assert.deepEqual(trust.envSecrets, { GITHUB_PERSONAL_ACCESS_TOKEN: "ghp_x" });
  const noTrust = buildDraftServerPayload(localGitHub, { GITHUB_PERSONAL_ACCESS_TOKEN: "ghp_x" }, false, ["windows"], ["claude-code"]);
  assert.deepEqual(noTrust.autoApproveTools, []);
});

test("空白输入不会被当成凭据存下去", () => {
  const payload = buildDraftServerPayload(remoteGitHub, { Authorization: "   " }, false, ["windows"], ["claude-code"]);
  assert.deepEqual(payload.headerSecrets, {});
});

test("试连与创建请求共用同一份凭据整理（前缀只补一次）", () => {
  const probe = draftProbeValues(remoteGitHub, { Authorization: "ghp_secret" });
  assert.deepEqual(probe.headers, { Authorization: "Bearer ghp_secret" });
  assert.deepEqual(probe.env, {});

  // 两个消费者必须给出同一份值：创建请求进 *Secrets，试连进 env / headers。
  // 两处各写一份实现时最典型的故障就是试连忘了补 Bearer —— 表现是 401，
  // 而用户会去反复检查一个完全正确的 token。
  const payload = buildDraftServerPayload(remoteGitHub, { Authorization: "ghp_secret" }, false, ["windows"], ["claude-code"]);
  assert.deepEqual(payload.headerSecrets, probe.headers);
  assert.deepEqual(payload.envSecrets, probe.env);
  // 创建请求的 env / headers 是「手动配置」的裸值通道，目录路径下必须为空。
  assert.deepEqual(payload.headers, {});
  assert.deepEqual(payload.env, {});
});

test("试连凭据按落点分流，空白不参与", () => {
  assert.deepEqual(draftProbeValues(localGitHub, { GITHUB_PERSONAL_ACCESS_TOKEN: "ghp_x" }), {
    env: { GITHUB_PERSONAL_ACCESS_TOKEN: "ghp_x" },
    headers: {},
  });
  assert.deepEqual(draftProbeValues(remoteGitHub, { Authorization: "  " }), { env: {}, headers: {} });
  assert.deepEqual(draftProbeValues(oauthOnly, {}), { env: {}, headers: {} });
});

