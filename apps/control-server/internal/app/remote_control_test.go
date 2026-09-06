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
