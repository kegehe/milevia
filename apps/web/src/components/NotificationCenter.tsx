// 通知中心组件 — 铃铛图标 + 未读通知下拉列表

import { useEffect, useState, useRef } from "react";
import { createPortal } from "react-dom";
import { useNavigate } from "react-router-dom";
import { useUnreadCount } from "./NotificationProvider";
import { api } from "../lib/api";
import { notificationConversationURL, type NotificationEvent } from "../lib/notifications";

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

export default function NotificationCenter() {
  const unreadCount = useUnreadCount();
  const navigate = useNavigate();
  const [open, setOpen] = useState(false);
  const [notifications, setNotifications] = useState<NotificationEvent[]>([]);
  const [loading, setLoading] = useState(false);
  const containerRef = useRef<HTMLDivElement>(null);
  const bellRef = useRef<HTMLButtonElement>(null);
  const dropdownRef = useRef<HTMLDivElement>(null);
  const [dropdownStyle, setDropdownStyle] = useState<React.CSSProperties>({});

  // 打开下拉时加载通知列表（带防竞态保护）并计算下拉框位置
  useEffect(() => {
    if (!open) return;
    let stale = false;
    setLoading(true);

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

    api<NotificationEvent[]>("/api/notifications")
      .then((list) => {
        if (!stale) setNotifications(list || []);
      })
      .catch(() => {
        /* ignore */
      })
      .finally(() => {
        if (!stale) setLoading(false);
      });
    return () => {
      stale = true;
      cancelAnimationFrame(rafId);
      window.removeEventListener("resize", onReposition);
      window.removeEventListener("scroll", onReposition);
    };
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
    } catch {
      /* ignore */
    }
  };

  const handleDismissAll = async () => {
    try {
      const result = await api<{ dismissedAt: string }>("/api/notifications/dismiss-all", { method: "POST" });
      setNotifications([]);
      window.dispatchEvent(new CustomEvent("notification:dismissed-all", { detail: result }));
    } catch {
      /* ignore */
    }
  };

  const handleNavigate = (notification: NotificationEvent) => {
    navigate(notificationConversationURL(notification));
    void handleDismiss(notification.id);
    setOpen(false);
  };

  const timeAgo = (dateStr: string) => {
    const seconds = Math.floor((Date.now() - new Date(dateStr).getTime()) / 1000);
    if (seconds < 60) return "刚刚";
    const minutes = Math.floor(seconds / 60);
    if (minutes < 60) return `${minutes} 分钟前`;
    const hours = Math.floor(minutes / 60);
    if (hours < 24) return `${hours} 小时前`;
    const days = Math.floor(hours / 24);
    return `${days} 天前`;
  };

  return (
    <div className="notification-center" ref={containerRef}>
      <button
        ref={bellRef}
        type="button"
        className={`notification-bell${open ? " is-open" : ""}`}
        title="通知"
        onClick={() => setOpen(!open)}
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
                  <span className="notification-header-title">通知</span>
                  <small>{unreadCount > 0 ? `${unreadCount} 条待处理` : "通知中心"}</small>
                </div>
              </div>
              {notifications.length > 0 && (
                <button type="button" className="notification-dismiss-all" title="全部标记为已读" aria-label="全部标记为已读" onClick={handleDismissAll}>
                  <NotificationReadAllIcon />
                </button>
              )}
            </div>
            <div className="notification-dropdown-list">
              {loading ? (
                <div className="notification-empty">加载中...</div>
              ) : notifications.length === 0 ? (
                <div className="notification-empty"><span className="notification-empty-icon"><NotificationBellIcon /></span><span>暂无未读通知</span></div>
              ) : (
                notifications.map((n) => (
                  <div key={n.id} className={`notification-item priority-${n.priority}`}>
                    <span className="notification-item-icon"><NotificationStatusIcon priority={n.priority} /></span>
                    <div className="notification-item-content">
                      <div className="notification-item-title">{n.title}</div>
                      <div className="notification-item-body">{n.body}</div>
                      <div className="notification-item-time">{timeAgo(n.createdAt)}</div>
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
