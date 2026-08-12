// 项目页共享布局：header、workspace tabs
// 通过 Outlet context 向子路由传递 project 数据，避免重复加载

import { useEffect, useState, useCallback, type ReactNode } from "react";
import { Outlet, useParams, useNavigate, useLocation } from "react-router-dom";
import NotificationCenter from "./NotificationCenter";
import type { Project, WorkspaceTab } from "../lib/types";
import { api } from "../lib/api";
import { useProjectContext } from "../stores/useProjectStore";

export interface ProjectLayoutOutletContext {
  project: Project;
}

function BackProjectsIcon() {
  return <svg className="back-projects-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M14.5 5.5 8 12l6.5 6.5M8.5 12h8" /></svg>;
}

function ErrorAlertIcon() {
  return <svg className="error-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M12 8.5v4.5M12 16.5h.01M10.3 4.5 2.6 18a2 2 0 0 0 1.74 3h15.32a2 2 0 0 0 1.74-3L13.7 4.5a2 2 0 0 0-3.4 0Z" /></svg>;
}

function WorkspaceTabIcon({ tab }: { tab: WorkspaceTab }) {
  const paths: Record<WorkspaceTab, ReactNode> = {
    conversation: <><path d="M6.5 17.5 3.8 20v-11A3.5 3.5 0 0 1 7.3 5.5h9.4A3.5 3.5 0 0 1 20.2 9v5a3.5 3.5 0 0 1-3.5 3.5H6.5Z" /><path d="M8 11.5h8M8 14.5h5" /></>,
    tasks: <><path d="M8.5 6.5h10M8.5 12h10M8.5 17.5h10" /><path d="m4.7 6.5.9.9 1.8-2M4.7 12l.9.9 1.8-2M4.7 17.5l.9.9 1.8-2" /></>,
    orchestration: <><circle cx="6" cy="6" r="2" /><circle cx="18" cy="12" r="2" /><circle cx="6" cy="18" r="2" /><path d="M8 6h3.5a4.5 4.5 0 0 1 0 9H8M15.8 10.8l-2.3 2.3 2.3 2.3" /></>,
    files: <><path d="M4.5 7.5A2.5 2.5 0 0 1 7 5h3l1.7 2H17A2.5 2.5 0 0 1 19.5 9.5v7A2.5 2.5 0 0 1 17 19H7a2.5 2.5 0 0 1-2.5-2.5v-9Z" /><path d="M4.8 9h14.4" /></>,
    git: <><circle cx="6" cy="6" r="2" /><circle cx="18" cy="18" r="2" /><circle cx="18" cy="6" r="2" /><path d="M8 6h5a3 3 0 0 1 3 3v7M8 6h5a3 3 0 0 1 3 3" /></>,
    run: <><path d="m9 7 7 5-7 5V7Z" /><path d="M5 5.5v13M19 5.5v13" /></>,
    insights: <><path d="M9.5 18.5h5M10 18.5l.6 2.4a.7.7 0 0 0 .68.51h1.44a.7.7 0 0 0 .68-.51L14 18.5" /><path d="M12 3.5a6 6 0 0 0-3 11.18c.73.48 1 1.35 1 2.32v.5h4v-.5c0-.97.27-1.84 1-2.32A6 6 0 0 0 12 3.5Z" /></>,
  };
  return <svg className="workspace-tab-icon" viewBox="0 0 24 24" aria-hidden="true">{paths[tab]}</svg>;
}

export default function ProjectLayout() {
  const { projectId } = useParams<{ projectId: string }>();
  const navigate = useNavigate();
  const location = useLocation();
	const [project, setProject] = useState<Project | null>(null);
	const [loading, setLoading] = useState(true);
	const [error, setError] = useState("");
  const { error: globalError, setError: setGlobalError, refreshStatuses } = useProjectContext();

  // 确定当前工作区标签
  const getWorkspaceTab = useCallback((): WorkspaceTab => {
    const path = location.pathname;
    if (path.endsWith("/tasks") || path.includes("/tasks/")) return "tasks";
    if (path.endsWith("/orchestration")) return "orchestration";
    if (path.endsWith("/files")) return "files";
    if (path.endsWith("/git")) return "git";
    if (path.endsWith("/run")) return "run";
    if (path.endsWith("/insights")) return "insights";
    return "conversation";
  }, [location.pathname]);

  const [workspaceTab, setWorkspaceTab] = useState<WorkspaceTab>(getWorkspaceTab);

  useEffect(() => {
    setWorkspaceTab(getWorkspaceTab());
  }, [getWorkspaceTab]);

  const selectWorkspaceTab = useCallback((tab: WorkspaceTab) => {
    setWorkspaceTab(tab);
    const base = `/projects/${projectId}`;
    switch (tab) {
      case "conversation": navigate(`${base}/conversations`); break;
      case "tasks": navigate(`${base}/tasks`); break;
      case "orchestration": navigate(`${base}/orchestration`); break;
      case "files": navigate(`${base}/files`); break;
      case "git": navigate(`${base}/git`); break;
      case "run": navigate(`${base}/run`); break;
      case "insights": navigate(`${base}/insights`); break;
    }
  }, [projectId, navigate]);

  // 加载项目数据（仅一次，通过 context 共享给子路由）
	// 用单项目接口而非全量 /api/projects：后者会对每个 SSH runner 同步探活，
	// 进入项目时被阻塞数秒。不重置 project 以避免请求期间全屏闪烁；但切换项目时
	// 仍需 setLoading(true) 兜底，否则数据到达前会误显错误页。
	useEffect(() => {
		if (!projectId) return;
		let cancelled = false;
		setError("");
		setLoading(true);
		(async () => {
      try {
        const found = await api<Project>(`/api/projects/${projectId}`);
        if (cancelled) return;
				setProject(found);
      } catch (cause) {
        if (!cancelled) setError(cause instanceof Error ? cause.message : "无法加载项目");
			} finally {
				if (!cancelled) {
					setLoading(false);
				}
			}
    })();
    return () => { cancelled = true; };
  }, [projectId]);

	// 项目页期间持续刷新项目状态（运行中/会话数等），供 FilesPage 等子路由使用。
	// 只调 /api/projects/statuses（纯 SQL 查询），不触发 refreshProjects 的远程探活。
	useEffect(() => {
		void refreshStatuses().catch(() => undefined);
		const interval = window.setInterval(() => { void refreshStatuses().catch(() => undefined); }, 10_000);
		return () => window.clearInterval(interval);
	}, [refreshStatuses]);

	// 当前 project 是否对应当前路由的项目 id。
	// 请求失败且无匹配数据时显示错误页。loading 期间 error 已被清空，不会误触发。
	if (error && (!project || project.id !== projectId)) {
    return <main className="app-shell project-open">
      <div style={{ display: "flex", flexDirection: "column", alignItems: "center", justifyContent: "center", minHeight: "100vh", gap: "16px" }}>
        <p style={{ color: "#d14233", fontSize: "14px" }}>{error}</p>
        <button className="primary" onClick={() => navigate("/")}>返回项目列表</button>
      </div>
    </main>;
  }

	// loading 期间，或 project 与当前路由不匹配时，显示全屏 loading。
	// 切换项目时旧 project 不匹配新 id，显示 loading 而非旧内容。
	// 此条件之后 project 必非 null 且匹配，TS 可收窄。
	if (loading || !project || project.id !== projectId) {
    return <main className="app-shell project-open">
      <div style={{ display: "flex", alignItems: "center", justifyContent: "center", minHeight: "100vh" }}>
        <p style={{ color: "#586273", fontSize: "14px" }}>加载项目中...</p>
      </div>
    </main>;
  }

  const navigateWorkspaceTabs = (event: React.KeyboardEvent<HTMLElement>) => {
    if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return;
    const tabs = Array.from(event.currentTarget.querySelectorAll<HTMLButtonElement>('[role="tab"]:not(:disabled)'));
    const currentIndex = tabs.indexOf(document.activeElement as HTMLButtonElement);
    if (currentIndex < 0 || tabs.length === 0) return;
    event.preventDefault();
    const nextIndex = event.key === "Home" ? 0 : event.key === "End" ? tabs.length - 1 : (currentIndex + (event.key === "ArrowRight" ? 1 : -1) + tabs.length) % tabs.length;
    const nextTab = tabs[nextIndex];
    const tab = nextTab.dataset.workspaceTab as WorkspaceTab | undefined;
    nextTab.focus();
    if (tab) selectWorkspaceTab(tab);
  };

  return <div className="chat">
    {globalError && <div className="error" role="alert"><ErrorAlertIcon /><span>{globalError}</span><button type="button" title="关闭错误提示" aria-label="关闭错误提示" onClick={() => setGlobalError("")}><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M6 6l12 12M18 6 6 18" /></svg></button></div>}
    <header className="project-head">
      <div className="project-heading">
        <button className="back-projects" type="button" title="返回项目列表" aria-label="返回项目列表" onClick={() => navigate("/")}><BackProjectsIcon /></button>
        <h2>{project.name}</h2>
      </div>
      <div className="head-actions-slot">
        <NotificationCenter />
      </div>
    </header>
    <nav className="workspace-tabs" role="tablist" aria-label="项目工作区" onKeyDown={navigateWorkspaceTabs}>
      <button data-workspace-tab="conversation" id="workspace-tab-conversation" type="button" role="tab" aria-controls="workspace-panel-conversation" aria-selected={workspaceTab === "conversation"} className={workspaceTab === "conversation" ? "active" : ""} onClick={() => selectWorkspaceTab("conversation")}><WorkspaceTabIcon tab="conversation" /><span>对话</span></button>
      <button data-workspace-tab="tasks" id="workspace-tab-tasks" type="button" role="tab" aria-controls="workspace-panel-tasks" aria-selected={workspaceTab === "tasks"} className={workspaceTab === "tasks" ? "active" : ""} onClick={() => selectWorkspaceTab("tasks")}><WorkspaceTabIcon tab="tasks" /><span>任务</span></button>
      <button data-workspace-tab="orchestration" id="workspace-tab-orchestration" type="button" role="tab" aria-controls="workspace-panel-orchestration" aria-selected={workspaceTab === "orchestration"} className={workspaceTab === "orchestration" ? "active" : ""} onClick={() => selectWorkspaceTab("orchestration")}><WorkspaceTabIcon tab="orchestration" /><span>自动编排</span></button>
      <button data-workspace-tab="files" id="workspace-tab-files" type="button" role="tab" aria-controls="workspace-panel-files" aria-selected={workspaceTab === "files"} className={workspaceTab === "files" ? "active" : ""} onClick={() => selectWorkspaceTab("files")}><WorkspaceTabIcon tab="files" /><span>文件</span></button>
      {project.gitBranch !== "非 Git 目录" && <button data-workspace-tab="git" id="workspace-tab-git" type="button" role="tab" aria-controls="workspace-panel-git" aria-selected={workspaceTab === "git"} className={workspaceTab === "git" ? "active" : ""} onClick={() => selectWorkspaceTab("git")}><WorkspaceTabIcon tab="git" /><span>Git工作台</span></button>}
      <button data-workspace-tab="run" id="workspace-tab-run" type="button" role="tab" aria-controls="workspace-panel-run" aria-selected={workspaceTab === "run"} className={workspaceTab === "run" ? "active" : ""} onClick={() => selectWorkspaceTab("run")}><WorkspaceTabIcon tab="run" /><span>项目启动</span></button>
      <button data-workspace-tab="insights" id="workspace-tab-insights" type="button" role="tab" aria-controls="workspace-panel-insights" aria-selected={workspaceTab === "insights"} className={workspaceTab === "insights" ? "active" : ""} onClick={() => selectWorkspaceTab("insights")}><WorkspaceTabIcon tab="insights" /><span>优化建议</span></button>
    </nav>
    <main className="workspace-content">
      <Outlet context={{ project } satisfies ProjectLayoutOutletContext} />
    </main>
  </div>;
}
