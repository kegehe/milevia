package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// CLI 的安装与升级。
//
// 两条与既有一致的地方（不重造）：
//   - 装完之后的路径/shim/回滚仍然交给 `npmCLIInstall`（npm_cli_install.go）；
//   - 并发闸门仍然是 `runnerUpdating`（见调用处）。
//
// 一条与既有实现不同、且**是有意的**：升级不再无条件调 CLI 自带的 `update`。
// 平台托管的安装是我们自己定的布局，直接 `npm install -g <pkg>@latest` 确定性最高，
// 而且 install 与 update 变成同一条代码路径（docs/42 §6.3）—— 于是也就不存在
// "两条路上的行为不一致"这种可能。
//
// 本期不做卸载（已拍板的操作面就是"安装 + 升级"）。不提供卸载入口比提供一个
// 半可靠的卸载更安全：卸载要动的是用户机器上的全局包。

const (
	installKindNpmManaged = "npm-global-managed"
	installKindNpmSystem  = "npm-global-system"
	installKindNative     = "native"
)

// agentInstallPlan 说明"这次安装/升级用哪个 npm、装到哪个 prefix"。
type agentInstallPlan struct {
	NpmPath string
	// Prefix 为空表示用 npm 自己的全局 prefix（系统全局）。
	Prefix string
	Kind   string
	// RuntimeVersion 是执行这次安装的 Node 版本（用于最低版本闸门）。
	RuntimeVersion string
}

// resolveAgentInstallPlan 决定这次安装该怎么装。
//
// 顺序（不许换，理由在每一步上）：
//
//  1. 该工具**已经**由平台托管 → 原地升级，不另造一份；
//  2. 该工具已经在系统 npm 全局里 → 也用系统 npm 原地升级，同样不另造一份；
//  3. 还没装过 → **托管工具链优先**（一定可写、路径可预测、与登记表同源），
//     没有托管工具链时才用系统 npm；
//  4. 两种运行时都没有 → 如实说"需要先安装 Node.js 运行时"。
//
// ⚠️ 第 3 步与 docs/42 §7.3 初版的顺序不同（那版写的是"系统 npm 优先"）。改掉的
// 理由是权限：系统 npm 的全局 prefix 在 Linux 上常是 `/usr/local`（不可写），装到
// 那里会以 EACCES 失败，而用户完全看不出该改什么。而"LookPath 能找到"这条当初的
// 理由已经失效 —— 现在路径经解析器读登记表，不依赖 PATH。
func (s *Server) resolveAgentInstallPlan(ctx context.Context, runnerID, agentID string) (agentInstallPlan, error) {
	// 只处理本机。跨端走 installAgentCLICross —— 那边的"npm 在哪、装到哪个 prefix"
	// 由目标环境自己报出来，本机这套（托管工具链目录 vs 系统 PATH）在那边不成立。
	if !isLocalRunnerID(runnerID) {
		return agentInstallPlan{}, fmt.Errorf("内部错误：跨端安装不应走到本机的安装计划（%s）", runnerID)
	}
	entry, ok := agentByID(agentID)
	if !ok {
		return agentInstallPlan{}, fmt.Errorf("不支持的工具 %s", agentID)
	}
	if !entry.SupportsInstall {
		return agentInstallPlan{}, fmt.Errorf("%s 不在平台内安装", entry.Name)
	}

	managedRoot, rootErr := managedToolchainRoot()
	managedNpm, managedPrefix, managedRuntime := "", "", ""
	if rootErr == nil && fileExists(managedNodeBinary(managedRoot)) {
		if candidate := managedNpmCommand(managedRoot); fileExists(candidate) {
			managedNpm = candidate
			managedPrefix = managedNpmGlobalPrefix(managedRoot)
			managedRuntime = runVersionCommand(ctx, managedNodeBinary(managedRoot), "--version")
		}
	}
	systemNpm, systemErr := exec.LookPath("npm")
	if systemErr != nil {
		systemNpm = ""
	}

	// 已登记的安装方式决定"原地升级"用哪条路。
	recorded, hasRecord, err := s.recordedInstallation(ctx, runnerID, agentID)
	if err != nil {
		return agentInstallPlan{}, err
	}
	if hasRecord {
		switch recorded.InstallKind {
		case installKindNpmManaged:
			if managedNpm == "" {
				// 托管运行时不见了：如实报错，**不**悄悄改用系统 npm 另装一份
				// （那会让用户机器上出现两份 CLI，而没人知道哪份在生效）。
				return agentInstallPlan{}, errors.New("该工具原本由托管工具链安装，但现在找不到托管运行时；请先重新安装 Node.js 运行时")
			}
			return agentInstallPlan{NpmPath: managedNpm, Prefix: managedPrefix, Kind: installKindNpmManaged, RuntimeVersion: managedRuntime}, nil
		case installKindNpmSystem:
			if systemNpm == "" {
				return agentInstallPlan{}, errors.New("该工具原本装在系统 npm 全局里，但现在找不到 npm")
			}
			return agentInstallPlan{NpmPath: systemNpm, Kind: installKindNpmSystem, RuntimeVersion: nodeVersionNearNpm(ctx, systemNpm)}, nil
		case installKindNative:
			return agentInstallPlan{}, errors.New("该工具由官方安装器安装，平台不接管它的升级；请使用该 CLI 自带的更新命令")
		}
	}

	// 还没装过：托管优先，其次系统。
	if managedNpm != "" {
		return agentInstallPlan{NpmPath: managedNpm, Prefix: managedPrefix, Kind: installKindNpmManaged, RuntimeVersion: managedRuntime}, nil
	}
	if systemNpm != "" {
		return agentInstallPlan{NpmPath: systemNpm, Kind: installKindNpmSystem, RuntimeVersion: nodeVersionNearNpm(ctx, systemNpm)}, nil
	}
	return agentInstallPlan{}, fmt.Errorf("目标环境没有可用的 npm：请先安装 Node.js 运行时（%s）", entry.Name)
}

// nodeVersionNearNpm 找出与这个 npm 同处一地的 node 的版本。
//
// 不直接查 PATH 上的 `node`：用户可能改过 PATH，让 node 与 npm 来自不同的安装。
// 而闸门要看的是"这次安装实际用的那个 npm 背后是哪个 Node"。
func nodeVersionNearNpm(ctx context.Context, npmPath string) string {
	dir := filepath.Dir(npmPath)
	for _, name := range []string{"node", "node.exe"} {
		if candidate := filepath.Join(dir, name); fileExists(candidate) {
			return runVersionCommand(ctx, candidate, "--version")
		}
	}
	if path, err := exec.LookPath("node"); err == nil {
		return runVersionCommand(ctx, path, "--version")
	}
	return ""
}

// agentInstallationFor 读某个 (runner, agent) 的登记项。
func (s *Server) agentInstallationFor(ctx context.Context, runnerID, agentID string) (agentInstallation, error) {
	var item agentInstallation
	err := s.db.QueryRowContext(ctx, `select runner_id,agent_id,binary_path,install_kind,prefix,version,source,installed_at,updated_at
		from agent_installations where runner_id=? and agent_id=?`, runnerID, agentID).
		Scan(&item.RunnerID, &item.AgentID, &item.BinaryPath, &item.InstallKind, &item.Prefix,
			&item.Version, &item.Source, &item.InstalledAt, &item.UpdatedAt)
	if err != nil {
		return agentInstallation{}, err
	}
	return item, nil
}

// recordedInstallation 读登记项，并把"没有记录"与"读失败"**分开**。
//
// 这个区分不是洁癖：把读失败当成"没登记过"，会让一份已经装在系统 npm 全局里的工具
// 被当成没装过 —— 于是升级时改走托管 npm 另装一份，用户机器上出现两份 CLI，
// 而没人知道哪份在生效（resolveAgentInstallPlan 里明令禁止的正是这件事）。
func (s *Server) recordedInstallation(ctx context.Context, runnerID, agentID string) (agentInstallation, bool, error) {
	recorded, err := s.agentInstallationFor(ctx, runnerID, agentID)
	switch {
	case err == nil:
		return recorded, recorded.InstallKind != "", nil
	case errors.Is(err, sql.ErrNoRows):
		// 真的没装过/没登记过 —— 这是唯一可以走"从零开始"分支的情况。
		return agentInstallation{}, false, nil
	default:
		return agentInstallation{}, false, fmt.Errorf("读取安装登记失败：%w", err)
	}
}

// installAgentCLI 通过 npm 全局安装（或原地升级）一个工具。
//
// install 与 update 共用它，所以两条路上的最低版本闸门、自检与登记完全一致。
func (s *Server) installAgentCLI(ctx context.Context, runnerID, agentID, versionSelector string) (agentInstallation, error) {
	entry, ok := agentByID(agentID)
	if !ok {
		return agentInstallation{}, fmt.Errorf("不支持的工具 %s", agentID)
	}
	plan, err := s.resolveAgentInstallPlan(ctx, runnerID, agentID)
	if err != nil {
		return agentInstallation{}, err
	}
	if err := checkRuntimeGate(plan.RuntimeVersion, entry); err != nil {
		return agentInstallation{}, err
	}

	version := strings.TrimSpace(versionSelector)
	if version == "" {
		version = "latest"
	}
	// 只允许"latest"或形如 1.2.3 / 1.2.3-rc.1 的版本号。这里是**命令参数**，
	// 不允许把任意字符串拼进去（对标 mcpRuntimeCommandPattern 的白名单纪律）。
	if version != "latest" {
		if _, err := parseSemver(version); err != nil {
			return agentInstallation{}, fmt.Errorf("版本号 %q 不合法", version)
		}
	}

	args := []string{}
	if plan.Prefix != "" {
		args = append(args, "--prefix", plan.Prefix)
	}
	args = append(args, "install", "-g", entry.NpmPackage+"@"+version)

	// previous 只用于"失败时报出旧版本"，读不到就留空（不阻塞安装）——
	// 这里与上面那条判据不同：那里读失败会导致**装错地方**，这里只是少一个展示字段。
	previous := ""
	if recorded, _, err := s.recordedInstallation(ctx, runnerID, agentID); err == nil {
		previous = recorded.Version
	}

	installCtx, cancel := context.WithTimeout(ctx, s.config.agentUpdateTimeout())
	defer cancel()
	cmd := exec.CommandContext(installCtx, plan.NpmPath, args...)
	configureProcessGroup(cmd)
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return agentInstallation{RunnerID: runnerID, AgentID: agentID, Version: previous, InstallKind: plan.Kind, Prefix: plan.Prefix},
			fmt.Errorf("安装 %s 失败：%w%s", entry.Name, err, updateOutputDetail(out.String()))
	}

	// 装后自检：**必须真的执行一次**产物。只看文件在不在不够 ——
	// 一个下载不完整的包也会"存在"，而用户拿到的是"安装成功"。
	binary, err := s.verifyAgentInstall(ctx, plan, entry)
	if err != nil {
		return agentInstallation{}, fmt.Errorf("安装 %s 后自检失败：%w%s", entry.Name, err, updateOutputDetail(out.String()))
	}

	installation := agentInstallation{
		RunnerID:    runnerID,
		AgentID:     agentID,
		BinaryPath:  binary,
		InstallKind: plan.Kind,
		Prefix:      plan.Prefix,
		Version:     agentVersionFromOutput(runVersionCommand(ctx, binary, entry.VersionArgs...)),
		Source:      "managed-install",
	}
	if err := s.recordAgentInstallation(ctx, installation); err != nil {
		return installation, err
	}
	return installation, nil
}

// checkRuntimeGate 是"光有 npm 就用"不够的那一步。
//
// Node 16 上装 Claude Code 是装了也跑不起来，而报错会在用户开对话时才出现，
// 完全指不到"运行时太旧"。所以这里先拦，且**不静默回落**到另一个运行时。
func checkRuntimeGate(runtimeVersion string, entry AgentCatalogEntry) error {
	if runtimeVersion == "" || entry.MinRuntimeVersion == "" {
		return nil
	}
	meets, err := runtimeMeetsMinimum(runtimeVersion, entry.MinRuntimeVersion)
	if err != nil {
		return fmt.Errorf("无法比较运行时版本：%w", err)
	}
	if !meets {
		return fmt.Errorf("运行时版本过低（当前 Node %s，%s 需要 >= %s）；请先升级 Node.js 运行时",
			runtimeVersion, entry.Name, entry.MinRuntimeVersion)
	}
	return nil
}

// verifyAgentInstall 找出刚装出来的可执行文件并确认它真的能运行。
//
// ⚠️ 当装到**托管 prefix** 时，只在 prefix 里找，**不回落查 PATH**。
// 原因是真出过的事：装完自检回落到 PATH，结果找到了用户自己那一份 CLI，
// 于是我们把**别人的路径**登记成自己的安装位置 —— 之后升级会去升级那一份，
// 而"我们到底装到哪了"这个事实就永远错了。装到哪、就在哪找。
func (s *Server) verifyAgentInstall(ctx context.Context, plan agentInstallPlan, entry AgentCatalogEntry) (string, error) {
	install := agentNpmCLIInstall(entry)
	candidates := []string{}
	if plan.Prefix != "" {
		candidates = append(candidates, install.commandPath(plan.Prefix), install.binaryPath(plan.Prefix))
	} else {
		// 系统 npm 全局安装：它的 shim 就在 PATH 上（或 PATH 继承陈旧时按平台兜底补一次）。
		if path, err := exec.LookPath(entry.CommandName); err == nil {
			candidates = append(candidates, path)
		}
		candidates = append(candidates, platformFallbackCandidates(entry)...)
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("找不到安装后的 %s 命令", entry.CommandName)
	}
	for _, candidate := range candidates {
		if !fileExists(candidate) {
			continue
		}
		if version := runVersionCommand(ctx, candidate, entry.VersionArgs...); version != "" {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("安装后的 %s 无法执行（试过 %s）", entry.CommandName, strings.Join(candidates, "、"))
}

// packageScope 给出 npm 包的作用域（"@anthropic-ai"）；无作用域时返回空串。
func packageScope(pkg string) string {
	if index := strings.LastIndex(pkg, "/"); index > 0 && strings.HasPrefix(pkg, "@") {
		return pkg[:index]
	}
	return ""
}

// packageBase 给出 npm 包名（去掉作用域）。
func packageBase(pkg string) string {
	if index := strings.LastIndex(pkg, "/"); index > 0 && strings.HasPrefix(pkg, "@") {
		return pkg[index+1:]
	}
	return pkg
}
