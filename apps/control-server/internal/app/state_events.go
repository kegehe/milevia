package app

import (
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// 轻量级「状态已变化」失效信号通道 /ws/events。与 notification / process_status
// 同构，但不携带业务负载：客户端收到事件后去拉取对应的 REST 数据。这样把前端
// 「固定周期轮询」降级为「事件驱动的按需拉取 + 兜底慢轮询」——状态未变化时不
// 再持续发请求，变化时也不再等待最多一个轮询周期的延迟。
//
// 即使个别变更点忘记广播，兜底慢轮询仍能最终收敛；事件只负责缩短延迟与减少请求。

const (
	// stEvAll 表示同时刷新全部订阅的数据（项目状态 + 定时任务）。订阅者连接
	// 建立后立即推送一帧，让首次连接/重连即补齐挂起期间遗漏的变化。
	stEvAll = "all"
	// stEvProjects 表示某项目的状态聚合已变化（列表页 running / 会话数 /
	// 活跃标题 / 优化建议分析）。ProjectID 为空表示所有项目都可能变化。
	stEvProjects = "projects"
	// stEvScheduledTasks 表示某项目的定时任务集合已变化（增删改、暂停/恢复、
	// 运行状态流转）。
	stEvScheduledTasks = "scheduled-tasks"
)

// StateEvent 是推送给 /ws/events 订阅者的粗粒度失效信号。
type StateEvent struct {
	Type      string `json:"type"`
	ProjectID string `json:"projectId,omitempty"`
}

type stateEventSubscriber struct {
	conn           *websocket.Conn
	send           chan StateEvent
	closeOnce      sync.Once
	closeFrameOnce sync.Once
}

func (sub *stateEventSubscriber) close() {
	sub.closeOnce.Do(func() { close(sub.send) })
}

func (sub *stateEventSubscriber) closeWithStatus(code int, reason string) {
	sub.close()
	sub.closeFrameOnce.Do(func() { initiateWebSocketClose(sub.conn, code, reason) })
}

const (
	stateEventSubscriberQueueSize = 64
	stateEventWriteTimeout        = 10 * time.Second
)

func (s *Server) subscribeStateEvents(w http.ResponseWriter, r *http.Request) {
	const path = "/ws/events"
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

	sub := &stateEventSubscriber{
		conn: conn,
		send: make(chan StateEvent, stateEventSubscriberQueueSize),
	}

	// writer goroutine（先启动，再注册，使随后的广播可被消费）
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for event := range sub.send {
			_ = conn.SetWriteDeadline(time.Now().Add(stateEventWriteTimeout))
			if err := conn.WriteJSON(event); err != nil {
				logWebSocketDisconnect(path, err)
				_ = conn.Close()
				return
			}
		}
	}()

	// 以与 Server.Close 相同的生命周期锁完成最终注册，避免升级后、注册前开始
	// 停机时留下未被关闭流程发现的连接。
	if !s.addStateEventSubscriber(conn, sub) {
		stopHeartbeat()
		sub.closeWithStatus(websocket.CloseGoingAway, "server shutting down")
		waitForWebSocketClose(conn)
		<-writerDone
		return
	}

	// 连接建立即推一帧全量信号，让前端立即补齐挂起期间的变化并建立明确来源。
	s.broadcastStateEvent(stEvAll, "")

	defer func() {
		stopHeartbeat()
		s.stateEventSubMu.Lock()
		delete(s.stateEventSubs, conn)
		s.stateEventSubMu.Unlock()
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

func (s *Server) addStateEventSubscriber(conn *websocket.Conn, sub *stateEventSubscriber) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return false
	}
	s.stateEventSubMu.Lock()
	s.stateEventSubs[conn] = sub
	s.stateEventSubMu.Unlock()
	return true
}

// broadcastStateEvent 向 /ws/events 全部订阅者广播一个失效信号。非阻塞；
// 慢客户端满队列即断连（与现有 WS 通道约定一致）。
func (s *Server) broadcastStateEvent(typ, projectID string) {
	event := StateEvent{Type: typ, ProjectID: projectID}
	toClose := make([]*stateEventSubscriber, 0)
	s.stateEventSubMu.Lock()
	for conn, sub := range s.stateEventSubs {
		select {
		case sub.send <- event:
		default:
			delete(s.stateEventSubs, conn)
			toClose = append(toClose, sub)
		}
	}
	s.stateEventSubMu.Unlock()
	for _, sub := range toClose {
		log.Printf("[ws] /ws/events subscriber queue full; closing slow client")
		go sub.closeWithStatus(websocket.CloseTryAgainLater, "client is too slow")
	}
}

// closeAllStateEventSubscribers 关闭全部 /ws/events 订阅者，须挂接在 Server.Close 停机清单。
func (s *Server) closeAllStateEventSubscribers() {
	s.stateEventSubMu.Lock()
	all := make([]*stateEventSubscriber, 0, len(s.stateEventSubs))
	for _, sub := range s.stateEventSubs {
		all = append(all, sub)
	}
	s.stateEventSubs = map[*websocket.Conn]*stateEventSubscriber{}
	s.stateEventSubMu.Unlock()
	var closeWG sync.WaitGroup
	closeWG.Add(len(all))
	for _, sub := range all {
		go func(sub *stateEventSubscriber) {
			defer closeWG.Done()
			sub.closeWithStatus(websocket.CloseGoingAway, "server shutting down")
		}(sub)
	}
	closeWG.Wait()
}