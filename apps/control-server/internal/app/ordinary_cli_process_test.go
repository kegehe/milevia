package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

func TestOrdinaryCLIProcessFixture(t *testing.T) {
	mode := os.Getenv("MILEVIA_ORDINARY_FIXTURE")
	if mode == "" {
		return
	}
	dir := os.Getenv("MILEVIA_ORDINARY_DIRECTORY")
	switch mode {
	case "leaf":
		_ = os.WriteFile(filepath.Join(dir, "leaf.pid"), []byte(strconv.Itoa(os.Getpid())), 0600)
		for {
			_ = os.WriteFile(filepath.Join(dir, "heartbeat"), []byte(time.Now().String()), 0600)
			time.Sleep(20 * time.Millisecond)
		}
	case "fork", "persist":
		childMode := "leaf"
		if mode == "fork" {
			childMode = "middle"
		}
		child := ordinaryFixtureCommand(childMode, dir)
		ordinaryFixtureDetach(child)
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(90)
		}
		if !ordinaryFixtureWaitFile(filepath.Join(dir, "leaf.pid"), 5*time.Second) {
			os.Exit(91)
		}
		fmt.Fprintln(os.Stdout, "fixture-ready")
		if mode == "fork" {
			os.Exit(7)
		}
		for {
			time.Sleep(time.Second)
		}
	case "middle":
		child := ordinaryFixtureCommand("leaf", dir)
		ordinaryFixtureDetach(child)
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(96)
		}
		if !ordinaryFixtureWaitFile(filepath.Join(dir, "leaf.pid"), 5*time.Second) {
			os.Exit(97)
		}
		os.Exit(0)
	case "owner":
		config := Config{WorkspaceGuardDir: filepath.Join(dir, "guard")}
		cmd := ordinaryFixtureCommand("persist", dir)
		cmd.Dir, cmd.Stdout, cmd.Stderr = dir, os.Stdout, os.Stderr
		if err := startOrdinaryCLICommand(context.Background(), cmd, config); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(92)
		}
		_ = waitOrdinaryCLICommand(cmd)
	case "owner-held":
		config := Config{WorkspaceGuardDir: filepath.Join(dir, "guard")}
		record, err := changeWorkspaceGuard(context.Background(), config, "begin", workspaceGuardRecord{ID: uuid.NewString(), Kind: "operation", Path: dir, ValidUntil: time.Now().Add(time.Minute), CLIContainer: uuid.NewString()})
		if err != nil {
			os.Exit(93)
		}
		cmd := ordinaryFixtureCommand("touch", dir)
		process, err := startOrdinaryContainedProcess(cmd, supervisionJobPrefix+record.CLIContainer)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(94)
		}
		if err := withWorkspaceGuard(context.Background(), config, func(f *workspaceGuardFacts) error {
			r := f.Records[record.ID]
			r.CLIStarted = true
			identity := process.identity()
			r.CLIProcess = &identity
			f.Records[r.ID] = r
			return nil
		}); err != nil {
			os.Exit(95)
		}
		_ = os.WriteFile(filepath.Join(dir, "held-ready"), []byte("registered without resume"), 0600)
		for {
			time.Sleep(time.Second)
		}
	case "stream":
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			fmt.Fprintln(os.Stdout, `{"type":"system","subtype":"init","session_id":"ordinary-fixture"}`)
			fmt.Fprintln(os.Stdout, `{"type":"assistant","message":{"content":[{"type":"text","text":"ok"}]}}`)
			fmt.Fprintln(os.Stdout, `{"type":"result","subtype":"success"}`)
		}
	case "touch":
		_ = os.WriteFile(filepath.Join(dir, "unsafe-start"), []byte("executed"), 0600)
	}
	os.Exit(0)
}

func ordinaryFixtureCommand(mode, dir string) *exec.Cmd {
	command := exec.Command(os.Args[0], "-test.run=^TestOrdinaryCLIProcessFixture$")
	command.Env = managedCLIEnvironment(nil, os.Environ(), "MILEVIA_ORDINARY_FIXTURE="+mode, "MILEVIA_ORDINARY_DIRECTORY="+dir)
	command.Dir = dir
	return command
}

func ordinaryFixtureWaitFile(path string, limit time.Duration) bool {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func ordinaryFixtureRecords(t *testing.T, config Config) []workspaceGuardRecord {
	t.Helper()
	var records []workspaceGuardRecord
	if err := withWorkspaceGuard(context.Background(), config, func(f *workspaceGuardFacts) error {
		for _, r := range f.Records {
			records = append(records, r)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return records
}

func TestOrdinaryCLIDetachedDescendantsAndOpenPipes(t *testing.T) {
	if err := supervisionLocalProcessCapability(); err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	config := Config{WorkspaceGuardDir: filepath.Join(dir, "guard")}
	cmd := ordinaryFixtureCommand("fork", dir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	if err := startOrdinaryCLICommand(context.Background(), cmd, config); err != nil {
		t.Fatal(err)
	}
	lines := make(chan string, 1)
	go func() {
		scan := bufio.NewScanner(stdout)
		var text strings.Builder
		for scan.Scan() {
			text.WriteString(scan.Text())
		}
		lines <- text.String()
	}()
	if err := waitOrdinaryCLICommand(cmd); err == nil || !strings.Contains(err.Error(), "7") {
		t.Fatal("exit status lost", err)
	}
	select {
	case text := <-lines:
		if !strings.Contains(text, "fixture-ready") {
			t.Fatal("output lost", text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("detached child retained output")
	}
	for _, record := range ordinaryFixtureRecords(t, config) {
		if !record.CLIStarted || record.State != "released" {
			t.Fatal("no durable release proof", record)
		}
		quiet, err := supervisionContainerQuiescent(supervisionJobPrefix + record.CLIContainer)
		if err != nil || !quiet {
			t.Fatal("descendants survived", quiet, err)
		}
	}
}

func TestOrdinaryCLIRejectsReleaseWhileRunningAndSeparatesContainers(t *testing.T) {
	if err := supervisionLocalProcessCapability(); err != nil {
		t.Skip(err)
	}
	root := t.TempDir()
	config := Config{WorkspaceGuardDir: filepath.Join(root, "guard")}
	first, second := filepath.Join(root, "first"), filepath.Join(root, "second")
	_ = os.Mkdir(first, 0700)
	_ = os.Mkdir(second, 0700)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, b := ordinaryFixtureCommand("persist", first), ordinaryFixtureCommand("persist", second)
	if err := startOrdinaryCLICommand(ctx, a, config); err != nil {
		t.Fatal(err)
	}
	defer func() { stopOrdinaryCLICommand(a) }()
	if err := startOrdinaryCLICommand(context.Background(), b, config); err != nil {
		t.Fatal(err)
	}
	defer func() { stopOrdinaryCLICommand(b); _ = waitOrdinaryCLICommand(b) }()
	if !ordinaryFixtureWaitFile(filepath.Join(first, "leaf.pid"), 5*time.Second) || !ordinaryFixtureWaitFile(filepath.Join(second, "leaf.pid"), 5*time.Second) {
		t.Fatal("children did not start")
	}
	for _, r := range ordinaryFixtureRecords(t, config) {
		if _, err := changeWorkspaceGuard(context.Background(), config, "release", r); !errors.Is(err, errWorkspaceNotQuiescent) {
			t.Fatal("active writer released", err)
		}
	}
	cancel()
	_ = waitOrdinaryCLICommand(a)
	for _, r := range ordinaryFixtureRecords(t, config) {
		quiet, err := supervisionContainerQuiescent(supervisionJobPrefix + r.CLIContainer)
		if err != nil {
			t.Fatal(err)
		}
		if strings.EqualFold(r.Path, second) && quiet {
			t.Fatal("unrelated CLI was terminated")
		}
	}
}

func TestOrdinaryCLIControllerCrashRecoversFromKernelEvidence(t *testing.T) {
	if err := supervisionLocalProcessCapability(); err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	owner := ordinaryFixtureCommand("owner", dir)
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Process.Kill() }()
	if !ordinaryFixtureWaitFile(filepath.Join(dir, "leaf.pid"), 10*time.Second) {
		_ = owner.Process.Kill()
		t.Fatal("owner did not launch child")
	}
	config := Config{WorkspaceGuardDir: filepath.Join(dir, "guard")}
	records := ordinaryFixtureRecords(t, config)
	if len(records) != 1 {
		t.Fatal(records)
	}
	_ = owner.Process.Kill()
	_ = owner.Wait()
	deadline := time.Now().Add(10 * time.Second)
	for {
		record, err := changeWorkspaceGuard(context.Background(), config, "query", records[0])
		if err != nil {
			t.Fatal(err)
		}
		if record.State == "released" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("owner crash left its container running", record)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestOrdinaryCLIControllerCrashBeforeResumeSettlesHeldContainer(t *testing.T) {
	if err := supervisionLocalProcessCapability(); err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	owner := ordinaryFixtureCommand("owner-held", dir)
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Process.Kill() }()
	if !ordinaryFixtureWaitFile(filepath.Join(dir, "held-ready"), 10*time.Second) {
		t.Fatal("owner did not register held process")
	}
	config := Config{WorkspaceGuardDir: filepath.Join(dir, "guard")}
	records := ordinaryFixtureRecords(t, config)
	if len(records) != 1 {
		t.Fatal(records)
	}
	_ = owner.Process.Kill()
	_ = owner.Wait()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r, err := changeWorkspaceGuard(context.Background(), config, "query", records[0])
		if err != nil {
			t.Fatal(err)
		}
		if r.State == "released" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("held container survived its owner", r)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(dir, "unsafe-start")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("held process executed after owner crash", err)
	}
}

func TestOrdinaryCLIDelayedStartCannotResurrectReleasedTicket(t *testing.T) {
	if err := supervisionLocalProcessCapability(); err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	config := Config{WorkspaceGuardDir: filepath.Join(dir, "guard")}
	record, err := changeWorkspaceGuard(context.Background(), config, "begin", workspaceGuardRecord{ID: uuid.NewString(), Kind: "operation", Path: dir, ValidUntil: time.Now().Add(time.Minute), CLIContainer: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := changeWorkspaceGuard(context.Background(), config, "release", record); err != nil {
		t.Fatal(err)
	}
	cmd := ordinaryFixtureCommand("touch", dir)
	if err := startOrdinaryCLICommand(ordinaryCLIContext(context.Background(), config, record), cmd, config); !errors.Is(err, targetexec.ErrAuthority) {
		t.Fatal("released grant accepted", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "unsafe-start")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unregistered command executed", err)
	}
}

func TestOrdinaryCLIRejectedLookupAndCancelledStartsHaveNoEffects(t *testing.T) {
	for _, scenario := range []string{"lookup-denied", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			cmd := ordinaryFixtureCommand("touch", dir)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := exec.ErrDot
			if scenario == "lookup-denied" {
				cmd.Err = want
			} else {
				cancel()
				want = context.Canceled
			}
			if err := startOrdinaryCLICommand(ctx, cmd, Config{WorkspaceGuardDir: filepath.Join(dir, "guard")}); !errors.Is(err, want) {
				t.Fatal("start rejection was bypassed", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "unsafe-start")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("rejected CLI executed", err)
			}
		})
	}
}

func TestOrdinaryCLIClaudeSessionSurvivesOutputDrainDeadline(t *testing.T) {
	if testing.Short() {
		t.Skip("exercises a real persistent session beyond the 30-second output drain deadline")
	}
	if err := supervisionLocalProcessCapability(); err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "claude-fixture")
	script := "#!/bin/sh\nexec " + shellQuote(executable) + " '-test.run=^TestOrdinaryCLIProcessFixture$'\n"
	if runtime.GOOS == "windows" {
		path += ".cmd"
		script = "@echo off\r\n\"" + executable + "\" \"-test.run=^TestOrdinaryCLIProcessFixture$\"\r\n"
	}
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MILEVIA_ORDINARY_FIXTURE", "stream")
	t.Setenv("MILEVIA_ORDINARY_DIRECTORY", dir)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	runner := &claudeCLIRunner{config: Config{ClaudePath: path, WorkspaceGuardDir: filepath.Join(dir, "guard")}}
	session, err := runner.StartSession(ctx, AgentSessionRequest{ProjectPath: dir, SessionID: uuid.NewString(), PermissionMode: "full_control"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		session.Stop()
		select {
		case <-session.Done():
		case <-time.After(10 * time.Second):
			t.Error("persistent Claude session did not stop")
		}
	}()
	turn := func() {
		t.Helper()
		sink := &claudeTurnTestSink{}
		if err := session.Send(AgentRunRequest{Prompt: "complete this turn", RunID: uuid.NewString()}, sink); err != nil {
			t.Fatal(err)
		}
		if result := waitForTurnErrors(t, sink, 1); len(result) != 1 || result[0] != nil {
			t.Fatal("live session lost a result frame", result)
		}
	}
	turn()
	// The deadline bounds output draining after exit, never session lifetime.
	select {
	case <-time.After(32 * time.Second):
	case <-session.Done():
		t.Fatal("persistent session exited before the second turn")
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	turn()
	records := ordinaryFixtureRecords(t, runner.config)
	if len(records) != 1 {
		t.Fatal("persistent session has no immutable directory ticket", records)
	}
	for _, record := range records {
		if record.State != "active" || !record.CLIStarted {
			t.Fatal("live persistent session lost directory protection", record)
		}
	}
}

func TestWorkspaceGuardRecoveryFairAcrossStatesAndBatchLimit(t *testing.T) {
	s, p, _ := admissionProjectForTest(t)
	release, err := s.beginWorkspaceOperationAtPath(p, p.Path, "confirmed-target")
	if err != nil {
		t.Fatal(err)
	}
	release()
	// Reinsert the controller's lost deletion; its target already has a release.
	var token workspaceGuardRecord
	if err := withWorkspaceGuard(context.Background(), s.config, func(f *workspaceGuardFacts) error {
		for _, r := range f.Records {
			token = r
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	route := s.supervisionProjectRoute(p)
	old := time.Now().UTC().Add(-time.Hour)
	if _, err := s.db.Exec(`insert into workspace_guard_operations(id,connection_key,path,state,created_at,updated_at,target_id,valid_until) values(?,?,?,'unconfirmed',?,?,?,?)`, token.ID, route.Key, p.Path, old, old.Add(time.Minute), "", token.ValidUntil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 16; i++ {
		if _, err := s.db.Exec(`insert into workspace_guard_operations(id,connection_key,path,state,created_at,updated_at) values(?,?,?,'release_pending',?,?)`, fmt.Sprintf("blocked-%02d", i), fmt.Sprintf("missing-target-%d", i%3), p.Path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := s.reconcilePendingWorkspaceGuardOperations(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := s.db.QueryRow(`select count(*) from workspace_guard_operations where id=?`, token.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("unknown operation starved behind failed releases", count, err)
	}
}

func TestWorkspaceGuardContainmentJournalUpgradePreservesLegacyOwnership(t *testing.T) {
	config := Config{WorkspaceGuardDir: t.TempDir()}
	root := t.TempDir()
	legacy := workspaceGuardFacts{Protocol: 1, ID: "stable-legacy-journal", Records: map[string]workspaceGuardRecord{"legacy-writer": {ID: "legacy-writer", Kind: "operation", Path: root, State: "active", ValidUntil: time.Now().Add(time.Minute)}}}
	name := filepath.Join(config.WorkspaceGuardDir, "journal.json")
	if err := os.WriteFile(name, mustJSON(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	if err := withWorkspaceGuard(context.Background(), config, func(f *workspaceGuardFacts) error {
		if f.ID != legacy.ID || f.Records["legacy-writer"].State != "active" {
			t.Fatal("legacy identity or ownership lost", f)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	var header struct {
		Protocol int `json:"protocol"`
	}
	if json.Unmarshal(raw, &header) != nil || header.Protocol != workspaceGuardJournalProtocol || header.Protocol == 1 {
		t.Fatal("old writers can erase containment fields", string(raw))
	}
	actual, err := changeWorkspaceGuard(context.Background(), config, "query", workspaceGuardRecord{ID: "legacy-writer", Kind: "operation", JournalID: legacy.ID})
	if err != nil || actual.State != "active" {
		t.Fatal("legacy unknown writer was released on migration", actual, err)
	}
}

func TestWorkspaceGuardRejectsIncompleteOrFutureJournalWithoutOverwriting(t *testing.T) {
	for _, body := range []string{`{}`, `{"protocol":1,"id":"retained"}`, `{"protocol":2,"records":{}}`, `{"protocol":99,"id":"future","records":{}}`} {
		t.Run(body, func(t *testing.T) {
			config := Config{WorkspaceGuardDir: t.TempDir()}
			name := filepath.Join(config.WorkspaceGuardDir, "journal.json")
			if err := os.WriteFile(name, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if err := withWorkspaceGuard(context.Background(), config, func(*workspaceGuardFacts) error { t.Fatal("invalid journal reached writer"); return nil }); err == nil {
				t.Fatal("invalid persisted facts became an empty journal")
			}
			raw, err := os.ReadFile(name)
			if err != nil || string(raw) != body {
				t.Fatal("unconfirmed protection was overwritten", string(raw), err)
			}
		})
	}
}

func TestOrdinaryCLIPersistentSessionMultipleTurns(t *testing.T) {
	if err := supervisionLocalProcessCapability(); err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	config := Config{WorkspaceGuardDir: filepath.Join(dir, "guard")}
	cmd := ordinaryFixtureCommand("stream", dir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := startOrdinaryCLICommand(context.Background(), cmd, config); err != nil {
		t.Fatal(err)
	}
	session := newStreamJSONSession(streamJSONSessionConfig{agentID: "codebuddy", label: "fixture", exitPrefix: "fixture failure: "}, cmd, stdin, stdout, stderr)
	session.watch(context.Background())
	defer session.Stop()
	for turn := 0; turn < 3; turn++ {
		sink := &conflictSuggestionTurnSink{finished: make(chan error, 1)}
		if err := session.Send(AgentRunRequest{AgentID: "codebuddy", RunID: uuid.NewString(), Prompt: "one more turn"}, sink); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-sink.finished:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("session turn did not finish")
		}
		for _, r := range ordinaryFixtureRecords(t, config) {
			if r.State != "active" || !r.CLIStarted {
				t.Fatal("turn completion released persistent session", r)
			}
		}
	}
	session.Stop()
	select {
	case <-session.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("session did not settle with open stdin")
	}
	for _, r := range ordinaryFixtureRecords(t, config) {
		if r.State != "released" {
			t.Fatal("session exit did not release", r)
		}
	}
}

func TestOrdinaryCLIExpiredRemoteLeaseStopsWholeContainer(t *testing.T) {
	if err := supervisionLocalProcessCapability(); err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	config := Config{WorkspaceGuardDir: filepath.Join(dir, "guard")}
	record, err := changeWorkspaceGuard(context.Background(), config, "begin", workspaceGuardRecord{ID: uuid.NewString(), Kind: "operation", Path: dir, ValidUntil: time.Now().Add(30 * time.Second), CLIContainer: uuid.NewString(), CLILeaseUntil: time.Now().Add(3 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	cmd := ordinaryFixtureCommand("persist", dir)
	if err := startOrdinaryCLICommand(ordinaryCLIContext(context.Background(), config, record), cmd, config); err != nil {
		t.Fatal(err)
	}
	defer stopOrdinaryCLICommand(cmd)
	if !ordinaryFixtureWaitFile(filepath.Join(dir, "leaf.pid"), 5*time.Second) {
		t.Fatal("child did not start")
	}
	done := make(chan error, 1)
	go func() { done <- waitOrdinaryCLICommand(cmd) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expired execution lease was reported successful")
		}
	case <-time.After(8 * time.Second):
		t.Fatal("lost controller lease left descendants running")
	}
	if _, err := changeWorkspaceGuard(context.Background(), config, "renew-cli", record); !errors.Is(err, targetexec.ErrAuthority) {
		t.Fatal("expired lease revived", err)
	}
	quiet, err := supervisionContainerQuiescent(supervisionJobPrefix + record.CLIContainer)
	if err != nil || !quiet {
		t.Fatal("lease expiry did not settle kernel container", quiet, err)
	}
}
