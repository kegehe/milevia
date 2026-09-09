//go:build windows

package app

import (
	"context"
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unsafe"

	"github.com/google/uuid"
	"golang.org/x/sys/windows"
)

// 提权终端实现：控制服务未提权时，CreateProcess 无法直接生成管理员子进程。
// 这里把一个 milevia-terminal-bridge.exe 经 ShellExecuteExW(runas) 提权拉起，
// bridge 在提权上下文中持有 ConPTY + Shell，并通过回环 TCP + 一次性令牌与本进程
// 交换二进制帧。会话侧只看到与普通 bridge/PTY 一致的 TerminalSession。
//
// 帧协议与 terminal-bridge/cmd 及 terminal_bridge_linux.go 使用的协议一致，这里
// 保留同名的私有常量和读写函数（仅 windows 构建编译，不会与 linux 文件冲突）。

const (
	elevatedBridgeProtocolVersion = 1
	elevatedBridgeMaxFrame        = 1 << 20
	elevatedBridgeOpenFrame       = 1
	elevatedBridgeInputFrame      = 2
	elevatedBridgeResizeFrame     = 3
	elevatedBridgeCloseFrame      = 4
	elevatedBridgeAuthFrame       = 10
	elevatedBridgeOutputFrame     = 16
	elevatedBridgeReadyFrame      = 17
	elevatedBridgeExitFrame       = 18
	elevatedBridgeErrorFrame      = 19
	// elevatedBridgeConnectTimeout 是 UAC 批准后等待 helper 拨号的时间上限。
	// ShellExecuteExW 会阻塞到用户点选，因此该等待只在批准后开始计时。
	elevatedBridgeConnectTimeout = 15 * time.Second
)

var (
	elevationShell32            = windows.NewLazySystemDLL("shell32.dll")
	elevationShellExecuteExProc = elevationShell32.NewProc("ShellExecuteExW")
)

const (
	elevationSeMaskNoCloseProcess = 0x00000040
	elevationSWHide               = 0
)

// shellExecuteInfoW 与 shell32!ShellExecuteExW 的 SHELLEXECUTEINFOW 布局一致。
type shellExecuteInfoW struct {
	cbSize       uint32
	fMask        uint32
	hwnd         uintptr
	lpVerb       *uint16
	lpFile       *uint16
	lpParameters *uint16
	lpDirectory  *uint16
	nShow        int32
	hInstApp     uintptr
	lpIDList     uintptr
	lpClass      *uint16
	hkeyClass    uintptr
	dwHotKey     uint32
	hIcon        uintptr
	hProcess     uintptr
}

// processIsElevated 报告当前控制服务是否以提权令牌运行。
func processIsElevated() bool {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return false
	}
	defer token.Close()
	return token.IsElevated()
}

// findWindowsTerminalBridge 定位提权 bridge 可执行文件：优先 MILEVIA_TERMINAL_BRIDGE
// （开发期 / WSL 部署），否则取 control-server 同目录（桌面打包两者相邻）。
func findWindowsTerminalBridge() (string, error) {
	candidates := []string{}
	if value := os.Getenv("MILEVIA_TERMINAL_BRIDGE"); value != "" {
		candidates = append(candidates, value)
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "milevia-terminal-bridge.exe"))
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", errors.New("找不到提权终端桥接程序 milevia-terminal-bridge.exe（可设置 MILEVIA_TERMINAL_BRIDGE 指向它）")
}

// launchElevatedBridge 经 UAC 以管理员身份启动 bridge。ShellExecuteExW 会阻塞直到
// 用户点选；用户取消时返回明确错误。SEE_MASK_NOCLOSEPROCESS 让我们持有 hProcess，
// 供会话关闭时强制结束 bridge（bridge 的 job 对象会连带回收 Shell 进程树）。
func launchElevatedBridge(bridgePath, args string) (windows.Handle, error) {
	verb, _ := windows.UTF16PtrFromString("runas")
	file, _ := windows.UTF16PtrFromString(bridgePath)
	params, _ := windows.UTF16PtrFromString(args)
	info := &shellExecuteInfoW{
		cbSize:       uint32(unsafe.Sizeof(shellExecuteInfoW{})),
		fMask:        elevationSeMaskNoCloseProcess,
		nShow:        elevationSWHide,
		lpVerb:       verb,
		lpFile:       file,
		lpParameters: params,
	}
	result, _, errno := elevationShellExecuteExProc.Call(uintptr(unsafe.Pointer(info)))
	if result == 0 {
		if errno == windows.ERROR_CANCELLED {
			return 0, errors.New("管理员授权已取消")
		}
		return 0, fmt.Errorf("以管理员身份启动终端失败：%v", errno)
	}
	return windows.Handle(info.hProcess), nil
}

type elevatedBridgeOpen struct {
	ProtocolVersion int    `json:"protocolVersion"`
	WorkDir         string `json:"workDir"`
	Shell           string `json:"shell"`
	Cols            uint16 `json:"cols"`
	Rows            uint16 `json:"rows"`
}

type elevatedWindowsTerminal struct {
	id, projectID string
	conn          net.Conn
	process       windows.Handle
	reader        *io.PipeReader
	writer        *io.PipeWriter
	ready         chan error
	consumeDone   chan struct{}
	exitCode      *int
	exitMu        sync.Mutex
	closeOnce     sync.Once
	writeMu       sync.Mutex
}

// openElevatedWindowsTerminal 启动一个提权 Windows 终端会话：
//  1. 本进程监听回环 TCP 并生成一次性令牌；
//  2. 以 runas 拉起 bridge，把监听地址与令牌放命令行；
//  3. bridge 拨号后先回传 auth 帧，本进程校验后再发送 open 帧；
//  4. 会话进入帧转发循环，Ready 由 bridge 输出 ready 标记后回传。
func openElevatedWindowsTerminal(ctx context.Context, spec TerminalSpec) (TerminalSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if spec.RunnerID == "wsl-local" {
		return nil, errors.New("WSL 终端不支持以管理员身份运行")
	}
	bridgePath, err := findWindowsTerminalBridge()
	if err != nil {
		return nil, err
	}
	token := uuid.NewString()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("监听提权终端回环端口失败：%w", err)
	}
	defer listener.Close()
	process, err := launchElevatedBridge(bridgePath, "-tcp "+listener.Addr().String()+" -token "+token)
	if err != nil {
		return nil, err
	}
	procOwned := true
	defer func() {
		if procOwned {
			_ = windows.TerminateProcess(process, 1)
			_ = windows.CloseHandle(process)
		}
	}()
	waitCtx, cancel := context.WithTimeout(ctx, elevatedBridgeConnectTimeout)
	defer cancel()
	conn, err := acceptElevatedBridge(waitCtx, listener, token)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, errors.New("管理员授权或提权终端启动超时")
		}
		if waitCtx.Err() == context.DeadlineExceeded {
			return nil, errors.New("提权终端桥接未在预期时间内连接")
		}
		return nil, err
	}
	// cmd 用旧令牌 "cmd.exe" 发送：新版 bridge 两种都收，旧版 bridge 只认 "cmd.exe"。
	shell := spec.Shell
	if shell == "cmd" {
		shell = "cmd.exe"
	}
	payload, err := json.Marshal(elevatedBridgeOpen{ProtocolVersion: elevatedBridgeProtocolVersion, WorkDir: spec.WorkDir, Shell: shell, Cols: spec.Cols, Rows: spec.Rows})
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := writeElevatedBridgeFrame(conn, elevatedBridgeOpenFrame, payload); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("初始化提权终端桥接失败：%w", err)
	}
	reader, writer := io.Pipe()
	t := &elevatedWindowsTerminal{id: uuid.NewString(), projectID: spec.ProjectID, conn: conn, process: process, reader: reader, writer: writer, ready: make(chan error, 1), consumeDone: make(chan struct{})}
	procOwned = false
	go t.consume()
	return t, nil
}

// acceptElevatedBridge 接受 bridge 拨号并校验一次性令牌；错误连接被关闭后继续等待，
// 直到合法 bridge 到达或 ctx 结束。
func acceptElevatedBridge(ctx context.Context, listener net.Listener, token string) (net.Conn, error) {
	type acceptResult struct {
		conn net.Conn
		err  error
	}
	resultCh := make(chan acceptResult, 1)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				select {
				case resultCh <- acceptResult{err: err}:
				default:
				}
				return
			}
			go func() {
				_ = conn.SetReadDeadline(time.Now().Add(elevatedBridgeConnectTimeout))
				typ, data, readErr := readElevatedBridgeFrame(conn)
				_ = conn.SetReadDeadline(time.Time{})
				if readErr == nil && typ == elevatedBridgeAuthFrame && len(data) == len(token) && subtle.ConstantTimeCompare(data, []byte(token)) == 1 {
					select {
					case resultCh <- acceptResult{conn: conn}:
						return
					default:
					}
				}
				_ = conn.Close()
			}()
		}
	}()
	select {
	case result := <-resultCh:
		if result.err != nil {
			return nil, result.err
		}
		return result.conn, nil
	case <-ctx.Done():
		_ = listener.Close()
		return nil, ctx.Err()
	}
}

func (t *elevatedWindowsTerminal) ID() string          { return t.id }
func (t *elevatedWindowsTerminal) ProjectID() string   { return t.projectID }
func (t *elevatedWindowsTerminal) Environment() string { return "windows" }
func (t *elevatedWindowsTerminal) TerminalElevated() bool {
	return true
}

func (t *elevatedWindowsTerminal) consume() {
	defer close(t.consumeDone)
	defer t.Close()
	readySent := false
	sendReady := func(err error) {
		if !readySent {
			t.ready <- err
			close(t.ready)
			readySent = true
		}
	}
	defer func() {
		sendReady(errors.New("提权终端桥接在就绪前关闭"))
		_ = t.writer.Close()
	}()
	for {
		typ, data, err := readElevatedBridgeFrame(t.conn)
		if err != nil {
			return
		}
		switch typ {
		case elevatedBridgeOutputFrame:
			if _, err := t.writer.Write(data); err != nil {
				return
			}
		case elevatedBridgeReadyFrame:
			var response struct {
				ProtocolVersion int `json:"protocolVersion"`
			}
			if json.Unmarshal(data, &response) != nil || response.ProtocolVersion != elevatedBridgeProtocolVersion {
				sendReady(errors.New("提权终端桥接协议版本不匹配"))
				return
			}
			sendReady(nil)
		case elevatedBridgeErrorFrame:
			sendReady(fmt.Errorf("提权终端桥接：%s", string(data)))
			return
		case elevatedBridgeExitFrame:
			if len(data) != 4 {
				sendReady(errors.New("提权终端桥接发送了无效退出帧"))
				return
			}
			code := int(binary.LittleEndian.Uint32(data))
			t.exitMu.Lock()
			t.exitCode = &code
			t.exitMu.Unlock()
			return
		default:
			sendReady(errors.New("提权终端桥接发送了无效帧"))
			return
		}
	}
}

func (t *elevatedWindowsTerminal) Read(p []byte) (int, error) { return t.reader.Read(p) }

func (t *elevatedWindowsTerminal) Write(p []byte) (int, error) {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	// 正常写入不设超时：helper 的读循环与 control-server 的读循环都持续排空，
	// 不会在健康链路上阻塞。若 helper 卡死，Close() 会关闭连接使此处立即返回。
	if err := writeElevatedBridgeFrame(t.conn, elevatedBridgeInputFrame, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (t *elevatedWindowsTerminal) Resize(cols, rows uint16) error {
	payload := make([]byte, 4)
	binary.LittleEndian.PutUint16(payload, cols)
	binary.LittleEndian.PutUint16(payload[2:], rows)
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	return writeElevatedBridgeFrame(t.conn, elevatedBridgeResizeFrame, payload)
}

func (t *elevatedWindowsTerminal) Close() error {
	t.closeOnce.Do(func() {
		// 关帧发送不拿 writeMu：若另一 goroutine 正阻塞在 Write，锁等待会让 Close
		// 被拖死。直接带 2s 写超时发 closeFrame 后关闭连接——关连接会解除任何
		// 正在阻塞的 Write，helper 侧读到 EOF/损坏帧即退出，job 回收 Shell 树。
		_ = t.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		_ = writeElevatedBridgeFrame(t.conn, elevatedBridgeCloseFrame, nil)
		_ = t.conn.SetWriteDeadline(time.Time{})
		_ = t.conn.Close()
		if t.process != 0 {
			_ = windows.TerminateProcess(t.process, 1)
			_ = windows.CloseHandle(t.process)
			t.process = 0
		}
		_ = t.reader.Close()
		_ = t.writer.Close()
	})
	return nil
}

func (t *elevatedWindowsTerminal) Wait() error {
	<-t.consumeDone
	t.exitMu.Lock()
	hasExit := t.exitCode != nil
	t.exitMu.Unlock()
	if hasExit {
		return nil
	}
	// 未收到 exit 帧就结束（helper 崩溃/被强杀）视为异常退出，避免被当作干净的
	// exit code 0 上报。
	return terminalExitStatus{code: 1}
}

func (t *elevatedWindowsTerminal) TerminalExitCode() *int {
	t.exitMu.Lock()
	defer t.exitMu.Unlock()
	if t.exitCode == nil {
		return nil
	}
	code := *t.exitCode
	return &code
}

func (t *elevatedWindowsTerminal) Ready() <-chan error { return t.ready }

func writeElevatedBridgeFrame(w io.Writer, typ byte, payload []byte) error {
	if len(payload)+1 > elevatedBridgeMaxFrame {
		return errors.New("terminal bridge frame is too large")
	}
	frame := make([]byte, 5+len(payload))
	binary.LittleEndian.PutUint32(frame, uint32(len(payload)+1))
	frame[4] = typ
	copy(frame[5:], payload)
	_, err := w.Write(frame)
	return err
}

func readElevatedBridgeFrame(r io.Reader) (byte, []byte, error) {
	var size uint32
	if err := binary.Read(r, binary.LittleEndian, &size); err != nil {
		return 0, nil, err
	}
	if size < 1 || size > elevatedBridgeMaxFrame {
		return 0, nil, errors.New("invalid terminal bridge frame length")
	}
	frame := make([]byte, size)
	if _, err := io.ReadFull(r, frame); err != nil {
		return 0, nil, err
	}
	return frame[0], frame[1:], nil
}
