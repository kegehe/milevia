package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/kegehe/milevia/apps/control-server/internal/app"
)

func main() {
	config := app.ConfigFromEnv()
	addr := os.Getenv("AUTO_HTTP_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	mode := flag.String("mode", config.Mode, "server mode: web or desktop-api")
	listenAddr := flag.String("addr", addr, "TCP listen address")
	dataDir := flag.String("data-dir", config.DataDir, "user data directory")
	webRoot := flag.String("web-root", "", "built web application directory for web mode")
	sessionToken := flag.String("session-token", "", "one-start desktop session token")
	allowedOrigin := flag.String("allowed-origin", "", "exact allowed browser origin for desktop mode")
	approvalHook := flag.String("approval-hook", config.ApprovalHook, "approval hook executable or script")
	nativeApprovalHook := flag.Bool("native-approval-hook", false, "run approval hook as a native executable")
	parentPID := flag.Int("parent-pid", 0, "desktop host process PID; sidecar exits if this process dies")
	flag.Parse()
	if *mode != "web" && *mode != "desktop-api" {
		log.Fatalf("invalid mode %q", *mode)
	}
	if *mode == "desktop-api" && (*sessionToken == "" || *allowedOrigin == "") {
		log.Fatal("desktop-api mode requires --session-token and --allowed-origin")
	}
	config.Mode = *mode
	config.DataDir = *dataDir
	// A supplied data directory owns all mutable desktop state. This prevents
	// ConfigFromEnv's legacy relative database default from escaping it.
	if *dataDir != "" {
		config.DatabasePath = ""
	}
	config.WebRoot = *webRoot
	config.SessionToken = *sessionToken
	config.ApprovalHook = *approvalHook
	config.NativeApprovalHook = *nativeApprovalHook
	if *allowedOrigin != "" {
		config.AllowedOrigins = []string{*allowedOrigin}
	}

	listener, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		log.Fatal(err)
	}
	// Bind before constructing runners: approval hooks and SSH tunnels must use
	// the actual random loopback port, never the legacy 8080 fallback.
	config.ControlURL = "http://" + listener.Addr().String()
	server, err := app.New(context.Background(), config)
	if err != nil {
		_ = listener.Close()
		log.Fatal(err)
	}
	defer server.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		log.Printf("control server shutting down: %v", ctx.Err())
		server.Close()
	}()
	// 父进程（桌面主程序）监视：仅当本进程确由桌面拉起（parent-pid>0）才启用。
	// sidecar 会因桌面主进程被强杀而变孤儿、长期占用 SQLite 库锁（导致下次启动卡在
	// "库被锁定"）。检测到父进程死了就自行关闭，从源头避免残留。
	if *parentPID > 0 {
		app.WatchParentProcess(*parentPID, func() {
			stop()
			_ = listener.Close()
			server.Close()
		})
	}
	if err := server.ServeListener(listener, func(url string) {
		log.Printf("control server listening on %s", url)
		fmt.Printf("MILEVIA_READY=%s\n", url)
	}); err != nil {
		log.Printf("control server stopped with error: %v", err)
	}
}
