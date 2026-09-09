import { useEffect, useMemo, useState, type CSSProperties } from "react";
import { useNavigate } from "react-router-dom";
import { invoke } from "@tauri-apps/api/core";
import { toast } from "sonner";
import { useProjectContext } from "../stores/useProjectStore";
import { useUIPreferences, type AppPreferences } from "../stores/useUIPreferences";
import { CODE_FONT_SIZE_BOUNDS, CODE_FONT_SIZE_STORAGE_KEY, readCodeFontSize, setCodeFontSize } from "../features/files/useCodeFontSize";
import { countStoredConversationDrafts } from "../lib/conversation-draft";
import { PROJECT_ORDER_STORAGE_KEY, resetProjectOrder } from "../lib/project-order";
import { isDesktop } from "../lib/runtime";
import { api, apiWithTimeout } from "../lib/api";
import "./settings.css";

type UpdaterStatus = {
  appVersion: string;
  status: "checking" | "complete" | "failed";
  update: { currentVersion: string; version: string; notes?: string | null } | null;
  error?: string | null;
};

type InstallUpdateResult = { installed: boolean };
type StorageUsage = {
  databaseBytes: number;
  walBytes: number;
  shmBytes: number;
  thinkingTokenEvents: number;
  thinkingTokenBytes: number;
  tables: Array<{ name: string; rows: number; payloadBytes?: number }>;
};

function formatBytes(value: number): string {
  if (!Number.isFinite(value) || value < 1024) return `${Math.max(0, Math.round(value || 0))} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let size = value;
  let unit = -1;
  do { size /= 1024; unit += 1; } while (size >= 1024 && unit < units.length - 1);
  return `${size.toFixed(size >= 10 ? 1 : 2)} ${units[unit]}`;
}

function tableBytes(storage: StorageUsage, name: string): number {
  return storage.tables.find((table) => table.name === name)?.payloadBytes ?? 0;
}

function BackIcon() {
  return <svg viewBox="0 0 24 24" aria-hidden="true"><path d="m14.5 5.5-6.5 6.5 6.5 6.5M8.5 12h8" /></svg>;
}

function SettingsIcon() {
  return <svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="2.9" /><path d="M20.17 10.56A8.3 8.3 0 0 1 20.17 13.44L17.51 12.97A5.6 5.6 0 0 1 16.59 15.21L18.80 16.76A8.3 8.3 0 0 1 16.76 18.80L15.21 16.59A5.6 5.6 0 0 1 12.97 17.51L13.44 20.17A8.3 8.3 0 0 1 10.56 20.17L11.03 17.51A5.6 5.6 0 0 1 8.79 16.59L7.24 18.80A8.3 8.3 0 0 1 5.20 16.76L7.41 15.21A5.6 5.6 0 0 1 6.49 12.97L3.83 13.44A8.3 8.3 0 0 1 3.83 10.56L6.49 11.03A5.6 5.6 0 0 1 7.41 8.79L5.20 7.24A8.3 8.3 0 0 1 7.24 5.20L8.79 7.41A5.6 5.6 0 0 1 11.03 6.49L10.56 3.83A8.3 8.3 0 0 1 13.44 3.83L12.97 6.49A5.6 5.6 0 0 1 15.21 7.41L16.76 5.20A8.3 8.3 0 0 1 18.80 7.24L16.59 8.79A5.6 5.6 0 0 1 17.51 11.03L20.17 10.56Z" /></svg>;
}

function Toggle({ checked, disabled, onChange, label }: { checked: boolean; disabled?: boolean; onChange: (checked: boolean) => void; label: string }) {
  return <label className="settings-toggle" title={label}><input type="checkbox" checked={checked} disabled={disabled} onChange={(event) => onChange(event.target.checked)} /><span aria-hidden="true" /></label>;
}

export default function SettingsPage() {
  const navigate = useNavigate();
  const { clearConversationDrafts } = useProjectContext();
  const {
    appPreferences,
    appPreferencesLoading,
    appPreferencesError,
    updateAppPreferences,
    localPreferences,
    updateLocalPreferences,
    resetLocalPreferences,
    notificationPermission,
    requestSystemNotificationPermission,
  } = useUIPreferences();
  const [fontSize, setFontSize] = useState(readCodeFontSize);
  const fontSizeProgress = `${((fontSize - CODE_FONT_SIZE_BOUNDS.min) / (CODE_FONT_SIZE_BOUNDS.max - CODE_FONT_SIZE_BOUNDS.min)) * 100}%`;
  const [pendingFullControl, setPendingFullControl] = useState<Partial<AppPreferences> | null>(null);
  const [clearDraftsOpen, setClearDraftsOpen] = useState(false);
  const [resetLocalOpen, setResetLocalOpen] = useState(false);
  const [draftVersion, setDraftVersion] = useState(0);
  const [updaterStatus, setUpdaterStatus] = useState<UpdaterStatus | null>(null);
  const [checkingUpdate, setCheckingUpdate] = useState(false);
  const [installingUpdate, setInstallingUpdate] = useState(false);
  const [openingDataDirectory, setOpeningDataDirectory] = useState(false);
  const [updaterError, setUpdaterError] = useState<string | null>(null);
  const [storage, setStorage] = useState<StorageUsage | null>(null);
  const [storageLoading, setStorageLoading] = useState(false);
  const [storageCleaning, setStorageCleaning] = useState(false);
  const [storageConfirmOpen, setStorageConfirmOpen] = useState(false);
  const draftCount = useMemo(() => countStoredConversationDrafts(localPreferences.draftRetentionDays), [draftVersion, localPreferences.draftRetentionDays]);

  const loadStorage = async () => {
    if (!isDesktop()) return;
    setStorageLoading(true);
    try { setStorage(await apiWithTimeout<StorageUsage>("/api/system/storage", undefined, 1, 60_000)); }
    catch (cause) { toast.error(cause instanceof Error ? cause.message : "无法读取数据占用"); }
    finally { setStorageLoading(false); }
  };

  const cleanupStorage = async () => {
    setStorageConfirmOpen(false);
    setStorageCleaning(true);
    try {
      const result = await apiWithTimeout<{ after: StorageUsage; freedBytes: number }>("/api/system/storage/cleanup", { method: "POST", body: "{}" }, 0, 120_000);
      setStorage(result.after);
      toast.success(`已清理思考事件，释放 ${formatBytes(result.freedBytes)}`);
    } catch (cause) { toast.error(cause instanceof Error ? cause.message : "清理失败"); }
    finally { setStorageCleaning(false); }
  };

  useEffect(() => { void loadStorage(); }, []);

  useEffect(() => {
    if (!isDesktop()) return;
    let cancelled = false;
    let timer: number | undefined;
    const load = () => {
      invoke<UpdaterStatus>("get_updater_status").then((status) => {
        if (cancelled) return;
        setUpdaterStatus(status);
        if (status.status === "checking") timer = window.setTimeout(load, 1_000);
      }).catch(() => {
        if (!cancelled) setUpdaterStatus(null);
      });
    };
    load();
    return () => { cancelled = true; if (timer !== undefined) window.clearTimeout(timer); };
  }, []);

  const saveAppPreferences = async (patch: Partial<AppPreferences>) => {
    try {
      await updateAppPreferences(patch);
      toast.success("默认设置已保存");
    } catch (cause) {
      toast.error(cause instanceof Error ? cause.message : "无法保存默认设置");
    }
  };

  const choosePermission = (patch: Partial<AppPreferences>) => {
    if (patch.claudePermissionMode === "full_control" || patch.codexPermissionMode === "full_control") {
      setPendingFullControl(patch);
      return;
    }
    void saveAppPreferences(patch);
  };

  const toggleSystemNotifications = async (enabled: boolean) => {
    if (!enabled) {
      updateLocalPreferences({ systemNotificationsEnabled: false });
      return;
    }
    const permission = await requestSystemNotificationPermission();
    if (permission === "granted") return;
    toast.error(permission === "denied" ? "系统通知已被拒绝，请在系统或 WebView 设置中允许。" : "当前环境不支持系统通知。");
  };

  const resetLocal = () => {
    try { window.localStorage.removeItem(CODE_FONT_SIZE_STORAGE_KEY); } catch { /* reset current view below */ }
    resetProjectOrder();
    resetLocalPreferences();
    setFontSize(CODE_FONT_SIZE_BOUNDS.default);
    setResetLocalOpen(false);
    toast.success("本地界面偏好已恢复默认值");
  };

  const installUpdate = async () => {
    setInstallingUpdate(true);
    try {
      const result = await invoke<InstallUpdateResult>("install_update");
      // 正常安装会立即重启；若更新源在检查后撤回版本，命令会无操作返回。
      if (!result.installed) {
        const status = await invoke<UpdaterStatus>("get_updater_status");
        setUpdaterStatus(status);
        toast.info("更新已不可用，当前已是最新版本。");
      }
    } catch {
      toast.error("无法安装更新，请稍后重试。");
    } finally {
      setInstallingUpdate(false);
    }
  };

  const checkForUpdate = async () => {
    setCheckingUpdate(true);
    setUpdaterError(null);
    try {
      const status = await invoke<UpdaterStatus>("check_for_update_now");
      setUpdaterStatus(status);
      if (status.status === "failed") {
        const message = status.error || "无法检查更新，请稍后重试。";
        setUpdaterError(message);
        toast.error(message);
      } else {
        toast.success(status.update ? `发现新版本 v${status.update.version}` : "当前已是最新版本");
      }
    } catch (cause) {
      const message = cause instanceof Error ? cause.message : "无法检查更新，请稍后重试。";
      setUpdaterError(message);
      toast.error(message);
    } finally {
      setCheckingUpdate(false);
    }
  };

  const openDataDirectory = async () => {
    setOpeningDataDirectory(true);
    try {
      await invoke("open_app_data_directory");
    } catch (cause) {
      toast.error(cause instanceof Error ? cause.message : "无法打开应用数据目录。");
    } finally {
      setOpeningDataDirectory(false);
    }
  };


  return <main className="settings-page">
    {storageConfirmOpen && <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="settings-clean-storage-title"><section className="modal settings-confirm-dialog"><header><div><h2 id="settings-clean-storage-title">清理思考事件</h2></div><button type="button" title="关闭" onClick={() => setStorageConfirmOpen(false)}>x</button></header><p>将删除数据库中的 thinking_tokens 遥测并执行 VACUUM 回收空间。会话消息、任务、任务结果和用量统计不会被删除；清理期间请不要运行任务。</p><footer><button type="button" className="secondary" onClick={() => setStorageConfirmOpen(false)}>取消</button><button type="button" className="primary danger" onClick={() => void cleanupStorage()}>确认清理</button></footer></section></div>}
    <header className="settings-header">
      <div className="settings-header-main"><button type="button" className="settings-back" title="返回首页" aria-label="返回首页" onClick={() => navigate("/")}><BackIcon /></button><div><span className="settings-kicker"><SettingsIcon />应用</span><h1>Milevia 设置</h1></div></div>
      <span className="settings-device-label">仅作用于当前设备</span>
    </header>
    <div className="settings-layout">
      <nav className="settings-nav" aria-label="设置分组"><a href="#general">通用</a><a href="#notifications">通知</a><a href="#security">新会话与安全</a><a href="#tasks">任务</a><a href="#data">数据</a><a href="#about">关于</a></nav>
      <div className="settings-content">
        <section id="general" className="settings-section"><header><div><p>通用</p><h2>界面体验</h2></div></header><div className="settings-row"><div><b>代码字号</b><span>文件查看器和编辑器共用此字号。</span></div><div className="font-size-control"><button type="button" aria-label="减小代码字号" title="减小代码字号" disabled={fontSize <= CODE_FONT_SIZE_BOUNDS.min} onClick={() => { const next = setCodeFontSize(fontSize - 1); setFontSize(next); }}>-</button><input aria-label="代码字号" type="range" min={CODE_FONT_SIZE_BOUNDS.min} max={CODE_FONT_SIZE_BOUNDS.max} value={fontSize} style={{ "--range-progress": fontSizeProgress } as CSSProperties} onChange={(event) => { const next = setCodeFontSize(Number(event.target.value)); setFontSize(next); }} /><output>{fontSize}px</output><button type="button" aria-label="增大代码字号" title="增大代码字号" disabled={fontSize >= CODE_FONT_SIZE_BOUNDS.max} onClick={() => { const next = setCodeFontSize(fontSize + 1); setFontSize(next); }}>+</button><button type="button" className="secondary settings-inline-action" onClick={() => { const next = setCodeFontSize(CODE_FONT_SIZE_BOUNDS.default); setFontSize(next); }}>恢复默认</button></div></div><div className="settings-row"><div><b>项目卡片排序</b><span>清除首页拖拽产生的自定义排列，恢复服务端顺序。</span></div><button type="button" className="secondary" onClick={() => { resetProjectOrder(); toast.success("项目卡片排序已恢复默认"); }}>恢复默认</button></div></section>

        <section id="notifications" className="settings-section"><header><div><p>通知</p><h2>提醒方式</h2></div></header><div className="settings-row"><div><b>使用系统通知</b><span>{notificationPermission === "granted" ? "系统通知已获授权。" : notificationPermission === "denied" ? "系统通知已被拒绝。" : notificationPermission === "unsupported" ? "当前环境不支持系统通知。" : "开启后会请求系统通知授权。"}</span></div><Toggle label="使用系统通知" checked={localPreferences.systemNotificationsEnabled} disabled={notificationPermission === "unsupported"} onChange={(checked) => void toggleSystemNotifications(checked)} /></div><div className="settings-row"><div><b>应用在后台时通知</b><span>仅在应用不可见时发送系统通知。</span></div><Toggle label="应用在后台时通知" checked={localPreferences.notifyWhenHidden} disabled={!localPreferences.systemNotificationsEnabled} onChange={(checked) => updateLocalPreferences({ notifyWhenHidden: checked })} /></div><div className="settings-row"><div><b>Windows 弹窗通知</b><span>在 Windows 系统右下角弹出系统通知；内容只显示“有任务完成”，不包含项目或任务名，点击后跳转到对应项目。</span></div><Toggle label="Windows 弹窗通知" checked={localPreferences.windowsToastsEnabled} onChange={(checked) => updateLocalPreferences({ windowsToastsEnabled: checked })} /></div><div className="settings-row"><div><b>任务完成与失败</b><span>控制普通优先级的 Toast 与系统通知；审批和人工介入始终保留应用内提醒。</span></div><Toggle label="任务完成与失败" checked={localPreferences.taskNotificationsEnabled} onChange={(checked) => updateLocalPreferences({ taskNotificationsEnabled: checked })} /></div><div className="settings-row"><div><b>低优先级状态变化</b><span>减少常规状态更新造成的打扰。</span></div><Toggle label="低优先级状态变化" checked={localPreferences.lowPriorityNotificationsEnabled} onChange={(checked) => updateLocalPreferences({ lowPriorityNotificationsEnabled: checked })} /></div><div className="settings-row"><div><b>免打扰时段</b><span>在此时段内不显示普通和低优先级的 Toast 或系统通知；审批和人工介入仍会提醒并保留在通知中心。</span></div><Toggle label="免打扰时段" checked={localPreferences.quietHoursEnabled} onChange={(checked) => updateLocalPreferences({ quietHoursEnabled: checked })} /></div><div className="settings-row settings-time-row"><div><b>免打扰时间</b><span>按当前设备的本地时间计算，支持跨午夜时段。</span></div><div className="settings-time-control"><input aria-label="免打扰开始时间" type="time" value={localPreferences.quietHoursStart} disabled={!localPreferences.quietHoursEnabled} onChange={(event) => updateLocalPreferences({ quietHoursStart: event.target.value })} /><span aria-hidden="true">至</span><input aria-label="免打扰结束时间" type="time" value={localPreferences.quietHoursEnd} disabled={!localPreferences.quietHoursEnabled} onChange={(event) => updateLocalPreferences({ quietHoursEnd: event.target.value })} /></div></div></section>

        <section id="security" className="settings-section"><header><div><p>新会话与安全</p><h2>默认执行边界</h2></div>{appPreferencesLoading && <span className="settings-loading">读取中</span>}</header>{appPreferencesError && <p className="settings-error">{appPreferencesError}</p>}<div className="settings-row"><div><b>默认 Agent</b><span>只影响之后打开的新会话，已创建会话不会改变。</span></div><select aria-label="默认 Agent" disabled={appPreferencesLoading} value={appPreferences.defaultAgentId} onChange={(event) => void saveAppPreferences({ defaultAgentId: event.target.value as AppPreferences["defaultAgentId"] })}><option value="claude-code">Claude Code</option><option value="codex">Codex</option></select></div><div className="settings-row"><div><b>Claude Code 默认权限</b><span>设置新建 Claude Code 会话的初始权限。</span></div><select aria-label="Claude Code 默认权限" disabled={appPreferencesLoading} value={appPreferences.claudePermissionMode} onChange={(event) => choosePermission({ claudePermissionMode: event.target.value as AppPreferences["claudePermissionMode"] })}><option value="approval_required">默认权限</option><option value="full_control">完全控制</option></select></div><div className="settings-row"><div><b>Codex 默认权限</b><span>设置新建 Codex 会话的初始权限。</span></div><select aria-label="Codex 默认权限" disabled={appPreferencesLoading} value={appPreferences.codexPermissionMode} onChange={(event) => choosePermission({ codexPermissionMode: event.target.value as AppPreferences["codexPermissionMode"] })}><option value="read_only">仅分析</option><option value="workspace_write">项目内执行</option><option value="full_control">完全控制</option></select></div></section>

        <section id="tasks" className="settings-section"><header><div><p>任务</p><h2>任务验收</h2></div>{appPreferencesLoading && <span className="settings-loading">读取中</span>}</header>{appPreferencesError && <p className="settings-error">{appPreferencesError}</p>}<div className="settings-row"><div><b>自动验收任务</b><span>开启后，执行成功的任务会自动验收并从队列消失，无需手动确认；默认关闭。失败或被中断的任务仍会进入「需处理」等待处理。</span></div><Toggle label="自动验收任务" checked={appPreferences.autoReview} disabled={appPreferencesLoading} onChange={(checked) => void saveAppPreferences({ autoReview: checked })} /></div></section>

        <section id="data" className="settings-section"><header><div><p>数据</p><h2>本地草稿与偏好</h2></div></header><div className="settings-row"><div><b>自动保存未发送草稿</b><span>关闭后停止写入新草稿，已有草稿不会被删除。</span></div><Toggle label="自动保存未发送草稿" checked={localPreferences.draftAutoSave} onChange={(checked) => updateLocalPreferences({ draftAutoSave: checked })} /></div><div className="settings-row"><div><b>草稿保留期限</b><span>缩短期限时会立即清理过期草稿。</span></div><select aria-label="草稿保留期限" value={localPreferences.draftRetentionDays} onChange={(event) => updateLocalPreferences({ draftRetentionDays: Number(event.target.value) as 7 | 30 | 90 })}><option value="7">7 天</option><option value="30">30 天</option><option value="90">90 天</option></select></div><div className="settings-row settings-danger-row"><div><b>清除未发送草稿</b><span>当前设备有 {draftCount} 条可清除草稿，不会影响项目、会话或任务。</span></div><button type="button" className="secondary" disabled={draftCount === 0} onClick={() => setClearDraftsOpen(true)}>清除草稿</button></div><div className="settings-row settings-danger-row"><div><b>恢复本地界面偏好</b><span>仅恢复通知、代码字号和项目卡片排序，不会删除项目或任何凭据。</span></div><button type="button" className="secondary" onClick={() => setResetLocalOpen(true)}>恢复默认</button></div>{isDesktop() && <><div className="settings-row settings-storage-row"><div><b>应用数据库占用</b><span>{storageLoading ? "正在读取占用..." : storage ? <>数据库总量 {formatBytes(storage.databaseBytes)}；消息负载 {formatBytes(tableBytes(storage, "messages"))}；会话事件负载 {formatBytes(tableBytes(storage, "events"))}；任务事件负载 {formatBytes(tableBytes(storage, "task_events"))}；thinking_tokens {storage.thinkingTokenEvents.toLocaleString()} 条（{formatBytes(storage.thinkingTokenBytes)}）</> : "暂时无法读取数据占用"}</span></div><button type="button" className="secondary" disabled={storageLoading} onClick={() => void loadStorage()}>刷新</button></div><div className="settings-row settings-danger-row"><div><b>清理思考事件</b><span>删除高频 thinking_tokens 遥测并回收数据库空间，不会删除会话消息、任务或结果；运行中的任务需要先完成。</span></div><button type="button" className="secondary" disabled={!storage || storage.thinkingTokenEvents === 0 || storageCleaning} onClick={() => setStorageConfirmOpen(true)}>{storageCleaning ? "清理中" : "清理数据"}</button></div></>}{isDesktop() && <div className="settings-row"><div><b>应用数据目录</b><span>打开当前设备保存应用数据和本地草稿的目录。</span></div><button type="button" className="secondary" disabled={openingDataDirectory} onClick={() => void openDataDirectory()}>{openingDataDirectory ? "打开中" : "打开目录"}</button></div>}</section>

        <section id="about" className="settings-section"><header><div><p>关于</p><h2>Milevia</h2></div></header><div className="settings-row"><div><b>运行环境</b><span>{isDesktop() ? "桌面端，本地偏好仅保存在当前设备。" : "Web 环境，本地偏好仅保存在当前浏览器。"}</span></div><span className="settings-value">{isDesktop() ? "桌面端" : "Web"}</span></div>{isDesktop() && <div className="settings-row"><div><b>当前版本</b><span>{updaterError || updaterStatus?.status === "failed" ? (updaterError || updaterStatus?.error || "更新检查失败，请稍后重试。") : updaterStatus?.status === "checking" ? "正在检查更新…" : updaterStatus?.update ? `发现新版本 v${updaterStatus.update.version}${updaterStatus.update.notes ? `：${updaterStatus.update.notes.trim().slice(0, 80)}` : ""}` : updaterStatus ? "当前已是最新版本。" : "正在读取更新状态。"}</span></div><div className="settings-update-actions"><button type="button" className="secondary" disabled={checkingUpdate || installingUpdate} onClick={() => void checkForUpdate()}>{checkingUpdate ? "检查中" : "检查更新"}</button>{updaterStatus?.status === "complete" && updaterStatus.update ? <button type="button" className="primary" disabled={checkingUpdate || installingUpdate} onClick={() => void installUpdate()}>{installingUpdate ? "升级中" : "立即升级"}</button> : <span className="settings-value">v{updaterStatus?.appVersion ?? "-"}</span>}</div></div>}</section>
      </div>
    </div>
    {pendingFullControl && <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="settings-full-control-title"><section className="modal settings-confirm-dialog"><header><div><h2 id="settings-full-control-title">设为完全控制</h2></div><button type="button" title="关闭" onClick={() => setPendingFullControl(null)}>x</button></header><p>完全控制会让新会话直接执行命令，不再等待确认。这个设置不会修改现有会话。</p><footer><button type="button" className="secondary" onClick={() => setPendingFullControl(null)}>取消</button><button type="button" className="primary danger" onClick={() => { const patch = pendingFullControl; setPendingFullControl(null); void saveAppPreferences(patch); }}>确认设置</button></footer></section></div>}
    {clearDraftsOpen && <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="settings-clear-drafts-title"><section className="modal settings-confirm-dialog"><header><div><h2 id="settings-clear-drafts-title">清除未发送草稿</h2></div><button type="button" title="关闭" onClick={() => setClearDraftsOpen(false)}>x</button></header><p>将清除当前设备中的 {draftCount} 条未发送草稿，此操作不会影响项目、会话、任务或 SSH 连接。</p><footer><button type="button" className="secondary" onClick={() => setClearDraftsOpen(false)}>取消</button><button type="button" className="primary danger" onClick={() => { const cleared = clearConversationDrafts(); setDraftVersion((value) => value + 1); setClearDraftsOpen(false); toast.success(`已清除 ${cleared} 条草稿`); }}>确认清除</button></footer></section></div>}
    {resetLocalOpen && <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="settings-reset-local-title"><section className="modal settings-confirm-dialog"><header><div><h2 id="settings-reset-local-title">恢复本地界面偏好</h2></div><button type="button" title="关闭" onClick={() => setResetLocalOpen(false)}>x</button></header><p>将恢复通知、代码字号和项目卡片排序。项目、会话、草稿、SSH 连接和凭据不会被删除。</p><footer><button type="button" className="secondary" onClick={() => setResetLocalOpen(false)}>取消</button><button type="button" className="primary danger" onClick={resetLocal}>确认恢复</button></footer></section></div>}
  </main>;
}
