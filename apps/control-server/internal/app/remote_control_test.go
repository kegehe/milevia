package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func newRemoteTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:remote-test?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`create table task_events (id text primary key, task_id text not null, task_run_id text, type text not null, payload text not null, created_at datetime not null)`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	return db
}

func TestRemoteEventOutboxUsesSameTransaction(t *testing.T) {
	db := newRemoteTestDB(t)
	defer db.Close()
	s := &Server{db: db, config: Config{RemoteCloudURL: "https://cloud.example.com", RemoteCloudToken: "token"}}
	if err := s.migrateRemoteControl(context.Background()); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.recordTaskEventTx(context.Background(), tx, "task-1", "", "task.created", map[string]string{"status": "todo"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var events, outbox int
	if err := db.QueryRow(`select count(*) from task_events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`select count(*) from remote_outbox`).Scan(&outbox); err != nil {
		t.Fatal(err)
	}
	if events != 1 || outbox != 1 {
		t.Fatalf("events=%d outbox=%d", events, outbox)
	}
	var sequence int64
	if err := db.QueryRow(`select agent_sequence from remote_outbox`).Scan(&sequence); err != nil {
		t.Fatal(err)
	}
	if sequence != 1 {
		t.Fatalf("sequence=%d", sequence)
	}
}

func TestConversationRemoteEventIsQueued(t *testing.T) {
	db := newRemoteTestDB(t)
	defer db.Close()
	s := &Server{db: db, config: Config{RemoteCloudURL: "https://cloud.example.com", RemoteCloudToken: "token"}}
	if err := s.migrateRemoteControl(context.Background()); err != nil {
		t.Fatal(err)
	}
	eventID := "conversation-event-1"
	if err := s.enqueueRemoteEvent(context.Background(), eventID, "", "run-1", "assistant.message", []byte(`{"content":"hello"}`), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var gotID, typ, runID, payload string
	if err := db.QueryRow(`select event_id,type,task_run_id,payload from remote_outbox`).Scan(&gotID, &typ, &runID, &payload); err != nil {
		t.Fatal(err)
	}
	if gotID != eventID || typ != "assistant.message" || runID != "run-1" || payload != `{"content":"hello"}` {
		t.Fatalf("unexpected remote event: id=%s type=%s run=%s payload=%s", gotID, typ, runID, payload)
	}
}

func TestConversationCreatedRemoteEventKeepsConversationID(t *testing.T) {
	payload := []byte(`{"conversationId":"conversation-1","projectId":"project-1"}`)
	if got := string(compactRemoteEventPayload("conversation.created", payload)); got != string(payload) {
		t.Fatalf("conversation.created payload=%s, want=%s", got, payload)
	}
}

func TestPersistedConversationEventAndRemoteOutboxCommitTogether(t *testing.T) {
	db := newRemoteTestDB(t)
	defer db.Close()
	if _, err := db.Exec(`create table events (id text primary key, conversation_id text not null, run_id text not null, type text not null, payload text not null, created_at datetime not null)`); err != nil {
		t.Fatal(err)
	}
	s := &Server{db: db, config: Config{RemoteCloudURL: "https://cloud.example.com", RemoteCloudToken: "token"}}
	if err := s.migrateRemoteControl(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.appendEvent("run-1", "conversation-1", "assistant.message", []byte(`{"content":"hello"}`))
	var events, outbox int
	if err := db.QueryRow(`select count(*) from events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`select count(*) from remote_outbox`).Scan(&outbox); err != nil {
		t.Fatal(err)
	}
	if events != 1 || outbox != 1 {
		t.Fatalf("events=%d outbox=%d", events, outbox)
	}
}

func TestRemoteOutboxSkippedWithoutRelayConfig(t *testing.T) {
	db := newRemoteTestDB(t)
	defer db.Close()
	s := &Server{db: db}
	if err := s.migrateRemoteControl(context.Background()); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.recordTaskEventTx(context.Background(), tx, "task-1", "", "task.created", map[string]string{"status": "todo"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var events, outbox int
	if err := db.QueryRow(`select count(*) from task_events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`select count(*) from remote_outbox`).Scan(&outbox); err != nil {
		t.Fatal(err)
	}
	if events != 1 || outbox != 0 {
		t.Fatalf("events=%d outbox=%d", events, outbox)
	}
}

func TestRemoteOutboxSkippedWithOnlyLocalAgentToken(t *testing.T) {
	db := newRemoteTestDB(t)
	defer db.Close()
	s := &Server{db: db, config: Config{RemoteAgentToken: "local-only"}}
	if err := s.migrateRemoteControl(context.Background()); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.recordTaskEventTx(context.Background(), tx, "task-1", "", "task.created", map[string]string{"status": "todo"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var outbox int
	if err := db.QueryRow(`select count(*) from remote_outbox`).Scan(&outbox); err != nil {
		t.Fatal(err)
	}
	if outbox != 0 {
		t.Fatalf("outbox=%d, want 0 when only local Agent auth is configured", outbox)
	}
}

func TestRuntimeAgentCredentialsEnablePairingAndOutbox(t *testing.T) {
	db := newRemoteTestDB(t)
	defer db.Close()
	s := &Server{db: db, config: Config{RemoteAgentToken: "local-agent-token"}}
	if err := s.migrateRemoteControl(context.Background()); err != nil {
		t.Fatal(err)
	}
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/agent/pairings" {
			t.Fatalf("path=%q", r.URL.Path)
		}
		if got := r.Header.Get("X-Milevia-Agent-Token"); got != "dynamic-agent-token" {
			t.Fatalf("agent token=%q", got)
		}
		if got := r.Header.Get("X-Milevia-Instance-ID"); got != "dynamic-instance-id" {
			t.Fatalf("instance ID=%q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"pairingId":"pairing-1","code":"123456"}`))
	}))
	defer cloud.Close()
	body := fmt.Sprintf(`{"cloudUrl":%q,"instanceId":"dynamic-instance-id","agentToken":"dynamic-agent-token"}`, cloud.URL)
	credentialRequest := httptest.NewRequest(http.MethodPost, "/api/remote/credentials", strings.NewReader(body))
	credentialRequest.Header.Set("X-Milevia-Agent-Token", "local-agent-token")
	credentialResponse := httptest.NewRecorder()
	s.remoteAgentOnly(http.HandlerFunc(s.updateRemoteCredentials)).ServeHTTP(credentialResponse, credentialRequest)
	if credentialResponse.Code != http.StatusOK {
		t.Fatalf("credential status=%d body=%s", credentialResponse.Code, credentialResponse.Body.String())
	}
	if !s.remoteRelayConfigured() {
		t.Fatal("runtime credentials did not configure the remote relay")
	}
	pairingResponse := httptest.NewRecorder()
	s.createRemotePairing(pairingResponse, httptest.NewRequest(http.MethodPost, "/api/remote/pairing", nil))
	if pairingResponse.Code != http.StatusCreated {
		t.Fatalf("pairing status=%d body=%s", pairingResponse.Code, pairingResponse.Body.String())
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.recordTaskEventTx(context.Background(), tx, "task-1", "", "task.created", map[string]string{"status": "todo"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var outbox int
	if err := db.QueryRow(`select count(*) from remote_outbox`).Scan(&outbox); err != nil {
		t.Fatal(err)
	}
	if outbox != 1 {
		t.Fatalf("outbox=%d, want 1", outbox)
	}
}

func TestRemoteOverviewScansSQLiteExpressionTimestamp(t *testing.T) {
	db := newRemoteTestDB(t)
	defer db.Close()
	s := &Server{db: db}
	if err := s.migrateRemoteControl(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`update remote_instance set status='online',last_seen_at=?,updated_at=?`, time.Now().UTC(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	s.remoteOverview(response, httptest.NewRequest(http.MethodGet, "/api/remote/overview", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"status":"online"`) {
		t.Fatalf("unexpected overview=%s", response.Body.String())
	}
}

// 页面能用桌面会话令牌打的端点是一张**显式小表**（desktopPairingPaths），其余
// `/api/remote/**` 只认 Agent 令牌。心跳端点绝不能被放进去：control-server 在
// `remoteOverview` 里记"最近听到 Agent 说话"的时刻，页面自己去打就等于自己给自己
// 发心跳 —— 电脑端那一屏的"收不到心跳"档会永远不可达（页面永远显示"运行中"）。
// 这条断言守的就是这个：白名单可以加端点，但加的绝不能是它。
func TestDesktopSessionWhitelistNeverIncludesTheAgentHeartbeat(t *testing.T) {
	if desktopPairingPaths["/api/remote/overview"] {
		t.Fatal("/api/remote/overview 是 Agent 的心跳端点，不能进 desktopPairingPaths（页面会自己刷新心跳）")
	}
	// 顺手钉住表里只有 remote 路径：这张表是"页面能碰的 relay 端点"，放进别的路由
	// 只会让它悄悄变成第二个 requireSession 豁免口。
	for path := range desktopPairingPaths {
		if !strings.HasPrefix(path, "/api/remote/") {
			t.Fatalf("desktopPairingPaths 里出现了非 /api/remote/ 的路径：%s", path)
		}
	}
}

func TestRemoteAgentStatusReportsHeartbeatOnlyAfterTheAgentSpeaks(t *testing.T) {
	db := newRemoteTestDB(t)
	defer db.Close()
	s := &Server{db: db}
	if err := s.migrateRemoteControl(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`update remote_instance set status='online',last_seen_at=?,updated_at=?`, time.Now().UTC(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	// ① 还没听到过 Agent 说话：必须给空字符串，而不是"现在"。
	// 把 0 当成 time.Unix(0,0) 会让页面显示"最近心跳 1970 年"，比不显示更误导。
	read := func() map[string]any {
		response := httptest.NewRecorder()
		s.remoteAgentStatus(response, httptest.NewRequest(http.MethodGet, "/api/remote/agent-status", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		var value map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	if beat, _ := read()["lastAgentHeartbeatAt"].(string); beat != "" {
		t.Fatalf("heartbeat=%q, want empty before the agent ever polls", beat)
	}

	// ② Agent 打一次心跳端点之后，状态里必须带出可解析且是"刚刚"的时刻。
	// ready 只看凭据在不在，Agent 进程崩了它依然是 true —— 心跳才是"还活着"的那一半。
	overview := httptest.NewRecorder()
	s.remoteOverview(overview, httptest.NewRequest(http.MethodGet, "/api/remote/overview", nil))
	if overview.Code != http.StatusOK {
		t.Fatalf("overview status=%d body=%s", overview.Code, overview.Body.String())
	}
	beat, _ := read()["lastAgentHeartbeatAt"].(string)
	parsed, err := time.Parse(time.RFC3339, beat)
	if err != nil {
		t.Fatalf("heartbeat=%q is not RFC3339: %v", beat, err)
	}
	if age := time.Since(parsed); age < 0 || age > time.Minute {
		t.Fatalf("heartbeat age=%s, want a fresh timestamp", age)
	}
}

func TestRemoteSnapshotIsBoundedToCurrentConversation(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('remote-project','Remote',?,'ssh-remote','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 2; index++ {
		id := fmt.Sprintf("remote-conversation-%d", index)
		current := index == 0
		status := "idle"
		if current {
			status = "running"
		}
		if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,claude_initialized,is_current,created_at) values (?,?,?,?,?,?,?)`, id, "remote-project", id, status, 0, current, now.Add(time.Duration(index)*time.Second)); err != nil {
			t.Fatal(err)
		}
		for messageIndex := 0; messageIndex < 25; messageIndex++ {
			if _, err := server.db.Exec(`insert into messages (id,conversation_id,run_id,role,content,parent_tool_use_id,created_at) values (?,?,?,?,?,?,?)`, fmt.Sprintf("%s-message-%d", id, messageIndex), id, "", "user", strings.Repeat("x", 3000), "", now.Add(time.Duration(messageIndex)*time.Millisecond)); err != nil {
				t.Fatal(err)
			}
		}
	}
	response := httptest.NewRecorder()
	server.remoteSnapshot(response, httptest.NewRequest(http.MethodGet, "/api/remote/snapshot", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var snapshot remoteSnapshot
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	var project *remoteSnapshotProject
	for index := range snapshot.Projects {
		if snapshot.Projects[index].ID == "remote-project" {
			project = &snapshot.Projects[index]
		}
	}
	if project == nil || len(project.Conversations) != 1 || project.Conversations[0].ID != "remote-conversation-0" {
		t.Fatalf("unexpected conversations: %+v", project)
	}
	if project.Environment != "remote-linux" || !project.Running {
		t.Fatalf("project presentation: environment=%q running=%v", project.Environment, project.Running)
	}
	// 每个会话 25 条 × 3000 字符（合计 75 KiB）远超单会话 24 KiB 预算：
	// 最新那条必须完整，其余按预算往回装，装不下的更旧消息一条都不带。
	messages := project.Conversations[0].Messages
	if len(messages) == 0 {
		t.Fatal("no messages in snapshot")
	}
	newest := messages[len(messages)-1]
	if len(newest.Content) != 3000 {
		t.Fatalf("newest content length=%d want 3000", len(newest.Content))
	}
	if strings.HasSuffix(newest.Content, remoteSnapshotMessageTruncated) {
		t.Fatal("newest message must never be truncated")
	}
	total := 0
	truncated := 0
	for _, message := range messages {
		total += len(message.Content)
		if strings.HasSuffix(message.Content, remoteSnapshotMessageTruncated) {
			truncated++
		}
	}
	if total > remoteSnapshotMessageBudget {
		t.Fatalf("messages total %d bytes, over budget %d", total, remoteSnapshotMessageBudget)
	}
	if len(messages) >= 25 {
		t.Fatalf("budget did not bound the window: %d messages", len(messages))
	}
	// 截断必须留痕：静默截断正是"手机上悄悄少了一段"的来源。
	if truncated != 1 {
		t.Fatalf("truncated messages=%d, want exactly 1", truncated)
	}
}

// 预算之内的会话必须**一条不落**地回来，且不做任何截断。这是这次改动的用户可见部分：
// 以前是"只带 20 条、除最新一条外一律砍到 2000 字符"，实测的几个活跃会话在手机上只能看到
// 27~115 条里的 20 条、字符数只剩三到五成。
func TestRemoteSnapshotCarriesWholeRecentConversation(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('whole-project','Whole',?,'local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,claude_initialized,is_current,created_at) values ('whole-conversation','whole-project','whole',?,0,1,?)`, "idle", now); err != nil {
		t.Fatal(err)
	}
	// 60 条消息、合计约 16 KiB（中文按 UTF-8 三字节算）：远超旧的 20 条上限，
	// 但稳稳落在 24 KiB 预算之内。
	const count = 60
	for index := 0; index < count; index++ {
		content := fmt.Sprintf("第 %d 条消息：%s", index, strings.Repeat("内容", 40))
		if _, err := server.db.Exec(`insert into messages (id,conversation_id,run_id,role,content,parent_tool_use_id,created_at) values (?,?,?,?,?,?,?)`,
			fmt.Sprintf("whole-message-%d", index), "whole-conversation", "", "user", content, "", now.Add(time.Duration(index)*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}
	response := httptest.NewRecorder()
	server.remoteSnapshot(response, httptest.NewRequest(http.MethodGet, "/api/remote/snapshot", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var snapshot remoteSnapshot
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	var messages []remoteSnapshotMessage
	for _, project := range snapshot.Projects {
		if project.ID == "whole-project" && len(project.Conversations) == 1 {
			messages = project.Conversations[0].Messages
		}
	}
	if len(messages) != count {
		t.Fatalf("messages=%d want all %d", len(messages), count)
	}
	for index, message := range messages {
		if strings.HasSuffix(message.Content, remoteSnapshotMessageTruncated) {
			t.Fatalf("message %d was truncated inside the budget", index)
		}
	}
	// 顺序自始至终是"旧 → 新"。
	if !strings.HasPrefix(messages[0].Content, "第 0 条消息") || !strings.HasPrefix(messages[count-1].Content, fmt.Sprintf("第 %d 条消息", count-1)) {
		t.Fatalf("unexpected order: first=%q last=%q", messages[0].Content, messages[count-1].Content)
	}
}

// 手机端「＋」面板要与电脑端左侧快捷栏同源，快照就必须把快捷方式库发下去。这条测试固定四件事：
// ① 库挂在**顶层**（一份数据被多个项目共用，复制到每个项目里会出现"同一份数据两个版本"）；
// ② 绑定关系随 projectIds 一起下发（手机端据此按项目过滤）；
// ③ 排序与 listShortcuts 一致（置顶 → sort_order → name），否则两端顺序不同；
// ④ 超长模板按 remoteSnapshotShortcutTemplateLimit 截断（一条超长提示词会撑大每一个快照）。
func TestRemoteSnapshotCarriesShortcutLibrary(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('shortcut-project','P',?,'local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatal(err)
	}
	insertShortcut := func(id, name, kind, template, scope string, pinned, enabled, sortOrder int) {
		if _, err := server.db.Exec(`insert into shortcuts (id,name,description,kind,template,scope,default_action,group_name,pinned,enabled,sort_order,created_at,updated_at) values (?,?,'',?,?,?,  'fill','',?,?,?,?,?)`,
			id, name, kind, template, scope, pinned, enabled, sortOrder, now, now); err != nil {
			t.Fatal(err)
		}
	}
	insertShortcut("s-later", "后添加的提示词", "prompt", "模板", "local", 0, 1, 20)
	insertShortcut("s-pinned", "置顶提示词", "prompt", "模板", "local", 1, 1, 30)
	insertShortcut("s-project", "项目提示词", "command_request", "npm test", "project", 0, 1, 10)
	insertShortcut("s-long", "超长提示词", "prompt", strings.Repeat("x", remoteSnapshotShortcutTemplateLimit+500), "local", 0, 1, 40)
	if _, err := server.db.Exec(`insert into shortcut_projects (shortcut_id,project_id) values ('s-project','shortcut-project')`); err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	server.remoteSnapshot(response, httptest.NewRequest(http.MethodGet, "/api/remote/snapshot", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var snapshot remoteSnapshot
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Shortcuts) != 4 {
		t.Fatalf("shortcuts=%d want 4 (%+v)", len(snapshot.Shortcuts), snapshot.Shortcuts)
	}
	// 顺序必须与桌面端 listShortcuts 的 `order by s.sort_order,s.name` 完全一致 —— 不提升 pinned。
	// 这条用例就是为此存在的：曾经这里多写了一个 `pinned desc`（并配上一句"对齐桌面端置顶语义"
	// 的错注释），当时只有"置顶恒在最前"这个错误期望能测出来。
	order := make([]string, 0, len(snapshot.Shortcuts))
	byID := map[string]remoteSnapshotShortcut{}
	for _, shortcut := range snapshot.Shortcuts {
		order = append(order, shortcut.ID)
		byID[shortcut.ID] = shortcut
	}
	// s-project(10) < s-later(20) < s-pinned(30) < s-long(40)；s-pinned 虽然 pinned=1 也不能提前。
	if got := strings.Join(order, ","); got != "s-project,s-later,s-pinned,s-long" {
		t.Fatalf("order=%s want s-project,s-later,s-pinned,s-long（pinned 不得提升）", got)
	}
	// 绑定关系：只有 s-project 绑了项目，其余发空数组（不是 null —— 手机端会直接 .includes）。
	if got := strings.Join(byID["s-project"].ProjectIDs, ","); got != "shortcut-project" {
		t.Fatalf("s-project projectIds=%q", got)
	}
	for _, id := range []string{"s-pinned", "s-later", "s-long"} {
		if byID[id].ProjectIDs == nil || len(byID[id].ProjectIDs) != 0 {
			t.Fatalf("%s projectIds=%v want empty slice", id, byID[id].ProjectIDs)
		}
	}
	if len(byID["s-long"].Template) != remoteSnapshotShortcutTemplateLimit {
		t.Fatalf("long template length=%d want %d", len(byID["s-long"].Template), remoteSnapshotShortcutTemplateLimit)
	}
	// 每个项目都要带上技能数组（哪怕是空的）：手机端读 project.skills 渲染技能组，
	// 字段缺失会让"没有技能"与"这个字段还不存在"两种状态在客户端无法区分。
	for index := range snapshot.Projects {
		if snapshot.Projects[index].Skills == nil {
			t.Fatalf("project %s skills is nil", snapshot.Projects[index].ID)
		}
	}
}

func TestRemoteCommandIdempotencyConflict(t *testing.T) {
	db := newRemoteTestDB(t)
	defer db.Close()
	s := &Server{db: db}
	if err := s.migrateRemoteControl(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := httptest.NewRecorder()
	s.enqueueRemoteCommand(first, httptest.NewRequest(http.MethodPost, "/api/remote/commands", strings.NewReader(`{"type":"task.dispatch","taskId":"t1","idempotencyKey":"same","payload":{"x":1}}`)))
	if first.Code != http.StatusAccepted {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	second := httptest.NewRecorder()
	s.enqueueRemoteCommand(second, httptest.NewRequest(http.MethodPost, "/api/remote/commands", strings.NewReader(`{"type":"task.dispatch","taskId":"t1","idempotencyKey":"same","payload":{"x":2}}`)))
	if second.Code != http.StatusConflict {
		t.Fatalf("second status=%d body=%s", second.Code, second.Body.String())
	}
}

func TestRemoteCommandEnqueueWakesWorkerImmediately(t *testing.T) {
	db := newRemoteTestDB(t)
	defer db.Close()
	s := &Server{db: db, remoteCommandWake: make(chan struct{}, 1)}
	if err := s.migrateRemoteControl(context.Background()); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	s.enqueueRemoteCommand(response, httptest.NewRequest(http.MethodPost, "/api/remote/commands", strings.NewReader(`{"type":"task.dispatch","taskId":"t1","idempotencyKey":"wake-now","payload":{}}`)))
	if response.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	select {
	case <-s.remoteCommandWake:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("queued remote command did not wake worker")
	}
}

func TestRemoteCommandWorkerExpiresQueuedCommand(t *testing.T) {
	db := newRemoteTestDB(t)
	defer db.Close()
	s := &Server{db: db, runtimeCtx: context.Background()}
	if err := s.migrateRemoteControl(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`insert into processed_remote_commands(command_id,idempotency_key,type,status,result,received_at,updated_at,expires_at,request_payload,request_hash) values(?,?,?,?,?,?,?,?,?,?)`, "cmd-expired", "idem-expired", "task.dispatch", "queued", "{}", time.Now().UTC(), time.Now().UTC(), time.Now().UTC().Add(-time.Minute), `{"taskId":"task-1"}`, "hash"); err != nil {
		t.Fatal(err)
	}
	s.processOneRemoteCommand(context.Background())
	var status string
	if err := db.QueryRow(`select status from processed_remote_commands where command_id='cmd-expired'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "expired" {
		t.Fatalf("status=%s", status)
	}
}

func TestRemoteCommandWorkerMarksExecutionFailure(t *testing.T) {
	db := newRemoteTestDB(t)
	defer db.Close()
	s := &Server{db: db, runtimeCtx: context.Background()}
	if err := s.migrateRemoteControl(context.Background()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	request := `{"commandId":"cmd-fail","type":"task.reopen","taskId":"missing","idempotencyKey":"idem-fail","payload":{}}`
	if _, err := db.Exec(`insert into processed_remote_commands(command_id,idempotency_key,type,status,result,received_at,updated_at,expires_at,request_payload,request_hash) values(?,?,?,?,?,?,?,?,?,?)`, "cmd-fail", "idem-fail", "task.reopen", "queued", "{}", now, now, now.Add(time.Minute), request, "hash"); err != nil {
		t.Fatal(err)
	}
	s.processOneRemoteCommand(context.Background())
	var status, result string
	if err := db.QueryRow(`select status,result from processed_remote_commands where command_id='cmd-fail'`).Scan(&status, &result); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || !strings.Contains(result, "error") {
		t.Fatalf("status=%s result=%s", status, result)
	}
}

// 一条超长消息不能把整个窗口挤成一条。
//
// 预算只有 24 KiB，而实测库里最长的一条消息有 47 KB —— 它一旦是最新那条，手机重进页面
// 就只剩这一条，用户看到的现象与"历史记录没了"一模一样。所以最新一条最多吃掉
// 「预算 - reserve」，剩下的必须留给更早的消息。
func TestRemoteSnapshotHugeNewestMessageLeavesOlderWindow(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('huge-project','Huge',?,'local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,claude_initialized,is_current,created_at) values ('huge-conversation','huge-project','huge','idle',0,1,?)`, now); err != nil {
		t.Fatal(err)
	}
	const older = 30
	for index := 0; index < older; index++ {
		if _, err := server.db.Exec(`insert into messages (id,conversation_id,run_id,role,content,parent_tool_use_id,created_at) values (?,?,?,?,?,?,?)`,
			fmt.Sprintf("huge-message-%d", index), "huge-conversation", "", "user", fmt.Sprintf("第 %d 条：%s", index, strings.Repeat("内容", 100)), "", now.Add(time.Duration(index)*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}
	// 最新一条 40 KiB（超过整个预算）。
	if _, err := server.db.Exec(`insert into messages (id,conversation_id,run_id,role,content,parent_tool_use_id,created_at) values (?,?,?,?,?,?,?)`,
		"huge-message-newest", "huge-conversation", "", "assistant", strings.Repeat("长", 40*1024/3), "", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	server.remoteSnapshot(response, httptest.NewRequest(http.MethodGet, "/api/remote/snapshot", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var snapshot remoteSnapshot
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	var messages []remoteSnapshotMessage
	for _, project := range snapshot.Projects {
		if project.ID == "huge-project" && len(project.Conversations) == 1 {
			messages = project.Conversations[0].Messages
		}
	}
	if len(messages) < 2 {
		t.Fatalf("超长的最新一条把窗口挤成了 %d 条", len(messages))
	}
	// 这里是关键差异：不加 reserve 时这个会话只会带回来**一条**（最新那条）。
	// 更早的消息一共 30 条、合计约 18 KiB，reserve 只有 6 KiB，所以装不满 30 条 ——
	// 断言的是"窗口没有被挤空"，不是"更早的全都带回来"（那是预算决定的，不是这条规则）。
	if len(messages) < 5 {
		t.Fatalf("reserve 没起作用：只带回来 %d 条", len(messages))
	}
	newest := messages[len(messages)-1]
	if !strings.HasSuffix(newest.Content, remoteSnapshotMessageTruncated) {
		t.Fatalf("超预算的最新一条既没被裁、也没留标记：%d 字节", len(newest.Content))
	}
	if len(newest.Content) > remoteSnapshotMessageBudget-remoteSnapshotMessageReserve+len(remoteSnapshotMessageTruncated) {
		t.Fatalf("最新一条吃掉了 %d 字节，超过它该有的配额", len(newest.Content))
	}
	// 更早的消息必须还在，且整段不超预算。
	total := 0
	for _, message := range messages {
		total += len(message.Content)
	}
	if total > remoteSnapshotMessageBudget {
		t.Fatalf("messages total %d bytes, over budget %d", total, remoteSnapshotMessageBudget)
	}
	if !strings.HasPrefix(messages[0].Content, "第 ") {
		t.Fatalf("窗口第一条不是更早的真实消息，而是 %q", messages[0].Content)
	}
}

// 配对卡片要能说出"现在绑在这台电脑上的是哪台手机"（换绑时得告诉用户会断开谁），
// 所以这个 relay 必须真的把云端的 bindings 透上来，而不是自己编一个。
func TestRemoteBindingsRelaysCloudBindings(t *testing.T) {
	db := newRemoteTestDB(t)
	defer db.Close()
	s := &Server{db: db, config: Config{RemoteAgentToken: "local-agent-token"}}
	if err := s.migrateRemoteControl(context.Background()); err != nil {
		t.Fatal(err)
	}
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/agent/bindings" {
			t.Fatalf("path=%q", r.URL.Path)
		}
		if got := r.Header.Get("X-Milevia-Agent-Token"); got != "dynamic-agent-token" {
			t.Fatalf("agent token=%q", got)
		}
		if got := r.Header.Get("X-Milevia-Instance-ID"); got != "dynamic-instance-id" {
			t.Fatalf("instance ID=%q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"instanceId":"dynamic-instance-id","bindings":[{"deviceName":"Xiaomi 14","activatedAt":"2026-08-12T06:00:00Z","lastUsedAt":"2026-09-18T02:10:00Z","platform":"android"},{"deviceName":"iPhone 15","activatedAt":"2026-08-10T06:00:00Z","lastUsedAt":null,"platform":""}]}`))
	}))
	defer cloud.Close()
	body := fmt.Sprintf(`{"cloudUrl":%q,"instanceId":"dynamic-instance-id","agentToken":"dynamic-agent-token"}`, cloud.URL)
	credentialRequest := httptest.NewRequest(http.MethodPost, "/api/remote/credentials", strings.NewReader(body))
	credentialRequest.Header.Set("X-Milevia-Agent-Token", "local-agent-token")
	if recorder := httptest.NewRecorder(); true {
		s.remoteAgentOnly(http.HandlerFunc(s.updateRemoteCredentials)).ServeHTTP(recorder, credentialRequest)
		if recorder.Code != http.StatusOK {
			t.Fatalf("credential status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	}
	recorder := httptest.NewRecorder()
	s.remoteBindings(recorder, httptest.NewRequest(http.MethodGet, "/api/remote/bindings", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("bindings status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "Xiaomi 14") {
		t.Fatalf("bindings body did not carry the phone name: %s", recorder.Body.String())
	}
	// ⚠️ 这个 relay 是**整块透传**（decode 到 any 再 encode），不是手抄字段的表。
	// 所以这条断言守的不是"云端会回什么"，而是"**将来别把它改成一张手抄的表**"：
	// 一旦有人在中间定义结构体却漏掉新字段，`lastUsedAt` 会静默消失 ——
	// 而桌面端对"缺键"的解释是「本机云端版本较旧」，于是它会把一个前端 bug
	// 报成"你的云端太老"，两边的排查方向全错。
	for _, want := range []string{`"lastUsedAt":"2026-09-18T02:10:00Z"`, `"platform":"android"`, `"lastUsedAt":null`} {
		if !strings.Contains(recorder.Body.String(), want) {
			t.Fatalf("relay 丢字段：正文里没有 %s —— 透传被改成手抄字段表了？body=%s", want, recorder.Body.String())
		}
	}
}

// 远程服务还没注册时，"当前绑定的手机"是"空"而不是"错误"：
// 页面照着空列表渲染，用户点到生成二维码那一步才会看到真正的引导。
func TestRemoteBindingsWithoutAgentIsEmptyNotAnError(t *testing.T) {
	s := &Server{config: Config{}}
	recorder := httptest.NewRecorder()
	s.remoteBindings(recorder, httptest.NewRequest(http.MethodGet, "/api/remote/bindings", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("bindings status=%d, want 200 (empty state, not an error)", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), `"ready":false`) {
		t.Fatalf("bindings body should mark the relay unready: %s", recorder.Body.String())
	}
}

// 「解除绑定」必须真的打到云端的 agent 解绑端点：电脑端页面只有会话令牌，
// 拿不到手机令牌，所以"我是这台机器"就是它能给出的全部证明。
func TestRemoteRevokeBindingsHitsCloudAgentEndpoint(t *testing.T) {
	db := newRemoteTestDB(t)
	defer db.Close()
	s := &Server{db: db, config: Config{RemoteAgentToken: "local-agent-token"}}
	if err := s.migrateRemoteControl(context.Background()); err != nil {
		t.Fatal(err)
	}
	hit := false
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/agent/bindings/revoke" || r.Method != http.MethodPost {
			t.Fatalf("unexpected call %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("X-Milevia-Instance-ID"); got != "dynamic-instance-id" {
			t.Fatalf("instance ID=%q", got)
		}
		hit = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"revoked","instanceId":"dynamic-instance-id","revoked":1}`))
	}))
	defer cloud.Close()
	body := fmt.Sprintf(`{"cloudUrl":%q,"instanceId":"dynamic-instance-id","agentToken":"dynamic-agent-token"}`, cloud.URL)
	credentialRequest := httptest.NewRequest(http.MethodPost, "/api/remote/credentials", strings.NewReader(body))
	credentialRequest.Header.Set("X-Milevia-Agent-Token", "local-agent-token")
	if recorder := httptest.NewRecorder(); true {
		s.remoteAgentOnly(http.HandlerFunc(s.updateRemoteCredentials)).ServeHTTP(recorder, credentialRequest)
		if recorder.Code != http.StatusOK {
			t.Fatalf("credential status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	}
	recorder := httptest.NewRecorder()
	s.remoteRevokeBindings(recorder, httptest.NewRequest(http.MethodPost, "/api/remote/bindings/revoke", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("revoke status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if !hit {
		t.Fatal("revoke never reached the cloud")
	}
}
