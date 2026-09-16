// MCP 模板与表单的纯逻辑。
//
// 从 McpManagerPage.tsx 抽出来的原因很具体：这几条规则原先只能靠**正则扫页面源码**来"验证"，
// 于是"模板声明的依赖优先于表单命令"这种**优先级**写错了也照样绿 —— 变异检验实测抓出过一次
// （把 `fromPreset` 过滤成空数组，页面照样过测试）。凡是"结果取决于几行代码的顺序/优先级"的逻辑，
// 都要能被真的调用一次，而不是被 grep 一次。

import type { MCPPreset, MCPPresetCredential, MCPTransport } from "../../lib/types";

// parseKeyValueLines 解析「每行 KEY=VALUE」文本框。
//
// `#` 开头的行是注释 —— 模板的凭据提示就写成注释（见 presetGuidanceLines），必须跳过，
// 否则提示行会被当成真实的环境变量 / 请求头保存下去。这两条规则是一对，改一边必须改另一边。
export function parseKeyValueLines(text: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const raw of text.split("\n")) {
    const line = raw.trim();
    if (!line || line.startsWith("#")) continue;
    const sep = line.indexOf("=");
    if (sep <= 0) continue;
    const key = line.slice(0, sep).trim();
    const value = line.slice(sep + 1).trim();
    if (key) out[key] = value;
  }
  return out;
}

// parseLines 解析「每行一个」文本框（参数列表）。
export function parseLines(text: string): string[] {
  return text.split("\n").map((line) => line.trim()).filter(Boolean);
}

// keyValueLines 把已有的键值对渲染回文本框形态。
export function keyValueLines(values?: Record<string, string>): string {
  if (!values) return "";
  return Object.entries(values).map(([key, value]) => `${key}=${value}`).join("\n");
}

// transportLabel 把传输类型写成可读文案，供模板卡片与列表共用。
export function transportLabel(transport: MCPTransport): string {
  if (transport === "stdio") return "本地进程";
  if (transport === "http") return "远程 HTTP";
  return "远程 SSE";
}

// credentialPlaceholder 给出凭据输入框 / 提示行里的占位符：`Bearer <GitHub Token>` 这种形态。
//
// 前缀来自条目声明的 valuePrefix，不由前端按「是 header 就加 Bearer」去猜 —— 猜错的表现是
// 莫名其妙的 401，而用户根本不知道自己哪里填错了。
export function credentialPlaceholder(credential: MCPPresetCredential): string {
  return `${credential.valuePrefix || ""}<${credential.label}>`;
}

// presetGuidanceLines 把模板声明的凭据转成 `#` 注释行，预填进环境变量 / 请求头文本框。
//
// 目的：让用户一眼看到「该填哪个 key、填成什么形态」，同时**不写进任何值** —— 留空即是留空，
// 而不是写一条空的环境变量。落点由 credential.target 决定（env / header）。
//
// **值位置只放「待替换的占位符」，不要在行尾追加说明文字**：用户照提示取消注释后，
// `KEY=<必填>  ← 说明` 会解析成 `KEY=xx  ← 说明` —— token 末尾多一段可见的尾巴，
// 表现为 401 而不是「没填」，排查成本极高。说明统一放在表单的「模板要求」区块里。
export function presetGuidanceLines(preset: MCPPreset): { envText: string; headersText: string } {
  const env: string[] = [];
  const headers: string[] = [];
  for (const item of preset.credentials || []) {
    const line = `# ${item.key}=${credentialPlaceholder(item)}`;
    (item.target === "header" ? headers : env).push(line);
  }
  return { envText: env.join("\n"), headersText: headers.join("\n") };
}

// runtimeCommandsFor 决定「运行时依赖检查」要查哪些命令。
//
// 优先级：模板声明的 requires ＞ 表单里的启动命令。模板最清楚自己需要什么运行时；只有**没有**
// 模板来源时才回落表单命令（且仅 stdio 有意义 —— http 形态不拉起本地进程）。
export function runtimeCommandsFor(preset: MCPPreset | null, transport: MCPTransport, command: string): string[] {
  const fromPreset = (preset?.requires || []).map((item) => item.command);
  if (fromPreset.length > 0) return fromPreset;
  if (transport === "stdio" && command.trim()) return [command.trim()];
  return [];
}

// ─────────────────────────────────────────────────────────────────────────────
// 「一键连接」：把「要不要用户动手、动哪一步」算清楚。
//
// 这一层的存在意义就是让页面只做渲染：用户不懂 MCP，界面上出现「传输类型 / 环境 / 作用域」
// 这类问题只会把人劝退，所以这些判断必须收在一处、可被直接断言。
// ─────────────────────────────────────────────────────────────────────────────

export type MCPConnectPlan = {
  /** 支持浏览器授权：点一次就完事，不用自己去申请 Token。 */
  canOAuth: boolean;
  /** 需要用户自己填凭据（不想授权时才用得上）。 */
  needsCredential: boolean;
  /** 需要在目标环境先装运行时；是否真的存在由运行时检查实答。 */
  needsInstall: boolean;
};

export function connectPlanFor(preset: MCPPreset | null): MCPConnectPlan {
  return {
    canOAuth: !!preset?.oauth,
    needsCredential: (preset?.credentials || []).length > 0,
    needsInstall: (preset?.requires || []).length > 0,
  };
}

// presetBadges 给出目录卡片上那行「需要准备什么」。
//
// 口径：**只说用户要不要动手**，不带任何协议名词（不出现 stdio / npx / 环境变量 / 作用域）。
export function presetBadges(preset: MCPPreset): string[] {
  const plan = connectPlanFor(preset);
  const badges: string[] = [];
  if (plan.needsInstall) {
    for (const item of preset.requires || []) badges.push(`先装 ${item.label}`);
  }
  if (plan.canOAuth) {
    badges.push(plan.needsCredential ? "可授权，也可填密钥" : "点一次授权即可");
  } else if (plan.needsCredential) {
    badges.push(`需要 ${(preset.credentials || [])[0].label}`);
  } else if (!plan.needsInstall) {
    badges.push("无需准备");
  }
  return badges;
}

// cardActionLabel 是目录卡片上的按钮文案：OAuth 服务直接说「用浏览器登录」，
// 比一个笼统的「连接」更能让不懂的人敢点。
export function cardActionLabel(preset: MCPPreset): string {
  const plan = connectPlanFor(preset);
  if (plan.canOAuth && !plan.needsCredential) return "用浏览器登录";
  if (plan.needsInstall) return "检查并连接";
  return "连接";
}

// wizardStartsAt 决定向导从哪一屏开始。
//
// 不需要用户提供任何东西的条目（本地起步、无凭据）直接进检查屏 —— 让用户白点一次「下一步」
// 没有任何价值。
export function wizardStartsAt(preset: MCPPreset): "credential" | "check" {
  const plan = connectPlanFor(preset);
  return plan.needsCredential || plan.canOAuth ? "credential" : "check";
}

// credentialsSatisfied 判断凭据屏能不能往下走。
//
// 支持授权的条目即使一个字都没填也放行 —— 用户可以改去点「用浏览器登录」。
export function credentialsSatisfied(preset: MCPPreset, secrets: MCPSecretInput): boolean {
  const plan = connectPlanFor(preset);
  if (!plan.needsCredential) return true;
  if (plan.canOAuth) return true;
  return (preset.credentials || []).every((item) => (secrets[item.key] || "").trim() !== "");
}

// groupPresetsByCategory 按服务端给的顺序分组，**不本地重排**（顺序是服务端的口径）。
export function groupPresetsByCategory(presets: MCPPreset[]): { category: string; items: MCPPreset[] }[] {
  const groups: { category: string; items: MCPPreset[] }[] = [];
  for (const preset of presets) {
    const category = preset.category || "";
    const existing = groups.find((group) => group.category === category);
    if (existing) existing.items.push(preset);
    else groups.push({ category, items: [preset] });
  }
  return groups;
}

export type MCPSecretInput = Record<string, string>;

// draftProbeValues 把用户输入整理成「真正要发出去」的 env / headers。
//
// **两个消费者共用这一份实现**，因为它们的失败方式一样恶心（401，而用户不知道自己哪填错了）：
//   - 创建请求：值进 envSecrets / headerSecrets，由服务端加密落库；
//   - 草稿态试连：**不落库**，值必须直接进 env / headers —— 否则试连根本不带凭据，
//     用户会看到一条假的「连不上」，然后反复检查一个其实完全正确的 token。
//
// 前缀只在这里补一次：条目声明 `ValuePrefix: "Bearer "` 时，用户只粘 token 本身。
export function draftProbeValues(preset: MCPPreset, secrets: MCPSecretInput): { env: Record<string, string>; headers: Record<string, string> } {
  const env: Record<string, string> = {};
  const headers: Record<string, string> = {};
  for (const item of preset.credentials || []) {
    const raw = (secrets[item.key] || "").trim();
    if (!raw) continue;
    const value = `${item.valuePrefix || ""}${raw}`;
    if (item.target === "header") headers[item.key] = value;
    else env[item.key] = value;
  }
  return { env, headers };
}

export type MCPDraftServerPayload = {
  name: string;
  displayName: string;
  description: string;
  transport: MCPTransport;
  command: string;
  args: string[];
  env: Record<string, string>;
  envSecrets: Record<string, string>;
  url: string;
  headers: Record<string, string>;
  headerSecrets: Record<string, string>;
  scope: "global";
  projectId: string;
  environments: string[];
  agents: string[];
  enabled: boolean;
  autoApproveTools: string[];
};

// buildDraftServerPayload 把「目录条目 + 用户只填的那一项 + 是否信任」拼成创建请求。
//
// 关键点是**默认值全给上**：作用域取全局、适用环境与 Agent 取全部、保存即启用 ——
// 这些问题用户答不出来，也不该由他答。密钥走 *Secrets 字段，交给服务端在保存时加密。
export function buildDraftServerPayload(
  preset: MCPPreset,
  secrets: MCPSecretInput,
  trust: boolean,
  environments: string[],
  agents: string[],
): MCPDraftServerPayload {
  const probe = draftProbeValues(preset, secrets);
  return {
    name: preset.name,
    displayName: preset.displayName,
    description: preset.summary || preset.description,
    transport: preset.transport,
    command: preset.command || "",
    args: preset.args || [],
    env: {},
    envSecrets: probe.env,
    url: preset.url || "",
    headers: {},
    headerSecrets: probe.headers,
    scope: "global",
    projectId: "",
    environments,
    agents,
    enabled: true,
    // 「以后不用再问我」= 放行该服务的全部工具，与工具级白名单共用同一套语法。
    autoApproveTools: trust ? [`mcp__${preset.name}__*`] : [],
  };
}
