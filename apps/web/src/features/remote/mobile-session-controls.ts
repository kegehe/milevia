import type { Ask, AskQuestion } from "../../lib/types";
import { getApproval, getAskEvent } from "../../lib/timeline";

/** 手机时间线上的一张运行记录。ask / approval 挂在同一条上，方便按 id 合并状态。 */
export type MobileNotice = {
  id: string;
  runId: string;
  createdAt: string;
  variant: string;
  title: string;
  detail?: string;
  state?: string;
  merge?: "append" | "replace";
  ask?: Ask;
  approvalId?: string;
  approvalDecision?: string;
};

export type MobileProfileChoice = {
  id: string;
  agentId: string;
  name: string;
  model?: string;
  enabled: boolean;
  isDefault: boolean;
  authMode: string;
  state: string;
};

const askStatusTitle: Record<Ask["status"], string> = {
  pending: "需要你回答",
  answered: "已回答",
  skipped: "已跳过",
  expired: "提问已超时",
  cancelled: "提问已取消",
};

/** 新建会话里能选的档案，与桌面端同一条过滤：启用、有效、且由 CLI 自己持有登录态。 */
export function selectableProfiles(choices: MobileProfileChoice[], agentId: string): MobileProfileChoice[] {
  return choices.filter((choice) => choice.agentId === agentId && choice.enabled && choice.state === "active" && choice.authMode === "cli_managed");
}

/** 预约发送只在这一轮还在跑时有意义。空闲了就该真的发出去。 */
export function scheduledSendReady(status: string | undefined, processing: boolean, orchestration: boolean): boolean {
  if (orchestration) return false;
  if (processing) return false;
  return status !== "running";
}

export function askFromNotice(notice: MobileNotice | undefined): Ask | null {
  if (!notice?.ask || notice.ask.status !== "pending" || notice.ask.questions.length === 0) return null;
  return notice.ask;
}

export function pendingApproval(notice: MobileNotice | undefined): { approvalId: string; detail: string } | null {
  if (!notice?.approvalId || notice.approvalDecision !== "pending") return null;
  return { approvalId: notice.approvalId, detail: notice.detail || notice.title };
}

/**
 * 提问的终态事件不带题目，只带 askId 和答案。合并时把先到的题目留住，
 * 否则卡片会在「已回答」那一刻变成一张没有问题的空卡。
 */
export function retainAskQuestions<T extends { ask?: Ask }>(kept: T, other: T): T {
  const questions = kept.ask?.questions?.length ? kept.ask.questions : other.ask?.questions;
  if (!kept.ask || !questions?.length || kept.ask.questions === questions) return kept;
  return { ...kept, ask: { ...kept.ask, questions } };
}

/** 把一条事件收成手机通知。提问和工具确认带上可操作的字段，其余返回 null 交给原来的解析。 */
export function interactiveNotice(type: string, payload: unknown, id: string, runId: string, createdAt: string): MobileNotice | null {
  const source = { id, type, payload, runId, createdAt };
  const askEvent = getAskEvent(source);
  if (askEvent?.askId) {
    const status = askEvent.status;
    const questions = (askEvent.questions || []).filter((question) => question.id && question.question) as AskQuestion[];
    const first = questions[0]?.question || askEvent.notes || "";
    return {
      id: `ask:${askEvent.askId}`,
      runId,
      createdAt,
      variant: "task",
      title: askStatusTitle[status] || "提问",
      detail: first,
      state: status === "pending" ? "info" : status === "answered" ? "success" : "failed",
      merge: "replace",
      ask: {
        askId: askEvent.askId,
        status,
        questions,
        answers: askEvent.answers,
        notes: askEvent.notes,
      },
    };
  }
  const approval = getApproval(source);
  if (!approval) return null;
  const resolution = type.slice("approval.".length);
  const command = typeof approval.toolInput.command === "string" ? approval.toolInput.command.replace(/\s+/g, " ").trim() : "";
  const detail = command ? `${approval.toolName}：${command.slice(0, 180)}${command.length > 180 ? "…" : ""}` : approval.toolName;
  const pending = resolution === "pending";
  return {
    id: `approval:${approval.approvalId}`,
    runId,
    createdAt,
    variant: "task",
    title: pending ? "等待工具确认" : resolution === "allow" ? "工具已确认" : resolution === "deny" ? "工具已拒绝" : resolution === "timeout" ? "工具确认超时" : "工具确认已中止",
    detail,
    state: pending ? "info" : resolution === "allow" ? "success" : resolution === "deny" || resolution === "timeout" ? "failed" : "info",
    merge: "replace",
    approvalId: approval.approvalId,
    approvalDecision: resolution,
  };
}
