package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMCPOAuthPendingCannotEnableBeforeVerification(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	s.createMCPServer(rec, httptest.NewRequest("POST", "/api/mcp/servers", strings.NewReader(`{"name":"pending","transport":"http","url":"https://example.com/mcp","scope":"global","environments":["windows"],"agents":["claude-code"],"oauthPending":true,"enabled":true,"autoApproveTools":["mcp__pending__*"]}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var server mcpServer
	if err := json.Unmarshal(rec.Body.Bytes(), &server); err != nil {
		t.Fatal(err)
	}
	if server.Enabled || server.Source != mcpSourceOAuthPending || len(server.AutoApproveTools) != 0 {
		t.Fatalf("unsafe draft: %+v", server)
	}
	rec = httptest.NewRecorder()
	s.updateMCPServer(rec, withURLParam(httptest.NewRequest("PATCH", "/", strings.NewReader(`{"enabled":true}`)), "serverID", server.ID))
	if rec.Code != http.StatusConflict {
		t.Fatalf("enable: %d %s", rec.Code, rec.Body.String())
	}
}

func TestMCPOAuthCancelDuringExchangePreservesExistingToken(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	id := insertTestMCPServerURL(t, s, "existing", "global", "", "http", "https://example.com/mcp", []string{"claude-code"})
	flow := &mcpOAuthFlow{ID: "flow", State: "state", ServerID: id, Status: "pending", ExpiresAt: time.Now().Add(time.Minute), ClientID: "client"}
	if err := s.storeMCPOAuthToken(ctx, flow, mcpOAuthTokenResponse{AccessToken: "old-token"}); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-token"}`))
	}))
	defer endpoint.Close()
	flow.Endpoints.TokenEndpoint = endpoint.URL
	s.mu.Lock()
	s.mcpOAuthFlows[flow.State] = flow
	s.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handleMCPOAuthCallback(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/mcp/oauth/callback?state=state&code=code", nil))
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("exchange did not start")
	}
	rec := httptest.NewRecorder()
	s.cancelMCPOAuthFlow(rec, withURLParam(httptest.NewRequest("DELETE", "/", nil), "flowID", flow.ID))
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("callback stuck")
	}
	row, err := s.loadMCPOAuthToken(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.profileSecrets.Load(s.db, ctx, row.AccessTokenRef)
	if err != nil || token != "old-token" {
		t.Fatalf("existing authorization changed: %q %v", token, err)
	}
}

func TestMCPOAuthCleanupOnlyRemovesExpiredDrafts(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	now := time.Now().UTC()
	old := insertTestMCPServerURL(t, s, "expired", "global", "", "http", "https://example.com/mcp", []string{"claude-code"})
	fresh := insertTestMCPServerURL(t, s, "fresh", "global", "", "http", "https://example.com/mcp", []string{"claude-code"})
	saved := insertTestMCPServerURL(t, s, "saved", "global", "", "http", "https://example.com/mcp", []string{"claude-code"})
	for _, id := range []string{old, fresh} {
		if _, err := s.db.ExecContext(ctx, `update mcp_servers set source=?,enabled=0 where id=?`, mcpSourceOAuthPending, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `update mcp_servers set created_at=? where id in (?,?)`, now.Add(-time.Hour), old, saved); err != nil {
		t.Fatal(err)
	}
	flow := &mcpOAuthFlow{ID: "old-flow", State: "old-state", ServerID: old, Status: "pending", ExpiresAt: now.Add(time.Minute)}
	if err := s.storeMCPOAuthToken(ctx, flow, mcpOAuthTokenResponse{AccessToken: "temporary"}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.mcpOAuthFlows[flow.State] = flow
	s.mu.Unlock()
	if err := s.cleanupPendingMCPOAuth(ctx, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.fetchStoredMCPServer(ctx, old); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("draft retained: %v", err)
	}
	if _, err := s.loadMCPOAuthToken(ctx, old); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("token retained: %v", err)
	}
	for _, id := range []string{fresh, saved} {
		if _, err := s.fetchStoredMCPServer(ctx, id); err != nil {
			t.Fatalf("valid server removed: %v", err)
		}
	}
	s.mu.Lock()
	_, exists := s.mcpOAuthFlows[flow.State]
	s.mu.Unlock()
	if exists {
		t.Fatal("expired flow retained")
	}
}
