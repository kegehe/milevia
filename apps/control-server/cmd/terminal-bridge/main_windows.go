//go:build windows

package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	protocolVersion = 1
	maxFrame        = 1 << 20
	openFrame       = 1
	inputFrame      = 2
	resizeFrame     = 3
	closeFrame      = 4
	authFrame       = 10
	outputFrame     = 16
	readyFrame      = 17
	exitFrame       = 18
	errorFrame      = 19
)

type openRequest struct {
	ProtocolVersion int    `json:"protocolVersion"`
	WorkDir         string `json:"workDir"`
	Shell           string `json:"shell"`
	Cols            uint16 `json:"cols"`
	Rows            uint16 `json:"rows"`
}
type frame struct {
	typ  byte
	data []byte
}

type terminalStartupInfoEx struct {
	windows.StartupInfo
	attributeList []byte
}

var (
	terminalKernel32                       = windows.NewLazySystemDLL("kernel32.dll")
	terminalInitializeProcThreadAttributes = terminalKernel32.NewProc("InitializeProcThreadAttributeList")
	terminalUpdateProcThreadAttribute      = terminalKernel32.NewProc("UpdateProcThreadAttribute")
	terminalDeleteProcThreadAttributes     = terminalKernel32.NewProc("DeleteProcThreadAttributeList")
)

func main() {
	in, out, closeTransport, err := openTransport()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer closeTransport()
	typ, data, err := readFrame(in)
	if err != nil || typ != openFrame {
		writeFrame(out, errorFrame, []byte("open frame required"))
		return
	}
	var request openRequest
	if json.Unmarshal(data, &request) != nil || request.ProtocolVersion != protocolVersion || request.Cols == 0 || request.Rows == 0 || request.WorkDir == "" {
		writeFrame(out, errorFrame, []byte("invalid open request"))
		return
	}
	inR, inW, err := newTerminalPipe()
	if err != nil {
		writeFrame(out, errorFrame, []byte(err.Error()))
		return
	}
	outR, outW, err := newTerminalPipe()
	if err != nil {
		_ = inR.Close()
		_ = inW.Close()
		writeFrame(out, errorFrame, []byte(err.Error()))
		return
	}
	defer inR.Close()
	defer inW.Close()
	defer outR.Close()
	defer outW.Close()
	var pty windows.Handle
	if err = windows.CreatePseudoConsole(windows.Coord{X: int16(request.Cols), Y: int16(request.Rows)}, windows.Handle(inR.Fd()), windows.Handle(outW.Fd()), 0, &pty); err != nil {
		writeFrame(out, errorFrame, []byte(err.Error()))
		return
	}
	defer windows.ClosePseudoConsole(pty)
	si, releaseAttributes, err := newTerminalStartupInfo(pty)
	if err != nil {
		writeFrame(out, errorFrame, []byte(err.Error()))
		return
	}
	defer releaseAttributes()
	readyMarker := "__MILEVIA_READY__"
	shellCommand, shellErr := buildShellCommand(request.Shell, readyMarker)
	if shellErr != nil {
		writeFrame(out, errorFrame, []byte(shellErr.Error()))
		return
	}
	command, _ := windows.UTF16PtrFromString(shellCommand)
	workDir, err := windows.UTF16PtrFromString(request.WorkDir)
	if err != nil {
		writeFrame(out, errorFrame, []byte(err.Error()))
		return
	}
	pi := windows.ProcessInformation{}
	if err = windows.CreateProcess(nil, command, nil, nil, false, windows.EXTENDED_STARTUPINFO_PRESENT, nil, workDir, &si.StartupInfo, &pi); err != nil {
		writeFrame(out, errorFrame, []byte(err.Error()))
		return
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		windows.CloseHandle(pi.Thread)
		windows.TerminateProcess(pi.Process, 1)
		windows.CloseHandle(pi.Process)
		writeFrame(out, errorFrame, []byte(err.Error()))
		return
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil || windows.AssignProcessToJobObject(job, pi.Process) != nil {
		windows.CloseHandle(job)
		windows.CloseHandle(pi.Thread)
		windows.TerminateProcess(pi.Process, 1)
		windows.CloseHandle(pi.Process)
		if err == nil {
			err = windows.ERROR_ACCESS_DENIED
		}
		writeFrame(out, errorFrame, []byte(err.Error()))
		return
	}
	windows.CloseHandle(pi.Thread)
	defer windows.CloseHandle(job)
	defer windows.CloseHandle(pi.Process)
	_ = inR.Close()
	_ = outW.Close()
	frames := make(chan frame, 32)
	outputDone := make(chan struct{})
	done := make(chan struct{})
	var once sync.Once
	closeAll := func() {
		once.Do(func() { _ = inW.Close(); _ = outR.Close(); _ = windows.TerminateProcess(pi.Process, 1); close(done) })
	}
	defer closeAll()
	ready, _ := json.Marshal(map[string]any{"protocolVersion": protocolVersion, "pid": pi.ProcessId})
	go func() {
		defer close(outputDone)
		buffer := make([]byte, 32<<10)
		pending := make([]byte, 0, 4096)
		marker := []byte(readyMarker)
		markerSeen := false
		send := func(item frame) bool {
			select {
			case frames <- item:
				return true
			case <-done:
				return false
			}
		}
		for {
			n, readErr := outR.Read(buffer)
			if n > 0 {
				if markerSeen {
					if !send(frame{outputFrame, append([]byte(nil), buffer[:n]...)}) {
						return
					}
				} else {
					pending = append(pending, buffer[:n]...)
					if index := bytes.Index(pending, marker); index >= 0 {
						if index > 0 && !send(frame{outputFrame, append([]byte(nil), pending[:index]...)}) {
							return
						}
						if !send(frame{readyFrame, ready}) {
							return
						}
						after := pending[index+len(marker):]
						if len(after) > 0 && !send(frame{outputFrame, append([]byte(nil), after...)}) {
							return
						}
						pending = nil
						markerSeen = true
					} else if len(pending) > 64<<10 {
						send(frame{errorFrame, []byte("terminal ready marker not received")})
						return
					}
				}
			}
			if readErr != nil {
				return
			}
		}
	}()
	go func() {
		_, _ = windows.WaitForSingleObject(pi.Process, windows.INFINITE)
		<-outputDone
		var code uint32
		_ = windows.GetExitCodeProcess(pi.Process, &code)
		payload := make([]byte, 4)
		binary.LittleEndian.PutUint32(payload, code)
		select {
		case frames <- frame{exitFrame, payload}:
		case <-done:
		}
	}()
	requests := make(chan frame)
	go func() {
		defer close(requests)
		for {
			typ, data, err := readFrame(in)
			if err != nil {
				return
			}
			select {
			case requests <- frame{typ, data}:
			case <-done:
				return
			}
		}
	}()
	for {
		select {
		case outgoing, ok := <-frames:
			if !ok {
				return
			}
			if writeFrame(out, outgoing.typ, outgoing.data) != nil {
				return
			}
			if outgoing.typ == exitFrame || outgoing.typ == errorFrame {
				return
			}
		case request, ok := <-requests:
			if !ok {
				return
			}
			switch request.typ {
			case inputFrame:
				if _, err := inW.Write(request.data); err != nil {
					return
				}
			case resizeFrame:
				if len(request.data) != 4 {
					return
				}
				if err := windows.ResizePseudoConsole(pty, windows.Coord{X: int16(binary.LittleEndian.Uint16(request.data)), Y: int16(binary.LittleEndian.Uint16(request.data[2:]))}); err != nil {
					return
				}
			case closeFrame:
				return
			default:
				return
			}
		}
	}
}

func newTerminalStartupInfo(pty windows.Handle) (*terminalStartupInfoEx, func(), error) {
	var size uintptr
	result, _, callErr := terminalInitializeProcThreadAttributes.Call(0, 1, 0, uintptr(unsafe.Pointer(&size)))
	if result != 0 || size == 0 {
		return nil, nil, fmt.Errorf("query terminal process attributes: %w", callErr)
	}
	si := &terminalStartupInfoEx{attributeList: make([]byte, size)}
	if result, _, callErr = terminalInitializeProcThreadAttributes.Call(uintptr(unsafe.Pointer(&si.attributeList[0])), 1, 0, uintptr(unsafe.Pointer(&size))); result == 0 {
		return nil, nil, fmt.Errorf("initialize terminal process attributes: %w", callErr)
	}
	if result, _, callErr = terminalUpdateProcThreadAttribute.Call(
		uintptr(unsafe.Pointer(&si.attributeList[0])),
		0,
		windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE,
		uintptr(pty),
		unsafe.Sizeof(pty),
		0,
		0,
	); result == 0 {
		terminalDeleteProcThreadAttributes.Call(uintptr(unsafe.Pointer(&si.attributeList[0])))
		return nil, nil, fmt.Errorf("set terminal pseudo-console attribute: %w", callErr)
	}
	si.Cb = uint32(unsafe.Sizeof(windows.StartupInfoEx{}))
	si.Flags = windows.STARTF_USESTDHANDLES
	return si, func() { terminalDeleteProcThreadAttributes.Call(uintptr(unsafe.Pointer(&si.attributeList[0]))) }, nil
}

// ConPTY requires synchronous pipe handles. Go's os.Pipe uses overlapped
// handles on Windows, so the bridge creates these channels directly.
func newTerminalPipe() (*os.File, *os.File, error) {
	var read, write windows.Handle
	if err := windows.CreatePipe(&read, &write, nil, 0); err != nil {
		return nil, nil, err
	}
	return os.NewFile(uintptr(read), "terminal-pipe-read"), os.NewFile(uintptr(write), "terminal-pipe-write"), nil
}

// openTransport 选择帧传输通道：
//   - 默认（无 -tcp 参数）：stdio 模式，WSL/本地 control-server 以子进程方式驱动；
//   - -tcp <addr> -token <token>：提权模式，control-server 经 UAC 拉起本进程后，
//     由本进程拨号回连并先发送 auth 帧（stdout 在 runas 下不可用）。
func openTransport() (io.Reader, io.Writer, func(), error) {
	addr := bridgeFlag("-tcp")
	if addr == "" {
		return os.Stdin, os.Stdout, func() {}, nil
	}
	conn, err := net.DialTimeout("tcp", addr, 15*time.Second)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("connect terminal bridge listener: %w", err)
	}
	if err := writeFrame(conn, authFrame, []byte(bridgeFlag("-token"))); err != nil {
		_ = conn.Close()
		return nil, nil, nil, fmt.Errorf("authenticate terminal bridge: %w", err)
	}
	return conn, conn, func() { _ = conn.Close() }, nil
}

func bridgeFlag(name string) string {
	args := os.Args[1:]
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name {
			return args[i+1]
		}
	}
	return ""
}

// buildShellCommand 按受限 Shell 令牌构造 CreateProcess 命令行。可执行路径由
// 本桥接解析，绝不信任来自协议文本里的可执行路径。
func buildShellCommand(shell, readyMarker string) (string, error) {
	systemDirectory, err := windows.GetSystemDirectory()
	if err != nil {
		return "", err
	}
	switch shell {
	case "", "cmd", "cmd.exe": // "cmd.exe" 兼容旧版 control-server 的 open 帧
		return quoteCmdArg(filepath.Join(systemDirectory, "cmd.exe")) + " /d /q /k \"chcp 65001 >nul & echo " + readyMarker + "\"", nil
	case "powershell":
		powershell := filepath.Join(systemDirectory, "WindowsPowerShell", "v1.0", "powershell.exe")
		init := "[Console]::OutputEncoding=[System.Text.Encoding]::UTF8; [Console]::InputEncoding=[System.Text.Encoding]::UTF8; chcp 65001 | Out-Null; Write-Output " + readyMarker
		return quoteCmdArg(powershell) + " -NoLogo -NoExit -Command " + quoteCmdArg(init), nil
	default:
		return "", errors.New("unsupported shell: " + shell)
	}
}

func quoteCmdArg(v string) string {
	if v != "" && !strings.ContainsAny(v, " \t\n\v\"") {
		return v
	}
	var b strings.Builder
	b.WriteByte('"')
	backslashes := 0
	for _, r := range v {
		if r == '\\' {
			backslashes++
			continue
		}
		if r == '"' {
			b.WriteString(strings.Repeat(`\`, backslashes*2+1))
			b.WriteRune(r)
			backslashes = 0
			continue
		}
		b.WriteString(strings.Repeat(`\`, backslashes))
		b.WriteRune(r)
		backslashes = 0
	}
	b.WriteString(strings.Repeat(`\`, backslashes*2))
	b.WriteByte('"')
	return b.String()
}

func writeFrame(w io.Writer, typ byte, payload []byte) error {
	if len(payload)+1 > maxFrame {
		return errors.New("frame too large")
	}
	data := make([]byte, 5+len(payload))
	binary.LittleEndian.PutUint32(data, uint32(len(payload)+1))
	data[4] = typ
	copy(data[5:], payload)
	_, err := w.Write(data)
	return err
}
func readFrame(r io.Reader) (byte, []byte, error) {
	var size uint32
	if err := binary.Read(r, binary.LittleEndian, &size); err != nil {
		return 0, nil, err
	}
	if size < 1 || size > maxFrame {
		return 0, nil, errors.New("invalid frame length")
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(r, data); err != nil {
		return 0, nil, err
	}
	return data[0], data[1:], nil
}
