package app

import (
	"context"
	"fmt"
)

// WSL 侧的 CLI 升级。
//
// 与"安装"是两条不同的路，两条都要留：
//
//   - **平台装的**（登记为 npm 类）→ 走 npm 重装。布局是我们定的，直接指定包与版本
//     确定性最高，而且 install 与 update 变成同一条代码路径（performAgentUpdate 分流）。
//   - **用户自己装的** → 走这里，让 CLI 自带的 `update` 去做 —— 我们不知道它当初是
//     用 npm、官方安装器还是别的方式装的，只有它自己清楚。
//
// 编排（确认来源 → 执行 → 健康检查 → 必要时按 npm 全局位置回滚）与 SSH 侧**共用
// 同一套实现**，差别只在"怎么在那边跑命令"（见 cross_npm_update.go）。

// wslRunScript 在 WSL 内跑一段 sh 脚本，返回 stdout。
//
// 与 wslNativeCommand 的分工：那个是"启动一个 CLI 并接管它的输出流"（会话用），
// 这个是"跑一段脚本拿结果"（升级这类短命令）。
//
// 两者都必须让 wslPathPrefix() 把用户原生的 npm bin 前置：WSL 的 PATH 里混着
// Windows 挂载进来的 npm shim，落到那上面会一跑即崩（它缺平台可选二进制）。
func (r *wslAgentRunner) wslRunScript(ctx context.Context, script string) (string, error) {
	env := &wslCrossEnvironment{runner: r}
	stdout, stderr, err := env.run(ctx, wslPathPrefix()+"\n"+script)
	if err != nil {
		return stdout, fmt.Errorf("%w%s", err, crossOutputDetail(stderr))
	}
	return stdout, nil
}

// Update implements AgentRunner。
func (r *wslAgentRunner) Update(ctx context.Context) (string, string, error) {
	return runCrossCLIUpdate(ctx, "WSL 内", "claude", "Claude Code", claudeNpmCLIInstall, r.wslRunScript, r.Version)
}

// CodexUpdate implements CodexCapableRunner。
func (r *wslAgentRunner) CodexUpdate(ctx context.Context) (string, string, error) {
	return runCrossCLIUpdate(ctx, "WSL 内", "codex", "Codex CLI", codexNpmCLIInstall, r.wslRunScript, r.CodexVersion)
}
