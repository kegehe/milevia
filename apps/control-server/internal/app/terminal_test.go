package app

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

type blockingTerminalFactory struct {
	started, release chan struct{}
	once             sync.Once
	session          *testTerminalSession
}

type contextTerminalFactory struct {
	started  chan struct{}
	canceled chan struct{}
}

type staticTerminalFactory struct{ session TerminalSession }

func (f staticTerminalFactory) Open(context.Context, TerminalSpec) (TerminalSession, error) {
	return f.session, nil
}

func (f contextTerminalFactory) Open(ctx context.Context, _ TerminalSpec) (TerminalSession, error) {
	close(f.started)
	<-ctx.Done()
	close(f.canceled)
	return nil, ctx.Err()
}

func (f *blockingTerminalFactory) Open(context.Context, TerminalSpec) (TerminalSession, error) {
	f.once.Do(func() { close(f.started) })
	<-f.release
	return f.session, nil
}

type testTerminalSession struct {
	id         string
	ready      chan error
	closeCount int
	waitCount  int
	mu         sync.Mutex
}

type closingTerminalSession struct {
	testTerminalSession
	done chan struct{}
}

func (s *closingTerminalSession) Close() error {
	_ = s.testTerminalSession.Close()
	close(s.done)
	return nil
}
func (s *closingTerminalSession) Wait() error { <-s.done; return nil }

func (s *testTerminalSession) ID() string                  { return s.id }
func (s *testTerminalSession) ProjectID() string           { return "project" }
func (s *testTerminalSession) Environment() string         { return "wsl" }
func (s *testTerminalSession) Ready() <-chan error         { return s.ready }
func (s *testTerminalSession) Read([]byte) (int, error)    { return 0, errors.New("closed") }
func (s *testTerminalSession) Write(p []byte) (int, error) { return len(p), nil }
func (s *testTerminalSession) Resize(uint16, uint16) error { return nil }
func (s *testTerminalSession) Close() error                { s.mu.Lock(); s.closeCount++; s.mu.Unlock(); return nil }
func (s *testTerminalSession) Wait() error                 { s.mu.Lock(); s.waitCount++; s.mu.Unlock(); return nil }

func TestTerminalCreateIsInvalidatedByProjectDeletion(t *testing.T) {
	session := &testTerminalSession{id: "terminal", ready: make(chan error, 1)}
	factory := &blockingTerminalFactory{started: make(chan struct{}), release: make(chan struct{}), session: session}
	manager := newTerminalManager(&Server{})
	manager.factory = factory
	result := make(chan error, 1)
	go func() {
		_, err := manager.create(context.Background(), Project{ID: "project", RunnerID: "local", Path: "/tmp/project"}, 120, 36)
		result <- err
	}()
	<-factory.started
	manager.closeProject("project")
	close(factory.release)
	if err := <-result; err == nil {
		t.Fatal("creation succeeded after project deletion")
	}
	session.mu.Lock()
	closes := session.closeCount
	session.mu.Unlock()
	if closes != 1 {
		t.Fatalf("closed %d times, want 1", closes)
	}
}

func TestTerminalBinaryFrameUsesLittleEndianSequence(t *testing.T) {
	frame := terminalBinaryFrame(0x0102030405060708, []byte("ok"))
	if frame.messageType != 2 || len(frame.data) != 10 {
		t.Fatalf("unexpected frame: %#v", frame)
	}
	if got := binary.LittleEndian.Uint64(frame.data[:8]); got != 0x0102030405060708 {
		t.Fatalf("sequence=%x", got)
	}
	if string(frame.data[8:]) != "ok" {
		t.Fatalf("payload=%q", frame.data[8:])
	}
}

func TestTerminalReplayTruncated(t *testing.T) {
	chunks := []terminalChunk{{seq: 4, data: []byte("kept")}}
	if !terminalReplayTruncated(chunks, 4, 0) {
		t.Fatal("missing truncation for evicted output")
	}
	if terminalReplayTruncated(chunks, 4, 3) {
		t.Fatal("reported truncation when the requested sequence is adjacent to replay")
	}
}

func TestTerminalCloseAllWaitsForRegisteredSession(t *testing.T) {
	manager := newTerminalManager(&Server{})
	session := &closingTerminalSession{testTerminalSession: testTerminalSession{id: "terminal", ready: make(chan error, 1)}, done: make(chan struct{})}
	record := &terminalRecord{session: session, state: "running"}
	manager.sessions[session.ID()] = record
	manager.sessionWG.Add(1)
	go manager.reap(record)
	closed := make(chan struct{})
	go func() {
		manager.closeAll()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("closeAll did not wait for terminal reaper")
	}
}

func TestTerminalRestoreProjectAllowsNewLease(t *testing.T) {
	manager := newTerminalManager(&Server{})
	project := Project{ID: "project", RunnerID: "local"}
	manager.closeProject(project.ID)
	if _, err := manager.reserve(project); err == nil {
		t.Fatal("reserve succeeded while project deletion is pending")
	}
	manager.restoreProject(project.ID)
	if _, err := manager.reserve(project); err != nil {
		t.Fatalf("reserve after restoring project: %v", err)
	}
}

func TestTerminalCreateIsInvalidatedByShutdown(t *testing.T) {
	session := &testTerminalSession{id: "terminal", ready: make(chan error, 1)}
	factory := &blockingTerminalFactory{started: make(chan struct{}), release: make(chan struct{}), session: session}
	manager := newTerminalManager(&Server{})
	manager.factory = factory
	result := make(chan error, 1)
	go func() {
		_, err := manager.create(context.Background(), Project{ID: "project", RunnerID: "local", Path: "/tmp/project"}, 120, 36)
		result <- err
	}()
	<-factory.started
	closed := make(chan struct{})
	go func() {
		manager.closeAll()
		close(closed)
	}()
	for {
		manager.mu.Lock()
		closing := manager.closing
		manager.mu.Unlock()
		if closing {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(factory.release)
	<-closed
	if err := <-result; err == nil {
		t.Fatal("creation succeeded after shutdown")
	}
	if _, err := manager.reserve(Project{ID: "other", RunnerID: "local"}); err == nil {
		t.Fatal("reserve succeeded while manager is closing")
	}
}

func TestTerminalShutdownCancelsPendingOpen(t *testing.T) {
	manager := newTerminalManager(&Server{})
	started := make(chan struct{})
	canceled := make(chan struct{})
	manager.factory = contextTerminalFactory{started: started, canceled: canceled}
	result := make(chan error, 1)
	go func() {
		_, err := manager.create(context.Background(), Project{ID: "project", RunnerID: "local"}, 120, 36)
		result <- err
	}()
	<-started
	manager.closeAll()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel terminal open")
	}
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("create error=%v, want context cancellation", err)
	}
}

func TestTerminalStartupTimeoutClosesUnreadySession(t *testing.T) {
	manager := newTerminalManager(&Server{})
	manager.startupTimeout = 10 * time.Millisecond
	session := &testTerminalSession{id: "terminal", ready: make(chan error)}
	manager.factory = staticTerminalFactory{session: session}
	record, err := manager.create(context.Background(), Project{ID: "project", RunnerID: "local"}, 120, 36)
	if err != nil {
		t.Fatalf("create terminal: %v", err)
	}
	select {
	case <-record.ready:
	case <-time.After(time.Second):
		t.Fatal("startup timeout did not complete readiness")
	}
	if !errors.Is(record.readyErr, errTerminalStartupTimeout) {
		t.Fatalf("ready error=%v, want startup timeout", record.readyErr)
	}
	session.mu.Lock()
	closes := session.closeCount
	session.mu.Unlock()
	if closes != 1 {
		t.Fatalf("closed %d times, want 1", closes)
	}
}

func TestSSHTerminalCloseOutputUnblocksReader(t *testing.T) {
	reader, writer := io.Pipe()
	session := &sshTerminalSession{reader: reader, writer: writer}
	session.closeOutput()
	var output [1]byte
	if _, err := session.Read(output[:]); !errors.Is(err, io.EOF) {
		t.Fatalf("read error=%v, want EOF", err)
	}
}

func TestTerminalSequenceUnmarshalPreservesUint64(t *testing.T) {
	var ctl terminalControl
	if err := json.Unmarshal([]byte(`{"type":"attach","afterSeq":"18446744073709551615"}`), &ctl); err != nil {
		t.Fatalf("unmarshal sequence: %v", err)
	}
	if uint64(ctl.AfterSeq) != ^uint64(0) {
		t.Fatalf("sequence=%d", ctl.AfterSeq)
	}
	if err := json.Unmarshal([]byte(`{"afterSeq":1.5}`), &ctl); err == nil {
		t.Fatal("accepted a fractional sequence")
	}
}

func TestTerminalConsumeNotifiesAttachedClientOfExit(t *testing.T) {
	manager := newTerminalManager(&Server{})
	record := &terminalRecord{
		session:    &testTerminalSession{id: "terminal", ready: make(chan error, 1)},
		state:      "running",
		waiterDone: true,
		exitCode:   terminalInt(7),
		subscriber: &terminalSubscriber{send: make(chan terminalFrame, 1), closeRequest: make(chan terminalCloseRequest, 1)},
	}
	manager.consume(record)
	if record.state != "exited" {
		t.Fatalf("state=%q", record.state)
	}
	frame := <-record.subscriber.send
	if frame.messageType != 1 || string(frame.data) != `{"code":7,"type":"exit"}` {
		t.Fatalf("exit frame=%#v", frame)
	}
}

func TestTerminalConsumeSchedulesDetachedExitedSessionForCleanup(t *testing.T) {
	manager := newTerminalManager(&Server{})
	record := &terminalRecord{
		session:    &testTerminalSession{id: "terminal", ready: make(chan error, 1)},
		state:      "running",
		waiterDone: true,
	}
	manager.sessions[record.session.ID()] = record
	manager.consume(record)
	if record.state != "exited" || record.detachedAt.IsZero() {
		t.Fatalf("exited record was not marked for cleanup: %#v", record)
	}
}

func terminalInt(value int) *int { return &value }
