package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 上下文重建：回退到某个节点之后，模型也真的只从那一刻继续。
//
// 截断消息只是把界面上的内容藏起来：长驻 CLI 的原生会话里那些回合还在，下一轮照样
// `--resume`。于是用户看到的是"回到了第 N 轮"，模型却仍然记得并引用被丢掉的第 N+1 轮
// 做过的事 —— 而工作区已经按那一刻恢复了，两边对不上（模型会说"那个文件我已经创建了"，
// 而它并不存在）。所以真的丢了回合时：换一个新的原生会话，并在下一轮把保留的历史喂回去。

func seedRebuildableConversation(t *testing.T, server *Server) (string, time.Time) {
	t.Helper()
	root := t.TempDir()
	now := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('rebuild-project','rebuild',?,'windows-local','main',1,?)`, root, now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,agent_id,agent_session_id,agent_initialized,claude_initialized,status,is_current,created_at,last_activity_at) values ('rebuild-conversation','rebuild-project','session-before','claude-code','session-before',1,1,'idle',1,?,?)`, now, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	return root, now
}

// 回退到某个节点时，只要真的丢了回合，原生会话就必须换一个新的身份：不换的话下一轮
// `--resume` 会把被丢掉的回合原样带回来。
func TestRewindResetsTheNativeSessionWhenTurnsAreDiscarded(t *testing.T) {
	server := newTestServer(t)
	_, now := seedRebuildableConversation(t, server)
	checkpointAt := now.Add(time.Minute)
	if _, err := server.db.Exec(`insert into conversation_checkpoints (id,conversation_id,agent_id,prompt,status,metadata,created_at) values ('rebuild-node','rebuild-conversation','claude-code','first','active','{}',?)`, checkpointAt); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into runs (id,conversation_id,status,created_at) values ('rebuild-run','rebuild-conversation','completed',?)`, checkpointAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into messages (id,conversation_id,run_id,role,content,parent_tool_use_id,created_at) values ('dropped-turn','rebuild-conversation','rebuild-run','user','这之后的一轮','',?)`, checkpointAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/conversations/rebuild-conversation/rewind", strings.NewReader(`{"checkpointId":"rebuild-node","mode":"conversation"}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("rewind status=%d body=%s", response.Code, response.Body.String())
	}
	var sessionID string
	var agentInitialized, claudeInitialized, pending int
	if err := server.db.QueryRow(`select agent_session_id,agent_initialized,claude_initialized,pending_context_rebuild from conversations where id='rebuild-conversation'`).Scan(&sessionID, &agentInitialized, &claudeInitialized, &pending); err != nil {
		t.Fatal(err)
	}
	if sessionID == "session-before" || sessionID == "" {
		t.Fatalf("agent_session_id = %q, want a fresh session id", sessionID)
	}
	if agentInitialized != 0 || claudeInitialized != 0 {
		t.Fatal("原生会话仍然被当成已初始化：下一轮会 --resume 旧会话，被回退掉的回合会回来")
	}
	if pending != 1 {
		t.Fatal("没有置位重建标记：下一轮不会拿到保留的历史")
	}
}

// 回退到**最新**节点是个空操作（后面没有回合可丢）：那时不许动会话身份，否则一次白点的
// 回退会把模型的记忆整段抹掉。
func TestRewindToTheLatestNodeLeavesTheSessionAlone(t *testing.T) {
	server := newTestServer(t)
	_, now := seedRebuildableConversation(t, server)
	checkpointAt := now.Add(time.Minute)
	if _, err := server.db.Exec(`insert into conversation_checkpoints (id,conversation_id,agent_id,prompt,status,metadata,created_at) values ('latest-node','rebuild-conversation','claude-code','first','active','{}',?)`, checkpointAt); err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/conversations/rebuild-conversation/rewind", strings.NewReader(`{"checkpointId":"latest-node","mode":"conversation"}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("rewind status=%d body=%s", response.Code, response.Body.String())
	}
	var sessionID string
	var pending int
	if err := server.db.QueryRow(`select agent_session_id,pending_context_rebuild from conversations where id='rebuild-conversation'`).Scan(&sessionID, &pending); err != nil {
		t.Fatal(err)
	}
	if sessionID != "session-before" || pending != 0 {
		t.Fatalf("session=%q pending=%d，空操作的回退不该重置会话", sessionID, pending)
	}
}

// 置位之后的第一轮：把保留的历史拼进**发出去的提示词**（用户那条消息本身保持原样），
// 用完清位；否则第二轮会再塞一遍同样的历史。
func TestNextTurnAfterRewindSeedsTheKeptHistory(t *testing.T) {
	server := newTestServer(t)
	_, now := seedRebuildableConversation(t, server)
	checkpointAt := now.Add(time.Minute)
	if _, err := server.db.Exec(`insert into conversation_checkpoints (id,conversation_id,agent_id,prompt,status,metadata,created_at) values ('seed-node','rebuild-conversation','claude-code','first','active','{}',?)`, checkpointAt); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into runs (id,conversation_id,status,created_at) values ('seed-run','rebuild-conversation','completed',?)`, now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into messages (id,conversation_id,run_id,role,content,parent_tool_use_id,created_at) values ('kept-user','rebuild-conversation','seed-run','user','把登录页改成深色','',?),('kept-assistant','rebuild-conversation','seed-run','assistant','已经改完 LoginPage.tsx','',?)`, now, now.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	// 被回退掉的那一轮（在节点之后）：它绝不能出现在喂回去的历史里。
	if _, err := server.db.Exec(`insert into runs (id,conversation_id,status,created_at) values ('dropped-run','rebuild-conversation','completed',?)`, checkpointAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into messages (id,conversation_id,run_id,role,content,parent_tool_use_id,created_at) values ('dropped-user','rebuild-conversation','dropped-run','user','再把配色换成紫色','',?),('dropped-assistant','rebuild-conversation','dropped-run','assistant','已改成紫色主题','',?)`, checkpointAt.Add(time.Second), checkpointAt.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`update conversations set pending_context_rebuild=1, checkpoint_hidden_after=?, checkpoint_rewound_at=?, agent_session_id='fresh-session', claude_session_id='fresh-session', agent_initialized=0, claude_initialized=0 where id='rebuild-conversation'`, checkpointAt, checkpointAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	prompts := make(chan string, 1)
	server.runner = runnerFunc(func(_ context.Context, request AgentRunRequest, _ AgentRunSink) error {
		prompts <- request.Prompt
		return nil
	})
	stored, runID, _, status, err := server.startMessage(context.Background(), "rebuild-conversation", "继续，把标题也改一下", "", nil)
	if err != nil || status >= 300 {
		t.Fatalf("startMessage status=%d err=%v", status, err)
	}
	var seeded string
	select {
	case seeded = <-prompts:
	case <-time.After(5 * time.Second):
		t.Fatal("这一轮没有跑到 runner")
	}
	if !strings.Contains(seeded, "【会话回退】") || !strings.Contains(seeded, "把登录页改成深色") || !strings.Contains(seeded, "已经改完 LoginPage.tsx") {
		t.Fatalf("提示词里没有保留的历史：%q", seeded)
	}
	if strings.Contains(seeded, "再把配色换成紫色") || strings.Contains(seeded, "已改成紫色主题") {
		t.Fatalf("被回退掉的回合混进了喂回去的历史：%q", seeded)
	}
	if !strings.HasSuffix(seeded, "继续，把标题也改一下") {
		t.Fatalf("用户这一轮的消息必须原样留在最后：%q", seeded)
	}
	// 用户那条消息本身保持原样：界面里只该看到用户打的那句话，不该出现那整段历史。
	if stored.Content != "继续，把标题也改一下" {
		t.Fatalf("存下来的用户消息被污染了：%q", stored.Content)
	}
	var polluted int
	if err := server.db.QueryRow(`select count(*) from messages where conversation_id='rebuild-conversation' and content like ?`, "%会话回退%").Scan(&polluted); err != nil {
		t.Fatal(err)
	}
	if polluted != 0 {
		t.Fatal("前置提示被写进了消息表：界面里会多出一整段历史")
	}
	_ = runID
	// 用完清位：第二轮不该再塞一遍。清位发生在**这一轮跑完**（finishRun completed）之后，
	// 不是"消息一受理"就清 —— 受理后 CLI 仍可能拉不起来，那时历史没喂进去，下一轮必须重喂。
	deadline := time.Now().Add(5 * time.Second)
	for {
		var pending int
		if err := server.db.QueryRow(`select pending_context_rebuild from conversations where id='rebuild-conversation'`).Scan(&pending); err != nil {
			t.Fatal(err)
		}
		if pending == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("重建标记没有清掉：每一轮都会把历史再塞一遍")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// 这一轮没跑起来（失败/被停止）时**不清**重建标记：原生会话已经是全新的，历史还没喂进去，
// 下一轮重试必须重新喂一遍。
func TestFailedTurnKeepsTheContextRebuildFlag(t *testing.T) {
	server := newTestServer(t)
	_, now := seedRebuildableConversation(t, server)
	checkpointAt := now.Add(time.Minute)
	if _, err := server.db.Exec(`insert into conversation_checkpoints (id,conversation_id,agent_id,prompt,status,metadata,created_at) values ('failed-node','rebuild-conversation','claude-code','first','active','{}',?)`, checkpointAt); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`update conversations set pending_context_rebuild=1, agent_session_id='fresh-session', claude_session_id='fresh-session', agent_initialized=0, claude_initialized=0 where id='rebuild-conversation'`); err != nil {
		t.Fatal(err)
	}
	failed := make(chan struct{}, 1)
	server.runner = runnerFunc(func(_ context.Context, _ AgentRunRequest, _ AgentRunSink) error {
		failed <- struct{}{}
		return errors.New("CLI 起不来")
	})
	if _, _, _, status, err := server.startMessage(context.Background(), "rebuild-conversation", "再来一次", "", nil); err != nil || status >= 300 {
		t.Fatalf("startMessage status=%d err=%v", status, err)
	}
	select {
	case <-failed:
	case <-time.After(5 * time.Second):
		t.Fatal("这一轮没有跑到 runner")
	}
	// 等 finishRun 收尾（失败分支不会清标记）。
	time.Sleep(200 * time.Millisecond)
	var pending int
	if err := server.db.QueryRow(`select pending_context_rebuild from conversations where id='rebuild-conversation'`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatal("失败的一轮把重建标记清掉了：重试时模型会在空会话里继续")
	}
}
