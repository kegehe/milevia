package targetexec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

// Input contains only the immutable authorized model observation and fixed role
// instructions. Runtime credentials stay inside the target's CLI adapter.
type Input struct {
	Observation  []byte `json:"observation"`
	Instructions string `json:"instructions"`
}

type Receipt struct {
	Quiet        bool
	ResultSHA256 string
	Usage        Usage
}

type Handshake func(ProcessIdentity, func() error) error

// Executor is implemented once per target runtime and delegates model protocols
// to CLI adapters. StopAndProbe must prove all descendants and output are quiet;
// neither a missing parent PID nor CLI exit=0 satisfies that contract.
type Executor interface {
	Run(context.Context, Execution, Input, Handshake) (Receipt, error)
	StopAndProbe(context.Context, Execution) (bool, error)
}

type Supervisor struct {
	store     *Store
	executor  Executor
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	active    map[string]context.CancelFunc
	wg        sync.WaitGroup
	closed    bool
	lastError error
}

func NewSupervisor(parent context.Context, store *Store, executor Executor) (*Supervisor, error) {
	if store == nil || executor == nil {
		return nil, errors.New("target supervisor requires a durable store and executor")
	}
	ctx, cancel := context.WithCancel(parent)
	s := &Supervisor{store: store, executor: executor, ctx: ctx, cancel: cancel, active: map[string]context.CancelFunc{}}
	s.wg.Add(1)
	go s.monitor()
	return s, nil
}

func hashBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

// Start is asynchronous: a broken response channel cannot trigger a second run.
// Repeated calls return durable facts, including an unknown interrupted attempt.
func (s *Supervisor) Start(spec ExecutionSpec, input Input, now time.Time) (Execution, error) {
	return s.StartWithCredentials(spec, input, nil, now)
}

func (s *Supervisor) StartWithCredentials(spec ExecutionSpec, input Input, credentials *RuntimeCredentials, now time.Time) (Execution, error) {
	if len(input.Observation) > 1<<20 || len(input.Instructions) > 256<<10 || hashBytes(input.Observation) != spec.InputSHA256 || hashBytes([]byte(input.Instructions)) != spec.InstructionsSHA256 {
		return Execution{}, ErrConflict
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.ctx.Err() != nil {
		return Execution{}, ErrClosed
	}
	execution, err := s.store.Prepare(spec, now)
	if err != nil {
		return Execution{}, err
	}
	if execution.State != "prepared" {
		return execution, nil
	}
	execution, err = s.store.ClaimStart(spec.SessionID, spec.ExecutionID, execution.Digest, now)
	if err != nil {
		return execution, err
	}
	deadline, err := s.store.turnDeadline(spec.SessionID, spec.ExecutionID)
	if err != nil {
		return execution, err
	}
	ctx, cancel := context.WithDeadline(s.ctx, deadline)
	if credentials != nil {
		// Freeze the projection before the caller can reuse its request object.
		frozen := *credentials
		frozen.Env = make(map[string]string, len(credentials.Env))
		for key, value := range credentials.Env {
			frozen.Env[key] = value
		}
		ctx = context.WithValue(ctx, runtimeCredentialsKey{}, &frozen)
	}
	s.active[spec.ExecutionID] = cancel
	s.wg.Add(1)
	go s.run(ctx, execution, input)
	return execution, nil
}

func (s *Supervisor) run(ctx context.Context, execution Execution, input Input) {
	defer s.wg.Done()
	receipt, runErr := s.executor.Run(ctx, execution, input, func(identity ProcessIdentity, resume func() error) error {
		if err := s.store.RegisterHeld(execution.Spec.SessionID, execution.Spec.ExecutionID, identity, time.Now().UTC()); err != nil {
			return err
		}
		return s.store.Resume(execution.Spec.SessionID, execution.Spec.ExecutionID, time.Now().UTC(), resume)
	})
	if receipt.Quiet {
		if err := s.store.RecordQuiet(execution.Spec.SessionID, execution.Spec.ExecutionID, execution.Digest, receipt.ResultSHA256, runErr != nil, receipt.Usage); err != nil {
			s.rememberError(err)
		}
	}
	s.mu.Lock()
	if cancel := s.active[execution.Spec.ExecutionID]; cancel != nil {
		cancel()
	}
	delete(s.active, execution.Spec.ExecutionID)
	s.mu.Unlock()
	if !receipt.Quiet {
		s.rememberError(errors.Join(runErr, ErrNotQuiet))
	}
}

func (s *Supervisor) rememberError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastError = err
}

func (s *Supervisor) ApplyControl(control Control, now time.Time) error {
	if err := s.store.ApplyControl(control, now); err != nil {
		return err
	}
	return s.reconcile(now)
}

func (s *Supervisor) reconcile(now time.Time) error {
	stops, err := s.store.DueStops(now)
	if err != nil {
		// A storage failure cannot leave active execution consuming an unrenewable
		// grant. Cancel only this supervisor's registered executions.
		s.mu.Lock()
		for _, cancel := range s.active {
			cancel()
		}
		s.mu.Unlock()
		return err
	}
	for _, execution := range stops {
		s.mu.Lock()
		cancel := s.active[execution.Spec.ExecutionID]
		s.mu.Unlock()
		if cancel != nil {
			cancel()
			continue
		}
		probe, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		quiet, probeErr := s.executor.StopAndProbe(probe, execution)
		cancel()
		if probeErr != nil {
			return probeErr
		}
		if !quiet {
			continue
		}
		if err := s.store.RecordQuiet(execution.Spec.SessionID, execution.Spec.ExecutionID, execution.Digest, "", true, execution.Usage); err != nil {
			return err
		}
	}
	return nil
}

func (s *Supervisor) monitor() {
	defer s.wg.Done()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case now := <-ticker.C:
			if err := s.reconcile(now.UTC()); err != nil {
				s.rememberError(err)
			}
		}
	}
}

// Close waits for the executor's containment and output checks. Unknown resources
// remain protected in the store and are reconciled on the next target launch.
func (s *Supervisor) Close() error {
	s.mu.Lock()
	s.closed = true
	s.cancel()
	for _, cancel := range s.active {
		cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
	err := s.store.Disconnect()
	err = errors.Join(err, s.reconcile(time.Now().UTC()))
	s.mu.Lock()
	defer s.mu.Unlock()
	return errors.Join(s.lastError, err)
}
