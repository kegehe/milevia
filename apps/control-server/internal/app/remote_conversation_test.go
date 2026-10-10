package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 本文件分三层，切法与 remote_git_test.go 一致：
//
//   - 表与纯函数（op 名单、relayTarget）不构造 Server，任何环境都能跑；
//   - 端到端那几条走 relayRPCRequest → invokeLocalHandler → handler 的**真链路**，
//     因为它们要证明的正是"接线接上了没有"（路径参数有没有注入路由上下文、
//     信封里的 conversationId 有没有真的变成落点）。只测 relayTarget 的返回值
//     证明不了后半段：那一段漏了不会有任何编译错误，症状是 handler 拿到空 id；
//   - 另有一条打**真实路由**的（TestConversationStopRouteIsRegistered）—— 上面那条真链路
//     其实绕过了 chi 的路由匹配（invokeLocalHandler 手工拼请求、手工塞参数），
//     所以"路由有没有注册"只能由它来验。

// ─── 会话域 op 名单 ────────────────────────────────────────────────────────

// 这份名单是手机端能对**会话**做的全部事情。现在只有一条，但同样写死：
// 多一条就是"手机能停/改别的东西了"，必须同时改这里，逼出一次有意识的决定。
func TestConversationRemoteOperationWhitelistIsExplicit(t *testing.T) {
	server := &Server{}
	operations := server.remoteConversationOperations()
	if len(operations) != 1 {
		t.Fatalf("conversation operation count = %d, want 1", len(operations))
	}
	operation, ok := operations["conversation.stop"]
	if !ok {
		t.Fatal("conversation.stop is missing")
	}
	if operation.Method != http.MethodPost {
		t.Errorf("conversation.stop method = %s, want POST", operation.Method)
	}
	if operation.Scope != relayScopeConversation {
		t.Errorf("conversation.stop scope = %v, want relayScopeConversation", operation.Scope)
	}
	if operation.Path != "/stop" {
		t.Errorf("conversation.stop path = %q, want %q", operation.Path, "/stop")
	}
	// 写操作走查询参数是本表唯一的例外（force 与桌面端 /api/runs/{runID}/stop?force=true 同形）。
	// 把它写死在这里，免得后来有人"顺手"把它改成请求体，让两处端点对 force 的解释分家。
	if !operation.Query {
		t.Error("conversation.stop must take its params as query arguments, mirroring the desktop endpoint")
	}
	if operation.Handle == nil {
		t.Error("conversation.stop has no handler")
	}
	if len(operation.PathParams) != 0 {
		t.Errorf("conversation.stop pathParams = %v, want none", operation.PathParams)
	}
	if operation.MaxBytes != 0 {
		t.Errorf("conversation.stop maxBytes = %d, want 0（响应只有一个状态词，不可能超限）", operation.MaxBytes)
	}
}

// 会话域的 op **一律**不许把落点声明成路径参数。
//
// 这是这个作用域唯一的结构性约束：conversationID 决定请求落到哪条会话，只能来自信封
// （见 remoteOperation.Scope）。一旦有人把它写进 Path 并声明成 PathParams，params 里
// 那个同名字段就会成为第二个来源 —— 中继层会按 params 的值拼 URL、按信封的值注入
// 路由上下文，两边一旦不同就是"响应的会话与执行的会话不是同一条"，而任何一层
// 单看代码都是对的。中文注释里那句"conversationId 只能来自信封"（relayTarget）说的就是它。
func TestConversationOperationsNeverDeclareTheirConversationAsAPathParam(t *testing.T) {
	server := &Server{}
	conversationScoped := 0
	for name, operation := range server.remoteOperations() {
		if operation.Scope != relayScopeConversation {
			if len(operation.PathParams) > 0 && strings.Contains(operation.Path, "{conversationID}") {
				// 项目域里出现会话占位符同样是失控的开始，一并拦下。
				t.Errorf("%s: 项目域的 op 不许把 conversationID 拼进 Path", name)
			}
			continue
		}
		conversationScoped++
		if len(operation.PathParams) != 0 {
			t.Errorf("%s: 会话域的 op 不许声明路径参数，实际 %v", name, operation.PathParams)
		}
		if placeholders := relayTestPlaceholders(operation.Path); len(placeholders) != 0 {
			t.Errorf("%s: 会话域的 Path %q 不许带占位符 %v", name, operation.Path, placeholders)
		}
	}
	// 这条用例必须真的覆盖到东西：作用域常量拼错、或将来有人把 op 挪回项目域时，
	// 上面那个循环会一条都不检查却照样绿。
	if conversationScoped == 0 {
		t.Fatal("没有任何 relayScopeConversation 的 op，这条用例什么都没验")
	}
}

// ─── relayTarget：会话作用域的落点 ──────────────────────────────────────────

func conversationStopOperation(t *testing.T) remoteOperation {
	t.Helper()
	operation, ok := (&Server{}).remoteConversationOperations()["conversation.stop"]
	if !ok {
		t.Fatal("conversation.stop is missing")
	}
	return operation
}

func TestRelayTargetScopesConversationCallsToTheConversationPath(t *testing.T) {
	target, body, pathParams, err := relayTarget(
		conversationStopOperation(t), "p1", "conv-1", json.RawMessage(`{"force":"true"}`),
	)
	if err != nil {
		t.Fatalf("relayTarget: %v", err)
	}
	if !strings.HasPrefix(target, "/api/conversations/conv-1/stop?") {
		t.Fatalf("target = %q, want the conversation path, not the project path", target)
	}
	// 项目前缀一个字符都不许留：留了就会打到 /api/projects/... 下的另一条路由上，
	// 而那正是"两种作用域混在一起"最典型的症状。
	if strings.Contains(target, "/api/projects/") {
		t.Fatalf("target = %q must not carry the project prefix", target)
	}
	if !strings.Contains(target, "force=true") {
		t.Fatalf("target = %q must carry force", target)
	}
	// 落点已经在路径里了，不该再往查询串里塞第二份 —— 同一个值出现两次，
	// 将来一旦分叉就没人说得清哪份作数。
	if strings.Contains(target, "conversationId=") {
		t.Fatalf("target = %q must not duplicate the conversation id as a query param", target)
	}
	// 路径参数必须交给 invokeLocalHandler 的 extraParams：合成请求没有真的走路由匹配，
	// handler 里 chi.URLParam(r, "conversationID") 读的就是这一份。漏了它 handler 拿到空串。
	if pathParams["conversationID"] != "conv-1" {
		t.Fatalf("pathParams = %v, want conversationID for the handler", pathParams)
	}
	// 这条 op 是**查询参数类**（Query: true，理由见 op 表里的说明）：params 全部进了查询串，
	// 因此没有请求体。写在这里是为了钉住这一点 —— 它意味着 force 不会同时以两种形式出现。
	if len(body) != 0 {
		t.Fatalf("body = %s, want none for a query-style operation", body)
	}
}

// 会话作用域的落点只能来自信封：params 里塞一个同名字段不算数。
func TestRelayTargetKeepsConversationStopOnTheEnvelopeConversation(t *testing.T) {
	target, _, pathParams, err := relayTarget(
		conversationStopOperation(t), "p1", "conv-real",
		json.RawMessage(`{"conversationId":"conv-forged"}`),
	)
	if err != nil {
		t.Fatalf("relayTarget: %v", err)
	}
	if strings.Contains(target, "conv-forged") || pathParams["conversationID"] != "conv-real" {
		t.Fatalf("target = %q pathParams = %v：落点被 params 改掉了", target, pathParams)
	}
}

func TestRelayTargetRequiresAWellFormedEnvelopeConversation(t *testing.T) {
	// 缺了信封里的会话：报错，而不是拼出一个 /api/conversations//stop 让 chi 去猜。
	if _, _, _, err := relayTarget(conversationStopOperation(t), "p1", "", nil); err == nil {
		t.Fatal("an empty envelope conversationId must be rejected")
	}
	if _, _, _, err := relayTarget(conversationStopOperation(t), "p1", "   ", nil); err == nil {
		t.Fatal("a blank envelope conversationId must be rejected")
	}
	// 它会被拼进 URL 路径：与 projectId 同样的理由，路径语法与编码字符一律挡在外面。
	for _, bad := range []string{"../etc", "a/b", "a?b", "a#b", "a%2Fb"} {
		if _, _, _, err := relayTarget(conversationStopOperation(t), "p1", bad, nil); err == nil {
			t.Errorf("conversationId %q must be rejected before building the request", bad)
		}
	}
}

// ─── 端到端：中继 → handler ────────────────────────────────────────────────

// relayConversation 走的是与手机端完全相同的入口：/api/remote/rpc 上那个唯一的端点。
// 刻意不直接调 handler —— 那会跳掉 relayTarget 的路径参数注入，而"注入漏了"正是
// 这条链路上唯一不会被编译器发现的一步。
func relayConversation(t *testing.T, server *Server, body map[string]any) (int, map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal relay body: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/remote/rpc", bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.relayRPCRequest(response, request)
	decoded := map[string]any{}
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode relay response %q: %v", response.Body.String(), err)
	}
	return response.Code, decoded
}

// 落点那条路由必须真的注册上。
//
// 上面那几条端到端用例走的是中继的合成请求：`invokeLocalHandler` **不查路由表**，
// 它按 op 声明的路径手工拼一个请求、手工塞路由参数。所以少注册一条路由时它们照样绿，
// 而真实链路上（桌面端直接打这个端点、或任何走 chi 的调用）会拿到 404。
// 这条就是补上那个洞：真路由 + 真匹配。
func TestConversationStopRouteIsRegistered(t *testing.T) {
	server, _, conversationID := seedTaskConversation(t)
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/conversations/"+conversationID+"/stop", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("stop route status = %d body = %s（404 就是没注册）", response.Code, response.Body.String())
	}
	var payload struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode stop response %q: %v", response.Body.String(), err)
	}
	if payload.Status != "idle" {
		t.Fatalf("status = %q, want idle", payload.Status)
	}
}

// seededRun 是一条直接写进库里的 run 行。
//
// created_at 由用例自己给：**活跃的那一轮**按 created_at 取最新，而同一个时刻插入的两行
// 排序是任意的 —— 用同一个 now 去断言"停的是最新那条"会变成一条时灵时不灵的用例。
type seededRun struct {
	id     string
	status string
	at     time.Time
}

func seedConversationRuns(t *testing.T, server *Server, conversationID string, runs []seededRun) {
	t.Helper()
	for _, run := range runs {
		if _, err := server.db.Exec(`insert into runs (id,conversation_id,status,created_at) values (?,?,?,?)`, run.id, conversationID, run.status, run.at); err != nil {
			t.Fatalf("insert run %s: %v", run.id, err)
		}
	}
}

func relayConversationStop(t *testing.T, server *Server, params map[string]any) (int, map[string]any) {
	t.Helper()
	if params == nil {
		params = map[string]any{}
	}
	code, payload := relayConversation(t, server, map[string]any{
		"op":             "conversation.stop",
		"projectId":      "project",
		"conversationId": "conversation",
		"params":         params,
	})
	if code != http.StatusOK {
		t.Fatalf("relay transport status = %d body %v", code, payload)
	}
	return code, payload
}

// 没有活跃的那一轮时，停止是一次**幂等的空操作**：返回 idle 而不是报错。
//
// 判据刻意不用 404/409：手机端按下停止与那一轮恰好自己跑完是常态竞态，
// 报错等于把"其实已经停了"说成失败，用户会去点第二下、第三下。
func TestRelayConversationStopIsIdleWhenNothingIsActive(t *testing.T) {
	server, _, conversationID := seedTaskConversation(t)
	seedConversationRuns(t, server, conversationID, []seededRun{{"finished-run", "completed", time.Now().UTC()}})

	_, payload := relayConversationStop(t, server, nil)
	if payload["ok"] != true {
		t.Fatalf("payload = %v", payload)
	}
	data, _ := payload["data"].(map[string]any)
	if data["status"] != "idle" {
		t.Fatalf("status = %v, want idle（已完成的那一轮不算活跃）", data["status"])
	}
}

// 有一轮在跑：停掉它。停的是**最新**的那个活跃 run —— 会话上一轮的进程会接住
// 后来的消息成为排队回合，所以"用户刚发的那条"才是他按停止时想停的东西。
//
// 这一条走的是"那一轮没有活跃会话"的那一支（只有 cancel，没有 sessions）：
// 停的是**选定那一轮**，更早的那一轮不碰。而线上最常见的那一支（有会话）在下一个用例里 ——
// 两支的行为**不一样**（那一支因为一个进程承载整条会话的排队回合，会连排队的一起停），
// 所以两支都要有用例，否则删掉其中一支的判据不会有任何测试红。
func TestRelayConversationStopStopsTheNewestActiveRun(t *testing.T) {
	server, _, conversationID := seedTaskConversation(t)
	now := time.Now().UTC()
	seedConversationRuns(t, server, conversationID, []seededRun{
		{"older-run", "running", now.Add(-time.Minute)},
		{"newest-run", "running", now},
	})

	stopped := map[string]bool{}
	server.mu.Lock()
	server.cancels["older-run"] = func() { stopped["older-run"] = true }
	server.cancels["newest-run"] = func() { stopped["newest-run"] = true }
	server.mu.Unlock()

	_, payload := relayConversationStop(t, server, nil)
	if payload["ok"] != true {
		t.Fatalf("payload = %v", payload)
	}
	data, _ := payload["data"].(map[string]any)
	if data["status"] != "stopping" {
		t.Fatalf("status = %v, want stopping", data["status"])
	}
	if !stopped["newest-run"] {
		t.Fatal("最新那一轮没有被停")
	}
	if stopped["older-run"] {
		t.Fatal("更早那一轮被一并停掉了（这一支只有 cancel 记录，无 force 时只该停选定那一轮）")
	}
}

// **线上最常见的那一支**：会话里只有一个活跃 run，且那条会话有一个活着的流式会话。
//
// 这一条与上一条走的是完全不同的分支（stopRunByID 的 session 分支，而不是末尾那个
// 裸 cancel）：线上每一次"停掉 AI 正在跑的那一轮"都落在这里 —— 流式准入时就写了
// runContexts，sessions 里也有那条会话。所以这一支里 `ensureStreamingStopIsIsolated`
// 与 `markStopping` 一个都不能少，而它们只有在这个形状下才会被执行到。
func TestRelayConversationStopStopsTheStreamingSession(t *testing.T) {
	server, _, conversationID := seedTaskConversation(t)
	now := time.Now().UTC()
	seedConversationRuns(t, server, conversationID, []seededRun{{"running-run", "running", now}})
	session := newQueuedAgentSession()
	server.mu.Lock()
	server.sessions[conversationID] = &activeAgentSession{agent: session, activeRunID: "running-run"}
	server.runContexts["running-run"] = conversationID
	server.mu.Unlock()

	_, payload := relayConversationStop(t, server, nil)
	if payload["ok"] != true {
		t.Fatalf("payload = %v", payload)
	}
	data, _ := payload["data"].(map[string]any)
	if data["status"] != "stopping" {
		t.Fatalf("status = %v, want stopping", data["status"])
	}
	select {
	case <-session.done:
	default:
		t.Fatal("这一支没有真的把流式会话停掉")
	}
	// 会话被标记为"正在退役"：它在 stopRunByID 里由 markStopping 维护，漏掉的话
	// 这条会话在进程退出前还能被新任务准入（那正是 stopping 标记存在的理由）。
	server.mu.Lock()
	stopping := server.sessions[conversationID] != nil && server.sessions[conversationID].stopping
	server.mu.Unlock()
	if !stopping {
		t.Fatal("会话没有被标记为正在停止")
	}
}

// 已经不存在的会话是 **404**，不是 `200 idle`。
//
// 少了这道判据，一个被删掉的会话 id 会拿到"它现在没在跑"—— 那是把"这条会话没了"说成
// "它闲着"，用户会一直等一个不存在的会话。手机端把 404 显示成"会话不存在或已被删除。"。
func TestRelayConversationStopRejectsUnknownConversation(t *testing.T) {
	server, _, _ := seedTaskConversation(t)
	code, payload := relayConversation(t, server, map[string]any{
		"op":             "conversation.stop",
		"projectId":      "project",
		"conversationId": "0b9d3f16-2f1e-4a5c-9d47-6a1b2c3d4e5f",
		"params":         map[string]any{},
	})
	if code != http.StatusOK {
		t.Fatalf("relay transport status = %d body %v", code, payload)
	}
	if payload["ok"] != false {
		t.Fatalf("payload = %v，want a refusal", payload)
	}
	if payload["status"] != float64(http.StatusNotFound) {
		t.Fatalf("status = %v, want 404", payload["status"])
	}
	if message, _ := payload["error"].(string); !containsChinese(message) {
		t.Fatalf("不存在会话的拒绝文案不是中文：%q", message)
	}
}

// 还有排队中的回合时，无 force 的停止会被拒，并且**带稳定机器码**回来 ——
// 手机端据此弹"强制停止"确认，而不是去匹配那句会被本地化的文案。
func TestRelayConversationStopAsksForForceWhenOtherRunsAreQueued(t *testing.T) {
	server, _, conversationID := seedTaskConversation(t)
	now := time.Now().UTC()
	seedConversationRuns(t, server, conversationID, []seededRun{
		{"running-run", "running", now.Add(-time.Minute)},
		{"queued-run", "queued", now},
	})
	session := newQueuedAgentSession()
	server.mu.Lock()
	server.sessions[conversationID] = &activeAgentSession{agent: session, activeRunID: "running-run"}
	// runContexts 也要照生产填上（准入时就写了，见 app.go 的 `s.runContexts[runID] = conversation.ID`）：
	// 少了它，stopRunByID 会走"没有 cancel 记录"那条兜底分支再去查一次库 —— 结果一样是
	// active_runs_present，但那不是线上真走的那条路（线上是"有 cancel + 有会话"这一支）。
	server.runContexts["running-run"] = conversationID
	server.runContexts["queued-run"] = conversationID
	server.mu.Unlock()

	_, payload := relayConversationStop(t, server, nil)
	if payload["ok"] != false {
		t.Fatalf("payload = %v, want a refusal", payload)
	}
	if payload["status"] != float64(http.StatusConflict) {
		t.Fatalf("status = %v, want 409", payload["status"])
	}
	if payload["code"] != "active_runs_present" {
		t.Fatalf("code = %v, want active_runs_present（手机端按码分支，文案会被本地化）", payload["code"])
	}
	select {
	case <-session.done:
		t.Fatal("被拒的那次请求不该真的把会话停掉")
	default:
	}

	// 带 force 再来一次：这次停掉整条会话（含排队回合）。
	_, forced := relayConversationStop(t, server, map[string]any{"force": "true"})
	if forced["ok"] != true {
		t.Fatalf("forced payload = %v", forced)
	}
	data, _ := forced["data"].(map[string]any)
	if data["status"] != "stopping" {
		t.Fatalf("forced status = %v, want stopping", data["status"])
	}
	select {
	case <-session.done:
	default:
		t.Fatal("强制停止没有真的把会话停掉")
	}
}

// 编排对话由编排面板接管：这里拒绝，而且**给的是中文**。
//
// 原文是英文，不翻译的话手机端看到的是"当前操作与进行中的操作冲突，请稍后重试。：
// stop automatic orchestration from the orchestration controls" —— 既没说清发生了什么，
// 也没说该去哪停。手机端的停止键是它唯一能碰到的停止入口，所以这条文案必须落地。
func TestRelayConversationStopRejectsOrchestrationConversations(t *testing.T) {
	server, projectID, conversationID := seedTaskConversation(t)
	// 这条对话被自动编排接管（isOrchestrationConversation 的唯一判据就是这张表里有它的记录），
	// 记录挂在一条真实的编排作业上 —— job_id 有外键，凭空造一条只会在插入时报 FK 失败。
	taskID := createTaskForTest(t, server.routes(), projectID, "Reject orchestration stop")
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into task_orchestration_jobs (id,project_id,task_id,queue_position,status,policy_snapshot,created_at,updated_at) values ('job',?,?,1,'queued','{}',?,?)`, projectID, taskID, now, now); err != nil {
		t.Fatalf("insert orchestration job: %v", err)
	}
	if _, err := server.db.Exec(`insert into git_task_records (job_id,base_dev_sha,task_branch,worktree_path,conversation_id,created_at,updated_at) values ('job','','','',?,?,?)`, conversationID, now, now); err != nil {
		t.Fatalf("insert orchestration record: %v", err)
	}
	seedConversationRuns(t, server, conversationID, []seededRun{{"orchestration-run", "running", now}})

	_, payload := relayConversationStop(t, server, nil)
	if payload["ok"] != false {
		t.Fatalf("payload = %v, want a refusal", payload)
	}
	if payload["status"] != float64(http.StatusConflict) {
		t.Fatalf("status = %v, want 409", payload["status"])
	}
	message, _ := payload["error"].(string)
	if !containsChinese(message) {
		t.Fatalf("拒绝对编排对话的停止时给的是英文：%q", message)
	}
	if strings.Contains(message, "stop automatic orchestration") {
		t.Fatalf("英文原文直接透到手机端了：%q", message)
	}
	if !strings.Contains(message, "编排") {
		t.Fatalf("文案没说清该去哪停：%q", message)
	}
}
