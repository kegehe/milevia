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
	"log"
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
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Config struct {
	DatabaseURL     string
	AgentTokens     map[string]string
	EnrollmentToken string
	UserToken       string
	AppURL          string
}

// eventBroker fans PostgreSQL notifications out to the connected mobile event
// streams. The deployment is a single node, so no external pub/sub is needed;
// the point is that a freshly stored event wakes every stream immediately
// instead of costing each connected phone a full poll interval of dead time.
type eventBroker struct {
	mu   sync.Mutex
	subs map[chan string]string
}

func newEventBroker() *eventBroker {
	return &eventBroker{subs: map[chan string]string{}}
}

// subscribe registers a listener for exactly one instance. Filtering here rather
// than inside the stream matters twice over: a stream is never woken by another
// computer's events, and it can therefore collapse its own backlog after a drain
// without ever swallowing a wake-up meant for a different instance.
func (b *eventBroker) subscribe(instanceID string) chan string {
	ch := make(chan string, 64)
	b.mu.Lock()
	b.subs[ch] = instanceID
	b.mu.Unlock()
	return ch
}

func (b *eventBroker) unsubscribe(ch chan string) {
	b.mu.Lock()
	if _, ok := b.subs[ch]; ok {
		delete(b.subs, ch)
		close(ch)
	}
	b.mu.Unlock()
}

func (b *eventBroker) publish(instanceID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch, subscribed := range b.subs {
		if subscribed != instanceID {
			continue
		}
		select {
		case ch <- instanceID:
		default:
		}
	}
}

type Server struct {
	db          *pgxpool.Pool
	config      Config
	mu          sync.Mutex
	connections map[string]*agentConnection
	// Rate limiters for the unauthenticated endpoints. The authenticated
	// mobile API is polled by design and is not throttled here.
	registerLimiter *rateLimiter
	claimLimiter    *rateLimiter
	statusLimiter   *rateLimiter
	// events carries LISTEN notifications to the mobile event streams.
	events *eventBroker
	// rpcHub 是远程调用的请求-响应通道（见 rpc.go）。用 once 懒初始化，这样直接构造
	// Server 的测试与旧部署都不需要显式初始化它。
	rpcHub     *rpcRequestHub
	rpcHubOnce sync.Once
	// listenCancel stops the dedicated LISTEN connection when the server closes.
	listenCancel context.CancelFunc
}

// agentConnection 是一条已注册的中继连接，连同它自己的写锁。
//
// 写锁是**每条连接**的，不是全局的。gorilla 的 websocket.Conn 不允许并发写，
// 所以每条连接必须串行化自己的写；但老实现用的是一把全局锁，把"检查连接是否仍是
// 当前连接 + 写一帧"整体包住。于是任意一条连接的写一旦阻塞（对端不读、socket
// 缓冲写满），**所有实例**的中继写都被压在同一个锁后面 —— 包括手机刚下发、
// 等着送到电脑端的命令。
//
// 真机实测（2026-09-16）：事件洪峰期间云端向一台电脑推送的确认在 4.5 小时里
// 累计 792 MB，那条连接一旦写不动，命令投递就排在同一个锁后面，往返从 3 秒
// 劣化到 75 秒，越过了手机端 30 秒的等待上限。
type agentConnection struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
}

// writeJSON 串行化这条连接上的写，并给每次写设上限：没有写截止时间的阻塞写会
// 一直占着写锁，把该实例的读取循环也一起钉死（读取循环里每一帧事件都要回一个
// 确认，写不出去就读不了下一条）。
//
// 写失败（含写超时）必须**关掉连接**。gorilla 的约定是：写超时之后这条连接处于
// 未定义状态，可能已经发出去了半帧；而这里的调用方基本都不看返回值（回执路径是
// `_ = s.agentWrite(...)`），继续在一条半帧连接上写只会让对端解出垃圾帧。关掉之后
// 读循环会立刻报错、连接被注销、Agent 自己重连 —— 这正是我们要的收敛方向。
func (c *agentConnection) writeJSON(value any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(agentWriteTimeout))
	if err := c.conn.WriteJSON(value); err != nil {
		_ = c.conn.Close()
		return err
	}
	return nil
}

func (c *agentConnection) Close() error { return c.conn.Close() }

// agentWriteTimeout 是向 Agent 写一帧的上限。控制帧都很小，正常网络下远低于它；
// 超过它说明这条连接已经写不动了，应当让写失败、由上层断开重连，而不是无限等。
const agentWriteTimeout = 15 * time.Second

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
	// eventNotifyChannel is the PostgreSQL NOTIFY channel carrying instance IDs.
	// Only the instance ID travels in the payload, which keeps it far below the
	// 8000 byte NOTIFY limit and avoids duplicating event bodies.
	eventNotifyChannel = "milevia_events"
	// mobileStreamFallbackInterval bounds how long a mobile stream can stay
	// stale if a notification is lost — while the LISTEN connection reconnects,
	// or when a subscriber channel overflowed. Notifications are the fast path;
	// this only guarantees convergence.
	mobileStreamFallbackInterval = 2 * time.Second
	// cloudEventBatchSize is the most events one page of a drain reads.
	cloudEventBatchSize = 100
	// cloudEventCatchUpPerDrain caps how many events a single drain delivers
	// before returning. A client resuming from a very old cursor is caught up
	// over several drains instead of pinning the handler in one long loop.
	cloudEventCatchUpPerDrain = 500
	// cloudEventBackfillWindow is how far behind the cursor an out-of-order
	// upload is still picked up. It must not exceed cloudEventBatchSize: the
	// backfill reads the window oldest-first with a limit, so a wider window
	// would fill up with already-sent rows and hide a late arrival beyond them.
	cloudEventBackfillWindow = cloudEventBatchSize
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
		connections:     map[string]*agentConnection{},
		registerLimiter: newRateLimiter(10, 30),
		claimLimiter:    newRateLimiter(10, 30),
		statusLimiter:   newRateLimiter(60, 300),
		events:          newEventBroker(),
	}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	// The LISTEN connection deliberately does not inherit the caller's context:
	// it has to outlive whatever request or startup context created the server,
	// and only stops on Close.
	listenCtx, cancel := context.WithCancel(context.Background())
	s.listenCancel = cancel
	go s.runEventListener(listenCtx)
	return s, nil
}

// runEventListener keeps a dedicated connection subscribed to eventNotifyChannel
// and forwards matching notifications to the in-process broker.
//
// pgxpool cannot be used here: a pooled connection may be handed to another
// caller at any time, which would silently end the subscription. Reconnects are
// retried with bounded backoff, and mobile streams keep a fallback poll so a
// listener outage degrades latency instead of stalling updates.
func (s *Server) runEventListener(ctx context.Context) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		if err := s.listenOnce(ctx); err != nil && ctx.Err() == nil {
			log.Printf("postgres LISTEN %s unavailable: %v", eventNotifyChannel, err)
		}
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (s *Server) listenOnce(ctx context.Context) error {
	conn, err := pgx.Connect(ctx, s.config.DatabaseURL)
	if err != nil {
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()
	if _, err := conn.Exec(ctx, "listen "+eventNotifyChannel); err != nil {
		return err
	}
	for {
		notification, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		if notification.Channel == eventNotifyChannel && notification.Payload != "" {
			s.events.publish(notification.Payload)
		}
	}
}

func (s *Server) Close() {
	// Stop the LISTEN connection before closing the pool it was created from.
	if s.listenCancel != nil {
		s.listenCancel()
	}
	s.mu.Lock()
	connections := make([]*agentConnection, 0, len(s.connections))
	for _, conn := range s.connections {
		connections = append(connections, conn)
	}
	s.connections = map[string]*agentConnection{}
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
		// 手机名。这张表在 2026-09-17 之前没有任何"手机身份"字段，于是"这台电脑现在
		// 被哪台手机占着"无从显示，换绑时也没法告诉用户"你要顶掉的是谁"。claim 时由
		// 手机上报（旧版本不发这个字段，落在默认空串上，不影响配对）。
		`alter table cloud_access_tokens add column if not exists device_name text not null default ''`,
		// 手机的"最近同步时刻"。在此之前桌面端只能答"绑过谁"，答不了"现在还在不在用" ——
		// 用户对着手机说"明明连着"，电脑上却一个字都没有。写入点在 userAuth（每次手机带令牌
		// 请求都会经过），并且**必须节流**：手机端当前设备是 5 秒一次轮询，逐次写会在
		// cloud_access_tokens 上每 5 秒产生一个死元组，而这张表的每次读都要顺带判一遍
		// 令牌有效性 —— 没必要为一条"最近活动"读数付这个代价。详见 userAuth 里的说明。
		`alter table cloud_access_tokens add column if not exists last_used_at timestamptz`,
		// 手机平台（android / ios / web）。旧版本不报这个字段，落在默认空串上，
		// 桌面端据此**整行不渲染**，而不是显示一个空格子。
		`alter table cloud_access_tokens add column if not exists platform text not null default ''`,
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
		// 文件读写。用 POST + body 里的 op 而不是一对 REST 路径：它是一次
		// 请求-响应 RPC（见 rpc.go），路径按业务资源展开只会让"有哪些操作"
		// 散到路由表里，而它必须是集中的一份。
		r.Post("/instances/{instanceID}/rpc", s.mobileRPCRequest)
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
	r.Get("/v1/agent/bindings", s.agentBindings)
	r.Post("/v1/agent/bindings/revoke", s.agentRevokeBindings)
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
	// 这台电脑同一时间只服务一台手机：确认新手机的那一刻，把该电脑上其它手机令牌
	// 全部作废（"顶替"）。**必须和下面那句激活在同一个事务里** —— 拆成两步就会出现
	// "两台手机同时有效"的窗口，而那正是这条约束要禁止的状态。
	// 旧手机随后会拿到 401：它已有的降级路径会把用户送回配对页，文案见手机端 401 分支。
	if _, err := tx.Exec(r.Context(), `update cloud_access_tokens set revoked_at=now() where instance_id=$1 and revoked_at is null and pairing_id<>$2`, instanceID, pairingID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
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

// agentBindings reports which phones currently hold an active token for this
// computer. The desktop uses it to answer "现在是谁在用这台电脑" before it
// generates a QR code — without it, confirming a new phone silently kicks the
// old one with nothing on screen having said so.
//
// Same shape as the other agent endpoints: the machine proves itself with
// X-Milevia-Agent-Token and may only read its own instance.
func (s *Server) agentBindings(w http.ResponseWriter, r *http.Request) {
	instanceID := strings.TrimSpace(r.Header.Get("X-Milevia-Instance-ID"))
	if instanceID == "" {
		writeError(w, http.StatusBadRequest, errors.New("X-Milevia-Instance-ID is required"))
		return
	}
	if ok, authErr := s.agentAuth(r, instanceID); agentAuthDenied(w, ok, authErr) {
		return
	}
	rows, err := s.db.Query(r.Context(), `select device_name,activated_at,created_at,last_used_at,platform from cloud_access_tokens where instance_id=$1 and revoked_at is null and activated_at is not null order by activated_at desc`, instanceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()
	items := make([]map[string]any, 0)
	for rows.Next() {
		var deviceName string
		var activatedAt time.Time
		var createdAt time.Time
		// 三个可空/可缺的字段，措辞完全不同（见前端 desktop-phone.ts）：
		//   last_used_at 为 null  → "绑定后还没同步过"（迁移后的老行就是这种）
		//   整个键缺失           → 云端版本还不提供这项（页面不许把两者说成一句）
		//   platform 为空串       → 手机没上报，那一行整条不渲染
		var lastUsedAt *time.Time
		var platform string
		if err := rows.Scan(&deviceName, &activatedAt, &createdAt, &lastUsedAt, &platform); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		items = append(items, map[string]any{
			"deviceName":  deviceName,
			"activatedAt": activatedAt,
			"createdAt":   createdAt,
			"lastUsedAt":  lastUsedAt,
			"platform":    platform,
		})
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"instanceId": instanceID, "bindings": items})
}

// agentRevokeBindings 让坐在电脑前的人**主动**把当前手机解绑。
//
// 这是"换手机"的另一半：顶替发生在确认新手机的那一刻，但用户也可能只是想先断开
// （手机丢了、要借给别人用）。它和 revokeInstanceTokens(scope=mobile) 的区别只有一个：
// 调用方是这台电脑自己（agent 令牌 + 自己的 instance 头），不是手机令牌或运营者令牌 ——
// 电脑端页面根本拿不到手机令牌，只能用"我是这台机器"来证明自己有权断开自己的手机。
func (s *Server) agentRevokeBindings(w http.ResponseWriter, r *http.Request) {
	instanceID := strings.TrimSpace(r.Header.Get("X-Milevia-Instance-ID"))
	if instanceID == "" {
		writeError(w, http.StatusBadRequest, errors.New("X-Milevia-Instance-ID is required"))
		return
	}
	if ok, authErr := s.agentAuth(r, instanceID); agentAuthDenied(w, ok, authErr) {
		return
	}
	result, err := s.db.Exec(r.Context(), `update cloud_access_tokens set revoked_at=now() where instance_id=$1 and revoked_at is null`, instanceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "revoked", "instanceId": instanceID, "revoked": result.RowsAffected()})
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
		Code       string `json:"code"`
		DeviceName string `json:"deviceName"`
		Platform   string `json:"platform"`
	}
	if !decode(w, r, &input) || len(strings.TrimSpace(input.Code)) != 6 {
		return
	}
	s.claimPairingSession(w, r, pairingID, strings.TrimSpace(input.Code), sanitizeDeviceName(input.DeviceName), sanitizePlatform(input.Platform))
}

// claimPairingByCode is the camera-free pairing path. Pairing codes are
// short-lived and rate-limited by the same claim counter as QR claims.
func (s *Server) claimPairingByCode(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Code       string `json:"code"`
		DeviceName string `json:"deviceName"`
		Platform   string `json:"platform"`
	}
	if !decode(w, r, &input) || len(strings.TrimSpace(input.Code)) != 6 {
		return
	}
	code := strings.TrimSpace(input.Code)
	deviceName := sanitizeDeviceName(input.DeviceName)
	platform := sanitizePlatform(input.Platform)
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
	s.claimPairingSession(w, r, pairingID, code, deviceName, platform)
}

// sanitizeDeviceName keeps the phone-reported name to something a desktop
// screen can show: no control characters, no unbounded length. It is display
// only — never authorized against — so an empty result simply means "unknown
// phone", and the UI falls back to a generic label.
func sanitizeDeviceName(raw string) string {
	trimmed := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(raw))
	runes := []rune(trimmed)
	if len(runes) > 64 {
		runes = runes[:64]
	}
	return strings.TrimSpace(string(runes))
}

// sanitizePlatform keeps the phone-reported platform to one of a handful of
// known words. Unlike the device name it is a closed set — a client that sends
// "android; drop table" must not end up on a desktop screen or, worse, in a
// path where someone later treats it as trusted. An unknown value collapses to
// the empty string, and the desktop hides the row rather than showing a guess.
// Old app builds do not send this field at all, so empty is the normal case.
func sanitizePlatform(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "android":
		return "android"
	case "ios":
		return "ios"
	case "web":
		return "web"
	default:
		return ""
	}
}

func (s *Server) claimPairingSession(w http.ResponseWriter, r *http.Request, pairingID, code, deviceName, platform string) {
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
	if _, err := s.db.Exec(r.Context(), `insert into cloud_access_tokens(token_hash,instance_id,expires_at,pairing_id,device_name,platform) values($1,$2,$3,$4,$5,$6)`, hashCode(accessToken), instanceID, time.Now().UTC().Add(90*24*time.Hour), pairingID, deviceName, platform); err != nil {
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
	// PostgreSQL notifications are the fast path: a stored event wakes this
	// stream within one round trip instead of waiting out a poll interval. The
	// slow fallback poll exists only so that a missed notification — during a
	// LISTEN reconnect, or when this subscriber's channel overflowed — costs
	// latency rather than correctness.
	notifications := s.events.subscribe(instanceID)
	defer s.events.unsubscribe(notifications)
	fallback := time.NewTicker(mobileStreamFallbackInterval)
	defer fallback.Stop()
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	// collapseNotifications drops wake-ups that the upcoming read is about to
	// satisfy. storeEvent notifies once per stored event, so a batch of 100
	// events would otherwise cost 100 further queries that all come back empty.
	// Only this instance's wake-ups are ever queued here, so collapsing cannot
	// swallow a notification belonging to another computer.
	collapseNotifications := func() {
		for {
			select {
			case <-notifications:
			default:
				return
			}
		}
	}
	// Sequences are assigned locally, so an upload that overtakes another can
	// only land a little behind the cursor. One batch of slack absorbs that.
	sentSequences := make(map[int64]struct{})
	// A stream with no resume cursor starts at the live edge instead of
	// replaying the retained history. The client fetches a full snapshot on
	// load, so a replay would push thousands of stored events to a phone that
	// already has their result — and, because delivery is bounded per pass,
	// those stale events would delay the live ones behind them.
	if lastSequence == 0 {
		if newest, err := s.instanceEventCursor(r.Context(), instanceID); err == nil {
			lastSequence = newest
		}
	}
	// Deliver whatever is already queued before blocking, so a fresh connection
	// does not start with an empty round trip of waiting.
	ok, more := s.drainInstanceEvents(r.Context(), instanceID, &lastSequence, sentSequences, writeEvent)
	if !ok {
		return
	}
	// catchUp re-enters the drain without waiting for a wake-up. It carries the
	// case where a drain hit its per-pass cap with events still waiting, so a
	// client resuming far behind converges in one go instead of at one cap per
	// notification.
	catchUp := make(chan struct{}, 1)
	scheduleCatchUp := func(pending bool) {
		if !pending {
			return
		}
		select {
		case catchUp <- struct{}{}:
		default:
		}
	}
	scheduleCatchUp(more)
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
			continue
		case <-notifications:
		case <-catchUp:
		case <-fallback.C:
		}
		// Every wake-up means the same thing: look at the events again. Drop the
		// wake-ups this pass is about to satisfy first, so a batch of stored
		// events costs one query instead of one per event; anything that arrives
		// while the read below is in flight stays queued and causes another
		// pass, so collapsing here cannot swallow an event.
		collapseNotifications()
		ok, more := s.drainInstanceEvents(r.Context(), instanceID, &lastSequence, sentSequences, writeEvent)
		if !ok {
			return
		}
		scheduleCatchUp(more)
	}
}

// instanceEventCursor returns the newest stored sequence for an instance, used
// to start a cursorless stream at the live edge.
func (s *Server) instanceEventCursor(ctx context.Context, instanceID string) (int64, error) {
	var newest int64
	err := s.db.QueryRow(ctx, `select coalesce(max(agent_sequence),0) from cloud_events where instance_id=$1`, instanceID).Scan(&newest)
	return newest, err
}

// drainInstanceEvents delivers everything the stream has not sent yet.
//
// Two passes, because one query cannot do both jobs:
//
//   - Forward catch-up walks strictly newer sequences in full batches, so a
//     cursor that is far behind still reaches the live edge. The single query
//     this replaced scanned a retention-wide window ordered ascending, so once
//     an instance had more events than one batch, the result was filled entirely
//     with rows that had already been sent: the newest events were unreachable
//     and the stream stalled until the client reconnected. Any instance past
//     100 events was affected.
//   - Backfill rescans the last batch of sequences for an upload that arrived
//     out of order, which is bounded so it cannot crowd out the forward pass.
//
// It reports whether the stream should keep running, and whether a full batch
// was still waiting when the per-pass cap was reached — the caller uses that to
// come straight back instead of waiting for the next wake-up.
func (s *Server) drainInstanceEvents(ctx context.Context, instanceID string, lastSequence *int64, sentSequences map[int64]struct{}, writeEvent func(eventEnvelope)) (ok bool, more bool) {
	delivered := 0
	pending := false
	for {
		rows, err := s.db.Query(ctx, `select event_id,agent_sequence,type,task_id,task_run_id,payload,created_at from cloud_events where instance_id=$1 and agent_sequence>$2 order by agent_sequence limit $3`, instanceID, *lastSequence, cloudEventBatchSize)
		if err != nil {
			return false, false
		}
		batch := 0
		for rows.Next() {
			var event eventEnvelope
			var payload []byte
			if err := rows.Scan(&event.EventID, &event.AgentSequence, &event.Type, &event.TaskID, &event.TaskRunID, &payload, &event.CreatedAt); err != nil {
				rows.Close()
				return false, false
			}
			event.InstanceID = instanceID
			event.Payload = json.RawMessage(payload)
			writeEvent(event)
			sentSequences[event.AgentSequence] = struct{}{}
			*lastSequence = event.AgentSequence
			batch++
			delivered++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return false, false
		}
		if batch < cloudEventBatchSize {
			break
		}
		if delivered >= cloudEventCatchUpPerDrain {
			// A full batch went out and the cap was reached, so there is almost
			// certainly more behind it.
			pending = true
			break
		}
	}
	windowFloor := *lastSequence - cloudEventBackfillWindow
	for sequence := range sentSequences {
		if sequence <= windowFloor {
			delete(sentSequences, sequence)
		}
	}
	if windowFloor < 0 {
		windowFloor = 0
	}
	rows, err := s.db.Query(ctx, `select event_id,agent_sequence,type,task_id,task_run_id,payload,created_at from cloud_events where instance_id=$1 and agent_sequence>$2 and agent_sequence<=$3 order by agent_sequence limit $4`, instanceID, windowFloor, *lastSequence, cloudEventBatchSize)
	if err != nil {
		return false, false
	}
	defer rows.Close()
	for rows.Next() {
		var event eventEnvelope
		var payload []byte
		if err := rows.Scan(&event.EventID, &event.AgentSequence, &event.Type, &event.TaskID, &event.TaskRunID, &payload, &event.CreatedAt); err != nil {
			return false, false
		}
		if _, alreadySent := sentSequences[event.AgentSequence]; alreadySent {
			continue
		}
		event.InstanceID = instanceID
		event.Payload = json.RawMessage(payload)
		writeEvent(event)
		sentSequences[event.AgentSequence] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return false, false
	}
	return true, pending
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
		hashed := hashCode(provided)
		err := s.db.QueryRow(r.Context(), `select instance_id,expires_at from cloud_access_tokens where token_hash=$1 and revoked_at is null and activated_at is not null`, hashed).Scan(&instanceID, &expires)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && expires != nil && !expires.After(time.Now().UTC())) {
			writeError(w, http.StatusUnauthorized, errors.New("invalid user token"))
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		// 「这台手机刚刚还在用」。桌面端侧栏那张「已绑定手机」卡上的在线/最近同步全靠它 ——
		// 在此之前那张卡只能答"绑过谁"，答不了"现在还在不在用"，于是用户在手机上看到"已连接"、
		// 在电脑上看到一片沉默。
		//
		// 四件事必须一起成立，少一件这条读数就不可信：
		//   ① **节流**：手机端"当前设备"5 秒轮询一次 `/v1/instances`，逐次写等于每 5 秒在
		//      cloud_access_tokens 上留一个死元组。窗口取 30 秒 —— 宁可让"在线"晚 30 秒
		//      翻成"未同步"，也不要把这条观测量的写入放大 6 倍。
		//   ② **条件写在 SQL 的 where 里**，不在 Go 里比时间。这是唯一正确的做法：
		//      比较用的 `now()` 和赋值的 `now()` 来自同一个时钟，多副本 / 多进程部署下
		//      不会因为某台机器时钟偏了就开始每次都写。
		//   ③ **写失败绝不让请求失败**：鉴权已经过了，这只是一条观测量的落库。
		//      在这里回 500 等于"因为记不上一条活动时间就把用户挡在门外"。
		//   ④ 用 `now()` 而不是 Go 的 `time.Now()`（同上）。
		//
		// ⚠️ **这个 30 秒窗口和手机端 5 秒的轮询间隔，一起决定了桌面端「在线」的判定阈值**
		//      （见 `apps/web/src/features/remote/desktop-phone.ts`：两次写入的真实间隔是
		//      30+5=35 秒，阈值取 90 秒 ≈ 2.6 倍）。**改这个窗口必须回去重算那个阈值** ——
		//      窗口调大到 120 秒而阈值不动，手机明明在用也会被判成"未同步"。
		//      这条耦合跨了语言和仓库目录，没有编译器帮忙，只能靠这两处注释互相指认。
		//
		// ⚠️ 读侧（桌面浏览器）拿这个时刻和自己的 `Date.now()` 相减算年龄 —— 那是**两台机器**
		//      的时钟。桌面时钟若明显快于云端，年龄会被算大，一台在线的手机会被报成"未同步"
		//      （反过来偏慢会被算成负数，而负数按"新鲜"处理，是安全的那一侧）。
		//      两台机器正常都跟着 NTP 走，偏差远小于 45 秒的判定阈值，因此不做额外校正；
		//      真出现"手机明明在用却显示未同步"且 age 接近一个整分钟的偏移，先查桌面时钟。
		_, _ = s.db.Exec(r.Context(), `update cloud_access_tokens set last_used_at=now() where token_hash=$1 and (last_used_at is null or last_used_at < now() - interval '30 seconds')`, hashed)
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
	// conversation.shortcut 让手机端触发电脑端同一套快捷方式渲染/执行路径
	// （preview 取渲染后的正文用于填入手机输入框，run/confirm 直接执行）。
	// 白名单必须与 control-server 的 enqueueRemoteCommand 保持一致：两边任一处漏加，
	// 手机端拿到的就是一个语焉不详的 400。
	allowed := map[string]bool{"task.create": true, "task.update": true, "task.delete": true, "task.dispatch": true, "task.stop": true, "task.review": true, "task.reopen": true, "conversation.create": true, "conversation.message": true, "conversation.shortcut": true}
	if !allowed[input.Type] {
		writeError(w, 400, errors.New("unsupported command type"))
		return
	}
	if len(input.Payload) > 256<<10 || (len(input.Payload) > 0 && !json.Valid(input.Payload)) {
		writeError(w, http.StatusBadRequest, errors.New("payload must be valid JSON and smaller than 256 KiB"))
		return
	}
	// 命令 payload 同样落进 jsonb 列，同样要挡住 NUL 转义（见 event_payload.go）。
	// 这里不报 400 而是静默替换：这条路径上的 NUL 只可能来自电脑端自己拼进去的
	// 文本，替换掉比让整条命令 500 更符合用户预期。
	input.Payload = sanitizeJSONNUL(input.Payload)
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
	// 电脑端此刻没有中继连接、命令也早就过了宽限期：它不会被投递，也不会在重连后
	// 被补投（状态已经不是 queued）。老实现让它留在 queued 直到 5 分钟后过期，
	// 手机端只能一路轮询到 30 秒上限再报"仍在处理中" —— 为一个已知结果白等半分钟。
	// 手机端在轮询这个端点，所以在这里判定一定会被执行到，不需要额外的后台任务。
	if err := s.failStaleQueuedCommand(r.Context(), commandID, commandInstanceID); err != nil {
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
		CheckOrigin:     func(r *http.Request) bool { return s.agentOriginAllowed(r.Header.Get("Origin")) },
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(512 << 10)
	_ = conn.SetReadDeadline(time.Now().Add(45 * time.Second))
	session := &agentConnection{conn: conn}
	s.mu.Lock()
	if previous := s.connections[instanceID]; previous != nil {
		_ = previous.Close()
	}
	s.mu.Unlock()
	// 顶替掉旧连接之后，挂在旧连接上的中继请求再也不会有人回答（回话会走新连接，
	// 但它带的是旧 requestId，永远对不上）。在这里落成明确失败，比让手机端干等
	// 20 秒超时有用得多。
	//
	// **必须在新连接装上之前**做这件事：装好之后再清理会连带把刚注册、
	// 正等着这条新连接回答的请求一起误杀。中间那一小段里 connections 是空的，
	// 此时到达的请求会拿到 instance_offline —— 那是真话，不是误报。
	s.failRPCRequestsForInstance(instanceID)
	s.mu.Lock()
	s.connections[instanceID] = session
	s.mu.Unlock()
	defer func() {
		wasCurrent := false
		s.mu.Lock()
		if s.connections[instanceID] == session {
			delete(s.connections, instanceID)
			wasCurrent = true
		}
		s.mu.Unlock()
		if wasCurrent {
			_, _ = s.db.Exec(context.Background(), `update cloud_instances set status='offline',updated_at=now() where instance_id=$1 and status='online'`, instanceID)
			s.failRPCRequestsForInstance(instanceID)
		}
		_ = conn.Close()
	}()
	_, _ = s.db.Exec(r.Context(), `insert into cloud_instances(instance_id,status,last_seen_at) values($1,'online',now()) on conflict(instance_id) do update set status='online',last_seen_at=now(),updated_at=now()`, instanceID)
	s.sendPendingCommands(r.Context(), instanceID, session)
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
			s.agentWrite(instanceID, session, map[string]string{"kind": "pong"})
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
		if kind.Kind == "rpc.response" {
			// 中继请求的回答（文件与 Git 共用这条通道，见 rpc.go）。它在**读循环里同步投递**而不是起 goroutine：
			// 投递只是往一个缓冲为 1 的 channel 写一次、不阻塞，而读循环每帧都要尽快
			// 回到 ReadJSON —— 起 goroutine 反而会在事件洪峰时积压大量小任务。
			if !s.resolveAgentRPCResponse(raw) {
				// 认不出来的 requestId：超时的、客户端已经断开放弃的、或者对端重复回话的。
				// 三种都是正常的收敛路径，丢掉即可，绝不能因此断开连接。
				continue
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
				_ = s.agentWrite(instanceID, session, map[string]any{
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
		s.agentWrite(instanceID, session, map[string]any{"kind": "event.ack", "eventId": envelope.EventID, "agentSequence": envelope.AgentSequence})
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
	// 上游快照里可能带上带 NUL 转义的对话原文，而 snapshot 列是 jsonb —— 同样的
	// 拒绝（见 event_payload.go）。不洗掉的话整份快照都写不进去，手机端就会一直
	// 停在上一版快照上。
	snapshot = sanitizeJSONNUL(snapshot)
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

// permanentEventStoreError 把"重发一万次也不会成功"的数据库错误升级成永久冲突。
//
// 这是本系统最贵的一个坑：agentConnect 对非冲突错误一律按**临时故障**处理 ——
// 既不回 ack 也不回 reject，理由是"数据库恢复后 Agent 会重试"。这个假设对连接
// 类故障成立，对**数据本身存不进去**（SQLSTATE 22xxx 数据异常 / 23xxx 完整性约束）
// 完全不成立：同一份数据重试多少次都是同样的失败。
//
// 真机后果（2026-09-16）：若干条 payload 里带 NUL 转义的事件永远存不进 jsonb，
// 云端对它们永远沉默，本地 outbox 的行永远删不掉，队头被钉死，111 万条事件全堵在
// 后面，一条 WebSocket 连接 4.5 小时被灌了 792 MB 的重复确认。
//
// 兜底性质：payload 已经在 storeEvent 里洗过一遍 NUL，这条路径正常情况下不会走到。
// 留着它是为了让**任何**未来的同类数据异常都变成一次明确的 event.reject，
// 而不是又一次静默的永久重试。
func permanentEventStoreError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || len(pgErr.Code) < 2 {
		return nil
	}
	switch pgErr.Code[:2] {
	case "22", "23":
		// 22 = data exception，23 = integrity constraint violation。
		return fmt.Errorf("%w: %s", errEventInsertConflict, pgErr.Message)
	default:
		return nil
	}
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
	// NUL 转义在 jsonb 里存不下（见 event_payload.go）。不先洗掉的话这条事件会永远
	// 入库失败，而失败又被当成临时故障 —— 既没有 ack 也没有 reject，本地 outbox
	// 那行就永远删不掉。
	event.Payload = sanitizeJSONNUL(event.Payload)
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
		if permanent := permanentEventStoreError(err); permanent != nil {
			return permanent
		}
		return err
	}
	// Waking the mobile streams is done inside the transaction on purpose:
	// PostgreSQL delivers a NOTIFY only when the transaction commits, so a
	// rolled-back or conflicting event never wakes a stream for nothing.
	if _, err := tx.Exec(ctx, `select pg_notify($1, $2)`, eventNotifyChannel, instanceID); err != nil {
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

func (s *Server) sendPendingCommands(ctx context.Context, instanceID string, session *agentConnection) {
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
		if err := s.agentWrite(instanceID, session, map[string]any{"commandId": id, "idempotencyKey": idempotency, "type": typ, "projectId": projectID, "taskId": taskID, "payload": json.RawMessage(payload), "expiresAt": expires}); err != nil {
			return
		}
	}
}

func (s *Server) pushCommand(instanceID, commandID string) {
	s.mu.Lock()
	session := s.connections[instanceID]
	s.mu.Unlock()
	if session == nil {
		// 这台电脑此刻没有中继连接。这里**不**直接判失败：Agent 重连之后
		// sendPendingCommands 会把 queued 的命令补投出去，一次几秒的抖动不该被
		// 当成故障。真正的判定在 getCommand 里带一个宽限期做（手机端每 500ms
		// 轮询一次，所以那个判定一定会被执行到）。
		return
	}
	var idempotency, typ, projectID, taskID string
	var payload []byte
	var expires time.Time
	if err := s.db.QueryRow(context.Background(), `select idempotency_key,type,project_id,task_id,payload,expires_at from cloud_commands where command_id=$1`, commandID).Scan(&idempotency, &typ, &projectID, &taskID, &payload, &expires); err != nil {
		return
	}
	if err := s.agentWrite(instanceID, session, map[string]any{"commandId": commandID, "idempotencyKey": idempotency, "type": typ, "projectId": projectID, "taskId": taskID, "payload": json.RawMessage(payload), "expiresAt": expires}); err != nil {
		return
	}
}

// commandDeliveryGrace 是一条命令在"电脑端没有中继连接"状态下还能等的时长。
//
// 取 8 秒：Agent 断线后的重连退避是 1/2/4/8/16/30 秒，宽限期要盖住头几次重试，
// 否则一次几秒的网络抖动就会把命令判死；同时又必须远小于手机端 30 秒的等待上限，
// 让用户在超时之前就拿到明确原因。宽限期内 Agent 接单就照常投递，超过就落 failed，
// 并且因为状态已经不是 queued，重连后的 sendPendingCommands 也不会再补投它 ——
// 不会出现"手机说失败了、电脑端过一会儿又执行了"。
var commandDeliveryGrace = 8 * time.Second

// failUndeliverableCommand 把一条投不出去的命令直接落成终态。
//
// 只动 queued：命令已经被 Agent 接单（received/executing）时不能覆盖它的状态 ——
// 那会把一条正在执行的命令说成失败。
func (s *Server) failUndeliverableCommand(commandID, reason string) {
	_, _ = s.db.Exec(context.Background(), `update cloud_commands set status='failed',result=jsonb_build_object('error',$2::text),updated_at=now() where command_id=$1 and status='queued'`, commandID, reason)
}

// hasRelayConnection 报告这台电脑此刻有没有活着的中继连接。
//
// 判据只能是内存里的连接表：cloud_instances.status 会被快照上传重新写成 online，
// 即使 WS 早就不在了（真机 2026-09-17 07:35 之后就是这个状态，status=online、
// last_seen_at 停在那一刻，而 agent 进程一个 TCP 连接都没有）。
func (s *Server) hasRelayConnection(instanceID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connections[instanceID] != nil
}

// failStaleQueuedCommand 把"电脑端没有连接、且已经等过宽限期"的 queued 命令落成 failed。
//
// 只动 queued 且只动超过宽限期的：宽限期内 Agent 可能正好重连并接单。
func (s *Server) failStaleQueuedCommand(ctx context.Context, commandID, instanceID string) error {
	if s.hasRelayConnection(instanceID) {
		return nil
	}
	_, err := s.db.Exec(ctx, `update cloud_commands set status='failed',result=jsonb_build_object('error',$3::text),updated_at=now() where command_id=$1 and instance_id=$2 and status='queued' and created_at < now() - $4::interval`,
		commandID, instanceID, "电脑端当前不在线，请确认电脑上的 Milevia 正在运行并已连接",
		fmt.Sprintf("%d milliseconds", commandDeliveryGrace.Milliseconds()))
	return err
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

// agentWrite 把一帧控制数据写给某个实例。锁只覆盖"这条连接"（见 agentConnection），
// 不再是一把全局锁：一条连接写不动不能连累其他实例，也不能连累同一实例上正要
// 下发的命令。
func (s *Server) agentWrite(instanceID string, conn *agentConnection, value any) error {
	s.mu.Lock()
	current := s.connections[instanceID]
	s.mu.Unlock()
	if current != conn {
		return errors.New("agent connection is no longer current")
	}
	return conn.writeJSON(value)
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
