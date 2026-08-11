// 路由入口 — 从 2003 行精简为 ~50 行

import { useEffect } from "react";
import { BrowserRouter, Routes, Route, Navigate, useNavigate } from "react-router-dom";
import { ProjectProvider } from "./stores/useProjectStore";
import { NotificationProvider } from "./components/NotificationProvider";
import { ProcessStatusProvider } from "./components/ProcessStatusProvider";
import { TooltipProvider } from "./components/TooltipProvider";
import DashboardPage from "./pages/DashboardPage";
import ImportProjectPage from "./pages/ImportProjectPage";
import SSHManagerPage from "./pages/SSHManagerPage";
import ProjectLayout from "./components/ProjectLayout";
import ConversationPage from "./pages/ConversationPage";
import TaskBoardPage from "./pages/TaskBoardPage";
import GitWorkbenchPage from "./pages/GitWorkbenchPage";
import ProjectRunPage from "./pages/ProjectRunPage";
import InsightsPage from "./pages/InsightsPage";
import FilesPage from "./pages/FilesPage";
import AgentProfilesPage from "./pages/AgentProfilesPage";
import { UpdateBanner } from "./features/updater/UpdateBanner";

declare global {
  interface Window {
    /** 由托盘面板经 Rust 注入触发的主窗口客户端路由导航钩子。 */
    __mileviaNavigate?: (path: string) => void;
  }
}

/** 把窗口级导航钩子接到 React Router：供托盘面板（`navigate_main` command）驱动主窗跳转。 */
function NavigationBridge() {
  const navigate = useNavigate();
  useEffect(() => {
    window.__mileviaNavigate = (path: string) => navigate(path);
    return () => {
      delete window.__mileviaNavigate;
    };
  }, [navigate]);
  return null;
}

export function App() {
  return (
    <BrowserRouter>
      <TooltipProvider>
        <NotificationProvider>
          <ProcessStatusProvider>
          <ProjectProvider>
            <NavigationBridge />
            <UpdateBanner />
            <Routes>
            <Route path="/" element={<DashboardPage />} />
            <Route path="/projects/import" element={<ImportProjectPage />} />
            <Route path="/ssh-manager" element={<SSHManagerPage />} />
            <Route path="/agent-profiles" element={<AgentProfilesPage />} />
            <Route path="/projects/:projectId" element={<ProjectLayout />}>
              <Route index element={<Navigate to="conversations" replace />} />
              <Route path="conversations" element={<ConversationPage />} />
              <Route path="conversations/:conversationId" element={<ConversationPage />} />
              <Route path="tasks" element={<TaskBoardPage />} />
              <Route path="tasks/:taskId" element={<TaskBoardPage />} />
              <Route path="files" element={<FilesPage />} />
              <Route path="git" element={<GitWorkbenchPage />} />
              <Route path="run" element={<ProjectRunPage />} />
              <Route path="insights" element={<InsightsPage />} />
            </Route>
            <Route path="*" element={<Navigate to="/" replace />} />
            </Routes>
          </ProjectProvider>
          </ProcessStatusProvider>
        </NotificationProvider>
      </TooltipProvider>
    </BrowserRouter>
  );
}
