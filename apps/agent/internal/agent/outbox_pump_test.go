package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// newOutboxPumpAgent wires an Agent to a fake local control server that always
// answers /api/remote/outbox with the same batch.
//
// That is what the real server does between forwarding an event and the cloud
// acknowledging it: rows are only deleted by /api/remote/outbox/ack, so while
// the acknowledgement is in flight the outbox keeps handing back the same rows,
// and the long poll returns them at once instead of holding the request.
func newOutboxPumpAgent(t *testing.T, batch []outboxItem, requests *int32) *Agent {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(requests, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(batch)
	}))
	t.Cleanup(server.Close)
	return &Agent{config: Config{LocalURL: server.URL}, client: &http.Client{Timeout: 5 * time.Second}}
}

// A batch that is still awaiting the cloud's acknowledgement must not be
// re-read and re-sent in a tight loop. Before the long poll, a fixed 200ms
// ticker bounded this; with the request held open, the only thing standing
// between an unacknowledged row and a hot loop is the pacing in syncOutbox.
func TestOutboxPumpPacesWhileBatchAwaitsAck(t *testing.T) {
	var requests int32
	batch := []outboxItem{{
		EventID:       "event-1",
		AgentSequence: 1,
		Type:          "assistant.message",
		Payload:       json.RawMessage(`{"content":"hi"}`),
		CreatedAt:     time.Now().UTC(),
	}}
	agent := newOutboxPumpAgent(t, batch, &requests)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writeCh := make(chan any, 64)
	// Drain like the connection writer does; otherwise a full channel would
	// throttle the loop and hide the problem.
	go func() {
		for range writeCh {
		}
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		agent.runOutboxPump(ctx, writeCh)
	}()

	const window = 400 * time.Millisecond
	time.Sleep(window)
	cancel()
	<-done

	// One read per ack grace period is the intent; a tight loop would issue
	// thousands of reads in this window.
	if got := atomic.LoadInt32(&requests); got > 12 {
		t.Fatalf("the pump issued %d local reads in %s while a batch awaited its ack; it is spinning", got, window)
	}
	if atomic.LoadInt32(&requests) == 0 {
		t.Fatal("the pump never read the outbox")
	}
}

// An idle long poll must not spin either: a server that answers empty instantly
// (one too old to know the wait parameter) has to be paced.
func TestOutboxPumpPacesOnInstantEmptyReads(t *testing.T) {
	var requests int32
	agent := newOutboxPumpAgent(t, []outboxItem{}, &requests)

	ctx, cancel := context.WithCancel(context.Background())
	writeCh := make(chan any, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		agent.runOutboxPump(ctx, writeCh)
	}()

	const window = 400 * time.Millisecond
	time.Sleep(window)
	cancel()
	<-done

	if got := atomic.LoadInt32(&requests); got > 12 {
		t.Fatalf("the pump issued %d local reads in %s against an instantly-responding server; it is spinning", got, window)
	}
	if atomic.LoadInt32(&requests) == 0 {
		t.Fatal("the pump never read the outbox")
	}
}

// Forwarding must still happen, and stop promptly when the connection goes
// away.
func TestOutboxPumpForwardsAndStopsOnCancel(t *testing.T) {
	var requests int32
	batch := []outboxItem{{
		EventID:       "event-1",
		AgentSequence: 1,
		Type:          "assistant.message",
		Payload:       json.RawMessage(`{"content":"hi"}`),
		CreatedAt:     time.Now().UTC(),
	}}
	agent := newOutboxPumpAgent(t, batch, &requests)

	ctx, cancel := context.WithCancel(context.Background())
	writeCh := make(chan any, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		agent.runOutboxPump(ctx, writeCh)
	}()

	select {
	case message := <-writeCh:
		envelope, ok := message.(map[string]any)
		if !ok {
			t.Fatalf("forwarded %T, want the relay envelope", message)
		}
		if envelope["eventId"] != "event-1" {
			t.Fatalf("forwarded %v, want event-1", envelope["eventId"])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the pump never forwarded the queued event")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the pump did not stop after the connection was cancelled")
	}
}
