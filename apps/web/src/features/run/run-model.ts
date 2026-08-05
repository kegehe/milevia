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

export interface RunLogPresentation {
	label: '输出' | '错误输出' | '系统' | '警告' | '错误';
	tone: '' | 'system' | 'is-warning' | 'is-error';
}

const errorLogPattern = /(?:^|\s)(?:error|failed|fatal|panic|exception|traceback)\b|\bERR!|[A-Za-z]+Error(?::|\b)|\b(?:not found|permission denied|exit code \d+|command not found|cannot find|no such file or directory)\b/i;
const warningLogPattern = /(?:^|\s)(?:warn|warning)\b/i;

export function runLogPresentation(entry: Pick<LogEntry, 'stream' | 'text'>): RunLogPresentation {
	if (entry.stream === 'system') return { label: '系统', tone: 'system' };
	if (entry.stream === 'stdout') return { label: '输出', tone: '' };
	if (errorLogPattern.test(entry.text)) return { label: '错误', tone: 'is-error' };
	if (warningLogPattern.test(entry.text)) return { label: '警告', tone: 'is-warning' };
	return { label: '错误输出', tone: '' };
}

export function runLogText(entry: Pick<LogEntry, 'stream' | 'text'>): string {
	return entry.text;
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
