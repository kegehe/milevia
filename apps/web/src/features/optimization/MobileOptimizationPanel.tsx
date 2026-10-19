import { InsightsPanel } from "../insights/InsightsPanel";
import "../insights/insights.css";
import "./mobile-optimization.css";
import type { AgentID } from "../../lib/types";

type Project = { id: string; name: string; runner: string };

export function MobileOptimizationPanel({ project, defaultAgentID, request, fail, onClose, onAgentChange, onOpenFile }: {
  project: Project;
  defaultAgentID: AgentID;
  request: <T>(path: string, init?: RequestInit) => Promise<T>;
  fail: (message: string) => void;
  onClose: () => void;
  onAgentChange: (agentID: AgentID) => void;
  onOpenFile?: (path: string) => void;
}) {
  return <section className="mobile-optimization" role="dialog" aria-modal="true" aria-labelledby="mobile-optimization-title">
    <header className="mobile-optimization-header"><div><small>优化建议</small><h2 id="mobile-optimization-title">{project.name}</h2></div><button type="button" onClick={onClose} aria-label="关闭优化建议">×</button></header>
    <div className="mobile-optimization-content"><InsightsPanel projectID={project.id} runnerID={project.runner} request={request} fail={fail} defaultAgentID={defaultAgentID} onAgentChange={onAgentChange} onOpenFile={onOpenFile} /></div>
  </section>;
}
