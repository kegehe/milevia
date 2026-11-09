package targetexec

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func transportFixture(t *testing.T) (*Client, *Store, Control, ExecutionSpec, Input, *executorForTest, <-chan error) {
	t.Helper()
	supervisor, store, control, spec, input, executor := supervisorFixture(t)
	serverPipe, clientPipe := net.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- (&Transport{Store: store, Supervisor: supervisor}).Serve(context.Background(), serverPipe)
	}()
	client := NewClient(clientPipe)
	t.Cleanup(func() { client.Close() })
	hello, err := client.Call(context.Background(), Request{ID: "hello-1", Method: "hello"})
	if err != nil || hello.TargetID != control.TargetID {
		t.Fatal(hello, err)
	}
	return client, store, control, spec, input, executor, done
}

func TestTransportRealChannelRunsExactlyOnceAndDisconnectRevokes(t *testing.T) {
	client, store, control, spec, input, executor, done := transportFixture(t)
	request := Request{ID: "start-1", Method: "start", SessionID: control.SessionID, ExecutionID: spec.ExecutionID, Spec: &spec, Input: &input}
	if _, err := client.Call(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	request.ID = "start-replay"
	if _, err := client.Call(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return executor.resumes.Load() == 1 })
	if executor.starts.Load() != 1 {
		t.Fatal("duplicate process start")
	}
	if _, err := client.Call(context.Background(), Request{ID: "bad-target", Method: "query", TargetID: "other-target", SessionID: control.SessionID}); !errors.Is(err, ErrAuthority) {
		t.Fatal("silently retargeted", err)
	}
	client.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("disconnect did not converge")
	}
	facts, err := store.Facts()
	if err != nil || !facts.Sessions[control.SessionID].RecoveryRequired || facts.Sessions[control.SessionID].Executions[spec.ExecutionID].State != "quiescent" {
		t.Fatal("lost channel released or forgot active execution", facts, err)
	}
	if err := store.Release(control.SessionID, control.OwnerEpoch, control.Version); !errors.Is(err, ErrAuthority) {
		t.Fatal("disconnected directory released without stopped control", err)
	}
}

func TestTransportRejectsUnknownVersionAndMalformedFrameWithoutStarting(t *testing.T) {
	supervisor, store, _, _, _, executor := supervisorFixture(t)
	a, b := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- (&Transport{Store: store, Supervisor: supervisor}).Serve(context.Background(), a) }()
	if _, err := b.Write([]byte("{\"protocol\":1,\"id\":\"frame\",\"method\":\"hello\",\"unknown\":true}\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrProtocol) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("invalid protocol remained connected")
	}
	b.Close()
	if executor.starts.Load() != 0 {
		t.Fatal("invalid request started execution")
	}
}

func TestClientCancellationInterruptsBlockedResponseAndCannotReuseStream(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	client := NewClient(a)
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := client.Call(ctx, Request{ID: "hello", Method: "hello"}); done <- err }()
	buffer := make([]byte, 4096)
	if _, err := b.Read(buffer); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("call leaked after cancellation")
	}
	if _, err := client.Call(context.Background(), Request{ID: "late", Method: "hello"}); !errors.Is(err, ErrClosed) {
		t.Fatal("canceled channel reused", err)
	}
}

func TestClientQueuedCancellationReturnsWithoutRevokingActiveCall(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	client := NewClient(a)
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	firstRead, release := make(chan struct{}), make(chan struct{})
	serverDone := make(chan error, 1)
	go func() {
		decoder, encoder := json.NewDecoder(b), json.NewEncoder(b)
		for i := 0; i < 2; i++ {
			var request Request
			if err := decoder.Decode(&request); err != nil {
				serverDone <- err
				return
			}
			if i == 0 {
				close(firstRead)
				select {
				case <-release:
				case <-ctx.Done():
					serverDone <- ctx.Err()
					return
				}
			}
			if err := encoder.Encode(Response{Protocol: ProtocolVersion, ID: request.ID, TargetID: "queued-cancellation-target", TargetTime: time.Now().UTC()}); err != nil {
				serverDone <- err
				return
			}
		}
		serverDone <- nil
	}()
	first := make(chan error, 1)
	go func() { _, err := client.Call(ctx, Request{ID: "first", Method: "hello"}); first <- err }()
	select {
	case <-firstRead:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	queued, cancelQueued := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancelQueued()
	result := make(chan error, 1)
	go func() { _, err := client.Call(queued, Request{ID: "cancelled", Method: "hello"}); result <- err }()
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) || client.IsClosed() {
			t.Fatal("queued cancellation revoked owner", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued call ignored cancellation while waiting for active response")
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal("queued cancellation broke active RPC", err)
	}
	if _, err := client.Call(ctx, Request{ID: "next", Method: "hello"}); err != nil {
		t.Fatal("owner stream unusable after queued cancellation", err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestTransportRenewalResponseCanBeRetriedWithoutExtendingOriginalDeadline(t *testing.T) {
	client, store, control, spec, input, executor, _ := transportFixture(t)
	if _, err := client.Call(context.Background(), Request{ID: "start", Method: "start", SessionID: control.SessionID, ExecutionID: spec.ExecutionID, Spec: &spec, Input: &input}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return executor.resumes.Load() == 1 })
	until := time.Now().UTC().Add(29 * time.Second)
	request := Request{ID: "renew", Method: "renew", SessionID: control.SessionID, ExecutionID: spec.ExecutionID, OwnerEpoch: control.OwnerEpoch, AuthorizationVersion: control.Version, Until: until}
	for range 2 {
		if _, err := client.Call(context.Background(), request); err != nil {
			t.Fatal("renew retry failed", err)
		}
	}
	facts, _ := store.Facts()
	if !facts.Sessions[control.SessionID].Executions[spec.ExecutionID].Spec.TurnDeadline.Equal(spec.TurnDeadline) {
		t.Fatal("renew reset turn budget")
	}
}

func TestTargetDeadlineUsesTargetClockAndConservativelyAccountsForTransit(t *testing.T) {
	// A remote clock may differ by hours; the controller must not stamp grants
	// from its own wall clock, or grant a full new TTL after a slow response.
	target := time.Now().UTC().Add(-8 * time.Hour)
	client := &Client{clockSample: target, clockSent: time.Now().Add(-2 * time.Second)}
	deadline, err := client.TargetDeadline(25 * time.Second)
	if err != nil || deadline.After(target.Add(23*time.Second)) || deadline.Before(target.Add(22*time.Second)) {
		t.Fatal("unsafe remote clock deadline", deadline, err)
	}
	client.clockSent = time.Now().Add(-31 * time.Second)
	if _, err := client.TargetDeadline(30 * time.Second); !errors.Is(err, ErrAuthority) {
		t.Fatal("stale clock sample renewed a grant", err)
	}
	client.clockSent = time.Now()
	if _, err := client.TargetDeadline(31 * time.Second); !errors.Is(err, ErrAuthority) {
		t.Fatal("oversized grant accepted", err)
	}
}

func TestTargetExecutionWindowUsesRemainingBudgetAndRequiresFreshClock(t *testing.T) {
	target := time.Now().UTC().Add(9 * time.Hour)
	client := &Client{clockSample: target, clockSent: time.Now().Add(-2 * time.Second)}
	prepared, deadline, err := client.TargetExecutionWindow(75 * time.Second)
	if err != nil || prepared != target || deadline.After(target.Add(73*time.Second)) || deadline.Before(target.Add(72*time.Second)) {
		t.Fatal("remaining turn was not mapped conservatively", prepared, deadline, err)
	}
	client.clockSent = time.Now().Add(-31 * time.Second)
	if _, _, err := client.TargetExecutionWindow(10 * time.Minute); !errors.Is(err, ErrAuthority) {
		t.Fatal("long turn accepted an expired clock sample", err)
	}
	client.clockSent = time.Now()
	for _, remaining := range []time.Duration{0, -time.Second, 31 * time.Minute} {
		if _, _, err := client.TargetExecutionWindow(remaining); !errors.Is(err, ErrAuthority) {
			t.Fatal("invalid remaining turn budget", remaining, err)
		}
	}
}

func TestTransportOversizedObservationKeepsOwnerControlChannelUsable(t *testing.T) {
	supervisor, store, control, _, _, _ := supervisorFixture(t)
	a, b := net.Pipe()
	done := make(chan error, 1)
	transport := &Transport{Store: store, Supervisor: supervisor, Extension: func(context.Context, Request) (json.RawMessage, error) {
		return json.Marshal(strings.Repeat("<", maxPacketBytes/3))
	}}
	go func() { done <- transport.Serve(context.Background(), a) }()
	client := NewClient(b)
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := client.Call(ctx, Request{ID: "hello", Method: "hello"}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Call(ctx, Request{ID: "large", Method: "snapshot"}); !errors.Is(err, ErrLimit) {
		t.Fatal("large valid observation closed the connection or masqueraded as success", err)
	}
	response, err := client.Call(ctx, Request{ID: "query", Method: "query", SessionID: control.SessionID})
	if err != nil || response.Session == nil || response.Session.RecoveryRequired {
		t.Fatal("capacity error revoked the owner channel", response, err)
	}
	client.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
