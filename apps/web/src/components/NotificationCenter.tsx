// 通知中心组件 — 铃铛图标 + 未读通知下拉列表

import { useEffect, useState, useRef } from "react";
import { useNavigate } from "react-router-dom";
import { useUnreadCount } from "./NotificationProvider";
import { api } from "../lib/api";
import type { NotificationEvent } from "../lib/notifications";

export default function NotificationCenter() {
  const unreadCount = useUnreadCount();
  const navigate = useNavigate();
  const [open, setOpen] = useState(false);
  const [notifications, setNotifications] = useState<NotificationEvent[]>([]);
  const [loading, setLoading] = useState(false);
  const dropdownRef = useRef<HTMLDivElement>(null);

  // 打开下拉时加载通知列表（带防竞态保护）
  useEffect(() => {
    if (!open) return;
    let stale = false;
    setLoading(true);
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
    };
  }, [open]);

  // 点击外部或 Escape 键关闭
  useEffect(() => {
    if (!open) return;
    const handleClickOutside = (e: MouseEvent) => {
      if (dropdownRef.current && !dropdownRef.current.contains(e.target as Node)) {
        setOpen(false);
      }
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

  const handleNavigate = (actionUrl: string, id: string) => {
    navigate(actionUrl);
    void handleDismiss(id);
    setOpen(false);
  };

  const priorityIcon = (priority: string) => {
    if (priority === "high") return "⚠️";
    if (priority === "normal") return "✅";
    return "ℹ️";
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
    <div className="notification-center" ref={dropdownRef}>
      <button
        className="notification-bell"
        title="通知"
        onClick={() => setOpen(!open)}
        aria-label={`通知${unreadCount > 0 ? ` (${unreadCount} 条未读)` : ""}`}
      >
        🔔
        {unreadCount > 0 && (
          <span className="notification-badge">{unreadCount > 99 ? "99+" : unreadCount}</span>
        )}
      </button>

      {open && (
        <div className="notification-dropdown">
          <div className="notification-dropdown-header">
            <span>通知</span>
            {notifications.length > 0 && (
              <button className="notification-dismiss-all" onClick={handleDismissAll}>
                全部已读
              </button>
            )}
          </div>
          <div className="notification-dropdown-list">
            {loading ? (
              <div className="notification-empty">加载中...</div>
            ) : notifications.length === 0 ? (
              <div className="notification-empty">暂无未读通知</div>
            ) : (
              notifications.map((n) => (
                <div key={n.id} className={`notification-item priority-${n.priority}`}>
                  <span className="notification-item-icon">{priorityIcon(n.priority)}</span>
                  <div className="notification-item-content">
                    <div className="notification-item-title">{n.title}</div>
                    <div className="notification-item-body">{n.body}</div>
                    <div className="notification-item-time">{timeAgo(n.createdAt)}</div>
                    <div className="notification-item-actions">
                      <button
                        className="notification-item-view"
                        onClick={() => handleNavigate(n.actionUrl, n.id)}
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
        </div>
      )}
    </div>
  );
}
