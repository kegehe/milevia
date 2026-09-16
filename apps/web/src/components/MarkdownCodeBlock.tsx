import { useEffect, useRef, useState, type ReactNode } from "react";
import type { Components } from "react-markdown";
import { copyToClipboard } from "../lib/clipboard";

// 代码块右上角的复制按钮。
//
// react-markdown 的 pre/code 默认渲染成裸的 <pre><code>，这里把 pre 包进一个定位容器，
// 按钮浮在右上方：不改变 pre 自身的横向滚动与排版，也不依赖语法高亮库。
//
// 取代码文本走 React 子树遍历（而不是 react-markdown 的 node 字段）：v10 的 node 只在
// passNode 打开时才有，子树遍历在任何版本都成立，且 SSR / 测试里可直接断言。

const COPY_FEEDBACK_MS = 1_600;

type CopyState = "idle" | "copied" | "failed";

function textContent(node: ReactNode): string {
  if (typeof node === "string") return node;
  if (typeof node === "number") return String(node);
  if (Array.isArray(node)) return node.map(textContent).join("");
  if (node && typeof node === "object" && "props" in node) {
    return textContent((node as { props?: { children?: ReactNode } }).props?.children);
  }
  return "";
}

export function MarkdownCodeBlock({ children }: { children?: ReactNode }) {
  const code = textContent(children);
  const [state, setState] = useState<CopyState>("idle");
  const timer = useRef<number | null>(null);
  const mounted = useRef(true);

  useEffect(() => {
    // StrictMode 下 effect 会「挂载 → 清理 → 再挂载」跑两次，标记必须在这里设回 true，
    // 否则开发模式下复制反馈会彻底失效（生产构建不双跑，看不出这个问题）。
    mounted.current = true;
    return () => {
      mounted.current = false;
      if (timer.current !== null) window.clearTimeout(timer.current);
    };
  }, []);

  const copy = async () => {
    const copied = await copyToClipboard(code);
    // 写入期间组件可能已经卸载（流式消息重排），此时不要再排一个没人清理的计时器。
    if (!mounted.current) return;
    setState(copied ? "copied" : "failed");
    if (timer.current !== null) window.clearTimeout(timer.current);
    timer.current = window.setTimeout(() => setState("idle"), COPY_FEEDBACK_MS);
  };

  const label = state === "copied" ? "已复制" : state === "failed" ? "复制失败" : "复制";
  const hint = state === "copied" ? "已复制到剪贴板" : state === "failed" ? "复制失败，请检查剪贴板权限" : "复制代码";

  return (
    <div className="markdown-code-block">
      <pre>{children}</pre>
      <button type="button" className={`markdown-code-copy${state === "idle" ? "" : ` ${state}`}`} title={hint} aria-label={hint} onClick={() => void copy()}>
        <MarkdownCopyIcon copied={state === "copied"} />
        <span className="markdown-code-copy-label">{label}</span>
      </button>
    </div>
  );
}

function MarkdownCopyIcon({ copied }: { copied: boolean }) {
  if (copied) return <svg className="markdown-code-copy-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="m5 12.5 4.5 4.5L19 7.5" /></svg>;
  return <svg className="markdown-code-copy-icon" viewBox="0 0 24 24" aria-hidden="true"><rect x="9" y="9" width="10.5" height="10.5" rx="2.2" /><path d="M15 5.6H6.6a2.1 2.1 0 0 0-2.1 2.1V16" /></svg>;
}

// 各渲染入口只需要把这份映射并进自己的 components 即可获得稳定的复制按钮。
export const markdownCodeComponents: Components = { pre: MarkdownCodeBlock };
