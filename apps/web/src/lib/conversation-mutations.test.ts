import assert from "node:assert/strict";
import test from "node:test";
import {
  applyPendingConversations,
  isPendingConversationID,
  matchPendingConversation,
  newPendingConversationID,
  pendingConversationCard,
  type PendingConversation,
  type PendingConversationCard,
} from "./conversation-mutations.ts";

// 这一组的每一条分支错了，表现都是"用户刚建的会话在界面上凭空消失一次"，或者
// "在还没建成的会话里发的消息永远发不出去" —— 都发生在乐观更新那条异步链上。

function conversation(id: string, isCurrent = false): PendingConversationCard {
  return { id, title: `会话 ${id}`, status: "idle", agentId: "claude-code", lastActivityAt: "2026-09-18T00:00:00.000Z", isCurrent, messages: [] };
}

function project(id: string, conversations: PendingConversationCard[]) {
  return { id, name: `项目 ${id}`, conversations };
}

// 临时 id 一律用库里的生成函数：前缀是"还没落地"的唯一判据，测试自己手写别的形状
// 就绕过了那条判断，测的就不是真实路径了。
function pending(options: { projectId?: string; known?: string[]; resolvedId?: string; seenRevision?: number } = {}): PendingConversation {
  const id = newPendingConversationID();
  return {
    id,
    projectId: options.projectId ?? "p1",
    agentId: "claude-code",
    createdAt: "2026-09-18T10:00:00.000Z",
    commandId: "",
    knownConversationIds: options.known ?? ["c1"],
    resolvedId: options.resolvedId,
    seenRevision: options.seenRevision ?? 3,
  };
}

test("没有待确认会话时原样返回入参（同一个数组引用）", () => {
  const projects = [project("p1", [conversation("c1")])];
  assert.equal(applyPendingConversations(projects, [], () => []), projects);
});

test("新建：本地那条立刻出现在最前面，原来的会话让出 isCurrent", () => {
  const item = pending();
  const projects = [project("p1", [conversation("c1", true), conversation("c2")])];
  const result = applyPendingConversations(projects, [item], () => []);

  assert.equal(result[0].conversations.length, 3);
  assert.equal(result[0].conversations[0].id, item.id);
  assert.equal(result[0].conversations[0].status, "creating");
  assert.equal(result[0].conversations[0].agentId, "claude-code");
  // 原来那条 isCurrent 必须让位，否则"当前会话"会有两条
  assert.deepEqual(result[0].conversations.slice(1).map((entry) => [entry.id, entry.isCurrent]), [["c1", false], ["c2", false]]);
});

test("新建：只落在自己那个项目上，别的项目一动不动（同一个对象引用）", () => {
  const item = pending({ projectId: "p2" });
  const other = project("p1", [conversation("c1")]);
  const projects = [other, project("p2", [conversation("c2")])];
  const result = applyPendingConversations(projects, [item], () => []);

  assert.equal(result[0], other);
  assert.equal(result[1].conversations[0].id, item.id);
});

test("新建：刚发出去的消息挂在本地这张卡上（否则气泡会凭空不见）", () => {
  const item = pending();
  const projects = [project("p1", [])];
  const result = applyPendingConversations(projects, [item], (id) => (id === item.id ? [{ requestId: "r1", content: "你好", createdAt: "2026-09-18T10:00:01.000Z" }] : []));

  assert.deepEqual(result[0].conversations[0].messages, [{ id: "pending-r1", role: "user", content: "你好", createdAt: "2026-09-18T10:00:01.000Z" }]);
});

// 快照里已经有这条会话时：不画卡（否则 React 会因为 key 重复报警，还会把电脑端真实的
// 会话内容盖掉一帧），也不能动快照自己的 isCurrent —— 卡没顶上去，让位就没有意义了。
test("新建：快照里已经有这个 id 时不画卡，也不动快照自己的 isCurrent", () => {
  const item = { ...pending(), id: "conv-real-1" };
  const projects = [project("p1", [conversation("conv-real-1", true), conversation("c2")])];
  const result = applyPendingConversations(projects, [item], () => []);

  assert.equal(result[0], projects[0], "没有卡要顶时必须原样返回");
  assert.equal(result[0].conversations[0].isCurrent, true);

  const withBubbles = applyPendingConversations(projects, [item], (id) => (id === "conv-real-1" ? [{ requestId: "r1", content: "在吗", createdAt: "2026-09-18T10:00:03.000Z" }] : []));
  assert.equal(withBubbles[0].conversations[0].isCurrent, true, "补气泡不等于顶卡，不能让位");
});

// SSE 的 conversation.created 会先把会话塞进快照（一条空壳），而它的正文还在本机。
// 这个补丁必须有人做：漏掉就是用户刚发出去的消息凭空消失一帧，链路上没有任何人负责补。
test("新建：快照里已有这条会话时，把还没落地的乐观气泡补到它上面", () => {
  const item = { ...pending({ resolvedId: "conv-real-1" }), id: newPendingConversationID() };
  const projects = [project("p1", [conversation("conv-real-1", true)])];
  const result = applyPendingConversations(projects, [item], (id) => (id === "conv-real-1" ? [{ requestId: "r1", content: "在吗", createdAt: "2026-09-18T10:00:04.000Z" }] : []));

  assert.deepEqual(result[0].conversations.map((entry) => entry.id), ["conv-real-1"]);
  assert.deepEqual(result[0].conversations[0].messages, [{ id: "pending-r1", role: "user", content: "在吗", createdAt: "2026-09-18T10:00:04.000Z" }]);
});

// 快照的对账（loadSnapshot）与这里的补丁会用同一套 `pending-<requestId>` id 各补一次。
test("新建：补气泡时按 id 去重，两边都补不会变成两条", () => {
  const item = pending({ resolvedId: "conv-real-1" });
  const withMessage = { ...conversation("conv-real-1", true), messages: [{ id: "pending-r1", role: "user" as const, content: "在吗", createdAt: "2026-09-18T10:00:04.000Z" }] };
  const projects = [project("p1", [withMessage])];
  const result = applyPendingConversations(projects, [item], () => [{ requestId: "r1", content: "在吗", createdAt: "2026-09-18T10:00:04.000Z" }]);

  assert.equal(result[0].conversations[0].messages.length, 1);
});

// 真 id 到手之后、快照追上之前，本地卡要**继续顶着**，只是改用真 id 渲染 ——
// 这段窗口里若把卡片摘掉（指望快照顶上），一次迟到的快照刷新就会把它连同用户刚打的
// 消息一起抹掉。浏览器探针实测复现过。
test("新建：拿到真 id 后本地卡改用真 id 继续顶着，直到快照出现才退场", () => {
  const item = { ...pending(), resolvedId: "conv-real-9" };
  const projects = [project("p1", [conversation("c1", true)])];
  const result = applyPendingConversations(projects, [item], (id) => (id === "conv-real-9" ? [{ requestId: "r1", content: "在吗", createdAt: "2026-09-18T10:00:02.000Z" }] : []));

  assert.deepEqual(result[0].conversations.map((entry) => entry.id), ["conv-real-9", "c1"]);
  // 状态从"创建中"变成"就绪"：真 id 一到，它已经不是本地占位了。
  assert.equal(result[0].conversations[0].status, "idle");
  // 气泡按真 id 索引取（消息早就搬到真 id 底下了）。
  assert.deepEqual(result[0].conversations[0].messages.map((message) => message.content), ["在吗"]);
  assert.equal(result[0].conversations[1].isCurrent, false);
});

test("临时 id 前缀只认自己那一套", () => {
  assert.equal(isPendingConversationID(newPendingConversationID()), true);
  assert.equal(isPendingConversationID("conv-abc"), false);
  assert.equal(isPendingConversationID("pending-abc"), false);
});

test("认领：回执给了真 id 时按 id 认，快照里还没有就继续等", () => {
  const item = pending({ resolvedId: "conv-real-9", known: [] });
  // 快照里有一条"以前不存在"的会话，但不是回执给的那条 —— 不许认领
  assert.equal(matchPendingConversation(item, [conversation("conv-other")]), "");
  assert.equal(matchPendingConversation(item, [conversation("conv-other"), conversation("conv-real-9")]), "conv-real-9");
});

test("认领：没拿到回执时，项目里第一条新出现的会话就是它", () => {
  const item = pending({ known: ["c1"] });
  assert.equal(matchPendingConversation(item, [conversation("c1")]), "");
  assert.equal(matchPendingConversation(item, [conversation("c1"), conversation("c2")]), "c2");
});

// 本地那条也在同一个列表里（认领那一帧）时，不能把另一条待确认会话当成真身 ——
// 那会把两条新建会话接成同一条，其中一条永远发不出消息。
test("认领：不会把另一条待确认会话当成真身", () => {
  const item = pending({ known: ["c1"] });
  const other = newPendingConversationID();
  assert.equal(matchPendingConversation(item, [conversation("c1"), conversation(other)]), "");
});

// 同一个项目里同时新建两条会话：两条的判据长得一模一样（"第一条以前没见过的会话"），
// 不把已被认下的 id 排掉，就会双双认到同一条上，其中一条的排队消息永远发不出去。
test("认领：已被别的待确认会话认下的 id 不再被认第二遍", () => {
  const item = pending({ known: ["c1"] });
  const list = [conversation("c1"), conversation("c2"), conversation("c3")];
  assert.equal(matchPendingConversation(item, list), "c2");
  assert.equal(matchPendingConversation(item, list, ["c2"]), "c3");
  assert.equal(matchPendingConversation(item, list, ["c2", "c3"]), "");
});

test("卡片标题与状态固定为「新会话 / 创建中」，不带云端字段", () => {
  const card = pendingConversationCard(pending(), []);
  assert.equal(card.title, "新会话");
  assert.equal(card.status, "creating");
  assert.equal(card.isCurrent, true);
  assert.deepEqual(card.messages, []);
});
