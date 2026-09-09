package app

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// TestEnsureConversationDeleteIntegrity verifies the background repair that
// makes conversation deletion fast: orphaned events (whose conversation no
// longer exists) are swept, and the events(run_id) index is created so a run
// cascade no longer scans the whole events table.
func TestEnsureConversationDeleteIntegrity(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	projectID := "project"
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values (?,?,?,'windows-local','main',1,?)`, projectID, "project", t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	for _, c := range []struct {
		id        string
		isCurrent int
	}{
		{"conversation-keep", 1}, {"conversation-orphan", 0},
	} {
		if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,agent_id,status,title,is_current,created_at) values (?,?,?,'claude-code','idle','x',?,?)`, c.id, projectID, c.id+"-session", c.isCurrent, now); err != nil {
			t.Fatalf("insert conversation %s: %v", c.id, err)
		}
	}
	for _, r := range []struct {
		id string
	}{
		{"run-keep"}, {"run-orphan"},
	} {
		conversation := "conversation-keep"
		if r.id == "run-orphan" {
			conversation = "conversation-orphan"
		}
		if _, err := server.db.Exec(`insert into runs (id,conversation_id,status,created_at) values (?,?,'completed',?)`, r.id, conversation, now); err != nil {
			t.Fatalf("insert run %s: %v", r.id, err)
		}
	}
	for _, e := range []struct {
		id   string
		conv string
		run  string
	}{
		{"event-keep", "conversation-keep", "run-keep"},
		{"event-orphan", "conversation-orphan", "run-orphan"},
	} {
		if _, err := server.db.Exec(`insert into events (id,conversation_id,run_id,type,payload,created_at) values (?,?,?,'assistant','{}',?)`, e.id, e.conv, e.run, now); err != nil {
			t.Fatalf("insert event %s: %v", e.id, err)
		}
	}

	// Simulate an orphan left behind by a deletion that ran before foreign-key
	// cascades were always enabled: drop the conversation (and its run) with
	// foreign_keys off so the event rows survive.
	if _, err := server.db.Exec(`pragma foreign_keys=off`); err != nil {
		t.Fatalf("disable foreign keys: %v", err)
	}
	if _, err := server.db.Exec(`delete from conversations where id='conversation-orphan'`); err != nil {
		t.Fatalf("delete orphan conversation: %v", err)
	}
	if _, err := server.db.Exec(`delete from runs where id='run-orphan'`); err != nil {
		t.Fatalf("delete orphan run: %v", err)
	}
	if _, err := server.db.Exec(`pragma foreign_keys=on`); err != nil {
		t.Fatalf("re-enable foreign keys: %v", err)
	}

	if err := server.ensureConversationDeleteIntegrity(context.Background()); err != nil {
		t.Fatalf("ensureConversationDeleteIntegrity: %v", err)
	}

	// The orphaned event is gone; the live conversation's event is retained.
	assertEventCount(t, server, "event-orphan", 0)
	assertEventCount(t, server, "event-keep", 1)

	// The events(run_id) index now exists.
	if !eventsRunIndexExists(t, server) {
		t.Fatal("events_run_id index was not created")
	}

	// The marker prevents a second sweep from running (idempotent and cheap).
	if err := server.ensureConversationDeleteIntegrity(context.Background()); err != nil {
		t.Fatalf("second ensureConversationDeleteIntegrity: %v", err)
	}
	assertEventCount(t, server, "event-keep", 1)
}

func assertEventCount(t *testing.T, server *Server, id string, want int) {
	t.Helper()
	var count int
	if err := server.db.QueryRow(`select count(*) from events where id=?`, id).Scan(&count); err != nil {
		t.Fatalf("count event %s: %v", id, err)
	}
	if count != want {
		t.Fatalf("event %s count=%d want %d", id, count, want)
	}
}

func eventsRunIndexExists(t *testing.T, server *Server) bool {
	t.Helper()
	var name string
	err := server.db.QueryRow(`select name from sqlite_master where type='index' and name=?`, eventsRunIDIndex).Scan(&name)
	if err == sql.ErrNoRows {
		return false
	}
	if err != nil {
		t.Fatalf("look up events_run_id index: %v", err)
	}
	return name == eventsRunIDIndex
}
