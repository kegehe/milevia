// 项目级 AI 配置 — 会话页头部"AI 配置"入口打开的弹窗。
// 以"当前项目"为第一视角，展示并编辑本项目 Claude / Codex 各自的 API 配置
// （model、base_url、认证方式 / Key）。底层复用 agent profile 的注入物理，
// 这里只把"档案"心智收敛为"项目配置"：普通用户不用理解 停用/撤销/版本/revision。
import { useCallback, useEffect, useState } from "react";
import { api } from "../../lib/api";
import type { AgentID } from "../../lib/types";
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
  claude?: AgentEntry | null;
  codex?: AgentEntry | null;
};
type CredentialPool = {
  currentRevisionId: string;
  name: string;
  members: { agentId: AgentID }[];
};

const AGENT_IDS: { id: AgentID; label: string; detail: string }[] = [
  { id: "claude-code", label: "Claude Code", detail: "Anthropic 官方 CLI" },
  { id: "codex", label: "Codex", detail: "OpenAI CLI" },
];

function AuthModeSwitch({ mode, onChange, disabled }: { mode: string; onChange: (next: "cli_managed" | "api_key") => void; disabled: boolean }) {
  return <div className="project-config-authmode">
    <button type="button" className={mode === "cli_managed" ? "active" : ""} disabled={disabled} onClick={() => onChange("cli_managed")}>使用 CLI 登录</button>
    <button type="button" className={mode === "api_key" ? "active" : ""} disabled={disabled} onClick={() => onChange("api_key")}>受管 API Key</button>
  </div>;
}

export function ProjectAiConfigDialog({ projectId, runnerID, close }: { projectId: string; runnerID: string; close: () => void }) {
  const [config, setConfig] = useState<AgentConfigView | null>(null);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [savingAgent, setSavingAgent] = useState<AgentID | null>(null);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [pools, setPools] = useState<CredentialPool[]>([]);
  const [poolSelections, setPoolSelections] = useState<Partial<Record<AgentID, string>>>({});
  // 编辑草稿：keyed by agentId
  const [drafts, setDrafts] = useState<Partial<Record<AgentID, { model: string; baseUrl: string; authMode: "cli_managed" | "api_key"; apiKey: string }>>>({});

  const refresh = useCallback(async () => {
    setLoading(true);
    try {
      const [nextConfig, nextPools] = await Promise.all([
        api<AgentConfigView>(`/api/projects/${projectId}/agent-config`),
        api<CredentialPool[]>(`/api/runners/${runnerID}/credential-pools`).catch(() => []),
      ]);
      setConfig(nextConfig);
      setPools(nextPools);
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
  const entryFor = (agentId: AgentID): AgentEntry | null | undefined => (agentId === "codex" ? config?.codex : config?.claude);
  const draftFor = (agentId: AgentID): { model: string; baseUrl: string; authMode: "cli_managed" | "api_key"; apiKey: string } => {
    const draft = drafts[agentId];
    const entry = entryFor(agentId);
    return {
      model: draft?.model ?? entry?.model ?? "",
      baseUrl: draft?.baseUrl ?? entry?.baseUrl ?? "",
      authMode: draft?.authMode ?? (entry?.authMode === "api_key" ? "api_key" : "cli_managed"),
      apiKey: draft?.apiKey ?? "",
    };
  };
  const setAgentDraft = (agentId: AgentID, patch: Partial<{ model: string; baseUrl: string; authMode: "cli_managed" | "api_key"; apiKey: string }>) => {
    setDrafts((current) => ({ ...current, [agentId]: { ...draftFor(agentId), ...patch } }));
  };

  // 认证方式从 CLI 登录切到 API Key 时，仅当本地还没配 Key 才允许继续（创建时必填）。
  const saveAgent = async (agentId: AgentID) => {
    if (saving) return;
    setSaving(true);
    setSavingAgent(agentId);
    setError("");
    setNotice("");
    const draft = draftFor(agentId);
    const entry = entryFor(agentId);
    // 未配置任何东西且原本也没有 profile：直接清除默认即可，无需新建。
    if (!entry?.profileId && !draft.model && !draft.baseUrl && draft.authMode === "cli_managed" && !draft.apiKey) {
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
    try {
      let profileId = entry?.profileId || "";
      const body: Record<string, unknown> = { model: draft.model, authMode: draft.authMode };
      let name = "项目配置";
      if (draft.authMode === "api_key") {
        body.baseUrl = draft.baseUrl;
        if (draft.apiKey) body.apiKey = draft.apiKey;
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
      setDrafts((current) => ({ ...current, [agentId]: { model: draft.model, baseUrl: draft.baseUrl, authMode: draft.authMode, apiKey: "" } }));
      setNotice(`已保存${agentId === "codex" ? " Codex" : " Claude Code"}配置，新会话将自动应用。`);
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
      setNotice(`已清空${agentId === "codex" ? " Codex" : " Claude Code"}配置，之后新会话使用 CLI 原有配置。`);
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
      setNotice(`已为 ${agentId === "codex" ? "Codex" : "Claude Code"} 应用凭据池。`);
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
        <div className="project-config-heading"><span className="project-config-mark"><ProjectConfigGlyph /></span><div><label>PROJECT AI CONFIG</label><h2 id="project-config-title">项目 AI 配置</h2></div></div>
        <button type="button" className="project-config-close" title="关闭" aria-label="关闭" disabled={saving} onClick={close}>×</button>
      </header>
      {!manageable ? <div className="project-config-remote">
        <p>该项目运行在<strong>远端 Runner</strong>（SSH 或 Windows 桌面版 WSL）。远端默认使用其所在机器上已登录的 CLI 配置，本机暂不能为它注入 API Key / base_url。</p>
        <footer><button className="secondary" onClick={close}>关闭</button></footer>
      </div>
      : <>
        <div className="project-config-intro">配置本项目新会话要使用的 Claude / Codex API、base URL 与模型。留空表示使用 CLI 默认。受管 Key 仅本地加密存储并按需注入执行进程。</div>
        {error && <div className="project-config-error" role="alert">{error}</div>}
        {notice && <div className="project-config-notice" role="status">{notice}</div>}
        {loading && !config ? <p className="project-config-loading">正在读取…</p>
          : <div className="project-config-agents">
            {AGENT_IDS.map(({ id, label, detail }) => {
              const entry = entryFor(id);
              const draft = draftFor(id);
              const busy = saving && savingAgent === id;
              const poolRoute = entry?.mode === "pool";
              const compatiblePools = pools.filter((pool) => pool.members.some((member) => member.agentId === id));
              return <article key={id} className={`project-config-agent${entry?.isDefault ? " is-default" : ""}`}>
                <header><div><b>{label}</b><small>{detail}{entry?.isDefault ? " · 项目默认" : ""}</small></div></header>
                {poolRoute ? <div className="project-config-pool-route">该项目正在使用凭据池。项目级设置不会覆盖池成员。</div> : <>
                <div className="project-config-fields">
                  <label className="project-config-field"><span>模型 <small>留空使用 CLI 默认</small></span><input maxLength={128} value={draft.model} onChange={(event) => setAgentDraft(id, { model: event.target.value })} placeholder="例如 opus、gpt-5" /></label>
                  <label className="project-config-field"><span>认证方式</span><AuthModeSwitch mode={draft.authMode} onChange={(next) => setAgentDraft(id, { authMode: next })} disabled={busy} /></label>
                  {draft.authMode === "api_key" && <>
                    <label className="project-config-field"><span>Base URL <small>可选</small></span><input value={draft.baseUrl} onChange={(event) => setAgentDraft(id, { baseUrl: event.target.value })} placeholder="https://api.example.com/v1" /></label>
                    <label className="project-config-field"><span>API Key <small>{entry?.profileId ? "留空保持现有 Key" : "必填"}</small></span><input type="password" autoComplete="new-password" value={draft.apiKey} onChange={(event) => setAgentDraft(id, { apiKey: event.target.value })} placeholder={entry?.profileId ? "••••••••（已配置，留空不修改）" : "sk-…"} /></label>
                  </>}
                </div>
                </>}
                <footer>{!poolRoute && <><select className="project-config-pool-select" aria-label={`${label} 凭据池`} value={poolSelections[id] || ""} disabled={saving || compatiblePools.length === 0} onChange={(event) => setPoolSelections((current) => ({ ...current, [id]: event.target.value }))}><option value="">使用凭据池</option>{compatiblePools.map((pool) => <option key={pool.currentRevisionId} value={pool.currentRevisionId}>{pool.name}</option>)}</select><button className="secondary" type="button" disabled={saving || !poolSelections[id]} onClick={() => void applyPool(id)}>应用池</button><button className="primary" type="button" disabled={saving} onClick={() => void saveAgent(id)}>{busy ? "保存中" : "保存"}</button></>}{entry?.isDefault && <button className="secondary" type="button" disabled={saving} onClick={() => void clearDefault(id)}>{poolRoute ? "解除凭据池" : "改用 CLI"}</button>}</footer>
              </article>;
            })}
          </div>}
        <footer className="project-config-dialog-footer"><button className="secondary" disabled={saving} onClick={close}>关闭</button></footer>
      </>}
    </section>
  </div>;
}

function ProjectConfigGlyph() {
  return <svg viewBox="0 0 24 24" aria-hidden="true"><path d="m12 3.5 8 3v5.6c0 4.3-3 7.8-8 9.4-5-1.6-8-5.1-8-9.4V6.5l8-3Z" /><path d="M9 11.2 11 13.2 15.2 9" /><path d="M12 6.5V19M8.5 18.5h7" /></svg>;
}
