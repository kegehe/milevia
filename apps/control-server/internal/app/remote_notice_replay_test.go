package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// seedEvent 往会话里插一条任意 payload 的事件。
func seedEvent(t *testing.T, server *Server, conversationID, runID, eventType, payload string, at time.Time) {
	t.Helper()
	if _, err := server.db.Exec(`insert into events (id,conversation_id,run_id,type,payload,created_at) values (?,?,?,?,?,?)`,
		conversationID+"-"+at.Format("150405.000000000"), conversationID, runID, eventType, payload, at); err != nil {
		t.Fatalf("insert %s event: %v", eventType, err)
	}
}

// noticeIDs 取一次快照运行记录，返回 id 集合（id 是插入时按时刻生成的）。
func noticeIDs(t *testing.T, server *Server, conversationID string) map[string]bool {
	t.Helper()
	items, err := server.remoteConversationNotices(context.Background(), conversationID)
	if err != nil {
		t.Fatalf("remoteConversationNotices: %v", err)
	}
	ids := make(map[string]bool, len(items))
	for _, item := range items {
		ids[item.ID] = true
	}
	return ids
}

// 手机端"运行记录"是定长窗口，窗口里每混进一条渲染不出来的心跳，用户就少看到一张真卡片。
// CLI 在等模型响应时每几秒发一条 {"status":"requesting","subtype":"status"}，它把 24 条
// 配额整个占满，于是一张"后台任务启动"卡片只要再收到几条心跳就掉出窗口 ——
// 手机上看得到，退出再进来（重新拉快照）就没了。这正是真机上反馈的现象。
func TestRemoteConversationNoticesSurviveStatusHeartbeats(t *testing.T) {
	server := newTestServer(t)
	const conversationID = "conversation-notices"
	runID := seedConversation(t, server, "project-notices", conversationID)
	base := time.Now().UTC().Add(-time.Hour)

	taskStartedAt := base
	seedEvent(t, server, conversationID, runID, "system",
		`{"type":"system","subtype":"task_started","task_id":"t1","description":"跑一遍全量回归"}`, taskStartedAt)
	// 任务启动之后是一长串心跳：数量刻意远超窗口大小。
	for i := 1; i <= 40; i++ {
		seedEvent(t, server, conversationID, runID, "system",
			`{"type":"system","subtype":"status","status":"requesting"}`, base.Add(time.Duration(i)*time.Second))
	}

	ids := noticeIDs(t, server, conversationID)
	key := conversationID + "-" + taskStartedAt.Format("150405.000000000")
	if !ids[key] {
		t.Fatalf("后台任务启动卡片被心跳挤出了运行记录窗口：窗口里 %d 条，期望含 %s", len(ids), key)
	}
	if len(ids) != 1 {
		t.Fatalf("运行记录 = %d 条，期望 1 条（40 条心跳都应被排除）", len(ids))
	}
}

// 会渲染的 system 子类型一条都不能少：这份清单对应 apps/web/src/lib/timeline.ts 的
// systemItemFromEvent。多排一条 = 手机上凭空少一张卡片。
func TestRemoteConversationNoticesKeepRenderableSystemEvents(t *testing.T) {
	server := newTestServer(t)
	const conversationID = "conversation-renderable"
	runID := seedConversation(t, server, "project-renderable", conversationID)
	base := time.Now().UTC().Add(-time.Hour)

	renderable := map[string]string{
		"compacting":             `{"type":"system","subtype":"status","status":"compacting"}`,
		"compact-result":         `{"type":"system","subtype":"status","compact_result":"success"}`,
		"compact-boundary":       `{"type":"system","subtype":"compact_boundary","compact_metadata":{"pre_tokens":100,"post_tokens":10}}`,
		"api-retry":              `{"type":"system","subtype":"api_retry","attempt":2,"max_retries":5,"error":"overloaded"}`,
		"task-started":           `{"type":"system","subtype":"task_started","task_id":"t1","description":"x"}`,
		"task-notification":      `{"type":"system","subtype":"task_notification","task_id":"t1","status":"completed","summary":"y"}`,
		"backgrounded":           `{"type":"system","subtype":"task_updated","task_id":"t1","patch":{"is_backgrounded":true}}`,
		"background-tasks":       `{"type":"system","subtype":"background_tasks_changed","tasks":[]}`,
		"run-failed":             `{"error":"boom"}`,
		"approval-pending":       `{"approvalId":"a1","toolName":"Bash","toolInput":{"command":"ls"}}`,
		"approval-deny":          `{"approvalId":"a1","toolName":"Bash","toolInput":{"command":"ls"}}`,
		"stream-error":           `{"error":"pipe closed"}`,
		"unknown-future-subtype": `{"type":"system","subtype":"某个将来才有的子类型","payload":"x"}`,
	}
	types := map[string]string{
		"run-failed":       "run.failed",
		"approval-pending": "approval.pending",
		"approval-deny":    "approval.deny",
		"stream-error":     "stream.error",
	}
	at := base
	want := make(map[string]string, len(renderable))
	for name, payload := range renderable {
		typ := types[name]
		if typ == "" {
			typ = "system"
		}
		seedEvent(t, server, conversationID, runID, typ, payload, at)
		want[conversationID+"-"+at.Format("150405.000000000")] = name
		at = at.Add(time.Second)
	}

	got := noticeIDs(t, server, conversationID)
	for id, name := range want {
		if !got[id] {
			t.Fatalf("可渲染的事件被漏掉了：%s（实际取回 %d 条）", name, len(got))
		}
	}
}

// 渲染不出来的子类型一条都不该占窗口（判定与 systemItemFromEvent 的 return null 一一对应）。
func TestRemoteConversationNoticesDropUnrenderableSystemEvents(t *testing.T) {
	server := newTestServer(t)
	const conversationID = "conversation-noise"
	runID := seedConversation(t, server, "project-noise", conversationID)
	base := time.Now().UTC().Add(-time.Hour)

	noise := []string{
		`{"type":"system","subtype":"status","status":"requesting"}`,
		// CLI 与 json.Marshal 的空白写法都要认出来。
		`{"type": "system", "subtype": "status", "status": "requesting"}`,
		// 空的 compact_result 与"没有"等价：客户端按没有处理。
		`{"type":"system","subtype":"status","compact_result":""}`,
		`{"type":"system","subtype":"init","tools":["Bash"]}`,
		`{"type":"system","subtype":"task_progress","task_id":"t1"}`,
		// task_updated 只有"任务转入后台"那一种 patch 会渲染。
		`{"type":"system","subtype":"task_updated","task_id":"t1","patch":{"status":"running"}}`,
		// 历史遗留的 thinking_tokens 遥测。
		`{"type":"system","subtype":"thinking_tokens","tokens":123}`,
	}
	at := base
	for _, payload := range noise {
		seedEvent(t, server, conversationID, runID, "system", payload, at)
		at = at.Add(time.Second)
	}

	if ids := noticeIDs(t, server, conversationID); len(ids) != 0 {
		t.Fatalf("运行记录里混进了 %d 条渲染不出来的事件，期望 0 条", len(ids))
	}
}

// 超出中继预算的 payload 以前整条变成 {}，于是手机端连卡片都渲染不出来——明明占着窗口
// 的一个位置。真机上撞到的就是"后台任务"：task_notification 的 summary 是后台代理交回来
// 的整份报告（实测 5384 字节），同一张卡片实时看得到、退出再进来就没了。
func TestRemoteConversationNoticesShrinkOversizedPayload(t *testing.T) {
	server := newTestServer(t)
	const conversationID = "conversation-oversized"
	runID := seedConversation(t, server, "project-oversized", conversationID)
	base := time.Now().UTC().Add(-time.Hour)

	summary := strings.Repeat("后台代理交回来的报告正文。", 400) // 远超 4096 字节
	seedEvent(t, server, conversationID, runID, "system",
		`{"type":"system","subtype":"task_notification","task_id":"t1","status":"completed","summary":`+mustJSONString(summary)+`}`,
		base)

	items, err := server.remoteConversationNotices(context.Background(), conversationID)
	if err != nil {
		t.Fatalf("remoteConversationNotices: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("运行记录 = %d 条，期望 1 条", len(items))
	}
	payload := string(items[0].Payload)
	if len(payload) > remoteSnapshotNoticePayloadLimit {
		t.Fatalf("payload %d 字节，超过预算 %d", len(payload), remoteSnapshotNoticePayloadLimit)
	}
	var fields struct {
		Subtype string `json:"subtype"`
		Status  string `json:"status"`
		TaskID  string `json:"task_id"`
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(items[0].Payload, &fields); err != nil {
		t.Fatalf("payload 不是合法 JSON：%v（%s）", err, payload)
	}
	// 结构字段一个都不能丢：手机的渲染完全由 subtype 驱动。
	if fields.Subtype != "task_notification" || fields.Status != "completed" || fields.TaskID != "t1" {
		t.Fatalf("结构字段被裁掉了：%s", payload)
	}
	if fields.Summary == "" {
		t.Fatal("summary 被整条清空了，卡片会只剩标题")
	}
	if len(fields.Summary) >= len(summary) {
		t.Fatalf("summary 没有被截短：%d", len(fields.Summary))
	}
}

func TestShrinkNoticePayload(t *testing.T) {
	small := `{"type":"system","subtype":"task_started","task_id":"t1"}`
	if got := shrinkNoticePayload(small); got != small {
		t.Fatalf("小 payload 被动过：%s", got)
	}
	// 不是 JSON 对象（手机端只认对象）与裁不下去一样退 {}：**返回值一定不超预算**，
	// 否则预算就形同虚设。对手机来说"退 {}"与"原样保留"是同一个结果（都会丢弃）。
	notJSON := strings.Repeat("x", remoteSnapshotNoticePayloadLimit+1)
	if got := shrinkNoticePayload(notJSON); got != `{}` {
		t.Fatalf("非 JSON payload 没有被封顶：%s", got)
	}
	// 嵌套结构不能截：task_updated 的 patch.is_backgrounded 决定这张卡片渲染不渲染。
	nested := `{"type":"system","subtype":"task_updated","patch":{"is_backgrounded":true},"note":` +
		mustJSONString(strings.Repeat("说明", 3000)) + `}`
	// 每条分支都要守住预算：这是这个函数存在的唯一理由。
	for _, candidate := range []string{
		small, notJSON, nested,
		`[{"a":1}]`,
		`{"type":"system","subtype":"task_notification","summary":` + mustJSONString(strings.Repeat("x", 100000)) + `}`,
	} {
		if got := shrinkNoticePayload(candidate); len(got) > remoteSnapshotNoticePayloadLimit {
			t.Fatalf("裁完仍超预算：%d 字节（%s…）", len(got), got[:40])
		}
	}
	got := shrinkNoticePayload(nested)
	if len(got) > remoteSnapshotNoticePayloadLimit {
		t.Fatalf("裁完仍超预算：%d", len(got))
	}
	var parsed struct {
		Patch map[string]any `json:"patch"`
	}
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("裁完不是合法 JSON：%v", err)
	}
	if parsed.Patch["is_backgrounded"] != true {
		t.Fatalf("嵌套判定字段被裁掉了：%s", got)
	}
}

// mustJSONString 把一段文本编码成 JSON 字符串字面量。
func mustJSONString(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}
