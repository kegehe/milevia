import { useCallback } from "react";
import { useOutletContext, useParams } from "react-router-dom";
import { FilesPanel } from "../features/files/FilesPanel";
import { useProjectContext } from "../stores/useProjectStore";
import { useActiveConversationId } from "../lib/use-active-conversation";
import type { ProjectLayoutOutletContext } from "../components/ProjectLayout";
import "../files.css";

export default function FilesPage() {
  const { projectId } = useParams<{ projectId: string }>();
  const { project, registerNavigationGuard, navigateWithGuard } = useOutletContext<ProjectLayoutOutletContext>();
  const { api, projectStatuses } = useProjectContext();
  const conversationId = useActiveConversationId(projectId);

  // 检查当前项目是否有 AI 运行中
  const status = projectStatuses[projectId ?? ""];
  const isWorkspaceOccupied = status?.running === true;

  const handleAddToChat = useCallback((path: string) => {
    if (!projectId) return;
    sessionStorage.setItem("milevia_add_file_to_chat", path);
    navigateWithGuard(`/projects/${projectId}/conversations?addFile=true`);
  }, [projectId, navigateWithGuard]);

  if (!projectId || !project) return null;
  return (
    <FilesPanel
      projectId={projectId}
      conversationId={conversationId}
      runner={project.runner}
      request={api}
      isWorkspaceOccupied={isWorkspaceOccupied}
      onAddToChat={handleAddToChat}
      registerNavigationGuard={registerNavigationGuard}
    />
  );
}
