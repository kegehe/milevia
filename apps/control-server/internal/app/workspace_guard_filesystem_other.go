//go:build !windows && !linux

package app

import "errors"

func verifyWorkspaceGuardFilesystem(string) error {
	return errors.New("该平台尚未实现共享目录身份核对")
}
