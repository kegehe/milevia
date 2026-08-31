import { useCallback, useEffect, useRef, useState } from "react";
import { useOutletContext, useParams } from "react-router-dom";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import { api } from "../lib/api";
import { createWebSocket } from "../lib/runtime";
import type { ProjectLayoutOutletContext } from "../components/ProjectLayout";
import type { TerminalSessionInfo } from "../lib/types";
import { useActiveConversationId } from "../lib/use-active-conversation";
import "../terminal.css";

function terminalSequence(data: ArrayBuffer): bigint {
  const view = new DataView(data);
  return view.getBigUint64(0, true);
}

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
  const [activeID, setActiveID] = useState<string | null>(null);
  const [status, setStatus] = useState("idle");
  const [exitCode, setExitCode] = useState<number | null>(null);
  const [error, setError] = useState("");
  const [replayTruncated, setReplayTruncated] = useState(false);
  const [connectionVersion, setConnectionVersion] = useState(0);
  projectIDRef.current = projectId;
  const conversationId = useActiveConversationId(projectId);
	const workspaceQuery = conversationId ? `?conversationId=${encodeURIComponent(conversationId)}` : "";

  const createSession = useCallback(() => {
    if (!projectId) return Promise.resolve(null);
    if (creatingRef.current?.projectId === projectId) return creatingRef.current.promise;
    const term = termRef.current;
    const request = api<TerminalSessionInfo>(`/api/projects/${projectId}/terminal/sessions${workspaceQuery}`, {
      method: "POST",
      body: JSON.stringify({ cols: term?.cols || 120, rows: term?.rows || 36 }),
    }).then((created) => {
      if (projectIDRef.current === projectId) {
        setSessions((current) => [...current.filter((item) => item.id !== created.id), created]);
        setActiveID(created.id);
        setExitCode(null);
        setError("");
        setReplayTruncated(false);
      }
      return created;
    });
    creatingRef.current = { projectId, promise: request };
    void request.finally(() => {
      if (creatingRef.current?.promise === request) creatingRef.current = null;
    });
    return request;
  }, [projectId, workspaceQuery]);

  const refreshSessions = useCallback(async () => {
    if (!projectId) return [];
    return api<TerminalSessionInfo[]>(`/api/projects/${projectId}/terminal/sessions${workspaceQuery}`);
  }, [projectId, workspaceQuery]);

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const listed = await refreshSessions();
        if (cancelled) return;
        setSessions(listed);
        const usable = listed.find((item) => item.status === "running" || item.status === "starting");
        if (usable) {
          setActiveID(usable.id);
          setExitCode(null);
          return;
        }
        await createSession();
      } catch (cause) {
        if (!cancelled) {
          setStatus("failed");
          setError(cause instanceof Error ? cause.message : "无法创建终端");
        }
      }
    })();
    return () => { cancelled = true; };
  }, [createSession, refreshSessions]);

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
          setExitCode(terminalStatus === "exited" && typeof message.code === "number" ? message.code : null);
          if (terminalStatus === "running") term.focus();
        } else if (message.type === "replay-complete") {
          setReplayTruncated(message.truncated === true);
        } else if (message.type === "exit") {
          statusRef.current = "exited";
          setStatus("exited");
          setExitCode(typeof message.code === "number" ? message.code : null);
        } else if (message.type === "error") {
          statusRef.current = "failed";
          setStatus("failed");
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
  }, [activeID, connectionVersion, projectId, sessions]);

  const closeActive = async () => {
    if (!projectId || !activeID) return;
    try {
      await api<void>(`/api/projects/${projectId}/terminal/sessions/${activeID}${workspaceQuery}`, { method: "DELETE" });
      sequencesRef.current.delete(activeID);
      const remaining = sessions.filter((item) => item.id !== activeID);
      setSessions(remaining);
      setActiveID(remaining[0]?.id || null);
      setStatus("idle");
      setExitCode(null);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "无法关闭终端");
    }
  };

  const selected = sessions.find((item) => item.id === activeID);
  const canReconnect = Boolean(activeID) && status !== "running" && status !== "connecting";
  const statusLabel = status === "running" ? "运行中" : status === "connecting" ? "连接中" : status === "disconnected" ? "已断开" : status === "exited" ? "已退出" : status === "failed" ? "不可用" : "未启动";
  return <section className="terminal-page">
    <header className="terminal-toolbar">
      <div className="terminal-title">
        <div className="terminal-title-heading"><span className="terminal-title-icon" aria-hidden="true"><TerminalIcon /></span><span className="terminal-kicker">项目终端</span></div>
        <h3 title={project.name}>{project.name}</h3>
      </div>
      <div className="terminal-actions">
        <label className="terminal-session-picker">
          <span className="terminal-session-icon" aria-hidden="true"><SessionIcon /></span>
          <select aria-label="终端会话" value={activeID || ""} onChange={(event) => { setActiveID(event.target.value || null); setConnectionVersion((version) => version + 1); }}>
          <option value="" disabled>选择终端</option>
          {sessions.map((item, index) => <option key={item.id} value={item.id}>终端 {index + 1} · {item.environment}</option>)}
          </select>
        </label>
        <div className="terminal-button-group">
          <button type="button" aria-label="新建终端" onClick={() => void createSession()} disabled={sessions.length >= 3} title="新建终端"><PlusIcon /><span>新建</span></button>
          <button type="button" aria-label="重新连接" onClick={() => setConnectionVersion((version) => version + 1)} disabled={!canReconnect} title="重新连接"><ReconnectIcon /><span>重连</span></button>
          <button type="button" aria-label="清空终端输出" onClick={() => termRef.current?.clear()} disabled={!activeID} title="清空终端输出"><ClearIcon /><span>清屏</span></button>
          <button type="button" aria-label="关闭当前终端" className="terminal-close" onClick={() => void closeActive()} disabled={!activeID} title="关闭当前终端"><CloseIcon /><span>关闭</span></button>
        </div>
      </div>
      <strong className={`terminal-status ${status}`}><i aria-hidden="true" />{statusLabel}</strong>
    </header>
    <div className="terminal-meta">
      {selected ? <><span className="terminal-meta-environment">{selected.environment}</span><span className="terminal-meta-separator" aria-hidden="true">/</span><code title={project.pathDisplay}>{project.pathDisplay}</code></> : <span>没有活动终端</span>}
      <span className="terminal-meta-count">{sessions.length}/3 会话</span>
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
function SessionIcon() { return <svg viewBox="0 0 24 24" aria-hidden="true"><rect x="4" y="5" width="16" height="14" rx="2" /><path d="M7 8h10M7 12h4M7 16h7" /></svg>; }
function PlusIcon() { return <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 5v14M5 12h14" /></svg>; }
function ReconnectIcon() { return <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M19 8a7 7 0 0 0-12-1L5 9" /><path d="M5 5v4h4M5 16a7 7 0 0 0 12 1l2-2" /><path d="M19 19v-4h-4" /></svg>; }
function ClearIcon() { return <svg viewBox="0 0 24 24" aria-hidden="true"><path d="m6 7 1 13h10l1-13M4 7h16M9 7V4h6v3M10 11v5M14 11v5" /></svg>; }
function CloseIcon() { return <svg viewBox="0 0 24 24" aria-hidden="true"><path d="m7 7 10 10M17 7 7 17" /></svg>; }
