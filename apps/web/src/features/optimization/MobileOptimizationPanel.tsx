import { memo, useRef } from "react";
import { InsightsPanel } from "../insights/InsightsPanel";
import "../insights/insights.css";
import "./mobile-optimization.css";
import type { AgentID } from "../../lib/types";

type Project = { id: string; name: string; runner: string };

// memo 是面板的**减负**：宿主页每 2 秒随快照轮询重渲染一次，面板若每次都跟着整棵
// 重渲染，InsightsPanel 的列表（可能几百条）就在后台白转。前提是宿主给的 props 全部
// 身份稳定（project / request / fail / onClose / onAgentChange / onOpenFile）——
// 任何一处内联都会让这层 memo 形同虚设，见 MobileRemotePage 的 insightsAdapter 一段。
export const MobileOptimizationPanel = memo(function MobileOptimizationPanel({ project, defaultAgentID, request, fail, onClose, onAgentChange, onOpenFile }: {
  project: Project;
  defaultAgentID: AgentID;
  request: <T>(path: string, init?: RequestInit) => Promise<T>;
  fail: (message: string) => void;
  onClose: () => void;
  onAgentChange: (agentID: AgentID) => void;
  onOpenFile?: (path: string) => void;
}) {
  // InsightsPanel 把 defaultAgentID 放进了**重置 effect** 的依赖（换项目时清选择集 /
  // 类型筛选 / 日志用的那条）：宿主若把用户在面板里的选择经 onAgentChange 回写进这个
  // prop，用户每换一次工具，选择集和筛选就会被整组清掉。这里只在挂载时取一次 ——
  // 面板每次打开都重新挂载，宿主的最新默认自然会在这时生效。
  const initialAgentRef = useRef(defaultAgentID);
  return <section className="mobile-optimization" role="dialog" aria-modal="true" aria-labelledby="mobile-optimization-title">
    <header className="mobile-optimization-header"><div><small>优化建议</small><h2 id="mobile-optimization-title">{project.name}</h2></div><button type="button" onClick={onClose} aria-label="关闭优化建议">×</button></header>
    <div className="mobile-optimization-content"><InsightsPanel projectID={project.id} runnerID={project.runner} request={request} fail={fail} defaultAgentID={initialAgentRef.current} onAgentChange={onAgentChange} onOpenFile={onOpenFile} /></div>
  </section>;
});
