//go:build windows

package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 上限到点时**必须把 npm 的整棵进程树杀掉**，否则这次调用根本没被上限框住。
//
// 形状是这样的：Windows 上 `npm` 是 npm.cmd，Go 手里那个进程是 cmd.exe，真正干活的
// node.exe 是它的子进程，而且**握着 stdout/stderr 管道**。默认的取消只 TerminateProcess
// 直接子进程，于是 cmd.Output() 要等孙进程自己退出才返回 —— 上限到点了、调用还挂着。
// docs/42 §25 那一笔"上限 10 秒、这次花了 14.6s 才失败"就是这个形状。
//
// 真 npm 在超时那一路跑得太快（node 冷启动 0.4s、网络 1~3s），复现不了"挂住"，
// 所以这里用一个假 npm.cmd 把孙进程造成长睡 30 秒。
func TestNpmVersionQueryTimeoutKillsTheWholeTree(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("这台机器上没有 node，造不出孙进程，跳过")
	}
	dir := t.TempDir()
	// 假 npm：起一个长睡的后代（占据 node.exe 的位置），然后等它。
	script := "@echo off\r\nnode -e \"setTimeout(function(){},30000)\"\r\n"
	if err := os.WriteFile(filepath.Join(dir, "npm.cmd"), []byte(script), 0o644); err != nil {
		t.Fatalf("写假 npm.cmd：%v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := latestNpmPackageVersion(ctx, "@anthropic-ai/claude-code")
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("假 npm 什么都没输出，不该被判成成功")
	}
	if !strings.Contains(err.Error(), "超时") {
		t.Fatalf("这次没被说成超时：%v", err)
	}
	// 上限 300 毫秒、孙进程要睡 30 秒：只有整棵树被杀掉，这次调用才可能这么快回来。
	// 留 5 秒的余量是给"杀进程 + 收管道"用的，30 秒那一边差着一个数量级。
	if elapsed > 5*time.Second {
		t.Fatalf("上限到点后调用还挂了 %v —— 孙进程没被杀掉（它握着 stdout 管道，Output() 得等它退出）", elapsed)
	}
}
