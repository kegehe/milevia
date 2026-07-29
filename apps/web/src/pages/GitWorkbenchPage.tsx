// Git 工作台页 — 包装 features/git/GitWorkbench

import { useParams } from "react-router-dom";
import { GitWorkbench } from "../features/git/GitWorkbench";
import "../git.css";
import { useProjectContext } from "../stores/useProjectStore";

export default function GitWorkbenchPage() {
  const { projectId } = useParams<{ projectId: string }>();
  const { api, setError } = useProjectContext();

  if (!projectId) return null;

  return <GitWorkbench projectID={projectId} request={api} fail={setError} active={true} />;
}