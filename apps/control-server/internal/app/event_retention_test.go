package app

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// seedConversation 造一个项目 + 会话 + run 的最小骨架，返回会话 id。
func seedConversation(t *testing.T, server *Server, projectID, conversationID string) string {
	t.Helper()
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values (?,?,?,'windows-local','main',1,?)`, projectID, projectID, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,agent_id,status,title,is_current,created_at) values (?,?,?,'claude-code','idle','x',1,?)`, conversationID, projectID, conversationID+"-session", now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	runID := conversationID + "-run"
	if _, err := server.db.Exec(`insert into runs (id,conversation_id,status,created_at) values (?,?,'completed',?)`, runID, conversationID, now); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	return runID
}

// seedEvents 往会话里插 n 条指定类型的事件，created_at 依次递增（用于判定"最旧的先删"）。
func seedEvents(t *testing.T, server *Server, conversationID, runID, eventType, idPrefix string, n int, base time.Time) {
	t.Helper()
	tx, err := server.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	stmt, err := tx.Prepare(`insert into events (id,conversation_id,run_id,type,payload,created_at) values (?,?,?,?,?,?)`)
	if err != nil {
		tx.Rollback()
		t.Fatalf("prepare: %v", err)
	}
	for i := 0; i < n; i++ {
		if _, err := stmt.Exec(idPrefix+padIndex(i), conversationID, runID, eventType, "{}", base.Add(time.Duration(i)*time.Millisecond)); err != nil {
			stmt.Close()
			tx.Rollback()
			t.Fatalf("insert event %d: %v", i, err)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func padIndex(i int) string {
	return fmt.Sprintf("%06d", i)
}

func countEvents(t *testing.T, server *Server, conversationID string) int {
	t.Helper()
	var n int
	if err := server.db.QueryRow(`select count(*) from events where conversation_id=?`, conversationID).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

func countEventsOfType(t *testing.T, server *Server, conversationID, eventType string) int {
	t.Helper()
	var n int
	if err := server.db.QueryRow(`select count(*) from events where conversation_id=? and type=?`, conversationID, eventType).Scan(&n); err != nil {
		t.Fatalf("count events of type: %v", err)
	}
	return n
}

// 核心语义：过程事件先被削到上限，会话内容事件原样保留 —— 裁掉过程数据不能让用户翻历史时
// 看到残缺的对话。
func TestPruneConversationEventsTrimsProcessEventsFirst(t *testing.T) {
	server := newTestServer(t)
	const conversationID = "conversation-process"
	runID := seedConversation(t, server, "project-process", conversationID)
	base := time.Now().UTC().Add(-time.Hour)

	seedEvents(t, server, conversationID, runID, "stream_event", "proc-", 60, base)
	seedEvents(t, server, conversationID, runID, "tool_progress", "tool-", 20, base.Add(time.Minute))
	seedEvents(t, server, conversationID, runID, "usage.updated", "usage-", 20, base.Add(2*time.Minute))
	seedEvents(t, server, conversationID, runID, "assistant", "assistant-", 40, base.Add(3*time.Minute))
	seedEvents(t, server, conversationID, runID, "user", "user-", 10, base.Add(4*time.Minute))

	if _, err := server.pruneConversationEventsWithLimits(context.Background(), 50, 1000); err != nil {
		t.Fatalf("pruneConversationEvents: %v", err)
	}

	process := countEventsOfType(t, server, conversationID, "stream_event") +
		countEventsOfType(t, server, conversationID, "tool_progress") +
		countEventsOfType(t, server, conversationID, "usage.updated")
	if process != 50 {
		t.Fatalf("process events = %d, want 50", process)
	}
	if got := countEventsOfType(t, server, conversationID, "assistant"); got != 40 {
		t.Fatalf("assistant events = %d, want 40 (content must survive)", got)
	}
	if got := countEventsOfType(t, server, conversationID, "user"); got != 10 {
		t.Fatalf("user events = %d, want 10 (content must survive)", got)
	}
}

// 保留的是**最新**的过程事件，不是最早的。
func TestPruneConversationEventsKeepsNewestProcessEvents(t *testing.T) {
	server := newTestServer(t)
	const conversationID = "conversation-newest"
	runID := seedConversation(t, server, "project-newest", conversationID)
	base := time.Now().UTC().Add(-time.Hour)
	seedEvents(t, server, conversationID, runID, "stream_event", "proc-", 30, base)

	if _, err := server.pruneConversationEventsWithLimits(context.Background(), 10, 1000); err != nil {
		t.Fatalf("pruneConversationEvents: %v", err)
	}
	var oldestKept string
	if err := server.db.QueryRow(`select id from events where conversation_id=? order by created_at asc limit 1`, conversationID).Scan(&oldestKept); err != nil {
		t.Fatalf("read oldest: %v", err)
	}
	if want := "proc-" + padIndex(20); oldestKept != want {
		t.Fatalf("oldest kept event = %q, want %q", oldestKept, want)
	}
}

// 过程事件清完仍超总量兜底时，才动内容事件。
func TestPruneConversationEventsFallsBackToTotalLimit(t *testing.T) {
	server := newTestServer(t)
	const conversationID = "conversation-total"
	runID := seedConversation(t, server, "project-total", conversationID)
	base := time.Now().UTC().Add(-time.Hour)
	seedEvents(t, server, conversationID, runID, "assistant", "assistant-", 80, base)

	if _, err := server.pruneConversationEventsWithLimits(context.Background(), 50, 30); err != nil {
		t.Fatalf("pruneConversationEvents: %v", err)
	}
	if got := countEvents(t, server, conversationID); got != 30 {
		t.Fatalf("total events = %d, want 30", got)
	}
}

// 未超限的会话不受影响；重复执行幂等。
func TestPruneConversationEventsLeavesSmallConversationsAlone(t *testing.T) {
	server := newTestServer(t)
	const conversationID = "conversation-small"
	runID := seedConversation(t, server, "project-small", conversationID)
	seedEvents(t, server, conversationID, runID, "stream_event", "proc-", 20, time.Now().UTC().Add(-time.Hour))
	seedEvents(t, server, conversationID, runID, "assistant", "assistant-", 5, time.Now().UTC())

	if _, err := server.pruneConversationEventsWithLimits(context.Background(), 50, 1000); err != nil {
		t.Fatalf("pruneConversationEvents: %v", err)
	}
	if got := countEvents(t, server, conversationID); got != 25 {
		t.Fatalf("events = %d, want 25 (nothing should be removed)", got)
	}
	if _, err := server.pruneConversationEventsWithLimits(context.Background(), 50, 1000); err != nil {
		t.Fatalf("second prune: %v", err)
	}
	if got := countEvents(t, server, conversationID); got != 25 {
		t.Fatalf("events after second prune = %d, want 25 (idempotent)", got)
	}
}

// outbox 里有引用**不**构成豁免：outbox 行自带 payload，中继只读 outbox，没有任何地方
// 按 id 回查 events。真机上 outbox 有近 6 万行，把引用的过程事件一并豁免掉会让保留策略
// 名存实亡，所以这条要有断言钉住。
func TestPruneConversationEventsDoesNotSpareOutboxEvents(t *testing.T) {
	server := newTestServer(t)
	const conversationID = "conversation-outbox"
	runID := seedConversation(t, server, "project-outbox", conversationID)
	base := time.Now().UTC().Add(-time.Hour)
	seedEvents(t, server, conversationID, runID, "stream_event", "proc-", 30, base)

	oldestID := "proc-" + padIndex(0) // 最旧的一条，正常一定会被裁掉
	if _, err := server.db.Exec(`insert into remote_outbox (event_id,agent_sequence,type,payload,created_at) values (?,?,?,?,?)`,
		oldestID, 1, "stream_event", "{}", base); err != nil {
		t.Fatalf("insert remote_outbox: %v", err)
	}

	if _, err := server.pruneConversationEventsWithLimits(context.Background(), 10, 1000); err != nil {
		t.Fatalf("pruneConversationEvents: %v", err)
	}
	var exists int
	if err := server.db.QueryRow(`select count(*) from events where id=?`, oldestID).Scan(&exists); err != nil {
		t.Fatalf("count pending event: %v", err)
	}
	if exists != 0 {
		t.Fatal("outbox-referenced event survived pruning; the retention policy would be defeated on real databases")
	}
	if got := countEvents(t, server, conversationID); got != 10 {
		t.Fatalf("events = %d, want 10", got)
	}
}

// 返回值是实际删除条数，供调用方决定要不要顺手回收空间。
func TestPruneConversationEventsReportsRemovedCount(t *testing.T) {
	server := newTestServer(t)
	const conversationID = "conversation-count"
	runID := seedConversation(t, server, "project-count", conversationID)
	seedEvents(t, server, conversationID, runID, "stream_event", "proc-", 30, time.Now().UTC().Add(-time.Hour))

	removed, err := server.pruneConversationEventsWithLimits(context.Background(), 10, 1000)
	if err != nil {
		t.Fatalf("pruneConversationEvents: %v", err)
	}
	if removed != 20 {
		t.Fatalf("removed = %d, want 20", removed)
	}
	// 幂等：再跑一次没有可删的。
	removed, err = server.pruneConversationEventsWithLimits(context.Background(), 10, 1000)
	if err != nil {
		t.Fatalf("second prune: %v", err)
	}
	if removed != 0 {
		t.Fatalf("second prune removed = %d, want 0", removed)
	}
}

// auto_vacuum 已是 INCREMENTAL 时，裁剪之后应顺手把小额空洞还回去 —— 否则应用连续运行
// 很多天，文件会一直长到下次重启才回收。
func TestReclaimAfterPruneShrinksFreelist(t *testing.T) {
	server := newTestServer(t)
	// 真实部署里这一步由启动期的一次性回收完成（切模式必须伴随一次 VACUUM）。
	if _, err := server.db.Exec(`pragma auto_vacuum=incremental`); err != nil {
		t.Fatalf("set auto_vacuum: %v", err)
	}
	if _, err := server.db.Exec(`vacuum`); err != nil {
		t.Fatalf("vacuum: %v", err)
	}

	const conversationID = "conversation-reclaim"
	runID := seedConversation(t, server, "project-reclaim", conversationID)
	seedEvents(t, server, conversationID, runID, "stream_event", "proc-", 3000, time.Now().UTC().Add(-time.Hour))
	removed, err := server.pruneConversationEventsWithLimits(context.Background(), 1000, 100000)
	if err != nil {
		t.Fatalf("pruneConversationEvents: %v", err)
	}
	if removed < eventsReclaimMinPrunedRows {
		t.Fatalf("removed = %d, need at least %d to trigger a reclaim", removed, eventsReclaimMinPrunedRows)
	}

	before := readFreelist(t, server)
	if before <= 0 {
		t.Fatalf("prune left no freelist pages: %d", before)
	}
	server.reclaimAfterPrune(context.Background(), removed)
	if after := readFreelist(t, server); after >= before {
		t.Fatalf("freelist = %d after reclaim, want < %d", after, before)
	}
}

// 模式还是 0（用户没经历过一次完整整理）时不做无谓的工作。
func TestReclaimAfterPruneSkipsWhenNotIncremental(t *testing.T) {
	server := newTestServer(t)
	if mode := readAutoVacuum(t, server); mode != 0 {
		t.Fatalf("auto_vacuum = %d, want 0", mode)
	}
	server.reclaimAfterPrune(context.Background(), eventsReclaimMinPrunedRows*10)
	if mode := readAutoVacuum(t, server); mode != 0 {
		t.Fatalf("auto_vacuum = %d, want 0 (must not switch modes here)", mode)
	}
}
