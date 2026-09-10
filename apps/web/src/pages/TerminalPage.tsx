import { useCallback, useEffect, useRef, useState } from "react";
import { useOutletContext, useParams } from "react-router-dom";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import { api, type APIError } from "../lib/api";
import { literalNameClass } from "../lib/utils";
import { createWebSocket } from "../lib/runtime";
import type { ProjectLayoutOutletContext } from "../components/ProjectLayout";
import type { TerminalSessionInfo, TerminalSessionList } from "../lib/types";
import { useActiveConversationId } from "../lib/use-active-conversation";
import "../terminal.css";

function terminalSequence(data: ArrayBuffer): bigint {
  const view = new DataView(data);
  return view.getBigUint64(0, true);
}

// 服务端在会话列表响应里下发真实的并发上限；列表尚未拉回前先使用与服务端默认
// 一致的值，避免拉取前误用旧的硬编码“最多 3 个”。
const FALLBACK_TERMINAL_LIMIT = 8;

export default function TerminalPage() {
  const { projectId } = useParams<{ projectId: string }>();
  const { project } = useOutletContext<ProjectLayoutOutletContext>();
  const hostRef = useRef<HTMLDivElement>(null);
  const termRef = useRef<Terminal | null>(null);
  const wsRef = useRef<WebSocket | null>(null);
  const statusRef = useRef("idle");
  const sequencesRef = useRef(new Map<string, bigint>());
  const projectIDRef = useRef(projectId);
  const creatingRef = useRef<{ projectId: string; promise: Promise<TerminalSessionInfo | null> } | null>(null);
  const [sessions, setSessions] = useState<TerminalSessionInfo[]>([]);
  const [maxSessions, setMaxSessions] = useState(FALLBACK_TERMINAL_LIMIT);
  const [activeID, setActiveID] = useState<string | null>(null);
  const activeIDRef = useRef(activeID);
  activeIDRef.current = activeID;
  const [status, setStatus] = useState("idle");
  const [exitCode, setExitCode] = useState<number | null>(null);
  const [error, setError] = useState("");
  const [replayTruncated, setReplayTruncated] = useState(false);
  const [connectionVersion, setConnectionVersion] = useState(0);
  const [showNewMenu, setShowNewMenu] = useState(false);
  const newMenuRef = useRef<HTMLDivElement>(null);
  projectIDRef.current = projectId;
  const conversationId = useActiveConversationId(projectId);
	const workspaceQuery = conversationId ? `?conversationId=${encodeURIComponent(conversationId)}` : "";
	// Windows 目标允许选择 Shell（cmd / PowerShell），并把上次选择记住为快捷新建默认；
	// WSL / 远程会话的 Shell 由执行环境决定，不提供选择。
	const isWindowsProject = project.environment === "windows";
	const defaultShell = isWindowsProject ? terminalPreferredShell(projectId || "", "cmd") : undefined;

  const createSession = useCallback((shell?: string, runAsAdmin = false) => {
    if (!projectId) return Promise.resolve(null);
    if (creatingRef.current?.projectId === projectId) return creatingRef.current.promise;
    const term = termRef.current;
    const request = api<TerminalSessionInfo>(`/api/projects/${projectId}/terminal/sessions${workspaceQuery}`, {
      method: "POST",
      body: JSON.stringify({ cols: term?.cols || 120, rows: term?.rows || 36, ...(shell ? { shell } : {}), runAsAdmin }),
    }).then((created) => {
      if (projectIDRef.current === projectId) {
        setSessions((current) => [...current.filter((item) => item.id !== created.id), created]);
        setActiveID(created.id);
        setStatus("connecting");
        setExitCode(null);
        setError("");
        setReplayTruncated(false);
        if (shell) terminalRememberShell(projectId, shell);
      }
      return created;
    });
    creatingRef.current = { projectId, promise: request };
    void request.finally(() => {
      if (creatingRef.current?.promise === request) creatingRef.current = null;
    });
    return request;
  }, [projectId, workspaceQuery]);

  const requestNewSession = (shell?: string, runAsAdmin = false) => {
    setShowNewMenu(false);
    void createSession(shell, runAsAdmin).catch((cause) => {
      // 只报错误，不改 status：status 描述的是当前激活终端的连接状态，
      // 新建失败不应把正在运行的终端标成 failed。
      setError(cause instanceof Error ? cause.message : "无法创建终端");
    });
  };

  // 快捷新建：Windows 目标用上次选择的 Shell（默认 cmd），其余环境交给服务端默认。
  const quickNewSession = () => requestNewSession(defaultShell, false);

  const refreshSessions = useCallback(async (): Promise<TerminalSessionList | null> => {
    if (!projectId) return null;
    return api<TerminalSessionList>(`/api/projects/${projectId}/terminal/sessions${workspaceQuery}`);
  }, [projectId, workspaceQuery]);

  // ws 事件驱动的会话状态回写：sessions[] 里对应项的 status 只有在列表刷新/新建时
  // 更新，若只在 status state 上反映实时状态，切到后台 tab 时它的状态点会保持陈旧。
  const patchSessionStatus = (id: string, next: TerminalSessionInfo["status"]) => {
    setSessions((current) => {
      const existing = current.find((item) => item.id === id);
      if (!existing || existing.status === next) return current;
      return current.map((item) => (item.id === id ? { ...item, status: next } : item));
    });
  };

  // 新建下拉菜单：点击菜单外或按 Esc 时收起。
  useEffect(() => {
    if (!showNewMenu) return;
    const onPointerDown = (event: PointerEvent) => {
      if (newMenuRef.current && !newMenuRef.current.contains(event.target as Node)) setShowNewMenu(false);
    };
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") setShowNewMenu(false);
    };
    document.addEventListener("pointerdown", onPointerDown);
    document.addEventListener("keydown", onKeyDown);
    return () => {
      document.removeEventListener("pointerdown", onPointerDown);
      document.removeEventListener("keydown", onKeyDown);
    };
  }, [showNewMenu]);

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const listed = await refreshSessions();
        if (cancelled || !listed) return;
        setSessions(listed.sessions);
        setMaxSessions(listed.maxPerProject);
        const usable = listed.sessions.find((item) => item.status === "running" || item.status === "starting");
        if (usable) {
          setActiveID(usable.id);
          setExitCode(null);
          return;
        }
        // 列表里只剩已退出/失败的会话且已占满服务端上限时，不再自动新建
        // （会撞到 409）：停在首个会话让用户看到退出状态，关闭后即可新建。
        if (listed.sessions.length > 0 && listed.sessions.length >= listed.maxPerProject) {
          setActiveID(listed.sessions[0].id);
          return;
        }
        await createSession(defaultShell, false);
      } catch (cause) {
        if (!cancelled) {
          setStatus("failed");
          setError(cause instanceof Error ? cause.message : "无法创建终端");
        }
      }
    })();
    return () => { cancelled = true; };
  }, [createSession, defaultShell, refreshSessions]);

  useEffect(() => {
    const activeSession = sessions.find((item) => item.id === activeID);
    if (!projectId || !activeID || !hostRef.current || activeSession?.projectId !== projectId) return;
    let disposed = false;
    const term = new Terminal({
      convertEol: false,
      cursorBlink: true,
      scrollback: 5000,
      fontSize: 13,
      fontFamily: '"JetBrains Mono", "Fira Code", "Cascadia Code", ui-monospace, SFMono-Regular, Menlo, monospace',
      lineHeight: 1.35,
      letterSpacing: 0.15,
      theme: {
        background: "#10171b",
        foreground: "#d7e3e7",
        cursor: "#75d7b2",
        cursorAccent: "#10171b",
        selectionBackground: "#315b58",
        black: "#172126",
        red: "#f08b86",
        green: "#75d7b2",
        yellow: "#edc36b",
        blue: "#88b9e8",
        magenta: "#c2a0e8",
        cyan: "#72cbd0",
        white: "#d7e3e7",
        brightBlack: "#6e828a",
        brightRed: "#ffaaa3",
        brightGreen: "#a2efd0",
        brightYellow: "#f6d68d",
        brightBlue: "#acd1f3",
        brightMagenta: "#ddc2fa",
        brightCyan: "#a0e9e9",
        brightWhite: "#f4f8f9",
      },
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(hostRef.current);
    termRef.current = term;
    const resize = () => {
      if (!hostRef.current || hostRef.current.clientWidth === 0) return;
      fit.fit();
      const ws = wsRef.current;
      if (ws?.readyState === WebSocket.OPEN && term.cols > 0 && term.rows > 0) {
        ws.send(JSON.stringify({ type: "resize", cols: term.cols, rows: term.rows }));
      }
    };
    const observer = new ResizeObserver(resize);
    observer.observe(hostRef.current);
    resize();
    statusRef.current = "connecting";
    setStatus("connecting");
    setExitCode(null);
    setError("");
    setReplayTruncated(false);
    const ws = createWebSocket(`/ws/projects/${projectId}/terminal/${activeID}${workspaceQuery}`);
    ws.binaryType = "arraybuffer";
    wsRef.current = ws;
    ws.onopen = () => {
      ws.send(JSON.stringify({ type: "attach", afterSeq: (sequencesRef.current.get(activeID) || 0n).toString(), cols: term.cols || 120, rows: term.rows || 36 }));
    };
    ws.onmessage = (event) => {
      if (typeof event.data === "string") {
        const message = JSON.parse(event.data) as { type?: string; code?: string | number; message?: string; status?: string; truncated?: boolean };
        if (message.type === "ready") {
          const terminalStatus = message.status === "exited" ? "exited" : "running";
          statusRef.current = terminalStatus;
          setStatus(terminalStatus);
          patchSessionStatus(activeID, terminalStatus);
          setExitCode(terminalStatus === "exited" && typeof message.code === "number" ? message.code : null);
          if (terminalStatus === "running") term.focus();
        } else if (message.type === "replay-complete") {
          setReplayTruncated(message.truncated === true);
        } else if (message.type === "exit") {
          statusRef.current = "exited";
          setStatus("exited");
          patchSessionStatus(activeID, "exited");
          setExitCode(typeof message.code === "number" ? message.code : null);
        } else if (message.type === "error") {
          statusRef.current = "failed";
          setStatus("failed");
          patchSessionStatus(activeID, "failed");
          setError(message.message || (typeof message.code === "string" ? message.code : "terminal connection failed"));
        }
        return;
      }
      const data = event.data as ArrayBuffer;
      if (data.byteLength <= 8) return;
      sequencesRef.current.set(activeID, terminalSequence(data));
      term.write(new Uint8Array(data, 8));
    };
    ws.onclose = () => {
      if (!disposed && statusRef.current !== "failed") {
        statusRef.current = "disconnected";
        setStatus("disconnected");
      }
    };
    const input = term.onData((data) => {
      if (ws.readyState !== WebSocket.OPEN || statusRef.current !== "running") return;
      const bytes = new TextEncoder().encode(data);
      const frame = new Uint8Array(8 + bytes.length);
      frame.set(bytes, 8);
      ws.send(frame);
    });
    return () => {
      disposed = true;
      input.dispose();
      observer.disconnect();
      ws.close();
      term.dispose();
      if (termRef.current === term) termRef.current = null;
    };
    // 依赖不含 sessions：关闭/新建后台 tab 会改 sessions，但不应打断当前激活终端
    // 的连接。projectId / activeID / connectionVersion 变化已覆盖需要重建的场景；
    // 顶部的 activeSession 守卫读取的是最新一次渲染的闭包，仍能挡住跨项目误连。
  }, [activeID, connectionVersion, projectId]);

  const closeSession = async (targetID: string) => {
    if (!projectId) return;
    try {
      await api<void>(`/api/projects/${projectId}/terminal/sessions/${targetID}${workspaceQuery}`, { method: "DELETE" });
    } catch (cause) {
      // 服务端 deleteTerminal 的 close() 会先从注册表删会话再关进程，因此
      // 404（已不存在/自动回收）与 5xx（close 失败）都意味着该资源已被服务端
      // 释放，本地应一并清除，避免残留一个永远报 "not found" 的失效条目。
      // 409（工作区未就绪，close 未执行）或网络错误则保留并报错。
      const err = cause as APIError | null;
      const status = err?.status;
      if (status == null || (status !== 404 && status < 500)) {
        setError(cause instanceof Error ? cause.message : "无法关闭终端");
        return;
      }
    }
    sequencesRef.current.delete(targetID);
    setSessions((current) => current.filter((item) => item.id !== targetID));
    setActiveID((current) => {
      if (current !== targetID) return current;
      const remaining = sessions.filter((item) => item.id !== targetID);
      return remaining[0]?.id || null;
    });
    // 用解析时刻的 activeID 判定，避免 DELETE 在途时用户已切到别的会话，
    // 却仍把那个会话的 status/exitCode 误重置。
    if (activeIDRef.current === targetID) {
      setStatus("idle");
      setExitCode(null);
    }
  };

  const dotForStatus = (statusKey: string) =>
    statusKey === "running" ? "is-running" : statusKey === "connecting" || statusKey === "starting" ? "is-booting" : statusKey === "exited" || statusKey === "disconnected" ? "is-done" : statusKey === "failed" ? "is-failed" : "is-stale";
  const activateSession = (id: string) => {
    if (id !== activeID) {
      setActiveID(id);
      setConnectionVersion((version) => version + 1);
      // 立即把状态置为 connecting，避免切换瞬间沿用上个会话的实时状态一帧
      setStatus("connecting");
      setExitCode(null);
      setError("");
      setReplayTruncated(false);
    }
  };
  const handleTabKeyDown = (event: React.KeyboardEvent<HTMLButtonElement>, targetID: string) => {
    const index = sessions.findIndex((item) => item.id === targetID);
    if (index < 0) return;
    let nextIndex = index;
    if (event.key === "ArrowRight") nextIndex = (index + 1) % sessions.length;
    else if (event.key === "ArrowLeft") nextIndex = (index - 1 + sessions.length) % sessions.length;
    else if (event.key === "Home") nextIndex = 0;
    else if (event.key === "End") nextIndex = sessions.length - 1;
    else return;
    event.preventDefault();
    const next = sessions[nextIndex];
    if (!next) return;
    activateSession(next.id);
    requestAnimationFrame(() => document.querySelector<HTMLButtonElement>(`[data-terminal-tab="${CSS.escape(next.id)}"]`)?.focus());
  };

  const selected = sessions.find((item) => item.id === activeID);
  const canReconnect = Boolean(activeID) && status !== "running" && status !== "connecting";
  const statusLabel = status === "running" ? "运行中" : status === "connecting" ? "连接中" : status === "disconnected" ? "已断开" : status === "exited" ? "已退出" : status === "failed" ? "不可用" : "未启动";
  const reachedLimit = sessions.length >= maxSessions;
  const limitTitle = reachedLimit ? `已达会话上限（${maxSessions} 个）：先关闭不再使用的会话后即可新建` : undefined;
  return <section className="terminal-page">
    <header className="terminal-toolbar">
      <div className="terminal-title">
        <div className="terminal-title-heading"><span className="terminal-title-icon" aria-hidden="true"><TerminalIcon /></span><span className="terminal-kicker">项目终端</span></div>
        <h3 title={project.name} className={literalNameClass(project.name)}>{project.name}</h3>
      </div>
      <div className="terminal-actions">
        <div className="terminal-button-group">
          {isWindowsProject ? (
            <div className="terminal-split-wrap" ref={newMenuRef}>
              <div className="terminal-split-group">
                <button type="button" aria-label="新建终端" onClick={quickNewSession} disabled={reachedLimit} title={limitTitle ?? "新建终端"}><PlusIcon /><span>新建</span></button>
                <button type="button" className="terminal-split-caret" aria-label="新建终端选项" aria-haspopup="menu" aria-expanded={showNewMenu} disabled={reachedLimit} title={limitTitle ?? "选择 Shell"} onClick={() => setShowNewMenu((open) => !open)}><ChevronDownIcon /></button>
              </div>
              {showNewMenu && (
                <div className="terminal-new-menu" role="menu" aria-label="新建终端 Shell">
                  <button type="button" role="menuitem" onClick={() => requestNewSession("cmd", false)}><CmdIcon /><span><em>命令提示符</em><i>cmd.exe</i></span></button>
                  <button type="button" role="menuitem" onClick={() => requestNewSession("powershell", false)}><PowershellIcon /><span><em>Windows PowerShell</em><i>powershell.exe</i></span></button>
                  <div className="terminal-new-menu-separator" role="separator" />
                  <button type="button" role="menuitem" className="terminal-new-menu-admin" onClick={() => requestNewSession("cmd", true)}><ShieldIcon /><span><em>命令提示符（管理员）</em><i>UAC 授权后运行</i></span></button>
                  <button type="button" role="menuitem" className="terminal-new-menu-admin" onClick={() => requestNewSession("powershell", true)}><ShieldIcon /><span><em>Windows PowerShell（管理员）</em><i>UAC 授权后运行</i></span></button>
                </div>
              )}
            </div>
          ) : (
            <button type="button" aria-label="新建终端" onClick={quickNewSession} disabled={reachedLimit} title={limitTitle ?? "新建终端"}><PlusIcon /><span>新建</span></button>
          )}
          <button type="button" aria-label="重新连接" onClick={() => setConnectionVersion((version) => version + 1)} disabled={!canReconnect} title="重新连接"><ReconnectIcon /><span>重连</span></button>
          <button type="button" aria-label="清空终端输出" onClick={() => termRef.current?.clear()} disabled={!activeID} title="清空终端输出"><ClearIcon /><span>清屏</span></button>
        </div>
      </div>
      <strong className={`terminal-status ${status}`}><i aria-hidden="true" />{statusLabel}</strong>
    </header>
    <div className="terminal-tabs" role="tablist" aria-label="终端会话">
      {sessions.map((item, index) => {
        const isActive = item.id === activeID;
        return (
          <div key={item.id} className={`terminal-tab ${isActive ? "active" : ""}`}
            onMouseDown={(event) => { if (event.button === 1) { event.preventDefault(); void closeSession(item.id); } }}>
            <button type="button" className="terminal-tab-select" role="tab" aria-selected={isActive} tabIndex={isActive ? 0 : -1}
              data-terminal-tab={item.id} title={`终端 ${index + 1} · ${sessionTitle(item)}`}
              onClick={() => activateSession(item.id)} onKeyDown={(event) => handleTabKeyDown(event, item.id)}>
              <span className={`terminal-tab-dot ${dotForStatus(item.id === activeID ? status : item.status)}`} aria-hidden="true"><i /></span>
              <span className="terminal-tab-label">终端 {index + 1}</span>
              {item.elevated && <span className="terminal-tab-shield" title="以管理员身份运行"><ShieldIcon /></span>}
              <span className="terminal-tab-env">{sessionEnvText(item)}</span>
            </button>
            <button type="button" className="terminal-tab-close" aria-label={`关闭终端 ${index + 1}`} title="关闭终端"
              onClick={() => void closeSession(item.id)}><CloseIcon /></button>
          </div>
        );
      })}
      <button type="button" className="terminal-tab-add" aria-label="新建终端" onClick={quickNewSession} disabled={reachedLimit} title={limitTitle ?? "新建终端"}><PlusIcon /></button>
    </div>
    <div className="terminal-meta">
      {selected ? <>
        <span className="terminal-meta-environment">{sessionEnvText(selected)}</span>
        {selected.elevated && <span className="terminal-meta-admin" title="以管理员身份运行"><ShieldIcon /><span>管理员</span></span>}
        <span className="terminal-meta-separator" aria-hidden="true">/</span>
        <code title={project.pathDisplay}>{project.pathDisplay}</code>
      </> : <span>没有活动终端</span>}
      <span className={`terminal-meta-count${reachedLimit ? " is-full" : ""}`} title={limitTitle ?? `${sessions.length}/${maxSessions} 会话`}>{sessions.length}/{maxSessions} 会话{reachedLimit ? "，已达上限" : ""}</span>
    </div>
    <div ref={hostRef} className="terminal-host">
      {(!activeID || selected?.projectId !== projectId) && <div className="terminal-empty"><span className="terminal-empty-icon" aria-hidden="true"><TerminalIcon /></span><strong>尚未选择终端</strong><span>创建一个新会话开始工作</span></div>}
    </div>
    {status === "exited" && exitCode !== null && <p className="terminal-exit-code">exit code {exitCode}</p>}
    {replayTruncated && <p className="terminal-notice">{"\u90e8\u5206\u7ec8\u7aef\u8f93\u51fa\u5df2\u8fc7\u671f\uff0c\u65e0\u6cd5\u6062\u590d"}</p>}
    {error && <p className="terminal-error" role="alert">{error}</p>}
  </section>;
}

function TerminalIcon() { return <svg viewBox="0 0 24 24" aria-hidden="true"><rect x="3.5" y="4" width="17" height="16" rx="2.5" /><path d="m7 9 3 3-3 3M13 15h4" /></svg>; }
function CmdIcon() { return <svg viewBox="0 0 24 24" aria-hidden="true"><rect x="3.5" y="4" width="17" height="16" rx="2.5" /><path d="m7 9 3 3-3 3M13 15h4" /></svg>; }
function PowershellIcon() { return <svg viewBox="0 0 24 24" aria-hidden="true"><path d="m4 7 5 5-5 5M11 17h8" /></svg>; }
function ShieldIcon() { return <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 3 5 5.8v5.4c0 4.5 3 7.9 7 9.3 4-1.4 7-4.8 7-9.3V5.8L12 3Z" /><path d="m9 11.8 2 2 4-4.4" /></svg>; }
function ChevronDownIcon() { return <svg viewBox="0 0 24 24" aria-hidden="true"><path d="m6 9 6 6 6-6" /></svg>; }
function PlusIcon() { return <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 5v14M5 12h14" /></svg>; }
function ReconnectIcon() { return <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M19 8a7 7 0 0 0-12-1L5 9" /><path d="M5 5v4h4M5 16a7 7 0 0 0 12 1l2-2" /><path d="M19 19v-4h-4" /></svg>; }
function ClearIcon() { return <svg viewBox="0 0 24 24" aria-hidden="true"><path d="m6 7 1 13h10l1-13M4 7h16M9 7V4h6v3M10 11v5M14 11v5" /></svg>; }
function CloseIcon() { return <svg viewBox="0 0 24 24" aria-hidden="true"><path d="m7 7 10 10M17 7 7 17" /></svg>; }

const TERMINAL_SHELL_KEY = "milevia.terminal.shell.";
function terminalPreferredShell(projectId: string, fallback: string): string {
  try {
    const raw = window.localStorage.getItem(TERMINAL_SHELL_KEY + projectId);
    if (raw === "powershell" || raw === "cmd") return raw;
  } catch { /* localStorage 不可用时回退默认 */ }
  return fallback;
}
function terminalRememberShell(projectId: string, shell: string) {
  try { window.localStorage.setItem(TERMINAL_SHELL_KEY + projectId, shell); } catch { /* ignore */ }
}
function environmentLabel(environment: string): string {
  switch (environment) {
    case "windows": return "Windows";
    case "wsl": return "WSL";
    case "remote-linux": return "远程 Linux";
    default: return environment;
  }
}
function shellLabel(shell?: string): string {
  if (shell === "powershell") return "PowerShell";
  if (shell === "cmd") return "命令提示符";
  return "";
}
function sessionEnvText(item: TerminalSessionInfo): string {
  const shell = shellLabel(item.shell);
  return shell ? `${environmentLabel(item.environment)} · ${shell}` : environmentLabel(item.environment);
}
function sessionTitle(item: TerminalSessionInfo): string {
  return item.elevated ? `${sessionEnvText(item)} · 管理员` : sessionEnvText(item);
}
