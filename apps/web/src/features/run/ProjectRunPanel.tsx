import { useCallback, useEffect, useRef, useState } from "react";
import { type LogEntry, type RunConfig, type RunStatusResponse, runLogPresentation, runLogText, statusLabels } from "./run-model";
import { type AnsiSegment, toAnsiSegments } from "./ansi";
import { linkifyText } from "./linkify";
import { createWebSocket, openExternal } from "../../lib/runtime";

type Request = <T>(path: string, init?: RequestInit) => Promise<T>;
type OrchestrationWorktree = { taskId: string; jobId: string; taskBranch: string; worktreePath: string; status: string };

const MAX_LOG_ENTRIES = 2_000;
const LOG_BOTTOM_THRESHOLD = 64;
const RECONNECT_MAX_DELAY = 10_000;

function mergeLogs(previous: LogEntry[], entries: LogEntry[]): LogEntry[] {
	let merged = previous;
	for (const entry of entries) {
		const index = entry.id > 0 ? merged.findIndex((item) => item.id === entry.id) : -1;
		if (index >= 0) {
			const current = merged[index];
			if (current.timestamp === entry.timestamp && current.stream === entry.stream && current.text === entry.text) continue;
			merged = [...merged];
			merged[index] = entry;
			continue;
		}
		if (merged === previous) merged = [...previous];
		merged.push(entry);
	}
	if (merged === previous) return previous;
	return merged.sort((a, b) => a.id - b.id).slice(-MAX_LOG_ENTRIES);
}

function environmentValidationError(envVars: Record<string, string>): string | null {
	for (const [key, value] of Object.entries(envVars)) {
		if (!key || key.includes("=") || key.includes("\0")) return "环境变量名称不能为空，且不能包含 = 或 NUL 字符";
		if (value.includes("\0")) return "环境变量值不能包含 NUL 字符";
	}
	return null;
}

function nextEnvironmentVariableKey(envVars: Record<string, string>): string {
	let suffix = 1;
	let key = "ENV_VAR";
	while (Object.prototype.hasOwnProperty.call(envVars, key)) {
		suffix += 1;
		key = `ENV_VAR_${suffix}`;
	}
	return key;
}

/** 渲染单条日志中的一个文本片段：把其中的 http/https 地址渲染成可点击链接。
 *  保持 ANSI 颜色类在链接上，未着色的链接继承行文字颜色。 */
function LogSegment({ segment }: { segment: AnsiSegment }) {
	const parts = linkifyText(segment.text);
	if (parts.length === 1 && !parts[0].url) {
		return <span className={segment.className}>{parts[0].text}</span>;
	}
	return <>{parts.map((part, index) => {
		if (part.url) {
			const href = part.url;
			return <a key={index} href={href} className={`run-log-link${segment.className ? ` ${segment.className}` : ""}`} rel="noreferrer" onClick={(event) => { event.preventDefault(); void openExternal(href); }}>{part.text}</a>;
		}
		return <span key={index} className={segment.className}>{part.text}</span>;
	})}</>;
}

export function ProjectRunPanel({ projectID, request, fail, active, isRemote = false }: { projectID: string; request: Request; fail: (message: string) => void; active: boolean; isRemote?: boolean }) {
	const [config, setConfig] = useState<RunConfig>({ workDir: "", command: "", envVars: {}, executionTarget: "auto" });
	const [status, setStatus] = useState<RunStatusResponse | null>(null);
	const [worktrees, setWorktrees] = useState<OrchestrationWorktree[]>([]);
	const [worktreeSelection, setWorktreeSelection] = useState("");
	const [logs, setLogs] = useState<LogEntry[]>([]);
	const [hasNewLogs, setHasNewLogs] = useState(false);
	const [busy, setBusy] = useState("");
	const logTerminalRef = useRef<HTMLDivElement>(null);
	const logNearBottomRef = useRef(true);
	const wsRef = useRef<WebSocket | null>(null);
	const pollRef = useRef<ReturnType<typeof setInterval> | null>(null);
	const reconnectTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
	const reconnectAttemptRef = useRef(0);
	const activeProjectIDRef = useRef<string | null>(null);
	const clearedThroughLogIDRef = useRef(0);
	const configDirtyRef = useRef(false);
	const configRevisionRef = useRef(0);
	const statusRequestVersionRef = useRef(0);
	const renderedProjectIDRef = useRef(projectID);

	const basePath = `/api/projects/${projectID}/run`;
	// 运行环境为 Windows cmd 时提示写法（反斜杠、不加 ./ 前缀）。优先用后端解析后的
	// 真实目标（status.executionTarget，SSH 远端为 ""），未加载时退回配置近似判断。
	const resolvedTarget = status?.executionTarget ?? (isRemote ? "" : config.executionTarget || "auto");
	const isWindowsTarget = resolvedTarget === "windows";
	const mergeIncomingLogs = useCallback((entries: LogEntry[]) => {
		const visibleEntries = entries.filter((entry) => entry.id <= 0 || entry.id > clearedThroughLogIDRef.current);
		setLogs((prev) => mergeLogs(prev, visibleEntries));
	}, []);

	const loadConfig = useCallback(async () => {
		const revision = configRevisionRef.current;
		try {
			const c = await request<RunConfig>(`${basePath}/config`);
			if (activeProjectIDRef.current === projectID && revision === configRevisionRef.current && !configDirtyRef.current) setConfig(c);
		}
		catch (cause) {
			if (activeProjectIDRef.current === projectID) fail(cause instanceof Error ? cause.message : "加载配置失败");
		}
	}, [basePath, request, fail, projectID]);
	const loadWorktrees = useCallback(async () => {
		try {
			const items = await request<OrchestrationWorktree[]>(`${basePath}/worktrees`);
			if (activeProjectIDRef.current === projectID) setWorktrees(items);
		} catch { /* 普通项目仍可使用项目根目录启动。 */ }
	}, [basePath, request, projectID]);
	const updateConfig = (next: RunConfig) => {
		configDirtyRef.current = true;
		configRevisionRef.current += 1;
		setConfig(next);
	};

	const loadStatus = useCallback(async () => {
		const requestVersion = ++statusRequestVersionRef.current;
		try {
			const s = await request<RunStatusResponse>(`${basePath}/status`);
			if (activeProjectIDRef.current !== projectID || requestVersion !== statusRequestVersionRef.current) return;
			setStatus(s);
			mergeIncomingLogs(s.recentLogs);
		} catch { /* 轮询忽略错误 */ }
	}, [basePath, request, projectID, mergeIncomingLogs]);

	useEffect(() => {
		if (renderedProjectIDRef.current === projectID) return;
		renderedProjectIDRef.current = projectID;

		configDirtyRef.current = false;
		configRevisionRef.current = 0;
		statusRequestVersionRef.current += 1;
		clearedThroughLogIDRef.current = 0;
		logNearBottomRef.current = true;
		setConfig({ workDir: "", command: "", envVars: {}, executionTarget: "auto" });
		setWorktreeSelection("");
		setStatus(null);
		setLogs([]);
		setHasNewLogs(false);
		setBusy("");
	}, [projectID]);

	useEffect(() => {
		if (!active) return;
		let disposed = false;
		activeProjectIDRef.current = projectID;
		void loadConfig();
		void loadStatus();
		void loadWorktrees();

		const connect = () => {
			if (disposed) return;
			const ws = createWebSocket(`/ws/projects/${projectID}/run`);
			wsRef.current = ws;

			ws.onmessage = (raw: MessageEvent) => {
				if (disposed) return;
				try { mergeIncomingLogs([JSON.parse(raw.data) as LogEntry]); }
				catch { /* 忽略无法解析的消息 */ }
			};
			ws.onopen = () => {
				if (disposed) { ws.close(); return; }
				reconnectAttemptRef.current = 0;
				void loadStatus();
			};
			ws.onerror = () => { if (!disposed) ws.close(); };
			ws.onclose = () => {
				if (wsRef.current === ws) wsRef.current = null;
				if (disposed) return;
				const delay = Math.min(1_000 * 2 ** reconnectAttemptRef.current, RECONNECT_MAX_DELAY);
				reconnectAttemptRef.current += 1;
				reconnectTimerRef.current = setTimeout(() => {
					reconnectTimerRef.current = null;
					void loadStatus();
					connect();
				}, delay);
			};
		};
		connect();

		pollRef.current = setInterval(() => { void loadStatus(); }, 10000);

		return () => {
			disposed = true;
			if (activeProjectIDRef.current === projectID) activeProjectIDRef.current = null;
			if (reconnectTimerRef.current) { clearTimeout(reconnectTimerRef.current); reconnectTimerRef.current = null; }
			wsRef.current?.close();
			wsRef.current = null;
			if (pollRef.current) { clearInterval(pollRef.current); pollRef.current = null; }
		};
	}, [projectID, active, loadConfig, loadStatus, loadWorktrees, mergeIncomingLogs]);

	useEffect(() => {
		const terminal = logTerminalRef.current;
		if (!terminal) return;
		if (logNearBottomRef.current) {
			terminal.scrollTo({ top: terminal.scrollHeight, behavior: "auto" });
			setHasNewLogs(false);
		} else {
			setHasNewLogs(true);
		}
	}, [logs]);

	const handleLogScroll = () => {
		const terminal = logTerminalRef.current;
		if (!terminal) return;
		const nearBottom = terminal.scrollHeight - terminal.scrollTop - terminal.clientHeight <= LOG_BOTTOM_THRESHOLD;
		logNearBottomRef.current = nearBottom;
		if (nearBottom) setHasNewLogs(false);
	};

	const jumpToLatestLogs = () => {
		const terminal = logTerminalRef.current;
		if (!terminal) return;
		logNearBottomRef.current = true;
		terminal.scrollTo({ top: terminal.scrollHeight, behavior: "smooth" });
		setHasNewLogs(false);
	};

	const saveConfig = async () => {
		const validationError = environmentValidationError(config.envVars || {});
		if (validationError) { fail(validationError); return; }
		setBusy("save");
		const revision = configRevisionRef.current;
		// 远程项目的执行环境固定为 auto，避免旧值被持久化。
		const payload: RunConfig = isRemote ? { ...config, executionTarget: "auto" } : config;
		try {
			await request(`${basePath}/config`, { method: "PUT", body: JSON.stringify(payload) });
			if (revision === configRevisionRef.current) configDirtyRef.current = false;
		}
		catch (cause) { fail(cause instanceof Error ? cause.message : "保存配置失败"); }
		finally { setBusy(""); }
	};

	const handleStart = async () => {
		setBusy("start");
		try { await request(`${basePath}/start`, { method: "POST" }); void loadStatus(); }
		catch (cause) { fail(cause instanceof Error ? cause.message : "启动失败"); }
		finally { setBusy(""); }
	};

	const handleStop = async () => {
		setBusy("stop");
		try { await request(`${basePath}/stop`, { method: "POST" }); void loadStatus(); }
		catch (cause) { fail(cause instanceof Error ? cause.message : "停止失败"); }
		finally { setBusy(""); }
	};

	const handleRestart = async () => {
		setBusy("restart");
		try { await request(`${basePath}/restart`, { method: "POST" }); void loadStatus(); }
		catch (cause) { fail(cause instanceof Error ? cause.message : "重启失败"); }
		finally { setBusy(""); }
	};

	const handleClearLogs = async () => {
		setBusy("clear-logs");
		try {
			const next = await request<RunStatusResponse>(`${basePath}/logs/clear`, { method: "POST" });
			if (activeProjectIDRef.current !== projectID) return;
			// Only after the server confirms the clear should the local log state
			// advance, otherwise a failed request would leave the UI empty while
			// the server still retains the logs (and loadStatus would filter them).
			setLogs((prev) => {
				for (const entry of prev) clearedThroughLogIDRef.current = Math.max(clearedThroughLogIDRef.current, entry.id);
				return [];
			});
			setHasNewLogs(false);
			setStatus(next);
		} catch (cause) {
			fail(cause instanceof Error ? cause.message : "清除日志失败");
			void loadStatus();
		} finally {
			setBusy("");
		}
	};

	const running = status?.status === "running";
	const transitioning = status?.status === "starting" || status?.status === "stopping";
	const statusLabel = status ? statusLabels[status.status] : statusLabels.stopped;
	const statusKey = status?.status || "stopped";
	const uptime = status?.startedAt ? formatUptime(new Date(status.startedAt)) : "";

	return <section id="workspace-panel-run" className="run-panel workspace-panel" role="tabpanel" aria-labelledby="workspace-tab-run" hidden={!active}>
			<div className="run-body">
				<section className="run-log-section">
					<header>
						<div className="run-section-heading"><span className="run-section-mark"><TerminalIcon /></span><div><h3>运行日志</h3><small>{logs.length ? `${logs.length} 条输出` : "等待服务输出"}</small></div></div>
						<button type="button" className="run-log-clear" title="清除当前日志" aria-label="清除当前日志" disabled={logs.length === 0 || busy !== ""} onClick={() => void handleClearLogs()}><ClearIcon /></button>
					</header>
					<div className="run-log-terminal-wrap">
						<div ref={logTerminalRef} className="run-log-terminal" onScroll={handleLogScroll}>
						{logs.length === 0 ? <div className="run-log-empty">尚未有日志输出。配置并启动命令后，日志将显示在此处。</div> : logs.map((entry, i) => {
							const presentation = runLogPresentation(entry);
							return <div key={entry.id || `${entry.timestamp}-${i}`} className={`run-log-line ${entry.stream} ${presentation.tone}`}>
								<time>{new Date(entry.timestamp).toLocaleTimeString()}</time>
									<span className="run-log-stream" aria-label={presentation.label}>{presentation.label}</span>
								<pre>{toAnsiSegments(runLogText(entry)).map((segment, index) => <LogSegment key={index} segment={segment} />)}</pre>
							</div>;
						})}
						</div>
						{hasNewLogs && <button type="button" className="run-log-jump" title="跳转到最新日志" aria-label="跳转到最新日志" onClick={jumpToLatestLogs}><DownIcon /></button>}
					</div>
				</section>

				<aside className="run-sidebar">
					<section className="run-controls">
						<header className="run-sidebar-heading"><div><span>服务状态</span><h3>项目进程</h3></div><div className={`run-status-badge ${statusKey}`} aria-live="polite"><i></i>{statusLabel}</div></header>
						<div className="run-status-details">
							<span><small>进程</small><b>{status?.pid ? `PID ${status.pid}` : "尚未启动"}</b></span>
							<span><small>运行时长</small><b>{uptime || "-"}</b></span>
							{status?.exitCode !== null && status?.exitCode !== undefined && status.status !== "running" ? <span className="run-exit-code"><small>退出码</small><b>{status.exitCode}</b></span> : null}
						</div>
						<div className="run-actions">
							<button type="button" className="primary" disabled={running || transitioning || busy !== ""} onClick={handleStart}><PlayIcon />{busy === "start" ? "启动中" : "启动"}</button>
							<button type="button" className="danger" disabled={!running || busy !== ""} onClick={handleStop}><StopIcon />{busy === "stop" ? "停止中" : "停止"}</button>
							<button type="button" className="secondary" disabled={busy !== ""} onClick={handleRestart}><RestartIcon />{busy === "restart" ? "重启中" : "重启"}</button>
						</div>
					</section>

					<section className="run-config">
						<header className="run-sidebar-heading"><div><span>启动配置</span><h3>命令与环境</h3></div><button type="button" className="run-save-config" disabled={busy === "save"} onClick={saveConfig}>{busy === "save" ? "保存中" : "保存"}</button></header>
						<div className="run-config-row">
							<label htmlFor="run-execution-target">运行环境</label>
							<select id="run-execution-target" value={isRemote ? "auto" : (config.executionTarget || "auto")} onChange={(e) => updateConfig({ ...config, executionTarget: e.target.value as RunConfig["executionTarget"] })} disabled={isRemote}>
								<option value="auto">{isRemote ? "远程" : "自动"}</option>
								{!isRemote && <option value="windows">Windows</option>}
								{!isRemote && <option value="wsl">WSL</option>}
							</select>
						</div>
						<div className="run-config-row">
							<label>工作目录</label>
							<input type="text" value={config.workDir} placeholder={isRemote ? "远程工作目录，例如 /srv/app" : "留空使用项目根目录，也可填写项目子目录"} onChange={(e) => { setWorktreeSelection(""); updateConfig({ ...config, workDir: e.target.value }); }} />
							{!isRemote && worktrees.length > 0 && <><select value={worktreeSelection} onChange={(e) => { const value = e.target.value; setWorktreeSelection(value); if (value) updateConfig({ ...config, workDir: value }); }}><option value="">选择自动编排 worktree 进行验收</option>{worktrees.map((worktree) => <option key={worktree.jobId} value={worktree.worktreePath}>{worktree.taskBranch || `任务 ${worktree.taskId.slice(0, 8)}`}{worktree.status === "released_to_main" ? "（已合并）" : ""}</option>)}</select><p className="run-config-hint">选择后会写入上方目录；也可继续填写项目内子目录。</p></>}
						</div>
						<div className="run-config-row">
							<label>启动命令</label>
							<input type="text" value={config.command} placeholder="例如: npm run dev" onChange={(e) => updateConfig({ ...config, command: e.target.value })} />
							{isWindowsTarget && <p className="run-config-hint">由 Windows cmd 执行：路径用反斜杠、不要加 ./ 前缀，例如用 start.bat 而非 ./start.bat</p>}
						</div>
						<div className="run-config-row">
							<label>环境变量</label>
							<div className="run-env-vars">
								{Object.entries(config.envVars || {}).map(([k, v]) => (
									<div key={k} className="run-env-var">
										<input type="text" value={k} placeholder="KEY" onChange={(e) => {
											const next = { ...config.envVars };
											delete next[k];
											next[e.target.value || k] = v;
											updateConfig({ ...config, envVars: next });
										}} />
										<span>=</span>
										<input type="text" value={v} placeholder="VALUE" onChange={(e) => {
											updateConfig({ ...config, envVars: { ...config.envVars, [k]: e.target.value } });
										}} />
										<button type="button" className="run-env-remove" title={`移除环境变量 ${k}`} aria-label={`移除环境变量 ${k}`} onClick={() => {
											const next = { ...config.envVars };
											delete next[k];
											updateConfig({ ...config, envVars: next });
										}}><CloseIcon /></button>
									</div>
								))}
									<button type="button" className="secondary" onClick={() => {
										const envVars = config.envVars || {};
										updateConfig({ ...config, envVars: { ...envVars, [nextEnvironmentVariableKey(envVars)]: "" } });
									}}><PlusIcon />添加环境变量</button>
							</div>
						</div>
					</section>
				</aside>
			</div>
	</section>;
}

function TerminalIcon() { return <svg viewBox="0 0 16 16" width="16" height="16" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round"><rect x="2" y="2.5" width="12" height="11" rx="1.5" /><path d="m5 6 2 2-2 2M9.5 10h1.5" /></svg>; }
function ClearIcon() { return <svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round"><path d="M3 4.5h10M6 2.5h4M5 4.5l.5 9h5l.5-9" /></svg>; }
function DownIcon() { return <svg viewBox="0 0 16 16" width="16" height="16" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round"><path d="M8 2.5v9M4.5 8.5 8 12l3.5-3.5" /></svg>; }
function PlayIcon() { return <svg viewBox="0 0 16 16" width="15" height="15" fill="currentColor"><path d="M5 3.2c0-.7.8-1.1 1.4-.7l5 3.1a1 1 0 0 1 0 1.7l-5 3.1A.8.8 0 0 1 5 9.7V3.2Z" /></svg>; }
function StopIcon() { return <svg viewBox="0 0 16 16" width="15" height="15" fill="currentColor"><rect x="4" y="4" width="8" height="8" rx="1" /></svg>; }
function RestartIcon() { return <svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round"><path d="M13 6A5.3 5.3 0 1 0 13.4 10" /><path d="M13 2.5V6H9.5" /></svg>; }
function PlusIcon() { return <svg viewBox="0 0 16 16" width="14" height="14" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round"><path d="M8 3v10M3 8h10" /></svg>; }
function CloseIcon() { return <svg viewBox="0 0 16 16" width="14" height="14" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round"><path d="m4 4 8 8M12 4l-8 8" /></svg>; }

function formatUptime(startedAt: Date): string {
	const diff = Date.now() - startedAt.getTime();
	const seconds = Math.floor(diff / 1000);
	if (seconds < 60) return `${seconds}s`;
	const minutes = Math.floor(seconds / 60);
	if (minutes < 60) return `${minutes}m ${seconds % 60}s`;
	const hours = Math.floor(minutes / 60);
	return `${hours}h ${minutes % 60}m`;
}
