package app

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
	"golang.org/x/crypto/ssh"
)

const (
	terminalMaxProjects    = 12
	terminalMaxPerProject  = 3
	terminalMaxInput       = 64 << 10
	terminalMaxOutputFrame = 256 << 10
	terminalMaxReplay      = 1 << 20
	terminalMaxQueue       = 1 << 20
	terminalDetachedTTL    = 60 * time.Second
	terminalStartupTimeout = 10 * time.Second
	terminalShutdownWait   = 10 * time.Second
)

var errTerminalStartupTimeout = errors.New("terminal startup timed out")

type TerminalSession interface {
	ID() string
	ProjectID() string
	Environment() string
	Ready() <-chan error
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	Resize(uint16, uint16) error
	Close() error
	Wait() error
}

type TerminalSpec struct {
	ProjectID string
	RunnerID  string
	Target    string
	WorkDir   string
	WSLDistro string
	Shell     string
	Cols      uint16
	Rows      uint16
}

type TerminalFactory interface {
	Open(context.Context, TerminalSpec) (TerminalSession, error)
}

type terminalFactory struct{ server *Server }

func (f terminalFactory) Open(ctx context.Context, spec TerminalSpec) (TerminalSession, error) {
	if strings.HasPrefix(spec.RunnerID, "ssh-") {
		return f.server.openSSHTerminal(ctx, spec)
	}
	return openPlatformTerminal(ctx, spec)
}

type terminalRecord struct {
	session                          TerminalSession
	projectID, runnerID, environment string
	createdAt                        time.Time
	mu                               sync.Mutex
	state                            string
	readyErr                         error
	ready                            chan struct{}
	seq                              uint64
	replay                           []terminalChunk
	subscriber                       *terminalSubscriber
	closed                           bool
	detachedAt                       time.Time
	readerDone, waiterDone           bool
	exitCode                         *int
}
type terminalChunk struct {
	seq  uint64
	data []byte
}

type terminalFrame struct {
	messageType int
	data        []byte
}

// terminalSubscriber owns all WebSocket writes for one terminal attachment.
// Its byte-accounted queue keeps a stalled browser from blocking PTY reads.
type terminalSubscriber struct {
	conn         *websocket.Conn
	send         chan terminalFrame
	closeRequest chan terminalCloseRequest
	mu           sync.Mutex
	queued       int
	closed       bool
}

type terminalCloseRequest struct {
	code   int
	reason string
}

func newTerminalSubscriber(conn *websocket.Conn) *terminalSubscriber {
	return &terminalSubscriber{conn: conn, send: make(chan terminalFrame, 64), closeRequest: make(chan terminalCloseRequest, 1)}
}

func (sub *terminalSubscriber) enqueue(frame terminalFrame) bool {
	sub.mu.Lock()
	if sub.closed {
		sub.mu.Unlock()
		return false
	}
	if len(frame.data) > terminalMaxQueue-sub.queued {
		sub.closed = true
		sub.mu.Unlock()
		sub.closeRequest <- terminalCloseRequest{code: websocket.CloseTryAgainLater, reason: "terminal client is too slow"}
		return false
	}
	sub.queued += len(frame.data)
	select {
	case sub.send <- frame:
		sub.mu.Unlock()
		return true
	default:
		sub.queued -= len(frame.data)
		sub.closed = true
		sub.mu.Unlock()
		sub.closeRequest <- terminalCloseRequest{code: websocket.CloseTryAgainLater, reason: "terminal client is too slow"}
		return false
	}
}

func (sub *terminalSubscriber) close(code int, reason string) {
	sub.mu.Lock()
	if sub.closed {
		sub.mu.Unlock()
		return
	}
	sub.closed = true
	sub.mu.Unlock()
	sub.closeRequest <- terminalCloseRequest{code: code, reason: reason}
}

func (sub *terminalSubscriber) writeLoop(done chan<- struct{}) {
	defer close(done)
	defer sub.conn.Close()
	for {
		select {
		case request := <-sub.closeRequest:
			_ = sub.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(request.code, request.reason), time.Now().Add(time.Second))
			return
		case frame := <-sub.send:
			sub.mu.Lock()
			sub.queued -= len(frame.data)
			sub.mu.Unlock()
			_ = sub.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := sub.conn.WriteMessage(frame.messageType, frame.data); err != nil {
				return
			}
		}
	}
}

type terminalManager struct {
	server             *Server
	mu                 sync.Mutex
	sessions           map[string]*terminalRecord
	factory            TerminalFactory
	projectGenerations map[string]uint64
	runnerGenerations  map[string]uint64
	deletedProjects    map[string]bool
	pending            map[uint64]terminalLease
	nextLease          uint64
	closing            bool
	shutdownCtx        context.Context
	shutdown           context.CancelFunc
	creationWG         sync.WaitGroup
	sessionWG          sync.WaitGroup
	startupTimeout     time.Duration
}

type terminalLease struct {
	projectID, runnerID                 string
	projectGeneration, runnerGeneration uint64
}

func newTerminalManager(s *Server) *terminalManager {
	shutdownCtx, shutdown := context.WithCancel(context.Background())
	return &terminalManager{server: s, sessions: map[string]*terminalRecord{}, factory: terminalFactory{server: s}, projectGenerations: map[string]uint64{}, runnerGenerations: map[string]uint64{}, deletedProjects: map[string]bool{}, pending: map[uint64]terminalLease{}, shutdownCtx: shutdownCtx, shutdown: shutdown, startupTimeout: terminalStartupTimeout}
}

func (m *terminalManager) create(_ context.Context, project Project, cols, rows uint16) (*terminalRecord, error) {
	if cols < 1 || cols > 500 || rows < 1 || rows > 200 {
		return nil, errors.New("invalid terminal size")
	}
	if err := m.beginCreation(); err != nil {
		return nil, err
	}
	defer m.creationWG.Done()
	leaseID, err := m.reserve(project)
	if err != nil {
		return nil, err
	}
	defer m.releaseLease(leaseID)

	target := m.server.resolveAgentTargetEnv(project.RunnerID, project.Path)
	if strings.HasPrefix(project.RunnerID, "ssh-") {
		target = agentTargetEnvRemote
	}
	spec := TerminalSpec{ProjectID: project.ID, RunnerID: project.RunnerID, Target: string(target), WorkDir: project.Path, WSLDistro: m.server.wslDistro, Cols: cols, Rows: rows}
	// A request context ends when the HTTP handler returns. It must not own a
	// terminal process, so Open only receives the manager lifetime and startup
	// deadline. Platform sessions detach their child process from this context.
	openCtx, cancelOpen := context.WithTimeout(m.shutdownCtx, m.startupTimeout)
	defer cancelOpen()
	sess, err := m.factory.Open(openCtx, spec)
	if err != nil {
		return nil, err
	}
	createdAt := time.Now().UTC()
	r := &terminalRecord{session: sess, projectID: project.ID, runnerID: project.RunnerID, environment: string(target), createdAt: createdAt, state: "starting", ready: make(chan struct{}), detachedAt: createdAt}
	m.mu.Lock()
	if !m.leaseValidLocked(leaseID) || len(m.sessions) >= terminalMaxProjects || m.projectSessionCountLocked(project.ID) >= terminalMaxPerProject {
		m.mu.Unlock()
		_ = sess.Close()
		_ = sess.Wait()
		return nil, errors.New("terminal creation was invalidated")
	}
	m.sessions[sess.ID()] = r
	m.sessionWG.Add(1)
	m.mu.Unlock()
	go m.consume(r)
	go m.reap(r)
	go m.awaitReady(r)
	m.closeDetachedLater(sess.ID(), createdAt)
	return r, nil
}

func (m *terminalManager) awaitReady(r *terminalRecord) {
	timer := time.NewTimer(m.startupTimeout)
	defer timer.Stop()
	var err error
	select {
	case err = <-r.session.Ready():
	case <-timer.C:
		err = errTerminalStartupTimeout
	case <-m.shutdownCtx.Done():
		err = m.shutdownCtx.Err()
	}
	r.mu.Lock()
	r.readyErr = err
	if err != nil && !r.closed {
		r.state = "failed"
	} else if !r.closed && r.state != "exited" {
		r.state = "running"
	}
	close(r.ready)
	r.mu.Unlock()
	if err != nil {
		_ = m.close(r.session.ID())
	}
}

func (m *terminalManager) beginCreation() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing {
		return errors.New("control service is shutting down")
	}
	m.creationWG.Add(1)
	return nil
}

func (m *terminalManager) reserve(project Project) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing {
		return 0, errors.New("control service is shutting down")
	}
	if m.deletedProjects[project.ID] {
		return 0, errors.New("project is being deleted")
	}
	if len(m.sessions)+len(m.pending) >= terminalMaxProjects {
		return 0, errors.New("terminal session limit reached")
	}
	pendingForProject := 0
	for _, lease := range m.pending {
		if lease.projectID == project.ID {
			pendingForProject++
		}
	}
	if m.projectSessionCountLocked(project.ID)+pendingForProject >= terminalMaxPerProject {
		return 0, errors.New("project terminal session limit reached")
	}
	m.nextLease++
	m.pending[m.nextLease] = terminalLease{projectID: project.ID, runnerID: project.RunnerID, projectGeneration: m.projectGenerations[project.ID], runnerGeneration: m.runnerGenerations[project.RunnerID]}
	return m.nextLease, nil
}
func (m *terminalManager) releaseLease(id uint64) { m.mu.Lock(); delete(m.pending, id); m.mu.Unlock() }
func (m *terminalManager) leaseValidLocked(id uint64) bool {
	lease, ok := m.pending[id]
	return ok && !m.closing && !m.deletedProjects[lease.projectID] && m.projectGenerations[lease.projectID] == lease.projectGeneration && m.runnerGenerations[lease.runnerID] == lease.runnerGeneration
}

func (m *terminalManager) projectSessionCountLocked(projectID string) int {
	count := 0
	for _, r := range m.sessions {
		if r.projectID == projectID && !r.closed {
			count++
		}
	}
	return count
}

func (m *terminalManager) consume(r *terminalRecord) {
	buf := make([]byte, 32<<10)
	for {
		n, err := r.session.Read(buf)
		if n > 0 {
			m.appendOutput(r, buf[:n])
		}
		if err != nil {
			m.markReaderDone(r)
			return
		}
	}
}

type terminalExitCoder interface{ ExitCode() int }
type terminalExitCodeProvider interface{ TerminalExitCode() *int }

type terminalExitStatus struct{ code int }

func (s terminalExitStatus) Error() string { return "terminal exited with a non-zero status" }
func (s terminalExitStatus) ExitCode() int { return s.code }

func terminalExitCode(session TerminalSession, err error) *int {
	if provider, ok := session.(terminalExitCodeProvider); ok {
		if code := provider.TerminalExitCode(); code != nil {
			return code
		}
	}
	if err == nil {
		code := 0
		return &code
	}
	var exitCoder terminalExitCoder
	if errors.As(err, &exitCoder) {
		code := exitCoder.ExitCode()
		return &code
	}
	var sshExit *ssh.ExitError
	if errors.As(err, &sshExit) {
		code := sshExit.ExitStatus()
		return &code
	}
	return nil
}

func (m *terminalManager) reap(r *terminalRecord) {
	defer m.sessionWG.Done()
	err := r.session.Wait()
	m.markWaiterDone(r, terminalExitCode(r.session, err))
}

func (m *terminalManager) markReaderDone(r *terminalRecord) {
	r.mu.Lock()
	r.readerDone = true
	detachedAt := m.markExitedLocked(r)
	r.mu.Unlock()
	if !detachedAt.IsZero() {
		m.closeDetachedLater(r.session.ID(), detachedAt)
	}
}

func (m *terminalManager) markWaiterDone(r *terminalRecord, exitCode *int) {
	r.mu.Lock()
	r.waiterDone = true
	r.exitCode = exitCode
	detachedAt := m.markExitedLocked(r)
	r.mu.Unlock()
	if !detachedAt.IsZero() {
		m.closeDetachedLater(r.session.ID(), detachedAt)
	}
}

// markExitedLocked preserves final PTY output by waiting for both the reader
// and process reaper before publishing the terminal exit event.
func (m *terminalManager) markExitedLocked(r *terminalRecord) time.Time {
	if r.closed || r.state == "failed" || r.state == "exited" || !r.readerDone || !r.waiterDone {
		return time.Time{}
	}
	r.state = "exited"
	message := map[string]any{"type": "exit"}
	if r.exitCode != nil {
		message["code"] = *r.exitCode
	}
	if r.subscriber != nil {
		_ = r.subscriber.enqueue(terminalControlFrame(message))
		return time.Time{}
	}
	r.detachedAt = time.Now()
	return r.detachedAt
}
func (m *terminalManager) appendOutput(r *terminalRecord, data []byte) {
	for len(data) > 0 {
		n := len(data)
		if n > terminalMaxOutputFrame {
			n = terminalMaxOutputFrame
		}
		p := append([]byte(nil), data[:n]...)
		data = data[n:]
		r.mu.Lock()
		r.seq++
		r.replay = append(r.replay, terminalChunk{r.seq, p})
		total := 0
		for _, c := range r.replay {
			total += len(c.data) + 8
		}
		for total > terminalMaxReplay && len(r.replay) > 0 {
			total -= len(r.replay[0].data) + 8
			r.replay = r.replay[1:]
		}
		sub := r.subscriber
		seq := r.seq
		r.mu.Unlock()
		if sub != nil && !sub.enqueue(terminalBinaryFrame(seq, p)) {
			if detachedAt, detached := m.detachSubscriber(r, sub); detached {
				m.closeDetachedLater(r.session.ID(), detachedAt)
			}
		}
	}
}
func terminalBinaryFrame(seq uint64, data []byte) terminalFrame {
	payload := make([]byte, 8+len(data))
	binary.LittleEndian.PutUint64(payload, seq)
	copy(payload[8:], data)
	return terminalFrame{messageType: websocket.BinaryMessage, data: payload}
}
func terminalControlFrame(message any) terminalFrame {
	payload, err := json.Marshal(message)
	if err != nil {
		return terminalFrame{}
	}
	return terminalFrame{messageType: websocket.TextMessage, data: payload}
}
func (m *terminalManager) detachSubscriber(r *terminalRecord, sub *terminalSubscriber) (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.subscriber == sub {
		r.subscriber = nil
		r.detachedAt = time.Now()
		return r.detachedAt, true
	}
	return time.Time{}, false
}

func (m *terminalManager) get(id string) (*terminalRecord, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.sessions[id]
	return r, ok
}
func (m *terminalManager) close(id string) error {
	m.mu.Lock()
	r, ok := m.sessions[id]
	if ok {
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	if !ok {
		return nil
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.state = "closed"
	if r.subscriber != nil {
		r.subscriber.close(websocket.CloseNormalClosure, "terminal closed")
		r.subscriber = nil
	}
	r.mu.Unlock()
	return r.session.Close()
}
func (m *terminalManager) closeProject(projectID string) {
	m.blockProject(projectID)
	m.mu.Lock()
	ids := []string{}
	for id, r := range m.sessions {
		if r.projectID == projectID {
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()
	for _, id := range ids {
		_ = m.close(id)
	}
}

// blockProject invalidates pending creation without disrupting terminals that
// remain usable if the enclosing project deletion later rolls back.
func (m *terminalManager) blockProject(projectID string) {
	m.mu.Lock()
	m.deletedProjects[projectID] = true
	m.projectGenerations[projectID]++
	m.mu.Unlock()
}
func (m *terminalManager) closeRunner(runnerID string) {
	m.mu.Lock()
	m.runnerGenerations[runnerID]++
	ids := []string{}
	for id, r := range m.sessions {
		if r.runnerID == runnerID {
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()
	for _, id := range ids {
		_ = m.close(id)
	}
}
func (m *terminalManager) closeAll() {
	m.mu.Lock()
	m.closing = true
	m.shutdown()
	// Removing pending leases makes an already-opened terminal fail the final
	// registration check, so shutdown cannot publish a late session.
	m.pending = map[uint64]terminalLease{}
	ids := []string{}
	for id := range m.sessions {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		_ = m.close(id)
	}
	// beginCreation and session registration share m.mu with the closing flag,
	// so no Add can race with these waits after closeAll has set closing. A
	// broken remote driver must not prevent the whole control service exiting.
	done := make(chan struct{})
	go func() {
		m.creationWG.Wait()
		m.sessionWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(terminalShutdownWait):
		log.Printf("[terminal] cleanup did not finish within %s", terminalShutdownWait)
	}
}

func (m *terminalManager) restoreProject(projectID string) {
	m.mu.Lock()
	delete(m.deletedProjects, projectID)
	m.projectGenerations[projectID]++
	m.mu.Unlock()
}
func (m *terminalManager) closeDetachedLater(id string, detachedAt time.Time) {
	time.AfterFunc(terminalDetachedTTL, func() {
		rec, ok := m.get(id)
		if !ok {
			return
		}
		rec.mu.Lock()
		shouldClose := !rec.closed && rec.subscriber == nil && rec.detachedAt.Equal(detachedAt)
		rec.mu.Unlock()
		if shouldClose {
			_ = m.close(id)
		}
	})
}

type terminalCreateRequest struct {
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

func (s *Server) createTerminal(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	p, err := s.getProjectByID(r.Context(), projectID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, 404, errors.New("project not found"))
		} else {
			writeError(w, 500, err)
		}
		return
	}
	var req terminalCreateRequest
	if !decodeOptional(w, r, &req) {
		return
	}
	if req.Cols == 0 {
		req.Cols = 120
	}
	if req.Rows == 0 {
		req.Rows = 36
	}
	rec, err := s.terminals.create(r.Context(), p, req.Cols, req.Rows)
	if err != nil {
		writeError(w, 409, err)
		return
	}
	writeJSON(w, 201, map[string]any{"id": rec.session.ID(), "projectId": projectID, "environment": rec.environment, "cwdDisplay": p.PathDisplay, "status": "starting", "createdAt": rec.createdAt})
}
func (s *Server) listTerminals(w http.ResponseWriter, r *http.Request) {
	pid := chi.URLParam(r, "projectID")
	s.terminals.mu.Lock()
	out := []map[string]any{}
	for _, rec := range s.terminals.sessions {
		if rec.projectID != pid {
			continue
		}
		rec.mu.Lock()
		out = append(out, map[string]any{"id": rec.session.ID(), "projectId": pid, "environment": rec.environment, "status": rec.state, "createdAt": rec.createdAt})
		rec.mu.Unlock()
	}
	s.terminals.mu.Unlock()
	writeJSON(w, 200, out)
}
func (s *Server) deleteTerminal(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "sessionID")
	rec, ok := s.terminals.get(sessionID)
	if !ok || rec.projectID != chi.URLParam(r, "projectID") {
		writeError(w, http.StatusNotFound, errors.New("terminal not found"))
		return
	}
	if err := s.terminals.close(sessionID); err != nil {
		writeError(w, 500, err)
		return
	}
	w.WriteHeader(204)
}

type terminalSequence uint64

// UnmarshalJSON accepts the legacy numeric form and the string form used by
// browsers, where JSON numbers cannot represent every uint64 exactly.
func (s *terminalSequence) UnmarshalJSON(data []byte) error {
	value := strings.TrimSpace(string(data))
	if len(value) >= 2 && value[0] == '"' {
		if err := json.Unmarshal(data, &value); err != nil {
			return err
		}
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return errors.New("afterSeq must be an unsigned integer")
	}
	*s = terminalSequence(parsed)
	return nil
}

type terminalControl struct {
	Type     string           `json:"type"`
	AfterSeq terminalSequence `json:"afterSeq"`
	Cols     uint16           `json:"cols"`
	Rows     uint16           `json:"rows"`
}

func (s *Server) terminalWebSocket(w http.ResponseWriter, r *http.Request) {
	const path = "/ws/projects/terminal"
	if !s.beginWebSocketSubscription(w) {
		return
	}
	defer s.websocketWG.Done()
	rec, ok := s.terminals.get(chi.URLParam(r, "sessionID"))
	if !ok || rec.projectID != chi.URLParam(r, "projectID") {
		writeError(w, 404, errors.New("terminal not found"))
		return
	}
	c, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c.SetReadLimit(terminalMaxInput + 8)
	if s.isClosing() {
		initiateWebSocketClose(c, websocket.CloseGoingAway, "server shutting down")
		waitForWebSocketClose(c)
		_ = c.Close()
		return
	}
	stopHeartbeat := startWebSocketHeartbeat(c, path)
	defer stopHeartbeat()
	sub := newTerminalSubscriber(c)
	writerDone := make(chan struct{})
	go sub.writeLoop(writerDone)
	defer func() {
		var detachedAt time.Time
		rec.mu.Lock()
		if rec.subscriber == sub {
			rec.subscriber = nil
			rec.detachedAt = time.Now()
			detachedAt = rec.detachedAt
		}
		rec.mu.Unlock()
		sub.close(websocket.CloseNormalClosure, "terminal detached")
		<-writerDone
		if !detachedAt.IsZero() {
			s.terminals.closeDetachedLater(rec.session.ID(), detachedAt)
		}
	}()
	typ, msg, err := c.ReadMessage()
	if err != nil {
		return
	}
	var ctl terminalControl
	if typ != websocket.TextMessage || json.Unmarshal(msg, &ctl) != nil || ctl.Type != "attach" {
		_ = sub.enqueue(terminalControlFrame(map[string]any{"type": "error", "code": "attach_required"}))
		return
	}
	// Startup is owned by terminalManager so an unattached session is also
	// reclaimed. This handler only waits for its one-shot result.
	<-rec.ready
	rec.mu.Lock()
	readyErr := rec.readyErr
	rec.mu.Unlock()
	if readyErr != nil {
		code := "start_failed"
		if errors.Is(readyErr, errTerminalStartupTimeout) {
			code = "start_timeout"
		}
		_ = sub.enqueue(terminalControlFrame(map[string]any{"type": "error", "code": code, "message": readyErr.Error()}))
		return
	}
	if ctl.Cols > 0 && ctl.Rows > 0 {
		_ = rec.session.Resize(ctl.Cols, ctl.Rows)
	}
	// Keep the manager lock while checking membership and assigning the
	// subscriber, so a concurrently closed/deleted terminal cannot be revived
	// through a handler that acquired its record before the close started.
	s.terminals.mu.Lock()
	if s.terminals.closing || s.terminals.sessions[rec.session.ID()] != rec {
		s.terminals.mu.Unlock()
		_ = sub.enqueue(terminalControlFrame(map[string]any{"type": "error", "code": "terminal_closed"}))
		return
	}
	rec.mu.Lock()
	chunks := append([]terminalChunk(nil), rec.replay...)
	state := rec.state
	currentSeq := rec.seq
	oldSub := rec.subscriber
	var exitCode *int
	if rec.exitCode != nil {
		code := *rec.exitCode
		exitCode = &code
	}
	truncated := terminalReplayTruncated(chunks, currentSeq, uint64(ctl.AfterSeq))
	for _, ch := range chunks {
		if ch.seq > uint64(ctl.AfterSeq) {
			if !sub.enqueue(terminalBinaryFrame(ch.seq, ch.data)) {
				rec.mu.Unlock()
				s.terminals.mu.Unlock()
				return
			}
		}
	}
	replayComplete := map[string]any{"type": "replay-complete", "seq": currentSeq}
	if truncated {
		replayComplete["truncated"] = true
	}
	if !sub.enqueue(terminalControlFrame(replayComplete)) {
		rec.mu.Unlock()
		s.terminals.mu.Unlock()
		return
	}
	readyMessage := map[string]any{"type": "ready", "environment": rec.environment, "status": state}
	if state == "exited" && exitCode != nil {
		readyMessage["code"] = *exitCode
	}
	if !sub.enqueue(terminalControlFrame(readyMessage)) {
		rec.mu.Unlock()
		s.terminals.mu.Unlock()
		return
	}
	rec.subscriber = sub
	rec.detachedAt = time.Time{}
	rec.mu.Unlock()
	s.terminals.mu.Unlock()
	if oldSub != nil {
		oldSub.close(websocket.CloseGoingAway, "terminal attached elsewhere")
	}
	for {
		typ, data, err := c.ReadMessage()
		if err != nil {
			return
		}
		if typ == websocket.TextMessage {
			var ctl terminalControl
			if json.Unmarshal(data, &ctl) != nil {
				continue
			}
			switch ctl.Type {
			case "resize":
				if ctl.Cols >= 1 && ctl.Cols <= 500 && ctl.Rows >= 1 && ctl.Rows <= 200 {
					_ = rec.session.Resize(ctl.Cols, ctl.Rows)
				}
			case "close":
				_ = s.terminals.close(rec.session.ID())
				return
			}
		} else if typ == websocket.BinaryMessage {
			if len(data) > terminalMaxInput+8 {
				continue
			}
			rec.mu.Lock()
			running := rec.state == "running"
			rec.mu.Unlock()
			if !running {
				_ = sub.enqueue(terminalControlFrame(map[string]any{"type": "error", "code": "not_ready"}))
				continue
			}
			if len(data) >= 8 && binary.LittleEndian.Uint64(data[:8]) == 0 {
				_, _ = rec.session.Write(data[8:])
			} else {
				_ = sub.enqueue(terminalControlFrame(map[string]any{"type": "error", "code": "invalid_input_frame"}))
			}
		}
	}
}

func terminalReplayTruncated(chunks []terminalChunk, currentSeq, afterSeq uint64) bool {
	if currentSeq <= afterSeq {
		return false
	}
	if len(chunks) == 0 {
		return true
	}
	return chunks[0].seq > 1 && afterSeq < chunks[0].seq-1
}
