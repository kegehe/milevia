package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"sync"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// A lost SSH channel is not a remote exit receipt. Known command scopes retain
// only overlapping operations; an unscoped/whole-transport failure remains
// conservative because there is no evidence for a narrower affected directory.
type workspaceSSHSession struct {
	ordinaryCLIStop context.CancelFunc

	*ssh.Session
	client                                  *sshClient
	workspacePath                           string
	mu                                      sync.Mutex
	attempted, confirmed, uncertain, closed bool
}

func sshWorkspaceExitConfirmed(err error) bool {
	var status *ssh.ExitError
	return err == nil || errors.As(err, &status) && status.ExitStatus() >= 0
}

func (s *workspaceSSHSession) markUncertainLocked() {
	if !s.uncertain {
		s.uncertain = true
		if s.workspacePath == "" {
			s.client.workspaceUncertainty.Add(1)
		} else {
			s.client.workspaceFailures.mark(s.workspacePath)
		}
	}
}

func (s *workspaceSSHSession) Start(command string) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("SSH session is closed")
	}
	s.attempted = true
	s.mu.Unlock()
	err := s.Session.Start(command)
	if err != nil {
		s.mu.Lock()
		s.markUncertainLocked() // The exec reply itself may have been lost.
		s.mu.Unlock()
	}
	return err
}

func (s *workspaceSSHSession) Wait() error {
	err := s.Session.Wait()
	s.mu.Lock()
	s.confirmed = sshWorkspaceExitConfirmed(err)
	if !s.confirmed {
		s.markUncertainLocked()
	}
	s.mu.Unlock()
	return err
}

func (s *workspaceSSHSession) Close() error {
	s.mu.Lock()
	stop := s.ordinaryCLIStop
	s.ordinaryCLIStop = nil
	s.closed = true
	if s.attempted && !s.confirmed {
		s.markUncertainLocked()
	}
	s.mu.Unlock()
	if stop != nil {
		stop()
	}
	return s.Session.Close()
}

func (s *workspaceSSHSession) setOrdinaryCLIStop(stop context.CancelFunc) {
	s.mu.Lock()
	closed := s.closed
	if !closed {
		s.ordinaryCLIStop = stop
	}
	s.mu.Unlock()
	if closed && stop != nil {
		stop()
	}
}

func (s *workspaceSSHSession) Run(command string) error {
	if err := s.Start(command); err != nil {
		return err
	}
	return s.Wait()
}

func (s *workspaceSSHSession) CombinedOutput(command string) ([]byte, error) {
	var buffer bytes.Buffer
	// SSH writes stdout/stderr concurrently; use the same locked writer.
	output := &lockedWriter{writer: &buffer}
	s.Stdout, s.Stderr = output, output
	err := s.Run(command)
	return buffer.Bytes(), err
}

func (s *workspaceSSHSession) WorkspaceExitConfirmed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.confirmed && !s.uncertain
}

func (c *sshClient) newWorkspaceSession(client *ssh.Client, paths ...string) (*workspaceSSHSession, error) {
	if client == nil {
		return nil, errors.New("SSH 连接已断开")
	}
	session, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	p := ""
	if len(paths) > 0 {
		p = paths[0]
	}
	return &workspaceSSHSession{Session: session, client: c, workspacePath: p}, nil
}

func (c *sshClient) workspaceSFTPError(err error, paths ...string) error {
	var status *sftp.StatusError
	// A server status is an acknowledged result. A lost SFTP reply is not.
	acknowledged := errors.As(err, &status) && status.Code != 6 && status.Code != 7
	if err != nil && !acknowledged && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, os.ErrExist) && !errors.Is(err, os.ErrPermission) {
		if len(paths) == 0 || paths[0] == "" {
			c.workspaceUncertainty.Add(1)
		} else {
			for _, p := range paths {
				c.workspaceFailures.mark(p)
			}
		}
	}
	return err
}
