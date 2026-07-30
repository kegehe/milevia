// 项目列表页 — 从 App.tsx ProjectDashboard 提取

import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useProjectContext } from "../stores/useProjectStore";
import NotificationCenter from "../components/NotificationCenter";
import type { Project, ProjectFilter, ProjectStatus } from "../lib/types";

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
  const filteredProjects = projects.filter((project) => filter === "all" || projectState(project) === filter).sort((left, right) => {
    const order: Record<ProjectFilter, number> = { running: 0, ready: 1, offline: 2, all: 3 };
    return order[projectState(left)] - order[projectState(right)] || left.name.localeCompare(right.name, "zh-CN");
  });
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
        : filteredProjects.length === 0 ? <div className="empty filtered-empty"><h2>没有匹配的项目</h2><p>当前筛选下没有项目，可切换筛选查看其他任务。</p></div>
          : <div className="project-grid">{filteredProjects.map((item) => <ProjectCard key={item.id} project={item} status={projectStatuses[item.id]} open={() => navigate(`/projects/${item.id}`)} onDelete={() => setDeleteTarget(item)} />)}</div>}
    </section>
    {deleteTarget && <DeleteProjectDialog project={deleteTarget} busy={deleting} close={() => setDeleteTarget(null)} confirm={confirmDelete} />}
  </main>;
}

function ProjectCard({ project, status, open, onDelete }: { project: Project; status?: ProjectStatus; open: () => void; onDelete: () => void }) {
  const state = status?.running ? "正在执行" : project.agentReady ? "已就绪" : "工具不可用";
  const stateClass = status?.running ? "running" : project.agentReady ? "ready" : "offline";
  const taskTitle = status?.running && status.activeTitle ? status.activeTitle : "等待新的任务";
  return <article className={`project-card task-card state-${stateClass}`} onClick={open}>
    <button className="project-card-open" type="button" onClick={(event) => { event.stopPropagation(); open(); }} aria-label={`进入 ${project.name} 的对话`} />
    <div className="project-card-top">
      <span className="project-mark">{project.name.slice(0, 1).toUpperCase()}</span>
      <span className="project-card-top-actions">
        <span className={`project-state ${stateClass}`}><i></i>{state}</span>
        {project.environment === "windows" && <span className="env-tag windows" title="Windows 项目" role="img" aria-label="Windows 项目"><WindowsEnvironmentIcon /></span>}
        {project.environment === "remote-linux" && <span className="env-tag remote" title="远程服务器" role="img" aria-label="远程服务器"><RemoteServerIcon /></span>}
        <button className="project-card-delete" type="button" title="删除项目" onClick={(event) => { event.stopPropagation(); onDelete(); }} aria-label={`删除项目 ${project.name}`}>×</button>
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
