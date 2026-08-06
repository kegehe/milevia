// 路由入口 — 从 2003 行精简为 ~50 行

import { BrowserRouter, Routes, Route, Navigate } from "react-router-dom";
import { ProjectProvider } from "./stores/useProjectStore";
import { NotificationProvider } from "./components/NotificationProvider";
import { TooltipProvider } from "./components/TooltipProvider";
import DashboardPage from "./pages/DashboardPage";
import ImportProjectPage from "./pages/ImportProjectPage";
import SSHManagerPage from "./pages/SSHManagerPage";
import ProjectLayout from "./components/ProjectLayout";
import ConversationPage from "./pages/ConversationPage";
import TaskBoardPage from "./pages/TaskBoardPage";
import GitWorkbenchPage from "./pages/GitWorkbenchPage";
import ProjectRunPage from "./pages/ProjectRunPage";
import FilesPage from "./pages/FilesPage";
import AgentProfilesPage from "./pages/AgentProfilesPage";

export function App() {
  return (
    <BrowserRouter>
      <TooltipProvider>
        <NotificationProvider>
          <ProjectProvider>
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
            </Route>
            <Route path="*" element={<Navigate to="/" replace />} />
            </Routes>
          </ProjectProvider>
        </NotificationProvider>
      </TooltipProvider>
    </BrowserRouter>
  );
}
