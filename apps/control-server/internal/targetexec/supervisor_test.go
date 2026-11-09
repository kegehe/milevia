package targetexec

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type executorForTest struct {
	starts  atomic.Int64
	resumes atomic.Int64
	quiet   atomic.Bool
	entered chan struct{}
	proceed chan struct{}
	probes  atomic.Int64
}

func (e *executorForTest) Run(ctx context.Context, execution Execution, _ Input, handshake Handshake) (Receipt, error) {
	e.starts.Add(1)
	if e.entered != nil {
		close(e.entered)
	}
	if e.proceed != nil {
		select {
		case <-e.proceed:
		case <-ctx.Done():
			return Receipt{Quiet: true}, ctx.Err()
		}
	}
	err := handshake(ProcessIdentity{Container: execution.Container, PID: 42, Birth: "fixture-birth", Executable: "/trusted/fixture"}, func() error { e.resumes.Add(1); return nil })
	if err != nil {
		return Receipt{Quiet: true}, err
	}
	<-ctx.Done()
	return Receipt{Quiet: e.quiet.Load(), Usage: Usage{Sequence: 1, ActiveMillis: 1}}, ctx.Err()
}

func (e *executorForTest) StopAndProbe(context.Context, Execution) (bool, error) {
	e.probes.Add(1)
	return e.quiet.Load(), nil
}

func supervisorFixture(t *testing.T) (*Supervisor, *Store, Control, ExecutionSpec, Input, *executorForTest) {
	t.Helper()
	store, control, spec, _ := fixture(t)
	input := Input{Observation: []byte(`{"authorized":true}`), Instructions: "fixed role instructions"}
	spec.InputSHA256, spec.InstructionsSHA256 = hashBytes(input.Observation), hashBytes([]byte(input.Instructions))
	executor := &executorForTest{}
	executor.quiet.Store(true)
	supervisor, err := NewSupervisor(context.Background(), store, executor)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { supervisor.Close() })
	return supervisor, store, control, spec, input, executor
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	until := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(until) {
			t.Fatal("target supervisor did not converge")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSupervisorLostResponseAndConcurrentStartupDoNotDuplicateExecution(t *testing.T) {
	supervisor, store, control, spec, input, executor := supervisorFixture(t)
	var callers sync.WaitGroup
	for range 20 {
		callers.Add(1)
		go func() {
			defer callers.Done()
			if _, err := supervisor.Start(spec, input, time.Now().UTC()); err != nil {
				t.Error(err)
			}
		}()
	}
	callers.Wait()
	waitFor(t, func() bool { return executor.resumes.Load() == 1 })
	if executor.starts.Load() != 1 {
		t.Fatal("duplicate CLI invocation", executor.starts.Load())
	}
	control.Version++
	control.State = "stopped"
	if err := supervisor.ApplyControl(control, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		facts, _ := store.Facts()
		return facts.Sessions[spec.SessionID].Executions[spec.ExecutionID].State == "quiescent"
	})
	if err := store.Release(control.SessionID, control.OwnerEpoch, control.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Start(spec, input, time.Now().UTC()); err != nil {
		t.Fatal("duplicate query should return quiescent facts", err)
	}
	if executor.starts.Load() != 1 {
		t.Fatal("late delivery revived released execution")
	}
}

func TestSupervisorRevokeBetweenPreparationAndHeldProcessPreventsResume(t *testing.T) {
	supervisor, store, control, spec, input, executor := supervisorFixture(t)
	executor.entered = make(chan struct{})
	executor.proceed = make(chan struct{})
	if _, err := supervisor.Start(spec, input, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	<-executor.entered
	control.Version++
	control.State = "stopped"
	if err := supervisor.ApplyControl(control, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	close(executor.proceed)
	waitFor(t, func() bool {
		facts, _ := store.Facts()
		return facts.Sessions[spec.SessionID].Executions[spec.ExecutionID].State == "quiescent"
	})
	if executor.resumes.Load() != 0 {
		t.Fatal("late process resumed after revoke")
	}
}

func TestSupervisorRejectsInputSubstitutionBeforeStartingCLI(t *testing.T) {
	supervisor, _, _, spec, input, executor := supervisorFixture(t)
	input.Observation = []byte(`{"different":true}`)
	if _, err := supervisor.Start(spec, input, time.Now().UTC()); !errors.Is(err, ErrConflict) {
		t.Fatal("unfrozen input accepted", err)
	}
	if executor.starts.Load() != 0 {
		t.Fatal("model called for unauthorized input")
	}
}

func TestSupervisorPreservesProtectionUntilUnknownResourcesActuallyStop(t *testing.T) {
	supervisor, store, control, spec, input, executor := supervisorFixture(t)
	executor.quiet.Store(false)
	if _, err := supervisor.Start(spec, input, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return executor.resumes.Load() == 1 })
	control.Version++
	control.State = "stopped"
	if err := supervisor.ApplyControl(control, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { supervisor.mu.Lock(); defer supervisor.mu.Unlock(); return len(supervisor.active) == 0 })
	if err := supervisor.reconcile(time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := store.Release(control.SessionID, control.OwnerEpoch, control.Version); !errors.Is(err, ErrNotQuiet) {
		t.Fatal("unknown resources released", err)
	}
	executor.quiet.Store(true)
	if err := supervisor.reconcile(time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := store.Release(control.SessionID, control.OwnerEpoch, control.Version); err != nil {
		t.Fatal(err)
	}
	if executor.probes.Load() < 2 {
		t.Fatal("container facts were not probed")
	}
}
