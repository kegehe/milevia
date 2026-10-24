import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

// 会话回退（checkpoint）的入口位置、如实呈现与两端同源。
//
// 背景：这个功能原先只有头部一个「回退」图标，点开是一张按回合倒序的列表，用户得从 72 字
// 截断的提示词里认出"我要回到哪一轮"—— 而节点本来就是"某一轮跑完之后"，那些轮次明明白白
// 摆在对话里。所以主入口先搬到时间线上；2026-10-23 再上卡片：回退与复制同住消息卡片
// 下方的动作行（.message-actions，hover / focus 该消息显形，时间线最后一条用户消息常驻），
// 头部「节点」列表撤除。手机端在消息的 ⋯ 菜单里给同一颗按钮。
//
// 另外几条容易回退的性质：
//   - 节点要等 run **成功结束**才在服务端生成 → 跑着的那一轮显示"本轮结束后可回退"，
//     失败/停止的回合没有节点（什么都不显示）；
//   - 没有代码快照的节点（远程 runner 项目、快照创建失败）只回退对话，不冒充"恢复全部"；
//   - 确认框的文案与"默认发哪个 mode"两端**共用一份**（lib/conversation-rewind），
//     回退不可撤销，两端说岔了就是同一件事两种说法。
const conversationPage = await readFile(new URL("./pages/ConversationPage.tsx", import.meta.url), "utf8");
const mobilePage = await readFile(new URL("./pages/MobileRemotePage.tsx", import.meta.url), "utf8");
const stylesheet = await readFile(new URL("./conversation.css", import.meta.url), "utf8");
const rewindLib = await readFile(new URL("./lib/conversation-rewind.ts", import.meta.url), "utf8");

test("回退主入口在消息卡片的动作行上，按 run 挂到产生它的那条用户消息", () => {
  // 节点与消息的稳定关联是 run（不是内容）：服务端存 run_id，前端按 runId 建索引。
  assert.match(conversationPage, /const checkpointByRun = useMemo\(\(\) => \{[\s\S]*?map\.set\(checkpoint\.runId, checkpoint\)/);
  assert.match(conversationPage, /const checkpoint = item\.kind === "message" && item\.message\.role === "user" && item\.message\.runId \? checkpointByRun\.get\(item\.message\.runId\) : undefined;/);
  // 节点与运行态经 props 进 MessageCard，与复制按钮同区（.message-actions 动作行）。
  assert.match(conversationPage, /<MessageCard message=\{item\.message\} agentID=\{agentID\} fail=\{fail\} checkpoint=\{checkpoint\} checkpointRunning=\{isUserMessage && item\.message\.runId === activeRunId\}/);
  // 点它按节点能力选默认动作（有代码快照 → 恢复全部；没有 → 只恢复对话），判据在共用模块里。
  assert.match(conversationPage, /const rewindDefault = useCallback\(\(target: ConversationCheckpoint\) => rewindToCheckpoint\(target, rewindPrimaryMode\(target\)\), \[rewindToCheckpoint\]\);/);
  // 回退是图标钮（与复制同一套 ghost 观感），挂在动作行里；无障碍名显式给出，不只靠 title。
  assert.match(conversationPage, /className="message-rewind reveal" disabled=\{rewindDisabled \|\| checkpointBusy\} title=\{rewindTitle\} aria-label="回退到此处" onClick=\{\(\) => rewind\(checkpoint\)\}><RewindInlineIcon \/><\/button>/);
  // 时间线上最后一条用户消息的动作行常驻显形（回退入口的可发现性兜底）。
  assert.match(conversationPage, /persistent=\{isUserMessage && item\.message\.id === persistentUserID\}/);
  assert.match(conversationPage, /data-keep=\{persistent \? "" : undefined\}/);
});

test("没有节点时必须说清为什么，而不是让按钮凭空消失", () => {
  // 这一轮还在跑：节点要等 run 成功结束才生成 → 动作行先挂 pending 说明。
  assert.match(conversationPage, /\{!checkpoint && checkpointRunning && <span className="checkpoint-pending">本轮结束后可回退到此处<\/span>\}/);
  // 失败/停止的回合服务端不留节点：这里什么都不显示，注释说明这不是故障。
  assert.match(conversationPage, /这一轮失败\/被停止：服务端不会留节点/);
  // 行内小字只在"这一轮和常规节点不一样"时出现（无代码快照 / 会丢几轮），文案同样来自共用模块。
  assert.match(conversationPage, /const notes = checkpoint \? rewindHint\(checkpoint, discarded \?\? 0\) : "";/);
  assert.match(conversationPage, /\{checkpoint && notes && <small className="reveal">\{notes\}<\/small>\}/);
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
  assert.match(conversationPage, /import \{ checkpointHasCode, rewindConfirmMessage, rewindHint, rewindPrimaryMode \} from "\.\.\/lib\/conversation-rewind";/);
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

test("头部「节点」列表已撤除：入口只有卡片动作行这一处，禁用判据仍然只有一份", () => {
  // 2026-10-23 上卡片：头部按钮与 popover（含「恢复代码/恢复对话/恢复全部/分支」）撤下。
  // 代价已在方案对比时确认（变体 A：分支暂无落点）；若要找回，按变体 C 加第三颗虚线图标钮。
  assert.doesNotMatch(conversationPage, /回退节点（按回合）/);
  assert.doesNotMatch(conversationPage, /checkpoint-menu|checkpoint-popover|checkpoint-entry/);
  assert.doesNotMatch(conversationPage, /恢复代码<\/button>/);
  // 禁用判据仍只有一份，卡片上的回退钮消费它。
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

test("conversation.css 承载动作行的样式与显形策略", () => {
  // 动作行：用户消息右对齐、AI 消息左对齐；右侧留 18px 避开圆点轨道（z-index 10），别让按钮被压住点不中。
  assert.match(stylesheet, /\.message-actions \{[\s\S]*?display: flex;/);
  assert.match(stylesheet, /\.message\.user \.message-actions \{ justify-content: flex-end; margin-right: 18px; \}/);
  // 回退钮禁用态必须如实降灰，不留"看着能点"的按钮。
  assert.match(stylesheet, /\.message-rewind:disabled \{/);
  // 图标语言与回退箭头同源（同一支 SVG 描边）。
  assert.match(stylesheet, /\.message-action-icon \{ width: 15px; height: 15px;[\s\S]*?stroke: currentColor;/);
  // 显形策略：精确指针设备上 hover / focus 显形，data-keep（最新一轮）与已复制态常显；
  // 触屏（无 hover）保持常显。
  assert.match(stylesheet, /@media \(hover: hover\) and \(pointer: fine\) \{\s*\.message-actions \.reveal \{ opacity: 0; \}/);
  assert.match(stylesheet, /\.message-actions\[data-keep\] \.reveal \{ opacity: 1; \}/);
  assert.match(stylesheet, /\.message-actions \.message-copy\.copied \{ opacity: 1; \}/);
});
