import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const conversationPage = await readFile(new URL("./pages/ConversationPage.tsx", import.meta.url), "utf8");

// 真机现象：会话内容明明已经翻到最上面，"加载更早记录"按钮却还挂着。
//
// 根因有两层。服务端那层是 hasMore 把过程遥测（stream_event / thinking_tokens）也算进了
// "还有更早的记录"，已在 getConversation 侧修掉（见 internal/app/event_replay.go）。
// 前端这层是：reload / 轮询拿回来的**永远是最新一页**，却无条件用它回包的 nextCursor
// 和 hasMore 覆盖分页状态 —— 用户往回翻过页之后，游标被倒回最新一页的边界，不但已经
// 翻过的页要重翻一遍，按钮还会在明明已经到底之后重新亮起。

test("history paging state is only rewritten by the page that actually advanced it", () => {
  // 用户往回翻过页的标记。
  assert.match(conversationPage, /const pagedBackRef = useRef\(false\);/);

  // reload（WebSocket 重连 / 首屏补拉）与 HTTP 轮询兜底都只认最新一页：可以合并内容，
  // 但不能碰分页状态。
  assert.match(
    conversationPage,
    /if \(!pagedBackRef\.current\) \{\s*setHasMoreHistory\(data\.hasMore\); setHasMoreMessageHistory\(data\.hasMoreMessages\); setHistoryCursor\(data\.nextCursor \|\| ""\);\s*\}/,
  );
  assert.match(
    conversationPage,
    /if \(!pagedBackRef\.current\) \{\s*setHasMoreHistory\(full\.hasMore\); setHasMoreMessageHistory\(full\.hasMoreMessages\); setHistoryCursor\(full\.nextCursor \|\| ""\);\s*\}/,
  );

  // 只有"加载更早记录"自己翻页时游标才允许往前走。两行之间留出余量：注释怎么写、
  // 中间插不插空行都不该让这条断言误报。
  assert.match(conversationPage, /setHistoryCursor\(data\.nextCursor \|\| ""\);[\s\S]{0,240}?pagedBackRef\.current = true;/);

  // 切换会话 / 清空会话后回到干净状态，新会话的首屏仍然要能建立游标。
  assert.match(conversationPage, /setHistoryCursor\(""\);\s*pagedBackRef\.current = false;/);

  // 原来那两处无条件覆盖分页状态的写法不能再出现。
  assert.doesNotMatch(conversationPage, /\}\);\s*setHasMoreHistory\(data\.hasMore\); setHasMoreMessageHistory\(data\.hasMoreMessages\); setHistoryCursor\(data\.nextCursor \|\| ""\);/);
  assert.doesNotMatch(conversationPage, /setRun\(full\.activeRunId \|\| ""\); setHasMoreHistory\(full\.hasMore\);/);
});

test("the load-earlier button still keys off the server's hasMore", () => {
  assert.match(conversationPage, /\{hasMoreHistory && <button className="secondary load-earlier-history"/);
  assert.match(conversationPage, /onClick=\{\(\) => void loadOlderHistory\(\)\}/);
});
