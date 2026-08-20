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
	want := filepath.Join("..", "lib", "node_modules", install.scope, install.packageName, "bin", install.binFile)
	if err != nil || target != want {
		t.Fatalf("recovered CLI link target=%q err=%v want=%q", target, err, want)
	}
}
