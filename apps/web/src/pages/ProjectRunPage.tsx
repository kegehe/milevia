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

  // WSL 项目（项目目录在 WSL 内、跑在 Windows 控制服务之外的 WSL 里）：执行环境只能是
  // WSL，运行环境选择由后端固定，不能当作普通本机项目。localRunner 由服务端下发，
  // 非 Windows 服务端上的 wsl-local 就是本机 runner，不能按 runner id 自行判断。
  const wslProject = project.runner === "wsl-local" && !project.localRunner;

  return <ProjectRunPanel projectID={projectId} conversationId={conversationId} request={api} fail={setError} active={true} isRemote={project.runner.startsWith("ssh-")} wslProject={wslProject} />;
}
