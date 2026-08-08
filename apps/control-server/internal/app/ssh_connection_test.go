package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

type sshTurnSink struct {
	started  int
	finished int
	errs     []error
}

func (sink *sshTurnSink) Event(string, json.RawMessage) {}
func (sink *sshTurnSink) AssistantText(string, string)  {}
func (sink *sshTurnSink) SessionIdentified(string)      {}
func (sink *sshTurnSink) SessionInitialized()           {}
func (sink *sshTurnSink) TurnStarted()                  { sink.started++ }
func (sink *sshTurnSink) TurnFinished(err error)        { sink.finished++; sink.errs = append(sink.errs, err) }

type sshTestWriteCloser struct{ bytes.Buffer }

func (sshTestWriteCloser) Close() error { return nil }

type sshOutputTestSink struct {
	events []json.RawMessage
	texts  []string
}

func (sink *sshOutputTestSink) Event(_ string, payload json.RawMessage) {
	sink.events = append(sink.events, append(json.RawMessage(nil), payload...))
}
func (sink *sshOutputTestSink) AssistantText(text, _ string) { sink.texts = append(sink.texts, text) }
func (*sshOutputTestSink) SessionIdentified(string)          {}
func (*sshOutputTestSink) SessionInitialized()               {}

func TestSSHOutputRedactsCredentialsBeforeEmitting(t *testing.T) {
	const secret = "sk-ssh-test-secret-value-12345"
	sink := &sshOutputTestSink{}
	if err := readClaudeJSONLines(strings.NewReader(`{"type":"assistant","api_key":"`+secret+`","message":{"content":[{"type":"text","text":"Authorization: Bearer `+secret+`"}]}}`+"\n"), sink); err != nil {
		t.Fatalf("read Claude JSONL: %v", err)
	}
	readStderrLines(strings.NewReader("ANTHROPIC_API_KEY="+secret+"\n"), sink)
	for _, payload := range sink.events {
		if strings.Contains(string(payload), secret) {
			t.Fatalf("credential leaked in event: %s", payload)
		}
	}
	for _, text := range sink.texts {
		if strings.Contains(text, secret) {
			t.Fatalf("credential leaked in assistant text: %s", text)
		}
	}
	if len(sink.texts) != 1 || !strings.Contains(sink.texts[0], "[REDACTED]") {
		t.Fatalf("assistant text was not redacted: %#v", sink.texts)
	}
}

func TestRemotePathWithinRoot(t *testing.T) {
	cases := []struct {
		root string
		path string
		want bool
	}{
		{"/srv/projects", "/srv/projects", true},
		{"/srv/projects", "/srv/projects/api", true},
		{"/srv/projects", "/srv/projects-other/api", false},
		{"/srv/projects", "/srv", false},
		{"/", "/", true},
		{"/", "/home/dev/project", true},
	}
	for _, test := range cases {
		if got := remotePathWithinRoot(test.root, test.path); got != test.want {
			t.Errorf("remotePathWithinRoot(%q, %q) = %v, want %v", test.root, test.path, got, test.want)
		}
	}
}

func TestSSHConnectCancelsStalledHandshake(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	port := listener.Addr().(*net.TCPAddr).Port
	client := &sshClient{
		host: "127.0.0.1",
		port: port,
		config: ssh.ClientConfig{
			User:            "test",
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- client.connect(ctx) }()
	var conn net.Conn
	select {
	case conn = <-accepted:
		defer conn.Close()
	case <-time.After(time.Second):
		t.Fatal("SSH client never reached the handshake")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("connect error=%v, want context cancellation", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("cancelled SSH handshake waited for its deadline")
	}
}

func TestSSHAgentSessionCompletesAndQueuesTurns(t *testing.T) {
	writer := &sshTestWriteCloser{}
	first, second := &sshTurnSink{}, &sshTurnSink{}
	session := &sshAgentSession{
		stdin:  writer,
		stdout: strings.NewReader(`{"type":"result","subtype":"success"}` + "\n" + `{"type":"result","subtype":"success"}` + "\n"),
	}
	if err := session.Send(AgentRunRequest{Prompt: "first"}, first); err != nil {
		t.Fatalf("send first turn: %v", err)
	}
	if err := session.Send(AgentRunRequest{Prompt: "second"}, second); err != nil {
		t.Fatalf("queue second turn: %v", err)
	}
	if err := session.readOutputLoop(); err != nil {
		t.Fatalf("read output: %v", err)
	}
	if first.started != 1 || first.finished != 1 || second.started != 1 || second.finished != 1 {
		t.Fatalf("turn lifecycle: first=%+v second=%+v", first, second)
	}
	if strings.Count(writer.String(), `"type":"user"`) != 2 {
		t.Fatalf("queued turn was not sent after first result: %q", writer.String())
	}
}

func TestParsePinnedHostKeyRejectsMalformedValue(t *testing.T) {
	if _, err := parsePinnedHostKey("not a public key"); err == nil {
		t.Fatal("expected malformed host key to be rejected")
	}

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatalf("create signer: %v", err)
	}
	if _, err := parsePinnedHostKey(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))); err != nil {
		t.Fatalf("parse valid host key: %v", err)
	}
}

func TestCreateSSHConnectionRequiresPreflightHostKey(t *testing.T) {
	server := newTestServer(t)
	request := httptest.NewRequest(http.MethodPost, "/api/ssh-connections", strings.NewReader(`{"name":"remote","host":"example.test","user":"dev","privateKeyPath":"/tmp/key","rootPath":"/srv/projects"}`))
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusBadRequest, response.Body.String())
	}
}

func TestSSHConnectionDeleteWaitsForProjectLifecycle(t *testing.T) {
	server := newTestServer(t)
	connection := seedSSHConnectionForTest(t, server, "disconnected")
	server.projectLifecycleMu.Lock()
	locked := true
	defer func() {
		if locked {
			server.projectLifecycleMu.Unlock()
		}
	}()
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/api/ssh-connections/"+connection.ID, nil))
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("SSH deletion bypassed the project lifecycle lock")
	case <-time.After(100 * time.Millisecond):
	}
	server.projectLifecycleMu.Unlock()
	locked = false
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("SSH deletion did not resume after lifecycle lock release")
	}
	if response.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestRemoteProjectCreationWaitsForSSHConnectionLifecycle(t *testing.T) {
	server := newTestServer(t)
	runnerID := "ssh-test"
	server.runnerRegistry.register(runnerID, &sshRunner{client: &sshClient{closed: true}, rootPath: "/srv/projects"}, RunnerMeta{ID: runnerID})
	server.projectLifecycleMu.Lock()
	locked := true
	defer func() {
		if locked {
			server.projectLifecycleMu.Unlock()
		}
	}()
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects", strings.NewReader(`{"name":"remote","path":"/srv/projects/demo","runner":"ssh-test"}`)))
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("remote project creation bypassed the SSH lifecycle lock")
	case <-time.After(100 * time.Millisecond):
	}
	server.projectLifecycleMu.Unlock()
	locked = false
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("remote project creation did not resume after lifecycle lock release")
	}
	if response.Code != http.StatusBadRequest {
		t.Fatalf("creation status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestSSHConfigProfilesIgnoresWildcardEntries(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config")
	config := "Host dev-api staging\n  HostName 192.0.2.10\nHost *\n  ServerAliveInterval 60\nHost *.internal\n  User dev\n"
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	profiles, err := listSSHProfilesFromFile(configPath)
	if err != nil {
		t.Fatalf("list profiles: %v", err)
	}
	if len(profiles) != 2 || profiles[0] != "dev-api" || profiles[1] != "staging" {
		t.Fatalf("profiles = %#v, want dev-api and staging", profiles)
	}
}

func TestGetSSHProfileReturnsResolvedConnectionParameters(t *testing.T) {
	originalGetSSHConfigOutput := getSSHConfigOutput
	t.Cleanup(func() { getSSHConfigOutput = originalGetSSHConfigOutput })
	getSSHConfigOutput = func(name string) ([]byte, error) {
		if name != "Tencent" {
			t.Fatalf("profile name=%q, want Tencent", name)
		}
		return []byte("hostname 192.0.2.10\nuser deploy\nport 2202\nidentityfile none\nproxyjump none\n"), nil
	}

	server := newTestServer(t)
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/ssh-profiles/Tencent", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var profile sshProfile
	if err := json.NewDecoder(response.Body).Decode(&profile); err != nil {
		t.Fatalf("decode profile: %v", err)
	}
	if profile.Host != "192.0.2.10" || profile.Port != 2202 || profile.User != "deploy" {
		t.Fatalf("profile=%+v", profile)
	}
}

func seedSSHConnectionForTest(t *testing.T, server *Server, status string) *SSHConnection {
	t.Helper()
	now := time.Now().UTC()
	connection := &SSHConnection{
		ID:             "ssh-test",
		Name:           "SSH test",
		Host:           "example.test",
		Port:           22,
		User:           "dev",
		PrivateKeyPath: "/tmp/test-key",
		KnownHosts:     "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEV4YW1wbGUgaG9zdCBrZXk=",
		RootPath:       "/srv/projects",
		Status:         status,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if _, err := server.db.Exec(`insert into ssh_connections (id,name,host,port,user,private_key_path,known_hosts,root_path,status,error_msg,created_at,updated_at,password) values (?,?,?,?,?,?,?,?,?,?,?,?,?)`, connection.ID, connection.Name, connection.Host, connection.Port, connection.User, connection.PrivateKeyPath, connection.KnownHosts, connection.RootPath, connection.Status, "", "", connection.CreatedAt, connection.UpdatedAt); err != nil {
		t.Fatalf("seed SSH connection: %v", err)
	}
	return connection
}

func TestFailedSSHReconnectKeepsExistingRunner(t *testing.T) {
	server := newTestServer(t)
	connection := seedSSHConnectionForTest(t, server, "connected")
	oldClient := &sshClient{}
	oldRunner := &sshRunner{client: oldClient, rootPath: connection.RootPath}
	runnerID := "ssh-" + connection.ID
	server.runnerRegistry.register(runnerID, oldRunner, RunnerMeta{ID: runnerID})
	server.sshPrepare = func(context.Context, SSHConnection) (*sshRunner, RunnerMeta, error) {
		return nil, RunnerMeta{}, errors.New("remote host unavailable")
	}

	err := server.tryConnectAndRegister(context.Background(), connection)
	if err == nil {
		t.Fatal("expected reconnect failure")
	}
	runner, ok := server.runnerRegistry.get(runnerID)
	if !ok || runner != oldRunner {
		t.Fatal("failed reconnect replaced the existing runner")
	}
	oldClient.mu.Lock()
	closed := oldClient.closed
	oldClient.mu.Unlock()
	if closed {
		t.Fatal("failed reconnect closed the existing SSH client")
	}
	var status string
	if err := server.db.QueryRow(`select status from ssh_connections where id=?`, connection.ID).Scan(&status); err != nil {
		t.Fatalf("read SSH status: %v", err)
	}
	if status != "connected" {
		t.Fatalf("status=%q, want connected", status)
	}
}

func TestSSHConnectionTestDoesNotReplaceRunner(t *testing.T) {
	server := newTestServer(t)
	connection := seedSSHConnectionForTest(t, server, "connected")
	oldRunner := &sshRunner{client: &sshClient{}, rootPath: connection.RootPath}
	runnerID := "ssh-" + connection.ID
	server.runnerRegistry.register(runnerID, oldRunner, RunnerMeta{ID: runnerID})
	candidateClient := &sshClient{}
	server.sshPrepare = func(_ context.Context, c SSHConnection) (*sshRunner, RunnerMeta, error) {
		return &sshRunner{client: candidateClient, rootPath: c.RootPath}, RunnerMeta{ID: runnerID}, nil
	}

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/ssh-connections/"+connection.ID+"/test", nil)
	server.routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("test status=%d body=%s", response.Code, response.Body.String())
	}
	runner, ok := server.runnerRegistry.get(runnerID)
	if !ok || runner != oldRunner {
		t.Fatal("connection test replaced the existing runner")
	}
	candidateClient.mu.Lock()
	closed := candidateClient.closed
	candidateClient.mu.Unlock()
	if !closed {
		t.Fatal("connection test did not close its temporary client")
	}
}

func TestSuccessfulSSHReconnectReplacesAndClosesOldRunner(t *testing.T) {
	server := newTestServer(t)
	connection := seedSSHConnectionForTest(t, server, "connected")
	oldClient := &sshClient{}
	oldRunner := &sshRunner{client: oldClient, rootPath: connection.RootPath}
	runnerID := "ssh-" + connection.ID
	server.runnerRegistry.register(runnerID, oldRunner, RunnerMeta{ID: runnerID})
	newRunner := &sshRunner{client: &sshClient{}, rootPath: "/srv/projects-canonical"}
	server.sshPrepare = func(_ context.Context, c SSHConnection) (*sshRunner, RunnerMeta, error) {
		return newRunner, RunnerMeta{ID: runnerID, Root: newRunner.rootPath}, nil
	}

	if err := server.tryConnectAndRegister(context.Background(), connection); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	runner, ok := server.runnerRegistry.get(runnerID)
	if !ok || runner != newRunner {
		t.Fatal("successful reconnect did not install the candidate runner")
	}
	oldClient.mu.Lock()
	closed := oldClient.closed
	oldClient.mu.Unlock()
	if !closed {
		t.Fatal("successful reconnect did not close the replaced SSH client")
	}
	var rootPath, status string
	if err := server.db.QueryRow(`select root_path,status from ssh_connections where id=?`, connection.ID).Scan(&rootPath, &status); err != nil {
		t.Fatalf("read SSH connection: %v", err)
	}
	if rootPath != newRunner.rootPath || status != "connected" {
		t.Fatalf("root=%q status=%q", rootPath, status)
	}
}

func TestActiveSSHRunnerCannotReconnectOrDisconnect(t *testing.T) {
	server := newTestServer(t)
	connection := seedSSHConnectionForTest(t, server, "connected")
	now := time.Now().UTC()
	runnerID := "ssh-" + connection.ID
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'main', 'main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`update projects set runner=? where id='project'`, runnerID); err != nil {
		t.Fatalf("set SSH runner: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,agent_runtime_id,status,claude_initialized,is_current,created_at) values ('conversation','project','session',?,'running',0,1,?)`, runnerID, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	if _, err := server.db.Exec(`insert into runs (id,conversation_id,agent_runtime_id,status,created_at) values ('run','conversation',?,'running',?)`, runnerID, now); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	prepareCalled := false
	server.sshPrepare = func(context.Context, SSHConnection) (*sshRunner, RunnerMeta, error) {
		prepareCalled = true
		return &sshRunner{client: &sshClient{}}, RunnerMeta{ID: runnerID}, nil
	}

	for _, path := range []string{"/api/ssh-connections/" + connection.ID + "/connect", "/api/ssh-connections/" + connection.ID + "/disconnect"} {
		response := httptest.NewRecorder()
		server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, nil))
		if response.Code != http.StatusConflict {
			t.Fatalf("%s status=%d body=%s", path, response.Code, response.Body.String())
		}
	}
	if prepareCalled {
		t.Fatal("reconnect validation ran despite an active SSH task")
	}
	var status string
	if err := server.db.QueryRow(`select status from ssh_connections where id=?`, connection.ID).Scan(&status); err != nil {
		t.Fatalf("load connection status: %v", err)
	}
	if status != "connected" {
		t.Fatalf("connection status=%q, want connected", status)
	}
}

func TestRecoverSSHConnectionsReleasesRowsBeforeReconnecting(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into ssh_connections (id,name,host,port,user,private_key_path,known_hosts,root_path,status,error_msg,created_at,updated_at,password) values ('connection','connection','host',22,'user','key','ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest','/workspace','connected','','',?,?)`, now, now); err != nil {
		t.Fatalf("insert ssh connection: %v", err)
	}
	server.sshPrepare = func(_ context.Context, connection SSHConnection) (*sshRunner, RunnerMeta, error) {
		return &sshRunner{client: &sshClient{}, connID: connection.ID, rootPath: connection.RootPath}, RunnerMeta{ID: "ssh-" + connection.ID, Root: connection.RootPath}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.recoverSSHConnections(ctx); err != nil {
		t.Fatalf("recover ssh connections: %v", err)
	}
	if _, ok := server.runnerRegistry.get("ssh-connection"); !ok {
		t.Fatal("recovered SSH runner was not registered")
	}
}

func TestUpdateSSHConnectionPreservesIdentityStatusAndSecrets(t *testing.T) {
	server := newTestServer(t)
	connection := seedSSHConnectionForTest(t, server, "disconnected")
	if _, err := server.db.Exec(`update ssh_connections set password=? where id=?`, "old-password", connection.ID); err != nil {
		t.Fatalf("seed password: %v", err)
	}
	before, err := server.loadSSHConnection(context.Background(), connection.ID)
	if err != nil {
		t.Fatalf("load original: %v", err)
	}
	body := `{"name":"renamed","host":"new.example.test","user":"dev","privateKeyPath":"","rootPath":"/new/path","authMethod":"password","password":"","hostKey":"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBd875TBQVvl6l2xrvhta9tuJAvo0toGSWCQaQsBRzYd","connect":false}`
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/api/ssh-connections/"+connection.ID, strings.NewReader(body)))
	if response.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", response.Code, response.Body.String())
	}
	updated, err := server.loadSSHConnection(context.Background(), connection.ID)
	if err != nil {
		t.Fatalf("reload connection: %v", err)
	}
	if updated.ID != before.ID || updated.CreatedAt != before.CreatedAt {
		t.Fatalf("identity not preserved: id=%s", updated.ID)
	}
	if updated.Name != "renamed" || updated.Host != "new.example.test" {
		t.Fatalf("fields not updated: %+v", updated)
	}
	if updated.Status != "disconnected" {
		t.Fatalf("status overwritten on non-connect edit: %q", updated.Status)
	}
	if updated.Password != "old-password" {
		t.Fatalf("blank password did not preserve existing: %q", updated.Password)
	}
}

func TestUpdateSSHConnectionRequiresPreflightHostKey(t *testing.T) {
	server := newTestServer(t)
	connection := seedSSHConnectionForTest(t, server, "disconnected")
	body := `{"name":"renamed","host":"new.example.test","user":"dev","privateKeyPath":"","rootPath":"/new/path","authMethod":"password","password":"","hostKey":"","connect":false}`
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/api/ssh-connections/"+connection.ID, strings.NewReader(body)))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("update without host key status=%d want 400", response.Code)
	}
}

func TestUpdateSSHConnectionMissingReturnsNotFound(t *testing.T) {
	server := newTestServer(t)
	body := `{"name":"x","host":"h","user":"u","privateKeyPath":"/tmp/k","rootPath":"/","authMethod":"key","password":"","hostKey":"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEV4YW1wbGUgaG9zdCBrZXk=","connect":false}`
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/api/ssh-connections/missing", strings.NewReader(body)))
	if response.Code != http.StatusNotFound {
		t.Fatalf("update missing status=%d want 404", response.Code)
	}
}

func TestPreflightEditReusesStoredPassword(t *testing.T) {
	server := newTestServer(t)
	connection := seedSSHConnectionForTest(t, server, "disconnected")
	if _, err := server.db.Exec(`update ssh_connections set password=? where id=?`, "stored-secret", connection.ID); err != nil {
		t.Fatalf("seed password: %v", err)
	}
	// 捕获预检实际使用的凭据：编辑模式密码为空时应沿用已保存的密码。
	var seen SSHConnection
	orig := newSSHClient
	newSSHClient = func(conn SSHConnection, trustOnFirstUse bool) (*sshClient, error) {
		seen = conn
		return nil, errors.New("stop after capture")
	}
	t.Cleanup(func() { newSSHClient = orig })
	// 编辑表单密码为空、认证方式为密码，附带 connectionId。
	body := `{"name":"renamed","host":"example.test","user":"dev","authMethod":"password","password":"","rootPath":"/srv/projects","connectionId":"` + connection.ID + `"}`
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/ssh-connections/preflight", strings.NewReader(body)))
	if seen.Password != "stored-secret" {
		t.Fatalf("preflight did not reuse stored password; got %q", seen.Password)
	}
	if seen.PrivateKeyPath != "" {
		t.Fatalf("password-auth edit carried over private key path: %q", seen.PrivateKeyPath)
	}
}

func TestPreflightEditReusesStoredPrivateKeyPath(t *testing.T) {
	server := newTestServer(t)
	connection := seedSSHConnectionForTest(t, server, "disconnected")
	var seen SSHConnection
	orig := newSSHClient
	newSSHClient = func(conn SSHConnection, trustOnFirstUse bool) (*sshClient, error) {
		seen = conn
		return nil, errors.New("stop after capture")
	}
	t.Cleanup(func() { newSSHClient = orig })
	body := `{"name":"renamed","host":"example.test","user":"dev","authMethod":"key","privateKeyPath":"","rootPath":"/srv/projects","connectionId":"` + connection.ID + `"}`
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/ssh-connections/preflight", strings.NewReader(body)))
	if seen.PrivateKeyPath != connection.PrivateKeyPath {
		t.Fatalf("preflight did not reuse stored private key path; got %q want %q", seen.PrivateKeyPath, connection.PrivateKeyPath)
	}
	if seen.Password != "" {
		t.Fatalf("key-auth edit carried over password: %q", seen.Password)
	}
}

func TestUpdateConnectedConnectionFailingReconnectKeepsConnectedStatus(t *testing.T) {
	server := newTestServer(t)
	connection := seedSSHConnectionForTest(t, server, "connected")
	// 注册一个存活的旧 runner，模拟已连接状态。
	oldRunner := &sshRunner{client: &sshClient{}, rootPath: connection.RootPath}
	runnerID := "ssh-" + connection.ID
	server.runnerRegistry.register(runnerID, oldRunner, RunnerMeta{ID: runnerID})
	// 让重连失败：prepareSSHRunner 返回错误，但旧 runner 应保留、状态应保持 connected。
	server.sshPrepare = func(context.Context, SSHConnection) (*sshRunner, RunnerMeta, error) {
		return nil, RunnerMeta{}, errors.New("remote host unreachable")
	}
	body := `{"name":"renamed","host":"example.test","user":"dev","privateKeyPath":"/tmp/test-key","authMethod":"key","password":"","rootPath":"/srv/projects","hostKey":"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBd875TBQVvl6l2xrvhta9tuJAvo0toGSWCQaQsBRzYd","connect":true}`
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/api/ssh-connections/"+connection.ID, strings.NewReader(body)))
	if response.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", response.Code, response.Body.String())
	}
	// 旧 runner 应仍存活。
	if runner, ok := server.runnerRegistry.get(runnerID); !ok || runner != oldRunner {
		t.Fatal("failed edit reconnect replaced the still-alive runner")
	}
	// DB 状态应保持 connected，而非被标为 error。
	var status string
	if err := server.db.QueryRow(`select status from ssh_connections where id=?`, connection.ID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "connected" {
		t.Fatalf("status = %q, want connected (old runner still alive)", status)
	}
}

func TestUpdateConnectionReconnectsAndRegistersNewRunner(t *testing.T) {
	server := newTestServer(t)
	connection := seedSSHConnectionForTest(t, server, "disconnected")
	newRunner := &sshRunner{client: &sshClient{}, rootPath: "/srv/projects"}
	server.sshPrepare = func(_ context.Context, c SSHConnection) (*sshRunner, RunnerMeta, error) {
		return newRunner, RunnerMeta{ID: "ssh-" + c.ID, Root: newRunner.rootPath}, nil
	}
	body := `{"name":"renamed","host":"example.test","user":"dev","privateKeyPath":"/tmp/test-key","authMethod":"key","password":"","rootPath":"/srv/projects","hostKey":"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBd875TBQVvl6l2xrvhta9tuJAvo0toGSWCQaQsBRzYd","connect":true}`
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/api/ssh-connections/"+connection.ID, strings.NewReader(body)))
	if response.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", response.Code, response.Body.String())
	}
	runner, ok := server.runnerRegistry.get("ssh-" + connection.ID)
	if !ok || runner != newRunner {
		t.Fatal("edit+reconnect did not register the new runner")
	}
	var status string
	if err := server.db.QueryRow(`select status from ssh_connections where id=?`, connection.ID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "connected" {
		t.Fatalf("status=%q want connected", status)
	}
}

func TestUpdateSwitchingAuthMethodClearsOppositeCredential(t *testing.T) {
	server := newTestServer(t)
	connection := seedSSHConnectionForTest(t, server, "disconnected")
	// 原连接是密码认证，存有密码。
	if _, err := server.db.Exec(`update ssh_connections set password=? where id=?`, "old-password", connection.ID); err != nil {
		t.Fatalf("seed password: %v", err)
	}
	// 编辑切换到密钥认证，填入新私钥路径，不提供密码。
	body := `{"name":"renamed","host":"example.test","user":"dev","privateKeyPath":"/tmp/new-key","authMethod":"key","password":"","rootPath":"/srv/projects","hostKey":"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBd875TBQVvl6l2xrvhta9tuJAvo0toGSWCQaQsBRzYd","connect":false}`
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/api/ssh-connections/"+connection.ID, strings.NewReader(body)))
	if response.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", response.Code, response.Body.String())
	}
	updated, err := server.loadSSHConnection(context.Background(), connection.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if updated.Password != "" {
		t.Fatalf("switching to key auth did not clear old password: %q", updated.Password)
	}
	if updated.PrivateKeyPath != "/tmp/new-key" {
		t.Fatalf("private key path not updated: %q", updated.PrivateKeyPath)
	}
}
