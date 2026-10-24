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

// TestCreateQoderConversationOnlyOnLocalRunner 钉住一条**创建时**就生效的边界：
//
// Qoder 的会话面只在本机（可执行文件按控制服务这台机器的 PATH 解析）。如果只在发送
// 消息时才 503，用户会先建好一个用不了的会话（手机端尤其难看出原因）。反过来，本机
// 项目必须能正常创建 —— 否则这条闸门就成了"把新工具挡在门外"。
func TestCreateQoderConversationOnlyOnLocalRunner(t *testing.T) {
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
		body := bytes.NewBufferString(`{"agentId":"qoder","permissionMode":"workspace_write"}`)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/conversations", body))
		return recorder
	}

	if response := create("remote-project"); response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "仅支持本机") {
		t.Fatalf("远端项目应被拒绝，实际 %d body=%s", response.Code, response.Body.String())
	}
	response := create("local-project")
	if response.Code != http.StatusCreated {
		t.Fatalf("本机项目应当可以创建 Qoder 会话，实际 %d body=%s", response.Code, response.Body.String())
	}
	var conversation Conversation
	if err := json.NewDecoder(response.Body).Decode(&conversation); err != nil {
		t.Fatalf("decode conversation: %v", err)
	}
	if conversation.AgentID != "qoder" || conversation.PermissionMode != "workspace_write" {
		t.Fatalf("unexpected conversation: %#v", conversation)
	}
}
