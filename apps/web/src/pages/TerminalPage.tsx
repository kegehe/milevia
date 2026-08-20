import { useCallback, useEffect, useRef, useState } from "react";
import { useOutletContext, useParams } from "react-router-dom";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import { api } from "../lib/api";
import { createWebSocket } from "../lib/runtime";
import type { ProjectLayoutOutletContext } from "../components/ProjectLayout";
import type { TerminalSessionInfo } from "../lib/types";
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

  const createSession = useCallback(() => {
    if (!projectId) return Promise.resolve(null);
    if (creatingRef.current?.projectId === projectId) return creatingRef.current.promise;
    const term = termRef.current;
    const request = api<TerminalSessionInfo>(`/api/projects/${projectId}/terminal/sessions`, {
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
  }, [projectId]);

  const refreshSessions = useCallback(async () => {
    if (!projectId) return [];
    return api<TerminalSessionInfo[]>(`/api/projects/${projectId}/terminal/sessions`);
  }, [projectId]);

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
      theme: { background: "#101917", foreground: "#d4e7df", cursor: "#7dd3b0" },
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
    const ws = createWebSocket(`/ws/projects/${projectId}/terminal/${activeID}`);
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
      await api<void>(`/api/projects/${projectId}/terminal/sessions/${activeID}`, { method: "DELETE" });
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
  return <section className="terminal-page">
    <header className="terminal-toolbar">
      <div className="terminal-title"><span>项目终端</span><h3>{project.name}</h3></div>
      <div className="terminal-actions">
        <select aria-label="终端会话" value={activeID || ""} onChange={(event) => { setActiveID(event.target.value || null); setConnectionVersion((version) => version + 1); }}>
          <option value="" disabled>选择终端</option>
          {sessions.map((item, index) => <option key={item.id} value={item.id}>终端 {index + 1} · {item.environment}</option>)}
        </select>
        <button type="button" onClick={() => void createSession()} disabled={sessions.length >= 3}>新建</button>
        <button type="button" onClick={() => setConnectionVersion((version) => version + 1)} disabled={!canReconnect}>重连</button>
        <button type="button" onClick={() => termRef.current?.clear()} disabled={!activeID}>清屏</button>
        <button type="button" className="terminal-close" onClick={() => void closeActive()} disabled={!activeID}>关闭</button>
      </div>
      <strong className={`terminal-status ${status}`}>{status === "running" ? "运行中" : status === "connecting" ? "连接中" : status === "disconnected" ? "已断开" : status === "exited" ? "已退出" : status === "failed" ? "不可用" : "未启动"}</strong>
    </header>
    <div className="terminal-meta">{selected ? `${selected.environment} · ${project.pathDisplay}` : "没有活动终端"}</div>
    <div ref={hostRef} className="terminal-host" />
    {status === "exited" && exitCode !== null && <p className="terminal-exit-code">exit code {exitCode}</p>}
    {replayTruncated && <p className="terminal-notice">{"\u90e8\u5206\u7ec8\u7aef\u8f93\u51fa\u5df2\u8fc7\u671f\uff0c\u65e0\u6cd5\u6062\u590d"}</p>}
    {error && <p className="terminal-error" role="alert">{error}</p>}
  </section>;
}
