//go:build !windows

package app

import "os"

// probeProcessJob 在非 Windows 平台是空实现：POSIX 上探针进程不会经过 .cmd 包装器，
// 直接子进程就是 CLI 本身，关 stdin + 杀进程组（见 terminateProcessGroup）已经足够。
type probeProcessJob struct{}

func newProbeProcessJob() (*probeProcessJob, error) { return nil, nil }

func (j *probeProcessJob) assign(*os.Process) error { return nil }

func (j *probeProcessJob) close() {}
