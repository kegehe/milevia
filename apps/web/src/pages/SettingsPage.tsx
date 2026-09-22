import { useCallback, useEffect, useMemo, useRef, useState, type CSSProperties, type ReactNode } from "react";
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
import { agentDisplayName, useAgentCatalog } from "../lib/agent-registry";
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
  pageSize: number;
  pageCount: number;
  freelistPages: number;
  freelistBytes: number;
  // 后台正在做一次性空间回收（VACUUM，会长时间独占数据库）。这段时间所有数据库接口
  // 都会变慢，界面上要说明白，免得被当成新的卡死。
  reclaiming: boolean;
  // 为 false 表示统计中途超时，数字不全。
  measurementComplete: boolean;
  tables: Array<{ name: string; rows: number; payloadBytes?: number; payloadEstimated?: boolean }>;
};
// 清理接口单独返回精确的 thinking_tokens 条数：它只在这个动作里扫一次全表，
// 日常的占用面板不再为它付代价。
type StorageCleanupResult = {
  after: StorageUsage;
  freedBytes: number;
  thinkingTokenEvents: number;
  thinkingTokenBytes: number;
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

function ShieldIcon() {
  return <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 3 5 6v6c0 4.5 3 7.5 7 9 4-1.5 7-4.5 7-9V6l-7-3Z" /><path d="m9 12 2 2 4-4" /></svg>;
}

function SettingsIcon() {
  return <svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="2.9" /><path d="M20.17 10.56A8.3 8.3 0 0 1 20.17 13.44L17.51 12.97A5.6 5.6 0 0 1 16.59 15.21L18.80 16.76A8.3 8.3 0 0 1 16.76 18.80L15.21 16.59A5.6 5.6 0 0 1 12.97 17.51L13.44 20.17A8.3 8.3 0 0 1 10.56 20.17L11.03 17.51A5.6 5.6 0 0 1 8.79 16.59L7.24 18.80A8.3 8.3 0 0 1 5.20 16.76L7.41 15.21A5.6 5.6 0 0 1 6.49 12.97L3.83 13.44A8.3 8.3 0 0 1 3.83 10.56L6.49 11.03A5.6 5.6 0 0 1 7.41 8.79L5.20 7.24A8.3 8.3 0 0 1 7.24 5.20L8.79 7.41A5.6 5.6 0 0 1 11.03 6.49L10.56 3.83A8.3 8.3 0 0 1 13.44 3.83L12.97 6.49A5.6 5.6 0 0 1 15.21 7.41L16.76 5.20A8.3 8.3 0 0 1 18.80 7.24L16.59 8.79A5.6 5.6 0 0 1 17.51 11.03L20.17 10.56Z" /></svg>;
}

function Toggle({ checked, disabled, onChange, label }: { checked: boolean; disabled?: boolean; onChange: (checked: boolean) => void; label: string }) {
  return <label className="settings-toggle" title={label}><input type="checkbox" checked={checked} disabled={disabled} onChange={(event) => onChange(event.target.checked)} /><span aria-hidden="true" /></label>;
}

// 分组表是唯一事实来源：Tab 顺序、标题、副标题、面板内容都从这里取。
// 加一个分组只改这里，别再去别处补第二份清单。
type TabId = "general" | "notifications" | "security" | "tasks" | "data" | "about";
const TAB_ORDER: TabId[] = ["general", "notifications", "security", "tasks", "data", "about"];
const TAB_LABELS: Record<TabId, { tab: string; eyebrow: string; title: string; lede: string }> = {
  general: { tab: "通用", eyebrow: "通用", title: "界面体验", lede: "字号会同时作用于文件查看器、编辑器与差异视图；这些偏好只保存在当前设备，不会同步到其它电脑。" },
  notifications: { tab: "通知", eyebrow: "通知", title: "提醒方式", lede: "系统通知、应用内 Toast 与免打扰时段集中在这一页。审批和人工介入不受这里的开关影响。" },
  security: { tab: "新会话与安全", eyebrow: "新会话与安全", title: "默认执行边界", lede: "以下设置只作用于之后新建的会话；已经打开的会话不会被改动。" },
  tasks: { tab: "任务", eyebrow: "任务", title: "任务验收", lede: "控制任务完成后的处理方式。这里改的是整台设备的默认行为。" },
  data: { tab: "数据", eyebrow: "数据", title: "本地草稿与偏好", lede: "草稿与界面偏好都保存在当前设备。下面标注为破坏性的操作无法撤销。" },
  about: { tab: "关于", eyebrow: "关于", title: "Milevia", lede: "当前运行环境与版本信息。更新只会应用于当前设备。" },
};

function isTabId(value: string): value is TabId {
  return (TAB_ORDER as string[]).includes(value);
}

/** 把 hash 当唯一真相：刷新、前进后退、深链都靠它，组件内不再另存一份。 */
function readTabFromHash(): TabId {
  if (typeof window === "undefined") return "general";
  const raw = window.location.hash.replace(/^#/, "");
  return isTabId(raw) ? raw : "general";
}

/** 卡片式设置项：左文右控件。危险项加 danger 变体（红底 + 红按钮）。 */
function SettingCard({ title, description, children, wide, danger }: { title: string; description: ReactNode; children?: ReactNode; wide?: boolean; danger?: boolean }) {
  return <div className={`settings-card${wide ? " wide" : ""}${danger ? " danger" : ""}`}>
    <div className="settings-card-text"><b>{title}</b><span>{description}</span></div>
    {children !== undefined && <div className="settings-card-side">{children}</div>}
  </div>;
}

export default function SettingsPage() {
  const navigate = useNavigate();
  // 默认 Agent 的候选来自服务端目录：写死两项的话，目录里新增的工具根本选不到。
  const agentOptions = useAgentCatalog();
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
  // 不支持 Web Notification 的环境（桌面端 / 原生包 / 非安全上下文的浏览器）里，
  // 那两条开关连同"点了必报错"的入口一起收起，见通知分组里的说明。
  const systemNotificationsAvailable = notificationPermission !== "unsupported";
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
  const [activeTab, setActiveTab] = useState<TabId>(readTabFromHash);
  const tabsRef = useRef<HTMLDivElement | null>(null);
  const draftCount = useMemo(() => countStoredConversationDrafts(localPreferences.draftRetentionDays), [draftVersion, localPreferences.draftRetentionDays]);

  // hash 是外部状态：浏览器前进/后退、外链跳进来都要能同步回组件。
  useEffect(() => {
    const sync = () => setActiveTab(readTabFromHash());
    window.addEventListener("hashchange", sync);
    return () => window.removeEventListener("hashchange", sync);
  }, []);

  // 窄屏下分页栏会横向溢出：选中项必须自己滚进可视区，否则用户按方向键切到「关于」
  // 时屏幕上毫无变化，看起来像"点了没反应"。inline:"nearest" 保证只在必要时滚、不抖。
  useEffect(() => {
    const bar = tabsRef.current;
    if (!bar) return;
    const active = bar.querySelector<HTMLElement>(".active");
    if (!active) return;
    const barBox = bar.getBoundingClientRect();
    const itemBox = active.getBoundingClientRect();
    if (itemBox.left < barBox.left) bar.scrollLeft -= barBox.left - itemBox.left;
    else if (itemBox.right > barBox.right) bar.scrollLeft += itemBox.right - barBox.right;
  }, [activeTab]);

  // 切 Tab 时把 URL 一起改掉（replace 避免每次切换都塞一条历史）。
  const selectTab = useCallback((next: TabId) => {
    setActiveTab(next);
    if (typeof window !== "undefined") window.history.replaceState(null, "", `#${next}`);
  }, []);

  // Tab 键盘导航：左右箭头在分页间移动，Home/End 跳首尾。
  const onTabKeyDown = (event: { key: string; preventDefault: () => void }) => {
    const index = TAB_ORDER.indexOf(activeTab);
    let next: TabId | null = null;
    if (event.key === "ArrowRight") next = TAB_ORDER[(index + 1) % TAB_ORDER.length];
    else if (event.key === "ArrowLeft") next = TAB_ORDER[(index - 1 + TAB_ORDER.length) % TAB_ORDER.length];
    else if (event.key === "Home") next = TAB_ORDER[0];
    else if (event.key === "End") next = TAB_ORDER[TAB_ORDER.length - 1];
    if (!next) return;
    event.preventDefault();
    selectTab(next);
  };

  // quiet 用于后台轮询：不动 loading 态、失败也不弹 toast，免得每 5 秒闪一次或刷屏。
  const loadStorage = async (quiet = false) => {
    if (!isDesktop()) return;
    if (!quiet) setStorageLoading(true);
    try { setStorage(await apiWithTimeout<StorageUsage>("/api/system/storage", undefined, 0, 30_000)); }
    catch (cause) { if (!quiet) toast.error(cause instanceof Error ? cause.message : "无法读取数据占用"); }
    finally { if (!quiet) setStorageLoading(false); }
  };

  const cleanupStorage = async () => {
    setStorageConfirmOpen(false);
    setStorageCleaning(true);
    try {
      // 这次调用可能触发整库重写（VACUUM），服务端给出的上限是 15 分钟且刻意不受客户端
      // abort 影响，所以这里不重试、给足时间。
      const result = await apiWithTimeout<StorageCleanupResult>("/api/system/storage/cleanup", { method: "POST", body: "{}" }, 0, 900_000);
      setStorage(result.after);
      const removed = result.thinkingTokenEvents > 0 ? `已删除 ${result.thinkingTokenEvents.toLocaleString()} 条思考事件，` : "";
      toast.success(`${removed}释放 ${formatBytes(result.freedBytes)}`);
    } catch (cause) { toast.error(cause instanceof Error ? cause.message : "清理失败"); }
    finally { setStorageCleaning(false); }
  };

  useEffect(() => { void loadStorage(); }, []);

  // 后台正在整理数据库空间时轮询刷新：整库重写期间所有数据库接口都会变慢，这一段
  // 不主动刷新的话，用户只会看到一个过期的数字，然后把"卡"当成新故障。
  //
  // 用「上一次跑完再排下一次」而不是 setInterval：整理期间一次读取就可能耗时数秒，
  // 固定间隔会让请求互相叠加 —— 那正是要避免的连接池压力。
  useEffect(() => {
    if (!storage?.reclaiming) return;
    let cancelled = false;
    let timer: number | undefined;
    const poll = async () => {
      if (cancelled) return;
      await loadStorage(true);
      if (cancelled) return;
      timer = window.setTimeout(() => void poll(), 5_000);
    };
    timer = window.setTimeout(() => void poll(), 5_000);
    return () => { cancelled = true; if (timer !== undefined) window.clearTimeout(timer); };
  }, [storage?.reclaiming]);

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
    toast.error(permission === "denied" ? "系统通知已被拒绝，请在浏览器或系统的通知设置里允许。" : "当前环境不支持系统通知。");
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
    {storageConfirmOpen && <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="settings-clean-storage-title"><section className="modal settings-confirm-dialog"><header><div><h2 id="settings-clean-storage-title">清理数据库</h2></div><button type="button" title="关闭" onClick={() => setStorageConfirmOpen(false)}>x</button></header><p>将删除数据库中遗留的 thinking_tokens 遥测，并回收文件里已释放的页（必要时会重写整个数据库文件，期间其它操作会明显变慢）。会话消息、任务、任务结果和用量统计不会被删除；清理期间请不要运行任务。</p><footer><button type="button" className="secondary" onClick={() => setStorageConfirmOpen(false)}>取消</button><button type="button" className="primary danger" onClick={() => void cleanupStorage()}>确认清理</button></footer></section></div>}
    <header className="settings-header">
      <div className="settings-header-main"><button type="button" className="settings-back" title="返回首页" aria-label="返回首页" onClick={() => navigate("/")}><BackIcon /></button><div><span className="settings-kicker"><SettingsIcon />应用</span><h1>Milevia 设置</h1></div></div>
      <span className="settings-device-label">仅作用于当前设备</span>
    </header>
    <div className="settings-tabs" role="tablist" aria-label="设置分组" ref={tabsRef} onKeyDown={onTabKeyDown}>{TAB_ORDER.map((id) => <button key={id} type="button" role="tab" id={`settings-tab-${id}`} aria-selected={activeTab === id} aria-controls={`settings-panel-${id}`} tabIndex={activeTab === id ? 0 : -1} className={activeTab === id ? "active" : ""} onClick={() => selectTab(id)}>{TAB_LABELS[id].tab}</button>)}</div>
    <div className="settings-board">
      <header className="settings-board-hero"><p>{TAB_LABELS[activeTab].eyebrow}</p><h2>{TAB_LABELS[activeTab].title}</h2><span>{TAB_LABELS[activeTab].lede}</span></header>
      <div className="settings-panel" role="tabpanel" id={`settings-panel-${activeTab}`} aria-labelledby={`settings-tab-${activeTab}`}>
        {activeTab === "general" && <div className="settings-grid">
          <SettingCard wide title="代码字号" description="文件查看器和编辑器共用此字号。">
            <div className="font-size-control"><button type="button" aria-label="减小代码字号" title="减小代码字号" disabled={fontSize <= CODE_FONT_SIZE_BOUNDS.min} onClick={() => { const next = setCodeFontSize(fontSize - 1); setFontSize(next); }}>-</button><input aria-label="代码字号" type="range" min={CODE_FONT_SIZE_BOUNDS.min} max={CODE_FONT_SIZE_BOUNDS.max} value={fontSize} style={{ "--range-progress": fontSizeProgress } as CSSProperties} onChange={(event) => { const next = setCodeFontSize(Number(event.target.value)); setFontSize(next); }} /><output>{fontSize}px</output><button type="button" aria-label="增大代码字号" title="增大代码字号" disabled={fontSize >= CODE_FONT_SIZE_BOUNDS.max} onClick={() => { const next = setCodeFontSize(fontSize + 1); setFontSize(next); }}>+</button></div>
          </SettingCard>
          <SettingCard title="项目卡片排序" description="清除首页拖拽产生的自定义排列，恢复服务端顺序。">
            <button type="button" className="secondary" onClick={() => { resetProjectOrder(); toast.success("项目卡片排序已恢复默认"); }}>恢复默认</button>
          </SettingCard>
        </div>}

        {activeTab === "notifications" && <div className="settings-grid">
          {/* 两条 Web Notification 开关只在浏览器里有意义（桌面端/原生包的支持判定见
              stores/useUIPreferences.tsx → webNotificationsSupported）：在桌面端留着它们，
              用户点了只会得到"已被拒绝"，而那个 denied 存在应用自己的 WebView2 profile 里，
              界面上没有任何地方能改。桌面端的系统通知入口是下面的「Windows 弹窗通知」。 */}
          {systemNotificationsAvailable && <>
            <SettingCard title="使用系统通知" description={notificationPermission === "granted" ? "系统通知已获授权。" : notificationPermission === "denied" ? "系统通知已被拒绝。" : "开启后会请求系统通知授权。"}>
              <Toggle label="使用系统通知" checked={localPreferences.systemNotificationsEnabled} onChange={(checked) => void toggleSystemNotifications(checked)} />
            </SettingCard>
            <SettingCard title="应用在后台时通知" description="仅在页面不可见时发送浏览器系统通知。">
              <Toggle label="应用在后台时通知" checked={localPreferences.notifyWhenHidden} disabled={!localPreferences.systemNotificationsEnabled} onChange={(checked) => updateLocalPreferences({ notifyWhenHidden: checked })} />
            </SettingCard>
          </>}
          {/* 反向的同一条死路：Windows 弹窗走 Rust winrt Toast，Web 端调用点被 isDesktop() 挡着，
              在浏览器里勾上永远不会有任何效果，所以只在桌面端显示。 */}
          {isDesktop() && <SettingCard wide title="Windows 弹窗通知" description="在 Windows 系统右下角弹出系统通知；内容只显示“有任务完成”，不包含项目或任务名，点击后跳转到对应项目。">
            <Toggle label="Windows 弹窗通知" checked={localPreferences.windowsToastsEnabled} onChange={(checked) => updateLocalPreferences({ windowsToastsEnabled: checked })} />
          </SettingCard>}
          <SettingCard title="任务完成与失败" description="控制普通优先级的 Toast 与系统通知；审批和人工介入始终保留应用内提醒。">
            <Toggle label="任务完成与失败" checked={localPreferences.taskNotificationsEnabled} onChange={(checked) => updateLocalPreferences({ taskNotificationsEnabled: checked })} />
          </SettingCard>
          <SettingCard title="低优先级状态变化" description="减少常规状态更新造成的打扰。">
            <Toggle label="低优先级状态变化" checked={localPreferences.lowPriorityNotificationsEnabled} onChange={(checked) => updateLocalPreferences({ lowPriorityNotificationsEnabled: checked })} />
          </SettingCard>
          <SettingCard wide title="免打扰时段" description="在此时段内不显示普通和低优先级的 Toast 或系统通知；审批和人工介入仍会提醒并保留在通知中心。">
            <div className="settings-time-control"><input aria-label="免打扰开始时间" type="time" value={localPreferences.quietHoursStart} disabled={!localPreferences.quietHoursEnabled} onChange={(event) => updateLocalPreferences({ quietHoursStart: event.target.value })} /><span aria-hidden="true">至</span><input aria-label="免打扰结束时间" type="time" value={localPreferences.quietHoursEnd} disabled={!localPreferences.quietHoursEnabled} onChange={(event) => updateLocalPreferences({ quietHoursEnd: event.target.value })} /></div>
            <Toggle label="免打扰时段" checked={localPreferences.quietHoursEnabled} onChange={(checked) => updateLocalPreferences({ quietHoursEnabled: checked })} />
          </SettingCard>
          <div className="settings-note settings-grid-full"><span className="settings-note-icon"><ShieldIcon /></span><div><b>审批与人工介入始终保留应用内提醒</b><span>上面这些开关与免打扰时段都不会静默这类中断，它们会一直出现在通知中心。</span></div></div>
        </div>}

        {activeTab === "security" && <div className="settings-grid">
          {appPreferencesError && <p className="settings-error">{appPreferencesError}</p>}
          <SettingCard wide title="默认 Agent" description="只影响之后打开的新会话，已创建会话不会改变。">
            <select aria-label="默认 Agent" disabled={appPreferencesLoading} value={appPreferences.defaultAgentId} onChange={(event) => void saveAppPreferences({ defaultAgentId: event.target.value as AppPreferences["defaultAgentId"] })}>{agentOptions.length === 0
							? <option value={appPreferences.defaultAgentId}>{agentDisplayName(appPreferences.defaultAgentId)}</option>
							: agentOptions.map((entry) => <option key={entry.id} value={entry.id}>{entry.name}</option>)}</select>
          </SettingCard>
          <SettingCard title="Claude Code 默认权限" description="设置新建 Claude Code 会话的初始权限。">
            <select aria-label="Claude Code 默认权限" disabled={appPreferencesLoading} value={appPreferences.claudePermissionMode} onChange={(event) => choosePermission({ claudePermissionMode: event.target.value as AppPreferences["claudePermissionMode"] })}><option value="approval_required">默认权限</option><option value="full_control">完全控制</option></select>
          </SettingCard>
          <SettingCard title="Codex 默认权限" description="设置新建 Codex 会话的初始权限。">
            <select aria-label="Codex 默认权限" disabled={appPreferencesLoading} value={appPreferences.codexPermissionMode} onChange={(event) => choosePermission({ codexPermissionMode: event.target.value as AppPreferences["codexPermissionMode"] })}><option value="read_only">仅分析</option><option value="workspace_write">项目内执行</option><option value="full_control">完全控制</option></select>
          </SettingCard>
          {appPreferencesLoading && <p className="settings-loading">正在读取默认设置…</p>}
        </div>}

        {activeTab === "tasks" && <div className="settings-grid">
          {appPreferencesError && <p className="settings-error">{appPreferencesError}</p>}
          <SettingCard wide title="自动验收任务" description="开启后，执行成功的任务会自动验收并从队列消失，无需手动确认；默认关闭。失败或被中断的任务仍会进入「需处理」等待处理。">
            <Toggle label="自动验收任务" checked={appPreferences.autoReview} disabled={appPreferencesLoading} onChange={(checked) => void saveAppPreferences({ autoReview: checked })} />
          </SettingCard>
          {appPreferencesLoading && <p className="settings-loading">正在读取默认设置…</p>}
        </div>}

        {activeTab === "data" && <div className="settings-grid">
          <SettingCard title="自动保存未发送草稿" description="关闭后停止写入新草稿，已有草稿不会被删除。">
            <Toggle label="自动保存未发送草稿" checked={localPreferences.draftAutoSave} onChange={(checked) => updateLocalPreferences({ draftAutoSave: checked })} />
          </SettingCard>
          <SettingCard title="草稿保留期限" description="缩短期限时会立即清理过期草稿。">
            <select aria-label="草稿保留期限" value={localPreferences.draftRetentionDays} onChange={(event) => updateLocalPreferences({ draftRetentionDays: Number(event.target.value) as 7 | 30 | 90 })}><option value="7">7 天</option><option value="30">30 天</option><option value="90">90 天</option></select>
          </SettingCard>
          {isDesktop() && <SettingCard wide title="应用数据库占用" description={storageLoading ? "正在读取占用..." : storage ? <>{storage.reclaiming ? "正在整理数据库空间（期间其它操作会变慢）… " : ""}数据库总量 {formatBytes(storage.databaseBytes)}；可回收空间 {formatBytes(storage.freelistBytes)}；消息负载约 {formatBytes(tableBytes(storage, "messages"))}；会话事件负载约 {formatBytes(tableBytes(storage, "events"))}{storage.measurementComplete ? "" : "（统计未跑完，数字不全）"}</> : "暂时无法读取数据占用"}>
            <button type="button" className="secondary" disabled={storageLoading} onClick={() => void loadStorage()}>刷新</button>
          </SettingCard>}
          {isDesktop() && <SettingCard wide title="应用数据目录" description="打开当前设备保存应用数据和本地草稿的目录。">
            <button type="button" className="secondary" disabled={openingDataDirectory} onClick={() => void openDataDirectory()}>{openingDataDirectory ? "打开中" : "打开目录"}</button>
          </SettingCard>}
          <div className="settings-danger-group settings-grid-full"><p className="settings-danger-title">破坏性操作</p>
            <SettingCard danger title="清除未发送草稿" description={`当前设备有 ${draftCount} 条可清除草稿，不会影响项目、会话或任务。`}>
              <button type="button" className="secondary" disabled={draftCount === 0} onClick={() => setClearDraftsOpen(true)}>清除草稿</button>
            </SettingCard>
            <SettingCard danger title="恢复本地界面偏好" description="仅恢复通知、代码字号和项目卡片排序，不会删除项目或任何凭据。">
              <button type="button" className="secondary" onClick={() => setResetLocalOpen(true)}>恢复默认</button>
            </SettingCard>
            {isDesktop() && <SettingCard danger wide title="清理数据库" description="删除遗留的 thinking_tokens 遥测，并回收数据库文件里已释放的页；不会删除会话消息、任务或结果。运行中的任务需要先完成，整库整理期间操作会变慢。">
              <button type="button" className="secondary" disabled={storageCleaning} onClick={() => setStorageConfirmOpen(true)}>{storageCleaning ? "清理中" : "清理数据"}</button>
            </SettingCard>}
          </div>
        </div>}

        {activeTab === "about" && <div className="settings-grid">
          <SettingCard title="运行环境" description={isDesktop() ? "桌面端，本地偏好仅保存在当前设备。" : "Web 环境，本地偏好仅保存在当前浏览器。"}>
            <span className="settings-value">{isDesktop() ? "桌面端" : "Web"}</span>
          </SettingCard>
          {isDesktop() && <SettingCard wide title="当前版本" description={updaterError || updaterStatus?.status === "failed" ? (updaterError || updaterStatus?.error || "更新检查失败，请稍后重试。") : updaterStatus?.status === "checking" ? "正在检查更新…" : updaterStatus?.update ? `发现新版本 v${updaterStatus.update.version}${updaterStatus.update.notes ? `：${updaterStatus.update.notes.trim().slice(0, 80)}` : ""}` : updaterStatus ? "当前已是最新版本。" : "正在读取更新状态。"}>
            <button type="button" className="secondary" disabled={checkingUpdate || installingUpdate} onClick={() => void checkForUpdate()}>{checkingUpdate ? "检查中" : "检查更新"}</button>
            {updaterStatus?.status === "complete" && updaterStatus.update
              ? <button type="button" className="primary" disabled={checkingUpdate || installingUpdate} onClick={() => void installUpdate()}>{installingUpdate ? "升级中" : "立即升级"}</button>
              : <span className="settings-value">v{updaterStatus?.appVersion ?? "-"}</span>}
          </SettingCard>}
        </div>}
      </div>
    </div>
    {pendingFullControl && <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="settings-full-control-title"><section className="modal settings-confirm-dialog"><header><div><h2 id="settings-full-control-title">设为完全控制</h2></div><button type="button" title="关闭" onClick={() => setPendingFullControl(null)}>x</button></header><p>完全控制会让新会话直接执行命令，不再等待确认。这个设置不会修改现有会话。</p><footer><button type="button" className="secondary" onClick={() => setPendingFullControl(null)}>取消</button><button type="button" className="primary danger" onClick={() => { const patch = pendingFullControl; setPendingFullControl(null); void saveAppPreferences(patch); }}>确认设置</button></footer></section></div>}
    {clearDraftsOpen && <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="settings-clear-drafts-title"><section className="modal settings-confirm-dialog"><header><div><h2 id="settings-clear-drafts-title">清除未发送草稿</h2></div><button type="button" title="关闭" onClick={() => setClearDraftsOpen(false)}>x</button></header><p>将清除当前设备中的 {draftCount} 条未发送草稿，此操作不会影响项目、会话、任务或 SSH 连接。</p><footer><button type="button" className="secondary" onClick={() => setClearDraftsOpen(false)}>取消</button><button type="button" className="primary danger" onClick={() => { const cleared = clearConversationDrafts(); setDraftVersion((value) => value + 1); setClearDraftsOpen(false); toast.success(`已清除 ${cleared} 条草稿`); }}>确认清除</button></footer></section></div>}
    {resetLocalOpen && <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="settings-reset-local-title"><section className="modal settings-confirm-dialog"><header><div><h2 id="settings-reset-local-title">恢复本地界面偏好</h2></div><button type="button" title="关闭" onClick={() => setResetLocalOpen(false)}>x</button></header><p>将恢复通知、代码字号和项目卡片排序。项目、会话、草稿、SSH 连接和凭据不会被删除。</p><footer><button type="button" className="secondary" onClick={() => setResetLocalOpen(false)}>取消</button><button type="button" className="primary danger" onClick={resetLocal}>确认恢复</button></footer></section></div>}
  </main>;
}
