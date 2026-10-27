package app

import (
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// 运行结束必须立刻广播 projects 失效事件：总览页/项目页是按 conversations.status
// 聚合"正在执行"的，少了这条广播它们只能等 30 秒兜底轮询，于是"刚跑完"还会
// 继续显示运行中（反之"刚开始跑"也显示不出来）。
func TestFinishRunBroadcastsProjectState(t *testing.T) {
	server := newTestServer(t)
	sub := &stateEventSubscriber{send: make(chan StateEvent, 4)}
	server.stateEventSubs = map[*websocket.Conn]*stateEventSubscriber{nil: sub}

	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,claude_initialized,is_current,created_at) values ('conversation','project','00000000-0000-4000-8000-000000000000','running',0,1,?)`, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	if _, err := server.db.Exec(`insert into runs (id,conversation_id,status,created_at) values ('run','conversation','running',?)`, now); err != nil {
		t.Fatalf("insert run: %v", err)
	}

	server.finishRun("run", "conversation", "completed", nil)

	// 只看"有没有 projects"，不假定它是第一条：服务启动时的后台维护也可能广播，
	// 断言"第一条恰好是 projects"会变成与启动顺序耦合的假失败。
	deadline := time.After(2 * time.Second)
	for {
		select {
		case event := <-sub.send:
			if event.Type == stEvProjects {
				return
			}
		case <-deadline:
			t.Fatal("运行结束没有广播 projects 失效事件：总览页只能等 30 秒兜底轮询")
		}
	}
}
