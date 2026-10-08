package app

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// CLI 自带 update 失败之后的原地修复。
//
// ── 为什么需要它 ──────────────────────────────────────────────────────────────
//
// Windows 上 npm 全局装的 Claude Code，`claude update` 会**必然**失败，而它给出的
// 理由与真实原因毫无关系（2026-09 在 2.1.266 + Node 22.15.0 上实测）：
//
//  1. CLI 要跑 `npm view <pkg>@latest version --prefer-online` 来问最新版本号；
//  2. 它自己用 `where.exe npm` 解析 npm，过滤规则是"基名必须以 .com/.exe/.bat/.cmd
//     结尾"——于是跳过无扩展名的 `npm`（POSIX sh 脚本）与 `npm.ps1`，选中 `npm.cmd`；
//  3. 然后 `child_process.spawn("…\\npm.cmd", …)`，**没有传 shell:true**。Node ≥ 22
//     （CVE-2024-27980 的修复）拒绝无 shell 直接 spawn `.cmd`/`.bat`，抛 EINVAL；
//  4. 子进程 0ms 就死、stdout 为空、退出码非 0 ⇒ CLI 判定"拿不到版本号"，把这一档
//     报成 "npm registry is unreachable / Corporate proxy/firewall blocking npm"。
//
// 也就是说：**网络是通的，被伪装成网络问题的是一次必然失败的进程启动**。用户在
// 界面看到的正是这段误导文案，按它去查网络只会白费时间。
//
// 平台侧不受影响：Go 的 exec 能正常跑 `npm.cmd`（同机实测 `npm view` 1 秒返回）。
// 所以这里用平台自己的 npm 原地重装一次。
//
// ── 三条纪律 ─────────────────────────────────────────────────────────────────
//
//  1. **只原地升级**。目标 prefix 必须逐个候选实测确认（verifyNpmCLICommand 核对
//     "这个命令确实来自那个 prefix 下的那个 npm 全局包"）。确认不了就什么都不做 ——
//     一份 pnpm / bun / 官方安装器装的 CLI，宁可不升，也不能被换成 npm 装的第二份。
//  2. **走 installAgentCLIWithPlan 的同一段**：版本号白名单、运行时闸门、装后自检、
//     登记，一样不少。
//  3. **装完必须登记**。这是它相对于"再调一次 CLI"的真正价值：登记之后，下一次
//     升级直接走 performAgentUpdate 里那条确定性路径，不必再撞一遍 CLI 的坑。

// repairAgentViaNpm 在 CLI 自带的 update 失败之后，用平台自己的 npm 把它原地重装到最新版。
//
// 只在**本机** runner 上做：跨端的"npm 在哪、装到哪个 prefix"由目标环境自己报出来
// （installAgentCLICross），本机这套（系统 PATH / 托管工具链）在那边不成立。
func (s *Server) repairAgentViaNpm(ctx context.Context, runnerID, agentID string) (agentInstallation, error) {
	if !isLocalRunnerID(runnerID) {
		return agentInstallation{}, errors.New("跨端暂不支持用平台 npm 原地修复")
	}
	entry, ok := agentByID(agentID)
	if !ok {
		return agentInstallation{}, fmt.Errorf("不支持的工具 %s", agentID)
	}
	if !entry.SupportsInstall || entry.NpmPackage == "" {
		return agentInstallation{}, fmt.Errorf("%s 不是 npm 全局包，无法用 npm 原地重装", entry.Name)
	}
	commandPath, err := exec.LookPath(s.agentBinary(agentID))
	if err != nil {
		return agentInstallation{}, fmt.Errorf("找不到 %s 命令：%w", entry.Name, err)
	}
	candidates := s.npmGlobalPrefixCandidates(ctx)
	if len(candidates) == 0 {
		return agentInstallation{}, errors.New("找不到 npm，无法用 npm 原地重装；请先安装 Node.js 运行时")
	}
	install := agentNpmCLIInstall(entry)
	prefix := ""
	for _, candidate := range candidates {
		if err := verifyNpmCLICommand(commandPath, candidate, install); err == nil {
			prefix = candidate
			break
		}
	}
	if prefix == "" {
		return agentInstallation{}, fmt.Errorf("%s 不在任何 npm 全局 prefix 下（查过 %s）",
			entry.Name, strings.Join(candidates, "、"))
	}
	plan, err := s.existingNpmInstallPlan(ctx, prefix)
	if err != nil {
		return agentInstallation{}, err
	}
	// source 与"平台自己装的"分开记：审计里要能看出这一份是修复出来的，
	// 而不是当初由平台安装的。
	return s.installAgentCLIWithPlan(ctx, runnerID, agentID, plan, "latest", "npm-repair")
}

// npmGlobalPrefixCandidates 给出"这份命令可能属于的 npm 全局 prefix"。
//
// 两个候选对应两种来源：平台托管的工具链、用户机器上的系统 npm。托管在前，与
// resolveAgentInstallPlan 的"托管优先"同源。跨端的 prefix 不在此列 —— 那由目标
// 环境自己报出来（installAgentCLICross），在本机猜会把工具装错机器。
//
// 系统那一档问不到（PATH 上没有 npm）就只留托管候选；一个都问不到时返回空，
// 由调用方如实报"找不到 npm"。
func (s *Server) npmGlobalPrefixCandidates(ctx context.Context) []string {
	out := []string{}
	if root, err := managedToolchainRoot(); err == nil && fileExists(managedNpmCommand(root)) {
		out = append(out, managedNpmGlobalPrefix(root))
	}
	if prefix, err := npmGlobalPrefix(ctx); err == nil {
		if trimmed := strings.TrimSpace(prefix); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
