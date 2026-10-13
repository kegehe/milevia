package app

import (
	"context"
	"testing"
	"time"
)

func seedHandoffConversation(t *testing.T, server *Server, id, projectID, status string, current bool) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,agent_id,agent_session_id,agent_runtime_id,execution_policy,status,permission_mode,title,last_activity_at,claude_initialized,agent_initialized,is_current,created_at) values (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, projectID, "session-"+id, "codex", "session-"+id, "local", "read_only", status, "read_only", id, now, 0, 0, boolToInt(current), now); err != nil {
		t.Fatalf("seed conversation %s: %v", id, err)
	}
}

func seedHandoffProject(t *testing.T, server *Server, id string) {
	t.Helper()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values (?,?,?,?,?,?,?)`, id, id, t.TempDir(), "local", "", 1, time.Now().UTC()); err != nil {
		t.Fatalf("seed project: %v", err)
	}
}

func TestMigrateConversationHandoffsAllowsInitialRecord(t *testing.T) {
	server := newTestServer(t)
	if _, err := server.db.Exec(`drop index if exists conversation_handoffs_source_status;
drop table conversation_handoffs;
create table conversation_handoffs (
 id text primary key,
 source_conversation_id text not null references conversations(id) on delete cascade,
 target_conversation_id text not null default '' references conversations(id) on delete set null,
 source_run_id text not null default '', target_run_id text not null default '',
 status text not null check(status in ('summarizing','initializing','completed','failed')),
 summary text not null default '', error_message text not null default '',
 created_at datetime not null, completed_at datetime
);
create index conversation_handoffs_source_status on conversation_handoffs(source_conversation_id,status);`); err != nil {
		t.Fatalf("create legacy handoff schema: %v", err)
	}
	if err := server.migrateConversationHandoffs(context.Background()); err != nil {
		t.Fatalf("migrate legacy handoff schema: %v", err)
	}
	seedHandoffProject(t, server, "handoff-project")
	seedHandoffConversation(t, server, "handoff-source", "handoff-project", "idle", true)
	if _, err := server.db.Exec(`insert into conversation_handoffs (id,source_conversation_id,status,created_at) values (?,?,?,?)`, "handoff", "handoff-source", "summarizing", time.Now().UTC()); err != nil {
		t.Fatalf("insert initial handoff after migration: %v", err)
	}
	var targetID *string
	if err := server.db.QueryRow(`select target_conversation_id from conversation_handoffs where id='handoff'`).Scan(&targetID); err != nil {
		t.Fatalf("read migrated handoff: %v", err)
	}
	if targetID != nil {
		t.Fatalf("initial target conversation id = %q, want NULL", *targetID)
	}
}

func TestCompleteConversationHandoffPreservesNewerCurrentSelection(t *testing.T) {
	server := newTestServer(t)
	seedHandoffProject(t, server, "handoff-project")
	seedHandoffConversation(t, server, "source", "handoff-project", "idle", false)
	seedHandoffConversation(t, server, "target", "handoff-project", "idle", false)
	seedHandoffConversation(t, server, "selected", "handoff-project", "idle", true)
	if _, err := server.db.Exec(`insert into conversation_handoffs (id,source_conversation_id,target_conversation_id,status,created_at) values (?,?,?,?,?)`, "handoff", "source", "target", "initializing", time.Now().UTC()); err != nil {
		t.Fatalf("seed handoff: %v", err)
	}
	if err := server.completeConversationHandoff(context.Background(), "handoff", "source", "target"); err != nil {
		t.Fatalf("complete handoff: %v", err)
	}
	var sourceStatus string
	var targetCurrent, selectedCurrent bool
	if err := server.db.QueryRow(`select status from conversations where id='source'`).Scan(&sourceStatus); err != nil {
		t.Fatalf("read source: %v", err)
	}
	if err := server.db.QueryRow(`select is_current from conversations where id='target'`).Scan(&targetCurrent); err != nil {
		t.Fatalf("read target: %v", err)
	}
	if err := server.db.QueryRow(`select is_current from conversations where id='selected'`).Scan(&selectedCurrent); err != nil {
		t.Fatalf("read selected: %v", err)
	}
	if sourceStatus != "archived" || targetCurrent || !selectedCurrent {
		t.Fatalf("unexpected completion state: source=%q targetCurrent=%t selectedCurrent=%t", sourceStatus, targetCurrent, selectedCurrent)
	}
	var handoffStatus string
	if err := server.db.QueryRow(`select status from conversation_handoffs where id='handoff'`).Scan(&handoffStatus); err != nil {
		t.Fatalf("read handoff: %v", err)
	}
	if handoffStatus != "completed" {
		t.Fatalf("handoff status=%q, want completed", handoffStatus)
	}
}

func TestStartMessageRejectsOrdinaryTurnDuringHandoff(t *testing.T) {
	server := newTestServer(t)
	seedHandoffProject(t, server, "handoff-project")
	seedHandoffConversation(t, server, "source", "handoff-project", "idle", true)
	if _, err := server.db.Exec(`insert into conversation_handoffs (id,source_conversation_id,status,created_at) values (?,?,?,?)`, "handoff", "source", "summarizing", time.Now().UTC()); err != nil {
		t.Fatalf("seed handoff: %v", err)
	}
	_, _, _, status, err := server.startMessage(context.Background(), "source", "later user work", "", nil)
	if status != 409 || err == nil || err.Error() != "context handoff is in progress" {
		t.Fatalf("startMessage status=%d err=%v, want 409 context handoff conflict", status, err)
	}
}

func TestConversationHandoffInProgress(t *testing.T) {
	server := newTestServer(t)
	seedHandoffProject(t, server, "handoff-project")
	seedHandoffConversation(t, server, "source", "handoff-project", "idle", true)
	if _, err := server.db.Exec(`insert into conversation_handoffs (id,source_conversation_id,status,created_at) values (?,?,?,?)`, "active", "source", "initializing", time.Now().UTC()); err != nil {
		t.Fatalf("seed active handoff: %v", err)
	}
	active, err := server.conversationHandoffInProgress(context.Background(), "source")
	if err != nil || !active {
		t.Fatalf("active handoff = %t, err = %v; want true, nil", active, err)
	}
	active, err = server.conversationHandoffInProgress(context.Background(), "other")
	if err != nil || active {
		t.Fatalf("other handoff = %t, err = %v; want false, nil", active, err)
	}
}
