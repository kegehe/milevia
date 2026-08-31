// Git 工作台页 — 包装 features/git/GitWorkbench

import { useParams } from "react-router-dom";
import { GitWorkbench } from "../features/git/GitWorkbench";
import "../git.css";
import { useProjectContext } from "../stores/useProjectStore";
import { useActiveConversationId } from "../lib/use-active-conversation";

export default function GitWorkbenchPage() {
  const { projectId } = useParams<{ projectId: string }>();
  const { api, setError } = useProjectContext();
  const conversationId = useActiveConversationId(projectId);

  if (!projectId) return null;

  return <GitWorkbench projectID={projectId} conversationId={conversationId} request={api} fail={setError} active={true} />;
}
