// 导入项目页 — 从 App.tsx Importer 提取

import { useEffect, useState, useCallback, useRef } from "react";
import { useNavigate } from "react-router-dom";
import { useProjectContext } from "../stores/useProjectStore";
import type { Project, Directory } from "../lib/types";
import DashboardPage from "./DashboardPage";

function displayPath(path: string): string {
  const matched = /^\/mnt\/([a-zA-Z])(?:\/(.*))?$/.exec(path);
  if (!matched) return path;
  const [, drive, rest = ""] = matched;
  return `${drive.toUpperCase()}:\\${rest.replaceAll("/", "\\")}`;
}

function EnvironmentIcon({ environment }: { environment?: string }) {
  if (environment === "windows") {
    return <svg className="root-option-icon windows" viewBox="0 0 24 24" aria-hidden="true"><path d="M3 5.5 10.5 4v7H3v-5.5ZM13 3.5 21 2v9h-8v-7.5ZM3 13h7.5v7L3 18.5V13ZM13 13h8v9l-8-1.5V13Z" /></svg>;
  }
  if (environment === "remote-linux") {
    return <svg className="root-option-icon remote" viewBox="0 0 24 24" aria-hidden="true"><rect x="4" y="4" width="16" height="6" rx="1.2" /><rect x="4" y="14" width="16" height="6" rx="1.2" /><path d="M8 7h.01M8 17h.01M12 7h5M12 17h5" /></svg>;
  }
  return <svg className="root-option-icon local" viewBox="0 0 24 24" aria-hidden="true"><rect x="3.5" y="4" width="17" height="16" rx="2" /><path d="m7.5 9 2.5 2.5L7.5 14M12.5 14h4" /></svg>;
}

function ImportWorkspaceIcon() {
  return <svg className="import-workspace-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M3.5 7.5h6l1.8 2h9.2v8.2a2 2 0 0 1-2 2h-13a2 2 0 0 1-2-2V7.5Z" /><path d="M15.5 12.5v5M13 15h5" /></svg>;
}

function ImportBackIcon() {
  return <svg className="import-control-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M14.5 5.5 8 12l6.5 6.5M8.5 12h8" /></svg>;
}

function ImportCloseIcon() {
  return <svg className="import-control-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="m7 7 10 10M17 7 7 17" /></svg>;
}

function ImportUpIcon() {
  return <svg className="import-control-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M12 18V6M7.5 10.5 12 6l4.5 4.5" /></svg>;
}

function ImportFolderIcon() {
  return <svg className="import-folder-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M3.5 7.5h6l1.8 2h9.2v8.2a2 2 0 0 1-2 2h-13a2 2 0 0 1-2-2V7.5Z" /></svg>;
}

function ImportCheckIcon() {
  return <svg className="import-check-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="m6.5 12 3.4 3.4 7.6-7.6" /></svg>;
}

function ImportNewFolderIcon() {
  return <svg className="import-control-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M3.5 7.5h6l1.8 2h9.2v8.2a2 2 0 0 1-2 2h-13a2 2 0 0 1-2-2V7.5Z" /><path d="M12 12v5M9.5 14.5h5" /></svg>;
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
  const [creatingDirectory, setCreatingDirectory] = useState(false);
  const [newFolderName, setNewFolderName] = useState("");
  const [showNewFolderInput, setShowNewFolderInput] = useState(false);
  const newFolderInputRef = useRef<HTMLInputElement>(null);
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

  useEffect(() => {
    if (showNewFolderInput && newFolderInputRef.current) newFolderInputRef.current.focus();
  }, [showNewFolderInput]);
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
    setShowNewFolderInput(false);
    setNewFolderName("");
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

  const createDirectory = useCallback(async () => {
    const trimmedName = newFolderName.trim();
    if (!trimmedName) return;
    if (trimmedName.includes("/") || trimmedName.includes("\\") || trimmedName === ".." || trimmedName.startsWith(".")) {
      setLocalError("文件夹名称不能包含 /、\\、.. 或以 . 开头");
      return;
    }
    setCreatingDirectory(true);
    setLocalError(null);
    try {
      const fullPath = path + "/" + trimmedName;
      await api("/api/directories/mkdir", { method: "POST", body: JSON.stringify({ path: fullPath, runner: activeRunner }) });
      if (!mountedRef.current) return;
      setShowNewFolderInput(false);
      setNewFolderName("");
      void browse(fullPath);
    } catch (cause) {
      if (mountedRef.current) setLocalError(cause instanceof Error ? cause.message : "无法创建文件夹");
    } finally {
      if (mountedRef.current) setCreatingDirectory(false);
    }
  }, [api, activeRunner, path, newFolderName, browse]);

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
    setShowNewFolderInput(false);
    setNewFolderName("");
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

  const choosingRoot = activeRoot === null;
  const selectedRoot = roots.find((root) => root.path === activeRoot);

  return <>
    <DashboardPage />
    <div className="backdrop import-project-backdrop" role="dialog" aria-modal="true" aria-labelledby="import-project-title">
      <section className="modal import-project-modal">
        <header className="import-project-header"><div className="import-project-title"><h1 id="import-project-title">加载项目</h1></div><button className="import-project-close" type="button" title="关闭" aria-label="关闭" disabled={busy} onClick={() => navigate("/")}><ImportCloseIcon /></button></header>
        <section className="import-project-shell">
      <div className="import-project-steps" aria-label="加载步骤"><span className={choosingRoot ? "active" : "done"}><i>1</i>选择环境</span><span className={!choosingRoot ? "active" : ""}><i>2</i>浏览目录</span><span className={result ? "active" : ""}><i>3</i>校验加载</span></div>
      {choosingRoot ? <section className="import-surface import-environment-surface"><header><div><span className="import-surface-mark"><ImportWorkspaceIcon /></span><div><h2>{loadingRoots ? "正在检测环境" : roots.length ? "选择项目环境" : "未找到环境"}</h2><p>{loadingRoots ? "正在读取可用运行环境" : roots.length ? "从可用环境开始浏览项目目录" : "请检查本地或远程运行环境的配置"}</p></div></div><span className="import-surface-count">{loadingRoots ? "检测中" : `${roots.length} 个环境`}</span></header>{localError && <p className="error" role="alert">{localError}</p>}{loadingRoots ? <div className="import-loading-state"><span></span><p>正在检测可用环境…</p></div> : roots.length ? <div className="import-root-grid">{roots.map((root) => <button className="environment-option" type="button" key={root.path} onClick={() => selectRoot(root.path, root.runnerId)}><b className="root-option"><EnvironmentIcon environment={root.label} /><span>{root.name}</span></b><small>{displayPath(root.path)}</small><i>浏览</i></button>)}</div> : <div className="import-empty-state"><p>{localError || "未检测到可用的运行环境。"}</p></div>}<footer><button className="secondary" type="button" onClick={() => navigate("/")}>返回总览</button></footer></section> : <section className="import-surface import-browser-surface"><header><div><span className="import-surface-mark"><EnvironmentIcon environment={selectedRoot?.label} /></span><div><h2>{selectedRoot?.name || "项目目录"}</h2><p>{loadingDirectory ? "正在读取目录" : `${dirs.length} 个可选目录`}</p></div></div><button className="import-change-environment" type="button" disabled={busy} onClick={returnToRootSelection}>切换环境</button></header><div className="import-browser-toolbar"><div className="import-path-controls"><button className="import-icon-button" type="button" title="返回环境选择" aria-label="返回环境选择" disabled={busy} onClick={returnToRootSelection}><ImportBackIcon /></button><button className="import-icon-button" type="button" title="上级目录" aria-label="上级目录" disabled={busy || loadingDirectory || !parent} onClick={() => void browse(parent)}><ImportUpIcon /></button><code>{displayPath(path)}</code></div><div className="import-new-folder-controls">{showNewFolderInput ? <div className="import-new-folder-input-row"><input ref={newFolderInputRef} className="import-new-folder-input" type="text" value={newFolderName} onChange={(e) => setNewFolderName(e.target.value)} onKeyDown={(e) => { if (e.key === "Enter") void createDirectory(); if (e.key === "Escape") { setShowNewFolderInput(false); setNewFolderName(""); } }} placeholder="文件夹名称" disabled={creatingDirectory || busy} /><button className="import-icon-button" type="button" title="确认创建" aria-label="确认创建" disabled={creatingDirectory || !newFolderName.trim()} onClick={() => void createDirectory()}><ImportCheckIcon /></button><button className="import-icon-button" type="button" title="取消" aria-label="取消" disabled={creatingDirectory} onClick={() => { setShowNewFolderInput(false); setNewFolderName(""); }}><ImportCloseIcon /></button></div> : <button className="import-new-folder-button" type="button" title="新建文件夹" aria-label="新建文件夹" disabled={busy || loadingDirectory || creatingDirectory} onClick={() => { setShowNewFolderInput(true); setNewFolderName(""); }}><ImportNewFolderIcon />新建</button>}</div><button className="primary import-validate-button" disabled={busy || loadingDirectory || creatingDirectory || !directoryReady} onClick={() => void validate()}><ImportCheckIcon />{busy ? "校验中" : "校验目录"}</button></div>{localError && <p className="error" role="alert">{localError}</p>}<div className="import-directory-head"><span>目录</span>{loadingDirectory ? <small className="is-loading">读取中</small> : <small>{dirs.length} 项</small>}</div><div className="dirs import-directory-list">{loadingDirectory ? <div className="import-loading-state"><span></span><p>正在读取目录…</p></div> : dirs.length ? dirs.map((dir) => <button type="button" key={dir.path} disabled={busy || loadingDirectory || creatingDirectory} onClick={() => void browse(dir.path)}><ImportFolderIcon /><span><b>{dir.name}</b><small>{displayPath(dir.path)}</small></span><i>›</i></button>) : <div className="import-empty-state"><p>当前目录没有可浏览的子目录。</p></div>}</div>{result ? <section className={`import-validation ${result.agentReady ? "ready" : "unavailable"}`}><header><div><span className="import-validation-icon"><ImportCheckIcon /></span><div><h3>{result.name}</h3><p>{result.agentReady ? "已通过加载校验" : "当前环境无法启动可用的 AI 工具"}</p></div></div><span>{result.agentReady ? "可加载" : "不可用"}</span></header><div><span>Git <b>{result.gitReady ? result.gitBranch : "非 Git 仓库"}</b></span><span>Claude Code <b>{result.claudeReady ? "可用" : "不可用"}</b></span><span>Codex <b>{result.codexReady ? "可用" : "不可用"}</b></span>{result.performance === "cross-filesystem" && <span className="import-performance-note">跨文件系统，读写性能可能较慢</span>}</div></section> : <div className="import-validation-placeholder"><ImportCheckIcon /><span>校验目录后显示项目运行状态</span></div>}<footer><button className="secondary" type="button" disabled={busy} onClick={() => navigate("/")}>取消</button><button className="primary import-load-button" disabled={!result?.agentReady || busy || loadingDirectory || !directoryReady} onClick={() => void create()}>{busy ? "加载中" : "确认加载"}</button></footer></section>}
        </section>
      </section>
    </div>
  </>;
}
