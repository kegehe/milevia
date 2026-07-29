export interface RunConfig {
	workDir: string;
	command: string;
	envVars: Record<string, string>;
	executionTarget: 'auto' | 'wsl' | 'windows';
}

export type RunStatus = 'stopped' | 'starting' | 'running' | 'stopping' | 'failed';

export interface RunStatusResponse {
	status: RunStatus;
	startedAt: string | null;
	pid: number | null;
	exitCode: number | null;
	recentLogs: LogEntry[];
}

export interface LogEntry {
	id: number;
	timestamp: string;
	stream: 'stdout' | 'stderr' | 'system';
	text: string;
}

export const statusLabels: Record<RunStatus, string> = {
	stopped: '已停止',
	starting: '启动中',
	running: '运行中',
	stopping: '停止中',
	failed: '异常退出',
};

export const statusColors: Record<RunStatus, string> = {
	stopped: '#6b7280',
	starting: '#f59e0b',
	running: '#10b981',
	stopping: '#f59e0b',
	failed: '#ef4444',
};
