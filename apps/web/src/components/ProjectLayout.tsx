// 项目页共享布局：header、workspace tabs
// 通过 Outlet context 向子路由传递 project 数据，避免重复加载

import { useEffect, useState, useCallback } from "react";
import { Outlet, useParams, useNavigate, useLocation } from "react-router-dom";
import NotificationCenter from "./NotificationCenter";
import type { Project, WorkspaceTab } from "../lib/types";
import { api } from "../lib/api";
import { useProjectContext } from "../stores/useProjectStore";

export interface ProjectLayoutOutletContext {
  project: Project;
}

export default function ProjectLayout() {
  const { projectId } = useParams<{ projectId: string }>();
  const navigate = useNavigate();
  const location = useLocation();
	const [project, setProject] = useState<Project | null>(null);
	const [loading, setLoading] = useState(true);
	const [resolvedProjectID, setResolvedProjectID] = useState<string | null>(null);
	const [error, setError] = useState("");
  const { error: globalError, setError: setGlobalError, refreshProjects } = useProjectContext();

  // 确定当前工作区标签
  const getWorkspaceTab = useCallback((): WorkspaceTab => {
    const path = location.pathname;
    if (path.endsWith("/tasks") || path.includes("/tasks/")) return "tasks";
    if (path.endsWith("/files")) return "files";
    if (path.endsWith("/git")) return "git";
    if (path.endsWith("/run")) return "run";
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
      case "files": navigate(`${base}/files`); break;
      case "git": navigate(`${base}/git`); break;
      case "run": navigate(`${base}/run`); break;
    }
  }, [projectId, navigate]);

  useEffect(() => {
    void refreshProjects().catch(() => undefined);
    const interval = window.setInterval(() => { void refreshProjects().catch(() => undefined); }, 10_000);
    return () => window.clearInterval(interval);
  }, [refreshProjects]);

  // 加载项目数据（仅一次，通过 context 共享给子路由）
	useEffect(() => {
		if (!projectId) return;
		let cancelled = false;
		setLoading(true);
		setError("");
		setProject(null);
		(async () => {
      try {
        const list = await api<Project[]>("/api/projects");
        if (cancelled) return;
        const found = list.find((p) => p.id === projectId);
			if (found) {
				setProject(found);
        } else {
          setError("项目不存在");
          navigate("/", { replace: true });
        }
      } catch (cause) {
        if (!cancelled) setError(cause instanceof Error ? cause.message : "无法加载项目");
		} finally {
			if (!cancelled) {
				setResolvedProjectID(projectId);
				setLoading(false);
			}
		}
    })();
    return () => { cancelled = true; };
  }, [projectId, navigate]);

	if (loading || resolvedProjectID !== projectId) {
    return <main className="app-shell project-open">
      <div style={{ display: "flex", alignItems: "center", justifyContent: "center", minHeight: "100vh" }}>
        <p style={{ color: "#586273", fontSize: "14px" }}>加载项目中...</p>
      </div>
    </main>;
  }

  if (error || !project) {
    return <main className="app-shell project-open">
      <div style={{ display: "flex", flexDirection: "column", alignItems: "center", justifyContent: "center", minHeight: "100vh", gap: "16px" }}>
        <p style={{ color: "#d14233", fontSize: "14px" }}>{error || "项目加载失败"}</p>
        <button className="primary" onClick={() => navigate("/")}>返回项目列表</button>
      </div>
    </main>;
  }

  const isRemoteProject = project.runner.startsWith("ssh-");

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
    {globalError && <div className="error" role="alert"><span>{globalError}</span><button type="button" title="关闭错误提示" onClick={() => setGlobalError("")}>x</button></div>}
    <header className="project-head">
      <div className="project-heading">
        <button className="back-projects" title="返回项目列表" onClick={() => navigate("/")}>←</button>
        <div>
          <label>{project.runner}</label>
          <h2>{project.name}</h2>
          <code>{project.pathDisplay}</code>
        </div>
      </div>
      <div className="head-actions-slot">
        <NotificationCenter />
      </div>
    </header>
    <nav className="workspace-tabs" role="tablist" aria-label="项目工作区" onKeyDown={navigateWorkspaceTabs}>
      <button data-workspace-tab="conversation" id="workspace-tab-conversation" type="button" role="tab" aria-controls="workspace-panel-conversation" aria-selected={workspaceTab === "conversation"} className={workspaceTab === "conversation" ? "active" : ""} onClick={() => selectWorkspaceTab("conversation")}>对话</button>
      <button data-workspace-tab="tasks" id="workspace-tab-tasks" type="button" role="tab" aria-controls="workspace-panel-tasks" aria-selected={workspaceTab === "tasks"} className={workspaceTab === "tasks" ? "active" : ""} onClick={() => selectWorkspaceTab("tasks")}>任务</button>
      <button data-workspace-tab="files" id="workspace-tab-files" type="button" role="tab" aria-controls="workspace-panel-files" aria-selected={workspaceTab === "files"} className={workspaceTab === "files" ? "active" : ""} onClick={() => selectWorkspaceTab("files")}>文件</button>
      {project.gitBranch !== "非 Git 目录" && <button data-workspace-tab="git" id="workspace-tab-git" type="button" role="tab" aria-controls="workspace-panel-git" aria-selected={workspaceTab === "git"} className={workspaceTab === "git" ? "active" : ""} disabled={isRemoteProject} title={isRemoteProject ? "远程 Git 工作台尚未支持" : undefined} onClick={() => selectWorkspaceTab("git")}>Git 工作台</button>}
      <button data-workspace-tab="run" id="workspace-tab-run" type="button" role="tab" aria-controls="workspace-panel-run" aria-selected={workspaceTab === "run"} className={workspaceTab === "run" ? "active" : ""} disabled={isRemoteProject} title={isRemoteProject ? "远程项目启动尚未支持" : undefined} onClick={() => selectWorkspaceTab("run")}>项目启动</button>
    </nav>
    <main className="workspace-content">
      <Outlet context={{ project } satisfies ProjectLayoutOutletContext} />
    </main>
  </div>;
}
