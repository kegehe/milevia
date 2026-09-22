package app

import "fmt"

// "这次安装/升级该用哪套运行时"的**唯一**判据。
//
// 两处调用，必须得出同一个结论：
//
//   - 安装路径（installAgentCLICross）：决定用哪个 npm、装到哪个 prefix；
//   - 管理页的运行时状态（runtimeStatusFor）：决定"这台机器上够不够装这个工具"。
//
// 分开判会怎样：界面按"当前生效的那套"（托管优先）算，而安装按"登记的那套"执行 ——
// 一台机器上两者可以不同（登记为系统 npm 装的工具，后来机器上又装了托管 Node）。
// 表现就是界面亮出一个"升级"按钮，点下去被服务端的闸门拒。
//
// 顺序与本机一致：已登记的原地升级；没装过的托管优先 —— 系统 npm 的全局 prefix
// 在 Linux 上常是 /usr/local（不可写），装到那里会以 EACCES 失败，而用户看不出该改什么。

// crossInstallTarget 描述一次跨端安装/升级会用到的运行时与落点。
type crossInstallTarget struct {
	InstallKind string
	NpmPath     string
	Prefix      string
	// GateRuntime 是这次**实际执行的那个 npm** 背后的 node 版本（闸门要量它）。
	GateRuntime string
}

// resolveCrossInstallTarget 选出这套组合；第二个返回值是"为什么不能用"（空串 = 可用）。
func resolveCrossInstallTarget(status runtimeStatus, hasRecord bool, recorded agentInstallation, toolchainRoot string) (crossInstallTarget, string) {
	managedNpm := crossNpmCommand(toolchainRoot)
	managedReady := status.Origin == "managed" && status.NpmVersion != ""
	systemNpm := status.SystemNpmPath
	systemReady := systemNpm != "" && status.SystemNpmVersion != ""

	switch {
	case hasRecord && recorded.InstallKind == installKindNative:
		return crossInstallTarget{}, "该工具由官方安装器安装，平台不接管它的升级；请使用该 CLI 自带的更新命令"
	case hasRecord && recorded.InstallKind == installKindNpmManaged:
		if !managedReady {
			// 如实报错，**不**悄悄改用系统 npm 另装一份 —— 那会让目标环境上出现
			// 两份 CLI，而没人知道哪份在生效。
			return crossInstallTarget{}, "该工具原本由托管工具链安装，但目标环境现在没有可用的托管运行时；请先重新安装 Node.js 运行时"
		}
		return crossInstallTarget{
			InstallKind: installKindNpmManaged,
			NpmPath:     managedNpm,
			Prefix:      firstNonEmpty(recorded.Prefix, crossNpmGlobalPrefix(toolchainRoot)),
			GateRuntime: status.Version,
		}, ""
	case hasRecord && recorded.InstallKind == installKindNpmSystem:
		if !systemReady {
			return crossInstallTarget{}, "该工具原本装在系统 npm 全局里，但目标环境现在找不到可用的系统 npm"
		}
		return crossInstallTarget{InstallKind: installKindNpmSystem, NpmPath: systemNpm, GateRuntime: status.SystemNodeVersion}, ""
	case hasRecord:
		// 认不出的登记值：**不猜**它属于哪一档（猜错会装到第二个位置）。
		return crossInstallTarget{}, fmt.Sprintf("不认识的安装方式 %q；请先在管理页确认这台机器上的安装情况", recorded.InstallKind)
	case managedReady:
		return crossInstallTarget{
			InstallKind: installKindNpmManaged,
			NpmPath:     managedNpm,
			Prefix:      crossNpmGlobalPrefix(toolchainRoot),
			GateRuntime: status.Version,
		}, ""
	case systemReady:
		// 目标环境只有系统 npm：用它自己的全局 prefix（不为此下一整套 Node）。
		return crossInstallTarget{InstallKind: installKindNpmSystem, NpmPath: systemNpm, GateRuntime: status.SystemNodeVersion}, ""
	}
	// 这一句必须与"运行时装了但版本太低"区分开：用户的下一步动作不同。
	return crossInstallTarget{}, "目标环境没有可用的 npm：请先安装 Node.js 运行时"
}

// meetsMinimumByInstallKind 回答"这台机器上装这个工具时，运行时够不够"。
func meetsMinimumByInstallKind(status runtimeStatus, hasRecord bool, recorded agentInstallation, entry AgentCatalogEntry, toolchainRoot string) bool {
	target, reason := resolveCrossInstallTarget(status, hasRecord, recorded, toolchainRoot)
	if reason != "" {
		return false
	}
	ok, err := runtimeMeetsMinimum(target.GateRuntime, entry.MinRuntimeVersion)
	return err == nil && ok
}
