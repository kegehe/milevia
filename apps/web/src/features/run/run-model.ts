/** 一条具名启动命令。一个项目可以保存多条（开发/预发/生产参数不同，或不同的启动方式），
 *  启动时选中其中一条执行。 */
export interface RunCommand {
	/** 标识这一条；选中、去重、列表 key 都以它为准。 */
	id: string;
	/** 展示名，如「开发环境」；留空时界面回落到命令文本。 */
	name: string;
	/** 实际执行的命令行。 */
	command: string;
	/** 这条命令专属的环境变量，启动时叠加在全局环境变量之上（同名覆盖）。 */
	envVars?: Record<string, string>;
}

export interface RunConfig {
	workDir: string;
	/** 所选命令的文本镜像（后端维护）。界面以 commands 为准，这个字段只为兼容旧契约保留。 */
	command: string;
	envVars: Record<string, string>;
	executionTarget: 'auto' | 'wsl' | 'windows';
	commands: RunCommand[];
	/** 上次选中的命令 ID。 */
	selectedCommandId: string;
}

export type RunStatus = 'stopped' | 'starting' | 'running' | 'stopping' | 'failed';

export interface RunStatusResponse {
	status: RunStatus;
	/** 后端解析后的真实执行目标：windows / wsl / auto / 空（SSH 远端） */
	executionTarget?: RunConfig['executionTarget'] | '';
	startedAt: string | null;
	pid: number | null;
	exitCode: number | null;
	/** 当前（或最近一次）运行实际执行的命令行文本；用来判断"跑的是哪一条"。 */
	command?: string;
	recentLogs: LogEntry[];
}

/** 生成命令 ID。非安全上下文（如局域网 http）没有 crypto.randomUUID，退回时间戳+随机串。 */
export function newRunCommandID(): string {
	return globalThis.crypto?.randomUUID?.() || `run-cmd-${Date.now()}-${Math.random().toString(36).slice(2)}`;
}

/** 新建一条空白启动命令（名称与命令都留给用户填）。 */
export function createRunCommand(): RunCommand {
	return { id: newRunCommandID(), name: '', command: '' };
}

/** 命令在界面上的称呼：优先名称，其次命令文本，都没有时用序号占位。 */
export function runCommandLabel(command: RunCommand, index = 0): string {
	return command.name.trim() || command.command.trim() || `命令 ${index + 1}`;
}

/** 这条命令是否"真实存在"。空命令的行只是「添加一条」留下的壳：后端保存时会丢弃，
 *  所以它既不该被选中，也不该被当成启动目标 —— 否则会发出一个后端不认识的命令 ID。 */
export function isRunnableCommand(command: RunCommand): boolean {
	return command.command.trim() !== '';
}

/** 当前选中的命令：选中项必须是一条**有命令文本的**行；否则回落到第一条这样的行。
 *  一条都没有时返回 undefined（对应"请先配置启动命令"，此时界面上没有任何圆点被选中，
 *  与后端"命令列表为空"的判定一致）。 */
export function selectedRunCommand(config: RunConfig): RunCommand | undefined {
	const commands = config.commands || [];
	return commands.find((item) => item.id === config.selectedCommandId && isRunnableCommand(item)) || commands.find(isRunnableCommand);
}

/** 命令列表的不可变更新：顺带修正选中项并同步 command 镜像，保证界面与后端规范化后的
 *  形态一致（选中项要么指向一条有命令文本的条目，要么为空）。 */
export function withRunCommands(config: RunConfig, commands: RunCommand[], selectedCommandId = config.selectedCommandId): RunConfig {
	const selected = selectedRunCommand({ ...config, commands, selectedCommandId });
	return { ...config, commands, selectedCommandId: selected?.id || '', command: selected?.command || '' };
}

/** 改名的结果。成功时才带出新的变量表（判别式联合，调用方 `if (result.ok)` 之后拿到的就是
 *  非空的新表），失败时只给原因。 */
export type EnvironmentVariableRename =
	| { ok: true; envVars: Record<string, string> }
	/** empty：新键名为空；duplicate：新键名已被别的变量占用；unchanged/missing：无需改名。 */
	| { ok: false; reason: "empty" | "duplicate" | "unchanged" | "missing" };

/** 改一条环境变量的键名。成功时带出新的变量表，失败时给出原因供界面提示。
 *
 *  键名冲突必须在这里挡住：`next[新键] = 值` 这种写法遇到已存在的同名键会**悄悄盖掉**那条
 *  变量的取值（用户把 PATH 改成 PWD，PWD 那一行连同它的值一起消失）。占用即拒绝，变量表
 *  原样不动，由界面回滚输入框并提示，用户的另一个变量不会被动。
 *  空键名同样拒绝 —— 键名框允许被清空，但清空不该往表里塞一条无名变量（后端会判成非法）。 */
export function renameEnvironmentVariable(envVars: Record<string, string>, oldKey: string, nextKey: string): EnvironmentVariableRename {
	const trimmed = nextKey.trim();
	if (!trimmed) return { ok: false, reason: "empty" };
	if (!Object.prototype.hasOwnProperty.call(envVars, oldKey)) return { ok: false, reason: "missing" };
	if (trimmed === oldKey) return { ok: false, reason: "unchanged" };
	if (Object.prototype.hasOwnProperty.call(envVars, trimmed)) return { ok: false, reason: "duplicate" };
	// 按键序重建而不是"删旧键再追加"：改名后这一行留在原处，不会跳到列表末尾。
	// 用 fromEntries（内部是 CreateDataProperty）而不是 `next[新键] = 值`：后者遇到 `__proto__`
	// 会被原型 setter 吃掉，变量会凭空消失 —— 而它是个合法的 POSIX 变量名，后端照收。
	return { ok: true, envVars: Object.fromEntries(Object.entries(envVars).map(([key, value]) => [key === oldKey ? trimmed : key, value])) };
}

export interface LogEntry {
	id: number;
	timestamp: string;
	stream: 'stdout' | 'stderr' | 'system';
	text: string;
}

export interface RunLogPresentation {
	label: '输出' | '错误输出' | '系统' | '警告' | '错误';
	tone: '' | 'system' | 'is-info' | 'is-warning' | 'is-error';
}

const errorLogPattern = /(?:^|\s)(?:error|failed|fatal|panic|exception|traceback)\b|\bERR!|[A-Za-z]+Error(?::|\b)|\b(?:not found|permission denied|exit code \d+|command not found|cannot find|no such file or directory)\b/i;
const warningLogPattern = /(?:^|\s)(?:warn|warning)\b/i;
// 构建工具把大量成功/进度信息也写到 stderr（如 cargo 的 Running/Finished/Compiling、
// `Info Watching ...`）。这些行无害，不能落入“错误输出”，须归为普通“输出”。
// 注意：`warning: ... generated N warnings` 这类汇总行以 warning 开头，已由
// warningLogPattern 先接住归为“警告”，无需在此处理。
const infoLogPattern = /(?:^|\s)(?:info|running|finished|compiling)\b/i;

export function runLogPresentation(entry: Pick<LogEntry, 'stream' | 'text'>): RunLogPresentation {
	if (entry.stream === 'system') return { label: '系统', tone: 'system' };
	if (entry.stream === 'stdout') return { label: '输出', tone: '' };
	if (errorLogPattern.test(entry.text)) return { label: '错误', tone: 'is-error' };
	if (warningLogPattern.test(entry.text)) return { label: '警告', tone: 'is-warning' };
	// stderr 里的无害信息/进度行归为普通“输出”，避免构建成功的工具被标成错误。
	// 用独立 tone 让该行在 stderr 上仍显示正常（绿）徽标，而非默认的棕褐色错误徽标。
	if (infoLogPattern.test(entry.text)) return { label: '输出', tone: 'is-info' };
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
