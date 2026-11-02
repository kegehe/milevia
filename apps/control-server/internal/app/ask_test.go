package app

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

func askFixtureQuestions() []askQuestion {
	return []askQuestion{{
		ID:       "q1",
		Header:   "数据库",
		Question: "迁移到哪个数据库？",
		Kind:     "choice",
		Options:  []askOption{{Label: "PostgreSQL"}, {Label: "SQLite"}},
	}}
}

// waitForAskID 等 beginAsk 把 waiter 注册进内存表，返回 askID。
// answer 必须走真实路由：answerAsk 用 chi.URLParam 取 askID，直接调 handler 会拿到空串。
func answer(t *testing.T, server *Server, askID, body string) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/asks/"+askID,
		bytes.NewBufferString(body)))
	return response
}

func waitForAskID(t *testing.T, server *Server, conversationID string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		server.mu.Lock()
		waiter := server.askForConversationLocked(conversationID)
		server.mu.Unlock()
		if waiter != nil {
			return waiter.askID
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("提问没有注册到内存表")
	return ""
}

func askEventTypes(t *testing.T, server *Server, conversationID string) []string {
	t.Helper()
	rows, err := server.db.Query(
		"select type from events where conversation_id=? and type like 'ask.%' order by created_at asc, rowid asc", conversationID)
	if err != nil {
		t.Fatalf("query ask events: %v", err)
	}
	defer rows.Close()
	types := []string{}
	for rows.Next() {
		var typ string
		if err := rows.Scan(&typ); err != nil {
			t.Fatalf("scan ask event: %v", err)
		}
		types = append(types, typ)
	}
	return types
}

func TestAskAnswerFlow(t *testing.T) {
	server := newTestServer(t)
	runID := seedConversation(t, server, "project", "conversation")

	outcomes := make(chan askOutcome, 1)
	go func() {
		outcome, err := server.beginAsk(context.Background(), "conversation", runID, askFixtureQuestions())
		if err != nil {
			t.Errorf("beginAsk: %v", err)
		}
		outcomes <- outcome
	}()

	askID := waitForAskID(t, server, "conversation")
	response := answer(t, server, askID, `{"answers":{"q1":"SQLite"}}`)
	if response.Code != http.StatusAccepted {
		t.Fatalf("answer status = %d body=%s", response.Code, response.Body.String())
	}

	outcome := <-outcomes
	if outcome.status != askStatusAnswered {
		t.Fatalf("outcome status = %q, want answered", outcome.status)
	}
	if outcome.answers["q1"] != "SQLite" {
		t.Fatalf("answer q1 = %q", outcome.answers["q1"])
	}

	types := askEventTypes(t, server, "conversation")
	if len(types) != 2 || types[0] != "ask.pending" || types[1] != "ask.answered" {
		t.Fatalf("event sequence = %v", types)
	}
	var actionURL string
	if err := server.db.QueryRow(`select action_url from notifications where type='ask.pending'`).Scan(&actionURL); err != nil {
		t.Fatalf("ask notification: %v", err)
	}
	if actionURL != "/projects/project/conversations/conversation" {
		t.Fatalf("ask notification url = %q", actionURL)
	}
}

func TestAskTimeoutProducesExpired(t *testing.T) {
	server := newTestServer(t)
	runID := seedConversation(t, server, "project", "conversation")
	server.config.UserAskTimeout = 40 * time.Millisecond

	outcome, err := server.beginAsk(context.Background(), "conversation", runID, askFixtureQuestions())
	if err != nil {
		t.Fatalf("beginAsk: %v", err)
	}
	if outcome.status != askStatusExpired {
		t.Fatalf("outcome = %q, want expired", outcome.status)
	}
	types := askEventTypes(t, server, "conversation")
	if len(types) != 2 || types[1] != "ask.expired" {
		t.Fatalf("event sequence = %v", types)
	}
	// 超时后必须能从内存表里摘干净，否则下一轮提问会被"已有未回答的问题"挡住。
	server.mu.Lock()
	pending := len(server.asks)
	server.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending asks = %d, want 0", pending)
	}
}

func TestAskCancelledWhenRunResolves(t *testing.T) {
	server := newTestServer(t)
	runID := seedConversation(t, server, "project", "conversation")

	outcomes := make(chan askOutcome, 1)
	go func() {
		outcome, err := server.beginAsk(context.Background(), "conversation", runID, askFixtureQuestions())
		if err != nil {
			t.Errorf("beginAsk: %v", err)
		}
		outcomes <- outcome
	}()

	waitForAskID(t, server, "conversation")
	// 走与 run 收尾完全相同的那条路径：审批结算时提问必须一起被取消。
	server.resolveRunApprovals(runID, "deny")

	outcome := <-outcomes
	if outcome.status != askStatusCancelled {
		t.Fatalf("outcome = %q, want cancelled", outcome.status)
	}
	types := askEventTypes(t, server, "conversation")
	if len(types) != 2 || types[1] != "ask.cancelled" {
		t.Fatalf("event sequence = %v", types)
	}
}

func TestAskRejectsSecondPendingQuestion(t *testing.T) {
	server := newTestServer(t)
	runID := seedConversation(t, server, "project", "conversation")

	first := make(chan askOutcome, 1)
	go func() {
		outcome, _ := server.beginAsk(context.Background(), "conversation", runID, askFixtureQuestions())
		first <- outcome
	}()
	waitForAskID(t, server, "conversation")

	if _, err := server.beginAsk(context.Background(), "conversation", runID, askFixtureQuestions()); err == nil {
		t.Fatal("同一会话的第二个提问应当被拒绝")
	}

	askID := waitForAskID(t, server, "conversation")
	answer(t, server, askID, `{"action":"skip"}`)
	<-first
}

func TestAskPerRunBudget(t *testing.T) {
	server := newTestServer(t)
	runID := seedConversation(t, server, "project", "conversation")
	server.mu.Lock()
	server.askBudgets["conversation"] = &askBudget{runID: runID, count: askMaxPerRun}
	server.mu.Unlock()

	if _, err := server.beginAsk(context.Background(), "conversation", runID, askFixtureQuestions()); err == nil {
		t.Fatal("超出提问次数上限时应返回错误")
	}
}

func TestParseAskQuestionsValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
		ok   bool
	}{
		{"choice 缺选项", `{"questions":[{"id":"q","header":"h","question":"?"}]}`, false},
		{"选项过多", `{"questions":[{"id":"q","header":"h","question":"?","options":[{"label":"1"},{"label":"2"},{"label":"3"},{"label":"4"},{"label":"5"},{"label":"6"},{"label":"7"},{"label":"8"},{"label":"9"}]}]}`, false},
		{"id 重复", `{"questions":[{"id":"q","header":"a","question":"?","options":[{"label":"1"},{"label":"2"}]},{"id":"q","header":"b","question":"?","options":[{"label":"1"},{"label":"2"}]}]}`, false},
		{"数字题缺 min", `{"questions":[{"id":"q","header":"h","question":"?","kind":"number"}]}`, false},
		{"数字题 min>max", `{"questions":[{"id":"q","header":"h","question":"?","kind":"number","min":10,"max":1}]}`, false},
		{"正常选择题", `{"questions":[{"id":"q","header":"h","question":"?","options":[{"label":"1"},{"label":"2"}]}]}`, true},
		{"正常文本题", `{"questions":[{"id":"q","header":"h","question":"?","kind":"text"}]}`, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := parseAskQuestions(json.RawMessage(testCase.body))
			if testCase.ok && err != nil {
				t.Fatalf("期望通过，得到 %v", err)
			}
			if !testCase.ok && err == nil {
				t.Fatal("期望报错，实际通过")
			}
		})
	}
}

// stubAgentSession 只为了满足 Server.Close 对 sessions 的收尾（它会调 agent.Stop）：
// 不放一个真实 agent，也不能放 nil —— nil 会让 Close 直接空指针崩溃，而崩溃发生在
// t.Cleanup 里，报出来的是"临时目录删不掉"，与真正的原因差着十万八千里。
type stubAgentSession struct{}

func (stubAgentSession) Send(AgentRunRequest, AgentRunSink) error { return nil }
func (stubAgentSession) Stop()                                    {}
func (stubAgentSession) Done() <-chan error                       { return nil }

// attachFakeSession 让 MCP 端点的凭据校验能通过：与真实会话一样按 conversationID 索引。
func attachFakeSession(server *Server, conversationID, runID, token string) {
	server.mu.Lock()
	server.sessions[conversationID] = &activeAgentSession{
		agent:         stubAgentSession{},
		approvalToken: token,
		activeRunID:   runID,
	}
	server.mu.Unlock()
}

func mcpRequest(t *testing.T, server *Server, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, interactionMCPPath, strings.NewReader(body))
	request.Header.Set("X-Auto-Conversation-ID", "conversation")
	request.Header.Set("X-Auto-Approval-Token", token)
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, request)
	return response
}

func TestInteractionMCPHandshakeAndToolCall(t *testing.T) {
	server := newTestServer(t)
	runID := seedConversation(t, server, "project", "conversation")
	attachFakeSession(server, "conversation", runID, "token")

	list := mcpRequest(t, server, "token", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), "ask_user") {
		t.Fatalf("tools/list = %d body=%s", list.Code, list.Body.String())
	}

	type mcpReply struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	replies := make(chan mcpReply, 1)
	go func() {
		response := mcpRequest(t, server, "token", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"ask_user","arguments":{"questions":[{"id":"q1","header":"数据库","question":"选哪个？","options":[{"label":"PostgreSQL"},{"label":"SQLite"}]}]}}}`)
		var reply mcpReply
		_ = json.Unmarshal(response.Body.Bytes(), &reply)
		replies <- reply
	}()

	askID := waitForAskID(t, server, "conversation")
	answer(t, server, askID, `{"answers":{"q1":"SQLite"}}`)

	reply := <-replies
	if reply.Result.IsError {
		t.Fatalf("工具调用返回错误：%+v", reply.Result)
	}
	if len(reply.Result.Content) == 0 || !strings.Contains(reply.Result.Content[0].Text, "SQLite") {
		t.Fatalf("工具结果没有带上答案：%+v", reply.Result.Content)
	}
}

func TestInteractionMCPRejectsUnknownTokenOnToolCall(t *testing.T) {
	server := newTestServer(t)
	runID := seedConversation(t, server, "project", "conversation")
	attachFakeSession(server, "conversation", runID, "token")

	// 握手只校验"带了 token"，归属校验落在 tools/call —— CLI 刚启动时会话行还没登记。
	handshake := mcpRequest(t, server, "wrong-token", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if handshake.Code != http.StatusOK {
		t.Fatalf("握手不应要求会话归属：status = %d", handshake.Code)
	}

	call := mcpRequest(t, server, "wrong-token", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"ask_user","arguments":{"questions":[{"id":"q","header":"h","question":"?","options":[{"label":"1"},{"label":"2"}]}]}}}`)
	if call.Code != http.StatusUnauthorized {
		t.Fatalf("tools/call 用错 token 应为 401，实际 %d", call.Code)
	}
}

func TestInteractionMCPRequiresTokenForHandshake(t *testing.T) {
	server := newTestServer(t)
	request := httptest.NewRequest(http.MethodPost, interactionMCPPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("没有 token 的握手应为 401，实际 %d", response.Code)
	}
}

func TestAnswerAskAfterTerminalReturnsConflict(t *testing.T) {
	server := newTestServer(t)
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/asks/missing",
		bytes.NewBufferString(`{"answers":{}}`)))
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", response.Code)
	}
}

func TestReconcilePendingAsksCancelsOrphans(t *testing.T) {
	server := newTestServer(t)
	runID := seedConversation(t, server, "project", "conversation")
	if _, err := server.db.Exec(
		"insert into events (id,conversation_id,run_id,type,payload,created_at) values (?,?,?,?,?,?)",
		"orphan", "conversation", runID, "ask.pending",
		`{"askId":"orphan-ask","status":"pending"}`, time.Now().UTC()); err != nil {
		t.Fatalf("seed pending ask: %v", err)
	}

	server.reconcilePendingAsks(context.Background())

	types := askEventTypes(t, server, "conversation")
	found := false
	for _, typ := range types {
		if typ == "ask.cancelled" {
			found = true
		}
	}
	if !found {
		t.Fatalf("孤儿 pending 没有被取消，事件 = %v", types)
	}
}

func TestInteractionServerIsInjectedForWindowsRun(t *testing.T) {
	server := newTestServer(t)
	seedConversation(t, server, "project", "conversation")

	injection := server.prepareMCPInjectionWith(context.Background(), "project", t.TempDir(), "claude-code",
		server.localRunnerID(), "run-ask", mcpInteractionIdentity{ConversationID: "conversation", ApprovalToken: "token"})
	if !strings.Contains(injection.JSON, interactionServerName) {
		t.Fatalf("注入内容里没有内置提问 server：%s", injection.JSON)
	}
	if !strings.Contains(injection.JSON, interactionMCPPath) {
		t.Fatalf("注入内容里没有交互端点：%s", injection.JSON)
	}
	// 内置 server 不能混进"用户配了哪些"的清单，否则界面会凭空多一条。
	for _, item := range injection.AutoApproveTools {
		if strings.Contains(item, interactionServerName) {
			t.Fatalf("内置 server 不应进自动放行清单：%v", injection.AutoApproveTools)
		}
	}
}

func TestLegacyInjectionDoesNotAddInteractionServer(t *testing.T) {
	server := newTestServer(t)
	seedConversation(t, server, "project", "conversation")
	injection := server.prepareMCPInjection(context.Background(), "project", t.TempDir(), "claude-code",
		server.localRunnerID(), "run-plain")
	if strings.Contains(injection.JSON, interactionServerName) {
		t.Fatalf("不启用身份时不应注入提问 server：%s", injection.JSON)
	}
}

func TestAskExemptFromApprovalHook(t *testing.T) {
	server := newTestServer(t)
	runID := seedConversation(t, server, "project", "conversation")
	attachFakeSession(server, "conversation", runID, "token")

	body := `{"hook_event_name":"PreToolUse","tool_name":"` + interactionApprovalToolName() + `","tool_input":{"questions":[]}}`
	request := httptest.NewRequest(http.MethodPost, "/api/internal/approvals/wait", strings.NewReader(body))
	request.Header.Set("X-Auto-Conversation-ID", "conversation")
	request.Header.Set("X-Auto-Approval-Token", "token")
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "allow") {
		t.Fatalf("提问工具应被直接放行：%s", response.Body.String())
	}
	// 放行不等于弹审批：不能产生 pending 事件。
	if types := askEventTypes(t, server, "conversation"); len(types) != 0 {
		t.Fatalf("不应产生 ask 事件：%v", types)
	}
}
