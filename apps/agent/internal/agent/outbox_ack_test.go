package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordingLocalServer 记录本地中继 API 收到的每一次调用，用来断言"每条回执一次
// HTTP 请求"已经变成"成批一次"。
type recordingLocalServer struct {
	mu       sync.Mutex
	requests []recordedRequest
}

type recordedRequest struct {
	path string
	body map[string]any
}

func (s *recordingLocalServer) handler(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	s.requests = append(s.requests, recordedRequest{path: r.URL.Path, body: body})
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"acknowledged"}`))
}

func (s *recordingLocalServer) snapshot() []recordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]recordedRequest, len(s.requests))
	copy(out, s.requests)
	return out
}

func newAckQueueAgent(t *testing.T) (*Agent, *recordingLocalServer) {
	t.Helper()
	recorder := &recordingLocalServer{}
	server := httptest.NewServer(http.HandlerFunc(recorder.handler))
	t.Cleanup(server.Close)
	return &Agent{config: Config{LocalURL: server.URL}, client: &http.Client{Timeout: 5 * time.Second}}, recorder
}

func eventIDsOf(body map[string]any) []string {
	raw, _ := body["eventIds"].([]any)
	ids := make([]string, 0, len(raw))
	for _, item := range raw {
		if text, ok := item.(string); ok {
			ids = append(ids, text)
		}
	}
	return ids
}

// 一批回执必须用**一次**请求提交。老实现是一条 event.ack 一次 POST，事件洪峰下
// 读循环被这些请求占死，下行的命令挤不进来（真机命令往返 3s → 75s 的原因）。
func TestOutboxAckQueueFlushesInOneRequest(t *testing.T) {
	agent, recorder := newAckQueueAgent(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	queue := newOutboxAckQueue()
	done := make(chan struct{})
	go func() {
		defer close(done)
		agent.runOutboxAckQueue(ctx, queue)
	}()

	for _, id := range []string{"event-1", "event-2", "event-3"} {
		queue.enqueue(outboxAck{eventID: id})
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(recorder.snapshot()) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	requests := recorder.snapshot()
	if len(requests) != 1 {
		t.Fatalf("ack flush issued %d local requests, want 1 (batched)", len(requests))
	}
	if requests[0].path != "/api/remote/outbox/ack" {
		t.Fatalf("ack flush hit %s, want /api/remote/outbox/ack", requests[0].path)
	}
	ids := eventIDsOf(requests[0].body)
	if len(ids) != 3 {
		t.Fatalf("ack flush carried %d ids, want 3 (%v)", len(ids), ids)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the ack queue did not stop after the connection was cancelled")
	}
}

// 云端明确拒绝的事件必须走 outbox/fail 且 permanent=true，本地才会删行。
func TestOutboxAckQueueRoutesRejectionsToFail(t *testing.T) {
	agent, recorder := newAckQueueAgent(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	queue := newOutboxAckQueue()
	done := make(chan struct{})
	go func() {
		defer close(done)
		agent.runOutboxAckQueue(ctx, queue)
	}()

	queue.enqueue(outboxAck{eventID: "event-ok"})
	queue.enqueue(outboxAck{eventID: "event-bad", reason: "event insert conflict", drop: true})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(recorder.snapshot()) >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	var ackPath, failPath string
	for _, request := range recorder.snapshot() {
		switch request.path {
		case "/api/remote/outbox/ack":
			ackPath = request.path
			if ids := eventIDsOf(request.body); len(ids) != 1 || ids[0] != "event-ok" {
				t.Fatalf("ack batch = %v, want [event-ok]", ids)
			}
		case "/api/remote/outbox/fail":
			failPath = request.path
			if ids := eventIDsOf(request.body); len(ids) != 1 || ids[0] != "event-bad" {
				t.Fatalf("fail batch = %v, want [event-bad]", ids)
			}
			if permanent, _ := request.body["permanent"].(bool); !permanent {
				t.Fatalf("fail request was not permanent: %v", request.body)
			}
			if reason, _ := request.body["error"].(string); reason != "event insert conflict" {
				t.Fatalf("fail request reason = %q", reason)
			}
		default:
			t.Fatalf("unexpected local request %s", request.path)
		}
	}
	if ackPath == "" || failPath == "" {
		t.Fatalf("expected both an ack and a fail request, got %v", recorder.snapshot())
	}

	cancel()
	<-done
}

// 队列满了必须直接丢弃而不是阻塞 —— 它跑在 WebSocket 读循环里，阻塞一次就等于
// 让下行命令全部排队。丢掉是安全的：本地 outbox 行没被删掉，重推会带回同一回执。
func TestOutboxAckQueueEnqueueNeverBlocks(t *testing.T) {
	queue := newOutboxAckQueue()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for index := 0; index < outboxAckQueueSize*3; index++ {
			queue.enqueue(outboxAck{eventID: "event"})
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("enqueue blocked on a full ack queue; it must drop instead")
	}
}

// 连接结束时手上那一批要尽量交出去，不能白丢一轮。
func TestOutboxAckQueueFlushesPendingBatchOnShutdown(t *testing.T) {
	agent, recorder := newAckQueueAgent(t)

	ctx, cancel := context.WithCancel(context.Background())
	queue := newOutboxAckQueue()
	done := make(chan struct{})
	go func() {
		defer close(done)
		agent.runOutboxAckQueue(ctx, queue)
	}()

	queue.enqueue(outboxAck{eventID: "event-pending"})
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the ack queue did not stop after cancellation")
	}

	requests := recorder.snapshot()
	if len(requests) != 1 {
		t.Fatalf("shutdown flush issued %d requests, want 1", len(requests))
	}
	if ids := eventIDsOf(requests[0].body); len(ids) != 1 || ids[0] != "event-pending" {
		t.Fatalf("shutdown flush carried %v, want [event-pending]", ids)
	}
}

// 一批**原样退回来**的事件不能一直按固定 200ms 重推。老实现每秒重推同一批 5 次，
// 云端对每条重发再回一次 ack：真机一条连接 4.5 小时被灌了 792MB 下行。
func TestOutboxPumpBacksOffWhileTheSameBatchKeepsComingBack(t *testing.T) {
	var requests int32
	batch := []outboxItem{{
		EventID:       "event-1",
		AgentSequence: 1,
		Type:          "assistant.message",
		Payload:       json.RawMessage(`{"content":"hi"}`),
		CreatedAt:     time.Now().UTC(),
	}}
	agent := newOutboxPumpAgent(t, batch, &requests)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writeCh := make(chan any, 256)
	go func() {
		for range writeCh {
		}
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		agent.runOutboxPump(ctx, writeCh)
	}()

	// 头一秒钟**不许**退避：回执是攒 250ms 成批提交的，泵读第二遍时那批行必然还在，
	// 那是正常收发的重叠、不是卡住。按"重复了几轮"升级退避会在这里误伤 ——
	// 一次普通的收发就要白等几百毫秒。
	early := 1 * time.Second
	time.Sleep(early)
	afterEarly := atomic.LoadInt32(&requests)

	// 一秒之后才算真的卡住，开始翻倍拉长。总共 5 秒里，固定 200ms 会读 25 次左右。
	window := 5 * time.Second
	time.Sleep(window - early)
	cancel()
	<-done
	total := atomic.LoadInt32(&requests)

	if afterEarly < 4 {
		t.Fatalf("the pump issued only %d reads in the first %s; it backed off before the batch was actually stalled", afterEarly, early)
	}
	if total > 14 {
		t.Fatalf("the pump issued %d reads in %s while the same batch kept coming back; the backoff is not working", total, window)
	}
}

// 批次一旦变化（说明云端确认了），退避必须立刻归零，正常投递不受影响。
func TestOutboxPumpResetsBackoffWhenTheBatchChanges(t *testing.T) {
	var requests int32
	var mu sync.Mutex
	second := false
	batchA := []outboxItem{{EventID: "event-a", AgentSequence: 1, Type: "assistant.message", Payload: json.RawMessage(`{}`), CreatedAt: time.Now().UTC()}}
	batchB := []outboxItem{{EventID: "event-b", AgentSequence: 2, Type: "assistant.message", Payload: json.RawMessage(`{}`), CreatedAt: time.Now().UTC()}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		mu.Lock()
		current := batchA
		if second {
			current = batchB
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(current)
	}))
	t.Cleanup(server.Close)
	agent := &Agent{config: Config{LocalURL: server.URL}, client: &http.Client{Timeout: 5 * time.Second}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writeCh := make(chan any, 256)
	go func() {
		for range writeCh {
		}
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		agent.runOutboxPump(ctx, writeCh)
	}()

	// 先让第一批重复几轮把退避拉起来。
	time.Sleep(1200 * time.Millisecond)
	mu.Lock()
	second = true
	mu.Unlock()
	before := atomic.LoadInt32(&requests)
	// 批次变化后必须立即恢复快节奏：400ms 里至少应该读到一次。
	time.Sleep(400 * time.Millisecond)
	after := atomic.LoadInt32(&requests)
	cancel()
	<-done

	if after <= before {
		t.Fatalf("the pump did not re-read promptly after the batch changed (%d -> %d)", before, after)
	}
}
