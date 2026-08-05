package app

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestCappedWriterStoresUpToLimit(t *testing.T) {
	var buf bytes.Buffer
	cw := &cappedWriter{buf: &buf, limit: 10}
	n, err := io.WriteString(cw, "hello world!")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 12 {
		t.Fatalf("expected 12 bytes consumed, got %d", n)
	}
	if buf.String() != "hello worl" {
		t.Fatalf("expected 'hello worl', got %q", buf.String())
	}
}

func TestCappedWriterDiscardsBeyondLimitWithoutBlocking(t *testing.T) {
	var buf bytes.Buffer
	cw := &cappedWriter{buf: &buf, limit: 4}
	// 写入大量数据，cappedWriter 应消费全部但只保留前 4 字节
	large := strings.Repeat("x", 100000)
	n, err := io.WriteString(cw, large)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != len(large) {
		t.Fatalf("expected %d consumed, got %d", len(large), n)
	}
	if buf.Len() != 4 {
		t.Fatalf("expected buffer len 4, got %d", buf.Len())
	}
}

func TestCappedWriterViaIoCopy(t *testing.T) {
	var buf bytes.Buffer
	cw := &cappedWriter{buf: &buf, limit: 8}
	src := strings.NewReader("this is a very long line that exceeds the limit")
	written, err := io.Copy(cw, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// io.Copy 返回 src 写出的字节数（cw.Write 返回 len(p)）
	if written != int64(len("this is a very long line that exceeds the limit")) {
		t.Fatalf("expected full consumption, got %d", written)
	}
	if buf.String() != "this is " {
		t.Fatalf("expected 'this is ', got %q", buf.String())
	}
}
