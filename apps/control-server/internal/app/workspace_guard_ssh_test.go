package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// Actual SSH framing, exit-status, and lost-channel behavior; no model or
// external account is required.
func workspaceSSHClientForTest(t *testing.T, run func(ssh.Channel, *ssh.Request)) *sshClient {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, portText, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	client, err := newSSHClient(SSHConnection{Host: host, Port: port, User: "test", Password: "fixture", KnownHosts: string(ssh.MarshalAuthorizedKey(signer.PublicKey()))}, false)
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		server, channels, requests, err := ssh.NewServerConn(connection, config)
		if err != nil {
			return
		}
		defer server.Close()
		go ssh.DiscardRequests(requests)
		for request := range channels {
			channel, requests, err := request.Accept()
			if err != nil {
				continue
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer channel.Close()
				for req := range requests {
					if req.Type == "exec" {
						run(channel, req)
						return
					}
					_ = req.Reply(false, nil)
				}
			}()
		}
	}()
	t.Cleanup(func() { client.close(); listener.Close(); workers.Wait() })
	return client
}

func TestWorkspaceGuardSSHExitReceiptsAndCancellation(t *testing.T) {
	for _, scenario := range []string{"success", "failure", "missing-status", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			client := workspaceSSHClientForTest(t, func(channel ssh.Channel, req *ssh.Request) {
				_ = req.Reply(true, nil)
				close(started)
				if scenario == "cancel" {
					<-release
					return
				}
				_, _ = channel.Write([]byte("actual SSH output"))
				if scenario != "missing-status" {
					code := uint32(0)
					if scenario == "failure" {
						code = 7
					}
					_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{code}))
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if scenario == "cancel" {
				go func() { <-started; cancel() }()
			}
			_, err := client.execCommand(ctx, "fixture")
			close(release)
			if scenario == "success" && err != nil {
				t.Fatal(err)
			}
			if scenario != "success" && err == nil {
				t.Fatal("missing failure")
			}
			unknown := client.workspaceUncertainty.Load() != 0
			if unknown != (scenario == "missing-status" || scenario == "cancel") {
				t.Fatal("incorrect remote process certainty", unknown, err)
			}
		})
	}
}

func TestWorkspaceGuardSSHProjectRunnerHoldsProcessLifetime(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	var finishOnce sync.Once
	defer finishOnce.Do(func() { close(finish) })
	client := workspaceSSHClientForTest(t, func(channel ssh.Channel, req *ssh.Request) {
		_ = req.Reply(true, nil)
		var command struct{ Value string }
		_ = ssh.Unmarshal(req.Payload, &command)
		if command.Value != "" && command.Value[0] != 'r' {
			close(started)
			<-finish
		}
		_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{0}))
	})
	var active atomic.Int32
	runner := newSSHProjectRunner("project", client, "/actual/project", func(LogEntry) {})
	runner.workspaceAdmission = func(path string) (func(), error) {
		if path != "/actual/project" {
			t.Fatal("wrong target directory", path)
		}
		active.Add(1)
		return func() { active.Add(-1) }, nil
	}
	if err := runner.StartWithConfig(context.Background(), "/actual/project", "", "fixture", nil, RunExecutionTargetAuto); err != nil {
		t.Fatal(err)
	}
	<-started
	if active.Load() != 1 {
		t.Fatal("request completion released live remote process")
	}
	finishOnce.Do(func() { close(finish) })
	select {
	case <-runner.exitChan:
	case <-time.After(5 * time.Second):
		t.Fatal("process did not settle")
	}
	if active.Load() != 0 {
		t.Fatal("confirmed process exit did not release guard")
	}
	runner.workspaceAdmission = func(string) (func(), error) { return nil, errWorkspaceSupervised }
	if err := runner.StartWithConfig(context.Background(), "/actual/project", "", "fixture", nil, RunExecutionTargetAuto); !errors.Is(err, errWorkspaceSupervised) {
		t.Fatal("owned directory bypassed", err)
	}
}

func TestWorkspaceGuardTerminalUnknownExitRetainsProtection(t *testing.T) {
	for _, code := range []*int{nil, func() *int { n := -1; return &n }()} {
		released := false
		r := &terminalRecord{readerDone: true, waiterDone: true, exitCode: code, releaseWorkspace: func() { released = true }}
		if release := r.takeWorkspaceReleaseLocked(); release != nil {
			release()
		}
		if released || r.releaseWorkspace == nil {
			t.Fatal("unknown target exit released protection")
		}
		zero := 0
		r.exitCode = &zero
		r.takeWorkspaceReleaseLocked()()
		if !released {
			t.Fatal("explicit exit receipt did not release protection")
		}
	}
}

func TestWorkspaceGuardForcedStopWaitsForRunnerReturn(t *testing.T) {
	released := false
	s := &Server{runWorkspaceReleases: map[string]func(){"run": func() { released = true }}, forcedStopRuns: map[string]string{"run": "conversation"}, runContexts: map[string]string{"run": "conversation"}}
	s.releaseRunWorkspace("run")
	if released {
		t.Fatal("logical stop released a runner still executing")
	}
	delete(s.runContexts, "run")
	s.releaseRunWorkspace("run")
	if !released {
		t.Fatal("runner completion did not allow release")
	}
}

func TestWorkspaceGuardSFTPTransportFailureRetainsUncertainty(t *testing.T) {
	for _, err := range []error{nil, &sftp.StatusError{Code: 3}, &sftp.StatusError{Code: 4}} {
		client := &sshClient{}
		client.workspaceSFTPError(err)
		if client.workspaceUncertainty.Load() != 0 {
			t.Fatal("acknowledged SFTP result treated as transport loss", err)
		}
	}
	for _, err := range []error{io.EOF, io.ErrUnexpectedEOF, &sftp.StatusError{Code: 6}, &sftp.StatusError{Code: 7}} {
		client := &sshClient{}
		client.workspaceSFTPError(err)
		if client.workspaceUncertainty.Load() != 1 {
			t.Fatal("lost SFTP reply treated as completion", err)
		}
	}
}

func TestWorkspaceGuardUnresolvedDirectoryCannotBecomeLegacyLease(t *testing.T) {
	s := &Server{}
	if release, err := s.beginSharedWorkspaceOperation(workspaceScope{OriginalPath: "unresolved-project"}); err == nil {
		release()
		t.Fatal("failed directory resolution silently skipped the shared journal")
	}
	if release, err := s.beginSharedWorkspaceOperation(workspaceScope{}); err != nil {
		t.Fatal("legacy non-directory lease was rejected", err)
	} else {
		release()
	}
}

func TestWorkspaceGuardStreamingExitReleasesPreviouslyFinalizedRun(t *testing.T) {
	s, project, conversation := admissionProjectForTest(t)
	if _, err := s.db.Exec(`insert into runs(id,conversation_id,status,created_at,completed_at) values ('replayed-exit',?,'stopped',?,?)`, conversation, time.Now().UTC(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	release, err := s.beginWorkspaceOperationAtPath(project, project.Path, "streaming-run")
	if err != nil {
		t.Fatal(err)
	}
	s.registerRunWorkspace("replayed-exit", release)
	session := newQueuedAgentSession()
	close(session.done)
	managed := &activeAgentSession{agent: session, stopping: true, runIDs: map[string]struct{}{"replayed-exit": {}}}
	s.mu.Lock()
	s.sessions[conversation] = managed
	s.mu.Unlock()
	s.watchStreamingSession(conversation, managed)
	s.mu.Lock()
	_, retained := s.runWorkspaceReleases["replayed-exit"]
	s.mu.Unlock()
	var tokens int
	if err := s.db.QueryRow(`select count(*) from workspace_guard_operations`).Scan(&tokens); err != nil || tokens != 0 || retained {
		t.Fatal("actual session exit left a logically finalized run's protection held", retained, tokens, err)
	}
	a := reserveAdmissionForTest(t, s, project, "after-confirmed-exit")
	ownAdmissionForTest(t, s, a)
}

func TestWorkspaceGuardRecoveryRetainsOrdinaryOperationToken(t *testing.T) {
	s, project, _ := admissionProjectForTest(t)
	release, err := s.beginWorkspaceOperationAtPath(project, project.Path, "controller-before-restart")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := s.recoverWorkspaceAdmissions(context.Background()); err != nil {
		t.Fatal(err)
	}
	var id, state string
	if err := s.db.QueryRow(`select id,state from workspace_guard_operations`).Scan(&id, &state); err != nil || id == "" || state != "unconfirmed" {
		t.Fatal("controller recovery lost the ordinary operation identity", id, state, err)
	}
	if _, err := changeWorkspaceGuard(context.Background(), s.config, "reserve", workspaceGuardRecord{ID: "after-restart", Kind: "supervision", Path: project.Path}); err != nil {
		t.Fatal(err)
	}
	if _, err := changeWorkspaceGuard(context.Background(), s.config, "own", workspaceGuardRecord{ID: "after-restart", Kind: "supervision"}); !errors.Is(err, errWorkspaceNotQuiescent) {
		t.Fatal("controller recovery cleared the still-unconfirmed writer", err)
	}
}
