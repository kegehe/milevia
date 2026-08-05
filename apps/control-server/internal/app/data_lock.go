package app

import (
	"fmt"
	"os"
	"path/filepath"
)

// dataDirLock prevents two control-server processes from opening the same
// SQLite data directory. The platform implementation uses an advisory OS lock
// instead of the existence of the lock file, so a crashed process is recoverable.
type dataDirLock struct {
	file *os.File
	path string
}

func acquireDataDirLock(dataDir string) (*dataDirLock, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	path := filepath.Join(dataDir, "milevia.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open data directory lock: %w", err)
	}
	if err := lockFile(file); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("data directory is already in use: %w", err)
	}
	return &dataDirLock{file: file, path: path}, nil
}

func (lock *dataDirLock) writeState(value string) error {
	if lock == nil {
		return nil
	}
	if err := lock.file.Truncate(0); err != nil {
		return err
	}
	if _, err := lock.file.Seek(0, 0); err != nil {
		return err
	}
	if _, err := lock.file.WriteString(value); err != nil {
		return err
	}
	return lock.file.Sync()
}

func (lock *dataDirLock) close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	_ = unlockFile(lock.file)
	return lock.file.Close()
}
