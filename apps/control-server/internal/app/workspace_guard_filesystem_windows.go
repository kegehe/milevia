package app

import (
	"errors"
	"strings"
)

func verifyWorkspaceGuardFilesystem(path string) error {
	if strings.HasPrefix(path, `\\`) {
		return errors.New("该 UNC 目录需要通过实际宿主 Runner 的共享保护准入")
	}
	return nil
}
