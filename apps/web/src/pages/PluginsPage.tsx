// 插件管理（路由 `/plugins`）。
//
// 这一页不发明插件格式，也不替 CLI 做决定：列表、启用状态、范围、版本都来自 CLI 自己的
// JSON 输出；安装/卸载/启停/市场全部转成该 CLI 的子命令执行。
//
// 三处刻意的分离：
//  1. **读不到 ≠ 没有** —— viewState 的 error 与 ready-but-empty 分开渲染。
//  2. **状态未知 ≠ 已禁用** —— 磁盘兜底扫描来的记录 stateKnown=false，只显示"状态未知"。
//  3. **marketplace 待执行命令必须由用户确认** —— 服务端回 needsConfirmation 时，把命令和
//     sha256 原样展示，用户点「确认执行」后才带 acceptCommandSha256 重试；绝不自动 -y。

import { useCallback, useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { toast } from "sonner";
import { useProjectContext } from "../stores/useProjectStore";
import { api } from "../lib/api";
import type { Project, RunnerInfo } from "../lib/types";
import "./plugins.css";

type PluginCapabilities = {
  list: boolean;
  details: boolean;
  install: boolean;
  uninstall: boolean;
  enable: boolean;
  disable: boolean;
  update: boolean;
  marketplaces: boolean;
  // 该 CLI 真正支持的安装范围。Codex 只有 user，前端不应显示"选项目却装到全局"。
  scopes?: string[];
};

type PluginComponents = {
  skills: number;
  commands: number;
  agents: number;
  hooks: number;
  mcpServers: number;
  lspServers: number;
};

type PluginRecord = {
  id: string;
  // 同一 ID 可同时装在 user 与 project 两个范围；key 由服务端按 id|scope|projectPath 生成。
  key?: string;
  name: string;
  displayName?: string;
  description?: string;
  version?: string;
  marketplace?: string;
  scope: string;
  enabled: boolean;
  stateKnown: boolean;
  canDisable: boolean;
  installPath?: string;
  installedAt?: string;
  lastUpdated?: string;
  projectPath?: string;
  components: PluginComponents;
};

type AvailablePlugin = {
  id: string;
  name: string;
  description?: string;
  version?: string;
  marketplace?: string;
  installed: boolean;
  installCount?: number;
};

type PluginMarketplace = {
  name: string;
  kind?: string;
  source?: string;
  installLocation?: string;
  lastUpdated?: string;
};

type PluginListView = {
  runnerId: string;
  agentId: string;
  env: string;
  source: string;
  capabilities: PluginCapabilities;
  plugins: PluginRecord[];
  available?: AvailablePlugin[];
  marketplaces: PluginMarketplace[];
  note?: string;
};

type PluginDetails = PluginRecord & { tokenCost?: string; manifestPath?: string };

type PluginActionResult = {
  ok: boolean;
  message?: string;
  needsConfirmation?: boolean;
  command?: string;
  commandSha256?: string;
  output?: string;
};

type PluginAgent = { id: string; name: string; managed: boolean };

type Scope = "user" | "project" | "local";

const scopeLabel = (scope: string) => (scope === "project" ? "项目" : scope === "local" ? "本地" : "用户");

const scopeOptionLabel = (scope: Scope, detailed = false) =>
  scope === "project" ? (detailed ? "项目（写入项目配置）" : "项目")
    : scope === "local" ? (detailed ? "本地（仅本机当前项目）" : "本地")
      : (detailed ? "用户（全局）" : "用户");

const componentLine = (components: PluginComponents) => {
  const parts: string[] = [];
  if (components.skills) parts.push(`${components.skills} 技能`);
  if (components.commands) parts.push(`${components.commands} 命令`);
  if (components.agents) parts.push(`${components.agents} 代理`);
  if (components.hooks) parts.push(`${components.hooks} 钩子`);
  if (components.mcpServers) parts.push(`${components.mcpServers} MCP`);
  if (components.lspServers) parts.push(`${components.lspServers} LSP`);
  return parts.length ? parts.join(" · ") : "未清点到组件";
};

const marketplaceName = (plugin: Pick<AvailablePlugin, "marketplace" | "id">) =>
  plugin.marketplace || (plugin.id.includes("@") ? plugin.id.split("@")[1] : "");

const recordKey = (record: PluginRecord) => record.key || record.id;

const ALL_SCOPES: Scope[] = ["user", "project", "local"];

export default function PluginsPage() {
  const navigate = useNavigate();
  const { projects, refreshProjects } = useProjectContext();

  const [runners, setRunners] = useState<RunnerInfo[]>([]);
  const [runnersState, setRunnersState] = useState<"loading" | "ready" | "error">("loading");
  const [runnersError, setRunnersError] = useState("");
  const [runnerId, setRunnerId] = useState("");

  const [agents, setAgents] = useState<PluginAgent[]>([]);
  const [agentsState, setAgentsState] = useState<"loading" | "ready" | "error">("loading");
  const [agentsError, setAgentsError] = useState("");
  const [agentId, setAgentId] = useState("");

  const [view, setView] = useState<PluginListView | null>(null);
  const [viewState, setViewState] = useState<"loading" | "ready" | "error">("loading");
  const [viewError, setViewError] = useState("");
  const [available, setAvailable] = useState<AvailablePlugin[] | null>(null);
  const [availableError, setAvailableError] = useState("");
  const [availableLoading, setAvailableLoading] = useState(false);

  const [details, setDetails] = useState<PluginDetails | null>(null);
  const [detailsLoading, setDetailsLoading] = useState(false);
  const [detailsError, setDetailsError] = useState("");

  const [busy, setBusy] = useState("");
  const [marketOpen, setMarketOpen] = useState(false);
  const [marketSource, setMarketSource] = useState("");
  const [marketScope, setMarketScope] = useState<Scope>("user");
  const [marketProject, setMarketProject] = useState("");

  const [installTarget, setInstallTarget] = useState<AvailablePlugin | null>(null);
  const [installScope, setInstallScope] = useState<Scope>("user");
  const [installProject, setInstallProject] = useState("");
  const [installRunning, setInstallRunning] = useState(false);
  const [installConfirm, setInstallConfirm] = useState<PluginActionResult | null>(null);
  const [installError, setInstallError] = useState("");

  const [uninstallTarget, setUninstallTarget] = useState<PluginRecord | null>(null);
  const [removeMarket, setRemoveMarket] = useState<PluginMarketplace | null>(null);
  const [updateConfirm, setUpdateConfirm] = useState<{ record: PluginRecord; result: PluginActionResult } | null>(null);

  const requestVersion = useRef(0);
  const availableVersion = useRef(0);
  const detailsVersion = useRef(0);

  const supportedScopes = (view?.capabilities.scopes?.length ? view.capabilities.scopes : ALL_SCOPES).filter(
    (scope): scope is Scope => scope === "user" || scope === "project" || scope === "local",
  );

  // 切换执行环境 / 工具后，旧环境打开的详情、安装、卸载对话框都失去上下文，直接关掉，
  // 避免把"A 环境的插件"误操作到 B 环境。
  useEffect(() => {
    detailsVersion.current++;
    availableVersion.current++;
    setDetails(null);
    setDetailsError("");
    setInstallTarget(null);
    setInstallConfirm(null);
    setUninstallTarget(null);
    setRemoveMarket(null);
    setUpdateConfirm(null);
    setMarketOpen(false);
    // 项目选择属于上一个执行环境：不清掉会被当成"已选中"发给新环境，服务端拒绝。
    setMarketProject("");
    setInstallProject("");
    // 旧环境的在途请求不能继续冻结新环境的按钮：它自己的 finally 会再清一次 busy。
    setBusy("");
    setAvailableLoading(false);
    setDetailsLoading(false);
    setAvailable(null);
    setAvailableError("");
  }, [runnerId, agentId]);

  useEffect(() => {
    if (!supportedScopes.includes(marketScope)) setMarketScope(supportedScopes[0] || "user");
    if (!supportedScopes.includes(installScope)) setInstallScope(supportedScopes[0] || "user");
  }, [supportedScopes, marketScope, installScope]);

  useEffect(() => {
    void refreshProjects();
  }, [refreshProjects]);

  const loadRunners = useCallback(async () => {
    setRunnersState("loading");
    try {
      const list = await api<RunnerInfo[]>("/api/runners");
      setRunners(list);
      setRunnersState("ready");
      setRunnerId((current) => (current && list.some((item) => item.id === current) ? current : list[0]?.id || ""));
    } catch (cause) {
      setRunnersState("error");
      setRunnersError(cause instanceof Error ? cause.message : "无法读取执行环境");
    }
  }, []);

  useEffect(() => {
    void loadRunners();
  }, [loadRunners]);

  useEffect(() => {
    let alive = true;
    setAgentsState("loading");
    api<PluginAgent[]>("/api/plugin-agents")
      .then((list) => {
        if (!alive) return;
        setAgents(list);
        setAgentsState("ready");
        setAgentId((current) => (current && list.some((item) => item.id === current) ? current : list[0]?.id || ""));
      })
      .catch((cause) => {
        if (!alive) return;
        setAgentsState("error");
        setAgentsError(cause instanceof Error ? cause.message : "无法读取插件工具目录");
      });
    return () => {
      alive = false;
    };
  }, []);

  const loadView = useCallback(async () => {
    if (!runnerId || !agentId) return;
    const version = ++requestVersion.current;
    setViewState("loading");
    setViewError("");
    setAvailable(null);
    setAvailableError("");
    try {
      const result = await api<PluginListView>(`/api/runners/${runnerId}/plugins?agentId=${encodeURIComponent(agentId)}`);
      if (version !== requestVersion.current) return;
      setView(result);
      setViewState("ready");
    } catch (cause) {
      if (version !== requestVersion.current) return;
      setView(null);
      setViewState("error");
      setViewError(cause instanceof Error ? cause.message : "无法读取插件列表");
    }
  }, [runnerId, agentId]);

  useEffect(() => {
    void loadView();
  }, [loadView]);

  const loadAvailable = useCallback(async () => {
    if (!runnerId || !agentId) return;
    const version = ++availableVersion.current;
    setAvailableLoading(true);
    setAvailableError("");
    try {
      const result = await api<PluginListView>(
        `/api/runners/${runnerId}/plugins?agentId=${encodeURIComponent(agentId)}&available=1`,
      );
      if (version !== availableVersion.current) return;
      setAvailable(result.available || []);
    } catch (cause) {
      if (version !== availableVersion.current) return;
      setAvailable([]);
      setAvailableError(cause instanceof Error ? cause.message : "无法读取可安装插件");
    } finally {
      if (version === availableVersion.current) setAvailableLoading(false);
    }
  }, [runnerId, agentId]);

  const closeDetails = () => {
    // 让在途的详情请求失效：否则它在关闭之后才返回，会把抽屉重新打开。
    detailsVersion.current++;
    setDetails(null);
  };

  const openDetails = async (record: PluginRecord) => {
    const version = ++detailsVersion.current;
    setDetails({ ...record });
    setDetailsLoading(true);
    setDetailsError("");
    const params = new URLSearchParams({ agentId });
    if (record.scope) params.set("scope", record.scope);
    if (record.projectPath) params.set("recordProjectPath", record.projectPath);
    try {
      const result = await api<PluginDetails>(
        `/api/runners/${runnerId}/plugins/${encodeURIComponent(record.id)}/details?${params.toString()}`,
      );
      if (version !== detailsVersion.current) return;
      setDetails(result);
    } catch (cause) {
      if (version !== detailsVersion.current) return;
      setDetailsError(cause instanceof Error ? cause.message : "无法读取插件详情");
    } finally {
      if (version === detailsVersion.current) setDetailsLoading(false);
    }
  };

  const afterMutation = async (result: PluginActionResult, success: string) => {
    if (result.needsConfirmation) return result;
    toast[result.ok ? "success" : "error"](result.ok ? success : result.message || "操作失败");
    if (result.ok) {
      await loadView();
      if (available) await loadAvailable();
    }
    return result;
  };

  const mutationPath = (record: PluginRecord, action: string) =>
    `/api/runners/${runnerId}/plugins/${encodeURIComponent(record.id)}/${action}`;

  const runMutation = async (
    key: string,
    path: string,
    body: Record<string, unknown>,
    success: string,
    method: "POST" | "DELETE" = "POST",
  ) => {
    setBusy(key);
    try {
      const result = await api<PluginActionResult>(path, {
        method,
        body: body ? JSON.stringify(body) : undefined,
      });
      return await afterMutation(result, success);
    } catch (cause) {
      toast.error(cause instanceof Error ? cause.message : "操作失败");
      return null;
    } finally {
      setBusy((prev) => (prev === key ? "" : prev));
    }
  };

  const togglePlugin = async (record: PluginRecord) => {
    if (!record.stateKnown || !record.canDisable) return;
    const next = !record.enabled;
    if (next ? !view?.capabilities.enable : !view?.capabilities.disable) return;
    const result = await runMutation(
      `toggle:${recordKey(record)}`,
      mutationPath(record, next ? "enable" : "disable"),
      { agentId, scope: record.scope, projectId: "", recordProjectPath: record.projectPath || "" },
      next ? "已启用" : "已禁用",
    );
    // 启用/禁用没有确认弹窗；CLI 若要用户先确认命令，必须说出来，否则按钮转一圈复原、
    // 插件没变、也没有任何提示。
    if (result?.needsConfirmation) {
      toast.error(result.message || "CLI 要求先确认要执行的命令，请先在 CLI 中确认后重试");
    }
  };

  const updatePlugin = async (record: PluginRecord) => {
    const result = await runMutation(
      `update:${recordKey(record)}`,
      mutationPath(record, "update"),
      { agentId, scope: record.scope, recordProjectPath: record.projectPath || "" },
      "已更新",
    );
    // 升级同样可能触发市场声明的命令确认；此时 CLI 什么都没改，必须让用户看见命令。
    if (result?.needsConfirmation) setUpdateConfirm({ record, result });
  };

  const confirmUpdate = async () => {
    if (!updateConfirm) return;
    const { record, result } = updateConfirm;
    const retried = await runMutation(
      `update:${recordKey(record)}`,
      mutationPath(record, "update"),
      {
        agentId,
        scope: record.scope,
        recordProjectPath: record.projectPath || "",
        acceptCommandSha256: result.commandSha256 || "",
      },
      "已更新",
    );
    // 命令或市场目录在确认期间又变了，CLI 会再次要求确认，不能替用户关掉。
    if (retried?.needsConfirmation) setUpdateConfirm({ record, result: retried });
    else setUpdateConfirm(null);
  };

  const uninstallPlugin = async () => {
    if (!uninstallTarget) return;
    const params = new URLSearchParams({ agentId });
    if (uninstallTarget.scope) params.set("scope", uninstallTarget.scope);
    if (uninstallTarget.projectPath) params.set("recordProjectPath", uninstallTarget.projectPath);
    const result = await runMutation(
      `uninstall:${recordKey(uninstallTarget)}`,
      `/api/runners/${runnerId}/plugins/${encodeURIComponent(uninstallTarget.id)}?${params.toString()}`,
      {},
      "已卸载",
      "DELETE",
    );
    // 需要确认命令时什么都没卸掉，不能关对话框装作完成；请求失败（4xx/超时）
    // 同样要保留对话框，runMutation 已经弹过错误提示。
    if (result?.needsConfirmation) {
      toast.error(result.message || "CLI 要求先确认要执行的命令，请先在 CLI 中确认后重试");
      return;
    }
    if (!result?.ok) return;
    setUninstallTarget(null);
  };

  const submitInstall = async (acceptCommandSha256 = "") => {
    if (!installTarget) return;
    if (installScope !== "user" && !installProject) {
      setInstallError("项目范围需要先选择一个项目");
      return;
    }
    setInstallRunning(true);
    setInstallError("");
    try {
      const result = await api<PluginActionResult>(`/api/runners/${runnerId}/plugins/install`, {
        method: "POST",
        body: JSON.stringify({
          agentId,
          plugin: installTarget.id,
          marketplace: marketplaceName(installTarget),
          scope: installScope,
          projectId: installProject,
          acceptCommandSha256,
        }),
      });
      if (result.needsConfirmation) {
        setInstallConfirm(result);
        return;
      }
      await afterMutation(result, "已安装");
      if (result.ok) {
        setInstallTarget(null);
        setInstallConfirm(null);
      }
    } catch (cause) {
      setInstallError(cause instanceof Error ? cause.message : "安装失败");
    } finally {
      setInstallRunning(false);
    }
  };

  const addMarketplace = async () => {
    const source = marketSource.trim();
    if (!source) return;
    if (marketScope !== "user" && !marketProject) {
      toast.error("项目范围需要先选择一个项目");
      return;
    }
    setBusy("market:add");
    try {      const result = await api<PluginActionResult>(`/api/runners/${runnerId}/plugin-marketplaces`, {
        method: "POST",
        body: JSON.stringify({ agentId, source, scope: marketScope, projectId: marketProject }),
      });
      if (result.needsConfirmation) {
        toast.error("该市场要求先确认命令，请稍后重试");
        return;
      }
      toast[result.ok ? "success" : "error"](result.ok ? "已添加市场" : result.message || "添加失败");
      if (result.ok) {
        setMarketSource("");
        await loadView();
      }
    } catch (cause) {
      toast.error(cause instanceof Error ? cause.message : "添加市场失败");
    } finally {
      // 切环境时 busy 会被重置；这里只在仍属于本次操作时才清，避免把新环境的进度清掉。
      setBusy((prev) => (prev === "market:add" ? "" : prev));
    }
  };

  const refreshMarketplaces = async (name = "") => {
    const key = `market:update:${name}`;
    setBusy(key);
    try {
      const result = await api<PluginActionResult>(`/api/runners/${runnerId}/plugin-marketplaces`, {
        method: "PUT",
        body: JSON.stringify({ agentId, name }),
      });
      toast[result.ok ? "success" : "error"](result.ok ? "市场已刷新" : result.message || "刷新失败");
      if (result.ok) await loadView();
    } catch (cause) {
      toast.error(cause instanceof Error ? cause.message : "刷新市场失败");
    } finally {
      setBusy((prev) => (prev === key ? "" : prev));
    }
  };

  const removeMarketplace = async () => {
    if (!removeMarket) return;
    const key = `market:remove:${removeMarket.name}`;
    setBusy(key);
    try {
      const result = await api<PluginActionResult>(
        `/api/runners/${runnerId}/plugin-marketplaces/${encodeURIComponent(removeMarket.name)}?agentId=${encodeURIComponent(agentId)}`,
        { method: "DELETE" },
      );
      toast[result.ok ? "success" : "error"](result.ok ? "已移除市场" : result.message || "移除失败");
      if (result.ok) await loadView();
    } catch (cause) {
      toast.error(cause instanceof Error ? cause.message : "移除市场失败");
    } finally {
      setBusy((prev) => (prev === key ? "" : prev));
      setRemoveMarket(null);
    }
  };

  const projectChoices = (): Project[] => {
    const sameRunner = projects.filter((project) => !project.runner || project.runner === runnerId);
    return sameRunner.length ? sameRunner : projects;
  };

  const capabilities = view?.capabilities;
  const busyOrLoading = busy !== "";

  return (
    <main className="app-shell plugins-shell">
      <header className="dashboard-bar plugins-bar">
        <button type="button" className="plugins-back" aria-label="返回首页" onClick={() => navigate("/")}>←</button>
        <span className="plugins-badge" aria-hidden="true">◈</span>
        <h1 className="plugins-title">插件</h1>
        {runnersState === "ready" && runners.length > 1 && (
          <div className="plugins-env" role="tablist" aria-label="执行环境">
            {runners.map((runner) => (
              <button
                key={runner.id}
                type="button"
                role="tab"
                aria-selected={runner.id === runnerId}
                className="plugins-env-item"
                onClick={() => setRunnerId(runner.id)}
              >
                {runner.name}
              </button>
            ))}
          </div>
        )}
        <div className="plugins-bar-actions">
          <button type="button" className="secondary" disabled={viewState === "loading"} onClick={() => void loadView()}>
            <span aria-hidden="true">↻</span> 刷新
          </button>
        </div>
      </header>

      <section className="plugins-body">
        {agentsState === "error" && (
          <div className="plugins-block plugins-error" role="alert">
            <b>读不到插件工具目录</b>
            <span>{agentsError}</span>
          </div>
        )}
        {agentsState === "ready" && agents.length === 0 && (
          <div className="plugins-block plugins-empty">
            <b>没有支持插件的工具</b>
            <span>先在「Cli管理」里安装 Claude Code、Codex 或 Qoder。</span>
            <button className="primary" type="button" onClick={() => navigate("/cli-tools")}>去 Cli管理</button>
          </div>
        )}

        {agents.length > 0 && (
          <div className="plugins-agents" role="tablist" aria-label="工具">
            {agents.map((agent) => (
              <button
                key={agent.id}
                type="button"
                role="tab"
                aria-selected={agent.id === agentId}
                className="plugins-agent"
                onClick={() => setAgentId(agent.id)}
              >
                <span>{agent.name}</span>
                <small>{agent.managed ? "可管理" : "只读"}</small>
              </button>
            ))}
          </div>
        )}

        {runnersState === "error" && (
          <div className="plugins-block plugins-error" role="alert">
            <b>读不到执行环境</b>
            <span>{runnersError}</span>
            <button className="secondary" type="button" onClick={() => void loadRunners()}>重试</button>
          </div>
        )}
        {runnersState === "ready" && runners.length === 0 && (
          <div className="plugins-block plugins-empty">
            <b>没有可用的执行环境</b>
            <button className="primary" type="button" onClick={() => navigate("/")}>回首页</button>
          </div>
        )}

        {runners.length > 0 && agents.length > 0 && (
          <>
            {viewState === "loading" && (
              <div className="plugins-skeleton" role="status" aria-label="正在读取插件">
                {[0, 1, 2].map((index) => (
                  <span key={index} className="plugins-skeleton-card" />
                ))}
              </div>
            )}

            {viewState === "error" && (
              <div className="plugins-block plugins-error" role="alert">
                <b>读不到插件列表</b>
                <span>{viewError}</span>
                <button className="secondary" type="button" onClick={() => void loadView()}>重试</button>
              </div>
            )}

            {viewState === "ready" && view && (
              <>
                {view.note && <div className="plugins-note" role="status">{view.note}</div>}

                {view.source === "scan" && (
                  <div className="plugins-block plugins-warn" role="status">
                    <b>这个工具没有插件管理命令</b>
                    <span>下面只列出磁盘上发现的插件，启用状态未知，也不能在这里启停。</span>
                  </div>
                )}

                <div className="plugins-sechead">
                  <div>
                    <h2>已安装</h2>
                    <span className="plugins-count">
                      {view.plugins.length} 个 · 已启用 {view.plugins.filter((item) => item.stateKnown && item.enabled).length} 个
                    </span>
                  </div>
                  <div className="plugins-sechead-actions">
                    {capabilities?.install && (
                      <button
                        className="primary"
                        type="button"
                        onClick={() => {
                          setInstallTarget({ id: "", name: "", installed: false });
                          setInstallScope("user");
                          setInstallProject("");
                          setInstallError("");
                          setInstallConfirm(null);
                          if (!available) void loadAvailable();
                        }}
                      >
                        安装插件
                      </button>
                    )}
                    {capabilities?.marketplaces && (
                      <button className="secondary" type="button" onClick={() => setMarketOpen((open) => !open)}>
                        {marketOpen ? "收起市场" : `插件市场（${view.marketplaces.length}）`}
                      </button>
                    )}
                  </div>
                </div>

                {view.plugins.length === 0 ? (
                  <div className="plugins-block plugins-empty">
                    <b>还没有安装插件</b>
                    <span>从已配置的插件市场安装，或先添加一个市场。</span>
                    {capabilities?.install && (
                      <button
                        className="primary"
                        type="button"
                        onClick={() => {
                          setInstallTarget({ id: "", name: "", installed: false });
                          setInstallScope("user");
                          setInstallProject("");
                          setInstallError("");
                          setInstallConfirm(null);
                          if (!available) void loadAvailable();
                        }}
                      >
                        安装插件
                      </button>
                    )}
                  </div>
                ) : (
                  <div className="plugins-grid">
                    {view.plugins.map((record) => {
                      const title = record.displayName || record.name;
                      const toggleable = record.stateKnown && record.canDisable && capabilities?.enable && capabilities?.disable;
                      return (
                        <article key={recordKey(record)} className="plugins-card" data-disabled={record.stateKnown && !record.enabled ? "true" : undefined}>
                          <header className="plugins-card-head">
                            <div>
                              <b>{title}</b>
                              <span className="plugins-card-id">{record.id}</span>
                            </div>
                            {record.stateKnown ? (
                              <span className="plugins-chip" data-tone={record.enabled ? "on" : "off"}>
                                {record.enabled ? "已启用" : "已禁用"}
                              </span>
                            ) : (
                              <span className="plugins-chip" data-tone="unknown">状态未知</span>
                            )}
                          </header>
                          {record.description && <p className="plugins-card-desc">{record.description}</p>}
                          <dl className="plugins-meta">
                            <div><dt>版本</dt><dd>{record.version || "—"}</dd></div>
                            <div><dt>范围</dt><dd>{scopeLabel(record.scope)}</dd></div>
                            <div><dt>市场</dt><dd>{record.marketplace || "—"}</dd></div>
                            {record.projectPath && (
                              <div><dt>项目</dt><dd className="plugins-path">{record.projectPath}</dd></div>
                            )}
                          </dl>
                          <p className="plugins-components">{componentLine(record.components)}</p>
                          <footer className="plugins-card-actions">
                            {toggleable && (
                              <button
                                type="button"
                                className="plugins-toggle"
                                role="switch"
                                aria-checked={record.enabled}
                                aria-label={record.enabled ? `禁用 ${title}` : `启用 ${title}`}
                                disabled={busyOrLoading}
                                onClick={() => void togglePlugin(record)}
                              >
                                {busy === `toggle:${recordKey(record)}` ? "处理中…" : record.enabled ? "禁用" : "启用"}
                              </button>
                            )}
                            {capabilities?.details && (
                              <button type="button" className="secondary" onClick={() => void openDetails(record)}>
                                详情
                              </button>
                            )}
                            {capabilities?.update && (
                              <button
                                type="button"
                                className="secondary"
                                disabled={busyOrLoading}
                                onClick={() => void updatePlugin(record)}
                              >
                                {busy === `update:${recordKey(record)}` ? "更新中…" : "更新"}
                              </button>
                            )}
                            {capabilities?.uninstall && (
                              <button type="button" className="danger" disabled={busyOrLoading} onClick={() => setUninstallTarget(record)}>
                                卸载
                              </button>
                            )}
                          </footer>
                        </article>
                      );
                    })}
                  </div>
                )}

                {marketOpen && capabilities?.marketplaces && (
                  <section className="plugins-market" aria-label="插件市场">
                    <header className="plugins-market-head">
                      <h2>插件市场</h2>
                      <button className="secondary" type="button" disabled={!view.marketplaces.length || busyOrLoading} onClick={() => void refreshMarketplaces()}>
                        全部刷新
                      </button>
                    </header>
                    {view.marketplaces.length === 0 ? (
                      <p className="plugins-market-empty">还没有配置市场。添加一个 GitHub 仓库、URL 或本地路径即可安装插件。</p>
                    ) : (
                      <ul className="plugins-market-list">
                        {view.marketplaces.map((market) => (
                          <li key={market.name}>
                            <div>
                              <b>{market.name}</b>
                              <small>{market.source || market.installLocation || "—"}</small>
                            </div>
                            <div className="plugins-market-actions">
                              <button className="secondary" type="button" disabled={busyOrLoading} onClick={() => void refreshMarketplaces(market.name)}>
                                {busy === `market:update:${market.name}` ? "刷新中…" : "刷新"}
                              </button>
                              <button className="danger" type="button" disabled={busyOrLoading} onClick={() => setRemoveMarket(market)}>
                                移除
                              </button>
                            </div>
                          </li>
                        ))}
                      </ul>
                    )}
                    <div className="plugins-market-add">
                      <input
                        value={marketSource}
                        onChange={(event) => setMarketSource(event.target.value)}
                        placeholder="owner/repo、Git URL 或本地路径"
                        aria-label="市场来源"
                      />
                      <select value={marketScope} onChange={(event) => setMarketScope(event.target.value as Scope)} aria-label="市场范围">
                        {supportedScopes.map((scope) => (
                          <option key={scope} value={scope}>{scopeOptionLabel(scope)}</option>
                        ))}
                      </select>
                      {marketScope !== "user" && (
                        <select value={marketProject} onChange={(event) => setMarketProject(event.target.value)} aria-label="选择项目">
                          <option value="">选择项目</option>
                          {projectChoices().map((project) => (
                            <option key={project.id} value={project.id}>{project.name}</option>
                          ))}
                        </select>
                      )}
                      <button className="primary" type="button" disabled={busyOrLoading || !marketSource.trim()} onClick={() => void addMarketplace()}>
                        添加市场
                      </button>
                    </div>
                  </section>
                )}
              </>
            )}
          </>
        )}
      </section>

      {details && (
        <div className="plugins-drawer-backdrop" role="presentation" onClick={closeDetails}>
          <aside className="plugins-drawer" role="dialog" aria-modal="true" aria-label="插件详情" onClick={(event) => event.stopPropagation()}>
            <header>
              <div>
                <h2>{details.displayName || details.name}</h2>
                <span>{details.id}</span>
              </div>
              <button type="button" className="plugins-drawer-close" aria-label="关闭" onClick={closeDetails}>×</button>
            </header>
            {detailsError && <div className="plugins-block plugins-error" role="alert">{detailsError}</div>}
            {detailsLoading && <p className="plugins-pending">正在读取详情…</p>}
            {details.description && <p className="plugins-drawer-desc">{details.description}</p>}
            <dl className="plugins-drawer-facts">
              <div><dt>版本</dt><dd>{details.version || "—"}</dd></div>
              <div><dt>范围</dt><dd>{scopeLabel(details.scope)}</dd></div>
              <div><dt>市场</dt><dd>{details.marketplace || "—"}</dd></div>
              <div><dt>状态</dt><dd>{details.stateKnown ? (details.enabled ? "已启用" : "已禁用") : "未知"}</dd></div>
              <div><dt>组件</dt><dd>{componentLine(details.components)}</dd></div>
              {details.tokenCost && <div><dt>常驻开销</dt><dd>{details.tokenCost}</dd></div>}
              {details.manifestPath && <div><dt>清单</dt><dd className="plugins-path">{details.manifestPath}</dd></div>}
              {details.installPath && <div><dt>安装位置</dt><dd className="plugins-path">{details.installPath}</dd></div>}
              {details.projectPath && <div><dt>项目</dt><dd className="plugins-path">{details.projectPath}</dd></div>}
            </dl>
            <p className="plugins-drawer-tip">插件可能包含技能、命令、代理、钩子和 MCP/LSP 服务；启用前请确认来源可信。</p>
          </aside>
        </div>
      )}

      {installTarget && (
        <div className="plugins-drawer-backdrop" role="presentation" onClick={() => setInstallTarget(null)}>
          <div className="plugins-modal" role="dialog" aria-modal="true" aria-label="安装插件" onClick={(event) => event.stopPropagation()}>
            <header>
              <div>
                <h2>安装插件</h2>
                <span>从当前工具已配置的市场安装</span>
              </div>
              <button type="button" className="plugins-drawer-close" aria-label="关闭" onClick={() => setInstallTarget(null)}>×</button>
            </header>

            {installConfirm ? (
              <div className="plugins-confirm">
                <b>该插件声明了待执行命令</b>
                <p>只有在你确认来源可信时才继续。命令如下：</p>
                <pre>{installConfirm.command || "（CLI 未返回命令内容）"}</pre>
                <p className="plugins-path">sha256: {installConfirm.commandSha256}</p>
                <div className="plugins-modal-actions">
                  <button className="secondary" type="button" onClick={() => setInstallConfirm(null)}>取消</button>
                  <button className="primary" type="button" disabled={installRunning} onClick={() => void submitInstall(installConfirm.commandSha256 || "")}>
                    {installRunning ? "安装中…" : "确认执行并安装"}
                  </button>
                </div>
              </div>
            ) : (
              <>
                {availableLoading && <p className="plugins-pending">正在读取可安装插件…</p>}
                {availableError && (
                  <div className="plugins-block plugins-error" role="alert">
                    <b>读不到可安装插件</b>
                    <span>{availableError}</span>
                    <button className="secondary" type="button" onClick={() => void loadAvailable()}>重试</button>
                  </div>
                )}
                {available && available.length === 0 && !availableError && (
                  <p className="plugins-market-empty">当前市场没有可安装的插件。先在上方添加一个市场。</p>
                )}
                <label className="plugins-field">
                  <span>选择插件</span>
                  <select value={installTarget.id} onChange={(event) => {
                    const picked = available?.find((item) => item.id === event.target.value);
                    setInstallTarget(picked || { id: "", name: "", installed: false });
                  }}>
                    <option value="">从市场选择</option>
                    {(available || []).filter((item) => !item.installed).map((item) => (
                      <option key={item.id} value={item.id}>
                        {item.name}{item.version ? ` · ${item.version}` : ""}{marketplaceName(item) ? ` @ ${marketplaceName(item)}` : ""}
                      </option>
                    ))}
                  </select>
                </label>
                {installTarget.id && (
                  <p className="plugins-components">
                    {installTarget.description || "该市场没有提供说明。"} {marketplaceName(installTarget) && `· 市场 ${marketplaceName(installTarget)}`}
                  </p>
                )}
                <div className="plugins-modal-row">
                  <label className="plugins-field">
                    <span>范围</span>
                    <select value={installScope} onChange={(event) => setInstallScope(event.target.value as Scope)}>
                      {supportedScopes.map((scope) => (
                        <option key={scope} value={scope}>{scopeOptionLabel(scope, true)}</option>
                      ))}
                    </select>
                  </label>
                  {installScope !== "user" && (
                    <label className="plugins-field">
                      <span>项目</span>
                      <select value={installProject} onChange={(event) => setInstallProject(event.target.value)}>
                        <option value="">选择项目</option>
                        {projectChoices().map((project) => (
                          <option key={project.id} value={project.id}>{project.name}</option>
                        ))}
                      </select>
                    </label>
                  )}
                </div>
                {installScope !== "user" && (
                  <p className="plugins-confirm-hint">项目范围会写入所选项目的 CLI 配置，可能被提交到版本库。</p>
                )}
                {installError && <div className="plugins-block plugins-error" role="alert">{installError}</div>}
                <div className="plugins-modal-actions">
                  <button className="secondary" type="button" onClick={() => setInstallTarget(null)}>取消</button>
                  <button
                    className="primary"
                    type="button"
                    disabled={installRunning || !installTarget.id || (installScope !== "user" && !installProject)}
                    onClick={() => void submitInstall()}
                  >
                    {installRunning ? "安装中…" : "安装"}
                  </button>
                </div>
              </>
            )}
          </div>
        </div>
      )}

      {uninstallTarget && (
        <div className="plugins-drawer-backdrop" role="presentation" onClick={() => setUninstallTarget(null)}>
          <div className="plugins-modal plugins-modal-small" role="dialog" aria-modal="true" aria-label="卸载插件" onClick={(event) => event.stopPropagation()}>
            <header>
              <h2>卸载 {uninstallTarget.displayName || uninstallTarget.name}？</h2>
            </header>
            <p>卸载会移除该插件在当前 CLI 里的安装，插件带来的技能与命令会同时失效。</p>
            <div className="plugins-modal-actions">
              <button className="secondary" type="button" onClick={() => setUninstallTarget(null)}>取消</button>
              <button className="danger" type="button" disabled={busyOrLoading} onClick={() => void uninstallPlugin()}>
                {busy === `uninstall:${recordKey(uninstallTarget)}` ? "卸载中…" : "卸载"}
              </button>
            </div>
          </div>
        </div>
      )}

      {updateConfirm && (
        <div className="plugins-drawer-backdrop" role="presentation" onClick={() => setUpdateConfirm(null)}>
          <div className="plugins-modal" role="dialog" aria-modal="true" aria-label="确认升级命令" onClick={(event) => event.stopPropagation()}>
            <header>
              <div>
                <h2>升级需要确认命令</h2>
                <span>{updateConfirm.record.displayName || updateConfirm.record.name}</span>
              </div>
              <button type="button" className="plugins-drawer-close" aria-label="关闭" onClick={() => setUpdateConfirm(null)}>×</button>
            </header>
            <div className="plugins-confirm">
              <b>该插件的市场声明了待执行命令</b>
              <p>只有在你确认来源可信时才继续。命令如下：</p>
              <pre>{updateConfirm.result.command || "（CLI 未返回命令内容）"}</pre>
              <p className="plugins-path">sha256: {updateConfirm.result.commandSha256}</p>
              <div className="plugins-modal-actions">
                <button className="secondary" type="button" onClick={() => setUpdateConfirm(null)}>取消</button>
                <button className="primary" type="button" disabled={busyOrLoading} onClick={() => void confirmUpdate()}>
                  {busyOrLoading ? "升级中…" : "确认执行并升级"}
                </button>
              </div>
            </div>
          </div>
        </div>
      )}

      {removeMarket && (
        <div className="plugins-drawer-backdrop" role="presentation" onClick={() => setRemoveMarket(null)}>
          <div className="plugins-modal plugins-modal-small" role="dialog" aria-modal="true" aria-label="移除市场" onClick={(event) => event.stopPropagation()}>
            <header>
              <h2>移除市场 {removeMarket.name}？</h2>
            </header>
            <p>移除会删除同名市场在所有范围（用户 / 项目 / 本地）里的声明，已经安装的插件不会自动卸载。</p>
            <div className="plugins-modal-actions">
              <button className="secondary" type="button" onClick={() => setRemoveMarket(null)}>取消</button>
              <button className="danger" type="button" disabled={busyOrLoading} onClick={() => void removeMarketplace()}>移除</button>
            </div>
          </div>
        </div>
      )}
    </main>
  );
}
