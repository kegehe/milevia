// 项目级全局状态管理 — React Context

import { createContext, useContext, useState, useCallback, useEffect, useRef, type ReactNode } from "react";
import type { Project, ProjectStatus } from "../lib/types";
import { api as apiFn } from "../lib/api";
import {
  conversationDraftKey,
  getConversationDraft as findConversationDraft,
  persistConversationDraft,
  readConversationDraft,
  updateConversationDraft,
} from "../lib/conversation-draft";

interface ProjectContextValue {
  projects: Project[];
  projectStatuses: Record<string, ProjectStatus>;
  error: string;
  setError: (msg: string) => void;
  refreshProjects: () => Promise<void>;
  refreshStatuses: () => Promise<void>;
  getConversationDraft: (projectID: string, conversationID: string) => string;
  saveConversationDraft: (projectID: string, conversationID: string, text: string) => void;
  flushConversationDraft: (projectID: string, conversationID: string) => void;
  api: typeof apiFn;
}

const ProjectContext = createContext<ProjectContextValue | null>(null);

type PendingConversationDraft = { projectID: string; conversationID: string; text: string };

export function useProjectContext(): ProjectContextValue {
  const ctx = useContext(ProjectContext);
  if (!ctx) throw new Error("useProjectContext must be used within ProjectProvider");
  return ctx;
}

export function ProjectProvider({ children }: { children: ReactNode }) {
  const [projects, setProjects] = useState<Project[]>([]);
  const [projectStatuses, setProjectStatuses] = useState<Record<string, ProjectStatus>>({});
  const [error, setError] = useState("");
  const transientConversationDrafts = useRef<Record<string, string>>({});
  const pendingConversationDrafts = useRef(new Map<string, PendingConversationDraft>());
  const pendingDraftTimers = useRef(new Map<string, number>());

  const flushConversationDraft = useCallback((projectID: string, conversationID: string) => {
    const key = conversationDraftKey(projectID, conversationID);
    const pendingDraft = pendingConversationDrafts.current.get(key);
    if (!pendingDraft) return;
    const timer = pendingDraftTimers.current.get(key);
    if (timer !== undefined) window.clearTimeout(timer);
    pendingDraftTimers.current.delete(key);
    pendingConversationDrafts.current.delete(key);

    const persisted = persistConversationDraft(projectID, conversationID, pendingDraft.text);
    transientConversationDrafts.current = updateConversationDraft(
      transientConversationDrafts.current,
      projectID,
      conversationID,
      persisted ? "" : pendingDraft.text,
    );
  }, []);

  const flushAllConversationDrafts = useCallback(() => {
    for (const draft of [...pendingConversationDrafts.current.values()]) {
      flushConversationDraft(draft.projectID, draft.conversationID);
    }
  }, [flushConversationDraft]);

  useEffect(() => {
    window.addEventListener("pagehide", flushAllConversationDrafts);
    return () => {
      window.removeEventListener("pagehide", flushAllConversationDrafts);
      flushAllConversationDrafts();
    };
  }, [flushAllConversationDrafts]);

  const getConversationDraft = useCallback((projectID: string, conversationID: string) => {
    const pendingDraft = pendingConversationDrafts.current.get(conversationDraftKey(projectID, conversationID));
    if (pendingDraft) return pendingDraft.text;
    const storedDraft = readConversationDraft(projectID, conversationID);
    return storedDraft ?? findConversationDraft(transientConversationDrafts.current, projectID, conversationID);
  }, []);

  const saveConversationDraft = useCallback((projectID: string, conversationID: string, text: string) => {
    const key = conversationDraftKey(projectID, conversationID);
    const previousTimer = pendingDraftTimers.current.get(key);
    if (previousTimer !== undefined) window.clearTimeout(previousTimer);
    pendingConversationDrafts.current.set(key, { projectID, conversationID, text });
    if (!text) {
      flushConversationDraft(projectID, conversationID);
      return;
    }
    pendingDraftTimers.current.set(key, window.setTimeout(() => flushConversationDraft(projectID, conversationID), 300));
  }, [flushConversationDraft]);

  const refreshStatuses = useCallback(async () => {
    try {
      const statusList = await apiFn<{ id: string; running: number; conversationCount: number; activeTitle: string }[]>("/api/projects/statuses");
      const entries = statusList.map((item) => [item.id, { running: item.running === 1, conversationCount: item.conversationCount, activeTitle: item.activeTitle }] as const);
      setProjectStatuses(Object.fromEntries(entries));
    } catch {
      // 失败时保留上一次已知的状态，不覆盖。
    }
  }, []);

  const refreshProjects = useCallback(async () => {
    try {
      const list = await apiFn<Project[]>("/api/projects");
      setProjects(list);
      await refreshStatuses();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "无法加载项目列表");
    }
  }, [refreshStatuses]);

  return (
    <ProjectContext.Provider value={{ projects, projectStatuses, error, setError, refreshProjects, refreshStatuses, getConversationDraft, saveConversationDraft, flushConversationDraft, api: apiFn }}>
      {children}
    </ProjectContext.Provider>
  );
}
