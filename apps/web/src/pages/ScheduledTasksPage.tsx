import { FormEvent, useCallback, useEffect, useMemo, useState } from "react";
import { createPortal } from "react-dom";
import { useNavigate, useParams } from "react-router-dom";
import { ConfirmDialog } from "../components/ConfirmDialog";
import { useProjectContext } from "../stores/useProjectStore";
import type { AgentID, PermissionMode, ScheduledTask, ScheduledTaskRun, Skill } from "../lib/types";
import { TasksSubnav } from "./TasksSubnav";

type ScheduleForm = {
  title: string;
  prompt: string;
  skills: string[];
  agentId: AgentID;
  permissionMode: PermissionMode;
  scheduleType: "once" | "daily" | "weekly";
  timezone: string;
  runAt: string;
  timeOfDay: string;
  weekdays: number[];
  enabled: boolean;
};

type PendingDelete = ScheduledTask | null;

const weekdayOptions = [
  { value: 1, label: "一" }, { value: 2, label: "二" }, { value: 3, label: "三" }, { value: 4, label: "四" }, { value: 5, label: "五" }, { value: 6, label: "六" }, { value: 7, label: "日" },
];

function localTimezone() {
  return Intl.DateTimeFormat().resolvedOptions().timeZone || "Asia/Shanghai";
}

function formatDateTime(value?: string, timezone?: string) {
  if (!value) return "-";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "-";
  try {
    return new Intl.DateTimeFormat("zh-CN", { month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit", hour12: false, timeZone: timezone }).format(date);
  } catch {
    return new Intl.DateTimeFormat("zh-CN", { month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit", hour12: false }).format(date);
  }
}

function toLocalDateTime(value?: string, timezone = localTimezone()) {
  const date = value ? new Date(value) : new Date(Date.now() + 60 * 60 * 1000);
  if (Number.isNaN(date.getTime())) return "";
  try {
    const parts = new Intl.DateTimeFormat("en-CA", { timeZone: timezone, year: "numeric", month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", hourCycle: "h23" }).formatToParts(date);
    const values = Object.fromEntries(parts.filter((part) => part.type !== "literal").map((part) => [part.type, part.value]));
    return `${values.year}-${values.month}-${values.day}T${values.hour}:${values.minute}`;
  } catch {
    return "";
  }
}

function defaultForm(): ScheduleForm {
  return {
    title: "",
    prompt: "",
    skills: [],
    agentId: "claude-code",
    permissionMode: "approval_required",
    scheduleType: "daily",
    timezone: localTimezone(),
    runAt: toLocalDateTime(),
    timeOfDay: "09:00",
    weekdays: [1, 2, 3, 4, 5],
    enabled: true,
  };
}

function formFromTask(task: ScheduledTask): ScheduleForm {
  return {
    title: task.title,
    prompt: task.prompt,
    skills: task.skills,
    agentId: task.agentId,
    permissionMode: task.permissionMode,
    scheduleType: task.scheduleType,
    timezone: task.timezone,
    runAt: toLocalDateTime(task.runAt, task.timezone),
    timeOfDay: task.timeOfDay || "09:00",
    weekdays: task.weekdays,
    enabled: task.enabled,
  };
}

function permissionOptions(agentID: AgentID): { value: PermissionMode; label: string }[] {
  return agentID === "codex"
    ? [{ value: "read_only", label: "只读分析" }, { value: "workspace_write", label: "项目内执行" }, { value: "full_control", label: "完全控制" }]
    : [{ value: "approval_required", label: "需审批" }, { value: "full_control", label: "完全控制" }];
}

function scheduleDescription(task: ScheduledTask) {
  if (task.scheduleType === "once") return `单次 · ${formatDateTime(task.runAt, task.timezone)} · ${task.timezone}`;
  if (task.scheduleType === "daily") return `每天 ${task.timeOfDay} · ${task.timezone}`;
  const days = task.weekdays.map((value) => weekdayOptions.find((day) => day.value === value)?.label).filter(Boolean).join("、");
  return `每周${days} ${task.timeOfDay} · ${task.timezone}`;
}

function runStatusLabel(run?: ScheduledTaskRun) {
  if (!run) return "尚未运行";
  const labels: Record<ScheduledTaskRun["status"], string> = { queued: "等待执行", running: "执行中", succeeded: "已完成", failed: "需处理", stopped: "已停止", interrupted: "已中断" };
  return labels[run.status];
}

function ScheduleIcon() {
  return <svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="13" r="6.5" /><path d="M12 9.5V13l2.5 1.5M8.5 3v2M15.5 3v2M5.5 6.5 4 5" /></svg>;
}

export default function ScheduledTasksPage() {
  const { projectId, scheduledTaskId } = useParams<{ projectId: string; scheduledTaskId?: string }>();
  const navigate = useNavigate();
  const { api: projectApi, setError } = useProjectContext();
  const [tasks, setTasks] = useState<ScheduledTask[]>([]);
  const [loading, setLoading] = useState(true);
  const [editor, setEditor] = useState<ScheduledTask | "new" | null>(null);
  const [busyID, setBusyID] = useState("");
  const [pendingDelete, setPendingDelete] = useState<PendingDelete>(null);

  const load = useCallback(async () => {
    if (!projectId) return;
    const next = await projectApi<ScheduledTask[]>(`/api/projects/${projectId}/scheduled-tasks`);
    setTasks(next);
  }, [projectApi, projectId]);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    void load().catch((cause) => { if (!cancelled) setError(cause instanceof Error ? cause.message : "无法加载定时任务"); }).finally(() => { if (!cancelled) setLoading(false); });
    return () => { cancelled = true; };
  }, [load, setError]);

  useEffect(() => {
    const interval = window.setInterval(() => { void load().catch(() => undefined); }, 10_000);
    return () => window.clearInterval(interval);
  }, [load]);

  useEffect(() => {
    if (!scheduledTaskId) return;
    let cancelled = false;
    void projectApi<ScheduledTask>(`/api/scheduled-tasks/${scheduledTaskId}`).then((task) => {
      if (!cancelled) setEditor(task);
    }).catch((cause) => { if (!cancelled) setError(cause instanceof Error ? cause.message : "无法加载定时任务详情"); });
    return () => { cancelled = true; };
  }, [projectApi, scheduledTaskId, setError]);

  const openEditor = async (task: ScheduledTask) => {
    setBusyID(task.id);
    try {
      const detail = await projectApi<ScheduledTask>(`/api/scheduled-tasks/${task.id}`);
      setEditor(detail);
      navigate(`/projects/${projectId}/tasks/schedules/${task.id}`, { replace: true });
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "无法加载定时任务详情");
    } finally {
      setBusyID("");
    }
  };

  const closeEditor = () => {
    setEditor(null);
    navigate(`/projects/${projectId}/tasks/schedules`, { replace: true });
  };

  const runNow = async (task: ScheduledTask) => {
    setBusyID(task.id);
    try {
      await projectApi(`/api/scheduled-tasks/${task.id}/run`, { method: "POST", body: "{}" });
      await load();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "无法执行定时任务");
    } finally {
      setBusyID("");
    }
  };

  const toggle = async (task: ScheduledTask) => {
    setBusyID(task.id);
    try {
      await projectApi(`/api/scheduled-tasks/${task.id}/${task.enabled ? "pause" : "resume"}`, { method: "POST", body: "{}" });
      await load();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "无法更新定时任务状态");
    } finally {
      setBusyID("");
    }
  };

  const remove = async () => {
    const task = pendingDelete;
    if (!task) return;
    setBusyID(task.id);
    setPendingDelete(null);
    try {
      await projectApi(`/api/scheduled-tasks/${task.id}`, { method: "DELETE" });
      await load();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "无法删除定时任务");
    } finally {
      setBusyID("");
    }
  };

  if (!projectId) return null;
  return <div className="workspace-tab-panel">
    <TasksSubnav projectId={projectId} active="schedules" />
    <section className="scheduled-workspace" aria-label="定时任务">
      <header className="scheduled-workspace-head">
        <div>
          <span className="scheduled-eyebrow">AUTOMATION</span>
          <h2>定时任务</h2>
        </div>
        <button className="task-toolbar-action task-toolbar-create" type="button" onClick={() => setEditor("new")}><span className="scheduled-new-icon">+</span>新建定时任务</button>
      </header>
      {loading ? <div className="scheduled-loading">正在加载定时任务...</div> : tasks.length === 0 ? <div className="scheduled-empty"><ScheduleIcon /><h3>还没有定时任务</h3><p>为定期检查、日报或维护工作设置提示词和执行时间。</p><button className="primary" type="button" onClick={() => setEditor("new")}>新建定时任务</button></div> : <div className="scheduled-task-list">
        {tasks.map((task) => <article className={`scheduled-task-row${task.enabled ? "" : " is-paused"}`} key={task.id}>
          <div className="scheduled-task-main">
            <div className="scheduled-task-title"><span className="scheduled-status-dot" data-status={task.lastRun?.status || "idle"} /><div><h3>{task.title}</h3><p>{scheduleDescription(task)}</p></div></div>
            <p className="scheduled-task-prompt">{task.prompt}</p>
            <div className="scheduled-task-meta"><span>{task.agentId === "codex" ? "Codex" : "Claude Code"}</span><span>{permissionOptions(task.agentId).find((item) => item.value === task.permissionMode)?.label || task.permissionMode}</span>{task.skills.length > 0 && <span>{task.skills.length} 个技能</span>}</div>
          </div>
          <div className="scheduled-task-timing"><span>下次运行</span><b>{task.enabled ? formatDateTime(task.nextRunAt, task.timezone) : "已暂停"}</b><small>最近：{runStatusLabel(task.lastRun)}</small>{task.lastRun?.failureReason && <em title={task.lastRun.failureReason}>{task.lastRun.failureReason}</em>}</div>
          <div className="scheduled-task-actions">
            <button className="scheduled-icon-button" type="button" title="立即运行" aria-label={`立即运行 ${task.title}`} disabled={Boolean(busyID) || task.lastRun?.status === "queued" || task.lastRun?.status === "running"} onClick={() => void runNow(task)}><svg viewBox="0 0 24 24" aria-hidden="true"><path d="m9 7 7 5-7 5V7Z" /></svg></button>
            <button className="scheduled-icon-button" type="button" title={task.enabled ? "暂停" : "恢复"} aria-label={`${task.enabled ? "暂停" : "恢复"} ${task.title}`} disabled={Boolean(busyID)} onClick={() => void toggle(task)}><svg viewBox="0 0 24 24" aria-hidden="true">{task.enabled ? <><path d="M8 6v12M16 6v12" /></> : <path d="m9 7 7 5-7 5V7Z" />}</svg></button>
            <button className="scheduled-icon-button" type="button" title="编辑" aria-label={`编辑 ${task.title}`} disabled={Boolean(busyID)} onClick={() => void openEditor(task)}><svg viewBox="0 0 24 24" aria-hidden="true"><path d="m5 16.5 8.9-8.9 3.5 3.5-8.9 8.9L5 20l.1-3.5ZM12.8 8.7l2.5-2.5a1.7 1.7 0 0 1 2.4 2.4l-2.5 2.5" /></svg></button>
            <button className="scheduled-icon-button danger" type="button" title="删除" aria-label={`删除 ${task.title}`} disabled={Boolean(busyID)} onClick={() => setPendingDelete(task)}><svg viewBox="0 0 24 24" aria-hidden="true"><path d="m6 7 1 12h10l1-12M9 7V4.5h6V7M4.5 7h15M10 11v4.5M14 11v4.5" /></svg></button>
          </div>
        </article>)}
      </div>}
    </section>
    {editor && createPortal(<ScheduledTaskEditor key={editor === "new" ? "new" : editor.id} projectId={projectId} task={editor === "new" ? undefined : editor} request={projectApi} onClose={closeEditor} onSaved={async () => { closeEditor(); await load(); }} onFail={setError} />, document.body)}
    {pendingDelete && createPortal(<ConfirmDialog title="删除定时任务" danger confirmLabel="删除" message={<>确认删除 <b>{pendingDelete.title}</b>？历史运行记录也会一并删除。</>} onConfirm={remove} onCancel={() => setPendingDelete(null)} />, document.body)}
  </div>;
}

function ScheduledTaskEditor({ projectId, task, request, onClose, onSaved, onFail }: { projectId: string; task?: ScheduledTask; request: <T>(path: string, init?: RequestInit) => Promise<T>; onClose: () => void; onSaved: () => Promise<void>; onFail: (message: string) => void }) {
  const [form, setForm] = useState<ScheduleForm>(() => task ? formFromTask(task) : defaultForm());
  const [skills, setSkills] = useState<Skill[]>([]);
  const [skillsLoading, setSkillsLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const permissionChoices = useMemo(() => permissionOptions(form.agentId), [form.agentId]);

  useEffect(() => {
    let cancelled = false;
    setSkillsLoading(true);
    void request<Skill[]>(`/api/projects/${projectId}/skills?agentId=${encodeURIComponent(form.agentId)}`).then((available) => {
      if (cancelled) return;
      setSkills(available);
      const names = new Set(available.map((skill) => skill.name));
      setForm((current) => ({ ...current, skills: current.skills.filter((name) => names.has(name)) }));
    }).catch(() => { if (!cancelled) setSkills([]); }).finally(() => { if (!cancelled) setSkillsLoading(false); });
    return () => { cancelled = true; };
  }, [form.agentId, projectId, request]);

  const changeAgent = (agentId: AgentID) => {
    const options = permissionOptions(agentId);
    setForm((current) => ({ ...current, agentId, permissionMode: options.some((option) => option.value === current.permissionMode) ? current.permissionMode : agentId === "codex" ? "workspace_write" : "approval_required" }));
  };

  const toggleSkill = (name: string) => setForm((current) => ({ ...current, skills: current.skills.includes(name) ? current.skills.filter((item) => item !== name) : [...current.skills, name] }));
  const toggleWeekday = (value: number) => setForm((current) => ({ ...current, weekdays: current.weekdays.includes(value) ? current.weekdays.filter((item) => item !== value) : [...current.weekdays, value].sort((left, right) => left - right) }));

  const submit = (event: FormEvent) => {
    event.preventDefault();
    if (form.permissionMode === "full_control") {
      if (window.confirm("定时任务会在无人值守时直接执行命令，不再等待审批。确认使用完全控制？")) save(true);
      return;
    }
    save();
  };

  const save = (fullControlConfirmed = false) => {
    if (form.scheduleType === "once" && !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}$/.test(form.runAt)) {
      onFail("请选择有效的单次执行时间");
      return;
    }
    if (form.scheduleType === "weekly" && form.weekdays.length === 0) {
      onFail("每周任务至少选择一天");
      return;
    }
    const runAt = form.scheduleType === "once" ? form.runAt : "";
    setSaving(true);
    void request(task ? `/api/scheduled-tasks/${task.id}` : `/api/projects/${projectId}/scheduled-tasks`, {
      method: task ? "PATCH" : "POST",
      body: JSON.stringify({ ...form, runAt, fullControlConfirmed }),
    }).then(onSaved).catch((cause) => onFail(cause instanceof Error ? cause.message : "无法保存定时任务")).finally(() => setSaving(false));
  };

  return <div className="backdrop" role="dialog" aria-modal="true" aria-label={task ? "编辑定时任务" : "新建定时任务"}>
    <section className="modal schedule-task-dialog">
      <header className="schedule-task-dialog-head"><div><span>{task ? "EDIT SCHEDULE" : "NEW SCHEDULE"}</span><h2>{task ? "编辑定时任务" : "新建定时任务"}</h2></div><button className="task-editor-close" type="button" title="关闭" aria-label="关闭" disabled={saving} onClick={onClose}>×</button></header>
      <form className="schedule-task-form" onSubmit={submit}>
        <div className="schedule-form-scroll">
          <section><h3>任务内容</h3><label>名称<input value={form.title} maxLength={120} required placeholder="例如：工作日晚间代码检查" onChange={(event) => setForm((current) => ({ ...current, title: event.target.value }))} /></label><label>提示词<textarea value={form.prompt} required placeholder="描述需要 Agent 完成的检查、分析或维护工作。" onChange={(event) => setForm((current) => ({ ...current, prompt: event.target.value }))} /></label></section>
          <section><h3>执行方式</h3><div className="schedule-form-grid"><label>Agent<select value={form.agentId} onChange={(event) => changeAgent(event.target.value as AgentID)}><option value="claude-code">Claude Code</option><option value="codex">Codex</option></select></label><label>权限<select value={form.permissionMode} onChange={(event) => setForm((current) => ({ ...current, permissionMode: event.target.value as PermissionMode }))}>{permissionChoices.map((option) => <option value={option.value} key={option.value}>{option.label}</option>)}</select></label></div></section>
          <section><h3>执行时间</h3><div className="schedule-type-switch"><button type="button" className={form.scheduleType === "once" ? "active" : ""} onClick={() => setForm((current) => ({ ...current, scheduleType: "once" }))}>单次</button><button type="button" className={form.scheduleType === "daily" ? "active" : ""} onClick={() => setForm((current) => ({ ...current, scheduleType: "daily" }))}>每天</button><button type="button" className={form.scheduleType === "weekly" ? "active" : ""} onClick={() => setForm((current) => ({ ...current, scheduleType: "weekly" }))}>每周</button></div><div className="schedule-form-grid">{form.scheduleType === "once" ? <label>执行时间<input type="datetime-local" value={form.runAt} required onChange={(event) => setForm((current) => ({ ...current, runAt: event.target.value }))} /></label> : <label>执行时间<input type="time" value={form.timeOfDay} required onChange={(event) => setForm((current) => ({ ...current, timeOfDay: event.target.value }))} /></label>}<label>时区<input value={form.timezone} required placeholder="Asia/Shanghai" onChange={(event) => setForm((current) => ({ ...current, timezone: event.target.value }))} /></label></div>{form.scheduleType === "weekly" && <div className="weekday-picker" role="group" aria-label="执行星期">{weekdayOptions.map((day) => <button key={day.value} type="button" className={form.weekdays.includes(day.value) ? "active" : ""} aria-pressed={form.weekdays.includes(day.value)} onClick={() => toggleWeekday(day.value)}>{day.label}</button>)}</div>}<label className="schedule-enabled"><input type="checkbox" checked={form.enabled} onChange={(event) => setForm((current) => ({ ...current, enabled: event.target.checked }))} />保存后立即启用</label></section>
          <section><div className="schedule-skills-heading"><h3>调用技能</h3><span>{skillsLoading ? "正在读取…" : `${skills.length} 个可用`}</span></div>{skills.length === 0 && !skillsLoading ? <p className="schedule-skills-empty">当前 Agent 未发现可调用技能。</p> : <div className="schedule-skill-list">{skills.map((skill) => <label className="schedule-skill-option" key={skill.name} data-tooltip-title={skill.name} data-tooltip-desc={skill.description}><input type="checkbox" checked={form.skills.includes(skill.name)} onChange={() => toggleSkill(skill.name)} /><span><b>{skill.name}</b><small>{skill.description}</small></span><em>{skill.source === "project" ? "项目" : skill.source === "plugin" ? "插件" : "用户"}</em></label>)}</div>}</section>
          {task?.runs && task.runs.length > 0 && <section className="schedule-history"><h3>运行记录</h3>{task.runs.map((run) => <div key={run.id}><span className={`scheduled-run-status ${run.status}`}>{runStatusLabel(run)}</span><time>{formatDateTime(run.createdAt, task.timezone)}</time>{run.failureReason && <small>{run.failureReason}</small>}</div>)}</section>}
        </div>
        <footer><button type="button" className="secondary" disabled={saving} onClick={onClose}>取消</button><button type="submit" className="primary" disabled={saving}>{saving ? "保存中" : "保存任务"}</button></footer>
      </form>
    </section>
  </div>;
}
