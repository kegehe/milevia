// MCP 连接管理页 — 管理注入到 AI 会话的外部 MCP server。
// 与 SSH 管理页同构：以 DashboardPage 为底、弹出管理面板；复用 ssh-* 样式类。

import { useCallback, useEffect, useMemo, useState } from "react";
import { toast } from "sonner";
import { useNavigate } from "react-router-dom";
import { useProjectContext } from "../stores/useProjectStore";
import { openExternal } from "../lib/runtime";
import { ConfirmDialog } from "../components/ConfirmDialog";
import type { MCPAuditEntry, MCPAuditResponse, MCPImportCandidate, MCPImportResult, MCPInjectionStatus, MCPOAuthStatus, MCPPreviewResult, MCPProjectView, MCPScope, MCPServer, MCPTestResult, MCPTransport } from "../lib/types";
import DashboardPage from "./DashboardPage";

const ENVIRONMENT_OPTIONS: { id: string; label: string }[] = [
  { id: "windows", label: "Windows" },
  { id: "wsl", label: "WSL" },
  { id: "remote-linux", label: "SSH 远端" },
];

const AGENT_OPTIONS: { id: string; label: string }[] = [
  { id: "claude-code", label: "Claude Code" },
  { id: "codex", label: "Codex（P1 起支持）" },
];

function McpIcon({ className = "ssh-icon" }: { className?: string }) {
  return <svg className={className} viewBox="0 0 24 24" aria-hidden="true"><rect x="4" y="4" width="7" height="7" rx="1.5" /><rect x="13" y="13" width="7" height="7" rx="1.5" /><path d="M11 7.5h4.5a1.5 1.5 0 0 1 1.5 1.5V11M13 16.5H8.5A1.5 1.5 0 0 1 7 15v-2" /></svg>;
}

function CloseIcon() {
  return <svg className="ssh-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="m7 7 10 10M17 7 7 17" /></svg>;
}

function EditIcon() {
  return <svg className="ssh-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M5 19l1.5-4.5L16 5a1.9 1.9 0 0 1 2.7 0l.3.3a1.9 1.9 0 0 1 0 2.7l-9.5 9.5L5 19M11 6.5l6.5 6.5M5 19h4" /></svg>;
}

function TrashIcon() {
  return <svg className="ssh-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M4.5 7h15M9 7V4.5h6V7M7 7l.8 12.5h8.4L17 7M10 11v5M14 11v5" /></svg>;
}

const DECISION_LABELS: Record<string, { label: string; severity: "info" | "warn" | "danger" }> = {
  allow: { label: "已批准", severity: "info" },
  auto_allow: { label: "免审批放行", severity: "warn" },
  deny: { label: "已拒绝", severity: "danger" },
  aborted: { label: "客户端断开", severity: "warn" },
  timeout: { label: "审批超时", severity: "warn" },
};

function decisionLabel(decision?: string) {
  return DECISION_LABELS[decision || ""]?.label || "待裁决";
}

function decisionSeverity(decision?: string) {
  return DECISION_LABELS[decision || ""]?.severity || "info";
}

function statusSeverity(status?: string) {
  if (status === "error") return "danger";
  if (status === "pending") return "warn";
  return "info";
}

// mcpPreviewRows 把预览结果摊平成 [标签, 值] 行，避免在 JSX 里嵌套 Fragment 又要补 key。
function mcpPreviewRows(result: MCPPreviewResult): [string, string][] {
  const rows: [string, string][] = [["项目路径", result.projectPath || "（未选择项目）"]];
  if (result.command) rows.push(["启动命令", result.command]);
  if (result.args.length > 0) rows.push(["参数", result.args.join(" ")]);
  if (result.url) rows.push(["地址", result.url]);
  for (const [key, value] of Object.entries(result.env)) rows.push([`env:${key}`, value]);
  for (const [key, value] of Object.entries(result.headers)) rows.push([`header:${key}`, value]);
  return rows;
}

// mcpSchemaText 把工具的 inputSchema 渲染成可读 JSON。schema 来自外部 server，
// 形状不可预期，故对无法序列化的值退化为原样字符串。
function mcpSchemaText(schema: unknown): string {
  if (schema === null || schema === undefined) return "";
  try {
    return JSON.stringify(schema, null, 2);
  } catch {
    return String(schema);
  }
}

function parseKeyValueLines(text: string): Record<string, string> {
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

function parseLines(text: string): string[] {
  return text.split("\n").map((line) => line.trim()).filter(Boolean);
}

function keyValueLines(values?: Record<string, string>): string {
  if (!values) return "";
  return Object.entries(values).map(([key, value]) => `${key}=${value}`).join("\n");
}

type FormState = {
  name: string;
  displayName: string;
  description: string;
  transport: MCPTransport;
  command: string;
  argsText: string;
  envText: string;
  url: string;
  headersText: string;
  scope: MCPScope;
  projectId: string;
  environments: string[];
  agents: string[];
  enabled: boolean;
  autoApproveText: string;
  envSecretKeys: string[];
  headerSecretKeys: string[];
  envSecrets: Record<string, string>;
  headerSecrets: Record<string, string>;
};

function emptyForm(): FormState {
  return {
    name: "",
    displayName: "",
    description: "",
    transport: "stdio",
    command: "",
    argsText: "",
    envText: "",
    url: "",
    headersText: "",
    scope: "global",
    projectId: "",
    environments: ENVIRONMENT_OPTIONS.map((item) => item.id),
    agents: ["claude-code"],
    enabled: true,
    autoApproveText: "",
    envSecretKeys: [],
    headerSecretKeys: [],
    envSecrets: {},
    headerSecrets: {},
  };
}

function formFromServer(server: MCPServer): FormState {
  return {
    name: server.name,
    displayName: server.displayName,
    description: server.description,
    transport: server.transport,
    command: server.command || "",
    argsText: (server.args || []).join("\n"),
    envText: keyValueLines(server.env),
    url: server.url || "",
    headersText: keyValueLines(server.headers),
    scope: server.scope,
    projectId: server.projectId || "",
    environments: server.environments.length > 0 ? server.environments : ENVIRONMENT_OPTIONS.map((item) => item.id),
    agents: server.agents.length > 0 ? server.agents : ["claude-code"],
    enabled: server.enabled,
    autoApproveText: (server.autoApproveTools || []).join("\n"),
    envSecretKeys: server.envSecretKeys || [],
    headerSecretKeys: server.headerSecretKeys || [],
    envSecrets: {},
    headerSecrets: {},
  };
}

export default function McpManagerPage() {
  const { api, projects } = useProjectContext();
  const navigate = useNavigate();
  const [servers, setServers] = useState<MCPServer[]>([]);
  const [presets, setPresets] = useState<MCPServer[]>([]);
  const [loading, setLoading] = useState(true);
  const [localError, setLocalError] = useState("");
  const [showForm, setShowForm] = useState(false);
  const [editingId, setEditingId] = useState<string | null>(null);
  const [form, setForm] = useState<FormState>(emptyForm);
  const [saving, setSaving] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<MCPServer | null>(null);
  const [deleting, setDeleting] = useState(false);
  const [autoApproveConfirm, setAutoApproveConfirm] = useState(false);
  const [projectView, setProjectView] = useState<MCPProjectView | null>(null);
  const [viewProjectId, setViewProjectId] = useState("");
  const [viewLoading, setViewLoading] = useState(false);
  // 连接测试：目标环境 + 工具列表（含可疑标注）。
  const [testTarget, setTestTarget] = useState<MCPServer | null>(null);
  const [testEnvironment, setTestEnvironment] = useState("windows");
  const [testResult, setTestResult] = useState<MCPTestResult | null>(null);
  const [testRunning, setTestRunning] = useState(false);
  // 从现有配置导入：预览 → 选择 → 确认写入。
  const [importOpen, setImportOpen] = useState(false);
  const [importProjectId, setImportProjectId] = useState("");
  const [importPreview, setImportPreview] = useState<MCPImportResult | null>(null);
  const [importLoading, setImportLoading] = useState(false);
  const [importSelection, setImportSelection] = useState<Record<string, boolean>>({});
  const [importRunning, setImportRunning] = useState(false);
  const [bindingBusy, setBindingBusy] = useState("");
  // P2：远程授权（OAuth 2.1 + PKCE，仅 http / sse）。
  const [oauthTarget, setOauthTarget] = useState<MCPServer | null>(null);
  const [oauthStatus, setOauthStatus] = useState<MCPOAuthStatus | null>(null);
  const [oauthStatusLoading, setOauthStatusLoading] = useState(false);
  const [oauthStarting, setOauthStarting] = useState(false);
  const [oauthRevoking, setOauthRevoking] = useState(false);
  const [oauthFlowId, setOauthFlowId] = useState("");
  // P2：调用审计。
  const [auditOpen, setAuditOpen] = useState(false);
  const [auditEntries, setAuditEntries] = useState<MCPAuditEntry[]>([]);
  const [auditTotal, setAuditTotal] = useState(0);
  const [auditLoading, setAuditLoading] = useState(false);
  const [auditFilter, setAuditFilter] = useState("");
  const [auditClearing, setAuditClearing] = useState(false);
  // P2：最近一次注入快照。
  const [injection, setInjection] = useState<MCPInjectionStatus | null>(null);
  // P2：工具级免审批的逐条切换与二次确认。
  const [toolToggleBusy, setToolToggleBusy] = useState("");
  const [toolConfirm, setToolConfirm] = useState<{ pattern: string; toolName: string; next: boolean } | null>(null);
  // P2：项目 .mcp.json 放行的二次确认。
  const [mcpJsonConfirm, setMcpJsonConfirm] = useState<{ next: boolean } | null>(null);
  const [mcpJsonBusy, setMcpJsonBusy] = useState(false);
  // 按环境预览：占位符在目标环境解析成什么。
  const [previewEnvironment, setPreviewEnvironment] = useState("windows");
  const [previewResult, setPreviewResult] = useState<MCPPreviewResult | null>(null);
  const [previewLoading, setPreviewLoading] = useState(false);

  const loadServers = useCallback(async () => {
    setLoading(true);
    try {
      const list = await api<MCPServer[]>("/api/mcp/servers");
      setServers(Array.isArray(list) ? list : []);
      setLocalError("");
    } catch (cause) {
      setLocalError(cause instanceof Error ? cause.message : "无法加载 MCP 配置");
    } finally {
      setLoading(false);
    }
  }, [api]);

  useEffect(() => { void loadServers(); }, [loadServers]);

  useEffect(() => {
    void (async () => {
      try {
        const list = await api<MCPServer[]>("/api/mcp/presets");
        setPresets(Array.isArray(list) ? list : []);
      } catch {
        setPresets([]);
      }
    })();
  }, [api]);

  const loadProjectView = useCallback(async (projectId: string) => {
    setViewProjectId(projectId);
    if (!projectId) { setProjectView(null); setInjection(null); return; }
    setViewLoading(true);
    try {
      const view = await api<MCPProjectView>(`/api/projects/${projectId}/mcp`);
      setProjectView(view);
    } catch (cause) {
      setProjectView(null);
      toast.error(cause instanceof Error ? cause.message : "无法加载项目 MCP 视图");
    } finally {
      setViewLoading(false);
    }
    // 注入快照是内存态、可能还没有（项目尚未跑过任务），失败静默即可。
    try {
      const status = await api<MCPInjectionStatus>(`/api/projects/${projectId}/mcp/status`);
      setInjection(status && status.updatedAt ? status : null);
    } catch {
      setInjection(null);
    }
  }, [api]);

  const startCreate = (preset?: MCPServer) => {
    setEditingId(null);
    if (preset) {
      const next = emptyForm();
      next.name = preset.name;
      next.displayName = preset.displayName;
      next.description = preset.description;
      next.transport = preset.transport;
      next.command = preset.command || "";
      next.argsText = (preset.args || []).join("\n");
      next.url = preset.url || "";
      setForm(next);
    } else {
      setForm(emptyForm());
    }
    setLocalError("");
    setPreviewResult(null);
    setShowForm(true);
  };

  const startEdit = (server: MCPServer) => {
    setEditingId(server.id);
    setForm(formFromServer(server));
    setLocalError("");
    setPreviewResult(null);
    setShowForm(true);
  };

  const closeForm = () => {
    if (saving) return;
    setShowForm(false);
    setEditingId(null);
    setLocalError("");
    setPreviewResult(null);
  };

  // 自动放行是高权限设置：命中即不再弹审批、直接执行 MCP 工具。保存前先二次确认，
  // 避免误配置让工具静默执行。无自动放行时直接保存。
  const submit = () => {
    if (parseLines(form.autoApproveText).length > 0) {
      setAutoApproveConfirm(true);
      return;
    }
    void performSave();
  };

  // 按环境预览：占位符（${PROJECT_DIR} 等）在不同环境解析成完全不同的路径，保存前先看清
  // 目标环境里到底长什么样，可以挡掉「本地能跑、远端跑不起来」的配置错误。
  const runPreview = async () => {
    setPreviewLoading(true);
    try {
      const result = await api<MCPPreviewResult>("/api/mcp/preview", {
        method: "POST",
        body: JSON.stringify({
          transport: form.transport,
          command: form.transport === "stdio" ? form.command.trim() : "",
          args: form.transport === "stdio" ? parseLines(form.argsText) : [],
          env: form.transport === "stdio" ? parseKeyValueLines(form.envText) : {},
          url: form.transport === "stdio" ? "" : form.url.trim(),
          headers: form.transport === "stdio" ? {} : parseKeyValueLines(form.headersText),
          environment: previewEnvironment,
          projectId: form.scope === "project" ? form.projectId : "",
        }),
      });
      setPreviewResult(result);
    } catch (cause) {
      setPreviewResult(null);
      toast.error(cause instanceof Error ? cause.message : "预览失败");
    } finally {
      setPreviewLoading(false);
    }
  };

  const performSave = async () => {
    setSaving(true);
    setLocalError("");
    try {
      const env = parseKeyValueLines(form.envText);
      const headers = parseKeyValueLines(form.headersText);
      const envSecrets: Record<string, string> = {};
      for (const key of form.envSecretKeys) {
        const value = form.envSecrets[key];
        if (value) envSecrets[key] = value;
      }
      const headerSecrets: Record<string, string> = {};
      for (const key of form.headerSecretKeys) {
        const value = form.headerSecrets[key];
        if (value) headerSecrets[key] = value;
      }
      const payload = {
        name: form.name.trim(),
        displayName: form.displayName.trim(),
        description: form.description.trim(),
        transport: form.transport,
        command: form.transport === "stdio" ? form.command.trim() : "",
        args: form.transport === "stdio" ? parseLines(form.argsText) : [],
        env,
        envSecrets,
        url: form.transport === "stdio" ? "" : form.url.trim(),
        headers,
        headerSecrets,
        scope: form.scope,
        projectId: form.scope === "project" ? form.projectId : "",
        environments: form.environments,
        agents: form.agents,
        enabled: form.enabled,
        autoApproveTools: parseLines(form.autoApproveText),
      };
      if (editingId) {
        await api(`/api/mcp/servers/${editingId}`, { method: "PATCH", body: JSON.stringify(payload) });
        toast.success("MCP 配置已更新");
      } else {
        await api("/api/mcp/servers", { method: "POST", body: JSON.stringify(payload) });
        toast.success("MCP 配置已创建");
      }
      setShowForm(false);
      setEditingId(null);
      await loadServers();
    } catch (cause) {
      setLocalError(cause instanceof Error ? cause.message : "保存失败");
    } finally {
      setSaving(false);
    }
  };

  const confirmDelete = async () => {
    if (!deleteTarget) return;
    setDeleting(true);
    try {
      await api(`/api/mcp/servers/${deleteTarget.id}`, { method: "DELETE" });
      toast.success("MCP 配置已删除");
      setDeleteTarget(null);
      await loadServers();
    } catch (cause) {
      toast.error(cause instanceof Error ? cause.message : "删除失败");
    } finally {
      setDeleting(false);
    }
  };

  const toggleEnabled = async (server: MCPServer) => {
    try {
      await api(`/api/mcp/servers/${server.id}`, { method: "PATCH", body: JSON.stringify({ enabled: !server.enabled }) });
      await loadServers();
    } catch (cause) {
      toast.error(cause instanceof Error ? cause.message : "更新失败");
    }
  };

  // 连接测试：在目标环境真实拉起 server 并列出工具。授权前先看清 server 声称能做什么，
  // 是抵御工具投毒最有效的一环。
  const openTest = (server: MCPServer) => {
    setTestTarget(server);
    setTestEnvironment(server.environments[0] || "windows");
    setTestResult(null);
  };

  const runTest = async () => {
    if (!testTarget) return;
    setTestRunning(true);
    setTestResult(null);
    try {
      const result = await api<MCPTestResult>(`/api/mcp/servers/${testTarget.id}/test`, {
        method: "POST",
        body: JSON.stringify({ environment: testEnvironment }),
      });
      setTestResult(result);
    } catch (cause) {
      toast.error(cause instanceof Error ? cause.message : "连接测试失败");
    } finally {
      setTestRunning(false);
    }
  };

  const loadImportPreview = useCallback(async (projectId: string) => {
    setImportProjectId(projectId);
    setImportLoading(true);
    try {
      const result = await api<MCPImportResult>("/api/mcp/import", {
        method: "POST",
        body: JSON.stringify({ projectId, confirm: false }),
      });
      setImportPreview(result);
      const next: Record<string, boolean> = {};
      for (const candidate of result.candidates) {
        next[`${candidate.source}|${candidate.originalName}`] = !candidate.conflict && !candidate.skipReason;
      }
      setImportSelection(next);
    } catch (cause) {
      setImportPreview(null);
      toast.error(cause instanceof Error ? cause.message : "无法读取现有配置");
    } finally {
      setImportLoading(false);
    }
  }, [api]);

  const confirmImport = async () => {
    setImportRunning(true);
    try {
      const selected = Object.entries(importSelection).filter(([, value]) => value).map(([key]) => key);
      const result = await api<MCPImportResult>("/api/mcp/import", {
        method: "POST",
        body: JSON.stringify({ projectId: importProjectId, confirm: true, selected }),
      });
      setImportPreview(result);
      await loadServers();
      if (result.imported > 0) {
        toast.success(`已导入 ${result.imported} 个 MCP server`);
      } else {
        toast.message("没有新的 MCP server 被导入");
      }
      for (const error of result.errors || []) {
        toast.error(error);
      }
    } catch (cause) {
      toast.error(cause instanceof Error ? cause.message : "导入失败");
    } finally {
      setImportRunning(false);
    }
  };

  const toggleBinding = async (serverId: string, enabled: boolean) => {
    if (!viewProjectId) return;
    setBindingBusy(serverId);
    try {
      const view = await api<MCPProjectView>(`/api/projects/${viewProjectId}/mcp`, {
        method: "PATCH",
        body: JSON.stringify({ bindings: [{ serverId, enabled }] }),
      });
      setProjectView(view);
    } catch (cause) {
      toast.error(cause instanceof Error ? cause.message : "更新项目开关失败");
    } finally {
      setBindingBusy("");
    }
  };

  // —— P2：远程授权（OAuth 2.1 + PKCE） ——

  const openOAuth = async (server: MCPServer) => {
    setOauthTarget(server);
    setOauthStatus(null);
    setOauthFlowId("");
    setOauthStatusLoading(true);
    try {
      const status = await api<MCPOAuthStatus>(`/api/mcp/servers/${server.id}/oauth`);
      setOauthStatus(status);
    } catch (cause) {
      toast.error(cause instanceof Error ? cause.message : "无法读取授权状态");
    } finally {
      setOauthStatusLoading(false);
    }
  };

  // 授权需要用户在浏览器里登录服务商。发起后轮询流程状态，完成后自动刷新授权状态。
  const startOAuth = async () => {
    if (!oauthTarget) return;
    setOauthStarting(true);
    try {
      const started = await api<{ flowId: string; authorizationUrl: string; redirectUri: string }>(
        `/api/mcp/servers/${oauthTarget.id}/oauth/start`,
        { method: "POST", body: JSON.stringify({}) },
      );
      setOauthFlowId(started.flowId);
      await openExternal(started.authorizationUrl);
      toast.message("已打开授权页面，请在浏览器中完成登录");
      void pollOAuthFlow(oauthTarget.id, started.flowId);
    } catch (cause) {
      toast.error(cause instanceof Error ? cause.message : "发起授权失败");
    } finally {
      setOauthStarting(false);
    }
  };

  const pollOAuthFlow = async (serverId: string, flowId: string) => {
    // 最多轮询 ~5 分钟（60 次 × 5s），与后端 flow TTL 对齐。
    for (let attempt = 0; attempt < 60; attempt += 1) {
      await new Promise((resolve) => setTimeout(resolve, 5000));
      let status: { status?: string; error?: string } | null = null;
      try {
        status = await api<{ status?: string; error?: string }>(`/api/mcp/oauth/flows/${flowId}`);
      } catch {
        // flow 过期或服务重启：停止轮询，让用户重新发起。
        return;
      }
      if (status?.status === "done") {
        toast.success("授权完成");
        try {
          const next = await api<MCPOAuthStatus>(`/api/mcp/servers/${serverId}/oauth`);
          setOauthStatus(next);
        } catch { /* 忽略：状态未刷新也不影响已完成的授权 */ }
        return;
      }
      if (status?.status === "error") {
        toast.error(status.error || "授权失败");
        return;
      }
    }
  };

  const revokeOAuth = async () => {
    if (!oauthTarget) return;
    setOauthRevoking(true);
    try {
      await api(`/api/mcp/servers/${oauthTarget.id}/oauth`, { method: "DELETE" });
      toast.success("已清除授权");
      setOauthStatus({ serverId: oauthTarget.id, authorized: false, expired: false, hasRefreshToken: false, hasClientSecret: false });
      setOauthFlowId("");
    } catch (cause) {
      toast.error(cause instanceof Error ? cause.message : "清除授权失败");
    } finally {
      setOauthRevoking(false);
    }
  };

  // —— P2：工具级免审批白名单 ——

  const toolPattern = (server: MCPServer, toolName: string) => `mcp__${server.name}__${toolName}`;

  const isToolAutoApproved = (server: MCPServer, toolName: string) => {
    const patterns = server.autoApproveTools || [];
    const exact = toolPattern(server, toolName);
    return patterns.includes(exact) || patterns.includes(`mcp__${server.name}__*`);
  };

  const applyToolAutoApprove = async (server: MCPServer, toolName: string, next: boolean) => {
    const exact = toolPattern(server, toolName);
    const serverWildcard = `mcp__${server.name}__*`;
    const current = server.autoApproveTools || [];
    let patterns: string[];
    if (next) {
      patterns = [...current.filter((item) => item !== serverWildcard), exact];
    } else {
      patterns = current.filter((item) => item !== exact);
    }
    patterns = Array.from(new Set(patterns)).sort();
    setToolToggleBusy(toolName);
    try {
      const updated = await api<MCPServer>(`/api/mcp/servers/${server.id}/auto-approve`, {
        method: "PUT",
        body: JSON.stringify({ patterns }),
      });
      setServers((old) => old.map((item) => (item.id === updated.id ? updated : item)));
      if (testTarget && testTarget.id === updated.id) setTestTarget(updated);
    } catch (cause) {
      toast.error(cause instanceof Error ? cause.message : "更新免审批白名单失败");
    } finally {
      setToolToggleBusy("");
    }
  };

  const confirmToolAutoApprove = async () => {
    if (!toolConfirm || !testTarget) return;
    const pending = toolConfirm;
    setToolConfirm(null);
    await applyToolAutoApprove(testTarget, pending.toolName, pending.next);
  };

  // —— P2：调用审计 ——

  const loadAudit = useCallback(async (serverName: string) => {
    setAuditLoading(true);
    try {
      const query = new URLSearchParams({ limit: "80" });
      if (serverName) query.set("serverName", serverName);
      const result = await api<MCPAuditResponse>(`/api/mcp/audit?${query.toString()}`);
      setAuditEntries(Array.isArray(result.entries) ? result.entries : []);
      setAuditTotal(result.total || 0);
    } catch (cause) {
      toast.error(cause instanceof Error ? cause.message : "无法读取调用审计");
    } finally {
      setAuditLoading(false);
    }
  }, [api]);

  const clearAudit = async () => {
    setAuditClearing(true);
    try {
      await api("/api/mcp/audit", { method: "DELETE" });
      setAuditEntries([]);
      setAuditTotal(0);
      toast.success("审计记录已清空");
    } catch (cause) {
      toast.error(cause instanceof Error ? cause.message : "清空审计失败");
    } finally {
      setAuditClearing(false);
    }
  };

  // —— P2：项目放行 .mcp.json ——

  const toggleAllowMcpJson = async (next: boolean) => {
    if (!viewProjectId) return;
    setMcpJsonBusy(true);
    try {
      const view = await api<MCPProjectView>(`/api/projects/${viewProjectId}/mcp`, {
        method: "PATCH",
        body: JSON.stringify({ allowMcpJson: next }),
      });
      setProjectView(view);
      toast.success(next ? "已放行项目 .mcp.json" : "已恢复严格模式");
    } catch (cause) {
      toast.error(cause instanceof Error ? cause.message : "更新项目设置失败");
    } finally {
      setMcpJsonBusy(false);
    }
  };

  const projectName = useMemo(() => {
    const map = new Map<string, string>();
    for (const project of projects) map.set(project.id, project.name);
    return map;
  }, [projects]);

  const setField = <K extends keyof FormState>(key: K, value: FormState[K]) => setForm((old) => ({ ...old, [key]: value }));

  const toggleIn = (key: "environments" | "agents", id: string) => {
    setForm((old) => {
      const list = old[key];
      return { ...old, [key]: list.includes(id) ? list.filter((item) => item !== id) : [...list, id] };
    });
  };

  const canSubmit = !saving && form.name.trim() !== "" &&
    (form.transport === "stdio" ? form.command.trim() !== "" : form.url.trim() !== "") &&
    form.environments.length > 0 && form.agents.length > 0 &&
    (form.scope !== "project" || form.projectId !== "");

  return <>
    <DashboardPage />
    <div className="backdrop ssh-manager-backdrop" role="dialog" aria-modal="true" aria-labelledby="mcp-manager-title">
      <section className="modal ssh-manager-dialog">
        <header><div className="ssh-dialog-heading"><span className="ssh-dialog-mark"><McpIcon /></span><div><h2 id="mcp-manager-title">MCP连接</h2><p>管理注入到 AI 会话的外部 MCP server</p></div></div><button className="ssh-dialog-close" type="button" title="关闭" aria-label="关闭" onClick={() => navigate("/")}><CloseIcon /></button></header>
        <div className="ssh-manager-toolbar"><span>{loading ? "正在同步 MCP 配置" : `${servers.length} 个 MCP server`}</span><div className="ssh-toolbar-actions"><button className="secondary" type="button" onClick={() => { setAuditOpen(true); setAuditFilter(""); void loadAudit(""); }}>调用审计</button><button className="secondary" type="button" onClick={() => { setImportOpen(true); setImportPreview(null); void loadImportPreview(""); }}>从现有配置导入</button><button className="primary ssh-add-button" type="button" onClick={() => startCreate()}><McpIcon />添加 MCP</button></div></div>
        {localError && <div className="ssh-error" role="alert"><span>{localError}</span><button type="button" title="关闭提示" aria-label="关闭提示" onClick={() => setLocalError("")}><CloseIcon /></button></div>}
        <div className="ssh-manager-body">
          {loading ? <div className="ssh-empty-state"><span className="ssh-loading-indicator"></span><p>正在读取 MCP 配置…</p></div>
            : servers.length === 0 ? <div className="ssh-empty-state"><span className="ssh-empty-icon"><McpIcon /></span><h3>还没有 MCP server</h3><p>添加后，AI 在 Windows / WSL / SSH 项目中执行任务时即可调用。</p><button className="primary" type="button" onClick={() => startCreate()}><McpIcon />添加 MCP server</button></div>
            : <div className="ssh-connection-list">{servers.map((server) => <article className={`ssh-connection-card is-${server.enabled ? "connected" : "unknown"}`} key={server.id}>
              <div className="ssh-connection-main">
                <span className="ssh-status"><i></i>{server.enabled ? "已启用" : "已停用"}</span>
                <h3>{server.displayName || server.name}</h3>
                <p>{server.transport} · {server.scope === "project" ? `项目：${projectName.get(server.projectId || "") || server.projectId}` : "全局"}</p>
                <small>环境：{server.environments.join(" / ")} · Agent：{server.agents.join(" / ")}{(server.envSecretKeys?.length || server.headerSecretKeys?.length) ? " · 含凭据" : ""}</small>
              </div>
              <div className="ssh-connection-actions">
                <button className="ssh-action-button mcp-test-button" type="button" title="测试连接" aria-label={`测试 ${server.name}`} onClick={() => openTest(server)}>测试</button>
                {server.transport !== "stdio" && <button className="ssh-action-button" type="button" title="OAuth 授权" aria-label={`授权 ${server.name}`} onClick={() => void openOAuth(server)}>授权</button>}
                <button className="ssh-action-button" type="button" title={server.enabled ? "停用" : "启用"} aria-label={`切换 ${server.name}`} onClick={() => void toggleEnabled(server)}>{server.enabled ? "停用" : "启用"}</button>
                <button className="ssh-action-button" type="button" title="编辑" aria-label={`编辑 ${server.name}`} onClick={() => startEdit(server)}><EditIcon /></button>
                <button className="ssh-action-button danger" type="button" title="删除" aria-label={`删除 ${server.name}`} onClick={() => setDeleteTarget(server)}><TrashIcon /></button>
              </div>
            </article>)}</div>}
          {presets.length > 0 && <div className="ssh-form-section"><header><h3>从模板添加</h3><p>一键带入常见 MCP server 的启动参数，保存前可继续修改。</p></header><div className="ssh-preflight-checks">{presets.map((preset) => <button key={preset.id} className="secondary" type="button" onClick={() => startCreate(preset)}>{preset.displayName || preset.name}</button>)}</div></div>}
          <div className="ssh-form-section"><header><h3>项目视图</h3><p>查看某个项目当前实际生效的 MCP server（只读）。</p></header>
            <label className="ssh-field">选择项目<select value={viewProjectId} onChange={(event) => void loadProjectView(event.target.value)}><option value="">未选择</option>{projects.map((project) => <option key={project.id} value={project.id}>{project.name}</option>)}</select></label>
            {viewLoading && <p className="ssh-form-notice"><span className="ssh-loading-indicator"></span>正在读取项目 MCP 视图…</p>}
            {projectView && !viewLoading && <div className="ssh-preflight valid"><header><span><McpIcon /></span><div><h4>{`环境：${projectView.environment}`}</h4><p>{projectView.strictMode ? "已启用严格模式（默认）" : "未启用严格模式"}</p></div></header>
              <div className="ssh-preflight-checks">{projectView.effective.length === 0 ? <span>该项目暂无生效的 MCP server</span> : projectView.effective.map((entry) => <span key={entry.id}>{entry.displayName || entry.name} <b>{entry.origin === "project" ? "项目" : "全局"}</b></span>)}</div>
              {projectView.bindings.length > 0 && <div className="ssh-form-section"><header><h3>项目开关</h3><p>关闭后该项目不再注入该 server，不影响其他项目。</p></header>
                <div className="ssh-preflight-checks">{projectView.bindings.map((binding) => <button key={binding.serverId} className={binding.enabled ? "" : "secondary"} type="button" disabled={bindingBusy === binding.serverId} onClick={() => void toggleBinding(binding.serverId, !binding.enabled)}>
                  {binding.displayName || binding.serverName}：{binding.enabled ? "已启用" : "已关闭"}{binding.overridden ? "（项目覆盖）" : ""}
                </button>)}</div>
              </div>}
              <div className="ssh-form-section"><header><h3>项目自带配置</h3><p>放行后，项目根目录的 <code>.mcp.json</code> 会随 Milevia 注入一并生效；此时不再强制严格模式，该文件里的 server 绕过 Milevia 的白名单与审计。</p></header>
                <div className="ssh-auth-method"><label className={projectView.allowMcpJson ? "active" : ""}><input type="checkbox" checked={projectView.allowMcpJson} disabled={mcpJsonBusy} onChange={() => setMcpJsonConfirm({ next: !projectView.allowMcpJson })} />放行项目 .mcp.json</label></div>
                {projectView.allowMcpJson && projectView.mcpJsonServers.length > 0 && <p className="ssh-form-notice">该文件中发现：{projectView.mcpJsonServers.join("、")}</p>}
                {projectView.allowMcpJson && projectView.mcpJsonServers.length === 0 && <p className="ssh-form-notice">未在项目根目录发现 .mcp.json，或文件中没有 mcpServers。</p>}
              </div>
              {injection && <div className="ssh-form-section"><header><h3>最近一次注入</h3><p>{`${injection.environment || "—"} · ${injection.agentId || "—"} · ${new Date(injection.updatedAt).toLocaleString("zh-CN")}`}</p></header>
                <div className="ssh-preflight-checks">{injection.serverCount === 0 ? <span>本次未注入任何 MCP server</span> : injection.servers.map((entry) => <span key={entry.name}>{entry.displayName || entry.name} <b>{entry.origin === "project" ? "项目" : "全局"}</b></span>)}</div>
                {injection.note && <p className="ssh-form-notice">{injection.note}</p>}
              </div>}
              {projectView.warnings.map((warning) => <p key={warning}>{warning}</p>)}
            </div>}
          </div>
        </div>
        <footer><button className="secondary" type="button" onClick={() => navigate("/")}>关闭</button></footer>
      </section>
    </div>
    {showForm && <div className="backdrop ssh-form-backdrop" role="dialog" aria-modal="true" aria-labelledby="mcp-form-title">
      <section className="modal ssh-connection-dialog">
        <header><div className="ssh-dialog-heading"><span className="ssh-dialog-mark"><McpIcon /></span><div><h2 id="mcp-form-title">{editingId ? "编辑 MCP server" : "添加 MCP server"}</h2><p>配置将按项目环境在运行时注入到 AI 会话</p></div></div><button className="ssh-dialog-close" type="button" title="关闭" aria-label="关闭" disabled={saving} onClick={closeForm}><CloseIcon /></button></header>
        <div className="ssh-form-body">
          <section className="ssh-form-section"><header><h3>基本信息</h3><p>名称会作为注入后的 server key，仅支持字母、数字、下划线与连字符。</p></header>
            <div className="ssh-fields">
              <label className="ssh-field">名称<input autoFocus type="text" value={form.name} onChange={(event) => setField("name", event.target.value)} placeholder="github" /></label>
              <label className="ssh-field">显示名称<input type="text" value={form.displayName} onChange={(event) => setField("displayName", event.target.value)} placeholder="GitHub" /></label>
              <label className="ssh-field ssh-field-wide">说明<input type="text" value={form.description} onChange={(event) => setField("description", event.target.value)} placeholder="读写 GitHub 仓库" /></label>
            </div>
          </section>
          <section className="ssh-form-section"><header><h3>传输方式</h3><p>stdio 在目标环境拉起本地进程；http/sse 连接远程服务。</p></header>
            <label className="ssh-field">类型<select value={form.transport} onChange={(event) => setField("transport", event.target.value as MCPTransport)}><option value="stdio">stdio（本地进程）</option><option value="http">http（Streamable HTTP）</option><option value="sse">sse（兼容旧版）</option></select></label>
            {form.transport === "stdio" ? <div className="ssh-fields">
              <label className="ssh-field">启动命令<input type="text" value={form.command} onChange={(event) => setField("command", event.target.value)} placeholder="npx" /></label>
              <label className="ssh-field ssh-field-wide">参数（每行一个）<textarea rows={3} value={form.argsText} onChange={(event) => setField("argsText", event.target.value)} placeholder={"-y\n@modelcontextprotocol/server-github"} /></label>
              <label className="ssh-field ssh-field-wide">环境变量（每行 KEY=VALUE，支持 {"${PROJECT_DIR}"}）<textarea rows={3} value={form.envText} onChange={(event) => setField("envText", event.target.value)} placeholder={"LOG_LEVEL=info"} /></label>
            </div> : <div className="ssh-fields">
              <label className="ssh-field ssh-field-wide">地址<input type="text" value={form.url} onChange={(event) => setField("url", event.target.value)} placeholder="https://example.com/mcp" /></label>
              <label className="ssh-field ssh-field-wide">请求头（每行 KEY=VALUE）<textarea rows={3} value={form.headersText} onChange={(event) => setField("headersText", event.target.value)} /></label>
            </div>}
          </section>
          <section className="ssh-form-section"><header><h3>凭据</h3><p>凭据在保存时加密存储，保存后不可回显。留空表示不修改。</p></header>
            {(form.transport === "stdio" ? form.envSecretKeys : form.headerSecretKeys).length === 0
              ? <p className="ssh-form-notice">暂无已保存的凭据。可在上面的环境变量 / 请求头中以明文写入，保存时自动加密处理。</p>
              : (form.transport === "stdio" ? form.envSecretKeys : form.headerSecretKeys).map((key) => <label className="ssh-field" key={key}>{key}（已设置）<input type="password" autoComplete="new-password" value={(form.transport === "stdio" ? form.envSecrets : form.headerSecrets)[key] || ""} onChange={(event) => {
                const target = form.transport === "stdio" ? "envSecrets" : "headerSecrets";
                setForm((old) => ({ ...old, [target]: { ...old[target], [key]: event.target.value } }));
              }} placeholder="留空保持原值" /></label>)}
          </section>
          <section className="ssh-form-section"><header><h3>适用范围</h3><p>只有同时命中作用域、环境与 Agent 的会话才会注入该 server。</p></header>
            <div className="ssh-fields">
              <label className="ssh-field">作用域<select value={form.scope} onChange={(event) => setField("scope", event.target.value as MCPScope)}><option value="global">全局（所有项目）</option><option value="project">仅指定项目</option></select></label>
              {form.scope === "project" && <label className="ssh-field">项目<select value={form.projectId} onChange={(event) => setField("projectId", event.target.value)}><option value="">请选择项目</option>{projects.map((project) => <option key={project.id} value={project.id}>{project.name}</option>)}</select></label>}
            </div>
            <div className="ssh-auth-method">{ENVIRONMENT_OPTIONS.map((option) => <label key={option.id} className={form.environments.includes(option.id) ? "active" : ""}><input type="checkbox" checked={form.environments.includes(option.id)} onChange={() => toggleIn("environments", option.id)} />{option.label}</label>)}</div>
            <div className="ssh-auth-method">{AGENT_OPTIONS.map((option) => <label key={option.id} className={form.agents.includes(option.id) ? "active" : ""}><input type="checkbox" checked={form.agents.includes(option.id)} onChange={() => toggleIn("agents", option.id)} />{option.label}</label>)}</div>
          </section>
          <section className="ssh-form-section"><header><h3>按环境预览</h3><p>占位符（如 {"${PROJECT_DIR}"}）在不同环境会解析成不同路径。保存前先确认目标环境下的实际结果。</p></header>
            <div className="ssh-fields">
              <label className="ssh-field">预览环境<select value={previewEnvironment} onChange={(event) => { setPreviewEnvironment(event.target.value); setPreviewResult(null); }}>{ENVIRONMENT_OPTIONS.map((option) => <option key={option.id} value={option.id}>{option.label}</option>)}</select></label>
            </div>
            <div className="ssh-preflight-checks"><button className="secondary" type="button" disabled={previewLoading} onClick={() => void runPreview()}>{previewLoading ? "解析中…" : "生成预览"}</button></div>
            {previewResult && <div className="mcp-preview">
              <dl>{mcpPreviewRows(previewResult).map(([label, value]) => <div key={label}><dt>{label}</dt><dd>{value}</dd></div>)}</dl>
              {previewResult.notes.map((note) => <p key={note} className="ssh-form-notice">{note}</p>)}
            </div>}
          </section>
          <section className="ssh-form-section"><header><h3>策略</h3><p>默认所有 MCP 工具调用都需要确认；填入自动放行模式后，命中的工具不再弹审批（保存时会再次确认）。</p></header>
            <label className="ssh-field ssh-field-wide">自动放行模式（每行一个，如 mcp__github__*；Bash(git status*) 按命令前缀放行）<textarea rows={2} value={form.autoApproveText} onChange={(event) => setField("autoApproveText", event.target.value)} /></label>
            <div className="ssh-auth-method"><label className={form.enabled ? "active" : ""}><input type="checkbox" checked={form.enabled} onChange={() => setField("enabled", !form.enabled)} />启用此 MCP server</label></div>
          </section>
        </div>
        <footer><button className="secondary" type="button" disabled={saving} onClick={closeForm}>取消</button><button className="primary" type="button" disabled={!canSubmit} onClick={() => void submit()}>{saving ? "保存中" : "保存"}</button></footer>
      </section>
    </div>}
    {deleteTarget && <ConfirmDialog title="删除 MCP server" message={<>确定要删除 <b>{deleteTarget.displayName || deleteTarget.name}</b> 吗？相关凭据会一并吊销。</>} confirmLabel="删除" danger busy={deleting} onConfirm={confirmDelete} onCancel={() => { if (!deleting) setDeleteTarget(null); }} />}
    {autoApproveConfirm && <ConfirmDialog title="确认自动放行范围" message={<>
      你为 <b>{form.displayName.trim() || form.name.trim()}</b> 配置了自动放行模式：
      <br /><code>{parseLines(form.autoApproveText).join("、")}</code><br />
      命中这些模式的 MCP 工具调用将<b>不再弹出审批、直接执行</b>。请确认相关 server 与工具可信。
    </>} confirmLabel="确认并保存" busy={saving} onConfirm={async () => { setAutoApproveConfirm(false); await performSave(); }} onCancel={() => { if (!saving) setAutoApproveConfirm(false); }} />}
    {testTarget && <div className="backdrop ssh-form-backdrop" role="dialog" aria-modal="true" aria-labelledby="mcp-test-title">
      <section className="modal ssh-connection-dialog">
        <header><div className="ssh-dialog-heading"><span className="ssh-dialog-mark"><McpIcon /></span><div><h2 id="mcp-test-title">测试连接</h2><p>{testTarget.displayName || testTarget.name} · {testTarget.transport}</p></div></div><button className="ssh-dialog-close" type="button" title="关闭" aria-label="关闭" disabled={testRunning} onClick={() => { setTestTarget(null); setTestResult(null); }}><CloseIcon /></button></header>
        <div className="ssh-form-body">
          <section className="ssh-form-section"><header><h3>目标环境</h3><p>测试会在该环境真实拉起 server 并执行 initialize + tools/list。stdio 型 server 依赖该环境已安装对应运行时。</p></header>
            <label className="ssh-field">环境<select value={testEnvironment} onChange={(event) => setTestEnvironment(event.target.value)}>{ENVIRONMENT_OPTIONS.map((option) => <option key={option.id} value={option.id}>{option.label}</option>)}</select></label>
            {!testTarget.environments.includes(testEnvironment) && <p className="ssh-form-notice">该 server 未声明适用于此环境，实跑会按当前选择执行。</p>}
            <div className="ssh-preflight-checks"><button className="primary" type="button" disabled={testRunning} onClick={() => void runTest()}>{testRunning ? "测试中…" : "开始测试"}</button></div>
          </section>
          {testRunning && <p className="ssh-form-notice"><span className="ssh-loading-indicator"></span>正在目标环境启动 MCP server…</p>}
          {testResult && !testResult.ok && <section className="ssh-form-section"><header><h3>测试失败</h3></header>
            <p className="ssh-form-notice">{testResult.error}</p>
            {testResult.hint && <p>{testResult.hint}</p>}
          </section>}
          {testResult && testResult.ok && <section className="ssh-form-section">
            <header><h3>工具列表（{testResult.toolCount}）</h3><p>{`耗时 ${testResult.durationMs} ms${testResult.protocolVersion ? ` · 协议 ${testResult.protocolVersion}` : ""}${testResult.flaggedCount ? ` · ${testResult.flaggedCount} 个工具命中可疑模式` : ""}`}</p></header>
            {testResult.hint && <p className="ssh-form-notice">{testResult.hint}</p>}
            {testResult.tools.length === 0 ? <p className="ssh-form-notice">该 server 未声明任何工具。</p> : <div className="mcp-tool-list">{testResult.tools.map((tool) => <article key={tool.name} className={`mcp-tool-card${tool.flags?.some((flag) => flag.severity === "danger") ? " is-danger" : tool.flags?.length ? " is-warn" : ""}`}>
              <header><b>{tool.name}</b>{tool.flags?.map((flag) => <span key={flag.code + flag.label} className={`mcp-tool-flag is-${flag.severity}`}>{flag.label}</span>)}</header>
              {tool.title && <small>{tool.title}</small>}
              {tool.description && <p>{tool.description}</p>}
              {tool.flags?.filter((flag) => flag.note).map((flag) => <small key={flag.code + "-note"} className="mcp-tool-note">{flag.note}</small>)}
              {tool.flags?.filter((flag) => flag.detail).map((flag) => <small key={flag.code + flag.detail}>命中片段：{flag.detail}</small>)}
              {tool.inputSchema ? <details className="mcp-tool-schema"><summary>参数 schema</summary><pre>{mcpSchemaText(tool.inputSchema)}</pre></details> : null}
              <label className="mcp-tool-approve"><input type="checkbox" checked={isToolAutoApproved(testTarget, tool.name)} disabled={toolToggleBusy === tool.name} onChange={() => setToolConfirm({ pattern: toolPattern(testTarget, tool.name), toolName: tool.name, next: !isToolAutoApproved(testTarget, tool.name) })} />免审批执行</label>
            </article>)}</div>}
            <p className="ssh-form-notice">工具描述由 server 自行提供。命中可疑模式不必然代表恶意，但请在授权前确认其行为符合预期。勾选「免审批执行」后，该工具不再弹出确认。</p>
          </section>}
          {testResult && testResult.ok && (testResult.resources?.length || testResult.prompts?.length) ? <section className="ssh-form-section">
            <header><h3>资源与提示词</h3><p>MCP 的可选原语。Claude Code 可使用它们；Codex 目前只消费工具。</p></header>
            {(testResult.resources?.length || 0) > 0 && <div className="mcp-primitive-block">
              <h4>资源（{testResult.resources.length}）</h4>
              <div className="mcp-tool-list">{testResult.resources.map((item) => <article key={item.uri} className="mcp-tool-card">
                <header><b>{item.title || item.name || item.uri}</b>{item.mimeType && <span className="mcp-tool-flag is-info">{item.mimeType}</span>}</header>
                {item.description && <p>{item.description}</p>}
                <small>{item.uri}</small>
              </article>)}</div>
            </div>}
            {(testResult.prompts?.length || 0) > 0 && <div className="mcp-primitive-block">
              <h4>提示词（{testResult.prompts.length}）</h4>
              <div className="mcp-tool-list">{testResult.prompts.map((item) => <article key={item.name} className="mcp-tool-card">
                <header><b>{item.title || item.name}</b></header>
                {item.description && <p>{item.description}</p>}
                {!item.title && <small>{item.name}</small>}
              </article>)}</div>
            </div>}
          </section> : null}
        </div>
        <footer><button className="secondary" type="button" disabled={testRunning} onClick={() => { setTestTarget(null); setTestResult(null); }}>关闭</button></footer>
      </section>
    </div>}
    {importOpen && <div className="backdrop ssh-form-backdrop" role="dialog" aria-modal="true" aria-labelledby="mcp-import-title">
      <section className="modal ssh-connection-dialog">
        <header><div className="ssh-dialog-heading"><span className="ssh-dialog-mark"><McpIcon /></span><div><h2 id="mcp-import-title">从现有配置导入</h2><p>读取 Claude 与 Codex 的 MCP 配置，确认后写入 Milevia</p></div></div><button className="ssh-dialog-close" type="button" title="关闭" aria-label="关闭" disabled={importRunning} onClick={() => { setImportOpen(false); setImportPreview(null); }}><CloseIcon /></button></header>
        <div className="ssh-form-body">
          <section className="ssh-form-section"><header><h3>来源</h3><p>选择项目可一并读取该项目专属的 Claude 配置与 .mcp.json。</p></header>
            <label className="ssh-field">项目（可选）<select value={importProjectId} onChange={(event) => void loadImportPreview(event.target.value)}><option value="">不指定</option>{projects.map((project) => <option key={project.id} value={project.id}>{project.name}</option>)}</select></label>
            {importLoading && <p className="ssh-form-notice"><span className="ssh-loading-indicator"></span>正在读取…</p>}
            {importPreview && <div className="ssh-preflight-checks">{importPreview.sources.map((source) => <span key={source.kind + source.path}>{source.label}：{source.present ? "已找到" : source.note || "未找到"}</span>)}</div>}
          </section>
          {importPreview && <section className="ssh-form-section"><header><h3>发现 {importPreview.candidates.length} 个 server</h3><p>勾选要导入的条目。同名冲突项默认跳过，可先在列表中删除后重导。</p></header>
            {importPreview.candidates.length === 0 ? <p className="ssh-form-notice">未在现有配置中发现 MCP server。</p> : <div className="mcp-import-list">{importPreview.candidates.map((candidate) => {
              const key = `${candidate.source}|${candidate.originalName}`;
              const disabled = candidate.conflict || !!candidate.skipReason;
              return <label key={key} className={`mcp-import-item${disabled ? " is-disabled" : ""}`}>
                <input type="checkbox" disabled={disabled} checked={!disabled && !!importSelection[key]} onChange={(event) => setImportSelection((old) => ({ ...old, [key]: event.target.checked }))} />
                <div><b>{candidate.originalName}</b><small>{candidate.sourceLabel} · {candidate.transport}{candidate.secretKeys.length ? ` · 含凭据（${candidate.secretKeys.join("、")}）` : ""}</small>
                  {candidate.command && <small>{candidate.command} {(candidate.args || []).join(" ")}</small>}
                  {candidate.url && <small>{candidate.url}</small>}
                  {candidate.skipReason && <small>{candidate.skipReason}</small>}
                  {candidate.warnings?.map((warning) => <small key={warning} className="mcp-import-warning">需手工补配：{warning}</small>)}
                </div>
              </label>;
            })}</div>}
          </section>}
          {(importPreview?.errors?.length || 0) > 0 && <section className="ssh-form-section"><header><h3>导入错误</h3></header>{importPreview?.errors?.map((error) => <p key={error} className="ssh-form-notice">{error}</p>)}</section>}
        </div>
        <footer><button className="secondary" type="button" disabled={importRunning} onClick={() => { setImportOpen(false); setImportPreview(null); }}>取消</button><button className="primary" type="button" disabled={importRunning || !importPreview || Object.values(importSelection).every((value) => !value)} onClick={() => void confirmImport()}>{importRunning ? "导入中" : "导入所选"}</button></footer>
      </section>
    </div>}
    {oauthTarget && <div className="backdrop ssh-form-backdrop" role="dialog" aria-modal="true" aria-labelledby="mcp-oauth-title">
      <section className="modal ssh-connection-dialog">
        <header><div className="ssh-dialog-heading"><span className="ssh-dialog-mark"><McpIcon /></span><div><h2 id="mcp-oauth-title">远程授权</h2><p>{oauthTarget.displayName || oauthTarget.name} · {oauthTarget.transport}</p></div></div><button className="ssh-dialog-close" type="button" title="关闭" aria-label="关闭" disabled={oauthStarting} onClick={() => { setOauthTarget(null); setOauthStatus(null); setOauthFlowId(""); }}><CloseIcon /></button></header>
        <div className="ssh-form-body">
          <section className="ssh-form-section"><header><h3>授权状态</h3><p>Milevia 使用 OAuth 2.1 + PKCE：在浏览器完成登录后，令牌加密存放在本机，注入时自动附加 Authorization 头。</p></header>
            {oauthStatusLoading && <p className="ssh-form-notice"><span className="ssh-loading-indicator"></span>正在读取授权状态…</p>}
            {oauthStatus && !oauthStatusLoading && <div className="ssh-preflight-checks">
              <span>{oauthStatus.authorized ? (oauthStatus.expired ? "已授权（令牌已过期）" : "已授权") : "未授权"}</span>
              {oauthStatus.scope && <span>{`scope：${oauthStatus.scope}`}</span>}
              {oauthStatus.expiresAt && <span>{`到期：${new Date(oauthStatus.expiresAt).toLocaleString("zh-CN")}`}</span>}
              {oauthStatus.hasRefreshToken && <span>支持自动续期</span>}
            </div>}
            {oauthFlowId && <p className="ssh-form-notice">已发起授权，请在浏览器完成登录。本窗口会在完成后自动更新。</p>}
            <div className="ssh-preflight-checks">
              <button className="primary" type="button" disabled={oauthStarting || oauthStatusLoading} onClick={() => void startOAuth()}>{oauthStarting ? "正在发起…" : oauthStatus?.authorized ? "重新授权" : "开始授权"}</button>
              {oauthStatus?.authorized && <button className="secondary" type="button" disabled={oauthRevoking} onClick={() => void revokeOAuth()}>{oauthRevoking ? "清除中…" : "清除授权"}</button>}
            </div>
            <p className="ssh-form-notice">在 server 上已手动配置请求头（如 Authorization）时，Milevia 不会覆盖它，也不会走此处的 OAuth。</p>
          </section>
        </div>
        <footer><button className="secondary" type="button" disabled={oauthStarting} onClick={() => { setOauthTarget(null); setOauthStatus(null); setOauthFlowId(""); }}>关闭</button></footer>
      </section>
    </div>}
    {auditOpen && <div className="backdrop ssh-form-backdrop" role="dialog" aria-modal="true" aria-labelledby="mcp-audit-title">
      <section className="modal ssh-connection-dialog">
        <header><div className="ssh-dialog-heading"><span className="ssh-dialog-mark"><McpIcon /></span><div><h2 id="mcp-audit-title">调用审计</h2><p>最近 2000 次 MCP 工具调用的裁决与结果</p></div></div><button className="ssh-dialog-close" type="button" title="关闭" aria-label="关闭" disabled={auditClearing} onClick={() => setAuditOpen(false)}><CloseIcon /></button></header>
        <div className="ssh-form-body">
          <section className="ssh-form-section"><header><h3>筛选</h3><p>参数中命中疑似凭据的键会被替换为 ***，不会落库明文。</p></header>
            <label className="ssh-field">server<select value={auditFilter} onChange={(event) => { setAuditFilter(event.target.value); void loadAudit(event.target.value); }}><option value="">全部</option>{servers.map((server) => <option key={server.id} value={server.name}>{server.displayName || server.name}</option>)}</select></label>
            <div className="ssh-preflight-checks"><button className="secondary" type="button" disabled={auditLoading} onClick={() => void loadAudit(auditFilter)}>{auditLoading ? "读取中…" : "刷新"}</button><button className="secondary" type="button" disabled={auditClearing || auditEntries.length === 0} onClick={() => void clearAudit()}>{auditClearing ? "清空中…" : "清空记录"}</button></div>
          </section>
          {!auditLoading && auditEntries.length === 0 && <section className="ssh-form-section"><p className="ssh-form-notice">暂无调用记录。AI 调用 MCP 工具后会在此出现。</p></section>}
          {auditEntries.length > 0 && <section className="ssh-form-section"><header><h3>{`显示 ${auditEntries.length} / ${auditTotal} 条`}</h3></header>
            <div className="mcp-audit-list">{auditEntries.map((entry) => <article key={entry.id} className="mcp-audit-item">
              <header><b>{entry.toolName}</b><span className={`mcp-tool-flag is-${decisionSeverity(entry.decision)}`}>{decisionLabel(entry.decision)}</span>{entry.status && entry.status !== "pending" && <span className={`mcp-tool-flag is-${statusSeverity(entry.status)}`}>{entry.status === "ok" ? "执行成功" : "执行出错"}</span>}</header>
              <small>{entry.serverName} · {new Date(entry.createdAt).toLocaleString("zh-CN")}{entry.durationMs > 0 ? ` · ${entry.durationMs} ms` : ""}</small>
              {entry.argsPreview && <code>{entry.argsPreview}</code>}
              {entry.error && <small className="mcp-audit-error">{entry.error}</small>}
            </article>)}</div>
          </section>}
        </div>
        <footer><button className="secondary" type="button" disabled={auditClearing} onClick={() => setAuditOpen(false)}>关闭</button></footer>
      </section>
    </div>}
    {toolConfirm && <ConfirmDialog title="确认免审批范围" message={<>
      为 <b>{testTarget?.displayName || testTarget?.name}</b> 的 <b>{toolConfirm.toolName}</b> 配置免审批：
      <br /><code>{toolConfirm.pattern}</code><br />
      {toolConfirm.next ? "之后该工具调用不再弹出审批，直接执行。" : "之后该工具调用将恢复为需要确认。"}
    </>} confirmLabel={toolConfirm.next ? "确认放行" : "确认恢复"} danger={toolConfirm.next} busy={toolToggleBusy !== ""} onConfirm={() => void confirmToolAutoApprove()} onCancel={() => setToolConfirm(null)} />}
    {mcpJsonConfirm && <ConfirmDialog title="确认放行项目 .mcp.json" message={<>
      {mcpJsonConfirm.next ? <>
        放行后，<b>{projectName.get(viewProjectId) || viewProjectId}</b> 根目录的 <code>.mcp.json</code> 会随 Milevia 注入一并生效。
        该文件中的 server <b>绕过 Milevia 的白名单与调用审计</b>，且在项目被 AI 修改后即刻生效。请确认该文件可信。
      </> : <>恢复严格模式后，项目 <code>.mcp.json</code> 将不再生效，仅注入 Milevia 中配置的 server。</>}
    </>} confirmLabel={mcpJsonConfirm.next ? "确认放行" : "确认恢复"} danger={mcpJsonConfirm.next} busy={mcpJsonBusy} onConfirm={() => { const next = mcpJsonConfirm.next; setMcpJsonConfirm(null); void toggleAllowMcpJson(next); }} onCancel={() => { if (!mcpJsonBusy) setMcpJsonConfirm(null); }} />}
  </>;
}
