//go:build !windows

package targetexec

import (
	"os"
	"path/filepath"
	"syscall"
)

func containerName(id string) string { return "milevia-supervision-" + id }

func lockFile(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}
func replaceDurable(source, target string) error {
	if err := os.Rename(source, target); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(target))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
