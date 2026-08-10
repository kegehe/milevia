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
  update: UpdateInfo | null;
};

type ProgressEvent = {
  received: number;
  total: number | null;
};

type Progress = {
  percent: number | null; // 未知总大小时为 null
  receivedMb: number;
};

export function UpdateBanner() {
  const [status, setStatus] = useState<UpdaterStatus | null>(null);
  const [dismissed, setDismissed] = useState(false);
  const [installing, setInstalling] = useState(false);
  const [progress, setProgress] = useState<Progress>({ percent: null, receivedMb: 0 });
  const unlistenRef = useRef<(() => void) | null>(null);

  // 启动后短暂延迟，后台查询一次升级状态。
  useEffect(() => {
    if (!isDesktop()) return;
    const timer = window.setTimeout(() => {
      invoke<UpdaterStatus>("get_updater_status")
        .then(setStatus)
        .catch(() => setStatus(null));
    }, 800);
    return () => window.clearTimeout(timer);
  }, []);

  // 监听下载进度事件；安装期间驱动横幅为进度条形态。
  useEffect(() => {
    if (!isDesktop()) return;
    let disposed = false;
    listen<ProgressEvent>("updater://progress", (event) => {
      if (disposed) return;
      const { received, total } = event.payload;
      setProgress({
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
    try {
      await invoke("install_update");
      // 成功后应用重启，一般不会走到这里。
    } catch (error) {
      setInstalling(false);
      console.error("[updater] install failed", error);
    }
  }, []);

  if (!isDesktop()) return null;
  const update = status?.update;
  if (!update || dismissed) return null;

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
          <span>下载更新中…</span>
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
