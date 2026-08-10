import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import "./style.css";
import "./markdown.css";
import "./notification.css";
import { App } from "./App";
import { getDesktopRuntime } from "./lib/runtime";
import { TrayPanel } from "./tray/TrayPanel";

// 托盘面板窗口通过 Rust 注入 mode:"tray" 分流渲染：绕过主应用路由，
// 只挂载轻量的品牌弹出面板（不含 WebSocket 轮询等主窗逻辑）。
const isTray = getDesktopRuntime()?.mode === "tray";

createRoot(document.getElementById("root")!).render(
  <StrictMode>{isTray ? <TrayPanel /> : <App />}</StrictMode>
);

