package app

import (
	"context"
	"errors"
	"fmt"
	pathpkg "path"
	"strings"
	"time"
)

// 跨端的 CLI 升级（SSH 与 WSL 共用一套判据与一套回滚）。
//
// 两端唯一的差别是"怎么在那边跑命令"，所以编排只留一个 run 参数。抽出来的直接
// 原因是 WSL 侧原先直接报"尚未就绪" —— 而跨端**安装**做通之后，"升级"就没有理由
// 还停在那里：从"平台装的那份"到"用户自装的那份"，两边都有一条能走的路。
//
// 三件在两端都必须成立的事（缺一件就会留下一个说不清的失败）：
//
//  1. **只回滚"提供该命令的那个 npm 包"**。不然一次失败的升级会去动无关的全局包；
//  2. **失败后必须做健康检查**，只有确实不可用才回滚 —— 升级成功但版本号读法变了之类
//     的情况不该被当成失败回滚掉；
//  3. **升级后必须真的读一次版本号**。只看退出码的话，"更新成功"只是一句话。

// crossCommandRunner 是"在目标环境跑一条命令拿输出"。
type crossCommandRunner func(ctx context.Context, script string) (string, error)

// runCrossCLIUpdate 是在目标环境里跑 `<cli> update` 的完整编排。
//
// where 只用于报错文案（"远程服务器"、"WSL 内"）。
func runCrossCLIUpdate(
	ctx context.Context,
	where, commandName, displayName string,
	install npmCLIInstall,
	run crossCommandRunner,
	version func(context.Context) string,
) (string, string, error) {
	previous := version(ctx)
	if previous == "" {
		return "", "", fmt.Errorf("%s未安装 %s", where, displayName)
	}
	// 先确认这个命令确实来自那个 npm 包，再动它 —— 顺序不能反。
	recovery, recoveryErr := npmCLIRecoveryFor(ctx, run, install)
	out, err := run(ctx, commandName+" update")
	if err != nil {
		return finishCrossNpmUpdate(previous, displayName, version, run,
			fmt.Errorf("%s执行 %s update 失败：%w%s", where, commandName, err, updateOutputDetail(out)), recovery, recoveryErr)
	}
	healthCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	current := version(healthCtx)
	if current == "" {
		return finishCrossNpmUpdate(previous, displayName, version, run,
			fmt.Errorf("%s上的 %s 更新后未通过健康检查", where, displayName), recovery, recoveryErr)
	}
	return previous, current, nil
}

// npmCLIRecoveryFor 确认"这个命令确实由该 npm 包提供"，并取回它的全局 prefix。
//
// 只接受提供该命令的包：一次失败的升级不该去动无关的全局包（这与 doctor 式的
// "找到什么修什么"是相反的取舍 —— 宁可回滚不可用，也不要改错东西）。
func npmCLIRecoveryFor(ctx context.Context, run crossCommandRunner, install npmCLIInstall) (remoteNpmCLIRecovery, error) {
	expected := pathpkg.Join("$prefix", "lib", "node_modules", install.scope, install.packageName, "bin", install.binFile)
	command := fmt.Sprintf(`set -eu
prefix=$(npm prefix -g)
command_path=$(command -v %s)
resolved=$(readlink -f -- "$command_path")
expected=$(readlink -f -- "%s")
[ "$resolved" = "$expected" ]
printf '%%s\n' "$prefix"`, install.commandName, expected)
	out, err := run(ctx, command)
	if err != nil {
		return remoteNpmCLIRecovery{}, fmt.Errorf("确认 npm 全局安装来源失败：%w", err)
	}
	prefix := strings.TrimSpace(out)
	if prefix == "" {
		return remoteNpmCLIRecovery{}, errors.New("npm global prefix 为空")
	}
	return remoteNpmCLIRecovery{prefix: prefix, install: install}, nil
}

// finishCrossNpmUpdate 处理"升级失败"之后的收尾：能自愈就自愈，不能就如实说清。
func finishCrossNpmUpdate(
	previous, displayName string,
	version func(context.Context) string,
	run crossCommandRunner,
	updateErr error,
	recovery remoteNpmCLIRecovery,
	recoveryErr error,
) (string, string, error) {
	healthCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	if version(healthCtx) != "" {
		// 命令报错但工具还能用：不回滚（回滚反而会把一个可用的版本换掉）。
		cancel()
		return previous, "", updateErr
	}
	cancel()
	if recoveryErr != nil {
		return previous, "", fmt.Errorf("%w；自动回滚不可用：%v", updateErr, recoveryErr)
	}
	recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), 15*time.Second)
	_, err := run(recoveryCtx, remoteNpmRollbackCommand(recovery.prefix, previous, recovery.install))
	recoveryCancel()
	if err != nil {
		return previous, "", fmt.Errorf("%w；自动回滚失败：%v", updateErr, err)
	}
	verifyCtx, verifyCancel := context.WithTimeout(context.Background(), 15*time.Second)
	current := version(verifyCtx)
	verifyCancel()
	if current != previous {
		return previous, "", fmt.Errorf("%w；自动回滚失败：回滚后的健康检查没过（版本 %q）", updateErr, current)
	}
	return previous, previous, fmt.Errorf("%w；已自动回滚到 %s %s", updateErr, displayName, previous)
}
