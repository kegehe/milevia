import assert from "node:assert/strict";
import test from "node:test";

import {
  checkpointHasCode,
  rewindConfirmAction,
  rewindConfirmBody,
  rewindConfirmMessage,
  rewindConfirmTitle,
  rewindHint,
  rewindPrimaryMode,
} from "./conversation-rewind";
import type { ConversationCheckpoint } from "./types";

function node(overrides: Partial<ConversationCheckpoint> = {}): ConversationCheckpoint {
  return {
    id: "node-1",
    conversationId: "conversation-1",
    agentId: "claude-code",
    prompt: "first turn",
    status: "active",
    createdAt: "2026-10-20T00:00:00Z",
    ...overrides,
  };
}

// 老服务端不返回 hasSnapshot：按"有代码快照"处理，不能因为缺字段就把「恢复代码」藏起来。
test("hasSnapshot 缺省按有代码快照处理", () => {
  assert.equal(checkpointHasCode(node()), true);
  assert.equal(checkpointHasCode(node({ hasSnapshot: true })), true);
  assert.equal(checkpointHasCode(node({ hasSnapshot: false })), false);
});

// 主入口的默认动作：有代码快照就一起恢复（"回到那一刻"的直觉），没有就只回退对话
// —— 远程 runner 项目的节点永远没有快照，默认发 conversation_and_code 只会拿到 409。
test("默认动作按节点能力选", () => {
  assert.equal(rewindPrimaryMode(node()), "conversation_and_code");
  assert.equal(rewindPrimaryMode(node({ hasSnapshot: false })), "conversation");
});

// 确认框的标题与动作键跟着模式走（按钮本体已改为图标钮，标签文案只活在确认框里）。
test("确认框标题与动作键跟着模式走", () => {
  assert.equal(rewindConfirmTitle({ mode: "conversation_and_code", discarded: 0 }), "恢复代码和对话");
  assert.equal(rewindConfirmTitle({ mode: "code", discarded: 0 }), "只恢复代码");
  assert.equal(rewindConfirmTitle({ mode: "conversation", discarded: 0 }), "只恢复对话");
  assert.equal(rewindConfirmTitle({ mode: "conversation_and_code", fork: true, discarded: 0 }), "从该节点创建分支");
  assert.equal(rewindConfirmAction({ mode: "conversation_and_code", discarded: 0 }, false), "恢复全部");
  assert.equal(rewindConfirmAction({ mode: "conversation_and_code", discarded: 0 }, true), "回退中");
});

// 确认框必须说清代价：这是不可撤销的破坏性操作。三句缺一不可 ——
// 丢几轮、代码会被改回去（含"快照之后新建的文件会被删除"）、没有代码快照时退化成什么。
test("确认文案说清丢几轮与代码的后果", () => {
  const all = rewindConfirmMessage(node(), { mode: "conversation_and_code", discarded: 3 });
  assert.match(all, /^恢复代码和对话？/);
  assert.match(all, /快照之后新建的文件会被删除/);
  assert.match(all, /对话也回到本轮结束时/);
  assert.match(all, /此后 3 轮对话将不再显示。$/);

  // 只恢复代码：对话历史不变，代码照旧会被改回去。
  const code = rewindConfirmMessage(node(), { mode: "code", discarded: 2 });
  assert.match(code, /对话历史不变。/);

  // 只恢复对话：代码不动，而且要说清电脑端会**重建模型上下文** —— 服务端在真的丢了回合时
  // 会换一个新的原生会话、并在下一轮把保留的历史喂回去。写成"模型不受影响"是旧行为，
  // 早就不成立了（用户会以为模型还记得被回退掉的回合）。
  const conversation = rewindConfirmMessage(node(), { mode: "conversation", discarded: 1 });
  assert.match(conversation, /代码不动。/);
  assert.match(conversation, /重建模型上下文/);
  assert.match(conversation, /被回退掉的回合它不再记得/);
  assert.doesNotMatch(conversation, /模型的原生上下文不受影响/);

  // 没有代码快照的节点：即使调用方发的是 conversation_and_code（老界面、或手滑），
  // 文案也要如实说"只回退对话"，不能承诺会改代码。
  const noSnapshot = rewindConfirmMessage(node({ hasSnapshot: false }), { mode: "conversation_and_code", discarded: 4 });
  assert.match(noSnapshot, /该节点没有代码快照，只回退对话/);
  assert.match(noSnapshot, /重建模型上下文/);
  assert.doesNotMatch(noSnapshot, /快照之后新建的文件会被删除/);

  // 分支不是回退：原会话不动。
  const fork = rewindConfirmMessage(node(), { mode: "conversation_and_code", fork: true, discarded: 5 });
  assert.match(fork, /原会话保持不变，新会话从这个节点继续。/);
  assert.match(fork, /此后 5 轮对话将不再显示。/);
});

// 桌面端用 ConfirmDialog：标题与正文分开，正文不能再把标题重复一遍；
// 手机端仍然只吃一句合成文案，两种壳共享同一份正文。
test("正文与标题分开，合成文案仍包含两者", () => {
  const request = { mode: "conversation_and_code" as const, discarded: 2 };
  const body = rewindConfirmBody(node(), request);
  assert.doesNotMatch(body, /^恢复代码和对话？/);
  assert.match(body, /此后 2 轮对话将不再显示。$/);
  assert.equal(rewindConfirmMessage(node(), request), `恢复代码和对话？${body}`);
});

// 丢了 0 轮时不许出现"此后 0 轮对话将不再显示"。
test("没有可丢的轮次时不提这句", () => {
  const message = rewindConfirmMessage(node(), { mode: "conversation_and_code", discarded: 0 });
  assert.doesNotMatch(message, /此后 0 轮/);
  assert.doesNotMatch(message, /轮对话将不再显示/);
});

test("行内小字只在节点与常规节点不一样时出现", () => {
  assert.equal(rewindHint(node(), 0), "");
  assert.equal(rewindHint(node(), 2), "之后 2 轮不再显示");
  assert.equal(rewindHint(node({ hasSnapshot: false }), 0), "无代码快照，只回退对话");
  assert.equal(rewindHint(node({ hasSnapshot: false }), 1), "无代码快照，只回退对话 · 之后 1 轮不再显示");
});
