import { useOutletContext, useParams } from "react-router-dom";
import { FilesPanel } from "../features/files/FilesPanel";
import { useProjectContext } from "../stores/useProjectStore";
import type { ProjectLayoutOutletContext } from "../components/ProjectLayout";
import "../files.css";

export default function FilesPage() {
  const { projectId } = useParams<{ projectId: string }>();
  const { project } = useOutletContext<ProjectLayoutOutletContext>();
  const { api, projectStatuses } = useProjectContext();

  if (!projectId || !project) return null;

  // 检查当前项目是否有 AI 运行中
  const status = projectStatuses[projectId];
  const isWorkspaceOccupied = status?.running === true;

  return (
    <FilesPanel
      projectId={projectId}
      runner={project.runner}
      request={api}
      isWorkspaceOccupied={isWorkspaceOccupied}
    />
  );
}
