import type { AgentID, PermissionMode } from "./types";
import type { AgentLogoKey } from "./cli-tools-view";

export type MobileAgent = {
  id: AgentID;
  name: string;
  vendor: string;
  /** 没有官方图标资产时留空，界面回落两字母牌（与 desktop 的 agentLogoKey 约定一致）。 */
  logo?: AgentLogoKey;
  permissionModes: PermissionMode[];
  defaultPermission: PermissionMode;
};

export const MOBILE_AGENTS: readonly MobileAgent[] = [
  { id: "claude-code", name: "Claude Code", vendor: "Anthropic", logo: "claude", permissionModes: ["approval_required", "full_control"], defaultPermission: "approval_required" },
  { id: "codex", name: "Codex", vendor: "OpenAI", logo: "openai", permissionModes: ["read_only", "workspace_write", "full_control"], defaultPermission: "workspace_write" },
  { id: "codebuddy", name: "CodeBuddy Code", vendor: "Tencent Cloud", logo: "codebuddy", permissionModes: ["read_only", "workspace_write", "full_control"], defaultPermission: "workspace_write" },
  { id: "qoder", name: "Qoder CLI", vendor: "Alibaba", permissionModes: ["read_only", "workspace_write", "full_control"], defaultPermission: "workspace_write" },
  { id: "opencode", name: "OpenCode", vendor: "Anomaly", permissionModes: ["read_only", "workspace_write", "full_control"], defaultPermission: "workspace_write" },
  { id: "cursor", name: "Cursor CLI", vendor: "Anysphere", permissionModes: ["read_only", "workspace_write", "full_control"], defaultPermission: "workspace_write" },
];

export function mobileAgent(id: string): MobileAgent | undefined {
  return MOBILE_AGENTS.find((agent) => agent.id === id);
}
