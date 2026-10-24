// 会话回退（checkpoint）的文案与默认动作 —— 桌面端与手机端**共用一份**。
//
// 回退是不可撤销的破坏性动作（改工作区文件、可能删掉快照之后新建的文件），所以两端的确认框
// 必须说清同一件事：丢几轮、代码会不会被改回去、没有代码快照时退化成什么。两端各写一遍
// 必然有一天分叉（本项目在"停止"的确认文案上已经栽过一次，见 lib/mobile-conversation-stop.ts），
// 所以这里连同"默认发哪个 mode"一起收口。
//
// 语义（服务端定的，两端都只能照说）：节点 N = "第 N 轮跑完之后"。回退到它 = 保留 ≤ N 轮。
// 比它晚的消息不再显示；代码快照命中的文件回到那一刻，快照之后新建的文件会被删除
// （.gitignore 命中的文件不在快照里，永远不会被动到）。

import type { ConversationCheckpoint } from "./types";

export type RewindMode = "conversation" | "code" | "conversation_and_code";

export type RewindRequest = {
  mode: RewindMode;
  fork?: boolean;
  /** 该节点之后还剩几轮（顶层用户消息），由调用方数出来 —— 它来自界面上的消息列表。 */
  discarded: number;
};

/** 节点有没有代码快照。`hasSnapshot` 缺省按"有"处理（老服务端不返回这个字段）。 */
export function checkpointHasCode(checkpoint: ConversationCheckpoint): boolean {
  return checkpoint.hasSnapshot !== false;
}

/** 主入口（时间线/消息菜单上那一颗按钮）该发哪个 mode：有代码快照就一起恢复，没有就只回退对话。 */
export function rewindPrimaryMode(checkpoint: ConversationCheckpoint): RewindMode {
  return checkpointHasCode(checkpoint) ? "conversation_and_code" : "conversation";
}

/** 行内/菜单里那行小字：只说"这个节点和常规节点不一样"的地方。 */
export function rewindHint(checkpoint: ConversationCheckpoint, discarded: number): string {
  const notes: string[] = [];
  if (!checkpointHasCode(checkpoint)) notes.push("无代码快照，只回退对话");
  if (discarded > 0) notes.push(`之后 ${discarded} 轮不再显示`);
  return notes.join(" · ");
}

/** 确认框标题。 */
export function rewindConfirmTitle(request: RewindRequest): string {
  if (request.fork) return "从该节点创建分支";
  if (request.mode === "code") return "只恢复代码";
  if (request.mode === "conversation") return "只恢复对话";
  return "恢复代码和对话";
}

/**
 * 确认框正文：会发生什么、代价是什么。
 *
 * 早先两端都是一句"这会停止当前会话上下文"—— 既没说会丢几轮，也没说代码会被改回去，
 * 而这是**不可撤销**的操作。
 *
 * "重建模型上下文"这句必须留着：服务端在真的丢了回合时会换一个新的原生会话，并在下一轮
 * 把保留下来的历史喂回去（见服务端 checkpoint.go 的"上下文重建"）。用户早知道这件事，
 * 才不会在看到模型"忘记"被回退掉的回合时以为哪里坏了 —— 也不该反过来写成"模型不受影响"
 * （那是旧行为，早就不成立了）。
 */
export function rewindConfirmMessage(checkpoint: ConversationCheckpoint, request: RewindRequest): string {
  const scope = request.fork
    ? "原会话保持不变，新会话从这个节点继续。"
    : request.mode === "code"
      ? "项目代码回到本轮结束时的状态：快照之后新建的文件会被删除，改动的文件被改回去。对话历史不变。"
      : !checkpointHasCode(checkpoint)
        ? "该节点没有代码快照，只回退对话：此后内容不再显示，电脑端还会重建模型上下文（被回退掉的回合它不再记得）。"
        : request.mode === "conversation"
          ? "对话回到本轮结束时，此后内容不再显示；电脑端会重建模型上下文（只保留这段历史，被回退掉的回合它不再记得）。代码不动。"
          : "代码回到本轮结束时的状态（快照之后新建的文件会被删除），对话也回到本轮结束时，并重建模型上下文（被回退掉的回合它不再记得）。";
  return `${rewindConfirmTitle(request)}？${scope}${request.discarded > 0 ? `此后 ${request.discarded} 轮对话将不再显示。` : ""}`;
}

/** 确认按钮上的字（回退中要能显示进度，所以单独给一个）。 */
export function rewindConfirmAction(request: RewindRequest, busy: boolean): string {
  if (busy) return "回退中";
  if (request.fork) return "创建分支";
  if (request.mode === "code") return "只恢复代码";
  if (request.mode === "conversation") return "只恢复对话";
  return "恢复全部";
}
