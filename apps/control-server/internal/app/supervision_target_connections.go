package app

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

type supervisionTargetConnectionEntry struct {
	ready        chan struct{}
	retired      chan struct{}
	link         *supervisionTargetLink
	err          error
	invalidating bool
	requests     chan struct{}
}

// Target ownership belongs to the control-service lifetime. A cancelled HTTP
// request may stop waiting for a connection, but cannot close another task's
// supervisor. Reconnection replaces transport only; it never replays a start or
// grants execution authority. Callers must reconcile durable target facts.
type supervisionTargetConnections struct {
	mu        sync.Mutex
	ctx       context.Context
	cancel    context.CancelFunc
	open      func(context.Context, string) (*supervisionTargetLink, error)
	entries   map[string]*supervisionTargetConnectionEntry
	wg        sync.WaitGroup
	closed    bool
	closeOnce sync.Once
	closeErr  error
}

func newSupervisionTargetConnections(ctx context.Context, open func(context.Context, string) (*supervisionTargetLink, error)) *supervisionTargetConnections {
	ctx, cancel := context.WithCancel(ctx)
	return &supervisionTargetConnections{ctx: ctx, cancel: cancel, open: open, entries: map[string]*supervisionTargetConnectionEntry{}}
}

func (p *supervisionTargetConnections) acquire(ctx context.Context, runnerID string) (*supervisionTargetLink, error) {
	if runnerID == "" {
		return nil, targetexec.ErrConflict
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p.mu.Lock()
		if p.closed || p.ctx.Err() != nil {
			p.mu.Unlock()
			return nil, targetexec.ErrClosed
		}
		entry := p.entries[runnerID]
		if entry != nil && entry.invalidating {
			retired := entry.retired
			p.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-retired:
				continue
			case <-p.ctx.Done():
				return nil, targetexec.ErrClosed
			}
		}
		if entry == nil {
			entry = &supervisionTargetConnectionEntry{ready: make(chan struct{}), retired: make(chan struct{}), requests: make(chan struct{}, 16)}
			p.entries[runnerID] = entry
			p.wg.Add(1) // Add and Close's closed gate share mu.
			go p.connect(runnerID, entry)
		}
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-p.ctx.Done():
			return nil, targetexec.ErrClosed
		case <-entry.ready:
		}
		p.mu.Lock()
		if p.closed || p.ctx.Err() != nil {
			p.mu.Unlock()
			return nil, targetexec.ErrClosed
		}
		if entry.err != nil {
			err := entry.err
			p.mu.Unlock()
			return nil, err
		}
		if p.entries[runnerID] != entry || entry.invalidating {
			p.mu.Unlock()
			continue
		}
		if entry.link.Client.IsClosed() {
			p.mu.Unlock()
			if err := p.invalidate(runnerID, entry.link); err != nil {
				return nil, err // A replacement requires confirmed owner teardown.
			}
			continue
		}
		// Closing a borrowed view does not drop the shared owner channel. The
		// pool keeps it until explicit invalidation or control-service shutdown.
		view := *entry.link
		view.close = func() error { return nil }
		view.call = func(ctx context.Context, request targetexec.Request) (targetexec.Response, error) {
			return p.call(ctx, entry, request)
		}
		p.mu.Unlock()
		return &view, nil
	}
}

// Once handed to the owner, an RPC runs with a bounded service context. An HTTP
// disconnect only stops waiting; closing the shared pipe here would revoke all
// active target grants. Durable operation IDs still require normal reconciliation
// after a lost reply. Do not replay an uncertain mutation/start automatically.
func (p *supervisionTargetConnections) call(ctx context.Context, entry *supervisionTargetConnectionEntry, request targetexec.Request) (targetexec.Response, error) {
	if err := ctx.Err(); err != nil {
		return targetexec.Response{}, err
	}
	// Cancelled pages cannot accumulate an unbounded queue of frozen source
	// payloads/goroutines behind a slow target. Queue admission remains tied to
	// the waiter until a bounded owner request slot has actually been reserved.
	select {
	case entry.requests <- struct{}{}:
	case <-ctx.Done():
		return targetexec.Response{}, ctx.Err()
	case <-p.ctx.Done():
		return targetexec.Response{}, targetexec.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		<-entry.requests
		return targetexec.Response{}, err
	}
	// Freeze pointers and raw buffers before the caller can return on cancellation.
	raw, err := json.Marshal(request)
	var frozen targetexec.Request
	if err != nil || json.Unmarshal(raw, &frozen) != nil {
		<-entry.requests
		return targetexec.Response{}, targetexec.ErrProtocol
	}
	type result struct {
		response targetexec.Response
		err      error
	}
	done := make(chan result, 1)
	go func() {
		defer func() { <-entry.requests }()
		owner, cancel := context.WithTimeout(p.ctx, 45*time.Second)
		defer cancel()
		response, err := entry.link.Client.Call(owner, frozen)
		done <- result{response, err}
	}()
	select {
	case <-ctx.Done():
		return targetexec.Response{}, ctx.Err()
	case <-p.ctx.Done():
		return targetexec.Response{}, targetexec.ErrClosed
	case completed := <-done:
		return completed.response, completed.err
	}
}

func (p *supervisionTargetConnections) connect(runnerID string, entry *supervisionTargetConnectionEntry) {
	defer p.wg.Done()
	link, err := p.open(p.ctx, runnerID)
	if err == nil && (link == nil || link.Client == nil || link.TargetID == "") {
		err = targetexec.ErrProtocol
	}
	if err == nil && link.Client.IsClosed() {
		err = targetexec.ErrClosed // A dead opener must fail once, not spin reopen.
	}
	p.mu.Lock()
	closed := p.closed || p.ctx.Err() != nil
	p.mu.Unlock()
	var cleanupErr error
	if closed || err != nil {
		if link != nil && (link.Client != nil || link.close != nil) {
			cleanupErr = link.Close()
		}
		if closed {
			err = targetexec.ErrClosed
		}
		err = errors.Join(err, cleanupErr)
	}
	p.mu.Lock()
	entry.link, entry.err = link, err
	if err != nil && cleanupErr == nil && p.entries[runnerID] == entry {
		delete(p.entries, runnerID) // A later caller may retry opening, never start.
	}
	close(entry.ready)
	p.mu.Unlock()
}

// An old caller cannot invalidate a newer connection to the same Runner. The
// old owner channel must finish closing before a replacement opens its store.
func (p *supervisionTargetConnections) invalidate(runnerID string, view *supervisionTargetLink) error {
	p.mu.Lock()
	entry := p.entries[runnerID]
	if entry == nil || entry.link == nil || view == nil || entry.link.Client != view.Client || entry.invalidating {
		p.mu.Unlock()
		return nil
	}
	if entry.err != nil {
		err := entry.err
		p.mu.Unlock()
		return err
	}
	entry.invalidating = true
	p.mu.Unlock()
	err := entry.link.Close()
	p.mu.Lock()
	if err == nil && p.entries[runnerID] == entry {
		delete(p.entries, runnerID)
	} else if err != nil {
		entry.err = err
	}
	entry.invalidating = false
	close(entry.retired)
	p.mu.Unlock()
	return err
}

func (p *supervisionTargetConnections) Close() error {
	p.closeOnce.Do(func() { p.closeErr = p.shutdown() })
	return p.closeErr
}

func (p *supervisionTargetConnections) shutdown() error {
	p.mu.Lock()
	p.closed = true
	p.cancel()
	p.mu.Unlock()
	p.wg.Wait()
	p.mu.Lock()
	links := make([]*supervisionTargetLink, 0, len(p.entries))
	for _, entry := range p.entries {
		if entry.link != nil {
			links = append(links, entry.link)
		}
	}
	p.mu.Unlock()
	var errs []error
	for _, link := range links {
		errs = append(errs, link.Close())
	}
	return errors.Join(errs...)
}

func (s *Server) supervisionTargetConnections() (*supervisionTargetConnections, error) {
	s.supervisionMu.Lock()
	defer s.supervisionMu.Unlock()
	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()
	if closing || s.runtimeCtx == nil || s.runtimeCtx.Err() != nil {
		return nil, targetexec.ErrClosed
	}
	if s.supervisionTargets == nil {
		s.supervisionTargets = newSupervisionTargetConnections(s.runtimeCtx, s.openSupervisionTargetBinding)
	}
	return s.supervisionTargets, nil
}

func (s *Server) closeSupervisionTargetConnections() {
	s.supervisionMu.Lock()
	pool := s.supervisionTargets
	guard := s.workspaceGuardTargets
	s.supervisionMu.Unlock()
	if pool != nil {
		_ = pool.Close() // Target EOF revokes grants; quiet is reconciled separately.
	}
	if guard != nil {
		_ = guard.Close()
	}
}
