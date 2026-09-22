package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"
	"time"
)

// 跨端（WSL / SSH）的运行时安装与探测。
//
// 与本机实现的分工（都是有意选的，不是没抽干净）：
//
//   - **下载与校验都在本机做**。校验点留在我们能信任的那一侧 —— 目标环境可能连
//     `sha256sum` 都没有，而"远端自己下自己算"等于把完整性判据交给被安装的那台机器。
//   - **解压必须在目标环境做**。压缩包里的 node 是给**那边**的平台编译的，
//     本机是 Windows 而目标是 Linux 时，本机解压出来的东西在那边一行都跑不了。
//   - **落点一律在 $HOME 下**，绝不用 sudo。
//
// WSL 有一条本机没有的捷径：`C:\a\b` 在 WSL 里就是 `/mnt/c/a/b`，于是 30MB 的
// 压缩包**不需要传输**，那条路是零拷贝的（见 cross_environment.go 的 sharedPath）。

// installRuntimeFor 按 Runner 分派：本机走本地安装，其余走跨端安装。
func (s *Server) installRuntimeFor(ctx context.Context, runnerID string, request installRuntimeRequest) (agentInstallation, error) {
	if isLocalRunnerID(runnerID) {
		return s.installManagedRuntime(ctx, request)
	}
	env := s.crossEnvironmentFor(runnerID)
	if env == nil {
		return agentInstallation{}, fmt.Errorf("%s 没有可用的跨端执行通道", runnerID)
	}
	return s.installManagedRuntimeCross(ctx, runnerID, env, request)
}

// installManagedRuntimeCross 在跨端环境里装托管 Node 运行时。
//
// env 由调用方传入（而不是在里面现取）：与本机实现的 installManagedRuntimeAt 同一个
// 理由 —— 让整条链路能在假的目标环境上端到端跑一遍，而不必真的有一台 WSL 或远端机器。
func (s *Server) installManagedRuntimeCross(ctx context.Context, runnerID string, env crossEnvironment, request installRuntimeRequest) (agentInstallation, error) {
	if s.runtimes == nil {
		return agentInstallation{}, errors.New("运行时管理器不可用")
	}

	target, err := probeTargetEnvironment(ctx, env)
	if err != nil {
		return agentInstallation{}, err
	}
	// 探测顺带把"哪些盘符经 /mnt 可读"告诉 WSL 通道 —— 它决定压缩包走不走传输。
	if wsl, ok := env.(*wslCrossEnvironment); ok {
		wsl.recordVisibleMounts(target.Mounts)
	}
	if !target.HasTar || !target.HasGzip {
		// 如实拒绝并说清缺什么。**不**退化成"本机解压后逐个文件上传"——
		// 那要传几千个文件，慢到用户会以为卡死，而报错至少是可操作的。
		return agentInstallation{}, fmt.Errorf("%s 里缺少 tar 或 gzip（解压官方分发包需要它们），无法在那里安装 Node.js 运行时", env.describe())
	}

	root, err := crossToolchainRoot(target)
	if err != nil {
		return agentInstallation{}, err
	}
	platform, err := target.platformKey()
	if err != nil {
		return agentInstallation{}, err
	}
	dist, err := s.runtimes.resolveDistribution(ctx, request.Version, platform)
	if err != nil {
		return agentInstallation{}, err
	}

	// 本机下载 + SHA256 校验。
	localStaging, err := os.MkdirTemp("", "milevia-runtime-")
	if err != nil {
		return agentInstallation{}, err
	}
	defer os.RemoveAll(localStaging)
	localArchive := filepath.Join(localStaging, dist.FileName)
	if err := s.runtimes.downloadVerified(ctx, dist, localArchive, nil); err != nil {
		return agentInstallation{}, err
	}

	staging := pathpkg.Join(root, fmt.Sprintf("node.staging-%d", time.Now().UTC().UnixNano()))
	// ⚠️ 压缩包落在 **root** 而不是 staging：安装脚本开头会 `rm -rf "$staging"` 清上次
	// 残留，若包本身就在 staging 里，SSH 那条"上传过去"的路会把自己刚传的包删掉，
	// 随后的 tar 只会报"找不到文件" —— 一个指不到真正原因的报错。
	remoteArchive, shared, err := stageArchive(ctx, env, localArchive, root)
	if err != nil {
		return agentInstallation{}, err
	}

	script := crossRuntimeInstallScript(root, staging, remoteArchive, shared, dist.FileName)
	stdout, stderr, err := env.run(ctx, script)
	if err != nil {
		return agentInstallation{}, fmt.Errorf("在%s安装 Node.js 运行时失败：%w%s", env.describe(), err, crossOutputDetail(stderr))
	}
	nodeVersion, npmVersion, err := parseCrossRuntimeInstall(stdout)
	if err != nil {
		return agentInstallation{}, fmt.Errorf("在%s装完运行时后自检失败：%w%s", env.describe(), err, crossOutputDetail(stderr))
	}
	if npmVersion == "" {
		// node 能跑但没有 npm：装不了 CLI。如实报错，并保留已装好的 node（它本身可用），
		// 但**不登记成可用运行时** —— 登记了就会让用户以为可以装工具了。
		return agentInstallation{}, fmt.Errorf("Node %s 已解压到%s，但随包的 npm 不可用；请检查该分发包", nodeVersion, env.describe())
	}

	installation := agentInstallation{
		RunnerID:    runnerID,
		AgentID:     runtimeAgentID,
		BinaryPath:  crossNodeBinary(root),
		InstallKind: "managed-toolchain",
		// prefix 必须一起记：CLI 装到哪由它决定，只有路径没有 prefix 时
		// "升级"会去找系统 npm 的全局位置。
		Prefix:  crossNpmGlobalPrefix(root),
		Version: nodeVersion,
		Source:  "managed-install",
	}
	if err := s.recordAgentInstallation(ctx, installation); err != nil {
		return installation, err
	}
	return installation, nil
}

// stageArchive 把压缩包送到目标环境，并报告它是"共享可见"还是"上传过去"的。
//
// 这个区分必须返回给调用方：共享可见的路径**在本机磁盘上**，收尾时绝不能对它
// 执行 `rm`（那会删掉本机文件）。
func stageArchive(ctx context.Context, env crossEnvironment, hostPath, targetDir string) (string, bool, error) {
	if shared, ok := env.sharedPath(hostPath); ok {
		return shared, true, nil
	}
	remotePath, err := env.upload(ctx, hostPath, targetDir)
	if err != nil {
		return "", false, fmt.Errorf("把分发包送到%s失败：%w", env.describe(), err)
	}
	return remotePath, false, nil
}

// crossRuntimeInstallScript 生成"解压 + 自检 + 到位"的一条脚本。
//
// 一条而不是几条：中间状态（解压到一半）不该被别的请求观察到，而逐条往返还会
// 让"失败时到底停在哪一步"变得含糊。
//
// 两个刻意的写法：
//
//   - **PATH 前置托管 node 的 bin**：`npm` 是个 shell 脚本（`#!/usr/bin/env node`），
//     直接执行它要求 PATH 里能找到 node。不前置的话报错是 `env: node: No such file`，
//     与"npm 没装"看起来一模一样。
//   - **不用 `tar --strip-components`**：busybox 的 tar 没有这个选项，而 musl 发行版
//     正是用 busybox。改成解压后用 POSIX 循环数顶层目录 —— 数量不等于 1 就如实报错
//     （包结构变了这件事必须被说出来，不能猜）。
func crossRuntimeInstallScript(root, staging, archive string, archiveShared bool, archiveName string) string {
	cleanup := "rm -f " + shellQuote(archive) + "\n"
	if archiveShared {
		// 压缩包在本机磁盘上（WSL 经 /mnt）：那边的 rm 会删掉本机的文件。
		cleanup = ""
	}
	return "set -eu\n" +
		"root=" + shellQuote(root) + "\n" +
		"staging=" + shellQuote(staging) + "\n" +
		"archive=" + shellQuote(archive) + "\n" +
		"mkdir -p \"$root\"\n" +
		"rm -rf \"$staging\"\n" +
		"mkdir -p \"$staging\"\n" +
		"tar -xzf \"$archive\" -C \"$staging\"\n" +
		"count=0\n" +
		"inner=\n" +
		"for entry in \"$staging\"/*; do\n" +
		"  count=$((count + 1))\n" +
		"  inner=$entry\n" +
		"done\n" +
		"if [ \"$count\" -ne 1 ] || [ ! -d \"$inner\" ]; then\n" +
		"  echo '分发包结构与预期不符：解压后应恰好有一层顶层目录（" + archiveName + "）' >&2\n" +
		"  exit 3\n" +
		"fi\n" +
		"mv \"$inner\" \"$staging/.payload\"\n" +
		// 先确认解出来的东西真能跑，再让它替换现有运行时 —— 顺序反过来，
		// 用户就损失了一个本来可用的运行时。
		"if [ ! -x \"$staging/.payload/bin/node\" ]; then\n" +
		"  echo '解压后没有找到可执行的 bin/node' >&2\n" +
		"  exit 4\n" +
		"fi\n" +
		"target=\"$root/node\"\n" +
		"previous=\n" +
		"if [ -e \"$target\" ]; then\n" +
		"  previous=\"$root/node.previous-" + fmt.Sprintf("%d", time.Now().UTC().UnixNano()) + "\"\n" +
		"  mv \"$target\" \"$previous\"\n" +
		"fi\n" +
		"if ! mv \"$staging/.payload\" \"$target\"; then\n" +
		"  if [ -n \"$previous\" ]; then mv \"$previous\" \"$target\"; fi\n" +
		"  exit 5\n" +
		"fi\n" +
		"if [ -n \"$previous\" ]; then rm -rf \"$previous\"; fi\n" +
		"rm -rf \"$staging\"\n" +
		cleanup +
		"export PATH=\"$target/bin:$PATH\"\n" +
		"node_v=$(node --version)\n" +
		"npm_v=$(npm --version 2>/dev/null || true)\n" +
		"printf 'node=%s\\n' \"$node_v\"\n" +
		"printf 'npm=%s\\n' \"$npm_v\"\n"
}

// parseCrossRuntimeInstall 解析安装脚本的输出。
func parseCrossRuntimeInstall(raw string) (nodeVersion, npmVersion string, err error) {
	values := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		index := strings.IndexByte(line, '=')
		if index <= 0 {
			continue
		}
		values[line[:index]] = line[index+1:]
	}
	nodeVersion = strings.TrimPrefix(strings.TrimSpace(values["node"]), "v")
	if nodeVersion == "" {
		return "", "", errors.New("目标环境里没有报告出 node 版本")
	}
	return nodeVersion, strings.TrimPrefix(strings.TrimSpace(values["npm"]), "v"), nil
}

// ── 跨端运行时探测 ───────────────────────────────────────────────────────────

// crossRuntimeProbeScript 一条脚本取回托管与系统两套 node/npm 的版本。
//
// 顺序与本机一致：**托管优先**于系统。理由同本机（managed 是我们自己装的、
// 版本可控、与登记表同源；系统那个可能版本过旧），但因平台而异 —— 用户 WSL 里
// 一个 Node 12 会把我们自己装好的顶掉。
const crossRuntimeProbeScript = `set -u
root=%s
managed="$root/node/bin"
if [ -x "$managed/node" ]; then
  printf 'managed_node=%%s\n' "$("$managed/node" --version 2>/dev/null || true)"
  if [ -x "$managed/npm" ]; then
    printf 'managed_npm=%%s\n' "$(PATH="$managed:$PATH" "$managed/npm" --version 2>/dev/null || true)"
  fi
fi
printf 'system_node=%%s\n' "$(command -v node >/dev/null 2>&1 && node --version 2>/dev/null || true)"
system_npm_path=$(command -v npm 2>/dev/null || true)
printf 'system_npm_path=%%s\n' "$system_npm_path"
if [ -n "$system_npm_path" ]; then
  printf 'system_npm=%%s\n' "$(npm --version 2>/dev/null || true)"
fi`

// probeRuntimeCross 探测跨端环境的 Node 运行时。
//
// target 由调用方传入：管理页在同一次请求里已经跑过一次环境探测，不该再跑一次。
func (s *Server) probeRuntimeCross(ctx context.Context, env crossEnvironment, target targetEnvironment) runtimeStatus {
	status := runtimeStatus{ID: runtimeAgentID, Origin: "none"}
	root, err := crossToolchainRoot(target)
	if err != nil {
		status.InstallSupported = false
		status.InstallBlockedReason = err.Error()
		return status
	}
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	script := fmt.Sprintf(crossRuntimeProbeScript, shellQuote(root))
	stdout, stderr, err := env.run(probeCtx, script)
	if err != nil {
		// 通道坏了与"没装运行时"是两件事，必须能分辨（这条在列表端点由 probeOk 兜住，
		// 这里把原因写进 InstallBlockedReason，界面就不会给出一个点了必失败的按钮）。
		status.InstallSupported = false
		status.InstallBlockedReason = fmt.Sprintf("无法探测运行时：%v%s", err, crossOutputDetail(stderr))
		return status
	}
	values := parseKeyValueLines(stdout)

	switch {
	case strings.TrimSpace(values["managed_node"]) != "":
		status.Origin = "managed"
		status.ManagedPath = root
		status.Version = strings.TrimPrefix(strings.TrimSpace(values["managed_node"]), "v")
		status.NpmPath = crossNpmCommand(root)
		status.NpmVersion = strings.TrimPrefix(strings.TrimSpace(values["managed_npm"]), "v")
	case strings.TrimSpace(values["system_node"]) != "":
		status.Origin = "system"
		status.Version = strings.TrimPrefix(strings.TrimSpace(values["system_node"]), "v")
		status.NpmPath = strings.TrimSpace(values["system_npm_path"])
		status.NpmVersion = strings.TrimPrefix(strings.TrimSpace(values["system_npm"]), "v")
	}
	status.Installed = status.Version != ""

	// 系统那套的事实**也要带出来**：Origin 只说"当前该用哪套"，而安装时要按
	// installKind 选（登记为系统 npm 装的就该用系统 npm）。只有一个 NpmPath 时，
	// "拿托管 npm 去装系统 prefix"这种分叉会悄悄发生。
	status.SystemNpmPath = strings.TrimSpace(values["system_npm_path"])
	status.SystemNpmVersion = strings.TrimPrefix(strings.TrimSpace(values["system_npm"]), "v")
	status.SystemNodeVersion = strings.TrimPrefix(strings.TrimSpace(values["system_node"]), "v")

	// MeetsMinimumFor **不在这里算**：这里只知道"当前生效的那套"（托管优先），
	// 而登记为系统 npm 装的工具升级时用的是系统那套，两者在同一台机器上可以不同。
	// 由 runtimeStatusFor 按 resolveCrossInstallTarget 逐工具算（判据只有一处）。

	// 能不能装：目标环境得能解压，平台也得有官方包。
	status.InstallSupported = true
	if !target.HasTar || !target.HasGzip {
		status.InstallSupported = false
		status.InstallBlockedReason = fmt.Sprintf("%s 里缺少 tar 或 gzip，无法解压官方分发包", env.describe())
	} else if _, err := target.platformKey(); err != nil {
		status.InstallSupported = false
		status.InstallBlockedReason = err.Error()
	}

	// "有新版"这一档同样要有：托管运行时自己也会过期（docs/42 §14.D）。
	if s.runtimes != nil {
		if entries, err := s.runtimes.fetchNodeVersionIndex(ctx); err == nil {
			if latest, err := pickNodeVersion(entries, "lts"); err == nil {
				status.LatestVersion = latest.nodeVersion()
				if current, err := parseSemver(status.Version); err == nil {
					if newest, err := parseSemver(status.LatestVersion); err == nil {
						status.UpdateAvailable = compareSemver(newest, current) > 0
					}
				}
			}
		}
	}
	return status
}

// runtimeMeetsMinimumFor 逐工具回答"这台 Runner 上装这个工具时，运行时够不够"。
//
// 判据与安装路径同源（meetsMinimumByInstallKind），因此界面给出的"安装/升级"入口与
// 服务端闸门的结论一致。
func (s *Server) runtimeMeetsMinimumFor(ctx context.Context, runnerID string, status runtimeStatus, toolchainRoot string) []string {
	out := []string{}
	for _, entry := range agentCatalog() {
		recorded, hasRecord, err := s.recordedInstallation(ctx, runnerID, entry.ID)
		if err != nil {
			// 读不到登记时**不**给"够用"这个信号：宁可少显示一个入口，
			// 也不要给一个基于猜测的结论。
			continue
		}
		if meetsMinimumByInstallKind(status, hasRecord, recorded, entry, toolchainRoot) {
			out = append(out, entry.ID)
		}
	}
	return out
}
