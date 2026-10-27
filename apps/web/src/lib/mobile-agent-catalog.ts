import type { AgentID, PermissionMode } from "./types";
import type { AgentLogoKey } from "./cli-tools-view";

export type MobileAgent = {
  id: AgentID;
  name: string;
  vendor: string;
  /** 中文展示名；vendor 保留服务端目录原值。 */
  vendorLabel?: string;
  /** 没有官方图标资产时留空，界面回落两字母牌（与 desktop 的 agentLogoKey 约定一致）。 */
  logo?: AgentLogoKey;
  permissionModes: PermissionMode[];
  defaultPermission: PermissionMode;
};

// 手机端读不到 GET /api/agents，设置页与远程控制页共用这一份目录镜像。
// 名称、厂商、权限档位及默认权限须与 control-server 的 agent_catalog.go 一致，
// 由 mobile-agent-catalog.test.ts 校验；默认权限使用目录声明值。
export const MOBILE_AGENTS: readonly MobileAgent[] = [
  { id: "claude-code", name: "Claude Code", vendor: "Anthropic", logo: "claude", permissionModes: ["approval_required", "full_control"], defaultPermission: "approval_required" },
  { id: "codex", name: "Codex", vendor: "OpenAI", logo: "openai", permissionModes: ["read_only", "workspace_write", "full_control"], defaultPermission: "workspace_write" },
  { id: "codebuddy", name: "CodeBuddy Code", vendor: "Tencent Cloud", vendorLabel: "腾讯云", logo: "codebuddy", permissionModes: ["read_only", "workspace_write", "full_control"], defaultPermission: "workspace_write" },
  { id: "qoder", name: "Qoder CLI", vendor: "Alibaba", vendorLabel: "阿里云", permissionModes: ["read_only", "workspace_write", "full_control"], defaultPermission: "workspace_write" },
  { id: "opencode", name: "OpenCode", vendor: "Anomaly", permissionModes: ["read_only", "workspace_write", "full_control"], defaultPermission: "workspace_write" },
  { id: "cursor", name: "Cursor CLI", vendor: "Anysphere", permissionModes: ["read_only", "workspace_write", "full_control"], defaultPermission: "workspace_write" },
];

export function mobileAgent(id: string): MobileAgent | undefined {
  return MOBILE_AGENTS.find((agent) => agent.id === id);
}
