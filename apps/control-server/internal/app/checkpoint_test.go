package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWorkspaceCheckpointRestore(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot, metadata, err := createWorkspaceCheckpoint(root, "test-conversation", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(snapshot)
	if metadata["fileCount"] != 1 {
		t.Fatalf("file count = %#v, want 1", metadata["fileCount"])
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("after\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := restoreCheckpointSnapshot(snapshot, root); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(root, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "before\n" {
		t.Fatalf("restored content = %q", content)
	}
}

func TestRestoreCheckpointRejectsMissingSnapshot(t *testing.T) {
	if err := restoreCheckpointSnapshot(filepath.Join(t.TempDir(), "missing"), t.TempDir()); err == nil {
		t.Fatal("missing snapshot was accepted")
	}
}

func seedCheckpointConversation(t *testing.T, server *Server) string {
	t.Helper()
	root := t.TempDir()
	now := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('checkpoint-project','checkpoint-project',?,'wsl-local','main',1,?)`, root, now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,agent_id,agent_session_id,status,is_current,created_at,last_activity_at) values ('checkpoint-conversation','checkpoint-project','checkpoint-session','claude-code','checkpoint-session','idle',1,?,?)`, now, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	return root
}

func TestListConversationCheckpoints(t *testing.T) {
	server := newTestServer(t)
	seedCheckpointConversation(t, server)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into conversation_checkpoints (id,conversation_id,agent_id,prompt,status,metadata,created_at) values ('checkpoint-1','checkpoint-conversation','claude-code','first','active','{}',?)`, now); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/conversations/checkpoint-conversation/checkpoints", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Checkpoints []conversationCheckpoint `json:"checkpoints"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Checkpoints) != 1 || body.Checkpoints[0].ID != "checkpoint-1" {
		t.Fatalf("unexpected checkpoints: %#v", body.Checkpoints)
	}
	if body.Checkpoints[0].SnapshotPath != "" {
		t.Fatal("snapshot path must not be exposed by the list API")
	}
}

func TestListConversationCheckpointsReturnsNotFound(t *testing.T) {
	server := newTestServer(t)
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/conversations/missing/checkpoints", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing conversation status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestRewindRejectsInvalidMode(t *testing.T) {
	server := newTestServer(t)
	seedCheckpointConversation(t, server)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/conversations/checkpoint-conversation/rewind", strings.NewReader(`{"checkpointId":"missing","mode":"invalid"}`))
	server.routes().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid mode status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestConversationCheckpointCutoffUsesLatestActiveCheckpoint(t *testing.T) {
	server := newTestServer(t)
	seedCheckpointConversation(t, server)
	first := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Millisecond)
	second := first.Add(time.Minute)
	if _, err := server.db.Exec(`insert into conversation_checkpoints (id,conversation_id,status,metadata,created_at) values ('checkpoint-old','checkpoint-conversation','rewound','{}',?),('checkpoint-new','checkpoint-conversation','active','{}',?)`, first, second); err != nil {
		t.Fatal(err)
	}
	cutoff, err := server.conversationCheckpointCutoff(t.Context(), "checkpoint-conversation")
	if err != nil {
		t.Fatal(err)
	}
	if cutoff == nil || !cutoff.Equal(second) {
		t.Fatalf("cutoff=%v want %v", cutoff, second)
	}
}

func TestGetConversationAppliesCheckpointCutoffToEvents(t *testing.T) {
	server := newTestServer(t)
	seedCheckpointConversation(t, server)
	base := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Millisecond)
	checkpointAt := base.Add(time.Minute)
	if _, err := server.db.Exec(`insert into runs (id,conversation_id,status,created_at) values ('run-before','checkpoint-conversation','completed',?),('run-after','checkpoint-conversation','completed',?)`, base, checkpointAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into events (id,conversation_id,run_id,type,payload,created_at) values ('event-before','checkpoint-conversation','run-before','assistant.message','{}',?),('event-after','checkpoint-conversation','run-after','assistant.message','{}',?)`, base, checkpointAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into conversation_checkpoints (id,conversation_id,status,metadata,created_at) values ('checkpoint-cutoff','checkpoint-conversation','active','{}',?)`, checkpointAt); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/conversations/checkpoint-conversation?limit=50", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("conversation status=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Events []Event `json:"events"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Events) != 1 || body.Events[0].ID != "event-before" {
		t.Fatalf("events after cutoff leaked: %#v", body.Events)
	}
}

func TestCreateConversationCheckpointParentsToActiveNode(t *testing.T) {
	server := newTestServer(t)
	root := seedCheckpointConversation(t, server)
	base := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Millisecond)
	if _, err := server.db.Exec(`insert into conversation_checkpoints (id,conversation_id,status,metadata,created_at) values ('checkpoint-active','checkpoint-conversation','active','{}',?),('checkpoint-rewound','checkpoint-conversation','rewound','{}',?)`, base, base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into runs (id,conversation_id,status,created_at) values ('run-new','checkpoint-conversation','completed',?)`, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into messages (id,conversation_id,run_id,role,content,created_at) values ('message-new','checkpoint-conversation','run-new','user','next',?)`, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	server.config.DataDir = t.TempDir()
	if err := server.createConversationCheckpoint(t.Context(), "checkpoint-conversation", "run-new", "next"); err != nil {
		t.Fatal(err)
	}
	var parentID string
	if err := server.db.QueryRow(`select parent_id from conversation_checkpoints where conversation_id=? order by created_at desc limit 1`, "checkpoint-conversation").Scan(&parentID); err != nil {
		t.Fatal(err)
	}
	if parentID != "checkpoint-active" {
		t.Fatalf("new checkpoint parent=%q want active checkpoint", parentID)
	}
	_ = root
}

func TestForkConversationAtCheckpointDoesNotReuseRunIDs(t *testing.T) {
	server := newTestServer(t)
	root := seedCheckpointConversation(t, server)
	if _, err := server.db.Exec(`insert into messages (id,conversation_id,run_id,role,content,created_at) values ('message-1','checkpoint-conversation','old-run','user','keep',?)`, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	snapshot, _, err := createWorkspaceCheckpoint(root, "checkpoint-conversation", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(snapshot)
	checkpointAt := time.Now().UTC().Add(time.Second)
	if _, err := server.db.Exec(`insert into conversation_checkpoints (id,conversation_id,agent_id,native_session_id,prompt,snapshot_path,status,metadata,created_at) values ('checkpoint-1','checkpoint-conversation','claude-code','checkpoint-session','first',?,'active','{}',?)`, snapshot, checkpointAt); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/conversations/checkpoint-conversation/rewind", strings.NewReader(`{"checkpointId":"checkpoint-1","mode":"conversation","fork":true}`))
	server.routes().ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("fork status=%d body=%s", response.Code, response.Body.String())
	}
	var child Conversation
	var envelope struct {
		Conversation Conversation `json:"conversation"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	child = envelope.Conversation
	if child.ID == "" || child.ID == "checkpoint-conversation" {
		t.Fatalf("invalid child conversation: %#v", child)
	}
	var runID string
	if err := server.db.QueryRow(`select run_id from messages where conversation_id=?`, child.ID).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	if runID != "" {
		t.Fatalf("forked message reused run id %q", runID)
	}
}
