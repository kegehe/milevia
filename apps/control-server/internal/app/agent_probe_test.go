package app

import (
	"context"
	"strings"
	"testing"
)

// readinessSplitRunner 是钉住"两个工具的就绪判据不同"的替身：
// Ready() 与 CodexReady() 都说"不可用"，而 Version() 与 CodexVersion() 都能报出版本。
//
// 这样一个替身就能同时证明两条判据：Claude（按版本判）应当 ready，
// Codex（按二进制判）应当 unavailable。改错任何一边都会让本组用例变红。
type readinessSplitRunner struct{ runnerFunc }

func (readinessSplitRunner) Ready(context.Context) bool          { return false }
func (readinessSplitRunner) Version(context.Context) string      { return "2.1.216" }
func (readinessSplitRunner) CodexReady(context.Context) bool     { return false }
func (readinessSplitRunner) CodexVersion(context.Context) string { return "0.146.0" }
func (readinessSplitRunner) CodexCheckUpdate(context.Context) (bool, string, error) {
	return false, "", nil
}
func (readinessSplitRunner) CodexUpdate(context.Context) (string, string, error) { return "", "", nil }

// TestProbeAgentsCoversEveryCatalogTool 保证探测结果跟着目录走：
// 目录里几个工具，探测结果就该有几个 —— 这是"新增工具自动出现"的前提。
func TestProbeAgentsCoversEveryCatalogTool(t *testing.T) {
	server := &Server{runnerRegistry: newRunnerRegistry(), runtimeCtx: context.Background()}
	localID := server.localRunnerID()
	server.runnerRegistry.register(localID, runnerFunc(func(context.Context, AgentRunRequest, AgentRunSink) error { return nil }), RunnerMeta{ID: localID})

	agents := server.probeAgents(context.Background(), RunnerMeta{ID: localID})
	if len(agents) != len(agentCatalog()) {
		t.Fatalf("探测到 %d 个工具，目录里有 %d 个", len(agents), len(agentCatalog()))
	}
	seen := map[string]bool{}
	for _, agent := range agents {
		seen[agent.ID] = true
	}
	for _, entry := range agentCatalog() {
		if !seen[entry.ID] {
			t.Fatalf("目录里的 %s 没有出现在探测结果中", entry.ID)
		}
	}
}

// TestProbeAgentsDistinguishesUnsupportedFromNotInstalled 是本组最重要的一条。
//
// 一个只提供 Claude 的远端 Runner 上，Codex 的真相是"这个环境不提供它"，
// 而不是"没装"。两者对用户的含义完全不同（换环境 vs 去安装），界面文案也必须不同。
// 这正是本项目"不能把读不到写成没有"的同一条红线。
func TestProbeAgentsDistinguishesUnsupportedFromNotInstalled(t *testing.T) {
	server := &Server{runnerRegistry: newRunnerRegistry(), runtimeCtx: context.Background()}
	meta := RunnerMeta{ID: "ssh-plain", Name: "plain-host", Environment: "remote-linux"}
	server.runnerRegistry.register(meta.ID, runnerFunc(func(context.Context, AgentRunRequest, AgentRunSink) error { return nil }), meta)

	statuses := statusByID(server.probeAgents(context.Background(), meta))

	if got := statuses["claude-code"].Status; got != agentStatusReady {
		t.Fatalf("claude-code status = %q，期望 %q", got, agentStatusReady)
	}
	codex := statuses["codex"]
	if codex.Status != agentStatusUnsupported {
		t.Fatalf("codex status = %q，期望 %q（不支持的 Runner 不能被说成'没装'）", codex.Status, agentStatusUnsupported)
	}
	if !strings.Contains(codex.Reason, "不支持") {
		t.Fatalf("codex reason = %q，期望说明该 Runner 不支持 Codex", codex.Reason)
	}
	if codex.Status == agentStatusUnavailable {
		t.Fatal("不支持与未安装被混成了同一档")
	}
}

// TestProbeAgentsFollowsPerToolReadiness 钉住"就绪判据来自目录"。
//
// Claude 只看能否报版本，Codex 只看二进制是否存在（受管 api_key 档案自带凭据，
// 查登录态会把可用环境误判为不可用）。替身同时满足"Ready 为假、Version 有值"，
// 于是两条判据必须给出不同结论 —— 任何一边被改成另一边都会红。
func TestProbeAgentsFollowsPerToolReadiness(t *testing.T) {
	server := &Server{runnerRegistry: newRunnerRegistry(), runtimeCtx: context.Background()}
	localID := server.localRunnerID()
	stub := readinessSplitRunner{}
	server.runnerRegistry.register(localID, stub, RunnerMeta{ID: localID})
	server.codexRunner = stub

	statuses := statusByID(server.probeAgents(context.Background(), RunnerMeta{ID: localID}))

	claude := statuses["claude-code"]
	if claude.Status != agentStatusReady || claude.Version != "2.1.216" {
		t.Fatalf("claude-code = %+v，期望按版本判就绪（ready/2.1.216）", claude)
	}
	codex := statuses["codex"]
	if codex.Status != agentStatusUnavailable {
		t.Fatalf("codex = %+v，期望按二进制判就绪（Ready() 为假 → unavailable）", codex)
	}
}

// TestProbeAgentsWithoutDatabaseStillReportsReasons 钉住"探测不依赖库"。
//
// ⚠️ 这是 2026-09-21 加"已安装但不可用"那一档时**被全量测试抓到的真实回归**：
// 探测里多了一次登记表查询，而上面几个夹具都不建库 —— 于是 probeAgents 直接
// nil pointer panic。而它跑在 /api/runners 的热路径上、每个工具一次，
// panic 会把整个控制服务带走。
//
// 判据：读不到登记表（含压根没有库）就按老说法走，**既不能崩、也不能猜成"有记录"**。
func TestProbeAgentsWithoutDatabaseStillReportsReasons(t *testing.T) {
	server := &Server{runnerRegistry: newRunnerRegistry(), runtimeCtx: context.Background()}
	localID := server.localRunnerID()
	server.runnerRegistry.register(localID, readinessSplitRunner{}, RunnerMeta{ID: localID})
	server.codexRunner = readinessSplitRunner{}

	statuses := statusByID(server.probeAgents(context.Background(), RunnerMeta{ID: localID}))
	codex := statuses["codex"]
	if codex.Status != agentStatusUnavailable {
		t.Fatalf("codex = %+v，期望 unavailable", codex)
	}
	if codex.Reason == "" {
		t.Fatal("没有库时也必须给出原因，否则界面只能凭空猜一个说法")
	}
	// 读不到登记表 → **不许**说"已安装但不可用"（那是把"读不到"猜成"有记录"）。
	if strings.Contains(codex.Reason, "已安装但不可用") {
		t.Fatalf("没有库却报出了「已安装但不可用」：%q", codex.Reason)
	}
	if !strings.Contains(codex.Reason, "未安装") {
		t.Fatalf("没有库时应当回落到老说法：%q", codex.Reason)
	}
}

// TestProbeAgentsReflectsMaintenance 断言进行中的安装/升级会盖掉状态，
// 且此时不带"失败原因"（那是暂时状态，不是失败）。
func TestProbeAgentsReflectsMaintenance(t *testing.T) {
	server := &Server{
		runnerRegistry: newRunnerRegistry(),
		runtimeCtx:     context.Background(),
		runnerUpdating: map[runnerAgentKey]bool{},
	}
	localID := server.localRunnerID()
	server.runnerRegistry.register(localID, runnerFunc(func(context.Context, AgentRunRequest, AgentRunSink) error { return nil }), RunnerMeta{ID: localID})
	server.runnerUpdating[runnerAgentKey{runnerID: localID, agentID: "codex"}] = true

	statuses := statusByID(server.probeAgents(context.Background(), RunnerMeta{ID: localID}))
	codex := statuses["codex"]
	if codex.Status != agentStatusUpdating {
		t.Fatalf("codex status = %q，期望 %q", codex.Status, agentStatusUpdating)
	}
	if codex.Reason != "" {
		t.Fatalf("更新中不该带失败原因，却得到 %q", codex.Reason)
	}
}

// TestLegacyAgentFieldsDeriveFromAgents 钉住过渡字段的形状与来源。
func TestLegacyAgentFieldsDeriveFromAgents(t *testing.T) {
	agents := []AgentStatus{
		{ID: "claude-code", Status: agentStatusReady, Version: "2.1.216"},
		{ID: "codex", Status: agentStatusUnavailable, Reason: "本机 Codex 未安装"},
	}

	registered := legacyAgentFields(agents, true)
	claude, ok := registered["claude"].(map[string]any)
	if !ok {
		t.Fatalf("claude 键形状不对：%#v", registered["claude"])
	}
	if claude["status"] != agentStatusReady || claude["version"] != "2.1.216" {
		t.Fatalf("claude = %#v", claude)
	}
	codex, ok := registered["codex"].(map[string]string)
	if !ok {
		t.Fatalf("codex 键形状不对：%#v", registered["codex"])
	}
	// codex 三个键必须齐全（旧契约如此，缺键会让前端读到 undefined）。
	for _, key := range []string{"status", "version", "reason"} {
		if _, present := codex[key]; !present {
			t.Fatalf("codex 缺少 %q 键：%#v", key, codex)
		}
	}

	// 原行为：runner 未注册时 claude 键不出现（而不是出现一个 unavailable）。
	unregistered := legacyAgentFields(agents, false)
	if _, present := unregistered["claude"]; present {
		t.Fatal("runner 未注册时不应出现 claude 键")
	}
	if _, present := unregistered["codex"]; !present {
		t.Fatal("codex 键应当始终存在")
	}
}

// TestRunnerEndpointsShareOneProbePath 是接线断言：两个列表接口必须共用同一份探测，
// 不能再各自写一遍工具分支。
func TestRunnerEndpointsShareOneProbePath(t *testing.T) {
	source := readGoSource(t, "app.go")
	code := stripGoComments(t, source)

	// 原先两段重复块里各自写了一遍这句漂移过的文案（"本机"/"本地"两种）。
	if strings.Contains(code, "未安装或未登录") {
		t.Fatal("app.go 里又出现了各写一遍的工具不可用文案，应走 agentUnavailableReason")
	}
	for _, signature := range []string{
		"func (s *Server) listRunners(",
		"func (s *Server) runnerStatus(",
	} {
		body := functionBody(t, source, signature)
		if !strings.Contains(body, "s.probeAgents(") {
			t.Fatalf("%s 没有调用 s.probeAgents，可能又写了一份重复的探测", signature)
		}
		if !strings.Contains(body, "legacyAgentFields(") {
			t.Fatalf("%s 没有用 legacyAgentFields 派生过渡字段", signature)
		}
	}
}

func statusByID(agents []AgentStatus) map[string]AgentStatus {
	out := make(map[string]AgentStatus, len(agents))
	for _, agent := range agents {
		out[agent.ID] = agent
	}
	return out
}

// functionBody 截取某个函数（从签名所在行到下一个顶层 func 之前）的源码。
func functionBody(t *testing.T, source, signature string) string {
	t.Helper()
	start := strings.Index(source, signature)
	if start < 0 {
		t.Fatalf("找不到 %s", signature)
	}
	rest := source[start+len(signature):]
	if end := strings.Index(rest, "\nfunc "); end >= 0 {
		return rest[:end]
	}
	return rest
}
