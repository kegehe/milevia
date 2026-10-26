package main

import (
	"bytes"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 日志必须在**写的过程中**滚动：桌面端会连续开着好几天，只在打开时看一次大小，
// 一个每秒报错的轮询端点就能把文件堆到几百 MB。
func TestRotatingLogWriterRotatesWhileWriting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "milevia-control.log")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	writer := &rotatingLogWriter{path: path, file: file, limit: 64}
	for index := 0; index < 5; index++ {
		if _, err := writer.Write([]byte(strings.Repeat("x", 32))); err != nil {
			t.Fatalf("write %d: %v", index, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	rotated, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("rotation produced no .1: %v", err)
	}
	if len(rotated) != 64 {
		t.Fatalf(".1 size = %d, want 64", len(rotated))
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read current: %v", err)
	}
	if len(current) != 32 {
		t.Fatalf("current size = %d, want 32", len(current))
	}
}

// 文件句柄没打开时写入要被静默丢弃，不能 panic，也不能让调用方看到错误。
func TestRotatingLogWriterDropsWritesWithoutFile(t *testing.T) {
	writer := &rotatingLogWriter{path: filepath.Join(t.TempDir(), "gone", "log"), limit: 1}
	payload := []byte("payload")
	if written, err := writer.Write(payload); err != nil || written != len(payload) {
		t.Fatalf("write = (%d, %v), want (%d, nil)", written, err, len(payload))
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// 控制台（stderr 管道）在桌面退出后是断的。io.MultiWriter 遇到第一个写失败就停，
// 所以文件必须排在前面 —— 否则那几行"收尾"日志会被整个吞掉，而它们正是排查现场时最需要的。
func TestSetupLogFileWritesEvenWhenConsoleFails(t *testing.T) {
	saved := log.Writer()
	defer log.SetOutput(saved)

	path := filepath.Join(t.TempDir(), "milevia-control.log")
	closeLog := setupLogFileWith(path, failingWriter{})
	defer closeLog()
	log.Printf("survives a broken console")

	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if !strings.Contains(string(payload), "survives a broken console") {
		t.Fatalf("log file missed the line: %q", payload)
	}
}

// 启动时不再"打开前滚动"（那会让第二个实例抢走正在写的日志）；已有文件超限时
// 由第一条日志触发的写入路径完成滚动。
func TestSetupLogFileRotatesOversizedExistingFileOnFirstWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "milevia-control.log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), maxLogFileBytes), 0o600); err != nil {
		t.Fatalf("seed oversized log: %v", err)
	}
	saved := log.Writer()
	defer log.SetOutput(saved)
	closeLog := setupLogFileWith(path, io.Discard)
	defer closeLog()
	log.Printf("first line")

	rotated, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("rotated file: %v", err)
	}
	if !strings.Contains(string(rotated), "first line") {
		t.Fatalf("rotated file missed the first line (size=%d)", len(rotated))
	}
	if info, err := os.Stat(path); err != nil || info.Size() != 0 {
		t.Fatalf("current log = %v (err=%v), want a fresh empty file", info, err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// io.MultiWriter 在第一个写失败时就返回；日志文件写失败（磁盘满/文件被替换）时
// 不能连带把 stderr 也吞掉，反之亦然。
func TestTeeWriterWritesBothWhenOneFails(t *testing.T) {
	var console bytes.Buffer
	writer := teeWriter{writers: []io.Writer{failingWriter{}, &console}}
	if _, err := writer.Write([]byte("line")); err == nil {
		t.Fatal("teeWriter should report the failing writer's error")
	}
	if console.String() != "line" {
		t.Fatalf("second writer missed the line: %q", console.String())
	}
}
