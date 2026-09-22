package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// replayableEventsPredicate 是黑名单而不是白名单：将来 CLI 新增的事件类型必须默认被保留，
// 宁可多带一条，也不能让某个新事件在回放里凭空消失。这里把"会被渲染的类型"逐个钉住，
// 顺便钉住"没见过的类型也放行"。
func TestConversationPageKeepsUnknownEventTypes(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC().Add(-time.Hour)
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,permission_mode,title,last_activity_at,claude_initialized,is_current,created_at) values ('conversation','project','00000000-0000-4000-8000-000000000000','idle','approval_required','conversation',?,1,1,?)`, now, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	if _, err := server.db.Exec(`insert into runs (id,conversation_id,status,created_at) values ('run','conversation','completed',?)`, now); err != nil {
		t.Fatalf("insert run: %v", err)
	}

	// 这些类型都会被时间线渲染（工具调用、审批、CLI 输出、诊断），或者只是"还没见过"，
	// 一律必须原样返回。
	kept := []struct{ id, eventType, payload string }{
		{"assistant-0", "assistant", `{}`},
		{"user-0", "user", `{}`},
		{"approval-0", "approval.pending", `{"approvalId":"a","toolInput":{}}`},
		{"stderr-0", "stderr", `{"message":"boom"}`},
		{"error-0", "error", `{"message":"boom"}`},
		{"item-0", "item.started", `{"item":{"id":"i","type":"command_execution"}}`},
		{"run-0", "run.completed", `{}`},
		{"future-0", "conversation.future_event", `{}`},
		// system 最容易被"顺手整类排除"掉：保留策略那边确实把 system 当过程数据先裁。
		// 但时间线要渲染它的压缩 / 重试 / 后台任务卡片，历史分页必须留着它。
		{"system-status-0", "system", `{"type":"system","subtype":"status","status":"compacting"}`},
		{"system-retry-0", "system", `{"type":"system","subtype":"api_retry","attempt":2,"max_retries":5}`},
		{"system-task-0", "system", `{"type":"system","subtype":"task_started","description":"跑测试"}`},
	}
	for index, event := range kept {
		createdAt := now.Add(time.Duration(index) * time.Millisecond)
		if _, err := server.db.Exec(`insert into events (id,conversation_id,run_id,type,payload,created_at) values (?,?,?,?,?,?)`, event.id, "conversation", "run", event.eventType, event.payload, createdAt); err != nil {
			t.Fatalf("insert %s: %v", event.eventType, err)
		}
	}
	// 同一条 system 类型里混着的遥测：整类排除 system 能"顺手"排掉它，代价是连上面那些
	// 卡片一起丢掉 —— 所以它只能靠 payload 单独判定。
	if _, err := server.db.Exec(`insert into events (id,conversation_id,run_id,type,payload,created_at) values (?,?,?,?,?,?)`,
		"thinking-0", "conversation", "run", "system", `{"type":"system","subtype":"thinking_tokens","estimated_tokens":875}`, now.Add(30*time.Second)); err != nil {
		t.Fatalf("insert thinking_tokens: %v", err)
	}
	// 过程遥测同样塞满，且时间戳更晚 —— 旧实现下它们会把上面这些挤出首屏。
	seedEvents(t, server, "conversation", "run", "stream_event", "stream-", 50, now.Add(time.Minute))
	seedEvents(t, server, "conversation", "run", "tool_progress", "progress-", 50, now.Add(time.Minute))
	seedEvents(t, server, "conversation", "run", "usage.updated", "usage-", 50, now.Add(time.Minute))

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/conversations/conversation?limit=50", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("page status: %d body=%s", response.Code, response.Body.String())
	}
	var page struct {
		Events     []Event `json:"events"`
		HasMore    bool    `json:"hasMore"`
		NextCursor string  `json:"nextCursor"`
	}
	if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	got := make(map[string]bool, len(page.Events))
	for _, event := range page.Events {
		got[event.ID] = true
	}
	for _, event := range kept {
		if !got[event.id] {
			t.Fatalf("replayable event %s (%s) was dropped: %#v", event.id, event.eventType, page.Events)
		}
	}
	if len(page.Events) != len(kept) {
		t.Fatalf("page has %d events, want only the %d replayable ones: %#v", len(page.Events), len(kept), page.Events)
	}
	if page.HasMore || page.NextCursor != "" {
		t.Fatalf("telemetry kept hasMore alive: hasMore=%v cursor=%q", page.HasMore, page.NextCursor)
	}
}

// 两条共用谓词的语义逐个钉死：它们决定"这条事件到不到得了屏幕"。答错的代价不是带宽，
// 而是界面上凭空少一段，所以这里直接对 SQL 求值，而不是只跑上层端点 —— 上层只会表现为
// "返回空"，看不出是哪一条式子错了。
//
// **必须按调用方的极性测**：`unrenderedSystemSubtypePredicate` 是"排除条件"，调用方一律
// 写成 `... and not <它>`。只测它自己的真假会让结合性错误蒙混过关 —— 实测踩过：
// 谓词少了一层括号时，"不是 system 且（子类型命中）"被解析成 NOT 先结合，于是
// assistant 这类普通事件在 `not (...)` 之后整批消失、整页返回空，而单独求值它仍然是 false，
// 一张表全绿。所以这里测的是 replayableEventsPredicate / remoteNoticeReplayPredicate
// 本身，也就是 WHERE 里真正的那个条件（为真 = 这条会被带回去）。
func TestReplayPredicatesKeepWhatTheClientRenders(t *testing.T) {
	server := newTestServer(t)
	cases := []struct {
		name      string
		eventType string
		payload   string
		// replayable 是会话历史分页的结论，notice 是手机端快照运行记录的结论。
		replayable bool
		notice     bool
	}{
		// —— 会话内容：照旧全部保留 ——
		{"助手消息", "assistant", `{}`, true, false},
		{"用户消息", "user", `{}`, true, false},
		{"未见过的事件类型", "conversation.future_event", `{}`, true, false},
		// —— system 里真正会渲染成卡片的子类型 ——
		{"正在压缩上下文", "system", `{"type":"system","subtype":"status","status":"compacting"}`, true, true},
		{"压缩结果", "system", `{"type":"system","subtype":"status","compact_result":"success"}`, true, true},
		{"压缩摘要", "system", `{"type":"system","subtype":"compact_boundary"}`, true, true},
		{"API 重试", "system", `{"type":"system","subtype":"api_retry","attempt":2}`, true, true},
		{"后台任务启动", "system", `{"type":"system","subtype":"task_started","description":"跑测试"}`, true, true},
		{"任务转入后台", "system", `{"type":"system","subtype":"task_updated","patch":{"is_backgrounded":true}}`, true, true},
		// 没见过的子类型默认保留：将来 CLI 新增的卡片不能整个消失。
		{"未来才有的子类型", "system", `{"type":"system","subtype":"某个将来才有的子类型","x":1}`, true, true},
		// —— 整类遥测：分页排除，本来也不在运行记录的类型表里 ——
		{"流式分片", "stream_event", `{}`, false, false},
		{"工具心跳", "tool_progress", `{}`, false, false},
		{"用量刷新", "usage.updated", `{}`, false, false},
		// —— 空转的 system 子类型：两条路都不该占位置 ——
		{"等待响应心跳", "system", `{"type":"system","subtype":"status","status":"requesting"}`, false, false},
		{"等待响应心跳(带空格)", "system", `{"type": "system", "subtype": "status", "status": "requesting"}`, false, false},
		{"空的压缩结果按没有处理", "system", `{"type":"system","subtype":"status","compact_result":""}`, false, false},
		{"CLI init", "system", `{"type":"system","subtype":"init","tools":[]}`, false, false},
		{"工具进度心跳", "system", `{"type":"system","subtype":"task_progress","task_id":"t"}`, false, false},
		{"普通状态推进", "system", `{"type":"system","subtype":"task_updated","patch":{"status":"running"}}`, false, false},
		{"历史遗留的 thinking_tokens", "system", `{"type":"system","subtype":"thinking_tokens","estimated_tokens":875}`, false, false},
		// —— 诊断与工具确认：运行记录要带，分页当然也带 ——
		{"执行失败", "run.failed", `{"message":"boom"}`, true, true},
		{"执行错误", "error", `{"message":"boom"}`, true, true},
		{"工具确认", "approval.pending", `{"approvalId":"a","toolInput":{}}`, true, true},
		// stderr 刻意不在运行记录的类型表里（逐行产生会把配额挤爆），但仍属于会话历史。
		{"CLI 输出", "stderr", `{"message":"boom"}`, true, false},
	}
	for _, testCase := range cases {
		row := func(predicate string) int {
			t.Helper()
			query := `select case when ` + predicate + ` then 1 else 0 end from (select ? as type, ? as payload, ? as conversation_id, ? as run_id, ? as id, ? as created_at) e`
			var kept int
			if err := server.db.QueryRow(query, testCase.eventType, testCase.payload, "c", "r", "i", time.Now().UTC()).Scan(&kept); err != nil {
				t.Fatalf("%s: %v", testCase.name, err)
			}
			return kept
		}
		if got := row(replayableEventsPredicate("e")) == 1; got != testCase.replayable {
			t.Fatalf("%s (%s): 会话历史分页 kept=%v, want %v", testCase.name, testCase.eventType, got, testCase.replayable)
		}
		if got := row(remoteNoticeReplayPredicate("e")) == 1; got != testCase.notice {
			t.Fatalf("%s (%s): 手机端运行记录 kept=%v, want %v", testCase.name, testCase.eventType, got, testCase.notice)
		}
	}
}
