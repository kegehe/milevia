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

// TestCreateCodeBuddyConversationOnlyOnLocalRunner 钉住与 Qoder 同一条边界：
//
// CodeBuddy 的会话面只在本机（可执行文件、工作目录与 MCP 配置都落在控制服务这台机器）。
// 如果只在发送消息时才拒绝，用户会先得到一个必然跑不起来的会话。本机项目必须仍可创建。
func TestCreateCodeBuddyConversationOnlyOnLocalRunner(t *testing.T) {
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
		body := bytes.NewBufferString(`{"agentId":"codebuddy","permissionMode":"workspace_write"}`)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/conversations", body))
		return recorder
	}

	if response := create("remote-project"); response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "仅支持本机") {
		t.Fatalf("远端项目应被拒绝，实际 %d body=%s", response.Code, response.Body.String())
	}
	response := create("local-project")
	if response.Code != http.StatusCreated {
		t.Fatalf("本机项目应当可以创建 CodeBuddy 会话，实际 %d body=%s", response.Code, response.Body.String())
	}
	var conversation Conversation
	if err := json.NewDecoder(response.Body).Decode(&conversation); err != nil {
		t.Fatalf("decode conversation: %v", err)
	}
	if conversation.AgentID != "codebuddy" || conversation.PermissionMode != "workspace_write" {
		t.Fatalf("unexpected conversation: %#v", conversation)
	}
}
