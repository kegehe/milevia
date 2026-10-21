package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeCheckpointFile 写一个测试文件（自动建目录）。
func writeCheckpointFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readCheckpointFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}

// 快照的核心契约：回退之后工作区**就是**那一刻的样子 —— 改回来的改回来、删掉的回来、
// 快照之后新建的删掉、.gitignore 命中的（node_modules / build / 日志）从头到尾不动。
// 这条链是"恢复代码"的全部意义，缺一条用户拿到的就是半个回退。
func TestWorkspaceSnapshotRestoreIsReal(t *testing.T) {
	server := newTestServer(t)
	root := seedCheckpointConversation(t, server)
	server.config.DataDir = t.TempDir()
	writeCheckpointFile(t, filepath.Join(root, ".gitignore"), "build/\n*.log\n")
	writeCheckpointFile(t, filepath.Join(root, "a.txt"), "before\n")
	writeCheckpointFile(t, filepath.Join(root, "sub", "s.txt"), "nested\n")
	writeCheckpointFile(t, filepath.Join(root, "build", "artifact.bin"), "ignored\n")
	writeCheckpointFile(t, filepath.Join(root, "debug.log"), "log\n")

	gitDir, ref, metadata, err := server.createWorkspaceSnapshot(t.Context(), "checkpoint-project", root, "windows-local", "checkpoint-conversation")
	if err != nil {
		t.Fatal(err)
	}
	if metadata["engine"] != checkpointEngineGit {
		t.Fatalf("engine = %#v, want %s", metadata["engine"], checkpointEngineGit)
	}
	if !isFullGitObjectID(ref) {
		t.Fatalf("snapshot ref = %q, want a full object id", ref)
	}
	if _, err := os.Stat(filepath.Join(gitDir, "HEAD")); err != nil {
		t.Fatalf("shadow repository missing: %v", err)
	}

	// 一轮之后：改一个、删一个、新建两个（其中一个是 .gitignore 命中的）。
	writeCheckpointFile(t, filepath.Join(root, "a.txt"), "after\n")
	if err := os.Remove(filepath.Join(root, "sub", "s.txt")); err != nil {
		t.Fatal(err)
	}
	writeCheckpointFile(t, filepath.Join(root, "new.txt"), "new\n")
	writeCheckpointFile(t, filepath.Join(root, "fresh.log"), "log\n")

	if err := server.restoreCheckpointCode(t.Context(), checkpointRestoreRequest{ProjectID: "checkpoint-project", ProjectPath: root, Runner: "windows-local", SnapshotDir: gitDir, SnapshotRef: ref, Engine: checkpointEngineGit}); err != nil {
		t.Fatal(err)
	}
	if got := readCheckpointFile(t, filepath.Join(root, "a.txt")); got != "before\n" {
		t.Fatalf("a.txt = %q, want the snapshot content", got)
	}
	if got := readCheckpointFile(t, filepath.Join(root, "sub", "s.txt")); got != "nested\n" {
		t.Fatalf("deleted file was not restored: %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "new.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("file created after the snapshot survived the rewind")
	}
	for _, kept := range []string{filepath.Join(root, "build", "artifact.bin"), filepath.Join(root, "debug.log"), filepath.Join(root, "fresh.log")} {
		if _, err := os.Stat(kept); err != nil {
			t.Fatalf("ignored file %s must not be touched by a rewind: %v", kept, err)
		}
	}
	// 恢复出来的文件必须可写：早先的遗留实现用 entry.Type() 当权限位，perm 0 在 Windows 上
	// 等于把每个文件打成只读，之后 agent 再也改不动。
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("again\n"), 0o644); err != nil {
		t.Fatalf("restored file is not writable: %v", err)
	}
}

// 手机端的快照也必须按节点时间过滤：快照是手机端**唯一**的历史来源（它没有翻页，
// 只看这一份），漏掉过滤就等于"用户在电脑端回退了，手机端却把丢掉的回合原样拿回来"。
func TestRemoteSnapshotAppliesCheckpointCutoff(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('snapshot-project','Snapshot',?,'ssh-remote','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,claude_initialized,is_current,created_at) values ('snapshot-conversation','snapshot-project','snapshot-session','idle',0,1,?)`, now); err != nil {
		t.Fatal(err)
	}
	cutoff := now.Add(time.Minute)
	if _, err := server.db.Exec(`insert into conversation_checkpoints (id,conversation_id,status,metadata,created_at) values ('snapshot-node','snapshot-conversation','active','{}',?)`, cutoff); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`update conversations set checkpoint_hidden_after=?,checkpoint_rewound_at=? where id='snapshot-conversation'`, cutoff, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into runs (id,conversation_id,status,created_at) values ('snapshot-run','snapshot-conversation','completed',?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into messages (id,conversation_id,run_id,role,content,parent_tool_use_id,created_at) values ('kept-message','snapshot-conversation','snapshot-run','user','kept','',?),('dropped-message','snapshot-conversation','snapshot-run','user','dropped','',?)`, now, cutoff.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into events (id,conversation_id,run_id,type,payload,created_at) values ('kept-event','snapshot-conversation','snapshot-run','compact.completed','{"subtype":"compact_result"}',?),('dropped-event','snapshot-conversation','snapshot-run','compact.completed','{"subtype":"compact_result"}',?)`, now, cutoff.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	server.remoteSnapshot(response, httptest.NewRequest(http.MethodGet, "/api/remote/snapshot", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("snapshot status=%d body=%s", response.Code, response.Body.String())
	}
	var snapshot remoteSnapshot
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Projects) != 1 || len(snapshot.Projects[0].Conversations) != 1 {
		t.Fatalf("unexpected snapshot shape: %#v", snapshot.Projects)
	}
	conversation := snapshot.Projects[0].Conversations[0]
	for _, message := range conversation.Messages {
		if message.ID == "dropped-message" {
			t.Fatal("回退掉的用户消息漏进了手机快照")
		}
	}
	if len(conversation.Messages) != 1 || conversation.Messages[0].ID != "kept-message" {
		t.Fatalf("messages = %#v, want only the kept one", conversation.Messages)
	}
	for _, notice := range conversation.Notices {
		if notice.ID == "dropped-event" {
			t.Fatal("回退掉的事件漏进了手机快照的通知")
		}
	}
}

// 被打断的 git 进程会留下 index.lock（Windows 上被 kill 不走清理路径），而快照仓库只有我们写、
// 同一仓库上的操作在构造上就是串行的 —— 所以命令开始前还存在的锁一定是死的。不清掉的话，
// 那个节点会**永远**转换/快照失败，而失败是静默的（只写日志）。真机上出现过这份死锁。
func TestWorkspaceSnapshotRecoversFromStaleIndexLock(t *testing.T) {
	server := newTestServer(t)
	root := seedCheckpointConversation(t, server)
	server.config.DataDir = t.TempDir()
	writeCheckpointFile(t, filepath.Join(root, "a.txt"), "before\n")

	gitDir, _, _, err := server.createWorkspaceSnapshot(t.Context(), "checkpoint-project", root, "windows-local", "checkpoint-conversation")
	if err != nil {
		t.Fatal(err)
	}
	writeCheckpointFile(t, filepath.Join(gitDir, "index.lock"), "")
	writeCheckpointFile(t, filepath.Join(root, "a.txt"), "after\n")

	_, ref, _, err := server.createWorkspaceSnapshot(t.Context(), "checkpoint-project", root, "windows-local", "checkpoint-conversation")
	if err != nil {
		t.Fatalf("snapshot with a stale index lock failed: %v", err)
	}
	if !isFullGitObjectID(ref) {
		t.Fatalf("snapshot ref = %q", ref)
	}
	if _, err := os.Stat(filepath.Join(gitDir, "index.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale index lock was left behind")
	}
}

func TestRestoreCheckpointRejectsMissingSnapshot(t *testing.T) {
	server := newTestServer(t)
	if err := server.restoreCheckpointCode(t.Context(), checkpointRestoreRequest{ProjectPath: t.TempDir(), SnapshotDir: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Fatal("missing snapshot was accepted")
	}
	if err := server.restoreCheckpointCode(t.Context(), checkpointRestoreRequest{ProjectPath: t.TempDir()}); err == nil {
		t.Fatal("empty snapshot path was accepted")
	}
}

// 遗留拷贝型快照（升级前创建的节点）仍然要能用，且恢复出来的文件必须可写。
func TestLegacyCheckpointRestoreKeepsFilesWritable(t *testing.T) {
	project := t.TempDir()
	snapshot := t.TempDir()
	writeCheckpointFile(t, filepath.Join(snapshot, "a.txt"), "before\n")
	writeCheckpointFile(t, filepath.Join(project, "a.txt"), "after\n")
	if err := restoreLegacyCheckpointSnapshot(snapshot, project); err != nil {
		t.Fatal(err)
	}
	if got := readCheckpointFile(t, filepath.Join(project, "a.txt")); got != "before\n" {
		t.Fatalf("a.txt = %q", got)
	}
	if err := os.WriteFile(filepath.Join(project, "a.txt"), []byte("again\n"), 0o644); err != nil {
		t.Fatalf("restored file is not writable: %v", err)
	}
}

func seedCheckpointConversation(t *testing.T, server *Server) string {
	t.Helper()
	root := t.TempDir()
	now := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('checkpoint-project','checkpoint-project',?,'windows-local','main',1,?)`, root, now); err != nil {
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

// 可见性边界存在会话行上，**不能**由"最新 active 节点"推导：回退之后再成功跑一轮就会多出
// 一个新节点，那个推导出来的边界会立刻越过被丢掉的区间，让被回退掉的内容重新出现。
func TestCheckpointVisibilityUsesTheStoredLandingPoint(t *testing.T) {
	server := newTestServer(t)
	seedCheckpointConversation(t, server)
	first := time.Now().UTC().Add(-4 * time.Minute).Truncate(time.Millisecond)
	landing := first.Add(time.Minute)
	rewoundAt := first.Add(2 * time.Minute)
	if _, err := server.db.Exec(`insert into conversation_checkpoints (id,conversation_id,status,metadata,created_at) values ('checkpoint-kept','checkpoint-conversation','active','{}',?),('checkpoint-discarded','checkpoint-conversation','rewound','{}',?)`, landing, first); err != nil {
		t.Fatal(err)
	}
	// 回退之后又成功跑了一轮：新节点是 active，但它不在"被丢掉"的边界上。
	if _, err := server.db.Exec(`insert into conversation_checkpoints (id,conversation_id,status,metadata,created_at) values ('checkpoint-newer','checkpoint-conversation','active','{}',?)`, rewoundAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`update conversations set checkpoint_hidden_after=?, checkpoint_rewound_at=? where id='checkpoint-conversation'`, landing, rewoundAt); err != nil {
		t.Fatal(err)
	}
	hiddenAfter, rewound, err := checkpointVisibilityFor(t.Context(), server.db, "checkpoint-conversation")
	if err != nil {
		t.Fatal(err)
	}
	if hiddenAfter == nil || !hiddenAfter.Equal(landing) {
		t.Fatalf("hiddenAfter=%v want %v（不能被更新的节点顶掉）", hiddenAfter, landing)
	}
	if rewound == nil || !rewound.Equal(rewoundAt) {
		t.Fatalf("rewoundAt=%v want %v", rewound, rewoundAt)
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
	rewoundAt := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := server.db.Exec(`update conversations set checkpoint_hidden_after=?,checkpoint_rewound_at=? where id='checkpoint-conversation'`, checkpointAt, rewoundAt); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/conversations/checkpoint-conversation?limit=50", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("conversation status=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Events               []Event `json:"events"`
		MessagesCutoff       string  `json:"messagesCutoff"`
		MessagesVisibleAfter string  `json:"messagesVisibleAfter"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Events) != 1 || body.Events[0].ID != "event-before" {
		t.Fatalf("events after cutoff leaked: %#v", body.Events)
	}
	// 两个边界必须一起下发：客户端（桌面 reload / 手机历史分页缓存）按 cutoff 丢掉本地旧内容，
	// 再按 visibleAfter 把**回退之后新发生的回合**留下来。只给 cutoff 会让回退后的新消息
	// 在下次刷新时整体消失 —— 这类缺字段的 bug 在界面上表现为"消息自己没了"，很难反查。
	if body.MessagesCutoff == "" || body.MessagesVisibleAfter == "" {
		t.Fatalf("回包必须同时带 messagesCutoff 与 messagesVisibleAfter：cutoff=%q visibleAfter=%q", body.MessagesCutoff, body.MessagesVisibleAfter)
	}
	cutoffAt, cutoffErr := time.Parse(time.RFC3339Nano, body.MessagesCutoff)
	visibleAt, visibleErr := time.Parse(time.RFC3339Nano, body.MessagesVisibleAfter)
	if cutoffErr != nil || visibleErr != nil || !cutoffAt.Equal(checkpointAt) || !visibleAt.Equal(rewoundAt) {
		t.Fatalf("可见性边界回读不一致：cutoff=%v/%v visibleAfter=%v/%v", cutoffAt, cutoffErr, visibleAt, visibleErr)
	}
}

// 回退之后新跑的回合必须还在：两端客户端都会拿回包里的两个边界去过滤**本地**缓存
// （桌面 reload 合并、手机历史分页），只给 cutoff 会让"回退后新发的那条消息"在下次
// 刷新时整体消失。这条用例同时打两个出口：桌面详情（messages + events）与手机历史
// （messagesOnly=true），两边都必须把那个新回合带回来，并同时给出两个边界。
func TestConversationDetailKeepsMessagesCreatedAfterRewind(t *testing.T) {
	server := newTestServer(t)
	seedCheckpointConversation(t, server)
	base := time.Now().UTC().Add(-5 * time.Minute).Truncate(time.Millisecond)
	hiddenAfter := base.Add(time.Minute)
	rewoundAt := base.Add(2 * time.Minute)
	newer := base.Add(3 * time.Minute)
	if _, err := server.db.Exec(`insert into messages (id,conversation_id,run_id,role,content,parent_tool_use_id,created_at) values ('message-before','checkpoint-conversation','run-before','user','回退前','',?),('message-discarded','checkpoint-conversation','run-old','user','被回退掉','',?),('message-after-rewind','checkpoint-conversation','run-new','user','回退之后新发的一条','',?)`, base, hiddenAfter.Add(30*time.Second), newer); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`update conversations set checkpoint_hidden_after=?,checkpoint_rewound_at=? where id='checkpoint-conversation'`, hiddenAfter, rewoundAt); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/api/conversations/checkpoint-conversation?limit=50",
		"/api/conversations/checkpoint-conversation?limit=50&messagesOnly=true",
	} {
		response := httptest.NewRecorder()
		server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, response.Code, response.Body.String())
		}
		var body struct {
			Messages             []Message `json:"messages"`
			MessagesCutoff       string    `json:"messagesCutoff"`
			MessagesVisibleAfter string    `json:"messagesVisibleAfter"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		ids := make([]string, 0, len(body.Messages))
		for _, message := range body.Messages {
			ids = append(ids, message.ID)
		}
		if len(ids) != 2 || ids[0] != "message-before" || ids[1] != "message-after-rewind" {
			t.Fatalf("%s messages=%v，期望 [message-before message-after-rewind]（被回退掉的那条要藏起来，回退后新发的要留下）", path, ids)
		}
		if body.MessagesCutoff == "" || body.MessagesVisibleAfter == "" {
			t.Fatalf("%s 必须同时下发 messagesCutoff 与 messagesVisibleAfter：cutoff=%q visibleAfter=%q", path, body.MessagesCutoff, body.MessagesVisibleAfter)
		}
		cutoffAt, cutoffErr := time.Parse(time.RFC3339Nano, body.MessagesCutoff)
		visibleAt, visibleErr := time.Parse(time.RFC3339Nano, body.MessagesVisibleAfter)
		if cutoffErr != nil || visibleErr != nil || !cutoffAt.Equal(hiddenAfter) || !visibleAt.Equal(rewoundAt) {
			t.Fatalf("%s 边界回读不一致：cutoff=%v/%v visibleAfter=%v/%v", path, cutoffAt, cutoffErr, visibleAt, visibleErr)
		}
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
	seedCheckpointConversation(t, server)
	if _, err := server.db.Exec(`insert into messages (id,conversation_id,run_id,role,content,created_at) values ('message-1','checkpoint-conversation','old-run','user','keep',?)`, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	snapshot := t.TempDir()
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

// 列表要给界面两样东西：runId（把节点挂到时间线上那条用户消息）与 hasSnapshot
// （这个节点现在到底还能不能恢复代码）。
func TestListConversationCheckpointsReportsRunIDAndSnapshotPresence(t *testing.T) {
	server := newTestServer(t)
	seedCheckpointConversation(t, server)
	server.config.DataDir = t.TempDir()
	now := time.Now().UTC()
	present := filepath.Join(server.checkpointStorageRoot(), "checkpoints", "checkpoint-conversation", checkpointShadowRepoName)
	if err := os.MkdirAll(present, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into conversation_checkpoints (id,conversation_id,agent_id,run_id,prompt,snapshot_path,snapshot_ref,status,metadata,created_at) values ('checkpoint-with','checkpoint-conversation','claude-code','run-1','first',?,?,'active','{}',?),('checkpoint-without','checkpoint-conversation','claude-code','run-2','second','',?,'rewound','{}',?)`, present, strings.Repeat("a", 40), now, "", now.Add(time.Second)); err != nil {
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
	if len(body.Checkpoints) != 2 {
		t.Fatalf("checkpoints = %#v", body.Checkpoints)
	}
	if body.Checkpoints[0].RunID != "run-1" || !body.Checkpoints[0].HasSnapshot {
		t.Fatalf("first checkpoint = %#v, want run-1 with a snapshot", body.Checkpoints[0])
	}
	if body.Checkpoints[1].RunID != "run-2" || body.Checkpoints[1].HasSnapshot {
		t.Fatalf("second checkpoint = %#v, want run-2 without a snapshot", body.Checkpoints[1])
	}
}

// 保留策略：最近 N 个节点与"当前节点"必须活下来，超出的删行；只有一节点一目录的遗留拷贝
// 快照能随行删目录 —— 影子仓库是整会话共用的，误删会把其它节点的快照一起带走。
func TestPruneConversationCheckpointsKeepsActiveAndNewest(t *testing.T) {
	server := newTestServer(t)
	seedCheckpointConversation(t, server)
	server.config.DataDir = t.TempDir()
	base := time.Now().UTC().Add(-time.Duration(checkpointRetentionPerConversation+10) * time.Minute)
	legacyDir := filepath.Join(server.checkpointStorageRoot(), "checkpoints", "checkpoint-conversation", "legacy-node")
	if err := os.MkdirAll(legacyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	shadowDir := filepath.Join(server.checkpointStorageRoot(), "checkpoints", "checkpoint-conversation", checkpointShadowRepoName)
	if err := os.MkdirAll(shadowDir, 0o700); err != nil {
		t.Fatal(err)
	}
	total := checkpointRetentionPerConversation + 5
	for index := 0; index < total; index++ {
		id := fmt.Sprintf("node-%02d", index)
		// 用户回退到了**最老**的那个节点：之后的全是 rewound，只有它一个 active。
		// 它不在"最近 N 个"里，但它是"我在哪"，必须活下来。
		// （created_at 随 index 递增 → index 0 才是最老的。）
		status, path, ref := "rewound", legacyDir, ""
		if index == 0 {
			status = "active"
		}
		if index == 1 {
			// 影子仓库（共用）——即使被裁掉也不能删目录。
			path, ref = shadowDir, strings.Repeat("b", 40)
		}
		if _, err := server.db.Exec(`insert into conversation_checkpoints (id,conversation_id,status,snapshot_path,snapshot_ref,metadata,created_at) values (?,?,?,?,?,'{}',?)`, id, "checkpoint-conversation", status, path, ref, base.Add(time.Duration(index)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	server.pruneConversationCheckpoints(t.Context(), "checkpoint-conversation")

	var remaining int
	if err := server.db.QueryRow(`select count(*) from conversation_checkpoints where conversation_id=?`, "checkpoint-conversation").Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	// 最近 N 个 + 那个被回退到的老节点。
	if remaining != checkpointRetentionPerConversation+1 {
		t.Fatalf("remaining = %d, want %d", remaining, checkpointRetentionPerConversation+1)
	}
	var activeRemaining int
	if err := server.db.QueryRow(`select count(*) from conversation_checkpoints where conversation_id=? and id=?`, "checkpoint-conversation", "node-00").Scan(&activeRemaining); err != nil {
		t.Fatal(err)
	}
	if activeRemaining != 1 {
		t.Fatal("被回退到的那个老节点被裁掉了：界面会失去「我在哪」")
	}
	if _, err := os.Stat(legacyDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("legacy copy snapshot of a pruned node was left on disk")
	}
	if _, err := os.Stat(shadowDir); err != nil {
		t.Fatalf("shared shadow repository must survive pruning: %v", err)
	}
}

// 启动清扫：会话已删除的目录整个删掉；没人引用的遗留目录删掉；还被引用的遗留拷贝快照
// 转成 git 快照（省空间），转换后原来的拷贝目录必须消失。
func TestSweepCheckpointSnapshotsCleansAndConverts(t *testing.T) {
	server := newTestServer(t)
	root := seedCheckpointConversation(t, server)
	server.config.DataDir = t.TempDir()
	storageRoot := server.checkpointStorageRoot()

	orphanDir := filepath.Join(storageRoot, "checkpoints", "deleted-conversation", "whatever")
	if err := os.MkdirAll(orphanDir, 0o700); err != nil {
		t.Fatal(err)
	}
	unreferencedDir := filepath.Join(storageRoot, "checkpoints", "checkpoint-conversation", "unreferenced")
	if err := os.MkdirAll(unreferencedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacyDir := filepath.Join(storageRoot, "checkpoints", "checkpoint-conversation", "legacy-copy")
	writeCheckpointFile(t, filepath.Join(legacyDir, "a.txt"), "before\n")
	writeCheckpointFile(t, filepath.Join(legacyDir, "node_modules", "x.js"), "dep\n")
	writeCheckpointFile(t, filepath.Join(legacyDir, ".gitignore"), "node_modules/\n")
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into conversation_checkpoints (id,conversation_id,status,snapshot_path,prompt,metadata,created_at) values ('node-legacy','checkpoint-conversation','active',?,'first','{"fileCount":2}',?)`, legacyDir, now); err != nil {
		t.Fatal(err)
	}

	server.sweepCheckpointSnapshots(t.Context())

	if _, err := os.Stat(orphanDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("snapshot directory of a deleted conversation was left on disk")
	}
	if _, err := os.Stat(unreferencedDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unreferenced snapshot directory was left on disk")
	}
	if _, err := os.Stat(legacyDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("converted legacy copy directory was left on disk")
	}
	var path, ref, metadata string
	if err := server.db.QueryRow(`select snapshot_path,snapshot_ref,metadata from conversation_checkpoints where id='node-legacy'`).Scan(&path, &ref, &metadata); err != nil {
		t.Fatal(err)
	}
	if !isFullGitObjectID(ref) {
		t.Fatalf("converted node ref = %q", ref)
	}
	if checkpointEngineOf(metadata, ref) != checkpointEngineGitLegacy {
		t.Fatalf("converted node engine = %q (metadata %s)", checkpointEngineOf(metadata, ref), metadata)
	}
	if !strings.Contains(metadata, `"fileCount":2`) {
		t.Fatalf("conversion dropped the original readings: %s", metadata)
	}
	// 转换后的节点仍然能恢复代码，而且是"覆盖式"：快照里被 .gitignore 命中的文件也在树里
	// （转换用了 -f），工作区里多出来的文件不会被删。
	if err := os.Remove(filepath.Join(root, "a.txt")); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	writeCheckpointFile(t, filepath.Join(root, "extra.txt"), "extra\n")
	if err := server.restoreCheckpointCode(t.Context(), checkpointRestoreRequest{ProjectID: "checkpoint-project", ProjectPath: root, Runner: "windows-local", SnapshotDir: path, SnapshotRef: ref, Engine: checkpointEngineOf(metadata, ref)}); err != nil {
		t.Fatal(err)
	}
	if got := readCheckpointFile(t, filepath.Join(root, "a.txt")); got != "before\n" {
		t.Fatalf("converted snapshot did not restore a.txt: %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "extra.txt")); err != nil {
		t.Fatalf("overlay restore must not delete unrelated files: %v", err)
	}
}

// 从没回退过的会话里，每个新节点插入时都是 active —— 保留策略不能把这个当豁免，
// 否则它一个都不会删（真机上一个会话的节点行会无上限地长）。
func TestPruneCheckpointsAppliesEvenWhenEveryNodeIsActive(t *testing.T) {
	server := newTestServer(t)
	seedCheckpointConversation(t, server)
	base := time.Now().UTC().Add(-time.Duration(checkpointRetentionPerConversation+10) * time.Minute)
	for index := 0; index < checkpointRetentionPerConversation+5; index++ {
		if _, err := server.db.Exec(`insert into conversation_checkpoints (id,conversation_id,status,metadata,created_at) values (?,?, 'active','{}',?)`, fmt.Sprintf("live-%02d", index), "checkpoint-conversation", base.Add(time.Duration(index)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	server.pruneConversationCheckpoints(t.Context(), "checkpoint-conversation")
	var remaining int
	if err := server.db.QueryRow(`select count(*) from conversation_checkpoints where conversation_id=?`, "checkpoint-conversation").Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != checkpointRetentionPerConversation {
		t.Fatalf("remaining = %d, want %d（全是 active 也必须按份数裁）", remaining, checkpointRetentionPerConversation)
	}
	var deleted bool
	if err := server.db.QueryRow(`select exists(select 1 from conversation_checkpoints where id='live-00')`).Scan(&deleted); err != nil {
		t.Fatal(err)
	}
	if deleted {
		t.Fatal("最老的节点没有被裁掉：保留策略仍然没生效")
	}
	var kept int
	if err := server.db.QueryRow(`select count(*) from conversation_checkpoints where id in ('live-05','live-54')`).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != 2 {
		t.Fatal("最近的节点被误删")
	}
}
