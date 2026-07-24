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
	if _, err := server.db.Exec(`insert into ssh_connections (id,name,host,port,user,private_key_path,known_hosts,root_path,status,error_msg,created_at,updated_at) values (?,?,?,?,?,?,?,?,?,?,?,?)`, connection.ID, connection.Name, connection.Host, connection.Port, connection.User, connection.PrivateKeyPath, connection.KnownHosts, connection.RootPath, connection.Status, "", connection.CreatedAt, connection.UpdatedAt); err != nil {
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
	if _, err := server.db.Exec(`insert into ssh_connections (id,name,host,port,user,private_key_path,known_hosts,root_path,status,created_at,updated_at) values ('connection','connection','host',22,'user','key','ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest','/workspace','connected',?,?)`, now, now); err != nil {
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
