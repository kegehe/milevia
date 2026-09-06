import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import "./style.css";
import "./markdown.css";
import "./notification.css";
import { App } from "./App";
import { getDesktopRuntime, isDesktop } from "./lib/runtime";
import { TrayPanel } from "./tray/TrayPanel";

// 托盘面板窗口通过 Rust 注入 mode:"tray" 分流渲染：绕过主应用路由，
// 只挂载轻量的品牌弹出面板（不含 WebSocket 轮询等主窗逻辑）。
const isTray = getDesktopRuntime()?.mode === "tray";

if (!isTray && "serviceWorker" in navigator && window.location.protocol.startsWith("http")) {
  window.addEventListener("load", () => { void navigator.serviceWorker.register("/sw.js"); });
}

// 仅桌面端正式版（Tauri WebView 中，且非 vite dev）隐藏 WebView2 的默认右键菜单
// （返回/刷新/另存为/打印 等浏览器菜单），但保留文本输入处的系统编辑菜单
// （剪切/复制/粘贴/全选）。
// - 开发模式保留完整菜单，便于右键“检查元素”调试前端。
// - Web 端（移动远程页等）不抑制：浏览器自有右键/长按菜单（复制、保存图片等）仍可用。
if (!import.meta.env.DEV && isDesktop()) {
  document.addEventListener(
    "contextmenu",
    (event) => {
      // 仅文本编辑控件（输入框/文本域/可编辑富文本，含 CodeMirror 编辑器的
      // .cm-content）保留系统编辑菜单（剪切/复制/粘贴/全选）；checkbox/radio/range
      // 等非文本输入与其余位置一律抑制 WebView2 默认菜单（返回/刷新/另存为/打印）。
      // 应用自身的自定义右键菜单（文件树/文件标签页等）在各自的 onContextMenu 中
      // preventDefault，不受影响。
      const target = event.target;
      if (
        target instanceof Element &&
        target.closest(
          "input:not([type]), input[type='text'], input[type='search'], input[type='url'], " +
            "input[type='tel'], input[type='email'], input[type='password'], input[type='number'], " +
            "textarea, [contenteditable]:not([contenteditable='false'])",
        )
      ) {
        return;
      }
      event.preventDefault();
    },
    true, // 捕获阶段，先于 React 合成事件执行
  );
}

createRoot(document.getElementById("root")!).render(
  <StrictMode>{isTray ? <TrayPanel /> : <App />}</StrictMode>
);

