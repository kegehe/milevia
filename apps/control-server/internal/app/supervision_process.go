package app

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

type supervisionExitError struct{ Code int }

func (e *supervisionExitError) Error() string { return fmt.Sprintf("托管进程退出码 %d", e.Code) }

type supervisionProcessIdentity struct {
	PID        int    `json:"pid"`
	Birth      string `json:"birth"`
	Container  string `json:"container"`
	Executable string `json:"executable"`
	Directory  string `json:"directory"`
}

// 回调先持久化身份，再在 launch/control gate 内调用 resume。不得在回调里等待进程。
type supervisionProcessHandshake func(supervisionProcessIdentity, func() error) error

type supervisionContainedProcess interface {
	identity() supervisionProcessIdentity
	resume() error
	terminate() error
	quiescent() (bool, error)
	wait() error
	close()
}

var errSupervisionProcessUnsupported = errors.New("当前平台尚不支持托管进程的严格容器核对")
var errSupervisionProcessIO = errors.New("托管进程输出尚未收敛")

func validSupervisionContainer(name string) bool {
	if !strings.HasPrefix(name, supervisionJobPrefix) {
		return false
	}
	suffix := strings.TrimPrefix(name, supervisionJobPrefix)
	if len(suffix) != 36 {
		return false
	}
	for i, char := range suffix {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if char != '-' {
				return false
			}
			continue
		}
		if !strings.ContainsRune("0123456789abcdef", char) {
			return false
		}
	}
	return true
}

func waitSupervisionProcessContainer(ctx context.Context, process supervisionContainedProcess) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		quiet, err := process.quiescent()
		if err != nil {
			return err
		}
		if quiet {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// 启动前容器名须已经持久化。Start 成功仍处于挂起状态，不允许 CLI 抢在登记前运行。
// 返回 containerQuiet=false 时，即使 cmd.Wait 已结束也不能释放工作项或目录。
func runSupervisionContainedCommand(ctx context.Context, cmd *exec.Cmd, container string, handshake supervisionProcessHandshake) (started, waited, containerQuiet bool, resultErr error) {
	if handshake == nil || container == "" {
		return false, false, false, errors.New("托管进程缺少持久容器及启动握手")
	}
	if err := ctx.Err(); err != nil {
		return false, false, true, err
	}
	process, err := startSupervisionContainedProcess(cmd, container)
	if err != nil {
		// 部分创建失败也可能已经产生挂起进程；平台实现只有确认无进程才返回 nil。
		if process == nil {
			return false, false, true, err
		}
		resultErr = err
	}
	defer process.close()
	started = true
	if resultErr == nil {
		resultErr = handshake(process.identity(), process.resume)
	}
	if resultErr != nil {
		_ = process.terminate()
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = process.terminate()
		case <-done:
		}
	}()
	waitErr := process.wait()
	waited = true
	// 主进程返回后，仍必须清理和核对整个作业对象；不依赖父 PID 的后裔枚举。
	terminateErr := process.terminate()
	settleCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	settleErr := waitSupervisionProcessContainer(settleCtx, process)
	containerQuiet = settleErr == nil && !errors.Is(waitErr, errSupervisionProcessIO)
	if ctx.Err() != nil {
		resultErr = errors.Join(resultErr, ctx.Err())
	}
	return started, waited, containerQuiet, errors.Join(resultErr, waitErr, terminateErr, settleErr)
}
