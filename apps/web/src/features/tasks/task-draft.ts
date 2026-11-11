import type { Priority, Request } from "./task-model";

export type TaskDraftItem = {
  key: string;
  description: string;
  priority: Priority;
  dependsOn: string[];
};

export type TaskDraft = {
  understood: string;
  mode: "single" | "split";
  assumptions: string[];
  questions: string[];
  tasks: TaskDraftItem[];
};

export type TaskDraftAction =
  | { kind: "questions"; questions: string[] }
  | { kind: "single"; description: string }
  | { kind: "split"; tasks: TaskDraftItem[] };

export function isDraftAbort(cause: unknown): boolean {
  return cause instanceof DOMException && cause.name === "AbortError";
}

// 有待补充的问题时不生成任务。只有一条时回填说明，两条及以上才拆开。
export function applyTaskDraft(draft: TaskDraft): TaskDraftAction {
  const questions = (draft.questions ?? []).map((item) => item.trim()).filter(Boolean);
  if (questions.length > 0) return { kind: "questions", questions };
  const tasks = (draft.tasks ?? []).filter((item) => item.description.trim());
  if (tasks.length <= 1) return { kind: "single", description: tasks[0]?.description.trim() ?? "" };
  return { kind: "split", tasks };
}

export async function organizeTaskDraft(options: {
  request: Request;
  projectID: string;
  text: string;
  signal: AbortSignal;
}): Promise<TaskDraft> {
  if (options.signal.aborted) throw new DOMException("Aborted", "AbortError");
  const text = options.text.trim();
  if (!text) throw new Error("请先输入要整理的内容");
  return options.request<TaskDraft>(`/api/projects/${options.projectID}/tasks/draft`, {
    method: "POST",
    body: JSON.stringify({ text }),
    signal: options.signal,
  });
}

export async function confirmDraftTasks(request: Request, projectID: string, tasks: TaskDraftItem[]): Promise<void> {
  await request(`/api/projects/${projectID}/tasks/batch`, {
    method: "POST",
    body: JSON.stringify({
      tasks: tasks.map((task) => ({
        key: task.key,
        description: task.description.trim(),
        priority: task.priority,
        dependsOn: task.dependsOn,
      })),
    }),
  });
}

export function draftWaitLabel(item: TaskDraftItem, tasks: TaskDraftItem[]): string {
  if (!item.dependsOn?.length) return "";
  const names = item.dependsOn.map((key) => {
    const index = tasks.findIndex((task) => task.key === key);
    return index >= 0 ? `第 ${index + 1} 条` : key;
  });
  return `等待${names.join("、")}`;
}
