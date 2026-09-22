package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/kegehe/milevia/apps/agent/internal/agent"
)

func main() {
	// 桌面宿主注入自己的 PID。父进程被强杀 / 崩溃 / 升级覆写时，本进程不会走正常退出路径，
	// 于是带着**上一个会话的本地令牌**继续跑：那个令牌在新 control-server 上必然无效
	// （AUTO_REMOTE_AGENT_TOKEN 每次桌面启动都重新生成），而云端仍会把手机的命令投过来
	// —— 结果是创建任务等操作一律 401 invalid agent token。
	// 独立运行 / 开发调试时没有这个参数，监视自动关闭（见 agent.WatchParentProcess）。
	parentPid := flag.Int("parent-pid", 0, "desktop host process PID; agent exits if this process dies")
	flag.Parse()
	config := agent.ConfigFromEnv()
	hasStoredCredential := false
	if config.CredentialFile != "" {
		if _, err := os.Stat(config.CredentialFile); err == nil {
			hasStoredCredential = true
		}
	}
	if config.CloudURL == "" || config.LocalURL == "" || (!hasStoredCredential && config.EnrollmentToken == "" && (config.InstanceID == "" || config.CloudToken == "")) {
		log.Fatal("MILEVIA_CLOUD_URL and MILEVIA_LOCAL_URL are required; provide an enrollment token or existing agent credentials")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	// 父进程消失 → cancel → Run 收到 ctx.Done 后优雅退出。放在 Run 之前注册，
	// 免得出现"刚要连云端、父进程已经没了"的窗口。
	agent.WatchParentProcess(*parentPid, cancel)
	if err := agent.New(config).Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}
