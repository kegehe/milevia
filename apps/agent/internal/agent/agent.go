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
	// outboxRetryDelay keeps a failing local read from spinning.
	outboxRetryDelay = 2 * time.Second
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
	go func() { readErr <- a.readCommands(connectionCtx, conn, writeCh) }()
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
	shutdown := func() {
		cancel()
		_ = conn.Close()
		<-writeDone
		<-readErr
		<-snapshotDone
		<-outboxDone
	}
	ticker := time.NewTicker(750 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			shutdown()
			return nil
		case err := <-readErr:
			cancel()
			_ = conn.Close()
			<-writeDone
			<-snapshotDone
			<-outboxDone
			return err
		case err := <-writeErr:
			cancel()
			_ = conn.Close()
			<-writeDone
			<-readErr
			<-snapshotDone
			<-outboxDone
			return err
		case <-ticker.C:
			if !sendMessage(connectionCtx, writeCh, map[string]string{"kind": "ping"}) {
				shutdown()
				return connectionCtx.Err()
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

func (a *Agent) readCommands(ctx context.Context, conn *websocket.Conn, writeCh chan<- any) error {
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
					_ = a.localPost(ctx, "/api/remote/outbox/ack", map[string]any{"eventIds": []string{eventAck.EventID}}, nil)
				case "event.reject":
					// The cloud reported a durable conflict, so resending this
					// event can never succeed. Drop it instead of leaving it at
					// the head of the outbox, where it would block every event
					// created after it.
					log.Printf("cloud permanently rejected remote event %s: %s", eventAck.EventID, eventAck.Reason)
					_ = a.localPost(ctx, "/api/remote/outbox/fail", map[string]any{
						"eventIds":  []string{eventAck.EventID},
						"error":     eventAck.Reason,
						"permanent": true,
					}, nil)
				}
			}
			continue
		}
		if err := a.submitLocalCommand(ctx, cmd); err != nil {
			if !sendMessage(ctx, writeCh, commandStatus{Kind: "command.status", CommandID: cmd.CommandID, Status: "failed", Result: json.RawMessage(fmt.Sprintf(`{"error":%q}`, err.Error()))}) {
				return ctx.Err()
			}
			continue
		}
		if !sendMessage(ctx, writeCh, commandStatus{Kind: "command.status", CommandID: cmd.CommandID, Status: "received"}) {
			return ctx.Err()
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
	// acknowledgement. Only this goroutine touches it.
	var lastBatch string
	for {
		err := a.syncOutbox(ctx, writeCh, &lastBatch)
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
// Two forms of pacing keep the caller's loop off the local API: an immediately
// answered empty read is walked at outboxIdleFallback, and a batch that comes
// back unchanged is walked at outboxAckGrace. The change in behaviour is what
// pays for the long poll — one event now leaves the outbox in a single round
// trip instead of on a tick.
func (a *Agent) syncOutbox(ctx context.Context, writeCh chan<- any, lastBatch *string) error {
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
		if !sendMessage(ctx, writeCh, map[string]any{"kind": "event", "eventId": item.EventID, "agentSequence": item.AgentSequence, "instanceId": a.config.InstanceID, "type": item.Type, "taskId": item.TaskID, "taskRunId": item.TaskRunID, "payload": item.Payload, "createdAt": item.CreatedAt}) {
			return ctx.Err()
		}
	}
	if repeated {
		// These rows are still queued, so the cloud has not acknowledged them.
		// Hold off before reading them again.
		return sleepOrCancel(ctx, outboxAckGrace)
	}
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

func sendMessage(ctx context.Context, writeCh chan<- any, value any) bool {
	select {
	case writeCh <- value:
		return true
	case <-ctx.Done():
		return false
	}
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
