package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/kegehe/milevia/apps/control-server/internal/app"
)

// maxLogFileBytes 是单个日志文件的上限；超了就滚动成 <name>.1（只留一份）。
// sidecar 由桌面以 GUI 子系统进程拉起，stderr 没有控制台可去 —— 不落文件的话，
// 慢请求日志（phase=slow / db_wait_ms）这类定位卡顿的关键信息会全部丢失。
const maxLogFileBytes = 8 << 20

// rotatingLogWriter 在写到上限时自己滚动，而不是只在打开时看一次大小：
// 桌面端会连续开着好几天，一个每秒都在报错的轮询端点足以把日志堆到几百 MB。
// log 包内部持锁串行调用 Write，但 Close 是主 goroutine 在退出时调的，可能和
// 尚未收敛的后台 goroutine 的日志撞上 —— 文件句柄与计数的读写仍要自己加锁。
type rotatingLogWriter struct {
	mu    sync.Mutex
	path  string
	file  *os.File
	size  int64
	limit int64
}

func (w *rotatingLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return len(p), nil
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	if w.limit > 0 && w.size >= w.limit {
		w.rotateLocked()
	}
	return n, err
}

// rotateLocked 的调用方必须持有 w.mu。
func (w *rotatingLogWriter) rotateLocked() {
	_ = w.file.Close()
	_ = os.Remove(w.path + ".1")
	if err := os.Rename(w.path, w.path+".1"); err != nil {
		// 滚不动（文件被编辑器/杀软/另一个实例占着等）：不再写文件。若在这里重开原文件，
		// size 会仍超上限，于是**每一行日志**都重跑 close/remove/rename/open；若把 size
		// 归零继续写，文件又会无限增长。两者都会在错误风暴里把服务拖垮。
		_, _ = fmt.Fprintf(os.Stderr, "control server log rotation failed for %s: %v; file logging disabled\n", w.path, err)
		w.file = nil
		return
	}
	file, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		// 滚不动就干脆不再写文件，绝不能让日志反过来把服务拖死。
		w.file = nil
		return
	}
	w.file = file
	w.size = 0 // 滚动成功，新文件为空
}

func (w *rotatingLogWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	file := w.file
	w.file = nil
	return file.Close()
}

// teeWriter 与 io.MultiWriter 的区别：后者在第一个 writer 写失败时直接返回，不会再写
// 第二个。日志文件写失败（磁盘满、文件被替换）时不能连带把 stderr 也吞掉，反之亦然。
type teeWriter struct{ writers []io.Writer }

func (w teeWriter) Write(p []byte) (int, error) {
	var firstErr error
	for _, writer := range w.writers {
		if _, err := writer.Write(p); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return len(p), firstErr
}

// setupLogFile 让日志同时写到 stderr 与文件，返回关闭文件的函数。
func setupLogFile(path string) func() {
	return setupLogFileWith(path, os.Stderr)
}

// setupLogFileWith 与 setupLogFile 相同，只是把控制台输出换成可注入的 writer（测试用）。
func setupLogFileWith(path string, console io.Writer) func() {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o700)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		log.Printf("open log file %s: %v", path, err)
		return func() {}
	}
	size := int64(0)
	if info, err := file.Stat(); err == nil {
		size = info.Size()
	}
	writer := &rotatingLogWriter{path: path, file: file, size: size, limit: maxLogFileBytes}
	// 两侧都要尝试写：桌面退出后 stderr 管道会断（EPIPE），磁盘满时文件写也会失败。
	// 任何一侧失败都不能连带吞掉另一侧的日志。
	log.SetOutput(teeWriter{writers: []io.Writer{writer, console}})
	return func() { _ = writer.Close() }
}

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
	logFile := flag.String("log-file", "", "append server logs to this file as well (rotated at 8 MiB); desktop-api mode defaults to <data-dir>/milevia-control.log")
	flag.Parse()
	// 桌面模式默认落盘：sidecar 由桌面的 GUI 子系统进程拉起，stderr 没有控制台可去，
	// 慢请求日志（phase=slow / db_wait_ms）不落文件就等于没有。web 模式（容器）交给容器日志。
	// 由 sidecar 自己决定路径（而不是桌面传参），这样老版本桌面配新 sidecar 也能拿到日志。
	if *logFile == "" && *mode == "desktop-api" && *dataDir != "" {
		*logFile = filepath.Join(*dataDir, "milevia-control.log")
	}
	if *logFile != "" {
		closeLog := setupLogFile(*logFile)
		defer closeLog()
		log.Printf("control server log file: %s", *logFile)
	}
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
		server.StartBackgroundMaintenance()
		log.Printf("control server listening on %s", url)
		fmt.Printf("MILEVIA_READY=%s\n", url)
	}); err != nil {
		log.Printf("control server stopped with error: %v", err)
	}
}
