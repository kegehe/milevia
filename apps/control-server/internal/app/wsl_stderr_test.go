package app

import (
	"bufio"
	"io"
	"strings"
	"testing"
)

// TestWslStderrSplitDecodesUTF16Le 验证 wsl.exe 的 UTF-16LE 主机侧警告被还原为可读中文。
// 输入使用真实捕获的 wsl.exe stderr 字节：ASCII 为 "x\x00" 两字节，中文为 UTF-16 码元
// （如 检=U+68C0 → c0 68），整行以 0d 00 0a 00（\r\n）结尾。修复前按 UTF-8 解读会得到
// "wsl: �hKm0R localhost ..." 乱码。
func TestWslStderrSplitDecodesUTF16Le(t *testing.T) {
	raw := utf16LE("wsl: 检测到 localhost 代理配置，但未镜像到 WSL。NAT 模式下的 WSL 不支持 localhost 代理。\r\n")
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	scanner.Split(wslStderrSplit)

	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1 (line-split must not break the UTF-16 line): %q", len(lines), lines)
	}
	want := "wsl: 检测到 localhost 代理配置，但未镜像到 WSL。NAT 模式下的 WSL 不支持 localhost 代理。"
	if lines[0] != want {
		t.Errorf("decoded = %q, want %q", lines[0], want)
	}
}

// TestWslStderrSplitLeavesUTF8Untouched 验证非 UTF-16（UTF-8/GBK）内容按普通行切分、
// 内容原样保留——这是本地（非 WSL）CLI stderr 的既有行为。
func TestWslStderrSplitLeavesUTF8Untouched(t *testing.T) {
	input := "first line\r\nsecond line\nsrc/TaskQueue.tsx(534,10): error TS2554\n"
	scanner := bufio.NewScanner(strings.NewReader(input))
	scanner.Split(wslStderrSplit)

	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	want := []string{"first line", "second line", "src/TaskQueue.tsx(534,10): error TS2554"}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d: %q", len(lines), len(want), lines)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line[%d] = %q, want %q", i, lines[i], want[i])
		}
	}
}

// TestWslStderrSplitMixedStream 验证 UTF-16LE 警告行与随后的 UTF-8 CLI 输出混流时：
// 警告行被解码、UTF-8 行原样保留，且各自成行。
func TestWslStderrSplitMixedStream(t *testing.T) {
	warning := utf16LE("wsl: 检测到 localhost 代理配置。\r\n")
	cliLine := []byte("Claude exited: exit status 1\n")
	stream := append(append([]byte{}, warning...), cliLine...)

	scanner := bufio.NewScanner(strings.NewReader(string(stream)))
	scanner.Split(wslStderrSplit)

	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	want := []string{"wsl: 检测到 localhost 代理配置。", "Claude exited: exit status 1"}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d: %q", len(lines), len(want), lines)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line[%d] = %q, want %q", i, lines[i], want[i])
		}
	}
}

// TestWslStderrSplitChunked 验证流式写入（一行被分成多次提供）也能正确组装解码，
// 不因缓冲边界把 UTF-16 码元或换行拆散。
func TestWslStderrSplitChunked(t *testing.T) {
	raw := utf16LE("wsl: 检测到 localhost 代理配置，但未镜像到 WSL。\r\n")
	rd := &chunkReader{data: raw, chunk: 3}
	scanner := bufio.NewScanner(rd)
	scanner.Split(wslStderrSplit)

	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1: %q", len(lines), lines)
	}
	if lines[0] != "wsl: 检测到 localhost 代理配置，但未镜像到 WSL。" {
		t.Errorf("decoded = %q", lines[0])
	}
}

// TestWslStderrSplitEOFNoTrailingNewline 验证流末无换行行也按 ScanLines 语义处理：
// UTF-8 行的尾部 \r 被剥离、UTF-16LE 行的尾部孤立 \r（0d 00）同样被剥离。
func TestWslStderrSplitEOFNoTrailingNewline(t *testing.T) {
	// UTF-8：最后一行 "last\r" 无换行，ScanLines 应剥掉 \r。
	utf8Scanner := bufio.NewScanner(strings.NewReader("first\nlast\r"))
	utf8Scanner.Split(wslStderrSplit)
	var utf8Lines []string
	for utf8Scanner.Scan() {
		utf8Lines = append(utf8Lines, utf8Scanner.Text())
	}
	wantUTF8 := []string{"first", "last"}
	if len(utf8Lines) != len(wantUTF8) {
		t.Fatalf("utf8 EOF lines = %q, want %q", utf8Lines, wantUTF8)
	}
	for i := range wantUTF8 {
		if utf8Lines[i] != wantUTF8[i] {
			t.Errorf("utf8 EOF line[%d] = %q, want %q", i, utf8Lines[i], wantUTF8[i])
		}
	}

	// UTF-16LE：整行以 0d 00 结尾、无 0a 00，EOF 时应剥掉该 \r 并还原为中文。
	utf16Raw := utf16LE("wsl: 检测到 localhost 代理配置。\r")
	utf16Scanner := bufio.NewScanner(strings.NewReader(string(utf16Raw)))
	utf16Scanner.Split(wslStderrSplit)
	var utf16Lines []string
	for utf16Scanner.Scan() {
		utf16Lines = append(utf16Lines, utf16Scanner.Text())
	}
	wantUTF16 := "wsl: 检测到 localhost 代理配置。"
	if len(utf16Lines) != 1 || utf16Lines[0] != wantUTF16 {
		t.Fatalf("utf16 EOF lines = %q, want [%q]", utf16Lines, wantUTF16)
	}
}

// chunkReader 按固定小块吐出数据，模拟管道分多次写入。
type chunkReader struct {
	data  []byte
	chunk int
	off   int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, io.EOF
	}
	n := r.chunk
	if n > len(p) {
		n = len(p)
	}
	if n > len(r.data)-r.off {
		n = len(r.data) - r.off
	}
	copy(p, r.data[r.off:r.off+n])
	r.off += n
	return n, nil
}
