//go:build !windows

package app

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

func lockWorkspaceGuard(file *os.File) (bool, error) {
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return false, nil
	}
	return err == nil, err
}

func unlockWorkspaceGuard(file *os.File) { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN) }

func replaceWorkspaceGuard(source, destination string) error {
	if err := os.Rename(source, destination); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(destination))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
