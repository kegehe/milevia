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
)

// 会话级模型选择（docs/36）的回归测试。

func TestRunModelPrecedence(t *testing.T) {
	profile := &AgentRuntimeProfile{Model: "profile-model"}
	cases := []struct {
		name     string
		override string
		profile  *AgentRuntimeProfile
		want     string
	}{
		{"会话覆盖优先于档案模型", "session-model", profile, "session-model"},
		{"无覆盖时用档案模型", "", profile, "profile-model"},
		{"无覆盖无档案时交给 CLI 默认", "", nil, ""},
		{"覆盖为纯空白视为未设置", "   ", profile, "profile-model"},
		{"前后空白被裁剪", " opus ", nil, "opus"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := runModel(testCase.override, testCase.profile); got != testCase.want {
				t.Fatalf("runModel() = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestNormalizeModelOverride(t *testing.T) {
	valid := map[string]string{
		"opus":                      "opus",
		"  claude-opus-5  ":         "claude-opus-5",
		"gpt-5.6-sol":               "gpt-5.6-sol",
		"openai/gpt-4o":             "openai/gpt-4o",
		"us.anthropic.x-v1:0":       "us.anthropic.x-v1:0",
		"claude-haiku-4-5@20251001": "claude-haiku-4-5@20251001",
		"":                          "",
		"   ":                       "",
	}
	for input, want := range valid {
		got, err := normalizeModelOverride(input)
		if err != nil {
			t.Fatalf("normalizeModelOverride(%q) 意外报错: %v", input, err)
		}
		if got != want {
			t.Fatalf("normalizeModelOverride(%q) = %q, want %q", input, got, want)
		}
	}

	invalid := []string{
		"opus; rm -rf /",  // 命令分隔符
		"opus && whoami",  // shell 连接符
		"$(whoami)",       // 命令替换
		"`whoami`",        // 反引号
		"opus opus",       // 空白
		"'quoted'",        // 引号
		"-dangerous",      // 不能以连字符开头
		"opus\n--verbose", // 换行
		"模型",              // 非 ASCII
		strings.Repeat("a", maxModelOverrideLength+1), // 过长
	}
	for _, input := range invalid {
		if got, err := normalizeModelOverride(input); err == nil {
			t.Fatalf("normalizeModelOverride(%q) 应被拒绝，却返回 %q", input, got)
		}
	}
}

func TestParseCodexModelCatalogFiltersHiddenModels(t *testing.T) {
	payload := `{"models":[
		{"slug":"gpt-6-astra","display_name":"GPT-6-Astra","description":"most capable","visibility":"list"},
		{"slug":"gpt-5.4-mini","display_name":"GPT-5.4-Mini","visibility":"hide"},
		{"slug":"gpt-5.5","display_name":"GPT-5.5","visibility":"list"},
		{"slug":"","display_name":"broken","visibility":"list"}
	]}`
	options := parseCodexModelCatalog([]byte(payload))
	if len(options) != 2 {
		t.Fatalf("expected 2 listed models, got %#v", options)
	}
	if options[0].ID != "gpt-6-astra" || options[0].Label != "GPT-6-Astra" || options[0].Description != "most capable" {
		t.Fatalf("unexpected first option: %#v", options[0])
	}
	if options[1].ID != "gpt-5.5" {
		t.Fatalf("hidden/slug-less entries must be dropped: %#v", options)
	}

	if options := parseCodexModelCatalog([]byte("not json")); options != nil {
		t.Fatalf("malformed catalog should yield nil, got %#v", options)
	}
	if options := parseCodexModelCatalog([]byte(`{"models":[]}`)); options != nil {
		t.Fatalf("empty catalog should yield nil, got %#v", options)
	}
}

// Claude 的模型来自 request.Model（由 runModel 解析），而不是 Profile.Model——
// 防止"两个字段都能表达模型"的分叉。
func TestClaudeRunnerArgsUseRequestModel(t *testing.T) {
	runner := &claudeCLIRunner{config: Config{PermissionMode: "acceptEdits"}}
	profile := &AgentRuntimeProfile{Model: "profile-model"}

	args, err := runner.args(AgentRunRequest{Prompt: "hi", PermissionMode: "read_only", Profile: profile, Model: "session-model"})
	if err != nil {
		t.Fatal(err)
	}
	if !containsArguments(args, "--model", "session-model") {
		t.Fatalf("session model missing from args: %q", args)
	}
	if strings.Contains(strings.Join(args, " "), "profile-model") {
		t.Fatalf("profile model must not leak into args when a request model is set: %q", args)
	}

	sessionArgs, err := runner.sessionArgs(AgentSessionRequest{SessionID: "session", Profile: profile, Model: "session-model"})
	if err != nil {
		t.Fatal(err)
	}
	if !containsArguments(sessionArgs, "--model", "session-model") {
		t.Fatalf("session model missing from session args: %q", sessionArgs)
	}

	// 无模型时不带 --model，交给 CLI 自己的配置。
	plain, err := runner.args(AgentRunRequest{Prompt: "hi", PermissionMode: "read_only", Profile: profile})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(plain, " "), "--model") {
		t.Fatalf("no model should be passed when nothing resolved one: %q", plain)
	}
}

func TestSSHCommandsCarryModel(t *testing.T) {
	// 模型名带引号风险字符的一半场景由 normalizeModelOverride 拦下，这里验证拼串本身
	// 经 shellQuote 闭合：注入尝试不会逃出引号。
	model := "opus'; rm -rf /tmp/x; echo '"
	quoted := " --model " + shellQuote(model)

	claudeCmd := buildSSHClaudeRunCommand(
		AgentRunRequest{Prompt: "hi", ProjectPath: "/srv/app", SessionID: "sid", Resume: true},
		"", "", "--permission-mode default", "--resume 'sid'", quoted,
	)
	if !strings.Contains(claudeCmd, "--model "+shellQuote(model)) {
		t.Fatalf("claude ssh command lost the model argument: %q", claudeCmd)
	}
	if strings.Contains(claudeCmd, "; rm -rf /tmp/x; echo ' ") {
		t.Fatalf("model must stay inside quotes: %q", claudeCmd)
	}
	if !strings.HasSuffix(claudeCmd, shellQuote("hi")) {
		t.Fatalf("prompt must remain the last argument: %q", claudeCmd)
	}

	// 参数顺序与本地 runner 一致：模型覆盖紧跟 exec，位于 resume / --sandbox 之前。
	codexCmd := buildSSHCodexExecCommand(
		AgentRunRequest{Prompt: "hi", ProjectPath: "/srv/app", SessionID: "sid", Resume: true, Model: "gpt-5.5"},
		"", `sandbox_mode="read-only"`, "", "read-only",
	)
	if !strings.Contains(codexCmd, `model="gpt-5.5"`) {
		t.Fatalf("codex ssh command lost the model argument: %q", codexCmd)
	}
	if strings.Count(codexCmd, `-c 'model="`) != 1 {
		t.Fatalf("model override must appear exactly once: %q", codexCmd)
	}
	if !strings.HasPrefix(codexCmd, `codex exec -c 'model="gpt-5.5"' resume `) {
		t.Fatalf("模型必须排在 exec 之后、resume 之前（与本地 runner 同序）: %q", codexCmd)
	}

	freshCodex := buildSSHCodexExecCommand(
		AgentRunRequest{Prompt: "hi", ProjectPath: "/srv/app", Model: "gpt-5.5"},
		"--disable responses_websockets", `sandbox_mode="workspace-write"`, "", "workspace-write",
	)
	if !strings.Contains(freshCodex, `--disable responses_websockets -c 'model="gpt-5.5"' -c 'sandbox_mode=`) {
		t.Fatalf("新会话的模型覆盖也应紧跟 transport 参数、排在 sandbox 之前: %q", freshCodex)
	}

	// 无模型时不得出现 model 参数（Codex 与 Claude 两个分支都测）。
	noModel := buildSSHCodexExecCommand(AgentRunRequest{Prompt: "hi", ProjectPath: "/srv/app"}, "", `sandbox_mode="read-only"`, "", "read-only")
	if strings.Contains(noModel, "model=") {
		t.Fatalf("没有模型时不应出现 model 覆盖: %q", noModel)
	}

	bareClaude := buildSSHClaudeRunCommand(AgentRunRequest{Prompt: "hi", ProjectPath: "/srv/app"}, "", "", "", "", "")
	if strings.Contains(bareClaude, "--model") {
		t.Fatalf("ssh claude command should omit --model when nothing resolved one: %q", bareClaude)
	}
}

func TestConversationSessionConfigTracksModel(t *testing.T) {
	base := Conversation{ID: "c", AgentID: "claude-code", PermissionMode: "approval_required", ExecutionPolicy: "approval_required"}
	withModel := base
	withModel.ModelOverride = "opus"

	plain := newConversationSessionConfig("windows-local", base, nil, "/srv/app")
	switched := newConversationSessionConfig("windows-local", withModel, nil, "/srv/app")
	if plain == switched {
		t.Fatal("模型变化必须让会话配置不同，否则长驻进程不会退役重启")
	}
	if switched.model != "opus" {
		t.Fatalf("session config model = %q, want opus", switched.model)
	}
	// 档案模型同样要进配置：档案里改模型也会让进程重启。
	fromProfile := newConversationSessionConfig("windows-local", base, &AgentRuntimeProfile{Model: "sonnet"}, "/srv/app")
	if fromProfile.model != "sonnet" || fromProfile == plain {
		t.Fatalf("profile model must feed the session config: %#v", fromProfile)
	}
}

// 会话级模型覆盖的读写闭环：POST 落库并回写会话，GET 目录报告来源。
func TestSetConversationModelRoundTrip(t *testing.T) {
	server := newTestServer(t)
	seedModelConversation(t, server, "claude-code", "idle")

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/conversations/model-conversation/model", strings.NewReader(`{"model":"opus"}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("set model status=%d body=%s", response.Code, response.Body.String())
	}
	var updated Conversation
	if err := json.Unmarshal(response.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode conversation: %v", err)
	}
	if updated.ModelOverride != "opus" {
		t.Fatalf("response modelOverride=%q", updated.ModelOverride)
	}
	var stored string
	if err := server.db.QueryRow(`select model_override from conversations where id='model-conversation'`).Scan(&stored); err != nil {
		t.Fatalf("read stored override: %v", err)
	}
	if stored != "opus" {
		t.Fatalf("stored model_override=%q", stored)
	}

	// 目录接口如实报告"会话覆盖"。
	response = httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/conversations/model-conversation/models", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("models status=%d body=%s", response.Code, response.Body.String())
	}
	var view ConversationModelsView
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode models view: %v", err)
	}
	if view.Selected != "opus" || view.Effective != "opus" || view.Source != "override" {
		t.Fatalf("unexpected models view: %#v", view)
	}
	if !view.CustomAllowed || len(view.Models) == 0 {
		t.Fatalf("claude catalog must be non-empty and custom models allowed: %#v", view)
	}

	// 清空覆盖 = 回到跟随配置。
	response = httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/conversations/model-conversation/model", strings.NewReader(`{"model":""}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("clear model status=%d body=%s", response.Code, response.Body.String())
	}
	if err := server.db.QueryRow(`select model_override from conversations where id='model-conversation'`).Scan(&stored); err != nil {
		t.Fatalf("read cleared override: %v", err)
	}
	if stored != "" {
		t.Fatalf("override should be cleared, got %q", stored)
	}
}

func TestSetConversationModelRejectsBadInput(t *testing.T) {
	server := newTestServer(t)
	seedModelConversation(t, server, "claude-code", "idle")

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/conversations/model-conversation/model", strings.NewReader(`{"model":"opus; rm -rf /"}`)))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid model must be rejected, status=%d body=%s", response.Code, response.Body.String())
	}

	// 运行中的会话不允许切换：切换需要重启长驻进程。
	if _, err := server.db.Exec(`update conversations set status='running' where id='model-conversation'`); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	response = httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/conversations/model-conversation/model", strings.NewReader(`{"model":"opus"}`)))
	if response.Code != http.StatusConflict {
		t.Fatalf("running conversation must reject model switch, status=%d body=%s", response.Code, response.Body.String())
	}

	response = httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/conversations/missing/model", strings.NewReader(`{"model":"opus"}`)))
	if response.Code != http.StatusNotFound {
		t.Fatalf("unknown conversation status=%d", response.Code)
	}
}

// 无档案的会话（SSH / Windows 侧 wsl-local 都是这种情况）也必须能选模型——这是本功能
// 相对"改 agent profile"的核心差别。
func TestConversationModelsWithoutProfile(t *testing.T) {
	server := newTestServer(t)
	seedModelConversation(t, server, "codex", "idle")

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/conversations/model-conversation/models", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("models status=%d body=%s", response.Code, response.Body.String())
	}
	var view ConversationModelsView
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode models view: %v", err)
	}
	if view.Source != "cli_default" {
		t.Fatalf("profile-less conversation should report cli_default, got %#v", view)
	}
	if len(view.Models) == 0 {
		t.Fatalf("codex catalog must never be empty (probe failure falls back): %#v", view)
	}
	if !view.CustomAllowed {
		t.Fatalf("custom models must always be allowed: %#v", view)
	}

	response = httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/conversations/model-conversation/model", strings.NewReader(`{"model":"gpt-5.5"}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("profile-less conversation must accept a model, status=%d body=%s", response.Code, response.Body.String())
	}
	var stored string
	if err := server.db.QueryRow(`select model_override from conversations where id='model-conversation'`).Scan(&stored); err != nil {
		t.Fatalf("read stored override: %v", err)
	}
	if stored != "gpt-5.5" {
		t.Fatalf("stored model_override=%q", stored)
	}
}

// 清空会话时应把模型覆盖带到接续会话上（与权限模式一致）。
func TestClearConversationCarriesModelOverride(t *testing.T) {
	server := newTestServer(t)
	seedModelConversation(t, server, "claude-code", "idle")
	if _, err := server.db.Exec(`update conversations set model_override='opus' where id='model-conversation'`); err != nil {
		t.Fatalf("seed override: %v", err)
	}

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/conversations/model-conversation/clear", strings.NewReader(`{}`)))
	if response.Code < 200 || response.Code > 299 {
		t.Fatalf("clear status=%d body=%s", response.Code, response.Body.String())
	}
	var fresh Conversation
	if err := json.Unmarshal(response.Body.Bytes(), &fresh); err != nil {
		t.Fatalf("decode cleared conversation: %v", err)
	}
	if fresh.ID == "model-conversation" {
		t.Fatalf("clear should mint a new conversation: %#v", fresh)
	}
	if fresh.ModelOverride != "opus" {
		t.Fatalf("cleared conversation lost the model override: %#v", fresh)
	}
}

// seedModelConversation 建一个带项目的会话，用于模型接口测试。
func seedModelConversation(t *testing.T, server *Server, agentID, status string) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('model-project','model-project',?,?,'main',1,?)`, t.TempDir(), server.localRunnerID(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,agent_id,agent_session_id,agent_runtime_id,execution_policy,status,permission_mode,title,last_activity_at,claude_initialized,agent_initialized,is_current,created_at) values ('model-conversation','model-project','model-session',?,'model-session',?,'approval_required',?,'approval_required','新会话',?,0,0,1,?)`, agentID, server.localRunnerID(), status, now, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
}

// sendAndCaptureModel 发送一条消息并返回 runner 实际收到的模型。
func sendAndCaptureModel(t *testing.T, server *Server) string {
	t.Helper()
	received := make(chan string, 1)
	server.runner = runnerFunc(func(_ context.Context, request AgentRunRequest, _ AgentRunSink) error {
		received <- request.Model
		return nil
	})
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/conversations/model-conversation/messages", strings.NewReader(`{"content":"用哪个模型"}`)))
	if response.Code != http.StatusAccepted {
		t.Fatalf("send status=%d body=%s", response.Code, response.Body.String())
	}
	select {
	case model := <-received:
		waitForConversationIdle(t, server, "model-conversation")
		return model
	case <-time.After(10 * time.Second):
		t.Fatal("runner never received the run")
		return ""
	}
}

// 会话级覆盖必须一路走到 runner：DB 列 → startMessage → AgentRunRequest.Model。
// 只测 runModel() 不足以证明它真的接在运行链路上。
func TestModelOverrideReachesRunner(t *testing.T) {
	server := newTestServer(t)
	seedModelConversation(t, server, "claude-code", "idle")
	if _, err := server.db.Exec(`update conversations set model_override='opus' where id='model-conversation'`); err != nil {
		t.Fatalf("seed override: %v", err)
	}
	if model := sendAndCaptureModel(t, server); model != "opus" {
		t.Fatalf("runner received model %q, want opus", model)
	}
}

// 没有会话覆盖时，档案模型照旧生效（新链路的优先级不能把老行为弄丢）。
func TestProfileModelStillReachesRunner(t *testing.T) {
	server := newTestServer(t)
	profile := createCLIManagedProfile(t, server, "claude-code", "claude-profile-model")
	seedModelConversation(t, server, "claude-code", "idle")
	if _, err := server.db.Exec(`update conversations set agent_profile_revision_id=? where id='model-conversation'`, profile.CurrentRevisionID); err != nil {
		t.Fatalf("link profile: %v", err)
	}
	if model := sendAndCaptureModel(t, server); model != "claude-profile-model" {
		t.Fatalf("runner received model %q, want the profile model", model)
	}
	if _, err := server.db.Exec(`update conversations set model_override='haiku' where id='model-conversation'`); err != nil {
		t.Fatalf("seed override: %v", err)
	}
	if model := sendAndCaptureModel(t, server); model != "haiku" {
		t.Fatalf("session override must win over the profile model, got %q", model)
	}
}

// Windows 上 npm 的 codex 是 .cmd 批处理；路径不能加引号（Go 会把引号转义成 \"，
// cmd.exe 于是把它当成命令名的一部分，报"不是内部或外部命令"）。这条断言锁住该修正，
// 同时保证用户参数仍然是独立 argv，不会被拼进命令串。
func TestCodexCommandContextQuotesOnlyWhenNeeded(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("npm .cmd shim 是 Windows 专有问题")
	}
	shim := `C:\Users\someone\AppData\Roaming\npm\codex.cmd`
	cmd := codexCommandContext(context.Background(), shim, "debug", "models")
	want := []string{"cmd.exe", "/d", "/c", shim, "debug", "models"}
	if len(cmd.Args) != len(want) {
		t.Fatalf("argv = %#v, want %#v", cmd.Args, want)
	}
	for i := range want {
		if cmd.Args[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q（完整 argv %#v）", i, cmd.Args[i], want[i], cmd.Args)
		}
	}

	// 含空格的安装路径仍走带引号形式（未验证可用，但不能退化成更差）。
	spaced := codexCommandContext(context.Background(), `C:\Program Files\npm\codex.cmd`, "--version")
	if !strings.Contains(strings.Join(spaced.Args, " "), `"C:\Program Files\npm\codex.cmd"`) {
		t.Fatalf("spaced path should keep the quoted form: %#v", spaced.Args)
	}

	// 非 .cmd/.bat 不做 cmd.exe 包装，行为保持不变。
	plain := codexCommandContext(context.Background(), `C:\tools\codex.exe`, "--version")
	if plain.Args[0] != `C:\tools\codex.exe` {
		t.Fatalf("plain binary must not be wrapped in cmd.exe: %#v", plain.Args)
	}
}

// 切换模型必须"一次点击就生效"。retireForConfiguration 在「刚发起退役」时也返回 false，
// 若直接把它当成失败抛 409，用户每次换模型都要点两次（实测确认过）。控制器因此会等退役的
// 长驻会话真正从 s.sessions 消失，再落库。
func TestSetConversationModelWaitsForSessionRetirement(t *testing.T) {
	server := newTestServer(t)
	seedModelConversation(t, server, "claude-code", "idle")
	session := newIdleAgentSession()
	server.mu.Lock()
	server.sessions["model-conversation"] = &activeAgentSession{agent: session, runnerID: server.localRunnerID(), lastUsedAt: time.Now().UTC()}
	server.mu.Unlock()

	// 模拟 watcher：进程退出后把会话从活跃表里摘掉。不加人为延迟——控制器只等 2 秒，
	// 测试自身不该在这条链路上再消耗余量（机器忙时容易变成偶发失败）。
	go func() {
		<-session.Done()
		server.mu.Lock()
		delete(server.sessions, "model-conversation")
		server.mu.Unlock()
	}()

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/conversations/model-conversation/model", strings.NewReader(`{"model":"opus"}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("切模型应等会话退役后一次成功，status=%d body=%s", response.Code, response.Body.String())
	}
	var stored string
	if err := server.db.QueryRow(`select model_override from conversations where id='model-conversation'`).Scan(&stored); err != nil {
		t.Fatalf("read stored override: %v", err)
	}
	if stored != "opus" {
		t.Fatalf("stored model_override=%q", stored)
	}

	// 会话没在退役（例如有审批挂起）时不得空等：立刻返回 false。
	busy := newIdleAgentSession()
	server.mu.Lock()
	server.sessions["model-conversation"] = &activeAgentSession{agent: busy, runnerID: server.localRunnerID(), lastUsedAt: time.Now().UTC()}
	server.mu.Unlock()
	start := time.Now()
	if server.awaitSessionRetired("model-conversation", 2*time.Second) {
		t.Fatal("未进入 stopping 的会话不应被判定为已退役")
	}
	// 阈值给到 1s：既证明"没有空等到 2s 超时"，又不会因机器负载造成偶发失败。
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("未在退役的会话应立刻失败，实际等了 %s", elapsed)
	}
	server.mu.Lock()
	delete(server.sessions, "model-conversation")
	server.mu.Unlock()
}

// 每处 run 请求构造都必须显式给出 Model。
//
// 为什么要有这条：runner 已改为只读 request.Model（模型只有一个来源，见 AgentRunRequest.Model），
// 于是"新写了一处 run 请求却忘了传 Model"不再有任何编译或运行期报错，而是**静默退化成 CLI
// 默认模型**——实施过程中 insights.go 与 orchestration.go 就是这样被漏掉、又靠这条思路查回来的。
// 用源码扫描把这类遗漏变成测试失败。
func TestEveryRunRequestSetsModel(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	needles := []string{"AgentRunRequest{", "AgentSessionRequest{"}
	checked := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		source, readErr := os.ReadFile(file)
		if readErr != nil {
			t.Fatalf("read %s: %v", file, readErr)
		}
		text := string(source)
		for _, needle := range needles {
			for offset := 0; ; {
				index := strings.Index(text[offset:], needle)
				if index < 0 {
					break
				}
				start := offset + index
				offset = start + len(needle)
				// 字面量都很短（< 800 字符）；截到闭合大括号，避免把下一个结构体的字段算进来。
				window := text[offset:]
				if end := strings.Index(window, "\n\t}"); end >= 0 && end < 800 {
					window = window[:end]
				} else if len(window) > 800 {
					window = window[:800]
				}
				if !strings.Contains(window, "Model:") {
					line := 1 + strings.Count(text[:start], "\n")
					t.Fatalf("%s:%d 的 %s 没有显式传 Model，会被静默丢成 CLI 默认模型", file, line, needle)
				}
				checked++
			}
		}
	}
	if checked < 5 {
		t.Fatalf("只检查到 %d 处 run 请求，扫描逻辑可能失效了", checked)
	}
}

// 本机 Codex 的 argv 是"模型有没有传下去"唯一没被覆盖的路径（Claude 本机 args、SSH 两个
// 构造器、WSL 复用 Claude args 都有测试）。模型串错位会顶掉 prompt，所以连位置一起断言。
func TestCodexExecArgsPassModel(t *testing.T) {
	resume := codexExecArgs(AgentRunRequest{
		Prompt: "hi", ProjectPath: `C:\proj`, SessionID: "sid", Resume: true,
		Model: "gpt-5.5", CodexMCPArgs: []string{"-c", `mcp_servers.x.command="npx"`},
	}, []string{"OPENAI_API_KEY=secret"}, "", "read-only")
	if joined := strings.Join(resume, " "); strings.Count(joined, "model=") != 1 || !strings.Contains(joined, `model="gpt-5.5"`) {
		t.Fatalf("模型覆盖缺失或重复: %q", resume)
	}
	if containsArguments(resume, "--model") {
		t.Fatalf("Codex 用的是 -c model=，不该出现 --model: %q", resume)
	}
	if resume[len(resume)-1] != "hi" {
		t.Fatalf("prompt 必须是最后一个参数，否则模型串会顶掉它: %q", resume)
	}
	modelIndex, resumeIndex := indexOfArgument(resume, `model="gpt-5.5"`), indexOfArgument(resume, "resume")
	if modelIndex < 0 || resumeIndex < 0 || modelIndex > resumeIndex {
		t.Fatalf("模型覆盖应排在 resume 之前（与既有行为一致）: %q", resume)
	}

	fresh := codexExecArgs(AgentRunRequest{Prompt: "hi", ProjectPath: "/p", Model: "gpt-5.5"}, nil, "/tmp/schema.json", "workspace-write")
	if !strings.Contains(strings.Join(fresh, " "), `--output-schema /tmp/schema.json`) {
		t.Fatalf("schema 参数丢失: %q", fresh)
	}
	if fresh[len(fresh)-1] != "hi" || !strings.Contains(strings.Join(fresh, " "), `-C /p --sandbox workspace-write`) {
		t.Fatalf("新会话参数装配异常: %q", fresh)
	}

	// 没有模型时不得注入任何 model 覆盖。
	plain := codexExecArgs(AgentRunRequest{Prompt: "hi", ProjectPath: "/p"}, nil, "", "read-only")
	if strings.Contains(strings.Join(plain, " "), "model=") {
		t.Fatalf("无模型时不该出现 model 覆盖: %q", plain)
	}
}

// 目录探测失败必须降级成内置列表，而不是让整个接口失败（docs/36 §3.4）。
func TestCodexModelCatalogFallsBackWhenProbeFails(t *testing.T) {
	server := newTestServer(t)
	server.codexRunner = failingCatalogRunner{}
	options, note := server.codexModelCatalogFor(context.Background(), server.localRunnerID(), t.TempDir())
	if options != nil {
		t.Fatalf("探测失败应返回 nil 目录，交给调用方回退: %#v", options)
	}
	if !strings.Contains(note, "回退") {
		t.Fatalf("降级说明未如实告知用户: %q", note)
	}
	if len(codexFallbackModelCatalog()) == 0 {
		t.Fatal("内置兜底列表不能为空，否则选择器会显示空菜单")
	}

	// 第二次调用走缓存，不应再次探测（探测失败也缓存，避免反复拉起进程）。
	again, againNote := server.codexModelCatalogFor(context.Background(), server.localRunnerID(), t.TempDir())
	if again != nil || !strings.Contains(againNote, "回退") {
		t.Fatalf("缓存命中后行为应一致: %#v %q", again, againNote)
	}
}

type failingCatalogRunner struct{ runnerFunc }

func (failingCatalogRunner) codexModelCatalog(context.Context) ([]AgentModelOption, error) {
	return nil, errors.New("probe failed")
}

func indexOfArgument(args []string, needle string) int {
	for i, arg := range args {
		if strings.Contains(arg, needle) {
			return i
		}
	}
	return -1
}
