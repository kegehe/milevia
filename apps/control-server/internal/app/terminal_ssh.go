package app

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"
)

type sshTerminalSession struct {
	id, projectID string
	session       *ssh.Session
	stdin         io.WriteCloser
	reader        *io.PipeReader
	writer        *io.PipeWriter
	ready         chan error
	closeOnce     sync.Once
	outputOnce    sync.Once
	writeMu       sync.Mutex
}

type lockedWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(p)
}

func (s *Server) openSSHTerminal(ctx context.Context, spec TerminalSpec) (TerminalSession, error) {
	runner, ok := s.runnerRegistry.get(spec.RunnerID)
	if !ok {
		return nil, errors.New("SSH runner is offline")
	}
	sshRunner, ok := runner.(*sshRunner)
	if !ok {
		return nil, errors.New("runner is not SSH")
	}
	workDir, err := sshRunner.canonicalProjectPath(ctx, spec.WorkDir)
	if err != nil {
		return nil, err
	}
	sess, err := sshRunner.client.newSession(ctx)
	if err != nil {
		return nil, err
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		_ = sess.Close()
		return nil, err
	}
	reader, writer := io.Pipe()
	output := &lockedWriter{writer: writer}
	sess.Stdout = output
	sess.Stderr = output
	if err := sess.RequestPty("xterm-256color", int(spec.Rows), int(spec.Cols), ssh.TerminalModes{}); err != nil {
		_ = writer.Close()
		_ = reader.Close()
		_ = sess.Close()
		return nil, err
	}
	command := "cd " + shellQuote(workDir) + " && exec \"${SHELL:-/bin/sh}\" -l"
	if err := sess.Start(command); err != nil {
		_ = writer.Close()
		_ = reader.Close()
		_ = sess.Close()
		return nil, err
	}
	t := &sshTerminalSession{id: uuid.NewString(), projectID: spec.ProjectID, session: sess, stdin: stdin, reader: reader, writer: writer, ready: make(chan error, 1)}
	t.ready <- nil
	close(t.ready)
	return t, nil
}
func (t *sshTerminalSession) ID() string                 { return t.id }
func (t *sshTerminalSession) ProjectID() string          { return t.projectID }
func (t *sshTerminalSession) Environment() string        { return "remote-linux" }
func (t *sshTerminalSession) Ready() <-chan error        { return t.ready }
func (t *sshTerminalSession) Read(p []byte) (int, error) { return t.reader.Read(p) }
func (t *sshTerminalSession) Write(p []byte) (int, error) {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	return t.stdin.Write(p)
}
func (t *sshTerminalSession) Resize(cols, rows uint16) error {
	return t.session.WindowChange(int(rows), int(cols))
}
func (t *sshTerminalSession) Close() error {
	var err error
	t.closeOnce.Do(func() { err = t.session.Close(); _ = t.stdin.Close(); t.closeOutput(); _ = t.reader.Close() })
	return err
}
func (t *sshTerminalSession) Wait() error {
	err := t.session.Wait()
	t.closeOutput()
	return err
}
func (t *sshTerminalSession) closeOutput() { t.outputOnce.Do(func() { _ = t.writer.Close() }) }
