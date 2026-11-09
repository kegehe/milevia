//go:build !windows && !linux

package app

import "os/exec"

const supervisionJobPrefix = "Milevia.Supervision."

func supervisionLocalProcessCapability() error { return errSupervisionProcessUnsupported }

func startSupervisionContainedProcess(*exec.Cmd, string) (supervisionContainedProcess, error) {
	return nil, errSupervisionProcessUnsupported
}

func startOrdinaryContainedProcess(*exec.Cmd, string) (supervisionContainedProcess, error) {
	return nil, errSupervisionProcessUnsupported
}

func supervisionContainerQuiescent(string) (bool, error) {
	return false, errSupervisionProcessUnsupported
}

func stopSupervisionContainer(string) error { return errSupervisionProcessUnsupported }

func supervisionNativeClaudeBinary(string) (string, error) {
	return "", errSupervisionProcessUnsupported
}
