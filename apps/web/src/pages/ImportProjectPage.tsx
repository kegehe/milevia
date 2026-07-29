// 导入项目页 — 从 App.tsx Importer 提取

import { useEffect, useState, useCallback, useRef } from "react";
import { useNavigate } from "react-router-dom";
import { useProjectContext } from "../stores/useProjectStore";
import type { Project, Directory } from "../lib/types";

function displayPath(path: string): string {
  const matched = /^\/mnt\/([a-zA-Z])(?:\/(.*))?$/.exec(path);
  if (!matched) return path;
  const [, drive, rest = ""] = matched;
  return `${drive.toUpperCase()}:\\${rest.replaceAll("/", "\\")}`;
}

export default function ImportProjectPage() {
  const { api } = useProjectContext();
  const navigate = useNavigate();
  const [path, setPath] = useState("");
  const [parent, setParent] = useState("");
  const [dirs, setDirs] = useState<Directory[]>([]);
  const [result, setResult] = useState<any>(null);
  const [busy, setBusy] = useState(false);
  const [roots, setRoots] = useState<{ name: string; path: string; label?: string; runnerId?: string }[]>([]);
  const [activeRoot, setActiveRoot] = useState<string | null>(null);
  const [activeRunner, setActiveRunner] = useState<string>("");
  const [localError, setLocalError] = useState<string | null>(null);
  const [loadingDirectory, setLoadingDirectory] = useState(false);
  const [directoryReady, setDirectoryReady] = useState(false);
  const mountedRef = useRef(true);
  const browseRequestRef = useRef(0);
  const validationRequestRef = useRef(0);
  const createRequestRef = useRef(0);
  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
      browseRequestRef.current++;
      validationRequestRef.current++;
      createRequestRef.current++;
    };
  }, []);
  const [loadingRoots, setLoadingRoots] = useState(true);

  useEffect(() => {
    let cancelled = false;
    api<{ roots?: { name: string; path: string; label?: string }[]; root?: string; id?: string }[]>("/api/runners")
      .then((runners) => {
        if (cancelled) return;
        const rootsFromRunner: { name: string; path: string; label?: string; runnerId?: string }[] = [];
        for (const runner of runners) {
          if (runner.roots) {
            for (const r of runner.roots) {
              rootsFromRunner.push({ ...r, runnerId: runner.id });
            }
          } else if (runner.root) {
            rootsFromRunner.push({ name: "WSL Home", path: runner.root, runnerId: runner.id });
          }
        }
        setRoots(rootsFromRunner);
      })
      .catch(() => { if (!cancelled) setLocalError("无法获取可用的根路径"); })
      .finally(() => { if (!cancelled) setLoadingRoots(false); });
    return () => { cancelled = true; };
  }, [api]);

  const browse = useCallback(async (target = "", runnerID = activeRunner) => {
    const requestID = ++browseRequestRef.current;
    validationRequestRef.current++;
    setLoadingDirectory(true);
    setBusy(false);
    setDirectoryReady(false);
    setLocalError(null);
    setPath(target);
    setParent("");
    setDirs([]);
    setResult(null);
    try {
      const query = target ? `?path=${encodeURIComponent(target)}` : "";
      const runnerParam = runnerID ? `${query ? "&" : "?"}runner=${encodeURIComponent(runnerID)}` : "";
      const data = await api<{ path: string; parent: string; directories: Directory[] }>(`/api/directories${query}${runnerParam}`);
      if (!mountedRef.current || requestID !== browseRequestRef.current) return;
      setPath(data.path); setParent(data.parent); setDirs(data.directories); setResult(null); setLocalError(null); setDirectoryReady(true);
    } catch (cause) {
      if (mountedRef.current && requestID === browseRequestRef.current) setLocalError(cause instanceof Error ? cause.message : "无法浏览目录");
    } finally {
      if (mountedRef.current && requestID === browseRequestRef.current) setLoadingDirectory(false);
    }
  }, [api, activeRunner]);

  const selectRoot = (rootPath: string, runnerId?: string) => {
    setActiveRoot(rootPath);
    setActiveRunner(runnerId || "");
    setLocalError(null);
    setResult(null);
    // State updates are asynchronous. Pass the selected runner explicitly so
    // the first directory request is authorized against that runner's roots.
    void browse(rootPath, runnerId || "");
  };
  const returnToRootSelection = () => {
    browseRequestRef.current++;
    validationRequestRef.current++;
    setActiveRoot(null);
    setActiveRunner("");
    setLocalError(null);
    setLoadingDirectory(false);
    setDirectoryReady(false);
    setResult(null);
  };
  const validate = async () => {
    const requestID = ++validationRequestRef.current;
    setBusy(true);
    try {
      const validationResult = await api("/api/projects/validate", { method: "POST", body: JSON.stringify({ path, runner: activeRunner }) });
      if (mountedRef.current && requestID === validationRequestRef.current) setResult(validationResult);
    }
    catch (cause) {
      if (mountedRef.current && requestID === validationRequestRef.current) setLocalError(cause instanceof Error ? cause.message : "无法校验目录");
    }
    finally {
      if (mountedRef.current && requestID === validationRequestRef.current) setBusy(false);
    }
  };
  const create = async () => {
    const requestID = ++createRequestRef.current;
    setBusy(true);
    try {
      const created = await api<Project>("/api/projects", { method: "POST", body: JSON.stringify({ path, name: result?.name, runner: activeRunner }) });
      if (mountedRef.current && requestID === createRequestRef.current) navigate(`/projects/${created.id}`);
    }
    catch (cause) { if (mountedRef.current && requestID === createRequestRef.current) setLocalError(cause instanceof Error ? cause.message : "无法加载项目"); }
    finally { if (mountedRef.current && requestID === createRequestRef.current) setBusy(false); }
  };

  if (activeRoot === null && roots.length > 0 && !loadingRoots) {
    return (
      <main className="app-shell">
        <div className="backdrop" role="dialog" aria-modal="true">
          <section className="modal">
            <header>
              <div><label>LOAD PROJECT</label><h2>选择环境</h2></div>
              <button title="关闭" onClick={() => navigate("/")}>x</button>
            </header>
            <p className="permission-confirmation" style={{ marginBottom: "1rem" }}>请选择项目所在的目录位置：</p>
            {localError && <p className="error" role="alert">{localError}</p>}
            <div className="dirs">
              {roots.map((root) => (
                <button className="environment-option" key={root.path} onClick={() => selectRoot(root.path, root.runnerId)}>
                  <b className="root-option"><i>{root.label === "windows" ? "🪟" : root.label === "remote-linux" ? "🖥️" : "🐧"}</i><span>{root.name}</span></b>
                  <small>{displayPath(root.path)}</small>
                </button>
              ))}
            </div>
            <footer><button className="secondary" onClick={() => navigate("/")}>取消</button></footer>
          </section>
        </div>
      </main>
    );
  }

  if (activeRoot === null && loadingRoots) {
    return <main className="app-shell">
      <div className="backdrop" role="dialog" aria-modal="true"><section className="modal"><header><div><label>LOAD PROJECT</label><h2>加载项目</h2></div><button title="关闭" onClick={() => navigate("/")}>x</button></header><p className="permission-confirmation">正在检测可用的环境…</p>{localError && <p className="error" role="alert">{localError}</p>}<footer><button className="secondary" onClick={() => navigate("/")}>取消</button></footer></section></div>
    </main>;
  }

  if (activeRoot === null && roots.length === 0) {
    return <main className="app-shell">
      <div className="backdrop" role="dialog" aria-modal="true"><section className="modal"><header><div><label>LOAD PROJECT</label><h2>加载项目</h2></div><button title="关闭" onClick={() => navigate("/")}>x</button></header><p className="permission-confirmation">{localError || "未检测到可用的运行环境，请确认 WSL 配置正确。"}</p><footer><button className="secondary" onClick={() => navigate("/")}>取消</button></footer></section></div>
    </main>;
  }

  return <main className="app-shell">
    <div className="backdrop" role="dialog" aria-modal="true"><section className="modal"><header><div><label>LOAD PROJECT</label><h2>加载项目</h2></div><button title="关闭" disabled={busy} onClick={() => navigate("/")}>x</button></header><div className="path"><button title="返回根选择" disabled={busy} onClick={returnToRootSelection}>⟵ 环境</button><button title="上级目录" disabled={busy || loadingDirectory} onClick={() => void browse(parent)}>Up</button><code>{displayPath(path)}</code><button className="secondary" disabled={busy || loadingDirectory || !directoryReady} onClick={() => void validate()}>校验目录</button></div>{localError && <p className="error" role="alert">{localError}</p>}<div className="dirs">{dirs.map((dir) => <button key={dir.path} disabled={busy || loadingDirectory} onClick={() => void browse(dir.path)}><b>{dir.name}</b><small>{displayPath(dir.path)}</small></button>)}</div>{result && <div className={result.agentReady ? "valid" : "invalid"}><b>{result.name}</b><span>Git: {result.gitReady ? result.gitBranch : "未检测到 Git 仓库（仍可加载）"}</span><span>Claude Code: {result.claudeReady ? "可用" : "不可用"}</span><span>Codex: {result.codexReady ? "可用" : "不可用"}</span>{result.performance === "cross-filesystem" && <span>⚠️ 跨文件系统 — 读写性能可能较慢</span>}</div>}<footer><button className="secondary" disabled={busy} onClick={() => navigate("/")}>取消</button><button className="primary" disabled={!result?.agentReady || busy || loadingDirectory || !directoryReady} onClick={() => void create()}>{busy ? "处理中" : "确认加载"}</button></footer></section></div>
  </main>;
}
