// 项目启动页 — 包装 features/run/ProjectRunPanel

import { useParams, useOutletContext } from "react-router-dom";
import { ProjectRunPanel } from "../features/run/ProjectRunPanel";
import { ProjectLayoutOutletContext } from "../components/ProjectLayout";
import "../run.css";
import { useProjectContext } from "../stores/useProjectStore";

export default function ProjectRunPage() {
  const { projectId } = useParams<{ projectId: string }>();
  const { api, setError } = useProjectContext();
  const { project } = useOutletContext<ProjectLayoutOutletContext>();

  if (!projectId) return null;

  return <ProjectRunPanel projectID={projectId} request={api} fail={setError} active={true} isRemote={project.runner.startsWith("ssh-")} />;
}