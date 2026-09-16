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
	if len(project.Conversations[0].Messages) != remoteSnapshotMessagesPerConversation {
		t.Fatalf("messages=%d want %d", len(project.Conversations[0].Messages), remoteSnapshotMessagesPerConversation)
	}
	if len(project.Conversations[0].Messages[0].Content) != remoteSnapshotMessageContentLimit {
		t.Fatalf("older content length=%d want %d", len(project.Conversations[0].Messages[0].Content), remoteSnapshotMessageContentLimit)
	}
	if len(project.Conversations[0].Messages[len(project.Conversations[0].Messages)-1].Content) != 3000 {
		t.Fatalf("newest content length=%d want 3000", len(project.Conversations[0].Messages[len(project.Conversations[0].Messages)-1].Content))
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
