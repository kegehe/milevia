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
	"sync"
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
	// Shortcuts 是电脑端的「常用提示词 / 常用命令」库，手机端输入条的「＋」面板与桌面端
	// 左侧快捷栏展示同一份数据（用户明确要求"同源"）。
	//
	// 放在顶层而不是每个项目里：一份快捷方式可能被多个项目共用（shortcut_projects 是多对多），
	// 每个项目复制一遍既膨胀负载又会出现"同一份数据在快照里有两个版本"。作用域信息随
	// projectIds 一起下发，由手机端按当前项目过滤（规则与 listShortcuts 的 SQL 完全一致：
	// scope='local' 或绑定到该项目）。
	Shortcuts []remoteSnapshotShortcut `json:"shortcuts"`
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
	// Skills 是该项目在当前会话所用 CLI 下可用的技能（扫描本机 / 远端文件系统得到）。
	// 挂在项目上而不是顶层：技能是"项目 + CLI"两个维度的产物（项目级 SKILL.md 目录会覆盖
	// 用户级同名技能），脱离项目就没有意义。详见 skills.go 的 discoverSkillsForProject。
	Skills []Skill `json:"skills"`
}

// remoteSnapshotShortcut 是 Shortcut 的裁剪版：只带手机端渲染与执行需要的字段。
// 刻意不下发 createdAt/updatedAt —— 手机端一次都没用上，白白让每个快照都多几百字节。
type remoteSnapshotShortcut struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Description   string   `json:"description"`
	Kind          string   `json:"kind"`
	Template      string   `json:"template"`
	Scope         string   `json:"scope"`
	DefaultAction string   `json:"defaultAction"`
	GroupName     string   `json:"groupName,omitempty"`
	Pinned        bool     `json:"pinned"`
	Enabled       bool     `json:"enabled"`
	SortOrder     int      `json:"sortOrder"`
	ProjectIDs    []string `json:"projectIds"`
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
	// Notices 是最近的状态/诊断事件原文（API 重试、上下文压缩、执行失败等）。
	// 实时的 outbox 事件只在运行中送达，手机重进页面或中途刷新时看不到它们，
	// 所以快照要一并回放，否则"执行失败"这类关键信息一刷新就消失了。
	Notices []remoteSnapshotNotice `json:"notices"`
}

type remoteSnapshotMessage struct {
	ID        string    `json:"id"`
	RunID     string    `json:"runId,omitempty"`
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"createdAt"`
}

// remoteSnapshotNotice carries one status/diagnostic event verbatim: the phone
// parses it with the same rules the desktop timeline uses, so the two surfaces
// cannot drift into describing the same event differently.
type remoteSnapshotNotice struct {
	ID        string          `json:"id"`
	RunID     string          `json:"runId,omitempty"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"createdAt"`
}

// Mobile receives durable message events in real time. The snapshot is only a
// bounded recovery/bootstrap view, so do not rebuild every historical
// conversation on each sync. Older desktop history remains in SQLite and is
// deliberately outside the mobile relay's hot path.
const (
	remoteSnapshotConversationsPerProject = 1
	remoteSnapshotMessagesPerConversation = 20
	remoteSnapshotMessageContentLimit     = 2000
	remoteSnapshotNoticesPerConversation  = 24
	remoteSnapshotNoticePayloadLimit      = 4096
	// 快捷方式库的配额。库是用户手工维护的，正常几十条；给上限是为了防"导入了上千条提示词"
	// 把每个快照都撑到几百 KB（快照每次有事件推进就要重传一次，3 秒节流）。
	remoteSnapshotShortcuts             = 200
	remoteSnapshotShortcutTemplateLimit = 2000
	// 技能扫描（本地目录 / 远端 SSH find+cat）比一次 SQL 贵得多，而技能只在安装/卸载时才变。
	// 快照路径单独走一层 60 秒缓存，桌面端的 listSkills 仍然是实时扫描、不受影响。
	remoteSnapshotSkillsTTL = 60 * time.Second
)

// remoteNoticeTypesSQL 是快照回放要带的"运行状态/诊断"事件类型（SQL 片段，值全部是
// 本文件的字面常量，不含任何用户输入）。approval.% 用 LIKE 覆盖 pending/allow/deny/
// aborted/timeout："卡在等工具确认"是重新打开会话时最需要看到的状态，漏掉它用户只会
// 以为程序死了。stderr 刻意不在其中——它逐行产生，几十行会把配额挤爆，而且真正的原因
// 通常已经在 run.failed / error 的 detail 里；实时通道仍会把它聚合成一条"CLI 输出"。
const remoteNoticeTypesSQL = `type in ('system','run.failed','run.interrupted','error','turn.failed','stream.error') or type like 'approval.%'`

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
	// conversation.shortcut 让手机端触发电脑端**同一条**快捷方式执行路径
	// （/api/conversations/{id}/shortcuts/{id}/preview|run）：模板里的 ${project.path}
	// 这类变量只有电脑端渲染得出来，命令类快捷方式的 shell 包装也只在服务端有一份实现，
	// 手机端本地拼一遍就会与桌面端分叉。详见 executeRemoteCommand。
	allowed := map[string]bool{"task.create": true, "task.update": true, "task.delete": true, "task.dispatch": true, "task.stop": true, "task.review": true, "task.reopen": true, "conversation.create": true, "conversation.message": true, "conversation.shortcut": true}
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

func (s *Server) enqueueRemoteEventTx(ctx context.Context, tx *sql.Tx, eventID, taskID, taskRunID, typ string, payload []byte, now time.Time) error {
	return s.enqueueRemoteEventTxWithConversation(ctx, tx, eventID, taskID, taskRunID, "", typ, payload, now)
}

// enqueueRemoteEventTxWithConversation is the conversation-aware form of
// enqueueRemoteEventTx. The outbox row has no conversation column and the CLI
// payloads carry none either, so without stamping the identifier here a relayed
// status event (API retry, context compaction) or execution failure arrives on
// the phone with nothing to attribute it to.
func (s *Server) enqueueRemoteEventTxWithConversation(ctx context.Context, tx *sql.Tx, eventID, taskID, taskRunID, conversationID, typ string, payload []byte, now time.Time) error {
	payload = compactRemoteEventPayload(typ, remoteEventPayloadWithConversation(payload, conversationID))
	if _, err := tx.ExecContext(ctx, `update remote_instance set last_agent_sequence=last_agent_sequence+1,updated_at=?`, now); err != nil {
		return err
	}
	var sequence int64
	if err := tx.QueryRowContext(ctx, `select last_agent_sequence from remote_instance limit 1`).Scan(&sequence); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `insert into remote_outbox(event_id,agent_sequence,type,task_id,task_run_id,payload,created_at) values(?,?,?,?,?,?,?)`, eventID, sequence, typ, taskID, taskRunID, string(payload), now); err != nil {
		return err
	}
	// Wake any held long-poll request so the Agent forwards this event now
	// rather than on its next tick. This fires from inside the caller's
	// transaction: waking a moment early only costs a re-read (see
	// remoteOutboxWakeGrace), while waking late would cost a poll interval on
	// every single event. Callers that own their transaction also wake once
	// more after commit, which makes the common path exact.
	s.wakeRemoteOutbox()
	return nil
}

// remoteEventPayloadWithConversation stamps the conversation identifier into the
// relayed copy of an event payload.
//
// The desktop transcript keeps the original payload; only the outbox copy is
// decorated. Mobile needs it because status/diagnostic events (API retry,
// context compaction, execution failure) are produced by the CLI and therefore
// carry no conversation field of their own, while the outbox envelope has no
// conversation column either — without this stamp the phone can only guess which
// conversation a "正在压缩上下文" or "执行失败" notice belongs to.
func remoteEventPayloadWithConversation(payload []byte, conversationID string) []byte {
	if conversationID == "" || len(payload) == 0 {
		return payload
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(payload, &object) != nil || object == nil {
		return payload
	}
	if _, exists := object["conversationId"]; exists {
		return payload
	}
	encoded, err := json.Marshal(conversationID)
	if err != nil {
		return payload
	}
	object["conversationId"] = encoded
	merged, err := json.Marshal(object)
	if err != nil {
		return payload
	}
	return merged
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
	return s.enqueueRemoteEventWithConversation(ctx, eventID, taskID, taskRunID, "", typ, payload, now)
}

// enqueueRemoteEventWithConversation is enqueueRemoteEvent plus the conversation
// stamp described on remoteEventPayloadWithConversation.
func (s *Server) enqueueRemoteEventWithConversation(ctx context.Context, eventID, taskID, taskRunID, conversationID, typ string, payload []byte, now time.Time) error {
	if !s.remoteRelayConfigured() {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.enqueueRemoteEventTxWithConversation(ctx, tx, eventID, taskID, taskRunID, conversationID, typ, payload, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// Wake again after the commit so a held poll sees the row on its first
	// read instead of paying the grace re-read.
	s.wakeRemoteOutbox()
	return nil
}

// enqueueRemoteDelta relays an incremental assistant chunk to the mobile relay
// without persisting it or broadcasting it to local subscribers.
//
// Deltas are phone-facing only. The desktop transcript is built from the raw CLI
// envelopes, while every event that goes through appendEvent is stored in the
// conversation's event history and reloaded with it — so persisting one row per
// chunk would bloat that history, and every client's event timeline, for a
// payload no local view renders.
func (s *Server) enqueueRemoteDelta(runID string, payload []byte) {
	if !s.remoteRelayConfigured() {
		return
	}
	if err := s.enqueueRemoteEvent(context.Background(), uuid.NewString(), "", runID, "assistant.delta", payload, time.Now().UTC()); err != nil {
		log.Printf("queue remote assistant.delta event: %v", err)
	}
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

// remoteConversationNotices replays the recent status/diagnostic events of one
// conversation in ascending order, so the phone timeline can interleave them
// with messages using a single rule.
func (s *Server) remoteConversationNotices(ctx context.Context, conversationID string) ([]remoteSnapshotNotice, error) {
	query := fmt.Sprintf(`select id,run_id,type,payload,created_at from events where conversation_id=? and (%s) order by created_at desc,id desc limit ?`, remoteNoticeTypesSQL)
	rows, err := s.db.QueryContext(ctx, query, conversationID, remoteSnapshotNoticesPerConversation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]remoteSnapshotNotice, 0, remoteSnapshotNoticesPerConversation)
	for rows.Next() {
		var item remoteSnapshotNotice
		var runID sql.NullString
		var payload string
		if err := rows.Scan(&item.ID, &runID, &item.Type, &payload, &item.CreatedAt); err != nil {
			return nil, err
		}
		item.RunID = runID.String
		// Payloads that exceed the relay budget degrade to an empty object: a
		// truncated JSON document would be worse than no detail at all, and the
		// notice itself still tells the phone what happened.
		if len(payload) > remoteSnapshotNoticePayloadLimit {
			payload = "{}"
		}
		item.Payload = json.RawMessage(payload)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for left, right := 0, len(items)-1; left < right; left, right = left+1, right-1 {
		items[left], items[right] = items[right], items[left]
	}
	return items, nil
}

// remoteSnapshotShortcuts 读取手机端要用的快捷方式库。
//
// 与 listShortcuts 共用同一张表，但**不按项目过滤**：一次把 (local + 所有项目绑定) 全取回来，
// 由手机端按 projectIds 自行过滤。这样做的好处是负载只有一份、且不会出现"同一份数据在快照里
// 有两个版本"；代价是手机端多几行过滤代码，而那几行正好也能被单测固定住。
//
// 排序必须与 listShortcuts **逐字一致**（`order by s.sort_order,s.name`），否则同一条库在两端的
// 顺序会不同。曾经这里多写了一个 `s.pinned desc`，理由是"与桌面端 sortQueueTasks 的置顶语义对齐"
// —— 那是任务队列的函数，与快捷方式无关；桌面端并不提升 pinned。实际影响很小（编辑器新建时恒写
// pinned=1、种子也全是 1，所以 pinned desc 目前是个空操作），但它是一处任人误信的假注释，删掉。
func (s *Server) remoteSnapshotShortcuts(ctx context.Context) ([]remoteSnapshotShortcut, error) {
	rows, err := s.db.QueryContext(ctx, `
		select s.id,s.name,s.description,s.kind,s.template,s.scope,s.default_action,s.group_name,s.pinned,s.enabled,s.sort_order,s.created_at,s.updated_at
		from shortcuts s
		order by s.sort_order, s.name
		limit ?`, remoteSnapshotShortcuts)
	if err != nil {
		return nil, err
	}
	items := make([]remoteSnapshotShortcut, 0)
	for rows.Next() {
		shortcut, err := scanShortcut(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		items = append(items, remoteSnapshotShortcut{
			ID:            shortcut.ID,
			Name:          shortcut.Name,
			Description:   shortcut.Description,
			Kind:          shortcut.Kind,
			Template:      truncateUTF8(shortcut.Template, remoteSnapshotShortcutTemplateLimit),
			Scope:         shortcut.Scope,
			DefaultAction: shortcut.DefaultAction,
			GroupName:     shortcut.GroupName,
			Pinned:        shortcut.Pinned,
			Enabled:       shortcut.Enabled,
			SortOrder:     shortcut.SortOrder,
			ProjectIDs:    []string{},
		})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return items, nil
	}
	// 绑定关系一次查完再回填：逐条调 shortcutProjectIDs 会变成 N+1 次查询，而这个函数
	// 每 3 秒就可能被跑一次。
	bindings, err := s.db.QueryContext(ctx, `select shortcut_id, project_id from shortcut_projects`)
	if err != nil {
		return nil, err
	}
	byShortcut := make(map[string][]string, len(items))
	for bindings.Next() {
		var shortcutID, projectID string
		if err := bindings.Scan(&shortcutID, &projectID); err != nil {
			bindings.Close()
			return nil, err
		}
		byShortcut[shortcutID] = append(byShortcut[shortcutID], projectID)
	}
	if err := bindings.Err(); err != nil {
		bindings.Close()
		return nil, err
	}
	bindings.Close()
	for index := range items {
		if projectIDs, ok := byShortcut[items[index].ID]; ok {
			items[index].ProjectIDs = projectIDs
		}
	}
	return items, nil
}

// snapshotSkillsEntry 是快照路径的技能扫描缓存项。
type snapshotSkillsEntry struct {
	skills []Skill
	at     time.Time
}

// remoteSnapshotSkills 取某个项目的技能列表（按该项目当前会话的 CLI 过滤），带 60 秒缓存。
// 扫描本身复用 discoverSkillsForProject —— 绝不在快照里另写一套扫描逻辑：项目级 SKILL.md
// 覆盖用户级同名技能这类规则只应有一处实现，否则手机端和桌面端会列出不同的技能。
func (s *Server) remoteSnapshotSkills(ctx context.Context, project Project, agentID string) []Skill {
	key := project.ID + "\x00" + project.Runner + "\x00" + agentID
	s.snapshotSkillsMu.Lock()
	if entry, ok := s.snapshotSkillsCache[key]; ok && time.Since(entry.at) < remoteSnapshotSkillsTTL {
		s.snapshotSkillsMu.Unlock()
		return entry.skills
	}
	s.snapshotSkillsMu.Unlock()
	// 扫描放在锁外：SSH 远端的 find+cat 可能耗时几百毫秒，持锁会让整个快照组装串行化。
	// 并发重复扫描同一 key 是可接受的（结果一致、只是多花一次 IO），换来的是互不阻塞。
	skills := s.discoverSkillsForProject(ctx, project, agentID)
	if skills == nil {
		skills = []Skill{}
	}
	s.snapshotSkillsMu.Lock()
	if s.snapshotSkillsCache == nil {
		s.snapshotSkillsCache = make(map[string]snapshotSkillsEntry)
	}
	s.snapshotSkillsCache[key] = snapshotSkillsEntry{skills: skills, at: time.Now()}
	s.snapshotSkillsMu.Unlock()
	return skills
}

func (s *Server) remoteSnapshot(w http.ResponseWriter, r *http.Request) {
	var revision int64
	if err := s.db.QueryRowContext(r.Context(), `select last_agent_sequence from remote_instance limit 1`).Scan(&revision); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	snapshot := remoteSnapshot{SnapshotRevision: revision, ObservedAt: time.Now().UTC(), Projects: make([]remoteSnapshotProject, 0), Shortcuts: make([]remoteSnapshotShortcut, 0)}
	// 快捷方式库与项目/任务无关，先取一次填到顶层。取失败不让整个快照失败：
	// 手机端拿到一份"没有快捷方式"的快照，仍能看会话、发消息，比整页空白好得多。
	if shortcuts, err := s.remoteSnapshotShortcuts(r.Context()); err == nil {
		snapshot.Shortcuts = shortcuts
	} else {
		log.Printf("remote snapshot: load shortcuts: %v", err)
	}
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
		project.Conversations = make([]remoteSnapshotConversation, 0)		// The active conversation is enough to make a project immediately usable
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
			notices, err := s.remoteConversationNotices(r.Context(), conversation.ID)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			conversation.Notices = notices
		}
		// 技能按"项目 + CLI"两个维度扫描，CLI 取该项目当前会话的 agentId（每个项目在快照里
		// 只带 1 个会话，所以这里就是手机端打开该项目时会看到的那一个）。
		//
		// 没有会话的项目直接跳过扫描：手机端打不开这种项目（点进去是"选 Agent 建会话"），
		// 而扫描本地技能目录会顺带触发 WSL 探测、远端 runner 更是要走一次 SSH find+cat ——
		// 每个项目每 60 秒白跑一次不值得。
		project.Skills = []Skill{}
		if len(project.Conversations) > 0 {
			if stored, err := s.getProjectByID(r.Context(), project.ID); err == nil {
				project.Skills = s.remoteSnapshotSkills(r.Context(), stored, project.Conversations[0].AgentID)
			} else {
				log.Printf("remote snapshot: load project %s for skills: %v", project.ID, err)
			}
		}
		snapshot.Projects = append(snapshot.Projects, *project)
	}
	writeJSON(w, http.StatusOK, snapshot)
}

const (
	// maxRemoteOutboxWaitSeconds bounds how long one long-poll may be held. It
	// stays well under the Agent's HTTP client timeout so a held request is
	// never the thing that trips it.
	maxRemoteOutboxWaitSeconds = 25
	// remoteOutboxWakeGrace covers the window between a writer inserting into
	// remote_outbox inside its transaction and that transaction committing. A
	// wake-up is sent from inside the transaction, so the first read after a
	// wake-up can legitimately see nothing yet; re-reading once after this
	// pause makes the fast path deterministic without polling in the steady
	// state.
	remoteOutboxWakeGrace = 15 * time.Millisecond
)

// remoteOutboxWaker broadcasts "the remote outbox changed" to every held
// long-poll request.
//
// A buffered channel (the shape used for remoteCommandWake, where exactly one
// worker consumes the signal) would be wrong here: it can only wake one waiter,
// and a signal sent while nobody is selecting is consumed by the next waiter
// even though it refers to an already-drained row. Closing and replacing the
// channel wakes all current waiters, and a waiter that captures the channel
// before its read can never miss a wake-up that lands mid-flight.
type remoteOutboxWaker struct {
	mu   sync.Mutex
	next chan struct{}
}

func newRemoteOutboxWaker() *remoteOutboxWaker {
	return &remoteOutboxWaker{next: make(chan struct{})}
}

func (w *remoteOutboxWaker) wait() <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.next
}

func (w *remoteOutboxWaker) wake() {
	w.mu.Lock()
	defer w.mu.Unlock()
	close(w.next)
	w.next = make(chan struct{})
}

// outboxWaker lazily creates the waker so tests that build a Server literal
// still get a working long-poll instead of a nil dereference.
func (s *Server) outboxWaker() *remoteOutboxWaker {
	s.remoteOutboxWakeMu.Lock()
	defer s.remoteOutboxWakeMu.Unlock()
	if s.remoteOutboxWake == nil {
		s.remoteOutboxWake = newRemoteOutboxWaker()
	}
	return s.remoteOutboxWake
}

func (s *Server) wakeRemoteOutbox() {
	s.remoteOutboxWakeMu.Lock()
	waker := s.remoteOutboxWake
	s.remoteOutboxWakeMu.Unlock()
	if waker != nil {
		waker.wake()
	}
}

func (s *Server) readRemoteOutbox(ctx context.Context, limit int) ([]remoteOutboxItem, error) {
	// Events that exhausted their delivery budget stay in the table as a
	// durable record but leave the delivery window. Without this bound a
	// single undeliverable event (for example one the cloud permanently
	// rejects) would sit at the head of this ordered batch forever and starve
	// every event behind it.
	rows, err := s.db.QueryContext(ctx, `select event_id,agent_sequence,type,task_id,task_run_id,payload,created_at from remote_outbox where (next_attempt_at is null or next_attempt_at<=?) and attempts < ? order by agent_sequence limit ?`, time.Now().UTC(), maxRemoteOutboxAttempts, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]remoteOutboxItem, 0)
	for rows.Next() {
		var item remoteOutboxItem
		var payload string
		if err := rows.Scan(&item.EventID, &item.AgentSequence, &item.Type, &item.TaskID, &item.TaskRunID, &payload, &item.CreatedAt); err != nil {
			return nil, err
		}
		item.Payload = json.RawMessage(payload)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

func (s *Server) remoteOutbox(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 && parsed <= 500 {
			limit = parsed
		}
	}
	// wait seconds lets the Agent hold the request open instead of asking every
	// 200ms. Absent or invalid, this stays a plain read so older Agents are
	// unaffected.
	wait := time.Duration(0)
	if raw := r.URL.Query().Get("wait"); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
			if seconds > maxRemoteOutboxWaitSeconds {
				seconds = maxRemoteOutboxWaitSeconds
			}
			wait = time.Duration(seconds) * time.Second
		}
	}
	if wait <= 0 {
		items, err := s.readRemoteOutbox(r.Context(), limit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, items)
		return
	}

	deadline := time.Now().Add(wait)
	gracePending := false
	for {
		// Capture the wake channel before reading. A wake-up sent while this
		// read is in flight closes the channel we already hold, so the select
		// below returns immediately instead of waiting out the whole window.
		pending := s.outboxWaker().wait()
		items, err := s.readRemoteOutbox(r.Context(), limit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if len(items) > 0 || !time.Now().Before(deadline) {
			writeJSON(w, http.StatusOK, items)
			return
		}
		if gracePending {
			// Woken but still empty: the writer had not committed yet. Pause
			// once so the next read observes the committed row.
			gracePending = false
			if !sleepWithContext(r.Context(), remoteOutboxWakeGrace) {
				return
			}
			continue
		}
		timer := time.NewTimer(time.Until(deadline))
		select {
		case <-r.Context().Done():
			timer.Stop()
			return
		case <-pending:
			gracePending = true
		case <-timer.C:
		}
		timer.Stop()
	}
}

// sleepWithContext reports whether the full duration elapsed before the request
// was cancelled.
func sleepWithContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
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
		// last_error 会在手机端诊断里显示，按字节限额截断时要退到完整字符边界。
		input.Error = truncateUTF8(input.Error, 2000)
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
	if command.Type == "conversation.shortcut" {
		var input struct {
			ConversationID string            `json:"conversationId"`
			ShortcutID     string            `json:"shortcutId"`
			Action         string            `json:"action"`
			Variables      map[string]string `json:"variables,omitempty"`
		}
		if err := json.Unmarshal(command.Payload, &input); err != nil {
			return nil, errors.New("conversation shortcut payload is invalid")
		}
		input.ConversationID = strings.TrimSpace(input.ConversationID)
		input.ShortcutID = strings.TrimSpace(input.ShortcutID)
		input.Action = strings.TrimSpace(input.Action)
		if input.ConversationID == "" || input.ShortcutID == "" {
			return nil, errors.New("conversationId and shortcutId are required")
		}
		// preview 与 run 走各自原本的 handler：渲染规则（含 ${project.path} 这类电脑端才有的
		// 变量）、命令类快捷方式的 shell 包装、confirm 的前置校验、shortcut_runs 审计记录，
		// 全部复用同一份实现，手机端不可能和桌面端跑出不同结果。
		//   fill  → 只渲染，内容回给手机端填进它自己的输入框（桌面端的 fill 是填桌面输入框，
		//           在手机上那个语义无意义，所以这里只取渲染结果）。
		//   run / confirm → 真的执行，与桌面端点击完全一致。
		if input.Action == "fill" {
			return s.executeRemoteHTTPCommand(ctx, http.MethodPost,
				"/api/conversations/"+input.ConversationID+"/shortcuts/"+input.ShortcutID+"/preview",
				"", "", input.ConversationID, s.previewShortcut,
				mustJSON(map[string]any{"variables": input.Variables}),
				"shortcutID", input.ShortcutID)
		}
		if input.Action != "run" && input.Action != "confirm" {
			return nil, errors.New("shortcut action must be fill, run or confirm")
		}
		return s.executeRemoteHTTPCommand(ctx, http.MethodPost,
			"/api/conversations/"+input.ConversationID+"/shortcuts/"+input.ShortcutID+"/run",
			"", "", input.ConversationID, s.runShortcut,
			mustJSON(map[string]any{"variables": input.Variables, "action": input.Action}),
			"shortcutID", input.ShortcutID)
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

func (s *Server) executeRemoteHTTPCommand(ctx context.Context, method, path, projectID, taskID, conversationID string, handler http.HandlerFunc, payload json.RawMessage, extraParams ...string) (any, error) {
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
	// extraParams 是 "名,值,名,值" 形式的补充路由参数。handler 用 chi.URLParam 取参数，
	// 而这里合成的请求没有真的走路由匹配 —— 少放一个参数，handler 里读到的就是空字符串。
	// 用可变参数而不是再加形参：现有十几个调用点全都不用动。
	for index := 0; index+1 < len(extraParams); index += 2 {
		if extraParams[index] == "" {
			continue
		}
		rctx.URLParams.Add(extraParams[index], extraParams[index+1])
	}
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	response := httptest.NewRecorder()
	handler(response, req)
	if response.Code >= 400 {
		// 这条文案会原样出现在手机端（「快捷方式执行失败：…」），所以：
		//   · 不能写死 "task" —— 同一个 helper 也服务 conversation.* 命令；
		//   · 能取到 {"error": "..."} 就只取那一句，别把整个 JSON 与 HTTP 码糊给用户看。
		detail := strings.TrimSpace(response.Body.String())
		var failure struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &failure); err == nil && failure.Error != "" {
			detail = failure.Error
		}
		return nil, fmt.Errorf("remote command failed (%d): %s", response.Code, detail)
	}
	var value any
	if response.Body.Len() > 0 {
		_ = json.Unmarshal(response.Body.Bytes(), &value)
	}
	return value, nil
}
