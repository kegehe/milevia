package app

import (
	"context"
	"errors"
	"fmt"
	pathpkg "path"
	"strings"
)

// 跨端的 CLI 安装与升级。
//
// 与本机实现共享的判据（不重造）：版本号白名单（parseSemver）、最低运行时闸门
// （checkRuntimeGate）、版本号提取（agentVersionFromOutput）、并发闸门
// （runnerUpdating，由 handler 的 beginAgentMaintenance 持有）、审计。
//
// 两处跨端特有的写法，都是踩过的形状：
//
//   - **执行前把 npm 所在目录前置到 PATH**。npm 与各家 CLI 都是 `#!/usr/bin/env node`
//     的脚本，PATH 里没有 node 时它们报的是 `env: node: No such file or directory` ——
//     与"没装这个命令"在文案上几乎一样，会把用户引到错的方向。
//   - **自检只认 prefix 里的产物，不查 PATH**。查 PATH 会把**用户自己装的那一份**
//     认成我们的安装，于是登记了别人的路径，之后"升级"去升级别人的那份
//     （本机实现踩过同一个坑，见 verifyAgentInstall 的注释）。

// installAgentCLIFor 按 Runner 分派：本机走本地 npm，其余走跨端 npm。
func (s *Server) installAgentCLIFor(ctx context.Context, runnerID, agentID, versionSelector string) (agentInstallation, error) {
	if isLocalRunnerID(runnerID) {
		return s.installAgentCLI(ctx, runnerID, agentID, versionSelector)
	}
	env := s.crossEnvironmentFor(runnerID)
	if env == nil {
		return agentInstallation{}, fmt.Errorf("%s 没有可用的跨端执行通道", runnerID)
	}
	return s.installAgentCLICross(ctx, runnerID, env, agentID, versionSelector)
}

// installAgentCLICross 在目标环境里用那边的 npm 安装（或原地升级）一个工具。
//
// env 由调用方传入，理由同 installManagedRuntimeCross：让编排可以在假的目标环境上测。
func (s *Server) installAgentCLICross(ctx context.Context, runnerID string, env crossEnvironment, agentID, versionSelector string) (agentInstallation, error) {
	entry, ok := agentByID(agentID)
	if !ok {
		return agentInstallation{}, fmt.Errorf("不支持的工具 %s", agentID)
	}
	if !entry.SupportsInstall {
		return agentInstallation{}, fmt.Errorf("%s 不在平台内安装", entry.Name)
	}
	target, err := probeTargetEnvironment(ctx, env)
	if err != nil {
		return agentInstallation{}, err
	}
	if wsl, ok := env.(*wslCrossEnvironment); ok {
		wsl.recordVisibleMounts(target.Mounts)
	}
	root, err := crossToolchainRoot(target)
	if err != nil {
		return agentInstallation{}, err
	}
	status := s.probeRuntimeCross(ctx, env, target)

	version := strings.TrimSpace(versionSelector)
	if version == "" {
		version = "latest"
	}
	if version != "latest" {
		// 版本号是**命令参数**，只允许 latest 或形如 1.2.3 的版本号。
		if _, err := parseSemver(version); err != nil {
			return agentInstallation{}, fmt.Errorf("版本号 %q 不合法", version)
		}
	}

	recorded, hasRecord, err := s.recordedInstallation(ctx, runnerID, agentID)
	if err != nil {
		return agentInstallation{}, err
	}

	// "用哪个 npm""装到哪个 prefix""用哪套 node 过闸门"是**同一个决定**，必须一起定。
	//
	// 原先这三件事各判一次：prefix 按 installKind 选、npm 一律用探测首选（托管优先）、
	// 闸门版本也一律用托管那套。于是"登记为系统 npm 装的那份"会被拿托管 npm 装到
	// 托管 prefix —— 实际落点与登记口径分叉，之后升级会按错误的 prefix 去找产物。
	//
	// 顺序与本机一致：已登记的原地升级；没装过的托管优先 —— 系统 npm 的全局 prefix
	// 在 Linux 上常是 /usr/local（不可写），装到那里会以 EACCES 失败，而用户看不出
	// 该改什么。
	// "用哪个 npm""装到哪个 prefix""以哪套 node 过闸门"是**同一个决定**，判据只有一处
	// （resolveCrossInstallTarget）—— 管理页那句"这台机器上够不够装这个工具"也用它，
	// 于是界面与服务端必然给出一致的结论。
	installTarget, reason := resolveCrossInstallTarget(status, hasRecord, recorded, root)
	if reason != "" {
		return agentInstallation{}, errors.New(reason)
	}
	installKind, npmPath, prefix := installTarget.InstallKind, installTarget.NpmPath, installTarget.Prefix
	if err := checkRuntimeGate(installTarget.GateRuntime, entry); err != nil {
		return agentInstallation{}, err
	}

	// 与安装计划那条不同：previous 只影响失败时的展示，读不到不阻塞本次安装。
	previous := ""
	if item, _, err := s.recordedInstallation(ctx, runnerID, agentID); err == nil {
		previous = item.Version
	}

	script := crossAgentInstallScript(npmPath, prefix, entry.NpmPackage+"@"+version, entry.CommandName)
	stdout, stderr, err := env.run(ctx, script)
	if err != nil {
		return agentInstallation{RunnerID: runnerID, AgentID: agentID, Version: previous, InstallKind: installKind, Prefix: prefix},
			fmt.Errorf("在%s安装 %s 失败：%w%s", env.describe(), entry.Name, err, crossOutputDetail(stderr))
	}
	values := parseKeyValueLines(stdout)
	binary := strings.TrimSpace(values["binary"])
	rawVersion := strings.TrimSpace(values["version"])
	if binary == "" || rawVersion == "" {
		// 装完了但自检没拿到版本：把现场（脚本输出）带进报错，否则只剩一句"失败"。
		return agentInstallation{}, fmt.Errorf("在%s装完 %s 后自检失败：没有取到可执行文件或版本%s",
			env.describe(), entry.Name, crossOutputDetail(stderr))
	}

	installation := agentInstallation{
		RunnerID:    runnerID,
		AgentID:     agentID,
		BinaryPath:  binary,
		InstallKind: installKind,
		Prefix:      prefix,
		Version:     agentVersionFromOutput(rawVersion),
		Source:      "managed-install",
	}
	if err := s.recordAgentInstallation(ctx, installation); err != nil {
		return installation, err
	}
	return installation, nil
}

// crossAgentInstallScript 生成"npm 安装 + 自检"的一条脚本。
//
// prefix 为空表示用 npm 自己的全局 prefix（系统全局）。自检一律落在**解析出来的
// prefix 里**，而不是 `command -v`：后者会把用户自己装的那一份认成我们的。
func crossAgentInstallScript(npmPath, prefix, packageSpec, commandName string) string {
	npmDir := pathpkg.Dir(npmPath)
	// ⚠️ `prefix` 的赋值必须在 install **之前**：脚本开头是 `set -eu`，而 install
	// 那一行就要用 `--prefix "$prefix"` —— 先引用后赋值会让脚本立刻以
	// "unbound variable" 退出，用户看到的只是"安装失败：exit status 1"，指不到原因。
	//
	// `$(npm prefix -g)` 那一档同样放在前面：它问的是 npm 自己的全局位置，
	// 跟这次装什么无关，不必等安装完。
	prefixLine := "prefix=$(npm prefix -g)\n"
	installPrefix := ""
	if prefix != "" {
		prefixLine = "prefix=" + shellQuote(prefix) + "\n"
		installPrefix = " --prefix \"$prefix\""
	}
	return "set -eu\n" +
		"export PATH=" + shellQuote(npmDir) + ":$PATH\n" +
		prefixLine +
		shellQuote(npmPath) + " install -g" + installPrefix + " " + shellQuote(packageSpec) + "\n" +
		"target=\"$prefix/bin/" + commandName + "\"\n" +
		"if [ ! -x \"$target\" ]; then\n" +
		"  echo '安装后没有在 prefix 里找到可执行的 " + commandName + "' >&2\n" +
		"  exit 3\n" +
		"fi\n" +
		"version=$(\"$target\" --version 2>/dev/null || true)\n" +
		"if [ -z \"$version\" ]; then\n" +
		"  echo '安装后的 " + commandName + " 无法执行' >&2\n" +
		"  exit 4\n" +
		"fi\n" +
		"printf 'binary=%s\\n' \"$target\"\n" +
		"printf 'version=%s\\n' \"$version\"\n"
}
