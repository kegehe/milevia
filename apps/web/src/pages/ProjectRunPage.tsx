// 项目启动页 — 包装 features/run/ProjectRunPanel

import { useParams, useOutletContext } from "react-router-dom";
import { ProjectRunPanel } from "../features/run/ProjectRunPanel";
import { ProjectLayoutOutletContext } from "../components/ProjectLayout";
import "../run.css";
import { useProjectContext } from "../stores/useProjectStore";
import { useActiveConversationId } from "../lib/use-active-conversation";

export default function ProjectRunPage() {
  const { projectId } = useParams<{ projectId: string }>();
  const { api, setError } = useProjectContext();
  const { project } = useOutletContext<ProjectLayoutOutletContext>();
  const conversationId = useActiveConversationId(projectId);

  if (!projectId) return null;

  return <ProjectRunPanel projectID={projectId} conversationId={conversationId} request={api} fail={setError} active={true} isRemote={project.runner.startsWith("ssh-")} />;
}
