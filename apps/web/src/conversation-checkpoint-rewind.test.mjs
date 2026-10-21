import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

// 会话回退（checkpoint）的入口位置、如实呈现与两端同源。
//
// 背景：这个功能原先只有头部一个「回退」图标，点开是一张按回合倒序的列表，用户得从 72 字
// 截断的提示词里认出"我要回到哪一轮"—— 而节点本来就是"某一轮跑完之后"，那些轮次明明白白
// 摆在对话里。所以主入口搬到时间线上（每条用户消息下面的「回退到此处」），头部那份退成
// "浏览所有节点"的次要入口；手机端在消息的 ⋯ 菜单里给同一颗按钮。
//
// 另外几条容易回退的性质：
//   - 节点要等 run **成功结束**才在服务端生成 → 跑着的那一轮显示"本轮结束后可回退"，
//     失败/停止的回合没有节点（什么都不显示）；
//   - 没有代码快照的节点（远程 runner 项目、快照创建失败）不能给「恢复代码/恢复全部」，
//     否则用户点了只会拿到一句 409；
//   - 确认框的文案与"默认发哪个 mode"两端**共用一份**（lib/conversation-rewind），
//     回退不可撤销，两端说岔了就是同一件事两种说法。
const conversationPage = await readFile(new URL("./pages/ConversationPage.tsx", import.meta.url), "utf8");
const mobilePage = await readFile(new URL("./pages/MobileRemotePage.tsx", import.meta.url), "utf8");
const stylesheet = await readFile(new URL("./conversation.css", import.meta.url), "utf8");
const rewindLib = await readFile(new URL("./lib/conversation-rewind.ts", import.meta.url), "utf8");

test("回退主入口在时间线上，按 run 挂到产生它的那条用户消息", () => {
  // 节点与消息的稳定关联是 run（不是内容）：服务端存 run_id，前端按 runId 建索引。
  assert.match(conversationPage, /const checkpointByRun = useMemo\(\(\) => \{[\s\S]*?map\.set\(checkpoint\.runId, checkpoint\)/);
  assert.match(conversationPage, /const checkpoint = item\.kind === "message" && item\.message\.role === "user" && item\.message\.runId \? checkpointByRun\.get\(item\.message\.runId\) : undefined;/);
  // 只有用户消息上挂回退入口，且这一轮有节点或正在跑时才渲染。
  assert.match(conversationPage, /item\.kind === "message" && item\.message\.role === "user" && item\.message\.runId && \(checkpoint \|\| item\.message\.runId === activeRunId\) && <CheckpointAnchor/);
  assert.match(conversationPage, /<RewindInlineIcon \/>\{busy \? "处理中" : rewindButtonLabel\(checkpoint\)\}/);
  // 点它按节点能力选默认动作（有代码快照 → 恢复全部；没有 → 只恢复对话），判据在共用模块里。
  assert.match(conversationPage, /rewind=\{\(target\) => rewindToCheckpoint\(target, rewindPrimaryMode\(target\)\)\}/);
});

test("没有节点时必须说清为什么，而不是让按钮凭空消失", () => {
  // 这一轮还在跑：节点要等 run 成功结束才生成。
  assert.match(conversationPage, /if \(!checkpoint\) \{\s*return running \? <p className="message-checkpoint pending">本轮结束后可回退到此处<\/p> : null;\s*\}/);
  // 失败/停止的回合服务端不留节点：这里什么都不显示，注释说明这不是故障。
  assert.match(conversationPage, /这一轮失败\/被停止：服务端不会留节点/);
  // 行内小字只在"这一轮和常规节点不一样"时出现（无代码快照 / 会丢几轮），文案同样来自共用模块。
  assert.match(conversationPage, /const notes = rewindHint\(checkpoint, discarded\);/);
  assert.match(conversationPage, /\{notes && <small>\{notes\}<\/small>\}/);
});

test("节点列表跟着回合刷新，否则刚跑完的那一轮永远不出现", () => {
  // 节点在服务端是 run 成功结束后才生成的；只在切会话时拉一次的话，新会话的回退入口会
  // 一直是灰的、老会话看不到最新节点。
  assert.match(conversationPage, /const refreshConversationCheckpoints = useCallback\(async \(conversationID\?: string\) => \{/);
  assert.match(conversationPage, /if \(conversationRef\.current\?\.id !== target\) return;/);
  assert.match(conversationPage, /rememberFinishedRun\(event\.runId\);[\s\S]*?void refreshConversationCheckpoints\(conversation\.id\);/);
  // 手机端同理：它只在"这条会话不再在跑"之后重新读一次节点列表。
  assert.match(mobilePage, /const loadCheckpoints = useCallback\(async \(\) => \{/);
  assert.match(mobilePage, /rpcTransport\("conversation\.checkpoints", \{\}, checkpointRPCTimeoutMs\)/);
});

test("确认框说清代价：丢几轮、代码会不会被改回去、两端同一份文案", () => {
  // 回退是不可撤销的破坏性操作：早先那句"这会停止当前会话上下文"既没说丢几轮，
  // 也没说快照之后新建的文件会被删除。文案收口在 lib/conversation-rewind，两端共用。
  assert.match(conversationPage, /import \{ checkpointHasCode, rewindButtonLabel, rewindConfirmMessage, rewindHint, rewindPrimaryMode \} from "\.\.\/lib\/conversation-rewind";/);
  assert.match(conversationPage, /const request = \{ mode, fork, discarded: discardedTurnsAfter\(checkpoint\) \};/);
  assert.match(conversationPage, /window\.confirm\(rewindConfirmMessage\(checkpoint, request\)\)/);
  // 手机端的两个数（默认动作 / 会丢几轮）在一次渲染里算一次，判据与桌面端同源。
  assert.match(mobilePage, /const rewindRequest = rewindTarget \? \{ mode: rewindPrimaryMode\(rewindTarget\), discarded: rewindDiscardedTurns\(rewindTarget\) \} : null;/);
  assert.match(mobilePage, /rewindConfirmMessage\(rewindTarget, rewindRequest\)/);
  // 手机端**只**有一次确认：确认框本身就是那一层（再套一个原生 window.confirm 等于对同一个
  // 不可撤销动作确认两次，而手机端别处从不使用原生 confirm）。
  assert.doesNotMatch(mobilePage, /window\.confirm\(rewindConfirmMessage/);
  assert.match(mobilePage, /const request = \{ mode: rewindPrimaryMode\(target\), discarded: rewindDiscardedTurns\(target\) \};\s*setRewindTarget\(null\);/);
  // 丢几轮 = 节点之后还剩几条顶层用户消息（子代理消息不算一轮）。
  assert.match(conversationPage, /messages\.filter\(\(message\) => message\.role === "user" && !message\.parentToolUseId && message\.createdAt > checkpoint\.createdAt\)\.length/);
  // 共用模块本身：三句代价必须都在，且"不可撤销"的后果要具体到文件。
  assert.match(rewindLib, /快照之后新建的文件会被删除/);
  // "重建模型上下文"必须写进去：服务端真的换了原生会话并把历史喂回去，说"模型不受影响"
  // 是旧行为（用户会以为模型还记得被回退掉的回合）。
  assert.match(rewindLib, /重建模型上下文/);
  assert.doesNotMatch(rewindLib, /模型的原生上下文不受影响/);
  assert.match(rewindLib, /此后 \$\{request\.discarded\} 轮对话将不再显示。/);
  assert.match(rewindLib, /export function rewindPrimaryMode\(checkpoint: ConversationCheckpoint\): RewindMode \{\s*return checkpointHasCode\(checkpoint\) \? "conversation_and_code" : "conversation";\s*\}/);
});

test("头部那份退成节点列表：无代码快照的节点不给「恢复代码/恢复全部」", () => {
  assert.match(conversationPage, /title="回退节点（按回合）" aria-label="回退节点"/);
  assert.match(conversationPage, /<span className="action-label">\{checkpointBusy \? "处理中" : "节点"\}<\/span>/);
  assert.match(conversationPage, /\{checkpointHasCode\(checkpoint\) && <button type="button" onClick=\{\(\) => void rewindToCheckpoint\(checkpoint, "code"\)\}>恢复代码<\/button>\}/);
  assert.match(conversationPage, /\{checkpointHasCode\(checkpoint\) && <button type="button" onClick=\{\(\) => void rewindToCheckpoint\(checkpoint, "conversation_and_code"\)\}>恢复全部<\/button>\}/);
  assert.match(conversationPage, /\{checkpointHasCode\(checkpoint\) \? "" : " · 无代码快照"\}/);
  // 两个入口共用同一份禁用判据。
  assert.match(conversationPage, /const rewindDisabled = readOnlyConversation \|\| checkpointBusy \|\| Boolean\(run\) \|\| clearing \|\| stopping \|\| handingOff;/);
});

test("时间线回退回调身份稳定，不把 memo 的时间线整条拖着重渲染", () => {
  assert.match(conversationPage, /const rewindRef = useRef\(rewindToCheckpoint\);\s*rewindRef\.current = rewindToCheckpoint;\s*const rewindFromTimeline = useCallback\(/);
  assert.match(conversationPage, /rewindToCheckpoint=\{rewindFromTimeline\}/);
});

test("手机端的回退入口在消息 ⋯ 菜单里，且破坏性动作要有自己的确认层", () => {
  // 入口：用户消息、这一轮有节点时才出现；点它只**打开确认框**，不在菜单里直接执行。
  assert.match(mobilePage, /entry\.message\.role === "user" && checkpointByRun\.has\(entry\.message\.runId \|\| ""\) && <button/);
  assert.match(mobilePage, /const target = checkpointByRun\.get\(entry\.message\.runId \|\| ""\); closeMessageActions\(\); if \(target\) setRewindTarget\(target\);/);
  // 确认层是 position: fixed 的一层，必须被返回键优先收掉（与「强制停止」同一条：
  // 漏掉这一句，返回键会一路退掉会话层，而「确认回退」仍可点）。
  assert.match(mobilePage, /if \(rewindTarget\) \{ setRewindTarget\(null\); return true; \}/);
  // 换会话时无条件复位：它挂在"我刚才看的那条会话"上。
  assert.match(mobilePage, /useEffect\(\(\) => \{ setStoppingConversation\(false\); setStopConfirmTarget\(""\); setRewindTarget\(null\); \}, \[conversation\?\.id\]\);/);
  // 回退走中继上那条与桌面端同形的端点，只发 mode（"从节点分支"是电脑端才有的动作）。
  assert.match(mobilePage, /rpcTransport\("conversation\.rewind", \{ checkpointId: target\.id, mode: request\.mode \}, checkpointRewindRPCTimeoutMs\)/);
  // 回退改了电脑端的工作区与对话：必须重读权威快照，否则手机端还停在旧内容上。
  assert.match(mobilePage, /void loadSnapshot\(\{ force: true \}\);\s*void loadConversationHistory\(projectID, target\.conversationId\);\s*void loadCheckpoints\(\);/);
});

test("conversation.css 承载行内回退入口的样式", () => {
  assert.match(stylesheet, /\.message-checkpoint \{[\s\S]*?justify-content: flex-end;/);
  assert.match(stylesheet, /\.message-checkpoint-button \{[\s\S]*?border-radius: 999px;/);
  assert.match(stylesheet, /\.message-checkpoint-button:disabled \{/);
  assert.match(stylesheet, /\.message-checkpoint-icon \{ width: 12px; height: 12px;[\s\S]*?stroke: currentColor;/);
});
