export type MobileRealtimeMessage = {
  id: string;
  role: string;
  content: string;
  createdAt: string;
  runId?: string;
};

export type MobileRealtimeConversation = {
  id: string;
  status: string;
  lastActivityAt?: string;
  messages: MobileRealtimeMessage[];
  [key: string]: unknown;
};

export type MobileRealtimeTask = {
  id: string;
  status: string;
  updatedAt?: string;
  [key: string]: unknown;
};

export type MobileRealtimeProject = {
  id: string;
  tasks: MobileRealtimeTask[];
  conversations: MobileRealtimeConversation[];
  [key: string]: unknown;
};

export type MobileRealtimeSnapshot = {
  projects: MobileRealtimeProject[];
  [key: string]: unknown;
};

export type MobileRealtimeEvent = {
  type?: unknown;
  taskId?: unknown;
  createdAt?: unknown;
  payload?: unknown;
};

type RecordValue = Record<string, unknown>;

function record(value: unknown): RecordValue {
  return value && typeof value === "object" && !Array.isArray(value) ? value as RecordValue : {};
}

function text(value: unknown): string {
  return typeof value === "string" ? value : "";
}

export function terminalConversationStatus(type: string): "completed" | "failed" | "stopped" | null {
  switch (type) {
    case "run.completed":
      return "completed";
    case "run.failed":
    case "run.interrupted":
      return "failed";
    case "run.stopped":
    case "run.cancelled":
      return "stopped";
    default:
      return null;
  }
}

export function applyMobileTerminalEvent<T extends MobileRealtimeSnapshot>(snapshot: T, event: MobileRealtimeEvent): { snapshot: T; conversationId: string; changed: boolean } {
  const type = text(event.type);
  const payload = record(event.payload);
  const conversationId = text(payload.conversationId);
  const taskId = text(event.taskId) || text(payload.taskId);
  const conversationStatus = terminalConversationStatus(type);
  // run.* payload.status describes the run itself. Task cards use their own
  // lifecycle, so only task.* events may apply payload.status directly.
  // A stopped/failed run is known to return the task to action_required;
  // completed runs stay pending snapshot reconciliation because auto-review
  // may promote them directly to done.
  const taskStatus = type.startsWith("task.")
    ? text(payload.status)
    : conversationStatus === "stopped" || conversationStatus === "failed"
      ? "action_required"
      : "";
  const derivedTaskStatus = !type.startsWith("task.") && taskStatus !== "";
  if (!conversationStatus && !taskId) return { snapshot, conversationId, changed: false };

  const createdAt = text(event.createdAt);
  let changed = false;
  const projects = snapshot.projects.map((project) => {
    let tasks = project.tasks;
    let conversations = project.conversations;
    if (taskId && (taskStatus || type.startsWith("task."))) {
      const nextTasks = project.tasks.map((task) => {
        if (task.id !== taskId || !taskStatus || task.status === taskStatus) return task;
        // A late terminal event must not regress a task that the authoritative
        // snapshot has already moved past its running state.
        if (derivedTaskStatus && task.status !== "running") return task;
        changed = true;
        return { ...task, status: taskStatus, ...(createdAt ? { updatedAt: createdAt } : {}) };
      });
      tasks = nextTasks;
    }
    if (conversationStatus && conversationId) {
      const nextConversations = project.conversations.map((conversation) => {
        if (conversation.id !== conversationId) return conversation;
        const messages = conversation.messages.filter((message) => !message.id.startsWith("stream-"));
        const sameStatus = conversation.status === conversationStatus;
        const sameMessages = messages.length === conversation.messages.length;
        if (sameStatus && sameMessages) return conversation;
        changed = true;
        return {
          ...conversation,
          status: conversationStatus,
          ...(createdAt ? { lastActivityAt: createdAt } : {}),
          messages,
        };
      });
      conversations = nextConversations;
    }
    if (tasks === project.tasks && conversations === project.conversations) return project;
    return { ...project, tasks, conversations };
  });
  return { snapshot: changed ? { ...snapshot, projects } : snapshot, conversationId, changed };
}
