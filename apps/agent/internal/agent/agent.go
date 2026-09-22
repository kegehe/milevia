package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type Config struct {
	InstanceID      string
	CloudURL        string
	CloudToken      string
	EnrollmentToken string
	CredentialFile  string
	LocalURL        string
	LocalURLFile    string
	LocalToken      string
}

func ConfigFromEnv() Config {
	return Config{InstanceID: strings.TrimSpace(getenv("MILEVIA_INSTANCE_ID")), CloudURL: strings.TrimRight(strings.TrimSpace(getenv("MILEVIA_CLOUD_URL")), "/"), CloudToken: getenv("MILEVIA_CLOUD_AGENT_TOKEN"), EnrollmentToken: getenv("MILEVIA_AGENT_ENROLLMENT_TOKEN"), CredentialFile: getenv("MILEVIA_AGENT_CREDENTIAL_FILE"), LocalURL: strings.TrimRight(strings.TrimSpace(getenvOr("MILEVIA_LOCAL_URL", "http://127.0.0.1:8080")), "/"), LocalURLFile: getenv("MILEVIA_LOCAL_URL_FILE"), LocalToken: getenv("AUTO_REMOTE_AGENT_TOKEN")}
}

func getenv(name string) string { return strings.TrimSpace(os.Getenv(name)) }
func getenvOr(name, fallback string) string {
	if value := getenv(name); value != "" {
		return value
	}
	return fallback
}

// New keeps construction testable while main injects environment through the
// standard lookup below.
func New(config Config) *Agent {
	// Snapshot uploads can contain the full project/task view and may cross a
	// busy reverse-proxy or database flush. Keep the request bounded, but allow
	// enough time for a transiently slow cloud response.
	return &Agent{config: config, client: &http.Client{Timeout: 60 * time.Second}}
}

type Agent struct {
	config                  Config
	client                  *http.Client
	mu                      sync.Mutex
	conn                    *websocket.Conn
	lastSnapshotFingerprint string
	lastSnapshotSequence    int64
	lastSnapshotUploadAt    time.Time
	snapshotReady           bool
	// credMu guards InstanceID/CloudToken: re-enrollment rewrites them while
	// the snapshot and outbox goroutines read them concurrently.
	credMu sync.RWMutex
	// reEnrollMu serializes re-enrollment attempts and enforces a cooldown so
	// a burst of rejections cannot trigger a registration storm.
	reEnrollMu     sync.Mutex
	lastReEnrollAt time.Time
	// rpcSlots 是中继请求（文件与 Git）的并发闸门（见 rpc_relay.go）。懒初始化，好让直接构造
	// Agent 的测试不必先跑一遍 New。
	rpcSlots     chan struct{}
	rpcSlotsOnce sync.Once
}

// reEnrollCooldown bounds how often a rejected credential may be replaced.
const reEnrollCooldown = 30 * time.Second

// Full snapshot uploads are throttled. One AI reply advances the event
// sequence many times, and each advance would otherwise push a complete
// project/history payload to the cloud — and from there to every phone. Live
// updates already travel over the event stream, so the snapshot only has to
// converge promptly, not instantly. A burst of events (>= the outstanding
// threshold) still uploads immediately, keeping large changes responsive.
const (
	snapshotMinUploadInterval    = 3 * time.Second
	snapshotMinOutstandingEvents = 5
)

// Outbox long polling. The local control server holds /api/remote/outbox open
// until the outbox gains a row, so a new event reaches the relay in one round
// trip rather than on a poll tick.
const (
	// outboxLongPollSeconds matches the server-side cap. Staying under the
	// Agent's own HTTP client timeout keeps the held request from being the
	// thing that trips it.
	outboxLongPollSeconds = 20
	// outboxIdleFallback paces the loop when the local server answers at once
	// instead of holding the request — either it predates long polling, or it
	// had nothing to wait for. Without this the loop would busy-poll the local
	// API, which is exactly what the old fixed ticker existed to prevent.
	outboxIdleFallback = 200 * time.Millisecond
	// outboxAckGrace paces the loop while the rows it just read are still in
	// the outbox because the cloud has not acknowledged them yet. Rows are only
	// deleted by /api/remote/outbox/ack, so without this pause the long poll
	// hands back the same batch as fast as the local database answers and the
	// relay re-sends every row on each pass. Re-sending is kept deliberately
	// (rather than skipped) because it is also the recovery path for an
	// acknowledgement lost in flight.
	outboxAckGrace = 200 * time.Millisecond
	// outboxAckStallThreshold 是一批事件"原样退回来"多久之后才算真的卡住。
	//
	// 它存在的唯一目的是把正常路径和故障路径分开：回执是攒 250ms 成批提交的，所以
	// 每次发完读第二遍时那批行必然还在 outbox 里（这正是"重复"的常见来源，与故障
	// 无关）。一秒钟之内不升级退避，正常收发的额外等待就只有 outboxAckGrace 本身。
	outboxAckStallThreshold = time.Second
	// outboxAckBackoffMaxShift bounds the exponential backoff applied once a
	// batch has been stalled that long: 200ms << 5 = 6.4s.
	//
	// 一个固定 200ms 的等待在正常情况下就够用了（云端确认一到，行就被删掉，
	// 下一轮读到的是新的一批）。但真机上出现过队列里有一批**永远确认不掉**的行
	// （云端既不发 ack 也不发 reject，见 cloud-control 对 storeEvent 错误的分类），
	// 这时固定 200ms 就变成"每秒把同一批事件重推 5 次"：云端对每一条重发再回一次
	// ack，agent 再确认一次。实测一条连接 4.5 小时被灌了 792 MB 下行，绝大部分是
	// 这种对同一批老事件的重复回执，读循环也被它拖住。
	//
	// 退避不会拖慢正常投递：只要云端在确认，行就会被删掉，批次内容随之改变，
	// 计时随即归零。只有真的卡住的那批才会越等越久。
	outboxAckBackoffMaxShift = 5
	// outboxRetryDelay keeps a failing local read from spinning.
	outboxRetryDelay = 2 * time.Second
)

// 关连接与守活。老实现里这几个等待都没有上限，真机上出现过 agent 卡死在里面：
// 进程活着、零个 TCP 连接、CPU 0%、日志停在断线那一刻，`Run` 的重连循环再也
// 没有跑过一次（2026-09-17 07:35 之后一直如此）。
const (
	// agentShutdownGrace 是关连接时等每个后台 goroutine 收尾的上限。
	agentShutdownGrace = 15 * time.Second
	// agentWriteQueueStallTimeout 是心跳能容忍的写队列拥塞上限。超过它说明连接
	// 实际已经卡住（对端不读，或 writer 卡在一次写里），宁可断开重连。
	agentWriteQueueStallTimeout = 20 * time.Second
)

// credentials returns the current machine credential under the read lock.
func (a *Agent) credentials() (string, string) {
	a.credMu.RLock()
	defer a.credMu.RUnlock()
	return a.config.InstanceID, a.config.CloudToken
}

// setCredentials replaces the machine credential under the write lock.
func (a *Agent) setCredentials(instanceID, token string) {
	a.credMu.Lock()
	defer a.credMu.Unlock()
	a.config.InstanceID, a.config.CloudToken = instanceID, token
}

// reEnroll discards a credential the cloud has rejected and registers again.
// The WebSocket handshake is not the only place a credential is presented —
// snapshot uploads use plain HTTP — so both paths funnel through here.
func (a *Agent) reEnroll(ctx context.Context) error {
	if strings.TrimSpace(a.config.EnrollmentToken) == "" {
		return errors.New("no enrollment token is available to re-register this machine")
	}
	a.reEnrollMu.Lock()
	defer a.reEnrollMu.Unlock()
	if !a.lastReEnrollAt.IsZero() && time.Since(a.lastReEnrollAt) < reEnrollCooldown {
		return errors.New("re-enrollment is cooling down after a recent attempt")
	}
	a.lastReEnrollAt = time.Now()
	a.setCredentials("", "")
	if a.config.CredentialFile != "" {
		_ = os.Remove(a.config.CredentialFile)
	}
	if err := a.register(ctx); err != nil {
		return err
	}
	// The credential is already durable at this point. Publishing only informs
	// the desktop UI, and the connect path retries it, so a failure here must
	// not masquerade as a failed re-enrollment.
	if err := a.publishCredentials(ctx); err != nil {
		log.Printf("publish re-enrolled credentials to local control server: %v", err)
	}
	return nil
}

type outboxItem struct {
	EventID       string          `json:"eventId"`
	AgentSequence int64           `json:"agentSequence"`
	Type          string          `json:"type"`
	TaskID        string          `json:"taskId"`
	TaskRunID     string          `json:"taskRunId,omitempty"`
	Payload       json.RawMessage `json:"payload"`
	CreatedAt     time.Time       `json:"createdAt"`
}
type command struct {
	CommandID      string          `json:"commandId"`
	Type           string          `json:"type"`
	ProjectID      string          `json:"projectId,omitempty"`
	TaskID         string          `json:"taskId,omitempty"`
	IdempotencyKey string          `json:"idempotencyKey"`
	Payload        json.RawMessage `json:"payload,omitempty"`
	ExpiresAt      time.Time       `json:"expiresAt"`
}
type commandStatus struct {
	Kind      string          `json:"kind"`
	CommandID string          `json:"commandId"`
	Status    string          `json:"status"`
	Result    json.RawMessage `json:"result,omitempty"`
}

func (a *Agent) Run(ctx context.Context) error {
	if a.config.CloudURL == "" {
		return errors.New("cloud URL is required")
	}
	a.loadStoredCredential()
	if instanceID, cloudToken := a.credentials(); instanceID == "" || cloudToken == "" {
		if err := a.register(ctx); err != nil {
			return fmt.Errorf("agent registration failed: %w", err)
		}
	}
	if err := a.publishCredentials(ctx); err != nil {
		log.Printf("publish agent credentials to local control server: %v", err)
	}
	backoff := time.Second
	for {
		if err := a.runConnection(ctx); err != nil && ctx.Err() == nil {
			log.Printf("agent connection lost: %v", err)
			backoff = minDuration(backoff*2, 30*time.Second)
		} else {
			backoff = time.Second
		}
		if ctx.Err() != nil {
			return nil
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func (a *Agent) publishCredentials(ctx context.Context) error {
	cloudURL, err := cloudHTTPURL(a.config.CloudURL)
	if err != nil {
		return err
	}
	instanceID, cloudToken := a.credentials()
	if instanceID == "" || cloudToken == "" {
		return errors.New("agent credentials are not available yet")
	}
	return a.localPost(ctx, "/api/remote/credentials", map[string]string{
		"cloudUrl":   cloudURL,
		"instanceId": instanceID,
		"agentToken": cloudToken,
	}, nil)
}

func (a *Agent) loadStoredCredential() {
	if a.config.CredentialFile == "" {
		return
	}
	credential, err := loadCredentialFile(a.config.CredentialFile)
	if err != nil {
		return
	}
	instanceID, token := a.credentials()
	if instanceID == "" {
		instanceID = credential.InstanceID
	}
	if token == "" {
		token = credential.AgentToken
	}
	a.setCredentials(instanceID, token)
}

func (a *Agent) register(ctx context.Context) error {
	if strings.TrimSpace(a.config.EnrollmentToken) == "" {
		return errors.New("instance ID and cloud token are required, or provide an enrollment token")
	}
	endpoint, err := cloudHTTPURL(a.config.CloudURL)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/v1/agent/register", strings.NewReader(`{"name":"Milevia computer"}`))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Milevia-Enrollment-Token", a.config.EnrollmentToken)
	response, err := a.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("cloud returned %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var value struct {
		InstanceID string `json:"instanceId"`
		AgentToken string `json:"agentToken"`
	}
	if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
		return err
	}
	if value.InstanceID == "" || value.AgentToken == "" {
		return errors.New("cloud returned incomplete agent credentials")
	}
	a.setCredentials(value.InstanceID, value.AgentToken)
	if a.config.CredentialFile != "" {
		if err := saveCredentialFile(a.config.CredentialFile, storedCredential{
			InstanceID: value.InstanceID,
			AgentToken: value.AgentToken,
		}); err != nil {
			return fmt.Errorf("store agent credentials: %w", err)
		}
	}
	return nil
}

func (a *Agent) runConnection(ctx context.Context) error {
	instanceID, cloudToken := a.credentials()
	endpoint, err := cloudWebSocketEndpoint(a.config.CloudURL, instanceID)
	if err != nil {
		return err
	}
	header := http.Header{"X-Milevia-Agent-Token": []string{cloudToken}}
	conn, response, err := websocket.DefaultDialer.DialContext(ctx, endpoint, header)
	if err != nil {
		if response != nil && response.StatusCode == http.StatusUnauthorized {
			// The cloud may have revoked this machine's credential. Discard the
			// stale secret and enroll again so the next connection uses a fresh
			// instance-scoped token.
			if reErr := a.reEnroll(ctx); reErr != nil {
				return fmt.Errorf("cloud rejected agent credentials (%w); re-enrollment failed: %v", err, reErr)
			}
			return errors.New("re-enrolled after the cloud rejected the previous credential")
		}
		return err
	}
	a.mu.Lock()
	a.conn = conn
	a.mu.Unlock()
	defer func() { a.mu.Lock(); a.conn = nil; a.mu.Unlock(); _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(45 * time.Second))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(45 * time.Second)) })
	connectionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	acks := newOutboxAckQueue()
	writeCh := make(chan any, 128)
	writeDone := make(chan struct{})
	writeErr := make(chan error, 1)
	go func() {
		defer close(writeDone)
		for {
			select {
			case <-connectionCtx.Done():
				return
			case message := <-writeCh:
				_ = conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
				if err := conn.WriteJSON(message); err != nil {
					writeErr <- err
					_ = conn.Close()
					return
				}
			}
		}
	}()
	readErr := make(chan error, 1)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		readErr <- a.readCommands(connectionCtx, conn, writeCh, acks)
	}()
	// 云端回执的落地单独跑一个 goroutine 成批提交。放在读循环里同步发 HTTP 的话，
	// 事件洪峰期间读循环会把全部时间花在确认上，下行的命令挤不进来（见 outbox_ack.go）。
	ackDone := make(chan struct{})
	go func() {
		defer close(ackDone)
		a.runOutboxAckQueue(connectionCtx, acks)
	}()
	// Snapshots are recovery data, never part of the command/event hot path.
	// Coalesce tick requests so a slow local SQLite read cannot delay WSS
	// commands, command status, or outbox events.
	snapshotWake := make(chan struct{}, 1)
	snapshotDone := make(chan struct{})
	go func() {
		defer close(snapshotDone)
		for {
			select {
			case <-connectionCtx.Done():
				return
			case <-snapshotWake:
				if err := a.syncSnapshot(connectionCtx); err != nil && connectionCtx.Err() == nil {
					log.Printf("snapshot sync failed: %v", err)
				}
			}
		}
	}()
	wakeSnapshot := func() {
		select {
		case snapshotWake <- struct{}{}:
		default:
		}
	}
	// Outbox reads touch the local SQLite-backed HTTP API and can briefly wait
	// behind a writer. Keep that wait away from the WebSocket control loop so a
	// slow local read cannot delay pings, disconnect handling, or commands.
	//
	// The read is a long poll: the local server holds the request until the
	// outbox actually gains a row, so an event reaches the relay in one round
	// trip instead of waiting out a poll tick.
	outboxDone := make(chan struct{})
	go func() {
		defer close(outboxDone)
		a.runOutboxPump(connectionCtx, writeCh)
	}()
	// Re-publish on every (re)connect. The local control server keeps these
	// credentials in process memory only, so a publish that failed during the
	// first connection attempt (local server still starting, port file not yet
	// readable) would otherwise leave the desktop UI reporting "agent not
	// ready" until the whole desktop app restarts.
	if err := a.publishCredentials(connectionCtx); err != nil && connectionCtx.Err() == nil {
		log.Printf("publish agent credentials to local control server: %v", err)
	}
	if err := a.localPost(connectionCtx, "/api/remote/status", map[string]string{"status": "online"}, nil); err != nil {
		log.Printf("mark local remote status online: %v", err)
	}
	wakeSnapshot()
	// 关连接时每个后台 goroutine 都要被等到，否则 runConnection 永远返回不了，
	// Run 的重连循环也就再也不会跑一次 —— 真机上出现过这个状态（进程活着、零个
	// TCP 连接、CPU 0%、日志停在断线那一刻）。所以这些等待一律带超时：宁可丢掉
	// 一个卡住的 goroutine 重新连一次，也不能让整台电脑从此失联。
	//
	// 五个等待**共用一个期限**，不是各给一份：各给 15 秒的话最坏就是 75 秒，
	// 把重连拖到一分多钟 —— 那和"卡死"在用户眼里差不多。正常路径上五个 channel
	// 都是微秒级关闭，这个期限只在真出问题时才起作用。
	shutdown := func() {
		cancel()
		_ = conn.Close()
		deadline := time.Now().Add(agentShutdownGrace)
		waitForBackground("websocket writer", writeDone, deadline)
		waitForBackground("command reader", readDone, deadline)
		waitForBackground("ack flusher", ackDone, deadline)
		waitForBackground("snapshot sync", snapshotDone, deadline)
		waitForBackground("outbox pump", outboxDone, deadline)
	}
	ticker := time.NewTicker(750 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			shutdown()
			return nil
		case err := <-readErr:
			shutdown()
			return err
		case err := <-writeErr:
			shutdown()
			return err
		case <-ticker.C:
			// 写队列灌满说明连接实际已经卡住（对端不读，或者 writer 卡在一次
			// 写里）。老实现会一直阻塞在 sendMessage 上：既不报错也不重连，
			// 整个 agent 就此静默。宁可断开重连。
			if !sendMessageWithin(connectionCtx, writeCh, map[string]string{"kind": "ping"}, agentWriteQueueStallTimeout) {
				shutdown()
				return agentWriteStallError(connectionCtx)
			}
			wakeSnapshot()
		}
	}
}

func cloudWebSocketEndpoint(rawURL, instanceID string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	switch parsed.Scheme {
	case "https":
		parsed.Scheme = "wss"
	case "http":
		parsed.Scheme = "ws"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("unsupported cloud URL scheme %q", parsed.Scheme)
	}
	endpoint, err := url.JoinPath(parsed.String(), "/v1/agent/connect")
	if err != nil {
		return "", err
	}
	return endpoint + "?instanceId=" + url.QueryEscape(instanceID), nil
}

func (a *Agent) syncSnapshot(ctx context.Context) error {
	// The overview endpoint is a single-row, cheap query. Avoid rebuilding the
	// full project snapshot on every heartbeat when no local event advanced the
	// sequence. This matters on large SQLite databases where snapshot assembly
	// is substantially more expensive than the relay heartbeat itself.
	var overview struct {
		LastAgentSequence int64 `json:"lastAgentSequence"`
	}
	overviewOK := a.localGet(ctx, "/api/remote/overview", &overview) == nil
	if overviewOK {
		if a.snapshotReady && overview.LastAgentSequence == a.lastSnapshotSequence {
			return nil
		}
		// Throttle the steady drip of full snapshots during a long reply; a
		// larger backlog uploads right away.
		if a.snapshotReady &&
			overview.LastAgentSequence-a.lastSnapshotSequence < snapshotMinOutstandingEvents &&
			time.Since(a.lastSnapshotUploadAt) < snapshotMinUploadInterval {
			return nil
		}
	}
	var snapshot json.RawMessage
	if err := a.localGet(ctx, "/api/remote/snapshot", &snapshot); err != nil {
		return err
	}
	sequence := overview.LastAgentSequence
	if !overviewOK {
		sequence = snapshotRevision(snapshot)
	}
	fingerprint := snapshotFingerprint(snapshot)
	if fingerprint != "" && fingerprint == a.lastSnapshotFingerprint {
		a.lastSnapshotSequence = sequence
		a.snapshotReady = true
		return nil
	}
	cloudHTTP, err := cloudHTTPURL(a.config.CloudURL)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, cloudHTTP+"/v1/agent/snapshot", bytes.NewReader(snapshot))
	if err != nil {
		return err
	}
	instanceID, cloudToken := a.credentials()
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Milevia-Agent-Token", cloudToken)
	request.Header.Set("X-Milevia-Instance-ID", instanceID)
	response, err := a.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized {
		// Snapshot uploads use plain HTTP, so they never pass through the
		// WebSocket handshake that normally detects a revoked credential.
		// Re-enroll here too; otherwise uploads would fail forever while the
		// relay connection appeared healthy.
		if reErr := a.reEnroll(ctx); reErr != nil {
			return fmt.Errorf("cloud rejected agent credentials (401); re-enrollment failed: %w", reErr)
		}
		return errors.New("snapshot upload skipped: credentials were re-enrolled")
	}
	if response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("cloud snapshot upload failed (%d): %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	a.lastSnapshotFingerprint = fingerprint
	a.lastSnapshotSequence = sequence
	a.lastSnapshotUploadAt = time.Now()
	a.snapshotReady = true
	return nil
}

func snapshotRevision(snapshot []byte) int64 {
	var value struct {
		SnapshotRevision int64 `json:"snapshotRevision"`
	}
	if err := json.Unmarshal(snapshot, &value); err != nil {
		return 0
	}
	return value.SnapshotRevision
}

// remote snapshots include an observation timestamp that changes on every
// read. Exclude that volatile field so idle desktops do not upload the same
// full project/history payload every heartbeat.
func snapshotFingerprint(snapshot []byte) string {
	var value map[string]json.RawMessage
	if err := json.Unmarshal(snapshot, &value); err != nil {
		return ""
	}
	delete(value, "observedAt")
	canonical, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(canonical)
	return fmt.Sprintf("%x", digest[:])
}

func cloudHTTPURL(rawURL string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	switch parsed.Scheme {
	case "wss":
		parsed.Scheme = "https"
	case "ws":
		parsed.Scheme = "http"
	case "http", "https":
	default:
		return "", fmt.Errorf("unsupported cloud URL scheme %q", parsed.Scheme)
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func (a *Agent) readCommands(ctx context.Context, conn *websocket.Conn, writeCh chan<- any, acks *outboxAckQueue) error {
	for {
		var raw json.RawMessage
		if err := conn.ReadJSON(&raw); err != nil {
			return err
		}
		var kind struct {
			Kind string `json:"kind"`
		}
		_ = json.Unmarshal(raw, &kind)
		if kind.Kind == "pong" {
			_ = conn.SetReadDeadline(time.Now().Add(45 * time.Second))
			continue
		}
		// 中继请求必须在 `var cmd command` 之前处理：下面那段会把"没有 commandId 的帧"
		// 当成事件回执解析，而 rpc.request 既没有 commandId 也没有 eventId —— 它会被
		// 静默丢弃，手机端只会看到超时。
		if kind.Kind == "rpc.request" {
			var request rpcRequest
			if json.Unmarshal(raw, &request) == nil && request.RequestID != "" {
				if err := conn.SetReadDeadline(time.Now().Add(45 * time.Second)); err != nil {
					return err
				}
				a.dispatchRPCRequest(ctx, request, writeCh)
			}
			continue
		}
		var cmd command
		if json.Unmarshal(raw, &cmd) != nil || cmd.CommandID == "" {
			var eventAck struct {
				Kind    string `json:"kind"`
				EventID string `json:"eventId"`
				Reason  string `json:"reason"`
			}
			if json.Unmarshal(raw, &eventAck) == nil && eventAck.EventID != "" {
				switch eventAck.Kind {
				case "event.ack":
					// 只入队，不在这里发 HTTP：本地确认一条一次请求会把读循环
					// 占死，下行的命令就挤不进来了（见 outbox_ack.go）。
					acks.enqueue(outboxAck{eventID: eventAck.EventID})
				case "event.reject":
					// The cloud reported a durable conflict, so resending this
					// event can never succeed. Drop it instead of leaving it at
					// the head of the outbox, where it would block every event
					// created after it.
					log.Printf("cloud permanently rejected remote event %s: %s", eventAck.EventID, eventAck.Reason)
					acks.enqueue(outboxAck{eventID: eventAck.EventID, reason: eventAck.Reason, drop: true})
				}
			}
			continue
		}
		if err := a.submitLocalCommand(ctx, cmd); err != nil {
			// 状态回执也带超时：写队列被事件灌满时，老实现会无限期阻塞在读循环里 ——
			// 读循环一停，新的命令就再也读不进来，整条下行链路静默。宁可断开重连。
			if !sendMessageWithin(ctx, writeCh, commandStatus{Kind: "command.status", CommandID: cmd.CommandID, Status: "failed", Result: json.RawMessage(fmt.Sprintf(`{"error":%q}`, err.Error()))}, agentWriteQueueStallTimeout) {
				return agentWriteStallError(ctx)
			}
			continue
		}
		if !sendMessageWithin(ctx, writeCh, commandStatus{Kind: "command.status", CommandID: cmd.CommandID, Status: "received"}, agentWriteQueueStallTimeout) {
			return agentWriteStallError(ctx)
		}
		go a.monitorCommand(ctx, cmd.CommandID, writeCh)
	}
}

func (a *Agent) submitLocalCommand(ctx context.Context, cmd command) error {
	body := mustJSON(cmd)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.localURL()+"/api/remote/commands", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Milevia-Agent-Token", a.config.LocalToken)
	response, err := a.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("local command rejected (%d): %s", response.StatusCode, strings.TrimSpace(string(data)))
	}
	return nil
}

func (a *Agent) monitorCommand(ctx context.Context, commandID string, writeCh chan<- any) {
	check := func() (bool, error) {
		var value struct {
			Status string          `json:"status"`
			Result json.RawMessage `json:"result"`
		}
		if err := a.localGet(ctx, "/api/remote/commands/"+url.PathEscape(commandID), &value); err != nil {
			return false, err
		}
		if value.Status == "queued" || value.Status == "received" || value.Status == "executing" {
			return false, nil
		}
		switch value.Status {
		case "completed", "failed", "expired", "cancelled", "indeterminate":
		default:
			return false, nil
		}
		return sendMessage(ctx, writeCh, commandStatus{Kind: "command.status", CommandID: commandID, Status: value.Status, Result: value.Result}), nil
	}
	// The local command worker is woken as soon as a command arrives. Query
	// immediately so the common fast path does not wait for the first ticker.
	if done, _ := check(); done {
		return
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			done, err := check()
			if err != nil {
				continue
			}
			if done {
				return
			}
		}
	}
}

// runOutboxPump forwards queued events to the relay for as long as the
// connection lives.
//
// The read is a long poll, so in the steady state this costs one open request
// and no traffic. The pacing lives in syncOutbox, which is what keeps the loop
// from becoming a busy poll against the local API.
func (a *Agent) runOutboxPump(ctx context.Context, writeCh chan<- any) {
	// lastBatch is the signature of the previous read, so the pump can tell a
	// genuinely new batch from one that is still waiting for its
	// acknowledgement. batchStalledSince records when that batch first came back
	// unchanged, which is what drives the backoff in syncOutbox.
	// Only this goroutine touches either of them.
	var lastBatch string
	var batchStalledSince time.Time
	for {
		err := a.syncOutbox(ctx, writeCh, &lastBatch, &batchStalledSince)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			continue
		}
		log.Printf("event sync failed: %v", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(outboxRetryDelay):
		}
	}
}

// syncOutbox forwards the pending outbox rows to the relay, then returns.
//
// Three forms of pacing keep the caller's loop off the local API: an immediately
// answered empty read is walked at outboxIdleFallback, a batch that comes back
// unchanged is walked at outboxAckGrace with an exponential backoff on top
// (outboxAckBackoffMaxShift), and a failing read is walked by the caller.
func (a *Agent) syncOutbox(ctx context.Context, writeCh chan<- any, lastBatch *string, batchStalledSince *time.Time) error {
	var items []outboxItem
	started := time.Now()
	// The wait parameter asks the local server to hold this request open until
	// the outbox changes. A local server too old to know the parameter simply
	// answers immediately, which the pacing below absorbs.
	if err := a.localGet(ctx, fmt.Sprintf("/api/remote/outbox?limit=100&wait=%d", outboxLongPollSeconds), &items); err != nil {
		return err
	}
	if len(items) == 0 {
		*lastBatch = ""
		*batchStalledSince = time.Time{}
		// An empty answer that came back instantly means the request was not
		// held. Pace the next attempt so this loop cannot become a busy poll.
		if time.Since(started) < time.Second {
			return sleepOrCancel(ctx, outboxIdleFallback)
		}
		return nil
	}
	signature := outboxBatchSignature(items)
	repeated := signature == *lastBatch
	*lastBatch = signature
	for _, item := range items {
		instanceID, _ := a.credentials()
		if !sendMessage(ctx, writeCh, map[string]any{"kind": "event", "eventId": item.EventID, "agentSequence": item.AgentSequence, "instanceId": instanceID, "type": item.Type, "taskId": item.TaskID, "taskRunId": item.TaskRunID, "payload": item.Payload, "createdAt": item.CreatedAt}) {
			return ctx.Err()
		}
	}
	if repeated {
		// These rows are still queued, so the cloud has not acknowledged them.
		// Hold off before reading them again, and back off further the longer the
		// same batch keeps coming back: a row the cloud can never acknowledge (it
		// is neither accepted nor rejected — see cloud-control's error
		// classification) would otherwise keep this loop re-sending the same
		// hundred events five times a second for as long as the connection lives.
		//
		// 退避的判据是"这批原样退回来**多久了**"，不是"重复了几轮"。轮次判据在
		// 正常路径上也会误伤：回执现在是攒 250ms 成批提交的，所以泵读第二遍时那批
		// 行**必然**还没被删掉 —— 按轮次算的话，一次普通的收发就会白白多等几百毫秒。
		// 按时间算则只在真的卡住（超过 outboxAckStallThreshold）之后才开始拉长。
		if batchStalledSince.IsZero() {
			*batchStalledSince = time.Now()
		}
		stalled := time.Since(*batchStalledSince)
		shift := 0
		if stalled > outboxAckStallThreshold {
			shift = min(int(stalled/outboxAckStallThreshold), outboxAckBackoffMaxShift)
		}
		return sleepOrCancel(ctx, outboxAckGrace<<shift)
	}
	*batchStalledSince = time.Time{}
	return nil
}

// outboxBatchSignature identifies a read batch by the ids the server returned,
// which are ordered by agent_sequence.
func outboxBatchSignature(items []outboxItem) string {
	var builder strings.Builder
	for _, item := range items {
		builder.WriteString(item.EventID)
		builder.WriteByte('\n')
	}
	return builder.String()
}

// sleepOrCancel waits for d and reports the context error if it ended first.
func sleepOrCancel(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// waitForBackground 等到 done 关闭、或到期限为止，返回是否等到了。
//
// 期限是**绝对时刻**而不是时长：一次收尾要等五个后台 goroutine，共用一个期限
// 总等待才有上界；各给一份的话总时间随数量线性增长（五个各 15 秒 = 75 秒），
// 把重连拖到用户以为是卡死。正常路径上五个 channel 都是微秒级关闭，期限只在
// 真出问题时才起作用。
func waitForBackground(name string, done <-chan struct{}, deadline time.Time) bool {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		log.Printf("agent connection shutdown: skipping %s, the shutdown grace is already spent", name)
		return false
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		log.Printf("agent connection shutdown: %s did not stop in time; abandoning it and reconnecting", name)
		return false
	}
}

func sendMessage(ctx context.Context, writeCh chan<- any, value any) bool {
	select {
	case writeCh <- value:
		return true
	case <-ctx.Done():
		return false
	}
}

// sendMessageWithin 是 sendMessage 的带超时版本：写队列一时排不下是正常的，
// 排不下超过 timeout 则说明连接已经卡死，调用方应当断开重连而不是继续等。
func sendMessageWithin(ctx context.Context, writeCh chan<- any, value any, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case writeCh <- value:
		return true
	case <-ctx.Done():
		return false
	case <-timer.C:
		return false
	}
}

// agentWriteStallError 把"写队列排不下"翻译成读循环该返回的错误：
// 连接正常结束时返回 ctx 的错误（不触发重连日志噪声），否则报写队列卡死。
func agentWriteStallError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New("agent relay write queue stalled")
}

func (a *Agent) localGet(ctx context.Context, path string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, a.localURL()+path, nil)
	if err != nil {
		return err
	}
	request.Header.Set("X-Milevia-Agent-Token", a.config.LocalToken)
	response, err := a.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		return errors.New("local request failed")
	}
	return json.NewDecoder(response.Body).Decode(target)
}
func (a *Agent) localPost(ctx context.Context, path string, body any, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.localURL()+path, bytes.NewReader(mustJSON(body)))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Milevia-Agent-Token", a.config.LocalToken)
	response, err := a.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		return errors.New("local request failed")
	}
	if target != nil {
		return json.NewDecoder(response.Body).Decode(target)
	}
	return nil
}

// localURL follows the desktop control server's dynamically assigned loopback
// port. The fallback keeps standalone Agent usage compatible with older setups.
func (a *Agent) localURL() string {
	if path := strings.TrimSpace(a.config.LocalURLFile); path != "" {
		if value, err := os.ReadFile(path); err == nil {
			if endpoint := strings.TrimRight(strings.TrimSpace(string(value)), "/"); endpoint != "" {
				return endpoint
			}
		}
	}
	return a.config.LocalURL
}
func mustJSON(value any) []byte { data, _ := json.Marshal(value); return data }
func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
