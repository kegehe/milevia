package cloud

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// 帧上限必须**远小于**云端读 Agent 帧的硬上限。
//
// 云端在 agentConnect 里设了 SetReadLimit(512 KiB)，超限的帧会让读失败并**掐断整条
// 中继连接** —— 那不只是这次请求失败，而是这台电脑与手机之间的命令、事件、快照
// 一起断掉。所以这个约束不是"建议值"，是一条不能漂移的不变式：把数值本身钉在这里，
// 改大 rpcFrameLimit 会立刻红。
func TestRPCFrameLimitStaysUnderTheAgentReadLimit(t *testing.T) {
	const agentReadLimit = 512 << 10
	if rpcFrameLimit >= agentReadLimit {
		t.Fatalf("rpcFrameLimit = %d must stay well under the %d agent read limit", rpcFrameLimit, agentReadLimit)
	}
}

// 默认等待上限必须**大于**电脑端执行一次读操作应有的时间，否则手机先报超时、
// 云端还在等一个早就没人要的回答。这里只钉住它是个有限的正数且不比命令通道更短。
func TestRPCDefaultTimeoutIsBounded(t *testing.T) {
	if rpcDefaultTimeout <= 0 || rpcDefaultTimeout > time.Minute {
		t.Fatalf("rpcDefaultTimeout = %s, want a bounded positive duration", rpcDefaultTimeout)
	}
	if !(rpcMinTimeout < rpcDefaultTimeout && rpcDefaultTimeout < rpcMaxTimeout) {
		t.Fatalf("default timeout %s must sit strictly inside [%s, %s]",
			rpcDefaultTimeout, rpcMinTimeout, rpcMaxTimeout)
	}
}

// 客户端提议的超时只被**夹取**，云端不解析 op。
//
// 这条是"Git 写操作必然比读操作慢"那个问题的落点（docs/41 §3.2）：云端无从知道
// push 要等多久，所以由适配器提议、云端只保证它落在合法区间内。
// 边界值本身要钉住：0 / 负数不能被解释成"立即超时"，超大值不能被原样接受
// （那会让一条卡住的请求把服务端连接占满 10 分钟）。
func TestRPCTimeoutIsClampedFromTheClientProposal(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		proposed int
		want     time.Duration
	}{
		{"no proposal falls back to the default", 0, rpcDefaultTimeout},
		{"a negative proposal falls back to the default", -1000, rpcDefaultTimeout},
		{"a tiny proposal is lifted to the floor", 500, rpcMinTimeout},
		{"a normal proposal is taken as-is", 45_000, 45 * time.Second},
		{"an oversized proposal is capped", 10 * 60 * 1000, rpcMaxTimeout},
	} {
		if got := rpcTimeoutFor(testCase.proposed); got != testCase.want {
			t.Errorf("%s: rpcTimeoutFor(%d) = %s, want %s", testCase.name, testCase.proposed, got, testCase.want)
		}
	}
}

// 504 文案里的秒数必须取自**这次实际用的**超时，不能写死 20 ——
// 一个把超时提到 60s 的 push 超时后，说"没有在 20 秒内回应"是在骗用户。
func TestRPCTimeoutTextMatchesTheActualWait(t *testing.T) {
	if got := rpcTimeoutText(20 * time.Second); got != "20 秒" {
		t.Errorf("rpcTimeoutText(20s) = %q, want 20 秒", got)
	}
	if got := rpcTimeoutText(1500 * time.Millisecond); got != "1.5 秒" {
		t.Errorf("rpcTimeoutText(1.5s) = %q, want 1.5 秒", got)
	}
	// 写死 20 的话，这两条会一起红 —— 用**精确值**断言，不要用"含不含 20"这种判断：
	// 120 秒里本来就含 "20"，那种断言会误报。
	if got := rpcTimeoutText(time.Minute); got != "60 秒" {
		t.Errorf("rpcTimeoutText(1m) = %q, want 60 秒 (a hardcoded 20 would print 20 秒)", got)
	}
	if got := rpcTimeoutText(rpcMaxTimeout); got != "120 秒" {
		t.Errorf("rpcTimeoutText(%s) = %q, want 120 秒", rpcMaxTimeout, got)
	}
}

func TestRPCRequestHubDeliversToTheRegisteredWaiter(t *testing.T) {
	hub := &rpcRequestHub{}
	requestID, result := hub.register("pc-1")
	if requestID == "" {
		t.Fatal("request id is empty")
	}
	if !hub.resolve(requestID, rpcResponse{OK: true, Status: http.StatusOK, Data: json.RawMessage(`{"a":1}`)}) {
		t.Fatal("resolve rejected a registered request")
	}
	select {
	case response := <-result:
		if !response.OK || response.Status != http.StatusOK {
			t.Fatalf("response = %+v", response)
		}
		if string(response.Data) != `{"a":1}` {
			t.Fatalf("data = %s", response.Data)
		}
	default:
		t.Fatal("the registered waiter received nothing")
	}
}

// 认不出来的 requestId 是**正常的收敛路径**：超时的、客户端已经断开放弃的、
// 或者对端重复回话的。它必须返回 false 让调用方安静丢掉，绝不能因此断开连接。
func TestRPCRequestHubRejectsUnknownAndDuplicateResponses(t *testing.T) {
	hub := &rpcRequestHub{}
	if hub.resolve("never-registered", rpcResponse{OK: true}) {
		t.Fatal("an unknown request id was accepted")
	}
	requestID, _ := hub.register("pc-1")
	if !hub.resolve(requestID, rpcResponse{OK: true}) {
		t.Fatal("first resolve must succeed")
	}
	if hub.resolve(requestID, rpcResponse{OK: true}) {
		t.Fatal("a duplicate response must be rejected, not delivered twice")
	}
}

// 读到响应之后调用方一定还会在 defer 里再 cancel 一次，所以它必须能重复调用。
func TestRPCRequestHubCancelIsIdempotent(t *testing.T) {
	hub := &rpcRequestHub{}
	requestID, _ := hub.register("pc-1")
	hub.cancel(requestID)
	hub.cancel(requestID)
	// 取消之后再回话必须被当成未知请求：这条缓存已经不属于任何人了。
	if hub.resolve(requestID, rpcResponse{OK: true}) {
		t.Fatal("a cancelled request must not be resumable")
	}
}

// 只失败那台掉线电脑的请求。串味会让另一台电脑上正等着的请求莫名其妙地失败。
func TestRPCRequestHubFailsOnlyTheDisconnectedInstance(t *testing.T) {
	hub := &rpcRequestHub{}
	lostID, lost := hub.register("pc-offline")
	keptID, kept := hub.register("pc-online")

	if failed := hub.failInstance("pc-offline", rpcResponse{Status: http.StatusConflict, Error: "instance_offline"}); failed != 1 {
		t.Fatalf("failed = %d, want 1", failed)
	}
	select {
	case response := <-lost:
		if response.OK || response.Status != http.StatusConflict {
			t.Fatalf("the disconnected instance got %+v", response)
		}
	default:
		t.Fatal("the disconnected instance's request was not failed")
	}
	// 已经被判定失败的那条请求不会再接受回话：掉线的电脑重新连上后可能补发一个
	// 迟到的响应，它必须被丢掉，而不是让已经收到失败的客户端再收一次成功。
	if hub.resolve(lostID, rpcResponse{OK: true, Status: http.StatusOK}) {
		t.Fatal("a failed request accepted a late response")
	}
	select {
	case response := <-kept:
		t.Fatalf("the other instance's request was failed too: %+v", response)
	default:
	}
	// 保留下来的那条仍然能正常收到回答。
	if !hub.resolve(keptID, rpcResponse{OK: true, Status: http.StatusOK}) {
		t.Fatal("the surviving request stopped being resolvable")
	}
}

// 中继断开时把在途请求**立刻**落成明确失败。没有这一步，手机端的每个请求都要干等
// 到 rpcDefaultTimeout 才知道结果 —— 用户看到"卡了 20 秒然后超时"，而真相（电脑掉线了）
// 在第一毫秒就已经确定。
func TestRPCRequestHubFailInstanceIsNoOpWhenNothingIsPending(t *testing.T) {
	hub := &rpcRequestHub{}
	if failed := hub.failInstance("pc-1", rpcResponse{Status: http.StatusConflict}); failed != 0 {
		t.Fatalf("failed = %d, want 0", failed)
	}
	// 空 hub 上调用也不能 panic：它会在每条连接收尾时被无条件调用。
	empty := &rpcRequestHub{}
	empty.failInstance("pc-1", rpcResponse{})
	empty.cancel("nothing")
}

func TestResolveAgentRPCResponseShape(t *testing.T) {
	server := &Server{}
	for _, testCase := range []struct {
		name     string
		frame    string
		accepted bool
	}{
		{"a well-formed response is accepted", `{"kind":"rpc.response","requestId":"rpc-1","ok":true,"status":200,"data":{"x":1}}`, true},
		{"a failure response is accepted", `{"kind":"rpc.response","requestId":"rpc-2","ok":false,"status":409,"error":"文件已被修改","code":"workspace_occupied"}`, true},
		{"a frame without a request id is ignored", `{"kind":"rpc.response","ok":true}`, false},
		{"a frame with an empty request id is ignored", `{"kind":"rpc.response","requestId":""}`, false},
		{"invalid json is ignored", `{`, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 先注册再解析，才能区分"被接受"与"格式不对被丢掉"。
			var requestID string
			if testCase.accepted {
				requestID, _ = server.rpcRequestHub().register("pc-1")
			}
			frame := testCase.frame
			if testCase.accepted {
				var decoded map[string]any
				if err := json.Unmarshal([]byte(frame), &decoded); err != nil {
					t.Fatalf("bad fixture: %v", err)
				}
				decoded["requestId"] = requestID
				encoded, err := json.Marshal(decoded)
				if err != nil {
					t.Fatalf("marshal fixture: %v", err)
				}
				frame = string(encoded)
			}
			got := server.resolveAgentRPCResponse(json.RawMessage(frame))
			if got != testCase.accepted {
				t.Fatalf("resolveAgentRPCResponse = %v, want %v", got, testCase.accepted)
			}
		})
	}
}

// code 必须穿过 hub 走到客户端：它是手机端**唯一**能用来分支的判据 ——
// 那句 error 会被电脑端本地化，按文案匹配的分支会静默失效。
func TestResolveAgentRPCResponseCarriesTheMachineCode(t *testing.T) {
	server := &Server{}
	requestID, result := server.rpcRequestHub().register("pc-1")
	frame, err := json.Marshal(map[string]any{
		"kind":      "rpc.response",
		"requestId": requestID,
		"ok":        false,
		"status":    http.StatusConflict,
		"error":     "项目工作区正被其他 AI 任务或 Git 操作占用，请等待当前操作完成后重试。",
		"code":      "workspace_occupied",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !server.resolveAgentRPCResponse(frame) {
		t.Fatal("the response was dropped")
	}
	select {
	case response := <-result:
		if response.Code != "workspace_occupied" {
			t.Fatalf("code = %q, want it to survive the hub", response.Code)
		}
	case <-time.After(time.Second):
		t.Fatal("no response was delivered")
	}
}

// rpcRequestHub() 必须能在直接构造的 Server 上工作：仓库里大量测试与旧部署都不跑 New。
func TestRPCRequestHubLazyInitialization(t *testing.T) {
	server := &Server{}
	first := server.rpcRequestHub()
	if first == nil {
		t.Fatal("lazy hub is nil")
	}
	if server.rpcRequestHub() != first {
		t.Fatal("the hub must be created once and reused")
	}
}
