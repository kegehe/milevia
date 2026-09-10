// 路由入口 — 从 2003 行精简为 ~50 行

import { useEffect } from "react";
import { BrowserRouter, Routes, Route, Navigate, useNavigate, useParams } from "react-router-dom";
import { ProjectProvider } from "./stores/useProjectStore";
import { UIPreferencesProvider } from "./stores/useUIPreferences";
import { NotificationProvider } from "./components/NotificationProvider";
import { ProcessStatusProvider } from "./components/ProcessStatusProvider";
import { LiveEventsProvider } from "./components/LiveEventsProvider";
import { TooltipProvider } from "./components/TooltipProvider";
import DashboardPage from "./pages/DashboardPage";
import ImportProjectPage from "./pages/ImportProjectPage";
import SSHManagerPage from "./pages/SSHManagerPage";
import ProjectLayout from "./components/ProjectLayout";
import ConversationPage from "./pages/ConversationPage";
import TaskBoardPage from "./pages/TaskBoardPage";
import ScheduledTasksPage from "./pages/ScheduledTasksPage";
import OrchestrationPage from "./pages/OrchestrationPage";
import GitWorkbenchPage from "./pages/GitWorkbenchPage";
import ProjectRunPage from "./pages/ProjectRunPage";
import InsightsPage from "./pages/InsightsPage";
import FilesPage from "./pages/FilesPage";
import AgentProfilesPage from "./pages/AgentProfilesPage";
import SettingsPage from "./pages/SettingsPage";
import McpManagerPage from "./pages/McpManagerPage";
import TerminalPage from "./pages/TerminalPage";
import MobileRemotePage from "./pages/MobileRemotePage";
import { UpdateBanner } from "./features/updater/UpdateBanner";
import { Capacitor } from "@capacitor/core";

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

// /tasks 归一到 /tasks/board 的中间跳转。这里不能用相对路径 <Navigate to="tasks/board">：
// 相对路径会相对当前 URL（/projects/:pid/tasks）解析成 /projects/:pid/tasks/tasks/board，
// 匹配不到任何路由，最终被 * 兜底重定向到项目总览（Dashboard）。改用绝对 /absolute 目标，
// 从 :projectId 现场拼出完整路径，绕开该问题。
function RedirectToTaskBoard() {
  const { projectId } = useParams<{ projectId: string }>();
  return <Navigate to={`/projects/${projectId}/tasks/board`} replace />;
}

export function App() {
  const nativeMobile = Capacitor.isNativePlatform();
  // Keep the browser /mobile route isolated as well. This prevents desktop
  // providers from opening local API/WebSocket connections while previewing
  // the remote page on Windows.
  const mobileRoute = typeof window !== "undefined" && window.location.pathname === "/mobile";
  return (
    <BrowserRouter>
      {nativeMobile || mobileRoute ? <MobileRemotePage /> : <>
      <TooltipProvider>
        <UIPreferencesProvider>
          <NotificationProvider>
            <LiveEventsProvider>
            <ProcessStatusProvider>
              <ProjectProvider>
                <NavigationBridge />
                <UpdateBanner />
                <Routes>
                  <Route path="/" element={<DashboardPage />} />
                  <Route path="/projects/import" element={<ImportProjectPage />} />
                  <Route path="/ssh-manager" element={<SSHManagerPage />} />
                  <Route path="/mcp-manager" element={<McpManagerPage />} />
                  <Route path="/agent-profiles" element={<AgentProfilesPage />} />
                  <Route path="/settings" element={<SettingsPage />} />
                  <Route path="/mobile" element={<MobileRemotePage />} />
                  <Route path="/projects/:projectId" element={<ProjectLayout />}>
                    <Route index element={<Navigate to="conversations" replace />} />
                    <Route path="conversations" element={<ConversationPage />} />
                    <Route path="conversations/:conversationId" element={<ConversationPage />} />
                    <Route path="tasks" element={<RedirectToTaskBoard />} />
                    <Route path="tasks/board" element={<TaskBoardPage />} />
                    <Route path="tasks/board/:taskId" element={<TaskBoardPage />} />
                    <Route path="tasks/schedules" element={<ScheduledTasksPage />} />
                    <Route path="tasks/schedules/:scheduledTaskId" element={<ScheduledTasksPage />} />
                    <Route path="tasks/:taskId" element={<TaskBoardPage />} />
                    <Route path="orchestration" element={<OrchestrationPage />} />
                    <Route path="files" element={<FilesPage />} />
                    <Route path="git" element={<GitWorkbenchPage />} />
                    <Route path="run" element={<ProjectRunPage />} />
                    <Route path="terminal" element={<TerminalPage />} />
                    <Route path="insights" element={<InsightsPage />} />
                  </Route>
                  <Route path="*" element={<Navigate to="/" replace />} />
                </Routes>
              </ProjectProvider>
            </ProcessStatusProvider>
            </LiveEventsProvider>
          </NotificationProvider>
        </UIPreferencesProvider>
      </TooltipProvider>
      </>}
    </BrowserRouter>
  );
}
