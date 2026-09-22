package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newFSRelayAgent 造一个 Agent，把它的"本机 control-server"指向一个测试服务。
// 不走 New 的环境变量路径：测试要显式控制本地端点。
func newFSRelayAgent(t *testing.T, handler http.Handler) *Agent {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	agent := New(Config{InstanceID: "pc-1", CloudURL: "https://cloud.invalid", LocalURL: server.URL, LocalToken: "local-token"})
	return agent
}

// 帧上限必须**远小于**云端读 Agent 帧的硬上限（cloud-control 的 SetReadLimit 512 KiB）。
// 超了会让云端读失败并掐断整条中继连接 —— 代价不只是这次请求失败，而是这台电脑与
// 手机之间的命令、事件、快照一起断掉。把它钉成不变式，改大就红。
func TestRPCFrameLimitStaysUnderCloudReadLimit(t *testing.T) {
	const cloudReadLimit = 512 << 10
	if rpcFrameLimit >= cloudReadLimit {
		t.Fatalf("rpcFrameLimit = %d must stay well under the %d cloud read limit", rpcFrameLimit, cloudReadLimit)
	}
	// 它还要装得下 control-server 的内容上限（320 KiB）加上 JSON 转义与信封，
	// 否则一个合法的最大文件也会被这里挡掉。
	if rpcFrameLimit < 320<<10 {
		t.Fatalf("rpcFrameLimit = %d must still fit the 320 KiB content budget", rpcFrameLimit)
	}
}

func TestCallRPCRelayForwardsSuccessVerbatim(t *testing.T) {
	var received map[string]any
	agent := newFSRelayAgent(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/remote/rpc" {
			t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
		}
		if token := r.Header.Get("X-Milevia-Agent-Token"); token != "local-token" {
			t.Errorf("agent token = %q", token)
		}
		_ = json.NewDecoder(r.Body).Decode(&received)
		_, _ = w.Write([]byte(`{"ok":true,"status":200,"data":{"content":"hello","editable":true}}`))
	}))

	response := agent.callRPCRelay(context.Background(), rpcRequest{
		Kind: "rpc.request", RequestID: "rpc-1", Op: "fs.open", ProjectID: "p1", Params: json.RawMessage(`{"path":"a.txt"}`),
	})
	if !response.OK {
		t.Fatalf("response = %+v", response)
	}
	if response.Kind != "rpc.response" || response.RequestID != "rpc-1" {
		t.Fatalf("envelope = %+v", response)
	}
	if !strings.Contains(string(response.Data), `"content":"hello"`) {
		t.Fatalf("data = %s", response.Data)
	}
	if received["op"] != "fs.open" || received["projectId"] != "p1" {
		t.Fatalf("forwarded request = %v", received)
	}
}

// 业务失败（HTTP 200 + ok:false）必须**原样**带回状态码、那句中文以及稳定错误码：
// 手机端要据此区分"版本冲突 / lease 占用 / 参数错误"，并原样显示服务端写的提示。
// **码必须带过去**：那句文案会被服务端本地化，按文案分支的判据会静默失效。
func TestCallRPCRelayForwardsBusinessFailureVerbatim(t *testing.T) {
	agent := newFSRelayAgent(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"status":409,"error":"项目工作区正被其他 AI 任务或 Git 操作占用，请等待当前操作完成后重试。","code":"workspace_occupied"}`))
	}))
	response := agent.callRPCRelay(context.Background(), rpcRequest{RequestID: "rpc-2", Op: "fs.write", ProjectID: "p1"})
	if response.OK {
		t.Fatalf("response = %+v, want ok:false", response)
	}
	if response.Status != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (客户端要靠它区分几种失败)", response.Status)
	}
	if response.Error != "项目工作区正被其他 AI 任务或 Git 操作占用，请等待当前操作完成后重试。" {
		t.Fatalf("error = %q", response.Error)
	}
	if response.Code != "workspace_occupied" {
		t.Fatalf("code = %q, want the stable machine code passed through", response.Code)
	}
}

// 中继端点自身的问题（op 不支持、projectId 非法、params 类型错）走 HTTP 错误码。
// 文案要取出来一路带到手机上：只说"保存失败"等于让用户白跑一趟。
func TestCallRPCRelaySurfacesLocalHTTPErrorText(t *testing.T) {
	agent := newFSRelayAgent(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"params 必须是字符串键值对"}`))
	}))
	response := agent.callRPCRelay(context.Background(), rpcRequest{RequestID: "rpc-3", Op: "fs.tree", ProjectID: "p1"})
	if response.OK {
		t.Fatalf("response = %+v", response)
	}
	if response.Status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Status)
	}
	if response.Error != "params 必须是字符串键值对" {
		t.Fatalf("error = %q", response.Error)
	}
}

func TestCallRPCRelayReportsUnreachableLocalService(t *testing.T) {
	// 指向一个已经关掉的端口，模拟 Milevia 没在跑。
	agent := New(Config{InstanceID: "pc-1", LocalURL: "http://127.0.0.1:1"})
	agent.client = &http.Client{Timeout: 2 * time.Second}
	response := agent.callRPCRelay(context.Background(), rpcRequest{RequestID: "rpc-4", Op: "fs.open", ProjectID: "p1"})
	if response.OK {
		t.Fatalf("response = %+v", response)
	}
	// 这句话是给用户看的：要说清让他去做什么，不能只说"失败"。
	if !strings.Contains(response.Error, "Milevia") {
		t.Fatalf("error = %q, want it to tell the user to check the desktop app", response.Error)
	}
}

// 这是这条链路上最要紧的一条防线。本机返回的内容超过帧上限时，**绝不能**照发 ——
// 云端读帧超限会掐断整条中继连接，代价是这台电脑与手机之间的命令/事件/快照
// 一起断掉，直到重连。宁可换成一个明确的错误。
//
// 注意载荷大小是**刻意选的**：它必须大于帧上限、但小于内存兜底（2 倍帧上限），
// 这样唯一能挡住它的就是"量真正要发出去的那一帧"这条判据。
// 早先这个用例的载荷开到了帧上限本身、又落在内存兜底之内 —— 那次变异检验发现
// 把帧判据放宽 32 倍测试仍然通过：它实际命中的是内存兜底，帧判据根本没被走到。
// 断言上因此还专门盯住帧判据那句文案，避免两条闸门互相顶替。
func TestCallRPCRelayRefusesOversizedFrames(t *testing.T) {
	agent := newFSRelayAgent(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		oversized := strings.Repeat("x", rpcFrameLimit)
		_, _ = w.Write([]byte(`{"ok":true,"status":200,"data":{"content":"` + oversized + `"}}`))
	}))
	response := agent.callRPCRelay(context.Background(), rpcRequest{RequestID: "rpc-5", Op: "fs.open", ProjectID: "p1"})
	if response.OK {
		t.Fatalf("an oversized frame must not be forwarded: %+v", response)
	}
	if response.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", response.Status)
	}
	// 认准帧判据那句文案。两条闸门的文案不同，就是为了让"到底是谁挡下的"可断言。
	if !strings.Contains(response.Error, "中继通道的容量") {
		t.Fatalf("error = %q, want the frame-budget wording (not the memory backstop)", response.Error)
	}
	// 载荷必须真的被丢掉了，而不是"标记失败但仍然带着内容"。
	if len(response.Data) != 0 {
		t.Fatalf("the oversized payload was kept: %d bytes", len(response.Data))
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(encoded) > rpcFrameLimit {
		t.Fatalf("the failure response itself is %d bytes, over the %d limit", len(encoded), rpcFrameLimit)
	}
}

// 内存兜底：本机服务返回了连缓冲都不该建的响应。它防的是本机侧出问题，
// 与"用户文件太大"是两件事，所以文案必须是另一句 —— 让用户去电脑上"换个小的看"
// 是胡说，问题不在他的文件上。
func TestCallRPCRelayBackstopsAbsurdLocalResponses(t *testing.T) {
	agent := newFSRelayAgent(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"status":200,"data":{"content":"` + strings.Repeat("x", 2*rpcFrameLimit) + `"}}`))
	}))
	response := agent.callRPCRelay(context.Background(), rpcRequest{RequestID: "rpc-7", Op: "fs.open", ProjectID: "p1"})
	if response.OK {
		t.Fatalf("response = %+v", response)
	}
	if response.Status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (本机侧的问题，不是用户的文件问题)", response.Status)
	}
	if !strings.Contains(response.Error, "异常大的响应") {
		t.Fatalf("error = %q, want the memory-backstop wording", response.Error)
	}
}

// 读循环**绝不能**被中继请求阻塞：它一停，下行的命令就再也读不进来，整条链路静默。
// 并发满的时候要立刻回一句明确的"忙"，而不是在原地等一个空位。
func TestDispatchRPCRequestRepliesBusyInsteadOfBlocking(t *testing.T) {
	release := make(chan struct{})
	agent := newFSRelayAgent(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		_, _ = w.Write([]byte(`{"ok":true,"status":200,"data":{}}`))
	}))
	writeCh := make(chan any, 32)
	ctx := context.Background()

	// 先把并发闸门占满。
	for index := 0; index < rpcRequestConcurrency; index++ {
		agent.dispatchRPCRequest(ctx, rpcRequest{RequestID: "busy-" + string(rune('a'+index)), Op: "fs.open", ProjectID: "p1"}, writeCh)
	}
	// 再发一条：它必须**立刻**回一句忙，而不是等待。
	done := make(chan struct{})
	go func() {
		defer close(done)
		agent.dispatchRPCRequest(ctx, rpcRequest{RequestID: "overflow", Op: "fs.open", ProjectID: "p1"}, writeCh)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("dispatchRPCRequest blocked while the slots were full")
	}

	deadline := time.After(3 * time.Second)
	for {
		select {
		case message := <-writeCh:
			encoded, err := json.Marshal(message)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if bytes.Contains(encoded, []byte(`"requestId":"overflow"`)) {
				if !bytes.Contains(encoded, []byte(`"ok":false`)) {
					close(release)
					t.Fatalf("overflow response = %s, want ok:false", encoded)
				}
				if !bytes.Contains(encoded, []byte(`"kind":"rpc.response"`)) {
					close(release)
					t.Fatalf("overflow response = %s, want kind rpc.response", encoded)
				}
				close(release)
				return
			}
		case <-deadline:
			close(release)
			t.Fatal("the saturated request never got an answer")
		}
	}
}

// 每一条回话都必须带 kind：云端是按 kind 分派的，缺了它整帧会被当成事件信封丢掉。
func TestFSResponsesCarryTheirKind(t *testing.T) {
	agent := newFSRelayAgent(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"status":200,"data":{}}`))
	}))
	writeCh := make(chan any, 4)
	agent.dispatchRPCRequest(context.Background(), rpcRequest{RequestID: "rpc-6", Op: "fs.tree", ProjectID: "p1"}, writeCh)
	select {
	case message := <-writeCh:
		response, ok := message.(rpcResponse)
		if !ok {
			t.Fatalf("message type = %T", message)
		}
		if response.Kind != "rpc.response" {
			t.Fatalf("kind = %q", response.Kind)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no response was written")
	}
}
