package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWorkspaceGuardFilesystemRejectsWSLWindowsMountAndSymlink(t *testing.T) {
	if os.Getenv("WSL_DISTRO_NAME") == "" {
		t.Skip("requires an actual WSL Windows mount")
	}
	if _, err := os.Stat("/mnt/c/Windows"); err != nil {
		t.Skip("Windows drive unavailable")
	}
	alias := filepath.Join(t.TempDir(), "windows-alias")
	if err := os.Symlink("/mnt/c/Windows", alias); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/mnt/c/Windows", alias, filepath.Join(alias, "not-created-yet")} {
		if err := verifyWorkspaceGuardFilesystem(path); err == nil {
			t.Fatal("Windows physical directory obtained an independent Linux guard", path)
		}
	}
	if err := verifyWorkspaceGuardFilesystem(filepath.Join(t.TempDir(), "not-created-yet")); err != nil {
		t.Fatal("native Linux directory rejected", err)
	}
}
