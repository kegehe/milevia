package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"
)

// eventsRunIDIndex is the index that lets a conversation deletion cascade into
// runs without scanning the whole events table. `events.run_id` references
// `runs(id)` ON DELETE CASCADE, but for a long time the schema carried no index
// on that column. Deleting one conversation therefore deleted every matching run
// one by one, and each run deletion ran `DELETE FROM events WHERE run_id=?` —
// a full events-table scan when the column is unindexed. On a database whose
// events table has grown large (this one had ~7.6M rows / ~2 GB of payload) a
// single scan takes tens of seconds, so the DELETE endpoint could not answer
// before the web client's 15 s timeout, and the client abort cancelled the
// transaction, rolling the deletion back.
const eventsRunIDIndex = "events_run_id"

// orphanedEventsCleanupKey records that orphaned events have been swept at
// least once. The sweep only needs to run on databases that accumulated orphans
// before foreign-key cascades were always enabled; the marker prevents the
// (expensive) orphan scan from running again on every startup.
const orphanedEventsCleanupKey = "events_orphan_cleanup_v1"

// ensureConversationDeleteIntegrity repairs the two conditions that made single
// conversation deletion degrade into multi-minute full-table scans:
//
//  1. It removes event rows whose conversation no longer exists (one-time,
//     gated by app_metadata). These are leftovers from eras when cascades were
//     disabled; they can be millions of rows that bloat the events table. Sweeping
//     them first keeps the index build short.
//  2. It creates the events(run_id) index if it is still missing (idempotent) —
//     this is the step that actually fixes deletion latency.
//
// The orphan sweep only needs to run on databases that accumulated orphans
// before foreign-key cascades were always enabled; the app_metadata marker
// prevents the (expensive) orphan scan from rerunning on every startup.
func (s *Server) ensureConversationDeleteIntegrity(ctx context.Context) error {
	// Sweep orphaned events first when this has not run yet: dropping millions of
	// dead rows before building the index keeps the CREATE INDEX pass short. The
	// marker prevents the orphan scan from rerunning on every startup once the
	// table is healthy.
	var marker int
	err := s.db.QueryRowContext(ctx, `select 1 from app_metadata where key=?`, orphanedEventsCleanupKey).Scan(&marker)
	if err == nil {
		// Already swept; only the index may still be missing.
	} else if errors.Is(err, sql.ErrNoRows) {
		removed, cleanupErr := s.cleanOrphanedConversationEvents(ctx)
		if cleanupErr != nil {
			return cleanupErr
		}
		if removed > 0 {
			log.Printf("[maintenance] removed %d orphaned conversation events", removed)
		}
		if _, markerErr := s.db.ExecContext(ctx, `insert or ignore into app_metadata (key,value) values (?,?)`, orphanedEventsCleanupKey, timeNowForMetadata()); markerErr != nil {
			return fmt.Errorf("record orphaned-event cleanup: %w", markerErr)
		}
	} else {
		return fmt.Errorf("check orphaned-event cleanup marker: %w", err)
	}

	if _, err := s.db.ExecContext(ctx, `create index if not exists `+eventsRunIDIndex+` on events(run_id)`); err != nil {
		return fmt.Errorf("create events(run_id) index: %w", err)
	}
	return nil
}

// cleanOrphanedConversationEvents deletes every event whose conversation row no
// longer exists. Each batch is scoped by a single conversation id so it hits the
// events(conversation_id, created_at) index instead of one unbounded statement.
// Rows are collected and the cursor closed before any delete runs: the server's
// SQLite pool holds a single connection, so iterating a live row set while
// writing would deadlock.
func (s *Server) cleanOrphanedConversationEvents(ctx context.Context) (int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		select e.conversation_id
		from events e
		where not exists (select 1 from conversations c where c.id = e.conversation_id)
		group by e.conversation_id`)
	if err != nil {
		return 0, fmt.Errorf("find orphaned conversation events: %w", err)
	}
	orphanIDs := []string{}
	for rows.Next() {
		var conversationID string
		if err := rows.Scan(&conversationID); err != nil {
			rows.Close()
			return 0, fmt.Errorf("read orphaned conversation id: %w", err)
		}
		orphanIDs = append(orphanIDs, conversationID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}

	var removed int64
	for _, conversationID := range orphanIDs {
		result, err := s.db.ExecContext(ctx, `delete from events where conversation_id=?`, conversationID)
		if err != nil {
			return removed, fmt.Errorf("delete orphaned events for conversation %s: %w", conversationID, err)
		}
		if affected, err := result.RowsAffected(); err == nil {
			removed += affected
		}
	}
	return removed, nil
}

func timeNowForMetadata() string {
	return time.Now().UTC().Format("2006-01-02 15:04:05.999999999Z07:00")
}
