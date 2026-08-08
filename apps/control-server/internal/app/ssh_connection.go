package app

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"
)

// SSHConnection represents an SSH connection configuration stored in the database.
type SSHConnection struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	Host           string     `json:"host"`
	Port           int        `json:"port"`
	User           string     `json:"user"`
	PrivateKeyPath string     `json:"-"`
	Password       string     `json:"-"`
	KnownHosts     string     `json:"-"`
	RootPath       string     `json:"rootPath"`
	Status         string     `json:"status"` // unknown / connected / disconnected / error
	LastSeen       *time.Time `json:"lastSeen"`
	ErrorMsg       string     `json:"errorMsg"`
	CreatedAt      time.Time  `json:"createdAt"`
	UpdatedAt      time.Time  `json:"updatedAt"`
}

// sanitizedSSHConnection returns a copy safe to expose via the API. The private
// key path (a filesystem location, not key material) is exposed so edits can
// re-preflight without re-entering it; the password and known_hosts remain
// redacted.
func sanitizedSSHConnection(c SSHConnection) map[string]any {
	authMethod := "key"
	if c.Password != "" {
		authMethod = "password"
	}
	return map[string]any{
		"id":             c.ID,
		"name":           c.Name,
		"host":           c.Host,
		"port":           c.Port,
		"user":           c.User,
		"authMethod":     authMethod,
		"privateKeyPath": c.PrivateKeyPath,
		"rootPath":       c.RootPath,
		"status":         c.Status,
		"lastSeen":       c.LastSeen,
		"errorMsg":       c.ErrorMsg,
		"createdAt":      c.CreatedAt,
	}
}

// registerSSHRoutes adds SSH connection management endpoints to the router.
func (s *Server) registerSSHRoutes(r chi.Router) {
	r.Get("/api/ssh-profiles", s.listSSHProfiles)
	r.Get("/api/ssh-profiles/{profileName}", s.getSSHProfile)
	r.Post("/api/ssh-connections/preflight", s.preflightSSHConnection)
	r.Get("/api/ssh-connections", s.listSSHConnections)
	r.Post("/api/ssh-connections", s.createSSHConnection)
	r.Get("/api/ssh-connections/{connectionID}", s.getSSHConnection)
	r.Put("/api/ssh-connections/{connectionID}", s.updateSSHConnection)
	r.Delete("/api/ssh-connections/{connectionID}", s.deleteSSHConnection)
	r.Post("/api/ssh-connections/{connectionID}/test", s.testSSHConnection)
	r.Post("/api/ssh-connections/{connectionID}/connect", s.connectSSHConnection)
	r.Post("/api/ssh-connections/{connectionID}/disconnect", s.disconnectSSHConnection)
}

// migrateSSHConnections creates the ssh_connections table.
func (s *Server) migrateSSHConnections(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `create table if not exists ssh_connections (
		id              text primary key,
		name            text not null,
		host            text not null,
		port            integer not null default 22,
		user            text not null,
		private_key_path text not null,
		known_hosts     text not null default '',
		root_path       text not null default '/',
		status          text not null default 'unknown',
		last_seen       datetime,
		error_msg       text not null default '',
		created_at      datetime not null,
		updated_at      datetime not null,
		password        text not null default ''
	)`)
	if err != nil {
		return fmt.Errorf("migrate ssh_connections: %w", err)
	}
	// Add password column for existing databases that don't have it yet.
	_, _ = s.db.ExecContext(ctx, `alter table ssh_connections add column password text not null default ''`)
	return nil
}

func listSSHProfilesFromFile(configPath string) ([]string, error) {
	seen := map[string]bool{}
	if err := collectSSHProfiles(configPath, seen, map[string]bool{}); err != nil {
		return nil, err
	}
	profiles := make([]string, 0, len(seen))
	for name := range seen {
		profiles = append(profiles, name)
	}
	sort.Strings(profiles)
	return profiles, nil
}

func collectSSHProfiles(configPath string, seen, visited map[string]bool) error {
	if visited[configPath] {
		return nil
	}
	visited[configPath] = true
	file, err := os.Open(configPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(strings.SplitN(scanner.Text(), "#", 2)[0])
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if strings.EqualFold(fields[0], "Include") {
			for _, pattern := range fields[1:] {
				if strings.HasPrefix(pattern, "~/") {
					if home, err := os.UserHomeDir(); err == nil {
						pattern = filepath.Join(home, strings.TrimPrefix(pattern, "~/"))
					}
				}
				if !filepath.IsAbs(pattern) {
					pattern = filepath.Join(filepath.Dir(configPath), pattern)
				}
				matches, _ := filepath.Glob(pattern)
				for _, match := range matches {
					if err := collectSSHProfiles(match, seen, visited); err != nil {
						return err
					}
				}
			}
			continue
		}
		if !strings.EqualFold(fields[0], "Host") {
			continue
		}
		for _, name := range fields[1:] {
			if !strings.ContainsAny(name, "*!?") {
				seen[name] = true
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return scanner.Err()
}

type sshProfile struct {
	Host           string `json:"host"`
	Port           int    `json:"port"`
	User           string `json:"user"`
	PrivateKeyPath string `json:"privateKeyPath"`
}

var getSSHConfigOutput = func(name string) ([]byte, error) {
	return exec.Command("ssh", "-G", name).Output()
}

func resolveSSHProfile(name string) (sshProfile, error) {
	if name == "" || strings.ContainsAny(name, " \t\n\r;|&$`\\\"") {
		return sshProfile{}, errors.New("SSH Profile 名称无效")
	}
	output, err := getSSHConfigOutput(name)
	if err != nil {
		return sshProfile{}, fmt.Errorf("读取 SSH Profile %q 失败：%w", name, err)
	}
	profile := sshProfile{Port: 22}
	identityFiles := []string{}
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		value := strings.Join(fields[1:], " ")
		switch strings.ToLower(fields[0]) {
		case "hostname":
			profile.Host = value
		case "user":
			profile.User = value
		case "port":
			if port, err := strconv.Atoi(value); err == nil {
				profile.Port = port
			}
		case "identityfile":
			identityFiles = append(identityFiles, value)
		case "proxyjump":
			if value != "none" {
				return sshProfile{}, errors.New("该 SSH Profile 使用了 ProxyJump；当前直连模式暂不支持跳板机")
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return sshProfile{}, fmt.Errorf("读取 SSH Profile %q 失败：%w", name, err)
	}
	for _, keyPath := range identityFiles {
		if strings.HasPrefix(keyPath, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				keyPath = filepath.Join(home, strings.TrimPrefix(keyPath, "~/"))
			}
		}
		if info, err := os.Stat(keyPath); err == nil && !info.IsDir() {
			profile.PrivateKeyPath = keyPath
			break
		}
	}
	if profile.Host == "" || profile.User == "" {
		return sshProfile{}, fmt.Errorf("SSH Profile %q 未解析出可用的主机或用户名", name)
	}
	return profile, nil
}

func (s *Server) listSSHProfiles(w http.ResponseWriter, r *http.Request) {
	home, err := os.UserHomeDir()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	profiles, err := listSSHProfilesFromFile(filepath.Join(home, ".ssh", "config"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("读取 SSH 配置失败：%w", err))
		return
	}
	writeJSON(w, http.StatusOK, profiles)
}

func (s *Server) getSSHProfile(w http.ResponseWriter, r *http.Request) {
	profile, err := resolveSSHProfile(chi.URLParam(r, "profileName"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, profile)
}

// preflightSSHConnection verifies the selected profile or manual settings
// before they are persisted. The returned host key must be confirmed on save.
// When connectionId is supplied (edit mode), stored secrets are reused so the
// preflight can actually connect without re-entering the password or key path.
func (s *Server) preflightSSHConnection(w http.ResponseWriter, r *http.Request) {
	var input sshConnectionInput
	if !decode(w, r, &input) {
		return
	}
	if strings.TrimSpace(input.ConnectionID) != "" {
		input.Update = true
		existing, err := s.loadSSHConnection(r.Context(), strings.TrimSpace(input.ConnectionID))
		if err == nil {
			// 凭据不回显到前端，编辑预检时沿用已保存的密码/私钥路径，使预检能真正连上远端。
			// 仅补全当前认证方式对应的凭据，切换认证方式时不携带旧凭据，避免 newSSHClient
			// 因残留密码而误用密码认证。
			if input.AuthMethod == "password" {
				if strings.TrimSpace(input.Password) == "" {
					input.Password = existing.Password
				}
				input.PrivateKeyPath = ""
			} else {
				input.Password = ""
				if strings.TrimSpace(input.PrivateKeyPath) == "" {
					input.PrivateKeyPath = existing.PrivateKeyPath
				}
			}
		}
	}
	if err := input.resolve(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	connection := input.connection()
	client, err := newSSHClient(connection, true)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	defer client.close()
	checks := map[string]bool{"ssh": false, "sftp": false, "claude": false, "approvalTunnel": false}
	if _, err := client.execCommand(r.Context(), "echo ok"); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "checks": checks, "error": errorText(err)})
		return
	}
	checks["ssh"] = true
	if _, err := client.readDir(r.Context(), connection.RootPath); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "checks": checks, "hostKey": client.lastHostKey, "fingerprint": hostKeyFingerprint(client.lastHostKey), "error": "无法读取 SSH 根目录，请检查目录路径和访问权限后重试。"})
		return
	}
	checks["sftp"] = true
	if output, err := client.execCommand(r.Context(), "command -v claude && claude --version"); err == nil && strings.TrimSpace(string(output)) != "" {
		checks["claude"] = true
	}
	if _, err := client.startApprovalTunnel(r.Context(), s.config.ControlURL); err == nil {
		checks["approvalTunnel"] = true
	} else {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "checks": checks, "hostKey": client.lastHostKey, "fingerprint": hostKeyFingerprint(client.lastHostKey), "error": errorText(err)})
		return
	}
	errorText := ""
	if !checks["claude"] {
		errorText = "远端未检测到可用的 Claude Code"
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "claudeReady": checks["claude"], "checks": checks, "hostKey": client.lastHostKey, "fingerprint": hostKeyFingerprint(client.lastHostKey), "resolved": map[string]any{"host": connection.Host, "port": connection.Port, "user": connection.User, "privateKeyPath": connection.PrivateKeyPath}, "error": errorText})
}

func hostKeyFingerprint(rawKey string) string {
	key, err := parsePinnedHostKey(rawKey)
	if err != nil {
		return ""
	}
	return ssh.FingerprintSHA256(key)
}

func (s *Server) listSSHConnections(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(), `select id,name,host,port,user,private_key_path,known_hosts,root_path,status,last_seen,error_msg,created_at,updated_at,password from ssh_connections order by created_at desc`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()
	items := make([]map[string]any, 0)
	for rows.Next() {
		var c SSHConnection
		if err := rows.Scan(&c.ID, &c.Name, &c.Host, &c.Port, &c.User, &c.PrivateKeyPath, &c.KnownHosts, &c.RootPath, &c.Status, &c.LastSeen, &c.ErrorMsg, &c.CreatedAt, &c.UpdatedAt, &c.Password); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		items = append(items, sanitizedSSHConnection(c))
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) getSSHConnection(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "connectionID")
	var c SSHConnection
	err := s.db.QueryRowContext(r.Context(), `select id,name,host,port,user,private_key_path,known_hosts,root_path,status,last_seen,error_msg,created_at,updated_at, password from ssh_connections where id=?`, id).
		Scan(&c.ID, &c.Name, &c.Host, &c.Port, &c.User, &c.PrivateKeyPath, &c.KnownHosts, &c.RootPath, &c.Status, &c.LastSeen, &c.ErrorMsg, &c.CreatedAt, &c.UpdatedAt, &c.Password)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("SSH connection not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, sanitizedSSHConnection(c))
}

type sshConnectionInput struct {
	Name           string `json:"name"`
	Host           string `json:"host"`
	Port           int    `json:"port"`
	User           string `json:"user"`
	PrivateKeyPath string `json:"privateKeyPath"`
	AuthMethod     string `json:"authMethod"`
	Password       string `json:"password"`
	RootPath       string `json:"rootPath"`
	ProfileName    string `json:"profileName"`
	HostKey        string `json:"hostKey"`
	Connect        bool   `json:"connect"`
	ConnectionID   string `json:"connectionId"`
	Update         bool   `json:"-"`
}

func (input *sshConnectionInput) resolve() error {
	if strings.TrimSpace(input.ProfileName) != "" {
		profile, err := resolveSSHProfile(strings.TrimSpace(input.ProfileName))
		if err != nil {
			return err
		}
		input.Host, input.Port, input.User, input.PrivateKeyPath = profile.Host, profile.Port, profile.User, profile.PrivateKeyPath
	}
	if strings.TrimSpace(input.Name) == "" || strings.TrimSpace(input.Host) == "" || strings.TrimSpace(input.User) == "" {
		return errors.New("name, host, and user are required")
	}
	if input.AuthMethod == "password" {
		if strings.TrimSpace(input.Password) == "" {
			// During an edit the existing password is preserved when the user does
			// not supply a new one — see updateSSHConnection.
			if !input.Update {
				return errors.New("请输入密码")
			}
		}
	} else {
		if strings.TrimSpace(input.PrivateKeyPath) == "" && os.Getenv("SSH_AUTH_SOCK") == "" {
			if !input.Update {
				return errors.New("请指定私钥路径，或启动并配置 SSH Agent")
			}
		}
	}
	if input.Port <= 0 || input.Port > 65535 {
		input.Port = 22
	}
	if strings.TrimSpace(input.RootPath) == "" {
		return errors.New("请设置远端项目根路径")
	}
	return nil
}

func (input sshConnectionInput) connection() SSHConnection {
	return SSHConnection{
		ID:             uuid.NewString(),
		Name:           strings.TrimSpace(input.Name),
		Host:           strings.TrimSpace(input.Host),
		Port:           input.Port,
		User:           strings.TrimSpace(input.User),
		PrivateKeyPath: strings.TrimSpace(input.PrivateKeyPath),
		Password:       strings.TrimSpace(input.Password),
		KnownHosts:     strings.TrimSpace(input.HostKey),
		RootPath:       strings.TrimSpace(input.RootPath),
		Status:         "unknown",
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
	}
}

func (s *Server) createSSHConnection(w http.ResponseWriter, r *http.Request) {
	var input sshConnectionInput
	if !decode(w, r, &input) {
		return
	}
	if err := input.resolve(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if _, err := parsePinnedHostKey(input.HostKey); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("请先完成预检并确认远端主机指纹"))
		return
	}
	c := input.connection()
	s.saveSSHConnection(w, r, &c, input.Connect)
}

// updateSSHConnection edits an existing SSH connection. Identity and creation
// time are preserved; a blank password/private key (kept secret from the API)
// is carried over from the stored row so editing other fields does not erase
// the saved credentials.
func (s *Server) updateSSHConnection(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "connectionID")
	// Replacing the underlying SSH client tears down remote sessions, so editing
	// a connected runner must be serialized against run admission and deletion.
	// The lock is held across load+save so a concurrent delete cannot revive the
	// row via the upsert below (TOCTOU).
	s.projectLifecycleMu.Lock()
	defer s.projectLifecycleMu.Unlock()
	existing, err := s.loadSSHConnection(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, errors.New("SSH connection not found"))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	var input sshConnectionInput
	if !decode(w, r, &input) {
		return
	}
	input.Update = true
	if err := input.resolve(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if _, err := parsePinnedHostKey(input.HostKey); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("请先完成预检并确认远端主机指纹"))
		return
	}
	c := input.connection()
	c.ID = existing.ID
	c.CreatedAt = existing.CreatedAt
	// 沿用原有凭据：预检/编辑表单不回显已保存的密码或私钥，留空即表示保持原值。
	// 切换认证方式时，清除不再使用的那一侧凭据，避免残留旧的密码或私钥。
	if input.AuthMethod == "password" {
		if strings.TrimSpace(input.Password) == "" {
			c.Password = existing.Password
		}
		c.PrivateKeyPath = ""
	} else {
		c.Password = ""
		if strings.TrimSpace(input.PrivateKeyPath) == "" {
			c.PrivateKeyPath = existing.PrivateKeyPath
		}
	}
	knownHosts := strings.TrimSpace(input.HostKey)
	if knownHosts == "" {
		knownHosts = existing.KnownHosts
	}
	c.KnownHosts = knownHosts

	if err := s.ensureSSHRunnerInactive(r.Context(), "ssh-"+c.ID); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	s.saveSSHConnection(w, r, &c, input.Connect)
}

// saveSSHConnection persists (inserts or updates) a connection and optionally
// connects it, writing the shared success/error handling. The connection struct
// must already carry its final ID and secrets.
func (s *Server) saveSSHConnection(w http.ResponseWriter, r *http.Request, c *SSHConnection, connect bool) {
	_, err := s.db.ExecContext(r.Context(), `
		insert into ssh_connections (id,name,host,port,user,private_key_path,known_hosts,root_path,status,error_msg,created_at,updated_at,password)
		values (?,?,?,?,?,?,?,?,?,?,?,?,?)
		on conflict(id) do update set
			name=excluded.name, host=excluded.host, port=excluded.port, user=excluded.user,
			private_key_path=excluded.private_key_path, known_hosts=excluded.known_hosts,
			root_path=excluded.root_path, updated_at=excluded.updated_at, password=excluded.password`,
		c.ID, c.Name, c.Host, c.Port, c.User, c.PrivateKeyPath, c.KnownHosts, c.RootPath, c.Status, c.ErrorMsg, c.CreatedAt, c.UpdatedAt, c.Password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("save ssh connection: %w", err))
		return
	}

	if connect {
		if err := s.tryConnectAndRegister(r.Context(), c); err != nil {
			// tryConnectAndRegister already wrote the correct status: "error" when no
			// prior runner survived, or left "connected" untouched when the old runner
			// is still alive. Reload that authoritative status instead of overwriting
			// it here — clobbering would mislabel a still-connected runner as errored.
			var status, errorMsg string
			if rowErr := s.db.QueryRowContext(r.Context(), `select status,error_msg from ssh_connections where id=?`, c.ID).Scan(&status, &errorMsg); rowErr == nil {
				c.Status = status
				c.ErrorMsg = errorMsg
			}
		}
	}
	writeJSON(w, http.StatusOK, sanitizedSSHConnection(*c))
}

func (s *Server) deleteSSHConnection(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "connectionID")
	if id == "" {
		writeError(w, http.StatusBadRequest, errors.New("connection ID is required"))
		return
	}
	// Serialize deletion with remote project creation. projects.runner is not a
	// foreign key, so the count-and-delete check must share creation's lifecycle
	// lock to avoid persisting a project that points at an unregistered runner.
	s.projectLifecycleMu.Lock()
	defer s.projectLifecycleMu.Unlock()
	s.sshMu.Lock()
	defer s.sshMu.Unlock()
	runnerID := "ssh-" + id
	var projCount int
	if err := s.db.QueryRowContext(r.Context(), `select count(*) from projects where coalesce(nullif(runner_id,''),runner)=?`, runnerID).Scan(&projCount); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if projCount > 0 {
		writeError(w, http.StatusConflict, fmt.Errorf("此连接下有 %d 个项目，请先删除关联项目后再删除连接", projCount))
		return
	}
	if _, err := s.db.ExecContext(r.Context(), `delete from ssh_connections where id=?`, id); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// The database row is gone, so no new Runner can be registered for it.
	if oldRunner, ok := s.runnerRegistry.get(runnerID); ok {
		if oldSSH, ok := oldRunner.(*sshRunner); ok {
			_ = oldSSH.client.close()
		}
	}
	s.runnerRegistry.unregister(runnerID)
	writeJSON(w, http.StatusOK, map[string]string{"id": id})
}

func (s *Server) testSSHConnection(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "connectionID")
	c, err := s.loadSSHConnection(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, errors.New("SSH connection not found"))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	runner, _, err := s.prepareSSHRunner(r.Context(), *c)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": errorText(err)})
		return
	}
	_ = runner.client.close()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "hostname": c.Host})
}

func (s *Server) connectSSHConnection(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "connectionID")
	c, err := s.loadSSHConnection(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, errors.New("SSH connection not found"))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// Keep runner replacement mutually exclusive with run admission. Replacing
	// an SSH client tears down its remote sessions, so it must never happen
	// while this runner owns a queued or running task.
	s.projectLifecycleMu.Lock()
	defer s.projectLifecycleMu.Unlock()
	if err := s.ensureSSHRunnerInactive(r.Context(), "ssh-"+c.ID); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	if err := s.tryConnectAndRegister(r.Context(), c); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("连接失败：%w", err))
		return
	}
	writeJSON(w, http.StatusOK, sanitizedSSHConnection(*c))
}

func (s *Server) disconnectSSHConnection(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "connectionID")
	s.projectLifecycleMu.Lock()
	defer s.projectLifecycleMu.Unlock()
	c, err := s.loadSSHConnection(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, errors.New("SSH connection not found"))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	runnerID := "ssh-" + c.ID
	if err := s.ensureSSHRunnerInactive(r.Context(), runnerID); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	s.sshMu.Lock()
	defer s.sshMu.Unlock()
	if oldRunner, ok := s.runnerRegistry.get(runnerID); ok {
		if oldSSH, ok := oldRunner.(*sshRunner); ok {
			_ = oldSSH.client.close()
		}
	}
	c.Status = "disconnected"
	c.UpdatedAt = time.Now().UTC()
	if _, err := s.db.ExecContext(r.Context(), `update ssh_connections set status=?,error_msg='',updated_at=? where id=?`, c.Status, c.UpdatedAt, c.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.runnerRegistry.unregister(runnerID)
	writeJSON(w, http.StatusOK, sanitizedSSHConnection(*c))
}

func (s *Server) ensureSSHRunnerInactive(ctx context.Context, runnerID string) error {
	var active bool
	if err := s.db.QueryRowContext(ctx, `select exists(select 1 from runs where agent_runtime_id=? and status in ('queued','running'))`, runnerID).Scan(&active); err != nil {
		return err
	}
	if active {
		return errors.New("该 SSH Runner 存在活动任务；请先停止或等待任务完成")
	}
	return nil
}

// loadSSHConnection reads a single SSH connection from the database.
func (s *Server) loadSSHConnection(ctx context.Context, id string) (*SSHConnection, error) {
	var c SSHConnection
	err := s.db.QueryRowContext(ctx, `select id,name,host,port,user,private_key_path,known_hosts,root_path,status,last_seen,error_msg,created_at,updated_at, password from ssh_connections where id=?`, id).
		Scan(&c.ID, &c.Name, &c.Host, &c.Port, &c.User, &c.PrivateKeyPath, &c.KnownHosts, &c.RootPath, &c.Status, &c.LastSeen, &c.ErrorMsg, &c.CreatedAt, &c.UpdatedAt, &c.Password)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// prepareSSHRunner fully validates a candidate connection without changing the
// registry or its persisted status. The caller owns the returned client.
func (s *Server) prepareSSHRunner(ctx context.Context, c SSHConnection) (_ *sshRunner, _ RunnerMeta, err error) {
	if s.sshPrepare != nil {
		return s.sshPrepare(ctx, c)
	}
	client, err := newSSHClient(c, false)
	if err != nil {
		return nil, RunnerMeta{}, err
	}
	defer func() {
		if err != nil {
			_ = client.close()
		}
	}()
	if _, err := client.execCommand(ctx, "echo ok"); err != nil {
		return nil, RunnerMeta{}, err
	}
	rootOutput, err := client.execCommand(ctx, "readlink -f -- "+shellQuote(c.RootPath))
	if err != nil {
		return nil, RunnerMeta{}, fmt.Errorf("无法解析远端根路径：%w", err)
	}
	rootPath := strings.TrimSpace(string(rootOutput))
	if rootPath == "" || !strings.HasPrefix(rootPath, "/") {
		return nil, RunnerMeta{}, errors.New("远端根路径不存在或不是绝对路径")
	}
	if _, err := client.readDir(ctx, rootPath); err != nil {
		return nil, RunnerMeta{}, fmt.Errorf("无法读取远端根路径：%w", err)
	}
	approvalURL, err := client.startApprovalTunnel(ctx, s.config.ControlURL)
	if err != nil {
		return nil, RunnerMeta{}, err
	}
	runnerID := "ssh-" + c.ID
	runner := &sshRunner{
		client:                 client,
		connID:                 c.ID,
		connName:               c.Name,
		controlURL:             s.config.ControlURL,
		approvalURL:            approvalURL,
		rootPath:               rootPath,
		turnIdleTimeout:        s.config.ClaudeTurnIdleTimeout,
		initialResponseTimeout: s.config.ClaudeInitialResponseTimeout,
		toolResultTimeout:      s.config.ClaudeToolResultTimeout,
	}
	return runner, RunnerMeta{
		ID:          runnerID,
		Name:        c.Name,
		Environment: "remote-linux",
		Host:        fmt.Sprintf("%s@%s:%d", c.User, c.Host, c.Port),
		Root:        rootPath,
		Roots:       []RootEntry{{Name: c.Name + " /home", Path: rootPath, Label: "remote-linux"}},
	}, nil
}

// tryConnectAndRegister validates a candidate before replacing an existing
// runner. A failed reconnect leaves an already-running connection untouched.
func (s *Server) tryConnectAndRegister(ctx context.Context, c *SSHConnection) error {
	s.sshMu.Lock()
	defer s.sshMu.Unlock()
	runnerID := "ssh-" + c.ID
	var exists bool
	if err := s.db.QueryRowContext(ctx, `select exists(select 1 from ssh_connections where id=?)`, c.ID).Scan(&exists); err != nil {
		return fmt.Errorf("读取 SSH 连接配置失败：%w", err)
	}
	if !exists {
		return errors.New("SSH 连接配置已被删除")
	}
	oldRunner, hadOldRunner := s.runnerRegistry.get(runnerID)
	runner, meta, err := s.prepareSSHRunner(ctx, *c)
	if err != nil {
		if !hadOldRunner {
			s.markSSHStatus(ctx, c, "error", errorText(err))
		}
		return err
	}
	now := time.Now().UTC()
	knownHosts := c.KnownHosts
	if knownHosts == "" {
		knownHosts = runner.client.lastHostKey
	}
	if _, err := s.db.ExecContext(ctx, `update ssh_connections set known_hosts=?,root_path=?,status='connected',last_seen=?,error_msg='',updated_at=? where id=?`, knownHosts, runner.rootPath, now, now, c.ID); err != nil {
		_ = runner.client.close()
		return fmt.Errorf("保存 SSH 连接状态失败：%w", err)
	}
	c.KnownHosts = knownHosts
	c.RootPath = runner.rootPath
	c.Status, c.ErrorMsg, c.LastSeen, c.UpdatedAt = "connected", "", &now, now
	s.runnerRegistry.register(runnerID, runner, meta)
	if oldSSH, ok := oldRunner.(*sshRunner); ok {
		_ = oldSSH.client.close()
	}
	return nil
}

func (s *Server) markSSHStatus(ctx context.Context, c *SSHConnection, status, errMsg string) {
	now := time.Now().UTC()
	c.Status = status
	c.ErrorMsg = errMsg
	c.UpdatedAt = now
	if status == "connected" {
		c.LastSeen = &now
	}
	s.db.ExecContext(ctx, `update ssh_connections set status=?,last_seen=?,error_msg=?,updated_at=? where id=?`, c.Status, c.LastSeen, c.ErrorMsg, c.UpdatedAt, c.ID)
}

// recoverSSHConnections is called during startup to re-register SSH connections
// that were previously connected.
func (s *Server) recoverSSHConnections(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `select id,name,host,port,user,private_key_path,known_hosts,root_path,status,last_seen,error_msg,created_at,updated_at, password from ssh_connections where status='connected'`)
	if err != nil {
		return fmt.Errorf("query ssh connections for recovery: %w", err)
	}
	connections := []SSHConnection{}
	for rows.Next() {
		var c SSHConnection
		if err := rows.Scan(&c.ID, &c.Name, &c.Host, &c.Port, &c.User, &c.PrivateKeyPath, &c.KnownHosts, &c.RootPath, &c.Status, &c.LastSeen, &c.ErrorMsg, &c.CreatedAt, &c.UpdatedAt, &c.Password); err != nil {
			rows.Close()
			return fmt.Errorf("scan ssh connection: %w", err)
		}
		connections = append(connections, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for index := range connections {
		c := &connections[index]
		log.Printf("[ssh] recovering connection %q (%s@%s:%d)", c.Name, c.User, c.Host, c.Port)
		if err := s.tryConnectAndRegister(ctx, c); err != nil {
			log.Printf("[ssh] recovery failed for %q: %v", c.Name, err)
			continue
		}
		log.Printf("[ssh] recovered connection %q", c.Name)
	}
	return nil
}
