// 工具目录（前端唯一来源）。
//
// 这个模块只做三件事：缓存服务端 `GET /api/agents` 的结果、提供同步取值、以及在
// 目录还没加载到的时候给出**不冒充其它工具**的退化值。
//
// 它取代的是散落在界面里的这类写法：
//
//   agentID === "codex" ? "Codex" : "Claude Code"
//
// 那种写法的默认分支永远落在 Claude 上，于是新增第三个工具时界面会把它静默标成
// "Claude Code"：编译不报错、现有测试也不报错（除非有人逐字钉住了那一行）。
// 本项目的处置办法一直是"把判据收到一处、让它只有一份"，这里照办。

import { useSyncExternalStore } from "react";
import { api } from "./api";
import type { AgentID, PermissionMode, RunnerInfo, ToolStatus, ToolStatusKind } from "./types";

/** 一个工具在目标环境需要的运行时（与后端 AgentRequirement 对应）。 */
export type AgentRequirement = { command: string; label: string; kind: string; installHint?: string };

/** 与后端 AgentCatalogEntry 对应的前端视图（字段名逐一对应，不做重命名）。 */
export type AgentCatalogEntry = {
  id: string;
  name: string;
  vendor: string;
  homepage?: string;
  docsUrl?: string;
  installKind: string;
  npmPackage: string;
  commandName: string;
  minRuntimeVersion: string;
  supportsInstall: boolean;
  permissionModes: PermissionMode[];
  defaultPermissionMode: PermissionMode;
  requires: AgentRequirement[];
  slashCommands: boolean;
  mcpInjection: string;
  readiness: string;
  /** 平台能否在管理页发起该工具自己的登录（浏览器授权）流程。 */
  supportsLogin?: boolean;
  /** 平台是否已实现该工具的 AgentRunner，可在项目对话中真正运行。 */
  runnableInProject?: boolean;
};

/**
 * 目录状态。**三个字段缺一不可**：只有 entries 的话，"还没读到"与"读失败"都会
 * 渲染成"一个工具都没有"，而那正是本项目反复禁止的"把读不到写成没有"。
 */
export type AgentCatalogState = {
  entries: AgentCatalogEntry[];
  /** 是否已经成功读到过目录。false 时界面应说"正在读取"，不能说"没有工具"。 */
  loaded: boolean;
  /** 读取失败的原因。非空时界面要如实说出来，而不是显示空列表。 */
  error: string;
};

let state: AgentCatalogState = { entries: [], loaded: false, error: "" };
const listeners = new Set<() => void>();

function publish(next: AgentCatalogState): void {
  state = next;
  for (const listener of listeners) listener();
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

/**
 * 加载工具目录。
 *
 * 失败时**保留上一次成功的结果**并只更新 error：清空会把"读取失败"变成
 * "平台没有工具"。错误同样向外抛，方便调用处决定是否提示。
 */
export async function loadAgentCatalog(): Promise<AgentCatalogEntry[]> {
  try {
    const entries = await api<AgentCatalogEntry[]>("/api/agents");
    const next = Array.isArray(entries) ? entries : [];
    publish({ entries: next, loaded: true, error: "" });
    return next;
  } catch (cause: unknown) {
    const message = cause instanceof Error ? cause.message : "无法读取工具目录";
    publish({ entries: state.entries, loaded: state.loaded, error: message });
    throw cause;
  }
}

/** 订阅目录状态。未加载完成时 entries 为空数组、loaded 为 false。 */
export function useAgentCatalogState(): AgentCatalogState {
  return useSyncExternalStore(subscribe, () => state, () => state);
}

/** 只要条目列表。 */
export function useAgentCatalog(): AgentCatalogEntry[] {
  return useAgentCatalogState().entries;
}

/** 同步快照，供非组件的纯逻辑使用。 */
export function agentCatalogState(): AgentCatalogState {
  return state;
}

/**
 * 把任意字符串收窄成 AgentID —— 仅当目录里确实登记了这个工具时。
 *
 * 服务端可能报出一个本前端版本还不认识的工具 ID。这时应当如实说"不认识"
 * （返回 undefined），而不是回落到某个已知工具上。它同时替掉了原先
 * `x === "claude-code" || x === "codex"` 那种顺带完成类型收窄、但把工具清单
 * 写死在判断里的写法。
 */
export function knownAgentID(value: string | undefined | null): AgentID | undefined {
  if (!value) return undefined;
  return agentEntry(value) ? (value as AgentID) : undefined;
}

export function agentEntry(agentID: string): AgentCatalogEntry | undefined {
  return state.entries.find((entry) => entry.id === agentID);
}

export function agentIDs(): string[] {
  return state.entries.map((entry) => entry.id);
}

/**
 * 把目录条目的 id 收窄成 `AgentID`。
 *
 * 目录由服务端给出，可能包含前端类型联合里还没有的工具；`AgentID` 仍是联合类型
 * （它能在写错字面量时编译报错，值得留着），所以两边需要**一个**转换点。
 * 集中在这里，而不是在界面各处散落 `as AgentID` —— 散落的断言会让"这里还没适配
 * 新工具"这条信息消失。
 */
export function catalogAgentID(entry: AgentCatalogEntry): AgentID {
  return entry.id as AgentID;
}

/**
 * 工具显示名。
 *
 * 目录未加载（或服务端不认识这个 id）时**原样返回 id**，绝不回落到某个已知工具名：
 * 把未知工具显示成 "Claude Code" 正是本次要消灭的错误。
 */
export function agentDisplayName(agentID: string): string {
  return agentEntry(agentID)?.name ?? agentID;
}

/** 该工具是否自报斜杠命令目录（决定界面是否显示命令选择器）。 */
export function agentSupportsSlashCommands(agentID: string): boolean {
  return agentEntry(agentID)?.slashCommands ?? false;
}

/** 该工具支持的权限模式。未加载时返回空数组 —— 界面据此不给出选项，而不是猜。 */
export function agentPermissionModes(agentID: string): PermissionMode[] {
  return agentEntry(agentID)?.permissionModes ?? [];
}

/**
 * 权限模式的可读文案。
 *
 * 按**模式**索引而不是按工具索引：同一个模式在两个工具上应当叫同一个名字，
 * 否则用户会以为它们不是一回事。原先这套文案是按工具写两遍的，其中
 * full_control 的说明文字在两边还不一样。
 */
export const permissionCopy: Record<PermissionMode, { title: string; detail: string }> = {
  approval_required: { title: "默认权限", detail: "终端命令执行前需要确认。" },
  full_control: { title: "完全控制", detail: "直接执行命令，不做额外确认。" },
  read_only: { title: "仅分析", detail: "只读检查，不修改项目文件。" },
  workspace_write: { title: "项目内执行", detail: "可在当前项目范围内读写和执行。" },
};

/**
 * 某个工具在某个 Runner 上的状态。
 *
 * 只读 `agents[]`：后端已保证它对目录里的每个工具都有条目，并让 claude / codex
 * 两个过渡字段由它派生。所以这里不再按工具 ID 分支，也不需要回落读旧字段。
 * 找不到时返回 undefined —— 调用方按"读不到"渲染，不要当成"不可用"。
 */
export function runnerAgentStatus(runner: RunnerInfo | null | undefined, agentID: string): ToolStatus | undefined {
  return runner?.agents?.find((item) => item.id === agentID);
}

/** 该 Runner 上该工具的状态；读不到时返回 "unknown"（与 unavailable 分开）。 */
export function runnerAgentStatusKind(runner: RunnerInfo | null | undefined, agentID: string): ToolStatusKind | "unknown" {
  return runnerAgentStatus(runner, agentID)?.status ?? "unknown";
}

/** 该 Runner 上该工具是否已就绪。 */
export function runnerAgentReady(runner: RunnerInfo | null | undefined, agentID: string): boolean {
  return runnerAgentStatusKind(runner, agentID) === "ready";
}

/**
 * 就地更新某个工具的状态（用于"我刚点了更新，先乐观显示更新中"）。
 *
 * 只改 agents[]：过渡字段由后端派生，前端不再维护第二份，因此不碰它们；
 * 下一次刷新会被服务端结果整体覆盖。
 */
export function withRunnerAgentStatus(runner: RunnerInfo, agentID: string, status: ToolStatusKind): RunnerInfo {
  if (!runner.agents) return runner;
  return {
    ...runner,
    agents: runner.agents.map((item) => (item.id === agentID ? { ...item, status } : item)),
  };
}
