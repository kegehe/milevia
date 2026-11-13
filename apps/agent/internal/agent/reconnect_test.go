package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// 退避节奏用缩小后的时长验证：判据是"等待是否回到最短档"，与真实秒数无关。
var testReconnectPolicy = reconnectPolicy{
	min:         20 * time.Millisecond,
	max:         400 * time.Millisecond,
	stableAfter: 100 * time.Millisecond,
}

// recorder 记录每次连接尝试的起始时刻，供断言相邻两次尝试的间隔。
type recorder struct {
	mu       sync.Mutex
	attempts []time.Time
}

func (r *recorder) wrap(connect connectFunc) connectFunc {
	return func(ctx context.Context) (time.Duration, error) {
		r.mu.Lock()
		r.attempts = append(r.attempts, time.Now())
		r.mu.Unlock()
		return connect(ctx)
	}
}

func (r *recorder) gaps() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	gaps := make([]time.Duration, 0, len(r.attempts)-1)
	for i := 1; i < len(r.attempts); i++ {
		gaps = append(gaps, r.attempts[i].Sub(r.attempts[i-1]))
	}
	return gaps
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.attempts)
}

func runLoop(t *testing.T, ctx context.Context, connect connectFunc) *recorder {
	t.Helper()
	rec := &recorder{}
	done := make(chan error, 1)
	go func() { done <- New(Config{}).runReconnectLoop(ctx, rec.wrap(connect), testReconnectPolicy) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reconnect loop returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reconnect loop did not stop: the backoff never came back down")
	}
	return rec
}

// 连续失败会把等待推到上限；一次真正建立并稳定存活的连接之后，下一次掉线必须
// 立刻回到最短等待，而不是继续沿用上一轮的上限。
func TestReconnectLoopResetsBackoffAfterStableConnection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	calls := 0
	connect := func(context.Context) (time.Duration, error) {
		mu.Lock()
		calls++
		call := calls
		mu.Unlock()
		switch {
		case call <= 6:
			// 拨号失败：连接从未建立。
			return 0, errors.New("dial failed")
		case call == 7:
			// 真正建立并稳定存活，随后被网络打断。
			return testReconnectPolicy.stableAfter, errors.New("read failed")
		default:
			cancel()
			return 0, context.Canceled
		}
	}
	rec := runLoop(t, ctx, connect)
	gaps := rec.gaps()
	if len(gaps) < 7 {
		t.Fatalf("attempts=%d, want at least 8", rec.count())
	}
	// 前六次失败已经把等待推高：第 6 次失败后应接近上限（20→40→80→160→320→400ms）。
	if gaps[5] < testReconnectPolicy.max/2 {
		t.Fatalf("backoff before the stable connection=%v, want >= %v (the failures did not grow the wait)", gaps[5], testReconnectPolicy.max/2)
	}
	// 稳定连接断开之后，等待必须回到最短档而不是上限。
	if gaps[6] > 5*testReconnectPolicy.min || gaps[6] >= testReconnectPolicy.max/2 {
		t.Fatalf("wait after the stable connection=%v, want about %v (backoff was not reset)", gaps[6], testReconnectPolicy.min)
	}
}

// 连接刚建立就被断开（存活不到 stableAfter）不算恢复，否则两个进程抢同一条
// 连接会退化成每秒一次的互相踢。这条不是本次 bug 的回归测试（旧实现同样只增
// 不减），它守住的是"复位不能过头"。
func TestReconnectLoopKeepsGrowingBackoffForFlappingConnections(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	calls := 0
	connect := func(context.Context) (time.Duration, error) {
		mu.Lock()
		calls++
		call := calls
		mu.Unlock()
		if call > 4 {
			cancel()
			return 0, context.Canceled
		}
		// 连上就断：存活时间远小于 stableAfter。
		return 5 * time.Millisecond, errors.New("connection closed by peer")
	}
	gaps := runLoop(t, ctx, connect).gaps()
	if len(gaps) < 4 {
		t.Fatalf("gaps=%d, want at least 4", len(gaps))
	}
	if gaps[0] > 3*testReconnectPolicy.min {
		t.Fatalf("first wait=%v, want about %v", gaps[0], testReconnectPolicy.min)
	}
	if gaps[3] < 4*testReconnectPolicy.min {
		t.Fatalf("wait after repeated flapping=%v, want >= %v (backoff must still grow)", gaps[3], 4*testReconnectPolicy.min)
	}
}

// runConnection 必须把"连接建立后活了多久"报给重连循环：拨号耗时不能算进去，
// 否则一次挂住几十秒的拨号会被误判成链路恢复。握手阶段慢一点即可识别——把计时
// 起点放在拨号之前，报出的时长就会超过握手的耗时。
func TestRunConnectionReportsEstablishedDuration(t *testing.T) {
	upgrader := websocket.Upgrader{}
	handshake := 500 * time.Millisecond
	hold := 100 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 握手本身要花的时间属于"拨号"，不属于"连接存活"。
		time.Sleep(handshake)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		time.Sleep(hold)
		_ = conn.Close()
	}))
	defer server.Close()
	agent := New(Config{
		CloudURL:   "ws" + strings.TrimPrefix(server.URL, "http"),
		InstanceID: "instance",
		CloudToken: "token",
		// 本地控制服务不可达，publish/status 只会打日志，不影响连接时长。
		LocalURL: "http://127.0.0.1:1",
	})
	established, err := agent.runConnection(context.Background())
	if err == nil {
		t.Fatal("runConnection returned no error after the peer closed the connection")
	}
	if established < hold {
		t.Fatalf("established duration=%v, want >= %v", established, hold)
	}
	if established >= handshake {
		t.Fatalf("established duration=%v, want < %v: the dial/handshake time leaked into the connection lifetime", established, handshake)
	}
}
