package agent

import (
	"os/exec"
	"testing"
	"time"
)

// 父进程消失必须触发 shutdown —— 这条契约是"桌面端被强杀后不留孤儿 Agent"的全部依据。
// 2026-09-20 的真实故障就是它没被守住：上一个会话残留的 Agent 带着旧令牌继续跑，
// 抢到云端投来的命令后一律 401 invalid agent token。
//
// 用 cmd.exe 起一个立刻退出的进程当"父进程"，两种实现路径都应当覆盖：
//   - 进程对象还在（句柄未关闭）→ WaitForSingleObject 立刻返回；
//   - 进程对象已销毁 → OpenProcess 失败，退回轮询，第一次检查就判定已死。
func TestWatchParentProcessShutsDownWhenParentExits(t *testing.T) {
	helper := exec.Command("cmd", "/c", "exit")
	if err := helper.Start(); err != nil {
		t.Skipf("无法启动辅助进程，跳过: %v", err)
	}
	pid := helper.Process.Pid

	called := make(chan struct{}, 1)
	WatchParentProcess(pid, func() {
		select {
		case called <- struct{}{}:
		default: // 回调可能被多条路径各触发一次，只记第一次。
		}
	})
	_ = helper.Wait()

	select {
	case <-called:
	case <-time.After(10 * time.Second):
		t.Fatal("父进程已退出，但监视回调始终没有被触发（孤儿 Agent 就会这样留下来）")
	}
}

// parentPid <= 0 表示不是由桌面宿主拉起的（独立运行 / 开发调试），
// 这时必须完全不启用监视：否则开发时跑一个 Agent 会莫名其妙自杀。
func TestWatchParentProcessDisabledWithoutParentPid(t *testing.T) {
	called := make(chan struct{}, 1)
	WatchParentProcess(0, func() {
		select {
		case called <- struct{}{}:
		default:
		}
	})
	select {
	case <-called:
		t.Fatal("parentPid<=0 时不应启用监视")
	case <-time.After(500 * time.Millisecond):
	}
}
