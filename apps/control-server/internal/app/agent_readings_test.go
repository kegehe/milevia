package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

// 读数缓存的用例。它们钉的是四件事：
//
//  1. **TTL 内只探一次**（"反复进出页面不再反复检测"）；
//  2. **force 真的重探**，且新读数写回缓存（"手动点重新检查才重新触发"）；
//  3. **维护态与跨越写操作的探测结果不入缓存**（这两档是"旧读数盖住新事实"的入口）；
//  4. **写操作完成后读数立刻作废**（"装完了界面还说没装"）。

// countingProbeRunner 是会计数的探测替身：每次 Version() 被调用就 +1。
//
// 用 Version 而不是 Ready 计数，是因为 claude-code 的判据是"能否报出版本"
// （见 probeAgent 的 readinessBinary 分支）—— 探一次就必然调它一次。
type countingProbeRunner struct {
	mu      sync.Mutex
	probes  int
	version string
}

func (r *countingProbeRunner) Ready(context.Context) bool                               { return true }
func (r *countingProbeRunner) Run(context.Context, AgentRunRequest, AgentRunSink) error { return nil }
func (r *countingProbeRunner) CheckUpdate(context.Context) (bool, string, error) {
	return false, "", nil
}
func (r *countingProbeRunner) Update(context.Context) (string, string, error) { return "", "", nil }

func (r *countingProbeRunner) Version(ctx context.Context) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.probes++
	// 真实的 runner 会因 ctx 取消而报不出版本（runVersionCommand 拿到的是失败）。
	// 替身也照做：否则"探测没查成"这条路径在测试里造不出来 —— 超时轮与正常轮的结果
	// 会一模一样，断言就区分不出"缓存被覆盖了"。
	if ctx.Err() != nil {
		return ""
	}
	return r.version
}

func (r *countingProbeRunner) probeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.probes
}

// blockingProbeRunner 把探测卡在 Version() 里，直到 release 关闭 —— 用来制造
// "探测在飞"的窗口（singleflight 与失效世代两条用例都要它）。
type blockingProbeRunner struct {
	countingProbeRunner
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *blockingProbeRunner) Version(ctx context.Context) string {
	r.once.Do(func() { close(r.started) })
	<-r.release
	return r.countingProbeRunner.Version(ctx)
}

// countingCheckRunner 统计 CheckUpdate 的调用次数，用来证明"最新版本"那一趟网络
// 查询也被缓存住了。
type countingCheckRunner struct {
	mu        sync.Mutex
	checks    int
	available bool
	latest    string
	err       error
}

func (r *countingCheckRunner) Ready(context.Context) bool                               { return true }
func (r *countingCheckRunner) Run(context.Context, AgentRunRequest, AgentRunSink) error { return nil }
func (r *countingCheckRunner) Version(context.Context) string                           { return "2.1.216" }
func (r *countingCheckRunner) Update(context.Context) (string, string, error)           { return "", "", nil }

func (r *countingCheckRunner) CheckUpdate(context.Context) (bool, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checks++
	return r.available, r.latest, r.err
}

func (r *countingCheckRunner) checkCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.checks
}

// newReadingsFixture 造一台"只有远端 runner"的轻量 Server。
//
// 刻意用**远端** meta：本机的 codex / codebuddy 由独立的 runner 提供（会真的去探），
// 而远端环境下 codex / codebuddy 走 agentBackend 的兜底分支 —— 这里的替身**没有**
// crossShellRunner（见 agent_catalog_backend.go），所以它们落进"尚未接通"那一档
// （实现了跨端执行面的 runner 会在那里被目录驱动后端接管，当时这一组用例的意图是
// 只依赖我们自己的替身、不去碰机器上真实装了什么）。
func newReadingsFixture(t *testing.T, runner AgentRunner) (*Server, RunnerMeta) {
	t.Helper()
	server := &Server{
		runnerRegistry: newRunnerRegistry(),
		runtimeCtx:     context.Background(),
		runnerUpdating: map[runnerAgentKey]bool{},
	}
	meta := RunnerMeta{ID: "readings-test", Name: "readings-test", Environment: "remote-linux"}
	server.runnerRegistry.register(meta.ID, runner, meta)
	return server, meta
}

// TestProbeAgentsReadsOnceWithinTTL 是"反复进出页面不再反复检测"的服务端证明。
func TestProbeAgentsReadsOnceWithinTTL(t *testing.T) {
	runner := &countingProbeRunner{version: "1.0.0"}
	server, meta := newReadingsFixture(t, runner)

	first := server.probeAgents(context.Background(), meta)
	afterFirst := runner.probeCount()
	if afterFirst == 0 {
		t.Fatal("第一次探测压根没探过（计数为 0），后面的断言证明不了任何事")
	}

	second := server.probeAgents(context.Background(), meta)
	if got := runner.probeCount(); got != afterFirst {
		t.Fatalf("TTL 内第二次探测又探了一遍（%d → %d）—— 缓存没生效", afterFirst, got)
	}
	if !agentStatusesEqual(first, second) {
		t.Fatal("缓存回放的读数与第一次不一致")
	}
}

// TestProbeAgentsForceReprobes 钉住"手动点重新检查才重新触发"，
// 并钉住"手动那次的新读数会被写回缓存"（否则点完刷新、切走再回来又探一遍）。
func TestProbeAgentsForceReprobes(t *testing.T) {
	runner := &countingProbeRunner{version: "1.0.0"}
	server, meta := newReadingsFixture(t, runner)

	server.probeAgents(context.Background(), meta)
	before := runner.probeCount()

	if _, at := server.probeAgentsFor(context.Background(), meta, true); at.IsZero() {
		t.Fatal("force 探测应当带回读数时刻")
	}
	afterForce := runner.probeCount()
	if afterForce == before {
		t.Fatal("force=true 没有重新探测")
	}

	// force 那次的结果必须被写回：随后的自动读取不该再探。
	server.probeAgents(context.Background(), meta)
	if got := runner.probeCount(); got != afterForce {
		t.Fatalf("force 探测的结果没有写回缓存（%d → %d）", afterForce, got)
	}
}

// TestProbeAgentsDoesNotCacheMaintenance 钉住维护态不入缓存。
//
// 缓存住 "正在更新" 的后果是：安装早就结束了，界面还挂着"正在更新"—— 用户会以为
// 卡住了，而实际上什么都没在跑。
func TestProbeAgentsDoesNotCacheMaintenance(t *testing.T) {
	runner := &countingProbeRunner{version: "1.0.0"}
	server, meta := newReadingsFixture(t, runner)
	key := runnerAgentKey{runnerID: meta.ID, agentID: "claude-code"}

	server.runnerMaintenanceMu.Lock()
	server.runnerUpdating[key] = true
	server.runnerMaintenanceMu.Unlock()

	statuses := statusByID(server.probeAgents(context.Background(), meta))
	if got := statuses["claude-code"].Status; got != agentStatusUpdating {
		t.Fatalf("维护中应当报 %q，得到 %q", agentStatusUpdating, got)
	}

	// 维护结束。
	server.runnerMaintenanceMu.Lock()
	delete(server.runnerUpdating, key)
	server.runnerMaintenanceMu.Unlock()

	statuses = statusByID(server.probeAgents(context.Background(), meta))
	if got := statuses["claude-code"].Status; got == agentStatusUpdating {
		t.Fatal("维护结束后仍然报 updating —— 维护态被缓存住了")
	}
	if runner.probeCount() == 0 {
		t.Fatal("维护结束后应当真的重新探一次")
	}
}

// TestProbeAgentsIgnoresCacheWhileMaintenanceActive 钉住维护期间**读**也不走缓存。
//
// 与上一条不同：上一条验的是"维护态不入缓存"，这一条验的是"缓存里的就绪不许盖住
// 维护态"。场景很实际 —— 先探到"就绪"（进了缓存），用户在**另一个客户端**上点了
// 安装，这台机器在别的客户端看来必须立刻是"正在更新"，而不是继续回放那份"就绪"。
func TestProbeAgentsIgnoresCacheWhileMaintenanceActive(t *testing.T) {
	runner := &countingProbeRunner{version: "1.0.0"}
	server, meta := newReadingsFixture(t, runner)
	key := runnerAgentKey{runnerID: meta.ID, agentID: "claude-code"}

	// 前置：先探一次，把"就绪"写进缓存。
	statuses := statusByID(server.probeAgents(context.Background(), meta))
	if got := statuses["claude-code"].Status; got != agentStatusReady {
		t.Fatalf("前置条件不成立：期望 %q，得到 %q", agentStatusReady, got)
	}

	server.runnerMaintenanceMu.Lock()
	server.runnerUpdating[key] = true
	server.runnerMaintenanceMu.Unlock()

	statuses = statusByID(server.probeAgents(context.Background(), meta))
	if got := statuses["claude-code"].Status; got != agentStatusUpdating {
		t.Fatalf("维护期间必须报 %q —— 缓存里的「就绪」不许盖住它，实际 %q", agentStatusUpdating, got)
	}
}

// TestProbeAgentsSingleflightsColdCache 钉住"一次页面加载只探一遍"。
//
// 进页面时 /api/runners 与 /agents 是**并发**打过来的，两边都会 miss 缓存；
// 没有 singleflight 就是同一件事探两遍（诊断那次还会是第三遍）。
func TestProbeAgentsSingleflightsColdCache(t *testing.T) {
	runner := &blockingProbeRunner{started: make(chan struct{}), release: make(chan struct{})}
	server, meta := newReadingsFixture(t, runner)

	done := make(chan []AgentStatus, 2)
	go func() { done <- server.probeAgents(context.Background(), meta) }()
	<-runner.started // 第一轮确实在飞了
	go func() { done <- server.probeAgents(context.Background(), meta) }()

	close(runner.release)
	<-done
	<-done

	if got := runner.probeCount(); got != 1 {
		t.Fatalf("冷缓存下的并发探测应当合并成一次，实际探了 %d 次", got)
	}
}

// TestProbeAgentsDiscardsResultCrossingAWrite 钉住失效世代。
//
// 场景：探测在写操作（安装/升级）开始前发起、写操作结束后才回来。若不挡住，
// 一份"装之前"的读数会被写进缓存并存活整整一个 TTL —— 正是这次要修的那类错的反面。
func TestProbeAgentsDiscardsResultCrossingAWrite(t *testing.T) {
	runner := &blockingProbeRunner{started: make(chan struct{}), release: make(chan struct{})}
	server, meta := newReadingsFixture(t, runner)

	done := make(chan []AgentStatus, 1)
	go func() { done <- server.probeAgents(context.Background(), meta) }()
	<-runner.started

	// 探测还在飞的时候，这台机器上发生了一次写操作。
	server.invalidateAgentReadings(meta.ID)

	close(runner.release)
	<-done

	// 那份跨越了写操作的读数不许被写回缓存。
	before := runner.probeCount()
	server.probeAgents(context.Background(), meta)
	if runner.probeCount() == before {
		t.Fatal("跨越写操作的探测结果被写回了缓存 —— 刚装完的工具会被旧读数盖住")
	}
}

// stagedProbeRunner 让**每次**探测的结束时刻与返回值都由测试自己排定。
//
// blockingProbeRunner 只能把"所有"探测一起卡住，因此"先发起的那一轮后结束"这种顺序
// 造不出来 —— 而"旧轮会不会顶掉新轮"恰恰只有把先后钉死才测得到。
//
// entered 是"某次探测已经进到 Version 里"的令牌（缓冲做成够用，发令牌不阻塞）：
// 用例据此**确定性**地等第 N 轮真的发起，而不是 sleep 一个猜出来的时长
// —— 那种写法在本机满负载跑整套测试时会偶发假失败。
// 序号按调用先后分配（持锁自增），所以第一次 Version 一定拿到下标 0：
// 用例必须先等到第 1 轮的令牌，再发起第 2 轮，否则两轮的下标会随调度翻转。
type stagedProbeRunner struct {
	mu       sync.Mutex
	calls    int
	versions []string
	gates    []chan struct{}
	entered  chan struct{}
}

func (r *stagedProbeRunner) Ready(context.Context) bool                               { return true }
func (r *stagedProbeRunner) Run(context.Context, AgentRunRequest, AgentRunSink) error { return nil }
func (r *stagedProbeRunner) CheckUpdate(context.Context) (bool, string, error) {
	return false, "", nil
}
func (r *stagedProbeRunner) Update(context.Context) (string, string, error) { return "", "", nil }

func (r *stagedProbeRunner) Version(ctx context.Context) string {
	r.mu.Lock()
	index := r.calls
	r.calls++
	r.mu.Unlock()
	if r.entered != nil {
		r.entered <- struct{}{}
	}
	if index < len(r.gates) && r.gates[index] != nil {
		<-r.gates[index]
	}
	if ctx.Err() != nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if index < len(r.versions) {
		return r.versions[index]
	}
	return ""
}

func (r *stagedProbeRunner) probeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// TestProbeAgentsWaiterDoesNotReplayUnacceptedRound 钉住等待者的回放判据。
//
// 场景：探测（第 1 轮）在飞时发生了一次写操作；此时**第二个调用方**到达，它看到在飞的那一轮
// 就等在上面。那一轮结束时因为"跨越了写操作"不入缓存 —— 等待者若照旧回放，拿到的就是
// **写操作之前**的读数、时间戳却是探测结束时刻，即"旧读数标注成刚刚读到的"，正是 epoch
// 机制要挡的那件事。正确行为：醒来后重新决策，自己再探一轮。
func TestProbeAgentsWaiterDoesNotReplayUnacceptedRound(t *testing.T) {
	runner := &blockingProbeRunner{started: make(chan struct{}), release: make(chan struct{})}
	runner.version = "1.0.0"
	server, meta := newReadingsFixture(t, runner)

	first := make(chan []AgentStatus, 1)
	go func() { first <- server.probeAgents(context.Background(), meta) }()
	<-runner.started // 第 1 轮确实在飞（此时 entry.round 已挂上）

	server.invalidateAgentReadings(meta.ID) // 写操作落在探测期间 → 那一轮不会被采纳

	// 第二个调用方：非 force，且缓存已被清空 → 必然等在飞的那一轮上。
	// （runner.started 只在 Version 里触发，也就是 entry.round 挂好之后，所以这里是确定的，
	// 不靠 sleep 抢时序。）
	second := make(chan []AgentStatus, 1)
	go func() { second <- server.probeAgents(context.Background(), meta) }()

	close(runner.release)
	<-first
	got := <-second

	if probes := runner.probeCount(); probes < 2 {
		t.Fatalf("等待者回放了那一轮没被采纳的读数（只探了 %d 次）—— 那是写操作之前的读数", probes)
	}
	if len(got) == 0 {
		t.Fatal("等待者拿到了空读数：它应当自己重探一轮并把结果给它")
	}
	if status := statusByID(got)["claude-code"]; status.Status != agentStatusReady {
		t.Fatalf("等待者拿到的不是一次次真实探测的结果：%+v", status)
	}
}

// TestProbeAgentsForceIsNotClobberedByOlderRound 钉住轮次序号。
//
// 场景：进页面的自动探测（第 1 轮）还在飞，用户点了「重新检查」（force，第 2 轮）；
// 第 2 轮先回、写进缓存，第 1 轮后回。没有序号时，旧那一轮只要 epoch 没变就照样写回，
// 把这份更新的读数顶掉 —— 与 probeAgentsFor 注释里"手动刷新之后，后续的自动读取应当
// 命中这份新读数"这条不变量相反。
func TestProbeAgentsForceIsNotClobberedByOlderRound(t *testing.T) {
	firstGate := make(chan struct{})
	secondGate := make(chan struct{})
	runner := &stagedProbeRunner{
		versions: []string{"1.0.0", "2.0.0"},
		gates:    []chan struct{}{firstGate, secondGate},
		entered:  make(chan struct{}, 4),
	}
	server, meta := newReadingsFixture(t, runner)

	done := make(chan struct{}, 1)
	// 第 1 轮：进页面的自动探测。等它的令牌 —— 这样第 2 轮才会确定拿到下标 1。
	go func() { server.probeAgents(context.Background(), meta); done <- struct{}{} }()
	<-runner.entered
	// 第 2 轮：用户点「重新检查」。force 不走缓存、也不等第 1 轮，直接新起一轮。
	go func() { _, _ = server.probeAgentsFor(context.Background(), meta, true); done <- struct{}{} }()
	<-runner.entered // 第 2 轮也确实进到 Version 里了（不是靠 sleep 猜的）

	close(secondGate) // 第 2 轮（force）先结束：它写进缓存
	<-done
	close(firstGate) // 第 1 轮后结束：它**不许**顶掉第 2 轮
	<-done

	// 缓存里必须是 force 那一轮的结果：紧接着的自动读取应当命中缓存、不再探。
	before := runner.probeCount()
	statuses, at := server.probeAgentsFor(context.Background(), meta, false)
	if runner.probeCount() != before {
		t.Fatal("紧跟其后的自动读取又探了一轮 —— 说明缓存没留下 force 那份读数")
	}
	if at.IsZero() {
		t.Fatal("缓存是空的：force 那一轮的结果被旧那一轮顶掉了")
	}
	if got := statusByID(statuses)["claude-code"].Version; got != "2.0.0" {
		t.Fatalf("缓存里是旧那一轮的读数（%q），force 那份被顶掉了", got)
	}
}

// TestInvalidateAgentReadingsClearsCaches 钉住失效覆盖三份缓存，且不误伤别的机器。
func TestInvalidateAgentReadingsClearsCaches(t *testing.T) {
	server := &Server{runnerRegistry: newRunnerRegistry(), runtimeCtx: context.Background()}
	now := time.Now()
	server.agentReadings = map[string]agentReading{
		"machine-a": {at: now, statuses: []AgentStatus{{ID: "claude-code", Status: agentStatusReady}}, diagnostics: &runnerDiagnosticsView{RunnerID: "machine-a"}, diagnosticsAt: now},
		"machine-b": {at: now, statuses: []AgentStatus{{ID: "claude-code", Status: agentStatusReady}}, diagnostics: &runnerDiagnosticsView{RunnerID: "machine-b"}, diagnosticsAt: now},
	}
	keyA := agentUpdateCheckKey{runnerID: "machine-a", agentID: "claude-code"}
	keyB := agentUpdateCheckKey{runnerID: "machine-b", agentID: "claude-code"}
	server.storeAgentUpdateCheck(keyA, agentUpdateCheck{at: now, latest: "1.0.0"})
	server.storeAgentUpdateCheck(keyB, agentUpdateCheck{at: now, latest: "1.0.0"})

	server.invalidateAgentReadings("machine-a")

	if _, ok := server.cachedAgentDiagnostics("machine-a"); ok {
		t.Fatal("失效后 machine-a 的诊断报告仍在")
	}
	if _, ok := server.cachedAgentUpdateCheck(keyA); ok {
		t.Fatal("失效后 machine-a 的最新版本读数仍在")
	}
	if _, ok := server.cachedAgentDiagnostics("machine-b"); !ok {
		t.Fatal("失效误伤了 machine-b 的诊断报告")
	}
	if _, ok := server.cachedAgentUpdateCheck(keyB); !ok {
		t.Fatal("失效误伤了 machine-b 的最新版本读数")
	}
}

// TestRecordAgentInstallationInvalidatesReadings 钉住"登记安装"这个漏斗点。
//
// 本机与跨端的 CLI 安装、运行时安装都经过 recordAgentInstallation，漏掉它就会留下
// "装完了界面还说没装"。
func TestRecordAgentInstallationInvalidatesReadings(t *testing.T) {
	server := newTestServer(t)
	runner := &countingProbeRunner{version: "1.0.0"}
	meta := RunnerMeta{ID: "install-invalidate", Name: "install-invalidate", Environment: "remote-linux"}
	server.runnerRegistry.register(meta.ID, runner, meta)

	server.probeAgents(context.Background(), meta)
	before := runner.probeCount()
	if before == 0 {
		t.Fatal("前置探测没有真的探过，断言证明不了任何事")
	}

	if err := server.recordAgentInstallation(context.Background(), agentInstallation{
		RunnerID: meta.ID, AgentID: "claude-code", BinaryPath: "/tmp/claude",
		InstallKind: installKindNpmManaged, Version: "1.0.0",
	}); err != nil {
		t.Fatal(err)
	}

	server.probeAgents(context.Background(), meta)
	if runner.probeCount() == before {
		t.Fatal("登记安装后读数没有作废 —— 装完了界面还会说没装")
	}
}

// TestProbeAgentsGivesUpWithCaller 钉住"调用方走了就不替它干等"。
//
// 在飞的探测属于**服务端**（它跑在服务端生命周期上、结果会进缓存给后来的人用），
// 但一个已经走掉的请求不该继续占着 goroutine 等它。
func TestProbeAgentsGivesUpWithCaller(t *testing.T) {
	runner := &blockingProbeRunner{started: make(chan struct{}), release: make(chan struct{})}
	server, meta := newReadingsFixture(t, runner)

	go func() { _ = server.probeAgents(context.Background(), meta) }()
	<-runner.started // 第一轮确实在飞

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 调用方立刻放弃
	statuses, at := server.probeAgentsFor(ctx, meta, false)
	if statuses != nil || !at.IsZero() {
		t.Fatalf("调用方已经放弃时不该还替它干等（statuses=%d, at=%v）", len(statuses), at)
	}

	close(runner.release)
}

// TestProbeAgentsKeepsCacheWhenProbeTimesOut 钉住"没查成"既不写缓存、也不清掉
// 仍然有效的旧读数。
//
// 整体超时/被取消时每个工具都会落成"不可用"——那不是事实，而是"我们没查到"。缓存住它
// 等于把一次抖动钉住一个 TTL；反过来，把已经攒下的有效读数清掉同样有害（用户会为一次
// 网络抖动丢掉一份本来还好用的读数）。
func TestProbeAgentsKeepsCacheWhenProbeTimesOut(t *testing.T) {
	runner := &countingProbeRunner{version: "1.0.0"}
	server, meta := newReadingsFixture(t, runner)

	// 先攒一份有效读数。
	if statuses := server.probeAgents(context.Background(), meta); len(statuses) == 0 {
		t.Fatal("前置探测没有拿到读数，断言证明不了任何事")
	}

	// 再跑一轮"没查成"的：让探测上下文一出生就是取消态。
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	server.runtimeCtx = cancelled
	// ⚠️ 必须 force：否则这一轮会命中上一步刚攒下的缓存、**根本不去探** ——
	// 那样这条用例就测不到"超时的结果会不会进缓存"，会变成一条恒真的假绿。
	server.probeAgentsFor(context.Background(), meta, true)
	server.runtimeCtx = context.Background()

	// 那份仍然有效的旧读数应当被原样回放：既没被"全不可用"覆盖，也没被清掉。
	before := runner.probeCount()
	statuses, at := server.probeAgentsFor(context.Background(), meta, false)
	if runner.probeCount() != before {
		t.Fatal("超时那一轮把缓存弄丢了 —— 它既不该写新值，也不该清掉仍然有效的旧读数")
	}
	if at.IsZero() {
		t.Fatal("应当回放那份仍然有效的旧读数")
	}
	// ⚠️ 这一条才是真正能区分"超时结果有没有进缓存"的断言：超时轮里替身报不出版本
	// （unavailable），所以那份结果若被写进缓存，这里读到的就是 unavailable 而不是 ready。
	if got := statusByID(statuses)["claude-code"].Status; got != agentStatusReady {
		t.Fatalf("回放的应当是超时之前那份有效读数（%q），得到 %q —— 超时那轮的结果进了缓存",
			agentStatusReady, got)
	}
}

// TestDiagnoseOneInvalidatesReadings 钉住"单个工具详查之后，缓存里那份批量报告不许
// 再盖住它"。
//
// 场景：进页面拿到一份批量报告（进了缓存）→ 用户点详情里的「重新检测这个工具」，
// 拿到一份**实测**结论 → 切走再回来。不作废的话，第二次进页面会把那份旧报告原样放回，
// 于是同一台机器上两份报告给出相反结论，而界面无从分辨该信哪个。
func TestDiagnoseOneInvalidatesReadings(t *testing.T) {
	server := newTestServer(t)
	runner := &countingProbeRunner{version: "1.0.0"}
	meta := RunnerMeta{ID: "diagnose-one", Name: "diagnose-one", Environment: "remote-linux"}
	server.runnerRegistry.register(meta.ID, runner, meta)

	// 前置：先攒一份读数缓存（模拟"进页面时探过"）。
	server.probeAgents(context.Background(), meta)
	if runner.probeCount() == 0 {
		t.Fatal("前置探测没有真的探过，断言证明不了任何事")
	}

	router := chi.NewRouter()
	router.Get("/api/runners/{runnerID}/agents/{agentID}/diagnose", server.diagnoseAgentHandler)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet,
		"/api/runners/"+meta.ID+"/agents/claude-code/diagnose", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 = %d body=%s", recorder.Code, recorder.Body.String())
	}
	// 详查自己会实测一次（buildAgentDiagnosis 里的 probeAgent），基准取这之后。
	afterDiagnose := runner.probeCount()

	// 详查是实测：缓存里那份读数不再可信，下一次自动读取必须重探。
	server.probeAgents(context.Background(), meta)
	if runner.probeCount() == afterDiagnose {
		t.Fatal("单个工具详查之后读数缓存没有作废 —— 旧报告会盖住这次详查的结论")
	}
}

// TestAgentViewReplaysSameProbedAt 钉住 HTTP 层的回放：第二次请求的读数时刻与
// 第一次**完全相同**，说明它拿到的是同一份缓存读数、而不是又探了一遍。
func TestAgentViewReplaysSameProbedAt(t *testing.T) {
	server := newTestServer(t)
	runner := &countingProbeRunner{version: "1.0.0"}
	meta := RunnerMeta{ID: "probed-at", Name: "probed-at", Environment: "remote-linux"}
	server.runnerRegistry.register(meta.ID, runner, meta)

	router := chi.NewRouter()
	router.Get("/api/runners/{runnerID}/agents", server.listRunnerAgents)

	read := func() runnerAgentsView {
		t.Helper()
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/runners/"+meta.ID+"/agents", nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("状态码 = %d body=%s", recorder.Code, recorder.Body.String())
		}
		var view runnerAgentsView
		if err := json.Unmarshal(recorder.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		return view
	}

	first := read()
	probesAfterFirst := runner.probeCount()
	second := read()

	if first.ProbedAt == nil || second.ProbedAt == nil {
		t.Fatal("响应里必须带读数时刻 —— 报告是快照，不写时间会变成此刻的事实")
	}
	if !first.ProbedAt.Equal(*second.ProbedAt) {
		t.Fatalf("两次请求的读数时刻不同（%v vs %v）—— 第二次没有命中缓存", first.ProbedAt, second.ProbedAt)
	}
	if runner.probeCount() != probesAfterFirst {
		t.Fatal("第二次请求又探了一遍 —— 缓存没生效")
	}
}

// TestCheckUpdateCachesBothOutcomes 钉住"最新版本"那一趟网络查询的缓存。
//
// 两档都要钉：成功档不许重复查，**失败档不许被回落成"已是最新"**
// （那正是本项目反复禁止的"把没查到写成没有"）。
func TestCheckUpdateCachesBothOutcomes(t *testing.T) {
	post := func(server *Server, runnerID string) *httptest.ResponseRecorder {
		t.Helper()
		recorder := httptest.NewRecorder()
		server.routes().ServeHTTP(recorder, httptest.NewRequest(
			http.MethodPost, "/api/runners/"+runnerID+"/agents/claude-code/check-update", nil))
		return recorder
	}

	t.Run("成功档只查一次", func(t *testing.T) {
		server := newTestServer(t)
		runner := &countingCheckRunner{available: true, latest: "9.9.9"}
		meta := RunnerMeta{ID: "check-ok", Name: "check-ok", Environment: "remote-linux"}
		server.runnerRegistry.register(meta.ID, runner, meta)

		first := post(server, meta.ID)
		if first.Code != http.StatusOK {
			t.Fatalf("状态码 = %d body=%s", first.Code, first.Body.String())
		}
		second := post(server, meta.ID)
		if second.Code != http.StatusOK {
			t.Fatalf("状态码 = %d body=%s", second.Code, second.Body.String())
		}
		if runner.checkCount() != 1 {
			t.Fatalf("TTL 内第二次 check-update 又查了一次 registry（共 %d 次）", runner.checkCount())
		}
		if first.Body.String() != second.Body.String() {
			t.Fatalf("回放与首查的响应不同：\n首查: %s\n回放: %s", first.Body.String(), second.Body.String())
		}
	})

	t.Run("失败档不许变成已是最新", func(t *testing.T) {
		server := newTestServer(t)
		runner := &countingCheckRunner{err: context.DeadlineExceeded}
		meta := RunnerMeta{ID: "check-fail", Name: "check-fail", Environment: "remote-linux"}
		server.runnerRegistry.register(meta.ID, runner, meta)

		first := post(server, meta.ID)
		if first.Code != http.StatusInternalServerError {
			t.Fatalf("查不到时应当报错，得到 %d body=%s", first.Code, first.Body.String())
		}
		if !strings.Contains(first.Body.String(), "error") {
			t.Fatalf("查不到必须如实带 error：%s", first.Body.String())
		}
		second := post(server, meta.ID)
		if second.Code != http.StatusInternalServerError {
			t.Fatalf("失败档回放也必须报错（不许变成 200/已是最新），得到 %d body=%s", second.Code, second.Body.String())
		}
		if first.Body.String() != second.Body.String() {
			t.Fatalf("失败档回放与首查不同：\n首查: %s\n回放: %s", first.Body.String(), second.Body.String())
		}
		if runner.checkCount() != 1 {
			t.Fatalf("失败档 TTL 内不该重复查 registry（共 %d 次）", runner.checkCount())
		}
	})
}

// TestRepairInvalidatesReadingsBeforeRerunningDiagnosis 是**顺序**断言。
//
// 修复后那份报告是拿来跟修复前那份算差集的（前端的 resolvedIssues）。失效若放在
// 重跑之后，报告就会照着**修复前**的读数写出来，"哪些症状真的没了"整条失真 ——
// 而那种错会让断言变绿而不是变红，只能靠源码顺序钉住。
func TestRepairInvalidatesReadingsBeforeRerunningDiagnosis(t *testing.T) {
	body := functionBody(t, readGoSource(t, "agent_repair.go"), "func (s *Server) repairAgentHandler(")
	invalidate := strings.Index(body, "invalidateAgentReadings(")
	rebuild := strings.Index(body, "buildAgentDiagnosis(")
	if invalidate < 0 {
		t.Fatal("修复链路没有作废读数 —— 修完了界面还会拿着修复前的读数")
	}
	if rebuild < 0 {
		t.Fatal("找不到重跑诊断那一步，这条断言失去意义")
	}
	if invalidate > rebuild {
		t.Fatal("读数作废必须发生在重跑诊断**之前**：否则这份修复后的报告会照着修复前的读数写出来")
	}
}
