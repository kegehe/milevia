// SSH 连接管理页 — 从 App.tsx SSHConnectionManager 提取

import { useEffect, useState, useCallback, useRef } from "react";
import { useNavigate } from "react-router-dom";
import { useProjectContext } from "../stores/useProjectStore";
import type { SSHPreflightResult, SSHProfile } from "../lib/types";

export default function SSHManagerPage() {
  const { api, setError } = useProjectContext();
  const navigate = useNavigate();
  const [connections, setConnections] = useState<any[]>([]);
  const [profiles, setProfiles] = useState<string[]>([]);
  const [loading, setLoading] = useState(true);
  const [showForm, setShowForm] = useState(false);
  const [testing, setTesting] = useState("");
  const [connecting, setConnecting] = useState("");
  const [form, setForm] = useState({ name: "", host: "", port: 22, user: "", privateKeyPath: "", rootPath: "", profileName: "" });
  const [preflight, setPreflight] = useState<SSHPreflightResult | null>(null);
  const [preflighting, setPreflighting] = useState(false);
  const [hostKeyConfirmed, setHostKeyConfirmed] = useState(false);
  const [localError, setLocalError] = useState("");
  const [resolvingProfile, setResolvingProfile] = useState(false);
  const profileRequestVersion = useRef(0);
  const resolvingCount = useRef(0);
  const preflightRequestVersion = useRef(0);
  const mountedRef = useRef(true);
  useEffect(() => { mountedRef.current = true; return () => { mountedRef.current = false; }; }, []);
  const [saving, setSaving] = useState(false);

  const showError = (msg: string) => { setLocalError(msg); setError(msg); };
  const clearError = () => { setLocalError(""); setError(""); };

  const loadConnections = useCallback(async () => {
    try { const list = await api<any[]>("/api/ssh-connections"); if (mountedRef.current) setConnections(list); } catch { /* ignore */ }
    if (mountedRef.current) setLoading(false);
  }, [api]);

  useEffect(() => { void loadConnections(); }, [loadConnections]);
  useEffect(() => { void api<string[]>("/api/ssh-profiles").then(setProfiles).catch(() => setProfiles([])); }, [api]);
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
    const nextForm = profileName ? { ...form, profileName, host: "", port: 22, user: "", privateKeyPath: "" } : { ...form, profileName };
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
      const result = await api<SSHPreflightResult>("/api/ssh-connections/preflight", { method: "POST", body: JSON.stringify(form) });
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
      if (result.ok) { clearError(); alert("连接成功！"); }
      else alert("连接失败：" + (result.error || "未知错误"));
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

  const handleDelete = async (id: string) => {
    if (!confirm("确定要删除此 SSH 连接配置吗？如有关联项目则无法删除。")) return;
    try { await api(`/api/ssh-connections/${id}`, { method: "DELETE" }); clearError(); void loadConnections(); }
    catch (cause) { showError(cause instanceof Error ? cause.message : "删除失败"); }
  };

  const handleSave = async () => {
    if (!preflight?.ok || !preflight.hostKey || !hostKeyConfirmed) { showError("请先完成全部预检并确认主机指纹"); return; }
    setSaving(true);
    try {
      await api("/api/ssh-connections", { method: "POST", body: JSON.stringify({ ...form, hostKey: preflight.hostKey, connect: true }) });
      setShowForm(false); updateForm({ name: "", host: "", port: 22, user: "", privateKeyPath: "", rootPath: "", profileName: "" }); void loadConnections();
    } catch (cause) { showError(cause instanceof Error ? cause.message : "保存失败"); }
    finally { setSaving(false); }
  };

  const statusIcon = (s: string) => s === "connected" ? "🟢" : s === "error" ? "🔴" : s === "disconnected" ? "⚫" : "⚪";

  // "确认并保存"按钮的禁用原因提示
  const saveDisabledReason = saving ? "保存中..."
    : !preflight?.ok ? "请先点击「预检连接」并通过检查"
    : !hostKeyConfirmed ? "请勾选确认主机指纹"
    : null;

  return <main className="app-shell">
    <div className="backdrop" role="dialog" aria-modal="true">
      <section className="modal">
        <header><div><label>SSH CONNECTIONS</label><h2>SSH 连接管理</h2></div><button title="关闭" onClick={() => navigate("/")}>x</button></header>
        {localError && <div style={{ padding: "0.5rem 0.75rem", margin: "0.5rem 0", background: "var(--danger-bg, #fef2f2)", color: "var(--danger, #d14233)", borderRadius: "6px", fontSize: "13px", display: "flex", justifyContent: "space-between", alignItems: "center" }}><span>{localError}</span><button style={{ background: "none", border: "none", color: "inherit", cursor: "pointer", fontSize: "16px", padding: "0 4px" }} onClick={clearError}>x</button></div>}
        {showForm && <div className="ssh-form">
          <label>SSH Profile<select value={form.profileName} onChange={(e) => void selectProfile(e.target.value)} disabled={resolvingProfile}><option value="">手工填写</option>{profiles.map((profile) => <option key={profile} value={profile}>{profile}</option>)}</select></label>
          {resolvingProfile && <p className="permission-confirmation" style={{ color: "var(--accent)" }}>正在解析 SSH Profile 配置...</p>}
          <p className="permission-confirmation">选择 Profile 时，系统会读取本机 OpenSSH 配置并在预检中解析连接参数。</p>
          <div className="ssh-fields">
            <label>连接名称<input type="text" value={form.name} onChange={(e) => updateForm({ ...form, name: e.target.value })} placeholder="开发服务器" /></label>
            <label>主机地址<input disabled={Boolean(form.profileName)} type="text" value={form.host} onChange={(e) => updateForm({ ...form, host: e.target.value })} placeholder="192.168.1.100" /></label>
            <label>端口<input disabled={Boolean(form.profileName)} type="number" value={form.port} onChange={(e) => updateForm({ ...form, port: Number(e.target.value) || 22 })} /></label>
            <label>用户名<input disabled={Boolean(form.profileName)} type="text" value={form.user} onChange={(e) => updateForm({ ...form, user: e.target.value })} placeholder="root" /></label>
            <label className="ssh-field-wide">私钥路径<input disabled={Boolean(form.profileName)} type="text" value={form.privateKeyPath} onChange={(e) => updateForm({ ...form, privateKeyPath: e.target.value })} placeholder="例如 /home/user/.ssh/id_rsa" /><small>留空时使用 SSH Agent。</small></label>
            <label className="ssh-field-wide">根路径<input required type="text" value={form.rootPath} onChange={(e) => updateForm({ ...form, rootPath: e.target.value })} placeholder="/home/user/projects" /></label>
          </div>
          <div className="ssh-form-actions">
            <button className="secondary" disabled={preflighting || resolvingProfile} onClick={() => void runPreflight()}>{preflighting ? "预检中..." : "预检连接"}</button>
            <button className="secondary" onClick={() => { setShowForm(false); updateForm({ name: "", host: "", port: 22, user: "", privateKeyPath: "", rootPath: "", profileName: "" }); setResolvingProfile(false); resolvingCount.current = 0; }}>取消</button>
            <button className="primary" disabled={Boolean(saveDisabledReason)} onClick={handleSave} title={saveDisabledReason ?? undefined}>{saving ? "保存中" : "确认并保存"}</button>
          </div>
          {!preflight && !preflighting && <p className="permission-confirmation" style={{ marginTop: "0.5rem" }}>请先点击「预检连接」验证远端服务器可达性，通过后确认主机指纹即可保存。</p>}
          {preflight && <div className={`ssh-preflight ${preflight.ok ? "valid" : "invalid"}`}><b>{preflight.ok ? "连接预检通过" : "预检未通过"}</b><span>SSH {preflight.checks?.ssh ? "可用" : "不可用"} · SFTP {preflight.checks?.sftp ? "可用" : "不可用"} · Claude {preflight.checks?.claude ? "可用" : "待安装或登录"} · 审批隧道 {preflight.checks?.approvalTunnel ? "可用" : "不可用"}</span>{preflight.fingerprint && <label><input type="checkbox" checked={hostKeyConfirmed} onChange={(e) => setHostKeyConfirmed(e.target.checked)} />我已核对并确认此主机指纹：<code>{preflight.fingerprint}</code></label>}{preflight.error && <span>{preflight.error}</span>}</div>}
        </div>}
        {loading ? <p className="permission-confirmation">加载中...</p> : connections.length === 0 ? <p className="permission-confirmation">暂无 SSH 连接配置。<br /><button className="primary" onClick={() => setShowForm(true)} style={{ marginTop: "0.5rem" }}>添加 SSH 连接</button></p> : <>
          {connections.map((conn) => (
            <div key={conn.id} style={{ padding: "0.75rem", marginBottom: "0.5rem", background: "var(--surface)", borderRadius: "8px", display: "flex", alignItems: "center", justifyContent: "space-between" }}>
              <div><b>{statusIcon(conn.status)} {conn.name}</b><small style={{ display: "block", color: "var(--muted)" }}>{conn.user}@{conn.host}:{conn.port}</small>{conn.errorMsg && <small style={{ display: "block", color: "var(--danger)" }}>{conn.errorMsg}</small>}</div>
              <div style={{ display: "flex", gap: "0.25rem", flexShrink: 0 }}>
                <button className="secondary" disabled={testing === conn.id} onClick={() => handleTest(conn.id)}>{testing === conn.id ? "测试中" : "测试"}</button>
                {conn.status === "connected" ? <button className="secondary" disabled={connecting === conn.id} onClick={() => handleDisconnect(conn.id)}>断开</button> : <button className="secondary" disabled={connecting === conn.id} onClick={() => handleConnect(conn.id)}>{connecting === conn.id ? "连接中" : "连接"}</button>}
                <button className="secondary danger" onClick={() => handleDelete(conn.id)}>删除</button>
              </div>
            </div>))}
          <button className="primary" onClick={() => setShowForm(true)} style={{ marginTop: "0.5rem" }}>+ 添加 SSH 连接</button>
        </>}
        <footer><button className="secondary" onClick={() => navigate("/")}>关闭</button></footer>
      </section>
    </div>
  </main>;
}
