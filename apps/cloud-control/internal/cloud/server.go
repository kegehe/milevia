package cloud

import (
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Config struct {
	DatabaseURL     string
	AgentTokens     map[string]string
	EnrollmentToken string
	UserToken       string
	AppURL          string
}

type Server struct {
	db          *pgxpool.Pool
	config      Config
	mu          sync.Mutex
	connections map[string]*websocket.Conn
	writeMu     sync.Mutex
	// Rate limiters for the unauthenticated endpoints. The authenticated
	// mobile API is polled by design and is not throttled here.
	registerLimiter *rateLimiter
	claimLimiter    *rateLimiter
	statusLimiter   *rateLimiter
}

// rateLimiter is a per-client token bucket held in process memory. The current
// deployment is a single node, so no shared store is needed; the goal is simply
// to stop unauthenticated endpoints (machine registration, pairing claims) from
// being hammered, and to bound the cost of a leaked enrollment token.
type rateLimiter struct {
	mu       sync.Mutex
	buckets  map[string]bucket
	capacity float64
	refill   float64
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(capacity, perMinute int) *rateLimiter {
	return &rateLimiter{buckets: map[string]bucket{}, capacity: float64(capacity), refill: float64(perMinute) / 60}
}

func (l *rateLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buckets) > 10000 {
		for k, b := range l.buckets {
			if now.Sub(b.last) > 10*time.Minute {
				delete(l.buckets, k)
			}
		}
	}
	current, ok := l.buckets[key]
	if !ok {
		current = bucket{tokens: l.capacity, last: now}
	}
	if elapsed := now.Sub(current.last).Seconds(); elapsed > 0 {
		current.tokens = math.Min(l.capacity, current.tokens+elapsed*l.refill)
		current.last = now
	}
	if current.tokens < 1 {
		l.buckets[key] = current
		return false
	}
	current.tokens--
	l.buckets[key] = current
	return true
}

// clientIP identifies the caller for rate limiting. The documented deployment
// terminates TLS at a reverse proxy on the same host, so X-Forwarded-For is the
// only way to see the real client; RemoteAddr is the fallback for direct
// access. Rate limiting is defence in depth here, not the only gate.
func clientIP(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		if first := strings.TrimSpace(strings.Split(forwarded, ",")[0]); first != "" {
			return first
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func (s *Server) limit(limiter *rateLimiter, scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if limiter == nil || limiter.allow(scope+":"+clientIP(r), time.Now().UTC()) {
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Set("Retry-After", "60")
			writeError(w, http.StatusTooManyRequests, errors.New("too many requests; please retry later"))
		})
	}
}

type instanceScopeKey struct{}

func scopedInstance(r *http.Request) string {
	value, _ := r.Context().Value(instanceScopeKey{}).(string)
	return value
}

func authorizedInstance(r *http.Request, instanceID string) bool {
	scope := scopedInstance(r)
	return scope == "" || scope == instanceID
}

func pairingCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

type commandRequest struct {
	Type      string          `json:"type"`
	ProjectID string          `json:"projectId,omitempty"`
	TaskID    string          `json:"taskId,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	ExpiresAt *time.Time      `json:"expiresAt,omitempty"`
}

type eventEnvelope struct {
	Kind          string          `json:"kind,omitempty"`
	EventID       string          `json:"eventId"`
	AgentSequence int64           `json:"agentSequence"`
	InstanceID    string          `json:"instanceId"`
	Type          string          `json:"type"`
	TaskID        string          `json:"taskId,omitempty"`
	TaskRunID     string          `json:"taskRunId,omitempty"`
	Payload       json.RawMessage `json:"payload"`
	CreatedAt     time.Time       `json:"createdAt"`
}

// Cloud events are a relay/reconnect buffer; canonical conversation history
// remains on the desktop. Keep a bounded per-instance tail so this table does
// not grow without limit while still allowing normal SSE reconnects to resume.
const (
	cloudEventRetentionPerInstance = 5000
	cloudEventPruneInterval        = 100
)

func New(ctx context.Context, config Config) (*Server, error) {
	if config.DatabaseURL == "" {
		return nil, errors.New("database URL is required")
	}
	if len(config.AgentTokens) == 0 && strings.TrimSpace(config.EnrollmentToken) == "" {
		return nil, errors.New("at least one instance-scoped agent token is required")
	}
	db, err := pgxpool.New(ctx, config.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("connect PostgreSQL: %w", err)
	}
	s := &Server{
		db:              db,
		config:          config,
		connections:     map[string]*websocket.Conn{},
		registerLimiter: newRateLimiter(10, 30),
		claimLimiter:    newRateLimiter(10, 30),
		statusLimiter:   newRateLimiter(60, 300),
	}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Server) Close() {
	s.mu.Lock()
	connections := make([]*websocket.Conn, 0, len(s.connections))
	for _, conn := range s.connections {
		connections = append(connections, conn)
	}
	s.connections = map[string]*websocket.Conn{}
	s.mu.Unlock()
	for _, conn := range connections {
		_ = conn.Close()
	}
	s.db.Close()
}

func (s *Server) migrate(ctx context.Context) error {
	_, err := s.db.Exec(ctx, `
create table if not exists cloud_instances (
  instance_id text primary key,
  name text not null default '',
  status text not null default 'offline',
  last_agent_sequence bigint not null default 0,
  last_seen_at timestamptz,
  updated_at timestamptz not null default now(),
  snapshot jsonb not null default '{"projects":[]}'::jsonb,
  snapshot_revision bigint not null default 0
);
create table if not exists cloud_commands (
  command_id text primary key,
  instance_id text not null references cloud_instances(instance_id) on delete cascade,
  idempotency_key text not null,
  type text not null,
  project_id text not null default '',
  task_id text not null default '',
  payload jsonb not null default '{}'::jsonb,
  status text not null default 'queued',
  result jsonb not null default '{}'::jsonb,
  expires_at timestamptz not null,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now(),
  request_hash text not null default '',
  unique(instance_id, idempotency_key, type)
);
create table if not exists cloud_events (
  event_id text primary key,
  instance_id text not null references cloud_instances(instance_id) on delete cascade,
  agent_sequence bigint not null,
  type text not null,
  task_id text not null default '',
  task_run_id text not null default '',
  payload jsonb not null default '{}'::jsonb,
  created_at timestamptz not null,
  unique(instance_id, agent_sequence)
);
create table if not exists pairing_sessions (
  pairing_id text primary key,
  instance_id text not null,
  code_hash text not null,
  status text not null default 'pending',
  expires_at timestamptz not null,
  created_at timestamptz not null default now(),
  claimed_at timestamptz
);
create table if not exists cloud_access_tokens (
  token_hash text primary key,
  instance_id text not null references cloud_instances(instance_id) on delete cascade,
  created_at timestamptz not null default now(),
  expires_at timestamptz,
  revoked_at timestamptz,
  pairing_id text not null default '',
  activated_at timestamptz
);
create table if not exists cloud_agent_credentials (
  instance_id text primary key references cloud_instances(instance_id) on delete cascade,
  token_hash text not null unique,
  created_at timestamptz not null default now(),
  revoked_at timestamptz
);
create index if not exists cloud_commands_pending on cloud_commands(instance_id,status,created_at);
create index if not exists cloud_events_instance_sequence on cloud_events(instance_id,agent_sequence);
`)
	if err != nil {
		return fmt.Errorf("migrate cloud database: %w", err)
	}
	for _, statement := range []string{
		`alter table cloud_commands add column if not exists request_hash text not null default ''`,
		`alter table pairing_sessions add column if not exists claim_attempts integer not null default 0`,
		`alter table cloud_instances add column if not exists snapshot jsonb not null default '{"projects":[]}'::jsonb`,
		`alter table cloud_instances add column if not exists snapshot_revision bigint not null default 0`,
		`alter table cloud_access_tokens add column if not exists pairing_id text not null default ''`,
		`alter table cloud_access_tokens add column if not exists activated_at timestamptz`,
		// Tokens issued before desktop confirmation became mandatory carry no
		// pairing link. Treat them as already activated so upgrading does not
		// silently break an existing pairing. Re-running this is a no-op.
		`update cloud_access_tokens set activated_at=now() where activated_at is null and pairing_id=''`,
	} {
		if _, err := s.db.Exec(ctx, statement); err != nil {
			return fmt.Errorf("migrate cloud database: %w", err)
		}
	}
	return nil
}

func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Route("/v1", func(r chi.Router) {
		r.Use(s.cors)
		r.Use(s.userAuth)
		r.Get("/instances", s.listInstances)
		r.Get("/instances/{instanceID}/overview", s.instanceOverview)
		r.Get("/instances/{instanceID}/snapshot", s.instanceSnapshot)
		r.Get("/stream", s.mobileEventStream)
		r.Post("/instances/{instanceID}/commands", s.createCommand)
		r.Post("/instances/{instanceID}/revoke", s.revokeInstanceTokens)
		r.Get("/commands/{commandID}", s.getCommand)
		r.Get("/events", s.listEvents)
		r.Post("/pairings", s.createPairing)
	})
	// Registration and pairing are the only unauthenticated entry points, so
	// they carry explicit rate limits. A leaked enrollment token or a scanned
	// pairing QR code is worth far less when it cannot be replayed at speed.
	r.Group(func(r chi.Router) {
		r.Use(s.cors)
		r.Use(s.limit(s.registerLimiter, "agent-register"))
		r.Post("/v1/agent/register", s.agentRegister)
	})
	r.Group(func(r chi.Router) {
		r.Use(s.cors)
		r.Use(s.limit(s.claimLimiter, "pairing-claim"))
		r.Post("/v1/pairings/claim", s.claimPairingByCode)
		r.Post("/v1/pairings/{pairingID}/claim", s.claimPairing)
	})
	// Pairing status is polled by the phone every 1.5s while it waits for the
	// desktop to confirm, so it gets a much looser budget than claim attempts.
	r.Group(func(r chi.Router) {
		r.Use(s.cors)
		r.Use(s.limit(s.statusLimiter, "pairing-status"))
		r.Get("/v1/pairings/{pairingID}/status", s.pairingStatus)
	})
	r.Get("/v1/agent/connect", s.agentConnect)
	r.Post("/v1/agent/events", s.agentEvents)
	r.Post("/v1/agent/snapshot", s.agentSnapshot)
	r.Post("/v1/agent/pairings", s.agentPairing)
	r.Post("/v1/agent/pairings/{pairingID}/confirm", s.agentConfirmPairing)
	return r
}

// agentRegister turns a short-lived deployment enrollment token into a unique
// per-machine credential. The enrollment token is never stored in the DB and
// should be rotated by the operator after provisioning a release build.
func (s *Server) agentRegister(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSpace(s.config.EnrollmentToken) == "" ||
		subtle.ConstantTimeCompare([]byte(strings.TrimSpace(r.Header.Get("X-Milevia-Enrollment-Token"))), []byte(strings.TrimSpace(s.config.EnrollmentToken))) != 1 {
		writeError(w, http.StatusUnauthorized, errors.New("invalid enrollment token"))
		return
	}
	var input struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &input) {
		return
	}
	instanceID := newID()
	var tokenBytes [32]byte
	if _, err := rand.Read(tokenBytes[:]); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	agentToken := fmt.Sprintf("mva_%x", tokenBytes[:])
	if _, err := s.db.Exec(r.Context(), `insert into cloud_instances(instance_id,name,status) values($1,$2,'offline')`, instanceID, strings.TrimSpace(input.Name)); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if _, err := s.db.Exec(r.Context(), `insert into cloud_agent_credentials(instance_id,token_hash) values($1,$2)`, instanceID, hashCode(agentToken)); err != nil {
		_, _ = s.db.Exec(r.Context(), `delete from cloud_instances where instance_id=$1`, instanceID)
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"instanceId": instanceID, "agentToken": agentToken})
}

func (s *Server) createPairing(w http.ResponseWriter, r *http.Request) {
	var input struct {
		InstanceID string `json:"instanceId"`
	}
	if !decode(w, r, &input) || strings.TrimSpace(input.InstanceID) == "" {
		return
	}
	if !authorizedInstance(r, input.InstanceID) {
		writeError(w, http.StatusForbidden, errors.New("instance access denied"))
		return
	}
	exists, err := s.instanceExists(r.Context(), input.InstanceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !exists {
		writeError(w, http.StatusNotFound, errors.New("instance not found"))
		return
	}
	pairingID := newID()
	code, err := pairingCode()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	expires := time.Now().UTC().Add(5 * time.Minute)
	if _, err := s.db.Exec(r.Context(), `insert into pairing_sessions(pairing_id,instance_id,code_hash,status,expires_at) values($1,$2,$3,'pending',$4)`, pairingID, input.InstanceID, hashCode(code), expires); err != nil {
		writeError(w, 500, err)
		return
	}
	// The code is returned separately for display on the desktop. It must not
	// be embedded in the QR URL, which can be retained in browser/proxy logs.
	pairingURL := strings.TrimRight(s.config.AppURL, "/") + "/mobile?pairingId=" + url.QueryEscape(pairingID)
	writeJSON(w, http.StatusCreated, map[string]any{"pairingId": pairingID, "code": code, "instanceId": input.InstanceID, "expiresAt": expires, "pairingURL": pairingURL})
}

func (s *Server) agentPairing(w http.ResponseWriter, r *http.Request) {
	instanceID := strings.TrimSpace(r.Header.Get("X-Milevia-Instance-ID"))
	if instanceID == "" {
		writeError(w, 400, errors.New("X-Milevia-Instance-ID is required"))
		return
	}
	if ok, authErr := s.agentAuth(r, instanceID); agentAuthDenied(w, ok, authErr) {
		return
	}
	if err := s.ensureInstance(r.Context(), instanceID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	pairingID := newID()
	code, err := pairingCode()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	expires := time.Now().UTC().Add(5 * time.Minute)
	if _, err := s.db.Exec(r.Context(), `insert into pairing_sessions(pairing_id,instance_id,code_hash,status,expires_at) values($1,$2,$3,'pending',$4)`, pairingID, instanceID, hashCode(code), expires); err != nil {
		writeError(w, 500, err)
		return
	}
	pairingURL := strings.TrimRight(s.config.AppURL, "/") + "/mobile?pairingId=" + url.QueryEscape(pairingID)
	writeJSON(w, http.StatusCreated, map[string]any{"pairingId": pairingID, "code": code, "instanceId": instanceID, "expiresAt": expires, "pairingURL": pairingURL})
}

// agentConfirmPairing records the desktop-side confirmation after the phone
// has claimed the QR code. The phone receives the pending token from
// claimPairing but only activates it after observing this confirmation.
func (s *Server) agentConfirmPairing(w http.ResponseWriter, r *http.Request) {
	instanceID := strings.TrimSpace(r.Header.Get("X-Milevia-Instance-ID"))
	if instanceID != "" {
		if ok, authErr := s.agentAuth(r, instanceID); agentAuthDenied(w, ok, authErr) {
			return
		}
	} else {
		writeError(w, http.StatusUnauthorized, errors.New("invalid agent token"))
		return
	}
	pairingID := chi.URLParam(r, "pairingID")
	// Confirming is what actually grants the phone access, so the pairing
	// state and the token activation must commit together: a crash between
	// them would otherwise leave a phone reading "confirmed" while its token
	// still cannot authenticate.
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback(r.Context())
	result, err := tx.Exec(r.Context(), `update pairing_sessions set status='confirmed' where pairing_id=$1 and instance_id=$2 and status='claimed' and expires_at>now()`, pairingID, instanceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if result.RowsAffected() != 1 {
		writeError(w, http.StatusConflict, errors.New("pairing is not ready for confirmation"))
		return
	}
	if _, err := tx.Exec(r.Context(), `update cloud_access_tokens set activated_at=now() where pairing_id=$1 and activated_at is null and revoked_at is null`, pairingID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "confirmed", "pairingId": pairingID, "instanceId": instanceID})
}

func (s *Server) pairingStatus(w http.ResponseWriter, r *http.Request) {
	var status string
	var expires time.Time
	if err := s.db.QueryRow(r.Context(), `select status,expires_at from pairing_sessions where pairing_id=$1`, chi.URLParam(r, "pairingID")).Scan(&status, &expires); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("pairing session not found"))
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if status == "pending" || status == "claimed" {
		if !expires.After(time.Now().UTC()) {
			status = "expired"
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": status, "expiresAt": expires})
}

func (s *Server) claimPairing(w http.ResponseWriter, r *http.Request) {
	pairingID := chi.URLParam(r, "pairingID")
	var input struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &input) || len(strings.TrimSpace(input.Code)) != 6 {
		return
	}
	s.claimPairingSession(w, r, pairingID, strings.TrimSpace(input.Code))
}

// claimPairingByCode is the camera-free pairing path. Pairing codes are
// short-lived and rate-limited by the same claim counter as QR claims.
func (s *Server) claimPairingByCode(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &input) || len(strings.TrimSpace(input.Code)) != 6 {
		return
	}
	code := strings.TrimSpace(input.Code)
	rows, err := s.db.Query(r.Context(), `select pairing_id from pairing_sessions where code_hash=$1 and status='pending' and expires_at>now() limit 2`, hashCode(code))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()
	var pairingID string
	if !rows.Next() {
		// Keep the failure actionable when the code was already claimed. The
		// code is short-lived and single-use, so callers must generate a new one.
		var status string
		var expires time.Time
		if err := s.db.QueryRow(r.Context(), `select status,expires_at from pairing_sessions where code_hash=$1 order by created_at desc limit 1`, hashCode(code)).Scan(&status, &expires); err == nil {
			if status != "pending" || !expires.After(time.Now().UTC()) {
				writeError(w, http.StatusGone, errors.New("pairing code has expired or was already used"))
				return
			}
		}
		writeError(w, http.StatusNotFound, errors.New("pairing code is invalid or expired"))
		return
	}
	if err := rows.Scan(&pairingID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if rows.Next() {
		writeError(w, http.StatusConflict, errors.New("pairing code is ambiguous; generate a new code"))
		return
	}
	s.claimPairingSession(w, r, pairingID, code)
}

func (s *Server) claimPairingSession(w http.ResponseWriter, r *http.Request, pairingID, code string) {
	var attempts int
	err := s.db.QueryRow(r.Context(), `update pairing_sessions set claim_attempts=claim_attempts+1 where pairing_id=$1 and status='pending' and expires_at>now() and claim_attempts<10 returning claim_attempts`, pairingID).Scan(&attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		var status string
		checkErr := s.db.QueryRow(r.Context(), `select status from pairing_sessions where pairing_id=$1`, pairingID).Scan(&status)
		if checkErr == nil && status == "pending" {
			writeError(w, http.StatusTooManyRequests, errors.New("too many pairing attempts"))
		} else {
			writeError(w, http.StatusNotFound, errors.New("pairing code is invalid"))
		}
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	var instanceID, status string
	var expires time.Time
	err = s.db.QueryRow(r.Context(), `select instance_id,status,expires_at from pairing_sessions where pairing_id=$1 and code_hash=$2`, pairingID, hashCode(code)).Scan(&instanceID, &status, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, errors.New("pairing code is invalid"))
		return
	}
	if err != nil {
		writeError(w, 500, err)
		return
	}
	if status != "pending" || expires.Before(time.Now().UTC()) {
		writeError(w, 410, errors.New("pairing code has expired or was already used"))
		return
	}
	result, err := s.db.Exec(r.Context(), `update pairing_sessions set status='claimed',claimed_at=now() where pairing_id=$1 and status='pending' and expires_at>now()`, pairingID)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	changed := result.RowsAffected()
	if changed != 1 {
		writeError(w, 409, errors.New("pairing code was already claimed"))
		return
	}
	var tokenBytes [32]byte
	if _, err := rand.Read(tokenBytes[:]); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	accessToken := fmt.Sprintf("mvt_%x", tokenBytes[:])
	// The token is recorded against its pairing session but stays inactive:
	// userAuth rejects tokens with a null activated_at, and only the desktop
	// side (agentConfirmPairing) can activate it. Claiming a QR code therefore
	// grants nothing on its own — the person at the computer must confirm.
	if _, err := s.db.Exec(r.Context(), `insert into cloud_access_tokens(token_hash,instance_id,expires_at,pairing_id) values($1,$2,$3,$4)`, hashCode(accessToken), instanceID, time.Now().UTC().Add(90*24*time.Hour), pairingID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "claimed", "pairingId": pairingID, "instanceId": instanceID, "accessToken": accessToken, "pendingConfirmation": true})
}

func (s *Server) instanceSnapshot(w http.ResponseWriter, r *http.Request) {
	if !authorizedInstance(r, chi.URLParam(r, "instanceID")) {
		writeError(w, http.StatusForbidden, errors.New("instance access denied"))
		return
	}
	var snapshot []byte
	var revision int64
	var observed time.Time
	err := s.db.QueryRow(r.Context(), `select snapshot,snapshot_revision,updated_at from cloud_instances where instance_id=$1`, chi.URLParam(r, "instanceID")).Scan(&snapshot, &revision, &observed)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, errors.New("instance not found"))
		return
	}
	if err != nil {
		writeError(w, 500, err)
		return
	}
	// A phone that already holds this revision needs nothing more. During an AI
	// reply the phone re-fetches the snapshot repeatedly while the real updates
	// arrive over the event stream, so answering with a small marker instead of
	// the whole project/history payload saves a lot of mobile data.
	if raw := strings.TrimSpace(r.URL.Query().Get("revision")); raw != "" {
		if since, parseErr := strconv.ParseInt(raw, 10, 64); parseErr == nil && since > 0 && since == revision {
			writeJSON(w, http.StatusOK, map[string]any{"unchanged": true, "snapshotRevision": revision, "observedAt": observed})
			return
		}
	}
	var value map[string]any
	if json.Unmarshal(snapshot, &value) != nil {
		value = map[string]any{"projects": []any{}}
	}
	value["snapshotRevision"] = revision
	value["observedAt"] = observed
	if strings.Contains(strings.ToLower(r.Header.Get("Accept-Encoding")), "gzip") {
		w.Header().Set("X-Milevia-Compress", "1")
	}
	writeJSON(w, 200, value)
}

// mobileEventStream provides a lightweight downlink for mobile clients. The
// desktop Agent remains the source of truth; message payloads are forwarded so
// the client can render immediately, while snapshots remain the recovery path.
func (s *Server) mobileEventStream(w http.ResponseWriter, r *http.Request) {
	instanceID := strings.TrimSpace(r.URL.Query().Get("instanceId"))
	if instanceID == "" || !authorizedInstance(r, instanceID) {
		writeError(w, http.StatusForbidden, errors.New("instance access denied"))
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, errors.New("streaming is unavailable"))
		return
	}
	lastSequence := int64(0)
	if raw := r.URL.Query().Get("after"); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil && parsed > 0 {
			lastSequence = parsed
		}
	}
	// EventSource reconnects automatically and carries the last delivered
	// event in this header. Honor it so reconnects resume from the right point
	// instead of replaying the entire event history.
	if raw := strings.TrimSpace(r.Header.Get("Last-Event-ID")); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil && parsed > lastSequence {
			lastSequence = parsed
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	writeEvent := func(event eventEnvelope) {
		data, _ := json.Marshal(event)
		fmt.Fprintf(w, "id: %d\ndata: %s\n\n", event.AgentSequence, data)
		flusher.Flush()
	}
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()
	// Keep relay latency below the Agent outbox interval while avoiding a
	// tight database polling loop for every connected mobile client.
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	// A reconnect or concurrent upload can deliver sequences out of order. Keep
	// a bounded sent set and rescan the retained tail so a late lower sequence
	// is still delivered instead of being hidden by the largest cursor.
	sentSequences := make(map[int64]struct{})
	firstPoll := true
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case <-ticker.C:
			fromSequence := lastSequence
			if !firstPoll {
				fromSequence = lastSequence - cloudEventRetentionPerInstance
			}
			if fromSequence < 0 {
				fromSequence = 0
			}
			firstPoll = false
			for sequence := range sentSequences {
				if sequence <= fromSequence {
					delete(sentSequences, sequence)
				}
			}
			rows, err := s.db.Query(r.Context(), `select event_id,agent_sequence,type,task_id,task_run_id,payload,created_at from cloud_events where instance_id=$1 and agent_sequence>$2 order by agent_sequence limit 100`, instanceID, fromSequence)
			if err != nil {
				return
			}
			for rows.Next() {
				var event eventEnvelope
				var payload []byte
				if err := rows.Scan(&event.EventID, &event.AgentSequence, &event.Type, &event.TaskID, &event.TaskRunID, &payload, &event.CreatedAt); err != nil {
					rows.Close()
					return
				}
				event.InstanceID = instanceID
				event.Payload = json.RawMessage(payload)
				if _, alreadySent := sentSequences[event.AgentSequence]; alreadySent {
					continue
				}
				writeEvent(event)
				sentSequences[event.AgentSequence] = struct{}{}
				if event.AgentSequence > lastSequence {
					lastSequence = event.AgentSequence
				}
			}
			rows.Close()
		}
	}
}

func (s *Server) userAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if provided != "" && s.config.UserToken != "" && subtle.ConstantTimeCompare([]byte(provided), []byte(s.config.UserToken)) == 1 {
			next.ServeHTTP(w, r)
			return
		}
		if provided == "" {
			writeError(w, http.StatusUnauthorized, errors.New("invalid user token"))
			return
		}
		// activated_at is set only by the desktop-side confirmation. A token
		// returned by claimPairing is therefore inert until someone at the
		// computer approves the pairing, which is what makes the confirmation
		// step a real authorization boundary rather than a client-side ritual.
		var instanceID string
		var expires *time.Time
		err := s.db.QueryRow(r.Context(), `select instance_id,expires_at from cloud_access_tokens where token_hash=$1 and revoked_at is null and activated_at is not null`, hashCode(provided)).Scan(&instanceID, &expires)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && expires != nil && !expires.After(time.Now().UTC())) {
			writeError(w, http.StatusUnauthorized, errors.New("invalid user token"))
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), instanceScopeKey{}, instanceID)))
	})
}

func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := strings.TrimRight(strings.TrimSpace(r.Header.Get("Origin")), "/")
		allowed := isAllowedWebOrigin(origin, appOrigin(s.config.AppURL))
		if allowed {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key, Last-Event-ID")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		}
		if r.Method == http.MethodOptions {
			if !allowed {
				writeError(w, http.StatusForbidden, errors.New("origin is not allowed"))
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Capacitor serves bundled Android assets from https://localhost. It is a
// fixed, non-public origin and is safe to allow alongside the configured web
// origin; arbitrary origins must remain denied.
func isAllowedWebOrigin(origin, configured string) bool {
	if origin == "" {
		return false
	}
	if configured != "" && origin == configured {
		return true
	}
	return origin == "https://localhost" || origin == "capacitor://localhost"
}

func appOrigin(rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return ""
	}
	host := strings.ToLower(parsed.Host)
	if scheme == "https" {
		host = strings.TrimSuffix(host, ":443")
	} else if scheme == "http" {
		host = strings.TrimSuffix(host, ":80")
	}
	return scheme + "://" + host
}

// agentOriginAllowed gates the Agent WebSocket upgrade. The Agent is not a
// browser and sends no Origin header, which must stay acceptable; when an
// Origin is present it has to belong to this deployment. Browsers cannot forge
// Origin, so this blocks cross-site WebSocket attempts from other pages while
// the Agent token remains the real gate.
func (s *Server) agentOriginAllowed(origin string) bool {
	origin = strings.TrimRight(strings.TrimSpace(origin), "/")
	return origin == "" || isAllowedWebOrigin(origin, appOrigin(s.config.AppURL))
}

// agentAuth authenticates an Agent request. It returns ok=false with a nil
// error for a genuine rejection (missing, mismatched or revoked credential),
// and ok=false with a non-nil error when the database is momentarily
// unavailable. Callers must distinguish the two: only the nil-error case
// should be reported as 401, because the Agent treats 401 as "credential
// revoked", discards its local DPAPI secret and re-enrolls. Reporting a
// transient DB failure as 401 would cascade into re-registration and orphaned
// instances on every connection during an outage.
func (s *Server) agentAuth(r *http.Request, instanceID string) (bool, error) {
	provided := r.Header.Get("X-Milevia-Agent-Token")
	if s.db != nil {
		var tokenHash string
		var revokedAt *time.Time
		err := s.db.QueryRow(r.Context(), `select token_hash,revoked_at from cloud_agent_credentials where instance_id=$1`, instanceID).Scan(&tokenHash, &revokedAt)
		if err == nil {
			if revokedAt != nil {
				return false, nil
			}
			return subtle.ConstantTimeCompare([]byte(hashCode(provided)), []byte(tokenHash)) == 1, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			// The DB did not answer. Refuse to authenticate rather than fall
			// through to the static config, and surface a 5xx (via agentAuthDenied)
			// so the Agent does not misread this as revocation.
			return false, err
		}
	}
	// Legacy deployments keep static credentials in configuration. Only use
	// that fallback when no database-backed credential exists for the instance;
	// this ensures revocation cannot be bypassed by an old static entry.
	expected, ok := s.config.AgentTokens[instanceID]
	return ok && expected != "" && subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1, nil
}

// agentAuthDenied writes the authentication failure response and reports
// whether the request must be rejected. ok=false with a DB/auth error is
// surfaced as 503 so a transient database failure is not mistaken for revoked
// credentials; ok=false with no error is a genuine rejection (401).
func agentAuthDenied(w http.ResponseWriter, ok bool, authErr error) bool {
	if ok {
		return false
	}
	if authErr != nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("agent authentication temporarily unavailable"))
		return true
	}
	writeError(w, http.StatusUnauthorized, errors.New("invalid agent token"))
	return true
}

func (s *Server) listInstances(w http.ResponseWriter, r *http.Request) {
	scope := scopedInstance(r)
	query := `select instance_id,name,status,last_agent_sequence,last_seen_at from cloud_instances`
	args := []any{}
	if scope != "" {
		query += ` where instance_id=$1`
		args = append(args, scope)
	}
	query += ` order by updated_at desc`
	rows, err := s.db.Query(r.Context(), query, args...)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	defer rows.Close()
	items := make([]map[string]any, 0)
	for rows.Next() {
		var id, name, status string
		var sequence int64
		var seen *time.Time
		if err := rows.Scan(&id, &name, &status, &sequence, &seen); err != nil {
			writeError(w, 500, err)
			return
		}
		items = append(items, map[string]any{"instanceId": id, "name": name, "status": status, "lastAgentSequence": sequence, "lastSeenAt": seen})
	}
	writeJSON(w, 200, items)
}

func (s *Server) instanceOverview(w http.ResponseWriter, r *http.Request) {
	if !authorizedInstance(r, chi.URLParam(r, "instanceID")) {
		writeError(w, http.StatusForbidden, errors.New("instance access denied"))
		return
	}
	var id, name, status string
	var sequence int64
	var seen *time.Time
	err := s.db.QueryRow(r.Context(), `select instance_id,name,status,last_agent_sequence,last_seen_at from cloud_instances where instance_id=$1`, chi.URLParam(r, "instanceID")).Scan(&id, &name, &status, &sequence, &seen)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, errors.New("instance not found"))
		return
	}
	if err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"instanceId": id, "name": name, "status": status, "lastAgentSequence": sequence, "lastSeenAt": seen})
}

// revokeInstanceTokens revokes access to one computer. Two very different
// things can be revoked, so the caller states which:
//
//	scope=mobile (default): forget the phones paired with this computer. The
//	    Agent credential and the relay connection stay untouched, because the
//	    computer itself is not what the user asked to disconnect.
//	scope=agent: additionally retire the machine's own credential, taking the
//	    computer off the relay until an operator enrolls it again.
//
// Conflating the two — the original behaviour — meant "remove my phone"
// silently unplugged the desktop as well, and on a release build the Agent had
// no way to re-enroll on its own.
func (s *Server) revokeInstanceTokens(w http.ResponseWriter, r *http.Request) {
	instanceID := chi.URLParam(r, "instanceID")
	if !authorizedInstance(r, instanceID) {
		writeError(w, http.StatusForbidden, errors.New("instance access denied"))
		return
	}
	scope := "mobile"
	// A missing or chunked body simply means "use the default scope"; only a
	// declared, non-empty body is parsed.
	if r.ContentLength > 0 {
		var input struct {
			Scope string `json:"scope"`
		}
		if !decode(w, r, &input) {
			return
		}
		if trimmed := strings.TrimSpace(input.Scope); trimmed != "" {
			scope = trimmed
		}
	}
	if scope != "mobile" && scope != "agent" {
		writeError(w, http.StatusBadRequest, errors.New("scope must be either mobile or agent"))
		return
	}
	if scope == "mobile" {
		if _, err := s.db.Exec(r.Context(), `update cloud_access_tokens set revoked_at=now() where instance_id=$1 and revoked_at is null`, instanceID); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "revoked", "scope": "mobile", "instanceId": instanceID})
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback(r.Context())
	if _, err := tx.Exec(r.Context(), `update cloud_access_tokens set revoked_at=now() where instance_id=$1 and revoked_at is null`, instanceID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if _, err := tx.Exec(r.Context(), `update cloud_agent_credentials set revoked_at=now() where instance_id=$1 and revoked_at is null`, instanceID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if _, err := tx.Exec(r.Context(), `update cloud_instances set status='offline',updated_at=now() where instance_id=$1`, instanceID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// Drop the active relay connection as soon as credentials are revoked. The
	// Agent will fail subsequent reconnects until it is explicitly re-enrolled.
	s.mu.Lock()
	conn := s.connections[instanceID]
	if conn != nil {
		delete(s.connections, instanceID)
	}
	s.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "revoked", "scope": "agent", "instanceId": instanceID})
}

func (s *Server) createCommand(w http.ResponseWriter, r *http.Request) {
	instanceID := chi.URLParam(r, "instanceID")
	if !authorizedInstance(r, instanceID) {
		writeError(w, http.StatusForbidden, errors.New("instance access denied"))
		return
	}
	// Refuse commands aimed at a computer that is not connected. Accepting one
	// would leave it queued, expire it minutes later, and leave the user
	// believing the task had been dispatched — the command never reaches the
	// machine because there is no relay connection to push it over.
	var instanceStatus string
	err := s.db.QueryRow(r.Context(), `select status from cloud_instances where instance_id=$1`, instanceID).Scan(&instanceStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("instance not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if instanceStatus != "online" {
		writeError(w, http.StatusConflict, errors.New("instance_offline: the computer is not connected right now"))
		return
	}
	idempotency := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotency == "" {
		writeError(w, 400, errors.New("Idempotency-Key is required"))
		return
	}
	var input commandRequest
	if !decode(w, r, &input) || input.Type == "" {
		return
	}
	allowed := map[string]bool{"task.create": true, "task.update": true, "task.delete": true, "task.dispatch": true, "task.stop": true, "task.review": true, "task.reopen": true, "conversation.create": true, "conversation.message": true}
	if !allowed[input.Type] {
		writeError(w, 400, errors.New("unsupported command type"))
		return
	}
	if len(input.Payload) > 256<<10 || (len(input.Payload) > 0 && !json.Valid(input.Payload)) {
		writeError(w, http.StatusBadRequest, errors.New("payload must be valid JSON and smaller than 256 KiB"))
		return
	}
	expires := time.Now().UTC().Add(5 * time.Minute)
	if input.ExpiresAt != nil {
		expires = input.ExpiresAt.UTC()
	}
	if expires.Before(time.Now().UTC()) || expires.After(time.Now().UTC().Add(15*time.Minute)) {
		writeError(w, 400, errors.New("expiresAt must be within 15 minutes"))
		return
	}
	commandID := newID()
	var status, existingHash string
	hash := hashCode(fmt.Sprintf("%s\x00%s\x00%s\x00%s", input.Type, input.ProjectID, input.TaskID, string(input.Payload)))
	created := true
	err = s.db.QueryRow(r.Context(), `insert into cloud_commands(command_id,instance_id,idempotency_key,type,project_id,task_id,payload,expires_at,request_hash) values($1,$2,$3,$4,$5,$6,$7,$8,$9) on conflict(instance_id,idempotency_key,type) do nothing returning command_id,status,request_hash`, commandID, instanceID, idempotency, input.Type, input.ProjectID, input.TaskID, input.Payload, expires, hash).Scan(&commandID, &status, &existingHash)
	if errors.Is(err, pgx.ErrNoRows) {
		created = false
		err = s.db.QueryRow(r.Context(), `select command_id,status,request_hash from cloud_commands where instance_id=$1 and idempotency_key=$2 and type=$3`, instanceID, idempotency, input.Type).Scan(&commandID, &status, &existingHash)
	}
	if err != nil {
		writeError(w, 500, err)
		return
	}
	if existingHash != "" && existingHash != hash {
		writeError(w, http.StatusConflict, errors.New("Idempotency-Key conflicts with a different command"))
		return
	}
	if status == "" {
		status = "queued"
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"commandId": commandID, "status": status})
	if created || status == "queued" {
		s.pushCommand(instanceID, commandID)
	}
}

func (s *Server) getCommand(w http.ResponseWriter, r *http.Request) {
	commandID := chi.URLParam(r, "commandID")
	var commandInstanceID string
	if err := s.db.QueryRow(r.Context(), `select instance_id from cloud_commands where command_id=$1`, commandID).Scan(&commandInstanceID); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("command not found"))
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// Do not mutate a command until the caller is authorized for its instance.
	if !authorizedInstance(r, commandInstanceID) {
		writeError(w, http.StatusForbidden, errors.New("instance access denied"))
		return
	}
	// expires_at bounds how long a command may wait for delivery, not how long
	// it may run. Only commands that were never picked up expire; marking a
	// received or executing command as expired would tell the user a running
	// task had failed to start.
	if _, err := s.db.Exec(r.Context(), `update cloud_commands set status='expired',updated_at=now() where command_id=$1 and instance_id=$2 and expires_at<=now() and status='queued'`, commandID, commandInstanceID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// A command that was delivered but never reached a terminal state (the
	// Agent stopped mid-flight, for example) must not report "in progress"
	// forever: the phone polls until it sees a terminal status, so a stall
	// would mean endless polling over a result the user can never act on.
	if _, err := s.db.Exec(r.Context(), `update cloud_commands set status='indeterminate',updated_at=now() where command_id=$1 and instance_id=$2 and status in ('received','executing') and updated_at < now() - interval '6 hours'`, commandID, commandInstanceID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	var instanceID, typ, projectID, taskID, status string
	var payload, result []byte
	var expires, created, updated time.Time
	err := s.db.QueryRow(r.Context(), `select command_id,instance_id,type,project_id,task_id,payload,status,result,expires_at,created_at,updated_at from cloud_commands where command_id=$1`, commandID).Scan(&commandID, &instanceID, &typ, &projectID, &taskID, &payload, &status, &result, &expires, &created, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, errors.New("command not found"))
		return
	}
	if err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"commandId": commandID, "instanceId": instanceID, "type": typ, "projectId": projectID, "taskId": taskID, "payload": json.RawMessage(payload), "status": status, "result": json.RawMessage(result), "expiresAt": expires, "createdAt": created, "updatedAt": updated})
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	instanceID := r.URL.Query().Get("instanceId")
	if instanceID == "" {
		writeError(w, 400, errors.New("instanceId is required"))
		return
	}
	if !authorizedInstance(r, instanceID) {
		writeError(w, http.StatusForbidden, errors.New("instance access denied"))
		return
	}
	rows, err := s.db.Query(r.Context(), `select event_id,agent_sequence,type,task_id,task_run_id,payload,created_at from cloud_events where instance_id=$1 order by agent_sequence desc limit 500`, instanceID)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	defer rows.Close()
	items := make([]eventEnvelope, 0)
	for rows.Next() {
		var event eventEnvelope
		var payload []byte
		if err := rows.Scan(&event.EventID, &event.AgentSequence, &event.Type, &event.TaskID, &event.TaskRunID, &payload, &event.CreatedAt); err != nil {
			writeError(w, 500, err)
			return
		}
		event.InstanceID = instanceID
		event.Payload = json.RawMessage(payload)
		items = append(items, event)
	}
	writeJSON(w, 200, items)
}

func (s *Server) agentConnect(w http.ResponseWriter, r *http.Request) {
	instanceID := strings.TrimSpace(r.URL.Query().Get("instanceId"))
	if instanceID == "" {
		writeError(w, 400, errors.New("instanceId is required"))
		return
	}
	if ok, authErr := s.agentAuth(r, instanceID); agentAuthDenied(w, ok, authErr) {
		return
	}
	upgrader := websocket.Upgrader{
		ReadBufferSize:  64 << 10,
		WriteBufferSize: 64 << 10,
		CheckOrigin: func(r *http.Request) bool { return s.agentOriginAllowed(r.Header.Get("Origin")) },
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(512 << 10)
	_ = conn.SetReadDeadline(time.Now().Add(45 * time.Second))
	s.mu.Lock()
	if previous := s.connections[instanceID]; previous != nil {
		_ = previous.Close()
	}
	s.connections[instanceID] = conn
	s.mu.Unlock()
	defer func() {
		wasCurrent := false
		s.mu.Lock()
		if s.connections[instanceID] == conn {
			delete(s.connections, instanceID)
			wasCurrent = true
		}
		s.mu.Unlock()
		if wasCurrent {
			_, _ = s.db.Exec(context.Background(), `update cloud_instances set status='offline',updated_at=now() where instance_id=$1 and status='online'`, instanceID)
		}
		_ = conn.Close()
	}()
	_, _ = s.db.Exec(r.Context(), `insert into cloud_instances(instance_id,status,last_seen_at) values($1,'online',now()) on conflict(instance_id) do update set status='online',last_seen_at=now(),updated_at=now()`, instanceID)
	s.sendPendingCommands(r.Context(), instanceID, conn)
	for {
		var raw json.RawMessage
		if err := conn.ReadJSON(&raw); err != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(45 * time.Second))
		var kind struct {
			Kind string `json:"kind"`
		}
		_ = json.Unmarshal(raw, &kind)
		if kind.Kind == "ping" {
			s.agentWrite(instanceID, conn, map[string]string{"kind": "pong"})
			continue
		}
		if kind.Kind == "command.status" {
			var status struct {
				CommandID string          `json:"commandId"`
				Status    string          `json:"status"`
				Result    json.RawMessage `json:"result"`
			}
			if json.Unmarshal(raw, &status) == nil && status.CommandID != "" && validCommandStatus(status.Status) {
				result := status.Result
				if len(result) == 0 || !json.Valid(result) {
					result = json.RawMessage(`{}`)
				}
				_, _ = s.db.Exec(r.Context(), `update cloud_commands set status=$3,result=$4::jsonb,updated_at=now() where command_id=$1 and instance_id=$2 and status not in ('completed','failed','expired','cancelled','indeterminate')`, status.CommandID, instanceID, status.Status, result)
			}
			continue
		}
		if kind.Kind == "event.ack" {
			continue
		}
		var envelope eventEnvelope
		if json.Unmarshal(raw, &envelope) != nil {
			continue
		}
		if envelope.EventID == "" || envelope.AgentSequence <= 0 || envelope.Type == "" || len(envelope.Payload) == 0 || !json.Valid(envelope.Payload) || envelope.CreatedAt.IsZero() {
			continue
		}
		if err := s.storeEvent(r.Context(), instanceID, envelope); err != nil {
			if isEventConflict(err) {
				// Dropping the connection here would be destructive: the Agent
				// would reconnect, resend the same conflicting event, and never
				// deliver the events queued behind it. Reject just this event
				// so the Agent can discard it and continue with the next one.
				_ = s.agentWrite(instanceID, conn, map[string]any{
					"kind":          "event.reject",
					"eventId":       envelope.EventID,
					"agentSequence": envelope.AgentSequence,
					"reason":        err.Error(),
				})
				continue
			}
			// Transient database failure: send neither an ack nor a reject, and
			// keep the connection. The Agent still holds the event and will
			// retry once the database recovers.
			continue
		}
		s.agentWrite(instanceID, conn, map[string]any{"kind": "event.ack", "eventId": envelope.EventID, "agentSequence": envelope.AgentSequence})
	}
}

func (s *Server) agentEvents(w http.ResponseWriter, r *http.Request) {
	instanceID := strings.TrimSpace(r.Header.Get("X-Milevia-Instance-ID"))
	if instanceID == "" {
		writeError(w, 400, errors.New("X-Milevia-Instance-ID is required"))
		return
	}
	if ok, authErr := s.agentAuth(r, instanceID); agentAuthDenied(w, ok, authErr) {
		return
	}
	if err := s.ensureInstance(r.Context(), instanceID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	var events []eventEnvelope
	if !decode(w, r, &events) {
		return
	}
	// Conflicting events are reported per-event instead of failing the whole
	// batch: a single stale event must not force the Agent to replay healthy
	// events behind it forever.
	rejected := make([]map[string]any, 0)
	accepted := 0
	for _, event := range events {
		if event.EventID == "" || event.AgentSequence <= 0 || event.Type == "" || len(event.Payload) == 0 || !json.Valid(event.Payload) || event.CreatedAt.IsZero() {
			writeError(w, http.StatusBadRequest, errors.New("invalid event envelope"))
			return
		}
		if err := s.storeEvent(r.Context(), instanceID, event); err != nil {
			if isEventConflict(err) {
				rejected = append(rejected, map[string]any{
					"eventId":       event.EventID,
					"agentSequence": event.AgentSequence,
					"reason":        err.Error(),
				})
				continue
			}
			writeError(w, 500, err)
			return
		}
		accepted++
	}
	writeJSON(w, 200, map[string]any{"accepted": accepted, "rejected": rejected})
}

func (s *Server) agentSnapshot(w http.ResponseWriter, r *http.Request) {
	instanceID := strings.TrimSpace(r.Header.Get("X-Milevia-Instance-ID"))
	if instanceID == "" {
		writeError(w, 400, errors.New("X-Milevia-Instance-ID is required"))
		return
	}
	if ok, authErr := s.agentAuth(r, instanceID); agentAuthDenied(w, ok, authErr) {
		return
	}
	if err := s.ensureInstance(r.Context(), instanceID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	var snapshot json.RawMessage
	// Snapshots contain recent conversation text. They are intentionally
	// larger than ordinary command payloads, which keep the conservative
	// 512 KiB decode limit below.
	if !decodeWithLimit(w, r, &snapshot, 32<<20) || !json.Valid(snapshot) {
		return
	}
	var snapshotObject map[string]json.RawMessage
	if json.Unmarshal(snapshot, &snapshotObject) != nil {
		writeError(w, http.StatusBadRequest, errors.New("snapshot must be a JSON object"))
		return
	}
	var revision int64
	if value := map[string]any{}; json.Unmarshal(snapshot, &value) == nil {
		if number, ok := value["snapshotRevision"].(float64); ok {
			revision = int64(number)
		}
	}
	// A reconnecting Agent may upload an older snapshot after a newer one has
	// already arrived. Keep both the revision and the payload monotonic; the
	// previous statement only protected the revision while still overwriting
	// the payload with stale content.
	_, err := s.db.Exec(r.Context(), `update cloud_instances set snapshot=case when $3>=snapshot_revision then $2::jsonb else snapshot end,snapshot_revision=greatest(snapshot_revision,$3),last_seen_at=now(),status='online',updated_at=now() where instance_id=$1`, instanceID, snapshot, revision)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"status": "accepted", "snapshotRevision": revision})
}

// Permanent event conflicts. These describe a durably inconsistent event (the
// same sequence carrying a different event, or the same event carrying a
// different sequence). Retrying them can never succeed, so callers must drop
// the event instead of reconnecting or backing off.
var (
	errEventInsertConflict   = errors.New("event insert conflict")
	errEventSequenceConflict = errors.New("event sequence conflict")
	errEventIDConflict       = errors.New("event id conflict")
)

// isEventConflict reports whether an error from storeEvent is a permanent
// conflict rather than a transient database failure.
func isEventConflict(err error) bool {
	return errors.Is(err, errEventInsertConflict) ||
		errors.Is(err, errEventSequenceConflict) ||
		errors.Is(err, errEventIDConflict)
}

func (s *Server) storeEvent(ctx context.Context, instanceID string, event eventEnvelope) error {
	if err := s.ensureInstance(ctx, instanceID); err != nil {
		return err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// The unique constraints make the insert atomic; the follow-up checks turn
	// a concurrent retry into a successful no-op while still rejecting conflicts.
	var insertedID string
	err = tx.QueryRow(ctx, `insert into cloud_events(event_id,instance_id,agent_sequence,type,task_id,task_run_id,payload,created_at) values($1,$2,$3,$4,$5,$6,$7,$8) on conflict do nothing returning event_id`, event.EventID, instanceID, event.AgentSequence, event.Type, event.TaskID, event.TaskRunID, event.Payload, event.CreatedAt).Scan(&insertedID)
	if errors.Is(err, pgx.ErrNoRows) {
		var existingID string
		err = tx.QueryRow(ctx, `select event_id from cloud_events where instance_id=$1 and agent_sequence=$2`, instanceID, event.AgentSequence).Scan(&existingID)
		if errors.Is(err, pgx.ErrNoRows) {
			var existingSequence int64
			err = tx.QueryRow(ctx, `select agent_sequence from cloud_events where instance_id=$1 and event_id=$2`, instanceID, event.EventID).Scan(&existingSequence)
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w for instance %s", errEventInsertConflict, instanceID)
			}
			if err != nil {
				return err
			}
			if existingSequence != event.AgentSequence {
				return fmt.Errorf("%w for instance %s: %s", errEventIDConflict, instanceID, event.EventID)
			}
		} else if err != nil {
			return err
		} else if existingID != event.EventID {
			return fmt.Errorf("%w for instance %s: sequence %d", errEventSequenceConflict, instanceID, event.AgentSequence)
		}
	} else if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	// Pruning is best-effort and deliberately outside the event correctness
	// path. A transient cleanup failure must not disconnect the Agent relay.
	if event.AgentSequence%cloudEventPruneInterval == 0 {
		_, _ = s.db.Exec(ctx, `delete from cloud_events where instance_id=$1 and agent_sequence < (select greatest(coalesce(max(agent_sequence),0)-$2,0) from cloud_events where instance_id=$1)`, instanceID, cloudEventRetentionPerInstance)
	}
	_, err = s.db.Exec(ctx, `update cloud_instances set status='online',last_agent_sequence=greatest(last_agent_sequence,$2),last_seen_at=now(),updated_at=now() where instance_id=$1`, instanceID, event.AgentSequence)
	return err
}

func (s *Server) sendPendingCommands(ctx context.Context, instanceID string, conn *websocket.Conn) {
	// Re-deliver the never-delivered queue *and* the commands the Agent last
	// reported as in-flight. The latter is what lets a command recover after an
	// Agent restart: re-submitting is safe because the local side is
	// idempotent by key and answers with the existing record, and the Agent
	// then relays whatever terminal state that record has reached. Without
	// this, a command that reached "received" before a crash would stay there
	// forever (the old query only re-sent status='queued').
	rows, err := s.db.Query(ctx, `select command_id,idempotency_key,type,project_id,task_id,payload,expires_at from cloud_commands where instance_id=$1 and ((status='queued' and expires_at>now()) or status in ('received','executing')) order by created_at limit 100`, instanceID)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id, idempotency, typ, projectID, taskID string
		var payload []byte
		var expires time.Time
		if rows.Scan(&id, &idempotency, &typ, &projectID, &taskID, &payload, &expires) != nil {
			return
		}
		if err := s.agentWrite(instanceID, conn, map[string]any{"commandId": id, "idempotencyKey": idempotency, "type": typ, "projectId": projectID, "taskId": taskID, "payload": json.RawMessage(payload), "expiresAt": expires}); err != nil {
			return
		}
	}
}

func (s *Server) pushCommand(instanceID, commandID string) {
	s.mu.Lock()
	conn := s.connections[instanceID]
	s.mu.Unlock()
	if conn == nil {
		return
	}
	var idempotency, typ, projectID, taskID string
	var payload []byte
	var expires time.Time
	if err := s.db.QueryRow(context.Background(), `select idempotency_key,type,project_id,task_id,payload,expires_at from cloud_commands where command_id=$1`, commandID).Scan(&idempotency, &typ, &projectID, &taskID, &payload, &expires); err != nil {
		return
	}
	if err := s.agentWrite(instanceID, conn, map[string]any{"commandId": commandID, "idempotencyKey": idempotency, "type": typ, "projectId": projectID, "taskId": taskID, "payload": json.RawMessage(payload), "expiresAt": expires}); err != nil {
		return
	}
}

func (s *Server) ensureInstance(ctx context.Context, instanceID string) error {
	_, err := s.db.Exec(ctx, `insert into cloud_instances(instance_id,status,last_seen_at) values($1,'offline',null) on conflict(instance_id) do nothing`, instanceID)
	return err
}

func (s *Server) instanceExists(ctx context.Context, instanceID string) (bool, error) {
	var exists bool
	err := s.db.QueryRow(ctx, `select exists(select 1 from cloud_instances where instance_id=$1)`, instanceID).Scan(&exists)
	return exists, err
}

func validCommandStatus(status string) bool {
	switch status {
	case "received", "executing", "completed", "failed", "expired", "cancelled", "indeterminate":
		return true
	default:
		return false
	}
}

func (s *Server) agentWrite(instanceID string, conn *websocket.Conn, value any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.Lock()
	current := s.connections[instanceID]
	s.mu.Unlock()
	if current != conn {
		return errors.New("agent connection is no longer current")
	}
	return conn.WriteJSON(value)
}

func newID() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err == nil {
		return fmt.Sprintf("cmd-%x", bytes[:])
	}
	return fmt.Sprintf("cmd-%d", time.Now().UnixNano())
}

func hashCode(value string) string {
	digest := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", digest[:])
}
func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	return decodeWithLimit(w, r, target, 512<<10)
}

func decodeWithLimit(w http.ResponseWriter, r *http.Request, target any, limit int64) bool {
	defer r.Body.Close()
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit)).Decode(target); err != nil {
		writeError(w, 400, errors.New("invalid JSON request"))
		return false
	}
	return true
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		status = http.StatusInternalServerError
		data = []byte(`{"error":"failed to encode response"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	compress := w.Header().Get("X-Milevia-Compress") == "1"
	w.Header().Del("X-Milevia-Compress")
	if compress && len(data) > 1024 {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Add("Vary", "Accept-Encoding")
		w.WriteHeader(status)
		gz := gzip.NewWriter(w)
		_, _ = gz.Write(data)
		_ = gz.Close()
		return
	}
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}
func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
