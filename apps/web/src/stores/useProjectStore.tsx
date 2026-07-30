// 项目级全局状态管理 — React Context

import { createContext, useContext, useState, useCallback, type ReactNode } from "react";
import type { Project, ProjectStatus } from "../lib/types";
import { api as apiFn } from "../lib/api";

interface ProjectContextValue {
  projects: Project[];
  projectStatuses: Record<string, ProjectStatus>;
  error: string;
  setError: (msg: string) => void;
  refreshProjects: () => Promise<void>;
  api: typeof apiFn;
}

const ProjectContext = createContext<ProjectContextValue | null>(null);

export function useProjectContext(): ProjectContextValue {
  const ctx = useContext(ProjectContext);
  if (!ctx) throw new Error("useProjectContext must be used within ProjectProvider");
  return ctx;
}

export function ProjectProvider({ children }: { children: ReactNode }) {
  const [projects, setProjects] = useState<Project[]>([]);
  const [projectStatuses, setProjectStatuses] = useState<Record<string, ProjectStatus>>({});
  const [error, setError] = useState("");

  const refreshProjects = useCallback(async () => {
    try {
      const list = await apiFn<Project[]>("/api/projects");
      setProjects(list);
      try {
        const statusList = await apiFn<{ id: string; running: number; conversationCount: number; activeTitle: string }[]>("/api/projects/statuses");
        const entries = statusList.map((item) => [item.id, { running: item.running === 1, conversationCount: item.conversationCount, activeTitle: item.activeTitle }] as const);
        setProjectStatuses(Object.fromEntries(entries));
      } catch {
        setProjectStatuses((prev) => {
          if (Object.keys(prev).length > 0) return prev;
          const entries = list.map((item) => [item.id, { running: false, conversationCount: 0, activeTitle: "" }] as const);
          return Object.fromEntries(entries);
        });
      }
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "无法加载项目列表");
    }
  }, []);

  return (
    <ProjectContext.Provider value={{ projects, projectStatuses, error, setError, refreshProjects, api: apiFn }}>
      {children}
    </ProjectContext.Provider>
  );
}
