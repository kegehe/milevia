package app

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingProbe 是一个可注入的假探测：记录调用次数，按脚本返回结果。
// 像真实的 wslBridgeProbe（exec.CommandContext）一样尊重 ctx —— 否则测不出
// "调用方 deadline 是否真的兜住了同步探测"。
type countingProbe struct {
	calls int32
	gate  chan struct{} // 非 nil 时，每次探测先等这个 channel
	value string
	err   error
}

func (p *countingProbe) run(ctx context.Context, command string) (string, error) {
	atomic.AddInt32(&p.calls, 1)
	if p.gate != nil {
		select {
		case <-p.gate:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return p.value, p.err
}

func (p *countingProbe) callCount() int32 { return atomic.LoadInt32(&p.calls) }

func newProbeTestRunner(p *countingProbe) *wslAgentRunner {
	runner := newWSLAgentRunner(Config{}, "Ubuntu", nil)
	runner.probeFn = p.run
	return runner
}

// 从未探测过时必须同步探测一次并写入缓存 —— 这是唯一允许阻塞请求线程的分支。
func TestWSLProbeCacheFirstCallProbesSynchronously(t *testing.T) {
	probe := &countingProbe{value: "codex-cli 0.153.2"}
	runner := newProbeTestRunner(probe)

	if !runner.codexReady(context.Background()) {
		t.Fatal("first probe should report ready")
	}
	if got := runner.codexVersion(context.Background()); got != "codex-cli 0.153.2" {
		t.Fatalf("version = %q, want %q", got, "codex-cli 0.153.2")
	}
	// 就绪与版本共用同一个探测键，第二次调用必须命中缓存。
	if got := probe.callCount(); got != 1 {
		t.Fatalf("probe calls = %d, want 1 (codex ready/version share one key)", got)
	}
}

// 保鲜期内不重新探测。
func TestWSLProbeCacheFreshEntrySkipsProbe(t *testing.T) {
	probe := &countingProbe{value: "codex-cli 1.0.0"}
	runner := newProbeTestRunner(probe)
	runner.codexVersion(context.Background())
	before := probe.callCount()

	for i := 0; i < 5; i++ {
		runner.codexVersion(context.Background())
	}
	if got := probe.callCount(); got != before {
		t.Fatalf("fresh cache should not re-probe: calls=%d want=%d", got, before)
	}
}

// 核心语义：缓存过期但已有旧值时，**立即返回旧值**并在后台刷新 —— 请求线程绝不等待 WSL。
// 这正是冷启动（WSL2 空闲自动关机后每次 wsl.exe 要重启发行版）不再拖垮请求的关键。
func TestWSLProbeCacheStaleValueReturnedWithoutBlocking(t *testing.T) {
	probe := &countingProbe{value: "codex-cli 1.0.0"}
	runner := newProbeTestRunner(probe)
	runner.codexVersion(context.Background())

	// 把条目改成过期，并让下一次真实探测卡住，便于断言请求线程没有被拖住。
	staleValue := "codex-cli 1.0.0"
	gate := make(chan struct{})
	probe.gate = gate
	probe.value = "codex-cli 2.0.0"
	runner.mu.Lock()
	entry := runner.probes[wslProbeKeyCodexCLI]
	entry.at = time.Now().Add(-2 * wslProbeFreshTTL)
	runner.probes[wslProbeKeyCodexCLI] = entry
	runner.mu.Unlock()

	started := time.Now()
	got := runner.codexVersion(context.Background())
	elapsed := time.Since(started)
	if got != staleValue {
		t.Fatalf("stale hit returned %q, want old value %q", got, staleValue)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("stale hit blocked the caller for %s; it must return immediately", elapsed)
	}

	// 放开后台刷新，缓存应更新为新值。
	close(gate)
	deadline := time.Now().Add(20 * time.Second)
	for {
		runner.mu.Lock()
		updated := runner.probes[wslProbeKeyCodexCLI].value
		runner.mu.Unlock()
		if updated == "codex-cli 2.0.0" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("background refresh did not update cache, still %q", updated)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// 后台刷新同一时刻只跑一次：一串并发调用不应各自拉起一个 wsl.exe。
func TestWSLProbeCacheCoalescesBackgroundRefresh(t *testing.T) {
	probe := &countingProbe{value: "codex-cli 1.0.0"}
	runner := newProbeTestRunner(probe)
	runner.codexVersion(context.Background())
	probe.gate = make(chan struct{})
	probe.value = "codex-cli 2.0.0"
	runner.mu.Lock()
	entry := runner.probes[wslProbeKeyCodexCLI]
	entry.at = time.Now().Add(-2 * wslProbeFreshTTL)
	runner.probes[wslProbeKeyCodexCLI] = entry
	runner.mu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runner.codexVersion(context.Background())
		}()
	}
	wg.Wait()
	// 后台刷新是 goroutine 里发起的，它可能还没跑到 probe 本体；等它进到调用计数再断言。
	deadline := time.Now().Add(20 * time.Second)
	for atomic.LoadInt32(&probe.calls) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("probe calls = %d, want 2 (refresh never started)", atomic.LoadInt32(&probe.calls))
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(probe.gate)
	// 刷新仍然只有一个在飞：并发调用不会各自拉起一个 wsl.exe。
	time.Sleep(300 * time.Millisecond)
	if got := probe.callCount(); got != 2 {
		t.Fatalf("probe calls = %d, want 2 (initial + one coalesced refresh)", got)
	}
}

// 失败结果的保鲜期比成功短：一次冷启动超时不能把"不可用"钉住太久。
func TestWSLProbeCacheFailureExpiresSooner(t *testing.T) {
	probe := &countingProbe{err: errors.New("cold start timeout")}
	runner := newProbeTestRunner(probe)
	if runner.claudeReady(context.Background()) {
		t.Fatal("failed probe must not report ready")
	}

	runner.mu.Lock()
	entryAt := runner.probes[wslProbeKeyClaudeReady].at
	runner.mu.Unlock()
	if time.Since(entryAt) > time.Second {
		t.Fatalf("entry timestamp not recorded on failure: %v", entryAt)
	}
	if wslProbeFailureTTL >= wslProbeFreshTTL {
		t.Fatalf("failure TTL %s must be shorter than success TTL %s", wslProbeFailureTTL, wslProbeFreshTTL)
	}

	// 失败保鲜期刚过即视为过期：返回旧的 false，同时后台重新探测。
	probe.err = nil
	probe.value = "codex-cli 3.0.0"
	runner.mu.Lock()
	entry := runner.probes[wslProbeKeyClaudeReady]
	entry.at = time.Now().Add(-2 * wslProbeFailureTTL)
	entry.ready = false
	runner.probes[wslProbeKeyClaudeReady] = entry
	runner.mu.Unlock()

	if runner.claudeReady(context.Background()) {
		t.Fatal("stale failure must still be reported as not ready")
	}
	deadline := time.Now().Add(20 * time.Second)
	for atomic.LoadInt32(&probe.calls) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("stale failure did not trigger a background refresh")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// 保活唤醒后只重探失败的条目：成功的条目必须原样留着 —— 清空缓存会让下一次请求退化成
// 同步探测，等于把 WSL 冷启动的时间又搬回请求路径上。
func TestWSLProbeCacheRefreshFailedProbesKeepsGoodEntries(t *testing.T) {
	probe := &countingProbe{value: "codex-cli 1.0.0"}
	runner := newProbeTestRunner(probe)
	if !runner.codexReady(context.Background()) {
		t.Fatal("expected codex to be ready")
	}
	goodCalls := probe.callCount()

	// 再让 claude 探测失败一次（同一把 probeFn，用返回值区分不方便，改用一个会失败的 runner）。
	failing := newProbeTestRunner(&countingProbe{err: errors.New("wsl stopped")})
	if failing.claudeReady(context.Background()) {
		t.Fatal("expected claude to be unavailable")
	}

	// 唤醒之后：codex 的成功条目不再重探。
	runner.refreshFailedProbes()
	time.Sleep(200 * time.Millisecond)
	if got := probe.callCount(); got != goodCalls {
		t.Fatalf("probe calls = %d, want %d (successful entries must not be re-probed)", got, goodCalls)
	}

	// 而失败条目会被重探：失败保鲜期内本不会重探，refreshFailedProbes 绕过它。
	okProbe := &countingProbe{value: "2.1.268 (Claude Code)"}
	failing.probeFn = okProbe.run
	failing.refreshFailedProbes()
	deadline := time.Now().Add(20 * time.Second)
	for !failing.claudeReady(context.Background()) {
		if time.Now().After(deadline) {
			t.Fatal("failed entry was not re-probed after the wake-up")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// 唤醒时若一条命令仍失败，不能演变成后台无限重探。
func TestWSLProbeCacheRefreshFailedProbesDoesNotLoop(t *testing.T) {
	probe := &countingProbe{err: errors.New("wsl stopped")}
	runner := newProbeTestRunner(probe)
	runner.claudeReady(context.Background())
	runner.refreshFailedProbes()

	// 自我触发的循环会在上一次探测结束的瞬间就再拉一次，这里用探测速度（毫秒级）的
	// 若干倍做观察窗口即可，不必久等。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	// 首次同步探测 1 次 + 一次唤醒重探 1 次；不该继续自我触发。
	if got := probe.callCount(); got > 2 {
		t.Fatalf("probe calls = %d, want at most 2 (refresh must not re-trigger itself)", got)
	}
}

// 同步探测必须被**调用方**的 ctx 兜住：项目连通性探测给的是 5s 预算，不能被一条冷启动
// 的 20s 探测拖垮。
func TestWSLProbeCacheSyncProbeHonoursCallerDeadline(t *testing.T) {
	probe := &countingProbe{value: "codex-cli 1.0.0", gate: make(chan struct{})}
	runner := newProbeTestRunner(probe)

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	started := time.Now()
	if runner.codexReady(ctx) {
		t.Fatal("probe blocked past the caller deadline cannot report ready")
	}
	elapsed := time.Since(started)
	if elapsed > 2*time.Second {
		t.Fatalf("sync probe took %s; it must be bounded by the caller's 80ms deadline", elapsed)
	}
	close(probe.gate)
}

// 调用方提前放弃，**不能**把共享的那次探测带走：缓存是共享状态，它的产出不该由
// "最先发起的那个请求多有耐心"决定 —— 否则冷启动时每次超时重试都要从头再等一遍。
func TestWSLProbeCacheCallerAbortDoesNotCancelSharedProbe(t *testing.T) {
	probe := &countingProbe{value: "codex-cli 1.0.0", gate: make(chan struct{})}
	runner := newProbeTestRunner(probe)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if runner.codexReady(ctx) {
		t.Fatal("caller's budget expired before the probe finished; it must report not-ready")
	}

	// 这次探测还在飞（有在飞标记、没有结论），而不是被取消掉。
	runner.mu.Lock()
	entry := runner.probes[wslProbeKeyCodexCLI]
	runner.mu.Unlock()
	if entry.done == nil {
		t.Fatal("shared probe should still be in flight after the caller gave up")
	}
	if entry.has {
		t.Fatal("no result should be recorded yet")
	}

	// 放开闸门：探测自行跑完并写进缓存，下一个调用者直接命中，不必重来。
	close(probe.gate)
	deadline := time.Now().Add(20 * time.Second)
	for {
		runner.mu.Lock()
		done := runner.probes[wslProbeKeyCodexCLI].ready
		runner.mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("shared probe did not finish after the caller gave up")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !runner.codexReady(context.Background()) {
		t.Fatal("next caller should hit the freshly written cache")
	}
	if got := probe.callCount(); got != 1 {
		t.Fatalf("probe calls = %d, want 1 (one shared probe, no restart)", got)
	}
}

// 同一个探测键并发首次调用只拉起一个 wsl.exe。
func TestWSLProbeCacheSingleflightsFirstProbe(t *testing.T) {
	probe := &countingProbe{value: "codex-cli 1.0.0", gate: make(chan struct{})}
	runner := newProbeTestRunner(probe)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 每个调用者自带 200ms 预算，探测本身在闸门后面。
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			runner.codexVersion(ctx)
		}()
	}
	deadline := time.Now().Add(20 * time.Second)
	for atomic.LoadInt32(&probe.calls) < 1 {
		if time.Now().After(deadline) {
			t.Fatal("shared probe never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(probe.gate)
	wg.Wait()
	if got := probe.callCount(); got != 1 {
		t.Fatalf("probe calls = %d, want 1 (first probe must be singleflighted)", got)
	}
}
