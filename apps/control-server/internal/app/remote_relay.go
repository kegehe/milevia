package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// 手机端与本机之间的远程调用（RPC）通道：Agent 把云端的请求帧原样转给
// /api/remote/rpc，本进程按 op 调用**既有的** handler。
//
// 这条通道承载**多个领域**的操作：文件（fs.*，见 remote_fs.go）与
// Git（git.*，见 remote_git.go）。形态是「一条通道、一个端点、一张由各领域合成的 op 表」，
// 不是「每个领域一条通道」—— 后者要求 Agent 按 op 前缀选择端点，那就等于把 op 映射
// 搬进 Agent，正是本设计刻意避开的结构性风险。见 docs/41 §3.1。
//
// 为什么走本机 HTTP 而不复用 /api/remote/commands 那条命令通道：
//
//   - 命令通道会把 payload 落进云端 PostgreSQL（cloud_commands.payload 是 jsonb）。
//     文件内容与 diff 一旦走那条路，就等于把项目源码持久化到云端，与「项目源码不上传云端」
//     直接冲突。这条通道全程只在内存里过，落盘为零。
//   - 命令通道的 payload / result 各限 256 KiB，且是 500ms 轮询取结果；这些操作是
//     幂等无副作用的请求-响应，用轮询去做既慢又在语义上绕远。
//   - Git 写操作确实需要"重放安全"，但那由服务端自己的 stateToken 乐观锁提供
//     （同版本写第二次会拿到明确的冲突，而不是静默重复写入），不需要命令通道的幂等键。
//
// 这是 relay 命名空间里唯一提供**文件读写与 Git 操作**的地方，因此它是这个命名空间
// 安全边界的核心，不是普通的一批新端点 —— 见 app.go 里 remote 路由组的那段说明。
// 它不提供 shell、任意命令执行、任意 `git` 子命令或任意 URL 转发。
//
// op 名单只有一份，且在**执行点**（本进程，由 remoteOperations 合成）：
// 云端只做鉴权与转发、不认识 op；Agent 也只把帧转给这一个端点。这样就不存在
// "两份名单要同步" 的问题 —— 那正是任务/会话命令（enqueueRemoteCommand 与云端
// createCommand）一直在承担的结构性风险。
//
// relayFrameBudget 是中继单帧的字节预算（384 KiB）。
//
// 它与 apps/agent 的 rpcFrameLimit、cloud-control 的 rpcFrameLimit 是**同一根预算的三端
// 表达**：那两个是真正执行这道限制的地方（云端读 Agent 帧超限会掐断整条中继连接，
// 不是这次请求失败），这里只是给"某个 op 的 MaxBytes 必须落在这条预算之内"提供判据，
// 并在测试里把数值本身钉住。改动任何一端都要同时看另外两端。
const relayFrameBudget = 384 << 10

// remoteOperation 是 op 表里的一条。
type remoteOperation struct {
	Method string
	// Path 是 /api/projects/{projectID} 之后的片段，可含 {name} 占位。
	Path string
	// Query 为 true 表示 params 是查询参数（取值必须都是字符串）；
	// 为 false 表示 params 整个作为 JSON 请求体。
	Query bool
	// PathParams 是**允许**从 params 替换进 Path 占位符的参数名白名单。
	// 不在这个列表里的参数只会留在 query / body 里，绝不会被拼进 URL 路径。
	// 每个名字都必须有校验器（见 relayPathParamValidators），否则报错 ——
	// 这样"新加一个路径参数却忘了写校验"在第一次调用时就会暴露，而不是带着
	// 一个能拼任意字符串进 URL 的口子悄悄上线。
	PathParams []string
	// MaxBytes 是该 op 允许的**响应帧**字节上限（0 表示不限）。
	//
	// 它必须小于 relayFrameBudget，否则这条 op 的判据永远排在通道判据之后 ——
	// 症状是"本该说'这个差异太大'，用户却收到一句'超过中继通道容量'"。
	// TestRemoteOperationBudgetsStayUnderTheFrameLimit 守着这条。
	//
	// 判据必须量**序列化之后的那一帧**，不是 handler 自己的响应体长度 ——
	// 项目为此栽过两次（图片 base64 膨胀、文本 JSON 转义），见 docs/40 §6。
	MaxBytes int
	// Label 是超限文案里的名词（"改动差异" / "提交历史" …）。
	Label  string
	Handle http.HandlerFunc
}

// remoteOperations 是 relay 唯一的一张 op 表：把各领域的名单合成一份。
//
// 领域各自导出自己的 map（remoteFSOperations / remoteGitOperations），而不是写成
// 一个巨型 switch：每份名单都能被独立测试钉死（条数、方法、路径、上限），
// 而"改名单必须同时改测试"这条纪律也就落到了具体的一份上。
//
// 两份名单的 op 名**不允许有交集**：有交集意味着后注册的那份静默覆盖前一份，
// 症状是"某个操作跑到另一个 handler 上去了"，而两边各自看代码都是对的。
// TestRelayCompositeOperationTableHasNoOverlap 守着这条。
func (s *Server) remoteOperations() map[string]remoteOperation {
	operations := map[string]remoteOperation{}
	for name, operation := range s.remoteFSOperations() {
		operations[name] = operation
	}
	for name, operation := range s.remoteGitOperations() {
		operations[name] = operation
	}
	return operations
}

// rpcResponse 是这次调用的答复。与云端 rpcResponse、手机端 MobileRpcReply 一一对应。
//
// **保留 status** 是刻意的：客户端要据此区分"版本冲突（409，文件在电脑上被改过）"、
// "工作区被 AI 占着（409）"与"参数错误（400，客户端自己的问题）"，压成一句话
// 就没法做这三种分支。Error 里是服务端原本那句面向用户的中文，客户端**原样显示**，
// 不要自己再翻译一遍。
//
// Code 是这次失败的**稳定机器码**（如 workspace_occupied），没有可判的码时为空。
// 它必须一路带到手机上，因为 Error 那句是给人看的、**会被本地化**：`writeError` 会把
// "project workspace is occupied by another run or Git operation" 换成"项目工作区正被
// 其他 AI 任务或 Git 操作占用…"。客户端若按原文匹配，那条分支就永远不命中 ——
// 而喂原文的单测照样绿，属于"判据量错了东西"。服务端本来就在 `httpErrorCode` 里
// 专门产出这个码，正是为了"不把行为耦合到本地化文案上"；中继以前把它丢掉了。
type rpcResponse struct {
	OK     bool            `json:"ok"`
	Status int             `json:"status"`
	Data   json.RawMessage `json:"data,omitempty"`
	Error  string          `json:"error,omitempty"`
	Code   string          `json:"code,omitempty"`
}

// relayRPCRequest 是 Agent 唯一会调用的远程操作端点。
func (s *Server) relayRPCRequest(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Op             string          `json:"op"`
		ProjectID      string          `json:"projectId"`
		ConversationID string          `json:"conversationId"`
		Params         json.RawMessage `json:"params"`
	}
	if !decode(w, r, &input) {
		return
	}
	input.Op = strings.TrimSpace(input.Op)
	input.ProjectID = strings.TrimSpace(input.ProjectID)
	input.ConversationID = strings.TrimSpace(input.ConversationID)

	operation, ok := s.remoteOperations()[input.Op]
	if !ok {
		writeError(w, http.StatusBadRequest, errors.New("不支持的远程操作"))
		return
	}
	// projectId 一律必填：所有 handler 都要用它解析工作区（resolveRequestWorkspace），
	// 缺了它会走到"项目不存在"，报错比这里直接拦下更含糊。
	if input.ProjectID == "" {
		writeError(w, http.StatusBadRequest, errors.New("projectId 必填"))
		return
	}
	// ProjectID 会被拼进 URL 路径。它随后仍要经 handler 查库校验存在性，但先把路径分隔符
	// 与编码字符挡在外面，否则一个 `../` 就能让合成的请求指到别的路由上去。
	if strings.ContainsAny(input.ProjectID, "/\\?#%") {
		writeError(w, http.StatusBadRequest, errors.New("projectId 非法"))
		return
	}

	target, body, pathParams, err := relayTarget(operation, input.ProjectID, input.ConversationID, input.Params)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	// 路径参数必须**同时**做两件事：替换进 URL（让日志与报错里的地址是真的），
	// 以及经 extraParams 注入 chi 的路由上下文（合成请求没有真的走路由匹配，
	// handler 里 chi.URLParam 读的就是这份）。少做第二件的话，oid 会是空串，
	// 症状是一句指不到原因的"Git 对象 ID 不合法"。
	extraParams := make([]string, 0, len(pathParams)*2)
	for _, name := range relaySortedKeys(pathParams) {
		extraParams = append(extraParams, name, pathParams[name])
	}

	status, responseBody := invokeLocalHandler(r.Context(), operation.Method, target, input.ProjectID, "", "", operation.Handle, body, extraParams...)
	if status >= 400 {
		writeJSON(w, http.StatusOK, relayErrorResponse(status, responseBody))
		return
	}
	payload := rpcResponse{OK: true, Status: status, Data: json.RawMessage(orEmptyJSONObject(responseBody))}
	// 该 op 自己的上限。量的是**这一帧**（信封 + data 序列化之后），因为真正上路的就是它。
	// 信封比这里的编码结果只多几十字节（Agent 会再加 kind / requestId / status），
	// 而 320 KiB 的上限与 384 KiB 的帧上限之间正好留着这段余量。
	if operation.MaxBytes > 0 {
		if encoded, err := json.Marshal(payload); err == nil && len(encoded) > operation.MaxBytes {
			writeJSON(w, http.StatusOK, rpcResponse{
				OK:     false,
				Status: http.StatusRequestEntityTooLarge,
				Error:  relayOversizeError(operation, len(encoded)).Error(),
			})
			return
		}
	}
	writeJSON(w, http.StatusOK, payload)
}

// relayTarget 把一条 op 加信封拼成"本机 handler 该看到的样子"。
//
// 抽成**纯函数**（不碰 Server、不发请求）有两个理由：它是整条链路上最容易出错的一步
// （把外部字符串拼进 URL 路径），而纯函数是这套代码里唯一能被穷举测试的部分 ——
// 沙箱里构造 Server 会拉起 wsl.exe 而被拦，所以"能测"这件事在这里是有实际价值的。
// 返回值里的 pathParams 要原样交给 invokeLocalHandler 的 extraParams（见调用点说明）。
func relayTarget(operation remoteOperation, projectID, conversationID string, params json.RawMessage) (string, json.RawMessage, map[string]string, error) {
	target := "/api/projects/" + projectID + operation.Path
	var body json.RawMessage

	pathValues, err := relayPathValues(params, operation.PathParams)
	if err != nil {
		return "", nil, nil, err
	}

	if operation.Query {
		values := map[string]string{}
		if len(params) > 0 {
			// 查询类操作的 params 必须是字符串映射。用 json.Unmarshal 到 map[string]string
			// 而不是 map[string]any 再自行转换：后者会把数字 / 布尔悄悄改成别的写法，
			// 客户端传错类型时应当明确报错，而不是被服务端猜一个值。
			if err := json.Unmarshal(params, &values); err != nil {
				return "", nil, nil, errors.New("params 必须是字符串键值对")
			}
		}
		query := url.Values{}
		for key, value := range values {
			// `conversationId` 只能来自信封。它决定**落在哪个工作区**（多会话有 worktree
			// 隔离），而 params 是调用方自由填的一袋参数 —— 两者同名时以 params 为准，
			// 就等于让一个"路径参数"顺手改了工作区选择：客户端多传一个字段、或者哪个
			// 调用方顺手把 URL 上的查询整串塞进 params，请求就会静默落到另一个工作区，
			// 或者直接死在"会话工作区不存在"上（这句报错完全指不到真正的原因）。
			// 信封里那个值是页面按当前会话定死传下来的，它必须是唯一的来源。
			if key == "conversationId" {
				continue
			}
			query.Set(key, value)
		}
		if conversationID != "" {
			query.Set("conversationId", conversationID)
		}
		if encoded := query.Encode(); encoded != "" {
			target += "?" + encoded
		}
	} else {
		// 请求体类操作同样要剔掉 params 里的 conversationId —— 见下面那段说明。
		stripped, err := stripRelayParam(params, "conversationId")
		if err != nil {
			return "", nil, nil, err
		}
		body = stripped
		// 请求体类操作的 conversationId 同样是**查询参数**（决定工作区，与信封同源），
		// 不塞进请求体 —— 体里那份是给 handler 的业务字段用的。
		if conversationID != "" {
			target += "?conversationId=" + url.QueryEscape(conversationID)
		}
	}

	return applyRelayPathValues(target, operation.PathParams, pathValues), body, pathValues, nil
}

// stripRelayParam 从请求体里删掉一个键，并保证结果仍是合法 JSON 对象。
//
// 为什么请求体里也要剔：`params` 是调用方自由填的一袋参数，而 conversationId
// 决定**落在哪个工作区**。查询分支早就剔了它，请求体分支第一版漏了 —— 当时并不会
// 出问题（handler 都 decode 到自己的结构体，多余字段被忽略；工作区一律从 query 读），
// 但那意味着这条不变量靠的是"**碰巧**没有任何 handler 去读体里那个键"。
// 那种保证会被后来一次无心的改动悄悄破坏，而且破坏时不会有任何测试红。
// 剔掉之后它由结构成立：调用方无论怎么填，都影响不到工作区选择。
func stripRelayParam(params json.RawMessage, name string) (json.RawMessage, error) {
	if len(params) == 0 || string(params) == "null" {
		return json.RawMessage(`{}`), nil
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(params, &fields); err != nil {
		return nil, errors.New("params 必须是 JSON 对象")
	}
	if _, found := fields[name]; !found {
		// 没这个键就原样返回，避免为了剔一个不存在的键去做一次无谓的重编码。
		return params, nil
	}
	delete(fields, name)
	encoded, err := json.Marshal(fields)
	if err != nil {
		return nil, errors.New("params 无法序列化")
	}
	return encoded, nil
}

// relaySortedKeys 让 extraParams 的顺序确定：map 遍历顺序随机会让"同一个请求两次
// 的参数顺序不同"，在断言与日志里都是噪音。
func relaySortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// relayPathValues 从 params 里取出该 op 声明过的路径参数，逐个校验形状。
func relayPathValues(params json.RawMessage, names []string) (map[string]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	raw := map[string]json.RawMessage{}
	if len(params) > 0 && string(params) != "null" {
		if err := json.Unmarshal(params, &raw); err != nil {
			return nil, errors.New("params 必须是 JSON 对象")
		}
	}
	values := make(map[string]string, len(names))
	for _, name := range names {
		payload, found := raw[name]
		if !found {
			return nil, fmt.Errorf("%s 必填", name)
		}
		var value string
		if err := json.Unmarshal(payload, &value); err != nil {
			return nil, fmt.Errorf("%s 必须是字符串", name)
		}
		if err := validateRelayPathParam(name, value); err != nil {
			return nil, err
		}
		values[name] = value
	}
	return values, nil
}

// applyRelayPathValues 把校验过的值替换进 Path 的占位符。
//
// 值一律经 url.PathEscape：即使校验器将来放宽，也不会有人能把 `/` 或 `..` 拼进路径。
func applyRelayPathValues(target string, names []string, values map[string]string) string {
	for _, name := range names {
		value, ok := values[name]
		if !ok {
			continue
		}
		target = strings.ReplaceAll(target, "{"+name+"}", url.PathEscape(value))
	}
	return target
}

var relayUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// relayPathParamValidators 是路径参数的形状校验表。**每个**被声明的路径参数都必须在表里，
// 否则一律拒绝 —— 见 remoteOperation.PathParams 的说明。
var relayPathParamValidators = map[string]func(string) error{
	"oid": func(value string) error {
		// 复用 handler 自己那个判据，不另写一份：手写 40 位十六进制看似够用，
		// 但它比 handler **更严** —— isFullGitObjectID 还接受 64 位（SHA-256 仓库），
		// 于是同一个 oid 在服务端能过、在中继层被拒，报错还指不到原因。
		// 这类"中继比 handler 严"的差异不会让任何测试红，只会让个别仓库用不了。
		if !isFullGitObjectID(value) {
			return errors.New("oid 必须是完整的 Git 对象 ID")
		}
		return nil
	},
	"suggestionID": func(value string) error {
		if !relayUUID.MatchString(value) {
			return errors.New("suggestionID 格式不正确")
		}
		return nil
	},
}

func validateRelayPathParam(name, value string) error {
	validate, ok := relayPathParamValidators[name]
	if !ok {
		// 这是接线错误，不是客户端错误：新增了路径参数却忘了登记校验器。
		return fmt.Errorf("路径参数 %s 没有校验器", name)
	}
	return validate(value)
}

// relayOversizeError 说明"这条 op 的返回太大"。
//
// 文案必须与通道帧判据那句（Agent 侧的"超过中继通道的容量"）**不同**：
// 两条闸门若能挡住同一个用例，那条测试就什么都没证明（docs/40 §6）。
// 而且要说得具体 —— 用户对"这个改动差异太大"能立刻理解，对"通道容量"不能。
func relayOversizeError(operation remoteOperation, size int) error {
	label := strings.TrimSpace(operation.Label)
	if label == "" {
		label = "内容"
	}
	return fmt.Errorf("%s太大（%s），手机上打不开，请在电脑上查看", label, relayHumanBytes(size))
}

func relayHumanBytes(size int) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	value := float64(size)
	units := []string{"KiB", "MiB", "GiB"}
	index := -1
	for value >= unit && index < len(units)-1 {
		value /= unit
		index++
	}
	return fmt.Sprintf("%.1f %s", value, units[index])
}

// relayErrorResponse 把 handler 的失败响应翻成中继的失败答复。
//
// 单独抽出来是为了让"**真正发给手机的那份**"能被测试直接断言：取值函数各自被测过，
// 但"接线接上了没有"是另一件事 —— 漏了 `Code:` 那一行不会有任何编译错误，
// 症状是客户端那条按码分支的分支永远不命中（而它自己的单测照样绿）。
func relayErrorResponse(status int, body []byte) rpcResponse {
	return rpcResponse{
		OK:     false,
		Status: status,
		Error:  localHandlerErrorText(body, status),
		Code:   localHandlerErrorCode(body),
	}
}

// localHandlerErrorText 从 handler 的响应体里取那句面向用户的文案。
//
// 取不到结构化 {"error": "..."} 时回落成状态码文案：返回空串会让手机端只能显示
// "操作失败"，那等于把服务端已经知道的原因藏起来。
func localHandlerErrorText(body []byte, status int) string {
	var failure struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &failure); err == nil && strings.TrimSpace(failure.Error) != "" {
		return strings.TrimSpace(failure.Error)
	}
	if text := strings.TrimSpace(string(body)); text != "" && len(text) <= 512 && !json.Valid(body) {
		return text
	}
	return http.StatusText(status)
}

// localHandlerErrorCode 取出 handler 给的稳定错误码（`{"code":"workspace_occupied"}`）。
//
// 只有少数失败有码（app.go 的 httpErrorCode 逐个列出来），取不到就返回空 ——
// 那些失败的客户端只能退回去匹配文案，这一点写在各自适配器的注释里。
// **有码的必须靠码判**：文案会被本地化（见 rpcResponse.Code 的说明）。
func localHandlerErrorCode(body []byte) string {
	var failure struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &failure); err != nil {
		return ""
	}
	return strings.TrimSpace(failure.Code)
}

// orEmptyJSONObject 保证写回的是合法 JSON：某些 handler 在只写 header 后没有 body，
// 直接当 RawMessage 塞进响应会让 outer 序列化失败。
func orEmptyJSONObject(body []byte) []byte {
	if len(body) == 0 || !json.Valid(body) {
		return []byte(`{}`)
	}
	return body
}
