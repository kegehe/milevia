// 应用更新横幅 — 启动时查询是否有可用新版本，若有则在主界面右上角浮出提示，点击可在线升级。

import { useCallback, useEffect, useRef, useState } from "react";
import { invoke } from "@tauri-apps/api/core";
import { listen } from "@tauri-apps/api/event";
import { isDesktop } from "../../lib/runtime";
import "./updater.css";

type UpdateInfo = {
  currentVersion: string;
  version: string;
  notes?: string | null;
};

type UpdaterStatus = {
  appVersion: string;
  status: "checking" | "complete" | "failed";
  update: UpdateInfo | null;
  error?: string | null;
};

type ProgressEvent = {
  phase?: "checking" | "starting" | "downloading" | "installing" | "failed";
  received: number;
  total: number | null;
  error?: string;
};

type Progress = {
  phase: "checking" | "starting" | "downloading" | "installing";
  percent: number | null; // 未知总大小时为 null
  receivedMb: number;
};

export function UpdateBanner() {
  const [status, setStatus] = useState<UpdaterStatus | null>(null);
  const [dismissed, setDismissed] = useState(false);
  const [installing, setInstalling] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [progress, setProgress] = useState<Progress>({ phase: "starting", percent: null, receivedMb: 0 });
  const unlistenRef = useRef<(() => void) | null>(null);
  const errorDismissedRef = useRef(false);
  const installErrorRef = useRef(false);

  // 启动后短暂延迟，后台查询一次升级状态。
  // 轮询只是为了等到后台启动检查（prime_update_check）给出终态 complete/failed。
  // 终态之后状态不会再自发变化（Rust 侧只在 check/install 命令时改写），因此只在
  // checking 期间续约轮询；网络瞬断做有界重试，避免横幅在应用生命周期里无限自我调度。
  useEffect(() => {
    if (!isDesktop()) return;
    let cancelled = false;
    let timer: number | undefined;
    let transientRetries = 0;
    // Rust 侧启动检查自带 45s 超时；这里允许 ~60s（240×250ms）的 checking 轮询，
    // 足以等到其成功或超时转 failed，也能兜住 Rust 万一卡在 checking 的极端情况。
    let checkingPolls = 0;
    const schedule = (delay: number) => {
      if (!cancelled) timer = window.setTimeout(load, delay);
    };
    const load = () => invoke<UpdaterStatus>("get_updater_status").then((next) => {
      if (cancelled) return;
      setStatus(next);
      if (next.status === "checking") {
        errorDismissedRef.current = false;
        installErrorRef.current = false;
        setError(null);
        checkingPolls += 1;
        if (checkingPolls <= 240) schedule(250);
      } else if (next.status === "failed") {
        if (!errorDismissedRef.current) {
          setError(next.error || "更新检查失败，请稍后重试。");
        }
      } else if (!installErrorRef.current) {
        errorDismissedRef.current = false;
        setError(null);
      }
      // 终态（complete/failed）不再续期：结果已在此次响应里，无需再查。
    }).catch(() => {
      if (cancelled) return;
      setStatus(null);
      if (transientRetries < 5) {
        transientRetries += 1;
        schedule(2_000);
      }
    });
    schedule(250);
    return () => { cancelled = true; if (timer !== undefined) window.clearTimeout(timer); };
  }, []);

  // 监听下载进度事件；安装期间驱动横幅为进度条形态。
  useEffect(() => {
    if (!isDesktop()) return;
    let disposed = false;
    listen<ProgressEvent>("updater://progress", (event) => {
      if (disposed) return;
      const { phase, received, total, error } = event.payload;
      if (phase === "failed") {
        setInstalling(false);
        setProgress({ phase: "starting", percent: null, receivedMb: 0 });
        errorDismissedRef.current = false;
        installErrorRef.current = true;
        setError(error || "更新下载失败，请检查网络后重试。");
        return;
      }
      if (phase === "starting") {
        setProgress({ phase, percent: null, receivedMb: 0 });
        return;
      }
      if (phase === "checking") {
        setProgress({ phase, percent: null, receivedMb: 0 });
        return;
      }
      setProgress({
        phase: phase === "installing" ? "installing" : "downloading",
        percent: total ? (received / total) * 100 : null,
        receivedMb: received / 1024 / 1024,
      });
    }).then((unlisten) => {
      if (disposed) unlisten();
      else unlistenRef.current = unlisten;
    });
    return () => {
      disposed = true;
      unlistenRef.current?.();
    };
  }, []);

  const install = useCallback(async () => {
    setInstalling(true);
    setDismissed(false); // 保持横幅显示，切换为进度形态
    setProgress({ phase: "checking", percent: null, receivedMb: 0 });
    try {
      const result = await invoke<{ installed: boolean }>("install_update");
      if (!result.installed) {
        setInstalling(false);
        setProgress({ phase: "starting", percent: null, receivedMb: 0 });
        errorDismissedRef.current = false;
        installErrorRef.current = false;
        setError(null);
        setStatus((current) => current ? { ...current, status: "complete", update: null, error: null } : current);
      }
      // 成功后应用重启，一般不会走到这里。
    } catch (error) {
      setInstalling(false);
      errorDismissedRef.current = false;
      installErrorRef.current = true;
      const message = error instanceof Error ? error.message : "更新安装失败，请稍后重试。";
      setError(message);
      console.error("[updater] install failed", error);
    }
  }, []);

  if (!isDesktop()) return null;
  const update = status?.update;
  if (error) return <div className="update-banner" role="alert"><div className="update-banner-text"><strong>更新失败</strong><span>{error}</span></div><button className="update-banner-dismiss" onClick={() => { errorDismissedRef.current = true; installErrorRef.current = false; setError(null); }} aria-label="关闭">✕</button></div>;
  if (!update || dismissed || status?.status !== "complete") return null;

  return (
    <div className="update-banner" role="status">
      {!installing ? (
        <>
          <div className="update-banner-text">
            <strong>发现新版本 v{update.version}</strong>
            <span>
              当前 v{status.appVersion}
              {update.notes ? ` · ${update.notes.trim().slice(0, 60)}` : ""}
            </span>
          </div>
          <button className="update-banner-action" onClick={install}>
            立即升级
          </button>
          <button
            className="update-banner-dismiss"
            onClick={() => setDismissed(true)}
            aria-label="稍后提醒"
          >
            ✕
          </button>
        </>
      ) : (
        <div className="update-banner-downloading">
          <span>
            {progress.phase === "checking"
              ? "正在检查更新..."
              : progress.phase === "installing"
                ? "正在安装更新..."
                : "正在下载更新..."}
          </span>
          <div className="update-banner-track" aria-hidden="true">
            <div
              className="update-banner-bar"
              style={{ width: progress.percent == null ? "100%" : `${progress.percent}%` }}
            />
          </div>
          <span className="update-banner-mb">{progress.receivedMb.toFixed(1)} MB</span>
        </div>
      )}
    </div>
  );
}
