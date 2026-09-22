package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestGenericAgentRouteMatchesLegacyRoute 是成对用例。
//
// 泛化路由是过渡期新加的入口，旧路由改为委托。两者必须**逐字**给出同一个结果 ——
// 否则前端切换过程中会出现"页面上看到的版本和更新按钮的结论不一致"这类难查的问题。
func TestGenericAgentRouteMatchesLegacyRoute(t *testing.T) {
	server := newTestServer(t)
	runner := &updateTestRunner{checkAvailable: true, latestVersion: "2.1.217"}
	server.runnerRegistry.register("paired", runner, RunnerMeta{ID: "paired"})

	pairs := []struct{ legacy, generic string }{
		{"/api/runners/paired/claude/check-update", "/api/runners/paired/agents/claude-code/check-update"},
		{"/api/runners/paired/claude/update", "/api/runners/paired/agents/claude-code/update"},
	}
	for _, pair := range pairs {
		legacy := httptest.NewRecorder()
		server.routes().ServeHTTP(legacy, httptest.NewRequest(http.MethodPost, pair.legacy, nil))
		generic := httptest.NewRecorder()
		server.routes().ServeHTTP(generic, httptest.NewRequest(http.MethodPost, pair.generic, nil))

		if legacy.Code != generic.Code {
			t.Fatalf("%s 与 %s 状态码不同：%d vs %d", pair.legacy, pair.generic, legacy.Code, generic.Code)
		}
		if legacy.Body.String() != generic.Body.String() {
			t.Fatalf("%s 与 %s 响应不同：\n旧: %s\n新: %s", pair.legacy, pair.generic, legacy.Body.String(), generic.Body.String())
		}
		if legacy.Code != http.StatusOK {
			t.Fatalf("%s 期望 200，得到 %d body=%s", pair.generic, legacy.Code, legacy.Body.String())
		}
	}
}

// TestGenericAgentRouteRejectsUnknownAgent 断言工具 ID 也走目录校验，
// 并且错误文案点名是哪个 ID（不回落成某个已知工具）。
func TestGenericAgentRouteRejectsUnknownAgent(t *testing.T) {
	server := newTestServer(t)
	server.runnerRegistry.register("known", &updateTestRunner{}, RunnerMeta{ID: "known"})

	recorder := httptest.NewRecorder()
	server.routes().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/runners/known/agents/gemini-cli/check-update", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("未知工具期望 404，得到 %d body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "gemini-cli") {
		t.Fatalf("错误文案没有点名未知工具：%s", recorder.Body.String())
	}
}

// TestGenericAgentRouteRejectsUnsupportedToolOnRunner 断言"该环境不提供此工具"
// 与"这个工具不存在"给出不同文案。
func TestGenericAgentRouteRejectsUnsupportedToolOnRunner(t *testing.T) {
	server := newTestServer(t)
	server.runnerRegistry.register("plain", runnerFunc(func(context.Context, AgentRunRequest, AgentRunSink) error { return nil }), RunnerMeta{ID: "plain"})

	recorder := httptest.NewRecorder()
	server.routes().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/runners/plain/agents/codex/check-update", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("期望 404，得到 %d body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "不支持") {
		t.Fatalf("错误文案没有说明该运行器不支持：%s", recorder.Body.String())
	}
}

// TestAgentRoutesExistForEveryCatalogTool 是本组里防"新增工具忘了加路由"的一条。
//
// 它逐个用目录里的工具 ID 打泛化路由，要求响应**不是**路由未命中（chi 的 404
// 由 "404 page not found" 判定），而是一个由业务逻辑给出的响应。于是往目录里加
// 工具时，只要忘记接后端，这里就会红。
func TestAgentRoutesExistForEveryCatalogTool(t *testing.T) {
	server := newTestServer(t)
	server.runnerRegistry.register("route-check", &updateTestRunner{checkAvailable: true, latestVersion: "9.9.9"}, RunnerMeta{ID: "route-check"})

	for _, id := range supportedAgentIDs() {
		recorder := httptest.NewRecorder()
		path := "/api/runners/route-check/agents/" + id + "/check-update"
		server.routes().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, nil))
		if strings.Contains(recorder.Body.String(), "404 page not found") {
			t.Fatalf("%s 没有对应路由（目录里已有这个工具，路由必须自动可用）", path)
		}
	}
}

// TestAgentStatusJSONShapeCoversCatalog 断言 /api/runners 的 agents[] 与目录同构，
// 且过渡字段由它派生 —— 前端可以放心只读 agents[]。
func TestAgentStatusJSONShapeCoversCatalog(t *testing.T) {
	server := newTestServer(t)
	localID := server.localRunnerID()

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/runners", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("list runners: %d body=%s", response.Code, response.Body.String())
	}
	var raw []map[string]any
	if err := json.NewDecoder(response.Body).Decode(&raw); err != nil {
		t.Fatalf("decode runners: %v", err)
	}
	var local map[string]any
	for _, entry := range raw {
		if entry["id"] == localID {
			local = entry
		}
	}
	if local == nil {
		t.Fatalf("列表里没有本机 runner %q", localID)
	}
	agents, ok := local["agents"].([]any)
	if !ok {
		t.Fatalf("本机 runner 没有 agents[]：%#v", local)
	}
	if len(agents) != len(agentCatalog()) {
		t.Fatalf("agents[] 有 %d 项，目录有 %d 项", len(agents), len(agentCatalog()))
	}
	for _, item := range agents {
		agent, _ := item.(map[string]any)
		id, _ := agent["id"].(string)
		if _, known := agentByID(id); !known {
			t.Fatalf("agents[] 里出现了目录之外的工具 %q", id)
		}
		if agent["status"] == nil || agent["status"] == "" {
			t.Fatalf("%s 没有 status：%#v", id, agent)
		}
	}
}
