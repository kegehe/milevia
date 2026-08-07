package app

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// processStatusSubscriber 订阅 /ws/processes 的进程状态广播。与 notificationSubscriber
// 同构：单 writer goroutine 消费订阅者自己的 send 队列，广播方零并发写连接。
type processStatusSubscriber struct {
	conn           *websocket.Conn
	send           chan RunStatusEvent
	closeOnce      sync.Once
	closeFrameOnce sync.Once
}

func (sub *processStatusSubscriber) close() {
	sub.closeOnce.Do(func() { close(sub.send) })
}

func (sub *processStatusSubscriber) closeWithStatus(code int, reason string) {
	sub.close()
	sub.closeFrameOnce.Do(func() { initiateWebSocketClose(sub.conn, code, reason) })
}

const (
	processStatusSubscriberQueueSize = 64
	processStatusWriteTimeout        = 10 * time.Second
)

// projectProcessStatusItem 是批量进程状态端点 /api/projects/processes/statuses 的响应项。
// 仅含精简字段（runStatus + 可选 runPid/runStartedAt），不携带日志，避免 N×200 拷贝。
type projectProcessStatusItem struct {
	ID           string     `json:"id"`
	RunStatus    RunStatus  `json:"runStatus"`
	RunPID       *int       `json:"runPid,omitempty"`
	RunStartedAt *time.Time `json:"runStartedAt,omitempty"`
}

// listProjectProcessStatuses 遍历现存项目的 runner，返回各项目进程状态。项目未创建
// runner 时置 stopped。持有 runManagersMu.RLock 与 DB 迭代锁资源不同，无死锁。
func (s *Server) listProjectProcessStatuses(w http.ResponseWriter, r *http.Request) {
	ids, err := s.listProjectIDs(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	s.runManagersMu.RLock()
	items := make([]projectProcessStatusItem, 0, len(ids))
	for _, id := range ids {
		item := projectProcessStatusItem{ID: id, RunStatus: RunStatusStopped}
		if runner, ok := s.runManagers[id]; ok {
			snap := runner.LightStatusSnapshot()
			item.RunStatus = snap.Status
			item.RunPID = snap.PID
			item.RunStartedAt = snap.StartedAt
		}
		items = append(items, item)
	}
	s.runManagersMu.RUnlock()

	writeJSON(w, http.StatusOK, items)
}

// listProjectIDs 返回全部现存项目 id（按 created_at 降序，与列表页一致）。
func (s *Server) listProjectIDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `select id from projects order by created_at desc`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if ids == nil {
		ids = []string{}
	}
	return ids, nil
}

// subscribeProcessStatuses WS 端点：连接即推送全量进程状态快照（每项目一帧，同形帧），
// 之后实时推送状态变更。生命周期与现有 WS 约定（websocket_lifecycle.go）完全对齐。
func (s *Server) subscribeProcessStatuses(w http.ResponseWriter, r *http.Request) {
	const path = "/ws/processes"
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

	sub := &processStatusSubscriber{
		conn: conn,
		send: make(chan RunStatusEvent, processStatusSubscriberQueueSize),
	}

	// writer goroutine（先启动，再注册，使注册后的快照可被消费）
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for event := range sub.send {
			_ = conn.SetWriteDeadline(time.Now().Add(processStatusWriteTimeout))
			if err := conn.WriteJSON(event); err != nil {
				logWebSocketDisconnect(path, err)
				_ = conn.Close()
				return
			}
		}
	}()

	// 以与 Server.Close 相同的生命周期锁完成最终注册，避免升级后、注册前开始停机
	// 时留下未被关闭流程发现的连接。
	if !s.addProcessStatusSubscriber(conn, sub) {
		stopHeartbeat()
		sub.closeWithStatus(websocket.CloseGoingAway, "server shutting down")
		waitForWebSocketClose(conn)
		<-writerDone
		return
	}

	// 注册后推送全量快照（与实时变更同形帧，前端零特判）。
	s.pushProcessStatusSnapshot(sub)

	defer func() {
		stopHeartbeat()
		s.processStatusSubMu.Lock()
		delete(s.processStatusSubs, conn)
		s.processStatusSubMu.Unlock()
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

func (s *Server) addProcessStatusSubscriber(conn *websocket.Conn, sub *processStatusSubscriber) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return false
	}
	s.processStatusSubMu.Lock()
	s.processStatusSubs[conn] = sub
	s.processStatusSubMu.Unlock()
	return true
}

// pushProcessStatusSnapshot 向单个订阅者逐项目推送全量快照帧。注册后立即调用；
// 快照与实时变更走同一帧形状，仅填有意义的 PID/StartedAt。
func (s *Server) pushProcessStatusSnapshot(sub *processStatusSubscriber) {
	ids, err := s.listProjectIDs(s.runtimeCtx)
	if err != nil {
		return
	}
	s.runManagersMu.RLock()
	for _, id := range ids {
		event := RunStatusEvent{ProjectID: id, Status: RunStatusStopped}
		if runner, ok := s.runManagers[id]; ok {
			snap := runner.LightStatusSnapshot()
			event.Status = snap.Status
			event.StartedAt = snap.StartedAt
			event.PID = snap.PID
		}
		select {
		case sub.send <- event:
		default:
			// 快照积压：丢弃剩余帧即可，实时变更仍会收敛快照与真实状态。
		}
	}
	s.runManagersMu.RUnlock()
}

// broadcastProcessStatus 不落库，直接推给所有订阅者；慢客户端满队列即断连。
func (s *Server) broadcastProcessStatus(event RunStatusEvent) {
	toClose := make([]*processStatusSubscriber, 0)
	s.processStatusSubMu.Lock()
	for conn, sub := range s.processStatusSubs {
		select {
		case sub.send <- event:
		default:
			delete(s.processStatusSubs, conn)
			toClose = append(toClose, sub)
		}
	}
	s.processStatusSubMu.Unlock()
	for _, sub := range toClose {
		log.Printf("[ws] /ws/processes subscriber queue full; closing slow client")
		go sub.closeWithStatus(websocket.CloseTryAgainLater, "client is too slow")
	}
}

// closeAllProcessStatusSubscribers 关闭所有进程状态订阅者，须挂接在 Server.Close 停机清单。
func (s *Server) closeAllProcessStatusSubscribers() {
	s.processStatusSubMu.Lock()
	all := make([]*processStatusSubscriber, 0, len(s.processStatusSubs))
	for _, sub := range s.processStatusSubs {
		all = append(all, sub)
	}
	s.processStatusSubs = map[*websocket.Conn]*processStatusSubscriber{}
	s.processStatusSubMu.Unlock()
	var closeWG sync.WaitGroup
	closeWG.Add(len(all))
	for _, sub := range all {
		go func(sub *processStatusSubscriber) {
			defer closeWG.Done()
			sub.closeWithStatus(websocket.CloseGoingAway, "server shutting down")
		}(sub)
	}
	closeWG.Wait()
}
