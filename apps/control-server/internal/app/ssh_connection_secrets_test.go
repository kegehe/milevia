package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testHostKey is a syntactically valid authorized-key line reused across tests
// that must pass the preflight host-key requirement.
const testHostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBd875TBQVvl6l2xrvhta9tuJAvo0toGSWCQaQsBRzYd"

// TestSSHConnectionPasswordEncryptedAtRest 验证新增/更新 SSH 连接时，密码以
// 主密钥加密后写入数据库，明文不落盘；loadSSHConnection 能解回明文供 SSH
// 客户端使用。
func TestSSHConnectionPasswordEncryptedAtRest(t *testing.T) {
	server := newTestServer(t)
	const password = "s3cret-passphrase"
	body := `{"name":"sec","host":"example.test","user":"dev","port":22,"authMethod":"password","password":"` + password + `","rootPath":"/srv/projects","hostKey":"` + testHostKey + `","connect":false}`
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/ssh-connections", strings.NewReader(body)))
	if response.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
	}
	var created map[string]any
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatal("create response did not include connection id")
	}

	var stored string
	if err := server.db.QueryRow(`select password from ssh_connections where id=?`, id).Scan(&stored); err != nil {
		t.Fatalf("read stored password: %v", err)
	}
	if strings.Contains(stored, password) {
		t.Fatalf("password stored in plaintext: %q", stored)
	}
	if !strings.HasPrefix(stored, sshPasswordCipherPrefix) {
		t.Fatalf("stored password lacks cipher prefix: %q", stored)
	}

	loaded, err := server.loadSSHConnection(context.Background(), id)
	if err != nil {
		t.Fatalf("load connection: %v", err)
	}
	if loaded.Password != password {
		t.Fatalf("loadSSHConnection returned %q, want %q", loaded.Password, password)
	}
}

// TestSSHConnectionUpdateRewritesEncryptedPassword 验证编辑连接（密码留空保持
// 原值）时，数据库中的密文被替换为新的密文，明文仍然不落盘。
func TestSSHConnectionUpdateRewritesEncryptedPassword(t *testing.T) {
	server := newTestServer(t)
	connection := seedSSHConnectionForTest(t, server, "disconnected")
	if _, err := server.db.Exec(`update ssh_connections set password=? where id=?`, "first-password", connection.ID); err != nil {
		t.Fatalf("seed password: %v", err)
	}
	body := `{"name":"renamed","host":"example.test","user":"dev","privateKeyPath":"","rootPath":"/srv/projects","authMethod":"password","password":"","hostKey":"` + testHostKey + `","connect":false}`
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/api/ssh-connections/"+connection.ID, strings.NewReader(body)))
	if response.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", response.Code, response.Body.String())
	}
	var stored string
	if err := server.db.QueryRow(`select password from ssh_connections where id=?`, connection.ID).Scan(&stored); err != nil {
		t.Fatalf("read stored password: %v", err)
	}
	if strings.Contains(stored, "first-password") {
		t.Fatalf("password stored in plaintext after update: %q", stored)
	}
	if !strings.HasPrefix(stored, sshPasswordCipherPrefix) {
		t.Fatalf("stored password lacks cipher prefix: %q", stored)
	}
	loaded, err := server.loadSSHConnection(context.Background(), connection.ID)
	if err != nil {
		t.Fatalf("reload connection: %v", err)
	}
	if loaded.Password != "first-password" {
		t.Fatalf("blank-password update did not preserve secret: %q", loaded.Password)
	}
}

// TestCreateSSHConnectionConnectUsesPlaintextPassword 验证创建连接且 connect=true
// 时，密码加密只发生在持久化边界：传给建连路径（sshPrepare / newSSHClient）的
// 仍是明文，而数据库里是密文。防止将来把 saveSSHConnection 里的 c.Password 提前
// 覆盖成密文导致建连失败。
func TestCreateSSHConnectionConnectUsesPlaintextPassword(t *testing.T) {
	server := newTestServer(t)
	var seen SSHConnection
	server.sshPrepare = func(_ context.Context, c SSHConnection) (*sshRunner, RunnerMeta, error) {
		seen = c
		return &sshRunner{client: &sshClient{}, rootPath: c.RootPath}, RunnerMeta{ID: "ssh-" + c.ID, Root: c.RootPath}, nil
	}
	const password = "hunter2-plaintext"
	body := `{"name":"sec","host":"example.test","user":"dev","authMethod":"password","password":"` + password + `","rootPath":"/srv/projects","hostKey":"` + testHostKey + `","connect":true}`
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/ssh-connections", strings.NewReader(body)))
	if response.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
	}
	if seen.Password != password {
		t.Fatalf("connect path received %q, want plaintext %q", seen.Password, password)
	}
	var stored string
	if err := server.db.QueryRow(`select password from ssh_connections where id=?`, seen.ID).Scan(&stored); err != nil {
		t.Fatalf("read stored password: %v", err)
	}
	if strings.Contains(stored, password) {
		t.Fatalf("password stored in plaintext: %q", stored)
	}
}

// TestRecoverSSHConnectionsSkipsUndecryptablePassword 验证单个连接的密码解密
// 失败（如主密钥轮换后旧密文不可解）时，启动恢复只跳过该连接，不影响其他
// 连接（尤其是密钥认证、不依赖密码的连接）继续恢复。
func TestRecoverSSHConnectionsSkipsUndecryptablePassword(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	// 好行：密钥认证（密码为空），应正常恢复。
	if _, err := server.db.Exec(`insert into ssh_connections (id,name,host,port,user,private_key_path,known_hosts,root_path,status,error_msg,created_at,updated_at,password) values ('good','good','host',22,'user','key','','/workspace','connected','',?,?,'')`, now, now); err != nil {
		t.Fatalf("seed good connection: %v", err)
	}
	// 坏行：密码是损坏的密文（带前缀但无法解密），应被跳过而非中止整批恢复。
	if _, err := server.db.Exec(`insert into ssh_connections (id,name,host,port,user,private_key_path,known_hosts,root_path,status,error_msg,created_at,updated_at,password) values ('bad','bad','host',22,'user','key','','/workspace','connected','',?,?,?)`, now, now, "enc:v1:!!!not-valid-ciphertext!!!"); err != nil {
		t.Fatalf("seed bad connection: %v", err)
	}
	server.sshPrepare = func(_ context.Context, connection SSHConnection) (*sshRunner, RunnerMeta, error) {
		return &sshRunner{client: &sshClient{}, connID: connection.ID, rootPath: connection.RootPath}, RunnerMeta{ID: "ssh-" + connection.ID, Root: connection.RootPath}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.recoverSSHConnections(ctx); err != nil {
		t.Fatalf("recover ssh connections: %v", err)
	}
	if _, ok := server.runnerRegistry.get("ssh-good"); !ok {
		t.Fatal("valid connection was not recovered")
	}
	if _, ok := server.runnerRegistry.get("ssh-bad"); ok {
		t.Fatal("connection with undecryptable password was unexpectedly recovered")
	}
}

func TestRecoverSSHConnectionsDecryptsPasswordBeforeConnecting(t *testing.T) {
	server := newTestServer(t)
	ciphertext, err := server.encryptSSHPassword("recover-password")
	if err != nil {
		t.Fatalf("encrypt password: %v", err)
	}
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into ssh_connections (id,name,host,port,user,private_key_path,known_hosts,root_path,status,error_msg,created_at,updated_at,password) values ('password','password','host',22,'user','','','/workspace','connected','',?,?,?)`, now, now, ciphertext); err != nil {
		t.Fatalf("seed password connection: %v", err)
	}
	var seen SSHConnection
	server.sshPrepare = func(_ context.Context, connection SSHConnection) (*sshRunner, RunnerMeta, error) {
		seen = connection
		return &sshRunner{client: &sshClient{}, connID: connection.ID, rootPath: connection.RootPath}, RunnerMeta{ID: "ssh-" + connection.ID, Root: connection.RootPath}, nil
	}
	if err := server.recoverSSHConnections(context.Background()); err != nil {
		t.Fatalf("recover ssh connections: %v", err)
	}
	if seen.Password != "recover-password" {
		t.Fatalf("recovery received password %q, want plaintext", seen.Password)
	}
}

// TestMigrateSSHConnectionsEncryptsLegacyPlaintext 模拟升级前已存在明文密码的
// 数据库：启动迁移（migrateSSHConnections）应把明文加密，且解密后可正常使用。
func TestMigrateSSHConnectionsEncryptsLegacyPlaintext(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into ssh_connections (id,name,host,port,user,private_key_path,known_hosts,root_path,status,error_msg,created_at,updated_at,password) values ('legacy','legacy','host',22,'user','key','','/workspace','disconnected','',?,?,?)`, now, now, "legacy-plaintext"); err != nil {
		t.Fatalf("seed legacy connection: %v", err)
	}

	if err := server.migrateSSHConnections(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var stored string
	if err := server.db.QueryRow(`select password from ssh_connections where id='legacy'`).Scan(&stored); err != nil {
		t.Fatalf("read migrated password: %v", err)
	}
	if strings.Contains(stored, "legacy-plaintext") {
		t.Fatalf("legacy password not encrypted by migration: %q", stored)
	}
	if !strings.HasPrefix(stored, sshPasswordCipherPrefix) {
		t.Fatalf("migrated password lacks cipher prefix: %q", stored)
	}
	loaded, err := server.loadSSHConnection(context.Background(), "legacy")
	if err != nil {
		t.Fatalf("load migrated connection: %v", err)
	}
	if loaded.Password != "legacy-plaintext" {
		t.Fatalf("migrated password did not decrypt back: %q", loaded.Password)
	}

	// 迁移需幂等：再跑一次不应改变密文。
	secondRun, err := server.loadSSHConnection(context.Background(), "legacy")
	if err != nil {
		t.Fatalf("reload after second migrate: %v", err)
	}
	if secondRun.Password != "legacy-plaintext" {
		t.Fatalf("password changed after idempotent migrate: %q", secondRun.Password)
	}
	var storedAfter string
	if err := server.db.QueryRow(`select password from ssh_connections where id='legacy'`).Scan(&storedAfter); err != nil {
		t.Fatalf("read after second migrate: %v", err)
	}
	if storedAfter != stored {
		t.Fatal("migrateSSHConnections re-encrypted an already-encrypted password")
	}
}
