import { FormEvent, useCallback, useEffect, useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
import { api } from "../lib/api";
import { ConfirmDialog } from "../components/ConfirmDialog";
import type { AgentID, AgentProfile, RunnerInfo } from "../lib/types";
import "../agent-profiles.css";

type ProfileAuthMode = "cli_managed";
type ProfileDraft = {
  id?: string;
  agentId: AgentID;
  name: string;
  model: string;
  authMode: ProfileAuthMode;
};

const emptyDraft = (): ProfileDraft => ({ agentId: "claude-code", name: "", model: "", authMode: "cli_managed" });
const draftForProfile = (profile: AgentProfile): ProfileDraft => ({ id: profile.id, agentId: profile.agentId, name: profile.name, model: profile.model || "", authMode: "cli_managed" });

// 停用/撤销的二次确认：仅记录「动作 + 目标档案」，实际执行在确认后由 runPending 完成。
type PendingConfirm = { kind: "disable" | "revoke"; profile: AgentProfile };

export default function AgentProfilesPage() {
  const navigate = useNavigate();
  const [runners, setRunners] = useState<RunnerInfo[]>([]);
  const [runnerID, setRunnerID] = useState("");
  const [profiles, setProfiles] = useState<AgentProfile[]>([]);
  const [draft, setDraft] = useState<ProfileDraft | null>(null);
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
      setProfiles(await api<AgentProfile[]>(`/api/runners/${runnerID}/agent-profiles`));
      setError("");
    } catch (cause) {
      setProfiles([]);
      setError(cause instanceof Error ? cause.message : "无法读取配置档案");
    } finally {
      setLoading(false);
    }
  }, [runnerID]);

  useEffect(() => { void refreshProfiles(); }, [refreshProfiles]);

  const currentRunner = useMemo(() => runners.find((item) => item.id === runnerID), [runnerID, runners]);
  const editableRunner = Boolean(currentRunner?.profileManagement);

  const save = async (event: FormEvent) => {
    event.preventDefault();
    if (!draft || !runnerID || saving) return;
    setSaving(true);
    setError("");
    setNotice("");
    const body: Record<string, unknown> = { name: draft.name, model: draft.model, authMode: draft.authMode };
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
		<header><div><h2>{currentRunner?.name || "Runner"}</h2><p>档案使用 CLI 原有登录，可为会话固定模型覆盖。</p></div><button className="primary" type="button" onClick={() => setDraft(emptyDraft())}>新建档案</button></header>
        {loading ? <p className="agent-profiles-loading">正在读取配置档案...</p>
          : profiles.length === 0 ? <div className="agent-profiles-empty"><h2>还没有配置档案</h2><p>新建一个档案后，可在新会话中选择它。</p></div>
            : <div className="agent-profiles-list">{profiles.map((profile) => <article key={profile.id} className={`agent-profile-row${profile.enabled ? "" : " disabled"}`}>
              <div className="agent-profile-name"><b>{profile.name}</b><span>{profile.agentId === "codex" ? "Codex" : "Claude Code"}</span></div>
              <div><small>模型</small><strong>{profile.model || "CLI 默认"}</strong></div>
              <div><small>认证</small><strong>{profile.authMode === "cli_managed" ? "CLI 登录" : "需要迁移"}</strong></div>
              <div><small>版本</small><strong>r{profile.revision}</strong></div>
              <div className="agent-profile-actions"><button type="button" className="secondary" disabled={!profile.enabled || profile.state !== "active" || profile.authMode !== "cli_managed" || Boolean(testingID)} onClick={() => void validate(profile)}>{testingID === profile.id ? "检测中" : "检测"}</button><button type="button" className="secondary" disabled={!profile.enabled || profile.state !== "active"} onClick={() => setDraft(draftForProfile(profile))}>编辑</button>{profile.enabled ? <button type="button" className="secondary" onClick={() => void disable(profile)}>停用</button> : <button type="button" className="secondary" disabled={profile.state !== "active"} onClick={() => void enable(profile)}>启用</button>}<button type="button" className="danger-text" disabled={profile.state !== "active"} onClick={() => void revoke(profile)}>撤销</button></div>
            </article>)}</div>}
      </section>}
    {draft && <div className="backdrop" role="dialog" aria-modal="true" onClick={(event) => { if (event.target === event.currentTarget && !saving) setDraft(null); }}><form className="modal agent-profile-editor" onSubmit={(event) => void save(event)}>
      <header><div><h2>{draft.id ? "编辑配置档案" : "新建配置档案"}</h2></div><button type="button" title="关闭" disabled={saving} onClick={() => setDraft(null)}>x</button></header>
      <div className="agent-profile-editor-body">
        {!draft.id && <label>工具<select value={draft.agentId} onChange={(event) => setDraft({ ...draft, agentId: event.target.value as AgentID })}><option value="claude-code">Claude Code</option><option value="codex">Codex</option></select></label>}
        <label>名称<input autoFocus required maxLength={80} value={draft.name} onChange={(event) => setDraft({ ...draft, name: event.target.value })} /></label>
        <label>认证方式<input value="CLI 登录" disabled /></label>
        <label>模型<input maxLength={128} value={draft.model} onChange={(event) => setDraft({ ...draft, model: event.target.value })} placeholder="留空使用 CLI 默认模型" /></label>
      </div>
      <footer><button type="button" className="secondary" disabled={saving} onClick={() => setDraft(null)}>取消</button><button className="primary" disabled={saving}>{saving ? "保存中" : "保存"}</button></footer>
    </form></div>}
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
