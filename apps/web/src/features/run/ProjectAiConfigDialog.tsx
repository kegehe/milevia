// 项目级 AI 配置 — 会话页头部"AI 配置"入口打开的弹窗。
// 方案 B（标签页版式）：一次只呈现一个工具的配置，标签由服务端目录驱动 ——
// 目录里将来多出第三个工具时自动多一个标签，不需要改这里。
// 底层仍复用 agent profile 的注入物理，这里只把"档案"心智收敛为"项目配置"：
// 普通用户不用理解 停用/撤销/版本/revision。实现细节文案（Key 如何存储、
// 如何注入）也不上屏 —— 留空/必填一律用 placeholder 表达。
import { useCallback, useEffect, useState } from "react";
import { api } from "../../lib/api";
import type { AgentID } from "../../lib/types";
import { agentDisplayName, catalogAgentID, useAgentCatalogState } from "../../lib/agent-registry";
import "./project-ai-config.css";

// GET /api/projects/{id}/agent-config 的只读视图。
type AgentEntry = {
  profileId?: string;
  poolRevisionId?: string;
  mode?: "pinned" | "pool" | "cli_managed" | string;
  model?: string;
  baseUrl?: string;
  authMode: "cli_managed" | "api_key" | string;
  isDefault: boolean;
  enabled: boolean;
  state: string;
};
type AgentConfigView = {
  runnerId: string;
  runnerManaged: boolean;
  // 按工具目录索引的视图（服务端派生）。claude / codex 两个键是过渡兼容，
  // 值由 agents 派生，前端不再分别读它们。
  agents?: Record<string, AgentEntry | null>;
  claude?: AgentEntry | null;
  codex?: AgentEntry | null;
};
type CredentialPool = {
  currentRevisionId: string;
  name: string;
  members: { agentId: AgentID }[];
};

type AgentDraft = { model: string; baseUrl: string; authMode: "cli_managed" | "api_key"; apiKey: string };

function AuthModeSwitch({ mode, onChange, disabled }: { mode: string; onChange: (next: "cli_managed" | "api_key") => void; disabled: boolean }) {
  return <div className="project-config-authmode">
    <button type="button" className={mode === "cli_managed" ? "active" : ""} disabled={disabled} onClick={() => onChange("cli_managed")}>CLI 登录</button>
    <button type="button" className={mode === "api_key" ? "active" : ""} disabled={disabled} onClick={() => onChange("api_key")}>API Key</button>
  </div>;
}

export function ProjectAiConfigDialog({ projectId, runnerID, close }: { projectId: string; runnerID: string; close: () => void }) {
  // 工具清单来自服务端目录（含 loaded / error 三态），本文件不再自己维护一份。
  const catalog = useAgentCatalogState();
  const [config, setConfig] = useState<AgentConfigView | null>(null);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [savingAgent, setSavingAgent] = useState<AgentID | null>(null);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [pools, setPools] = useState<CredentialPool[]>([]);
  // 凭据池列表"读不到"要单独记住并如实上屏，不能折叠成空数组 —— 那是把读不到写成没有。
  const [poolsFailed, setPoolsFailed] = useState(false);
  const [poolSelections, setPoolSelections] = useState<Partial<Record<AgentID, string>>>({});
  // 当前标签：null 表示用户还没点过（回落到目录第一个）。
  const [activeId, setActiveId] = useState<AgentID | null>(null);
  // 凭据池是低频动作，收成安静链接 + 可展开的一行，不再常驻每个卡片。
  const [poolPanelOpen, setPoolPanelOpen] = useState(false);
  // 编辑草稿：keyed by agentId
  const [drafts, setDrafts] = useState<Partial<Record<AgentID, AgentDraft>>>({});

  const refresh = useCallback(async () => {
    setLoading(true);
    try {
      const [nextConfig, poolsResult] = await Promise.all([
        api<AgentConfigView>(`/api/projects/${projectId}/agent-config`),
        api<CredentialPool[]>(`/api/runners/${runnerID}/credential-pools`)
          .then((value) => ({ ok: true as const, value }))
          .catch(() => ({ ok: false as const })),
      ]);
      // 配置视图缺 agents 键 = 服务端给了半份数据：按"读不到"处理（走 catch 置空 + 报错），
      // 绝不能拿着残缺视图渲染出全空的"可编辑表单"，让用户把半盲配置写回去。
      if (!nextConfig.agents) throw new Error("配置视图不完整，请重试。");
      setConfig(nextConfig);
      if (poolsResult.ok) {
        setPools(poolsResult.value);
        setPoolsFailed(false);
      } else {
        setPools([]);
        setPoolsFailed(true);
      }
      setError("");
    } catch (cause) {
      setConfig(null);
      setError(cause instanceof Error ? cause.message : "无法读取项目 AI 配置");
    } finally {
      setLoading(false);
    }
  }, [projectId, runnerID]);

  useEffect(() => { void refresh(); }, [refresh]);

  const manageable = config?.runnerManaged ?? true;
  const entryFor = (agentId: AgentID): AgentEntry | null | undefined => config?.agents?.[agentId] ?? null;
  const draftFor = (agentId: AgentID): AgentDraft => {
    const draft = drafts[agentId];
    const entry = entryFor(agentId);
    return {
      model: draft?.model ?? entry?.model ?? "",
      baseUrl: draft?.baseUrl ?? entry?.baseUrl ?? "",
      authMode: draft?.authMode ?? (entry?.authMode === "api_key" ? "api_key" : "cli_managed"),
      apiKey: draft?.apiKey ?? "",
    };
  };
  const setAgentDraft = (agentId: AgentID, patch: Partial<AgentDraft>) => {
    setDrafts((current) => ({ ...current, [agentId]: { ...draftFor(agentId), ...patch } }));
  };

  const activeEntry = catalog.entries.find((item) => catalogAgentID(item) === activeId) ?? catalog.entries[0] ?? null;
  const id = activeEntry ? catalogAgentID(activeEntry) : null;
  const entry = id ? entryFor(id) ?? null : null;
  const draft = id ? draftFor(id) : null;
  const busy = saving && savingAgent === id;
  const poolRoute = entry?.mode === "pool";
  const compatiblePools = id ? pools.filter((pool) => pool.members.some((member) => member.agentId === id)) : [];

  const selectTab = (next: AgentID) => {
    setActiveId(next);
    setPoolPanelOpen(false);
    // 横幅指向的是上一个工具的操作结果，切标签还挂着会指错对象。
    setError("");
    setNotice("");
  };

  // 认证方式从 CLI 登录切到 API Key 时，仅当本地还没配 Key 才允许继续（创建时必填）。
  const saveAgent = async (agentId: AgentID) => {
    if (saving) return;
    const agentDraft = draftFor(agentId);
    const agentEntry = entryFor(agentId);
    // 未配置任何东西且原本也没有 profile：直接清除默认即可，无需新建。
    if (!agentEntry?.profileId && !agentDraft.model && !agentDraft.baseUrl && agentDraft.authMode === "cli_managed" && !agentDraft.apiKey) {
      setSaving(true);
      setSavingAgent(agentId);
      setError("");
      setNotice("");
      try {
        await api(`/api/projects/${projectId}/agent-profile`, { method: "PATCH", body: JSON.stringify({ agentId, profileId: "" }) });
        setNotice("已清空该工具的配置，使用 CLI 原有配置。");
        await refresh();
      } catch (cause) {
        setError(cause instanceof Error ? cause.message : "无法保存配置");
      } finally {
        setSaving(false);
        setSavingAgent(null);
      }
      return;
    }
    // 新建 API Key 档案时 Key 必填：placeholder 写了"必填"，这里就要真的拦住，
    // 不能放行一个没有 Key 的 api_key 档案出去。已有 profile 时留空 = 保持现有 Key，放行。
    if (agentDraft.authMode === "api_key" && !agentEntry?.profileId && !agentDraft.apiKey) {
      setNotice("");
      setError("创建 API Key 配置前，先填写 API Key。");
      return;
    }
    setSaving(true);
    setSavingAgent(agentId);
    setError("");
    setNotice("");
    try {
      let profileId = agentEntry?.profileId || "";
      const body: Record<string, unknown> = { model: agentDraft.model, authMode: agentDraft.authMode };
      let name = "项目配置";
      if (agentDraft.authMode === "api_key") {
        body.baseUrl = agentDraft.baseUrl;
        if (agentDraft.apiKey) body.apiKey = agentDraft.apiKey;
      }
      if (profileId) {
        await api(`/api/agent-profiles/${profileId}`, { method: "PATCH", body: JSON.stringify(body) });
      } else {
        const created = await api<{ profile: { id: string } }>(`/api/runners/${runnerID}/agent-profiles`, {
          method: "POST",
          body: JSON.stringify({ agentId, name, ...body }),
        });
        profileId = created.profile.id;
      }
      // 设为本项目默认，新会话自动套用。
      await api(`/api/projects/${projectId}/agent-profile`, { method: "PATCH", body: JSON.stringify({ agentId, profileId }) });
      setDrafts((current) => ({ ...current, [agentId]: { model: agentDraft.model, baseUrl: agentDraft.baseUrl, authMode: agentDraft.authMode, apiKey: "" } }));
      setNotice(`已保存 ${agentDisplayName(agentId)} 配置，新会话将自动应用。`);
      await refresh();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "无法保存配置");
    } finally {
      setSaving(false);
      setSavingAgent(null);
    }
  };

  const clearDefault = async (agentId: AgentID) => {
    if (saving) return;
    setSaving(true);
    setError("");
    setNotice("");
    try {
      await api(`/api/projects/${projectId}/agent-profile`, { method: "PATCH", body: JSON.stringify({ agentId, profileId: "" }) });
      // 服务端已清空：本地草稿必须一并清掉 —— 否则残留草稿会盖过服务端空值，
      // 下次点"保存"把刚清掉的配置原样复活（草稿优先级高于 entry，见 draftFor）。
      setDrafts((current) => {
        if (!(agentId in current)) return current;
        const next = { ...current };
        delete next[agentId];
        return next;
      });
      setNotice(`已清空 ${agentDisplayName(agentId)} 配置，之后新会话使用 CLI 原有配置。`);
      await refresh();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "无法清空配置");
    } finally {
      setSaving(false);
    }
  };

  const applyPool = async (agentId: AgentID) => {
    const poolRevisionId = poolSelections[agentId];
    if (!poolRevisionId || saving) return;
    setSaving(true); setSavingAgent(agentId); setError(""); setNotice("");
    try {
      await api(`/api/projects/${projectId}/agent-pool`, { method: "POST", body: JSON.stringify({ agentId, poolRevisionId }) });
      // 与 clearDefault 同理：池路由下表单已让位，残留草稿留着只会在解除池后复活旧配置。
      setDrafts((current) => {
        if (!(agentId in current)) return current;
        const next = { ...current };
        delete next[agentId];
        return next;
      });
      setNotice(`已为 ${agentDisplayName(agentId)} 应用凭据池。`);
      setPoolPanelOpen(false);
      await refresh();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "无法应用凭据池");
    } finally {
      setSaving(false); setSavingAgent(null);
    }
  };

  return <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="project-config-title" onClick={(event) => { if (event.target === event.currentTarget && !saving) close(); }}>
    <section className="modal project-config-dialog">
      <header>
        <div className="project-config-heading"><span className="project-config-mark"><ProjectConfigGlyph /></span><h2 id="project-config-title">项目 AI 配置</h2></div>
        <button type="button" className="project-config-close" title="关闭" aria-label="关闭" disabled={saving} onClick={close}>×</button>
      </header>
      {!manageable ? <div className="project-config-remote">
        <p>该项目运行在<strong>远端 Runner</strong>（SSH 或 Windows 桌面版 WSL）。远端默认使用其所在机器上已登录的 CLI 配置，本机暂不能为它注入 API Key / base_url。</p>
        <footer><button className="secondary" onClick={close}>关闭</button></footer>
      </div>
      : <>
        {error && <div className="project-config-error" role="alert">{error}</div>}
        {notice && <div className="project-config-notice" role="status">{notice}</div>}
        {/* 三态分开：正在读取 / 读取失败（只显示错误横幅，绝不渲染空表单 —— 那会把
            "配置读不到"伪装成"什么都没配"，用户此时点保存会误清配置）/ 正常渲染。 */}
        {loading && !config ? <p className="project-config-loading">正在读取…</p>
          : !config ? <footer className="project-config-dialog-footer"><button type="button" className="secondary" disabled={saving} onClick={close}>关闭</button></footer>
          : id && draft ? <>
            <div className="project-config-tabs" role="tablist" aria-label="工具"
              onKeyDown={(event) => {
                // ARIA tabs 约定：←/→ 在标签间移动选择与焦点（roving tabindex）。
                if (event.key !== "ArrowLeft" && event.key !== "ArrowRight") return;
                event.preventDefault();
                const ids = catalog.entries.map(catalogAgentID);
                const index = ids.indexOf(id);
                if (index < 0 || ids.length < 2) return;
                const nextId = ids[(index + (event.key === "ArrowRight" ? 1 : ids.length - 1)) % ids.length];
                selectTab(nextId);
                requestAnimationFrame(() => document.getElementById(`project-config-tab-${nextId}`)?.focus());
              }}>
              {catalog.entries.map((catalogEntry) => {
                const tabId = catalogAgentID(catalogEntry);
                const tabEntry = entryFor(tabId);
                return <button key={tabId} type="button" role="tab" id={`project-config-tab-${tabId}`}
                  aria-selected={tabId === id} aria-controls="project-config-panel"
                  tabIndex={tabId === id ? 0 : -1}
                  className={`project-config-tab${tabId === id ? " is-active" : ""}`}
                  onClick={() => selectTab(tabId)}>
                  {catalogEntry.name}
                  {tabEntry?.isDefault && <span className="project-config-tab-dot" role="img" aria-label="项目默认" title="项目默认" />}
                </button>;
              })}
            </div>
            <div className="project-config-body" role="tabpanel" id="project-config-panel" aria-labelledby={`project-config-tab-${id}`}>
              {poolRoute ? <div className="project-config-pool-route">该项目正在使用凭据池。项目级设置不会覆盖池成员。</div> : <>
                <div className="project-config-status">
                  {entry?.isDefault && <span className="project-config-badge"><i />项目默认 · 新会话生效</span>}
                  <span className="project-config-hint">留空的项使用 CLI 默认</span>
                </div>
                <label className="project-config-field"><span>模型</span>
                  <input maxLength={128} disabled={busy} value={draft.model} onChange={(event) => setAgentDraft(id, { model: event.target.value })} placeholder="例如 opus、gpt-5" />
                </label>
                <div className="project-config-field"><span>认证方式</span>
                  <AuthModeSwitch mode={draft.authMode} onChange={(next) => setAgentDraft(id, { authMode: next })} disabled={busy} />
                </div>
                {draft.authMode === "api_key" && <>
                  <label className="project-config-field"><span>Base URL</span>
                    <input disabled={busy} value={draft.baseUrl} onChange={(event) => setAgentDraft(id, { baseUrl: event.target.value })} placeholder="https://api.example.com/v1" />
                  </label>
                  <label className="project-config-field"><span>API Key</span>
                    <input type="password" autoComplete="new-password" disabled={busy} value={draft.apiKey} onChange={(event) => setAgentDraft(id, { apiKey: event.target.value })} placeholder={entry?.profileId ? "已配置，留空不修改" : "sk-…（必填）"} />
                  </label>
                </>}
                <div className="project-config-links">
                  {poolsFailed && <span className="project-config-links-failed">凭据池列表读取失败</span>}
                  {compatiblePools.length > 0 && <button type="button" className="project-config-link" onClick={() => setPoolPanelOpen((open) => !open)}>{poolPanelOpen ? "收起凭据池" : `使用凭据池（${compatiblePools.length} 个可用）`}</button>}
                  {entry?.isDefault && <button type="button" className="project-config-link" onClick={() => void clearDefault(id)}>改用 CLI 原有配置</button>}
                </div>
                {poolPanelOpen && compatiblePools.length > 0 && <div className="project-config-pool-row">
                  <select aria-label={`${activeEntry?.name ?? id} 凭据池`} value={poolSelections[id] || ""} disabled={saving} onChange={(event) => setPoolSelections((current) => ({ ...current, [id]: event.target.value }))}>
                    <option value="">选择凭据池</option>
                    {compatiblePools.map((pool) => <option key={pool.currentRevisionId} value={pool.currentRevisionId}>{pool.name}</option>)}
                  </select>
                  <button type="button" className="secondary" disabled={saving || !poolSelections[id]} onClick={() => void applyPool(id)}>应用</button>
                </div>}
              </>}
            </div>
            <footer className="project-config-dialog-footer">
              {poolRoute && <button type="button" className="secondary" disabled={saving} onClick={() => void clearDefault(id)}>解除凭据池</button>}
              <button type="button" className="secondary" disabled={saving} onClick={close}>关闭</button>
              {!poolRoute && <button type="button" className="primary" disabled={saving} onClick={() => void saveAgent(id)}>{busy ? "保存中" : "保存"}</button>}
            </footer>
          </>
          : <p className="project-config-loading">
            {catalog.error ? `工具目录读取失败：${catalog.error}` : catalog.loaded ? "该项目没有可配置的工具。" : "正在读取…"}
          </p>}
      </>}
    </section>
  </div>;
}

function ProjectConfigGlyph() {
  return <svg viewBox="0 0 24 24" aria-hidden="true"><path d="m12 3.5 8 3v5.6c0 4.3-3 7.8-8 9.4-5-1.6-8-5.1-8-9.4V6.5l8-3Z" /><path d="M9 11.2 11 13.2 15.2 9" /><path d="M12 6.5V19M8.5 18.5h7" /></svg>;
}
