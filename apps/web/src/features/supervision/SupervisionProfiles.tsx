import { useEffect, useRef, useState } from "react";
import { api } from "../../lib/api";

type Profile = { id: string; currentRevisionId: string; name: string; agentId: string; model: string; baseUrl?: string; authMode: string; env?: Record<string, string>; enabled: boolean; state: string; executionMode: string; targetId?: string };
const empty = () => ({ id: "", agentId: "claude-code", name: "", model: "", baseUrl: "", authMode: "cli_managed", apiKey: "", env: "{}" });

export function SupervisionProfiles({ runnerId, projectId, targetId, close, changed }: { runnerId: string; projectId: string; targetId?: string; close: () => void; changed: () => void }) {
  const [profiles, setProfiles] = useState<Profile[]>([]);
  const [form, setForm] = useState(empty);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const generation = useRef(0);
  const endpoint = `/api/runners/${encodeURIComponent(runnerId)}/supervision-profiles`;
  useEffect(() => {
    const current = ++generation.current;
    void api<Profile[]>(endpoint).then(value => { if (current === generation.current) setProfiles(value); }).catch(cause => { if (current === generation.current) setError(String(cause)); });
    return () => { generation.current++; };
  }, [endpoint]);
  async function act(work: () => Promise<unknown>) {
    const current = generation.current;
    setBusy(true); setError("");
    try {
      await work();
      const value = await api<Profile[]>(endpoint);
      if (current === generation.current) { setProfiles(value); setForm(empty()); changed(); }
    } catch (cause) { if (current === generation.current) setError(cause instanceof Error ? cause.message : String(cause)); }
    finally { if (current === generation.current) setBusy(false); }
  }
  return <dialog open className="supervision-profile-dialog" aria-label="托管认证档案">
    <header><h3>当前目标的托管认证档案</h3><button type="button" disabled={busy} onClick={close}>关闭</button></header>
    <p>默认使用 CLI 已有登录。这里的档案仅供任务托管，绑定当前项目的实际执行环境；API Key 经现有 Runner 的认证管道提供给本次执行。</p>
    {error && <p role="alert" className="supervision-error">{error}</p>}
    <div className="supervision-profile-list">{profiles.filter(profile => profile.executionMode === "target_isolated" && profile.targetId === targetId).map(profile => <article key={profile.id}>
      <strong>{profile.name}</strong><span>{profile.agentId} · {profile.model || "CLI 默认模型"} · {profile.state === "revoked" ? "已吊销" : profile.enabled ? "可选择" : "已停用"}</span>
      <div className="supervision-actions"><button type="button" disabled={busy || profile.state !== "active"} onClick={() => setForm({ id: profile.id, agentId: profile.agentId, name: profile.name, model: profile.model ?? "", baseUrl: profile.baseUrl ?? "", authMode: profile.authMode, apiKey: "", env: JSON.stringify(profile.env ?? {}, null, 2) })}>编辑新版本</button>
        <button type="button" disabled={busy || profile.state !== "active"} onClick={() => void act(() => api(`/api/supervision-profiles/${profile.id}/${profile.enabled ? "disable" : "enable"}`, { method: "POST" }))}>{profile.enabled ? "停用新选择" : "启用"}</button>
        <button type="button" disabled={busy || profile.state === "revoked"} onClick={() => void act(() => api(`/api/supervision-profile-revisions/${profile.currentRevisionId}/revoke`, { method: "POST" }))}>吊销此版本并停止相关运行</button></div>
    </article>)}</div>
    <form onSubmit={event => { event.preventDefault(); void act(async () => {
      const env: unknown = JSON.parse(form.env);
      if (!env || typeof env !== "object" || Array.isArray(env) || Object.values(env).some(value => typeof value !== "string")) throw Error("附加环境变量必须是字符串组成的 JSON 对象");
      const { id, apiKey, agentId, ...fields } = form;
      const body = { ...fields, ...(id ? {} : { agentId }), env, baseUrl: form.authMode === "api_key" ? form.baseUrl : "", ...(form.authMode === "api_key" && apiKey ? { apiKey } : {}) };
      await api(id ? `/api/supervision-profiles/${id}` : `${endpoint}?projectId=${encodeURIComponent(projectId)}`, { method: id ? "PATCH" : "POST", body: JSON.stringify(body) });
    }); }}>
      <h4>{form.id ? "保存为新版本，已有运行继续引用原版本" : "新增托管档案"}</h4>
      <label>CLI<select aria-label="CLI" disabled={busy || !!form.id} value={form.agentId} onChange={event => setForm({ ...empty(), agentId: event.target.value })}><option value="claude-code">Claude Code</option><option value="codex">Codex</option></select></label>
      <label>名称<input required maxLength={200} disabled={busy} value={form.name} onChange={event => setForm({ ...form, name: event.target.value })} /></label>
      <label>模型<input disabled={busy} value={form.model} onChange={event => setForm({ ...form, model: event.target.value })} placeholder="CLI 默认模型" /></label>
      <label>认证方式<select aria-label="认证方式" disabled={busy} value={form.authMode} onChange={event => setForm({ ...form, authMode: event.target.value, apiKey: "", baseUrl: "" })}><option value="cli_managed">目标 CLI 已有登录</option><option value="api_key">API Key</option></select></label>
      {form.authMode === "api_key" && <><label>服务地址<input disabled={busy} value={form.baseUrl} onChange={event => setForm({ ...form, baseUrl: event.target.value })} placeholder="使用工具默认地址" /></label><label>API Key<input type="password" autoComplete="new-password" required={!form.id} disabled={busy} value={form.apiKey} onChange={event => setForm({ ...form, apiKey: event.target.value })} placeholder={form.id ? "留空保留原凭据" : "输入 API Key"} /></label></>}
      <label>附加环境变量<textarea disabled={busy} value={form.env} onChange={event => setForm({ ...form, env: event.target.value })} /></label>
      <div className="supervision-actions"><button disabled={busy || !targetId}>{busy ? "保存中…" : "保存档案"}</button>{form.id && <button type="button" disabled={busy} onClick={() => setForm(empty())}>取消编辑</button>}</div>
    </form>
  </dialog>;
}
