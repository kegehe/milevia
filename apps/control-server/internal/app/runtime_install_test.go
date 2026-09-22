package app

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// ── 解压安全性 ──────────────────────────────────────────────────────────────
//
// 解压是"把来自网络的字节写到本地磁盘"，所以这一组测的不是功能而是**边界**：
// 一个被投毒的包能不能写到托管目录之外。官方包不会有这些条目，但我们不依赖
// "官方不会这么做"。

func writeTarGz(t *testing.T, path string, entries []tarFixture) {
	t.Helper()
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, entry := range entries {
		header := &tar.Header{Name: entry.name, Mode: entry.mode, Typeflag: entry.kind, Linkname: entry.link}
		if entry.kind == tar.TypeReg {
			header.Size = int64(len(entry.body))
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if entry.kind == tar.TypeReg {
			if _, err := tarWriter.Write([]byte(entry.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buffer.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

type tarFixture struct {
	name string
	kind byte
	mode int64
	body string
	link string
}

func TestExtractTarGzStripsRootAndKeepsExecutableNpmLink(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "node.tar.gz")
	// 形状照抄官方包：顶层目录 + bin/ 下的可执行文件 + 指向包内脚本的符号链接。
	writeTarGz(t, archive, []tarFixture{
		{name: "node-v1.2.3-linux-x64/", kind: tar.TypeDir, mode: 0o755},
		{name: "node-v1.2.3-linux-x64/bin/", kind: tar.TypeDir, mode: 0o755},
		{name: "node-v1.2.3-linux-x64/bin/node", kind: tar.TypeReg, mode: 0o755, body: "#!/bin/sh\necho v1.2.3\n"},
		{name: "node-v1.2.3-linux-x64/lib/node_modules/npm/bin/npm-cli.js", kind: tar.TypeReg, mode: 0o644, body: "// npm\n"},
		// Node 的包里 bin/npm 是**符号链接**。跳过它就会得到一个"没有 npm 的 Node"。
		{name: "node-v1.2.3-linux-x64/bin/npm", kind: tar.TypeSymlink, mode: 0o777, link: "../lib/node_modules/npm/bin/npm-cli.js"},
	})

	dest := filepath.Join(dir, "payload")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := extractArchive(archive, nodeArchiveTarGz, dest); err != nil {
		t.Fatalf("解压失败：%v", err)
	}

	if !fileExists(filepath.Join(dest, "bin", "node")) {
		t.Fatal("bin/node 没有被解出来（顶层目录没剥掉？）")
	}
	if !fileExists(filepath.Join(dest, "lib", "node_modules", "npm", "bin", "npm-cli.js")) {
		t.Fatal("包内脚本没有被解出来")
	}
	// 顶层目录本身不该被建出来（dest 已经是"这个版本的家"）。
	if _, err := os.Stat(filepath.Join(dest, "node-v1.2.3-linux-x64")); err == nil {
		t.Fatal("顶层目录没有被剥掉")
	}

	// 符号链接只在 POSIX 上能建（Windows 创建符号链接需要特权，而 Windows 用的
	// 是 zip 包、里面根本没有符号链接）。所以这条断言按平台分流 —— 但**判据本身**
	// 由下面那组纯函数用例在所有平台上守着。
	if runtime.GOOS == "windows" {
		t.Log("Windows 上不建符号链接（生产上 Windows 走 zip 包，其中没有符号链接）")
		return
	}
	npmLink := filepath.Join(dest, "bin", "npm")
	if _, err := os.Lstat(npmLink); err != nil {
		t.Fatalf("bin/npm 不存在：%v（符号链接被跳过了？那会得到一个没有 npm 的 Node）", err)
	}
	target, err := os.Readlink(npmLink)
	if err != nil {
		t.Fatalf("bin/npm 不是符号链接：%v", err)
	}
	resolved := filepath.Clean(filepath.Join(filepath.Dir(npmLink), target))
	if !strings.HasPrefix(resolved, filepath.Clean(dest)) {
		t.Fatalf("符号链接指向了托管目录之外：%s", resolved)
	}
}

// TestArchiveEntryPathsAreDecidedByPureFunctions 把"哪些条目能写"这件事挪到纯函数上测。
//
// 理由：符号链接的**创建**在 Windows 上做不到（需要特权），但"该不该创建"这个判断
// 与平台无关，也正是安全边界所在。放在纯函数里，它在每个平台上都被验到。
func TestArchiveEntryPathsAreDecidedByPureFunctions(t *testing.T) {
	tests := []struct {
		name     string
		entry    string
		isDir    bool
		wantRel  string
		wantOkay bool
	}{
		{name: "normal file", entry: "node-v1.2.3-linux-x64/bin/node", wantRel: "bin/node", wantOkay: true},
		{name: "normal dir", entry: "node-v1.2.3-linux-x64/bin/", isDir: true, wantRel: "bin", wantOkay: true},
		{name: "top dir itself", entry: "node-v1.2.3-linux-x64/", isDir: true, wantOkay: false},
		{name: "parent escape", entry: "../../evil", wantOkay: false},
		{name: "escape under top dir", entry: "node-v1.2.3-linux-x64/../../evil", wantOkay: false},
		{name: "absolute", entry: "/etc/passwd", wantOkay: false},
		{name: "windows absolute", entry: `C:\Windows\system32\evil`, wantOkay: false},
		{name: "backslash escape", entry: `..\..\evil`, wantOkay: false},
		{name: "bare file at top", entry: "node.exe", wantOkay: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			relative, ok := stripArchiveRoot(test.entry, test.isDir)
			if ok != test.wantOkay {
				t.Fatalf("stripArchiveRoot(%q) 可用性 = %t，期望 %t", test.entry, ok, test.wantOkay)
			}
			if ok && relative != test.wantRel {
				t.Fatalf("stripArchiveRoot(%q) = %q，期望 %q", test.entry, relative, test.wantRel)
			}
		})
	}

	root := filepath.Join(t.TempDir(), "payload")
	link := filepath.Join(root, "bin", "npm")
	cases := []struct {
		target string
		want   bool
	}{
		{"../lib/node_modules/npm/bin/npm-cli.js", true},
		{"node", true},
		{"../../../../etc/passwd", false},
		{"/etc/passwd", false},
		{"", false},
	}
	for _, item := range cases {
		if got := linkStaysInside(root, link, item.target); got != item.want {
			t.Fatalf("linkStaysInside(%q) = %t，期望 %t", item.target, got, item.want)
		}
	}
}

func TestExtractArchiveRejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "payload")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(dir, "escaped.txt")

	// 每个逃逸手法配一个条目：`..` 段、绝对路径、以及藏在正常顶层目录里的 `..`。
	archive := filepath.Join(dir, "evil.tar.gz")
	writeTarGz(t, archive, []tarFixture{
		{name: "node-v1.2.3-linux-x64/bin/node", kind: tar.TypeReg, mode: 0o755, body: "ok\n"},
		{name: "../../escaped.txt", kind: tar.TypeReg, mode: 0o644, body: "evil\n"},
		{name: "node-v1.2.3-linux-x64/../../escaped2.txt", kind: tar.TypeReg, mode: 0o644, body: "evil\n"},
		{name: "/escaped3.txt", kind: tar.TypeReg, mode: 0o644, body: "evil\n"},
	})
	if err := extractArchive(archive, nodeArchiveTarGz, dest); err != nil {
		t.Fatalf("解压应当跳过危险条目而不是失败：%v", err)
	}
	for _, name := range []string{"escaped.txt", "escaped2.txt", "escaped3.txt"} {
		if fileExists(filepath.Join(dir, name)) {
			t.Fatalf("%s 被写到了托管目录之外", name)
		}
	}
	if !fileExists(filepath.Join(dest, "bin", "node")) {
		t.Fatal("正常条目没有被解出来")
	}
	if fileExists(outside) {
		t.Fatal("逃逸文件被写出了")
	}

	// zip 走的是另一条代码路径，同样要挡住（Windows 上用的是 zip）。
	zipPath := filepath.Join(dir, "evil.zip")
	writeZipFixture(t, zipPath, map[string]string{
		"node-v1.2.3-win-x64/node.exe":           "ok",
		"../../../escaped-from-zip.txt":          "evil",
		"node-v1.2.3-win-x64/../../escaped2.txt": "evil",
	})
	if err := extractArchive(zipPath, nodeArchiveZip, dest); err != nil {
		t.Fatalf("zip 解压应当跳过危险条目：%v", err)
	}
	for _, name := range []string{"escaped-from-zip.txt", "escaped2.txt"} {
		if fileExists(filepath.Join(dir, name)) {
			t.Fatalf("zip 里的 %s 被写到了托管目录之外", name)
		}
	}
}

func TestExtractTarGzSkipsEscapingSymlink(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "payload")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(dir, "link.tar.gz")
	writeTarGz(t, archive, []tarFixture{
		{name: "node-v1.2.3-linux-x64/bin/node", kind: tar.TypeReg, mode: 0o755, body: "ok\n"},
		// 一条能把我们指到系统目录里的链接：必须跳过。
		{name: "node-v1.2.3-linux-x64/bin/hijack", kind: tar.TypeSymlink, mode: 0o777, link: "../../../../etc/passwd"},
		{name: "node-v1.2.3-linux-x64/bin/absolute", kind: tar.TypeSymlink, mode: 0o777, link: "/etc/passwd"},
	})
	if err := extractArchive(archive, nodeArchiveTarGz, dest); err != nil {
		t.Fatalf("解压失败：%v", err)
	}
	for _, name := range []string{"hijack", "absolute"} {
		if _, err := os.Lstat(filepath.Join(dest, "bin", name)); err == nil {
			t.Fatalf("逃逸符号链接 %s 被创建了", name)
		}
	}
}

func writeZipFixture(t *testing.T, path string, files map[string]string) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	writer := zip.NewWriter(file)
	for name, body := range files {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
}

// ── 校验和 ──────────────────────────────────────────────────────────────────

// TestDownloadVerifiedRejectsChecksumMismatch 是"校验和不是摆设"的证明。
//
// 不匹配时必须：报错 + 删掉已下载的文件。留一个不完整的包在盘上，下次可能被误用。
func TestDownloadVerifiedRejectsChecksumMismatch(t *testing.T) {
	body := []byte("this is not the real archive")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer server.Close()

	manager := newNodeRuntimeManager()
	dest := filepath.Join(t.TempDir(), "node.tar.gz")
	dist := runtimeDistribution{
		Version: "1.2.3", Platform: "linux-x64", FileName: "node-v1.2.3-linux-x64.tar.gz",
		Format: nodeArchiveTarGz, SHA256: strings.Repeat("a", 64), URL: server.URL + "/node.tar.gz",
	}
	err := manager.downloadVerified(context.Background(), dist, dest, nil)
	if err == nil {
		t.Fatal("校验和不匹配却没有报错")
	}
	if !strings.Contains(err.Error(), "校验和不匹配") {
		t.Fatalf("报错没有说明是校验和问题：%v", err)
	}
	if fileExists(dest) {
		t.Fatal("校验失败后文件仍留在盘上")
	}

	// 正向：给对校验和时必须通过（否则上一条就只是"永远失败"）。
	dist.SHA256 = hex.EncodeToString(sha256Sum(body))
	if err := manager.downloadVerified(context.Background(), dist, dest, nil); err != nil {
		t.Fatalf("校验和正确时仍失败：%v", err)
	}
	if !fileExists(dest) {
		t.Fatal("校验通过后文件没有落盘")
	}
}

func sha256Sum(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

// ── 端到端 ──────────────────────────────────────────────────────────────────

// TestInstallManagedRuntimeEndToEnd 把整条链路跑一遍：取索引 → 取校验和 → 下载 →
// 校验 → 解压 → 自检 → 登记 → 解析器可用。
//
// 用 httptest 当"官方源"，托管目录用临时目录 —— 不去碰用户的真实目录。
func TestInstallManagedRuntimeEndToEnd(t *testing.T) {
	platform, err := nodePlatformKey(runtime.GOOS, runtime.GOARCH, localNodeLibc())
	if err != nil {
		t.Skipf("当前平台没有官方分发包：%v", err)
	}
	version := "24.21.0"
	topDir := "node-v" + version + "-" + string(platform)
	nodeName := "node"
	npmName := "npm"
	if runtime.GOOS == "windows" {
		nodeName = "node.exe"
		npmName = "npm.cmd"
	}
	archiveName := topDir + ".tar.gz"
	archiveFormat := nodeArchiveTarGz
	if strings.HasPrefix(string(platform), "win-") {
		archiveName = topDir + ".zip"
		archiveFormat = nodeArchiveZip
	}

	// 造一个"看起来像官方包"的归档：bin/node 报版本，bin/npm 报 npm 版本。
	workDir := t.TempDir()
	archivePath := filepath.Join(workDir, archiveName)
	if archiveFormat == nodeArchiveTarGz {
		writeTarGz(t, archivePath, []tarFixture{
			{name: topDir + "/", kind: tar.TypeDir, mode: 0o755},
			{name: topDir + "/bin/", kind: tar.TypeDir, mode: 0o755},
			{name: topDir + "/bin/" + nodeName, kind: tar.TypeReg, mode: 0o755, body: "#!/bin/sh\necho v24.21.0\n"},
			{name: topDir + "/bin/" + npmName, kind: tar.TypeReg, mode: 0o755, body: "#!/bin/sh\necho 11.0.0\n"},
		})
	} else {
		// Windows 的 zip 把 node.exe / npm.cmd 放在**顶层**（不是 bin/ 下）——
		// 这与 Linux 的 tar.gz 不同，夹具必须照着各自的真实布局造。
		writeZipFixture(t, archivePath, map[string]string{
			topDir + "/" + nodeName: "@echo off\r\necho v24.21.0\r\n",
			topDir + "/" + npmName:  "@echo off\r\necho 11.0.0\r\n",
		})
	}
	raw, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	sum := hex.EncodeToString(sha256Sum(raw))

	// 假官方源：index.json 与 SHASUMS256.txt 都是真实形状。
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/index.json"):
			fmt.Fprintf(w, `[{"version":"v%s","date":"2026-08-20","lts":"Krypton","files":[%q]}]`, version, string(platform))
		case strings.HasSuffix(r.URL.Path, "/SHASUMS256.txt"):
			fmt.Fprintf(w, "%s  %s\n", sum, archiveName)
		case strings.Contains(r.URL.Path, "/v"+version+"/"):
			w.Write(raw)
		default:
			http.NotFound(w, r)
		}
	}))
	defer source.Close()
	t.Setenv(nodeRuntimeMirrorEnv, source.URL)

	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "e2e.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server := &Server{
		db: db, paths: newAgentPathResolver(Config{}), runtimes: newNodeRuntimeManager(),
		runnerUpdating: map[runnerAgentKey]bool{},
	}
	if err := server.migrateAgentInstallations(context.Background()); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	installation, err := server.installManagedRuntimeAt(context.Background(), installRuntimeRequest{}, root)
	if runtime.GOOS == "windows" {
		// Windows 上夹具造不出一个**真的** PE 可执行文件（写出来的 node.exe 内容
		// 其实是批处理）。所以这一步必须失败 —— 而这恰好证明"自检真的去执行了那个
		// 文件"，而不是只看它存在就把安装登记成成功。
		if err == nil {
			t.Fatal("夹具里的 node.exe 不是可执行文件，安装却报成功 —— 说明自检没有真的执行它")
		}
		if !strings.Contains(err.Error(), "无法执行") {
			t.Fatalf("失败原因不像自检失败：%v", err)
		}
		recorded, loadErr := server.loadAgentInstallations(context.Background(), server.localRunnerID())
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		if len(recorded) != 0 {
			t.Fatalf("自检失败却登记了安装：%#v", recorded)
		}
		t.Log("Windows 无法用夹具走完整条成功路径（需要一个真实的 node.exe）；成功路径由 POSIX 侧覆盖，" +
			"本条已证明自检会拦住不可执行的产物")
		return
	}
	if err != nil {
		t.Fatalf("安装失败：%v", err)
	}
	if installation.Version != "24.21.0" {
		t.Fatalf("登记版本 = %q", installation.Version)
	}
	if installation.InstallKind != "managed-toolchain" {
		t.Fatalf("install_kind = %q", installation.InstallKind)
	}
	// prefix 必须与路径一起记：只有路径时"升级"会去找系统 npm 的全局位置。
	if installation.Prefix != managedNpmGlobalPrefix(root) {
		t.Fatalf("prefix = %q，期望 %q", installation.Prefix, managedNpmGlobalPrefix(root))
	}
	if !fileExists(managedNodeBinary(root)) {
		t.Fatalf("托管 node 没有落到 %s", managedNodeBinary(root))
	}
	// 暂存目录必须被清掉，不能留一堆 node.staging-*。
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), "staging") {
			t.Fatalf("暂存目录没被清理：%s", entry.Name())
		}
	}
	// 登记表里查得到，并且已经同步进了解析器。
	recorded, err := server.loadAgentInstallations(context.Background(), server.localRunnerID())
	if err != nil {
		t.Fatal(err)
	}
	if recorded[runtimeAgentID] != managedNodeBinary(root) {
		t.Fatalf("登记表里没有 node：%#v", recorded)
	}
}

// TestInstallManagedRuntimeKeepsPreviousOnFailure 保证"装失败不会毁掉现有运行时"。
//
// 判据：镜像源返回损坏的归档时，原有 node 目录必须原样还在。
func TestInstallManagedRuntimeKeepsPreviousOnFailure(t *testing.T) {
	platform, err := nodePlatformKey(runtime.GOOS, runtime.GOARCH, localNodeLibc())
	if err != nil {
		t.Skipf("当前平台没有官方分发包：%v", err)
	}
	version := "24.21.0"
	// 归档内容损坏（不是合法 gzip/zip），但校验和是"对的"—— 也就是说它能通过
	// 校验、走到解压那一步才失败。这正是"校验和挡不住的那类坏包"。
	broken := []byte("not really an archive")
	sum := hex.EncodeToString(sha256Sum(broken))
	archiveName := fmt.Sprintf("node-v%s-%s.tar.gz", version, platform)

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/index.json"):
			fmt.Fprintf(w, `[{"version":"v%s","lts":"Krypton","files":[%q]}]`, version, string(platform))
		case strings.HasSuffix(r.URL.Path, "/SHASUMS256.txt"):
			fmt.Fprintf(w, "%s  %s\n", sum, archiveName)
		default:
			w.Write(broken)
		}
	}))
	defer source.Close()
	t.Setenv(nodeRuntimeMirrorEnv, source.URL)

	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "fail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server := &Server{db: db, paths: newAgentPathResolver(Config{}), runtimes: newNodeRuntimeManager()}
	if err := server.migrateAgentInstallations(context.Background()); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	// 先放一个"现有可用运行时"。
	existing := managedNodeBinary(root)
	if err := os.MkdirAll(filepath.Dir(existing), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(existing, []byte("existing"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := server.installManagedRuntimeAt(context.Background(), installRuntimeRequest{}, root); err == nil {
		t.Fatal("归档损坏却没有报错")
	}
	if !fileExists(existing) {
		t.Fatal("安装失败把现有运行时毁掉了")
	}
	if data, err := os.ReadFile(existing); err != nil || string(data) != "existing" {
		t.Fatalf("现有运行时被改动了：%q err=%v", string(data), err)
	}
}
