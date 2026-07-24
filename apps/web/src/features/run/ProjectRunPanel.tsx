import { useCallback, useEffect, useRef, useState } from "react";
import { type LogEntry, type RunConfig, type RunStatusResponse, statusLabels, statusColors } from "./run-model";

type Request = <T>(path: string, init?: RequestInit) => Promise<T>;

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

export function ProjectRunPanel({ projectID, request, fail, close }: { projectID: string; request: Request; fail: (message: string) => void; close: () => void }) {
	const [config, setConfig] = useState<RunConfig>({ workDir: "", command: "", envVars: {} });
	const [status, setStatus] = useState<RunStatusResponse | null>(null);
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

	const basePath = `/api/projects/${projectID}/run`;
	const mergeIncomingLogs = useCallback((entries: LogEntry[]) => {
		const visibleEntries = entries.filter((entry) => entry.id <= 0 || entry.id > clearedThroughLogIDRef.current);
		setLogs((prev) => mergeLogs(prev, visibleEntries));
	}, []);

	const loadConfig = useCallback(async () => {
		try {
			const c = await request<RunConfig>(`${basePath}/config`);
			if (activeProjectIDRef.current === projectID) setConfig(c);
		}
		catch (cause) {
			if (activeProjectIDRef.current === projectID) fail(cause instanceof Error ? cause.message : "加载配置失败");
		}
	}, [basePath, request, fail, projectID]);

	const loadStatus = useCallback(async () => {
		try {
			const s = await request<RunStatusResponse>(`${basePath}/status`);
			if (activeProjectIDRef.current !== projectID) return;
			setStatus(s);
			mergeIncomingLogs(s.recentLogs);
		} catch { /* 轮询忽略错误 */ }
	}, [basePath, request, projectID, mergeIncomingLogs]);

	useEffect(() => {
		let disposed = false;
		activeProjectIDRef.current = projectID;
		clearedThroughLogIDRef.current = 0;
		void loadConfig();
		void loadStatus();

		const protocol = location.protocol === "https:" ? "wss" : "ws";
		const connect = () => {
			if (disposed) return;
			const ws = new WebSocket(`${protocol}://${location.host}/ws/projects/${projectID}/run`);
			wsRef.current = ws;

			ws.onmessage = (raw) => {
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
	}, [projectID, loadConfig, loadStatus, mergeIncomingLogs]);

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
		try { await request(`${basePath}/config`, { method: "PUT", body: JSON.stringify(config) }); }
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

	const handleClearLogs = () => {
		setLogs((prev) => {
			for (const entry of prev) clearedThroughLogIDRef.current = Math.max(clearedThroughLogIDRef.current, entry.id);
			return [];
		});
		setHasNewLogs(false);
	};

	const running = status?.status === "running";
	const transitioning = status?.status === "starting" || status?.status === "stopping";
	const statusColor = status ? statusColors[status.status] : statusColors.stopped;
	const statusLabel = status ? statusLabels[status.status] : statusLabels.stopped;
	const uptime = status?.startedAt ? formatUptime(new Date(status.startedAt)) : "";

	return <div className="run-panel-backdrop" role="presentation" onMouseDown={(e) => { if (e.currentTarget === e.target) close(); }}>
		<section className="run-panel" role="dialog" aria-modal="true" aria-label="项目启动">
			<header className="run-panel-head">
				<div><label>PROJECT RUNNER</label><h2>项目启动</h2></div>
				<button type="button" className="git-close" title="关闭" aria-label="关闭" onClick={close}>×</button>
			</header>

			<section className="run-config">
				<div className="run-config-row">
					<label>工作目录</label>
					<input type="text" value={config.workDir} placeholder="留空使用项目根目录" onChange={(e) => setConfig({ ...config, workDir: e.target.value })} />
				</div>
				<div className="run-config-row">
					<label>启动命令</label>
					<input type="text" value={config.command} placeholder="例如: npm run dev" onChange={(e) => setConfig({ ...config, command: e.target.value })} />
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
									setConfig({ ...config, envVars: next });
								}} />
								<span>=</span>
								<input type="text" value={v} placeholder="VALUE" onChange={(e) => {
									setConfig({ ...config, envVars: { ...config.envVars, [k]: e.target.value } });
								}} />
								<button type="button" className="secondary" onClick={() => {
									const next = { ...config.envVars };
									delete next[k];
									setConfig({ ...config, envVars: next });
								}}>×</button>
							</div>
						))}
							<button type="button" className="secondary" onClick={() => {
								const envVars = config.envVars || {};
								setConfig({ ...config, envVars: { ...envVars, [nextEnvironmentVariableKey(envVars)]: "" } });
							}}>+ 添加</button>
					</div>
				</div>
				<button type="button" className="primary" disabled={busy === "save"} onClick={saveConfig}>{busy === "save" ? "保存中" : "保存配置"}</button>
			</section>

			<section className="run-controls">
				<div className="run-status-bar">
					<span className="run-status-dot" style={{ background: statusColor }}></span>
					<span>{statusLabel}</span>
					{status?.pid ? <span>PID: {status.pid}</span> : null}
					{uptime ? <span>运行 {uptime}</span> : null}
					{status?.exitCode !== null && status?.exitCode !== undefined && status.status !== "running" ?
						<span>退出码: {status.exitCode}</span> : null}
				</div>
				<div className="run-actions">
					<button type="button" className="primary" disabled={running || transitioning || busy !== ""} onClick={handleStart}>{busy === "start" ? "启动中" : "启动"}</button>
					<button type="button" className="danger" disabled={!running || busy !== ""} onClick={handleStop}>{busy === "stop" ? "停止中" : "停止"}</button>
					<button type="button" className="secondary" disabled={busy !== ""} onClick={handleRestart}>{busy === "restart" ? "重启中" : "重启"}</button>
				</div>
			</section>

			<section className="run-log-section">
				<header>
					<h3>日志输出</h3>
					<button type="button" className="secondary" onClick={handleClearLogs}>清除</button>
				</header>
				<div className="run-log-terminal-wrap">
					<div ref={logTerminalRef} className="run-log-terminal" onScroll={handleLogScroll}>
						{logs.length === 0 ? <div className="run-log-empty">尚未有日志输出。配置并启动命令后，日志将显示在此处。</div> : logs.map((entry, i) => (
							<div key={entry.id || `${entry.timestamp}-${i}`} className={`run-log-line ${entry.stream}`}>
								<time>{new Date(entry.timestamp).toLocaleTimeString()}</time>
								<pre>{entry.text}</pre>
							</div>
						))}
					</div>
					{hasNewLogs && <button type="button" className="run-log-jump" title="跳转到最新日志" aria-label="跳转到最新日志" onClick={jumpToLatestLogs}>↓</button>}
				</div>
			</section>
		</section>
	</div>;
}

function formatUptime(startedAt: Date): string {
	const diff = Date.now() - startedAt.getTime();
	const seconds = Math.floor(diff / 1000);
	if (seconds < 60) return `${seconds}s`;
	const minutes = Math.floor(seconds / 60);
	if (minutes < 60) return `${minutes}m ${seconds % 60}s`;
	const hours = Math.floor(minutes / 60);
	return `${hours}h ${minutes % 60}m`;
}
