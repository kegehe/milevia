package app

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

func TestWorkspaceGuardIndependentOwnersDrainAndTombstone(t *testing.T) {
	ctx := context.Background()
	config := Config{WorkspaceGuardDir: filepath.Join(t.TempDir(), "common")}
	root := t.TempDir()
	child := filepath.Join(root, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	change := func(method, id, kind, path string) error {
		_, err := changeWorkspaceGuard(ctx, config, method, workspaceGuardRecord{ID: id, Kind: kind, Path: path, ValidUntil: time.Now().UTC().Add(time.Minute)})
		return err
	}
	if err := change("begin", "writer", "operation", child); err != nil {
		t.Fatal(err)
	}
	if err := change("reserve", "supervisor", "supervision", root); err != nil {
		t.Fatal(err)
	}
	if err := change("begin", "second-writer", "operation", child); !errors.Is(err, errWorkspaceSupervised) {
		t.Fatalf("pending allowed writer: %v", err)
	}
	if err := change("own", "supervisor", "supervision", ""); !errors.Is(err, errWorkspaceNotQuiescent) {
		t.Fatalf("failed to drain: %v", err)
	}
	if err := change("release", "writer", "operation", ""); err != nil {
		t.Fatal(err)
	}
	if err := change("own", "supervisor", "supervision", ""); err != nil {
		t.Fatal(err)
	}
	// A second database/controller cannot overlap the directory or its parent.
	if err := change("reserve", "other-owner", "supervision", filepath.Dir(root)); !errors.Is(err, errWorkspaceSupervised) {
		t.Fatalf("parent escaped: %v", err)
	}
	if err := change("release", "supervisor", "supervision", ""); err != nil {
		t.Fatal(err)
	}
	if err := change("reserve", "supervisor", "supervision", root); !errors.Is(err, targetexec.ErrAuthority) {
		t.Fatalf("late reserve resurrected: %v", err)
	}
	if err := change("release", "cancelled-first", "operation", ""); err != nil {
		t.Fatal(err)
	}
	if err := change("begin", "cancelled-first", "operation", root); err == nil {
		t.Fatal("late ordinary begin resurrected")
	}
}

func TestWorkspaceGuardProcessPersistsAfterOwnerExit(t *testing.T) {
	root, storage := t.TempDir(), t.TempDir()
	command := exec.Command(os.Args[0], "-test.run=^TestWorkspaceGuardChild$", "-test.count=1")
	command.Env = append(os.Environ(), "MILEVIA_GUARD_CHILD=1", "MILEVIA_GUARD_ROOT="+root, "MILEVIA_GUARD_STORAGE="+storage)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("child: %v %s", err, output)
	}
	config := Config{WorkspaceGuardDir: storage}
	ctx := context.Background()
	if _, err := changeWorkspaceGuard(ctx, config, "reserve", workspaceGuardRecord{ID: "new-supervisor", Kind: "supervision", Path: root}); err != nil {
		t.Fatal(err)
	}
	if _, err := changeWorkspaceGuard(ctx, config, "own", workspaceGuardRecord{ID: "new-supervisor", Kind: "supervision"}); !errors.Is(err, errWorkspaceNotQuiescent) {
		t.Fatalf("owner exit falsely proved writer quiet: %v", err)
	}
}

func TestWorkspaceGuardChild(t *testing.T) {
	if os.Getenv("MILEVIA_GUARD_CHILD") != "1" {
		t.Skip("subprocess only")
	}
	_, err := changeWorkspaceGuard(context.Background(), Config{WorkspaceGuardDir: os.Getenv("MILEVIA_GUARD_STORAGE")}, "begin", workspaceGuardRecord{ID: "orphan-writer", Kind: "operation", Path: os.Getenv("MILEVIA_GUARD_ROOT"), ValidUntil: time.Now().UTC().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceGuardConcurrentControllersSingleWinner(t *testing.T) {
	config := Config{WorkspaceGuardDir: t.TempDir()}
	root := t.TempDir()
	var wg sync.WaitGroup
	results := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := time.Unix(int64(i), 0).Format("150405")
			_, err := changeWorkspaceGuard(context.Background(), config, "reserve", workspaceGuardRecord{ID: id, Kind: "supervision", Path: root})
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		} else if !errors.Is(err, errWorkspaceSupervised) {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatalf("winners=%d", winners)
	}
}

func TestWorkspaceGuardRestrictedPipeDisconnectRetainsOperation(t *testing.T) {
	config := Config{WorkspaceGuardDir: t.TempDir()}
	root := t.TempDir()
	owner, channel := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- ServeWorkspaceGuard(context.Background(), config, channel) }()
	client := targetexec.NewClient(owner)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := client.Call(ctx, targetexec.Request{ID: "hello", Method: "hello"}); err != nil {
		t.Fatal(err)
	}
	record := workspaceGuardRecord{ID: "pipe-writer", Kind: "operation", Path: root, ValidUntil: time.Now().UTC().Add(time.Minute)}
	if _, err := client.Call(ctx, targetexec.Request{ID: "begin", Method: "guard.begin", Parameters: mustJSON(record)}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Call(ctx, targetexec.Request{ID: "forbidden", Method: "start"}); !errors.Is(err, targetexec.ErrProtocol) {
		t.Fatalf("restricted endpoint accepted execution: %v", err)
	}
	client.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var facts workspaceGuardFacts
	path := filepath.Join(config.WorkspaceGuardDir, "journal.json")
	raw, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(raw, &facts) != nil || facts.Records[record.ID].State != "active" {
		t.Fatalf("EOF released writer: %s %v", raw, err)
	}
}

func TestWorkspaceGuardRemoteBeginUsesFreshBoundedTargetClock(t *testing.T) {
	s := newTestServer(t)
	left, right := net.Pipe()
	var healthCalls, writers, releases atomic.Int32
	transport := &targetexec.Transport{EndpointIdentity: "clock-guard", Extension: func(_ context.Context, request targetexec.Request) (json.RawMessage, error) {
		if request.Method == "health" {
			healthCalls.Add(1)
			return mustJSON(map[string]any{"protocol": targetexec.ProtocolVersion, "workspaceGuard": true}), nil
		}
		var record workspaceGuardRecord
		if json.Unmarshal(request.Parameters, &record) != nil {
			return nil, targetexec.ErrProtocol
		}
		if request.Method == "guard.begin" {
			if record.Path != "/project" || !record.ValidUntil.After(time.Now().UTC()) || record.ValidUntil.After(time.Now().UTC().Add(30*time.Second)) {
				return nil, targetexec.ErrAuthority
			}
			writers.Add(1)
			record.State = "active"
		} else if request.Method == "guard.release" {
			releases.Add(1)
			record.State = "released"
		} else {
			return nil, targetexec.ErrProtocol
		}
		return mustJSON(record), nil
	}}
	done := make(chan error, 1)
	go func() { done <- transport.Serve(context.Background(), right) }()
	client := targetexec.NewClient(left)
	if _, err := client.Call(context.Background(), targetexec.Request{ID: "hello", Method: "hello"}); err != nil {
		t.Fatal(err)
	}
	link := &supervisionTargetLink{Client: client, TargetID: "clock-guard"}
	s.workspaceGuardTargets = newSupervisionTargetConnections(s.runtimeCtx, func(context.Context, string) (*supervisionTargetLink, error) { return link, nil })
	t.Cleanup(func() { s.workspaceGuardTargets.Close(); client.Close(); <-done })
	for i := 0; i < 2; i++ {
		release, err := s.beginWorkspaceOperationAtPath(Project{Runner: "ssh-clock"}, "/project", "writer")
		if err != nil {
			t.Fatal("new ordinary remote operation could not obtain a protocol-bounded begin window", err)
		}
		release()
	}
	if healthCalls.Load() != 2 || writers.Load() != 2 {
		t.Fatal("reused connection did not refresh each new operation's clock", healthCalls.Load(), writers.Load())
	}
	remote := &sshClient{}
	s.runnerRegistry.register("ssh-clock", &sshRunner{client: remote}, RunnerMeta{ID: "ssh-clock"})
	release, err := s.beginWorkspaceOperationAtPath(Project{Runner: "ssh-clock"}, "/project", "uncertain-writer")
	if err != nil {
		t.Fatal(err)
	}
	remote.workspaceUncertainty.Add(1)
	release()
	release()
	var retained int
	if err := s.db.QueryRow(`select count(*) from workspace_guard_operations where state='unconfirmed' and path='/project'`).Scan(&retained); err != nil || retained != 1 || releases.Load() != 2 {
		t.Fatal("uncertain target exit lost its durable token or sent release", retained, releases.Load(), err)
	}
	if runtime.GOOS == "windows" {
		distro := "guard-clock-fixture"
		s.wslRunner = &wslAgentRunner{distro: distro}
		release, err := s.beginWorkspaceOperationAtPath(Project{Runner: "wsl-local"}, wslToUncPath("/project", distro), "uncertain-wsl-writer")
		if err != nil {
			t.Fatal(err)
		}
		workspaceWSLUncertainty(distro).Add(1)
		release()
		if err := s.db.QueryRow(`select count(*) from workspace_guard_operations where state='unconfirmed' and path='/project'`).Scan(&retained); err != nil || retained != 2 || releases.Load() != 2 {
			t.Fatal("WSL wrapper failure released target protection", retained, releases.Load(), err)
		}
	}
}

func TestWorkspaceGuardPublicNativeAdmissionSharesAcrossDatabases(t *testing.T) {
	s, p, _, draft, now := supervisionFixture(t, defaultSupervisionBudget())
	other := newTestServer(t)
	other.config.WorkspaceGuardDir = s.config.WorkspaceGuardDir
	writer, err := other.beginWorkspaceOperationAtPath(p, p.Path, "other-database-writer")
	if err != nil {
		t.Fatal(err)
	}
	defer writer()
	ctx := context.Background()
	if err := s.startSupervisionSession(ctx, draft.ID, draft.StateVersion, now); err != nil {
		t.Fatal(err)
	}
	ss := supervisionSessionForTest(t, s, draft.ID)
	if err := s.admitSupervisionWorkspace(ctx, ss.ID, ss.StateVersion, time.Now().UTC()); !errors.Is(err, errWorkspaceNotQuiescent) {
		t.Fatal("a separate database bypassed its existing ordinary writer", err)
	}
	writer()
	if err := s.admitSupervisionWorkspace(ctx, ss.ID, ss.StateVersion, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if release, err := other.beginWorkspaceOperationAtPath(p, filepath.Join(p.Path, "new-child"), "child-writer"); err == nil {
		release()
		t.Fatal("ordinary operation in another database bypassed owned parent protection")
	}
	ss = supervisionSessionForTest(t, s, ss.ID)
	if err := s.requestSupervisionControl(ctx, ss.ID, ss.StateVersion, "stop", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.settleSupervisionControl(ctx, ss.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	release, err := other.beginWorkspaceOperationAtPath(p, p.Path, "after-release")
	if err != nil {
		t.Fatal("confirmed release stranded the other controller", err)
	}
	release()
}
