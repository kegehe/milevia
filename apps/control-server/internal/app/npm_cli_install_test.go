package app

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func assertNpmCLICommandForTest(t *testing.T, prefix string, install npmCLIInstall) {
	t.Helper()
	command := install.commandPath(prefix)
	if runtime.GOOS == "windows" {
		content, err := os.ReadFile(command)
		if err != nil || !strings.Contains(string(content), install.packageName) {
			t.Fatalf("recovered command shim=%q err=%v", content, err)
		}
		if _, err := os.Stat(filepath.Join(prefix, install.commandName+".ps1")); err != nil {
			t.Fatalf("recovered PowerShell command shim: %v", err)
		}
		return
	}
	target, err := os.Readlink(command)
	want := filepath.Join("..", "lib", "node_modules", install.scope, install.packageName, filepath.FromSlash(install.binPath))
	if err != nil || target != want {
		t.Fatalf("recovered CLI link target=%q err=%v want=%q", target, err, want)
	}
}

// TestNpmCLIInstallUsesPackageRelativeBinPath 钉住"落点目录不再假设是 bin/"。
//
// qoder 的 npm bin 目标在 bundle/ 下；旧实现固定拼 <包根>/bin/<文件名>，会指到一个
// 不存在的路径 —— 而它只被回滚与跨端升级的"确认来源"用到，症状是"升级失败时回滚不了"。
func TestNpmCLIInstallUsesPackageRelativeBinPath(t *testing.T) {
	install := npmCLIInstall{scope: "@qoder-ai", packageName: "qodercli", commandName: "qoder", binPath: "bundle/qoder-npm-dispatcher.cjs"}
	want := filepath.Join(install.packageRoot("P"), "qodercli", "bundle", "qoder-npm-dispatcher.cjs")
	if got := install.binaryPath("P"); got != want {
		t.Fatalf("binaryPath = %q, want %q", got, want)
	}
}

// TestNpmCLIScriptSuffix 钉住"哪些入口要用 node 跑"。
// 只认 .js 时，.cjs 的入口会被写成 ""<target>" %*" 的 Windows shim，调用即报
// "不是有效的 Win32 应用"。
func TestNpmCLIScriptSuffix(t *testing.T) {
	cases := map[string]bool{
		"bin/claude.exe":                  false,
		"bin/codex.js":                    true,
		"bin/codebuddy":                   false,
		"bundle/qoder-npm-dispatcher.cjs": true,
		"bundle/worker.mjs":               true,
		"":                                false,
	}
	for path, want := range cases {
		if got := npmCLIScriptSuffix(path); got != want {
			t.Fatalf("npmCLIScriptSuffix(%q) = %v, want %v", path, got, want)
		}
	}
}
