package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestNativeModelCatalogSeparatesSSHDiagnostics(t *testing.T) {
	client := workspaceSSHClientForTest(t, func(channel ssh.Channel, req *ssh.Request) {
		var command struct{ Value string }
		_ = ssh.Unmarshal(req.Payload, &command)
		if !strings.Contains(command.Value, "cd '/project with spaces' &&") {
			t.Error("probe lost project working directory", command.Value)
		}
		_ = req.Reply(true, nil)
		_, _ = channel.Stderr().Write([]byte("Checking for updates...\n"))
		_, _ = channel.Write([]byte("[{\"id\":\"auto\"}]"))
		_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{0}))
	})
	s := &Server{runnerRegistry: newRunnerRegistry()}
	meta := RunnerMeta{ID: "ssh-model-review", Environment: "remote-linux"}
	s.runnerRegistry.register(meta.ID, &sshRunner{client: client}, meta)
	models := s.nativeModelCatalog(context.Background(), "qoder", meta.ID, "/project with spaces")
	if len(models) != 1 || models[0].ID != "auto" {
		t.Fatalf("stderr contaminated structured model list: %+v", models)
	}
}

func TestSSHBoundedProbeRejectsOversizedStdout(t *testing.T) {
	client := workspaceSSHClientForTest(t, func(channel ssh.Channel, req *ssh.Request) {
		_ = req.Reply(true, nil)
		_, _ = channel.Write(bytes.Repeat([]byte("x"), 4<<20))
		_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{0}))
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stdout, _, err := client.execCommandSeparate(ctx, "fixture", 1024)
	if err != nil || len(stdout) != 1025 {
		t.Fatalf("stdout cap lost overflow sentinel or blocked: %d %v", len(stdout), err)
	}
	output, err := (&sshRunner{client: client}).crossRunLimited(ctx, "fixture", 1024)
	if err == nil || output != "" || ctx.Err() != nil {
		t.Fatalf("oversized probe was accepted or not drained: %d %v", len(output), err)
	}
}

func TestSSHSeparateOutputStartFailure(t *testing.T) {
	client := workspaceSSHClientForTest(t, func(_ ssh.Channel, req *ssh.Request) {
		_ = req.Reply(false, nil)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := client.execCommandWithStdinSeparate(ctx, "fixture", []byte("input"), 1024)
	if err == nil || ctx.Err() != nil {
		t.Fatal("exec rejection did not settle promptly", err)
	}
	if client.workspaceUncertainty.Load() == 0 {
		t.Fatal("exec failure bypassed conservative process tracking")
	}
}

func TestSSHSeparateOutputDrainsBothStreams(t *testing.T) {
	for _, withStdin := range []bool{false, true} {
		name := "without-stdin"
		if withStdin {
			name = "with-stdin"
		}
		t.Run(name, func(t *testing.T) {
			client := workspaceSSHClientForTest(t, func(channel ssh.Channel, req *ssh.Request) {
				_ = req.Reply(true, nil)
				if withStdin {
					input, _ := io.ReadAll(channel)
					if string(input) != "input" {
						t.Error("stdin not delivered", string(input))
					}
				}
				// More than SSH's shared window: sequential readers deadlock here.
				_, _ = channel.Stderr().Write(bytes.Repeat([]byte("e"), 4<<20))
				_, _ = channel.Write([]byte("structured-result"))
				_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{7}))
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var stdout, stderr []byte
			var err error
			if withStdin {
				stdout, stderr, err = client.execCommandWithStdinSeparate(ctx, "fixture", []byte("input"), 1024)
			} else {
				stdout, stderr, err = client.execCommandSeparate(ctx, "fixture", 1024)
			}
			var exit *ssh.ExitError
			if !errors.As(err, &exit) || exit.ExitStatus() != 7 {
				t.Fatalf("expected exit 7 without blocking: %v", err)
			}
			if string(stdout) != "structured-result" || len(stderr) != 64<<10 {
				t.Fatalf("incorrect output: stdout=%q stderr size=%d", stdout, len(stderr))
			}
			if client.workspaceUncertainty.Load() != 0 {
				t.Fatal("confirmed nonzero exit was treated as unknown process lifetime")
			}
		})
	}
}
