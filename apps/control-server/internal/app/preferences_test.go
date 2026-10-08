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

func TestAppPreferencesDefaultAndPatch(t *testing.T) {
	server := newTestServer(t)

	initial := httptest.NewRecorder()
	server.routes().ServeHTTP(initial, httptest.NewRequest(http.MethodGet, "/api/preferences", nil))
	if initial.Code != http.StatusOK {
		t.Fatalf("get preferences status=%d body=%s", initial.Code, initial.Body.String())
	}
	var preferences AppPreferences
	if err := json.NewDecoder(initial.Body).Decode(&preferences); err != nil {
		t.Fatalf("decode preferences: %v", err)
	}
	if preferences.DefaultAgentID != defaultAgentID || preferences.ClaudePermissionMode != defaultClaudePermission || preferences.CodexPermissionMode != defaultCodexPermission {
		t.Fatalf("unexpected defaults: %#v", preferences)
	}
	if preferences.AutoReview {
		t.Fatalf("autoReview should default to false, got %#v", preferences)
	}

	patch := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPatch, "/api/preferences", strings.NewReader(`{"defaultAgentId":"codex","codexPermissionMode":"read_only","autoReview":true}`))
	request.Header.Set("Content-Type", "application/json")
	server.routes().ServeHTTP(patch, request)
	if patch.Code != http.StatusOK {
		t.Fatalf("patch preferences status=%d body=%s", patch.Code, patch.Body.String())
	}
	if err := json.NewDecoder(patch.Body).Decode(&preferences); err != nil {
		t.Fatalf("decode patched preferences: %v", err)
	}
	if preferences.DefaultAgentID != "codex" || preferences.ClaudePermissionMode != defaultClaudePermission || preferences.CodexPermissionMode != "read_only" {
		t.Fatalf("unexpected patched preferences: %#v", preferences)
	}
	if !preferences.AutoReview {
		t.Fatalf("autoReview should have been persisted as true, got %#v", preferences)
	}

	var count int
	if err := server.db.QueryRow(`select count(*) from app_preferences where id=1`).Scan(&count); err != nil {
		t.Fatalf("count preference rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("preference row count=%d, want 1", count)
	}
}

// 手机端的 conversation.create 现在会显式带 permissionMode，但"没带"的路径依然存在
// （老版本手机端、或其他调用方）。空权限的派生不能再按 claude/codex 二元式回落 ——
// codebuddy 的权限面不含 approval_required，落到 Claude 的偏好会被 validAgentPolicy
// 拒成 400。新工具的默认权限必须来自目录的 DefaultPermissionMode。
func TestNewConversationDerivesCatalogDefaultPermissionForNewAgents(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,?,'main',1,?)`, t.TempDir(), server.localRunnerID(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects/project/conversations?new=true", strings.NewReader(`{"agentId":"codebuddy"}`)))
	if response.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
	}
	var conversation Conversation
	if err := json.NewDecoder(response.Body).Decode(&conversation); err != nil {
		t.Fatalf("decode conversation: %v", err)
	}
	if conversation.AgentID != "codebuddy" || conversation.PermissionMode != "workspace_write" {
		t.Fatalf("conversation defaults=%+v want codebuddy/workspace_write", conversation)
	}
}

func TestNewConversationUsesApplicationDefaultsWhenRequestOmitsThem(t *testing.T) {
	server := newTestServer(t)
	server.codexRunner = runnerFunc(func(context.Context, AgentRunRequest, AgentRunSink) error { return nil })
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`update app_preferences set default_agent_id='codex',codex_permission_mode='read_only' where id=1`); err != nil {
		t.Fatalf("set app preferences: %v", err)
	}

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects/project/conversations?new=true", nil))
	if response.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
	}
	var conversation Conversation
	if err := json.NewDecoder(response.Body).Decode(&conversation); err != nil {
		t.Fatalf("decode conversation: %v", err)
	}
	if conversation.AgentID != "codex" || conversation.PermissionMode != "read_only" {
		t.Fatalf("conversation defaults=%+v", conversation)
	}
}

func TestAppPreferencesRejectInvalidAgentPermissionCombination(t *testing.T) {
	server := newTestServer(t)

	for _, body := range []string{
		`{"defaultAgentId":"unknown"}`,
		`{"claudePermissionMode":"workspace_write"}`,
		`{"codexPermissionMode":"approval_required"}`,
		`{}`,
	} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPatch, "/api/preferences", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		server.routes().ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("body=%s status=%d want %d body=%s", body, response.Code, http.StatusBadRequest, response.Body.String())
		}
	}
}

func TestAppPreferencesMissingSingletonFallsBackToSafeDefaults(t *testing.T) {
	server := newTestServer(t)
	if _, err := server.db.Exec(`delete from app_preferences where id=1`); err != nil {
		t.Fatalf("delete preference singleton: %v", err)
	}

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/preferences", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("get preferences status=%d body=%s", response.Code, response.Body.String())
	}
	var preferences AppPreferences
	if err := json.NewDecoder(response.Body).Decode(&preferences); err != nil {
		t.Fatalf("decode fallback preferences: %v", err)
	}
	if preferences.DefaultAgentID != defaultAgentID || preferences.ClaudePermissionMode != defaultClaudePermission || preferences.CodexPermissionMode != defaultCodexPermission {
		t.Fatalf("unexpected fallback defaults: %#v", preferences)
	}

	patch := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPatch, "/api/preferences", strings.NewReader(`{"defaultAgentId":"codex"}`))
	request.Header.Set("Content-Type", "application/json")
	server.routes().ServeHTTP(patch, request)
	if patch.Code != http.StatusOK {
		t.Fatalf("patch missing singleton status=%d body=%s", patch.Code, patch.Body.String())
	}
}
