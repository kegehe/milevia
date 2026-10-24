package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

// 登录态读数（agent_auth.go）的用例。它们钉四件事：
//
//  1. 三态：读不到 ≠ 未登录 —— 后者会把一个已登录的用户指去再登录一次；
//  2. 缓存：TTL 内只探一次（页面进入就会问一遍，反复进出不该反复拉起 node）；
//  3. force：手动「检查登录状态」必须真的重探；
//  4. 在线格式上，判断不了要说判断不了（authStatus=unreadable），而 loggedIn=false
//     只是给旧客户端留的兼容位，不许被新界面当成"未登录"。

// fakeAuthRunner 是会计数的登录态替身。
type fakeAuthRunner struct {
	mu       sync.Mutex
	calls    int
	state    agentAuthState
	err      error
	started  chan struct{}
	release  chan struct{}
	blockOne sync.Once
}

func (r *fakeAuthRunner) Ready(context.Context) bool { return true }
func (r *fakeAuthRunner) Run(context.Context, AgentRunRequest, AgentRunSink) error {
	return nil
}
func (r *fakeAuthRunner) Version(context.Context) string { return "1.0.0" }
func (r *fakeAuthRunner) CheckUpdate(context.Context) (bool, string, error) {
	return false, "", nil
}
func (r *fakeAuthRunner) Update(context.Context) (string, string, error) { return "", "", nil }

func (r *fakeAuthRunner) LoginStatus(context.Context) (agentAuthState, error) {
	if r.started != nil {
		r.blockOne.Do(func() { close(r.started) })
		<-r.release
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return r.state, r.err
}

func (r *fakeAuthRunner) probeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func TestAgentAuthStateFromStatusOutputIsThreeWay(t *testing.T) {
	markers := []string{"0 credentials", "No credentials", "not logged in"}
	if got := agentAuthStateFromStatusOutput("0 credentials\n", markers); got != agentAuthLoggedOut {
		t.Fatalf("命中空标记应读成未登录，得到 %q", got)
	}
	if got := agentAuthStateFromStatusOutput("OpenCode Zen  api_key\n", markers); got != agentAuthLoggedIn {
		t.Fatalf("命令成功且没有空标记应读成已登录，得到 %q", got)
	}
	// 空输出是**判断不了**：既不冒充未登录，也不冒充已登录。
	if got := agentAuthStateFromStatusOutput("   \n", markers); got != agentAuthUnreadable {
		t.Fatalf("空输出只能报 unreadable，得到 %q", got)
	}
}

func TestAgentAuthReadingIsCachedAndForceReprobes(t *testing.T) {
	entry, _ := agentByID("qoder")
	runner := &fakeAuthRunner{state: agentAuthLoggedIn}
	server := &Server{}
	ctx := context.Background()

	first := server.agentAuthFor(ctx, "runner-a", entry, runner, false)
	if first.state != agentAuthLoggedIn || !first.readable() {
		t.Fatalf("第一次读数不对：%+v", first)
	}
	if runner.probeCount() != 1 {
		t.Fatalf("第一次读数没有真的探测：%d", runner.probeCount())
	}

	// TTL 内直接回放：不再为了一次读数去拉起 qoder status（冷启动 7.5s）。
	if cached := server.agentAuthFor(ctx, "runner-a", entry, runner, false); cached.state != agentAuthLoggedIn {
		t.Fatalf("缓存回放的读数不对：%+v", cached)
	}
	if runner.probeCount() != 1 {
		t.Fatalf("TTL 内又探了一遍（%d）—— 缓存没生效", runner.probeCount())
	}

	// 手动「检查登录状态」带 force：必须真的重探，并把新结论写回缓存。
	runner.mu.Lock()
	runner.state = agentAuthLoggedOut
	runner.mu.Unlock()
	refreshed := server.agentAuthFor(ctx, "runner-a", entry, runner, true)
	if refreshed.state != agentAuthLoggedOut {
		t.Fatalf("force 之后没读到新结论：%+v", refreshed)
	}
	if runner.probeCount() != 2 {
		t.Fatalf("force 没有真的重探：%d", runner.probeCount())
	}
	if again := server.agentAuthFor(ctx, "runner-a", entry, runner, false); again.state != agentAuthLoggedOut {
		t.Fatalf("force 的新读数没有写回缓存：%+v", again)
	}

	// 另一台机器上的同名工具是**另一条读数**：登录态是逐机器的。
	if other := server.agentAuthFor(ctx, "runner-b", entry, &fakeAuthRunner{state: agentAuthLoggedIn}, false); other.state != agentAuthLoggedIn {
		t.Fatalf("换一台机器读到了别的读数：%+v", other)
	}
}

// TestAgentAuthInvalidatedAfterWriteOperation 钉住：装/升/修之后这份读数跟着作废。
// 否则用户刚修好一个工具，卡片上还挂着修复之前读到的登录态。
func TestAgentAuthInvalidatedAfterWriteOperation(t *testing.T) {
	entry, _ := agentByID("qoder")
	runner := &fakeAuthRunner{state: agentAuthLoggedIn}
	server := &Server{}
	ctx := context.Background()

	server.agentAuthFor(ctx, "runner-a", entry, runner, false)
	server.agentAuthFor(ctx, "runner-b", entry, runner, false)
	server.invalidateAgentReadings("runner-a")

	// runner-a 的读数被丢掉 ⇒ 再读会重新探测；runner-b 不受影响。
	server.agentAuthFor(ctx, "runner-a", entry, runner, false)
	if got := runner.probeCount(); got != 3 {
		t.Fatalf("写操作之后没有重新探测：%d 次（期望 3）", got)
	}
	server.agentAuthFor(ctx, "runner-b", entry, runner, false)
	if got := runner.probeCount(); got != 3 {
		t.Fatalf("另一台机器的缓存被误删了：%d 次（期望 3）", got)
	}
}

func TestAgentAuthSingleFlightProbesOnce(t *testing.T) {
	entry, _ := agentByID("qoder")
	runner := &fakeAuthRunner{
		state:   agentAuthLoggedIn,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	server := &Server{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	results := make(chan agentAuthReading, 2)
	go func() { results <- server.agentAuthFor(ctx, "runner-a", entry, runner, false) }()
	<-runner.started
	go func() { results <- server.agentAuthFor(ctx, "runner-a", entry, runner, false) }()

	// 第二个调用者必须等在飞的那一轮上，而不是再拉起一个 node 进程。
	time.Sleep(50 * time.Millisecond)
	close(runner.release)
	first, second := <-results, <-results
	if first.state != agentAuthLoggedIn || second.state != agentAuthLoggedIn {
		t.Fatalf("等待者拿到的读数不对：%+v / %+v", first, second)
	}
	if runner.probeCount() != 1 {
		t.Fatalf("并发两次只该探一次，实际 %d 次", runner.probeCount())
	}
}

func TestAgentAuthFailedProbeUsesShortTTL(t *testing.T) {
	entry, _ := agentByID("qoder")
	runner := &fakeAuthRunner{state: agentAuthUnreadable, err: errors.New("status 超时")}
	server := &Server{}
	read := server.agentAuthFor(context.Background(), "runner-a", entry, runner, false)
	if read.readable() {
		t.Fatalf("失败不该产出结论：%+v", read)
	}
	if !read.transient || !strings.Contains(read.note, "status 超时") {
		t.Fatalf("失败读数必须带原因且按短 TTL 保鲜：%+v", read)
	}
	if agentAuthTTLFor(read) != agentAuthFailureTTL {
		t.Fatalf("失败读数的 TTL 应是 %s，实际 %s", agentAuthFailureTTL, agentAuthTTLFor(read))
	}
	// 工具本来就不能自报（codebuddy 那一档）不是"这次没读成"，用常规 TTL。
	stable := agentAuthReading{state: agentAuthUnreadable, note: "无法自动判断"}
	if agentAuthTTLFor(stable) != agentAuthTTL {
		t.Fatalf("稳定结论的 TTL 应是 %s，实际 %s", agentAuthTTL, agentAuthTTLFor(stable))
	}
}

// TestLoginStatusWireKeepsUnreadableOutOfLoggedOut 走完整 HTTP 链路（远端 + 目录驱动的
// 后端），钉住"判断不了"在**线上格式**里也是一个独立的取值。
func TestLoginStatusWireKeepsUnreadableOutOfLoggedOut(t *testing.T) {
	shell := newFakeCrossShell()
	runner := &crossShellOnlyRunner{shell}
	server := &Server{
		runnerRegistry: newRunnerRegistry(),
		runtimeCtx:     context.Background(),
		runnerUpdating: map[runnerAgentKey]bool{},
	}
	meta := RunnerMeta{ID: "remote-auth", Name: "远端", Environment: "remote-linux"}
	server.runnerRegistry.register(meta.ID, runner, meta)

	call := func(force bool) map[string]any {
		path := "/api/runners/remote-auth/agents/opencode/login-status"
		if force {
			path += "?force=true"
		}
		request := httptest.NewRequest(http.MethodGet, path, nil)
		// 两个参数必须塞进**同一个**路由上下文：withURLParam 每次都会新建一个，
		// 连用两次会把前一个参数覆盖掉（runnerID 变空 ⇒ 落到本机分支）。
		routeCtx := chi.NewRouteContext()
		routeCtx.URLParams.Add("runnerID", "remote-auth")
		routeCtx.URLParams.Add("agentID", "opencode")
		request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeCtx))
		recorder := httptest.NewRecorder()
		server.agentLoginStatus(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("login-status 返回 %d：%s", recorder.Code, recorder.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("解析响应失败：%v", err)
		}
		return body
	}

	shell.outputs["opencode"] = "0 credentials\n"
	if body := call(false); body["authStatus"] != agentAuthLoggedOut || body["loggedIn"] != false {
		t.Fatalf("空标记应读成未登录：%v", body)
	}
	shell.outputs["opencode"] = "OpenCode Zen  api_key\n"
	if body := call(true); body["authStatus"] != agentAuthLoggedIn || body["loggedIn"] != true {
		t.Fatalf("有凭据应读成已登录：%v", body)
	}
	// 命令跑不起来：判断不了，而 loggedIn 这个兼容位仍然是 false —— 新界面读的是
	// authStatus，所以它不会把这句 false 念成"未登录"。
	shell.failing["opencode"] = true
	body := call(true)
	if body["authStatus"] != agentAuthUnreadable {
		t.Fatalf("读不到时必须报 unreadable：%v", body)
	}
	if note, _ := body["note"].(string); !strings.Contains(note, "opencode") {
		t.Fatalf("读不到时要带可读原因：%v", body)
	}
}

func TestLoginStatusRejectsToolsOutsidePlatformLogin(t *testing.T) {
	server := &Server{runtimeCtx: context.Background()}
	request := httptest.NewRequest(http.MethodGet, "/api/runners/windows-local/agents/claude-code/login-status", nil)
	request = withURLParam(withURLParam(request, "runnerID", "windows-local"), "agentID", "claude-code")
	recorder := httptest.NewRecorder()
	server.agentLoginStatus(recorder, request)
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("不支持平台内登录的工具应回 501，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
}
