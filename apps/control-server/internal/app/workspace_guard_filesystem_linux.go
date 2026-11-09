package app

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// A WSL view of a Windows mount cannot own an independent Linux journal.
// Check the actual filesystem, including symlinks and nonstandard mount points.
// Network filesystems likewise cannot prove a single host's protection domain.
func verifyWorkspaceGuardFilesystem(path string) error {
	for {
		var stat unix.Statfs_t
		err := unix.Statfs(path, &stat)
		if errors.Is(err, os.ErrNotExist) && filepath.Dir(path) != path {
			path = filepath.Dir(path)
			continue
		}
		if err != nil {
			return err
		}
		switch uint64(stat.Type) {
		case 0x01021997, 0x9fa0, 0x6969, 0xff534d42, 0xfe534d42: // 9P, drvfs, NFS, CIFS, SMB2
			return errors.New("该共享挂载无法证明与实际宿主共用目录保护，请使用实际宿主路径及对应 Runner")
		}
		return nil
	}
}
