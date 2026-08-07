package app

import (
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	websocketCloseWriteTimeout = time.Second
	websocketCloseWait         = time.Second
	websocketShutdownWait      = websocketCloseWait + 250*time.Millisecond
	websocketPingInterval      = 30 * time.Second
	websocketPongWait          = 45 * time.Second
)

// initiateWebSocketClose starts a protocol-level close handshake. The owning
// read loop closes the TCP connection only after it receives the peer's close
// response or a bounded force-close timer expires. The timer only calls Close,
// which Gorilla explicitly permits concurrently with the owning read loop.
func initiateWebSocketClose(conn *websocket.Conn, code int, reason string) {
	if conn == nil {
		return
	}
	_ = conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(code, reason),
		time.Now().Add(websocketCloseWriteTimeout),
	)
	time.AfterFunc(websocketCloseWait, func() { _ = conn.Close() })
}

func waitForWebSocketClose(conn *websocket.Conn) {
	if conn == nil {
		return
	}
	_ = conn.SetReadDeadline(time.Now().Add(websocketCloseWait))
	_, _, _ = conn.ReadMessage()
}

// startWebSocketHeartbeat makes stale browser and proxy connections observable
// and bounded. Browsers automatically answer WebSocket ping frames with pong.
func startWebSocketHeartbeat(conn *websocket.Conn, path string) func() {
	if conn == nil {
		return func() {}
	}
	_ = conn.SetReadDeadline(time.Now().Add(websocketPongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(websocketPongWait))
	})

	done := make(chan struct{})
	stopped := make(chan struct{})
	var stopOnce sync.Once
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(websocketPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(websocketCloseWriteTimeout)); err != nil {
					logWebSocketDisconnect(path, err)
					_ = conn.Close()
					return
				}
			}
		}
	}()

	return func() {
		stopOnce.Do(func() { close(done) })
		<-stopped
	}
}

func (s *Server) beginWebSocketSubscription(w http.ResponseWriter) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		http.Error(w, "control service is shutting down", http.StatusServiceUnavailable)
		return false
	}
	s.websocketWG.Add(1)
	return true
}

func (s *Server) isClosing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closing
}

func (s *Server) waitForWebSocketSubscriptions() {
	done := make(chan struct{})
	go func() {
		s.websocketWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(websocketShutdownWait):
		log.Printf("[ws] close handshake timed out; forcing remaining connections closed")
	}
}

func logWebSocketDisconnect(path string, err error) {
	if err == nil {
		return
	}
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) && (closeErr.Code == websocket.CloseNormalClosure || closeErr.Code == websocket.CloseGoingAway || closeErr.Code == websocket.CloseTryAgainLater) {
		return
	}
	log.Printf("[ws] %s disconnected: %v", path, err)
}
