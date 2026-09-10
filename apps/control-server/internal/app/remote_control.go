package app

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// RemoteInstance is the stable identity and health view sent to the cloud.
type RemoteInstance struct {
	InstanceID        string    `json:"instanceId"`
	Status            string    `json:"status"`
	LastAgentSequence int64     `json:"lastAgentSequence"`
	ObservedAt        time.Time `json:"observedAt"`
}

type remoteOutboxItem struct {
	EventID       string          `json:"eventId"`
	AgentSequence int64           `json:"agentSequence"`
	Type          string          `json:"type"`
	TaskID        string          `json:"taskId"`
	TaskRunID     string          `json:"taskRunId,omitempty"`
	Payload       json.RawMessage `json:"payload"`
	CreatedAt     time.Time       `json:"createdAt"`
}

// maxRemoteOutboxAttempts bounds delivery retries for a single event. Reaching
// it removes the event from the delivery window (it stays in the table as a
// durable record and is reported in the log), which is what stops one
// permanently undeliverable event from blocking every event behind it.
const maxRemoteOutboxAttempts = 20

// remoteCommandTimeout bounds one remote command's execution. The command
// worker is intentionally serial, so an unbounded execution would stall the
// whole remote queue.
const remoteCommandTimeout = 60 * time.Second

type remoteSnapshot struct {
	SnapshotRevision int64                   `json:"snapshotRevision"`
	ObservedAt       time.Time               `json:"observedAt"`
	Projects         []remoteSnapshotProject `json:"projects"`
}

type remoteSnapshotProject struct {
	ID            string                       `json:"id"`
	Name          string                       `json:"name"`
	Runner        string                       `json:"runner"`
	Environment   string                       `json:"environment"`
	Running       bool                         `json:"running"`
	GitBranch     string                       `json:"gitBranch"`
	CreatedAt     time.Time                    `json:"createdAt"`
	Tasks         []remoteSnapshotTask         `json:"tasks"`
	Conversations []remoteSnapshotConversation `json:"conversations"`
}

type remoteSnapshotTask struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Priority    string    `json:"priority"`
	Status      string    `json:"status"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type remoteSnapshotConversation struct {
	ID             string                  `json:"id"`
	Title          string                  `json:"title"`
	Status         string                  `json:"status"`
	AgentID        string                  `json:"agentId"`
	LastActivityAt time.Time               `json:"lastActivityAt"`
	IsCurrent      bool                    `json:"isCurrent"`
	Messages       []remoteSnapshotMessage `json:"messages"`
}

type remoteSnapshotMessage struct {
	ID        string    `json:"id"`
	RunID     string    `json:"runId,omitempty"`
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"createdAt"`
}

// Mobile receives durable message events in real time. The snapshot is only a
// bounded recovery/bootstrap view, so do not rebuild every historical
// conversation on each sync. Older desktop history remains in SQLite and is
// deliberately outside the mobile relay's hot path.
const (
	remoteSnapshotConversationsPerProject = 1
	remoteSnapshotMessagesPerConversation = 20
	remoteSnapshotMessageContentLimit     = 2000
)

type remoteCommand struct {
	CommandID      string          `json:"commandId"`
	Type           string          `json:"type"`
	ProjectID      string          `json:"projectId,omitempty"`
	TaskID         string          `json:"taskId,omitempty"`
	IdempotencyKey string          `json:"idempotencyKey"`
	Payload        json.RawMessage `json:"payload,omitempty"`
	ExpiresAt      time.Time       `json:"expiresAt"`
}

var errRemoteCommandExpired = errors.New("remote command expired")

func remoteCommandHash(command remoteCommand) string {
	value := fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s", command.Type, command.ProjectID, command.TaskID, command.IdempotencyKey, string(command.Payload))
	digest := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", digest[:])
}

// migrateRemoteControl creates only local durable state. Cloud credentials and
// project source files are intentionally not stored in these tables.
func (s *Server) migrateRemoteControl(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
create table if not exists remote_instance (
		instance_id text primary key,
		status text not null default 'agent_unavailable',
		last_agent_sequence integer not null default 0,
		last_seen_at datetime,
		updated_at datetime not null
);
create table if not exists remote_outbox (
		event_id text primary key,
		agent_sequence integer not null unique,
		type text not null,
		task_id text not null default '',
		task_run_id text not null default '',
		payload text not null default '{}',
		created_at datetime not null,
		attempts integer not null default 0,
		next_attempt_at datetime,
		last_error text not null default ''
);
create index if not exists remote_outbox_pending on remote_outbox(next_attempt_at,agent_sequence);
	create table if not exists processed_remote_commands (
		command_id text primary key,
		idempotency_key text not null,
		type text not null,
		status text not null,
		result text not null default '{}',
		received_at datetime not null,
		updated_at datetime not null,
		expires_at datetime not null,
		request_payload text not null default '{}',
		request_hash text not null default ''
	);
create unique index if not exists processed_remote_commands_idempotency
	on processed_remote_commands(idempotency_key,type);
`)
	if err != nil {
		return err
	}
	if err := ensureColumn(ctx, s.db, "processed_remote_commands", "request_hash", "text not null default ''"); err != nil {
		return err
	}
	if err := ensureColumn(ctx, s.db, "processed_remote_commands", "request_payload", "text not null default '{}' "); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `update processed_remote_commands set status='indeterminate',updated_at=? where status='executing'`, time.Now().UTC()); err != nil {
		return err
	}
	var instanceID string
	err = s.db.QueryRowContext(ctx, `select instance_id from remote_instance limit 1`).Scan(&instanceID)
	if errors.Is(err, sql.ErrNoRows) {
		instanceID = uuid.NewString()
		_, err = s.db.ExecContext(ctx, `insert into remote_instance(instance_id,status,updated_at) values(?,?,?)`, instanceID, "agent_unavailable", time.Now().UTC())
	}
	if err != nil {
		return err
	}
	if configured := strings.TrimSpace(s.config.RemoteInstanceID); configured != "" {
		var current string
		if err := s.db.QueryRowContext(ctx, `select instance_id from remote_instance limit 1`).Scan(&current); err == nil && current != configured {
			if _, err := s.db.ExecContext(ctx, `update remote_instance set instance_id=? where instance_id=?`, configured, current); err != nil {
				return err
			}
		}
	}
	return nil
}

// desktopPairingPaths are the only relay endpoints the desktop page itself
// calls: generating a pairing QR code and confirming a pairing request. Both
// describe an action taken by the person sitting at the computer, so they
// cannot be driven by the Agent's process token alone. Keep this list explicit
// and tiny — every other relay endpoint must stay Agent-only, otherwise a
// compromised page session could synthesize Agent sync traffic.
var desktopPairingPaths = map[string]bool{
	"/api/remote/pairing":         true,
	"/api/remote/pairing/confirm": true,
	"/api/remote/pairing/status":  true,
	"/api/remote/agent-status":    true,
}

// validDesktopSession reports whether the request carries the desktop page's
// one-start session token. Only desktop-api mode issues that token; web mode
// has no session and must never satisfy this check.
func (s *Server) validDesktopSession(r *http.Request) bool {
	if s.config.Mode != "desktop-api" {
		return false
	}
	expected := strings.TrimSpace(s.config.SessionToken)
	if expected == "" {
		return false
	}
	provided := strings.TrimSpace(r.Header.Get("X-Milevia-Session"))
	return provided != "" && subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

// remoteAgentOnly keeps the relay API off the public desktop/web surface. A
// deployment should set AUTO_REMOTE_AGENT_TOKEN; loopback is accepted only as
// a development fallback when the relay runs beside control-server.
func (s *Server) remoteAgentOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The desktop page holds the session token, not the Agent token, so
		// pairing would otherwise be unreachable from the UI that triggers it.
		if desktopPairingPaths[strings.TrimSuffix(r.URL.Path, "/")] && s.validDesktopSession(r) {
			next.ServeHTTP(w, r)
			return
		}
		if token := strings.TrimSpace(s.config.RemoteAgentToken); token != "" {
			provided := r.Header.Get("X-Milevia-Agent-Token")
			if subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
				writeError(w, http.StatusUnauthorized, errors.New("invalid agent token"))
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		// 桌面宿主会为 sidecar 与 Agent 注入同一随机进程令牌。绝不能在
		// desktop-api 模式降级为“任意 loopback 进程均可信”，否则本机其他
		// 程序可篡改远程凭据或伪造 Agent 同步。
		if s.config.Mode == "desktop-api" {
			writeError(w, http.StatusUnauthorized, errors.New("remote agent token is not configured"))
			return
		}
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			writeError(w, http.StatusUnauthorized, errors.New("remote agent token is not configured"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) updateRemoteStatus(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Status string `json:"status"`
	}
	if !decode(w, r, &input) {
		return
	}
	allowed := map[string]bool{"online": true, "user_session_unavailable": true, "machine_offline": true, "agent_unavailable": true, "sync_error": true}
	if !allowed[input.Status] {
		writeError(w, http.StatusBadRequest, errors.New("invalid remote status"))
		return
	}
	if _, err := s.db.ExecContext(r.Context(), `update remote_instance set status=?,last_seen_at=?,updated_at=?`, input.Status, time.Now().UTC(), time.Now().UTC()); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": input.Status})
}

// updateRemoteCredentials receives the Agent's per-installation cloud
// credential over the authenticated loopback channel. The token remains in
// process memory; the Agent keeps the durable, DPAPI-protected copy.
func (s *Server) updateRemoteCredentials(w http.ResponseWriter, r *http.Request) {
	var input struct {
		CloudURL   string `json:"cloudUrl"`
		InstanceID string `json:"instanceId"`
		AgentToken string `json:"agentToken"`
	}
	if !decode(w, r, &input) {
		return
	}
	input.CloudURL = strings.TrimRight(strings.TrimSpace(input.CloudURL), "/")
	input.InstanceID = strings.TrimSpace(input.InstanceID)
	input.AgentToken = strings.TrimSpace(input.AgentToken)
	if input.CloudURL == "" || input.InstanceID == "" || input.AgentToken == "" {
		writeError(w, http.StatusBadRequest, errors.New("cloudUrl, instanceId and agentToken are required"))
		return
	}
	parsed, err := url.Parse(input.CloudURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		writeError(w, http.StatusBadRequest, errors.New("cloudUrl must be an absolute HTTP(S) URL"))
		return
	}
	if _, err := s.db.ExecContext(r.Context(), `update remote_instance set instance_id=?,updated_at=?`, input.InstanceID, time.Now().UTC()); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.remoteCredentialMu.Lock()
	s.remoteCloudURL = input.CloudURL
	s.remoteCloudToken = input.AgentToken
	s.remoteInstanceID = input.InstanceID
	s.remoteCredentialMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"status": "configured", "instanceId": input.InstanceID})
}

func (s *Server) remoteCloudCredentials() (cloudURL, cloudToken, instanceID string) {
	s.remoteCredentialMu.RLock()
	cloudURL, cloudToken, instanceID = s.remoteCloudURL, s.remoteCloudToken, s.remoteInstanceID
	s.remoteCredentialMu.RUnlock()
	if cloudURL == "" {
		cloudURL = strings.TrimRight(s.config.RemoteCloudURL, "/")
	}
	if cloudToken == "" {
		cloudToken = s.config.RemoteCloudToken
	}
	if instanceID == "" {
		instanceID = s.config.RemoteInstanceID
	}
	return strings.TrimRight(strings.TrimSpace(cloudURL), "/"), strings.TrimSpace(cloudToken), strings.TrimSpace(instanceID)
}

func (s *Server) createRemotePairing(w http.ResponseWriter, r *http.Request) {
	cloudURL, cloudToken, instanceID := s.remoteCloudCredentials()
	if cloudURL == "" || cloudToken == "" {
		writeError(w, http.StatusServiceUnavailable, errors.New("remote Agent is not ready; wait for registration or check milevia-agent.log"))
		return
	}
	if instanceID == "" {
		if err := s.db.QueryRowContext(r.Context(), `select instance_id from remote_instance limit 1`).Scan(&instanceID); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, cloudURL+"/v1/agent/pairings", nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	request.Header.Set("X-Milevia-Agent-Token", cloudToken)
	request.Header.Set("X-Milevia-Instance-ID", instanceID)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		writeError(w, http.StatusBadGateway, fmt.Errorf("cloud pairing failed (%d)", response.StatusCode))
		return
	}
	var value any
	if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusCreated, value)
}

func (s *Server) confirmRemotePairing(w http.ResponseWriter, r *http.Request) {
	cloudURL, cloudToken, instanceID := s.remoteCloudCredentials()
	if cloudURL == "" || cloudToken == "" {
		writeError(w, http.StatusServiceUnavailable, errors.New("remote Agent is not ready; wait for registration or check milevia-agent.log"))
		return
	}
	var input struct {
		PairingID string `json:"pairingId"`
	}
	if !decode(w, r, &input) || strings.TrimSpace(input.PairingID) == "" {
		return
	}
	if instanceID == "" {
		if err := s.db.QueryRowContext(r.Context(), `select instance_id from remote_instance limit 1`).Scan(&instanceID); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, cloudURL+"/v1/agent/pairings/"+url.PathEscape(input.PairingID)+"/confirm", nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	request.Header.Set("X-Milevia-Agent-Token", cloudToken)
	request.Header.Set("X-Milevia-Instance-ID", instanceID)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		writeError(w, http.StatusBadGateway, fmt.Errorf("cloud pairing confirmation failed (%d)", response.StatusCode))
		return
	}
	var value any
	if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

// remotePairingStatus relays the cloud's pairing-session state to the desktop
// page. The page cannot reach the cloud directly — its origin is the tauri
// protocol, which the cloud's allowed-origin list rejects — so without this
// relay the UI cannot tell that the phone has scanned and submitted, and the
// user is left clicking "confirm" blind.
func (s *Server) remotePairingStatus(w http.ResponseWriter, r *http.Request) {
	pairingID := strings.TrimSpace(r.URL.Query().Get("pairingId"))
	if pairingID == "" {
		writeError(w, http.StatusBadRequest, errors.New("pairingId is required"))
		return
	}
	cloudURL, _, _ := s.remoteCloudCredentials()
	if cloudURL == "" {
		writeError(w, http.StatusServiceUnavailable, errors.New("remote cloud is not configured"))
		return
	}
	// Pairing status carries no secret beyond the random session id, so the
	// cloud serves it publicly and this relay adds no credential of its own.
	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, cloudURL+"/v1/pairings/"+url.PathEscape(pairingID)+"/status", nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		writeError(w, http.StatusBadGateway, fmt.Errorf("cloud pairing status failed (%d)", response.StatusCode))
		return
	}
	var value any
	if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

// remoteAgentStatus tells the desktop page whether the computer-side Agent has
// registered with the cloud and published its credential. Without it the page
// cannot distinguish "the cloud is unreachable" from "this computer was never
// enrolled", and clicking "generate QR code" would only ever return 503. The
// agent token itself is never returned — only whether one exists.
func (s *Server) remoteAgentStatus(w http.ResponseWriter, r *http.Request) {
	cloudURL, cloudToken, instanceID := s.remoteCloudCredentials()
	writeJSON(w, http.StatusOK, map[string]any{
		"ready":      cloudURL != "" && cloudToken != "",
		"cloudUrl":   cloudURL,
		"instanceId": instanceID,
	})
}

func (s *Server) enqueueRemoteCommand(w http.ResponseWriter, r *http.Request) {
	var command remoteCommand
	if !decode(w, r, &command) {
		return
	}
	command.Type = strings.TrimSpace(command.Type)
	command.IdempotencyKey = strings.TrimSpace(command.IdempotencyKey)
	allowed := map[string]bool{"task.create": true, "task.update": true, "task.delete": true, "task.dispatch": true, "task.stop": true, "task.review": true, "task.reopen": true, "conversation.create": true, "conversation.message": true}
	if !allowed[command.Type] || command.IdempotencyKey == "" {
		writeError(w, http.StatusBadRequest, errors.New("type and idempotencyKey are required and must be supported"))
		return
	}
	if len(command.Payload) > 256*1024 {
		writeError(w, http.StatusRequestEntityTooLarge, errors.New("remote command payload is too large"))
		return
	}
	if command.CommandID == "" {
		command.CommandID = uuid.NewString()
	}
	if command.ExpiresAt.IsZero() {
		command.ExpiresAt = time.Now().UTC().Add(5 * time.Minute)
	}
	if command.ExpiresAt.Before(time.Now().UTC()) || command.ExpiresAt.After(time.Now().UTC().Add(15*time.Minute)) {
		writeError(w, http.StatusBadRequest, errors.New("expiresAt must be within the next 15 minutes"))
		return
	}
	hash := remoteCommandHash(command)
	now := time.Now().UTC()
	result := json.RawMessage(`{}`)
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	var existingID, existingHash, existingStatus string
	err = tx.QueryRowContext(r.Context(), `select command_id,request_hash,status from processed_remote_commands where idempotency_key=? and type=?`, command.IdempotencyKey, command.Type).Scan(&existingID, &existingHash, &existingStatus)
	if err == nil {
		_ = tx.Rollback()
		if existingHash != "" && existingHash != hash {
			writeError(w, http.StatusConflict, errors.New("idempotency key conflicts with a different command"))
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"commandId": existingID, "status": existingStatus, "idempotent": true})
		return
	}
	if !errors.Is(err, sql.ErrNoRows) {
		_ = tx.Rollback()
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	requestBody := mustJSON(command)
	_, err = tx.ExecContext(r.Context(), `insert into processed_remote_commands(command_id,idempotency_key,type,status,result,received_at,updated_at,expires_at,request_payload,request_hash) values(?,?,?,?,?,?,?,?,?,?)`, command.CommandID, command.IdempotencyKey, command.Type, "queued", string(result), now, now, command.ExpiresAt, requestBody, hash)
	if err != nil {
		_ = tx.Rollback()
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err = tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.wakeRemoteCommandWorker()
	writeJSON(w, http.StatusAccepted, map[string]any{"commandId": command.CommandID, "status": "queued", "idempotent": false})
}

func (s *Server) wakeRemoteCommandWorker() {
	if s.remoteCommandWake == nil {
		return
	}
	select {
	case s.remoteCommandWake <- struct{}{}:
	default:
	}
}

// remoteRelayConfigured reports whether the cloud relay is fully configured.
// When it is not configured there is no consumer to drain the outbox, so task
// events stay local and ordinary desktop deployments do not accumulate data.
func (s *Server) remoteRelayConfigured() bool {
	// The local Agent token only protects the loopback relay API; it does not
	// imply that an Agent is configured to drain the outbox. Require both cloud
	// settings so standalone desktop installs do not accumulate undeliverable
	// events indefinitely.
	cloudURL, cloudToken, _ := s.remoteCloudCredentials()
	return cloudURL != "" && cloudToken != ""
}

func enqueueRemoteEventTx(ctx context.Context, tx *sql.Tx, eventID, taskID, taskRunID, typ string, payload []byte, now time.Time) error {
	payload = compactRemoteEventPayload(typ, payload)
	if _, err := tx.ExecContext(ctx, `update remote_instance set last_agent_sequence=last_agent_sequence+1,updated_at=?`, now); err != nil {
		return err
	}
	var sequence int64
	if err := tx.QueryRowContext(ctx, `select last_agent_sequence from remote_instance limit 1`).Scan(&sequence); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `insert into remote_outbox(event_id,agent_sequence,type,task_id,task_run_id,payload,created_at) values(?,?,?,?,?,?,?)`, eventID, sequence, typ, taskID, taskRunID, string(payload), now)
	return err
}

// Remote events are notifications; the complete conversation content is
// recovered from the snapshot. Keep large assistant/tool payloads out of the
// Agent WebSocket frame, whose read limit is intentionally conservative.
func compactRemoteEventPayload(typ string, payload []byte) []byte {
	// Keep small lifecycle payloads (especially conversation.created, which
	// carries the new conversation ID) so mobile can switch views without
	// waiting for a full snapshot. Oversized payloads remain recoverable from
	// the snapshot and must not occupy the Agent WebSocket frame.
	if len(payload) > 256<<10 {
		return []byte(`{}`)
	}
	return payload
}

// enqueueRemoteEvent persists a conversation event for the Agent relay when
// the event was not created inside an existing business transaction (for
// example, orchestration status messages).
func (s *Server) enqueueRemoteEvent(ctx context.Context, eventID, taskID, taskRunID, typ string, payload []byte, now time.Time) error {
	if !s.remoteRelayConfigured() {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := enqueueRemoteEventTx(ctx, tx, eventID, taskID, taskRunID, typ, payload, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Server) remoteOverview(w http.ResponseWriter, r *http.Request) {
	var item RemoteInstance
	// SQLite returns an expression such as coalesce(...) as text even when both
	// source columns contain timestamps. Scan the raw value and normalize it so
	// the relay health endpoint works with both SQLite and PostgreSQL drivers.
	var observedAt any
	if err := s.db.QueryRowContext(r.Context(), `select instance_id,status,last_agent_sequence,coalesce(last_seen_at,updated_at) from remote_instance limit 1`).Scan(&item.InstanceID, &item.Status, &item.LastAgentSequence, &observedAt); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	parsed, err := parseRemoteTimestamp(observedAt)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	item.ObservedAt = parsed
	writeJSON(w, http.StatusOK, item)
}

func parseRemoteTimestamp(value any) (time.Time, error) {
	switch raw := value.(type) {
	case time.Time:
		return raw, nil
	case string:
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999"} {
			if parsed, err := time.Parse(layout, raw); err == nil {
				return parsed, nil
			}
		}
		return time.Time{}, fmt.Errorf("invalid remote timestamp %q", raw)
	case []byte:
		return parseRemoteTimestamp(string(raw))
	default:
		return time.Time{}, fmt.Errorf("invalid remote timestamp type %T", value)
	}
}

func (s *Server) remoteSnapshot(w http.ResponseWriter, r *http.Request) {
	var revision int64
	if err := s.db.QueryRowContext(r.Context(), `select last_agent_sequence from remote_instance limit 1`).Scan(&revision); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	snapshot := remoteSnapshot{SnapshotRevision: revision, ObservedAt: time.Now().UTC(), Projects: make([]remoteSnapshotProject, 0)}
	rows, err := s.db.QueryContext(r.Context(), `
		select
			p.id,
			p.name,
			coalesce(nullif(p.runner_id,''),p.runner),
			p.path,
			p.git_branch,
			p.created_at,
			case when exists(select 1 from conversations c where c.project_id=p.id and c.status='running') then 1 else 0 end
		from projects p
		order by p.created_at desc`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	projects := make([]remoteSnapshotProject, 0)
	for rows.Next() {
		var project remoteSnapshotProject
		var path string
		if err := rows.Scan(&project.ID, &project.Name, &project.Runner, &path, &project.GitBranch, &project.CreatedAt, &project.Running); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		project.Environment = string(s.resolveAgentTargetEnv(project.Runner, path))
		projects = append(projects, project)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := rows.Close(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	for index := range projects {
		project := &projects[index]
		project.Tasks = make([]remoteSnapshotTask, 0)
		taskRows, err := s.db.QueryContext(r.Context(), `select id,title,description,priority,status,updated_at from tasks where project_id=? order by position,id`, project.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		for taskRows.Next() {
			var task remoteSnapshotTask
			if err := taskRows.Scan(&task.ID, &task.Title, &task.Description, &task.Priority, &task.Status, &task.UpdatedAt); err != nil {
				taskRows.Close()
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			project.Tasks = append(project.Tasks, task)
		}
		if err := taskRows.Err(); err != nil {
			taskRows.Close()
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		taskRows.Close()
		project.Conversations = make([]remoteSnapshotConversation, 0)
		// The active conversation is enough to make a project immediately usable
		// on mobile. Do not turn a recovery snapshot into a full history export.
		conversationRows, err := s.db.QueryContext(r.Context(), `select id,title,status,agent_id,last_activity_at,is_current from conversations where project_id=? order by is_current desc,last_activity_at desc,id desc limit ?`, project.ID, remoteSnapshotConversationsPerProject)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		for conversationRows.Next() {
			var conversation remoteSnapshotConversation
			if err := conversationRows.Scan(&conversation.ID, &conversation.Title, &conversation.Status, &conversation.AgentID, &conversation.LastActivityAt, &conversation.IsCurrent); err != nil {
				conversationRows.Close()
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			conversation.Messages = make([]remoteSnapshotMessage, 0)
			project.Conversations = append(project.Conversations, conversation)
		}
		if err := conversationRows.Err(); err != nil {
			conversationRows.Close()
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		conversationRows.Close()
		if len(project.Conversations) > 0 {
			conversation := &project.Conversations[0]
			// The newest message is rendered immediately on mobile and must not be
			// truncated. Older messages stay bounded to keep the recovery snapshot
			// small; the realtime event path carries their full content when live.
			messageRows, err := s.db.QueryContext(r.Context(), `select id,run_id,role,content,created_at from messages where conversation_id=? and parent_tool_use_id='' order by created_at desc,id desc limit ?`, conversation.ID, remoteSnapshotMessagesPerConversation)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			messageIndex := 0
			for messageRows.Next() {
				var message remoteSnapshotMessage
				if err := messageRows.Scan(&message.ID, &message.RunID, &message.Role, &message.Content, &message.CreatedAt); err != nil {
					messageRows.Close()
					writeError(w, http.StatusInternalServerError, err)
					return
				}
				if messageIndex > 0 {
					message.Content = truncateUTF8(message.Content, remoteSnapshotMessageContentLimit)
				}
				conversation.Messages = append(conversation.Messages, message)
				messageIndex++
			}
			if err := messageRows.Err(); err != nil {
				messageRows.Close()
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			if err := messageRows.Close(); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			for left, right := 0, len(conversation.Messages)-1; left < right; left, right = left+1, right-1 {
				conversation.Messages[left], conversation.Messages[right] = conversation.Messages[right], conversation.Messages[left]
			}
		}
		snapshot.Projects = append(snapshot.Projects, *project)
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func (s *Server) remoteOutbox(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 && parsed <= 500 {
			limit = parsed
		}
	}
	// Events that exhausted their delivery budget stay in the table as a
	// durable record but leave the delivery window. Without this bound a
	// single undeliverable event (for example one the cloud permanently
	// rejects) would sit at the head of this ordered batch forever and starve
	// every event behind it.
	rows, err := s.db.QueryContext(r.Context(), `select event_id,agent_sequence,type,task_id,task_run_id,payload,created_at from remote_outbox where (next_attempt_at is null or next_attempt_at<=?) and attempts < ? order by agent_sequence limit ?`, time.Now().UTC(), maxRemoteOutboxAttempts, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()
	items := make([]remoteOutboxItem, 0)
	for rows.Next() {
		var item remoteOutboxItem
		var payload string
		if err := rows.Scan(&item.EventID, &item.AgentSequence, &item.Type, &item.TaskID, &item.TaskRunID, &payload, &item.CreatedAt); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		item.Payload = json.RawMessage(payload)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) ackRemoteOutbox(w http.ResponseWriter, r *http.Request) {
	var input struct {
		AcceptedThrough int64    `json:"acceptedThrough"`
		EventIDs        []string `json:"eventIds"`
	}
	if !decode(w, r, &input) {
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if input.AcceptedThrough > 0 {
		if _, err = tx.ExecContext(r.Context(), `delete from remote_outbox where agent_sequence<=?`, input.AcceptedThrough); err != nil {
			_ = tx.Rollback()
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	for _, id := range input.EventIDs {
		if strings.TrimSpace(id) == "" {
			continue
		}
		if _, err = tx.ExecContext(r.Context(), `delete from remote_outbox where event_id=?`, id); err != nil {
			_ = tx.Rollback()
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	if err = tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "acknowledged"})
}

// failRemoteOutbox schedules a retry for an event the cloud refused. With
// permanent=true the event can never succeed (the cloud reported a durable
// conflict), so it is dropped instead of being retried until it exhausts the
// attempt budget and blocks the queue head.
func (s *Server) failRemoteOutbox(w http.ResponseWriter, r *http.Request) {
	var input struct {
		EventIDs  []string `json:"eventIds"`
		Error     string   `json:"error"`
		Permanent bool     `json:"permanent"`
	}
	if !decode(w, r, &input) {
		return
	}
	if len(input.EventIDs) == 0 || len(input.EventIDs) > 500 {
		writeError(w, http.StatusBadRequest, errors.New("eventIds must contain between 1 and 500 items"))
		return
	}
	if len(input.Error) > 2000 {
		input.Error = input.Error[:2000]
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	status := "scheduled"
	for _, id := range input.EventIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if input.Permanent {
			// Keep the rejection visible in the service log; the row itself is
			// removed so it cannot starve later events.
			log.Printf("drop undeliverable remote event %s: %s", id, input.Error)
			if _, err = tx.ExecContext(r.Context(), `delete from remote_outbox where event_id=?`, id); err != nil {
				_ = tx.Rollback()
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			status = "dropped"
			continue
		}
		_, err = tx.ExecContext(r.Context(), `update remote_outbox set attempts=attempts+1,next_attempt_at=datetime(?, '+' || min(3600, max(5, 5 * (1 << min(attempts, 8)))) || ' seconds'),last_error=? where event_id=?`, now, input.Error, id)
		if err != nil {
			_ = tx.Rollback()
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	if err = tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": status})
}

func (s *Server) getRemoteCommand(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "commandID")
	var command remoteCommand
	var requestPayload, result string
	var status string
	if err := s.db.QueryRowContext(r.Context(), `select command_id,type,idempotency_key,status,result,expires_at,request_payload from processed_remote_commands where command_id=?`, id).Scan(&command.CommandID, &command.Type, &command.IdempotencyKey, &status, &result, &command.ExpiresAt, &requestPayload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, errors.New("remote command not found"))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	command.Payload = json.RawMessage(requestPayload)
	writeJSON(w, http.StatusOK, map[string]any{"command": command, "status": status, "result": json.RawMessage(result)})
}

func (s *Server) updateRemoteCommand(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "commandID")
	var input struct {
		Status string          `json:"status"`
		Result json.RawMessage `json:"result"`
	}
	if !decode(w, r, &input) {
		return
	}
	allowed := map[string]bool{"received": true, "executing": true, "completed": true, "failed": true, "expired": true, "indeterminate": true, "cancelled": true}
	if !allowed[input.Status] {
		writeError(w, http.StatusBadRequest, errors.New("invalid command status"))
		return
	}
	if len(input.Result) == 0 {
		input.Result = json.RawMessage(`{}`)
	}
	if !json.Valid(input.Result) || len(input.Result) > 256*1024 {
		writeError(w, http.StatusBadRequest, errors.New("result must be valid JSON and smaller than 256 KiB"))
		return
	}
	now := time.Now().UTC()
	result, err := s.db.ExecContext(r.Context(), `update processed_remote_commands set status=?,result=?,updated_at=? where command_id=? and status not in ('completed','failed','expired','cancelled','indeterminate')`, input.Status, string(input.Result), now, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	changed, _ := result.RowsAffected()
	if changed == 0 {
		var current string
		if err := s.db.QueryRowContext(r.Context(), `select status from processed_remote_commands where command_id=?`, id).Scan(&current); errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, errors.New("remote command not found"))
			return
		} else if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		} else if current != input.Status {
			writeError(w, http.StatusConflict, errors.New("remote command is already terminal or has changed"))
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"commandId": id, "status": input.Status})
}

// startRemoteCommandWorker is the local consumer for commands submitted by a
// trusted Agent relay. Claiming and completing a command are both durable, so
// a process crash cannot silently turn a remote request into a successful task.
func (s *Server) startRemoteCommandWorker() {
	s.runWG.Add(1)
	go func() {
		defer s.runWG.Done()
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-s.runtimeCtx.Done():
				return
			case <-ticker.C:
				s.processOneRemoteCommand(s.runtimeCtx)
			case <-s.remoteCommandWake:
				s.processOneRemoteCommand(s.runtimeCtx)
			}
		}
	}()
}

func (s *Server) processOneRemoteCommand(ctx context.Context) {
	var command remoteCommand
	var status string
	var payload string
	err := func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var id, typ, idem string
		var expires time.Time
		if err := tx.QueryRowContext(ctx, `select command_id,type,request_payload,idempotency_key,expires_at,status from processed_remote_commands where status='queued' order by received_at limit 1`).Scan(&id, &typ, &payload, &idem, &expires, &status); err != nil {
			return err
		}
		if expires.Before(time.Now().UTC()) {
			_, err = tx.ExecContext(ctx, `update processed_remote_commands set status='expired',updated_at=? where command_id=? and status='queued'`, time.Now().UTC(), id)
			if err != nil {
				return err
			}
			if err := tx.Commit(); err != nil {
				return err
			}
			return errRemoteCommandExpired
		}
		// The request body remains opaque in storage so command schemas can evolve.
		command.CommandID, command.Type, command.IdempotencyKey, command.ExpiresAt = id, typ, idem, expires
		if err := json.Unmarshal([]byte(payload), &command); err != nil {
			command.Payload = json.RawMessage(payload)
		}
		result, err := tx.ExecContext(ctx, `update processed_remote_commands set status='executing',updated_at=? where command_id=? and status='queued'`, time.Now().UTC(), id)
		if err != nil {
			return err
		}
		changed, _ := result.RowsAffected()
		if changed != 1 {
			return sql.ErrNoRows
		}
		return tx.Commit()
	}()
	if errors.Is(err, errRemoteCommandExpired) || errors.Is(err, sql.ErrNoRows) {
		return
	}
	if err != nil {
		return
	}
	// A single worker goroutine drains this queue, so one slow command would
	// otherwise hold up every command behind it. Bound each execution and let
	// the failure surface as a normal command failure.
	execCtx, cancel := context.WithTimeout(ctx, remoteCommandTimeout)
	result, execErr := s.executeRemoteCommand(execCtx, command)
	cancel()
	finalStatus := "completed"
	if execErr != nil {
		finalStatus = "failed"
		result = map[string]string{"error": execErr.Error()}
	}
	encoded := mustJSON(result)
	s.finalizeRemoteCommand(command.CommandID, finalStatus, encoded)
}

// finalizeRemoteCommand persists the terminal state independently of the
// command execution context. A transient SQLite error must not strand a
// command in "executing" forever, so retry a few times before surfacing a
// durable error in the service log for manual recovery.
func (s *Server) finalizeRemoteCommand(commandID, status string, result []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		_, err = s.db.ExecContext(ctx, `update processed_remote_commands set status=?,result=?,updated_at=? where command_id=? and status='executing'`, status, result, time.Now().UTC(), commandID)
		if err == nil {
			return
		}
		if attempt < 2 {
			timer := time.NewTimer(time.Duration(attempt+1) * 100 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				break
			case <-timer.C:
			}
			if ctx.Err() != nil {
				break
			}
		}
	}
	log.Printf("finalize remote command %s (%s): %v", commandID, status, err)
}

func (s *Server) executeRemoteCommand(ctx context.Context, command remoteCommand) (any, error) {
	if command.Type == "conversation.create" {
		if command.ProjectID == "" {
			return nil, errors.New("projectId is required")
		}
		return s.executeRemoteHTTPCommand(ctx, http.MethodPost, "/api/projects/"+command.ProjectID+"/conversations?new=true", command.ProjectID, "", "", s.createConversation, command.Payload)
	}
	if command.Type == "conversation.message" {
		var input struct {
			ConversationID  string `json:"conversationId"`
			Content         string `json:"content"`
			ClientRequestID string `json:"clientRequestId,omitempty"`
		}
		if err := json.Unmarshal(command.Payload, &input); err != nil {
			return nil, errors.New("conversation message payload is invalid")
		}
		input.ConversationID = strings.TrimSpace(input.ConversationID)
		input.Content = strings.TrimSpace(input.Content)
		if input.ConversationID == "" || input.Content == "" {
			return nil, errors.New("conversationId and content are required")
		}
		return s.executeRemoteHTTPCommand(ctx, http.MethodPost, "/api/conversations/"+input.ConversationID+"/messages", "", "", input.ConversationID, s.sendMessage, mustJSON(input))
	}
	if command.Type == "task.create" {
		if command.ProjectID == "" {
			return nil, errors.New("projectId is required")
		}
		return s.executeRemoteHTTPCommand(ctx, http.MethodPost, "/api/projects/"+command.ProjectID+"/tasks", command.ProjectID, "", "", s.createTask, command.Payload)
	}
	if command.TaskID == "" {
		return nil, errors.New("taskId is required")
	}
	if command.Type == "task.dispatch" {
		result, _, err := s.dispatchTaskByID(ctx, command.TaskID)
		if err != nil {
			return nil, err
		}
		return result, nil
	}
	path := "/api/tasks/" + command.TaskID
	var handler http.HandlerFunc
	method := http.MethodPost
	switch command.Type {
	case "task.update":
		handler = s.updateTask
		method = http.MethodPatch
	case "task.delete":
		handler = s.deleteTask
		method = http.MethodDelete
	case "task.stop":
		handler = s.stopTask
		path += "/stop"
	case "task.review":
		handler = s.reviewTask
		path += "/review"
	case "task.reopen":
		handler = s.reopenTask
		path += "/reopen"
	default:
		return nil, errors.New("remote command type is not implemented")
	}
	return s.executeRemoteHTTPCommand(ctx, method, path, "", command.TaskID, "", handler, command.Payload)
}

func (s *Server) executeRemoteHTTPCommand(ctx context.Context, method, path, projectID, taskID, conversationID string, handler http.HandlerFunc, payload json.RawMessage) (any, error) {
	req := httptest.NewRequest(method, path, strings.NewReader(string(payload))).WithContext(ctx)
	rctx := chi.NewRouteContext()
	if projectID != "" {
		rctx.URLParams.Add("projectID", projectID)
	}
	if taskID != "" {
		rctx.URLParams.Add("taskID", taskID)
	}
	if conversationID != "" {
		rctx.URLParams.Add("conversationID", conversationID)
	}
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	response := httptest.NewRecorder()
	handler(response, req)
	if response.Code >= 400 {
		return nil, fmt.Errorf("task command failed (%d): %s", response.Code, strings.TrimSpace(response.Body.String()))
	}
	var value any
	if response.Body.Len() > 0 {
		_ = json.Unmarshal(response.Body.Bytes(), &value)
	}
	return value, nil
}
