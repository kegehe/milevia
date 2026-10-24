package app

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// 目录驱动的跨端工具后端。
//
// 为什么要有这一层：管理页要为目录里的**每个**工具回答"装了没、什么版本、能不能升"，
// 而跨端（WSL / SSH）真正共通的东西只有一样 —— 能在目标环境里跑一段 shell。探测命令、
// 升级包、就绪判据全都写在 `AgentCatalogEntry` 里。此前每个工具都要在 runner 上各加一组
// 方法（Claude 一组、Codex 一组），加第三个工具就得再加一组；`agent_probe.go` 的
// agentBackend 注释把这处耦合记为"会在引入 Runtime 适配层时收敛"，并明令**不要继续扩散**。
//
// 现在：**目录驱动是默认，逐工具特化只留给真有额外语义的那一档**（Codex 的就绪还要看
// 登录态；Claude 的跨端探测键已被 WSL 的读数缓存与保活唤醒依赖）。于是目录里新增一个
// npm 全局分发的工具，跨端这一侧一行代码都不用加。
//
// ⚠️ 这里**任何工具名都不许出现**。出现一个，就等于把目录之外的清单又开了一份 ——
// 而目录化的全部意义就是"这份清单只有一处"（agent_catalog.go 文件头）。

// crossShellRunner 是"能在目标环境里跑 shell"的 runner（wslAgentRunner / sshRunner）。
//
// 它是 catalogAgentBackend 唯一需要 runner 提供的东西：怎么连过去、命令怎么落地是各端
// 自己的事，这里只关心"把这条命令跑起来、把 stdout 拿回来"。
type crossShellRunner interface {
	// crossProbe 在目标环境执行 `name args...`，返回 stdout 与该命令是否成功。
	//
	// 命令名与参数**分开传**，而不是给一整条命令行：怎么解析这个名字是各端自己的事，
	// 而 WSL 那端必须确认它解析到的是**这台机器上**的那一份（见
	// wslNativeProbeCommand：经 /mnt/ 命中的是 Windows 的程序）。
	//
	// **实现方应当缓存**：/api/runners 会为目录里的每个工具各调一次，而跨端拉起一次
	// 进程的代价是 WSL 冷启动 7s 起（见 wslAgentRunner 的读数缓存，那里记着不缓存
	// 会怎样把请求线程占死）。
	crossProbe(ctx context.Context, name string, args ...string) (string, bool)
	// crossRun 在目标环境跑一段脚本，用于升级这类短命令（crossCommandRunner 的形状）。
	crossRun(ctx context.Context, script string) (string, error)
	// crossWhere 是"在哪台机器上"的说法，只用于报错文案（"WSL 内" / "远程服务器上"）。
	crossWhere() string
}

// freshProbeRunner 是**可选**的：能不能绕过读数缓存取一次真实读数。
//
// 只有 WSL 侧需要它（SSH 侧本来每次都是真探）。升级那条路上的**每一次**取版本
// （升级前、健康检查、回滚后的核对）都必须走它，理由见 catalogAgentBackend.freshVersion。
//
// ⚠️ 实现方要是把这个方法的名字或签名写歪了，可选接口断言会**静默失败** —— 表现是
// "升级的健康检查又读回旧值"，与不做这个修复一模一样。所以实现方要挂一条
// `var _ freshProbeRunner = ...` 的编译期断言（wslAgentRunner 已挂）。
type freshProbeRunner interface {
	crossProbeFresh(ctx context.Context, name string, args ...string) (string, bool)
}

// catalogAgentBackend 把一个目录条目接成 AgentRunner：探测、查新版、升级全由目录数据驱动。
//
// 它不带任何状态，也不缓存任何东西：探测的缓存在 runner 那一侧（那是唯一知道"跨一次界
// 有多贵"的地方）。
type catalogAgentBackend struct {
	entry AgentCatalogEntry
	shell crossShellRunner
}

var (
	_ AgentRunner               = (*catalogAgentBackend)(nil)
	_ autoUpdateSupportedRunner = (*catalogAgentBackend)(nil)
	_ loginAgentRunner          = (*catalogAgentBackend)(nil)
	_ authStateRunner           = (*catalogAgentBackend)(nil)
)

// probeVersion 跑目录里那条版本命令，返回归一化后的版本与这条命令是否成功。
//
// Version 与 Ready 共用它：两条读数是**同一个事实的两面**，各拼一份迟早会出现
// "卡片说未安装、详情说版本是 X"这种同屏两个结论。
func (b *catalogAgentBackend) probeVersion(ctx context.Context) (string, bool) {
	version, _, ok := b.probeVersionWithCommand(ctx, false)
	return version, ok
}

// probeCommand 依次用主命令与别名在目标环境执行同一条命令。
//
// 返回真正成功的那个命令名：原生分发的工具可能只装出了别名（Cursor 的 agent），
// 后续升级必须用**同一个名字**，否则升级会去找一个不存在的入口。
func (b *catalogAgentBackend) probeCommand(ctx context.Context, args []string, fresh bool) (string, string, bool) {
	for _, name := range agentCommandNames(b.entry) {
		var out string
		var ok bool
		if fresh {
			if f, okFresh := b.shell.(freshProbeRunner); okFresh {
				out, ok = f.crossProbeFresh(ctx, name, args...)
			} else {
				out, ok = b.shell.crossProbe(ctx, name, args...)
			}
		} else {
			out, ok = b.shell.crossProbe(ctx, name, args...)
		}
		if ok {
			return out, name, true
		}
	}
	return "", "", false
}

// probeVersionWithCommand 多返回一列"实际命中的命令名"，供原生升级使用。
func (b *catalogAgentBackend) probeVersionWithCommand(ctx context.Context, fresh bool) (string, string, bool) {
	out, command, ok := b.probeCommand(ctx, b.entry.VersionArgs, fresh)
	if !ok {
		return "", "", false
	}
	// 归一化交给唯一那处实现：各家输出形状并不一致（`2.1.293 (Claude Code)`、
	// `codex-cli 0.153.2`、`2.162.0`）。
	return agentVersionFromOutput(out), command, true
}

// Version 在目标环境跑目录声明的版本命令。读不到就是空串（"没装"与"版本未知"由调用方
// 按上下文分开说，这里不替它们圆场）。
func (b *catalogAgentBackend) Version(ctx context.Context) string {
	version, _ := b.probeVersion(ctx)
	return version
}

// Ready 报告"装上了且真能跑起来"。
//
// **不能只测 `command -v`**：能解析到名字不等于跑得起来。所以判据与版本探测共用同一条
// 命令 —— 能报出版本才算就绪。哪些"解析得到但不算数"的情形由各端的 crossProbe 兜住
// （WSL 那一档就是经 /mnt/ 命中的 Windows 程序，见 wslNativeProbeCommand）。
func (b *catalogAgentBackend) Ready(ctx context.Context) bool {
	version, ok := b.probeVersion(ctx)
	return ok && version != ""
}

// CheckUpdate 与其余 runner 同一条判据：本机版本 → registry 最新版 → **semver 比较**
// （不是字符串不等：装了比 registry 更新的预发布版时，字符串不等会把降级报成"有更新"）。
//
// registry 查询在控制服务这一侧执行（npm registry 的版本号跨平台唯一），不必为此再跨一次界。
func (b *catalogAgentBackend) CheckUpdate(ctx context.Context) (bool, string, error) {
	local := b.Version(ctx)
	if local == "" {
		return false, "", fmt.Errorf("%s未安装 %s", b.shell.crossWhere(), b.entry.Name)
	}
	latest, err := latestAgentVersion(ctx, b.entry.ID)
	if err != nil {
		return false, "", err
	}
	available, err := updateAvailableFrom(local, latest)
	if err != nil {
		return false, latest, err
	}
	return available, latest, nil
}

// Update 按分发方式分两条路：
//   - npm 全局包走与 Claude / Codex **完全相同**的编排（确认来源 → 执行 → 健康检查 →
//     必要时按 npm 全局位置回滚，见 cross_npm_update.go）；
//   - 原生分发（官方安装器）走 CLI 自己的 update，不经过 npm 编排 —— 那条路会去确认
//     一个不存在的 npm 包，报出的"平台 npm 重装"是假话。
//
// ⚠️ 版本读数用 freshVersion 而**不是** Version：这段编排里的每一次取版本都必须是真的，
// 见 freshVersion。
func (b *catalogAgentBackend) Update(ctx context.Context) (string, string, error) {
	if b.entry.InstallKind != InstallKindNpmGlobal {
		return b.updateNative(ctx)
	}
	return runCrossCLIUpdate(ctx, b.shell.crossWhere(), b.entry.CommandName, b.entry.UpdateArgs, b.entry.Name,
		agentNpmCLIInstall(b.entry), b.shell.crossRun, b.freshVersion)
}

// updateNative 在目标环境执行 CLI 自带的 update 并做健康检查。
//
// 命令名取**实际探测到的那个**（主名或别名）：只装出 `agent` 的机器上执行
// `cursor-agent update` 必然失败。
func (b *catalogAgentBackend) updateNative(ctx context.Context) (string, string, error) {
	previous, command, ok := b.probeVersionWithCommand(ctx, true)
	if !ok || previous == "" {
		return "", "", fmt.Errorf("%s未安装 %s", b.shell.crossWhere(), b.entry.Name)
	}
	script := shellQuote(command)
	for _, arg := range b.entry.UpdateArgs {
		script += " " + shellQuote(arg)
	}
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	out, err := b.shell.crossRun(ctx, script)
	if err != nil {
		return previous, "", fmt.Errorf("升级 %s 失败：%w%s", b.entry.Name, err, updateOutputDetail(out))
	}
	current := b.freshVersion(context.Background())
	if current == "" {
		return previous, "", fmt.Errorf("升级 %s 失败：更新后未通过健康检查", b.entry.Name)
	}
	return previous, current, nil
}

// freshVersion 与 Version 同义，但尽量取一次**真实**读数（能真探就真探）。
//
// 为什么升级这条路非它不可：WSL 侧的读数是 stale-while-revalidate —— 只要探过一次，
// 之后**永远先回旧值**、后台再刷新（wslAgentRunner.probe 的三条分支）。这条编排里有三处
// 取版本，每一处都会被它骗过去：
//
//	① 升级前的 previous 与升级后的健康检查读到同一个版本号 ⇒ 界面报出"升到同一个版本"
//	   （审计里 From == To）；
//	② **升级真的把工具升坏了也判成健康 ⇒ 回滚永不触发**（那条"命令报错 ≠ 工具坏了，
//	   还能读出版本就不回滚"的判据被反转成"永远能读出版本"）；
//	③ 回滚之后的核对（`current != previous` 就报"回滚失败"）同样拿旧值比对，于是
//	   一次没生效的回滚会被判成成功。
//
// 真探失败时**退回常规读数**而不是直接报空：那是"这次没探成"，不是"没装"，
// 而 runCrossCLIUpdate 会把空版本当成"未安装"（一句指错方向的结论）。
// 退回之后的行为与改动前一致 —— 宁可维持旧毛病，也不要凭空多出一种误判。
func (b *catalogAgentBackend) freshVersion(ctx context.Context) string {
	if version, _, ok := b.probeVersionWithCommand(ctx, true); ok && version != "" {
		return version
	}
	return b.Version(ctx)
}

// AutoUpdateSupported：跨端这条路上的升级已经接通（平台装的走 npm 重装，用户自己装的走
// CLI 自带的 update）。显式写出来而不是靠 agentAutoUpdatable 的默认值，是为了让这一档
// 有一个可读的出处 —— 默认值那种"没写就等于支持"的约定，在读数上等于没有判据。
func (b *catalogAgentBackend) AutoUpdateSupported() bool { return true }

// Run：跨端会话（StartSession / 一次性 Run）尚未接通，如实报错，不假装能跑。
// 装好之后能在目标环境里执行 CLI，但项目对话还选不到它 —— 界面上不要说成"装完就能用"。
func (b *catalogAgentBackend) Run(context.Context, AgentRunRequest, AgentRunSink) error {
	return fmt.Errorf("%s 的跨端会话尚未接通", b.entry.Name)
}

// Login 发起该工具的登录。
//
// 目录驱动的这一层**不驱动交互式 TUI**（无头环境无法可靠驱动它，与 codebuddyCLIRunner
// 的取舍一致）：如实返回空信息，由 agent_login.go 拼出"去目标环境终端里运行 <命令>"的
// 指引 —— 那句话本来就该按目录里的名字生成，而不是写死某一个工具。
//
// 未安装时报错而不是回一句空指引：让用户"去终端里跑"一个还不存在的命令，是把一次
// 必然失败的操作说成下一步。
func (b *catalogAgentBackend) Login(ctx context.Context) (agentLoginInfo, error) {
	if b.Version(ctx) == "" {
		return agentLoginInfo{}, fmt.Errorf("%s 未安装", b.entry.Name)
	}
	return agentLoginInfo{}, nil
}

// LoginStatus：目录里登记了非交互登录态命令（AuthStatusArgs）时，在目标环境执行它，
// 按 AuthStatusEmptyMarkers 判断是否“没有凭据”。
//
// 三态（见 agent_auth.go）：没有登记命令 ⇒ unreadable 且不带 error（"这个工具不能自报"）；
// 命令没跑成 ⇒ unreadable + 失败原因（"这次没读成"，界面说法不同，且按短 TTL 重试）。
func (b *catalogAgentBackend) LoginStatus(ctx context.Context) (agentAuthState, error) {
	if len(b.entry.AuthStatusArgs) == 0 {
		return agentAuthUnreadable, nil
	}
	// 登录态查询不能跟着一个没有期限的 HTTP 请求无限挂着；与本地 runner 的 20s 对齐。
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, _, ok := b.probeCommand(ctx, b.entry.AuthStatusArgs, true)
	if !ok {
		return agentAuthUnreadable, fmt.Errorf("在目标环境执行 %s %s 失败", b.entry.CommandName, strings.Join(b.entry.AuthStatusArgs, " "))
	}
	return agentAuthStateFromStatusOutput(out, b.entry.AuthStatusEmptyMarkers), nil
}
