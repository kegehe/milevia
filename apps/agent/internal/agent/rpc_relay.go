package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

// 手机端的远程调用（文件与 Git）：云端发 rpc.request，本机调 control-server 的
// /api/remote/rpc，再把结果作为 rpc.response 回给云端。
//
// 转发而不是自己实现：op 到具体 handler 的映射、路径沙箱、大小闸门、版本冲突语义
// 全部留在 control-server 一份。Agent 在这条链路上只做两件事 —— 搬运，以及**挡住
// 会把云端中继连接读崩的超大帧**。它**不认识 op**（不按 op 前缀分流到不同端点）：
// 一旦它开始映射 op，就等于把那条名单复制了第二份，而"两份名单要同步"正是本设计
// 一直在避开的结构性风险（见 docs/41 §3.1）。
//
// 这一步不是可有可无的防御：云端读 Agent 帧的上限是 512 KiB（cloud-control 的
// SetReadLimit），超了会让它**读失败并掐断整条中继连接** —— 代价不只是这次请求失败，
// 而是这台电脑与手机之间的命令、事件、快照一起断掉，直到重连。所以宁可在这里把
// 一个超大响应换成一句明确的错误。
const (
	// rpcFrameLimit 与云端 cloud-control 的 rpcFrameLimit 是同一根预算的两端，必须同值。
	// 上界是云端的 512 KiB 读限，下界要装得下 control-server 的 320 KiB 内容上限
	// 加上 JSON 转义与信封。改一处必须同时改另一处。
	rpcFrameLimit = 384 << 10
	// rpcRequestConcurrency 是同时在跑的中继请求数。
	//
	// 取 8 而不是 4：Git 视图一进去就是一个 Promise.all 发 5 条
	// （summary / changes / branches / operations / conflicts），给 4 意味着**第一屏
	// 必然有一条被拒**。而被拒的那条会拿到一句"同时处理的请求太多"，
	// 用户面对的是一个刚打开就报错的视图 —— 而仓库其实完全正常。
	// 8 仍是个有界的小数，真正串行的是本机那条唯一 SQLite 连接，而 git 读是只读的。
	rpcRequestConcurrency = 8
)

type rpcRequest struct {
	Kind           string          `json:"kind"`
	RequestID      string          `json:"requestId"`
	Op             string          `json:"op"`
	ProjectID      string          `json:"projectId,omitempty"`
	ConversationID string          `json:"conversationId,omitempty"`
	Params         json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	Kind      string          `json:"kind"`
	RequestID string          `json:"requestId"`
	OK        bool            `json:"ok"`
	Status    int             `json:"status"`
	Data      json.RawMessage `json:"data,omitempty"`
	Error     string          `json:"error,omitempty"`
	// Code 是电脑端给的稳定机器码（如 workspace_occupied）。**必须原样转发**：
	// 那句 Error 是给人看的、会被本地化，手机端要按码分支的判据就落在这个字段上。
	Code string `json:"code,omitempty"`
}

// dispatchRPCRequest 把一帧 rpc.request 交给一个后台 goroutine。
//
// **绝不能阻塞读循环**：读循环一停，下行的命令就再也读不进来，整条链路静默
// （这个故障模式在本项目里出现过，见 readCommands 里对 command.status 回执的那段说明）。
// 所以并发满的时候回一句明确的"忙"，而不是在原地等一个空位。
func (a *Agent) dispatchRPCRequest(ctx context.Context, request rpcRequest, writeCh chan<- any) {
	slots := a.rpcSlotPool()
	select {
	case slots <- struct{}{}:
	default:
		// 并发已满。丢掉这一帧会让手机端干等到它自己的超时，阻塞读循环会让整条
		// 下行链路停止 —— 两者都比"立刻回一句忙"差。
		a.sendRPCResponse(ctx, writeCh, rpcResponse{
			RequestID: request.RequestID,
			OK:        false,
			Status:    http.StatusServiceUnavailable,
			Error:     "电脑端同时处理的请求太多，请稍后重试",
		})
		return
	}
	go func() {
		defer func() { <-slots }()
		a.sendRPCResponse(ctx, writeCh, a.callRPCRelay(ctx, request))
	}()
}

func (a *Agent) sendRPCResponse(ctx context.Context, writeCh chan<- any, response rpcResponse) {
	response.Kind = "rpc.response"
	// 写入同样带超时：写队列被事件灌满时无限期等待会把这条 goroutine 钉死，
	// 而连接其实早就该重连了（与命令回执同一条处理）。
	if !sendMessageWithin(ctx, writeCh, response, agentWriteQueueStallTimeout) {
		log.Printf("rpc response for %s could not be written: %v", response.RequestID, agentWriteStallError(ctx))
	}
}

// callRPCRelay 调本机的中继端点并把回答原样带回。
//
// 它不复用 localPost：那个 helper 只回一句 "local request failed"，而这条文案要一路
// 显示到手机上 —— 用户看到"保存失败"却不知道是版本冲突还是权限问题，等于白跑一趟。
// 这里要把 control-server 原本那句面向用户的中文取出来。
func (a *Agent) callRPCRelay(ctx context.Context, request rpcRequest) rpcResponse {
	body := mustJSON(map[string]any{
		"op":             request.Op,
		"projectId":      request.ProjectID,
		"conversationId": request.ConversationID,
		"params":         request.Params,
	})
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, a.localURL()+"/api/remote/rpc", bytes.NewReader(body))
	if err != nil {
		return rpcFailure(request.RequestID, http.StatusInternalServerError, err.Error())
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("X-Milevia-Agent-Token", a.config.LocalToken)
	response, err := a.client.Do(httpRequest)
	if err != nil {
		// 本机服务通常意味着 Milevia 没在跑。这句话是给用户看的，要说清让他去做什么。
		return rpcFailure(request.RequestID, http.StatusBadGateway, "读不到本机服务，请确认电脑上的 Milevia 正在运行")
	}
	defer response.Body.Close()

	// 第一道：读的内存上限。它是**给本机服务**的兜底（正常响应不会接近它），
	// 不是帧的判据 —— 帧的判据只有下面那一道。
	//
	// 两道不能合并成一个数：这一道的输入是"本机响应体"，而真正要发出去的是
	// **重新序列化后的帧**（多了 kind / requestId / status 这些信封字段）。
	// 把两者当成同一个数，会让信封那几十字节落进无人检查的缝里；
	// 而如果这一道就卡在帧上限上，它会让下面的真判据**几乎永远走不到** ——
	// 一条写了却没人走的分支，等于这条防线上什么都没验。
	payload, err := io.ReadAll(io.LimitReader(response.Body, 2*rpcFrameLimit+1))
	if err != nil {
		return rpcFailure(request.RequestID, http.StatusBadGateway, "读取本机服务的响应失败")
	}
	if len(payload) > 2*rpcFrameLimit {
		// 本机服务自己出了问题：正常响应远到不了这里，连缓冲都不该继续建。
		return rpcFailure(request.RequestID, http.StatusBadGateway, "本机服务返回了异常大的响应")
	}
	if response.StatusCode >= 300 {
		// 中继端点自身的问题（op 不支持、projectId 非法、params 类型错）。
		// 业务失败不走这里：它是 HTTP 200 + ok:false，见 control-server 的 relayRPCRequest。
		return rpcFailure(request.RequestID, response.StatusCode, localErrorMessage(payload, response.StatusCode))
	}

	var relayed struct {
		OK     bool            `json:"ok"`
		Status int             `json:"status"`
		Data   json.RawMessage `json:"data"`
		Error  string          `json:"error"`
		Code   string          `json:"code"`
	}
	if err := json.Unmarshal(payload, &relayed); err != nil {
		return rpcFailure(request.RequestID, http.StatusBadGateway, "本机服务返回了无法解析的响应")
	}
	result := rpcResponse{
		Kind:      "rpc.response",
		RequestID: request.RequestID,
		OK:        relayed.OK,
		Status:    relayed.Status,
		Data:      relayed.Data,
		Error:     relayed.Error,
		Code:      relayed.Code,
	}
	// 第二道：**真正要发出去的那一帧**的字节数。这是唯一的判据。
	// 超了就不能发：云端读帧失败会掐断整条中继连接，代价是这台电脑与手机之间的
	// 命令、事件、快照一起断掉，而不只是这次请求失败。
	encoded, err := json.Marshal(result)
	if err != nil {
		return rpcFailure(request.RequestID, http.StatusInternalServerError, "无法序列化响应")
	}
	if len(encoded) > rpcFrameLimit {
		return rpcFailure(request.RequestID, http.StatusRequestEntityTooLarge, "这次请求的内容超过了中继通道的容量，请在电脑上查看")
	}
	return result
}

func rpcFailure(requestID string, status int, message string) rpcResponse {
	return rpcResponse{Kind: "rpc.response", RequestID: requestID, OK: false, Status: status, Error: message}
}

// localErrorMessage 从本机响应体里取那句面向用户的文案。
func localErrorMessage(payload []byte, status int) string {
	var failure struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(payload, &failure); err == nil && strings.TrimSpace(failure.Error) != "" {
		return strings.TrimSpace(failure.Error)
	}
	return fmt.Sprintf("本机服务返回 %d", status)
}

// rpcSlotPool 懒初始化并发闸门，这样直接构造 Agent 的测试不必先跑一遍 New。
func (a *Agent) rpcSlotPool() chan struct{} {
	a.rpcSlotsOnce.Do(func() {
		a.rpcSlots = make(chan struct{}, rpcRequestConcurrency)
	})
	return a.rpcSlots
}
