import { FormEvent, useCallback, useEffect, useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
import { api } from "../lib/api";
import { ConfirmDialog } from "../components/ConfirmDialog";
import type { AgentID, AgentProfile, CredentialPool, QuotaGroup, RunnerInfo } from "../lib/types";
import "../agent-profiles.css";

type ProfileAuthMode = "cli_managed" | "api_key";
type ProfileDraft = {
  id?: string;
  agentId: AgentID;
  name: string;
  model: string;
  authMode: ProfileAuthMode;
  baseUrl: string;
  apiKey: string;
};

const emptyDraft = (): ProfileDraft => ({ agentId: "claude-code", name: "", model: "", authMode: "cli_managed", baseUrl: "", apiKey: "" });
const draftForProfile = (profile: AgentProfile): ProfileDraft => ({ id: profile.id, agentId: profile.agentId, name: profile.name, model: profile.model || "", authMode: profile.authMode === "api_key" ? "api_key" : "cli_managed", baseUrl: profile.baseUrl || "", apiKey: "" });
type QuotaDraft = { name: string; scope: QuotaGroup["scope"]; scopeKey: string; rpmLimit: number; tpmLimit: number; maxConcurrency: number };
const emptyQuotaDraft = (): QuotaDraft => ({ name: "", scope: "credential", scopeKey: "", rpmLimit: 0, tpmLimit: 0, maxConcurrency: 1 });
type PoolDraft = { name: string; strategy: CredentialPool["strategy"]; projectMaxConcurrency: number; profileIds: string[] };
const emptyPoolDraft = (): PoolDraft => ({ name: "", strategy: "fair_queue", projectMaxConcurrency: 1, profileIds: [] });

// 停用/撤销的二次确认：仅记录「动作 + 目标档案」，实际执行在确认后由 runPending 完成。
type PendingConfirm = { kind: "disable" | "revoke"; profile: AgentProfile };

export default function AgentProfilesPage() {
  const navigate = useNavigate();
  const [runners, setRunners] = useState<RunnerInfo[]>([]);
  const [runnerID, setRunnerID] = useState("");
  const [profiles, setProfiles] = useState<AgentProfile[]>([]);
  const [pools, setPools] = useState<CredentialPool[]>([]);
  const [quotaGroups, setQuotaGroups] = useState<Record<string, QuotaGroup[]>>({});
  const [draft, setDraft] = useState<ProfileDraft | null>(null);
  const [quotaProfile, setQuotaProfile] = useState<AgentProfile | null>(null);
  const [quotaDraft, setQuotaDraft] = useState<QuotaDraft>(emptyQuotaDraft());
  const [editingQuotaGroup, setEditingQuotaGroup] = useState<QuotaGroup | null>(null);
  const [quotaAttachmentID, setQuotaAttachmentID] = useState("");
  const [poolDraft, setPoolDraft] = useState<PoolDraft | null>(null);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [testingID, setTestingID] = useState("");
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [pendingConfirm, setPendingConfirm] = useState<PendingConfirm | null>(null);
  const [confirming, setConfirming] = useState(false);

  useEffect(() => {
    void api<RunnerInfo[]>("/api/runners").then((items) => {
      setRunners(items);
      setRunnerID((current) => current || items.find((item) => item.environment !== "remote-linux")?.id || items[0]?.id || "");
    }).catch((cause) => { setError(cause instanceof Error ? cause.message : "无法读取 Runner"); setLoading(false); });
  }, []);

  const refreshProfiles = useCallback(async () => {
    if (!runnerID) return;
    setLoading(true);
    try {
      const nextProfiles = await api<AgentProfile[]>(`/api/runners/${runnerID}/agent-profiles`);
      setProfiles(nextProfiles);
      const results = await Promise.allSettled(nextProfiles.map(async (profile) => ({ profileID: profile.id, groups: await api<QuotaGroup[]>(`/api/agent-profiles/${profile.id}/quota-groups`) })));
      setQuotaGroups((previous) => {
        const next: Record<string, QuotaGroup[]> = {};
        for (const result of results) {
          if (result.status === "fulfilled") next[result.value.profileID] = result.value.groups;
        }
        for (const profile of nextProfiles) {
          if (!next[profile.id] && previous[profile.id]) next[profile.id] = previous[profile.id];
        }
        return next;
      });
      setError(results.some((result) => result.status === "rejected") ? "部分额度组暂时无法读取，请稍后刷新。" : "");
    } catch (cause) {
      setProfiles([]);
      setError(cause instanceof Error ? cause.message : "无法读取配置档案");
    } finally {
      setLoading(false);
    }
  }, [runnerID]);

  const refreshPools = useCallback(async () => {
    if (!runnerID) return;
    try { setPools(await api<CredentialPool[]>(`/api/runners/${runnerID}/credential-pools`)); } catch { setPools([]); }
  }, [runnerID]);

  useEffect(() => { void refreshProfiles(); }, [refreshProfiles]);
  useEffect(() => { void refreshPools(); }, [refreshPools]);

  const currentRunner = useMemo(() => runners.find((item) => item.id === runnerID), [runnerID, runners]);
  const availableQuotaGroups = useMemo(() => {
    const seen = new Set<string>();
    return Object.values(quotaGroups).flat().filter((group) => group.enabled && !seen.has(group.id) && seen.add(group.id));
  }, [quotaGroups]);
  const editableRunner = Boolean(currentRunner?.profileManagement);

  const save = async (event: FormEvent) => {
    event.preventDefault();
    if (!draft || !runnerID || saving) return;
    setSaving(true);
    setError("");
    setNotice("");
    const body: Record<string, unknown> = { name: draft.name, model: draft.model, authMode: draft.authMode };
    if (draft.authMode === "api_key") {
      body.baseUrl = draft.baseUrl;
      if (draft.apiKey) body.apiKey = draft.apiKey;
    }
    try {
      if (draft.id) {
        await api(`/api/agent-profiles/${draft.id}`, { method: "PATCH", body: JSON.stringify(body) });
      } else {
        await api(`/api/runners/${runnerID}/agent-profiles`, { method: "POST", body: JSON.stringify({ agentId: draft.agentId, ...body }) });
      }
      setDraft(null);
      await refreshProfiles();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "无法保存配置档案");
    } finally {
      setSaving(false);
    }
  };

  const saveQuota = async (event: FormEvent) => {
    event.preventDefault();
    if (!quotaProfile || saving) return;
    setSaving(true); setError(""); setNotice("");
    try {
      if (editingQuotaGroup) {
        await api(`/api/quota-groups/${editingQuotaGroup.id}`, { method: "PATCH", body: JSON.stringify({ name: quotaDraft.name, rpmLimit: quotaDraft.rpmLimit, tpmLimit: quotaDraft.tpmLimit, maxConcurrency: quotaDraft.maxConcurrency }) });
      } else {
        await api(`/api/agent-profiles/${quotaProfile.id}/quota-groups`, { method: "POST", body: JSON.stringify(quotaDraft) });
      }
      setEditingQuotaGroup(null); setQuotaDraft(emptyQuotaDraft()); await refreshProfiles();
    } catch (cause) { setError(cause instanceof Error ? cause.message : "无法保存额度组"); } finally { setSaving(false); }
  };

  const attachQuotaGroup = async () => {
    if (!quotaProfile || !quotaAttachmentID || saving) return;
    setSaving(true); setError(""); setNotice("");
    try {
      await api(`/api/quota-groups/${quotaAttachmentID}/profiles/${quotaProfile.id}`, { method: "POST" });
      setQuotaAttachmentID(""); await refreshProfiles();
    } catch (cause) { setError(cause instanceof Error ? cause.message : "无法关联额度组"); } finally { setSaving(false); }
  };

  const editQuotaGroup = (group: QuotaGroup) => {
    setEditingQuotaGroup(group);
    setQuotaDraft({ name: group.name, scope: group.scope, scopeKey: group.scopeKey, rpmLimit: group.rpmLimit, tpmLimit: group.tpmLimit, maxConcurrency: group.maxConcurrency });
  };

  const setQuotaGroupEnabled = async (group: QuotaGroup, enabled: boolean) => {
    if (saving) return;
    setSaving(true); setError(""); setNotice("");
    try {
      await api(`/api/quota-groups/${group.id}`, { method: "PATCH", body: JSON.stringify({ enabled }) });
      await refreshProfiles();
    } catch (cause) { setError(cause instanceof Error ? cause.message : "无法更新额度组"); } finally { setSaving(false); }
  };

  const savePool = async (event: FormEvent) => {
    event.preventDefault();
    if (!poolDraft || saving) return;
    setSaving(true); setError(""); setNotice("");
    try {
      await api(`/api/runners/${runnerID}/credential-pools`, { method: "POST", body: JSON.stringify(poolDraft) });
      setPoolDraft(null); await refreshPools();
    } catch (cause) { setError(cause instanceof Error ? cause.message : "无法创建凭据池"); } finally { setSaving(false); }
  };

  const validate = async (profile: AgentProfile) => {
    if (testingID) return;
    setTestingID(profile.id);
    setError("");
    setNotice("");
    try {
      const result = await api<Record<string, string>>(`/api/agent-profiles/${profile.id}/validate`, { method: "POST" });
      setNotice(`“${profile.name}”检测通过：${result.credential === "ok" ? "密钥可用，" : ""}${result.endpoint === "reachable" ? "endpoint 可连接" : "无需 endpoint"}`);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "配置检测失败");
    } finally {
      setTestingID("");
    }
  };

  const disable = (profile: AgentProfile) => {
    setPendingConfirm({ kind: "disable", profile });
  };

  const revoke = (profile: AgentProfile) => {
    setPendingConfirm({ kind: "revoke", profile });
  };

  const enable = async (profile: AgentProfile) => {
    try {
      await api(`/api/agent-profiles/${profile.id}/enable`, { method: "POST" });
      await refreshProfiles();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "无法启用配置档案");
    }
  };

  // 二次确认后真正执行停用/撤销，busy 状态由 confirming 控制。
  const runPending = async () => {
    if (!pendingConfirm || confirming) return;
    setConfirming(true);
    const { kind, profile } = pendingConfirm;
    try {
      if (kind === "disable") {
        await api(`/api/agent-profiles/${profile.id}/disable`, { method: "POST" });
      } else {
        await api(`/api/agent-profile-revisions/${profile.currentRevisionId}/revoke`, { method: "POST" });
      }
      setPendingConfirm(null);
      await refreshProfiles();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : kind === "disable" ? "无法停用配置档案" : "无法撤销配置档案版本");
    } finally {
      setConfirming(false);
    }
  };

  const closeConfirm = () => {
    if (confirming) return;
    setPendingConfirm(null);
  };

  return <main className="app-shell agent-profiles-page">
    <header className="agent-profiles-head">
      <button className="secondary" type="button" onClick={() => navigate(-1)}>返回</button>
      <div><h1>AI 配置管理</h1><p>高级管理视图：项目级配置请用「会话页 → AI 配置」。此处可停用 / 撤销版本。</p></div>
      <label>Runner<select value={runnerID} onChange={(event) => { setRunnerID(event.target.value); setDraft(null); setNotice(""); }} disabled={runners.length === 0}>{runners.map((runner) => <option key={runner.id} value={runner.id}>{runner.name}</option>)}</select></label>
    </header>
    {error && <div className="agent-profiles-error" role="alert">{error}</div>}
    {notice && <div className="agent-profiles-notice" role="status">{notice}</div>}
    {!editableRunner ? <section className="agent-profiles-empty"><h2>远端 Runner 由本机管理</h2><p>SSH 和 Windows 桌面版 WSL 暂只支持远端默认 CLI 配置，不接收本机档案或密钥。</p></section>
      : <section className="agent-profiles-workspace">
		<header><div><h2>{currentRunner?.name || "Runner"}</h2><p>档案可使用独立 CLI 登录或受管 API Key；密钥不会回显。</p></div><div className="agent-profiles-header-actions"><button className="secondary" type="button" onClick={() => setPoolDraft(emptyPoolDraft())}>新建凭据池</button><button className="primary" type="button" onClick={() => setDraft(emptyDraft())}>新建档案</button></div></header>
        {loading ? <p className="agent-profiles-loading">正在读取配置档案...</p>
          : profiles.length === 0 ? <div className="agent-profiles-empty"><h2>还没有配置档案</h2><p>新建一个档案后，可在新会话中选择它。</p></div>
            : <div className="agent-profiles-list">{profiles.map((profile) => <article key={profile.id} className={`agent-profile-row${profile.enabled ? "" : " disabled"}`}>
              <div className="agent-profile-name"><b>{profile.name}</b><span>{profile.agentId === "codex" ? "Codex" : "Claude Code"}</span></div>
              <div><small>模型</small><strong>{profile.model || "CLI 默认"}</strong></div>
              <div><small>认证</small><strong>{profile.authMode === "cli_managed" ? "CLI 登录" : "受管 API Key"}</strong></div>
              <div><small>版本</small><strong>r{profile.revision}</strong></div>
              <div className="agent-profile-actions"><button type="button" className="secondary" disabled={!profile.enabled || profile.state !== "active" || Boolean(testingID)} onClick={() => void validate(profile)}>{testingID === profile.id ? "检测中" : "检测"}</button><button type="button" className="secondary" disabled={!profile.enabled || profile.state !== "active"} onClick={() => { setQuotaProfile(profile); setQuotaDraft(emptyQuotaDraft()); setEditingQuotaGroup(null); setQuotaAttachmentID(""); }}>额度</button><button type="button" className="secondary" disabled={!profile.enabled || profile.state !== "active"} onClick={() => setDraft(draftForProfile(profile))}>编辑</button>{profile.enabled ? <button type="button" className="secondary" onClick={() => void disable(profile)}>停用</button> : <button type="button" className="secondary" disabled={profile.state !== "active"} onClick={() => void enable(profile)}>启用</button>}<button type="button" className="danger-text" disabled={profile.state !== "active"} onClick={() => void revoke(profile)}>撤销</button></div>
            </article>)}</div>}
        {pools.length > 0 && <section className="credential-pools-section"><header><div><h2>凭据池</h2><p>池成员固定到创建时的版本；只允许相同 Agent、协议、端点与模型。</p></div></header><div className="credential-pools-list">{pools.map((pool) => <article key={pool.id}><div><b>{pool.name}</b><small>{pool.strategy} · 项目并发 {pool.projectMaxConcurrency}</small></div><p>{pool.members.map((member) => `${member.name} (${member.agentId === "codex" ? "Codex" : "Claude"}${member.model ? ` · ${member.model}` : ""})`).join("，")}</p></article>)}</div></section>}
      </section>}
    {draft && <div className="backdrop" role="dialog" aria-modal="true" onClick={(event) => { if (event.target === event.currentTarget && !saving) setDraft(null); }}><form className="modal agent-profile-editor" onSubmit={(event) => void save(event)}>
      <header><div><h2>{draft.id ? "编辑配置档案" : "新建配置档案"}</h2></div><button type="button" title="关闭" disabled={saving} onClick={() => setDraft(null)}>x</button></header>
      <div className="agent-profile-editor-body">
        {!draft.id && <label>工具<select value={draft.agentId} onChange={(event) => setDraft({ ...draft, agentId: event.target.value as AgentID })}><option value="claude-code">Claude Code</option><option value="codex">Codex</option></select></label>}
        <label>名称<input autoFocus required maxLength={80} value={draft.name} onChange={(event) => setDraft({ ...draft, name: event.target.value })} /></label>
        <label>认证方式<select value={draft.authMode} onChange={(event) => setDraft({ ...draft, authMode: event.target.value as ProfileAuthMode, baseUrl: event.target.value === "api_key" ? draft.baseUrl : "" })}><option value="cli_managed">CLI 登录</option><option value="api_key">受管 API Key</option></select></label>
        <label>模型<input maxLength={128} value={draft.model} onChange={(event) => setDraft({ ...draft, model: event.target.value })} placeholder="留空使用 CLI 默认模型" /></label>
        {draft.authMode === "api_key" && <><label>Base URL<input required value={draft.baseUrl} onChange={(event) => setDraft({ ...draft, baseUrl: event.target.value })} placeholder="https://api.example.com/v1" /></label><label>API Key<input type="password" autoComplete="new-password" required={!draft.id} value={draft.apiKey} onChange={(event) => setDraft({ ...draft, apiKey: event.target.value })} placeholder={draft.id ? "留空保持现有 Key" : "输入 API Key"} /></label></>}
      </div>
      <footer><button type="button" className="secondary" disabled={saving} onClick={() => setDraft(null)}>取消</button><button className="primary" disabled={saving}>{saving ? "保存中" : "保存"}</button></footer>
    </form></div>}
    {quotaProfile && <div className="backdrop" role="dialog" aria-modal="true" onClick={(event) => { if (event.target === event.currentTarget && !saving) setQuotaProfile(null); }}><form className="modal agent-profile-editor" onSubmit={(event) => void saveQuota(event)}><header><div><h2>{quotaProfile.name} 的额度组</h2></div><button type="button" title="关闭" disabled={saving} onClick={() => setQuotaProfile(null)}>x</button></header><div className="agent-profile-editor-body">{(quotaGroups[quotaProfile.id] || []).map((group) => <div className="quota-group-summary" key={group.id}><div><b>{group.name}</b><span>{group.scope} · RPM {group.rpmLimit || "未设"} · TPM {group.tpmLimit || "未设"} · 并发 {group.maxConcurrency}</span></div><div className="quota-group-actions"><button type="button" className="secondary" disabled={saving} onClick={() => editQuotaGroup(group)}>编辑</button><button type="button" className="secondary" disabled={saving} onClick={() => void setQuotaGroupEnabled(group, !group.enabled)}>{group.enabled ? "停用" : "启用"}</button></div></div>)}<div className="quota-group-attach"><select value={quotaAttachmentID} disabled={saving} onChange={(event) => setQuotaAttachmentID(event.target.value)}><option value="">关联已有额度组</option>{availableQuotaGroups.filter((group) => !quotaGroups[quotaProfile.id]?.some((current) => current.id === group.id)).map((group) => <option key={group.id} value={group.id}>{group.name} · {group.scope} · {group.scopeKey}</option>)}</select><button type="button" className="secondary" disabled={saving || !quotaAttachmentID} onClick={() => void attachQuotaGroup()}>关联</button></div><label>名称<input required maxLength={80} value={quotaDraft.name} onChange={(event) => setQuotaDraft({ ...quotaDraft, name: event.target.value })} /></label>{!editingQuotaGroup && <><label>范围<select value={quotaDraft.scope} onChange={(event) => setQuotaDraft({ ...quotaDraft, scope: event.target.value as QuotaGroup["scope"] })}>{["credential", "organization", "workspace", "model", "ip"].map((scope) => <option key={scope} value={scope}>{scope}</option>)}</select></label><label>范围标识<input required maxLength={128} value={quotaDraft.scopeKey} onChange={(event) => setQuotaDraft({ ...quotaDraft, scopeKey: event.target.value })} placeholder="例如组织或中转账户 ID" /></label></>}<label>RPM<input type="number" min="0" value={quotaDraft.rpmLimit} onChange={(event) => setQuotaDraft({ ...quotaDraft, rpmLimit: Number(event.target.value) })} /></label><label>TPM<input type="number" min="0" value={quotaDraft.tpmLimit} onChange={(event) => setQuotaDraft({ ...quotaDraft, tpmLimit: Number(event.target.value) })} /></label><label>最大并发<input type="number" min="1" value={quotaDraft.maxConcurrency} onChange={(event) => setQuotaDraft({ ...quotaDraft, maxConcurrency: Number(event.target.value) })} /></label></div><footer><button type="button" className="secondary" disabled={saving} onClick={() => { setEditingQuotaGroup(null); setQuotaDraft(emptyQuotaDraft()); }}>新建额度组</button><button type="button" className="secondary" disabled={saving} onClick={() => setQuotaProfile(null)}>取消</button><button className="primary" disabled={saving}>{saving ? "保存中" : editingQuotaGroup ? "保存额度组" : "新增额度组"}</button></footer></form></div>}
    {poolDraft && <div className="backdrop" role="dialog" aria-modal="true" onClick={(event) => { if (event.target === event.currentTarget && !saving) setPoolDraft(null); }}><form className="modal agent-profile-editor" onSubmit={(event) => void savePool(event)}><header><div><h2>新建凭据池</h2></div><button type="button" title="关闭" disabled={saving} onClick={() => setPoolDraft(null)}>x</button></header><div className="agent-profile-editor-body"><label>名称<input autoFocus required maxLength={80} value={poolDraft.name} onChange={(event) => setPoolDraft({ ...poolDraft, name: event.target.value })} /></label><label>选择策略<select value={poolDraft.strategy} onChange={(event) => setPoolDraft({ ...poolDraft, strategy: event.target.value as PoolDraft["strategy"] })}><option value="fair_queue">公平轮询</option><option value="round_robin">轮询</option><option value="least_loaded">最小负载</option></select></label><label>单项目最大并发<input type="number" min="1" value={poolDraft.projectMaxConcurrency} onChange={(event) => setPoolDraft({ ...poolDraft, projectMaxConcurrency: Number(event.target.value) })} /></label><fieldset className="pool-member-picker"><legend>成员</legend>{profiles.filter((profile) => profile.enabled && profile.state === "active").map((profile) => <label key={profile.id}><input type="checkbox" checked={poolDraft.profileIds.includes(profile.id)} onChange={(event) => setPoolDraft({ ...poolDraft, profileIds: event.target.checked ? [...poolDraft.profileIds, profile.id] : poolDraft.profileIds.filter((id) => id !== profile.id) })} />{profile.name} · {profile.agentId} · {profile.model || "默认模型"}</label>)}</fieldset></div><footer><button type="button" className="secondary" disabled={saving} onClick={() => setPoolDraft(null)}>取消</button><button className="primary" disabled={saving || poolDraft.profileIds.length === 0}>{saving ? "保存中" : "创建凭据池"}</button></footer></form></div>}
    {pendingConfirm && <ConfirmDialog
      title={pendingConfirm.kind === "disable" ? "停用配置档案" : "撤销配置档案版本"}
      message={pendingConfirm.kind === "disable"
        ? <>停用“<b>{pendingConfirm.profile.name}</b>”？现有会话不受影响。</>
        : <>撤销“<b>{pendingConfirm.profile.name}</b>”的当前版本会停止使用该版本的运行。此操作不可恢复。</>}
      confirmLabel={pendingConfirm.kind === "disable" ? "停用" : "撤销"}
      danger={pendingConfirm.kind === "revoke"}
      busy={confirming}
      onConfirm={runPending}
      onCancel={closeConfirm}
    />}
  </main>;
}
