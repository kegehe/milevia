package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 回退之后**新发生**的回合不属于被丢掉的那段历史：它们必须照常显示。
//
// 只按"比节点新就藏起来"过滤，会把用户回退后刚发的那条消息连同失败卡片一起藏掉 ——
// 而那一刻正是他最需要看见它的时候（失败/被停止的回合不会生成新节点，cutoff 停在原地）。
func TestTurnsAfterARewindStayVisible(t *testing.T) {
	server := newTestServer(t)
	seedCheckpointConversation(t, server)
	base := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Millisecond)
	checkpointAt := base.Add(time.Minute)
	if _, err := server.db.Exec(`insert into conversation_checkpoints (id,conversation_id,agent_id,prompt,status,metadata,created_at) values ('visible-node','checkpoint-conversation','claude-code','first','active','{}',?)`, checkpointAt); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into runs (id,conversation_id,status,created_at) values ('old-run','checkpoint-conversation','completed',?)`, base); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into messages (id,conversation_id,run_id,role,content,parent_tool_use_id,created_at) values ('kept','checkpoint-conversation','old-run','user','保留的那条','',?),('discarded','checkpoint-conversation','old-run','user','被回退掉的那条','',?)`, base, checkpointAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/conversations/checkpoint-conversation/rewind", strings.NewReader(`{"checkpointId":"visible-node","mode":"conversation"}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("rewind status=%d body=%s", response.Code, response.Body.String())
	}
	// 回退之后用户又发了一条（这一轮失败了，所以不会有新节点）。
	if _, err := server.db.Exec(`insert into runs (id,conversation_id,status,created_at) values ('retry-run','checkpoint-conversation','failed',?)`, time.Now().UTC().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into messages (id,conversation_id,run_id,role,content,parent_tool_use_id,created_at) values ('retry','checkpoint-conversation','retry-run','user','回退后重试的那条','',?)`, time.Now().UTC().Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	// 再成功跑一轮（新节点 + 新消息）：这一步是回归点 —— 早先的判据用"最新 active 节点"当边界，
	// 新节点一出来，边界就越过被丢掉的区间，被回退掉的内容会**重新出现**。
	if _, err := server.db.Exec(`insert into conversation_checkpoints (id,conversation_id,agent_id,prompt,status,metadata,created_at) values ('new-node','checkpoint-conversation','claude-code','third','active','{}',?)`, time.Now().UTC().Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into runs (id,conversation_id,status,created_at) values ('after-run','checkpoint-conversation','completed',?)`, time.Now().UTC().Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into messages (id,conversation_id,run_id,role,content,parent_tool_use_id,created_at) values ('after-success','checkpoint-conversation','after-run','user','成功的新一轮','',?)`, time.Now().UTC().Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}

	response = httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/conversations/checkpoint-conversation?limit=50", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Messages []Message `json:"messages"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, message := range body.Messages {
		seen[message.ID] = true
	}
	if seen["discarded"] {
		t.Fatal("被回退掉的那条消息还看得见")
	}
	if !seen["kept"] {
		t.Fatal("节点之前的消息被误藏了")
	}
	if !seen["retry"] {
		t.Fatal("回退之后新发的那条被藏了：用户会以为自己的消息丢了")
	}
	if !seen["after-success"] {
		t.Fatal("回退之后成功跑的那一轮被藏了")
	}
}

// 「只恢复代码」不动节点状态：那一颗按钮写的是"对话历史不变"，跟着改 cutoff 等于
// 把之后的对话也从界面上抹掉（而且原生会话没重置，模型还记得，两端更不一致）。
func TestCodeOnlyRewindKeepsTheConversationCutoff(t *testing.T) {
	server := newTestServer(t)
	seedCheckpointConversation(t, server)
	base := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Millisecond)
	legacySnapshot := t.TempDir()
	if _, err := server.db.Exec(`insert into conversation_checkpoints (id,conversation_id,agent_id,prompt,snapshot_path,status,metadata,created_at) values ('code-node','checkpoint-conversation','claude-code','first',?,'active','{}',?)`, legacySnapshot, base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into conversation_checkpoints (id,conversation_id,agent_id,prompt,status,metadata,created_at) values ('later-node','checkpoint-conversation','claude-code','second','active','{}',?)`, base.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into runs (id,conversation_id,status,created_at) values ('run-later','checkpoint-conversation','completed',?)`, base.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into messages (id,conversation_id,run_id,role,content,parent_tool_use_id,created_at) values ('later-message','checkpoint-conversation','run-later','user','之后那一轮','',?)`, base.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/conversations/checkpoint-conversation/rewind", strings.NewReader(`{"checkpointId":"code-node","mode":"code"}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("rewind status=%d body=%s", response.Code, response.Body.String())
	}
	var laterStatus string
	if err := server.db.QueryRow(`select status from conversation_checkpoints where id='later-node'`).Scan(&laterStatus); err != nil {
		t.Fatal(err)
	}
	if laterStatus != "active" {
		t.Fatalf("later node status = %q，只恢复代码不该把对话回退掉", laterStatus)
	}
	response = httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/conversations/checkpoint-conversation?limit=50", nil))
	var body struct {
		Messages []Message `json:"messages"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, message := range body.Messages {
		if message.ID == "later-message" {
			found = true
		}
	}
	if !found {
		t.Fatal("只恢复代码却把之后的对话藏起来了")
	}
}
