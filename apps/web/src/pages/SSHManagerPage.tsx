// SSH 连接管理页 — 从 App.tsx SSHConnectionManager 提取

import { useEffect, useState, useCallback, useRef } from "react";
import { toast } from "sonner";
import { useNavigate } from "react-router-dom";
import { useProjectContext } from "../stores/useProjectStore";
import { ConfirmDialog } from "../components/ConfirmDialog";
import type { SSHConnection, SSHPreflightResult, SSHProfile } from "../lib/types";
import DashboardPage from "./DashboardPage";

function SshIcon({ className = "ssh-icon" }: { className?: string }) {
  return <svg className={className} viewBox="0 0 24 24" aria-hidden="true"><rect x="3.5" y="4" width="17" height="16" rx="2" /><path d="m7.5 9 2.5 2.5L7.5 14M12.5 14h4" /></svg>;
}

function CloseIcon() {
  return <svg className="ssh-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="m7 7 10 10M17 7 7 17" /></svg>;
}

function CheckIcon() {
  return <svg className="ssh-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="m6.5 12 3.4 3.4 7.6-7.6" /></svg>;
}

function LinkIcon() {
  return <svg className="ssh-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="m9.5 14.5 5-5M7.5 16.5l-1 1a3 3 0 0 1-4.2-4.2l3-3a3 3 0 0 1 4.2 0M16.5 7.5l1-1a3 3 0 0 1 4.2 4.2l-3 3a3 3 0 0 1-4.2 0" /></svg>;
}

function TrashIcon() {
  return <svg className="ssh-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M4.5 7h15M9 7V4.5h6V7M7 7l.8 12.5h8.4L17 7M10 11v5M14 11v5" /></svg>;
}

function EditIcon() {
  return <svg className="ssh-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M5 19l1.5-4.5L16 5a1.9 1.9 0 0 1 2.7 0l.3.3a1.9 1.9 0 0 1 0 2.7l-9.5 9.5L5 19M11 6.5l6.5 6.5M5 19h4" /></svg>;
}

export default function SSHManagerPage() {
  const { api, setError } = useProjectContext();
  const navigate = useNavigate();
  const [connections, setConnections] = useState<SSHConnection[]>([]);
  const [profiles, setProfiles] = useState<string[]>([]);
  const [loading, setLoading] = useState(true);
  const [showForm, setShowForm] = useState(false);
  const [editingId, setEditingId] = useState<string | null>(null);
  const [testing, setTesting] = useState("");
  const [connecting, setConnecting] = useState("");
  const [form, setForm] = useState({ name: "", host: "", port: 22, user: "", privateKeyPath: "", authMethod: "key" as "key" | "password", password: "", rootPath: "", profileName: "" });
  const [preflight, setPreflight] = useState<SSHPreflightResult | null>(null);
  const [preflighting, setPreflighting] = useState(false);
  const [hostKeyConfirmed, setHostKeyConfirmed] = useState(false);
  const [localError, setLocalError] = useState("");
  const [resolvingProfile, setResolvingProfile] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<SSHConnection | null>(null);
  const [deleting, setDeleting] = useState(false);
  const profileRequestVersion = useRef(0);
  const resolvingCount = useRef(0);
  const preflightRequestVersion = useRef(0);
  const connectionsRequestVersion = useRef(0);
  const profilesRequestVersion = useRef(0);
  const mountedRef = useRef(true);
  useEffect(() => { mountedRef.current = true; return () => { mountedRef.current = false; }; }, []);
  const [saving, setSaving] = useState(false);

  const showError = (msg: string) => { setLocalError(msg); setError(msg); };
  const clearError = () => { setLocalError(""); setError(""); };

  const loadConnections = useCallback(async () => {
    const requestVersion = ++connectionsRequestVersion.current;
    try {
      const list = await api<any[]>("/api/ssh-connections");
      if (mountedRef.current && requestVersion === connectionsRequestVersion.current) setConnections(list);
    } catch { /* ignore */ }
    if (mountedRef.current && requestVersion === connectionsRequestVersion.current) setLoading(false);
  }, [api]);

  useEffect(() => { void loadConnections(); }, [loadConnections]);
  useEffect(() => {
    const requestVersion = ++profilesRequestVersion.current;
    void api<string[]>("/api/ssh-profiles")
      .then((next) => { if (mountedRef.current && requestVersion === profilesRequestVersion.current) setProfiles(next); })
      .catch(() => { if (mountedRef.current && requestVersion === profilesRequestVersion.current) setProfiles([]); });
  }, [api]);
  const updateForm = (next: typeof form) => {
    if (next.profileName !== form.profileName) profileRequestVersion.current++;
    preflightRequestVersion.current++;
    setForm(next);
    setPreflight(null);
    setHostKeyConfirmed(false);
    setPreflighting(false);
    clearError();
  };
  const selectProfile = async (profileName: string) => {
    const nextForm = profileName ? { ...form, profileName, host: "", port: 22, user: "", privateKeyPath: "", password: "", authMethod: "key" as const } : { ...form, profileName };
    updateForm(nextForm);
    if (!profileName) return;
    setResolvingProfile(true);
    resolvingCount.current++;
    const requestVersion = profileRequestVersion.current;
    try {
      const resolved = await api<SSHProfile>(`/api/ssh-profiles/${encodeURIComponent(profileName)}`);
      if (requestVersion === profileRequestVersion.current) {
        // 自动填充连接名称（如果用户尚未填写），合并为一次 setForm 避免中间状态
        setForm((current) => ({ ...current, ...resolved, name: current.name || profileName }));
      }
    } catch (cause) {
      if (requestVersion === profileRequestVersion.current) showError(cause instanceof Error ? cause.message : "读取 SSH Profile 失败");
    } finally {
      resolvingCount.current--;
      if (resolvingCount.current <= 0) { resolvingCount.current = 0; setResolvingProfile(false); }
    }
  };
  const runPreflight = async () => {
    clearError();
    // 前端校验：必填字段
    if (!form.name.trim()) { showError("请填写连接名称"); return; }
    if (!form.rootPath.trim()) { showError("请填写远端项目根路径"); return; }
    // Profile 模式下 host/user 由后端解析；手工模式下必须填写
    if (!form.profileName.trim()) {
      if (!form.host.trim()) { showError("请填写主机地址"); return; }
      if (!form.user.trim()) { showError("请填写用户名"); return; }
    } else if (resolvingProfile) {
      // Profile 正在解析中，等待完成后再预检
      showError("SSH Profile 正在解析中，请稍候"); return;
    }
    const requestVersion = ++preflightRequestVersion.current;
    setPreflighting(true); setPreflight(null); setHostKeyConfirmed(false);
    try {
      // 编辑模式下附带 connectionId，让后端用已保存的凭据补全预检
      // （密码不回显，预检时需沿用原密码才能真正连上远端验证）。
      const payload = editingId ? { ...form, connectionId: editingId } : form;
      const result = await api<SSHPreflightResult>("/api/ssh-connections/preflight", { method: "POST", body: JSON.stringify(payload) });
      if (requestVersion !== preflightRequestVersion.current) return;
      setPreflight(result);
      if (result.resolved) {
        setForm((current) => ({ ...current, ...result.resolved }));
      }
      if (!result.ok && result.error) {
        showError(result.error);
      }
    }
    catch (cause) { if (requestVersion === preflightRequestVersion.current) showError(cause instanceof Error ? cause.message : "连接预检失败"); }
    finally { if (requestVersion === preflightRequestVersion.current) setPreflighting(false); }
  };

  const handleTest = async (id: string) => {
    setTesting(id);
    try {
      const result = await api<{ ok: boolean; error?: string }>(`/api/ssh-connections/${id}/test`, { method: "POST" });
      if (result.ok) { clearError(); toast.success("连接成功！"); }
      else toast.error("连接失败：" + (result.error || "未知错误"));
    } catch (cause) { showError(cause instanceof Error ? cause.message : "测试连接失败"); }
    finally { setTesting(""); void loadConnections(); }
  };

  const handleConnect = async (id: string) => {
    setConnecting(id);
    try { await api(`/api/ssh-connections/${id}/connect`, { method: "POST" }); clearError(); }
    catch (cause) { showError(cause instanceof Error ? cause.message : "连接失败"); }
    finally { setConnecting(""); void loadConnections(); }
  };

  const handleDisconnect = async (id: string) => {
    setConnecting(id);
    try { await api(`/api/ssh-connections/${id}/disconnect`, { method: "POST" }); clearError(); }
    catch (cause) { showError(cause instanceof Error ? cause.message : "断开失败"); }
    finally { setConnecting(""); void loadConnections(); }
  };

  const handleDelete = (id: string) => {
    const conn = connections.find((item) => item.id === id);
    if (!conn || deleting) return;
    setDeleteTarget(conn);
  };

  const confirmDelete = async () => {
    if (!deleteTarget || deleting) return;
    setDeleting(true);
    try { await api(`/api/ssh-connections/${deleteTarget.id}`, { method: "DELETE" }); clearError(); setDeleteTarget(null); void loadConnections(); }
    catch (cause) { showError(cause instanceof Error ? cause.message : "删除失败"); }
    finally { setDeleting(false); }
  };

  const emptyForm = () => ({ name: "", host: "", port: 22, user: "", privateKeyPath: "", authMethod: "key" as "key" | "password", password: "", rootPath: "", profileName: "" });

  const startCreate = () => {
    setEditingId(null);
    updateForm(emptyForm());
    setShowForm(true);
  };

  const startEdit = async (conn: SSHConnection) => {
    clearError();
    setEditingId(conn.id);
    // 作废任何在途的预检/Profile 解析请求，避免旧请求返回后覆盖编辑表单。
    profileRequestVersion.current++;
    preflightRequestVersion.current++;
    // 编辑时回填已保存的配置。私钥路径（非内容）服务端会回显以便重新预检；
    // 密码不会回显，认证方式按保存的 authMethod 恢复，需要改密码时在此重新填写。
    setForm({
      name: conn.name || "",
      host: conn.host || "",
      port: conn.port || 22,
      user: conn.user || "",
      privateKeyPath: conn.authMethod === "key" ? (conn.privateKeyPath || "") : "",
      authMethod: conn.authMethod === "password" ? "password" : "key",
      password: "",
      rootPath: conn.rootPath || "",
      profileName: "",
    });
    setPreflight(null);
    setHostKeyConfirmed(false);
    setPreflighting(false);
    setShowForm(true);
  };

  const handleSave = async () => {
    if (!preflight?.ok || !preflight.hostKey || !hostKeyConfirmed) { showError("请先完成全部预检并确认主机指纹"); return; }
    setSaving(true);
    try {
      if (editingId) {
        await api(`/api/ssh-connections/${editingId}`, { method: "PUT", body: JSON.stringify({ ...form, hostKey: preflight.hostKey, connect: true }) });
      } else {
        await api("/api/ssh-connections", { method: "POST", body: JSON.stringify({ ...form, hostKey: preflight.hostKey, connect: true }) });
      }
      setShowForm(false); setEditingId(null); updateForm(emptyForm()); void loadConnections();
    } catch (cause) { showError(cause instanceof Error ? cause.message : "保存失败"); }
    finally { setSaving(false); }
  };

  const closeForm = () => {
    setShowForm(false);
    setEditingId(null);
    updateForm(emptyForm());
    setResolvingProfile(false);
    resolvingCount.current = 0;
  };
  const statusLabel = (status: string) => status === "connected" ? "已连接" : status === "error" ? "连接异常" : status === "disconnected" ? "未连接" : "未知状态";

  // "确认并保存"按钮的禁用原因提示
  const saveDisabledReason = saving ? "保存中..."
    : !preflight?.ok ? "请先点击「预检连接」并通过检查"
    : !preflight.hostKey ? "未获取到主机指纹，请重新预检"
    : !hostKeyConfirmed ? "请勾选确认主机指纹"
    : null;

  return <>
    <DashboardPage />
    <div className="backdrop ssh-manager-backdrop" role="dialog" aria-modal="true" aria-labelledby="ssh-manager-title">
      <section className="modal ssh-manager-dialog">
        <header><div className="ssh-dialog-heading"><span className="ssh-dialog-mark"><SshIcon /></span><div><h2 id="ssh-manager-title">SSH连接</h2><p>管理远程开发环境与项目目录</p></div></div><button className="ssh-dialog-close" type="button" title="关闭" aria-label="关闭" onClick={() => navigate("/")}><CloseIcon /></button></header>
        <div className="ssh-manager-toolbar"><span>{loading ? "正在同步连接状态" : `${connections.length} 个已保存连接`}</span><button className="primary ssh-add-button" type="button" onClick={startCreate}><SshIcon />添加连接</button></div>
        {localError && <div className="ssh-error" role="alert"><span>{localError}</span><button type="button" title="关闭提示" aria-label="关闭提示" onClick={clearError}><CloseIcon /></button></div>}
        <div className="ssh-manager-body">
          {loading ? <div className="ssh-empty-state"><span className="ssh-loading-indicator"></span><p>正在读取SSH连接…</p></div> : connections.length === 0 ? <div className="ssh-empty-state"><span className="ssh-empty-icon"><SshIcon /></span><h3>还没有远程连接</h3><p>添加一台服务器后，即可从其中加载项目。</p><button className="primary" type="button" onClick={startCreate}><SshIcon />添加SSH连接</button></div> : <div className="ssh-connection-list">{connections.map((conn) => <article className={`ssh-connection-card is-${conn.status || "unknown"}`} key={conn.id}><div className="ssh-connection-main"><span className="ssh-status"><i></i>{statusLabel(conn.status)}</span><h3>{conn.name}</h3><p>{conn.user}@{conn.host}:{conn.port}</p>{conn.errorMsg && <small>{conn.errorMsg}</small>}</div><div className="ssh-connection-actions"><button className="ssh-action-button" type="button" title="测试连接" aria-label={`测试连接 ${conn.name}`} disabled={testing === conn.id} onClick={() => void handleTest(conn.id)}><CheckIcon /></button>{conn.status === "connected" ? <button className="ssh-action-button" type="button" title="断开连接" aria-label={`断开连接 ${conn.name}`} disabled={connecting === conn.id} onClick={() => void handleDisconnect(conn.id)}><LinkIcon /></button> : <button className="ssh-action-button connect" type="button" title="连接" aria-label={`连接 ${conn.name}`} disabled={connecting === conn.id} onClick={() => void handleConnect(conn.id)}><LinkIcon /></button>}<button className="ssh-action-button" type="button" title="编辑连接" aria-label={`编辑连接 ${conn.name}`} onClick={() => void startEdit(conn)}><EditIcon /></button><button className="ssh-action-button danger" type="button" title="删除连接" aria-label={`删除连接 ${conn.name}`} onClick={() => void handleDelete(conn.id)}><TrashIcon /></button></div></article>)}</div>}
        </div>
        <footer><button className="secondary" type="button" onClick={() => navigate("/")}>关闭</button></footer>
      </section>
    </div>
    {showForm && <div className="backdrop ssh-form-backdrop" role="dialog" aria-modal="true" aria-labelledby="ssh-form-title"><section className="modal ssh-connection-dialog"><header><div className="ssh-dialog-heading"><span className="ssh-dialog-mark"><SshIcon /></span><div><h2 id="ssh-form-title">{editingId ? "编辑SSH连接" : "添加SSH连接"}</h2><p>{editingId ? "修改连接信息并重新完成安全预检" : "填写连接信息并完成安全预检"}</p></div></div><button className="ssh-dialog-close" type="button" title="关闭" aria-label="关闭" disabled={saving} onClick={closeForm}><CloseIcon /></button></header><form onSubmit={(event) => { event.preventDefault(); void handleSave(); }}><div className="ssh-form-progress" aria-label={editingId ? "编辑连接步骤" : "添加连接步骤"}><span className="active"><i>1</i>配置连接</span><span className={preflight ? "active" : ""}><i>2</i>预检环境</span><span className={preflight?.ok && hostKeyConfirmed ? "active" : ""}><i>3</i>保存连接</span></div>{localError && <div className="ssh-error" role="alert"><span>{localError}</span><button type="button" title="关闭提示" aria-label="关闭提示" onClick={clearError}><CloseIcon /></button></div>}<div className="ssh-form-body"><section className="ssh-form-section"><header><h3>连接方式</h3><p>可使用本机 SSH Profile 自动填充连接信息。</p></header><label className="ssh-field">SSH Profile<select value={form.profileName} onChange={(event) => void selectProfile(event.target.value)} disabled={resolvingProfile}><option value="">手工填写</option>{profiles.map((profile) => <option key={profile} value={profile}>{profile}</option>)}</select></label>{resolvingProfile && <p className="ssh-form-notice"><span className="ssh-loading-indicator"></span>正在解析 SSH Profile…</p>}</section><section className="ssh-form-section"><header><h3>服务器信息</h3><p>用于识别远程主机及其登录方式。</p></header><div className="ssh-fields"><label className="ssh-field">连接名称<input autoFocus type="text" value={form.name} onChange={(event) => updateForm({ ...form, name: event.target.value })} placeholder="开发服务器" /></label><label className="ssh-field">主机地址<input disabled={Boolean(form.profileName)} type="text" value={form.host} onChange={(event) => updateForm({ ...form, host: event.target.value })} placeholder="192.168.1.100" /></label><label className="ssh-field">端口<input disabled={Boolean(form.profileName)} type="number" value={form.port} onChange={(event) => updateForm({ ...form, port: Number(event.target.value) || 22 })} /></label><label className="ssh-field">用户名<input disabled={Boolean(form.profileName)} type="text" value={form.user} onChange={(event) => updateForm({ ...form, user: event.target.value })} placeholder="root" /></label><label className="ssh-field ssh-field-wide">认证方式<div className="ssh-auth-method"><label className={form.authMethod === "key" ? "active" : ""}><input type="radio" name="authMethod" value="key" checked={form.authMethod === "key"} onChange={() => updateForm({ ...form, authMethod: "key", password: "" })} disabled={Boolean(form.profileName)} />密钥认证</label><label className={form.authMethod === "password" ? "active" : ""}><input type="radio" name="authMethod" value="password" checked={form.authMethod === "password"} onChange={() => updateForm({ ...form, authMethod: "password", privateKeyPath: "" })} disabled={Boolean(form.profileName)} />密码认证</label></div></label>{form.authMethod === "password" ? <label className="ssh-field ssh-field-wide">密码<input disabled={Boolean(form.profileName)} type="password" value={form.password} onChange={(event) => updateForm({ ...form, password: event.target.value })} placeholder="输入 SSH 登录密码" autoComplete="new-password" /></label> : <label className="ssh-field ssh-field-wide">私钥路径<input disabled={Boolean(form.profileName)} type="text" value={form.privateKeyPath} onChange={(event) => updateForm({ ...form, privateKeyPath: event.target.value })} placeholder="例如 /home/user/.ssh/id_rsa" /><small>留空时使用 SSH Agent。</small></label>}</div></section><section className="ssh-form-section"><header><h3>项目目录</h3><p>该目录会作为远程项目浏览的起点。</p></header><label className="ssh-field"><span>远端根路径</span><input required type="text" value={form.rootPath} onChange={(event) => updateForm({ ...form, rootPath: event.target.value })} placeholder="/home/user/projects" /></label></section><section className="ssh-form-section ssh-preflight-section"><header><div><h3>连接预检</h3><p>检查 SSH、SFTP、Claude Code 与审批隧道是否可用。</p></div><button className="secondary ssh-preflight-button" type="button" disabled={preflighting || resolvingProfile} onClick={() => void runPreflight()}><CheckIcon />{preflighting ? "预检中" : "预检连接"}</button></header>{!preflight && !preflighting && <div className="ssh-preflight-placeholder"><CheckIcon /><span>完成连接信息后开始预检。</span></div>}{preflight && <div className={`ssh-preflight ${preflight.ok ? "valid" : "invalid"}`}><header><span><CheckIcon /></span><div><h4>{preflight.ok ? "连接预检通过" : "预检未通过"}</h4><p>{preflight.ok ? "远程环境已满足连接要求。" : preflight.error || "请检查连接信息后重新预检。"}</p></div></header><div className="ssh-preflight-checks"><span>SSH <b>{preflight.checks?.ssh ? "可用" : "不可用"}</b></span><span>SFTP <b>{preflight.checks?.sftp ? "可用" : "不可用"}</b></span><span>Claude Code <b>{preflight.checks?.claude ? "可用" : "待安装或登录"}</b></span><span>审批隧道 <b>{preflight.checks?.approvalTunnel ? "可用" : "不可用"}</b></span></div>{preflight.fingerprint && <label className="ssh-host-key"><input type="checkbox" checked={hostKeyConfirmed} onChange={(event) => setHostKeyConfirmed(event.target.checked)} /><span>我已核对并确认此主机指纹：<code>{preflight.fingerprint}</code></span></label>}</div>}</section></div><footer><button className="secondary" type="button" disabled={saving} onClick={closeForm}>取消</button><button className="primary" disabled={Boolean(saveDisabledReason)} title={saveDisabledReason ?? undefined}>{saving ? "保存中" : "确认并保存"}</button></footer></form></section></div>}
    {deleteTarget && <ConfirmDialog title="删除SSH连接" message={<>确定要删除此 SSH 连接配置 <b>{deleteTarget.name}</b> 吗？如有关联项目则无法删除。</>} confirmLabel="删除" danger busy={deleting} onConfirm={confirmDelete} onCancel={() => { if (!deleting) setDeleteTarget(null); }} />}
  </>;
}
