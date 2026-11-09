import { useEffect, useRef, useState } from "react";
import { Link, useParams, useSearchParams } from "react-router-dom";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { api } from "../lib/api";
import { apiURL, sessionHeaders } from "../lib/runtime";
import type { Task } from "../features/tasks/task-model";
import { type RoleConfigs, type Budget, type Session, type Detail, type Capabilities, type ProcessEntry, type ProcessPage, label, terminal, mergeProcess } from "../features/supervision/model";
import { supervisionWrite } from "../features/supervision/requests";
import { SupervisionProfiles } from "../features/supervision/SupervisionProfiles";
import { WorkspaceOperations } from "../features/supervision/WorkspaceOperations";
import "./supervision.css";

const pretty = (value: unknown) => typeof value === "string" ? value : JSON.stringify(value, null, 2);
const time = (value: string) => new Date(value).toLocaleString();
const message = (cause: unknown) => cause instanceof Error ? cause.message : String(cause);
const object = (value: unknown): Record<string, unknown> | undefined => value !== null && typeof value === "object" && !Array.isArray(value) ? value as Record<string, unknown> : undefined;
function Raw({ title, value }: { title: string; value: unknown }) { return <details><summary>{title}</summary><pre>{pretty(value)}</pre></details>; }
function ProtectionSummary({ state }: { state: string }) {
  if (state === "not_acquired") return null;
  return <p role="status" className="supervision-protection">{state === "released"
    ? "工作区已交还，可以继续其他操作。"
    : state === "unknown"
      ? "工作区保护状态待核对，暂不能归档或清理产物。"
      : "正在核对执行资源并交还工作区；交还完成前，目录仍受托管保护，暂不能归档或清理产物。"}</p>;
}
function AgentContent({ entry }: { entry: ProcessEntry }) {
  if (entry.type === "supervision.runtime") {
    const payload = object(entry.payload); const cli = object(payload?.cli); const authentication = object(payload?.authentication);
    if (typeof cli?.agentId === "string" && typeof cli.version === "string" && typeof payload?.targetId === "string") return <><p>执行配置：{cli.agentId} · {cli.version} · {payload.targetId}</p>{(authentication?.source === "profile" || authentication?.source === "cli_managed") && <p>认证来源：{authentication.source === "profile" ? "项目认证档案" : "CLI 现有配置"}</p>}<Raw title="CLI 路径、协议与输入指纹" value={payload} /></>;
  }
  if (!entry.content) {
    const payload = entry.payload as { text?: string; stream?: string } | undefined;
    return entry.type === "verification.output" && payload?.text ? <><small>{payload.stream}</small><pre>{payload.text}</pre></> : <Raw title={entry.type ?? "执行事件"} value={entry.payload} />;
  }
  let parsed: Record<string, unknown> | undefined;
  try { parsed = object(JSON.parse(entry.content)); } catch { /* 普通对话按 Markdown 展示 */ }
  // Invalid model output is deliberately retained by the backend for diagnosis.
  // Rendering it must not assume it has already passed the role schema checks.
  const instruction = object(object(parsed?.job)?.spec)?.instruction;
  const summary = typeof parsed?.reason === "string" ? parsed.reason : typeof parsed?.summary === "string" ? parsed.summary : undefined;
  const actionInstruction = object(parsed?.action)?.instruction;
  const editPaths = Array.isArray(parsed?.edits) && parsed.edits.every(edit => typeof object(edit)?.path === "string")
    ? parsed.edits.map(edit => object(edit)!.path as string) : undefined;
  if (entry.role === "user") return <>{typeof instruction === "string" && instruction && <p>{instruction}</p>}<Raw title="完整上下文 / 下发内容" value={entry.content} /></>;
  if (summary) return <><p>{summary}</p>{typeof actionInstruction === "string" && actionInstruction && <blockquote>{actionInstruction}</blockquote>}{editPaths && <p>文件变更：{editPaths.join("、") || "无"}</p>}{(parsed?.verdict === "pass" || parsed?.verdict === "fail") && <strong>审查：{parsed.verdict === "pass" ? "通过" : "未通过"}</strong>}<Raw title="完整结构化输出" value={parsed} /></>;
  return <ReactMarkdown remarkPlugins={[remarkGfm]}>{entry.content}</ReactMarkdown>;
}
type Changes = { baselineRevisionId: string; currentRevisionId: string; changes: { path: string; beforeSha256: string; afterSha256: string; readable: boolean; beforeSize: number; afterSize: number }[] };
function BudgetFields({ value, change }: { value: Budget; change: (value: Budget) => void }) {
  const fields: [keyof Budget, string, number, number, number][] = [["maxStorageBytes", "存储预算（MiB）", 1048576, 16, 65536], ["maxActiveMillis", "活动时间（分钟）", 60000, 1, 525600], ["maxAgentTurns", "Agent 调用次数", 1, 1, 100000], ["maxTurnMillis", "单轮上限（分钟）", 60000, 1, 30], ["maxFixRounds", "每任务修复次数", 1, 0, 100], ["maxTransientRetries", "临时故障重试次数", 1, 0, 100], ["finalReservePercent", "最终验收预留（%）", 1, 1, 90]];
  return <div className="supervision-budget-fields">{fields.map(([key, title, unit, min, max]) => <label key={key}>{title}<input type="number" min={min} max={max} required value={(value[key] ?? (key === "maxStorageBytes" ? 2147483648 : 0)) / unit} onChange={e => change({ ...value, [key]: Number(e.target.value) * unit })} /></label>)}</div>;
}

function RoleFields({ value, change, capabilities, fallback }: { value: RoleConfigs; change: (value: RoleConfigs) => void; capabilities: Capabilities | null; fallback: string }) {
  return <div className="supervision-role-fields">{(["scheduler", "implementation", "review"] as const).map(role => {
    const config = value[role] ?? { agentId: "" }; const agentId = config.agentId || value.scheduler?.agentId || fallback;
    const capability = capabilities?.agents.find(agent => agent.agentId === agentId);
    const efforts = capability?.reasoningEfforts ?? [];
    const update = (patch: Partial<typeof config>) => change({ ...value, [role]: { ...config, ...patch } });
    return <fieldset key={role}><legend>{label(role)}</legend>
      <label>CLI<select aria-label={`${label(role)} CLI`} value={config.agentId} onChange={e => update({ agentId: e.target.value, profileId: "", reasoningEffort: "" })}><option value="">跟随默认 Agent</option>{capabilities?.agents.map(agent => <option key={agent.agentId} value={agent.agentId} disabled={!agent.available}>{agent.name ?? agent.agentId}{agent.state && agent.state !== "ready" ? `（${label(agent.state)}）` : !agent.available ? "（不可用）" : ""}</option>)}</select></label>
      {capability?.reason && <p>{capability.reason}</p>}
      {capability?.supportsProfiles === false ? <p>认证：使用项目目标环境的 CLI 登录与配置。</p> : <label>认证档案<select aria-label={`${label(role)}认证档案`} value={config.profileId ?? ""} onChange={e => update({ profileId: e.target.value })}><option value="">跟随项目认证 / CLI 登录</option>{capabilities?.profiles?.filter(profile => profile.agentId === agentId).map(profile => <option key={profile.id} value={profile.id}>{profile.name}{profile.model ? ` · ${profile.model}` : ""}</option>)}{config.profileId && !capabilities?.profiles?.some(profile => profile.id === config.profileId) && <option value={config.profileId}>原档案（当前不可选）</option>}</select></label>}
      <label>模型<input aria-label={`${label(role)}模型`} maxLength={128} placeholder="跟随认证档案 / CLI 默认" value={config.model ?? ""} onChange={e => update({ model: e.target.value })} /></label>
      {(efforts.length > 0 || config.reasoningEffort) && <label>思考强度<select aria-label={`${label(role)}思考强度`} value={config.reasoningEffort ?? ""} onChange={e => update({ reasoningEffort: e.target.value })}><option value="">CLI 默认</option>{efforts.map(effort => <option key={effort} value={effort}>{effort}</option>)}{config.reasoningEffort && !efforts.includes(config.reasoningEffort) && <option value={config.reasoningEffort}>原配置（当前不可选）</option>}</select></label>}
    </fieldset>;
  })}<p>各角色独立使用所选配置；测试执行器运行实际检查，不调用模型。调整已启动会话的配置前，请先暂停并等待执行结束。</p></div>;
}

export default function SupervisionPage() {
  const { projectId } = useParams(); const [params, setParams] = useSearchParams(); const sessionID = params.get("session") ?? "";
  const [sessions, setSessions] = useState<Session[]>([]); const [tasks, setTasks] = useState<Task[]>([]);
  const [showArchived, setShowArchived] = useState(false);
  const [capabilities, setCapabilities] = useState<Capabilities | null>(null); const [selected, setSelected] = useState<string[]>(() => params.getAll("task"));
  const [roles, setRoles] = useState<RoleConfigs>({}); const [rolesOpen, setRolesOpen] = useState(false);
  const [profilesOpen, setProfilesOpen] = useState(false);
  const [agent, setAgent] = useState("claude-code"); const [budget, setBudget] = useState<Budget | null>(null);
  const [detail, setDetail] = useState<Detail | null>(null); const [entries, setEntries] = useState<ProcessEntry[]>([]);
  const [tab, setTab] = useState("all"); const [focusTask, setFocusTask] = useState("");
  const [instruction, setInstruction] = useState(""); const [instructionTask, setInstructionTask] = useState(""); const [immediate, setImmediate] = useState(false);
  const [answers, setAnswers] = useState<Record<string, string>>({}); const [manualNote, setManualNote] = useState(""); const [manualPoint, setManualPoint] = useState(""); const [manualPassed, setManualPassed] = useState(true);
  const [error, setError] = useState(""); const [loading, setLoading] = useState(true); const [busy, setBusy] = useState(""); const [acceptDrift, setAcceptDrift] = useState(false); const [budgetOpen, setBudgetOpen] = useState(false);
  const [artifact, setArtifact] = useState<{ url: string; title: string; image: boolean } | null>(null);
  const generation = useRef(0); const [refresh, setRefresh] = useState(0);
  const defaultAgentProject = useRef<string | undefined>(undefined);
  const focusRef = useRef("");
  const [changes, setChanges] = useState<Changes | null>(null);
  const [diff, setDiff] = useState<{ path: string; before: string; after: string } | null>(null);
  useEffect(() => { focusRef.current = focusTask; }, [focusTask]);
  useEffect(() => { setSelected(params.getAll("task")); }, [projectId]);
  useEffect(() => { let alive = true; setLoading(true); setError(""); setTasks([]); setCapabilities(null);
    void Promise.all([api<Task[]>(`/api/projects/${projectId}/tasks`), api<Capabilities>(`/api/projects/${projectId}/supervision/capabilities`)]).then(([taskList, caps]) => {
      if (!alive) return; setTasks(taskList); setCapabilities(caps); setBudget(caps.defaultBudget);
      if (defaultAgentProject.current !== projectId) {
        defaultAgentProject.current = projectId;
        setAgent(caps.defaultAgentId);
      }
    }).catch(cause => { if (alive) setError(message(cause)); }).finally(() => { if (alive) setLoading(false); });
    return () => { alive = false; };
  }, [projectId, refresh]);
  useEffect(() => { let alive = true; setSessions([]);
    void api<Session[]>(`/api/projects/${projectId}/supervision${showArchived ? "?includeArchived=1" : ""}`).then(list => {
      if (alive) setSessions(list);
    }).catch(cause => { if (alive) setError(message(cause)); });
    return () => { alive = false; };
  }, [projectId, refresh, showArchived]);
  useEffect(() => {
    const current = ++generation.current; let timer: number; let disposed = false; let afterMessage = 0; let afterEvent = 0;
    setDetail(null); setEntries([]); setError(""); setFocusTask(""); focusRef.current = ""; setChanges(null); setDiff(null); setAcceptDrift(false); setBudgetOpen(false); setRolesOpen(false); setRoles({}); setAnswers({}); setArtifact(null); setBusy(""); setInstruction(""); setInstructionTask(""); setImmediate(false); setManualNote(""); setManualPoint(""); setManualPassed(true);
    if (!sessionID) return;
    const poll = async () => {
      try {
        const requestedTask = focusRef.current;
        const value = await api<Detail>(`/api/supervision/${sessionID}?taskId=${encodeURIComponent(requestedTask)}`);
        if (disposed || current !== generation.current) return;
        if (value.session.projectId !== projectId) throw Error("该托管会话不属于当前项目");
        if (requestedTask === focusRef.current) setDetail(previous => previous && previous.session.stateVersion > value.session.stateVersion ? previous : value);
        setSessions(list => { const known = list.find(item => item.id === value.session.id); return known && known.stateVersion > value.session.stateVersion ? list : [value.session, ...list.filter(item => item.id !== value.session.id)]; });
        let more = true; let pages = 0;
        while (more && pages++ < 8) {
          const page = await api<ProcessPage>(`/api/supervision/${sessionID}/process?afterMessage=${afterMessage}&afterEvent=${afterEvent}`);
          if (disposed || current !== generation.current) return;
          setEntries(previous => mergeProcess(previous, [...page.messages, ...page.events])); afterMessage = page.afterMessage; afterEvent = page.afterEvent; more = page.hasMore;
        }
      } catch (cause) { if (!disposed) setError(message(cause)); }
      if (!disposed) timer = window.setTimeout(() => void poll(), 2000);
    };
    void poll(); return () => { disposed = true; window.clearTimeout(timer); };
  }, [sessionID, projectId]);
  useEffect(() => () => { if (artifact) URL.revokeObjectURL(artifact.url); }, [artifact]);
  async function act(name: string, work: (stillCurrent: () => boolean) => Promise<unknown>) {
    const current = generation.current; setBusy(name); setError("");
    try { await work(() => current === generation.current); if (current === generation.current && sessionID) { const requestedTask = focusRef.current; const value = await api<Detail>(`/api/supervision/${sessionID}?taskId=${encodeURIComponent(requestedTask)}`); if (current === generation.current && requestedTask === focusRef.current) setDetail(previous => previous && previous.session.stateVersion > value.session.stateVersion ? previous : value); } }
    catch (cause) { if (current === generation.current) setError(message(cause)); }
    finally { if (current === generation.current) setBusy(""); }
  }
  const post = (path: string, body: unknown) => supervisionWrite(`/api/supervision/${sessionID}/${path}`, "POST", body);
  const ss = detail?.session; const active = ss && !terminal(ss.state) && ss.state !== "draft";
  const protectionHeld = !!detail?.protection && !["released", "not_acquired"].includes(detail.protection.state);
  const eligibleTasks = tasks.filter(task => task.status === "todo" || task.status === "action_required");
  const eligibleIDs = new Set(eligibleTasks.map(task => task.id));
  const selectedTaskIDs = [...new Set(selected)].filter(id => eligibleIDs.has(id));
  const skippedSelections = new Set(selected).size - selectedTaskIDs.length;
  const sessionOptions = ss && ss.projectId === projectId
    ? [ss, ...sessions.filter(item => item.id !== ss.id)] : sessions;
  const focused = detail?.tasks.find(task => task.taskId === focusTask) ?? detail?.tasks[0]; const plan = detail?.plans.find(item => item.taskId === focused?.taskId);
  const visibleEntries = entries.filter(entry => (tab === "all" || entry.agentRole === tab) && (!focusTask || entry.taskId === focusTask || !entry.taskId));
  const chooseSession = (id: string) => setParams(id ? { session: id } : {});
  async function openArtifact(execution: string, asset: string) {
    const current = generation.current; const response = await fetch(apiURL(`/api/supervision/${sessionID}/artifacts/${execution}/${asset}`), { headers: sessionHeaders() });
    if (!response.ok) throw Error("无法读取浏览器产物"); const blob = await response.blob();
    if (current === generation.current) setArtifact({ url: URL.createObjectURL(blob), title: asset, image: asset.endsWith(".png") });
  }
  async function loadChanges() {
    const current = generation.current;
    const value = await api<Changes>(`/api/supervision/${sessionID}/changes`);
    if (current === generation.current) setChanges(value);
  }
  async function loadDiff(change: Changes["changes"][number]) {
    if (!changes) return;
    const current = generation.current;
    const read = async (revision: string, hash: string) => {
      if (!hash) return "（文件不存在）";
      const response = await fetch(apiURL(`/api/supervision/${sessionID}/source?revisionId=${revision}&path=${encodeURIComponent(change.path)}`), { headers: sessionHeaders() });
      if (!response.ok) throw Error("无法读取该版本文件");
      return response.text();
    };
    const [before, after] = await Promise.all([read(changes.baselineRevisionId, change.beforeSha256), read(changes.currentRevisionId, change.afterSha256)]);
    if (current === generation.current) setDiff({ path: change.path, before, after });
  }
  return <section id="workspace-panel-supervision" role="tabpanel" className="supervision-page" aria-labelledby="workspace-tab-supervision">
    <header className="supervision-heading"><div><h2 id="supervision-title">任务托管</h2><p>全局 Agent 负责计划、派发与验收，实施和测试分别执行。</p></div><div className="supervision-actions"><select aria-label="托管会话" value={sessionID} onChange={e => chooseSession(e.target.value)}><option value="">新建托管</option>{sessionOptions.filter(item => showArchived || !item.archivedAt || item.id === sessionID).map(item => <option key={item.id} value={item.id}>{time(item.createdAt)} · {label(item.state)}{item.archivedAt ? " · 已归档" : ""}</option>)}</select><label><input type="checkbox" checked={showArchived} onChange={e => setShowArchived(e.target.checked)} />显示已归档</label><button onClick={() => { if (sessionID) chooseSession(""); else setRefresh(v => v + 1); }}>新建 / 刷新</button></div></header>
    {error && <div className="supervision-error" role="alert">{error}<button onClick={() => setError("")}>关闭</button></div>}{loading && <p role="status">正在读取任务与托管能力…</p>}
    {projectId && <WorkspaceOperations key={projectId} projectId={projectId} />}
    {capabilities?.target && <button type="button" disabled={!!busy || !capabilities.target.targetId} onClick={() => setProfilesOpen(true)}>管理当前目标的托管认证档案</button>}
    {profilesOpen && capabilities?.target && projectId && <SupervisionProfiles key={`${projectId}:${capabilities.target.targetId}`} projectId={projectId} runnerId={capabilities.target.runnerId} targetId={capabilities.target.targetId} close={() => setProfilesOpen(false)} changed={() => setRefresh(value => value + 1)} />}
    {!sessionID && !loading && <form className="supervision-create" onSubmit={e => { e.preventDefault(); if (!budget || !selectedTaskIDs.length || selectedTaskIDs.length > (capabilities?.maxTasks ?? 500)) return; const current = generation.current; void act("create", async () => { const created = await supervisionWrite<Session>(`/api/projects/${projectId}/supervision`, "POST", { taskIds: selectedTaskIDs, schedulerAgentId: agent, roles, budget }); if (current !== generation.current) return; setSessions(list => [created, ...list]); chooseSession(created.id); }); }}>
      <div><h3>选择要完成的任务</h3><p>可以选择互不关联的任务；Agent 会根据依赖安排执行顺序。仅支持待办或需要处理的任务。</p>{skippedSelections > 0 && <p role="status">已排除 {skippedSelections} 项当前不可托管或已不存在的预选任务。</p>}<div className="supervision-task-picker">{eligibleTasks.map(task => <label key={task.id}><input type="checkbox" checked={selected.includes(task.id)} onChange={e => setSelected(ids => e.target.checked ? [...ids, task.id] : ids.filter(id => id !== task.id))} /><span>{task.title}</span><small>{task.status}</small></label>)}{tasks.length === 0 && <p>当前项目还没有任务。<Link to="../tasks/board">前往任务看板</Link></p>}</div></div>
      <div><h3>调度与资源</h3>{capabilities?.target && <p>项目执行环境：{capabilities.target.environment} · {capabilities.target.runnerId}{capabilities.target.reason && ` · ${capabilities.target.reason}`}</p>}<label>项目环境 Agent<select value={agent} onChange={e => setAgent(e.target.value)}>{capabilities?.agents.map(item => <option key={item.agentId} value={item.agentId} disabled={!item.available}>{item.name ?? item.agentId}{item.state && item.state !== "ready" ? `（${label(item.state)}）` : !item.available ? "（不可用）" : ""}</option>)}</select></label><p>复用项目目标环境的 CLI 登录和项目认证配置。{capabilities?.agents.find(item => item.agentId === agent)?.reason}</p><RoleFields value={roles} change={setRoles} capabilities={capabilities} fallback={agent} />{budget && <BudgetFields value={budget} change={setBudget} />}<button className="primary" disabled={!!busy || !selectedTaskIDs.length || selectedTaskIDs.length > (capabilities?.maxTasks ?? 500) || !capabilities?.agents.find(item => item.agentId === agent)?.available}>创建托管草稿（{selectedTaskIDs.length} 项）</button></div>
    </form>}
    {sessionID && !detail && <p role="status">正在回读托管状态与历史过程…</p>}
    {ss && detail && <>
      <div className="supervision-status"><div><strong>{label(ss.state)}</strong><span>调用 {ss.usedTurns} / {ss.budget.maxAgentTurns}（执行预占 {ss.reservedTurns}；最终验收保留 {ss.budget.finalReservePercent}%）</span>{detail.storage && <span title="统计源码、对话、计划、证据和产物的逻辑字节；共享数据库页、索引和 WAL 不计入会话预算">存储 {(detail.storage.bytes / 1048576).toFixed(1)} / {Math.round(detail.storage.limitBytes / 1048576)} MiB{detail.storage.reservedBytes ? `（产物预占 ${(detail.storage.reservedBytes / 1048576).toFixed(1)} MiB）` : ""}{detail.storage.diskFreeBytes !== undefined ? ` · 磁盘可用 ${(detail.storage.diskFreeBytes / 1073741824).toFixed(1)} GiB` : ""}</span>}<span>活动 {Math.ceil(ss.activeMillis / 60000)} / {Math.ceil(ss.budget.maxActiveMillis / 60000)} 分钟</span>{ss.reason && <code title={ss.reason}>{label(ss.reason)}</code>}{ss.recoveryState !== "ready" && <span>恢复核对：{label(ss.recoveryState)}</span>}</div><div className="supervision-actions">
        {ss.state === "draft" && <button className="primary" disabled={!!busy} onClick={() => void act("start", () => post("start", { stateVersion: ss.stateVersion }))}>启动托管</button>}
        {active && ["running", "finalizing"].includes(ss.state) && <button disabled={!!busy} onClick={() => void act("pause", () => post("control", { stateVersion: ss.stateVersion, action: "pause" }))}>暂停</button>}
        {active && ["paused", "needs_input"].includes(ss.state) && <button disabled={!!busy} onClick={() => void act("resume", () => post("control", { stateVersion: ss.stateVersion, action: "resume", acceptDrift }))}>核对并继续</button>}
        {active && <button disabled={!!busy || ss.control === "stop"} onClick={() => void act("stop", () => post("control", { stateVersion: ss.stateVersion, action: "stop" }))}>停止并交还任务</button>}
        {["draft", "paused", "needs_input"].includes(ss.state) && <button disabled={!!busy || ss.recoveryState !== "ready"} onClick={() => { setRoles(ss.roles ?? {}); setRolesOpen(v => !v); }}>角色配置</button>}
        {!terminal(ss.state) && <button onClick={() => { setBudget(ss.budget); setBudgetOpen(v => !v); }}>调整预算</button>}
      </div></div>
      {active && ["paused", "needs_input"].includes(ss.state) && <label className="supervision-drift"><input type="checkbox" checked={acceptDrift} onChange={e => setAcceptDrift(e.target.checked)} />接受当前目录 / 规则变更并重新验收（保留启动基线与历史）</label>}
      {detail.wait && <p role="status" className="supervision-wait">正在等待：{detail.wait.condition} · 下次复查：{time(detail.wait.nextCheckAt)}（每30秒核对目录与依赖，复查到点后重新判断；连续三轮无新事实将交还处理）</p>}
      {budgetOpen && budget && <form className="supervision-budget-edit" onSubmit={e => { e.preventDefault(); void act("budget", async stillCurrent => { await supervisionWrite(`/api/supervision/${sessionID}/budget`, "PUT", { stateVersion: ss.stateVersion, budget }); if (stillCurrent()) setBudgetOpen(false); }); }}><BudgetFields value={budget} change={setBudget} /><button disabled={!!busy}>保存预算</button></form>}
      {rolesOpen && <form className="supervision-budget-edit" onSubmit={e => { e.preventDefault(); void act("roles", async stillCurrent => { await supervisionWrite(`/api/supervision/${sessionID}/roles`, "PUT", { stateVersion: ss.stateVersion, roles }); if (stillCurrent()) setRolesOpen(false); }); }}><RoleFields value={roles} change={setRoles} capabilities={capabilities} fallback={ss.schedulerAgentId ?? "claude-code"} /><button disabled={!!busy}>保存角色配置</button></form>}
      <Raw title="问题与回答历史（过期回答不触发执行）" value={detail.questions} />
      <Raw title="当前角色配置" value={{ defaultAgentId: ss.schedulerAgentId, roles: ss.roles ?? {} }} />
      <div className="supervision-grid"><aside className="supervision-tasks"><h3>选中任务 <small>{detail.tasks.length}</small></h3><button className={!focusTask ? "selected" : ""} onClick={() => setFocusTask("")}>查看全部过程</button>{detail.tasks.map(task => <button className={focusTask === task.taskId ? "selected" : ""} key={task.taskId} onClick={() => { setFocusTask(task.taskId); setManualPoint(""); }}><strong>{task.taskSnapshot.title ?? task.taskId}</strong><span>{label(task.phase)} · 证据{label(task.evidenceFreshness)}</span><small>要求 v{task.requirementsRevision} / 计划 v{task.planRevision} · 修复 {task.fixRounds}</small></button>)}</aside>
      <main className="supervision-process"><div className="supervision-process-tabs" role="tablist" aria-label="Agent 过程">{[["all", "全部过程"], ["scheduler", "调度对话"], ["implementation", "实施"], ["test", "测试"], ["review", "审查"], ["events", "调度事件"]].map(([key, title]) => <button role="tab" aria-selected={tab === key} key={key} onClick={() => setTab(key)}>{title}</button>)}</div>
        <div className="supervision-transcript" role="log" aria-live="off">{tab !== "events" ? <>{visibleEntries.length === 0 && <p className="supervision-empty">{ss.state === "draft" ? "启动后，这里会持续保存各角色的下发内容、对话和实际执行结果。" : "暂无该角色的过程记录，任务状态仍会自动刷新。"}</p>}{visibleEntries.map(entry => <article key={entry.id} className={`supervision-message ${entry.role === "user" ? "input" : "output"}`}><header><strong>{label(entry.agentRole)}{entry.role === "user" ? " · 输入" : ""}</strong><time>{time(entry.createdAt)}</time></header>{entry.taskId && <small>{detail.tasks.find(task => task.taskId === entry.taskId)?.taskSnapshot.title ?? entry.taskId}</small>}<AgentContent entry={entry} />{entry.conversationId && <Link to={`../conversations/${entry.conversationId}`}>原始角色会话</Link>}</article>)}</> : <>{[...detail.events].reverse().map(event => <article key={event.seq}><header>#{event.seq} {event.type} <time>{time(event.createdAt)}</time></header><Raw title="事件内容" value={event.payload} /></article>)}{detail.eventsTruncated && <p>仅显示最近 150 条领域事件；角色对话与执行过程从历史分页完整回读。</p>}</>}</div>
        {active && <form className="supervision-instruction" onSubmit={e => { e.preventDefault(); void act("instruction", async stillCurrent => { await post("instructions", { stateVersion: ss.stateVersion, taskId: instructionTask, content: instruction, immediate }); if (stillCurrent()) setInstruction(""); }); }}><label>补充要求<textarea required maxLength={16000} value={instruction} onChange={e => setInstruction(e.target.value)} placeholder="补充目标、限制或发现的问题…" /></label><div className="supervision-actions"><select aria-label="补充要求范围" value={instructionTask} onChange={e => setInstructionTask(e.target.value)}><option value="">全局要求</option>{detail.tasks.map(task => <option key={task.taskId} value={task.taskId}>{task.taskSnapshot.title}</option>)}</select><label><input type="checkbox" checked={immediate} onChange={e => setImmediate(e.target.checked)} />中断当前回合后处理</label><button disabled={!!busy || !instruction.trim()}>发送要求</button></div></form>}
      </main>
      <aside className="supervision-inspector"><h3>{focused?.taskSnapshot.title ?? "计划与验收"}</h3>{focused && <p>{focused.taskSnapshot.description}</p>}{plan ? <><p>{plan.plan.goal}</p><small>计划 v{plan.revision} · {label(plan.status)}</small><Raw title="执行范围与依赖" value={{ scope: plan.plan.scope, dependsOn: plan.plan.dependsOn, environment: plan.plan.environment, limitations: plan.plan.limitations }} /><h4>验收要求</h4>{plan.plan.acceptance.map(point => <div className="supervision-point" key={point.id}><strong>{point.id} · {point.critical ? "关键 · " : ""}{label(point.kind)}</strong><p>{point.requirement}</p><small>{point.verification}</small></div>)}</> : <p>Agent 尚未生成任务计划。</p>}
        {detail.questions.filter(q => !q.answered && !q.expired).map(q => <form className="supervision-question" key={q.id} onSubmit={e => { e.preventDefault(); void act("answer", () => post(`questions/${q.id}/answer`, { answer: answers[q.id] })); }}><strong>Agent 需要确认</strong><p>{q.question}</p><textarea aria-label="回答 Agent" required value={answers[q.id] ?? ""} onChange={e => setAnswers(values => ({ ...values, [q.id]: e.target.value }))} /><button disabled={!!busy}>提交回答</button></form>)}
        <h4>验证证据</h4>{detail.evidence.filter(item => item.taskId === focused?.taskId).map(item => <div className="supervision-evidence" key={item.id}><strong>{label(item.kind)} · {item.passed ? "通过" : "未通过"}</strong><small>{item.revisionId === detail.projectContext?.revisionId && item.planRevision === focused?.planRevision && item.requirementsRevision === focused?.requirementsRevision ? "当前版本" : "历史 / 已过期"} · {time(item.createdAt)}</small><Raw title="实际执行与判定依据" value={item.details} />{item.kind === "browser" && (detail.artifacts?.some(asset => asset.executionId === item.executionId && asset.state === "cleaned") ? <p>原始产物已清理，结构化报告与哈希保留。</p> : <div className="supervision-actions"><button onClick={() => void act("artifact", () => openArtifact(item.executionId, "screenshot.png"))}>截图</button><button onClick={() => void act("artifact", () => openArtifact(item.executionId, "report.json"))}>报告</button></div>)}</div>)}
        {active && plan?.plan.acceptance.some(point => point.kind === "manual") && <form onSubmit={e => { e.preventDefault(); void act("manual", async stillCurrent => { await post("manual-evidence", { stateVersion: ss.stateVersion, taskId: focused?.taskId, pointId: manualPoint, note: manualNote, passed: manualPassed }); if (stillCurrent()) setManualNote(""); }); }}><h4>记录人工验收</h4><select aria-label="人工验收点" required value={manualPoint} onChange={e => setManualPoint(e.target.value)}><option value="">选择验收点</option>{plan.plan.acceptance.filter(point => point.kind === "manual").map(point => <option key={point.id} value={point.id}>{point.id} · {point.requirement}</option>)}</select><textarea aria-label="实际操作与结果" placeholder="记录实际操作和结果" required value={manualNote} onChange={e => setManualNote(e.target.value)} /><label><input type="checkbox" checked={manualPassed} onChange={e => setManualPassed(e.target.checked)} />人工验收通过</label><button disabled={!!busy}>保存记录</button></form>}
        {detail.evidenceTruncated && <p>仅显示最近100条证据；完整执行结果可在过程历史中回读。</p>}
        <h4>工作区代码变更</h4><button disabled={!!busy || !detail.projectContext} onClick={() => void act("changes", loadChanges)}>比较启动基线与当前快照</button>
        {changes && <div>{changes.changes.length === 0 && <p>暂无代码差异。</p>}{changes.changes.map(change => <div key={change.path}><button disabled={!!busy || !change.readable || Math.max(change.beforeSize, change.afterSize) > 320 * 1024} onClick={() => void act("diff", () => loadDiff(change))}>{change.path}</button><small>{change.beforeSha256.slice(0, 8)} → {change.afterSha256.slice(0, 8)}</small></div>)}</div>}
        {diff && <details open><summary>{diff.path}</summary><strong>启动基线</strong><pre>{diff.before}</pre><strong>当前版本</strong><pre>{diff.after}</pre></details>}
        {detail.projectContext && <Raw title="代码快照与项目规则" value={detail.projectContext} />}<Raw title="角色派发与结果历史" value={detail.jobs.filter(job => !focusTask || job.taskId === focusTask)} />
      </aside></div>
      {terminal(ss.state) && detail.protection && <ProtectionSummary state={detail.protection.state} />}
      {terminal(ss.state) && <section className="supervision-report"><h3>托管结果：{label(ss.state)}</h3><p>候选通过 {detail.tasks.filter(task => task.phase === "accepted").length} / {detail.tasks.length} 项。工作区成果及全部过程已保留。</p>{ss.archivedAt && <p>已归档，过程与证据保留。</p>}<button disabled={!!busy || protectionHeld} onClick={() => void act("archive", () => supervisionWrite(`/api/supervision/${sessionID}/archive`, "PUT", { stateVersion: ss.stateVersion, archived: !ss.archivedAt }))}>{ss.archivedAt ? "取消归档" : "归档会话"}</button><button disabled={!!busy || protectionHeld || !detail.artifacts?.some(asset => asset.state === "present")} onClick={() => void act("clean", () => api(`/api/supervision/${sessionID}/artifacts`, { method: "DELETE" }))}>清理原始截图与报告文件（保留结构化结果）</button>{detail.tasks.filter(task => task.phase !== "accepted").map(task => <p key={task.taskId}>{task.taskSnapshot.title}：{label(task.phase)}</p>)}<Raw title="未完成原因与收口记录" value={detail.events.filter(event => ["task.disposition", "session.completed", "session.completed_with_issues", "session.failed", "session.stopped"].includes(event.type))} /></section>}
    </>}
    {artifact && <dialog open className="supervision-artifact" aria-label={artifact.title}><button onClick={() => setArtifact(null)}>关闭</button><a href={artifact.url} download={artifact.title}>下载 {artifact.title}</a>{artifact.image ? <img src={artifact.url} alt="浏览器验收截图" /> : <iframe src={artifact.url} title="浏览器执行报告" sandbox="" />}</dialog>}
  </section>;
}
