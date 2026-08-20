//go:build linux

package app

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/google/uuid"
)

const (
	terminalBridgeProtocolVersion = 1
	terminalBridgeMaxFrame        = 1 << 20
	bridgeOpenFrame               = 1
	bridgeInputFrame              = 2
	bridgeResizeFrame             = 3
	bridgeCloseFrame              = 4
	bridgeOutputFrame             = 16
	bridgeReadyFrame              = 17
	bridgeExitFrame               = 18
	bridgeErrorFrame              = 19
)

type terminalBridgeOpen struct {
	ProtocolVersion int    `json:"protocolVersion"`
	WorkDir         string `json:"workDir"`
	Shell           string `json:"shell"`
	Cols            uint16 `json:"cols"`
	Rows            uint16 `json:"rows"`
}

type bridgeTerminalSession struct {
	id, projectID string
	cmd           *exec.Cmd
	stdin         io.WriteCloser
	reader        *io.PipeReader
	writer        *io.PipeWriter
	ready         chan error
	waitDone      chan struct{}
	consumeDone   chan struct{}
	waitErr       error
	exitMu        sync.Mutex
	exitCode      *int
	closeOnce     sync.Once
	writeMu       sync.Mutex
}

func openWindowsBridgeTerminal(ctx context.Context, spec TerminalSpec) (TerminalSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bridge := os.Getenv("MILEVIA_TERMINAL_BRIDGE")
	if bridge == "" {
		return nil, errors.New("Windows terminal bridge is unavailable; set MILEVIA_TERMINAL_BRIDGE")
	}
	workDir, ok := wslPathToWindowsPath(spec.WorkDir)
	if !ok {
		return nil, fmt.Errorf("cannot map project path to Windows: %s", spec.WorkDir)
	}
	cmd := exec.Command(bridge)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	reader, writer := io.Pipe()
	t := &bridgeTerminalSession{id: uuid.NewString(), projectID: spec.ProjectID, cmd: cmd, stdin: stdin, reader: reader, writer: writer, ready: make(chan error, 1), waitDone: make(chan struct{}), consumeDone: make(chan struct{})}
	openPayload, err := json.Marshal(terminalBridgeOpen{ProtocolVersion: terminalBridgeProtocolVersion, WorkDir: workDir, Shell: "cmd.exe", Cols: spec.Cols, Rows: spec.Rows})
	if err != nil || t.writeFrame(bridgeOpenFrame, openPayload) != nil {
		_ = t.Close()
		_ = cmd.Wait()
		if err == nil {
			err = errors.New("cannot initialize Windows terminal bridge")
		}
		return nil, err
	}
	go t.consumeBridge(stdout)
	go func() {
		// exec.Cmd.Wait closes StdoutPipe. Waiting until consumeBridge finishes
		// prevents it from racing the protocol reader and dropping ready/output
		// frames on a fast-exiting bridge.
		<-t.consumeDone
		t.waitErr = cmd.Wait()
		close(t.waitDone)
		_ = writer.Close()
	}()
	return t, nil
}

func (t *bridgeTerminalSession) consumeBridge(reader io.Reader) {
	defer close(t.consumeDone)
	// A bridge represents exactly one terminal. Once its output protocol ends,
	// stop its process as well so Wait cannot hold a terminal record forever.
	defer t.Close()
	readySent := false
	sendReady := func(err error) {
		if !readySent {
			t.ready <- err
			close(t.ready)
			readySent = true
		}
	}
	defer func() { sendReady(errors.New("Windows terminal bridge closed before ready")); _ = t.writer.Close() }()
	for {
		typ, data, err := readBridgeFrame(reader)
		if err != nil {
			return
		}
		switch typ {
		case bridgeOutputFrame:
			if _, err := t.writer.Write(data); err != nil {
				return
			}
		case bridgeReadyFrame:
			var response struct {
				ProtocolVersion int `json:"protocolVersion"`
			}
			if json.Unmarshal(data, &response) != nil || response.ProtocolVersion != terminalBridgeProtocolVersion {
				sendReady(errors.New("Windows terminal bridge protocol mismatch"))
				return
			}
			sendReady(nil)
		case bridgeErrorFrame:
			sendReady(fmt.Errorf("Windows terminal bridge: %s", string(data)))
			return
		case bridgeExitFrame:
			if len(data) != 4 {
				sendReady(errors.New("Windows terminal bridge sent an invalid exit frame"))
				return
			}
			code := int(binary.LittleEndian.Uint32(data))
			t.exitMu.Lock()
			t.exitCode = &code
			t.exitMu.Unlock()
			return
		default:
			sendReady(errors.New("Windows terminal bridge sent an invalid frame"))
			return
		}
	}
}

func (t *bridgeTerminalSession) ID() string                 { return t.id }
func (t *bridgeTerminalSession) ProjectID() string          { return t.projectID }
func (t *bridgeTerminalSession) Environment() string        { return "windows" }
func (t *bridgeTerminalSession) Ready() <-chan error        { return t.ready }
func (t *bridgeTerminalSession) Read(p []byte) (int, error) { return t.reader.Read(p) }
func (t *bridgeTerminalSession) Write(p []byte) (int, error) {
	if err := t.writeFrame(bridgeInputFrame, p); err != nil {
		return 0, err
	}
	return len(p), nil
}
func (t *bridgeTerminalSession) Resize(cols, rows uint16) error {
	payload := make([]byte, 4)
	binary.LittleEndian.PutUint16(payload, cols)
	binary.LittleEndian.PutUint16(payload[2:], rows)
	return t.writeFrame(bridgeResizeFrame, payload)
}
func (t *bridgeTerminalSession) Close() error {
	t.closeOnce.Do(func() {
		_ = t.writeFrame(bridgeCloseFrame, nil)
		_ = t.stdin.Close()
		_ = t.reader.Close()
		_ = t.writer.Close()
		if t.cmd.Process != nil {
			_ = t.cmd.Process.Kill()
		}
	})
	return nil
}
func (t *bridgeTerminalSession) Wait() error {
	<-t.waitDone
	<-t.consumeDone
	return t.waitErr
}
func (t *bridgeTerminalSession) TerminalExitCode() *int {
	t.exitMu.Lock()
	defer t.exitMu.Unlock()
	if t.exitCode == nil {
		return nil
	}
	code := *t.exitCode
	return &code
}
func (t *bridgeTerminalSession) writeFrame(typ byte, payload []byte) error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	return writeBridgeFrame(t.stdin, typ, payload)
}

func writeBridgeFrame(w io.Writer, typ byte, payload []byte) error {
	if len(payload)+1 > terminalBridgeMaxFrame {
		return errors.New("terminal bridge frame is too large")
	}
	frame := make([]byte, 5+len(payload))
	binary.LittleEndian.PutUint32(frame, uint32(len(payload)+1))
	frame[4] = typ
	copy(frame[5:], payload)
	_, err := w.Write(frame)
	return err
}
func readBridgeFrame(r io.Reader) (byte, []byte, error) {
	var size uint32
	if err := binary.Read(r, binary.LittleEndian, &size); err != nil {
		return 0, nil, err
	}
	if size < 1 || size > terminalBridgeMaxFrame {
		return 0, nil, errors.New("invalid terminal bridge frame length")
	}
	frame := make([]byte, size)
	if _, err := io.ReadFull(r, frame); err != nil {
		return 0, nil, err
	}
	return frame[0], frame[1:], nil
}
