//go:build windows

package app

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestRunProbeCommandKillsGrandchild 钉住"探针超时就是超时"。
//
// Windows 上 `cmd /c` 只是包装器：`CommandContext` 到点只杀得掉 cmd.exe，它拉起的
// 子进程还握着 stdout 写端，于是 `cmd.Output()` 会一直等到子进程自己跑完 ——
// 实测 700ms 的 ctx 拖到 5.46 秒（见 runProbeCommand 的注释）。
//
// 探针自己的 5~10 秒超时因此形同虚设：一轮聚合探测可以被拖到 65 秒，
// 一次 `/api/runners` 读到 84 秒（2026-10-30 实测）。这条用例把那个形状固定下来：
// 300ms 的 ctx + 约 5 秒的 ping，必须很快返回（而不是等 ping 跑完）。
func TestRunProbeCommandKillsGrandchild(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "cmd", "/d", "/c", "ping -n 6 127.0.0.1")

	started := time.Now()
	_, err := runProbeCommand(ctx, cmd)
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("被超时掐断的探针应当返回错误")
	}
	// 只有 WaitDelay（2s）兜底时也会超过 2s，所以这个上限同时钉住"作业对象真的生效了"。
	if elapsed > 2*time.Second {
		t.Fatalf("探针在 ctx 到点后又等了 %v —— 孙进程没被杀掉，管道一直没关", elapsed)
	}
}

// TestRunProbeCommandKeepsStdoutAndExitStatus 钉住"修超时不改正常语义"：
// 成功时拿到 stdout（版本号靠它），失败时拿到 *exec.ExitError（Readiness 靠它）。
func TestRunProbeCommandKeepsStdoutAndExitStatus(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out, err := runProbeCommand(ctx, exec.Command("cmd", "/d", "/c", "echo 1.2.3"))
	if err != nil {
		t.Fatalf("正常命令不该报错：%v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "1.2.3" {
		t.Fatalf("stdout 没传回来（%q）—— 版本号会读成空，工具被判成未安装", got)
	}

	exitCtx, exitCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer exitCancel()
	if _, err := runProbeCommand(exitCtx, exec.Command("cmd", "/d", "/c", "exit 3")); err == nil {
		t.Fatal("非零退出码应当作为错误返回（Ready 判据依赖它）")
	}
}
