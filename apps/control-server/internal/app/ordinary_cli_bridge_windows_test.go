package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
	"golang.org/x/crypto/ssh"
)

// Opt-in real Windows -> WSL execution. The fixture forks a setsid background
// writer; no model account is needed to test kernel and recovery guarantees.
func TestOrdinaryCLIRealWSLContainmentAndRecovery(t *testing.T) {
	bundle := os.Getenv("MILEVIA_SUPERVISION_TARGET_BUNDLE")
	if bundle == "" {
		t.Skip("set a freshly built target bundle")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	distro := os.Getenv("MILEVIA_SUPERVISION_TARGET_DISTRO")
	if distro == "" {
		distro = "Ubuntu-24.04"
	}
	config := Config{SupervisionTargetsDir: bundle}
	runner := newWSLAgentRunner(config, distro, ctx)
	raw, err := runner.supervisionTargetCommand(ctx, `mktemp -d /tmp/milevia-ordinary-cli-XXXXXX`, nil)
	if err != nil {
		t.Fatal(err)
	}
	root := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(root, "/tmp/milevia-ordinary-cli-") || strings.ContainsAny(root, "\r\n") {
		t.Fatal(root)
	}
	fixture := `set -eu
mode=$1
dir=$2
case "$mode" in
 leaf) echo $$ >"$dir/leaf.pid"; while :; do date +%s >"$dir/heartbeat"; sleep .1; done;;
 fork|persist)
   setsid /bin/sh "$0" leaf "$dir" </dev/null &
   while ! test -f "$dir/leaf.pid"; do sleep .02; done
   printf 'fixture-ready\n'
   test "$mode" != fork || exit 7
   while :; do sleep 1; done;;
esac
`
	if _, err := runner.supervisionTargetCommand(ctx, `mkdir -p "$1/first" "$1/second" "$1/fork"; ln -s -- "$1/fork" "$1/alias"; cat >"$1/fixture.sh"`, []byte(fixture), root); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t)
	s.config.SupervisionTargetsDir = bundle
	s.wslMu.Lock()
	s.wslRunner, s.wslDistro = runner, distro
	s.wslMu.Unlock()
	s.runnerRegistry.register("wsl-local", runner, RunnerMeta{ID: "wsl-local", Environment: "wsl"})
	project := Project{Runner: "wsl-local", Path: wslToUncPath(root+"/alias", distro)}
	forkCtx, releaseFork, err := s.beginCLIWorkspaceOperationAtPath(ctx, project, project.Path, "ordinary-real-fork")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseFork()
	cmd, err := runner.wslOrdinaryCLICommand(forkCtx, "/bin/sh", []string{root + "/fixture.sh", "fork", root + "/fork"}, nil, root+"/alias")
	if err != nil {
		t.Fatal(err)
	}
	var output, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &stderr
	if err := startOrdinaryCLITransport(cmd); err != nil {
		t.Fatal(err)
	}
	err = waitOrdinaryCLICommand(cmd)
	if err == nil || !strings.Contains(output.String(), "fixture-ready") {
		t.Fatal("actual fork/exit stream failed", err, output.String(), stderr.String())
	}
	noteWorkspaceWSLExit(distro, forkCtx, err, true, root+"/fork")
	releaseFork()
	if err := s.reconcilePendingWorkspaceGuardOperations(ctx); err != nil {
		t.Fatal(err)
	}
	remaining, err := s.workspaceGuardOperations(ctx, "", true, 100)
	if err != nil || len(remaining) != 0 {
		t.Fatal("real target release was not recovered", remaining, err)
	}
	// Two independent long-running containers; lose one Windows transport.
	start := func(name string) (workspaceGuardRecord, context.CancelFunc, func(), func()) {
		t.Helper()
		childCtx, stop := context.WithCancel(ctx)
		p := Project{Runner: "wsl-local", Path: wslToUncPath(root+"/"+name, distro)}
		launch, release, err := s.beginCLIWorkspaceOperationAtPath(childCtx, p, p.Path, "ordinary-real-"+name)
		if err != nil {
			t.Fatal(err)
		}
		ticket := launch.Value(ordinaryCLIContextKey{}).(ordinaryCLIExecution).ticket
		process, err := runner.wslOrdinaryCLICommand(launch, "/bin/sh", []string{root + "/fixture.sh", "persist", root + "/" + name}, nil, root+"/"+name)
		if err != nil {
			t.Fatal(err)
		}
		if err := startOrdinaryCLITransport(process); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, err := runner.supervisionTargetCommand(ctx, `test -f "$1/leaf.pid"`, nil, root+"/"+name); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("real child did not start")
			}
			time.Sleep(50 * time.Millisecond)
		}
		lose := func() {
			_ = process.Process.Kill()
			_ = waitOrdinaryCLICommand(process)
			noteWorkspaceWSLExit(distro, launch, errors.New("fixture transport lost"), true, root+"/"+name)
			release()
		}
		return ticket, stop, release, lose
	}
	first, stopFirst, releaseFirst, loseFirst := start("first")
	defer stopFirst()
	defer releaseFirst()
	second, stopSecond, releaseSecond, loseSecond := start("second")
	defer stopSecond()
	defer releaseSecond()
	loseFirst()
	route := s.supervisionProjectRoute(project)
	link, err := s.connectWorkspaceGuard(ctx, route)
	if err != nil {
		t.Fatal(err)
	}
	query := func(ticket workspaceGuardRecord) (workspaceGuardRecord, error) {
		response, err := link.Call(ctx, targetexec.Request{ID: uuid.NewString(), Method: "guard.query", Parameters: mustJSON(ticket)})
		var actual workspaceGuardRecord
		if err == nil {
			err = json.Unmarshal(response.Data, &actual)
		}
		return actual, err
	}
	deadline := time.Now().Add(40 * time.Second)
	for {
		actual, err := query(first)
		if err != nil {
			t.Fatal(err)
		}
		if actual.State == "released" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("lost WSL transport left container alive", actual)
		}
		time.Sleep(100 * time.Millisecond)
	}
	actual, err := query(second)
	if err != nil || actual.State != "active" || !actual.CLIStarted || actual.CLIProcess == nil {
		t.Fatal("other container or durable identity was lost", actual, err)
	}
	loseSecond()
	deadline = time.Now().Add(40 * time.Second)
	for {
		if err := s.reconcilePendingWorkspaceGuardOperations(ctx); err != nil {
			t.Fatal(err)
		}
		records, err := s.workspaceGuardOperations(ctx, "", true, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(records) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("unknown CLI operations did not recover", records)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Log("real setsid fork, lost transport, two containers, target receipts and controller recovery passed; retained fixture:", root)
}

func TestOrdinaryCLIRealSSHProtocolWithLinuxContainer(t *testing.T) {
	bundle := os.Getenv("MILEVIA_SUPERVISION_TARGET_BUNDLE")
	if bundle == "" {
		t.Skip("set a freshly built target bundle and provide Ubuntu WSL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	distro := os.Getenv("MILEVIA_SUPERVISION_TARGET_DISTRO")
	if distro == "" {
		distro = "Ubuntu-24.04"
	}
	wsl := newWSLAgentRunner(Config{}, distro, ctx)
	raw, err := wsl.supervisionTargetCommand(ctx, `mktemp -d /tmp/milevia-ordinary-ssh-XXXXXX`, nil)
	if err != nil {
		t.Fatal(err)
	}
	root := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(root, "/tmp/milevia-ordinary-ssh-") || strings.ContainsAny(root, "\r\n") {
		t.Fatal(root)
	}
	// A real encrypted SSH connection executes each requested command on Linux.
	// Only the SSH daemon is a local fixture; target helper and cgroups are real.
	client := workspaceSSHClientForTest(t, func(channel ssh.Channel, request *ssh.Request) {
		var payload struct{ Command string }
		if ssh.Unmarshal(request.Payload, &payload) != nil {
			_ = request.Reply(false, nil)
			return
		}
		cmd := exec.CommandContext(ctx, "wsl.exe", "-d", distro, "--exec", "/bin/sh", "-c", payload.Command)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = channel, channel, channel.Stderr()
		cmd.WaitDelay = time.Second
		if err := cmd.Start(); err != nil {
			_ = request.Reply(false, nil)
			return
		}
		_ = request.Reply(true, nil)
		_ = cmd.Wait()
		code := uint32(125)
		if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() >= 0 {
			code = uint32(cmd.ProcessState.ExitCode())
		}
		_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{code}))
	})
	runner := &sshRunner{client: client, connID: "ordinary-fixture", rootPath: root}
	s := newTestServer(t)
	s.config.SupervisionTargetsDir = bundle
	s.runnerRegistry.register("ssh-ordinary-fixture", runner, RunnerMeta{ID: "ssh-ordinary-fixture", Environment: "ssh"})
	project := Project{Runner: "ssh-ordinary-fixture", Path: root}
	launch, release, err := s.beginCLIWorkspaceOperationAtPath(ctx, project, root, "ordinary-ssh")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	command, stop, err := ordinaryCLIRemoteScript(launch, s.config, runner, root, `setsid /bin/sh -c 'while :; do sleep 1; done' </dev/null & printf 'ssh-fixture-ready\n'; while :; do sleep 1; done`)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	session, err := client.newSession(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	session.setOrdinaryCLIStop(stop)
	stdout, err := session.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Start(command); err != nil {
		t.Fatal(err)
	}
	ready := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		ready <- scanner.Scan() && scanner.Text() == "ssh-fixture-ready"
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("SSH execution output lost")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("SSH target did not execute")
	}
	// Closing the SSH channel has no exit-status receipt. Recovery must wait
	// for the target's container proof instead of trusting channel EOF.
	_ = session.Close()
	release()
	deadline := time.Now().Add(40 * time.Second)
	for {
		if err := s.reconcilePendingWorkspaceGuardOperations(ctx); err != nil {
			t.Fatal(err)
		}
		records, err := s.workspaceGuardOperations(ctx, "", true, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(records) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("SSH lost-channel container did not settle", records)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Log("real SSH framing, verified deployment, Linux detached child, missing exit receipt and target recovery passed; retained fixture:", root)
}
