package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

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
