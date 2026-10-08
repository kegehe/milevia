// 应用更新横幅 — 启动时查询升级状态。发现新版本后由 Rust 在后台静默预下载，
// 下载期间这里什么都不显示；包备好（已验签）才浮出提示，点击即安装。

import { useCallback, useEffect, useRef, useState } from "react";
import { invoke } from "@tauri-apps/api/core";
import { listen } from "@tauri-apps/api/event";
import { isDesktop } from "../../lib/runtime";
import { bannerUpdate, downloadPhase, nextStatusPollMs, readyUpdate, CHECK_POLL_MS, IDLE_POLL_MS, type UpdaterStatus } from "./update-view";
import "./updater.css";

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

/** 取状态失败（IPC 出错）时的重试间隔；连试几次都不行再降到 IDLE_POLL_MS。 */
const IPC_RETRY_MS = 2_000;

export function UpdateBanner() {
  const [status, setStatus] = useState<UpdaterStatus | null>(null);
  // 记录"用户关掉的是哪个版本的提示"而不是一个布尔值：关掉 0.1.8 的提示不该
  // 连 0.1.9 的提示一起吞掉。
  const [dismissedVersion, setDismissedVersion] = useState<string | null>(null);
  const [installing, setInstalling] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [progress, setProgress] = useState<Progress>({ phase: "starting", percent: null, receivedMb: 0 });
  const unlistenRef = useRef<(() => void) | null>(null);
  const errorDismissedRef = useRef(false);
  const installErrorRef = useRef(false);

  // 启动后短暂延迟，后台查询一次升级状态，之后按当前相位决定续约节奏：
  // - checking：后台启动检查（prime_update_check）自带 45s 超时，这里允许 ~60s
  //   等到它落成 complete/failed，也兜住 Rust 万一卡在 checking 的极端情况；
  // - downloading：后台静默预下载，等它落成 ready/failed 即可，不需要更密；
  // - 其余情况按 IDLE_POLL_MS 慢节奏续约，**不彻底停** —— 状态不只由启动检查改写，
  //   打开托盘面板、点设置页的"检查更新"都会重查一次，而"后台下好之后提示用户"
  //   正是靠这里才能浮出来。节奏判定见 update-view 的 nextStatusPollMs（有单测）。
  // 网络瞬断做有界重试，避免横幅在应用生命周期里密集自我调度。
  useEffect(() => {
    if (!isDesktop()) return;
    let cancelled = false;
    let timer: number | undefined;
    let transientRetries = 0;
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
      } else {
        // 一次检查已落定：预算清零，下次检查重新从零算。不清的话，开过几十次托盘
        // 之后累计值会顶到上限，横幅在"检查中"就从 250ms 退化成 30s 一问。
        checkingPolls = 0;
        if (next.status === "failed" && !readyUpdate(next)) {
          if (!errorDismissedRef.current) {
            setError(next.error || "更新检查失败，请稍后重试。");
          }
        } else if (!installErrorRef.current) {
          // 这一支同时兜住两件事：检查成功后清掉旧的检查错误，以及**手上已有备好的包
          // 时不让"最近一次检查失败"盖住"可以直接装"**（失败原因在设置页里如实写着）。
          // 安装失败的错误（installErrorRef）不受影响，用户必须知道它没成。
          errorDismissedRef.current = false;
          setError(null);
        }
      }
      schedule(nextStatusPollMs(next, checkingPolls));
    }).catch(() => {
      if (cancelled) return;
      setStatus(null);
      // 取不到状态时先密一点重试；连试几次都不行就降到慢节奏 —— 别密集重试，
      // 但也别彻底放弃（IPC 恢复之后横幅要能自己回来）。
      transientRetries += 1;
      schedule(transientRetries <= 5 ? IPC_RETRY_MS : IDLE_POLL_MS);
    });
    schedule(CHECK_POLL_MS);
    return () => { cancelled = true; if (timer !== undefined) window.clearTimeout(timer); };
  }, []);

  // 监听下载进度事件；安装期间驱动横幅为进度条形态。
  // 只有"用户点出来的安装"会走到这里：后台预下载不发这个事件。
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

  const install = useCallback(async (ready: boolean) => {
    setInstalling(true);
    setDismissedVersion(null); // 保持横幅显示，切换为进度形态
    // 就绪时安装只剩本地解包 + 拉起安装器，照实显示"正在安装"，别让用户以为又在联网。
    setProgress({ phase: ready ? "installing" : "checking", percent: null, receivedMb: 0 });
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
  if (error) return <div className="update-banner" role="alert"><div className="update-banner-text"><strong>更新失败</strong><span>{error}</span></div><button className="update-banner-dismiss" onClick={() => { errorDismissedRef.current = true; installErrorRef.current = false; setError(null); }} aria-label="关闭">✕</button></div>;

  const view = bannerUpdate(status, dismissedVersion);
  // 后台静默下载中：view 是 none，此时必须什么都不显示 —— 这是"静默"的落点。
  if (view.kind === "none" && !installing) return null;
  if (installing) return renderDownloading(progress);

  const update = view.kind === "none" ? null : view.update;
  if (!update) return null;
  const ready = view.kind === "ready";
  const failed = downloadPhase(status) === "failed";

  return (
    <div className="update-banner" role="status">
      <div className="update-banner-text">
        <strong>{ready ? `新版本 v${update.version} 已就绪` : `发现新版本 v${update.version}`}</strong>
        <span>
          当前 v{status?.appVersion}
          {ready
            ? " · 更新包已下载完成，点击安装后应用会自动重启"
            : failed
              ? " · 后台下载未完成，点击后重新下载并安装"
              : update.notes ? ` · ${update.notes.trim().slice(0, 60)}` : ""}
        </span>
      </div>
      <button className="update-banner-action" onClick={() => void install(ready)}>
        {ready ? "立即安装" : "立即升级"}
      </button>
      <button
        className="update-banner-dismiss"
        onClick={() => setDismissedVersion(update.version)}
        aria-label="稍后提醒"
      >
        ✕
      </button>
    </div>
  );
}

/** 安装进行中：进度条形态。 */
function renderDownloading(progress: Progress) {
  return (
    <div className="update-banner" role="status">
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
    </div>
  );
}
