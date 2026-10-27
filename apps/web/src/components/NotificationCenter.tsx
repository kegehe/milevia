// 通知中心组件 — 铃铛图标 + 未读通知下拉列表

import { useEffect, useState, useRef, useCallback } from "react";
import { createPortal } from "react-dom";
import { useNavigate } from "react-router-dom";
import { useUnreadCount } from "./NotificationProvider";
import { api } from "../lib/api";
import { notificationTargetURL, type NotificationEvent } from "../lib/notifications";

function NotificationBellIcon() {
  return (
    <svg className="notification-bell-icon" viewBox="0 0 24 24" aria-hidden="true">
      <path d="M18 10a6 6 0 0 0-12 0c0 7-3 7-3 9h18c0-2-3-2-3-9" />
      <path d="M10 22h4" />
    </svg>
  );
}

function NotificationStatusIcon({ priority }: { priority: string }) {
  if (priority === "high") {
    return <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 4 3.8 19h16.4L12 4Z" /><path d="M12 9v4.5M12 16.7h.01" /></svg>;
  }
  if (priority === "normal") {
    return <svg viewBox="0 0 24 24" aria-hidden="true"><path d="m7.5 12.5 3 3 6-7" /></svg>;
  }
  return <svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="8" /><path d="M12 10v5M12 7.4h.01" /></svg>;
}

function NotificationReadAllIcon() {
  return <svg viewBox="0 0 24 24" aria-hidden="true"><path d="m4 12 3 3 5-6M11 15l2 2 7-8" /></svg>;
}

export function formatNotificationTimeAgo(dateStr: string, now: number): string {
  const seconds = Math.floor((now - new Date(dateStr).getTime()) / 1000);
  if (seconds < 60) return "刚刚";
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes} 分钟前`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours} 小时前`;
  const days = Math.floor(hours / 24);
  return `${days} 天前`;
}

export default function NotificationCenter() {
  const unreadCount = useUnreadCount();
  const navigate = useNavigate();
  const [open, setOpen] = useState(false);
  const [now, setNow] = useState(() => Date.now());
  const [notifications, setNotifications] = useState<NotificationEvent[]>([]);
  const [loading, setLoading] = useState(false);
  // 读取失败与「暂无未读通知」是两种事实：前者要保留在界面上并给出重试，
  // 不能被空列表吞掉。loadedRef 用于让后台静默刷新失败时不打扰已有列表。
  const [loadError, setLoadError] = useState("");
  const loadedRef = useRef(false);
  // 本次打开（或重试）的可见加载是否还没有成功过。后台失效刷新会顶掉它的序号，
  // 此时后台请求若失败也必须报错，否则会落回上一次的旧数据/空态。
  const visibleLoadPendingRef = useRef(false);
  const containerRef = useRef<HTMLDivElement>(null);
  const bellRef = useRef<HTMLButtonElement>(null);
  const dropdownRef = useRef<HTMLDivElement>(null);
  // 打开时的首拉与失效重拉共用同一个序号：否则先发起的首拉若后返回，会把
  // 失效事件刚拉到的较新列表覆盖回旧快照。
  const listRequestSeq = useRef(0);
  const [dropdownStyle, setDropdownStyle] = useState<React.CSSProperties>({});

  // 拉取通知列表。showLoading 为真时（打开下拉 / 点重试）失败会进入错误态；
  // 后台失效刷新失败则保留旧列表，避免无谓地清空用户正在看的内容。
  const loadList = useCallback((showLoading = false) => {
    const seq = ++listRequestSeq.current;
    if (showLoading) {
      visibleLoadPendingRef.current = true;
      setLoading(true);
      setLoadError("");
    }
    api<NotificationEvent[]>("/api/notifications")
      .then((list) => {
        if (seq !== listRequestSeq.current) return;
        visibleLoadPendingRef.current = false;
        setNotifications(list || []);
        loadedRef.current = true;
        setLoadError("");
      })
      .catch((cause) => {
        if (seq !== listRequestSeq.current) return;
        // 后台刷新失败通常保留旧列表；但本次可见加载还没成功过时不能静默，
        // 否则用户会看到上一次的旧数据（甚至「暂无未读通知」）。
        if (!showLoading && loadedRef.current && !visibleLoadPendingRef.current) return;
        // 兜底消息：Error("") 会让错误态退化成空态。
        setLoadError(cause instanceof Error && cause.message ? cause.message : "读取通知失败");
      })
      .finally(() => {
        if (seq === listRequestSeq.current) setLoading(false);
      });
  }, []);

  // 打开下拉时加载通知列表（带防竞态保护）并计算下拉框位置
  useEffect(() => {
    if (!open) return;

    const updatePosition = () => {
      // 移动端由 CSS 媒体查询全宽定位，不需要 JS 更新位置
      if (window.innerWidth <= 480) return;
      if (bellRef.current) {
        const rect = bellRef.current.getBoundingClientRect();
        setDropdownStyle({
          position: "fixed",
          top: rect.bottom + 8,
          right: window.innerWidth - rect.right,
        });
      }
    };

    updatePosition();

    let rafId = 0;
    const onReposition = () => {
      cancelAnimationFrame(rafId);
      rafId = requestAnimationFrame(updatePosition);
    };
    window.addEventListener("resize", onReposition);
    window.addEventListener("scroll", onReposition, { passive: true });

    void loadList(true);
    return () => {
      cancelAnimationFrame(rafId);
      window.removeEventListener("resize", onReposition);
      window.removeEventListener("scroll", onReposition);
    };
  }, [open, loadList]);

  // 服务端删掉通知目标后 Provider 会广播 notification:invalidated：
  // 下拉正开着时同步重拉列表（未读角标已由 Provider 自己校正）。
  useEffect(() => {
    if (!open) return;
    // 连续失效事件可能并发拉取：loadList 内的序号只接受最后一次请求的结果，
    // 避免旧响应覆盖新列表。
    const reload = () => { void loadList(); };
    window.addEventListener("notification:invalidated", reload);
    return () => {
      window.removeEventListener("notification:invalidated", reload);
    };
  }, [open, loadList]);

  // 相对时间依赖当前时刻；仅在下拉打开期间刷新，避免关闭时持续重渲染。
  useEffect(() => {
    if (!open) return;
    setNow(Date.now());
    const intervalId = window.setInterval(() => setNow(Date.now()), 1_000);
    return () => window.clearInterval(intervalId);
  }, [open]);

  // 点击外部或 Escape 键关闭
  useEffect(() => {
    if (!open) return;
    const handleClickOutside = (e: MouseEvent) => {
      const target = e.target as Node;
      // 点击铃铛按钮 — 由按钮自身的 onClick 处理切换
      if (bellRef.current?.contains(target)) return;
      // 点击下拉框内部 — 不关闭
      if (dropdownRef.current?.contains(target)) return;
      // 点击其他区域 — 关闭
      setOpen(false);
    };
    const handleKeyDown = (e: KeyboardEvent) => {
      if (e.key === "Escape") setOpen(false);
    };
    document.addEventListener("mousedown", handleClickOutside);
    document.addEventListener("keydown", handleKeyDown);
    return () => {
      document.removeEventListener("mousedown", handleClickOutside);
      document.removeEventListener("keydown", handleKeyDown);
    };
  }, [open]);

  const handleDismiss = async (id: string) => {
    try {
      await api(`/api/notifications/${id}/dismiss`, { method: "POST" });
      setNotifications((prev) => prev.filter((n) => n.id !== id));
      window.dispatchEvent(new CustomEvent("notification:dismissed", { detail: { id } }));
      // 提交后用服务端新快照作废在途的旧响应：忽略期间返回的 GET 不会把该项写回。
      void loadList();
    } catch {
      /* ignore */
    }
  };

  const handleDismissAll = async () => {
    try {
      const result = await api<{ dismissedAt: string }>("/api/notifications/dismiss-all", { method: "POST" });
      setNotifications([]);
      window.dispatchEvent(new CustomEvent("notification:dismissed-all", { detail: result }));
      // 同上：避免在途的失效刷新用提交前的列表把已清空的通知重新写回来。
      void loadList();
    } catch {
      /* ignore */
    }
  };

  const handleNavigate = (notification: NotificationEvent) => {
    navigate(notificationTargetURL(notification));
    void handleDismiss(notification.id);
    setOpen(false);
  };

  const toggleOpen = () => {
    if (!open) {
      // 打开就是一次新的读取：先进入加载态，否则首帧会先闪一下「暂无未读通知」。
      setLoading(true);
      setLoadError("");
    }
    setOpen(!open);
  };

  return (
    <div className="notification-center" ref={containerRef}>
      <button
        ref={bellRef}
        type="button"
        className={`notification-bell${open ? " is-open" : ""}`}
        title="通知"
        onClick={toggleOpen}
        aria-label={`通知${unreadCount > 0 ? ` (${unreadCount} 条未读)` : ""}`}
        aria-expanded={open}
        aria-controls="notification-dropdown"
      >
        <NotificationBellIcon />
        {unreadCount > 0 && (
          <span className="notification-badge">{unreadCount > 99 ? "99+" : unreadCount}</span>
        )}
      </button>

      {open &&
        createPortal(
          <div id="notification-dropdown" ref={dropdownRef} className="notification-dropdown" style={dropdownStyle}>
            <div className="notification-dropdown-header">
              <div className="notification-header-title-group">
                <span className="notification-header-icon"><NotificationBellIcon /></span>
                <div>
                  <span className="notification-header-title" title="未读通知保留 30 天，通知记录最多保留最新 1000 条">通知</span>
                  <small>{unreadCount > 0 ? `${unreadCount} 条待处理` : "通知中心"}</small>
                </div>
              </div>
              {!loading && !loadError && notifications.length > 0 && (
                <button type="button" className="notification-dismiss-all" title="全部标记为已读" aria-label="全部标记为已读" onClick={handleDismissAll}>
                  <NotificationReadAllIcon />
                </button>
              )}
            </div>
            <div className="notification-dropdown-list">
              {loading ? (
                <div className="notification-empty">加载中...</div>
              ) : loadError ? (
                <div className="notification-empty notification-load-failed" role="alert">
                  <span>通知列表读取失败</span>
                  <small>{loadError}</small>
                  <button type="button" className="secondary" onClick={() => void loadList(true)}>重试</button>
                </div>
              ) : notifications.length === 0 ? (
                <div className="notification-empty"><span className="notification-empty-icon"><NotificationBellIcon /></span><span>暂无未读通知</span></div>
              ) : (
                notifications.map((n) => (
                  <div key={n.id} className={`notification-item priority-${n.priority}`}>
                    <span className="notification-item-icon"><NotificationStatusIcon priority={n.priority} /></span>
                    <div className="notification-item-content">
                      <div className="notification-item-title">{n.title}</div>
                      <div className="notification-item-body">{n.body}</div>
                      <div className="notification-item-time">{formatNotificationTimeAgo(n.createdAt, now)}</div>
                      <div className="notification-item-actions">
                        <button
                          className="notification-item-view"
                          onClick={() => handleNavigate(n)}
                        >
                          查看
                        </button>
                        <button
                          className="notification-item-dismiss"
                          onClick={() => handleDismiss(n.id)}
                        >
                          忽略
                        </button>
                      </div>
                    </div>
                  </div>
                ))
              )}
            </div>
          </div>,
          document.body
        )}
    </div>
  );
}
