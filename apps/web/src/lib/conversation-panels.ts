// 对话页侧栏模块的折叠状态（常用提示词/常用命令/技能/任务队列）。
// 按项目持久化到 localStorage，切换项目/重进页面时保持用户上次的折叠偏好。

export type ConversationPanelKey = "prompt" | "command" | "skills" | "taskQueue";

export type ConversationPanelsState = Record<ConversationPanelKey, boolean>;

export function defaultConversationPanels(): ConversationPanelsState {
  return { prompt: false, command: false, skills: false, taskQueue: false };
}

function storageKey(projectId: string): string {
  return `milevia:conversation-panels:${projectId}`;
}

export function readConversationPanels(projectId: string): ConversationPanelsState {
  try {
    const raw = window.localStorage.getItem(storageKey(projectId));
    if (!raw) return defaultConversationPanels();
    const parsed: unknown = JSON.parse(raw);
    if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) return defaultConversationPanels();
    const value = parsed as Partial<ConversationPanelsState>;
    const defaults = defaultConversationPanels();
    return {
      prompt: typeof value.prompt === "boolean" ? value.prompt : defaults.prompt,
      command: typeof value.command === "boolean" ? value.command : defaults.command,
      skills: typeof value.skills === "boolean" ? value.skills : defaults.skills,
      taskQueue: typeof value.taskQueue === "boolean" ? value.taskQueue : defaults.taskQueue,
    };
  } catch {
    return defaultConversationPanels();
  }
}

export function writeConversationPanels(projectId: string, state: ConversationPanelsState): void {
  try {
    window.localStorage.setItem(storageKey(projectId), JSON.stringify(state));
  } catch {
    // 隐私模式或存储满时忽略，折叠状态仅在本会话内生效。
  }
}
