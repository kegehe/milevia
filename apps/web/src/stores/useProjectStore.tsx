// 项目级全局状态管理 — React Context

import { createContext, useContext, useState, useCallback, useEffect, useMemo, useRef, type ReactNode } from "react";
import type { Project, ProjectAvailability, ProjectStatus } from "../lib/types";
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
import { coalesceRefresh } from "../lib/refresh-coalesce";
import { useUIPreferences } from "./useUIPreferences";
import { useTransientError } from "../lib/use-transient-error";

interface ProjectContextValue {
  projects: Project[];
  projectStatuses: Record<string, ProjectStatus>;
  // 列表首屏三态：loading（还没成功拉到过、当前有请求在飞）/ loaded（至少成功过一次）/
  // failed（从未成功且最近一次失败）。区分它们是为了让总览页不再把"还没加载完/加载失败"
  // 误画成"还没有项目"。
  projectsLoading: boolean;
  projectsLoaded: boolean;
  projectsLoadError: string;
  error: string;
  setError: (msg: string) => void;
  refreshProjects: () => Promise<void>;
  refreshAvailability: () => Promise<void>;
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

/** 把一个每次渲染都换身份的刷新函数包成稳定引用 + 并发合并的刷新入口。 */
function useCoalescedRefresh(run: () => Promise<void>): () => Promise<void> {
  const runRef = useRef(run);
  runRef.current = run;
  const coalescedRef = useRef<(() => Promise<void>) | null>(null);
  if (coalescedRef.current === null) {
    coalescedRef.current = coalesceRefresh(() => runRef.current());
  }
  return coalescedRef.current;
}

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
  // 初始为 true：Provider 挂载后，总览页的首个 effect 就会发起列表请求；
  // 若等到请求真正开始才置位，首帧会先闪一下"还没有项目"。
  const [projectsLoading, setProjectsLoading] = useState(true);
  const [projectsLoaded, setProjectsLoaded] = useState(false);
  const [projectsLoadError, setProjectsLoadError] = useState("");
  const [error, setError] = useTransientError();
  const transientConversationDrafts = useRef<Record<string, string>>({});
  const pendingConversationDrafts = useRef(new Map<string, PendingConversationDraft>());
  const pendingDraftTimers = useRef(new Map<string, number>());
  const projectRequestVersion = useRef(0);
  // 连通性探测独立编号：只用于丢弃"被更新一轮探测超越"的过期响应。刻意不复用
  // projectRequestVersion——列表请求失败时版本已自增却不会发起探活，会导致一次
  // 本来有效的探活结果被误丢弃，就绪度迟迟不刷新。
  const availabilityRequestVersion = useRef(0);
  const statusRequestVersion = useRef(0);
  // 镜像当前 projectStatuses，供轮询去重在组件外对比（状态值语义稳定，引用比较不可靠）。
  const statusesRef = useRef<Record<string, ProjectStatus>>({});
  // 镜像当前 projects，供 Dashboard 轮询去重。
  const projectsRef = useRef<Project[]>([]);
  // 镜像 projectsLoaded：刷新闭包只在"首屏阶段"跟踪 loading，
  // 用 ref 判断而不读 state，避免把过期的渲染闭包值带进异步续体。
  const projectsLoadedRef = useRef(false);
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

  const refreshStatuses = useCoalescedRefresh(async () => {
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
  });

  // 连通性探测独立于列表刷新：/api/projects 已不再同步探活（远端主机慢或离线时
  // 会拖住整个列表页），codex/agent 就绪度改由本函数单独拉取、按项目 id 合并。
  // 失败时保留上一次已知就绪度，不把卡片误判为不可用。
  const refreshAvailability = useCoalescedRefresh(async () => {
    const requestVersion = ++availabilityRequestVersion.current;
    try {
      // 探活本身在服务端有超时与短 TTL 缓存；这里不重试，避免超时后再叠一轮远端探测。
      const items = await apiFn<ProjectAvailability[]>("/api/projects/availability", undefined, 0);
      // 期间已发起更新一轮的探测：丢弃这次过期结果，避免旧结果覆盖新结果。
      if (requestVersion !== availabilityRequestVersion.current) return;
      const byId = new Map(items.map((item) => [item.id, item]));
      const current = projectsRef.current;
      let changed = false;
      const next = current.map((project) => {
        const item = byId.get(project.id);
        if (!item) return project;
        if (project.claudeReady === item.claudeReady && project.codexReady === item.codexReady && project.agentReady === item.agentReady) return project;
        changed = true;
        return { ...project, claudeReady: item.claudeReady, codexReady: item.codexReady, agentReady: item.agentReady };
      });
      if (!changed) return;
      projectsRef.current = next;
      setProjects(next);
    } catch {
      // 保留上一次已知的连通性状态。
    }
  });

  const refreshProjects = useCoalescedRefresh(async () => {
    const requestVersion = ++projectRequestVersion.current;
    // 只在"还没成功拉到过列表"时跟踪 loading：首屏骨架靠它；成功之后如果每次
    // 轮询仍把 loading 来回切换，context value 会不断换新引用，把整页（含全部
    // 项目卡）带着无意义重渲染 —— 这与下面 projectsEqual / statusesEqual 的去重
    // 目的直接冲突。
    const firstLoad = !projectsLoadedRef.current;
    if (firstLoad) setProjectsLoading(true);
    try {
      const list = await apiFn<Project[]>("/api/projects");
      if (requestVersion !== projectRequestVersion.current) return;
      // 列表接口不再同步探活，它返回的 codex/agent 就绪度只是落库值。这里保留上一次
      // 已探明的连通性，避免卡片在"已就绪/不可用"之间闪一下；真实值随后由
      // refreshAvailability 异步刷新。
      const known = new Map(projectsRef.current.map((project) => [project.id, project]));
      const merged = list.map((project) => {
        const previous = known.get(project.id);
        if (!previous) return project;
        return { ...project, codexReady: previous.codexReady, agentReady: project.claudeReady || previous.codexReady };
      });
      // 数据未变化则保持现有引用，避免轮询每次都触发 Provider 重渲染。
      if (!projectsEqual(projectsRef.current, merged)) {
        projectsRef.current = merged;
        setProjects(merged);
      }
      // 拿到列表即视为"已经知道项目有哪些"：即便结果是空数组，也是确定的空，而非加载中。
      if (!projectsLoadedRef.current) {
        projectsLoadedRef.current = true;
        setProjectsLoaded(true);
      }
      setProjectsLoadError("");
      // 连通性探测不阻塞列表：拿到列表即返回，探测结果异步合并。
      void refreshAvailability();
      await refreshStatuses();
    } catch (cause) {
      if (requestVersion === projectRequestVersion.current) {
        // 消息可能为空串（Error("")），那样错误态会退化成空态，所以兜底。
        const message = cause instanceof Error && cause.message ? cause.message : "无法加载项目列表";
        setError(message);
        // 持久错误只服务于"从未成功加载"的首屏；已加载后的轮询失败保留全局短暂提示即可，
        // 否则总览页会凭空出现一块与当前列表矛盾的错误面板。
        if (!projectsLoadedRef.current) setProjectsLoadError(message);
      }
    } finally {
      if (firstLoad && requestVersion === projectRequestVersion.current) setProjectsLoading(false);
    }
  });

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
    projectsLoading,
    projectsLoaded,
    projectsLoadError,
    error,
    setError,
    refreshProjects,
    refreshAvailability,
    refreshStatuses,
    removeProject,
    getConversationDraft,
    saveConversationDraft,
    flushConversationDraft,
    clearConversationDrafts,
    api: apiFn,
  }), [projects, projectStatuses, projectsLoading, projectsLoaded, projectsLoadError, error, refreshProjects, refreshAvailability, refreshStatuses, removeProject, getConversationDraft, saveConversationDraft, flushConversationDraft, clearConversationDrafts]);

  return (
    <ProjectContext.Provider value={value}>
      {children}
    </ProjectContext.Provider>
  );
}
