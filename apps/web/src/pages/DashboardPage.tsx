// 项目列表页 — 从 App.tsx ProjectDashboard 提取

import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useProjectContext } from "../stores/useProjectStore";
import NotificationCenter from "../components/NotificationCenter";
import type { Project, ProjectFilter, ProjectStatus } from "../lib/types";

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
      <a className="brand" href="/"><b>A</b><span>Auto<br />Development</span></a>
      <div className="dashboard-actions">
        <NotificationCenter />
        <button className="secondary" onClick={() => navigate("/ssh-manager")}>SSH 连接</button>
        <button className="primary" onClick={() => navigate("/projects/import")}>加载项目 <i>+</i></button>
      </div>
    </header>
    <section className="project-dashboard command-dashboard">
      <header className="dashboard-intro">
        <div><h1>项目<em>总览</em></h1></div>
        <div className="dashboard-summary" aria-label="按项目状态筛选">
          {filters.map((item) => <button key={item.id} className={`${filter === item.id ? "selected " : ""}${item.id === "running" ? "summary-running" : ""}`} aria-pressed={filter === item.id} onClick={() => setFilter(item.id)}><small>{item.label}</small><b>{item.count}</b></button>)}
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
        {project.environment === "windows" && <span className="env-tag windows" title="Windows 项目">🪟</span>}
        {project.environment === "remote-linux" && <span className="env-tag remote" title="远程服务器">🖥️</span>}
      </span>
    </div>
    <div className="project-card-title"><span>项目</span><h2>{project.name}</h2><p>{taskTitle}</p></div>
    <div className="project-card-meta"><span><small>会话</small><b>{status ? status.conversationCount : "--"}</b></span><span><small>工作目录</small><b title={project.pathDisplay}>{project.pathDisplay}</b></span></div>
    <footer><span>{project.gitBranch || "非 Git 项目"}</span><b aria-hidden="true">打开对话 <i>→</i></b></footer>
    <button className="project-card-delete" type="button" title="删除项目" onClick={(event) => { event.stopPropagation(); onDelete(); }} aria-label={`删除项目 ${project.name}`}>×</button>
  </article>;
}

function DeleteProjectDialog({ project, busy, close, confirm }: { project: Project; busy: boolean; close: () => void; confirm: () => Promise<void> }) {
  return <div className="backdrop" role="dialog" aria-modal="true" aria-labelledby="delete-project-title"><section className="modal"><header><div><label>DELETE PROJECT</label><h2 id="delete-project-title">删除项目</h2></div><button title="关闭" disabled={busy} onClick={close}>x</button></header><p className="permission-confirmation">确定要从列表中删除项目 <b>{project.name}</b> 吗？此操作会同时删除该项目下的所有对话历史、任务、运行记录及相关数据。项目源文件不会被删除。</p><footer><button className="secondary" disabled={busy} onClick={close}>取消</button><button className="primary danger" disabled={busy} onClick={() => void confirm()}>{busy ? "删除中" : "确认删除"}</button></footer></section></div>;
}