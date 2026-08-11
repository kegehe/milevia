// 项目优化建议页 —— 包装 features/insights/InsightsPanel

import { useParams } from "react-router-dom";
import { InsightsPanel } from "../features/insights/InsightsPanel";
import { useProjectContext } from "../stores/useProjectStore";
import "../features/insights/insights.css";

export default function InsightsPage() {
  const { projectId } = useParams<{ projectId: string }>();
  const { api, setError } = useProjectContext();

  if (!projectId) return null;

  return (
    <div className="workspace-tab-panel">
      <InsightsPanel projectID={projectId} request={api} fail={setError} />
    </div>
  );
}
