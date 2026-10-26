package app

import (
	"encoding/json"
	"testing"
	"time"
)

// announceSessionStarting 是"大会话 resume 期间界面靠什么说话"的唯一数据源。
// 它必须：① 落成 system/session_starting；② 带上是否 resume；③ 历史条数只数主消息。
func TestAnnounceSessionStartingReportsResumeHistory(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('proj','proj',?,?,'main',1,?)`, t.TempDir(), server.localRunnerID(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,agent_id,agent_session_id,agent_runtime_id,status,created_at) values ('conv','proj','sess','claude-code','sess',?,'running',?)`, server.localRunnerID(), now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	if _, err := server.db.Exec(`insert into runs (id,conversation_id,status,created_at) values ('run','conv','running',?)`, now); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	// 两条主消息 + 一条子代理消息：后者不该计入"历史条数"（与远端快照口径一致）。
	for _, row := range []struct{ id, parent string }{
		{"m1", ""},
		{"m2", ""},
		{"m3", "toolu_1"},
	} {
		if _, err := server.db.Exec(`insert into messages (id,conversation_id,run_id,role,content,parent_tool_use_id,created_at) values (?,?,?,?,?,?,?)`, row.id, "conv", "run", "user", "hi", row.parent, now); err != nil {
			t.Fatalf("insert message %s: %v", row.id, err)
		}
	}

	server.announceSessionStarting("run", "conv", true)

	var raw string
	if err := server.db.QueryRow(`select payload from events where run_id='run' and type='system' order by created_at desc limit 1`).Scan(&raw); err != nil {
		t.Fatalf("load event: %v", err)
	}
	var payload struct {
		Subtype         string `json:"subtype"`
		Resume          bool   `json:"resume"`
		HistoryMessages int    `json:"historyMessages"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.Subtype != "session_starting" {
		t.Fatalf("subtype = %q, want session_starting", payload.Subtype)
	}
	if !payload.Resume {
		t.Fatal("resume = false, want true")
	}
	if payload.HistoryMessages != 2 {
		t.Fatalf("historyMessages = %d, want 2 (子代理消息不计入)", payload.HistoryMessages)
	}
}

// 新会话没有历史可载入：resume=false 时不该去数消息（多一条查询就多一次单连接占用）。
func TestAnnounceSessionStartingSkipsHistoryCountForFreshSession(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('proj2','proj2',?,?,'main',1,?)`, t.TempDir(), server.localRunnerID(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,agent_id,agent_session_id,agent_runtime_id,status,created_at) values ('conv2','proj2','sess2','claude-code','sess2',?,'running',?)`, server.localRunnerID(), now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	if _, err := server.db.Exec(`insert into runs (id,conversation_id,status,created_at) values ('run2','conv2','running',?)`, now); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	server.announceSessionStarting("run2", "conv2", false)

	var raw string
	if err := server.db.QueryRow(`select payload from events where run_id='run2' and type='system' order by created_at desc limit 1`).Scan(&raw); err != nil {
		t.Fatalf("load event: %v", err)
	}
	var payload struct {
		Resume          bool `json:"resume"`
		HistoryMessages int  `json:"historyMessages"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.Resume || payload.HistoryMessages != 0 {
		t.Fatalf("fresh session payload = %+v, want resume=false history=0", payload)
	}
}

func TestMCPTimeoutEnvClampsAndSkipsEmpty(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout time.Duration
		want    string
	}{
		{"未注入时不设变量", 0, ""},
		{"低于下限抬到 5s", 2 * time.Second, "MCP_TIMEOUT=5000"},
		{"按配置值", 20 * time.Second, "MCP_TIMEOUT=20000"},
		{"高于上限压到 30s", 2 * time.Minute, "MCP_TIMEOUT=30000"},
	} {
		got := mcpTimeoutEnv(tc.timeout)
		if tc.want == "" {
			if len(got) != 0 {
				t.Fatalf("%s: got %v, want none", tc.name, got)
			}
			continue
		}
		if len(got) != 1 || got[0] != tc.want {
			t.Fatalf("%s: got %v, want [%s]", tc.name, got, tc.want)
		}
	}
}
