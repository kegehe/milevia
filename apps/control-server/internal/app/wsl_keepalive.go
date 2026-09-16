package app

import (
	"context"
	"log"
	"os"
	"runtime"
	"time"
)

// WSL 保活。
//
// 背景（真机实测）：WSL2 发行版空闲后会自动 Stopped，之后每一次 `wsl.exe` 调用或
// `\\wsl.localhost` UNC 访问都要付冷启动代价 —— 拉起轻量虚拟机，实测数秒到十几秒。
// 控制服务里受影响的面比探针大得多：
//
//   - 探针类：就绪 / 版本 / 模型目录（已由 wslAgentRunner 的 stale-while-revalidate
//     缓存挡住，请求线程不再等待）；
//   - **完全没有超时保护**的 UNC 路径：`GET /api/directories`、文件树与文件读写、
//     skills 扫描、以及每条 30s 上限的 git 命令 —— 它们直接走 Windows 侧 os/git
//     访问 `\\wsl.localhost\<distro>\...`，冷启动时会把请求拖住，且无法被 ctx 中断。
//
// 所以除了让探针不阻塞，还需要一个统一的"唤醒点"：进程起来就把发行版拉起来，之后
// 周期性轻量 ping 维持，避免用户在空闲一段时间后第一次点开 WSL 项目时撞上冷启动。
//
// 开关：环境变量 AUTO_WSL_KEEPALIVE=0 可关闭（沿用 AUTO_DATA_DIR 等既有 env 风格）。

const (
	// wslKeepAliveEnvVar 置为 "0" 时关闭保活。
	wslKeepAliveEnvVar = "AUTO_WSL_KEEPALIVE"

	// wslKeepAliveInterval 是稳态下的保活间隔。
	//
	// 取值依据：本机实测带 systemd + docker/postgres 的发行版在无 wsl.exe / UNC 活动时
	// 仍能维持十几分钟不关机，而 WSL 的 idle 判定随版本与发行版内容变化、给不出稳定
	// 保证，所以取一个有余量的短间隔。代价只是每 3 分钟一次 `wsl.exe -e true`
	// （热态实测中位 0.13s），比冷启动一次（实测 7s 起，且会拖住所有 UNC 路径）便宜得多。
	wslKeepAliveInterval = 3 * time.Minute

	// wslKeepAliveRetryInterval 是保活失败后的快速重试间隔（用于发行版还没注册、
	// 或刚被 `wsl --shutdown` 干掉的情况）。连续失败若干次后退回稳态间隔，避免在
	// 根本没装 WSL 的机器上无休止地拉起进程。
	wslKeepAliveRetryInterval = 30 * time.Second

	// wslKeepAliveMaxFastRetries 是快速重试的次数上限。
	wslKeepAliveMaxFastRetries = 5

	// wslKeepAliveTimeout 是单次唤醒 ping 的上限，需覆盖冷启动。
	wslKeepAliveTimeout = 30 * time.Second
)

// startWSLKeepAlive 启动 WSL 保活循环。由 StartBackgroundMaintenance 调用（HTTP 监听
// 可用之后），因此不会拖慢服务就绪。非 Windows 平台直接返回。
func (s *Server) startWSLKeepAlive() {
	if runtime.GOOS != "windows" {
		return
	}
	if os.Getenv(wslKeepAliveEnvVar) == "0" {
		log.Printf("[wsl] keep-alive disabled by %s=0", wslKeepAliveEnvVar)
		return
	}
	s.runWG.Add(1)
	go func() {
		defer s.runWG.Done()
		failures := 0
		for {
			if s.wakeWSL(s.runtimeCtx) {
				failures = 0
			} else {
				failures++
			}
			wait := wslKeepAliveInterval
			if failures > 0 && failures <= wslKeepAliveMaxFastRetries {
				wait = wslKeepAliveRetryInterval
			}
			timer := time.NewTimer(wait)
			select {
			case <-s.runtimeCtx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
}

// wakeWSL 唤醒并保活 WSL 默认发行版，返回是否成功。
//
// 若此刻还没探测到发行版（启动期探测可能因冷启动超时失败），就地补注册 —— 本函数跑在
// 后台线程上，同步阻塞无害；这正是旧实现缺的那一环：`ensureWSLRunner` 只在请求期被调用，
// 一旦启动期注册失败且用户没碰 wsl-local 请求，进程存活期内就一直缺 wsl-local。
func (s *Server) wakeWSL(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	distro := s.wslDistroName()
	if distro == "" {
		s.ensureWSLRunner()
		distro = s.wslDistroName()
		if distro == "" {
			return false
		}
	}
	wslPath, err := wslExePath()
	if err != nil {
		return false // 本机没装 WSL：不当作失败刷日志
	}
	probeCtx, cancel := context.WithTimeout(ctx, wslKeepAliveTimeout)
	defer cancel()
	// `-e true` 只做一次进程创建，不碰原生 PATH（不需要 wslPathPrefix：本命令不解析
	// claude/codex），是最廉价的唤醒手段。
	if _, err := runWSLProbe(probeCtx, wslPath, "-d", distro, "-e", "true"); err != nil {
		if ctx.Err() == nil {
			log.Printf("[wsl] keep-alive ping failed (distro=%s): %v", distro, err)
		}
		return false
	}
	// 刚唤醒：把唤醒前记下的失败结果在后台重探一遍，用户请求不必等失败保鲜期过期。
	// 注意不能清空整个缓存 —— 那会让下一次请求退化成同步探测，等于把冷启动搬回请求路径。
	if runner, ok := s.wslAgentRunner().(*wslAgentRunner); ok {
		runner.refreshFailedProbes()
	}
	return true
}
