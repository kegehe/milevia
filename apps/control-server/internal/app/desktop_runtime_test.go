package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func newDesktopTestServer(t *testing.T, config Config) *Server {
	t.Helper()
	server, err := NewWithRunner(context.Background(), config, runnerFunc(func(context.Context, AgentRunRequest, AgentRunSink) error {
		return nil
	}))
	if err != nil {
		t.Fatalf("create desktop test server: %v", err)
	}
	t.Cleanup(server.Close)
	return server
}

func TestDataDirectoryLockPreventsConcurrentServer(t *testing.T) {
	dataDir := t.TempDir()
	config := Config{DataDir: dataDir, AllowedRoot: dataDir}
	server := newDesktopTestServer(t, config)
	var kind string
	localRunnerID := server.localRunnerID()
	if err := server.db.QueryRow(`select kind from runners where id=?`, localRunnerID).Scan(&kind); err != nil {
		t.Fatalf("load persisted local runner: %v", err)
	}
	expectedKind := "wsl"
	if runtime.GOOS == "windows" {
		expectedKind = "windows-local"
	}
	if kind != expectedKind {
		t.Fatalf("persisted local runner kind: got %q want %q", kind, expectedKind)
	}

	_, err := NewWithRunner(context.Background(), config, runnerFunc(func(context.Context, AgentRunRequest, AgentRunSink) error {
		return nil
	}))
	if err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("expected data directory lock error, got %v", err)
	}

	server.Close()
	reopened := newDesktopTestServer(t, config)
	if reopened.config.DatabasePath != filepath.Join(dataDir, "data", "milevia.db") {
		t.Fatalf("unexpected data directory database path: %s", reopened.config.DatabasePath)
	}
	if err := reopened.dataLock.writeState("pid=1\n"); err != nil {
		t.Fatalf("write data directory lock state: %v", err)
	}
}

func TestDesktopSessionProtectsHTTPAndWebSocket(t *testing.T) {
	const token = "test-desktop-session-token"
	server := newDesktopTestServer(t, Config{
		DatabasePath:   filepath.Join(t.TempDir(), "milevia.db"),
		AllowedRoot:    t.TempDir(),
		Mode:           "desktop-api",
		SessionToken:   token,
		AllowedOrigins: []string{"https://tauri.localhost"},
	})

	health := httptest.NewRecorder()
	server.routes().ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health status: got %d want %d", health.Code, http.StatusOK)
	}

	unauthorized := httptest.NewRecorder()
	server.routes().ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/projects", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("missing session status: got %d want %d", unauthorized.Code, http.StatusUnauthorized)
	}

	authorizedRequest := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	authorizedRequest.Header.Set("X-Milevia-Session", token)
	authorizedRequest.Header.Set("Origin", "https://tauri.localhost")
	authorized := httptest.NewRecorder()
	server.routes().ServeHTTP(authorized, authorizedRequest)
	if authorized.Code != http.StatusOK {
		t.Fatalf("authenticated request status: got %d body=%s", authorized.Code, authorized.Body.String())
	}
	if got := authorized.Header().Get("Access-Control-Allow-Origin"); got != "https://tauri.localhost" {
		t.Fatalf("allowed origin header: got %q", got)
	}

	approvalRequest := httptest.NewRequest(http.MethodPost, "/api/internal/approvals/wait", strings.NewReader(`{"tool_name":"Bash","tool_input":{"command":"pwd"}}`))
	approvalRequest.Header.Set("X-Auto-Run-ID", "missing-run")
	approvalRequest.Header.Set("X-Auto-Approval-Token", "one-time-approval-token")
	approval := httptest.NewRecorder()
	server.routes().ServeHTTP(approval, approvalRequest)
	if approval.Code != http.StatusUnauthorized || strings.Contains(approval.Body.String(), "invalid desktop session") {
		t.Fatalf("approval hook was blocked by desktop session middleware: status=%d body=%s", approval.Code, approval.Body.String())
	}

	foreignOriginRequest := httptest.NewRequest(http.MethodOptions, "/api/projects", nil)
	foreignOriginRequest.Header.Set("Origin", "https://untrusted.example")
	foreignOrigin := httptest.NewRecorder()
	server.routes().ServeHTTP(foreignOrigin, foreignOriginRequest)
	if got := foreignOrigin.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("foreign origin was allowed: %q", got)
	}

	httpServer := httptest.NewServer(server.routes())
	t.Cleanup(httpServer.Close)
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/notifications"
	dialer := websocket.Dialer{Subprotocols: []string{server.websocketSessionProtocol()}}
	connection, response, err := dialer.Dial(wsURL, http.Header{"Origin": []string{"https://tauri.localhost"}})
	if err != nil {
		if response != nil {
			t.Fatalf("dial authenticated WebSocket: %v (status %d)", err, response.StatusCode)
		}
		t.Fatalf("dial authenticated WebSocket: %v", err)
	}
	defer connection.Close()
	if connection.Subprotocol() != server.websocketSessionProtocol() {
		t.Fatalf("unexpected WebSocket protocol: %q", connection.Subprotocol())
	}
}

func TestDesktopDownloadTicketAllowsOnlyItsBoundFile(t *testing.T) {
	const token = "test-download-ticket-session"
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "archive.bin"), []byte{0, 1, 2, 255}, 0o644); err != nil {
		t.Fatalf("write download fixture: %v", err)
	}
	server := newDesktopTestServer(t, Config{
		DatabasePath: filepath.Join(t.TempDir(), "milevia.db"),
		AllowedRoot:  root,
		Mode:         "desktop-api",
		SessionToken: token,
	})
	addFilesystemTestProject(t, server, root)

	issue := httptest.NewRecorder()
	issueRequest := httptest.NewRequest(http.MethodPost, "/api/projects/files/fs/download-ticket", strings.NewReader(`{"path":"archive.bin"}`))
	issueRequest.Header.Set("X-Milevia-Session", token)
	server.routes().ServeHTTP(issue, issueRequest)
	if issue.Code != http.StatusOK {
		t.Fatalf("issue download ticket: status=%d body=%s", issue.Code, issue.Body.String())
	}
	var result struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(issue.Body).Decode(&result); err != nil || result.URL == "" {
		t.Fatalf("decode download ticket: url=%q err=%v", result.URL, err)
	}

	download := httptest.NewRecorder()
	server.routes().ServeHTTP(download, httptest.NewRequest(http.MethodGet, result.URL, nil))
	if download.Code != http.StatusOK || string(download.Body.Bytes()) != string([]byte{0, 1, 2, 255}) {
		t.Fatalf("ticket download: status=%d body=%v", download.Code, download.Body.Bytes())
	}
	if download.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("ticket download cache policy: %q", download.Header().Get("Cache-Control"))
	}

	boundElsewhere := httptest.NewRecorder()
	server.routes().ServeHTTP(boundElsewhere, httptest.NewRequest(http.MethodGet, strings.Replace(result.URL, "archive.bin", "other.bin", 1), nil))
	if boundElsewhere.Code != http.StatusUnauthorized {
		t.Fatalf("ticket was accepted for a different path: status=%d", boundElsewhere.Code)
	}
}

func TestNotificationSubscriptionClosesWithGoingAway(t *testing.T) {
	server := newTestServer(t)
	httpServer := httptest.NewServer(server.routes())
	t.Cleanup(httpServer.Close)

	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/notifications"
	connection, response, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Origin": []string{httpServer.URL}})
	if err != nil {
		if response != nil {
			t.Fatalf("dial notification WebSocket: %v (status %d)", err, response.StatusCode)
		}
		t.Fatalf("dial notification WebSocket: %v", err)
	}
	defer connection.Close()

	deadline := time.Now().Add(time.Second)
	for {
		server.notificationSubMu.Lock()
		registered := len(server.notificationSubs) == 1
		server.notificationSubMu.Unlock()
		if registered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("notification WebSocket was not registered")
		}
		time.Sleep(10 * time.Millisecond)
	}

	server.closeAllNotificationSubscribers()
	if err := connection.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	_, _, err = connection.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("expected WebSocket close error, got %v", err)
	}
	if closeErr.Code != websocket.CloseGoingAway {
		t.Fatalf("close code: got %d want %d", closeErr.Code, websocket.CloseGoingAway)
	}

	handlersDone := make(chan struct{})
	go func() {
		server.websocketWG.Wait()
		close(handlersDone)
	}()
	select {
	case <-handlersDone:
	case <-time.After(time.Second):
		t.Fatal("notification WebSocket handler did not exit after close handshake")
	}
}

func TestNotificationSubscriptionClosesSlowClientWithTryAgainLater(t *testing.T) {
	server := newTestServer(t)
	httpServer := httptest.NewServer(server.routes())
	t.Cleanup(httpServer.Close)

	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/notifications"
	connection, response, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Origin": []string{httpServer.URL}})
	if err != nil {
		if response != nil {
			t.Fatalf("dial notification WebSocket: %v (status %d)", err, response.StatusCode)
		}
		t.Fatalf("dial notification WebSocket: %v", err)
	}
	defer connection.Close()

	var sub *notificationSubscriber
	deadline := time.Now().Add(time.Second)
	for sub == nil {
		server.notificationSubMu.Lock()
		for _, candidate := range server.notificationSubs {
			sub = candidate
		}
		server.notificationSubMu.Unlock()
		if sub != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("notification WebSocket was not registered")
		}
		time.Sleep(10 * time.Millisecond)
	}

	go sub.closeWithStatus(websocket.CloseTryAgainLater, "client is too slow")
	if err := connection.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	_, _, err = connection.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("expected WebSocket close error, got %v", err)
	}
	if closeErr.Code != websocket.CloseTryAgainLater {
		t.Fatalf("close code: got %d want %d", closeErr.Code, websocket.CloseTryAgainLater)
	}

	handlersDone := make(chan struct{})
	go func() {
		server.websocketWG.Wait()
		close(handlersDone)
	}()
	select {
	case <-handlersDone:
	case <-time.After(time.Second):
		t.Fatal("slow-client WebSocket handler did not exit after close handshake")
	}
}

func TestWebSocketSubscriptionOverridesHTTPReadTimeout(t *testing.T) {
	server := newTestServer(t)
	httpServer := httptest.NewUnstartedServer(server.routes())
	httpServer.Config.ReadTimeout = 50 * time.Millisecond
	httpServer.Start()
	t.Cleanup(httpServer.Close)

	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/notifications"
	connection, response, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Origin": []string{httpServer.URL}})
	if err != nil {
		if response != nil {
			t.Fatalf("dial notification WebSocket: %v (status %d)", err, response.StatusCode)
		}
		t.Fatalf("dial notification WebSocket: %v", err)
	}
	defer connection.Close()

	var sub *notificationSubscriber
	deadline := time.Now().Add(time.Second)
	for sub == nil {
		server.notificationSubMu.Lock()
		for _, candidate := range server.notificationSubs {
			sub = candidate
		}
		server.notificationSubMu.Unlock()
		if sub != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("notification WebSocket was not registered")
		}
		time.Sleep(10 * time.Millisecond)
	}

	time.Sleep(100 * time.Millisecond)
	sub.send <- []byte(`{"type":"after-http-read-timeout"}`)
	if err := connection.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	_, data, err := connection.ReadMessage()
	if err != nil {
		t.Fatalf("read notification after HTTP timeout: %v", err)
	}
	if string(data) != `{"type":"after-http-read-timeout"}` {
		t.Fatalf("notification payload: got %q", data)
	}
}

func TestRemovingConversationSubscriberClosesConnection(t *testing.T) {
	server := newTestServer(t)
	httpServer := httptest.NewServer(server.routes())
	t.Cleanup(httpServer.Close)

	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/conversations/conversation"
	connection, response, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Origin": []string{httpServer.URL}})
	if err != nil {
		if response != nil {
			t.Fatalf("dial conversation WebSocket: %v (status %d)", err, response.StatusCode)
		}
		t.Fatalf("dial conversation WebSocket: %v", err)
	}
	defer connection.Close()

	var sub *subscriber
	deadline := time.Now().Add(time.Second)
	for sub == nil {
		server.mu.Lock()
		for _, candidate := range server.subscribers["conversation"] {
			sub = candidate
		}
		server.mu.Unlock()
		if sub != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("conversation WebSocket was not registered")
		}
		time.Sleep(10 * time.Millisecond)
	}

	server.removeConversationSubscriber("conversation", sub)
	if err := connection.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if _, _, err := connection.ReadMessage(); err == nil {
		t.Fatal("conversation WebSocket remained open after subscriber removal")
	}

	handlersDone := make(chan struct{})
	go func() {
		server.websocketWG.Wait()
		close(handlersDone)
	}()
	select {
	case <-handlersDone:
	case <-time.After(time.Second):
		t.Fatal("conversation WebSocket handler did not exit after subscriber removal")
	}
}

func TestShutdownRejectsNewWebSocketSubscriptions(t *testing.T) {
	server := newTestServer(t)
	httpServer := httptest.NewServer(server.routes())
	t.Cleanup(httpServer.Close)
	server.Close()

	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/notifications"
	_, response, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Origin": []string{httpServer.URL}})
	if err == nil {
		t.Fatal("expected shutdown WebSocket subscription to be rejected")
	}
	if response == nil || response.StatusCode != http.StatusServiceUnavailable {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Fatalf("shutdown WebSocket status: got %d want %d (error %v)", status, http.StatusServiceUnavailable, err)
	}
}

func TestClosingServerRejectsPendingWebSocketRegistrations(t *testing.T) {
	server := newTestServer(t)
	server.mu.Lock()
	server.closing = true
	server.mu.Unlock()

	conversation := &subscriber{send: make(chan []byte, 1)}
	if server.addConversationSubscriber("conversation", conversation) {
		t.Fatal("closing server registered conversation WebSocket")
	}
	notification := &notificationSubscriber{send: make(chan []byte, 1)}
	if server.addNotificationSubscriber(nil, notification) {
		t.Fatal("closing server registered notification WebSocket")
	}
	runLog := &runLogSubscriber{send: make(chan LogEntry, 1)}
	if server.addRunLogSubscriber("project", runLog, nil) {
		t.Fatal("closing server registered run-log WebSocket")
	}
}

func TestWebModeServesAssetsAndBrowserRoutes(t *testing.T) {
	webRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(webRoot, "assets"), 0o755); err != nil {
		t.Fatalf("create assets directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(webRoot, "index.html"), []byte("<main>Milevia</main>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(webRoot, "assets", "app.js"), []byte("console.log('milevia')"), 0o644); err != nil {
		t.Fatalf("write asset: %v", err)
	}
	server := newDesktopTestServer(t, Config{
		DatabasePath: filepath.Join(t.TempDir(), "milevia.db"),
		AllowedRoot:  t.TempDir(),
		Mode:         "web",
		WebRoot:      webRoot,
	})

	for _, path := range []string{"/projects/project-1/conversations", "/assets/app.js"} {
		response := httptest.NewRecorder()
		server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s: got %d body=%s", path, response.Code, response.Body.String())
		}
	}
	for _, path := range []string{"/api/does-not-exist", "/ws/does-not-exist"} {
		response := httptest.NewRecorder()
		server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusNotFound {
			t.Fatalf("GET %s: got %d want %d", path, response.Code, http.StatusNotFound)
		}
	}
	shutdown := httptest.NewRecorder()
	server.routes().ServeHTTP(shutdown, httptest.NewRequest(http.MethodPost, "/api/internal/shutdown", nil))
	if shutdown.Code != http.StatusNotFound {
		t.Fatalf("web shutdown endpoint status: got %d want %d", shutdown.Code, http.StatusNotFound)
	}
}
