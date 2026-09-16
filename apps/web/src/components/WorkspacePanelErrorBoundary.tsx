// 工作区面板错误边界 — 子面板在渲染期抛错时只替换该面板，而不是白掉整页。
//
// 全站没有别的 ErrorBoundary，而 React 的默认行为是把抛错的那棵子树整个卸载，
// 于是任何一次渲染期异常都会表现成"整页空白"（已实测：根提交 parents=null 让
// Git 工作台分支页整页空白）。这一层是"渲染异常不再打白整页"的唯一保障。
//
// 两条实现约束：
// 1. 不额外包一层 DOM：正常路径直接返回 children，`.workspace-content` 的 flex
//    布局（`> .workspace-tab-panel { flex: 1 }`）才不会被打断。
// 2. 用 resetKey 清错误状态，不用 key 强制重挂：切换路由时子路由能保留原有的
//    组件状态，行为与加边界之前完全一致。

import { Component, type ErrorInfo, type PropsWithChildren } from "react";

// 与 ProjectLayout 的 ErrorAlertIcon 同一图形，错误语义保持一致。
function PanelErrorIcon() {
  return <svg className="workspace-panel-error-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M12 8.5v4.5M12 16.5h.01M10.3 4.5 2.6 18a2 2 0 0 0 1.74 3h15.32a2 2 0 0 0 1.74-3L13.7 4.5a2 2 0 0 0-3.4 0Z" /></svg>;
}

// 兜底界面独立成组件：SSR 的 renderToString 不会把子组件抛出的错误交给边界
// （客户端才会），拆开之后这条 UI 才能被直接渲染断言。
export function WorkspacePanelErrorFallback({ message, onRetry }: { message: string; onRetry: () => void }) {
  return <div className="workspace-panel-error" role="alert">
    <PanelErrorIcon />
    <b>这个面板出错了</b>
    <p>其他面板仍可正常使用。可以点「重试」，或切到别的标签再切回来。</p>
    <pre>{message}</pre>
    <button className="secondary" type="button" onClick={onRetry}>重试</button>
  </div>;
}

// children 用 PropsWithChildren 声明成可选：写成必填会让 createElement(边界, {resetKey}, 子元素)
// 这种标准调用在类型上不合法（React 的 Attributes & Props 会要求 props 里必须有 children）。
type Props = PropsWithChildren<{
  // 复位标识：值一变就清掉错误状态。传 location.pathname，切工作区标签或子路由
  // （会话 / 任务）后自动恢复；否则崩一次就再也进不去别的面板。
  resetKey: string;
}>;

type State = { error: Error | null };

export class WorkspacePanelErrorBoundary extends Component<Props, State> {
  state: State = { error: null };

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    // 界面只给一条简短提示，完整堆栈留在控制台，方便定位。
    console.error("[Milevia] 工作区面板渲染失败：", error, info.componentStack);
  }

  componentDidUpdate(previous: Props) {
    // 只在已经出错时才复位，且只清状态、不重挂 children。
    if (previous.resetKey !== this.props.resetKey && this.state.error) this.setState({ error: null });
  }

  render() {
    const { error } = this.state;
    if (!error) return this.props.children;
    return <WorkspacePanelErrorFallback message={error.message || String(error)} onRetry={() => this.setState({ error: null })} />;
  }
}
