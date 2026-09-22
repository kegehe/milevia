package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	pathpkg "path"
	"strings"
	"time"
)

// 跨端执行通道（WSL / SSH）。
//
// 目标环境与"本机"的差别只有三件事，本文件把它们收成一个接口：
//
//  1. **命令要在那边跑** —— 不是 `exec.Command`，而是经 wsl.exe 或 SSH 的 exec；
//  2. **文件不一定过得去** —— WSL 经 `/mnt/<盘符>` 能直接读到本机文件（零传输），
//     SSH 不能，必须上传；
//  3. **目标平台的形状与执行本进程的平台无关** —— 在 Windows 上要选 linux-x64 的包。
//
// 第 3 条最容易漏：`nodeHomeBinary` / `localNodeLibc` 这类函数按**本机** GOOS 判断
// 布局，跨端直接复用会指向错误的形状（Windows 的 zip 把 node.exe 放顶层，
// Linux 的 tar.gz 放在 bin/ 下）。所以跨端一律走本文件的 cross* 函数。

// crossOutputLimit 是跨端命令的 stdout 上限。探测与自检的输出都很短，
// 设上限只是别让一条失控的命令把内存吃掉。
const crossOutputLimit = 64 * 1024

// crossEnvironment 是"在另一个执行环境里干活"的最小能力集。
type crossEnvironment interface {
	// describe 只用于报错文案：用户看到的是 "WSL (Ubuntu)" 或 "生产服务器"。
	describe() string
	// run 在目标环境执行一段 POSIX sh 脚本。
	//
	// 传进来的必须是**已经完全拼好、且变量都经过 shellQuote** 的脚本 ——
	// 这里不做任何转义，跨端的转义责任在构造脚本的那一侧（与 ssh_runner 一致）。
	run(ctx context.Context, script string) (stdout string, stderr string, err error)
	// sharedPath 给出"目标环境里能读到本机这个文件的路径"。
	//
	// WSL 上 `C:\a\b` → `/mnt/c/a/b`（前提是该盘符已挂载可读）；SSH 上恒为 false。
	// 返回 true 时不需要上传 —— 这是 WSL 路径能做到"零传输"的原因。
	sharedPath(hostPath string) (string, bool)
	// upload 把本机文件送进目标环境的一个目录，返回它在目标环境里的路径。
	upload(ctx context.Context, hostPath, targetDir string) (string, error)
}

// ── 目标环境事实 ─────────────────────────────────────────────────────────────

// targetEnvironment 是在目标环境里实测出来的事实。
//
// 每一项都必须**实测**，不许用默认值填：默认值会把"读不到"变成"看起来是这样"，
// 而这一层的每个判据都直接决定下载哪个包、能不能解压。
type targetEnvironment struct {
	// OS 是归一化后的 GOOS 形状（linux / darwin）。
	OS string
	// Arch 是归一化后的 GOARCH 形状（amd64 / arm64 / arm / 386）。
	Arch string
	// Libc 决定选 glibc 还是 musl 构建（Alpine 上装 glibc 包会在运行时才报 not found）。
	Libc NodeLibc
	// Home 是目标环境的家目录。安装落点一律在它下面 —— 这正是"绝不用 sudo"的前提。
	Home string
	// HomeWritable 是实测结果（不是"应该有权限"）。
	HomeWritable bool
	// HasTar / HasGzip：解压分发包要用。缺了**如实报错**，不假装成功。
	HasTar  bool
	HasGzip bool
	// Mounts 是目标环境里可读的 /mnt 子目录名。WSL 上即盘符（c、d…）；
	// 普通 Linux 通常为空（那不是"没有能力"，而是"不需要这条路"）。
	Mounts []string
}

// platformKey 把**目标环境**的形状映射成官方分发包平台键。
//
// 注意入参是目标环境的 uname 结果，而不是 runtime.GOOS / GOARCH ——
// 在 Windows 上给 WSL 装包时，这两个值恰好相反。
func (t targetEnvironment) platformKey() (NodePlatformKey, error) {
	if t.OS == "" || t.Arch == "" {
		return "", errors.New("目标环境的操作系统/架构未知（探测没有返回）")
	}
	return nodePlatformKey(t.OS, t.Arch, t.Libc)
}

// targetProbeScript 用**一条**脚本取回全部事实。
//
// 一条而不是多条：每条 WSL/SSH 命令都要付一次进程/连接往返，而这次探测在每次
// 打开管理页时都可能跑（见 docs/42 §14.G：不放热路径，但也不该是最慢的那一步）。
//
// 所有取值都做了"命令不存在也要继续"的处理（`|| true`）：某个命令缺失是**信息**，
// 不是探测失败 —— 探测失败与"没有这个命令"必须能分辨，否则报错会指错方向。
const targetProbeScript = `set -u
printf 'os=%s\n' "$(uname -s 2>/dev/null || true)"
printf 'arch=%s\n' "$(uname -m 2>/dev/null || true)"
printf 'home=%s\n' "${HOME:-}"
printf 'alpine=%s\n' "$([ -f /etc/alpine-release ] && echo yes || echo no)"
printf 'tar=%s\n' "$(command -v tar 2>/dev/null || true)"
printf 'gzip=%s\n' "$(command -v gzip 2>/dev/null || true)"
printf 'mounts=%s\n' "$(for m in /mnt/*; do [ -r "$m" ] && printf '%s ' "${m##*/}"; done 2>/dev/null || true)"
printf 'homewritable=%s\n' "$(h=${HOME:-}; if [ -n "$h" ] && [ -w "$h" ]; then echo yes; else echo no; fi)"`

// probeTargetEnvironment 在目标环境里跑一次探测。
func probeTargetEnvironment(ctx context.Context, env crossEnvironment) (targetEnvironment, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	stdout, stderr, err := env.run(probeCtx, targetProbeScript)
	if err != nil {
		return targetEnvironment{}, fmt.Errorf("无法探测%s的环境：%w%s", env.describe(), err, crossOutputDetail(stderr))
	}
	parsed, err := parseTargetProbe(stdout)
	if err != nil {
		return targetEnvironment{}, fmt.Errorf("无法解析%s的环境信息：%w", env.describe(), err)
	}
	return parsed, nil
}

// parseTargetProbe 解析探测脚本的输出。
//
// 拆成纯函数是为了能用**真实的 Linux 输出片段**做夹具 —— 这一层的判据全是
// "那边的机器长什么样"，凭印象编脚本等于把猜测固化成测试。
func parseTargetProbe(raw string) (targetEnvironment, error) {
	values := parseKeyValueLines(raw)
	if strings.TrimSpace(values["os"]) == "" {
		return targetEnvironment{}, errors.New("没有取到操作系统（uname -s 无输出）")
	}
	if strings.TrimSpace(values["arch"]) == "" {
		return targetEnvironment{}, errors.New("没有取到处理器架构（uname -m 无输出）")
	}

	env := targetEnvironment{
		OS:   normalizeTargetOS(values["os"]),
		Arch: normalizeTargetArch(values["arch"]),
		// libc 只看 Alpine 标记：musl 发行版极少，而 `/etc/alpine-release` 是
		// 最可靠的判据（与本机 localNodeLibc 同源，理由一致）。
		Libc:         NodeLibcGlibc,
		Home:         strings.TrimSpace(values["home"]),
		HomeWritable: values["homewritable"] == "yes",
		HasTar:       strings.TrimSpace(values["tar"]) != "",
		HasGzip:      strings.TrimSpace(values["gzip"]) != "",
	}
	if values["alpine"] == "yes" {
		env.Libc = NodeLibcMusl
	}
	for _, name := range strings.Fields(values["mounts"]) {
		name = strings.TrimSpace(name)
		if name != "" {
			env.Mounts = append(env.Mounts, name)
		}
	}
	if env.OS == "" {
		return targetEnvironment{}, fmt.Errorf("不认识的操作系统：%s", strings.TrimSpace(values["os"]))
	}
	if env.Arch == "" {
		return targetEnvironment{}, fmt.Errorf("不认识的处理器架构：%s", strings.TrimSpace(values["arch"]))
	}
	return env, nil
}

// normalizeTargetOS 把 uname -s 归一化成 GOOS 形状。
func normalizeTargetOS(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "linux":
		return "linux"
	case "darwin":
		return "darwin"
	}
	return ""
}

// normalizeTargetArch 把 uname -m 归一化成 GOARCH 形状。
func normalizeTargetArch(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	case "armv7l", "armv6l", "armv8l":
		return "arm"
	case "i386", "i686":
		return "386"
	}
	return ""
}

// firstNonEmpty 返回第一个非空值（去空白后）。
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// ── 目标环境里的落点 ─────────────────────────────────────────────────────────

// crossToolchainRoot 给出目标环境里的托管工具链根目录（`~/.local/share/milevia/toolchain`）。
//
// 与本机同构，于是"哪份文件该在哪"这套知识只有一份。**一律落在 $HOME 下**：
// 这正是"绝不用 sudo"的落点，也是"删目录即还原"的前提。
func crossToolchainRoot(env targetEnvironment) (string, error) {
	if env.Home == "" {
		return "", errors.New("目标环境没有可用的家目录（$HOME 为空），平台不在家目录之外安装")
	}
	if !env.HomeWritable {
		return "", errors.New("目标环境的 $HOME 不可写；平台不会在这里提权安装（请确认该账户的家目录可写）")
	}
	return pathpkg.Join(env.Home, ".local", "share", "milevia", "toolchain"), nil
}

// 跨端目标一律是非 Windows（WSL 是 Linux；SSH 远端是 remote-linux），因此布局
// 固定为 `bin/` 下。**不能**复用 nodeHomeBinary —— 那个按本机 GOOS 判断，
// 本机是 Windows 而目标是 Linux 时会指向 Windows 的顶层布局。
func crossNodeBinary(toolchainRoot string) string {
	return pathpkg.Join(toolchainRoot, "node", "bin", "node")
}

func crossNpmCommand(toolchainRoot string) string {
	return pathpkg.Join(toolchainRoot, "node", "bin", "npm")
}

func crossNpmGlobalPrefix(toolchainRoot string) string {
	return pathpkg.Join(toolchainRoot, "npm-global")
}

// ── WSL ─────────────────────────────────────────────────────────────────────

// wslCrossEnvironment 经 wsl.exe 在 WSL 发行版里执行。
type wslCrossEnvironment struct {
	runner *wslAgentRunner
	// visibleMounts 是探测到的、可读的 /mnt 子目录（盘符，小写）。
	// nil 表示**还没探测过** —— 这时 sharedPath 一律返回 false（保守）。
	visibleMounts map[string]bool
}

func (e *wslCrossEnvironment) describe() string {
	if e.runner != nil && e.runner.distro != "" {
		return "WSL (" + e.runner.distro + ")"
	}
	return "WSL"
}

func (e *wslCrossEnvironment) run(ctx context.Context, script string) (string, string, error) {
	wslPath, err := wslExePath()
	if err != nil {
		return "", "", fmt.Errorf("无法访问 wsl.exe：%w", err)
	}
	// 用 `--` 形式交给默认 shell 重展开 base64 token（`-e` 会按空格把 -c 的实参
	// 提前拆开、引号丢失）。编码由 wslEncodeArg 负责，与 wslNativeCommand 同源。
	cmd := exec.CommandContext(ctx, wslPath, "-d", e.runner.distro, "--", "sh", "-c", wslEncodeArg(script))
	configureProcessGroup(cmd)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), stderr.String(), fmt.Errorf("%s 执行失败：%w", e.describe(), err)
	}
	return stdout.String(), stderr.String(), nil
}

// sharedPath 把 `C:\a\b` 映射成 `/mnt/c/a/b`。
//
// **只在探测确认该盘符可读之后**才返回 true：WSL 可以配 `automount=false`，
// 那时 /mnt/c 根本不存在，而我们若照直返回路径，用户拿到的会是 tar 的
// "找不到文件" —— 一个指不到真正原因的报错。
func (e *wslCrossEnvironment) sharedPath(hostPath string) (string, bool) {
	if e.visibleMounts == nil {
		return "", false
	}
	if len(hostPath) < 3 || hostPath[1] != ':' {
		return "", false
	}
	volume := strings.ToLower(hostPath[:1])
	if !e.visibleMounts[volume] {
		return "", false
	}
	rest := strings.ReplaceAll(hostPath[2:], `\`, "/")
	rest = strings.TrimPrefix(rest, "/")
	return "/mnt/" + volume + "/" + rest, true
}

// upload 对 WSL 不该被用到：本机盘对 WSL 可选挂载，走 sharedPath 那条路。
//
// 真的走到这里说明 /mnt 不可用（automount 关掉了）。如实报错并说清怎么办，
// 而不是退化成"把 30MB 用 base64 塞进命令行"这种会静默截断的做法。
func (e *wslCrossEnvironment) upload(context.Context, string, string) (string, error) {
	return "", fmt.Errorf("%s 里看不到本机磁盘（/mnt 未挂载可读）；请在 WSL 里启用 automount 后重试", e.describe())
}

// recordVisibleMounts 由探测把可读的 /mnt 子目录登记进来。
func (e *wslCrossEnvironment) recordVisibleMounts(mounts []string) {
	visible := map[string]bool{}
	for _, name := range mounts {
		visible[strings.ToLower(strings.TrimSpace(name))] = true
	}
	e.visibleMounts = visible
}

// ── SSH ─────────────────────────────────────────────────────────────────────

// sshCrossEnvironment 经 SSH 在远端执行。
type sshCrossEnvironment struct {
	runner *sshRunner
}

func (e *sshCrossEnvironment) describe() string {
	if e.runner != nil {
		if name := strings.TrimSpace(e.runner.connName); name != "" {
			return name
		}
	}
	return "远程服务器"
}

func (e *sshCrossEnvironment) run(ctx context.Context, script string) (string, string, error) {
	stdout, stderr, err := e.runner.client.execCommandSeparate(ctx, script, crossOutputLimit)
	if err != nil {
		return string(stdout), string(stderr), fmt.Errorf("%s 执行失败：%w", e.describe(), err)
	}
	return string(stdout), string(stderr), nil
}

// sharedPath 对 SSH 恒为 false：远端看不到本机的文件系统。
func (e *sshCrossEnvironment) sharedPath(string) (string, bool) {
	return "", false
}

// upload 用 SFTP 流式写入。
//
// **不用 SFTPFilesystem.WriteFile**：它绑着项目沙箱 rootPath（工具链要落在
// $HOME，不在任何项目里），而且签名要求 expectedVersion 那种编辑语义。
// 这里要的只是"把一个文件放过去"，用 sftp.Client 直接流式拷 —— 顺带也就
// 不存在"30MB 包整块进内存"那个问题（docs/42 §7.5 记的顾虑）。
func (e *sshCrossEnvironment) upload(ctx context.Context, hostPath, targetDir string) (string, error) {
	local, err := os.Open(hostPath)
	if err != nil {
		return "", err
	}
	defer local.Close()
	info, err := local.Stat()
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s 是目录，上传只接受文件", hostPath)
	}
	client, err := e.runner.client.getSFTPClient(ctx)
	if err != nil {
		return "", fmt.Errorf("无法建立 SFTP 连接：%w", err)
	}
	if err := client.MkdirAll(targetDir); err != nil {
		return "", fmt.Errorf("无法在%s创建目录 %s：%w", e.describe(), targetDir, err)
	}
	remote := pathpkg.Join(targetDir, info.Name())
	file, err := client.Create(remote)
	if err != nil {
		return "", fmt.Errorf("无法在%s创建文件 %s：%w", e.describe(), remote, err)
	}
	if _, err := io.Copy(file, local); err != nil {
		file.Close()
		_ = client.Remove(remote)
		return "", fmt.Errorf("上传到%s失败：%w", e.describe(), err)
	}
	if err := file.Close(); err != nil {
		_ = client.Remove(remote)
		return "", fmt.Errorf("上传到%s失败：%w", e.describe(), err)
	}
	return remote, nil
}

// crossEnvironmentFor 选出某个 Runner 的跨端执行通道。
//
// 返回 nil 表示"这个 Runner 没有跨端执行能力"（本机 runner、或注册表里是别的实现）。
func (s *Server) crossEnvironmentFor(runnerID string) crossEnvironment {
	if isLocalRunnerID(runnerID) {
		return nil
	}
	runner, ok := s.runnerRegistry.get(runnerID)
	if !ok {
		return nil
	}
	switch typed := runner.(type) {
	case *wslAgentRunner:
		return &wslCrossEnvironment{runner: typed}
	case *sshRunner:
		return &sshCrossEnvironment{runner: typed}
	}
	return nil
}

// crossOutputDetail 把目标环境的 stderr 附到报错文案里（截断+去空行）。
func crossOutputDetail(output string) string {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return ""
	}
	if index := strings.IndexByte(trimmed, '\n'); index >= 0 {
		trimmed = strings.TrimSpace(trimmed[:index])
	}
	if len(trimmed) > 300 {
		trimmed = trimmed[:300] + "…"
	}
	return "：" + trimmed
}

// parseKeyValueLines 解析 `key=value` 形式的输出（值保留原样，由调用方决定是否 TrimSpace）。
//
// 探测脚本一律用这个形状输出：它在两端都好写（`printf 'k=%s\n' "$v"`），
// 而且天然容忍"某个命令不存在导致某一行缺失"—— 缺失就是缺失，不是空串。
func parseKeyValueLines(raw string) map[string]string {
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
	return values
}
