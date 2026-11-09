//go:build windows

package app

import (
	"path/filepath"

	"golang.org/x/sys/windows"
)

func supervisionDiskFree(path string) (uint64, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return 0, err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var available uint64
	err = windows.GetDiskFreeSpaceEx(name, &available, nil, nil)
	return available, err
}
