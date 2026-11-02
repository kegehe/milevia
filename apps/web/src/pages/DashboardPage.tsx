// 项目列表页 — 从 App.tsx ProjectDashboard 提取

import { useEffect, useState, useRef, useCallback, useMemo, type DragEvent } from "react";
import { toast } from "sonner";
import { useNavigate } from "react-router-dom";
import { useProjectContext } from "../stores/useProjectStore";
import { useProcessStatusMap } from "../components/ProcessStatusProvider";
import { useLiveStateEventsFor } from "../components/LiveEventsProvider";
import NotificationCenter from "../components/NotificationCenter";
import type { Project, ProjectFilter, ProjectStatus } from "../lib/types";
import type { RunStatus } from "../features/run/run-model";
import { statusColors, statusLabels, runDurationLabel } from "../features/run/run-model";
import { sortProjectIds, moveProject, persistOrder } from "../lib/project-order";
import { autoscrollDelta } from "../lib/card-drag";
import { literalNameClass, workspacePathForDisplay } from "../lib/utils";
import { AppVersionTag } from "../features/updater/AppVersionTag";
import { IcpFooter } from "../components/IcpFooter";
import { isDesktop } from "../lib/runtime";
import "./dashboard.css";

function WindowsEnvironmentIcon() {
  return <svg className="env-tag-icon windows" viewBox="0 0 24 24" aria-hidden="true"><path d="M3 5.5 10.5 4v7H3v-5.5ZM13 3.5 21 2v9h-8v-7.5ZM3 13h7.5v7L3 18.5V13ZM13 13h8v9l-8-1.5V13Z" /></svg>;
}

function RemoteServerIcon() {
  return <svg className="env-tag-icon remote" viewBox="0 0 24 24" aria-hidden="true"><rect x="4" y="4" width="16" height="6" rx="1.2" /><rect x="4" y="14" width="16" height="6" rx="1.2" /><path d="M8 7h.01M8 17h.01M12 7h5M12 17h5" /></svg>;
}

function WslEnvironmentIcon() {
  // 企鹅：WSL = Windows 上的 Linux，用 Tux 剪影比抽象的"四叶风车"更易识别。
  // 身体/头部用 evenodd 挖出白色肚皮、眼睛与嘴，翅膀单独绘制盖在肚皮上。
  return <svg className="env-tag-icon wsl" viewBox="0 0 24 24" aria-hidden="true"><g fill="currentColor"><path fillRule="evenodd" d="M9 7.8L15 7.8C18.2 9 19.2 11.2 19 14C18.8 16.8 18 19.2 15.6 20.6C14.4 21.4 9.6 21.4 8.4 20.6C6 19.2 5.2 16.8 5 14C4.8 11.2 5.8 9 9 7.8ZM8.6 15.6a3.4 3.8 0 1 0 6.8 0a3.4 3.8 0 1 0 -6.8 0Z" /><path fillRule="evenodd" d="M12 1.2a4.2 4.2 0 1 0 0 8.4a4.2 4.2 0 1 0 0 -8.4ZM10.2 4.2a1 1 0 1 0 0 2a1 1 0 1 0 0 -2ZM13.8 4.2a1 1 0 1 0 0 2a1 1 0 1 0 0 -2ZM10.9 6.9L13.1 6.9L12 8.8Z" /><path d="M16.5 10.8C18.8 11.8 20.2 14 20.2 16.2C20.2 18.4 18.6 19.6 16.2 19.6C15.2 19.6 14.6 19 14.4 18.4C14.8 16.2 15.2 13 16.5 10.8Z" /><path d="M7.5 10.8C5.2 11.8 3.8 14 3.8 16.2C3.8 18.4 5.4 19.6 7.8 19.6C8.8 19.6 9.4 19 9.6 18.4C9.2 16.2 8.8 13 7.5 10.8Z" /></g></svg>;
}

function SshConnectionIcon() {
  return <svg className="dashboard-action-icon" viewBox="0 0 24 24" aria-hidden="true"><rect x="3.5" y="4" width="17" height="16" rx="2" /><path d="m7.5 9 2.5 2.5L7.5 14M12.5 14h4" /></svg>;
}

function SettingsIcon() {
  // 齿轮为单线描边路径（8 齿 + 独立轮毂圆），1.8 描边下齿间不糊连。
  return <svg className="dashboard-action-icon" viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="2.9" /><path d="M20.17 10.56A8.3 8.3 0 0 1 20.17 13.44L17.51 12.97A5.6 5.6 0 0 1 16.59 15.21L18.80 16.76A8.3 8.3 0 0 1 16.76 18.80L15.21 16.59A5.6 5.6 0 0 1 12.97 17.51L13.44 20.17A8.3 8.3 0 0 1 10.56 20.17L11.03 17.51A5.6 5.6 0 0 1 8.79 16.59L7.24 18.80A8.3 8.3 0 0 1 5.20 16.76L7.41 15.21A5.6 5.6 0 0 1 6.49 12.97L3.83 13.44A8.3 8.3 0 0 1 3.83 10.56L6.49 11.03A5.6 5.6 0 0 1 7.41 8.79L5.20 7.24A8.3 8.3 0 0 1 7.24 5.20L8.79 7.41A5.6 5.6 0 0 1 11.03 6.49L10.56 3.83A8.3 8.3 0 0 1 13.44 3.83L12.97 6.49A5.6 5.6 0 0 1 15.21 7.41L16.76 5.20A8.3 8.3 0 0 1 18.80 7.24L16.59 8.79A5.6 5.6 0 0 1 17.51 11.03L20.17 10.56Z" /></svg>;
}

function ImportProjectIcon() {
  return <svg className="dashboard-action-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M3.5 7.5h6l1.8 2h9.2v8.2a2 2 0 0 1-2 2h-13a2 2 0 0 1-2-2V7.5Z" /><path d="M15.5 12.5v5M13 15h5" /></svg>;
}

function RemotePairingIcon() {
  return <svg className="dashboard-action-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M4 4h6v6H4zM14 4h6v6h-6zM4 14h6v6H4zM14 14h2v2h-2zM18 14h2v2h-2zM14 18h6v2h-6z" /></svg>;
}

// MCP 用「插头」意象：外部工具通过 MCP 接入。
function McpIcon() {
  return <svg className="dashboard-action-icon" viewBox="0 0 24 24" aria-hidden="true"><rect x="4" y="4" width="7" height="7" rx="1.5" /><rect x="13" y="13" width="7" height="7" rx="1.5" /><path d="M11 7.5h4.5a1.5 1.5 0 0 1 1.5 1.5V11M13 16.5H8.5A1.5 1.5 0 0 1 7 15v-2" /></svg>;
}

function CliToolsIcon() {
  return <svg className="dashboard-action-icon" viewBox="0 0 24 24" aria-hidden="true"><rect x="3.5" y="5" width="17" height="14" rx="2" /><path d="M7.5 10.5 10 13l-2.5 2.5M12.5 15.5h4" /></svg>;
}

export default function DashboardPage() {
  const { projects, projectStatuses, projectsLoading, projectsLoaded, projectsLoadError, error, setError, refreshProjects, removeProject, api } = useProjectContext();
  const desktop = isDesktop();
  const processStatuses = useProcessStatusMap();
  const navigate = useNavigate();
  const [filter, setFilter] = useState<ProjectFilter>("all");
  const [query, setQuery] = useState("");
  const [deleteTarget, setDeleteTarget] = useState<Project | null>(null);
  const [deleting, setDeleting] = useState(false);

  useEffect(() => {
    void refreshProjects().catch(() => undefined);
    const mounted = { current: true };
    // WS 事件驱动为主：数据变化时由 /ws/events 立即触发刷新；固定轮询降为 30s
    // 兜底，只覆盖事件丢失 / WS 断连的窗口，避免状态未变化时仍每 10s 发请求。
    const interval = window.setInterval(() => { if (mounted.current) void refreshProjects().catch(() => undefined); }, 30_000);
    return () => { mounted.current = false; window.clearInterval(interval); };
  }, [refreshProjects]);

  const onRealtime = useCallback(() => { void refreshProjects().catch(() => undefined); }, [refreshProjects]);
  useLiveStateEventsFor("projects", undefined, onRealtime);
  useLiveStateEventsFor("all", undefined, onRealtime);

  // 首屏三态：projects 为空可能只是"还没拉到"或"拉失败了"，不能一律当成"没有项目"。
  // 失败态一旦成立就持续显示（即使后台 30s 兜底轮询正在重试），避免错误面板与空态来回闪。
  const showInitialLoading = !projectsLoaded && !projectsLoadError && projectsLoading;
  const showLoadFailed = !projectsLoaded && Boolean(projectsLoadError);
  const retryLoad = useCallback(() => { void refreshProjects().catch(() => undefined); }, [refreshProjects]);

  type ProjectState = "running" | "ready" | "offline";
  // 状态判定必须与筛选共用同一个函数：先前标签数用 agentReady 单独统计，
  // 会把"正在跑任务但工具暂时不可用"的项目同时算进「不可用」，
  // 而点进去却按「进行中」归类，导致标签数字比列表实际条目多。
  const projectState = (project: Project): ProjectState => projectStatuses[project.id]?.running ? "running" : project.agentReady ? "ready" : "offline";
  const countByState = (state: ProjectState) => projects.filter((project) => projectState(project) === state).length;
  const runningCount = countByState("running");
  const readyCount = countByState("ready");
  const offlineCount = countByState("offline");

  // 卡片顺序固定，由 localStorage 持久化，不随运行状态变化。
  // orderVersion 用于在拖拽重排后触发重新计算（localStorage 变化本身不会触发渲染）。
  const [orderVersion, setOrderVersion] = useState(0);
  const projectById = useMemo(() => new Map(projects.map((project) => [project.id, project])), [projects]);
  const orderedIds = useMemo(() => sortProjectIds(projects.map((project) => project.id)), [projects, orderVersion]);
  // 排除掉已删除但仍残留在 localStorage 中的 id。
  const orderedProjects = orderedIds.map((id) => projectById.get(id)).filter((project): project is Project => Boolean(project));
  const normalizedQuery = query.trim().toLowerCase();
  const filteredProjects = orderedProjects.filter((project) =>
    (filter === "all" || projectState(project) === filter) && project.name.toLowerCase().includes(normalizedQuery));

  // 把当前实际展示的顺序同步回存储：吸纳新增项目、清理已删除项目。
  // 放在副作用里执行，避免渲染期写 localStorage（StrictMode 下渲染会双调用）。
  // 仅在 projects 非空时写入——首次挂载或加载失败时 projects 暂为空，
  // 此时写入会误清用户已保存的顺序。
  useEffect(() => { if (projects.length > 0) persistOrder(orderedIds); }, [orderedIds, projects.length]);

  // ---- 拖拽重排 ----
  const dragIdRef = useRef<string | null>(null);
  const [draggingId, setDraggingId] = useState<string | null>(null);
  const [dropTargetId, setDropTargetId] = useState<string | null>(null);

  // 拖到视口上下边缘时自动滚动。
  // 浏览器自带的拖拽滚动指望不上：实测它只在指针贴住视口最外侧十几像素时才动
  // （离底边 10px 会滚、20px 就完全不动），而这里卡片上方压着一条 76px 高的 sticky 页头 ——
  // 指针停在页头上就不滚了，项目一多就变成「只能拖到屏幕中间，松手、滚一屏、再拖第二次」。
  // 所以自己接管：dragover 只负责记指针坐标（因此挂在 <main> 上，页头也算数），
  // 真正的位移交给每帧循环，免得滚动速度被 dragover 的派发频率带着抖。
  const dragPointerXRef = useRef(0);
  const dragPointerYRef = useRef(0);
  const autoScrollFrameRef = useRef(0);
  /** 这次拖拽有没有真的滚过页面：决定松手时要不要补一次落点（见 handleDragEnd）。 */
  const didAutoScrollRef = useRef(false);

  // 落点高亮的 ref 镜像：自动滚动时每帧都要和指针下的卡片比对一次，
  // 只靠 state 读不到「当前值」，而且每次都 setState 会白渲染一帧。
  const dropTargetIdRef = useRef<string | null>(null);
  const setDropTarget = useCallback((id: string | null) => {
    if (dropTargetIdRef.current === id) return;
    dropTargetIdRef.current = id;
    setDropTargetId(id);
  }, []);

  const stopAutoScroll = useCallback(() => {
    if (autoScrollFrameRef.current) cancelAnimationFrame(autoScrollFrameRef.current);
    autoScrollFrameRef.current = 0;
  }, []);

  /** 指针底下那张卡（不含正被拖走的那张）。 */
  const cardIdUnderPointer = useCallback(() => {
    const element = document.elementFromPoint(dragPointerXRef.current, dragPointerYRef.current);
    const card = element instanceof Element ? element.closest("[data-project-id]") : null;
    const id = card?.getAttribute("data-project-id") ?? null;
    return id && id !== dragIdRef.current ? id : null;
  }, []);

  /**
   * 按指针位置重新认一次落点卡片。
   * 必须自己算：程序化滚动之后浏览器不会重新派发 dragover（实测滚 500px 后事件还停在原来那张卡），
   * 指针没动、卡片却已经在指针底下换了一张。不补这一下，高亮会留在滚走的卡上，
   * 而松手时浏览器按指针下的新卡片落点 —— 看到的位置和实际落点对不上，比没有高亮更糟。
   */
  const syncDropTargetFromPointer = useCallback(() => {
    setDropTarget(cardIdUnderPointer());
  }, [cardIdUnderPointer, setDropTarget]);

  const startAutoScroll = useCallback(() => {
    stopAutoScroll();
    const step = () => {
      // dragend / drop 会把 dragIdRef 清空，循环就此停在这一帧，不需要额外的停止信号。
      if (!dragIdRef.current) {
        autoScrollFrameRef.current = 0;
        return;
      }
      const delta = autoscrollDelta(dragPointerYRef.current, window.innerHeight);
      if (delta) {
        // 只有页面真的动了，指针底下的卡片才会换人。已经顶到最上/最下（滚不动）时不许记这一笔，
        // 否则松手兜底会把「其实什么都没变」也当成一次重排。
        const before = window.scrollY;
        window.scrollBy(0, delta);
        if (window.scrollY !== before) didAutoScrollRef.current = true;
        // 只在真的滚了之后重量：指针自己在动的时候，dragover 会把落点带上来。
        syncDropTargetFromPointer();
      }
      autoScrollFrameRef.current = requestAnimationFrame(step);
    };
    autoScrollFrameRef.current = requestAnimationFrame(step);
  }, [stopAutoScroll, syncDropTargetFromPointer]);

  // 组件卸载时循环必须停：留着它会拿着过期的指针坐标一直滚页面。
  useEffect(() => stopAutoScroll, [stopAutoScroll]);

  const trackDragPointer = useCallback((event: DragEvent) => {
    dragPointerXRef.current = event.clientX;
    dragPointerYRef.current = event.clientY;
  }, []);

  const handleDragStart = useCallback((event: DragEvent, id: string) => {
    dragIdRef.current = id;
    didAutoScrollRef.current = false;
    setDraggingId(id);
    setDropTarget(null);
    trackDragPointer(event);
    startAutoScroll();
    event.dataTransfer.effectAllowed = "move";
    // Firefox 等浏览器需要设置 data 才能触发拖拽。
    event.dataTransfer.setData("text/plain", id);
  }, [startAutoScroll, setDropTarget, trackDragPointer]);

  const handleDragOver = useCallback((event: DragEvent, id: string) => {
    if (!dragIdRef.current || dragIdRef.current === id) return;
    event.preventDefault();
    event.dataTransfer.dropEffect = "move";
    setDropTarget(id);
  }, [setDropTarget]);

  const handleDrop = useCallback((event: DragEvent, id: string) => {
    event.preventDefault();
    stopAutoScroll();
    const fromId = dragIdRef.current;
    if (!fromId || fromId === id) {
      dragIdRef.current = null;
      setDraggingId(null);
      setDropTarget(null);
      return;
    }
    moveProject(orderedIds, fromId, id);
    dragIdRef.current = null;
    setDraggingId(null);
    setDropTarget(null);
    setOrderVersion((v) => v + 1);
  }, [orderedIds, stopAutoScroll, setDropTarget]);

  const handleDragEnd = useCallback(() => {
    stopAutoScroll();
    // 走到这里 dragIdRef 还留着 id，说明这次松手**没有**收到过 drop（handleDrop 会清掉它）。
    // 原因是自动滚动把指针底下的卡片换了一张、指针自己却没动：浏览器只认「松手前最后一次
    // dragover 落在谁身上」，于是原地松手时 drop 一次都不派发（实测：只有 dragend，
    // dropEffect=none）。这种情况按指针底下那张卡补一次落点 —— 屏幕上一直亮着的就是它。
    // 只在真的滚过页面时才补：没滚过的松手（Esc 取消、拖出窗口）保持原来的「什么都不做」。
    const fromId = dragIdRef.current;
    const toId = fromId && didAutoScrollRef.current ? cardIdUnderPointer() : null;
    didAutoScrollRef.current = false;
    dragIdRef.current = null;
    setDraggingId(null);
    setDropTarget(null);
    if (!fromId || !toId) return;
    moveProject(orderedIds, fromId, toId);
    setOrderVersion((v) => v + 1);
  }, [cardIdUnderPointer, orderedIds, setDropTarget, stopAutoScroll]);

  const filters: { id: ProjectFilter; label: string; count: number }[] = [{ id: "all", label: "全部", count: projects.length }, { id: "running", label: "进行中", count: runningCount }, { id: "ready", label: "已就绪", count: readyCount }, { id: "offline", label: "不可用", count: offlineCount }];

  const confirmDelete = async () => {
    if (!deleteTarget || deleting) return;
    setDeleting(true);
    try {
      await api(`/api/projects/${deleteTarget.id}`, { method: "DELETE" });
      // 立即从内存列表移除，卡片马上消失，不依赖后续网络刷新收敛。
      removeProject(deleteTarget.id);
      setDeleteTarget(null);
      // 后台再拉一次做最终校准，即便被并发轮询的竞态覆盖也不影响即时反馈。
      void refreshProjects();
    } catch (cause) {
      toast.error(cause instanceof Error ? cause.message : "无法删除项目");
    } finally {
      setDeleting(false);
    }
  };

  // onDragOver 挂在 <main>（不是 grid）上：拖拽途中指针常常落在页头或留白里，
  // 只有挂在整页上才记得到那些位置的纵坐标，贴边滚动才不会在半路「断线」。
  return <main className="app-shell" onDragOver={trackDragPointer}>
    {error && <div className="error"><span>{error}</span><button title="关闭错误提示" onClick={() => setError("")}>x</button></div>}
    <header className={`dashboard-bar${desktop ? " dashboard-bar-fixed" : ""}`}>
      <a className="brand" href="/"><img className="brand-mark" src="/milevia-mark.svg" width="42" height="42" alt="" /><span className="brand-word"><strong>Mile</strong><em>via</em></span></a>
      <div className="dashboard-actions">
        {/* 方案 A 图标化降噪：桌面端（>820px）次要入口只露图标（.action-label 由 CSS 收起，
            title 承担悬停提示）；≤820px 标签重新显示，触屏上不依赖悬停。 */}
        <NotificationCenter />
        <button type="button" className="dashboard-action dashboard-action-settings secondary" title="设置" aria-label="设置" onClick={() => navigate("/settings")}><SettingsIcon /></button>
        <button type="button" className="dashboard-action dashboard-action-remote secondary" title="远程控制" aria-label="远程控制" onClick={() => navigate("/mobile")}><RemotePairingIcon /><span className="action-label">远程控制</span></button>
        <button className="dashboard-action dashboard-action-ssh secondary" title="SSH连接" aria-label="SSH连接" onClick={() => navigate("/ssh-manager")}><SshConnectionIcon /><span className="action-label">SSH连接</span></button>
        <button className="dashboard-action dashboard-action-mcp secondary" title="MCP连接" aria-label="MCP连接" onClick={() => navigate("/mcp-manager")}><McpIcon /><span className="action-label">MCP连接</span></button>
        <button className="dashboard-action dashboard-action-cli-tools secondary" title="Cli管理" aria-label="Cli管理" onClick={() => navigate("/cli-tools")}><CliToolsIcon /><span className="action-label">Cli管理</span></button>
        <button className="dashboard-action dashboard-action-import primary" onClick={() => navigate("/projects/import")}><ImportProjectIcon /><span>加载项目</span></button>
      </div>
    </header>
    <section className="project-dashboard command-dashboard">
      <header className="dashboard-intro">
        <div><h1>项目<em>总览</em></h1></div>
        {/* 搜索框与状态筛选同组靠右（B2 方案）：标题独占左侧，「搜索 + 筛选」同属过滤工具。 */}
        <div className="dashboard-tools">
          <label className="dashboard-search">
            <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M11 4a7 7 0 1 0 0 14 7 7 0 0 0 0-14Z" /><path d="m16 16 4 4" /></svg>
            <input type="search" value={query} placeholder="按项目名称搜索…" aria-label="按项目名称搜索" onChange={(event) => setQuery(event.target.value)} />
          </label>
          {query && <button className="secondary" type="button" onClick={() => setQuery("")}>清除搜索</button>}
          <div className="dashboard-summary" aria-label="按项目状态筛选">
            {filters.map((item) => <button key={item.id} className={`${filter === item.id ? "selected " : ""}summary-${item.id}`} aria-pressed={filter === item.id} onClick={() => setFilter(item.id)}><b>{projectsLoaded ? item.count : "—"}</b><small>{item.label}</small></button>)}
          </div>
        </div>
      </header>
      {projects.length === 0
        ? showInitialLoading ? <ProjectGridSkeleton />
          : showLoadFailed ? <div className="empty load-failed">
            <h2>项目列表加载失败</h2>
            <p>{projectsLoadError}</p>
            <p className="load-failed-hint">项目数据没有被删除，只是这次没读到控制服务。恢复后重试即可。</p>
            <button className="primary" type="button" disabled={projectsLoading} onClick={retryLoad}>{projectsLoading ? "重试中…" : "重试"}</button>
          </div>
            : <div className="empty"><h2>还没有项目</h2><p>加载一个本地目录后，即可在这里开始 AI 工具对话。</p><button className="primary" onClick={() => navigate("/projects/import")}>加载项目</button></div>
        : filteredProjects.length === 0 ? (() => {
          const current = filters.find((item) => item.id === filter);
          const others = filters.filter((item) => item.id !== filter && item.id !== "all" && item.count > 0);
          return <div className="empty filtered-empty">
            <span className="filtered-empty-icon" aria-hidden="true"><svg viewBox="0 0 24 24"><path d="M4 6h16M4 12h16M4 18h10" /><circle cx="18.5" cy="17.5" r="3.5" /><path d="M18.5 16v1.5l1 1" /></svg></span>
            <h2>{normalizedQuery ? "没有匹配的项目" : `暂无${current?.label ?? ""}项目`}</h2>
            <p>{normalizedQuery ? "当前名称搜索和状态筛选下没有匹配的项目，请更换关键词、清除搜索或切换状态。" : `当前筛选下没有匹配的项目。${others.length ? "试试切换到下方其他状态，或查看全部项目。" : "切换到全部项目查看所有任务。"}`}</p>
            {!normalizedQuery && others.length > 0 && <div className="filtered-empty-suggest">{others.map((item) => <button key={item.id} type="button" className="filtered-empty-chip" onClick={() => setFilter(item.id)}><small>{item.label}</small><b>{item.count}</b></button>)}</div>}
            <button className="primary" type="button" onClick={() => { setFilter("all"); setQuery(""); }}>查看全部项目</button>
          </div>;
        })()
          : <div className="project-grid">{filteredProjects.map((item) => <ProjectCard key={item.id} project={item} status={projectStatuses[item.id]} runStatus={(processStatuses[item.id]?.runStatus ?? "stopped") as RunStatus} runStartedAt={processStatuses[item.id]?.runStartedAt ?? null} open={() => navigate(`/projects/${item.id}`)} onDelete={() => setDeleteTarget(item)} onDragStart={handleDragStart} onDragOver={handleDragOver} onDrop={handleDrop} onDragEnd={handleDragEnd} isDragging={draggingId === item.id} isDropTarget={dropTargetId === item.id} />)}</div>}
    </section>
    {desktop && <div className="dashboard-version"><AppVersionTag /></div>}
    {/* 法定展示项：备案号必须在网站底部可见，不能只在设置里。 */}
    <IcpFooter />
    {deleteTarget && <DeleteProjectDialog project={deleteTarget} status={projectStatuses[deleteTarget.id]} busy={deleting} close={() => setDeleteTarget(null)} confirm={confirmDelete} />}
  </main>;
}

// 首屏占位：与 .project-card 同构的骨架卡。此前首屏直接渲染"还没有项目"，
// 加载慢或失败时会被误读成项目丢失，进而引导用户重复添加。
function ProjectGridSkeleton() {
  return <div className="project-grid" role="status" aria-label="正在加载项目列表">
    {[0, 1, 2].map((index) => <div className="project-card project-skeleton-card" key={index} aria-hidden="true">
      <span className="project-skeleton-band"><i /></span>
      <span className="project-skeleton-title" />
      <span className="project-skeleton-line" />
      <span className="project-skeleton-line short" />
      <span className="project-skeleton-footer" />
    </div>)}
  </div>;
}

function ProjectCard({ project, status, runStatus, runStartedAt, open, onDelete, onDragStart, onDragOver, onDrop, onDragEnd, isDragging, isDropTarget }: {
  project: Project;
  status?: ProjectStatus;
  runStatus: RunStatus;
  runStartedAt?: string | null;
  open: () => void;
  onDelete: () => void;
  onDragStart: (event: DragEvent, id: string) => void;
  onDragOver: (event: DragEvent, id: string) => void;
  onDrop: (event: DragEvent, id: string) => void;
  onDragEnd: () => void;
  isDragging: boolean;
  isDropTarget: boolean;
}) {
  const state = status?.running ? "正在执行" : project.agentReady ? "已就绪" : "工具不可用";
  const stateClass = status?.running ? "running" : project.agentReady ? "ready" : "offline";
  // 优化建议分析中：项目总览卡片上的闪烁状态（与对话执行状态相互独立）。
  const analyzing = status?.insightsRunning === true;
  const analysisTitle = status?.insightsMessage || "正在优化建议分析…";
  const taskTitle = status?.running && status.activeTitle
    ? status.activeTitle
    : analyzing
      ? `优化建议分析：${analysisTitle}`
      : "等待新的任务";
  const dragClass = isDragging ? " dragging" : isDropTarget ? " drop-target" : "";
  // 目录名含下划线：标题改等宽渲染，避免 `_` 被正文无衬线字体弱化成“看起来像空格”。
  const nameLiteral = literalNameClass(project.name);
  // 开发进程状态的可视化分工：running 且能算出时长 → 色带右侧展示「进程已运行 X 分钟」；
  // 其余情况（启动中/停止中/失败/时长算不出）回退终端小徽标——运行中的事实不许双双消失。
  const runDuration = runStatus === "running" && runStartedAt ? runDurationLabel(runStartedAt) : "";
  const runBadgeVisible = runStatus !== "stopped" && !runDuration;
  // WSL 项目挂在 \\wsl$\<发行版> 下，卡片上方已有企鹅图标标明环境，这里只显示 Linux 侧路径。
  const workspacePath = workspacePathForDisplay(project.fullPath);
  // data-project-id 是拖拽时「指针底下是哪张卡」的唯一判据：自动滚动时浏览器不再派发 dragover，
  // 只能自己用 elementFromPoint 认卡（见 syncDropTargetFromPointer）。
  return <article className={`project-card task-card state-${stateClass}${dragClass}`} draggable data-project-id={project.id} onClick={open}
    onDragStart={(event) => onDragStart(event, project.id)}
    onDragOver={(event) => onDragOver(event, project.id)}
    onDrop={(event) => onDrop(event, project.id)}
    onDragEnd={onDragEnd}>
    <button className="project-card-open" type="button" draggable={false} onClick={(event) => { event.stopPropagation(); open(); }} aria-label={`进入 ${project.name} 的对话`} />
    <span className="project-mark" aria-hidden="true">{project.name.slice(0, 1).toUpperCase()}</span>
    <div className="project-band">
      <span className="project-band-status"><i></i>{state}</span>
      {runDuration && <span className="project-band-duration" title="开发进程运行时长">{`进程${runDuration}`}</span>}
      {project.environment === "windows" && <span className="project-band-env" title="Windows 项目" role="img" aria-label="Windows 项目"><WindowsEnvironmentIcon /></span>}
      {project.environment === "remote-linux" && <span className="project-band-env" title="远程服务器项目" role="img" aria-label="远程服务器项目"><RemoteServerIcon /></span>}
      {project.environment === "wsl" && <span className="project-band-env" title="WSL 项目" role="img" aria-label="WSL 项目"><WslEnvironmentIcon /></span>}
      <button className="project-card-delete" type="button" draggable={false} title="删除项目" onClick={(event) => { event.stopPropagation(); onDelete(); }} aria-label={`删除项目 ${project.name}`}>×</button>
    </div>
    <div className="project-card-body">
      <div className="project-card-title">
        <div className="project-card-heading">
          <h2 title={nameLiteral ? project.name : undefined} className={nameLiteral}>{project.name}</h2>
          {analyzing && <span className="project-analysis-badge" title={analysisTitle}><i></i>优化建议分析中</span>}
          {runBadgeVisible && <span className={`project-run-state run-state-${runStatus}`} title={`开发进程: ${statusLabels[runStatus]}`}><svg className="project-run-state-icon" viewBox="0 0 16 16" aria-hidden="true"><path d="M1.5 3.5 6 8l-4.5 4.5M7.5 12.5h7" /></svg><i style={{ background: statusColors[runStatus] }}></i>{statusLabels[runStatus]}</span>}
        </div>
        <p>{taskTitle}</p>
      </div>
      <div className="project-card-meta"><span><small>会话数</small><b>{status ? status.conversationCount : "--"}</b></span><span title={project.fullPath}><small>工作目录</small><b className={literalNameClass(workspacePath)}>{workspacePath}</b></span></div>
      <footer><span>{project.gitBranch || "非 Git 项目"}</span><b aria-hidden="true">打开对话<i>→</i></b></footer>
    </div>
  </article>;
}

function DeleteWarnIcon() {
  return <svg className="delete-project-warn-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M10.3 3.9 2.7 17.2A2 2 0 0 0 4.4 20h15.2a2 2 0 0 0 1.7-2.8L13.7 3.9a2 2 0 0 0-3.4 0Z" /><path d="M12 9v4.5M12 17.5h.01" /></svg>;
}

function DeleteTrashIcon() {
  return <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 7h16M9 7V5h6v2M6 7l1 13h10l1-13M10 11v5M14 11v5" /></svg>;
}

function DeleteProjectDialog({ project, status, busy, close, confirm }: { project: Project; status?: ProjectStatus; busy: boolean; close: () => void; confirm: () => Promise<void> }) {
  // 环境标签：让用户一眼确认删除的是哪一类项目（本地 Windows / WSL / 远端服务器）。
  const envLabel = project.environment === "windows" ? "Windows"
    : project.environment === "wsl" ? "WSL"
      : project.environment === "remote-linux" ? "Remote"
        : project.environment;
  const envIcon = project.environment === "windows" ? <WindowsEnvironmentIcon />
    : project.environment === "wsl" ? <WslEnvironmentIcon />
      : <RemoteServerIcon />;
  const conversationCount = status?.conversationCount ?? 0;
  return (
    <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="delete-project-title" onClick={(event) => { if (event.target === event.currentTarget && !busy) close(); }}>
      <section className="modal dialog-studio delete-project-dialog">
        <header>
          <div className="delete-project-heading">
            <span className="delete-project-mark"><DeleteTrashIcon /></span>
            <div className="delete-project-heading-text">
              <h2 id="delete-project-title">删除项目</h2>
            </div>
          </div>
          <button type="button" className="delete-project-close" title="关闭" aria-label="关闭" disabled={busy} onClick={close}><svg viewBox="0 0 24 24" aria-hidden="true"><path d="m7 7 10 10M17 7 7 17" /></svg></button>
        </header>
        <div className="delete-project-body">
          <p className="delete-project-lead"><DeleteWarnIcon /><span>即将删除项目 <b>{project.name}</b>，其下所有会话历史、任务、运行记录将一并清除。此操作<b>不可撤销</b>。</span></p>
          <div className="delete-project-chip">
            <span className="delete-project-chip-mark">{project.name.slice(0, 1).toUpperCase()}</span>
            <div className="delete-project-chip-body">
              <span className="delete-project-chip-path" title={project.fullPath}>{project.pathDisplay || project.fullPath}</span>
              <span className="delete-project-chip-meta">
                {project.environment && <span className="delete-project-chip-env" title={`${envLabel} 项目`} role="img" aria-label={`${envLabel} 项目`}>{envIcon}{envLabel}</span>}
                {conversationCount > 0 && <span className="delete-project-chip-count">{`已建立 ${conversationCount} 个会话`}</span>}
              </span>
            </div>
          </div>
          <div className="delete-project-effects">
            <div className="delete-project-effects-title"><DeleteWarnIcon />删除后将移除此项目的数据：</div>
            <ul className="delete-project-effects-list">
              <li>与该项目的全部对话历史记录</li>
              <li>任务队列、运行记录与优化建议分析数据</li>
              <li>项目本身及其工作列表条目</li>
            </ul>
            <p className="delete-project-effects-keep"><b>项目源文件不受影响</b>：仅移除 Milevia 内的数据，目录中的代码文件不会被删除。</p>
          </div>
        </div>
        <footer className="delete-project-footer">
          <button type="button" className="secondary" disabled={busy} onClick={close}>取消</button>
          <button type="button" className="primary danger" disabled={busy} onClick={() => void confirm()}>{busy ? "删除中…" : "确认删除"}</button>
        </footer>
      </section>
    </div>
  );
}
