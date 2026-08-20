// 项目级全局状态管理 — React Context

import { createContext, useContext, useState, useCallback, useEffect, useMemo, useRef, type ReactNode } from "react";
import type { Project, ProjectStatus } from "../lib/types";
import { api as apiFn } from "../lib/api";
import {
  conversationDraftKey,
  clearExpiredConversationDrafts,
  clearStoredConversationDrafts,
  getConversationDraft as findConversationDraft,
  persistConversationDraft,
  readConversationDraft,
  updateConversationDraft,
} from "../lib/conversation-draft";
import { useUIPreferences } from "./useUIPreferences";

interface ProjectContextValue {
  projects: Project[];
  projectStatuses: Record<string, ProjectStatus>;
  error: string;
  setError: (msg: string) => void;
  refreshProjects: () => Promise<void>;
  refreshStatuses: () => Promise<void>;
  removeProject: (projectID: string) => void;
  getConversationDraft: (projectID: string, conversationID: string) => string;
  saveConversationDraft: (projectID: string, conversationID: string, text: string) => void;
  flushConversationDraft: (projectID: string, conversationID: string) => void;
  clearConversationDrafts: () => number;
  api: typeof apiFn;
}

const ProjectContext = createContext<ProjectContextValue | null>(null);

type PendingConversationDraft = { projectID: string; conversationID: string; text: string };

// 判断两批项目状态是否等价。用于 10 秒轮询去重：只在实际数据变化时才更新状态，
// 避免每次轮询都产生新的状态对象引用，触发 Provider 及所有消费组件无意义整体重渲染。
function projectStatusesEqual(a: Record<string, ProjectStatus>, b: Record<string, ProjectStatus>): boolean {
  const aKeys = Object.keys(a);
  if (aKeys.length !== Object.keys(b).length) return false;
  for (const key of aKeys) {
    const va = a[key];
    const vb = b[key];
    if (
      !vb ||
      va.running !== vb.running ||
      va.conversationCount !== vb.conversationCount ||
      va.activeTitle !== vb.activeTitle ||
      va.insightsRunning !== vb.insightsRunning ||
      va.insightsMessage !== vb.insightsMessage
    ) {
      return false;
    }
  }
  return true;
}

// 判断两条项目列表是否等价（浅比较：长度 + 各项目 id 与字段）。
// 用于 Dashboard 10 秒轮询去重，避免每次刷新都产生新列表引用触发多余重渲染。
function projectsEqual(a: Project[], b: Project[]): boolean {
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) {
    const pa = a[i];
    const pb = b[i];
    if (JSON.stringify(pa) !== JSON.stringify(pb)) return false;
  }
  return true;
}

export function useProjectContext(): ProjectContextValue {
  const ctx = useContext(ProjectContext);
  if (!ctx) throw new Error("useProjectContext must be used within ProjectProvider");
  return ctx;
}

export function ProjectProvider({ children }: { children: ReactNode }) {
  const { localPreferences } = useUIPreferences();
  const [projects, setProjects] = useState<Project[]>([]);
  const [projectStatuses, setProjectStatuses] = useState<Record<string, ProjectStatus>>({});
  const [error, setError] = useState("");
  const transientConversationDrafts = useRef<Record<string, string>>({});
  const pendingConversationDrafts = useRef(new Map<string, PendingConversationDraft>());
  const pendingDraftTimers = useRef(new Map<string, number>());
  const projectRequestVersion = useRef(0);
  const statusRequestVersion = useRef(0);
  // 镜像当前 projectStatuses，供轮询去重在组件外对比（状态值语义稳定，引用比较不可靠）。
  const statusesRef = useRef<Record<string, ProjectStatus>>({});
  // 镜像当前 projects，供 Dashboard 轮询去重。
  const projectsRef = useRef<Project[]>([]);
  const localPreferencesRef = useRef(localPreferences);
  localPreferencesRef.current = localPreferences;

  useEffect(() => {
    clearExpiredConversationDrafts(localPreferences.draftRetentionDays);
  }, [localPreferences.draftRetentionDays]);

  const flushConversationDraft = useCallback((projectID: string, conversationID: string) => {
    const key = conversationDraftKey(projectID, conversationID);
    const pendingDraft = pendingConversationDrafts.current.get(key);
    if (!pendingDraft) return;
    const timer = pendingDraftTimers.current.get(key);
    if (timer !== undefined) window.clearTimeout(timer);
    pendingDraftTimers.current.delete(key);
    pendingConversationDrafts.current.delete(key);

    const preferences = localPreferencesRef.current;
    const persisted = !pendingDraft.text || preferences.draftAutoSave
      ? persistConversationDraft(projectID, conversationID, pendingDraft.text, undefined, preferences.draftRetentionDays)
      : false;
    transientConversationDrafts.current = updateConversationDraft(
      transientConversationDrafts.current,
      projectID,
      conversationID,
      persisted ? "" : pendingDraft.text,
    );
  }, []);

  useEffect(() => {
    if (localPreferences.draftAutoSave) return;
    for (const draft of [...pendingConversationDrafts.current.values()]) {
      flushConversationDraft(draft.projectID, draft.conversationID);
    }
  }, [flushConversationDraft, localPreferences.draftAutoSave]);

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
    const storedDraft = readConversationDraft(projectID, conversationID, undefined, localPreferencesRef.current.draftRetentionDays);
    return findConversationDraft(transientConversationDrafts.current, projectID, conversationID) || storedDraft || "";
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
    if (!localPreferencesRef.current.draftAutoSave) {
      pendingConversationDrafts.current.delete(key);
      transientConversationDrafts.current = updateConversationDraft(transientConversationDrafts.current, projectID, conversationID, text);
      return;
    }
    pendingDraftTimers.current.set(key, window.setTimeout(() => flushConversationDraft(projectID, conversationID), 300));
  }, [flushConversationDraft]);

  const clearConversationDrafts = useCallback(() => {
    for (const timer of pendingDraftTimers.current.values()) window.clearTimeout(timer);
    pendingDraftTimers.current.clear();
    pendingConversationDrafts.current.clear();
    transientConversationDrafts.current = {};
    return clearStoredConversationDrafts();
  }, []);

  const refreshStatuses = useCallback(async () => {
    const requestVersion = ++statusRequestVersion.current;
    try {
      const statusList = await apiFn<{ id: string; running: number; conversationCount: number; activeTitle: string; insightsRunning: number; insightsMessage: string }[]>("/api/projects/statuses");
      if (requestVersion !== statusRequestVersion.current) return;
      const entries = statusList.map((item) => [item.id, {
        running: item.running === 1,
        conversationCount: item.conversationCount,
        activeTitle: item.activeTitle,
        insightsRunning: item.insightsRunning === 1,
        insightsMessage: item.insightsMessage ?? "",
      }] as const);
      const next = Object.fromEntries(entries);
      // 数据未变化则保持现有引用，避免 10 秒轮询每次都触发 Provider 及整站重渲染。
      if (projectStatusesEqual(statusesRef.current, next)) return;
      statusesRef.current = next;
      setProjectStatuses(next);
    } catch {
      // 失败时保留上一次已知的状态，不覆盖。
    }
  }, []);

  const refreshProjects = useCallback(async () => {
    const requestVersion = ++projectRequestVersion.current;
    try {
      const list = await apiFn<Project[]>("/api/projects");
      if (requestVersion !== projectRequestVersion.current) return;
      // 数据未变化则保持现有引用，避免 10 秒轮询每次都触发 Provider 重渲染。
      if (!projectsEqual(projectsRef.current, list)) {
        projectsRef.current = list;
        setProjects(list);
      }
      await refreshStatuses();
    } catch (cause) {
      if (requestVersion === projectRequestVersion.current) setError(cause instanceof Error ? cause.message : "无法加载项目列表");
    }
  }, [refreshStatuses]);

  // 乐观删除：立即从内存列表移除，避免依赖后续网络刷新（可能被并发轮询
  // 的"最新发起者"竞态覆盖，导致已删项目残留到下次手动刷新）。
  const removeProject = useCallback((projectID: string) => {
    setProjects((current) => {
      const next = current.filter((project) => project.id !== projectID);
      projectsRef.current = next;
      return next;
    });
  }, []);

  const value = useMemo<ProjectContextValue>(() => ({
    projects,
    projectStatuses,
    error,
    setError,
    refreshProjects,
    refreshStatuses,
    removeProject,
    getConversationDraft,
    saveConversationDraft,
    flushConversationDraft,
    clearConversationDrafts,
    api: apiFn,
  }), [projects, projectStatuses, error, refreshProjects, refreshStatuses, removeProject, getConversationDraft, saveConversationDraft, flushConversationDraft, clearConversationDrafts]);

  return (
    <ProjectContext.Provider value={value}>
      {children}
    </ProjectContext.Provider>
  );
}
