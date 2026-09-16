package app

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// wslAgentRunner 在 Windows 服务端下为 wsl-local 项目跨到 WSL 侧执行 Claude/Codex CLI。
// 它只在服务端运行于 Windows 且探测到 WSL 时被构造（见 app.go New()）。与
// windowsAgentRunner（WSL→Windows 方向）对称，本 runner 方向为 Windows→WSL。
//
// 执行能力分级：
//   - 就绪探测 / 版本 / 更新检查：经 wsl.exe -d <distro> -e sh -c '...' 真实探测，绝不
//     静默回退 Windows 侧。
//   - Claude 长驻会话（StartSession）与一次性 Run、Codex 一次性 Run：经 wsl.exe 包裹
//     claude/codex，stdin/stdout 双向管道转发，terminateProcessGroup 回收进程树。
//
// 复用 claudeCLIRunner / codexCLIRunner 的纯方法（args/sessionArgs/profileLaunch/
// readOutput/readStderr/approvalHookCommand）组装参数与解析输出，仅进程拉起部分由
// 本 runner 自行实现为 wsl.exe 版本，避免改动现有 runner 的执行路径。
type wslAgentRunner struct {
	config          Config
	distro          string           // 探测到的默认发行版，如 "Ubuntu"
	claude          *claudeCLIRunner // 复用 args/sessionArgs/profileLaunch/readOutput/readStderr
	codex           *codexCLIRunner  // 复用 codex Run 的 profileLaunch/args 组装
	codexSkillsRoot string           // WSL 用户级 Codex skills 的 Windows UNC 路径

	// background 是后台刷新用的生命周期上下文（服务端 runtimeCtx），不受任何单个请求
	// 的取消影响；为零值时退回 context.Background()（仅测试会走到）。
	background context.Context
	// probeFn 执行一次真实探测，默认是 wslBridgeProbe；抽成字段是为了让测试注入假探测。
	probeFn func(ctx context.Context, command string) (string, error)

	mu     sync.Mutex
	probes map[string]wslProbeEntry
}

// wslProbeEntry 是一次 WSL 探测的缓存条目。
type wslProbeEntry struct {
	at      time.Time // 写入时刻
	value   string    // stdout（已 TrimSpace）；就绪类探测用 ready 判定
	ready   bool      // err == nil
	has     bool      // 是否探测过：区分"从未探测"与"探测过但失败"
	command string    // 该探测键对应的命令，供保活唤醒后按需重探
	// done 非 nil 表示该键有一次探测在飞，关闭即结束。它同时充当 singleflight：
	// 同一时刻每个键最多一个 wsl.exe，无论是首次探测还是后台刷新。
	done chan struct{}
}

// fresh 报告条目是否还在保鲜期内。失败结果用更短的 TTL，免得一次冷启动超时把
// "不可用"钉住太久；成功结果可以缓存久一些，因为 CLI 是否安装几秒内不会变。
func (e wslProbeEntry) fresh(now time.Time) bool {
	if !e.has {
		return false
	}
	ttl := wslProbeFreshTTL
	if !e.ready {
		ttl = wslProbeFailureTTL
	}
	return now.Sub(e.at) < ttl
}

// 探测缓存的保鲜期与超时。
//
// 背景（真机实测）：WSL2 发行版空闲后会自动 Stopped，之后每个 wsl.exe 调用都要付
// 冷启动代价。旧实现有两条互相叠加的坑：wslBridgeProbe 的兜底超时只有 5s（冷启动
// 必然撞墙），而 cachedReady 把"失败"也按 5s 缓存；claudeVersion / codexVersion /
// codexModelCatalog 更是完全没有缓存。结果是冷启动窗口内 /api/runners、
// /api/runners/{id}/status、/api/projects/availability 一律稳定 5s 起，把前端那 6 条
// HTTP/1.1 连接占满，用户在项目页的其它请求只能排队，15s/30s 双双超时。
//
// 现在统一走 stale-while-revalidate：缓存过期但已有旧值时**立即返回旧值**并在后台
// 刷新，请求线程永不等待 WSL；只有"从未探测过"才同步探测，且用较宽的超时对接冷启动。
const (
	// wslProbeFreshTTL 是成功探测结果的保鲜期。
	wslProbeFreshTTL = 60 * time.Second
	// wslProbeFailureTTL 是失败探测结果的保鲜期，取得比成功短。
	//
	// 取舍：这是"新鲜但慢"与"陈旧但快"之间的选择。旧实现每次过期都同步重探，冷启动
	// 时每个点位都要等 5s（实测冷启动一次 wsl.exe 要 7s 以上），把前端那 6 条 HTTP/1.1
	// 连接占满 —— 那正是要修的故障。代价是缓存的 false 最多"陈旧"这么久：像
	// sendMessage 的就绪闸门（app.go）会据此回 503，用户重试一次即可（后台刷新通常
	// 在一秒内就把真值写回）。保活常驻 WSL 之后，这种冷启动窗口本身也很少见。
	wslProbeFailureTTL = 10 * time.Second
	// wslProbeFirstTimeout 是"从未探测过"时同步探测的超时，需覆盖 WSL 冷启动。
	wslProbeFirstTimeout = 20 * time.Second
	// wslProbeRefreshTimeout 是后台刷新的超时，不在请求路径上，可以给得更宽。
	wslProbeRefreshTimeout = 30 * time.Second
)

// 探测键与命令。同一个探测键只缓存一份结果：`codex --version` 同时回答"就绪吗"
// （跑得起来才算就绪）与"版本号是多少"，共用一个键，避免同一条命令探两次。
const (
	wslProbeKeyClaudeReady   = "claude-ready"
	wslProbeKeyClaudeVersion = "claude-version"
	wslProbeKeyCodexCLI      = "codex-cli"
	wslProbeKeyCodexModel    = "codex-default-model"
	wslProbeKeyCodexCatalog  = "codex-model-catalog"

	wslProbeCommandClaudeAuth = "claude auth status"
	wslProbeCommandClaudeVer  = "claude --version"
	wslProbeCommandCodexVer   = "codex --version"
	wslProbeCommandCodexModel = `cat "$HOME/.codex/config.toml" 2>/dev/null`
	wslProbeCommandCodexCat   = "codex debug models 2>/dev/null"
)

// newWSLAgentRunner 构造 WSL runner。background 是后台刷新用的生命周期上下文，
// 传 nil 时退回 context.Background()（测试用）。
func newWSLAgentRunner(config Config, distro string, background context.Context) *wslAgentRunner {
	claude := &claudeCLIRunner{config: config}
	// WSL 内 claude 通过 sh 执行审批 hook，hook 经 stdin 接收 JSON（非 argv）。
	// 把 Windows 侧 ApprovalHook 可执行文件转为 /mnt/<盘符>/... 互操作路径，WSL 默认
	// 开启互操作即可直接执行 .exe；用引号包裹以容忍路径含空格。
	if config.ApprovalHook != "" {
		hook := config.ApprovalHook
		claude.approvalHookOverride = func() string {
			return `"` + windowsToWSLMntPath(hook) + `"`
		}
	}
	runner := &wslAgentRunner{
		config:     config,
		distro:     distro,
		claude:     claude,
		codex:      &codexCLIRunner{config: config},
		background: background,
		probes:     map[string]wslProbeEntry{},
	}
	runner.probeFn = runner.wslBridgeProbe
	return runner
}

// backgroundContext 返回后台刷新应使用的上下文。
func (r *wslAgentRunner) backgroundContext() context.Context {
	if r.background != nil {
		return r.background
	}
	return context.Background()
}

// wslPathPrefix 返回在 WSL 内把原生 npm bin 目录前置到 PATH 的 shell 语句前缀。
//
// 背景：wsl.exe 不会继承 Windows 侧 PATH，而是为默认 shell 重建一份，且会把 Windows
// npm 全局目录（/mnt/c/Users/.../AppData/Roaming/npm）混进 PATH 并置于用户原生目录之前。
// 于是 WSL 内裸 `codex` 会命中 Windows 那个 npm shim——它在 Linux 平台缺
// @openai/codex-linux-x64 可选二进制，一跑即抛 "Missing optional dependency"。用户原生
// 目录（$HOME/.npm-global/bin、$HOME/.local/bin，见 WSL .bashrc/.zshrc）里的同名单文件
// 才是可在 Linux 执行的版本。探测与运行都前置这两个目录，保证"WSL 项目用 WSL 原生
// codex/claude"，不让 Windows shim 劫持。
func wslPathPrefix() string {
	return `export PATH="$HOME/.npm-global/bin:$HOME/.local/bin:$PATH"`
}

// wslBridgeProbe 在 WSL 内执行一条 sh 命令，返回 stdout 文本（UTF-8）。找不到 wsl.exe
// 或命令失败均如实返回错误，不降级为 Windows 侧。执行前先施加 wslPathPrefix，使
// claude/codex 就绪与版本探测解析到 WSL 原生二进制，而非 Windows 挂载的 npm shim。
//
// ctx 无截止时间时兜底 5s —— 调用方都应自行给出超时：探测缓存（probe）走
// wslProbeFirstTimeout / wslProbeRefreshTimeout，跨端 MCP 检查走 mcpRuntimeCheckTimeout。
// 这条兜底只防"忘了传超时"把请求挂死。
func (r *wslAgentRunner) wslBridgeProbe(ctx context.Context, command string) (string, error) {
	wslPath, err := wslExePath()
	if err != nil {
		return "", fmt.Errorf("无法访问 wsl.exe：%w", err)
	}
	probeCtx := ctx
	if _, hasDeadline := probeCtx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		probeCtx, cancel = context.WithTimeout(probeCtx, 5*time.Second)
		defer cancel()
	}
	full := wslPathPrefix() + ";" + command
	cmd := exec.CommandContext(probeCtx, wslPath, "-d", r.distro, "-e", "sh", "-c", full)
	configureProcessGroup(cmd)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("WSL 探测失败：%s: %w", strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// probe 返回某个探测键的结果，走 stale-while-revalidate：
//
//   - 缓存还在保鲜期内 → 直接返回；
//   - 已过期但探测过 → **立即返回旧值**，同时确保后台有一次刷新在飞；
//   - 从未探测过 → 起一次探测，最多等调用方自己的 deadline。
//
// 三条共同点：**探测本身跑在生命周期上下文上，永远不会被某个调用方的取消带走**。
// 这不只是省事：缓存是共享状态，如果它的产出取决于"最先发起的那个请求有多有耐心"，
// 那么冷启动时每个提前放弃的请求都会把已经跑了一半的探测丢掉，下一个调用者还得从头
// 再等一遍（wsl.exe 冷启动实测 7s 起）。调用方等不等得起只决定它自己拿到什么，不决定
// 缓存写不写、别人要不要重来。
//
// 同一个探测键同一时刻最多一个在飞（singleflight）：真机上一次探测就是一次跨端进程
// 创建，项目页一次会并发打好几个读接口，不去重就会叠成一串 wsl.exe。
func (r *wslAgentRunner) probe(ctx context.Context, key, command string) wslProbeEntry {
	cached, done := r.beginProbe(key, command)
	if done == nil {
		return *cached
	}
	// 从未探测过：必须给个答案，但只等调用方自己的预算，超了就如实说"还没结论"。
	select {
	case <-done:
		r.mu.Lock()
		result := r.probes[key]
		r.mu.Unlock()
		return result
	case <-ctx.Done():
		return wslProbeEntry{command: command}
	}
}

// beginProbe 在**同一次加锁**里决定这次调用该怎么走：命中缓存 / 返回旧值并确保后台刷新 /
// 需要等一次首次探测。
//
// 读条目与发起探测必须落在同一把锁内。分两次加锁的话，两次之间别的 goroutine 可能已经
// 探完并把结果写进缓存，这里却又拉起一次多余的 wsl.exe —— 这正是偶发多探一次的根因
// （TestWSLProbeCacheSingleflightsFirstProbe 钉的就是它，单跑 -race 十次才会撞上一次）。
// done 为 nil 时返回的是可直接使用的缓存快照。
func (r *wslAgentRunner) beginProbe(key, command string) (*wslProbeEntry, chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := r.probes[key]
	if entry.fresh(time.Now()) {
		return &entry, nil
	}
	if entry.has {
		r.startProbeLocked(key, command, wslProbeRefreshTimeout)
		return &entry, nil
	}
	return nil, r.startProbeLocked(key, command, wslProbeFirstTimeout)
}

// startProbe 确保该探测键有一次在飞的探测，返回它的完成信号；已有在飞的直接复用。
func (r *wslAgentRunner) startProbe(key, command string, timeout time.Duration) chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.startProbeLocked(key, command, timeout)
}

// startProbeLocked 是 startProbe 的"调用方已持锁"版本：起一个后台探测（backgroundContext
// 生命周期上下文，超时由 timeout 控制），结束后清掉在飞标记、关闭 done 并唤醒等待者。
// 写缓存在关闭 done 之前完成，等到的调用者一定能看到新值。
func (r *wslAgentRunner) startProbeLocked(key, command string, timeout time.Duration) chan struct{} {
	entry := r.probes[key]
	if entry.done != nil {
		return entry.done
	}
	done := make(chan struct{})
	entry.done = done
	if entry.command == "" {
		entry.command = command
	}
	r.probes[key] = entry

	go func() {
		// 收尾用 defer：无论如何都要清掉在飞标记并放行等待者，否则这个探测键会永久卡在
		// "在飞"状态，之后每次调用都只能等到自己的 deadline。
		defer func() {
			r.mu.Lock()
			current := r.probes[key]
			current.done = nil
			r.probes[key] = current
			r.mu.Unlock()
			close(done)
		}()
		r.runProbe(r.backgroundContext(), key, command, timeout)
	}()
	return done
}

// runProbe 同步执行一次探测并写回缓存。timeout 与 ctx 的 deadline 取更早的那个。
//
// ctx 只应是生命周期上下文（见 startProbeLocked）。它到期意味着"这次没探出结论"，
// 而不是"探测对象不可用"，所以这种情况不写缓存 —— 否则服务停机时的一次半途而废，
// 会在失败保鲜期内把探测键钉成"不可用"。
func (r *wslAgentRunner) runProbe(ctx context.Context, key, command string, timeout time.Duration) wslProbeEntry {
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	value, err := r.probeFn(probeCtx, command)
	entry := wslProbeEntry{at: time.Now(), value: strings.TrimSpace(value), ready: err == nil, has: true, command: command}
	if err != nil && ctx.Err() != nil {
		return entry
	}
	r.mu.Lock()
	// 保留并发置上的在飞标记：它归 startProbe 的收尾清，这里不覆盖。
	entry.done = r.probes[key].done
	r.probes[key] = entry
	r.mu.Unlock()
	return entry
}

// refreshFailedProbes 把此前失败的探测在后台重探一遍，供保活刚唤醒发行版之后调用。
//
// 刻意**只动失败的条目**、且不阻塞调用方：
//   - 清空整个缓存是不行的 —— 下一次请求就会从"命中缓存"退化成同步探测，把 WSL 冷启动
//     的时间又搬回请求路径上，正好是我们整套改动要消灭的东西；
//   - 成功的条目本来就还在保鲜期内，重探只是白拉起 wsl.exe。
//
// 唤醒前那些失败结果会被重探覆盖，用户不需要等 10s 的失败保鲜期自然过期。
func (r *wslAgentRunner) refreshFailedProbes() {
	// 先取快照再重探：逐个重探后若又失败，条目仍是"失败"状态，边遍历边补会变成
	// 后台无限重探。一次唤醒只重探一轮，下一轮由下一次保活触发。
	r.mu.Lock()
	pending := make(map[string]string)
	for key, entry := range r.probes {
		if entry.ready || entry.done != nil || entry.command == "" {
			continue
		}
		pending[key] = entry.command
	}
	r.mu.Unlock()
	// startProbe 自带 singleflight，重复触发不会叠加探测。
	for key, command := range pending {
		r.startProbe(key, command, wslProbeRefreshTimeout)
	}
}

// codexDefaultModel 返回 WSL 内 cli_managed（无档案模型）Codex 将使用的默认模型。
// WSL 内 codex 默认读 $HOME/.codex/config.toml；经 wslBridgeProbe 原样 cat 后按顶层
// model 键解析。读取/解析失败返回空串，调用方回退到仅显示工具名。探测缓存见 probe。
func (r *wslAgentRunner) codexDefaultModel(ctx context.Context) string {
	entry := r.probe(ctx, wslProbeKeyCodexModel, wslProbeCommandCodexModel)
	if !entry.ready {
		return ""
	}
	return codexModelFromConfig([]byte(entry.value))
}

// codexModelCatalog 列出 WSL 内 Codex 的模型目录（modelCatalogRunner 实现）。
// 与 codexDefaultModel 一样经 wslBridgeProbe 拉起 WSL 内进程，失败即由调用方回退静态表。
func (r *wslAgentRunner) codexModelCatalog(ctx context.Context) ([]AgentModelOption, error) {
	entry := r.probe(ctx, wslProbeKeyCodexCatalog, wslProbeCommandCodexCat)
	if !entry.ready {
		return nil, errors.New("codex debug models unavailable in WSL")
	}
	options := parseCodexModelCatalog([]byte(entry.value))
	if len(options) == 0 {
		return nil, errors.New("codex debug models returned no usable model (wsl)")
	}
	return options, nil
}

func (r *wslAgentRunner) claudeReady(ctx context.Context) bool {
	// 与原版 claudeCLIRunner.Ready 语义一致：验证 WSL 内 claude 已安装且已登录
	// （auth status 可执行）。仅测 command -v 会误报未登录为就绪。
	return r.probe(ctx, wslProbeKeyClaudeReady, wslProbeCommandClaudeAuth).ready
}

func (r *wslAgentRunner) codexReady(ctx context.Context) bool {
	// 仅测 command -v 不够：WSL 的 PATH 混入的 Windows 挂载 npm shim 也能被找到，但它在
	// Linux 平台缺 codex-linux-x64，无法真正运行。故在施加 wslPathPrefix 后代真跑一遍
	// `codex --version`，能跑才判就绪，避免"假就绪→一跑就崩"。需先探测到默认发行版，
	// 否则 wslBridgeProbe 必失败、如实返回不可用。
	return r.probe(ctx, wslProbeKeyCodexCLI, wslProbeCommandCodexVer).ready
}

func (r *wslAgentRunner) claudeVersion(ctx context.Context) string {
	// 输出形如 "2.1.218 (Claude Code)"，剥离后缀与原版 Version 一致。
	return strings.TrimSuffix(r.probe(ctx, wslProbeKeyClaudeVersion, wslProbeCommandClaudeVer).value, " (Claude Code)")
}

func (r *wslAgentRunner) codexVersion(ctx context.Context) string {
	return r.probe(ctx, wslProbeKeyCodexCLI, wslProbeCommandCodexVer).value
}

// Ready implements AgentRunner（Claude 就绪，用于 claude 分发与 createProject 校验）。
func (r *wslAgentRunner) Ready(parent context.Context) bool { return r.claudeReady(parent) }

// CodexReady implements CodexCapableRunner，供 codexRunnerFor(wsl) 调用处断言。
func (r *wslAgentRunner) CodexReady(parent context.Context) bool { return r.codexReady(parent) }

// Version implements AgentRunner（Claude 版本）。
func (r *wslAgentRunner) Version(parent context.Context) string { return r.claudeVersion(parent) }

// CodexVersion implements CodexCapableRunner。
func (r *wslAgentRunner) CodexVersion(parent context.Context) string { return r.codexVersion(parent) }

// CheckUpdate implements AgentRunner。与 claudeCLIRunner / sshRunner 语义一致：先取 WSL
// 内探测到的本机版本，再查 npm registry 最新版并比较。npm registry 版本号跨平台唯一，
// 故查询在本机执行即可（见 latestNpmPackageVersion），不用跨界再拉起一次 wsl.exe。
func (r *wslAgentRunner) CheckUpdate(parent context.Context) (bool, string, error) {
	local := normalizeClaudeVersion(r.claudeVersion(parent))
	if local == "" {
		return false, "", errors.New("WSL 内未安装 Claude Code")
	}
	latest, err := latestNpmPackageVersion(parent, "@anthropic-ai/claude-code")
	if err != nil {
		return false, "", err
	}
	return latest != local, latest, nil
}

// CodexCheckUpdate implements CodexCapableRunner，与 codexCLIRunner 的语义一致。
func (r *wslAgentRunner) CodexCheckUpdate(parent context.Context) (bool, string, error) {
	local := normalizeCodexVersion(r.codexVersion(parent))
	if local == "" {
		return false, "", errors.New("WSL 内未安装 Codex CLI")
	}
	latest, err := latestNpmPackageVersion(parent, "@openai/codex")
	if err != nil {
		return false, "", err
	}
	available, err := codexUpdateAvailable(local, latest)
	if err != nil {
		return false, latest, err
	}
	return available, latest, nil
}

// AutoUpdateSupported implements autoUpdateSupportedRunner。跨端（Windows→WSL）升级
// 尚未就绪（见 Update），如实告知调用方不支持应用内自动升级。
func (r *wslAgentRunner) AutoUpdateSupported() bool { return false }

// CodexAutoUpdateSupported implements codexAutoUpdateSupportedRunner。
func (r *wslAgentRunner) CodexAutoUpdateSupported() bool { return false }

// Update implements AgentRunner。跨端升级如实提示降级，不伪造成功。
func (r *wslAgentRunner) Update(parent context.Context) (string, string, error) {
	return "", "", errors.New("跨端（Windows→WSL）升级 Claude Code 尚未就绪，请在 WSL 内自行更新")
}

// CodexUpdate implements CodexCapableRunner。
func (r *wslAgentRunner) CodexUpdate(parent context.Context) (string, string, error) {
	return "", "", errors.New("跨端（Windows→WSL）升级 Codex 尚未就绪，请在 WSL 内自行更新")
}

// Run implements AgentRunner。经 wsl.exe 在 WSL 内执行 Claude/Codex 一次性会话。
// 详见 wslAgentRun。
func (r *wslAgentRunner) Run(ctx context.Context, request AgentRunRequest, sink AgentRunSink) error {
	return wslAgentRun(ctx, r, request, sink)
}

// StartSession implements StreamingAgentRunner。经 wsl.exe 在 WSL 内启动 Claude
// 长驻会话。详见 wslAgentStartSession。
func (r *wslAgentRunner) StartSession(ctx context.Context, request AgentSessionRequest) (AgentSession, error) {
	return wslAgentStartSession(ctx, r, request)
}
