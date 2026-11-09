//go:build !windows

package app

import (
	"fmt"
	"os"
	"syscall"
)

func workspaceFinalDirectoryPath(path string) (string, error) { return path, nil }

func workspaceDirectoryIdentity(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace is not a directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("workspace directory identity unavailable")
	}
	return fmt.Sprintf("unix:%x:%x", stat.Dev, stat.Ino), nil
}
