package app

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
	"golang.org/x/crypto/ssh"
)

const (
	// defaultTerminalMaxProjects / defaultTerminalMaxPerProject 是并发会话上限的
	// 默认值，可用 AUTO_TERMINAL_MAX_PROJECTS 与 AUTO_TERMINAL_MAX_PER_PROJECT
	// 覆盖。旧默认 12 / 3 对同时跑构建、调试与多个 Shell 的重负载用户偏紧，改为
	// 24 / 8 后仍按“每项目 + 全局”双上限约束资源占用。
	defaultTerminalMaxProjects   = 24
	defaultTerminalMaxPerProject = 8
	terminalMaxInput             = 64 << 10
	terminalMaxOutputFrame       = 256 << 10
	terminalMaxReplay            = 1 << 20
	terminalMaxQueue             = 1 << 20
	terminalDetachedTTL          = 60 * time.Second
	terminalStartupTimeout       = 10 * time.Second
	terminalShutdownWait         = 10 * time.Second
	// terminalElevationPromptTimeout 覆盖 UAC 弹窗的等待：ShellExecuteEx(runas)
	// 会阻塞到用户点允许/取消。启动一个"管理员终端"的 Open 使用该更长上限，
	// 普通终端仍用 terminalStartupTimeout。
	terminalElevationPromptTimeout = 2 * time.Minute
)

// terminalShellToken 是 Windows 目标终端允许的 Shell 枚举。协议只透传受限令牌，
// 具体可执行路径由各自平台实现解析，绝不让前端提交可执行路径。
var terminalShellTokens = map[string]bool{
	"cmd":        true,
	"powershell": true,
}

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
	// Shell 是受限的 Shell 令牌（"cmd"/"powershell"），仅对 Windows 目标有效；
	// 为空时各平台按自身默认值处理。可执行路径由平台实现解析，不由调用方提供。
	Shell string
	// RunAsAdmin 请求以管理员权限启动该会话。平台 Open 负责实际实现：
	// 控制服务已提权时子进程直接继承；未提权时经提权 bridge 承载。
	RunAsAdmin bool
	Cols       uint16
	Rows       uint16
}

// terminalLaunchOptions 是控制层（manager）把 HTTP 请求里的 shell/提权意图
// 传入 create 流程的载体，与平台 TerminalSpec 分开。
type terminalLaunchOptions struct {
	Shell      string
	RunAsAdmin bool
}

// terminalElevationProvider 由能确知自身子进程是否以管理员令牌运行的分层会话实现。
// 供 manager 在创建时记录 elevated 状态，透出给列表/创建响应。
type terminalElevationProvider interface{ TerminalElevated() bool }

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
	session                                       TerminalSession
	projectID, workspaceID, runnerID, environment string
	shell                                         string
	elevated                                      bool
	createdAt                                     time.Time
	mu                                            sync.Mutex
	state                                         string
	readyErr                                      error
	ready                                         chan struct{}
	seq                                           uint64
	replay                                        []terminalChunk
	subscriber                                    *terminalSubscriber
	closed                                        bool
	detachedAt                                    time.Time
	readerDone, waiterDone                        bool
	exitCode                                      *int
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

// maxProjects / maxPerProject 返回并发会话上限。未在配置中显式设置（<=0，含单测
// 直接构造的 &Server{}）时回退到包级默认值。
func (m *terminalManager) maxProjects() int {
	if m.server != nil && m.server.config.TerminalMaxProjects > 0 {
		return m.server.config.TerminalMaxProjects
	}
	return defaultTerminalMaxProjects
}
func (m *terminalManager) maxPerProject() int {
	if m.server != nil && m.server.config.TerminalMaxPerProject > 0 {
		return m.server.config.TerminalMaxPerProject
	}
	return defaultTerminalMaxPerProject
}

func (m *terminalManager) create(ctx context.Context, project Project, cols, rows uint16) (*terminalRecord, error) {
	return m.createInWorkspace(ctx, project, "project-shared:"+project.ID, cols, rows, terminalLaunchOptions{})
}

func (m *terminalManager) createInWorkspace(ctx context.Context, project Project, workspaceID string, cols, rows uint16, opts terminalLaunchOptions) (*terminalRecord, error) {
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
	shell := ""
	if target == agentTargetEnvWindows {
		shell = opts.Shell
		if shell == "" {
			shell = "cmd"
		}
		if !terminalShellTokens[shell] {
			return nil, fmt.Errorf("unsupported terminal shell %q", opts.Shell)
		}
	}
	// 提权只对"Windows 控制服务 + Windows 目标"有意义：WSL/SSH 会话各自有
	// sudo/远端权限模型，不能经由 UAC 提权；非 Windows 部署没有 runas 通道。
	if opts.RunAsAdmin && (runtime.GOOS != "windows" || target != agentTargetEnvWindows) {
		return nil, errors.New("以管理员身份运行仅支持 Windows 控制服务上的 Windows 项目终端")
	}
	spec := TerminalSpec{ProjectID: project.ID, RunnerID: project.RunnerID, Target: string(target), WorkDir: project.Path, WSLDistro: m.server.wslDistroName(), Shell: shell, RunAsAdmin: opts.RunAsAdmin, Cols: cols, Rows: rows}
	// A request context ends when the HTTP handler returns. It must not own a
	// terminal process, so Open only receives the manager lifetime and startup
	// deadline. Platform sessions detach their child process from this context.
	// UAC 弹窗会阻塞到用户点选，管理员会话用更长上限，普通会话仍用 startup 超时。
	openTimeout := m.startupTimeout
	if opts.RunAsAdmin {
		openTimeout = terminalElevationPromptTimeout
	}
	openCtx, cancelOpen := context.WithTimeout(m.shutdownCtx, openTimeout)
	defer cancelOpen()
	sess, err := m.factory.Open(openCtx, spec)
	if err != nil {
		return nil, err
	}
	createdAt := time.Now().UTC()
	r := &terminalRecord{session: sess, projectID: project.ID, workspaceID: workspaceID, runnerID: project.RunnerID, environment: string(target), shell: shell, createdAt: createdAt, state: "starting", ready: make(chan struct{}), detachedAt: createdAt}
	if target == agentTargetEnvWindows {
		if elevationProvider, ok := sess.(terminalElevationProvider); ok {
			r.elevated = elevationProvider.TerminalElevated()
		}
	}
	m.mu.Lock()
	if !m.leaseValidLocked(leaseID) || len(m.sessions) >= m.maxProjects() || m.projectSessionCountLocked(project.ID) >= m.maxPerProject() {
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
	if len(m.sessions)+len(m.pending) >= m.maxProjects() {
		return 0, fmt.Errorf("terminal session limit reached (max %d sessions across all projects)", m.maxProjects())
	}
	pendingForProject := 0
	for _, lease := range m.pending {
		if lease.projectID == project.ID {
			pendingForProject++
		}
	}
	if m.projectSessionCountLocked(project.ID)+pendingForProject >= m.maxPerProject() {
		return 0, fmt.Errorf("project terminal session limit reached (max %d concurrent sessions per project)", m.maxPerProject())
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
	// Shell 可选：windows 目标终端的受限 Shell 令牌（cmd/powershell），空则默认 cmd。
	Shell string `json:"shell"`
	// RunAsAdmin 请求以管理员权限运行该会话（仅 windows 目标终端可用）。
	RunAsAdmin bool `json:"runAsAdmin"`
}

// terminalSessionJSON 组装创建/列表响应的公共字段。
func terminalSessionJSON(rec *terminalRecord, projectID, cwdDisplay string) map[string]any {
	return map[string]any{
		"id":          rec.session.ID(),
		"projectId":   projectID,
		"workspaceId": rec.workspaceID,
		"environment": rec.environment,
		"shell":       rec.shell,
		"elevated":    rec.elevated,
		"cwdDisplay":  cwdDisplay,
		"status":      rec.state,
		"createdAt":   rec.createdAt,
	}
}

func (s *Server) createTerminal(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	// 只在整个生命周期锁内做工作区解析（保证看到一致的 project/workspace 快照），
	// 解析完成立即释放：createInWorkspace 可能阻塞在 UAC 授权（最长 2 分钟），
	// 不能把全局 projectLifecycleMu 握在手上，否则会阻塞其他项目的删除/会话操作。
	// 终端管理器自身的 lease + 代际校验负责与项目删除的竞态（list/delete 均不加此锁）。
	s.projectLifecycleMu.Lock()
	workspace, err := s.resolveRequestWorkspaceFromRequest(r)
	s.projectLifecycleMu.Unlock()
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, 404, errors.New("project not found"))
		} else {
			writeError(w, 500, err)
		}
		return
	}
	p := workspace.Project
	p.Path = workspace.Workspace.Path
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
	rec, err := s.terminals.createInWorkspace(r.Context(), p, workspace.Workspace.ID, req.Cols, req.Rows, terminalLaunchOptions{Shell: req.Shell, RunAsAdmin: req.RunAsAdmin})
	if err != nil {
		writeError(w, 409, err)
		return
	}
	// awaitReady 可能在响应序列化前把 state 从 starting 更新为 running/failed，
	// 因此序列化需与状态写入同锁，避免数据竞争。
	rec.mu.Lock()
	payload := terminalSessionJSON(rec, projectID, p.Path)
	rec.mu.Unlock()
	writeJSON(w, 201, payload)
}
func (s *Server) listTerminals(w http.ResponseWriter, r *http.Request) {
	pid := chi.URLParam(r, "projectID")
	workspace, err := s.resolveRequestWorkspaceFromRequest(r)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	s.terminals.mu.Lock()
	out := []map[string]any{}
	for _, rec := range s.terminals.sessions {
		if rec.projectID != pid || rec.workspaceID != workspace.Workspace.ID {
			continue
		}
		rec.mu.Lock()
		out = append(out, terminalSessionJSON(rec, pid, workspace.Workspace.Path))
		rec.mu.Unlock()
	}
	maxPerProject := s.terminals.maxPerProject()
	maxProjects := s.terminals.maxProjects()
	s.terminals.mu.Unlock()
	// 会话清单附带并发上限：界面禁用“新建”与计数都应以服务端配置为准，
	// 不再在前端硬编码“最多 3 个”。
	writeJSON(w, 200, map[string]any{
		"sessions":      out,
		"maxPerProject": maxPerProject,
		"maxProjects":   maxProjects,
	})
}
func (s *Server) deleteTerminal(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "sessionID")
	workspace, err := s.resolveRequestWorkspaceFromRequest(r)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	rec, ok := s.terminals.get(sessionID)
	if !ok || rec.projectID != chi.URLParam(r, "projectID") || rec.workspaceID != workspace.Workspace.ID {
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
	workspace, resolveErr := s.resolveRequestWorkspaceFromRequest(r)
	if resolveErr != nil {
		writeError(w, http.StatusConflict, resolveErr)
		return
	}
	rec, ok := s.terminals.get(chi.URLParam(r, "sessionID"))
	if !ok || rec.projectID != chi.URLParam(r, "projectID") || rec.workspaceID != workspace.Workspace.ID {
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
