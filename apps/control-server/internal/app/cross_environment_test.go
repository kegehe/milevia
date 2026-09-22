package app

import (
	"archive/tar"
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 跨端安装的测试。
//
// 全套都不碰真的 WSL / SSH：把目标环境做成一个**可控的假实现**（fakeCrossEnvironment），
// 于是可以精确断言"我们到底在那边跑了一条什么脚本"。这一层的判据几乎全是
// "发出的那条脚本长什么样"，而脚本错了的表现是目标环境上的一次失败 —— 靠真机测
// 既慢又只能覆盖到手上有的那台机器。

type fakeCrossReply struct {
	match  string
	out    string
	stderr string
	err    error
}

type fakeCrossEnvironment struct {
	name    string
	replies []fakeCrossReply
	// scripts 记录收到的每一条脚本，供断言。
	scripts []string
	// uploads 记录上传过的本机路径。
	uploads []string
	// sharedRoot 非空表示"本机文件在目标环境里能直接看到"，映射到该前缀下。
	// 空串表示走上传那条路（SSH 的形状）。
	sharedRoot string
}

func (f *fakeCrossEnvironment) describe() string { return f.name }

func (f *fakeCrossEnvironment) run(_ context.Context, script string) (string, string, error) {
	f.scripts = append(f.scripts, script)
	for _, reply := range f.replies {
		if strings.Contains(script, reply.match) {
			return reply.out, reply.stderr, reply.err
		}
	}
	// 没有预设就报错，**不**返回空输出：空输出会被上层当成"那边什么都没报告"，
	// 于是测试会以一条含糊的失败通过，而不是指出漏了预设。
	return "", "", fmt.Errorf("fake 目标环境没有为这条脚本预设输出：%.160s", script)
}

func (f *fakeCrossEnvironment) sharedPath(hostPath string) (string, bool) {
	if f.sharedRoot == "" {
		return "", false
	}
	return pathpkg.Join(f.sharedRoot, filepath.Base(hostPath)), true
}

func (f *fakeCrossEnvironment) upload(_ context.Context, hostPath, targetDir string) (string, error) {
	f.uploads = append(f.uploads, hostPath)
	return pathpkg.Join(targetDir, filepath.Base(hostPath)), nil
}

// scriptContaining 取出收到的脚本里含某关键字的那一条。
func (f *fakeCrossEnvironment) scriptContaining(t *testing.T, needle string) string {
	t.Helper()
	for _, script := range f.scripts {
		if strings.Contains(script, needle) {
			return script
		}
	}
	t.Fatalf("没有收到含 %q 的脚本；实际收到 %d 条：%v", needle, len(f.scripts), f.scripts)
	return ""
}

// newCrossTestServer 建一个带运行时管理器与数据库的 Server（不碰用户目录）。
func newCrossTestServer(t *testing.T) *Server {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "cross.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	server := &Server{
		db: db, paths: newAgentPathResolver(Config{}), runtimes: newNodeRuntimeManager(),
		runnerUpdating: map[runnerAgentKey]bool{},
	}
	if err := server.migrateAgentInstallations(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := server.migrateRunnerInstallGrants(context.Background()); err != nil {
		t.Fatal(err)
	}
	return server
}

// ── 探测解析 ─────────────────────────────────────────────────────────────────

// ubuntuProbeOutput 是一台普通 Ubuntu 的真实形状。
const ubuntuProbeOutput = `os=Linux
arch=x86_64
home=/home/dev
alpine=no
tar=/usr/bin/tar
gzip=/usr/bin/gzip
curl=/usr/bin/curl
wget=
sha256=/usr/bin/sha256sum
mounts=c d 
homewritable=yes
`

// alpineProbeOutput 是一台 Alpine（musl）的形状。
const alpineProbeOutput = `os=Linux
arch=aarch64
home=/root
alpine=yes
tar=/bin/tar
gzip=/bin/gzip
curl=
wget=/usr/bin/wget
sha256=
mounts=
homewritable=no
`

func TestParseTargetProbeReadsLinuxFacts(t *testing.T) {
	env, err := parseTargetProbe(ubuntuProbeOutput)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if env.OS != "linux" || env.Arch != "amd64" {
		t.Fatalf("os/arch = %q/%q", env.OS, env.Arch)
	}
	if env.Libc != NodeLibcGlibc {
		t.Fatalf("libc = %q，期望 glibc", env.Libc)
	}
	if env.Home != "/home/dev" || !env.HomeWritable {
		t.Fatalf("home = %q writable=%v", env.Home, env.HomeWritable)
	}
	if !env.HasTar || !env.HasGzip {
		t.Fatal("tar/gzip 应当判定为可用")
	}
	if len(env.Mounts) != 2 || env.Mounts[0] != "c" {
		t.Fatalf("mounts = %v", env.Mounts)
	}
}

func TestParseTargetProbeDetectsMuslAndUnwritableHome(t *testing.T) {
	env, err := parseTargetProbe(alpineProbeOutput)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	// musl 判据来自 /etc/alpine-release：装 glibc 构建的包会在**运行时**才报
	// not found，而用户拿到的是一句"安装成功"。
	if env.Libc != NodeLibcMusl {
		t.Fatalf("libc = %q，期望 musl", env.Libc)
	}
	if env.Arch != "arm64" {
		t.Fatalf("arch = %q，期望 arm64", env.Arch)
	}
	if env.HomeWritable {
		t.Fatal("家目录不可写时必须如实报 false")
	}
	if _, err := env.platformKey(); err == nil {
		t.Fatal("arm64 + musl 没有官方分发包，应当**如实拒绝**而不是回落 glibc" +
			"（装 glibc 包会在运行时才报 not found，而用户拿到的是“安装成功”）")
	}
}

func TestParseTargetProbeRefusesToGuessMissingFacts(t *testing.T) {
	// 缺 os：不能默认成 linux —— 默认值会把"读不到"变成"看起来是这样"，
	// 而下一步就是按这个猜测去下载一个包。
	if _, err := parseTargetProbe("arch=x86_64\nhome=/home/dev\n"); err == nil {
		t.Fatal("缺 uname -s 时应当报错")
	}
	if _, err := parseTargetProbe("os=Linux\nhome=/home/dev\n"); err == nil {
		t.Fatal("缺 uname -m 时应当报错")
	}
	if _, err := parseTargetProbe("os=Plan9\narch=x86_64\n"); err == nil {
		t.Fatal("不认识的操作系统应当报错")
	}
}

// ── WSL 的 /mnt 映射 ─────────────────────────────────────────────────────────

func TestWSLSharedPathNeedsProbedMount(t *testing.T) {
	env := &wslCrossEnvironment{runner: &wslAgentRunner{distro: "Ubuntu"}}
	// 还没探测过：保守返回 false。若这里返回 true，automount=false 的 WSL 上
	// 我们就会给出一个不存在的路径，而 tar 的报错会指向"找不到文件"。
	if _, ok := env.sharedPath(`C:\Users\dev\archive.tar.gz`); ok {
		t.Fatal("未探测挂载时不该给出共享路径")
	}
	env.recordVisibleMounts([]string{"c"})
	got, ok := env.sharedPath(`C:\Users\dev\archive.tar.gz`)
	if !ok {
		t.Fatal("C 盘可读时应当映射成功")
	}
	if got != "/mnt/c/Users/dev/archive.tar.gz" {
		t.Fatalf("映射结果 = %q", got)
	}
	// 未挂载的盘符依旧不可用。
	if _, ok := env.sharedPath(`D:\x\y.tar.gz`); ok {
		t.Fatal("D 盘没挂载时不该给出共享路径")
	}
	// 小写盘符也要能映射（Windows 路径的大小写不固定）。
	if _, ok := env.sharedPath(`c:\tmp\a.tar.gz`); !ok {
		t.Fatal("小写盘符应当同样映射")
	}
}

// ── 落点与脚本 ───────────────────────────────────────────────────────────────

func TestCrossToolchainRootRefusesUnusableHome(t *testing.T) {
	if _, err := crossToolchainRoot(targetEnvironment{Home: "", HomeWritable: true}); err == nil {
		t.Fatal("$HOME 为空时必须拒绝（否则会往 / 下装）")
	}
	if _, err := crossToolchainRoot(targetEnvironment{Home: "/home/dev", HomeWritable: false}); err == nil {
		t.Fatal("$HOME 不可写时必须拒绝（平台不提权）")
	}
	root, err := crossToolchainRoot(targetEnvironment{Home: "/home/dev", HomeWritable: true})
	if err != nil {
		t.Fatal(err)
	}
	if root != "/home/dev/.local/share/milevia/toolchain" {
		t.Fatalf("落点 = %q", root)
	}
}

func TestCrossRuntimeInstallScriptKeepsNonWindowsLayout(t *testing.T) {
	script := crossRuntimeInstallScript("/home/dev/.local/share/milevia/toolchain", "/tmp/staging", "/tmp/a.tar.gz", false, "node-v24.21.0-linux-x64.tar.gz")
	// 目标一定是非 Windows，因此布局固定 bin/ 下。若这里退回 nodeHomeBinary
	// （按**本机** GOOS 判断），在 Windows 上就会去找顶层的 node.exe。
	if !strings.Contains(script, ".payload/bin/node") {
		t.Fatalf("自检路径不是 bin/node：%s", script)
	}
	// busybox 的 tar 没有 --strip-components，而 musl 发行版正是 busybox。
	// 用 POSIX 循环数顶层目录，数量不等于 1 就如实报错。
	if strings.Contains(script, "--strip-components") {
		t.Fatal("不该用 --strip-components（busybox 的 tar 不支持）")
	}
	if !strings.Contains(script, "count=$((count + 1))") {
		t.Fatal("缺少顶层目录数量的判断")
	}
	// npm 是 `#!/usr/bin/env node` 的脚本：不前置 PATH 时报的是
	// `env: node: No such file`，与"没装 npm"看起来一样。
	if !strings.Contains(script, `export PATH="$target/bin:$PATH"`) {
		t.Fatal("执行 npm 之前必须把托管 node 的 bin 前置到 PATH")
	}
}

func TestCrossRuntimeInstallScriptQuotesEveryInterpolatedPath(t *testing.T) {
	// 家目录带空格在 Linux 上少见但合法；不引用的结果是脚本被切成两个词，
	// 报错完全指不到原因。
	script := crossRuntimeInstallScript("/home/a b/.local/share/milevia/toolchain", "/tmp/sta ging", "/tmp/x y.tar.gz", false, "n.tar.gz")
	if !strings.Contains(script, `root='/home/a b/.local/share/milevia/toolchain'`) {
		t.Fatalf("root 没有被单引号括起来：%s", script)
	}
	if !strings.Contains(script, `staging='/tmp/sta ging'`) {
		t.Fatalf("staging 没有被单引号括起来：%s", script)
	}
}

// TestCrossRuntimeInstallScriptNeverDeletesSharedArchive 是一条**安全**断言。
//
// WSL 走 /mnt 时压缩包在**本机磁盘**上。若脚本里对它执行 rm，删掉的是本机的文件，
// 而用户完全看不出是谁删的。
func TestCrossRuntimeInstallScriptNeverDeletesSharedArchive(t *testing.T) {
	shared := crossRuntimeInstallScript("/root", "/tmp/s", "/mnt/c/Users/dev/a.tar.gz", true, "a.tar.gz")
	if strings.Contains(shared, "rm -f '/mnt/c/Users/dev/a.tar.gz'") {
		t.Fatal("共享（/mnt）路径的压缩包在本机磁盘上，绝不能在目标环境里 rm 它")
	}
	uploaded := crossRuntimeInstallScript("/root", "/tmp/s", "/tmp/uploaded.tar.gz", false, "a.tar.gz")
	if !strings.Contains(uploaded, "rm -f '/tmp/uploaded.tar.gz'") {
		t.Fatal("上传过去的压缩包应当在收尾时清掉（30MB 不该留在那边）")
	}
}

func TestCrossAgentInstallScriptSelfCheckStaysInsidePrefix(t *testing.T) {
	script := crossAgentInstallScript("/opt/npm/bin/npm", "/opt/npm-global", "@anthropic-ai/claude-code@latest", "claude")
	// npm 与各家 CLI 都是 node 脚本，PATH 里没有 node 时它们的报错会误导方向。
	if !strings.Contains(script, `export PATH='/opt/npm/bin':$PATH`) {
		t.Fatalf("没有把 npm 所在目录前置到 PATH：%s", script)
	}
	// 自检必须落在解析出来的 prefix 里。查 `command -v claude` 会把**用户自己装的那一份**
	// 认成我们的安装，于是登记了别人的路径，之后升级去升级别人的那份。
	if strings.Contains(script, "command -v claude") {
		t.Fatalf("自检不该查 PATH：%s", script)
	}
	if !strings.Contains(script, `target="$prefix/bin/claude"`) {
		t.Fatalf("自检路径不是 prefix 内的产物：%s", script)
	}
	// 包名要引起来：npm 的作用域包名本身含 @ 与 /，将来出现别的字符也不该被 shell 解释。
	if !strings.Contains(script, `'@anthropic-ai/claude-code@latest'`) {
		t.Fatalf("包名没有被引起来：%s", script)
	}
}

func TestCrossAgentInstallScriptUsesNpmGlobalPrefixWhenUnmanaged(t *testing.T) {
	script := crossAgentInstallScript("/usr/bin/npm", "", "openai/codex@latest", "codex")
	if strings.Contains(script, "--prefix") {
		t.Fatal("prefix 为空时不该传 --prefix（那会用 npm 自己的全局 prefix）")
	}
	if !strings.Contains(script, "prefix=$(npm prefix -g)") {
		t.Fatal("自检仍要知道真实 prefix：应当问 npm prefix -g")
	}
}

// ── 编排：假目标环境 + 假官方源 ──────────────────────────────────────────────

// fakeNodeSource 起一个假的官方分发源（index.json / SHASUMS256.txt / 包本体）。
//
// 形状照着真实抓到的数据造 —— 这一层的判据全是"官方长什么样"。
func fakeNodeSource(t *testing.T, version, platform, archiveName string, archive []byte) string {
	t.Helper()
	sum := hex.EncodeToString(sha256Sum(archive))
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/index.json"):
			fmt.Fprintf(w, `[{"version":"v%s","date":"2026-08-20","lts":"Krypton","files":[%q]}]`, version, platform)
		case strings.HasSuffix(r.URL.Path, "/SHASUMS256.txt"):
			fmt.Fprintf(w, "%s  %s\n", sum, archiveName)
		case strings.Contains(r.URL.Path, "/v"+version+"/"):
			w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(source.Close)
	return source.URL
}

func TestInstallManagedRuntimeCrossEndToEnd(t *testing.T) {
	version := "24.21.0"
	topDir := "node-v" + version + "-linux-x64"
	archiveName := topDir + ".tar.gz"
	workDir := t.TempDir()
	archivePath := filepath.Join(workDir, archiveName)
	writeTarGz(t, archivePath, []tarFixture{
		{name: topDir + "/", kind: tar.TypeDir, mode: 0o755},
		{name: topDir + "/bin/node", kind: tar.TypeReg, mode: 0o755, body: "#!/bin/sh\necho v24.21.0\n"},
		{name: topDir + "/bin/npm", kind: tar.TypeReg, mode: 0o755, body: "#!/bin/sh\necho 11.0.0\n"},
	})
	archive, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(nodeRuntimeMirrorEnv, fakeNodeSource(t, version, "linux-x64", archiveName, archive))

	env := &fakeCrossEnvironment{
		name: "WSL (Ubuntu)",
		replies: []fakeCrossReply{
			{match: "uname -s", out: ubuntuProbeOutput},
			{match: "tar -xzf", out: "node=v24.21.0\nnpm=11.0.0\n"},
		},
		sharedRoot: "/mnt/c/Users/dev",
	}
	server := newCrossTestServer(t)
	installation, err := server.installManagedRuntimeCross(context.Background(), "wsl-local", env, installRuntimeRequest{})
	if err != nil {
		t.Fatalf("安装失败：%v", err)
	}

	// 版本不带 v（与本机 runner 的 Version() 同形）。
	if installation.Version != "24.21.0" {
		t.Fatalf("登记版本 = %q", installation.Version)
	}
	if installation.BinaryPath != "/home/dev/.local/share/milevia/toolchain/node/bin/node" {
		t.Fatalf("登记路径 = %q", installation.BinaryPath)
	}
	if installation.Prefix != "/home/dev/.local/share/milevia/toolchain/npm-global" {
		t.Fatalf("登记 prefix = %q", installation.Prefix)
	}
	if installation.InstallKind != "managed-toolchain" {
		t.Fatalf("installKind = %q", installation.InstallKind)
	}
	// 走的是共享那条路（/mnt），因此**没有上传**。
	if len(env.uploads) != 0 {
		t.Fatalf("WSL 能直接读到本机文件时不该上传，实际上传了 %v", env.uploads)
	}
	installScript := env.scriptContaining(t, "tar -xzf")
	if !strings.Contains(installScript, "/mnt/c/Users/dev/"+archiveName) {
		t.Fatalf("安装脚本没有用 /mnt 共享路径：%s", installScript)
	}

	// 登记在库里，供管理页显示"装在哪"。
	recorded, err := server.agentInstallationFor(context.Background(), "wsl-local", runtimeAgentID)
	if err != nil {
		t.Fatalf("没有登记：%v", err)
	}
	if recorded.Version != "24.21.0" {
		t.Fatalf("库里的版本 = %q", recorded.Version)
	}
}

func TestInstallManagedRuntimeCrossUploadsWhenNotShared(t *testing.T) {
	version := "24.21.0"
	topDir := "node-v" + version + "-linux-x64"
	archiveName := topDir + ".tar.gz"
	workDir := t.TempDir()
	archivePath := filepath.Join(workDir, archiveName)
	writeTarGz(t, archivePath, []tarFixture{
		{name: topDir + "/bin/node", kind: tar.TypeReg, mode: 0o755, body: "#!/bin/sh\necho v24.21.0\n"},
	})
	archive, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(nodeRuntimeMirrorEnv, fakeNodeSource(t, version, "linux-x64", archiveName, archive))

	env := &fakeCrossEnvironment{
		name: "prod-server",
		replies: []fakeCrossReply{
			{match: "uname -s", out: ubuntuProbeOutput},
			{match: "tar -xzf", out: "node=v24.21.0\nnpm=11.0.0\n"},
		},
		// sharedRoot 留空：SSH 的形状，必须走上传。
	}
	server := newCrossTestServer(t)
	if _, err := server.installManagedRuntimeCross(context.Background(), "ssh-prod", env, installRuntimeRequest{}); err != nil {
		t.Fatalf("安装失败：%v", err)
	}
	if len(env.uploads) != 1 {
		t.Fatalf("SSH 场景应当上传一次压缩包，实际上传 %d 次", len(env.uploads))
	}
	installScript := env.scriptContaining(t, "tar -xzf")
	// 上传过去的压缩包不该留在远端（30MB）。
	if !strings.Contains(installScript, "rm -f ") {
		t.Fatal("上传过去的压缩包应当清掉")
	}
	// 压缩包**不能**落在 staging 里：脚本开头会 `rm -rf "$staging"` 清上次残留，
	// 那会把它自己刚上传的包删掉，tar 随后报"找不到文件"。
	archiveLine := ""
	for _, line := range strings.Split(installScript, "\n") {
		if strings.HasPrefix(line, "archive=") {
			archiveLine = strings.TrimRight(line, "\r")
		}
	}
	if archiveLine == "" {
		t.Fatalf("脚本里没有 archive= 行：%s", installScript)
	}
	if strings.Contains(archiveLine, "node.staging-") {
		t.Fatalf("压缩包落在 staging 里，会被脚本开头的 rm -rf 删掉：%s", archiveLine)
	}
}

func TestInstallManagedRuntimeCrossRefusesMissingUnpacker(t *testing.T) {
	env := &fakeCrossEnvironment{
		name: "Alpine 小机器",
		replies: []fakeCrossReply{
			// 没有 tar/gzip。
			{match: "uname -s", out: "os=Linux\narch=x86_64\nhome=/root\ntar=\ngzip=\nmounts=\nhomewritable=yes\n"},
		},
	}
	server := newCrossTestServer(t)
	_, err := server.installManagedRuntimeCross(context.Background(), "ssh-prod", env, installRuntimeRequest{})
	if err == nil {
		t.Fatal("缺 tar/gzip 时应当如实报错，而不是假装成功")
	}
	// 文案要可操作：用户需要知道是缺东西，而不是"安装失败"。
	if !strings.Contains(err.Error(), "tar") || !strings.Contains(err.Error(), "gzip") {
		t.Fatalf("报错没有指出缺什么：%v", err)
	}
}

func TestInstallAgentCLICrossEndToEnd(t *testing.T) {
	env := &fakeCrossEnvironment{
		name: "WSL (Ubuntu)",
		replies: []fakeCrossReply{
			// 环境探测与运行时探测是两条不同的脚本（前者取 uname，后者取 node/npm 版本），
			// 所以必须分别预设 —— 这也顺带钉住了"两条脚本各司其职"。
			{match: "uname -s", out: ubuntuProbeOutput},
			{match: "managed_node", out: "managed_node=v24.21.0\nmanaged_npm=11.0.0\n"},
			{match: "install -g", out: "binary=/home/dev/.local/share/milevia/toolchain/npm-global/bin/claude\nversion=2.1.217 (Claude Code)\n"},
		},
	}
	server := newCrossTestServer(t)
	installation, err := server.installAgentCLICross(context.Background(), "wsl-local", env, "claude-code", "")
	if err != nil {
		t.Fatalf("安装失败：%v", err)
	}
	if installation.BinaryPath != "/home/dev/.local/share/milevia/toolchain/npm-global/bin/claude" {
		t.Fatalf("登记路径 = %q", installation.BinaryPath)
	}
	// 版本必须剥掉产品名后缀：登记表里的版本要与界面显示的版本**逐字一致**，
	// 否则"当前 X → 最新 Y"这类比较会在两处得出不同结论。
	if installation.Version != "2.1.217" {
		t.Fatalf("登记版本 = %q，期望剥掉 (Claude Code)", installation.Version)
	}
	if installation.InstallKind != installKindNpmManaged {
		t.Fatalf("installKind = %q", installation.InstallKind)
	}
	if installation.Prefix != "/home/dev/.local/share/milevia/toolchain/npm-global" {
		t.Fatalf("prefix = %q", installation.Prefix)
	}
}

func TestInstallAgentCLICrossReportsMissingRuntimeNotMissingNpm(t *testing.T) {
	// 干净机器：既没有托管运行时，也没有系统 node/npm。
	env := &fakeCrossEnvironment{
		name: "prod-server",
		replies: []fakeCrossReply{
			{match: "uname -s", out: "os=Linux\narch=x86_64\nhome=/home/dev\ntar=/usr/bin/tar\ngzip=/usr/bin/gzip\nmounts=\nhomewritable=yes\n"},
			{match: "managed_node", out: "system_node=\nsystem_npm=\n"},
		},
	}
	server := newCrossTestServer(t)
	_, err := server.installAgentCLICross(context.Background(), "ssh-prod", env, "claude-code", "")
	if err == nil {
		t.Fatal("没有 npm 时应当报错")
	}
	// 这一档必须与"版本太低"分开说：用户的下一步动作完全不同。
	if !strings.Contains(err.Error(), "Node.js 运行时") {
		t.Fatalf("报错应当指向运行时缺失：%v", err)
	}
}

func TestInstallAgentCLICrossRejectsBadVersionSelector(t *testing.T) {
	env := &fakeCrossEnvironment{
		name: "prod-server",
		replies: []fakeCrossReply{
			{match: "uname -s", out: ubuntuProbeOutput},
			{match: "managed_node", out: "managed_node=v24.21.0\nmanaged_npm=11.0.0\n"},
		},
	}
	server := newCrossTestServer(t)
	// 版本号是**命令参数**，只允许 latest 或形如 1.2.3 的版本号。
	if _, err := server.installAgentCLICross(context.Background(), "ssh-prod", env, "claude-code", "1.2.3; rm -rf /"); err == nil {
		t.Fatal("非法版本号应当被拒")
	}
	// 被拒时**一条安装命令都不该发出去** —— 校验必须发生在拼脚本之前。
	if strings.Contains(strings.Join(env.scripts, "\n"), "install -g") {
		t.Fatal("非法版本号在发出安装命令之前就该被拒")
	}
}

// ── 跨端运行时探测 ───────────────────────────────────────────────────────────

func TestProbeRuntimeCrossPrefersManagedOverSystem(t *testing.T) {
	env := &fakeCrossEnvironment{
		name: "WSL (Ubuntu)",
		replies: []fakeCrossReply{
			// 用户自己的 Node 12 + 平台装的 24：必须报平台那套，
			// 否则一个旧 Node 会把自己装好的顶掉。
			{match: "managed_node", out: "managed_node=v24.21.0\nmanaged_npm=11.0.0\nsystem_node=v12.22.12\nsystem_npm_path=/usr/bin/npm\nsystem_npm=8.19.4\n"},
		},
	}
	server := newCrossTestServer(t)
	target, err := parseTargetProbe(ubuntuProbeOutput)
	if err != nil {
		t.Fatal(err)
	}
	status := server.probeRuntimeCross(context.Background(), env, target)
	if status.Origin != "managed" {
		t.Fatalf("origin = %q，期望 managed", status.Origin)
	}
	if status.Version != "24.21.0" {
		t.Fatalf("version = %q", status.Version)
	}
	if status.NpmVersion != "11.0.0" {
		t.Fatalf("npm version = %q", status.NpmVersion)
	}
	// MeetsMinimumFor 现在由 runtimeStatusFor 按**登记**逐工具算（判据与安装路径同源），
	// 不在这里填 —— 那部分由 TestRuntimeMeetsMinimumForFollowsInstallKind 覆盖。
}

func TestProbeRuntimeCrossReportsProbeFailureSeparately(t *testing.T) {
	env := &fakeCrossEnvironment{
		name:    "prod-server",
		replies: []fakeCrossReply{{match: "managed_node", err: fmt.Errorf("connection reset")}},
	}
	server := newCrossTestServer(t)
	target, err := parseTargetProbe(ubuntuProbeOutput)
	if err != nil {
		t.Fatal(err)
	}
	status := server.probeRuntimeCross(context.Background(), env, target)
	if status.InstallSupported {
		t.Fatal("通道坏了的时候不该给出可安装的信号")
	}
	if status.InstallBlockedReason == "" {
		t.Fatal("必须说明为什么不能装")
	}
	// 不能把"读不到"说成"没装"。
	if status.Installed {
		t.Fatal("通道失败不等于运行时已安装")
	}
}

func TestProbeRuntimeCrossRefusesPlatformWithoutDistribution(t *testing.T) {
	env := &fakeCrossEnvironment{
		name:    "小机器",
		replies: []fakeCrossReply{{match: "managed_node", out: "system_node=v20.11.1\nsystem_npm_path=/usr/bin/npm\nsystem_npm=10.2.4\n"}},
	}
	server := newCrossTestServer(t)
	target := targetEnvironment{OS: "linux", Arch: "mips", Home: "/home/dev", HomeWritable: true, HasTar: true, HasGzip: true}
	status := server.probeRuntimeCross(context.Background(), env, target)
	if status.InstallSupported {
		t.Fatal("没有官方分发包的架构不该显示可安装")
	}
	if status.InstallBlockedReason == "" {
		t.Fatal("必须说明原因")
	}
	if status.Origin != "system" {
		t.Fatalf("已装的系统运行时仍要如实报告：origin = %q", status.Origin)
	}
}

// ── 授权闸门 ─────────────────────────────────────────────────────────────────

// TestCrossInstallEndpointsRequireGrant 钉住"逐主机授权是**服务端**的闸门"。
//
// 界面在未授权时不给安装按钮，但接口不能靠界面挡人 —— 否则"逐主机授权"就只是
// 一句界面文案，任何能调到本地 API 的东西都能往别人的机器上装东西。
func TestCrossInstallEndpointsRequireGrant(t *testing.T) {
	server := newTestServer(t)
	for _, path := range []string{
		"/api/runners/ssh-prod/agents/claude-code/install",
		"/api/runners/ssh-prod/runtime/install",
	} {
		response := httptest.NewRecorder()
		server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}")))
		if response.Code != http.StatusForbidden {
			t.Fatalf("%s：未授权时应当 403，实际 %d（body=%s）", path, response.Code, response.Body.String())
		}
	}

	// 本机永远不需要授权（那是用户自己的机器，也不涉及远程执行）。
	if !server.remoteInstallAllowed(context.Background(), server.localRunnerID()) {
		t.Fatal("本机不该要求逐主机授权")
	}

	// 授权之后这一档必须消失：后续可能因别的原因失败，但"未授权"不再成立。
	if _, err := server.db.ExecContext(context.Background(),
		`insert into runner_install_grants (runner_id,granted_at) values (?,?)`, "ssh-prod", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/runners/ssh-prod/runtime/install", strings.NewReader("{}")))
	if response.Code == http.StatusForbidden {
		t.Fatalf("已授权之后不该再 403（body=%s）", response.Body.String())
	}
	// 授权是按主机的：给 ssh-prod 授权不该顺带把别的机器也放开。
	if server.remoteInstallAllowed(context.Background(), "ssh-staging") {
		t.Fatal("授权是按主机的，不该波及其他 Runner")
	}
}

// TestCrossUpdateNpmInstallIsGrantScoped 钉住升级闸门的**边界**。
//
// 跨端升级只有在"这份工具是平台装的（登记为 npm 类）"时才走 npm 重装，那与 install
// 是同一条代码路径，因此要过同一道闸门；用户自己用官方安装器装的仍走 CLI 自带的
// update，属于本次改动之前就有的行为，不该被顺带改掉。
func TestCrossUpdateNpmInstallIsGrantScoped(t *testing.T) {
	server := newInstallTestServer(t)
	ctx := context.Background()
	if server.crossUpdateUsesNpmInstall(ctx, "ssh-prod", "claude-code") {
		t.Fatal("没有登记项时不该走 npm 重装（那是 CLI 自带 update 的场景）")
	}
	if server.crossUpdateUsesNpmInstall(ctx, server.localRunnerID(), "claude-code") {
		t.Fatal("本机与跨端闸门无关")
	}

	register := func(kind string) {
		t.Helper()
		if err := server.recordAgentInstallation(ctx, agentInstallation{
			RunnerID: "ssh-prod", AgentID: "claude-code",
			BinaryPath:  "/home/dev/.local/share/milevia/toolchain/npm-global/bin/claude",
			InstallKind: kind,
			Prefix:      "/home/dev/.local/share/milevia/toolchain/npm-global",
			Version:     "2.1.217", Source: "managed-install",
		}); err != nil {
			t.Fatal(err)
		}
	}
	register(installKindNpmManaged)
	if !server.crossUpdateUsesNpmInstall(ctx, "ssh-prod", "claude-code") {
		t.Fatal("登记为平台托管的工具，升级会走 npm 重装，因此要过授权闸门")
	}
	register(installKindNpmSystem)
	if !server.crossUpdateUsesNpmInstall(ctx, "ssh-prod", "claude-code") {
		t.Fatal("系统 npm 全局装的同样是平台接管的，也要过闸门")
	}
	register(installKindNative)
	if server.crossUpdateUsesNpmInstall(ctx, "ssh-prod", "claude-code") {
		t.Fatal("官方安装器装的不归平台管，升级走 CLI 自带 update，不受这道闸门约束")
	}
}
