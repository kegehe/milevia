package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func insertFixTestProject(t *testing.T, s *Server, projectID string) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(), `insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values (?,?,?,?,?,?,?)`,
		projectID, "P", t.TempDir(), s.localRunnerID(), "main", true, time.Now().UTC()); err != nil {
		t.Fatalf("insert project: %v", err)
	}
}

func TestNormalizeAutoApproveForServer(t *testing.T) {
	if _, err := normalizeAutoApproveForServer([]string{"mcp__*"}, "github"); err == nil {
		t.Fatal("无锚点的 mcp__* 必须被拒")
	}
	if _, err := normalizeAutoApproveForServer([]string{"mcp__other__*"}, "github"); err == nil {
		t.Fatal("不属于本 server 的模式必须被拒")
	}
	got, err := normalizeAutoApproveForServer([]string{" mcp__github__* ", "Bash(git status*)"}, "github")
	if err != nil {
		t.Fatalf("合法模式被拒: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("normalize = %v", got)
	}
}

func TestBuildStoredMapsAutoEncryptsSecretLookingValues(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	env, headers, created, err := s.buildStoredMaps(ctx, s.db,
		map[string]string{"GITHUB_TOKEN": "ghp_plain", "LOG_LEVEL": "info"},
		map[string]string{"Authorization": "Bearer plain-token"},
		nil, nil)
	if err != nil {
		t.Fatalf("buildStoredMaps: %v", err)
	}
	if !strings.HasPrefix(env["GITHUB_TOKEN"], mcpSecretRefPrefix) {
		t.Fatalf("GITHUB_TOKEN 应加密存储，实际 %q", env["GITHUB_TOKEN"])
	}
	if env["LOG_LEVEL"] != "info" {
		t.Fatalf("普通值不应被改动: %q", env["LOG_LEVEL"])
	}
	if !strings.HasPrefix(headers["Authorization"], mcpSecretRefPrefix) {
		t.Fatalf("Authorization 应加密存储，实际 %q", headers["Authorization"])
	}
	if len(created) != 2 {
		t.Fatalf("created refs = %v", created)
	}
	// 已是 sec_ 引用的值不能二次加密。
	env2, _, _, err := s.buildStoredMaps(ctx, s.db, map[string]string{"GITHUB_TOKEN": env["GITHUB_TOKEN"]}, nil, nil, nil)
	if err != nil {
		t.Fatalf("buildStoredMaps second: %v", err)
	}
	if env2["GITHUB_TOKEN"] != env["GITHUB_TOKEN"] {
		t.Fatalf("sec_ 引用被改写: %q", env2["GITHUB_TOKEN"])
	}
}

func TestUpdateMCPServerKeepsDescriptionAndAllowsScopeChange(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	projectID := "proj-patch"
	insertFixTestProject(t, s, projectID)
	serverID := insertTestMCPServer(t, s, "patchdemo", mcpScopeProject, projectID, mcpTransportStdio, "npx",
		map[string]string{"LOG_LEVEL": "info"}, []string{"windows"}, []string{"claude-code"})

	// 先写入说明。
	rec := httptest.NewRecorder()
	s.updateMCPServer(rec, withURLParam(httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(`{"description":"保留我"}`)), "serverID", serverID))
	if rec.Code != http.StatusOK {
		t.Fatalf("set description status = %d: %s", rec.Code, rec.Body.String())
	}
	// 卡片开关只发 enabled：说明不能被空串覆盖。
	rec = httptest.NewRecorder()
	s.updateMCPServer(rec, withURLParam(httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(`{"enabled":false}`)), "serverID", serverID))
	if rec.Code != http.StatusOK {
		t.Fatalf("toggle status = %d: %s", rec.Code, rec.Body.String())
	}
	stored, err := s.fetchStoredMCPServer(ctx, serverID)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if stored.Description != "保留我" {
		t.Fatalf("description 被清空: %q", stored.Description)
	}
	if stored.Enabled {
		t.Fatal("enabled 应为 false")
	}

	// 项目级改全局：projectId:"" 是显式清空，不能再被 validate 拒掉。
	rec = httptest.NewRecorder()
	s.updateMCPServer(rec, withURLParam(httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(`{"scope":"global","projectId":""}`)), "serverID", serverID))
	if rec.Code != http.StatusOK {
		t.Fatalf("scope change status = %d: %s", rec.Code, rec.Body.String())
	}
	stored, err = s.fetchStoredMCPServer(ctx, serverID)
	if err != nil {
		t.Fatalf("fetch after scope: %v", err)
	}
	if stored.Scope != mcpScopeGlobal || stored.ProjectID != "" {
		t.Fatalf("scope=%q project=%q", stored.Scope, stored.ProjectID)
	}
}

func TestProjectBindingDisablesProjectScopedServer(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	projectID := "proj-binding"
	insertFixTestProject(t, s, projectID)
	serverID := insertTestMCPServer(t, s, "projectonly", mcpScopeProject, projectID, mcpTransportStdio, "npx", nil,
		[]string{"windows"}, []string{"claude-code"})
	if _, err := s.db.ExecContext(ctx, `insert into mcp_project_bindings (project_id,server_id,enabled,created_at,updated_at) values (?,?,0,?,?)`,
		projectID, serverID, time.Now().UTC(), time.Now().UTC()); err != nil {
		t.Fatalf("insert binding: %v", err)
	}

	selected, err := s.selectMCPServers(ctx, projectID, "claude-code", agentTargetEnvWindows)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(selected) != 0 {
		t.Fatalf("被项目关闭的项目级 server 仍被选中: %+v", selected)
	}
	views, err := s.listProjectMCPBindings(ctx, projectID)
	if err != nil {
		t.Fatalf("bindings: %v", err)
	}
	if len(views) != 1 || views[0].Enabled || !views[0].Overridden {
		t.Fatalf("binding view = %+v", views)
	}
}

func TestCodexInjectionSkipsConflictingEnvAndDangerousKeys(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	projectID := "proj-codex-env"
	projectPath := t.TempDir()
	insertFixTestProject(t, s, projectID)
	insertTestMCPServer(t, s, "alpha", mcpScopeGlobal, "", mcpTransportStdio, "npx",
		map[string]string{"GITHUB_TOKEN": "first", "PATH": "/evil"}, []string{"windows"}, []string{"codex"})
	insertTestMCPServer(t, s, "beta", mcpScopeGlobal, "", mcpTransportStdio, "npx",
		map[string]string{"GITHUB_TOKEN": "second"}, []string{"windows"}, []string{"codex"})

	servers, err := s.selectMCPServers(ctx, projectID, "codex", agentTargetEnvWindows)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	injection, injected := s.buildCodexInjection(ctx, servers, agentTargetEnvWindows, projectPath)
	if len(injected) != 1 || injected[0].Name != "alpha" {
		t.Fatalf("同名 env 冲突时应只注入第一个 server，实际 %+v", injected)
	}
	if !strings.Contains(injection.Note, "已跳过该 server") {
		t.Fatalf("note 未说明跳过：%q", injection.Note)
	}
	if !strings.Contains(injection.Note, "PATH") {
		t.Fatalf("note 未说明被忽略的危险键：%q", injection.Note)
	}
	for _, pair := range injection.Env {
		if strings.HasPrefix(pair, "PATH=") {
			t.Fatalf("PATH 不应进入 Codex 进程环境: %v", injection.Env)
		}
		if strings.HasPrefix(pair, "GITHUB_TOKEN=") && !strings.HasSuffix(pair, "first") {
			t.Fatalf("GITHUB_TOKEN 取值错误: %v", injection.Env)
		}
	}
}

func TestSummarizeMCPToolInputRedactsNested(t *testing.T) {
	raw := json.RawMessage(`{"headers":{"Authorization":"Bearer ghp_secret"},"connection":"postgres://u:p@host/db","safe":"ok"}`)
	got := summarizeMCPToolInput(raw)
	if strings.Contains(got, "ghp_secret") {
		t.Fatalf("嵌套密钥未脱敏: %s", got)
	}
	if strings.Contains(got, "u:p@") {
		t.Fatalf("URL userinfo 未脱敏: %s", got)
	}
	if !strings.Contains(got, `"safe":"ok"`) {
		t.Fatalf("普通值被误删: %s", got)
	}
}

func TestDeleteProjectCleansMCPData(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	projectID := "proj-del-mcp"
	insertFixTestProject(t, s, projectID)
	secretRef, err := s.profileSecrets.Store(s.db, ctx, "project-token")
	if err != nil {
		t.Fatalf("store secret: %v", err)
	}
	oauthRef, err := s.profileSecrets.Store(s.db, ctx, "oauth-token")
	if err != nil {
		t.Fatalf("store oauth secret: %v", err)
	}
	serverID := insertTestMCPServer(t, s, "projectmcp", mcpScopeProject, projectID, mcpTransportStdio, "npx",
		map[string]string{"GITHUB_TOKEN": secretRef}, []string{"windows"}, []string{"claude-code"})
	globalID := insertTestMCPServer(t, s, "globalmcp", mcpScopeGlobal, "", mcpTransportStdio, "npx",
		nil, []string{"windows"}, []string{"claude-code"})
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx, `insert into mcp_project_bindings (project_id,server_id,enabled,created_at,updated_at) values (?,?,0,?,?)`,
		projectID, globalID, now, now); err != nil {
		t.Fatalf("insert binding: %v", err)
	}
	if err := s.setProjectMCPAllowMcpJson(ctx, projectID, true); err != nil {
		t.Fatalf("set project settings: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `insert into mcp_oauth_tokens (server_id,access_token_ref,created_at,updated_at) values (?,?,?,?)`,
		serverID, oauthRef, now, now); err != nil {
		t.Fatalf("insert oauth token: %v", err)
	}

	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/projects/"+projectID, nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete project status = %d: %s", rec.Code, rec.Body.String())
	}

	count := func(query string, args ...any) int {
		var value int
		if err := s.db.QueryRowContext(ctx, query, args...).Scan(&value); err != nil {
			t.Fatalf("count %q: %v", query, err)
		}
		return value
	}
	if got := count(`select count(*) from mcp_servers where id=?`, serverID); got != 0 {
		t.Fatalf("项目级 MCP server 未删除: %d", got)
	}
	if got := count(`select count(*) from mcp_servers where id=?`, globalID); got != 1 {
		t.Fatalf("全局 MCP server 被误删: %d", got)
	}
	if got := count(`select count(*) from mcp_project_bindings where project_id=?`, projectID); got != 0 {
		t.Fatalf("项目绑定未清理: %d", got)
	}
	if got := count(`select count(*) from mcp_project_settings where project_id=?`, projectID); got != 0 {
		t.Fatalf("项目 MCP 设置未清理: %d", got)
	}
	if got := count(`select count(*) from mcp_oauth_tokens where server_id=?`, serverID); got != 0 {
		t.Fatalf("OAuth 令牌未清理: %d", got)
	}
	if plain, err := s.profileSecrets.Load(s.db, ctx, secretRef); err == nil && plain != "" {
		t.Fatalf("项目级 MCP 密文未回收: %q", plain)
	}
	if plain, err := s.profileSecrets.Load(s.db, ctx, oauthRef); err == nil && plain != "" {
		t.Fatalf("项目级 OAuth 密文未回收: %q", plain)
	}
}

func TestUpdateMCPServerRewritesAutoApproveOnRename(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	serverID := insertTestMCPServer(t, s, "alpha", mcpScopeGlobal, "", mcpTransportStdio, "npx", nil,
		[]string{"windows"}, []string{"claude-code"})
	if _, err := s.db.ExecContext(ctx, `update mcp_servers set auto_approve_tools=? where id=?`, `["mcp__alpha__*"]`, serverID); err != nil {
		t.Fatalf("seed auto approve: %v", err)
	}
	rec := httptest.NewRecorder()
	s.updateMCPServer(rec, withURLParam(httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(`{"name":"beta"}`)), "serverID", serverID))
	if rec.Code != http.StatusOK {
		t.Fatalf("rename status = %d: %s", rec.Code, rec.Body.String())
	}
	stored, err := s.fetchStoredMCPServer(ctx, serverID)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !slices.Equal(stored.AutoApproveTools, []string{"mcp__beta__*"}) {
		t.Fatalf("白名单未随改名重写: %v", stored.AutoApproveTools)
	}
}

func TestUpdateMCPServerToleratesUnchangedLegacyAutoApprove(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	serverID := insertTestMCPServer(t, s, "legacy", mcpScopeGlobal, "", mcpTransportStdio, "npx", nil,
		[]string{"windows"}, []string{"claude-code"})
	if _, err := s.db.ExecContext(ctx, `update mcp_servers set auto_approve_tools=? where id=?`, `["mcp__*"]`, serverID); err != nil {
		t.Fatalf("seed legacy auto approve: %v", err)
	}
	// 卡片开关只发 enabled：不应因历史非法值被拒。
	rec := httptest.NewRecorder()
	s.updateMCPServer(rec, withURLParam(httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(`{"enabled":false}`)), "serverID", serverID))
	if rec.Code != http.StatusOK {
		t.Fatalf("toggle status = %d: %s", rec.Code, rec.Body.String())
	}
	// 整表保存原样回传历史非法值：仍应放行并保留。
	rec = httptest.NewRecorder()
	s.updateMCPServer(rec, withURLParam(httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(`{"autoApproveTools":["mcp__*"]}`)), "serverID", serverID))
	if rec.Code != http.StatusOK {
		t.Fatalf("resend legacy status = %d: %s", rec.Code, rec.Body.String())
	}
	stored, err := s.fetchStoredMCPServer(ctx, serverID)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !slices.Equal(stored.AutoApproveTools, []string{"mcp__*"}) {
		t.Fatalf("历史值被改动: %v", stored.AutoApproveTools)
	}
	// 新增一条合法模式：历史 mcp__* 继续保留，新条目通过校验。
	rec = httptest.NewRecorder()
	s.updateMCPServer(rec, withURLParam(httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(`{"autoApproveTools":["mcp__*","mcp__legacy__tool"]}`)), "serverID", serverID))
	if rec.Code != http.StatusOK {
		t.Fatalf("add valid pattern status = %d: %s", rec.Code, rec.Body.String())
	}
	// 新增一条不属于本 server 的模式：必须拒绝。
	rec = httptest.NewRecorder()
	s.updateMCPServer(rec, withURLParam(httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(`{"autoApproveTools":["mcp__*","mcp__other__tool"]}`)), "serverID", serverID))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("add foreign pattern status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestCodexInjectionDetectsCaseInsensitiveEnvConflict(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	projectID := "proj-codex-case"
	insertFixTestProject(t, s, projectID)
	insertTestMCPServer(t, s, "alpha", mcpScopeGlobal, "", mcpTransportStdio, "npx",
		map[string]string{"Token": "first"}, []string{"windows"}, []string{"codex"})
	insertTestMCPServer(t, s, "beta", mcpScopeGlobal, "", mcpTransportStdio, "npx",
		map[string]string{"TOKEN": "second"}, []string{"windows"}, []string{"codex"})
	servers, err := s.selectMCPServers(ctx, projectID, "codex", agentTargetEnvWindows)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	injection, injected := s.buildCodexInjection(ctx, servers, agentTargetEnvWindows, t.TempDir())
	if len(injected) != 1 {
		t.Fatalf("大小写不同的同名变量应按冲突处理，实际注入 %+v", injected)
	}
	if !strings.Contains(injection.Note, "已跳过该 server") {
		t.Fatalf("note 未说明跳过: %q", injection.Note)
	}
}

func TestRewriteAutoApproveServerPrefix(t *testing.T) {
	got := rewriteAutoApproveServerPrefix([]string{"mcp__old__*", "mcp__old__tool", "Bash(git status*)"}, "old", "new")
	want := []string{"mcp__new__*", "mcp__new__tool", "Bash(git status*)"}
	if !slices.Equal(got, want) {
		t.Fatalf("rewrite = %v, want %v", got, want)
	}
}

func TestSSHAgentSessionCleanupMCPOnce(t *testing.T) {
	calls := 0
	session := &sshAgentSession{mcpCleanup: func() { calls++ }}
	session.cleanupMCP()
	session.cleanupMCP()
	if calls != 1 {
		t.Fatalf("cleanup 执行了 %d 次，want 1", calls)
	}
}

func TestMCPCapabilityBackoffLooksUnsupported(t *testing.T) {
	runner := &sshRunner{mcpFailureUntil: time.Now().Add(time.Minute)}
	checked, injectable, _ := runner.mcpCapabilityCached()
	if !checked || injectable {
		t.Fatalf("退避窗口内应报告已知不可注入: checked=%v injectable=%v", checked, injectable)
	}
}

func TestMCPOAuthAuthorizedDoesNotReenterLifecycleLock(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	ref, err := s.profileSecrets.Store(s.db, ctx, "oauth-access")
	if err != nil {
		t.Fatalf("store token: %v", err)
	}
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx, `insert into mcp_oauth_tokens (server_id,access_token_ref,created_at,updated_at) values (?,?,?,?)`,
		"oauth-server", ref, now, now); err != nil {
		t.Fatalf("insert token: %v", err)
	}
	// 模拟 testMCPServer 的 oauth-pending 分支：持锁后再做授权检查。
	// 若这里误用会触发刷新的 mcpOAuthAuthorizationHeader，会在这把非可重入锁上挂死。
	s.mcpOAuthLifecycleMu.Lock()
	defer s.mcpOAuthLifecycleMu.Unlock()
	if !s.mcpOAuthAuthorized(ctx, "oauth-server") {
		t.Fatal("有效令牌应判定为已授权")
	}
}

func TestUpdateMCPServerRemovesSavedSecret(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	ref, err := s.profileSecrets.Store(s.db, ctx, "saved-token")
	if err != nil {
		t.Fatalf("store secret: %v", err)
	}
	serverID := insertTestMCPServer(t, s, "removeme", mcpScopeGlobal, "", mcpTransportStdio, "npx",
		map[string]string{"GITHUB_TOKEN": ref, "LOG_LEVEL": "info"}, []string{"windows"}, []string{"claude-code"})
	rec := httptest.NewRecorder()
	s.updateMCPServer(rec, withURLParam(httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(`{"removedEnvSecrets":["GITHUB_TOKEN"]}`)), "serverID", serverID))
	if rec.Code != http.StatusOK {
		t.Fatalf("remove secret status = %d: %s", rec.Code, rec.Body.String())
	}
	stored, err := s.fetchStoredMCPServer(ctx, serverID)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if _, exists := stored.envRaw["GITHUB_TOKEN"]; exists {
		t.Fatalf("凭据键未删除: %v", stored.envRaw)
	}
	if stored.envRaw["LOG_LEVEL"] != "info" {
		t.Fatalf("普通值被误删: %v", stored.envRaw)
	}
	if plain, err := s.profileSecrets.Load(s.db, ctx, ref); err == nil && plain != "" {
		t.Fatalf("被移除的密文未回收: %q", plain)
	}
}

func TestProjectDeleteAndMCPUpdateDoNotDeadlock(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	projectID := "proj-concurrent"
	insertFixTestProject(t, s, projectID)
	serverID := insertTestMCPServer(t, s, "concurrent", mcpScopeProject, projectID, mcpTransportStdio, "npx", nil,
		[]string{"windows"}, []string{"claude-code"})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _, _, _ = s.deleteProjectLocked(ctx, projectID)
	}()
	go func() {
		defer wg.Done()
		rec := httptest.NewRecorder()
		s.updateMCPServer(rec, withURLParam(httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(`{"description":"concurrent"}`)), "serverID", serverID))
	}()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("删除项目与更新 MCP 并发时疑似死锁")
	}
}

func TestUpdateMCPServerRenameWithLegacyAutoApproveAndReorder(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	serverID := insertTestMCPServer(t, s, "legacy3", mcpScopeGlobal, "", mcpTransportStdio, "npx", nil,
		[]string{"windows"}, []string{"claude-code"})
	if _, err := s.db.ExecContext(ctx, `update mcp_servers set auto_approve_tools=? where id=?`, `["mcp__*","mcp__legacy3__tool"]`, serverID); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rec := httptest.NewRecorder()
	s.updateMCPServer(rec, withURLParam(httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(`{"name":"renamed3","autoApproveTools":["mcp__*","mcp__legacy3__tool"]}`)), "serverID", serverID))
	if rec.Code != http.StatusOK {
		t.Fatalf("rename+legacy status = %d: %s", rec.Code, rec.Body.String())
	}
	stored, err := s.fetchStoredMCPServer(ctx, serverID)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !slices.Equal(stored.AutoApproveTools, []string{"mcp__*", "mcp__renamed3__tool"}) {
		t.Fatalf("patterns = %v", stored.AutoApproveTools)
	}
	// 仅重排（工具列表 PUT 会 sort）不能触发重新校验。
	rec = httptest.NewRecorder()
	s.updateMCPServer(rec, withURLParam(httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(`{"autoApproveTools":["mcp__renamed3__tool","mcp__*"]}`)), "serverID", serverID))
	if rec.Code != http.StatusOK {
		t.Fatalf("reorder status = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestImportCandidateInitializesSlicesAndFingerprint(t *testing.T) {
	base := buildImportCandidate("demo", claudeMCPServer{Command: "npx"}, mcpScopeGlobal, "", "src", "label", "path", []string{"claude-code"})
	if base.SecretKeys == nil {
		t.Fatal("SecretKeys 不能是 nil（会序列化成 null）")
	}
	if base.Fingerprint == "" {
		t.Fatal("fingerprint 不能为空")
	}
	changed := buildImportCandidate("demo", claudeMCPServer{Command: "uvx"}, mcpScopeGlobal, "", "src", "label", "path", []string{"claude-code"})
	if changed.Fingerprint == base.Fingerprint {
		t.Fatal("command 变化后 fingerprint 应变化")
	}
}

func TestMarkBatchImportDuplicates(t *testing.T) {
	candidates := []mcpImportCandidate{
		{Name: "same", Scope: mcpScopeGlobal, SourceLabel: "s1"},
		{Name: "same", Scope: mcpScopeGlobal, SourceLabel: "s2"},
		{Name: "same", Scope: mcpScopeProject, ProjectID: "p", SourceLabel: "s3"},
	}
	markBatchImportDuplicates(candidates)
	if candidates[0].Conflict {
		t.Fatal("第一条不应冲突")
	}
	if !candidates[1].Conflict || candidates[1].SkipReason == "" {
		t.Fatalf("第二条应标记同批次冲突: %+v", candidates[1])
	}
	if candidates[2].Conflict {
		t.Fatal("不同项目/作用域不应冲突")
	}
}

func TestSummarizeMCPToolInputRedactsPasswordWithAtAndNameValueHeader(t *testing.T) {
	raw := json.RawMessage(`{"connection":"postgres://user:p@ss@host/db","headers":[{"name":"Authorization","value":"Bearer ghp_x"}]}`)
	got := summarizeMCPToolInput(raw)
	if strings.Contains(got, "ss@host") || strings.Contains(got, "ghp_x") {
		t.Fatalf("敏感内容未脱敏: %s", got)
	}
}

func TestProjectBindingViewReportsDefinitionDisabled(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	projectID := "proj-def-disabled"
	insertFixTestProject(t, s, projectID)
	serverID := insertTestMCPServer(t, s, "offdef", mcpScopeProject, projectID, mcpTransportStdio, "npx", nil,
		[]string{"windows"}, []string{"claude-code"})
	if _, err := s.db.ExecContext(ctx, `update mcp_servers set enabled=0 where id=?`, serverID); err != nil {
		t.Fatalf("disable: %v", err)
	}
	views, err := s.listProjectMCPBindings(ctx, projectID)
	if err != nil {
		t.Fatalf("bindings: %v", err)
	}
	if len(views) != 1 || views[0].DefinitionEnabled || views[0].Enabled {
		t.Fatalf("视图应如实反映定义已停用: %+v", views)
	}
}

func TestUpdateMCPServerScopeChangeClearsBindings(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	projectID := "proj-scope-bind"
	insertFixTestProject(t, s, projectID)
	serverID := insertTestMCPServer(t, s, "scopebind", mcpScopeProject, projectID, mcpTransportStdio, "npx", nil,
		[]string{"windows"}, []string{"claude-code"})
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx, `insert into mcp_project_bindings (project_id,server_id,enabled,created_at,updated_at) values (?,?,0,?,?)`,
		projectID, serverID, now, now); err != nil {
		t.Fatalf("insert binding: %v", err)
	}
	rec := httptest.NewRecorder()
	s.updateMCPServer(rec, withURLParam(httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(`{"scope":"global","projectId":""}`)), "serverID", serverID))
	if rec.Code != http.StatusOK {
		t.Fatalf("scope change status = %d: %s", rec.Code, rec.Body.String())
	}
	var remain int
	if err := s.db.QueryRowContext(ctx, `select count(*) from mcp_project_bindings where server_id=?`, serverID).Scan(&remain); err != nil {
		t.Fatalf("count: %v", err)
	}
	if remain != 0 {
		t.Fatalf("作用域变更后旧绑定未清理: %d", remain)
	}
}
