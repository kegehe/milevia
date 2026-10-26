package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// NotificationEvent 是推送给前端的通知事件。
type NotificationEvent struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	ProjectID   string `json:"projectId"`
	ProjectName string `json:"projectName"`
	// TaskTitle 是任务的通知用标题（title 为空时回落到描述前 80 字）。
	// Windows 弹窗（方案A分层完整版）第二行「项目「X」· 任务Y」直接消费它，
	// 不允许前端从 body 里反解析 —— 那会变成同一事实的第二份判据。
	TaskTitle string `json:"taskTitle,omitempty"`
	// DurationMs 是最近一次任务执行的耗时（startedAt→finishedAt，服务端算好）。
	// 无执行记录或时间戳不完整时为 0，前端据此省略耗时行。
	DurationMs     int64  `json:"durationMs,omitempty"`
	ConversationID string `json:"conversationId,omitempty"`
	TaskID         string `json:"taskId,omitempty"`
	// ScheduledTaskID 是定时任务通知的目标，仅用于删定时任务时一并清掉通知；
	// 前端按 actionUrl 跳转，不需要这个内部关联。
	ScheduledTaskID string    `json:"-"`
	Title           string    `json:"title"`
	Body            string    `json:"body"`
	Priority        string    `json:"priority"` // "high" | "normal" | "low"
	ActionURL       string    `json:"actionUrl"`
	CreatedAt       time.Time `json:"createdAt"`
	DedupeKey       string    `json:"-"`
}

type notificationSubscriber struct {
	conn           *websocket.Conn
	send           chan []byte
	closeOnce      sync.Once
	closeFrameOnce sync.Once
}

func (sub *notificationSubscriber) close() {
	sub.closeOnce.Do(func() { close(sub.send) })
}

func (sub *notificationSubscriber) closeWithStatus(code int, reason string) {
	sub.close()
	sub.closeFrameOnce.Do(func() { initiateWebSocketClose(sub.conn, code, reason) })
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
	task_title text not null default '',
	duration_ms integer not null default 0,
	conversation_id text not null default '',
	task_id text not null default '',
	scheduled_task_id text not null default '',
	title text not null,
	body text not null,
	priority text not null default 'normal',
	action_url text not null,
	dismissed integer not null default 0,
	created_at datetime not null,
	dedupe_key text not null default ''
);
create index if not exists idx_notifications_dismissed on notifications(dismissed, created_at desc);
`)
	if err != nil {
		return fmt.Errorf("create notifications table: %w", err)
	}
	// 兼容旧表：添加新列（列已存在时 SQLite 返回错误但无副作用，忽略即可）
	s.db.ExecContext(ctx, `alter table notifications add column project_name text not null default ''`)
	s.db.ExecContext(ctx, `alter table notifications add column task_title text not null default ''`)
	s.db.ExecContext(ctx, `alter table notifications add column duration_ms integer not null default 0`)
	s.db.ExecContext(ctx, `alter table notifications add column conversation_id text not null default ''`)
	s.db.ExecContext(ctx, `alter table notifications add column task_id text not null default ''`)
	s.db.ExecContext(ctx, `alter table notifications add column scheduled_task_id text not null default ''`)
	s.db.ExecContext(ctx, `alter table notifications add column dedupe_key text not null default ''`)
	// 索引放在加列之后：旧表要等 alter 完成才有 conversation_id / scheduled_task_id。
	// 删除项目/会话/定时任务都按这三列清理通知，不能每次全表扫。
	if _, err = s.db.ExecContext(ctx, `create index if not exists idx_notifications_project on notifications(project_id) where project_id<>'';
create index if not exists idx_notifications_conversation on notifications(conversation_id) where conversation_id<>'';
create index if not exists idx_notifications_scheduled_task on notifications(scheduled_task_id) where scheduled_task_id<>''`); err != nil {
		return fmt.Errorf("create notification target indexes: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `create unique index if not exists idx_notifications_dedupe_key on notifications(dedupe_key) where dedupe_key<>''`)
	return err
}

// pruneOrphanedNotifications 清理目标实体已不存在的旧通知。
//
// 新删除的项目/会话/定时任务都会在各自事务里一并清掉通知；但修复之前删掉的实体
// 不会经过那些路径，这些行会一直留在通知中心，点“查看”落到不存在的页面。
// 必须在 projects / conversations / scheduled_tasks / notifications 都建好之后调用
// （启动迁移末尾）。
//
// 定时任务通知的会话只是执行历史，目标仍是定时任务：新行有 scheduled_task_id，
// 修复前的旧行只有 action_url，所以这里两种都认。
func (s *Server) pruneOrphanedNotifications(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `delete from notifications
where (project_id<>'' and not exists (select 1 from projects p where p.id=notifications.project_id))
   or (scheduled_task_id<>'' and not exists (select 1 from scheduled_tasks t where t.id=notifications.scheduled_task_id))
   or (scheduled_task_id=''
       and action_url like '%/tasks/schedules/%'
       and not exists (select 1 from scheduled_tasks t
                       where notifications.action_url = '/projects/'||notifications.project_id||'/tasks/schedules/'||t.id))
   or (scheduled_task_id=''
       and action_url not like '%/tasks/schedules/%'
       and conversation_id<>''
       and not exists (select 1 from conversations c where c.id=notifications.conversation_id))`)
	return err
}

// deleteConversationNotificationsTx 删除“查看”目标会话已被删除的通知。
// 定时任务通知（新行有 scheduled_task_id，旧行的 actionUrl 指向定时任务页）的目标
// 不是会话，会话只是那次执行的历史，因此保留 —— 删会话不该让一条仍然有效的
// 定时任务通知消失。
func deleteConversationNotificationsTx(ctx context.Context, tx *sql.Tx, conversationID string) error {
	_, err := tx.ExecContext(ctx, `delete from notifications
where conversation_id<>'' and conversation_id=? and scheduled_task_id='' and action_url not like '%/tasks/schedules/%'`, conversationID)
	return err
}

// deleteScheduledTaskNotificationsTx 删除指向已删定时任务的通知。
// 除了新行按 scheduled_task_id 命中，还要处理修复前没有该列、只有 action_url 的旧行。
func deleteScheduledTaskNotificationsTx(ctx context.Context, tx *sql.Tx, projectID, scheduledTaskID string) error {
	// 新行按索引删；修复前的旧行没有该列，只能按 action_url 扫一遍（数量很少）。
	if _, err := tx.ExecContext(ctx, `delete from notifications where scheduled_task_id<>'' and scheduled_task_id=?`, scheduledTaskID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `delete from notifications
where scheduled_task_id='' and action_url=?`, "/projects/"+projectID+"/tasks/schedules/"+scheduledTaskID)
	return err
}

// --- WebSocket 端点 ---

func (s *Server) subscribeNotifications(w http.ResponseWriter, r *http.Request) {
	const path = "/ws/notifications"
	if !s.beginWebSocketSubscription(w) {
		return
	}
	defer s.websocketWG.Done()
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	if s.isClosing() {
		initiateWebSocketClose(conn, websocket.CloseGoingAway, "server shutting down")
		waitForWebSocketClose(conn)
		return
	}
	stopHeartbeat := startWebSocketHeartbeat(conn, path)

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
				logWebSocketDisconnect(path, err)
				_ = conn.Close()
				return
			}
		}
	}()

	// 以与 Server.Close 相同的生命周期锁完成最终注册，避免升级后、
	// 注册前开始停机时留下未被关闭流程发现的连接。
	if !s.addNotificationSubscriber(conn, sub) {
		stopHeartbeat()
		sub.closeWithStatus(websocket.CloseGoingAway, "server shutting down")
		waitForWebSocketClose(conn)
		<-writerDone
		return
	}

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
		stopHeartbeat()
		s.notificationSubMu.Lock()
		delete(s.notificationSubs, conn)
		s.notificationSubMu.Unlock()
		sub.close()
		_ = conn.Close()
		<-writerDone
	}()

	// 读取循环检测断开
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			logWebSocketDisconnect(path, err)
			return
		}
	}
}

func (s *Server) addNotificationSubscriber(conn *websocket.Conn, sub *notificationSubscriber) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return false
	}
	s.notificationSubMu.Lock()
	s.notificationSubs[conn] = sub
	s.notificationSubMu.Unlock()
	return true
}

// --- 广播函数 ---

func (s *Server) broadcastNotification(event NotificationEvent) {
	// 1. 持久化到 notifications 表。project_name/conversation_id/task_id 都是冗余快照，
	// 显示时不依赖 join；但项目本身被删除时，这些通知会随项目一起清掉
	// （见 deleteProjectLocked），否则“查看”会跳到已不存在的项目。
	// where 再挡一次"检查目标存在之后、插入之前目标被删"的竞态：项目不存在就不落库、
	// 也不广播；任务/编排通知的会话不存在时退回项目会话入口重试一次，不丢通知。
	// 定时任务通知的目标是任务页，会话只是执行历史，不受会话检查限制。
	s.notificationMu.Lock()
	event.CreatedAt = time.Now().UTC()
	insert := func(conversationID, actionURL string) (sql.Result, error) {
		return s.db.ExecContext(s.runtimeCtx,
			`insert or ignore into notifications (id, type, project_id, project_name, task_title, duration_ms, conversation_id, task_id, scheduled_task_id, title, body, priority, action_url, dismissed, created_at, dedupe_key)
			select ?,?,?,?,?,?,?,?,?,?,?,?,?,0,?,?
			where (?='' or exists (select 1 from projects where id=?))
			  and (?<>'' or ?='' or exists (select 1 from conversations where id=?))`,
			event.ID, event.Type, event.ProjectID, event.ProjectName, event.TaskTitle, event.DurationMs, conversationID, event.TaskID, event.ScheduledTaskID, event.Title, event.Body, event.Priority, actionURL, event.CreatedAt, event.DedupeKey,
			event.ProjectID, event.ProjectID, event.ScheduledTaskID, conversationID, conversationID)
	}
	result, err := insert(event.ConversationID, event.ActionURL)
	if err == nil {
		if changed, rowsErr := result.RowsAffected(); rowsErr == nil && changed == 0 && event.ConversationID != "" && event.ScheduledTaskID == "" {
			// rowsAffected=0 也可能是去重键命中（同一通知已存在），不能一律当成
			// "会话被删"。先确认会话确实不在了、项目还在，才退回项目入口重试；
			// 这样去重命中时不再白做一次被忽略的插入。
			var conversationExists, projectExists bool
			if queryErr := s.db.QueryRowContext(s.runtimeCtx,
				`select exists(select 1 from conversations where id=?), exists(select 1 from projects where id=?)`,
				event.ConversationID, event.ProjectID).Scan(&conversationExists, &projectExists); queryErr == nil && !conversationExists && projectExists {
				event.ConversationID = ""
				event.ActionURL = "/projects/" + event.ProjectID + "/conversations"
				result, err = insert("", event.ActionURL)
			}
		}
	}
	s.notificationMu.Unlock()
	if err != nil {
		log.Printf("persist notification %s: %v", event.ID, err)
		return // 持久化失败则不广播
	}
	if changed, err := result.RowsAffected(); err != nil || changed == 0 {
		return
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
		log.Printf("[ws] /ws/notifications subscriber queue full; closing slow client")
		go sub.closeWithStatus(websocket.CloseTryAgainLater, "client is too slow")
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
	rows, err := s.db.QueryContext(ctx, `select n.id, n.type, n.project_id, coalesce(nullif(n.project_name,''), p.name,''), n.task_title, n.duration_ms, n.conversation_id, n.task_id, n.title, n.body, n.priority, n.action_url, n.created_at from notifications n left join projects p on p.id=n.project_id where n.dismissed=0 order by n.created_at desc`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []NotificationEvent
	for rows.Next() {
		var n NotificationEvent
		if err := rows.Scan(&n.ID, &n.Type, &n.ProjectID, &n.ProjectName, &n.TaskTitle, &n.DurationMs, &n.ConversationID, &n.TaskID, &n.Title, &n.Body, &n.Priority, &n.ActionURL, &n.CreatedAt); err != nil {
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
	var closeWG sync.WaitGroup
	closeWG.Add(len(all))
	for _, sub := range all {
		go func(sub *notificationSubscriber) {
			defer closeWG.Done()
			sub.closeWithStatus(websocket.CloseGoingAway, "server shutting down")
		}(sub)
	}
	closeWG.Wait()
}

// --- 通知触发辅助 ---

// notifyTaskStatusChange 在任务状态变更为需要通知的状态后调用。
// 使用 s.runtimeCtx 而非调用者 ctx，避免请求取消导致通知丢失。
func (s *Server) notifyTaskStatusChange(_ context.Context, taskID, newStatus string) {
	ctx := s.runtimeCtx
	var task Task
	if err := s.db.QueryRowContext(ctx, `select id,project_id,title,description,status,updated_at from tasks where id=?`, taskID).Scan(&task.ID, &task.ProjectID, &task.Title, &task.Description, &task.Status, &task.UpdatedAt); err != nil {
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
		body = "项目「" + project.Name + "」的任务「" + taskNotificationTitle(task) + "」已完成，等待审查"
		priority = "normal"
	case taskActionRequired:
		notifType = "task.action_required"
		title = "任务需要处理"
		body = "项目「" + project.Name + "」的任务「" + taskNotificationTitle(task) + "」需要你的操作"
		priority = "high"
	case taskDone:
		notifType = "task.done"
		title = "任务已完成"
		body = "项目「" + project.Name + "」的任务「" + taskNotificationTitle(task) + "」已完成"
		priority = "normal"
	default:
		return
	}
	conversationID, actionURL := s.taskNotificationTarget(ctx, task.ProjectID, task.ID)

	// 方案A分层完整版：耗时由服务端从最近一次执行算好（startedAt→finishedAt），
	// 前端只消费、不自己算。没有 started_at（任务没真正跑起来，比如排队中被打断）
	// 时不拿 created_at 兜底 —— 那会把排队等待当执行时间；时间戳不完整就保持 0，
	// 前端据此省略耗时行（三态不并档）。
	var durationMs int64
	if run, err := s.latestTaskRun(ctx, taskID); err == nil && run.StartedAt != nil && run.FinishedAt != nil && run.FinishedAt.After(*run.StartedAt) {
		durationMs = run.FinishedAt.Sub(*run.StartedAt).Milliseconds()
	}

	s.broadcastNotification(NotificationEvent{
		ID:             uuid.NewString(),
		Type:           notifType,
		ProjectID:      task.ProjectID,
		ProjectName:    project.Name,
		TaskTitle:      taskNotificationTitle(task),
		DurationMs:     durationMs,
		ConversationID: conversationID,
		TaskID:         task.ID,
		Title:          title,
		Body:           body,
		Priority:       priority,
		ActionURL:      actionURL,
		CreatedAt:      time.Now().UTC(),
		DedupeKey:      "task-status:" + task.ID + ":" + newStatus + ":" + task.UpdatedAt.UTC().Format(time.RFC3339Nano),
	})
}

func taskNotificationTitle(task Task) string {
	if title := strings.TrimSpace(task.Title); title != "" {
		return title
	}
	words := strings.Fields(task.Description)
	if len(words) == 0 {
		return "未命名任务"
	}
	value := strings.Join(words, " ")
	runes := []rune(value)
	if len(runes) > 80 {
		return string(runes[:80]) + "..."
	}
	return value
}

func (s *Server) taskNotificationTitleByID(ctx context.Context, taskID string) string {
	var task Task
	if err := s.db.QueryRowContext(ctx, `select title,description from tasks where id=?`, taskID).Scan(&task.Title, &task.Description); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("taskNotificationTitleByID: load task %s: %v", taskID, err)
		}
		return "未命名任务"
	}
	return taskNotificationTitle(task)
}

// notifyOrchestrationNeedsHuman 在编排任务进入 needs_human 状态后调用。
// 使用 s.runtimeCtx 而非调用者 ctx，避免请求取消导致通知丢失。
func (s *Server) notifyOrchestrationNeedsHuman(_ context.Context, job OrchestrationJob, cause error) {
	ctx := s.runtimeCtx
	taskTitle := s.taskNotificationTitleByID(ctx, job.TaskID)
	project, err := s.getProjectByID(ctx, job.ProjectID)
	if err != nil {
		log.Printf("notifyOrchestrationNeedsHuman: getProjectByID(%s): %v", job.ProjectID, err)
		return
	}
	conversationID, actionURL := s.taskNotificationTarget(ctx, job.ProjectID, job.TaskID)

	s.broadcastNotification(NotificationEvent{
		ID:             uuid.NewString(),
		Type:           "orchestration.needs_human",
		ProjectID:      job.ProjectID,
		ProjectName:    project.Name,
		ConversationID: conversationID,
		TaskID:         job.TaskID,
		Title:          "编排任务需要人工介入",
		Body:           "项目「" + project.Name + "」的编排任务「" + taskTitle + "」需要人工介入：" + orchestrationFailureText(cause),
		Priority:       "high",
		ActionURL:      actionURL,
		CreatedAt:      time.Now().UTC(),
	})
}

// notifyOrchestrationRetry 在编排任务重试后调用。
// 使用 s.runtimeCtx 而非调用者 ctx，避免请求取消导致通知丢失。
func (s *Server) notifyOrchestrationRetry(_ context.Context, job OrchestrationJob, cause error) {
	ctx := s.runtimeCtx
	taskTitle := s.taskNotificationTitleByID(ctx, job.TaskID)
	project, err := s.getProjectByID(ctx, job.ProjectID)
	if err != nil {
		log.Printf("notifyOrchestrationRetry: getProjectByID(%s): %v", job.ProjectID, err)
		return
	}
	conversationID, actionURL := s.taskNotificationTarget(ctx, job.ProjectID, job.TaskID)

	s.broadcastNotification(NotificationEvent{
		ID:             uuid.NewString(),
		Type:           "orchestration.retry",
		ProjectID:      job.ProjectID,
		ProjectName:    project.Name,
		ConversationID: conversationID,
		TaskID:         job.TaskID,
		Title:          "编排任务正在重试",
		Body:           "项目「" + project.Name + "」的编排任务「" + taskTitle + "」正在重试：" + orchestrationFailureText(cause),
		Priority:       "normal",
		ActionURL:      actionURL,
		CreatedAt:      time.Now().UTC(),
	})
}

// notifyOrchestrationPaused 在编排任务被暂停后调用。
// 使用 s.runtimeCtx 而非调用者 ctx，避免请求取消导致通知丢失。
func (s *Server) notifyOrchestrationPaused(_ context.Context, projectID, taskID string) {
	ctx := s.runtimeCtx
	taskTitle := s.taskNotificationTitleByID(ctx, taskID)
	project, err := s.getProjectByID(ctx, projectID)
	if err != nil {
		log.Printf("notifyOrchestrationPaused: getProjectByID(%s): %v", projectID, err)
		return
	}
	conversationID, actionURL := s.taskNotificationTarget(ctx, projectID, taskID)

	s.broadcastNotification(NotificationEvent{
		ID:             uuid.NewString(),
		Type:           "orchestration.paused",
		ProjectID:      projectID,
		ProjectName:    project.Name,
		ConversationID: conversationID,
		TaskID:         taskID,
		Title:          "编排任务已暂停",
		Body:           "项目「" + project.Name + "」的编排任务「" + taskTitle + "」已暂停",
		Priority:       "normal",
		ActionURL:      actionURL,
		CreatedAt:      time.Now().UTC(),
	})
}

// notifyOrchestrationResumed 在编排任务从 needs_human 恢复后调用。
// 使用 s.runtimeCtx 而非调用者 ctx，避免请求取消导致通知丢失。
func (s *Server) notifyOrchestrationResumed(_ context.Context, projectID, taskID string) {
	ctx := s.runtimeCtx
	taskTitle := s.taskNotificationTitleByID(ctx, taskID)
	project, err := s.getProjectByID(ctx, projectID)
	if err != nil {
		log.Printf("notifyOrchestrationResumed: getProjectByID(%s): %v", projectID, err)
		return
	}
	conversationID, actionURL := s.taskNotificationTarget(ctx, projectID, taskID)

	s.broadcastNotification(NotificationEvent{
		ID:             uuid.NewString(),
		Type:           "orchestration.resumed",
		ProjectID:      projectID,
		ProjectName:    project.Name,
		ConversationID: conversationID,
		TaskID:         taskID,
		Title:          "编排任务已恢复",
		Body:           "项目「" + project.Name + "」的编排任务「" + taskTitle + "」已恢复执行",
		Priority:       "normal",
		ActionURL:      actionURL,
		CreatedAt:      time.Now().UTC(),
	})
}

// taskNotificationTarget 让任务相关通知回到发起该任务的会话。
// 未关联执行记录时，回到项目最近活动的会话入口。这只是通知回退，不能
// 作为任何交互任务的隐式目标。
func (s *Server) taskNotificationTarget(ctx context.Context, projectID, taskID string) (string, string) {
	conversationID := ""
	taskRun, err := s.latestTaskRun(ctx, taskID)
	if err == nil {
		conversationID = taskRun.ConversationID
	} else if !errors.Is(err, sql.ErrNoRows) {
		log.Printf("taskNotificationTarget: load task run for %s: %v", taskID, err)
	}
	if conversationID == "" {
		if err := s.db.QueryRowContext(ctx, `select id from conversations where project_id=? order by last_activity_at desc,id desc limit 1`, projectID).Scan(&conversationID); err != nil && !errors.Is(err, sql.ErrNoRows) {
			log.Printf("taskNotificationTarget: load recent conversation for %s: %v", projectID, err)
		}
	}
	baseURL := "/projects/" + projectID + "/conversations"
	if conversationID == "" {
		return "", baseURL
	}
	return conversationID, baseURL + "/" + conversationID
}
