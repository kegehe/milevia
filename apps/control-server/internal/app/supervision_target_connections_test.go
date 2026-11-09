package app

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

func supervisionTargetPoolPipe(t *testing.T, ctx context.Context, storage string, closes *atomic.Int32) (*supervisionTargetLink, error) {
	t.Helper()
	left, right := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- ServeSupervisionTarget(context.Background(), Config{DataDir: storage}, right) }()
	client := targetexec.NewClient(left)
	var once sync.Once
	var closeErr error
	closeLink := func() error {
		once.Do(func() {
			closes.Add(1)
			_ = client.Close()
			closeErr = <-done // Includes closing the durable target store lock.
		})
		return closeErr
	}
	hello, err := client.Call(ctx, targetexec.Request{ID: "hello", Method: "hello"})
	if err != nil {
		_ = closeLink()
		return nil, err
	}
	return &supervisionTargetLink{Client: client, TargetID: hello.TargetID, close: closeLink}, nil
}

func TestSupervisionTargetConnectionsRejectAlreadyClosedOpenerWithoutRetryLoop(t *testing.T) {
	left, right := net.Pipe()
	defer right.Close()
	client := targetexec.NewClient(left)
	client.Close()
	var opens atomic.Int32
	pool := newSupervisionTargetConnections(context.Background(), func(context.Context, string) (*supervisionTargetLink, error) {
		opens.Add(1)
		return &supervisionTargetLink{Client: client, TargetID: "target"}, nil
	})
	defer pool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := pool.acquire(ctx, "runner"); !errors.Is(err, targetexec.ErrClosed) || opens.Load() != 1 {
		t.Fatal("dead opener did not fail once", opens.Load(), err)
	}
}

func TestSupervisionTargetConnectionsShareOwnerAndCallerCancellationDoesNotRevoke(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started, release := make(chan struct{}), make(chan struct{})
	var opens, closes atomic.Int32
	storage := t.TempDir()
	pool := newSupervisionTargetConnections(ctx, func(owner context.Context, _ string) (*supervisionTargetLink, error) {
		opens.Add(1)
		close(started)
		select {
		case <-release:
		case <-owner.Done():
			return nil, owner.Err()
		}
		return supervisionTargetPoolPipe(t, owner, storage, &closes)
	})
	defer pool.Close()
	request, stopRequest := context.WithCancel(ctx)
	first := make(chan error, 1)
	go func() { _, err := pool.acquire(request, "runner"); first <- err }()
	<-started
	stopRequest()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled request did not stop waiting", err)
	}
	const callers = 16
	links := make(chan *supervisionTargetLink, callers)
	errs := make(chan error, callers)
	for range callers {
		go func() { link, err := pool.acquire(ctx, "runner"); links <- link; errs <- err }()
	}
	close(release)
	var reference *supervisionTargetLink
	for range callers {
		link, err := <-links, <-errs
		if err != nil || link == nil {
			t.Fatal("shared connection unavailable", err)
		}
		if reference != nil && link.Client != reference.Client {
			t.Fatal("concurrent callers opened different target owners")
		}
		reference = link
		if err := link.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if opens.Load() != 1 || closes.Load() != 0 {
		t.Fatal("request cancellation or view close revoked shared owner", opens.Load(), closes.Load())
	}
	if _, err := reference.Client.Call(ctx, targetexec.Request{ID: "still-alive", Method: "hello"}); err != nil {
		t.Fatal("borrowed view close ended target supervisor", err)
	}
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
	if closes.Load() != 1 {
		t.Fatal("shutdown did not close exactly one owner", closes.Load())
	}
	if _, err := pool.acquire(ctx, "runner"); !errors.Is(err, targetexec.ErrClosed) {
		t.Fatal("shutdown admitted new target connection", err)
	}
}

func TestSupervisionTargetConnectionsRetryOpenAndOldInvalidationCannotCloseReplacement(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var opens, closes atomic.Int32
	storage := t.TempDir()
	failure := errors.New("unavailable test target")
	pool := newSupervisionTargetConnections(ctx, func(owner context.Context, _ string) (*supervisionTargetLink, error) {
		if opens.Add(1) == 1 {
			return nil, failure
		}
		return supervisionTargetPoolPipe(t, owner, storage, &closes)
	})
	defer pool.Close()
	if _, err := pool.acquire(ctx, "runner"); !errors.Is(err, failure) {
		t.Fatal("opening failure was not returned", err)
	}
	old, err := pool.acquire(ctx, "runner")
	if err != nil {
		t.Fatal("failed opening was permanently cached", err)
	}
	if err := pool.invalidate("runner", old); err != nil {
		t.Fatal(err)
	}
	replacement, err := pool.acquire(ctx, "runner")
	if err != nil || replacement.Client == old.Client || replacement.TargetID != old.TargetID {
		t.Fatal("reconnect failed to use original durable target", err)
	}
	if err := pool.invalidate("runner", old); err != nil {
		t.Fatal(err)
	}
	if _, err := replacement.Client.Call(ctx, targetexec.Request{ID: "after-stale-caller", Method: "hello"}); err != nil {
		t.Fatal("old caller closed replacement owner", err)
	}
	if opens.Load() != 3 || closes.Load() != 1 {
		t.Fatal("unexpected owner lifetime", opens.Load(), closes.Load())
	}
}

func TestSupervisionTargetConnectionsShutdownWaitsForOpeningAndClosesLateResult(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started := make(chan struct{})
	pool := newSupervisionTargetConnections(ctx, func(owner context.Context, _ string) (*supervisionTargetLink, error) {
		close(started)
		<-owner.Done()
		return nil, owner.Err()
	})
	result := make(chan error, 1)
	go func() { _, err := pool.acquire(ctx, "runner"); result <- err }()
	<-started
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, targetexec.ErrClosed) {
		t.Fatal("shutdown did not fence pending acquisition", err)
	}
	if _, err := pool.acquire(ctx, "runner"); !errors.Is(err, targetexec.ErrClosed) {
		t.Fatal(err)
	}
}

func TestSupervisionTargetConnectionsRPCWaiterCancellationPreservesOwner(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started, release := make(chan struct{}), make(chan struct{})
	pool := newSupervisionTargetConnections(ctx, func(owner context.Context, _ string) (*supervisionTargetLink, error) {
		store, err := targetexec.Open(t.TempDir())
		if err != nil {
			return nil, err
		}
		runtime := &supervisionTargetRuntime{store: store, storage: t.TempDir(), config: Config{WorkspaceGuardDir: t.TempDir()}}
		supervisor, err := targetexec.NewSupervisor(context.Background(), store, runtime)
		if err != nil {
			store.Close()
			return nil, err
		}
		left, right := net.Pipe()
		done := make(chan error, 1)
		transport := &targetexec.Transport{Store: store, Supervisor: supervisor, Extension: func(context.Context, targetexec.Request) (json.RawMessage, error) {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return json.RawMessage(`{}`), nil
		}}
		go func() { done <- transport.Serve(context.Background(), right) }()
		client := targetexec.NewClient(left)
		hello, err := client.Call(owner, targetexec.Request{ID: "hello", Method: "hello"})
		if err != nil {
			client.Close()
			<-done
			store.Close()
			return nil, err
		}
		var once sync.Once
		var closeErr error
		return &supervisionTargetLink{Client: client, TargetID: hello.TargetID, close: func() error {
			once.Do(func() { client.Close(); closeErr = errors.Join(<-done, store.Close()) })
			return closeErr
		}}, nil
	})
	defer pool.Close()
	link, err := pool.acquire(ctx, "runner")
	if err != nil {
		t.Fatal(err)
	}
	waiter, stopWaiting := context.WithCancel(ctx)
	first := make(chan error, 1)
	go func() {
		_, err := link.Call(waiter, targetexec.Request{ID: "slow", Method: "slow-observation"})
		first <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	stopWaiting()
	if err := <-first; !errors.Is(err, context.Canceled) || link.Client.IsClosed() {
		t.Fatal("HTTP waiter cancelled shared owner", err)
	}
	close(release)
	if _, err := link.Call(ctx, targetexec.Request{ID: "after-cancellation", Method: "hello"}); err != nil {
		t.Fatal("lost observation reply broke later RPC framing", err)
	}
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSupervisionTargetConnectionsClosedOwnerIsRetiredBeforeReplacement(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var opens, closes atomic.Int32
	storage := t.TempDir()
	pool := newSupervisionTargetConnections(ctx, func(owner context.Context, _ string) (*supervisionTargetLink, error) {
		opens.Add(1)
		return supervisionTargetPoolPipe(t, owner, storage, &closes)
	})
	defer pool.Close()
	old, err := pool.acquire(ctx, "runner")
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Client.Close(); err != nil {
		t.Fatal(err)
	}
	replacement, err := pool.acquire(ctx, "runner")
	if err != nil || replacement.Client == old.Client || replacement.TargetID != old.TargetID || opens.Load() != 2 || closes.Load() != 1 {
		t.Fatal("closed owner reused or target identity changed", err, opens.Load(), closes.Load())
	}
}

func TestSupervisionTargetConnectionsUnconfirmedTeardownCannotOpenReplacement(t *testing.T) {
	for _, opening := range []bool{false, true} {
		t.Run(map[bool]string{false: "invalidate", true: "failed-opening"}[opening], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var opens, closes atomic.Int32
			failure := errors.New("owner teardown not confirmed")
			storage := t.TempDir()
			pool := newSupervisionTargetConnections(ctx, func(owner context.Context, _ string) (*supervisionTargetLink, error) {
				opens.Add(1)
				link, err := supervisionTargetPoolPipe(t, owner, storage, &closes)
				if err != nil {
					return nil, err
				}
				closeOwner := link.close
				link.close = func() error { return errors.Join(closeOwner(), failure) }
				if opening {
					return link, targetexec.ErrProtocol
				}
				return link, nil
			})
			defer pool.Close()
			link, err := pool.acquire(ctx, "runner")
			if opening {
				if !errors.Is(err, failure) {
					t.Fatal("opening lost unconfirmed cleanup", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if err := pool.invalidate("runner", link); !errors.Is(err, failure) {
					t.Fatal("invalidation lost teardown failure", err)
				}
			}
			for range 2 {
				if _, err := pool.acquire(ctx, "runner"); !errors.Is(err, failure) {
					t.Fatal("quarantined owner was reopened", err)
				}
			}
			if opens.Load() != 1 || closes.Load() != 1 {
				t.Fatal("unconfirmed cleanup created another owner", opens.Load(), closes.Load())
			}
		})
	}
}
