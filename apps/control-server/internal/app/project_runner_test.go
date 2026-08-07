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

func TestDecodeRunOutputLine(t *testing.T) {
	// GBK 编码的 "中文测试"：d6d0 cec4 b2e2 cad4（来自真实 cmd.exe 的实测字节）
	gbkChinese := []byte{0xd6, 0xd0, 0xce, 0xc4, 0xb2, 0xe2, 0xca, 0xd4}
	utf8Chinese := "中文测试"

	tests := []struct {
		name      string
		line      []byte
		transcode bool
		want      string
	}{
		{
			name:      "windows GBK 乱码被转成 UTF-8",
			line:      gbkChinese,
			transcode: true,
			want:      utf8Chinese,
		},
		{
			name:      "非 windows 目标原样保留",
			line:      gbkChinese,
			transcode: false,
			want:      string(gbkChinese),
		},
		{
			name:      "已是合法 UTF-8 不再二次转码",
			line:      []byte(utf8Chinese),
			transcode: true,
			want:      utf8Chinese,
		},
		{
			name:      "纯 ASCII 不受影响",
			line:      []byte("npm run dev"),
			transcode: true,
			want:      "npm run dev",
		},
		{
			name:      "空行",
			line:      []byte{},
			transcode: true,
			want:      "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decodeRunOutputLine(tt.line, tt.transcode); got != tt.want {
				t.Fatalf("decodeRunOutputLine(%q, %v) = %q, want %q", tt.line, tt.transcode, got, tt.want)
			}
		})
	}
}

// TestDecodeRunOutputLineConcurrent 用多个 goroutine 同时解码（模拟 readPipe 里
// stdout/stderr 两个管道线程共享同一个 gbkDecoder），配合 -race 验证共享解码器无数据竞争。
func TestDecodeRunOutputLineConcurrent(t *testing.T) {
	gbkChinese := []byte{0xd6, 0xd0, 0xce, 0xc4, 0xb2, 0xe2, 0xca, 0xd4}
	const workers = 8
	const iterations = 2000
	done := make(chan struct{})
	for w := 0; w < workers; w++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for i := 0; i < iterations; i++ {
				if got := decodeRunOutputLine(gbkChinese, true); got != "中文测试" {
					t.Errorf("并发解码结果错误: %q", got)
					return
				}
				// 混合合法 UTF-8/ASCII 行，确保这些不被转码破坏
				if got := decodeRunOutputLine([]byte("npm run dev"), true); got != "npm run dev" {
					t.Errorf("并发 ASCII 行被改写: %q", got)
					return
				}
			}
		}()
	}
	for w := 0; w < workers; w++ {
		<-done
	}
}

// TestLightStatusSnapshotNoLogs 验证 LightStatusSnapshot 不携带日志（避免 N×200 拷贝），
// 且其余精简字段与 StatusSnapshot 语义一致。直接操作 runner 状态（hold pr.mu）注入，
// 不启动真实进程。
func TestLightStatusSnapshotNoLogs(t *testing.T) {
	pr := newProjectRunner("p1", "", "", nil, func(LogEntry) {})
	now := time.Now()
	pr.mu.Lock()
	pr.status = RunStatusRunning
	pr.startedAt = now
	pr.pid = 4242
	pr.hasExitCode = false
	pr.mu.Unlock()

	// 写入一条日志，验证 LightStatusSnapshot 不拷贝它。
	pr.emitLog(LogEntry{Timestamp: now, Stream: "stdout", Text: "hello"})

	light := pr.LightStatusSnapshot()
	if light.RecentLogs != nil {
		t.Fatalf("LightStatusSnapshot.RecentLogs 应为空，得到 %d 条", len(light.RecentLogs))
	}
	if light.Status != RunStatusRunning {
		t.Fatalf("LightStatusSnapshot.Status = %q, want running", light.Status)
	}
	if light.PID == nil || *light.PID != 4242 {
		t.Fatalf("LightStatusSnapshot.PID = %v, want 4242", light.PID)
	}
	if light.StartedAt == nil || !light.StartedAt.Equal(now) {
		t.Fatalf("LightStatusSnapshot.StartedAt = %v, want %v", light.StartedAt, now)
	}

	// running 态下退出码不应有值。
	if light.ExitCode != nil {
		t.Fatalf("running 态下 ExitCode 应为 nil, 得到 %v", *light.ExitCode)
	}

	// failed 态：PID 应清空、退出码应有值。
	pr.mu.Lock()
	pr.status = RunStatusFailed
	pr.hasExitCode = true
	pr.exitCode = 7
	pr.mu.Unlock()
	light = pr.LightStatusSnapshot()
	if light.Status != RunStatusFailed {
		t.Fatalf("failed 态 Status = %q", light.Status)
	}
	if light.PID != nil {
		t.Fatalf("failed 态 PID 应为 nil（进程已崩）, 得到 %v", *light.PID)
	}
	if light.ExitCode == nil || *light.ExitCode != 7 {
		t.Fatalf("failed 态 ExitCode = %v, want 7", light.ExitCode)
	}
}

// TestSetStatusListenerFires 验证 setStatusListener 注册的回调在状态翻转时触发。
// stopping → running 状态序列各触发一次且事件字段正确。
func TestSetStatusListenerFires(t *testing.T) {
	pr := newProjectRunner("p1", "", "", nil, func(LogEntry) {})
	events := make([]RunStatusEvent, 0, 3)
	pr.setStatusListener(func(e RunStatusEvent) { events = append(events, e) })

	// 手动模拟 stop() 置 stopping 后的 emitStatus 位。
	pr.mu.Lock()
	pr.status = RunStatusStopping
	pr.mu.Unlock()
	pr.emitStatus(RunStatusEvent{ProjectID: pr.projectID, Status: RunStatusStopping})
	if len(events) != 1 || events[0].Status != RunStatusStopping {
		t.Fatalf("stopping 回调 = %v, want 1 个 stopping 事件", events)
	}
	if events[0].ProjectID != "p1" {
		t.Fatalf("事件 ProjectID = %q, want p1", events[0].ProjectID)
	}

	// 模拟 wait() 置 stopped 后的 emitStatus 位。
	pr.mu.Lock()
	pr.status = RunStatusStopped
	pr.mu.Unlock()
	pr.emitStatus(RunStatusEvent{ProjectID: pr.projectID, Status: RunStatusStopped})
	if len(events) != 2 || events[1].Status != RunStatusStopped {
		t.Fatalf("stopped 回调 = %v, want 2 个事件且末尾为 stopped", events)
	}
}

// TestSetStatusListenerNoListener 未注册监听器时 emitStatus 为安全 no-op（不 panic）。
func TestSetStatusListenerNoListener(t *testing.T) {
	pr := newProjectRunner("p1", "", "", nil, func(LogEntry) {})
	pr.emitStatus(RunStatusEvent{ProjectID: "p1", Status: RunStatusRunning}) // 不应 panic
}

// TestListProjectProcessStatuses 验证批量进程状态端点：有 runner 的项目用 LightStatusSnapshot
// 精简取值（含 PID/StartedAt），无 runner 的项目缺省 stopped；断言的响应字段与前端契约一致。
func TestListProjectProcessStatuses(t *testing.T) {
	server := newTestServer(t)
	now := time.Now()
	for _, id := range []string{"project-running", "project-idle"} {
		if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values (?,?,?,?,?,1,?)`, id, "name-"+id, t.TempDir(), "wsl-local", "main", now); err != nil {
			t.Fatalf("insert project %s: %v", id, err)
		}
	}

	// 为 project-running 创建 runner 并注入 running 态（不启动真实进程）。
	pr := newProjectRunner("project-running", "", "", nil, func(LogEntry) {})
	pr.mu.Lock()
	pr.status = RunStatusRunning
	pr.startedAt = now
	pr.pid = 9000
	pr.mu.Unlock()
	server.runManagersMu.Lock()
	server.runManagers["project-running"] = pr
	server.runManagersMu.Unlock()

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/projects/processes/statuses", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", response.Code, response.Body.String())
	}
	var items []projectProcessStatusItem
	if err := json.Unmarshal(response.Body.Bytes(), &items); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("item count = %d, want 2 (%+v)", len(items), items)
	}
	var running, idle *projectProcessStatusItem
	for i := range items {
		switch items[i].ID {
		case "project-running":
			running = &items[i]
		case "project-idle":
			idle = &items[i]
		}
	}
	if running == nil || running.RunStatus != RunStatusRunning {
		t.Fatalf("project-running item = %+v, want running", running)
	}
	if running.RunPID == nil || *running.RunPID != 9000 {
		t.Fatalf("project-running RunPID = %v, want 9000", running.RunPID)
	}
	if running.RunStartedAt == nil || !running.RunStartedAt.Equal(now) {
		t.Fatalf("project-running RunStartedAt = %v, want %v", running.RunStartedAt, now)
	}
	if idle == nil || idle.RunStatus != RunStatusStopped {
		t.Fatalf("project-idle item = %+v, want stopped", idle)
	}
	if idle.RunPID != nil {
		t.Fatalf("project-idle RunPID = %v, want nil", *idle.RunPID)
	}
}

// TestListProjectProcessStatusesEmptyDB 无项目时端点返回空数组而非 null。
func TestListProjectProcessStatusesEmptyDB(t *testing.T) {
	server := newTestServer(t)
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/projects/processes/statuses", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	if body := response.Body.String(); body != "[]\n" {
		t.Fatalf("empty DB body = %q, want []", body)
	}
}

// TestProcessStatusWebSocketBroadcastContract 用真实 gorilla/websocket 客户端连接 /ws/processes，
// 触发一次 broadcastProcessStatus，读取一帧并校验线上 JSON 字段名（projectId/status）与
// 语义 —— 这是前端 /ws/processes 解析依赖的契约，防止 json tag 改名而测试假绿。
func TestProcessStatusWebSocketBroadcastContract(t *testing.T) {
	server := newTestServer(t)
	httpServer := httptest.NewServer(server.routes())
	t.Cleanup(httpServer.Close)

	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/processes"
	connection, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Origin": []string{httpServer.URL}})
	if err != nil {
		t.Fatalf("dial /ws/processes: %v", err)
	}
	defer connection.Close()

	// 等待订阅者注册，再广播，确保帧被本连接收到。
	deadline := time.Now().Add(2 * time.Second)
	for {
		server.processStatusSubMu.Lock()
		registered := len(server.processStatusSubs) == 1
		server.processStatusSubMu.Unlock()
		if registered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("process status WebSocket was not registered")
		}
		time.Sleep(10 * time.Millisecond)
	}

	pid := 4242
	server.broadcastProcessStatus(RunStatusEvent{ProjectID: "proj-1", Status: RunStatusRunning, StartedAt: &time.Time{}, PID: &pid})

	if err := connection.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	_, data, err := connection.ReadMessage()
	if err != nil {
		t.Fatalf("read process status frame: %v", err)
	}
	// 用原始 map 断言线上字段名，而非复用 Go struct（避免 struct→json tag 隐式推断掩盖改名）。
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("decode frame %s: %v", data, err)
	}
	if raw["projectId"] != "proj-1" {
		t.Fatalf("frame projectId = %v, want proj-1 (线上字段名必须是 projectId)", raw["projectId"])
	}
	if raw["status"] != "running" {
		t.Fatalf("frame status = %v, want running", raw["status"])
	}
	if _, exists := raw["pid"]; !exists || raw["pid"] != float64(4242) {
		t.Fatalf("frame pid = %v, want 4242", raw["pid"])
	}
}

// TestProcessStatusWebSocketSnapshotForRunningProject 校验连接建立后的全量快照帧：
// running 项目带 pid，stopped 项目不带 pid 且 status=stopped（REST/WS 契约的上线侧）。
func TestProcessStatusWebSocketSnapshotForRunningProject(t *testing.T) {
	server := newTestServer(t)
	now := time.Now()
	for _, id := range []string{"ps-on", "ps-off"} {
		if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values (?,?,?,?,?,1,?)`, id, "name-"+id, t.TempDir(), "wsl-local", "main", now); err != nil {
			t.Fatalf("insert project %s: %v", id, err)
		}
	}
	running := newProjectRunner("ps-on", "", "", nil, func(LogEntry) {})
	running.mu.Lock()
	running.status = RunStatusRunning
	running.startedAt = now
	running.pid = 555
	running.mu.Unlock()
	server.runManagersMu.Lock()
	server.runManagers["ps-on"] = running
	server.runManagersMu.Unlock()

	httpServer := httptest.NewServer(server.routes())
	t.Cleanup(httpServer.Close)
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/processes"
	connection, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Origin": []string{httpServer.URL}})
	if err != nil {
		t.Fatalf("dial /ws/processes: %v", err)
	}
	defer connection.Close()

	// 连接即推全量快照：按项目顺序读到 running 与 stopped 两帧。
	frames := map[string]map[string]any{}
	if err := connection.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	for len(frames) < 2 {
		_, data, err := connection.ReadMessage()
		if err != nil {
			t.Fatalf("read snapshot frame (got %d): %v", len(frames), err)
		}
		var raw map[string]any
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatalf("decode %s: %v", data, err)
		}
		id, _ := raw["projectId"].(string)
		frames[id] = raw
	}
	on := frames["ps-on"]
	if on == nil || on["status"] != "running" {
		t.Fatalf("ps-on frame = %v, want running", on)
	}
	if pid, _ := on["pid"].(float64); pid != 555 {
		t.Fatalf("ps-on pid = %v, want 555", on["pid"])
	}
	off := frames["ps-off"]
	if off == nil || off["status"] != "stopped" {
		t.Fatalf("ps-off frame = %v, want stopped", off)
	}
	if _, exists := off["pid"]; exists {
		t.Fatalf("ps-off 不应携带 pid（stopped 清空），实际 = %v", off["pid"])
	}
}


