package cloud

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// These tests cover the two halves of the mobile event stream that cannot be
// checked without PostgreSQL: the LISTEN/NOTIFY wake-up path and the drain
// query. They are skipped unless MILEVIA_TEST_DATABASE_URL is set, so they can
// be pointed at the deployment's own database.
//
// They deliberately do not call New(), because that runs the schema migration.
// The Server is built directly instead, and the listener is started through the
// same method production uses.
func newStreamRuntimeServer(t *testing.T) (*Server, string) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("MILEVIA_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("set MILEVIA_TEST_DATABASE_URL to run the mobile stream runtime tests")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to the test database: %v", err)
	}
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	instanceID := "test-stream-" + hex.EncodeToString(suffix)
	// DatabaseURL is set as well as the pool: the event listener opens its own
	// dedicated connection from that string, exactly as production does.
	server := &Server{db: pool, config: Config{DatabaseURL: dsn}, events: newEventBroker()}
	listenCtx, cancel := context.WithCancel(context.Background())
	server.listenCancel = cancel
	go server.runEventListener(listenCtx)
	t.Cleanup(func() {
		// The throwaway instance cascades to its events, so one delete undoes
		// everything this test wrote.
		_, _ = pool.Exec(context.Background(), `delete from cloud_instances where instance_id=$1`, instanceID)
		server.Close()
	})
	return server, instanceID
}

// insertStreamTestEvents writes a contiguous run of sequences for the throwaway
// instance in one statement.
func insertStreamTestEvents(t *testing.T, server *Server, instanceID string, from, to int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := server.db.Exec(ctx, `insert into cloud_instances(instance_id,status,last_seen_at) values($1,'offline',null) on conflict(instance_id) do nothing`, instanceID); err != nil {
		t.Fatalf("create throwaway instance: %v", err)
	}
	if _, err := server.db.Exec(ctx, `insert into cloud_events(event_id,instance_id,agent_sequence,type,task_id,task_run_id,payload,created_at) select $1 || '-event-' || g, $1, g, 'assistant.message', '', '', '{"content":"x"}'::jsonb, now() from generate_series($2::bigint,$3::bigint) g`, instanceID, from, to); err != nil {
		t.Fatalf("insert events %d..%d: %v", from, to, err)
	}
}

func drainInto(t *testing.T, server *Server, instanceID string, cursor *int64, sent map[int64]struct{}) []int64 {
	t.Helper()
	var delivered []int64
	ok, _ := server.drainInstanceEvents(context.Background(), instanceID, cursor, sent, func(event eventEnvelope) {
		delivered = append(delivered, event.AgentSequence)
	})
	if !ok {
		t.Fatal("drain reported a failure")
	}
	return delivered
}

// A committed notify must reach the dedicated listener and be published to a
// subscriber for exactly that instance; a notify from a rolled-back transaction
// must never be published, and another instance's notify must not leak.
func TestMobileStreamNotifyWakesOnlyForCommittedEventsOfThatInstance(t *testing.T) {
	server, instanceID := newStreamRuntimeServer(t)
	ctx := context.Background()

	notifications := server.events.subscribe(instanceID)
	defer server.events.unsubscribe(notifications)

	notify := func(payload string, commit bool) {
		t.Helper()
		tx, err := server.db.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.Exec(ctx, `select pg_notify($1,$2)`, eventNotifyChannel, payload); err != nil {
			t.Fatalf("notify: %v", err)
		}
		if commit {
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("commit: %v", err)
			}
			return
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("rollback: %v", err)
		}
	}

	// The listener connects in the background, so retry until a wake-up lands
	// rather than assuming it is subscribed already.
	woke := false
	for attempt := 0; attempt < 40 && !woke; attempt++ {
		notify(instanceID, true)
		select {
		case <-notifications:
			woke = true
		case <-time.After(250 * time.Millisecond):
		}
	}
	if !woke {
		t.Fatal("a committed pg_notify never reached the stream subscriber")
	}

	quiet := func(window time.Duration) bool {
		select {
		case <-notifications:
			return false
		case <-time.After(window):
			return true
		}
	}
	// Drop anything still queued from the retry loop above.
	for {
		select {
		case <-notifications:
			continue
		default:
		}
		break
	}

	notify(instanceID, false)
	if !quiet(500 * time.Millisecond) {
		t.Fatal("a notification from a rolled-back transaction was published")
	}

	notify("some-other-instance", true)
	if !quiet(500 * time.Millisecond) {
		t.Fatal("another instance's notification was published to this instance's subscriber")
	}
}

// The drain must reach the newest event even when the backlog is far larger
// than one batch. The single retention-wide query this replaced scanned
// ascending, so a backlog larger than a batch filled the whole result with
// already-sent rows and the newest events became unreachable — which is what
// stalled live updates once an instance passed 100 events.
func TestDrainInstanceEventsReachesTheNewestEvent(t *testing.T) {
	server, instanceID := newStreamRuntimeServer(t)
	insertStreamTestEvents(t, server, instanceID, 1, 150)

	cursor := int64(0)
	sent := map[int64]struct{}{}
	delivered := drainInto(t, server, instanceID, &cursor, sent)

	if cursor != 150 {
		t.Fatalf("cursor=%d, want 150: the newest event was not reached", cursor)
	}
	if len(delivered) != 150 {
		t.Fatalf("delivered %d events, want 150", len(delivered))
	}
	if delivered[len(delivered)-1] != 150 {
		t.Fatalf("last delivered sequence=%d, want 150", delivered[len(delivered)-1])
	}

	// Nothing new: the drain must be a no-op rather than replaying the window.
	if again := drainInto(t, server, instanceID, &cursor, sent); len(again) != 0 {
		t.Fatalf("an idle drain re-delivered %d events", len(again))
	}

	// A newly stored event is delivered exactly once.
	insertStreamTestEvents(t, server, instanceID, 151, 151)
	fresh := drainInto(t, server, instanceID, &cursor, sent)
	if len(fresh) != 1 || fresh[0] != 151 {
		t.Fatalf("delivered %v after a new event, want [151]", fresh)
	}
	if again := drainInto(t, server, instanceID, &cursor, sent); len(again) != 0 {
		t.Fatalf("the new event was delivered twice: %v", again)
	}
}

// A cursor that is behind by more than the per-drain cap must still converge,
// and the backfill must not re-send rows that were already delivered.
func TestDrainInstanceEventsCatchesUpPastThePerDrainCap(t *testing.T) {
	server, instanceID := newStreamRuntimeServer(t)
	insertStreamTestEvents(t, server, instanceID, 1, 1000)

	cursor := int64(0)
	sent := map[int64]struct{}{}
	total := 0
	for pass := 0; pass < 10 && cursor < 1000; pass++ {
		var delivered []int64
		ok, more := server.drainInstanceEvents(context.Background(), instanceID, &cursor, sent, func(event eventEnvelope) {
			delivered = append(delivered, event.AgentSequence)
		})
		if !ok {
			t.Fatal("drain reported a failure")
		}
		// A pass that stops at the cap has to say so, otherwise the handler
		// waits for a wake-up that is not coming.
		if pass < 1 && !more {
			t.Fatalf("pass %d stopped at the cap without reporting more work pending", pass)
		}
		total += len(delivered)
		if len(delivered) > cloudEventCatchUpPerDrain {
			t.Fatalf("pass %d delivered %d events, over the %d cap", pass, len(delivered), cloudEventCatchUpPerDrain)
		}
	}
	if cursor != 1000 {
		t.Fatalf("cursor=%d after catch-up, want 1000", cursor)
	}
	if total != 1000 {
		t.Fatalf("delivered %d events in total, want exactly 1000 (no duplicates)", total)
	}
	if again := drainInto(t, server, instanceID, &cursor, sent); len(again) != 0 {
		t.Fatalf("an idle drain re-delivered %d events", len(again))
	}
}

// An upload that lands behind the cursor must still be delivered, which is what
// the backfill pass exists for.
func TestDrainInstanceEventsBackfillsAnOutOfOrderArrival(t *testing.T) {
	server, instanceID := newStreamRuntimeServer(t)
	// Leave a gap, as if the upload of sequence 11 lost a race to sequence 12.
	insertStreamTestEvents(t, server, instanceID, 1, 10)
	insertStreamTestEvents(t, server, instanceID, 12, 20)

	cursor := int64(0)
	sent := map[int64]struct{}{}
	if delivered := drainInto(t, server, instanceID, &cursor, sent); len(delivered) != 19 {
		t.Fatalf("delivered %d events, want 19", len(delivered))
	}
	if cursor != 20 {
		t.Fatalf("cursor=%d, want 20", cursor)
	}

	// The late arrival lands behind the cursor and must still be delivered.
	insertStreamTestEvents(t, server, instanceID, 11, 11)
	filled := drainInto(t, server, instanceID, &cursor, sent)
	if len(filled) != 1 || filled[0] != 11 {
		t.Fatalf("backfill delivered %v, want [11]", filled)
	}
	if cursor != 20 {
		t.Fatalf("cursor=%d, want 20: a backfilled event must not move the cursor backwards", cursor)
	}
	if again := drainInto(t, server, instanceID, &cursor, sent); len(again) != 0 {
		t.Fatalf("the backfilled event was delivered twice: %v", again)
	}
}
