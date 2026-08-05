// 项目列表页 — 从 App.tsx ProjectDashboard 提取

import { useEffect, useState, useRef, useCallback, useMemo, type DragEvent } from "react";
import { useNavigate } from "react-router-dom";
import { useProjectContext } from "../stores/useProjectStore";
import NotificationCenter from "../components/NotificationCenter";
import type { Project, ProjectFilter, ProjectStatus } from "../lib/types";
import { sortProjectIds, moveProject, persistOrder } from "../lib/project-order";

function WindowsEnvironmentIcon() {
  return <svg className="env-tag-icon windows" viewBox="0 0 24 24" aria-hidden="true"><path d="M3 5.5 10.5 4v7H3v-5.5ZM13 3.5 21 2v9h-8v-7.5ZM3 13h7.5v7L3 18.5V13ZM13 13h8v9l-8-1.5V13Z" /></svg>;
}

function RemoteServerIcon() {
  return <svg className="env-tag-icon remote" viewBox="0 0 24 24" aria-hidden="true"><rect x="4" y="4" width="16" height="6" rx="1.2" /><rect x="4" y="14" width="16" height="6" rx="1.2" /><path d="M8 7h.01M8 17h.01M12 7h5M12 17h5" /></svg>;
}

function SshConnectionIcon() {
  return <svg className="dashboard-action-icon" viewBox="0 0 24 24" aria-hidden="true"><rect x="3.5" y="4" width="17" height="16" rx="2" /><path d="m7.5 9 2.5 2.5L7.5 14M12.5 14h4" /></svg>;
}

function ImportProjectIcon() {
  return <svg className="dashboard-action-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M3.5 7.5h6l1.8 2h9.2v8.2a2 2 0 0 1-2 2h-13a2 2 0 0 1-2-2V7.5Z" /><path d="M15.5 12.5v5M13 15h5" /></svg>;
}

export default function DashboardPage() {
  const { projects, projectStatuses, error, setError, refreshProjects, api } = useProjectContext();
  const navigate = useNavigate();
  const [filter, setFilter] = useState<ProjectFilter>("all");
  const [deleteTarget, setDeleteTarget] = useState<Project | null>(null);
  const [deleting, setDeleting] = useState(false);

  useEffect(() => {
    void refreshProjects().catch(() => undefined);
    const mounted = { current: true };
    const interval = window.setInterval(() => { if (mounted.current) void refreshProjects().catch(() => undefined); }, 10_000);
    return () => { mounted.current = false; window.clearInterval(interval); };
  }, [refreshProjects]);

  const runningCount = Object.values(projectStatuses).filter((status) => status.running).length;
  const readyCount = projects.filter((project) => project.agentReady && !projectStatuses[project.id]?.running).length;
  const offlineCount = projects.filter((project) => !project.agentReady).length;
  type ProjectState = "running" | "ready" | "offline";
  const projectState = (project: Project): ProjectState => projectStatuses[project.id]?.running ? "running" : project.agentReady ? "ready" : "offline";

  // 卡片顺序固定，由 localStorage 持久化，不随运行状态变化。
  // orderVersion 用于在拖拽重排后触发重新计算（localStorage 变化本身不会触发渲染）。
  const [orderVersion, setOrderVersion] = useState(0);
  const projectById = useMemo(() => new Map(projects.map((project) => [project.id, project])), [projects]);
  const orderedIds = useMemo(() => sortProjectIds(projects.map((project) => project.id)), [projects, orderVersion]);
  // 排除掉已删除但仍残留在 localStorage 中的 id。
  const orderedProjects = orderedIds.map((id) => projectById.get(id)).filter((project): project is Project => Boolean(project));
  const filteredProjects = orderedProjects.filter((project) => filter === "all" || projectState(project) === filter);

  // 把当前实际展示的顺序同步回存储：吸纳新增项目、清理已删除项目。
  // 放在副作用里执行，避免渲染期写 localStorage（StrictMode 下渲染会双调用）。
  // 仅在 projects 非空时写入——首次挂载或加载失败时 projects 暂为空，
  // 此时写入会误清用户已保存的顺序。
  useEffect(() => { if (projects.length > 0) persistOrder(orderedIds); }, [orderedIds, projects.length]);

  // ---- 拖拽重排 ----
  const dragIdRef = useRef<string | null>(null);
  const [draggingId, setDraggingId] = useState<string | null>(null);
  const [dropTargetId, setDropTargetId] = useState<string | null>(null);

  const handleDragStart = useCallback((event: DragEvent, id: string) => {
    dragIdRef.current = id;
    setDraggingId(id);
    event.dataTransfer.effectAllowed = "move";
    // Firefox 等浏览器需要设置 data 才能触发拖拽。
    event.dataTransfer.setData("text/plain", id);
  }, []);

  const handleDragOver = useCallback((event: DragEvent, id: string) => {
    if (!dragIdRef.current || dragIdRef.current === id) return;
    event.preventDefault();
    event.dataTransfer.dropEffect = "move";
    setDropTargetId(id);
  }, []);

  const handleDrop = useCallback((event: DragEvent, id: string) => {
    event.preventDefault();
    const fromId = dragIdRef.current;
    if (!fromId || fromId === id) {
      dragIdRef.current = null;
      setDraggingId(null);
      setDropTargetId(null);
      return;
    }
    moveProject(orderedIds, fromId, id);
    dragIdRef.current = null;
    setDraggingId(null);
    setDropTargetId(null);
    setOrderVersion((v) => v + 1);
  }, [orderedIds]);

  const handleDragEnd = useCallback(() => {
    dragIdRef.current = null;
    setDraggingId(null);
    setDropTargetId(null);
  }, []);

  const filters: { id: ProjectFilter; label: string; count: number }[] = [{ id: "all", label: "全部", count: projects.length }, { id: "running", label: "进行中", count: runningCount }, { id: "ready", label: "已就绪", count: readyCount }, { id: "offline", label: "不可用", count: offlineCount }];

  const confirmDelete = async () => {
    if (!deleteTarget || deleting) return;
    setDeleting(true);
    try {
      await api(`/api/projects/${deleteTarget.id}`, { method: "DELETE" });
      setDeleteTarget(null);
      void refreshProjects();
    } catch (cause) {
      alert(cause instanceof Error ? cause.message : "无法删除项目");
    } finally {
      setDeleting(false);
    }
  };

  return <main className="app-shell">
    {error && <div className="error"><span>{error}</span><button title="关闭错误提示" onClick={() => setError("")}>x</button></div>}
    <header className="dashboard-bar">
      <a className="brand" href="/"><img className="brand-mark" src="/milevia-mark.svg" width="42" height="42" alt="" /><span className="brand-word"><strong>Mile</strong><em>via</em></span></a>
      <div className="dashboard-actions">
        <NotificationCenter />
        <button className="dashboard-action dashboard-action-ssh secondary" title="SSH连接" onClick={() => navigate("/ssh-manager")}><SshConnectionIcon /><span>SSH连接</span></button>
        <button className="dashboard-action dashboard-action-import primary" onClick={() => navigate("/projects/import")}><ImportProjectIcon /><span>加载项目</span></button>
      </div>
    </header>
    <section className="project-dashboard command-dashboard">
      <header className="dashboard-intro">
        <div><h1>项目<em>总览</em></h1></div>
        <div className="dashboard-summary" aria-label="按项目状态筛选">
          {filters.map((item) => <button key={item.id} className={`${filter === item.id ? "selected " : ""}summary-${item.id}`} aria-pressed={filter === item.id} onClick={() => setFilter(item.id)}><small>{item.label}</small><b>{item.count}</b></button>)}
        </div>
      </header>
      <div className="dashboard-divider"><span>项目列表</span><i></i>{filter !== "all" && <small>{`显示${filters.find((item) => item.id === filter)?.label}项目`}</small>}</div>
      {projects.length === 0 ? <div className="empty"><h2>还没有项目</h2><p>加载一个本地目录后，即可在这里开始 AI 工具对话。</p><button className="primary" onClick={() => navigate("/projects/import")}>加载项目</button></div>
        : filteredProjects.length === 0 ? (() => {
          const current = filters.find((item) => item.id === filter);
          const others = filters.filter((item) => item.id !== filter && item.id !== "all" && item.count > 0);
          return <div className="empty filtered-empty">
            <span className="filtered-empty-icon" aria-hidden="true"><svg viewBox="0 0 24 24"><path d="M4 6h16M4 12h16M4 18h10" /><circle cx="18.5" cy="17.5" r="3.5" /><path d="M18.5 16v1.5l1 1" /></svg></span>
            <h2>{`暂无${current?.label ?? ""}项目`}</h2>
            <p>{`当前筛选下没有匹配的项目。${others.length ? "试试切换到下方其他状态，或查看全部项目。" : "切换到全部项目查看所有任务。"}`}</p>
            {others.length > 0 && <div className="filtered-empty-suggest">{others.map((item) => <button key={item.id} type="button" className="filtered-empty-chip" onClick={() => setFilter(item.id)}><small>{item.label}</small><b>{item.count}</b></button>)}</div>}
            <button className="primary" type="button" onClick={() => setFilter("all")}>查看全部项目</button>
          </div>;
        })()
          : <div className="project-grid">{filteredProjects.map((item) => <ProjectCard key={item.id} project={item} status={projectStatuses[item.id]} open={() => navigate(`/projects/${item.id}`)} onDelete={() => setDeleteTarget(item)} onDragStart={handleDragStart} onDragOver={handleDragOver} onDrop={handleDrop} onDragEnd={handleDragEnd} isDragging={draggingId === item.id} isDropTarget={dropTargetId === item.id} />)}</div>}
    </section>
    {deleteTarget && <DeleteProjectDialog project={deleteTarget} busy={deleting} close={() => setDeleteTarget(null)} confirm={confirmDelete} />}
  </main>;
}

function ProjectCard({ project, status, open, onDelete, onDragStart, onDragOver, onDrop, onDragEnd, isDragging, isDropTarget }: {
  project: Project;
  status?: ProjectStatus;
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
  const taskTitle = status?.running && status.activeTitle ? status.activeTitle : "等待新的任务";
  const dragClass = isDragging ? " dragging" : isDropTarget ? " drop-target" : "";
  return <article className={`project-card task-card state-${stateClass}${dragClass}`} draggable onClick={open}
    onDragStart={(event) => onDragStart(event, project.id)}
    onDragOver={(event) => onDragOver(event, project.id)}
    onDrop={(event) => onDrop(event, project.id)}
    onDragEnd={onDragEnd}>
    <button className="project-card-open" type="button" draggable={false} onClick={(event) => { event.stopPropagation(); open(); }} aria-label={`进入 ${project.name} 的对话`} />
    <div className="project-card-top">
      <span className="project-mark">{project.name.slice(0, 1).toUpperCase()}</span>
      <span className="project-card-top-actions">
        <span className={`project-state ${stateClass}`}><i></i>{state}</span>
        {project.environment === "windows" && <span className="env-tag windows" title="Windows 项目" role="img" aria-label="Windows 项目"><WindowsEnvironmentIcon /></span>}
        {project.environment === "remote-linux" && <span className="env-tag remote" title="远程服务器" role="img" aria-label="远程服务器"><RemoteServerIcon /></span>}
        <button className="project-card-delete" type="button" draggable={false} title="删除项目" onClick={(event) => { event.stopPropagation(); onDelete(); }} aria-label={`删除项目 ${project.name}`}>×</button>
      </span>
    </div>
    <div className="project-card-title"><span>项目</span><h2>{project.name}</h2><p>{taskTitle}</p></div>
    <div className="project-card-meta"><span><small>会话</small><b>{status ? status.conversationCount : "--"}</b></span><span><small>工作目录</small><b title={project.pathDisplay}>{project.pathDisplay}</b></span></div>
    <footer><span>{project.gitBranch || "非 Git 项目"}</span><b aria-hidden="true">打开对话 <i>→</i></b></footer>
  </article>;
}

function DeleteProjectDialog({ project, busy, close, confirm }: { project: Project; busy: boolean; close: () => void; confirm: () => Promise<void> }) {
  return <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="delete-project-title"><section className="modal"><header><div><h2 id="delete-project-title">删除项目</h2></div><button title="关闭" disabled={busy} onClick={close}>x</button></header><p className="permission-confirmation">确定要从列表中删除项目 <b>{project.name}</b> 吗？此操作会同时删除该项目下的所有对话历史、任务、运行记录及相关数据。项目源文件不会被删除。</p><footer><button className="secondary" disabled={busy} onClick={close}>取消</button><button className="primary danger" disabled={busy} onClick={() => void confirm()}>{busy ? "删除中" : "确认删除"}</button></footer></section></div>;
}
