package app

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// NotificationEvent 是推送给前端的通知事件。
type NotificationEvent struct {
	ID             string    `json:"id"`
	Type           string    `json:"type"`
	ProjectID      string    `json:"projectId"`
	ProjectName    string    `json:"projectName"`
	ConversationID string    `json:"conversationId,omitempty"`
	TaskID         string    `json:"taskId,omitempty"`
	Title          string    `json:"title"`
	Body           string    `json:"body"`
	Priority       string    `json:"priority"` // "high" | "normal" | "low"
	ActionURL      string    `json:"actionUrl"`
	CreatedAt      time.Time `json:"createdAt"`
}

type notificationSubscriber struct {
	conn      *websocket.Conn
	send      chan []byte
	closeOnce sync.Once
}

func (sub *notificationSubscriber) close() {
	sub.closeOnce.Do(func() {
		close(sub.send)
		if sub.conn != nil {
			_ = sub.conn.Close()
		}
	})
}

const (
	notificationSubscriberQueueSize = 64
	notificationWriteTimeout        = 10 * time.Second
)

// migrateNotifications 创建 notifications 表。
func (s *Server) migrateNotifications(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `create table if not exists notifications (
	id text primary key,
	type text not null,
	project_id text not null,
	project_name text not null default '',
	conversation_id text not null default '',
	task_id text not null default '',
	title text not null,
	body text not null,
	priority text not null default 'normal',
	action_url text not null,
	dismissed integer not null default 0,
	created_at datetime not null
);
create index if not exists idx_notifications_dismissed on notifications(dismissed, created_at desc);`)
	if err != nil {
		return err
	}
	// 兼容旧表：添加新列（列已存在时 SQLite 返回错误但无副作用，忽略即可）
	s.db.ExecContext(ctx, `alter table notifications add column project_name text not null default ''`)
	s.db.ExecContext(ctx, `alter table notifications add column conversation_id text not null default ''`)
	s.db.ExecContext(ctx, `alter table notifications add column task_id text not null default ''`)
	return nil
}

// --- WebSocket 端点 ---

func (s *Server) subscribeNotifications(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	sub := &notificationSubscriber{
		conn: conn,
		send: make(chan []byte, notificationSubscriberQueueSize),
	}

	// writer goroutine（先启动，再发送历史通知）
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for data := range sub.send {
			_ = conn.SetWriteDeadline(time.Now().Add(notificationWriteTimeout))
			if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
				_ = conn.Close()
				return
			}
		}
	}()

	// 注册订阅者
	s.notificationSubMu.Lock()
	s.notificationSubs[conn] = sub
	s.notificationSubMu.Unlock()

	// 连接后发送未读通知（writer goroutine 已启动，可以消费）
	unread, err := s.getUnreadNotifications(r.Context())
	if err == nil {
		for _, n := range unread {
			data, _ := json.Marshal(n)
			select {
			case sub.send <- data:
			default:
				// 队列已满，跳过历史通知
			}
		}
	}

	defer func() {
		s.notificationSubMu.Lock()
		delete(s.notificationSubs, conn)
		s.notificationSubMu.Unlock()
		sub.close()
		<-writerDone
	}()

	// 读取循环检测断开
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}

// --- 广播函数 ---

func (s *Server) broadcastNotification(event NotificationEvent) {
	// 1. 持久化到 notifications 表（冗余存储 project_name/conversation_id/task_id，项目删除后仍可显示）
	s.notificationMu.Lock()
	event.CreatedAt = time.Now().UTC()
	_, err := s.db.ExecContext(s.runtimeCtx,
		`insert into notifications (id, type, project_id, project_name, conversation_id, task_id, title, body, priority, action_url, dismissed, created_at) values (?,?,?,?,?,?,?,?,?,?,0,?)`,
		event.ID, event.Type, event.ProjectID, event.ProjectName, event.ConversationID, event.TaskID, event.Title, event.Body, event.Priority, event.ActionURL, event.CreatedAt)
	s.notificationMu.Unlock()
	if err != nil {
		log.Printf("persist notification %s: %v", event.ID, err)
		return // 持久化失败则不广播
	}

	// 2. 广播给所有通知订阅者
	data, _ := json.Marshal(event)
	toClose := make([]*notificationSubscriber, 0)
	s.notificationSubMu.Lock()
	for conn, sub := range s.notificationSubs {
		select {
		case sub.send <- data:
		default:
			delete(s.notificationSubs, conn)
			toClose = append(toClose, sub)
		}
	}
	s.notificationSubMu.Unlock()
	for _, sub := range toClose {
		sub.close()
	}
}

// --- REST API ---

func (s *Server) listNotifications(w http.ResponseWriter, r *http.Request) {
	notifs, err := s.getUnreadNotifications(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if notifs == nil {
		notifs = []NotificationEvent{}
	}
	writeJSON(w, http.StatusOK, notifs)
}

func (s *Server) dismissNotification(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "notificationID")
	result, err := s.db.ExecContext(r.Context(), `update notifications set dismissed=1 where id=?`, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	changed, err := result.RowsAffected()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if changed == 0 {
		writeError(w, http.StatusNotFound, errNotificationNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) dismissAllNotifications(w http.ResponseWriter, r *http.Request) {
	s.notificationMu.Lock()
	dismissedAt := time.Now().UTC()
	_, err := s.db.ExecContext(r.Context(), `update notifications set dismissed=1 where dismissed=0`)
	s.notificationMu.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]time.Time{"dismissedAt": dismissedAt})
}

var errNotificationNotFound = errors.New("notification not found")

// --- 内部辅助 ---

func (s *Server) getUnreadNotifications(ctx context.Context) ([]NotificationEvent, error) {
	rows, err := s.db.QueryContext(ctx, `select n.id, n.type, n.project_id, coalesce(nullif(n.project_name,''), p.name,''), n.conversation_id, n.task_id, n.title, n.body, n.priority, n.action_url, n.created_at from notifications n left join projects p on p.id=n.project_id where n.dismissed=0 order by n.created_at desc`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []NotificationEvent
	for rows.Next() {
		var n NotificationEvent
		if err := rows.Scan(&n.ID, &n.Type, &n.ProjectID, &n.ProjectName, &n.ConversationID, &n.TaskID, &n.Title, &n.Body, &n.Priority, &n.ActionURL, &n.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, n)
	}
	return items, rows.Err()
}

// closeAllNotificationSubscribers 关闭所有通知 WebSocket 订阅者。
func (s *Server) closeAllNotificationSubscribers() {
	s.notificationSubMu.Lock()
	all := make([]*notificationSubscriber, 0, len(s.notificationSubs))
	for _, sub := range s.notificationSubs {
		all = append(all, sub)
	}
	s.notificationSubs = map[*websocket.Conn]*notificationSubscriber{}
	s.notificationSubMu.Unlock()
	for _, sub := range all {
		sub.close()
	}
}

// --- 通知触发辅助 ---

// notifyTaskStatusChange 在任务状态变更为需要通知的状态后调用。
// 使用 s.runtimeCtx 而非调用者 ctx，避免请求取消导致通知丢失。
func (s *Server) notifyTaskStatusChange(_ context.Context, taskID, newStatus string) {
	ctx := s.runtimeCtx
	var task Task
	if err := s.db.QueryRowContext(ctx, `select id,project_id,title,status from tasks where id=?`, taskID).Scan(&task.ID, &task.ProjectID, &task.Title, &task.Status); err != nil {
		return
	}
	project, err := s.getProjectByID(ctx, task.ProjectID)
	if err != nil {
		log.Printf("notifyTaskStatusChange: getProjectByID(%s): %v", task.ProjectID, err)
		return
	}

	var notifType, title, body, priority string
	switch newStatus {
	case taskAwaitingReview:
		notifType = "task.awaiting_review"
		title = "任务等待审查"
		body = "项目「" + project.Name + "」的任务「" + task.Title + "」已完成，等待审查"
		priority = "normal"
	case taskActionRequired:
		notifType = "task.action_required"
		title = "任务需要处理"
		body = "项目「" + project.Name + "」的任务「" + task.Title + "」需要你的操作"
		priority = "high"
	case taskDone:
		notifType = "task.done"
		title = "任务已完成"
		body = "项目「" + project.Name + "」的任务「" + task.Title + "」已完成"
		priority = "normal"
	default:
		return
	}

	s.broadcastNotification(NotificationEvent{
		ID:          uuid.NewString(),
		Type:        notifType,
		ProjectID:   task.ProjectID,
		ProjectName: project.Name,
		TaskID:      task.ID,
		Title:       title,
		Body:        body,
		Priority:    priority,
		ActionURL:   "/projects/" + task.ProjectID + "/tasks/" + task.ID,
		CreatedAt:   time.Now().UTC(),
	})
}

// notifyOrchestrationNeedsHuman 在编排任务进入 needs_human 状态后调用。
// 使用 s.runtimeCtx 而非调用者 ctx，避免请求取消导致通知丢失。
func (s *Server) notifyOrchestrationNeedsHuman(_ context.Context, job OrchestrationJob, cause error) {
	ctx := s.runtimeCtx
	var taskTitle string
	_ = s.db.QueryRowContext(ctx, `select title from tasks where id=?`, job.TaskID).Scan(&taskTitle)
	project, err := s.getProjectByID(ctx, job.ProjectID)
	if err != nil {
		log.Printf("notifyOrchestrationNeedsHuman: getProjectByID(%s): %v", job.ProjectID, err)
		return
	}

	s.broadcastNotification(NotificationEvent{
		ID:          uuid.NewString(),
		Type:        "orchestration.needs_human",
		ProjectID:   job.ProjectID,
		ProjectName: project.Name,
		TaskID:      job.TaskID,
		Title:       "编排任务需要人工介入",
		Body:        "项目「" + project.Name + "」的编排任务「" + taskTitle + "」需要人工介入：" + cause.Error(),
		Priority:    "high",
		ActionURL:   "/projects/" + job.ProjectID + "/tasks/" + job.TaskID,
		CreatedAt:   time.Now().UTC(),
	})
}

// notifyOrchestrationRetry 在编排任务重试后调用。
// 使用 s.runtimeCtx 而非调用者 ctx，避免请求取消导致通知丢失。
func (s *Server) notifyOrchestrationRetry(_ context.Context, job OrchestrationJob, cause error) {
	ctx := s.runtimeCtx
	var taskTitle string
	_ = s.db.QueryRowContext(ctx, `select title from tasks where id=?`, job.TaskID).Scan(&taskTitle)
	project, err := s.getProjectByID(ctx, job.ProjectID)
	if err != nil {
		log.Printf("notifyOrchestrationRetry: getProjectByID(%s): %v", job.ProjectID, err)
		return
	}

	s.broadcastNotification(NotificationEvent{
		ID:          uuid.NewString(),
		Type:        "orchestration.retry",
		ProjectID:   job.ProjectID,
		ProjectName: project.Name,
		TaskID:      job.TaskID,
		Title:       "编排任务正在重试",
		Body:        "项目「" + project.Name + "」的编排任务「" + taskTitle + "」正在重试：" + cause.Error(),
		Priority:    "normal",
		ActionURL:   "/projects/" + job.ProjectID + "/tasks/" + job.TaskID,
		CreatedAt:   time.Now().UTC(),
	})
}

// notifyOrchestrationPaused 在编排任务被暂停后调用。
// 使用 s.runtimeCtx 而非调用者 ctx，避免请求取消导致通知丢失。
func (s *Server) notifyOrchestrationPaused(_ context.Context, projectID, taskID string) {
	ctx := s.runtimeCtx
	var taskTitle string
	_ = s.db.QueryRowContext(ctx, `select title from tasks where id=?`, taskID).Scan(&taskTitle)
	project, err := s.getProjectByID(ctx, projectID)
	if err != nil {
		log.Printf("notifyOrchestrationPaused: getProjectByID(%s): %v", projectID, err)
		return
	}

	s.broadcastNotification(NotificationEvent{
		ID:          uuid.NewString(),
		Type:        "orchestration.paused",
		ProjectID:   projectID,
		ProjectName: project.Name,
		TaskID:      taskID,
		Title:       "编排任务已暂停",
		Body:        "项目「" + project.Name + "」的编排任务「" + taskTitle + "」已暂停",
		Priority:    "normal",
		ActionURL:   "/projects/" + projectID + "/tasks/" + taskID,
		CreatedAt:   time.Now().UTC(),
	})
}

// notifyOrchestrationResumed 在编排任务从 needs_human 恢复后调用。
// 使用 s.runtimeCtx 而非调用者 ctx，避免请求取消导致通知丢失。
func (s *Server) notifyOrchestrationResumed(_ context.Context, projectID, taskID string) {
	ctx := s.runtimeCtx
	var taskTitle string
	_ = s.db.QueryRowContext(ctx, `select title from tasks where id=?`, taskID).Scan(&taskTitle)
	project, err := s.getProjectByID(ctx, projectID)
	if err != nil {
		log.Printf("notifyOrchestrationResumed: getProjectByID(%s): %v", projectID, err)
		return
	}

	s.broadcastNotification(NotificationEvent{
		ID:          uuid.NewString(),
		Type:        "orchestration.resumed",
		ProjectID:   projectID,
		ProjectName: project.Name,
		TaskID:      taskID,
		Title:       "编排任务已恢复",
		Body:        "项目「" + project.Name + "」的编排任务「" + taskTitle + "」已恢复执行",
		Priority:    "normal",
		ActionURL:   "/projects/" + projectID + "/tasks/" + taskID,
		CreatedAt:   time.Now().UTC(),
	})
}
