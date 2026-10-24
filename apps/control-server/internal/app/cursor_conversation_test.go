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

// TestCursorConversationOnlyOnLocalRunner 钉住 Cursor 的本机会话边界：
// 建会话时就拒绝远端项目（否则用户会先得到一个必然跑不起来的会话），
// 同时旧会话或直接请求在发送消息时再守一次。
func TestCursorConversationOnlyOnLocalRunner(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	for id, runner := range map[string]string{
		"local-project":  server.localRunnerID(),
		"remote-project": "ssh-remote-1",
	} {
		if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values (?,?,?,?,'main',1,?)`,
			id, id, t.TempDir(), runner, now); err != nil {
			t.Fatalf("insert project %s: %v", id, err)
		}
	}
	handler := server.routes()
	create := func(projectID string) *httptest.ResponseRecorder {
		body := bytes.NewBufferString(`{"agentId":"cursor","permissionMode":"workspace_write"}`)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/conversations", body))
		return recorder
	}

	if response := create("remote-project"); response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "仅支持本机") {
		t.Fatalf("远端项目应被拒绝，实际 %d body=%s", response.Code, response.Body.String())
	}
	response := create("local-project")
	if response.Code != http.StatusCreated {
		t.Fatalf("本机项目应当可以创建 Cursor 会话，实际 %d body=%s", response.Code, response.Body.String())
	}
	var conversation Conversation
	if err := json.NewDecoder(response.Body).Decode(&conversation); err != nil {
		t.Fatalf("decode conversation: %v", err)
	}
	if conversation.AgentID != "cursor" || conversation.PermissionMode != "workspace_write" {
		t.Fatalf("unexpected conversation: %#v", conversation)
	}

	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,agent_id,agent_runtime_id,status,claude_initialized,is_current,created_at) values ('remote-cursor','remote-project','','cursor','ssh-remote-1','idle',0,1,?)`, now); err != nil {
		t.Fatalf("insert remote cursor conversation: %v", err)
	}
	message := httptest.NewRecorder()
	handler.ServeHTTP(message, httptest.NewRequest(http.MethodPost, "/api/conversations/remote-cursor/messages", strings.NewReader(`{"content":"hello"}`)))
	if message.Code != http.StatusServiceUnavailable || !strings.Contains(message.Body.String(), "仅支持本机") {
		t.Fatalf("远端 Cursor 会话发送消息应被拒绝，实际 %d body=%s", message.Code, message.Body.String())
	}
}
