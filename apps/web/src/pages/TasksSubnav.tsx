import { useNavigate } from "react-router-dom";

export function TasksSubnav({ projectId, active }: { projectId: string; active: "board" | "schedules" }) {
  const navigate = useNavigate();
  return <nav className="tasks-subnav" aria-label="任务页面">
    <button type="button" className={active === "board" ? "active" : ""} aria-current={active === "board" ? "page" : undefined} onClick={() => navigate(`/projects/${projectId}/tasks/board`)}>
      <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M8 6h11M8 12h11M8 18h11M4 6h.01M4 12h.01M4 18h.01" /></svg>
      任务看板
    </button>
    <button type="button" className={active === "schedules" ? "active" : ""} aria-current={active === "schedules" ? "page" : undefined} onClick={() => navigate(`/projects/${projectId}/tasks/schedules`)}>
      <svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="13" r="6.5" /><path d="M12 9.5V13l2.5 1.5M8.5 3v2M15.5 3v2M5.5 6.5 4 5" /></svg>
      定时任务
    </button>
  </nav>;
}
