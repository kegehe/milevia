// 任务看板页 — 包装 features/tasks/TaskBoard

import { useParams, useNavigate } from "react-router-dom";
import { TaskBoard } from "../features/tasks/TaskBoard";
import { useProjectContext } from "../stores/useProjectStore";

export default function TaskBoardPage() {
  const { projectId, taskId } = useParams<{ projectId: string; taskId?: string }>();
  const navigate = useNavigate();
  const { api, setError } = useProjectContext();

  if (!projectId) return null;

  return (
    <div className="workspace-tab-panel">
      <TaskBoard
        projectID={projectId}
        initialTaskID={taskId}
        permissionMode="full_control"
        request={api}
        fail={setError}
        close={() => navigate(`/projects/${projectId}/conversations`)}
        onDispatched={() => {
          navigate(`/projects/${projectId}/conversations`);
        }}
      />
    </div>
  );
}