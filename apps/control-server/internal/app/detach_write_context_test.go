package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// detachWriteContext 的判据：**写操作不因客户端挂断而中止**，以及三条不许脱开的边界。
//
// 为什么这类改动必须有反例：把 ctx 脱开的写法看起来都一样，而脱错方向的后果是
// "客户端断开了，服务端的等待还挂在那里" —— 没有测试的话，下一轮把 GET 也一起脱开
// （或者把 /api/internal/ 一起脱开）不会有任何东西报红。

// middlewareProbe 让处理器把自己看到的 ctx 交出来，并阻塞到测试放行为止 ——
// 模拟"正在跑的一条 git 命令 / 一次 npm 安装"。
type middlewareProbe struct {
	seen          context.Context
	release       func()
	cancelRequest context.CancelFunc
}

func observeThroughMiddleware(t *testing.T, server *Server, method, path string) middlewareProbe {
	t.Helper()
	seenCh := make(chan context.Context, 1)
	releaseCh := make(chan struct{})
	handler := server.detachWriteContext(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenCh <- r.Context()
		<-releaseCh
		w.WriteHeader(http.StatusNoContent)
	}))
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, path, nil).WithContext(requestCtx))
	}()
	seen := <-seenCh
	return middlewareProbe{
		seen:          seen,
		cancelRequest: cancelRequest,
		release: func() {
			close(releaseCh)
			<-done
		},
	}
}

func newDetachTestServer() (*Server, context.CancelFunc) {
	runtimeCtx, stopRuntime := context.WithCancel(context.Background())
	return &Server{runtimeCtx: runtimeCtx}, stopRuntime
}

func TestDetachWriteContextKeepsWriteAliveAfterClientDisconnect(t *testing.T) {
	server, stopRuntime := newDetachTestServer()
	defer stopRuntime()
	probe := observeThroughMiddleware(t, server, http.MethodPost, "/api/projects/p1/git/push")

	// 浏览器断开（切走 / 关页面 / 自己的请求超时 abort）。
	probe.cancelRequest()
	// 取消是**同步**传播的：真接到请求 ctx 上时，这一句之后 Err() 立刻非 nil。
	// 所以这里不需要 sleep，看见 nil 就是真的脱开了。
	if err := probe.seen.Err(); err != nil {
		t.Fatalf("客户端断开把写操作也取消了（%v）—— git push / npm 安装会在服务端被杀在半途", err)
	}
	probe.release()
}

func TestDetachWriteContextStillFollowsServerShutdown(t *testing.T) {
	server, stopRuntime := newDetachTestServer()
	probe := observeThroughMiddleware(t, server, http.MethodPost, "/api/projects/p1/git/push")
	defer probe.release()

	// 脱开的是**客户端连接**，不是服务端生命周期。
	stopRuntime()
	deadline := time.After(5 * time.Second)
	for probe.seen.Err() == nil {
		select {
		case <-deadline:
			t.Fatal("服务端关闭时，挂起的写操作没有被取消（ctx 挂错了根）")
		case <-time.After(2 * time.Millisecond):
		}
	}
}

func TestDetachWriteContextLeavesReadsOnTheConnection(t *testing.T) {
	// GET 里混着长轮询（/api/remote/outbox?wait=）与文件下载，它们的生命周期**本来就**
	// 等于连接：客户端走了它们就该结束，脱开只会留下没人要的 goroutine。
	server, stopRuntime := newDetachTestServer()
	defer stopRuntime()
	probe := observeThroughMiddleware(t, server, http.MethodGet, "/api/remote/outbox?wait=1")

	probe.cancelRequest()
	if err := probe.seen.Err(); err == nil {
		t.Fatal("GET 被脱开了 —— 长轮询与下载会挂到服务端自己的超时为止")
	}
	probe.release()
}

func TestDetachWriteContextLeavesApprovalWaitOnTheConnection(t *testing.T) {
	// /api/internal/approvals/wait 是刻意挂在连接上的 5 分钟长轮询：
	// "客户端断开 → deny" 正是它的语义（审批钩子进程一死，那次工具调用就该被判拒绝）。
	server, stopRuntime := newDetachTestServer()
	defer stopRuntime()
	probe := observeThroughMiddleware(t, server, http.MethodPost, "/api/internal/approvals/wait")

	probe.cancelRequest()
	if err := probe.seen.Err(); err == nil {
		t.Fatal("审批长轮询被脱开了 —— 被取消的会话会一直挂在 approvals 里等一个不会来的裁决")
	}
	probe.release()
}

// 上面四条验的是中间件本身。这一条验的是**它真的接在路由链上** ——
// 中间件写对了却没挂进 routes()，不会有任何东西报红。
//
// 探针用 POST /api/notifications/dismiss-all：它是一条真实的写，且直接拿
// `r.Context()` 去 ExecContext（notification.go）。所以"请求 ctx 已经取消"这件事
// 在它身上是可观察的：接着请求 ctx 时它必然 500，脱开之后它照常 200。
func TestWritesThroughRealRouterSurviveAnAlreadyCancelledRequestContext(t *testing.T) {
	server := newTestServer(t)
	cancelledRequest := func() *http.Request {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return httptest.NewRequest(http.MethodPost, "/api/notifications/dismiss-all", strings.NewReader("{}")).WithContext(ctx)
	}

	// ① 探针是灵敏的：直接调处理器（不经过路由链），取消过的 ctx 必然让它失败。
	direct := httptest.NewRecorder()
	server.dismissAllNotifications(direct, cancelledRequest())
	if direct.Code == http.StatusOK {
		t.Fatal("探针不灵敏：dismiss-all 在 ctx 已取消时仍然成功，这一条验不出任何东西")
	}

	// ② 经过真实路由链：写操作照样完成。
	routed := httptest.NewRecorder()
	server.routes().ServeHTTP(routed, cancelledRequest())
	if routed.Code != http.StatusOK {
		t.Fatalf("写操作没被接进 detachWriteContext（状态 %d，body %s）—— 客户端一断开，"+
			"服务端的 git push / npm 安装就会被杀在半途", routed.Code, routed.Body.String())
	}
}

// 手工构造的测试 Server（`&Server{db: db}` 这种）没有 runtimeCtx，而
// `context.AfterFunc(nil, …)` 是**空指针崩溃** —— 崩在中间件里，报错完全指不到原因。
// 这一条钉住"没有 runtimeCtx 时如实退回不脱开"，而不是炸掉一个请求。
func TestDetachWriteContextToleratesServerWithoutRuntimeContext(t *testing.T) {
	bare := &Server{} // 没有 runtimeCtx
	seen := make(chan context.Context, 1)
	handler := bare.detachWriteContext(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Context()
		w.WriteHeader(http.StatusNoContent)
	}))
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/projects/p1/git/push", nil).WithContext(requestCtx))
	cancelRequest()
	if got := <-seen; got.Err() == nil {
		t.Fatal("没有 runtimeCtx 时不该脱开：脱开就失去了唯一的取消来源")
	}
}
