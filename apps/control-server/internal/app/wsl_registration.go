package app

import (
	"context"
	"log"
	"path/filepath"
	"runtime"
	"time"
)

// wslEnsureRetryInterval 是 wsl-local 补注册失败后的重试冷却。对话页/runner 列表等会高频
// 轮询（每 5s），若 WSL 长期不可用又无冷却，每次轮询都会尝试拉起 wsl.exe 探测。
const wslEnsureRetryInterval = 30 * time.Second

// registerWSLRunner 探测默认 WSL 发行版与 home，并把 wsl-local runner 注册进 registry。
// 探测成功才写 wslRunner/wslDistro/wslHome 并注册；失败返回原因（由启动 / 补注册统一打日志）。
// 字段写入经 wslMu 串行化——补注册可能发生在请求期，须与并发读者（wslAgentRunner()、
// discoverLocalSkillRoots、terminal.go 的 wslDistro 读取）错开；runner 在写字段后才发布到
// registry，读者看到注册时字段必已就绪。
func (s *Server) registerWSLRunner(ctx context.Context) error {
	distro, err := detectDefaultWSLDistro(ctx)
	if err != nil {
		return err
	}
	home, err := detectWSLHome(ctx, distro)
	if err != nil {
		return err
	}
	wslRunner := newWSLAgentRunner(s.config, distro, s.runtimeCtx)
	wslRunner.codexSkillsRoot = filepath.Join(wslToUncPath(home, distro), ".codex", "skills")
	s.wslMu.Lock()
	s.wslRunner = wslRunner
	s.wslDistro = distro
	s.wslHome = home
	s.wslMu.Unlock()
	s.runnerRegistry.register("wsl-local", wslRunner, s.wslLocalRunnerMeta(distro, home))
	log.Printf("[wsl] registered wsl-local runner (distro=%s home=%s)", distro, home)
	return nil
}

// ensureWSLRunner 确保 Windows 服务端已注册 wsl-local runner（仅 Windows 生效）。
//
// 背景：启动期探测可能因 WSL 冷启动超过超时而失败（见 app.go New()），而注册只发生一次，
// 此后进程生命周期内 wsl-local 缺失，导致 wsl-local 项目的新会话/文件操作报"无可用 CLI"。
// 本方法让需要 wsl-local 的请求有机会按需补探测并注册，无需重启应用。
//
// 并发与节流：registry 检查是廉价快路径；未注册时用 wslProbeMu 串行化探测（同一时刻仅一个
// wsl.exe 探测）。wslProbeAt 记录"探测结束时刻"（含失败），失败后按 wslEnsureRetryInterval
// 冷却，避免高频轮询反复拉起 wsl.exe；在探测进行期间排队等待锁的请求拿到锁时会看到刚更新的
// wslProbeAt 而不再重试，不会连环重探。探测使用独立超时上下文而非请求 ctx，避免客户端断开
// 导致半途取消、冷却空转。
func (s *Server) ensureWSLRunner() {
	if runtime.GOOS != "windows" {
		return
	}
	if _, ok := s.runnerRegistry.getMeta("wsl-local"); ok {
		return
	}
	s.wslProbeMu.Lock()
	defer s.wslProbeMu.Unlock()
	if _, ok := s.runnerRegistry.getMeta("wsl-local"); ok {
		return // 等待锁期间其他请求已注册
	}
	if !s.wslProbeAt.IsZero() && time.Since(s.wslProbeAt) < wslEnsureRetryInterval {
		return // 冷却期内不再拉起 wsl.exe
	}
	// 用 runtimeCtx 而不是 context.Background()：这条探测最坏要吃满 60s，而补注册可能
	// 发生在后台保活线程里 —— 挂在 runWG 上的 goroutine 若在这里阻塞，Close() 的
	// runWG.Wait() 就得跟着等一分钟。仍然不是请求 ctx，客户端断开不会半途取消。
	probeCtx, cancel := context.WithTimeout(s.runtimeCtx, 2*wslDiscoveryProbeTimeout)
	defer cancel()
	err := s.registerWSLRunner(probeCtx)
	// 无论成败都在结束后记录，冷却从本次探测完成算起：长时间探测（冷启动）结束后，
	// 排队等待的请求看到的是"刚刚探测过"，不会立刻再触发一轮。
	s.wslProbeAt = time.Now()
	if err != nil {
		log.Printf("[wsl] lazy wsl-local registration failed, will retry in %s: %v", wslEnsureRetryInterval, err)
	}
}

// wslUserHomeDistro 返回探测到的 WSL 发行版与 home（RLock 保护）。wsl-local 可能在请求期
// 补注册并再次写入字段，读取方不可直接无锁读 wslHome/wslDistro。
func (s *Server) wslUserHomeDistro() (home, distro string) {
	s.wslMu.RLock()
	defer s.wslMu.RUnlock()
	return s.wslHome, s.wslDistro
}

// wslDistroName 返回探测到的默认 WSL 发行版名，供终端启动等场景读取（RLock 保护）。
func (s *Server) wslDistroName() string {
	s.wslMu.RLock()
	defer s.wslMu.RUnlock()
	return s.wslDistro
}
