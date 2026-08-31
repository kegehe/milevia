//go:build windows

package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
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
	typ, data, err := readFrame(os.Stdin)
	if err != nil || typ != openFrame {
		writeFrame(os.Stdout, errorFrame, []byte("open frame required"))
		return
	}
	var request openRequest
	if json.Unmarshal(data, &request) != nil || request.ProtocolVersion != protocolVersion || request.Cols == 0 || request.Rows == 0 || request.WorkDir == "" {
		writeFrame(os.Stdout, errorFrame, []byte("invalid open request"))
		return
	}
	if request.Shell != "" && request.Shell != "cmd.exe" {
		writeFrame(os.Stdout, errorFrame, []byte("unsupported shell"))
		return
	}
	inR, inW, err := newTerminalPipe()
	if err != nil {
		writeFrame(os.Stdout, errorFrame, []byte(err.Error()))
		return
	}
	outR, outW, err := newTerminalPipe()
	if err != nil {
		_ = inR.Close()
		_ = inW.Close()
		writeFrame(os.Stdout, errorFrame, []byte(err.Error()))
		return
	}
	defer inR.Close()
	defer inW.Close()
	defer outR.Close()
	defer outW.Close()
	var pty windows.Handle
	if err = windows.CreatePseudoConsole(windows.Coord{X: int16(request.Cols), Y: int16(request.Rows)}, windows.Handle(inR.Fd()), windows.Handle(outW.Fd()), 0, &pty); err != nil {
		writeFrame(os.Stdout, errorFrame, []byte(err.Error()))
		return
	}
	defer windows.ClosePseudoConsole(pty)
	si, releaseAttributes, err := newTerminalStartupInfo(pty)
	if err != nil {
		writeFrame(os.Stdout, errorFrame, []byte(err.Error()))
		return
	}
	defer releaseAttributes()
	readyMarker := "__MILEVIA_READY__"
	command, _ := windows.UTF16PtrFromString("cmd.exe /d /q /k \"chcp 65001 >nul & echo " + readyMarker + "\"")
	workDir, err := windows.UTF16PtrFromString(request.WorkDir)
	if err != nil {
		writeFrame(os.Stdout, errorFrame, []byte(err.Error()))
		return
	}
	pi := windows.ProcessInformation{}
	if err = windows.CreateProcess(nil, command, nil, nil, false, windows.EXTENDED_STARTUPINFO_PRESENT, nil, workDir, &si.StartupInfo, &pi); err != nil {
		writeFrame(os.Stdout, errorFrame, []byte(err.Error()))
		return
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		windows.CloseHandle(pi.Thread)
		windows.TerminateProcess(pi.Process, 1)
		windows.CloseHandle(pi.Process)
		writeFrame(os.Stdout, errorFrame, []byte(err.Error()))
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
		writeFrame(os.Stdout, errorFrame, []byte(err.Error()))
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
			typ, data, err := readFrame(os.Stdin)
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
			if writeFrame(os.Stdout, outgoing.typ, outgoing.data) != nil {
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
