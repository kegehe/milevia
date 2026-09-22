// Git 工作台页 — 包装 features/git/GitWorkbench

import { useOutletContext, useParams } from "react-router-dom";
import { GitWorkbench } from "../features/git/GitWorkbench";
import "../git.css";
import { ProjectLayoutOutletContext } from "../components/ProjectLayout";
import { useProjectContext } from "../stores/useProjectStore";
import { useActiveConversationId } from "../lib/use-active-conversation";
import { NON_GIT_BRANCH } from "../lib/types";

export default function GitWorkbenchPage() {
  const { projectId } = useParams<{ projectId: string }>();
  const { project, refreshProject } = useOutletContext<ProjectLayoutOutletContext>();
  const { api, setError } = useProjectContext();
  const conversationId = useActiveConversationId(projectId);

  if (!projectId) return null;

  // 非 git 项目也能进 Git 工作台：工作台据此渲染「未初始化 Git 仓库」空态并提供 git init。
  const initialIsGitRepo = project.gitBranch !== NON_GIT_BRANCH;

  return <GitWorkbench projectID={projectId} conversationId={conversationId} request={api} fail={setError} active={true} initialIsGitRepo={initialIsGitRepo} onGitInitialized={refreshProject} />;
}