// 工作区面板错误边界测试。
//
// 边界本身的价值是"渲染异常不再打白整页"，所以这里断言：兜底 UI 能渲染、
// 状态里的错误能一路传到兜底 UI、正常时子面板原样透传不包 DOM、重试能清掉错误。
//
// 注意：不要试图用 renderToString 渲染一个会抛错的子组件来验证"边界接住了错误"——
// 服务端的 legacy 渲染器不会把子组件抛出的错误交给边界（客户端才会），那样写只会
// 让异常直接逃出去。所以兜底 UI 拆成了独立组件，这里直接渲染它。

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { createElement, type ReactElement } from "react";
import { renderToString } from "react-dom/server";

import { WorkspacePanelErrorBoundary, WorkspacePanelErrorFallback } from "./WorkspacePanelErrorBoundary.tsx";

// 复刻踩过的真实异常：根提交 parents=null 时前端读 .length 抛出的就是这条。
const CRASH_MESSAGE = "Cannot read properties of null (reading 'length')";

test("renders the panel fallback with the message and a retry action", () => {
  const html = renderToString(createElement(WorkspacePanelErrorFallback, { message: CRASH_MESSAGE, onRetry: () => undefined }));

  assert.match(html, /class="workspace-panel-error"/);
  assert.match(html, /role="alert"/);
  assert.match(html, /这个面板出错了/);
  assert.match(html, /其他面板仍可正常使用/);
  assert.match(html, />重试</);
  // 原始错误信息必须显示出来，否则用户只能看到一句没有线索的提示。
  assert.match(html, /Cannot read properties of null/);
});

test("renders children untouched and adds no wrapper element while nothing throws", () => {
  const html = renderToString(createElement(WorkspacePanelErrorBoundary, { resetKey: "/projects/p1/git" },
    createElement("div", { className: "workspace-tab-panel" }, "面板内容")));

  // 多包一层 DOM 会打断 .workspace-content 的 flex 布局（> .workspace-tab-panel）。
  assert.equal(html, '<div class="workspace-tab-panel">面板内容</div>');
});

test("renders the fallback once an error is caught and children again once it clears", () => {
  const boundary = new WorkspacePanelErrorBoundary({ resetKey: "/projects/p1/git", children: createElement("i", null, "面板内容") });

  // 未出错：原样返回 children。
  assert.equal(boundary.render(), boundary.props.children);

  // 出错：返回兜底组件，并把错误信息带过去。
  boundary.state = { error: new Error(CRASH_MESSAGE) };
  const fallback = boundary.render() as ReactElement<{ message: string; onRetry: () => void }>;
  assert.equal(fallback.type, WorkspacePanelErrorFallback);
  assert.equal(fallback.props.message, CRASH_MESSAGE);

  // 「重试」必须真的把错误状态清掉，而不只是挂了个函数上去。
  const updates: unknown[] = [];
  boundary.setState = ((next: unknown) => { updates.push(next); }) as unknown as typeof boundary.setState;
  fallback.props.onRetry();
  assert.deepEqual(updates, [{ error: null }]);

  // 没有 message 的异常也不能渲染出空白文本。
  boundary.state = { error: new Error("") };
  assert.equal((boundary.render() as ReactElement<{ message: string }>).props.message, "Error");
});

test("captures the thrown error into state and resets through the route-derived resetKey", () => {
  const error = new Error(CRASH_MESSAGE);

  assert.equal(WorkspacePanelErrorBoundary.getDerivedStateFromError(error).error, error);

  // resetKey 变化只清错误状态、不重挂 children：加边界不能让子路由丢掉已有状态。
  // 这段依赖 componentDidUpdate，纯渲染覆盖不到，所以用源码断言兜住。
  const source = readFileSync(new URL("./WorkspacePanelErrorBoundary.tsx", import.meta.url), "utf8");

  assert.match(source, /if \(previous\.resetKey !== this\.props\.resetKey && this\.state\.error\) this\.setState\(\{ error: null \}\);/);
});

test("keeps ProjectLayout wiring the boundary around the outlet, keyed by pathname", () => {
  // resetKey 用 pathname，才能"崩一次之后仍能切到别的面板"。
  const layout = readFileSync(new URL("./ProjectLayout.tsx", import.meta.url), "utf8");

  assert.match(layout, /<WorkspacePanelErrorBoundary resetKey=\{location\.pathname\}>/);
  assert.match(layout, /<Outlet context=/);
  assert.match(layout, /<\/WorkspacePanelErrorBoundary>/);
  // 关键：不能改写成给边界传 key —— 那样切路由会强制重挂子路由、丢掉组件状态，
  // 等于顺手改掉别人的行为。复位必须走 resetKey。
  assert.doesNotMatch(layout, /<WorkspacePanelErrorBoundary[^>]*\skey=/);
});

test("styles the fallback inside the workspace content area", () => {
  const styles = readFileSync(new URL("../conversation.css", import.meta.url), "utf8");
  const rule = /\.workspace-content > \.workspace-panel-error \{([^}]*)\}/.exec(styles);

  // 兜底 UI 会直接替代 .workspace-tab-panel，必须自己撑满并接管滚动。
  assert.ok(rule, "conversation.css 里缺少 .workspace-content > .workspace-panel-error 规则");
  assert.match(rule[1], /flex: 1;/);
  assert.match(rule[1], /overflow: auto;/);
});
