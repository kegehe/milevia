package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newRemoteOutboxTestServer(t *testing.T) *Server {
	t.Helper()
	db := newRemoteTestDB(t)
	t.Cleanup(func() { db.Close() })
	s := &Server{db: db, config: Config{RemoteCloudURL: "https://cloud.example.com", RemoteCloudToken: "token"}}
	if err := s.migrateRemoteControl(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

// Without wait the endpoint must stay a plain read, so an Agent that does not
// know about long polling keeps working unchanged.
func TestRemoteOutboxWithoutWaitReturnsImmediately(t *testing.T) {
	s := newRemoteOutboxTestServer(t)

	start := time.Now()
	response := httptest.NewRecorder()
	s.remoteOutbox(response, httptest.NewRequest(http.MethodGet, "/api/remote/outbox?limit=100", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("a waitless read took %s; it must not hold the request", elapsed)
	}
}

// Queued data must be returned at once rather than after the whole window.
func TestRemoteOutboxWaitReturnsImmediatelyWhenDataIsQueued(t *testing.T) {
	s := newRemoteOutboxTestServer(t)
	if err := s.enqueueRemoteEvent(context.Background(), "event-1", "", "run-1", "assistant.message", []byte(`{"content":"hi"}`), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	response := httptest.NewRecorder()
	s.remoteOutbox(response, httptest.NewRequest(http.MethodGet, "/api/remote/outbox?wait=25", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("a read with data queued took %s", elapsed)
	}
	var items []remoteOutboxItem
	if err := json.Unmarshal(response.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items=%d want 1", len(items))
	}
}

// The whole point of the change: an empty outbox holds the request, and the
// next enqueue wakes it immediately instead of leaving the Agent to discover
// the row on a poll tick.
func TestRemoteOutboxWaitIsWokenByNewEvent(t *testing.T) {
	s := newRemoteOutboxTestServer(t)

	returned := make(chan time.Time, 1)
	go func() {
		response := httptest.NewRecorder()
		s.remoteOutbox(response, httptest.NewRequest(http.MethodGet, "/api/remote/outbox?wait=25", nil))
		returned <- time.Now()
	}()

	// Let the handler reach its wait before anything is queued.
	time.Sleep(150 * time.Millisecond)
	enqueued := time.Now()
	if err := s.enqueueRemoteEvent(context.Background(), "event-wake", "", "run-1", "assistant.message", []byte(`{"content":"wake"}`), enqueued.UTC()); err != nil {
		t.Fatal(err)
	}

	select {
	case woke := <-returned:
		if delay := woke.Sub(enqueued); delay > 2*time.Second {
			t.Fatalf("long-poll woke %s after the enqueue; it should wake on the write", delay)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("long-poll never returned after an event was enqueued")
	}
}

// A request cancelled while held must release the handler; otherwise a phone or
// Agent that goes away would pin a goroutine for the whole window.
func TestRemoteOutboxWaitStopsOnCancelledRequest(t *testing.T) {
	s := newRemoteOutboxTestServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet, "/api/remote/outbox?wait=25", nil).WithContext(ctx)

	returned := make(chan struct{}, 1)
	go func() {
		s.remoteOutbox(httptest.NewRecorder(), request)
		close(returned)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-returned:
	case <-time.After(3 * time.Second):
		t.Fatal("handler kept running after the request was cancelled")
	}
}

// The waker is broadcast, not a single-slot signal: two held requests must both
// be released by one enqueue.
func TestRemoteOutboxWakerReleasesEveryWaiter(t *testing.T) {
	waker := newRemoteOutboxWaker()
	first := waker.wait()
	second := waker.wait()

	waker.wake()

	for name, ch := range map[string]<-chan struct{}{"first": first, "second": second} {
		select {
		case <-ch:
		default:
			t.Fatalf("%s waiter was not released", name)
		}
	}
}
