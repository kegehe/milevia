// 项目优化建议页 —— 包装 features/insights/InsightsPanel

import { useCallback } from "react";
import { useOutletContext, useParams } from "react-router-dom";
import { InsightsPanel } from "../features/insights/InsightsPanel";
import { OPEN_FILE_STORAGE_KEY } from "../features/files/file-model";
import { useProjectContext } from "../stores/useProjectStore";
import type { ProjectLayoutOutletContext } from "../components/ProjectLayout";
import "../features/insights/insights.css";

export default function InsightsPage() {
  const { projectId } = useParams<{ projectId: string }>();
  const { api, setError } = useProjectContext();
  const { navigateWithGuard } = useOutletContext<ProjectLayoutOutletContext>();

  // 建议卡片里的「查看文件」：把目标路径交给文件页（与"添加到对话"同样的会话内传递方式），
  // 再跳过去由 FilesPanel 自动打开。路径来自 AI 输出（相对项目根），文件页会做越权校验。
  const openFile = useCallback((path: string) => {
    if (!projectId) return;
    sessionStorage.setItem(OPEN_FILE_STORAGE_KEY, path);
    navigateWithGuard(`/projects/${projectId}/files`);
  }, [projectId, navigateWithGuard]);

  if (!projectId) return null;

  return (
    <div className="workspace-tab-panel">
      <InsightsPanel projectID={projectId} request={api} fail={setError} onOpenFile={openFile} />
    </div>
  );
}
