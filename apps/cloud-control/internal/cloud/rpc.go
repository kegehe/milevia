package cloud

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

// 手机端与电脑端之间的远程调用（RPC）通道：文件（fs.*）与 Git（git.*）共用它。
//
// 它刻意**不走 cloud_commands**：
//
//   - 命令表会把 payload 与 result 落进 PostgreSQL。文件内容与 diff 一旦走那条路，
//     就等于把项目源码持久化到云端，与「项目源码不上传云端」直接冲突。这条通道全程
//     只在内存里过，云端对内容零留存 —— 这是产品明确要求的一条，不是优化。
//   - 这些操作是幂等无副作用的请求-响应；命令通道那套 500ms 轮询 + 幂等键 + 终态机
//     是为"会产生持久业务效果"的操作准备的，用在这里既慢又在语义上绕远。
//
// 云端**不认识 op**：它只做鉴权、限长、判在线三件事。op 名单留在真正执行操作的
// 那一端（control-server），这样就不存在"两份名单要同步"的老问题。
//
// 通道复用 Agent 那条已有的 WSS 长连接，因此不需要任何新的握手、鉴权或连接管理：
// Agent 连上来的那条连接本来就按 instanceId 归好了位。
const (
	// rpcFrameLimit 是一条中继帧的字节上限。
	//
	// 这个数是**被夹出来的**，两头都有硬边界：
	//
	//   · 上界：云端读 Agent 帧的上限是 512 KiB（agentConnect 的 SetReadLimit）。
	//     帧一旦超了，gorilla 会让**读失败并掐断整条中继连接** —— 那不是"这次请求失败"，
	//     而是这台电脑与手机之间的一切（命令、事件、快照）一起断掉。所以必须远小于它。
	//   · 下界：control-server 那边单个 op 的上限最高是 320 KiB（改动差异 / 变更列表），
	//     加上 JSON 转义与服务端本来的实现细节、再留出信封空间，384 KiB 才安全。
	//
	// 三端共用同一根预算：这是同一根预算的三端表达，改一处必须同时看另外两处
	// （Agent 侧的 rpcFrameLimit 在 apps/agent，control-server 侧的 relayFrameBudget
	// 在 remote_relay.go，各自带同样的说明）。
	rpcFrameLimit = 384 << 10
	// rpcDefaultTimeout 是客户端没提议超时时，云端等电脑回话的上限。
	// 取 20 秒：本地 stat / read 都是毫秒级，慢的是 SSH 项目的 SFTP 往返
	// （单次 50–200ms），而一次递归取树可能要跑几十次。
	// 它必须大于手机端自己的请求超时，否则手机先报"超时"，云端还在等一个早就丢掉的响应。
	rpcDefaultTimeout = 20 * time.Second
	// rpcMinTimeout / rpcMaxTimeout 是"客户端提议超时"的夹取区间。
	//
	// 为什么让客户端提议：云端不解析 op，所以它无从知道"push 要比读仓库状态等更久"，
	// 而适配器知道（control-server 的单条 git 命令上限是 30s，网络写还要加远端握手）。
	// 云端只负责把这个数夹在合法区间里 —— 它仍然不需要认识任何 op 语义。
	//
	// 上界 120s 是给"一次网络写 + 通道余量"留的余量，不是鼓励；下界 1s 挡住"客户端
	// 传 0 或负数"被解释成"立即超时"。
	rpcMinTimeout = time.Second
	rpcMaxTimeout = 120 * time.Second
)

// rpcTimeoutFor 把客户端提议的超时夹进合法区间；未提议（<=0）时用默认值。
func rpcTimeoutFor(proposedMs int) time.Duration {
	if proposedMs <= 0 {
		return rpcDefaultTimeout
	}
	timeout := time.Duration(proposedMs) * time.Millisecond
	if timeout < rpcMinTimeout {
		return rpcMinTimeout
	}
	if timeout > rpcMaxTimeout {
		return rpcMaxTimeout
	}
	return timeout
}

// rpcResponse 是电脑端对一次远程调用的回答。
//
// Status 必须一路保留到手机端：客户端要据此区分"版本冲突（409，文件在电脑上被改过）"、
// "lease 占用（409，AI 正在跑）"与"参数错误（400，客户端自己的问题）"，压成一句话
// 就没法做这三种分支。Error 里是电脑端原本那句面向用户的中文，客户端原样显示。
//
// Code 是电脑端给的**稳定机器码**（如 workspace_occupied）。云端同样不自作主张：
// 它不认识那些码，只是原样搬运 —— 判据留在真正执行操作的那一端。
// 这个字段不是可有可无的：那句 Error 会被本地化，按文案分支的客户端判据会静默失效。
type rpcResponse struct {
	OK     bool            `json:"ok"`
	Status int             `json:"status"`
	Data   json.RawMessage `json:"data,omitempty"`
	Error  string          `json:"error,omitempty"`
	Code   string          `json:"code,omitempty"`
}

type rpcPendingRequest struct {
	instanceID string
	result     chan rpcResponse
}

// rpcRequestHub 用 requestId 把"云端发出去的帧"与"Agent 回来的帧"对上。
//
// pending 表放在内存里：进程重启会让在途请求全部丢失，而那些请求对应的 HTTP 连接
// 本来也已经断了（客户端会重试），所以不需要持久化。
type rpcRequestHub struct {
	mu      sync.Mutex
	pending map[string]*rpcPendingRequest
}

// register 认领一个 requestId。缓冲为 1，所以回话方永远不需要等接收方。
func (h *rpcRequestHub) register(instanceID string) (string, chan rpcResponse) {
	requestID := newRequestID()
	result := make(chan rpcResponse, 1)
	h.mu.Lock()
	if h.pending == nil {
		h.pending = map[string]*rpcPendingRequest{}
	}
	h.pending[requestID] = &rpcPendingRequest{instanceID: instanceID, result: result}
	h.mu.Unlock()
	return requestID, result
}

// resolve 投递 Agent 的回答；返回 false 表示这条 requestId 已经不在等待了
// （超时、客户端断开、或对端重复回话）。
func (h *rpcRequestHub) resolve(requestID string, response rpcResponse) bool {
	h.mu.Lock()
	pending, ok := h.pending[requestID]
	if ok {
		delete(h.pending, requestID)
	}
	h.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case pending.result <- response:
	default:
	}
	return true
}

// cancel 放弃等待。它必须能被重复调用：正常路径上 resolve 已经删过一遍，
// 调用方仍然会在 defer 里再调一次兜住超时与客户端断开两种情形。
func (h *rpcRequestHub) cancel(requestID string) {
	h.mu.Lock()
	delete(h.pending, requestID)
	h.mu.Unlock()
}

// failInstance 让一台电脑上所有在途的中继请求立刻拿到明确的失败。
//
// 没有这一步，中继断开后手机端的每一个请求都要干等到 rpcDefaultTimeout 才知道结果 ——
// 用户看到的是"卡了 20 秒然后说超时"，而真相（电脑掉线了）在第一毫秒就已经确定。
func (h *rpcRequestHub) failInstance(instanceID string, response rpcResponse) int {
	h.mu.Lock()
	victims := make([]*rpcPendingRequest, 0)
	for requestID, pending := range h.pending {
		if pending.instanceID == instanceID {
			victims = append(victims, pending)
			delete(h.pending, requestID)
		}
	}
	h.mu.Unlock()
	for _, pending := range victims {
		select {
		case pending.result <- response:
		default:
		}
	}
	return len(victims)
}

// agentSession 取这台电脑当前的中继连接；nil 表示它此刻不在线。
func (s *Server) agentSession(instanceID string) *agentConnection {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connections[instanceID]
}

var errInstanceOffline = errors.New("instance_offline: the computer is not connected right now")

// mobileRPCRequest 是手机端唯一的远程调用入口：文件与 Git 都走它。
//
// 返回值语义有两条容易搞错的地方，刻意分开：
//
//   - **HTTP 状态码表示这条通道本身是否走通**（200 = 问到了电脑 / 409 = 电脑不在线 /
//     504 = 电脑没在时限内回话）。
//   - **响应体里的 ok 表示那次文件操作是否成功**。电脑回一句"版本冲突"是通道完全
//     正常、业务明确失败，HTTP 码仍是 200。
//
// 混在一起的话，客户端就只能用一个 catch 去接两种完全不同的事实：一种该提示用户
// "文件被改过"，另一种该提示"电脑掉线了，去看看"。
func (s *Server) mobileRPCRequest(w http.ResponseWriter, r *http.Request) {
	instanceID := chi.URLParam(r, "instanceID")
	if !authorizedInstance(r, instanceID) {
		writeError(w, http.StatusForbidden, errors.New("instance access denied"))
		return
	}
	var input struct {
		Op             string          `json:"op"`
		ProjectID      string          `json:"projectId"`
		ConversationID string          `json:"conversationId"`
		Params         json.RawMessage `json:"params"`
		// TimeoutMs 是**客户端提议**的等待上限（见 rpcMinTimeout / rpcMaxTimeout）。
		TimeoutMs int `json:"timeoutMs"`
	}
	if !decode(w, r, &input) {
		return
	}
	input.Op = strings.TrimSpace(input.Op)
	input.ProjectID = strings.TrimSpace(input.ProjectID)
	input.ConversationID = strings.TrimSpace(input.ConversationID)
	// op / projectId 在这里只做形状校验，**不查名单**：真正执行操作的是电脑端，
	// 名单就必须留在执行点上（control-server 的 remoteOperations）。云端再存一份
	// 会带来"两份名单要同步"的老问题，而那正是任务/会话命令一直在承担的结构性风险。
	// 云端这一层的职责是鉴权、限长、夹超时，以及"电脑不在线就别问"。
	if input.Op == "" || input.ProjectID == "" {
		writeError(w, http.StatusBadRequest, errors.New("op and projectId are required"))
		return
	}
	session := s.agentSession(instanceID)
	if session == nil {
		writeError(w, http.StatusConflict, errInstanceOffline)
		return
	}

	requestID, result := s.rpcRequestHub().register(instanceID)
	frame := map[string]any{
		"kind":      "rpc.request",
		"requestId": requestID,
		"op":        input.Op,
		"projectId": input.ProjectID,
	}
	if input.ConversationID != "" {
		frame["conversationId"] = input.ConversationID
	}
	if len(input.Params) > 0 && string(input.Params) != "null" {
		frame["params"] = input.Params
	}
	encoded, err := json.Marshal(frame)
	if err != nil {
		s.rpcRequestHub().cancel(requestID)
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// 在**写之前**挡住超大帧。这里超限不会掐断连接（电脑端的读没有设限），
	// 所以它防的不是连接安全，而是"让客户端用一条请求把云端内存写爆"。
	if len(encoded) > rpcFrameLimit {
		s.rpcRequestHub().cancel(requestID)
		writeError(w, http.StatusRequestEntityTooLarge, errors.New("这次请求过大，已超过中继通道容量"))
		return
	}
	// 兜住所有没走到 resolve 的退出路径：超时、客户端断开、以及下面的写失败。
	// resolve 已经删过一次，重复 cancel 是安全的。
	defer s.rpcRequestHub().cancel(requestID)

	if err := s.agentWrite(instanceID, session, json.RawMessage(encoded)); err != nil {
		writeError(w, http.StatusConflict, errInstanceOffline)
		return
	}

	timeout := rpcTimeoutFor(input.TimeoutMs)
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	select {
	case response := <-result:
		// 业务失败也是 200：通道走通了，只是那次操作没成功。见上面那段说明。
		writeJSON(w, http.StatusOK, response)
	case <-ctx.Done():
		// 文案里的秒数取自**这次实际用的**超时，不写死 20 —— 写死的话，一个把超时
		// 提到 60s 的 push 超时后会告诉用户"没有在 20 秒内回应"，而它其实等了 60 秒。
		writeError(w, http.StatusGatewayTimeout, fmt.Errorf("电脑端没有在 %s 内回应这次请求", rpcTimeoutText(timeout)))
	}
}

// rpcTimeoutText 给人看的秒数：整秒不带小数，非整秒保留一位。
func rpcTimeoutText(timeout time.Duration) string {
	seconds := timeout.Seconds()
	if seconds == float64(int(seconds)) {
		return fmt.Sprintf("%d 秒", int(seconds))
	}
	return fmt.Sprintf("%.1f 秒", seconds)
}

// rpcRequestHub 懒初始化，并容忍直接构造 Server 的测试与旧部署。
func (s *Server) rpcRequestHub() *rpcRequestHub {
	s.rpcHubOnce.Do(func() {
		if s.rpcHub == nil {
			s.rpcHub = &rpcRequestHub{}
		}
	})
	return s.rpcHub
}

// resolveAgentRPCResponse 处理 Agent 回来的 rpc.response 帧。
func (s *Server) resolveAgentRPCResponse(raw json.RawMessage) bool {
	var response struct {
		RequestID string          `json:"requestId"`
		OK        bool            `json:"ok"`
		Status    int             `json:"status"`
		Data      json.RawMessage `json:"data"`
		Error     string          `json:"error"`
		Code      string          `json:"code"`
	}
	if json.Unmarshal(raw, &response) != nil || strings.TrimSpace(response.RequestID) == "" {
		return false
	}
	return s.rpcRequestHub().resolve(response.RequestID, rpcResponse{
		OK:     response.OK,
		Status: response.Status,
		Data:   response.Data,
		Error:  response.Error,
		Code:   response.Code,
	})
}

// failRPCRequestsForInstance 在中继断开时把在途请求立刻落成明确失败。
func (s *Server) failRPCRequestsForInstance(instanceID string) {
	failed := s.rpcRequestHub().failInstance(instanceID, rpcResponse{
		OK:     false,
		Status: http.StatusConflict,
		Error:  errInstanceOffline.Error(),
	})
	if failed > 0 {
		log.Printf("failed %d in-flight remote calls for %s: relay disconnected", failed, instanceID)
	}
}

// newRequestID 生成一个不透明的一次性请求标识。
// 前缀与命令 id（cmd-）分开，日志里一眼能看出这是中继请求而不是命令。
func newRequestID() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err == nil {
		return fmt.Sprintf("rpc-%x", bytes[:])
	}
	return fmt.Sprintf("rpc-%d", time.Now().UnixNano())
}
