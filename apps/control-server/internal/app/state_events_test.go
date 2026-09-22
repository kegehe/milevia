package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestStateEventsWebSocketContract 用真实 gorilla/websocket 客户端连接 /ws/events，
// 触发一次 broadcastStateEvent，读取一帧并校验线上 JSON 字段名（type/projectId 及
// omitempty）与语义 —— 这是前端 /ws/events 解析依赖的契约，防止 json tag 改名而测试假绿。
func TestStateEventsWebSocketContract(t *testing.T) {
	server := newTestServer(t)
	httpServer := httptest.NewServer(server.routes())
	t.Cleanup(httpServer.Close)

	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/events"
	connection, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Origin": []string{httpServer.URL}})
	if err != nil {
		t.Fatalf("dial /ws/events: %v", err)
	}
	defer connection.Close()

	// 连接建立即推一帧 all（服务端 subscribeStateEvents 调 broadcastStateEvent(stEvAll, "")）。
	// 先读完这一帧，避免干扰后续断言。
	connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := connection.ReadMessage(); err != nil {
		t.Fatalf("read initial all frame: %v", err)
	}

	// 等待订阅者注册，再广播，确保帧被本连接收到。
	deadline := time.Now().Add(2 * time.Second)
	for {
		server.stateEventSubMu.Lock()
		registered := len(server.stateEventSubs) == 1
		server.stateEventSubMu.Unlock()
		if registered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("state event WebSocket was not registered")
		}
		time.Sleep(10 * time.Millisecond)
	}

	server.broadcastStateEvent(stEvScheduledTasks, "proj-1")

	if err := connection.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	_, data, err := connection.ReadMessage()
	if err != nil {
		t.Fatalf("read state event frame: %v", err)
	}
	// 用原始 map 断言线上字段名，而非复用 Go struct（避免 struct→json tag 隐式推断掩盖改名）。
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("decode frame %s: %v", data, err)
	}
	if raw["type"] != "scheduled-tasks" {
		t.Fatalf("frame type = %v, want scheduled-tasks (线上字段名必须是 type)", raw["type"])
	}
	if raw["projectId"] != "proj-1" {
		t.Fatalf("frame projectId = %v, want proj-1 (线上字段名必须是 projectId)", raw["projectId"])
	}
}
